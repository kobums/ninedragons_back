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

// 테스트에서는 단계 마감과 봇의 생각 시간을 짧게 낮춘다
// (실사용은 심기 30초 · 거래 60초 · 받은 카드 심기 20초)
func init() {
	bzPlantTimeout = 120 * time.Millisecond
	bzTradeTimeout = 120 * time.Millisecond
	bzReceiveTimeout = 100 * time.Millisecond
	bzBotDelay = 0
	bzBotJitterMs = 0
}

// bzTestClient 공용 testConn 에 보난자 메시지 타입의 waitFor 를 얹은 래퍼
type bzTestClient struct {
	testConn[BZMessage]
}

func newBZTestServer(t *testing.T, grace time.Duration) (*BZHub, string, func()) {
	t.Helper()
	hub := NewBZHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeBZWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func bzDial(t *testing.T, url string) *bzTestClient {
	t.Helper()
	return &bzTestClient{dialWS[BZMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *bzTestClient) waitFor(t *testing.T, msgType BZMessageType) BZMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m BZMessage) bool { return m.Type == msgType })
}

func bzPayloadMap(t *testing.T, msg BZMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// bzJoin 입장하고 bz_player_joined payload 를 돌려준다
func bzJoin(t *testing.T, c *bzTestClient, name, room string) map[string]interface{} {
	t.Helper()
	c.send(t, BZMessage{Type: BZMsgJoinGame, Payload: BZJoinGamePayload{Name: name, Room: room}})
	return bzPayloadMap(t, c.waitFor(t, BZMsgPlayerJoined))
}

// bzWaitPhase 지정한 phase 의 스냅샷이 올 때까지 소비
func (c *bzTestClient) bzWaitPhase(t *testing.T, phase string) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "bz_game_state("+phase+")", func(m BZMessage) bool {
		if m.Type != BZMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		return ok && state["phase"] == phase
	})
	return bzPayloadMap(t, msg)
}

