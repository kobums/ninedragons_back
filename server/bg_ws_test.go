package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// 테스트에서는 단계 마감과 봇의 생각 시간을 짧게 낮춘다
// (실사용은 차례 60초 · 대응 20초 · 잡화점 15초 · 손패 줄이기 15초)
func init() {
	bgTurnTimeout = 150 * time.Millisecond
	bgRespondTimeout = 100 * time.Millisecond
	bgStoreTimeout = 80 * time.Millisecond
	bgDiscardTimeout = 80 * time.Millisecond
	bgBotDelay = 0
	bgBotJitterMs = 0
}

// bgTestClient 공용 testConn 에 뱅! 메시지 타입의 waitFor 를 얹은 래퍼
type bgTestClient struct {
	testConn[BGMessage]
}

func newBGTestServer(t *testing.T, grace time.Duration) (*BGHub, string, func()) {
	t.Helper()
	hub := NewBGHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeBGWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func bgDial(t *testing.T, url string) *bgTestClient {
	t.Helper()
	return &bgTestClient{dialWS[BGMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *bgTestClient) waitFor(t *testing.T, msgType BGMessageType) BGMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m BGMessage) bool { return m.Type == msgType })
}

func bgPayloadMap(t *testing.T, msg BGMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// bgJoin 입장하고 bg_player_joined payload 를 돌려준다
func bgJoin(t *testing.T, c *bgTestClient, name, room string) map[string]interface{} {
	t.Helper()
	c.send(t, BGMessage{Type: BGMsgJoinGame, Payload: BGJoinGamePayload{Name: name, Room: room}})
	return bgPayloadMap(t, c.waitFor(t, BGMsgPlayerJoined))
}

// bgDrainConn 배경으로 소켓을 비운다 (읽지 않는 연결의 버퍼 포화 방지)
func bgDrainConn(c *bgTestClient) {
	conn := c.conn
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

// bgAwaitGameOver 게임이 끝날 때까지 읽는다 (긴 대기 — waitMatch 의 3초 한도
// 로는 한 판을 담을 수 없다). 중간에 온 스냅샷은 onState 로 흘려준다.
func bgAwaitGameOver(t *testing.T, c *bgTestClient, timeout time.Duration,
	onState func(BGMessage)) map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var msg BGMessage
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				t.Fatalf("unmarshal: %v (%s)", err, line)
			}
			if msg.Type == BGMsgGameOver {
				return bgPayloadMap(t, msg)
			}
			if onState != nil {
				onState(msg)
			}
		}
	}
	t.Fatalf("%s 안에 게임이 끝나지 않았다", timeout)
	return nil
}

// ==================== 완주 ====================

// TestBGFiveBotsCompleteGame 봇을 채운 5인 게임이 120초 안에 완주하는지 —
// 가장 중요한 회귀 장치 (대응 창 교착·거리 오류·종료 판정 감지).
// 좌석 0은 서버 연습봇 두뇌(bgBrain)를 WS 로 감싼 드라이버가 잡는다.
func TestBGFiveBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newBGTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := bgDial(t, url)
	defer c.conn.Close()
	bgJoin(t, c, "보안관후보", "")
	c.send(t, BGMessage{Type: BGMsgFillBots}) // 5인까지 채우고 즉시 시작

	start := time.Now()
	brain := newBGBrain()
	over := bgAwaitGameOver(t, c, 120*time.Second, func(msg BGMessage) {
		if msg.Type == BGMsgError {
			return // 두뇌가 스스로 복구한다
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	})

	winner, _ := over["winner"].(string)
	switch winner {
	case "sheriff", "outlaw", "renegade":
	default:
		t.Fatalf("승리 진영 = %v", over["winner"])
	}
	seats, _ := over["winnerSeats"].([]interface{})
	names, _ := over["winnerNames"].([]interface{})
	if len(seats) == 0 || len(seats) != len(names) {
		t.Fatalf("승자 = %v / %v", over["winnerSeats"], over["winnerNames"])
	}
	if m, _ := over["message"].(string); m == "" || !hasHangul(m) {
		t.Fatalf("종료 문구 = %v", over["message"])
	}
	turns := int(over["turns"].(float64))
	if turns < 1 || turns > BGMaxTurns {
		t.Fatalf("차례 = %d", turns)
	}

	players := over["players"].([]interface{})
	if len(players) != BGFillBotTarget {
		t.Fatalf("players 길이 = %d, want %d", len(players), BGFillBotTarget)
	}
	roles := map[string]int{}
	for _, pRaw := range players {
		p := pRaw.(map[string]interface{})
		role, _ := p["role"].(string)
		if role == "" {
			t.Fatalf("종료 화면인데 역할이 비공개다: %v", p)
		}
		roles[role]++
		// 종료 화면에도 남의 손패 내용은 없다
		if _, leaked := p["hand"]; leaked {
			t.Fatalf("종료 화면에 손패 유출: %v", p)
		}
		for _, key := range []string{"handCount", "hp", "maxHp", "alive", "equipment"} {
			if _, ok := p[key]; !ok {
				t.Fatalf("%s 부재: %v", key, p)
			}
		}
	}
	if roles["sheriff"] != 1 {
		t.Fatalf("보안관 = %d명", roles["sheriff"])
	}
	if roles["outlaw"] != 2 || roles["renegade"] != 1 || roles["deputy"] != 1 {
		t.Fatalf("5인 역할 구성 = %v", roles)
	}
	t.Logf("완주: %s 진영 승 (%v) · %d차례 (%.1fs)",
		winner, over["winnerNames"], turns, time.Since(start).Seconds())
}

