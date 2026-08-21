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

// crTurnTimeout 대기 상태(전달·결정) 마감 — 발화하면 자동 행동으로 해소한다.
// 판정은 50/50 무작위, 전달 차례면 무작위 카드·무작위 대상·실물 그대로 선언
// (테스트에서 짧게 낮춘다).
var crTurnTimeout = 30 * time.Second

// crRoom 게임(순수 상태)과 좌석별 연결의 매핑
type crRoom struct {
	Game       *CRGame
	Clients    map[int]*CRClient // seat → client
	PhaseTimer *time.Timer       // 대기 상태 마감 타이머

	// DeadlineSeq 마지막으로 마감을 건 StateSeq — 같은 대기 상태에 스냅샷이
	// 여러 번 방송돼도 마감·cr_peek 이 한 번만 나가는 근거
	DeadlineSeq int

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string

	// Spectators 관전자 연결 (사설 방 코드로만 진입, 상한 maxSpectators).
	// 좌석·세션이 없어 재접속을 지원하지 않는다 — 끊기면 목록에서만 뗀다.
	Spectators map[*CRClient]bool

	// LastReact 좌석별 마지막 리액션 발신 시각 (레이트리밋 장부)
	LastReact map[int]time.Time
}

// crPhaseSignal 마감 타이머의 발화 표식 — 대기 상태 일련번호(AfkSeq)로
// 지나간 발화를 구분한다
type crPhaseSignal struct {
	GameID string
	Seq    int
}

type CRHub struct {
	// 등록된 클라이언트
	clients map[*CRClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*crRoom

	// 글로벌 로비. 시작 전 공용 방은 하나뿐이다.
	lobby *crRoom

	// 사설 방 (초대 코드 → 시작 전 방). 허브 고루틴에서만 접근한다.
	// 시작하면 privateLobbies 에서 activeCodes 로 옮긴다.
	privateLobbies map[string]*crRoom

	// 진행 중 사설 방 (초대 코드 → gameID). 시작 후에도 코드로 방을 찾아
	// 관전 입장시키는 근거다. finishGame 에서 해제한다 (코드 재사용 복귀).
	activeCodes map[string]string

	// 클라이언트 등록
	register chan *CRClient

	// 클라이언트 등록 해제
	unregister chan *CRClient

	// 게임 메시지
	gameMessage chan CRGameMessage

	// 마감 타이머 발화 (time.AfterFunc → 허브 채널 경유)
	phaseFired chan crPhaseSignal

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*CRClient]

	// 셔플·자동 행동용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type CRGameMessage struct {
	Client  *CRClient
	Message CRMessage
}

