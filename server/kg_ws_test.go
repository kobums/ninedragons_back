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
// (비딩 45초·플레이 45초·정산 5초는 실사용 값)
func init() {
	kgBidTimeout = 150 * time.Millisecond
	kgPlayTimeout = 150 * time.Millisecond
	kgRoundEndDelay = 20 * time.Millisecond

	kgBotBidDelay = 2 * time.Millisecond
	kgBotBidJitterMs = 3
	kgBotPlayDelay = 2 * time.Millisecond
	kgBotPlayJitterMs = 3
}

// kgTestClient 공용 testConn 에 스컬킹 메시지 타입의 waitFor 를 얹은 래퍼
type kgTestClient struct {
	testConn[KGMessage]
}

func newKGTestServer(t *testing.T, grace time.Duration) (*KGHub, string, func()) {
	t.Helper()
	hub := NewKGHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeKGWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func kgDial(t *testing.T, url string) *kgTestClient {
	t.Helper()
	return &kgTestClient{dialWS[KGMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *kgTestClient) waitFor(t *testing.T, msgType KGMessageType) KGMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m KGMessage) bool { return m.Type == msgType })
}

func kgPayloadMap(t *testing.T, msg KGMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// kgJoin 입장하고 kg_player_joined payload 를 돌려준다
func kgJoin(t *testing.T, c *kgTestClient, name, room string) map[string]interface{} {
	t.Helper()
	c.send(t, KGMessage{Type: KGMsgJoinGame, Payload: KGJoinGamePayload{Name: name, Room: room}})
	return kgPayloadMap(t, c.waitFor(t, KGMsgPlayerJoined))
}

// kgWaitPhase 지정한 phase 의 스냅샷이 올 때까지 소비
func (c *kgTestClient) kgWaitPhase(t *testing.T, phase string) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "kg_game_state("+phase+")", func(m KGMessage) bool {
		if m.Type != KGMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		return ok && state["phase"] == phase
	})
	return kgPayloadMap(t, msg)
}

