package server

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// dvRoom 게임(순수 상태)과 좌석별 연결의 매핑. DVGame 은 클라이언트를
// 모르므로 허브가 이 래퍼로 둘을 잇는다.
type dvRoom struct {
	Game    *DVGame
	Clients map[int]*DVClient // seat → client

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string

	// Spectators 관전자 연결 (사설 방 코드로만 진입, 상한 maxSpectators).
	// 좌석·세션이 없어 재접속을 지원하지 않는다 — 끊기면 목록에서만 뗀다.
	Spectators map[*DVClient]bool

	// LastReact 좌석별 마지막 리액션 발신 시각 (레이트리밋 장부)
	LastReact map[int]time.Time
}

type DVHub struct {
	// 등록된 클라이언트
	clients map[*DVClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*dvRoom

	// 글로벌 로비. 시작 전 공용 방은 하나뿐이다.
	lobby *dvRoom

	// 사설 방 (초대 코드 → 시작 전 방). 허브 고루틴에서만 접근한다.
	// 시작하면 privateLobbies 에서 activeCodes 로 옮긴다.
	privateLobbies map[string]*dvRoom

	// 진행 중 사설 방 (초대 코드 → gameID). 시작 후에도 코드로 방을 찾아
	// 관전 입장시키는 근거다. finishIfOver 에서 해제한다 (코드 재사용 복귀).
	activeCodes map[string]string

	// 클라이언트 등록
	register chan *DVClient

	// 클라이언트 등록 해제
	unregister chan *DVClient

	// 게임 메시지
	gameMessage chan DVGameMessage

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*DVClient]

	// 셔플·선공 결정용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type DVGameMessage struct {
	Client  *DVClient
	Message DVMessage
}

