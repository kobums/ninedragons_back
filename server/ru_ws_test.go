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

// 테스트에서는 차례 마감과 봇의 생각 시간을 짧게 낮춘다 (실사용은 차례 90초)
func init() {
	ruTurnTimeout = 100 * time.Millisecond
	ruBotDelay = 0
	ruBotJitterMs = 0
}

// ruTestClient 공용 testConn 에 루미큐브 메시지 타입의 waitFor 를 얹은 래퍼
type ruTestClient struct {
	testConn[RUMessage]
}

func newRUTestServer(t *testing.T, grace time.Duration) (*RUHub, string, func()) {
	t.Helper()
	hub := NewRUHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeRUWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func ruDial(t *testing.T, url string) *ruTestClient {
	t.Helper()
	return &ruTestClient{dialWS[RUMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *ruTestClient) waitFor(t *testing.T, msgType RUMessageType) RUMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m RUMessage) bool { return m.Type == msgType })
}

func ruPayloadMap(t *testing.T, msg RUMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// ruJoin 입장하고 ru_player_joined payload 를 돌려준다
func ruJoin(t *testing.T, c *ruTestClient, name, room string) map[string]interface{} {
	t.Helper()
	c.send(t, RUMessage{Type: RUMsgJoinGame, Payload: RUJoinGamePayload{Name: name, Room: room}})
	return ruPayloadMap(t, c.waitFor(t, RUMsgPlayerJoined))
}

// ruWaitPhase 지정한 phase 의 스냅샷이 올 때까지 소비
func (c *ruTestClient) ruWaitPhase(t *testing.T, phase string) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "ru_game_state("+phase+")", func(m RUMessage) bool {
		if m.Type != RUMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		return ok && state["phase"] == phase
	})
	return ruPayloadMap(t, msg)
}

