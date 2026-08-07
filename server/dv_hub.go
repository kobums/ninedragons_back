package server

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"sort"
	"time"

	"github.com/google/uuid"
)

// dvRoom 게임(순수 상태)과 좌석별 연결의 매핑. DVGame 은 클라이언트를
// 모르므로 허브가 이 래퍼로 둘을 잇는다.
type dvRoom struct {
	Game    *DVGame
	Clients map[int]*DVClient // seat → client
}

type DVHub struct {
	// 등록된 클라이언트
	clients map[*DVClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*dvRoom

	// 글로벌 로비. 시작 전 방은 하나뿐이다.
	lobby *dvRoom

	// 클라이언트 등록
	register chan *DVClient

	// 클라이언트 등록 해제
	unregister chan *DVClient

	// 게임 메시지
	gameMessage chan DVGameMessage

	// 세션 ID → 현재(또는 마지막) 클라이언트
	sessions map[string]*DVClient

	// 세션 ID → 재접속 대기 타이머
	graceTimers map[string]*time.Timer

	// 재접속 대기 시간 만료 알림
	graceExpired chan string

	// 재접속 대기 시간
	grace time.Duration

	// 셔플·선공 결정용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type DVGameMessage struct {
	Client  *DVClient
	Message DVMessage
}

func NewDVHub() *DVHub {
	return &DVHub{
		register:     make(chan *DVClient),
		unregister:   make(chan *DVClient),
		clients:      make(map[*DVClient]bool),
		rooms:        make(map[string]*dvRoom),
		gameMessage:  make(chan DVGameMessage),
		sessions:     make(map[string]*DVClient),
		graceTimers:  make(map[string]*time.Timer),
		graceExpired: make(chan string),
		grace:        defaultDisconnectGrace,
		rng:          rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *DVHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[DV] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[DV] Client unregistered: %s", client.ID)
			}

		case sessionID := <-h.graceExpired:
			h.handleGraceExpired(sessionID)

		case message := <-h.gameMessage:
			h.handleGameMessage(message)
		}
	}
}

func (h *DVHub) handleGameMessage(gm DVGameMessage) {
	switch gm.Message.Type {
	case DVMsgJoinLobby:
		h.handleJoinLobby(gm.Client, gm.Message)
	case DVMsgLeaveLobby:
		h.handleLeaveLobby(gm.Client)
	case DVMsgStartGame:
		h.handleStartGame(gm.Client)
	case DVMsgRejoinGame:
		h.handleRejoin(gm.Client, gm.Message)
	case DVMsgPlaceJoker:
		h.handlePlaceJoker(gm.Client, gm.Message)
	case DVMsgDrawTile:
		h.handleDrawTile(gm.Client, gm.Message)
	case DVMsgGuess:
		h.handleGuess(gm.Client, gm.Message)
	case DVMsgContinueChoice:
		h.handleContinueChoice(gm.Client, gm.Message)
	case DVMsgRevealOwn:
		h.handleRevealOwn(gm.Client, gm.Message)
	}
}

// ==================== 로비 ====================

