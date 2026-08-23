package server

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// 익스플로딩 키튼 대기 상태 마감 타이머 (테스트에서 짧게 낮춘다).
// 노프 창은 쿠(cp_hub.go)와 같은 방식으로 관리한다 — StateSeq 가 바뀔 때만
// 마감을 다시 걸고(syncDeadline), 발화는 AfkSeq 로 지나간 것을 걸러낸다.
var (
	ekTurnTimeout   = 45 * time.Second // 차례 방치 → 자동 1장 뽑기
	ekNopeTimeout   = 5 * time.Second  // 노프 창 → 통과
	ekFavorTimeout  = 20 * time.Second // 부탁 방치 → 무작위 카드
	ekDefuseTimeout = 15 * time.Second // 되꽂기 방치 → 무작위 위치
)

// ekRoom 게임(순수 상태)과 좌석별 연결의 매핑
type ekRoom struct {
	Game       *EKGame
	Clients    map[int]*EKClient // seat → client (탈락해도 남는다 — 관전 전환)
	PhaseTimer *time.Timer

	// DeadlineSeq 마지막으로 마감을 건 StateSeq — 같은 창에 통과가 쌓일 때마다
	// 마감이 늘어나지 않게 하는 근거 (노프가 겹치면 StateSeq 가 올라 새로 걸린다)
	DeadlineSeq int

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string

	// Spectators 순수 관전자 연결 (좌석·세션 없음 — 재접속 미지원).
	// 폭탄으로 탈락한 사람은 여기 들어오지 않는다 — 좌석을 유지한 채
	// alive=false 로 관전 전환된다.
	Spectators map[*EKClient]bool

	// LastReact 좌석별 마지막 리액션 발신 시각 (레이트리밋 장부)
	LastReact map[int]time.Time
}

// ekPhaseSignal 마감 타이머의 발화 표식
type ekPhaseSignal struct {
	GameID string
	Seq    int
}

type EKHub struct {
	clients map[*EKClient]bool

	rooms          map[string]*ekRoom
	lobby          *ekRoom
	privateLobbies map[string]*ekRoom
	activeCodes    map[string]string

	register    chan *EKClient
	unregister  chan *EKClient
	gameMessage chan EKGameMessage
	phaseFired  chan ekPhaseSignal

	sessionManager[*EKClient]

	rng *rand.Rand
}

type EKGameMessage struct {
	Client  *EKClient
	Message EKMessage
}

func NewEKHub() *EKHub {
	return &EKHub{
		register:       make(chan *EKClient),
		unregister:     make(chan *EKClient),
		clients:        make(map[*EKClient]bool),
		rooms:          make(map[string]*ekRoom),
		privateLobbies: make(map[string]*ekRoom),
		activeCodes:    make(map[string]string),
		gameMessage:    make(chan EKGameMessage),
		phaseFired:     make(chan ekPhaseSignal, 8),
		sessionManager: newSessionManager[*EKClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *EKHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[EK] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[EK] Client unregistered: %s", client.ID)
			}

		case sessionID := <-h.graceExpired:
			h.handleGraceExpired(sessionID)

		case sig := <-h.phaseFired:
			h.handlePhaseFired(sig)

		case message := <-h.gameMessage:
			h.handleGameMessage(message)
		}
	}
}

func (h *EKHub) handleGameMessage(gm EKGameMessage) {
	// 순수 관전자는 어떤 행동도 할 수 없다 (탈락자는 좌석이 있어 리액션은 된다)
	if h.isSpectator(gm.Client) {
		h.sendError(gm.Client, spectatorDeniedMsg)
		return
	}
	switch gm.Message.Type {
	case EKMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case EKMsgReact:
		h.handleReact(gm.Client, gm.Message)
	case EKMsgFillBots:
		h.handleFillBots(gm.Client)
	case EKMsgStart:
		h.handleStart(gm.Client)
	case EKMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case EKMsgPlay:
		h.handlePlay(gm.Client, gm.Message)
	case EKMsgPlayPair:
		h.handlePlayPair(gm.Client, gm.Message)
	case EKMsgDraw:
		h.handleDraw(gm.Client)
	case EKMsgNope:
		h.handleNope(gm.Client)
	case EKMsgPass:
		h.handlePass(gm.Client)
	case EKMsgGive:
		h.handleGive(gm.Client, gm.Message)
	case EKMsgDefusePlace:
		h.handleDefusePlace(gm.Client, gm.Message)
	}
}

