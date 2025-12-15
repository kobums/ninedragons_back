package server

import (
	"encoding/json"
	"log"

	"github.com/google/uuid"
)

type NCHub struct {
	// 등록된 클라이언트
	clients map[*NCClient]bool

	// 게임 목록
	games map[string]*NCGame

	// 대기 중인 게임
	waitingGame *NCGame

	// 클라이언트로부터 받은 메시지
	broadcast chan []byte

	// 클라이언트 등록
	register chan *NCClient

	// 클라이언트 등록 해제
	unregister chan *NCClient

	// 게임 메시지
	gameMessage chan NCGameMessage
}

type NCGameMessage struct {
	Client  *NCClient
	Message NCMessage
}

func NewNCHub() *NCHub {
	return &NCHub{
		broadcast:   make(chan []byte),
		register:    make(chan *NCClient),
		unregister:  make(chan *NCClient),
		clients:     make(map[*NCClient]bool),
		games:       make(map[string]*NCGame),
		gameMessage: make(chan NCGameMessage),
	}
}

func (h *NCHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[NC] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.Send)
				h.handleDisconnect(client)
				log.Printf("[NC] Client unregistered: %s", client.ID)
			}

		case message := <-h.gameMessage:
			h.handleGameMessage(message)
		}
	}
}

