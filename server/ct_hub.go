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

// ==================== 시타델 허브 ====================
//
// 다인 결(sl_hub / kr_hub)을 그대로 복제한다 — 공용 로비 + 사설 방 코드 +
// 관전 + 리액션 + 재접속 유예 + 봇 대체. 시타델만의 차이는 대기 상태가
// 넷이라는 점이다 (직업 선택 45초 · 차례 60초 · 카드 고르기 60초 ·
// 직업 능력 30초). 각 단계의 마감은 ct_game.go 의 Force* 로 해소된다.

// 시타델 대기 상태 마감 타이머 (테스트에서 짧게 낮춘다)
var (
	ctPickTimeout    = 45 * time.Second
	ctTurnTimeout    = 60 * time.Second
	ctAbilityTimeout = 30 * time.Second
)

// ctRoom 게임(순수 상태)과 좌석별 연결의 매핑
type ctRoom struct {
	Game       *CTGame
	Clients    map[int]*CTClient // seat → client
	PhaseTimer *time.Timer       // 대기 상태 마감 타이머

	// DeadlineSeq 마지막으로 마감을 건 StateSeq — 같은 대기 상태에 스냅샷이
	// 쌓일 때마다(관전 입장·접속 변화 등) 마감이 늘어나지 않게 하는 근거
	DeadlineSeq int

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string

	// Spectators 관전자 연결 (사설 방 코드로만 진입, 상한 maxSpectators).
	// 좌석·세션이 없어 재접속을 지원하지 않는다 — 끊기면 목록에서만 뗀다.
	Spectators map[*CTClient]bool

	// LastReact 좌석별 마지막 리액션 발신 시각 (레이트리밋 장부)
	LastReact map[int]time.Time
}

// ctPhaseSignal 마감 타이머의 발화 표식 — 대기 상태 일련번호(AfkSeq)로
// 지나간 발화를 구분한다
type ctPhaseSignal struct {
	GameID string
	Seq    int
}

type CTHub struct {
	clients map[*CTClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*ctRoom

	// 글로벌 로비. 시작 전 공용 방은 하나뿐이다.
	lobby *ctRoom

	// 사설 방 (초대 코드 → 시작 전 방). 허브 고루틴에서만 접근한다.
	privateLobbies map[string]*ctRoom

	// 진행 중 사설 방 (초대 코드 → gameID). 시작 후에도 코드로 방을 찾아
	// 관전 입장시키는 근거다. finishGame 에서 해제한다 (코드 재사용 복귀).
	activeCodes map[string]string

	register    chan *CTClient
	unregister  chan *CTClient
	gameMessage chan CTGameMessage
	phaseFired  chan ctPhaseSignal

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*CTClient]

	// 덱 셔플·자동 진행용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type CTGameMessage struct {
	Client  *CTClient
	Message CTMessage
}

