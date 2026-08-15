package server

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"time"

	"github.com/google/uuid"
)

// otRoom 게임(순수 상태)과 진영별 연결의 매핑
type otRoom struct {
	Game    *OTGame
	Clients map[OTSide]*OTClient
}

type OTHub struct {
	// 등록된 클라이언트
	clients map[*OTClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*otRoom

	// 상대를 기다리는 방
	waitingRoom *otRoom

	// 클라이언트 등록
	register chan *OTClient

	// 클라이언트 등록 해제
	unregister chan *OTClient

	// 게임 메시지
	gameMessage chan OTGameMessage

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*OTClient]

	// 카드 분배·선공 결정용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type OTGameMessage struct {
	Client  *OTClient
	Message OTMessage
}

func NewOTHub() *OTHub {
	return &OTHub{
		register:       make(chan *OTClient),
		unregister:     make(chan *OTClient),
		clients:        make(map[*OTClient]bool),
		rooms:          make(map[string]*otRoom),
		gameMessage:    make(chan OTGameMessage),
		sessionManager: newSessionManager[*OTClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *OTHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[OT] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[OT] Client unregistered: %s", client.ID)
			}

		case sessionID := <-h.graceExpired:
			h.handleGraceExpired(sessionID)

		case message := <-h.gameMessage:
			h.handleGameMessage(message)
		}
	}
}

func (h *OTHub) handleGameMessage(gm OTGameMessage) {
	switch gm.Message.Type {
	case OTMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case OTMsgRejoinGame:
		h.handleRejoin(gm.Client, gm.Message)
	case OTMsgMove:
		h.handleMove(gm.Client, gm.Message)
	case OTMsgPass:
		h.handlePass(gm.Client, gm.Message)
	}
}

// ==================== 입장 / 매치메이킹 ====================

