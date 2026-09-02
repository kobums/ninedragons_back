package server

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// 스타트업스 대기 상태 마감 타이머 — 차례 45초 무응답은 자동으로 시장 최상단
// 또는 덱에서 가져오고 손패 무작위 1장을 낸다 (테스트에서 짧게 낮춘다).
var suTurnTimeout = 45 * time.Second

// suRoom 게임(순수 상태)과 좌석별 연결의 매핑
type suRoom struct {
	Game       *SUGame
	Clients    map[int]*SUClient // seat → client
	PhaseTimer *time.Timer       // 대기 상태 마감 타이머

	// DeadlineSeq 마지막으로 마감을 건 StateSeq — 같은 차례에 스냅샷이
	// 쌓일 때마다(관전 입장·접속 변화 등) 마감이 늘어나지 않게 하는 근거
	DeadlineSeq int

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string

	// Spectators 관전자 연결 (사설 방 코드로만 진입, 상한 maxSpectators).
	// 좌석·세션이 없어 재접속을 지원하지 않는다 — 끊기면 목록에서만 뗀다.
	Spectators map[*SUClient]bool

	// LastReact 좌석별 마지막 리액션 발신 시각 (레이트리밋 장부)
	LastReact map[int]time.Time
}

// suPhaseSignal 마감 타이머의 발화 표식 — 대기 상태 일련번호(AfkSeq)로
// 지나간 발화를 구분한다
type suPhaseSignal struct {
	GameID string
	Seq    int
}

type SUHub struct {
	clients map[*SUClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*suRoom

	// 글로벌 로비. 시작 전 공용 방은 하나뿐이다.
	lobby *suRoom

	// 사설 방 (초대 코드 → 시작 전 방). 허브 고루틴에서만 접근한다.
	privateLobbies map[string]*suRoom

	// 진행 중 사설 방 (초대 코드 → gameID). 시작 후에도 코드로 방을 찾아
	// 관전 입장시키는 근거다. finishGame 에서 해제한다 (코드 재사용 복귀).
	activeCodes map[string]string

	register    chan *SUClient
	unregister  chan *SUClient
	gameMessage chan SUGameMessage
	phaseFired  chan suPhaseSignal

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*SUClient]

	// 덱 셔플·자동 진행용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type SUGameMessage struct {
	Client  *SUClient
	Message SUMessage
}