// TestBGAfkDrivesGameToEnd 아무도 조작하지 않아도 마감 타이머가 판을 끝까지
// 민다 — 차례 60초·대응 20초·잡화점 15초·손패 줄이기 15초의 회귀 장치.
func TestBGAfkDrivesGameToEnd(t *testing.T) {
	_, url, cleanup := newBGTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := bgDial(t, url)
	defer c.conn.Close()
	bgJoin(t, c, "무응답", "")
	c.send(t, BGMessage{Type: BGMsgFillBots})

	// 사람은 한 번도 응답하지 않는다 — 봇 넷과 AFK 자동 진행만으로 끝나야 한다
	over := bgAwaitGameOver(t, c, 120*time.Second, nil)
	if m, _ := over["message"].(string); !hasHangul(m) {
		t.Fatalf("종료 문구 = %v", over["message"])
	}
	if turns := int(over["turns"].(float64)); turns < 1 {
		t.Fatalf("차례 = %d", turns)
	}
}

// ==================== 은닉 (와이어) ====================

// TestBGHiddenOverWire 실제 소켓으로 받은 스냅샷의 은닉 계약.
//   - yourRole·yourHand·yourBangUsed 는 본인 payload 에만 (관전자는 키 부재)
//   - players[].role 은 보안관만 공개
func TestBGHiddenOverWire(t *testing.T) {
	_, url, cleanup := newBGTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := bgDial(t, url)
	defer host.conn.Close()
	joined := bgJoin(t, host, "호스트", roomCodeNew)
	code, _ := joined["roomCode"].(string)
	if len(code) != roomCodeLen {
		t.Fatalf("방 코드 = %q", code)
	}
	host.send(t, BGMessage{Type: BGMsgFillBots})

	state := host.bgWaitPlaying(t)
	for _, key := range []string{"yourRole", "yourHand", "yourBangUsed"} {
		if _, ok := state[key]; !ok {
			t.Fatalf("본인 스냅샷에 %s 부재: %v", key, state)
		}
	}
	myRole, _ := state["yourRole"].(string)
	if myRole == "" {
		t.Fatalf("yourRole 이 비었다: %v", state)
	}

	players := state["players"].([]interface{})
	revealed := 0
	for _, pRaw := range players {
		p := pRaw.(map[string]interface{})
		if role, _ := p["role"].(string); role != "" {
			revealed++
			if role != "sheriff" {
				t.Fatalf("보안관 아닌 역할이 공개됐다: %v", p)
			}
		}
		if _, leaked := p["hand"]; leaked {
			t.Fatalf("남의 손패 유출: %v", p)
		}
	}
	if revealed != 1 {
		t.Fatalf("공개된 역할 = %d개, want 1 (보안관만)", revealed)
	}

	// 같은 방 코드로 관전 입장
	spec := bgDial(t, url)
	defer spec.conn.Close()
	spec.send(t, BGMessage{Type: BGMsgJoinGame,
		Payload: BGJoinGamePayload{Name: "구경꾼", Room: code}})
	spec.waitFor(t, BGMsgSpectateJoined)

	specState := bgPayloadMap(t, spec.waitFor(t, BGMsgGameState))
	for _, key := range []string{"yourRole", "yourHand", "yourBangUsed"} {
		if _, leaked := specState[key]; leaked {
			t.Fatalf("관전자 스냅샷에 %s 유출: %v", key, specState)
		}
	}
	if seat := int(specState["yourSeat"].(float64)); seat != -1 {
		t.Fatalf("관전자 yourSeat = %d, want -1", seat)
	}
	for _, pRaw := range specState["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		if d := int(p["distanceFromYou"].(float64)); d != -1 {
			t.Fatalf("관전자에게 거리 %d 노출: %v", d, p)
		}
	}

	// 관전자는 어떤 행동도 할 수 없다
	spec.send(t, BGMessage{Type: BGMsgEndTurn})
	errMsg := bgPayloadMap(t, spec.waitFor(t, BGMsgError))
	if msg, _ := errMsg["message"].(string); msg != spectatorDeniedMsg {
		t.Fatalf("관전자 거절 문구 = %q", msg)
	}
}

