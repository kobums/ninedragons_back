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

// ==================== 뱅! 허브 ====================
//
// 다인 결(ct_hub / kr_hub)을 그대로 복제한다 — 공용 로비 + 사설 방 코드 +
// 관전 + 리액션 + 재접속 유예 + 봇 대체. 뱅!만의 차이는 대기 상태가 넷이라는
// 점이다 (차례 60초 · 대응 20초 · 잡화점 15초 · 손패 줄이기 15초). 각 단계의
// 마감은 bg_game.go 의 Force* 로 해소된다.

// 뱅! 대기 상태 마감 타이머 (테스트에서 짧게 낮춘다)
var (
	bgTurnTimeout    = 60 * time.Second
	bgRespondTimeout = 20 * time.Second
	bgStoreTimeout   = 15 * time.Second
	bgDiscardTimeout = 15 * time.Second
)

// bgRoom 게임(순수 상태)과 좌석별 연결의 매핑
type bgRoom struct {
	Game       *BGGame
	Clients    map[int]*BGClient // seat → client
	PhaseTimer *time.Timer       // 대기 상태 마감 타이머

	// DeadlineSeq 마지막으로 마감을 건 StateSeq — 같은 대기 상태에 스냅샷이
	// 쌓일 때마다(관전 입장·접속 변화 등) 마감이 늘어나지 않게 하는 근거
	DeadlineSeq int

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string

	// Spectators 관전자 연결 (사설 방 코드로만 진입, 상한 maxSpectators).
	// 좌석·세션이 없어 재접속을 지원하지 않는다 — 끊기면 목록에서만 뗀다.
	Spectators map[*BGClient]bool

	// LastReact 좌석별 마지막 리액션 발신 시각 (레이트리밋 장부)
	LastReact map[int]time.Time
}

// bgPhaseSignal 마감 타이머의 발화 표식 — 대기 상태 일련번호(AfkSeq)로
// 지나간 발화를 구분한다
type bgPhaseSignal struct {
	GameID string
	Seq    int
}

type BGHub struct {
	clients map[*BGClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*bgRoom

	// 글로벌 로비. 시작 전 공용 방은 하나뿐이다.
	lobby *bgRoom

	// 사설 방 (초대 코드 → 시작 전 방). 허브 고루틴에서만 접근한다.
	privateLobbies map[string]*bgRoom

	// 진행 중 사설 방 (초대 코드 → gameID). 시작 후에도 코드로 방을 찾아
	// 관전 입장시키는 근거다. finishGame 에서 해제한다 (코드 재사용 복귀).
	activeCodes map[string]string

	register    chan *BGClient
	unregister  chan *BGClient
	gameMessage chan BGGameMessage
	phaseFired  chan bgPhaseSignal

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*BGClient]

	// 덱 셔플·자동 진행용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type BGGameMessage struct {
	Client  *BGClient
	Message BGMessage
}