// bzDrainConn 배경으로 소켓을 비운다 (읽지 않는 연결의 버퍼 포화 방지)
func bzDrainConn(c *bzTestClient) {
	conn := c.conn
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

// TestBZThreeBotsCompleteGame 봇을 채운 3인 게임이 120초 안에 완주하는지 —
// 가장 중요한 회귀 장치 (단계 교착·거래 상태 꼬임·덱 소진 종료 감지).
// 좌석 0은 서버 연습봇 두뇌(bzBrain)를 WS 로 감싼 드라이버가 잡는다.
func TestBZThreeBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newBZTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := bzDial(t, url)
	defer c.conn.Close()
	bzJoin(t, c, "감독", "")
	c.send(t, BZMessage{Type: BZMsgFillBots}) // 3인까지 채우고 즉시 시작

	start := time.Now()
	brain := newBZBrain()
	deadline := start.Add(120 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "state-or-over", func(m BZMessage) bool {
			return m.Type == BZMsgGameState || m.Type == BZMsgGameOver
		})
		if msg.Type == BZMsgGameOver {
			over := bzPayloadMap(t, msg)
			seats, _ := over["winnerSeats"].([]interface{})
			names, _ := over["winnerNames"].([]interface{})
			if len(seats) == 0 || len(seats) != len(names) {
				t.Fatalf("승자 = %v / %v", over["winnerSeats"], over["winnerNames"])
			}
			if m, _ := over["message"].(string); m == "" || !hasHangul(m) {
				t.Fatalf("종료 문구 = %v", over["message"])
			}
			turns := int(over["turns"].(float64))
			if turns < 1 || turns >= BZMaxTurns {
				t.Fatalf("turns = %d", turns)
			}
			rows, _ := over["rows"].([]interface{})
			if len(rows) != BZFillBotTarget {
				t.Fatalf("정산 표 길이 = %d, want %d", len(rows), BZFillBotTarget)
			}
			best := -1
			for _, rRaw := range rows {
				row := rRaw.(map[string]interface{})
				for _, key := range []string{"seat", "coins", "handCount"} {
					if _, ok := row[key]; !ok {
						t.Fatalf("정산 행에 %s 부재: %v", key, row)
					}
				}
				if coins := int(row["coins"].(float64)); coins > best {
					best = coins
				}
			}
			if best <= 0 {
				t.Fatalf("최고 금화 = %d — 아무도 수확하지 못했다", best)
			}

			players := over["players"].([]interface{})
			if len(players) != BZFillBotTarget {
				t.Fatalf("players 길이 = %d, want %d", len(players), BZFillBotTarget)
			}
			for _, pRaw := range players {
				p := pRaw.(map[string]interface{})
				// 종료 화면에도 남의 손패 내용은 없다
				for _, leaked := range []string{"hand", "yourHand", "pending"} {
					if _, bad := p[leaked]; bad {
						t.Fatalf("종료 화면에 %s 유출: %v", leaked, p)
					}
				}
				if _, ok := p["fields"].([]interface{}); !ok {
					t.Fatalf("fields = %v", p["fields"])
				}
			}
			t.Logf("완주: 승자 %v · 최고 금화 %d · %d차례 (%.1fs)",
				over["winnerNames"], best, turns, time.Since(start).Seconds())
			return
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("120초 안에 게임이 끝나지 않았다 — 진행 불가 상태")
}

// bzTakeMessages 봇 채널에 쌓인 메시지를 모두 꺼낸다
func bzTakeMessages(t *testing.T, c *BZClient) []BZMessage {
	t.Helper()
	out := []BZMessage{}
	for {
		select {
		case data := <-c.Send:
			var msg BZMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			out = append(out, msg)
		default:
			return out
		}
	}
}

// TestBZHiddenState 은닉의 핵심 계약 — 허브 고루틴 없이 핸들러를 직접 불러
// 결정적으로 검증한다.
//   - yourHand·yourPending 은 본인 스냅샷에만 (타인·관전자 raw JSON 에 키 부재)
//   - 제안의 요구 카드(wantHand)는 당사자 둘에게만
//   - 덱 내용은 남은 장수만 나간다
//   - 관전자(viewerSeat -1)·좌석 없는 방 스냅샷이 패닉 없이 만들어지고
//     빈 슬라이스는 [] 다
func TestBZHiddenState(t *testing.T) {
	h, room, clients := bzBotFixture(t, 3, 20260829)
	game := room.Game

	rawOf := func(viewer int) string {
		data, err := json.Marshal(h.buildBZState(room, viewer))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(data)
	}

	// 판을 고정한다 — 차례인 좌석 0이 2단계에서 제안을 하나 걸어 둔다
	bzSetBoard(game, 0, []BZBean{BZRed, BZBlue, BZGarden, BZGarden, BZGarden})
	for i, p := range game.Players {
		p.Hand = []BZBean{BZSoy, BZChili, BZStink}
		p.Coins = i
	}
	game.Players[0].Fields[0] = BZField{Bean: BZGreen, Count: 2}
	game.beginTradePhase()
	game.Players[2].Pending = []BZBean{BZBlackeyed}
	if _, err := game.Offer(0, BZOfferPayload{
		ToSeat: 1, GiveFlipped: []int{0}, WantHand: []int{0}}); err != nil {
		t.Fatalf("제안 실패: %v", err)
	}
	game.DrainEvents()

	// 와이어 계약을 눈으로 확인할 수 있게 남긴다 (-v 로만 보인다)
	t.Logf("본인(seat0) 스냅샷: %s", rawOf(0))
	t.Logf("제3자(seat2) 스냅샷: %s", rawOf(2))
	t.Logf("관전자 스냅샷:      %s", rawOf(-1))

	// ---- yourHand·yourPending 은 본인에게만 ----
	for _, key := range []string{`"yourHand"`, `"yourPending"`} {
		if strings.Count(rawOf(0), key) != 1 {
			t.Fatalf("%s 키가 본인 것 하나가 아니다:\n%s", key, rawOf(0))
		}
		if strings.Contains(rawOf(-1), key) {
			t.Fatalf("관전자 스냅샷에 %s 키 유출:\n%s", key, rawOf(-1))
		}
	}
	for _, viewer := range []int{0, 1, 2} {
		state := h.buildBZState(room, viewer)
		if state.YourHand == nil || len(*state.YourHand) != 3 {
			t.Fatalf("viewer %d yourHand = %v", viewer, state.YourHand)
		}
		if !bzSameSeq(*state.YourHand, game.Players[viewer].Hand) {
			t.Fatalf("viewer %d 손패가 자기 것이 아니다 (순서 포함)", viewer)
		}
		if state.YourPending == nil {
			t.Fatalf("viewer %d yourPending 부재", viewer)
		}
		// 남의 손패는 장수만 보인다
		for _, pv := range state.Players {
			if pv.HandCount != len(game.Players[pv.Seat].Hand) {
				t.Fatalf("handCount = %d", pv.HandCount)
			}
		}
	}
	// 받은 카드도 본인만 내용을 본다
	if got := h.buildBZState(room, 2).YourPending; got == nil || len(*got) != 1 {
		t.Fatalf("seat2 yourPending = %v", got)
	}
	if got := h.buildBZState(room, 0).YourPending; got == nil || len(*got) != 0 {
		t.Fatalf("seat0 yourPending = %v (빈 목록이어야 한다)", got)
	}
	if !strings.Contains(rawOf(0), `"yourPending":[]`) {
		t.Fatalf("빈 받은 카드가 []가 아니다:\n%s", rawOf(0))
	}
	// 빈 손패도 [] 로 나간다
	game.Players[0].Hand = []BZBean{}
	if !strings.Contains(rawOf(0), `"yourHand":[]`) {
		t.Fatalf("빈 손패가 []가 아니다:\n%s", rawOf(0))
	}
	game.Players[0].Hand = []BZBean{BZSoy, BZChili, BZStink}

	// ---- 제안 상세는 당사자 둘에게만, 요구한 자리의 콩은 주인에게만 ----
	//
	// 제안자에게 wantBeans 를 펼쳐 주면 "네 2번 카드를 달라"는 제안을 반복해
	// 수락 없이 상대 손패를 훑어낼 수 있다. 그래서 제안자에게는 자리(인덱스)만
	// 보이고 그 자리에 무엇이 있는지는 카드의 주인만 본다.
	from := h.buildBZState(room, 0) // 제안자
	if len(from.Offers) != 1 {
		t.Fatalf("제안자 제안 목록 = %+v", from.Offers)
	}
	if from.Offers[0].ID == "" {
		t.Fatalf("제안 id 가 비었다 (문자열이어야 한다): %+v", from.Offers[0])
	}
	if from.Offers[0].WantHand == nil || len(*from.Offers[0].WantHand) != 1 ||
		(*from.Offers[0].WantHand)[0] != 0 {
		t.Fatalf("제안자 wantHand = %v, want [0] (자리 인덱스)", from.Offers[0].WantHand)
	}
	if from.Offers[0].WantBeans != nil {
		t.Fatalf("제안자에게 wantBeans 유출: %v", *from.Offers[0].WantBeans)
	}
	if strings.Contains(rawOf(0), `"wantBeans"`) {
		t.Fatalf("제안자 raw JSON 에 wantBeans 키 유출:\n%s", rawOf(0))
	}
	if from.Offers[0].GiveFlipped == nil || len(*from.Offers[0].GiveFlipped) != 1 {
		t.Fatalf("제안자에게 내주는 카드가 안 보인다: %+v", from.Offers[0])
	}

	to := h.buildBZState(room, 1) // 요청받은 사람 — 자기 카드니 콩을 본다
	if to.Offers[0].WantBeans == nil || len(*to.Offers[0].WantBeans) != 1 ||
		(*to.Offers[0].WantBeans)[0] != BZSoy {
		t.Fatalf("요청받은 사람 wantBeans = %v, want [soy]", to.Offers[0].WantBeans)
	}
	if to.Offers[0].WantHand == nil || to.Offers[0].GiveFlipped == nil {
		t.Fatalf("요청받은 사람에게 상세가 없다: %+v", to.Offers[0])
	}
	if !strings.Contains(rawOf(1), `"wantBeans"`) {
		t.Fatalf("요청받은 사람 raw JSON 에 wantBeans 부재:\n%s", rawOf(1))
	}

	// 제3자·관전자는 "누가 누구에게 제안했다"만 본다
	for _, viewer := range []int{2, -1} {
		state := h.buildBZState(room, viewer)
		if len(state.Offers) != 1 {
			t.Fatalf("viewer %d 제안 목록 = %+v", viewer, state.Offers)
		}
		o := state.Offers[0]
		if o.WantHand != nil || o.WantBeans != nil || o.GiveHand != nil || o.GiveFlipped != nil {
			t.Fatalf("제3자 viewer %d 에게 제안 상세 유출: %+v", viewer, o)
		}
		if o.ID == "" || o.FromSeat != 0 || o.ToSeat != 1 {
			t.Fatalf("viewer %d 제안 요약 = %+v", viewer, o)
		}
		raw := rawOf(viewer)
		for _, key := range []string{`"wantHand"`, `"wantBeans"`, `"giveHand"`, `"giveFlipped"`} {
			if strings.Contains(raw, key) {
				t.Fatalf("viewer %d raw JSON 에 %s 키 유출:\n%s", viewer, key, raw)
			}
		}
	}

	// ---- 덱 내용은 남은 장수만 ----
	for _, viewer := range []int{0, -1} {
		raw := rawOf(viewer)
		if strings.Contains(raw, `"deck":`) || strings.Contains(raw, `"discard"`) {
			t.Fatalf("viewer %d 스냅샷에 덱/버린 더미 유출:\n%s", viewer, raw)
		}
		for _, key := range []string{`"deckLeft":`, `"deckCycle":`, `"flipped":[`,
			`"offers":[`, `"players":[`} {
			if !strings.Contains(raw, key) {
				t.Fatalf("viewer %d raw 스냅샷에 %s 부재:\n%s", viewer, key, raw)
			}
		}
	}

	// ---- 관전자 스냅샷 ----
	spec := h.buildBZState(room, -1)
	if spec.YourSeat != -1 || spec.YourHand != nil || spec.YourPending != nil {
		t.Fatalf("관전자 스냅샷: yourSeat=%d hand=%v pending=%v",
			spec.YourSeat, spec.YourHand, spec.YourPending)
	}
	// 밭·금화·공개 카드는 전원 공개
	for _, pv := range spec.Players {
		if len(pv.Fields) != len(game.Players[pv.Seat].Fields) {
			t.Fatalf("관전자에게 밭이 안 보인다: %+v", pv)
		}
		if pv.Coins != game.Players[pv.Seat].Coins {
			t.Fatalf("관전자 금화 = %d", pv.Coins)
		}
	}
	if len(spec.Flipped) != len(game.Flipped) {
		t.Fatalf("관전자 공개 카드 = %v", spec.Flipped)
	}

	// ---- 좌석 없는 빈 방도 패닉 없이 [] 로 나간다 ----
	empty := h.lobbyRoomFor("ZZZZ")
	raw, _ := json.Marshal(h.buildBZState(empty, -1))
	for _, key := range []string{`"players":[]`, `"flipped":[]`, `"offers":[]`,
		`"result":null`, `"lastAction":null`, `"yourSeat":-1`, `"currentSeat":-1`,
		`"deckCycle":0`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("빈 방 스냅샷에 %s 부재:\n%s", key, raw)
		}
	}
	for _, key := range []string{`"yourHand"`, `"yourPending"`} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("시작 전 방에 %s 유출:\n%s", key, raw)
		}
	}
	json.Marshal(h.buildBZState(empty, 0)) // 좌석이 없는 방의 좌석 뷰 — 패닉 금지

	// ---- 차례가 아닌 좌석의 심기는 거부된다 ----
	game.Phase = BZPhasePlant
	for _, c := range clients {
		bzTakeMessages(t, c)
	}
	other := (game.CurrentSeat + 1) % len(game.Players)
	handBefore := len(game.Players[other].Hand)
	h.handleGameMessage(BZGameMessage{Client: clients[other], Message: BZMessage{
		Type: BZMsgPlant, Payload: BZPlantPayload{Second: false}}})
	if len(game.Players[other].Hand) != handBefore {
		t.Fatal("차례가 아닌 좌석의 심기가 통과했다")
	}
	sawError := false
	for _, out := range bzTakeMessages(t, clients[other]) {
		if out.Type != BZMsgError {
			continue
		}
		text, _ := asPayloadMap(t, out.Payload)["message"].(string)
		if !hasHangul(text) {
			t.Fatalf("오류 문구가 한글이 아니다: %q", text)
		}
		sawError = true
	}
	if !sawError {
		t.Fatal("차례가 아닌 행동에 bz_error 가 없다")
	}
}

