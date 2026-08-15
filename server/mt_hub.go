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

// mtRoom 게임(순수 상태)과 좌석별 연결의 매핑
type mtRoom struct {
	Game    *MTGame
	Clients map[int]*MTClient // seat → client
}

type MTHub struct {
	// 등록된 클라이언트
	clients map[*MTClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*mtRoom

	// 글로벌 로비. 시작 전 방은 하나뿐이다.
	lobby *mtRoom

	// 클라이언트 등록
	register chan *MTClient

	// 클라이언트 등록 해제
	unregister chan *MTClient

	// 게임 메시지
	gameMessage chan MTGameMessage

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*MTClient]

	// 셔플용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type MTGameMessage struct {
	Client  *MTClient
	Message MTMessage
}

func NewMTHub() *MTHub {
	return &MTHub{
		register:       make(chan *MTClient),
		unregister:     make(chan *MTClient),
		clients:        make(map[*MTClient]bool),
		rooms:          make(map[string]*mtRoom),
		gameMessage:    make(chan MTGameMessage),
		sessionManager: newSessionManager[*MTClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *MTHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[MT] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[MT] Client unregistered: %s", client.ID)
			}

		case sessionID := <-h.graceExpired:
			h.handleGraceExpired(sessionID)

		case message := <-h.gameMessage:
			h.handleGameMessage(message)
		}
	}
}

func (h *MTHub) handleGameMessage(gm MTGameMessage) {
	switch gm.Message.Type {
	case MTMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case MTMsgFillBots:
		h.handleFillBots(gm.Client)
	case MTMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	case MTMsgBid:
		h.handleBid(gm.Client, gm.Message)
	case MTMsgPass:
		h.handlePass(gm.Client)
	case MTMsgKitty:
		h.handleKitty(gm.Client, gm.Message)
	case MTMsgFriend:
		h.handleFriend(gm.Client, gm.Message)
	case MTMsgPlay:
		h.handlePlay(gm.Client, gm.Message)
	}
}

// ==================== 로비 ====================

func (h *MTHub) handleJoinGame(client *MTClient, msg MTMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload MTJoinGamePayload
	json.Unmarshal(payloadBytes, &payload)

	if h.lobby == nil {
		game := NewMTGame(uuid.New().String())
		h.lobby = &mtRoom{Game: game, Clients: map[int]*MTClient{}}
		h.rooms[game.ID] = h.lobby
		log.Printf("[MT] Created lobby game %s", game.ID)
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

	log.Printf("[마이티][입장] game=%s | seat%d=%s 게임 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), MTPlayerCount)
	notify("마이티 참가", fmt.Sprintf("%s 입장 (%d/%d)",
		displayName(client.Name), len(room.Game.Players), MTPlayerCount))

	h.sendToClient(client, MTMessage{
		Type: MTMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSeat":  seat,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
		},
	})

	h.broadcastEvent(room, MTEventPayload{Kind: "joined", Seat: &seat, Name: client.Name})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)

	if len(room.Game.Players) == MTPlayerCount {
		h.startGame(room)
	}
}

