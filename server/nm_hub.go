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

// 6 님트 단계 타이머 — 접속만 유지한 채 행동하지 않는 좌석이 게임을 영구
// 정지시키지 않게, 만료 시 자동 행동으로 해소한다 (테스트에서 짧게 낮춘다).
//   - picking: 미제출 전원 무작위 카드 제출
//   - choosing_row: 소머리 최소 행 자동 선택
//   - revealing: 공개 연출 대기 후 배치 진행 (nmRevealDelay)
var (
	nmAfkTimeout  = 30 * time.Second
	nmRevealDelay = 2 * time.Second
)

// nmRoom 게임(순수 상태)과 좌석별 연결의 매핑
type nmRoom struct {
	Game    *NMGame
	Clients map[int]*NMClient // seat → client

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string

	// PhaseTimer 단계 타임아웃 타이머 — 상태가 바뀔 때마다 다시 건다
	PhaseTimer *time.Timer
	// AfkSeq 단계 전환 일련번호 (뒤늦은 발화 무시용)
	AfkSeq int
	// EndsAt 현재 단계의 자동 진행 마감 시각 (unixMillis) — 스냅샷 노출용
	EndsAt int64

	// Spectators 관전자 연결 (사설 방 코드로만 진입, 상한 maxSpectators).
	// 좌석·세션이 없어 재접속을 지원하지 않는다 — 끊기면 목록에서만 뗀다.
	Spectators map[*NMClient]bool

	// LastReact 좌석별 마지막 리액션 발신 시각 (레이트리밋 장부)
	LastReact map[int]time.Time
}

// nmPhaseSignal 단계 타임아웃 타이머의 발화 표식 (뒤늦은 발화 무시용)
type nmPhaseSignal struct {
	GameID string
	Seq    int
}

type NMHub struct {
	// 등록된 클라이언트
	clients map[*NMClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*nmRoom

	// 글로벌 로비. 시작 전 공용 방은 하나뿐이다.
	lobby *nmRoom

	// 사설 방 (초대 코드 → 시작 전 방). 허브 고루틴에서만 접근한다.
	// 시작하면 privateLobbies 에서 activeCodes 로 옮긴다.
	privateLobbies map[string]*nmRoom

	// 진행 중 사설 방 (초대 코드 → gameID). 시작 후에도 코드로 방을 찾아
	// 관전 입장시키는 근거다. finishGame 에서 해제한다 (코드 재사용 복귀).
	activeCodes map[string]string

	// 클라이언트 등록
	register chan *NMClient

	// 클라이언트 등록 해제
	unregister chan *NMClient

	// 게임 메시지
	gameMessage chan NMGameMessage

	// 단계 타임아웃 발화 (time.AfterFunc → 허브 채널 경유)
	phaseFired chan nmPhaseSignal

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*NMClient]

	// 셔플·자동 선택용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type NMGameMessage struct {
	Client  *NMClient
	Message NMMessage
}

