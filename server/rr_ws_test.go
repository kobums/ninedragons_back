package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"
)

// 테스트에서는 단계 마감과 봇의 뜸 들이는 시간을 짧게 낮춘다 (60초 카운트다운·
// 5분 목표 상한·2.5초 뜸은 실사용 값). 값은 init 에서 한 번만 정한다 —
// 테스트 도중에 바꾸면 허브·봇 고루틴과 경합한다(-race). 허브의 시간 설정은
// 필드라 테스트마다 따로 정한다.
func init() {
	rrBotThinkBase = 30 * time.Millisecond
	rrBotThinkPerMove = 15 * time.Millisecond
	rrBotImproveDelay = 70 * time.Millisecond
	rrBotDemoDelay = 15 * time.Millisecond
	rrBotIdleTick = 15 * time.Millisecond
	rrBotSettleWait = 600 * time.Millisecond
	rrBotMinWait = 5 * time.Millisecond
}

// rrTestTimings 허브 시간 설정 묶음 (Run 전에 정한다)
type rrTestTimings struct {
	bidWindow    time.Duration
	goalCap      time.Duration
	demoCap      time.Duration
	goalEndDelay time.Duration
	gameCap      time.Duration
}

// rrFastTimings 봇이 17개 목표를 빠르게 소진하는 설정
func rrFastTimings() rrTestTimings {
	return rrTestTimings{
		bidWindow:    250 * time.Millisecond,
		goalCap:      4 * time.Second,
		demoCap:      1500 * time.Millisecond,
		goalEndDelay: 60 * time.Millisecond,
		gameCap:      5 * time.Minute,
	}
}

// rrTestClient 공용 testConn 에 리코셰 메시지 타입의 waitFor 를 얹은 래퍼
type rrTestClient struct {
	testConn[RRMessage]
}

func newRRTestServer(t *testing.T, grace time.Duration, tm rrTestTimings) (*RRHub, string, func()) {
	t.Helper()
	hub := NewRRHub()
	hub.grace = grace
	hub.bidWindow = tm.bidWindow
	hub.goalCap = tm.goalCap
	hub.demoCap = tm.demoCap
	hub.goalEndDelay = tm.goalEndDelay
	hub.gameCap = tm.gameCap
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeRRWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func rrDial(t *testing.T, url string) *rrTestClient {
	t.Helper()
	return &rrTestClient{dialWS[RRMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *rrTestClient) waitFor(t *testing.T, msgType RRMessageType) RRMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m RRMessage) bool { return m.Type == msgType })
}

func rrPayloadMap(t *testing.T, msg RRMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// rrJoin 입장하고 rr_player_joined payload 를 돌려준다
func rrJoin(t *testing.T, c *rrTestClient, name, room string) map[string]interface{} {
	t.Helper()
	c.send(t, RRMessage{Type: RRMsgJoinGame, Payload: RRJoinGamePayload{Name: name, Room: room}})
	return rrPayloadMap(t, c.waitFor(t, RRMsgPlayerJoined))
}

// rrWaitPhase 지정한 phase 의 스냅샷이 올 때까지 소비
func (c *rrTestClient) rrWaitPhase(t *testing.T, phase string) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "rr_game_state("+phase+")", func(m RRMessage) bool {
		if m.Type != RRMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		return ok && state["phase"] == phase
	})
	return rrPayloadMap(t, msg)
}

