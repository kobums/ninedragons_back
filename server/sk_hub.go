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

// 스컬 단계 타이머 — 접속만 유지한 채 행동하지 않는 좌석이 게임을 영구
// 정지시키지 않게, 만료 시 자동 행동으로 해소한다 (테스트에서 짧게 낮춘다).
//   - placing(동시): 미배치 전원 무작위 카드 1장
//   - placing(턴): 무작위 카드 1장 (손패가 없으면 최소 배팅 1)
//   - bidding: 패스
//   - flipping: 무작위 상대 더미 1장
var (
	skAfkTimeout    = 45 * time.Second
	skRoundEndDelay = 5 * time.Second // round_end 발표 → 다음 라운드 자동 진행
)

// skRoom 게임(순수 상태)과 좌석별 연결의 매핑
type skRoom struct {
	Game       *SKGame
	Clients    map[int]*SKClient // seat → client
	PhaseTimer *time.Timer       // 단계 타임아웃 타이머

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string

	// Spectators 관전자 연결 (사설 방 코드로만 진입, 상한 maxSpectators).
	// 좌석·세션이 없어 재접속을 지원하지 않는다 — 끊기면 목록에서만 뗀다.
	Spectators map[*SKClient]bool

	// LastReact 좌석별 마지막 리액션 발신 시각 (레이트리밋 장부)
	LastReact map[int]time.Time
}

// skPhaseSignal 타임아웃 타이머의 발화 표식. 같은 단계가 반복될 수 있어
// (턴마다 마감 갱신) 단계 전환 일련번호(AfkSeq)로 지나간 발화를 구분한다.
type skPhaseSignal struct {
	GameID string
	Seq    int
}

type SKHub struct {
	// 등록된 클라이언트
	clients map[*SKClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*skRoom

	// 글로벌 로비. 시작 전 공용 방은 하나뿐이다.
	lobby *skRoom

	// 사설 방 (초대 코드 → 시작 전 방). 허브 고루틴에서만 접근한다.
	// 시작하면 privateLobbies 에서 activeCodes 로 옮긴다.
	privateLobbies map[string]*skRoom

	// 진행 중 사설 방 (초대 코드 → gameID). 시작 후에도 코드로 방을 찾아
	// 관전 입장시키는 근거다. finishGame 에서 해제한다 (코드 재사용 복귀).
	activeCodes map[string]string

	// 클라이언트 등록
	register chan *SKClient

	// 클라이언트 등록 해제
	unregister chan *SKClient

	// 게임 메시지
	gameMessage chan SKGameMessage

	// 단계 타임아웃 발화 (time.AfterFunc → 허브 채널 경유)
	phaseFired chan skPhaseSignal

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*SKClient]

	// 셔플·무작위 제거·자동 행동용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type SKGameMessage struct {
	Client  *SKClient
	Message SKMessage
}

