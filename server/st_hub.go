package server

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"time"

	"github.com/google/uuid"
)

// stRoom 게임(순수 상태)과 진영별 연결의 매핑
type stRoom struct {
	Game    *STGame
	Clients map[STSide]*STClient
}

type STHub struct {
	// 등록된 클라이언트
	clients map[*STClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*stRoom

	// 모드별(기본/전술) 상대를 기다리는 방
	waitingRooms map[bool]*stRoom

	// 클라이언트 등록
	register chan *STClient

	// 클라이언트 등록 해제
	unregister chan *STClient

	// 게임 메시지
	gameMessage chan STGameMessage

	// 세션 ID → 현재(또는 마지막) 클라이언트
	sessions map[string]*STClient

	// 세션 ID → 재접속 대기 타이머
	graceTimers map[string]*time.Timer

	// 재접속 대기 시간 만료 알림
	graceExpired chan string

	// 재접속 대기 시간
	grace time.Duration

	// 셔플·선공 결정용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type STGameMessage struct {
	Client  *STClient
	Message STMessage
}

func NewSTHub() *STHub {
	return &STHub{
		register:     make(chan *STClient),
		unregister:   make(chan *STClient),
		clients:      make(map[*STClient]bool),
		rooms:        make(map[string]*stRoom),
		waitingRooms: make(map[bool]*stRoom),
		gameMessage:  make(chan STGameMessage),
		sessions:     make(map[string]*STClient),
		graceTimers:  make(map[string]*time.Timer),
		graceExpired: make(chan string),
		grace:        defaultDisconnectGrace,
		rng:          rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *STHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[ST] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[ST] Client unregistered: %s", client.ID)
			}

		case sessionID := <-h.graceExpired:
			h.handleGraceExpired(sessionID)

		case message := <-h.gameMessage:
			h.handleGameMessage(message)
		}
	}
}

func (h *STHub) handleGameMessage(gm STGameMessage) {
	switch gm.Message.Type {
	case STMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case STMsgRejoinGame:
		h.handleRejoin(gm.Client, gm.Message)
	case STMsgPlayCard:
		h.handlePlayCard(gm.Client, gm.Message)
	case STMsgPlayRuse:
		h.handlePlayRuse(gm.Client, gm.Message)
	case STMsgClaimStone:
		h.handleClaimStone(gm.Client, gm.Message)
	case STMsgEndTurn:
		h.handleEndTurn(gm.Client)
	case STMsgDraw:
		h.handleDraw(gm.Client, gm.Message)
	case STMsgPass:
		h.handlePass(gm.Client)
	case STMsgRecruiterDraw:
		h.handleRecruiterDraw(gm.Client, gm.Message)
	case STMsgRecruiterReturn:
		h.handleRecruiterReturn(gm.Client, gm.Message)
	}
}

// ==================== 입장 / 매치메이킹 ====================

func (h *STHub) handleJoinGame(client *STClient, msg STMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하는 것을 막는다
	// (막지 않으면 자기 자신과 매칭되거나 유령 자리가 생긴다)
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload STJoinGamePayload
	json.Unmarshal(payloadBytes, &payload)

	client.Name = payload.PlayerName
	client.SessionID = uuid.New().String()
	h.sessions[client.SessionID] = client

	tacticMode := payload.Mode == "tactic"

	var room *stRoom
	if h.waitingRooms[tacticMode] == nil {
		game := NewSTGame(uuid.New().String(), tacticMode)
		room = &stRoom{Game: game, Clients: map[STSide]*STClient{}}
		h.waitingRooms[tacticMode] = room
		h.rooms[game.ID] = room
		log.Printf("[ST] Created new game %s (tactic=%v)", game.ID, tacticMode)
	} else {
		room = h.waitingRooms[tacticMode]
		delete(h.waitingRooms, tacticMode)
	}

	side, err := room.Game.AddPlayer(client.Name)
	if err != nil {
		h.sendError(client, err.Error())
		return
	}
	client.GameID = room.Game.ID
	client.Side = side
	room.Clients[side] = client

	log.Printf("[쇼텐토텐][입장] game=%s | %s=%s 게임 입장 (%d/2)",
		room.Game.ID, stSideLabel(side), displayName(client.Name), len(room.Game.Names))
	notify("쇼텐토텐 참가", fmt.Sprintf("%s(%s) 입장 (%d/2)",
		displayName(client.Name), stSideLabel(side), len(room.Game.Names)))

	h.sendToClient(client, STMessage{
		Type: STMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSide":  side,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
		},
	})

	if room.Game.IsReady() {
		h.startGame(room)
	} else {
		h.sendToClient(client, STMessage{
			Type:    STMsgWaitingPlayer,
			Payload: map[string]string{"message": "상대방을 기다리는 중..."},
		})
	}
}

