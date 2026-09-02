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

// ==================== 더 마인드 허브 ====================
//
// 다인 결(kr_hub/cc_hub)을 그대로 따르되 **턴 상태기계는 들어냈다** —
// 세트(se_hub)와 같은 선착 판정 모델이다. currentSeat 도, 좌석별 AFK 자동
// 진행도 없다. 모든 mi_play 는 h.gameMessage 채널로 모이고 허브 고루틴이
// 도착한 순서대로 하나씩 처리하므로, 동시에 두 사람이 내면 먼저 도착한 쪽이
// 먼저 판정되고 뒤에 온 카드가 더 작으면 그대로 실수가 된다. 이 게임의 긴장이
// 거기서 나오므로 별도 유예를 두지 않는다. 판정이 한 고루틴에 직렬화되므로
// 게임 상태에는 락이 필요 없다.
//
// 규칙상 소통이 금지라 **리액션이 없다** — mi_react 를 아예 두지 않았다.
//
// 타이머는 셋이다.
//   - PhaseTimer  단계 마감 (ready 카운트다운 → playing, round_end → 다음
//     라운드, playing 라운드 캡 → 자동 진행). (phase, round) 가 바뀔 때만
//     다시 건다 — 카드를 낼 때마다 라운드 캡이 늘어나면 안 되기 때문이다.
//   - StarTimer   수리검 만장일치 창
//   - EndTimer    게임 전체 캡 (무한 게임 방지)

// miRoom 게임(순수 상태)과 좌석별 연결의 매핑
type miRoom struct {
	Game    *MIGame
	Clients map[int]*MIClient // seat → client

	PhaseTimer *time.Timer // 단계 마감
	StarTimer  *time.Timer // 수리검 창
	EndTimer   *time.Timer // 게임 전체 캡

	// PhaseKey 마지막으로 마감을 건 (phase, round). 같은 라운드에 스냅샷이
	// 여러 번 쌓여도 라운드 캡이 늘어나지 않게 하는 근거다.
	PhaseKey string

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string

	// Spectators 관전자 연결 (사설 방 코드로만 진입, 상한 maxSpectators).
	// 좌석·세션이 없어 재접속을 지원하지 않는다 — 끊기면 목록에서만 뗀다.
	Spectators map[*MIClient]bool
}

// miTimerSignal 타이머 발화 표식 — 일련번호로 지나간 발화를 구분한다
type miTimerSignal struct {
	GameID string
	Kind   string // "phase" | "star" | "end"
	Seq    int
}

