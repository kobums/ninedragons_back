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

// 저스트 원 대기 상태 마감 타이머 — 단서 60초 미제출은 빈 단서로,
// 추리 60초 무응답은 넘김으로, 인정 창 15초는 오답 확정으로 해소하고,
// 라운드 정산은 5초 뒤 자동으로 다음 라운드를 연다 (테스트에서 짧게 낮춘다).
var (
	joClueTimeout   = 60 * time.Second // clue      — 미제출은 빈 단서
	joGuessTimeout  = 60 * time.Second // guess     — 자동 넘김
	joAcceptWindow  = 15 * time.Second // judging   — 인정 창
	joRoundEndDelay = 5 * time.Second  // round_end — 다음 라운드
)

// joFailWinnerTag 협력 실패 기록의 Winner 표기.
//
// 전적 장부(stats.go)는 Winner == "" 를 무승부로 집계한다. 저스트 원은 협력
// 게임이라 실패에 "이긴 사람"이 없지만 무승부도 아니므로, 어떤 닉네임과도
// 겹치지 않는 표식을 넣어 참가자 전원이 패배로 집계되게 한다.
// 성공(총점이 라운드 수의 절반 이상)일 때는 반대로 전원 닉네임이 Winner 에
// 들어간다 (전원 승자).
const joFailWinnerTag = "합작 실패"

// joRoom 게임(순수 상태)과 좌석별 연결의 매핑
type joRoom struct {
	Game       *JOGame
	Clients    map[int]*JOClient // seat → client
	PhaseTimer *time.Timer       // 대기 상태 마감 타이머

	// DeadlineSeq 마지막으로 마감을 건 StateSeq — 같은 대기 상태에 스냅샷이
	// 쌓일 때마다(단서 제출·관전 입장 등) 마감이 늘어나지 않게 하는 근거
	DeadlineSeq int

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string

	// Spectators 관전자 연결 (사설 방 코드로만 진입, 상한 maxSpectators).
	// 좌석·세션이 없어 재접속을 지원하지 않는다 — 끊기면 목록에서만 뗀다.
	Spectators map[*JOClient]bool

	// LastReact 좌석별 마지막 리액션 발신 시각 (레이트리밋 장부)
	LastReact map[int]time.Time
}

// joPhaseSignal 마감 타이머의 발화 표식 — 대기 상태 일련번호(AfkSeq)로
// 지나간 발화를 구분한다
type joPhaseSignal struct {
	GameID string
	Seq    int
}

type JOHub struct {
	// 등록된 클라이언트
	clients map[*JOClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*joRoom

	// 글로벌 로비. 시작 전 공용 방은 하나뿐이다.
	lobby *joRoom

	// 사설 방 (초대 코드 → 시작 전 방). 허브 고루틴에서만 접근한다.
	// 시작하면 privateLobbies 에서 activeCodes 로 옮긴다.
	privateLobbies map[string]*joRoom

	// 진행 중 사설 방 (초대 코드 → gameID). 시작 후에도 코드로 방을 찾아
	// 관전 입장시키는 근거다. finishGame 에서 해제한다 (코드 재사용 복귀).
	activeCodes map[string]string

	// 클라이언트 등록
	register chan *JOClient

	// 클라이언트 등록 해제
	unregister chan *JOClient

	// 게임 메시지
	gameMessage chan JOGameMessage

	// 마감 타이머 발화 (time.AfterFunc → 허브 채널 경유)
	phaseFired chan joPhaseSignal

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*JOClient]

	// 제시어 추출·방 코드 발급용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type JOGameMessage struct {
	Client  *JOClient
	Message JOMessage
}

