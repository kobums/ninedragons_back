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

// 테스트에서는 차례 마감과 봇의 생각 시간을 짧게 낮춘다 (실사용은 차례 45초)
func init() {
	suTurnTimeout = 120 * time.Millisecond
	suBotDelay = 0
	suBotJitterMs = 0
}

// suTestClient 공용 testConn 에 스타트업스 메시지 타입의 waitFor 를 얹은 래퍼
type suTestClient struct {
	testConn[SUMessage]
}

func newSUTestServer(t *testing.T, grace time.Duration) (*SUHub, string, func()) {
	t.Helper()
	hub := NewSUHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeSUWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func suDial(t *testing.T, url string) *suTestClient {
	t.Helper()
	return &suTestClient{dialWS[SUMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *suTestClient) waitFor(t *testing.T, msgType SUMessageType) SUMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m SUMessage) bool { return m.Type == msgType })
}

func suPayloadMap(t *testing.T, msg SUMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// suJoin 입장하고 su_player_joined payload 를 돌려준다
func suJoin(t *testing.T, c *suTestClient, name, room string) map[string]interface{} {
	t.Helper()
	c.send(t, SUMessage{Type: SUMsgJoinGame, Payload: SUJoinGamePayload{Name: name, Room: room}})
	return suPayloadMap(t, c.waitFor(t, SUMsgPlayerJoined))
}

// suWaitPhase 지정한 phase 의 스냅샷이 올 때까지 소비
func (c *suTestClient) suWaitPhase(t *testing.T, phase string) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "su_game_state("+phase+")", func(m SUMessage) bool {
		if m.Type != SUMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		return ok && state["phase"] == phase
	})
	return suPayloadMap(t, msg)
}

