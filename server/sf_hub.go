package server

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// sfPhaseDelay day_result·execution 발표 화면에서 다음 단계로 자동 진행하기까지의
// 시간 (테스트에서 짧게 낮춘다)
var sfPhaseDelay = 5 * time.Second

// 밤 행동·낮 투표의 제한 시간 — 접속만 유지한 채 제출하지 않는 좌석이
// 게임을 영구 정지시키지 않게, 만료 시 제출된 것만으로 해소한다
// (밤: 미제출 마피아 표 없이 다수결·무표면 조용한 밤 / 투표: 미제출 = 기권)
var (
	sfNightTimeout = 2 * time.Minute
	sfVoteTimeout  = 3 * time.Minute
)

// sfRoom 게임(순수 상태)과 좌석별 연결의 매핑
type sfRoom struct {
	Game       *SFGame
	Clients    map[int]*SFClient // seat → client
	PhaseTimer *time.Timer       // day_result/execution 자동 진행 타이머

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string

	// Spectators 관전자 연결 (사설 방 코드로만 진입, 상한 maxSpectators).
	// 좌석·세션이 없어 재접속을 지원하지 않는다 — 끊기면 목록에서만 뗀다.
	Spectators map[*SFClient]bool

	// LastReact 좌석별 마지막 리액션 발신 시각 (레이트리밋 장부)
	LastReact map[int]time.Time
}

// sfPhaseSignal 자동 진행 타이머의 발화 표식. 발화 시점의 (단계, 일차)가
// 현재와 다르면 이미 지나간 신호로 보고 무시한다.
type sfPhaseSignal struct {
	GameID string
	Phase  SFPhase
	DayNo  int
}

type SFHub struct {
	// 등록된 클라이언트
	clients map[*SFClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*sfRoom

	// 글로벌 로비. 시작 전 공용 방은 하나뿐이다.
	lobby *sfRoom

	// 사설 방 (초대 코드 → 시작 전 방). 허브 고루틴에서만 접근한다.
	// 시작하면 privateLobbies 에서 activeCodes 로 옮긴다.
	privateLobbies map[string]*sfRoom

	// 진행 중 사설 방 (초대 코드 → gameID). 시작 후에도 코드로 방을 찾아
	// 관전 입장시키는 근거다. finishGame 에서 해제한다 (코드 재사용 복귀).
	activeCodes map[string]string

	// 클라이언트 등록
	register chan *SFClient

	// 클라이언트 등록 해제
	unregister chan *SFClient

	// 게임 메시지
	gameMessage chan SFGameMessage

	// day_result/execution 자동 진행 (time.AfterFunc → 허브 채널 경유)
	phaseFired chan sfPhaseSignal

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*SFClient]

	// 셔플·동률 해소용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type SFGameMessage struct {
	Client  *SFClient
	Message SFMessage
}

