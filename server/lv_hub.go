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

// lvAfkTimeout 접속 유지 AFK 의 자동 진행 대기 시간 — 발화하면 현재 턴
// 좌석의 수를 봇 두뇌로 1회 자동 실행한다 (테스트에서 짧게 낮춘다).
var lvAfkTimeout = 45 * time.Second

// lvRoundEndDelay round_end 결과 표시 후 다음 라운드로 자동 진행하기까지의
// 시간 (테스트에서 짧게 낮춘다)
var lvRoundEndDelay = 3 * time.Second

// lvRoom 게임(순수 상태)과 좌석별 연결의 매핑
type lvRoom struct {
	Game    *LVGame
	Clients map[int]*LVClient // seat → client

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string

	// AfkTimer 접속 유지 AFK 구제 타이머 — 상태가 바뀔 때마다 리셋되고,
	// 발화하면 현재 턴 좌석의 수를 봇 두뇌로 1회 자동 실행한다.
	AfkTimer *time.Timer
	// AfkSeq 상태 변경 일련번호 (뒤늦은 발화 무시용)
	AfkSeq int
	// EndsAt 현재 턴의 AFK 마감 시각 (unixMillis) — 스냅샷 노출용
	EndsAt int64

	// RoundTimer round_end 자동 진행 타이머
	RoundTimer *time.Timer

	// Spectators 관전자 연결 (사설 방 코드로만 진입, 상한 maxSpectators).
	// 좌석·세션이 없어 재접속을 지원하지 않는다 — 끊기면 목록에서만 뗀다.
	Spectators map[*LVClient]bool

	// LastReact 좌석별 마지막 리액션 발신 시각 (레이트리밋 장부)
	LastReact map[int]time.Time
}

// lvAfkSignal AFK 타이머 발화 표식
type lvAfkSignal struct {
	GameID string
	Seq    int
}

// lvRoundSignal 어느 게임의 몇 번째 라운드 종료 타이머인지 (뒤늦은 발화 무시용)
type lvRoundSignal struct {
	GameID  string
	RoundNo int
}

type LVHub struct {
	// 등록된 클라이언트
	clients map[*LVClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*lvRoom

	// 글로벌 로비. 시작 전 공용 방은 하나뿐이다.
	lobby *lvRoom

	// 사설 방 (초대 코드 → 시작 전 방). 허브 고루틴에서만 접근한다.
	// 시작하면 privateLobbies 에서 activeCodes 로 옮긴다.
	privateLobbies map[string]*lvRoom

	// 진행 중 사설 방 (초대 코드 → gameID). 시작 후에도 코드로 방을 찾아
	// 관전 입장시키는 근거다. finishGame 에서 해제한다 (코드 재사용 복귀).
	activeCodes map[string]string

	// 클라이언트 등록
	register chan *LVClient

	// 클라이언트 등록 해제
	unregister chan *LVClient

	// 게임 메시지
	gameMessage chan LVGameMessage

	// 자동 진행 알림 (time.AfterFunc → 허브 채널 경유)
	roundFired chan lvRoundSignal
	afkFired   chan lvAfkSignal

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*LVClient]

	// 셔플·선공 결정용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type LVGameMessage struct {
	Client  *LVClient
	Message LVMessage
}