// bgWaitPlaying 진행 중(대기 아님) 스냅샷이 올 때까지 소비
func (c *bgTestClient) bgWaitPlaying(t *testing.T) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "bg_game_state(playing)", func(m BGMessage) bool {
		if m.Type != BGMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		if !ok {
			return false
		}
		phase, _ := state["phase"].(string)
		return phase != string(BGPhaseWaiting)
	})
	return bgPayloadMap(t, msg)
}

// ==================== 방 코드 · 재접속 · 봇 대체 ====================

// TestBGRoomCodeAndRejoin 사설 방 코드로 둘이 모이고, 끊긴 뒤 세션으로
// 돌아오면 좌석이 그대로 이어진다
func TestBGRoomCodeAndRejoin(t *testing.T) {
	_, url, cleanup := newBGTestServer(t, 5*time.Second)
	defer cleanup()

	host := bgDial(t, url)
	defer host.conn.Close()
	hostJoined := bgJoin(t, host, "호스트", roomCodeNew)
	code, _ := hostJoined["roomCode"].(string)

	guest := bgDial(t, url)
	guestJoined := bgJoin(t, guest, "손님", strings.ToLower(code)) // 소문자도 받는다
	if got, _ := guestJoined["roomCode"].(string); got != code {
		t.Fatalf("손님 방 코드 = %q, want %q", got, code)
	}
	if seat := int(guestJoined["yourSeat"].(float64)); seat != 1 {
		t.Fatalf("손님 좌석 = %d", seat)
	}
	session, _ := guestJoined["sessionId"].(string)

	host.send(t, BGMessage{Type: BGMsgFillBots})
	bgDrainConn(host)
	guest.bgWaitPlaying(t)

	// 손님이 끊겼다가 세션으로 돌아온다
	guest.conn.Close()
	back := bgDial(t, url)
	defer back.conn.Close()
	back.send(t, BGMessage{Type: BGMsgRejoin, Payload: BGRejoinPayload{SessionID: session}})
	state := back.bgWaitPlaying(t)
	if seat := int(state["yourSeat"].(float64)); seat != 1 {
		t.Fatalf("재접속 좌석 = %d, want 1", seat)
	}
	if _, ok := state["yourHand"]; !ok {
		t.Fatalf("재접속 스냅샷에 손패가 없다: %v", state)
	}
}

