package server

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"time"

	"github.com/google/uuid"
)

// tcHandEndDelay hand_end 에서 다음 핸드로 자동 진행하기까지의 시간
// (전원 tc_ready 면 즉시, 테스트에서는 짧게 낮춘다)
var tcHandEndDelay = 5 * time.Second

// tcRoom 게임(순수 상태)과 좌석별 연결의 매핑
type tcRoom struct {
	Game    *TCGame
	Clients map[int]*TCClient // seat → client

	// HandEndTimer hand_end 자동 진행 타이머 (허브 채널 경유로만 상태를 만진다)
	HandEndTimer *time.Timer
}

// tcHandEndSignal 어느 게임의 몇 번째 핸드 종료 타이머인지 (뒤늦은 발화 무시용)
type tcHandEndSignal struct {
	GameID string
	HandNo int
}

type TCHub struct {
	// 등록된 클라이언트
	clients map[*TCClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*tcRoom

	// 글로벌 로비. 시작 전 방은 하나뿐이다.
	lobby *tcRoom

	// 클라이언트 등록
	register chan *TCClient

	// 클라이언트 등록 해제
	unregister chan *TCClient

	// 게임 메시지
	gameMessage chan TCGameMessage

	// hand_end 자동 진행 알림
	handEndFired chan tcHandEndSignal

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*TCClient]

	// 셔플용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type TCGameMessage struct {
	Client  *TCClient
	Message TCMessage
}

func NewTCHub() *TCHub {
	return &TCHub{
		register:       make(chan *TCClient),
		unregister:     make(chan *TCClient),
		clients:        make(map[*TCClient]bool),
		rooms:          make(map[string]*tcRoom),
		gameMessage:    make(chan TCGameMessage),
		handEndFired:   make(chan tcHandEndSignal, 8),
		sessionManager: newSessionManager[*TCClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *TCHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[TC] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[TC] Client unregistered: %s", client.ID)
			}

		case sessionID := <-h.graceExpired:
			h.handleGraceExpired(sessionID)

		case sig := <-h.handEndFired:
			h.handleHandEndFired(sig)

		case message := <-h.gameMessage:
			h.handleGameMessage(message)
		}
	}
}

func (h *TCHub) handleGameMessage(gm TCGameMessage) {
	switch gm.Message.Type {
	case TCMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case TCMsgSetTarget:
		h.handleSetTarget(gm.Client, gm.Message)
	case TCMsgFillBots:
		h.handleFillBots(gm.Client)
	case TCMsgCallGrand:
		h.handleCallGrand(gm.Client, gm.Message)
	case TCMsgCallTichu:
		h.handleCallTichu(gm.Client)
	case TCMsgExchange:
		h.handleExchange(gm.Client, gm.Message)
	case TCMsgPlay:
		h.handlePlay(gm.Client, gm.Message)
	case TCMsgPass:
		h.handlePass(gm.Client)
	case TCMsgDragonGive:
		h.handleDragonGive(gm.Client, gm.Message)
	case TCMsgReady:
		h.handleReady(gm.Client)
	case TCMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	}
}

// ==================== 대기실 ====================