// kgDrain 배경으로 소켓을 비운다 (읽지 않는 연결의 버퍼 포화 방지)
func kgDrain(c *kgTestClient) {
	conn := c.conn
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

// TestKGFiveBotsCompleteGame 봇을 채운 5인 게임이 60초 안에 완주하는지 —
// 가장 중요한 회귀 장치 (비딩 교착·트릭 진행 불가·정산 실패 감지).
// 5인은 10라운드(총 55트릭)라 반드시 끝나야 한다. 좌석 0은 서버 연습봇
// 두뇌(kgBrain)를 WS 로 감싼 드라이버가 잡는다.
func TestKGFiveBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newKGTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := kgDial(t, url)
	defer c.conn.Close()
	kgJoin(t, c, "선장", "")
	c.send(t, KGMessage{Type: KGMsgFillBots}) // 5인까지 채우고 즉시 시작

	start := time.Now()
	brain := newKGBrain()
	deadline := start.Add(60 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "state-or-over", func(m KGMessage) bool {
			return m.Type == KGMsgGameState || m.Type == KGMsgGameOver
		})
		if msg.Type == KGMsgGameOver {
			over := kgPayloadMap(t, msg)
			winners, _ := over["winners"].([]interface{})
			if len(winners) == 0 {
				t.Fatalf("우승자가 없다: %v", over)
			}
			if names, _ := over["winnerNames"].(string); names == "" {
				t.Fatalf("우승자 이름 부재: %v", over)
			}
			if m, _ := over["message"].(string); m == "" {
				t.Fatalf("종료 문구 부재: %v", over)
			}
			maxRound := int(over["maxRound"].(float64))
			if maxRound != kgMaxRound(KGFillBotTarget) {
				t.Fatalf("maxRound = %d, want %d", maxRound, kgMaxRound(KGFillBotTarget))
			}
			if round := int(over["round"].(float64)); round != maxRound {
				t.Fatalf("종료 라운드 = %d, want %d", round, maxRound)
			}
			players := over["players"].([]interface{})
			if len(players) != KGFillBotTarget {
				t.Fatalf("players 길이 = %d, want %d", len(players), KGFillBotTarget)
			}
			best := 0
			for i, pRaw := range players {
				p := pRaw.(map[string]interface{})
				score := int(p["score"].(float64))
				if i == 0 || score > best {
					best = score
				}
				if int(p["handCount"].(float64)) != 0 {
					t.Fatalf("종료 후 손패가 남았다: %v", p)
				}
				if int(p["bid"].(float64)) < 0 {
					t.Fatalf("종료 후 비드가 비공개다: %v", p)
				}
			}
			for _, wRaw := range winners {
				seat := int(wRaw.(float64))
				p := players[seat].(map[string]interface{})
				if int(p["score"].(float64)) != best {
					t.Fatalf("우승 좌석 %d 점수가 최고점이 아니다: %v", seat, p)
				}
			}
			t.Logf("완주: winners=%v 최고점=%d (%.1fs)", winners, best, time.Since(start).Seconds())
			return
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("60초 안에 게임이 끝나지 않았다 — 진행 불가 상태")
}

// TestKGHiddenState 은닉의 핵심 계약 — 허브 고루틴 없이 핸들러를 직접 불러
// 결정적으로 검증한다. yourHand·yourBid 는 본인 스냅샷에만 실리고, 타인·
// 관전자의 raw JSON 에는 키 자체가 없어야 한다. players[].bid 는 비딩이
// 끝나기 전까지 전원 -1 이다.
func TestKGHiddenState(t *testing.T) {
	h := NewKGHub()
	room := h.lobbyRoomFor("")
	clients := make([]*KGClient, 5)
	for i := range clients {
		c := &KGClient{wsClient: newBotWSClient(), Hub: h}
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
	// 결정적 구도: 3라운드 비딩 단계로 되돌리고 손패를 직접 깐다
	game.Round = 3
	game.Phase = KGPhaseBidding
	game.BidsRevealed = false
	game.TrickNo = 0
	game.Trick = []KGTrickPlay{}
	game.LeadSuit = KGSuitNone
	game.LastTrick = nil
	game.RoundResult = nil
	for i, p := range game.Players {
		p.Hand = []KGCard{kgNum(KGSuitGreen, i+2), kgNum(KGSuitYellow, i+2), kgPirate}
		p.Bid = -1
		p.Tricks = 0
		p.Bonus = 0
	}

	rawOf := func(viewer int) string {
		data, err := json.Marshal(h.buildKGState(room, viewer))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(data)
	}

	// ---- yourHand·yourBid: 본인만 (관전자는 키 자체 부재) ----
	raw0 := rawOf(0)
	if !strings.Contains(raw0, `"yourHand":[{"kind":"number","suit":"green","rank":2}`) {
		t.Fatalf("본인 손패가 스냅샷에 없다:\n%s", raw0)
	}
	if !strings.Contains(raw0, `"yourBid":-1`) {
		t.Fatalf("미제출 비드가 -1 로 실리지 않았다:\n%s", raw0)
	}
	if strings.Count(raw0, `"yourHand"`) != 1 || strings.Count(raw0, `"yourBid"`) != 1 {
		t.Fatalf("본인 것 외의 손패·비드 키가 있다:\n%s", raw0)
	}
	rawSpec := rawOf(-1)
	if strings.Contains(rawSpec, `"yourHand"`) {
		t.Fatalf("관전자 스냅샷에 yourHand 키 유출:\n%s", rawSpec)
	}
	if strings.Contains(rawSpec, `"yourBid"`) {
		t.Fatalf("관전자 스냅샷에 yourBid 키 유출:\n%s", rawSpec)
	}
	// 카드 내용은 관전자에게 한 장도 가면 안 된다
	if strings.Contains(rawSpec, `"kind"`) {
		t.Fatalf("관전자 스냅샷에 카드 유출:\n%s", rawSpec)
	}
	// 빈 슬라이스는 [] (nil → null 금지), 아직 없는 결과는 null
	if !strings.Contains(rawSpec, `"trick":[]`) ||
		!strings.Contains(rawSpec, `"lastTrick":null`) ||
		!strings.Contains(rawSpec, `"roundResult":null`) {
		t.Fatalf("관전자 raw 스냅샷 이상:\n%s", rawSpec)
	}
	// 손패를 전부 소진해도 [] 로 나간다
	game.Players[4].Hand = []KGCard{}
	if !strings.Contains(rawOf(4), `"yourHand":[]`) {
		t.Fatalf("빈 손패가 []가 아니다:\n%s", rawOf(4))
	}
	game.Players[4].Hand = []KGCard{kgNum(KGSuitGreen, 6), kgNum(KGSuitYellow, 6), kgPirate}

	// ---- 비딩 진행 중 players[].bid 는 전원 -1 ----
	h.handleGameMessage(KGGameMessage{Client: clients[0], Message: KGMessage{
		Type: KGMsgBid, Payload: KGBidPayload{Bid: 3}}})
	if game.Players[0].Bid != 3 {
		t.Fatalf("비드가 반영되지 않았다: %d", game.Players[0].Bid)
	}
	for _, viewer := range []int{0, 1, 2, -1} {
		state := h.buildKGState(room, viewer)
		for _, pv := range state.Players {
			if pv.Bid != -1 {
				t.Fatalf("viewer %d 에게 seat%d 비드 유출: %d", viewer, pv.Seat, pv.Bid)
			}
		}
		if !state.Players[0].BidSubmitted {
			t.Fatalf("viewer %d 에게 제출 여부가 안 보인다", viewer)
		}
		if state.Players[1].BidSubmitted {
			t.Fatalf("viewer %d 에게 미제출이 제출로 보인다", viewer)
		}
	}
	// 본인에게는 자기 비드가 보인다
	if !strings.Contains(rawOf(0), `"yourBid":3`) {
		t.Fatalf("본인 비드가 안 실렸다:\n%s", rawOf(0))
	}
	if strings.Contains(rawOf(1), `"yourBid":3`) {
		t.Fatalf("남의 비드가 타인 스냅샷에 실렸다:\n%s", rawOf(1))
	}

	// ---- 전원 제출 → 일괄 공개 ----
	for i := 1; i < 5; i++ {
		h.handleGameMessage(KGGameMessage{Client: clients[i], Message: KGMessage{
			Type: KGMsgBid, Payload: KGBidPayload{Bid: i % 2}}})
	}
	if !game.BidsRevealed || game.Phase != KGPhasePlaying {
		t.Fatalf("전원 제출 후 revealed=%v phase=%s", game.BidsRevealed, game.Phase)
	}
	for _, viewer := range []int{0, 2, -1} {
		if bid := h.buildKGState(room, viewer).Players[0].Bid; bid != 3 {
			t.Fatalf("공개 후 viewer %d 가 본 seat0 비드 = %d, want 3", viewer, bid)
		}
	}

	// ---- 트릭 한 판: 해적 리드가 숫자를 전부 이긴다 (공개 정보 확인) ----
	game.CurrentSeat = 0
	game.LeadSeat = 0
	h.handleGameMessage(KGGameMessage{Client: clients[0], Message: KGMessage{
		Type: KGMsgPlay, Payload: KGPlayPayload{Index: 2}}}) // 해적
	if game.LeadSuit != KGSuitNone {
		t.Fatalf("해적 리드가 무늬를 정했다: %q", game.LeadSuit)
	}
	if !strings.Contains(rawOf(-1), `"trick":[{"seat":0,"card":{"kind":"pirate"`) {
		t.Fatalf("진행 중 트릭이 공개 정보가 아니다:\n%s", rawOf(-1))
	}
	// 차례가 아닌 좌석은 거부된다
	h.handleGameMessage(KGGameMessage{Client: clients[3], Message: KGMessage{
		Type: KGMsgPlay, Payload: KGPlayPayload{Index: 0}}})
	if len(game.Trick) != 1 {
		t.Fatalf("차례 아닌 좌석의 플레이가 통과했다: %v", game.Trick)
	}
	for i := 1; i < 5; i++ {
		h.handleGameMessage(KGGameMessage{Client: clients[i], Message: KGMessage{
			Type: KGMsgPlay, Payload: KGPlayPayload{Index: 0}}})
	}
	if game.Players[0].Tricks != 1 {
		t.Fatalf("해적이 트릭을 못 가져갔다: %+v", game.Players[0])
	}
	if !strings.Contains(rawOf(-1), `"lastTrick":{"winnerSeat":0`) {
		t.Fatalf("lastTrick 이 공개 정보가 아니다:\n%s", rawOf(-1))
	}
	if strings.Contains(rawOf(-1), `"yourHand"`) {
		t.Fatalf("플레이 중 관전자에게 손패 유출:\n%s", rawOf(-1))
	}

	// ---- 빈 대기실 스냅샷도 빈 배열 [] (관전자 시점 패닉 금지) ----
	empty := h.lobbyRoomFor("ZZZZ")
	rawEmpty, _ := json.Marshal(h.buildKGState(empty, -1))
	if !strings.Contains(string(rawEmpty), `"players":[]`) ||
		!strings.Contains(string(rawEmpty), `"trick":[]`) {
		t.Fatalf("빈 슬라이스가 []가 아니다:\n%s", rawEmpty)
	}
	if spec := h.buildKGState(empty, -1); spec.YourSeat != -1 || spec.HostSeat != -1 {
		t.Fatalf("빈 방 관전 스냅샷: yourSeat=%d hostSeat=%d", spec.YourSeat, spec.HostSeat)
	}
}

// TestKGAfkAutoProgress 접속만 유지한 채 아무도 움직이지 않는 4인전 —
// 비딩 마감은 0 자동 제출로, 플레이 마감은 최약 카드 자동 제출로 풀린다.
// 자동 진행만으로 2라운드까지 넘어가면 교착이 없다는 뜻이다.
func TestKGAfkAutoProgress(t *testing.T) {
	_, url, cleanup := newKGTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	conns := make([]*kgTestClient, 4)
	for i := range conns {
		conns[i] = kgDial(t, url)
		defer conns[i].conn.Close()
		kgJoin(t, conns[i], fmt.Sprintf("잠수%d", i), "")
	}
	host := conns[0]
	host.send(t, KGMessage{Type: KGMsgStart})

	state := host.kgWaitPhase(t, string(KGPhaseBidding))
	if ends := int64(state["endsAt"].(float64)); ends <= 0 {
		t.Fatalf("비딩 스냅샷의 endsAt = %d, want unixMillis", ends)
	}
	if int(state["round"].(float64)) != 1 || int(state["maxRound"].(float64)) != 10 {
		t.Fatalf("시작 스냅샷 = round %v / maxRound %v", state["round"], state["maxRound"])
	}
	if hand, ok := state["yourHand"].([]interface{}); !ok || len(hand) != 1 {
		t.Fatalf("1라운드 본인 손패 = %v", state["yourHand"])
	}
	if int(state["yourBid"].(float64)) != -1 {
		t.Fatalf("미제출 yourBid = %v, want -1", state["yourBid"])
	}
	for _, pRaw := range state["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		if int(p["bid"].(float64)) != -1 || p["bidSubmitted"].(bool) {
			t.Fatalf("비딩 시작 시 좌석 상태 이상: %v", p)
		}
	}

	// 나머지는 더 읽지 않는다 — 백그라운드로 비워 버퍼 포화만 막는다
	for _, c := range conns[1:] {
		kgDrain(c)
	}

	sawAutoBid, sawAutoPlay, sawRoundEnd := false, false, false
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		msg := host.waitMatch(t, "event-or-state", func(m KGMessage) bool {
			return m.Type == KGMsgEvent || m.Type == KGMsgGameState
		})
		if msg.Type == KGMsgEvent {
			ev := kgPayloadMap(t, msg)
			text, _ := ev["message"].(string)
			switch ev["kind"] {
			case "afk":
				if !strings.Contains(text, "자동") {
					t.Fatalf("afk 문구 = %q", text)
				}
				if strings.Contains(text, "비드") {
					sawAutoBid = true
				} else {
					sawAutoPlay = true
				}
			case "round_end":
				sawRoundEnd = true
			}
			continue
		}
		st := kgPayloadMap(t, msg)
		if int(st["round"].(float64)) >= 2 && st["phase"] == string(KGPhaseBidding) {
			if !sawAutoBid {
				t.Fatal("비딩 자동 제출 이벤트가 없었다")
			}
			if !sawAutoPlay {
				t.Fatal("플레이 자동 제출 이벤트가 없었다")
			}
			if !sawRoundEnd {
				t.Fatal("라운드 정산 이벤트가 없었다")
			}
			if int(st["yourBid"].(float64)) != -1 {
				t.Fatalf("2라운드 시작 yourBid = %v, want -1", st["yourBid"])
			}
			if hand, ok := st["yourHand"].([]interface{}); !ok || len(hand) != 2 {
				t.Fatalf("2라운드 본인 손패 = %v, want 2장", st["yourHand"])
			}
			return
		}
	}
	t.Fatal("전원 방치 게임이 30초 안에 2라운드에 닿지 못했다")
}