func (h *DVHub) handleJoinLobby(client *DVClient, msg DVMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload DVJoinLobbyPayload
	json.Unmarshal(payloadBytes, &payload)

	if h.lobby == nil {
		game := NewDVGame(uuid.New().String())
		h.lobby = &dvRoom{Game: game, Clients: map[int]*DVClient{}}
		h.rooms[game.ID] = h.lobby
		log.Printf("[DV] Created lobby game %s", game.ID)
	}

	room := h.lobby
	seat, err := room.Game.AddPlayer(payload.PlayerName)
	if err != nil {
		h.sendError(client, err.Error())
		return
	}

	client.Name = payload.PlayerName
	client.SessionID = uuid.New().String()
	client.GameID = room.Game.ID
	client.Seat = seat
	h.sessions[client.SessionID] = client
	room.Clients[seat] = client

	log.Printf("[다빈치][입장] game=%s | seat%d=%s 로비 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), len(room.Game.Players), DVMaxPlayers)
	notify("다빈치코드 참가", fmt.Sprintf("%s 입장 (%d/%d)",
		displayName(client.Name), len(room.Game.Players), DVMaxPlayers))

	h.broadcastLobby(room)

	// 4명이 차면 자동 시작
	if len(room.Game.Players) == DVMaxPlayers {
		h.startGame(room)
	}
}

func (h *DVHub) handleLeaveLobby(client *DVClient) {
	room := h.lobby
	if room == nil || client.GameID != room.Game.ID {
		return
	}
	h.removeFromLobby(room, client)
	h.sendToClient(client, DVMessage{Type: DVMsgSessionExpired})
}

// removeFromLobby 로비에서 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *DVHub) removeFromLobby(room *dvRoom, client *DVClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	// 좌석 압축: oldSeat 뒤의 클라이언트를 한 칸씩 당긴다
	rebuilt := map[int]*DVClient{}
	for seat, c := range room.Clients {
		if seat > oldSeat {
			c.Seat = seat - 1
			rebuilt[seat-1] = c
		} else {
			rebuilt[seat] = c
		}
	}
	room.Clients = rebuilt

	delete(h.sessions, client.SessionID)
	client.GameID = ""
	client.Seat = -1

	log.Printf("[다빈치][퇴장] game=%s | %s 로비 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), DVMaxPlayers)

	if len(room.Game.Players) == 0 {
		delete(h.rooms, room.Game.ID)
		h.lobby = nil
		return
	}
	h.broadcastLobby(room)
}

// hostSeat 현재 접속 중인 가장 낮은 좌석 (호스트)
func (h *DVHub) hostSeat(room *dvRoom) int {
	seats := []int{}
	for seat, c := range room.Clients {
		if c != nil && c.Connected {
			seats = append(seats, seat)
		}
	}
	if len(seats) == 0 {
		return -1
	}
	sort.Ints(seats)
	return seats[0]
}

func (h *DVHub) broadcastLobby(room *dvRoom) {
	host := h.hostSeat(room)
	players := []DVLobbyPlayer{}
	for _, p := range room.Game.Players {
		c := room.Clients[p.Seat]
		players = append(players, DVLobbyPlayer{
			Seat:      p.Seat,
			Name:      p.Name,
			Connected: c != nil && c.Connected,
		})
	}

	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, DVMessage{
			Type: DVMsgLobbyState,
			Payload: DVLobbyStatePayload{
				GameID:    room.Game.ID,
				YourSeat:  seat,
				SessionID: c.SessionID,
				HostSeat:  host,
				Players:   players,
				CanStart:  room.Game.CanStart(),
			},
		})
	}
}

func (h *DVHub) handleStartGame(client *DVClient) {
	room := h.lobby
	if room == nil || client.GameID != room.Game.ID {
		h.sendError(client, "로비를 찾을 수 없습니다")
		return
	}
	if client.Seat != h.hostSeat(room) {
		h.sendError(client, "호스트만 시작할 수 있습니다")
		return
	}
	if !room.Game.CanStart() {
		h.sendError(client, "2명 이상 모여야 시작할 수 있습니다")
		return
	}
	h.startGame(room)
}

func (h *DVHub) startGame(room *dvRoom) {
	if err := room.Game.Start(h.rng); err != nil {
		return
	}
	h.lobby = nil // 시작한 방은 로비에서 떼어낸다

	names := []string{}
	for _, p := range room.Game.Players {
		names = append(names, displayName(p.Name))
	}
	log.Printf("[다빈치][경기시작] game=%s | 인원=%d | 선공=seat%d | %v",
		room.Game.ID, len(room.Game.Players), room.Game.CurrentSeat, names)
	notify("다빈치코드 게임 시작", fmt.Sprintf("%d인전 시작", len(room.Game.Players)))

	first := room.Game.CurrentSeat
	h.broadcastEvent(room, DVEventPayload{Kind: "game_started", Seat: &first})
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *DVHub) roomOf(client *DVClient) *dvRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Ready {
		return nil
	}
	return room
}

func (h *DVHub) handlePlaceJoker(client *DVClient, msg DVMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload DVPlaceJokerPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.PlaceJoker(client.Seat, payload.TileID, payload.Position); err != nil {
		h.sendError(client, err.Error())
		return
	}
	seat := client.Seat
	h.broadcastEvent(room, DVEventPayload{Kind: "joker_placed", Seat: &seat})
	h.broadcastState(room)
	h.finishIfOver(room, "last_standing")
}

