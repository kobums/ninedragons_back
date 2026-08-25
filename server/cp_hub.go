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

// 쿠 대기 상태 마감 타이머 — 응답 창은 생존자 전원 허용 또는 마감 경과로
// 통과되고, 액션 차례·카드 제거·교환 방치는 자동 행동으로 해소된다
// (테스트에서 짧게 낮춘다).
var (
	cpTurnTimeout   = 30 * time.Second // action 단계 — 자동 수입(칩 10+면 자동 쿠)
	cpWindowTimeout = 15 * time.Second // 창 통과·무작위 제거·무작위 교환
)

// cpRoom 게임(순수 상태)과 좌석별 연결의 매핑
type cpRoom struct {
	Game       *CPGame
	Clients    map[int]*CPClient // seat → client
	PhaseTimer *time.Timer       // 대기 상태 마감 타이머

	// DeadlineSeq 마지막으로 마감을 건 StateSeq — 같은 대기 상태(창)에
	// 허용이 쌓일 때마다 마감이 늘어나지 않게 하는 근거
	DeadlineSeq int

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string

	// Spectators 관전자 연결 (사설 방 코드로만 진입, 상한 maxSpectators).
	// 좌석·세션이 없어 재접속을 지원하지 않는다 — 끊기면 목록에서만 뗀다.
	Spectators map[*CPClient]bool

	// LastReact 좌석별 마지막 리액션 발신 시각 (레이트리밋 장부)
	LastReact map[int]time.Time
}

// cpPhaseSignal 마감 타이머의 발화 표식 — 대기 상태 일련번호(AfkSeq)로
// 지나간 발화를 구분한다
type cpPhaseSignal struct {
	GameID string
	Seq    int
}

type CPHub struct {
	// 등록된 클라이언트
	clients map[*CPClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*cpRoom

	// 글로벌 로비. 시작 전 공용 방은 하나뿐이다.
	lobby *cpRoom

	// 사설 방 (초대 코드 → 시작 전 방). 허브 고루틴에서만 접근한다.
	// 시작하면 privateLobbies 에서 activeCodes 로 옮긴다.
	privateLobbies map[string]*cpRoom

	// 진행 중 사설 방 (초대 코드 → gameID). 시작 후에도 코드로 방을 찾아
	// 관전 입장시키는 근거다. finishGame 에서 해제한다 (코드 재사용 복귀).
	activeCodes map[string]string

	// 클라이언트 등록
	register chan *CPClient

	// 클라이언트 등록 해제
	unregister chan *CPClient

	// 게임 메시지
	gameMessage chan CPGameMessage

	// 마감 타이머 발화 (time.AfterFunc → 허브 채널 경유)
	phaseFired chan cpPhaseSignal

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*CPClient]

	// 셔플·무작위 제거·자동 행동용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type CPGameMessage struct {
	Client  *CPClient
	Message CPMessage
}

