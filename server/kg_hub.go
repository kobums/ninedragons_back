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

// 스컬킹 대기 상태 마감 타이머 — 비딩 45초 미제출은 0으로 자동 제출하고,
// 플레이 45초 무응답은 가장 약한 합법 카드를 자동으로 내며, 라운드 정산은
// 5초 뒤 자동으로 다음 라운드를 연다 (테스트에서 짧게 낮춘다).
var (
	kgBidTimeout    = 45 * time.Second // bidding — 미제출 0 자동
	kgPlayTimeout   = 45 * time.Second // playing — 최약 카드 자동
	kgRoundEndDelay = 5 * time.Second  // round_end — 자동 다음 라운드
)

// kgRoom 게임(순수 상태)과 좌석별 연결의 매핑
type kgRoom struct {
	Game       *KGGame
	Clients    map[int]*KGClient // seat → client
	PhaseTimer *time.Timer       // 대기 상태 마감 타이머

	// DeadlineSeq 마지막으로 마감을 건 StateSeq — 같은 대기 상태에 스냅샷이
	// 쌓일 때마다(비드 제출·관전 입장 등) 마감이 늘어나지 않게 하는 근거
	DeadlineSeq int

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string

	// Spectators 관전자 연결 (사설 방 코드로만 진입, 상한 maxSpectators).
	// 좌석·세션이 없어 재접속을 지원하지 않는다 — 끊기면 목록에서만 뗀다.
	Spectators map[*KGClient]bool

	// LastReact 좌석별 마지막 리액션 발신 시각 (레이트리밋 장부)
	LastReact map[int]time.Time
}

// kgPhaseSignal 마감 타이머의 발화 표식 — 대기 상태 일련번호(AfkSeq)로
// 지나간 발화를 구분한다
type kgPhaseSignal struct {
	GameID string
	Seq    int
}

type KGHub struct {
	// 등록된 클라이언트
	clients map[*KGClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*kgRoom

	// 글로벌 로비. 시작 전 공용 방은 하나뿐이다.
	lobby *kgRoom

	// 사설 방 (초대 코드 → 시작 전 방). 허브 고루틴에서만 접근한다.
	// 시작하면 privateLobbies 에서 activeCodes 로 옮긴다.
	privateLobbies map[string]*kgRoom

	// 진행 중 사설 방 (초대 코드 → gameID). 시작 후에도 코드로 방을 찾아
	// 관전 입장시키는 근거다. finishGame 에서 해제한다 (코드 재사용 복귀).
	activeCodes map[string]string

	// 클라이언트 등록
	register chan *KGClient

	// 클라이언트 등록 해제
	unregister chan *KGClient

	// 게임 메시지
	gameMessage chan KGGameMessage

	// 마감 타이머 발화 (time.AfterFunc → 허브 채널 경유)
	phaseFired chan kgPhaseSignal

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*KGClient]

	// 덱 셔플·선 결정·자동 진행용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type KGGameMessage struct {
	Client  *KGClient
	Message KGMessage
}