// suDrainConn 배경으로 소켓을 비운다 (읽지 않는 연결의 버퍼 포화 방지)
func suDrainConn(c *suTestClient) {
	conn := c.conn
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

// TestSUFourBotsCompleteGame 봇을 채운 4인 게임이 90초 안에 완주하는지 —
// 가장 중요한 회귀 장치 (차례 교착·안티 계산 오류·종료 판정 감지).
// 좌석 0은 서버 연습봇 두뇌(suBrain)를 WS 로 감싼 드라이버가 잡는다.
func TestSUFourBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newSUTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := suDial(t, url)
	defer c.conn.Close()
	suJoin(t, c, "감독", "")
	c.send(t, SUMessage{Type: SUMsgFillBots}) // 4인까지 채우고 즉시 시작

	start := time.Now()
	brain := newSUBrain()
	deadline := start.Add(90 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "state-or-over", func(m SUMessage) bool {
			return m.Type == SUMsgGameState || m.Type == SUMsgGameOver
		})
		if msg.Type == SUMsgGameOver {
			over := suPayloadMap(t, msg)
			seats, _ := over["winnerSeats"].([]interface{})
			names, _ := over["winnerNames"].([]interface{})
			if len(seats) == 0 || len(seats) != len(names) {
				t.Fatalf("승자 = %v / %v", over["winnerSeats"], over["winnerNames"])
			}
			if m, _ := over["message"].(string); m == "" || !hasHangul(m) {
				t.Fatalf("종료 문구 = %v", over["message"])
			}
			turns := int(over["turns"].(float64))
			if turns < 1 || turns >= SUMaxTurns {
				t.Fatalf("turns = %d", turns)
			}
			rows, _ := over["rows"].([]interface{})
			if len(rows) != SUFillBotTarget {
				t.Fatalf("정산 표 길이 = %d, want %d", len(rows), SUFillBotTarget)
			}
			for _, rRaw := range rows {
				row := rRaw.(map[string]interface{})
				if _, ok := row["seat"]; !ok {
					t.Fatalf("정산 행에 seat 부재: %v", row)
				}
				if d, _ := row["detail"].(string); !hasHangul(d) {
					t.Fatalf("정산 설명이 한글이 아니다: %v", row["detail"])
				}
			}

			players := over["players"].([]interface{})
			if len(players) != SUFillBotTarget {
				t.Fatalf("players 길이 = %d, want %d", len(players), SUFillBotTarget)
			}
			best, cards := -1, 0
			for _, pRaw := range players {
				p := pRaw.(map[string]interface{})
				money := int(p["money"].(float64))
				if money > best {
					best = money
				}
				// 종료 화면에도 남의 손패 내용은 없다 (정산 때 앞면 더미로 합쳐진다)
				if _, leaked := p["hand"]; leaked {
					t.Fatalf("종료 화면에 손패 유출: %v", p)
				}
				faceUp, ok := p["faceUp"].(map[string]interface{})
				if !ok || len(faceUp) != 6 {
					t.Fatalf("faceUp = %v", p["faceUp"])
				}
				for _, v := range faceUp {
					cards += int(v.(float64))
				}
			}
			if best <= 0 {
				t.Fatalf("최고 최종 돈 = %d", best)
			}
			t.Logf("완주: 승자 %v · 최고 %d원 · %d차례 · 최종 보유 %d장 (%.1fs)",
				over["winnerNames"], best, turns, cards, time.Since(start).Seconds())
			return
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("90초 안에 게임이 끝나지 않았다 — 진행 불가 상태")
}

// suTakeMessages 봇 채널에 쌓인 메시지를 모두 꺼낸다
func suTakeMessages(t *testing.T, c *SUClient) []SUMessage {
	t.Helper()
	out := []SUMessage{}
	for {
		select {
		case data := <-c.Send:
			var msg SUMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			out = append(out, msg)
		default:
			return out
		}
	}
}

// TestSUHiddenState 은닉의 핵심 계약 — 허브 고루틴 없이 핸들러를 직접 불러
// 결정적으로 검증한다.
//   - yourHand 는 본인 스냅샷에만 (타인·관전자 raw JSON 에 키 부재)
//   - 게임에서 제외한 3장은 어떤 스냅샷에도 없다 (카드 총량 회계로 증명)
//   - 관전자(viewerSeat -1)·좌석 없는 방 스냅샷이 패닉 없이 만들어지고
//     빈 슬라이스는 [] 다
func TestSUHiddenState(t *testing.T) {
	h, room, clients := suBotFixture(t, 3, 20260828)
	game := room.Game

	rawOf := func(viewer int) string {
		data, err := json.Marshal(h.buildSUState(room, viewer))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(data)
	}

	// 와이어 계약을 눈으로 확인할 수 있게 남긴다 (-v 로만 보인다)
	t.Logf("본인(seat0) 스냅샷: %s", rawOf(0))
	t.Logf("관전자 스냅샷:     %s", rawOf(-1))

	// ---- yourHand 는 본인에게만 ----
	if strings.Count(rawOf(0), `"yourHand"`) != 1 {
		t.Fatalf("yourHand 키가 본인 것 하나가 아니다:\n%s", rawOf(0))
	}
	if strings.Contains(rawOf(-1), `"yourHand"`) {
		t.Fatalf("관전자 스냅샷에 yourHand 키 유출:\n%s", rawOf(-1))
	}
	for _, viewer := range []int{0, 1, 2} {
		state := h.buildSUState(room, viewer)
		if state.YourHand == nil || len(*state.YourHand) != SUStartHand {
			t.Fatalf("viewer %d yourHand = %v", viewer, state.YourHand)
		}
		if (*state.YourHand)[0] != game.Players[viewer].Hand[0] {
			t.Fatalf("viewer %d 손패가 자기 것이 아니다", viewer)
		}
		// 남의 손패는 숫자만 보인다
		for _, pv := range state.Players {
			if pv.HandCount != len(game.Players[pv.Seat].Hand) {
				t.Fatalf("handCount = %d", pv.HandCount)
			}
		}
	}
	// 빈 손패도 [] 로 나간다
	game.Players[0].Hand = []SUCompany{}
	if !strings.Contains(rawOf(0), `"yourHand":[]`) {
		t.Fatalf("빈 손패가 []가 아니다:\n%s", rawOf(0))
	}
	game.Players[0].Hand = []SUCompany{SUGeeks}

	// ---- 제외한 3장은 어떤 스냅샷에도 없다 ----
	if len(game.Removed) != SURemovedCards {
		t.Fatalf("제외 장수 = %d", len(game.Removed))
	}
	countVisible := func(viewer int) int {
		state := h.buildSUState(room, viewer)
		total := state.DeckLeft + len(state.Market)
		for _, pv := range state.Players {
			total += pv.HandCount
			for _, n := range pv.FaceUp {
				total += n
			}
		}
		return total
	}
	wantVisible := 33 - SURemovedCards
	for _, viewer := range []int{0, 1, 2, -1} {
		if got := countVisible(viewer); got != wantVisible {
			t.Fatalf("viewer %d 가 세는 카드 총량 = %d, want %d (제외 3장이 스냅샷에 샜다)",
				viewer, got, wantVisible)
		}
	}
	for _, viewer := range []int{0, -1} {
		if strings.Contains(rawOf(viewer), "removed") || strings.Contains(rawOf(viewer), `"deck":`) {
			t.Fatalf("viewer %d 스냅샷에 덱/제외 카드 필드 유출:\n%s", viewer, rawOf(viewer))
		}
	}
	// 이벤트에도 제외 3장·덱 내용은 담기지 않는다
	for _, ev := range game.DrainEvents() {
		for _, def := range suCompanyDefs {
			if strings.Contains(ev.Message, def.Name) {
				t.Fatalf("이벤트에 회사명이 실렸다(덱 정보 유출 위험): %q", ev.Message)
			}
		}
	}

	// ---- 관전자 스냅샷 ----
	spec := h.buildSUState(room, -1)
	if spec.YourSeat != -1 || spec.YourHand != nil {
		t.Fatalf("관전자 스냅샷: yourSeat=%d hand=%v", spec.YourSeat, spec.YourHand)
	}
	for _, key := range []string{`"market":[`, `"companies":[`, `"players":[`,
		`"deckLeft":`, `"deckAnte":`} {
		if !strings.Contains(rawOf(-1), key) {
			t.Fatalf("관전자 raw 스냅샷에 %s 부재:\n%s", key, rawOf(-1))
		}
	}
	// 회사 현황판은 전원 공개 (6종·대주주 좌석 포함)
	if len(spec.Companies) != 6 {
		t.Fatalf("회사 현황판 = %+v", spec.Companies)
	}
	for _, ci := range spec.Companies {
		if ci.Size != suSize(ci.ID) || !hasHangul(ci.Name) {
			t.Fatalf("회사 현황판 항목 = %+v", ci)
		}
	}

	// ---- 좌석 없는 빈 방도 패닉 없이 [] 로 나간다 ----
	empty := h.lobbyRoomFor("ZZZZ")
	raw, _ := json.Marshal(h.buildSUState(empty, -1))
	for _, key := range []string{`"players":[]`, `"market":[]`, `"companies":[`,
		`"result":null`, `"lastAction":null`, `"yourSeat":-1`, `"currentSeat":-1`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("빈 방 스냅샷에 %s 부재:\n%s", key, raw)
		}
	}
	if strings.Contains(string(raw), `"yourHand"`) {
		t.Fatalf("시작 전 방에 yourHand 유출:\n%s", raw)
	}
	json.Marshal(h.buildSUState(empty, 0)) // 좌석이 없는 방의 좌석 뷰 — 패닉 금지

	// ---- 차례가 아닌 좌석의 행동은 거부된다 ----
	for _, c := range clients {
		suTakeMessages(t, c)
	}
	seat := game.CurrentSeat
	other := (seat + 1) % len(game.Players)
	deckBefore := len(game.Deck)
	h.handleGameMessage(SUGameMessage{Client: clients[other], Message: SUMessage{
		Type: SUMsgTake, Payload: SUTakePayload{From: SUTakeDeck}}})
	if len(game.Deck) != deckBefore || game.CurrentSeat != seat {
		t.Fatal("차례가 아닌 좌석의 행동이 통과했다")
	}
	sawError := false
	for _, out := range suTakeMessages(t, clients[other]) {
		if out.Type != SUMsgError {
			continue
		}
		text, _ := asPayloadMap(t, out.Payload)["message"].(string)
		if !hasHangul(text) {
			t.Fatalf("오류 문구가 한글이 아니다: %q", text)
		}
		sawError = true
	}
	if !sawError {
		t.Fatal("차례가 아닌 행동에 su_error 가 없다")
	}
}

