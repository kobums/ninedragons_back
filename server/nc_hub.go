package server

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/google/uuid"
)

// ncRoom 게임(순수 상태)과 팀별 연결의 매핑
type ncRoom struct {
	Game    *NCGame
	Clients map[TeamColor]*NCClient

	// ---- 재대결 창 (게임 종료 후) ----
	Rematch      map[TeamColor]bool
	CleanupTimer *time.Timer
}

type NCHub struct {
	// 등록된 클라이언트
	clients map[*NCClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*ncRoom

	// 상대를 기다리는 방
	waitingRoom *ncRoom

	// 클라이언트 등록
	register chan *NCClient

	// 클라이언트 등록 해제
	unregister chan *NCClient

	// 게임 메시지
	gameMessage chan NCGameMessage

	// 재대결 창 만료 알림 (gameID)
	roomExpired chan string

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*NCClient]
}

type NCGameMessage struct {
	Client  *NCClient
	Message NCMessage
}

func NewNCHub() *NCHub {
	return &NCHub{
		register:       make(chan *NCClient),
		unregister:     make(chan *NCClient),
		clients:        make(map[*NCClient]bool),
		rooms:          make(map[string]*ncRoom),
		gameMessage:    make(chan NCGameMessage),
		roomExpired:    make(chan string, 8),
		sessionManager: newSessionManager[*NCClient](),
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

		case gameID := <-h.roomExpired:
			h.handleRoomExpired(gameID)

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
			lobbySetWaiting("numberchange", false)
		}
		h.drop(client.SessionID)
		return
	}

	// 진행 중인 게임: 유예 시간 동안 세션을 유지하고 재접속을 기다린다
	log.Printf("[넘버체인지][연결끊김] game=%s | %s=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, teamLabel(client.Team), displayName(client.Name), h.grace.Seconds())

	if opponent := h.opponentOf(room, client.Team); opponent != nil {
		h.sendToClient(opponent, NCMessage{
			Type: NCMsgOpponentDisconnected,
			Payload: NCOpponentDisconnectedPayload{
				Message:      "상대방 연결이 끊겼습니다. 재접속을 기다리는 중...",
				GraceSeconds: int(h.grace.Seconds()),
			},
		})
	}

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 시간 안에 재접속하지 않은 세션 정리
func (h *NCHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil {
		return
	}

	log.Printf("[넘버체인지][재접속실패] game=%s | %s=%s 유예 시간 만료로 게임 종료",
		room.Game.ID, teamLabel(client.Team), displayName(client.Name))

	if opponent := h.opponentOf(room, client.Team); opponent != nil {
		h.sendToClient(opponent, NCMessage{
			Type: NCMsgError,
			Payload: NCErrorPayload{
				Message: "상대방이 재접속하지 않아 게임이 종료되었습니다",
			},
		})
		// 상대는 로비로 돌아가도록 세션도 만료 처리
		h.sendToClient(opponent, NCMessage{Type: NCMsgSessionExpired})
		h.drop(opponent.SessionID)
		opponent.GameID = ""
	}

	delete(h.rooms, room.Game.ID)
	if h.waitingRoom != nil && h.waitingRoom.Game.ID == room.Game.ID {
		h.waitingRoom = nil
		lobbySetWaiting("numberchange", false)
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
	case NCMsgRematch:
		h.handleRematch(gm.Client)
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

	room := h.rooms[old.GameID]
	if room == nil || !room.Game.Ready {
		h.drop(payload.SessionID)
		h.sendToClient(client, NCMessage{Type: NCMsgSessionExpired})
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
	client.Team = old.Team
	h.sessions[client.SessionID] = client
	room.Clients[client.Team] = client

	log.Printf("[넘버체인지][재접속] game=%s | %s=%s 재접속 완료",
		room.Game.ID, teamLabel(client.Team), displayName(client.Name))

	if opponent := h.opponentOf(room, client.Team); opponent != nil {
		h.sendToClient(opponent, NCMessage{Type: NCMsgOpponentReconnected})
	}

	// 현재 게임 상태 전체를 내려서 클라이언트 상태를 복원시킨다
	h.sendToClient(client, NCMessage{
		Type:    NCMsgGameState,
		Payload: h.buildGameState(room, client.Team),
	})
}

// buildGameState 재접속 복원용 게임 상태 스냅샷
func (h *NCHub) buildGameState(room *ncRoom, yourTeam TeamColor) NCGameStatePayload {
	game := room.Game
	opponentTeam := Team1
	if yourTeam == Team1 {
		opponentTeam = Team2
	}

	opponentConnected := false
	if opponent := room.Clients[opponentTeam]; opponent != nil && opponent.Connected {
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
		Team1Name:                   game.Names[Team1],
		Team2Name:                   game.Names[Team2],
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

// opponentOf 방에서 해당 팀의 상대 플레이어
func (h *NCHub) opponentOf(room *ncRoom, team TeamColor) *NCClient {
	for t, player := range room.Clients {
		if t != team {
			return player
		}
	}
	return nil
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *NCHub) clearGameSessions(room *ncRoom) {
	for _, player := range room.Clients {
		if player == nil {
			continue
		}
		h.drop(player.SessionID)
	}
}

func (h *NCHub) handleJoinGame(client *NCClient, msg NCMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하는 것을 막는다
	// (막지 않으면 자기 자신과 매칭되거나 유령 자리가 생긴다)
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload NCJoinGamePayload
	json.Unmarshal(payloadBytes, &payload)
	// 초기 게임군은 playerName, 이후 게임군은 name — 양쪽 표기를 받는다
	payload.PlayerName = resolveJoinName(payloadBytes, payload.PlayerName)

	log.Printf("[NC] Player %s (%s) joining with team preference: %s", client.ID, payload.PlayerName, payload.Team)

	// 플레이어 이름 저장 및 재접속용 세션 발급
	client.Name = payload.PlayerName
	client.SessionID = uuid.New().String()
	h.sessions[client.SessionID] = client

	// 혼자 연습: 대기 슬롯을 거치지 않고 연습봇과 즉시 매칭
	if payload.VsBot {
		botRoom := &ncRoom{Game: NewNCGame(uuid.New().String()), Clients: map[TeamColor]*NCClient{}}
		h.rooms[botRoom.Game.ID] = botRoom
		game := botRoom.Game
		client.GameID = game.ID

		// 사람은 선호 팀, 봇은 남은 팀
		team := game.AddPlayer(client.Name, payload.Team)
		client.Team = team
		botRoom.Clients[team] = client

		log.Printf("[넘버체인지][입장] game=%s | %s=%s 봇전 시작",
			game.ID, teamLabel(team), displayName(client.Name))

		h.sendToClient(client, NCMessage{
			Type: NCMsgPlayerJoined,
			Payload: map[string]interface{}{
				"yourTeam":  team,
				"gameId":    game.ID,
				"sessionId": client.SessionID,
			},
		})

		h.spawnBot(botRoom)
		h.startGame(botRoom)
		return
	}

	var room *ncRoom

	// 대기 중인 게임이 있으면 참가, 없으면 새로 생성
	if h.waitingRoom == nil {
		gameID := uuid.New().String()
		room = &ncRoom{Game: NewNCGame(gameID), Clients: map[TeamColor]*NCClient{}}
		h.waitingRoom = room
		lobbySetWaiting("numberchange", true)
		h.rooms[room.Game.ID] = room
		log.Printf("[NC] Created new game %s", room.Game.ID)
	} else {
		room = h.waitingRoom
		h.waitingRoom = nil // 게임이 가득 찼으므로 대기 게임 초기화
		lobbySetWaiting("numberchange", false)
		log.Printf("[NC] Joining existing game %s", room.Game.ID)
	}

	game := room.Game
	client.GameID = game.ID

	// 플레이어 팀 배정
	team := game.AddPlayer(client.Name, payload.Team)
	client.Team = team
	room.Clients[team] = client

	log.Printf("[넘버체인지][입장] game=%s | %s=%s 게임 입장 (%d/2)",
		game.ID, teamLabel(team), displayName(client.Name), len(room.Clients))

	notify("넘버체인지 참가", fmt.Sprintf("%s(%s) 입장 (%d/2)",
		displayName(client.Name), teamLabel(team), len(room.Clients)))

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
		log.Printf("[NC] Game %s is ready! Starting game with %d players", game.ID, len(room.Clients))
		h.startGame(room)
	} else {
		log.Printf("[NC] Game %s waiting for more players. Current: %d", game.ID, len(room.Clients))
		// 대기 중 메시지
		h.sendToClient(client, NCMessage{
			Type: NCMsgWaitingPlayer,
			Payload: map[string]string{
				"message": "상대방을 기다리는 중...",
			},
		})
	}
}

// startGame 두 팀이 모두 앉은 방의 게임 시작 브로드캐스트 (입장·재대결 공용)
func (h *NCHub) startGame(room *ncRoom) {
	game := room.Game
	if !game.IsReady() || game.Ready {
		return
	}
	game.Start()

	// 플레이어 이름 가져오기
	team1Name := game.Names[Team1]
	team2Name := game.Names[Team2]

	// 경기 시작 로그 (닉네임)
	log.Printf("[넘버체인지][경기시작] game=%s | 팀1=%s | 팀2=%s | 선공=%s",
		game.ID, displayName(team1Name), displayName(team2Name), game.CurrentTeam)

	if !ncRoomHasBot(room) {
		notify("넘버체인지 게임 시작", fmt.Sprintf("팀1 %s vs 팀2 %s",
			displayName(team1Name), displayName(team2Name)))
	}

	// 두 플레이어 모두에게 게임 시작 알림
	for playerTeam, player := range room.Clients {
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
}

func (h *NCHub) handleSubmitBlocks(client *NCClient, msg NCMessage) {
	room := h.rooms[client.GameID]
	if room == nil {
		h.sendToClient(client, NCMessage{
			Type: NCMsgError,
			Payload: NCErrorPayload{
				Message: "게임을 찾을 수 없습니다",
			},
		})
		return
	}

	game := room.Game

	// 재대결 창(게임 종료 후) 동안 들어온 뒤늦은 제출은 무시한다
	if over, _ := game.IsGameOver(); over {
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

		if opponentClient := room.Clients[opponentTeam]; opponentClient != nil {
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
	h.tryProcessRound(room)
}

// tryProcessRound 필요한 제출·블록 선택이 모두 끝났을 때만 라운드를 처리하고 결과를 전송
func (h *NCHub) tryProcessRound(room *ncRoom) {
	game := room.Game
	if !game.ReadyToProcess() {
		return
	}

	result, err := game.ProcessRound()
	if err != nil {
		log.Printf("[NC] Error processing round: %v", err)
		return
	}

	// 라운드 결과 전송
	h.broadcastToRoom(room, NCMessage{
		Type:    NCMsgRoundResult,
		Payload: result,
	})

	// 게임 종료 확인
	isOver, reason := game.IsGameOver()
	if !isOver {
		return
	}

	winner := game.GetWinner()
	h.broadcastToRoom(room, NCMessage{
		Type: NCMsgGameOver,
		Payload: NCGameOverPayload{
			Winner:     winner,
			Team1Score: game.Team1Score,
			Team2Score: game.Team2Score,
			Reason:     reason,
		},
	})

	// 경기 결과 로그
	h.logMatchResult(game, winner, reason)

	// 재대결 창: 방·세션을 잠시 유지하고 재대결 신청을 기다린다
	room.Rematch = map[TeamColor]bool{}
	gameID := game.ID
	room.CleanupTimer = time.AfterFunc(rematchWindow, func() { h.roomExpired <- gameID })
}

// handleRematch 게임 종료 후 재대결 신청. 봇전은 즉시, 사람전은 양쪽 신청 시 재시작.
func (h *NCHub) handleRematch(client *NCClient) {
	room := h.rooms[client.GameID]
	if room == nil || room.CleanupTimer == nil {
		return
	}
	if over, _ := room.Game.IsGameOver(); !over {
		return
	}
	if room.Rematch == nil {
		room.Rematch = map[TeamColor]bool{}
	}
	room.Rematch[client.Team] = true

	otherTeam := Team2
	if client.Team == Team2 {
		otherTeam = Team1
	}
	opponent := room.Clients[otherTeam]
	if (opponent != nil && opponent.Bot) || room.Rematch[otherTeam] {
		h.restartRematch(room)
		return
	}
	if opponent != nil {
		h.sendToClient(opponent, NCMessage{Type: NCMsgRematchOffer})
	}
}

// restartRematch 같은 방에서 새 게임을 시작한다 (연결·세션 유지, 봇은 재소환)
func (h *NCHub) restartRematch(room *ncRoom) {
	if room.CleanupTimer != nil {
		room.CleanupTimer.Stop()
		room.CleanupTimer = nil
	}
	room.Rematch = nil

	humans := []*NCClient{}
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

	game := NewNCGame(room.Game.ID)
	room.Game = game
	room.Clients = map[TeamColor]*NCClient{}
	for _, c := range humans {
		// 이전 판과 같은 팀 유지 (선호 팀으로 재등록)
		team := game.AddPlayer(c.Name, c.Team)
		c.Team = team
		room.Clients[team] = c
		// 프론트가 세션 키를 다시 저장하도록 입장 확인을 재전송한다
		h.sendToClient(c, NCMessage{
			Type: NCMsgPlayerJoined,
			Payload: map[string]interface{}{
				"yourTeam":  team,
				"gameId":    game.ID,
				"sessionId": c.SessionID,
			},
		})
	}
	if hadBot {
		h.spawnBot(room)
	}

	log.Printf("[넘버체인지][재대결] game=%s | 같은 방에서 재시작", game.ID)
	h.startGame(room)
}

// handleRoomExpired 재대결 창이 지나도록 신청이 없으면 방·세션 정리
func (h *NCHub) handleRoomExpired(gameID string) {
	room := h.rooms[gameID]
	if room == nil {
		return
	}
	if over, _ := room.Game.IsGameOver(); !over {
		return
	}
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		if !c.Bot {
			h.sendToClient(c, NCMessage{Type: NCMsgSessionExpired})
		}
		h.drop(c.SessionID)
		// 같은 연결이 새 게임에 입장할 수 있도록 신원을 비운다
		// (비우지 않으면 join 연타 가드에 걸려 재입장이 막힌다)
		c.SessionID = ""
		c.GameID = ""
	}
	delete(h.rooms, gameID)
}

func (h *NCHub) handleSelectBlock(client *NCClient, msg NCMessage) {
	room := h.rooms[client.GameID]
	if room == nil {
		h.sendToClient(client, NCMessage{
			Type: NCMsgError,
			Payload: NCErrorPayload{
				Message: "게임을 찾을 수 없습니다",
			},
		})
		return
	}

	// 재대결 창(게임 종료 후) 동안 들어온 뒤늦은 선택은 무시한다
	if over, _ := room.Game.IsGameOver(); over {
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
	if submit := room.Game.RoundSubmits[client.Team]; submit != nil {
		submit.SelectedBlockChoice = payload.SelectedBlockChoice
		log.Printf("[NC] Team %s updated block choice: %d", client.Team, payload.SelectedBlockChoice)

		// 제출·블록 선택이 모두 끝났으면 라운드 처리
		// (양팀 모두 히든을 쓴 경우 두 번째 선택이 도착할 때까지 기다린다)
		h.tryProcessRound(room)
	}
}

// logMatchResult 경기 종료 결과 로그 (닉네임, 점수, 라운드, 소요 시간)
func (h *NCHub) logMatchResult(game *NCGame, winner TeamColor, reason string) {
	team1Name := displayName(game.Names[Team1])
	team2Name := displayName(game.Names[Team2])

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

	winnerName := ""
	switch winner {
	case Team1:
		winnerName = team1Name
	case Team2:
		winnerName = team2Name
	}
	RecordMatch(MatchRecord{
		Game:     "numberchange",
		Players:  team1Name + " vs " + team2Name,
		Winner:   winnerName,
		Reason:   reason,
		Duration: matchSeconds(game.StartedAt),
	})
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
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[NC] ")
}

func (h *NCHub) broadcastToRoom(room *ncRoom, message NCMessage) {
	for _, player := range room.Clients {
		if player != nil {
			h.sendToClient(player, message)
		}
	}
}