func NewJOHub() *JOHub {
	return &JOHub{
		register:       make(chan *JOClient),
		unregister:     make(chan *JOClient),
		clients:        make(map[*JOClient]bool),
		rooms:          make(map[string]*joRoom),
		privateLobbies: make(map[string]*joRoom),
		activeCodes:    make(map[string]string),
		gameMessage:    make(chan JOGameMessage),
		phaseFired:     make(chan joPhaseSignal, 8),
		sessionManager: newSessionManager[*JOClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *JOHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[JO] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[JO] Client unregistered: %s", client.ID)
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

func (h *JOHub) handleGameMessage(gm JOGameMessage) {
	// 관전자는 어떤 행동도 할 수 없다 (보기 전용 — 리액션도 좌석 보유자만)
	if h.isSpectator(gm.Client) {
		h.sendError(gm.Client, spectatorDeniedMsg)
		return
	}
	switch gm.Message.Type {
	case JOMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case JOMsgReact:
		h.handleReact(gm.Client, gm.Message)
	case JOMsgFillBots:
		h.handleFillBots(gm.Client)
	case JOMsgStart:
		h.handleStart(gm.Client)
	case JOMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case JOMsgClue:
		h.handleClue(gm.Client, gm.Message)
	case JOMsgGuess:
		h.handleGuess(gm.Client, gm.Message)
	case JOMsgPass:
		h.handlePass(gm.Client)
	case JOMsgAccept:
		h.handleAccept(gm.Client)
	}
}

// ==================== 대기실 ====================

func (h *JOHub) handleJoinGame(client *JOClient, msg JOMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload JOJoinGamePayload
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

	log.Printf("[저스트원][입장] game=%s | seat%d=%s 대기 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), JOMaxPlayers)
	if room.Code == "" { // 사설 방 입장은 조용히 (공용 로비만 알림)
		notify("저스트 원 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), len(room.Game.Players), JOMaxPlayers))
	}

	h.sendToClient(client, JOMessage{
		Type: JOMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
			"roomCode":  room.Code,
		},
	})

	h.broadcastEvent(room, JOEventPayload{Kind: "joined", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 입장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	// 정원이 차도 자동 시작하지 않는다 — 호스트 명시 시작만 (봇 채우기와 충돌 방지)
	h.broadcastState(room)
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다.
// 생략/빈 문자열은 기존 공용 로비 경로 그대로, "NEW"는 새 코드 발급,
// 그 외 코드는 해당 사설 방 (없으면 그 코드로 관대하게 새로 생성).
func (h *JOHub) lobbyRoomFor(roomField string) *joRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewJOGame(uuid.New().String())
			h.lobby = &joRoom{Game: game, Clients: map[int]*JOClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[JO] Created lobby game %s", game.ID)
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
		game := NewJOGame(uuid.New().String())
		room = &joRoom{Game: game, Clients: map[int]*JOClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[JO] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// ==================== 관전 / 리액션 ====================

// addSpectator 진행 중(또는 가득 찬) 사설 방에 관전자로 등록한다.
// 좌석·세션 없음 — 재접속 미지원, 끊기면 다시 코드로 들어온다.
func (h *JOHub) addSpectator(room *joRoom, client *JOClient, name string) {
	if len(room.Spectators) >= maxSpectators {
		h.sendError(client, spectatorFullMsg)
		return
	}
	if room.Spectators == nil {
		room.Spectators = map[*JOClient]bool{}
	}
	client.Name = name
	client.GameID = room.Game.ID
	room.Spectators[client] = true

	log.Printf("[저스트원][관전] game=%s | %s 관전 입장 (%d명)",
		room.Game.ID, displayName(name), len(room.Spectators))

	h.sendToClient(client, JOMessage{
		Type:    JOMsgSpectateJoined,
		Payload: map[string]interface{}{"gameId": room.Game.ID, "roomCode": room.Code},
	})
	// 전원(관전자 포함)에게 관전자 수가 반영된 스냅샷 — 신규 관전자의 첫 스냅샷 겸용
	h.broadcastState(room)
}

// isSpectator 관전자 연결인지 (좌석 보유자·미입장 연결은 false)
func (h *JOHub) isSpectator(client *JOClient) bool {
	if client.GameID == "" {
		return false
	}
	room := h.rooms[client.GameID]
	return room != nil && room.Spectators[client]
}

// handleReact 리액션 이모지 — 좌석 보유자만, waiting 중에도 허용.
// 화이트리스트 외·레이트리밋 초과는 조용히 무시한다 (상태 저장 없음).
func (h *JOHub) handleReact(client *JOClient, msg JOMessage) {
	room := h.rooms[client.GameID]
	if room == nil || client.Seat < 0 {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload JOReactPayload
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
	h.broadcastEvent(room, JOEventPayload{Kind: "react", Seat: &seat, Name: client.Name,
		Message: payload.Emoji})
}

// waitingRoomOf 클라이언트가 속한 시작 전 방 (공용 로비 또는 사설 방)
func (h *JOHub) waitingRoomOf(client *JOClient) *joRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Ready {
		return nil
	}
	return room
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *JOHub) hostSeat(room *joRoom) int {
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

// joHumanCount 방의 사람 수
func joHumanCount(room *joRoom) int {
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
func (h *JOHub) updateLobbyWaiting(room *joRoom) {
	if room.Code != "" {
		return
	}
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && joHumanCount(room) >= 1
	lobbySetWaiting("justone", waiting)
}

// handleFillBots host 가 빈 좌석을 연습봇으로 4인까지 채운 뒤 즉시
// 시작한다 (스폰은 join 을 거치지 않으므로 명시 시작 호출).
func (h *JOHub) handleFillBots(client *JOClient) {
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
	for len(room.Game.Players) < JOFillBotTarget {
		botNo++
		if !h.spawnJOBot(room, fmt.Sprintf("%s%d", botName, botNo)) {
			break
		}
	}

	if room.Game.CanStart() {
		h.startGame(room)
		return
	}
	h.broadcastState(room)
}

func (h *JOHub) handleStart(client *JOClient) {
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
		h.sendError(client, fmt.Sprintf("%d명 이상 모여야 시작할 수 있습니다", JOMinPlayers))
		return
	}
	h.startGame(room)
}

func (h *JOHub) startGame(room *joRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	if room.Code != "" {
		// 시작한 사설 방의 코드는 진행 중 장부로 옮긴다 (관전 입장 근거)
		delete(h.privateLobbies, room.Code)
		h.activeCodes[room.Code] = room.Game.ID
	} else {
		h.lobby = nil // 시작한 방은 로비에서 떼어낸다
		lobbySetWaiting("justone", false)
	}

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	log.Printf("[저스트원][경기시작] game=%s | 인원=%d | 라운드=%d | 첫 출제자=seat%d | %v",
		room.Game.ID, len(room.Game.Players), room.Game.TotalRounds,
		room.Game.GuesserSeat, names)
	if !joRoomHasBot(room) { // 연습봇전은 운영자 알림을 억제한다
		notify("저스트 원 시작", fmt.Sprintf("%d인전 시작", len(room.Game.Players)))
	}

	h.broadcastEvent(room, JOEventPayload{Kind: "game_started",
		Message: fmt.Sprintf(
			"게임 시작 — %d인 협력전, 총 %d라운드입니다. 겹친 단서는 전부 지워지니 남들과 다른 단어를 노리세요",
			len(room.Game.Players), room.Game.TotalRounds)})
	h.afterProgress(room)
}

// removeFromLobby 대기실에서 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *JOHub) removeFromLobby(room *joRoom, client *JOClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	// 좌석 압축: oldSeat 뒤의 클라이언트를 한 칸씩 당긴다
	rebuilt := map[int]*JOClient{}
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

	log.Printf("[저스트원][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), JOMaxPlayers)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if joHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, JOMessage{Type: JOMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			lobbySetWaiting("justone", false)
		}
		return
	}

	h.broadcastEvent(room, JOEventPayload{Kind: "left", Name: client.Name,
		Message: fmt.Sprintf("%s님이 퇴장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *JOHub) roomOf(client *JOClient) *joRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

// handleClue 단서 제출 — 한 라운드 1회, 12자 제한을 순수 규칙이 검증한다
func (h *JOHub) handleClue(client *JOClient, msg JOMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload JOCluePayload
	json.Unmarshal(payloadBytes, &payload)

	game := room.Game
	if err := game.SubmitClue(client.Seat, payload.Text); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[저스트원][단서] game=%s | R%d seat%d=%s 제출 (%d/%d)",
		game.ID, game.Round, client.Seat, displayName(client.Name),
		game.SubmittedCount(), game.clueGiverCount())
	h.afterProgress(room)
}

// handleGuess 출제자의 답
func (h *JOHub) handleGuess(client *JOClient, msg JOMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload JOGuessPayload
	json.Unmarshal(payloadBytes, &payload)

	game := room.Game
	if err := game.SubmitGuess(client.Seat, payload.Text); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[저스트원][추리] game=%s | R%d seat%d=%s → '%s'",
		game.ID, game.Round, client.Seat, displayName(client.Name), game.Guess)
	h.afterProgress(room)
}

// handlePass 출제자가 넘긴다 (0점)
func (h *JOHub) handlePass(client *JOClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	game := room.Game
	if err := game.Pass(client.Seat); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[저스트원][추리] game=%s | R%d seat%d=%s 넘김",
		game.ID, game.Round, client.Seat, displayName(client.Name))
	h.afterProgress(room)
}

// handleAccept 오답 인정 — 출제자를 뺀 한 명이면 정답 처리된다
func (h *JOHub) handleAccept(client *JOClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	game := room.Game
	round := game.Round
	if err := game.Accept(client.Seat); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[저스트원][판정] game=%s | R%d seat%d=%s 정답 인정 (총점 %d/%d)",
		game.ID, round, client.Seat, displayName(client.Name), game.Score, game.TotalRounds)
	h.afterProgress(room)
}

// afterProgress 모든 게임 변이 뒤의 공통 마무리 — 이벤트 방송·종료 판정·
// 새 대기 상태의 마감 예약·스냅샷 방송을 한 번에 처리한다.
func (h *JOHub) afterProgress(room *joRoom) {
	h.drainEvents(room)
	if room.Game.Phase == JOPhaseGameOver {
		h.finishGame(room)
		return
	}
	h.syncDeadline(room)
	h.broadcastState(room)
}

// drainEvents 순수 규칙이 쌓은 이벤트를 jo_event 로 방송한다
func (h *JOHub) drainEvents(room *joRoom) {
	for _, ev := range room.Game.DrainEvents() {
		payload := JOEventPayload{Kind: ev.Kind, Message: ev.Message}
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
// 같은 단계에 단서 제출·관전 입장으로 스냅샷이 쌓여도 마감은 늘어나지 않는다.
func (h *JOHub) syncDeadline(room *joRoom) {
	game := room.Game
	if room.DeadlineSeq == game.StateSeq {
		return
	}
	room.DeadlineSeq = game.StateSeq
	var dur time.Duration
	switch game.Phase {
	case JOPhaseClue:
		dur = joClueTimeout
	case JOPhaseGuess:
		dur = joGuessTimeout
	case JOPhaseJudging:
		dur = joAcceptWindow
	case JOPhaseRoundEnd:
		dur = joRoundEndDelay
	default:
		h.stopPhaseTimer(room)
		return
	}
	h.scheduleDeadline(room, dur)
}

// scheduleDeadline 마감 타이머 예약. AfkSeq 를 올려 지나간 발화를 구분하고,
// 발화는 허브 채널을 경유하므로 허브 밖에서 상태를 만지지 않는다.
func (h *JOHub) scheduleDeadline(room *joRoom, dur time.Duration) {
	h.stopPhaseTimer(room)
	room.Game.AfkSeq++
	room.Game.Deadline = time.Now().Add(dur).UnixMilli()
	sig := joPhaseSignal{GameID: room.Game.ID, Seq: room.Game.AfkSeq}
	room.PhaseTimer = time.AfterFunc(dur, func() {
		h.phaseFired <- sig
	})
}

func (h *JOHub) stopPhaseTimer(room *joRoom) {
	if room.PhaseTimer != nil {
		room.PhaseTimer.Stop()
		room.PhaseTimer = nil
	}
}

// handlePhaseFired 마감 발화 — 단계별 자동 행동으로 해소한다.
//   - clue:      미제출은 빈 단서로 처리하고 소거·추리 단계로 넘긴다
//   - guess:     출제자 무응답은 넘김 (0점)
//   - judging:   아무도 인정하지 않았으므로 오답 확정 (-1)
//   - round_end: 다음 라운드 개시 (마지막 라운드였으면 종료)
func (h *JOHub) handlePhaseFired(sig joPhaseSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.Game.AfkSeq != sig.Seq {
		return
	}
	game := room.Game
	switch game.Phase {
	case JOPhaseClue:
		missing := []string{}
		for _, p := range game.Players {
			if p.Seat != game.GuesserSeat && !p.Submitted {
				missing = append(missing, p.Name)
			}
		}
		if len(missing) > 0 {
			h.broadcastEvent(room, JOEventPayload{Kind: "afk",
				Message: fmt.Sprintf("%s님이 오래 응답하지 않아 빈 단서로 처리합니다",
					strings.Join(missing, "·"))})
		}
		game.ForceCloseClues()
		log.Printf("[저스트원][자동진행] game=%s | R%d 단서 마감 — 미제출 %d명",
			game.ID, game.Round, len(missing))

	case JOPhaseGuess:
		seat := game.GuesserSeat
		if seat < 0 || seat >= len(game.Players) {
			return
		}
		guesser := game.Players[seat]
		h.broadcastEvent(room, JOEventPayload{Kind: "afk", Seat: &seat, Name: guesser.Name,
			Message: fmt.Sprintf("%s님이 오래 응답하지 않아 자동으로 넘깁니다", guesser.Name)})
		game.ForcePass()
		log.Printf("[저스트원][자동진행] game=%s | R%d seat%d 무응답 — 자동 넘김",
			game.ID, game.Round, seat)

	case JOPhaseJudging:
		game.CloseJudging()
		log.Printf("[저스트원][판정] game=%s | R%d 인정 없음 — 오답 (총점 %d/%d)",
			game.ID, game.Round, game.Score, game.TotalRounds)

	case JOPhaseRoundEnd:
		game.NextRound()
		if game.Phase == JOPhaseClue {
			log.Printf("[저스트원][라운드] game=%s | %d/%d 라운드 시작 (출제자 seat%d)",
				game.ID, game.Round, game.TotalRounds, game.GuesserSeat)
		}

	default:
		return
	}
	h.afterProgress(room)
}

// ==================== 상태 뷰 (은닉의 핵심) ====================

// buildJOState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
func (h *JOHub) buildJOState(room *joRoom, viewerSeat int) JOGameStatePayload {
	return h.buildJOStateFor(room, viewerSeat, false)
}

// buildJOStateFor 개인화 게임 스냅샷의 본체.
//
// 은닉:
//   - word 는 단서 제공자에게만 실린다 — 출제자·관전자(viewerSeat -1)의 raw
//     JSON 에는 키 자체가 없다 (nil 포인터 생략). 끝난 라운드의 제시어는
//     history 로 전원 공개되므로 이 불변식은 게임 내내 유지된다.
//   - yourClue 는 본인에게만 실린다 (미제출은 "" — 빈 문자열도 실려야 하므로
//     문자열 포인터로 부재를 표현한다).
//   - clues 는 단서 단계에 항상 빈 배열이라 남의 단서가 새어 나가지 않는다.
//     추리·인정 단계에는 살아남은 단서만, 판정이 끝난 round_end 부터 소거된
//     단서까지 함께 나간다.
//
// forBot 은 연습봇 좌석 전용 예외다 — 출제자 봇도 제시어를 받아야 "40% 정답"
// 이라는 사람 실력 시뮬레이션을 흉내 낼 수 있다(jo_bot.go 머리말). 사람 좌석과
// 관전자에게는 어떤 경우에도 켜지지 않는다.
func (h *JOHub) buildJOStateFor(room *joRoom, viewerSeat int, forBot bool) JOGameStatePayload {
	game := room.Game
	seated := viewerSeat >= 0 && viewerSeat < len(game.Players)
	inRound := game.Ready && game.Phase != JOPhaseWaiting && game.Phase != JOPhaseGameOver

	var word *string
	var yourClue *string
	if seated && inRound {
		if viewerSeat != game.GuesserSeat || forBot {
			w := game.Word
			word = &w
		}
		if viewerSeat != game.GuesserSeat {
			c := game.Players[viewerSeat].Clue
			yourClue = &c
		}
	}

	players := []JOPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		players = append(players, JOPlayerView{
			Seat:      p.Seat,
			Name:      p.Name,
			Connected: c != nil && c.Connected,
			Bot:       c != nil && c.Bot,
			Submitted: p.Submitted,
			IsGuesser: game.Ready && p.Seat == game.GuesserSeat,
		})
	}

	endsAt := int64(0)
	switch game.Phase {
	case JOPhaseClue, JOPhaseGuess, JOPhaseJudging, JOPhaseRoundEnd:
		endsAt = game.Deadline
	}

	return JOGameStatePayload{
		GameID:         game.ID,
		RoomCode:       room.Code,
		Phase:          game.Phase,
		HostSeat:       h.hostSeat(room),
		YourSeat:       viewerSeat,
		Spectators:     len(room.Spectators),
		EndsAt:         endsAt,
		Round:          game.Round,
		TotalRounds:    game.TotalRounds,
		GuesserSeat:    game.GuesserSeat,
		Score:          game.Score,
		Word:           word,
		YourClue:       yourClue,
		Clues:          joVisibleClues(game),
		SubmittedCount: game.SubmittedCount(),
		Guess:          game.Guess,
		Judged:         game.Judged,
		Players:        players,
		History:        append([]JOHistoryEntry{}, game.History...),
	}
}

// joVisibleClues 단계별로 공개할 단서 목록 — 전원에게 같은 값이 간다.
// 단서 단계에는 빈 배열, 추리·인정 단계에는 살아남은 단서만,
// 판정이 끝난 뒤에는 소거된 단서까지 함께 (취소선 연출용).
func joVisibleClues(game *JOGame) []JOClueView {
	switch game.Phase {
	case JOPhaseGuess, JOPhaseJudging:
		return joSurvivors(game.Clues)
	case JOPhaseRoundEnd, JOPhaseGameOver:
		return append([]JOClueView{}, game.Clues...)
	default:
		return []JOClueView{}
	}
}

// broadcastState 좌석마다 개인화 스냅샷을 만들어 보낸다.
// 관전자에게는 공개 정보만 담긴 스냅샷(viewerSeat -1)이 간다.
func (h *JOHub) broadcastState(room *joRoom) {
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, JOMessage{
			Type:    JOMsgGameState,
			Payload: h.buildJOStateFor(room, seat, c.Bot),
		})
	}
	if len(room.Spectators) > 0 {
		msg := JOMessage{Type: JOMsgGameState, Payload: h.buildJOState(room, -1)}
		for c := range room.Spectators {
			h.sendToClient(c, msg)
		}
	}
}

func (h *JOHub) broadcastEvent(room *joRoom, event JOEventPayload) {
	h.broadcastToRoom(room, JOMessage{Type: JOMsgEvent, Payload: event})
}

// finishGame 게임 종료 발표·전적 기록·방 정리 (재대결은 같은 방 코드
// 재입장으로 — finishGame 이 코드를 반납해 같은 코드로 새 방을 만들 수 있다).
//
// 협력 게임이라 전적은 진영전과 다르게 적는다 — 총점이 라운드 수의 절반
// 이상이면 참가자 전원이 Winner 에 들어가고(전원 승자), 그 미만이면 어떤
// 닉네임과도 겹치지 않는 joFailWinnerTag 를 넣어 전원 패자로 집계한다
// (Winner "" 는 무승부다).
func (h *JOHub) finishGame(room *joRoom) {
	game := room.Game
	h.stopPhaseTimer(room)
	game.Deadline = 0

	cleared := joSuccess(game.Score, game.TotalRounds)
	grade := joGrade(game.Score, game.TotalRounds)
	message := joGradeMessage(game.Score, game.TotalRounds)

	team := []string{}
	for _, p := range game.Players {
		team = append(team, displayName(p.Name))
	}
	reason := "low_score"
	winner := joFailWinnerTag
	if cleared {
		reason = "cleared"
		winner = strings.Join(team, "·") // 전원 승자
	}

	h.broadcastEvent(room, JOEventPayload{Kind: "game_over",
		Message: fmt.Sprintf("게임 종료 — 총점 %d/%d. %s", game.Score, game.TotalRounds, message)})
	// 최종 스냅샷을 먼저 보낸 뒤 종료를 알린다
	// (봇 러너는 jo_game_over 를 보고 스스로 끝난다)
	h.broadcastState(room)
	h.broadcastToRoom(room, JOMessage{
		Type: JOMsgGameOver,
		Payload: JOGameOverPayload{
			Cleared:     cleared,
			Score:       game.Score,
			TotalRounds: game.TotalRounds,
			Grade:       grade,
			Message:     message,
			History:     append([]JOHistoryEntry{}, game.History...),
			Players:     h.buildJOState(room, -1).Players,
		},
	})

	log.Printf("[저스트원][경기결과] game=%s | 성공=%t | 등급=%s | 총점=%d/%d | 소요=%s",
		game.ID, cleared, grade, game.Score, game.TotalRounds, matchDuration(game.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "justone",
		Players:  strings.Join(team, "·"), // 협력전 — 진영 구분자 없이 한 팀
		Winner:   winner,
		Reason:   reason,
		Duration: matchSeconds(game.StartedAt),
		Bot:      joRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
	if room.Code != "" {
		delete(h.activeCodes, room.Code) // 코드 재사용 복귀
	}
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *JOHub) handleDisconnect(client *JOClient) {
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
	log.Printf("[저스트원][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, JOMessage{
		Type: JOMsgPlayerDisconnected,
		Payload: JOPlayerDisconnectedPayload{
			Seat:         client.Seat,
			Name:         client.Name,
			GraceSeconds: int(h.grace.Seconds()),
		},
	})
	h.broadcastState(room)

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 안에 재접속하지 않은 좌석은 연습봇으로 대체하고
// 게임은 계속한다 — 라운드가 이탈 좌석에 막히지 않는 근거
func (h *JOHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == JOPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[저스트원][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := h.takeoverJOBot(room, seat, client.Name)
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.broadcastEvent(room, JOEventPayload{Kind: "bot_takeover", Seat: &seat,
		Name:    client.Name,
		Message: fmt.Sprintf("%s님 자리를 봇이 이어받았습니다", client.Name)})
	// 새 봇이 이 스냅샷을 받아 자기 차례면 즉시 이어간다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *JOHub) handleRejoin(client *JOClient, msg JOMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload JORejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, JOMessage{Type: JOMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, JOMessage{Type: JOMsgSessionExpired})
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

	log.Printf("[저스트원][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, JOMessage{
		Type:    JOMsgPlayerReconnected,
		Payload: JOPlayerReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	// 전원에게 접속 상태가 반영된 스냅샷 (재접속 당사자의 제시어·단서 복원 포함)
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *JOHub) clearGameSessions(room *joRoom) {
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		h.drop(c.SessionID)
	}
}

// ==================== 전송 ====================

func (h *JOHub) sendError(client *JOClient, message string) {
	h.sendToClient(client, JOMessage{Type: JOMsgError, Payload: JOErrorPayload{Message: message}})
}

func (h *JOHub) sendToClient(client *JOClient, message JOMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[JO] ")
}

func (h *JOHub) broadcastToRoom(room *joRoom, message JOMessage) {
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

func ServeJOWs(hub *JOHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[JO] Error upgrading connection:", err)
		return
	}

	client := &JOClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[JO] ",
		func(msg JOMessage) { hub.gameMessage <- JOGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
