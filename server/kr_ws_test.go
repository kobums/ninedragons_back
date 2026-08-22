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
// (지목 45초·라운드 정산 5초는 실사용 값)
func init() {
	krPointTimeout = 400 * time.Millisecond
	krRoundEndDelay = 40 * time.Millisecond

	krBotClaimDelay = 3 * time.Millisecond
	krBotClaimJitterMs = 5
	krBotPointDelay = 3 * time.Millisecond
	krBotPointJitterMs = 5
}

// krTestClient 공용 testConn 에 크라켄 메시지 타입의 waitFor 를 얹은 래퍼
type krTestClient struct {
	testConn[KRMessage]
}

func newKRTestServer(t *testing.T, grace time.Duration) (*KRHub, string, func()) {
	t.Helper()
	hub := NewKRHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeKRWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func krDial(t *testing.T, url string) *krTestClient {
	t.Helper()
	return &krTestClient{dialWS[KRMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *krTestClient) waitFor(t *testing.T, msgType KRMessageType) KRMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m KRMessage) bool { return m.Type == msgType })
}

func krPayloadMap(t *testing.T, msg KRMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// krJoin 입장하고 kr_player_joined payload 를 돌려준다
func krJoin(t *testing.T, c *krTestClient, name, room string) map[string]interface{} {
	t.Helper()
	c.send(t, KRMessage{Type: KRMsgJoinGame, Payload: KRJoinGamePayload{Name: name, Room: room}})
	return krPayloadMap(t, c.waitFor(t, KRMsgPlayerJoined))
}

// krWaitPhase 지정한 phase 의 스냅샷이 올 때까지 소비
func (c *krTestClient) krWaitPhase(t *testing.T, phase string) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "kr_game_state("+phase+")", func(m KRMessage) bool {
		if m.Type != KRMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		return ok && state["phase"] == phase
	})
	return krPayloadMap(t, msg)
}