// hostSeat 현재 접속 중인 사람의 가장 낮은 좌석 (호스트)
func (h *MTHub) hostSeat(room *mtRoom) int {
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

// handleFillBots host 가 남은 좌석을 연습봇으로 채운다 → 5좌석이 차며 자동 시작
func (h *MTHub) handleFillBots(client *MTClient) {
	room := h.lobby
	if room == nil || client.GameID != room.Game.ID {
		h.sendError(client, "로비를 찾을 수 없습니다")
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
	for len(room.Game.Players) < MTPlayerCount {
		botNo++
		if h.spawnBot(room, fmt.Sprintf("%s%d", botName, botNo)) == nil {
			break
		}
	}
	h.updateLobbyWaiting(room)
	h.broadcastState(room)

	if len(room.Game.Players) == MTPlayerCount {
		h.startGame(room)
	}
}

// mtHumanCount 방의 사람 수
func mtHumanCount(room *mtRoom) int {
	n := 0
	for _, c := range room.Clients {
		if c != nil && !c.Bot {
			n++
		}
	}
	return n
}

// mtRoomHasBot 방에 연습봇이 있는지 (ntfy 억제 판단용)
func mtRoomHasBot(room *mtRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}

// updateLobbyWaiting 로비 현황판 갱신 — 사람 1명 이상 대기 && 미시작
func (h *MTHub) updateLobbyWaiting(room *mtRoom) {
	waiting := h.lobby != nil && h.lobby == room && !room.Game.Ready && mtHumanCount(room) >= 1
	lobbySetWaiting("mighty", waiting)
}

func (h *MTHub) startGame(room *mtRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	h.lobby = nil // 시작한 방은 로비에서 떼어낸다
	lobbySetWaiting("mighty", false)

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	log.Printf("[마이티][경기시작] game=%s | 인원=%d | 선입찰=seat%d | %v",
		room.Game.ID, len(room.Game.Players), room.Game.BidTurn, names)
	if !mtRoomHasBot(room) {
		notify("마이티 게임 시작", fmt.Sprintf("%d인전 시작", len(room.Game.Players)))
	}

	h.broadcastState(room)
}

// removeFromLobby 대기 중 이탈 — 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *MTHub) removeFromLobby(room *mtRoom, client *MTClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	rebuilt := map[int]*MTClient{}
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

	log.Printf("[마이티][퇴장] game=%s | %s 대기 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), MTPlayerCount)

	// 사람이 아무도 없으면 방 자체를 정리한다 (남은 봇 러너도 종료)
	if mtHumanCount(room) == 0 {
		for _, c := range room.Clients {
			if c == nil {
				continue
			}
			h.sendToClient(c, MTMessage{Type: MTMsgSessionExpired})
			h.drop(c.SessionID)
		}
		delete(h.rooms, room.Game.ID)
		if h.lobby == room {
			h.lobby = nil
		}
		lobbySetWaiting("mighty", false)
		return
	}

	h.broadcastEvent(room, MTEventPayload{Kind: "left", Name: client.Name})
	h.updateLobbyWaiting(room)
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *MTHub) roomOf(client *MTClient) *mtRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

func (h *MTHub) handleBid(client *MTClient, msg MTMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload MTBidPayload
	json.Unmarshal(payloadBytes, &payload)

	step, err := room.Game.Bid(client.Seat, payload.Suit, payload.Count)
	if err != nil {
		h.sendError(client, err.Error())
		return
	}
	seat := client.Seat
	count := payload.Count
	h.broadcastEvent(room, MTEventPayload{Kind: "bid", Seat: &seat, Suit: payload.Suit, Count: &count})
	h.afterBidStep(room, step)
}

func (h *MTHub) handlePass(client *MTClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	step, err := room.Game.Pass(client.Seat)
	if err != nil {
		h.sendError(client, err.Error())
		return
	}
	seat := client.Seat
	h.broadcastEvent(room, MTEventPayload{Kind: "pass", Seat: &seat})
	h.afterBidStep(room, step)
}

// afterBidStep 입찰 액션의 공통 마무리 — 재배분 또는 낙찰 처리 후 스냅샷
func (h *MTHub) afterBidStep(room *mtRoom, step mtBidStep) {
	game := room.Game
	if step.Redeal {
		log.Printf("[마이티][재배분] game=%s | 전원 패스 (%d회차)", game.ID, game.RedealCount+1)
		h.broadcastEvent(room, MTEventPayload{Kind: "redeal"})
		game.Redeal(h.rng)
	}
	if step.Awarded {
		declarer := game.Declarer
		count := game.ContractCount
		log.Printf("[마이티][낙찰] game=%s | 주공=seat%d(%s) | 공약=%s%d",
			game.ID, declarer, displayName(game.Players[declarer].Name), game.Trump, count)
		h.broadcastEvent(room, MTEventPayload{Kind: "contract", Seat: &declarer, Suit: game.Trump, Count: &count})
	}
	h.broadcastState(room)
}

func (h *MTHub) handleKitty(client *MTClient, msg MTMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload MTKittyPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.SubmitKitty(client.Seat, payload.Discard, payload.Suit); err != nil {
		h.sendError(client, err.Error())
		return
	}
	game := room.Game
	seat := client.Seat
	count := game.ContractCount
	// 버림 카드는 비밀 — 이벤트에는 싣지 않는다
	h.broadcastEvent(room, MTEventPayload{Kind: "kitty_done", Seat: &seat})
	h.broadcastEvent(room, MTEventPayload{Kind: "contract", Seat: &seat, Suit: game.Trump, Count: &count})
	h.broadcastState(room)
}

func (h *MTHub) handleFriend(client *MTClient, msg MTMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload MTFriendPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.SetFriend(client.Seat, payload.Type, payload.Card); err != nil {
		h.sendError(client, err.Error())
		return
	}
	game := room.Game
	seat := client.Seat
	log.Printf("[마이티][프렌드] game=%s | 유형=%s 카드=%s", game.ID, game.FriendType, game.FriendCard)
	h.broadcastEvent(room, MTEventPayload{
		Kind: "friend_set", Seat: &seat,
		FriendType: game.FriendType, Card: game.FriendCard,
	})
	h.broadcastState(room)
}

func (h *MTHub) handlePlay(client *MTClient, msg MTMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload MTPlayPayload
	json.Unmarshal(payloadBytes, &payload)

	step, err := room.Game.Play(client.Seat, payload.Card, payload.JokerCall, payload.JokerSuit)
	if err != nil {
		h.sendError(client, err.Error())
		return
	}
	game := room.Game
	seat := client.Seat

	h.broadcastEvent(room, MTEventPayload{Kind: "play", Seat: &seat, Card: payload.Card})
	if step.JokerCall {
		h.broadcastEvent(room, MTEventPayload{Kind: "joker_call", Seat: &seat})
	}
	if step.FriendRevealed {
		friendSeat := game.FriendSeat
		h.broadcastEvent(room, MTEventPayload{
			Kind: "friend_revealed", Seat: &friendSeat,
			Name: game.Players[friendSeat].Name, Card: game.FriendCard,
		})
	}
	if step.TrickDone {
		winner := step.TrickWinner
		trickNo := step.WonTrickNo
		h.broadcastEvent(room, MTEventPayload{Kind: "trick_won", Winner: &winner, TrickNo: &trickNo})
	}

	h.broadcastState(room)
	h.finishIfOver(room)
}

// ==================== 상태 뷰 (은닉의 핵심) ====================

// buildMTState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
// 은닉: 남의 손패는 개수만, 키티는 kitty 단계의 주공에게만.
func (h *MTHub) buildMTState(room *mtRoom, viewerSeat int) MTGameStatePayload {
	game := room.Game

	players := []MTPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		players = append(players, MTPlayerView{
			Seat:           p.Seat,
			Name:           p.Name,
			Connected:      c != nil && c.Connected,
			Bot:            c != nil && c.Bot,
			HandCount:      len(game.Hands[p.Seat]),
			Passed:         game.Passed[p.Seat],
			IsDeclarer:     game.Ready && game.Phase != MTPhaseBidding && p.Seat == game.Declarer,
			FriendRevealed: game.FriendRevealed && game.FriendSeat == p.Seat,
			TricksWon:      game.TricksWon[p.Seat],
			CapturedPoints: game.CapturedPoints[p.Seat],
		})
	}

	var best *MTBidView
	if game.Best != nil {
		best = &MTBidView{Seat: game.Best.Seat, Suit: game.Best.Suit, Count: game.Best.Count}
	}

	var contract *MTContractView
	if game.Ready && game.Phase != MTPhaseWaiting && game.Phase != MTPhaseBidding {
		contract = &MTContractView{Declarer: game.Declarer, Suit: game.Trump, Count: game.ContractCount}
	}

	var yourKitty []string
	if game.Phase == MTPhaseKitty && viewerSeat == game.Declarer {
		yourKitty = append([]string{}, game.Kitty...)
	}

	friendCard := ""
	if game.FriendType == "card" {
		friendCard = game.FriendCard
	}

	return MTGameStatePayload{
		GameID:   game.ID,
		Phase:    game.Phase,
		YourSeat: viewerSeat,
		Players:  players,
		YourHand: append([]string{}, game.Hands[viewerSeat]...),
		Bidding:  MTBiddingView{Turn: game.BidTurn, Best: best},
		Contract: contract,
		YourKitty: yourKitty,
		Friend: MTFriendView{
			Type:     game.FriendType,
			Card:     friendCard,
			Revealed: game.FriendRevealed,
			Seat:     game.FriendSeat,
		},
		TrickNo:         game.TrickNo,
		CurrentTurn:     game.Turn,
		LedSuit:         game.LedSuit,
		JokerCallActive: game.JokerCallActive,
		Trick:           append([]MTTrickPlay{}, game.Trick...),
		LastTrick:       game.LastTrick,
		Result:          game.Result,
	}
}

// broadcastState 좌석마다 개인화 스냅샷을 만들어 보낸다
func (h *MTHub) broadcastState(room *mtRoom) {
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, MTMessage{
			Type:    MTMsgGameState,
			Payload: h.buildMTState(room, seat),
		})
	}
}

