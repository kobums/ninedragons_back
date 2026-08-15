package server

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// defaultDisconnectGrace 연결이 끊긴 플레이어의 재접속 대기 시간 기본값
const defaultDisconnectGrace = 90 * time.Second

// ndRoom 게임(순수 상태)과 색상별 연결의 매핑
type ndRoom struct {
	Game    *Game
	Clients map[PlayerColor]*Client
}

type Hub struct {
	// 등록된 클라이언트
	clients map[*Client]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*ndRoom

	// 상대를 기다리는 방
	waitingRoom *ndRoom

	// 클라이언트 등록
	register chan *Client

	// 클라이언트 등록 해제
	unregister chan *Client

	// 게임 메시지
	gameMessage chan GameMessage

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*Client]
}

type GameMessage struct {
	Client  *Client
	Message Message
}

func NewHub() *Hub {
	return &Hub{
		register:       make(chan *Client),
		unregister:     make(chan *Client),
		clients:        make(map[*Client]bool),
		rooms:          make(map[string]*ndRoom),
		gameMessage:    make(chan GameMessage),
		sessionManager: newSessionManager[*Client](),
	}
}

func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("Client unregistered: %s", client.ID)
			}

		case sessionID := <-h.graceExpired:
			h.handleGraceExpired(sessionID)

		case message := <-h.gameMessage:
			h.handleGameMessage(message)
		}
	}
}