func NewCRHub() *CRHub {
	return &CRHub{
		register:       make(chan *CRClient),
		unregister:     make(chan *CRClient),
		clients:        make(map[*CRClient]bool),
		rooms:          make(map[string]*crRoom),
		privateLobbies: make(map[string]*crRoom),
		activeCodes:    make(map[string]string),
		gameMessage:    make(chan CRGameMessage),
		phaseFired:     make(chan crPhaseSignal, 8),
		sessionManager: newSessionManager[*CRClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *CRHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[CR] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[CR] Client unregistered: %s", client.ID)
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

func (h *CRHub) handleGameMessage(gm CRGameMessage) {
	// 관전자는 어떤 행동도 할 수 없다 (보기 전용 — 리액션도 좌석 보유자만)
	if h.isSpectator(gm.Client) {
		h.sendError(gm.Client, spectatorDeniedMsg)
		return
	}
	switch gm.Message.Type {
	case CRMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case CRMsgReact:
		h.handleReact(gm.Client, gm.Message)
	case CRMsgFillBots:
		h.handleFillBots(gm.Client)
	case CRMsgStart:
		h.handleStart(gm.Client)
	case CRMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case CRMsgPassCard:
		h.handlePassCard(gm.Client, gm.Message)
	case CRMsgRelay:
		h.handleRelay(gm.Client, gm.Message)
	case CRMsgJudge:
		h.handleJudge(gm.Client, gm.Message)
	}
}

// ==================== 대기실 ====================

func (h *CRHub) handleJoinGame(client *CRClient, msg CRMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload CRJoinGamePayload
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

	log.Printf("[바퀴벌레][입장] game=%s | seat%d=%s 대기 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), CRMaxPlayers)
	if room.Code == "" { // 사설 방 입장은 조용히 (공용 로비만 알림)
		notify("바퀴벌레 포커 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), len(room.Game.Players), CRMaxPlayers))
	}

	h.sendToClient(client, CRMessage{
		Type: CRMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
			"roomCode":  room.Code,
		},
	})

	h.broadcastEvent(room, CREventPayload{Kind: "joined", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 입장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	// 정원이 차도 자동 시작하지 않는다 — 호스트 명시 시작만 (봇 채우기와 충돌 방지)
	h.broadcastState(room)
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다.
// 생략/빈 문자열은 기존 공용 로비 경로 그대로, "NEW"는 새 코드 발급,
// 그 외 코드는 해당 사설 방 (없으면 그 코드로 관대하게 새로 생성).
func (h *CRHub) lobbyRoomFor(roomField string) *crRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewCRGame(uuid.New().String())
			h.lobby = &crRoom{Game: game, Clients: map[int]*CRClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[CR] Created lobby game %s", game.ID)
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
		game := NewCRGame(uuid.New().String())
		room = &crRoom{Game: game, Clients: map[int]*CRClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[CR] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// ==================== 관전 / 리액션 ====================

// addSpectator 진행 중(또는 가득 찬) 사설 방에 관전자로 등록한다.
// 좌석·세션 없음 — 재접속 미지원, 끊기면 다시 코드로 들어온다.
func (h *CRHub) addSpectator(room *crRoom, client *CRClient, name string) {
	if len(room.Spectators) >= maxSpectators {
		h.sendError(client, spectatorFullMsg)
		return
	}
	if room.Spectators == nil {
		room.Spectators = map[*CRClient]bool{}
	}
	client.Name = name
	client.GameID = room.Game.ID
	room.Spectators[client] = true

	log.Printf("[바퀴벌레][관전] game=%s | %s 관전 입장 (%d명)",
		room.Game.ID, displayName(name), len(room.Spectators))

	h.sendToClient(client, CRMessage{
		Type:    CRMsgSpectateJoined,
		Payload: map[string]interface{}{"gameId": room.Game.ID, "roomCode": room.Code},
	})
	// 전원(관전자 포함)에게 관전자 수가 반영된 스냅샷 — 신규 관전자의 첫 스냅샷 겸용
	h.broadcastState(room)
}

// isSpectator 관전자 연결인지 (좌석 보유자·미입장 연결은 false)
func (h *CRHub) isSpectator(client *CRClient) bool {
	if client.GameID == "" {
		return false
	}
	room := h.rooms[client.GameID]
	return room != nil && room.Spectators[client]
}

// handleReact 리액션 이모지 — 좌석 보유자만, waiting 중에도 허용.
// 화이트리스트 외·레이트리밋 초과는 조용히 무시한다 (상태 저장 없음).
func (h *CRHub) handleReact(client *CRClient, msg CRMessage) {
	room := h.rooms[client.GameID]
	if room == nil || client.Seat < 0 {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload CRReactPayload
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
	h.broadcastEvent(room, CREventPayload{Kind: "react", Seat: &seat, Name: client.Name, Message: payload.Emoji})
}

// waitingRoomOf 클라이언트가 속한 시작 전 방 (공용 로비 또는 사설 방)
func (h *CRHub) waitingRoomOf(client *CRClient) *crRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Ready {
		return nil
	}
	return room
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *CRHub) hostSeat(room *crRoom) int {
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

// crHumanCount 방의 사람 수
func crHumanCount(room *crRoom) int {
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
func (h *CRHub) updateLobbyWaiting(room *crRoom) {
	if room.Code != "" {
		return
	}
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && crHumanCount(room) >= 1
	lobbySetWaiting("cockroach", waiting)
}

// handleFillBots host 가 빈 좌석을 연습봇으로 4인까지 채운 뒤 즉시
// 시작한다 (스폰은 join 을 거치지 않으므로 명시 시작 호출).
func (h *CRHub) handleFillBots(client *CRClient) {
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
	for len(room.Game.Players) < CRFillBotTarget {
		botNo++
		if !h.spawnCRBot(room, fmt.Sprintf("%s%d", botName, botNo)) {
			break
		}
	}

	if room.Game.CanStart() {
		h.startGame(room)
		return
	}
	h.broadcastState(room)
}

func (h *CRHub) handleStart(client *CRClient) {
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
		h.sendError(client, fmt.Sprintf("%d명 이상 모여야 시작할 수 있습니다", CRMinPlayers))
		return
	}
	h.startGame(room)
}

func (h *CRHub) startGame(room *crRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	if room.Code != "" {
		// 시작한 사설 방의 코드는 진행 중 장부로 옮긴다 (관전 입장 근거)
		delete(h.privateLobbies, room.Code)
		h.activeCodes[room.Code] = room.Game.ID
	} else {
		h.lobby = nil // 시작한 방은 로비에서 떼어낸다
		lobbySetWaiting("cockroach", false)
	}

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	per := len(room.Game.Players[0].Hand)
	log.Printf("[바퀴벌레][경기시작] game=%s | 인원=%d | 각 %d장 | 선=seat%d | %v",
		room.Game.ID, len(room.Game.Players), per, room.Game.PasserSeat, names)
	if !crRoomHasBot(room) { // 연습봇전은 운영자 알림을 억제한다
		notify("바퀴벌레 포커 게임 시작", fmt.Sprintf("%d인전 시작", len(room.Game.Players)))
	}

	first := room.Game.PasserSeat
	h.broadcastEvent(room, CREventPayload{Kind: "game_started", Seat: &first,
		Name: room.Game.Players[first].Name,
		Message: fmt.Sprintf("게임 시작 — %s님부터 전달합니다 (각 %d장, 같은 동물 %d장이 모이면 패배)",
			room.Game.Players[first].Name, per, CRLoseCount)})
	h.afterProgress(room)
}

// removeFromLobby 대기실에서 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *CRHub) removeFromLobby(room *crRoom, client *CRClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	// 좌석 압축: oldSeat 뒤의 클라이언트를 한 칸씩 당긴다
	rebuilt := map[int]*CRClient{}
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

	log.Printf("[바퀴벌레][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), CRMaxPlayers)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if crHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, CRMessage{Type: CRMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			lobbySetWaiting("cockroach", false)
		}
		return
	}

	h.broadcastEvent(room, CREventPayload{Kind: "left", Name: client.Name,
		Message: fmt.Sprintf("%s님이 퇴장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *CRHub) roomOf(client *CRClient) *crRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

func (h *CRHub) handlePassCard(client *CRClient, msg CRMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload CRPassCardPayload
	json.Unmarshal(payloadBytes, &payload)

	game := room.Game
	if err := game.PassCard(client.Seat, CRAnimal(payload.Card), payload.TargetSeat,
		CRAnimal(payload.Claim)); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[바퀴벌레][전달] game=%s | seat%d=%s → seat%d 선언=%s",
		game.ID, client.Seat, displayName(client.Name), payload.TargetSeat, payload.Claim)
	h.afterProgress(room)
}

func (h *CRHub) handleRelay(client *CRClient, msg CRMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload CRRelayPayload
	json.Unmarshal(payloadBytes, &payload)

	game := room.Game
	if err := game.Relay(client.Seat, payload.TargetSeat, CRAnimal(payload.Claim)); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[바퀴벌레][릴레이] game=%s | seat%d=%s → seat%d 선언=%s (체인 %d)",
		game.ID, client.Seat, displayName(client.Name), payload.TargetSeat, payload.Claim, len(game.Chain))
	h.afterProgress(room)
}

func (h *CRHub) handleJudge(client *CRClient, msg CRMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload CRJudgePayload
	json.Unmarshal(payloadBytes, &payload)

	game := room.Game
	if err := game.Judge(client.Seat, payload.Truth); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[바퀴벌레][판정] game=%s | seat%d=%s truth=%v",
		game.ID, client.Seat, displayName(client.Name), payload.Truth)
	h.afterProgress(room)
}

// afterProgress 모든 게임 변이 뒤의 공통 마무리 — 이벤트 방송·종료 판정·
// 새 대기 상태의 마감 예약·cr_peek 발송·스냅샷 방송을 한 번에 처리한다.
func (h *CRHub) afterProgress(room *crRoom) {
	h.drainEvents(room)
	if room.Game.Phase == CRPhaseGameOver {
		h.finishGame(room)
		return
	}
	// 새 대기 상태(StateSeq 변경)가 열렸을 때만 마감·cr_peek 을 건다 —
	// 관전 입장 등으로 같은 상태의 스냅샷이 반복 방송돼도 중복되지 않는다
	if room.DeadlineSeq != room.Game.StateSeq {
		room.DeadlineSeq = room.Game.StateSeq
		h.sendPeek(room, room.Game.HolderSeat)
		h.scheduleDeadline(room, crTurnTimeout)
	}
	h.broadcastState(room)
}

// sendPeek 릴레이 카드 실물을 결정권자에게만 개인 이벤트로 보낸다 — 넘기기가
// 가능한 좌석만 (마지막 남은 사람은 강제 판정이라 실물을 볼 수 없다).
// 스냅샷 방송보다 먼저 보내야 봇이 실물을 알고 결정한다.
func (h *CRHub) sendPeek(room *crRoom, seat int) {
	game := room.Game
	if game.Phase != CRPhaseDeciding || seat != game.HolderSeat || !game.CanRelay(seat) {
		return
	}
	h.sendToClient(room.Clients[seat], CRMessage{
		Type:    CRMsgPeek,
		Payload: CRPeekPayload{Animal: string(game.Card)},
	})
}

// drainEvents 순수 규칙이 쌓은 이벤트를 cr_event 로 방송한다
func (h *CRHub) drainEvents(room *crRoom) {
	for _, ev := range room.Game.DrainEvents() {
		payload := CREventPayload{Kind: ev.Kind, Message: ev.Message}
		if ev.Seat >= 0 && ev.Seat < len(room.Game.Players) {
			seat := ev.Seat
			payload.Seat = &seat
			payload.Name = room.Game.Players[seat].Name
		}
		h.broadcastEvent(room, payload)
	}
}

// ==================== 대기 상태 마감 타이머 (AFK 진행 보장) ====================

// scheduleDeadline 마감 타이머 예약. AfkSeq 를 올려 지나간 발화를 구분하고,
// 발화는 허브 채널을 경유하므로 허브 밖에서 상태를 만지지 않는다.
func (h *CRHub) scheduleDeadline(room *crRoom, dur time.Duration) {
	h.stopPhaseTimer(room)
	room.Game.AfkSeq++
	room.Game.Deadline = time.Now().Add(dur).UnixMilli()
	sig := crPhaseSignal{GameID: room.Game.ID, Seq: room.Game.AfkSeq}
	room.PhaseTimer = time.AfterFunc(dur, func() {
		h.phaseFired <- sig
	})
}

func (h *CRHub) stopPhaseTimer(room *crRoom) {
	if room.PhaseTimer != nil {
		room.PhaseTimer.Stop()
		room.PhaseTimer = nil
	}
}

// handlePhaseFired 마감 발화 — 단계별 자동 행동으로 해소한다.
//   - passing: 무작위 카드·무작위 대상·실물 그대로 선언
//   - deciding: 50/50 무작위 판정
func (h *CRHub) handlePhaseFired(sig crPhaseSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.Game.AfkSeq != sig.Seq {
		return
	}
	game := room.Game
	switch game.Phase {
	case CRPhasePassing:
		seat := game.PasserSeat
		p := game.Players[seat]
		if len(p.Hand) == 0 {
			return // 방어 — beginPassing 에서 이미 패배 처리됐어야 한다
		}
		h.broadcastEvent(room, CREventPayload{Kind: "afk", Seat: &seat, Name: p.Name,
			Message: fmt.Sprintf("%s님이 오래 응답하지 않아 자동으로 전달합니다", p.Name)})
		card := p.Hand[h.rng.Intn(len(p.Hand))]
		game.PassCard(seat, card, h.randomOtherSeat(game, seat), card) // 실물 그대로 선언
		log.Printf("[바퀴벌레][자동전달] game=%s | seat%d 무응답 — 무작위 카드·대상", game.ID, seat)

	case CRPhaseDeciding:
		seat := game.HolderSeat
		p := game.Players[seat]
		h.broadcastEvent(room, CREventPayload{Kind: "afk", Seat: &seat, Name: p.Name,
			Message: fmt.Sprintf("%s님이 오래 응답하지 않아 자동으로 판정합니다", p.Name)})
		game.Judge(seat, h.rng.Intn(2) == 0) // 50/50 무작위
		log.Printf("[바퀴벌레][자동판정] game=%s | seat%d 무응답 — 무작위 판정", game.ID, seat)

	default:
		return
	}
	h.afterProgress(room)
}

// randomOtherSeat seat 을 제외한 무작위 좌석 (자동 전달 대상)
func (h *CRHub) randomOtherSeat(game *CRGame, seat int) int {
	others := []int{}
	for _, p := range game.Players {
		if p.Seat != seat {
			others = append(others, p.Seat)
		}
	}
	return others[h.rng.Intn(len(others))]
}

// ==================== 상태 뷰 (은닉의 핵심) ====================

// buildCRState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를
// 쓴다. 은닉: yourHand 는 본인에게만 (타인·관전자는 필드 부재), 릴레이 중인
// 카드 실물(game.Card)은 어떤 스냅샷에도 싣지 않는다 — cr_peek 전용.
// 선언(claim)·체인·진열·손패 장수는 전원 공개다.
func (h *CRHub) buildCRState(room *crRoom, viewerSeat int) CRGameStatePayload {
	game := room.Game

	players := []CRPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		display := map[string]int{}
		for a, n := range p.Display {
			display[string(a)] = n
		}
		players = append(players, CRPlayerView{
			Seat:      p.Seat,
			Name:      p.Name,
			Connected: c != nil && c.Connected,
			Bot:       c != nil && c.Bot,
			HandCount: len(p.Hand),
			Display:   display,
		})
	}

	// 좌석 보유자만 yourHand 필드를 받는다 (빈 손패도 [] — nil 금지)
	var yourHand *[]string
	if viewerSeat >= 0 && viewerSeat < len(game.Players) {
		hand := []string{}
		for _, a := range game.Players[viewerSeat].Hand {
			hand = append(hand, string(a))
		}
		yourHand = &hand
	}

	endsAt := int64(0)
	if game.Phase == CRPhasePassing || game.Phase == CRPhaseDeciding {
		endsAt = game.Deadline
	}

	return CRGameStatePayload{
		GameID:     game.ID,
		RoomCode:   room.Code,
		Phase:      game.Phase,
		HostSeat:   h.hostSeat(room),
		YourSeat:   viewerSeat,
		Spectators: len(room.Spectators),
		EndsAt:     endsAt,
		PasserSeat: game.PasserSeat,
		HolderSeat: game.HolderSeat,
		Claim:      string(game.Claim),
		Chain:      append([]int{}, game.Chain...),
		YourHand:   yourHand,
		Players:    players,
		LoserSeat:  game.LoserSeat,
		LoseReason: game.LoseReason,
	}
}

// broadcastState 좌석마다 개인화 스냅샷을 만들어 보낸다.
// 관전자에게는 공개 정보만 담긴 스냅샷(viewerSeat -1)이 간다.
func (h *CRHub) broadcastState(room *crRoom) {
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, CRMessage{
			Type:    CRMsgGameState,
			Payload: h.buildCRState(room, seat),
		})
	}
	if len(room.Spectators) > 0 {
		msg := CRMessage{Type: CRMsgGameState, Payload: h.buildCRState(room, -1)}
		for c := range room.Spectators {
			h.sendToClient(c, msg)
		}
	}
}

func (h *CRHub) broadcastEvent(room *crRoom, event CREventPayload) {
	h.broadcastToRoom(room, CRMessage{Type: CRMsgEvent, Payload: event})
}

// finishGame 게임 종료 발표·전적 기록·방 정리 (재대결은 같은 방 코드
// 재입장으로 — finishGame 이 코드를 반납해 같은 코드로 새 방을 만들 수 있다)
func (h *CRHub) finishGame(room *crRoom) {
	game := room.Game
	h.stopPhaseTimer(room)
	game.Deadline = 0

	loser := game.LoserSeat
	loserName := ""
	if loser >= 0 && loser < len(game.Players) {
		loserName = game.Players[loser].Name
	}
	names := []string{}
	winners := []string{}
	for _, p := range game.Players {
		names = append(names, displayName(p.Name))
		if p.Seat != loser {
			winners = append(winners, displayName(p.Name))
		}
	}

	h.broadcastEvent(room, CREventPayload{Kind: "game_over", Seat: &loser,
		Name:    loserName,
		Message: fmt.Sprintf("%s님이 패배했습니다 — 나머지 전원 승리!", loserName)})
	// 최종 스냅샷을 먼저 보낸 뒤 종료를 알린다
	// (봇 러너는 cr_game_over 를 보고 스스로 끝난다)
	h.broadcastState(room)
	h.broadcastToRoom(room, CRMessage{
		Type: CRMsgGameOver,
		Payload: CRGameOverPayload{
			LoserSeat: loser,
			LoserName: loserName,
			Reason:    game.LoseReason,
			Players:   h.buildCRState(room, -1).Players,
		},
	})

	log.Printf("[바퀴벌레][경기결과] game=%s | 패자=seat%d(%s) | 사유=%s | 소요=%s",
		game.ID, loser, displayName(loserName), game.LoseReason, matchDuration(game.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "cockroach",
		Players:  strings.Join(names, " vs "),
		Winner:   strings.Join(winners, ", "),
		Reason:   game.LoseReason,
		Duration: matchSeconds(game.StartedAt),
		Bot:      crRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
	if room.Code != "" {
		delete(h.activeCodes, room.Code) // 코드 재사용 복귀
	}
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *CRHub) handleDisconnect(client *CRClient) {
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
	log.Printf("[바퀴벌레][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, CRMessage{
		Type: CRMsgPlayerDisconnected,
		Payload: CRPlayerDisconnectedPayload{
			Seat:         client.Seat,
			Name:         client.Name,
			GraceSeconds: int(h.grace.Seconds()),
		},
	})
	h.broadcastState(room)

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 안에 재접속하지 않은 좌석은 연습봇으로 대체하고
// 게임은 계속한다 — 전달·판정이 이탈 좌석에 막히지 않는 근거
func (h *CRHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == CRPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[바퀴벌레][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := h.takeoverCRBot(room, seat, client.Name)
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.broadcastEvent(room, CREventPayload{Kind: "bot_takeover", Seat: &seat,
		Name:    client.Name,
		Message: fmt.Sprintf("%s님 자리를 봇이 이어받았습니다", client.Name)})
	// 봇이 결정권자로 넘겨받았다면 실물을 다시 보내야 릴레이를 결정할 수 있다
	h.sendPeek(room, seat)
	// 새 봇이 이 스냅샷을 받아 자기 차례면 즉시 이어간다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *CRHub) handleRejoin(client *CRClient, msg CRMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload CRRejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, CRMessage{Type: CRMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, CRMessage{Type: CRMsgSessionExpired})
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

	log.Printf("[바퀴벌레][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, CRMessage{
		Type:    CRMsgPlayerReconnected,
		Payload: CRPlayerReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	// 결정권자로 복귀했다면 실물(cr_peek)을 다시 보내 릴레이 결정을 복원한다
	h.sendPeek(room, client.Seat)
	// 전원에게 접속 상태가 반영된 스냅샷 (재접속 당사자 복원 포함)
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *CRHub) clearGameSessions(room *crRoom) {
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		h.drop(c.SessionID)
	}
}

// ==================== 전송 ====================

func (h *CRHub) sendError(client *CRClient, message string) {
	h.sendToClient(client, CRMessage{Type: CRMsgError, Payload: CRErrorPayload{Message: message}})
}

func (h *CRHub) sendToClient(client *CRClient, message CRMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[CR] ")
}

func (h *CRHub) broadcastToRoom(room *crRoom, message CRMessage) {
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

func ServeCRWs(hub *CRHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[CR] Error upgrading connection:", err)
		return
	}

	client := &CRClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[CR] ",
		func(msg CRMessage) { hub.gameMessage <- CRGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