// TestBZRejections 와이어로 들어온 잘못된 요청이 한글 오류로 되돌아오고
// 판을 건드리지 않는지
func TestBZRejections(t *testing.T) {
	h, room, clients := bzBotFixture(t, 3, 777)
	game := room.Game
	bzSetBoard(game, 0, []BZBean{BZRed, BZBlue, BZGarden, BZGarden})
	for _, p := range game.Players {
		p.Hand = []BZBean{BZSoy, BZChili}
	}
	seat := 0

	bad := []BZMessage{
		{Type: BZMsgHarvest, Payload: BZHarvestPayload{Field: 0}},  // 빈 밭
		{Type: BZMsgHarvest, Payload: BZHarvestPayload{Field: 9}},  // 없는 밭
		{Type: BZMsgHarvest, Payload: BZHarvestPayload{Field: -1}}, // 음수 밭
		{Type: BZMsgBuyField}, // 금화 0개 (외상 불가)
		{Type: BZMsgPlantReceived, Payload: BZPlantReceivedPayload{ // 3단계가 아니다
			CardIndex: 0, Field: 0}},
		{Type: BZMsgEndPhase}, // 2단계가 아니다
		{Type: BZMsgOffer, Payload: BZOfferPayload{ToSeat: 1}},            // 2단계가 아니다
		{Type: BZMsgRespond, Payload: BZRespondPayload{OfferID: "없는-제안"}}, // 없는 제안
	}
	for i, msg := range bad {
		for _, c := range clients {
			bzTakeMessages(t, c)
		}
		deckBefore, turnsBefore, phaseBefore := len(game.Deck), game.Turns, game.Phase
		h.handleGameMessage(BZGameMessage{Client: clients[seat], Message: msg})
		if game.Turns != turnsBefore || game.CurrentSeat != seat ||
			len(game.Deck) != deckBefore || game.Phase != phaseBefore {
			t.Fatalf("%d번째 잘못된 요청이 판을 바꿨다", i)
		}
		sawError := false
		for _, out := range bzTakeMessages(t, clients[seat]) {
			if out.Type != BZMsgError {
				continue
			}
			text, _ := asPayloadMap(t, out.Payload)["message"].(string)
			if text == "" || !hasHangul(text) {
				t.Fatalf("%d번째 오류 문구가 한글이 아니다: %q", i, text)
			}
			sawError = true
		}
		if !sawError {
			t.Fatalf("%d번째 잘못된 요청에 bz_error 가 없다", i)
		}
	}

	// 정상 요청은 통과하고 단계가 넘어간다
	h.handleGameMessage(BZGameMessage{Client: clients[seat], Message: BZMessage{
		Type: BZMsgPlant, Payload: BZPlantPayload{Second: false}}})
	if game.Phase != BZPhaseTrade {
		t.Fatalf("정상 심기가 반영되지 않았다 (phase=%s)", game.Phase)
	}
	if game.LastAction == nil || !hasHangul(game.LastAction.Message) {
		t.Fatalf("lastAction = %+v", game.LastAction)
	}
	h.handleGameMessage(BZGameMessage{Client: clients[seat], Message: BZMessage{
		Type: BZMsgEndPhase}})
	if game.Phase == BZPhaseTrade {
		t.Fatalf("거래 마감이 반영되지 않았다 (phase=%s)", game.Phase)
	}
}

