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

// 테스트에서는 대기 상태 마감과 봇의 생각 시간을 짧게 낮춘다
// (단서 60초·추리 60초·인정 15초·정산 5초는 실사용 값). 값은 init 에서 한 번만
// 정한다 — 테스트 도중에 바꾸면 허브 고루틴과 경합한다(-race).
func init() {
	joClueTimeout = 150 * time.Millisecond
	joGuessTimeout = 150 * time.Millisecond
	joAcceptWindow = 60 * time.Millisecond
	joRoundEndDelay = 20 * time.Millisecond

	joBotClueDelay = 2 * time.Millisecond
	joBotClueJitterMs = 3
	joBotGuessDelay = 2 * time.Millisecond
	joBotGuessJitterMs = 3
	joBotAcceptDelay = 2 * time.Millisecond
	joBotAcceptJitterMs = 3
}

// joTestClient 공용 testConn 에 저스트 원 메시지 타입의 waitFor 를 얹은 래퍼
type joTestClient struct {
	testConn[JOMessage]
}

func newJOTestServer(t *testing.T, grace time.Duration) (*JOHub, string, func()) {
	t.Helper()
	hub := NewJOHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeJOWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func joDial(t *testing.T, url string) *joTestClient {
	t.Helper()
	return &joTestClient{dialWS[JOMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *joTestClient) waitFor(t *testing.T, msgType JOMessageType) JOMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m JOMessage) bool { return m.Type == msgType })
}

func joPayloadMap(t *testing.T, msg JOMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// joJoin 입장하고 jo_player_joined payload 를 돌려준다
func joJoin(t *testing.T, c *joTestClient, name, room string) map[string]interface{} {
	t.Helper()
	c.send(t, JOMessage{Type: JOMsgJoinGame, Payload: JOJoinGamePayload{Name: name, Room: room}})
	return joPayloadMap(t, c.waitFor(t, JOMsgPlayerJoined))
}

// joWaitPhase 지정한 phase 의 스냅샷이 올 때까지 소비
func (c *joTestClient) joWaitPhase(t *testing.T, phase string) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "jo_game_state("+phase+")", func(m JOMessage) bool {
		if m.Type != JOMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		return ok && state["phase"] == phase
	})
	return joPayloadMap(t, msg)
}