// TestKGRoomCodeAndSpectate 방 코드 발급·코드 입장·시작 후 코드 관전 진입.
// 관전자 스냅샷은 yourSeat -1 에 yourHand·yourBid 부재. 행동은 전부 차단된다.
func TestKGRoomCodeAndSpectate(t *testing.T) {
	_, url, cleanup := newKGTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := kgDial(t, url)
	defer host.conn.Close()
	joined := kgJoin(t, host, "호스트", "NEW")
	code, _ := joined["roomCode"].(string)
	if len(code) != roomCodeLen {
		t.Fatalf("roomCode = %q, want %d자", code, roomCodeLen)
	}

	guests := make([]*kgTestClient, 2)
	for i := range guests {
		guests[i] = kgDial(t, url)
		defer guests[i].conn.Close()
		g := kgJoin(t, guests[i], fmt.Sprintf("친구%d", i), code)
		if g["roomCode"] != code || int(g["yourSeat"].(float64)) != i+1 {
			t.Fatalf("코드 입장 실패: %v", g)
		}
	}

	host.send(t, KGMessage{Type: KGMsgStart})
	state := host.kgWaitPhase(t, string(KGPhaseBidding))
	if state["roomCode"] != code || len(state["players"].([]interface{})) != 3 {
		t.Fatalf("시작 실패: %v", state["players"])
	}
	if _, ok := state["yourHand"].([]interface{}); !ok {
		t.Fatalf("본인 손패 부재: %v", state)
	}
	for _, c := range guests {
		kgDrain(c)
	}

	// 시작된 방의 코드로 들어오면 관전자
	spec := kgDial(t, url)
	defer spec.conn.Close()
	spec.send(t, KGMessage{Type: KGMsgJoinGame, Payload: KGJoinGamePayload{Name: "구경꾼", Room: code}})
	specJoined := kgPayloadMap(t, spec.waitFor(t, KGMsgSpectateJoined))
	if specJoined["roomCode"] != code {
		t.Fatalf("관전 입장 roomCode = %v", specJoined["roomCode"])
	}

	specState := kgPayloadMap(t, spec.waitFor(t, KGMsgGameState))
	if int(specState["yourSeat"].(float64)) != -1 {
		t.Fatalf("관전자 yourSeat = %v, want -1", specState["yourSeat"])
	}
	if leaked, ok := specState["yourHand"]; ok {
		t.Fatalf("관전자에게 손패 유출: %v", leaked)
	}
	if leaked, ok := specState["yourBid"]; ok {
		t.Fatalf("관전자에게 비드 유출: %v", leaked)
	}
	if _, ok := specState["trick"].([]interface{}); !ok {
		t.Fatalf("관전자 trick 이 배열이 아니다: %v", specState["trick"])
	}
	for _, pRaw := range specState["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		if int(p["handCount"].(float64)) < 0 {
			t.Fatalf("관전자 handCount = %v", p["handCount"])
		}
		if _, ok := p["score"].(float64); !ok {
			t.Fatalf("관전자에게 점수가 안 보인다: %v", p)
		}
	}

	// 관전자는 어떤 행동도 못 한다
	spec.send(t, KGMessage{Type: KGMsgPlay, Payload: KGPlayPayload{Index: 0}})
	errPayload := kgPayloadMap(t, spec.waitFor(t, KGMsgError))
	if errPayload["message"] != spectatorDeniedMsg {
		t.Fatalf("관전자 차단 문구 = %v", errPayload["message"])
	}
}

