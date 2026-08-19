package server

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"time"

	"github.com/google/uuid"
)

// tcHandEndDelay hand_end 에서 다음 핸드로 자동 진행하기까지의 시간
// (전원 tc_ready 면 즉시, 테스트에서는 짧게 낮춘다)
var tcHandEndDelay = 5 * time.Second

// tcRoom 게임(순수 상태)과 좌석별 연결의 매핑
type tcRoom struct {
	Game    *TCGame
	Clients map[int]*TCClient // seat → client

	// Code 사설 방 초대 코드 (공용 로비는 "")
	Code string

	// HandEndTimer hand_end 자동 진행 타이머 (허브 채널 경유로만 상태를 만진다)
	HandEndTimer *time.Timer

	// AfkTimer 접속 유지 AFK 구제 타이머 — 상태가 바뀔 때마다 리셋되고,
	// 발화하면 진행을 막는 좌석의 수를 봇 두뇌로 1회 자동 실행한다.
	AfkTimer *time.Timer
	// AfkSeq 상태 변경 일련번호 (뒤늦은 발화 무시용)
	AfkSeq int

	// Spectators 관전자 연결 (사설 방 코드로만 진입, 상한 maxSpectators).
	// 좌석·세션이 없어 재접속을 지원하지 않는다 — 끊기면 목록에서만 뗀다.
	Spectators map[*TCClient]bool

	// LastReact 좌석별 마지막 리액션 발신 시각 (레이트리밋 장부)
	LastReact map[int]time.Time
}

// tcAfkSignal AFK 타이머 발화 표식
type tcAfkSignal struct {
	GameID string
	Seq    int
}

// tcAfkTimeout 접속 유지 AFK 의 자동 진행 대기 시간.
// 그랜드·교환·플레이 턴·용 전달 모두에 적용 — 한 명이 4인 게임을
// 영구히 볼모로 잡지 않게 한다 (끊긴 좌석의 90초 유예 봇 대체와 별개).
var tcAfkTimeout = 90 * time.Second

// tcHandEndSignal 어느 게임의 몇 번째 핸드 종료 타이머인지 (뒤늦은 발화 무시용)
type tcHandEndSignal struct {
	GameID string
	HandNo int
}

type TCHub struct {
	// 등록된 클라이언트
	clients map[*TCClient]bool

	// 진행/대기 중인 방 (gameID → room)
	rooms map[string]*tcRoom

	// 글로벌 로비. 시작 전 공용 방은 하나뿐이다.
	lobby *tcRoom

	// 사설 방 (초대 코드 → 시작 전 방). 허브 고루틴에서만 접근한다.
	// 시작하면 privateLobbies 에서 activeCodes 로 옮긴다.
	privateLobbies map[string]*tcRoom

	// 진행 중 사설 방 (초대 코드 → gameID). 시작 후에도 코드로 방을 찾아
	// 관전 입장시키는 근거다. finishGame 에서 해제한다 (코드 재사용 복귀).
	activeCodes map[string]string

	// 클라이언트 등록
	register chan *TCClient

	// 클라이언트 등록 해제
	unregister chan *TCClient

	// 게임 메시지
	gameMessage chan TCGameMessage

	// hand_end 자동 진행 알림
	handEndFired chan tcHandEndSignal
	afkFired     chan tcAfkSignal

	// 세션·유예 타이머 장부 (sessions/grace/graceExpired 필드 승격)
	sessionManager[*TCClient]

	// 셔플용 난수원 (허브 고루틴에서만 사용)
	rng *rand.Rand
}

type TCGameMessage struct {
	Client  *TCClient
	Message TCMessage
}

func NewTCHub() *TCHub {
	return &TCHub{
		register:       make(chan *TCClient),
		unregister:     make(chan *TCClient),
		clients:        make(map[*TCClient]bool),
		rooms:          make(map[string]*tcRoom),
		privateLobbies: make(map[string]*tcRoom),
		activeCodes:    make(map[string]string),
		gameMessage:    make(chan TCGameMessage),
		handEndFired:   make(chan tcHandEndSignal, 8),
		afkFired:       make(chan tcAfkSignal, 8),
		sessionManager: newSessionManager[*TCClient](),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *TCHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[TC] Client registered: %s", client.ID)

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if client.Connected {
					client.Connected = false
					close(client.Send)
				}
				h.handleDisconnect(client)
				log.Printf("[TC] Client unregistered: %s", client.ID)
			}

		case sessionID := <-h.graceExpired:
			h.handleGraceExpired(sessionID)

		case sig := <-h.handEndFired:
			h.handleHandEndFired(sig)

		case sig := <-h.afkFired:
			h.handleAfkFired(sig)

		case message := <-h.gameMessage:
			h.handleGameMessage(message)
		}
	}
}

