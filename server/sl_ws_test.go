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
// (실사용은 차례 60초 · 버리기 20초)
func init() {
	slTurnTimeout = 150 * time.Millisecond
	slDiscardTimeout = 100 * time.Millisecond
	slBotDelay = 0
	slBotJitterMs = 0
}

// slTestClient 공용 testConn 에 스플렌더 메시지 타입의 waitFor 를 얹은 래퍼
type slTestClient struct {
	testConn[SLMessage]
}

func newSLTestServer(t *testing.T, grace time.Duration) (*SLHub, string, func()) {
	t.Helper()
	hub := NewSLHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeSLWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func slDial(t *testing.T, url string) *slTestClient {
	t.Helper()
	return &slTestClient{dialWS[SLMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *slTestClient) waitFor(t *testing.T, msgType SLMessageType) SLMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m SLMessage) bool { return m.Type == msgType })
}

func slPayloadMap(t *testing.T, msg SLMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// slJoin 입장하고 sl_player_joined payload 를 돌려준다
func slJoin(t *testing.T, c *slTestClient, name, room string) map[string]interface{} {
	t.Helper()
	c.send(t, SLMessage{Type: SLMsgJoinGame, Payload: SLJoinGamePayload{Name: name, Room: room}})
	return slPayloadMap(t, c.waitFor(t, SLMsgPlayerJoined))
}

// slWaitPhase 지정한 phase 의 스냅샷이 올 때까지 소비
func (c *slTestClient) slWaitPhase(t *testing.T, phase string) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "sl_game_state("+phase+")", func(m SLMessage) bool {
		if m.Type != SLMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		return ok && state["phase"] == phase
	})
	return slPayloadMap(t, msg)
}