func (h *STHub) startGame(room *stRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	game := room.Game

	log.Printf("[쇼텐토텐][경기시작] game=%s | 남=%s | 북=%s | 선공=%s",
		game.ID, displayName(game.Names[STSouth]), displayName(game.Names[STNorth]),
		stSideLabel(game.CurrentSide))
	notify("쇼텐토텐 게임 시작", fmt.Sprintf("%s vs %s",
		displayName(game.Names[STSouth]), displayName(game.Names[STNorth])))

	for side, c := range room.Clients {
		h.sendToClient(c, STMessage{
			Type: STMsgGameStart,
			Payload: STGameStartPayload{
				YourSide:  side,
				FirstSide: game.CurrentSide,
				SouthName: game.Names[STSouth],
				NorthName: game.Names[STNorth],
			},
		})
	}
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *STHub) roomOf(client *STClient) *stRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

func (h *STHub) handlePlayCard(client *STClient, msg STMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload STPlayCardPayload
	json.Unmarshal(payloadBytes, &payload)

	// 이벤트에 실을 카드를 미리 읽어둔다 (PlayCard 가 손패에서 제거하므로)
	var played *STCard
	if hand := room.Game.Hands[client.Side]; payload.HandIndex >= 0 && payload.HandIndex < len(hand) {
		card := hand[payload.HandIndex]
		played = &card
	}

	if err := room.Game.PlayCard(client.Side, payload.HandIndex, payload.StoneIndex); err != nil {
		h.sendError(client, err.Error())
		return
	}

	h.broadcastEvent(room, STEventPayload{
		Kind:       "card_played",
		Side:       client.Side,
		StoneIndex: payload.StoneIndex,
		Card:       played,
	})
	h.broadcastState(room)
	h.finishIfOver(room)
}

func (h *STHub) handleClaimStone(client *STClient, msg STMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload STClaimStonePayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.ClaimStone(client.Side, payload.StoneIndex); err != nil {
		h.sendError(client, err.Error())
		return
	}

	log.Printf("[쇼텐토텐][획득] game=%s | %s=%s 돌 %d 획득",
		room.Game.ID, stSideLabel(client.Side), displayName(client.Name), payload.StoneIndex+1)

	h.broadcastEvent(room, STEventPayload{
		Kind:       "stone_claimed",
		Side:       client.Side,
		StoneIndex: payload.StoneIndex,
	})
	h.broadcastState(room)
	h.finishIfOver(room)
}

func (h *STHub) handleEndTurn(client *STClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	if err := room.Game.EndTurn(client.Side); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.broadcastState(room)
	h.finishIfOver(room)
}

func (h *STHub) handlePlayRuse(client *STClient, msg STMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload STPlayRusePayload
	json.Unmarshal(payloadBytes, &payload)

	// 이벤트에 실을 카드를 미리 읽어둔다
	var played *STCard
	if hand := room.Game.Hands[client.Side]; payload.HandIndex >= 0 && payload.HandIndex < len(hand) {
		card := hand[payload.HandIndex]
		played = &card
	}

	if err := room.Game.PlayRuse(client.Side, payload); err != nil {
		h.sendError(client, err.Error())
		return
	}

	h.broadcastEvent(room, STEventPayload{
		Kind: "ruse_played",
		Side: client.Side,
		Card: played,
	})
	h.broadcastState(room)
	h.finishIfOver(room)
}