func (h *OTHub) handleJoinGame(client *OTClient, msg OTMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하는 것을 막는다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload OTJoinGamePayload
	json.Unmarshal(payloadBytes, &payload)

	client.Name = payload.PlayerName
	client.SessionID = uuid.New().String()
	h.sessions[client.SessionID] = client

	// 혼자 연습: 대기 슬롯을 거치지 않고 연습봇과 즉시 매칭
	if payload.VsBot {
		game := NewOTGame(uuid.New().String())
		botRoom := &otRoom{Game: game, Clients: map[OTSide]*OTClient{}}
		h.rooms[game.ID] = botRoom

		side, err := game.AddPlayer(client.Name)
		if err != nil {
			h.sendError(client, err.Error())
			return
		}
		client.GameID = game.ID
		client.Side = side
		botRoom.Clients[side] = client

		log.Printf("[오니타마][입장] game=%s | %s=%s 봇전 시작",
			game.ID, otSideLabel(side), displayName(client.Name))

		h.sendToClient(client, OTMessage{
			Type: OTMsgPlayerJoined,
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

	var room *otRoom
	if h.waitingRoom == nil {
		game := NewOTGame(uuid.New().String())
		room = &otRoom{Game: game, Clients: map[OTSide]*OTClient{}}
		h.waitingRoom = room
		h.rooms[game.ID] = room
		log.Printf("[OT] Created new game %s", game.ID)
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

	log.Printf("[오니타마][입장] game=%s | %s=%s 게임 입장 (%d/2)",
		room.Game.ID, otSideLabel(side), displayName(client.Name), len(room.Game.Names))
	notify("오니타마 참가", fmt.Sprintf("%s(%s) 입장 (%d/2)",
		displayName(client.Name), otSideLabel(side), len(room.Game.Names)))

	h.sendToClient(client, OTMessage{
		Type: OTMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSide":  side,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
		},
	})

	if room.Game.IsReady() {
		h.startGame(room)
	} else {
		h.sendToClient(client, OTMessage{
			Type:    OTMsgWaitingPlayer,
			Payload: map[string]string{"message": "상대방을 기다리는 중..."},
		})
	}
}

func (h *OTHub) startGame(room *otRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	game := room.Game

	log.Printf("[오니타마][경기시작] game=%s | 남=%s | 북=%s | 선공=%s",
		game.ID, displayName(game.Names[OTSouth]), displayName(game.Names[OTNorth]),
		otSideLabel(game.CurrentSide))
	if !otRoomHasBot(room) {
		notify("오니타마 게임 시작", fmt.Sprintf("%s vs %s",
			displayName(game.Names[OTSouth]), displayName(game.Names[OTNorth])))
	}

	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *OTHub) roomOf(client *OTClient) *otRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

func (h *OTHub) handleMove(client *OTClient, msg OTMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload OTMovePayload
	json.Unmarshal(payloadBytes, &payload)

	result, err := room.Game.Move(client.Side, payload.Card, payload.From, payload.To)
	if err != nil {
		h.sendError(client, err.Error())
		return
	}

	from, to := payload.From, payload.To
	h.broadcastEvent(room, OTEventPayload{
		Kind: "move", Side: client.Side, Card: payload.Card, From: &from, To: &to,
	})
	if result.Captured {
		master := result.CapturedMaster
		h.broadcastEvent(room, OTEventPayload{
			Kind: "capture", Side: client.Side, Card: payload.Card, To: &to, Master: &master,
		})
	}

	h.broadcastState(room)
	h.finishIfOver(room)
}

func (h *OTHub) handlePass(client *OTClient, msg OTMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload OTPassPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.Pass(client.Side, payload.Card); err != nil {
		h.sendError(client, err.Error())
		return
	}

	h.broadcastEvent(room, OTEventPayload{Kind: "pass", Side: client.Side, Card: payload.Card})
	h.broadcastState(room)
}

// ==================== 상태 뷰 ====================

// buildOTState 게임 스냅샷. 완전 공개 정보라 YourSide 외에는 양측 동일하다.
// 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
func (h *OTHub) buildOTState(room *otRoom, viewer OTSide) OTGameStatePayload {
	game := room.Game

	opponentConnected := false
	if opponent := room.Clients[otOther(viewer)]; opponent != nil && opponent.Connected {
		opponentConnected = true
	}

	pieces := []OTPiece{}
	for _, p := range game.Pieces {
		if p.Captured {
			continue
		}
		pieces = append(pieces, *p)
	}

	legalMoves := []OTLegalMove{}
	if game.Phase == OTPhasePlay {
		legalMoves = game.LegalMoves(game.CurrentSide)
	}

	return OTGameStatePayload{
		GameID:            game.ID,
		YourSide:          viewer,
		Phase:             game.Phase,
		CurrentSide:       game.CurrentSide,
		SouthName:         game.Names[OTSouth],
		NorthName:         game.Names[OTNorth],
		Pieces:            pieces,
		SouthHand:         game.Hands[OTSouth],
		NorthHand:         game.Hands[OTNorth],
		WaitingCard:       game.WaitingCard,
		LegalMoves:        legalMoves,
		OpponentConnected: opponentConnected,
	}
}

// broadcastState 진영마다 스냅샷을 만들어 보낸다
func (h *OTHub) broadcastState(room *otRoom) {
	for side, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, OTMessage{
			Type:    OTMsgGameState,
			Payload: h.buildOTState(room, side),
		})
	}
}

func (h *OTHub) broadcastEvent(room *otRoom, event OTEventPayload) {
	h.broadcastToRoom(room, OTMessage{Type: OTMsgEvent, Payload: event})
}

// finishIfOver 게임이 끝났으면 결과를 알리고 방을 정리한다
func (h *OTHub) finishIfOver(room *otRoom) {
	game := room.Game
	if game.Phase != OTPhaseGameOver {
		return
	}

	h.broadcastToRoom(room, OTMessage{
		Type: OTMsgGameOver,
		Payload: OTGameOverPayload{
			Winner:     game.Winner,
			WinnerName: game.Names[game.Winner],
			Reason:     game.EndReason,
		},
	})

	log.Printf("[오니타마][경기결과] game=%s | 승자=%s(%s) | 사유=%s | 소요=%s",
		game.ID, displayName(game.Names[game.Winner]), otSideLabel(game.Winner),
		otEndReasonLabel(game.EndReason), matchDuration(game.StartedAt))

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
}

// ==================== 재접속 / 연결 끊김 ====================

func (h *OTHub) handleDisconnect(client *OTClient) {
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
	log.Printf("[오니타마][연결끊김] game=%s | %s=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, otSideLabel(client.Side), displayName(client.Name), h.grace.Seconds())

	if opponent := room.Clients[otOther(client.Side)]; opponent != nil {
		h.sendToClient(opponent, OTMessage{
			Type: OTMsgOpponentDisconnected,
			Payload: map[string]interface{}{
				"message":      "상대방 연결이 끊겼습니다. 재접속을 기다리는 중...",
				"graceSeconds": int(h.grace.Seconds()),
			},
		})
	}

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 시간 안에 재접속하지 않은 세션 정리
func (h *OTHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil {
		return
	}

	log.Printf("[오니타마][재접속실패] game=%s | %s=%s 유예 시간 만료로 게임 종료",
		room.Game.ID, otSideLabel(client.Side), displayName(client.Name))

	if opponent := room.Clients[otOther(client.Side)]; opponent != nil {
		h.sendToClient(opponent, OTMessage{
			Type:    OTMsgError,
			Payload: map[string]string{"message": "상대방이 재접속하지 않아 게임이 종료되었습니다"},
		})
		// 상대는 로비로 돌아가도록 세션도 만료 처리
		h.sendToClient(opponent, OTMessage{Type: OTMsgSessionExpired})
		h.drop(opponent.SessionID)
		opponent.GameID = ""
	}

	delete(h.rooms, room.Game.ID)
	if h.waitingRoom != nil && h.waitingRoom.Game.ID == room.Game.ID {
		h.waitingRoom = nil
	}
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *OTHub) handleRejoin(client *OTClient, msg OTMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload OTRejoinGamePayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil {
		h.sendToClient(client, OTMessage{Type: OTMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil || !room.Game.Ready {
		h.drop(payload.SessionID)
		h.sendToClient(client, OTMessage{Type: OTMsgSessionExpired})
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

	log.Printf("[오니타마][재접속] game=%s | %s=%s 재접속 완료",
		room.Game.ID, otSideLabel(client.Side), displayName(client.Name))

	if opponent := room.Clients[otOther(client.Side)]; opponent != nil {
		h.sendToClient(opponent, OTMessage{Type: OTMsgOpponentReconnected})
	}

	// 현재 게임 상태 전체를 내려서 클라이언트 상태를 복원시킨다
	h.sendToClient(client, OTMessage{
		Type:    OTMsgGameState,
		Payload: h.buildOTState(room, client.Side),
	})
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *OTHub) clearGameSessions(room *otRoom) {
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		h.drop(c.SessionID)
	}
}

// ==================== 라벨 / 전송 ====================

// otSideLabel 진영 한글 표기
func otSideLabel(side OTSide) string {
	switch side {
	case OTSouth:
		return "남"
	case OTNorth:
		return "북"
	}
	return string(side)
}

// otEndReasonLabel 종료 사유 한글 표기
func otEndReasonLabel(reason string) string {
	switch reason {
	case "capture_master":
		return "마스터 포획"
	case "reach_temple":
		return "사원 도달"
	}
	return reason
}

func (h *OTHub) sendError(client *OTClient, message string) {
	h.sendToClient(client, OTMessage{Type: OTMsgError, Payload: map[string]string{"message": message}})
}

func (h *OTHub) sendToClient(client *OTClient, message OTMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[OT] ")
}

func (h *OTHub) broadcastToRoom(room *otRoom, message OTMessage) {
	for _, c := range room.Clients {
		if c != nil {
			h.sendToClient(c, message)
		}
	}
}
