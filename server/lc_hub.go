package server

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"time"

	"github.com/google/uuid"
)

// lcRoom 게임(순수 상태)과 진영별 연결의 매핑
type lcRoom struct {
	Game    *LCGame
	Clients map[LCSide]*LCClient

	// ---- 재대결 창 (게임 종료 후) ----
	Rematch      map[LCSide]bool
	CleanupTimer *time.Timer
}

type LCHub struct {
	// 등록된 클라이언트
	clients map[*LCClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*lcRoom

	// 상대를 기다리는 방
	waitingRoom *lcRoom

	// 클라이언트 등록
	register chan *LCClient

	// 클라이언트 등록 해제
	unregister chan *LCClient

	// 게임 메시지
	gameMessage chan LCGameMessage

	// 재대결 창 만료 알림 (gameID)
	roomExpired chan string

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*LCClient]

	// 덱 셔플·선공 결정용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type LCGameMessage struct {
	Client  *LCClient
	Message LCMessage
}

func NewLCHub() *LCHub {
	return &LCHub{
		register:       make(chan *LCClient),
		unregister:     make(chan *LCClient),
		clients:        make(map[*LCClient]bool),
		rooms:          make(map[string]*lcRoom),
		gameMessage:    make(chan LCGameMessage),
		roomExpired:    make(chan string, 8),
		sessionManager: newSessionManager[*LCClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *LCHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[LC] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[LC] Client unregistered: %s", client.ID)
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

func (h *LCHub) handleGameMessage(gm LCGameMessage) {
	switch gm.Message.Type {
	case LCMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case LCMsgRejoinGame:
		h.handleRejoin(gm.Client, gm.Message)
	case LCMsgMove:
		h.handleMove(gm.Client, gm.Message)
	case LCMsgRematch:
		h.handleRematch(gm.Client)
	}
}

// ==================== 입장 / 매치메이킹 ====================

func (h *LCHub) handleJoinGame(client *LCClient, msg LCMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하는 것을 막는다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload LCJoinGamePayload
	json.Unmarshal(payloadBytes, &payload)

	client.Name = payload.PlayerName
	client.SessionID = uuid.New().String()
	h.sessions[client.SessionID] = client

	// 혼자 연습: 대기 슬롯을 거치지 않고 연습봇과 즉시 매칭
	if payload.VsBot {
		game := NewLCGame(uuid.New().String())
		botRoom := &lcRoom{Game: game, Clients: map[LCSide]*LCClient{}}
		h.rooms[game.ID] = botRoom

		side, err := game.AddPlayer(client.Name)
		if err != nil {
			h.sendError(client, err.Error())
			return
		}
		client.GameID = game.ID
		client.Side = side
		botRoom.Clients[side] = client

		log.Printf("[로스트시티][입장] game=%s | %s=%s 봇전 시작",
			game.ID, lcSideLabel(side), displayName(client.Name))

		h.sendToClient(client, LCMessage{
			Type: LCMsgPlayerJoined,
			Payload: map[string]interface{}{
				"yourSide":  side,
				"gameId":    game.ID,
				"sessionId": client.SessionID,
			},
		})

		h.spawnBot(botRoom)
		h.startGame(botRoom)
		return
	}

	var room *lcRoom
	if h.waitingRoom == nil {
		game := NewLCGame(uuid.New().String())
		room = &lcRoom{Game: game, Clients: map[LCSide]*LCClient{}}
		h.waitingRoom = room
		h.rooms[game.ID] = room
		log.Printf("[LC] Created new game %s", game.ID)
	} else {
		room = h.waitingRoom
		h.waitingRoom = nil
	}

	side, err := room.Game.AddPlayer(client.Name)
	if err != nil {
		h.sendError(client, err.Error())
		return
	}
	client.GameID = room.Game.ID
	client.Side = side
	room.Clients[side] = client

	log.Printf("[로스트시티][입장] game=%s | %s=%s 게임 입장 (%d/2)",
		room.Game.ID, lcSideLabel(side), displayName(client.Name), len(room.Game.Names))
	notify("로스트 시티 참가", fmt.Sprintf("%s(%s) 입장 (%d/2)",
		displayName(client.Name), lcSideLabel(side), len(room.Game.Names)))

	h.sendToClient(client, LCMessage{
		Type: LCMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSide":  side,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
		},
	})

	if room.Game.IsReady() {
		h.startGame(room)
	} else {
		h.sendToClient(client, LCMessage{
			Type:    LCMsgWaitingPlayer,
			Payload: map[string]string{"message": "상대방을 기다리는 중..."},
		})
	}
}

func (h *LCHub) startGame(room *lcRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	game := room.Game

	log.Printf("[로스트시티][경기시작] game=%s | 남=%s | 북=%s | 선공=%s",
		game.ID, displayName(game.Names[LCSouth]), displayName(game.Names[LCNorth]),
		lcSideLabel(game.CurrentSide))
	if !lcRoomHasBot(room) {
		notify("로스트 시티 게임 시작", fmt.Sprintf("%s vs %s",
			displayName(game.Names[LCSouth]), displayName(game.Names[LCNorth])))
	}

	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *LCHub) roomOf(client *LCClient) *lcRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

func (h *LCHub) handleMove(client *LCClient, msg LCMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload LCMovePayload
	json.Unmarshal(payloadBytes, &payload)

	result, err := room.Game.Move(client.Side, payload)
	if err != nil {
		h.sendError(client, err.Error())
		return
	}

	// 놓기/버리기 카드는 공개 정보
	card := result.Card
	h.broadcastEvent(room, LCEventPayload{Kind: payload.Action, Side: client.Side, Card: &card})
	// 뽑기: 덱은 출처만, 버림 더미는 카드까지 공개
	drawEvent := LCEventPayload{Kind: "draw", Side: client.Side, Source: payload.Draw}
	if result.DrawnFromPile != nil {
		drawEvent.Card = result.DrawnFromPile
	}
	h.broadcastEvent(room, drawEvent)

	h.broadcastState(room)
	h.finishIfOver(room)
}

// ==================== 상태 뷰 (손패 은닉의 핵심) ====================

// buildLCState 개인화 스냅샷 — 내 손패는 전체, 상대는 장수만.
// 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
func (h *LCHub) buildLCState(room *lcRoom, viewer LCSide) LCGameStatePayload {
	game := room.Game

	opponentConnected := false
	if opponent := room.Clients[lcOther(viewer)]; opponent != nil && opponent.Connected {
		opponentConnected = true
	}

	return LCGameStatePayload{
		GameID:            game.ID,
		YourSide:          viewer,
		Phase:             game.Phase,
		CurrentSide:       game.CurrentSide,
		SouthName:         game.Names[LCSouth],
		NorthName:         game.Names[LCNorth],
		YourHand:          game.Hands[viewer],
		OpponentHandCount: len(game.Hands[lcOther(viewer)]),
		DeckCount:         len(game.Deck),
		SouthExpeditions:  game.Expeditions[LCSouth],
		NorthExpeditions:  game.Expeditions[LCNorth],
		Discards:          game.Discards,
		SouthScore:        game.Score(LCSouth),
		NorthScore:        game.Score(LCNorth),
		OpponentConnected: opponentConnected,
	}
}

// broadcastState 진영마다 개인화 스냅샷을 만들어 보낸다
func (h *LCHub) broadcastState(room *lcRoom) {
	for side, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, LCMessage{
			Type:    LCMsgGameState,
			Payload: h.buildLCState(room, side),
		})
	}
}