// slDrainConn 배경으로 소켓을 비운다 (읽지 않는 연결의 버퍼 포화 방지)
func slDrainConn(c *slTestClient) {
	conn := c.conn
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

// TestSLThreeBotsCompleteGame 봇을 채운 3인 게임이 90초 안에 완주하는지 —
// 가장 중요한 회귀 장치 (차례 교착·비용 계산 오류·종료 판정 감지).
// 좌석 0은 서버 연습봇 두뇌(slBrain)를 WS 로 감싼 드라이버가 잡는다.
func TestSLThreeBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newSLTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := slDial(t, url)
	defer c.conn.Close()
	slJoin(t, c, "감독", "")
	c.send(t, SLMessage{Type: SLMsgFillBots}) // 3인까지 채우고 즉시 시작

	start := time.Now()
	brain := newSLBrain()
	deadline := start.Add(90 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "state-or-over", func(m SLMessage) bool {
			return m.Type == SLMsgGameState || m.Type == SLMsgGameOver
		})
		if msg.Type == SLMsgGameOver {
			over := slPayloadMap(t, msg)
			seats, _ := over["winnerSeats"].([]interface{})
			names, _ := over["winnerNames"].([]interface{})
			if len(seats) == 0 || len(seats) != len(names) {
				t.Fatalf("승자 = %v / %v", over["winnerSeats"], over["winnerNames"])
			}
			if m, _ := over["message"].(string); m == "" || !hasHangul(m) {
				t.Fatalf("종료 문구 = %v", over["message"])
			}
			turns := int(over["turns"].(float64))
			if turns < 1 || turns > SLMaxTurns {
				t.Fatalf("turns = %d", turns)
			}

			players := over["players"].([]interface{})
			if len(players) != SLFillBotTarget {
				t.Fatalf("players 길이 = %d, want %d", len(players), SLFillBotTarget)
			}
			best := -1
			for _, pRaw := range players {
				p := pRaw.(map[string]interface{})
				pts := int(p["points"].(float64))
				if pts > best {
					best = pts
				}
				// 종료 화면에도 남의 예약 카드 내용은 없다
				if _, leaked := p["reserved"]; leaked {
					t.Fatalf("종료 화면에 예약 카드 유출: %v", p)
				}
				if _, ok := p["reservedCount"]; !ok {
					t.Fatalf("reservedCount 부재: %v", p)
				}
			}
			if best < SLWinPoints {
				t.Fatalf("최고 명성 점수 = %d — %d점에 못 닿고 끝났다", best, SLWinPoints)
			}
			t.Logf("완주: 승자 %v · 최고 명성 점수 %d · %d차례 (%.1fs)",
				over["winnerNames"], best, turns, time.Since(start).Seconds())
			return
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("90초 안에 게임이 끝나지 않았다 — 진행 불가 상태")
}

// slHiddenFixture 허브 고루틴 없이 결정적으로 검증하기 위한 3인 방
func slHiddenFixture(t *testing.T) (*SLHub, *slRoom, []*SLClient) {
	t.Helper()
	h, room, clients := slBotFixture(t, 3, 12345)
	return h, room, clients
}

// slTakeMessages 봇 채널에 쌓인 메시지를 모두 꺼낸다
func slTakeMessages(t *testing.T, c *SLClient) []SLMessage {
	t.Helper()
	out := []SLMessage{}
	for {
		select {
		case data := <-c.Send:
			var msg SLMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			out = append(out, msg)
		default:
			return out
		}
	}
}

// TestSLHiddenState 은닉의 핵심 계약 — 허브 고루틴 없이 핸들러를 직접 불러
// 결정적으로 검증한다.
//   - yourReserved 는 본인 스냅샷에만 (타인·관전자 raw JSON 에 키 부재)
//   - 덱에서 비공개로 예약한 개발 카드의 내용은 남에게 절대 안 보인다
//     (reservedCount 숫자만 보인다)
//   - 관전자(viewerSeat -1) 스냅샷이 패닉 없이 만들어지고 빈 슬라이스는 [] 다
func TestSLHiddenState(t *testing.T) {
	h, room, clients := slHiddenFixture(t)
	game := room.Game

	rawOf := func(viewer int) string {
		data, err := json.Marshal(h.buildSLState(room, viewer))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(data)
	}

	// ---- 시작 직후: 예약이 비어도 본인에게는 [] 로, 남에게는 키 자체가 없다 ----
	if !strings.Contains(rawOf(0), `"yourReserved":[]`) {
		t.Fatalf("빈 예약이 []가 아니다:\n%s", rawOf(0))
	}
	rawSpec := rawOf(-1)
	if strings.Contains(rawSpec, `"yourReserved"`) {
		t.Fatalf("관전자 스냅샷에 yourReserved 키 유출:\n%s", rawSpec)
	}
	if strings.Count(rawOf(1), `"yourReserved"`) != 1 {
		t.Fatalf("yourReserved 키가 본인 것 하나가 아니다:\n%s", rawOf(1))
	}

	// ---- 덱 맨 위를 비공개로 예약한다 (좌석 0) ----
	game.CurrentSeat, game.Phase = 0, SLPhaseTurn
	secret := game.Decks[2][0] // 3단계 덱 맨 위 — 아무도 모르는 카드
	h.handleGameMessage(SLGameMessage{Client: clients[0], Message: SLMessage{
		Type: SLMsgReserve, Payload: SLReservePayload{Tier: 3}}})
	if len(game.Players[0].Reserved) != 1 || game.Players[0].Reserved[0].ID != secret.ID {
		t.Fatalf("덱 예약이 반영되지 않았다: %+v", game.Players[0].Reserved)
	}

	// 본인만 그 카드를 본다
	mine := rawOf(0)
	if !strings.Contains(mine, fmt.Sprintf(`"id":%d`, secret.ID)) {
		t.Fatalf("본인 스냅샷에 예약 카드가 없다:\n%s", mine)
	}
	// 남·관전자에게는 카드 내용이 어디에도 없다 (진열에도 없는 id 라 검색이 곧 증거).
	// 다른 좌석은 자기 예약칸([])만 보고, 관전자는 키 자체가 없다.
	for _, viewer := range []int{1, 2} {
		raw := rawOf(viewer)
		if !strings.Contains(raw, `"yourReserved":[]`) {
			t.Fatalf("viewer %d 의 빈 예약칸이 []가 아니다:\n%s", viewer, raw)
		}
		if strings.Contains(raw, fmt.Sprintf(`"id":%d`, secret.ID)) {
			t.Fatalf("viewer %d 에게 비공개 예약 카드(id %d) 내용 유출:\n%s",
				viewer, secret.ID, raw)
		}
	}
	if raw := rawOf(-1); strings.Contains(raw, `"yourReserved"`) ||
		strings.Contains(raw, fmt.Sprintf(`"id":%d`, secret.ID)) {
		t.Fatalf("관전자에게 예약 유출:\n%s", raw)
	}
	// 남에게는 장수만 보인다
	for _, viewer := range []int{1, -1} {
		for _, pv := range h.buildSLState(room, viewer).Players {
			if pv.Seat == 0 && pv.ReservedCount != 1 {
				t.Fatalf("viewer %d 가 본 seat0 예약 장수 = %d", viewer, pv.ReservedCount)
			}
		}
	}
	// 이벤트에도 카드 내용은 실리지 않는다 (단계까지만)
	for _, ev := range game.DrainEvents() {
		if strings.Contains(ev.Message, fmt.Sprintf("%d", secret.ID)) {
			t.Fatalf("이벤트에 예약 카드 정보 유출: %q", ev.Message)
		}
	}

	// ---- 관전자 스냅샷은 패닉 없이 빌드되고 빈 슬라이스는 [] 다 ----
	spec := h.buildSLState(room, -1)
	if spec.YourSeat != -1 || spec.YourReserved != nil {
		t.Fatalf("관전자 스냅샷: yourSeat=%d reserved=%v", spec.YourSeat, spec.YourReserved)
	}
	for _, key := range []string{`"board":{`, `"tier1":[`, `"nobles":[`, `"players":[`, `"bank":{`} {
		if !strings.Contains(rawOf(-1), key) {
			t.Fatalf("관전자 raw 스냅샷에 %s 부재:\n%s", key, rawOf(-1))
		}
	}
	empty := h.lobbyRoomFor("ZZZZ")
	raw, _ := json.Marshal(h.buildSLState(empty, -1))
	for _, key := range []string{`"players":[]`, `"nobles":[]`, `"tier1":[]`, `"tier3":[]`,
		`"result":null`, `"lastAction":null`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("빈 방 스냅샷에 %s 부재:\n%s", key, raw)
		}
	}
	// 시작 전 방은 예약이 없으니 본인에게도 키가 없다 (패닉 없이)
	if strings.Contains(string(raw), `"yourReserved"`) {
		t.Fatalf("시작 전 방에 yourReserved 유출:\n%s", raw)
	}
	json.Marshal(h.buildSLState(empty, 0)) // 좌석이 없는 방의 좌석 뷰 — 패닉 금지

	// ---- 차례가 아닌 좌석의 행동은 거부된다 ----
	for _, c := range clients {
		slTakeMessages(t, c)
	}
	seat := game.CurrentSeat
	other := (seat + 1) % len(game.Players)
	bankBefore := game.Bank
	h.handleGameMessage(SLGameMessage{Client: clients[other], Message: SLMessage{
		Type: SLMsgTake, Payload: SLTakePayload{Colors: []SLGem{SLDiamond, SLRuby, SLOnyx}}}})
	if game.Bank != bankBefore || game.CurrentSeat != seat {
		t.Fatal("차례가 아닌 좌석의 행동이 통과했다")
	}
	sawError := false
	for _, out := range slTakeMessages(t, clients[other]) {
		if out.Type != SLMsgError {
			continue
		}
		text, _ := asPayloadMap(t, out.Payload)["message"].(string)
		if !hasHangul(text) {
			t.Fatalf("오류 문구가 한글이 아니다: %q", text)
		}
		sawError = true
	}
	if !sawError {
		t.Fatal("차례가 아닌 행동에 sl_error 가 없다")
	}
}