func NewCPHub() *CPHub {
	return &CPHub{
		register:       make(chan *CPClient),
		unregister:     make(chan *CPClient),
		clients:        make(map[*CPClient]bool),
		rooms:          make(map[string]*cpRoom),
		privateLobbies: make(map[string]*cpRoom),
		activeCodes:    make(map[string]string),
		gameMessage:    make(chan CPGameMessage),
		phaseFired:     make(chan cpPhaseSignal, 8),
		sessionManager: newSessionManager[*CPClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *CPHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[CP] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[CP] Client unregistered: %s", client.ID)
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

func (h *CPHub) handleGameMessage(gm CPGameMessage) {
	// 관전자는 어떤 행동도 할 수 없다 (보기 전용 — 리액션도 좌석 보유자만)
	if h.isSpectator(gm.Client) {
		h.sendError(gm.Client, spectatorDeniedMsg)
		return
	}
	switch gm.Message.Type {
	case CPMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case CPMsgReact:
		h.handleReact(gm.Client, gm.Message)
	case CPMsgFillBots:
		h.handleFillBots(gm.Client)
	case CPMsgStart:
		h.handleStart(gm.Client)
	case CPMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case CPMsgAction:
		h.handleAction(gm.Client, gm.Message)
	case CPMsgAllow:
		h.handleAllow(gm.Client)
	case CPMsgChallenge:
		h.handleChallenge(gm.Client)
	case CPMsgBlock:
		h.handleBlock(gm.Client, gm.Message)
	case CPMsgLose:
		h.handleLose(gm.Client, gm.Message)
	case CPMsgExchangeKeep:
		h.handleExchangeKeep(gm.Client, gm.Message)
	}
}

// ==================== 대기실 ====================

func (h *CPHub) handleJoinGame(client *CPClient, msg CPMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload CPJoinGamePayload
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

	log.Printf("[쿠][입장] game=%s | seat%d=%s 대기 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), CPMaxPlayers)
	if room.Code == "" { // 사설 방 입장은 조용히 (공용 로비만 알림)
		notify("쿠 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), len(room.Game.Players), CPMaxPlayers))
	}

	h.sendToClient(client, CPMessage{
		Type: CPMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
			"roomCode":  room.Code,
		},
	})

	h.broadcastEvent(room, CPEventPayload{Kind: "joined", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 입장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	// 정원이 차도 자동 시작하지 않는다 — 호스트 명시 시작만 (봇 채우기와 충돌 방지)
	h.broadcastState(room)
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다.
// 생략/빈 문자열은 기존 공용 로비 경로 그대로, "NEW"는 새 코드 발급,
// 그 외 코드는 해당 사설 방 (없으면 그 코드로 관대하게 새로 생성).
func (h *CPHub) lobbyRoomFor(roomField string) *cpRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewCPGame(uuid.New().String())
			h.lobby = &cpRoom{Game: game, Clients: map[int]*CPClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[CP] Created lobby game %s", game.ID)
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
		game := NewCPGame(uuid.New().String())
		room = &cpRoom{Game: game, Clients: map[int]*CPClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[CP] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// ==================== 관전 / 리액션 ====================

// addSpectator 진행 중(또는 가득 찬) 사설 방에 관전자로 등록한다.
// 좌석·세션 없음 — 재접속 미지원, 끊기면 다시 코드로 들어온다.
func (h *CPHub) addSpectator(room *cpRoom, client *CPClient, name string) {
	if len(room.Spectators) >= maxSpectators {
		h.sendError(client, spectatorFullMsg)
		return
	}
	if room.Spectators == nil {
		room.Spectators = map[*CPClient]bool{}
	}
	client.Name = name
	client.GameID = room.Game.ID
	room.Spectators[client] = true

	log.Printf("[쿠][관전] game=%s | %s 관전 입장 (%d명)",
		room.Game.ID, displayName(name), len(room.Spectators))

	h.sendToClient(client, CPMessage{
		Type:    CPMsgSpectateJoined,
		Payload: map[string]interface{}{"gameId": room.Game.ID, "roomCode": room.Code},
	})
	// 전원(관전자 포함)에게 관전자 수가 반영된 스냅샷 — 신규 관전자의 첫 스냅샷 겸용
	h.broadcastState(room)
}

// isSpectator 관전자 연결인지 (좌석 보유자·미입장 연결은 false)
func (h *CPHub) isSpectator(client *CPClient) bool {
	if client.GameID == "" {
		return false
	}
	room := h.rooms[client.GameID]
	return room != nil && room.Spectators[client]
}

// handleReact 리액션 이모지 — 좌석 보유자만, waiting 중에도 허용.
// 화이트리스트 외·레이트리밋 초과는 조용히 무시한다 (상태 저장 없음).
func (h *CPHub) handleReact(client *CPClient, msg CPMessage) {
	room := h.rooms[client.GameID]
	if room == nil || client.Seat < 0 {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload CPReactPayload
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
	h.broadcastEvent(room, CPEventPayload{Kind: "react", Seat: &seat, Name: client.Name, Message: payload.Emoji})
}

// waitingRoomOf 클라이언트가 속한 시작 전 방 (공용 로비 또는 사설 방)
func (h *CPHub) waitingRoomOf(client *CPClient) *cpRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Ready {
		return nil
	}
	return room
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *CPHub) hostSeat(room *cpRoom) int {
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

// cpHumanCount 방의 사람 수
func cpHumanCount(room *cpRoom) int {
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
func (h *CPHub) updateLobbyWaiting(room *cpRoom) {
	if room.Code != "" {
		return
	}
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && cpHumanCount(room) >= 1
	lobbySetWaiting("coup", waiting)
}

// handleFillBots host 가 빈 좌석을 연습봇으로 4인까지 채운 뒤 즉시
// 시작한다 (스폰은 join 을 거치지 않으므로 명시 시작 호출).
func (h *CPHub) handleFillBots(client *CPClient) {
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
	for len(room.Game.Players) < CPFillBotTarget {
		botNo++
		if !h.spawnCPBot(room, fmt.Sprintf("%s%d", botName, botNo)) {
			break
		}
	}

	if room.Game.CanStart() {
		h.startGame(room)
		return
	}
	h.broadcastState(room)
}

func (h *CPHub) handleStart(client *CPClient) {
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

func (h *CPHub) startGame(room *cpRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	if room.Code != "" {
		// 시작한 사설 방의 코드는 진행 중 장부로 옮긴다 (관전 입장 근거)
		delete(h.privateLobbies, room.Code)
		h.activeCodes[room.Code] = room.Game.ID
	} else {
		h.lobby = nil // 시작한 방은 로비에서 떼어낸다
		lobbySetWaiting("coup", false)
	}

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	log.Printf("[쿠][경기시작] game=%s | 인원=%d | 선=seat%d | %v",
		room.Game.ID, len(room.Game.Players), room.Game.CurrentSeat, names)
	if !cpRoomHasBot(room) { // 연습봇전은 운영자 알림을 억제한다
		notify("쿠 게임 시작", fmt.Sprintf("%d인전 시작", len(room.Game.Players)))
	}

	first := room.Game.CurrentSeat
	h.broadcastEvent(room, CPEventPayload{Kind: "game_started", Seat: &first,
		Name: room.Game.Players[first].Name,
		Message: fmt.Sprintf("게임 시작 — %s님부터 (비공개 카드 %d장, 시작 은화 %d개)",
			room.Game.Players[first].Name, CPCardsPerPlayer, CPStartChips)})
	h.afterProgress(room)
}

// removeFromLobby 대기실에서 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *CPHub) removeFromLobby(room *cpRoom, client *CPClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	// 좌석 압축: oldSeat 뒤의 클라이언트를 한 칸씩 당긴다
	rebuilt := map[int]*CPClient{}
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

	log.Printf("[쿠][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), CPMaxPlayers)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if cpHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, CPMessage{Type: CPMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			lobbySetWaiting("coup", false)
		}
		return
	}

	h.broadcastEvent(room, CPEventPayload{Kind: "left", Name: client.Name,
		Message: fmt.Sprintf("%s님이 퇴장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *CPHub) roomOf(client *CPClient) *cpRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

func (h *CPHub) handleAction(client *CPClient, msg CPMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload CPActionPayload
	json.Unmarshal(payloadBytes, &payload)

	target := -1
	if payload.TargetSeat != nil {
		target = *payload.TargetSeat
	}
	game := room.Game
	if err := game.DeclareAction(client.Seat, CPActionKind(payload.Kind), target, h.rng); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[쿠][액션] game=%s | seat%d=%s %s (대상 seat%d)",
		game.ID, client.Seat, displayName(client.Name), payload.Kind, target)
	h.afterProgress(room)
}

// handleAllow 창 통과 동의 — 뒤늦은(이미 지나간 창) 허용은 순수 규칙이
// 조용히 무시한다 (창 경쟁의 정상 경로라 에러로 취급하지 않는다)
func (h *CPHub) handleAllow(client *CPClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	room.Game.SubmitAllow(client.Seat, h.rng)
	h.afterProgress(room)
}

// handleChallenge 도전 — 뒤늦은 도전 역시 조용히 무시된다
func (h *CPHub) handleChallenge(client *CPClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	room.Game.SubmitChallenge(client.Seat, h.rng)
	h.afterProgress(room)
}

func (h *CPHub) handleBlock(client *CPClient, msg CPMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload CPBlockPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.SubmitBlock(client.Seat, CPRole(payload.Role)); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.afterProgress(room)
}

func (h *CPHub) handleLose(client *CPClient, msg CPMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload CPLosePayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.SubmitLose(client.Seat, payload.Index, h.rng); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.afterProgress(room)
}

func (h *CPHub) handleExchangeKeep(client *CPClient, msg CPMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload CPExchangeKeepPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.SubmitExchangeKeep(client.Seat, payload.Indices, h.rng); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.afterProgress(room)
}

// afterProgress 모든 게임 변이 뒤의 공통 마무리 — 이벤트 방송·종료 판정·
// 새 대기 상태의 마감 예약·스냅샷 방송을 한 번에 처리한다.
func (h *CPHub) afterProgress(room *cpRoom) {
	h.drainEvents(room)
	if room.Game.Phase == CPPhaseGameOver {
		h.finishGame(room)
		return
	}
	h.syncDeadline(room)
	h.broadcastState(room)
}

// drainEvents 순수 규칙이 쌓은 이벤트를 cp_event 로 방송한다
func (h *CPHub) drainEvents(room *cpRoom) {
	for _, ev := range room.Game.DrainEvents() {
		payload := CPEventPayload{Kind: ev.Kind, Message: ev.Message}
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
// 같은 창에 허용이 쌓이는 동안에는 처음 건 마감을 유지한다 (전원 공용 마감).
func (h *CPHub) syncDeadline(room *cpRoom) {
	game := room.Game
	if room.DeadlineSeq == game.StateSeq {
		return
	}
	room.DeadlineSeq = game.StateSeq
	dur := cpWindowTimeout
	if game.Phase == CPPhaseAction {
		dur = cpTurnTimeout
	}
	h.scheduleDeadline(room, dur)
}

// scheduleDeadline 마감 타이머 예약. AfkSeq 를 올려 지나간 발화를 구분하고,
// 발화는 허브 채널을 경유하므로 허브 밖에서 상태를 만지지 않는다.
func (h *CPHub) scheduleDeadline(room *cpRoom, dur time.Duration) {
	h.stopPhaseTimer(room)
	room.Game.AfkSeq++
	room.Game.Deadline = time.Now().Add(dur).UnixMilli()
	sig := cpPhaseSignal{GameID: room.Game.ID, Seq: room.Game.AfkSeq}
	room.PhaseTimer = time.AfterFunc(dur, func() {
		h.phaseFired <- sig
	})
}

func (h *CPHub) stopPhaseTimer(room *cpRoom) {
	if room.PhaseTimer != nil {
		room.PhaseTimer.Stop()
		room.PhaseTimer = nil
	}
}

// handlePhaseFired 마감 발화 — 단계별 자동 행동으로 해소한다.
//   - action: 자동 수입 (칩 10+면 쿠 강제라 자동 쿠)
//   - 세 응답 창: 전원 허용과 같은 통과
//   - lose_card: 무작위 카드 제거
//   - exchange_pick: 무작위 유지
func (h *CPHub) handlePhaseFired(sig cpPhaseSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.Game.AfkSeq != sig.Seq {
		return
	}
	game := room.Game
	switch game.Phase {
	case CPPhaseAction:
		seat := game.CurrentSeat
		if seat < 0 {
			return
		}
		actor := game.Players[seat]
		h.broadcastEvent(room, CPEventPayload{Kind: "afk", Seat: &seat, Name: actor.Name,
			Message: fmt.Sprintf("%s님이 오래 응답하지 않아 자동으로 진행합니다", actor.Name)})
		if actor.Chips >= CPForceCoupChips {
			game.DeclareAction(seat, CPActCoup, game.AutoCoupTarget(seat), h.rng)
		} else {
			game.DeclareAction(seat, CPActIncome, -1, h.rng)
		}
		log.Printf("[쿠][자동진행] game=%s | seat%d 무응답 — 자동 액션", game.ID, seat)

	case CPPhaseChallengeWindow, CPPhaseBlockWindow, CPPhaseBlockChallengeWindow:
		log.Printf("[쿠][창통과] game=%s | %s 마감 — 통과", game.ID, game.Phase)
		game.ForcePassWindow(h.rng)

	case CPPhaseLoseCard:
		seat := game.LoseSeat
		if seat < 0 {
			return
		}
		n := len(game.Players[seat].HiddenIdx())
		if n == 0 {
			return
		}
		h.broadcastEvent(room, CPEventPayload{Kind: "afk", Seat: &seat, Name: game.Players[seat].Name,
			Message: fmt.Sprintf("%s님이 카드를 선택하지 않아 자동으로 무작위 제거합니다", game.Players[seat].Name)})
		game.SubmitLose(seat, h.rng.Intn(n), h.rng)
		log.Printf("[쿠][자동제거] game=%s | seat%d 무응답 — 무작위 제거", game.ID, seat)

	case CPPhaseExchangePick:
		game.AutoExchangeKeep(h.rng)
		log.Printf("[쿠][자동교환] game=%s | 무응답 — 무작위 유지", game.ID)

	default:
		return
	}
	h.afterProgress(room)
}

// ==================== 상태 뷰 (은닉의 핵심) ====================

// buildCPState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를
// 쓴다. 은닉: 비공개 카드(yourCards)·교환 선택지(yourExchange)는 본인에게만
// 내용이 실린다 — 타인·관전자(viewerSeat -1)는 빈 배열 []. 공개로 뒤집힌
// 카드(revealed)와 장수(cardCount)·덱 장수(deckCount)만 공용 정보다.
func (h *CPHub) buildCPState(room *cpRoom, viewerSeat int) CPGameStatePayload {
	game := room.Game

	yourCards := []string{}
	if viewerSeat >= 0 && viewerSeat < len(game.Players) {
		for _, r := range game.Players[viewerSeat].HiddenRoles() {
			yourCards = append(yourCards, string(r))
		}
	}
	yourExchange := []string{}
	if game.Phase == CPPhaseExchangePick && game.Pending != nil &&
		viewerSeat == game.Pending.ActorSeat {
		for _, r := range game.ExchangeCards {
			yourExchange = append(yourExchange, string(r))
		}
	}

	players := []CPPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		revealed := []string{}
		for _, card := range p.Cards {
			if card.Revealed {
				revealed = append(revealed, string(card.Role))
			}
		}
		players = append(players, CPPlayerView{
			Seat:      p.Seat,
			Name:      p.Name,
			Connected: c != nil && c.Connected,
			Bot:       c != nil && c.Bot,
			Alive:     !game.Ready || p.Alive(), // 시작 전에는 카드가 없어도 생존 표기
			Chips:     p.Chips,
			Revealed:  revealed,
			CardCount: len(p.HiddenIdx()),
		})
	}

	var pending *CPPendingView
	if game.Pending != nil {
		pending = &CPPendingView{
			Kind:        string(game.Pending.Kind),
			ActorSeat:   game.Pending.ActorSeat,
			TargetSeat:  game.Pending.TargetSeat,
			BlockerSeat: game.Pending.BlockerSeat,
			BlockRole:   string(game.Pending.BlockRole),
			Message:     game.Pending.Message,
		}
	}

	endsAt := int64(0)
	switch game.Phase {
	case CPPhaseAction, CPPhaseChallengeWindow, CPPhaseBlockWindow,
		CPPhaseBlockChallengeWindow, CPPhaseLoseCard, CPPhaseExchangePick:
		endsAt = game.Deadline
	}

	return CPGameStatePayload{
		GameID:       game.ID,
		RoomCode:     room.Code,
		Phase:        game.Phase,
		HostSeat:     h.hostSeat(room),
		YourSeat:     viewerSeat,
		Spectators:   len(room.Spectators),
		EndsAt:       endsAt,
		CurrentSeat:  game.CurrentSeat,
		Pending:      pending,
		LoseSeat:     game.LoseSeat,
		YourCards:    yourCards,
		YourExchange: yourExchange,
		Players:      players,
		DeckCount:    len(game.Deck),
	}
}

// broadcastState 좌석마다 개인화 스냅샷을 만들어 보낸다.
// 관전자에게는 공개 정보만 담긴 스냅샷(viewerSeat -1)이 간다.
func (h *CPHub) broadcastState(room *cpRoom) {
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, CPMessage{
			Type:    CPMsgGameState,
			Payload: h.buildCPState(room, seat),
		})
	}
	if len(room.Spectators) > 0 {
		msg := CPMessage{Type: CPMsgGameState, Payload: h.buildCPState(room, -1)}
		for c := range room.Spectators {
			h.sendToClient(c, msg)
		}
	}
}

func (h *CPHub) broadcastEvent(room *cpRoom, event CPEventPayload) {
	h.broadcastToRoom(room, CPMessage{Type: CPMsgEvent, Payload: event})
}

// finishGame 게임 종료 발표·전적 기록·방 정리 (재대결은 같은 방 코드
// 재입장으로 — finishGame 이 코드를 반납해 같은 코드로 새 방을 만들 수 있다)
func (h *CPHub) finishGame(room *cpRoom) {
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

	h.broadcastEvent(room, CPEventPayload{Kind: "game_over", Seat: &winner,
		Name:    winnerName,
		Message: fmt.Sprintf("%s님이 최후의 1인으로 승리했습니다!", winnerName)})
	// 최종 스냅샷을 먼저 보낸 뒤 종료를 알린다
	// (봇 러너는 cp_game_over 를 보고 스스로 끝난다)
	h.broadcastState(room)
	h.broadcastToRoom(room, CPMessage{
		Type: CPMsgGameOver,
		Payload: CPGameOverPayload{
			WinnerSeat: winner,
			WinnerName: winnerName,
			Players:    h.buildCPState(room, -1).Players,
		},
	})

	log.Printf("[쿠][경기결과] game=%s | 승자=seat%d(%s) | 소요=%s",
		game.ID, winner, displayName(winnerName), matchDuration(game.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "coup",
		Players:  strings.Join(names, " vs "),
		Winner:   displayName(winnerName),
		Reason:   "last_standing",
		Duration: matchSeconds(game.StartedAt),
		Bot:      cpRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
	if room.Code != "" {
		delete(h.activeCodes, room.Code) // 코드 재사용 복귀
	}
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *CPHub) handleDisconnect(client *CPClient) {
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

	// 진행 중: 유예 시간 동안 재접속을 기다린다 (만료 시 봇 대체)
	log.Printf("[쿠][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, CPMessage{
		Type: CPMsgPlayerDisconnected,
		Payload: CPPlayerDisconnectedPayload{
			Seat:         client.Seat,
			Name:         client.Name,
			GraceSeconds: int(h.grace.Seconds()),
		},
	})
	h.broadcastState(room)

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 안에 재접속하지 않은 좌석은 연습봇으로 대체하고
// 게임은 계속한다 — 창 허용·카드 제거가 이탈 좌석에 막히지 않는 근거
func (h *CPHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == CPPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[쿠][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := h.takeoverCPBot(room, seat, client.Name)
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.broadcastEvent(room, CPEventPayload{Kind: "bot_takeover", Seat: &seat,
		Name:    client.Name,
		Message: fmt.Sprintf("%s님 자리를 봇이 이어받았습니다", client.Name)})
	// 새 봇이 이 스냅샷을 받아 응답이 남았으면 즉시 이어간다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *CPHub) handleRejoin(client *CPClient, msg CPMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload CPRejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, CPMessage{Type: CPMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, CPMessage{Type: CPMsgSessionExpired})
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

	log.Printf("[쿠][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, CPMessage{
		Type:    CPMsgPlayerReconnected,
		Payload: CPPlayerReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	// 전원에게 접속 상태가 반영된 스냅샷 (재접속 당사자 복원 포함)
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *CPHub) clearGameSessions(room *cpRoom) {
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		h.drop(c.SessionID)
	}
}

// ==================== 전송 ====================

func (h *CPHub) sendError(client *CPClient, message string) {
	h.sendToClient(client, CPMessage{Type: CPMsgError, Payload: CPErrorPayload{Message: message}})
}

func (h *CPHub) sendToClient(client *CPClient, message CPMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[CP] ")
}

func (h *CPHub) broadcastToRoom(room *cpRoom, message CPMessage) {
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

func ServeCPWs(hub *CPHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[CP] Error upgrading connection:", err)
		return
	}

	client := &CPClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[CP] ",
		func(msg CPMessage) { hub.gameMessage <- CPGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
