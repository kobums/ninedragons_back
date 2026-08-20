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

// AFK 자동 진행 대기 시간 — 2단계다. 스파이마스터가 힌트를 안 내면 봇 힌트,
// 요원들이 카드를 안 고르면 턴 종료 처리 (테스트에서 짧게 낮춘다).
var (
	cnClueTimeout  = 90 * time.Second
	cnGuessTimeout = 60 * time.Second
)

// cnRoom 게임(순수 상태)과 좌석별 연결의 매핑
type cnRoom struct {
	Game    *CNGame
	Clients map[int]*CNClient // seat → client

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string

	// AfkTimer 접속 유지 AFK 구제 타이머 — 상태가 바뀔 때마다 리셋되고,
	// 발화하면 단계별 자동 진행(봇 힌트 / 턴 종료)을 1회 실행한다.
	AfkTimer *time.Timer
	// AfkSeq 상태 변경 일련번호 (뒤늦은 발화 무시용)
	AfkSeq int
	// EndsAt 현재 단계의 AFK 마감 시각 (unixMillis) — 스냅샷 노출용
	EndsAt int64

	// Spectators 관전자 연결 (사설 방 코드로만 진입, 상한 maxSpectators).
	// 좌석·세션이 없어 재접속을 지원하지 않는다 — 끊기면 목록에서만 뗀다.
	Spectators map[*CNClient]bool

	// LastReact 좌석별 마지막 리액션 발신 시각 (레이트리밋 장부)
	LastReact map[int]time.Time
}

// cnAfkSignal AFK 타이머 발화 표식
type cnAfkSignal struct {
	GameID string
	Seq    int
}

type CNHub struct {
	// 등록된 클라이언트
	clients map[*CNClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*cnRoom

	// 글로벌 로비. 시작 전 공용 방은 하나뿐이다.
	lobby *cnRoom

	// 사설 방 (초대 코드 → 시작 전 방). 허브 고루틴에서만 접근한다.
	// 시작하면 privateLobbies 에서 activeCodes 로 옮긴다.
	privateLobbies map[string]*cnRoom

	// 진행 중 사설 방 (초대 코드 → gameID). 시작 후에도 코드로 방을 찾아
	// 관전 입장시키는 근거다. finishGame 에서 해제한다 (코드 재사용 복귀).
	activeCodes map[string]string

	// 클라이언트 등록
	register chan *CNClient

	// 클라이언트 등록 해제
	unregister chan *CNClient

	// 게임 메시지
	gameMessage chan CNGameMessage

	// 자동 진행 알림 (time.AfterFunc → 허브 채널 경유)
	afkFired chan cnAfkSignal

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*CNClient]

	// 셔플·키 카드 배치용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type CNGameMessage struct {
	Client  *CNClient
	Message CNMessage
}

