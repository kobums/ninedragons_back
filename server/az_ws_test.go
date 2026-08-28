package server

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ==================== 아줄 WS 통합 테스트 ====================
//
// 값은 init 에서 한 번만 정한다 — 테스트 도중에 바꾸면 허브·봇 고루틴과
// 경합한다(-race). 차례 마감은 허브 필드라 테스트마다 따로 정한다.
func init() {
	azBotTakeDelay = 0
	azBotTakeJitterMs = 0
	azTilingDelay = 10 * time.Millisecond
}

// azTestTurn 차례 마감으로 끝나면 안 되는 테스트가 쓰는 넉넉한 값
const azTestTurn = 90 * time.Second

// azTestClient 공용 testConn 에 아줄 메시지 타입의 waitFor 를 얹은 래퍼
type azTestClient struct {
	testConn[AZMessage]
}

func newAZTestServer(t *testing.T, grace, turnTimeout time.Duration) (*AZHub, string, func()) {
	t.Helper()
	hub := NewAZHub()
	hub.grace = grace
	hub.turnTimeout = turnTimeout // Run 전에 정한다 (허브 고루틴과 경합 금지)
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeAZWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func azDial(t *testing.T, url string) *azTestClient {
	t.Helper()
	return &azTestClient{dialWS[AZMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *azTestClient) waitFor(t *testing.T, msgType AZMessageType) AZMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m AZMessage) bool { return m.Type == msgType })
}

func azPayloadMap(t *testing.T, msg AZMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// azJoin 입장하고 az_player_joined payload 를 돌려준다
func azJoin(t *testing.T, c *azTestClient, name, room string) map[string]interface{} {
	t.Helper()
	c.send(t, AZMessage{Type: AZMsgJoinGame, Payload: AZJoinGamePayload{Name: name, Room: room}})
	return azPayloadMap(t, c.waitFor(t, AZMsgPlayerJoined))
}

// azWaitPhase 지정한 phase 의 스냅샷이 올 때까지 소비
func (c *azTestClient) azWaitPhase(t *testing.T, phase string) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "az_game_state("+phase+")", func(m AZMessage) bool {
		if m.Type != AZMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		return ok && state["phase"] == phase
	})
	return azPayloadMap(t, msg)
}