// rrDrain 배경으로 소켓을 비운다 (읽지 않는 연결의 버퍼 포화 방지)
func rrDrain(c *rrTestClient) {
	conn := c.conn
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

// rrSeatClients 허브 고루틴 없이 핸들러를 직접 부르는 결정적 테스트용 —
// 소켓 없는 사람 좌석 n개를 앉힌 방을 만든다
func rrSeatClients(t *testing.T, h *RRHub, room *rrRoom, n int) []*RRClient {
	t.Helper()
	clients := make([]*RRClient, n)
	for i := range clients {
		c := &RRClient{wsClient: newBotWSClient(), Hub: h}
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

// rrTake 소켓 없는 클라이언트의 Send 큐에 쌓인 메시지를 전부 꺼낸다
func rrTake(t *testing.T, c *RRClient) []RRMessage {
	t.Helper()
	out := []RRMessage{}
	for {
		select {
		case data := <-c.Send:
			var msg RRMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			out = append(out, msg)
		default:
			return out
		}
	}
}

// rrMinFromState 스냅샷만 보고 최소 횟수를 구한다 — 봇이 하는 것과 똑같이
// 공개 정보(벽·로봇·목표)만으로 푼다. 테스트가 봇 품질을 재는 근거다.
func rrMinFromState(s rrBotState) (int, bool) {
	board := rrBoardFromWalls(s.Walls)
	if board == nil {
		return 0, false
	}
	robots, ok := rrRobotsFromView(s.Robots)
	if !ok {
		return 0, false
	}
	moves, solved := rrSolve(board, robots, s.Goal, RRMaxDepth)
	if !solved {
		return 0, false
	}
	return len(moves), true
}

// ==================== 3봇 완주 ====================

// TestRRThreeBotsCompleteGame 봇 3기를 채운 4인전이 120초 안에 완주하는지 —
// 가장 중요한 회귀 장치. 차례가 없는 게임이라 "아무도 움직이지 않아 멈추는"
// 교착이 가장 무서운데, 봇이 스스로 시계를 돌리므로 반드시 진행된다.
// 종료는 전체 캡이 아니라 **목표 17개 소진**이어야 한다.
func TestRRThreeBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newRRTestServer(t, defaultDisconnectGrace, rrFastTimings())
	defer cleanup()

	c := rrDial(t, url)
	defer c.conn.Close()
	rrJoin(t, c, "사람", "")
	c.send(t, RRMessage{Type: RRMsgFillBots}) // 호스트 + 봇 3기로 채우고 즉시 시작

	start := time.Now()
	deadline := start.Add(120 * time.Second)
	sawBid, sawDemoOK := false, false
	goalsSeen := map[int]bool{}

	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "state-event-or-over", func(m RRMessage) bool {
			return m.Type == RRMsgGameState || m.Type == RRMsgGameOver || m.Type == RRMsgEvent
		})

		if msg.Type == RRMsgEvent {
			ev := rrPayloadMap(t, msg)
			kind, _ := ev["kind"].(string)
			switch kind {
			case "bid", "bid_first":
				// 외침 이벤트에는 누가 했는지가 반드시 실린다
				if name, _ := ev["name"].(string); name == "" {
					t.Fatalf("외침 이벤트에 name 부재: %v", ev)
				}
				if _, ok := ev["seat"].(float64); !ok {
					t.Fatalf("외침 이벤트에 seat 부재: %v", ev)
				}
				sawBid = true
			case "demo_ok":
				if name, _ := ev["name"].(string); name == "" {
					t.Fatalf("증명 성공 이벤트에 name 부재: %v", ev)
				}
				sawDemoOK = true
			}
			continue
		}

		if msg.Type == RRMsgGameState {
			state := rrPayloadMap(t, msg)
			if idx, ok := state["goalIndex"].(float64); ok {
				goalsSeen[int(idx)] = true
			}
			continue
		}

		// ---- rr_game_over ----
		over := rrPayloadMap(t, msg)
		if reason, _ := over["reason"].(string); reason != "goals_done" {
			t.Fatalf("종료 사유 = %q, want goals_done (전체 캡으로 끝나면 안 된다)", reason)
		}
		if m, _ := over["message"].(string); m == "" {
			t.Fatalf("종료 문구 부재: %v", over)
		}
		if played := int(over["goalsPlayed"].(float64)); played != RRGoalTotal {
			t.Fatalf("소진한 목표 = %d개, want %d개", played, RRGoalTotal)
		}
		winners, ok := over["winnerSeats"].([]interface{})
		if !ok || len(winners) == 0 {
			t.Fatalf("승자 좌석 = %v", over["winnerSeats"])
		}
		if names, ok := over["winnerNames"].([]interface{}); !ok || len(names) != len(winners) {
			t.Fatalf("승자 이름 = %v", over["winnerNames"])
		}
		players, ok := over["players"].([]interface{})
		if !ok || len(players) != RRFillBotTarget {
			t.Fatalf("players 길이 = %v, want %d", over["players"], RRFillBotTarget)
		}

		total, botScore := 0, 0
		for _, raw := range players {
			p := raw.(map[string]interface{})
			score := int(p["score"].(float64))
			if score < 0 {
				t.Fatalf("점수가 0 미만이다: %v", p)
			}
			if p["bot"] == true {
				botScore += score
			}
			total += score
		}
		if total > RRGoalTotal {
			t.Fatalf("점수 합 %d 이 목표 수 %d 를 넘었다", total, RRGoalTotal)
		}
		if !sawBid || !sawDemoOK {
			t.Fatalf("외침 %v / 증명 성공 %v — 봇이 실제로 판을 굴리지 않았다", sawBid, sawDemoOK)
		}
		// 사람은 아무것도 하지 않았으므로 점수는 전부 봇의 것이어야 한다.
		// 0이면 봇의 자체 시계가 돌지 않았다는 뜻이라 회귀로 잡는다.
		if botScore == 0 {
			t.Fatal("봇이 목표를 하나도 못 가져갔다 — 자체 시계가 돌지 않았다")
		}
		if len(goalsSeen) < RRGoalTotal/2 {
			t.Fatalf("스냅샷에서 본 목표 번호가 %d개뿐이다", len(goalsSeen))
		}
		t.Logf("완주: 목표 %d개 소진, 봇 획득 %d개 (총 %d개, %.1fs)",
			RRGoalTotal, botScore, total, time.Since(start).Seconds())
		return
	}
	t.Fatal("120초 안에 게임이 끝나지 않았다 — 진행 불가 상태")
}