func NewSUHub() *SUHub {
	return &SUHub{
		register:       make(chan *SUClient),
		unregister:     make(chan *SUClient),
		clients:        make(map[*SUClient]bool),
		rooms:          make(map[string]*suRoom),
		privateLobbies: make(map[string]*suRoom),
		activeCodes:    make(map[string]string),
		gameMessage:    make(chan SUGameMessage),
		phaseFired:     make(chan suPhaseSignal, 8),
		sessionManager: newSessionManager[*SUClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *SUHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[SU] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[SU] Client unregistered: %s", client.ID)
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

func (h *SUHub) handleGameMessage(gm SUGameMessage) {
	// 관전자는 어떤 행동도 할 수 없다 (보기 전용 — 리액션도 좌석 보유자만)
	if h.isSpectator(gm.Client) {
		h.sendError(gm.Client, spectatorDeniedMsg)
		return
	}
	switch gm.Message.Type {
	case SUMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case SUMsgReact:
		h.handleReact(gm.Client, gm.Message)
	case SUMsgFillBots:
		h.handleFillBots(gm.Client)
	case SUMsgStart:
		h.handleStart(gm.Client)
	case SUMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case SUMsgTake:
		h.handleTake(gm.Client, gm.Message)
	case SUMsgPlay:
		h.handlePlay(gm.Client, gm.Message)
	}
}

// ==================== 대기실 ====================

func (h *SUHub) handleJoinGame(client *SUClient, msg SUMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SUJoinGamePayload
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

	log.Printf("[스타트업스][입장] game=%s | seat%d=%s 대기 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), SUMaxPlayers)
	if room.Code == "" { // 사설 방 입장은 조용히 (공용 로비만 알림)
		notify("스타트업스 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), len(room.Game.Players), SUMaxPlayers))
	}

	h.sendToClient(client, SUMessage{
		Type: SUMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
			"roomCode":  room.Code,
		},
	})

	h.broadcastEvent(room, SUEventPayload{Kind: "joined", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 입장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	// 정원이 차도 자동 시작하지 않는다 — 호스트 명시 시작만
	h.broadcastState(room)
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다.
// 생략/빈 문자열은 공용 로비, "NEW"는 새 코드 발급, 그 외 코드는 해당 사설 방
// (없으면 그 코드로 관대하게 새로 생성).
func (h *SUHub) lobbyRoomFor(roomField string) *suRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewSUGame(uuid.New().String())
			h.lobby = &suRoom{Game: game, Clients: map[int]*SUClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[SU] Created lobby game %s", game.ID)
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
		game := NewSUGame(uuid.New().String())
		room = &suRoom{Game: game, Clients: map[int]*SUClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[SU] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// ==================== 관전 / 리액션 ====================

// addSpectator 진행 중(또는 가득 찬) 사설 방에 관전자로 등록한다.
// 좌석·세션 없음 — 재접속 미지원, 끊기면 다시 코드로 들어온다.
func (h *SUHub) addSpectator(room *suRoom, client *SUClient, name string) {
	if len(room.Spectators) >= maxSpectators {
		h.sendError(client, spectatorFullMsg)
		return
	}
	if room.Spectators == nil {
		room.Spectators = map[*SUClient]bool{}
	}
	client.Name = name
	client.GameID = room.Game.ID
	room.Spectators[client] = true

	log.Printf("[스타트업스][관전] game=%s | %s 관전 입장 (%d명)",
		room.Game.ID, displayName(name), len(room.Spectators))

	h.sendToClient(client, SUMessage{
		Type:    SUMsgSpectateJoined,
		Payload: map[string]interface{}{"gameId": room.Game.ID, "roomCode": room.Code},
	})
	h.broadcastState(room)
}

// isSpectator 관전자 연결인지 (좌석 보유자·미입장 연결은 false)
func (h *SUHub) isSpectator(client *SUClient) bool {
	if client.GameID == "" {
		return false
	}
	room := h.rooms[client.GameID]
	return room != nil && room.Spectators[client]
}

// handleReact 리액션 이모지 — 좌석 보유자만, waiting 중에도 허용.
// 화이트리스트 외·레이트리밋 초과는 조용히 무시한다 (상태 저장 없음).
func (h *SUHub) handleReact(client *SUClient, msg SUMessage) {
	room := h.rooms[client.GameID]
	if room == nil || client.Seat < 0 {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SUReactPayload
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
	h.broadcastEvent(room, SUEventPayload{Kind: "react", Seat: &seat, Name: client.Name,
		Message: payload.Emoji})
}

// waitingRoomOf 클라이언트가 속한 시작 전 방 (공용 로비 또는 사설 방)
func (h *SUHub) waitingRoomOf(client *SUClient) *suRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Ready {
		return nil
	}
	return room
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *SUHub) hostSeat(room *suRoom) int {
	return hostSeatOf(room.Clients)
}

// suHumanCount 방의 사람 수
func suHumanCount(room *suRoom) int {
	return humanCountOf(room.Clients)
}

// updateLobbyWaiting 로비 현황판 갱신 — 사람 1명 이상 대기 && 미시작.
// 사설 방은 현황판에 노출하지 않는다 (초대 링크로만 접근).
func (h *SUHub) updateLobbyWaiting(room *suRoom) {
	if room.Code != "" {
		return
	}
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && suHumanCount(room) >= 1
	lobbySetWaiting("startups", waiting)
}

// handleFillBots host 가 빈 좌석을 연습봇으로 4인까지 채운 뒤 즉시 시작한다
// (스폰은 join 을 거치지 않으므로 명시 시작 호출).
func (h *SUHub) handleFillBots(client *SUClient) {
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
	for len(room.Game.Players) < SUFillBotTarget {
		botNo++
		if !h.spawnSUBot(room, fmt.Sprintf("%s%d", botName, botNo)) {
			break
		}
	}

	if room.Game.CanStart() {
		h.startGame(room)
		return
	}
	h.broadcastState(room)
}

func (h *SUHub) handleStart(client *SUClient) {
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
		h.sendError(client, fmt.Sprintf("%d명 이상 모여야 시작할 수 있습니다", SUMinPlayers))
		return
	}
	h.startGame(room)
}

func (h *SUHub) startGame(room *suRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	if room.Code != "" {
		// 시작한 사설 방의 코드는 진행 중 장부로 옮긴다 (관전 입장 근거)
		delete(h.privateLobbies, room.Code)
		h.activeCodes[room.Code] = room.Game.ID
	} else {
		h.lobby = nil
		lobbySetWaiting("startups", false)
	}

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	n := len(room.Game.Players)
	log.Printf("[스타트업스][경기시작] game=%s | 인원=%d | 덱=%d장(제외 %d장) | 시작 돈 %d원 | 선=seat%d | %v",
		room.Game.ID, n, len(room.Game.Deck), SURemovedCards, SUStartMoney,
		room.Game.CurrentSeat, names)
	if !suRoomHasBot(room) { // 연습봇전은 운영자 알림을 억제한다
		notify("스타트업스 시작", fmt.Sprintf("%d인전 시작", n))
	}

	h.afterProgress(room)
}

// removeFromLobby 대기실에서 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *SUHub) removeFromLobby(room *suRoom, client *SUClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	rebuilt := map[int]*SUClient{}
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

	log.Printf("[스타트업스][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), SUMaxPlayers)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if suHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, SUMessage{Type: SUMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			lobbySetWaiting("startups", false)
		}
		return
	}

	h.broadcastEvent(room, SUEventPayload{Kind: "left", Name: client.Name,
		Message: fmt.Sprintf("%s님이 퇴장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *SUHub) roomOf(client *SUClient) *suRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

// handleTake ① 카드 얻기 — "deck" 또는 "market:N"
func (h *SUHub) handleTake(client *SUClient, msg SUMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SUTakePayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.Take(client.Seat, payload.From); err != nil {
		h.sendError(client, err.Error())
		return
	}
	// 덱에서 뽑은 카드의 회사는 로그에도 남기지 않는다 (은닉 계약)
	log.Printf("[스타트업스][가져오기] game=%s | seat%d=%s from=%s (덱 %d장 · 덱 안티 %d원 · 시장 %d장)",
		room.Game.ID, client.Seat, displayName(client.Name), payload.From,
		len(room.Game.Deck), room.Game.DeckAnte, len(room.Game.Market))
	h.afterProgress(room)
}

// handlePlay ② 손패 1장을 시장에 내려놓기
func (h *SUHub) handlePlay(client *SUClient, msg SUMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SUPlayPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.Play(client.Seat, payload.Index); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[스타트업스][내려놓기] game=%s | seat%d=%s index=%d (시장 %d장 · %d차례)",
		room.Game.ID, client.Seat, displayName(client.Name), payload.Index,
		len(room.Game.Market), room.Game.Turns)
	h.afterProgress(room)
}

// afterProgress 모든 게임 변이 뒤의 공통 마무리 — 이벤트 방송·종료 판정·
// 새 대기 상태의 마감 예약·스냅샷 방송을 한 번에 처리한다.
func (h *SUHub) afterProgress(room *suRoom) {
	h.drainEvents(room)
	if room.Game.Phase == SUPhaseGameOver {
		h.finishGame(room)
		return
	}
	h.syncDeadline(room)
	h.broadcastState(room)
}

// drainEvents 순수 규칙이 쌓은 이벤트를 su_event 로 방송한다
func (h *SUHub) drainEvents(room *suRoom) {
	for _, ev := range room.Game.DrainEvents() {
		payload := SUEventPayload{Kind: ev.Kind, Message: ev.Message}
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
func (h *SUHub) syncDeadline(room *suRoom) {
	game := room.Game
	if room.DeadlineSeq == game.StateSeq {
		return
	}
	room.DeadlineSeq = game.StateSeq
	switch game.Phase {
	case SUPhaseTake, SUPhasePlay:
		h.scheduleDeadline(room, suTurnTimeout)
	default:
		h.stopPhaseTimer(room)
	}
}

// scheduleDeadline 마감 타이머 예약. AfkSeq 를 올려 지나간 발화를 구분하고,
// 발화는 허브 채널을 경유하므로 허브 밖에서 상태를 만지지 않는다.
func (h *SUHub) scheduleDeadline(room *suRoom, dur time.Duration) {
	h.stopPhaseTimer(room)
	room.Game.AfkSeq++
	room.Game.Deadline = time.Now().Add(dur).UnixMilli()
	sig := suPhaseSignal{GameID: room.Game.ID, Seq: room.Game.AfkSeq}
	room.PhaseTimer = time.AfterFunc(dur, func() {
		h.phaseFired <- sig
	})
}

func (h *SUHub) stopPhaseTimer(room *suRoom) {
	stopTimer(&room.PhaseTimer)
}

// handlePhaseFired 마감 발화 — 단계별 자동 행동으로 해소한다.
//   - take: 자동으로 시장 최상단(또는 덱)에서 가져오고 손패 무작위 1장을 낸다
//   - play: 손패 무작위 1장을 낸다
func (h *SUHub) handlePhaseFired(sig suPhaseSignal) {
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
	case SUPhaseTake:
		h.broadcastEvent(room, SUEventPayload{Kind: "afk", Seat: &seat, Name: actor.Name,
			Message: fmt.Sprintf("%s님이 오래 응답하지 않아 자동으로 진행합니다", actor.Name)})
		game.ForceTurn(h.rng)
		log.Printf("[스타트업스][자동진행] game=%s | seat%d 무응답 — 자동 차례 진행", game.ID, seat)

	case SUPhasePlay:
		h.broadcastEvent(room, SUEventPayload{Kind: "afk", Seat: &seat, Name: actor.Name,
			Message: fmt.Sprintf("%s님이 오래 응답하지 않아 자동으로 카드를 냅니다", actor.Name)})
		game.ForcePlay(h.rng)
		log.Printf("[스타트업스][자동진행] game=%s | seat%d 무응답 — 자동 내려놓기", game.ID, seat)

	default:
		return
	}
	h.afterProgress(room)
}

// ==================== 상태 뷰 (은닉의 핵심) ====================

// buildSUState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
//
// 은닉:
//   - yourHand 는 본인에게만 실린다 — 타인·관전자(viewerSeat -1)의 raw JSON
//     에는 키 자체가 없다 (nil 포인터 + omitempty). 빈 손패도 [] 로 보내야
//     하므로 슬라이스 포인터를 쓴다.
//   - 시작 때 게임에서 제외한 3장(game.Removed)은 이 스냅샷 어디에도 없다.
//     덱은 남은 장수(deckLeft)만 나간다.
//   - market·faceUp·companies 는 전원 공개다.
//
// viewerSeat -1(관전자)·좌석 없는 방에서도 패닉 없이 만들어져야 한다.
func (h *SUHub) buildSUState(room *suRoom, viewerSeat int) SUGameStatePayload {
	game := room.Game
	seated := viewerSeat >= 0 && viewerSeat < len(game.Players)

	var yourHand *[]SUCompany
	if seated && game.Ready {
		hand := append([]SUCompany{}, game.Players[viewerSeat].Hand...)
		yourHand = &hand
	}

	players := []SUPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		faceUp := make(map[SUCompany]int, len(suCompanyDefs))
		for _, def := range suCompanyDefs {
			faceUp[def.ID] = p.FaceUp[def.ID]
		}
		players = append(players, SUPlayerView{
			Seat:      p.Seat,
			Name:      p.Name,
			Connected: c != nil && c.Connected,
			Bot:       c != nil && c.Bot,
			Money:     p.Money,
			HandCount: len(p.Hand),
			FaceUp:    faceUp,
		})
	}

	endsAt := int64(0)
	switch game.Phase {
	case SUPhaseTake, SUPhasePlay:
		endsAt = game.Deadline
	}

	return SUGameStatePayload{
		GameID:      game.ID,
		RoomCode:    room.Code,
		Phase:       game.Phase,
		HostSeat:    h.hostSeat(room),
		YourSeat:    viewerSeat,
		Spectators:  len(room.Spectators),
		EndsAt:      endsAt,
		CurrentSeat: game.CurrentSeat,
		DeckLeft:    len(game.Deck),
		DeckAnte:    game.DeckAnte,
		Market:      append([]SUMarketCard{}, game.Market...),
		Companies:   game.CompanyBoard(),
		YourHand:    yourHand,
		Players:     players,
		LastAction:  game.LastAction,
		Result:      game.Result,
	}
}

// broadcastState 좌석마다 개인화 스냅샷을 만들어 보낸다.
// 관전자에게는 공개 정보만 담긴 스냅샷(viewerSeat -1)이 간다.
func (h *SUHub) broadcastState(room *suRoom) {
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, SUMessage{
			Type:    SUMsgGameState,
			Payload: h.buildSUState(room, seat),
		})
	}
	if len(room.Spectators) > 0 {
		msg := SUMessage{Type: SUMsgGameState, Payload: h.buildSUState(room, -1)}
		for c := range room.Spectators {
			h.sendToClient(c, msg)
		}
	}
}

func (h *SUHub) broadcastEvent(room *suRoom, event SUEventPayload) {
	h.broadcastToRoom(room, SUMessage{Type: SUMsgEvent, Payload: event})
}

// finishGame 게임 종료 발표·전적 기록·방 정리 (재대결은 같은 방 코드
// 재입장으로 — finishGame 이 코드를 반납해 같은 코드로 새 방을 만들 수 있다)
func (h *SUHub) finishGame(room *suRoom) {
	game := room.Game
	h.stopPhaseTimer(room)
	game.Deadline = 0

	result := game.Result
	if result == nil { // 방어선 — 종료는 항상 결과와 함께 온다
		result = &SUResult{Rows: []SUResultRow{}, WinnerSeats: []int{},
			WinnerNames: []string{}, Message: "게임이 종료됐습니다"}
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

	h.broadcastEvent(room, SUEventPayload{Kind: "game_over", Message: result.Message})
	// 최종 스냅샷을 먼저 보낸 뒤 종료를 알린다
	// (봇 러너는 su_game_over 를 보고 스스로 끝난다)
	h.broadcastState(room)
	h.broadcastToRoom(room, SUMessage{
		Type: SUMsgGameOver,
		Payload: SUGameOverPayload{
			Rows:        append([]SUResultRow{}, result.Rows...),
			WinnerSeats: append([]int{}, result.WinnerSeats...),
			WinnerNames: append([]string{}, result.WinnerNames...),
			Message:     result.Message,
			Turns:       game.Turns,
			Companies:   game.CompanyBoard(),
			Players:     h.buildSUState(room, -1).Players,
		},
	})

	moneys := []string{}
	for _, p := range game.Players {
		moneys = append(moneys, fmt.Sprintf("%s %d원", displayName(p.Name), p.Money))
	}
	log.Printf("[스타트업스][경기결과] game=%s | 승자=%s | 차례=%d | 소요=%s | %s",
		game.ID, strings.Join(winners, "·"), game.Turns,
		matchDuration(game.StartedAt), strings.Join(moneys, " / "))

	RecordMatch(MatchRecord{
		Game:     "startups",
		Players:  strings.Join(winners, "·") + " vs " + strings.Join(losers, "·"),
		Winner:   strings.Join(winners, "·"),
		Reason:   "money",
		Duration: matchSeconds(game.StartedAt),
		Bot:      suRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
	if room.Code != "" {
		delete(h.activeCodes, room.Code) // 코드 재사용 복귀
	}
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *SUHub) handleDisconnect(client *SUClient) {
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
	log.Printf("[스타트업스][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, SUMessage{
		Type: SUMsgPlayerDisconnected,
		Payload: SUPlayerDisconnectedPayload{
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
func (h *SUHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == SUPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[스타트업스][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := h.takeoverSUBot(room, seat, client.Name)
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.broadcastEvent(room, SUEventPayload{Kind: "bot_takeover", Seat: &seat,
		Name:    client.Name,
		Message: fmt.Sprintf("%s님 자리를 봇이 이어받았습니다", client.Name)})
	// 새 봇이 이 스냅샷을 받아 자기 차례면 즉시 이어간다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *SUHub) handleRejoin(client *SUClient, msg SUMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SURejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, SUMessage{Type: SUMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, SUMessage{Type: SUMsgSessionExpired})
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

	log.Printf("[스타트업스][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, SUMessage{
		Type:    SUMsgPlayerReconnected,
		Payload: SUPlayerReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *SUHub) clearGameSessions(room *suRoom) {
	clearRoomSessions(&h.sessionManager, room.Clients)
}

// ==================== 전송 ====================

func (h *SUHub) sendError(client *SUClient, message string) {
	h.sendToClient(client, SUMessage{Type: SUMsgError, Payload: SUErrorPayload{Message: message}})
}

func (h *SUHub) sendToClient(client *SUClient, message SUMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[SU] ")
}

func (h *SUHub) broadcastToRoom(room *suRoom, message SUMessage) {
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

func ServeSUWs(hub *SUHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[SU] Error upgrading connection:", err)
		return
	}

	client := &SUClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[SU] ",
		func(msg SUMessage) { hub.gameMessage <- SUGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