// TestSURejections 와이어로 들어온 잘못된 요청이 한글 오류로 되돌아오고
// 판을 건드리지 않는지
func TestSURejections(t *testing.T) {
	h, room, clients := suBotFixture(t, 3, 777)
	game := room.Game
	seat := game.CurrentSeat
	game.Market = []SUMarketCard{{Company: SUGaga, Ante: 1}}

	bad := []SUMessage{
		{Type: SUMsgTake, Payload: SUTakePayload{From: ""}},          // 지정 없음
		{Type: SUMsgTake, Payload: SUTakePayload{From: "bank"}},      // 없는 출처
		{Type: SUMsgTake, Payload: SUTakePayload{From: "market:"}},   // 인덱스 없음
		{Type: SUMsgTake, Payload: SUTakePayload{From: "market:x"}},  // 숫자 아님
		{Type: SUMsgTake, Payload: SUTakePayload{From: "market:9"}},  // 없는 시장 카드
		{Type: SUMsgTake, Payload: SUTakePayload{From: "market:-1"}}, // 음수 인덱스
		{Type: SUMsgPlay, Payload: SUPlayPayload{Index: 0}},          // 가져오기 전 내려놓기
		{Type: SUMsgPlay, Payload: SUPlayPayload{Index: 99}},         // 없는 손패
	}
	for i, msg := range bad {
		for _, c := range clients {
			suTakeMessages(t, c)
		}
		deckBefore, turnsBefore, phaseBefore := len(game.Deck), game.Turns, game.Phase
		h.handleGameMessage(SUGameMessage{Client: clients[seat], Message: msg})
		if game.Turns != turnsBefore || game.CurrentSeat != seat ||
			len(game.Deck) != deckBefore || game.Phase != phaseBefore {
			t.Fatalf("%d번째 잘못된 요청이 판을 바꿨다", i)
		}
		sawError := false
		for _, out := range suTakeMessages(t, clients[seat]) {
			if out.Type != SUMsgError {
				continue
			}
			text, _ := asPayloadMap(t, out.Payload)["message"].(string)
			if text == "" || !hasHangul(text) {
				t.Fatalf("%d번째 오류 문구가 한글이 아니다: %q", i, text)
			}
			sawError = true
		}
		if !sawError {
			t.Fatalf("%d번째 잘못된 요청에 su_error 가 없다", i)
		}
	}

	// 정상 요청은 통과하고 단계가 넘어간다
	h.handleGameMessage(SUGameMessage{Client: clients[seat], Message: SUMessage{
		Type: SUMsgTake, Payload: SUTakePayload{From: SUTakeDeck}}})
	if game.Phase != SUPhasePlay {
		t.Fatalf("정상 요청이 반영되지 않았다 (phase=%s)", game.Phase)
	}
	if game.LastAction == nil || !hasHangul(game.LastAction.Message) {
		t.Fatalf("lastAction = %+v", game.LastAction)
	}
	h.handleGameMessage(SUGameMessage{Client: clients[seat], Message: SUMessage{
		Type: SUMsgPlay, Payload: SUPlayPayload{Index: 0}}})
	if game.Turns != 1 || game.CurrentSeat == seat {
		t.Fatalf("내려놓기가 반영되지 않았다 (turns=%d seat=%d)", game.Turns, game.CurrentSeat)
	}
}

