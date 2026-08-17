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

// spMinute 타이머 1분의 실제 길이 (테스트에서 짧게 낮춘다)
var spMinute = time.Minute

// spVoteTimeoutMinutes 투표 제한 시간(분) — 접속만 유지한 채 투표하지 않는
// 좌석이 방을 영구 정지시키지 않게 만료 시 제출된 표만으로 판정한다
const spVoteTimeoutMinutes = 1

// spRoom 게임(순수 상태)과 좌석별 연결의 매핑
type spRoom struct {
	Game    *SPGame
	Clients map[int]*SPClient // seat → client

	// Timer playing 종료(→ voting) 타이머
	Timer *time.Timer
}

// spTimerSignal playing 타이머의 발화 표식. 발화 시점의 (게임, 단계)가
// 현재와 다르면 — 스파이 추리로 이미 끝났다면 — 지나간 신호로 보고 무시한다.
type spTimerSignal struct {
	GameID string
	Phase  SPPhase
}

type SPHub struct {
	// 등록된 클라이언트
	clients map[*SPClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*spRoom

	// 글로벌 로비. 시작 전 방은 하나뿐이다.
	lobby *spRoom

	// 클라이언트 등록
	register chan *SPClient

	// 클라이언트 등록 해제
	unregister chan *SPClient

	// 게임 메시지
	gameMessage chan SPGameMessage

	// playing 타이머 종료 (time.AfterFunc → 허브 채널 경유)
	timerFired chan spTimerSignal

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*SPClient]

	// 장소·스파이 배정용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type SPGameMessage struct {
	Client  *SPClient
	Message SPMessage
}

func NewSPHub() *SPHub {
	return &SPHub{
		register:       make(chan *SPClient),
		unregister:     make(chan *SPClient),
		clients:        make(map[*SPClient]bool),
		rooms:          make(map[string]*spRoom),
		gameMessage:    make(chan SPGameMessage),
		timerFired:     make(chan spTimerSignal, 8),
		sessionManager: newSessionManager[*SPClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *SPHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[SP] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[SP] Client unregistered: %s", client.ID)
			}

		case sessionID := <-h.graceExpired:
			h.handleGraceExpired(sessionID)

		case sig := <-h.timerFired:
			h.handleTimerFired(sig)

		case message := <-h.gameMessage:
			h.handleGameMessage(message)
		}
	}
}

func (h *SPHub) handleGameMessage(gm SPGameMessage) {
	switch gm.Message.Type {
	case SPMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case SPMsgFillBots:
		h.handleFillBots(gm.Client)
	case SPMsgStart:
		h.handleStart(gm.Client)
	case SPMsgSetTimer:
		h.handleSetTimer(gm.Client, gm.Message)
	case SPMsgSetCategory:
		h.handleSetCategory(gm.Client, gm.Message)
	case SPMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case SPMsgGuess:
		h.handleGuess(gm.Client, gm.Message)
	case SPMsgVote:
		h.handleVote(gm.Client, gm.Message)
	}
}

// ==================== 로비 ====================

func (h *SPHub) handleJoinGame(client *SPClient, msg SPMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SPJoinGamePayload
	json.Unmarshal(payloadBytes, &payload)

	if h.lobby == nil {
		game := NewSPGame(uuid.New().String())
		h.lobby = &spRoom{Game: game, Clients: map[int]*SPClient{}}
		h.rooms[game.ID] = h.lobby
		log.Printf("[SP] Created lobby game %s", game.ID)
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

	log.Printf("[스파이폴][입장] game=%s | seat%d=%s 게임 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), SPMaxPlayers)
	notify("스파이폴 참가", fmt.Sprintf("%s 입장 (%d/%d)",
		displayName(client.Name), len(room.Game.Players), SPMaxPlayers))

	h.sendToClient(client, SPMessage{
		Type: SPMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
		},
	})

	h.broadcastEvent(room, SPEventPayload{Kind: "joined", Seat: &seat, Name: client.Name})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *SPHub) hostSeat(room *spRoom) int {
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

// handleFillBots host 가 최소 성립 인원(3)까지 연습봇으로 채운다.
// 봇은 연습용이라 정원(8)까지 채우지 않는다. 시작은 별도의 sp_start.
func (h *SPHub) handleFillBots(client *SPClient) {
	room := h.lobby
	if room == nil || client.GameID != room.Game.ID {
		h.sendError(client, "로비를 찾을 수 없습니다")
		return
	}
	if client.Seat != h.hostSeat(room) {
		h.sendError(client, "호스트만 봇을 채울 수 있습니다")
		return
	}
	if len(room.Game.Players) >= SPBotFillTarget {
		h.sendError(client, fmt.Sprintf("%d명 미만일 때만 봇을 채울 수 있습니다", SPBotFillTarget))
		return
	}

	botNo := 0
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			botNo++
		}
	}
	for len(room.Game.Players) < SPBotFillTarget {
		botNo++
		if h.spawnBot(room, fmt.Sprintf("%s%d", botName, botNo)) == nil {
			break
		}
	}
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

func (h *SPHub) handleStart(client *SPClient) {
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
		h.sendError(client, fmt.Sprintf("%d명 이상 모여야 시작할 수 있습니다", SPMinPlayers))
		return
	}
	h.startGame(room)
}