// TestSLRejections 와이어로 들어온 잘못된 요청이 한글 오류로 되돌아오고
// 판을 건드리지 않는지
func TestSLRejections(t *testing.T) {
	h, room, clients := slHiddenFixture(t)
	game := room.Game
	seat := game.CurrentSeat
	game.Bank = slTokens(5, 5, 5, 5, 5, 5)

	bad := []SLMessage{
		{Type: SLMsgTake, Payload: SLTakePayload{Colors: []SLGem{SLGold, SLGold}}},         // 황금
		{Type: SLMsgTake, Payload: SLTakePayload{Colors: []SLGem{SLRuby, SLRuby, SLRuby}}}, // 같은 색 3개
		{Type: SLMsgTake, Payload: SLTakePayload{Colors: []SLGem{SLRuby, SLOnyx}}},         // 색이 넉넉한데 2개
		{Type: SLMsgTake, Payload: SLTakePayload{Colors: []SLGem{}}},                       // 빈 선택
		{Type: SLMsgBuy, Payload: SLBuyPayload{CardID: 0}},                                 // 카드 미지정
		{Type: SLMsgBuy, Payload: SLBuyPayload{CardID: 99999}},                             // 없는 카드
		{Type: SLMsgBuy, Payload: SLBuyPayload{CardID: game.Board[2][0].ID}},               // 토큰 부족
		{Type: SLMsgReserve, Payload: SLReservePayload{Tier: 9}},                           // 없는 단계
		{Type: SLMsgReserve, Payload: SLReservePayload{}},                                  // 지정 없음
		{Type: SLMsgDiscard, Payload: SLDiscardPayload{Colors: []SLGem{SLRuby}}},           // 버릴 때가 아님
	}
	for i, msg := range bad {
		for _, c := range clients {
			slTakeMessages(t, c)
		}
		bankBefore, turnsBefore := game.Bank, game.Turns
		h.handleGameMessage(SLGameMessage{Client: clients[seat], Message: msg})
		if game.Turns != turnsBefore || game.CurrentSeat != seat || game.Bank != bankBefore {
			t.Fatalf("%d번째 잘못된 요청이 판을 바꿨다", i)
		}
		sawError := false
		for _, out := range slTakeMessages(t, clients[seat]) {
			if out.Type != SLMsgError {
				continue
			}
			text, _ := asPayloadMap(t, out.Payload)["message"].(string)
			if text == "" || !hasHangul(text) {
				t.Fatalf("%d번째 오류 문구가 한글이 아니다: %q", i, text)
			}
			sawError = true
		}
		if !sawError {
			t.Fatalf("%d번째 잘못된 요청에 sl_error 가 없다", i)
		}
	}

	// 정상 요청은 통과하고 차례가 넘어간다
	h.handleGameMessage(SLGameMessage{Client: clients[seat], Message: SLMessage{
		Type: SLMsgTake, Payload: SLTakePayload{Colors: []SLGem{SLDiamond, SLRuby, SLOnyx}}}})
	if game.Turns != 1 || game.CurrentSeat == seat {
		t.Fatalf("정상 요청이 반영되지 않았다 (turns=%d seat=%d)", game.Turns, game.CurrentSeat)
	}
	if game.LastAction == nil || !hasHangul(game.LastAction.Message) {
		t.Fatalf("lastAction = %+v", game.LastAction)
	}
}

