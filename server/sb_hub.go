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

// 사보타지 차례 마감 타이머 — 45초 무응답은 무작위 카드 자동 버리기로
// 해소한다 (테스트에서 짧게 낮춘다).
var sbTurnTimeout = 45 * time.Second

// sbRoom 게임(순수 상태)과 좌석별 연결의 매핑
type sbRoom struct {
	Game       *SBGame
	Clients    map[int]*SBClient // seat → client
	PhaseTimer *time.Timer       // 차례 마감 타이머

	// DeadlineSeq 마지막으로 마감을 건 StateSeq — 같은 차례에 스냅샷이
	// 쌓일 때마다(관전 입장·접속 변화 등) 마감이 늘어나지 않게 하는 근거
	DeadlineSeq int

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string

	// Spectators 관전자 연결 (사설 방 코드로만 진입, 상한 maxSpectators).
	// 좌석·세션이 없어 재접속을 지원하지 않는다 — 끊기면 목록에서만 뗀다.
	Spectators map[*SBClient]bool

	// LastReact 좌석별 마지막 리액션 발신 시각 (레이트리밋 장부)
	LastReact map[int]time.Time
}

// sbPhaseSignal 마감 타이머의 발화 표식 — 대기 상태 일련번호(AfkSeq)로
// 지나간 발화를 구분한다
type sbPhaseSignal struct {
	GameID string
	Seq    int
}

type SBHub struct {
	clients map[*SBClient]bool

	rooms          map[string]*sbRoom
	lobby          *sbRoom
	privateLobbies map[string]*sbRoom
	activeCodes    map[string]string

	register    chan *SBClient
	unregister  chan *SBClient
	gameMessage chan SBGameMessage
	phaseFired  chan sbPhaseSignal

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*SBClient]

	// 역할 배분·덱 셔플·자동 버리기용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type SBGameMessage struct {
	Client  *SBClient
	Message SBMessage
}