// krDrain 배경으로 소켓을 비운다 (읽지 않는 연결의 버퍼 포화 방지)
func krDrain(c *krTestClient) {
	conn := c.conn
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

// TestKRFiveBotsCompleteGame 봇을 채운 5인 게임이 30초 안에 완주하는지 —
// 가장 중요한 회귀 장치 (지목권 교착·라운드 재배분 실패·종료 판정 감지).
// 4라운드가 유한하므로 반드시 끝나야 한다. 좌석 0은 서버 연습봇 두뇌(krBrain)를
// WS 로 감싼 드라이버가 잡는다.
func TestKRFiveBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newKRTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := krDial(t, url)
	defer c.conn.Close()
	krJoin(t, c, "감독", "")
	c.send(t, KRMessage{Type: KRMsgFillBots}) // 5인까지 채우고 즉시 시작

	start := time.Now()
	brain := newKRBrain()
	deadline := start.Add(30 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "state-or-over", func(m KRMessage) bool {
			return m.Type == KRMsgGameState || m.Type == KRMsgGameOver
		})
		if msg.Type == KRMsgGameOver {
			over := krPayloadMap(t, msg)
			winner, _ := over["winner"].(string)
			if winner != "expedition" && winner != "skeleton" {
				t.Fatalf("winner = %q (무승부는 없다)", winner)
			}
			reason, _ := over["reason"].(string)
			if reason != "kraken" && reason != "all_treasure" && reason != "timeout" {
				t.Fatalf("reason = %q", reason)
			}
			if m, _ := over["message"].(string); m == "" {
				t.Fatalf("종료 문구 부재: %v", over)
			}
			round := int(over["round"].(float64))
			if round < 1 || round > KRRounds {
				t.Fatalf("round = %d", round)
			}
			found := over["found"].(map[string]interface{})
			if int(found["treasureTotal"].(float64)) != KRFillBotTarget {
				t.Fatalf("treasureTotal = %v, want %d", found["treasureTotal"], KRFillBotTarget)
			}
			switch reason {
			case "kraken":
				if int(found["kraken"].(float64)) != 1 {
					t.Fatalf("크라켄 종료인데 found.kraken = %v", found["kraken"])
				}
			case "all_treasure":
				if int(found["treasure"].(float64)) != KRFillBotTarget {
					t.Fatalf("보물 종료인데 found.treasure = %v", found["treasure"])
				}
			case "timeout":
				if round != KRRounds {
					t.Fatalf("시간 초과 종료인데 round = %d", round)
				}
			}
			players := over["players"].([]interface{})
			if len(players) != KRFillBotTarget {
				t.Fatalf("players 길이 = %d, want %d", len(players), KRFillBotTarget)
			}
			roles := map[string]int{}
			for _, pRaw := range players {
				p := pRaw.(map[string]interface{})
				role, _ := p["role"].(string)
				if role != "expedition" && role != "skeleton" {
					t.Fatalf("종료 화면에 역할 미공개: %v", p)
				}
				roles[role]++
			}
			pool := krRolePoolFor(KRFillBotTarget)
			if roles["expedition"] > pool.Expedition || roles["skeleton"] > pool.Skeleton {
				t.Fatalf("종료 역할 분포가 풀을 넘었다: %v vs %+v", roles, pool)
			}
			t.Logf("완주: winner=%s reason=%s round=%d (%.1fs)",
				winner, reason, round, time.Since(start).Seconds())
			return
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("30초 안에 게임이 끝나지 않았다 — 진행 불가 상태")
}

// TestKRHiddenState 은닉의 핵심 계약 — 허브 고루틴 없이 핸들러를 직접 불러
// 결정적으로 검증한다. yourRole·yourHand 는 본인 스냅샷에만 실리고, 타인·
// 관전자의 raw JSON 에는 키 자체가 없어야 한다. players[].role 은 game_over
// 전까지 전원 "" 이고, rolePool 은 전원 공개다.
func TestKRHiddenState(t *testing.T) {
	h := NewKRHub()
	room := h.lobbyRoomFor("")
	clients := make([]*KRClient, 5)
	for i := range clients {
		c := &KRClient{wsClient: newBotWSClient(), Hub: h}
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
	// 결정적 구도: seat1 만 해골, 나머지 탐험대. 손패는 전부 꽝으로 깔고
	// seat3 의 0번 카드만 크라켄으로 둔다.
	for _, p := range game.Players {
		p.Role = KRRoleExpedition
	}
	game.Players[1].Role = KRRoleSkeleton
	krFillHands(game, KRCardBlank)
	game.Players[3].Hand[0] = KRCardKraken
	game.FoundTreasure, game.FoundKraken = 0, 0
	game.CurrentSeat = 0

	rawOf := func(viewer int) string {
		data, err := json.Marshal(h.buildKRState(room, viewer))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(data)
	}

	// ---- yourRole: 본인만 (관전자는 키 자체 부재) ----
	if !strings.Contains(rawOf(1), `"yourRole":"skeleton"`) {
		t.Fatalf("해골 본인에게 역할이 없다:\n%s", rawOf(1))
	}
	if !strings.Contains(rawOf(0), `"yourRole":"expedition"`) {
		t.Fatalf("탐험대 본인에게 역할이 없다:\n%s", rawOf(0))
	}
	rawSpec := rawOf(-1)
	if strings.Contains(rawSpec, `"yourRole"`) {
		t.Fatalf("관전자 스냅샷에 yourRole 키 유출:\n%s", rawSpec)
	}

	// ---- yourHand: 본인만 (관전자는 키 자체 부재), 빈 손패도 [] ----
	if !strings.Contains(rawOf(3), `"yourHand":["kraken"`) {
		t.Fatalf("본인 손패가 스냅샷에 없다:\n%s", rawOf(3))
	}
	if strings.Contains(rawSpec, `"yourHand"`) {
		t.Fatalf("관전자 스냅샷에 yourHand 키 유출:\n%s", rawSpec)
	}
	// "kraken" 이 카드 값으로 등장하면 유출 — 키 표기("kraken":)만 허용한다
	// (found.kraken·rolePool 은 공개 정보라 키로는 늘 등장한다)
	leaksKrakenCard := func(raw string) bool {
		return strings.Count(raw, `"kraken"`) != strings.Count(raw, `"kraken":`)
	}
	if leaksKrakenCard(rawSpec) {
		t.Fatalf("관전자 스냅샷에 크라켄 카드 유출:\n%s", rawSpec)
	}
	// 남의 크라켄은 타인 스냅샷에도 보이면 안 된다 (seat0 은 꽝만 들었다)
	raw0 := rawOf(0)
	if leaksKrakenCard(raw0) {
		t.Fatalf("타인 스냅샷에 남의 크라켄 유출:\n%s", raw0)
	}
	if strings.Count(raw0, `"yourHand"`) != 1 {
		t.Fatalf("yourHand 키가 본인 것 하나가 아니다:\n%s", raw0)
	}
	// 손패를 전부 소진한 좌석은 [] 로 나간다 (nil → null 금지)
	game.Players[4].Hand = []KRCard{}
	if !strings.Contains(rawOf(4), `"yourHand":[]`) {
		t.Fatalf("빈 손패가 []가 아니다:\n%s", rawOf(4))
	}

	// ---- 진행 중 players[].role 은 전원 "" ----
	for _, viewer := range []int{0, 1, 2, -1} {
		for _, pv := range h.buildKRState(room, viewer).Players {
			if pv.Role != "" {
				t.Fatalf("viewer %d 에게 seat%d 역할 유출: %q", viewer, pv.Seat, pv.Role)
			}
		}
	}

	// ---- 공개 정보와 관전자 스냅샷 (패닉 없이 빌드된다) ----
	spec := h.buildKRState(room, -1)
	if spec.YourSeat != -1 || spec.Round != 1 || spec.RevealsLeft != 5 || spec.CurrentSeat != 0 {
		t.Fatalf("관전자 스냅샷: yourSeat=%d round=%d revealsLeft=%d currentSeat=%d",
			spec.YourSeat, spec.Round, spec.RevealsLeft, spec.CurrentSeat)
	}
	if spec.RolePool != krRolePoolFor(5) || spec.Found.TreasureTotal != 5 {
		t.Fatalf("공개 정보 이상: pool=%+v found=%+v", spec.RolePool, spec.Found)
	}
	if spec.Result != nil || spec.LastReveal != nil {
		t.Fatalf("진행 중 result/lastReveal = %+v %+v", spec.Result, spec.LastReveal)
	}
	if !strings.Contains(rawSpec, `"result":null`) ||
		!strings.Contains(rawSpec, `"lastReveal":null`) ||
		!strings.Contains(rawSpec, `"revealedThisRound":[]`) ||
		!strings.Contains(rawSpec, `"claim":null`) {
		t.Fatalf("관전자 raw 스냅샷 이상:\n%s", rawSpec)
	}

	// ---- 빈 대기실 스냅샷도 빈 배열 [] (null 금지) ----
	empty := h.lobbyRoomFor("ZZZZ")
	if raw, _ := json.Marshal(h.buildKRState(empty, -1)); !strings.Contains(string(raw), `"players":[]`) {
		t.Fatalf("빈 슬라이스가 []가 아니다:\n%s", raw)
	}

	// ---- 지목: 차례가 아닌 좌석은 거부, 공개 결과는 전원 공개 ----
	h.handleGameMessage(KRGameMessage{Client: clients[2], Message: KRMessage{
		Type: KRMsgPoint, Payload: KRPointPayload{TargetSeat: 0, CardIndex: 0}}})
	if game.CurrentSeat != 0 || game.RevealsLeft != 5 {
		t.Fatalf("차례 아닌 좌석의 지목이 통과했다: current=%d left=%d",
			game.CurrentSeat, game.RevealsLeft)
	}
	h.handleGameMessage(KRGameMessage{Client: clients[0], Message: KRMessage{
		Type: KRMsgPoint, Payload: KRPointPayload{TargetSeat: 1, CardIndex: 0}}})
	if game.RevealsLeft != 4 || game.CurrentSeat != 1 {
		t.Fatalf("지목 반영 실패: left=%d current=%d", game.RevealsLeft, game.CurrentSeat)
	}
	if !strings.Contains(rawOf(-1), `"bySeat":0`) {
		t.Fatalf("lastReveal 이 공개 정보가 아니다:\n%s", rawOf(-1))
	}

	// ---- 선언은 공개 정보 (거짓 가능) ----
	h.handleGameMessage(KRGameMessage{Client: clients[1], Message: KRMessage{
		Type: KRMsgClaim, Payload: KRClaimPayload{Treasure: 2, Kraken: 1}}})
	if !strings.Contains(rawOf(-1), `"claim":{"treasure":2,"kraken":1}`) {
		t.Fatalf("선언이 관전자에게 안 보인다:\n%s", rawOf(-1))
	}

	// ---- 크라켄 공개 → 즉시 해골 승, 전원 역할 공개 ----
	game.CurrentSeat = 2
	h.handleGameMessage(KRGameMessage{Client: clients[2], Message: KRMessage{
		Type: KRMsgPoint, Payload: KRPointPayload{TargetSeat: 3, CardIndex: 0}}})
	if game.Phase != KRPhaseGameOver {
		t.Fatalf("크라켄 공개 후 phase = %s", game.Phase)
	}
	if game.Result == nil || game.Result.Winner != "skeleton" || game.Result.Reason != "kraken" {
		t.Fatalf("result = %+v", game.Result)
	}
	// 종료 스냅샷은 finishGame 이 방을 정리하기 전 값으로 확인한다
	overSpec := h.buildKRState(room, -1)
	roles := map[string]int{}
	for _, pv := range overSpec.Players {
		roles[pv.Role]++
	}
	if roles["skeleton"] != 1 || roles["expedition"] != 4 {
		t.Fatalf("종료 후 역할 공개 이상: %v", roles)
	}
	if overSpec.Result == nil || overSpec.Result.Reason != "kraken" {
		t.Fatalf("종료 result = %+v", overSpec.Result)
	}
	// 종료 뒤에도 남의 손패는 스냅샷에 실리지 않는다 (본인 것만)
	if strings.Count(rawOf(0), `"yourHand"`) != 1 || strings.Contains(rawOf(-1), `"yourHand"`) {
		t.Fatal("종료 스냅샷에서 손패 은닉이 깨졌다")
	}
}

// TestKRAfkAutoProgress 접속만 유지한 채 아무도 지목하지 않는 4인전 —
// 자동 지목과 라운드 자동 재배분만으로 게임이 끝까지 완주하는지
// (endsAt 노출·afk 이벤트 포함). 4라운드가 유한하므로 반드시 끝난다.
func TestKRAfkAutoProgress(t *testing.T) {
	_, url, cleanup := newKRTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	conns := make([]*krTestClient, KRMinPlayers)
	for i := range conns {
		conns[i] = krDial(t, url)
		defer conns[i].conn.Close()
		krJoin(t, conns[i], fmt.Sprintf("잠수%d", i), "")
	}
	host := conns[0]
	host.send(t, KRMessage{Type: KRMsgStart})

	state := host.krWaitPhase(t, string(KRPhaseRevealing))
	if ends := int64(state["endsAt"].(float64)); ends <= 0 {
		t.Fatalf("지목 스냅샷의 endsAt = %d, want unixMillis", ends)
	}
	if int(state["round"].(float64)) != 1 || int(state["revealsLeft"].(float64)) != KRMinPlayers {
		t.Fatalf("시작 스냅샷 = round %v / revealsLeft %v", state["round"], state["revealsLeft"])
	}
	pool := state["rolePool"].(map[string]interface{})
	if int(pool["expedition"].(float64)) != 3 || int(pool["skeleton"].(float64)) != 2 {
		t.Fatalf("4인 rolePool = %v", pool)
	}
	if _, ok := state["yourHand"].([]interface{}); !ok {
		t.Fatalf("본인 스냅샷에 yourHand 부재: %v", state)
	}

	// 나머지는 더 읽지 않는다 — 백그라운드로 비워 버퍼 포화만 막는다
	for _, c := range conns[1:] {
		krDrain(c)
	}

	sawAfk := false
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		msg := host.waitMatch(t, "event-or-over", func(m KRMessage) bool {
			return m.Type == KRMsgEvent || m.Type == KRMsgGameOver
		})
		if msg.Type == KRMsgEvent {
			ev := krPayloadMap(t, msg)
			if ev["kind"] == "afk" {
				if !strings.Contains(ev["message"].(string), "자동") {
					t.Fatalf("afk 문구 = %v", ev["message"])
				}
				sawAfk = true
			}
			continue
		}
		over := krPayloadMap(t, msg)
		winner, _ := over["winner"].(string)
		if winner != "expedition" && winner != "skeleton" {
			t.Fatalf("winner = %q", winner)
		}
		if !sawAfk {
			t.Fatal("afk 자동 진행 이벤트가 한 번도 없었다")
		}
		return
	}
	t.Fatal("전원 방치 게임이 25초 안에 끝나지 않았다")
}

// TestKRRoomCodeAndSpectate 방 코드 발급·코드 입장·시작 후 코드 관전 진입.
// 관전자 스냅샷은 yourSeat -1 에 yourRole·yourHand 부재. 행동은 전부 차단된다.
func TestKRRoomCodeAndSpectate(t *testing.T) {
	_, url, cleanup := newKRTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := krDial(t, url)
	defer host.conn.Close()
	joined := krJoin(t, host, "호스트", "NEW")
	code, _ := joined["roomCode"].(string)
	if len(code) != roomCodeLen {
		t.Fatalf("roomCode = %q, want %d자", code, roomCodeLen)
	}

	guests := make([]*krTestClient, KRMinPlayers-1)
	for i := range guests {
		guests[i] = krDial(t, url)
		defer guests[i].conn.Close()
		g := krJoin(t, guests[i], fmt.Sprintf("친구%d", i), code)
		if g["roomCode"] != code || int(g["yourSeat"].(float64)) != i+1 {
			t.Fatalf("코드 입장 실패: %v", g)
		}
	}

	host.send(t, KRMessage{Type: KRMsgStart})
	state := host.krWaitPhase(t, string(KRPhaseRevealing))
	if state["roomCode"] != code || len(state["players"].([]interface{})) != KRMinPlayers {
		t.Fatalf("시작 실패: %v", state["players"])
	}
	role, _ := state["yourRole"].(string)
	if role != "expedition" && role != "skeleton" {
		t.Fatalf("yourRole = %q", role)
	}
	hand, ok := state["yourHand"].([]interface{})
	if !ok || len(hand) != krHandSize(1) {
		t.Fatalf("본인 손패 = %v, want %d장", state["yourHand"], krHandSize(1))
	}
	for _, pRaw := range state["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		if p["role"] != "" {
			t.Fatalf("진행 중 좌석 역할 유출: %v", p)
		}
		if _, ok := p["revealedThisRound"].([]interface{}); !ok {
			t.Fatalf("revealedThisRound 가 배열이 아니다: %v", p)
		}
	}
	for _, c := range guests {
		krDrain(c)
	}

	// 시작된 방의 코드로 들어오면 관전자
	spec := krDial(t, url)
	defer spec.conn.Close()
	spec.send(t, KRMessage{Type: KRMsgJoinGame, Payload: KRJoinGamePayload{Name: "구경꾼", Room: code}})
	specJoined := krPayloadMap(t, spec.waitFor(t, KRMsgSpectateJoined))
	if specJoined["roomCode"] != code {
		t.Fatalf("관전 입장 roomCode = %v", specJoined["roomCode"])
	}

	specState := krPayloadMap(t, spec.waitFor(t, KRMsgGameState))
	if int(specState["yourSeat"].(float64)) != -1 {
		t.Fatalf("관전자 yourSeat = %v, want -1", specState["yourSeat"])
	}
	if leaked, ok := specState["yourRole"]; ok {
		t.Fatalf("관전자에게 역할 유출: %v", leaked)
	}
	if leaked, ok := specState["yourHand"]; ok {
		t.Fatalf("관전자에게 손패 유출: %v", leaked)
	}
	specPool := specState["rolePool"].(map[string]interface{})
	if int(specPool["expedition"].(float64)) != 3 {
		t.Fatalf("관전자 rolePool = %v (풀은 공개 정보)", specPool)
	}
	for _, pRaw := range specState["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		if p["role"] != "" {
			t.Fatalf("관전자에게 좌석 역할 유출: %v", p)
		}
		if int(p["handCount"].(float64)) < 0 {
			t.Fatalf("관전자 handCount = %v", p["handCount"])
		}
	}

	// 관전자는 어떤 행동도 못 한다
	spec.send(t, KRMessage{Type: KRMsgPoint, Payload: KRPointPayload{TargetSeat: 0, CardIndex: 0}})
	errPayload := krPayloadMap(t, spec.waitFor(t, KRMsgError))
	if errPayload["message"] != spectatorDeniedMsg {
		t.Fatalf("관전자 차단 문구 = %v", errPayload["message"])
	}
}