func NewDVHub() *DVHub {
	return &DVHub{
		register:       make(chan *DVClient),
		unregister:     make(chan *DVClient),
		clients:        make(map[*DVClient]bool),
		rooms:          make(map[string]*dvRoom),
		privateLobbies: make(map[string]*dvRoom),
		activeCodes:    make(map[string]string),
		gameMessage:    make(chan DVGameMessage),
		sessionManager: newSessionManager[*DVClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
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
	// 관전자는 어떤 행동도 할 수 없다 (보기 전용 — 리액션도 좌석 보유자만)
	if h.isSpectator(gm.Client) {
		h.sendError(gm.Client, spectatorDeniedMsg)
		return
	}
	switch gm.Message.Type {
	case DVMsgJoinLobby:
		h.handleJoinLobby(gm.Client, gm.Message)
	case DVMsgReact:
		h.handleReact(gm.Client, gm.Message)
	case DVMsgLeaveLobby:
		h.handleLeaveLobby(gm.Client)
	case DVMsgStartGame:
		h.handleStartGame(gm.Client)
	case DVMsgRejoinGame:
		h.handleRejoin(gm.Client, gm.Message)
	case DVMsgPlaceJoker:
		h.handlePlaceJoker(gm.Client, gm.Message)
	case DVMsgTakeInitial:
		h.handleTakeInitial(gm.Client, gm.Message)
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

	// 이미 시작된 사설 방의 코드로 들어오면 에러 대신 관전자로 입장시킨다
	if gameID, ok := h.activeCodes[normalizeRoomCode(payload.Room)]; ok {
		h.addSpectator(h.rooms[gameID], client, payload.PlayerName)
		return
	}

	room := h.lobbyRoomFor(payload.Room)
	seat, err := room.Game.AddPlayer(payload.PlayerName)
	if err != nil {
		// 사설 방이 가득 차 못 앉는 경우도 관전 진입 (공용 로비는 기존 에러)
		if room.Code != "" {
			h.addSpectator(room, client, payload.PlayerName)
			return
		}
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
	if room.Code == "" { // 사설 방 입장은 조용히 (공용 로비만 알림)
		notify("다빈치코드 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), len(room.Game.Players), DVMaxPlayers))
	}

	h.broadcastLobby(room)

	// 4명이 차면 자동 시작
	if len(room.Game.Players) == DVMaxPlayers {
		h.startGame(room)
	}
}

func (h *DVHub) handleLeaveLobby(client *DVClient) {
	room := h.waitingRoomOf(client)
	if room == nil {
		return
	}
	h.removeFromLobby(room, client)
	h.sendToClient(client, DVMessage{Type: DVMsgSessionExpired})
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다.
// 생략/빈 문자열은 기존 공용 로비 경로 그대로, "NEW"는 새 코드 발급,
// 그 외 코드는 해당 사설 방 (없으면 그 코드로 관대하게 새로 생성).
func (h *DVHub) lobbyRoomFor(roomField string) *dvRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewDVGame(uuid.New().String())
			h.lobby = &dvRoom{Game: game, Clients: map[int]*DVClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[DV] Created lobby game %s", game.ID)
		}
		return h.lobby
	}
	if code == roomCodeNew {
		taken := takenCodes(h.privateLobbies)
		for c := range h.activeCodes { // 진행 중 방의 코드도 재사용 금지
			taken[c] = true
		}
		code = generateRoomCode(h.rng, taken)
	}
	room := h.privateLobbies[code]
	if room == nil {
		game := NewDVGame(uuid.New().String())
		room = &dvRoom{Game: game, Clients: map[int]*DVClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[DV] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// ==================== 관전 / 리액션 ====================

// addSpectator 진행 중 사설 방에 관전자로 등록한다.
// 좌석·세션 없음 — 재접속 미지원, 끊기면 다시 코드로 들어온다.
func (h *DVHub) addSpectator(room *dvRoom, client *DVClient, name string) {
	if len(room.Spectators) >= maxSpectators {
		h.sendError(client, spectatorFullMsg)
		return
	}
	if room.Spectators == nil {
		room.Spectators = map[*DVClient]bool{}
	}
	client.Name = name
	client.GameID = room.Game.ID
	room.Spectators[client] = true

	log.Printf("[다빈치][관전] game=%s | %s 관전 입장 (%d명)",
		room.Game.ID, displayName(name), len(room.Spectators))

	h.sendToClient(client, DVMessage{
		Type:    DVMsgSpectateJoined,
		Payload: map[string]interface{}{"gameId": room.Game.ID, "roomCode": room.Code},
	})
	// 전원(관전자 포함)에게 관전자 수가 반영된 스냅샷 — 신규 관전자의 첫 스냅샷 겸용
	h.broadcastState(room)
}

// isSpectator 관전자 연결인지 (좌석 보유자·미입장 연결은 false)
func (h *DVHub) isSpectator(client *DVClient) bool {
	if client.GameID == "" {
		return false
	}
	room := h.rooms[client.GameID]
	return room != nil && room.Spectators[client]
}

// handleReact 리액션 이모지 — 좌석 보유자만, 로비 대기 중에도 허용.
// 화이트리스트 외·레이트리밋 초과는 조용히 무시한다 (상태 저장 없음).
func (h *DVHub) handleReact(client *DVClient, msg DVMessage) {
	room := h.rooms[client.GameID]
	if room == nil || client.Seat < 0 {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload DVReactPayload
	json.Unmarshal(payloadBytes, &payload)

	if !reactAllowed(payload.Emoji) {
		return
	}
	if room.LastReact == nil {
		room.LastReact = map[int]time.Time{}
	}
	if !reactPass(room.LastReact, client.Seat, time.Now()) {
		return
	}
	seat := client.Seat
	h.broadcastEvent(room, DVEventPayload{Kind: "react", Seat: &seat, Name: client.Name, Message: payload.Emoji})
}

// waitingRoomOf 클라이언트가 속한 시작 전 방 (공용 로비 또는 사설 방)
func (h *DVHub) waitingRoomOf(client *DVClient) *dvRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Ready {
		return nil
	}
	return room
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

	h.drop(client.SessionID)
	client.GameID = ""
	client.Seat = -1

	log.Printf("[다빈치][퇴장] game=%s | %s 로비 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), len(room.Game.Players), DVMaxPlayers)

	if len(room.Game.Players) == 0 {
		delete(h.rooms, room.Game.ID)
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			h.lobby = nil
		}
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
				RoomCode:  room.Code,
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
	room := h.waitingRoomOf(client)
	if room == nil {
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
	if room.Code != "" {
		// 시작한 사설 방의 코드는 진행 중 장부로 옮긴다 (관전 입장 근거)
		delete(h.privateLobbies, room.Code)
		h.activeCodes[room.Code] = room.Game.ID
	} else {
		h.lobby = nil // 시작한 방은 로비에서 떼어낸다
	}

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
	// 조커 보유는 비밀 정보이므로 배치 이벤트를 알리지 않는다
	h.broadcastState(room)
	h.finishIfOver(room, "last_standing")
}

func (h *DVHub) handleTakeInitial(client *DVClient, msg DVMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload DVTakeInitialPayload
	json.Unmarshal(payloadBytes, &payload)

	if _, err := room.Game.TakeInitial(client.Seat, payload.Color); err != nil {
		h.sendError(client, err.Error())
		return
	}
	// 시작 타일은 인원×장수만큼 연달아 일어나므로 이벤트 토스트 없이
	// 상태 스냅샷만 갱신한다
	h.broadcastState(room)
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
// 모드가 아니면 값을 감춘다. 남의 비공개 타일은 실제 타일 ID 대신
// 위치 기반 가짜 ID 를 줘서 클라이언트가 특정 타일(조커 등)을 상태
// 스냅샷 사이에서 추적하지 못하게 한다.
func maskDVTile(tile DVTile, ownerSeat, viewerSeat, index int, revealAll bool) DVTileView {
	if tile.Revealed || ownerSeat == viewerSeat || revealAll {
		return DVTileView{
			ID:       tile.ID,
			Color:    tile.Color,
			Revealed: tile.Revealed,
			Value:    dvIntPtr(tile.Value),
			Joker:    dvBoolPtr(tile.Joker),
		}
	}
	return DVTileView{ID: ownerSeat*100 + index, Color: tile.Color, Revealed: false}
}

// buildDVPlayers 좌석별 플레이어 뷰 목록. 셋업(시작 타일·조커 배치) 중에는
// 남의 줄 배치를 아예 감춘다 — 배치 과정을 지켜보면 어느 타일이 조커인지
// 위치까지 새기 때문이다. 원작처럼 셋업이 끝나야 상대 줄이 보인다.
func (h *DVHub) buildDVPlayers(room *dvRoom, viewerSeat int, revealAll bool) []DVPlayerView {
	game := room.Game
	setup := game.Phase == DVPhaseInitialDraw || game.Phase == DVPhaseJokerSetup

	views := []DVPlayerView{}
	for _, p := range game.Players {
		c := room.Clients[p.Seat]
		tiles := []DVTileView{}
		for idx, tile := range p.Tiles {
			if setup && p.Seat != viewerSeat && !revealAll {
				// 색도 감춘 중립 뒷면 (개수만 보인다)
				tiles = append(tiles, DVTileView{ID: p.Seat*100 + idx, Revealed: false})
				continue
			}
			tiles = append(tiles, maskDVTile(tile, p.Seat, viewerSeat, idx, revealAll))
		}
		views = append(views, DVPlayerView{
			Seat:             p.Seat,
			Name:             p.Name,
			Connected:        c != nil && c.Connected,
			Eliminated:       p.Eliminated,
			InitialRemaining: room.Game.InitialRemaining[p.Seat],
			Tiles:            tiles,
		})
	}
	return views
}

// maskDVPhase 수신자 관점의 단계. 조커 관련 단계는 조커 보유가 새는
// 정보이므로 당사자가 아니면 직전 단계로 위장한다.
func maskDVPhase(game *DVGame, viewerSeat int) DVPhase {
	switch game.Phase {
	case DVPhaseJokerSetup:
		// 조커를 배치할 사람만 실제 단계를 본다. 나머지에게는 아직
		// 시작 타일 단계인 것처럼 보인다 (본인 몫은 이미 0이라 대기 문구).
		if len(game.PendingJokerTiles[viewerSeat]) == 0 {
			return DVPhaseInitialDraw
		}
	case DVPhasePlaceDrawnJoker:
		// 뽑은 타일이 조커라는 사실 자체가 비밀이다. 남에게는 직전
		// 단계(추리 중 / 계속 여부 고민 중)가 이어지는 것처럼 보인다.
		if viewerSeat != game.CurrentSeat {
			if game.DrawnJokerRevealed {
				return DVPhaseGuess
			}
			return DVPhaseContinueChoice
		}
	}
	return game.Phase
}

// buildDVState 개인화 게임 스냅샷. 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
func (h *DVHub) buildDVState(room *dvRoom, viewerSeat int) DVGameStatePayload {
	game := room.Game

	var yourPending []DVTileView
	for idx, tile := range game.PendingJokerTiles[viewerSeat] {
		yourPending = append(yourPending, maskDVTile(tile, viewerSeat, viewerSeat, idx, false))
	}

	var drawn *DVTileView
	if game.DrawnTile != nil {
		// 값(조커 여부 포함)은 뽑은 본인에게만 보인다. 실패로 공개될
		// 조커도 줄에 놓이기 전까지는 남에게 알리지 않는다.
		view := maskDVTile(*game.DrawnTile, game.CurrentSeat, viewerSeat, 0, false)
		if viewerSeat == game.CurrentSeat {
			view.Revealed = game.DrawnJokerRevealed
		}
		drawn = &view
	}

	return DVGameStatePayload{
		GameID:            game.ID,
		RoomCode:          room.Code,
		YourSeat:          viewerSeat,
		Phase:             maskDVPhase(game, viewerSeat),
		CurrentSeat:       game.CurrentSeat,
		DeckCount:         len(game.Deck),
		DeckBlackCount:    game.DeckCountByColor(DVBlack),
		DeckWhiteCount:    game.DeckCountByColor(DVWhite),
		PlayerCount:       len(game.Players),
		YourPendingJokers: yourPending,
		DrawnTile:         drawn,
		Players:           h.buildDVPlayers(room, viewerSeat, false),
		Spectators:        len(room.Spectators),
	}
}

// broadcastState 좌석마다 개인화 스냅샷을 만들어 보낸다.
// 관전자에게는 공개 정보만 담긴 스냅샷(viewerSeat -1)이 간다.
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
	if len(room.Spectators) > 0 {
		msg := DVMessage{Type: DVMsgGameState, Payload: h.buildDVState(room, -1)}
		for c := range room.Spectators {
			h.sendToClient(c, msg)
		}
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

	dvNames := []string{}
	for _, p := range game.Players {
		dvNames = append(dvNames, displayName(p.Name))
	}
	RecordMatch(MatchRecord{
		Game:     "davinci",
		Players:  strings.Join(dvNames, " vs "),
		Winner:   displayName(winnerName),
		Reason:   reason,
		Duration: matchSeconds(game.StartedAt),
	})

	h.clearGameSessions(room)
	delete(h.rooms, game.ID)
	if room.Code != "" {
		delete(h.activeCodes, room.Code) // 코드 재사용 복귀
	}
}

// ==================== 재접속 / 연결 끊김 ====================

func (h *DVHub) handleDisconnect(client *DVClient) {
	// 관전자 연결 종료 — 세션·유예 없이 목록에서만 뗀다
	if room := h.rooms[client.GameID]; room != nil && room.Spectators[client] {
		delete(room.Spectators, client)
		h.broadcastState(room) // 관전자 수 갱신
		return
	}
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

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 안에 재접속하지 않은 플레이어는 몰수 처리하고
// 게임은 남은 인원으로 계속한다
func (h *DVHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

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
		h.drop(payload.SessionID)
		h.sendToClient(client, DVMessage{Type: DVMsgSessionExpired})
		return
	}

	// 유예 타이머 취소
	h.cancelGrace(payload.SessionID)

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
		h.drop(c.SessionID)
	}
}

// ==================== 전송 ====================

func (h *DVHub) sendError(client *DVClient, message string) {
	h.sendToClient(client, DVMessage{Type: DVMsgError, Payload: DVErrorPayload{Message: message}})
}

func (h *DVHub) sendToClient(client *DVClient, message DVMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[DV] ")
}

func (h *DVHub) broadcastToRoom(room *dvRoom, message DVMessage) {
	for _, c := range room.Clients {
		if c != nil {
			h.sendToClient(c, message)
		}
	}
	for c := range room.Spectators { // 이벤트·종료 발표는 관전자에게도 간다
		h.sendToClient(c, message)
	}
}
