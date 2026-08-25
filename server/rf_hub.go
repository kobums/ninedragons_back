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

// 리포메이션 대기 상태 마감 타이머 — 응답 창은 생존자 전원 통과 또는 마감
// 경과로 닫히고, 행동 차례·카드 제거·교환 방치는 자동 행동으로 해소된다
// (테스트 init 에서 짧게 낮춘다).
var (
	rfTurnTimeout   = 30 * time.Second // action 단계 — 자동 수입(칩 10+면 자동 쿠)
	rfWindowTimeout = 15 * time.Second // 창 통과·무작위 제거·무작위 교환
)

// rfRoom 게임(순수 상태)과 좌석별 연결의 매핑
type rfRoom struct {
	Game       *RFGame
	Clients    map[int]*RFClient // seat → client
	PhaseTimer *time.Timer       // 대기 상태 마감 타이머

	// DeadlineSeq 마지막으로 마감을 건 StateSeq — 같은 대기 상태(창)에
	// 통과가 쌓일 때마다 마감이 늘어나지 않게 하는 근거
	DeadlineSeq int

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string

	// Spectators 관전자 연결 (사설 방 코드로만 진입, 상한 maxSpectators).
	// 좌석·세션이 없어 재접속을 지원하지 않는다 — 끊기면 목록에서만 뗀다.
	Spectators map[*RFClient]bool

	// LastReact 좌석별 마지막 리액션 발신 시각 (레이트리밋 장부)
	LastReact map[int]time.Time
}

// rfPhaseSignal 마감 타이머의 발화 표식 — 대기 상태 일련번호(AfkSeq)로
// 지나간 발화를 구분한다
type rfPhaseSignal struct {
	GameID string
	Seq    int
}

type RFHub struct {
	// 등록된 클라이언트
	clients map[*RFClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*rfRoom

	// 글로벌 로비. 시작 전 공용 방은 하나뿐이다.
	lobby *rfRoom

	// 사설 방 (초대 코드 → 시작 전 방). 허브 고루틴에서만 접근한다.
	privateLobbies map[string]*rfRoom

	// 진행 중 사설 방 (초대 코드 → gameID) — 관전 입장의 근거
	activeCodes map[string]string

	register    chan *RFClient
	unregister  chan *RFClient
	gameMessage chan RFGameMessage

	// 마감 타이머 발화 (time.AfterFunc → 허브 채널 경유)
	phaseFired chan rfPhaseSignal

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*RFClient]

	// 셔플·무작위 제거·자동 행동용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type RFGameMessage struct {
	Client  *RFClient
	Message RFMessage
}