// ==================== 은닉 없음 ====================

// TestRRPublicSnapshot 이 게임의 핵심 계약 — 은닉이 없다.
// 관전자(viewerSeat -1)의 스냅샷은 참가자의 것과 yourSeat 하나만 다르고
// 나머지 raw JSON 이 완전히 같아야 한다. 판·로봇·목표·외침이 전부 공개다.
// 허브 고루틴 없이 핸들러를 직접 불러 결정적으로 검증한다.
func TestRRPublicSnapshot(t *testing.T) {
	h := NewRRHub()
	room := h.lobbyRoomFor("")
	clients := rrSeatClients(t, h, room, 3)
	h.startGame(room)
	defer h.stopTimers(room)

	game := room.Game
	rawOf := func(viewer int) string {
		data, err := json.Marshal(h.buildRRState(room, viewer))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(data)
	}

	// ---- 관전자 스냅샷은 yourSeat 만 다르다 ----
	spectatorRaw := rawOf(-1)
	for _, seat := range []int{0, 1, 2} {
		want := strings.Replace(spectatorRaw,
			`"yourSeat":-1`, fmt.Sprintf(`"yourSeat":%d`, seat), 1)
		if rawOf(seat) != want {
			t.Fatalf("seat%d 스냅샷이 관전자 것과 yourSeat 말고도 다르다:\n%s\n%s",
				seat, rawOf(seat), spectatorRaw)
		}
	}
	if !strings.Contains(spectatorRaw, `"yourSeat":-1`) {
		t.Fatalf("관전자 yourSeat 가 -1 이 아니다:\n%s", spectatorRaw)
	}

	// ---- 공개 필드 계약 ----
	for _, want := range []string{
		`"lastResult":null`, `"result":null`, `"bids":[]`, `"demoSeat":-1`,
		`"goalIndex":0`, fmt.Sprintf(`"goalTotal":%d`, RRGoalTotal),
		`"phase":"thinking"`, `"score":0`, `"walls":[[`,
		`"robots":{"blue":{`, `"goal":{"color":"`,
	} {
		if !strings.Contains(spectatorRaw, want) {
			t.Fatalf("스냅샷에 %s 부재:\n%s", want, spectatorRaw)
		}
	}

	spec := h.buildRRState(room, -1)
	if len(spec.Walls) != RRSize {
		t.Fatalf("walls 행 수 = %d, want %d", len(spec.Walls), RRSize)
	}
	for r, row := range spec.Walls {
		if len(row) != RRSize {
			t.Fatalf("walls[%d] 길이 = %d", r, len(row))
		}
	}
	if len(spec.Robots) != RRRobotCount {
		t.Fatalf("robots = %v", spec.Robots)
	}
	if spec.EndsAt <= 0 {
		t.Fatalf("endsAt = %d, want 미래의 unixMillis (목표 상한)", spec.EndsAt)
	}
	// 정답(최소 횟수)만은 아무에게도 나가지 않는다
	if strings.Contains(spectatorRaw, "minMoves") || strings.Contains(spectatorRaw, "solution") {
		t.Fatalf("스냅샷이 정답을 흘린다:\n%s", spectatorRaw)
	}

	// ---- 목표는 항상 최소 2~10회 ----
	if game.MinMoves < RRMinGoalMoves || game.MinMoves > RRMaxGoalMoves {
		t.Fatalf("시작 목표 최소 횟수 = %d회", game.MinMoves)
	}

	// ---- 빈 대기실 스냅샷도 빈 배열 [] (null 금지, 패닉 금지) ----
	empty := h.lobbyRoomFor("ZZZZ")
	emptyRaw, _ := json.Marshal(h.buildRRState(empty, -1))
	for _, want := range []string{
		`"players":[]`, `"bids":[]`, `"hostSeat":-1`, `"yourSeat":-1`,
		`"demoSeat":-1`, `"endsAt":0`, `"phase":"waiting"`, `"walls":[[0,`,
	} {
		if !strings.Contains(string(emptyRaw), want) {
			t.Fatalf("빈 대기실 스냅샷에 %s 부재:\n%s", want, emptyRaw)
		}
	}

	// ---- 외침·증명이 전원에게 똑같이 보인다 ----
	for _, c := range clients {
		rrTake(t, c)
	}
	solution, ok := rrSolve(game.Board, game.Robots, game.Goal, RRMaxDepth)
	if !ok {
		t.Fatal("시작 목표를 못 풀었다")
	}
	h.handleGameMessage(RRGameMessage{Client: clients[1], Message: RRMessage{
		Type: RRMsgBid, Payload: RRBidPayload{Moves: len(solution)}}})
	if game.Phase != RRPhaseBidding {
		t.Fatalf("외침 뒤 단계 = %s", game.Phase)
	}
	bidRaw := rawOf(-1)
	if !strings.Contains(bidRaw, fmt.Sprintf(`"bids":[{"seat":1,"moves":%d}]`, len(solution))) {
		t.Fatalf("외침이 공개되지 않았다:\n%s", bidRaw)
	}

	game.CloseBidding()
	h.handleGameMessage(RRGameMessage{Client: clients[1], Message: RRMessage{
		Type: RRMsgDemo, Payload: RRDemoPayload{Moves: solution}}})
	if game.Players[1].Score != 1 {
		t.Fatalf("증명 성공인데 점수 = %d", game.Players[1].Score)
	}
	okRaw := rawOf(-1)
	if !strings.Contains(okRaw, `"lastResult":{"seat":1,"name":"P1","ok":true`) {
		t.Fatalf("증명 결과가 공개되지 않았다:\n%s", okRaw)
	}

	// 판정 이벤트에 이름이 실렸는지 (프론트 배너 근거)
	sawOK := false
	for _, msg := range rrTake(t, clients[0]) {
		if msg.Type != RRMsgEvent {
			continue
		}
		ev := rrPayloadMap(t, msg)
		if ev["kind"] == "demo_ok" {
			if ev["name"] != "P1" || int(ev["seat"].(float64)) != 1 {
				t.Fatalf("demo_ok 이벤트 = %v", ev)
			}
			sawOK = true
		}
	}
	if !sawOK {
		t.Fatal("demo_ok 이벤트가 방송되지 않았다")
	}

	// ---- 남의 차례에 증명하면 에러 ----
	game.NextGoal(h.rng)
	h.handleGameMessage(RRGameMessage{Client: clients[0], Message: RRMessage{
		Type: RRMsgDemo, Payload: RRDemoPayload{Moves: solution}}})
	sawErr := false
	for _, msg := range rrTake(t, clients[0]) {
		if msg.Type == RRMsgError {
			sawErr = true
		}
	}
	if !sawErr {
		t.Fatal("증명 단계가 아닌데 증명이 통과했다")
	}
}

