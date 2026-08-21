package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 테스트에서는 대기 상태 마감을 짧게 낮춘다 (창 자동 통과·AFK 자동 진행)
func init() {
	cpTurnTimeout = 150 * time.Millisecond
	cpWindowTimeout = 120 * time.Millisecond
}

// cpTestClient 공용 testConn 에 쿠 메시지 타입의 waitFor 를 얹은 래퍼
type cpTestClient struct {
	testConn[CPMessage]
}

func newCPTestServer(t *testing.T, grace time.Duration) (*CPHub, string, func()) {
	t.Helper()
	hub := NewCPHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeCPWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func cpDial(t *testing.T, url string) *cpTestClient {
	t.Helper()
	return &cpTestClient{dialWS[CPMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *cpTestClient) waitFor(t *testing.T, msgType CPMessageType) CPMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m CPMessage) bool { return m.Type == msgType })
}

func cpPayloadMap(t *testing.T, msg CPMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// cpJoin 입장하고 cp_player_joined payload 를 돌려준다
func cpJoin(t *testing.T, c *cpTestClient, name, room string) map[string]interface{} {
	t.Helper()
	c.send(t, CPMessage{Type: CPMsgJoinGame, Payload: CPJoinGamePayload{Name: name, Room: room}})
	return cpPayloadMap(t, c.waitFor(t, CPMsgPlayerJoined))
}

// cpWaitPhase 지정한 phase 의 스냅샷이 올 때까지 소비
func (c *cpTestClient) cpWaitPhase(t *testing.T, phase string) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "cp_game_state("+phase+")", func(m CPMessage) bool {
		if m.Type != CPMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		return ok && state["phase"] == phase
	})
	return cpPayloadMap(t, msg)
}

// TestCPFourBotsCompleteGame 봇을 채운 4인 게임이 60초 안에 완주하는지 —
// 가장 중요한 회귀 장치 (창 교착·챌린지 상호작용·턴 미전환 감지).
// 좌석 0은 서버 연습봇 두뇌(cpBrain)를 WS 로 감싼 드라이버가 잡는다.
func TestCPFourBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newCPTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := cpDial(t, url)
	defer c.conn.Close()
	cpJoin(t, c, "감독", "")
	c.send(t, CPMessage{Type: CPMsgFillBots}) // 4인까지 채우고 즉시 시작

	start := time.Now()
	brain := newCPBrain()
	deadline := start.Add(60 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "state-or-over", func(m CPMessage) bool {
			return m.Type == CPMsgGameState || m.Type == CPMsgGameOver
		})
		if msg.Type == CPMsgGameOver {
			over := cpPayloadMap(t, msg)
			winner := int(over["winnerSeat"].(float64))
			if winner < 0 || winner >= CPFillBotTarget {
				t.Fatalf("winnerSeat = %d", winner)
			}
			if name, _ := over["winnerName"].(string); name == "" {
				t.Fatalf("winnerName 부재: %v", over)
			}
			players := over["players"].([]interface{})
			if len(players) != CPFillBotTarget {
				t.Fatalf("players 길이 = %d, want %d", len(players), CPFillBotTarget)
			}
			// 승자만 비공개 카드를 남기고 나머지는 전부 잃었어야 한다
			for _, pRaw := range players {
				p := pRaw.(map[string]interface{})
				seat := int(p["seat"].(float64))
				count := int(p["cardCount"].(float64))
				if seat == winner && count < 1 {
					t.Fatalf("승자 카드 0장: %v", p)
				}
				if seat != winner && count != 0 {
					t.Fatalf("패자 seat%d 카드 %d장 잔존", seat, count)
				}
			}
			t.Logf("완주: winner=seat%d (%.1fs)", winner, time.Since(start).Seconds())
			return
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("60초 안에 게임이 끝나지 않았다 — 진행 불가 상태")
}

