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

// 보난자 단계별 마감 타이머 (테스트에서 짧게 낮춘다)
//   - 심기 30초 → 맨 앞 카드만 심기
//   - 거래 60초 → 마감 (남은 공개 카드는 차례인 사람이 심는다)
//   - 받은 카드 심기 20초 → 전원분 자동 배치
var (
	bzPlantTimeout   = 30 * time.Second
	bzTradeTimeout   = 60 * time.Second
	bzReceiveTimeout = 20 * time.Second
)

// bzRoom 게임(순수 상태)과 좌석별 연결의 매핑
type bzRoom struct {
	Game       *BZGame
	Clients    map[int]*BZClient // seat → client
	PhaseTimer *time.Timer       // 대기 상태 마감 타이머

	// DeadlineSeq 마지막으로 마감을 건 StateSeq — 같은 단계에 스냅샷이
	// 쌓일 때마다(관전 입장·거래 제안 등) 마감이 늘어나지 않게 하는 근거
	DeadlineSeq int

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string

	// Spectators 관전자 연결 (사설 방 코드로만 진입, 상한 maxSpectators).
	// 좌석·세션이 없어 재접속을 지원하지 않는다 — 끊기면 목록에서만 뗀다.
	Spectators map[*BZClient]bool

	// LastReact 좌석별 마지막 리액션 발신 시각 (레이트리밋 장부)
	LastReact map[int]time.Time
}

// bzPhaseSignal 마감 타이머의 발화 표식 — 대기 상태 일련번호(AfkSeq)로
// 지나간 발화를 구분한다
type bzPhaseSignal struct {
	GameID string
	Seq    int
}

type BZHub struct {
	clients map[*BZClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*bzRoom

	// 글로벌 로비. 시작 전 공용 방은 하나뿐이다.
	lobby *bzRoom

	// 사설 방 (초대 코드 → 시작 전 방). 허브 고루틴에서만 접근한다.
	privateLobbies map[string]*bzRoom

	// 진행 중 사설 방 (초대 코드 → gameID). 시작 후에도 코드로 방을 찾아
	// 관전 입장시키는 근거다. finishGame 에서 해제한다 (코드 재사용 복귀).
	activeCodes map[string]string

	register    chan *BZClient
	unregister  chan *BZClient
	gameMessage chan BZGameMessage
	phaseFired  chan bzPhaseSignal

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*BZClient]

	// 덱 셔플·자동 진행용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type BZGameMessage struct {
	Client  *BZClient
	Message BZMessage
}