// ruDrainConn 배경으로 소켓을 비운다 (읽지 않는 연결의 버퍼 포화 방지)
func ruDrainConn(c *ruTestClient) {
	conn := c.conn
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

// TestRUThreeBotsCompleteGame 봇을 채운 3인 게임이 120초 안에 완주하는지 —
// 가장 중요한 회귀 장치 (차례 교착·세트 판정 오류·종료 판정 감지).
// 좌석 0은 서버 연습봇 두뇌(ruBrain)를 WS 로 감싼 드라이버가 잡는다.
func TestRUThreeBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newRUTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := ruDial(t, url)
	defer c.conn.Close()
	ruJoin(t, c, "감독", "")
	c.send(t, RUMessage{Type: RUMsgFillBots}) // 3인까지 채우고 즉시 시작

	start := time.Now()
	brain := newRUBrain()
	deadline := start.Add(120 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "state-or-over", func(m RUMessage) bool {
			return m.Type == RUMsgGameState || m.Type == RUMsgGameOver
		})
		if msg.Type == RUMsgGameOver {
			over := ruPayloadMap(t, msg)
			seats, _ := over["winnerSeats"].([]interface{})
			names, _ := over["winnerNames"].([]interface{})
			if len(seats) == 0 || len(seats) != len(names) {
				t.Fatalf("승자 = %v / %v", over["winnerSeats"], over["winnerNames"])
			}
			if m, _ := over["message"].(string); m == "" || !hasHangul(m) {
				t.Fatalf("종료 문구 = %v", over["message"])
			}
			turns := int(over["turns"].(float64))
			if turns < 1 || turns >= RUMaxTurns {
				t.Fatalf("turns = %d", turns)
			}
			rows, _ := over["rows"].([]interface{})
			if len(rows) != RUFillBotTarget {
				t.Fatalf("정산 표 길이 = %d, want %d", len(rows), RUFillBotTarget)
			}
			for _, rRaw := range rows {
				row := rRaw.(map[string]interface{})
				if _, ok := row["seat"]; !ok {
					t.Fatalf("정산 행에 seat 부재: %v", row)
				}
				if _, ok := row["score"]; !ok {
					t.Fatalf("정산 행에 score 부재: %v", row)
				}
				if d, _ := row["detail"].(string); !hasHangul(d) {
					t.Fatalf("정산 설명이 한글이 아니다: %v", row["detail"])
				}
			}

			players := over["players"].([]interface{})
			if len(players) != RUFillBotTarget {
				t.Fatalf("players 길이 = %d, want %d", len(players), RUFillBotTarget)
			}
			for _, pRaw := range players {
				p := pRaw.(map[string]interface{})
				// 종료 화면에도 남의 받침대 내용은 없다
				for _, leak := range []string{"rack", "yourRack", "tiles"} {
					if _, ok := p[leak]; ok {
						t.Fatalf("종료 화면에 받침대 유출(%s): %v", leak, p)
					}
				}
				for _, key := range []string{"seat", "rackCount", "melded", "score"} {
					if _, ok := p[key]; !ok {
						t.Fatalf("players 항목에 %s 부재: %v", key, p)
					}
				}
			}
			t.Logf("완주: 승자 %v · %d차례 (%.1fs)",
				over["winnerNames"], turns, time.Since(start).Seconds())
			return
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("120초 안에 게임이 끝나지 않았다 — 진행 불가 상태")
}

// ruTakeMessages 봇 채널에 쌓인 메시지를 모두 꺼낸다
func ruTakeMessages(t *testing.T, c *RUClient) []RUMessage {
	t.Helper()
	out := []RUMessage{}
	for {
		select {
		case data := <-c.Send:
			var msg RUMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			out = append(out, msg)
		default:
			return out
		}
	}
}

// TestRUHiddenState 은닉의 핵심 계약 — 허브 고루틴 없이 핸들러를 직접 불러
// 결정적으로 검증한다.
//   - yourRack·yourMelded 는 본인 스냅샷에만 (타인·관전자 raw JSON 에 키 부재)
//   - 타일더미의 내용은 어떤 스냅샷에도 없다 (타일 총량 회계로 증명)
//   - 관전자(viewerSeat -1)·좌석 없는 방 스냅샷이 패닉 없이 만들어지고
//     빈 슬라이스는 [] 다
func TestRUHiddenState(t *testing.T) {
	h, room, clients := ruBotFixture(t, 3, 20260829)
	game := room.Game

	rawOf := func(viewer int) string {
		data, err := json.Marshal(h.buildRUState(room, viewer))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(data)
	}

	// 와이어 계약을 눈으로 확인할 수 있게 남긴다 (-v 로만 보인다)
	t.Logf("본인(seat0) 스냅샷: %s", rawOf(0))
	t.Logf("관전자 스냅샷:     %s", rawOf(-1))

	// ---- yourRack·yourMelded 는 본인에게만 ----
	for _, key := range []string{`"yourRack"`, `"yourMelded"`} {
		if strings.Count(rawOf(0), key) != 1 {
			t.Fatalf("%s 키가 본인 것 하나가 아니다:\n%s", key, rawOf(0))
		}
		if strings.Contains(rawOf(-1), key) {
			t.Fatalf("관전자 스냅샷에 %s 키 유출:\n%s", key, rawOf(-1))
		}
	}
	for _, viewer := range []int{0, 1, 2} {
		state := h.buildRUState(room, viewer)
		if state.YourRack == nil || len(*state.YourRack) != RUStartRack {
			t.Fatalf("viewer %d yourRack = %v", viewer, state.YourRack)
		}
		if state.YourMelded == nil || *state.YourMelded {
			t.Fatalf("viewer %d yourMelded = %v", viewer, state.YourMelded)
		}
		if (*state.YourRack)[0].ID != game.Players[viewer].Rack[0].ID {
			t.Fatalf("viewer %d 받침대가 자기 것이 아니다", viewer)
		}
		// 남의 받침대는 개수만 보인다
		for _, pv := range state.Players {
			if pv.RackCount != len(game.Players[pv.Seat].Rack) {
				t.Fatalf("rackCount = %d", pv.RackCount)
			}
		}
	}
	// 빈 받침대도 [] 로 나간다
	saved := game.Players[0].Rack
	game.Players[0].Rack = []RUTile{}
	if !strings.Contains(rawOf(0), `"yourRack":[]`) {
		t.Fatalf("빈 받침대가 []가 아니다:\n%s", rawOf(0))
	}
	game.Players[0].Rack = saved

	// ---- 타일더미의 내용은 어떤 스냅샷에도 없다 ----
	countVisible := func(viewer int) int {
		state := h.buildRUState(room, viewer)
		total := state.PoolLeft
		for _, set := range state.Sets {
			total += len(set)
		}
		for _, pv := range state.Players {
			total += pv.RackCount
		}
		return total
	}
	for _, viewer := range []int{0, 1, 2, -1} {
		if got := countVisible(viewer); got != 106 {
			t.Fatalf("viewer %d 가 세는 타일 총량 = %d, want 106", viewer, got)
		}
	}
	for _, viewer := range []int{0, -1} {
		for _, leak := range []string{`"pool":`, `"Pool"`, `"deck"`} {
			if strings.Contains(rawOf(viewer), leak) {
				t.Fatalf("viewer %d 스냅샷에 타일더미 유출(%s):\n%s", viewer, leak, rawOf(viewer))
			}
		}
	}

	// ---- 관전자 스냅샷 ----
	spec := h.buildRUState(room, -1)
	if spec.YourSeat != -1 || spec.YourRack != nil || spec.YourMelded != nil {
		t.Fatalf("관전자 스냅샷: yourSeat=%d rack=%v melded=%v",
			spec.YourSeat, spec.YourRack, spec.YourMelded)
	}
	for _, key := range []string{`"sets":[`, `"players":[`, `"poolLeft":`, `"currentSeat":`,
		`"hostSeat":`, `"spectators":`, `"roomCode":`, `"endsAt":`} {
		if !strings.Contains(rawOf(-1), key) {
			t.Fatalf("관전자 raw 스냅샷에 %s 부재:\n%s", key, rawOf(-1))
		}
	}

	// ---- 좌석 없는 빈 방도 패닉 없이 [] 로 나간다 ----
	empty := h.lobbyRoomFor("ZZZZ")
	raw, _ := json.Marshal(h.buildRUState(empty, -1))
	for _, key := range []string{`"players":[]`, `"sets":[]`, `"result":null`,
		`"lastAction":null`, `"yourSeat":-1`, `"currentSeat":-1`, `"poolLeft":0`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("빈 방 스냅샷에 %s 부재:\n%s", key, raw)
		}
	}
	for _, key := range []string{`"yourRack"`, `"yourMelded"`} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("시작 전 방에 %s 유출:\n%s", key, raw)
		}
	}
	json.Marshal(h.buildRUState(empty, 0)) // 좌석이 없는 방의 좌석 뷰 — 패닉 금지

	// ---- 차례가 아닌 좌석의 행동은 거부된다 ----
	for _, c := range clients {
		ruTakeMessages(t, c)
	}
	seat := game.CurrentSeat
	other := (seat + 1) % len(game.Players)
	poolBefore := len(game.Pool)
	h.handleGameMessage(RUGameMessage{Client: clients[other], Message: RUMessage{Type: RUMsgDraw}})
	if len(game.Pool) != poolBefore || game.CurrentSeat != seat {
		t.Fatal("차례가 아닌 좌석의 행동이 통과했다")
	}
	sawError := false
	for _, out := range ruTakeMessages(t, clients[other]) {
		if out.Type != RUMsgError {
			continue
		}
		text, _ := asPayloadMap(t, out.Payload)["message"].(string)
		if !hasHangul(text) {
			t.Fatalf("오류 문구가 한글이 아니다: %q", text)
		}
		sawError = true
	}
	if !sawError {
		t.Fatal("차례가 아닌 행동에 ru_error 가 없다")
	}
}

