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

// 인사이더 단계 마감 타이머 — 질문 타임 초과는 전원 패배, 토론 마감은 자동
// 투표 개시, 투표 미제출은 무작위 투표로 해소된다 (테스트에서 짧게 낮춘다).
var (
	idQuestionTimeout   = 5 * time.Minute  // question — 초과 시 전원 패배 종료
	idDiscussionTimeout = 2 * time.Minute  // discussion — 마감 시 자동 투표 개시
	idVoteTimeout       = 45 * time.Second // voting — 미제출 좌석 무작위 투표
)

// idRoom 게임(순수 상태)과 좌석별 연결의 매핑
type idRoom struct {
	Game       *IDGame
	Clients    map[int]*IDClient // seat → client
	PhaseTimer *time.Timer       // 단계 마감 타이머

	// DeadlineSeq 마지막으로 마감을 건 StateSeq — 같은 대기 상태에 스냅샷이
	// 쌓일 때마다 마감이 늘어나지 않게 하는 근거
	DeadlineSeq int

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string

	// Spectators 관전자 연결 (사설 방 코드로만 진입, 상한 maxSpectators).
	// 좌석·세션이 없어 재접속을 지원하지 않는다 — 끊기면 목록에서만 뗀다.
	Spectators map[*IDClient]bool

	// LastReact 좌석별 마지막 리액션 발신 시각 (레이트리밋 장부)
	LastReact map[int]time.Time
}

// idPhaseSignal 마감 타이머의 발화 표식 — 대기 상태 일련번호(AfkSeq)로
// 지나간 발화를 구분한다
type idPhaseSignal struct {
	GameID string
	Seq    int
}

type IDHub struct {
	// 등록된 클라이언트
	clients map[*IDClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*idRoom

	// 글로벌 로비. 시작 전 공용 방은 하나뿐이다.
	lobby *idRoom

	// 사설 방 (초대 코드 → 시작 전 방). 허브 고루틴에서만 접근한다.
	// 시작하면 privateLobbies 에서 activeCodes 로 옮긴다.
	privateLobbies map[string]*idRoom

	// 진행 중 사설 방 (초대 코드 → gameID). 시작 후에도 코드로 방을 찾아
	// 관전 입장시키는 근거다. finishGame 에서 해제한다 (코드 재사용 복귀).
	activeCodes map[string]string

	// 클라이언트 등록
	register chan *IDClient

	// 클라이언트 등록 해제
	unregister chan *IDClient

	// 게임 메시지
	gameMessage chan IDGameMessage

	// 마감 타이머 발화 (time.AfterFunc → 허브 채널 경유)
	phaseFired chan idPhaseSignal

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*IDClient]

	// 역할 배정·제시어 추출·무작위 투표용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type IDGameMessage struct {
	Client  *IDClient
	Message IDMessage
}