func (h *NCHub) handleDisconnect(client *NCClient) {
	if client.GameID != "" {
		game := h.games[client.GameID]
		if game != nil {
			// 상대방에게 알림
			for team, player := range game.Players {
				if player != nil && player.ID != client.ID {
					h.sendToClient(player, NCMessage{
						Type: NCMsgError,
						Payload: NCErrorPayload{
							Message: "상대방이 연결을 종료했습니다",
						},
					})
					// 상대방도 게임에서 제거
					delete(game.Players, team)
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

func (h *NCHub) handleGameMessage(gm NCGameMessage) {
	switch gm.Message.Type {
	case NCMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case NCMsgSubmitBlocks:
		h.handleSubmitBlocks(gm.Client, gm.Message)
	case NCMsgSelectBlock:
		h.handleSelectBlock(gm.Client, gm.Message)
	}
}

func (h *NCHub) handleJoinGame(client *NCClient, msg NCMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload NCJoinGamePayload
	json.Unmarshal(payloadBytes, &payload)

	log.Printf("[NC] Player %s (%s) joining with team preference: %s", client.ID, payload.PlayerName, payload.Team)

	// 플레이어 이름 저장
	client.Name = payload.PlayerName

	var game *NCGame

	// 대기 중인 게임이 있으면 참가, 없으면 새로 생성
	if h.waitingGame == nil {
		gameID := uuid.New().String()
		game = NewNCGame(gameID)
		h.waitingGame = game
		h.games[game.ID] = game
		log.Printf("[NC] Created new game %s", game.ID)
	} else {
		game = h.waitingGame
		h.waitingGame = nil // 게임이 가득 찼으므로 대기 게임 초기화
		log.Printf("[NC] Joining existing game %s", game.ID)
	}

	client.GameID = game.ID

	// 플레이어 팀 배정
	team := game.AddPlayer(client, payload.Team)
	client.Team = team

	log.Printf("[NC] Player %s (%s) joined as %s. Total players: %d", client.ID, payload.PlayerName, team, len(game.Players))

	// 플레이어에게 자신의 팀 알림
	h.sendToClient(client, NCMessage{
		Type: NCMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourTeam": team,
			"gameId":   game.ID,
		},
	})

	// 게임 시작 확인
	if game.IsReady() && !game.Ready {
		game.Start()
		log.Printf("[NC] Game %s is ready! Starting game with %d players", game.ID, len(game.Players))

		// 플레이어 이름 가져오기
		team1Name := ""
		team2Name := ""
		if p := game.Players[Team1]; p != nil {
			team1Name = p.Name
		}
		if p := game.Players[Team2]; p != nil {
			team2Name = p.Name
		}

		// 두 플레이어 모두에게 게임 시작 알림
		for playerTeam, player := range game.Players {
			h.sendToClient(player, NCMessage{
				Type: NCMsgGameStart,
				Payload: NCGameStartPayload{
					YourTeam:  playerTeam,
					FirstTeam: game.CurrentTeam,
					Team1Name: team1Name,
					Team2Name: team2Name,
				},
			})
		}
	} else {
		log.Printf("[NC] Game %s waiting for more players. Current: %d", game.ID, len(game.Players))
		// 대기 중 메시지
		h.sendToClient(client, NCMessage{
			Type: NCMsgWaitingPlayer,
			Payload: map[string]string{
				"message": "상대방을 기다리는 중...",
			},
		})
	}
}

func (h *NCHub) handleSubmitBlocks(client *NCClient, msg NCMessage) {
	game := h.games[client.GameID]
	if game == nil {
		h.sendToClient(client, NCMessage{
			Type: NCMsgError,
			Payload: NCErrorPayload{
				Message: "게임을 찾을 수 없습니다",
			},
		})
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload NCSubmitBlocksPayload
	json.Unmarshal(payloadBytes, &payload)

	// 블록 제출
	if err := game.SubmitBlocks(client.Team, payload.Block1, payload.Block2, payload.UseHidden, payload.SelectedBlockChoice); err != nil {
		h.sendToClient(client, NCMessage{
			Type: NCMsgError,
			Payload: NCErrorPayload{
				Message: err.Error(),
			},
		})
		return
	}

	log.Printf("[NC] Team %s submitted blocks: %d, %d (hidden: %v, choice: %d)",
		client.Team, payload.Block1, payload.Block2, payload.UseHidden, payload.SelectedBlockChoice)

	// 히든 찬스 사용 시 상대방에게 알림
	if payload.UseHidden {
		var opponentTeam TeamColor
		if client.Team == Team1 {
			opponentTeam = Team2
		} else {
			opponentTeam = Team1
		}

		if opponentClient := game.Players[opponentTeam]; opponentClient != nil {
			h.sendToClient(opponentClient, NCMessage{
				Type: NCMsgUseHidden,
				Payload: map[string]interface{}{
					"team": client.Team,
				},
			})
			log.Printf("[NC] Notified %s that %s used hidden chance", opponentTeam, client.Team)
		}
	}

	// 양 팀이 모두 제출했는지 확인
	if len(game.RoundSubmits) == 2 {
		// 어느 한 팀이라도 히든을 사용했는지 확인
		team1Submit := game.RoundSubmits[Team1]
		team2Submit := game.RoundSubmits[Team2]

		anyoneUsedHidden := (team1Submit != nil && team1Submit.UseHidden) || (team2Submit != nil && team2Submit.UseHidden)

		// 누군가 히든을 사용했다면, 상대방의 블록 선택을 기다려야 함
		if anyoneUsedHidden {
			log.Printf("[NC] Someone used hidden, waiting for block selection in handleSelectBlock")
			return
		}

		// 라운드 처리 (둘 다 히든을 사용하지 않은 경우만)
		result, err := game.ProcessRound()
		if err != nil {
			log.Printf("[NC] Error processing round: %v", err)
			return
		}

		// 라운드 결과 전송
		h.broadcastToGame(game, NCMessage{
			Type:    NCMsgRoundResult,
			Payload: result,
		})

		// 게임 종료 확인
		isOver, reason := game.IsGameOver()
		if isOver {
			winner := game.GetWinner()
			h.broadcastToGame(game, NCMessage{
				Type: NCMsgGameOver,
				Payload: NCGameOverPayload{
					Winner:     winner,
					Team1Score: game.Team1Score,
					Team2Score: game.Team2Score,
					Reason:     reason,
				},
			})

			// 게임 종료 처리
			delete(h.games, game.ID)
			log.Printf("[NC] Game %s ended. Winner: %s, Reason: %s", game.ID, winner, reason)
		}
	}
}

func (h *NCHub) handleSelectBlock(client *NCClient, msg NCMessage) {
	game := h.games[client.GameID]
	if game == nil {
		h.sendToClient(client, NCMessage{
			Type: NCMsgError,
			Payload: NCErrorPayload{
				Message: "게임을 찾을 수 없습니다",
			},
		})
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload NCSelectBlockPayload
	json.Unmarshal(payloadBytes, &payload)

	// 이미 제출한 상태에서 블록 선택 업데이트
	if submit := game.RoundSubmits[client.Team]; submit != nil {
		submit.SelectedBlockChoice = payload.SelectedBlockChoice
		log.Printf("[NC] Team %s updated block choice: %d", client.Team, payload.SelectedBlockChoice)

		// 양 팀이 모두 제출했는지 확인
		if len(game.RoundSubmits) == 2 {
			// 상대가 히든을 사용했는지 확인
			var opponentTeam TeamColor
			if client.Team == Team1 {
				opponentTeam = Team2
			} else {
				opponentTeam = Team1
			}

			opponentSubmit := game.RoundSubmits[opponentTeam]
			if opponentSubmit != nil && opponentSubmit.UseHidden {
				// 블록 선택이 완료되었으므로 라운드 처리
				result, err := game.ProcessRound()
				if err != nil {
					log.Printf("[NC] Error processing round: %v", err)
					return
				}

				// 라운드 결과 전송
				h.broadcastToGame(game, NCMessage{
					Type:    NCMsgRoundResult,
					Payload: result,
				})

				// 게임 종료 확인
				isOver, reason := game.IsGameOver()
				if isOver {
					winner := game.GetWinner()
					h.broadcastToGame(game, NCMessage{
						Type: NCMsgGameOver,
						Payload: NCGameOverPayload{
							Winner:     winner,
							Team1Score: game.Team1Score,
							Team2Score: game.Team2Score,
							Reason:     reason,
						},
					})

					// 게임 종료 처리
					delete(h.games, game.ID)
					log.Printf("[NC] Game %s ended. Winner: %s, Reason: %s", game.ID, winner, reason)
				}
			}
		}
	}
}

func (h *NCHub) sendToClient(client *NCClient, message NCMessage) {
	data, err := json.Marshal(message)
	if err != nil {
		log.Printf("[NC] Error marshaling message: %v", err)
		return
	}

	select {
	case client.Send <- data:
	default:
		close(client.Send)
		delete(h.clients, client)
	}
}

func (h *NCHub) broadcastToGame(game *NCGame, message NCMessage) {
	for _, player := range game.Players {
		if player != nil {
			h.sendToClient(player, message)
		}
	}
}
