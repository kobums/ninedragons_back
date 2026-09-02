package server

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"time"

	"github.com/google/uuid"
)

// jhRoom 게임(순수 상태)과 역할별 연결의 매핑
type jhRoom struct {
	Game    *JHGame
	Clients map[JHRole]*JHClient

	// ---- 재대결 창 (게임 종료 후) ----
	Rematch      map[JHRole]bool
	CleanupTimer *time.Timer
}

type JHHub struct {
	// 등록된 클라이언트
	clients map[*JHClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*jhRoom

	// 상대를 기다리는 방
	waitingRoom *jhRoom

	// 클라이언트 등록
	register chan *JHClient

	// 클라이언트 등록 해제
	unregister chan *JHClient

	// 게임 메시지
	gameMessage chan JHGameMessage

	// 재대결 창 만료 알림 (gameID)
	roomExpired chan string

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*JHClient]

	// 셔플용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type JHGameMessage struct {
	Client  *JHClient
	Message JHMessage
}

func NewJHHub() *JHHub {
	return &JHHub{
		register:       make(chan *JHClient),
		unregister:     make(chan *JHClient),
		clients:        make(map[*JHClient]bool),
		rooms:          make(map[string]*jhRoom),
		gameMessage:    make(chan JHGameMessage),
		roomExpired:    make(chan string, 8),
		sessionManager: newSessionManager[*JHClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *JHHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[JH] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[JH] Client unregistered: %s", client.ID)
			}

		case sessionID := <-h.graceExpired:
			h.handleGraceExpired(sessionID)

		case gameID := <-h.roomExpired:
			h.handleRoomExpired(gameID)

		case message := <-h.gameMessage:
			h.handleGameMessage(message)
		}
	}
}

func (h *JHHub) handleGameMessage(gm JHGameMessage) {
	switch gm.Message.Type {
	case JHMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case JHMsgRejoinGame:
		h.handleRejoin(gm.Client, gm.Message)
	case JHMsgExchangeCards:
		h.handleExchangeCards(gm.Client, gm.Message)
	case JHMsgPlayCard:
		h.handlePlayCard(gm.Client, gm.Message)
	case JHMsgDeclareSuit:
		h.handleDeclareSuit(gm.Client, gm.Message)
	case JHMsgStealTrick:
		h.handleStealTrick(gm.Client, gm.Message)
	case JHMsgGreedCards:
		h.handleGreedCards(gm.Client, gm.Message)
	case JHMsgRematch:
		h.handleRematch(gm.Client)
	}
}

// ==================== 입장 / 매치메이킹 ====================

func (h *JHHub) handleJoinGame(client *JHClient, msg JHMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하는 것을 막는다
	// (막지 않으면 자기 자신과 매칭되거나 유령 자리가 생긴다)
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload JHJoinGamePayload
	json.Unmarshal(payloadBytes, &payload)
	// 초기 게임군은 playerName, 이후 게임군은 name — 양쪽 표기를 받는다
	payload.PlayerName = resolveJoinName(payloadBytes, payload.PlayerName)

	client.Name = payload.PlayerName
	client.SessionID = uuid.New().String()
	h.sessions[client.SessionID] = client

	var room *jhRoom
	if h.waitingRoom == nil {
		game := NewJHGame(uuid.New().String())
		room = &jhRoom{Game: game, Clients: map[JHRole]*JHClient{}}
		h.waitingRoom = room
		lobbySetWaiting("jekyllhyde", true)
		h.rooms[game.ID] = room
		log.Printf("[JH] Created new game %s", game.ID)
	} else {
		room = h.waitingRoom
		h.waitingRoom = nil
		lobbySetWaiting("jekyllhyde", false)
	}

	role, err := room.Game.AddPlayer(client.Name)
	if err != nil {
		h.sendError(client, err.Error())
		return
	}
	client.GameID = room.Game.ID
	client.Role = role
	room.Clients[role] = client

	log.Printf("[지킬앤하이드][입장] game=%s | %s=%s 게임 입장 (%d/2)",
		room.Game.ID, jhRoleLabel(role), displayName(client.Name), len(room.Game.Names))
	notify("지킬앤하이드 참가", fmt.Sprintf("%s(%s) 입장 (%d/2)",
		displayName(client.Name), jhRoleLabel(role), len(room.Game.Names)))

	h.sendToClient(client, JHMessage{
		Type: JHMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourRole":  role,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
		},
	})

	if room.Game.IsReady() {
		h.startGame(room)
	} else {
		h.sendToClient(client, JHMessage{
			Type:    JHMsgWaitingPlayer,
			Payload: map[string]string{"message": "상대방을 기다리는 중..."},
		})
	}
}