// ==================== 방 코드 / 관전 / 리액션 ====================

// TestRRRoomCodeAndSpectate 방 코드 발급·코드 입장·시작 후 코드 관전 진입.
// 관전자는 yourSeat -1 만 다른 동일 스냅샷을 받고, 행동은 전부 차단된다.
func TestRRRoomCodeAndSpectate(t *testing.T) {
	tm := rrFastTimings()
	tm.goalCap = 30 * time.Second // 관전 확인 중에 목표가 넘어가지 않게
	_, url, cleanup := newRRTestServer(t, defaultDisconnectGrace, tm)
	defer cleanup()

	host := rrDial(t, url)
	defer host.conn.Close()
	joined := rrJoin(t, host, "호스트", "NEW")
	code, _ := joined["roomCode"].(string)
	if len(code) != roomCodeLen {
		t.Fatalf("roomCode = %q, want %d자", code, roomCodeLen)
	}

	guest := rrDial(t, url)
	defer guest.conn.Close()
	g := rrJoin(t, guest, "동료", code)
	if g["roomCode"] != code || int(g["yourSeat"].(float64)) != 1 {
		t.Fatalf("코드 입장 실패: %v", g)
	}

	host.send(t, RRMessage{Type: RRMsgStart})
	state := host.rrWaitPhase(t, string(RRPhaseThinking))
	if state["roomCode"] != code || len(state["players"].([]interface{})) != 2 {
		t.Fatalf("시작 실패: %v", state)
	}
	walls, ok := state["walls"].([]interface{})
	if !ok || len(walls) != RRSize {
		t.Fatalf("시작 벽 격자 = %v", state["walls"])
	}
	if ends := int64(state["endsAt"].(float64)); ends <= time.Now().UnixMilli() {
		t.Fatalf("endsAt = %d, want 미래의 unixMillis", ends)
	}
	if state["lastResult"] != nil || state["result"] != nil {
		t.Fatalf("시작 스냅샷 lastResult=%v result=%v", state["lastResult"], state["result"])
	}
	rrDrain(guest)

	// 시작된 방의 코드로 들어오면 관전자
	spec := rrDial(t, url)
	defer spec.conn.Close()
	spec.send(t, RRMessage{Type: RRMsgJoinGame,
		Payload: RRJoinGamePayload{Name: "구경꾼", Room: code}})
	specJoined := rrPayloadMap(t, spec.waitFor(t, RRMsgSpectateJoined))
	if specJoined["roomCode"] != code {
		t.Fatalf("관전 입장 roomCode = %v", specJoined["roomCode"])
	}

	specState := rrPayloadMap(t, spec.waitFor(t, RRMsgGameState))
	if int(specState["yourSeat"].(float64)) != -1 {
		t.Fatalf("관전자 yourSeat = %v, want -1", specState["yourSeat"])
	}
	// 은닉이 없다 — 관전자도 벽·로봇·목표·외침을 전부 본다
	specWalls, ok := specState["walls"].([]interface{})
	if !ok || len(specWalls) != RRSize {
		t.Fatalf("관전자 벽 격자 = %v", specState["walls"])
	}
	robots, ok := specState["robots"].(map[string]interface{})
	if !ok || len(robots) != RRRobotCount {
		t.Fatalf("관전자 로봇 = %v", specState["robots"])
	}
	for _, color := range rrColors {
		cell, ok := robots[string(color)].(map[string]interface{})
		if !ok {
			t.Fatalf("관전자에게 %s 로봇 부재: %v", color, robots)
		}
		if _, ok := cell["r"].(float64); !ok {
			t.Fatalf("로봇 좌표 부재: %v", cell)
		}
	}
	goal, ok := specState["goal"].(map[string]interface{})
	if !ok || goal["color"] == "" {
		t.Fatalf("관전자 목표 지점 = %v", specState["goal"])
	}
	if _, ok := specState["bids"].([]interface{}); !ok {
		t.Fatalf("관전자 외침 목록 = %v (want [])", specState["bids"])
	}
	if int(specState["spectators"].(float64)) != 1 {
		t.Fatalf("관전자 수 = %v", specState["spectators"])
	}

	// 관전자는 어떤 행동도 못 한다
	spec.send(t, RRMessage{Type: RRMsgBid, Payload: RRBidPayload{Moves: 3}})
	errPayload := rrPayloadMap(t, spec.waitFor(t, RRMsgError))
	if errPayload["message"] != spectatorDeniedMsg {
		t.Fatalf("관전자 차단 문구 = %v", errPayload["message"])
	}

	// 리액션은 좌석 보유자만, 이벤트로 되돈다
	host.send(t, RRMessage{Type: RRMsgReact, Payload: RRReactPayload{Emoji: "🔥"}})
	ev := rrPayloadMap(t, host.waitMatch(t, "react", func(m RRMessage) bool {
		if m.Type != RRMsgEvent {
			return false
		}
		p, ok := m.Payload.(map[string]interface{})
		return ok && p["kind"] == "react"
	}))
	if ev["message"] != "🔥" || ev["name"] != "호스트" {
		t.Fatalf("리액션 이벤트 = %v", ev)
	}
}

