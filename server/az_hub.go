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

// ==================== 아줄 허브 ====================
//
// 다인 결(kr_hub/se_hub)을 그대로 복제했다 — 방코드·관전 인터셉트·리액션·
// 대기 상태 마감 타이머·fill_bots 3인 채움 즉시 시작·이탈 90초 봇 대체·
// 재접속 3종·전적 기록까지 같은 자리에 같은 모양으로 있다.
//
// 다른 점은 딱 둘이다.
//
//  1. 은닉이 없다. buildAZState 는 viewerSeat 로 갈라지는 분기가 yourSeat
//     하나뿐이고, 관전자는 참가자와 완전히 같은 스냅샷을 받는다.
//  2. 대기 상태가 둘이다. drafting(차례 60초 → 감점이 가장 적은 수 자동 선택)
//     과 tiling(정산 5초 → 자동으로 다음 라운드). kr_hub 의 StateSeq 기반
//     syncDeadline 을 그대로 쓴다 — 같은 차례에 스냅샷이 여러 번 쌓여도
//     마감이 늘어나지 않는다.

// azRoom 게임(순수 상태)과 좌석별 연결의 매핑
type azRoom struct {
	Game       *AZGame
	Clients    map[int]*AZClient // seat → client
	PhaseTimer *time.Timer       // 대기 상태 마감 타이머

	// DeadlineSeq 마지막으로 마감을 건 StateSeq — 같은 대기 상태에 스냅샷이
	// 쌓일 때마다(관전 입장·접속 변화 등) 마감이 늘어나지 않게 하는 근거
	DeadlineSeq int

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string

	// Spectators 관전자 연결 (사설 방 코드로만 진입, 상한 maxSpectators).
	// 좌석·세션이 없어 재접속을 지원하지 않는다 — 끊기면 목록에서만 뗀다.
	Spectators map[*AZClient]bool

	// LastReact 좌석별 마지막 리액션 발신 시각 (레이트리밋 장부)
	LastReact map[int]time.Time
}

// azPhaseSignal 마감 타이머의 발화 표식 — 대기 상태 일련번호(AfkSeq)로
// 지나간 발화를 구분한다
type azPhaseSignal struct {
	GameID string
	Seq    int
}

type AZHub struct {
	// 등록된 클라이언트
	clients map[*AZClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*azRoom

	// 글로벌 로비. 시작 전 공용 방은 하나뿐이다.
	lobby *azRoom

	// 사설 방 (초대 코드 → 시작 전 방). 허브 고루틴에서만 접근한다.
	// 시작하면 privateLobbies 에서 activeCodes 로 옮긴다.
	privateLobbies map[string]*azRoom

	// 진행 중 사설 방 (초대 코드 → gameID). 시작 후에도 코드로 방을 찾아
	// 관전 입장시키는 근거다. finishGame 에서 해제한다 (코드 재사용 복귀).
	activeCodes map[string]string

	// 클라이언트 등록
	register chan *AZClient

	// 클라이언트 등록 해제
	unregister chan *AZClient

	// 게임 메시지
	gameMessage chan AZGameMessage

	// 마감 타이머 발화 (time.AfterFunc → 허브 채널 경유)
	phaseFired chan azPhaseSignal

	// turnTimeout 차례 마감 (테스트가 Run 전에 낮춘다 — 허브 고루틴과 경합 금지)
	turnTimeout time.Duration

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*AZClient]

	// 타일 셔플용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type AZGameMessage struct {
	Client  *AZClient
	Message AZMessage
}