// TestSLAfkAutoProgress 접속만 유지한 채 아무도 행동하지 않는 2인전 —
// 차례 마감(자동 행동)과 버리기 마감(자동 버리기)만으로 판이 완주하는지
func TestSLAfkAutoProgress(t *testing.T) {
	_, url, cleanup := newSLTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	conns := make([]*slTestClient, SLMinPlayers)
	for i := range conns {
		conns[i] = slDial(t, url)
		defer conns[i].conn.Close()
		slJoin(t, conns[i], fmt.Sprintf("잠수%d", i), "")
	}
	host := conns[0]
	host.send(t, SLMessage{Type: SLMsgStart})

	state := host.slWaitPhase(t, string(SLPhaseTurn))
	if ends := int64(state["endsAt"].(float64)); ends <= 0 {
		t.Fatalf("차례 스냅샷의 endsAt = %d, want unixMillis", ends)
	}
	if state["lastRound"] != false {
		t.Fatalf("시작 lastRound = %v", state["lastRound"])
	}
	bank := state["bank"].(map[string]interface{})
	if int(bank["onyx"].(float64)) != slBankFor(SLMinPlayers) ||
		int(bank["gold"].(float64)) != SLGoldCount {
		t.Fatalf("2인 공동 창고 = %v", bank)
	}
	board := state["board"].(map[string]interface{})
	for _, tier := range []string{"tier1", "tier2", "tier3"} {
		row, ok := board[tier].([]interface{})
		if !ok || len(row) != SLBoardSlots {
			t.Fatalf("%s 진열 = %v", tier, board[tier])
		}
		card := row[0].(map[string]interface{})
		cost := card["cost"].(map[string]interface{})
		if _, ok := cost["onyx"]; !ok { // 와이어 영문 키 고정 (onyx = 줄마노)
			t.Fatalf("카드 비용 키 = %v", cost)
		}
		if _, leaked := cost["gold"]; leaked {
			t.Fatalf("비용에 황금 키가 있다: %v", cost)
		}
	}
	if len(state["nobles"].([]interface{})) != slNobleCount(SLMinPlayers) {
		t.Fatalf("귀족 타일 = %v", state["nobles"])
	}
	if _, ok := state["yourReserved"].([]interface{}); !ok {
		t.Fatalf("본인 스냅샷에 yourReserved 부재: %v", state["yourReserved"])
	}

	slDrainConn(conns[1])

	sawAfk, sawDiscard := false, false
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		msg := host.waitMatch(t, "event-or-over", func(m SLMessage) bool {
			return m.Type == SLMsgEvent || m.Type == SLMsgGameOver
		})
		if msg.Type == SLMsgEvent {
			ev := slPayloadMap(t, msg)
			switch ev["kind"] {
			case "afk":
				if !strings.Contains(ev["message"].(string), "자동") {
					t.Fatalf("afk 문구 = %v", ev["message"])
				}
				if ev["name"] == nil || ev["name"] == "" {
					t.Fatalf("afk 이벤트에 name 부재: %v", ev)
				}
				sawAfk = true
			case "discard_needed":
				sawDiscard = true
			}
			continue
		}
		over := slPayloadMap(t, msg)
		if !sawAfk {
			t.Fatal("afk 자동 진행 이벤트가 한 번도 없었다")
		}
		if !sawDiscard {
			t.Fatal("10개 상한 버리기 단계를 한 번도 안 거쳤다")
		}
		if seats, _ := over["winnerSeats"].([]interface{}); len(seats) == 0 {
			t.Fatalf("종료 payload = %v", over)
		}
		return
	}
	t.Fatal("전원 방치 게임이 60초 안에 끝나지 않았다")
}