// TestSUAfkAutoProgress 접속만 유지한 채 아무도 행동하지 않는 3인전 —
// 차례 마감(자동 가져오기 + 자동 내려놓기)만으로 판이 완주하는지
func TestSUAfkAutoProgress(t *testing.T) {
	_, url, cleanup := newSUTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	conns := make([]*suTestClient, SUMinPlayers)
	for i := range conns {
		conns[i] = suDial(t, url)
		defer conns[i].conn.Close()
		suJoin(t, conns[i], fmt.Sprintf("잠수%d", i), "")
	}
	host := conns[0]
	host.send(t, SUMessage{Type: SUMsgStart})

	state := host.suWaitPhase(t, string(SUPhaseTake))
	if ends := int64(state["endsAt"].(float64)); ends <= 0 {
		t.Fatalf("차례 스냅샷의 endsAt = %d, want unixMillis", ends)
	}
	if int(state["deckLeft"].(float64)) != 33-SURemovedCards-SUMinPlayers {
		t.Fatalf("덱 잔량 = %v", state["deckLeft"])
	}
	if int(state["deckAnte"].(float64)) != 0 {
		t.Fatalf("시작 덱 안티 = %v", state["deckAnte"])
	}
	if market, ok := state["market"].([]interface{}); !ok || len(market) != 0 {
		t.Fatalf("시작 시장 = %v (빈 시장도 [] 여야 한다)", state["market"])
	}
	companies, _ := state["companies"].([]interface{})
	if len(companies) != 6 {
		t.Fatalf("회사 현황판 = %v", state["companies"])
	}
	for _, ciRaw := range companies {
		ci := ciRaw.(map[string]interface{})
		if int(ci["majoritySeat"].(float64)) != -1 {
			t.Fatalf("시작 대주주 = %v", ci)
		}
		if _, ok := ci["size"]; !ok {
			t.Fatalf("회사 size 부재: %v", ci)
		}
	}
	for _, pRaw := range state["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		if int(p["money"].(float64)) != SUStartMoney {
			t.Fatalf("시작 돈 = %v", p["money"])
		}
		if int(p["handCount"].(float64)) != SUStartHand {
			t.Fatalf("시작 손패 수 = %v", p["handCount"])
		}
		if _, leaked := p["hand"]; leaked {
			t.Fatalf("남의 손패 유출: %v", p)
		}
	}
	if _, ok := state["yourHand"].([]interface{}); !ok {
		t.Fatalf("본인 스냅샷에 yourHand 부재: %v", state["yourHand"])
	}

	for _, c := range conns[1:] {
		suDrainConn(c)
	}

	sawAfk := false
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		msg := host.waitMatch(t, "event-or-over", func(m SUMessage) bool {
			return m.Type == SUMsgEvent || m.Type == SUMsgGameOver
		})
		if msg.Type == SUMsgEvent {
			ev := suPayloadMap(t, msg)
			if ev["kind"] == "afk" {
				if !strings.Contains(ev["message"].(string), "자동") {
					t.Fatalf("afk 문구 = %v", ev["message"])
				}
				if ev["name"] == nil || ev["name"] == "" {
					t.Fatalf("afk 이벤트에 name 부재: %v", ev)
				}
				sawAfk = true
			}
			continue
		}
		over := suPayloadMap(t, msg)
		if !sawAfk {
			t.Fatal("afk 자동 진행 이벤트가 한 번도 없었다")
		}
		if seats, _ := over["winnerSeats"].([]interface{}); len(seats) == 0 {
			t.Fatalf("종료 payload = %v", over)
		}
		return
	}
	t.Fatal("전원 방치 게임이 60초 안에 끝나지 않았다")
}

