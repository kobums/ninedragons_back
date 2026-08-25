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

// spMinute 타이머 1분의 실제 길이 (테스트에서 짧게 낮춘다)
var spMinute = time.Minute

// spVoteTimeoutMinutes 투표 제한 시간(분) — 접속만 유지한 채 투표하지 않는
// 좌석이 방을 영구 정지시키지 않게 만료 시 제출된 표만으로 판정한다
const spVoteTimeoutMinutes = 1

// spRoom 게임(순수 상태)과 좌석별 연결의 매핑
type spRoom struct {
	Game    *SPGame
	Clients map[int]*SPClient // seat → client

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string

	// Timer playing 종료(→ voting) 타이머
	Timer *time.Timer

	// Spectators 관전자 연결 (사설 방 코드로만 진입, 상한 maxSpectators).
	// 좌석·세션이 없어 재접속을 지원하지 않는다 — 끊기면 목록에서만 뗀다.
	Spectators map[*SPClient]bool

	// LastReact 좌석별 마지막 리액션 발신 시각 (레이트리밋 장부)
	LastReact map[int]time.Time
}

// spTimerSignal playing 타이머의 발화 표식. 발화 시점의 (게임, 단계)가
// 현재와 다르면 — 스파이 추리로 이미 끝났다면 — 지나간 신호로 보고 무시한다.
type spTimerSignal struct {
	GameID string
	Phase  SPPhase
}

type SPHub struct {
	// 등록된 클라이언트
	clients map[*SPClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*spRoom

	// 글로벌 로비. 시작 전 공용 방은 하나뿐이다.
	lobby *spRoom

	// 사설 방 (초대 코드 → 시작 전 방). 허브 고루틴에서만 접근한다.
	// 시작하면 privateLobbies 에서 activeCodes 로 옮긴다.
	privateLobbies map[string]*spRoom

	// 진행 중 사설 방 (초대 코드 → gameID). 시작 후에도 코드로 방을 찾아
	// 관전 입장시키는 근거다. finishGame 에서 해제한다 (코드 재사용 복귀).
	activeCodes map[string]string

	// 클라이언트 등록
	register chan *SPClient

	// 클라이언트 등록 해제
	unregister chan *SPClient

	// 게임 메시지
	gameMessage chan SPGameMessage

	// playing 타이머 종료 (time.AfterFunc → 허브 채널 경유)
	timerFired chan spTimerSignal

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*SPClient]

	// 장소·스파이 배정용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type SPGameMessage struct {
	Client  *SPClient
	Message SPMessage
}