// TestSLRoomCodeAndSpectate 방 코드 발급·코드 입장·시작 후 코드 관전 진입.
// 관전자 스냅샷은 yourSeat -1 에 yourReserved 부재. 행동은 전부 차단된다.
func TestSLRoomCodeAndSpectate(t *testing.T) {
	_, url, cleanup := newSLTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := slDial(t, url)
	defer host.conn.Close()
	joined := slJoin(t, host, "호스트", "NEW")
	code, _ := joined["roomCode"].(string)
	if len(code) != roomCodeLen {
		t.Fatalf("roomCode = %q, want %d자", code, roomCodeLen)
	}

	guests := make([]*slTestClient, SLFillBotTarget-1)
	for i := range guests {
		guests[i] = slDial(t, url)
		defer guests[i].conn.Close()
		g := slJoin(t, guests[i], fmt.Sprintf("친구%d", i), code)
		if g["roomCode"] != code || int(g["yourSeat"].(float64)) != i+1 {
			t.Fatalf("코드 입장 실패: %v", g)
		}
	}

	host.send(t, SLMessage{Type: SLMsgStart})
	state := host.slWaitPhase(t, string(SLPhaseTurn))
	if state["roomCode"] != code || len(state["players"].([]interface{})) != SLFillBotTarget {
		t.Fatalf("시작 실패: %v", state["players"])
	}
	for _, pRaw := range state["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		if int(p["points"].(float64)) != 0 || int(p["reservedCount"].(float64)) != 0 {
			t.Fatalf("시작 좌석 상태 = %v", p)
		}
		if _, ok := p["tokens"]; !ok {
			t.Fatalf("좌석 토큰 부재: %v", p)
		}
		if _, ok := p["nobles"].([]interface{}); !ok {
			t.Fatalf("좌석 귀족 타일이 []가 아니다: %v", p["nobles"])
		}
	}
	for _, c := range guests {
		slDrainConn(c)
	}

	// 시작된 방의 코드로 들어오면 관전자
	spec := slDial(t, url)
	defer spec.conn.Close()
	spec.send(t, SLMessage{Type: SLMsgJoinGame, Payload: SLJoinGamePayload{Name: "구경꾼", Room: code}})
	specJoined := slPayloadMap(t, spec.waitFor(t, SLMsgSpectateJoined))
	if specJoined["roomCode"] != code {
		t.Fatalf("관전 입장 roomCode = %v", specJoined["roomCode"])
	}

	specState := slPayloadMap(t, spec.waitFor(t, SLMsgGameState))
	if int(specState["yourSeat"].(float64)) != -1 {
		t.Fatalf("관전자 yourSeat = %v, want -1", specState["yourSeat"])
	}
	if leaked, ok := specState["yourReserved"]; ok {
		t.Fatalf("관전자에게 예약 카드 유출: %v", leaked)
	}
	if specState["bank"] == nil || specState["board"] == nil {
		t.Fatalf("관전자에게 공개 정보가 없다: %v", specState)
	}

	// 관전자는 어떤 행동도 못 한다
	spec.send(t, SLMessage{Type: SLMsgTake,
		Payload: SLTakePayload{Colors: []SLGem{SLDiamond, SLRuby, SLOnyx}}})
	errPayload := slPayloadMap(t, spec.waitFor(t, SLMsgError))
	if errPayload["message"] != spectatorDeniedMsg {
		t.Fatalf("관전자 차단 문구 = %v", errPayload["message"])
	}
}