// TestBZAfkAutoProgress 접속만 유지한 채 아무도 행동하지 않는 3인전 —
// 단계 마감(자동 심기 · 거래 마감 · 받은 카드 자동 배치)만으로 완주하는지
func TestBZAfkAutoProgress(t *testing.T) {
	_, url, cleanup := newBZTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	conns := make([]*bzTestClient, BZMinPlayers)
	for i := range conns {
		conns[i] = bzDial(t, url)
		defer conns[i].conn.Close()
		bzJoin(t, conns[i], fmt.Sprintf("잠수%d", i), "")
	}
	host := conns[0]
	host.send(t, BZMessage{Type: BZMsgStart})

	state := host.bzWaitPhase(t, string(BZPhasePlant))
	if ends := int64(state["endsAt"].(float64)); ends <= 0 {
		t.Fatalf("심기 스냅샷의 endsAt = %d, want unixMillis", ends)
	}
	if int(state["deckLeft"].(float64)) != 104-BZStartHand*BZMinPlayers {
		t.Fatalf("덱 잔량 = %v", state["deckLeft"])
	}
	if int(state["deckCycle"].(float64)) != 0 {
		t.Fatalf("시작 소진 횟수 = %v", state["deckCycle"])
	}
	if flipped, ok := state["flipped"].([]interface{}); !ok || len(flipped) != 0 {
		t.Fatalf("시작 공개 카드 = %v (빈 경우도 [] 여야 한다)", state["flipped"])
	}
	if offers, ok := state["offers"].([]interface{}); !ok || len(offers) != 0 {
		t.Fatalf("시작 제안 = %v (빈 경우도 [] 여야 한다)", state["offers"])
	}
	for _, pRaw := range state["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		if int(p["coins"].(float64)) != 0 {
			t.Fatalf("시작 금화 = %v", p["coins"])
		}
		if int(p["handCount"].(float64)) != BZStartHand {
			t.Fatalf("시작 손패 수 = %v", p["handCount"])
		}
		if int(p["fieldCount"].(float64)) != BZStartFields {
			t.Fatalf("시작 밭 수 = %v", p["fieldCount"])
		}
		fields, ok := p["fields"].([]interface{})
		if !ok || len(fields) != BZStartFields {
			t.Fatalf("밭 = %v", p["fields"])
		}
		if _, leaked := p["hand"]; leaked {
			t.Fatalf("남의 손패 유출: %v", p)
		}
	}
	if hand, ok := state["yourHand"].([]interface{}); !ok || len(hand) != BZStartHand {
		t.Fatalf("본인 스냅샷에 yourHand 부재: %v", state["yourHand"])
	}

	for _, c := range conns[1:] {
		bzDrainConn(c)
	}

	sawAfk := false
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		msg := host.waitMatch(t, "event-or-over", func(m BZMessage) bool {
			return m.Type == BZMsgEvent || m.Type == BZMsgGameOver
		})
		if msg.Type == BZMsgEvent {
			ev := bzPayloadMap(t, msg)
			if ev["kind"] == "afk" {
				if !strings.Contains(ev["message"].(string), "자동") {
					t.Fatalf("afk 문구 = %v", ev["message"])
				}
				sawAfk = true
			}
			continue
		}
		over := bzPayloadMap(t, msg)
		if !sawAfk {
			t.Fatal("afk 자동 진행 이벤트가 한 번도 없었다")
		}
		if seats, _ := over["winnerSeats"].([]interface{}); len(seats) == 0 {
			t.Fatalf("종료 payload = %v", over)
		}
		return
	}
	t.Fatal("전원 방치 게임이 120초 안에 끝나지 않았다")
}

