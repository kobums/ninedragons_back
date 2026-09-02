package server

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"time"

	"github.com/google/uuid"
)

// csRoom 게임(순수 상태)과 진영별 연결의 매핑
type csRoom struct {
	Game    *CSGame
	Clients map[CSSide]*CSClient

	// ---- 재대결 창 (게임 종료 후) ----
	Rematch      map[CSSide]bool
	CleanupTimer *time.Timer
}

type CSHub struct {
	// 등록된 클라이언트
	clients map[*CSClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*csRoom

	// 상대를 기다리는 방
	waitingRoom *csRoom

	// 클라이언트 등록
	register chan *CSClient

	// 클라이언트 등록 해제
	unregister chan *CSClient

	// 게임 메시지
	gameMessage chan CSGameMessage

	// 재대결 창 만료 알림 (gameID)
	roomExpired chan string

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*CSClient]

	// 주사위·선공 결정용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type CSGameMessage struct {
	Client  *CSClient
	Message CSMessage
}

func NewCSHub() *CSHub {
	return &CSHub{
		register:       make(chan *CSClient),
		unregister:     make(chan *CSClient),
		clients:        make(map[*CSClient]bool),
		rooms:          make(map[string]*csRoom),
		gameMessage:    make(chan CSGameMessage),
		roomExpired:    make(chan string, 8),
		sessionManager: newSessionManager[*CSClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *CSHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[CS] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[CS] Client unregistered: %s", client.ID)
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

func (h *CSHub) handleGameMessage(gm CSGameMessage) {
	switch gm.Message.Type {
	case CSMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case CSMsgRejoinGame:
		h.handleRejoin(gm.Client, gm.Message)
	case CSMsgRoll:
		h.handleRoll(gm.Client)
	case CSMsgChoose:
		h.handleChoose(gm.Client, gm.Message)
	case CSMsgStop:
		h.handleStop(gm.Client)
	case CSMsgRematch:
		h.handleRematch(gm.Client)
	}
}

// ==================== 입장 / 매치메이킹 ====================

func (h *CSHub) handleJoinGame(client *CSClient, msg CSMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하는 것을 막는다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload CSJoinGamePayload
	json.Unmarshal(payloadBytes, &payload)
	// 초기 게임군은 playerName, 이후 게임군은 name — 양쪽 표기를 받는다
	payload.PlayerName = resolveJoinName(payloadBytes, payload.PlayerName)

	client.Name = payload.PlayerName
	client.SessionID = uuid.New().String()
	h.sessions[client.SessionID] = client

	// 혼자 연습: 대기 슬롯을 거치지 않고 연습봇과 즉시 매칭
	if payload.VsBot {
		game := NewCSGame(uuid.New().String())
		botRoom := &csRoom{Game: game, Clients: map[CSSide]*CSClient{}}
		h.rooms[game.ID] = botRoom

		side, err := game.AddPlayer(client.Name)
		if err != nil {
			h.sendError(client, err.Error())
			return
		}
		client.GameID = game.ID
		client.Side = side
		botRoom.Clients[side] = client

		log.Printf("[캔트스톱][입장] game=%s | %s=%s 봇전 시작",
			game.ID, csSideLabel(side), displayName(client.Name))

		h.sendToClient(client, CSMessage{
			Type: CSMsgPlayerJoined,
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

	var room *csRoom
	if h.waitingRoom == nil {
		game := NewCSGame(uuid.New().String())
		room = &csRoom{Game: game, Clients: map[CSSide]*CSClient{}}
		h.waitingRoom = room
		lobbySetWaiting("cantstop", true)
		h.rooms[game.ID] = room
		log.Printf("[CS] Created new game %s", game.ID)
	} else {
		room = h.waitingRoom
		h.waitingRoom = nil
		lobbySetWaiting("cantstop", false)
	}

	side, err := room.Game.AddPlayer(client.Name)
	if err != nil {
		h.sendError(client, err.Error())
		return
	}
	client.GameID = room.Game.ID
	client.Side = side
	room.Clients[side] = client

	log.Printf("[캔트스톱][입장] game=%s | %s=%s 게임 입장 (%d/2)",
		room.Game.ID, csSideLabel(side), displayName(client.Name), len(room.Game.Names))
	notify("캔트 스톱 참가", fmt.Sprintf("%s(%s) 입장 (%d/2)",
		displayName(client.Name), csSideLabel(side), len(room.Game.Names)))

	h.sendToClient(client, CSMessage{
		Type: CSMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSide":  side,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
		},
	})

	if room.Game.IsReady() {
		h.startGame(room)
	} else {
		h.sendToClient(client, CSMessage{
			Type:    CSMsgWaitingPlayer,
			Payload: map[string]string{"message": "상대방을 기다리는 중..."},
		})
	}
}

func (h *CSHub) startGame(room *csRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	game := room.Game

	log.Printf("[캔트스톱][경기시작] game=%s | 남=%s | 북=%s | 선공=%s",
		game.ID, displayName(game.Names[CSSouth]), displayName(game.Names[CSNorth]),
		csSideLabel(game.CurrentSide))
	if !csRoomHasBot(room) {
		notify("캔트 스톱 게임 시작", fmt.Sprintf("%s vs %s",
			displayName(game.Names[CSSouth]), displayName(game.Names[CSNorth])))
	}

	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *CSHub) roomOf(client *CSClient) *csRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

func (h *CSHub) handleRoll(client *CSClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}

	result, err := room.Game.Roll(client.Side, h.rng)
	if err != nil {
		h.sendError(client, err.Error())
		return
	}

	h.broadcastEvent(room, CSEventPayload{Kind: "roll", Side: client.Side, Dice: result.Dice})
	if result.Busted {
		h.broadcastEvent(room, CSEventPayload{Kind: "bust", Side: client.Side})
	}
	h.broadcastState(room)
}

func (h *CSHub) handleChoose(client *CSClient, msg CSMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload CSChoosePayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.Choose(client.Side, payload.Sums); err != nil {
		h.sendError(client, err.Error())
		return
	}

	h.broadcastEvent(room, CSEventPayload{Kind: "advance", Side: client.Side, Sums: payload.Sums})
	h.broadcastState(room)
}

func (h *CSHub) handleStop(client *CSClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}

	result, err := room.Game.Stop(client.Side)
	if err != nil {
		h.sendError(client, err.Error())
		return
	}

	h.broadcastEvent(room, CSEventPayload{Kind: "bank", Side: client.Side})
	for _, col := range result.ClaimedCols {
		h.broadcastEvent(room, CSEventPayload{Kind: "claim", Side: client.Side, Col: col})
	}
	h.broadcastState(room)
	h.finishIfOver(room)
}

// ==================== 상태 뷰 ====================

// buildCSState 게임 스냅샷. 완전 공개 정보라 YourSide 외에는 양측 동일하다.
// 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
func (h *CSHub) buildCSState(room *csRoom, viewer CSSide) CSGameStatePayload {
	game := room.Game

	opponentConnected := false
	if opponent := room.Clients[csOther(viewer)]; opponent != nil && opponent.Connected {
		opponentConnected = true
	}

	return CSGameStatePayload{
		GameID:            game.ID,
		YourSide:          viewer,
		Phase:             game.Phase,
		CurrentSide:       game.CurrentSide,
		SouthName:         game.Names[CSSouth],
		NorthName:         game.Names[CSNorth],
		SouthProgress:     game.Progress[CSSouth],
		NorthProgress:     game.Progress[CSNorth],
		Claimed:           game.Claimed,
		Temp:              game.Temp,
		Dice:              game.Dice,
		Options:           game.Options,
		CanRoll:           game.Phase == CSPhasePlay && game.Dice == nil,
		CanStop:           game.Phase == CSPhasePlay && game.Dice == nil && len(game.Temp) > 0,
		OpponentConnected: opponentConnected,
	}
}

// broadcastState 진영마다 스냅샷을 만들어 보낸다
func (h *CSHub) broadcastState(room *csRoom) {
	for side, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, CSMessage{
			Type:    CSMsgGameState,
			Payload: h.buildCSState(room, side),
		})
	}
}

func (h *CSHub) broadcastEvent(room *csRoom, event CSEventPayload) {
	h.broadcastToRoom(room, CSMessage{Type: CSMsgEvent, Payload: event})
}

// finishIfOver 게임이 끝났으면 결과를 알리고 방을 정리한다
func (h *CSHub) finishIfOver(room *csRoom) {
	game := room.Game
	if game.Phase != CSPhaseGameOver {
		return
	}

	h.broadcastToRoom(room, CSMessage{
		Type: CSMsgGameOver,
		Payload: CSGameOverPayload{
			Winner:      game.Winner,
			WinnerName:  game.Names[game.Winner],
			Reason:      game.EndReason,
			ClaimedCols: game.ClaimedBy(game.Winner),
		},
	})

	log.Printf("[캔트스톱][경기결과] game=%s | 승자=%s(%s) | 완등=%v | 소요=%s",
		game.ID, displayName(game.Names[game.Winner]), csSideLabel(game.Winner),
		game.ClaimedBy(game.Winner), matchDuration(game.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "cantstop",
		Players:  displayName(game.Names[CSSouth]) + " vs " + displayName(game.Names[CSNorth]),
		Winner:   displayName(game.Names[game.Winner]),
		Reason:   game.EndReason,
		Duration: matchSeconds(game.StartedAt),
		Bot:      csRoomHasBot(room),
	})

	// 재대결 창: 방·세션을 잠시 유지하고 재대결 신청을 기다린다
	room.Rematch = map[CSSide]bool{}
	gameID := game.ID
	room.CleanupTimer = time.AfterFunc(rematchWindow, func() { h.roomExpired <- gameID })
}

// handleRematch 게임 종료 후 재대결 신청. 봇전은 즉시, 사람전은 양쪽 신청 시 재시작.
func (h *CSHub) handleRematch(client *CSClient) {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase != CSPhaseGameOver || room.CleanupTimer == nil {
		return
	}
	if room.Rematch == nil {
		room.Rematch = map[CSSide]bool{}
	}
	room.Rematch[client.Side] = true

	opponent := room.Clients[csOther(client.Side)]
	if (opponent != nil && opponent.Bot) || room.Rematch[csOther(client.Side)] {
		h.restartRematch(room)
		return
	}
	if opponent != nil {
		h.sendToClient(opponent, CSMessage{Type: CSMsgRematchOffer})
	}
}

// restartRematch 같은 방에서 새 게임을 시작한다 (연결·세션 유지, 봇은 재소환)
func (h *CSHub) restartRematch(room *csRoom) {
	if room.CleanupTimer != nil {
		room.CleanupTimer.Stop()
		room.CleanupTimer = nil
	}
	room.Rematch = nil

	humans := []*CSClient{}
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

	game := NewCSGame(room.Game.ID)
	room.Game = game
	room.Clients = map[CSSide]*CSClient{}
	for _, c := range humans {
		side, err := game.AddPlayer(c.Name)
		if err != nil {
			continue
		}
		c.Side = side
		room.Clients[side] = c
		// 프론트가 세션 키를 다시 저장하도록 입장 확인을 재전송한다
		h.sendToClient(c, CSMessage{
			Type: CSMsgPlayerJoined,
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

	log.Printf("[캔트스톱][재대결] game=%s | 같은 방에서 재시작", game.ID)
	h.startGame(room)
}

// handleRoomExpired 재대결 창이 지나도록 신청이 없으면 방·세션 정리
func (h *CSHub) handleRoomExpired(gameID string) {
	room := h.rooms[gameID]
	if room == nil || room.Game.Phase != CSPhaseGameOver {
		return
	}
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		if !c.Bot {
			h.sendToClient(c, CSMessage{Type: CSMsgSessionExpired})
		}
		h.drop(c.SessionID)
		// 같은 연결이 새 게임에 입장할 수 있도록 신원을 비운다
		// (비우지 않으면 join 연타 가드에 걸려 재입장이 막힌다)
		c.SessionID = ""
		c.GameID = ""
	}
	delete(h.rooms, gameID)
}

// ==================== 재접속 / 연결 끊김 ====================

func (h *CSHub) handleDisconnect(client *CSClient) {
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
			lobbySetWaiting("cantstop", false)
		}
		h.drop(client.SessionID)
		return
	}

	// 진행 중인 게임: 유예 시간 동안 세션을 유지하고 재접속을 기다린다
	log.Printf("[캔트스톱][연결끊김] game=%s | %s=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, csSideLabel(client.Side), displayName(client.Name), h.grace.Seconds())

	if opponent := room.Clients[csOther(client.Side)]; opponent != nil {
		h.sendToClient(opponent, CSMessage{
			Type: CSMsgOpponentDisconnected,
			Payload: map[string]interface{}{
				"message":      "상대방 연결이 끊겼습니다. 재접속을 기다리는 중...",
				"graceSeconds": int(h.grace.Seconds()),
			},
		})
	}

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 시간 안에 재접속하지 않은 세션 정리
func (h *CSHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil {
		return
	}

	log.Printf("[캔트스톱][재접속실패] game=%s | %s=%s 유예 시간 만료로 게임 종료",
		room.Game.ID, csSideLabel(client.Side), displayName(client.Name))

	if opponent := room.Clients[csOther(client.Side)]; opponent != nil {
		h.sendToClient(opponent, CSMessage{
			Type:    CSMsgError,
			Payload: map[string]string{"message": "상대방이 재접속하지 않아 게임이 종료되었습니다"},
		})
		// 상대는 로비로 돌아가도록 세션도 만료 처리
		h.sendToClient(opponent, CSMessage{Type: CSMsgSessionExpired})
		h.drop(opponent.SessionID)
		opponent.GameID = ""
	}

	delete(h.rooms, room.Game.ID)
	if h.waitingRoom != nil && h.waitingRoom.Game.ID == room.Game.ID {
		h.waitingRoom = nil
		lobbySetWaiting("cantstop", false)
	}
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *CSHub) handleRejoin(client *CSClient, msg CSMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload CSRejoinGamePayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil {
		h.sendToClient(client, CSMessage{Type: CSMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil || !room.Game.Ready {
		h.drop(payload.SessionID)
		h.sendToClient(client, CSMessage{Type: CSMsgSessionExpired})
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

	log.Printf("[캔트스톱][재접속] game=%s | %s=%s 재접속 완료",
		room.Game.ID, csSideLabel(client.Side), displayName(client.Name))

	if opponent := room.Clients[csOther(client.Side)]; opponent != nil {
		h.sendToClient(opponent, CSMessage{Type: CSMsgOpponentReconnected})
	}

	// 현재 게임 상태 전체를 내려서 클라이언트 상태를 복원시킨다
	h.sendToClient(client, CSMessage{
		Type:    CSMsgGameState,
		Payload: h.buildCSState(room, client.Side),
	})
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *CSHub) clearGameSessions(room *csRoom) {
	clearRoomSessions(&h.sessionManager, room.Clients)
}

// ==================== 라벨 / 전송 ====================

// csSideLabel 진영 한글 표기
func csSideLabel(side CSSide) string {
	switch side {
	case CSSouth:
		return "남"
	case CSNorth:
		return "북"
	}
	return string(side)
}

func (h *CSHub) sendError(client *CSClient, message string) {
	h.sendToClient(client, CSMessage{Type: CSMsgError, Payload: map[string]string{"message": message}})
}

func (h *CSHub) sendToClient(client *CSClient, message CSMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[CS] ")
}

func (h *CSHub) broadcastToRoom(room *csRoom, message CSMessage) {
	for _, c := range room.Clients {
		if c != nil {
			h.sendToClient(c, message)
		}
	}
}
