package server

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// 단계별 AFK 타임아웃 — 접속만 유지한 채 제출하지 않는 좌석이 게임을 영구
// 정지시키지 않게, 만료 시 자동 행동으로 해소한다 (테스트에서 짧게 낮춘다).
//   - team_pick: 리더 미지명 → 무작위 합법 지명
//   - team_vote: 미제출자 찬성 처리 (부결 루프 방지)
//   - quest: 미제출 원정대원 성공 카드 처리
//   - assassin: 무작위 선 플레이어 지목
var (
	avPickTimeout     = 90 * time.Second
	avTeamVoteTimeout = 60 * time.Second
	avQuestTimeout    = 60 * time.Second
	avAssassinTimeout = 90 * time.Second
)

// avRoom 게임(순수 상태)과 좌석별 연결의 매핑
type avRoom struct {
	Game       *AVGame
	Clients    map[int]*AVClient // seat → client
	PhaseTimer *time.Timer       // 단계 타임아웃 타이머

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string
}

// avPhaseSignal 타임아웃 타이머의 발화 표식. 아발론은 같은 (phase, round)가
// 반복될 수 있어(부결 후 재지명) 단계 전환 일련번호(Seq)로 구분한다.
type avPhaseSignal struct {
	GameID string
	Seq    int
}

type AVHub struct {
	// 등록된 클라이언트
	clients map[*AVClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*avRoom

	// 글로벌 로비. 시작 전 공용 방은 하나뿐이다.
	lobby *avRoom

	// 사설 방 (초대 코드 → 시작 전 방). 허브 고루틴에서만 접근한다.
	// 시작하면 rooms 로만 유지하고 여기서는 뗀다 (코드 재사용 가능).
	privateLobbies map[string]*avRoom

	// 클라이언트 등록
	register chan *AVClient

	// 클라이언트 등록 해제
	unregister chan *AVClient

	// 게임 메시지
	gameMessage chan AVGameMessage

	// 단계 타임아웃 발화 (time.AfterFunc → 허브 채널 경유)
	phaseFired chan avPhaseSignal

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*AVClient]

	// 셔플·자동 행동용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type AVGameMessage struct {
	Client  *AVClient
	Message AVMessage
}