func NewAZHub() *AZHub {
	return &AZHub{
		register:       make(chan *AZClient),
		unregister:     make(chan *AZClient),
		clients:        make(map[*AZClient]bool),
		rooms:          make(map[string]*azRoom),
		privateLobbies: make(map[string]*azRoom),
		activeCodes:    make(map[string]string),
		gameMessage:    make(chan AZGameMessage),
		phaseFired:     make(chan azPhaseSignal, 8),
		turnTimeout:    azTurnTimeout,
		sessionManager: newSessionManager[*AZClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *AZHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[AZ] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[AZ] Client unregistered: %s", client.ID)
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

func (h *AZHub) handleGameMessage(gm AZGameMessage) {
	// 관전자는 어떤 행동도 할 수 없다 (보기 전용 — 리액션도 좌석 보유자만)
	if h.isSpectator(gm.Client) {
		h.sendError(gm.Client, spectatorDeniedMsg)
		return
	}
	switch gm.Message.Type {
	case AZMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case AZMsgReact:
		h.handleReact(gm.Client, gm.Message)
	case AZMsgFillBots:
		h.handleFillBots(gm.Client)
	case AZMsgStart:
		h.handleStart(gm.Client)
	case AZMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case AZMsgTake:
		h.handleTake(gm.Client, gm.Message)
	}
}

// ==================== 대기실 ====================

func (h *AZHub) handleJoinGame(client *AZClient, msg AZMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload AZJoinGamePayload
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

	log.Printf("[아줄][입장] game=%s | seat%d=%s 대기 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), AZMaxPlayers)
	if room.Code == "" { // 사설 방 입장은 조용히 (공용 로비만 알림)
		notify("아줄 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), len(room.Game.Players), AZMaxPlayers))
	}

	h.sendToClient(client, AZMessage{
		Type: AZMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
			"roomCode":  room.Code,
		},
	})

	h.broadcastEvent(room, AZEventPayload{Kind: "joined", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 입장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	// 정원이 차도 자동 시작하지 않는다 — 호스트 명시 시작만 (봇 채우기와 충돌 방지)
	h.broadcastState(room)
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다.
// 생략/빈 문자열은 기존 공용 로비 경로 그대로, "NEW"는 새 코드 발급,
// 그 외 코드는 해당 사설 방 (없으면 그 코드로 관대하게 새로 생성).
func (h *AZHub) lobbyRoomFor(roomField string) *azRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewAZGame(uuid.New().String())
			h.lobby = &azRoom{Game: game, Clients: map[int]*AZClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[AZ] Created lobby game %s", game.ID)
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
		game := NewAZGame(uuid.New().String())
		room = &azRoom{Game: game, Clients: map[int]*AZClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[AZ] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// ==================== 관전 / 리액션 ====================

// addSpectator 진행 중(또는 가득 찬) 사설 방에 관전자로 등록한다.
// 좌석·세션 없음 — 재접속 미지원, 끊기면 다시 코드로 들어온다.
// 은닉이 없는 게임이라 관전자는 참가자와 완전히 같은 스냅샷을 본다.
func (h *AZHub) addSpectator(room *azRoom, client *AZClient, name string) {
	if len(room.Spectators) >= maxSpectators {
		h.sendError(client, spectatorFullMsg)
		return
	}
	if room.Spectators == nil {
		room.Spectators = map[*AZClient]bool{}
	}
	client.Name = name
	client.GameID = room.Game.ID
	room.Spectators[client] = true

	log.Printf("[아줄][관전] game=%s | %s 관전 입장 (%d명)",
		room.Game.ID, displayName(name), len(room.Spectators))

	h.sendToClient(client, AZMessage{
		Type:    AZMsgSpectateJoined,
		Payload: map[string]interface{}{"gameId": room.Game.ID, "roomCode": room.Code},
	})
	// 전원(관전자 포함)에게 관전자 수가 반영된 스냅샷 — 신규 관전자의 첫 스냅샷 겸용
	h.broadcastState(room)
}

// isSpectator 관전자 연결인지 (좌석 보유자·미입장 연결은 false)
func (h *AZHub) isSpectator(client *AZClient) bool {
	if client.GameID == "" {
		return false
	}
	room := h.rooms[client.GameID]
	return room != nil && room.Spectators[client]
}

// handleReact 리액션 이모지 — 좌석 보유자만, waiting 중에도 허용.
// 화이트리스트 외·레이트리밋 초과는 조용히 무시한다 (상태 저장 없음).
func (h *AZHub) handleReact(client *AZClient, msg AZMessage) {
	room := h.rooms[client.GameID]
	if room == nil || client.Seat < 0 {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload AZReactPayload
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
	h.broadcastEvent(room, AZEventPayload{Kind: "react", Seat: &seat, Name: client.Name,
		Message: payload.Emoji})
}

// waitingRoomOf 클라이언트가 속한 시작 전 방 (공용 로비 또는 사설 방)
func (h *AZHub) waitingRoomOf(client *AZClient) *azRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Ready {
		return nil
	}
	return room
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *AZHub) hostSeat(room *azRoom) int {
	return hostSeatOf(room.Clients)
}

// azHumanCount 방의 사람 수
func azHumanCount(room *azRoom) int {
	return humanCountOf(room.Clients)
}

// updateLobbyWaiting 로비 현황판 갱신 — 사람 1명 이상 대기 && 미시작.
// 사설 방은 현황판에 노출하지 않는다 (초대 링크로만 접근).
func (h *AZHub) updateLobbyWaiting(room *azRoom) {
	if room.Code != "" {
		return
	}
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && azHumanCount(room) >= 1
	lobbySetWaiting("azul", waiting)
}

// handleFillBots host 가 빈 좌석을 연습봇으로 3인까지 채운 뒤 즉시
// 시작한다 (스폰은 join 을 거치지 않으므로 명시 시작 호출).
func (h *AZHub) handleFillBots(client *AZClient) {
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
	for len(room.Game.Players) < AZFillBotTarget {
		botNo++
		if !h.spawnAZBot(room, fmt.Sprintf("%s%d", botName, botNo)) {
			break
		}
	}

	if room.Game.CanStart() {
		h.startGame(room)
		return
	}
	h.broadcastState(room)
}

func (h *AZHub) handleStart(client *AZClient) {
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
		h.sendError(client, fmt.Sprintf("%d명 이상 모여야 시작할 수 있습니다", AZMinPlayers))
		return
	}
	h.startGame(room)
}

func (h *AZHub) startGame(room *azRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	if room.Code != "" {
		// 시작한 사설 방의 코드는 진행 중 장부로 옮긴다 (관전 입장 근거)
		delete(h.privateLobbies, room.Code)
		h.activeCodes[room.Code] = room.Game.ID
	} else {
		h.lobby = nil // 시작한 방은 로비에서 떼어낸다
		lobbySetWaiting("azul", false)
	}

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	log.Printf("[아줄][경기시작] game=%s | 인원=%d | 진열대=%d개 | 주머니=%d장 | %v",
		room.Game.ID, len(room.Game.Players), len(room.Game.Factories),
		len(room.Game.Bag), names)
	if !azRoomHasBot(room) { // 연습봇전은 운영자 알림을 억제한다
		notify("아줄 시작", fmt.Sprintf("%d인전 시작", len(room.Game.Players)))
	}

	h.broadcastEvent(room, AZEventPayload{Kind: "game_started",
		Message: fmt.Sprintf(
			"게임 시작 — %d인전, 진열대 %d개. 진열대나 중앙에서 같은 색 전부를 가져와 패턴 라인에 놓으세요",
			len(room.Game.Players), len(room.Game.Factories))})
	h.afterProgress(room)
}

// removeFromLobby 대기실에서 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *AZHub) removeFromLobby(room *azRoom, client *AZClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	// 좌석 압축: oldSeat 뒤의 클라이언트를 한 칸씩 당긴다
	rebuilt := map[int]*AZClient{}
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

	log.Printf("[아줄][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), AZMaxPlayers)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if azHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, AZMessage{Type: AZMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			lobbySetWaiting("azul", false)
		}
		return
	}

	h.broadcastEvent(room, AZEventPayload{Kind: "left", Name: client.Name,
		Message: fmt.Sprintf("%s님이 퇴장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *AZHub) roomOf(client *AZClient) *azRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

// handleTake 공장 수주 — 진열대/중앙에서 같은 색 전부를 가져와 패턴 라인에 놓는다
func (h *AZHub) handleTake(client *AZClient, msg AZMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload AZTakePayload
	json.Unmarshal(payloadBytes, &payload)

	game := room.Game
	if err := game.Take(client.Seat, payload.From, payload.Color, payload.Line); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[아줄][수주] game=%s | R%d seat%d=%s %s %s → 라인%d (중앙 %d장, 남은 진열대 %d개)",
		game.ID, game.Round, client.Seat, displayName(client.Name),
		payload.From, azColorLabel(payload.Color), payload.Line,
		len(game.Center), azFactoriesLeft(game))
	h.afterProgress(room)
}

// azFactoriesLeft 아직 타일이 남은 진열대 수 (로그용)
func azFactoriesLeft(game *AZGame) int {
	n := 0
	for _, f := range game.Factories {
		if len(f) > 0 {
			n++
		}
	}
	return n
}

// afterProgress 모든 게임 변이 뒤의 공통 마무리 — 이벤트 방송·종료 판정·
// 새 대기 상태의 마감 예약·스냅샷 방송을 한 번에 처리한다.
func (h *AZHub) afterProgress(room *azRoom) {
	h.drainEvents(room)
	if room.Game.Phase == AZPhaseGameOver {
		h.finishGame(room)
		return
	}
	h.syncDeadline(room)
	h.broadcastState(room)
}

// drainEvents 순수 규칙이 쌓은 이벤트를 az_event 로 방송한다
func (h *AZHub) drainEvents(room *azRoom) {
	for _, ev := range room.Game.DrainEvents() {
		payload := AZEventPayload{Kind: ev.Kind, Message: ev.Message}
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
// 같은 차례에 관전 입장·리액션으로 스냅샷이 쌓여도 마감은 늘어나지 않는다.
func (h *AZHub) syncDeadline(room *azRoom) {
	game := room.Game
	if room.DeadlineSeq == game.StateSeq {
		return
	}
	room.DeadlineSeq = game.StateSeq
	var dur time.Duration
	switch game.Phase {
	case AZPhaseDrafting:
		dur = h.turnTimeout
	case AZPhaseTiling:
		dur = azTilingDelay
	default:
		h.stopPhaseTimer(room)
		return
	}
	h.scheduleDeadline(room, dur)
}

// scheduleDeadline 마감 타이머 예약. AfkSeq 를 올려 지나간 발화를 구분하고,
// 발화는 허브 채널을 경유하므로 허브 밖에서 상태를 만지지 않는다.
func (h *AZHub) scheduleDeadline(room *azRoom, dur time.Duration) {
	h.stopPhaseTimer(room)
	room.Game.AfkSeq++
	room.Game.Deadline = time.Now().Add(dur).UnixMilli()
	sig := azPhaseSignal{GameID: room.Game.ID, Seq: room.Game.AfkSeq}
	room.PhaseTimer = time.AfterFunc(dur, func() {
		h.phaseFired <- sig
	})
}

func (h *AZHub) stopPhaseTimer(room *azRoom) {
	stopTimer(&room.PhaseTimer)
}

// handlePhaseFired 마감 발화 — 단계별 자동 행동으로 해소한다.
//   - drafting: 감점이 가장 적은 수를 자동 선택
//   - tiling: 다음 라운드 준비(또는 종료)
func (h *AZHub) handlePhaseFired(sig azPhaseSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.Game.AfkSeq != sig.Seq {
		return
	}
	game := room.Game
	switch game.Phase {
	case AZPhaseDrafting:
		seat := game.CurrentSeat
		if seat < 0 || seat >= len(game.Players) {
			return
		}
		actor := game.Players[seat]
		h.broadcastEvent(room, AZEventPayload{Kind: "afk", Seat: &seat, Name: actor.Name,
			Message: fmt.Sprintf("%s님이 오래 응답하지 않아 감점이 가장 적은 수를 자동으로 둡니다",
				actor.Name)})
		if !game.ForceMove() {
			return
		}
		log.Printf("[아줄][자동진행] game=%s | R%d seat%d 무응답 — 자동 수주", game.ID, game.Round, seat)

	case AZPhaseTiling:
		game.AdvanceRound(h.rng)
		log.Printf("[아줄][라운드] game=%s | R%d 준비 완료 (선=seat%d, 주머니 %d장, 버린 타일 %d장)",
			game.ID, game.Round, game.CurrentSeat, len(game.Bag), len(game.Discard))

	default:
		return
	}
	h.afterProgress(room)
}

// ==================== 상태 뷰 (은닉 없음) ====================

// buildAZState 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
//
// 이 게임에는 은닉이 없다 — viewerSeat 가 무엇이든 yourSeat 를 뺀 모든 필드가
// 동일하다. 관전자(viewerSeat -1)도 참가자와 똑같은 진열대·중앙·개인 보드를
// 본다. 빈 대기실(플레이어 0명·관전자 시점)에도 패닉 없이 빈 배열을 돌려준다.
func (h *AZHub) buildAZState(room *azRoom, viewerSeat int) AZGameStatePayload {
	game := room.Game

	players := []AZPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		lines := []AZLine{}
		for i := 0; i < AZWallSize; i++ {
			lines = append(lines, p.Lines[i])
		}
		wall := [][]bool{}
		for r := 0; r < AZWallSize; r++ {
			row := []bool{}
			for col := 0; col < AZWallSize; col++ {
				row = append(row, p.Wall[r][col])
			}
			wall = append(wall, row)
		}
		players = append(players, AZPlayerView{
			Seat:      p.Seat,
			Name:      p.Name,
			Connected: c != nil && c.Connected,
			Bot:       c != nil && c.Bot,
			Score:     p.Score,
			Lines:     lines,
			Wall:      wall,
			Floor:     append([]AZColor{}, p.Floor...),
		})
	}

	factories := [][]AZColor{}
	for _, f := range game.Factories {
		factories = append(factories, append([]AZColor{}, f...))
	}

	endsAt := int64(0)
	switch game.Phase {
	case AZPhaseDrafting, AZPhaseTiling:
		endsAt = game.Deadline
	}

	return AZGameStatePayload{
		GameID:         game.ID,
		RoomCode:       room.Code,
		Phase:          game.Phase,
		HostSeat:       h.hostSeat(room),
		YourSeat:       viewerSeat,
		Spectators:     len(room.Spectators),
		EndsAt:         endsAt,
		Round:          game.Round,
		CurrentSeat:    game.CurrentSeat,
		FirstNextSeat:  game.FirstNextSeat,
		Factories:      factories,
		Center:         append([]AZColor{}, game.Center...),
		CenterHasFirst: game.CenterHasFirst,
		BagLeft:        len(game.Bag),
		DiscardLeft:    len(game.Discard),
		Players:        players,
		LastAction:     game.LastAction,
		RoundResult:    game.RoundResult,
		Result:         game.Result,
	}
}

// broadcastState 좌석마다 스냅샷을 보낸다. 관전자에게 가는 스냅샷은
// yourSeat 가 -1 일 뿐 내용이 완전히 같다 (은닉 없음).
func (h *AZHub) broadcastState(room *azRoom) {
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, AZMessage{
			Type:    AZMsgGameState,
			Payload: h.buildAZState(room, seat),
		})
	}
	if len(room.Spectators) > 0 {
		msg := AZMessage{Type: AZMsgGameState, Payload: h.buildAZState(room, -1)}
		for c := range room.Spectators {
			h.sendToClient(c, msg)
		}
	}
}

func (h *AZHub) broadcastEvent(room *azRoom, event AZEventPayload) {
	h.broadcastToRoom(room, AZMessage{Type: AZMsgEvent, Payload: event})
}

// finishGame 게임 종료 발표·전적 기록·방 정리 (재대결은 같은 방 코드
// 재입장으로 — finishGame 이 코드를 반납해 같은 코드로 새 방을 만들 수 있다)
func (h *AZHub) finishGame(room *azRoom) {
	game := room.Game
	h.stopPhaseTimer(room)
	game.Deadline = 0

	result := game.Result
	if result == nil { // 방어선 — 종료는 항상 결과와 함께 온다
		seats, names := azWinners(game.Players)
		result = &AZResult{WinnerSeats: seats, WinnerNames: names,
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
		reason = "row_complete"
	}

	h.broadcastEvent(room, AZEventPayload{Kind: "game_over",
		Message: fmt.Sprintf("게임 종료 — %s", result.Message)})
	// 최종 보너스가 반영된 스냅샷을 먼저 보낸 뒤 종료를 알린다
	// (봇 러너는 az_game_over 를 보고 스스로 끝난다)
	h.broadcastState(room)
	h.broadcastToRoom(room, AZMessage{
		Type: AZMsgGameOver,
		Payload: AZGameOverPayload{
			WinnerSeats: append([]int{}, result.WinnerSeats...),
			WinnerNames: append([]string{}, result.WinnerNames...),
			Reason:      reason,
			Message:     result.Message,
			Round:       game.Round,
			Bonuses:     append([]AZBonusRow{}, game.Bonuses...),
			Players:     h.buildAZState(room, -1).Players,
		},
	})

	log.Printf("[아줄][경기결과] game=%s | 승자=%v(%s) | 사유=%s | 점수=%v | R%d | 소요=%s",
		game.ID, result.WinnerSeats, strings.Join(winners, "·"), reason,
		azScoreboard(game.Players), game.Round, matchDuration(game.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "azul",
		Players:  strings.Join(all, " vs "),
		Winner:   strings.Join(winners, "·"), // 동점 공동 승리는 "·" 로 잇는다
		Reason:   reason,
		Duration: matchSeconds(game.StartedAt),
		Bot:      azRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
	if room.Code != "" {
		delete(h.activeCodes, room.Code) // 코드 재사용 복귀
	}
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *AZHub) handleDisconnect(client *AZClient) {
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
	log.Printf("[아줄][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, AZMessage{
		Type: AZMsgPlayerDisconnected,
		Payload: AZPlayerDisconnectedPayload{
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
func (h *AZHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == AZPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[아줄][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := h.takeoverAZBot(room, seat, client.Name)
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.broadcastEvent(room, AZEventPayload{Kind: "bot_takeover", Seat: &seat,
		Name:    client.Name,
		Message: fmt.Sprintf("%s님 자리를 봇이 이어받았습니다", client.Name)})
	// 새 봇이 이 스냅샷을 받아 자기 차례면 즉시 이어간다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *AZHub) handleRejoin(client *AZClient, msg AZMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload AZRejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, AZMessage{Type: AZMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, AZMessage{Type: AZMsgSessionExpired})
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

	log.Printf("[아줄][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, AZMessage{
		Type:    AZMsgPlayerReconnected,
		Payload: AZPlayerReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	// 전원에게 접속 상태가 반영된 스냅샷 (재접속 당사자의 보드 복원 포함)
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *AZHub) clearGameSessions(room *azRoom) {
	clearRoomSessions(&h.sessionManager, room.Clients)
}

// ==================== 전송 ====================

func (h *AZHub) sendError(client *AZClient, message string) {
	h.sendToClient(client, AZMessage{Type: AZMsgError, Payload: AZErrorPayload{Message: message}})
}

func (h *AZHub) sendToClient(client *AZClient, message AZMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[AZ] ")
}

func (h *AZHub) broadcastToRoom(room *azRoom, message AZMessage) {
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

func ServeAZWs(hub *AZHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[AZ] Error upgrading connection:", err)
		return
	}

	client := &AZClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[AZ] ",
		func(msg AZMessage) { hub.gameMessage <- AZGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