// TestCPAfkAutoProgress 접속만 유지한 채 아무것도 하지 않는 2인전 —
// 세금 선언 뒤 아무도 허용하지 않아도 챌린지 창이 마감으로 통과되고(+3),
// 이후 전원 방치로도 자동 수입 → 쿠 강제 → 무작위 제거를 거쳐 게임이
// 끝까지 완주하는지 (endsAt 노출·afk 이벤트 포함).
func TestCPAfkAutoProgress(t *testing.T) {
	_, url, cleanup := newCPTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := cpDial(t, url)
	defer host.conn.Close()
	cpJoin(t, host, "잠수1", "")

	guest := cpDial(t, url)
	defer guest.conn.Close()
	cpJoin(t, guest, "잠수2", "")

	host.send(t, CPMessage{Type: CPMsgStart})
	state := host.cpWaitPhase(t, string(CPPhaseAction))
	if ends := int64(state["endsAt"].(float64)); ends <= 0 {
		t.Fatalf("action 스냅샷의 endsAt = %d, want unixMillis", ends)
	}

	// guest 는 더 읽지 않는다 — 백그라운드로 비워 버퍼 포화만 막는다
	go func() {
		for {
			if _, _, err := guest.conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	conns := map[int]*cpTestClient{0: host, 1: guest}

	// 현재 차례 좌석으로 세금을 선언한다 (AFK 자동 수입과 경합할 수 있어
	// action 스냅샷이 올 때마다 재시도 — 하나만 창에 안착하면 된다)
	var window map[string]interface{}
	taxDeadline := time.Now().Add(10 * time.Second)
	for window == nil && time.Now().Before(taxDeadline) {
		msg := host.waitMatch(t, "cp_game_state", func(m CPMessage) bool {
			return m.Type == CPMsgGameState
		})
		st := cpPayloadMap(t, msg)
		switch st["phase"] {
		case string(CPPhaseChallengeWindow):
			window = st
		case string(CPPhaseAction):
			cur := int(st["currentSeat"].(float64))
			conns[cur].send(t, CPMessage{Type: CPMsgAction,
				Payload: CPActionPayload{Kind: string(CPActTax)}})
		}
	}
	if window == nil {
		t.Fatal("세금 챌린지 창이 열리지 않았다")
	}
	pending := asPayloadMap(t, window["pending"])
	actorSeat := int(pending["actorSeat"].(float64))
	prevChips := 0
	for _, pRaw := range window["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		if int(p["seat"].(float64)) == actorSeat {
			prevChips = int(p["chips"].(float64))
		}
	}
	// 아무도 허용하지 않는다 — 마감 경과만으로 창이 통과되고 세금이 걷힌다
	host.waitMatch(t, "tax-resolved", func(m CPMessage) bool {
		if m.Type != CPMsgGameState {
			return false
		}
		st, ok := m.Payload.(map[string]interface{})
		if !ok {
			return false
		}
		for _, pRaw := range st["players"].([]interface{}) {
			p := pRaw.(map[string]interface{})
			if int(p["seat"].(float64)) == actorSeat {
				return int(p["chips"].(float64)) == prevChips+3
			}
		}
		return false
	})

	// 이후 전원 방치 — 자동 수입·쿠 강제·무작위 제거로 완주해야 한다
	sawAfk := false
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		msg := host.waitMatch(t, "event-or-over", func(m CPMessage) bool {
			return m.Type == CPMsgEvent || m.Type == CPMsgGameOver
		})
		if msg.Type == CPMsgEvent {
			ev := cpPayloadMap(t, msg)
			if ev["kind"] == "afk" {
				if !strings.Contains(ev["message"].(string), "자동") {
					t.Fatalf("afk 문구 = %v", ev["message"])
				}
				sawAfk = true
			}
			continue
		}
		over := cpPayloadMap(t, msg)
		if int(over["winnerSeat"].(float64)) < 0 {
			t.Fatalf("winnerSeat = %v", over["winnerSeat"])
		}
		if !sawAfk {
			t.Fatal("afk 자동 진행 이벤트가 한 번도 없었다")
		}
		return
	}
	t.Fatal("전원 방치 게임이 45초 안에 끝나지 않았다")
}

