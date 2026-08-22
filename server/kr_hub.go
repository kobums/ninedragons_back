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

// 크라켄 대기 상태 마감 타이머 — 지목 45초 미응답은 무작위 대상·무작위 카드
// 자동 지목으로 해소하고, 라운드 정산은 5초 뒤 자동으로 다음 라운드를 연다
// (테스트에서 짧게 낮춘다).
var (
	krPointTimeout  = 45 * time.Second // revealing — 자동 지목
	krRoundEndDelay = 5 * time.Second  // round_end — 자동 재배분
)

// krRoom 게임(순수 상태)과 좌석별 연결의 매핑
type krRoom struct {
	Game       *KRGame
	Clients    map[int]*KRClient // seat → client
	PhaseTimer *time.Timer       // 대기 상태 마감 타이머

	// DeadlineSeq 마지막으로 마감을 건 StateSeq — 같은 대기 상태에 스냅샷이
	// 쌓일 때마다(선언·관전 입장 등) 마감이 늘어나지 않게 하는 근거
	DeadlineSeq int

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string

	// Spectators 관전자 연결 (사설 방 코드로만 진입, 상한 maxSpectators).
	// 좌석·세션이 없어 재접속을 지원하지 않는다 — 끊기면 목록에서만 뗀다.
	Spectators map[*KRClient]bool

	// LastReact 좌석별 마지막 리액션 발신 시각 (레이트리밋 장부)
	LastReact map[int]time.Time
}

// krPhaseSignal 마감 타이머의 발화 표식 — 대기 상태 일련번호(AfkSeq)로
// 지나간 발화를 구분한다
type krPhaseSignal struct {
	GameID string
	Seq    int
}

type KRHub struct {
	// 등록된 클라이언트
	clients map[*KRClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*krRoom

	// 글로벌 로비. 시작 전 공용 방은 하나뿐이다.
	lobby *krRoom

	// 사설 방 (초대 코드 → 시작 전 방). 허브 고루틴에서만 접근한다.
	// 시작하면 privateLobbies 에서 activeCodes 로 옮긴다.
	privateLobbies map[string]*krRoom

	// 진행 중 사설 방 (초대 코드 → gameID). 시작 후에도 코드로 방을 찾아
	// 관전 입장시키는 근거다. finishGame 에서 해제한다 (코드 재사용 복귀).
	activeCodes map[string]string

	// 클라이언트 등록
	register chan *KRClient

	// 클라이언트 등록 해제
	unregister chan *KRClient

	// 게임 메시지
	gameMessage chan KRGameMessage

	// 마감 타이머 발화 (time.AfterFunc → 허브 채널 경유)
	phaseFired chan krPhaseSignal

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*KRClient]

	// 역할 배분·덱 셔플·자동 지목용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type KRGameMessage struct {
	Client  *KRClient
	Message KRMessage
}