func (h *TCHub) handleGameMessage(gm TCGameMessage) {
	// 관전자는 어떤 행동도 할 수 없다 (보기 전용 — 리액션도 좌석 보유자만)
	if h.isSpectator(gm.Client) {
		h.sendError(gm.Client, spectatorDeniedMsg)
		return
	}
	switch gm.Message.Type {
	case TCMsgJoinGame:
		h.handleJoinGame(gm.Client, gm.Message)
	case TCMsgReact:
		h.handleReact(gm.Client, gm.Message)
	case TCMsgSetTarget:
		h.handleSetTarget(gm.Client, gm.Message)
	case TCMsgFillBots:
		h.handleFillBots(gm.Client)
	case TCMsgCallGrand:
		h.handleCallGrand(gm.Client, gm.Message)
	case TCMsgCallTichu:
		h.handleCallTichu(gm.Client)
	case TCMsgExchange:
		h.handleExchange(gm.Client, gm.Message)
	case TCMsgPlay:
		h.handlePlay(gm.Client, gm.Message)
	case TCMsgPass:
		h.handlePass(gm.Client)
	case TCMsgDragonGive:
		h.handleDragonGive(gm.Client, gm.Message)
	case TCMsgReady:
		h.handleReady(gm.Client)
	case TCMsgRejoin:
		h.handleRejoin(gm.Client, gm.Message)
	}
}

// ==================== 대기실 ====================