func (h *LCHub) broadcastEvent(room *lcRoom, event LCEventPayload) {
	h.broadcastToRoom(room, LCMessage{Type: LCMsgEvent, Payload: event})
}

// finishIfOver 게임이 끝났으면 점수를 공개하고 방을 정리한다
func (h *LCHub) finishIfOver(room *lcRoom) {
	game := room.Game
	if game.Phase != LCPhaseGameOver {
		return
	}

	winnerName := game.Names[game.Winner]
	h.broadcastToRoom(room, LCMessage{
		Type: LCMsgGameOver,
		Payload: LCGameOverPayload{
			Winner:     game.Winner,
			WinnerName: winnerName,
			Reason:     game.EndReason,
			SouthScore: game.Score(LCSouth),
			NorthScore: game.Score(LCNorth),
		},
	})

	resultLabel := "무승부"
	if game.Winner != "" {
		resultLabel = fmt.Sprintf("승자=%s(%s)", displayName(winnerName), lcSideLabel(game.Winner))
	}
	log.Printf("[로스트시티][경기결과] game=%s | %s | 남 %d : 북 %d | 소요=%s",
		game.ID, resultLabel, game.Score(LCSouth), game.Score(LCNorth), matchDuration(game.StartedAt))

	// 재대결 창: 방·세션을 잠시 유지하고 재대결 신청을 기다린다
	room.Rematch = map[LCSide]bool{}
	gameID := game.ID
	room.CleanupTimer = time.AfterFunc(rematchWindow, func() { h.roomExpired <- gameID })
}

// handleRematch 게임 종료 후 재대결 신청. 봇전은 즉시, 사람전은 양쪽 신청 시 재시작.
func (h *LCHub) handleRematch(client *LCClient) {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase != LCPhaseGameOver || room.CleanupTimer == nil {
		return
	}
	if room.Rematch == nil {
		room.Rematch = map[LCSide]bool{}
	}
	room.Rematch[client.Side] = true

	opponent := room.Clients[lcOther(client.Side)]
	if (opponent != nil && opponent.Bot) || room.Rematch[lcOther(client.Side)] {
		h.restartRematch(room)
		return
	}
	if opponent != nil {
		h.sendToClient(opponent, LCMessage{Type: LCMsgRematchOffer})
	}
}