func NewIDHub() *IDHub {
	return &IDHub{
		register:       make(chan *IDClient),
		unregister:     make(chan *IDClient),
		clients:        make(map[*IDClient]bool),
		rooms:          make(map[string]*idRoom),
		privateLobbies: make(map[string]*idRoom),
		activeCodes:    make(map[string]string),
		gameMessage:    make(chan IDGameMessage),
		phaseFired:     make(chan idPhaseSignal, 8),
		sessionManager: newSessionManager[*IDClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *IDHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[ID] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[ID] Client unregistered: %s", client.ID)
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

func (h *IDHub) handleGameMessage(gm IDGameMessage) {
	// 관전자는 어떤 행동도 할 수 없다 (보기 전용 — 리액션도 좌석 보유자만)
	if h.isSpectator(gm.Client) {
		h.sendError(gm.Client, spectatorDeniedMsg)
		return
	}
	switch gm.Message.Type {
	case IDMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case IDMsgReact:
		h.handleReact(gm.Client, gm.Message)
	case IDMsgFillBots:
		h.handleFillBots(gm.Client)
	case IDMsgStart:
		h.handleStart(gm.Client)
	case IDMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case IDMsgCorrect:
		h.handleCorrect(gm.Client)
	case IDMsgOpenVote:
		h.handleOpenVote(gm.Client)
	case IDMsgVote:
		h.handleVote(gm.Client, gm.Message)
	}
}

// ==================== 대기실 ====================

func (h *IDHub) handleJoinGame(client *IDClient, msg IDMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload IDJoinGamePayload
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

	log.Printf("[인사이더][입장] game=%s | seat%d=%s 대기 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), IDMaxPlayers)
	if room.Code == "" { // 사설 방 입장은 조용히 (공용 로비만 알림)
		notify("인사이더 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), len(room.Game.Players), IDMaxPlayers))
	}

	h.sendToClient(client, IDMessage{
		Type: IDMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
			"roomCode":  room.Code,
		},
	})

	h.broadcastEvent(room, IDEventPayload{Kind: "joined", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 입장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	// 정원이 차도 자동 시작하지 않는다 — 호스트 명시 시작만 (봇 채우기와 충돌 방지)
	h.broadcastState(room)
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다.
// 생략/빈 문자열은 기존 공용 로비 경로 그대로, "NEW"는 새 코드 발급,
// 그 외 코드는 해당 사설 방 (없으면 그 코드로 관대하게 새로 생성).
func (h *IDHub) lobbyRoomFor(roomField string) *idRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewIDGame(uuid.New().String())
			h.lobby = &idRoom{Game: game, Clients: map[int]*IDClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[ID] Created lobby game %s", game.ID)
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
		game := NewIDGame(uuid.New().String())
		room = &idRoom{Game: game, Clients: map[int]*IDClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[ID] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// ==================== 관전 / 리액션 ====================

// addSpectator 진행 중(또는 가득 찬) 사설 방에 관전자로 등록한다.
// 좌석·세션 없음 — 재접속 미지원, 끊기면 다시 코드로 들어온다.
func (h *IDHub) addSpectator(room *idRoom, client *IDClient, name string) {
	if len(room.Spectators) >= maxSpectators {
		h.sendError(client, spectatorFullMsg)
		return
	}
	if room.Spectators == nil {
		room.Spectators = map[*IDClient]bool{}
	}
	client.Name = name
	client.GameID = room.Game.ID
	room.Spectators[client] = true

	log.Printf("[인사이더][관전] game=%s | %s 관전 입장 (%d명)",
		room.Game.ID, displayName(name), len(room.Spectators))

	h.sendToClient(client, IDMessage{
		Type:    IDMsgSpectateJoined,
		Payload: map[string]interface{}{"gameId": room.Game.ID, "roomCode": room.Code},
	})
	// 전원(관전자 포함)에게 관전자 수가 반영된 스냅샷 — 신규 관전자의 첫 스냅샷 겸용
	h.broadcastState(room)
}

// isSpectator 관전자 연결인지 (좌석 보유자·미입장 연결은 false)
func (h *IDHub) isSpectator(client *IDClient) bool {
	if client.GameID == "" {
		return false
	}
	room := h.rooms[client.GameID]
	return room != nil && room.Spectators[client]
}

// handleReact 리액션 이모지 — 좌석 보유자만, waiting 중에도 허용.
// 화이트리스트 외·레이트리밋 초과는 조용히 무시한다 (상태 저장 없음).
func (h *IDHub) handleReact(client *IDClient, msg IDMessage) {
	room := h.rooms[client.GameID]
	if room == nil || client.Seat < 0 {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload IDReactPayload
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
	h.broadcastEvent(room, IDEventPayload{Kind: "react", Seat: &seat, Name: client.Name, Message: payload.Emoji})
}

// waitingRoomOf 클라이언트가 속한 시작 전 방 (공용 로비 또는 사설 방)
func (h *IDHub) waitingRoomOf(client *IDClient) *idRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Ready {
		return nil
	}
	return room
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *IDHub) hostSeat(room *idRoom) int {
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

// idHumanCount 방의 사람 수
func idHumanCount(room *idRoom) int {
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
func (h *IDHub) updateLobbyWaiting(room *idRoom) {
	if room.Code != "" {
		return
	}
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && idHumanCount(room) >= 1
	lobbySetWaiting("insider", waiting)
}

// handleFillBots host 가 빈 좌석을 연습봇으로 5인까지 채운 뒤 즉시
// 시작한다 (스폰은 join 을 거치지 않으므로 명시 시작 호출).
func (h *IDHub) handleFillBots(client *IDClient) {
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
	for len(room.Game.Players) < IDFillBotTarget {
		botNo++
		if !h.spawnIDBot(room, fmt.Sprintf("%s%d", botName, botNo)) {
			break
		}
	}

	if room.Game.CanStart() {
		h.startGame(room)
		return
	}
	h.broadcastState(room)
}

func (h *IDHub) handleStart(client *IDClient) {
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
		h.sendError(client, fmt.Sprintf("%d명 이상 모여야 시작할 수 있습니다", IDMinPlayers))
		return
	}
	h.startGame(room)
}

func (h *IDHub) startGame(room *idRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	if room.Code != "" {
		// 시작한 사설 방의 코드는 진행 중 장부로 옮긴다 (관전 입장 근거)
		delete(h.privateLobbies, room.Code)
		h.activeCodes[room.Code] = room.Game.ID
	} else {
		h.lobby = nil // 시작한 방은 로비에서 떼어낸다
		lobbySetWaiting("insider", false)
	}

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	log.Printf("[인사이더][경기시작] game=%s | 인원=%d | 마스터=seat%d | %v",
		room.Game.ID, len(room.Game.Players), room.Game.MasterSeat, names)
	if !idRoomHasBot(room) { // 연습봇전은 운영자 알림을 억제한다
		notify("인사이더 게임 시작", fmt.Sprintf("%d인전 시작", len(room.Game.Players)))
	}

	master := room.Game.MasterSeat
	h.broadcastEvent(room, IDEventPayload{Kind: "game_started", Seat: &master,
		Name: room.Game.Players[master].Name,
		Message: fmt.Sprintf("게임 시작 — 마스터는 %s님입니다. 음성으로 스무고개를 진행하세요 (질문 타임)",
			room.Game.Players[master].Name)})
	h.afterProgress(room)
}

// removeFromLobby 대기실에서 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *IDHub) removeFromLobby(room *idRoom, client *IDClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	// 좌석 압축: oldSeat 뒤의 클라이언트를 한 칸씩 당긴다
	rebuilt := map[int]*IDClient{}
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

	log.Printf("[인사이더][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), IDMaxPlayers)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if idHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, IDMessage{Type: IDMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			lobbySetWaiting("insider", false)
		}
		return
	}

	h.broadcastEvent(room, IDEventPayload{Kind: "left", Name: client.Name,
		Message: fmt.Sprintf("%s님이 퇴장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *IDHub) roomOf(client *IDClient) *idRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

// handleCorrect 마스터의 [정답 나옴] — 질문 타임을 닫고 토론 타임을 연다
func (h *IDHub) handleCorrect(client *IDClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	if err := room.Game.MarkCorrect(client.Seat); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[인사이더][정답] game=%s | seat%d=%s 정답 나옴 선언 — 토론 시작",
		room.Game.ID, client.Seat, displayName(client.Name))
	h.afterProgress(room)
}

// handleOpenVote 마스터의 [투표 시작] — 토론 타임 단축
func (h *IDHub) handleOpenVote(client *IDClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	if err := room.Game.OpenVote(client.Seat); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[인사이더][투표시작] game=%s | seat%d=%s 투표 개시",
		room.Game.ID, client.Seat, displayName(client.Name))
	h.afterProgress(room)
}

// handleVote 비공개 투표 제출 — 전원 제출 시 순수 규칙이 즉시 개표한다
func (h *IDHub) handleVote(client *IDClient, msg IDMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload IDVotePayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.SubmitVote(client.Seat, payload.Seat); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[인사이더][투표] game=%s | seat%d=%s → seat%d 지목 (%d/%d)",
		room.Game.ID, client.Seat, displayName(client.Name), payload.Seat,
		room.Game.votedCount(), len(room.Game.Players))
	h.afterProgress(room)
}

// afterProgress 모든 게임 변이 뒤의 공통 마무리 — 이벤트 방송·종료 판정·
// 새 대기 상태의 마감 예약·스냅샷 방송을 한 번에 처리한다.
func (h *IDHub) afterProgress(room *idRoom) {
	h.drainEvents(room)
	if room.Game.Phase == IDPhaseGameOver {
		h.finishGame(room)
		return
	}
	h.syncDeadline(room)
	h.broadcastState(room)
}

// drainEvents 순수 규칙이 쌓은 이벤트를 id_event 로 방송한다
func (h *IDHub) drainEvents(room *idRoom) {
	for _, ev := range room.Game.DrainEvents() {
		payload := IDEventPayload{Kind: ev.Kind, Message: ev.Message}
		if ev.Seat >= 0 && ev.Seat < len(room.Game.Players) {
			seat := ev.Seat
			payload.Seat = &seat
			payload.Name = room.Game.Players[seat].Name
		}
		h.broadcastEvent(room, payload)
	}
}

// ==================== 단계 마감 타이머 (AFK 진행 보장) ====================

// syncDeadline 새 대기 상태(StateSeq 변경)가 열렸을 때만 마감을 다시 건다.
// 같은 단계에 스냅샷이 쌓이는 동안에는 처음 건 마감을 유지한다 (전원 공용 마감).
func (h *IDHub) syncDeadline(room *idRoom) {
	game := room.Game
	if room.DeadlineSeq == game.StateSeq {
		return
	}
	room.DeadlineSeq = game.StateSeq
	var dur time.Duration
	switch game.Phase {
	case IDPhaseQuestion:
		dur = idQuestionTimeout
	case IDPhaseDiscussion:
		dur = idDiscussionTimeout
	case IDPhaseVoting:
		dur = idVoteTimeout
	default:
		h.stopPhaseTimer(room)
		return
	}
	h.scheduleDeadline(room, dur)
}

// scheduleDeadline 마감 타이머 예약. AfkSeq 를 올려 지나간 발화를 구분하고,
// 발화는 허브 채널을 경유하므로 허브 밖에서 상태를 만지지 않는다.
func (h *IDHub) scheduleDeadline(room *idRoom, dur time.Duration) {
	h.stopPhaseTimer(room)
	room.Game.AfkSeq++
	room.Game.Deadline = time.Now().Add(dur).UnixMilli()
	sig := idPhaseSignal{GameID: room.Game.ID, Seq: room.Game.AfkSeq}
	room.PhaseTimer = time.AfterFunc(dur, func() {
		h.phaseFired <- sig
	})
}

func (h *IDHub) stopPhaseTimer(room *idRoom) {
	if room.PhaseTimer != nil {
		room.PhaseTimer.Stop()
		room.PhaseTimer = nil
	}
}

// handlePhaseFired 마감 발화 — 단계별 자동 행동으로 해소한다.
//   - question: [정답 나옴] 없이 5분 경과 — 전원 패배 종료
//   - discussion: 토론 마감 — 자동 투표 개시
//   - voting: 미제출 좌석 무작위 투표 후 개표
func (h *IDHub) handlePhaseFired(sig idPhaseSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.Game.AfkSeq != sig.Seq {
		return
	}
	game := room.Game
	switch game.Phase {
	case IDPhaseQuestion:
		h.broadcastEvent(room, IDEventPayload{Kind: "afk",
			Message: "질문 시간이 초과됐습니다 — 정답을 맞히지 못해 자동으로 종료합니다"})
		game.ForceQuestionTimeout()
		log.Printf("[인사이더][자동진행] game=%s | 질문 타임 초과 — 전원 패배 종료", game.ID)

	case IDPhaseDiscussion:
		h.broadcastEvent(room, IDEventPayload{Kind: "afk",
			Message: "토론 시간이 끝나 자동으로 투표를 시작합니다"})
		game.ForceOpenVote()
		log.Printf("[인사이더][자동진행] game=%s | 토론 마감 — 자동 투표 개시", game.ID)

	case IDPhaseVoting:
		h.broadcastEvent(room, IDEventPayload{Kind: "afk",
			Message: "투표하지 않은 좌석은 무작위로 투표 처리합니다"})
		game.ForceVoteDeadline(h.rng)
		log.Printf("[인사이더][자동진행] game=%s | 투표 마감 — 무작위 투표 개표", game.ID)

	default:
		return
	}
	h.afterProgress(room)
}

// ==================== 상태 뷰 (은닉의 핵심) ====================

// buildIDState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를
// 쓴다. 은닉: yourRole 은 본인에게만, word 는 마스터·인사이더에게만 실린다 —
// 그 외·관전자(viewerSeat -1)는 raw JSON 에 필드 자체가 없다 (빈 문자열 생략).
// masterSeat 은 전원 공개, 역할·득표는 종료 후에만 players 에 실린다.
func (h *IDHub) buildIDState(room *idRoom, viewerSeat int) IDGameStatePayload {
	game := room.Game

	yourRole := ""
	if viewerSeat >= 0 && viewerSeat < len(game.Players) {
		yourRole = string(game.Players[viewerSeat].Role) // 시작 전엔 "" → 필드 생략
	}
	word := ""
	if game.Ready {
		if game.Phase == IDPhaseGameOver ||
			viewerSeat == game.MasterSeat ||
			(viewerSeat >= 0 && viewerSeat == game.InsiderSeat) {
			word = game.Word
		}
	}

	revealed := game.Phase == IDPhaseGameOver
	players := []IDPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		role := ""
		votes := 0
		if revealed {
			role = string(p.Role)
			votes = p.Votes
		}
		players = append(players, IDPlayerView{
			Seat:      p.Seat,
			Name:      p.Name,
			Connected: c != nil && c.Connected,
			Bot:       c != nil && c.Bot,
			Voted:     p.Voted(),
			Role:      role,
			Votes:     votes,
		})
	}

	endsAt := int64(0)
	switch game.Phase {
	case IDPhaseQuestion, IDPhaseDiscussion, IDPhaseVoting:
		endsAt = game.Deadline
	}

	return IDGameStatePayload{
		GameID:      game.ID,
		RoomCode:    room.Code,
		Phase:       game.Phase,
		HostSeat:    h.hostSeat(room),
		YourSeat:    viewerSeat,
		Spectators:  len(room.Spectators),
		EndsAt:      endsAt,
		MasterSeat:  game.MasterSeat,
		YourRole:    yourRole,
		Word:        word,
		CorrectSeat: -1, // 예약 — 정답 외친 사람은 앱이 모른다
		Players:     players,
		Result:      game.Result,
	}
}

// broadcastState 좌석마다 개인화 스냅샷을 만들어 보낸다.
// 관전자에게는 공개 정보만 담긴 스냅샷(viewerSeat -1)이 간다.
func (h *IDHub) broadcastState(room *idRoom) {
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, IDMessage{
			Type:    IDMsgGameState,
			Payload: h.buildIDState(room, seat),
		})
	}
	if len(room.Spectators) > 0 {
		msg := IDMessage{Type: IDMsgGameState, Payload: h.buildIDState(room, -1)}
		for c := range room.Spectators {
			h.sendToClient(c, msg)
		}
	}
}

func (h *IDHub) broadcastEvent(room *idRoom, event IDEventPayload) {
	h.broadcastToRoom(room, IDMessage{Type: IDMsgEvent, Payload: event})
}

// finishGame 게임 종료 발표·전적 기록·방 정리 (재대결은 같은 방 코드
// 재입장으로 — finishGame 이 코드를 반납해 같은 코드로 새 방을 만들 수 있다)
func (h *IDHub) finishGame(room *idRoom) {
	game := room.Game
	h.stopPhaseTimer(room)
	game.Deadline = 0

	result := game.Result
	if result == nil { // 방어선 — 종료는 항상 결과와 함께 온다
		result = &IDResult{InsiderSeat: game.InsiderSeat, TopSeat: -1, Winner: "none",
			Message: "게임이 종료됐습니다"}
	}
	insiderName := ""
	if game.InsiderSeat >= 0 && game.InsiderSeat < len(game.Players) {
		insiderName = game.Players[game.InsiderSeat].Name
	}
	recordWinner := ""
	switch result.Winner {
	case "citizens":
		recordWinner = "일반인팀"
	case "insider":
		recordWinner = displayName(insiderName)
	}
	names := []string{}
	for _, p := range game.Players {
		names = append(names, displayName(p.Name))
	}

	insiderSeat := game.InsiderSeat
	h.broadcastEvent(room, IDEventPayload{Kind: "game_over", Seat: &insiderSeat,
		Name: insiderName,
		Message: fmt.Sprintf("게임 종료 — 인사이더는 %s님, 테마는 [%s]였습니다. %s",
			insiderName, game.Word, result.Message)})
	// 최종 스냅샷(역할·제시어 전원 공개)을 먼저 보낸 뒤 종료를 알린다
	// (봇 러너는 id_game_over 를 보고 스스로 끝난다)
	h.broadcastState(room)
	h.broadcastToRoom(room, IDMessage{
		Type: IDMsgGameOver,
		Payload: IDGameOverPayload{
			Winner:      result.Winner,
			InsiderSeat: game.InsiderSeat,
			MasterSeat:  game.MasterSeat,
			TopSeat:     result.TopSeat,
			Word:        game.Word,
			Players:     h.buildIDState(room, -1).Players,
		},
	})

	log.Printf("[인사이더][경기결과] game=%s | 승자=%s | 인사이더=seat%d(%s) | %s | 소요=%s",
		game.ID, result.Winner, game.InsiderSeat, displayName(insiderName),
		game.EndReason, matchDuration(game.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "insider",
		Players:  strings.Join(names, " vs "),
		Winner:   recordWinner,
		Reason:   game.EndReason,
		Duration: matchSeconds(game.StartedAt),
		Bot:      idRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
	if room.Code != "" {
		delete(h.activeCodes, room.Code) // 코드 재사용 복귀
	}
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *IDHub) handleDisconnect(client *IDClient) {
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
	log.Printf("[인사이더][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, IDMessage{
		Type: IDMsgPlayerDisconnected,
		Payload: IDPlayerDisconnectedPayload{
			Seat:         client.Seat,
			Name:         client.Name,
			GraceSeconds: int(h.grace.Seconds()),
		},
	})
	h.broadcastState(room)

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 안에 재접속하지 않은 좌석은 연습봇으로 대체하고
// 게임은 계속한다 — 마스터 선언·투표가 이탈 좌석에 막히지 않는 근거
func (h *IDHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == IDPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[인사이더][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := h.takeoverIDBot(room, seat, client.Name)
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.broadcastEvent(room, IDEventPayload{Kind: "bot_takeover", Seat: &seat,
		Name:    client.Name,
		Message: fmt.Sprintf("%s님 자리를 봇이 이어받았습니다", client.Name)})
	// 새 봇이 이 스냅샷을 받아 응답이 남았으면 즉시 이어간다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *IDHub) handleRejoin(client *IDClient, msg IDMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload IDRejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, IDMessage{Type: IDMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, IDMessage{Type: IDMsgSessionExpired})
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

	log.Printf("[인사이더][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, IDMessage{
		Type:    IDMsgPlayerReconnected,
		Payload: IDPlayerReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	// 전원에게 접속 상태가 반영된 스냅샷 (재접속 당사자 복원 포함)
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *IDHub) clearGameSessions(room *idRoom) {
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		h.drop(c.SessionID)
	}
}

// ==================== 전송 ====================

func (h *IDHub) sendError(client *IDClient, message string) {
	h.sendToClient(client, IDMessage{Type: IDMsgError, Payload: IDErrorPayload{Message: message}})
}

func (h *IDHub) sendToClient(client *IDClient, message IDMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[ID] ")
}

func (h *IDHub) broadcastToRoom(room *idRoom, message IDMessage) {
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

func ServeIDWs(hub *IDHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[ID] Error upgrading connection:", err)
		return
	}

	client := &IDClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[ID] ",
		func(msg IDMessage) { hub.gameMessage <- IDGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