func (h *STHub) handleDraw(client *STClient, msg STMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload STDrawPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.DrawFrom(client.Side, payload.Deck); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.broadcastState(room)
	h.finishIfOver(room)
}

func (h *STHub) handlePass(client *STClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	if err := room.Game.Pass(client.Side); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.broadcastEvent(room, STEventPayload{Kind: "turn_passed", Side: client.Side})
	h.broadcastState(room)
	h.finishIfOver(room)
}

func (h *STHub) handleRecruiterDraw(client *STClient, msg STMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload STRecruiterDrawPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.RecruiterDraw(client.Side, payload.Deck); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.broadcastState(room)
}

func (h *STHub) handleRecruiterReturn(client *STClient, msg STMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload STRecruiterReturnPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.RecruiterReturn(client.Side, payload.HandIndex); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.broadcastState(room)
	h.finishIfOver(room)
}

// ==================== 상태 뷰 ====================

// buildSTState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
func (h *STHub) buildSTState(room *stRoom, viewerSide STSide) STGameStatePayload {
	game := room.Game
	opp := stOther(viewerSide)

	opponentConnected := false
	if c := room.Clients[opp]; c != nil && c.Connected {
		opponentConnected = true
	}

	// 획득 가능 표시는 claim 단계의 현재 턴 플레이어에게만 제공한다
	claimable := map[int]bool{}
	if game.Phase == STPhaseClaim && game.CurrentSide == viewerSide {
		for _, i := range game.ClaimableStones(viewerSide) {
			claimable[i] = true
		}
	}

	stones := make([]STStoneView, STStoneCount)
	for i, stone := range game.Stones {
		owner := ""
		if stone.Owner == viewerSide {
			owner = "you"
		} else if stone.Owner == opp {
			owner = "opponent"
		}
		stones[i] = STStoneView{
			Index:     i,
			YourCards: append([]STCard{}, stone.Cards[viewerSide]...),
			OppCards:  append([]STCard{}, stone.Cards[opp]...),
			Owner:     owner,
			Claimable: claimable[i],
			Blind:     stone.Blind,
			Mud:       stone.Mud,
			Required:  stone.Required(),
		}
	}

	canPass := game.TacticMode && game.Phase == STPhasePlay &&
		game.CurrentSide == viewerSide && !game.clanPlayable(viewerSide)

	return STGameStatePayload{
		GameID:            game.ID,
		YourSide:          viewerSide,
		TacticMode:        game.TacticMode,
		Phase:             game.Phase,
		CurrentSide:       game.CurrentSide,
		DeckCount:         len(game.Deck),
		TacticDeckCount:   len(game.TacticDeck),
		Discard:           append([]STCard{}, game.Discard...),
		YourHand:          append([]STCard{}, game.Hands[viewerSide]...),
		OpponentHandCount: len(game.Hands[opp]),
		Stones:            stones,
		SouthName:         game.Names[STSouth],
		NorthName:         game.Names[STNorth],
		YourStoneCount:    game.stoneCount(viewerSide),
		OppStoneCount:     game.stoneCount(opp),
		YourPlayedTactics: game.PlayedTactics[viewerSide],
		OppPlayedTactics:  game.PlayedTactics[opp],
		CanPass:           canPass,
		RecruiterDraws:    game.RecruiterDraws,
		RecruiterReturns:  game.RecruiterReturns,
		OpponentConnected: opponentConnected,
	}
}

// broadcastState 진영마다 개인화 스냅샷을 만들어 보낸다
func (h *STHub) broadcastState(room *stRoom) {
	for side, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, STMessage{
			Type:    STMsgGameState,
			Payload: h.buildSTState(room, side),
		})
	}
}

func (h *STHub) broadcastEvent(room *stRoom, event STEventPayload) {
	h.broadcastToRoom(room, STMessage{Type: STMsgEvent, Payload: event})
}