func NewBGHub() *BGHub {
	return &BGHub{
		register:       make(chan *BGClient),
		unregister:     make(chan *BGClient),
		clients:        make(map[*BGClient]bool),
		rooms:          make(map[string]*bgRoom),
		privateLobbies: make(map[string]*bgRoom),
		activeCodes:    make(map[string]string),
		gameMessage:    make(chan BGGameMessage),
		phaseFired:     make(chan bgPhaseSignal, 8),
		sessionManager: newSessionManager[*BGClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *BGHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[BG] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[BG] Client unregistered: %s", client.ID)
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

func (h *BGHub) handleGameMessage(gm BGGameMessage) {
	// 관전자는 어떤 행동도 할 수 없다 (보기 전용 — 리액션도 좌석 보유자만)
	if h.isSpectator(gm.Client) {
		h.sendError(gm.Client, spectatorDeniedMsg)
		return
	}
	switch gm.Message.Type {
	case BGMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case BGMsgReact:
		h.handleReact(gm.Client, gm.Message)
	case BGMsgFillBots:
		h.handleFillBots(gm.Client)
	case BGMsgStart:
		h.handleStart(gm.Client)
	case BGMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case BGMsgPlay:
		h.handlePlay(gm.Client, gm.Message)
	case BGMsgRespond:
		h.handleRespond(gm.Client, gm.Message)
	case BGMsgPick:
		h.handlePick(gm.Client, gm.Message)
	case BGMsgDiscard:
		h.handleDiscard(gm.Client, gm.Message)
	case BGMsgEndTurn:
		h.handleEndTurn(gm.Client)
	}
}

// ==================== 대기실 ====================

func (h *BGHub) handleJoinGame(client *BGClient, msg BGMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload BGJoinGamePayload
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

	log.Printf("[뱅][입장] game=%s | seat%d=%s 대기 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), BGMaxPlayers)
	if room.Code == "" { // 사설 방 입장은 조용히 (공용 로비만 알림)
		notify("뱅! 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), len(room.Game.Players), BGMaxPlayers))
	}

	h.sendToClient(client, BGMessage{
		Type: BGMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
			"roomCode":  room.Code,
		},
	})

	h.broadcastEvent(room, BGEventPayload{Kind: "joined", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 입장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	// 정원이 차도 자동 시작하지 않는다 — 호스트 명시 시작만
	h.broadcastState(room)
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다.
// 생략/빈 문자열은 공용 로비, "NEW"는 새 코드 발급, 그 외 코드는 해당 사설 방
// (없으면 그 코드로 관대하게 새로 생성).
func (h *BGHub) lobbyRoomFor(roomField string) *bgRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewBGGame(uuid.New().String())
			h.lobby = &bgRoom{Game: game, Clients: map[int]*BGClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[BG] Created lobby game %s", game.ID)
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
		game := NewBGGame(uuid.New().String())
		room = &bgRoom{Game: game, Clients: map[int]*BGClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[BG] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// ==================== 관전 / 리액션 ====================

// addSpectator 진행 중(또는 가득 찬) 사설 방에 관전자로 등록한다.
func (h *BGHub) addSpectator(room *bgRoom, client *BGClient, name string) {
	if room == nil {
		h.sendError(client, "방을 찾을 수 없습니다")
		return
	}
	if len(room.Spectators) >= maxSpectators {
		h.sendError(client, spectatorFullMsg)
		return
	}
	if room.Spectators == nil {
		room.Spectators = map[*BGClient]bool{}
	}
	client.Name = name
	client.GameID = room.Game.ID
	room.Spectators[client] = true

	log.Printf("[뱅][관전] game=%s | %s 관전 입장 (%d명)",
		room.Game.ID, displayName(name), len(room.Spectators))

	h.sendToClient(client, BGMessage{
		Type:    BGMsgSpectateJoined,
		Payload: map[string]interface{}{"gameId": room.Game.ID, "roomCode": room.Code},
	})
	h.broadcastState(room)
}

// isSpectator 관전자 연결인지 (좌석 보유자·미입장 연결은 false)
func (h *BGHub) isSpectator(client *BGClient) bool {
	if client.GameID == "" {
		return false
	}
	room := h.rooms[client.GameID]
	return room != nil && room.Spectators[client]
}

// handleReact 리액션 이모지 — 좌석 보유자만, waiting 중에도 허용.
func (h *BGHub) handleReact(client *BGClient, msg BGMessage) {
	room := h.rooms[client.GameID]
	if room == nil || client.Seat < 0 {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload BGReactPayload
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
	h.broadcastEvent(room, BGEventPayload{Kind: "react", Seat: &seat, Name: client.Name,
		Message: payload.Emoji})
}

// waitingRoomOf 클라이언트가 속한 시작 전 방 (공용 로비 또는 사설 방)
func (h *BGHub) waitingRoomOf(client *BGClient) *bgRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Ready {
		return nil
	}
	return room
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *BGHub) hostSeat(room *bgRoom) int {
	return hostSeatOf(room.Clients)
}

// bgHumanCount 방의 사람 수
func bgHumanCount(room *bgRoom) int {
	return humanCountOf(room.Clients)
}

// updateLobbyWaiting 로비 현황판 갱신 — 사람 1명 이상 대기 && 미시작.
func (h *BGHub) updateLobbyWaiting(room *bgRoom) {
	if room.Code != "" {
		return
	}
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && bgHumanCount(room) >= 1
	lobbySetWaiting("bang", waiting)
}

// handleFillBots host 가 빈 좌석을 연습봇으로 5인까지 채운 뒤 즉시 시작한다
func (h *BGHub) handleFillBots(client *BGClient) {
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
	for len(room.Game.Players) < BGFillBotTarget {
		botNo++
		if !h.spawnBGBot(room, fmt.Sprintf("%s%d", botName, botNo)) {
			break
		}
	}

	if room.Game.CanStart() {
		h.startGame(room)
		return
	}
	h.broadcastState(room)
}

func (h *BGHub) handleStart(client *BGClient) {
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
		h.sendError(client, fmt.Sprintf("%d명 이상 모여야 시작할 수 있습니다", BGMinPlayers))
		return
	}
	h.startGame(room)
}

func (h *BGHub) startGame(room *bgRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	if room.Code != "" {
		// 시작한 사설 방의 코드는 진행 중 장부로 옮긴다 (관전 입장 근거)
		delete(h.privateLobbies, room.Code)
		h.activeCodes[room.Code] = room.Game.ID
	} else {
		h.lobby = nil
		lobbySetWaiting("bang", false)
	}

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	n := len(room.Game.Players)
	log.Printf("[뱅][경기시작] game=%s | 인원=%d | 보안관=seat%d | 덱 %d장 | %v",
		room.Game.ID, n, room.Game.CurrentSeat, len(room.Game.Deck), names)
	if !bgRoomHasBot(room) { // 연습봇전은 운영자 알림을 억제한다
		notify("뱅! 시작", fmt.Sprintf("%d인전 시작", n))
	}

	h.afterProgress(room)
}

// removeFromLobby 대기실에서 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *BGHub) removeFromLobby(room *bgRoom, client *BGClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	rebuilt := map[int]*BGClient{}
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

	log.Printf("[뱅][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), BGMaxPlayers)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if bgHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, BGMessage{Type: BGMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			lobbySetWaiting("bang", false)
		}
		return
	}

	h.broadcastEvent(room, BGEventPayload{Kind: "left", Name: client.Name,
		Message: fmt.Sprintf("%s님이 퇴장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *BGHub) roomOf(client *BGClient) *bgRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

// handlePlay 카드 사용
func (h *BGHub) handlePlay(client *BGClient, msg BGMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload BGPlayPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.Play(client.Seat, payload.Index,
		payload.TargetSeat, payload.TargetCardIndex); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.afterProgress(room)
}

// handleRespond 대응 창 (빗나감!/뱅! — index 생략은 포기)
func (h *BGHub) handleRespond(client *BGClient, msg BGMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload BGRespondPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.Respond(client.Seat, payload.Index); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.afterProgress(room)
}

// handlePick 잡화점 고르기
func (h *BGHub) handlePick(client *BGClient, msg BGMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload BGPickPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.Pick(client.Seat, payload.Index); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.afterProgress(room)
}

// handleDiscard 차례 끝 손패 줄이기
func (h *BGHub) handleDiscard(client *BGClient, msg BGMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload BGDiscardPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.DiscardCards(client.Seat, payload.Indexes); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.afterProgress(room)
}

// handleEndTurn 차례 마무리
func (h *BGHub) handleEndTurn(client *BGClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	if err := room.Game.EndTurn(client.Seat); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.afterProgress(room)
}

// afterProgress 모든 게임 변이 뒤의 공통 마무리 — 이벤트 방송·종료 판정·
// 새 대기 상태의 마감 예약·스냅샷 방송을 한 번에 처리한다.
func (h *BGHub) afterProgress(room *bgRoom) {
	h.drainEvents(room)
	if room.Game.Phase == BGPhaseGameOver {
		h.finishGame(room)
		return
	}
	h.syncDeadline(room)
	h.broadcastState(room)
}

// drainEvents 순수 규칙이 쌓은 이벤트를 bg_event 로 방송한다
func (h *BGHub) drainEvents(room *bgRoom) {
	for _, ev := range room.Game.DrainEvents() {
		payload := BGEventPayload{Kind: ev.Kind, Message: ev.Message}
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
func (h *BGHub) syncDeadline(room *bgRoom) {
	game := room.Game
	if room.DeadlineSeq == game.StateSeq {
		return
	}
	room.DeadlineSeq = game.StateSeq
	var dur time.Duration
	switch game.Phase {
	case BGPhaseTurn:
		dur = bgTurnTimeout
	case BGPhaseRespond:
		dur = bgRespondTimeout
	case BGPhaseStorePick:
		dur = bgStoreTimeout
	case BGPhaseDiscard:
		dur = bgDiscardTimeout
	default:
		h.stopPhaseTimer(room)
		return
	}
	h.scheduleDeadline(room, dur)
}

// scheduleDeadline 마감 타이머 예약. AfkSeq 를 올려 지나간 발화를 구분하고,
// 발화는 허브 채널을 경유하므로 허브 밖에서 상태를 만지지 않는다.
func (h *BGHub) scheduleDeadline(room *bgRoom, dur time.Duration) {
	h.stopPhaseTimer(room)
	room.Game.AfkSeq++
	room.Game.Deadline = time.Now().Add(dur).UnixMilli()
	sig := bgPhaseSignal{GameID: room.Game.ID, Seq: room.Game.AfkSeq}
	room.PhaseTimer = time.AfterFunc(dur, func() {
		h.phaseFired <- sig
	})
}

func (h *BGHub) stopPhaseTimer(room *bgRoom) {
	stopTimer(&room.PhaseTimer)
}

// handlePhaseFired 마감 발화 — 단계별 자동 행동으로 해소한다.
//
//	turn:       차례를 끝낸다 (손패가 넘치면 줄이기 단계로)
//	respond:    포기 (체력 −1)
//	store_pick: 첫 장을 집는다
//	discard:    손패 앞에서부터 버린다
func (h *BGHub) handlePhaseFired(sig bgPhaseSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.Game.AfkSeq != sig.Seq {
		return
	}
	game := room.Game

	// 대기 좌석은 단계마다 다르다 — 대응·잡화점은 pending 이 가리킨다
	seat := game.CurrentSeat
	switch game.Phase {
	case BGPhaseRespond, BGPhaseStorePick:
		if game.Pending == nil {
			return
		}
		seat = game.Pending.TargetSeat
	}
	if seat < 0 || seat >= len(game.Players) {
		return
	}
	actor := game.Players[seat]

	switch game.Phase {
	case BGPhaseTurn:
		h.broadcastEvent(room, BGEventPayload{Kind: "afk", Seat: &seat, Name: actor.Name,
			Message: fmt.Sprintf("%s님이 오래 응답하지 않아 차례를 넘깁니다", actor.Name)})
		game.ForceTurn()
		log.Printf("[뱅][자동진행] game=%s | seat%d 무응답 — 자동 차례 종료", game.ID, seat)

	case BGPhaseRespond:
		h.broadcastEvent(room, BGEventPayload{Kind: "afk", Seat: &seat, Name: actor.Name,
			Message: fmt.Sprintf("%s님이 오래 응답하지 않아 대응을 포기합니다", actor.Name)})
		game.ForceRespond()
		log.Printf("[뱅][자동진행] game=%s | seat%d 무응답 — 대응 포기", game.ID, seat)

	case BGPhaseStorePick:
		h.broadcastEvent(room, BGEventPayload{Kind: "afk", Seat: &seat, Name: actor.Name,
			Message: fmt.Sprintf("%s님이 오래 응답하지 않아 잡화점 첫 장을 집습니다", actor.Name)})
		game.ForcePick()
		log.Printf("[뱅][자동진행] game=%s | seat%d 무응답 — 잡화점 자동 선택", game.ID, seat)

	case BGPhaseDiscard:
		h.broadcastEvent(room, BGEventPayload{Kind: "afk", Seat: &seat, Name: actor.Name,
			Message: fmt.Sprintf("%s님이 오래 응답하지 않아 손패를 앞에서부터 버립니다", actor.Name)})
		game.ForceDiscard()
		log.Printf("[뱅][자동진행] game=%s | seat%d 무응답 — 손패 자동 정리", game.ID, seat)

	default:
		return
	}
	h.afterProgress(room)
}

// ==================== 상태 뷰 (은닉의 핵심) ====================

// bgRoleVisible 그 좌석의 역할을 뷰어에게 보여도 되는가.
//
//	보안관   시작부터 전원 공개
//	그 외    사망 시 공개 (종료 화면에서는 전원 공개)
func bgRoleVisible(game *BGGame, p *BGPlayer) bool {
	if !game.Ready {
		return false
	}
	return p.Role == BGRoleSheriff || !p.Alive || game.Phase == BGPhaseGameOver
}

// buildBGState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
//
// 은닉 계약:
//   - yourRole·yourHand 는 본인에게만 (타인·관전자 raw JSON 에 키 부재)
//   - players[].role 은 보안관만 시작부터 공개, 나머지는 사망 시 공개
//   - distanceFromYou 는 뷰어별로 계산된다 (관전자·탈락자는 -1)
//
// 빈 슬라이스도 [] 로 나가야 하므로 슬라이스 포인터로 부재를 표현한다.
// viewerSeat -1(관전자)과 좌석이 없는 방에서도 패닉하지 않는다.
func (h *BGHub) buildBGState(room *bgRoom, viewerSeat int) BGGameStatePayload {
	game := room.Game
	seated := viewerSeat >= 0 && viewerSeat < len(game.Players)

	var yourRole *BGRole
	var yourHand *[]BGCard
	var yourBangUsed *bool

	if seated && game.Ready {
		me := game.Players[viewerSeat]
		role := me.Role
		yourRole = &role
		hand := append([]BGCard{}, me.Hand...)
		yourHand = &hand
		blocked := game.BangBlocked(viewerSeat)
		yourBangUsed = &blocked
	}

	players := []BGPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		role := BGRoleNone
		if bgRoleVisible(game, p) {
			role = p.Role
		}
		dist := -1
		switch {
		case !game.Ready:
			dist = -1
		case seated && p.Seat == viewerSeat:
			dist = 0
		case seated:
			dist = game.DistanceBetween(viewerSeat, p.Seat)
		}
		players = append(players, BGPlayerView{
			Seat:            p.Seat,
			Name:            p.Name,
			Connected:       c != nil && c.Connected,
			Bot:             c != nil && c.Bot,
			Alive:           p.Alive,
			HP:              p.HP,
			MaxHP:           p.MaxHP,
			HandCount:       len(p.Hand),
			Equipment:       append([]BGCard{}, p.Equipment...),
			Role:            role,
			DistanceFromYou: dist,
		})
	}

	endsAt := int64(0)
	switch game.Phase {
	case BGPhaseTurn, BGPhaseRespond, BGPhaseStorePick, BGPhaseDiscard:
		endsAt = game.Deadline
	}

	var pending *BGPending
	if game.Pending != nil {
		p := *game.Pending
		p.Passed = append([]int{}, game.Pending.Passed...)
		p.Queue = nil // 와이어 비노출 (json:"-" 이지만 값도 남기지 않는다)
		pending = &p
	}

	return BGGameStatePayload{
		GameID:       game.ID,
		RoomCode:     room.Code,
		Phase:        game.Phase,
		HostSeat:     h.hostSeat(room),
		YourSeat:     viewerSeat,
		Spectators:   len(room.Spectators),
		EndsAt:       endsAt,
		CurrentSeat:  game.CurrentSeat,
		DeckLeft:     len(game.Deck),
		DiscardTop:   game.discardTop(),
		Pending:      pending,
		StoreCards:   append([]BGCard{}, game.StoreCards...),
		YourRole:     yourRole,
		YourHand:     yourHand,
		YourBangUsed: yourBangUsed,
		Players:      players,
		LastAction:   game.LastAction,
		Result:       game.Result,
	}
}

// broadcastState 좌석마다 개인화 스냅샷을 만들어 보낸다.
// 관전자에게는 공개 정보만 담긴 스냅샷(viewerSeat -1)이 간다.
func (h *BGHub) broadcastState(room *bgRoom) {
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, BGMessage{
			Type:    BGMsgGameState,
			Payload: h.buildBGState(room, seat),
		})
	}
	if len(room.Spectators) > 0 {
		msg := BGMessage{Type: BGMsgGameState, Payload: h.buildBGState(room, -1)}
		for c := range room.Spectators {
			h.sendToClient(c, msg)
		}
	}
}

func (h *BGHub) broadcastEvent(room *bgRoom, event BGEventPayload) {
	h.broadcastToRoom(room, BGMessage{Type: BGMsgEvent, Payload: event})
}

// finishGame 게임 종료 발표·전적 기록·방 정리 (재대결은 같은 방 코드
// 재입장으로 — finishGame 이 코드를 반납해 같은 코드로 새 방을 만들 수 있다)
func (h *BGHub) finishGame(room *bgRoom) {
	game := room.Game
	h.stopPhaseTimer(room)
	game.Deadline = 0

	result := game.Result
	if result == nil { // 방어선 — 종료는 항상 결과와 함께 온다
		result = &BGResult{Winner: "sheriff", WinnerSeats: []int{},
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

	h.broadcastEvent(room, BGEventPayload{Kind: "game_over", Message: result.Message})
	// 최종 스냅샷을 먼저 보낸 뒤 종료를 알린다
	// (봇 러너는 bg_game_over 를 보고 스스로 끝난다)
	h.broadcastState(room)
	h.broadcastToRoom(room, BGMessage{
		Type: BGMsgGameOver,
		Payload: BGGameOverPayload{
			Winner:      result.Winner,
			WinnerSeats: append([]int{}, result.WinnerSeats...),
			WinnerNames: append([]string{}, result.WinnerNames...),
			Message:     result.Message,
			Turns:       game.Turns,
			Players:     h.buildBGState(room, -1).Players,
		},
	})

	rows := []string{}
	for _, p := range game.Players {
		state := "생존"
		if !p.Alive {
			state = "탈락"
		}
		rows = append(rows, fmt.Sprintf("%s %s(%s, 체력 %d)",
			displayName(p.Name), bgRoleLabel(p.Role), state, p.HP))
	}
	log.Printf("[뱅][경기결과] game=%s | 승리진영=%s | 승자=%s | 차례=%d | 소요=%s | %s",
		game.ID, result.Winner, strings.Join(winners, "·"), game.Turns,
		matchDuration(game.StartedAt), strings.Join(rows, " / "))

	RecordMatch(MatchRecord{
		Game:     "bang",
		Players:  strings.Join(winners, "·") + " vs " + strings.Join(losers, "·"),
		Winner:   strings.Join(winners, "·"),
		Reason:   result.Winner,
		Duration: matchSeconds(game.StartedAt),
		Bot:      bgRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
	if room.Code != "" {
		delete(h.activeCodes, room.Code) // 코드 재사용 복귀
	}
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *BGHub) handleDisconnect(client *BGClient) {
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
	log.Printf("[뱅][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, BGMessage{
		Type: BGMsgPlayerDisconnected,
		Payload: BGPlayerDisconnectedPayload{
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
func (h *BGHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == BGPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[뱅][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := h.takeoverBGBot(room, seat, client.Name)
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.broadcastEvent(room, BGEventPayload{Kind: "bot_takeover", Seat: &seat,
		Name:    client.Name,
		Message: fmt.Sprintf("%s님 자리를 봇이 이어받았습니다", client.Name)})
	// 새 봇이 이 스냅샷을 받아 자기 차례면 즉시 이어간다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *BGHub) handleRejoin(client *BGClient, msg BGMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload BGRejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, BGMessage{Type: BGMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, BGMessage{Type: BGMsgSessionExpired})
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

	log.Printf("[뱅][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, BGMessage{
		Type:    BGMsgPlayerReconnected,
		Payload: BGPlayerReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *BGHub) clearGameSessions(room *bgRoom) {
	clearRoomSessions(&h.sessionManager, room.Clients)
}

// ==================== 전송 ====================

func (h *BGHub) sendError(client *BGClient, message string) {
	h.sendToClient(client, BGMessage{Type: BGMsgError, Payload: BGErrorPayload{Message: message}})
}

func (h *BGHub) sendToClient(client *BGClient, message BGMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[BG] ")
}

func (h *BGHub) broadcastToRoom(room *bgRoom, message BGMessage) {
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

func ServeBGWs(hub *BGHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[BG] Error upgrading connection:", err)
		return
	}

	client := &BGClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[BG] ",
		func(msg BGMessage) { hub.gameMessage <- BGGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