func NewNMHub() *NMHub {
	return &NMHub{
		register:       make(chan *NMClient),
		unregister:     make(chan *NMClient),
		clients:        make(map[*NMClient]bool),
		rooms:          make(map[string]*nmRoom),
		privateLobbies: make(map[string]*nmRoom),
		activeCodes:    make(map[string]string),
		gameMessage:    make(chan NMGameMessage),
		phaseFired:     make(chan nmPhaseSignal, 8),
		sessionManager: newSessionManager[*NMClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *NMHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[NM] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[NM] Client unregistered: %s", client.ID)
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

func (h *NMHub) handleGameMessage(gm NMGameMessage) {
	// 관전자는 어떤 행동도 할 수 없다 (보기 전용 — 리액션도 좌석 보유자만)
	if h.isSpectator(gm.Client) {
		h.sendError(gm.Client, spectatorDeniedMsg)
		return
	}
	switch gm.Message.Type {
	case NMMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case NMMsgReact:
		h.handleReact(gm.Client, gm.Message)
	case NMMsgFillBots:
		h.handleFillBots(gm.Client)
	case NMMsgStart:
		h.handleStart(gm.Client)
	case NMMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case NMMsgPick:
		h.handlePick(gm.Client, gm.Message)
	case NMMsgChooseRow:
		h.handleChooseRow(gm.Client, gm.Message)
	}
}

// ==================== 대기실 ====================

func (h *NMHub) handleJoinGame(client *NMClient, msg NMMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload NMJoinGamePayload
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

	log.Printf("[6님트][입장] game=%s | seat%d=%s 대기 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), NMMaxPlayers)
	if room.Code == "" { // 사설 방 입장은 조용히 (공용 로비만 알림)
		notify("6님트 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), len(room.Game.Players), NMMaxPlayers))
	}

	h.sendToClient(client, NMMessage{
		Type: NMMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
			"roomCode":  room.Code,
		},
	})

	h.broadcastEvent(room, NMEventPayload{Kind: "joined", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 입장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	// 정원이 차도 자동 시작하지 않는다 — 호스트 명시 시작만 (봇 채우기와 충돌 방지)
	h.broadcastState(room)
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다.
// 생략/빈 문자열은 기존 공용 로비 경로 그대로, "NEW"는 새 코드 발급,
// 그 외 코드는 해당 사설 방 (없으면 그 코드로 관대하게 새로 생성).
func (h *NMHub) lobbyRoomFor(roomField string) *nmRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewNMGame(uuid.New().String())
			h.lobby = &nmRoom{Game: game, Clients: map[int]*NMClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[NM] Created lobby game %s", game.ID)
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
		game := NewNMGame(uuid.New().String())
		room = &nmRoom{Game: game, Clients: map[int]*NMClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[NM] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// ==================== 관전 / 리액션 ====================

// addSpectator 진행 중(또는 가득 찬) 사설 방에 관전자로 등록한다.
// 좌석·세션 없음 — 재접속 미지원, 끊기면 다시 코드로 들어온다.
func (h *NMHub) addSpectator(room *nmRoom, client *NMClient, name string) {
	if len(room.Spectators) >= maxSpectators {
		h.sendError(client, spectatorFullMsg)
		return
	}
	if room.Spectators == nil {
		room.Spectators = map[*NMClient]bool{}
	}
	client.Name = name
	client.GameID = room.Game.ID
	room.Spectators[client] = true

	log.Printf("[6님트][관전] game=%s | %s 관전 입장 (%d명)",
		room.Game.ID, displayName(name), len(room.Spectators))

	h.sendToClient(client, NMMessage{
		Type:    NMMsgSpectateJoined,
		Payload: map[string]interface{}{"gameId": room.Game.ID, "roomCode": room.Code},
	})
	// 전원(관전자 포함)에게 관전자 수가 반영된 스냅샷 — 신규 관전자의 첫 스냅샷 겸용
	h.broadcastState(room)
}

// isSpectator 관전자 연결인지 (좌석 보유자·미입장 연결은 false)
func (h *NMHub) isSpectator(client *NMClient) bool {
	if client.GameID == "" {
		return false
	}
	room := h.rooms[client.GameID]
	return room != nil && room.Spectators[client]
}

// handleReact 리액션 이모지 — 좌석 보유자만, waiting 중에도 허용.
// 화이트리스트 외·레이트리밋 초과는 조용히 무시한다 (상태 저장 없음).
func (h *NMHub) handleReact(client *NMClient, msg NMMessage) {
	room := h.rooms[client.GameID]
	if room == nil || client.Seat < 0 {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload NMReactPayload
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
	h.broadcastEvent(room, NMEventPayload{Kind: "react", Seat: &seat, Name: client.Name, Message: payload.Emoji})
}

// waitingRoomOf 클라이언트가 속한 시작 전 방 (공용 로비 또는 사설 방)
func (h *NMHub) waitingRoomOf(client *NMClient) *nmRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Ready {
		return nil
	}
	return room
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *NMHub) hostSeat(room *nmRoom) int {
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

// nmHumanCount 방의 사람 수
func nmHumanCount(room *nmRoom) int {
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
func (h *NMHub) updateLobbyWaiting(room *nmRoom) {
	if room.Code != "" {
		return
	}
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && nmHumanCount(room) >= 1
	lobbySetWaiting("nimmt", waiting)
}

// handleFillBots host 가 빈 좌석을 연습봇으로 6인까지 채운 뒤 즉시
// 시작한다 (스폰은 join 을 거치지 않으므로 명시 시작 호출).
func (h *NMHub) handleFillBots(client *NMClient) {
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
	for len(room.Game.Players) < NMFillBotTarget {
		botNo++
		if !h.spawnNMBot(room, fmt.Sprintf("%s%d", botName, botNo)) {
			break
		}
	}

	if room.Game.CanStart() {
		h.startGame(room)
		return
	}
	h.broadcastState(room)
}

func (h *NMHub) handleStart(client *NMClient) {
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

func (h *NMHub) startGame(room *nmRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	if room.Code != "" {
		// 시작한 사설 방의 코드는 진행 중 장부로 옮긴다 (관전 입장 근거)
		delete(h.privateLobbies, room.Code)
		h.activeCodes[room.Code] = room.Game.ID
	} else {
		h.lobby = nil // 시작한 방은 로비에서 떼어낸다
		lobbySetWaiting("nimmt", false)
	}

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	log.Printf("[6님트][경기시작] game=%s | 인원=%d | %d트릭 | %v",
		room.Game.ID, len(room.Game.Players), NMTricks, names)
	if !nmRoomHasBot(room) { // 연습봇전은 운영자 알림을 억제한다
		notify("6님트 게임 시작", fmt.Sprintf("%d인전 시작", len(room.Game.Players)))
	}

	h.broadcastEvent(room, NMEventPayload{Kind: "game_started",
		Message: fmt.Sprintf("게임 시작 — 총 %d트릭, 소머리를 가장 적게 먹으면 승리!", NMTricks)})
	h.announceTrickBegin(room)
	// 마감 시각을 먼저 세팅해야 첫 스냅샷에 endsAt 이 실린다
	h.scheduleDeadline(room, nmAfkTimeout)
	h.broadcastState(room)
}

// announceTrickBegin 트릭 시작 발표 (동시 선택 안내)
func (h *NMHub) announceTrickBegin(room *nmRoom) {
	h.broadcastEvent(room, NMEventPayload{Kind: "trick_begin",
		Message: fmt.Sprintf("%d번째 트릭 — 전원 카드 1장을 선택하세요", room.Game.Trick)})
}

// removeFromLobby 대기실에서 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *NMHub) removeFromLobby(room *nmRoom, client *NMClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	// 좌석 압축: oldSeat 뒤의 클라이언트를 한 칸씩 당긴다
	rebuilt := map[int]*NMClient{}
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

	log.Printf("[6님트][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), NMMaxPlayers)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if nmHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, NMMessage{Type: NMMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			lobbySetWaiting("nimmt", false)
		}
		return
	}

	h.broadcastEvent(room, NMEventPayload{Kind: "left", Name: client.Name,
		Message: fmt.Sprintf("%s님이 퇴장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *NMHub) roomOf(client *NMClient) *nmRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

// handlePick 동시 선택 제출 — 카드 내용은 비공개, 제출 사실만 알린다.
// 전원 제출이 모이면 일괄 공개(revealing)로 넘어간다.
func (h *NMHub) handlePick(client *NMClient, msg NMMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload NMPickPayload
	json.Unmarshal(payloadBytes, &payload)

	game := room.Game
	if err := game.SubmitPick(client.Seat, payload.Card); err != nil {
		h.sendError(client, err.Error())
		return
	}
	seat := client.Seat
	// 카드 내용은 비공개 — 제출 사실만 알린다
	h.broadcastEvent(room, NMEventPayload{Kind: "picked", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 카드를 냈습니다", client.Name)})

	if game.AllPicked() {
		h.startReveal(room)
		return
	}
	// 동시 선택 중에는 처음 건 마감을 유지한다 (전원 공용 마감)
	h.broadcastState(room)
}

// startReveal 전원 제출 — 일괄 공개하고 잠깐의 연출 대기 후 배치를 진행한다
func (h *NMHub) startReveal(room *nmRoom) {
	game := room.Game
	game.StartReveal()

	parts := []string{}
	for _, e := range game.Picks {
		parts = append(parts, fmt.Sprintf("%s %d", game.Players[e.Seat].Name, e.Card))
	}
	log.Printf("[6님트][공개] game=%s | %d트릭 | %s",
		game.ID, game.Trick, strings.Join(parts, " · "))
	h.broadcastEvent(room, NMEventPayload{Kind: "reveal",
		Message: fmt.Sprintf("전원 선택 완료 — 공개: %s", strings.Join(parts, " · "))})

	h.scheduleDeadline(room, nmRevealDelay)
	h.broadcastState(room)
}

// handleChooseRow 최소 카드의 행 선택 — 그 좌석만 허용된다
func (h *NMHub) handleChooseRow(client *NMClient, msg NMMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload NMChooseRowPayload
	json.Unmarshal(payloadBytes, &payload)

	placement, err := room.Game.ChooseRow(client.Seat, payload.Row)
	if err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.announcePlacement(room, placement)
	h.runPlacements(room)
}

// runPlacements 공개된 카드를 낮은 순으로 배치한다. 모든 행 끝보다 작은
// 카드를 만나면 choosing_row 로 멈추고 그 좌석의 선택(또는 AFK 자동)을
// 기다린다. 대기열이 소진되면 트릭을 마무리한다.
func (h *NMHub) runPlacements(room *nmRoom) {
	game := room.Game
	for {
		placement, needChoice := game.PlaceNext()
		if needChoice {
			seat := game.ChooserSeat
			card := game.Pending[0].Card
			h.broadcastEvent(room, NMEventPayload{Kind: "choose_row", Seat: &seat,
				Name: game.Players[seat].Name,
				Message: fmt.Sprintf("%s님의 %d은(는) 모든 행보다 작습니다 — 가져갈 행을 선택하세요",
					game.Players[seat].Name, card)})
			h.scheduleDeadline(room, nmAfkTimeout)
			h.broadcastState(room)
			return
		}
		if placement == nil {
			break // 대기열 소진
		}
		h.announcePlacement(room, placement)
	}
	h.finishTrick(room)
}

// announcePlacement 배치 한 건을 공개 이벤트로 번역한다
func (h *NMHub) announcePlacement(room *nmRoom, placement *NMPlacement) {
	game := room.Game
	seat := placement.Seat
	name := game.Players[seat].Name
	if placement.Ate {
		log.Printf("[6님트][행먹기] game=%s | %d트릭 | seat%d=%s %d → %d행 먹음 (누적 소머리 %d)",
			game.ID, game.Trick, seat, displayName(name), placement.Card, placement.Row,
			game.Players[seat].Penalty)
		h.broadcastEvent(room, NMEventPayload{Kind: "ate", Seat: &seat, Name: name,
			Message: fmt.Sprintf("%s님이 %d행을 먹었습니다 (누적 소머리 %d) — %d이 새 행이 됩니다",
				name, placement.Row+1, game.Players[seat].Penalty, placement.Card)})
		return
	}
	h.broadcastEvent(room, NMEventPayload{Kind: "placed", Seat: &seat, Name: name,
		Message: fmt.Sprintf("%s님의 %d이 %d행에 놓였습니다", name, placement.Card, placement.Row+1)})
}

// finishTrick 트릭 마무리 — 10트릭이면 종료, 아니면 다음 동시 선택을 연다
func (h *NMHub) finishTrick(room *nmRoom) {
	game := room.Game
	if game.FinishTrick() {
		h.finishGame(room)
		return
	}
	h.announceTrickBegin(room)
	h.scheduleDeadline(room, nmAfkTimeout)
	h.broadcastState(room)
}

// ==================== 단계 타임아웃 (AFK 진행 보장) ====================

// scheduleDeadline 현재 단계의 타임아웃 타이머를 건다. AfkSeq 를 올려
// 지나간 발화를 구분하고, 발화는 허브 채널을 경유하므로 허브 밖에서
// 상태를 만지지 않는다.
func (h *NMHub) scheduleDeadline(room *nmRoom, dur time.Duration) {
	h.stopPhaseTimer(room)
	room.AfkSeq++
	room.EndsAt = time.Now().Add(dur).UnixMilli()
	sig := nmPhaseSignal{GameID: room.Game.ID, Seq: room.AfkSeq}
	room.PhaseTimer = time.AfterFunc(dur, func() {
		h.phaseFired <- sig
	})
}

func (h *NMHub) stopPhaseTimer(room *nmRoom) {
	if room.PhaseTimer != nil {
		room.PhaseTimer.Stop()
		room.PhaseTimer = nil
	}
}

// handlePhaseFired 단계 타임아웃 — 자동 행동으로 해소한다
func (h *NMHub) handlePhaseFired(sig nmPhaseSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.AfkSeq != sig.Seq {
		return
	}
	game := room.Game
	switch game.Phase {
	case NMPhasePicking:
		// 동시 선택 마감 — 미제출 전원 무작위 카드
		seats := game.AutoPickAll(h.rng)
		for _, s := range seats {
			seat := s
			log.Printf("[6님트][자동진행] game=%s | %d트릭 | seat%d=%s 무응답 — 무작위 카드 제출",
				game.ID, game.Trick, seat, displayName(game.Players[seat].Name))
			h.broadcastEvent(room, NMEventPayload{Kind: "afk", Seat: &seat,
				Name:    game.Players[seat].Name,
				Message: fmt.Sprintf("%s님이 오래 응답하지 않아 무작위 카드를 냅니다", game.Players[seat].Name)})
		}
		if game.AllPicked() {
			h.startReveal(room)
			return
		}
		h.broadcastState(room)

	case NMPhaseRevealing:
		// 공개 연출 대기 종료 — 낮은 순 배치 진행
		h.runPlacements(room)

	case NMPhaseChoosingRow:
		// 행 선택 방치 — 소머리 최소 행 자동 선택
		seat := game.ChooserSeat
		row := game.MinHeadsRow()
		placement, err := game.ChooseRow(seat, row)
		if err != nil {
			return
		}
		log.Printf("[6님트][자동진행] game=%s | %d트릭 | seat%d=%s 무응답 — 소머리 최소 %d행 자동 선택",
			game.ID, game.Trick, seat, displayName(game.Players[seat].Name), row)
		h.broadcastEvent(room, NMEventPayload{Kind: "afk", Seat: &seat,
			Name:    game.Players[seat].Name,
			Message: fmt.Sprintf("%s님이 오래 응답하지 않아 소머리가 가장 적은 행을 자동으로 가져갑니다", game.Players[seat].Name)})
		h.announcePlacement(room, placement)
		h.runPlacements(room)
	}
}

// ==================== 상태 뷰 (은닉의 핵심) ====================

// nmPlayerViews 좌석별 공개 정보 — picking 중 제출 카드는 절대 싣지 않는다
// (제출 여부 picked 만)
func (h *NMHub) nmPlayerViews(room *nmRoom) []NMPlayerView {
	game := room.Game
	players := []NMPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		players = append(players, NMPlayerView{
			Seat:      p.Seat,
			Name:      p.Name,
			Connected: c != nil && c.Connected,
			Bot:       c != nil && c.Bot,
			Picked:    p.Pick != 0,
			Penalty:   p.Penalty,
		})
	}
	return players
}

// buildNMState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를
// 쓴다. 은닉: yourHand 는 본인만 실값(타인·관전자 viewerSeat -1 은 빈 배열),
// picks 는 revealing/choosing_row 에만 존재 — picking 중에는 필드 자체가
// 부재하고 제출 여부(picked)만 공개된다.
func (h *NMHub) buildNMState(room *nmRoom, viewerSeat int) NMGameStatePayload {
	game := room.Game

	yourHand := []int{}
	if viewerSeat >= 0 && viewerSeat < len(game.Players) {
		yourHand = append(yourHand, game.Players[viewerSeat].Hand...)
	}

	// nil 슬라이스는 JSON null 로 나가 프론트 .length 접근이 죽는다 — 빈 배열 보장
	rows := make([][]int, NMRows)
	for r := range rows {
		rows[r] = []int{}
		if r < len(game.Rows) {
			rows[r] = append(rows[r], game.Rows[r]...)
		}
	}

	var picks []NMPickEntry
	if game.Phase == NMPhaseRevealing || game.Phase == NMPhaseChoosingRow {
		picks = append([]NMPickEntry{}, game.Picks...)
	}

	endsAt := int64(0)
	switch game.Phase {
	case NMPhasePicking, NMPhaseRevealing, NMPhaseChoosingRow:
		endsAt = room.EndsAt
	}

	return NMGameStatePayload{
		GameID:        game.ID,
		RoomCode:      room.Code,
		Phase:         game.Phase,
		HostSeat:      h.hostSeat(room),
		YourSeat:      viewerSeat,
		Spectators:    len(room.Spectators),
		EndsAt:        endsAt,
		Trick:         game.Trick,
		Rows:          rows,
		YourHand:      yourHand,
		Picks:         picks,
		Players:       h.nmPlayerViews(room),
		ChooserSeat:   game.ChooserSeat,
		LastPlacement: game.LastPlacement,
	}
}

// broadcastState 좌석마다 개인화 스냅샷을 만들어 보낸다.
// 관전자에게는 손패 없는 공개 스냅샷(viewerSeat -1)이 간다.
func (h *NMHub) broadcastState(room *nmRoom) {
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, NMMessage{
			Type:    NMMsgGameState,
			Payload: h.buildNMState(room, seat),
		})
	}
	if len(room.Spectators) > 0 {
		msg := NMMessage{Type: NMMsgGameState, Payload: h.buildNMState(room, -1)}
		for c := range room.Spectators {
			h.sendToClient(c, msg)
		}
	}
}

func (h *NMHub) broadcastEvent(room *nmRoom, event NMEventPayload) {
	h.broadcastToRoom(room, NMMessage{Type: NMMsgEvent, Payload: event})
}

// finishGame 게임 종료 발표·전적 기록·방 정리
func (h *NMHub) finishGame(room *nmRoom) {
	game := room.Game
	h.stopPhaseTimer(room)
	room.EndsAt = 0

	winnerNames := []string{}
	for _, s := range game.WinnerSeats {
		winnerNames = append(winnerNames, game.Players[s].Name)
	}
	penalties := []int{}
	names := []string{}
	for _, p := range game.Players {
		penalties = append(penalties, p.Penalty)
		names = append(names, displayName(p.Name))
	}

	first := -1
	firstPenalty := 0
	if len(game.WinnerSeats) > 0 {
		first = game.WinnerSeats[0]
		firstPenalty = game.Players[first].Penalty
	}
	h.broadcastEvent(room, NMEventPayload{Kind: "game_over", Seat: &first,
		Name: strings.Join(winnerNames, ", "),
		Message: fmt.Sprintf("%s님이 승리했습니다! (소머리 %d개)",
			strings.Join(winnerNames, "님, "), firstPenalty)})
	// 최종 벌점이 반영된 스냅샷을 먼저 보낸 뒤 종료를 알린다
	// (봇 러너는 nm_game_over 를 보고 스스로 끝난다)
	h.broadcastState(room)
	h.broadcastToRoom(room, NMMessage{
		Type: NMMsgGameOver,
		Payload: NMGameOverPayload{
			WinnerSeats: append([]int{}, game.WinnerSeats...),
			WinnerNames: winnerNames,
			Penalties:   penalties,
			Players:     h.nmPlayerViews(room),
		},
	})

	displayWinners := []string{}
	for _, s := range game.WinnerSeats {
		displayWinners = append(displayWinners, displayName(game.Players[s].Name))
	}
	log.Printf("[6님트][경기결과] game=%s | 승자=%v(%s) | 소머리=%v | 소요=%s",
		game.ID, game.WinnerSeats, strings.Join(displayWinners, ", "), penalties,
		matchDuration(game.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "nimmt",
		Players:  strings.Join(names, " vs "),
		Winner:   strings.Join(displayWinners, ", "),
		Reason:   "소머리 최소",
		Duration: matchSeconds(game.StartedAt),
		Bot:      nmRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
	if room.Code != "" {
		delete(h.activeCodes, room.Code) // 코드 재사용 복귀
	}
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *NMHub) handleDisconnect(client *NMClient) {
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
	log.Printf("[6님트][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, NMMessage{
		Type: NMMsgPlayerDisconnected,
		Payload: NMPlayerDisconnectedPayload{
			Seat:         client.Seat,
			Name:         client.Name,
			GraceSeconds: int(h.grace.Seconds()),
		},
	})
	h.broadcastState(room)

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 안에 재접속하지 않은 좌석은 연습봇으로 대체하고
// 게임은 계속한다 — 동시 선택·행 선택 진행이 이탈 좌석에 막히지 않는 근거
func (h *NMHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == NMPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[6님트][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := &NMClient{wsClient: newBotWSClient(), Hub: h, Seat: seat}
	bot.Name = client.Name // 좌석 이름은 유지 (표시는 bot 플래그로 구분)
	bot.GameID = room.Game.ID
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runNMBot(bot)

	h.broadcastEvent(room, NMEventPayload{Kind: "bot_takeover", Seat: &seat,
		Name:    client.Name,
		Message: fmt.Sprintf("%s님 자리를 봇이 이어받았습니다", client.Name)})
	// 새 봇이 이 스냅샷을 받아 제출·선택이 남았으면 즉시 이어간다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *NMHub) handleRejoin(client *NMClient, msg NMMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload NMRejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, NMMessage{Type: NMMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, NMMessage{Type: NMMsgSessionExpired})
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

	log.Printf("[6님트][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, NMMessage{
		Type:    NMMsgPlayerReconnected,
		Payload: NMPlayerReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	// 전원에게 접속 상태가 반영된 스냅샷 (재접속 당사자 복원 포함)
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *NMHub) clearGameSessions(room *nmRoom) {
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		h.drop(c.SessionID)
	}
}

// ==================== 전송 ====================

func (h *NMHub) sendError(client *NMClient, message string) {
	h.sendToClient(client, NMMessage{Type: NMMsgError, Payload: NMErrorPayload{Message: message}})
}

func (h *NMHub) sendToClient(client *NMClient, message NMMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[NM] ")
}

func (h *NMHub) broadcastToRoom(room *nmRoom, message NMMessage) {
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

func ServeNMWs(hub *NMHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[NM] Error upgrading connection:", err)
		return
	}

	client := &NMClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[NM] ",
		func(msg NMMessage) { hub.gameMessage <- NMGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
