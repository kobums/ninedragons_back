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

// ytAfkTimeout 접속 유지 AFK 의 자동 진행 대기 시간 — 발화하면 현재 턴
// 좌석의 남은 굴림을 무시하고 봇 두뇌로 점수를 기록한다 (테스트에서 짧게 낮춘다).
var ytAfkTimeout = 45 * time.Second

// ytRoom 게임(순수 상태)과 좌석별 연결의 매핑
type ytRoom struct {
	Game    *YTGame
	Clients map[int]*YTClient // seat → client

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string

	// AfkTimer 접속 유지 AFK 구제 타이머 — 상태가 바뀔 때마다 리셋되고,
	// 발화하면 현재 턴 좌석의 수를 봇 두뇌로 마무리한다.
	AfkTimer *time.Timer
	// AfkSeq 상태 변경 일련번호 (뒤늦은 발화 무시용)
	AfkSeq int
	// EndsAt 현재 턴의 AFK 마감 시각 (unixMillis) — 스냅샷 노출용
	EndsAt int64

	// Spectators 관전자 연결 (사설 방 코드로만 진입, 상한 maxSpectators).
	// 좌석·세션이 없어 재접속을 지원하지 않는다 — 끊기면 목록에서만 뗀다.
	Spectators map[*YTClient]bool

	// LastReact 좌석별 마지막 리액션 발신 시각 (레이트리밋 장부)
	LastReact map[int]time.Time
}

// ytAfkSignal AFK 타이머 발화 표식
type ytAfkSignal struct {
	GameID string
	Seq    int
}

type YTHub struct {
	// 등록된 클라이언트
	clients map[*YTClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*ytRoom

	// 글로벌 로비. 시작 전 공용 방은 하나뿐이다.
	lobby *ytRoom

	// 사설 방 (초대 코드 → 시작 전 방). 허브 고루틴에서만 접근한다.
	// 시작하면 privateLobbies 에서 activeCodes 로 옮긴다.
	privateLobbies map[string]*ytRoom

	// 진행 중 사설 방 (초대 코드 → gameID). 시작 후에도 코드로 방을 찾아
	// 관전 입장시키는 근거다. finishGame 에서 해제한다 (코드 재사용 복귀).
	activeCodes map[string]string

	// 클라이언트 등록
	register chan *YTClient

	// 클라이언트 등록 해제
	unregister chan *YTClient

	// 게임 메시지
	gameMessage chan YTGameMessage

	// AFK 자동 진행 알림 (time.AfterFunc → 허브 채널 경유)
	afkFired chan ytAfkSignal

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*YTClient]

	// 주사위·선공 결정용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type YTGameMessage struct {
	Client  *YTClient
	Message YTMessage
}

func NewYTHub() *YTHub {
	return &YTHub{
		register:       make(chan *YTClient),
		unregister:     make(chan *YTClient),
		clients:        make(map[*YTClient]bool),
		rooms:          make(map[string]*ytRoom),
		privateLobbies: make(map[string]*ytRoom),
		activeCodes:    make(map[string]string),
		gameMessage:    make(chan YTGameMessage),
		afkFired:       make(chan ytAfkSignal, 8),
		sessionManager: newSessionManager[*YTClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *YTHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[YT] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[YT] Client unregistered: %s", client.ID)
			}

		case sessionID := <-h.graceExpired:
			h.handleGraceExpired(sessionID)

		case sig := <-h.afkFired:
			h.handleAfkFired(sig)

		case message := <-h.gameMessage:
			h.handleGameMessage(message)
		}
	}
}

func (h *YTHub) handleGameMessage(gm YTGameMessage) {
	// 관전자는 어떤 행동도 할 수 없다 (보기 전용 — 리액션도 좌석 보유자만)
	if h.isSpectator(gm.Client) {
		h.sendError(gm.Client, spectatorDeniedMsg)
		return
	}
	switch gm.Message.Type {
	case YTMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case YTMsgReact:
		h.handleReact(gm.Client, gm.Message)
	case YTMsgFillBots:
		h.handleFillBots(gm.Client)
	case YTMsgStart:
		h.handleStart(gm.Client)
	case YTMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case YTMsgRoll:
		h.handleRoll(gm.Client, gm.Message)
	case YTMsgScore:
		h.handleScore(gm.Client, gm.Message)
	}
}

// ==================== 대기실 ====================