// finishIfOver 게임이 끝났으면 종료를 알리고 방을 정리한다
func (h *STHub) finishIfOver(room *stRoom) {
	game := room.Game
	if game.Phase != STPhaseGameOver {
		return
	}

	h.broadcastToRoom(room, STMessage{
		Type: STMsgGameOver,
		Payload: STGameOverPayload{
			Winner:     string(game.Winner),
			Reason:     game.EndReason,
			SouthName:  game.Names[STSouth],
			NorthName:  game.Names[STNorth],
			SouthCount: game.stoneCount(STSouth),
			NorthCount: game.stoneCount(STNorth),
		},
	})

	winnerLabel := "무승부"
	if game.Winner != "" {
		winnerLabel = fmt.Sprintf("%s(%s)", displayName(game.Names[game.Winner]), stSideLabel(game.Winner))
	}
	log.Printf("[쇼텐토텐][경기결과] game=%s | 승자=%s | 남=%s(%d개) vs 북=%s(%d개) | 사유=%s | 소요=%s",
		game.ID, winnerLabel,
		displayName(game.Names[STSouth]), game.stoneCount(STSouth),
		displayName(game.Names[STNorth]), game.stoneCount(STNorth),
		stEndReasonLabel(game.EndReason), matchDuration(game.StartedAt))

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
}

// ==================== 재접속 / 연결 끊김 ====================

