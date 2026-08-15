package server

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"time"

	"github.com/google/uuid"
)

// qdRoom 게임(순수 상태)과 진영별 연결의 매핑
type qdRoom struct {
	Game    *QDGame
	Clients map[QDSide]*QDClient

	// ---- 재대결 창 (게임 종료 후) ----
	Rematch      map[QDSide]bool
	CleanupTimer *time.Timer
}

type QDHub struct {
	// 등록된 클라이언트
	clients map[*QDClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*qdRoom

	// 상대를 기다리는 방
	waitingRoom *qdRoom

	// 클라이언트 등록
	register chan *QDClient

	// 클라이언트 등록 해제
	unregister chan *QDClient

	// 게임 메시지
	gameMessage chan QDGameMessage

	// 재대결 창 만료 알림 (gameID)
	roomExpired chan string

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*QDClient]

	// 선공 결정용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type QDGameMessage struct {
	Client  *QDClient
	Message QDMessage
}

func NewQDHub() *QDHub {
	return &QDHub{
		register:       make(chan *QDClient),
		unregister:     make(chan *QDClient),
		clients:        make(map[*QDClient]bool),
		rooms:          make(map[string]*qdRoom),
		gameMessage:    make(chan QDGameMessage),
		roomExpired:    make(chan string, 8),
		sessionManager: newSessionManager[*QDClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *QDHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[QD] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[QD] Client unregistered: %s", client.ID)
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

func (h *QDHub) handleGameMessage(gm QDGameMessage) {
	switch gm.Message.Type {
	case QDMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case QDMsgRejoinGame:
		h.handleRejoin(gm.Client, gm.Message)
	case QDMsgMove:
		h.handleMove(gm.Client, gm.Message)
	case QDMsgPlaceWall:
		h.handlePlaceWall(gm.Client, gm.Message)
	case QDMsgRematch:
		h.handleRematch(gm.Client)
	}
}

// ==================== 입장 / 매치메이킹 ====================

func (h *QDHub) handleJoinGame(client *QDClient, msg QDMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하는 것을 막는다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload QDJoinGamePayload
	json.Unmarshal(payloadBytes, &payload)

	client.Name = payload.PlayerName
	client.SessionID = uuid.New().String()
	h.sessions[client.SessionID] = client

	// 혼자 연습: 대기 슬롯을 거치지 않고 연습봇과 즉시 매칭
	if payload.VsBot {
		game := NewQDGame(uuid.New().String())
		botRoom := &qdRoom{Game: game, Clients: map[QDSide]*QDClient{}}
		h.rooms[game.ID] = botRoom

		side, err := game.AddPlayer(client.Name)
		if err != nil {
			h.sendError(client, err.Error())
			return
		}
		client.GameID = game.ID
		client.Side = side
		botRoom.Clients[side] = client

		log.Printf("[쿼리도][입장] game=%s | %s=%s 봇전 시작",
			game.ID, qdSideLabel(side), displayName(client.Name))

		h.sendToClient(client, QDMessage{
			Type: QDMsgPlayerJoined,
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

	var room *qdRoom
	if h.waitingRoom == nil {
		game := NewQDGame(uuid.New().String())
		room = &qdRoom{Game: game, Clients: map[QDSide]*QDClient{}}
		h.waitingRoom = room
		h.rooms[game.ID] = room
		log.Printf("[QD] Created new game %s", game.ID)
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

	log.Printf("[쿼리도][입장] game=%s | %s=%s 게임 입장 (%d/2)",
		room.Game.ID, qdSideLabel(side), displayName(client.Name), len(room.Game.Names))
	notify("쿼리도 참가", fmt.Sprintf("%s(%s) 입장 (%d/2)",
		displayName(client.Name), qdSideLabel(side), len(room.Game.Names)))

	h.sendToClient(client, QDMessage{
		Type: QDMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSide":  side,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
		},
	})

	if room.Game.IsReady() {
		h.startGame(room)
	} else {
		h.sendToClient(client, QDMessage{
			Type:    QDMsgWaitingPlayer,
			Payload: map[string]string{"message": "상대방을 기다리는 중..."},
		})
	}
}

func (h *QDHub) startGame(room *qdRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	game := room.Game

	log.Printf("[쿼리도][경기시작] game=%s | 남=%s | 북=%s | 선공=%s",
		game.ID, displayName(game.Names[QDSouth]), displayName(game.Names[QDNorth]),
		qdSideLabel(game.CurrentSide))
	if !qdRoomHasBot(room) {
		notify("쿼리도 게임 시작", fmt.Sprintf("%s vs %s",
			displayName(game.Names[QDSouth]), displayName(game.Names[QDNorth])))
	}

	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *QDHub) roomOf(client *QDClient) *qdRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

func (h *QDHub) handleMove(client *QDClient, msg QDMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload QDMovePayload
	json.Unmarshal(payloadBytes, &payload)

	result, err := room.Game.MovePawn(client.Side, payload.To)
	if err != nil {
		h.sendError(client, err.Error())
		return
	}

	from, to := result.From, payload.To
	h.broadcastEvent(room, QDEventPayload{Kind: "move", Side: client.Side, From: &from, To: &to})
	h.broadcastState(room)
	h.finishIfOver(room)
}

func (h *QDHub) handlePlaceWall(client *QDClient, msg QDMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload QDPlaceWallPayload
	json.Unmarshal(payloadBytes, &payload)

	wall := QDWall{Row: payload.Row, Col: payload.Col, Orientation: payload.Orientation}
	if err := room.Game.PlaceWall(client.Side, wall); err != nil {
		h.sendError(client, err.Error())
		return
	}

	h.broadcastEvent(room, QDEventPayload{Kind: "wall", Side: client.Side, Wall: &wall})
	h.broadcastState(room)
}

// ==================== 상태 뷰 ====================

// buildQDState 게임 스냅샷. 완전 공개 정보라 YourSide 외에는 양측 동일하다.
// 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
func (h *QDHub) buildQDState(room *qdRoom, viewer QDSide) QDGameStatePayload {
	game := room.Game

	opponentConnected := false
	if opponent := room.Clients[qdOther(viewer)]; opponent != nil && opponent.Connected {
		opponentConnected = true
	}

	legalMoves := []QDCell{}
	if game.Phase == QDPhasePlay {
		legalMoves = game.LegalPawnMoves(game.CurrentSide)
	}

	return QDGameStatePayload{
		GameID:            game.ID,
		YourSide:          viewer,
		Phase:             game.Phase,
		CurrentSide:       game.CurrentSide,
		SouthName:         game.Names[QDSouth],
		NorthName:         game.Names[QDNorth],
		SouthPawn:         game.Pawns[QDSouth],
		NorthPawn:         game.Pawns[QDNorth],
		SouthWalls:        game.WallsLeft[QDSouth],
		NorthWalls:        game.WallsLeft[QDNorth],
		Walls:             game.Walls,
		LegalMoves:        legalMoves,
		OpponentConnected: opponentConnected,
	}
}

// broadcastState 진영마다 스냅샷을 만들어 보낸다
func (h *QDHub) broadcastState(room *qdRoom) {
	for side, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, QDMessage{
			Type:    QDMsgGameState,
			Payload: h.buildQDState(room, side),
		})
	}
}

func (h *QDHub) broadcastEvent(room *qdRoom, event QDEventPayload) {
	h.broadcastToRoom(room, QDMessage{Type: QDMsgEvent, Payload: event})
}

// finishIfOver 게임이 끝났으면 결과를 알리고 방을 정리한다
func (h *QDHub) finishIfOver(room *qdRoom) {
	game := room.Game
	if game.Phase != QDPhaseGameOver {
		return
	}

	h.broadcastToRoom(room, QDMessage{
		Type: QDMsgGameOver,
		Payload: QDGameOverPayload{
			Winner:     game.Winner,
			WinnerName: game.Names[game.Winner],
			Reason:     game.EndReason,
		},
	})

	log.Printf("[쿼리도][경기결과] game=%s | 승자=%s(%s) | 사유=%s | 소요=%s",
		game.ID, displayName(game.Names[game.Winner]), qdSideLabel(game.Winner),
		qdEndReasonLabel(game.EndReason), matchDuration(game.StartedAt))

	// 재대결 창: 방·세션을 잠시 유지하고 재대결 신청을 기다린다
	room.Rematch = map[QDSide]bool{}
	gameID := game.ID
	room.CleanupTimer = time.AfterFunc(rematchWindow, func() { h.roomExpired <- gameID })
}

// handleRematch 게임 종료 후 재대결 신청. 봇전은 즉시, 사람전은 양쪽 신청 시 재시작.
func (h *QDHub) handleRematch(client *QDClient) {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase != QDPhaseGameOver || room.CleanupTimer == nil {
		return
	}
	if room.Rematch == nil {
		room.Rematch = map[QDSide]bool{}
	}
	room.Rematch[client.Side] = true

	opponent := room.Clients[qdOther(client.Side)]
	if (opponent != nil && opponent.Bot) || room.Rematch[qdOther(client.Side)] {
		h.restartRematch(room)
		return
	}
	if opponent != nil {
		h.sendToClient(opponent, QDMessage{Type: QDMsgRematchOffer})
	}
}

// restartRematch 같은 방에서 새 게임을 시작한다 (연결·세션 유지, 봇은 재소환)
func (h *QDHub) restartRematch(room *qdRoom) {
	if room.CleanupTimer != nil {
		room.CleanupTimer.Stop()
		room.CleanupTimer = nil
	}
	room.Rematch = nil

	humans := []*QDClient{}
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

	game := NewQDGame(room.Game.ID)
	room.Game = game
	room.Clients = map[QDSide]*QDClient{}
	for _, c := range humans {
		side, err := game.AddPlayer(c.Name)
		if err != nil {
			continue
		}
		c.Side = side
		room.Clients[side] = c
		// 프론트가 세션 키를 다시 저장하도록 입장 확인을 재전송한다
		h.sendToClient(c, QDMessage{
			Type: QDMsgPlayerJoined,
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

	log.Printf("[쿼리도][재대결] game=%s | 같은 방에서 재시작", game.ID)
	h.startGame(room)
}

// handleRoomExpired 재대결 창이 지나도록 신청이 없으면 방·세션 정리
func (h *QDHub) handleRoomExpired(gameID string) {
	room := h.rooms[gameID]
	if room == nil || room.Game.Phase != QDPhaseGameOver {
		return
	}
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		if !c.Bot {
			h.sendToClient(c, QDMessage{Type: QDMsgSessionExpired})
		}
		h.drop(c.SessionID)
	}
	delete(h.rooms, gameID)
}

// ==================== 재접속 / 연결 끊김 ====================

func (h *QDHub) handleDisconnect(client *QDClient) {
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
	log.Printf("[쿼리도][연결끊김] game=%s | %s=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, qdSideLabel(client.Side), displayName(client.Name), h.grace.Seconds())

	if opponent := room.Clients[qdOther(client.Side)]; opponent != nil {
		h.sendToClient(opponent, QDMessage{
			Type: QDMsgOpponentDisconnected,
			Payload: map[string]interface{}{
				"message":      "상대방 연결이 끊겼습니다. 재접속을 기다리는 중...",
				"graceSeconds": int(h.grace.Seconds()),
			},
		})
	}

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 시간 안에 재접속하지 않은 세션 정리
func (h *QDHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil {
		return
	}

	log.Printf("[쿼리도][재접속실패] game=%s | %s=%s 유예 시간 만료로 게임 종료",
		room.Game.ID, qdSideLabel(client.Side), displayName(client.Name))

	if opponent := room.Clients[qdOther(client.Side)]; opponent != nil {
		h.sendToClient(opponent, QDMessage{
			Type:    QDMsgError,
			Payload: map[string]string{"message": "상대방이 재접속하지 않아 게임이 종료되었습니다"},
		})
		// 상대는 로비로 돌아가도록 세션도 만료 처리
		h.sendToClient(opponent, QDMessage{Type: QDMsgSessionExpired})
		h.drop(opponent.SessionID)
		opponent.GameID = ""
	}

	delete(h.rooms, room.Game.ID)
	if h.waitingRoom != nil && h.waitingRoom.Game.ID == room.Game.ID {
		h.waitingRoom = nil
	}
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *QDHub) handleRejoin(client *QDClient, msg QDMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload QDRejoinGamePayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil {
		h.sendToClient(client, QDMessage{Type: QDMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil || !room.Game.Ready {
		h.drop(payload.SessionID)
		h.sendToClient(client, QDMessage{Type: QDMsgSessionExpired})
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

	log.Printf("[쿼리도][재접속] game=%s | %s=%s 재접속 완료",
		room.Game.ID, qdSideLabel(client.Side), displayName(client.Name))

	if opponent := room.Clients[qdOther(client.Side)]; opponent != nil {
		h.sendToClient(opponent, QDMessage{Type: QDMsgOpponentReconnected})
	}

	// 현재 게임 상태 전체를 내려서 클라이언트 상태를 복원시킨다
	h.sendToClient(client, QDMessage{
		Type:    QDMsgGameState,
		Payload: h.buildQDState(room, client.Side),
	})
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *QDHub) clearGameSessions(room *qdRoom) {
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		h.drop(c.SessionID)
	}
}

// ==================== 라벨 / 전송 ====================

// qdSideLabel 진영 한글 표기
func qdSideLabel(side QDSide) string {
	switch side {
	case QDSouth:
		return "남"
	case QDNorth:
		return "북"
	}
	return string(side)
}

// qdEndReasonLabel 종료 사유 한글 표기
func qdEndReasonLabel(reason string) string {
	if reason == "reach_goal" {
		return "골 라인 도달"
	}
	return reason
}

func (h *QDHub) sendError(client *QDClient, message string) {
	h.sendToClient(client, QDMessage{Type: QDMsgError, Payload: map[string]string{"message": message}})
}

func (h *QDHub) sendToClient(client *QDClient, message QDMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[QD] ")
}

func (h *QDHub) broadcastToRoom(room *qdRoom, message QDMessage) {
	for _, c := range room.Clients {
		if c != nil {
			h.sendToClient(c, message)
		}
	}
}