// TestBGBotTakeover 유예가 만료되면 이탈 좌석을 봇이 이어받고 판은 계속된다
func TestBGBotTakeover(t *testing.T) {
	_, url, cleanup := newBGTestServer(t, 80*time.Millisecond)
	defer cleanup()

	host := bgDial(t, url)
	defer host.conn.Close()
	hostJoined := bgJoin(t, host, "호스트", roomCodeNew)
	code, _ := hostJoined["roomCode"].(string)

	guest := bgDial(t, url)
	bgJoin(t, guest, "이탈자", code)

	host.send(t, BGMessage{Type: BGMsgFillBots})
	host.bgWaitPlaying(t)

	guest.conn.Close()
	host.waitFor(t, BGMsgPlayerDisconnected)

	msg := host.waitMatch(t, "bot_takeover", func(m BGMessage) bool {
		if m.Type != BGMsgEvent {
			return false
		}
		ev, ok := m.Payload.(map[string]interface{})
		if !ok {
			return false
		}
		kind, _ := ev["kind"].(string)
		return kind == "bot_takeover"
	})
	ev := bgPayloadMap(t, msg)
	if text, _ := ev["message"].(string); !hasHangul(text) {
		t.Fatalf("봇 대체 문구 = %q", text)
	}
	if _, ok := ev["name"]; !ok {
		t.Fatalf("이벤트에 name 이 없다: %v", ev)
	}

	// 판은 계속 굴러 끝까지 간다
	bgAwaitGameOver(t, host, 120*time.Second, nil)
}

// TestBGReactAndJoinGuards 리액션 화이트리스트·연타 가드
func TestBGReactAndJoinGuards(t *testing.T) {
	_, url, cleanup := newBGTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	saved := reactRateLimit
	reactRateLimit = time.Millisecond
	defer func() { reactRateLimit = saved }()

	a := bgDial(t, url)
	defer a.conn.Close()
	joined := bgJoin(t, a, "A", roomCodeNew)
	code, _ := joined["roomCode"].(string)

	b := bgDial(t, url)
	defer b.conn.Close()
	bgJoin(t, b, "B", code)

	// 같은 연결의 재입장은 무시된다 (유령 좌석 방지)
	a.send(t, BGMessage{Type: BGMsgJoinGame, Payload: BGJoinGamePayload{Name: "A2", Room: code}})

	a.send(t, BGMessage{Type: BGMsgReact, Payload: BGReactPayload{Emoji: "🔥"}})
	msg := b.waitMatch(t, "react", func(m BGMessage) bool {
		if m.Type != BGMsgEvent {
			return false
		}
		ev, ok := m.Payload.(map[string]interface{})
		if !ok {
			return false
		}
		kind, _ := ev["kind"].(string)
		return kind == "react"
	})
	ev := bgPayloadMap(t, msg)
	if ev["message"] != "🔥" {
		t.Fatalf("리액션 = %v", ev["message"])
	}
	if _, ok := ev["seat"]; !ok {
		t.Fatalf("리액션에 seat 이 없다: %v", ev)
	}

	// 화이트리스트 밖은 조용히 무시된다 — 뒤이은 정상 리액션이 먼저 온다
	a.send(t, BGMessage{Type: BGMsgReact, Payload: BGReactPayload{Emoji: "💀"}})
	time.Sleep(10 * time.Millisecond)
	a.send(t, BGMessage{Type: BGMsgReact, Payload: BGReactPayload{Emoji: "👍"}})
	msg = b.waitMatch(t, "react2", func(m BGMessage) bool {
		if m.Type != BGMsgEvent {
			return false
		}
		ev, ok := m.Payload.(map[string]interface{})
		if !ok {
			return false
		}
		kind, _ := ev["kind"].(string)
		return kind == "react"
	})
	if bgPayloadMap(t, msg)["message"] != "👍" {
		t.Fatalf("금지 이모지가 통과했다")
	}
}

// TestBGStartGuards 4인 미만 시작 거절 · 호스트만 시작
func TestBGStartGuards(t *testing.T) {
	_, url, cleanup := newBGTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	a := bgDial(t, url)
	defer a.conn.Close()
	joined := bgJoin(t, a, "A", roomCodeNew)
	code, _ := joined["roomCode"].(string)

	a.send(t, BGMessage{Type: BGMsgStart})
	errMsg := bgPayloadMap(t, a.waitFor(t, BGMsgError))
	if msg, _ := errMsg["message"].(string); !strings.Contains(msg, "4명") {
		t.Fatalf("시작 거절 문구 = %q", msg)
	}

	b := bgDial(t, url)
	defer b.conn.Close()
	bgJoin(t, b, "B", code)
	b.send(t, BGMessage{Type: BGMsgStart}) // 호스트가 아니다
	errMsg = bgPayloadMap(t, b.waitFor(t, BGMsgError))
	if msg, _ := errMsg["message"].(string); !strings.Contains(msg, "호스트") {
		t.Fatalf("호스트 거절 문구 = %q", msg)
	}
}