// TestRURejections 와이어로 들어온 잘못된 확정이 한글 ru_error 로 되돌아오고
// 판을 건드리지 않는지 — 프론트는 이 ru_error 를 보고 로컬 배치를 차례 시작
// 상태로 되돌린다.
func TestRURejections(t *testing.T) {
	h, room, clients := ruBotFixture(t, 3, 777)
	game := room.Game
	seat := game.CurrentSeat

	// 테이블·받침대를 손으로 세운다 (등록을 마친 좌석)
	t3, t4, t5 := ruT(1001, RURed, 3), ruT(1002, RURed, 4), ruT(1003, RURed, 5)
	m6, m7 := ruT(1010, RURed, 6), ruT(1011, RURed, 7)
	spare := ruT(1012, RUBlue, 1) // 받침대가 비어 게임이 끝나지 않게 하나 남겨 둔다
	game.Sets = [][]RUTile{{t3, t4, t5}}
	game.Players[seat].Rack = []RUTile{m6, m7, spare}
	game.Players[seat].Melded = true

	bad := []RUMessage{
		{Type: RUMsgCommit, Payload: RUCommitPayload{Sets: [][]int{}}},                         // 빈 배치
		{Type: RUMsgCommit, Payload: RUCommitPayload{Sets: [][]int{{1001, 1002, 1003}}}},       // 내 타일 0개
		{Type: RUMsgCommit, Payload: RUCommitPayload{Sets: [][]int{{1001, 1002, 1003}, {}}}},   // 빈 세트
		{Type: RUMsgCommit, Payload: RUCommitPayload{Sets: [][]int{{1001, 1002, 1003, 9999}}}}, // 모르는 타일
		{Type: RUMsgCommit, Payload: RUCommitPayload{Sets: [][]int{{1001, 1002, 1010}}}},       // 테이블 타일 빼돌리기
		{Type: RUMsgCommit, Payload: RUCommitPayload{
			Sets: [][]int{{1001, 1002, 1003}, {1010, 1010, 1011}}}}, // 같은 타일 두 번
		{Type: RUMsgCommit, Payload: RUCommitPayload{
			Sets: [][]int{{1001, 1002, 1003}, {1010, 1011}}}}, // 2장짜리 세트
	}
	for i, msg := range bad {
		for _, c := range clients {
			ruTakeMessages(t, c)
		}
		before := ruSnapshot(game)
		h.handleGameMessage(RUGameMessage{Client: clients[seat], Message: msg})
		if got := ruSnapshot(game); got != before {
			t.Fatalf("%d번째 잘못된 확정이 판을 바꿨다\n전: %s\n후: %s", i, before, got)
		}
		sawError := false
		for _, out := range ruTakeMessages(t, clients[seat]) {
			if out.Type != RUMsgError {
				continue
			}
			text, _ := asPayloadMap(t, out.Payload)["message"].(string)
			if text == "" || !hasHangul(text) {
				t.Fatalf("%d번째 오류 문구가 한글이 아니다: %q", i, text)
			}
			sawError = true
		}
		if !sawError {
			t.Fatalf("%d번째 잘못된 확정에 ru_error 가 없다", i)
		}
	}

	// 정상 확정은 통과하고 차례가 넘어간다
	h.handleGameMessage(RUGameMessage{Client: clients[seat], Message: RUMessage{
		Type: RUMsgCommit, Payload: RUCommitPayload{Sets: [][]int{{1001, 1002, 1003, 1010, 1011}}}}})
	if game.Turns != 1 || game.CurrentSeat == seat {
		t.Fatalf("정상 확정이 반영되지 않았다 (turns=%d seat=%d)", game.Turns, game.CurrentSeat)
	}
	if game.LastAction == nil || !hasHangul(game.LastAction.Message) {
		t.Fatalf("lastAction = %+v", game.LastAction)
	}
	if len(game.Players[seat].Rack) != 1 || game.Players[seat].Rack[0].ID != spare.ID {
		t.Fatalf("받침대 = %+v, want 파랑1 하나", game.Players[seat].Rack)
	}
	if len(game.Sets) != 1 || len(game.Sets[0]) != 5 {
		t.Fatalf("테이블 = %+v, want 5장짜리 연속 하나", game.Sets)
	}
}

