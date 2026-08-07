package server

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"time"

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

	// 세션 ID → 현재(또는 마지막) 클라이언트
	sessions map[string]*NCClient

	// 세션 ID → 재접속 대기 타이머
	graceTimers map[string]*time.Timer

	// 재접속 대기 시간 만료 알림
	graceExpired chan string

	// 재접속 대기 시간
	grace time.Duration
}

type NCGameMessage struct {
	Client  *NCClient
	Message NCMessage
}

func NewNCHub() *NCHub {
	return &NCHub{
		broadcast:    make(chan []byte),
		register:     make(chan *NCClient),
		unregister:   make(chan *NCClient),
		clients:      make(map[*NCClient]bool),
		games:        make(map[string]*NCGame),
		gameMessage:  make(chan NCGameMessage),
		sessions:     make(map[string]*NCClient),
		graceTimers:  make(map[string]*time.Timer),
		graceExpired: make(chan string),
		grace:        defaultDisconnectGrace,
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
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[NC] Client unregistered: %s", client.ID)
			}

		case sessionID := <-h.graceExpired:
			h.handleGraceExpired(sessionID)

		case message := <-h.gameMessage:
			h.handleGameMessage(message)
		}
	}
}

func (h *NCHub) handleDisconnect(client *NCClient) {
	// 게임 참가 전에 끊긴 연결
	if client.SessionID == "" {
		return
	}
	// 이미 새 연결로 교체된 세션의 옛 연결
	if h.sessions[client.SessionID] != client {
		return
	}

	game := h.games[client.GameID]
	if game == nil {
		delete(h.sessions, client.SessionID)
		return
	}

	// 상대를 기다리던 게임은 유지할 이유가 없으니 즉시 정리
	if !game.Ready {
		delete(h.games, game.ID)
		if h.waitingGame != nil && h.waitingGame.ID == game.ID {
			h.waitingGame = nil
		}
		delete(h.sessions, client.SessionID)
		return
	}

	// 진행 중인 게임: 유예 시간 동안 세션을 유지하고 재접속을 기다린다
	log.Printf("[넘버체인지][연결끊김] game=%s | %s=%s 재접속 대기 시작 (%.0f초)",
		game.ID, teamLabel(client.Team), displayName(client.Name), h.grace.Seconds())

	if opponent := h.opponentOf(game, client.Team); opponent != nil {
		h.sendToClient(opponent, NCMessage{
			Type: NCMsgOpponentDisconnected,
			Payload: NCOpponentDisconnectedPayload{
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
func (h *NCHub) handleGraceExpired(sessionID string) {
	delete(h.graceTimers, sessionID)

	client := h.sessions[sessionID]
	if client == nil || client.Connected {
		// 그 사이 재접속했으면 아무것도 하지 않는다
		return
	}
	delete(h.sessions, sessionID)

	game := h.games[client.GameID]
	if game == nil {
		return
	}

	log.Printf("[넘버체인지][재접속실패] game=%s | %s=%s 유예 시간 만료로 게임 종료",
		game.ID, teamLabel(client.Team), displayName(client.Name))

	if opponent := h.opponentOf(game, client.Team); opponent != nil {
		h.sendToClient(opponent, NCMessage{
			Type: NCMsgError,
			Payload: NCErrorPayload{
				Message: "상대방이 재접속하지 않아 게임이 종료되었습니다",
			},
		})
		// 상대는 로비로 돌아가도록 세션도 만료 처리
		h.sendToClient(opponent, NCMessage{Type: NCMsgSessionExpired})
		delete(h.sessions, opponent.SessionID)
		opponent.GameID = ""
	}

	delete(h.games, game.ID)
	if h.waitingGame != nil && h.waitingGame.ID == game.ID {
		h.waitingGame = nil
	}
}

func (h *NCHub) handleGameMessage(gm NCGameMessage) {
	switch gm.Message.Type {
	case NCMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case NCMsgRejoinGame:
		h.handleRejoin(gm.Client, gm.Message)
	case NCMsgSubmitBlocks:
		h.handleSubmitBlocks(gm.Client, gm.Message)
	case NCMsgSelectBlock:
		h.handleSelectBlock(gm.Client, gm.Message)
	}
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *NCHub) handleRejoin(client *NCClient, msg NCMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload NCRejoinGamePayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil {
		h.sendToClient(client, NCMessage{Type: NCMsgSessionExpired})
		return
	}

	game := h.games[old.GameID]
	if game == nil || !game.Ready {
		if t := h.graceTimers[payload.SessionID]; t != nil {
			t.Stop()
			delete(h.graceTimers, payload.SessionID)
		}
		delete(h.sessions, payload.SessionID)
		h.sendToClient(client, NCMessage{Type: NCMsgSessionExpired})
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
	client.Team = old.Team
	h.sessions[client.SessionID] = client
	game.Players[client.Team] = client

	log.Printf("[넘버체인지][재접속] game=%s | %s=%s 재접속 완료",
		game.ID, teamLabel(client.Team), displayName(client.Name))

	if opponent := h.opponentOf(game, client.Team); opponent != nil {
		h.sendToClient(opponent, NCMessage{Type: NCMsgOpponentReconnected})
	}

	// 현재 게임 상태 전체를 내려서 클라이언트 상태를 복원시킨다
	h.sendToClient(client, NCMessage{
		Type:    NCMsgGameState,
		Payload: h.buildGameState(game, client.Team),
	})
}

// buildGameState 재접속 복원용 게임 상태 스냅샷
func (h *NCHub) buildGameState(game *NCGame, yourTeam TeamColor) NCGameStatePayload {
	opponentTeam := Team1
	if yourTeam == Team1 {
		opponentTeam = Team2
	}

	opponentConnected := false
	if opponent := game.Players[opponentTeam]; opponent != nil && opponent.Connected {
		opponentConnected = true
	}

	yourBlocks := append([]int{}, game.AvailableBlocks[yourTeam]...)
	opponentBlocks := append([]int{}, game.AvailableBlocks[opponentTeam]...)
	sort.Ints(yourBlocks)
	sort.Ints(opponentBlocks)

	yourUsedHidden := game.Team1UsedHidden
	opponentUsedHidden := game.Team2UsedHidden
	if yourTeam == Team2 {
		yourUsedHidden, opponentUsedHidden = opponentUsedHidden, yourUsedHidden
	}

	yourSubmit := game.RoundSubmits[yourTeam]
	opponentSubmit := game.RoundSubmits[opponentTeam]

	return NCGameStatePayload{
		GameID:                      game.ID,
		YourTeam:                    yourTeam,
		CurrentRound:                game.CurrentRound,
		Team1Score:                  game.Team1Score,
		Team2Score:                  game.Team2Score,
		Team1Name:                   ncClientName(game.Players[Team1]),
		Team2Name:                   ncClientName(game.Players[Team2]),
		CurrentTeam:                 game.CurrentTeam,
		YourBlocks:                  yourBlocks,
		OpponentBlocks:              opponentBlocks,
		RoundHistory:                append([]NCRoundHistory{}, game.RoundHistory...),
		YourUsedHidden:              yourUsedHidden,
		OpponentUsedHidden:          opponentUsedHidden,
		YouSubmitted:                yourSubmit != nil,
		OpponentSubmitted:           opponentSubmit != nil,
		OpponentUsedHiddenThisRound: opponentSubmit != nil && opponentSubmit.UseHidden,
		YourBlockChoiceMade:         yourSubmit != nil && yourSubmit.SelectedBlockChoice != 0,
		OpponentConnected:           opponentConnected,
	}
}

// opponentOf 게임에서 해당 팀의 상대 플레이어
func (h *NCHub) opponentOf(game *NCGame, team TeamColor) *NCClient {
	for t, player := range game.Players {
		if t != team {
			return player
		}
	}
	return nil
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *NCHub) clearGameSessions(game *NCGame) {
	for _, player := range game.Players {
		if player == nil {
			continue
		}
		if t := h.graceTimers[player.SessionID]; t != nil {
			t.Stop()
			delete(h.graceTimers, player.SessionID)
		}
		delete(h.sessions, player.SessionID)
	}
}

func (h *NCHub) handleJoinGame(client *NCClient, msg NCMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload NCJoinGamePayload
	json.Unmarshal(payloadBytes, &payload)

	log.Printf("[NC] Player %s (%s) joining with team preference: %s", client.ID, payload.PlayerName, payload.Team)

	// 플레이어 이름 저장 및 재접속용 세션 발급
	client.Name = payload.PlayerName
	client.SessionID = uuid.New().String()
	h.sessions[client.SessionID] = client

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

	log.Printf("[넘버체인지][입장] game=%s | %s=%s 게임 입장 (%d/2)",
		game.ID, teamLabel(team), displayName(client.Name), len(game.Players))

	notify("넘버체인지 참가", fmt.Sprintf("%s(%s) 입장 (%d/2)",
		displayName(client.Name), teamLabel(team), len(game.Players)))

	// 플레이어에게 자신의 팀 알림
	h.sendToClient(client, NCMessage{
		Type: NCMsgPlayerJoined,
		Payload: map[string]interface{}{
			"yourTeam":  team,
			"gameId":    game.ID,
			"sessionId": client.SessionID,
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

		// 경기 시작 로그 (닉네임)
		log.Printf("[넘버체인지][경기시작] game=%s | 팀1=%s | 팀2=%s | 선공=%s",
			game.ID, displayName(team1Name), displayName(team2Name), game.CurrentTeam)

		notify("넘버체인지 게임 시작", fmt.Sprintf("팀1 %s vs 팀2 %s",
			displayName(team1Name), displayName(team2Name)))

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

	// 제출·블록 선택이 모두 끝났으면 라운드 처리
	// (히든 사용 시 필요한 블록 선택은 제출 페이로드에 포함돼 오거나
	//  이후 nc_select_block으로 도착한다 — ReadyToProcess가 두 경우 모두 판별)
	h.tryProcessRound(game)
}

// tryProcessRound 필요한 제출·블록 선택이 모두 끝났을 때만 라운드를 처리하고 결과를 전송
func (h *NCHub) tryProcessRound(game *NCGame) {
	if !game.ReadyToProcess() {
		return
	}

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
	if !isOver {
		return
	}

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
	h.clearGameSessions(game)
	delete(h.games, game.ID)
	h.logMatchResult(game, winner, reason)
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

	// 블록 선택 값 검증 (1: 상대 블록1, 2: 상대 블록2)
	if payload.SelectedBlockChoice != 1 && payload.SelectedBlockChoice != 2 {
		h.sendToClient(client, NCMessage{
			Type: NCMsgError,
			Payload: NCErrorPayload{
				Message: "잘못된 블록 선택입니다",
			},
		})
		return
	}

	// 이미 제출한 상태에서 블록 선택 업데이트
	if submit := game.RoundSubmits[client.Team]; submit != nil {
		submit.SelectedBlockChoice = payload.SelectedBlockChoice
		log.Printf("[NC] Team %s updated block choice: %d", client.Team, payload.SelectedBlockChoice)

		// 제출·블록 선택이 모두 끝났으면 라운드 처리
		// (양팀 모두 히든을 쓴 경우 두 번째 선택이 도착할 때까지 기다린다)
		h.tryProcessRound(game)
	}
}

// logMatchResult 경기 종료 결과 로그 (닉네임, 점수, 라운드, 소요 시간)
func (h *NCHub) logMatchResult(game *NCGame, winner TeamColor, reason string) {
	team1Name := displayName(ncClientName(game.Players[Team1]))
	team2Name := displayName(ncClientName(game.Players[Team2]))

	result := "무승부"
	switch winner {
	case Team1:
		result = fmt.Sprintf("%s(팀1)", team1Name)
	case Team2:
		result = fmt.Sprintf("%s(팀2)", team2Name)
	}

	log.Printf("[넘버체인지][경기결과] game=%s | 승자=%s | 팀1=%s(%d점) vs 팀2=%s(%d점) | 라운드=%d | 종료사유=%s | 소요=%s",
		game.ID, result, team1Name, game.Team1Score, team2Name, game.Team2Score,
		game.CurrentRound-1, ncEndReason(reason), matchDuration(game.StartedAt))
}

// teamLabel 팀 한글 표기
func teamLabel(team TeamColor) string {
	switch team {
	case Team1:
		return "팀1"
	case Team2:
		return "팀2"
	}
	return string(team)
}

// ncClientName 클라이언트 이름 (nil 안전)
func ncClientName(client *NCClient) string {
	if client == nil {
		return ""
	}
	return client.Name
}

// ncEndReason 종료 사유 한글 표기
func ncEndReason(reason string) string {
	switch reason {
	case "score_limit":
		return "7점 선취"
	case "rounds_complete":
		return "12라운드 종료"
	case "overtime":
		return "연장전"
	}
	return reason
}

func (h *NCHub) sendToClient(client *NCClient, message NCMessage) {
	// 연결이 끊긴(재접속 대기 중) 플레이어에게는 보내지 않는다
	if client == nil || !client.Connected {
		return
	}

	data, err := json.Marshal(message)
	if err != nil {
		log.Printf("[NC] Error marshaling message: %v", err)
		return
	}

	select {
	case client.Send <- data:
	default:
		// 버퍼가 가득 찬 연결은 죽은 것으로 간주하고 닫는다
		// (이후 readPump 종료 → unregister 경로에서 세션 정리가 이어진다)
		client.Connected = false
		close(client.Send)
	}
}

func (h *NCHub) broadcastToGame(game *NCGame, message NCMessage) {
	for _, player := range game.Players {
		if player != nil {
			h.sendToClient(player, message)
		}
	}
}