// TestBZRoomCodeAndSpectate 방 코드 발급·코드 입장·시작 후 코드 관전 진입.
// 관전자 스냅샷은 yourSeat -1 에 yourHand·yourPending 부재. 행동은 전부 차단.
func TestBZRoomCodeAndSpectate(t *testing.T) {
	_, url, cleanup := newBZTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := bzDial(t, url)
	defer host.conn.Close()
	joined := bzJoin(t, host, "호스트", "NEW")
	code, _ := joined["roomCode"].(string)
	if len(code) != roomCodeLen {
		t.Fatalf("roomCode = %q, want %d자", code, roomCodeLen)
	}

	guests := make([]*bzTestClient, BZMinPlayers-1)
	for i := range guests {
		guests[i] = bzDial(t, url)
		defer guests[i].conn.Close()
		g := bzJoin(t, guests[i], fmt.Sprintf("친구%d", i), code)
		if g["roomCode"] != code || int(g["yourSeat"].(float64)) != i+1 {
			t.Fatalf("코드 입장 실패: %v", g)
		}
	}

	host.send(t, BZMessage{Type: BZMsgStart})
	state := host.bzWaitPhase(t, string(BZPhasePlant))
	if state["roomCode"] != code || len(state["players"].([]interface{})) != BZMinPlayers {
		t.Fatalf("시작 실패: %v", state["players"])
	}
	for _, c := range guests {
		bzDrainConn(c)
	}

	// 시작된 방의 코드로 들어오면 관전자
	spec := bzDial(t, url)
	defer spec.conn.Close()
	spec.send(t, BZMessage{Type: BZMsgJoinGame, Payload: BZJoinGamePayload{Name: "구경꾼", Room: code}})
	specJoined := bzPayloadMap(t, spec.waitFor(t, BZMsgSpectateJoined))
	if specJoined["roomCode"] != code {
		t.Fatalf("관전 입장 roomCode = %v", specJoined["roomCode"])
	}

	specState := bzPayloadMap(t, spec.waitFor(t, BZMsgGameState))
	if int(specState["yourSeat"].(float64)) != -1 {
		t.Fatalf("관전자 yourSeat = %v, want -1", specState["yourSeat"])
	}
	for _, key := range []string{"yourHand", "yourPending"} {
		if leaked, ok := specState[key]; ok {
			t.Fatalf("관전자에게 %s 유출: %v", key, leaked)
		}
	}
	if specState["players"] == nil || specState["flipped"] == nil {
		t.Fatalf("관전자에게 공개 정보가 없다: %v", specState)
	}

	// 관전자는 어떤 행동도 못 한다
	spec.send(t, BZMessage{Type: BZMsgPlant, Payload: BZPlantPayload{Second: false}})
	errPayload := bzPayloadMap(t, spec.waitFor(t, BZMsgError))
	if errPayload["message"] != spectatorDeniedMsg {
		t.Fatalf("관전자 차단 문구 = %v", errPayload["message"])
	}
}

