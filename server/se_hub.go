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

// ==================== 세트 허브 ====================
//
// 다인 결(kr_hub/cw_hub)을 그대로 따르되 **턴 상태기계만 들어냈다**.
// 이 게임에는 차례가 없다 — currentSeat 도, 좌석별 AFK 자동 진행도 없고,
// 대기 상태 일련번호(StateSeq)로 마감을 다시 거는 syncDeadline 도 없다.
//
// 대신 선착 판정이다. se_claim 은 전부 h.gameMessage 채널로 모이고 허브
// 고루틴이 도착한 순서대로 하나씩 처리하므로, 동시에 같은 세트를 집어도
// 먼저 도착한 쪽만 성립하고 뒤에 온 쪽은 "이미 사라진 카드"로 오답 처리된다.
// 판정이 한 고루틴에 직렬화되므로 게임 상태에는 락이 필요 없다.
//
// 남은 타이머는 하나뿐이다 — 시작 10분 뒤의 강제 종료(무한 게임 방지).
// 좌석 잠금(5초)은 타이머가 아니라 lockedUntil 타임스탬프로만 다룬다.
// 서버는 claim 이 올 때 시각을 비교하고, 프론트는 그 값으로 카운트다운한다.

// seRoom 게임(순수 상태)과 좌석별 연결의 매핑
type seRoom struct {
	Game    *SEGame
	Clients map[int]*SEClient // seat → client

	// EndTimer 강제 종료 타이머 (시작 때 한 번만 건다)
	EndTimer *time.Timer

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string

	// Spectators 관전자 연결 (사설 방 코드로만 진입, 상한 maxSpectators).
	// 좌석·세션이 없어 재접속을 지원하지 않는다 — 끊기면 목록에서만 뗀다.
	Spectators map[*SEClient]bool

	// LastReact 좌석별 마지막 리액션 발신 시각 (레이트리밋 장부)
	LastReact map[int]time.Time
}

// seEndSignal 강제 종료 타이머의 발화 표식 — 일련번호로 지나간 발화를 구분한다
type seEndSignal struct {
	GameID string
	Seq    int
}

type SEHub struct {
	// 등록된 클라이언트
	clients map[*SEClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*seRoom

	// 글로벌 로비. 시작 전 공용 방은 하나뿐이다.
	lobby *seRoom

	// 사설 방 (초대 코드 → 시작 전 방). 허브 고루틴에서만 접근한다.
	// 시작하면 privateLobbies 에서 activeCodes 로 옮긴다.
	privateLobbies map[string]*seRoom

	// 진행 중 사설 방 (초대 코드 → gameID). 시작 후에도 코드로 방을 찾아
	// 관전 입장시키는 근거다. finishGame 에서 해제한다 (코드 재사용 복귀).
	activeCodes map[string]string

	// 클라이언트 등록
	register chan *SEClient

	// 클라이언트 등록 해제
	unregister chan *SEClient

	// 게임 메시지 — 선착 판정의 직렬화 지점이다
	gameMessage chan SEGameMessage

	// 강제 종료 타이머 발화 (time.AfterFunc → 허브 채널 경유)
	endFired chan seEndSignal

	// forceEnd 강제 종료까지의 시간 (테스트가 Run 전에 낮춘다)
	forceEnd time.Duration

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*SEClient]

	// 덱 셔플·바닥 재배치용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type SEGameMessage struct {
	Client  *SEClient
	Message SEMessage
}

func NewSEHub() *SEHub {
	return &SEHub{
		register:       make(chan *SEClient),
		unregister:     make(chan *SEClient),
		clients:        make(map[*SEClient]bool),
		rooms:          make(map[string]*seRoom),
		privateLobbies: make(map[string]*seRoom),
		activeCodes:    make(map[string]string),
		gameMessage:    make(chan SEGameMessage),
		endFired:       make(chan seEndSignal, 8),
		forceEnd:       seForceEnd,
		sessionManager: newSessionManager[*SEClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *SEHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[SE] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[SE] Client unregistered: %s", client.ID)
			}

		case sessionID := <-h.graceExpired:
			h.handleGraceExpired(sessionID)

		case sig := <-h.endFired:
			h.handleEndFired(sig)

		case message := <-h.gameMessage:
			h.handleGameMessage(message)
		}
	}
}

