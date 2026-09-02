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

// vgAfkTimeout 접속 유지 AFK 의 자동 진행 대기 시간 — 발화하면 현재 차례
// 좌석의 굴린 주사위 중 최다 눈을 자동 배치한다 (테스트에서 짧게 낮춘다).
var vgAfkTimeout = 30 * time.Second

// vgRoundEndDelay round_end 정산 결과 표시 후 다음 라운드로 자동 진행하기까지의
// 시간 (테스트에서 짧게 낮춘다)
var vgRoundEndDelay = 4 * time.Second

// vgRoom 게임(순수 상태)과 좌석별 연결의 매핑
type vgRoom struct {
	Game    *VGGame
	Clients map[int]*VGClient // seat → client

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string

	// AfkTimer 접속 유지 AFK 구제 타이머 — 상태가 바뀔 때마다 리셋되고,
	// 발화하면 현재 차례 좌석의 최다 눈을 자동 배치한다.
	AfkTimer *time.Timer
	// AfkSeq 상태 변경 일련번호 (뒤늦은 발화 무시용)
	AfkSeq int
	// EndsAt 현재 차례의 AFK 마감 시각 (unixMillis) — 스냅샷 노출용
	EndsAt int64

	// RoundTimer round_end 자동 진행 타이머
	RoundTimer *time.Timer

	// Spectators 관전자 연결 (사설 방 코드로만 진입, 상한 maxSpectators).
	// 좌석·세션이 없어 재접속을 지원하지 않는다 — 끊기면 목록에서만 뗀다.
	Spectators map[*VGClient]bool

	// LastReact 좌석별 마지막 리액션 발신 시각 (레이트리밋 장부)
	LastReact map[int]time.Time
}

// vgAfkSignal AFK 타이머 발화 표식
type vgAfkSignal struct {
	GameID string
	Seq    int
}

// vgRoundSignal 어느 게임의 몇 번째 라운드 종료 타이머인지 (뒤늦은 발화 무시용)
type vgRoundSignal struct {
	GameID string
	Round  int
}

type VGHub struct {
	// 등록된 클라이언트
	clients map[*VGClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*vgRoom

	// 글로벌 로비. 시작 전 공용 방은 하나뿐이다.
	lobby *vgRoom

	// 사설 방 (초대 코드 → 시작 전 방). 허브 고루틴에서만 접근한다.
	// 시작하면 privateLobbies 에서 activeCodes 로 옮긴다.
	privateLobbies map[string]*vgRoom

	// 진행 중 사설 방 (초대 코드 → gameID). 시작 후에도 코드로 방을 찾아
	// 관전 입장시키는 근거다. finishGame 에서 해제한다 (코드 재사용 복귀).
	activeCodes map[string]string

	// 클라이언트 등록
	register chan *VGClient

	// 클라이언트 등록 해제
	unregister chan *VGClient

	// 게임 메시지
	gameMessage chan VGGameMessage

	// 자동 진행 알림 (time.AfterFunc → 허브 채널 경유)
	roundFired chan vgRoundSignal
	afkFired   chan vgAfkSignal

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*VGClient]

	// 주사위·지폐 셔플·선공 결정용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type VGGameMessage struct {
	Client  *VGClient
	Message VGMessage
}

func NewVGHub() *VGHub {
	return &VGHub{
		register:       make(chan *VGClient),
		unregister:     make(chan *VGClient),
		clients:        make(map[*VGClient]bool),
		rooms:          make(map[string]*vgRoom),
		privateLobbies: make(map[string]*vgRoom),
		activeCodes:    make(map[string]string),
		gameMessage:    make(chan VGGameMessage),
		roundFired:     make(chan vgRoundSignal, 8),
		afkFired:       make(chan vgAfkSignal, 8),
		sessionManager: newSessionManager[*VGClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *VGHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[VG] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[VG] Client unregistered: %s", client.ID)
			}

		case sessionID := <-h.graceExpired:
			h.handleGraceExpired(sessionID)

		case sig := <-h.roundFired:
			h.handleRoundFired(sig)

		case sig := <-h.afkFired:
			h.handleAfkFired(sig)

		case message := <-h.gameMessage:
			h.handleGameMessage(message)
		}
	}
}

