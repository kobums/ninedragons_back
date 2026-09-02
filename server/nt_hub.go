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

// ntAfkTimeout 접속 유지 AFK 의 자동 진행 대기 시간 — 발화하면 현재 차례
// 좌석을 자동 처리한다 (칩 있으면 패스, 없으면 가져가기).
// (테스트에서 짧게 낮춘다)
var ntAfkTimeout = 30 * time.Second

// ntRoom 게임(순수 상태)과 좌석별 연결의 매핑
type ntRoom struct {
	Game    *NTGame
	Clients map[int]*NTClient // seat → client

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string

	// AfkTimer 접속 유지 AFK 구제 타이머 — 상태가 바뀔 때마다 리셋되고,
	// 발화하면 현재 차례 좌석을 자동 진행 처리한다.
	AfkTimer *time.Timer
	// AfkSeq 상태 변경 일련번호 (뒤늦은 발화 무시용)
	AfkSeq int
	// EndsAt 현재 차례의 AFK 마감 시각 (unixMillis) — 스냅샷 노출용
	EndsAt int64

	// Spectators 관전자 연결 (사설 방 코드로만 진입, 상한 maxSpectators).
	// 좌석·세션이 없어 재접속을 지원하지 않는다 — 끊기면 목록에서만 뗀다.
	Spectators map[*NTClient]bool

	// LastReact 좌석별 마지막 리액션 발신 시각 (레이트리밋 장부)
	LastReact map[int]time.Time
}

// ntAfkSignal AFK 타이머 발화 표식
type ntAfkSignal struct {
	GameID string
	Seq    int
}

type NTHub struct {
	// 등록된 클라이언트
	clients map[*NTClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*ntRoom

	// 글로벌 로비. 시작 전 공용 방은 하나뿐이다.
	lobby *ntRoom

	// 사설 방 (초대 코드 → 시작 전 방). 허브 고루틴에서만 접근한다.
	// 시작하면 privateLobbies 에서 activeCodes 로 옮긴다.
	privateLobbies map[string]*ntRoom

	// 진행 중 사설 방 (초대 코드 → gameID). 시작 후에도 코드로 방을 찾아
	// 관전 입장시키는 근거다. finishGame 에서 해제한다 (코드 재사용 복귀).
	activeCodes map[string]string

	// 클라이언트 등록
	register chan *NTClient

	// 클라이언트 등록 해제
	unregister chan *NTClient

	// 게임 메시지
	gameMessage chan NTGameMessage

	// 자동 진행 알림 (time.AfterFunc → 허브 채널 경유)
	afkFired chan ntAfkSignal

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*NTClient]

	// 셔플·선 결정용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type NTGameMessage struct {
	Client  *NTClient
	Message NTMessage
}