func (h *SEHub) handleGameMessage(gm SEGameMessage) {
	// 관전자는 어떤 행동도 할 수 없다 (보기 전용 — 리액션도 좌석 보유자만)
	if h.isSpectator(gm.Client) {
		h.sendError(gm.Client, spectatorDeniedMsg)
		return
	}
	switch gm.Message.Type {
	case SEMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case SEMsgReact:
		h.handleReact(gm.Client, gm.Message)
	case SEMsgFillBots:
		h.handleFillBots(gm.Client)
	case SEMsgStart:
		h.handleStart(gm.Client)
	case SEMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case SEMsgClaim:
		h.handleClaim(gm.Client, gm.Message)
	}
}

// ==================== 대기실 ====================

func (h *SEHub) handleJoinGame(client *SEClient, msg SEMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SEJoinGamePayload
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

	log.Printf("[세트][입장] game=%s | seat%d=%s 대기 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), SEMaxPlayers)
	if room.Code == "" { // 사설 방 입장은 조용히 (공용 로비만 알림)
		notify("세트 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), len(room.Game.Players), SEMaxPlayers))
	}

	h.sendToClient(client, SEMessage{
		Type: SEMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
			"roomCode":  room.Code,
		},
	})

	h.broadcastEvent(room, SEEventPayload{Kind: "joined", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 입장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	// 정원이 차도 자동 시작하지 않는다 — 호스트 명시 시작만 (봇 채우기와 충돌 방지)
	h.broadcastState(room)
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다.
// 생략/빈 문자열은 기존 공용 로비 경로 그대로, "NEW"는 새 코드 발급,
// 그 외 코드는 해당 사설 방 (없으면 그 코드로 관대하게 새로 생성).
func (h *SEHub) lobbyRoomFor(roomField string) *seRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewSEGame(uuid.New().String())
			h.lobby = &seRoom{Game: game, Clients: map[int]*SEClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[SE] Created lobby game %s", game.ID)
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
		game := NewSEGame(uuid.New().String())
		room = &seRoom{Game: game, Clients: map[int]*SEClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[SE] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// ==================== 관전 / 리액션 ====================

// addSpectator 진행 중(또는 가득 찬) 사설 방에 관전자로 등록한다.
// 좌석·세션 없음 — 재접속 미지원, 끊기면 다시 코드로 들어온다.
// 은닉이 없는 게임이라 관전자는 참가자와 완전히 같은 스냅샷을 본다.
func (h *SEHub) addSpectator(room *seRoom, client *SEClient, name string) {
	if len(room.Spectators) >= maxSpectators {
		h.sendError(client, spectatorFullMsg)
		return
	}
	if room.Spectators == nil {
		room.Spectators = map[*SEClient]bool{}
	}
	client.Name = name
	client.GameID = room.Game.ID
	room.Spectators[client] = true

	log.Printf("[세트][관전] game=%s | %s 관전 입장 (%d명)",
		room.Game.ID, displayName(name), len(room.Spectators))

	h.sendToClient(client, SEMessage{
		Type:    SEMsgSpectateJoined,
		Payload: map[string]interface{}{"gameId": room.Game.ID, "roomCode": room.Code},
	})
	// 전원(관전자 포함)에게 관전자 수가 반영된 스냅샷 — 신규 관전자의 첫 스냅샷 겸용
	h.broadcastState(room)
}

// isSpectator 관전자 연결인지 (좌석 보유자·미입장 연결은 false)
func (h *SEHub) isSpectator(client *SEClient) bool {
	if client.GameID == "" {
		return false
	}
	room := h.rooms[client.GameID]
	return room != nil && room.Spectators[client]
}

// handleReact 리액션 이모지 — 좌석 보유자만, waiting 중에도 허용.
// 화이트리스트 외·레이트리밋 초과는 조용히 무시한다 (상태 저장 없음).
func (h *SEHub) handleReact(client *SEClient, msg SEMessage) {
	room := h.rooms[client.GameID]
	if room == nil || client.Seat < 0 {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SEReactPayload
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
	h.broadcastEvent(room, SEEventPayload{Kind: "react", Seat: &seat, Name: client.Name,
		Message: payload.Emoji})
}

// waitingRoomOf 클라이언트가 속한 시작 전 방 (공용 로비 또는 사설 방)
func (h *SEHub) waitingRoomOf(client *SEClient) *seRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Ready {
		return nil
	}
	return room
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *SEHub) hostSeat(room *seRoom) int {
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

// seHumanCount 방의 사람 수
func seHumanCount(room *seRoom) int {
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
func (h *SEHub) updateLobbyWaiting(room *seRoom) {
	if room.Code != "" {
		return
	}
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && seHumanCount(room) >= 1
	lobbySetWaiting("set", waiting)
}

// handleFillBots host 가 빈 좌석을 연습봇으로 4인까지 채운 뒤 즉시
// 시작한다 (스폰은 join 을 거치지 않으므로 명시 시작 호출).
func (h *SEHub) handleFillBots(client *SEClient) {
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
	for len(room.Game.Players) < SEFillBotTarget {
		botNo++
		if !h.spawnSEBot(room, fmt.Sprintf("%s%d", botName, botNo)) {
			break
		}
	}

	if room.Game.CanStart() {
		h.startGame(room)
		return
	}
	h.broadcastState(room)
}

func (h *SEHub) handleStart(client *SEClient) {
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
		h.sendError(client, fmt.Sprintf("%d명 이상 모여야 시작할 수 있습니다", SEMinPlayers))
		return
	}
	h.startGame(room)
}

func (h *SEHub) startGame(room *seRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	if room.Code != "" {
		// 시작한 사설 방의 코드는 진행 중 장부로 옮긴다 (관전 입장 근거)
		delete(h.privateLobbies, room.Code)
		h.activeCodes[room.Code] = room.Game.ID
	} else {
		h.lobby = nil // 시작한 방은 로비에서 떼어낸다
		lobbySetWaiting("set", false)
	}

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	log.Printf("[세트][경기시작] game=%s | 인원=%d | 바닥=%d장 | 덱=%d장 | 캡=%.0f분 | %v",
		room.Game.ID, len(room.Game.Players), len(room.Game.Board), len(room.Game.Deck),
		h.forceEnd.Minutes(), names)
	if !seRoomHasBot(room) { // 연습봇전은 운영자 알림을 억제한다
		notify("세트 시작", fmt.Sprintf("%d인전 시작", len(room.Game.Players)))
	}

	h.broadcastEvent(room, SEEventPayload{Kind: "game_started",
		Message: fmt.Sprintf(
			"게임 시작 — %d인전, 차례가 없습니다. 세트를 먼저 찾아 누르는 사람이 가져갑니다 (오답은 -1점·%.0f초 잠금)",
			len(room.Game.Players), seClaimLock.Seconds())})

	// 타이머는 이 하나뿐이다 — 무한 게임 방지용 강제 종료 캡
	h.scheduleForceEnd(room)
	h.afterProgress(room)
}

// removeFromLobby 대기실에서 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *SEHub) removeFromLobby(room *seRoom, client *SEClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	// 좌석 압축: oldSeat 뒤의 클라이언트를 한 칸씩 당긴다
	rebuilt := map[int]*SEClient{}
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

	log.Printf("[세트][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), SEMaxPlayers)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if seHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, SEMessage{Type: SEMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			lobbySetWaiting("set", false)
		}
		return
	}

	h.broadcastEvent(room, SEEventPayload{Kind: "left", Name: client.Name,
		Message: fmt.Sprintf("%s님이 퇴장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 (선착 판정) ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *SEHub) roomOf(client *SEClient) *seRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

// handleClaim 세트 선언. 차례 검사가 없다 — 누구든 언제든 보낼 수 있고,
// 이 함수에 도착한 순서가 곧 판정 순서다(허브 고루틴 직렬화).
// 같은 세트를 동시에 집으면 뒤에 온 쪽의 카드는 이미 바닥에서 사라져 있어
// 자연히 오답(-1점·잠금)이 된다.
func (h *SEHub) handleClaim(client *SEClient, msg SEMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SEClaimPayload
	json.Unmarshal(payloadBytes, &payload)

	game := room.Game
	if err := game.Claim(client.Seat, payload.IDs, time.Now(), h.rng); err != nil {
		h.sendError(client, err.Error())
		return
	}

	claim := game.LastClaim
	if claim != nil && claim.OK {
		log.Printf("[세트][성립] game=%s | seat%d=%s ids=%v → %d점 (세트 %d개, 바닥 %d장, 덱 %d장)",
			game.ID, client.Seat, displayName(client.Name), claim.IDs,
			game.Players[client.Seat].Score, game.SetsFound, len(game.Board), len(game.Deck))
	} else if claim != nil {
		log.Printf("[세트][오답] game=%s | seat%d=%s ids=%v → %s (%d점, %.0f초 잠금)",
			game.ID, client.Seat, displayName(client.Name), claim.IDs, claim.Message,
			game.Players[client.Seat].Score, seClaimLock.Seconds())
	}
	h.afterProgress(room)
}

// afterProgress 모든 게임 변이 뒤의 공통 마무리 — 이벤트 방송·종료 판정·
// 스냅샷 방송. 차례가 없어 대기 상태 마감을 다시 걸 일이 없다.
func (h *SEHub) afterProgress(room *seRoom) {
	h.drainEvents(room)
	if room.Game.Phase == SEPhaseGameOver {
		h.finishGame(room)
		return
	}
	h.broadcastState(room)
}

// drainEvents 순수 규칙이 쌓은 이벤트를 se_event 로 방송한다
func (h *SEHub) drainEvents(room *seRoom) {
	for _, ev := range room.Game.DrainEvents() {
		payload := SEEventPayload{Kind: ev.Kind, Message: ev.Message}
		if ev.Seat >= 0 && ev.Seat < len(room.Game.Players) {
			seat := ev.Seat
			payload.Seat = &seat
			payload.Name = room.Game.Players[seat].Name
		}
		h.broadcastEvent(room, payload)
	}
}

// ==================== 강제 종료 캡 (무한 게임 방지) ====================

// scheduleForceEnd 시작 시 한 번만 거는 강제 종료 타이머. 차례가 없어
// 재예약이 없으므로 일련번호는 방이 재사용될 때의 지연 발화만 걸러낸다.
func (h *SEHub) scheduleForceEnd(room *seRoom) {
	h.stopEndTimer(room)
	room.Game.EndSeq++
	room.Game.Deadline = time.Now().Add(h.forceEnd).UnixMilli()
	sig := seEndSignal{GameID: room.Game.ID, Seq: room.Game.EndSeq}
	room.EndTimer = time.AfterFunc(h.forceEnd, func() {
		h.endFired <- sig
	})
}

func (h *SEHub) stopEndTimer(room *seRoom) {
	if room.EndTimer != nil {
		room.EndTimer.Stop()
		room.EndTimer = nil
	}
}

// handleEndFired 강제 종료 발화 — 현재 점수 그대로 정산한다
func (h *SEHub) handleEndFired(sig seEndSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.Game.EndSeq != sig.Seq || room.Game.Phase != SEPhasePlaying {
		return
	}
	log.Printf("[세트][강제종료] game=%s | %.0f분 경과 — 현재 점수로 정산 (세트 %d개)",
		room.Game.ID, h.forceEnd.Minutes(), room.Game.SetsFound)
	h.broadcastEvent(room, SEEventPayload{Kind: "time_up",
		Message: "제한 시간이 끝나 현재 점수로 정산합니다"})
	room.Game.ForceEnd()
	h.afterProgress(room)
}

// ==================== 상태 뷰 (은닉 없음) ====================

// buildSEState 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
//
// 이 게임에는 은닉이 없다 — viewerSeat 가 무엇이든 yourSeat 를 뺀 모든 필드가
// 동일하다. 관전자(viewerSeat -1)도 참가자와 똑같은 바닥·점수·잠금을 본다.
// 빈 대기실(플레이어 0명·관전자 시점)에도 패닉 없이 빈 배열을 돌려준다.
func (h *SEHub) buildSEState(room *seRoom, viewerSeat int) SEGameStatePayload {
	game := room.Game

	players := []SEPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		players = append(players, SEPlayerView{
			Seat:        p.Seat,
			Name:        p.Name,
			Connected:   c != nil && c.Connected,
			Bot:         c != nil && c.Bot,
			Score:       p.Score,
			LockedUntil: p.LockedUntil,
		})
	}

	endsAt := int64(0)
	if game.Phase == SEPhasePlaying {
		endsAt = game.Deadline
	}

	return SEGameStatePayload{
		GameID:     game.ID,
		RoomCode:   room.Code,
		Phase:      game.Phase,
		HostSeat:   h.hostSeat(room),
		YourSeat:   viewerSeat,
		Spectators: len(room.Spectators),
		EndsAt:     endsAt,
		Board:      append([]SECard{}, game.Board...),
		DeckLeft:   len(game.Deck),
		SetsFound:  game.SetsFound,
		Players:    players,
		LastClaim:  game.LastClaim,
		Result:     game.Result,
	}
}

// broadcastState 좌석마다 스냅샷을 보낸다. 관전자에게 가는 스냅샷은
// yourSeat 가 -1 일 뿐 내용이 완전히 같다 (은닉 없음).
func (h *SEHub) broadcastState(room *seRoom) {
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, SEMessage{
			Type:    SEMsgGameState,
			Payload: h.buildSEState(room, seat),
		})
	}
	if len(room.Spectators) > 0 {
		msg := SEMessage{Type: SEMsgGameState, Payload: h.buildSEState(room, -1)}
		for c := range room.Spectators {
			h.sendToClient(c, msg)
		}
	}
}

func (h *SEHub) broadcastEvent(room *seRoom, event SEEventPayload) {
	h.broadcastToRoom(room, SEMessage{Type: SEMsgEvent, Payload: event})
}

// finishGame 게임 종료 발표·전적 기록·방 정리 (재대결은 같은 방 코드
// 재입장으로 — finishGame 이 코드를 반납해 같은 코드로 새 방을 만들 수 있다)
func (h *SEHub) finishGame(room *seRoom) {
	game := room.Game
	h.stopEndTimer(room)
	game.Deadline = 0

	result := game.Result
	if result == nil { // 방어선 — 종료는 항상 결과와 함께 온다
		seats, names := seWinners(game.Players)
		result = &SEResult{WinnerSeats: seats, WinnerNames: names,
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
		reason = "deck_empty"
	}

	h.broadcastEvent(room, SEEventPayload{Kind: "game_over",
		Message: fmt.Sprintf("게임 종료 — %s", result.Message)})
	// 최종 점수가 반영된 스냅샷을 먼저 보낸 뒤 종료를 알린다
	// (봇 러너는 se_game_over 를 보고 스스로 끝난다)
	h.broadcastState(room)
	h.broadcastToRoom(room, SEMessage{
		Type: SEMsgGameOver,
		Payload: SEGameOverPayload{
			WinnerSeats: append([]int{}, result.WinnerSeats...),
			WinnerNames: append([]string{}, result.WinnerNames...),
			Reason:      reason,
			Message:     result.Message,
			SetsFound:   game.SetsFound,
			Players:     h.buildSEState(room, -1).Players,
		},
	})

	scores := []int{}
	for _, p := range game.Players {
		scores = append(scores, p.Score)
	}
	log.Printf("[세트][경기결과] game=%s | 승자=%v(%s) | 사유=%s | 점수=%v | 세트=%d개 | 소요=%s",
		game.ID, result.WinnerSeats, strings.Join(winners, "·"), reason,
		scores, game.SetsFound, matchDuration(game.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "set",
		Players:  strings.Join(all, " vs "),
		Winner:   strings.Join(winners, "·"), // 동점 공동 승리는 "·" 로 잇는다
		Reason:   reason,
		Duration: matchSeconds(game.StartedAt),
		Bot:      seRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
	if room.Code != "" {
		delete(h.activeCodes, room.Code) // 코드 재사용 복귀
	}
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *SEHub) handleDisconnect(client *SEClient) {
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

	// 진행 중: 유예 시간 동안 재접속을 기다린다 (만료 시 봇 대체).
	// 차례가 없어 이탈이 진행을 막지는 않지만, 빈 좌석이 남지 않도록
	// 다른 게임과 같은 90초 봇 대체를 그대로 둔다.
	log.Printf("[세트][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, SEMessage{
		Type: SEMsgPlayerDisconnected,
		Payload: SEPlayerDisconnectedPayload{
			Seat:         client.Seat,
			Name:         client.Name,
			GraceSeconds: int(h.grace.Seconds()),
		},
	})
	h.broadcastState(room)

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 안에 재접속하지 않은 좌석은 연습봇으로 대체한다
func (h *SEHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == SEPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[세트][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := h.takeoverSEBot(room, seat, client.Name)
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.broadcastEvent(room, SEEventPayload{Kind: "bot_takeover", Seat: &seat,
		Name:    client.Name,
		Message: fmt.Sprintf("%s님 자리를 봇이 이어받았습니다", client.Name)})
	// 새 봇이 이 스냅샷을 받아 곧바로 바닥을 훑기 시작한다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *SEHub) handleRejoin(client *SEClient, msg SEMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SERejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, SEMessage{Type: SEMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, SEMessage{Type: SEMsgSessionExpired})
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

	log.Printf("[세트][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, SEMessage{
		Type:    SEMsgPlayerReconnected,
		Payload: SEPlayerReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	// 전원에게 접속 상태가 반영된 스냅샷 (재접속 당사자의 바닥 복원 포함)
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *SEHub) clearGameSessions(room *seRoom) {
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		h.drop(c.SessionID)
	}
}

// ==================== 전송 ====================

func (h *SEHub) sendError(client *SEClient, message string) {
	h.sendToClient(client, SEMessage{Type: SEMsgError, Payload: SEErrorPayload{Message: message}})
}

func (h *SEHub) sendToClient(client *SEClient, message SEMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[SE] ")
}

func (h *SEHub) broadcastToRoom(room *seRoom, message SEMessage) {
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

func ServeSEWs(hub *SEHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[SE] Error upgrading connection:", err)
		return
	}

	client := &SEClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[SE] ",
		func(msg SEMessage) { hub.gameMessage <- SEGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