func (h *DVHub) handleDrawTile(client *DVClient, msg DVMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload DVDrawTilePayload
	json.Unmarshal(payloadBytes, &payload)

	tile, err := room.Game.DrawTile(client.Seat, payload.Color)
	if err != nil {
		h.sendError(client, err.Error())
		return
	}
	seat := client.Seat
	h.broadcastEvent(room, DVEventPayload{Kind: "tile_drawn", Seat: &seat, Color: tile.Color})
	h.broadcastState(room)
}

func (h *DVHub) handleGuess(client *DVClient, msg DVMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload DVGuessPayload
	json.Unmarshal(payloadBytes, &payload)

	result, err := room.Game.Guess(client.Seat, payload.TargetSeat, payload.TileIndex, payload.Value)
	if err != nil {
		h.sendError(client, err.Error())
		return
	}

	log.Printf("[다빈치][추리] game=%s | seat%d → seat%d[%d] = %d | %v",
		room.Game.ID, client.Seat, payload.TargetSeat, payload.TileIndex, payload.Value, result.Correct)

	guessed := payload.Value
	correct := result.Correct
	h.broadcastEvent(room, DVEventPayload{
		Kind:         "guess_made",
		GuesserSeat:  client.Seat,
		TargetSeat:   payload.TargetSeat,
		TileIndex:    payload.TileIndex,
		GuessedValue: &guessed,
		Correct:      &correct,
	})
	if result.Correct {
		owner := result.TargetSeat
		value := result.Tile.Value
		h.broadcastEvent(room, DVEventPayload{
			Kind:      "tile_revealed",
			Seat:      &owner,
			TileIndex: result.TileIndex,
			Color:     result.Tile.Color,
			Value:     &value,
		})
		if result.Eliminated {
			h.broadcastEvent(room, DVEventPayload{Kind: "player_eliminated", Seat: &owner})
		}
	}
	h.broadcastState(room)
	h.finishIfOver(room, "last_standing")
}

func (h *DVHub) handleContinueChoice(client *DVClient, msg DVMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload DVContinueChoicePayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.ContinueChoice(client.Seat, payload.Continue); err != nil {
		h.sendError(client, err.Error())
		return
	}
	if !payload.Continue {
		seat := client.Seat
		h.broadcastEvent(room, DVEventPayload{Kind: "turn_stopped", Seat: &seat})
	}
	h.broadcastState(room)
}

func (h *DVHub) handleRevealOwn(client *DVClient, msg DVMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload DVRevealOwnPayload
	json.Unmarshal(payloadBytes, &payload)

	seat := client.Seat
	if err := room.Game.RevealOwn(seat, payload.TileIndex); err != nil {
		h.sendError(client, err.Error())
		return
	}
	tile := room.Game.Players[seat].Tiles[payload.TileIndex]
	value := tile.Value
	h.broadcastEvent(room, DVEventPayload{
		Kind:      "tile_revealed",
		Seat:      &seat,
		TileIndex: payload.TileIndex,
		Color:     tile.Color,
		Value:     &value,
	})
	if room.Game.Players[seat].Eliminated {
		h.broadcastEvent(room, DVEventPayload{Kind: "player_eliminated", Seat: &seat})
	}
	h.broadcastState(room)
	h.finishIfOver(room, "last_standing")
}

// ==================== 상태 뷰 (정보 은닉의 핵심) ====================

func dvIntPtr(v int) *int    { return &v }
func dvBoolPtr(v bool) *bool { return &v }

// maskDVTile 수신자 관점의 타일 뷰. 공개됐거나 내 것이거나 전체 공개
// 모드가 아니면 값을 감춘다.
func maskDVTile(tile DVTile, ownerSeat, viewerSeat int, revealAll bool) DVTileView {
	view := DVTileView{ID: tile.ID, Color: tile.Color, Revealed: tile.Revealed}
	if tile.Revealed || ownerSeat == viewerSeat || revealAll {
		view.Value = dvIntPtr(tile.Value)
		view.Joker = dvBoolPtr(tile.Joker)
	}
	return view
}