// azDrain 배경으로 소켓을 비운다 (읽지 않는 연결의 버퍼 포화 방지)
func azDrain(c *azTestClient) {
	conn := c.conn
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

// azSeatClients 허브 고루틴 없이 스냅샷을 직접 만들어 보는 결정적 테스트용 —
// 소켓 없는 사람 좌석 n개를 앉힌 방을 만든다
func azSeatClients(t *testing.T, h *AZHub, room *azRoom, n int) []*AZClient {
	t.Helper()
	clients := make([]*AZClient, n)
	for i := range clients {
		c := &AZClient{wsClient: newBotWSClient(), Hub: h}
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
	return clients
}

// azNewRoom 허브 고루틴 없이 쓰는 빈 방
func azNewRoom(h *AZHub) *azRoom {
	game := NewAZGame(uuid.New().String())
	room := &azRoom{Game: game, Clients: map[int]*AZClient{}, Code: "TEST"}
	h.rooms[game.ID] = room
	return room
}

// ==================== 은닉 없음 (관전자 = 참가자) ====================

// TestAZSpectatorSeesIdenticalSnapshot 아줄은 모든 정보가 공개다 — 관전자
// 스냅샷은 yourSeat 하나만 다르고 나머지가 참가자와 **완전히 같아야** 한다.
// 이 게임의 계약에서 가장 중요한 회귀 장치다.
func TestAZSpectatorSeesIdenticalSnapshot(t *testing.T) {
	hub := NewAZHub()
	room := azNewRoom(hub)
	azSeatClients(t, hub, room, 3)
	if err := room.Game.Start(rand.New(rand.NewSource(42))); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// 진행 중 상태를 만든다 (패턴 라인·바닥 라인·중앙이 채워진 스냅샷)
	game := room.Game
	game.Factories[0] = []AZColor{AZColorRed, AZColorRed, AZColorBlue, AZColorBlue}
	if err := game.Take(0, "factory:0", AZColorRed, 1); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if err := game.Take(1, "center", AZColorBlue, AZLineTargetFloor); err != nil {
		t.Fatalf("Take(center): %v", err)
	}

	seated := azStateMap(t, hub.buildAZState(room, 0))
	spectator := azStateMap(t, hub.buildAZState(room, -1))

	if spectator["yourSeat"] != float64(-1) {
		t.Fatalf("관전자 yourSeat = %v, want -1", spectator["yourSeat"])
	}
	if seated["yourSeat"] != float64(0) {
		t.Fatalf("참가자 yourSeat = %v, want 0", seated["yourSeat"])
	}
	delete(seated, "yourSeat")
	delete(spectator, "yourSeat")
	if !reflect.DeepEqual(seated, spectator) {
		for k, v := range seated {
			if !reflect.DeepEqual(v, spectator[k]) {
				t.Errorf("필드 %q 가 다릅니다 — 참가자 %v / 관전자 %v", k, v, spectator[k])
			}
		}
		t.Fatal("관전자 스냅샷이 참가자와 다릅니다 (아줄은 은닉이 없다)")
	}

	// 다른 좌석끼리도 yourSeat 만 다르다
	other := azStateMap(t, hub.buildAZState(room, 2))
	delete(other, "yourSeat")
	if !reflect.DeepEqual(seated, other) {
		t.Fatal("좌석마다 스냅샷이 달라졌습니다 (아줄은 은닉이 없다)")
	}
}

// azStateMap 스냅샷을 raw JSON 을 거쳐 map 으로 (와이어에 실제로 나가는 모양)
func azStateMap(t *testing.T, state AZGameStatePayload) map[string]interface{} {
	t.Helper()
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

// TestAZEmptyRoomSnapshotNoPanic 빈 대기실을 관전자 시점(-1)으로 그려도
// 패닉 없이 빈 배열이 나온다
func TestAZEmptyRoomSnapshotNoPanic(t *testing.T) {
	hub := NewAZHub()
	room := azNewRoom(hub)

	state := hub.buildAZState(room, -1)
	if state.YourSeat != -1 || state.HostSeat != -1 {
		t.Fatalf("빈 방 스냅샷 = yourSeat%d hostSeat%d", state.YourSeat, state.HostSeat)
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{
		`"factories":[]`, `"center":[]`, `"players":[]`,
		`"lastAction":null`, `"roundResult":null`, `"result":null`,
		`"phase":"waiting"`, `"currentSeat":-1`, `"firstNextSeat":-1`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("빈 방 스냅샷에 %s 가 없습니다: %s", want, raw)
		}
	}
}

// TestAZSnapshotEmptySlicesAreArrays 빈 슬라이스는 [] 로 나간다 (JSON null 금지)
func TestAZSnapshotEmptySlicesAreArrays(t *testing.T) {
	hub := NewAZHub()
	room := azNewRoom(hub)
	azSeatClients(t, hub, room, 2)
	if err := room.Game.Start(rand.New(rand.NewSource(7))); err != nil {
		t.Fatalf("Start: %v", err)
	}

	raw, err := json.Marshal(hub.buildAZState(room, 0))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	text := string(raw)
	if strings.Contains(text, ":null,\"") && strings.Contains(text, `"floor":null`) {
		t.Fatalf("빈 슬라이스가 null 로 나갔습니다: %s", text)
	}
	for _, want := range []string{
		`"center":[]`, `"floor":[]`, `"wall":[[`, `"lines":[{"color":"","count":0}`,
		`"centerHasFirst":true`, `"round":1`, `"score":0`, `"seat":0`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("스냅샷에 %s 가 없습니다: %s", want, text)
		}
	}
	// 색 와이어 값은 영문 고정이다
	for _, color := range []string{"blue", "yellow", "red", "black", "cyan"} {
		if !strings.Contains(text, `"`+color+`"`) && azCountColor(room.Game.Bag, AZColor(color)) == 0 {
			t.Errorf("색 와이어 값 %q 를 찾을 수 없습니다", color)
		}
	}
}

// ==================== 3봇 완주 ====================

// TestAZThreeBotsCompleteGame 봇을 채운 3인전이 90초 안에 반드시 끝난다 —
// 가장 중요한 회귀 장치. 사람 좌석(테스트 클라이언트)도 같은 평가 함수로
// 스스로 두므로 az_take 의 WS 왕복 경로까지 함께 검증된다.
func TestAZThreeBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newAZTestServer(t, defaultDisconnectGrace, azTestTurn)
	defer cleanup()

	c := azDial(t, url)
	defer c.conn.Close()
	joined := azJoin(t, c, "사람", "")
	mySeat := int(joined["yourSeat"].(float64))
	c.send(t, AZMessage{Type: AZMsgFillBots}) // 3인까지 채우고 즉시 시작

	rng := rand.New(rand.NewSource(20260828))
	deadline := time.Now().Add(90 * time.Second)
	lastKey, sawTake, sawRoundEnd := "", false, false

	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "state-event-or-over", func(m AZMessage) bool {
			return m.Type == AZMsgGameState || m.Type == AZMsgGameOver || m.Type == AZMsgEvent
		})

		if msg.Type == AZMsgEvent {
			ev := azPayloadMap(t, msg)
			kind, _ := ev["kind"].(string)
			switch kind {
			case "take":
				// 수주 이벤트에는 누가 했는지가 반드시 실린다
				if name, _ := ev["name"].(string); name == "" {
					t.Fatalf("수주 이벤트에 name 부재: %v", ev)
				}
				if _, ok := ev["seat"].(float64); !ok {
					t.Fatalf("수주 이벤트에 seat 부재: %v", ev)
				}
				sawTake = true
			case "round_end":
				sawRoundEnd = true
			}
			continue
		}

		if msg.Type == AZMsgGameOver {
			over := azPayloadMap(t, msg)
			if m, _ := over["message"].(string); m == "" {
				t.Fatalf("종료 문구 부재: %v", over)
			}
			winners, _ := over["winnerSeats"].([]interface{})
			if len(winners) == 0 {
				t.Fatalf("승자가 없습니다: %v", over)
			}
			bonuses, _ := over["bonuses"].([]interface{})
			if len(bonuses) != 3 {
				t.Fatalf("최종 보너스 내역 = %d개, want 3개: %v", len(bonuses), over)
			}
			if !sawTake {
				t.Fatal("수주 이벤트를 한 번도 못 봤습니다")
			}
			if !sawRoundEnd {
				t.Fatal("라운드 정산 이벤트를 한 번도 못 봤습니다")
			}
			return
		}

		// 내 차례면 봇과 같은 평가로 직접 둔다
		state, ok := botPayloadAs[azBotState](msg.Payload)
		if !ok || state.Phase != AZPhaseDrafting || state.CurrentSeat != mySeat {
			continue
		}
		left := len(state.Center)
		for _, f := range state.Factories {
			left += len(f)
		}
		key := fmt.Sprintf("%d|%d", state.Round, left)
		if key == lastKey {
			continue
		}
		lastKey = key
		mv, ok := azBotChoose(state.Factories, state.Center, state.CenterHasFirst,
			azBoardOf(state.Players[mySeat]), rng)
		if !ok {
			continue
		}
		c.send(t, AZMessage{Type: AZMsgTake,
			Payload: AZTakePayload{From: mv.From, Color: mv.Color, Line: mv.Line}})
	}
	t.Fatal("90초 안에 게임이 끝나지 않았습니다")
}

