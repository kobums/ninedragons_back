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

// ==================== 리코셰 로봇 허브 ====================
//
// 다인 결(kr_hub)을 그대로 따르되 **턴 상태기계는 들어냈다** —
// 세트(se_hub)·더 마인드(mi_hub)와 같은 선착 판정 모델이다. currentSeat 도,
// 좌석별 AFK 자동 진행도 없다. 모든 rr_bid 는 h.gameMessage 채널로 모이고
// 허브 고루틴이 도착한 순서대로 하나씩 처리하므로, 같은 횟수를 동시에 외쳐도
// 먼저 도착한 쪽이 증명권을 가진다. 판정이 한 고루틴에 직렬화되므로 게임
// 상태에는 락이 필요 없다.
//
// 타이머는 둘이다.
//   - PhaseTimer 단계 마감. thinking(목표 상한) → bidding(카운트다운 60초)
//     → demo(증명 제한) → goal_end(정산 표시) 를 (phase, goalIndex, demoTurn)
//     키가 바뀔 때만 다시 건다. **외침이 추가돼도 카운트다운을 다시 걸지
//     않는다** — 규칙상 60초는 한 번만 흐르기 때문이다.
//   - EndTimer 게임 전체 캡 (무한 게임 방지)

// rrRoom 게임(순수 상태)과 좌석별 연결의 매핑
type rrRoom struct {
	Game    *RRGame
	Clients map[int]*RRClient // seat → client

	PhaseTimer *time.Timer // 단계 마감
	EndTimer   *time.Timer // 게임 전체 캡

	// PhaseKey 마지막으로 마감을 건 (phase, goalIndex, demoTurn).
	// 같은 단계에서 스냅샷이 여러 번 쌓여도 마감이 늘어나지 않게 하는 근거다.
	PhaseKey string

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string

	// Spectators 관전자 연결 (사설 방 코드로만 진입, 상한 maxSpectators).
	// 좌석·세션이 없어 재접속을 지원하지 않는다 — 끊기면 목록에서만 뗀다.
	Spectators map[*RRClient]bool

	// LastReact 좌석별 마지막 리액션 발신 시각 (레이트리밋 장부)
	LastReact map[int]time.Time
}

// rrTimerSignal 타이머 발화 표식 — 일련번호로 지나간 발화를 구분한다
type rrTimerSignal struct {
	GameID string
	Kind   string // "phase" | "end"
	Seq    int
}

type RRHub struct {
	clients map[*RRClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*rrRoom

	// 글로벌 로비. 시작 전 공용 방은 하나뿐이다.
	lobby *rrRoom

	// 사설 방 (초대 코드 → 시작 전 방). 허브 고루틴에서만 접근한다.
	privateLobbies map[string]*rrRoom

	// 진행 중 사설 방 (초대 코드 → gameID). 관전 입장의 근거이며
	// finishGame 에서 해제한다 (코드 재사용 복귀).
	activeCodes map[string]string

	register   chan *RRClient
	unregister chan *RRClient

	// 게임 메시지 — 선착 판정의 직렬화 지점이다
	gameMessage chan RRGameMessage

	// 타이머 발화 (time.AfterFunc → 허브 채널 경유)
	timerFired chan rrTimerSignal

	// 시간 설정 (테스트가 Run 전에 낮춘다 — 고루틴과 경합 금지)
	bidWindow    time.Duration
	goalCap      time.Duration
	demoCap      time.Duration
	goalEndDelay time.Duration
	gameCap      time.Duration

	// 세션·유예 타이머 장부
	sessionManager[*RRClient]

	// 판 생성·목표 추첨용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type RRGameMessage struct {
	Client  *RRClient
	Message RRMessage
}

func NewRRHub() *RRHub {
	return &RRHub{
		register:       make(chan *RRClient),
		unregister:     make(chan *RRClient),
		clients:        make(map[*RRClient]bool),
		rooms:          make(map[string]*rrRoom),
		privateLobbies: make(map[string]*rrRoom),
		activeCodes:    make(map[string]string),
		gameMessage:    make(chan RRGameMessage),
		timerFired:     make(chan rrTimerSignal, 16),
		bidWindow:      rrBidWindow,
		goalCap:        rrGoalCap,
		demoCap:        rrDemoCap,
		goalEndDelay:   rrGoalEndDelay,
		gameCap:        rrGameCap,
		sessionManager: newSessionManager[*RRClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *RRHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[RR] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[RR] Client unregistered: %s", client.ID)
			}

		case sessionID := <-h.graceExpired:
			h.handleGraceExpired(sessionID)

		case sig := <-h.timerFired:
			h.handleTimerFired(sig)

		case message := <-h.gameMessage:
			h.handleGameMessage(message)
		}
	}
}

