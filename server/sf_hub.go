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

// sfPhaseDelay day_result·execution 발표 화면에서 다음 단계로 자동 진행하기까지의
// 시간 (테스트에서 짧게 낮춘다)
var sfPhaseDelay = 5 * time.Second

// sfRoom 게임(순수 상태)과 좌석별 연결의 매핑
type sfRoom struct {
	Game       *SFGame
	Clients    map[int]*SFClient // seat → client
	PhaseTimer *time.Timer       // day_result/execution 자동 진행 타이머
}

// sfPhaseSignal 자동 진행 타이머의 발화 표식. 발화 시점의 (단계, 일차)가
// 현재와 다르면 이미 지나간 신호로 보고 무시한다.
type sfPhaseSignal struct {
	GameID string
	Phase  SFPhase
	DayNo  int
}

type SFHub struct {
	// 등록된 클라이언트
	clients map[*SFClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*sfRoom

	// 글로벌 로비. 시작 전 방은 하나뿐이다.
	lobby *sfRoom

	// 클라이언트 등록
	register chan *SFClient

	// 클라이언트 등록 해제
	unregister chan *SFClient

	// 게임 메시지
	gameMessage chan SFGameMessage

	// day_result/execution 자동 진행 (time.AfterFunc → 허브 채널 경유)
	phaseFired chan sfPhaseSignal

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*SFClient]

	// 셔플·동률 해소용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type SFGameMessage struct {
	Client  *SFClient
	Message SFMessage
}