func NewSBHub() *SBHub {
	return &SBHub{
		register:       make(chan *SBClient),
		unregister:     make(chan *SBClient),
		clients:        make(map[*SBClient]bool),
		rooms:          make(map[string]*sbRoom),
		privateLobbies: make(map[string]*sbRoom),
		activeCodes:    make(map[string]string),
		gameMessage:    make(chan SBGameMessage),
		phaseFired:     make(chan sbPhaseSignal, 8),
		sessionManager: newSessionManager[*SBClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *SBHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[SB] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[SB] Client unregistered: %s", client.ID)
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

func (h *SBHub) handleGameMessage(gm SBGameMessage) {
	// 관전자는 어떤 행동도 할 수 없다 (보기 전용 — 리액션도 좌석 보유자만)
	if h.isSpectator(gm.Client) {
		h.sendError(gm.Client, spectatorDeniedMsg)
		return
	}
	switch gm.Message.Type {
	case SBMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case SBMsgReact:
		h.handleReact(gm.Client, gm.Message)
	case SBMsgFillBots:
		h.handleFillBots(gm.Client)
	case SBMsgStart:
		h.handleStart(gm.Client)
	case SBMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case SBMsgPlace:
		h.handlePlace(gm.Client, gm.Message)
	case SBMsgAction:
		h.handleAction(gm.Client, gm.Message)
	case SBMsgDiscard:
		h.handleDiscard(gm.Client, gm.Message)
	}
}

// ==================== 대기실 ====================

func (h *SBHub) handleJoinGame(client *SBClient, msg SBMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SBJoinGamePayload
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

	log.Printf("[사보타지][입장] game=%s | seat%d=%s 대기 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), SBMaxPlayers)
	if room.Code == "" { // 사설 방 입장은 조용히 (공용 로비만 알림)
		notify("사보타지 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), len(room.Game.Players), SBMaxPlayers))
	}

	h.sendToClient(client, SBMessage{
		Type: SBMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
			"roomCode":  room.Code,
		},
	})

	h.broadcastEvent(room, SBEventPayload{Kind: "joined", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 입장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	// 정원이 차도 자동 시작하지 않는다 — 호스트 명시 시작만
	h.broadcastState(room)
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다
func (h *SBHub) lobbyRoomFor(roomField string) *sbRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewSBGame(uuid.New().String())
			h.lobby = &sbRoom{Game: game, Clients: map[int]*SBClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[SB] Created lobby game %s", game.ID)
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
		game := NewSBGame(uuid.New().String())
		room = &sbRoom{Game: game, Clients: map[int]*SBClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[SB] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// ==================== 관전 / 리액션 ====================

// addSpectator 진행 중(또는 가득 찬) 사설 방에 관전자로 등록한다
func (h *SBHub) addSpectator(room *sbRoom, client *SBClient, name string) {
	if len(room.Spectators) >= maxSpectators {
		h.sendError(client, spectatorFullMsg)
		return
	}
	if room.Spectators == nil {
		room.Spectators = map[*SBClient]bool{}
	}
	client.Name = name
	client.GameID = room.Game.ID
	room.Spectators[client] = true

	log.Printf("[사보타지][관전] game=%s | %s 관전 입장 (%d명)",
		room.Game.ID, displayName(name), len(room.Spectators))

	h.sendToClient(client, SBMessage{
		Type:    SBMsgSpectateJoined,
		Payload: map[string]interface{}{"gameId": room.Game.ID, "roomCode": room.Code},
	})
	h.broadcastState(room)
}

// isSpectator 관전자 연결인지 (좌석 보유자·미입장 연결은 false)
func (h *SBHub) isSpectator(client *SBClient) bool {
	if client.GameID == "" {
		return false
	}
	room := h.rooms[client.GameID]
	return room != nil && room.Spectators[client]
}

// handleReact 리액션 이모지 — 좌석 보유자만, waiting 중에도 허용
func (h *SBHub) handleReact(client *SBClient, msg SBMessage) {
	room := h.rooms[client.GameID]
	if room == nil || client.Seat < 0 {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SBReactPayload
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
	h.broadcastEvent(room, SBEventPayload{Kind: "react", Seat: &seat, Name: client.Name,
		Message: payload.Emoji})
}

// waitingRoomOf 클라이언트가 속한 시작 전 방
func (h *SBHub) waitingRoomOf(client *SBClient) *sbRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Ready {
		return nil
	}
	return room
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *SBHub) hostSeat(room *sbRoom) int {
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

// sbHumanCount 방의 사람 수
func sbHumanCount(room *sbRoom) int {
	n := 0
	for _, c := range room.Clients {
		if c != nil && !c.Bot {
			n++
		}
	}
	return n
}

// updateLobbyWaiting 로비 현황판 갱신 — 사람 1명 이상 대기 && 미시작
func (h *SBHub) updateLobbyWaiting(room *sbRoom) {
	if room.Code != "" {
		return
	}
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && sbHumanCount(room) >= 1
	lobbySetWaiting("saboteur", waiting)
}

// handleFillBots host 가 빈 좌석을 연습봇으로 5인까지 채운 뒤 즉시 시작한다
func (h *SBHub) handleFillBots(client *SBClient) {
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
	for len(room.Game.Players) < SBFillBotTarget {
		botNo++
		if !h.spawnSBBot(room, fmt.Sprintf("%s%d", botName, botNo)) {
			break
		}
	}

	if room.Game.CanStart() {
		h.startGame(room)
		return
	}
	h.broadcastState(room)
}

func (h *SBHub) handleStart(client *SBClient) {
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
		h.sendError(client, fmt.Sprintf("%d명 이상 모여야 시작할 수 있습니다", SBMinPlayers))
		return
	}
	h.startGame(room)
}

func (h *SBHub) startGame(room *sbRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	if room.Code != "" {
		delete(h.privateLobbies, room.Code)
		h.activeCodes[room.Code] = room.Game.ID
	} else {
		h.lobby = nil
		lobbySetWaiting("saboteur", false)
	}

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	pool := room.Game.Pool
	log.Printf("[사보타지][경기시작] game=%s | 인원=%d | 풀=광부%d:방해꾼%d | 선=seat%d | %v",
		room.Game.ID, len(room.Game.Players), pool.Miner, pool.Saboteur,
		room.Game.CurrentSeat, names)
	if !sbRoomHasBot(room) { // 연습봇전은 운영자 알림을 억제한다
		notify("사보타지 시작", fmt.Sprintf("%d인전 시작", len(room.Game.Players)))
	}

	h.broadcastEvent(room, SBEventPayload{Kind: "game_started",
		Message: fmt.Sprintf(
			"게임 시작 — %d인전, 역할 풀은 광부 %d·방해꾼 %d 중 %d장을 나눠 가집니다. 자기 역할은 자기만 봅니다",
			len(room.Game.Players), pool.Miner, pool.Saboteur, len(room.Game.Players))})
	h.afterProgress(room)
}

// removeFromLobby 대기실에서 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *SBHub) removeFromLobby(room *sbRoom, client *SBClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	rebuilt := map[int]*SBClient{}
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

	log.Printf("[사보타지][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), SBMaxPlayers)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if sbHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, SBMessage{Type: SBMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			lobbySetWaiting("saboteur", false)
		}
		return
	}

	h.broadcastEvent(room, SBEventPayload{Kind: "left", Name: client.Name,
		Message: fmt.Sprintf("%s님이 퇴장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *SBHub) roomOf(client *SBClient) *sbRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

// handlePlace 길 타일 배치
func (h *SBHub) handlePlace(client *SBClient, msg SBMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SBPlacePayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.Place(client.Seat, payload.Index, payload.Col, payload.Row, payload.Flip); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[사보타지][배치] game=%s | seat%d=%s → (%d,%d) flip=%v (덱 %d장)",
		room.Game.ID, client.Seat, displayName(client.Name),
		payload.Col, payload.Row, payload.Flip, len(room.Game.Deck))
	h.afterProgress(room)
}

// handleAction 행동 카드 (지도·낙석·파괴·수리)
func (h *SBHub) handleAction(client *SBClient, msg SBMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SBActionPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.Action(client.Seat, payload.Index, payload); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[사보타지][행동] game=%s | seat%d=%s 행동 카드 사용 (덱 %d장)",
		room.Game.ID, client.Seat, displayName(client.Name), len(room.Game.Deck))
	h.afterProgress(room)
}

// handleDiscard 버리기
func (h *SBHub) handleDiscard(client *SBClient, msg SBMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SBDiscardPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.Discard(client.Seat, payload.Index); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.afterProgress(room)
}

// afterProgress 모든 게임 변이 뒤의 공통 마무리 — 이벤트 방송·개인 통지·
// 종료 판정·새 차례의 마감 예약·스냅샷 방송을 한 번에 처리한다.
func (h *SBHub) afterProgress(room *sbRoom) {
	h.drainEvents(room)
	h.drainPrivates(room)
	if room.Game.Phase == SBPhaseGameOver {
		h.finishGame(room)
		return
	}
	h.syncDeadline(room)
	h.broadcastState(room)
}

// drainEvents 순수 규칙이 쌓은 이벤트를 sb_event 로 방송한다
func (h *SBHub) drainEvents(room *sbRoom) {
	for _, ev := range room.Game.DrainEvents() {
		payload := SBEventPayload{Kind: ev.Kind, Message: ev.Message}
		if ev.Seat >= 0 && ev.Seat < len(room.Game.Players) {
			seat := ev.Seat
			payload.Seat = &seat
			payload.Name = room.Game.Players[seat].Name
		}
		h.broadcastEvent(room, payload)
	}
}

// drainPrivates 지도 결과를 그 좌석 한 명에게만 보낸다 (은닉의 유일한
// 예외 경로 — 방송하지 않는다)
func (h *SBHub) drainPrivates(room *sbRoom) {
	for _, pv := range room.Game.DrainPrivates() {
		c := room.Clients[pv.Seat]
		if c == nil {
			continue
		}
		h.sendToClient(c, SBMessage{
			Type:    SBMsgMap,
			Payload: SBMapPayload{Index: pv.Index, Gold: pv.Gold},
		})
	}
}

// ==================== 차례 마감 타이머 (AFK 진행 보장) ====================

// syncDeadline 새 차례(StateSeq 변경)가 열렸을 때만 마감을 다시 건다
func (h *SBHub) syncDeadline(room *sbRoom) {
	game := room.Game
	if room.DeadlineSeq == game.StateSeq {
		return
	}
	room.DeadlineSeq = game.StateSeq
	if game.Phase != SBPhasePlaying {
		h.stopPhaseTimer(room)
		return
	}
	h.scheduleDeadline(room, sbTurnTimeout)
}

// scheduleDeadline 마감 타이머 예약. AfkSeq 를 올려 지나간 발화를 구분하고,
// 발화는 허브 채널을 경유하므로 허브 밖에서 상태를 만지지 않는다.
func (h *SBHub) scheduleDeadline(room *sbRoom, dur time.Duration) {
	h.stopPhaseTimer(room)
	room.Game.AfkSeq++
	room.Game.Deadline = time.Now().Add(dur).UnixMilli()
	sig := sbPhaseSignal{GameID: room.Game.ID, Seq: room.Game.AfkSeq}
	room.PhaseTimer = time.AfterFunc(dur, func() {
		h.phaseFired <- sig
	})
}

func (h *SBHub) stopPhaseTimer(room *sbRoom) {
	if room.PhaseTimer != nil {
		room.PhaseTimer.Stop()
		room.PhaseTimer = nil
	}
}

// handlePhaseFired 차례 마감 발화 — 무작위 카드 자동 버리기로 해소한다
func (h *SBHub) handlePhaseFired(sig sbPhaseSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.Game.AfkSeq != sig.Seq {
		return
	}
	game := room.Game
	if game.Phase != SBPhasePlaying {
		return
	}
	seat := game.CurrentSeat
	if seat < 0 || seat >= len(game.Players) {
		return
	}
	actor := game.Players[seat]
	h.broadcastEvent(room, SBEventPayload{Kind: "afk", Seat: &seat, Name: actor.Name,
		Message: fmt.Sprintf("%s님이 오래 응답하지 않아 자동으로 카드를 버립니다", actor.Name)})
	game.ForceDiscard(h.rng)
	log.Printf("[사보타지][자동진행] game=%s | seat%d 무응답 — 자동 버리기",
		game.ID, seat)
	h.afterProgress(room)
}

// ==================== 상태 뷰 (은닉의 핵심) ====================

// sbBoardView 판의 공개 표현. 목표 타일의 gold 는 공개된 것만 실린다 —
// 그 전에는 키 자체가 없다 (뷰어가 누구든 동일하다).
func sbBoardView(game *SBGame) []SBBoardCell {
	cells := []SBBoardCell{}
	for row := 0; row < SBRows; row++ {
		for col := 0; col < SBCols; col++ {
			c := game.Board[sbIdx(col, row)]
			if c == nil {
				continue
			}
			view := SBBoardCell{
				Col: c.Col, Row: c.Row, Kind: c.Kind,
				Up: c.Up, Right: c.Right, Down: c.Down, Left: c.Left,
				Dead: c.Dead, Revealed: c.Revealed,
			}
			if c.Kind == SBTileGoal && c.Revealed {
				gold := c.Gold
				view.Gold = &gold
			}
			cells = append(cells, view)
		}
	}
	return cells
}

// buildSBState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
// 은닉: yourRole·yourHand 는 본인에게만 실린다 — 타인·관전자(viewerSeat -1)의
// raw JSON 에는 키 자체가 없다 (빈 문자열·nil 포인터 생략). yourHand 는 빈
// 손패도 [] 로 보내야 하므로 슬라이스 포인터를 쓴다.
// 목표 타일의 gold 는 공개 전까지 누구의 스냅샷에도 없고, players[].role 은
// game_over 전까지 전원 "" 다. rolePool 은 전원 공개 정보다.
func (h *SBHub) buildSBState(room *sbRoom, viewerSeat int) SBGameStatePayload {
	game := room.Game
	seated := viewerSeat >= 0 && viewerSeat < len(game.Players)

	yourRole := ""
	if seated {
		yourRole = string(game.Players[viewerSeat].Role) // 시작 전엔 "" → 필드 생략
	}
	var yourHand *[]SBCard
	if seated && game.Ready {
		hand := append([]SBCard{}, game.Players[viewerSeat].Hand...)
		yourHand = &hand
	}

	revealed := game.Phase == SBPhaseGameOver
	players := []SBPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		role := ""
		if revealed {
			role = string(p.Role)
		}
		players = append(players, SBPlayerView{
			Seat:      p.Seat,
			Name:      p.Name,
			Connected: c != nil && c.Connected,
			Bot:       c != nil && c.Bot,
			HandCount: len(p.Hand),
			Tools:     p.Tools,
			Role:      role,
		})
	}

	// 시작 전에는 현재 인원 기준 풀을 미리 보여준다 (표 밖 인원은 0:0)
	pool := game.Pool
	if !game.Ready {
		pool = sbRolePoolFor(len(game.Players))
	}

	endsAt := int64(0)
	if game.Phase == SBPhasePlaying {
		endsAt = game.Deadline
	}

	return SBGameStatePayload{
		GameID:      game.ID,
		RoomCode:    room.Code,
		Phase:       game.Phase,
		HostSeat:    h.hostSeat(room),
		YourSeat:    viewerSeat,
		Spectators:  len(room.Spectators),
		EndsAt:      endsAt,
		CurrentSeat: game.CurrentSeat,
		DeckLeft:    len(game.Deck),
		Turns:       game.Turns,
		RolePool:    pool,
		Board:       sbBoardView(game),
		YourRole:    yourRole,
		YourHand:    yourHand,
		Players:     players,
		LastAction:  game.LastAction,
		Result:      game.Result,
	}
}

// broadcastState 좌석마다 개인화 스냅샷을 만들어 보낸다.
// 관전자에게는 공개 정보만 담긴 스냅샷(viewerSeat -1)이 간다.
func (h *SBHub) broadcastState(room *sbRoom) {
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, SBMessage{
			Type:    SBMsgGameState,
			Payload: h.buildSBState(room, seat),
		})
	}
	if len(room.Spectators) > 0 {
		msg := SBMessage{Type: SBMsgGameState, Payload: h.buildSBState(room, -1)}
		for c := range room.Spectators {
			h.sendToClient(c, msg)
		}
	}
}

func (h *SBHub) broadcastEvent(room *sbRoom, event SBEventPayload) {
	h.broadcastToRoom(room, SBMessage{Type: SBMsgEvent, Payload: event})
}

// finishGame 게임 종료 발표·전적 기록·방 정리 (재대결은 같은 방 코드
// 재입장으로 — finishGame 이 코드를 반납해 같은 코드로 새 방을 만들 수 있다)
func (h *SBHub) finishGame(room *sbRoom) {
	game := room.Game
	h.stopPhaseTimer(room)
	game.Deadline = 0

	result := game.Result
	if result == nil { // 방어선 — 종료는 항상 결과와 함께 온다 (무승부 없음)
		result = &SBResult{Winner: string(SBRoleSaboteur), GoldIndex: game.GoldIndex,
			Reason: "exhausted", Message: "게임이 종료됐습니다"}
	}

	winners, losers := []string{}, []string{}
	for _, p := range game.Players {
		if string(p.Role) == result.Winner {
			winners = append(winners, displayName(p.Name))
		} else {
			losers = append(losers, displayName(p.Name))
		}
	}

	h.broadcastEvent(room, SBEventPayload{Kind: "game_over",
		Message: fmt.Sprintf("게임 종료 — %s 진영의 승리! %s",
			sbRoleLabel(result.Winner), result.Message)})
	// 전원 역할·금 위치가 공개된 최종 스냅샷을 먼저 보낸 뒤 종료를 알린다
	// (봇 러너는 sb_game_over 를 보고 스스로 끝난다)
	h.revealAllGoals(room)
	h.broadcastState(room)
	h.broadcastToRoom(room, SBMessage{
		Type: SBMsgGameOver,
		Payload: SBGameOverPayload{
			Winner:    result.Winner,
			Reason:    result.Reason,
			Message:   result.Message,
			GoldIndex: result.GoldIndex,
			Turns:     game.Turns,
			Board:     sbBoardView(game),
			Players:   h.buildSBState(room, -1).Players,
		},
	})

	log.Printf("[사보타지][경기결과] game=%s | 승자=%s(%s) | 사유=%s | 금=%d번 | 차례=%d | 소요=%s",
		game.ID, result.Winner, sbRoleLabel(result.Winner), result.Reason,
		result.GoldIndex, game.Turns, matchDuration(game.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "saboteur",
		Players:  strings.Join(winners, "·") + " vs " + strings.Join(losers, "·"),
		Winner:   strings.Join(winners, "·"),
		Reason:   result.Reason,
		Duration: matchSeconds(game.StartedAt),
		Bot:      sbRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
	if room.Code != "" {
		delete(h.activeCodes, room.Code) // 코드 재사용 복귀
	}
}

// revealAllGoals 종료 화면에서 목표 타일 3장을 모두 뒤집는다 (금 위치 공개)
func (h *SBHub) revealAllGoals(room *sbRoom) {
	game := room.Game
	for i, gc := range sbGoalCells {
		cell := game.Board[sbIdx(gc[0], gc[1])]
		if cell == nil || cell.Kind != SBTileGoal {
			continue
		}
		cell.Revealed = true
		cell.Gold = i == game.GoldIndex
	}
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *SBHub) handleDisconnect(client *SBClient) {
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
	log.Printf("[사보타지][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, SBMessage{
		Type: SBMsgPlayerDisconnected,
		Payload: SBPlayerDisconnectedPayload{
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
func (h *SBHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == SBPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[사보타지][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := h.takeoverSBBot(room, seat, client.Name)
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.broadcastEvent(room, SBEventPayload{Kind: "bot_takeover", Seat: &seat,
		Name:    client.Name,
		Message: fmt.Sprintf("%s님 자리를 봇이 이어받았습니다", client.Name)})
	// 새 봇이 이 스냅샷을 받아 자기 차례면 즉시 이어간다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *SBHub) handleRejoin(client *SBClient, msg SBMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SBRejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, SBMessage{Type: SBMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, SBMessage{Type: SBMsgSessionExpired})
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

	log.Printf("[사보타지][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, SBMessage{
		Type:    SBMsgPlayerReconnected,
		Payload: SBPlayerReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *SBHub) clearGameSessions(room *sbRoom) {
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		h.drop(c.SessionID)
	}
}

// ==================== 전송 ====================

func (h *SBHub) sendError(client *SBClient, message string) {
	h.sendToClient(client, SBMessage{Type: SBMsgError, Payload: SBErrorPayload{Message: message}})
}

func (h *SBHub) sendToClient(client *SBClient, message SBMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[SB] ")
}

func (h *SBHub) broadcastToRoom(room *sbRoom, message SBMessage) {
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

func ServeSBWs(hub *SBHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[SB] Error upgrading connection:", err)
		return
	}

	client := &SBClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[SB] ",
		func(msg SBMessage) { hub.gameMessage <- SBGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
