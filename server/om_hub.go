package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// omRoom 게임(순수 상태)과 색별 연결의 매핑
type omRoom struct {
	Game    *OMGame
	Clients map[OMColor]*OMClient

	// ---- 재대결 창 (게임 종료 후) ----
	Rematch      map[OMColor]bool
	CleanupTimer *time.Timer
}

type OMHub struct {
	// 등록된 클라이언트
	clients map[*OMClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*omRoom

	// 상대를 기다리는 방
	waitingRoom *omRoom

	// 클라이언트 등록
	register chan *OMClient

	// 클라이언트 등록 해제
	unregister chan *OMClient

	// 게임 메시지
	gameMessage chan OMGameMessage

	// 재대결 창 만료 알림 (gameID)
	roomExpired chan string

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*OMClient]
}

type OMGameMessage struct {
	Client  *OMClient
	Message OMMessage
}

func NewOMHub() *OMHub {
	return &OMHub{
		register:       make(chan *OMClient),
		unregister:     make(chan *OMClient),
		clients:        make(map[*OMClient]bool),
		rooms:          make(map[string]*omRoom),
		gameMessage:    make(chan OMGameMessage),
		roomExpired:    make(chan string, 8),
		sessionManager: newSessionManager[*OMClient](),
	}
}

func (h *OMHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[OM] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[OM] Client unregistered: %s", client.ID)
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

func (h *OMHub) handleGameMessage(gm OMGameMessage) {
	switch gm.Message.Type {
	case OMMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case OMMsgRejoinGame:
		h.handleRejoin(gm.Client, gm.Message)
	case OMMsgMove:
		h.handleMove(gm.Client, gm.Message)
	case OMMsgRematch:
		h.handleRematch(gm.Client)
	}
}

// ==================== 입장 / 매치메이킹 ====================

func (h *OMHub) handleJoinGame(client *OMClient, msg OMMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하는 것을 막는다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload OMJoinGamePayload
	json.Unmarshal(payloadBytes, &payload)
	// 초기 게임군은 playerName, 이후 게임군은 name — 양쪽 표기를 받는다
	payload.PlayerName = resolveJoinName(payloadBytes, payload.PlayerName)

	client.Name = payload.PlayerName
	client.SessionID = uuid.New().String()
	h.sessions[client.SessionID] = client

	// 혼자 연습: 대기 슬롯을 거치지 않고 연습봇과 즉시 매칭 (사람이 먼저 앉아 흑)
	if payload.VsBot {
		game := NewOMGame(uuid.New().String())
		botRoom := &omRoom{Game: game, Clients: map[OMColor]*OMClient{}}
		h.rooms[game.ID] = botRoom

		color, err := game.AddPlayer(client.Name)
		if err != nil {
			h.sendError(client, err.Error())
			return
		}
		client.GameID = game.ID
		client.Color = color
		botRoom.Clients[color] = client

		log.Printf("[오목][입장] game=%s | %s=%s 봇전 시작",
			game.ID, omColorLabel(color), displayName(client.Name))

		h.sendToClient(client, OMMessage{
			Type: OMMsgPlayerJoined,
			Payload: map[string]interface{}{
				"yourColor": color,
				"gameId":    game.ID,
				"sessionId": client.SessionID,
			},
		})
		h.broadcastJoined(botRoom, color, client.Name)

		h.spawnBot(botRoom)
		h.startGame(botRoom)
		return
	}

	var room *omRoom
	if h.waitingRoom == nil {
		game := NewOMGame(uuid.New().String())
		room = &omRoom{Game: game, Clients: map[OMColor]*OMClient{}}
		h.waitingRoom = room
		lobbySetWaiting("omok", true)
		h.rooms[game.ID] = room
		log.Printf("[OM] Created new game %s", game.ID)
	} else {
		room = h.waitingRoom
		h.waitingRoom = nil
		lobbySetWaiting("omok", false)
	}

	color, err := room.Game.AddPlayer(client.Name)
	if err != nil {
		h.sendError(client, err.Error())
		return
	}
	client.GameID = room.Game.ID
	client.Color = color
	room.Clients[color] = client

	log.Printf("[오목][입장] game=%s | %s=%s 게임 입장 (%d/2)",
		room.Game.ID, omColorLabel(color), displayName(client.Name), len(room.Game.Names))
	notify("오목 참가", fmt.Sprintf("%s(%s) 입장 (%d/2)",
		displayName(client.Name), omColorLabel(color), len(room.Game.Names)))

	h.sendToClient(client, OMMessage{
		Type: OMMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourColor": color,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
		},
	})
	h.broadcastJoined(room, color, client.Name)

	if room.Game.IsReady() {
		h.startGame(room)
	} else {
		h.sendToClient(client, OMMessage{
			Type:    OMMsgWaitingPlayer,
			Payload: map[string]string{"message": "상대방을 기다리는 중..."},
		})
	}
}