// buildDVPlayers 좌석별 플레이어 뷰 목록
func (h *DVHub) buildDVPlayers(room *dvRoom, viewerSeat int, revealAll bool) []DVPlayerView {
	views := []DVPlayerView{}
	for _, p := range room.Game.Players {
		c := room.Clients[p.Seat]
		tiles := []DVTileView{}
		for _, tile := range p.Tiles {
			tiles = append(tiles, maskDVTile(tile, p.Seat, viewerSeat, revealAll))
		}
		views = append(views, DVPlayerView{
			Seat:       p.Seat,
			Name:       p.Name,
			Connected:  c != nil && c.Connected,
			Eliminated: p.Eliminated,
			Tiles:      tiles,
		})
	}
	return views
}

// buildDVState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
func (h *DVHub) buildDVState(room *dvRoom, viewerSeat int) DVGameStatePayload {
	game := room.Game

	pendingSeats := []int{}
	for seat := range game.PendingJokerTiles {
		pendingSeats = append(pendingSeats, seat)
	}
	sort.Ints(pendingSeats)

	var yourPending []DVTileView
	for _, tile := range game.PendingJokerTiles[viewerSeat] {
		yourPending = append(yourPending, maskDVTile(tile, viewerSeat, viewerSeat, false))
	}

	var drawn *DVTileView
	if game.DrawnTile != nil {
		// 실패로 공개가 확정된 조커는 전원에게 값이 보인다.
		visible := viewerSeat == game.CurrentSeat || game.DrawnJokerRevealed
		view := maskDVTile(*game.DrawnTile, game.CurrentSeat, viewerSeat, visible)
		view.Revealed = game.DrawnJokerRevealed
		drawn = &view
	}

	return DVGameStatePayload{
		GameID:            game.ID,
		YourSeat:          viewerSeat,
		Phase:             game.Phase,
		CurrentSeat:       game.CurrentSeat,
		DeckCount:         len(game.Deck),
		DeckBlackCount:    game.DeckCountByColor(DVBlack),
		DeckWhiteCount:    game.DeckCountByColor(DVWhite),
		PlayerCount:       len(game.Players),
		PendingJokerSeats: pendingSeats,
		YourPendingJokers: yourPending,
		DrawnTile:         drawn,
		Players:           h.buildDVPlayers(room, viewerSeat, false),
	}
}

// broadcastState 좌석마다 개인화 스냅샷을 만들어 보낸다
func (h *DVHub) broadcastState(room *dvRoom) {
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, DVMessage{
			Type:    DVMsgGameState,
			Payload: h.buildDVState(room, seat),
		})
	}
}

func (h *DVHub) broadcastEvent(room *dvRoom, event DVEventPayload) {
	h.broadcastToRoom(room, DVMessage{Type: DVMsgEvent, Payload: event})
}

// finishIfOver 게임이 끝났으면 전체 공개 상태로 종료를 알리고 방을 정리한다
func (h *DVHub) finishIfOver(room *dvRoom, reason string) {
	game := room.Game
	if game.Phase != DVPhaseGameOver {
		return
	}

	winnerName := ""
	if game.WinnerSeat >= 0 && game.WinnerSeat < len(game.Players) {
		winnerName = game.Players[game.WinnerSeat].Name
	}

	h.broadcastToRoom(room, DVMessage{
		Type: DVMsgGameOver,
		Payload: DVGameOverPayload{
			WinnerSeat: game.WinnerSeat,
			WinnerName: winnerName,
			Reason:     reason,
			Players:    h.buildDVPlayers(room, -1, true),
		},
	})

	log.Printf("[다빈치][경기결과] game=%s | 승자=seat%d(%s) | 인원=%d | 소요=%s",
		game.ID, game.WinnerSeat, displayName(winnerName), len(game.Players), matchDuration(game.StartedAt))

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
}

// ==================== 재접속 / 연결 끊김 ====================