func (h *STHub) handleDisconnect(client *STClient) {
	// 게임 참가 전에 끊긴 연결
	if client.SessionID == "" {
		return
	}
	// 이미 새 연결로 교체된 세션의 옛 연결
	if h.sessions[client.SessionID] != client {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil {
		delete(h.sessions, client.SessionID)
		return
	}

	// 상대를 기다리던 방은 유지할 이유가 없으니 즉시 정리
	if !room.Game.Ready {
		delete(h.rooms, room.Game.ID)
		h.clearWaiting(room)
		delete(h.sessions, client.SessionID)
		return
	}

	// 진행 중인 게임: 유예 시간 동안 세션을 유지하고 재접속을 기다린다
	log.Printf("[쇼텐토텐][연결끊김] game=%s | %s=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, stSideLabel(client.Side), displayName(client.Name), h.grace.Seconds())

	if opponent := room.Clients[stOther(client.Side)]; opponent != nil {
		h.sendToClient(opponent, STMessage{
			Type: STMsgOpponentDisconnected,
			Payload: STOpponentDisconnectedPayload{
				Message:      "상대방 연결이 끊겼습니다. 재접속을 기다리는 중...",
				GraceSeconds: int(h.grace.Seconds()),
			},
		})
	}

	sessionID := client.SessionID
	h.graceTimers[sessionID] = time.AfterFunc(h.grace, func() {
		h.graceExpired <- sessionID
	})
}

// handleGraceExpired 유예 시간 안에 재접속하지 않은 세션 정리
func (h *STHub) handleGraceExpired(sessionID string) {
	delete(h.graceTimers, sessionID)

	client := h.sessions[sessionID]
	if client == nil || client.Connected {
		// 그 사이 재접속했으면 아무것도 하지 않는다
		return
	}
	delete(h.sessions, sessionID)

	room := h.rooms[client.GameID]
	if room == nil {
		return
	}

	log.Printf("[쇼텐토텐][재접속실패] game=%s | %s=%s 유예 시간 만료로 게임 종료",
		room.Game.ID, stSideLabel(client.Side), displayName(client.Name))

	if opponent := room.Clients[stOther(client.Side)]; opponent != nil {
		h.sendToClient(opponent, STMessage{
			Type: STMsgError,
			Payload: STErrorPayload{
				Message: "상대방이 재접속하지 않아 게임이 종료되었습니다",
			},
		})
		// 상대는 로비로 돌아가도록 세션도 만료 처리
		h.sendToClient(opponent, STMessage{Type: STMsgSessionExpired})
		delete(h.sessions, opponent.SessionID)
		opponent.GameID = ""
	}

	delete(h.rooms, room.Game.ID)
	h.clearWaiting(room)
}

// clearWaiting 대기 큐에서 해당 방을 제거한다
func (h *STHub) clearWaiting(room *stRoom) {
	for mode, waiting := range h.waitingRooms {
		if waiting != nil && waiting.Game.ID == room.Game.ID {
			delete(h.waitingRooms, mode)
		}
	}
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *STHub) handleRejoin(client *STClient, msg STMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload STRejoinGamePayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil {
		h.sendToClient(client, STMessage{Type: STMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil || !room.Game.Ready {
		if t := h.graceTimers[payload.SessionID]; t != nil {
			t.Stop()
			delete(h.graceTimers, payload.SessionID)
		}
		delete(h.sessions, payload.SessionID)
		h.sendToClient(client, STMessage{Type: STMsgSessionExpired})
		return
	}

	// 유예 타이머 취소
	if t := h.graceTimers[payload.SessionID]; t != nil {
		t.Stop()
		delete(h.graceTimers, payload.SessionID)
	}

	// 옛 연결이 아직 살아있다면 강제 종료 (중복 접속 방지)
	if old != client && old.Connected {
		old.Conn.Close()
	}

	// 신원 인계: 새 연결이 기존 플레이어 슬롯을 이어받는다
	client.SessionID = old.SessionID
	client.Name = old.Name
	client.GameID = old.GameID
	client.Side = old.Side
	h.sessions[client.SessionID] = client
	room.Clients[client.Side] = client

	log.Printf("[쇼텐토텐][재접속] game=%s | %s=%s 재접속 완료",
		room.Game.ID, stSideLabel(client.Side), displayName(client.Name))

	if opponent := room.Clients[stOther(client.Side)]; opponent != nil {
		h.sendToClient(opponent, STMessage{Type: STMsgOpponentReconnected})
	}

	// 현재 게임 상태 전체를 내려서 클라이언트 상태를 복원시킨다
	h.sendToClient(client, STMessage{
		Type:    STMsgGameState,
		Payload: h.buildSTState(room, client.Side),
	})
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *STHub) clearGameSessions(room *stRoom) {
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		if t := h.graceTimers[c.SessionID]; t != nil {
			t.Stop()
			delete(h.graceTimers, c.SessionID)
		}
		delete(h.sessions, c.SessionID)
	}
}

// ==================== 라벨 / 전송 ====================

// stSideLabel 진영 한글 표기
func stSideLabel(side STSide) string {
	switch side {
	case STSouth:
		return "남"
	case STNorth:
		return "북"
	}
	return string(side)
}

// stEndReasonLabel 종료 사유 한글 표기
func stEndReasonLabel(reason string) string {
	switch reason {
	case "five_stones":
		return "돌 5개 획득"
	case "three_adjacent":
		return "인접 돌 3개 획득"
	case "stalemate":
		return "교착 (돌 개수 승부)"
	case "forfeit":
		return "몰수"
	}
	return reason
}

func (h *STHub) sendError(client *STClient, message string) {
	h.sendToClient(client, STMessage{Type: STMsgError, Payload: STErrorPayload{Message: message}})
}

func (h *STHub) sendToClient(client *STClient, message STMessage) {
	// 연결이 끊긴(재접속 대기 중) 플레이어에게는 보내지 않는다
	if client == nil || !client.Connected {
		return
	}

	data, err := json.Marshal(message)
	if err != nil {
		log.Printf("[ST] Error marshaling message: %v", err)
		return
	}

	select {
	case client.Send <- data:
	default:
		// 버퍼가 가득 찬 연결은 죽은 것으로 간주하고 닫는다
		client.Connected = false
		close(client.Send)
	}
}

func (h *STHub) broadcastToRoom(room *stRoom, message STMessage) {
	for _, c := range room.Clients {
		if c != nil {
			h.sendToClient(c, message)
		}
	}
}