func NewCTHub() *CTHub {
	return &CTHub{
		register:       make(chan *CTClient),
		unregister:     make(chan *CTClient),
		clients:        make(map[*CTClient]bool),
		rooms:          make(map[string]*ctRoom),
		privateLobbies: make(map[string]*ctRoom),
		activeCodes:    make(map[string]string),
		gameMessage:    make(chan CTGameMessage),
		phaseFired:     make(chan ctPhaseSignal, 8),
		sessionManager: newSessionManager[*CTClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *CTHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[CT] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[CT] Client unregistered: %s", client.ID)
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

func (h *CTHub) handleGameMessage(gm CTGameMessage) {
	// 관전자는 어떤 행동도 할 수 없다 (보기 전용 — 리액션도 좌석 보유자만)
	if h.isSpectator(gm.Client) {
		h.sendError(gm.Client, spectatorDeniedMsg)
		return
	}
	switch gm.Message.Type {
	case CTMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case CTMsgReact:
		h.handleReact(gm.Client, gm.Message)
	case CTMsgFillBots:
		h.handleFillBots(gm.Client)
	case CTMsgStart:
		h.handleStart(gm.Client)
	case CTMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case CTMsgPickRole:
		h.handlePickRole(gm.Client, gm.Message)
	case CTMsgGather:
		h.handleGather(gm.Client, gm.Message)
	case CTMsgKeep:
		h.handleKeep(gm.Client, gm.Message)
	case CTMsgBuild:
		h.handleBuild(gm.Client, gm.Message)
	case CTMsgAbility:
		h.handleAbility(gm.Client, gm.Message)
	case CTMsgEndTurn:
		h.handleEndTurn(gm.Client)
	}
}

// ==================== 대기실 ====================

func (h *CTHub) handleJoinGame(client *CTClient, msg CTMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload CTJoinGamePayload
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

	log.Printf("[시타델][입장] game=%s | seat%d=%s 대기 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), CTMaxPlayers)
	if room.Code == "" { // 사설 방 입장은 조용히 (공용 로비만 알림)
		notify("시타델 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), len(room.Game.Players), CTMaxPlayers))
	}

	h.sendToClient(client, CTMessage{
		Type: CTMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
			"roomCode":  room.Code,
		},
	})

	h.broadcastEvent(room, CTEventPayload{Kind: "joined", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 입장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	// 정원이 차도 자동 시작하지 않는다 — 호스트 명시 시작만
	h.broadcastState(room)
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다.
// 생략/빈 문자열은 공용 로비, "NEW"는 새 코드 발급, 그 외 코드는 해당 사설 방
// (없으면 그 코드로 관대하게 새로 생성).
func (h *CTHub) lobbyRoomFor(roomField string) *ctRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewCTGame(uuid.New().String())
			h.lobby = &ctRoom{Game: game, Clients: map[int]*CTClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[CT] Created lobby game %s", game.ID)
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
		game := NewCTGame(uuid.New().String())
		room = &ctRoom{Game: game, Clients: map[int]*CTClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[CT] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// ==================== 관전 / 리액션 ====================

// addSpectator 진행 중(또는 가득 찬) 사설 방에 관전자로 등록한다.
// 좌석·세션 없음 — 재접속 미지원, 끊기면 다시 코드로 들어온다.
func (h *CTHub) addSpectator(room *ctRoom, client *CTClient, name string) {
	if len(room.Spectators) >= maxSpectators {
		h.sendError(client, spectatorFullMsg)
		return
	}
	if room.Spectators == nil {
		room.Spectators = map[*CTClient]bool{}
	}
	client.Name = name
	client.GameID = room.Game.ID
	room.Spectators[client] = true

	log.Printf("[시타델][관전] game=%s | %s 관전 입장 (%d명)",
		room.Game.ID, displayName(name), len(room.Spectators))

	h.sendToClient(client, CTMessage{
		Type:    CTMsgSpectateJoined,
		Payload: map[string]interface{}{"gameId": room.Game.ID, "roomCode": room.Code},
	})
	h.broadcastState(room)
}

// isSpectator 관전자 연결인지 (좌석 보유자·미입장 연결은 false)
func (h *CTHub) isSpectator(client *CTClient) bool {
	if client.GameID == "" {
		return false
	}
	room := h.rooms[client.GameID]
	return room != nil && room.Spectators[client]
}

// handleReact 리액션 이모지 — 좌석 보유자만, waiting 중에도 허용.
// 화이트리스트 외·레이트리밋 초과는 조용히 무시한다 (상태 저장 없음).
func (h *CTHub) handleReact(client *CTClient, msg CTMessage) {
	room := h.rooms[client.GameID]
	if room == nil || client.Seat < 0 {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload CTReactPayload
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
	h.broadcastEvent(room, CTEventPayload{Kind: "react", Seat: &seat, Name: client.Name,
		Message: payload.Emoji})
}

// waitingRoomOf 클라이언트가 속한 시작 전 방 (공용 로비 또는 사설 방)
func (h *CTHub) waitingRoomOf(client *CTClient) *ctRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Ready {
		return nil
	}
	return room
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *CTHub) hostSeat(room *ctRoom) int {
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

// ctHumanCount 방의 사람 수
func ctHumanCount(room *ctRoom) int {
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
func (h *CTHub) updateLobbyWaiting(room *ctRoom) {
	if room.Code != "" {
		return
	}
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && ctHumanCount(room) >= 1
	lobbySetWaiting("citadels", waiting)
}

// handleFillBots host 가 빈 좌석을 연습봇으로 4인까지 채운 뒤 즉시 시작한다
// (스폰은 join 을 거치지 않으므로 명시 시작 호출).
func (h *CTHub) handleFillBots(client *CTClient) {
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
	for len(room.Game.Players) < CTFillBotTarget {
		botNo++
		if !h.spawnCTBot(room, fmt.Sprintf("%s%d", botName, botNo)) {
			break
		}
	}

	if room.Game.CanStart() {
		h.startGame(room)
		return
	}
	h.broadcastState(room)
}

func (h *CTHub) handleStart(client *CTClient) {
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
		h.sendError(client, fmt.Sprintf("%d명 이상 모여야 시작할 수 있습니다", CTMinPlayers))
		return
	}
	h.startGame(room)
}

func (h *CTHub) startGame(room *ctRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	if room.Code != "" {
		// 시작한 사설 방의 코드는 진행 중 장부로 옮긴다 (관전 입장 근거)
		delete(h.privateLobbies, room.Code)
		h.activeCodes[room.Code] = room.Game.ID
	} else {
		h.lobby = nil
		lobbySetWaiting("citadels", false)
	}

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	n := len(room.Game.Players)
	log.Printf("[시타델][경기시작] game=%s | 인원=%d | 앞면 제외 %d장 + 뒷면 1장 | 왕관=seat%d | %v",
		room.Game.ID, n, ctFaceUpCount(n), room.Game.CrownSeat, names)
	if !ctRoomHasBot(room) { // 연습봇전은 운영자 알림을 억제한다
		notify("시타델 시작", fmt.Sprintf("%d인전 시작", n))
	}

	h.broadcastEvent(room, CTEventPayload{Kind: "game_started",
		Message: fmt.Sprintf(
			"게임 시작 — %d인전, 각자 건물 카드 %d장과 금화 %d개로 시작합니다. 건물 %d채를 먼저 완성하면 그 라운드까지만 진행합니다",
			n, CTHandStart, CTGoldStart, CTBuildTarget)})
	h.afterProgress(room)
}

// removeFromLobby 대기실에서 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *CTHub) removeFromLobby(room *ctRoom, client *CTClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	rebuilt := map[int]*CTClient{}
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

	log.Printf("[시타델][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), CTMaxPlayers)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if ctHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, CTMessage{Type: CTMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			lobbySetWaiting("citadels", false)
		}
		return
	}

	h.broadcastEvent(room, CTEventPayload{Kind: "left", Name: client.Name,
		Message: fmt.Sprintf("%s님이 퇴장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *CTHub) roomOf(client *CTClient) *ctRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

// handlePickRole 직업 선택 (어떤 직업을 골랐는지는 로그에도 남기지 않는다)
func (h *CTHub) handlePickRole(client *CTClient, msg CTMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload CTPickRolePayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.PickRole(client.Seat, payload.Role); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[시타델][직업선택] game=%s | %d라운드 seat%d=%s 선택 완료 (남은 후보 %d장)",
		room.Game.ID, room.Game.Round, client.Seat, displayName(client.Name),
		len(room.Game.RolePool))
	h.afterProgress(room)
}

// handleGather 자원 — 금화 2 또는 건물 카드 2장
func (h *CTHub) handleGather(client *CTClient, msg CTMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload CTGatherPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.Gather(client.Seat, payload.Kind); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.afterProgress(room)
}

// handleKeep 뽑은 카드 중 남길 것 고르기 (내용은 로그에 남기지 않는다)
func (h *CTHub) handleKeep(client *CTClient, msg CTMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload CTKeepPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.Keep(client.Seat, payload.Index); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.afterProgress(room)
}

// handleBuild 건설
func (h *CTHub) handleBuild(client *CTClient, msg CTMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload CTBuildPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.Build(client.Seat, payload.CardID); err != nil {
		h.sendError(client, err.Error())
		return
	}
	built, gold := 0, 0
	if client.Seat >= 0 && client.Seat < len(room.Game.Players) {
		p := room.Game.Players[client.Seat]
		built, gold = len(p.Built), p.Gold
	}
	log.Printf("[시타델][건설] game=%s | %d라운드 seat%d=%s cardId=%d → %d채 (금화 %d)",
		room.Game.ID, room.Game.Round, client.Seat, displayName(client.Name),
		payload.CardID, built, gold)
	h.afterProgress(room)
}

// handleAbility 직업 능력
func (h *CTHub) handleAbility(client *CTClient, msg CTMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload CTAbilityPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.Ability(client.Seat, payload); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.afterProgress(room)
}

// handleEndTurn 차례 마무리 (능력이 남았으면 능력 단계로 넘어간다)
func (h *CTHub) handleEndTurn(client *CTClient) {
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
func (h *CTHub) afterProgress(room *ctRoom) {
	h.drainEvents(room)
	if room.Game.Phase == CTPhaseGameOver {
		h.finishGame(room)
		return
	}
	h.syncDeadline(room)
	h.broadcastState(room)
}

// drainEvents 순수 규칙이 쌓은 이벤트를 ct_event 로 방송한다
func (h *CTHub) drainEvents(room *ctRoom) {
	for _, ev := range room.Game.DrainEvents() {
		payload := CTEventPayload{Kind: ev.Kind, Message: ev.Message}
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
func (h *CTHub) syncDeadline(room *ctRoom) {
	game := room.Game
	if room.DeadlineSeq == game.StateSeq {
		return
	}
	room.DeadlineSeq = game.StateSeq
	var dur time.Duration
	switch game.Phase {
	case CTPhasePickRoles:
		dur = ctPickTimeout
	case CTPhaseTurn, CTPhaseKeepCard:
		dur = ctTurnTimeout
	case CTPhaseAbility:
		dur = ctAbilityTimeout
	default:
		h.stopPhaseTimer(room)
		return
	}
	h.scheduleDeadline(room, dur)
}

// scheduleDeadline 마감 타이머 예약. AfkSeq 를 올려 지나간 발화를 구분하고,
// 발화는 허브 채널을 경유하므로 허브 밖에서 상태를 만지지 않는다.
func (h *CTHub) scheduleDeadline(room *ctRoom, dur time.Duration) {
	h.stopPhaseTimer(room)
	room.Game.AfkSeq++
	room.Game.Deadline = time.Now().Add(dur).UnixMilli()
	sig := ctPhaseSignal{GameID: room.Game.ID, Seq: room.Game.AfkSeq}
	room.PhaseTimer = time.AfterFunc(dur, func() {
		h.phaseFired <- sig
	})
}

func (h *CTHub) stopPhaseTimer(room *ctRoom) {
	if room.PhaseTimer != nil {
		room.PhaseTimer.Stop()
		room.PhaseTimer = nil
	}
}

// handlePhaseFired 마감 발화 — 단계별 자동 행동으로 해소한다.
//   - pick_roles: 남은 후보에서 무작위로 집는다
//   - turn:       금화 2를 받고 차례를 끝낸다
//   - keep_card:  첫 장을 남기고 차례를 끝낸다
//   - ability:    능력을 쓰지 않고 차례를 끝낸다
func (h *CTHub) handlePhaseFired(sig ctPhaseSignal) {
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
	case CTPhasePickRoles:
		h.broadcastEvent(room, CTEventPayload{Kind: "afk", Seat: &seat, Name: actor.Name,
			Message: fmt.Sprintf("%s님이 오래 응답하지 않아 직업을 자동으로 고릅니다", actor.Name)})
		game.ForcePick()
		log.Printf("[시타델][자동진행] game=%s | seat%d 무응답 — 직업 자동 선택", game.ID, seat)

	case CTPhaseTurn:
		h.broadcastEvent(room, CTEventPayload{Kind: "afk", Seat: &seat, Name: actor.Name,
			Message: fmt.Sprintf("%s님이 오래 응답하지 않아 자동으로 금화를 받고 차례를 넘깁니다", actor.Name)})
		game.ForceTurn()
		log.Printf("[시타델][자동진행] game=%s | seat%d 무응답 — 자동 차례 종료", game.ID, seat)

	case CTPhaseKeepCard:
		h.broadcastEvent(room, CTEventPayload{Kind: "afk", Seat: &seat, Name: actor.Name,
			Message: fmt.Sprintf("%s님이 오래 응답하지 않아 건물 카드를 자동으로 남깁니다", actor.Name)})
		game.ForceKeep()
		log.Printf("[시타델][자동진행] game=%s | seat%d 무응답 — 카드 자동 선택", game.ID, seat)

	case CTPhaseAbility:
		h.broadcastEvent(room, CTEventPayload{Kind: "afk", Seat: &seat, Name: actor.Name,
			Message: fmt.Sprintf("%s님이 오래 응답하지 않아 직업 능력을 쓰지 않습니다", actor.Name)})
		game.ForceAbility()
		log.Printf("[시타델][자동진행] game=%s | seat%d 무응답 — 능력 생략", game.ID, seat)

	default:
		return
	}
	h.afterProgress(room)
}

// ==================== 상태 뷰 (은닉의 핵심) ====================

// buildCTState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
//
// 은닉 계약:
//   - yourRole·yourHand 는 본인에게만 (타인·관전자 raw JSON 에 키 부재)
//   - yourDraw 는 keep_card 단계의 본인에게만
//   - pickPool 은 지금 고르는 좌석에게만 (남은 후보를 남이 알면 추리가 무너진다)
//   - 남의 직업은 호출로 공개되기 전까지 roleRevealed 0
//   - 뒷면으로 제외된 직업(FaceDown)은 어떤 필드에도 실리지 않는다
//
// 빈 슬라이스도 [] 로 나가야 하므로 슬라이스 포인터로 부재를 표현한다.
// viewerSeat -1(관전자)과 좌석이 없는 방에서도 패닉하지 않는다.
func (h *CTHub) buildCTState(room *ctRoom, viewerSeat int) CTGameStatePayload {
	game := room.Game
	seated := viewerSeat >= 0 && viewerSeat < len(game.Players)

	var yourRole *int
	var yourHand *[]CTCard
	var yourDraw *[]CTCard
	var pickPool *[]int

	if seated && game.Ready {
		me := game.Players[viewerSeat]
		role := me.Role
		yourRole = &role
		hand := append([]CTCard{}, me.Hand...)
		yourHand = &hand
		if game.Phase == CTPhaseKeepCard && game.CurrentSeat == viewerSeat {
			draw := append([]CTCard{}, me.Draw...)
			yourDraw = &draw
		}
		if game.Phase == CTPhasePickRoles && game.CurrentSeat == viewerSeat {
			pool := append([]int{}, game.RolePool...)
			pickPool = &pool
		}
	}

	players := []CTPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		score, _ := ctScore(p, game.FirstCompleteSeat)
		players = append(players, CTPlayerView{
			Seat:         p.Seat,
			Name:         p.Name,
			Connected:    c != nil && c.Connected,
			Bot:          c != nil && c.Bot,
			Gold:         p.Gold,
			HandCount:    len(p.Hand),
			Built:        append([]CTCard{}, p.Built...),
			Score:        score,
			RoleRevealed: p.RoleRevealed,
			Killed:       p.Killed,
			Robbed:       p.Robbed,
		})
	}

	endsAt := int64(0)
	switch game.Phase {
	case CTPhasePickRoles, CTPhaseTurn, CTPhaseKeepCard, CTPhaseAbility:
		endsAt = game.Deadline
	}

	return CTGameStatePayload{
		GameID:        game.ID,
		RoomCode:      room.Code,
		Phase:         game.Phase,
		HostSeat:      h.hostSeat(room),
		YourSeat:      viewerSeat,
		Spectators:    len(room.Spectators),
		EndsAt:        endsAt,
		Round:         game.Round,
		LastRound:     game.LastRound,
		CrownSeat:     game.CrownSeat,
		CallingRole:   game.CallingRole,
		CurrentSeat:   game.CurrentSeat,
		FaceUpRemoved: append([]int{}, game.FaceUp...),
		PickPool:      pickPool,
		YourRole:      yourRole,
		YourHand:      yourHand,
		YourDraw:      yourDraw,
		Players:       players,
		LastAction:    game.LastAction,
		Result:        game.Result,
	}
}

// broadcastState 좌석마다 개인화 스냅샷을 만들어 보낸다.
// 관전자에게는 공개 정보만 담긴 스냅샷(viewerSeat -1)이 간다.
func (h *CTHub) broadcastState(room *ctRoom) {
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, CTMessage{
			Type:    CTMsgGameState,
			Payload: h.buildCTState(room, seat),
		})
	}
	if len(room.Spectators) > 0 {
		msg := CTMessage{Type: CTMsgGameState, Payload: h.buildCTState(room, -1)}
		for c := range room.Spectators {
			h.sendToClient(c, msg)
		}
	}
}

func (h *CTHub) broadcastEvent(room *ctRoom, event CTEventPayload) {
	h.broadcastToRoom(room, CTMessage{Type: CTMsgEvent, Payload: event})
}

// finishGame 게임 종료 발표·전적 기록·방 정리 (재대결은 같은 방 코드
// 재입장으로 — finishGame 이 코드를 반납해 같은 코드로 새 방을 만들 수 있다)
func (h *CTHub) finishGame(room *ctRoom) {
	game := room.Game
	h.stopPhaseTimer(room)
	game.Deadline = 0

	result := game.Result
	if result == nil { // 방어선 — 종료는 항상 결과와 함께 온다
		result = &CTResult{WinnerSeats: []int{}, WinnerNames: []string{},
			Rows: []CTResultRow{}, Message: "게임이 종료됐습니다"}
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

	h.broadcastEvent(room, CTEventPayload{Kind: "game_over", Message: result.Message})
	// 최종 스냅샷을 먼저 보낸 뒤 종료를 알린다
	// (봇 러너는 ct_game_over 를 보고 스스로 끝난다)
	h.broadcastState(room)
	h.broadcastToRoom(room, CTMessage{
		Type: CTMsgGameOver,
		Payload: CTGameOverPayload{
			WinnerSeats: append([]int{}, result.WinnerSeats...),
			WinnerNames: append([]string{}, result.WinnerNames...),
			Message:     result.Message,
			Rounds:      game.Round,
			Rows:        append([]CTResultRow{}, result.Rows...),
			Players:     h.buildCTState(room, -1).Players,
		},
	})

	scores := []string{}
	for i, p := range game.Players {
		score := 0
		if i < len(result.Rows) {
			score = result.Rows[i].Score
		}
		scores = append(scores, fmt.Sprintf("%s %d점(건물 %d채)",
			displayName(p.Name), score, len(p.Built)))
	}
	log.Printf("[시타델][경기결과] game=%s | 승자=%s | 라운드=%d | 소요=%s | %s",
		game.ID, strings.Join(winners, "·"), game.Round,
		matchDuration(game.StartedAt), strings.Join(scores, " / "))

	RecordMatch(MatchRecord{
		Game:     "citadels",
		Players:  strings.Join(winners, "·") + " vs " + strings.Join(losers, "·"),
		Winner:   strings.Join(winners, "·"),
		Reason:   "score",
		Duration: matchSeconds(game.StartedAt),
		Bot:      ctRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
	if room.Code != "" {
		delete(h.activeCodes, room.Code) // 코드 재사용 복귀
	}
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *CTHub) handleDisconnect(client *CTClient) {
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
	log.Printf("[시타델][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, CTMessage{
		Type: CTMsgPlayerDisconnected,
		Payload: CTPlayerDisconnectedPayload{
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
func (h *CTHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == CTPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[시타델][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := h.takeoverCTBot(room, seat, client.Name)
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.broadcastEvent(room, CTEventPayload{Kind: "bot_takeover", Seat: &seat,
		Name:    client.Name,
		Message: fmt.Sprintf("%s님 자리를 봇이 이어받았습니다", client.Name)})
	// 새 봇이 이 스냅샷을 받아 자기 차례면 즉시 이어간다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *CTHub) handleRejoin(client *CTClient, msg CTMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload CTRejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, CTMessage{Type: CTMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, CTMessage{Type: CTMsgSessionExpired})
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

	log.Printf("[시타델][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, CTMessage{
		Type:    CTMsgPlayerReconnected,
		Payload: CTPlayerReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *CTHub) clearGameSessions(room *ctRoom) {
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		h.drop(c.SessionID)
	}
}

// ==================== 전송 ====================

func (h *CTHub) sendError(client *CTClient, message string) {
	h.sendToClient(client, CTMessage{Type: CTMsgError, Payload: CTErrorPayload{Message: message}})
}

func (h *CTHub) sendToClient(client *CTClient, message CTMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[CT] ")
}

func (h *CTHub) broadcastToRoom(room *ctRoom, message CTMessage) {
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

func ServeCTWs(hub *CTHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[CT] Error upgrading connection:", err)
		return
	}

	client := &CTClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[CT] ",
		func(msg CTMessage) { hub.gameMessage <- CTGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