// TestRUAfkAutoProgress 접속만 유지한 채 아무도 행동하지 않는 3인전 —
// 차례 마감(자동으로 타일 1개 가져오기)만으로 판이 완주하는지
func TestRUAfkAutoProgress(t *testing.T) {
	_, url, cleanup := newRUTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	conns := make([]*ruTestClient, RUFillBotTarget)
	for i := range conns {
		conns[i] = ruDial(t, url)
		defer conns[i].conn.Close()
		ruJoin(t, conns[i], fmt.Sprintf("잠수%d", i), "")
	}
	host := conns[0]
	host.send(t, RUMessage{Type: RUMsgStart})

	state := host.ruWaitPhase(t, string(RUPhaseTurn))
	if ends := int64(state["endsAt"].(float64)); ends <= 0 {
		t.Fatalf("차례 스냅샷의 endsAt = %d, want unixMillis", ends)
	}
	if want := 106 - RUStartRack*RUFillBotTarget; int(state["poolLeft"].(float64)) != want {
		t.Fatalf("타일더미 잔량 = %v, want %d", state["poolLeft"], want)
	}
	if sets, ok := state["sets"].([]interface{}); !ok || len(sets) != 0 {
		t.Fatalf("시작 테이블 = %v (빈 테이블도 [] 여야 한다)", state["sets"])
	}
	rack, ok := state["yourRack"].([]interface{})
	if !ok || len(rack) != RUStartRack {
		t.Fatalf("본인 스냅샷의 yourRack = %v", state["yourRack"])
	}
	// 타일 와이어 형태 — id·color·num·joker 가 항상 있다
	for _, tRaw := range rack {
		tile := tRaw.(map[string]interface{})
		for _, key := range []string{"id", "color", "num", "joker"} {
			if _, ok := tile[key]; !ok {
				t.Fatalf("타일에 %s 부재: %v", key, tile)
			}
		}
		if _, ok := tile["standsFor"]; ok {
			t.Fatalf("받침대 타일에 standsFor 유출: %v", tile)
		}
	}
	if melded, ok := state["yourMelded"].(bool); !ok || melded {
		t.Fatalf("yourMelded = %v", state["yourMelded"])
	}
	for _, pRaw := range state["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		if int(p["rackCount"].(float64)) != RUStartRack {
			t.Fatalf("시작 받침대 수 = %v", p["rackCount"])
		}
		if _, leaked := p["rack"]; leaked {
			t.Fatalf("남의 받침대 유출: %v", p)
		}
		if _, ok := p["score"]; !ok {
			t.Fatalf("players 에 score 부재: %v", p)
		}
	}

	for _, c := range conns[1:] {
		ruDrainConn(c)
	}

	sawAfk := false
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		msg := host.waitMatch(t, "event-or-over", func(m RUMessage) bool {
			return m.Type == RUMsgEvent || m.Type == RUMsgGameOver
		})
		if msg.Type == RUMsgEvent {
			ev := ruPayloadMap(t, msg)
			if ev["kind"] == "auto_action" {
				if !strings.Contains(ev["message"].(string), "자동") {
					t.Fatalf("자동 진행 문구 = %v", ev["message"])
				}
				if ev["name"] == nil || ev["name"] == "" {
					t.Fatalf("자동 진행 이벤트에 name 부재: %v", ev)
				}
				sawAfk = true
			}
			continue
		}
		over := ruPayloadMap(t, msg)
		if !sawAfk {
			t.Fatal("자동 진행 이벤트가 한 번도 없었다")
		}
		if seats, _ := over["winnerSeats"].([]interface{}); len(seats) == 0 {
			t.Fatalf("종료 payload = %v", over)
		}
		// 아무도 등록하지 못했으므로 전원 −100점 동점 → 공동 승
		if msg, _ := over["message"].(string); !strings.Contains(msg, "타일더미 소진") {
			t.Fatalf("종료 사유 = %q", msg)
		}
		return
	}
	t.Fatal("전원 방치 게임이 90초 안에 끝나지 않았다")
}