func NewRFHub() *RFHub {
	return &RFHub{
		register:       make(chan *RFClient),
		unregister:     make(chan *RFClient),
		clients:        make(map[*RFClient]bool),
		rooms:          make(map[string]*rfRoom),
		privateLobbies: make(map[string]*rfRoom),
		activeCodes:    make(map[string]string),
		gameMessage:    make(chan RFGameMessage),
		phaseFired:     make(chan rfPhaseSignal, 8),
		sessionManager: newSessionManager[*RFClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *RFHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[RF] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[RF] Client unregistered: %s", client.ID)
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

func (h *RFHub) handleGameMessage(gm RFGameMessage) {
	// 관전자는 어떤 행동도 할 수 없다 (보기 전용 — 리액션도 좌석 보유자만)
	if h.isSpectator(gm.Client) {
		h.sendError(gm.Client, spectatorDeniedMsg)
		return
	}
	switch gm.Message.Type {
	case RFMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case RFMsgReact:
		h.handleReact(gm.Client, gm.Message)
	case RFMsgFillBots:
		h.handleFillBots(gm.Client)
	case RFMsgStart:
		h.handleStart(gm.Client)
	case RFMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case RFMsgAction:
		h.handleAction(gm.Client, gm.Message)
	case RFMsgConvert:
		h.handleConvert(gm.Client)
	case RFMsgConvertOther:
		h.handleConvertOther(gm.Client, gm.Message)
	case RFMsgEmbezzle:
		h.handleEmbezzle(gm.Client)
	case RFMsgPass:
		h.handlePass(gm.Client)
	case RFMsgChallenge:
		h.handleChallenge(gm.Client)
	case RFMsgBlock:
		h.handleBlock(gm.Client, gm.Message)
	case RFMsgLoseCard:
		h.handleLoseCard(gm.Client, gm.Message)
	case RFMsgExchange:
		h.handleExchange(gm.Client, gm.Message)
	}
}

// ==================== 대기실 ====================

func (h *RFHub) handleJoinGame(client *RFClient, msg RFMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload RFJoinGamePayload
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

	log.Printf("[리포메이션][입장] game=%s | seat%d=%s 대기 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), RFMaxPlayers)
	if room.Code == "" { // 사설 방 입장은 조용히 (공용 로비만 알림)
		notify("리포메이션 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), len(room.Game.Players), RFMaxPlayers))
	}

	h.sendToClient(client, RFMessage{
		Type: RFMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
			"roomCode":  room.Code,
		},
	})

	h.broadcastEvent(room, RFEventPayload{Kind: "joined", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 입장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	// 정원이 차도 자동 시작하지 않는다 — 호스트 명시 시작만 (봇 채우기와 충돌 방지)
	h.broadcastState(room)
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다.
func (h *RFHub) lobbyRoomFor(roomField string) *rfRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewRFGame(uuid.New().String())
			h.lobby = &rfRoom{Game: game, Clients: map[int]*RFClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[RF] Created lobby game %s", game.ID)
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
		game := NewRFGame(uuid.New().String())
		room = &rfRoom{Game: game, Clients: map[int]*RFClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[RF] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// ==================== 관전 / 리액션 ====================

// addSpectator 진행 중(또는 가득 찬) 사설 방에 관전자로 등록한다.
func (h *RFHub) addSpectator(room *rfRoom, client *RFClient, name string) {
	if len(room.Spectators) >= maxSpectators {
		h.sendError(client, spectatorFullMsg)
		return
	}
	if room.Spectators == nil {
		room.Spectators = map[*RFClient]bool{}
	}
	client.Name = name
	client.GameID = room.Game.ID
	room.Spectators[client] = true

	log.Printf("[리포메이션][관전] game=%s | %s 관전 입장 (%d명)",
		room.Game.ID, displayName(name), len(room.Spectators))

	h.sendToClient(client, RFMessage{
		Type:    RFMsgSpectateJoined,
		Payload: map[string]interface{}{"gameId": room.Game.ID, "roomCode": room.Code},
	})
	// 전원(관전자 포함)에게 관전자 수가 반영된 스냅샷 — 신규 관전자의 첫 스냅샷 겸용
	h.broadcastState(room)
}

// isSpectator 관전자 연결인지 (좌석 보유자·미입장 연결은 false)
func (h *RFHub) isSpectator(client *RFClient) bool {
	if client.GameID == "" {
		return false
	}
	room := h.rooms[client.GameID]
	return room != nil && room.Spectators[client]
}

// handleReact 리액션 이모지 — 좌석 보유자만, waiting 중에도 허용.
func (h *RFHub) handleReact(client *RFClient, msg RFMessage) {
	room := h.rooms[client.GameID]
	if room == nil || client.Seat < 0 {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload RFReactPayload
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
	h.broadcastEvent(room, RFEventPayload{Kind: "react", Seat: &seat, Name: client.Name, Message: payload.Emoji})
}

// waitingRoomOf 클라이언트가 속한 시작 전 방 (공용 로비 또는 사설 방)
func (h *RFHub) waitingRoomOf(client *RFClient) *rfRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Ready {
		return nil
	}
	return room
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *RFHub) hostSeat(room *rfRoom) int {
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

// rfHumanCount 방의 사람 수
func rfHumanCount(room *rfRoom) int {
	n := 0
	for _, c := range room.Clients {
		if c != nil && !c.Bot {
			n++
		}
	}
	return n
}

// updateLobbyWaiting 로비 현황판 갱신 — 사람 1명 이상 대기 && 미시작.
func (h *RFHub) updateLobbyWaiting(room *rfRoom) {
	if room.Code != "" {
		return
	}
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && rfHumanCount(room) >= 1
	lobbySetWaiting("reformation", waiting)
}

// handleFillBots host 가 빈 좌석을 연습봇으로 5인까지 채운 뒤 즉시 시작한다
func (h *RFHub) handleFillBots(client *RFClient) {
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
	for len(room.Game.Players) < RFFillBotTarget {
		botNo++
		if !h.spawnRFBot(room, fmt.Sprintf("%s%d", botName, botNo)) {
			break
		}
	}

	if room.Game.CanStart() {
		h.startGame(room)
		return
	}
	h.broadcastState(room)
}

func (h *RFHub) handleStart(client *RFClient) {
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
		h.sendError(client, "2명 이상 모여야 시작할 수 있습니다")
		return
	}
	h.startGame(room)
}

func (h *RFHub) startGame(room *rfRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	if room.Code != "" {
		// 시작한 사설 방의 코드는 진행 중 장부로 옮긴다 (관전 입장 근거)
		delete(h.privateLobbies, room.Code)
		h.activeCodes[room.Code] = room.Game.ID
	} else {
		h.lobby = nil // 시작한 방은 로비에서 떼어낸다
		lobbySetWaiting("reformation", false)
	}

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, fmt.Sprintf("%s(%s)", displayName(p.Name), rfFactionName(p.Faction)))
	}
	log.Printf("[리포메이션][경기시작] game=%s | 인원=%d | 선=seat%d | 충성파=%d 개혁파=%d | %v",
		room.Game.ID, len(room.Game.Players), room.Game.CurrentSeat,
		room.Game.FactionCount(RFFactionLoyalist), room.Game.FactionCount(RFFactionReformist), names)
	if !rfRoomHasBot(room) { // 연습봇전은 운영자 알림을 억제한다
		notify("리포메이션 게임 시작", fmt.Sprintf("%d인전 시작", len(room.Game.Players)))
	}

	first := room.Game.CurrentSeat
	h.broadcastEvent(room, RFEventPayload{Kind: "game_started", Seat: &first,
		Name: room.Game.Players[first].Name,
		Message: fmt.Sprintf("게임 시작 — %s님부터 (비공개 카드 %d장, 시작 은화 %d개, 피난처 0)",
			room.Game.Players[first].Name, RFCardsPerPlayer, RFStartChips)})
	h.afterProgress(room)
}

// removeFromLobby 대기실에서 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *RFHub) removeFromLobby(room *rfRoom, client *RFClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	// 좌석 압축: oldSeat 뒤의 클라이언트를 한 칸씩 당긴다
	rebuilt := map[int]*RFClient{}
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

	log.Printf("[리포메이션][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), RFMaxPlayers)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if rfHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, RFMessage{Type: RFMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			lobbySetWaiting("reformation", false)
		}
		return
	}

	h.broadcastEvent(room, RFEventPayload{Kind: "left", Name: client.Name,
		Message: fmt.Sprintf("%s님이 퇴장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 행동 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *RFHub) roomOf(client *RFClient) *rfRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

func (h *RFHub) handleAction(client *RFClient, msg RFMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload RFActionPayload
	json.Unmarshal(payloadBytes, &payload)

	target := -1
	if payload.TargetSeat != nil {
		target = *payload.TargetSeat
	}
	game := room.Game
	if err := game.DeclareAction(client.Seat, RFActionKind(payload.Kind), target, h.rng); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[리포메이션][액션] game=%s | seat%d=%s %s (대상 seat%d)",
		game.ID, client.Seat, displayName(client.Name), payload.Kind, target)
	h.afterProgress(room)
}

func (h *RFHub) handleConvert(client *RFClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	if err := room.Game.SubmitConvert(client.Seat, h.rng); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[리포메이션][개종] game=%s | seat%d=%s 자기 개종 (피난처 %d)",
		room.Game.ID, client.Seat, displayName(client.Name), room.Game.Treasury)
	h.afterProgress(room)
}

func (h *RFHub) handleConvertOther(client *RFClient, msg RFMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload RFConvertOtherPayload
	json.Unmarshal(payloadBytes, &payload)

	target := -1
	if payload.TargetSeat != nil {
		target = *payload.TargetSeat
	}
	if err := room.Game.SubmitConvertOther(client.Seat, target, h.rng); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[리포메이션][개종] game=%s | seat%d=%s → seat%d 개종 (피난처 %d)",
		room.Game.ID, client.Seat, displayName(client.Name), target, room.Game.Treasury)
	h.afterProgress(room)
}

func (h *RFHub) handleEmbezzle(client *RFClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	if err := room.Game.SubmitEmbezzle(client.Seat, h.rng); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[리포메이션][횡령] game=%s | seat%d=%s 피난처 은화 %d개 노림",
		room.Game.ID, client.Seat, displayName(client.Name), room.Game.Treasury)
	h.afterProgress(room)
}

// handlePass 창 통과 동의 — 뒤늦은(이미 지나간 창) 통과는 순수 규칙이
// 조용히 무시한다 (창 경쟁의 정상 경로라 에러로 취급하지 않는다)
func (h *RFHub) handlePass(client *RFClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	room.Game.SubmitPass(client.Seat, h.rng)
	h.afterProgress(room)
}

// handleChallenge 도전 — 뒤늦은 도전 역시 조용히 무시된다
func (h *RFHub) handleChallenge(client *RFClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	room.Game.SubmitChallenge(client.Seat, h.rng)
	h.afterProgress(room)
}

func (h *RFHub) handleBlock(client *RFClient, msg RFMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload RFBlockPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.SubmitBlock(client.Seat, RFRole(payload.Role)); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.afterProgress(room)
}

func (h *RFHub) handleLoseCard(client *RFClient, msg RFMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload RFLoseCardPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.SubmitLoseCard(client.Seat, payload.Index, h.rng); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.afterProgress(room)
}

func (h *RFHub) handleExchange(client *RFClient, msg RFMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload RFExchangePayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.SubmitExchange(client.Seat, payload.Keep, h.rng); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.afterProgress(room)
}

// afterProgress 모든 게임 변이 뒤의 공통 마무리 — 이벤트 방송·종료 판정·
// 새 대기 상태의 마감 예약·스냅샷 방송을 한 번에 처리한다.
func (h *RFHub) afterProgress(room *rfRoom) {
	h.drainEvents(room)
	if room.Game.Phase == RFPhaseGameOver {
		h.finishGame(room)
		return
	}
	h.syncDeadline(room)
	h.broadcastState(room)
}

// drainEvents 순수 규칙이 쌓은 이벤트를 rf_event 로 방송한다
func (h *RFHub) drainEvents(room *rfRoom) {
	for _, ev := range room.Game.DrainEvents() {
		payload := RFEventPayload{Kind: ev.Kind, Message: ev.Message}
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
func (h *RFHub) syncDeadline(room *rfRoom) {
	game := room.Game
	if room.DeadlineSeq == game.StateSeq {
		return
	}
	room.DeadlineSeq = game.StateSeq
	dur := rfWindowTimeout
	if game.Phase == RFPhaseAction {
		dur = rfTurnTimeout
	}
	h.scheduleDeadline(room, dur)
}

// scheduleDeadline 마감 타이머 예약. AfkSeq 를 올려 지나간 발화를 구분하고,
// 발화는 허브 채널을 경유하므로 허브 밖에서 상태를 만지지 않는다.
func (h *RFHub) scheduleDeadline(room *rfRoom, dur time.Duration) {
	h.stopPhaseTimer(room)
	room.Game.AfkSeq++
	room.Game.Deadline = time.Now().Add(dur).UnixMilli()
	sig := rfPhaseSignal{GameID: room.Game.ID, Seq: room.Game.AfkSeq}
	room.PhaseTimer = time.AfterFunc(dur, func() {
		h.phaseFired <- sig
	})
}

func (h *RFHub) stopPhaseTimer(room *rfRoom) {
	if room.PhaseTimer != nil {
		room.PhaseTimer.Stop()
		room.PhaseTimer = nil
	}
}

// handlePhaseFired 마감 발화 — 단계별 자동 행동으로 해소한다.
//   - action: 자동 수입 (칩 10+면 쿠 강제라 자동 쿠 — 같은 진영은 제외)
//   - 응답 창: 전원 통과와 같은 처리
//   - lose_card: 무작위 카드 제거
//   - exchange: 무작위 유지
func (h *RFHub) handlePhaseFired(sig rfPhaseSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.Game.AfkSeq != sig.Seq {
		return
	}
	game := room.Game
	switch game.Phase {
	case RFPhaseAction:
		seat := game.CurrentSeat
		if seat < 0 {
			return
		}
		actor := game.Players[seat]
		h.broadcastEvent(room, RFEventPayload{Kind: "afk", Seat: &seat, Name: actor.Name,
			Message: fmt.Sprintf("%s님이 오래 응답하지 않아 자동으로 진행합니다", actor.Name)})
		if actor.Chips >= RFForceCoupChips {
			if target := game.AutoAttackTarget(seat); target >= 0 {
				game.DeclareAction(seat, RFActCoup, target, h.rng)
			}
		} else {
			game.DeclareAction(seat, RFActIncome, -1, h.rng)
		}
		log.Printf("[리포메이션][자동진행] game=%s | seat%d 무응답 — 자동 행동", game.ID, seat)

	case RFPhaseChallengeWindow, RFPhaseBlockWindow:
		log.Printf("[리포메이션][창통과] game=%s | %s 마감 — 통과", game.ID, game.Phase)
		game.ForcePassWindow(h.rng)

	case RFPhaseLoseCard:
		seat := game.LoseSeat
		if seat < 0 {
			return
		}
		n := len(game.Players[seat].HiddenIdx())
		if n == 0 {
			return
		}
		h.broadcastEvent(room, RFEventPayload{Kind: "afk", Seat: &seat, Name: game.Players[seat].Name,
			Message: fmt.Sprintf("%s님이 카드를 선택하지 않아 자동으로 무작위 제거합니다", game.Players[seat].Name)})
		game.SubmitLoseCard(seat, h.rng.Intn(n), h.rng)
		log.Printf("[리포메이션][자동제거] game=%s | seat%d 무응답 — 무작위 제거", game.ID, seat)

	case RFPhaseExchange:
		game.AutoExchange(h.rng)
		log.Printf("[리포메이션][자동교환] game=%s | 무응답 — 무작위 유지", game.ID)

	default:
		return
	}
	h.afterProgress(room)
}

// ==================== 상태 뷰 (은닉의 핵심) ====================

// buildRFState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를
// 쓴다.
//
// 은닉: yourRoles(비공개 카드)·yourExchange(교환 선택지)는 포인터 슬라이스라
// 본인이 아닌 뷰어에게는 JSON 키 자체가 사라진다. 관전자(viewerSeat -1)도
// 마찬가지다. 공개되는 것은 잃은 카드(lostRoles)·장수(cardCount)·진영
// (faction)·국고(treasury)뿐이다.
func (h *RFHub) buildRFState(room *rfRoom, viewerSeat int) RFGameStatePayload {
	game := room.Game

	var yourRoles *[]string
	if viewerSeat >= 0 && viewerSeat < len(game.Players) {
		roles := []string{}
		for _, r := range game.Players[viewerSeat].HiddenRoles() {
			roles = append(roles, string(r))
		}
		yourRoles = &roles
	}
	var yourExchange *[]string
	if game.Phase == RFPhaseExchange && game.Pending != nil &&
		viewerSeat >= 0 && viewerSeat == game.Pending.ActorSeat {
		cards := []string{}
		for _, r := range game.ExchangeCards {
			cards = append(cards, string(r))
		}
		yourExchange = &cards
	}

	players := []RFPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		players = append(players, RFPlayerView{
			Seat:      p.Seat,
			Name:      p.Name,
			Connected: c != nil && c.Connected,
			Bot:       c != nil && c.Bot,
			Coins:     p.Chips,
			Alive:     !game.Ready || p.Alive(), // 시작 전에는 카드가 없어도 생존 표기
			Faction:   p.Faction,
			LostRoles: p.LostRoles(),
			CardCount: len(p.HiddenIdx()),
		})
	}

	var pending *RFPendingView
	if game.Pending != nil {
		passed := []int{}
		for seat := range game.Pending.passed {
			passed = append(passed, seat)
		}
		sort.Ints(passed)
		pending = &RFPendingView{
			Kind:        string(game.Pending.Kind),
			BySeat:      game.Pending.ActorSeat,
			TargetSeat:  game.Pending.TargetSeat,
			ClaimRole:   string(game.Pending.ClaimRole),
			BlockRole:   string(game.Pending.BlockRole),
			BlockerSeat: game.Pending.BlockerSeat,
			Passed:      passed,
			Message:     game.Pending.Message,
		}
	}

	var lastAction *RFLastActionView
	if game.LastAction != nil {
		lastAction = &RFLastActionView{
			Seat:    game.LastAction.Seat,
			Name:    game.LastAction.Name,
			Message: game.LastAction.Message,
		}
	}

	var result *RFResultView
	if game.Result != nil {
		result = &RFResultView{
			Winner:      game.Result.Winner,
			WinnerSeats: append([]int{}, game.Result.WinnerSeats...),
			WinnerNames: append([]string{}, game.Result.WinnerNames...),
			Message:     game.Result.Message,
		}
	}

	endsAt := int64(0)
	switch game.Phase {
	case RFPhaseAction, RFPhaseChallengeWindow, RFPhaseBlockWindow,
		RFPhaseLoseCard, RFPhaseExchange:
		endsAt = game.Deadline
	}

	return RFGameStatePayload{
		GameID:       game.ID,
		RoomCode:     room.Code,
		Phase:        game.Phase,
		HostSeat:     h.hostSeat(room),
		YourSeat:     viewerSeat,
		Spectators:   len(room.Spectators),
		EndsAt:       endsAt,
		CurrentSeat:  game.CurrentSeat,
		Treasury:     game.Treasury,
		YourRoles:    yourRoles,
		YourExchange: yourExchange,
		Pending:      pending,
		Players:      players,
		LastAction:   lastAction,
		Result:       result,
		LoseSeat:     game.LoseSeat,
		DeckCount:    len(game.Deck),
	}
}

// broadcastState 좌석마다 개인화 스냅샷을 만들어 보낸다.
// 관전자에게는 공개 정보만 담긴 스냅샷(viewerSeat -1)이 간다.
func (h *RFHub) broadcastState(room *rfRoom) {
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, RFMessage{
			Type:    RFMsgGameState,
			Payload: h.buildRFState(room, seat),
		})
	}
	if len(room.Spectators) > 0 {
		msg := RFMessage{Type: RFMsgGameState, Payload: h.buildRFState(room, -1)}
		for c := range room.Spectators {
			h.sendToClient(c, msg)
		}
	}
}

func (h *RFHub) broadcastEvent(room *rfRoom, event RFEventPayload) {
	h.broadcastToRoom(room, RFMessage{Type: RFMsgEvent, Payload: event})
}

// finishGame 게임 종료 발표·전적 기록·방 정리. 진영 승리는 승자가 여럿이라
// 아발론(av_hub.go)의 진영전 기록 형식(승자·패자 이름 묶음)을 따른다.
func (h *RFHub) finishGame(room *rfRoom) {
	game := room.Game
	h.stopPhaseTimer(room)
	game.Deadline = 0

	res := game.Result
	if res == nil { // 방어 — 규칙상 종료 시 항상 채워진다
		res = &RFResult{Winner: "seat", WinnerSeats: []int{}, WinnerNames: []string{},
			Message: "게임이 종료되었습니다"}
	}
	winnerSeats := append([]int{}, res.WinnerSeats...)
	winnerNames := append([]string{}, res.WinnerNames...)

	winnerSet := map[int]bool{}
	for _, s := range winnerSeats {
		winnerSet[s] = true
	}
	winners, losers := []string{}, []string{}
	for _, p := range game.Players {
		if winnerSet[p.Seat] {
			winners = append(winners, displayName(p.Name))
		} else {
			losers = append(losers, displayName(p.Name))
		}
	}

	reason := "last_standing"
	if res.Winner != "seat" {
		reason = "faction_" + res.Winner + " (" + rfFactionName(RFFaction(res.Winner)) + " 진영 승리)"
	}

	h.broadcastEvent(room, RFEventPayload{Kind: "game_over", Message: res.Message})
	// 최종 스냅샷을 먼저 보낸 뒤 종료를 알린다
	// (봇 러너는 rf_game_over 를 보고 스스로 끝난다)
	h.broadcastState(room)
	h.broadcastToRoom(room, RFMessage{
		Type: RFMsgGameOver,
		Payload: RFGameOverPayload{
			Winner:      res.Winner,
			WinnerSeats: winnerSeats,
			WinnerNames: winnerNames,
			Message:     res.Message,
			Players:     h.buildRFState(room, -1).Players,
		},
	})

	log.Printf("[리포메이션][경기결과] game=%s | 승리=%s | 승자=%v | 소요=%s",
		game.ID, res.Winner, winners, matchDuration(game.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "reformation",
		Players:  strings.Join(winners, "·") + " vs " + strings.Join(losers, "·"),
		Winner:   strings.Join(winners, "·"),
		Reason:   reason,
		Duration: matchSeconds(game.StartedAt),
		Bot:      rfRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
	if room.Code != "" {
		delete(h.activeCodes, room.Code) // 코드 재사용 복귀
	}
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *RFHub) handleDisconnect(client *RFClient) {
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
	log.Printf("[리포메이션][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, RFMessage{
		Type: RFMsgPlayerDisconnected,
		Payload: RFPlayerDisconnectedPayload{
			Seat:         client.Seat,
			Name:         client.Name,
			GraceSeconds: int(h.grace.Seconds()),
		},
	})
	h.broadcastState(room)

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 안에 재접속하지 않은 좌석은 연습봇으로 대체하고
// 게임은 계속한다 — 창 통과·카드 제거가 이탈 좌석에 막히지 않는 근거
func (h *RFHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == RFPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[리포메이션][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := h.takeoverRFBot(room, seat, client.Name)
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.broadcastEvent(room, RFEventPayload{Kind: "bot_takeover", Seat: &seat,
		Name:    client.Name,
		Message: fmt.Sprintf("%s님 자리를 봇이 이어받았습니다", client.Name)})
	// 새 봇이 이 스냅샷을 받아 응답이 남았으면 즉시 이어간다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *RFHub) handleRejoin(client *RFClient, msg RFMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload RFRejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, RFMessage{Type: RFMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, RFMessage{Type: RFMsgSessionExpired})
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

	log.Printf("[리포메이션][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, RFMessage{
		Type:    RFMsgPlayerReconnected,
		Payload: RFPlayerReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	// 전원에게 접속 상태가 반영된 스냅샷 (재접속 당사자 복원 포함)
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *RFHub) clearGameSessions(room *rfRoom) {
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		h.drop(c.SessionID)
	}
}

// ==================== 전송 ====================

func (h *RFHub) sendError(client *RFClient, message string) {
	h.sendToClient(client, RFMessage{Type: RFMsgError, Payload: RFErrorPayload{Message: message}})
}

func (h *RFHub) sendToClient(client *RFClient, message RFMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[RF] ")
}

func (h *RFHub) broadcastToRoom(room *rfRoom, message RFMessage) {
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

func ServeRFWs(hub *RFHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[RF] Error upgrading connection:", err)
		return
	}

	client := &RFClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[RF] ",
		func(msg RFMessage) { hub.gameMessage <- RFGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