func (h *JHHub) startGame(room *jhRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	game := room.Game

	log.Printf("[지킬앤하이드][경기시작] game=%s | 지킬=%s | 하이드=%s",
		game.ID, displayName(game.Names[JHJekyll]), displayName(game.Names[JHHyde]))
	notify("지킬앤하이드 게임 시작", fmt.Sprintf("%s vs %s",
		displayName(game.Names[JHJekyll]), displayName(game.Names[JHHyde])))

	for role, c := range room.Clients {
		h.sendToClient(c, JHMessage{
			Type: JHMsgGameStart,
			Payload: JHGameStartPayload{
				YourRole:   role,
				JekyllName: game.Names[JHJekyll],
				HydeName:   game.Names[JHHyde],
			},
		})
	}
	h.afterAction(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *JHHub) roomOf(client *JHClient) *jhRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

// afterAction 액션 처리 후 공통 마무리: 이벤트 → 상태 → 종료 확인
func (h *JHHub) afterAction(room *jhRoom) {
	for _, ev := range room.Game.DrainEvents() {
		h.broadcastToRoom(room, JHMessage{Type: JHMsgEvent, Payload: ev})
	}
	h.broadcastState(room)
	h.finishIfOver(room)
}

func (h *JHHub) handleExchangeCards(client *JHClient, msg JHMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload JHExchangeCardsPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.SubmitExchange(client.Role, payload.Indices); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.afterAction(room)
}

func (h *JHHub) handlePlayCard(client *JHClient, msg JHMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload JHPlayCardPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.PlayCard(client.Role, payload.HandIndex); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.afterAction(room)
}

func (h *JHHub) handleDeclareSuit(client *JHClient, msg JHMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload JHDeclareSuitPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.DeclareSuit(client.Role, payload.Suit); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.afterAction(room)
}

func (h *JHHub) handleStealTrick(client *JHClient, msg JHMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload JHStealTrickPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.StealTrick(client.Role, payload.TrickIndex); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.afterAction(room)
}

func (h *JHHub) handleGreedCards(client *JHClient, msg JHMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload JHGreedCardsPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.SubmitGreed(client.Role, payload.Indices); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.afterAction(room)
}

// ==================== 상태 뷰 ====================

// buildJHState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
func (h *JHHub) buildJHState(room *jhRoom, viewer JHRole) JHGameStatePayload {
	game := room.Game
	opp := jhOther(viewer)

	opponentConnected := false
	if c := room.Clients[opp]; c != nil && c.Connected {
		opponentConnected = true
	}

	yourTurn := false
	switch game.Phase {
	case JHPhaseLead, JHPhaseDeclare, JHPhaseFollow, JHPhasePrideSteal:
		yourTurn = game.currentActor() == viewer
	case JHPhaseExchange:
		_, submitted := game.ExchangeSel[viewer]
		yourTurn = !submitted
	case JHPhaseGreedExchange:
		_, submitted := game.GreedSel[viewer]
		yourTurn = !submitted
	}

	youSubmitted, oppSubmitted := false, false
	switch game.Phase {
	case JHPhaseExchange:
		_, youSubmitted = game.ExchangeSel[viewer]
		_, oppSubmitted = game.ExchangeSel[opp]
	case JHPhaseGreedExchange:
		_, youSubmitted = game.GreedSel[viewer]
		_, oppSubmitted = game.GreedSel[opp]
	}

	greedPick := 0
	if game.Phase == JHPhaseGreedExchange {
		greedPick = game.GreedPickCount()
	}

	return JHGameStatePayload{
		GameID:     game.ID,
		YourRole:   viewer,
		Phase:      game.Phase,
		Round:      game.Round,
		Marker:     game.Marker,
		JekyllName: game.Names[JHJekyll],
		HydeName:   game.Names[JHHyde],

		RankOrder:    append([]JHSuit{}, game.RankOrder...),
		Leader:       game.Leader,
		TableLead:    game.TableLead,
		TableFollow:  game.TableFollow,
		DeclaredSuit: game.DeclaredSuit,

		YourHand:     append([]JHCard{}, game.Hands[viewer]...),
		OppHandCount: len(game.Hands[opp]),

		YourTricks: append([]JHTrick{}, game.Tricks[viewer]...),
		OppTricks:  append([]JHTrick{}, game.Tricks[opp]...),

		YourTurn:     yourTurn,
		LegalIndices: game.LegalPlays(viewer),

		ExchangeCount:     game.ExchangeCount(),
		MustIncludePotion: game.MustIncludePotion(viewer),
		YouSubmitted:      youSubmitted,
		OppSubmitted:      oppSubmitted,
		GreedPickCount:    greedPick,

		RoundResults:      append([]JHRoundResult{}, game.RoundResults...),
		OpponentConnected: opponentConnected,
	}
}

// broadcastState 역할마다 개인화 스냅샷을 만들어 보낸다
func (h *JHHub) broadcastState(room *jhRoom) {
	for role, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, JHMessage{
			Type:    JHMsgGameState,
			Payload: h.buildJHState(room, role),
		})
	}
}