// TestSLReconnect 재접속 3종 — 이탈 통지(sl_player_disconnected) 후 세션으로
// 돌아오면 좌석·예약이 그대로 복원되고(sl_player_reconnected), 모르는 세션은
// sl_session_expired 로 거절된다.
func TestSLReconnect(t *testing.T) {
	_, url, cleanup := newSLTestServer(t, 3*time.Second)
	defer cleanup()

	conns := make([]*slTestClient, SLFillBotTarget)
	sessions := make([]string, SLFillBotTarget)
	for i := range conns {
		conns[i] = slDial(t, url)
		joined := slJoin(t, conns[i], fmt.Sprintf("P%d", i), "")
		sessions[i], _ = joined["sessionId"].(string)
	}
	defer conns[0].conn.Close()
	conns[0].send(t, SLMessage{Type: SLMsgStart})
	conns[0].slWaitPhase(t, string(SLPhaseTurn))
	for _, c := range conns[2:] {
		slDrainConn(c)
	}

	// 좌석 1 이탈 → 남은 사람에게 이탈 통지
	conns[1].conn.Close()
	discon := slPayloadMap(t, conns[0].waitFor(t, SLMsgPlayerDisconnected))
	if int(discon["seat"].(float64)) != 1 || discon["name"] != "P1" {
		t.Fatalf("이탈 통지 = %v", discon)
	}
	if int(discon["graceSeconds"].(float64)) <= 0 {
		t.Fatalf("graceSeconds = %v", discon["graceSeconds"])
	}

	// 세션으로 재접속 → 좌석·예약 복원
	back := slDial(t, url)
	defer back.conn.Close()
	back.send(t, SLMessage{Type: SLMsgRejoin, Payload: SLRejoinPayload{SessionID: sessions[1]}})
	recon := slPayloadMap(t, back.waitFor(t, SLMsgPlayerReconnected))
	if int(recon["seat"].(float64)) != 1 {
		t.Fatalf("재접속 통지 = %v", recon)
	}
	restored := slPayloadMap(t, back.waitFor(t, SLMsgGameState))
	if int(restored["yourSeat"].(float64)) != 1 {
		t.Fatalf("복원 yourSeat = %v", restored["yourSeat"])
	}
	if _, ok := restored["yourReserved"].([]interface{}); !ok {
		t.Fatalf("복원 스냅샷에 yourReserved 부재: %v", restored)
	}

	// 모르는 세션은 만료 처리
	ghost := slDial(t, url)
	defer ghost.conn.Close()
	ghost.send(t, SLMessage{Type: SLMsgRejoin, Payload: SLRejoinPayload{SessionID: "없는-세션"}})
	ghost.waitFor(t, SLMsgSessionExpired)
}