// ==================== 방 코드 / 관전 ====================

// TestAZRoomCodeAndSpectate 사설 방 코드로 모이고, 시작 뒤 같은 코드로
// 들어온 사람은 관전자가 된다 (yourSeat -1, 내용은 참가자와 동일)
func TestAZRoomCodeAndSpectate(t *testing.T) {
	_, url, cleanup := newAZTestServer(t, defaultDisconnectGrace, azTestTurn)
	defer cleanup()

	host := azDial(t, url)
	defer host.conn.Close()
	joined := azJoin(t, host, "호스트", "NEW")
	code, _ := joined["roomCode"].(string)
	if len(code) != roomCodeLen {
		t.Fatalf("방 코드 = %q, want %d자", code, roomCodeLen)
	}

	guest := azDial(t, url)
	defer guest.conn.Close()
	g := azJoin(t, guest, "손님", strings.ToLower(code)) // 대소문자 무시
	if int(g["yourSeat"].(float64)) != 1 {
		t.Fatalf("손님 좌석 = %v, want 1", g["yourSeat"])
	}
	azDrain(guest)

	host.send(t, AZMessage{Type: AZMsgFillBots}) // 3인 채움 즉시 시작
	host.azWaitPhase(t, "drafting")

	spec := azDial(t, url)
	defer spec.conn.Close()
	spec.send(t, AZMessage{Type: AZMsgJoinGame, Payload: AZJoinGamePayload{Name: "관전", Room: code}})
	spec.waitFor(t, AZMsgSpectateJoined)

	state := spec.azWaitPhase(t, "drafting")
	if state["yourSeat"] != float64(-1) {
		t.Fatalf("관전자 yourSeat = %v, want -1", state["yourSeat"])
	}
	if state["spectators"] != float64(1) {
		t.Fatalf("관전자 수 = %v, want 1", state["spectators"])
	}
	players, _ := state["players"].([]interface{})
	if len(players) != 3 {
		t.Fatalf("관전자가 본 좌석 수 = %d, want 3", len(players))
	}
	// 관전자도 모든 개인 보드를 본다 (은닉 없음)
	for i, raw := range players {
		p, _ := raw.(map[string]interface{})
		for _, key := range []string{"lines", "wall", "floor", "score"} {
			if _, ok := p[key]; !ok {
				t.Fatalf("관전자 스냅샷 seat%d 에 %q 가 없습니다: %v", i, key, p)
			}
		}
	}
	if _, ok := state["factories"]; !ok {
		t.Fatal("관전자 스냅샷에 진열대가 없습니다")
	}

	// 관전자는 아무 행동도 할 수 없다
	spec.send(t, AZMessage{Type: AZMsgTake,
		Payload: AZTakePayload{From: "factory:0", Color: AZColorBlue, Line: 0}})
	errMsg := azPayloadMap(t, spec.waitFor(t, AZMsgError))
	if errMsg["message"] != spectatorDeniedMsg {
		t.Fatalf("관전자 거부 문구 = %v, want %q", errMsg["message"], spectatorDeniedMsg)
	}
}