func NewSKHub() *SKHub {
	return &SKHub{
		register:       make(chan *SKClient),
		unregister:     make(chan *SKClient),
		clients:        make(map[*SKClient]bool),
		rooms:          make(map[string]*skRoom),
		privateLobbies: make(map[string]*skRoom),
		activeCodes:    make(map[string]string),
		gameMessage:    make(chan SKGameMessage),
		phaseFired:     make(chan skPhaseSignal, 8),
		sessionManager: newSessionManager[*SKClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *SKHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[SK] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[SK] Client unregistered: %s", client.ID)
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

func (h *SKHub) handleGameMessage(gm SKGameMessage) {
	// 관전자는 어떤 행동도 할 수 없다 (보기 전용 — 리액션도 좌석 보유자만)
	if h.isSpectator(gm.Client) {
		h.sendError(gm.Client, spectatorDeniedMsg)
		return
	}
	switch gm.Message.Type {
	case SKMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case SKMsgReact:
		h.handleReact(gm.Client, gm.Message)
	case SKMsgFillBots:
		h.handleFillBots(gm.Client)
	case SKMsgStart:
		h.handleStart(gm.Client)
	case SKMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case SKMsgPlace:
		h.handlePlace(gm.Client, gm.Message)
	case SKMsgBid:
		h.handleBid(gm.Client, gm.Message)
	case SKMsgPass:
		h.handlePass(gm.Client)
	case SKMsgFlip:
		h.handleFlip(gm.Client, gm.Message)
	}
}

// ==================== 로비 ====================

func (h *SKHub) handleJoinGame(client *SKClient, msg SKMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SKJoinGamePayload
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

	log.Printf("[스컬][입장] game=%s | seat%d=%s 게임 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), SKMaxPlayers)
	if room.Code == "" { // 사설 방 입장은 조용히 (공용 로비만 알림)
		notify("스컬 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), len(room.Game.Players), SKMaxPlayers))
	}

	h.sendToClient(client, SKMessage{
		Type: SKMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
			"roomCode":  room.Code,
		},
	})

	h.broadcastEvent(room, SKEventPayload{Kind: "joined", Seat: &seat, Name: client.Name})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다.
// 생략/빈 문자열은 기존 공용 로비 경로 그대로, "NEW"는 새 코드 발급,
// 그 외 코드는 해당 사설 방 (없으면 그 코드로 관대하게 새로 생성).
func (h *SKHub) lobbyRoomFor(roomField string) *skRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewSKGame(uuid.New().String())
			h.lobby = &skRoom{Game: game, Clients: map[int]*SKClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[SK] Created lobby game %s", game.ID)
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
		game := NewSKGame(uuid.New().String())
		room = &skRoom{Game: game, Clients: map[int]*SKClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[SK] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// ==================== 관전 / 리액션 ====================

// addSpectator 진행 중(또는 가득 찬) 사설 방에 관전자로 등록한다.
// 좌석·세션 없음 — 재접속 미지원, 끊기면 다시 코드로 들어온다.
func (h *SKHub) addSpectator(room *skRoom, client *SKClient, name string) {
	if len(room.Spectators) >= maxSpectators {
		h.sendError(client, spectatorFullMsg)
		return
	}
	if room.Spectators == nil {
		room.Spectators = map[*SKClient]bool{}
	}
	client.Name = name
	client.GameID = room.Game.ID
	room.Spectators[client] = true

	log.Printf("[스컬][관전] game=%s | %s 관전 입장 (%d명)",
		room.Game.ID, displayName(name), len(room.Spectators))

	h.sendToClient(client, SKMessage{
		Type:    SKMsgSpectateJoined,
		Payload: map[string]interface{}{"gameId": room.Game.ID, "roomCode": room.Code},
	})
	// 전원(관전자 포함)에게 관전자 수가 반영된 스냅샷 — 신규 관전자의 첫 스냅샷 겸용
	h.broadcastState(room)
}

// isSpectator 관전자 연결인지 (좌석 보유자·미입장 연결은 false)
func (h *SKHub) isSpectator(client *SKClient) bool {
	if client.GameID == "" {
		return false
	}
	room := h.rooms[client.GameID]
	return room != nil && room.Spectators[client]
}

// handleReact 리액션 이모지 — 좌석 보유자만, waiting 중에도 허용.
// 화이트리스트 외·레이트리밋 초과는 조용히 무시한다 (상태 저장 없음).
func (h *SKHub) handleReact(client *SKClient, msg SKMessage) {
	room := h.rooms[client.GameID]
	if room == nil || client.Seat < 0 {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SKReactPayload
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
	h.broadcastEvent(room, SKEventPayload{Kind: "react", Seat: &seat, Name: client.Name, Message: payload.Emoji})
}

// waitingRoomOf 클라이언트가 속한 시작 전 방 (공용 로비 또는 사설 방)
func (h *SKHub) waitingRoomOf(client *SKClient) *skRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Ready {
		return nil
	}
	return room
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *SKHub) hostSeat(room *skRoom) int {
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

// handleFillBots host 가 정원(6)까지 연습봇으로 채운다. 시작은 별도의 sk_start.
func (h *SKHub) handleFillBots(client *SKClient) {
	room := h.waitingRoomOf(client)
	if room == nil {
		h.sendError(client, "로비를 찾을 수 없습니다")
		return
	}
	if client.Seat != h.hostSeat(room) {
		h.sendError(client, "호스트만 봇을 채울 수 있습니다")
		return
	}
	if len(room.Game.Players) >= SKBotFillTarget {
		h.sendError(client, fmt.Sprintf("%d명 미만일 때만 봇을 채울 수 있습니다", SKBotFillTarget))
		return
	}

	botNo := 0
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			botNo++
		}
	}
	for len(room.Game.Players) < SKBotFillTarget {
		botNo++
		if h.spawnBot(room, fmt.Sprintf("%s%d", botName, botNo)) == nil {
			break
		}
	}
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

func (h *SKHub) handleStart(client *SKClient) {
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
		h.sendError(client, fmt.Sprintf("%d명 이상 모여야 시작할 수 있습니다", SKMinPlayers))
		return
	}
	h.startGame(room)
}

// skHumanCount 방의 사람 수
func skHumanCount(room *skRoom) int {
	n := 0
	for _, c := range room.Clients {
		if c != nil && !c.Bot {
			n++
		}
	}
	return n
}

// skRoomHasBot 방에 연습봇이 있는지 (전적 기록용)
func skRoomHasBot(room *skRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}

// updateLobbyWaiting 로비 현황판 갱신 — 사람 1명 이상 대기 && 미시작.
// 사설 방은 현황판에 노출하지 않는다 (초대 링크로만 접근).
func (h *SKHub) updateLobbyWaiting(room *skRoom) {
	if room.Code != "" {
		return
	}
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && skHumanCount(room) >= 1
	lobbySetWaiting("skull", waiting)
}

func (h *SKHub) startGame(room *skRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	if room.Code != "" {
		// 시작한 사설 방의 코드는 진행 중 장부로 옮긴다 (관전 입장 근거)
		delete(h.privateLobbies, room.Code)
		h.activeCodes[room.Code] = room.Game.ID
	} else {
		h.lobby = nil // 시작한 방은 로비에서 떼어낸다
		lobbySetWaiting("skull", false)
	}

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	log.Printf("[스컬][경기시작] game=%s | 인원=%d | %v",
		room.Game.ID, len(room.Game.Players), names)
	// 봇 채우기로 시작한 판도 포함해 시작 시점에 1회만 알린다
	notify("스컬 게임 시작", fmt.Sprintf("%d인전 시작", len(room.Game.Players)))

	h.broadcastEvent(room, SKEventPayload{Kind: "started"})
	h.announceRoundBegin(room)
	// 마감 시각을 먼저 세팅해야 첫 스냅샷에 endsAt 이 실린다
	h.scheduleDeadline(room, skAfkTimeout)
	h.broadcastState(room)
}

// announceRoundBegin 라운드 시작 발표 (동시 배치 안내)
func (h *SKHub) announceRoundBegin(room *skRoom) {
	h.broadcastEvent(room, SKEventPayload{Kind: "round_begin",
		Message: fmt.Sprintf("%d라운드 — 전원 카드 1장을 내려놓으세요", room.Game.RoundNo)})
}

// removeFromLobby 대기 중 이탈 — 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *SKHub) removeFromLobby(room *skRoom, client *SKClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	rebuilt := map[int]*SKClient{}
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

	log.Printf("[스컬][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), SKMaxPlayers)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if skHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, SKMessage{Type: SKMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			lobbySetWaiting("skull", false)
		}
		return
	}

	h.broadcastEvent(room, SKEventPayload{Kind: "left", Name: client.Name})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *SKHub) roomOf(client *SKClient) *skRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

func (h *SKHub) handlePlace(client *SKClient, msg SKMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SKPlacePayload
	json.Unmarshal(payloadBytes, &payload)

	game := room.Game
	if err := game.SubmitPlace(client.Seat, payload.Index); err != nil {
		h.sendError(client, err.Error())
		return
	}
	seat := client.Seat
	// 카드 내용은 비공개 — 배치 사실만 알린다
	h.broadcastEvent(room, SKEventPayload{Kind: "placed", Seat: &seat, Name: client.Name})

	// 턴제 파트(진입 포함)에서는 차례마다 마감을 새로 건다.
	// 동시 배치 중에는 처음 건 마감을 유지한다 (전원 공용 마감).
	if game.PlacingTurns {
		h.scheduleDeadline(room, skAfkTimeout)
	}
	h.broadcastState(room)
}

func (h *SKHub) handleBid(client *SKClient, msg SKMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SKBidPayload
	json.Unmarshal(payloadBytes, &payload)

	game := room.Game
	prevChallenger := game.ChallengerSeat
	if err := game.SubmitBid(client.Seat, payload.Count, h.rng); err != nil {
		h.sendError(client, err.Error())
		return
	}
	seat := client.Seat
	log.Printf("[스컬][배팅] game=%s | %d라운드 | seat%d=%s 장미 %d장 선언",
		game.ID, game.RoundNo, seat, displayName(client.Name), payload.Count)
	h.broadcastEvent(room, SKEventPayload{Kind: "bid", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 장미 %d장 뒤집기를 선언했습니다", client.Name, payload.Count)})
	h.afterProgress(room, prevChallenger)
}

func (h *SKHub) handlePass(client *SKClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	game := room.Game
	prevChallenger := game.ChallengerSeat
	if err := game.SubmitPass(client.Seat, h.rng); err != nil {
		h.sendError(client, err.Error())
		return
	}
	seat := client.Seat
	h.broadcastEvent(room, SKEventPayload{Kind: "pass", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 패스했습니다", client.Name)})
	h.afterProgress(room, prevChallenger)
}

func (h *SKHub) handleFlip(client *SKClient, msg SKMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SKFlipPayload
	json.Unmarshal(payloadBytes, &payload)

	game := room.Game
	prevChallenger := game.ChallengerSeat
	if err := game.SubmitFlip(client.Seat, payload.Seat, h.rng); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.emitFlipEvent(room)
	h.afterProgress(room, prevChallenger)
}

// emitFlipEvent 방금 공개된 카드(Flipped 마지막)를 이벤트로 알린다 —
// 뒤집힌 카드는 공개 정보다
func (h *SKHub) emitFlipEvent(room *skRoom) {
	game := room.Game
	if len(game.Flipped) == 0 {
		return
	}
	last := game.Flipped[len(game.Flipped)-1]
	owner := last.Seat
	label := "장미"
	if last.Card == string(SKCardSkull) {
		label = "해골"
	}
	h.broadcastEvent(room, SKEventPayload{Kind: "flip", Seat: &owner, Name: game.Players[owner].Name,
		Message: fmt.Sprintf("%s님 더미에서 %s가 나왔습니다", game.Players[owner].Name, label)})
}

// afterProgress 배팅·패스·뒤집기 뒤의 공통 마무리 — 도전 확정·라운드 결과
// 이벤트를 쏘고, 다음 단계의 마감을 걸고, 스냅샷을 뿌린다.
func (h *SKHub) afterProgress(room *skRoom, prevChallenger int) {
	game := room.Game
	if prevChallenger < 0 && game.ChallengerSeat >= 0 {
		seat := game.ChallengerSeat
		log.Printf("[스컬][도전] game=%s | %d라운드 | seat%d=%s 장미 %d장 도전",
			game.ID, game.RoundNo, seat, displayName(game.Players[seat].Name), game.HighBid)
		h.broadcastEvent(room, SKEventPayload{Kind: "challenge", Seat: &seat, Name: game.Players[seat].Name,
			Message: fmt.Sprintf("%s님이 도전자 — 장미 %d장 뒤집기에 도전합니다",
				game.Players[seat].Name, game.HighBid)})
	}

	switch game.Phase {
	case SKPhaseBidding, SKPhaseFlipping:
		h.scheduleDeadline(room, skAfkTimeout)
		h.broadcastState(room)
	case SKPhaseRoundEnd:
		h.announceRoundResult(room)
		h.scheduleDeadline(room, skRoundEndDelay)
		h.broadcastState(room)
	case SKPhaseGameOver:
		h.announceRoundResult(room)
		h.finishGame(room)
	default:
		h.scheduleDeadline(room, skAfkTimeout)
		h.broadcastState(room)
	}
}

// announceRoundResult 라운드 결과·탈락 발표
func (h *SKHub) announceRoundResult(room *skRoom) {
	game := room.Game
	rr := game.RoundResult
	if rr == nil {
		return
	}
	seat := rr.Seat
	h.broadcastEvent(room, SKEventPayload{Kind: "round_result", Seat: &seat,
		Name: game.Players[seat].Name, Message: rr.Message})
	if !game.Players[seat].Alive {
		h.broadcastEvent(room, SKEventPayload{Kind: "eliminated", Seat: &seat,
			Name:    game.Players[seat].Name,
			Message: fmt.Sprintf("%s님이 탈락했습니다", game.Players[seat].Name)})
	}
	log.Printf("[스컬][라운드결과] game=%s | %d라운드 | %s",
		game.ID, game.RoundNo, rr.Message)
}

// ==================== 단계 타임아웃 (AFK 진행 보장) ====================

// scheduleDeadline 현재 단계의 타임아웃 타이머를 건다. AfkSeq 를 올려
// 지나간 발화를 구분하고, 발화는 허브 채널을 경유하므로 허브 밖에서
// 상태를 만지지 않는다.
func (h *SKHub) scheduleDeadline(room *skRoom, dur time.Duration) {
	h.stopPhaseTimer(room)
	room.Game.AfkSeq++
	room.Game.Deadline = time.Now().Add(dur).UnixMilli()
	sig := skPhaseSignal{GameID: room.Game.ID, Seq: room.Game.AfkSeq}
	room.PhaseTimer = time.AfterFunc(dur, func() {
		h.phaseFired <- sig
	})
}

func (h *SKHub) stopPhaseTimer(room *skRoom) {
	if room.PhaseTimer != nil {
		room.PhaseTimer.Stop()
		room.PhaseTimer = nil
	}
}

// handlePhaseFired 단계 타임아웃 — 자동 행동으로 해소한다
func (h *SKHub) handlePhaseFired(sig skPhaseSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.Game.AfkSeq != sig.Seq {
		return
	}
	game := room.Game
	switch game.Phase {
	case SKPhasePlacing:
		if !game.PlacingTurns {
			// 동시 배치 마감 — 미배치 전원 무작위 카드 1장
			log.Printf("[스컬][배치마감] game=%s | %d라운드 | 타임아웃 — 미배치 자동 배치",
				game.ID, game.RoundNo)
			game.AutoPlaceAll(h.rng)
			h.scheduleDeadline(room, skAfkTimeout)
			h.broadcastState(room)
			return
		}
		cur := game.CurrentSeat
		if cur < 0 {
			return
		}
		prevChallenger := game.ChallengerSeat
		if len(game.Players[cur].Hand) > 0 {
			// 턴 방치 — 무작위 카드 1장 배치
			if err := game.SubmitPlace(cur, h.rng.Intn(len(game.Players[cur].Hand))); err != nil {
				return
			}
			log.Printf("[스컬][배치마감] game=%s | %d라운드 | seat%d 타임아웃 — 자동 배치",
				game.ID, game.RoundNo, cur)
			h.broadcastEvent(room, SKEventPayload{Kind: "placed", Seat: &cur, Name: game.Players[cur].Name})
			h.scheduleDeadline(room, skAfkTimeout)
			h.broadcastState(room)
			return
		}
		// 손패가 없으면 최소 배팅 1을 대신 선언한다
		if err := game.SubmitBid(cur, 1, h.rng); err != nil {
			return
		}
		log.Printf("[스컬][배치마감] game=%s | %d라운드 | seat%d 타임아웃 — 자동 배팅 1",
			game.ID, game.RoundNo, cur)
		h.broadcastEvent(room, SKEventPayload{Kind: "bid", Seat: &cur, Name: game.Players[cur].Name,
			Message: fmt.Sprintf("%s님이 장미 1장 뒤집기를 선언했습니다 (자동)", game.Players[cur].Name)})
		h.afterProgress(room, prevChallenger)

	case SKPhaseBidding:
		cur := game.CurrentSeat
		if cur < 0 {
			return
		}
		prevChallenger := game.ChallengerSeat
		if err := game.SubmitPass(cur, h.rng); err != nil {
			return
		}
		log.Printf("[스컬][배팅마감] game=%s | %d라운드 | seat%d 타임아웃 — 자동 패스",
			game.ID, game.RoundNo, cur)
		h.broadcastEvent(room, SKEventPayload{Kind: "pass", Seat: &cur, Name: game.Players[cur].Name,
			Message: fmt.Sprintf("%s님이 패스했습니다 (자동)", game.Players[cur].Name)})
		h.afterProgress(room, prevChallenger)

	case SKPhaseFlipping:
		target := game.RandomFlipTarget(h.rng)
		if target < 0 {
			return
		}
		prevChallenger := game.ChallengerSeat
		if err := game.SubmitFlip(game.ChallengerSeat, target, h.rng); err != nil {
			return
		}
		log.Printf("[스컬][뒤집기마감] game=%s | %d라운드 | 타임아웃 — seat%d 더미 자동 뒤집기",
			game.ID, game.RoundNo, target)
		h.emitFlipEvent(room)
		h.afterProgress(room, prevChallenger)

	case SKPhaseRoundEnd:
		game.NextRound()
		h.announceRoundBegin(room)
		h.scheduleDeadline(room, skAfkTimeout)
		h.broadcastState(room)
	}
}

// ==================== 상태 뷰 (은닉의 핵심) ====================

// skPlayerViews 좌석별 공개 정보 — 카드는 장수만, 내용은 절대 싣지 않는다
func (h *SKHub) skPlayerViews(room *skRoom) []SKPlayerView {
	game := room.Game
	players := []SKPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		players = append(players, SKPlayerView{
			Seat:       p.Seat,
			Name:       p.Name,
			Connected:  c != nil && c.Connected,
			Bot:        c != nil && c.Bot,
			Alive:      p.Alive,
			HandCount:  len(p.Hand),
			StackCount: len(p.Stack),
			Points:     p.Points,
			Passed:     p.Passed,
			Bid:        p.Bid,
		})
	}
	return players
}

// buildSKState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
// 은닉: 손패·더미 내용은 본인(yourHand/yourStack)에게만, 제거된 카드는
// 아무에게도(저장조차 안 함), 뒤집힌 카드(flipped)만 공개.
// viewerSeat -1(관전자)도 안전하다 — 좌석 인덱싱 전에 범위를 가드한다.
func (h *SKHub) buildSKState(room *skRoom, viewerSeat int) SKGameStatePayload {
	game := room.Game

	yourHand := []string{}
	yourStack := []string{}
	if viewerSeat >= 0 && viewerSeat < len(game.Players) {
		for _, c := range game.Players[viewerSeat].Hand {
			yourHand = append(yourHand, string(c))
		}
		for _, c := range game.Players[viewerSeat].Stack {
			yourStack = append(yourStack, string(c))
		}
	}

	endsAt := int64(0)
	switch game.Phase {
	case SKPhasePlacing, SKPhaseBidding, SKPhaseFlipping, SKPhaseRoundEnd:
		endsAt = game.Deadline
	}

	flipTarget := 0
	if game.Phase == SKPhaseFlipping {
		flipTarget = game.HighBid - game.rosesFlipped()
	}

	return SKGameStatePayload{
		GameID:         game.ID,
		RoomCode:       room.Code,
		Phase:          game.Phase,
		HostSeat:       h.hostSeat(room),
		YourSeat:       viewerSeat,
		Spectators:     len(room.Spectators),
		EndsAt:         endsAt,
		CurrentSeat:    game.CurrentSeat,
		YourHand:       yourHand,
		YourStack:      yourStack,
		Players:        h.skPlayerViews(room),
		HighBid:        game.HighBid,
		ChallengerSeat: game.ChallengerSeat,
		// nil 슬라이스는 JSON null 로 나가 프론트 .length 접근이 죽는다 — 빈 배열 보장
		Flipped:     append([]SKFlippedCard{}, game.Flipped...),
		FlipTarget:  flipTarget,
		RoundResult: game.RoundResult,
	}
}

// broadcastState 좌석마다 개인화 스냅샷을 만들어 보낸다.
// 관전자에게는 공개 정보만 담긴 스냅샷(viewerSeat -1)이 간다.
func (h *SKHub) broadcastState(room *skRoom) {
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, SKMessage{
			Type:    SKMsgGameState,
			Payload: h.buildSKState(room, seat),
		})
	}
	if len(room.Spectators) > 0 {
		msg := SKMessage{Type: SKMsgGameState, Payload: h.buildSKState(room, -1)}
		for c := range room.Spectators {
			h.sendToClient(c, msg)
		}
	}
}

func (h *SKHub) broadcastEvent(room *skRoom, event SKEventPayload) {
	h.broadcastToRoom(room, SKMessage{Type: SKMsgEvent, Payload: event})
}

// finishGame 종료 발표와 방 정리 (재대결은 같은 방 코드 재입장으로 —
// finishGame 이 코드를 반납해 같은 코드로 새 사설 방을 만들 수 있다)
func (h *SKHub) finishGame(room *skRoom) {
	game := room.Game
	h.stopPhaseTimer(room)
	game.Deadline = 0

	winner := game.WinnerSeat
	winnerName := ""
	reason := "단독 생존"
	if winner >= 0 && winner < len(game.Players) {
		winnerName = game.Players[winner].Name
		if game.Players[winner].Points >= SKWinPoints {
			reason = fmt.Sprintf("%d점 선취", SKWinPoints)
		}
	}
	detail := fmt.Sprintf("%s 승리 (%s)", winnerName, reason)

	losers := []string{}
	for _, p := range game.Players {
		if p.Seat != winner {
			losers = append(losers, displayName(p.Name))
		}
	}

	h.broadcastEvent(room, SKEventPayload{Kind: "game_over", Seat: &winner,
		Name: winnerName, Message: detail})
	// 최종 스냅샷을 먼저 보낸 뒤 종료를 알린다
	// (봇 러너는 sk_game_over 를 보고 스스로 끝난다)
	h.broadcastState(room)
	h.broadcastToRoom(room, SKMessage{
		Type: SKMsgGameOver,
		Payload: SKGameOverPayload{
			WinnerSeat: winner,
			WinnerName: winnerName,
			Reason:     reason,
			Players:    h.skPlayerViews(room),
		},
	})

	log.Printf("[스컬][경기결과] game=%s | %s 승리 (%s) | 소요=%s",
		game.ID, displayName(winnerName), reason, matchDuration(game.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "skull",
		Players:  displayName(winnerName) + " vs " + strings.Join(losers, "·"),
		Winner:   displayName(winnerName),
		Reason:   reason,
		Duration: matchSeconds(game.StartedAt),
		Bot:      skRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
	if room.Code != "" {
		delete(h.activeCodes, room.Code) // 코드 재사용 복귀
	}
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *SKHub) handleDisconnect(client *SKClient) {
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
	log.Printf("[스컬][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, SKMessage{
		Type: SKMsgOpponentDisconnected,
		Payload: SKOpponentDisconnectedPayload{
			Seat:         client.Seat,
			Name:         client.Name,
			GraceSeconds: int(h.grace.Seconds()),
		},
	})
	h.broadcastState(room)

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 안에 재접속하지 않은 좌석은 연습봇으로 대체하고
// 게임은 계속한다 — 배치·배팅·뒤집기 진행이 이탈 좌석에 막히지 않는 근거
func (h *SKHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == SKPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[스컬][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := h.takeoverBot(room, seat, client.Name)
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.broadcastEvent(room, SKEventPayload{Kind: "bot_takeover", Seat: &seat, Name: client.Name})
	// 새 봇이 이 스냅샷을 받아 제출이 남았으면 즉시 이어간다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *SKHub) handleRejoin(client *SKClient, msg SKMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SKRejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, SKMessage{Type: SKMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, SKMessage{Type: SKMsgSessionExpired})
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

	log.Printf("[스컬][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, SKMessage{
		Type:    SKMsgReconnected,
		Payload: SKReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	// 전원에게 접속 상태가 반영된 스냅샷 (재접속 당사자 복원 포함)
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *SKHub) clearGameSessions(room *skRoom) {
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		h.drop(c.SessionID)
	}
}

// ==================== 전송 ====================

func (h *SKHub) sendError(client *SKClient, message string) {
	h.sendToClient(client, SKMessage{Type: SKMsgError, Payload: SKErrorPayload{Message: message}})
}

func (h *SKHub) sendToClient(client *SKClient, message SKMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[SK] ")
}

func (h *SKHub) broadcastToRoom(room *skRoom, message SKMessage) {
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

func ServeSKWs(hub *SKHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[SK] Error upgrading connection:", err)
		return
	}

	client := &SKClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[SK] ",
		func(msg SKMessage) { hub.gameMessage <- SKGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