func NewSPHub() *SPHub {
	return &SPHub{
		register:       make(chan *SPClient),
		unregister:     make(chan *SPClient),
		clients:        make(map[*SPClient]bool),
		rooms:          make(map[string]*spRoom),
		privateLobbies: make(map[string]*spRoom),
		activeCodes:    make(map[string]string),
		gameMessage:    make(chan SPGameMessage),
		timerFired:     make(chan spTimerSignal, 8),
		sessionManager: newSessionManager[*SPClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *SPHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[SP] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[SP] Client unregistered: %s", client.ID)
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

func (h *SPHub) handleGameMessage(gm SPGameMessage) {
	// 관전자는 어떤 행동도 할 수 없다 (보기 전용 — 리액션도 좌석 보유자만)
	if h.isSpectator(gm.Client) {
		h.sendError(gm.Client, spectatorDeniedMsg)
		return
	}
	switch gm.Message.Type {
	case SPMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case SPMsgReact:
		h.handleReact(gm.Client, gm.Message)
	case SPMsgFillBots:
		h.handleFillBots(gm.Client)
	case SPMsgStart:
		h.handleStart(gm.Client)
	case SPMsgSetTimer:
		h.handleSetTimer(gm.Client, gm.Message)
	case SPMsgSetCategory:
		h.handleSetCategory(gm.Client, gm.Message)
	case SPMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case SPMsgGuess:
		h.handleGuess(gm.Client, gm.Message)
	case SPMsgVote:
		h.handleVote(gm.Client, gm.Message)
	}
}

// ==================== 로비 ====================

func (h *SPHub) handleJoinGame(client *SPClient, msg SPMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SPJoinGamePayload
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

	log.Printf("[스파이폴][입장] game=%s | seat%d=%s 게임 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), SPMaxPlayers)
	if room.Code == "" { // 사설 방 입장은 조용히 (공용 로비만 알림)
		notify("스파이폴 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), len(room.Game.Players), SPMaxPlayers))
	}

	h.sendToClient(client, SPMessage{
		Type: SPMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
			"roomCode":  room.Code,
		},
	})

	h.broadcastEvent(room, SPEventPayload{Kind: "joined", Seat: &seat, Name: client.Name})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다.
// 생략/빈 문자열은 기존 공용 로비 경로 그대로, "NEW"는 새 코드 발급,
// 그 외 코드는 해당 사설 방 (없으면 그 코드로 관대하게 새로 생성 —
// 링크로 들어왔는데 방이 이미 사라진 경우 대비).
func (h *SPHub) lobbyRoomFor(roomField string) *spRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewSPGame(uuid.New().String())
			h.lobby = &spRoom{Game: game, Clients: map[int]*SPClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[SP] Created lobby game %s", game.ID)
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
		game := NewSPGame(uuid.New().String())
		room = &spRoom{Game: game, Clients: map[int]*SPClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[SP] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// ==================== 관전 / 리액션 ====================

// addSpectator 진행 중(또는 가득 찬) 사설 방에 관전자로 등록한다.
// 좌석·세션 없음 — 재접속 미지원, 끊기면 다시 코드로 들어온다.
func (h *SPHub) addSpectator(room *spRoom, client *SPClient, name string) {
	if len(room.Spectators) >= maxSpectators {
		h.sendError(client, spectatorFullMsg)
		return
	}
	if room.Spectators == nil {
		room.Spectators = map[*SPClient]bool{}
	}
	client.Name = name
	client.GameID = room.Game.ID
	room.Spectators[client] = true

	log.Printf("[스파이폴][관전] game=%s | %s 관전 입장 (%d명)",
		room.Game.ID, displayName(name), len(room.Spectators))

	h.sendToClient(client, SPMessage{
		Type:    SPMsgSpectateJoined,
		Payload: map[string]interface{}{"gameId": room.Game.ID, "roomCode": room.Code},
	})
	// 전원(관전자 포함)에게 관전자 수가 반영된 스냅샷 — 신규 관전자의 첫 스냅샷 겸용
	h.broadcastState(room)
}

// isSpectator 관전자 연결인지 (좌석 보유자·미입장 연결은 false)
func (h *SPHub) isSpectator(client *SPClient) bool {
	if client.GameID == "" {
		return false
	}
	room := h.rooms[client.GameID]
	return room != nil && room.Spectators[client]
}

// handleReact 리액션 이모지 — 좌석 보유자만, waiting 중에도 허용.
// 화이트리스트 외·레이트리밋 초과는 조용히 무시한다 (상태 저장 없음).
func (h *SPHub) handleReact(client *SPClient, msg SPMessage) {
	room := h.rooms[client.GameID]
	if room == nil || client.Seat < 0 {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SPReactPayload
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
	h.broadcastEvent(room, SPEventPayload{Kind: "react", Seat: &seat, Name: client.Name, Message: payload.Emoji})
}

// waitingRoomOf 클라이언트가 속한 시작 전 방 (공용 로비 또는 사설 방)
func (h *SPHub) waitingRoomOf(client *SPClient) *spRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Ready {
		return nil
	}
	return room
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *SPHub) hostSeat(room *spRoom) int {
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

// handleFillBots host 가 최소 성립 인원(3)까지 연습봇으로 채운다.
// 봇은 연습용이라 정원(8)까지 채우지 않는다. 시작은 별도의 sp_start.
func (h *SPHub) handleFillBots(client *SPClient) {
	room := h.waitingRoomOf(client)
	if room == nil {
		h.sendError(client, "로비를 찾을 수 없습니다")
		return
	}
	if client.Seat != h.hostSeat(room) {
		h.sendError(client, "호스트만 봇을 채울 수 있습니다")
		return
	}
	if len(room.Game.Players) >= SPBotFillTarget {
		h.sendError(client, fmt.Sprintf("%d명 미만일 때만 봇을 채울 수 있습니다", SPBotFillTarget))
		return
	}

	botNo := 0
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			botNo++
		}
	}
	for len(room.Game.Players) < SPBotFillTarget {
		botNo++
		if h.spawnBot(room, fmt.Sprintf("%s%d", botName, botNo)) == nil {
			break
		}
	}
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

func (h *SPHub) handleStart(client *SPClient) {
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
		h.sendError(client, fmt.Sprintf("%d명 이상 모여야 시작할 수 있습니다", SPMinPlayers))
		return
	}
	h.startGame(room)
}

// handleSetTimer host 의 대기실 타이머 선택 (3|5|8분)
func (h *SPHub) handleSetTimer(client *SPClient, msg SPMessage) {
	room := h.waitingRoomOf(client)
	if room == nil {
		h.sendError(client, "로비를 찾을 수 없습니다")
		return
	}
	if client.Seat != h.hostSeat(room) {
		h.sendError(client, "호스트만 타이머를 바꿀 수 있습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SPSetTimerPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.SetTimer(payload.Minutes); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.broadcastState(room)
}

// handleSetCategory host 의 대기실 카테고리 선택 (랜덤 + spCategoryNames 9종)
func (h *SPHub) handleSetCategory(client *SPClient, msg SPMessage) {
	room := h.waitingRoomOf(client)
	if room == nil {
		h.sendError(client, "로비를 찾을 수 없습니다")
		return
	}
	if client.Seat != h.hostSeat(room) {
		h.sendError(client, "호스트만 카테고리를 바꿀 수 있습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SPSetCategoryPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.SetCategory(payload.Category); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.broadcastState(room)
}

// spHumanCount 방의 사람 수
func spHumanCount(room *spRoom) int {
	n := 0
	for _, c := range room.Clients {
		if c != nil && !c.Bot {
			n++
		}
	}
	return n
}

// spRoomHasBot 방에 연습봇이 있는지 (전적 기록용)
func spRoomHasBot(room *spRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}

// updateLobbyWaiting 로비 현황판 갱신 — 사람 1명 이상 대기 && 미시작.
// 사설 방은 현황판에 노출하지 않는다 (초대 링크로만 접근).
func (h *SPHub) updateLobbyWaiting(room *spRoom) {
	if room.Code != "" {
		return
	}
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && spHumanCount(room) >= 1
	lobbySetWaiting("spyfall", waiting)
}

func (h *SPHub) startGame(room *spRoom) {
	timerDur := time.Duration(room.Game.TimerMinutes) * spMinute
	if err := room.Game.Start(h.rng, timerDur); err != nil {
		return
	}
	if room.Code != "" {
		// 시작한 사설 방의 코드는 진행 중 장부로 옮긴다 (관전 입장 근거)
		delete(h.privateLobbies, room.Code)
		h.activeCodes[room.Code] = room.Game.ID
	} else {
		h.lobby = nil // 시작한 방은 로비에서 떼어낸다
		lobbySetWaiting("spyfall", false)
	}

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	log.Printf("[스파이폴][경기시작] game=%s | 인원=%d | 타이머=%d분 | %v",
		room.Game.ID, len(room.Game.Players), room.Game.TimerMinutes, names)
	// 봇 채우기로 시작한 판도 포함해 시작 시점에 1회만 알린다
	notify("스파이폴 게임 시작", fmt.Sprintf("%d인전 시작", len(room.Game.Players)))

	h.broadcastEvent(room, SPEventPayload{Kind: "started",
		Message: fmt.Sprintf("%d분 타이머로 시작합니다", room.Game.TimerMinutes)})
	h.broadcastState(room)
	h.scheduleTimer(room)
}

// removeFromLobby 대기 중 이탈 — 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *SPHub) removeFromLobby(room *spRoom, client *SPClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	rebuilt := map[int]*SPClient{}
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

	log.Printf("[스파이폴][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), SPMaxPlayers)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if spHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, SPMessage{Type: SPMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			lobbySetWaiting("spyfall", false)
		}
		return
	}

	h.broadcastEvent(room, SPEventPayload{Kind: "left", Name: client.Name})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *SPHub) roomOf(client *SPClient) *spRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

// handleGuess 스파이의 장소 추리 — 적중/오답 즉시 종료
func (h *SPHub) handleGuess(client *SPClient, msg SPMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SPGuessPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.Guess(client.Seat, payload.Location); err != nil {
		h.sendError(client, err.Error())
		return
	}

	seat := client.Seat
	log.Printf("[스파이폴][추리] game=%s | seat%d=%s → %q (정답 %q)",
		room.Game.ID, seat, displayName(client.Name), payload.Location, room.Game.Location)
	h.broadcastEvent(room, SPEventPayload{Kind: "guess", Seat: &seat,
		Message: fmt.Sprintf("스파이가 정답을 추리했습니다 — %s", payload.Location)})
	h.finishGame(room)
}

func (h *SPHub) handleVote(client *SPClient, msg SPMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SPVotePayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.SubmitVote(client.Seat, payload.Target); err != nil {
		h.sendError(client, err.Error())
		return
	}
	if room.Game.VoteComplete() {
		room.Game.ResolveVotes()
		h.finishGame(room)
		return
	}
	// 공개 투표 — 표가 쌓일 때마다 실시간 스냅샷
	h.broadcastState(room)
}

// ==================== playing 타이머 ====================

// scheduleTimer playing 종료 타이머를 건다.
// 발화는 허브 채널을 경유하므로 허브 밖에서 상태를 만지지 않는다.
func (h *SPHub) scheduleTimer(room *spRoom) {
	sig := spTimerSignal{GameID: room.Game.ID, Phase: SPPhasePlaying}
	dur := time.Until(time.UnixMilli(room.Game.EndsAt))
	room.Timer = time.AfterFunc(dur, func() {
		h.timerFired <- sig
	})
}

// handleTimerFired 타이머 종료 — 스파이 추리로 이미 끝난 게임의 늦은 신호는
// (GameID·Phase 가드로) 무시한다
func (h *SPHub) handleTimerFired(sig spTimerSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.Game.Phase != sig.Phase {
		return
	}
	if sig.Phase == SPPhaseVoting {
		// 투표 타임아웃 — 제출된 표만으로 판정 (표가 없으면 미검거 = 스파이 승)
		log.Printf("[스파이폴][투표마감] game=%s | 타임아웃 — %d표로 판정",
			room.Game.ID, len(room.Game.Votes))
		room.Game.ResolveVotes()
		h.finishGame(room)
		return
	}

	room.Game.BeginVoting(spVoteTimeoutMinutes * spMinute)
	log.Printf("[스파이폴][투표시작] game=%s | 타이머 종료", room.Game.ID)
	h.broadcastEvent(room, SPEventPayload{Kind: "vote_begin",
		Message: "시간 종료 — 스파이로 의심되는 사람을 지목하세요"})
	h.broadcastState(room)
	h.scheduleVoteTimer(room)
}

// scheduleVoteTimer 투표 타임아웃 타이머 (playing 타이머와 같은 채널 경유)
func (h *SPHub) scheduleVoteTimer(room *spRoom) {
	sig := spTimerSignal{GameID: room.Game.ID, Phase: SPPhaseVoting}
	dur := time.Until(time.UnixMilli(room.Game.EndsAt))
	room.Timer = time.AfterFunc(dur, func() {
		h.timerFired <- sig
	})
}

// ==================== 상태 뷰 (은닉의 핵심) ====================

// spPlayerViews 좌석별 공개 정보 — 스파이 여부는 어떤 형태로도 싣지 않는다
func (h *SPHub) spPlayerViews(room *spRoom) []SPPlayerView {
	game := room.Game
	players := []SPPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		_, voted := game.Votes[p.Seat]
		players = append(players, SPPlayerView{
			Seat:      p.Seat,
			Name:      p.Name,
			Connected: c != nil && c.Connected,
			Bot:       c != nil && c.Bot,
			Voted:     voted,
		})
	}
	return players
}

// buildSPState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
// 은닉: 비스파이 스냅샷에는 스파이 좌석 정보가 어떤 형태로도 없다.
// isSpy 는 본인 것만, location 은 비스파이에게만 (스파이·waiting 은 빈 문자열).
// 스파이 정체는 game_over 의 result 로만 공개된다.
func (h *SPHub) buildSPState(room *spRoom, viewerSeat int) SPGameStatePayload {
	game := room.Game

	isSpy := game.Ready && viewerSeat == game.SpySeat
	location := ""
	var locations []string
	if game.Ready {
		locations = game.Words()
		// 관전자(viewerSeat -1)에게는 장소를 주지 않는다 — 공개 정보만
		if !isSpy && viewerSeat >= 0 {
			location = game.Location
		}
	}

	endsAt := int64(0)
	if game.Phase == SPPhasePlaying || game.Phase == SPPhaseVoting {
		endsAt = game.EndsAt
	}

	var votes []SPVoteView
	if game.Votes != nil {
		votes = game.VoteViews()
	}

	return SPGameStatePayload{
		GameID:         game.ID,
		RoomCode:       room.Code,
		Phase:          game.Phase,
		HostSeat:       h.hostSeat(room),
		YourSeat:       viewerSeat,
		TimerMinutes:   game.TimerMinutes,
		CategoryChoice: game.CategoryChoice,
		Category:       game.Category,
		EndsAt:         endsAt,
		Locations:      locations,
		IsSpy:          isSpy,
		Location:       location,
		Players:        h.spPlayerViews(room),
		Spectators:     len(room.Spectators),
		Votes:          votes,
		Result:         game.Result,
	}
}

// broadcastState 좌석마다 개인화 스냅샷을 만들어 보낸다.
// 관전자에게는 공개 정보만 담긴 스냅샷(viewerSeat -1)이 간다.
func (h *SPHub) broadcastState(room *spRoom) {
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, SPMessage{
			Type:    SPMsgGameState,
			Payload: h.buildSPState(room, seat),
		})
	}
	if len(room.Spectators) > 0 {
		msg := SPMessage{Type: SPMsgGameState, Payload: h.buildSPState(room, -1)}
		for c := range room.Spectators {
			h.sendToClient(c, msg)
		}
	}
}

func (h *SPHub) broadcastEvent(room *spRoom, event SPEventPayload) {
	h.broadcastToRoom(room, SPMessage{Type: SPMsgEvent, Payload: event})
}

// finishGame 종료 발표(스파이 정체·장소·사유 공개)와 방 정리 (재대결 없음)
func (h *SPHub) finishGame(room *spRoom) {
	game := room.Game
	if room.Timer != nil {
		room.Timer.Stop()
		room.Timer = nil
	}

	res := game.Result
	winnerLabel := "일반인 승"
	if res.Winner == "spy" {
		winnerLabel = "스파이 승"
	}
	detail := fmt.Sprintf("%s (%s)", winnerLabel, spReasonLabel(res.Reason))

	winners, losers := []string{}, []string{}
	for _, p := range game.Players {
		isSpy := p.Seat == game.SpySeat
		if (res.Winner == "spy") == isSpy {
			winners = append(winners, displayName(p.Name))
		} else {
			losers = append(losers, displayName(p.Name))
		}
	}

	h.broadcastEvent(room, SPEventPayload{Kind: "game_over", Message: detail})
	// 결과가 실린 최종 스냅샷을 먼저 보낸 뒤 종료를 알린다
	// (봇 러너는 sp_game_over 를 보고 스스로 끝난다)
	h.broadcastState(room)
	h.broadcastToRoom(room, SPMessage{
		Type: SPMsgGameOver,
		Payload: SPGameOverPayload{
			Winner:          res.Winner,
			SpySeat:         res.SpySeat,
			Category:        res.Category,
			Location:        res.Location,
			Reason:          res.Reason,
			GuessedLocation: res.GuessedLocation,
			TopSeat:         res.TopSeat,
			Players:         h.spPlayerViews(room),
		},
	})

	log.Printf("[스파이폴][경기결과] game=%s | %s | %s=%s | 스파이=seat%d | 소요=%s",
		game.ID, detail, game.Category, game.Location, game.SpySeat, matchDuration(game.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "spyfall",
		Players:  strings.Join(winners, "·") + " vs " + strings.Join(losers, "·"),
		Winner:   strings.Join(winners, "·"),
		Reason:   detail,
		Duration: matchSeconds(game.StartedAt),
		Bot:      spRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
	if room.Code != "" {
		delete(h.activeCodes, room.Code) // 코드 재사용 복귀
	}
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *SPHub) handleDisconnect(client *SPClient) {
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
	log.Printf("[스파이폴][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, SPMessage{
		Type: SPMsgOpponentDisconnected,
		Payload: SPOpponentDisconnectedPayload{
			Seat:         client.Seat,
			Name:         client.Name,
			GraceSeconds: int(h.grace.Seconds()),
		},
	})
	h.broadcastState(room)

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 안에 재접속하지 않은 좌석은 연습봇으로 대체하고
// 게임은 계속한다 — 투표 해소가 이탈 좌석에 막히지 않는 근거
func (h *SPHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == SPPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[스파이폴][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := h.takeoverBot(room, seat, client.Name)
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.broadcastEvent(room, SPEventPayload{Kind: "bot_takeover", Seat: &seat, Name: client.Name})
	// 새 봇이 이 스냅샷을 받아 제출이 남았으면 즉시 이어간다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *SPHub) handleRejoin(client *SPClient, msg SPMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SPRejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, SPMessage{Type: SPMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, SPMessage{Type: SPMsgSessionExpired})
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

	log.Printf("[스파이폴][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, SPMessage{
		Type:    SPMsgReconnected,
		Payload: SPReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	// 전원에게 접속 상태가 반영된 스냅샷 (재접속 당사자 복원 포함)
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *SPHub) clearGameSessions(room *spRoom) {
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		h.drop(c.SessionID)
	}
}

// ==================== 전송 ====================

func (h *SPHub) sendError(client *SPClient, message string) {
	h.sendToClient(client, SPMessage{Type: SPMsgError, Payload: SPErrorPayload{Message: message}})
}

func (h *SPHub) sendToClient(client *SPClient, message SPMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[SP] ")
}

func (h *SPHub) broadcastToRoom(room *spRoom, message SPMessage) {
	for _, c := range room.Clients {
		if c != nil {
			h.sendToClient(c, message)
		}
	}
	for c := range room.Spectators { // 이벤트·종료 발표는 관전자에게도 간다
		h.sendToClient(c, message)
	}
}