func NewLVHub() *LVHub {
	return &LVHub{
		register:       make(chan *LVClient),
		unregister:     make(chan *LVClient),
		clients:        make(map[*LVClient]bool),
		rooms:          make(map[string]*lvRoom),
		privateLobbies: make(map[string]*lvRoom),
		activeCodes:    make(map[string]string),
		gameMessage:    make(chan LVGameMessage),
		roundFired:     make(chan lvRoundSignal, 8),
		afkFired:       make(chan lvAfkSignal, 8),
		sessionManager: newSessionManager[*LVClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *LVHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[LV] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[LV] Client unregistered: %s", client.ID)
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

func (h *LVHub) handleGameMessage(gm LVGameMessage) {
	// 관전자는 어떤 행동도 할 수 없다 (보기 전용 — 리액션도 좌석 보유자만)
	if h.isSpectator(gm.Client) {
		h.sendError(gm.Client, spectatorDeniedMsg)
		return
	}
	switch gm.Message.Type {
	case LVMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case LVMsgReact:
		h.handleReact(gm.Client, gm.Message)
	case LVMsgFillBots:
		h.handleFillBots(gm.Client)
	case LVMsgStart:
		h.handleStart(gm.Client)
	case LVMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case LVMsgPlay:
		h.handlePlay(gm.Client, gm.Message)
	}
}

// ==================== 대기실 ====================

func (h *LVHub) handleJoinGame(client *LVClient, msg LVMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload LVJoinGamePayload
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

	log.Printf("[러브레터][입장] game=%s | seat%d=%s 대기 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), LVMaxPlayers)
	if room.Code == "" { // 사설 방 입장은 조용히 (공용 로비만 알림)
		notify("러브레터 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), len(room.Game.Players), LVMaxPlayers))
	}

	h.sendToClient(client, LVMessage{
		Type: LVMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
			"roomCode":  room.Code,
		},
	})

	h.broadcastEvent(room, LVEventPayload{Kind: "joined", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 입장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	// 4인이 차도 자동 시작하지 않는다 — 호스트 명시 시작만 (봇 채우기와 충돌 방지)
	h.broadcastState(room)
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다.
// 생략/빈 문자열은 기존 공용 로비 경로 그대로, "NEW"는 새 코드 발급,
// 그 외 코드는 해당 사설 방 (없으면 그 코드로 관대하게 새로 생성).
func (h *LVHub) lobbyRoomFor(roomField string) *lvRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewLVGame(uuid.New().String())
			h.lobby = &lvRoom{Game: game, Clients: map[int]*LVClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[LV] Created lobby game %s", game.ID)
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
		game := NewLVGame(uuid.New().String())
		room = &lvRoom{Game: game, Clients: map[int]*LVClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[LV] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// ==================== 관전 / 리액션 ====================

// addSpectator 진행 중(또는 가득 찬) 사설 방에 관전자로 등록한다.
// 좌석·세션 없음 — 재접속 미지원, 끊기면 다시 코드로 들어온다.
func (h *LVHub) addSpectator(room *lvRoom, client *LVClient, name string) {
	if len(room.Spectators) >= maxSpectators {
		h.sendError(client, spectatorFullMsg)
		return
	}
	if room.Spectators == nil {
		room.Spectators = map[*LVClient]bool{}
	}
	client.Name = name
	client.GameID = room.Game.ID
	room.Spectators[client] = true

	log.Printf("[러브레터][관전] game=%s | %s 관전 입장 (%d명)",
		room.Game.ID, displayName(name), len(room.Spectators))

	h.sendToClient(client, LVMessage{
		Type:    LVMsgSpectateJoined,
		Payload: map[string]interface{}{"gameId": room.Game.ID, "roomCode": room.Code},
	})
	// 전원(관전자 포함)에게 관전자 수가 반영된 스냅샷 — 신규 관전자의 첫 스냅샷 겸용
	h.broadcastState(room)
}

// isSpectator 관전자 연결인지 (좌석 보유자·미입장 연결은 false)
func (h *LVHub) isSpectator(client *LVClient) bool {
	if client.GameID == "" {
		return false
	}
	room := h.rooms[client.GameID]
	return room != nil && room.Spectators[client]
}

// handleReact 리액션 이모지 — 좌석 보유자만, waiting 중에도 허용.
// 화이트리스트 외·레이트리밋 초과는 조용히 무시한다 (상태 저장 없음).
func (h *LVHub) handleReact(client *LVClient, msg LVMessage) {
	room := h.rooms[client.GameID]
	if room == nil || client.Seat < 0 {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload LVReactPayload
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
	h.broadcastEvent(room, LVEventPayload{Kind: "react", Seat: &seat, Name: client.Name, Message: payload.Emoji})
}

// waitingRoomOf 클라이언트가 속한 시작 전 방 (공용 로비 또는 사설 방)
func (h *LVHub) waitingRoomOf(client *LVClient) *lvRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Ready {
		return nil
	}
	return room
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *LVHub) hostSeat(room *lvRoom) int {
	return hostSeatOf(room.Clients)
}

// lvHumanCount 방의 사람 수
func lvHumanCount(room *lvRoom) int {
	return humanCountOf(room.Clients)
}

// updateLobbyWaiting 로비 현황판 갱신 — 사람 1명 이상 대기 && 미시작.
// 사설 방은 현황판에 노출하지 않는다 (초대 링크로만 접근).
func (h *LVHub) updateLobbyWaiting(room *lvRoom) {
	if room.Code != "" {
		return
	}
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && lvHumanCount(room) >= 1
	lobbySetWaiting("loveletter", waiting)
}

// handleFillBots host 가 빈 좌석 전부(4인까지)를 연습봇으로 채운 뒤 즉시
// 시작한다 (스폰은 join 을 거치지 않으므로 명시 시작 호출).
func (h *LVHub) handleFillBots(client *LVClient) {
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
	for len(room.Game.Players) < LVMaxPlayers {
		botNo++
		if !h.spawnLVBot(room, fmt.Sprintf("%s%d", botName, botNo)) {
			break
		}
	}

	if len(room.Game.Players) == LVMaxPlayers {
		h.startGame(room)
		return
	}
	h.broadcastState(room)
}

func (h *LVHub) handleStart(client *LVClient) {
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

func (h *LVHub) startGame(room *lvRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	if room.Code != "" {
		// 시작한 사설 방의 코드는 진행 중 장부로 옮긴다 (관전 입장 근거)
		delete(h.privateLobbies, room.Code)
		h.activeCodes[room.Code] = room.Game.ID
	} else {
		h.lobby = nil // 시작한 방은 로비에서 떼어낸다
		lobbySetWaiting("loveletter", false)
	}

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	log.Printf("[러브레터][경기시작] game=%s | 인원=%d | 목표=%d토큰 | 선공=seat%d | %v",
		room.Game.ID, len(room.Game.Players), room.Game.TargetTokens,
		room.Game.CurrentSeat, names)
	if !lvRoomHasBot(room) { // 연습봇전은 운영자 알림을 억제한다
		notify("러브레터 게임 시작", fmt.Sprintf("%d인전 시작", len(room.Game.Players)))
	}

	first := room.Game.CurrentSeat
	h.broadcastEvent(room, LVEventPayload{Kind: "game_started", Seat: &first,
		Name: room.Game.Players[first].Name,
		Message: fmt.Sprintf("게임 시작 — %s님 선공 (목표 %d토큰)",
			room.Game.Players[first].Name, room.Game.TargetTokens)})
	h.broadcastState(room)
}

// removeFromLobby 대기실에서 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *LVHub) removeFromLobby(room *lvRoom, client *LVClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	// 좌석 압축: oldSeat 뒤의 클라이언트를 한 칸씩 당긴다
	rebuilt := map[int]*LVClient{}
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

	log.Printf("[러브레터][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), LVMaxPlayers)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if lvHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, LVMessage{Type: LVMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			lobbySetWaiting("loveletter", false)
		}
		return
	}

	h.broadcastEvent(room, LVEventPayload{Kind: "left", Name: client.Name,
		Message: fmt.Sprintf("%s님이 퇴장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *LVHub) roomOf(client *LVClient) *lvRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

func (h *LVHub) handlePlay(client *LVClient, msg LVMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload LVPlayPayload
	json.Unmarshal(payloadBytes, &payload)

	game := room.Game
	res, err := game.Play(client.Seat, payload.Card, payload.TargetSeat, payload.Guess)
	if err != nil {
		h.sendError(client, err.Error())
		return
	}

	log.Printf("[러브레터][플레이] game=%s | seat%d=%s → %s(%d) 대상=%d",
		game.ID, client.Seat, displayName(client.Name), lvCardName(res.Card), res.Card, res.TargetSeat)

	h.announcePlay(room, client, res)

	if res.RoundEnded {
		h.announceRoundEnd(room)
	}
	h.broadcastState(room)
	if res.GameOver {
		h.finishGame(room)
	} else if res.RoundEnded {
		h.scheduleRoundEnd(room)
	}
}

// announcePlay 플레이 결과를 공개 이벤트·비공개 통지로 번역한다.
// 공개 정보: 낸 카드·대상·경비 추측값·탈락·왕자 대상이 버린 카드(더미 공개).
// 비공개 정보: 사제 열람 결과(요청자만)·남작 비교 값(당사자만).
func (h *LVHub) announcePlay(room *lvRoom, client *LVClient, res *LVPlayResult) {
	game := room.Game
	seat := res.ActorSeat
	actorName := client.Name
	cardLabel := fmt.Sprintf("%s(%d)", lvCardName(res.Card), res.Card)

	targetName := ""
	if res.TargetSeat >= 0 {
		targetName = game.Players[res.TargetSeat].Name
	}

	message := ""
	switch {
	case res.NoEffect:
		message = fmt.Sprintf("%s님이 %s을(를) 냈습니다 — 지정할 대상이 없어 효과가 없습니다", actorName, cardLabel)
	case res.Card == LVGuard && res.TargetSeat >= 0:
		outcome := "빗나감"
		if res.GuessCorrect {
			outcome = "적중!"
		}
		message = fmt.Sprintf("%s님이 경비병으로 %s님의 카드를 %s(%d)(으)로 추측 — %s",
			actorName, targetName, lvCardName(res.Guess), res.Guess, outcome)
	case res.Card == LVPriest:
		message = fmt.Sprintf("%s님이 사제로 %s님의 손패를 확인했습니다", actorName, targetName)
	case res.Card == LVBaron:
		outcome := "동점 — 무효"
		if res.BaronLoserSeat >= 0 {
			outcome = fmt.Sprintf("%s님 패배", game.Players[res.BaronLoserSeat].Name)
		}
		message = fmt.Sprintf("%s님이 남작으로 %s님과 손패를 비교 — %s", actorName, targetName, outcome)
	case res.Card == LVHandmaid:
		message = fmt.Sprintf("%s님이 시녀의 보호를 받습니다 (다음 자기 턴까지)", actorName)
	case res.Card == LVPrince:
		message = fmt.Sprintf("%s님이 왕자로 %s님의 손패 %s(%d)을(를) 버리게 했습니다",
			actorName, targetName, lvCardName(res.PrinceDiscarded), res.PrinceDiscarded)
	case res.Card == LVKing:
		message = fmt.Sprintf("%s님이 왕으로 %s님과 손패를 교환했습니다", actorName, targetName)
	case res.Card == LVPrincess:
		message = fmt.Sprintf("%s님이 공주를 버려 탈락했습니다", actorName)
	default: // 백작부인 등 효과 없는 카드
		message = fmt.Sprintf("%s님이 %s을(를) 냈습니다", actorName, cardLabel)
	}

	event := LVEventPayload{Kind: "card_played", Seat: &seat, Name: actorName, Message: message}
	if res.TargetSeat >= 0 {
		target := res.TargetSeat
		event.TargetSeat = &target
	}
	h.broadcastEvent(room, event)

	// 비공개 통지 — 사제는 요청자만, 남작 비교 값은 당사자끼리만
	if res.PriestSeen >= 0 {
		h.sendToClient(client, LVMessage{Type: LVMsgPrivate, Payload: LVPrivatePayload{
			Kind: "priest",
			Message: fmt.Sprintf("%s님의 손패는 %s(%d)입니다",
				targetName, lvCardName(res.PriestSeen), res.PriestSeen),
		}})
	}
	if res.Card == LVBaron && res.TargetSeat >= 0 && !res.NoEffect {
		outcome := "동점 — 무효"
		if res.BaronLoserSeat >= 0 {
			outcome = fmt.Sprintf("%s님 탈락", game.Players[res.BaronLoserSeat].Name)
		}
		private := LVMessage{Type: LVMsgPrivate, Payload: LVPrivatePayload{
			Kind: "baron",
			Message: fmt.Sprintf("남작 비교 — %s: %s(%d) vs %s: %s(%d) → %s",
				actorName, lvCardName(res.BaronActorCard), res.BaronActorCard,
				targetName, lvCardName(res.BaronTargetCard), res.BaronTargetCard, outcome),
		}}
		h.sendToClient(client, private)
		h.sendToClient(room.Clients[res.TargetSeat], private)
	}

	for _, elim := range res.Eliminated {
		s := elim
		h.broadcastEvent(room, LVEventPayload{Kind: "eliminated", Seat: &s,
			Name:    game.Players[s].Name,
			Message: fmt.Sprintf("%s님이 탈락했습니다", game.Players[s].Name)})
	}
}

// announceRoundEnd 라운드 결과 이벤트 (스냅샷의 roundResult 와 병행)
func (h *LVHub) announceRoundEnd(room *lvRoom) {
	game := room.Game
	rr := game.RoundResult
	if rr == nil {
		return
	}
	winner := rr.WinnerSeat
	reason := "홀로 생존"
	if rr.Reason == "highest_card" {
		reason = "가장 높은 카드"
	}
	log.Printf("[러브레터][라운드종료] game=%s | %d라운드 | 승자=seat%d(%s) | %s",
		game.ID, game.RoundNo, winner, displayName(game.Players[winner].Name), rr.Reason)
	h.broadcastEvent(room, LVEventPayload{Kind: "round_end", Seat: &winner,
		Name: game.Players[winner].Name,
		Message: fmt.Sprintf("%s님이 라운드 승리 (%s) — 토큰 %d개",
			game.Players[winner].Name, reason, game.Players[winner].Tokens)})
}

// ==================== 자동 진행 타이머 ====================

// scheduleRoundEnd round_end 진입 시 다음 라운드 자동 진행 타이머를 건다.
// 발화는 허브 채널을 경유하므로 허브 밖에서 상태를 만지지 않는다.
func (h *LVHub) scheduleRoundEnd(room *lvRoom) {
	sig := lvRoundSignal{GameID: room.Game.ID, RoundNo: room.Game.RoundNo}
	room.RoundTimer = time.AfterFunc(lvRoundEndDelay, func() {
		h.roundFired <- sig
	})
}

func (h *LVHub) handleRoundFired(sig lvRoundSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.Game.Phase != LVPhaseRoundEnd || room.Game.RoundNo != sig.RoundNo {
		return
	}
	if err := room.Game.NextRound(h.rng); err != nil {
		return
	}
	game := room.Game
	first := game.CurrentSeat
	log.Printf("[러브레터][라운드시작] game=%s | %d라운드 | 선공=seat%d", game.ID, game.RoundNo, first)
	h.broadcastEvent(room, LVEventPayload{Kind: "round_started", Seat: &first,
		Name:    game.Players[first].Name,
		Message: fmt.Sprintf("%d라운드 시작 — %s님 선공", game.RoundNo, game.Players[first].Name)})
	h.broadcastState(room)
}

// resetAfkTimer 상태가 바뀔 때마다 AFK 타이머를 다시 건다.
// 턴 응답을 기다리는 playing 단계에서만 동작한다.
func (h *LVHub) resetAfkTimer(room *lvRoom) {
	room.AfkSeq++
	if room.AfkTimer != nil {
		room.AfkTimer.Stop()
		room.AfkTimer = nil
	}
	room.EndsAt = 0
	if room.Game.Phase != LVPhasePlaying {
		return
	}
	room.EndsAt = time.Now().Add(lvAfkTimeout).UnixMilli()
	sig := lvAfkSignal{GameID: room.Game.ID, Seq: room.AfkSeq}
	room.AfkTimer = time.AfterFunc(lvAfkTimeout, func() {
		h.afkFired <- sig
	})
}

// handleAfkFired AFK 타이머 발화 — 현재 턴 좌석(사람)의 수를 봇 두뇌로
// 1회 자동 실행한다. 좌석은 유지된다.
func (h *LVHub) handleAfkFired(sig lvAfkSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.AfkSeq != sig.Seq || room.Game.Phase != LVPhasePlaying {
		return
	}
	game := room.Game
	seat := game.CurrentSeat
	client := room.Clients[seat]
	if client == nil || client.Bot {
		return // 봇 좌석은 스스로 행동한다
	}

	brain := newLVBrain()
	state := h.buildLVState(room, seat)
	act := brain.decide(LVMessage{Type: LVMsgGameState, Payload: state})
	if act == nil {
		return
	}
	log.Printf("[러브레터][자동진행] game=%s | seat%d=%s 무응답 — %s 자동 실행",
		game.ID, seat, displayName(game.Players[seat].Name), act.Type)
	h.broadcastEvent(room, LVEventPayload{Kind: "afk", Seat: &seat,
		Name:    game.Players[seat].Name,
		Message: fmt.Sprintf("%s님이 오래 응답하지 않아 자동으로 진행합니다", game.Players[seat].Name)})
	h.handleGameMessage(LVGameMessage{Client: client, Message: *act})
}

// ==================== 상태 뷰 (은닉의 핵심) ====================

// buildLVState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를
// 쓴다. 은닉은 yourHand 뿐 — 관전자(viewerSeat -1)·타인은 필드 자체가 없다.
// discards 는 전원 공개 (카운팅 요소).
func (h *LVHub) buildLVState(room *lvRoom, viewerSeat int) LVGameStatePayload {
	game := room.Game

	players := []LVPlayerView{}
	tokens := []int{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		players = append(players, LVPlayerView{
			Seat:      p.Seat,
			Name:      p.Name,
			Connected: c != nil && c.Connected,
			Bot:       c != nil && c.Bot,
			Alive:     p.Alive,
			Protected: p.Protected,
			Tokens:    p.Tokens,
			Discards:  append([]int{}, p.Discards...),
		})
		tokens = append(tokens, p.Tokens)
	}

	// 좌석 보유자만 yourHand 필드를 받는다 (빈 손패도 [] — nil 금지)
	var yourHand *[]int
	if viewerSeat >= 0 && viewerSeat < len(game.Players) {
		hand := append([]int{}, game.Players[viewerSeat].Hand...)
		yourHand = &hand
	}

	endsAt := int64(0)
	if game.Phase == LVPhasePlaying {
		endsAt = room.EndsAt
	}

	return LVGameStatePayload{
		GameID:        game.ID,
		RoomCode:      room.Code,
		Phase:         game.Phase,
		HostSeat:      h.hostSeat(room),
		YourSeat:      viewerSeat,
		Spectators:    len(room.Spectators),
		EndsAt:        endsAt,
		YourHand:      yourHand,
		CurrentSeat:   game.CurrentSeat,
		DeckCount:     len(game.Deck),
		Tokens:        tokens,
		TargetTokens:  game.TargetTokens,
		Players:       players,
		RoundResult:   game.RoundResult,
		RemovedFaceUp: append([]int{}, game.RemovedFaceUp...),
	}
}

// broadcastState 좌석마다 개인화 스냅샷을 만들어 보낸다.
// 관전자에게는 공개 정보만 담긴 스냅샷(viewerSeat -1)이 간다.
// AFK 타이머를 먼저 리셋해야 스냅샷의 endsAt 이 새 마감 시각을 싣는다.
func (h *LVHub) broadcastState(room *lvRoom) {
	h.resetAfkTimer(room)
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, LVMessage{
			Type:    LVMsgGameState,
			Payload: h.buildLVState(room, seat),
		})
	}
	if len(room.Spectators) > 0 {
		msg := LVMessage{Type: LVMsgGameState, Payload: h.buildLVState(room, -1)}
		for c := range room.Spectators {
			h.sendToClient(c, msg)
		}
	}
}

func (h *LVHub) broadcastEvent(room *lvRoom, event LVEventPayload) {
	h.broadcastToRoom(room, LVMessage{Type: LVMsgEvent, Payload: event})
}

// finishGame 게임 종료 발표·전적 기록·방 정리
func (h *LVHub) finishGame(room *lvRoom) {
	game := room.Game
	if room.RoundTimer != nil {
		room.RoundTimer.Stop()
		room.RoundTimer = nil
	}
	if room.AfkTimer != nil {
		room.AfkTimer.Stop()
		room.AfkTimer = nil
	}

	winnerName := ""
	if game.WinnerSeat >= 0 && game.WinnerSeat < len(game.Players) {
		winnerName = game.Players[game.WinnerSeat].Name
	}
	tokens := []int{}
	names := []string{}
	for _, p := range game.Players {
		tokens = append(tokens, p.Tokens)
		names = append(names, displayName(p.Name))
	}

	h.broadcastEvent(room, LVEventPayload{Kind: "game_over", Seat: &game.WinnerSeat,
		Name:    winnerName,
		Message: fmt.Sprintf("%s님이 최종 승리했습니다!", winnerName)})
	// 최종 토큰이 반영된 스냅샷을 먼저 보낸 뒤 종료를 알린다
	// (봇 러너는 lv_game_over 를 보고 스스로 끝난다)
	h.broadcastState(room)
	h.broadcastToRoom(room, LVMessage{
		Type: LVMsgGameOver,
		Payload: LVGameOverPayload{
			WinnerSeat: game.WinnerSeat,
			WinnerName: winnerName,
			Tokens:     tokens,
			Players:    h.buildLVState(room, -1).Players,
		},
	})

	log.Printf("[러브레터][경기결과] game=%s | 승자=seat%d(%s) | 토큰=%v | 소요=%s",
		game.ID, game.WinnerSeat, displayName(winnerName), tokens, matchDuration(game.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "loveletter",
		Players:  strings.Join(names, " vs "),
		Winner:   displayName(winnerName),
		Reason:   "tokens",
		Duration: matchSeconds(game.StartedAt),
		Bot:      lvRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
	if room.Code != "" {
		delete(h.activeCodes, room.Code) // 코드 재사용 복귀
	}
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *LVHub) handleDisconnect(client *LVClient) {
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
	log.Printf("[러브레터][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, LVMessage{
		Type: LVMsgPlayerDisconnected,
		Payload: LVPlayerDisconnectedPayload{
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
func (h *LVHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == LVPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[러브레터][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := &LVClient{wsClient: newBotWSClient(), Hub: h, Seat: seat}
	bot.Name = client.Name // 좌석 이름은 유지 (표시는 bot 플래그로 구분)
	bot.GameID = room.Game.ID
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runLVBot(bot)

	h.broadcastEvent(room, LVEventPayload{Kind: "bot_takeover", Seat: &seat,
		Name:    client.Name,
		Message: fmt.Sprintf("%s님 자리를 봇이 이어받았습니다", client.Name)})
	// 새 봇이 이 스냅샷을 받아 자기 턴이면 즉시 이어간다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *LVHub) handleRejoin(client *LVClient, msg LVMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload LVRejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, LVMessage{Type: LVMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, LVMessage{Type: LVMsgSessionExpired})
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

	log.Printf("[러브레터][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, LVMessage{
		Type:    LVMsgPlayerReconnected,
		Payload: LVPlayerReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	// 전원에게 접속 상태가 반영된 스냅샷 (재접속 당사자 복원 포함)
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *LVHub) clearGameSessions(room *lvRoom) {
	clearRoomSessions(&h.sessionManager, room.Clients)
}

// ==================== 전송 ====================

func (h *LVHub) sendError(client *LVClient, message string) {
	h.sendToClient(client, LVMessage{Type: LVMsgError, Payload: LVErrorPayload{Message: message}})
}

func (h *LVHub) sendToClient(client *LVClient, message LVMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[LV] ")
}

func (h *LVHub) broadcastToRoom(room *lvRoom, message LVMessage) {
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

func ServeLVWs(hub *LVHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[LV] Error upgrading connection:", err)
		return
	}

	client := &LVClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[LV] ",
		func(msg LVMessage) { hub.gameMessage <- LVGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