// broadcastJoined 입장 연출 이벤트
func (h *OMHub) broadcastJoined(room *omRoom, color OMColor, name string) {
	h.broadcastEvent(room, OMEventPayload{
		Kind:    "joined",
		Seat:    color,
		Name:    name,
		Message: fmt.Sprintf("%s님이 %s으로 입장했습니다", displayName(name), omColorLabel(color)),
	})
}

func (h *OMHub) startGame(room *omRoom) {
	if err := room.Game.Start(); err != nil {
		return
	}
	game := room.Game

	log.Printf("[오목][경기시작] game=%s | 흑=%s | 백=%s | 선공=흑",
		game.ID, displayName(game.Names[OMBlack]), displayName(game.Names[OMWhite]))
	if !omRoomHasBot(room) {
		notify("오목 게임 시작", fmt.Sprintf("%s vs %s",
			displayName(game.Names[OMBlack]), displayName(game.Names[OMWhite])))
	}

	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *OMHub) roomOf(client *OMClient) *omRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

func (h *OMHub) handleMove(client *OMClient, msg OMMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload OMMovePayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.Place(client.Color, payload.Row, payload.Col); err != nil {
		h.sendError(client, err.Error())
		return
	}

	h.broadcastEvent(room, OMEventPayload{
		Kind: "placed",
		Seat: client.Color,
		Name: client.Name,
		Message: fmt.Sprintf("%s님이 (%d, %d)에 착수했습니다",
			displayName(client.Name), payload.Row, payload.Col),
	})
	h.broadcastState(room)
	h.finishIfOver(room)
}

// ==================== 상태 뷰 ====================

// buildOMState 게임 스냅샷. 완전 공개 정보라 YourColor 외에는 양측 동일하다.
// 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
func (h *OMHub) buildOMState(room *omRoom, viewer OMColor) OMGameStatePayload {
	game := room.Game

	opponentConnected := false
	if opponent := room.Clients[omOther(viewer)]; opponent != nil && opponent.Connected {
		opponentConnected = true
	}

	// 보드는 항상 15×15 로 채워 보낸다 (nil 슬라이스 금지)
	board := make([][]int, OMBoardSize)
	for r := 0; r < OMBoardSize; r++ {
		row := make([]int, OMBoardSize)
		copy(row, game.Board[r][:])
		board[r] = row
	}

	var lastMove *OMCell
	if game.LastMove != nil {
		lm := *game.LastMove
		lastMove = &lm
	}

	return OMGameStatePayload{
		GameID:            game.ID,
		YourColor:         viewer,
		CurrentColor:      game.CurrentColor,
		BlackName:         game.Names[OMBlack],
		WhiteName:         game.Names[OMWhite],
		Board:             board,
		MoveCount:         game.MoveCount,
		LastMove:          lastMove,
		OpponentConnected: opponentConnected,
	}
}

// broadcastState 색마다 스냅샷을 만들어 보낸다
func (h *OMHub) broadcastState(room *omRoom) {
	for color, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, OMMessage{
			Type:    OMMsgGameState,
			Payload: h.buildOMState(room, color),
		})
	}
}

func (h *OMHub) broadcastEvent(room *omRoom, event OMEventPayload) {
	h.broadcastToRoom(room, OMMessage{Type: OMMsgEvent, Payload: event})
}