func NewAVHub() *AVHub {
	return &AVHub{
		register:       make(chan *AVClient),
		unregister:     make(chan *AVClient),
		clients:        make(map[*AVClient]bool),
		rooms:          make(map[string]*avRoom),
		privateLobbies: make(map[string]*avRoom),
		gameMessage:    make(chan AVGameMessage),
		phaseFired:     make(chan avPhaseSignal, 8),
		sessionManager: newSessionManager[*AVClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *AVHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[AV] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[AV] Client unregistered: %s", client.ID)
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

func (h *AVHub) handleGameMessage(gm AVGameMessage) {
	switch gm.Message.Type {
	case AVMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case AVMsgFillBots:
		h.handleFillBots(gm.Client)
	case AVMsgStart:
		h.handleStart(gm.Client)
	case AVMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case AVMsgPick:
		h.handlePick(gm.Client, gm.Message)
	case AVMsgTeamVote:
		h.handleTeamVote(gm.Client, gm.Message)
	case AVMsgQuest:
		h.handleQuest(gm.Client, gm.Message)
	case AVMsgAssassinate:
		h.handleAssassinate(gm.Client, gm.Message)
	}
}

// ==================== 로비 ====================

func (h *AVHub) handleJoinGame(client *AVClient, msg AVMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload AVJoinGamePayload
	json.Unmarshal(payloadBytes, &payload)

	room := h.lobbyRoomFor(payload.Room)
	seat, err := room.Game.AddPlayer(payload.Name)
	if err != nil {
		h.sendError(client, err.Error())
		return
	}

	client.Name = payload.Name
	client.SessionID = uuid.New().String()
	client.GameID = room.Game.ID
	client.Seat = seat
	h.sessions[client.SessionID] = client
	room.Clients[seat] = client

	log.Printf("[아발론][입장] game=%s | seat%d=%s 게임 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), AVMaxPlayers)
	if room.Code == "" { // 사설 방 입장은 조용히 (공용 로비만 알림)
		notify("아발론 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), len(room.Game.Players), AVMaxPlayers))
	}

	h.sendToClient(client, AVMessage{
		Type: AVMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
			"roomCode":  room.Code,
		},
	})

	h.broadcastEvent(room, AVEventPayload{Kind: "joined", Seat: &seat, Name: client.Name})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다.
// 생략/빈 문자열은 기존 공용 로비 경로 그대로, "NEW"는 새 코드 발급,
// 그 외 코드는 해당 사설 방 (없으면 그 코드로 관대하게 새로 생성).
func (h *AVHub) lobbyRoomFor(roomField string) *avRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewAVGame(uuid.New().String())
			h.lobby = &avRoom{Game: game, Clients: map[int]*AVClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[AV] Created lobby game %s", game.ID)
		}
		return h.lobby
	}
	if code == roomCodeNew {
		code = generateRoomCode(h.rng, takenCodes(h.privateLobbies))
	}
	room := h.privateLobbies[code]
	if room == nil {
		game := NewAVGame(uuid.New().String())
		room = &avRoom{Game: game, Clients: map[int]*AVClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[AV] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// waitingRoomOf 클라이언트가 속한 시작 전 방 (공용 로비 또는 사설 방)
func (h *AVHub) waitingRoomOf(client *AVClient) *avRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Ready {
		return nil
	}
	return room
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *AVHub) hostSeat(room *avRoom) int {
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

// handleFillBots host 가 최소 성립 인원(5)까지 연습봇으로 채운다.
// 봇은 연습용이라 정원(10)까지 채우지 않는다. 시작은 별도의 av_start.
func (h *AVHub) handleFillBots(client *AVClient) {
	room := h.waitingRoomOf(client)
	if room == nil {
		h.sendError(client, "로비를 찾을 수 없습니다")
		return
	}
	if client.Seat != h.hostSeat(room) {
		h.sendError(client, "호스트만 봇을 채울 수 있습니다")
		return
	}
	if len(room.Game.Players) >= AVBotFillTarget {
		h.sendError(client, fmt.Sprintf("%d명 미만일 때만 봇을 채울 수 있습니다", AVBotFillTarget))
		return
	}

	botNo := 0
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			botNo++
		}
	}
	for len(room.Game.Players) < AVBotFillTarget {
		botNo++
		if h.spawnBot(room, fmt.Sprintf("%s%d", botName, botNo)) == nil {
			break
		}
	}
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

func (h *AVHub) handleStart(client *AVClient) {
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
		h.sendError(client, fmt.Sprintf("%d명 이상 모여야 시작할 수 있습니다", AVMinPlayers))
		return
	}
	h.startGame(room)
}

// avHumanCount 방의 사람 수
func avHumanCount(room *avRoom) int {
	n := 0
	for _, c := range room.Clients {
		if c != nil && !c.Bot {
			n++
		}
	}
	return n
}

// avRoomHasBot 방에 연습봇이 있는지 (전적 기록용)
func avRoomHasBot(room *avRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}

// updateLobbyWaiting 로비 현황판 갱신 — 사람 1명 이상 대기 && 미시작.
// 사설 방은 현황판에 노출하지 않는다 (초대 링크로만 접근).
func (h *AVHub) updateLobbyWaiting(room *avRoom) {
	if room.Code != "" {
		return
	}
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && avHumanCount(room) >= 1
	lobbySetWaiting("avalon", waiting)
}

func (h *AVHub) startGame(room *avRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	if room.Code != "" {
		delete(h.privateLobbies, room.Code) // 시작한 사설 방은 코드 장부에서 뗀다
	} else {
		h.lobby = nil // 시작한 방은 로비에서 떼어낸다
		lobbySetWaiting("avalon", false)
	}

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	log.Printf("[아발론][경기시작] game=%s | 인원=%d | %v",
		room.Game.ID, len(room.Game.Players), names)
	// 봇 채우기로 시작한 판도 포함해 시작 시점에 1회만 알린다
	notify("아발론 게임 시작", fmt.Sprintf("%d인전 시작", len(room.Game.Players)))

	h.broadcastEvent(room, AVEventPayload{Kind: "started"})
	h.beginTeamPick(room)
}

// beginTeamPick 지명 단계 진입 발표 + 마감 세팅 + 스냅샷.
// 마감 시각을 먼저 세팅해야 스냅샷에 endsAt 이 실린다.
func (h *AVHub) beginTeamPick(room *avRoom) {
	game := room.Game
	leader := game.LeaderSeat
	h.broadcastEvent(room, AVEventPayload{Kind: "team_pick", Seat: &leader,
		Name: game.Players[leader].Name,
		Message: fmt.Sprintf("%d라운드 원정대 지명 — %s님이 리더입니다 (%d명)",
			game.Round, game.Players[leader].Name, game.CurrentQuestSize())})
	h.scheduleDeadline(room, avPickTimeout)
	h.broadcastState(room)
}

// removeFromLobby 대기 중 이탈 — 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *AVHub) removeFromLobby(room *avRoom, client *AVClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	rebuilt := map[int]*AVClient{}
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

	log.Printf("[아발론][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), AVMaxPlayers)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if avHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, AVMessage{Type: AVMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			lobbySetWaiting("avalon", false)
		}
		return
	}

	h.broadcastEvent(room, AVEventPayload{Kind: "left", Name: client.Name})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *AVHub) roomOf(client *AVClient) *avRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

func (h *AVHub) handlePick(client *AVClient, msg AVMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload AVPickPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.SubmitPick(client.Seat, payload.Seats); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.afterTeamProposed(room)
}

// afterTeamProposed 지명 확정(수동·자동 공통) — 팀 발표 후 투표 단계로
func (h *AVHub) afterTeamProposed(room *avRoom) {
	game := room.Game
	leader := game.LeaderSeat
	log.Printf("[아발론][지명] game=%s | %d라운드 | 리더 seat%d 원정대 %v",
		game.ID, game.Round, leader, game.Team)
	h.broadcastEvent(room, AVEventPayload{Kind: "team_proposed", Seat: &leader,
		Name:    game.Players[leader].Name,
		Message: fmt.Sprintf("%s님이 원정대를 지명했습니다 — 찬반 투표를 시작합니다", game.Players[leader].Name)})
	h.scheduleDeadline(room, avTeamVoteTimeout)
	h.broadcastState(room)
}

func (h *AVHub) handleTeamVote(client *AVClient, msg AVMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload AVTeamVotePayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.SubmitTeamVote(client.Seat, payload.Approve); err != nil {
		h.sendError(client, err.Error())
		return
	}
	if room.Game.TeamVoteComplete() {
		h.resolveTeamVote(room)
		return
	}
	// 표 내용은 비공개 — 제출 여부(votedTeam)만 실시간 반영
	h.broadcastState(room)
}

// resolveTeamVote 팀 투표 해소 — 표를 일괄 공개하고 승인/부결을 판정한다
func (h *AVHub) resolveTeamVote(room *avRoom) {
	game := room.Game
	h.stopPhaseTimer(room)
	game.Deadline = 0

	approves := 0
	for _, v := range game.TeamVotes {
		if v {
			approves++
		}
	}
	approved := game.ResolveTeamVote()

	if approved {
		log.Printf("[아발론][팀투표] game=%s | %d라운드 | 승인 (%d/%d 찬성)",
			game.ID, game.Round, approves, len(game.Players))
		h.broadcastEvent(room, AVEventPayload{Kind: "team_approved",
			Message: fmt.Sprintf("원정대가 승인되었습니다 (찬성 %d표) — 원정 카드를 제출하세요", approves)})
		h.scheduleDeadline(room, avQuestTimeout)
		h.broadcastState(room)
		return
	}

	if game.Phase == AVPhaseGameOver {
		// 연속 5부결 — 악 즉시 승
		log.Printf("[아발론][팀투표] game=%s | %d라운드 | 부결 5연속 — 악의 세력 승",
			game.ID, game.Round)
		h.broadcastEvent(room, AVEventPayload{Kind: "team_rejected",
			Message: "원정대가 5연속 부결되었습니다 — 악의 세력 승리"})
		h.finishGame(room)
		return
	}

	log.Printf("[아발론][팀투표] game=%s | %d라운드 | 부결 (연속 %d회, 찬성 %d/%d)",
		game.ID, game.Round, game.RejectCount, approves, len(game.Players))
	h.broadcastEvent(room, AVEventPayload{Kind: "team_rejected",
		Message: fmt.Sprintf("원정대가 부결되었습니다 (연속 %d회) — 다음 리더가 지명합니다", game.RejectCount)})
	h.beginTeamPick(room)
}

func (h *AVHub) handleQuest(client *AVClient, msg AVMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload AVQuestPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.SubmitQuest(client.Seat, payload.Success); err != nil {
		h.sendError(client, err.Error())
		return
	}
	if room.Game.QuestComplete() {
		h.resolveQuest(room)
		return
	}
	// 카드 내용은 비밀 — 제출 여부(questDone)만 실시간 반영
	h.broadcastState(room)
}

// resolveQuest 원정 집계 — 집계(실패 N장)만 공개하고 다음 단계로
func (h *AVHub) resolveQuest(room *avRoom) {
	game := room.Game
	h.stopPhaseTimer(room)
	game.Deadline = 0

	round := game.Round
	result, fails := game.ResolveQuest()

	msg := fmt.Sprintf("%d라운드 원정 성공", round)
	if result == "evil" {
		msg = fmt.Sprintf("%d라운드 원정 실패 — 실패 %d장", round, fails)
	} else if fails > 0 {
		// 7인+ 4라운드: 실패 1장은 성공 (2장 필요 규칙)
		msg = fmt.Sprintf("%d라운드 원정 성공 — 실패 %d장 (2장 미만)", round, fails)
	}
	log.Printf("[아발론][원정] game=%s | %d라운드 | %s (실패 %d장/%d명)",
		game.ID, round, avSideLabel(result), fails, game.LastQuest.Size)
	h.broadcastEvent(room, AVEventPayload{Kind: "quest_result", Message: msg})

	switch game.Phase {
	case AVPhaseGameOver:
		h.finishGame(room)
	case AVPhaseAssassin:
		h.broadcastEvent(room, AVEventPayload{Kind: "assassin_begin",
			Message: "선의 세력이 원정 3승 — 암살자가 멀린을 지목합니다"})
		h.scheduleDeadline(room, avAssassinTimeout)
		h.broadcastState(room)
	default:
		h.beginTeamPick(room)
	}
}

func (h *AVHub) handleAssassinate(client *AVClient, msg AVMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload AVAssassinatePayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.SubmitAssassinate(client.Seat, payload.Seat); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.finishGame(room)
}

// ==================== 단계 타임아웃 ====================

// scheduleDeadline 현재 단계의 타임아웃 타이머를 건다. 단계 전환 일련번호를
// 올려 지나간 발화를 구분하고, 발화는 허브 채널을 경유하므로 허브 밖에서
// 상태를 만지지 않는다.
func (h *AVHub) scheduleDeadline(room *avRoom, dur time.Duration) {
	h.stopPhaseTimer(room)
	room.Game.Seq++
	room.Game.Deadline = time.Now().Add(dur).UnixMilli()
	sig := avPhaseSignal{GameID: room.Game.ID, Seq: room.Game.Seq}
	room.PhaseTimer = time.AfterFunc(dur, func() {
		h.phaseFired <- sig
	})
}

func (h *AVHub) stopPhaseTimer(room *avRoom) {
	if room.PhaseTimer != nil {
		room.PhaseTimer.Stop()
		room.PhaseTimer = nil
	}
}

// handlePhaseFired 단계 타임아웃 — 자동 행동으로 해소한다 (AFK 진행 보장)
func (h *AVHub) handlePhaseFired(sig avPhaseSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.Game.Seq != sig.Seq {
		return
	}
	game := room.Game
	switch game.Phase {
	case AVPhaseTeamPick:
		// 리더 미지명 — 무작위 합법 지명 자동 실행
		seats := game.AutoPick(h.rng)
		if err := game.SubmitPick(game.LeaderSeat, seats); err != nil {
			return
		}
		log.Printf("[아발론][지명마감] game=%s | %d라운드 | 타임아웃 — 자동 지명 %v",
			game.ID, game.Round, game.Team)
		h.afterTeamProposed(room)
	case AVPhaseTeamVote:
		// 미제출자는 찬성 처리 (진행 보장 — 부결 루프 방지)
		log.Printf("[아발론][팀투표마감] game=%s | %d라운드 | 타임아웃 — 미제출 %d명 찬성 처리",
			game.ID, game.Round, len(game.Players)-len(game.TeamVotes))
		game.AutoCompleteTeamVote()
		h.resolveTeamVote(room)
	case AVPhaseQuest:
		// 미제출 원정대원은 성공 카드 처리
		log.Printf("[아발론][원정마감] game=%s | %d라운드 | 타임아웃 — 미제출 %d명 성공 처리",
			game.ID, game.Round, len(game.Team)-len(game.QuestCards))
		game.AutoCompleteQuest()
		h.resolveQuest(room)
	case AVPhaseAssassin:
		// 암살자 미지목 — 무작위 선 플레이어 지목
		target := game.RandomGoodSeat(h.rng)
		if err := game.SubmitAssassinate(game.AssassinSeat(), target); err != nil {
			return
		}
		log.Printf("[아발론][암살마감] game=%s | 타임아웃 — 무작위 지목 seat%d", game.ID, target)
		h.finishGame(room)
	}
}

// ==================== 상태 뷰 (은닉의 핵심) ====================

// avPlayerViews 좌석별 공개 정보. role 은 게임 종료 후에만 채운다.
// 원정 카드는 제출 여부(questDone)만 — 카드 내용은 끝까지 비공개.
func (h *AVHub) avPlayerViews(room *avRoom) []AVPlayerView {
	game := room.Game
	players := []AVPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		role := ""
		if game.Phase == AVPhaseGameOver {
			role = string(p.Role)
		}
		_, voted := game.TeamVotes[p.Seat]
		_, questDone := game.QuestCards[p.Seat]
		players = append(players, AVPlayerView{
			Seat:      p.Seat,
			Name:      p.Name,
			Connected: c != nil && c.Connected,
			Bot:       c != nil && c.Bot,
			OnTeam:    game.OnTeam(p.Seat),
			VotedTeam: voted,
			QuestDone: questDone,
			Role:      role,
		})
	}
	return players
}

// buildAVState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
// 은닉: evilSeats 는 악 진영 + 멀린에게만(그 외 필드 자체 생략), 타인 role 은
// 종료 전까지 빈 문자열, 팀 투표는 해소 후 일괄 공개, 원정 카드는 집계만.
func (h *AVHub) buildAVState(room *avRoom, viewerSeat int) AVGameStatePayload {
	game := room.Game

	yourRole := ""
	if game.Ready && viewerSeat >= 0 && viewerSeat < len(game.Players) {
		yourRole = string(game.Players[viewerSeat].Role)
	}

	var evilSeats []int
	if yourRole == string(AVRoleMerlin) || avIsEvilRole(AVRole(yourRole)) {
		evilSeats = game.EvilSeats()
	}

	endsAt := int64(0)
	switch game.Phase {
	case AVPhaseTeamPick, AVPhaseTeamVote, AVPhaseQuest, AVPhaseAssassin:
		endsAt = game.Deadline
	}

	return AVGameStatePayload{
		GameID:     game.ID,
		RoomCode:   room.Code,
		Phase:      game.Phase,
		HostSeat:   h.hostSeat(room),
		YourSeat:   viewerSeat,
		EndsAt:     endsAt,
		YourRole:   yourRole,
		EvilSeats:  evilSeats,
		Players:    h.avPlayerViews(room),
		Round:      game.Round,
		QuestSizes: game.QuestSizes,
		// nil 슬라이스는 JSON null 로 나가 프론트 .length 접근이 죽는다 — 빈 배열 보장
		Results:        append([]string{}, game.Results...),
		LeaderSeat:     game.LeaderSeat,
		RejectCount:    game.RejectCount,
		Team:           append([]int{}, game.Team...),
		TeamVotes:      game.RevealedVotes,
		LastQuest:      game.LastQuest,
		Winner:         game.Winner,
		AssassinTarget: game.AssassinTarget,
	}
}

// broadcastState 좌석마다 개인화 스냅샷을 만들어 보낸다
func (h *AVHub) broadcastState(room *avRoom) {
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, AVMessage{
			Type:    AVMsgGameState,
			Payload: h.buildAVState(room, seat),
		})
	}
}