// TestKGReconnect 재접속 3종 — 이탈 통지(kg_player_disconnected) 후 세션으로
// 돌아오면 좌석·손패·비드가 그대로 복원되고(kg_player_reconnected), 모르는
// 세션은 kg_session_expired 로 거절된다.
func TestKGReconnect(t *testing.T) {
	_, url, cleanup := newKGTestServer(t, 3*time.Second)
	defer cleanup()

	conns := make([]*kgTestClient, 4)
	sessions := make([]string, 4)
	for i := range conns {
		conns[i] = kgDial(t, url)
		joined := kgJoin(t, conns[i], fmt.Sprintf("P%d", i), "")
		sessions[i], _ = joined["sessionId"].(string)
	}
	defer conns[0].conn.Close()
	conns[0].send(t, KGMessage{Type: KGMsgStart})
	before := conns[0].kgWaitPhase(t, string(KGPhaseBidding))
	if _, ok := before["yourHand"].([]interface{}); !ok {
		t.Fatal("시작 스냅샷에 yourHand 부재")
	}
	for _, c := range conns[2:] {
		kgDrain(c)
	}

	// 좌석 1 이탈 → 남은 사람에게 이탈 통지
	conns[1].conn.Close()
	discon := kgPayloadMap(t, conns[0].waitFor(t, KGMsgPlayerDisconnected))
	if int(discon["seat"].(float64)) != 1 || discon["name"] != "P1" {
		t.Fatalf("이탈 통지 = %v", discon)
	}
	if int(discon["graceSeconds"].(float64)) <= 0 {
		t.Fatalf("graceSeconds = %v", discon["graceSeconds"])
	}

	// 세션으로 재접속 → 좌석·손패·비드 복원
	back := kgDial(t, url)
	defer back.conn.Close()
	back.send(t, KGMessage{Type: KGMsgRejoin, Payload: KGRejoinPayload{SessionID: sessions[1]}})
	recon := kgPayloadMap(t, back.waitFor(t, KGMsgPlayerReconnected))
	if int(recon["seat"].(float64)) != 1 {
		t.Fatalf("재접속 통지 = %v", recon)
	}
	restored := kgPayloadMap(t, back.waitFor(t, KGMsgGameState))
	if int(restored["yourSeat"].(float64)) != 1 {
		t.Fatalf("복원 yourSeat = %v", restored["yourSeat"])
	}
	if _, ok := restored["yourHand"].([]interface{}); !ok {
		t.Fatalf("복원 스냅샷에 yourHand 부재: %v", restored)
	}
	if _, ok := restored["yourBid"].(float64); !ok {
		t.Fatalf("복원 스냅샷에 yourBid 부재: %v", restored)
	}

	// 모르는 세션은 만료 처리
	ghost := kgDial(t, url)
	defer ghost.conn.Close()
	ghost.send(t, KGMessage{Type: KGMsgRejoin, Payload: KGRejoinPayload{SessionID: "없는-세션"}})
	ghost.waitFor(t, KGMsgSessionExpired)
}