// restartRematch 같은 방에서 새 게임을 시작한다 (연결·세션 유지, 봇은 재소환)
func (h *LCHub) restartRematch(room *lcRoom) {
	if room.CleanupTimer != nil {
		room.CleanupTimer.Stop()
		room.CleanupTimer = nil
	}
	room.Rematch = nil

	humans := []*LCClient{}
	hadBot := false
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		if c.Bot {
			hadBot = true
			h.drop(c.SessionID)
		} else {
			humans = append(humans, c)
		}
	}

	game := NewLCGame(room.Game.ID)
	room.Game = game
	room.Clients = map[LCSide]*LCClient{}
	for _, c := range humans {
		side, err := game.AddPlayer(c.Name)
		if err != nil {
			continue
		}
		c.Side = side
		room.Clients[side] = c
		// 프론트가 세션 키를 다시 저장하도록 입장 확인을 재전송한다
		h.sendToClient(c, LCMessage{
			Type: LCMsgPlayerJoined,
			Payload: map[string]interface{}{
				"yourSide":  side,
				"gameId":    game.ID,
				"sessionId": c.SessionID,
			},
		})
	}
	if hadBot {
		h.spawnBot(room)
	}

	log.Printf("[로스트시티][재대결] game=%s | 같은 방에서 재시작", game.ID)
	h.startGame(room)
}

// handleRoomExpired 재대결 창이 지나도록 신청이 없으면 방·세션 정리
func (h *LCHub) handleRoomExpired(gameID string) {
	room := h.rooms[gameID]
	if room == nil || room.Game.Phase != LCPhaseGameOver {
		return
	}
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		if !c.Bot {
			h.sendToClient(c, LCMessage{Type: LCMsgSessionExpired})
		}
		h.drop(c.SessionID)
	}
	delete(h.rooms, gameID)
}

// ==================== 재접속 / 연결 끊김 ====================

func (h *LCHub) handleDisconnect(client *LCClient) {
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
		}
		h.drop(client.SessionID)
		return
	}

	// 진행 중인 게임: 유예 시간 동안 세션을 유지하고 재접속을 기다린다
	log.Printf("[로스트시티][연결끊김] game=%s | %s=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, lcSideLabel(client.Side), displayName(client.Name), h.grace.Seconds())

	if opponent := room.Clients[lcOther(client.Side)]; opponent != nil {
		h.sendToClient(opponent, LCMessage{
			Type: LCMsgOpponentDisconnected,
			Payload: map[string]interface{}{
				"message":      "상대방 연결이 끊겼습니다. 재접속을 기다리는 중...",
				"graceSeconds": int(h.grace.Seconds()),
			},
		})
	}

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 시간 안에 재접속하지 않은 세션 정리
func (h *LCHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil {
		return
	}

	log.Printf("[로스트시티][재접속실패] game=%s | %s=%s 유예 시간 만료로 게임 종료",
		room.Game.ID, lcSideLabel(client.Side), displayName(client.Name))

	if opponent := room.Clients[lcOther(client.Side)]; opponent != nil {
		h.sendToClient(opponent, LCMessage{
			Type:    LCMsgError,
			Payload: map[string]string{"message": "상대방이 재접속하지 않아 게임이 종료되었습니다"},
		})
		// 상대는 로비로 돌아가도록 세션도 만료 처리
		h.sendToClient(opponent, LCMessage{Type: LCMsgSessionExpired})
		h.drop(opponent.SessionID)
		opponent.GameID = ""
	}

	delete(h.rooms, room.Game.ID)
	if h.waitingRoom != nil && h.waitingRoom.Game.ID == room.Game.ID {
		h.waitingRoom = nil
	}
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *LCHub) handleRejoin(client *LCClient, msg LCMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload LCRejoinGamePayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil {
		h.sendToClient(client, LCMessage{Type: LCMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil || !room.Game.Ready {
		h.drop(payload.SessionID)
		h.sendToClient(client, LCMessage{Type: LCMsgSessionExpired})
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
	client.Side = old.Side
	h.sessions[client.SessionID] = client
	room.Clients[client.Side] = client

	log.Printf("[로스트시티][재접속] game=%s | %s=%s 재접속 완료",
		room.Game.ID, lcSideLabel(client.Side), displayName(client.Name))

	if opponent := room.Clients[lcOther(client.Side)]; opponent != nil {
		h.sendToClient(opponent, LCMessage{Type: LCMsgOpponentReconnected})
	}

	// 현재 게임 상태 전체를 내려서 클라이언트 상태를 복원시킨다
	h.sendToClient(client, LCMessage{
		Type:    LCMsgGameState,
		Payload: h.buildLCState(room, client.Side),
	})
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *LCHub) clearGameSessions(room *lcRoom) {
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		h.drop(c.SessionID)
	}
}

// ==================== 라벨 / 전송 ====================

// lcSideLabel 진영 한글 표기
func lcSideLabel(side LCSide) string {
	switch side {
	case LCSouth:
		return "남"
	case LCNorth:
		return "북"
	}
	return string(side)
}

func (h *LCHub) sendError(client *LCClient, message string) {
	h.sendToClient(client, LCMessage{Type: LCMsgError, Payload: map[string]string{"message": message}})
}

func (h *LCHub) sendToClient(client *LCClient, message LCMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[LC] ")
}

func (h *LCHub) broadcastToRoom(room *lcRoom, message LCMessage) {
	for _, c := range room.Clients {
		if c != nil {
			h.sendToClient(c, message)
		}
	}
}