func NewSFHub() *SFHub {
	return &SFHub{
		register:       make(chan *SFClient),
		unregister:     make(chan *SFClient),
		clients:        make(map[*SFClient]bool),
		rooms:          make(map[string]*sfRoom),
		privateLobbies: make(map[string]*sfRoom),
		activeCodes:    make(map[string]string),
		gameMessage:    make(chan SFGameMessage),
		phaseFired:     make(chan sfPhaseSignal, 8),
		sessionManager: newSessionManager[*SFClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *SFHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[SF] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[SF] Client unregistered: %s", client.ID)
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

func (h *SFHub) handleGameMessage(gm SFGameMessage) {
	// 관전자는 어떤 행동도 할 수 없다 (보기 전용 — 리액션도 좌석 보유자만)
	if h.isSpectator(gm.Client) {
		h.sendError(gm.Client, spectatorDeniedMsg)
		return
	}
	switch gm.Message.Type {
	case SFMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case SFMsgReact:
		h.handleReact(gm.Client, gm.Message)
	case SFMsgFillBots:
		h.handleFillBots(gm.Client)
	case SFMsgStart:
		h.handleStart(gm.Client)
	case SFMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case SFMsgNightAction:
		h.handleNightAction(gm.Client, gm.Message)
	case SFMsgVote:
		h.handleVote(gm.Client, gm.Message)
	}
}

// ==================== 로비 ====================

func (h *SFHub) handleJoinGame(client *SFClient, msg SFMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SFJoinGamePayload
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

	log.Printf("[마피아][입장] game=%s | seat%d=%s 게임 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), SFMaxPlayers)
	if room.Code == "" { // 사설 방 입장은 조용히 (공용 로비만 알림)
		notify("마피아 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), len(room.Game.Players), SFMaxPlayers))
	}

	h.sendToClient(client, SFMessage{
		Type: SFMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
			"roomCode":  room.Code,
		},
	})

	h.broadcastEvent(room, SFEventPayload{Kind: "joined", Seat: &seat, Name: client.Name})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다.
// 생략/빈 문자열은 기존 공용 로비 경로 그대로, "NEW"는 새 코드 발급,
// 그 외 코드는 해당 사설 방 (없으면 그 코드로 관대하게 새로 생성).
func (h *SFHub) lobbyRoomFor(roomField string) *sfRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewSFGame(uuid.New().String())
			h.lobby = &sfRoom{Game: game, Clients: map[int]*SFClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[SF] Created lobby game %s", game.ID)
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
		game := NewSFGame(uuid.New().String())
		room = &sfRoom{Game: game, Clients: map[int]*SFClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[SF] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// ==================== 관전 / 리액션 ====================

// addSpectator 진행 중(또는 가득 찬) 사설 방에 관전자로 등록한다.
// 좌석·세션 없음 — 재접속 미지원, 끊기면 다시 코드로 들어온다.
func (h *SFHub) addSpectator(room *sfRoom, client *SFClient, name string) {
	if len(room.Spectators) >= maxSpectators {
		h.sendError(client, spectatorFullMsg)
		return
	}
	if room.Spectators == nil {
		room.Spectators = map[*SFClient]bool{}
	}
	client.Name = name
	client.GameID = room.Game.ID
	room.Spectators[client] = true

	log.Printf("[마피아][관전] game=%s | %s 관전 입장 (%d명)",
		room.Game.ID, displayName(name), len(room.Spectators))

	h.sendToClient(client, SFMessage{
		Type:    SFMsgSpectateJoined,
		Payload: map[string]interface{}{"gameId": room.Game.ID, "roomCode": room.Code},
	})
	// 전원(관전자 포함)에게 관전자 수가 반영된 스냅샷 — 신규 관전자의 첫 스냅샷 겸용
	h.broadcastState(room)
}

// isSpectator 관전자 연결인지 (좌석 보유자·미입장 연결은 false)
func (h *SFHub) isSpectator(client *SFClient) bool {
	if client.GameID == "" {
		return false
	}
	room := h.rooms[client.GameID]
	return room != nil && room.Spectators[client]
}

// handleReact 리액션 이모지 — 좌석 보유자만, waiting 중에도 허용.
// 화이트리스트 외·레이트리밋 초과는 조용히 무시한다 (상태 저장 없음).
func (h *SFHub) handleReact(client *SFClient, msg SFMessage) {
	room := h.rooms[client.GameID]
	if room == nil || client.Seat < 0 {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SFReactPayload
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
	h.broadcastEvent(room, SFEventPayload{Kind: "react", Seat: &seat, Name: client.Name, Message: payload.Emoji})
}

// waitingRoomOf 클라이언트가 속한 시작 전 방 (공용 로비 또는 사설 방)
func (h *SFHub) waitingRoomOf(client *SFClient) *sfRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Ready {
		return nil
	}
	return room
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *SFHub) hostSeat(room *sfRoom) int {
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

// handleFillBots host 가 최소 성립 인원(6)까지 연습봇으로 채운다.
// 봇은 연습용이라 정원(10)까지 채우지 않는다. 시작은 별도의 sf_start.
func (h *SFHub) handleFillBots(client *SFClient) {
	room := h.waitingRoomOf(client)
	if room == nil {
		h.sendError(client, "로비를 찾을 수 없습니다")
		return
	}
	if client.Seat != h.hostSeat(room) {
		h.sendError(client, "호스트만 봇을 채울 수 있습니다")
		return
	}
	if len(room.Game.Players) >= SFBotFillTarget {
		h.sendError(client, fmt.Sprintf("%d명 미만일 때만 봇을 채울 수 있습니다", SFBotFillTarget))
		return
	}

	botNo := 0
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			botNo++
		}
	}
	for len(room.Game.Players) < SFBotFillTarget {
		botNo++
		if h.spawnBot(room, fmt.Sprintf("%s%d", botName, botNo)) == nil {
			break
		}
	}
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

func (h *SFHub) handleStart(client *SFClient) {
	room := h.waitingRoomOf(client)
	if room == nil {
		h.sendError(client, "로비를 찾을 수 없습니다")
		return
	}
	if client.Seat != h.hostSeat(room) {
		h.sendError(client, "호스트만 시작할 수 있습니다")
		return
	}
	if !room.Game.CanStart() {
		h.sendError(client, fmt.Sprintf("%d명 이상 모여야 시작할 수 있습니다", SFMinPlayers))
		return
	}
	h.startGame(room)
}

// sfHumanCount 방의 사람 수
func sfHumanCount(room *sfRoom) int {
	n := 0
	for _, c := range room.Clients {
		if c != nil && !c.Bot {
			n++
		}
	}
	return n
}

// sfRoomHasBot 방에 연습봇이 있는지 (전적 기록용)
func sfRoomHasBot(room *sfRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}

// updateLobbyWaiting 로비 현황판 갱신 — 사람 1명 이상 대기 && 미시작.
// 사설 방은 현황판에 노출하지 않는다 (초대 링크로만 접근).
func (h *SFHub) updateLobbyWaiting(room *sfRoom) {
	if room.Code != "" {
		return
	}
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && sfHumanCount(room) >= 1
	lobbySetWaiting("skyfall", waiting)
}

func (h *SFHub) startGame(room *sfRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	if room.Code != "" {
		// 시작한 사설 방의 코드는 진행 중 장부로 옮긴다 (관전 입장 근거)
		delete(h.privateLobbies, room.Code)
		h.activeCodes[room.Code] = room.Game.ID
	} else {
		h.lobby = nil // 시작한 방은 로비에서 떼어낸다
		lobbySetWaiting("skyfall", false)
	}

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	log.Printf("[마피아][경기시작] game=%s | 인원=%d | %v",
		room.Game.ID, len(room.Game.Players), names)
	// 봇 채우기로 시작한 판도 포함해 시작 시점에 1회만 알린다
	notify("마피아 게임 시작", fmt.Sprintf("%d인전 시작", len(room.Game.Players)))

	h.broadcastEvent(room, SFEventPayload{Kind: "started"})
	h.broadcastEvent(room, SFEventPayload{Kind: "night_begin",
		Message: fmt.Sprintf("%d일차 밤이 시작되었습니다", room.Game.DayNo)})
	// 마감 시각을 먼저 세팅해야 첫 스냅샷에 endsAt 이 실린다
	h.scheduleDeadline(room, sfNightTimeout)
	h.broadcastState(room)
}

// removeFromLobby 대기 중 이탈 — 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *SFHub) removeFromLobby(room *sfRoom, client *SFClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	rebuilt := map[int]*SFClient{}
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

	log.Printf("[마피아][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), SFMaxPlayers)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if sfHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, SFMessage{Type: SFMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			lobbySetWaiting("skyfall", false)
		}
		return
	}

	h.broadcastEvent(room, SFEventPayload{Kind: "left", Name: client.Name})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *SFHub) roomOf(client *SFClient) *sfRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

func (h *SFHub) handleNightAction(client *SFClient, msg SFMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SFNightActionPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.SubmitNightAction(client.Seat, payload.Target); err != nil {
		h.sendError(client, err.Error())
		return
	}
	if room.Game.NightComplete() {
		h.resolveNight(room)
		return
	}
	h.broadcastState(room)
}

// resolveNight 밤 해소 — 결과 발표 후 5초 뒤 투표로 자동 진행
func (h *SFHub) resolveNight(room *sfRoom) {
	game := room.Game
	if room.PhaseTimer != nil {
		room.PhaseTimer.Stop()
	}
	game.Deadline = 0
	killed := game.ResolveNight(h.rng)

	if killed >= 0 {
		seat := killed
		msg := fmt.Sprintf("%s님이 살해당했습니다", game.Players[seat].Name)
		log.Printf("[마피아][밤해소] game=%s | %d일차 | seat%d=%s 살해",
			game.ID, game.DayNo, seat, displayName(game.Players[seat].Name))
		h.broadcastEvent(room, SFEventPayload{Kind: "day_result", Seat: &seat, Message: msg})
	} else {
		log.Printf("[마피아][밤해소] game=%s | %d일차 | 의사 세이브 — 사망자 없음",
			game.ID, game.DayNo)
		h.broadcastEvent(room, SFEventPayload{Kind: "day_result", Message: "아무도 죽지 않았습니다"})
	}

	h.broadcastState(room)
	if game.Phase == SFPhaseGameOver {
		h.finishGame(room)
		return
	}
	h.schedulePhase(room, SFPhaseDayResult)
}

func (h *SFHub) handleVote(client *SFClient, msg SFMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SFVotePayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.SubmitVote(client.Seat, payload.Target); err != nil {
		h.sendError(client, err.Error())
		return
	}
	if room.Game.VoteComplete() {
		h.resolveVotes(room)
		return
	}
	// 공개 투표 — 표가 쌓일 때마다 실시간 스냅샷
	h.broadcastState(room)
}

// resolveVotes 투표 집계 — 처형(역할 공개) 또는 무처형 발표 후 5초 뒤 밤으로
func (h *SFHub) resolveVotes(room *sfRoom) {
	game := room.Game
	if room.PhaseTimer != nil {
		room.PhaseTimer.Stop()
	}
	game.Deadline = 0
	game.ResolveVotes()

	if game.Execution != nil && game.Execution.Seat >= 0 {
		seat := game.Execution.Seat
		role := game.Players[seat].Role
		log.Printf("[마피아][처형] game=%s | %d일차 | seat%d=%s (%s)",
			game.ID, game.DayNo, seat, displayName(game.Players[seat].Name), sfRoleLabel(role))
		h.broadcastEvent(room, SFEventPayload{Kind: "executed", Seat: &seat,
			Message: fmt.Sprintf("%s님이 처형되었습니다 — %s", game.Players[seat].Name, sfRoleLabel(role))})
	} else {
		log.Printf("[마피아][무처형] game=%s | %d일차 | 동률·전원 기권", game.ID, game.DayNo)
		h.broadcastEvent(room, SFEventPayload{Kind: "no_execution",
			Message: "동률 또는 전원 기권 — 처형이 없습니다"})
	}

	h.broadcastState(room)
	if game.Phase == SFPhaseGameOver {
		h.finishGame(room)
		return
	}
	h.schedulePhase(room, SFPhaseExecution)
}

// ==================== 자동 진행 타이머 ====================

// schedulePhase 발표 단계의 자동 진행 타이머를 건다.
// 발화는 허브 채널을 경유하므로 허브 밖에서 상태를 만지지 않는다.
func (h *SFHub) schedulePhase(room *sfRoom, phase SFPhase) {
	sig := sfPhaseSignal{GameID: room.Game.ID, Phase: phase, DayNo: room.Game.DayNo}
	room.PhaseTimer = time.AfterFunc(sfPhaseDelay, func() {
		h.phaseFired <- sig
	})
}

// scheduleDeadline 밤 행동·낮 투표의 타임아웃 타이머 (같은 채널 경유)
func (h *SFHub) scheduleDeadline(room *sfRoom, dur time.Duration) {
	room.Game.Deadline = time.Now().Add(dur).UnixMilli()
	sig := sfPhaseSignal{GameID: room.Game.ID, Phase: room.Game.Phase, DayNo: room.Game.DayNo}
	room.PhaseTimer = time.AfterFunc(dur, func() {
		h.phaseFired <- sig
	})
}

func (h *SFHub) handlePhaseFired(sig sfPhaseSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.Game.Phase != sig.Phase || room.Game.DayNo != sig.DayNo {
		return
	}
	game := room.Game
	switch sig.Phase {
	case SFPhaseNight:
		// 밤 타임아웃 — 제출된 행동만으로 해소 (미제출 마피아 표는 빠진다)
		log.Printf("[마피아][밤마감] game=%s | %d일차 | 타임아웃 — 미제출 %d명",
			game.ID, game.DayNo, game.PendingNightCount())
		h.resolveNight(room)
	case SFPhaseDayVote:
		// 투표 타임아웃 — 미제출자는 기권으로 판정
		log.Printf("[마피아][투표마감] game=%s | %d일차 | 타임아웃 — %d표로 판정",
			game.ID, game.DayNo, len(game.Votes))
		h.resolveVotes(room)
	case SFPhaseDayResult:
		game.BeginVote()
		h.broadcastEvent(room, SFEventPayload{Kind: "vote_begin",
			Message: fmt.Sprintf("%d일차 투표를 시작합니다", game.DayNo)})
		h.scheduleDeadline(room, sfVoteTimeout)
		h.broadcastState(room)
	case SFPhaseExecution:
		game.BeginNight()
		h.broadcastEvent(room, SFEventPayload{Kind: "night_begin",
			Message: fmt.Sprintf("%d일차 밤이 시작되었습니다", game.DayNo)})
		h.scheduleDeadline(room, sfNightTimeout)
		h.broadcastState(room)
	}
}

// ==================== 상태 뷰 (은닉의 핵심) ====================

// sfPlayerViews 좌석별 공개 정보. role 은 처형·게임 종료로 공개된 경우에만
// 채운다 (게임 종료 시 finish 가 전원 RoleRevealed 처리).
func (h *SFHub) sfPlayerViews(room *sfRoom) []SFPlayerView {
	game := room.Game
	players := []SFPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		role := ""
		if p.RoleRevealed {
			role = string(p.Role)
		}
		players = append(players, SFPlayerView{
			Seat:      p.Seat,
			Name:      p.Name,
			Connected: c != nil && c.Connected,
			Bot:       c != nil && c.Bot,
			Alive:     p.Alive,
			Role:      role,
		})
	}
	return players
}

// buildSFState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
// 은닉: mafiaSeats 는 마피아에게만(그 외 필드 자체 생략), 타인 role 은 공개
// 전까지 빈 문자열, 경찰 조사 결과는 경찰에게만.
func (h *SFHub) buildSFState(room *sfRoom, viewerSeat int) SFGameStatePayload {
	game := room.Game

	yourRole := ""
	if game.Ready && viewerSeat >= 0 && viewerSeat < len(game.Players) {
		yourRole = string(game.Players[viewerSeat].Role)
	}

	var mafiaSeats []int
	if yourRole == string(SFRoleMafia) {
		mafiaSeats = game.MafiaSeats()
	}

	var night *SFNightView
	if game.Phase == SFPhaseNight {
		_, done := game.NightActions[viewerSeat]
		night = &SFNightView{YourActionDone: done, PendingCount: game.PendingNightCount()}
	}

	var investigation *SFInvestigationView
	if yourRole == string(SFRolePolice) &&
		viewerSeat >= 0 && viewerSeat < len(game.Players) && game.Players[viewerSeat].Alive {
		investigation = game.Investigation
	}

	var votes []SFVoteView
	if game.Votes != nil {
		votes = game.VoteViews()
	}

	endsAt := int64(0)
	if game.Phase == SFPhaseNight || game.Phase == SFPhaseDayVote {
		endsAt = game.Deadline
	}

	return SFGameStatePayload{
		GameID:        game.ID,
		RoomCode:      room.Code,
		Phase:         game.Phase,
		DayNo:         game.DayNo,
		EndsAt:        endsAt,
		HostSeat:      h.hostSeat(room),
		YourSeat:      viewerSeat,
		YourRole:      yourRole,
		MafiaSeats:    mafiaSeats,
		Players:       h.sfPlayerViews(room),
		Spectators:    len(room.Spectators),
		Night:         night,
		Investigation: investigation,
		Announcement:  game.Announcement,
		Votes:         votes,
		Execution:     game.Execution,
		Winner:        game.Winner,
	}
}

// broadcastState 좌석마다 개인화 스냅샷을 만들어 보낸다.
// 관전자에게는 공개 정보만 담긴 스냅샷(viewerSeat -1)이 간다.
func (h *SFHub) broadcastState(room *sfRoom) {
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, SFMessage{
			Type:    SFMsgGameState,
			Payload: h.buildSFState(room, seat),
		})
	}
	if len(room.Spectators) > 0 {
		msg := SFMessage{Type: SFMsgGameState, Payload: h.buildSFState(room, -1)}
		for c := range room.Spectators {
			h.sendToClient(c, msg)
		}
	}
}

func (h *SFHub) broadcastEvent(room *sfRoom, event SFEventPayload) {
	h.broadcastToRoom(room, SFMessage{Type: SFMsgEvent, Payload: event})
}

// finishGame 종료 발표(전원 역할 공개)와 방 정리 (단판 승부 — 재대결 없음)
func (h *SFHub) finishGame(room *sfRoom) {
	game := room.Game
	if room.PhaseTimer != nil {
		room.PhaseTimer.Stop()
		room.PhaseTimer = nil
	}

	winnerLabel := "시민 승"
	if game.Winner == "mafia" {
		winnerLabel = "마피아 승"
	}
	detail := fmt.Sprintf("%s (%d일차)", winnerLabel, game.DayNo)

	winners, losers := []string{}, []string{}
	for _, p := range game.Players {
		isMafia := p.Role == SFRoleMafia
		if (game.Winner == "mafia") == isMafia {
			winners = append(winners, displayName(p.Name))
		} else {
			losers = append(losers, displayName(p.Name))
		}
	}

	h.broadcastEvent(room, SFEventPayload{Kind: "game_over", Message: detail})
	// 전원 역할이 공개된 최종 스냅샷을 먼저 보낸 뒤 종료를 알린다
	// (봇 러너는 sf_game_over 를 보고 스스로 끝난다)
	h.broadcastState(room)
	h.broadcastToRoom(room, SFMessage{
		Type: SFMsgGameOver,
		Payload: SFGameOverPayload{
			Winner:  game.Winner,
			DayNo:   game.DayNo,
			Players: h.sfPlayerViews(room),
		},
	})

	log.Printf("[마피아][경기결과] game=%s | %s | 소요=%s",
		game.ID, detail, matchDuration(game.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "skyfall",
		Players:  strings.Join(winners, "·") + " vs " + strings.Join(losers, "·"),
		Winner:   strings.Join(winners, "·"),
		Reason:   detail,
		Duration: matchSeconds(game.StartedAt),
		Bot:      sfRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
	if room.Code != "" {
		delete(h.activeCodes, room.Code) // 코드 재사용 복귀
	}
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *SFHub) handleDisconnect(client *SFClient) {
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

	// 진행 중: 유예 시간 동안 세션을 유지하고 재접속을 기다린다
	log.Printf("[마피아][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, SFMessage{
		Type: SFMsgOpponentDisconnected,
		Payload: SFOpponentDisconnectedPayload{
			Seat:         client.Seat,
			Name:         client.Name,
			GraceSeconds: int(h.grace.Seconds()),
		},
	})
	h.broadcastState(room)

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 안에 재접속하지 않은 좌석은 연습봇으로 대체하고
// 게임은 계속한다 — 밤·투표 해소가 이탈 좌석에 막히지 않는 근거
func (h *SFHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == SFPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[마피아][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := h.takeoverBot(room, seat, client.Name)
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.broadcastEvent(room, SFEventPayload{Kind: "bot_takeover", Seat: &seat, Name: client.Name})
	// 새 봇이 이 스냅샷을 받아 제출이 남았으면 즉시 이어간다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *SFHub) handleRejoin(client *SFClient, msg SFMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SFRejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, SFMessage{Type: SFMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, SFMessage{Type: SFMsgSessionExpired})
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

	log.Printf("[마피아][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, SFMessage{
		Type:    SFMsgReconnected,
		Payload: SFReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	// 전원에게 접속 상태가 반영된 스냅샷 (재접속 당사자 복원 포함)
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *SFHub) clearGameSessions(room *sfRoom) {
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		h.drop(c.SessionID)
	}
}

// ==================== 전송 ====================

func (h *SFHub) sendError(client *SFClient, message string) {
	h.sendToClient(client, SFMessage{Type: SFMsgError, Payload: SFErrorPayload{Message: message}})
}

func (h *SFHub) sendToClient(client *SFClient, message SFMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[SF] ")
}

func (h *SFHub) broadcastToRoom(room *sfRoom, message SFMessage) {
	for _, c := range room.Clients {
		if c != nil {
			h.sendToClient(c, message)
		}
	}
	for c := range room.Spectators { // 이벤트·종료 발표는 관전자에게도 간다
		h.sendToClient(c, message)
	}
}