// TestKGBotTakeover 유예 만료 좌석을 봇이 이어받아 게임이 멈추지 않는지
func TestKGBotTakeover(t *testing.T) {
	_, url, cleanup := newKGTestServer(t, 120*time.Millisecond)
	defer cleanup()

	conns := make([]*kgTestClient, 3)
	for i := range conns {
		conns[i] = kgDial(t, url)
		defer conns[i].conn.Close()
		kgJoin(t, conns[i], fmt.Sprintf("P%d", i), "")
	}
	conns[0].send(t, KGMessage{Type: KGMsgStart})
	conns[0].kgWaitPhase(t, string(KGPhaseBidding))
	for _, c := range conns[2:] {
		kgDrain(c)
	}

	// 좌석 1 이탈 → 유예 만료 → 봇 대체 → 이후에도 트릭이 계속 굴러간다
	conns[1].conn.Close()
	sawTakeover := false
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		msg := conns[0].waitMatch(t, "event", func(m KGMessage) bool {
			return m.Type == KGMsgEvent
		})
		ev := kgPayloadMap(t, msg)
		switch ev["kind"] {
		case "bot_takeover":
			if int(ev["seat"].(float64)) != 1 {
				t.Fatalf("봇 대체 좌석 = %v, want 1", ev["seat"])
			}
			if ev["name"] == nil || ev["name"] == "" {
				t.Fatalf("봇 대체 이벤트에 name 부재: %v", ev)
			}
			sawTakeover = true
		case "round_end":
			if !sawTakeover {
				t.Fatal("봇 대체 없이 라운드가 끝났다")
			}
			bot := false
			state := conns[0].kgWaitPhase(t, string(KGPhaseBidding))
			for _, pRaw := range state["players"].([]interface{}) {
				p := pRaw.(map[string]interface{})
				if int(p["seat"].(float64)) == 1 && p["bot"].(bool) {
					bot = true
				}
			}
			if !bot {
				t.Fatalf("좌석 1이 봇으로 표시되지 않았다: %v", state["players"])
			}
			return
		}
	}
	t.Fatal("봇 대체 후 30초 안에 라운드가 넘어가지 않았다")
}