// joDrain 배경으로 소켓을 비운다 (읽지 않는 연결의 버퍼 포화 방지)
func joDrain(c *joTestClient) {
	conn := c.conn
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

// joSeatClients 허브 고루틴 없이 핸들러를 직접 부르는 결정적 테스트용 —
// 소켓 없는 사람 좌석 n개를 앉힌 방을 만든다
func joSeatClients(t *testing.T, h *JOHub, room *joRoom, n int) []*JOClient {
	t.Helper()
	clients := make([]*JOClient, n)
	for i := range clients {
		c := &JOClient{wsClient: newBotWSClient(), Hub: h}
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

// joRawState 좌석 시점 스냅샷의 raw JSON 과 최상위 키 집합.
// "키 자체 부재"를 문자열 포함이 아니라 최상위 키로 검사한다
// (history 안에도 word 필드가 있으므로 문자열 검사만으로는 부족하다).
func joRawState(t *testing.T, h *JOHub, room *joRoom, viewer int) (string, map[string]json.RawMessage) {
	t.Helper()
	data, err := json.Marshal(h.buildJOState(room, viewer))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return string(data), keys
}

// TestJOFourBotsCompleteGame 봇을 채운 4인 협력전이 60초 안에 완주하는지 —
// 가장 중요한 회귀 장치. 좌석 0은 서버 연습봇 두뇌(joBrain)를 WS 로 감싼
// 드라이버가 잡는다. 드라이버는 사람 좌석이라 자기가 출제자인 라운드에는
// 제시어를 못 받고 넘긴다 (은닉이 지켜진다는 방증이기도 하다).
func TestJOFourBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newJOTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := joDial(t, url)
	defer c.conn.Close()
	joJoin(t, c, "진행자", "")
	c.send(t, JOMessage{Type: JOMsgFillBots}) // 4인까지 채우고 즉시 시작

	start := time.Now()
	brain := newJOBrain()
	wantRounds := JOFillBotTarget * JORoundsPerPlayer
	deadline := start.Add(60 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "state-or-over", func(m JOMessage) bool {
			return m.Type == JOMsgGameState || m.Type == JOMsgGameOver
		})
		if msg.Type == JOMsgGameOver {
			over := joPayloadMap(t, msg)
			cleared, ok := over["cleared"].(bool)
			if !ok {
				t.Fatalf("종료 payload 에 cleared 부재: %v", over)
			}
			score := int(over["score"].(float64))
			total := int(over["totalRounds"].(float64))
			if total != wantRounds {
				t.Fatalf("totalRounds = %d, want %d", total, wantRounds)
			}
			if score < 0 || score > total {
				t.Fatalf("score = %d (0~%d)", score, total)
			}
			if cleared != (score*2 >= total) {
				t.Fatalf("성공 판정 불일치: cleared=%t score=%d/%d", cleared, score, total)
			}
			if grade, _ := over["grade"].(string); grade == "" {
				t.Fatalf("등급 부재: %v", over)
			}
			if m, _ := over["message"].(string); m == "" {
				t.Fatalf("종료 문구 부재: %v", over)
			}
			history := over["history"].([]interface{})
			if len(history) != wantRounds {
				t.Fatalf("history 길이 = %d, want %d", len(history), wantRounds)
			}
			correct := 0
			for i, raw := range history {
				h := raw.(map[string]interface{})
				if int(h["round"].(float64)) != i+1 {
					t.Fatalf("history 순서 이상: %v", history)
				}
				if w, _ := h["word"].(string); w == "" {
					t.Fatalf("history 에 제시어 부재: %v", h)
				}
				if h["correct"] == true {
					correct++
				}
			}
			// 점수는 정답 +1 / 오답 -1(0 하한) / 넘김 0 이라 정답 수 이하이고,
			// 정답이 아닌 라운드 수만큼만 깎일 수 있다
			if score > correct || correct-score > total-correct {
				t.Fatalf("정답 라운드 %d 와 총점 %d 가 어긋난다 (%d라운드)", correct, score, total)
			}
			players := over["players"].([]interface{})
			if len(players) != JOFillBotTarget {
				t.Fatalf("players 길이 = %d, want %d", len(players), JOFillBotTarget)
			}
			t.Logf("완주: cleared=%t score=%d/%d (%.1fs)",
				cleared, score, total, time.Since(start).Seconds())
			return
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("60초 안에 게임이 끝나지 않았다 — 진행 불가 상태")
}

// TestJOHiddenState 은닉의 핵심 계약 — 허브 고루틴 없이 핸들러를 직접 불러
// 결정적으로 검증한다.
//
//	· word     : 단서 제공자에게만. 출제자·관전자의 raw JSON 에는 키 자체 부재
//	· yourClue : 본인에게만
//	· clues    : 단서 단계에는 항상 빈 배열 — 남의 단서가 절대 새지 않는다
func TestJOHiddenState(t *testing.T) {
	h := NewJOHub()
	room := h.lobbyRoomFor("")
	clients := joSeatClients(t, h, room, 4)
	h.startGame(room)

	game := room.Game
	game.Word = "사과" // 결정적 구도 (출제자는 seat0)
	if game.GuesserSeat != 0 || game.Phase != JOPhaseClue {
		t.Fatalf("시작 상태 = guesser%d %s", game.GuesserSeat, game.Phase)
	}

	// ---- 단서 단계: word 는 단서 제공자만, clues 는 전원 빈 배열 ----
	for _, viewer := range []int{1, 2, 3} {
		raw, keys := joRawState(t, h, room, viewer)
		if string(keys["word"]) != `"사과"` {
			t.Fatalf("seat%d 에게 제시어가 안 보인다:\n%s", viewer, raw)
		}
		if string(keys["yourClue"]) != `""` {
			t.Fatalf("seat%d 의 yourClue = %s (미제출은 빈 문자열)", viewer, keys["yourClue"])
		}
		if strings.Count(raw, `"yourClue"`) != 1 {
			t.Fatalf("yourClue 키가 본인 것 하나가 아니다:\n%s", raw)
		}
		if string(keys["clues"]) != `[]` {
			t.Fatalf("단서 단계 clues = %s, want []", keys["clues"])
		}
	}
	for _, viewer := range []int{0, -1} { // 출제자·관전자
		raw, keys := joRawState(t, h, room, viewer)
		if _, ok := keys["word"]; ok {
			t.Fatalf("viewer %d 에게 word 키 유출:\n%s", viewer, raw)
		}
		if _, ok := keys["yourClue"]; ok {
			t.Fatalf("viewer %d 에게 yourClue 키 유출:\n%s", viewer, raw)
		}
		if strings.Contains(raw, "사과") {
			t.Fatalf("viewer %d 의 스냅샷 어디에도 제시어가 있으면 안 된다:\n%s", viewer, raw)
		}
		if string(keys["clues"]) != `[]` {
			t.Fatalf("viewer %d 의 단서 단계 clues = %s, want []", viewer, keys["clues"])
		}
	}

	// ---- 남의 단서는 단서 단계에 절대 새지 않는다 ----
	h.handleGameMessage(JOGameMessage{Client: clients[1], Message: JOMessage{
		Type: JOMsgClue, Payload: JOCluePayload{Text: "빨강"}}})
	h.handleGameMessage(JOGameMessage{Client: clients[2], Message: JOMessage{
		Type: JOMsgClue, Payload: JOCluePayload{Text: "빨강"}}})
	if game.Phase != JOPhaseClue || game.SubmittedCount() != 2 {
		t.Fatalf("2명 제출 후 = %s (%d명)", game.Phase, game.SubmittedCount())
	}
	for _, viewer := range []int{0, 3, -1} {
		raw, _ := joRawState(t, h, room, viewer)
		if strings.Contains(raw, "빨강") {
			t.Fatalf("viewer %d 에게 남의 단서가 샜다:\n%s", viewer, raw)
		}
	}
	raw1, keys1 := joRawState(t, h, room, 1)
	if string(keys1["yourClue"]) != `"빨강"` {
		t.Fatalf("본인 단서가 안 보인다: %s", keys1["yourClue"])
	}
	if strings.Count(raw1, "빨강") != 1 { // yourClue 딱 한 곳
		t.Fatalf("본인 스냅샷에 단서가 두 번 이상 실렸다:\n%s", raw1)
	}
	// 출제자는 단서를 낼 수 없다 (에러 경로에서도 상태 불변)
	h.handleGameMessage(JOGameMessage{Client: clients[0], Message: JOMessage{
		Type: JOMsgClue, Payload: JOCluePayload{Text: "몰래"}}})
	if game.SubmittedCount() != 2 {
		t.Fatalf("출제자의 단서가 통과했다 (%d명)", game.SubmittedCount())
	}

	// ---- 추리 단계: 살아남은 단서만 공개, 소거된 단서는 아직 숨긴다 ----
	h.handleGameMessage(JOGameMessage{Client: clients[3], Message: JOMessage{
		Type: JOMsgClue, Payload: JOCluePayload{Text: "동그라미"}}})
	if game.Phase != JOPhaseGuess {
		t.Fatalf("전원 제출 후 phase = %s", game.Phase)
	}
	for _, viewer := range []int{0, 3, -1} {
		raw, _ := joRawState(t, h, room, viewer)
		if !strings.Contains(raw, "동그라미") {
			t.Fatalf("viewer %d 에게 살아남은 단서가 안 보인다:\n%s", viewer, raw)
		}
		if strings.Contains(raw, "빨강") {
			t.Fatalf("viewer %d 에게 소거된 단서가 판정 전에 보인다:\n%s", viewer, raw)
		}
	}
	if _, keys := joRawState(t, h, room, 0); len(keys["word"]) != 0 {
		t.Fatalf("추리 단계에도 출제자에게 word 키가 있으면 안 된다: %s", keys["word"])
	}

	// ---- 인정 창 → 인정 → round_end: 소거된 단서가 취소선용으로 함께 공개 ----
	h.handleGameMessage(JOGameMessage{Client: clients[0], Message: JOMessage{
		Type: JOMsgGuess, Payload: JOGuessPayload{Text: "포도"}}})
	if game.Phase != JOPhaseJudging {
		t.Fatalf("오답 후 phase = %s", game.Phase)
	}
	h.handleGameMessage(JOGameMessage{Client: clients[1], Message: JOMessage{Type: JOMsgAccept}})
	if game.Phase != JOPhaseRoundEnd || game.Score != 1 {
		t.Fatalf("인정 후 = %s score=%d", game.Phase, game.Score)
	}

	for _, viewer := range []int{0, 1, -1} {
		raw, keys := joRawState(t, h, room, viewer)
		if !strings.Contains(raw, "빨강") || !strings.Contains(raw, "동그라미") {
			t.Fatalf("viewer %d 의 정산 화면에 단서가 다 안 보인다:\n%s", viewer, raw)
		}
		if !strings.Contains(raw, `"removed":true`) {
			t.Fatalf("viewer %d 에게 소거 표시가 없다:\n%s", viewer, raw)
		}
		// 끝난 라운드의 제시어는 history 로 공개된다 — 최상위 word 키는 그래도 없다
		if !strings.Contains(string(keys["history"]), `"word":"사과"`) {
			t.Fatalf("viewer %d 의 history 에 제시어가 없다: %s", viewer, keys["history"])
		}
		if viewer != 1 {
			if _, ok := keys["word"]; ok {
				t.Fatalf("viewer %d 에게 word 키 유출:\n%s", viewer, raw)
			}
		}
	}

	// ---- 연습봇 좌석 예외는 사람 스냅샷을 건드리지 않는다 ----
	botView := h.buildJOStateFor(room, 0, true)
	if botView.Word == nil || *botView.Word != "사과" {
		t.Fatalf("출제자 봇 시뮬레이션에 제시어가 실리지 않았다: %v", botView.Word)
	}
	if h.buildJOState(room, 0).Word != nil {
		t.Fatal("사람 출제자 스냅샷에 제시어가 실렸다")
	}

	// ---- 관전자 스냅샷은 패닉 없이 빌드되고 공개 정보만 담는다 ----
	spec := h.buildJOState(room, -1)
	if spec.YourSeat != -1 || spec.Word != nil || spec.YourClue != nil {
		t.Fatalf("관전자 스냅샷: yourSeat=%d word=%v yourClue=%v",
			spec.YourSeat, spec.Word, spec.YourClue)
	}
	if spec.TotalRounds != 8 || spec.Score != 1 || spec.GuesserSeat != 0 {
		t.Fatalf("공개 정보 이상: %d라운드 score=%d guesser=%d",
			spec.TotalRounds, spec.Score, spec.GuesserSeat)
	}

	// ---- 빈 대기실 스냅샷도 빈 배열 [] (null 금지) ----
	empty := h.lobbyRoomFor("ZZZZ")
	emptyRaw, _ := json.Marshal(h.buildJOState(empty, -1))
	for _, want := range []string{
		`"players":[]`, `"clues":[]`, `"history":[]`, `"judged":null`,
		`"guesserSeat":-1`, `"yourSeat":-1`, `"phase":"waiting"`,
	} {
		if !strings.Contains(string(emptyRaw), want) {
			t.Fatalf("빈 대기실 스냅샷에 %s 부재:\n%s", want, emptyRaw)
		}
	}
	if strings.Contains(string(emptyRaw), `"word"`) {
		t.Fatalf("대기실 스냅샷에 word 키:\n%s", emptyRaw)
	}
}

// TestJOAfkAutoProgress 접속만 유지한 채 아무도 응답하지 않는 3인전 —
// 빈 단서 마감·자동 넘김·라운드 자동 진행만으로 완주하는지
// (endsAt 노출·afk 이벤트 포함)
func TestJOAfkAutoProgress(t *testing.T) {
	_, url, cleanup := newJOTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	conns := make([]*joTestClient, JOMinPlayers)
	for i := range conns {
		conns[i] = joDial(t, url)
		defer conns[i].conn.Close()
		joJoin(t, conns[i], fmt.Sprintf("잠수%d", i), "")
	}
	host := conns[0]
	host.send(t, JOMessage{Type: JOMsgStart})

	state := host.joWaitPhase(t, string(JOPhaseClue))
	if ends := int64(state["endsAt"].(float64)); ends <= 0 {
		t.Fatalf("단서 스냅샷의 endsAt = %d, want unixMillis", ends)
	}
	if int(state["round"].(float64)) != 1 ||
		int(state["totalRounds"].(float64)) != JOMinPlayers*JORoundsPerPlayer {
		t.Fatalf("시작 스냅샷 = %v/%v 라운드", state["round"], state["totalRounds"])
	}
	if int(state["guesserSeat"].(float64)) != 0 {
		t.Fatalf("첫 출제자 = %v, want 0", state["guesserSeat"])
	}
	if int(state["score"].(float64)) != 0 || int(state["submittedCount"].(float64)) != 0 {
		t.Fatalf("시작 점수·제출 수 = %v / %v", state["score"], state["submittedCount"])
	}
	if clues, ok := state["clues"].([]interface{}); !ok || len(clues) != 0 {
		t.Fatalf("단서 단계 clues = %v (빈 배열이어야 한다)", state["clues"])
	}
	if history, ok := state["history"].([]interface{}); !ok || len(history) != 0 {
		t.Fatalf("시작 history = %v (빈 배열이어야 한다)", state["history"])
	}
	if _, leaked := state["word"]; leaked { // seat0 은 출제자
		t.Fatalf("출제자에게 제시어 유출: %v", state["word"])
	}
	if state["judged"] != nil {
		t.Fatalf("시작 judged = %v", state["judged"])
	}
	for _, pRaw := range state["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		if p["submitted"] == true {
			t.Fatalf("시작 제출 표시 = %v", p)
		}
		if _, ok := p["isGuesser"].(bool); !ok {
			t.Fatalf("좌석에 isGuesser 부재: %v", p)
		}
	}

	// 나머지는 더 읽지 않는다 — 백그라운드로 비워 버퍼 포화만 막는다
	for _, c := range conns[1:] {
		joDrain(c)
	}

	sawAfk := false
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		msg := host.waitMatch(t, "event-or-over", func(m JOMessage) bool {
			return m.Type == JOMsgEvent || m.Type == JOMsgGameOver
		})
		if msg.Type == JOMsgEvent {
			ev := joPayloadMap(t, msg)
			if ev["kind"] == "afk" {
				if !strings.Contains(ev["message"].(string), "자동") &&
					!strings.Contains(ev["message"].(string), "빈 단서") {
					t.Fatalf("afk 문구 = %v", ev["message"])
				}
				sawAfk = true
			}
			continue
		}
		over := joPayloadMap(t, msg)
		if !sawAfk {
			t.Fatal("afk 자동 진행 이벤트가 한 번도 없었다")
		}
		if over["cleared"] != false || int(over["score"].(float64)) != 0 {
			t.Fatalf("전원 방치인데 결과 = %v", over)
		}
		return
	}
	t.Fatal("전원 방치 게임이 45초 안에 끝나지 않았다")
}

// TestJORoomCodeAndSpectate 방 코드 발급·코드 입장·시작 후 코드 관전 진입.
// 관전자 스냅샷은 yourSeat -1 에 word·yourClue 부재고, 행동은 전부 차단된다.
func TestJORoomCodeAndSpectate(t *testing.T) {
	_, url, cleanup := newJOTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := joDial(t, url)
	defer host.conn.Close()
	joined := joJoin(t, host, "호스트", "NEW")
	code, _ := joined["roomCode"].(string)
	if len(code) != roomCodeLen {
		t.Fatalf("roomCode = %q, want %d자", code, roomCodeLen)
	}

	guests := make([]*joTestClient, JOMinPlayers-1)
	for i := range guests {
		guests[i] = joDial(t, url)
		defer guests[i].conn.Close()
		g := joJoin(t, guests[i], fmt.Sprintf("동료%d", i), code)
		if g["roomCode"] != code || int(g["yourSeat"].(float64)) != i+1 {
			t.Fatalf("코드 입장 실패: %v", g)
		}
	}

	host.send(t, JOMessage{Type: JOMsgStart})
	state := host.joWaitPhase(t, string(JOPhaseClue))
	if state["roomCode"] != code || len(state["players"].([]interface{})) != JOMinPlayers {
		t.Fatalf("시작 실패: %v", state["players"])
	}
	// 호스트(seat0)는 첫 출제자라 제시어가 없다 — 동료 좌석에는 있어야 한다
	guestState := guests[0].joWaitPhase(t, string(JOPhaseClue))
	if w, ok := guestState["word"].(string); !ok || w == "" {
		t.Fatalf("단서 제공자에게 제시어 부재: %v", guestState)
	}
	if _, ok := guestState["yourClue"].(string); !ok {
		t.Fatalf("단서 제공자에게 yourClue 부재: %v", guestState)
	}
	for _, c := range guests {
		joDrain(c)
	}

	// 시작된 방의 코드로 들어오면 관전자
	spec := joDial(t, url)
	defer spec.conn.Close()
	spec.send(t, JOMessage{Type: JOMsgJoinGame, Payload: JOJoinGamePayload{Name: "구경꾼", Room: code}})
	specJoined := joPayloadMap(t, spec.waitFor(t, JOMsgSpectateJoined))
	if specJoined["roomCode"] != code {
		t.Fatalf("관전 입장 roomCode = %v", specJoined["roomCode"])
	}

	specState := joPayloadMap(t, spec.waitFor(t, JOMsgGameState))
	if int(specState["yourSeat"].(float64)) != -1 {
		t.Fatalf("관전자 yourSeat = %v, want -1", specState["yourSeat"])
	}
	if leaked, ok := specState["word"]; ok {
		t.Fatalf("관전자에게 제시어 유출: %v", leaked)
	}
	if leaked, ok := specState["yourClue"]; ok {
		t.Fatalf("관전자에게 yourClue 유출: %v", leaked)
	}
	if _, ok := specState["clues"].([]interface{}); !ok {
		t.Fatalf("관전자 clues = %v (배열이어야 한다)", specState["clues"])
	}
	if int(specState["spectators"].(float64)) != 1 {
		t.Fatalf("관전자 수 = %v", specState["spectators"])
	}
	if int(specState["totalRounds"].(float64)) != JOMinPlayers*JORoundsPerPlayer {
		t.Fatalf("관전자에게 총 라운드가 안 보인다: %v", specState["totalRounds"])
	}

	// 관전자는 어떤 행동도 못 한다
	spec.send(t, JOMessage{Type: JOMsgClue, Payload: JOCluePayload{Text: "몰래"}})
	errPayload := joPayloadMap(t, spec.waitFor(t, JOMsgError))
	if errPayload["message"] != spectatorDeniedMsg {
		t.Fatalf("관전자 차단 문구 = %v", errPayload["message"])
	}
}

// TestJOReconnect 재접속 3종 — 이탈 통지(jo_player_disconnected) 후 세션으로
// 돌아오면 좌석이 그대로 복원되고(jo_player_reconnected), 모르는 세션은
// jo_session_expired 로 거절된다.
func TestJOReconnect(t *testing.T) {
	_, url, cleanup := newJOTestServer(t, 3*time.Second)
	defer cleanup()

	conns := make([]*joTestClient, JOMinPlayers)
	sessions := make([]string, JOMinPlayers)
	for i := range conns {
		conns[i] = joDial(t, url)
		joined := joJoin(t, conns[i], fmt.Sprintf("P%d", i), "")
		sessions[i], _ = joined["sessionId"].(string)
	}
	defer conns[0].conn.Close()
	conns[0].send(t, JOMessage{Type: JOMsgStart})
	conns[0].joWaitPhase(t, string(JOPhaseClue))
	for _, c := range conns[2:] {
		joDrain(c)
	}

	// 좌석 1 이탈 → 남은 사람에게 이탈 통지
	conns[1].conn.Close()
	discon := joPayloadMap(t, conns[0].waitFor(t, JOMsgPlayerDisconnected))
	if int(discon["seat"].(float64)) != 1 || discon["name"] != "P1" {
		t.Fatalf("이탈 통지 = %v", discon)
	}
	if int(discon["graceSeconds"].(float64)) <= 0 {
		t.Fatalf("graceSeconds = %v", discon["graceSeconds"])
	}

	// 세션으로 재접속 → 좌석 복원
	back := joDial(t, url)
	defer back.conn.Close()
	back.send(t, JOMessage{Type: JOMsgRejoin, Payload: JORejoinPayload{SessionID: sessions[1]}})
	recon := joPayloadMap(t, back.waitFor(t, JOMsgPlayerReconnected))
	if int(recon["seat"].(float64)) != 1 || recon["name"] != "P1" {
		t.Fatalf("재접속 통지 = %v", recon)
	}
	restored := joPayloadMap(t, back.waitFor(t, JOMsgGameState))
	if int(restored["yourSeat"].(float64)) != 1 {
		t.Fatalf("복원 yourSeat = %v", restored["yourSeat"])
	}
	if int(restored["round"].(float64)) < 1 {
		t.Fatalf("복원 스냅샷 라운드 = %v", restored["round"])
	}
	players := restored["players"].([]interface{})
	if p := players[1].(map[string]interface{}); p["connected"] != true {
		t.Fatalf("복원 좌석 접속 상태 = %v", p)
	}

	// 모르는 세션은 만료 처리
	ghost := joDial(t, url)
	defer ghost.conn.Close()
	ghost.send(t, JOMessage{Type: JOMsgRejoin, Payload: JORejoinPayload{SessionID: "없는-세션"}})
	ghost.waitFor(t, JOMsgSessionExpired)
}

// TestJOBotTakeover 유예 만료 좌석을 봇이 이어받아 게임이 멈추지 않는지
func TestJOBotTakeover(t *testing.T) {
	_, url, cleanup := newJOTestServer(t, 120*time.Millisecond)
	defer cleanup()

	conns := make([]*joTestClient, JOMinPlayers)
	for i := range conns {
		conns[i] = joDial(t, url)
		defer conns[i].conn.Close()
		joJoin(t, conns[i], fmt.Sprintf("P%d", i), "")
	}
	conns[0].send(t, JOMessage{Type: JOMsgStart})
	conns[0].joWaitPhase(t, string(JOPhaseClue))
	for _, c := range conns[2:] {
		joDrain(c)
	}

	// 좌석 1 이탈 → 유예 만료 → 봇 대체
	conns[1].conn.Close()
	sawTakeover := false
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		msg := conns[0].waitMatch(t, "event-or-over", func(m JOMessage) bool {
			return m.Type == JOMsgEvent || m.Type == JOMsgGameOver
		})
		if msg.Type == JOMsgEvent {
			ev := joPayloadMap(t, msg)
			if ev["kind"] == "bot_takeover" {
				if int(ev["seat"].(float64)) != 1 {
					t.Fatalf("봇 대체 좌석 = %v, want 1", ev["seat"])
				}
				if ev["name"] == nil || ev["name"] == "" {
					t.Fatalf("봇 대체 이벤트에 name 부재: %v", ev)
				}
				sawTakeover = true
			}
			continue
		}
		if !sawTakeover {
			t.Fatal("봇 대체 없이 게임이 끝났다")
		}
		over := joPayloadMap(t, msg)
		if _, ok := over["cleared"].(bool); !ok {
			t.Fatalf("종료 payload = %v", over)
		}
		return
	}
	t.Fatal("봇 대체 후 게임이 45초 안에 끝나지 않았다")
}