// ==================== 재접속 3종 ====================

// TestRRReconnect 이탈 통지(rr_player_disconnected) 후 세션으로 돌아오면
// 좌석·판이 그대로 복원되고(rr_player_reconnected), 모르는 세션은
// rr_session_expired 로 거절된다.
func TestRRReconnect(t *testing.T) {
	tm := rrFastTimings()
	tm.goalCap = 30 * time.Second
	_, url, cleanup := newRRTestServer(t, 3*time.Second, tm)
	defer cleanup()

	conns := make([]*rrTestClient, 2)
	sessions := make([]string, 2)
	for i := range conns {
		conns[i] = rrDial(t, url)
		joined := rrJoin(t, conns[i], fmt.Sprintf("P%d", i), "")
		sessions[i], _ = joined["sessionId"].(string)
	}
	defer conns[0].conn.Close()
	conns[0].send(t, RRMessage{Type: RRMsgStart})
	conns[0].rrWaitPhase(t, string(RRPhaseThinking))

	// 좌석 1 이탈 → 남은 사람에게 이탈 통지
	conns[1].conn.Close()
	discon := rrPayloadMap(t, conns[0].waitFor(t, RRMsgPlayerDisconnected))
	if int(discon["seat"].(float64)) != 1 || discon["name"] != "P1" {
		t.Fatalf("이탈 통지 = %v", discon)
	}
	if int(discon["graceSeconds"].(float64)) <= 0 {
		t.Fatalf("graceSeconds = %v", discon["graceSeconds"])
	}

	// 세션으로 재접속 → 좌석·판 복원
	back := rrDial(t, url)
	defer back.conn.Close()
	back.send(t, RRMessage{Type: RRMsgRejoin, Payload: RRRejoinPayload{SessionID: sessions[1]}})
	recon := rrPayloadMap(t, back.waitFor(t, RRMsgPlayerReconnected))
	if int(recon["seat"].(float64)) != 1 {
		t.Fatalf("재접속 통지 = %v", recon)
	}
	restored := rrPayloadMap(t, back.waitFor(t, RRMsgGameState))
	if int(restored["yourSeat"].(float64)) != 1 {
		t.Fatalf("복원 yourSeat = %v", restored["yourSeat"])
	}
	if w, ok := restored["walls"].([]interface{}); !ok || len(w) != RRSize {
		t.Fatalf("복원 스냅샷 벽 격자 = %v", restored["walls"])
	}

	// 재접속한 좌석은 그대로 외칠 수 있다
	var state rrBotState
	raw, _ := json.Marshal(restored)
	json.Unmarshal(raw, &state)
	min, ok := rrMinFromState(state)
	if !ok {
		t.Fatal("복원 스냅샷으로 퍼즐을 못 풀었다")
	}
	back.send(t, RRMessage{Type: RRMsgBid, Payload: RRBidPayload{Moves: min}})
	after := back.waitMatch(t, "외침 스냅샷", func(m RRMessage) bool {
		if m.Type != RRMsgGameState {
			return false
		}
		s, ok := m.Payload.(map[string]interface{})
		if !ok {
			return false
		}
		bids, ok := s["bids"].([]interface{})
		return ok && len(bids) > 0
	})
	bids := rrPayloadMap(t, after)["bids"].([]interface{})
	first := bids[0].(map[string]interface{})
	if int(first["seat"].(float64)) != 1 || int(first["moves"].(float64)) != min {
		t.Fatalf("복원 좌석의 외침이 다르게 기록됐다: %v", first)
	}

	// 모르는 세션은 만료 처리
	ghost := rrDial(t, url)
	defer ghost.conn.Close()
	ghost.send(t, RRMessage{Type: RRMsgRejoin, Payload: RRRejoinPayload{SessionID: "없는-세션"}})
	ghost.waitFor(t, RRMsgSessionExpired)
}