// TestSURoomCodeAndSpectate 방 코드 발급·코드 입장·시작 후 코드 관전 진입.
// 관전자 스냅샷은 yourSeat -1 에 yourHand 부재. 행동은 전부 차단된다.
func TestSURoomCodeAndSpectate(t *testing.T) {
	_, url, cleanup := newSUTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := suDial(t, url)
	defer host.conn.Close()
	joined := suJoin(t, host, "호스트", "NEW")
	code, _ := joined["roomCode"].(string)
	if len(code) != roomCodeLen {
		t.Fatalf("roomCode = %q, want %d자", code, roomCodeLen)
	}

	guests := make([]*suTestClient, SUFillBotTarget-1)
	for i := range guests {
		guests[i] = suDial(t, url)
		defer guests[i].conn.Close()
		g := suJoin(t, guests[i], fmt.Sprintf("친구%d", i), code)
		if g["roomCode"] != code || int(g["yourSeat"].(float64)) != i+1 {
			t.Fatalf("코드 입장 실패: %v", g)
		}
	}

	host.send(t, SUMessage{Type: SUMsgStart})
	state := host.suWaitPhase(t, string(SUPhaseTake))
	if state["roomCode"] != code || len(state["players"].([]interface{})) != SUFillBotTarget {
		t.Fatalf("시작 실패: %v", state["players"])
	}
	for _, c := range guests {
		suDrainConn(c)
	}

	// 시작된 방의 코드로 들어오면 관전자
	spec := suDial(t, url)
	defer spec.conn.Close()
	spec.send(t, SUMessage{Type: SUMsgJoinGame, Payload: SUJoinGamePayload{Name: "구경꾼", Room: code}})
	specJoined := suPayloadMap(t, spec.waitFor(t, SUMsgSpectateJoined))
	if specJoined["roomCode"] != code {
		t.Fatalf("관전 입장 roomCode = %v", specJoined["roomCode"])
	}

	specState := suPayloadMap(t, spec.waitFor(t, SUMsgGameState))
	if int(specState["yourSeat"].(float64)) != -1 {
		t.Fatalf("관전자 yourSeat = %v, want -1", specState["yourSeat"])
	}
	if leaked, ok := specState["yourHand"]; ok {
		t.Fatalf("관전자에게 손패 유출: %v", leaked)
	}
	if specState["market"] == nil || specState["companies"] == nil {
		t.Fatalf("관전자에게 공개 정보가 없다: %v", specState)
	}

	// 관전자는 어떤 행동도 못 한다
	spec.send(t, SUMessage{Type: SUMsgTake, Payload: SUTakePayload{From: SUTakeDeck}})
	errPayload := suPayloadMap(t, spec.waitFor(t, SUMsgError))
	if errPayload["message"] != spectatorDeniedMsg {
		t.Fatalf("관전자 차단 문구 = %v", errPayload["message"])
	}
}