// TestSLBotTakeover 유예 만료 좌석을 봇이 이어받아 게임이 멈추지 않는지
func TestSLBotTakeover(t *testing.T) {
	_, url, cleanup := newSLTestServer(t, 120*time.Millisecond)
	defer cleanup()

	conns := make([]*slTestClient, SLFillBotTarget)
	for i := range conns {
		conns[i] = slDial(t, url)
		defer conns[i].conn.Close()
		slJoin(t, conns[i], fmt.Sprintf("P%d", i), "")
	}
	conns[0].send(t, SLMessage{Type: SLMsgStart})
	conns[0].slWaitPhase(t, string(SLPhaseTurn))
	for _, c := range conns[2:] {
		slDrainConn(c)
	}

	// 좌석 1 이탈 → 유예 만료 → 봇 대체
	conns[1].conn.Close()
	sawTakeover := false
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		msg := conns[0].waitMatch(t, "event-or-over", func(m SLMessage) bool {
			return m.Type == SLMsgEvent || m.Type == SLMsgGameOver
		})
		if msg.Type == SLMsgEvent {
			ev := slPayloadMap(t, msg)
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
		if seats, _ := slPayloadMap(t, msg)["winnerSeats"].([]interface{}); len(seats) == 0 {
			t.Fatalf("종료 payload = %v", msg.Payload)
		}
		return
	}
	t.Fatal("봇 대체 후 게임이 60초 안에 끝나지 않았다")
}

// TestSLReactAndLobby 리액션은 좌석 보유자만·화이트리스트만, 대기 현황판은
// 사람이 대기할 때만 켜진다
func TestSLReactAndLobby(t *testing.T) {
	_, url, cleanup := newSLTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	a := slDial(t, url)
	defer a.conn.Close()
	b := slDial(t, url)
	defer b.conn.Close()
	slJoin(t, a, "가", "")
	slJoin(t, b, "나", "")

	a.send(t, SLMessage{Type: SLMsgReact, Payload: SLReactPayload{Emoji: "🔥"}})
	ev := slPayloadMap(t, b.waitMatch(t, "react", func(m SLMessage) bool {
		if m.Type != SLMsgEvent {
			return false
		}
		p, ok := m.Payload.(map[string]interface{})
		return ok && p["kind"] == "react"
	}))
	if ev["message"] != "🔥" || ev["name"] != "가" || int(ev["seat"].(float64)) != 0 {
		t.Fatalf("리액션 이벤트 = %v", ev)
	}

	// 화이트리스트 밖 이모지는 조용히 무시된다 — 다음에 오는 것은 시작 스냅샷이다
	a.send(t, SLMessage{Type: SLMsgReact, Payload: SLReactPayload{Emoji: "💀"}})
	a.send(t, SLMessage{Type: SLMsgStart})
	state := a.slWaitPhase(t, string(SLPhaseTurn))
	if int(state["hostSeat"].(float64)) != 0 {
		t.Fatalf("hostSeat = %v", state["hostSeat"])
	}
}