// TestRRBotTakeover 유예 만료 좌석을 봇이 이어받고, 이어받은 봇이 실제로
// 판을 굴리는지 (스스로 시계를 돌리는 근거).
func TestRRBotTakeover(t *testing.T) {
	tm := rrFastTimings()
	tm.goalCap = 30 * time.Second
	_, url, cleanup := newRRTestServer(t, 120*time.Millisecond, tm)
	defer cleanup()

	conns := make([]*rrTestClient, 2)
	for i := range conns {
		conns[i] = rrDial(t, url)
		defer conns[i].conn.Close()
		rrJoin(t, conns[i], fmt.Sprintf("P%d", i), "")
	}
	conns[0].send(t, RRMessage{Type: RRMsgStart})
	conns[0].rrWaitPhase(t, string(RRPhaseThinking))

	// 좌석 1 이탈 → 유예 만료 → 봇 대체
	conns[1].conn.Close()
	ev := rrPayloadMap(t, conns[0].waitMatch(t, "bot_takeover", func(m RRMessage) bool {
		if m.Type != RRMsgEvent {
			return false
		}
		p, ok := m.Payload.(map[string]interface{})
		return ok && p["kind"] == "bot_takeover"
	}))
	if int(ev["seat"].(float64)) != 1 {
		t.Fatalf("봇 대체 좌석 = %v, want 1", ev["seat"])
	}
	if ev["name"] == nil || ev["name"] == "" {
		t.Fatalf("봇 대체 이벤트에 name 부재: %v", ev)
	}

	// 이어받은 봇이 실제로 외친다
	state := rrPayloadMap(t, conns[0].waitMatch(t, "봇 좌석 외침", func(m RRMessage) bool {
		if m.Type != RRMsgGameState {
			return false
		}
		s, ok := m.Payload.(map[string]interface{})
		if !ok {
			return false
		}
		bids, ok := s["bids"].([]interface{})
		if !ok {
			return false
		}
		for _, raw := range bids {
			if int(raw.(map[string]interface{})["seat"].(float64)) == 1 {
				return true
			}
		}
		return false
	}))
	players := state["players"].([]interface{})
	bot := players[1].(map[string]interface{})
	if bot["bot"] != true {
		t.Fatalf("좌석 1이 봇으로 표시되지 않았다: %v", bot)
	}
}

// ==================== 안전장치 ====================

// TestRRForceEndCap 무한 게임 방지 캡 — 아무도 움직이지 않아도 제한 시간이
// 지나면 현재 점수로 정산하고 끝난다. 1인 연습판도 함께 확인한다.
func TestRRForceEndCap(t *testing.T) {
	tm := rrFastTimings()
	tm.goalCap = 30 * time.Second
	tm.gameCap = 200 * time.Millisecond
	_, url, cleanup := newRRTestServer(t, defaultDisconnectGrace, tm)
	defer cleanup()

	c := rrDial(t, url)
	defer c.conn.Close()
	joined := rrJoin(t, c, "혼자", "")
	if int(joined["yourSeat"].(float64)) != 0 {
		t.Fatalf("yourSeat = %v", joined["yourSeat"])
	}

	c.send(t, RRMessage{Type: RRMsgStart}) // 1인도 시작할 수 있다
	state := c.rrWaitPhase(t, string(RRPhaseThinking))
	if len(state["players"].([]interface{})) != 1 {
		t.Fatalf("1인 연습판 인원 = %v", state["players"])
	}

	over := rrPayloadMap(t, c.waitFor(t, RRMsgGameOver))
	if over["reason"] != "time_up" {
		t.Fatalf("종료 사유 = %v, want time_up", over["reason"])
	}
	if int(over["goalsPlayed"].(float64)) != 1 {
		t.Fatalf("아무도 안 움직였는데 goalsPlayed = %v", over["goalsPlayed"])
	}
	winners, ok := over["winnerSeats"].([]interface{})
	if !ok || len(winners) != 1 || int(winners[0].(float64)) != 0 {
		t.Fatalf("강제 종료 승자 = %v", over["winnerSeats"])
	}
	if m, _ := over["message"].(string); !strings.Contains(m, "제한 시간") {
		t.Fatalf("강제 종료 문구 = %q", m)
	}
}