func (h *DVHub) handleDisconnect(client *DVClient) {
	// 게임 참가 전에 끊긴 연결
	if client.SessionID == "" {
		return
	}
	// 이미 새 연결로 교체된 세션의 옛 연결
	if h.sessions[client.SessionID] != client {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil {
		delete(h.sessions, client.SessionID)
		return
	}

	// 로비 단계: 유예 없이 즉시 자리를 비운다
	if !room.Game.Ready {
		h.removeFromLobby(room, client)
		return
	}

	// 진행 중: 유예 시간 동안 세션을 유지하고 재접속을 기다린다
	log.Printf("[다빈치][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, DVMessage{
		Type: DVMsgPlayerDisconnected,
		Payload: DVPlayerDisconnectedPayload{
			Seat:         client.Seat,
			Name:         client.Name,
			GraceSeconds: int(h.grace.Seconds()),
		},
	})
	h.broadcastState(room)

	sessionID := client.SessionID
	h.graceTimers[sessionID] = time.AfterFunc(h.grace, func() {
		h.graceExpired <- sessionID
	})
}

// handleGraceExpired 유예 안에 재접속하지 않은 플레이어는 몰수 처리하고
// 게임은 남은 인원으로 계속한다
func (h *DVHub) handleGraceExpired(sessionID string) {
	delete(h.graceTimers, sessionID)

	client := h.sessions[sessionID]
	if client == nil || client.Connected {
		// 그 사이 재접속했으면 아무것도 하지 않는다
		return
	}
	delete(h.sessions, sessionID)

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == DVPhaseGameOver {
		return
	}

	log.Printf("[다빈치][재접속실패] game=%s | seat%d=%s 유예 만료 → 몰수",
		room.Game.ID, client.Seat, displayName(client.Name))

	room.Game.ForfeitPlayer(client.Seat)
	seat := client.Seat
	h.broadcastEvent(room, DVEventPayload{Kind: "player_forfeited", Seat: &seat})
	h.broadcastState(room)
	h.finishIfOver(room, "forfeit_win")
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *DVHub) handleRejoin(client *DVClient, msg DVMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload DVRejoinGamePayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil {
		h.sendToClient(client, DVMessage{Type: DVMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		if t := h.graceTimers[payload.SessionID]; t != nil {
			t.Stop()
			delete(h.graceTimers, payload.SessionID)
		}
		delete(h.sessions, payload.SessionID)
		h.sendToClient(client, DVMessage{Type: DVMsgSessionExpired})
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

	// 신원 인계: 새 연결이 기존 좌석을 이어받는다
	client.SessionID = old.SessionID
	client.Name = old.Name
	client.GameID = old.GameID
	client.Seat = old.Seat
	h.sessions[client.SessionID] = client
	room.Clients[client.Seat] = client

	log.Printf("[다빈치][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	if !room.Game.Ready {
		// 로비 재접속
		h.broadcastLobby(room)
		return
	}

	h.broadcastToRoom(room, DVMessage{
		Type:    DVMsgPlayerReconnected,
		Payload: DVPlayerReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	// 전원에게 접속 상태가 반영된 스냅샷 (재접속 당사자 복원 포함)
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *DVHub) clearGameSessions(room *dvRoom) {
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		if t := h.graceTimers[c.SessionID]; t != nil {
			t.Stop()
			delete(h.graceTimers, c.SessionID)
		}
		delete(h.sessions, c.SessionID)
	}
}

// ==================== 전송 ====================

func (h *DVHub) sendError(client *DVClient, message string) {
	h.sendToClient(client, DVMessage{Type: DVMsgError, Payload: DVErrorPayload{Message: message}})
}

func (h *DVHub) sendToClient(client *DVClient, message DVMessage) {
	// 연결이 끊긴(재접속 대기 중) 플레이어에게는 보내지 않는다
	if client == nil || !client.Connected {
		return
	}

	data, err := json.Marshal(message)
	if err != nil {
		log.Printf("[DV] Error marshaling message: %v", err)
		return
	}

	select {
	case client.Send <- data:
	default:
		// 버퍼가 가득 찬 연결은 죽은 것으로 간주하고 닫는다
		client.Connected = false
		close(client.Send)
	}
}

func (h *DVHub) broadcastToRoom(room *dvRoom, message DVMessage) {
	for _, c := range room.Clients {
		if c != nil {
			h.sendToClient(c, message)
		}
	}
}