// ==================== AFK 자동 진행 (단계별) ====================

// TestBGPhaseDeadlines 네 대기 상태의 마감 처리를 허브 경로로 직접 검증한다.
// 지나간 발화(옛 AfkSeq)는 무시돼야 한다.
func TestBGPhaseDeadlines(t *testing.T) {
	fire := func(h *BGHub, room *bgRoom) {
		h.handlePhaseFired(bgPhaseSignal{GameID: room.Game.ID, Seq: room.Game.AfkSeq})
	}

	t.Run("차례 — 자동 종료", func(t *testing.T) {
		h, room, _ := bgBotFixture(t, 5, 11)
		g := room.Game
		seat := g.CurrentSeat
		h.syncDeadline(room)
		h.stopPhaseTimer(room)
		fire(h, room)
		if g.CurrentSeat == seat && g.Phase == BGPhaseTurn {
			t.Fatalf("차례가 넘어가지 않았다 (seat%d)", seat)
		}
	})

	t.Run("대응 — 포기하고 체력 −1", func(t *testing.T) {
		h, room, _ := bgBotFixture(t, 5, 12)
		g := room.Game
		actor := g.CurrentSeat
		victim := g.nextAliveSeat(actor)
		bgHand(g, actor, bgCard(BGBang, BGSpade, "2"))
		bgHand(g, victim, bgCard(BGMiss, BGClub, "8"))
		g.Players[actor].Equipment = []BGCard{}
		g.Players[victim].Equipment = []BGCard{}
		hp := g.Players[victim].HP
		if err := g.Play(actor, 0, &victim, nil); err != nil {
			t.Fatalf("Play: %v", err)
		}
		if g.Phase != BGPhaseRespond {
			t.Fatalf("phase = %s", g.Phase)
		}
		h.syncDeadline(room)
		h.stopPhaseTimer(room)
		fire(h, room)
		if g.Players[victim].HP != hp-1 {
			t.Fatalf("체력 = %d, want %d", g.Players[victim].HP, hp-1)
		}
	})

	t.Run("잡화점 — 첫 장", func(t *testing.T) {
		h, room, _ := bgBotFixture(t, 5, 13)
		g := room.Game
		actor := g.CurrentSeat
		bgHand(g, actor, bgCard(BGStore, BGSpade, "9"))
		if err := g.Play(actor, 0, nil, nil); err != nil {
			t.Fatalf("Play: %v", err)
		}
		if g.Phase != BGPhaseStorePick {
			t.Fatalf("phase = %s", g.Phase)
		}
		before := len(g.StoreCards)
		h.syncDeadline(room)
		h.stopPhaseTimer(room)
		fire(h, room)
		if len(g.StoreCards) != before-1 {
			t.Fatalf("공개분 = %d장, want %d", len(g.StoreCards), before-1)
		}
	})

	t.Run("손패 줄이기 — 앞에서부터", func(t *testing.T) {
		h, room, _ := bgBotFixture(t, 5, 14)
		g := room.Game
		actor := g.CurrentSeat
		g.Players[actor].HP = 1
		bgHand(g, actor, bgCard(BGBang, BGSpade, "2"), bgCard(BGBeer, BGHeart, "3"),
			bgCard(BGMiss, BGClub, "4"))
		if err := g.EndTurn(actor); err != nil {
			t.Fatalf("EndTurn: %v", err)
		}
		if g.Phase != BGPhaseDiscard {
			t.Fatalf("phase = %s", g.Phase)
		}
		h.syncDeadline(room)
		h.stopPhaseTimer(room)
		fire(h, room)
		if len(g.Players[actor].Hand) != 1 {
			t.Fatalf("손패 = %d장, want 1", len(g.Players[actor].Hand))
		}
		if g.Players[actor].Hand[0].Kind != BGMiss {
			t.Fatalf("앞에서부터 버리지 않았다: %+v", g.Players[actor].Hand)
		}
	})

	t.Run("지나간 발화는 무시된다", func(t *testing.T) {
		h, room, _ := bgBotFixture(t, 5, 15)
		g := room.Game
		h.syncDeadline(room)
		h.stopPhaseTimer(room)
		stale := bgPhaseSignal{GameID: g.ID, Seq: g.AfkSeq - 1}
		seat, phase := g.CurrentSeat, g.Phase
		h.handlePhaseFired(stale)
		if g.CurrentSeat != seat || g.Phase != phase {
			t.Fatalf("옛 발화가 판을 밀었다 (seat%d %s → seat%d %s)",
				seat, phase, g.CurrentSeat, g.Phase)
		}
	})
}