func (h *RRHub) handleGameMessage(gm RRGameMessage) {
	// 관전자는 어떤 행동도 할 수 없다 (보기 전용 — 리액션도 좌석 보유자만)
	if h.isSpectator(gm.Client) {
		h.sendError(gm.Client, spectatorDeniedMsg)
		return
	}
	switch gm.Message.Type {
	case RRMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case RRMsgReact:
		h.handleReact(gm.Client, gm.Message)
	case RRMsgFillBots:
		h.handleFillBots(gm.Client)
	case RRMsgStart:
		h.handleStart(gm.Client)
	case RRMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case RRMsgBid:
		h.handleBid(gm.Client, gm.Message)
	case RRMsgDemo:
		h.handleDemo(gm.Client, gm.Message)
	case RRMsgPass:
		h.handlePass(gm.Client)
	}
}

// ==================== 대기실 ====================

func (h *RRHub) handleJoinGame(client *RRClient, msg RRMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload RRJoinGamePayload
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

	log.Printf("[리코셰][입장] game=%s | seat%d=%s 대기 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), RRMaxPlayers)
	if room.Code == "" { // 사설 방 입장은 조용히 (공용 로비만 알림)
		notify("리코셰 로봇 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), len(room.Game.Players), RRMaxPlayers))
	}

	h.sendToClient(client, RRMessage{
		Type: RRMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
			"roomCode":  room.Code,
		},
	})

	h.broadcastEvent(room, RREventPayload{Kind: "joined", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 입장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	// 정원이 차도 자동 시작하지 않는다 — 호스트 명시 시작만 (봇 채우기와 충돌 방지)
	h.broadcastState(room)
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다.
// 생략/빈 문자열은 공용 로비, "NEW"는 새 코드 발급, 그 외 코드는 해당 사설 방
// (없으면 그 코드로 관대하게 새로 생성).
func (h *RRHub) lobbyRoomFor(roomField string) *rrRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewRRGame(uuid.New().String())
			h.lobby = &rrRoom{Game: game, Clients: map[int]*RRClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[RR] Created lobby game %s", game.ID)
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
		game := NewRRGame(uuid.New().String())
		room = &rrRoom{Game: game, Clients: map[int]*RRClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[RR] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// ==================== 관전 / 리액션 ====================

// addSpectator 진행 중(또는 가득 찬) 사설 방에 관전자로 등록한다.
// 좌석·세션 없음 — 재접속 미지원, 끊기면 다시 코드로 들어온다.
// 은닉이 없는 게임이라 관전자는 참가자와 완전히 같은 스냅샷을 본다.
func (h *RRHub) addSpectator(room *rrRoom, client *RRClient, name string) {
	if len(room.Spectators) >= maxSpectators {
		h.sendError(client, spectatorFullMsg)
		return
	}
	if room.Spectators == nil {
		room.Spectators = map[*RRClient]bool{}
	}
	client.Name = name
	client.GameID = room.Game.ID
	room.Spectators[client] = true

	log.Printf("[리코셰][관전] game=%s | %s 관전 입장 (%d명)",
		room.Game.ID, displayName(name), len(room.Spectators))

	h.sendToClient(client, RRMessage{
		Type:    RRMsgSpectateJoined,
		Payload: map[string]interface{}{"gameId": room.Game.ID, "roomCode": room.Code},
	})
	// 전원(관전자 포함)에게 관전자 수가 반영된 스냅샷 — 신규 관전자의 첫 스냅샷 겸용
	h.broadcastState(room)
}

// isSpectator 관전자 연결인지 (좌석 보유자·미입장 연결은 false)
func (h *RRHub) isSpectator(client *RRClient) bool {
	if client.GameID == "" {
		return false
	}
	room := h.rooms[client.GameID]
	return room != nil && room.Spectators[client]
}

// handleReact 리액션 이모지 — 좌석 보유자만, waiting 중에도 허용.
// 화이트리스트 외·레이트리밋 초과는 조용히 무시한다 (상태 저장 없음).
func (h *RRHub) handleReact(client *RRClient, msg RRMessage) {
	room := h.rooms[client.GameID]
	if room == nil || client.Seat < 0 {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload RRReactPayload
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
	h.broadcastEvent(room, RREventPayload{Kind: "react", Seat: &seat, Name: client.Name,
		Message: payload.Emoji})
}

// waitingRoomOf 클라이언트가 속한 시작 전 방 (공용 로비 또는 사설 방)
func (h *RRHub) waitingRoomOf(client *RRClient) *rrRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Ready {
		return nil
	}
	return room
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *RRHub) hostSeat(room *rrRoom) int {
	return hostSeatOf(room.Clients)
}

// rrHumanCount 방의 사람 수
func rrHumanCount(room *rrRoom) int {
	return humanCountOf(room.Clients)
}

// updateLobbyWaiting 로비 현황판 갱신 — 사람 1명 이상 대기 && 미시작.
// 사설 방은 현황판에 노출하지 않는다 (초대 링크로만 접근).
func (h *RRHub) updateLobbyWaiting(room *rrRoom) {
	if room.Code != "" {
		return
	}
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && rrHumanCount(room) >= 1
	lobbySetWaiting("ricochet", waiting)
}

// handleFillBots host 가 빈 좌석을 연습봇으로 RRFillBotTarget 명까지 채운 뒤
// 즉시 시작한다 (스폰은 join 을 거치지 않으므로 명시 시작 호출).
func (h *RRHub) handleFillBots(client *RRClient) {
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
	for len(room.Game.Players) < RRFillBotTarget {
		botNo++
		if !h.spawnRRBot(room, fmt.Sprintf("%s%d", botName, botNo)) {
			break
		}
	}

	if room.Game.CanStart() {
		h.startGame(room)
		return
	}
	h.broadcastState(room)
}

func (h *RRHub) handleStart(client *RRClient) {
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
		h.sendError(client, fmt.Sprintf("%d명 이상 모여야 시작할 수 있습니다", RRMinPlayers))
		return
	}
	h.startGame(room)
}

func (h *RRHub) startGame(room *rrRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	if room.Code != "" {
		// 시작한 사설 방의 코드는 진행 중 장부로 옮긴다 (관전 입장 근거)
		delete(h.privateLobbies, room.Code)
		h.activeCodes[room.Code] = room.Game.ID
	} else {
		h.lobby = nil // 시작한 방은 로비에서 떼어낸다
		lobbySetWaiting("ricochet", false)
	}

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	log.Printf("[리코셰][경기시작] game=%s | 인원=%d | 목표=%d개 | 캡=%.0f분 | %v",
		room.Game.ID, len(room.Game.Players), RRGoalTotal, h.gameCap.Minutes(), names)
	if !rrRoomHasBot(room) { // 연습봇전은 운영자 알림을 억제한다
		notify("리코셰 로봇 시작", fmt.Sprintf("%d인전 시작", len(room.Game.Players)))
	}

	h.broadcastEvent(room, RREventPayload{Kind: "game_started",
		Message: fmt.Sprintf(
			"게임 시작 — %d인전, 차례가 없습니다. 답을 찾으면 이동 횟수를 외치세요 (첫 외침 뒤 %.0f초 카운트다운)",
			len(room.Game.Players), h.bidWindow.Seconds())})

	h.scheduleGameCap(room)
	h.afterProgress(room)
}

// removeFromLobby 대기실에서 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *RRHub) removeFromLobby(room *rrRoom, client *RRClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	// 좌석 압축: oldSeat 뒤의 클라이언트를 한 칸씩 당긴다
	rebuilt := map[int]*RRClient{}
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

	log.Printf("[리코셰][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), RRMaxPlayers)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if rrHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, RRMessage{Type: RRMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			lobbySetWaiting("ricochet", false)
		}
		return
	}

	h.broadcastEvent(room, RREventPayload{Kind: "left", Name: client.Name,
		Message: fmt.Sprintf("%s님이 퇴장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 (선착 판정) ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *RRHub) roomOf(client *RRClient) *rrRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

// handleBid 외침. 차례 검사가 없다 — 누구든 언제든 보낼 수 있고, 이 함수에
// 도착한 순서가 곧 우선순위다(허브 고루틴 직렬화).
func (h *RRHub) handleBid(client *RRClient, msg RRMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload RRBidPayload
	json.Unmarshal(payloadBytes, &payload)

	game := room.Game
	if err := game.Bid(client.Seat, payload.Moves); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[리코셰][외침] game=%s | 목표%d | seat%d=%s → %d회 (최소 %d회)",
		game.ID, game.GoalIndex+1, client.Seat, displayName(client.Name),
		payload.Moves, game.MinMoves)
	h.afterProgress(room)
}

// handleDemo 증명 제출 — 이동 순서를 그대로 적용해 판정한다
func (h *RRHub) handleDemo(client *RRClient, msg RRMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload RRDemoPayload
	json.Unmarshal(payloadBytes, &payload)

	game := room.Game
	if err := game.Demo(client.Seat, payload.Moves); err != nil {
		h.sendError(client, err.Error())
		return
	}
	if r := game.LastResult; r != nil {
		log.Printf("[리코셰][증명] game=%s | 목표%d | seat%d=%s | %d회 → %s",
			game.ID, game.GoalIndex+1, client.Seat, displayName(client.Name),
			r.Moves, r.Message)
	}
	h.afterProgress(room)
}

// handlePass 증명 포기 — 다음으로 적게 외친 사람에게 넘어간다
func (h *RRHub) handlePass(client *RRClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	if err := room.Game.Pass(client.Seat); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[리코셰][포기] game=%s | 목표%d | seat%d=%s 증명 포기",
		room.Game.ID, room.Game.GoalIndex+1, client.Seat, displayName(client.Name))
	h.afterProgress(room)
}

// afterProgress 모든 게임 변이 뒤의 공통 마무리 — 이벤트 방송·종료 판정·
// 단계 마감 재설정·스냅샷 방송.
func (h *RRHub) afterProgress(room *rrRoom) {
	h.drainEvents(room)
	if room.Game.Phase == RRPhaseGameOver {
		h.finishGame(room)
		return
	}
	h.syncDeadline(room)
	h.broadcastState(room)
}

// drainEvents 순수 규칙이 쌓은 이벤트를 rr_event 로 방송한다
func (h *RRHub) drainEvents(room *rrRoom) {
	for _, ev := range room.Game.DrainEvents() {
		payload := RREventPayload{Kind: ev.Kind, Message: ev.Message}
		if ev.Seat >= 0 && ev.Seat < len(room.Game.Players) {
			seat := ev.Seat
			payload.Seat = &seat
			payload.Name = room.Game.Players[seat].Name
		}
		h.broadcastEvent(room, payload)
	}
}

// ==================== 타이머 ====================

// syncDeadline (phase, goalIndex, demoTurn) 이 바뀐 순간에만 단계 마감을
// 다시 건다. **외침이 하나 더 와도 카운트다운을 다시 걸지 않는 근거**다 —
// 규칙상 60초는 첫 외침에서 한 번만 흐른다.
func (h *RRHub) syncDeadline(room *rrRoom) {
	game := room.Game
	key := fmt.Sprintf("%s:%d:%d", game.Phase, game.GoalIndex, game.DemoTurn)
	if room.PhaseKey == key {
		return
	}
	room.PhaseKey = key

	switch game.Phase {
	case RRPhaseThinking:
		h.schedulePhase(room, h.goalCap)
	case RRPhaseBidding:
		h.schedulePhase(room, h.bidWindow)
	case RRPhaseDemo:
		h.schedulePhase(room, h.demoCap)
	case RRPhaseGoalEnd:
		h.schedulePhase(room, h.goalEndDelay)
	default:
		h.stopPhaseTimer(room)
		game.Deadline = 0
	}
}

func (h *RRHub) schedulePhase(room *rrRoom, d time.Duration) {
	h.stopPhaseTimer(room)
	room.Game.StateSeq++
	room.Game.Deadline = time.Now().Add(d).UnixMilli()
	sig := rrTimerSignal{GameID: room.Game.ID, Kind: "phase", Seq: room.Game.StateSeq}
	room.PhaseTimer = time.AfterFunc(d, func() { h.timerFired <- sig })
}

func (h *RRHub) stopPhaseTimer(room *rrRoom) {
	stopTimer(&room.PhaseTimer)
}

// scheduleGameCap 시작 시 한 번만 거는 전체 캡 (무한 게임 방지)
func (h *RRHub) scheduleGameCap(room *rrRoom) {
	h.stopEndTimer(room)
	room.Game.EndSeq++
	sig := rrTimerSignal{GameID: room.Game.ID, Kind: "end", Seq: room.Game.EndSeq}
	room.EndTimer = time.AfterFunc(h.gameCap, func() { h.timerFired <- sig })
}

func (h *RRHub) stopEndTimer(room *rrRoom) {
	if room.EndTimer != nil {
		room.EndTimer.Stop()
		room.EndTimer = nil
	}
}

// stopTimers 방을 정리할 때 두 타이머를 함께 거둔다
func (h *RRHub) stopTimers(room *rrRoom) {
	h.stopPhaseTimer(room)
	h.stopEndTimer(room)
}

// handleTimerFired 두 타이머의 발화 처리 — 지나간 발화는 일련번호로 걸러낸다
func (h *RRHub) handleTimerFired(sig rrTimerSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.Game.Phase == RRPhaseGameOver {
		return
	}
	game := room.Game

	switch sig.Kind {
	case "phase":
		if game.StateSeq != sig.Seq {
			return
		}
		switch game.Phase {
		case RRPhaseThinking:
			log.Printf("[리코셰][목표넘김] game=%s | 목표%d 를 %.0f분 안에 아무도 외치지 못했다 (최소 %d회)",
				game.ID, game.GoalIndex+1, h.goalCap.Minutes(), game.MinMoves)
			game.GoalTimeout()
		case RRPhaseBidding:
			game.CloseBidding()
		case RRPhaseDemo:
			game.DemoTimeout()
		case RRPhaseGoalEnd:
			game.NextGoal(h.rng)
			if game.Phase == RRPhaseThinking {
				log.Printf("[리코셰][목표] game=%s | %d/%d번째 목표 — %s (최소 %d회)",
					game.ID, game.GoalIndex+1, RRGoalTotal,
					rrGoalLabel(game.Goal), game.MinMoves)
			}
		}
	case "end":
		if game.EndSeq != sig.Seq {
			return
		}
		log.Printf("[리코셰][강제종료] game=%s | %.0f분 경과 — %d/%d번째 목표에서 정산",
			game.ID, h.gameCap.Minutes(), game.GoalsPlayed(), RRGoalTotal)
		h.broadcastEvent(room, RREventPayload{Kind: "time_up",
			Message: "제한 시간이 끝나 현재 점수로 정산합니다"})
		game.ForceEnd()
	}

	h.afterProgress(room)
}

// ==================== 상태 뷰 (은닉 없음) ====================

// rrEmptyWalls 아직 판이 없는 대기실용 16×16 빈 벽 격자.
// 계약상 walls 는 항상 16×16 이어야 하므로 nil 을 내보내지 않는다.
func rrEmptyWalls() [][]int {
	walls := make([][]int, 0, RRSize)
	for r := 0; r < RRSize; r++ {
		walls = append(walls, make([]int, RRSize))
	}
	return walls
}

// rrDefaultRobots 판이 없는 대기실에서 보여줄 네 귀퉁이 배치
func rrDefaultRobots() map[RRColor]RRCell {
	return map[RRColor]RRCell{
		RRRed:    {R: 0, C: 0},
		RRBlue:   {R: 0, C: RRSize - 1},
		RRGreen:  {R: RRSize - 1, C: 0},
		RRYellow: {R: RRSize - 1, C: RRSize - 1},
	}
}

// buildRRState 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
//
// 이 게임에는 은닉이 없다 — viewerSeat 가 무엇이든 yourSeat 를 뺀 모든 필드가
// 동일하다. 관전자(viewerSeat -1)도 참가자와 똑같은 판·로봇·목표·외침을 본다.
// 유일하게 숨기는 값은 정답인 최소 횟수(MinMoves)이고, 그건 아무에게도 보내지
// 않는다. 빈 대기실(플레이어 0명·관전자 시점)에도 패닉 없이 빈 배열을 돌려준다.
func (h *RRHub) buildRRState(room *rrRoom, viewerSeat int) RRGameStatePayload {
	game := room.Game

	players := []RRPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		players = append(players, RRPlayerView{
			Seat:      p.Seat,
			Name:      p.Name,
			Connected: c != nil && c.Connected,
			Bot:       c != nil && c.Bot,
			Score:     p.Score,
		})
	}

	walls, robots := rrEmptyWalls(), rrDefaultRobots()
	if game.Board != nil {
		walls = make([][]int, 0, RRSize)
		for r := 0; r < RRSize; r++ {
			row := make([]int, 0, RRSize)
			for c := 0; c < RRSize; c++ {
				row = append(row, int(game.Board.Walls[rrIndex(r, c)]))
			}
			walls = append(walls, row)
		}
		robots = map[RRColor]RRCell{}
		for i, color := range rrColors {
			r, c := rrRowCol(game.Robots[i])
			robots[color] = RRCell{R: r, C: c}
		}
	}

	bids := []RRBidView{}
	for _, b := range game.SortedBids() {
		bids = append(bids, RRBidView{Seat: b.Seat, Moves: b.Moves})
	}

	return RRGameStatePayload{
		GameID:     game.ID,
		RoomCode:   room.Code,
		Phase:      game.Phase,
		HostSeat:   h.hostSeat(room),
		YourSeat:   viewerSeat,
		Spectators: len(room.Spectators),
		EndsAt:     game.Deadline,
		GoalIndex:  game.GoalIndex,
		GoalTotal:  RRGoalTotal,
		Walls:      walls,
		Robots:     robots,
		Goal:       game.Goal,
		Bids:       bids,
		DemoSeat:   game.DemoSeat(),
		Players:    players,
		LastResult: game.LastResult,
		Result:     game.Result,
	}
}

// broadcastState 좌석마다 스냅샷을 보낸다. 관전자에게 가는 스냅샷은
// yourSeat 가 -1 일 뿐 내용이 완전히 같다 (은닉 없음).
func (h *RRHub) broadcastState(room *rrRoom) {
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, RRMessage{
			Type:    RRMsgGameState,
			Payload: h.buildRRState(room, seat),
		})
	}
	if len(room.Spectators) > 0 {
		msg := RRMessage{Type: RRMsgGameState, Payload: h.buildRRState(room, -1)}
		for c := range room.Spectators {
			h.sendToClient(c, msg)
		}
	}
}

func (h *RRHub) broadcastEvent(room *rrRoom, event RREventPayload) {
	h.broadcastToRoom(room, RRMessage{Type: RRMsgEvent, Payload: event})
}

// finishGame 게임 종료 발표·전적 기록·방 정리 (재대결은 같은 방 코드
// 재입장으로 — finishGame 이 코드를 반납해 같은 코드로 새 방을 만들 수 있다)
func (h *RRHub) finishGame(room *rrRoom) {
	game := room.Game
	h.stopTimers(room)
	game.Deadline = 0

	result := game.Result
	if result == nil { // 방어선 — 종료는 항상 결과와 함께 온다
		seats, names := rrWinners(game.Players)
		result = &RRResult{WinnerSeats: seats, WinnerNames: names,
			Message: "게임이 종료됐습니다"}
	}

	winners, all := []string{}, []string{}
	for _, name := range result.WinnerNames {
		winners = append(winners, displayName(name))
	}
	for _, p := range game.Players {
		all = append(all, displayName(p.Name))
	}

	reason := game.EndReason
	if reason == "" {
		reason = "goals_done"
	}

	h.broadcastEvent(room, RREventPayload{Kind: "game_over",
		Message: fmt.Sprintf("게임 종료 — %s", result.Message)})
	// 최종 점수가 반영된 스냅샷을 먼저 보낸 뒤 종료를 알린다
	// (봇 러너는 rr_game_over 를 보고 스스로 끝난다)
	h.broadcastState(room)
	h.broadcastToRoom(room, RRMessage{
		Type: RRMsgGameOver,
		Payload: RRGameOverPayload{
			WinnerSeats: append([]int{}, result.WinnerSeats...),
			WinnerNames: append([]string{}, result.WinnerNames...),
			Reason:      reason,
			Message:     result.Message,
			GoalsPlayed: game.GoalsPlayed(),
			Players:     h.buildRRState(room, -1).Players,
		},
	})

	scores := []int{}
	for _, p := range game.Players {
		scores = append(scores, p.Score)
	}
	log.Printf("[리코셰][경기결과] game=%s | 승자=%v(%s) | 사유=%s | 점수=%v | 목표=%d/%d | 소요=%s",
		game.ID, result.WinnerSeats, strings.Join(winners, "·"), reason,
		scores, game.GoalsPlayed(), RRGoalTotal, matchDuration(game.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "ricochet",
		Players:  strings.Join(all, " vs "),
		Winner:   strings.Join(winners, "·"), // 동점 공동 승리는 "·" 로 잇는다
		Reason:   reason,
		Duration: matchSeconds(game.StartedAt),
		Bot:      rrRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
	if room.Code != "" {
		delete(h.activeCodes, room.Code) // 코드 재사용 복귀
	}
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *RRHub) handleDisconnect(client *RRClient) {
	// 관전자 연결 종료 — 세션·유예 없이 목록에서만 뗀다
	if room := h.rooms[client.GameID]; room != nil && room.Spectators[client] {
		delete(room.Spectators, client)
		h.broadcastState(room) // 관전자 수 갱신
		return
	}
	// 게임 참가 전에 끊긴 연결
	if client.SessionID == "" {
		return
	}
	// 이미 새 연결로 교체된 세션의 옛 연결
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

	// 진행 중: 유예 시간 동안 재접속을 기다린다 (만료 시 봇 대체).
	// 차례가 없어 이탈이 진행을 막지는 않지만, 증명 차례가 걸린 좌석이
	// 비어 있으면 증명 제한 시간만큼 판이 늘어지므로 봇이 이어받는다.
	log.Printf("[리코셰][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, RRMessage{
		Type: RRMsgPlayerDisconnected,
		Payload: RRPlayerDisconnectedPayload{
			Seat:         client.Seat,
			Name:         client.Name,
			GraceSeconds: int(h.grace.Seconds()),
		},
	})
	h.broadcastState(room)

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 안에 재접속하지 않은 좌석은 연습봇으로 대체한다
func (h *RRHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == RRPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[리코셰][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := h.takeoverRRBot(room, seat, client.Name)
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.broadcastEvent(room, RREventPayload{Kind: "bot_takeover", Seat: &seat,
		Name:    client.Name,
		Message: fmt.Sprintf("%s님 자리를 봇이 이어받았습니다", client.Name)})
	// 새 봇이 이 스냅샷을 받아 곧바로 퍼즐을 풀기 시작한다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *RRHub) handleRejoin(client *RRClient, msg RRMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload RRRejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, RRMessage{Type: RRMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, RRMessage{Type: RRMsgSessionExpired})
		return
	}

	// 유예 타이머 취소
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

	log.Printf("[리코셰][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, RRMessage{
		Type:    RRMsgPlayerReconnected,
		Payload: RRPlayerReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	// 전원에게 접속 상태가 반영된 스냅샷 (재접속 당사자의 판 복원 포함)
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *RRHub) clearGameSessions(room *rrRoom) {
	clearRoomSessions(&h.sessionManager, room.Clients)
}

// ==================== 전송 ====================

func (h *RRHub) sendError(client *RRClient, message string) {
	h.sendToClient(client, RRMessage{Type: RRMsgError, Payload: RRErrorPayload{Message: message}})
}

func (h *RRHub) sendToClient(client *RRClient, message RRMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[RR] ")
}

func (h *RRHub) broadcastToRoom(room *rrRoom, message RRMessage) {
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

func ServeRRWs(hub *RRHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[RR] Error upgrading connection:", err)
		return
	}

	client := &RRClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[RR] ",
		func(msg RRMessage) { hub.gameMessage <- RRGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