func (h *AVHub) broadcastEvent(room *avRoom, event AVEventPayload) {
	h.broadcastToRoom(room, AVMessage{Type: AVMsgEvent, Payload: event})
}

// finishGame 종료 발표(전원 역할 공개)와 방 정리 (단판 승부 — 재대결 없음)
func (h *AVHub) finishGame(room *avRoom) {
	game := room.Game
	h.stopPhaseTimer(room)
	game.Deadline = 0

	winnerLabel := avSideLabel(game.Winner) + " 승"
	detail := fmt.Sprintf("%s (사유: %s)", winnerLabel, game.WinReason)

	winners, losers := []string{}, []string{}
	for _, p := range game.Players {
		isEvil := avIsEvilRole(p.Role)
		if (game.Winner == "evil") == isEvil {
			winners = append(winners, displayName(p.Name))
		} else {
			losers = append(losers, displayName(p.Name))
		}
	}

	h.broadcastEvent(room, AVEventPayload{Kind: "game_over", Message: detail})
	// 전원 역할이 공개된 최종 스냅샷을 먼저 보낸 뒤 종료를 알린다
	// (봇 러너는 av_game_over 를 보고 스스로 끝난다)
	h.broadcastState(room)
	h.broadcastToRoom(room, AVMessage{
		Type: AVMsgGameOver,
		Payload: AVGameOverPayload{
			Winner:         game.Winner,
			Reason:         game.WinReason,
			AssassinTarget: game.AssassinTarget,
			Players:        h.avPlayerViews(room),
		},
	})

	log.Printf("[아발론][경기결과] game=%s | %s | 소요=%s",
		game.ID, detail, matchDuration(game.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "avalon",
		Players:  strings.Join(winners, "·") + " vs " + strings.Join(losers, "·"),
		Winner:   strings.Join(winners, "·"),
		Reason:   detail,
		Duration: matchSeconds(game.StartedAt),
		Bot:      avRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *AVHub) handleDisconnect(client *AVClient) {
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
	log.Printf("[아발론][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, AVMessage{
		Type: AVMsgOpponentDisconnected,
		Payload: AVOpponentDisconnectedPayload{
			Seat:         client.Seat,
			Name:         client.Name,
			GraceSeconds: int(h.grace.Seconds()),
		},
	})
	h.broadcastState(room)

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 안에 재접속하지 않은 좌석은 연습봇으로 대체하고
// 게임은 계속한다 — 지명·투표·원정·암살 해소가 이탈 좌석에 막히지 않는 근거
func (h *AVHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == AVPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[아발론][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := h.takeoverBot(room, seat, client.Name)
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.broadcastEvent(room, AVEventPayload{Kind: "bot_takeover", Seat: &seat, Name: client.Name})
	// 새 봇이 이 스냅샷을 받아 제출이 남았으면 즉시 이어간다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *AVHub) handleRejoin(client *AVClient, msg AVMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload AVRejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, AVMessage{Type: AVMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, AVMessage{Type: AVMsgSessionExpired})
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

	log.Printf("[아발론][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, AVMessage{
		Type:    AVMsgReconnected,
		Payload: AVReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	// 전원에게 접속 상태가 반영된 스냅샷 (재접속 당사자 복원 포함)
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *AVHub) clearGameSessions(room *avRoom) {
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		h.drop(c.SessionID)
	}
}

// ==================== 전송 ====================

func (h *AVHub) sendError(client *AVClient, message string) {
	h.sendToClient(client, AVMessage{Type: AVMsgError, Payload: AVErrorPayload{Message: message}})
}

func (h *AVHub) sendToClient(client *AVClient, message AVMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[AV] ")
}

func (h *AVHub) broadcastToRoom(room *avRoom, message AVMessage) {
	for _, c := range room.Clients {
		if c != nil {
			h.sendToClient(c, message)
		}
	}
}
