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

// 루미큐브 차례 마감 타이머 — 90초 무응답은 자동으로 타일 1개를 가져가고
// 차례를 끝낸다 (재배치가 오래 걸리는 게임이라 다른 게임보다 길다).
// 테스트에서 짧게 낮춘다.
var ruTurnTimeout = 90 * time.Second

// ruRoom 게임(순수 상태)과 좌석별 연결의 매핑
type ruRoom struct {
	Game       *RUGame
	Clients    map[int]*RUClient // seat → client
	PhaseTimer *time.Timer       // 차례 마감 타이머

	// DeadlineSeq 마지막으로 마감을 건 StateSeq — 같은 차례에 스냅샷이
	// 쌓일 때마다(관전 입장·접속 변화 등) 마감이 늘어나지 않게 하는 근거
	DeadlineSeq int

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string

	// Spectators 관전자 연결 (사설 방 코드로만 진입, 상한 maxSpectators).
	// 좌석·세션이 없어 재접속을 지원하지 않는다 — 끊기면 목록에서만 뗀다.
	Spectators map[*RUClient]bool

	// LastReact 좌석별 마지막 리액션 발신 시각 (레이트리밋 장부)
	LastReact map[int]time.Time
}

// ruPhaseSignal 마감 타이머의 발화 표식 — 대기 상태 일련번호(AfkSeq)로
// 지나간 발화를 구분한다
type ruPhaseSignal struct {
	GameID string
	Seq    int
}

type RUHub struct {
	clients map[*RUClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*ruRoom

	// 글로벌 로비. 시작 전 공용 방은 하나뿐이다.
	lobby *ruRoom

	// 사설 방 (초대 코드 → 시작 전 방). 허브 고루틴에서만 접근한다.
	privateLobbies map[string]*ruRoom

	// 진행 중 사설 방 (초대 코드 → gameID). 시작 후에도 코드로 방을 찾아
	// 관전 입장시키는 근거다. finishGame 에서 해제한다 (코드 재사용 복귀).
	activeCodes map[string]string

	register    chan *RUClient
	unregister  chan *RUClient
	gameMessage chan RUGameMessage
	phaseFired  chan ruPhaseSignal

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*RUClient]

	// 타일 셔플·자동 진행용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type RUGameMessage struct {
	Client  *RUClient
	Message RUMessage
}