func (h *Hub) handleDisconnect(client *Client) {
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

	// 상대를 기다리던 게임은 유지할 이유가 없으니 즉시 정리
	if !room.Game.Ready {
		delete(h.rooms, room.Game.ID)
		if h.waitingRoom != nil && h.waitingRoom.Game.ID == room.Game.ID {
			h.waitingRoom = nil
			lobbySetWaiting("ninedragons", false)
		}
		h.drop(client.SessionID)
		return
	}

	// 진행 중인 게임: 유예 시간 동안 세션을 유지하고 재접속을 기다린다
	log.Printf("[구룡투][연결끊김] game=%s | %s=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, colorLabel(client.Color), displayName(client.Name), h.grace.Seconds())

	if opponent := h.opponentOf(room, client.Color); opponent != nil {
		h.sendToClient(opponent, Message{
			Type: MsgOpponentDisconnected,
			Payload: OpponentDisconnectedPayload{
				Message:      "상대방 연결이 끊겼습니다. 재접속을 기다리는 중...",
				GraceSeconds: int(h.grace.Seconds()),
			},
		})
	}

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 시간 안에 재접속하지 않은 세션 정리
func (h *Hub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil {
		return
	}

	log.Printf("[구룡투][재접속실패] game=%s | %s=%s 유예 시간 만료로 게임 종료",
		room.Game.ID, colorLabel(client.Color), displayName(client.Name))

	if opponent := h.opponentOf(room, client.Color); opponent != nil {
		h.sendToClient(opponent, Message{
			Type: MsgError,
			Payload: ErrorPayload{
				Message: "상대방이 재접속하지 않아 게임이 종료되었습니다",
			},
		})
		// 상대는 로비로 돌아가도록 세션도 만료 처리
		h.sendToClient(opponent, Message{Type: MsgSessionExpired})
		h.drop(opponent.SessionID)
		opponent.GameID = ""
	}

	delete(h.rooms, room.Game.ID)
	if h.waitingRoom != nil && h.waitingRoom.Game.ID == room.Game.ID {
		h.waitingRoom = nil
		lobbySetWaiting("ninedragons", false)
	}
}

func (h *Hub) handleGameMessage(gm GameMessage) {
	switch gm.Message.Type {
	case MsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case MsgRejoinGame:
		h.handleRejoin(gm.Client, gm.Message)
	case MsgPlayTile:
		h.handlePlayTile(gm.Client, gm.Message)
	}
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *Hub) handleRejoin(client *Client, msg Message) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload RejoinGamePayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil {
		h.sendToClient(client, Message{Type: MsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil || !room.Game.Ready {
		h.drop(payload.SessionID)
		h.sendToClient(client, Message{Type: MsgSessionExpired})
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

	log.Printf("[구룡투][재접속] game=%s | %s=%s 재접속 완료",
		room.Game.ID, colorLabel(client.Color), displayName(client.Name))

	if opponent := h.opponentOf(room, client.Color); opponent != nil {
		h.sendToClient(opponent, Message{Type: MsgOpponentReconnected})
	}

	// 현재 게임 상태 전체를 내려서 클라이언트 상태를 복원시킨다
	h.sendToClient(client, Message{
		Type:    MsgGameState,
		Payload: h.buildGameState(room, client.Color),
	})
}

// buildGameState 재접속 복원용 게임 상태 스냅샷
func (h *Hub) buildGameState(room *ndRoom, yourColor PlayerColor) GameStatePayload {
	game := room.Game
	blueName := game.Names[Blue]
	redName := game.Names[Red]

	opponentConnected := false
	if opponent := h.opponentOf(room, yourColor); opponent != nil && opponent.Connected {
		opponentConnected = true
	}

	return GameStatePayload{
		GameID:            game.ID,
		YourColor:         yourColor,
		CurrentRound:      game.CurrentRound,
		BlueWins:          game.BlueWins,
		RedWins:           game.RedWins,
		BlueName:          blueName,
		RedName:           redName,
		CurrentPlayer:     game.GetNextPlayer(),
		BlueUsedTiles:     append([]int{}, game.UsedTiles[Blue]...),
		RedUsedTiles:      append([]int{}, game.UsedTiles[Red]...),
		BlueRoundTile:     game.RoundTiles[Blue],
		RedRoundTile:      game.RoundTiles[Red],
		RoundHistory:      game.History(),
		OpponentConnected: opponentConnected,
	}
}

// opponentOf 방에서 해당 색상의 상대 플레이어
func (h *Hub) opponentOf(room *ndRoom, color PlayerColor) *Client {
	for c, player := range room.Clients {
		if c != color {
			return player
		}
	}
	return nil
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *Hub) clearGameSessions(room *ndRoom) {
	for _, player := range room.Clients {
		if player == nil {
			continue
		}
		h.drop(player.SessionID)
	}
}

func (h *Hub) handleJoinGame(client *Client, msg Message) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하는 것을 막는다
	// (막지 않으면 자기 자신과 매칭되거나 유령 자리가 생긴다)
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload JoinGamePayload
	json.Unmarshal(payloadBytes, &payload)

	log.Printf("Player %s (%s) joining with color preference: %s", client.ID, payload.PlayerName, payload.Color)

	// 플레이어 이름 저장 및 재접속용 세션 발급
	client.Name = payload.PlayerName
	client.SessionID = uuid.New().String()
	h.sessions[client.SessionID] = client

	var room *ndRoom

	// 대기 중인 게임이 있으면 참가, 없으면 새로 생성
	if h.waitingRoom == nil {
		room = &ndRoom{Game: NewGame(), Clients: map[PlayerColor]*Client{}}
		h.waitingRoom = room
		lobbySetWaiting("ninedragons", true)
		h.rooms[room.Game.ID] = room
		log.Printf("Created new game %s", room.Game.ID)
	} else {
		room = h.waitingRoom
		h.waitingRoom = nil // 게임이 가득 찼으므로 대기 게임 초기화
		lobbySetWaiting("ninedragons", false)
		log.Printf("Joining existing game %s", room.Game.ID)
	}

	game := room.Game
	client.GameID = game.ID

	// 플레이어 색상 결정
	var color PlayerColor

	// 첫 번째 플레이어인 경우
	if len(room.Clients) == 0 {
		// 색상을 선택했으면 그 색상, 아니면 파랑
		if payload.Color == Blue || payload.Color == Red {
			color = payload.Color
		} else {
			color = Blue
		}
	} else {
		// 두 번째 플레이어인 경우 남은 색상 자동 배정
		if room.Clients[Blue] == nil {
			color = Blue
		} else if room.Clients[Red] == nil {
			color = Red
		} else {
			h.sendToClient(client, Message{
				Type: MsgError,
				Payload: ErrorPayload{
					Message: "게임이 가득 찼습니다",
				},
			})
			return
		}
	}

	// 플레이어 추가
	if err := game.AddPlayer(client.Name, color); err != nil {
		log.Printf("Error adding player: %v", err)
		h.sendToClient(client, Message{
			Type: MsgError,
			Payload: ErrorPayload{
				Message: err.Error(),
			},
		})
		return
	}
	client.Color = color
	room.Clients[color] = client

	log.Printf("[구룡투][입장] game=%s | %s=%s 게임 입장 (%d/2)",
		game.ID, colorLabel(color), displayName(client.Name), len(room.Clients))

	notify("구룡투 참가", fmt.Sprintf("%s(%s) 입장 (%d/2)",
		displayName(client.Name), colorLabel(color), len(room.Clients)))

	// 플레이어에게 자신의 색상 알림
	h.sendToClient(client, Message{
		Type: MsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourColor": color,
			"gameId":    game.ID,
			"sessionId": client.SessionID,
		},
	})

	// 게임 시작 확인
	if game.Ready {
		log.Printf("Game %s is ready! Starting game with %d players", game.ID, len(room.Clients))

		// 플레이어 이름 가져오기
		blueName := game.Names[Blue]
		redName := game.Names[Red]

		// 경기 시작 로그 (닉네임)
		game.StartedAt = time.Now()
		log.Printf("[구룡투][경기시작] game=%s | 파랑=%s | 빨강=%s | 선공=%s",
			game.ID, displayName(blueName), displayName(redName), game.CurrentPlayer)

		notify("구룡투 게임 시작", fmt.Sprintf("파랑 %s vs 빨강 %s",
			displayName(blueName), displayName(redName)))

		// 두 플레이어 모두에게 게임 시작 알림
		for playerColor, player := range room.Clients {
			h.sendToClient(player, Message{
				Type: MsgGameStart,
				Payload: GameStartPayload{
					FirstPlayer: game.CurrentPlayer,
					YourColor:   playerColor,
					BlueName:    blueName,
					RedName:     redName,
				},
			})
		}
	} else {
		log.Printf("Game %s waiting for more players. Current: %d", game.ID, len(room.Clients))
		// 대기 중 메시지
		h.sendToClient(client, Message{
			Type: MsgWaitingPlayer,
			Payload: map[string]string{
				"message": "상대방을 기다리는 중...",
			},
		})
	}
}

func (h *Hub) handlePlayTile(client *Client, msg Message) {
	room := h.rooms[client.GameID]
	if room == nil {
		h.sendToClient(client, Message{
			Type: MsgError,
			Payload: ErrorPayload{
				Message: "게임을 찾을 수 없습니다",
			},
		})
		return
	}
	game := room.Game

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload PlayTilePayload
	json.Unmarshal(payloadBytes, &payload)

	// 타일 플레이
	if err := game.PlayTile(client.Color, payload.Tile); err != nil {
		h.sendToClient(client, Message{
			Type: MsgError,
			Payload: ErrorPayload{
				Message: err.Error(),
			},
		})
		return
	}

	log.Printf("Player %s (%s) played tile %d", client.ID, client.Color, payload.Tile)

	// 다음 플레이어 결정
	nextPlayer := game.GetNextPlayer()

	// 모든 플레이어에게 타일이 플레이되었음을 알림
	h.broadcastToRoom(room, Message{
		Type: MsgTilePlayed,
		Payload: TilePlayedPayload{
			Color:          client.Color,
			Tile:           payload.Tile,
			Round:          game.CurrentRound,
			NextPlayer:     nextPlayer,
			WaitingFor:     nextPlayer,
			BlueTilePlayed: game.RoundTiles[Blue] != nil,
			RedTilePlayed:  game.RoundTiles[Red] != nil,
		},
	})

	// 라운드 처리
	log.Printf("Checking if round is complete. Blue tile: %v, Red tile: %v", game.RoundTiles[Blue], game.RoundTiles[Red])

	// ProcessRound를 호출하기 전에 현재 라운드 정보 저장
	completedRound := game.CurrentRound
	winner, complete := game.ProcessRound()
	log.Printf("Round complete: %v, Winner: %s, Completed round: %d, New current round: %d", complete, winner, completedRound, game.CurrentRound)

	if complete {
		// 라운드 결과 전송
		result := RoundResultPayload{
			Round:      completedRound,
			BlueTile:   0,
			RedTile:    0,
			Winner:     winner,
			BlueWins:   game.BlueWins,
			RedWins:    game.RedWins,
			NextPlayer: game.CurrentPlayer,
		}

		// 타일 값 설정 (이전 라운드 타일)
		if len(game.UsedTiles[Blue]) > 0 {
			result.BlueTile = game.UsedTiles[Blue][len(game.UsedTiles[Blue])-1]
		}
		if len(game.UsedTiles[Red]) > 0 {
			result.RedTile = game.UsedTiles[Red][len(game.UsedTiles[Red])-1]
		}

		log.Printf("Broadcasting round_result: Round %d, Blue: %d, Red: %d, Winner: %s, Next player: %s",
			result.Round, result.BlueTile, result.RedTile, result.Winner, result.NextPlayer)

		h.broadcastToRoom(room, Message{
			Type:    MsgRoundResult,
			Payload: result,
		})

		// 게임 종료 확인
		isOver, finalWinner := game.IsGameOver()
		if isOver {
			h.broadcastToRoom(room, Message{
				Type: MsgGameOver,
				Payload: GameOverPayload{
					Winner:   finalWinner,
					BlueWins: game.BlueWins,
					RedWins:  game.RedWins,
				},
			})

			// 경기 결과 로그
			h.logMatchResult(game, finalWinner)

			// 게임 종료 처리
			h.clearGameSessions(room)
			delete(h.rooms, game.ID)
		}
	}
}

// logMatchResult 경기 종료 결과 로그 (닉네임, 점수, 라운드, 소요 시간)
func (h *Hub) logMatchResult(game *Game, winner PlayerColor) {
	blueName := displayName(game.Names[Blue])
	redName := displayName(game.Names[Red])

	result := "무승부"
	switch winner {
	case Blue:
		result = fmt.Sprintf("%s(파랑)", blueName)
	case Red:
		result = fmt.Sprintf("%s(빨강)", redName)
	}

	log.Printf("[구룡투][경기결과] game=%s | 승자=%s | 파랑=%s(%d승) vs 빨강=%s(%d승) | 라운드=%d | 소요=%s",
		game.ID, result, blueName, game.BlueWins, redName, game.RedWins,
		game.CurrentRound-1, matchDuration(game.StartedAt))

	winnerName := ""
	switch winner {
	case Blue:
		winnerName = blueName
	case Red:
		winnerName = redName
	}
	RecordMatch(MatchRecord{
		Game:     "ninedragons",
		Players:  blueName + " vs " + redName,
		Winner:   winnerName,
		Duration: matchSeconds(game.StartedAt),
	})
}

// colorLabel 색상 한글 표기
func colorLabel(color PlayerColor) string {
	switch color {
	case Blue:
		return "파랑"
	case Red:
		return "빨강"
	}
	return string(color)
}

// maxLoggedNameLen 로그에 남길 닉네임 최대 길이
const maxLoggedNameLen = 20

// displayName 로그용 닉네임 표기
// 닉네임은 클라이언트가 보낸 값이므로 개행·제어문자를 제거해 로그 위조를 막고 길이를 제한한다
func displayName(name string) string {
	if name == "" {
		return "(이름없음)"
	}

	safe := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, name)

	runes := []rune(safe)
	if len(runes) > maxLoggedNameLen {
		return string(runes[:maxLoggedNameLen]) + "…"
	}
	return safe
}

// matchDuration 경기 소요 시간
func matchDuration(startedAt time.Time) string {
	if startedAt.IsZero() {
		return "-"
	}
	return time.Since(startedAt).Truncate(time.Second).String()
}

func (h *Hub) sendToClient(client *Client, message Message) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "")
}

func (h *Hub) broadcastToRoom(room *ndRoom, message Message) {
	for _, player := range room.Clients {
		if player != nil {
			h.sendToClient(player, message)
		}
	}
}