func (h *TCHub) handleJoinGame(client *TCClient, msg TCMessage) {
	// 버튼 연타 등으로 같은 연결이 두 번 입장하면 유령 좌석이 생기므로
	// 이미 세션이 있는 연결의 재입장은 무시한다
	if client.SessionID != "" {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload TCJoinGamePayload
	json.Unmarshal(payloadBytes, &payload)

	// 이미 시작된 사설 방의 코드로 들어오면 에러 대신 관전자로 입장시킨다
	if gameID, ok := h.activeCodes[normalizeRoomCode(payload.Room)]; ok {
		h.addSpectator(h.rooms[gameID], client, payload.Name)
		return
	}

	room := h.lobbyRoomFor(payload.Room)
	seat, err := room.Game.AddPlayer(payload.Name)
	if err != nil {
		// 사설 방이 가득 차 못 앉는 경우도 관전 진입 (공용 로비는 기존 에러)
		if room.Code != "" {
			h.addSpectator(room, client, payload.Name)
			return
		}
		h.sendError(client, err.Error())
		return
	}

	client.Name = payload.Name
	client.SessionID = uuid.New().String()
	client.GameID = room.Game.ID
	client.Seat = seat
	h.sessions[client.SessionID] = client
	room.Clients[seat] = client

	log.Printf("[티츄][입장] game=%s | seat%d=%s 로비 입장 (%d/%d)",
		room.Game.ID, seat, displayName(client.Name), room.Game.PlayerCount, TCSeats)
	if room.Code == "" { // 사설 방은 알림·로비 현황판 미반영 (초대 링크로만 접근)
		notify("티츄 참가", fmt.Sprintf("%s 입장 (%d/%d)",
			displayName(client.Name), room.Game.PlayerCount, TCSeats))
		lobbySetWaiting("tichu", true)
	}

	h.sendToClient(client, TCMessage{
		Type: TCMsgPlayerJoined,
		Payload: TCPlayerJoinedPayload{
			GameID:    room.Game.ID,
			SessionID: client.SessionID,
			YourSeat:  seat,
			Name:      client.Name,
			RoomCode:  room.Code,
		},
	})
	h.broadcastEvent(room, TCEventPayload{Kind: "joined", Seat: &seat,
		Message: fmt.Sprintf("%s 입장", client.Name)})
	h.broadcastState(room)

	if room.Game.PlayerCount == TCSeats {
		h.startGame(room)
	}
}

// lobbyRoomFor join payload 의 room 값으로 입장할 시작 전 방을 찾거나 만든다.
// 생략/빈 문자열은 기존 공용 로비 경로 그대로, "NEW"는 새 코드 발급,
// 그 외 코드는 해당 사설 방 (없으면 그 코드로 관대하게 새로 생성).
func (h *TCHub) lobbyRoomFor(roomField string) *tcRoom {
	code := normalizeRoomCode(roomField)
	if code == "" {
		if h.lobby == nil {
			game := NewTCGame(uuid.New().String())
			h.lobby = &tcRoom{Game: game, Clients: map[int]*TCClient{}}
			h.rooms[game.ID] = h.lobby
			log.Printf("[TC] Created lobby game %s", game.ID)
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
		game := NewTCGame(uuid.New().String())
		room = &tcRoom{Game: game, Clients: map[int]*TCClient{}, Code: code}
		h.privateLobbies[code] = room
		h.rooms[game.ID] = room
		log.Printf("[TC] Created private room %s (code=%s)", game.ID, code)
	}
	return room
}

// ==================== 관전 / 리액션 ====================

// addSpectator 진행 중(또는 가득 찬) 사설 방에 관전자로 등록한다.
// 좌석·세션 없음 — 재접속 미지원, 끊기면 다시 코드로 들어온다.
func (h *TCHub) addSpectator(room *tcRoom, client *TCClient, name string) {
	if len(room.Spectators) >= maxSpectators {
		h.sendError(client, spectatorFullMsg)
		return
	}
	if room.Spectators == nil {
		room.Spectators = map[*TCClient]bool{}
	}
	client.Name = name
	client.GameID = room.Game.ID
	room.Spectators[client] = true

	log.Printf("[티츄][관전] game=%s | %s 관전 입장 (%d명)",
		room.Game.ID, displayName(name), len(room.Spectators))

	h.sendToClient(client, TCMessage{
		Type:    TCMsgSpectateJoined,
		Payload: map[string]interface{}{"gameId": room.Game.ID, "roomCode": room.Code},
	})
	// 전원(관전자 포함)에게 관전자 수가 반영된 스냅샷 — 신규 관전자의 첫 스냅샷 겸용
	h.broadcastState(room)
}

// isSpectator 관전자 연결인지 (좌석 보유자·미입장 연결은 false)
func (h *TCHub) isSpectator(client *TCClient) bool {
	if client.GameID == "" {
		return false
	}
	room := h.rooms[client.GameID]
	return room != nil && room.Spectators[client]
}

// handleReact 리액션 이모지 — 좌석 보유자만, waiting 중에도 허용.
// 화이트리스트 외·레이트리밋 초과는 조용히 무시한다 (상태 저장 없음).
func (h *TCHub) handleReact(client *TCClient, msg TCMessage) {
	room := h.rooms[client.GameID]
	if room == nil || client.Seat < 0 {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload TCReactPayload
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
	h.broadcastEvent(room, TCEventPayload{Kind: "react", Seat: &seat, Name: client.Name, Message: payload.Emoji})
}

// waitingRoomOf 클라이언트가 속한 시작 전 방 (공용 로비 또는 사설 방)
func (h *TCHub) waitingRoomOf(client *TCClient) *tcRoom {
	room := h.rooms[client.GameID]
	if room == nil || room.Game.Started {
		return nil
	}
	return room
}

func (h *TCHub) handleSetTarget(client *TCClient, msg TCMessage) {
	room := h.waitingRoomOf(client)
	if room == nil {
		h.sendError(client, "대기실을 찾을 수 없습니다")
		return
	}
	if client.Seat != 0 {
		h.sendError(client, "호스트만 목표점수를 바꿀 수 있습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload TCSetTargetPayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.SetTarget(payload.Target); err != nil {
		h.sendError(client, err.Error())
		return
	}
	h.broadcastState(room)
}

func (h *TCHub) handleFillBots(client *TCClient) {
	room := h.waitingRoomOf(client)
	if room == nil {
		h.sendError(client, "대기실을 찾을 수 없습니다")
		return
	}
	if client.Seat != 0 {
		h.sendError(client, "호스트만 봇을 채울 수 있습니다")
		return
	}
	botNo := 1
	for room.Game.PlayerCount < TCSeats {
		if !h.spawnTCBot(room, fmt.Sprintf("%s%d", botName, botNo)) {
			break
		}
		botNo++
	}
	if room.Game.PlayerCount == TCSeats {
		h.startGame(room)
	}
}

func (h *TCHub) startGame(room *tcRoom) {
	if room.Code != "" {
		// 시작한 사설 방의 코드는 진행 중 장부로 옮긴다 (관전 입장 근거)
		delete(h.privateLobbies, room.Code)
		h.activeCodes[room.Code] = room.Game.ID
	} else {
		h.lobby = nil // 시작한 방은 로비에서 떼어낸다
		lobbySetWaiting("tichu", false)
	}
	room.Game.StartHand(h.rng)

	names := []string{}
	for s := 0; s < TCSeats; s++ {
		names = append(names, displayName(room.Game.Names[s]))
	}
	log.Printf("[티츄][경기시작] game=%s | 목표=%d | %v",
		room.Game.ID, room.Game.Target, names)
	notify("티츄 게임 시작", fmt.Sprintf("%s·%s vs %s·%s (목표 %d점)",
		names[0], names[2], names[1], names[3], room.Game.Target))

	h.broadcastState(room)
}

// removeFromLobby 대기실에서 좌석을 비우고 남은 좌석을 앞으로 당긴다
func (h *TCHub) removeFromLobby(room *tcRoom, client *TCClient) {
	oldSeat := client.Seat
	room.Game.RemovePlayer(oldSeat)
	delete(room.Clients, oldSeat)

	// 좌석 압축: oldSeat 뒤의 클라이언트를 한 칸씩 당긴다
	rebuilt := map[int]*TCClient{}
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

	log.Printf("[티츄][퇴장] game=%s | %s 로비 퇴장 (%d/%d)",
		room.Game.ID, displayName(client.Name), room.Game.PlayerCount, TCSeats)

	if room.Game.PlayerCount == 0 {
		delete(h.rooms, room.Game.ID)
		if room.Code != "" {
			delete(h.privateLobbies, room.Code)
		} else {
			h.lobby = nil
			lobbySetWaiting("tichu", false)
		}
		return
	}
	seat := oldSeat
	h.broadcastEvent(room, TCEventPayload{Kind: "left", Seat: &seat,
		Message: fmt.Sprintf("%s 퇴장", client.Name)})
	h.broadcastState(room)
}

// ==================== 게임 액션 ====================

// roomOf 클라이언트가 속한 진행 중인 방
func (h *TCHub) roomOf(client *TCClient) *tcRoom {
	room := h.rooms[client.GameID]
	if room == nil || !room.Game.Started {
		return nil
	}
	return room
}

func (h *TCHub) handleCallGrand(client *TCClient, msg TCMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload TCCallGrandPayload
	json.Unmarshal(payloadBytes, &payload)

	if _, err := room.Game.CallGrand(client.Seat, payload.Call); err != nil {
		h.sendError(client, err.Error())
		return
	}
	if payload.Call {
		seat := client.Seat
		h.broadcastEvent(room, TCEventPayload{Kind: "grand", Seat: &seat,
			Message: fmt.Sprintf("%s 그랜드 티츄 선언!", client.Name)})
	}
	h.broadcastState(room)
}

func (h *TCHub) handleCallTichu(client *TCClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	if err := room.Game.CallTichu(client.Seat); err != nil {
		h.sendError(client, err.Error())
		return
	}
	seat := client.Seat
	h.broadcastEvent(room, TCEventPayload{Kind: "tichu", Seat: &seat,
		Message: fmt.Sprintf("%s 티츄 선언!", client.Name)})
	h.broadcastState(room)
}

func (h *TCHub) handleExchange(client *TCClient, msg TCMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload TCExchangePayload
	json.Unmarshal(payloadBytes, &payload)

	if _, err := room.Game.SubmitExchange(client.Seat, payload); err != nil {
		h.sendError(client, err.Error())
		return
	}
	seat := client.Seat
	h.broadcastEvent(room, TCEventPayload{Kind: "exchanged", Seat: &seat,
		Message: fmt.Sprintf("%s 교환 완료", client.Name)})
	h.broadcastState(room)
}

func (h *TCHub) handlePlay(client *TCClient, msg TCMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload TCPlayPayload
	json.Unmarshal(payloadBytes, &payload)

	res, err := room.Game.Play(client.Seat, payload.Cards, payload.Wish)
	if err != nil {
		h.sendError(client, err.Error())
		return
	}

	seat := client.Seat
	kind := "play"
	message := fmt.Sprintf("%s: %s", client.Name, tcComboLabel(res.Combo.Kind))
	if tcIsBombKind(res.Combo.Kind) {
		kind = "bomb"
		message = fmt.Sprintf("%s 폭탄!", client.Name)
	} else if res.Dog {
		kind = "dog"
		message = fmt.Sprintf("%s 개 — 파트너에게 리드", client.Name)
	}
	h.broadcastEvent(room, TCEventPayload{Kind: kind, Seat: &seat, Message: message})

	if res.WishSet > 0 {
		h.broadcastEvent(room, TCEventPayload{Kind: "wish", Seat: &seat,
			Message: fmt.Sprintf("소원: %s", tcRankLabel(res.WishSet))})
	}
	if res.WishDone {
		h.broadcastEvent(room, TCEventPayload{Kind: "wish_done", Seat: &seat,
			Message: "소원 이행"})
	}
	if res.PlayerOut {
		h.broadcastEvent(room, TCEventPayload{Kind: "player_out", Seat: &seat,
			Message: fmt.Sprintf("%s %d위로 아웃", client.Name, res.OutRank)})
	}
	if res.HandEnded {
		result := room.Game.HandResult
		h.broadcastEvent(room, TCEventPayload{Kind: "hand_end", Message: result.Detail})
		log.Printf("[티츄][핸드종료] game=%s | 핸드%d | %s | 누적 02=%d 13=%d",
			room.Game.ID, room.Game.HandNo, result.Detail, room.Game.Score02, room.Game.Score13)
	}

	h.broadcastState(room)

	if res.GameOver {
		h.finishGame(room)
	} else if res.HandEnded {
		h.scheduleHandEnd(room)
	}
}

func (h *TCHub) handlePass(client *TCClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	res, err := room.Game.Pass(client.Seat)
	if err != nil {
		h.sendError(client, err.Error())
		return
	}
	seat := client.Seat
	h.broadcastEvent(room, TCEventPayload{Kind: "pass", Seat: &seat,
		Message: fmt.Sprintf("%s 패스", client.Name)})
	if res.TrickWon {
		winner := res.WinnerSeat
		h.broadcastEvent(room, TCEventPayload{Kind: "trick_won", Seat: &winner,
			Message: fmt.Sprintf("%s 트릭 획득", room.Game.Names[winner])})
	}
	h.broadcastState(room)
}

func (h *TCHub) handleDragonGive(client *TCClient, msg TCMessage) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload TCDragonGivePayload
	json.Unmarshal(payloadBytes, &payload)

	if err := room.Game.DragonGive(client.Seat, payload.Seat); err != nil {
		h.sendError(client, err.Error())
		return
	}
	target := payload.Seat
	h.broadcastEvent(room, TCEventPayload{Kind: "dragon_given", Seat: &target,
		Message: fmt.Sprintf("%s → %s 용 트릭 전달", client.Name, room.Game.Names[target])})
	h.broadcastState(room)
}

func (h *TCHub) handleReady(client *TCClient) {
	room := h.roomOf(client)
	if room == nil {
		h.sendError(client, "게임을 찾을 수 없습니다")
		return
	}
	allReady, changed := room.Game.SetReady(client.Seat)
	if !changed {
		return // 중복 ready 는 조용히 무시 (봇 재전송 대비)
	}
	h.broadcastState(room)
	if allReady {
		h.startNextHand(room)
	}
}

// ==================== hand_end 자동 진행 ====================

// scheduleHandEnd hand_end 진입 시 자동 진행 타이머를 건다.
// 발화는 허브 채널을 경유하므로 허브 밖에서 상태를 만지지 않는다.
func (h *TCHub) scheduleHandEnd(room *tcRoom) {
	sig := tcHandEndSignal{GameID: room.Game.ID, HandNo: room.Game.HandNo}
	room.HandEndTimer = time.AfterFunc(tcHandEndDelay, func() {
		h.handEndFired <- sig
	})
}

func (h *TCHub) handleHandEndFired(sig tcHandEndSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.Game.Phase != TCPhaseHandEnd || room.Game.HandNo != sig.HandNo {
		return
	}
	h.startNextHand(room)
}

func (h *TCHub) startNextHand(room *tcRoom) {
	if room.HandEndTimer != nil {
		room.HandEndTimer.Stop()
		room.HandEndTimer = nil
	}
	room.Game.StartHand(h.rng)
	log.Printf("[티츄][핸드시작] game=%s | 핸드%d", room.Game.ID, room.Game.HandNo)
	h.broadcastState(room)
}

// ==================== 상태 뷰 (은닉의 핵심) ====================

// buildTCState 개인화 게임 스냅샷 — 남의 손패는 개수만 싣는다.
// 재접속 복원과 일반 갱신이 같은 경로를 쓴다.
func (h *TCHub) buildTCState(room *tcRoom, viewerSeat int) TCGameStatePayload {
	g := room.Game

	players := make([]TCPlayerView, TCSeats)
	for s := 0; s < TCSeats; s++ {
		c := room.Clients[s]
		handCount := len(g.Hands[s])
		if g.Phase == TCPhaseGrand {
			handCount = 8 // 나머지 6장은 그랜드 응답 후 배분
		} else if g.Phase == TCPhaseWaiting {
			handCount = 0
		}
		players[s] = TCPlayerView{
			Seat:      s,
			Name:      g.Names[s],
			Connected: c != nil && c.Connected,
			Bot:       c != nil && c.Bot,
			HandCount: handCount,
			Tichu:     g.Tichu[s],
			Out:       g.Out[s],
			OutRank:   g.OutRank[s],
			Ready:     g.Ready[s],
		}
	}

	// 관전자(viewerSeat -1)는 좌석 배열 인덱스 접근이 불가 — 손패는 빈 배열,
	// 좌석 개인화 플래그는 false 로 둔다 (공개 정보만 담긴 스냅샷)
	yourHand := []string{}
	if viewerSeat >= 0 {
		switch g.Phase {
		case TCPhaseWaiting:
		case TCPhaseGrand:
			yourHand = tcSortHand(g.Dealt[viewerSeat][:8])
		default:
			yourHand = tcSortHand(g.Hands[viewerSeat])
		}
	}

	trick := append([]TCTrickPlay{}, g.Trick...)
	var lastPlay *TCTrickPlay
	if len(trick) > 0 {
		lastPlay = &trick[len(trick)-1]
	}

	return TCGameStatePayload{
		GameID:            g.ID,
		RoomCode:          room.Code,
		Phase:             g.Phase,
		TargetScore:       g.Target,
		YourSeat:          viewerSeat,
		Players:           players,
		YourHand:          yourHand,
		Spectators:        len(room.Spectators),
		ExchangeDone:      viewerSeat >= 0 && g.ExchangeSub[viewerSeat] != nil,
		GrandAnswered:     viewerSeat >= 0 && g.GrandAnswered[viewerSeat],
		Scores:            TCScores{Team02: g.Score02, Team13: g.Score13},
		HandNo:            g.HandNo,
		CurrentTurn:       g.CurrentTurn,
		Trick:             trick,
		LastPlay:          lastPlay,
		WishRank:          g.WishRank,
		DragonPendingSeat: g.DragonPendingSeat,
		HandResult:        g.HandResult,
		WinnerTeam:        g.WinnerTeam,
	}
}

// broadcastState 좌석마다 개인화 스냅샷을 만들어 보낸다.
// 관전자에게는 공개 정보만 담긴 스냅샷(viewerSeat -1)이 간다.
func (h *TCHub) broadcastState(room *tcRoom) {
	for seat, c := range room.Clients {
		if c == nil {
			continue
		}
		h.sendToClient(c, TCMessage{
			Type:    TCMsgGameState,
			Payload: h.buildTCState(room, seat),
		})
	}
	if len(room.Spectators) > 0 {
		msg := TCMessage{Type: TCMsgGameState, Payload: h.buildTCState(room, -1)}
		for c := range room.Spectators {
			h.sendToClient(c, msg)
		}
	}
	h.resetAfkTimer(room)
}

// resetAfkTimer 상태가 바뀔 때마다 AFK 타이머를 다시 건다.
// 응답을 기다리는 단계(그랜드·교환·플레이)에서만 동작한다.
func (h *TCHub) resetAfkTimer(room *tcRoom) {
	room.AfkSeq++
	if room.AfkTimer != nil {
		room.AfkTimer.Stop()
		room.AfkTimer = nil
	}
	switch room.Game.Phase {
	case TCPhaseGrand, TCPhaseExchange, TCPhasePlay:
	default:
		return
	}
	sig := tcAfkSignal{GameID: room.Game.ID, Seq: room.AfkSeq}
	room.AfkTimer = time.AfterFunc(tcAfkTimeout, func() {
		h.afkFired <- sig
	})
}

// handleAfkFired AFK 타이머 발화 — 진행을 막는 사람 좌석의 수를
// 봇 두뇌(서버 검증과 같은 열거)로 1회 자동 실행한다. 좌석은 유지된다.
func (h *TCHub) handleAfkFired(sig tcAfkSignal) {
	room := h.rooms[sig.GameID]
	if room == nil || room.AfkSeq != sig.Seq {
		return
	}
	game := room.Game

	// 이 단계의 진행을 막고 있는 좌석들
	blocking := []int{}
	switch game.Phase {
	case TCPhaseGrand:
		for seat := 0; seat < TCSeats; seat++ {
			if !game.GrandAnswered[seat] {
				blocking = append(blocking, seat)
			}
		}
	case TCPhaseExchange:
		for seat := 0; seat < TCSeats; seat++ {
			if game.ExchangeSub[seat] == nil {
				blocking = append(blocking, seat)
			}
		}
	case TCPhasePlay:
		if game.DragonPendingSeat >= 0 {
			blocking = append(blocking, game.DragonPendingSeat)
		} else {
			blocking = append(blocking, game.CurrentTurn)
		}
	default:
		return
	}

	brain := newTCBrain()
	for _, seat := range blocking {
		client := room.Clients[seat]
		if client == nil || client.Bot {
			continue // 봇 좌석은 스스로 행동한다
		}
		state := h.buildTCState(room, seat)
		act := brain.decide(TCMessage{Type: TCMsgGameState, Payload: state})
		if act == nil {
			continue
		}
		log.Printf("[티츄][자동진행] game=%s | seat%d=%s 무응답 — %s 자동 실행",
			game.ID, seat, displayName(game.Names[seat]), act.Type)
		h.broadcastEvent(room, TCEventPayload{Kind: "afk", Seat: &seat,
			Message: fmt.Sprintf("%s님이 오래 응답하지 않아 자동으로 진행합니다", game.Names[seat])})
		h.handleGameMessage(TCGameMessage{Client: client, Message: *act})
	}
}

func (h *TCHub) broadcastEvent(room *tcRoom, event TCEventPayload) {
	h.broadcastToRoom(room, TCMessage{Type: TCMsgEvent, Payload: event})
}

// finishGame 목표점수 도달 — 결과 통지·전적 기록·방 정리
func (h *TCHub) finishGame(room *tcRoom) {
	g := room.Game
	team02 := displayName(g.Names[0]) + "·" + displayName(g.Names[2])
	team13 := displayName(g.Names[1]) + "·" + displayName(g.Names[3])
	winner, loser := team02, team13
	if g.WinnerTeam == "13" {
		winner, loser = team13, team02
	}

	h.broadcastToRoom(room, TCMessage{
		Type: TCMsgGameOver,
		Payload: TCGameOverPayload{
			WinnerTeam: g.WinnerTeam,
			Scores:     TCScores{Team02: g.Score02, Team13: g.Score13},
		},
	})

	log.Printf("[티츄][경기결과] game=%s | 승자=%s팀(%s) | 점수 02=%d 13=%d | 핸드=%d | 소요=%s",
		g.ID, g.WinnerTeam, winner, g.Score02, g.Score13, g.HandNo, matchDuration(g.StartedAt))

	RecordMatch(MatchRecord{
		Game:     "tichu",
		Players:  winner + " vs " + loser,
		Winner:   winner,
		Reason:   "target",
		Duration: matchSeconds(g.StartedAt),
		Bot:      tcRoomHasBot(room),
	})

	h.clearGameSessions(room)
	delete(h.rooms, g.ID)
	if room.Code != "" {
		delete(h.activeCodes, room.Code) // 코드 재사용 복귀
	}
}

// tcRoomHasBot 방에 연습봇이 있는지
func tcRoomHasBot(room *tcRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}

// ==================== 재접속 / 연결 끊김 / 봇 대체 ====================

func (h *TCHub) handleDisconnect(client *TCClient) {
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

	// 대기실: 유예 없이 즉시 자리를 비운다
	if !room.Game.Started {
		h.removeFromLobby(room, client)
		return
	}

	// 진행 중: 유예 시간 동안 재접속을 기다린다 (만료 시 봇 대체)
	log.Printf("[티츄][연결끊김] game=%s | seat%d=%s 재접속 대기 시작 (%.0f초)",
		room.Game.ID, client.Seat, displayName(client.Name), h.grace.Seconds())

	h.broadcastToRoom(room, TCMessage{
		Type: TCMsgOpponentDisconnected,
		Payload: TCOpponentDisconnectedPayload{
			Seat:         client.Seat,
			Name:         client.Name,
			GraceSeconds: int(h.grace.Seconds()),
		},
	})
	h.broadcastState(room)

	h.startGrace(client.SessionID)
}

// handleGraceExpired 유예 안에 재접속하지 않은 좌석은 봇으로 대체하고
// 게임은 계속한다 (세션은 폐기 — 재접속 불가)
func (h *TCHub) handleGraceExpired(sessionID string) {
	client, ok := h.expire(sessionID)
	if !ok {
		return
	}

	room := h.rooms[client.GameID]
	if room == nil || room.Game.Phase == TCPhaseGameOver {
		return
	}

	seat := client.Seat
	log.Printf("[티츄][봇대체] game=%s | seat%d=%s 유예 만료 → 봇 대체",
		room.Game.ID, seat, displayName(client.Name))

	bot := &TCClient{wsClient: newBotWSClient(), Hub: h, Seat: seat}
	bot.Name = client.Name // 좌석 이름은 유지 (표시는 bot 플래그로 구분)
	bot.GameID = room.Game.ID
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runTCBot(bot)

	h.broadcastEvent(room, TCEventPayload{Kind: "bot_takeover", Seat: &seat,
		Message: fmt.Sprintf("%s 자리를 봇이 이어받았습니다", client.Name)})
	h.broadcastState(room)
}

// handleRejoin 세션 ID로 기존 게임에 재접속
func (h *TCHub) handleRejoin(client *TCClient, msg TCMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload TCRejoinPayload
	json.Unmarshal(payloadBytes, &payload)

	old := h.sessions[payload.SessionID]
	if old == nil {
		h.sendToClient(client, TCMessage{Type: TCMsgSessionExpired})
		return
	}

	room := h.rooms[old.GameID]
	if room == nil {
		h.drop(payload.SessionID)
		h.sendToClient(client, TCMessage{Type: TCMsgSessionExpired})
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

	log.Printf("[티츄][재접속] game=%s | seat%d=%s 재접속 완료",
		room.Game.ID, client.Seat, displayName(client.Name))

	h.broadcastToRoom(room, TCMessage{
		Type:    TCMsgReconnected,
		Payload: TCReconnectedPayload{Seat: client.Seat, Name: client.Name},
	})
	// 전원에게 접속 상태가 반영된 스냅샷 (재접속 당사자 복원 포함)
	h.broadcastState(room)
}

// clearGameSessions 게임이 정상 종료됐을 때 관련 세션·타이머 정리
func (h *TCHub) clearGameSessions(room *tcRoom) {
	for _, c := range room.Clients {
		if c == nil {
			continue
		}
		h.drop(c.SessionID)
	}
}

// ==================== 전송 ====================

func (h *TCHub) sendError(client *TCClient, message string) {
	h.sendToClient(client, TCMessage{Type: TCMsgError, Payload: TCErrorPayload{Message: message}})
}

func (h *TCHub) sendToClient(client *TCClient, message TCMessage) {
	if client == nil {
		return
	}
	sendTo(&client.wsClient, message, "[TC] ")
}

func (h *TCHub) broadcastToRoom(room *tcRoom, message TCMessage) {
	for _, c := range room.Clients {
		if c != nil {
			h.sendToClient(c, message)
		}
	}
	for c := range room.Spectators { // 이벤트·종료 발표는 관전자에게도 간다
		h.sendToClient(c, message)
	}
}