func NewRUHub() *RUHub {
	return &RUHub{
		register:       make(chan *RUClient),
		unregister:     make(chan *RUClient),
		clients:        make(map[*RUClient]bool),
		rooms:          make(map[string]*ruRoom),
		privateLobbies: make(map[string]*ruRoom),
		activeCodes:    make(map[string]string),
		gameMessage:    make(chan RUGameMessage),
		phaseFired:     make(chan ruPhaseSignal, 8),
		sessionManager: newSessionManager[*RUClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *RUHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[RU] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[RU] Client unregistered: %s", client.ID)
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

func (h *RUHub) handleGameMessage(gm RUGameMessage) {
	// 관전자는 어떤 행동도 할 수 없다 (보기 전용 — 리액션도 좌석 보유자만)
	if h.isSpectator(gm.Client) {
		h.sendError(gm.Client, spectatorDeniedMsg)
		return
	}
	switch gm.Message.Type {
	case RUMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case RUMsgReact:
		h.handleReact(gm.Client, gm.Message)
	case RUMsgFillBots:
		h.handleFillBots(gm.Client)
	case RUMsgStart:
		h.handleStart(gm.Client)
	case RUMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case RUMsgCommit:
		h.handleCommit(gm.Client, gm.Message)
	case RUMsgDraw:
		h.handleDraw(gm.Client)
	}
}

// ==================== 대기실 ====================

func (h *RUHub) handleJoinGame(client *RUClient, msg RUMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload RUJoinGamePayload
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

	log.Printf("[루미큐브][입장] game=%s | seat%d=%s 대기 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), RUMaxPlayers)
	if room.Code == "" { // 사설 방 입장은 조용히 (공용 로비만 알림)
		notify("루미큐브 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), len(room.Game.Players), RUMaxPlayers))
	}

	h.sendToClient(client, RUMessage{
		Type: RUMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
			"roomCode":  room.Code,
		},
	})

	h.broadcastEvent(room, RUEventPayload{Kind: "joined", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 입장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	// 정원이 차도 자동 시작하지 않는다 — 호스트 명시 시작만
	h.broadcastState(room)
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다.
// 생략/빈 문자열은 공용 로비, "NEW"는 새 코드 발급, 그 외 코드는 해당 사설 방
// (없으면 그 코드로 관대하게 새로 생성).
func (h *RUHub) lobbyRoomFor(roomField string) *ruRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewRUGame(uuid.New().String())
			h.lobby = &ruRoom{Game: game, Clients: map[int]*RUClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[RU] Created lobby game %s", game.ID)
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
		game := NewRUGame(uuid.New().String())
		room = &ruRoom{Game: game, Clients: map[int]*RUClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[RU] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// ==================== 관전 / 리액션 ====================

// addSpectator 진행 중(또는 가득 찬) 사설 방에 관전자로 등록한다.
// 좌석·세션 없음 — 재접속 미지원, 끊기면 다시 코드로 들어온다.
func (h *RUHub) addSpectator(room *ruRoom, client *RUClient, name string) {
	if len(room.Spectators) >= maxSpectators {
		h.sendError(client, spectatorFullMsg)
		return
	}
	if room.Spectators == nil {
		room.Spectators = map[*RUClient]bool{}
	}
	client.Name = name
	client.GameID = room.Game.ID
	room.Spectators[client] = true

	log.Printf("[루미큐브][관전] game=%s | %s 관전 입장 (%d명)",
		room.Game.ID, displayName(name), len(room.Spectators))

	h.sendToClient(client, RUMessage{
		Type:    RUMsgSpectateJoined,
		Payload: map[string]interface{}{"gameId": room.Game.ID, "roomCode": room.Code},
	})
	h.broadcastState(room)
}

// isSpectator 관전자 연결인지 (좌석 보유자·미입장 연결은 false)
func (h *RUHub) isSpectator(client *RUClient) bool {
	if client.GameID == "" {
		return false
	}
	room := h.rooms[client.GameID]
	return room != nil && room.Spectators[client]
}

// handleReact 리액션 이모지 — 좌석 보유자만, waiting 중에도 허용.
// 화이트리스트 외·레이트리밋 초과는 조용히 무시한다 (상태 저장 없음).
func (h *RUHub) handleReact(client *RUClient, msg RUMessage) {
	room := h.rooms[client.GameID]
	if room == nil || client.Seat < 0 {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload RUReactPayload
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
	h.broadcastEvent(room, RUEventPayload{Kind: "react", Seat: &seat, Name: client.Name,
		Message: payload.Emoji})
}

// waitingRoomOf 클라이언트가 속한 시작 전 방 (공용 로비 또는 사설 방)
func (h *RUHub) waitingRoomOf(client *RUClient) *ruRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Ready {
		return nil
	}
	return room
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *RUHub) hostSeat(room *ruRoom) int {
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

// ruHumanCount 방의 사람 수
func ruHumanCount(room *ruRoom) int {
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
func (h *RUHub) updateLobbyWaiting(room *ruRoom) {
	if room.Code != "" {
		return
	}
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && ruHumanCount(room) >= 1
	lobbySetWaiting("rummikub", waiting)
}

// handleFillBots host 가 빈 좌석을 연습봇으로 3인까지 채운 뒤 즉시 시작한다
// (스폰은 join 을 거치지 않으므로 명시 시작 호출).
func (h *RUHub) handleFillBots(client *RUClient) {
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
	for len(room.Game.Players) < RUFillBotTarget {
		botNo++
		if !h.spawnRUBot(room, fmt.Sprintf("%s%d", botName, botNo)) {
			break
		}
	}

	if room.Game.CanStart() {
		h.startGame(room)
		return
	}
	h.broadcastState(room)
}

func (h *RUHub) handleStart(client *RUClient) {
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
		h.sendError(client, fmt.Sprintf("%d명 이상 모여야 시작할 수 있습니다", RUMinPlayers))
		return
	}
	h.startGame(room)
}

func (h *RUHub) startGame(room *ruRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	if room.Code != "" {
		// 시작한 사설 방의 코드는 진행 중 장부로 옮긴다 (관전 입장 근거)
		delete(h.privateLobbies, room.Code)
		h.activeCodes[room.Code] = room.Game.ID
	} else {
		h.lobby = nil
		lobbySetWaiting("rummikub", false)
	}

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	n := len(room.Game.Players)
	log.Printf("[루미큐브][경기시작] game=%s | 인원=%d | 타일더미=%d개 | 각자 %d개 | 선=seat%d | %v",
		room.Game.ID, n, len(room.Game.Pool), RUStartRack, room.Game.CurrentSeat, names)
	if !ruRoomHasBot(room) { // 연습봇전은 운영자 알림을 억제한다
		notify("루미큐브 시작", fmt.Sprintf("%d인전 시작", n))
	}

	h.afterProgress(room)
}

// removeFromLobby 대기실에서 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *RUHub) removeFromLobby(room *ruRoom, client *RUClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	rebuilt := map[int]*RUClient{}
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

	log.Printf("[루미큐브][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), RUMaxPlayers)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if ruHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, RUMessage{Type: RUMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			lobbySetWaiting("rummikub", false)
		}
		return
	}

	h.broadcastEvent(room, RUEventPayload{Kind: "left", Name: client.Name,
		Message: fmt.Sprintf("%s님이 퇴장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *RUHub) roomOf(client *RUClient) *ruRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

// handleCommit 차례 확정 — 테이블 전체 배치를 통째로 받는다.
// 검사에 하나라도 걸리면 판은 그대로고 오류만 돌아간다 (부분 적용 없음).
func (h *RUHub) handleCommit(client *RUClient, msg RUMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload RUCommitPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.Commit(client.Seat, payload.Sets); err != nil {
		log.Printf("[루미큐브][확정거부] game=%s | seat%d=%s | %s",
			room.Game.ID, client.Seat, displayName(client.Name), err.Error())
		h.sendError(client, err.Error())
		// 거부된 차례는 차례 시작 상태 그대로다 — 클라이언트가 확실히
		// 원래 배치로 돌아가도록 스냅샷을 다시 보내 준다
		h.sendToClient(client, RUMessage{
			Type:    RUMsgGameState,
			Payload: h.buildRUState(room, client.Seat),
		})
		return
	}
	log.Printf("[루미큐브][확정] game=%s | seat%d=%s (테이블 %d세트 · 받침대 %d개 · %d차례)",
		room.Game.ID, client.Seat, displayName(client.Name),
		len(room.Game.Sets), len(room.Game.Players[client.Seat].Rack), room.Game.Turns)
	h.afterProgress(room)
}

// handleDraw 못 내겠으니 타일더미에서 1개 가져오고 차례 종료
func (h *RUHub) handleDraw(client *RUClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	if err := room.Game.Draw(client.Seat); err != nil {
		h.sendError(client, err.Error())
		return
	}
	// 가져온 타일의 정체는 로그에도 남기지 않는다 (은닉 계약)
	log.Printf("[루미큐브][가져오기] game=%s | seat%d=%s (타일더미 %d개 · %d차례)",
		room.Game.ID, client.Seat, displayName(client.Name),
		len(room.Game.Pool), room.Game.Turns)
	h.afterProgress(room)
}

// afterProgress 모든 게임 변이 뒤의 공통 마무리 — 이벤트 방송·종료 판정·
// 새 차례의 마감 예약·스냅샷 방송을 한 번에 처리한다.
func (h *RUHub) afterProgress(room *ruRoom) {
	h.drainEvents(room)
	if room.Game.Phase == RUPhaseGameOver {
		h.finishGame(room)
		return
	}
	h.syncDeadline(room)
	h.broadcastState(room)
}

// drainEvents 순수 규칙이 쌓은 이벤트를 ru_event 로 방송한다
func (h *RUHub) drainEvents(room *ruRoom) {
	for _, ev := range room.Game.DrainEvents() {
		payload := RUEventPayload{Kind: ev.Kind, Message: ev.Message}
		if ev.Seat >= 0 && ev.Seat < len(room.Game.Players) {
			seat := ev.Seat
			payload.Seat = &seat
			payload.Name = room.Game.Players[seat].Name
		}
		h.broadcastEvent(room, payload)
	}
}

// ==================== 차례 마감 타이머 (AFK 진행 보장) ====================

// syncDeadline 새 차례(StateSeq 변경)가 열렸을 때만 마감을 다시 건다.
func (h *RUHub) syncDeadline(room *ruRoom) {
	game := room.Game
	if room.DeadlineSeq == game.StateSeq {
		return
	}
	room.DeadlineSeq = game.StateSeq
	switch game.Phase {
	case RUPhaseTurn:
		h.scheduleDeadline(room, ruTurnTimeout)
	default:
		h.stopPhaseTimer(room)
	}
}

// scheduleDeadline 마감 타이머 예약. AfkSeq 를 올려 지나간 발화를 구분하고,
// 발화는 허브 채널을 경유하므로 허브 밖에서 상태를 만지지 않는다.
func (h *RUHub) scheduleDeadline(room *ruRoom, dur time.Duration) {
	h.stopPhaseTimer(room)
	room.Game.AfkSeq++
	room.Game.Deadline = time.Now().Add(dur).UnixMilli()
	sig := ruPhaseSignal{GameID: room.Game.ID, Seq: room.Game.AfkSeq}
	room.PhaseTimer = time.AfterFunc(dur, func() {
		h.phaseFired <- sig
	})
}

func (h *RUHub) stopPhaseTimer(room *ruRoom) {
	if room.PhaseTimer != nil {
		room.PhaseTimer.Stop()
		room.PhaseTimer = nil
	}
}

// handlePhaseFired 마감 발화 — 타일 1개를 가져가고 차례를 끝낸다
func (h *RUHub) handlePhaseFired(sig ruPhaseSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.Game.AfkSeq != sig.Seq {
		return
	}
	game := room.Game
	if game.Phase != RUPhaseTurn {
		return
	}
	seat := game.CurrentSeat
	if seat < 0 || seat >= len(game.Players) {
		return
	}
	actor := game.Players[seat]

	h.broadcastEvent(room, RUEventPayload{Kind: "auto_action", Seat: &seat, Name: actor.Name,
		Message: fmt.Sprintf("%s님이 오래 응답하지 않아 자동으로 타일 1개를 가져갑니다", actor.Name)})
	game.ForceTurn(h.rng)
	log.Printf("[루미큐브][자동진행] game=%s | seat%d 무응답 — 자동 가져오기", game.ID, seat)

	h.afterProgress(room)
}

// ==================== 상태 뷰 (은닉의 핵심) ====================

// buildRUState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
//
// 은닉:
//   - yourRack·yourMelded 는 본인에게만 실린다 — 타인·관전자(viewerSeat -1)
//     의 raw JSON 에는 키 자체가 없다 (nil 포인터 + omitempty). 빈 받침대도
//     [] 로 보내야 하므로 슬라이스 포인터를 쓴다.
//   - 타일더미의 내용은 이 스냅샷 어디에도 없다 — 남은 개수(poolLeft)만.
//   - sets 는 전원 공개이며 조커에 standsFor 가 채워져 있다.
//
// viewerSeat -1(관전자)·좌석 없는 방에서도 패닉 없이 만들어져야 한다.
func (h *RUHub) buildRUState(room *ruRoom, viewerSeat int) RUGameStatePayload {
	game := room.Game
	seated := viewerSeat >= 0 && viewerSeat < len(game.Players)

	var yourRack *[]RUTile
	var yourMelded *bool
	if seated && game.Ready {
		rack := append([]RUTile{}, game.Players[viewerSeat].Rack...)
		yourRack = &rack
		melded := game.Players[viewerSeat].Melded
		yourMelded = &melded
	}

	players := []RUPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		players = append(players, RUPlayerView{
			Seat:      p.Seat,
			Name:      p.Name,
			Connected: c != nil && c.Connected,
			Bot:       c != nil && c.Bot,
			RackCount: len(p.Rack),
			Melded:    p.Melded,
			Score:     p.Score,
		})
	}

	endsAt := int64(0)
	if game.Phase == RUPhaseTurn {
		endsAt = game.Deadline
	}

	return RUGameStatePayload{
		GameID:      game.ID,
		RoomCode:    room.Code,
		Phase:       game.Phase,
		HostSeat:    h.hostSeat(room),
		YourSeat:    viewerSeat,
		Spectators:  len(room.Spectators),
		EndsAt:      endsAt,
		CurrentSeat: game.CurrentSeat,
		PoolLeft:    len(game.Pool),
		Sets:        ruTableView(game.Sets),
		YourRack:    yourRack,
		YourMelded:  yourMelded,
		Players:     players,
		LastAction:  game.LastAction,
		Result:      game.Result,
	}
}

// broadcastState 좌석마다 개인화 스냅샷을 만들어 보낸다.
// 관전자에게는 공개 정보만 담긴 스냅샷(viewerSeat -1)이 간다.
func (h *RUHub) broadcastState(room *ruRoom) {
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, RUMessage{
			Type:    RUMsgGameState,
			Payload: h.buildRUState(room, seat),
		})
	}
	if len(room.Spectators) > 0 {
		msg := RUMessage{Type: RUMsgGameState, Payload: h.buildRUState(room, -1)}
		for c := range room.Spectators {
			h.sendToClient(c, msg)
		}
	}
}

func (h *RUHub) broadcastEvent(room *ruRoom, event RUEventPayload) {
	h.broadcastToRoom(room, RUMessage{Type: RUMsgEvent, Payload: event})
}

// finishGame 게임 종료 발표·전적 기록·방 정리 (재대결은 같은 방 코드
// 재입장으로 — finishGame 이 코드를 반납해 같은 코드로 새 방을 만들 수 있다)
func (h *RUHub) finishGame(room *ruRoom) {
	game := room.Game
	h.stopPhaseTimer(room)
	game.Deadline = 0

	result := game.Result
	if result == nil { // 방어선 — 종료는 항상 결과와 함께 온다
		result = &RUResult{Rows: []RUResultRow{}, WinnerSeats: []int{},
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

	h.broadcastEvent(room, RUEventPayload{Kind: "game_over", Message: result.Message})
	// 최종 스냅샷을 먼저 보낸 뒤 종료를 알린다
	// (봇 러너는 ru_game_over 를 보고 스스로 끝난다)
	h.broadcastState(room)
	h.broadcastToRoom(room, RUMessage{
		Type: RUMsgGameOver,
		Payload: RUGameOverPayload{
			WinnerSeats: append([]int{}, result.WinnerSeats...),
			WinnerNames: append([]string{}, result.WinnerNames...),
			Rows:        append([]RUResultRow{}, result.Rows...),
			Message:     result.Message,
			Turns:       game.Turns,
			Players:     h.buildRUState(room, -1).Players,
		},
	})

	scores := []string{}
	for _, p := range game.Players {
		scores = append(scores, fmt.Sprintf("%s %d점", displayName(p.Name), p.Score))
	}
	log.Printf("[루미큐브][경기결과] game=%s | 승자=%s | 차례=%d | 소요=%s | %s",
		game.ID, strings.Join(winners, "·"), game.Turns,
		matchDuration(game.StartedAt), strings.Join(scores, " / "))

	RecordMatch(MatchRecord{
		Game:     "rummikub",
		Players:  strings.Join(winners, "·") + " vs " + strings.Join(losers, "·"),
		Winner:   strings.Join(winners, "·"),
		Reason:   "score",
		Duration: matchSeconds(game.StartedAt),
		Bot:      ruRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
	if room.Code != "" {
		delete(h.activeCodes, room.Code) // 코드 재사용 복귀
	}
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *RUHub) handleDisconnect(client *RUClient) {
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
	log.Printf("[루미큐브][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, RUMessage{
		Type: RUMsgPlayerDisconnected,
		Payload: RUPlayerDisconnectedPayload{
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
func (h *RUHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == RUPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[루미큐브][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := h.takeoverRUBot(room, seat, client.Name)
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.broadcastEvent(room, RUEventPayload{Kind: "bot_takeover", Seat: &seat,
		Name:    client.Name,
		Message: fmt.Sprintf("%s님 자리를 봇이 이어받았습니다", client.Name)})
	// 새 봇이 이 스냅샷을 받아 자기 차례면 즉시 이어간다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *RUHub) handleRejoin(client *RUClient, msg RUMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload RURejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, RUMessage{Type: RUMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, RUMessage{Type: RUMsgSessionExpired})
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

	log.Printf("[루미큐브][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, RUMessage{
		Type:    RUMsgPlayerReconnected,
		Payload: RUPlayerReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *RUHub) clearGameSessions(room *ruRoom) {
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		h.drop(c.SessionID)
	}
}

// ==================== 전송 ====================

func (h *RUHub) sendError(client *RUClient, message string) {
	h.sendToClient(client, RUMessage{Type: RUMsgError, Payload: RUErrorPayload{Message: message}})
}

func (h *RUHub) sendToClient(client *RUClient, message RUMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[RU] ")
}

func (h *RUHub) broadcastToRoom(room *ruRoom, message RUMessage) {
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

func ServeRUWs(hub *RUHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[RU] Error upgrading connection:", err)
		return
	}

	client := &RUClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[RU] ",
		func(msg RUMessage) { hub.gameMessage <- RUGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