func NewSFHub() *SFHub {
	return &SFHub{
		register:       make(chan *SFClient),
		unregister:     make(chan *SFClient),
		clients:        make(map[*SFClient]bool),
		rooms:          make(map[string]*sfRoom),
		gameMessage:    make(chan SFGameMessage),
		phaseFired:     make(chan sfPhaseSignal, 8),
		sessionManager: newSessionManager[*SFClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *SFHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[SF] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[SF] Client unregistered: %s", client.ID)
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

func (h *SFHub) handleGameMessage(gm SFGameMessage) {
	switch gm.Message.Type {
	case SFMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case SFMsgFillBots:
		h.handleFillBots(gm.Client)
	case SFMsgStart:
		h.handleStart(gm.Client)
	case SFMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case SFMsgNightAction:
		h.handleNightAction(gm.Client, gm.Message)
	case SFMsgVote:
		h.handleVote(gm.Client, gm.Message)
	}
}

// ==================== 로비 ====================

func (h *SFHub) handleJoinGame(client *SFClient, msg SFMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SFJoinGamePayload
	json.Unmarshal(payloadBytes, &payload)

	if h.lobby == nil {
		game := NewSFGame(uuid.New().String())
		h.lobby = &sfRoom{Game: game, Clients: map[int]*SFClient{}}
		h.rooms[game.ID] = h.lobby
		log.Printf("[SF] Created lobby game %s", game.ID)
	}

	room := h.lobby
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

	log.Printf("[마피아][입장] game=%s | seat%d=%s 게임 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), SFMaxPlayers)
	notify("마피아 참가", fmt.Sprintf("%s 입장 (%d/%d)",
		displayName(client.Name), len(room.Game.Players), SFMaxPlayers))

	h.sendToClient(client, SFMessage{
		Type: SFMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
		},
	})

	h.broadcastEvent(room, SFEventPayload{Kind: "joined", Seat: &seat, Name: client.Name})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *SFHub) hostSeat(room *sfRoom) int {
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

// handleFillBots host 가 최소 성립 인원(6)까지 연습봇으로 채운다.
// 봇은 연습용이라 정원(10)까지 채우지 않는다. 시작은 별도의 sf_start.
func (h *SFHub) handleFillBots(client *SFClient) {
	room := h.lobby
	if room == nil || client.GameID != room.Game.ID {
		h.sendError(client, "로비를 찾을 수 없습니다")
		return
	}
	if client.Seat != h.hostSeat(room) {
		h.sendError(client, "호스트만 봇을 채울 수 있습니다")
		return
	}
	if len(room.Game.Players) >= SFBotFillTarget {
		h.sendError(client, fmt.Sprintf("%d명 미만일 때만 봇을 채울 수 있습니다", SFBotFillTarget))
		return
	}

	botNo := 0
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			botNo++
		}
	}
	for len(room.Game.Players) < SFBotFillTarget {
		botNo++
		if h.spawnBot(room, fmt.Sprintf("%s%d", botName, botNo)) == nil {
			break
		}
	}
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

func (h *SFHub) handleStart(client *SFClient) {
	room := h.lobby
	if room == nil || client.GameID != room.Game.ID {
		h.sendError(client, "로비를 찾을 수 없습니다")
		return
	}
	if client.Seat != h.hostSeat(room) {
		h.sendError(client, "호스트만 시작할 수 있습니다")
		return
	}
	if !room.Game.CanStart() {
		h.sendError(client, fmt.Sprintf("%d명 이상 모여야 시작할 수 있습니다", SFMinPlayers))
		return
	}
	h.startGame(room)
}

// sfHumanCount 방의 사람 수
func sfHumanCount(room *sfRoom) int {
	n := 0
	for _, c := range room.Clients {
		if c != nil && !c.Bot {
			n++
		}
	}
	return n
}

// sfRoomHasBot 방에 연습봇이 있는지 (전적 기록용)
func sfRoomHasBot(room *sfRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}

// updateLobbyWaiting 로비 현황판 갱신 — 사람 1명 이상 대기 && 미시작
func (h *SFHub) updateLobbyWaiting(room *sfRoom) {
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && sfHumanCount(room) >= 1
	lobbySetWaiting("skyfall", waiting)
}

func (h *SFHub) startGame(room *sfRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	h.lobby = nil // 시작한 방은 로비에서 떼어낸다
	lobbySetWaiting("skyfall", false)

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	log.Printf("[마피아][경기시작] game=%s | 인원=%d | %v",
		room.Game.ID, len(room.Game.Players), names)
	// 봇 채우기로 시작한 판도 포함해 시작 시점에 1회만 알린다
	notify("마피아 게임 시작", fmt.Sprintf("%d인전 시작", len(room.Game.Players)))

	h.broadcastEvent(room, SFEventPayload{Kind: "started"})
	h.broadcastEvent(room, SFEventPayload{Kind: "night_begin",
		Message: fmt.Sprintf("%d일차 밤이 시작되었습니다", room.Game.DayNo)})
	h.broadcastState(room)
}

// removeFromLobby 대기 중 이탈 — 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *SFHub) removeFromLobby(room *sfRoom, client *SFClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	rebuilt := map[int]*SFClient{}
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

	log.Printf("[마피아][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), SFMaxPlayers)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if sfHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, SFMessage{Type: SFMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		lobbySetWaiting("skyfall", false)
		return
	}

	h.broadcastEvent(room, SFEventPayload{Kind: "left", Name: client.Name})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *SFHub) roomOf(client *SFClient) *sfRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

func (h *SFHub) handleNightAction(client *SFClient, msg SFMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SFNightActionPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.SubmitNightAction(client.Seat, payload.Target); err != nil {
		h.sendError(client, err.Error())
		return
	}
	if room.Game.NightComplete() {
		h.resolveNight(room)
		return
	}
	h.broadcastState(room)
}

// resolveNight 밤 해소 — 결과 발표 후 5초 뒤 투표로 자동 진행
func (h *SFHub) resolveNight(room *sfRoom) {
	game := room.Game
	killed := game.ResolveNight(h.rng)

	if killed >= 0 {
		seat := killed
		msg := fmt.Sprintf("%s님이 살해당했습니다", game.Players[seat].Name)
		log.Printf("[마피아][밤해소] game=%s | %d일차 | seat%d=%s 살해",
			game.ID, game.DayNo, seat, displayName(game.Players[seat].Name))
		h.broadcastEvent(room, SFEventPayload{Kind: "day_result", Seat: &seat, Message: msg})
	} else {
		log.Printf("[마피아][밤해소] game=%s | %d일차 | 의사 세이브 — 사망자 없음",
			game.ID, game.DayNo)
		h.broadcastEvent(room, SFEventPayload{Kind: "day_result", Message: "아무도 죽지 않았습니다"})
	}

	h.broadcastState(room)
	if game.Phase == SFPhaseGameOver {
		h.finishGame(room)
		return
	}
	h.schedulePhase(room, SFPhaseDayResult)
}

func (h *SFHub) handleVote(client *SFClient, msg SFMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SFVotePayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.SubmitVote(client.Seat, payload.Target); err != nil {
		h.sendError(client, err.Error())
		return
	}
	if room.Game.VoteComplete() {
		h.resolveVotes(room)
		return
	}
	// 공개 투표 — 표가 쌓일 때마다 실시간 스냅샷
	h.broadcastState(room)
}

// resolveVotes 투표 집계 — 처형(역할 공개) 또는 무처형 발표 후 5초 뒤 밤으로
func (h *SFHub) resolveVotes(room *sfRoom) {
	game := room.Game
	game.ResolveVotes()

	if game.Execution != nil && game.Execution.Seat >= 0 {
		seat := game.Execution.Seat
		role := game.Players[seat].Role
		log.Printf("[마피아][처형] game=%s | %d일차 | seat%d=%s (%s)",
			game.ID, game.DayNo, seat, displayName(game.Players[seat].Name), sfRoleLabel(role))
		h.broadcastEvent(room, SFEventPayload{Kind: "executed", Seat: &seat,
			Message: fmt.Sprintf("%s님이 처형되었습니다 — %s", game.Players[seat].Name, sfRoleLabel(role))})
	} else {
		log.Printf("[마피아][무처형] game=%s | %d일차 | 동률·전원 기권", game.ID, game.DayNo)
		h.broadcastEvent(room, SFEventPayload{Kind: "no_execution",
			Message: "동률 또는 전원 기권 — 처형이 없습니다"})
	}

	h.broadcastState(room)
	if game.Phase == SFPhaseGameOver {
		h.finishGame(room)
		return
	}
	h.schedulePhase(room, SFPhaseExecution)
}

// ==================== 자동 진행 타이머 ====================

// schedulePhase 발표 단계의 자동 진행 타이머를 건다.
// 발화는 허브 채널을 경유하므로 허브 밖에서 상태를 만지지 않는다.
func (h *SFHub) schedulePhase(room *sfRoom, phase SFPhase) {
	sig := sfPhaseSignal{GameID: room.Game.ID, Phase: phase, DayNo: room.Game.DayNo}
	room.PhaseTimer = time.AfterFunc(sfPhaseDelay, func() {
		h.phaseFired <- sig
	})
}

func (h *SFHub) handlePhaseFired(sig sfPhaseSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.Game.Phase != sig.Phase || room.Game.DayNo != sig.DayNo {
		return
	}
	game := room.Game
	switch sig.Phase {
	case SFPhaseDayResult:
		game.BeginVote()
		h.broadcastEvent(room, SFEventPayload{Kind: "vote_begin",
			Message: fmt.Sprintf("%d일차 투표를 시작합니다", game.DayNo)})
		h.broadcastState(room)
	case SFPhaseExecution:
		game.BeginNight()
		h.broadcastEvent(room, SFEventPayload{Kind: "night_begin",
			Message: fmt.Sprintf("%d일차 밤이 시작되었습니다", game.DayNo)})
		h.broadcastState(room)
	}
}

// ==================== 상태 뷰 (은닉의 핵심) ====================

// sfPlayerViews 좌석별 공개 정보. role 은 처형·게임 종료로 공개된 경우에만
// 채운다 (게임 종료 시 finish 가 전원 RoleRevealed 처리).
func (h *SFHub) sfPlayerViews(room *sfRoom) []SFPlayerView {
	game := room.Game
	players := []SFPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		role := ""
		if p.RoleRevealed {
			role = string(p.Role)
		}
		players = append(players, SFPlayerView{
			Seat:      p.Seat,
			Name:      p.Name,
			Connected: c != nil && c.Connected,
			Bot:       c != nil && c.Bot,
			Alive:     p.Alive,
			Role:      role,
		})
	}
	return players
}

// buildSFState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
// 은닉: mafiaSeats 는 마피아에게만(그 외 필드 자체 생략), 타인 role 은 공개
// 전까지 빈 문자열, 경찰 조사 결과는 경찰에게만.
func (h *SFHub) buildSFState(room *sfRoom, viewerSeat int) SFGameStatePayload {
	game := room.Game

	yourRole := ""
	if game.Ready && viewerSeat >= 0 && viewerSeat < len(game.Players) {
		yourRole = string(game.Players[viewerSeat].Role)
	}

	var mafiaSeats []int
	if yourRole == string(SFRoleMafia) {
		mafiaSeats = game.MafiaSeats()
	}

	var night *SFNightView
	if game.Phase == SFPhaseNight {
		_, done := game.NightActions[viewerSeat]
		night = &SFNightView{YourActionDone: done, PendingRoles: game.PendingNightRoles()}
	}

	var investigation *SFInvestigationView
	if yourRole == string(SFRolePolice) {
		investigation = game.Investigation
	}

	var votes []SFVoteView
	if game.Votes != nil {
		votes = game.VoteViews()
	}

	return SFGameStatePayload{
		GameID:        game.ID,
		Phase:         game.Phase,
		DayNo:         game.DayNo,
		HostSeat:      h.hostSeat(room),
		YourSeat:      viewerSeat,
		YourRole:      yourRole,
		MafiaSeats:    mafiaSeats,
		Players:       h.sfPlayerViews(room),
		Night:         night,
		Investigation: investigation,
		Announcement:  game.Announcement,
		Votes:         votes,
		Execution:     game.Execution,
		Winner:        game.Winner,
	}
}

// broadcastState 좌석마다 개인화 스냅샷을 만들어 보낸다
func (h *SFHub) broadcastState(room *sfRoom) {
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, SFMessage{
			Type:    SFMsgGameState,
			Payload: h.buildSFState(room, seat),
		})
	}
}