// ==================== 재접속 / 봇 대체 ====================

// TestAZReconnect 세션 ID로 좌석·보드가 그대로 복원된다
func TestAZReconnect(t *testing.T) {
	_, url, cleanup := newAZTestServer(t, 5*time.Second, azTestTurn)
	defer cleanup()

	host := azDial(t, url)
	defer host.conn.Close()
	azJoin(t, host, "호스트", "NEW")

	guest := azDial(t, url)
	joined := azJoin(t, guest, "손님", "")
	_ = joined
	guest.conn.Close()

	// 공용 로비로 다시 붙는다 (사설 방 코드 없이 2인전)
	a := azDial(t, url)
	defer a.conn.Close()
	pa := azJoin(t, a, "가", "")
	b := azDial(t, url)
	pb := azJoin(t, b, "나", "")
	sessionB, _ := pb["sessionId"].(string)
	seatB := int(pb["yourSeat"].(float64))
	if pa["gameId"] != pb["gameId"] {
		t.Fatalf("같은 로비에 앉지 않았습니다: %v vs %v", pa["gameId"], pb["gameId"])
	}
	azDrain(a)

	a.send(t, AZMessage{Type: AZMsgStart})
	time.Sleep(100 * time.Millisecond)

	b.conn.Close()
	time.Sleep(100 * time.Millisecond)

	back := azDial(t, url)
	defer back.conn.Close()
	back.send(t, AZMessage{Type: AZMsgRejoin, Payload: AZRejoinPayload{SessionID: sessionB}})
	rec := azPayloadMap(t, back.waitFor(t, AZMsgPlayerReconnected))
	if int(rec["seat"].(float64)) != seatB {
		t.Fatalf("복원 좌석 = %v, want %d", rec["seat"], seatB)
	}

	state := back.azWaitPhase(t, "drafting")
	if int(state["yourSeat"].(float64)) != seatB {
		t.Fatalf("복원 스냅샷 yourSeat = %v, want %d", state["yourSeat"], seatB)
	}
	players, _ := state["players"].([]interface{})
	me, _ := players[seatB].(map[string]interface{})
	if me["connected"] != true {
		t.Fatalf("재접속 후에도 연결 끊김으로 표시됩니다: %v", me)
	}
}