// TestSUReconnect 재접속 3종 — 이탈 통지(su_player_disconnected) 후 세션으로
// 돌아오면 좌석·손패가 그대로 복원되고(su_player_reconnected), 모르는 세션은
// su_session_expired 로 거절된다.
func TestSUReconnect(t *testing.T) {
	_, url, cleanup := newSUTestServer(t, 3*time.Second)
	defer cleanup()

	conns := make([]*suTestClient, SUFillBotTarget)
	sessions := make([]string, SUFillBotTarget)
	for i := range conns {
		conns[i] = suDial(t, url)
		joined := suJoin(t, conns[i], fmt.Sprintf("P%d", i), "")
		sessions[i], _ = joined["sessionId"].(string)
	}
	defer conns[0].conn.Close()
	conns[0].send(t, SUMessage{Type: SUMsgStart})
	conns[0].suWaitPhase(t, string(SUPhaseTake))
	for _, c := range conns[2:] {
		suDrainConn(c)
	}

	// 좌석 1 이탈 → 남은 사람에게 이탈 통지
	conns[1].conn.Close()
	discon := suPayloadMap(t, conns[0].waitFor(t, SUMsgPlayerDisconnected))
	if int(discon["seat"].(float64)) != 1 || discon["name"] != "P1" {
		t.Fatalf("이탈 통지 = %v", discon)
	}
	if int(discon["graceSeconds"].(float64)) <= 0 {
		t.Fatalf("graceSeconds = %v", discon["graceSeconds"])
	}

	// 세션으로 재접속 → 좌석·손패 복원
	back := suDial(t, url)
	defer back.conn.Close()
	back.send(t, SUMessage{Type: SUMsgRejoin, Payload: SURejoinPayload{SessionID: sessions[1]}})
	recon := suPayloadMap(t, back.waitFor(t, SUMsgPlayerReconnected))
	if int(recon["seat"].(float64)) != 1 {
		t.Fatalf("재접속 통지 = %v", recon)
	}
	restored := suPayloadMap(t, back.waitFor(t, SUMsgGameState))
	if int(restored["yourSeat"].(float64)) != 1 {
		t.Fatalf("복원 yourSeat = %v", restored["yourSeat"])
	}
	if _, ok := restored["yourHand"].([]interface{}); !ok {
		t.Fatalf("복원 스냅샷에 yourHand 부재: %v", restored)
	}

	// 모르는 세션은 만료 처리
	ghost := suDial(t, url)
	defer ghost.conn.Close()
	ghost.send(t, SUMessage{Type: SUMsgRejoin, Payload: SURejoinPayload{SessionID: "없는-세션"}})
	ghost.waitFor(t, SUMsgSessionExpired)
}