// TestRURoomCodeAndSpectate 방 코드 발급·코드 입장·시작 후 코드 관전 진입.
// 관전자 스냅샷은 yourSeat -1 에 yourRack·yourMelded 부재. 행동은 전부 차단된다.
func TestRURoomCodeAndSpectate(t *testing.T) {
	_, url, cleanup := newRUTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := ruDial(t, url)
	defer host.conn.Close()
	joined := ruJoin(t, host, "호스트", "NEW")
	code, _ := joined["roomCode"].(string)
	if len(code) != roomCodeLen {
		t.Fatalf("roomCode = %q, want %d자", code, roomCodeLen)
	}

	guests := make([]*ruTestClient, RUFillBotTarget-1)
	for i := range guests {
		guests[i] = ruDial(t, url)
		defer guests[i].conn.Close()
		g := ruJoin(t, guests[i], fmt.Sprintf("친구%d", i), code)
		if g["roomCode"] != code || int(g["yourSeat"].(float64)) != i+1 {
			t.Fatalf("코드 입장 실패: %v", g)
		}
	}

	host.send(t, RUMessage{Type: RUMsgStart})
	state := host.ruWaitPhase(t, string(RUPhaseTurn))
	if state["roomCode"] != code || len(state["players"].([]interface{})) != RUFillBotTarget {
		t.Fatalf("시작 실패: %v", state["players"])
	}
	for _, c := range guests {
		ruDrainConn(c)
	}

	// 시작된 방의 코드로 들어오면 관전자
	spec := ruDial(t, url)
	defer spec.conn.Close()
	spec.send(t, RUMessage{Type: RUMsgJoinGame, Payload: RUJoinGamePayload{Name: "구경꾼", Room: code}})
	specJoined := ruPayloadMap(t, spec.waitFor(t, RUMsgSpectateJoined))
	if specJoined["roomCode"] != code {
		t.Fatalf("관전 입장 roomCode = %v", specJoined["roomCode"])
	}

	specState := ruPayloadMap(t, spec.waitFor(t, RUMsgGameState))
	if int(specState["yourSeat"].(float64)) != -1 {
		t.Fatalf("관전자 yourSeat = %v, want -1", specState["yourSeat"])
	}
	for _, key := range []string{"yourRack", "yourMelded"} {
		if leaked, ok := specState[key]; ok {
			t.Fatalf("관전자에게 %s 유출: %v", key, leaked)
		}
	}
	if specState["sets"] == nil || specState["players"] == nil {
		t.Fatalf("관전자에게 공개 정보가 없다: %v", specState)
	}

	// 관전자는 어떤 행동도 못 한다
	spec.send(t, RUMessage{Type: RUMsgDraw})
	errPayload := ruPayloadMap(t, spec.waitFor(t, RUMsgError))
	if errPayload["message"] != spectatorDeniedMsg {
		t.Fatalf("관전자 차단 문구 = %v", errPayload["message"])
	}
}