func (h *SFHub) broadcastEvent(room *sfRoom, event SFEventPayload) {
	h.broadcastToRoom(room, SFMessage{Type: SFMsgEvent, Payload: event})
}

// finishGame 종료 발표(전원 역할 공개)와 방 정리 (단판 승부 — 재대결 없음)
func (h *SFHub) finishGame(room *sfRoom) {
	game := room.Game
	if room.PhaseTimer != nil {
		room.PhaseTimer.Stop()
		room.PhaseTimer = nil
	}

	winnerLabel := "시민 승"
	if game.Winner == "mafia" {
		winnerLabel = "마피아 승"
	}
	detail := fmt.Sprintf("%s (%d일차)", winnerLabel, game.DayNo)

	winners, losers := []string{}, []string{}
	for _, p := range game.Players {
		isMafia := p.Role == SFRoleMafia
		if (game.Winner == "mafia") == isMafia {
			winners = append(winners, displayName(p.Name))
		} else {
			losers = append(losers, displayName(p.Name))
		}
	}

	h.broadcastEvent(room, SFEventPayload{Kind: "game_over", Message: detail})
	// 전원 역할이 공개된 최종 스냅샷을 먼저 보낸 뒤 종료를 알린다
	// (봇 러너는 sf_game_over 를 보고 스스로 끝난다)
	h.broadcastState(room)
	h.broadcastToRoom(room, SFMessage{
		Type: SFMsgGameOver,
		Payload: SFGameOverPayload{
			Winner:  game.Winner,
			DayNo:   game.DayNo,
			Players: h.sfPlayerViews(room),
		},
	})

	log.Printf("[마피아][경기결과] game=%s | %s | 소요=%s",
		game.ID, detail, matchDuration(game.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "skyfall",
		Players:  strings.Join(winners, "·") + " vs " + strings.Join(losers, "·"),
		Winner:   strings.Join(winners, "·"),
		Reason:   detail,
		Duration: matchSeconds(game.StartedAt),
		Bot:      sfRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *SFHub) handleDisconnect(client *SFClient) {
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
	log.Printf("[마피아][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, SFMessage{
		Type: SFMsgOpponentDisconnected,
		Payload: SFOpponentDisconnectedPayload{
			Seat:         client.Seat,
			Name:         client.Name,
			GraceSeconds: int(h.grace.Seconds()),
		},
	})
	h.broadcastState(room)

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 안에 재접속하지 않은 좌석은 연습봇으로 대체하고
// 게임은 계속한다 — 밤·투표 해소가 이탈 좌석에 막히지 않는 근거
func (h *SFHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == SFPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[마피아][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := h.takeoverBot(room, seat, client.Name)
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.broadcastEvent(room, SFEventPayload{Kind: "bot_takeover", Seat: &seat, Name: client.Name})
	// 새 봇이 이 스냅샷을 받아 제출이 남았으면 즉시 이어간다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *SFHub) handleRejoin(client *SFClient, msg SFMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SFRejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, SFMessage{Type: SFMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, SFMessage{Type: SFMsgSessionExpired})
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

	log.Printf("[마피아][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, SFMessage{
		Type:    SFMsgReconnected,
		Payload: SFReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	// 전원에게 접속 상태가 반영된 스냅샷 (재접속 당사자 복원 포함)
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *SFHub) clearGameSessions(room *sfRoom) {
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		h.drop(c.SessionID)
	}
}

// ==================== 전송 ====================

func (h *SFHub) sendError(client *SFClient, message string) {
	h.sendToClient(client, SFMessage{Type: SFMsgError, Payload: SFErrorPayload{Message: message}})
}

func (h *SFHub) sendToClient(client *SFClient, message SFMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[SF] ")
}

func (h *SFHub) broadcastToRoom(room *sfRoom, message SFMessage) {
	for _, c := range room.Clients {
		if c != nil {
			h.sendToClient(c, message)
		}
	}
}