func NewKGHub() *KGHub {
	return &KGHub{
		register:       make(chan *KGClient),
		unregister:     make(chan *KGClient),
		clients:        make(map[*KGClient]bool),
		rooms:          make(map[string]*kgRoom),
		privateLobbies: make(map[string]*kgRoom),
		activeCodes:    make(map[string]string),
		gameMessage:    make(chan KGGameMessage),
		phaseFired:     make(chan kgPhaseSignal, 8),
		sessionManager: newSessionManager[*KGClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *KGHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[KG] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[KG] Client unregistered: %s", client.ID)
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

func (h *KGHub) handleGameMessage(gm KGGameMessage) {
	// 관전자는 어떤 행동도 할 수 없다 (보기 전용 — 리액션도 좌석 보유자만)
	if h.isSpectator(gm.Client) {
		h.sendError(gm.Client, spectatorDeniedMsg)
		return
	}
	switch gm.Message.Type {
	case KGMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case KGMsgReact:
		h.handleReact(gm.Client, gm.Message)
	case KGMsgFillBots:
		h.handleFillBots(gm.Client)
	case KGMsgStart:
		h.handleStart(gm.Client)
	case KGMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case KGMsgBid:
		h.handleBid(gm.Client, gm.Message)
	case KGMsgPlay:
		h.handlePlay(gm.Client, gm.Message)
	}
}

// ==================== 대기실 ====================

func (h *KGHub) handleJoinGame(client *KGClient, msg KGMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload KGJoinGamePayload
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

	log.Printf("[스컬킹][입장] game=%s | seat%d=%s 대기 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), KGMaxPlayers)
	if room.Code == "" { // 사설 방 입장은 조용히 (공용 로비만 알림)
		notify("스컬킹 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), len(room.Game.Players), KGMaxPlayers))
	}

	h.sendToClient(client, KGMessage{
		Type: KGMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
			"roomCode":  room.Code,
		},
	})

	h.broadcastEvent(room, KGEventPayload{Kind: "joined", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 입장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	// 정원이 차도 자동 시작하지 않는다 — 호스트 명시 시작만 (봇 채우기와 충돌 방지)
	h.broadcastState(room)
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다.
// 생략/빈 문자열은 기존 공용 로비 경로 그대로, "NEW"는 새 코드 발급,
// 그 외 코드는 해당 사설 방 (없으면 그 코드로 관대하게 새로 생성).
func (h *KGHub) lobbyRoomFor(roomField string) *kgRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewKGGame(uuid.New().String())
			h.lobby = &kgRoom{Game: game, Clients: map[int]*KGClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[KG] Created lobby game %s", game.ID)
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
		game := NewKGGame(uuid.New().String())
		room = &kgRoom{Game: game, Clients: map[int]*KGClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[KG] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// ==================== 관전 / 리액션 ====================

// addSpectator 진행 중(또는 가득 찬) 사설 방에 관전자로 등록한다.
// 좌석·세션 없음 — 재접속 미지원, 끊기면 다시 코드로 들어온다.
func (h *KGHub) addSpectator(room *kgRoom, client *KGClient, name string) {
	if len(room.Spectators) >= maxSpectators {
		h.sendError(client, spectatorFullMsg)
		return
	}
	if room.Spectators == nil {
		room.Spectators = map[*KGClient]bool{}
	}
	client.Name = name
	client.GameID = room.Game.ID
	room.Spectators[client] = true

	log.Printf("[스컬킹][관전] game=%s | %s 관전 입장 (%d명)",
		room.Game.ID, displayName(name), len(room.Spectators))

	h.sendToClient(client, KGMessage{
		Type:    KGMsgSpectateJoined,
		Payload: map[string]interface{}{"gameId": room.Game.ID, "roomCode": room.Code},
	})
	// 전원(관전자 포함)에게 관전자 수가 반영된 스냅샷 — 신규 관전자의 첫 스냅샷 겸용
	h.broadcastState(room)
}

// isSpectator 관전자 연결인지 (좌석 보유자·미입장 연결은 false)
func (h *KGHub) isSpectator(client *KGClient) bool {
	if client.GameID == "" {
		return false
	}
	room := h.rooms[client.GameID]
	return room != nil && room.Spectators[client]
}

// handleReact 리액션 이모지 — 좌석 보유자만, waiting 중에도 허용.
// 화이트리스트 외·레이트리밋 초과는 조용히 무시한다 (상태 저장 없음).
func (h *KGHub) handleReact(client *KGClient, msg KGMessage) {
	room := h.rooms[client.GameID]
	if room == nil || client.Seat < 0 {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload KGReactPayload
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
	h.broadcastEvent(room, KGEventPayload{Kind: "react", Seat: &seat, Name: client.Name,
		Message: payload.Emoji})
}

// waitingRoomOf 클라이언트가 속한 시작 전 방 (공용 로비 또는 사설 방)
func (h *KGHub) waitingRoomOf(client *KGClient) *kgRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Ready {
		return nil
	}
	return room
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *KGHub) hostSeat(room *kgRoom) int {
	return hostSeatOf(room.Clients)
}

// kgHumanCount 방의 사람 수
func kgHumanCount(room *kgRoom) int {
	return humanCountOf(room.Clients)
}

// updateLobbyWaiting 로비 현황판 갱신 — 사람 1명 이상 대기 && 미시작.
// 사설 방은 현황판에 노출하지 않는다 (초대 링크로만 접근).
func (h *KGHub) updateLobbyWaiting(room *kgRoom) {
	if room.Code != "" {
		return
	}
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && kgHumanCount(room) >= 1
	lobbySetWaiting("skullking", waiting)
}

// handleFillBots host 가 빈 좌석을 연습봇으로 5인까지 채운 뒤 즉시
// 시작한다 (스폰은 join 을 거치지 않으므로 명시 시작 호출).
func (h *KGHub) handleFillBots(client *KGClient) {
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
	for len(room.Game.Players) < KGFillBotTarget {
		botNo++
		if !h.spawnKGBot(room, fmt.Sprintf("%s%d", botName, botNo)) {
			break
		}
	}

	if room.Game.CanStart() {
		h.startGame(room)
		return
	}
	h.broadcastState(room)
}

func (h *KGHub) handleStart(client *KGClient) {
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
		h.sendError(client, fmt.Sprintf("%d명 이상 모여야 시작할 수 있습니다", KGMinPlayers))
		return
	}
	h.startGame(room)
}

func (h *KGHub) startGame(room *kgRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	if room.Code != "" {
		// 시작한 사설 방의 코드는 진행 중 장부로 옮긴다 (관전 입장 근거)
		delete(h.privateLobbies, room.Code)
		h.activeCodes[room.Code] = room.Game.ID
	} else {
		h.lobby = nil // 시작한 방은 로비에서 떼어낸다
		lobbySetWaiting("skullking", false)
	}

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	log.Printf("[스컬킹][경기시작] game=%s | 인원=%d | 최대라운드=%d | 선=seat%d | %v",
		room.Game.ID, len(room.Game.Players), room.Game.MaxRound, room.Game.StartSeat, names)
	if !kgRoomHasBot(room) { // 연습봇전은 운영자 알림을 억제한다
		notify("스컬킹 시작", fmt.Sprintf("%d인전 시작", len(room.Game.Players)))
	}

	h.broadcastEvent(room, KGEventPayload{Kind: "game_started",
		Message: fmt.Sprintf(
			"게임 시작 — %d인전, 총 %d라운드입니다. 라운드마다 라운드 수만큼 카드를 받아 몇 트릭을 먹을지 먼저 비드합니다",
			len(room.Game.Players), room.Game.MaxRound)})
	h.afterProgress(room)
}

// removeFromLobby 대기실에서 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *KGHub) removeFromLobby(room *kgRoom, client *KGClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	// 좌석 압축: oldSeat 뒤의 클라이언트를 한 칸씩 당긴다
	rebuilt := map[int]*KGClient{}
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

	log.Printf("[스컬킹][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), KGMaxPlayers)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if kgHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, KGMessage{Type: KGMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			lobbySetWaiting("skullking", false)
		}
		return
	}

	h.broadcastEvent(room, KGEventPayload{Kind: "left", Name: client.Name,
		Message: fmt.Sprintf("%s님이 퇴장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *KGHub) roomOf(client *KGClient) *kgRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

// handleBid 비드 제출 — 값은 비딩이 끝날 때까지 남에게 보이지 않는다
func (h *KGHub) handleBid(client *KGClient, msg KGMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload KGBidPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.SubmitBid(client.Seat, payload.Bid); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[스컬킹][비드] game=%s | R%d seat%d=%s 비드 제출 (%d/%d명)",
		room.Game.ID, room.Game.Round, client.Seat, displayName(client.Name),
		kgBidCount(room.Game), len(room.Game.Players))
	h.afterProgress(room)
}

// handlePlay 카드 내기 (본인 손패 인덱스)
func (h *KGHub) handlePlay(client *KGClient, msg KGMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload KGPlayPayload
	json.Unmarshal(payloadBytes, &payload)

	round, trickNo := room.Game.Round, room.Game.TrickNo
	if err := room.Game.Play(client.Seat, payload.Index); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[스컬킹][플레이] game=%s | R%d T%d seat%d=%s 카드 제출",
		room.Game.ID, round, trickNo, client.Seat, displayName(client.Name))
	h.afterProgress(room)
}

// kgBidCount 제출된 비드 수 (로그용)
func kgBidCount(g *KGGame) int {
	n := 0
	for _, p := range g.Players {
		if p.Bid >= 0 {
			n++
		}
	}
	return n
}

// afterProgress 모든 게임 변이 뒤의 공통 마무리 — 이벤트 방송·종료 판정·
// 새 대기 상태의 마감 예약·스냅샷 방송을 한 번에 처리한다.
func (h *KGHub) afterProgress(room *kgRoom) {
	h.drainEvents(room)
	if room.Game.Phase == KGPhaseGameOver {
		h.finishGame(room)
		return
	}
	h.syncDeadline(room)
	h.broadcastState(room)
}

// drainEvents 순수 규칙이 쌓은 이벤트를 kg_event 로 방송한다
func (h *KGHub) drainEvents(room *kgRoom) {
	for _, ev := range room.Game.DrainEvents() {
		payload := KGEventPayload{Kind: ev.Kind, Message: ev.Message}
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
// 같은 비딩 창에 제출이 쌓이거나 관전자가 들어와도 마감은 늘어나지 않는다.
func (h *KGHub) syncDeadline(room *kgRoom) {
	game := room.Game
	if room.DeadlineSeq == game.StateSeq {
		return
	}
	room.DeadlineSeq = game.StateSeq
	var dur time.Duration
	switch game.Phase {
	case KGPhaseBidding:
		dur = kgBidTimeout
	case KGPhasePlaying:
		dur = kgPlayTimeout
	case KGPhaseRoundEnd:
		dur = kgRoundEndDelay
	default:
		h.stopPhaseTimer(room)
		return
	}
	h.scheduleDeadline(room, dur)
}

// scheduleDeadline 마감 타이머 예약. AfkSeq 를 올려 지나간 발화를 구분하고,
// 발화는 허브 채널을 경유하므로 허브 밖에서 상태를 만지지 않는다.
func (h *KGHub) scheduleDeadline(room *kgRoom, dur time.Duration) {
	h.stopPhaseTimer(room)
	room.Game.AfkSeq++
	room.Game.Deadline = time.Now().Add(dur).UnixMilli()
	sig := kgPhaseSignal{GameID: room.Game.ID, Seq: room.Game.AfkSeq}
	room.PhaseTimer = time.AfterFunc(dur, func() {
		h.phaseFired <- sig
	})
}

func (h *KGHub) stopPhaseTimer(room *kgRoom) {
	stopTimer(&room.PhaseTimer)
}

// handlePhaseFired 마감 발화 — 단계별 자동 행동으로 해소한다.
//   - bidding: 미제출 좌석을 0으로 자동 제출하고 일괄 공개
//   - playing: 현재 좌석의 가장 약한 합법 카드를 자동 제출
//   - round_end: 다음 라운드 개시 (마지막 라운드면 종료)
func (h *KGHub) handlePhaseFired(sig kgPhaseSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.Game.AfkSeq != sig.Seq {
		return
	}
	game := room.Game
	switch game.Phase {
	case KGPhaseBidding:
		game.ForceBids()
		log.Printf("[스컬킹][자동진행] game=%s | R%d 비딩 마감 — 미제출 0 자동 제출",
			game.ID, game.Round)

	case KGPhasePlaying:
		seat := game.CurrentSeat
		if seat < 0 || seat >= len(game.Players) {
			return
		}
		actor := game.Players[seat]
		h.broadcastEvent(room, KGEventPayload{Kind: "afk", Seat: &seat, Name: actor.Name,
			Message: fmt.Sprintf("%s님이 오래 응답하지 않아 자동으로 카드를 냅니다", actor.Name)})
		game.ForcePlay()
		log.Printf("[스컬킹][자동진행] game=%s | R%d T%d seat%d 무응답 — 자동 제출",
			game.ID, game.Round, game.TrickNo, seat)

	case KGPhaseRoundEnd:
		game.NextRound(h.rng)
		log.Printf("[스컬킹][라운드] game=%s | %d라운드 (1인 %d장)",
			game.ID, game.Round, game.Round)

	default:
		return
	}
	h.afterProgress(room)
}

// ==================== 상태 뷰 (은닉의 핵심) ====================

// buildKGState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
//
// 은닉: yourHand·yourBid 는 본인에게만 실린다 — 타인·관전자(viewerSeat -1)의
// raw JSON 에는 키 자체가 없다 (nil 포인터 생략). yourHand 는 빈 손패도 [] 로
// 보내야 하므로 슬라이스 포인터를 쓰고, yourBid 는 미제출이 -1 이라 값으로
// 부재를 표현할 수 없어 정수 포인터를 쓴다.
// players[].bid 는 비딩이 끝나기 전까지 전원 -1 이다.
func (h *KGHub) buildKGState(room *kgRoom, viewerSeat int) KGGameStatePayload {
	game := room.Game
	seated := viewerSeat >= 0 && viewerSeat < len(game.Players)

	var yourHand *[]KGCard
	var yourBid *int
	if seated {
		hand := append([]KGCard{}, game.Players[viewerSeat].Hand...)
		yourHand = &hand
		bid := game.Players[viewerSeat].Bid
		yourBid = &bid
	}

	players := []KGPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		bid := p.Bid
		if !game.BidsRevealed { // 비딩 진행 중에는 전원 -1 (제출 여부만 공개)
			bid = -1
		}
		players = append(players, KGPlayerView{
			Seat:         p.Seat,
			Name:         p.Name,
			Connected:    c != nil && c.Connected,
			Bot:          c != nil && c.Bot,
			Bid:          bid,
			Tricks:       p.Tricks,
			Score:        p.Score,
			HandCount:    len(p.Hand),
			BidSubmitted: p.Bid >= 0,
		})
	}

	endsAt := int64(0)
	switch game.Phase {
	case KGPhaseBidding, KGPhasePlaying, KGPhaseRoundEnd:
		endsAt = game.Deadline
	}

	return KGGameStatePayload{
		GameID:      game.ID,
		RoomCode:    room.Code,
		Phase:       game.Phase,
		HostSeat:    h.hostSeat(room),
		YourSeat:    viewerSeat,
		Spectators:  len(room.Spectators),
		EndsAt:      endsAt,
		Round:       game.Round,
		MaxRound:    game.MaxRound,
		TrickNo:     game.TrickNo,
		CurrentSeat: game.CurrentSeat,
		LeadSuit:    game.LeadSuit,
		Trick:       append([]KGTrickPlay{}, game.Trick...),
		YourHand:    yourHand,
		YourBid:     yourBid,
		Players:     players,
		LastTrick:   game.LastTrick,
		RoundResult: game.RoundResult,
	}
}

// broadcastState 좌석마다 개인화 스냅샷을 만들어 보낸다.
// 관전자에게는 공개 정보만 담긴 스냅샷(viewerSeat -1)이 간다.
func (h *KGHub) broadcastState(room *kgRoom) {
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, KGMessage{
			Type:    KGMsgGameState,
			Payload: h.buildKGState(room, seat),
		})
	}
	if len(room.Spectators) > 0 {
		msg := KGMessage{Type: KGMsgGameState, Payload: h.buildKGState(room, -1)}
		for c := range room.Spectators {
			h.sendToClient(c, msg)
		}
	}
}

func (h *KGHub) broadcastEvent(room *kgRoom, event KGEventPayload) {
	h.broadcastToRoom(room, KGMessage{Type: KGMsgEvent, Payload: event})
}

// finishGame 게임 종료 발표·전적 기록·방 정리 (재대결은 같은 방 코드
// 재입장으로 — finishGame 이 코드를 반납해 같은 코드로 새 방을 만들 수 있다)
func (h *KGHub) finishGame(room *kgRoom) {
	game := room.Game
	h.stopPhaseTimer(room)
	game.Deadline = 0

	winnerNames := game.WinnerNames()
	best := game.BestScore()
	message := fmt.Sprintf("게임 종료 — %s님이 %d점으로 우승했습니다", winnerNames, best)
	reason := "score"
	if len(game.Winners) > 1 {
		message = fmt.Sprintf("게임 종료 — %s님이 %d점으로 공동 우승했습니다", winnerNames, best)
		reason = "tie"
	}

	winners, losers := []string{}, []string{}
	winnerSeats := map[int]bool{}
	for _, seat := range game.Winners {
		winnerSeats[seat] = true
	}
	for _, p := range game.Players {
		if winnerSeats[p.Seat] {
			winners = append(winners, displayName(p.Name))
		} else {
			losers = append(losers, displayName(p.Name))
		}
	}

	// 전원 최종 점수가 담긴 스냅샷을 먼저 보낸 뒤 종료를 알린다
	// (봇 러너는 kg_game_over 를 보고 스스로 끝난다)
	h.broadcastState(room)
	h.broadcastToRoom(room, KGMessage{
		Type: KGMsgGameOver,
		Payload: KGGameOverPayload{
			Winners:     append([]int{}, game.Winners...),
			WinnerNames: winnerNames,
			Message:     message,
			Round:       game.Round,
			MaxRound:    game.MaxRound,
			Players:     h.buildKGState(room, -1).Players,
		},
	})

	log.Printf("[스컬킹][경기결과] game=%s | 우승=%s(%d점) | 사유=%s | R%d/%d | 소요=%s",
		game.ID, winnerNames, best, reason, game.Round, game.MaxRound,
		matchDuration(game.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "skullking",
		Players:  strings.Join(winners, "·") + " vs " + strings.Join(losers, "·"),
		Winner:   strings.Join(winners, "·"),
		Reason:   reason,
		Duration: matchSeconds(game.StartedAt),
		Bot:      kgRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
	if room.Code != "" {
		delete(h.activeCodes, room.Code) // 코드 재사용 복귀
	}
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *KGHub) handleDisconnect(client *KGClient) {
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
	log.Printf("[스컬킹][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, KGMessage{
		Type: KGMsgPlayerDisconnected,
		Payload: KGPlayerDisconnectedPayload{
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
func (h *KGHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == KGPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[스컬킹][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := h.takeoverKGBot(room, seat, client.Name)
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.broadcastEvent(room, KGEventPayload{Kind: "bot_takeover", Seat: &seat,
		Name:    client.Name,
		Message: fmt.Sprintf("%s님 자리를 봇이 이어받았습니다", client.Name)})
	// 새 봇이 이 스냅샷을 받아 자기 차례면 즉시 이어간다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *KGHub) handleRejoin(client *KGClient, msg KGMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload KGRejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, KGMessage{Type: KGMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, KGMessage{Type: KGMsgSessionExpired})
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

	log.Printf("[스컬킹][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, KGMessage{
		Type:    KGMsgPlayerReconnected,
		Payload: KGPlayerReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	// 전원에게 접속 상태가 반영된 스냅샷 (재접속 당사자의 손패·비드 복원 포함)
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *KGHub) clearGameSessions(room *kgRoom) {
	clearRoomSessions(&h.sessionManager, room.Clients)
}

// ==================== 전송 ====================

func (h *KGHub) sendError(client *KGClient, message string) {
	h.sendToClient(client, KGMessage{Type: KGMsgError, Payload: KGErrorPayload{Message: message}})
}

func (h *KGHub) sendToClient(client *KGClient, message KGMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[KG] ")
}

func (h *KGHub) broadcastToRoom(room *kgRoom, message KGMessage) {
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

func ServeKGWs(hub *KGHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[KG] Error upgrading connection:", err)
		return
	}

	client := &KGClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[KG] ",
		func(msg KGMessage) { hub.gameMessage <- KGGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