// TestRUReconnect 재접속 3종 — 이탈 통지(ru_player_disconnected) 후 세션으로
// 돌아오면 좌석·받침대가 그대로 복원되고(ru_player_reconnected), 모르는 세션은
// ru_session_expired 로 거절된다.
func TestRUReconnect(t *testing.T) {
	_, url, cleanup := newRUTestServer(t, 5*time.Second)
	defer cleanup()

	conns := make([]*ruTestClient, RUFillBotTarget)
	sessions := make([]string, RUFillBotTarget)
	for i := range conns {
		conns[i] = ruDial(t, url)
		joined := ruJoin(t, conns[i], fmt.Sprintf("P%d", i), "")
		sessions[i], _ = joined["sessionId"].(string)
	}
	defer conns[0].conn.Close()
	conns[0].send(t, RUMessage{Type: RUMsgStart})
	conns[0].ruWaitPhase(t, string(RUPhaseTurn))
	for _, c := range conns[2:] {
		ruDrainConn(c)
	}

	// 좌석 1 이탈 → 남은 사람에게 이탈 통지
	conns[1].conn.Close()
	discon := ruPayloadMap(t, conns[0].waitFor(t, RUMsgPlayerDisconnected))
	if int(discon["seat"].(float64)) != 1 || discon["name"] != "P1" {
		t.Fatalf("이탈 통지 = %v", discon)
	}
	if int(discon["graceSeconds"].(float64)) <= 0 {
		t.Fatalf("graceSeconds = %v", discon["graceSeconds"])
	}

	// 세션으로 재접속 → 좌석·받침대 복원
	back := ruDial(t, url)
	defer back.conn.Close()
	back.send(t, RUMessage{Type: RUMsgRejoin, Payload: RURejoinPayload{SessionID: sessions[1]}})
	recon := ruPayloadMap(t, back.waitFor(t, RUMsgPlayerReconnected))
	if int(recon["seat"].(float64)) != 1 {
		t.Fatalf("재접속 통지 = %v", recon)
	}
	restored := ruPayloadMap(t, back.waitFor(t, RUMsgGameState))
	if int(restored["yourSeat"].(float64)) != 1 {
		t.Fatalf("복원 yourSeat = %v", restored["yourSeat"])
	}
	if _, ok := restored["yourRack"].([]interface{}); !ok {
		t.Fatalf("복원 스냅샷에 yourRack 부재: %v", restored)
	}
	if _, ok := restored["yourMelded"].(bool); !ok {
		t.Fatalf("복원 스냅샷에 yourMelded 부재: %v", restored)
	}

	// 모르는 세션은 만료 처리
	ghost := ruDial(t, url)
	defer ghost.conn.Close()
	ghost.send(t, RUMessage{Type: RUMsgRejoin, Payload: RURejoinPayload{SessionID: "없는-세션"}})
	ghost.waitFor(t, RUMsgSessionExpired)
}