func (h *TCHub) handleJoinGame(client *TCClient, msg TCMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload TCJoinGamePayload
	json.Unmarshal(payloadBytes, &payload)

	if h.lobby == nil {
		game := NewTCGame(uuid.New().String())
		h.lobby = &tcRoom{Game: game, Clients: map[int]*TCClient{}}
		h.rooms[game.ID] = h.lobby
		log.Printf("[TC] Created lobby game %s", game.ID)
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

	log.Printf("[티츄][입장] game=%s | seat%d=%s 로비 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), room.Game.PlayerCount, TCSeats)
	notify("티츄 참가", fmt.Sprintf("%s 입장 (%d/%d)",
		displayName(client.Name), room.Game.PlayerCount, TCSeats))
	lobbySetWaiting("tichu", true)

	h.sendToClient(client, TCMessage{
		Type: TCMsgPlayerJoined,
		Payload: TCPlayerJoinedPayload{
			GameID:    room.Game.ID,
			SessionID: client.SessionID,
			YourSeat:  seat,
			Name:      client.Name,
		},
	})
	h.broadcastEvent(room, TCEventPayload{Kind: "joined", Seat: &seat,
		Message: fmt.Sprintf("%s 입장", client.Name)})
	h.broadcastState(room)

	if room.Game.PlayerCount == TCSeats {
		h.startGame(room)
	}
}

func (h *TCHub) handleSetTarget(client *TCClient, msg TCMessage) {
	room := h.lobby
	if room == nil || client.GameID != room.Game.ID {
		h.sendError(client, "대기실을 찾을 수 없습니다")
		return
	}
	if client.Seat != 0 {
		h.sendError(client, "호스트만 목표점수를 바꿀 수 있습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload TCSetTargetPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.SetTarget(payload.Target); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.broadcastState(room)
}

func (h *TCHub) handleFillBots(client *TCClient) {
	room := h.lobby
	if room == nil || client.GameID != room.Game.ID {
		h.sendError(client, "대기실을 찾을 수 없습니다")
		return
	}
	if client.Seat != 0 {
		h.sendError(client, "호스트만 봇을 채울 수 있습니다")
		return
	}
	botNo := 1
	for room.Game.PlayerCount < TCSeats {
		if !h.spawnTCBot(room, fmt.Sprintf("%s%d", botName, botNo)) {
			break
		}
		botNo++
	}
	if room.Game.PlayerCount == TCSeats {
		h.startGame(room)
	}
}

func (h *TCHub) startGame(room *tcRoom) {
	h.lobby = nil // 시작한 방은 로비에서 떼어낸다
	lobbySetWaiting("tichu", false)
	room.Game.StartHand(h.rng)

	names := []string{}
	for s := 0; s < TCSeats; s++ {
		names = append(names, displayName(room.Game.Names[s]))
	}
	log.Printf("[티츄][경기시작] game=%s | 목표=%d | %v",
		room.Game.ID, room.Game.Target, names)
	notify("티츄 게임 시작", fmt.Sprintf("%s·%s vs %s·%s (목표 %d점)",
		names[0], names[2], names[1], names[3], room.Game.Target))

	h.broadcastState(room)
}

// removeFromLobby 대기실에서 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *TCHub) removeFromLobby(room *tcRoom, client *TCClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	// 좌석 압축: oldSeat 뒤의 클라이언트를 한 칸씩 당긴다
	rebuilt := map[int]*TCClient{}
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

	log.Printf("[티츄][퇴장] game=%s | %s 로비 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), room.Game.PlayerCount, TCSeats)

	if room.Game.PlayerCount == 0 {
		delete(h.rooms, room.Game.ID)
		h.lobby = nil
		lobbySetWaiting("tichu", false)
		return
	}
	seat := oldSeat
	h.broadcastEvent(room, TCEventPayload{Kind: "left", Seat: &seat,
		Message: fmt.Sprintf("%s 퇴장", client.Name)})
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *TCHub) roomOf(client *TCClient) *tcRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Started {
		return nil
	}
	return room
}

func (h *TCHub) handleCallGrand(client *TCClient, msg TCMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload TCCallGrandPayload
	json.Unmarshal(payloadBytes, &payload)

	if _, err := room.Game.CallGrand(client.Seat, payload.Call); err != nil {
		h.sendError(client, err.Error())
		return
	}
	if payload.Call {
		seat := client.Seat
		h.broadcastEvent(room, TCEventPayload{Kind: "grand", Seat: &seat,
			Message: fmt.Sprintf("%s 그랜드 티츄 선언!", client.Name)})
	}
	h.broadcastState(room)
}

func (h *TCHub) handleCallTichu(client *TCClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	if err := room.Game.CallTichu(client.Seat); err != nil {
		h.sendError(client, err.Error())
		return
	}
	seat := client.Seat
	h.broadcastEvent(room, TCEventPayload{Kind: "tichu", Seat: &seat,
		Message: fmt.Sprintf("%s 티츄 선언!", client.Name)})
	h.broadcastState(room)
}

func (h *TCHub) handleExchange(client *TCClient, msg TCMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload TCExchangePayload
	json.Unmarshal(payloadBytes, &payload)

	if _, err := room.Game.SubmitExchange(client.Seat, payload); err != nil {
		h.sendError(client, err.Error())
		return
	}
	seat := client.Seat
	h.broadcastEvent(room, TCEventPayload{Kind: "exchanged", Seat: &seat,
		Message: fmt.Sprintf("%s 교환 완료", client.Name)})
	h.broadcastState(room)
}

func (h *TCHub) handlePlay(client *TCClient, msg TCMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload TCPlayPayload
	json.Unmarshal(payloadBytes, &payload)

	res, err := room.Game.Play(client.Seat, payload.Cards, payload.Wish)
	if err != nil {
		h.sendError(client, err.Error())
		return
	}

	seat := client.Seat
	kind := "play"
	message := fmt.Sprintf("%s: %s", client.Name, res.Combo.Kind)
	if tcIsBombKind(res.Combo.Kind) {
		kind = "bomb"
		message = fmt.Sprintf("%s 폭탄!", client.Name)
	} else if res.Dog {
		kind = "dog"
		message = fmt.Sprintf("%s 개 — 파트너에게 리드", client.Name)
	}
	h.broadcastEvent(room, TCEventPayload{Kind: kind, Seat: &seat, Message: message})

	if res.WishSet > 0 {
		h.broadcastEvent(room, TCEventPayload{Kind: "wish", Seat: &seat,
			Message: fmt.Sprintf("소원: %s", tcRankLabel(res.WishSet))})
	}
	if res.WishDone {
		h.broadcastEvent(room, TCEventPayload{Kind: "wish_done", Seat: &seat,
			Message: "소원 이행"})
	}
	if res.PlayerOut {
		h.broadcastEvent(room, TCEventPayload{Kind: "player_out", Seat: &seat,
			Message: fmt.Sprintf("%s %d위로 아웃", client.Name, res.OutRank)})
	}
	if res.HandEnded {
		result := room.Game.HandResult
		h.broadcastEvent(room, TCEventPayload{Kind: "hand_end", Message: result.Detail})
		log.Printf("[티츄][핸드종료] game=%s | 핸드%d | %s | 누적 02=%d 13=%d",
			room.Game.ID, room.Game.HandNo, result.Detail, room.Game.Score02, room.Game.Score13)
	}

	h.broadcastState(room)

	if res.GameOver {
		h.finishGame(room)
	} else if res.HandEnded {
		h.scheduleHandEnd(room)
	}
}

func (h *TCHub) handlePass(client *TCClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	res, err := room.Game.Pass(client.Seat)
	if err != nil {
		h.sendError(client, err.Error())
		return
	}
	seat := client.Seat
	h.broadcastEvent(room, TCEventPayload{Kind: "pass", Seat: &seat,
		Message: fmt.Sprintf("%s 패스", client.Name)})
	if res.TrickWon {
		winner := res.WinnerSeat
		h.broadcastEvent(room, TCEventPayload{Kind: "trick_won", Seat: &winner,
			Message: fmt.Sprintf("%s 트릭 획득", room.Game.Names[winner])})
	}
	h.broadcastState(room)
}

func (h *TCHub) handleDragonGive(client *TCClient, msg TCMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload TCDragonGivePayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.DragonGive(client.Seat, payload.Seat); err != nil {
		h.sendError(client, err.Error())
		return
	}
	target := payload.Seat
	h.broadcastEvent(room, TCEventPayload{Kind: "dragon_given", Seat: &target,
		Message: fmt.Sprintf("%s → %s 용 트릭 전달", client.Name, room.Game.Names[target])})
	h.broadcastState(room)
}

func (h *TCHub) handleReady(client *TCClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	allReady, changed := room.Game.SetReady(client.Seat)
	if !changed {
		return // 중복 ready 는 조용히 무시 (봇 재전송 대비)
	}
	h.broadcastState(room)
	if allReady {
		h.startNextHand(room)
	}
}

// ==================== hand_end 자동 진행 ====================

// scheduleHandEnd hand_end 진입 시 자동 진행 타이머를 건다.
// 발화는 허브 채널을 경유하므로 허브 밖에서 상태를 만지지 않는다.
func (h *TCHub) scheduleHandEnd(room *tcRoom) {
	sig := tcHandEndSignal{GameID: room.Game.ID, HandNo: room.Game.HandNo}
	room.HandEndTimer = time.AfterFunc(tcHandEndDelay, func() {
		h.handEndFired <- sig
	})
}

func (h *TCHub) handleHandEndFired(sig tcHandEndSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.Game.Phase != TCPhaseHandEnd || room.Game.HandNo != sig.HandNo {
		return
	}
	h.startNextHand(room)
}

func (h *TCHub) startNextHand(room *tcRoom) {
	if room.HandEndTimer != nil {
		room.HandEndTimer.Stop()
		room.HandEndTimer = nil
	}
	room.Game.StartHand(h.rng)
	log.Printf("[티츄][핸드시작] game=%s | 핸드%d", room.Game.ID, room.Game.HandNo)
	h.broadcastState(room)
}

// ==================== 상태 뷰 (은닉의 핵심) ====================

// buildTCState 개인화 게임 스냅샷 — 남의 손패는 개수만 싣는다.
// 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
func (h *TCHub) buildTCState(room *tcRoom, viewerSeat int) TCGameStatePayload {
	g := room.Game

	players := make([]TCPlayerView, TCSeats)
	for s := 0; s < TCSeats; s++ {
		c := room.Clients[s]
		handCount := len(g.Hands[s])
		if g.Phase == TCPhaseGrand {
			handCount = 8 // 나머지 6장은 그랜드 응답 후 배분
		} else if g.Phase == TCPhaseWaiting {
			handCount = 0
		}
		players[s] = TCPlayerView{
			Seat:      s,
			Name:      g.Names[s],
			Connected: c != nil && c.Connected,
			Bot:       c != nil && c.Bot,
			HandCount: handCount,
			Tichu:     g.Tichu[s],
			Out:       g.Out[s],
			OutRank:   g.OutRank[s],
			Ready:     g.Ready[s],
		}
	}

	yourHand := []string{}
	switch g.Phase {
	case TCPhaseWaiting:
	case TCPhaseGrand:
		yourHand = tcSortHand(g.Dealt[viewerSeat][:8])
	default:
		yourHand = tcSortHand(g.Hands[viewerSeat])
	}

	trick := append([]TCTrickPlay{}, g.Trick...)
	var lastPlay *TCTrickPlay
	if len(trick) > 0 {
		lastPlay = &trick[len(trick)-1]
	}

	return TCGameStatePayload{
		GameID:            g.ID,
		Phase:             g.Phase,
		TargetScore:       g.Target,
		YourSeat:          viewerSeat,
		Players:           players,
		YourHand:          yourHand,
		ExchangeDone:      g.ExchangeSub[viewerSeat] != nil,
		GrandAnswered:     g.GrandAnswered[viewerSeat],
		Scores:            TCScores{Team02: g.Score02, Team13: g.Score13},
		HandNo:            g.HandNo,
		CurrentTurn:       g.CurrentTurn,
		Trick:             trick,
		LastPlay:          lastPlay,
		WishRank:          g.WishRank,
		DragonPendingSeat: g.DragonPendingSeat,
		HandResult:        g.HandResult,
		WinnerTeam:        g.WinnerTeam,
	}
}

// broadcastState 좌석마다 개인화 스냅샷을 만들어 보낸다
func (h *TCHub) broadcastState(room *tcRoom) {
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, TCMessage{
			Type:    TCMsgGameState,
			Payload: h.buildTCState(room, seat),
		})
	}
}

func (h *TCHub) broadcastEvent(room *tcRoom, event TCEventPayload) {
	h.broadcastToRoom(room, TCMessage{Type: TCMsgEvent, Payload: event})
}

// finishGame 목표점수 도달 — 결과 통지·전적 기록·방 정리
func (h *TCHub) finishGame(room *tcRoom) {
	g := room.Game
	team02 := displayName(g.Names[0]) + "·" + displayName(g.Names[2])
	team13 := displayName(g.Names[1]) + "·" + displayName(g.Names[3])
	winner, loser := team02, team13
	if g.WinnerTeam == "13" {
		winner, loser = team13, team02
	}

	h.broadcastToRoom(room, TCMessage{
		Type: TCMsgGameOver,
		Payload: TCGameOverPayload{
			WinnerTeam: g.WinnerTeam,
			Scores:     TCScores{Team02: g.Score02, Team13: g.Score13},
		},
	})

	log.Printf("[티츄][경기결과] game=%s | 승자=%s팀(%s) | 점수 02=%d 13=%d | 핸드=%d | 소요=%s",
		g.ID, g.WinnerTeam, winner, g.Score02, g.Score13, g.HandNo, matchDuration(g.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "tichu",
		Players:  winner + " vs " + loser,
		Winner:   winner,
		Reason:   "target",
		Duration: matchSeconds(g.StartedAt),
		Bot:      tcRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, g.ID)
}

// tcRoomHasBot 방에 연습봇이 있는지
func tcRoomHasBot(room *tcRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *TCHub) handleDisconnect(client *TCClient) {
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

	// 대기실: 유예 없이 즉시 자리를 비운다
	if !room.Game.Started {
		h.removeFromLobby(room, client)
		return
	}

	// 진행 중: 유예 시간 동안 재접속을 기다린다 (만료 시 봇 대체)
	log.Printf("[티츄][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, TCMessage{
		Type: TCMsgOpponentDisconnected,
		Payload: TCOpponentDisconnectedPayload{
			Seat:         client.Seat,
			Name:         client.Name,
			GraceSeconds: int(h.grace.Seconds()),
		},
	})
	h.broadcastState(room)

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 안에 재접속하지 않은 좌석은 봇으로 대체하고
// 게임은 계속한다 (세션은 폐기 — 재접속 불가)
func (h *TCHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == TCPhaseGameOver {
		return
	}

	seat := client.Seat
	log.Printf("[티츄][봇대체] game=%s | seat%d=%s 유예 만료 → 봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := &TCClient{wsClient: newBotWSClient(), Hub: h, Seat: seat}
	bot.Name = client.Name // 좌석 이름은 유지 (표시는 bot 플래그로 구분)
	bot.GameID = room.Game.ID
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runTCBot(bot)

	h.broadcastEvent(room, TCEventPayload{Kind: "bot_takeover", Seat: &seat,
		Message: fmt.Sprintf("%s 자리를 봇이 이어받았습니다", client.Name)})
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *TCHub) handleRejoin(client *TCClient, msg TCMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload TCRejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil {
		h.sendToClient(client, TCMessage{Type: TCMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, TCMessage{Type: TCMsgSessionExpired})
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

	log.Printf("[티츄][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, TCMessage{
		Type:    TCMsgReconnected,
		Payload: TCReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	// 전원에게 접속 상태가 반영된 스냅샷 (재접속 당사자 복원 포함)
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *TCHub) clearGameSessions(room *tcRoom) {
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		h.drop(c.SessionID)
	}
}

// ==================== 전송 ====================

func (h *TCHub) sendError(client *TCClient, message string) {
	h.sendToClient(client, TCMessage{Type: TCMsgError, Payload: TCErrorPayload{Message: message}})
}

func (h *TCHub) sendToClient(client *TCClient, message TCMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[TC] ")
}

func (h *TCHub) broadcastToRoom(room *tcRoom, message TCMessage) {
	for _, c := range room.Clients {
		if c != nil {
			h.sendToClient(c, message)
		}
	}
}