// ==================== 대기실 ====================

func (h *EKHub) handleJoinGame(client *EKClient, msg EKMessage) {
	// 같은 연결의 재입장(버튼 연타)은 유령 좌석을 만들므로 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload EKJoinGamePayload
	json.Unmarshal(payloadBytes, &payload)

	// 이미 시작된 사설 방의 코드로 들어오면 관전자로 입장시킨다
	if gameID, ok := h.activeCodes[normalizeRoomCode(payload.Room)]; ok {
		h.addSpectator(h.rooms[gameID], client, payload.Name)
		return
	}

	room := h.lobbyRoomFor(payload.Room)
	seat, err := room.Game.AddPlayer(payload.Name)
	if err != nil {
		if room.Code != "" { // 가득 찬 사설 방은 관전 진입
			h.addSpectator(room, client, payload.Name)
			return
		}
		h.sendError(client, err.Error())
		return
	}

	client.Name = payload.Name
	client.SessionID = uuid.New().String()
	client.GameID = room.Game.ID
	client.Seat = seat
	h.sessions[client.SessionID] = client
	room.Clients[seat] = client

	log.Printf("[키튼][입장] game=%s | seat%d=%s 대기 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), EKMaxPlayers)
	if room.Code == "" {
		notify("익스플로딩 키튼 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), len(room.Game.Players), EKMaxPlayers))
	}

	h.sendToClient(client, EKMessage{
		Type: EKMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
			"roomCode":  room.Code,
		},
	})

	h.broadcastEvent(room, EKEventPayload{Kind: "joined", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 입장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다
func (h *EKHub) lobbyRoomFor(roomField string) *ekRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewEKGame(uuid.New().String())
			h.lobby = &ekRoom{Game: game, Clients: map[int]*EKClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[EK] Created lobby game %s", game.ID)
		}
		return h.lobby
	}
	if code == roomCodeNew {
		taken := takenCodes(h.privateLobbies)
		for c := range h.activeCodes {
			taken[c] = true
		}
		code = generateRoomCode(h.rng, taken)
	}
	room := h.privateLobbies[code]
	if room == nil {
		game := NewEKGame(uuid.New().String())
		room = &ekRoom{Game: game, Clients: map[int]*EKClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[EK] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// ==================== 관전 / 리액션 ====================

func (h *EKHub) addSpectator(room *ekRoom, client *EKClient, name string) {
	if len(room.Spectators) >= maxSpectators {
		h.sendError(client, spectatorFullMsg)
		return
	}
	if room.Spectators == nil {
		room.Spectators = map[*EKClient]bool{}
	}
	client.Name = name
	client.GameID = room.Game.ID
	room.Spectators[client] = true

	log.Printf("[키튼][관전] game=%s | %s 관전 입장 (%d명)",
		room.Game.ID, displayName(name), len(room.Spectators))

	h.sendToClient(client, EKMessage{
		Type:    EKMsgSpectateJoined,
		Payload: map[string]interface{}{"gameId": room.Game.ID, "roomCode": room.Code},
	})
	h.broadcastState(room)
}

// isSpectator 순수 관전자 연결인지 (좌석 보유자·탈락자는 false)
func (h *EKHub) isSpectator(client *EKClient) bool {
	if client.GameID == "" {
		return false
	}
	room := h.rooms[client.GameID]
	return room != nil && room.Spectators[client]
}

// handleReact 리액션 이모지 — 좌석 보유자만 (탈락해 관전 전환된 사람 포함)
func (h *EKHub) handleReact(client *EKClient, msg EKMessage) {
	room := h.rooms[client.GameID]
	if room == nil || client.Seat < 0 {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload EKReactPayload
	json.Unmarshal(payloadBytes, &payload)

	if !reactAllowed(payload.Emoji) {
		return
	}
	if room.LastReact == nil {
		room.LastReact = map[int]time.Time{}
	}
	if !reactPass(room.LastReact, client.Seat, time.Now()) {
		return
	}
	seat := client.Seat
	h.broadcastEvent(room, EKEventPayload{Kind: "react", Seat: &seat, Name: client.Name,
		Message: payload.Emoji})
}

func (h *EKHub) waitingRoomOf(client *EKClient) *ekRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Ready {
		return nil
	}
	return room
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석
func (h *EKHub) hostSeat(room *ekRoom) int {
	seats := []int{}
	for seat, c := range room.Clients {
		if c != nil && c.Connected && !c.Bot {
			seats = append(seats, seat)
		}
	}
	if len(seats) == 0 {
		return -1
	}
	sort.Ints(seats)
	return seats[0]
}

func ekHumanCount(room *ekRoom) int {
	n := 0
	for _, c := range room.Clients {
		if c != nil && !c.Bot {
			n++
		}
	}
	return n
}

// updateLobbyWaiting 로비 현황판 갱신 — 사설 방은 노출하지 않는다
func (h *EKHub) updateLobbyWaiting(room *ekRoom) {
	if room.Code != "" {
		return
	}
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && ekHumanCount(room) >= 1
	lobbySetWaiting("kittens", waiting)
}

// handleFillBots host 가 빈 좌석을 연습봇으로 4인까지 채운 뒤 즉시 시작한다
func (h *EKHub) handleFillBots(client *EKClient) {
	room := h.waitingRoomOf(client)
	if room == nil {
		h.sendError(client, "대기실을 찾을 수 없습니다")
		return
	}
	if client.Seat != h.hostSeat(room) {
		h.sendError(client, "호스트만 봇을 채울 수 있습니다")
		return
	}

	botNo := 0
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			botNo++
		}
	}
	for len(room.Game.Players) < EKFillBotTarget {
		botNo++
		if !h.spawnEKBot(room, fmt.Sprintf("%s%d", botName, botNo)) {
			break
		}
	}

	if room.Game.CanStart() {
		h.startGame(room)
		return
	}
	h.broadcastState(room)
}

func (h *EKHub) handleStart(client *EKClient) {
	room := h.waitingRoomOf(client)
	if room == nil {
		h.sendError(client, "대기실을 찾을 수 없습니다")
		return
	}
	if client.Seat != h.hostSeat(room) {
		h.sendError(client, "호스트만 시작할 수 있습니다")
		return
	}
	if !room.Game.CanStart() {
		h.sendError(client, "2명 이상 모여야 시작할 수 있습니다")
		return
	}
	h.startGame(room)
}

func (h *EKHub) startGame(room *ekRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	if room.Code != "" {
		delete(h.privateLobbies, room.Code)
		h.activeCodes[room.Code] = room.Game.ID
	} else {
		h.lobby = nil
		lobbySetWaiting("kittens", false)
	}

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	n := len(room.Game.Players)
	log.Printf("[키튼][경기시작] game=%s | 인원=%d | 선=seat%d | 폭탄=%d장 | %v",
		room.Game.ID, n, room.Game.CurrentSeat, n-1, names)
	if !ekRoomHasBot(room) {
		notify("익스플로딩 키튼 시작", fmt.Sprintf("%d인전 시작", n))
	}

	first := room.Game.CurrentSeat
	h.broadcastEvent(room, EKEventPayload{Kind: "game_started", Seat: &first,
		Name: room.Game.Players[first].Name,
		Message: fmt.Sprintf("게임 시작 — %s님부터 (폭탄 고양이 %d장, 시작 손패 %d장)",
			room.Game.Players[first].Name, n-1, EKStartHand+1)})
	h.afterProgress(room)
}

// removeFromLobby 대기실에서 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *EKHub) removeFromLobby(room *ekRoom, client *EKClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	rebuilt := map[int]*EKClient{}
	for seat, c := range room.Clients {
		if seat > oldSeat {
			c.Seat = seat - 1
			rebuilt[seat-1] = c
		} else {
			rebuilt[seat] = c
		}
	}
	room.Clients = rebuilt

	h.drop(client.SessionID)
	client.GameID = ""
	client.Seat = -1

	log.Printf("[키튼][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), EKMaxPlayers)

	if ekHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, EKMessage{Type: EKMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			lobbySetWaiting("kittens", false)
		}
		return
	}

	h.broadcastEvent(room, EKEventPayload{Kind: "left", Name: client.Name,
		Message: fmt.Sprintf("%s님이 퇴장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

func (h *EKHub) roomOf(client *EKClient) *ekRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

func (h *EKHub) handlePlay(client *EKClient, msg EKMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload EKPlayPayload
	json.Unmarshal(payloadBytes, &payload)

	target := -1
	if payload.TargetSeat != nil {
		target = *payload.TargetSeat
	}
	if err := room.Game.Play(client.Seat, payload.Index, target, h.rng); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[키튼][카드] game=%s | seat%d=%s 카드 내기 (대상 seat%d)",
		room.Game.ID, client.Seat, displayName(client.Name), target)
	h.afterProgress(room)
}

func (h *EKHub) handlePlayPair(client *EKClient, msg EKMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload EKPlayPairPayload
	json.Unmarshal(payloadBytes, &payload)

	target := -1
	if payload.TargetSeat != nil {
		target = *payload.TargetSeat
	}
	if err := room.Game.PlayPair(client.Seat, payload.Indexes, target, h.rng); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[키튼][짝] game=%s | seat%d=%s 고양이 짝 훔치기 (대상 seat%d)",
		room.Game.ID, client.Seat, displayName(client.Name), target)
	h.afterProgress(room)
}

func (h *EKHub) handleDraw(client *EKClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	if err := room.Game.Draw(client.Seat, h.rng); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.afterProgress(room)
}

// handleNope 노프 겹치기 — 뒤늦은(이미 지나간 창) 노프는 순수 규칙이
// 조용히 무시한다. 카드가 없을 때만 에러다.
func (h *EKHub) handleNope(client *EKClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	if err := room.Game.Nope(client.Seat); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.afterProgress(room)
}

// handlePass 노프 창 통과 동의 — 뒤늦은 통과는 조용히 무시된다
func (h *EKHub) handlePass(client *EKClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	room.Game.Pass(client.Seat, h.rng)
	h.afterProgress(room)
}

func (h *EKHub) handleGive(client *EKClient, msg EKMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload EKGivePayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.Give(client.Seat, payload.Index); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.afterProgress(room)
}

func (h *EKHub) handleDefusePlace(client *EKClient, msg EKMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload EKDefusePlacePayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.DefusePlace(client.Seat, payload.Position); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.afterProgress(room)
}

// afterProgress 모든 게임 변이 뒤의 공통 마무리 — 이벤트·개인 이벤트 방송,
// 종료 판정, 새 대기 상태의 마감 예약, 스냅샷 방송을 한 번에 처리한다.
func (h *EKHub) afterProgress(room *ekRoom) {
	h.drainEvents(room)
	h.drainPrivates(room)
	if room.Game.Phase == EKPhaseGameOver {
		h.finishGame(room)
		return
	}
	h.syncDeadline(room)
	h.broadcastState(room)
}

// drainEvents 순수 규칙이 쌓은 이벤트를 ek_event 로 방송한다
func (h *EKHub) drainEvents(room *ekRoom) {
	for _, ev := range room.Game.DrainEvents() {
		payload := EKEventPayload{Kind: ev.Kind, Message: ev.Message}
		if ev.Seat >= 0 && ev.Seat < len(room.Game.Players) {
			seat := ev.Seat
			payload.Seat = &seat
			payload.Name = room.Game.Players[seat].Name
		}
		h.broadcastEvent(room, payload)
	}
}

// drainPrivates 예지 결과를 그 좌석 한 명에게만 보낸다 (은닉의 유일한
// 예외 경로 — 방송하지 않는다)
func (h *EKHub) drainPrivates(room *ekRoom) {
	for _, pv := range room.Game.DrainPrivates() {
		c := room.Clients[pv.Seat]
		if c == nil {
			continue
		}
		cards := []string{}
		for _, card := range pv.Cards {
			cards = append(cards, string(card))
		}
		h.sendToClient(c, EKMessage{Type: EKMsgFuture, Payload: EKFuturePayload{Cards: cards}})
	}
}

// ==================== 대기 상태 마감 타이머 (AFK 진행 보장) ====================

// syncDeadline 새 대기 상태(StateSeq 변경)가 열렸을 때만 마감을 다시 건다.
// 같은 노프 창에 통과가 쌓이는 동안에는 처음 건 마감을 유지하고, 노프가
// 겹치면 StateSeq 가 올라 5초가 새로 시작된다.
func (h *EKHub) syncDeadline(room *ekRoom) {
	game := room.Game
	if room.DeadlineSeq == game.StateSeq {
		return
	}
	room.DeadlineSeq = game.StateSeq

	var dur time.Duration
	switch game.Phase {
	case EKPhaseTurn:
		dur = ekTurnTimeout
	case EKPhaseNopeWindow:
		dur = ekNopeTimeout
	case EKPhaseFavorWait:
		dur = ekFavorTimeout
	case EKPhaseDefusePlace:
		dur = ekDefuseTimeout
	default:
		h.stopPhaseTimer(room)
		game.Deadline = 0
		return
	}
	h.scheduleDeadline(room, dur)
}

// scheduleDeadline 마감 타이머 예약. AfkSeq 를 올려 지나간 발화를 구분하고,
// 발화는 허브 채널을 경유하므로 허브 밖에서 상태를 만지지 않는다.
func (h *EKHub) scheduleDeadline(room *ekRoom, dur time.Duration) {
	h.stopPhaseTimer(room)
	room.Game.AfkSeq++
	room.Game.Deadline = time.Now().Add(dur).UnixMilli()
	sig := ekPhaseSignal{GameID: room.Game.ID, Seq: room.Game.AfkSeq}
	room.PhaseTimer = time.AfterFunc(dur, func() {
		h.phaseFired <- sig
	})
}

func (h *EKHub) stopPhaseTimer(room *ekRoom) {
	if room.PhaseTimer != nil {
		room.PhaseTimer.Stop()
		room.PhaseTimer = nil
	}
}

// handlePhaseFired 마감 발화 — 단계별 자동 행동으로 해소한다.
//   - turn: 자동 1장 뽑기
//   - nope_window: 전원 통과와 같은 판정
//   - favor_wait: 무작위 카드 건네기
//   - defuse_place: 무작위 위치 되꽂기
func (h *EKHub) handlePhaseFired(sig ekPhaseSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.Game.AfkSeq != sig.Seq {
		return
	}
	game := room.Game
	switch game.Phase {
	case EKPhaseTurn:
		seat := game.CurrentSeat
		game.AutoDraw(h.rng)
		log.Printf("[키튼][자동진행] game=%s | seat%d 무응답 — 자동 뽑기", game.ID, seat)

	case EKPhaseNopeWindow:
		if game.Pending == nil {
			return
		}
		log.Printf("[키튼][창통과] game=%s | 노프 창 마감 — 통과 (겹침 %d)",
			game.ID, game.Pending.NopeCount)
		game.ForcePassWindow(h.rng)

	case EKPhaseFavorWait:
		game.AutoGive(h.rng)
		log.Printf("[키튼][자동부탁] game=%s | 무응답 — 무작위 카드", game.ID)

	case EKPhaseDefusePlace:
		game.AutoDefusePlace(h.rng)
		log.Printf("[키튼][자동되꽂기] game=%s | 무응답 — 무작위 위치", game.ID)

	default:
		return
	}
	h.afterProgress(room)
}

// ==================== 상태 뷰 (은닉의 핵심) ====================

// buildEKState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
//
// 은닉:
//   - yourHand 는 좌석 보유자 본인에게만 실린다 (포인터 — 타인·관전자는 키 부재).
//   - 덱 내용·폭탄 위치는 어디에도 없다. deckLeft 장수와 discardTop 만 공개.
//   - viewerSeat -1(관전자)에서도 절대 패닉하지 않는다.
func (h *EKHub) buildEKState(room *ekRoom, viewerSeat int) EKGameStatePayload {
	game := room.Game

	var yourHand *[]EKHandCardView
	if viewerSeat >= 0 && viewerSeat < len(game.Players) {
		hand := []EKHandCardView{}
		for _, c := range game.Players[viewerSeat].Hand {
			hand = append(hand, EKHandCardView{Kind: string(c)})
		}
		yourHand = &hand
	}

	players := []EKPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		players = append(players, EKPlayerView{
			Seat:      p.Seat,
			Name:      p.Name,
			Connected: c != nil && c.Connected,
			Bot:       c != nil && c.Bot,
			HandCount: len(p.Hand),
			Alive:     p.Alive,
		})
	}

	var pending *EKPendingView
	if game.Pending != nil {
		pending = &EKPendingView{
			Kind:       game.Pending.Kind,
			BySeat:     game.Pending.BySeat,
			TargetSeat: game.Pending.TargetSeat,
			NopeCount:  game.Pending.NopeCount,
		}
	}

	var result *EKResultView
	if game.Phase == EKPhaseGameOver {
		name := ""
		if game.WinnerSeat >= 0 && game.WinnerSeat < len(game.Players) {
			name = game.Players[game.WinnerSeat].Name
		}
		result = &EKResultView{
			WinnerSeat: game.WinnerSeat,
			WinnerName: name,
			Message:    fmt.Sprintf("%s님이 최후의 1인으로 살아남았습니다!", name),
		}
	}

	endsAt := int64(0)
	switch game.Phase {
	case EKPhaseTurn, EKPhaseNopeWindow, EKPhaseFavorWait, EKPhaseDefusePlace:
		endsAt = game.Deadline
	}

	return EKGameStatePayload{
		GameID:      game.ID,
		RoomCode:    room.Code,
		Phase:       game.Phase,
		HostSeat:    h.hostSeat(room),
		YourSeat:    viewerSeat,
		Spectators:  len(room.Spectators),
		EndsAt:      endsAt,
		CurrentSeat: game.CurrentSeat,
		TurnsLeft:   game.TurnsLeft,
		DeckLeft:    len(game.Deck),
		DiscardTop:  game.discardTop(),
		Pending:     pending,
		YourHand:    yourHand,
		Players:     players,
		LastAction:  game.LastAction,
		Result:      result,
	}
}

// broadcastState 좌석마다 개인화 스냅샷을 만들어 보낸다.
// 관전자에게는 공개 정보만 담긴 스냅샷(viewerSeat -1)이 간다.
func (h *EKHub) broadcastState(room *ekRoom) {
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, EKMessage{
			Type:    EKMsgGameState,
			Payload: h.buildEKState(room, seat),
		})
	}
	if len(room.Spectators) > 0 {
		msg := EKMessage{Type: EKMsgGameState, Payload: h.buildEKState(room, -1)}
		for c := range room.Spectators {
			h.sendToClient(c, msg)
		}
	}
}

func (h *EKHub) broadcastEvent(room *ekRoom, event EKEventPayload) {
	h.broadcastToRoom(room, EKMessage{Type: EKMsgEvent, Payload: event})
}

// finishGame 게임 종료 발표·전적 기록·방 정리 (재대결은 같은 방 코드
// 재입장으로 — finishGame 이 코드를 반납한다)
func (h *EKHub) finishGame(room *ekRoom) {
	game := room.Game
	h.stopPhaseTimer(room)
	game.Deadline = 0

	winner := game.WinnerSeat
	winnerName := ""
	if winner >= 0 && winner < len(game.Players) {
		winnerName = game.Players[winner].Name
	}
	names := []string{}
	for _, p := range game.Players {
		names = append(names, displayName(p.Name))
	}

	h.broadcastEvent(room, EKEventPayload{Kind: "game_over", Seat: &winner,
		Name:    winnerName,
		Message: fmt.Sprintf("%s님이 최후의 1인으로 살아남았습니다!", winnerName)})
	// 최종 스냅샷을 먼저 보낸 뒤 종료를 알린다 (봇 러너는 ek_game_over 로 끝난다)
	h.broadcastState(room)
	h.broadcastToRoom(room, EKMessage{
		Type: EKMsgGameOver,
		Payload: EKGameOverPayload{
			WinnerSeat: winner,
			WinnerName: winnerName,
			Players:    h.buildEKState(room, -1).Players,
		},
	})

	log.Printf("[키튼][경기결과] game=%s | 승자=seat%d(%s) | 소요=%s",
		game.ID, winner, displayName(winnerName), matchDuration(game.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "kittens",
		Players:  strings.Join(names, " vs "),
		Winner:   displayName(winnerName),
		Reason:   "last_standing",
		Duration: matchSeconds(game.StartedAt),
		Bot:      ekRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
	if room.Code != "" {
		delete(h.activeCodes, room.Code)
	}
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *EKHub) handleDisconnect(client *EKClient) {
	// 순수 관전자 연결 종료 — 세션·유예 없이 목록에서만 뗀다
	if room := h.rooms[client.GameID]; room != nil && room.Spectators[client] {
		delete(room.Spectators, client)
		h.broadcastState(room)
		return
	}
	if client.SessionID == "" {
		return
	}
	if !h.isCurrent(client) {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil {
		h.drop(client.SessionID)
		return
	}

	// 대기 단계: 유예 없이 즉시 좌석을 비운다
	if !room.Game.Ready {
		h.removeFromLobby(room, client)
		return
	}

	log.Printf("[키튼][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, EKMessage{
		Type: EKMsgPlayerDisconnected,
		Payload: EKPlayerDisconnectedPayload{
			Seat:         client.Seat,
			Name:         client.Name,
			GraceSeconds: int(h.grace.Seconds()),
		},
	})
	h.broadcastState(room)

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 안에 재접속하지 않은 좌석은 연습봇으로 대체한다 —
// 노프 창 통과·되꽂기가 이탈 좌석에 막히지 않는 근거
func (h *EKHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == EKPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[키튼][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := h.takeoverEKBot(room, seat, client.Name)
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.broadcastEvent(room, EKEventPayload{Kind: "bot_takeover", Seat: &seat,
		Name:    client.Name,
		Message: fmt.Sprintf("%s님 자리를 봇이 이어받았습니다", client.Name)})
	// 새 봇이 이 스냅샷을 받아 응답이 남았으면 즉시 이어간다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *EKHub) handleRejoin(client *EKClient, msg EKMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload EKRejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, EKMessage{Type: EKMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, EKMessage{Type: EKMsgSessionExpired})
		return
	}

	h.cancelGrace(payload.SessionID)

	if old != client && old.Connected {
		old.Conn.Close()
	}

	client.SessionID = old.SessionID
	client.Name = old.Name
	client.GameID = old.GameID
	client.Seat = old.Seat
	h.sessions[client.SessionID] = client
	room.Clients[client.Seat] = client

	log.Printf("[키튼][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, EKMessage{
		Type:    EKMsgPlayerReconnected,
		Payload: EKPlayerReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	h.broadcastState(room)
}

func (h *EKHub) clearGameSessions(room *ekRoom) {
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		h.drop(c.SessionID)
	}
}

// ==================== 전송 ====================

func (h *EKHub) sendError(client *EKClient, message string) {
	h.sendToClient(client, EKMessage{Type: EKMsgError, Payload: EKErrorPayload{Message: message}})
}

func (h *EKHub) sendToClient(client *EKClient, message EKMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[EK] ")
}

func (h *EKHub) broadcastToRoom(room *ekRoom, message EKMessage) {
	for _, c := range room.Clients {
		if c != nil {
			h.sendToClient(c, message)
		}
	}
	for c := range room.Spectators {
		h.sendToClient(c, message)
	}
}

// ==================== WS 엔드포인트 ====================

func ServeEKWs(hub *EKHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[EK] Error upgrading connection:", err)
		return
	}

	client := &EKClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[EK] ",
		func(msg EKMessage) { hub.gameMessage <- EKGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