// finishIfOver 게임이 끝났으면 결과를 알리고 재대결 창을 연다
func (h *OMHub) finishIfOver(room *omRoom) {
	game := room.Game
	if game.Phase != OMPhaseGameOver {
		return
	}

	line := game.WinLine
	if line == nil {
		line = []OMCell{}
	}
	winnerName := game.Names[game.Winner]

	if game.Winner != "" {
		h.broadcastEvent(room, OMEventPayload{
			Kind:    "game_over",
			Seat:    game.Winner,
			Name:    winnerName,
			Message: fmt.Sprintf("%s님이 오목을 완성했습니다", displayName(winnerName)),
		})
	} else {
		h.broadcastEvent(room, OMEventPayload{
			Kind:    "game_over",
			Name:    "",
			Message: "225수가 모두 채워져 무승부입니다",
		})
	}

	h.broadcastToRoom(room, OMMessage{
		Type: OMMsgGameOver,
		Payload: OMGameOverPayload{
			Winner:     game.Winner,
			WinnerName: winnerName,
			Reason:     game.EndReason,
			Line:       line,
		},
	})

	h.recordResult(room)

	// 재대결 창: 방·세션을 잠시 유지하고 재대결 신청을 기다린다
	room.Rematch = map[OMColor]bool{}
	gameID := game.ID
	room.CleanupTimer = time.AfterFunc(rematchWindow, func() { h.roomExpired <- gameID })
}

// recordResult 경기 로그·ntfy·전적 기록
func (h *OMHub) recordResult(room *omRoom) {
	game := room.Game

	winnerLabel := "무승부"
	recordWinner := ""
	if game.Winner != "" {
		winnerLabel = fmt.Sprintf("%s(%s)", displayName(game.Names[game.Winner]), omColorLabel(game.Winner))
		recordWinner = displayName(game.Names[game.Winner])
	}

	log.Printf("[오목][경기결과] game=%s | 승자=%s | 사유=%s | 소요=%s",
		game.ID, winnerLabel, omEndReasonLabel(game.EndReason), matchDuration(game.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "omok",
		Players:  displayName(game.Names[OMBlack]) + " vs " + displayName(game.Names[OMWhite]),
		Winner:   recordWinner,
		Reason:   game.EndReason,
		Duration: matchSeconds(game.StartedAt),
		Bot:      omRoomHasBot(room),
	})
}

// handleRematch 게임 종료 후 재대결 신청. 봇전은 즉시, 사람전은 양쪽 신청 시 재시작.
func (h *OMHub) handleRematch(client *OMClient) {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase != OMPhaseGameOver || room.CleanupTimer == nil {
		return
	}
	if room.Rematch == nil {
		room.Rematch = map[OMColor]bool{}
	}
	room.Rematch[client.Color] = true

	opponent := room.Clients[omOther(client.Color)]
	if (opponent != nil && opponent.Bot) || room.Rematch[omOther(client.Color)] {
		h.restartRematch(room)
		return
	}
	if opponent != nil {
		h.sendToClient(opponent, OMMessage{Type: OMMsgRematchOffer})
	}
}

// restartRematch 같은 방에서 새 게임을 시작한다 (연결·세션 유지, 봇은 재소환)
func (h *OMHub) restartRematch(room *omRoom) {
	if room.CleanupTimer != nil {
		room.CleanupTimer.Stop()
		room.CleanupTimer = nil
	}
	room.Rematch = nil

	humans := []*OMClient{}
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

	game := NewOMGame(room.Game.ID)
	room.Game = game
	room.Clients = map[OMColor]*OMClient{}
	for _, c := range humans {
		color, err := game.AddPlayer(c.Name)
		if err != nil {
			continue
		}
		c.Color = color
		room.Clients[color] = c
		// 프론트가 세션 키를 다시 저장하도록 입장 확인을 재전송한다
		h.sendToClient(c, OMMessage{
			Type: OMMsgPlayerJoined,
			Payload: map[string]interface{}{
				"yourColor": color,
				"gameId":    game.ID,
				"sessionId": c.SessionID,
			},
		})
	}
	if hadBot {
		h.spawnBot(room)
	}

	log.Printf("[오목][재대결] game=%s | 같은 방에서 재시작", game.ID)
	h.startGame(room)
}