// TestBZReconnect 재접속 3종 — 이탈 통지(bz_player_disconnected) 후 세션으로
// 돌아오면 좌석·손패가 그대로 복원되고(bz_player_reconnected), 모르는 세션은
// bz_session_expired 로 거절된다.
func TestBZReconnect(t *testing.T) {
	_, url, cleanup := newBZTestServer(t, 3*time.Second)
	defer cleanup()

	conns := make([]*bzTestClient, BZMinPlayers)
	sessions := make([]string, BZMinPlayers)
	for i := range conns {
		conns[i] = bzDial(t, url)
		joined := bzJoin(t, conns[i], fmt.Sprintf("P%d", i), "")
		sessions[i], _ = joined["sessionId"].(string)
	}
	defer conns[0].conn.Close()
	conns[0].send(t, BZMessage{Type: BZMsgStart})
	conns[0].bzWaitPhase(t, string(BZPhasePlant))
	for _, c := range conns[2:] {
		bzDrainConn(c)
	}

	// 좌석 1 이탈 → 남은 사람에게 이탈 통지
	conns[1].conn.Close()
	discon := bzPayloadMap(t, conns[0].waitFor(t, BZMsgPlayerDisconnected))
	if int(discon["seat"].(float64)) != 1 || discon["name"] != "P1" {
		t.Fatalf("이탈 통지 = %v", discon)
	}
	if int(discon["graceSeconds"].(float64)) <= 0 {
		t.Fatalf("graceSeconds = %v", discon["graceSeconds"])
	}

	// 세션으로 재접속 → 좌석·손패 복원
	back := bzDial(t, url)
	defer back.conn.Close()
	back.send(t, BZMessage{Type: BZMsgRejoin, Payload: BZRejoinPayload{SessionID: sessions[1]}})
	recon := bzPayloadMap(t, back.waitFor(t, BZMsgPlayerReconnected))
	if int(recon["seat"].(float64)) != 1 {
		t.Fatalf("재접속 통지 = %v", recon)
	}
	restored := bzPayloadMap(t, back.waitFor(t, BZMsgGameState))
	if int(restored["yourSeat"].(float64)) != 1 {
		t.Fatalf("복원 yourSeat = %v", restored["yourSeat"])
	}
	if _, ok := restored["yourHand"].([]interface{}); !ok {
		t.Fatalf("복원 스냅샷에 yourHand 부재: %v", restored)
	}

	// 모르는 세션은 만료 처리
	ghost := bzDial(t, url)
	defer ghost.conn.Close()
	ghost.send(t, BZMessage{Type: BZMsgRejoin, Payload: BZRejoinPayload{SessionID: "없는-세션"}})
	ghost.waitFor(t, BZMsgSessionExpired)
}