func NewCNHub() *CNHub {
	return &CNHub{
		register:       make(chan *CNClient),
		unregister:     make(chan *CNClient),
		clients:        make(map[*CNClient]bool),
		rooms:          make(map[string]*cnRoom),
		privateLobbies: make(map[string]*cnRoom),
		activeCodes:    make(map[string]string),
		gameMessage:    make(chan CNGameMessage),
		afkFired:       make(chan cnAfkSignal, 8),
		sessionManager: newSessionManager[*CNClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *CNHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[CN] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[CN] Client unregistered: %s", client.ID)
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

func (h *CNHub) handleGameMessage(gm CNGameMessage) {
	// 관전자는 어떤 행동도 할 수 없다 (보기 전용 — 리액션도 좌석 보유자만)
	if h.isSpectator(gm.Client) {
		h.sendError(gm.Client, spectatorDeniedMsg)
		return
	}
	switch gm.Message.Type {
	case CNMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case CNMsgReact:
		h.handleReact(gm.Client, gm.Message)
	case CNMsgFillBots:
		h.handleFillBots(gm.Client)
	case CNMsgStart:
		h.handleStart(gm.Client)
	case CNMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case CNMsgClue:
		h.handleClue(gm.Client, gm.Message)
	case CNMsgPick:
		h.handlePick(gm.Client, gm.Message)
	case CNMsgEndTurn:
		h.handleEndTurn(gm.Client)
	}
}

// ==================== 대기실 ====================

func (h *CNHub) handleJoinGame(client *CNClient, msg CNMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload CNJoinGamePayload
	json.Unmarshal(payloadBytes, &payload)

	// 이미 시작된 사설 방의 코드로 들어오면 에러 대신 관전자로 입장시킨다
	if gameID, ok := h.activeCodes[normalizeRoomCode(payload.Room)]; ok {
		h.addSpectator(h.rooms[gameID], client, payload.Name)
		return
	}

	room := h.lobbyRoomFor(payload.Room)
	seat, err := room.Game.AddPlayer(payload.Name, false)
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

	log.Printf("[코드네임][입장] game=%s | seat%d=%s 대기 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), CNMaxPlayers)
	if room.Code == "" { // 사설 방 입장은 조용히 (공용 로비만 알림)
		notify("코드네임 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), len(room.Game.Players), CNMaxPlayers))
	}

	h.sendToClient(client, CNMessage{
		Type: CNMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
			"roomCode":  room.Code,
		},
	})

	h.broadcastEvent(room, CNEventPayload{Kind: "joined", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 입장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	// 인원이 차도 자동 시작하지 않는다 — 호스트 명시 시작만
	h.broadcastState(room)
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다.
// 생략/빈 문자열은 기존 공용 로비 경로 그대로, "NEW"는 새 코드 발급,
// 그 외 코드는 해당 사설 방 (없으면 그 코드로 관대하게 새로 생성).
func (h *CNHub) lobbyRoomFor(roomField string) *cnRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewCNGame(uuid.New().String())
			h.lobby = &cnRoom{Game: game, Clients: map[int]*CNClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[CN] Created lobby game %s", game.ID)
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
		game := NewCNGame(uuid.New().String())
		room = &cnRoom{Game: game, Clients: map[int]*CNClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[CN] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// ==================== 관전 / 리액션 ====================

// addSpectator 진행 중(또는 가득 찬) 사설 방에 관전자로 등록한다.
// 좌석·세션 없음 — 재접속 미지원, 끊기면 다시 코드로 들어온다.
// 관전자는 키 카드 없이 공개 보드만 본다.
func (h *CNHub) addSpectator(room *cnRoom, client *CNClient, name string) {
	if len(room.Spectators) >= maxSpectators {
		h.sendError(client, spectatorFullMsg)
		return
	}
	if room.Spectators == nil {
		room.Spectators = map[*CNClient]bool{}
	}
	client.Name = name
	client.GameID = room.Game.ID
	room.Spectators[client] = true

	log.Printf("[코드네임][관전] game=%s | %s 관전 입장 (%d명)",
		room.Game.ID, displayName(name), len(room.Spectators))

	h.sendToClient(client, CNMessage{
		Type:    CNMsgSpectateJoined,
		Payload: map[string]interface{}{"gameId": room.Game.ID, "roomCode": room.Code},
	})
	// 전원(관전자 포함)에게 관전자 수가 반영된 스냅샷 — 신규 관전자의 첫 스냅샷 겸용
	h.broadcastState(room)
}

// isSpectator 관전자 연결인지 (좌석 보유자·미입장 연결은 false)
func (h *CNHub) isSpectator(client *CNClient) bool {
	if client.GameID == "" {
		return false
	}
	room := h.rooms[client.GameID]
	return room != nil && room.Spectators[client]
}

// handleReact 리액션 이모지 — 좌석 보유자만, waiting 중에도 허용.
// 화이트리스트 외·레이트리밋 초과는 조용히 무시한다 (상태 저장 없음).
func (h *CNHub) handleReact(client *CNClient, msg CNMessage) {
	room := h.rooms[client.GameID]
	if room == nil || client.Seat < 0 {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload CNReactPayload
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
	h.broadcastEvent(room, CNEventPayload{Kind: "react", Seat: &seat, Name: client.Name, Message: payload.Emoji})
}

// waitingRoomOf 클라이언트가 속한 시작 전 방 (공용 로비 또는 사설 방)
func (h *CNHub) waitingRoomOf(client *CNClient) *cnRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Ready {
		return nil
	}
	return room
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *CNHub) hostSeat(room *cnRoom) int {
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

// cnHumanCount 방의 사람 수
func cnHumanCount(room *cnRoom) int {
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
func (h *CNHub) updateLobbyWaiting(room *cnRoom) {
	if room.Code != "" {
		return
	}
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && cnHumanCount(room) >= 1
	lobbySetWaiting("codenames", waiting)
}

// handleFillBots host 가 빈 좌석을 연습봇으로 6인까지 채운다.
// 봇은 요원 전담이지만 사람이 1명뿐인 팀에서는 봇이 스파이마스터를 맡아
// 무작위 힌트를 낸다 (역할 배정은 게임 쪽 assignRoles 규칙).
// 자동 시작하지 않는다 — 호스트가 명시적으로 시작한다.
func (h *CNHub) handleFillBots(client *CNClient) {
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
	for len(room.Game.Players) < CNBotFillTarget {
		botNo++
		if !h.spawnCNBot(room, fmt.Sprintf("%s%d", botName, botNo)) {
			break
		}
	}
	h.broadcastState(room)
}

func (h *CNHub) handleStart(client *CNClient) {
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
		h.sendError(client, "4명 이상 모여야 시작할 수 있습니다")
		return
	}
	h.startGame(room)
}

func (h *CNHub) startGame(room *cnRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	if room.Code != "" {
		// 시작한 사설 방의 코드는 진행 중 장부로 옮긴다 (관전 입장 근거)
		delete(h.privateLobbies, room.Code)
		h.activeCodes[room.Code] = room.Game.ID
	} else {
		h.lobby = nil // 시작한 방은 로비에서 떼어낸다
		lobbySetWaiting("codenames", false)
	}

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	log.Printf("[코드네임][경기시작] game=%s | 인원=%d | 선공=적팀(9단어) | %v",
		room.Game.ID, len(room.Game.Players), names)
	if !cnRoomHasBot(room) { // 연습봇전은 운영자 알림을 억제한다
		notify("코드네임 게임 시작", fmt.Sprintf("%d인전 시작", len(room.Game.Players)))
	}

	h.broadcastEvent(room, CNEventPayload{Kind: "game_started",
		Message: fmt.Sprintf("게임 시작 — %s 선공 (9단어 대 8단어)", cnTeamName(CNTeamRed))})
	h.broadcastState(room)
}

// removeFromLobby 대기실에서 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *CNHub) removeFromLobby(room *cnRoom, client *CNClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	// 좌석 압축: oldSeat 뒤의 클라이언트를 한 칸씩 당긴다
	rebuilt := map[int]*CNClient{}
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

	log.Printf("[코드네임][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), CNMaxPlayers)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if cnHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, CNMessage{Type: CNMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			lobbySetWaiting("codenames", false)
		}
		return
	}

	h.broadcastEvent(room, CNEventPayload{Kind: "left", Name: client.Name,
		Message: fmt.Sprintf("%s님이 퇴장했습니다", client.Name)})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *CNHub) roomOf(client *CNClient) *cnRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

func (h *CNHub) handleClue(client *CNClient, msg CNMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload CNCluePayload
	json.Unmarshal(payloadBytes, &payload)

	h.applyClue(room, client, payload.Word, payload.Count)
}

// applyClue 힌트 기록의 공통 경로 (사람 입력·AFK 봇 힌트 겸용)
func (h *CNHub) applyClue(room *cnRoom, client *CNClient, word string, count int) {
	game := room.Game
	if err := game.GiveClue(client.Seat, word, count); err != nil {
		h.sendError(client, err.Error())
		return
	}
	clue := game.Clue
	seat := client.Seat

	log.Printf("[코드네임][힌트] game=%s | seat%d=%s(%s) → %q %d",
		game.ID, seat, displayName(client.Name), cnTeamName(game.CurrentTeam), clue.Word, clue.Count)

	h.broadcastEvent(room, CNEventPayload{Kind: "clue", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s 힌트 — %s %d (선택 %d회)",
			cnTeamName(game.CurrentTeam), clue.Word, clue.Count, clue.Remaining)})
	h.broadcastState(room)
}

func (h *CNHub) handlePick(client *CNClient, msg CNMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload CNPickPayload
	json.Unmarshal(payloadBytes, &payload)

	game := room.Game
	pickerTeam := game.CurrentTeam
	res, err := game.Pick(client.Seat, payload.Index)
	if err != nil {
		h.sendError(client, err.Error())
		return
	}

	log.Printf("[코드네임][선택] game=%s | seat%d=%s(%s) → %q = %s",
		game.ID, client.Seat, displayName(client.Name), cnTeamName(pickerTeam), res.Word, res.Color)

	seat := res.Seat
	message := ""
	switch {
	case res.Color == CNColorAssassin:
		message = fmt.Sprintf("%s님이 %q을(를) 선택 — 암살자! %s 즉시 패배",
			client.Name, res.Word, cnTeamName(pickerTeam))
	case res.Correct && !res.TurnEnded && !res.GameOver:
		message = fmt.Sprintf("%s님이 %q을(를) 선택 — %s 정답! 계속 선택할 수 있습니다",
			client.Name, res.Word, cnTeamName(pickerTeam))
	case res.Correct:
		message = fmt.Sprintf("%s님이 %q을(를) 선택 — %s 정답!",
			client.Name, res.Word, cnTeamName(pickerTeam))
	case res.Color == CNColorNeutral:
		message = fmt.Sprintf("%s님이 %q을(를) 선택 — 중립 단어, 턴 종료",
			client.Name, res.Word)
	default: // 상대 팀 단어
		message = fmt.Sprintf("%s님이 %q을(를) 선택 — %s입니다, 턴 종료",
			client.Name, res.Word, cnColorName(res.Color))
	}
	h.broadcastEvent(room, CNEventPayload{Kind: "pick", Seat: &seat, Name: client.Name, Message: message})

	if res.GameOver {
		h.broadcastState(room)
		h.finishGame(room)
		return
	}
	if res.TurnEnded {
		h.broadcastEvent(room, CNEventPayload{Kind: "turn_end",
			Message: fmt.Sprintf("%s 힌트 차례입니다", cnTeamName(game.CurrentTeam))})
	}
	h.broadcastState(room)
}

func (h *CNHub) handleEndTurn(client *CNClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	game := room.Game
	if err := game.EndTurn(client.Seat); err != nil {
		h.sendError(client, err.Error())
		return
	}
	seat := client.Seat
	log.Printf("[코드네임][그만] game=%s | seat%d=%s 턴 종료 선언",
		game.ID, seat, displayName(client.Name))
	h.broadcastEvent(room, CNEventPayload{Kind: "turn_end", Seat: &seat, Name: client.Name,
		Message: fmt.Sprintf("%s님이 턴을 넘겼습니다 — %s 힌트 차례입니다",
			client.Name, cnTeamName(game.CurrentTeam))})
	h.broadcastState(room)
}

// ==================== 자동 진행 타이머 (AFK 2단계) ====================

// resetAfkTimer 상태가 바뀔 때마다 AFK 타이머를 다시 건다.
// clue 단계는 스파이마스터 힌트 대기(90초), guess 단계는 요원 선택
// 대기(60초)로 서로 다른 마감을 쓴다.
func (h *CNHub) resetAfkTimer(room *cnRoom) {
	room.AfkSeq++
	if room.AfkTimer != nil {
		room.AfkTimer.Stop()
		room.AfkTimer = nil
	}
	room.EndsAt = 0

	var timeout time.Duration
	switch room.Game.Phase {
	case CNPhaseClue:
		timeout = cnClueTimeout
	case CNPhaseGuess:
		timeout = cnGuessTimeout
	default:
		return
	}
	room.EndsAt = time.Now().Add(timeout).UnixMilli()
	sig := cnAfkSignal{GameID: room.Game.ID, Seq: room.AfkSeq}
	room.AfkTimer = time.AfterFunc(timeout, func() {
		h.afkFired <- sig
	})
}

// handleAfkFired AFK 타이머 발화 — clue 단계면 무응답 스파이마스터 대신
// 봇 힌트를 내고, guess 단계면 턴을 종료 처리한다. 좌석은 유지된다.
func (h *CNHub) handleAfkFired(sig cnAfkSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.AfkSeq != sig.Seq {
		return
	}
	game := room.Game

	switch game.Phase {
	case CNPhaseClue:
		seat := game.SpymasterSeat(game.CurrentTeam)
		client := room.Clients[seat]
		if client == nil || client.Bot {
			return // 봇 좌석은 스스로 행동한다
		}
		word, count := cnBotClueValue(h.rng)
		log.Printf("[코드네임][자동진행] game=%s | seat%d=%s 힌트 무응답 — 봇 힌트 %q %d",
			game.ID, seat, displayName(client.Name), word, count)
		h.broadcastEvent(room, CNEventPayload{Kind: "afk", Seat: &seat, Name: client.Name,
			Message: fmt.Sprintf("%s님이 오래 응답하지 않아 자동 힌트로 진행합니다", client.Name)})
		h.applyClue(room, client, word, count)

	case CNPhaseGuess:
		if cnTeamAgentsAllBots(room, game.CurrentTeam) {
			return // 봇 요원들은 스스로 행동한다
		}
		team := game.CurrentTeam
		if !game.ForceEndTurn() {
			return
		}
		log.Printf("[코드네임][자동진행] game=%s | %s 요원 무응답 — 턴 종료 처리",
			game.ID, cnTeamName(team))
		h.broadcastEvent(room, CNEventPayload{Kind: "afk",
			Message: fmt.Sprintf("%s 요원들이 오래 응답하지 않아 턴을 넘깁니다 — %s 힌트 차례입니다",
				cnTeamName(team), cnTeamName(game.CurrentTeam))})
		h.broadcastState(room)
	}
}

// cnTeamAgentsAllBots 팀의 요원이 전부 봇인지 (AFK 개입 필요 판단)
func cnTeamAgentsAllBots(room *cnRoom, team CNTeam) bool {
	for _, p := range room.Game.Players {
		if p.Team != team || p.Role != CNRoleAgent {
			continue
		}
		c := room.Clients[p.Seat]
		if c != nil && !c.Bot {
			return false
		}
	}
	return true
}

// ==================== 상태 뷰 (은닉의 핵심) ====================

// buildCNState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를
// 쓴다. 은닉은 keyCard 뿐 — 스파이마스터에게만 실리고 요원·관전자
// (viewerSeat -1)는 필드 자체가 없다. 미공개 카드의 color 는 빈 값이다.
func (h *CNHub) buildCNState(room *cnRoom, viewerSeat int) CNGameStatePayload {
	game := room.Game

	players := []CNPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		players = append(players, CNPlayerView{
			Seat:      p.Seat,
			Name:      p.Name,
			Connected: c != nil && c.Connected,
			Bot:       c != nil && c.Bot,
			Team:      p.Team,
			Role:      p.Role,
		})
	}

	board := []CNCardView{}
	for i, card := range game.Board {
		view := CNCardView{Word: card.Word, Revealed: card.Revealed}
		if card.Revealed {
			view.Color = game.KeyCard[i]
		}
		board = append(board, view)
	}

	yourTeam := CNTeam("")
	yourRole := CNRole("")
	var keyCard []CNColor // nil = 필드 부재 (omitempty)
	if viewer := game.playerAt(viewerSeat); viewer != nil {
		yourTeam = viewer.Team
		yourRole = viewer.Role
		if viewer.Role == CNRoleSpymaster && len(game.KeyCard) > 0 {
			keyCard = append([]CNColor{}, game.KeyCard...)
		}
	}

	var clue *CNClueView
	if game.Clue != nil {
		clue = &CNClueView{Word: game.Clue.Word, Count: game.Clue.Count, Remaining: game.Clue.Remaining}
	}

	endsAt := int64(0)
	if game.Phase == CNPhaseClue || game.Phase == CNPhaseGuess {
		endsAt = room.EndsAt
	}

	return CNGameStatePayload{
		GameID:      game.ID,
		RoomCode:    room.Code,
		Phase:       game.Phase,
		HostSeat:    h.hostSeat(room),
		YourSeat:    viewerSeat,
		Spectators:  len(room.Spectators),
		EndsAt:      endsAt,
		CurrentTeam: game.CurrentTeam,
		YourTeam:    yourTeam,
		YourRole:    yourRole,
		KeyCard:     keyCard,
		Board:       board,
		Clue:        clue,
		ClueHistory: append([]CNClueEntry{}, game.ClueHistory...),
		RedLeft:     game.RedLeft,
		BlueLeft:    game.BlueLeft,
		Players:     players,
		Winner:      game.Winner,
		LoseReason:  game.LoseReason,
	}
}

// broadcastState 좌석마다 개인화 스냅샷을 만들어 보낸다.
// 관전자에게는 공개 정보만 담긴 스냅샷(viewerSeat -1)이 간다.
// AFK 타이머를 먼저 리셋해야 스냅샷의 endsAt 이 새 마감 시각을 싣는다.
func (h *CNHub) broadcastState(room *cnRoom) {
	h.resetAfkTimer(room)
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, CNMessage{
			Type:    CNMsgGameState,
			Payload: h.buildCNState(room, seat),
		})
	}
	if len(room.Spectators) > 0 {
		msg := CNMessage{Type: CNMsgGameState, Payload: h.buildCNState(room, -1)}
		for c := range room.Spectators {
			h.sendToClient(c, msg)
		}
	}
}

func (h *CNHub) broadcastEvent(room *cnRoom, event CNEventPayload) {
	h.broadcastToRoom(room, CNMessage{Type: CNMsgEvent, Payload: event})
}

// finishGame 게임 종료 발표·전적 기록·방 정리
func (h *CNHub) finishGame(room *cnRoom) {
	game := room.Game
	if room.AfkTimer != nil {
		room.AfkTimer.Stop()
		room.AfkTimer = nil
	}

	winners := []string{}
	losers := []string{}
	for _, p := range game.Players {
		if p.Team == game.Winner {
			winners = append(winners, displayName(p.Name))
		} else {
			losers = append(losers, displayName(p.Name))
		}
	}
	reason := "words"
	overMessage := fmt.Sprintf("%s이 모든 팀 단어를 찾아 승리했습니다!", cnTeamName(game.Winner))
	if game.LoseReason == "assassin" {
		reason = "assassin"
		overMessage = fmt.Sprintf("%s이 암살자를 피해 승리했습니다! (%s 암살자 선택)",
			cnTeamName(game.Winner), cnTeamName(cnOtherTeam(game.Winner)))
	}

	h.broadcastEvent(room, CNEventPayload{Kind: "game_over", Message: overMessage})
	// 최종 보드가 반영된 스냅샷을 먼저 보낸 뒤 종료를 알린다
	// (봇 러너는 cn_game_over 를 보고 스스로 끝난다)
	h.broadcastState(room)
	h.broadcastToRoom(room, CNMessage{
		Type: CNMsgGameOver,
		Payload: CNGameOverPayload{
			Winner:     game.Winner,
			LoseReason: game.LoseReason,
			RedLeft:    game.RedLeft,
			BlueLeft:   game.BlueLeft,
			Players:    h.buildCNState(room, -1).Players,
		},
	})

	log.Printf("[코드네임][경기결과] game=%s | 승리=%s | 사유=%s | 잔여 적%d/청%d | 소요=%s",
		game.ID, cnTeamName(game.Winner), reason, game.RedLeft, game.BlueLeft,
		matchDuration(game.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "codenames",
		Players:  strings.Join(winners, "·") + " vs " + strings.Join(losers, "·"),
		Winner:   strings.Join(winners, "·"),
		Reason:   reason,
		Duration: matchSeconds(game.StartedAt),
		Bot:      cnRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
	if room.Code != "" {
		delete(h.activeCodes, room.Code) // 코드 재사용 복귀
	}
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *CNHub) handleDisconnect(client *CNClient) {
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
	log.Printf("[코드네임][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, CNMessage{
		Type: CNMsgPlayerDisconnected,
		Payload: CNPlayerDisconnectedPayload{
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
func (h *CNHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == CNPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[코드네임][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := &CNClient{wsClient: newBotWSClient(), Hub: h, Seat: seat}
	bot.Name = client.Name // 좌석 이름은 유지 (표시는 bot 플래그로 구분)
	bot.GameID = room.Game.ID
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	if p := room.Game.playerAt(seat); p != nil {
		p.IsBot = true // 역할은 유지 — 스파이마스터 좌석이면 봇 힌트가 이어간다
	}
	h.runCNBot(bot)

	h.broadcastEvent(room, CNEventPayload{Kind: "bot_takeover", Seat: &seat,
		Name:    client.Name,
		Message: fmt.Sprintf("%s님 자리를 봇이 이어받았습니다", client.Name)})
	// 새 봇이 이 스냅샷을 받아 자기 차례면 즉시 이어간다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *CNHub) handleRejoin(client *CNClient, msg CNMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload CNRejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, CNMessage{Type: CNMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, CNMessage{Type: CNMsgSessionExpired})
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

	log.Printf("[코드네임][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, CNMessage{
		Type:    CNMsgPlayerReconnected,
		Payload: CNPlayerReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	// 전원에게 접속 상태가 반영된 스냅샷 (재접속 당사자 복원 포함)
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *CNHub) clearGameSessions(room *cnRoom) {
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		h.drop(c.SessionID)
	}
}

// ==================== 전송 ====================

func (h *CNHub) sendError(client *CNClient, message string) {
	h.sendToClient(client, CNMessage{Type: CNMsgError, Payload: CNErrorPayload{Message: message}})
}

func (h *CNHub) sendToClient(client *CNClient, message CNMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[CN] ")
}

func (h *CNHub) broadcastToRoom(room *cnRoom, message CNMessage) {
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

func ServeCNWs(hub *CNHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[CN] Error upgrading connection:", err)
		return
	}

	client := &CNClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[CN] ",
		func(msg CNMessage) { hub.gameMessage <- CNGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