// finishIfOver 게임이 끝났으면 종료를 알리고 방을 정리한다
func (h *JHHub) finishIfOver(room *jhRoom) {
	game := room.Game
	if game.Phase != JHPhaseGameOver {
		return
	}

	h.broadcastToRoom(room, JHMessage{
		Type: JHMsgGameOver,
		Payload: JHGameOverPayload{
			Winner:       game.Winner,
			Reason:       game.EndReason,
			Marker:       game.Marker,
			JekyllName:   game.Names[JHJekyll],
			HydeName:     game.Names[JHHyde],
			RoundResults: append([]JHRoundResult{}, game.RoundResults...),
		},
	})

	log.Printf("[지킬앤하이드][경기결과] game=%s | 승자=%s(%s) | 마커=%d | 사유=%s | 소요=%s",
		game.ID, displayName(game.Names[game.Winner]), jhRoleLabel(game.Winner),
		game.Marker, jhEndReasonLabel(game.EndReason), matchDuration(game.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "jekyllhyde",
		Players:  displayName(game.Names[JHJekyll]) + " vs " + displayName(game.Names[JHHyde]),
		Winner:   displayName(game.Names[game.Winner]),
		Reason:   game.EndReason,
		Duration: matchSeconds(game.StartedAt),
	})

	// 재대결 창: 방·세션을 잠시 유지하고 재대결 신청을 기다린다
	room.Rematch = map[JHRole]bool{}
	gameID := game.ID
	room.CleanupTimer = time.AfterFunc(rematchWindow, func() { h.roomExpired <- gameID })
}

// handleRematch 게임 종료 후 재대결 신청. 양쪽 모두 신청하면 재시작.
func (h *JHHub) handleRematch(client *JHClient) {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase != JHPhaseGameOver || room.CleanupTimer == nil {
		return
	}
	if room.Rematch == nil {
		room.Rematch = map[JHRole]bool{}
	}
	room.Rematch[client.Role] = true

	if room.Rematch[jhOther(client.Role)] {
		h.restartRematch(room)
		return
	}
	if opponent := room.Clients[jhOther(client.Role)]; opponent != nil {
		h.sendToClient(opponent, JHMessage{Type: JHMsgRematchOffer})
	}
}

// restartRematch 같은 방에서 새 게임을 시작한다 (연결·세션 유지).
// AddPlayer 규칙(먼저 등록되는 쪽이 지킬)을 따르므로 역할이 바뀔 수 있다 —
// 원작도 라운드마다 역할이 갈리는 게임이라 무방하다.
func (h *JHHub) restartRematch(room *jhRoom) {
	if room.CleanupTimer != nil {
		room.CleanupTimer.Stop()
		room.CleanupTimer = nil
	}
	room.Rematch = nil

	humans := []*JHClient{}
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		humans = append(humans, c)
	}

	game := NewJHGame(room.Game.ID)
	room.Game = game
	room.Clients = map[JHRole]*JHClient{}
	for _, c := range humans {
		role, err := game.AddPlayer(c.Name)
		if err != nil {
			continue
		}
		c.Role = role
		room.Clients[role] = c
		// 프론트가 세션 키를 다시 저장하도록 입장 확인을 재전송한다
		h.sendToClient(c, JHMessage{
			Type: JHMsgPlayerJoined,
			Payload: map[string]interface{}{
				"yourRole":  role,
				"gameId":    game.ID,
				"sessionId": c.SessionID,
			},
		})
	}

	log.Printf("[지킬앤하이드][재대결] game=%s | 같은 방에서 재시작", game.ID)
	h.startGame(room)
}

// handleRoomExpired 재대결 창이 지나도록 신청이 없으면 방·세션 정리
func (h *JHHub) handleRoomExpired(gameID string) {
	room := h.rooms[gameID]
	if room == nil || room.Game.Phase != JHPhaseGameOver {
		return
	}
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, JHMessage{Type: JHMsgSessionExpired})
		h.drop(c.SessionID)
		// 같은 연결이 새 게임에 입장할 수 있도록 신원을 비운다
		// (비우지 않으면 join 연타 가드에 걸려 재입장이 막힌다)
		c.SessionID = ""
		c.GameID = ""
	}
	delete(h.rooms, gameID)
}

// ==================== 재접속 / 연결 끊김 ====================