// TestSUBotTakeover 유예 만료 좌석을 봇이 이어받아 게임이 멈추지 않는지
func TestSUBotTakeover(t *testing.T) {
	_, url, cleanup := newSUTestServer(t, 120*time.Millisecond)
	defer cleanup()

	conns := make([]*suTestClient, SUFillBotTarget)
	for i := range conns {
		conns[i] = suDial(t, url)
		defer conns[i].conn.Close()
		suJoin(t, conns[i], fmt.Sprintf("P%d", i), "")
	}
	conns[0].send(t, SUMessage{Type: SUMsgStart})
	conns[0].suWaitPhase(t, string(SUPhaseTake))
	for _, c := range conns[2:] {
		suDrainConn(c)
	}

	// 좌석 1 이탈 → 유예 만료 → 봇 대체
	conns[1].conn.Close()
	sawTakeover := false
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		msg := conns[0].waitMatch(t, "event-or-over", func(m SUMessage) bool {
			return m.Type == SUMsgEvent || m.Type == SUMsgGameOver
		})
		if msg.Type == SUMsgEvent {
			ev := suPayloadMap(t, msg)
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
		if seats, _ := suPayloadMap(t, msg)["winnerSeats"].([]interface{}); len(seats) == 0 {
			t.Fatalf("종료 payload = %v", msg.Payload)
		}
		return
	}
	t.Fatal("봇 대체 후 게임이 60초 안에 끝나지 않았다")
}

// TestSUReactAndLobby 리액션은 좌석 보유자만·화이트리스트만, 대기 현황판은
// 사람이 대기할 때만 켜진다
func TestSUReactAndLobby(t *testing.T) {
	_, url, cleanup := newSUTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	conns := make([]*suTestClient, SUMinPlayers)
	for i := range conns {
		conns[i] = suDial(t, url)
		defer conns[i].conn.Close()
		suJoin(t, conns[i], fmt.Sprintf("가나다%d", i), "")
	}
	a, b := conns[0], conns[1]

	a.send(t, SUMessage{Type: SUMsgReact, Payload: SUReactPayload{Emoji: "🔥"}})
	ev := suPayloadMap(t, b.waitMatch(t, "react", func(m SUMessage) bool {
		if m.Type != SUMsgEvent {
			return false
		}
		p, ok := m.Payload.(map[string]interface{})
		return ok && p["kind"] == "react"
	}))
	if ev["message"] != "🔥" || ev["name"] != "가나다0" || int(ev["seat"].(float64)) != 0 {
		t.Fatalf("리액션 이벤트 = %v", ev)
	}

	// 화이트리스트 밖 이모지는 조용히 무시된다 — 다음에 오는 것은 시작 스냅샷이다
	a.send(t, SUMessage{Type: SUMsgReact, Payload: SUReactPayload{Emoji: "💀"}})
	a.send(t, SUMessage{Type: SUMsgStart})
	state := a.suWaitPhase(t, string(SUPhaseTake))
	if int(state["hostSeat"].(float64)) != 0 {
		t.Fatalf("hostSeat = %v", state["hostSeat"])
	}
}