// handleSetTimer host 의 대기실 타이머 선택 (3|5|8분)
func (h *SPHub) handleSetTimer(client *SPClient, msg SPMessage) {
	room := h.lobby
	if room == nil || client.GameID != room.Game.ID {
		h.sendError(client, "로비를 찾을 수 없습니다")
		return
	}
	if client.Seat != h.hostSeat(room) {
		h.sendError(client, "호스트만 타이머를 바꿀 수 있습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SPSetTimerPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.SetTimer(payload.Minutes); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.broadcastState(room)
}

// handleSetCategory host 의 대기실 카테고리 선택 (랜덤 + spCategoryNames 9종)
func (h *SPHub) handleSetCategory(client *SPClient, msg SPMessage) {
	room := h.lobby
	if room == nil || client.GameID != room.Game.ID {
		h.sendError(client, "로비를 찾을 수 없습니다")
		return
	}
	if client.Seat != h.hostSeat(room) {
		h.sendError(client, "호스트만 카테고리를 바꿀 수 있습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SPSetCategoryPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.SetCategory(payload.Category); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.broadcastState(room)
}

// spHumanCount 방의 사람 수
func spHumanCount(room *spRoom) int {
	n := 0
	for _, c := range room.Clients {
		if c != nil && !c.Bot {
			n++
		}
	}
	return n
}

// spRoomHasBot 방에 연습봇이 있는지 (전적 기록용)
func spRoomHasBot(room *spRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}

// updateLobbyWaiting 로비 현황판 갱신 — 사람 1명 이상 대기 && 미시작
func (h *SPHub) updateLobbyWaiting(room *spRoom) {
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && spHumanCount(room) >= 1
	lobbySetWaiting("spyfall", waiting)
}

func (h *SPHub) startGame(room *spRoom) {
	timerDur := time.Duration(room.Game.TimerMinutes) * spMinute
	if err := room.Game.Start(h.rng, timerDur); err != nil {
		return
	}
	h.lobby = nil // 시작한 방은 로비에서 떼어낸다
	lobbySetWaiting("spyfall", false)

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	log.Printf("[스파이폴][경기시작] game=%s | 인원=%d | 타이머=%d분 | %v",
		room.Game.ID, len(room.Game.Players), room.Game.TimerMinutes, names)
	// 봇 채우기로 시작한 판도 포함해 시작 시점에 1회만 알린다
	notify("스파이폴 게임 시작", fmt.Sprintf("%d인전 시작", len(room.Game.Players)))

	h.broadcastEvent(room, SPEventPayload{Kind: "started",
		Message: fmt.Sprintf("%d분 타이머로 시작합니다", room.Game.TimerMinutes)})
	h.broadcastState(room)
	h.scheduleTimer(room)
}

// removeFromLobby 대기 중 이탈 — 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *SPHub) removeFromLobby(room *spRoom, client *SPClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	rebuilt := map[int]*SPClient{}
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

	log.Printf("[스파이폴][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), SPMaxPlayers)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if spHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, SPMessage{Type: SPMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		lobbySetWaiting("spyfall", false)
		return
	}

	h.broadcastEvent(room, SPEventPayload{Kind: "left", Name: client.Name})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *SPHub) roomOf(client *SPClient) *spRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

// handleGuess 스파이의 장소 추리 — 적중/오답 즉시 종료
func (h *SPHub) handleGuess(client *SPClient, msg SPMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SPGuessPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.Guess(client.Seat, payload.Location); err != nil {
		h.sendError(client, err.Error())
		return
	}

	seat := client.Seat
	log.Printf("[스파이폴][추리] game=%s | seat%d=%s → %q (정답 %q)",
		room.Game.ID, seat, displayName(client.Name), payload.Location, room.Game.Location)
	h.broadcastEvent(room, SPEventPayload{Kind: "guess", Seat: &seat,
		Message: fmt.Sprintf("스파이가 정답을 추리했습니다 — %s", payload.Location)})
	h.finishGame(room)
}

func (h *SPHub) handleVote(client *SPClient, msg SPMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SPVotePayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.SubmitVote(client.Seat, payload.Target); err != nil {
		h.sendError(client, err.Error())
		return
	}
	if room.Game.VoteComplete() {
		room.Game.ResolveVotes()
		h.finishGame(room)
		return
	}
	// 공개 투표 — 표가 쌓일 때마다 실시간 스냅샷
	h.broadcastState(room)
}

// ==================== playing 타이머 ====================

// scheduleTimer playing 종료 타이머를 건다.
// 발화는 허브 채널을 경유하므로 허브 밖에서 상태를 만지지 않는다.
func (h *SPHub) scheduleTimer(room *spRoom) {
	sig := spTimerSignal{GameID: room.Game.ID, Phase: SPPhasePlaying}
	dur := time.Until(time.UnixMilli(room.Game.EndsAt))
	room.Timer = time.AfterFunc(dur, func() {
		h.timerFired <- sig
	})
}

// handleTimerFired 타이머 종료 — 스파이 추리로 이미 끝난 게임의 늦은 신호는
// (GameID·Phase 가드로) 무시한다
func (h *SPHub) handleTimerFired(sig spTimerSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.Game.Phase != sig.Phase {
		return
	}
	if sig.Phase == SPPhaseVoting {
		// 투표 타임아웃 — 제출된 표만으로 판정 (표가 없으면 미검거 = 스파이 승)
		log.Printf("[스파이폴][투표마감] game=%s | 타임아웃 — %d표로 판정",
			room.Game.ID, len(room.Game.Votes))
		room.Game.ResolveVotes()
		h.finishGame(room)
		return
	}

	room.Game.BeginVoting(spVoteTimeoutMinutes * spMinute)
	log.Printf("[스파이폴][투표시작] game=%s | 타이머 종료", room.Game.ID)
	h.broadcastEvent(room, SPEventPayload{Kind: "vote_begin",
		Message: "시간 종료 — 스파이로 의심되는 사람을 지목하세요"})
	h.broadcastState(room)
	h.scheduleVoteTimer(room)
}

// scheduleVoteTimer 투표 타임아웃 타이머 (playing 타이머와 같은 채널 경유)
func (h *SPHub) scheduleVoteTimer(room *spRoom) {
	sig := spTimerSignal{GameID: room.Game.ID, Phase: SPPhaseVoting}
	dur := time.Until(time.UnixMilli(room.Game.EndsAt))
	room.Timer = time.AfterFunc(dur, func() {
		h.timerFired <- sig
	})
}

// ==================== 상태 뷰 (은닉의 핵심) ====================

// spPlayerViews 좌석별 공개 정보 — 스파이 여부는 어떤 형태로도 싣지 않는다
func (h *SPHub) spPlayerViews(room *spRoom) []SPPlayerView {
	game := room.Game
	players := []SPPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		_, voted := game.Votes[p.Seat]
		players = append(players, SPPlayerView{
			Seat:      p.Seat,
			Name:      p.Name,
			Connected: c != nil && c.Connected,
			Bot:       c != nil && c.Bot,
			Voted:     voted,
		})
	}
	return players
}

// buildSPState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
// 은닉: 비스파이 스냅샷에는 스파이 좌석 정보가 어떤 형태로도 없다.
// isSpy 는 본인 것만, location 은 비스파이에게만 (스파이·waiting 은 빈 문자열).
// 스파이 정체는 game_over 의 result 로만 공개된다.
func (h *SPHub) buildSPState(room *spRoom, viewerSeat int) SPGameStatePayload {
	game := room.Game

	isSpy := game.Ready && viewerSeat == game.SpySeat
	location := ""
	var locations []string
	if game.Ready {
		locations = game.Words()
		if !isSpy {
			location = game.Location
		}
	}

	endsAt := int64(0)
	if game.Phase == SPPhasePlaying || game.Phase == SPPhaseVoting {
		endsAt = game.EndsAt
	}

	var votes []SPVoteView
	if game.Votes != nil {
		votes = game.VoteViews()
	}

	return SPGameStatePayload{
		GameID:         game.ID,
		Phase:          game.Phase,
		HostSeat:       h.hostSeat(room),
		YourSeat:       viewerSeat,
		TimerMinutes:   game.TimerMinutes,
		CategoryChoice: game.CategoryChoice,
		Category:       game.Category,
		EndsAt:         endsAt,
		Locations:      locations,
		IsSpy:          isSpy,
		Location:       location,
		Players:        h.spPlayerViews(room),
		Votes:          votes,
		Result:         game.Result,
	}
}

// broadcastState 좌석마다 개인화 스냅샷을 만들어 보낸다
func (h *SPHub) broadcastState(room *spRoom) {
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, SPMessage{
			Type:    SPMsgGameState,
			Payload: h.buildSPState(room, seat),
		})
	}
}