// TestBZBotTakeover 유예 만료 좌석을 봇이 이어받아 게임이 멈추지 않는지
func TestBZBotTakeover(t *testing.T) {
	_, url, cleanup := newBZTestServer(t, 120*time.Millisecond)
	defer cleanup()

	conns := make([]*bzTestClient, BZMinPlayers)
	for i := range conns {
		conns[i] = bzDial(t, url)
		defer conns[i].conn.Close()
		bzJoin(t, conns[i], fmt.Sprintf("P%d", i), "")
	}
	conns[0].send(t, BZMessage{Type: BZMsgStart})
	conns[0].bzWaitPhase(t, string(BZPhasePlant))
	for _, c := range conns[2:] {
		bzDrainConn(c)
	}

	// 좌석 1 이탈 → 유예 만료 → 봇 대체
	conns[1].conn.Close()
	sawTakeover := false
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		msg := conns[0].waitMatch(t, "event-or-over", func(m BZMessage) bool {
			return m.Type == BZMsgEvent || m.Type == BZMsgGameOver
		})
		if msg.Type == BZMsgEvent {
			ev := bzPayloadMap(t, msg)
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
		if seats, _ := bzPayloadMap(t, msg)["winnerSeats"].([]interface{}); len(seats) == 0 {
			t.Fatalf("종료 payload = %v", msg.Payload)
		}
		return
	}
	t.Fatal("봇 대체 후 게임이 120초 안에 끝나지 않았다")
}