// TestRRGoalCapAdvances 아무도 외치지 않는 목표는 상한이 지나면 넘어간다 —
// 사람만 있는 방이 한 목표에서 영원히 멈추지 않는 근거다.
func TestRRGoalCapAdvances(t *testing.T) {
	tm := rrFastTimings()
	tm.goalCap = 150 * time.Millisecond
	_, url, cleanup := newRRTestServer(t, defaultDisconnectGrace, tm)
	defer cleanup()

	c := rrDial(t, url)
	defer c.conn.Close()
	rrJoin(t, c, "혼자", "")
	c.send(t, RRMessage{Type: RRMsgStart})
	c.rrWaitPhase(t, string(RRPhaseThinking))

	state := c.waitMatch(t, "다음 목표", func(m RRMessage) bool {
		if m.Type != RRMsgGameState {
			return false
		}
		s, ok := m.Payload.(map[string]interface{})
		if !ok || s["phase"] != string(RRPhaseThinking) {
			return false
		}
		idx, ok := s["goalIndex"].(float64)
		return ok && int(idx) >= 1
	})
	s := rrPayloadMap(t, state)
	if s["lastResult"] == nil {
		t.Fatal("넘어간 목표의 결과가 남지 않았다")
	}
	last := s["lastResult"].(map[string]interface{})
	if last["ok"] != false || int(last["seat"].(float64)) != -1 {
		t.Fatalf("넘어간 목표 결과 = %v", last)
	}
}

// ==================== 봇 품질 ====================

// rrGoalSample 목표 하나의 관측치
type rrGoalSample struct {
	min      int
	elapsed  time.Duration
	bids     map[int]int // seat → 마지막으로 외친 횟수
	winner   int         // 증명에 성공한 좌석 (-1 없음)
	winMoves int
}

// TestRRBotQuality 봇 품질을 숫자로 잰다 — 3봇 30판의 평균 목표 소요 시간과
// 외친 횟수 분포(최소 대비 초과분).
//
// 봇이 늘 최소 횟수를 외치면 사람이 이길 여지가 없다. rrBotFloorWeights 가
// 그 손잡이이고, 이 테스트가 그 결과를 계측한다. "최소 그대로 외친 비율"이
// 지나치게 높으면(사실상 100%) 회귀로 잡는다.
func TestRRBotQuality(t *testing.T) {
	const wantGoals = 30

	samples := []rrGoalSample{}
	for game := 0; len(samples) < wantGoals && game < 4; game++ {
		samples = append(samples, rrCollectGoalSamples(t, wantGoals-len(samples))...)
	}
	if len(samples) < wantGoals {
		t.Fatalf("목표 표본이 %d개뿐이다 (want %d)", len(samples), wantGoals)
	}
	samples = samples[:wantGoals]

	var totalElapsed time.Duration
	excess := map[int]int{}
	bidCount, exactBids, solvedGoals := 0, 0, 0
	winnerExcess := map[int]int{}

	for _, s := range samples {
		totalElapsed += s.elapsed
		for _, moves := range s.bids {
			bidCount++
			d := moves - s.min
			if d < 0 {
				t.Fatalf("최소 %d회인데 %d회를 외쳤다 — BFS가 틀렸다", s.min, moves)
			}
			excess[d]++
			if d == 0 {
				exactBids++
			}
		}
		if s.winner >= 0 {
			solvedGoals++
			winnerExcess[s.winMoves-s.min]++
		}
	}

	avg := totalElapsed / time.Duration(len(samples))
	exactRate := float64(exactBids) / float64(bidCount) * 100

	keys := []int{}
	for d := range excess {
		keys = append(keys, d)
	}
	sort.Ints(keys)
	parts := []string{}
	for _, d := range keys {
		parts = append(parts, fmt.Sprintf("+%d:%d(%.0f%%)", d, excess[d],
			float64(excess[d])/float64(bidCount)*100))
	}

	t.Logf("3봇 %d판 | 평균 목표 소요 %v | 외침 %d건 | 최소 대비 분포 %s | 최소 그대로 %.1f%%",
		len(samples), avg.Round(time.Millisecond), bidCount,
		strings.Join(parts, " "), exactRate)
	t.Logf("증명 성공 %d/%d판 | 증명자의 최소 대비 초과분 분포 %v",
		solvedGoals, len(samples), winnerExcess)

	if bidCount < len(samples) {
		t.Fatalf("외침이 %d건뿐이다 — 봇이 거의 외치지 않았다", bidCount)
	}
	if solvedGoals == 0 {
		t.Fatal("증명에 성공한 목표가 하나도 없다")
	}
	// 사람이 끼어들 여지 — 봇이 늘 최소를 외치면 사람은 절대 못 이긴다.
	// 목표별로 "아무 봇도 최소를 외치지 않은" 판이 충분히 있어야 한다.
	openGoals := 0
	for _, s := range samples {
		beaten := false
		for _, moves := range s.bids {
			if moves == s.min {
				beaten = true
				break
			}
		}
		if !beaten {
			openGoals++
		}
	}
	openRate := float64(openGoals) / float64(len(samples)) * 100
	t.Logf("아무 봇도 최소 횟수를 외치지 않은 판 %d/%d (%.0f%%) — 사람이 최소로 이길 여지",
		openGoals, len(samples), openRate)
	if openGoals == 0 {
		t.Fatal("모든 판에서 봇이 최소 횟수를 외쳤다 — 사람이 이길 여지가 없다")
	}
}