func (h *SPHub) broadcastEvent(room *spRoom, event SPEventPayload) {
	h.broadcastToRoom(room, SPMessage{Type: SPMsgEvent, Payload: event})
}

// finishGame 종료 발표(스파이 정체·장소·사유 공개)와 방 정리 (재대결 없음)
func (h *SPHub) finishGame(room *spRoom) {
	game := room.Game
	if room.Timer != nil {
		room.Timer.Stop()
		room.Timer = nil
	}

	res := game.Result
	winnerLabel := "시민 승"
	if res.Winner == "spy" {
		winnerLabel = "스파이 승"
	}
	detail := fmt.Sprintf("%s (%s)", winnerLabel, spReasonLabel(res.Reason))

	winners, losers := []string{}, []string{}
	for _, p := range game.Players {
		isSpy := p.Seat == game.SpySeat
		if (res.Winner == "spy") == isSpy {
			winners = append(winners, displayName(p.Name))
		} else {
			losers = append(losers, displayName(p.Name))
		}
	}

	h.broadcastEvent(room, SPEventPayload{Kind: "game_over", Message: detail})
	// 결과가 실린 최종 스냅샷을 먼저 보낸 뒤 종료를 알린다
	// (봇 러너는 sp_game_over 를 보고 스스로 끝난다)
	h.broadcastState(room)
	h.broadcastToRoom(room, SPMessage{
		Type: SPMsgGameOver,
		Payload: SPGameOverPayload{
			Winner:          res.Winner,
			SpySeat:         res.SpySeat,
			Category:        res.Category,
			Location:        res.Location,
			Reason:          res.Reason,
			GuessedLocation: res.GuessedLocation,
			TopSeat:         res.TopSeat,
			Players:         h.spPlayerViews(room),
		},
	})

	log.Printf("[스파이폴][경기결과] game=%s | %s | %s=%s | 스파이=seat%d | 소요=%s",
		game.ID, detail, game.Category, game.Location, game.SpySeat, matchDuration(game.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "spyfall",
		Players:  strings.Join(winners, "·") + " vs " + strings.Join(losers, "·"),
		Winner:   strings.Join(winners, "·"),
		Reason:   detail,
		Duration: matchSeconds(game.StartedAt),
		Bot:      spRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *SPHub) handleDisconnect(client *SPClient) {
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
	log.Printf("[스파이폴][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, SPMessage{
		Type: SPMsgOpponentDisconnected,
		Payload: SPOpponentDisconnectedPayload{
			Seat:         client.Seat,
			Name:         client.Name,
			GraceSeconds: int(h.grace.Seconds()),
		},
	})
	h.broadcastState(room)

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 안에 재접속하지 않은 좌석은 연습봇으로 대체하고
// 게임은 계속한다 — 투표 해소가 이탈 좌석에 막히지 않는 근거
func (h *SPHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == SPPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[스파이폴][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := h.takeoverBot(room, seat, client.Name)
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.broadcastEvent(room, SPEventPayload{Kind: "bot_takeover", Seat: &seat, Name: client.Name})
	// 새 봇이 이 스냅샷을 받아 제출이 남았으면 즉시 이어간다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *SPHub) handleRejoin(client *SPClient, msg SPMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload SPRejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, SPMessage{Type: SPMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, SPMessage{Type: SPMsgSessionExpired})
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

	log.Printf("[스파이폴][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, SPMessage{
		Type:    SPMsgReconnected,
		Payload: SPReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	// 전원에게 접속 상태가 반영된 스냅샷 (재접속 당사자 복원 포함)
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *SPHub) clearGameSessions(room *spRoom) {
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		h.drop(c.SessionID)
	}
}

// ==================== 전송 ====================

func (h *SPHub) sendError(client *SPClient, message string) {
	h.sendToClient(client, SPMessage{Type: SPMsgError, Payload: SPErrorPayload{Message: message}})
}

func (h *SPHub) sendToClient(client *SPClient, message SPMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[SP] ")
}

func (h *SPHub) broadcastToRoom(room *spRoom, message SPMessage) {
	for _, c := range room.Clients {
		if c != nil {
			h.sendToClient(c, message)
		}
	}
}
