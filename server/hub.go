package server

import (
	"encoding/json"
	"log"
)

type Hub struct {
	// 등록된 클라이언트
	clients map[*Client]bool

	// 게임 목록
	games map[string]*Game

	// 대기 중인 게임
	waitingGame *Game

	// 클라이언트로부터 받은 메시지
	broadcast chan []byte

	// 클라이언트 등록
	register chan *Client

	// 클라이언트 등록 해제
	unregister chan *Client

	// 게임 메시지
	gameMessage chan GameMessage
}

type GameMessage struct {
	Client  *Client
	Message Message
}

func NewHub() *Hub {
	return &Hub{
		broadcast:   make(chan []byte),
		register:    make(chan *Client),
		unregister:  make(chan *Client),
		clients:     make(map[*Client]bool),
		games:       make(map[string]*Game),
		gameMessage: make(chan GameMessage),
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
				close(client.Send)
				h.handleDisconnect(client)
				log.Printf("Client unregistered: %s", client.ID)
			}

		case message := <-h.gameMessage:
			h.handleGameMessage(message)
		}
	}
}

func (h *Hub) handleDisconnect(client *Client) {
	if client.GameID != "" {
		game := h.games[client.GameID]
		if game != nil {
			// 상대방에게 알림
			for color, player := range game.Players {
				if player != nil && player.ID != client.ID {
					h.sendToClient(player, Message{
						Type: MsgError,
						Payload: ErrorPayload{
							Message: "상대방이 연결을 종료했습니다",
						},
					})
					// 상대방도 게임에서 제거
					delete(game.Players, color)
				}
			}
			// 게임 삭제
			delete(h.games, client.GameID)

			// 대기 중인 게임이었다면 초기화
			if h.waitingGame != nil && h.waitingGame.ID == client.GameID {
				h.waitingGame = nil
			}
		}
	}
}

func (h *Hub) handleGameMessage(gm GameMessage) {
	switch gm.Message.Type {
	case MsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case MsgPlayTile:
		h.handlePlayTile(gm.Client, gm.Message)
	}
}

func (h *Hub) handleJoinGame(client *Client, msg Message) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload JoinGamePayload
	json.Unmarshal(payloadBytes, &payload)

	log.Printf("Player %s (%s) joining with color preference: %s", client.ID, payload.PlayerName, payload.Color)

	// 플레이어 이름 저장
	client.Name = payload.PlayerName

	var game *Game

	// 대기 중인 게임이 있으면 참가, 없으면 새로 생성
	if h.waitingGame == nil {
		game = NewGame()
		h.waitingGame = game
		h.games[game.ID] = game
		log.Printf("Created new game %s", game.ID)
	} else {
		game = h.waitingGame
		h.waitingGame = nil // 게임이 가득 찼으므로 대기 게임 초기화
		log.Printf("Joining existing game %s", game.ID)
	}

	client.GameID = game.ID

	// 플레이어 색상 결정
	var color PlayerColor

	// 첫 번째 플레이어인 경우
	if len(game.Players) == 0 {
		// 색상을 선택했으면 그 색상, 아니면 파랑
		if payload.Color == Blue || payload.Color == Red {
			color = payload.Color
		} else {
			color = Blue
		}
	} else {
		// 두 번째 플레이어인 경우 남은 색상 자동 배정
		if game.Players[Blue] == nil {
			color = Blue
		} else if game.Players[Red] == nil {
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
	if err := game.AddPlayer(client, color); err != nil {
		log.Printf("Error adding player: %v", err)
		h.sendToClient(client, Message{
			Type: MsgError,
			Payload: ErrorPayload{
				Message: err.Error(),
			},
		})
		return
	}

	log.Printf("Player %s joined as %s. Total players: %d", client.ID, color, len(game.Players))

	// 플레이어에게 자신의 색상 알림
	h.sendToClient(client, Message{
		Type: MsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourColor": color,
			"gameId":    game.ID,
		},
	})

	// 게임 시작 확인
	if game.Ready {
		log.Printf("Game %s is ready! Starting game with %d players", game.ID, len(game.Players))

		// 플레이어 이름 가져오기
		blueName := ""
		redName := ""
		if p := game.Players[Blue]; p != nil {
			blueName = p.Name
		}
		if p := game.Players[Red]; p != nil {
			redName = p.Name
		}

		// 두 플레이어 모두에게 게임 시작 알림
		for playerColor, player := range game.Players {
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
		log.Printf("Game %s waiting for more players. Current: %d", game.ID, len(game.Players))
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
	game := h.games[client.GameID]
	if game == nil {
		h.sendToClient(client, Message{
			Type: MsgError,
			Payload: ErrorPayload{
				Message: "게임을 찾을 수 없습니다",
			},
		})
		return
	}

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
	h.broadcastToGame(game, Message{
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

		h.broadcastToGame(game, Message{
			Type:    MsgRoundResult,
			Payload: result,
		})

		// 게임 종료 확인
		isOver, finalWinner := game.IsGameOver()
		if isOver {
			h.broadcastToGame(game, Message{
				Type: MsgGameOver,
				Payload: GameOverPayload{
					Winner:   finalWinner,
					BlueWins: game.BlueWins,
					RedWins:  game.RedWins,
				},
			})

			// 게임 종료 처리
			delete(h.games, game.ID)
		}
	}
}

func (h *Hub) sendToClient(client *Client, message Message) {
	data, err := json.Marshal(message)
	if err != nil {
		log.Printf("Error marshaling message: %v", err)
		return
	}

	select {
	case client.Send <- data:
	default:
		close(client.Send)
		delete(h.clients, client)
	}
}

func (h *Hub) broadcastToGame(game *Game, message Message) {
	for _, player := range game.Players {
		if player != nil {
			h.sendToClient(player, message)
		}
	}
}