// TestRUBotTakeover 유예 만료 좌석을 봇이 이어받아 게임이 멈추지 않는지
func TestRUBotTakeover(t *testing.T) {
	_, url, cleanup := newRUTestServer(t, 120*time.Millisecond)
	defer cleanup()

	conns := make([]*ruTestClient, RUFillBotTarget)
	for i := range conns {
		conns[i] = ruDial(t, url)
		defer conns[i].conn.Close()
		ruJoin(t, conns[i], fmt.Sprintf("P%d", i), "")
	}
	conns[0].send(t, RUMessage{Type: RUMsgStart})
	conns[0].ruWaitPhase(t, string(RUPhaseTurn))
	for _, c := range conns[2:] {
		ruDrainConn(c)
	}

	// 좌석 1 이탈 → 유예 만료 → 봇 대체
	conns[1].conn.Close()
	sawTakeover := false
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		msg := conns[0].waitMatch(t, "event-or-over", func(m RUMessage) bool {
			return m.Type == RUMsgEvent || m.Type == RUMsgGameOver
		})
		if msg.Type == RUMsgEvent {
			ev := ruPayloadMap(t, msg)
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
		if seats, _ := ruPayloadMap(t, msg)["winnerSeats"].([]interface{}); len(seats) == 0 {
			t.Fatalf("종료 payload = %v", msg.Payload)
		}
		return
	}
	t.Fatal("봇 대체 후 게임이 90초 안에 끝나지 않았다")
}

// TestRUReactAndLobby 리액션은 좌석 보유자만·화이트리스트만, 대기 현황판은
// 사람이 대기할 때만 켜진다
func TestRUReactAndLobby(t *testing.T) {
	_, url, cleanup := newRUTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	conns := make([]*ruTestClient, RUMinPlayers)
	for i := range conns {
		conns[i] = ruDial(t, url)
		defer conns[i].conn.Close()
		ruJoin(t, conns[i], fmt.Sprintf("가나다%d", i), "")
	}
	a, b := conns[0], conns[1]

	a.send(t, RUMessage{Type: RUMsgReact, Payload: RUReactPayload{Emoji: "🔥"}})
	ev := ruPayloadMap(t, b.waitMatch(t, "react", func(m RUMessage) bool {
		if m.Type != RUMsgEvent {
			return false
		}
		p, ok := m.Payload.(map[string]interface{})
		return ok && p["kind"] == "react"
	}))
	if ev["message"] != "🔥" || ev["name"] != "가나다0" || int(ev["seat"].(float64)) != 0 {
		t.Fatalf("리액션 이벤트 = %v", ev)
	}

	// 화이트리스트 밖 이모지는 조용히 무시된다 — 다음에 오는 것은 시작 스냅샷이다
	a.send(t, RUMessage{Type: RUMsgReact, Payload: RUReactPayload{Emoji: "💀"}})
	a.send(t, RUMessage{Type: RUMsgStart})
	state := a.ruWaitPhase(t, string(RUPhaseTurn))
	if int(state["hostSeat"].(float64)) != 0 {
		t.Fatalf("hostSeat = %v", state["hostSeat"])
	}
	if int(state["currentSeat"].(float64)) < 0 {
		t.Fatalf("currentSeat = %v", state["currentSeat"])
	}
}