// rrCollectGoalSamples 3봇 한 판을 돌려 목표별 관측치를 모은다 (최대 limit 개)
func rrCollectGoalSamples(t *testing.T, limit int) []rrGoalSample {
	t.Helper()
	_, url, cleanup := newRRTestServer(t, defaultDisconnectGrace, rrFastTimings())
	defer cleanup()

	c := rrDial(t, url)
	defer c.conn.Close()
	rrJoin(t, c, "관찰자", "") // 아무것도 하지 않는다 — 순수 3봇 측정
	c.send(t, RRMessage{Type: RRMsgFillBots})

	samples := []rrGoalSample{}
	cur := rrGoalSample{bids: map[int]int{}, winner: -1, min: -1}
	curIndex := -1
	openedAt := time.Time{}
	deadline := time.Now().Add(90 * time.Second)

	flush := func() {
		if curIndex >= 0 && cur.min > 0 {
			cur.elapsed = time.Since(openedAt)
			samples = append(samples, cur)
		}
		cur = rrGoalSample{bids: map[int]int{}, winner: -1, min: -1}
	}

	for len(samples) < limit && time.Now().Before(deadline) {
		msg := c.waitMatch(t, "state-or-over", func(m RRMessage) bool {
			return m.Type == RRMsgGameState || m.Type == RRMsgGameOver
		})
		if msg.Type == RRMsgGameOver {
			flush()
			return samples
		}

		var s rrBotState
		raw, _ := json.Marshal(msg.Payload)
		if json.Unmarshal(raw, &s) != nil {
			continue
		}
		if s.Phase == RRPhaseWaiting || s.Phase == RRPhaseGameOver {
			continue
		}

		if s.GoalIndex != curIndex {
			flush()
			curIndex = s.GoalIndex
			openedAt = time.Now()
			if min, ok := rrMinFromState(s); ok {
				cur.min = min
			}
		}
		for _, b := range s.Bids {
			cur.bids[b.Seat] = b.Moves
		}

		full := rrPayloadMap(t, msg)
		if last, ok := full["lastResult"].(map[string]interface{}); ok && last != nil {
			if last["ok"] == true && s.Phase == RRPhaseGoalEnd {
				cur.winner = int(last["seat"].(float64))
				cur.winMoves = int(last["moves"].(float64))
			}
		}
	}
	flush()
	return samples
}

// ==================== 전적 ====================

// TestRRMatchRecordFormat 전적 표기 — 동점 공동 승리는 "·" 로 이어야
// 전적 장부(splitWinners)가 전원을 승자로 읽는다. Winner "" 는 무승부다.
func TestRRMatchRecordFormat(t *testing.T) {
	all := []string{"가", "나", "다"}
	players := strings.Join(all, " vs ")
	winner := strings.Join([]string{"나", "다"}, "·")

	parsed := splitPlayers(players)
	if len(parsed) != 3 {
		t.Fatalf("참가자 파싱 = %v", parsed)
	}
	winners := splitWinners(winner)
	if len(winners) != 2 || winners[0] != "나" || winners[1] != "다" {
		t.Fatalf("공동 승자 파싱 = %v", winners)
	}
	if splitWinners("") != nil {
		t.Fatal("빈 Winner 는 무승부여야 한다")
	}
}