func NewNTHub() *NTHub {
	return &NTHub{
		register:       make(chan *NTClient),
		unregister:     make(chan *NTClient),
		clients:        make(map[*NTClient]bool),
		rooms:          make(map[string]*ntRoom),
		privateLobbies: make(map[string]*ntRoom),
		activeCodes:    make(map[string]string),
		gameMessage:    make(chan NTGameMessage),
		afkFired:       make(chan ntAfkSignal, 8),
		sessionManager: newSessionManager[*NTClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *NTHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[NT] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[NT] Client unregistered: %s", client.ID)
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

func (h *NTHub) handleGameMessage(gm NTGameMessage) {
	// 관전자는 어떤 행동도 할 수 없다 (보기 전용 — 리액션도 좌석 보유자만)
	if h.isSpectator(gm.Client) {
		h.sendError(gm.Client, spectatorDeniedMsg)
		return
	}
	switch gm.Message.Type {
	case NTMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case NTMsgReact:
		h.handleReact(gm.Client, gm.Message)
	case NTMsgFillBots:
		h.handleFillBots(gm.Client)
	case NTMsgStart:
		h.handleStart(gm.Client)
	case NTMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case NTMsgPass:
		h.handleAction(gm.Client, "pass")
	case NTMsgTake:
		h.handleAction(gm.Client, "take")
	}
}

// ==================== 대기실 ====================

func (h *NTHub) handleJoinGame(client *NTClient, msg NTMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload NTJoinGamePayload
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

	log.Printf("[노땡스][입장] game=%s | seat%d=%s 대기 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), NTMaxPlayers)
	if room.Code == "" { // 사설 방 입장은 조용히 (공용 로비만 알림)
		notify("노 땡스! 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), len(room.Game.Players), NTMaxPlayers))
	}

	h.sendToClient(client, NTMessage{
		Type: NTMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
			"roomCode":  room.Code,
		},
	})

	h.broadcastEvent(room, NTEventPayload{Kind: "joined", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 입장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	// 정원이 차도 자동 시작하지 않는다 — 호스트 명시 시작만 (봇 채우기와 충돌 방지)
	h.broadcastState(room)
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다.
// 생략/빈 문자열은 기존 공용 로비 경로 그대로, "NEW"는 새 코드 발급,
// 그 외 코드는 해당 사설 방 (없으면 그 코드로 관대하게 새로 생성).
func (h *NTHub) lobbyRoomFor(roomField string) *ntRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewNTGame(uuid.New().String())
			h.lobby = &ntRoom{Game: game, Clients: map[int]*NTClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[NT] Created lobby game %s", game.ID)
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
		game := NewNTGame(uuid.New().String())
		room = &ntRoom{Game: game, Clients: map[int]*NTClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[NT] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// ==================== 관전 / 리액션 ====================

// addSpectator 진행 중(또는 가득 찬) 사설 방에 관전자로 등록한다.
// 좌석·세션 없음 — 재접속 미지원, 끊기면 다시 코드로 들어온다.
func (h *NTHub) addSpectator(room *ntRoom, client *NTClient, name string) {
	if len(room.Spectators) >= maxSpectators {
		h.sendError(client, spectatorFullMsg)
		return
	}
	if room.Spectators == nil {
		room.Spectators = map[*NTClient]bool{}
	}
	client.Name = name
	client.GameID = room.Game.ID
	room.Spectators[client] = true

	log.Printf("[노땡스][관전] game=%s | %s 관전 입장 (%d명)",
		room.Game.ID, displayName(name), len(room.Spectators))

	h.sendToClient(client, NTMessage{
		Type:    NTMsgSpectateJoined,
		Payload: map[string]interface{}{"gameId": room.Game.ID, "roomCode": room.Code},
	})
	// 전원(관전자 포함)에게 관전자 수가 반영된 스냅샷 — 신규 관전자의 첫 스냅샷 겸용
	h.broadcastState(room)
}

// isSpectator 관전자 연결인지 (좌석 보유자·미입장 연결은 false)
func (h *NTHub) isSpectator(client *NTClient) bool {
	if client.GameID == "" {
		return false
	}
	room := h.rooms[client.GameID]
	return room != nil && room.Spectators[client]
}

// handleReact 리액션 이모지 — 좌석 보유자만, waiting 중에도 허용.
// 화이트리스트 외·레이트리밋 초과는 조용히 무시한다 (상태 저장 없음).
func (h *NTHub) handleReact(client *NTClient, msg NTMessage) {
	room := h.rooms[client.GameID]
	if room == nil || client.Seat < 0 {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload NTReactPayload
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
	h.broadcastEvent(room, NTEventPayload{Kind: "react", Seat: &seat, Name: client.Name, Message: payload.Emoji})
}

// waitingRoomOf 클라이언트가 속한 시작 전 방 (공용 로비 또는 사설 방)
func (h *NTHub) waitingRoomOf(client *NTClient) *ntRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Ready {
		return nil
	}
	return room
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *NTHub) hostSeat(room *ntRoom) int {
	return hostSeatOf(room.Clients)
}

// ntHumanCount 방의 사람 수
func ntHumanCount(room *ntRoom) int {
	return humanCountOf(room.Clients)
}

// updateLobbyWaiting 로비 현황판 갱신 — 사람 1명 이상 대기 && 미시작.
// 사설 방은 현황판에 노출하지 않는다 (초대 링크로만 접근).
func (h *NTHub) updateLobbyWaiting(room *ntRoom) {
	if room.Code != "" {
		return
	}
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && ntHumanCount(room) >= 1
	lobbySetWaiting("nothanks", waiting)
}

// handleFillBots host 가 빈 좌석을 연습봇으로 5인까지 채운 뒤 즉시
// 시작한다 (스폰은 join 을 거치지 않으므로 명시 시작 호출).
func (h *NTHub) handleFillBots(client *NTClient) {
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
	for len(room.Game.Players) < NTFillBotTarget {
		botNo++
		if !h.spawnNTBot(room, fmt.Sprintf("%s%d", botName, botNo)) {
			break
		}
	}

	if room.Game.CanStart() {
		h.startGame(room)
		return
	}
	h.broadcastState(room)
}

func (h *NTHub) handleStart(client *NTClient) {
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
		h.sendError(client, "3명 이상 모여야 시작할 수 있습니다")
		return
	}
	h.startGame(room)
}

func (h *NTHub) startGame(room *ntRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	if room.Code != "" {
		// 시작한 사설 방의 코드는 진행 중 장부로 옮긴다 (관전 입장 근거)
		delete(h.privateLobbies, room.Code)
		h.activeCodes[room.Code] = room.Game.ID
	} else {
		h.lobby = nil // 시작한 방은 로비에서 떼어낸다
		lobbySetWaiting("nothanks", false)
	}

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	log.Printf("[노땡스][경기시작] game=%s | 인원=%d | 덱=%d장 | 선=seat%d | %v",
		room.Game.ID, len(room.Game.Players), len(room.Game.Deck)+1, room.Game.FirstSeat, names)
	if !ntRoomHasBot(room) { // 연습봇전은 운영자 알림을 억제한다
		notify("노 땡스! 게임 시작", fmt.Sprintf("%d인전 시작", len(room.Game.Players)))
	}

	first := room.Game.FirstSeat
	h.broadcastEvent(room, NTEventPayload{Kind: "game_started", Seat: &first,
		Name: room.Game.Players[first].Name,
		Message: fmt.Sprintf("게임 시작 — %s님 선 (덱 %d장, 시작 칩 %d개)",
			room.Game.Players[first].Name, NTDeckSize, NTStartChips)})
	h.broadcastState(room)
}

// removeFromLobby 대기실에서 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *NTHub) removeFromLobby(room *ntRoom, client *NTClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	// 좌석 압축: oldSeat 뒤의 클라이언트를 한 칸씩 당긴다
	rebuilt := map[int]*NTClient{}
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

	log.Printf("[노땡스][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), NTMaxPlayers)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if ntHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, NTMessage{Type: NTMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			lobbySetWaiting("nothanks", false)
		}
		return
	}

	h.broadcastEvent(room, NTEventPayload{Kind: "left", Name: client.Name,
		Message: fmt.Sprintf("%s님이 퇴장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *NTHub) roomOf(client *NTClient) *ntRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

// handleAction 패스/가져가기 공통 경로 — 순수 규칙 판정 후 이벤트·스냅샷·
// 종료 처리까지 한 번에 처리한다.
func (h *NTHub) handleAction(client *NTClient, action string) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	game := room.Game

	var res *NTActionResult
	var err error
	switch action {
	case "pass":
		res, err = game.Pass(client.Seat)
	case "take":
		res, err = game.Take(client.Seat)
	}
	if err != nil {
		h.sendError(client, err.Error())
		return
	}

	log.Printf("[노땡스][행동] game=%s | seat%d=%s %s (카드 %d, 얹힌 칩 %d, 남은 덱 %d)",
		game.ID, res.Seat, displayName(client.Name), res.Kind, res.Card, game.PotChips, len(game.Deck))

	h.announceAction(room, client, res)
	if res.GameEnded {
		h.finishGame(room)
		return
	}
	h.broadcastState(room)
}

// announceAction 행동을 공개 이벤트로 번역한다 (비밀 정보 없음 — 얹힌 칩
// 수는 공개 정보다)
func (h *NTHub) announceAction(room *ntRoom, client *NTClient, res *NTActionResult) {
	seat := res.Seat
	name := client.Name
	message := ""
	switch res.Kind {
	case "pass":
		message = fmt.Sprintf("%s님이 칩 1개를 얹고 패스했습니다 (얹힌 칩 %d개)",
			name, room.Game.PotChips)
	case "take":
		message = fmt.Sprintf("%s님이 카드 %d(칩 %d개 포함)을 가져갔습니다",
			name, res.Card, res.GainedChips)
	}
	h.broadcastEvent(room, NTEventPayload{Kind: res.Kind, Seat: &seat, Name: name, Message: message})
}

// ==================== 자동 진행 타이머 (AFK) ====================

// resetAfkTimer 상태가 바뀔 때마다 AFK 타이머를 다시 건다.
// 행동 응답을 기다리는 playing 단계에서만 동작한다.
func (h *NTHub) resetAfkTimer(room *ntRoom) {
	room.AfkSeq++
	if room.AfkTimer != nil {
		room.AfkTimer.Stop()
		room.AfkTimer = nil
	}
	room.EndsAt = 0
	if room.Game.Phase != NTPhasePlaying {
		return
	}
	room.EndsAt = time.Now().Add(ntAfkTimeout).UnixMilli()
	sig := ntAfkSignal{GameID: room.Game.ID, Seq: room.AfkSeq}
	room.AfkTimer = time.AfterFunc(ntAfkTimeout, func() {
		h.afkFired <- sig
	})
}

// handleAfkFired AFK 타이머 발화 — 현재 차례 좌석(사람)을 자동 진행한다.
// 칩이 있으면 패스, 없으면 가져가기 (규칙상 유일한 합법 수). 좌석은 유지된다.
func (h *NTHub) handleAfkFired(sig ntAfkSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.AfkSeq != sig.Seq || room.Game.Phase != NTPhasePlaying {
		return
	}
	game := room.Game
	seat := game.CurrentSeat
	client := room.Clients[seat]
	if client == nil || client.Bot {
		return // 봇 좌석은 스스로 행동한다
	}

	action := NTMsgTake
	verb := "카드를 가져갑니다"
	if game.Players[seat].Chips > 0 {
		action = NTMsgPass
		verb = "패스합니다"
	}
	log.Printf("[노땡스][자동진행] game=%s | seat%d=%s 무응답 — 자동으로 %s",
		game.ID, seat, displayName(game.Players[seat].Name), verb)
	h.broadcastEvent(room, NTEventPayload{Kind: "afk", Seat: &seat,
		Name:    game.Players[seat].Name,
		Message: fmt.Sprintf("%s님이 오래 응답하지 않아 자동으로 %s", game.Players[seat].Name, verb)})
	h.handleGameMessage(NTGameMessage{Client: client, Message: NTMessage{Type: action}})
}

// ==================== 상태 뷰 (칩 은닉의 핵심) ====================

// buildNTState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를
// 쓴다. 은닉: chips 는 viewer 본인 좌석만 실값, 타인은 -1 (관전자
// viewerSeat -1 은 전원 -1). game_over 에만 전원 실값 공개 + score 확정.
// 획득 카드는 언제나 공개 정보다.
func (h *NTHub) buildNTState(room *ntRoom, viewerSeat int) NTGameStatePayload {
	game := room.Game
	reveal := game.Phase == NTPhaseGameOver

	players := []NTPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		chips := -1
		if reveal || p.Seat == viewerSeat {
			chips = p.Chips
		}
		score := 0
		if reveal {
			score = p.Score
		}
		players = append(players, NTPlayerView{
			Seat:      p.Seat,
			Name:      p.Name,
			Connected: c != nil && c.Connected,
			Bot:       c != nil && c.Bot,
			Chips:     chips,
			Cards:     append([]int{}, p.Cards...),
			Score:     score,
		})
	}

	endsAt := int64(0)
	if game.Phase == NTPhasePlaying {
		endsAt = room.EndsAt
	}

	return NTGameStatePayload{
		GameID:      game.ID,
		RoomCode:    room.Code,
		Phase:       game.Phase,
		HostSeat:    h.hostSeat(room),
		YourSeat:    viewerSeat,
		Spectators:  len(room.Spectators),
		EndsAt:      endsAt,
		CurrentSeat: game.CurrentSeat,
		DeckCount:   len(game.Deck),
		Card:        game.Card,
		PotChips:    game.PotChips,
		Players:     players,
	}
}

// broadcastState 좌석마다 개인화 스냅샷을 만들어 보낸다.
// 관전자에게는 전원 칩이 가려진 스냅샷(viewerSeat -1)이 간다.
// AFK 타이머를 먼저 리셋해야 스냅샷의 endsAt 이 새 마감 시각을 싣는다.
func (h *NTHub) broadcastState(room *ntRoom) {
	h.resetAfkTimer(room)
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, NTMessage{
			Type:    NTMsgGameState,
			Payload: h.buildNTState(room, seat),
		})
	}
	if len(room.Spectators) > 0 {
		msg := NTMessage{Type: NTMsgGameState, Payload: h.buildNTState(room, -1)}
		for c := range room.Spectators {
			h.sendToClient(c, msg)
		}
	}
}

func (h *NTHub) broadcastEvent(room *ntRoom, event NTEventPayload) {
	h.broadcastToRoom(room, NTMessage{Type: NTMsgEvent, Payload: event})
}

// finishGame 게임 종료 발표·전적 기록·방 정리 (쇼다운 — 전원 칩·점수 공개)
func (h *NTHub) finishGame(room *ntRoom) {
	game := room.Game
	if room.AfkTimer != nil {
		room.AfkTimer.Stop()
		room.AfkTimer = nil
	}

	winnerNames := []string{}
	for _, s := range game.WinnerSeats {
		winnerNames = append(winnerNames, game.Players[s].Name)
	}
	scores := []int{}
	names := []string{}
	for _, p := range game.Players {
		scores = append(scores, p.Score)
		names = append(names, displayName(p.Name))
	}

	first := -1
	firstScore := 0
	if len(game.WinnerSeats) > 0 {
		first = game.WinnerSeats[0]
		firstScore = game.Players[first].Score
	}
	h.broadcastEvent(room, NTEventPayload{Kind: "game_over", Seat: &first,
		Name: strings.Join(winnerNames, ", "),
		Message: fmt.Sprintf("%s님이 최저 %d점으로 승리했습니다!",
			strings.Join(winnerNames, "님, "), firstScore)})
	// 전원 칩·점수가 공개된 쇼다운 스냅샷을 먼저 보낸 뒤 종료를 알린다
	// (봇 러너는 nt_game_over 를 보고 스스로 끝난다)
	h.broadcastState(room)
	h.broadcastToRoom(room, NTMessage{
		Type: NTMsgGameOver,
		Payload: NTGameOverPayload{
			WinnerSeats: append([]int{}, game.WinnerSeats...),
			WinnerNames: winnerNames,
			Scores:      scores,
			Players:     h.buildNTState(room, -1).Players,
		},
	})

	displayWinners := []string{}
	for _, s := range game.WinnerSeats {
		displayWinners = append(displayWinners, displayName(game.Players[s].Name))
	}
	log.Printf("[노땡스][경기결과] game=%s | 승자=%v(%s) | 점수=%v | %s | 소요=%s",
		game.ID, game.WinnerSeats, strings.Join(displayWinners, ", "), scores,
		game.EndReason, matchDuration(game.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "nothanks",
		Players:  strings.Join(names, " vs "),
		Winner:   strings.Join(displayWinners, ", "),
		Reason:   game.EndReason,
		Duration: matchSeconds(game.StartedAt),
		Bot:      ntRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
	if room.Code != "" {
		delete(h.activeCodes, room.Code) // 코드 재사용 복귀
	}
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *NTHub) handleDisconnect(client *NTClient) {
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
	log.Printf("[노땡스][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, NTMessage{
		Type: NTMsgPlayerDisconnected,
		Payload: NTPlayerDisconnectedPayload{
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
func (h *NTHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == NTPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[노땡스][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := &NTClient{wsClient: newBotWSClient(), Hub: h, Seat: seat}
	bot.Name = client.Name // 좌석 이름은 유지 (표시는 bot 플래그로 구분)
	bot.GameID = room.Game.ID
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runNTBot(bot)

	h.broadcastEvent(room, NTEventPayload{Kind: "bot_takeover", Seat: &seat,
		Name:    client.Name,
		Message: fmt.Sprintf("%s님 자리를 봇이 이어받았습니다", client.Name)})
	// 새 봇이 이 스냅샷을 받아 자기 차례면 즉시 이어간다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *NTHub) handleRejoin(client *NTClient, msg NTMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload NTRejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, NTMessage{Type: NTMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, NTMessage{Type: NTMsgSessionExpired})
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

	log.Printf("[노땡스][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, NTMessage{
		Type:    NTMsgPlayerReconnected,
		Payload: NTPlayerReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	// 전원에게 접속 상태가 반영된 스냅샷 (재접속 당사자 복원 포함)
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *NTHub) clearGameSessions(room *ntRoom) {
	clearRoomSessions(&h.sessionManager, room.Clients)
}

// ==================== 전송 ====================

func (h *NTHub) sendError(client *NTClient, message string) {
	h.sendToClient(client, NTMessage{Type: NTMsgError, Payload: NTErrorPayload{Message: message}})
}

func (h *NTHub) sendToClient(client *NTClient, message NTMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[NT] ")
}

func (h *NTHub) broadcastToRoom(room *ntRoom, message NTMessage) {
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

func ServeNTWs(hub *NTHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[NT] Error upgrading connection:", err)
		return
	}

	client := &NTClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[NT] ",
		func(msg NTMessage) { hub.gameMessage <- NTGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