// TestKRReconnect 재접속 3종 — 이탈 통지(kr_player_disconnected) 후 세션으로
// 돌아오면 좌석·역할·손패가 그대로 복원되고(kr_player_reconnected), 모르는
// 세션은 kr_session_expired 로 거절된다.
func TestKRReconnect(t *testing.T) {
	_, url, cleanup := newKRTestServer(t, 3*time.Second)
	defer cleanup()

	conns := make([]*krTestClient, KRMinPlayers)
	sessions := make([]string, KRMinPlayers)
	for i := range conns {
		conns[i] = krDial(t, url)
		joined := krJoin(t, conns[i], fmt.Sprintf("P%d", i), "")
		sessions[i], _ = joined["sessionId"].(string)
	}
	defer conns[0].conn.Close()
	conns[0].send(t, KRMessage{Type: KRMsgStart})
	before := conns[0].krWaitPhase(t, string(KRPhaseRevealing))
	if r, _ := before["yourRole"].(string); r == "" {
		t.Fatal("시작 스냅샷에 yourRole 부재")
	}
	for _, c := range conns[2:] {
		krDrain(c)
	}

	// 좌석 1 이탈 → 남은 사람에게 이탈 통지
	conns[1].conn.Close()
	discon := krPayloadMap(t, conns[0].waitFor(t, KRMsgPlayerDisconnected))
	if int(discon["seat"].(float64)) != 1 || discon["name"] != "P1" {
		t.Fatalf("이탈 통지 = %v", discon)
	}
	if int(discon["graceSeconds"].(float64)) <= 0 {
		t.Fatalf("graceSeconds = %v", discon["graceSeconds"])
	}

	// 세션으로 재접속 → 좌석·역할·손패 복원
	back := krDial(t, url)
	defer back.conn.Close()
	back.send(t, KRMessage{Type: KRMsgRejoin, Payload: KRRejoinPayload{SessionID: sessions[1]}})
	recon := krPayloadMap(t, back.waitFor(t, KRMsgPlayerReconnected))
	if int(recon["seat"].(float64)) != 1 {
		t.Fatalf("재접속 통지 = %v", recon)
	}
	restored := krPayloadMap(t, back.waitFor(t, KRMsgGameState))
	if int(restored["yourSeat"].(float64)) != 1 {
		t.Fatalf("복원 yourSeat = %v", restored["yourSeat"])
	}
	if r, _ := restored["yourRole"].(string); r == "" {
		t.Fatal("복원 스냅샷에 yourRole 부재")
	}
	if _, ok := restored["yourHand"].([]interface{}); !ok {
		t.Fatalf("복원 스냅샷에 yourHand 부재: %v", restored)
	}

	// 모르는 세션은 만료 처리
	ghost := krDial(t, url)
	defer ghost.conn.Close()
	ghost.send(t, KRMessage{Type: KRMsgRejoin, Payload: KRRejoinPayload{SessionID: "없는-세션"}})
	ghost.waitFor(t, KRMsgSessionExpired)
}