func (h *JHHub) handleDisconnect(client *JHClient) {
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

	// 상대를 기다리던 방은 유지할 이유가 없으니 즉시 정리
	if !room.Game.Ready {
		delete(h.rooms, room.Game.ID)
		if h.waitingRoom != nil && h.waitingRoom.Game.ID == room.Game.ID {
			h.waitingRoom = nil
			lobbySetWaiting("jekyllhyde", false)
		}
		h.drop(client.SessionID)
		return
	}

	// 진행 중인 게임: 유예 시간 동안 세션을 유지하고 재접속을 기다린다
	log.Printf("[지킬앤하이드][연결끊김] game=%s | %s=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, jhRoleLabel(client.Role), displayName(client.Name), h.grace.Seconds())

	if opponent := room.Clients[jhOther(client.Role)]; opponent != nil {
		h.sendToClient(opponent, JHMessage{
			Type: JHMsgOpponentDisconnected,
			Payload: JHOpponentDisconnectedPayload{
				Message:      "상대방 연결이 끊겼습니다. 재접속을 기다리는 중...",
				GraceSeconds: int(h.grace.Seconds()),
			},
		})
	}

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 시간 안에 재접속하지 않은 세션 정리
func (h *JHHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil {
		return
	}

	log.Printf("[지킬앤하이드][재접속실패] game=%s | %s=%s 유예 시간 만료로 게임 종료",
		room.Game.ID, jhRoleLabel(client.Role), displayName(client.Name))

	if opponent := room.Clients[jhOther(client.Role)]; opponent != nil {
		h.sendToClient(opponent, JHMessage{
			Type: JHMsgError,
			Payload: JHErrorPayload{
				Message: "상대방이 재접속하지 않아 게임이 종료되었습니다",
			},
		})
		// 상대는 로비로 돌아가도록 세션도 만료 처리
		h.sendToClient(opponent, JHMessage{Type: JHMsgSessionExpired})
		h.drop(opponent.SessionID)
		opponent.GameID = ""
	}

	delete(h.rooms, room.Game.ID)
	if h.waitingRoom != nil && h.waitingRoom.Game.ID == room.Game.ID {
		h.waitingRoom = nil
		lobbySetWaiting("jekyllhyde", false)
	}
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *JHHub) handleRejoin(client *JHClient, msg JHMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload JHRejoinGamePayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil {
		h.sendToClient(client, JHMessage{Type: JHMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil || !room.Game.Ready {
		h.drop(payload.SessionID)
		h.sendToClient(client, JHMessage{Type: JHMsgSessionExpired})
		return
	}

	// 유예 타이머 취소
	h.cancelGrace(payload.SessionID)

	// 옛 연결이 아직 살아있다면 강제 종료 (중복 접속 방지)
	if old != client && old.Connected {
		old.Conn.Close()
	}

	// 신원 인계: 새 연결이 기존 플레이어 슬롯을 이어받는다
	client.SessionID = old.SessionID
	client.Name = old.Name
	client.GameID = old.GameID
	client.Role = old.Role
	h.sessions[client.SessionID] = client
	room.Clients[client.Role] = client

	log.Printf("[지킬앤하이드][재접속] game=%s | %s=%s 재접속 완료",
		room.Game.ID, jhRoleLabel(client.Role), displayName(client.Name))

	if opponent := room.Clients[jhOther(client.Role)]; opponent != nil {
		h.sendToClient(opponent, JHMessage{Type: JHMsgOpponentReconnected})
	}

	// 현재 게임 상태 전체를 내려서 클라이언트 상태를 복원시킨다
	h.sendToClient(client, JHMessage{
		Type:    JHMsgGameState,
		Payload: h.buildJHState(room, client.Role),
	})
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *JHHub) clearGameSessions(room *jhRoom) {
	clearRoomSessions(&h.sessionManager, room.Clients)
}

// ==================== 라벨 / 전송 ====================

// jhRoleLabel 역할 한글 표기
func jhRoleLabel(role JHRole) string {
	switch role {
	case JHJekyll:
		return "지킬"
	case JHHyde:
		return "하이드"
	}
	return string(role)
}

// jhEndReasonLabel 종료 사유 한글 표기
func jhEndReasonLabel(reason string) string {
	switch reason {
	case "corrupted":
		return "하이드 완전 잠식"
	case "survived":
		return "지킬 3라운드 버팀"
	case "forfeit":
		return "몰수"
	}
	return reason
}

func (h *JHHub) sendError(client *JHClient, message string) {
	h.sendToClient(client, JHMessage{Type: JHMsgError, Payload: JHErrorPayload{Message: message}})
}

func (h *JHHub) sendToClient(client *JHClient, message JHMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[JH] ")
}

func (h *JHHub) broadcastToRoom(room *jhRoom, message JHMessage) {
	for _, c := range room.Clients {
		if c != nil {
			h.sendToClient(c, message)
		}
	}
}
