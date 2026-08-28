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

// 스플렌더 대기 상태 마감 타이머 — 차례 60초 무응답은 가능한 행동 하나로,
// 버리기 20초 무응답은 무작위 버리기로 해소한다 (테스트에서 짧게 낮춘다).
var (
	slTurnTimeout    = 60 * time.Second
	slDiscardTimeout = 20 * time.Second
)

// slRoom 게임(순수 상태)과 좌석별 연결의 매핑
type slRoom struct {
	Game       *SLGame
	Clients    map[int]*SLClient // seat → client
	PhaseTimer *time.Timer       // 대기 상태 마감 타이머

	// DeadlineSeq 마지막으로 마감을 건 StateSeq — 같은 차례에 스냅샷이
	// 쌓일 때마다(관전 입장·접속 변화 등) 마감이 늘어나지 않게 하는 근거
	DeadlineSeq int

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string

	// Spectators 관전자 연결 (사설 방 코드로만 진입, 상한 maxSpectators).
	// 좌석·세션이 없어 재접속을 지원하지 않는다 — 끊기면 목록에서만 뗀다.
	Spectators map[*SLClient]bool

	// LastReact 좌석별 마지막 리액션 발신 시각 (레이트리밋 장부)
	LastReact map[int]time.Time
}

// slPhaseSignal 마감 타이머의 발화 표식 — 대기 상태 일련번호(AfkSeq)로
// 지나간 발화를 구분한다
type slPhaseSignal struct {
	GameID string
	Seq    int
}

type SLHub struct {
	clients map[*SLClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*slRoom

	// 글로벌 로비. 시작 전 공용 방은 하나뿐이다.
	lobby *slRoom

	// 사설 방 (초대 코드 → 시작 전 방). 허브 고루틴에서만 접근한다.
	privateLobbies map[string]*slRoom

	// 진행 중 사설 방 (초대 코드 → gameID). 시작 후에도 코드로 방을 찾아
	// 관전 입장시키는 근거다. finishGame 에서 해제한다 (코드 재사용 복귀).
	activeCodes map[string]string

	register    chan *SLClient
	unregister  chan *SLClient
	gameMessage chan SLGameMessage
	phaseFired  chan slPhaseSignal

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*SLClient]

	// 덱 셔플·자동 진행용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type SLGameMessage struct {
	Client  *SLClient
	Message SLMessage
}