// TestCPHiddenState 은닉의 핵심 계약 — 허브 고루틴 없이 핸들러를 직접 불러
// 결정적으로 검증한다. yourCards/yourExchange 는 본인 스냅샷에만 실리고,
// 타인·관전자의 스냅샷 raw JSON 에는 그 역할 문자열 자체가 없어야 한다.
func TestCPHiddenState(t *testing.T) {
	h := NewCPHub()
	room := h.lobbyRoomFor("")
	clients := make([]*CPClient, 3)
	for i := range clients {
		c := &CPClient{wsClient: newBotWSClient(), Hub: h}
		c.Bot = false // 소켓 없는 사람 취급
		c.Name = fmt.Sprintf("P%d", i)
		seat, err := room.Game.AddPlayer(c.Name)
		if err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
		c.GameID = room.Game.ID
		c.Seat = seat
		room.Clients[seat] = c
		h.sessions[c.SessionID] = c
		clients[i] = c
	}
	h.startGame(room)

	game := room.Game
	game.CurrentSeat = 0
	cpSetCards(game, 0, CPRoleDuke, CPRoleAssassin)
	cpSetCards(game, 1, CPRoleCaptain, CPRoleCaptain)
	cpSetCards(game, 2, CPRoleContessa, CPRoleAmbassador)
	// 덱까지 결정적으로 — 교환으로 뽑히는 2장도 duke 라 유출 검사가 성립한다
	game.Deck = make([]CPRole, 9)
	for i := range game.Deck {
		game.Deck[i] = CPRoleDuke
	}

	rawOf := func(viewer int) string {
		data, err := json.Marshal(h.buildCPState(room, viewer))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(data)
	}
	mustHide := func(viewer int, raw string, roles ...CPRole) {
		t.Helper()
		for _, r := range roles {
			if strings.Contains(raw, string(r)) {
				t.Fatalf("viewer %d 스냅샷에 %s 유출:\n%s", viewer, r, raw)
			}
		}
	}

	// ---- action 단계: 본인 카드만 보인다 ----
	raw0 := rawOf(0)
	if !strings.Contains(raw0, `"yourCards":["duke","assassin"]`) {
		t.Fatalf("본인 카드 부재:\n%s", raw0)
	}
	mustHide(0, raw0, CPRoleCaptain, CPRoleContessa, CPRoleAmbassador)

	raw1 := rawOf(1)
	if !strings.Contains(raw1, `"yourCards":["captain","captain"]`) {
		t.Fatalf("본인 카드 부재:\n%s", raw1)
	}
	mustHide(1, raw1, CPRoleDuke, CPRoleAssassin, CPRoleContessa, CPRoleAmbassador)

	// 관전자(-1)에게는 역할 문자열이 하나도 없다 (빈 배열 보장 포함)
	rawSpec := rawOf(-1)
	mustHide(-1, rawSpec, CPRoleDuke, CPRoleAssassin, CPRoleCaptain, CPRoleContessa, CPRoleAmbassador)
	if !strings.Contains(rawSpec, `"yourCards":[]`) || !strings.Contains(rawSpec, `"yourExchange":[]`) {
		t.Fatalf("빈 슬라이스가 []가 아니다:\n%s", rawSpec)
	}
	specState := h.buildCPState(room, -1)
	if specState.YourSeat != -1 || specState.DeckCount != 9 {
		t.Fatalf("관전자 스냅샷: yourSeat=%d deckCount=%d", specState.YourSeat, specState.DeckCount)
	}

	// ---- 교환: 선택지는 본인에게만 ----
	h.handleGameMessage(CPGameMessage{Client: clients[0], Message: CPMessage{
		Type: CPMsgAction, Payload: CPActionPayload{Kind: string(CPActExchange)}}})
	if game.Phase != CPPhaseChallengeWindow {
		t.Fatalf("phase = %s, want challenge_window", game.Phase)
	}
	if h.buildCPState(room, 1).EndsAt <= 0 {
		t.Fatal("창 스냅샷의 endsAt 부재")
	}
	h.handleGameMessage(CPGameMessage{Client: clients[1], Message: CPMessage{Type: CPMsgAllow}})
	if game.Phase != CPPhaseChallengeWindow {
		t.Fatalf("한 명 허용만으로 창이 닫혔다: %s", game.Phase)
	}
	h.handleGameMessage(CPGameMessage{Client: clients[2], Message: CPMessage{Type: CPMsgAllow}})
	if game.Phase != CPPhaseExchangePick {
		t.Fatalf("phase = %s, want exchange_pick", game.Phase)
	}

	raw0 = rawOf(0)
	if !strings.Contains(raw0, `"yourExchange":["duke","assassin","duke","duke"]`) {
		t.Fatalf("교환 선택지 부재:\n%s", raw0)
	}
	raw1 = rawOf(1)
	mustHide(1, raw1, CPRoleDuke, CPRoleAssassin, CPRoleContessa, CPRoleAmbassador)
	if !strings.Contains(raw1, `"yourExchange":[]`) {
		t.Fatalf("타인의 교환 선택지가 비어 있지 않다:\n%s", raw1)
	}

	h.handleGameMessage(CPGameMessage{Client: clients[0], Message: CPMessage{
		Type: CPMsgExchangeKeep, Payload: CPExchangeKeepPayload{Indices: []int{2, 3}}}})
	if game.Phase != CPPhaseAction || game.CurrentSeat != 1 {
		t.Fatalf("교환 뒤 턴 전환 실패: phase=%s current=%d", game.Phase, game.CurrentSeat)
	}
	if len(game.Deck) != 9 {
		t.Fatalf("덱 장수 = %d, want 9", len(game.Deck))
	}

	// ---- 공개로 뒤집힌 카드는 전원에게 보인다 (은닉 해제의 유일한 경로) ----
	game.Players[2].Cards[0].Revealed = true
	state1 := h.buildCPState(room, 1)
	found := false
	for _, pv := range state1.Players {
		if pv.Seat == 2 {
			if pv.CardCount != 1 || len(pv.Revealed) != 1 || pv.Revealed[0] != string(CPRoleContessa) {
				t.Fatalf("공개 카드 미반영: %+v", pv)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("seat2 부재")
	}
	// 여전히 남의 비공개(ambassador)는 새지 않는다
	mustHide(1, rawOf(1), CPRoleAmbassador)
}

// TestCPRoomCodeAndSpectate 방 코드 발급·코드 입장·시작 후 코드 관전 진입.
// 관전자 스냅샷은 yourSeat -1 에 비공개 카드 없음. 행동은 전부 차단된다.
func TestCPRoomCodeAndSpectate(t *testing.T) {
	_, url, cleanup := newCPTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := cpDial(t, url)
	defer host.conn.Close()
	joined := cpJoin(t, host, "호스트", "NEW")
	code, _ := joined["roomCode"].(string)
	if len(code) != roomCodeLen {
		t.Fatalf("roomCode = %q, want %d자", code, roomCodeLen)
	}

	guest := cpDial(t, url)
	defer guest.conn.Close()
	guestJoined := cpJoin(t, guest, "친구", code)
	if guestJoined["roomCode"] != code || int(guestJoined["yourSeat"].(float64)) != 1 {
		t.Fatalf("코드 입장 실패: %v", guestJoined)
	}

	host.send(t, CPMessage{Type: CPMsgStart})
	state := host.cpWaitPhase(t, string(CPPhaseAction))
	if state["roomCode"] != code || len(state["players"].([]interface{})) != 2 {
		t.Fatalf("시작 실패: %v", state["players"])
	}
	if cards := state["yourCards"].([]interface{}); len(cards) != CPCardsPerPlayer {
		t.Fatalf("호스트 비공개 카드 = %v", cards)
	}

	// 시작된 방의 코드로 들어오면 관전자
	spec := cpDial(t, url)
	defer spec.conn.Close()
	spec.send(t, CPMessage{Type: CPMsgJoinGame, Payload: CPJoinGamePayload{Name: "구경꾼", Room: code}})
	specJoined := cpPayloadMap(t, spec.waitFor(t, CPMsgSpectateJoined))
	if specJoined["roomCode"] != code {
		t.Fatalf("관전 입장 roomCode = %v", specJoined["roomCode"])
	}

	specState := cpPayloadMap(t, spec.waitFor(t, CPMsgGameState))
	if int(specState["yourSeat"].(float64)) != -1 {
		t.Fatalf("관전자 yourSeat = %v, want -1", specState["yourSeat"])
	}
	if cards := specState["yourCards"].([]interface{}); len(cards) != 0 {
		t.Fatalf("관전자에게 비공개 카드 유출: %v", cards)
	}
	for _, pRaw := range specState["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		if int(p["cardCount"].(float64)) < 1 {
			t.Fatalf("관전자 스냅샷 좌석 정보 이상: %v", p)
		}
	}

	// 관전자는 어떤 행동도 못 한다
	spec.send(t, CPMessage{Type: CPMsgAllow})
	errPayload := cpPayloadMap(t, spec.waitFor(t, CPMsgError))
	if errPayload["message"] != spectatorDeniedMsg {
		t.Fatalf("관전자 차단 문구 = %v", errPayload["message"])
	}
}