// TestKRBotTakeover 유예 만료 좌석을 봇이 이어받아 게임이 멈추지 않는지 —
// 지목권자가 이탈해도 봇이 이어받아 완주한다.
func TestKRBotTakeover(t *testing.T) {
	_, url, cleanup := newKRTestServer(t, 120*time.Millisecond)
	defer cleanup()

	conns := make([]*krTestClient, KRMinPlayers)
	for i := range conns {
		conns[i] = krDial(t, url)
		defer conns[i].conn.Close()
		krJoin(t, conns[i], fmt.Sprintf("P%d", i), "")
	}
	conns[0].send(t, KRMessage{Type: KRMsgStart})
	conns[0].krWaitPhase(t, string(KRPhaseRevealing))
	for _, c := range conns[2:] {
		krDrain(c)
	}

	// 좌석 1 이탈 → 유예 만료 → 봇 대체
	conns[1].conn.Close()
	sawTakeover := false
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		msg := conns[0].waitMatch(t, "event-or-over", func(m KRMessage) bool {
			return m.Type == KRMsgEvent || m.Type == KRMsgGameOver
		})
		if msg.Type == KRMsgEvent {
			ev := krPayloadMap(t, msg)
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
		over := krPayloadMap(t, msg)
		if _, ok := over["winner"].(string); !ok {
			t.Fatalf("종료 payload = %v", over)
		}
		return
	}
	t.Fatal("봇 대체 후 게임이 25초 안에 끝나지 않았다")
}