// TestBGWireContract 스냅샷 봉투와 필드 이름이 프론트 계약과 맞는지.
// 카드 kind 는 영문 고정, 이름표는 서버가 내려주지 않는다(화면 표기만 한국어).
func TestBGWireContract(t *testing.T) {
	h, room, _ := bgBotFixture(t, 5, 4242)
	raw := bgRawState(t, h, room, 0)

	for _, key := range []string{
		`"gameId"`, `"roomCode"`, `"phase"`, `"hostSeat"`, `"yourSeat"`,
		`"spectators"`, `"endsAt"`, `"currentSeat"`, `"deckLeft"`, `"discardTop"`,
		`"pending"`, `"storeCards"`, `"players"`, `"lastAction"`, `"result"`,
	} {
		if !strings.Contains(raw, key) {
			t.Fatalf("스냅샷에 %s 가 없다:\n%s", key, raw)
		}
	}
	// 대응 창의 내부 큐는 와이어에 실리지 않는다
	if strings.Contains(raw, `"Queue"`) || strings.Contains(raw, `"queue"`) {
		t.Fatalf("pending 내부 큐가 새어 나갔다:\n%s", raw)
	}

	// 카드 kind 는 영문 고정
	hand := *h.buildBGState(room, 0).YourHand
	for _, c := range hand {
		if _, ok := bgDef(c.Kind); !ok {
			t.Fatalf("알 수 없는 kind: %s", c.Kind)
		}
		if hasHangul(string(c.Kind)) {
			t.Fatalf("kind 에 한글이 들어갔다: %s", c.Kind)
		}
		if c.Suit == "" || c.Rank == "" {
			t.Fatalf("무늬·숫자 없는 카드: %+v", c)
		}
	}

	// 대응 창 payload 모양
	g := room.Game
	actor := g.CurrentSeat
	victim := g.nextAliveSeat(actor)
	g.Players[actor].Equipment = []BGCard{}
	g.Players[victim].Equipment = []BGCard{}
	bgHand(g, actor, bgCard(BGBang, BGSpade, "2"))
	bgHand(g, victim, bgCard(BGMiss, BGClub, "8"))
	if err := g.Play(actor, 0, &victim, nil); err != nil {
		t.Fatalf("Play: %v", err)
	}
	raw = bgRawState(t, h, room, victim)
	for _, key := range []string{`"kind":"bang"`, `"bySeat"`, `"targetSeat"`,
		`"need":"miss"`, `"passed":[`} {
		if !strings.Contains(raw, key) {
			t.Fatalf("pending 에 %s 가 없다:\n%s", key, raw)
		}
	}
}

// TestBGSpectatorLimit 관전 정원
func TestBGSpectatorLimit(t *testing.T) {
	_, url, cleanup := newBGTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := bgDial(t, url)
	defer host.conn.Close()
	joined := bgJoin(t, host, "호스트", roomCodeNew)
	code, _ := joined["roomCode"].(string)
	host.send(t, BGMessage{Type: BGMsgFillBots})
	host.bgWaitPlaying(t)
	bgDrainConn(host)

	conns := []*websocket.Conn{}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < maxSpectators; i++ {
		s := bgDial(t, url)
		conns = append(conns, s.conn)
		s.send(t, BGMessage{Type: BGMsgJoinGame,
			Payload: BGJoinGamePayload{Name: "구경꾼", Room: code}})
		s.waitFor(t, BGMsgSpectateJoined)
		bgDrainConn(s)
	}

	over := bgDial(t, url)
	conns = append(conns, over.conn)
	over.send(t, BGMessage{Type: BGMsgJoinGame,
		Payload: BGJoinGamePayload{Name: "늦은구경꾼", Room: code}})
	errMsg := bgPayloadMap(t, over.waitFor(t, BGMsgError))
	if msg, _ := errMsg["message"].(string); msg != spectatorFullMsg {
		t.Fatalf("정원 초과 문구 = %q", msg)
	}
}