func (h *MTHub) broadcastEvent(room *mtRoom, event MTEventPayload) {
	h.broadcastToRoom(room, MTMessage{Type: MTMsgEvent, Payload: event})
}

// finishIfOver 게임이 끝났으면 결과를 알리고 방을 정리한다 (단판 승부 — 재대결 없음)
func (h *MTHub) finishIfOver(room *mtRoom) {
	game := room.Game
	if game.Phase != MTPhaseGameOver || game.Result == nil {
		return
	}
	result := game.Result

	h.broadcastToRoom(room, MTMessage{Type: MTMsgGameOver, Payload: result})

	outcome := "실패"
	if result.Win {
		outcome = "성공"
	}
	declNames := strings.Join(result.DeclarerTeam, "·")
	defNames := strings.Join(result.DefenderTeam, "·")
	detail := fmt.Sprintf("주공팀(%s) 공약 %s — 주공팀 %d점(키티 %d 포함) vs 수비팀 %d점",
		outcome, result.Contract.Suit+fmt.Sprint(result.Contract.Count),
		result.DeclarerPoints, game.KittyPoints, result.DefenderPoints)

	log.Printf("[마이티][경기결과] game=%s | %s | 소요=%s",
		game.ID, detail, matchDuration(game.StartedAt))

	winner, loser := declNames, defNames
	if !result.Win {
		winner, loser = defNames, declNames
	}
	RecordMatch(MatchRecord{
		Game:     "mighty",
		Players:  winner + " vs " + loser,
		Winner:   winner,
		Reason:   detail,
		Duration: matchSeconds(game.StartedAt),
		Bot:      mtRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *MTHub) handleDisconnect(client *MTClient) {
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
	log.Printf("[마이티][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, MTMessage{
		Type: MTMsgOpponentDisconnected,
		Payload: MTOpponentDisconnectedPayload{
			Seat:         client.Seat,
			Name:         client.Name,
			GraceSeconds: int(h.grace.Seconds()),
		},
	})
	h.broadcastState(room)

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 안에 재접속하지 않은 좌석은 연습봇으로 대체하고
// 게임은 계속한다 (5인 게임은 한 명 빠지면 성립하지 않는다)
func (h *MTHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == MTPhaseGameOver || !room.Game.Ready {
		return
	}

	seat := client.Seat
	log.Printf("[마이티][봇대체] game=%s | seat%d=%s 유예 만료 → 연습봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := h.takeoverBot(room, seat, client.Name)
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.broadcastEvent(room, MTEventPayload{Kind: "bot_takeover", Seat: &seat, Name: client.Name})
	// 새 봇이 이 스냅샷을 받아 자기 차례면 즉시 이어간다
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *MTHub) handleRejoin(client *MTClient, msg MTMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload MTRejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil || old.Bot {
		h.sendToClient(client, MTMessage{Type: MTMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, MTMessage{Type: MTMsgSessionExpired})
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

	log.Printf("[마이티][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, MTMessage{
		Type:    MTMsgReconnected,
		Payload: MTReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	// 전원에게 접속 상태가 반영된 스냅샷 (재접속 당사자 복원 포함)
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *MTHub) clearGameSessions(room *mtRoom) {
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		h.drop(c.SessionID)
	}
}

// ==================== 전송 ====================

func (h *MTHub) sendError(client *MTClient, message string) {
	h.sendToClient(client, MTMessage{Type: MTMsgError, Payload: MTErrorPayload{Message: message}})
}

func (h *MTHub) sendToClient(client *MTClient, message MTMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[MT] ")
}

func (h *MTHub) broadcastToRoom(room *mtRoom, message MTMessage) {
	for _, c := range room.Clients {
		if c != nil {
			h.sendToClient(c, message)
		}
	}
}