func NewSLHub() *SLHub {
	return &SLHub{
		register:       make(chan *SLClient),
		unregister:     make(chan *SLClient),
		clients:        make(map[*SLClient]bool),
		rooms:          make(map[string]*slRoom),
		privateLobbies: make(map[string]*slRoom),
		activeCodes:    make(map[string]string),
		gameMessage:    make(chan SLGameMessage),
		phaseFired:     make(chan slPhaseSignal, 8),
		sessionManager: newSessionManager[*SLClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *SLHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[SL] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[SL] Client unregistered: %s", client.ID)
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

func (h *SLHub) handleGameMessage(gm SLGameMessage) {
	// 관전자는 어떤 행동도 할 수 없다 (보기 전용 — 리액션도 좌석 보유자만)
	if h.isSpectator(gm.Client) {
		h.sendError(gm.Client, spectatorDeniedMsg)
		return
	}
	switch gm.Message.Type {
	case SLMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case SLMsgReact:
		h.handleReact(gm.Client, gm.Message)
	case SLMsgFillBots:
		h.handleFillBots(gm.Client)
	case SLMsgStart:
		h.handleStart(gm.Client)
	case SLMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case SLMsgTake:
		h.handleTake(gm.Client, gm.Message)
	case SLMsgReserve:
		h.handleReserve(gm.Client, gm.Message)
	case SLMsgBuy:
		h.handleBuy(gm.Client, gm.Message)
	case SLMsgDiscard:
		h.handleDiscard(gm.Client, gm.Message)
	}
}

// ==================== 대기실 ====================

func (h *SLHub) handleJoinGame(client *SLClient, msg SLMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SLJoinGamePayload
	json.Unmarshal(payloadBytes, &payload)

	// 이미 시작된 사설 방의 코드로 들어오면 에러 대신 관전자로 입장시킨다
	if gameID, ok := h.activeCodes[normalizeRoomCode(payload.Room)]; ok {
		h.addSpectator(h.rooms[gameID], client, payload.Name)
		return
	}

	room := h.lobbyRoomFor(payload.Room)
	seat, err := room.Game.AddPlayer(payload.Name)
	if err != nil {
		// 사설 방이 가득 차 못 앉는 경우도 관전 진입 (공용 로비는 기존 에러)
		if room.Code != "" {
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

	log.Printf("[스플렌더][입장] game=%s | seat%d=%s 대기 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), SLMaxPlayers)
	if room.Code == "" { // 사설 방 입장은 조용히 (공용 로비만 알림)
		notify("스플렌더 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), len(room.Game.Players), SLMaxPlayers))
	}

	h.sendToClient(client, SLMessage{
		Type: SLMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
			"roomCode":  room.Code,
		},
	})

	h.broadcastEvent(room, SLEventPayload{Kind: "joined", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 입장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	// 정원이 차도 자동 시작하지 않는다 — 호스트 명시 시작만
	h.broadcastState(room)
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다.
// 생략/빈 문자열은 공용 로비, "NEW"는 새 코드 발급, 그 외 코드는 해당 사설 방
// (없으면 그 코드로 관대하게 새로 생성).
func (h *SLHub) lobbyRoomFor(roomField string) *slRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewSLGame(uuid.New().String())
			h.lobby = &slRoom{Game: game, Clients: map[int]*SLClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[SL] Created lobby game %s", game.ID)
		}
		return h.lobby
	}
	if code == roomCodeNew {
		taken := takenCodes(h.privateLobbies)
		for c := range h.activeCodes { // 진행 중 방의 코드도 재사용 금지
			taken[c] = true
		}
		code = generateRoomCode(h.rng, taken)
	}
	room := h.privateLobbies[code]
	if room == nil {
		game := NewSLGame(uuid.New().String())
		room = &slRoom{Game: game, Clients: map[int]*SLClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[SL] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// ==================== 관전 / 리액션 ====================

// addSpectator 진행 중(또는 가득 찬) 사설 방에 관전자로 등록한다.
// 좌석·세션 없음 — 재접속 미지원, 끊기면 다시 코드로 들어온다.
func (h *SLHub) addSpectator(room *slRoom, client *SLClient, name string) {
	if len(room.Spectators) >= maxSpectators {
		h.sendError(client, spectatorFullMsg)
		return
	}
	if room.Spectators == nil {
		room.Spectators = map[*SLClient]bool{}
	}
	client.Name = name
	client.GameID = room.Game.ID
	room.Spectators[client] = true

	log.Printf("[스플렌더][관전] game=%s | %s 관전 입장 (%d명)",
		room.Game.ID, displayName(name), len(room.Spectators))

	h.sendToClient(client, SLMessage{
		Type:    SLMsgSpectateJoined,
		Payload: map[string]interface{}{"gameId": room.Game.ID, "roomCode": room.Code},
	})
	h.broadcastState(room)
}

// isSpectator 관전자 연결인지 (좌석 보유자·미입장 연결은 false)
func (h *SLHub) isSpectator(client *SLClient) bool {
	if client.GameID == "" {
		return false
	}
	room := h.rooms[client.GameID]
	return room != nil && room.Spectators[client]
}

// handleReact 리액션 이모지 — 좌석 보유자만, waiting 중에도 허용.
// 화이트리스트 외·레이트리밋 초과는 조용히 무시한다 (상태 저장 없음).
func (h *SLHub) handleReact(client *SLClient, msg SLMessage) {
	room := h.rooms[client.GameID]
	if room == nil || client.Seat < 0 {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SLReactPayload
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
	h.broadcastEvent(room, SLEventPayload{Kind: "react", Seat: &seat, Name: client.Name,
		Message: payload.Emoji})
}

// waitingRoomOf 클라이언트가 속한 시작 전 방 (공용 로비 또는 사설 방)
func (h *SLHub) waitingRoomOf(client *SLClient) *slRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Ready {
		return nil
	}
	return room
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *SLHub) hostSeat(room *slRoom) int {
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

// slHumanCount 방의 사람 수
func slHumanCount(room *slRoom) int {
	n := 0
	for _, c := range room.Clients {
		if c != nil && !c.Bot {
			n++
		}
	}
	return n
}

// updateLobbyWaiting 로비 현황판 갱신 — 사람 1명 이상 대기 && 미시작.
// 사설 방은 현황판에 노출하지 않는다 (초대 링크로만 접근).
func (h *SLHub) updateLobbyWaiting(room *slRoom) {
	if room.Code != "" {
		return
	}
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && slHumanCount(room) >= 1
	lobbySetWaiting("splendor", waiting)
}

// handleFillBots host 가 빈 좌석을 연습봇으로 3인까지 채운 뒤 즉시 시작한다
// (스폰은 join 을 거치지 않으므로 명시 시작 호출).
func (h *SLHub) handleFillBots(client *SLClient) {
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
	for len(room.Game.Players) < SLFillBotTarget {
		botNo++
		if !h.spawnSLBot(room, fmt.Sprintf("%s%d", botName, botNo)) {
			break
		}
	}

	if room.Game.CanStart() {
		h.startGame(room)
		return
	}
	h.broadcastState(room)
}

func (h *SLHub) handleStart(client *SLClient) {
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
		h.sendError(client, fmt.Sprintf("%d명 이상 모여야 시작할 수 있습니다", SLMinPlayers))
		return
	}
	h.startGame(room)
}

func (h *SLHub) startGame(room *slRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	if room.Code != "" {
		// 시작한 사설 방의 코드는 진행 중 장부로 옮긴다 (관전 입장 근거)
		delete(h.privateLobbies, room.Code)
		h.activeCodes[room.Code] = room.Game.ID
	} else {
		h.lobby = nil
		lobbySetWaiting("splendor", false)
	}

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	n := len(room.Game.Players)
	log.Printf("[스플렌더][경기시작] game=%s | 인원=%d | 공동 창고 색당 %d개 | 귀족 타일 %d장 | 선=seat%d | %v",
		room.Game.ID, n, slBankFor(n), len(room.Game.Nobles), room.Game.CurrentSeat, names)
	if !slRoomHasBot(room) { // 연습봇전은 운영자 알림을 억제한다
		notify("스플렌더 시작", fmt.Sprintf("%d인전 시작", n))
	}

	h.broadcastEvent(room, SLEventPayload{Kind: "game_started",
		Message: fmt.Sprintf(
			"게임 시작 — %d인전, 공동 창고는 보석 색마다 %d개·황금 %d개입니다. 명성 점수 %d점에 먼저 닿으면 그 라운드까지만 진행합니다",
			n, slBankFor(n), SLGoldCount, SLWinPoints)})
	h.afterProgress(room)
}

// removeFromLobby 대기실에서 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *SLHub) removeFromLobby(room *slRoom, client *SLClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	rebuilt := map[int]*SLClient{}
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

	log.Printf("[스플렌더][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), SLMaxPlayers)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if slHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, SLMessage{Type: SLMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			lobbySetWaiting("splendor", false)
		}
		return
	}

	h.broadcastEvent(room, SLEventPayload{Kind: "left", Name: client.Name,
		Message: fmt.Sprintf("%s님이 퇴장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *SLHub) roomOf(client *SLClient) *slRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

// handleTake 토큰 가져오기 (서로 다른 색 3개 또는 같은 색 2개)
func (h *SLHub) handleTake(client *SLClient, msg SLMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SLTakePayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.Take(client.Seat, payload.Colors); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[스플렌더][토큰] game=%s | seat%d=%s → %v (공동 창고 %+v)",
		room.Game.ID, client.Seat, displayName(client.Name), payload.Colors, room.Game.Bank)
	h.afterProgress(room)
}

// handleReserve 개발 카드 예약 (+ 황금 1개)
func (h *SLHub) handleReserve(client *SLClient, msg SLMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SLReservePayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.Reserve(client.Seat, payload.CardID, payload.Tier); err != nil {
		h.sendError(client, err.Error())
		return
	}
	// 비공개 예약의 내용은 로그에도 남기지 않는다 (은닉 계약)
	log.Printf("[스플렌더][예약] game=%s | seat%d=%s cardId=%d tier=%d",
		room.Game.ID, client.Seat, displayName(client.Name), payload.CardID, payload.Tier)
	h.afterProgress(room)
}

// handleBuy 개발 카드 구매
func (h *SLHub) handleBuy(client *SLClient, msg SLMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SLBuyPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.Buy(client.Seat, payload.CardID); err != nil {
		h.sendError(client, err.Error())
		return
	}
	seat := client.Seat
	points := 0
	if seat >= 0 && seat < len(room.Game.Players) {
		points = room.Game.Players[seat].Points
	}
	log.Printf("[스플렌더][구매] game=%s | seat%d=%s cardId=%d → 명성 점수 %d",
		room.Game.ID, seat, displayName(client.Name), payload.CardID, points)
	h.afterProgress(room)
}

// handleDiscard 10개 초과분 버리기
func (h *SLHub) handleDiscard(client *SLClient, msg SLMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SLDiscardPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.Discard(client.Seat, payload.Colors); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.afterProgress(room)
}

// afterProgress 모든 게임 변이 뒤의 공통 마무리 — 이벤트 방송·종료 판정·
// 새 대기 상태의 마감 예약·스냅샷 방송을 한 번에 처리한다.
func (h *SLHub) afterProgress(room *slRoom) {
	h.drainEvents(room)
	if room.Game.Phase == SLPhaseGameOver {
		h.finishGame(room)
		return
	}
	h.syncDeadline(room)
	h.broadcastState(room)
}

// drainEvents 순수 규칙이 쌓은 이벤트를 sl_event 로 방송한다
func (h *SLHub) drainEvents(room *slRoom) {
	for _, ev := range room.Game.DrainEvents() {
		payload := SLEventPayload{Kind: ev.Kind, Message: ev.Message}
		if ev.Seat >= 0 && ev.Seat < len(room.Game.Players) {
			seat := ev.Seat
			payload.Seat = &seat
			payload.Name = room.Game.Players[seat].Name
		}
		h.broadcastEvent(room, payload)
	}
}

// ==================== 대기 상태 마감 타이머 (AFK 진행 보장) ====================

// syncDeadline 새 대기 상태(StateSeq 변경)가 열렸을 때만 마감을 다시 건다.
// 같은 차례에 스냅샷이 쌓여도(관전 입장·접속 변화 등) 마감은 늘어나지 않는다.
func (h *SLHub) syncDeadline(room *slRoom) {
	game := room.Game
	if room.DeadlineSeq == game.StateSeq {
		return
	}
	room.DeadlineSeq = game.StateSeq
	var dur time.Duration
	switch game.Phase {
	case SLPhaseTurn:
		dur = slTurnTimeout
	case SLPhaseDiscard:
		dur = slDiscardTimeout
	default:
		h.stopPhaseTimer(room)
		return
	}
	h.scheduleDeadline(room, dur)
}

// scheduleDeadline 마감 타이머 예약. AfkSeq 를 올려 지나간 발화를 구분하고,
// 발화는 허브 채널을 경유하므로 허브 밖에서 상태를 만지지 않는다.
func (h *SLHub) scheduleDeadline(room *slRoom, dur time.Duration) {
	h.stopPhaseTimer(room)
	room.Game.AfkSeq++
	room.Game.Deadline = time.Now().Add(dur).UnixMilli()
	sig := slPhaseSignal{GameID: room.Game.ID, Seq: room.Game.AfkSeq}
	room.PhaseTimer = time.AfterFunc(dur, func() {
		h.phaseFired <- sig
	})
}

func (h *SLHub) stopPhaseTimer(room *slRoom) {
	if room.PhaseTimer != nil {
		room.PhaseTimer.Stop()
		room.PhaseTimer = nil
	}
}

// handlePhaseFired 마감 발화 — 단계별 자동 행동으로 해소한다.
//   - turn:    가능한 행동 하나 (토큰 3색 우선, 없으면 구매, 없으면 예약)
//   - discard: 무작위로 버려 10개를 맞춘다
func (h *SLHub) handlePhaseFired(sig slPhaseSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.Game.AfkSeq != sig.Seq {
		return
	}
	game := room.Game
	seat := game.CurrentSeat
	if seat < 0 || seat >= len(game.Players) {
		return
	}
	actor := game.Players[seat]

	switch game.Phase {
	case SLPhaseTurn:
		h.broadcastEvent(room, SLEventPayload{Kind: "afk", Seat: &seat, Name: actor.Name,
			Message: fmt.Sprintf("%s님이 오래 응답하지 않아 자동으로 행동합니다", actor.Name)})
		game.ForceAction(h.rng)
		log.Printf("[스플렌더][자동진행] game=%s | seat%d 무응답 — 자동 행동", game.ID, seat)

	case SLPhaseDiscard:
		h.broadcastEvent(room, SLEventPayload{Kind: "afk", Seat: &seat, Name: actor.Name,
			Message: fmt.Sprintf("%s님이 오래 응답하지 않아 자동으로 토큰을 버립니다", actor.Name)})
		game.ForceDiscard(h.rng)
		log.Printf("[스플렌더][자동진행] game=%s | seat%d 무응답 — 자동 버리기", game.ID, seat)

	default:
		return
	}
	h.afterProgress(room)
}

// ==================== 상태 뷰 (은닉의 핵심) ====================

// slBoardView 단계별 공개 진열 (빈 단계도 [] 로 나간다)
func slBoardView(game *SLGame) SLBoardView {
	return SLBoardView{
		Tier1: append([]SLCard{}, game.Board[0]...),
		Tier2: append([]SLCard{}, game.Board[1]...),
		Tier3: append([]SLCard{}, game.Board[2]...),
	}
}

// buildSLState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
//
// 은닉: yourReserved 는 본인에게만 실린다 — 타인·관전자(viewerSeat -1)의 raw
// JSON 에는 키 자체가 없다 (nil 포인터 + omitempty). 예약 카드의 내용은 이
// 필드 말고는 어떤 경로로도 나가지 않으므로, 덱 맨 위에서 비공개로 예약한
// 카드는 남에게 players[].reservedCount 숫자로만 보인다.
// 빈 예약도 [] 로 보내야 하므로 슬라이스 포인터를 쓴다.
func (h *SLHub) buildSLState(room *slRoom, viewerSeat int) SLGameStatePayload {
	game := room.Game
	seated := viewerSeat >= 0 && viewerSeat < len(game.Players)

	var yourReserved *[]SLCard
	if seated && game.Ready {
		reserved := append([]SLCard{}, game.Players[viewerSeat].Reserved...)
		yourReserved = &reserved
	}

	players := []SLPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		players = append(players, SLPlayerView{
			Seat:          p.Seat,
			Name:          p.Name,
			Connected:     c != nil && c.Connected,
			Bot:           c != nil && c.Bot,
			Points:        p.Points,
			Cards:         p.Cards,
			Tokens:        p.Tokens,
			ReservedCount: len(p.Reserved),
			Nobles:        append([]int{}, p.Nobles...),
		})
	}

	endsAt := int64(0)
	switch game.Phase {
	case SLPhaseTurn, SLPhaseDiscard:
		endsAt = game.Deadline
	}

	return SLGameStatePayload{
		GameID:      game.ID,
		RoomCode:    room.Code,
		Phase:       game.Phase,
		HostSeat:    h.hostSeat(room),
		YourSeat:    viewerSeat,
		Spectators:  len(room.Spectators),
		EndsAt:      endsAt,
		CurrentSeat: game.CurrentSeat,
		LastRound:   game.LastRound,
		Bank:        game.Bank,
		Board:       slBoardView(game),
		DeckLeft: SLDeckLeft{
			Tier1: len(game.Decks[0]),
			Tier2: len(game.Decks[1]),
			Tier3: len(game.Decks[2]),
		},
		Nobles:       append([]SLNoble{}, game.Nobles...),
		YourReserved: yourReserved,
		Players:      players,
		LastAction:   game.LastAction,
		Result:       game.Result,
	}
}

// broadcastState 좌석마다 개인화 스냅샷을 만들어 보낸다.
// 관전자에게는 공개 정보만 담긴 스냅샷(viewerSeat -1)이 간다.
func (h *SLHub) broadcastState(room *slRoom) {
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, SLMessage{
			Type:    SLMsgGameState,
			Payload: h.buildSLState(room, seat),
		})
	}
	if len(room.Spectators) > 0 {
		msg := SLMessage{Type: SLMsgGameState, Payload: h.buildSLState(room, -1)}
		for c := range room.Spectators {
			h.sendToClient(c, msg)
		}
	}
}

func (h *SLHub) broadcastEvent(room *slRoom, event SLEventPayload) {
	h.broadcastToRoom(room, SLMessage{Type: SLMsgEvent, Payload: event})
}

// finishGame 게임 종료 발표·전적 기록·방 정리 (재대결은 같은 방 코드
// 재입장으로 — finishGame 이 코드를 반납해 같은 코드로 새 방을 만들 수 있다)
func (h *SLHub) finishGame(room *slRoom) {
	game := room.Game
	h.stopPhaseTimer(room)
	game.Deadline = 0

	result := game.Result
	if result == nil { // 방어선 — 종료는 항상 결과와 함께 온다
		result = &SLResult{WinnerSeats: []int{}, WinnerNames: []string{},
			Message: "게임이 종료됐습니다"}
	}

	winnerSeats := map[int]bool{}
	for _, s := range result.WinnerSeats {
		winnerSeats[s] = true
	}
	winners, losers := []string{}, []string{}
	for _, p := range game.Players {
		if winnerSeats[p.Seat] {
			winners = append(winners, displayName(p.Name))
		} else {
			losers = append(losers, displayName(p.Name))
		}
	}

	h.broadcastEvent(room, SLEventPayload{Kind: "game_over", Message: result.Message})
	// 최종 스냅샷을 먼저 보낸 뒤 종료를 알린다
	// (봇 러너는 sl_game_over 를 보고 스스로 끝난다)
	h.broadcastState(room)
	h.broadcastToRoom(room, SLMessage{
		Type: SLMsgGameOver,
		Payload: SLGameOverPayload{
			WinnerSeats: append([]int{}, result.WinnerSeats...),
			WinnerNames: append([]string{}, result.WinnerNames...),
			Message:     result.Message,
			Turns:       game.Turns,
			Players:     h.buildSLState(room, -1).Players,
		},
	})

	scores := []string{}
	for _, p := range game.Players {
		scores = append(scores, fmt.Sprintf("%s %d점(개발 카드 %d)",
			displayName(p.Name), p.Points, p.Cards.total()))
	}
	log.Printf("[스플렌더][경기결과] game=%s | 승자=%s | 차례=%d | 소요=%s | %s",
		game.ID, strings.Join(winners, "·"), game.Turns,
		matchDuration(game.StartedAt), strings.Join(scores, " / "))

	RecordMatch(MatchRecord{
		Game:     "splendor",
		Players:  strings.Join(winners, "·") + " vs " + strings.Join(losers, "·"),
		Winner:   strings.Join(winners, "·"),
		Reason:   "points",
		Duration: matchSeconds(game.StartedAt),
		Bot:      slRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
	if room.Code != "" {
		delete(h.activeCodes, room.Code) // 코드 재사용 복귀
	}
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *SLHub) handleDisconnect(client *SLClient) {
	// 관전자 연결 종료 — 세션·유예 없이 목록에서만 뗀다
	if room := h.rooms[client.GameID]; room != nil && room.Spectators[client] {
		delete(room.Spectators, client)
		h.broadcastState(room) // 관전자 수 갱신
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

	// 진행 중: 유예 시간 동안 재접속을 기다린다 (만료 시 봇 대체)
	log.Printf("[스플렌더][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, SLMessage{
		Type: SLMsgPlayerDisconnected,
		Payload: SLPlayerDisconnectedPayload{
			Seat:         client.Seat,
			Name:         client.Name,
			GraceSeconds: int(h.grace.Seconds()),
		},
	})
	h.broadcastState(room)

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 안에 재접속하지 않은 좌석은 연습봇으로 대체하고
// 게임은 계속한다 — 차례가 이탈 좌석에 막히지 않는 근거
func (h *SLHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == SLPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[스플렌더][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := h.takeoverSLBot(room, seat, client.Name)
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.broadcastEvent(room, SLEventPayload{Kind: "bot_takeover", Seat: &seat,
		Name:    client.Name,
		Message: fmt.Sprintf("%s님 자리를 봇이 이어받았습니다", client.Name)})
	// 새 봇이 이 스냅샷을 받아 자기 차례면 즉시 이어간다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *SLHub) handleRejoin(client *SLClient, msg SLMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SLRejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, SLMessage{Type: SLMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, SLMessage{Type: SLMsgSessionExpired})
		return
	}

	h.cancelGrace(payload.SessionID)

	// 옛 연결이 아직 살아있다면 강제 종료 (중복 접속 방지)
	if old != client && old.Connected {
		old.Conn.Close()
	}

	// 신원 인계: 새 연결이 기존 좌석을 이어받는다
	client.SessionID = old.SessionID
	client.Name = old.Name
	client.GameID = old.GameID
	client.Seat = old.Seat
	h.sessions[client.SessionID] = client
	room.Clients[client.Seat] = client

	log.Printf("[스플렌더][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, SLMessage{
		Type:    SLMsgPlayerReconnected,
		Payload: SLPlayerReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *SLHub) clearGameSessions(room *slRoom) {
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		h.drop(c.SessionID)
	}
}

// ==================== 전송 ====================

func (h *SLHub) sendError(client *SLClient, message string) {
	h.sendToClient(client, SLMessage{Type: SLMsgError, Payload: SLErrorPayload{Message: message}})
}

func (h *SLHub) sendToClient(client *SLClient, message SLMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[SL] ")
}

func (h *SLHub) broadcastToRoom(room *slRoom, message SLMessage) {
	for _, c := range room.Clients {
		if c != nil {
			h.sendToClient(c, message)
		}
	}
	for c := range room.Spectators { // 이벤트·종료 발표는 관전자에게도 간다
		h.sendToClient(c, message)
	}
}

// ==================== WS 엔드포인트 ====================

func ServeSLWs(hub *SLHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[SL] Error upgrading connection:", err)
		return
	}

	client := &SLClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[SL] ",
		func(msg SLMessage) { hub.gameMessage <- SLGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