// TestBZReactAndLobby 리액션은 좌석 보유자만·화이트리스트만, 대기 현황판은
// 사람이 대기할 때만 켜진다
func TestBZReactAndLobby(t *testing.T) {
	_, url, cleanup := newBZTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	conns := make([]*bzTestClient, BZMinPlayers)
	for i := range conns {
		conns[i] = bzDial(t, url)
		defer conns[i].conn.Close()
		bzJoin(t, conns[i], fmt.Sprintf("가나다%d", i), "")
	}
	a, b := conns[0], conns[1]

	a.send(t, BZMessage{Type: BZMsgReact, Payload: BZReactPayload{Emoji: "🔥"}})
	ev := bzPayloadMap(t, b.waitMatch(t, "react", func(m BZMessage) bool {
		if m.Type != BZMsgEvent {
			return false
		}
		p, ok := m.Payload.(map[string]interface{})
		return ok && p["kind"] == "react"
	}))
	if ev["message"] != "🔥" || ev["name"] != "가나다0" || int(ev["seat"].(float64)) != 0 {
		t.Fatalf("리액션 이벤트 = %v", ev)
	}

	// 화이트리스트 밖 이모지는 조용히 무시된다 — 다음에 오는 것은 시작 스냅샷이다
	a.send(t, BZMessage{Type: BZMsgReact, Payload: BZReactPayload{Emoji: "💀"}})
	a.send(t, BZMessage{Type: BZMsgStart})
	state := a.bzWaitPhase(t, string(BZPhasePlant))
	if int(state["hostSeat"].(float64)) != 0 {
		t.Fatalf("hostSeat = %v", state["hostSeat"])
	}
}