// TestJOCoopMatchRecord 협력 전적 표기 — 성공(총점이 라운드의 절반 이상)이면
// 전원이 Winner 에 들어가고, 실패면 어떤 닉네임과도 겹치지 않는 표식이 들어가
// 전원 패자로 집계된다 (Winner "" 는 전적 장부에서 무승부라 쓰면 안 된다).
func TestJOCoopMatchRecord(t *testing.T) {
	team := []string{"가", "나", "다", "라"}
	players := strings.Join(team, "·")

	// 성공 — 전원 승자
	winners := map[string]bool{}
	for _, name := range splitWinners(players) {
		winners[name] = true
	}
	for _, name := range splitPlayers(players) {
		if !winners[name] {
			t.Fatalf("성공인데 %s 가 승자가 아니다", name)
		}
	}

	// 실패 — 전원 패자 (무승부 아님)
	if joFailWinnerTag == "" {
		t.Fatal("실패 표기가 빈 문자열이면 무승부로 집계된다")
	}
	lost := splitWinners(joFailWinnerTag)
	if len(lost) == 0 {
		t.Fatalf("실패 표기 파싱 = %v", lost)
	}
	for _, name := range splitPlayers(players) {
		for _, w := range lost {
			if w == name {
				t.Fatalf("실패 표기가 참가자 닉네임 %s 와 겹친다", name)
			}
		}
	}

	// 성공 기준 — 8라운드에서 4점이면 성공, 3점이면 실패
	if !joSuccess(4, 8) || joSuccess(3, 8) {
		t.Fatal("성공 기준(총점 ≥ 라운드/2)이 어긋났다")
	}
}