// handleRoomExpired 재대결 창이 지나도록 신청이 없으면 방·세션 정리
func (h *OMHub) handleRoomExpired(gameID string) {
	room := h.rooms[gameID]
	if room == nil || room.Game.Phase != OMPhaseGameOver {
		return
	}
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		if !c.Bot {
			h.sendToClient(c, OMMessage{Type: OMMsgSessionExpired})
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

func (h *OMHub) handleDisconnect(client *OMClient) {
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
			lobbySetWaiting("omok", false)
		}
		h.drop(client.SessionID)
		return
	}

	// 진행 중인 게임: 유예 시간 동안 세션을 유지하고 재접속을 기다린다
	log.Printf("[오목][연결끊김] game=%s | %s=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, omColorLabel(client.Color), displayName(client.Name), h.grace.Seconds())

	if opponent := room.Clients[omOther(client.Color)]; opponent != nil {
		h.sendToClient(opponent, OMMessage{
			Type: OMMsgOpponentDisconnected,
			Payload: map[string]interface{}{
				"message":      "상대방 연결이 끊겼습니다. 재접속을 기다리는 중...",
				"graceSeconds": int(h.grace.Seconds()),
			},
		})
	}

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 시간 안에 재접속하지 않은 세션 정리.
// 진행 중이던 판은 남은 쪽의 몰수승(forfeit)으로 종료한다.
func (h *OMHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil {
		return
	}

	log.Printf("[오목][재접속실패] game=%s | %s=%s 유예 시간 만료로 게임 종료",
		room.Game.ID, omColorLabel(client.Color), displayName(client.Name))

	game := room.Game
	forfeited := false
	if game.Phase == OMPhasePlay {
		forfeited = true
		winner := omOther(client.Color)
		game.Winner = winner
		game.EndReason = "forfeit"
		game.WinLine = []OMCell{}
		game.Phase = OMPhaseGameOver
		game.CurrentColor = ""

		h.broadcastToRoom(room, OMMessage{
			Type: OMMsgGameOver,
			Payload: OMGameOverPayload{
				Winner:     winner,
				WinnerName: game.Names[winner],
				Reason:     "forfeit",
				Line:       []OMCell{},
			},
		})
		h.recordResult(room)
	}

	if opponent := room.Clients[omOther(client.Color)]; opponent != nil {
		if !forfeited && !opponent.Bot {
			h.sendToClient(opponent, OMMessage{
				Type:    OMMsgError,
				Payload: map[string]string{"message": "상대방이 재접속하지 않아 게임이 종료되었습니다"},
			})
		}
		// 상대는 로비로 돌아가도록 세션도 만료 처리 (결과 화면은 프론트가 유지)
		h.sendToClient(opponent, OMMessage{Type: OMMsgSessionExpired})
		h.drop(opponent.SessionID)
		opponent.GameID = ""
	}

	delete(h.rooms, room.Game.ID)
	if h.waitingRoom != nil && h.waitingRoom.Game.ID == room.Game.ID {
		h.waitingRoom = nil
		lobbySetWaiting("omok", false)
	}
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *OMHub) handleRejoin(client *OMClient, msg OMMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload OMRejoinGamePayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil {
		h.sendToClient(client, OMMessage{Type: OMMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil || !room.Game.Ready {
		h.drop(payload.SessionID)
		h.sendToClient(client, OMMessage{Type: OMMsgSessionExpired})
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
	client.Color = old.Color
	h.sessions[client.SessionID] = client
	room.Clients[client.Color] = client

	log.Printf("[오목][재접속] game=%s | %s=%s 재접속 완료",
		room.Game.ID, omColorLabel(client.Color), displayName(client.Name))

	if opponent := room.Clients[omOther(client.Color)]; opponent != nil {
		h.sendToClient(opponent, OMMessage{Type: OMMsgOpponentReconnected})
	}

	// 현재 게임 상태 전체를 내려서 클라이언트 상태를 복원시킨다
	h.sendToClient(client, OMMessage{
		Type:    OMMsgGameState,
		Payload: h.buildOMState(room, client.Color),
	})
}

// ==================== 라벨 / 전송 ====================

// omColorLabel 돌 색 한글 표기
func omColorLabel(color OMColor) string {
	switch color {
	case OMBlack:
		return "흑"
	case OMWhite:
		return "백"
	}
	return string(color)
}

// omEndReasonLabel 종료 사유 한글 표기
func omEndReasonLabel(reason string) string {
	switch reason {
	case "five":
		return "오목 완성"
	case "draw":
		return "무승부 (판이 가득 참)"
	case "forfeit":
		return "몰수"
	}
	return reason
}

func (h *OMHub) sendError(client *OMClient, message string) {
	h.sendToClient(client, OMMessage{Type: OMMsgError, Payload: map[string]string{"message": message}})
}

func (h *OMHub) sendToClient(client *OMClient, message OMMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[OM] ")
}

func (h *OMHub) broadcastToRoom(room *omRoom, message OMMessage) {
	for _, c := range room.Clients {
		if c != nil {
			h.sendToClient(c, message)
		}
	}
}

// ==================== WS 핸들러 ====================

func ServeOMWs(hub *OMHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[OM] Error upgrading connection:", err)
		return
	}

	client := &OMClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[OM] ",
		func(msg OMMessage) { hub.gameMessage <- OMGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