// TestAZRejoinUnknownSessionExpires 모르는 세션은 만료 통지
func TestAZRejoinUnknownSessionExpires(t *testing.T) {
	_, url, cleanup := newAZTestServer(t, defaultDisconnectGrace, azTestTurn)
	defer cleanup()

	c := azDial(t, url)
	defer c.conn.Close()
	c.send(t, AZMessage{Type: AZMsgRejoin, Payload: AZRejoinPayload{SessionID: "없는-세션"}})
	c.waitFor(t, AZMsgSessionExpired)
}

// TestAZBotTakeover 유예가 지나면 이탈 좌석을 연습봇이 이어받는다
func TestAZBotTakeover(t *testing.T) {
	_, url, cleanup := newAZTestServer(t, 120*time.Millisecond, azTestTurn)
	defer cleanup()

	a := azDial(t, url)
	defer a.conn.Close()
	azJoin(t, a, "가", "")
	b := azDial(t, url)
	azJoin(t, b, "나", "")

	a.send(t, AZMessage{Type: AZMsgStart})
	a.azWaitPhase(t, "drafting")

	b.conn.Close()

	msg := a.waitMatch(t, "bot_takeover", func(m AZMessage) bool {
		if m.Type != AZMsgEvent {
			return false
		}
		ev, ok := m.Payload.(map[string]interface{})
		return ok && ev["kind"] == "bot_takeover"
	})
	ev := azPayloadMap(t, msg)
	if ev["name"] != "나" {
		t.Fatalf("봇 대체 이벤트 name = %v, want 나", ev["name"])
	}
	if _, ok := ev["seat"].(float64); !ok {
		t.Fatalf("봇 대체 이벤트에 seat 부재: %v", ev)
	}
}

// ==================== AFK 자동 진행 ====================

// TestAZAFKAutoTake 차례 마감이 지나면 감점이 가장 적은 수를 자동으로 둔다
func TestAZAFKAutoTake(t *testing.T) {
	_, url, cleanup := newAZTestServer(t, defaultDisconnectGrace, 120*time.Millisecond)
	defer cleanup()

	a := azDial(t, url)
	defer a.conn.Close()
	azJoin(t, a, "가", "")
	b := azDial(t, url)
	defer b.conn.Close()
	azJoin(t, b, "나", "")
	azDrain(b)

	a.send(t, AZMessage{Type: AZMsgStart})
	a.azWaitPhase(t, "drafting")

	// 아무도 두지 않아도 자동으로 진행된다
	msg := a.waitMatch(t, "afk", func(m AZMessage) bool {
		if m.Type != AZMsgEvent {
			return false
		}
		ev, ok := m.Payload.(map[string]interface{})
		return ok && ev["kind"] == "afk"
	})
	ev := azPayloadMap(t, msg)
	if name, _ := ev["name"].(string); name == "" {
		t.Fatalf("AFK 이벤트에 name 부재: %v", ev)
	}

	// 자동 수가 실제로 반영돼 lastAction 이 채워진다
	state := a.waitMatch(t, "az_game_state(lastAction)", func(m AZMessage) bool {
		if m.Type != AZMsgGameState {
			return false
		}
		s, ok := m.Payload.(map[string]interface{})
		if !ok {
			return false
		}
		last, _ := s["lastAction"].(map[string]interface{})
		return last != nil
	})
	s := azPayloadMap(t, state)
	last, _ := s["lastAction"].(map[string]interface{})
	if msgText, _ := last["message"].(string); msgText == "" {
		t.Fatalf("lastAction 문구 부재: %v", last)
	}
	if name, _ := last["name"].(string); name == "" {
		t.Fatalf("lastAction 에 name 부재: %v", last)
	}
}