type MIHub struct {
	clients map[*MIClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*miRoom

	// 글로벌 로비. 시작 전 공용 방은 하나뿐이다.
	lobby *miRoom

	// 사설 방 (초대 코드 → 시작 전 방). 허브 고루틴에서만 접근한다.
	privateLobbies map[string]*miRoom

	// 진행 중 사설 방 (초대 코드 → gameID). 관전 입장의 근거이며
	// finishGame 에서 해제한다 (코드 재사용 복귀).
	activeCodes map[string]string

	register   chan *MIClient
	unregister chan *MIClient

	// 게임 메시지 — 선착 판정의 직렬화 지점이다
	gameMessage chan MIGameMessage

	// 타이머 발화 (time.AfterFunc → 허브 채널 경유)
	timerFired chan miTimerSignal

	// 시간 설정 (테스트가 Run 전에 낮춘다 — 고루틴과 경합 금지)
	readyDelay    time.Duration
	roundEndDelay time.Duration
	roundCap      time.Duration
	starWindow    time.Duration
	gameCap       time.Duration

	// 세션·유예 타이머 장부
	sessionManager[*MIClient]

	// 덱 셔플용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type MIGameMessage struct {
	Client  *MIClient
	Message MIMessage
}

func NewMIHub() *MIHub {
	return &MIHub{
		register:       make(chan *MIClient),
		unregister:     make(chan *MIClient),
		clients:        make(map[*MIClient]bool),
		rooms:          make(map[string]*miRoom),
		privateLobbies: make(map[string]*miRoom),
		activeCodes:    make(map[string]string),
		gameMessage:    make(chan MIGameMessage),
		timerFired:     make(chan miTimerSignal, 16),
		readyDelay:     miReadyDelay,
		roundEndDelay:  miRoundEndDelay,
		roundCap:       miRoundCap,
		starWindow:     miStarVoteWindow,
		gameCap:        miGameCap,
		sessionManager: newSessionManager[*MIClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *MIHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[MI] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[MI] Client unregistered: %s", client.ID)
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

func (h *MIHub) handleGameMessage(gm MIGameMessage) {
	// 관전자는 어떤 행동도 할 수 없다 (보기 전용)
	if h.isSpectator(gm.Client) {
		h.sendError(gm.Client, spectatorDeniedMsg)
		return
	}
	switch gm.Message.Type {
	case MIMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case MIMsgFillBots:
		h.handleFillBots(gm.Client)
	case MIMsgStart:
		h.handleStart(gm.Client)
	case MIMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case MIMsgPlay:
		h.handlePlay(gm.Client)
	case MIMsgStarPropose:
		h.handleStarPropose(gm.Client)
	case MIMsgStarAccept:
		h.handleStarVote(gm.Client, true)
	case MIMsgStarDecline:
		h.handleStarVote(gm.Client, false)
	}
}

// ==================== 대기실 ====================

func (h *MIHub) handleJoinGame(client *MIClient, msg MIMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload MIJoinGamePayload
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

	log.Printf("[더마인드][입장] game=%s | seat%d=%s 대기 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), MIMaxPlayers)
	if room.Code == "" { // 사설 방 입장은 조용히 (공용 로비만 알림)
		notify("더 마인드 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), len(room.Game.Players), MIMaxPlayers))
	}

	h.sendToClient(client, MIMessage{
		Type: MIMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
			"roomCode":  room.Code,
		},
	})

	h.broadcastEvent(room, MIEventPayload{Kind: "joined", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 입장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	// 정원이 차도 자동 시작하지 않는다 — 호스트 명시 시작만
	h.broadcastState(room)
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다.
// 생략/빈 문자열은 공용 로비, "NEW"는 새 코드 발급, 그 외 코드는 해당 사설 방
// (없으면 그 코드로 관대하게 새로 생성).
func (h *MIHub) lobbyRoomFor(roomField string) *miRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewMIGame(uuid.New().String())
			h.lobby = &miRoom{Game: game, Clients: map[int]*MIClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[MI] Created lobby game %s", game.ID)
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
		game := NewMIGame(uuid.New().String())
		room = &miRoom{Game: game, Clients: map[int]*MIClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[MI] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// ==================== 관전 ====================

// addSpectator 진행 중(또는 가득 찬) 사설 방에 관전자로 등록한다.
// 좌석·세션 없음 — 재접속 미지원, 끊기면 다시 코드로 들어온다.
// 관전자에게는 손패가 하나도 보이지 않는다 (yourHand 키 자체가 없다).
func (h *MIHub) addSpectator(room *miRoom, client *MIClient, name string) {
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	if len(room.Spectators) >= maxSpectators {
		h.sendError(client, spectatorFullMsg)
		return
	}
	if room.Spectators == nil {
		room.Spectators = map[*MIClient]bool{}
	}
	client.Name = name
	client.GameID = room.Game.ID
	room.Spectators[client] = true

	log.Printf("[더마인드][관전] game=%s | %s 관전 입장 (%d명)",
		room.Game.ID, displayName(name), len(room.Spectators))

	h.sendToClient(client, MIMessage{
		Type:    MIMsgSpectateJoined,
		Payload: map[string]interface{}{"gameId": room.Game.ID, "roomCode": room.Code},
	})
	h.broadcastState(room)
}

// isSpectator 관전자 연결인지 (좌석 보유자·미입장 연결은 false)
func (h *MIHub) isSpectator(client *MIClient) bool {
	if client.GameID == "" {
		return false
	}
	room := h.rooms[client.GameID]
	return room != nil && room.Spectators[client]
}

// waitingRoomOf 클라이언트가 속한 시작 전 방
func (h *MIHub) waitingRoomOf(client *MIClient) *miRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Ready {
		return nil
	}
	return room
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *MIHub) hostSeat(room *miRoom) int {
	return hostSeatOf(room.Clients)
}

// miHumanCount 방의 사람 수
func miHumanCount(room *miRoom) int {
	return humanCountOf(room.Clients)
}

// updateLobbyWaiting 로비 현황판 갱신 — 사설 방은 노출하지 않는다
func (h *MIHub) updateLobbyWaiting(room *miRoom) {
	if room.Code != "" {
		return
	}
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && miHumanCount(room) >= 1
	lobbySetWaiting("mind", waiting)
}

// handleFillBots host 가 빈 좌석을 연습봇으로 3인까지 채운 뒤 즉시 시작한다
func (h *MIHub) handleFillBots(client *MIClient) {
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
	for len(room.Game.Players) < MIFillBotTarget {
		botNo++
		if !h.spawnMIBot(room, fmt.Sprintf("%s%d", botName, botNo)) {
			break
		}
	}

	if room.Game.CanStart() {
		h.startGame(room)
		return
	}
	h.broadcastState(room)
}

func (h *MIHub) handleStart(client *MIClient) {
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
		h.sendError(client, fmt.Sprintf("%d명 이상 모여야 시작할 수 있습니다", MIMinPlayers))
		return
	}
	h.startGame(room)
}

func (h *MIHub) startGame(room *miRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	if room.Code != "" {
		delete(h.privateLobbies, room.Code)
		h.activeCodes[room.Code] = room.Game.ID
	} else {
		h.lobby = nil
		lobbySetWaiting("mind", false)
	}

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	log.Printf("[더마인드][경기시작] game=%s | 인원=%d | 최종라운드=%d | 생명=%d | 수리검=%d | 캡=%.0f분 | %v",
		room.Game.ID, len(room.Game.Players), room.Game.MaxRound, room.Game.Lives,
		room.Game.Stars, h.gameCap.Minutes(), names)
	if !miRoomHasBot(room) { // 연습봇전은 운영자 알림을 억제한다
		notify("더 마인드 시작", fmt.Sprintf("%d인 협력전 시작", len(room.Game.Players)))
	}

	h.scheduleGameCap(room)
	h.afterProgress(room)
}

// removeFromLobby 대기실에서 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *MIHub) removeFromLobby(room *miRoom, client *MIClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	rebuilt := map[int]*MIClient{}
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

	log.Printf("[더마인드][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), MIMaxPlayers)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if miHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, MIMessage{Type: MIMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			lobbySetWaiting("mind", false)
		}
		return
	}

	h.broadcastEvent(room, MIEventPayload{Kind: "left", Name: client.Name,
		Message: fmt.Sprintf("%s님이 퇴장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 (선착 판정) ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *MIHub) roomOf(client *MIClient) *miRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

// handlePlay 카드 내기. 차례 검사가 없다 — 누구든 언제든 보낼 수 있고,
// 이 함수에 도착한 순서가 곧 판정 순서다(허브 고루틴 직렬화).
// 카드 지정이 없어 언제나 그 좌석의 최저 카드가 나간다.
func (h *MIHub) handlePlay(client *MIClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	game := room.Game
	before := game.Lives
	if err := game.Play(client.Seat); err != nil {
		h.sendError(client, err.Error())
		return
	}

	if game.Lives < before && game.LastMistake != nil {
		log.Printf("[더마인드][실수] game=%s | seat%d=%s 냄=%d | 소각=%d장 | 생명 %d→%d (라운드 %d/%d)",
			game.ID, client.Seat, displayName(client.Name), game.LastMistake.Played,
			len(game.LastMistake.Burned), before, game.Lives, game.Round, game.MaxRound)
	} else {
		log.Printf("[더마인드][카드] game=%s | seat%d=%s → %d (더미 %d장, 남은 손패 %d장)",
			game.ID, client.Seat, displayName(client.Name), game.LastPlayed,
			len(game.Pile), miCardsLeft(game.Players))
	}
	h.afterProgress(room)
}

// handleStarPropose 수리검 제안 — 누구든 제안할 수 있고 제안자는 자동 찬성이다
func (h *MIHub) handleStarPropose(client *MIClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	if err := room.Game.ProposeStar(client.Seat, time.Now(), h.starWindow); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[더마인드][수리검] game=%s | seat%d=%s 제안 (남은 수리검 %d)",
		room.Game.ID, client.Seat, displayName(client.Name), room.Game.Stars)
	if room.Game.StarVote != nil {
		h.scheduleStarWindow(room)
	}
	h.afterProgress(room)
}

// handleStarVote 수리검 찬성·거절. 만장일치면 즉시 발동한다.
func (h *MIHub) handleStarVote(client *MIClient, accept bool) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	game := room.Game
	var err error
	if accept {
		err = game.AcceptStar(client.Seat)
	} else {
		err = game.DeclineStar(client.Seat)
	}
	if err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[더마인드][수리검] game=%s | seat%d=%s %s (남은 수리검 %d)",
		game.ID, client.Seat, displayName(client.Name),
		map[bool]string{true: "찬성", false: "거절"}[accept], game.Stars)
	if game.StarVote == nil { // 발동·무산 — 창 타이머를 거둔다
		h.stopStarTimer(room)
	}
	h.afterProgress(room)
}

// afterProgress 모든 게임 변이 뒤의 공통 마무리 — 이벤트 방송·종료 판정·
// 단계 마감 재설정·스냅샷 방송.
func (h *MIHub) afterProgress(room *miRoom) {
	h.drainEvents(room)
	if room.Game.Phase == MIPhaseGameOver {
		h.finishGame(room)
		return
	}
	// 투표가 사라졌으면(발동·무산·라운드 전환) 창 타이머도 거둔다
	if room.Game.StarVote == nil {
		h.stopStarTimer(room)
	}
	h.syncDeadline(room)
	h.broadcastState(room)
}

// drainEvents 순수 규칙이 쌓은 이벤트를 mi_event 로 방송한다
func (h *MIHub) drainEvents(room *miRoom) {
	for _, ev := range room.Game.DrainEvents() {
		payload := MIEventPayload{Kind: ev.Kind, Message: ev.Message}
		if ev.Seat >= 0 && ev.Seat < len(room.Game.Players) {
			seat := ev.Seat
			payload.Seat = &seat
			payload.Name = room.Game.Players[seat].Name
		}
		h.broadcastEvent(room, payload)
	}
}

// ==================== 타이머 ====================

// syncDeadline (phase, round) 가 바뀐 순간에만 단계 마감을 다시 건다.
// 카드를 낼 때마다 라운드 캡이 늘어나면 안전장치가 무의미해지므로,
// 같은 라운드의 playing 안에서는 절대 다시 걸지 않는다.
func (h *MIHub) syncDeadline(room *miRoom) {
	game := room.Game
	key := fmt.Sprintf("%s:%d", game.Phase, game.Round)
	if room.PhaseKey == key {
		return
	}
	room.PhaseKey = key

	switch game.Phase {
	case MIPhaseReady:
		h.schedulePhase(room, h.readyDelay)
	case MIPhasePlaying:
		h.schedulePhase(room, h.roundCap)
	case MIPhaseRoundEnd:
		h.schedulePhase(room, h.roundEndDelay)
	default:
		h.stopPhaseTimer(room)
		game.Deadline = 0
	}
}

func (h *MIHub) schedulePhase(room *miRoom, d time.Duration) {
	h.stopPhaseTimer(room)
	room.Game.StateSeq++
	room.Game.Deadline = time.Now().Add(d).UnixMilli()
	sig := miTimerSignal{GameID: room.Game.ID, Kind: "phase", Seq: room.Game.StateSeq}
	room.PhaseTimer = time.AfterFunc(d, func() { h.timerFired <- sig })
}

func (h *MIHub) stopPhaseTimer(room *miRoom) {
	stopTimer(&room.PhaseTimer)
}

func (h *MIHub) scheduleStarWindow(room *miRoom) {
	h.stopStarTimer(room)
	sig := miTimerSignal{GameID: room.Game.ID, Kind: "star", Seq: room.Game.StarSeq}
	room.StarTimer = time.AfterFunc(h.starWindow, func() { h.timerFired <- sig })
}

func (h *MIHub) stopStarTimer(room *miRoom) {
	if room.StarTimer != nil {
		room.StarTimer.Stop()
		room.StarTimer = nil
	}
}

// scheduleGameCap 시작 시 한 번만 거는 전체 캡 (무한 게임 방지)
func (h *MIHub) scheduleGameCap(room *miRoom) {
	h.stopEndTimer(room)
	room.Game.EndSeq++
	sig := miTimerSignal{GameID: room.Game.ID, Kind: "end", Seq: room.Game.EndSeq}
	room.EndTimer = time.AfterFunc(h.gameCap, func() { h.timerFired <- sig })
}

func (h *MIHub) stopEndTimer(room *miRoom) {
	if room.EndTimer != nil {
		room.EndTimer.Stop()
		room.EndTimer = nil
	}
}

// stopTimers 방을 정리할 때 세 타이머를 함께 거둔다
func (h *MIHub) stopTimers(room *miRoom) {
	h.stopPhaseTimer(room)
	h.stopStarTimer(room)
	h.stopEndTimer(room)
}

// handleTimerFired 세 타이머의 발화 처리 — 지나간 발화는 일련번호로 걸러낸다
func (h *MIHub) handleTimerFired(sig miTimerSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.Game.Phase == MIPhaseGameOver {
		return
	}
	game := room.Game

	switch sig.Kind {
	case "phase":
		if game.StateSeq != sig.Seq {
			return
		}
		switch game.Phase {
		case MIPhaseReady:
			game.BeginPlaying()
		case MIPhaseRoundEnd:
			game.BeginRound(h.rng)
			log.Printf("[더마인드][라운드] game=%s | %d 라운드 배분 (생명 %d, 수리검 %d)",
				game.ID, game.Round, game.Lives, game.Stars)
		case MIPhasePlaying:
			log.Printf("[더마인드][정체] game=%s | %d 라운드가 %.0f초를 넘겨 자동 진행 (생명 %d)",
				game.ID, game.Round, h.roundCap.Seconds(), game.Lives)
			game.AutoAdvance()
			// 같은 라운드의 playing 이 이어지면 syncDeadline 이 다시 걸지
			// 않으므로(키 동일) 여기서 캡을 새로 건다
			if game.Phase == MIPhasePlaying {
				h.schedulePhase(room, h.roundCap)
			}
		}
	case "star":
		if !game.ExpireStar(sig.Seq) {
			return
		}
		h.stopStarTimer(room)
	case "end":
		if game.EndSeq != sig.Seq {
			return
		}
		log.Printf("[더마인드][강제종료] game=%s | %.0f분 경과 — %d/%d 라운드에서 정산",
			game.ID, h.gameCap.Minutes(), game.Round, game.MaxRound)
		h.broadcastEvent(room, MIEventPayload{Kind: "time_up",
			Message: "제한 시간이 끝나 게임을 마칩니다"})
		game.ForceEnd()
	}

	h.afterProgress(room)
}

// ==================== 상태 뷰 (은닉: yourHand) ====================

// buildMIState 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
//
// 은닉은 하나뿐이다 — yourHand 는 본인에게만 실린다. 타인·관전자
// (viewerSeat -1)의 raw JSON 에는 yourHand 키 자체가 없다(포인터+omitempty).
// 공개되는 것은 handCount 뿐이고, 중앙 더미·생명·수리검은 전원 동일하다.
// 빈 대기실(플레이어 0명·관전자 시점)에도 패닉 없이 빈 배열을 돌려준다.
func (h *MIHub) buildMIState(room *miRoom, viewerSeat int) MIGameStatePayload {
	game := room.Game

	players := []MIPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		players = append(players, MIPlayerView{
			Seat:      p.Seat,
			Name:      p.Name,
			Connected: c != nil && c.Connected,
			Bot:       c != nil && c.Bot,
			HandCount: len(p.Hand),
		})
	}

	var yourHand *[]int
	if viewerSeat >= 0 && viewerSeat < len(game.Players) {
		hand := append([]int{}, game.Players[viewerSeat].Hand...)
		yourHand = &hand
	}

	return MIGameStatePayload{
		GameID:      game.ID,
		RoomCode:    room.Code,
		Phase:       game.Phase,
		HostSeat:    h.hostSeat(room),
		YourSeat:    viewerSeat,
		Spectators:  len(room.Spectators),
		EndsAt:      game.Deadline,
		Round:       game.Round,
		MaxRound:    game.MaxRound,
		Lives:       game.Lives,
		Stars:       game.Stars,
		LastPlayed:  game.LastPlayed,
		Pile:        append([]int{}, game.Pile...),
		YourHand:    yourHand,
		Players:     players,
		StarVote:    game.StarVote,
		LastMistake: game.LastMistake,
		Result:      game.Result,
	}
}

// broadcastState 좌석마다 자기 손패가 실린 스냅샷을 따로 보낸다.
// 관전자에게 가는 스냅샷에는 yourHand 키가 없다.
func (h *MIHub) broadcastState(room *miRoom) {
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, MIMessage{
			Type:    MIMsgGameState,
			Payload: h.buildMIState(room, seat),
		})
	}
	if len(room.Spectators) > 0 {
		msg := MIMessage{Type: MIMsgGameState, Payload: h.buildMIState(room, -1)}
		for c := range room.Spectators {
			h.sendToClient(c, msg)
		}
	}
}

func (h *MIHub) broadcastEvent(room *miRoom, event MIEventPayload) {
	h.broadcastToRoom(room, MIMessage{Type: MIMsgEvent, Payload: event})
}

// finishGame 게임 종료 발표·전적 기록·방 정리 (재대결은 같은 방 코드
// 재입장으로 — finishGame 이 코드를 반납해 같은 코드로 새 방을 만들 수 있다).
//
// 협력 게임이라 전적은 진영전과 다르게 적는다 — 클리어면 참가자 전원이
// Winner 에 들어가고(전원 승자), 실패면 어떤 닉네임과도 겹치지 않는
// miFailWinnerTag 를 넣어 전원 패자로 집계한다 (Winner "" 는 무승부다).
func (h *MIHub) finishGame(room *miRoom) {
	game := room.Game
	h.stopTimers(room)
	game.Deadline = 0

	result := game.Result
	if result == nil { // 방어선 — 종료는 항상 결과와 함께 온다
		result = &MIResult{Cleared: false, Round: game.Round,
			Message: "게임이 종료됐습니다"}
	}

	crew := []string{}
	for _, p := range game.Players {
		crew = append(crew, displayName(p.Name))
	}
	reason := game.EndReason
	if reason == "" {
		reason = "no_lives"
	}
	winner := miFailWinnerTag
	if result.Cleared {
		winner = strings.Join(crew, "·") // 전원 승자
	}

	headline := "실패"
	if result.Cleared {
		headline = "클리어"
	}
	h.broadcastEvent(room, MIEventPayload{Kind: "game_over",
		Message: fmt.Sprintf("게임 종료 — %s! %s", headline, result.Message)})
	// 최종 스냅샷을 먼저 보낸 뒤 종료를 알린다
	// (봇 러너는 mi_game_over 를 보고 스스로 끝난다)
	h.broadcastState(room)
	h.broadcastToRoom(room, MIMessage{
		Type: MIMsgGameOver,
		Payload: MIGameOverPayload{
			Cleared:  result.Cleared,
			Reason:   reason,
			Round:    result.Round,
			MaxRound: game.MaxRound,
			Lives:    game.Lives,
			Stars:    game.Stars,
			Message:  result.Message,
			Players:  h.buildMIState(room, -1).Players,
		},
	})

	log.Printf("[더마인드][경기결과] game=%s | 클리어=%t | 사유=%s | 라운드=%d/%d | 생명=%d | 수리검=%d | 소요=%s",
		game.ID, result.Cleared, reason, result.Round, game.MaxRound,
		game.Lives, game.Stars, matchDuration(game.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "mind",
		Players:  strings.Join(crew, "·"), // 협력전 — 진영 구분자 없이 한 팀
		Winner:   winner,
		Reason:   reason,
		Duration: matchSeconds(game.StartedAt),
		Bot:      miRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
	if room.Code != "" {
		delete(h.activeCodes, room.Code) // 코드 재사용 복귀
	}
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *MIHub) handleDisconnect(client *MIClient) {
	// 관전자 연결 종료 — 세션·유예 없이 목록에서만 뗀다
	if room := h.rooms[client.GameID]; room != nil && room.Spectators[client] {
		delete(room.Spectators, client)
		h.broadcastState(room) // 관전자 수 갱신
		return
	}
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

	// 진행 중: 유예 시간 동안 재접속을 기다린다 (만료 시 봇 대체).
	// 차례가 없어 이탈이 진행을 막지는 않지만, 손패를 든 좌석이 침묵하면
	// 라운드가 끝나지 않으므로 다른 게임과 같은 90초 봇 대체를 그대로 둔다.
	log.Printf("[더마인드][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, MIMessage{
		Type: MIMsgPlayerDisconnected,
		Payload: MIPlayerDisconnectedPayload{
			Seat:         client.Seat,
			Name:         client.Name,
			GraceSeconds: int(h.grace.Seconds()),
		},
	})
	h.broadcastState(room)

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 안에 재접속하지 않은 좌석은 연습봇으로 대체한다
func (h *MIHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == MIPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[더마인드][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := h.takeoverMIBot(room, seat, client.Name)
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.broadcastEvent(room, MIEventPayload{Kind: "bot_takeover", Seat: &seat,
		Name:    client.Name,
		Message: fmt.Sprintf("%s님 자리를 봇이 이어받았습니다", client.Name)})
	// 새 봇이 이 스냅샷(자기 손패 포함)을 받아 곧바로 시계를 돌린다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *MIHub) handleRejoin(client *MIClient, msg MIMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload MIRejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, MIMessage{Type: MIMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, MIMessage{Type: MIMsgSessionExpired})
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

	log.Printf("[더마인드][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, MIMessage{
		Type:    MIMsgPlayerReconnected,
		Payload: MIPlayerReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	// 전원에게 접속 상태가 반영된 스냅샷 (재접속 당사자의 손패 복원 포함)
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *MIHub) clearGameSessions(room *miRoom) {
	clearRoomSessions(&h.sessionManager, room.Clients)
}

// ==================== 전송 ====================

func (h *MIHub) sendError(client *MIClient, message string) {
	h.sendToClient(client, MIMessage{Type: MIMsgError, Payload: MIErrorPayload{Message: message}})
}

func (h *MIHub) sendToClient(client *MIClient, message MIMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[MI] ")
}

func (h *MIHub) broadcastToRoom(room *miRoom, message MIMessage) {
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

func ServeMIWs(hub *MIHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[MI] Error upgrading connection:", err)
		return
	}

	client := &MIClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[MI] ",
		func(msg MIMessage) { hub.gameMessage <- MIGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
