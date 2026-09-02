package server

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"time"

	"github.com/google/uuid"
)

// gsRoom 게임(순수 상태)과 진영별 연결의 매핑
type gsRoom struct {
	Game    *GSGame
	Clients map[GSSide]*GSClient

	// ---- 재대결 창 (게임 종료 후) ----
	Rematch      map[GSSide]bool
	CleanupTimer *time.Timer
}

type GSHub struct {
	// 등록된 클라이언트
	clients map[*GSClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*gsRoom

	// 상대를 기다리는 방
	waitingRoom *gsRoom

	// 클라이언트 등록
	register chan *GSClient

	// 클라이언트 등록 해제
	unregister chan *GSClient

	// 게임 메시지
	gameMessage chan GSGameMessage

	// 재대결 창 만료 알림 (gameID)
	roomExpired chan string

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*GSClient]

	// 선공 결정용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type GSGameMessage struct {
	Client  *GSClient
	Message GSMessage
}

func NewGSHub() *GSHub {
	return &GSHub{
		register:       make(chan *GSClient),
		unregister:     make(chan *GSClient),
		clients:        make(map[*GSClient]bool),
		rooms:          make(map[string]*gsRoom),
		gameMessage:    make(chan GSGameMessage),
		roomExpired:    make(chan string, 8),
		sessionManager: newSessionManager[*GSClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *GSHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[GS] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[GS] Client unregistered: %s", client.ID)
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

func (h *GSHub) handleGameMessage(gm GSGameMessage) {
	switch gm.Message.Type {
	case GSMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case GSMsgRejoinGame:
		h.handleRejoin(gm.Client, gm.Message)
	case GSMsgSubmitSetup:
		h.handleSubmitSetup(gm.Client, gm.Message)
	case GSMsgMove:
		h.handleMove(gm.Client, gm.Message)
	case GSMsgRematch:
		h.handleRematch(gm.Client)
	}
}

// ==================== 입장 / 매치메이킹 ====================

func (h *GSHub) handleJoinGame(client *GSClient, msg GSMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하는 것을 막는다
	// (막지 않으면 자기 자신과 매칭되거나 유령 자리가 생긴다)
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload GSJoinGamePayload
	json.Unmarshal(payloadBytes, &payload)
	// 초기 게임군은 playerName, 이후 게임군은 name — 양쪽 표기를 받는다
	payload.PlayerName = resolveJoinName(payloadBytes, payload.PlayerName)

	client.Name = payload.PlayerName
	client.SessionID = uuid.New().String()
	h.sessions[client.SessionID] = client

	// 혼자 연습: 대기 슬롯을 거치지 않고 연습봇과 즉시 매칭
	if payload.VsBot {
		game := NewGSGame(uuid.New().String())
		botRoom := &gsRoom{Game: game, Clients: map[GSSide]*GSClient{}}
		h.rooms[game.ID] = botRoom

		side, err := game.AddPlayer(client.Name)
		if err != nil {
			h.sendError(client, err.Error())
			return
		}
		client.GameID = game.ID
		client.Side = side
		botRoom.Clients[side] = client

		log.Printf("[가이스터][입장] game=%s | %s=%s 봇전 시작",
			game.ID, gsSideLabel(side), displayName(client.Name))

		h.sendToClient(client, GSMessage{
			Type: GSMsgPlayerJoined,
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

	var room *gsRoom
	if h.waitingRoom == nil {
		game := NewGSGame(uuid.New().String())
		room = &gsRoom{Game: game, Clients: map[GSSide]*GSClient{}}
		h.waitingRoom = room
		lobbySetWaiting("geister", true)
		h.rooms[game.ID] = room
		log.Printf("[GS] Created new game %s", game.ID)
	} else {
		room = h.waitingRoom
		h.waitingRoom = nil
		lobbySetWaiting("geister", false)
	}

	side, err := room.Game.AddPlayer(client.Name)
	if err != nil {
		h.sendError(client, err.Error())
		return
	}
	client.GameID = room.Game.ID
	client.Side = side
	room.Clients[side] = client

	log.Printf("[가이스터][입장] game=%s | %s=%s 게임 입장 (%d/2)",
		room.Game.ID, gsSideLabel(side), displayName(client.Name), len(room.Game.Names))
	notify("가이스터 참가", fmt.Sprintf("%s(%s) 입장 (%d/2)",
		displayName(client.Name), gsSideLabel(side), len(room.Game.Names)))

	h.sendToClient(client, GSMessage{
		Type: GSMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourSide":  side,
			"gameId":    room.Game.ID,
			"sessionId": client.SessionID,
		},
	})

	if room.Game.IsReady() {
		h.startGame(room)
	} else {
		h.sendToClient(client, GSMessage{
			Type:    GSMsgWaitingPlayer,
			Payload: map[string]string{"message": "상대방을 기다리는 중..."},
		})
	}
}

func (h *GSHub) startGame(room *gsRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	game := room.Game

	log.Printf("[가이스터][경기시작] game=%s | 남=%s | 북=%s | 선공=%s",
		game.ID, displayName(game.Names[GSSouth]), displayName(game.Names[GSNorth]),
		gsSideLabel(game.CurrentSide))
	if !gsRoomHasBot(room) {
		notify("가이스터 게임 시작", fmt.Sprintf("%s vs %s",
			displayName(game.Names[GSSouth]), displayName(game.Names[GSNorth])))
	}

	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *GSHub) roomOf(client *GSClient) *gsRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

func (h *GSHub) handleSubmitSetup(client *GSClient, msg GSMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload GSSubmitSetupPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.SubmitSetup(client.Side, payload.GoodCells); err != nil {
		h.sendError(client, err.Error())
		return
	}
	// 배치 내용은 비밀 — 이벤트 없이 상태(준비 여부)만 갱신한다
	h.broadcastState(room)
}

func (h *GSHub) handleMove(client *GSClient, msg GSMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload GSMovePayload
	json.Unmarshal(payloadBytes, &payload)

	result, err := room.Game.Move(client.Side, payload.From, payload.To, payload.Escape)
	if err != nil {
		h.sendError(client, err.Error())
		return
	}

	from := payload.From
	if result.Escaped {
		log.Printf("[가이스터][탈출] game=%s | %s=%s (%d,%d) 탈출",
			room.Game.ID, gsSideLabel(client.Side), displayName(client.Name), from.Row, from.Col)
		h.broadcastEvent(room, GSEventPayload{Kind: "escape", Side: client.Side, From: &from})
	} else {
		to := payload.To
		h.broadcastEvent(room, GSEventPayload{Kind: "move", Side: client.Side, From: &from, To: &to})
		if result.Captured {
			good := result.CapturedGood
			h.broadcastEvent(room, GSEventPayload{Kind: "capture", Side: client.Side, To: &to, Good: &good})
		}
	}

	h.broadcastState(room)
	h.finishIfOver(room)
}

// ==================== 상태 뷰 (정체 은닉의 핵심) ====================

func gsBoolPtr(v bool) *bool { return &v }

// buildGSGhosts 수신자 관점의 유령 목록. 정체(Good)는 내 유령이거나
// 전체 공개 모드(게임 종료)일 때만 싣는다. 잡힘/탈출 유령은 revealAll
// 모드에서만 포함한다(최종 보드 표시용).
func buildGSGhosts(game *GSGame, viewer GSSide, revealAll bool) []GSGhostView {
	views := []GSGhostView{}
	for _, ghost := range game.Ghosts {
		if (ghost.Captured || ghost.Escaped) && !revealAll {
			continue
		}
		view := GSGhostView{
			ID: ghost.ID, Side: ghost.Side, Row: ghost.Row, Col: ghost.Col,
			Captured: ghost.Captured, Escaped: ghost.Escaped,
		}
		if ghost.Side == viewer || revealAll {
			view.Good = gsBoolPtr(ghost.Good)
		}
		views = append(views, view)
	}
	return views
}

// buildGSCaptured side 진영이 잡은 유령 목록 (색은 공개 정보)
func buildGSCaptured(game *GSGame, capturer GSSide) []GSCapturedView {
	views := []GSCapturedView{}
	for _, ghost := range game.Ghosts {
		if ghost.Captured && ghost.Side == gsOther(capturer) {
			views = append(views, GSCapturedView{Good: ghost.Good})
		}
	}
	return views
}

// buildGSState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
func (h *GSHub) buildGSState(room *gsRoom, viewer GSSide) GSGameStatePayload {
	game := room.Game

	opponentConnected := false
	if opponent := room.Clients[gsOther(viewer)]; opponent != nil && opponent.Connected {
		opponentConnected = true
	}

	return GSGameStatePayload{
		GameID:            game.ID,
		YourSide:          viewer,
		Phase:             game.Phase,
		CurrentSide:       game.CurrentSide,
		SouthName:         game.Names[GSSouth],
		NorthName:         game.Names[GSNorth],
		YourReady:         game.SetupDone[viewer],
		OpponentReady:     game.SetupDone[gsOther(viewer)],
		Ghosts:            buildGSGhosts(game, viewer, false),
		CapturedBySouth:   buildGSCaptured(game, GSSouth),
		CapturedByNorth:   buildGSCaptured(game, GSNorth),
		OpponentConnected: opponentConnected,
	}
}

// broadcastState 진영마다 개인화 스냅샷을 만들어 보낸다
func (h *GSHub) broadcastState(room *gsRoom) {
	for side, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, GSMessage{
			Type:    GSMsgGameState,
			Payload: h.buildGSState(room, side),
		})
	}
}

func (h *GSHub) broadcastEvent(room *gsRoom, event GSEventPayload) {
	h.broadcastToRoom(room, GSMessage{Type: GSMsgEvent, Payload: event})
}

// finishIfOver 게임이 끝났으면 전체 정체를 공개하고 방을 정리한다
func (h *GSHub) finishIfOver(room *gsRoom) {
	game := room.Game
	if game.Phase != GSPhaseGameOver {
		return
	}

	h.broadcastToRoom(room, GSMessage{
		Type: GSMsgGameOver,
		Payload: GSGameOverPayload{
			Winner:     game.Winner,
			WinnerName: game.Names[game.Winner],
			Reason:     game.EndReason,
			Ghosts:     buildGSGhosts(game, "", true),
		},
	})

	log.Printf("[가이스터][경기결과] game=%s | 승자=%s(%s) | 사유=%s | 소요=%s",
		game.ID, displayName(game.Names[game.Winner]), gsSideLabel(game.Winner),
		gsEndReasonLabel(game.EndReason), matchDuration(game.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "geister",
		Players:  displayName(game.Names[GSSouth]) + " vs " + displayName(game.Names[GSNorth]),
		Winner:   displayName(game.Names[game.Winner]),
		Reason:   game.EndReason,
		Duration: matchSeconds(game.StartedAt),
		Bot:      gsRoomHasBot(room),
	})

	// 재대결 창: 방·세션을 잠시 유지하고 재대결 신청을 기다린다
	room.Rematch = map[GSSide]bool{}
	gameID := game.ID
	room.CleanupTimer = time.AfterFunc(rematchWindow, func() { h.roomExpired <- gameID })
}

// handleRematch 게임 종료 후 재대결 신청. 봇전은 즉시, 사람전은 양쪽 신청 시 재시작.
func (h *GSHub) handleRematch(client *GSClient) {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase != GSPhaseGameOver || room.CleanupTimer == nil {
		return
	}
	if room.Rematch == nil {
		room.Rematch = map[GSSide]bool{}
	}
	room.Rematch[client.Side] = true

	opponent := room.Clients[gsOther(client.Side)]
	if (opponent != nil && opponent.Bot) || room.Rematch[gsOther(client.Side)] {
		h.restartRematch(room)
		return
	}
	if opponent != nil {
		h.sendToClient(opponent, GSMessage{Type: GSMsgRematchOffer})
	}
}

// restartRematch 같은 방에서 새 게임을 시작한다 (연결·세션 유지, 봇은 재소환)
func (h *GSHub) restartRematch(room *gsRoom) {
	if room.CleanupTimer != nil {
		room.CleanupTimer.Stop()
		room.CleanupTimer = nil
	}
	room.Rematch = nil

	humans := []*GSClient{}
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

	game := NewGSGame(room.Game.ID)
	room.Game = game
	room.Clients = map[GSSide]*GSClient{}
	for _, c := range humans {
		side, err := game.AddPlayer(c.Name)
		if err != nil {
			continue
		}
		c.Side = side
		room.Clients[side] = c
		// 프론트가 세션 키를 다시 저장하도록 입장 확인을 재전송한다
		h.sendToClient(c, GSMessage{
			Type: GSMsgPlayerJoined,
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

	log.Printf("[가이스터][재대결] game=%s | 같은 방에서 재시작", game.ID)
	h.startGame(room)
}

// handleRoomExpired 재대결 창이 지나도록 신청이 없으면 방·세션 정리
func (h *GSHub) handleRoomExpired(gameID string) {
	room := h.rooms[gameID]
	if room == nil || room.Game.Phase != GSPhaseGameOver {
		return
	}
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		if !c.Bot {
			h.sendToClient(c, GSMessage{Type: GSMsgSessionExpired})
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

func (h *GSHub) handleDisconnect(client *GSClient) {
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
			lobbySetWaiting("geister", false)
		}
		h.drop(client.SessionID)
		return
	}

	// 진행 중인 게임: 유예 시간 동안 세션을 유지하고 재접속을 기다린다
	log.Printf("[가이스터][연결끊김] game=%s | %s=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, gsSideLabel(client.Side), displayName(client.Name), h.grace.Seconds())

	if opponent := room.Clients[gsOther(client.Side)]; opponent != nil {
		h.sendToClient(opponent, GSMessage{
			Type: GSMsgOpponentDisconnected,
			Payload: map[string]interface{}{
				"message":      "상대방 연결이 끊겼습니다. 재접속을 기다리는 중...",
				"graceSeconds": int(h.grace.Seconds()),
			},
		})
	}

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 시간 안에 재접속하지 않은 세션 정리
func (h *GSHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil {
		return
	}

	log.Printf("[가이스터][재접속실패] game=%s | %s=%s 유예 시간 만료로 게임 종료",
		room.Game.ID, gsSideLabel(client.Side), displayName(client.Name))

	if opponent := room.Clients[gsOther(client.Side)]; opponent != nil {
		h.sendToClient(opponent, GSMessage{
			Type:    GSMsgError,
			Payload: map[string]string{"message": "상대방이 재접속하지 않아 게임이 종료되었습니다"},
		})
		// 상대는 로비로 돌아가도록 세션도 만료 처리
		h.sendToClient(opponent, GSMessage{Type: GSMsgSessionExpired})
		h.drop(opponent.SessionID)
		opponent.GameID = ""
	}

	delete(h.rooms, room.Game.ID)
	if h.waitingRoom != nil && h.waitingRoom.Game.ID == room.Game.ID {
		h.waitingRoom = nil
		lobbySetWaiting("geister", false)
	}
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *GSHub) handleRejoin(client *GSClient, msg GSMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload GSRejoinGamePayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil {
		h.sendToClient(client, GSMessage{Type: GSMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil || !room.Game.Ready {
		h.drop(payload.SessionID)
		h.sendToClient(client, GSMessage{Type: GSMsgSessionExpired})
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

	log.Printf("[가이스터][재접속] game=%s | %s=%s 재접속 완료",
		room.Game.ID, gsSideLabel(client.Side), displayName(client.Name))

	if opponent := room.Clients[gsOther(client.Side)]; opponent != nil {
		h.sendToClient(opponent, GSMessage{Type: GSMsgOpponentReconnected})
	}

	// 현재 게임 상태 전체를 내려서 클라이언트 상태를 복원시킨다
	h.sendToClient(client, GSMessage{
		Type:    GSMsgGameState,
		Payload: h.buildGSState(room, client.Side),
	})
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *GSHub) clearGameSessions(room *gsRoom) {
	clearRoomSessions(&h.sessionManager, room.Clients)
}

// ==================== 라벨 / 전송 ====================

// gsSideLabel 진영 한글 표기
func gsSideLabel(side GSSide) string {
	switch side {
	case GSSouth:
		return "남"
	case GSNorth:
		return "북"
	}
	return string(side)
}

// gsEndReasonLabel 종료 사유 한글 표기
func gsEndReasonLabel(reason string) string {
	switch reason {
	case "escape":
		return "탈출"
	case "captured_all_good":
		return "좋은 유령 전멸"
	case "fed_all_evil":
		return "나쁜 유령 헌납"
	}
	return reason
}

func (h *GSHub) sendError(client *GSClient, message string) {
	h.sendToClient(client, GSMessage{Type: GSMsgError, Payload: map[string]string{"message": message}})
}

func (h *GSHub) sendToClient(client *GSClient, message GSMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[GS] ")
}

func (h *GSHub) broadcastToRoom(room *gsRoom, message GSMessage) {
	for _, c := range room.Clients {
		if c != nil {
			h.sendToClient(c, message)
		}
	}
}