// ==================== 리액션 ====================

// TestAZReact 화이트리스트 이모지는 전원에게 되쏘고, 그 외는 조용히 무시한다
func TestAZReact(t *testing.T) {
	_, url, cleanup := newAZTestServer(t, defaultDisconnectGrace, azTestTurn)
	defer cleanup()

	a := azDial(t, url)
	defer a.conn.Close()
	azJoin(t, a, "가", "")
	b := azDial(t, url)
	defer b.conn.Close()
	azJoin(t, b, "나", "")

	b.send(t, AZMessage{Type: AZMsgReact, Payload: AZReactPayload{Emoji: "🔥"}})
	msg := a.waitMatch(t, "react", func(m AZMessage) bool {
		if m.Type != AZMsgEvent {
			return false
		}
		ev, ok := m.Payload.(map[string]interface{})
		return ok && ev["kind"] == "react"
	})
	ev := azPayloadMap(t, msg)
	if ev["message"] != "🔥" || ev["name"] != "나" {
		t.Fatalf("리액션 이벤트 = %v", ev)
	}
	if _, ok := ev["seat"].(float64); !ok {
		t.Fatalf("리액션 이벤트에 seat 부재: %v", ev)
	}
}

// ==================== 에러 경로 ====================

// TestAZTakeErrorsOverWS 규약 위반은 az_error 로만 응답하고 상태를 건드리지 않는다
func TestAZTakeErrorsOverWS(t *testing.T) {
	_, url, cleanup := newAZTestServer(t, defaultDisconnectGrace, azTestTurn)
	defer cleanup()

	a := azDial(t, url)
	defer a.conn.Close()
	azJoin(t, a, "가", "")
	b := azDial(t, url)
	defer b.conn.Close()
	azJoin(t, b, "나", "")
	azDrain(b)

	a.send(t, AZMessage{Type: AZMsgStart})
	state := a.azWaitPhase(t, "drafting")
	current := int(state["currentSeat"].(float64))
	if current != 0 {
		t.Fatalf("선 = seat%d, want seat0", current)
	}

	// 없는 진열대
	a.send(t, AZMessage{Type: AZMsgTake,
		Payload: AZTakePayload{From: "factory:99", Color: AZColorBlue, Line: 0}})
	e := azPayloadMap(t, a.waitFor(t, AZMsgError))
	if msg, _ := e["message"].(string); msg == "" {
		t.Fatalf("에러 문구 부재: %v", e)
	}

	// 시작 전에는 봇 채우기·시작 권한이 호스트에게만 있다
	b.send(t, AZMessage{Type: AZMsgStart})
}

// TestAZHostOnlyControls 호스트가 아니면 시작·봇 채우기를 못 한다
func TestAZHostOnlyControls(t *testing.T) {
	_, url, cleanup := newAZTestServer(t, defaultDisconnectGrace, azTestTurn)
	defer cleanup()

	a := azDial(t, url)
	defer a.conn.Close()
	azJoin(t, a, "가", "")
	azDrain(a)

	b := azDial(t, url)
	defer b.conn.Close()
	azJoin(t, b, "나", "")

	b.send(t, AZMessage{Type: AZMsgFillBots})
	e := azPayloadMap(t, b.waitFor(t, AZMsgError))
	if e["message"] != "호스트만 봇을 채울 수 있습니다" {
		t.Fatalf("에러 문구 = %v", e["message"])
	}

	b.send(t, AZMessage{Type: AZMsgStart})
	e = azPayloadMap(t, b.waitFor(t, AZMsgError))
	if e["message"] != "호스트만 시작할 수 있습니다" {
		t.Fatalf("에러 문구 = %v", e["message"])
	}
}