func NewBZHub() *BZHub {
	return &BZHub{
		register:       make(chan *BZClient),
		unregister:     make(chan *BZClient),
		clients:        make(map[*BZClient]bool),
		rooms:          make(map[string]*bzRoom),
		privateLobbies: make(map[string]*bzRoom),
		activeCodes:    make(map[string]string),
		gameMessage:    make(chan BZGameMessage),
		phaseFired:     make(chan bzPhaseSignal, 8),
		sessionManager: newSessionManager[*BZClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *BZHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[BZ] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[BZ] Client unregistered: %s", client.ID)
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

func (h *BZHub) handleGameMessage(gm BZGameMessage) {
	// 관전자는 어떤 행동도 할 수 없다 (보기 전용 — 리액션도 좌석 보유자만)
	if h.isSpectator(gm.Client) {
		h.sendError(gm.Client, spectatorDeniedMsg)
		return
	}
	switch gm.Message.Type {
	case BZMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case BZMsgReact:
		h.handleReact(gm.Client, gm.Message)
	case BZMsgFillBots:
		h.handleFillBots(gm.Client)
	case BZMsgStart:
		h.handleStart(gm.Client)
	case BZMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case BZMsgPlant:
		h.handlePlant(gm.Client, gm.Message)
	case BZMsgHarvest:
		h.handleHarvest(gm.Client, gm.Message)
	case BZMsgBuyField:
		h.handleBuyField(gm.Client)
	case BZMsgOffer:
		h.handleOffer(gm.Client, gm.Message)
	case BZMsgRespond:
		h.handleRespond(gm.Client, gm.Message)
	case BZMsgPlantReceived:
		h.handlePlantReceived(gm.Client, gm.Message)
	case BZMsgEndPhase:
		h.handleEndPhase(gm.Client)
	}
}

// ==================== 대기실 ====================

func (h *BZHub) handleJoinGame(client *BZClient, msg BZMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload BZJoinGamePayload
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

	log.Printf("[보난자][입장] game=%s | seat%d=%s 대기 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), BZMaxPlayers)
	if room.Code == "" { // 사설 방 입장은 조용히 (공용 로비만 알림)
		notify("보난자 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), len(room.Game.Players), BZMaxPlayers))
	}

	h.sendToClient(client, BZMessage{
		Type: BZMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
			"roomCode":  room.Code,
		},
	})

	h.broadcastEvent(room, BZEventPayload{Kind: "joined", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 입장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	// 정원이 차도 자동 시작하지 않는다 — 호스트 명시 시작만
	h.broadcastState(room)
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다.
// 생략/빈 문자열은 공용 로비, "NEW"는 새 코드 발급, 그 외 코드는 해당 사설 방
// (없으면 그 코드로 관대하게 새로 생성).
func (h *BZHub) lobbyRoomFor(roomField string) *bzRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewBZGame(uuid.New().String())
			h.lobby = &bzRoom{Game: game, Clients: map[int]*BZClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[BZ] Created lobby game %s", game.ID)
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
		game := NewBZGame(uuid.New().String())
		room = &bzRoom{Game: game, Clients: map[int]*BZClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[BZ] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// ==================== 관전 / 리액션 ====================

// addSpectator 진행 중(또는 가득 찬) 사설 방에 관전자로 등록한다.
// 좌석·세션 없음 — 재접속 미지원, 끊기면 다시 코드로 들어온다.
func (h *BZHub) addSpectator(room *bzRoom, client *BZClient, name string) {
	if len(room.Spectators) >= maxSpectators {
		h.sendError(client, spectatorFullMsg)
		return
	}
	if room.Spectators == nil {
		room.Spectators = map[*BZClient]bool{}
	}
	client.Name = name
	client.GameID = room.Game.ID
	room.Spectators[client] = true

	log.Printf("[보난자][관전] game=%s | %s 관전 입장 (%d명)",
		room.Game.ID, displayName(name), len(room.Spectators))

	h.sendToClient(client, BZMessage{
		Type:    BZMsgSpectateJoined,
		Payload: map[string]interface{}{"gameId": room.Game.ID, "roomCode": room.Code},
	})
	h.broadcastState(room)
}

// isSpectator 관전자 연결인지 (좌석 보유자·미입장 연결은 false)
func (h *BZHub) isSpectator(client *BZClient) bool {
	if client.GameID == "" {
		return false
	}
	room := h.rooms[client.GameID]
	return room != nil && room.Spectators[client]
}

// handleReact 리액션 이모지 — 좌석 보유자만, waiting 중에도 허용.
// 화이트리스트 외·레이트리밋 초과는 조용히 무시한다 (상태 저장 없음).
func (h *BZHub) handleReact(client *BZClient, msg BZMessage) {
	room := h.rooms[client.GameID]
	if room == nil || client.Seat < 0 {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload BZReactPayload
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
	h.broadcastEvent(room, BZEventPayload{Kind: "react", Seat: &seat, Name: client.Name,
		Message: payload.Emoji})
}

// waitingRoomOf 클라이언트가 속한 시작 전 방 (공용 로비 또는 사설 방)
func (h *BZHub) waitingRoomOf(client *BZClient) *bzRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Ready {
		return nil
	}
	return room
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *BZHub) hostSeat(room *bzRoom) int {
	return hostSeatOf(room.Clients)
}

// bzHumanCount 방의 사람 수
func bzHumanCount(room *bzRoom) int {
	return humanCountOf(room.Clients)
}

// updateLobbyWaiting 로비 현황판 갱신 — 사람 1명 이상 대기 && 미시작.
// 사설 방은 현황판에 노출하지 않는다 (초대 링크로만 접근).
func (h *BZHub) updateLobbyWaiting(room *bzRoom) {
	if room.Code != "" {
		return
	}
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && bzHumanCount(room) >= 1
	lobbySetWaiting("bohnanza", waiting)
}

// handleFillBots host 가 빈 좌석을 연습봇으로 3인까지 채운 뒤 즉시 시작한다
// (스폰은 join 을 거치지 않으므로 명시 시작 호출).
func (h *BZHub) handleFillBots(client *BZClient) {
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
	for len(room.Game.Players) < BZFillBotTarget {
		botNo++
		if !h.spawnBZBot(room, fmt.Sprintf("%s%d", botName, botNo)) {
			break
		}
	}

	if room.Game.CanStart() {
		h.startGame(room)
		return
	}
	h.broadcastState(room)
}

func (h *BZHub) handleStart(client *BZClient) {
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
		h.sendError(client, fmt.Sprintf("%d명 이상 모여야 시작할 수 있습니다", BZMinPlayers))
		return
	}
	h.startGame(room)
}

func (h *BZHub) startGame(room *bzRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	if room.Code != "" {
		// 시작한 사설 방의 코드는 진행 중 장부로 옮긴다 (관전 입장 근거)
		delete(h.privateLobbies, room.Code)
		h.activeCodes[room.Code] = room.Game.ID
	} else {
		h.lobby = nil
		lobbySetWaiting("bohnanza", false)
	}

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	n := len(room.Game.Players)
	log.Printf("[보난자][경기시작] game=%s | 인원=%d | 덱=%d장 | 손패 %d장 · 콩밭 %d개 | 종료=%d번째 소진 | 선=seat%d | %v",
		room.Game.ID, n, len(room.Game.Deck), BZStartHand, BZStartFields,
		room.Game.EndCycle, room.Game.CurrentSeat, names)
	if !bzRoomHasBot(room) { // 연습봇전은 운영자 알림을 억제한다
		notify("보난자 시작", fmt.Sprintf("%d인전 시작", n))
	}

	h.afterProgress(room)
}

// removeFromLobby 대기실에서 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *BZHub) removeFromLobby(room *bzRoom, client *BZClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	rebuilt := map[int]*BZClient{}
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

	log.Printf("[보난자][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), BZMaxPlayers)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if bzHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, BZMessage{Type: BZMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			lobbySetWaiting("bohnanza", false)
		}
		return
	}

	h.broadcastEvent(room, BZEventPayload{Kind: "left", Name: client.Name,
		Message: fmt.Sprintf("%s님이 퇴장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *BZHub) roomOf(client *BZClient) *bzRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

// handlePlant ① 손패 맨 앞(선택적으로 두 번째까지) 심기
func (h *BZHub) handlePlant(client *BZClient, msg BZMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload BZPlantPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.Plant(client.Seat, payload.Second); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[보난자][심기] game=%s | seat%d=%s second=%v (손패 %d장 · 덱 %d장)",
		room.Game.ID, client.Seat, displayName(client.Name), payload.Second,
		len(room.Game.Players[client.Seat].Hand), len(room.Game.Deck))
	h.afterProgress(room)
}

// handleHarvest 수확 — 자기 차례가 아니어도 언제든
func (h *BZHub) handleHarvest(client *BZClient, msg BZMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload BZHarvestPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.Harvest(client.Seat, payload.Field); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[보난자][수확] game=%s | seat%d=%s field=%d (금화 %d개)",
		room.Game.ID, client.Seat, displayName(client.Name), payload.Field,
		room.Game.Players[client.Seat].Coins)
	h.afterProgress(room)
}

// handleBuyField 세 번째 콩밭 구매 — 금화 3개, 게임 중 1회
func (h *BZHub) handleBuyField(client *BZClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	if err := room.Game.BuyField(client.Seat); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[보난자][밭구매] game=%s | seat%d=%s 세 번째 콩밭 구매 (남은 금화 %d개)",
		room.Game.ID, client.Seat, displayName(client.Name),
		room.Game.Players[client.Seat].Coins)
	h.afterProgress(room)
}

// handleOffer 거래·기부 제안
func (h *BZHub) handleOffer(client *BZClient, msg BZMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload BZOfferPayload
	json.Unmarshal(payloadBytes, &payload)

	id, err := room.Game.Offer(client.Seat, payload)
	if err != nil {
		h.sendError(client, err.Error())
		return
	}
	// 어떤 콩을 주고받는지는 스냅샷에서만 드러난다 (로그에는 남기지 않는다)
	log.Printf("[보난자][거래제안] game=%s | seat%d=%s → seat%d | offer=%s (주 %d장 · 요구 %d장)",
		room.Game.ID, client.Seat, displayName(client.Name), payload.ToSeat, id,
		len(payload.GiveHand)+len(payload.GiveFlipped), len(payload.WantHand))
	h.afterProgress(room)
}

// handleRespond 제안 수락·거절
func (h *BZHub) handleRespond(client *BZClient, msg BZMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload BZRespondPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.Respond(client.Seat, payload.OfferID, payload.Accept); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[보난자][거래응답] game=%s | seat%d=%s offer=%s accept=%v",
		room.Game.ID, client.Seat, displayName(client.Name), payload.OfferID, payload.Accept)
	h.afterProgress(room)
}

// handlePlantReceived ③ 받은 카드 심기
func (h *BZHub) handlePlantReceived(client *BZClient, msg BZMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload BZPlantReceivedPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.PlantReceived(client.Seat, payload.CardIndex, payload.Field); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[보난자][받은카드심기] game=%s | seat%d=%s card=%d field=%d",
		room.Game.ID, client.Seat, displayName(client.Name), payload.CardIndex, payload.Field)
	h.afterProgress(room)
}

// handleEndPhase 현재 단계 종료 (2단계 거래 마감)
func (h *BZHub) handleEndPhase(client *BZClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	if err := room.Game.EndPhase(client.Seat); err != nil {
		h.sendError(client, err.Error())
		return
	}
	log.Printf("[보난자][거래마감] game=%s | seat%d=%s 거래를 마감했습니다",
		room.Game.ID, client.Seat, displayName(client.Name))
	h.afterProgress(room)
}

// afterProgress 모든 게임 변이 뒤의 공통 마무리 — 이벤트 방송·종료 판정·
// 새 대기 상태의 마감 예약·스냅샷 방송을 한 번에 처리한다.
func (h *BZHub) afterProgress(room *bzRoom) {
	h.drainEvents(room)
	if room.Game.Phase == BZPhaseGameOver {
		h.finishGame(room)
		return
	}
	h.syncDeadline(room)
	h.broadcastState(room)
}

// drainEvents 순수 규칙이 쌓은 이벤트를 bz_event 로 방송한다
func (h *BZHub) drainEvents(room *bzRoom) {
	for _, ev := range room.Game.DrainEvents() {
		payload := BZEventPayload{Kind: ev.Kind, Message: ev.Message}
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
// 같은 단계에 스냅샷이 쌓여도(거래 제안·관전 입장 등) 마감은 늘어나지 않는다.
// draw 는 서버가 즉시 해소하는 전이 단계라 마감이 없다.
func (h *BZHub) syncDeadline(room *bzRoom) {
	game := room.Game
	if room.DeadlineSeq == game.StateSeq {
		return
	}
	room.DeadlineSeq = game.StateSeq
	var dur time.Duration
	switch game.Phase {
	case BZPhasePlant:
		dur = bzPlantTimeout
	case BZPhaseTrade:
		dur = bzTradeTimeout
	case BZPhasePlantReceived:
		dur = bzReceiveTimeout
	default:
		h.stopPhaseTimer(room)
		return
	}
	h.scheduleDeadline(room, dur)
}

// scheduleDeadline 마감 타이머 예약. AfkSeq 를 올려 지나간 발화를 구분하고,
// 발화는 허브 채널을 경유하므로 허브 밖에서 상태를 만지지 않는다.
func (h *BZHub) scheduleDeadline(room *bzRoom, dur time.Duration) {
	h.stopPhaseTimer(room)
	room.Game.AfkSeq++
	room.Game.Deadline = time.Now().Add(dur).UnixMilli()
	sig := bzPhaseSignal{GameID: room.Game.ID, Seq: room.Game.AfkSeq}
	room.PhaseTimer = time.AfterFunc(dur, func() {
		h.phaseFired <- sig
	})
}

func (h *BZHub) stopPhaseTimer(room *bzRoom) {
	stopTimer(&room.PhaseTimer)
}

// handlePhaseFired 마감 발화 — 단계별 자동 행동으로 해소한다.
//   - plant: 맨 앞 카드만 심는다 (자리가 없으면 자동으로 수확해 만든다)
//   - trade: 거래를 마감한다 (남은 공개 카드는 차례인 사람이 심는다)
//   - plant_received: 남은 받은 카드를 전원분 자동 배치한다
func (h *BZHub) handlePhaseFired(sig bzPhaseSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.Game.AfkSeq != sig.Seq {
		return
	}
	game := room.Game
	seat := game.CurrentSeat
	if seat < 0 || seat >= len(game.Players) {
		return
	}
	actor := game.Players[seat]

	switch game.Phase {
	case BZPhasePlant:
		h.broadcastEvent(room, BZEventPayload{Kind: "afk", Seat: &seat, Name: actor.Name,
			Message: fmt.Sprintf("%s님이 오래 응답하지 않아 자동으로 맨 앞 카드를 심습니다", actor.Name)})
		game.ForcePlant()
		log.Printf("[보난자][자동진행] game=%s | seat%d 무응답 — 자동 심기", game.ID, seat)

	case BZPhaseTrade:
		h.broadcastEvent(room, BZEventPayload{Kind: "afk", Seat: &seat, Name: actor.Name,
			Message: fmt.Sprintf("%s님의 거래 시간이 끝나 자동으로 마감합니다", actor.Name)})
		game.ForceTradeEnd()
		log.Printf("[보난자][자동진행] game=%s | seat%d 거래 마감", game.ID, seat)

	case BZPhasePlantReceived:
		h.broadcastEvent(room, BZEventPayload{Kind: "afk", Seat: &seat, Name: actor.Name,
			Message: "받은 카드 심기 시간이 끝나 자동으로 배치합니다"})
		game.ForcePlantReceived()
		log.Printf("[보난자][자동진행] game=%s | 받은 카드 자동 배치", game.ID)

	default:
		return
	}
	h.afterProgress(room)
}

// ==================== 상태 뷰 (은닉의 핵심) ====================

// buildBZState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
//
// 은닉:
//   - yourHand·yourPending 은 본인에게만 실린다 — 타인·관전자
//     (viewerSeat -1)의 raw JSON 에는 키 자체가 없다 (nil 포인터 + omitempty).
//     빈 손패도 [] 로 보내야 하므로 슬라이스 포인터를 쓴다.
//   - 제안의 요구 카드(wantHand)는 **당사자 둘에게만** 실린다. 주는 카드는
//     협상 재료라 전원 공개다.
//   - fields·coins·flipped·deckLeft·deckCycle 은 전원 공개다. 덱 내용은
//     남은 장수만 나간다.
//
// viewerSeat -1(관전자)·좌석 없는 방에서도 패닉 없이 만들어져야 한다.
func (h *BZHub) buildBZState(room *bzRoom, viewerSeat int) BZGameStatePayload {
	game := room.Game
	seated := viewerSeat >= 0 && viewerSeat < len(game.Players)

	var yourHand *[]BZBean
	var yourPending *[]BZBean
	if seated && game.Ready {
		hand := append([]BZBean{}, game.Players[viewerSeat].Hand...)
		yourHand = &hand
		pending := append([]BZBean{}, game.Players[viewerSeat].Pending...)
		yourPending = &pending
	}

	players := []BZPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		players = append(players, BZPlayerView{
			Seat:       p.Seat,
			Name:       p.Name,
			Connected:  c != nil && c.Connected,
			Bot:        c != nil && c.Bot,
			Coins:      p.Coins,
			HandCount:  len(p.Hand),
			FieldCount: len(p.Fields),
			Fields:     append([]BZField{}, p.Fields...),
		})
	}

	offers := []BZOfferView{}
	for _, o := range game.Offers {
		view := BZOfferView{ID: o.ID, FromSeat: o.FromSeat, ToSeat: o.ToSeat}
		// 상세는 당사자 둘만 본다 (제3자·관전자 raw JSON 에 키 자체가 없다)
		if seated && (viewerSeat == o.FromSeat || viewerSeat == o.ToSeat) {
			giveHand := bzBeansAt(game.Players[o.FromSeat].Hand, o.GiveHand)
			giveFlipped := bzBeansAt(game.Flipped, o.GiveFlipped)
			wantHand := append([]int{}, o.WantHand...)
			view.GiveHand = &giveHand
			view.GiveFlipped = &giveFlipped
			view.WantHand = &wantHand
		}
		// 요구한 자리에 실제로 무엇이 있는지는 **그 카드의 주인에게만** 실린다.
		// 제안자에게 펼쳐 주면 제안을 반복해 상대 손패를 수락 없이 읽어낼 수 있다.
		if seated && viewerSeat == o.ToSeat {
			wantBeans := bzBeansAt(game.Players[o.ToSeat].Hand, o.WantHand)
			view.WantBeans = &wantBeans
		}
		offers = append(offers, view)
	}

	endsAt := int64(0)
	switch game.Phase {
	case BZPhasePlant, BZPhaseTrade, BZPhasePlantReceived:
		endsAt = game.Deadline
	}

	return BZGameStatePayload{
		GameID:      game.ID,
		RoomCode:    room.Code,
		Phase:       game.Phase,
		HostSeat:    h.hostSeat(room),
		YourSeat:    viewerSeat,
		Spectators:  len(room.Spectators),
		EndsAt:      endsAt,
		CurrentSeat: game.CurrentSeat,
		DeckLeft:    len(game.Deck),
		DeckCycle:   game.DeckCycle,
		Flipped:     append([]BZBean{}, game.Flipped...),
		Offers:      offers,
		YourHand:    yourHand,
		YourPending: yourPending,
		Players:     players,
		LastAction:  game.LastAction,
		Result:      game.Result,
	}
}

// bzBeansAt 인덱스 목록이 가리키는 콩들 (범위 밖은 건너뛴다 — 빈 목록도 [])
func bzBeansAt(cards []BZBean, idx []int) []BZBean {
	out := []BZBean{}
	for _, i := range idx {
		if i >= 0 && i < len(cards) {
			out = append(out, cards[i])
		}
	}
	return out
}

// broadcastState 좌석마다 개인화 스냅샷을 만들어 보낸다.
// 관전자에게는 공개 정보만 담긴 스냅샷(viewerSeat -1)이 간다.
func (h *BZHub) broadcastState(room *bzRoom) {
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, BZMessage{
			Type:    BZMsgGameState,
			Payload: h.buildBZState(room, seat),
		})
	}
	if len(room.Spectators) > 0 {
		msg := BZMessage{Type: BZMsgGameState, Payload: h.buildBZState(room, -1)}
		for c := range room.Spectators {
			h.sendToClient(c, msg)
		}
	}
}

func (h *BZHub) broadcastEvent(room *bzRoom, event BZEventPayload) {
	h.broadcastToRoom(room, BZMessage{Type: BZMsgEvent, Payload: event})
}

// finishGame 게임 종료 발표·전적 기록·방 정리 (재대결은 같은 방 코드
// 재입장으로 — finishGame 이 코드를 반납해 같은 코드로 새 방을 만들 수 있다)
func (h *BZHub) finishGame(room *bzRoom) {
	game := room.Game
	h.stopPhaseTimer(room)
	game.Deadline = 0

	result := game.Result
	if result == nil { // 방어선 — 종료는 항상 결과와 함께 온다
		result = &BZResult{Rows: []BZResultRow{}, WinnerSeats: []int{},
			WinnerNames: []string{}, Message: "게임이 종료됐습니다"}
	}

	winnerSeats := map[int]bool{}
	for _, s := range result.WinnerSeats {
		winnerSeats[s] = true
	}
	winners, losers := []string{}, []string{}
	for _, p := range game.Players {
		if winnerSeats[p.Seat] {
			winners = append(winners, displayName(p.Name))
		} else {
			losers = append(losers, displayName(p.Name))
		}
	}

	h.broadcastEvent(room, BZEventPayload{Kind: "game_over", Message: result.Message})
	// 최종 스냅샷을 먼저 보낸 뒤 종료를 알린다
	// (봇 러너는 bz_game_over 를 보고 스스로 끝난다)
	h.broadcastState(room)
	h.broadcastToRoom(room, BZMessage{
		Type: BZMsgGameOver,
		Payload: BZGameOverPayload{
			Rows:        append([]BZResultRow{}, result.Rows...),
			WinnerSeats: append([]int{}, result.WinnerSeats...),
			WinnerNames: append([]string{}, result.WinnerNames...),
			Message:     result.Message,
			Turns:       game.Turns,
			Players:     h.buildBZState(room, -1).Players,
		},
	})

	coins := []string{}
	for _, p := range game.Players {
		coins = append(coins, fmt.Sprintf("%s 금화 %d개", displayName(p.Name), p.Coins))
	}
	log.Printf("[보난자][경기결과] game=%s | 승자=%s | 차례=%d | 덱소진=%d회 | 소요=%s | %s",
		game.ID, strings.Join(winners, "·"), game.Turns, game.DeckCycle,
		matchDuration(game.StartedAt), strings.Join(coins, " / "))

	RecordMatch(MatchRecord{
		Game:     "bohnanza",
		Players:  strings.Join(winners, "·") + " vs " + strings.Join(losers, "·"),
		Winner:   strings.Join(winners, "·"),
		Reason:   "coins",
		Duration: matchSeconds(game.StartedAt),
		Bot:      bzRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
	if room.Code != "" {
		delete(h.activeCodes, room.Code) // 코드 재사용 복귀
	}
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *BZHub) handleDisconnect(client *BZClient) {
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
	log.Printf("[보난자][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, BZMessage{
		Type: BZMsgPlayerDisconnected,
		Payload: BZPlayerDisconnectedPayload{
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
func (h *BZHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == BZPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[보난자][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := h.takeoverBZBot(room, seat, client.Name)
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.broadcastEvent(room, BZEventPayload{Kind: "bot_takeover", Seat: &seat,
		Name:    client.Name,
		Message: fmt.Sprintf("%s님 자리를 봇이 이어받았습니다", client.Name)})
	// 새 봇이 이 스냅샷을 받아 자기 차례면 즉시 이어간다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *BZHub) handleRejoin(client *BZClient, msg BZMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload BZRejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, BZMessage{Type: BZMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, BZMessage{Type: BZMsgSessionExpired})
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

	log.Printf("[보난자][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, BZMessage{
		Type:    BZMsgPlayerReconnected,
		Payload: BZPlayerReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *BZHub) clearGameSessions(room *bzRoom) {
	clearRoomSessions(&h.sessionManager, room.Clients)
}

// ==================== 전송 ====================

func (h *BZHub) sendError(client *BZClient, message string) {
	h.sendToClient(client, BZMessage{Type: BZMsgError, Payload: BZErrorPayload{Message: message}})
}

func (h *BZHub) sendToClient(client *BZClient, message BZMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[BZ] ")
}

func (h *BZHub) broadcastToRoom(room *bzRoom, message BZMessage) {
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

func ServeBZWs(hub *BZHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[BZ] Error upgrading connection:", err)
		return
	}

	client := &BZClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[BZ] ",
		func(msg BZMessage) { hub.gameMessage <- BZGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