func (h *VGHub) handleGameMessage(gm VGGameMessage) {
	// 관전자는 어떤 행동도 할 수 없다 (보기 전용 — 리액션도 좌석 보유자만)
	if h.isSpectator(gm.Client) {
		h.sendError(gm.Client, spectatorDeniedMsg)
		return
	}
	switch gm.Message.Type {
	case VGMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case VGMsgReact:
		h.handleReact(gm.Client, gm.Message)
	case VGMsgFillBots:
		h.handleFillBots(gm.Client)
	case VGMsgStart:
		h.handleStart(gm.Client)
	case VGMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case VGMsgPlace:
		h.handlePlace(gm.Client, gm.Message)
	}
}

// ==================== 대기실 ====================

func (h *VGHub) handleJoinGame(client *VGClient, msg VGMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload VGJoinGamePayload
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

	log.Printf("[라스베가스][입장] game=%s | seat%d=%s 대기 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), VGMaxPlayers)
	if room.Code == "" { // 사설 방 입장은 조용히 (공용 로비만 알림)
		notify("라스베가스 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), len(room.Game.Players), VGMaxPlayers))
	}

	h.sendToClient(client, VGMessage{
		Type: VGMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
			"roomCode":  room.Code,
		},
	})

	h.broadcastEvent(room, VGEventPayload{Kind: "joined", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 입장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	// 정원이 차도 자동 시작하지 않는다 — 호스트 명시 시작만 (봇 채우기와 충돌 방지)
	h.broadcastState(room)
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다.
// 생략/빈 문자열은 기존 공용 로비 경로 그대로, "NEW"는 새 코드 발급,
// 그 외 코드는 해당 사설 방 (없으면 그 코드로 관대하게 새로 생성).
func (h *VGHub) lobbyRoomFor(roomField string) *vgRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewVGGame(uuid.New().String())
			h.lobby = &vgRoom{Game: game, Clients: map[int]*VGClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[VG] Created lobby game %s", game.ID)
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
		game := NewVGGame(uuid.New().String())
		room = &vgRoom{Game: game, Clients: map[int]*VGClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[VG] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// ==================== 관전 / 리액션 ====================

// addSpectator 진행 중(또는 가득 찬) 사설 방에 관전자로 등록한다.
// 좌석·세션 없음 — 재접속 미지원, 끊기면 다시 코드로 들어온다.
func (h *VGHub) addSpectator(room *vgRoom, client *VGClient, name string) {
	if len(room.Spectators) >= maxSpectators {
		h.sendError(client, spectatorFullMsg)
		return
	}
	if room.Spectators == nil {
		room.Spectators = map[*VGClient]bool{}
	}
	client.Name = name
	client.GameID = room.Game.ID
	room.Spectators[client] = true

	log.Printf("[라스베가스][관전] game=%s | %s 관전 입장 (%d명)",
		room.Game.ID, displayName(name), len(room.Spectators))

	h.sendToClient(client, VGMessage{
		Type:    VGMsgSpectateJoined,
		Payload: map[string]interface{}{"gameId": room.Game.ID, "roomCode": room.Code},
	})
	// 전원(관전자 포함)에게 관전자 수가 반영된 스냅샷 — 신규 관전자의 첫 스냅샷 겸용
	h.broadcastState(room)
}

// isSpectator 관전자 연결인지 (좌석 보유자·미입장 연결은 false)
func (h *VGHub) isSpectator(client *VGClient) bool {
	if client.GameID == "" {
		return false
	}
	room := h.rooms[client.GameID]
	return room != nil && room.Spectators[client]
}

// handleReact 리액션 이모지 — 좌석 보유자만, waiting 중에도 허용.
// 화이트리스트 외·레이트리밋 초과는 조용히 무시한다 (상태 저장 없음).
func (h *VGHub) handleReact(client *VGClient, msg VGMessage) {
	room := h.rooms[client.GameID]
	if room == nil || client.Seat < 0 {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload VGReactPayload
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
	h.broadcastEvent(room, VGEventPayload{Kind: "react", Seat: &seat, Name: client.Name, Message: payload.Emoji})
}

// waitingRoomOf 클라이언트가 속한 시작 전 방 (공용 로비 또는 사설 방)
func (h *VGHub) waitingRoomOf(client *VGClient) *vgRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Ready {
		return nil
	}
	return room
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *VGHub) hostSeat(room *vgRoom) int {
	return hostSeatOf(room.Clients)
}

// vgHumanCount 방의 사람 수
func vgHumanCount(room *vgRoom) int {
	return humanCountOf(room.Clients)
}

// updateLobbyWaiting 로비 현황판 갱신 — 사람 1명 이상 대기 && 미시작.
// 사설 방은 현황판에 노출하지 않는다 (초대 링크로만 접근).
func (h *VGHub) updateLobbyWaiting(room *vgRoom) {
	if room.Code != "" {
		return
	}
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && vgHumanCount(room) >= 1
	lobbySetWaiting("lasvegas", waiting)
}

// handleFillBots host 가 빈 좌석을 연습봇으로 4인까지 채운 뒤 즉시
// 시작한다 (스폰은 join 을 거치지 않으므로 명시 시작 호출).
func (h *VGHub) handleFillBots(client *VGClient) {
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
	for len(room.Game.Players) < VGFillBotTarget {
		botNo++
		if !h.spawnVGBot(room, fmt.Sprintf("%s%d", botName, botNo)) {
			break
		}
	}

	if room.Game.CanStart() {
		h.startGame(room)
		return
	}
	h.broadcastState(room)
}

func (h *VGHub) handleStart(client *VGClient) {
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

func (h *VGHub) startGame(room *vgRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	if room.Code != "" {
		// 시작한 사설 방의 코드는 진행 중 장부로 옮긴다 (관전 입장 근거)
		delete(h.privateLobbies, room.Code)
		h.activeCodes[room.Code] = room.Game.ID
	} else {
		h.lobby = nil // 시작한 방은 로비에서 떼어낸다
		lobbySetWaiting("lasvegas", false)
	}

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	log.Printf("[라스베가스][경기시작] game=%s | 인원=%d | 총 %d라운드 | 선공=seat%d | %v",
		room.Game.ID, len(room.Game.Players), VGTotalRounds, room.Game.FirstSeat, names)
	if !vgRoomHasBot(room) { // 연습봇전은 운영자 알림을 억제한다
		notify("라스베가스 게임 시작", fmt.Sprintf("%d인전 시작", len(room.Game.Players)))
	}

	first := room.Game.FirstSeat
	h.broadcastEvent(room, VGEventPayload{Kind: "game_started", Seat: &first,
		Name: room.Game.Players[first].Name,
		Message: fmt.Sprintf("게임 시작 — %s님 선공 (총 %d라운드, 각자 주사위 %d개)",
			room.Game.Players[first].Name, VGTotalRounds, VGDiceCount)})
	h.broadcastState(room)
}

// removeFromLobby 대기실에서 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *VGHub) removeFromLobby(room *vgRoom, client *VGClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	// 좌석 압축: oldSeat 뒤의 클라이언트를 한 칸씩 당긴다
	rebuilt := map[int]*VGClient{}
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

	log.Printf("[라스베가스][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), VGMaxPlayers)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if vgHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, VGMessage{Type: VGMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			lobbySetWaiting("lasvegas", false)
		}
		return
	}

	h.broadcastEvent(room, VGEventPayload{Kind: "left", Name: client.Name,
		Message: fmt.Sprintf("%s님이 퇴장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *VGHub) roomOf(client *VGClient) *vgRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

// handlePlace 배치 — 방금 굴린 주사위 중 고른 눈 전부를 그 카지노에 놓는다.
// 전원 소진이면 순수 규칙이 즉시 정산까지 마치므로 결과 발표·라운드 전환
// 타이머까지 한 번에 처리한다.
func (h *VGHub) handlePlace(client *VGClient, msg VGMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload VGPlacePayload
	json.Unmarshal(payloadBytes, &payload)

	game := room.Game
	res, err := game.Place(client.Seat, payload.Face, h.rng)
	if err != nil {
		h.sendError(client, err.Error())
		return
	}

	seat := res.Seat
	log.Printf("[라스베가스][배치] game=%s | %d라운드 | seat%d=%s | 눈%d ×%d (남은 %d개)",
		game.ID, game.Round, seat, displayName(client.Name), res.Face, res.Count,
		game.Players[seat].DiceLeft)
	h.broadcastEvent(room, VGEventPayload{Kind: "place", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 %d 카지노에 주사위 %d개를 배치했습니다",
			client.Name, res.Face, res.Count)})

	if res.RoundEnded {
		h.announceRoundEnd(room)
	}
	h.broadcastState(room)
	if res.RoundEnded {
		h.scheduleRoundEnd(room)
	}
}

// announceRoundEnd 라운드 정산 결과 이벤트 (스냅샷의 roundResult 와 병행)
func (h *VGHub) announceRoundEnd(room *vgRoom) {
	game := room.Game
	if game.RoundResult == nil {
		return
	}
	log.Printf("[라스베가스][라운드정산] game=%s | %d라운드 | %s",
		game.ID, game.Round, game.RoundResult.Message)
	h.broadcastEvent(room, VGEventPayload{Kind: "round_end",
		Message: game.RoundResult.Message})
}

// ==================== 자동 진행 타이머 ====================

// scheduleRoundEnd round_end 진입 시 다음 라운드 자동 진행 타이머를 건다.
// 발화는 허브 채널을 경유하므로 허브 밖에서 상태를 만지지 않는다.
func (h *VGHub) scheduleRoundEnd(room *vgRoom) {
	sig := vgRoundSignal{GameID: room.Game.ID, Round: room.Game.Round}
	room.RoundTimer = time.AfterFunc(vgRoundEndDelay, func() {
		h.roundFired <- sig
	})
}

func (h *VGHub) handleRoundFired(sig vgRoundSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.Game.Round != sig.Round || room.Game.Phase != VGPhaseRoundEnd {
		return
	}
	game := room.Game
	if err := game.NextRound(h.rng); err != nil {
		return
	}
	if game.Phase == VGPhaseGameOver {
		h.finishGame(room)
		return
	}

	first := game.CurrentSeat
	log.Printf("[라스베가스][라운드시작] game=%s | %d라운드 | 선공=seat%d",
		game.ID, game.Round, first)
	h.broadcastEvent(room, VGEventPayload{Kind: "round_started", Seat: &first,
		Name: game.Players[first].Name,
		Message: fmt.Sprintf("%d라운드 시작 — %s님부터 배치합니다",
			game.Round, game.Players[first].Name)})
	h.broadcastState(room)
}

// resetAfkTimer 상태가 바뀔 때마다 AFK 타이머를 다시 건다.
// 배치 응답을 기다리는 placing 단계에서만 동작한다.
func (h *VGHub) resetAfkTimer(room *vgRoom) {
	room.AfkSeq++
	if room.AfkTimer != nil {
		room.AfkTimer.Stop()
		room.AfkTimer = nil
	}
	room.EndsAt = 0
	if room.Game.Phase != VGPhasePlacing {
		return
	}
	room.EndsAt = time.Now().Add(vgAfkTimeout).UnixMilli()
	sig := vgAfkSignal{GameID: room.Game.ID, Seq: room.AfkSeq}
	room.AfkTimer = time.AfterFunc(vgAfkTimeout, func() {
		h.afkFired <- sig
	})
}

// handleAfkFired AFK 타이머 발화 — 현재 차례 좌석(사람)의 굴린 주사위 중
// 최다 눈을 자동 배치한다. 좌석은 유지된다.
func (h *VGHub) handleAfkFired(sig vgAfkSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.AfkSeq != sig.Seq || room.Game.Phase != VGPhasePlacing {
		return
	}
	game := room.Game
	seat := game.CurrentSeat
	client := room.Clients[seat]
	if client == nil || client.Bot {
		return // 봇 좌석은 스스로 행동한다
	}
	face := vgMostCommonFace(game.Dice)
	if face == 0 {
		return
	}

	log.Printf("[라스베가스][자동진행] game=%s | seat%d=%s 무응답 — 최다 눈(%d) 자동 배치",
		game.ID, seat, displayName(game.Players[seat].Name), face)
	h.broadcastEvent(room, VGEventPayload{Kind: "afk", Seat: &seat,
		Name: game.Players[seat].Name,
		Message: fmt.Sprintf("%s님이 오래 응답하지 않아 가장 많은 눈을 자동으로 배치합니다",
			game.Players[seat].Name)})
	h.handleGameMessage(VGGameMessage{Client: client, Message: VGMessage{
		Type: VGMsgPlace, Payload: VGPlacePayload{Face: face},
	}})
}

// ==================== 상태 뷰 ====================

// buildVGState 게임 스냅샷. 주사위·지폐·배치가 전원 공개라 은닉이 없다 —
// 개인화는 yourSeat 뿐이고 관전자(viewerSeat -1)도 같은 공개 정보를 받는다.
// dice/casinos/bills/placed 는 항상 배열·맵으로 나간다 (nil 금지).
func (h *VGHub) buildVGState(room *vgRoom, viewerSeat int) VGGameStatePayload {
	game := room.Game

	players := []VGPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		players = append(players, VGPlayerView{
			Seat:      p.Seat,
			Name:      p.Name,
			Connected: c != nil && c.Connected,
			Bot:       c != nil && c.Bot,
			Cash:      p.Cash,
			DiceLeft:  p.DiceLeft,
		})
	}

	casinos := []VGCasinoView{}
	for _, c := range game.Casinos {
		placed := map[int]int{}
		for seat, n := range c.Placed {
			placed[seat] = n
		}
		casinos = append(casinos, VGCasinoView{
			Face:   c.Face,
			Bills:  append([]int{}, c.Bills...),
			Placed: placed,
		})
	}

	endsAt := int64(0)
	if game.Phase == VGPhasePlacing {
		endsAt = room.EndsAt
	}

	return VGGameStatePayload{
		GameID:      game.ID,
		RoomCode:    room.Code,
		Phase:       game.Phase,
		HostSeat:    h.hostSeat(room),
		YourSeat:    viewerSeat,
		Spectators:  len(room.Spectators),
		EndsAt:      endsAt,
		Round:       game.Round,
		CurrentSeat: game.CurrentSeat,
		Dice:        append([]int{}, game.Dice...),
		Casinos:     casinos,
		Players:     players,
		RoundResult: game.RoundResult,
	}
}

// broadcastState 좌석마다 스냅샷을 만들어 보낸다. 관전자에게는
// viewerSeat -1 스냅샷이 간다. AFK 타이머를 먼저 리셋해야 스냅샷의
// endsAt 이 새 마감 시각을 싣는다.
func (h *VGHub) broadcastState(room *vgRoom) {
	h.resetAfkTimer(room)
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, VGMessage{
			Type:    VGMsgGameState,
			Payload: h.buildVGState(room, seat),
		})
	}
	if len(room.Spectators) > 0 {
		msg := VGMessage{Type: VGMsgGameState, Payload: h.buildVGState(room, -1)}
		for c := range room.Spectators {
			h.sendToClient(c, msg)
		}
	}
}

func (h *VGHub) broadcastEvent(room *vgRoom, event VGEventPayload) {
	h.broadcastToRoom(room, VGMessage{Type: VGMsgEvent, Payload: event})
}

// finishGame 게임 종료 발표·전적 기록·방 정리 (동점 공동 승 지원)
func (h *VGHub) finishGame(room *vgRoom) {
	game := room.Game
	if room.RoundTimer != nil {
		room.RoundTimer.Stop()
		room.RoundTimer = nil
	}
	if room.AfkTimer != nil {
		room.AfkTimer.Stop()
		room.AfkTimer = nil
	}

	totals := []int{}
	names := []string{}
	for _, p := range game.Players {
		totals = append(totals, p.Cash)
		names = append(names, displayName(p.Name))
	}
	winnerNames := []string{}
	winnerDisplay := []string{}
	for _, seat := range game.WinnerSeats {
		winnerNames = append(winnerNames, game.Players[seat].Name)
		winnerDisplay = append(winnerDisplay, displayName(game.Players[seat].Name))
	}

	message := ""
	if len(game.WinnerSeats) == 1 {
		message = fmt.Sprintf("%s님이 총 %d만 달러로 승리했습니다!",
			winnerNames[0], totals[game.WinnerSeats[0]])
	} else {
		message = fmt.Sprintf("%s님이 총 %d만 달러로 공동 승리했습니다!",
			strings.Join(winnerNames, "님, "), totals[game.WinnerSeats[0]])
	}
	first := game.WinnerSeats[0]
	h.broadcastEvent(room, VGEventPayload{Kind: "game_over", Seat: &first,
		Name: winnerNames[0], Message: message})
	// 최종 총액이 반영된 스냅샷을 먼저 보낸 뒤 종료를 알린다
	// (봇 러너는 vg_game_over 를 보고 스스로 끝난다)
	h.broadcastState(room)
	h.broadcastToRoom(room, VGMessage{
		Type: VGMsgGameOver,
		Payload: VGGameOverPayload{
			WinnerSeats: append([]int{}, game.WinnerSeats...),
			WinnerNames: winnerNames,
			Totals:      totals,
			Players:     h.buildVGState(room, -1).Players,
		},
	})

	log.Printf("[라스베가스][경기결과] game=%s | 승자=%v(%s) | 총액=%v | 소요=%s",
		game.ID, game.WinnerSeats, strings.Join(winnerDisplay, ","), totals, matchDuration(game.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "lasvegas",
		Players:  strings.Join(names, " vs "),
		Winner:   strings.Join(winnerDisplay, ", "),
		Reason:   "cash",
		Duration: matchSeconds(game.StartedAt),
		Bot:      vgRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
	if room.Code != "" {
		delete(h.activeCodes, room.Code) // 코드 재사용 복귀
	}
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *VGHub) handleDisconnect(client *VGClient) {
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
	log.Printf("[라스베가스][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, VGMessage{
		Type: VGMsgPlayerDisconnected,
		Payload: VGPlayerDisconnectedPayload{
			Seat:         client.Seat,
			Name:         client.Name,
			GraceSeconds: int(h.grace.Seconds()),
		},
	})
	h.broadcastState(room)

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 안에 재접속하지 않은 좌석은 연습봇으로 대체하고
// 게임은 계속한다 (세션은 폐기 — 재접속 불가)
func (h *VGHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == VGPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[라스베가스][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := &VGClient{wsClient: newBotWSClient(), Hub: h, Seat: seat}
	bot.Name = client.Name // 좌석 이름은 유지 (표시는 bot 플래그로 구분)
	bot.GameID = room.Game.ID
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runVGBot(bot)

	h.broadcastEvent(room, VGEventPayload{Kind: "bot_takeover", Seat: &seat,
		Name:    client.Name,
		Message: fmt.Sprintf("%s님 자리를 봇이 이어받았습니다", client.Name)})
	// 새 봇이 이 스냅샷을 받아 자기 차례면 즉시 이어간다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *VGHub) handleRejoin(client *VGClient, msg VGMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload VGRejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, VGMessage{Type: VGMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, VGMessage{Type: VGMsgSessionExpired})
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

	log.Printf("[라스베가스][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, VGMessage{
		Type:    VGMsgPlayerReconnected,
		Payload: VGPlayerReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	// 전원에게 접속 상태가 반영된 스냅샷 (재접속 당사자 복원 포함)
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *VGHub) clearGameSessions(room *vgRoom) {
	clearRoomSessions(&h.sessionManager, room.Clients)
}

// ==================== 전송 ====================

func (h *VGHub) sendError(client *VGClient, message string) {
	h.sendToClient(client, VGMessage{Type: VGMsgError, Payload: VGErrorPayload{Message: message}})
}

func (h *VGHub) sendToClient(client *VGClient, message VGMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[VG] ")
}

func (h *VGHub) broadcastToRoom(room *vgRoom, message VGMessage) {
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

func ServeVGWs(hub *VGHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[VG] Error upgrading connection:", err)
		return
	}

	client := &VGClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[VG] ",
		func(msg VGMessage) { hub.gameMessage <- VGGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