func (h *YTHub) handleJoinGame(client *YTClient, msg YTMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload YTJoinGamePayload
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

	log.Printf("[요트][입장] game=%s | seat%d=%s 대기 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), YTMaxPlayers)
	if room.Code == "" { // 사설 방 입장은 조용히 (공용 로비만 알림)
		notify("요트 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), len(room.Game.Players), YTMaxPlayers))
	}

	h.sendToClient(client, YTMessage{
		Type: YTMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
			"roomCode":  room.Code,
		},
	})

	h.broadcastEvent(room, YTEventPayload{Kind: "joined", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 입장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	// 4인이 차도 자동 시작하지 않는다 — 호스트 명시 시작만 (봇 채우기와 충돌 방지)
	h.broadcastState(room)
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다.
// 생략/빈 문자열은 기존 공용 로비 경로 그대로, "NEW"는 새 코드 발급,
// 그 외 코드는 해당 사설 방 (없으면 그 코드로 관대하게 새로 생성).
func (h *YTHub) lobbyRoomFor(roomField string) *ytRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewYTGame(uuid.New().String())
			h.lobby = &ytRoom{Game: game, Clients: map[int]*YTClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[YT] Created lobby game %s", game.ID)
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
		game := NewYTGame(uuid.New().String())
		room = &ytRoom{Game: game, Clients: map[int]*YTClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[YT] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// ==================== 관전 / 리액션 ====================

// addSpectator 진행 중(또는 가득 찬) 사설 방에 관전자로 등록한다.
// 좌석·세션 없음 — 재접속 미지원, 끊기면 다시 코드로 들어온다.
func (h *YTHub) addSpectator(room *ytRoom, client *YTClient, name string) {
	if len(room.Spectators) >= maxSpectators {
		h.sendError(client, spectatorFullMsg)
		return
	}
	if room.Spectators == nil {
		room.Spectators = map[*YTClient]bool{}
	}
	client.Name = name
	client.GameID = room.Game.ID
	room.Spectators[client] = true

	log.Printf("[요트][관전] game=%s | %s 관전 입장 (%d명)",
		room.Game.ID, displayName(name), len(room.Spectators))

	h.sendToClient(client, YTMessage{
		Type:    YTMsgSpectateJoined,
		Payload: map[string]interface{}{"gameId": room.Game.ID, "roomCode": room.Code},
	})
	// 전원(관전자 포함)에게 관전자 수가 반영된 스냅샷 — 신규 관전자의 첫 스냅샷 겸용
	h.broadcastState(room)
}

// isSpectator 관전자 연결인지 (좌석 보유자·미입장 연결은 false)
func (h *YTHub) isSpectator(client *YTClient) bool {
	if client.GameID == "" {
		return false
	}
	room := h.rooms[client.GameID]
	return room != nil && room.Spectators[client]
}

// handleReact 리액션 이모지 — 좌석 보유자만, waiting 중에도 허용.
// 화이트리스트 외·레이트리밋 초과는 조용히 무시한다 (상태 저장 없음).
func (h *YTHub) handleReact(client *YTClient, msg YTMessage) {
	room := h.rooms[client.GameID]
	if room == nil || client.Seat < 0 {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload YTReactPayload
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
	h.broadcastEvent(room, YTEventPayload{Kind: "react", Seat: &seat, Name: client.Name, Message: payload.Emoji})
}

// waitingRoomOf 클라이언트가 속한 시작 전 방 (공용 로비 또는 사설 방)
func (h *YTHub) waitingRoomOf(client *YTClient) *ytRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Ready {
		return nil
	}
	return room
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *YTHub) hostSeat(room *ytRoom) int {
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

// ytHumanCount 방의 사람 수
func ytHumanCount(room *ytRoom) int {
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
func (h *YTHub) updateLobbyWaiting(room *ytRoom) {
	if room.Code != "" {
		return
	}
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && ytHumanCount(room) >= 1
	lobbySetWaiting("yacht", waiting)
}

// handleFillBots host 가 빈 좌석 전부(4인까지)를 연습봇으로 채운 뒤 즉시
// 시작한다 (스폰은 join 을 거치지 않으므로 명시 시작 호출).
func (h *YTHub) handleFillBots(client *YTClient) {
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
	for len(room.Game.Players) < YTMaxPlayers {
		botNo++
		if !h.spawnYTBot(room, fmt.Sprintf("%s%d", botName, botNo)) {
			break
		}
	}

	if len(room.Game.Players) == YTMaxPlayers {
		h.startGame(room)
		return
	}
	h.broadcastState(room)
}

func (h *YTHub) handleStart(client *YTClient) {
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

func (h *YTHub) startGame(room *ytRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	if room.Code != "" {
		// 시작한 사설 방의 코드는 진행 중 장부로 옮긴다 (관전 입장 근거)
		delete(h.privateLobbies, room.Code)
		h.activeCodes[room.Code] = room.Game.ID
	} else {
		h.lobby = nil // 시작한 방은 로비에서 떼어낸다
		lobbySetWaiting("yacht", false)
	}

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	log.Printf("[요트][경기시작] game=%s | 인원=%d | 총 %d라운드 | 선공=seat%d | %v",
		room.Game.ID, len(room.Game.Players), ytTotalRounds(), room.Game.CurrentSeat, names)
	if !ytRoomHasBot(room) { // 연습봇전은 운영자 알림을 억제한다
		notify("요트 게임 시작", fmt.Sprintf("%d인전 시작", len(room.Game.Players)))
	}

	first := room.Game.CurrentSeat
	h.broadcastEvent(room, YTEventPayload{Kind: "game_started", Seat: &first,
		Name:    room.Game.Players[first].Name,
		Message: fmt.Sprintf("게임 시작 — %s님 선공", room.Game.Players[first].Name)})
	h.broadcastState(room)
}

// removeFromLobby 대기실에서 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *YTHub) removeFromLobby(room *ytRoom, client *YTClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	// 좌석 압축: oldSeat 뒤의 클라이언트를 한 칸씩 당긴다
	rebuilt := map[int]*YTClient{}
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

	log.Printf("[요트][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), YTMaxPlayers)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if ytHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, YTMessage{Type: YTMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			lobbySetWaiting("yacht", false)
		}
		return
	}

	h.broadcastEvent(room, YTEventPayload{Kind: "left", Name: client.Name,
		Message: fmt.Sprintf("%s님이 퇴장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *YTHub) roomOf(client *YTClient) *ytRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

// handleRoll 주사위 굴리기 — 결과는 전원 공개 이벤트·스냅샷으로 나간다
func (h *YTHub) handleRoll(client *YTClient, msg YTMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload YTRollPayload
	json.Unmarshal(payloadBytes, &payload)

	game := room.Game
	if err := game.Roll(client.Seat, h.rng, payload.Hold); err != nil {
		h.sendError(client, err.Error())
		return
	}

	seat := client.Seat
	log.Printf("[요트][굴림] game=%s | %d라운드 | seat%d=%s | %s (남은 %d회)",
		game.ID, game.Round, seat, displayName(client.Name), ytDiceString(game.Dice), game.RollsLeft)
	h.broadcastEvent(room, YTEventPayload{Kind: "roll", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 주사위를 굴렸습니다 — %s (남은 굴림 %d회)",
			client.Name, ytDiceString(game.Dice), game.RollsLeft)})
	h.broadcastState(room)
}

// handleScore 점수 기록 — 빈 칸 하나에 기록하고 턴을 넘긴다
func (h *YTHub) handleScore(client *YTClient, msg YTMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload YTScorePayload
	json.Unmarshal(payloadBytes, &payload)

	game := room.Game
	res, err := game.Score(client.Seat, payload.Category)
	if err != nil {
		h.sendError(client, err.Error())
		return
	}

	seat := res.Seat
	log.Printf("[요트][기록] game=%s | %d라운드 | seat%d=%s | %s=%d점",
		game.ID, game.Round, seat, displayName(client.Name), res.Category, res.Points)
	h.broadcastEvent(room, YTEventPayload{Kind: "score", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 %s에 %d점을 기록했습니다",
			client.Name, ytCategoryName(res.Category), res.Points)})

	h.broadcastState(room)
	if res.GameOver {
		h.finishGame(room)
	}
}

// ==================== AFK 자동 진행 ====================

// resetAfkTimer 상태가 바뀔 때마다 AFK 타이머를 다시 건다.
// 턴 응답을 기다리는 playing 단계에서만 동작한다.
func (h *YTHub) resetAfkTimer(room *ytRoom) {
	room.AfkSeq++
	if room.AfkTimer != nil {
		room.AfkTimer.Stop()
		room.AfkTimer = nil
	}
	room.EndsAt = 0
	if room.Game.Phase != YTPhasePlaying {
		return
	}
	room.EndsAt = time.Now().Add(ytAfkTimeout).UnixMilli()
	sig := ytAfkSignal{GameID: room.Game.ID, Seq: room.AfkSeq}
	room.AfkTimer = time.AfterFunc(ytAfkTimeout, func() {
		h.afkFired <- sig
	})
}

// handleAfkFired AFK 타이머 발화 — 현재 턴 좌석(사람)의 남은 굴림을 무시하고
// 봇 두뇌로 점수를 기록한다. 아직 한 번도 안 굴렸으면 1회만 굴리고 기록한다.
// 좌석은 유지된다.
func (h *YTHub) handleAfkFired(sig ytAfkSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.AfkSeq != sig.Seq || room.Game.Phase != YTPhasePlaying {
		return
	}
	game := room.Game
	seat := game.CurrentSeat
	client := room.Clients[seat]
	if client == nil || client.Bot {
		return // 봇 좌석은 스스로 행동한다
	}

	log.Printf("[요트][자동진행] game=%s | seat%d=%s 무응답 — 남은 굴림 무시하고 기록",
		game.ID, seat, displayName(game.Players[seat].Name))
	h.broadcastEvent(room, YTEventPayload{Kind: "afk", Seat: &seat,
		Name:    game.Players[seat].Name,
		Message: fmt.Sprintf("%s님이 오래 응답하지 않아 자동으로 진행합니다", game.Players[seat].Name)})

	// 아직 굴리지 않은 턴이면 첫 굴림만 대신한다 (기록에는 주사위가 필요)
	if game.RollsLeft == YTMaxRolls {
		h.handleGameMessage(YTGameMessage{Client: client, Message: YTMessage{
			Type: YTMsgRoll, Payload: YTRollPayload{Hold: make([]bool, YTDiceCount)},
		}})
	}
	if game.Phase != YTPhasePlaying || game.CurrentSeat != seat {
		return
	}
	category, _ := ytBestCategory(game.Dice, game.Players[seat].Scores)
	if category == "" {
		return
	}
	h.handleGameMessage(YTGameMessage{Client: client, Message: YTMessage{
		Type: YTMsgScore, Payload: YTScorePayload{Category: category},
	}})
}

// ==================== 상태 뷰 ====================

// buildYTState 게임 스냅샷. 주사위·점수표가 전원 공개라 은닉이 없다 —
// 개인화는 yourSeat 뿐이고 관전자(viewerSeat -1)도 같은 공개 정보를 받는다.
// dice/held/scores/winnerSeats 는 항상 배열·맵으로 나간다 (nil 금지).
func (h *YTHub) buildYTState(room *ytRoom, viewerSeat int) YTGameStatePayload {
	game := room.Game

	players := []YTPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		scores := map[string]int{}
		for cat, v := range p.Scores {
			scores[cat] = v
		}
		players = append(players, YTPlayerView{
			Seat:      p.Seat,
			Name:      p.Name,
			Connected: c != nil && c.Connected,
			Bot:       c != nil && c.Bot,
			Total:     ytTotal(p.Scores),
			UpperSum:  ytUpperSum(p.Scores),
			Bonus:     ytBonus(p.Scores),
			Scores:    scores,
		})
	}

	endsAt := int64(0)
	if game.Phase == YTPhasePlaying {
		endsAt = room.EndsAt
	}

	return YTGameStatePayload{
		GameID:      game.ID,
		RoomCode:    room.Code,
		Phase:       game.Phase,
		HostSeat:    h.hostSeat(room),
		YourSeat:    viewerSeat,
		Spectators:  len(room.Spectators),
		EndsAt:      endsAt,
		CurrentSeat: game.CurrentSeat,
		Round:       game.Round,
		Dice:        append([]int{}, game.Dice...),
		Held:        append([]bool{}, game.Held...),
		RollsLeft:   game.RollsLeft,
		Players:     players,
		LastAction:  game.LastAction,
		WinnerSeats: append([]int{}, game.WinnerSeats...),
	}
}

// broadcastState 좌석마다 스냅샷을 만들어 보낸다. 관전자에게는
// viewerSeat -1 스냅샷이 간다. AFK 타이머를 먼저 리셋해야 스냅샷의
// endsAt 이 새 마감 시각을 싣는다.
func (h *YTHub) broadcastState(room *ytRoom) {
	h.resetAfkTimer(room)
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, YTMessage{
			Type:    YTMsgGameState,
			Payload: h.buildYTState(room, seat),
		})
	}
	if len(room.Spectators) > 0 {
		msg := YTMessage{Type: YTMsgGameState, Payload: h.buildYTState(room, -1)}
		for c := range room.Spectators {
			h.sendToClient(c, msg)
		}
	}
}

func (h *YTHub) broadcastEvent(room *ytRoom, event YTEventPayload) {
	h.broadcastToRoom(room, YTMessage{Type: YTMsgEvent, Payload: event})
}

// finishGame 게임 종료 발표·전적 기록·방 정리 (동점 공동 승 지원)
func (h *YTHub) finishGame(room *ytRoom) {
	game := room.Game
	if room.AfkTimer != nil {
		room.AfkTimer.Stop()
		room.AfkTimer = nil
	}

	totals := []int{}
	names := []string{}
	for _, p := range game.Players {
		totals = append(totals, ytTotal(p.Scores))
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
		message = fmt.Sprintf("%s님이 %d점으로 승리했습니다!",
			winnerNames[0], totals[game.WinnerSeats[0]])
	} else {
		message = fmt.Sprintf("%s님이 %d점으로 공동 승리했습니다!",
			strings.Join(winnerNames, "님, "), totals[game.WinnerSeats[0]])
	}
	first := game.WinnerSeats[0]
	h.broadcastEvent(room, YTEventPayload{Kind: "game_over", Seat: &first,
		Name: winnerNames[0], Message: message})
	// 최종 점수가 반영된 스냅샷을 먼저 보낸 뒤 종료를 알린다
	// (봇 러너는 yt_game_over 를 보고 스스로 끝난다)
	h.broadcastState(room)
	h.broadcastToRoom(room, YTMessage{
		Type: YTMsgGameOver,
		Payload: YTGameOverPayload{
			WinnerSeats: append([]int{}, game.WinnerSeats...),
			WinnerNames: winnerNames,
			Totals:      totals,
			Players:     h.buildYTState(room, -1).Players,
		},
	})

	log.Printf("[요트][경기결과] game=%s | 승자=%v(%s) | 총점=%v | 소요=%s",
		game.ID, game.WinnerSeats, strings.Join(winnerDisplay, ","), totals, matchDuration(game.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "yacht",
		Players:  strings.Join(names, " vs "),
		Winner:   strings.Join(winnerDisplay, ", "),
		Reason:   "score",
		Duration: matchSeconds(game.StartedAt),
		Bot:      ytRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
	if room.Code != "" {
		delete(h.activeCodes, room.Code) // 코드 재사용 복귀
	}
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *YTHub) handleDisconnect(client *YTClient) {
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
	log.Printf("[요트][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, YTMessage{
		Type: YTMsgPlayerDisconnected,
		Payload: YTPlayerDisconnectedPayload{
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
func (h *YTHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == YTPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[요트][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := &YTClient{wsClient: newBotWSClient(), Hub: h, Seat: seat}
	bot.Name = client.Name // 좌석 이름은 유지 (표시는 bot 플래그로 구분)
	bot.GameID = room.Game.ID
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runYTBot(bot)

	h.broadcastEvent(room, YTEventPayload{Kind: "bot_takeover", Seat: &seat,
		Name:    client.Name,
		Message: fmt.Sprintf("%s님 자리를 봇이 이어받았습니다", client.Name)})
	// 새 봇이 이 스냅샷을 받아 자기 턴이면 즉시 이어간다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *YTHub) handleRejoin(client *YTClient, msg YTMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload YTRejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, YTMessage{Type: YTMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, YTMessage{Type: YTMsgSessionExpired})
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

	log.Printf("[요트][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, YTMessage{
		Type:    YTMsgPlayerReconnected,
		Payload: YTPlayerReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	// 전원에게 접속 상태가 반영된 스냅샷 (재접속 당사자 복원 포함)
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *YTHub) clearGameSessions(room *ytRoom) {
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		h.drop(c.SessionID)
	}
}

// ==================== 전송 ====================

func (h *YTHub) sendError(client *YTClient, message string) {
	h.sendToClient(client, YTMessage{Type: YTMsgError, Payload: YTErrorPayload{Message: message}})
}

func (h *YTHub) sendToClient(client *YTClient, message YTMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[YT] ")
}

func (h *YTHub) broadcastToRoom(room *ytRoom, message YTMessage) {
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

func ServeYTWs(hub *YTHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[YT] Error upgrading connection:", err)
		return
	}

	client := &YTClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[YT] ",
		func(msg YTMessage) { hub.gameMessage <- YTGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