// TestKGReact 리액션은 화이트리스트 이모지만, 좌석당 레이트리밋을 지나야
// 방송된다. 걸러진 리액션은 조용히 사라진다 (에러 없음).
func TestKGReact(t *testing.T) {
	_, url, cleanup := newKGTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	a := kgDial(t, url)
	defer a.conn.Close()
	kgJoin(t, a, "가", "")
	b := kgDial(t, url)
	defer b.conn.Close()
	kgJoin(t, b, "나", "")
	kgDrain(b)

	waitReact := func(t *testing.T) map[string]interface{} {
		t.Helper()
		msg := a.waitMatch(t, "kg_event(react)", func(m KGMessage) bool {
			if m.Type != KGMsgEvent {
				return false
			}
			p, ok := m.Payload.(map[string]interface{})
			return ok && p["kind"] == "react"
		})
		return kgPayloadMap(t, msg)
	}

	a.send(t, KGMessage{Type: KGMsgReact, Payload: KGReactPayload{Emoji: "👍"}})
	ev := waitReact(t)
	if int(ev["seat"].(float64)) != 0 || ev["message"] != "👍" || ev["name"] != "가" {
		t.Fatalf("첫 리액션 = %v", ev)
	}

	// 화이트리스트 밖 이모지와 레이트리밋에 걸린 재발신은 조용히 무시된다.
	// 다음으로 도착하는 react 는 좌석 1의 것이어야 한다.
	a.send(t, KGMessage{Type: KGMsgReact, Payload: KGReactPayload{Emoji: "🍕"}})
	a.send(t, KGMessage{Type: KGMsgReact, Payload: KGReactPayload{Emoji: "🔥"}})
	b.send(t, KGMessage{Type: KGMsgReact, Payload: KGReactPayload{Emoji: "😂"}})

	ev = waitReact(t)
	if int(ev["seat"].(float64)) != 1 || ev["message"] != "😂" {
		t.Fatalf("걸러져야 할 리액션이 통과했다: %v", ev)
	}
}