func NewKRHub() *KRHub {
	return &KRHub{
		register:       make(chan *KRClient),
		unregister:     make(chan *KRClient),
		clients:        make(map[*KRClient]bool),
		rooms:          make(map[string]*krRoom),
		privateLobbies: make(map[string]*krRoom),
		activeCodes:    make(map[string]string),
		gameMessage:    make(chan KRGameMessage),
		phaseFired:     make(chan krPhaseSignal, 8),
		sessionManager: newSessionManager[*KRClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *KRHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[KR] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[KR] Client unregistered: %s", client.ID)
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

func (h *KRHub) handleGameMessage(gm KRGameMessage) {
	// 관전자는 어떤 행동도 할 수 없다 (보기 전용 — 리액션도 좌석 보유자만)
	if h.isSpectator(gm.Client) {
		h.sendError(gm.Client, spectatorDeniedMsg)
		return
	}
	switch gm.Message.Type {
	case KRMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case KRMsgReact:
		h.handleReact(gm.Client, gm.Message)
	case KRMsgFillBots:
		h.handleFillBots(gm.Client)
	case KRMsgStart:
		h.handleStart(gm.Client)
	case KRMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case KRMsgPoint:
		h.handlePoint(gm.Client, gm.Message)
	case KRMsgClaim:
		h.handleClaim(gm.Client, gm.Message)
	}
}

// ==================== 대기실 ====================

func (h *KRHub) handleJoinGame(client *KRClient, msg KRMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload KRJoinGamePayload
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

	log.Printf("[크라켄][입장] game=%s | seat%d=%s 대기 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), KRMaxPlayers)
	if room.Code == "" { // 사설 방 입장은 조용히 (공용 로비만 알림)
		notify("노 터치 크라켄 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), len(room.Game.Players), KRMaxPlayers))
	}

	h.sendToClient(client, KRMessage{
		Type: KRMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
			"roomCode":  room.Code,
		},
	})

	h.broadcastEvent(room, KREventPayload{Kind: "joined", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 입장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	// 정원이 차도 자동 시작하지 않는다 — 호스트 명시 시작만 (봇 채우기와 충돌 방지)
	h.broadcastState(room)
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다.
// 생략/빈 문자열은 기존 공용 로비 경로 그대로, "NEW"는 새 코드 발급,
// 그 외 코드는 해당 사설 방 (없으면 그 코드로 관대하게 새로 생성).
func (h *KRHub) lobbyRoomFor(roomField string) *krRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewKRGame(uuid.New().String())
			h.lobby = &krRoom{Game: game, Clients: map[int]*KRClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[KR] Created lobby game %s", game.ID)
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
		game := NewKRGame(uuid.New().String())
		room = &krRoom{Game: game, Clients: map[int]*KRClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[KR] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// ==================== 관전 / 리액션 ====================

// addSpectator 진행 중(또는 가득 찬) 사설 방에 관전자로 등록한다.
// 좌석·세션 없음 — 재접속 미지원, 끊기면 다시 코드로 들어온다.
func (h *KRHub) addSpectator(room *krRoom, client *KRClient, name string) {
	if len(room.Spectators) >= maxSpectators {
		h.sendError(client, spectatorFullMsg)
		return
	}
	if room.Spectators == nil {
		room.Spectators = map[*KRClient]bool{}
	}
	client.Name = name
	client.GameID = room.Game.ID
	room.Spectators[client] = true

	log.Printf("[크라켄][관전] game=%s | %s 관전 입장 (%d명)",
		room.Game.ID, displayName(name), len(room.Spectators))

	h.sendToClient(client, KRMessage{
		Type:    KRMsgSpectateJoined,
		Payload: map[string]interface{}{"gameId": room.Game.ID, "roomCode": room.Code},
	})
	// 전원(관전자 포함)에게 관전자 수가 반영된 스냅샷 — 신규 관전자의 첫 스냅샷 겸용
	h.broadcastState(room)
}

// isSpectator 관전자 연결인지 (좌석 보유자·미입장 연결은 false)
func (h *KRHub) isSpectator(client *KRClient) bool {
	if client.GameID == "" {
		return false
	}
	room := h.rooms[client.GameID]
	return room != nil && room.Spectators[client]
}

// handleReact 리액션 이모지 — 좌석 보유자만, waiting 중에도 허용.
// 화이트리스트 외·레이트리밋 초과는 조용히 무시한다 (상태 저장 없음).
func (h *KRHub) handleReact(client *KRClient, msg KRMessage) {
	room := h.rooms[client.GameID]
	if room == nil || client.Seat < 0 {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload KRReactPayload
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
	h.broadcastEvent(room, KREventPayload{Kind: "react", Seat: &seat, Name: client.Name,
		Message: payload.Emoji})
}

// waitingRoomOf 클라이언트가 속한 시작 전 방 (공용 로비 또는 사설 방)
func (h *KRHub) waitingRoomOf(client *KRClient) *krRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Ready {
		return nil
	}
	return room
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *KRHub) hostSeat(room *krRoom) int {
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

// krHumanCount 방의 사람 수
func krHumanCount(room *krRoom) int {
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
func (h *KRHub) updateLobbyWaiting(room *krRoom) {
	if room.Code != "" {
		return
	}
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && krHumanCount(room) >= 1
	lobbySetWaiting("kraken", waiting)
}

// handleFillBots host 가 빈 좌석을 연습봇으로 5인까지 채운 뒤 즉시
// 시작한다 (스폰은 join 을 거치지 않으므로 명시 시작 호출).
func (h *KRHub) handleFillBots(client *KRClient) {
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
	for len(room.Game.Players) < KRFillBotTarget {
		botNo++
		if !h.spawnKRBot(room, fmt.Sprintf("%s%d", botName, botNo)) {
			break
		}
	}

	if room.Game.CanStart() {
		h.startGame(room)
		return
	}
	h.broadcastState(room)
}

func (h *KRHub) handleStart(client *KRClient) {
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
		h.sendError(client, fmt.Sprintf("%d명 이상 모여야 시작할 수 있습니다", KRMinPlayers))
		return
	}
	h.startGame(room)
}

func (h *KRHub) startGame(room *krRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	if room.Code != "" {
		// 시작한 사설 방의 코드는 진행 중 장부로 옮긴다 (관전 입장 근거)
		delete(h.privateLobbies, room.Code)
		h.activeCodes[room.Code] = room.Game.ID
	} else {
		h.lobby = nil // 시작한 방은 로비에서 떼어낸다
		lobbySetWaiting("kraken", false)
	}

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	pool := room.Game.Pool
	log.Printf("[크라켄][경기시작] game=%s | 인원=%d | 풀=탐험대%d:해골%d | 선=seat%d | %v",
		room.Game.ID, len(room.Game.Players), pool.Expedition, pool.Skeleton,
		room.Game.CurrentSeat, names)
	if !krRoomHasBot(room) { // 연습봇전은 운영자 알림을 억제한다
		notify("노 터치 크라켄 시작", fmt.Sprintf("%d인전 시작", len(room.Game.Players)))
	}

	h.broadcastEvent(room, KREventPayload{Kind: "game_started",
		Message: fmt.Sprintf(
			"게임 시작 — %d인전, 역할 풀은 탐험대 %d·해골 %d 중 %d장을 나눠 가집니다. 자기 손패는 자기만 봅니다",
			len(room.Game.Players), pool.Expedition, pool.Skeleton, len(room.Game.Players))})
	h.afterProgress(room)
}

// removeFromLobby 대기실에서 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *KRHub) removeFromLobby(room *krRoom, client *KRClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	// 좌석 압축: oldSeat 뒤의 클라이언트를 한 칸씩 당긴다
	rebuilt := map[int]*KRClient{}
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

	log.Printf("[크라켄][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), KRMaxPlayers)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if krHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, KRMessage{Type: KRMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			lobbySetWaiting("kraken", false)
		}
		return
	}

	h.broadcastEvent(room, KREventPayload{Kind: "left", Name: client.Name,
		Message: fmt.Sprintf("%s님이 퇴장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *KRHub) roomOf(client *KRClient) *krRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

// handlePoint 지목 — 자기 외 좌석의 미공개 카드 1장을 즉시 공개한다
func (h *KRHub) handlePoint(client *KRClient, msg KRMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload KRPointPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.Point(client.Seat, payload.TargetSeat, payload.CardIndex); err != nil {
		h.sendError(client, err.Error())
		return
	}
	card := KRCard("")
	if room.Game.Reveal != nil {
		card = room.Game.Reveal.Card
	}
	log.Printf("[크라켄][공개] game=%s | R%d seat%d=%s → seat%d 카드 → %s (남은 공개 %d)",
		room.Game.ID, room.Game.Round, client.Seat, displayName(client.Name),
		payload.TargetSeat, krCardLabel(card), room.Game.RevealsLeft)
	h.afterProgress(room)
}

// handleClaim 손패 구성 선언 (거짓 허용 — 구속력 없는 디지털 보조)
func (h *KRHub) handleClaim(client *KRClient, msg KRMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload KRClaimPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.SubmitClaim(client.Seat, payload.Treasure, payload.Kraken); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[크라켄][선언] game=%s | seat%d=%s 보물%d 크라켄%d",
		room.Game.ID, client.Seat, displayName(client.Name), payload.Treasure, payload.Kraken)
	h.afterProgress(room)
}

// afterProgress 모든 게임 변이 뒤의 공통 마무리 — 이벤트 방송·종료 판정·
// 새 대기 상태의 마감 예약·스냅샷 방송을 한 번에 처리한다.
func (h *KRHub) afterProgress(room *krRoom) {
	h.drainEvents(room)
	if room.Game.Phase == KRPhaseGameOver {
		h.finishGame(room)
		return
	}
	h.syncDeadline(room)
	h.broadcastState(room)
}

// drainEvents 순수 규칙이 쌓은 이벤트를 kr_event 로 방송한다
func (h *KRHub) drainEvents(room *krRoom) {
	for _, ev := range room.Game.DrainEvents() {
		payload := KREventPayload{Kind: ev.Kind, Message: ev.Message}
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
// 같은 지목 차례에 선언·관전 입장으로 스냅샷이 쌓여도 마감은 늘어나지 않는다.
func (h *KRHub) syncDeadline(room *krRoom) {
	game := room.Game
	if room.DeadlineSeq == game.StateSeq {
		return
	}
	room.DeadlineSeq = game.StateSeq
	var dur time.Duration
	switch game.Phase {
	case KRPhaseRevealing:
		dur = krPointTimeout
	case KRPhaseRoundEnd:
		dur = krRoundEndDelay
	default:
		h.stopPhaseTimer(room)
		return
	}
	h.scheduleDeadline(room, dur)
}

// scheduleDeadline 마감 타이머 예약. AfkSeq 를 올려 지나간 발화를 구분하고,
// 발화는 허브 채널을 경유하므로 허브 밖에서 상태를 만지지 않는다.
func (h *KRHub) scheduleDeadline(room *krRoom, dur time.Duration) {
	h.stopPhaseTimer(room)
	room.Game.AfkSeq++
	room.Game.Deadline = time.Now().Add(dur).UnixMilli()
	sig := krPhaseSignal{GameID: room.Game.ID, Seq: room.Game.AfkSeq}
	room.PhaseTimer = time.AfterFunc(dur, func() {
		h.phaseFired <- sig
	})
}

func (h *KRHub) stopPhaseTimer(room *krRoom) {
	if room.PhaseTimer != nil {
		room.PhaseTimer.Stop()
		room.PhaseTimer = nil
	}
}

// handlePhaseFired 마감 발화 — 단계별 자동 행동으로 해소한다.
//   - revealing: 무작위 대상·무작위 카드 자동 지목
//   - round_end: 남은 카드 회수·재배분으로 다음 라운드 개시
func (h *KRHub) handlePhaseFired(sig krPhaseSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.Game.AfkSeq != sig.Seq {
		return
	}
	game := room.Game
	switch game.Phase {
	case KRPhaseRevealing:
		seat := game.CurrentSeat
		if seat < 0 || seat >= len(game.Players) {
			return
		}
		actor := game.Players[seat]
		h.broadcastEvent(room, KREventPayload{Kind: "afk", Seat: &seat, Name: actor.Name,
			Message: fmt.Sprintf("%s님이 오래 응답하지 않아 자동으로 지목합니다", actor.Name)})
		game.ForcePoint(h.rng)
		log.Printf("[크라켄][자동진행] game=%s | R%d seat%d 무응답 — 자동 지목",
			game.ID, game.Round, seat)

	case KRPhaseRoundEnd:
		game.NextRound(h.rng)
		log.Printf("[크라켄][라운드] game=%s | %d라운드 시작 (1인 %d장)",
			game.ID, game.Round, krHandSize(game.Round))

	default:
		return
	}
	h.afterProgress(room)
}

// ==================== 상태 뷰 (은닉의 핵심) ====================

// buildKRState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
// 은닉: yourRole·yourHand 는 본인에게만 실린다 — 타인·관전자(viewerSeat -1)의
// raw JSON 에는 키 자체가 없다 (빈 문자열·nil 포인터 생략). yourHand 는 빈
// 손패도 [] 로 보내야 하므로 슬라이스 포인터를 쓴다.
// rolePool·found·revealedThisRound·claim 은 전원 공개 정보이고,
// players[].role 은 game_over 전까지 전원 "" 다.
func (h *KRHub) buildKRState(room *krRoom, viewerSeat int) KRGameStatePayload {
	game := room.Game
	seated := viewerSeat >= 0 && viewerSeat < len(game.Players)

	yourRole := ""
	if seated {
		yourRole = string(game.Players[viewerSeat].Role) // 시작 전엔 "" → 필드 생략
	}
	var yourHand *[]KRCard
	if seated && game.Ready {
		hand := append([]KRCard{}, game.Players[viewerSeat].Hand...)
		yourHand = &hand
	}

	revealed := game.Phase == KRPhaseGameOver
	players := []KRPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		role := ""
		if revealed {
			role = string(p.Role)
		}
		players = append(players, KRPlayerView{
			Seat:              p.Seat,
			Name:              p.Name,
			Connected:         c != nil && c.Connected,
			Bot:               c != nil && c.Bot,
			HandCount:         len(p.Hand),
			RevealedThisRound: append([]KRCard{}, p.RevealedThisRound...),
			Claim:             p.Claim,
			Role:              role,
		})
	}

	// 시작 전에는 현재 인원 기준 풀을 미리 보여준다 (표 밖 인원은 0:0)
	pool := game.Pool
	if !game.Ready {
		pool = krRolePoolFor(len(game.Players))
	}

	endsAt := int64(0)
	switch game.Phase {
	case KRPhaseRevealing, KRPhaseRoundEnd:
		endsAt = game.Deadline
	}

	return KRGameStatePayload{
		GameID:      game.ID,
		RoomCode:    room.Code,
		Phase:       game.Phase,
		HostSeat:    h.hostSeat(room),
		YourSeat:    viewerSeat,
		Spectators:  len(room.Spectators),
		EndsAt:      endsAt,
		Round:       game.Round,
		RevealsLeft: game.RevealsLeft,
		CurrentSeat: game.CurrentSeat,
		RolePool:    pool,
		YourRole:    yourRole,
		YourHand:    yourHand,
		Found: KRFound{
			Treasure:      game.FoundTreasure,
			TreasureTotal: len(game.Players),
			Kraken:        game.FoundKraken,
		},
		Players:    players,
		LastReveal: game.Reveal,
		Result:     game.Result,
	}
}

// broadcastState 좌석마다 개인화 스냅샷을 만들어 보낸다.
// 관전자에게는 공개 정보만 담긴 스냅샷(viewerSeat -1)이 간다.
func (h *KRHub) broadcastState(room *krRoom) {
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, KRMessage{
			Type:    KRMsgGameState,
			Payload: h.buildKRState(room, seat),
		})
	}
	if len(room.Spectators) > 0 {
		msg := KRMessage{Type: KRMsgGameState, Payload: h.buildKRState(room, -1)}
		for c := range room.Spectators {
			h.sendToClient(c, msg)
		}
	}
}

func (h *KRHub) broadcastEvent(room *krRoom, event KREventPayload) {
	h.broadcastToRoom(room, KRMessage{Type: KRMsgEvent, Payload: event})
}

// finishGame 게임 종료 발표·전적 기록·방 정리 (재대결은 같은 방 코드
// 재입장으로 — finishGame 이 코드를 반납해 같은 코드로 새 방을 만들 수 있다)
func (h *KRHub) finishGame(room *krRoom) {
	game := room.Game
	h.stopPhaseTimer(room)
	game.Deadline = 0

	result := game.Result
	if result == nil { // 방어선 — 종료는 항상 결과와 함께 온다 (무승부 없음)
		result = &KRResult{Winner: string(KRRoleSkeleton), Reason: "timeout",
			Message: "게임이 종료됐습니다"}
	}

	winners, losers := []string{}, []string{}
	for _, p := range game.Players {
		if string(p.Role) == result.Winner {
			winners = append(winners, displayName(p.Name))
		} else {
			losers = append(losers, displayName(p.Name))
		}
	}

	h.broadcastEvent(room, KREventPayload{Kind: "game_over",
		Message: fmt.Sprintf("게임 종료 — %s의 승리! %s",
			krRoleLabel(result.Winner), result.Message)})
	// 전원 역할이 공개된 최종 스냅샷을 먼저 보낸 뒤 종료를 알린다
	// (봇 러너는 kr_game_over 를 보고 스스로 끝난다)
	h.broadcastState(room)
	h.broadcastToRoom(room, KRMessage{
		Type: KRMsgGameOver,
		Payload: KRGameOverPayload{
			Winner:  result.Winner,
			Reason:  result.Reason,
			Message: result.Message,
			Round:   game.Round,
			Found: KRFound{
				Treasure:      game.FoundTreasure,
				TreasureTotal: len(game.Players),
				Kraken:        game.FoundKraken,
			},
			Players: h.buildKRState(room, -1).Players,
		},
	})

	log.Printf("[크라켄][경기결과] game=%s | 승자=%s(%s) | 사유=%s | 보물=%d/%d | R%d | 소요=%s",
		game.ID, result.Winner, krRoleLabel(result.Winner), result.Reason,
		game.FoundTreasure, len(game.Players), game.Round, matchDuration(game.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "kraken",
		Players:  strings.Join(winners, "·") + " vs " + strings.Join(losers, "·"),
		Winner:   strings.Join(winners, "·"),
		Reason:   result.Reason,
		Duration: matchSeconds(game.StartedAt),
		Bot:      krRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
	if room.Code != "" {
		delete(h.activeCodes, room.Code) // 코드 재사용 복귀
	}
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *KRHub) handleDisconnect(client *KRClient) {
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
	log.Printf("[크라켄][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, KRMessage{
		Type: KRMsgPlayerDisconnected,
		Payload: KRPlayerDisconnectedPayload{
			Seat:         client.Seat,
			Name:         client.Name,
			GraceSeconds: int(h.grace.Seconds()),
		},
	})
	h.broadcastState(room)

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 안에 재접속하지 않은 좌석은 연습봇으로 대체하고
// 게임은 계속한다 — 지목권이 이탈 좌석에 막히지 않는 근거
func (h *KRHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == KRPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[크라켄][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := h.takeoverKRBot(room, seat, client.Name)
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.broadcastEvent(room, KREventPayload{Kind: "bot_takeover", Seat: &seat,
		Name:    client.Name,
		Message: fmt.Sprintf("%s님 자리를 봇이 이어받았습니다", client.Name)})
	// 새 봇이 이 스냅샷을 받아 자기 차례면 즉시 이어간다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *KRHub) handleRejoin(client *KRClient, msg KRMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload KRRejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, KRMessage{Type: KRMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, KRMessage{Type: KRMsgSessionExpired})
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

	log.Printf("[크라켄][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, KRMessage{
		Type:    KRMsgPlayerReconnected,
		Payload: KRPlayerReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	// 전원에게 접속 상태가 반영된 스냅샷 (재접속 당사자의 손패 복원 포함)
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *KRHub) clearGameSessions(room *krRoom) {
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		h.drop(c.SessionID)
	}
}

// ==================== 전송 ====================

func (h *KRHub) sendError(client *KRClient, message string) {
	h.sendToClient(client, KRMessage{Type: KRMsgError, Payload: KRErrorPayload{Message: message}})
}

func (h *KRHub) sendToClient(client *KRClient, message KRMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[KR] ")
}

func (h *KRHub) broadcastToRoom(room *krRoom, message KRMessage) {
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

func ServeKRWs(hub *KRHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[KR] Error upgrading connection:", err)
		return
	}

	client := &KRClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[KR] ",
		func(msg KRMessage) { hub.gameMessage <- KRGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