// TestKRClaimWirePayload kr_claim 의 크라켄 필드는 0/1 정수와 true/false
// 불리언 표기를 모두 받아들이고, 출력은 항상 0/1 정수다 (프론트 토글 대응).
func TestKRClaimWirePayload(t *testing.T) {
	cases := []struct {
		raw  string
		want KRClaimPayload
	}{
		{`{"treasure":2,"kraken":1}`, KRClaimPayload{Treasure: 2, Kraken: 1}},
		{`{"treasure":2,"kraken":true}`, KRClaimPayload{Treasure: 2, Kraken: 1}},
		{`{"treasure":0,"kraken":false}`, KRClaimPayload{Treasure: 0, Kraken: 0}},
		{`{"treasure":3}`, KRClaimPayload{Treasure: 3, Kraken: 0}},
		{`{}`, KRClaimPayload{}},
	}
	for _, tc := range cases {
		var got KRClaimPayload
		if err := json.Unmarshal([]byte(tc.raw), &got); err != nil {
			t.Fatalf("Unmarshal(%s): %v", tc.raw, err)
		}
		if got != tc.want {
			t.Fatalf("Unmarshal(%s) = %+v, want %+v", tc.raw, got, tc.want)
		}
	}
	out, _ := json.Marshal(KRClaim{Treasure: 1, Kraken: 1})
	if string(out) != `{"treasure":1,"kraken":1}` {
		t.Fatalf("claim 출력 = %s", out)
	}
}
