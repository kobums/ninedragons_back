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

// 테스트에서는 차례 마감과 봇의 생각 시간을 짧게 낮춘다 (실사용은 45초)
func init() {
	sbTurnTimeout = 150 * time.Millisecond
	sbBotDelay = 0
	sbBotJitterMs = 0
}

// sbTestClient 공용 testConn 에 사보타지 메시지 타입의 waitFor 를 얹은 래퍼
type sbTestClient struct {
	testConn[SBMessage]
}

func newSBTestServer(t *testing.T, grace time.Duration) (*SBHub, string, func()) {
	t.Helper()
	hub := NewSBHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeSBWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func sbDial(t *testing.T, url string) *sbTestClient {
	t.Helper()
	return &sbTestClient{dialWS[SBMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *sbTestClient) waitFor(t *testing.T, msgType SBMessageType) SBMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m SBMessage) bool { return m.Type == msgType })
}

func sbPayloadMap(t *testing.T, msg SBMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// sbJoin 입장하고 sb_player_joined payload 를 돌려준다
func sbJoin(t *testing.T, c *sbTestClient, name, room string) map[string]interface{} {
	t.Helper()
	c.send(t, SBMessage{Type: SBMsgJoinGame, Payload: SBJoinGamePayload{Name: name, Room: room}})
	return sbPayloadMap(t, c.waitFor(t, SBMsgPlayerJoined))
}

// sbWaitPhase 지정한 phase 의 스냅샷이 올 때까지 소비
func (c *sbTestClient) sbWaitPhase(t *testing.T, phase string) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "sb_game_state("+phase+")", func(m SBMessage) bool {
		if m.Type != SBMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		return ok && state["phase"] == phase
	})
	return sbPayloadMap(t, msg)
}

// sbDrain 배경으로 소켓을 비운다 (읽지 않는 연결의 버퍼 포화 방지)
func sbDrain(c *sbTestClient) {
	conn := c.conn
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

// TestSBFiveBotsCompleteGame 봇을 채운 5인 게임이 60초 안에 완주하는지 —
// 가장 중요한 회귀 장치 (차례 교착·배치 판정 오류·종료 판정 감지).
// 매 차례 손패에서 1장이 영구히 빠지므로 40차례 안에 반드시 끝난다.
// 좌석 0은 서버 연습봇 두뇌(sbBrain)를 WS 로 감싼 드라이버가 잡는다.
func TestSBFiveBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newSBTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := sbDial(t, url)
	defer c.conn.Close()
	sbJoin(t, c, "감독", "")
	c.send(t, SBMessage{Type: SBMsgFillBots}) // 5인까지 채우고 즉시 시작

	start := time.Now()
	brain := newSBBrain()
	deadline := start.Add(60 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "state-or-over", func(m SBMessage) bool {
			return m.Type == SBMsgGameState || m.Type == SBMsgGameOver
		})
		if msg.Type == SBMsgGameOver {
			over := sbPayloadMap(t, msg)
			winner, _ := over["winner"].(string)
			if winner != string(SBRoleMiner) && winner != string(SBRoleSaboteur) {
				t.Fatalf("winner = %q (무승부는 없다)", winner)
			}
			reason, _ := over["reason"].(string)
			if reason != "gold" && reason != "exhausted" {
				t.Fatalf("reason = %q", reason)
			}
			if m, _ := over["message"].(string); m == "" {
				t.Fatalf("종료 문구 부재: %v", over)
			}
			turns := int(over["turns"].(float64))
			if turns < 1 || turns > SBDeckSize {
				t.Fatalf("turns = %d (덱이 %d장이라 그 이상은 불가능하다)", turns, SBDeckSize)
			}
			goldIndex := int(over["goldIndex"].(float64))
			if goldIndex < 0 || goldIndex >= len(sbGoalCells) {
				t.Fatalf("goldIndex = %d", goldIndex)
			}

			// 종료 화면에서는 목표 타일 3장이 모두 뒤집혀 금 위치가 드러난다
			golds := 0
			for _, cellRaw := range over["board"].([]interface{}) {
				cell := cellRaw.(map[string]interface{})
				if cell["kind"] != string(SBTileGoal) {
					continue
				}
				if cell["revealed"] != true {
					t.Fatalf("종료 화면에 안 뒤집힌 목표 타일: %v", cell)
				}
				if cell["gold"] == true {
					golds++
				}
			}
			if golds != 1 {
				t.Fatalf("종료 화면 금덩이 = %d개, want 1", golds)
			}

			players := over["players"].([]interface{})
			if len(players) != SBFillBotTarget {
				t.Fatalf("players 길이 = %d, want %d", len(players), SBFillBotTarget)
			}
			roles := map[string]int{}
			for _, pRaw := range players {
				p := pRaw.(map[string]interface{})
				role, _ := p["role"].(string)
				if role != string(SBRoleMiner) && role != string(SBRoleSaboteur) {
					t.Fatalf("종료 화면에 역할 미공개: %v", p)
				}
				roles[role]++
			}
			pool := sbRolePoolFor(SBFillBotTarget)
			if roles[string(SBRoleMiner)] > pool.Miner || roles[string(SBRoleSaboteur)] > pool.Saboteur {
				t.Fatalf("종료 역할 분포가 풀을 넘었다: %v vs %+v", roles, pool)
			}
			t.Logf("완주: winner=%s reason=%s turns=%d (%.1fs)",
				winner, reason, turns, time.Since(start).Seconds())
			return
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("60초 안에 게임이 끝나지 않았다 — 진행 불가 상태")
}

// sbHiddenFixture 허브 고루틴 없이 결정적으로 검증하기 위한 5인 방
func sbHiddenFixture(t *testing.T) (*SBHub, *sbRoom, []*SBClient) {
	t.Helper()
	h := NewSBHub()
	room := h.lobbyRoomFor("")
	clients := make([]*SBClient, 5)
	for i := range clients {
		c := &SBClient{wsClient: newBotWSClient(), Hub: h}
		c.Bot = false // 소켓 없는 사람 취급
		c.Name = fmt.Sprintf("P%d", i)
		seat, err := room.Game.AddPlayer(c.Name)
		if err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
		c.GameID, c.Seat = room.Game.ID, seat
		room.Clients[seat] = c
		h.sessions[c.SessionID] = c
		clients[i] = c
	}
	h.startGame(room)
	return h, room, clients
}

// sbTakeMessages 봇 채널에 쌓인 메시지를 모두 꺼낸다 (개인 통지 검증용)
func sbTakeMessages(t *testing.T, c *SBClient) []SBMessage {
	t.Helper()
	out := []SBMessage{}
	for {
		select {
		case data := <-c.Send:
			var msg SBMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			out = append(out, msg)
		default:
			return out
		}
	}
}

// TestSBHiddenState 은닉의 핵심 계약 — 허브 고루틴 없이 핸들러를 직접 불러
// 결정적으로 검증한다.
//   - yourRole·yourHand 는 본인 스냅샷에만 (타인·관전자 raw JSON 에 키 부재)
//   - 목표 타일의 gold 는 공개 전까지 어떤 스냅샷에도 없다
//   - players[].role 은 game_over 전까지 전원 ""
func TestSBHiddenState(t *testing.T) {
	h, room, clients := sbHiddenFixture(t)
	game := room.Game

	// 결정적 구도: seat1 만 파괴꾼, 나머지 광부. 금은 가운데 목표 타일.
	for _, p := range game.Players {
		p.Role = SBRoleMiner
	}
	game.Players[1].Role = SBRoleSaboteur
	game.GoldIndex = 1
	game.CurrentSeat = 0

	rawOf := func(viewer int) string {
		data, err := json.Marshal(h.buildSBState(room, viewer))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(data)
	}

	// ---- yourRole: 본인만 (관전자는 키 자체 부재) ----
	if !strings.Contains(rawOf(1), `"yourRole":"saboteur"`) {
		t.Fatalf("파괴꾼 본인에게 역할이 없다:\n%s", rawOf(1))
	}
	if !strings.Contains(rawOf(0), `"yourRole":"miner"`) {
		t.Fatalf("광부 본인에게 역할이 없다:\n%s", rawOf(0))
	}
	rawSpec := rawOf(-1)
	if strings.Contains(rawSpec, `"yourRole"`) {
		t.Fatalf("관전자 스냅샷에 yourRole 키 유출:\n%s", rawSpec)
	}
	// 진영 이름이 "값"으로 등장하면 유출이다 — rolePool 의 키 표기만 허용한다
	for _, role := range []string{`:"saboteur"`, `:"miner"`} {
		if strings.Contains(rawSpec, role) {
			t.Fatalf("관전자 스냅샷에 진영 값 유출(%s):\n%s", role, rawSpec)
		}
	}

	// ---- yourHand: 본인만, 키는 정확히 하나 ----
	if !strings.Contains(rawOf(3), `"yourHand":[`) {
		t.Fatalf("본인 손패가 스냅샷에 없다:\n%s", rawOf(3))
	}
	if strings.Contains(rawSpec, `"yourHand"`) {
		t.Fatalf("관전자 스냅샷에 yourHand 키 유출:\n%s", rawSpec)
	}
	if strings.Count(rawOf(0), `"yourHand"`) != 1 {
		t.Fatalf("yourHand 키가 본인 것 하나가 아니다:\n%s", rawOf(0))
	}
	// 손패를 다 쓴 좌석은 [] 로 나간다 (nil → null 금지)
	game.Players[4].Hand = []SBCard{}
	if !strings.Contains(rawOf(4), `"yourHand":[]`) {
		t.Fatalf("빈 손패가 []가 아니다:\n%s", rawOf(4))
	}

	// ---- 목표 타일의 gold 는 공개 전까지 누구에게도 없다 ----
	for _, viewer := range []int{0, 1, 2, 3, 4, -1} {
		if strings.Contains(rawOf(viewer), `"gold"`) {
			t.Fatalf("viewer %d 스냅샷에 gold 키 유출:\n%s", viewer, rawOf(viewer))
		}
	}

	// ---- 진행 중 players[].role 은 전원 "" ----
	for _, viewer := range []int{0, 1, 2, -1} {
		for _, pv := range h.buildSBState(room, viewer).Players {
			if pv.Role != "" {
				t.Fatalf("viewer %d 에게 seat%d 역할 유출: %q", viewer, pv.Seat, pv.Role)
			}
		}
	}

	// ---- 관전자 스냅샷은 패닉 없이 빌드되고 빈 슬라이스는 [] 다 ----
	spec := h.buildSBState(room, -1)
	if spec.YourSeat != -1 || spec.CurrentSeat != 0 || spec.RolePool != sbRolePoolFor(5) {
		t.Fatalf("관전자 스냅샷: yourSeat=%d current=%d pool=%+v",
			spec.YourSeat, spec.CurrentSeat, spec.RolePool)
	}
	if !strings.Contains(rawSpec, `"result":null`) ||
		!strings.Contains(rawSpec, `"lastAction":null`) ||
		!strings.Contains(rawSpec, `"board":[`) {
		t.Fatalf("관전자 raw 스냅샷 이상:\n%s", rawSpec)
	}
	empty := h.lobbyRoomFor("ZZZZ")
	raw, _ := json.Marshal(h.buildSBState(empty, -1))
	if !strings.Contains(string(raw), `"players":[]`) || !strings.Contains(string(raw), `"board":[]`) {
		t.Fatalf("빈 슬라이스가 []가 아니다:\n%s", raw)
	}

	// ---- 차례가 아닌 좌석의 배치는 거부된다 ----
	game.Players[2].Hand = []SBCard{sbTHoriz}
	h.handleGameMessage(SBGameMessage{Client: clients[2], Message: SBMessage{
		Type: SBMsgPlace, Payload: SBPlacePayload{Index: 0, Col: 1, Row: 2}}})
	if game.Board[sbIdx(1, 2)] != nil {
		t.Fatal("차례가 아닌 좌석의 배치가 통과했다")
	}

	// ---- 지도 결과는 쓴 사람에게만 간다 (sb_map) ----
	for _, c := range clients {
		sbTakeMessages(t, c) // 여기까지의 메시지를 비운다
	}
	game.CurrentSeat = 0
	game.Players[0].Hand = []SBCard{{Kind: SBCardMap}}
	gc := sbGoalCells[game.GoldIndex]
	h.handleGameMessage(SBGameMessage{Client: clients[0], Message: SBMessage{
		Type:    SBMsgAction,
		Payload: SBActionPayload{Index: 0, Col: gc[0], Row: gc[1]}}})

	sawMap := false
	for i, c := range clients {
		for _, msg := range sbTakeMessages(t, c) {
			if msg.Type != SBMsgMap {
				continue
			}
			if i != 0 {
				t.Fatalf("seat%d 에게 남의 지도 결과가 갔다: %+v", i, msg.Payload)
			}
			payload := asPayloadMap(t, msg.Payload)
			if int(payload["index"].(float64)) != game.GoldIndex || payload["gold"] != true {
				t.Fatalf("지도 결과 = %v (금 위치를 잘못 알려 줬다)", payload)
			}
			sawMap = true
		}
	}
	if !sawMap {
		t.Fatal("지도를 쓴 사람에게 sb_map 이 안 갔다")
	}
	// 지도를 쓴 뒤에도 판 스냅샷의 gold 는 그대로 감춰져 있다
	for _, viewer := range []int{0, -1} {
		if strings.Contains(rawOf(viewer), `"gold"`) {
			t.Fatalf("지도 사용 후 viewer %d 스냅샷에 gold 유출:\n%s", viewer, rawOf(viewer))
		}
	}

	// ---- 금맥에 닿으면 광부 승리 + 전원 역할·금 위치 공개 ----
	game.Board = sbRow2(6)
	game.CurrentSeat = 0
	game.Players[0].Hand = []SBCard{sbTCross}
	h.handleGameMessage(SBGameMessage{Client: clients[0], Message: SBMessage{
		Type: SBMsgPlace, Payload: SBPlacePayload{Index: 0, Col: 7, Row: 2}}})
	if game.Phase != SBPhaseGameOver || game.Result == nil ||
		game.Result.Winner != string(SBRoleMiner) {
		t.Fatalf("금맥에 닿았는데 result = %+v (phase %s)", game.Result, game.Phase)
	}
	overSpec := h.buildSBState(room, -1)
	roles := map[string]int{}
	for _, pv := range overSpec.Players {
		roles[pv.Role]++
	}
	if roles[string(SBRoleSaboteur)] != 1 || roles[string(SBRoleMiner)] != 4 {
		t.Fatalf("종료 후 역할 공개 이상: %v", roles)
	}
	if !strings.Contains(rawOf(-1), `"gold":true`) {
		t.Fatalf("종료 스냅샷에 금 위치가 없다:\n%s", rawOf(-1))
	}
	// 종료 뒤에도 남의 손패는 실리지 않는다 (본인 것만)
	if strings.Count(rawOf(0), `"yourHand"`) != 1 || strings.Contains(rawOf(-1), `"yourHand"`) {
		t.Fatal("종료 스냅샷에서 손패 은닉이 깨졌다")
	}
}

// TestSBAfkAutoProgress 접속만 유지한 채 아무도 내지 않는 3인전 —
// 차례 마감의 자동 버리기만으로 게임이 끝까지 완주하는지
// (endsAt 노출·afk 이벤트 포함). 카드가 유한하므로 반드시 끝난다.
func TestSBAfkAutoProgress(t *testing.T) {
	_, url, cleanup := newSBTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	conns := make([]*sbTestClient, SBMinPlayers)
	for i := range conns {
		conns[i] = sbDial(t, url)
		defer conns[i].conn.Close()
		sbJoin(t, conns[i], fmt.Sprintf("잠수%d", i), "")
	}
	host := conns[0]
	host.send(t, SBMessage{Type: SBMsgStart})

	state := host.sbWaitPhase(t, string(SBPhasePlaying))
	if ends := int64(state["endsAt"].(float64)); ends <= 0 {
		t.Fatalf("차례 스냅샷의 endsAt = %d, want unixMillis", ends)
	}
	if int(state["deckLeft"].(float64)) != SBDeckSize-SBMinPlayers*sbHandSize(SBMinPlayers) {
		t.Fatalf("덱 잔량 = %v", state["deckLeft"])
	}
	hand, ok := state["yourHand"].([]interface{})
	if !ok || len(hand) != sbHandSize(SBMinPlayers) {
		t.Fatalf("본인 손패 = %v", state["yourHand"])
	}
	pool := state["rolePool"].(map[string]interface{})
	if int(pool["saboteur"].(float64)) != 1 || int(pool["miner"].(float64)) != 3 {
		t.Fatalf("3인 rolePool = %v", pool)
	}
	// 판에는 시작 타일 1장 + 목표 타일 3장이 이미 깔려 있고 gold 는 감춰져 있다
	board := state["board"].([]interface{})
	if len(board) != 4 {
		t.Fatalf("시작 판 = %d칸, want 4", len(board))
	}
	for _, cellRaw := range board {
		cell := cellRaw.(map[string]interface{})
		if _, leaked := cell["gold"]; leaked {
			t.Fatalf("시작 판에 gold 유출: %v", cell)
		}
	}

	for _, c := range conns[1:] {
		sbDrain(c)
	}

	sawAfk := false
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		msg := host.waitMatch(t, "event-or-over", func(m SBMessage) bool {
			return m.Type == SBMsgEvent || m.Type == SBMsgGameOver
		})
		if msg.Type == SBMsgEvent {
			ev := sbPayloadMap(t, msg)
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
		over := sbPayloadMap(t, msg)
		if over["winner"] != string(SBRoleSaboteur) {
			t.Fatalf("전원 방치인데 winner = %v", over["winner"])
		}
		if !sawAfk {
			t.Fatal("afk 자동 진행 이벤트가 한 번도 없었다")
		}
		return
	}
	t.Fatal("전원 방치 게임이 30초 안에 끝나지 않았다")
}

// TestSBRoomCodeAndSpectate 방 코드 발급·코드 입장·시작 후 코드 관전 진입.
// 관전자 스냅샷은 yourSeat -1 에 yourRole·yourHand 부재. 행동은 전부 차단된다.
func TestSBRoomCodeAndSpectate(t *testing.T) {
	_, url, cleanup := newSBTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := sbDial(t, url)
	defer host.conn.Close()
	joined := sbJoin(t, host, "호스트", "NEW")
	code, _ := joined["roomCode"].(string)
	if len(code) != roomCodeLen {
		t.Fatalf("roomCode = %q, want %d자", code, roomCodeLen)
	}

	guests := make([]*sbTestClient, SBMinPlayers-1)
	for i := range guests {
		guests[i] = sbDial(t, url)
		defer guests[i].conn.Close()
		g := sbJoin(t, guests[i], fmt.Sprintf("친구%d", i), code)
		if g["roomCode"] != code || int(g["yourSeat"].(float64)) != i+1 {
			t.Fatalf("코드 입장 실패: %v", g)
		}
	}

	host.send(t, SBMessage{Type: SBMsgStart})
	state := host.sbWaitPhase(t, string(SBPhasePlaying))
	if state["roomCode"] != code || len(state["players"].([]interface{})) != SBMinPlayers {
		t.Fatalf("시작 실패: %v", state["players"])
	}
	role, _ := state["yourRole"].(string)
	if role != string(SBRoleMiner) && role != string(SBRoleSaboteur) {
		t.Fatalf("yourRole = %q", role)
	}
	for _, pRaw := range state["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		if p["role"] != "" {
			t.Fatalf("진행 중 좌석 역할 유출: %v", p)
		}
		tools := p["tools"].(map[string]interface{})
		if tools["pick"] != true || tools["cart"] != true || tools["lamp"] != true {
			t.Fatalf("시작 장비 = %v", tools)
		}
	}
	for _, c := range guests {
		sbDrain(c)
	}

	// 시작된 방의 코드로 들어오면 관전자
	spec := sbDial(t, url)
	defer spec.conn.Close()
	spec.send(t, SBMessage{Type: SBMsgJoinGame, Payload: SBJoinGamePayload{Name: "구경꾼", Room: code}})
	specJoined := sbPayloadMap(t, spec.waitFor(t, SBMsgSpectateJoined))
	if specJoined["roomCode"] != code {
		t.Fatalf("관전 입장 roomCode = %v", specJoined["roomCode"])
	}

	specState := sbPayloadMap(t, spec.waitFor(t, SBMsgGameState))
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
	if int(specPool["saboteur"].(float64)) != 1 {
		t.Fatalf("관전자 rolePool = %v (풀은 공개 정보)", specPool)
	}

	// 관전자는 어떤 행동도 못 한다
	spec.send(t, SBMessage{Type: SBMsgDiscard, Payload: SBDiscardPayload{Index: 0}})
	errPayload := sbPayloadMap(t, spec.waitFor(t, SBMsgError))
	if errPayload["message"] != spectatorDeniedMsg {
		t.Fatalf("관전자 차단 문구 = %v", errPayload["message"])
	}
}

// TestSBReconnect 재접속 3종 — 이탈 통지(sb_player_disconnected) 후 세션으로
// 돌아오면 좌석·역할·손패가 그대로 복원되고(sb_player_reconnected), 모르는
// 세션은 sb_session_expired 로 거절된다.
func TestSBReconnect(t *testing.T) {
	_, url, cleanup := newSBTestServer(t, 3*time.Second)
	defer cleanup()

	conns := make([]*sbTestClient, SBMinPlayers)
	sessions := make([]string, SBMinPlayers)
	for i := range conns {
		conns[i] = sbDial(t, url)
		joined := sbJoin(t, conns[i], fmt.Sprintf("P%d", i), "")
		sessions[i], _ = joined["sessionId"].(string)
	}
	defer conns[0].conn.Close()
	conns[0].send(t, SBMessage{Type: SBMsgStart})
	before := conns[0].sbWaitPhase(t, string(SBPhasePlaying))
	if r, _ := before["yourRole"].(string); r == "" {
		t.Fatal("시작 스냅샷에 yourRole 부재")
	}
	for _, c := range conns[2:] {
		sbDrain(c)
	}

	// 좌석 1 이탈 → 남은 사람에게 이탈 통지
	conns[1].conn.Close()
	discon := sbPayloadMap(t, conns[0].waitFor(t, SBMsgPlayerDisconnected))
	if int(discon["seat"].(float64)) != 1 || discon["name"] != "P1" {
		t.Fatalf("이탈 통지 = %v", discon)
	}
	if int(discon["graceSeconds"].(float64)) <= 0 {
		t.Fatalf("graceSeconds = %v", discon["graceSeconds"])
	}

	// 세션으로 재접속 → 좌석·역할·손패 복원
	back := sbDial(t, url)
	defer back.conn.Close()
	back.send(t, SBMessage{Type: SBMsgRejoin, Payload: SBRejoinPayload{SessionID: sessions[1]}})
	recon := sbPayloadMap(t, back.waitFor(t, SBMsgPlayerReconnected))
	if int(recon["seat"].(float64)) != 1 {
		t.Fatalf("재접속 통지 = %v", recon)
	}
	restored := sbPayloadMap(t, back.waitFor(t, SBMsgGameState))
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
	ghost := sbDial(t, url)
	defer ghost.conn.Close()
	ghost.send(t, SBMessage{Type: SBMsgRejoin, Payload: SBRejoinPayload{SessionID: "없는-세션"}})
	ghost.waitFor(t, SBMsgSessionExpired)
}

// TestSBBotTakeover 유예 만료 좌석을 봇이 이어받아 게임이 멈추지 않는지
func TestSBBotTakeover(t *testing.T) {
	_, url, cleanup := newSBTestServer(t, 120*time.Millisecond)
	defer cleanup()

	conns := make([]*sbTestClient, SBMinPlayers)
	for i := range conns {
		conns[i] = sbDial(t, url)
		defer conns[i].conn.Close()
		sbJoin(t, conns[i], fmt.Sprintf("P%d", i), "")
	}
	conns[0].send(t, SBMessage{Type: SBMsgStart})
	conns[0].sbWaitPhase(t, string(SBPhasePlaying))
	for _, c := range conns[2:] {
		sbDrain(c)
	}

	// 좌석 1 이탈 → 유예 만료 → 봇 대체
	conns[1].conn.Close()
	sawTakeover := false
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		msg := conns[0].waitMatch(t, "event-or-over", func(m SBMessage) bool {
			return m.Type == SBMsgEvent || m.Type == SBMsgGameOver
		})
		if msg.Type == SBMsgEvent {
			ev := sbPayloadMap(t, msg)
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
		if _, ok := sbPayloadMap(t, msg)["winner"].(string); !ok {
			t.Fatalf("종료 payload = %v", msg.Payload)
		}
		return
	}
	t.Fatal("봇 대체 후 게임이 30초 안에 끝나지 않았다")
}

// TestSBPlaceRejections 와이어로 들어온 잘못된 배치·행동이 한글 오류로
// 되돌아오고 판을 건드리지 않는지
func TestSBPlaceRejections(t *testing.T) {
	_, room, clients := sbHiddenFixture(t)
	h := clients[0].Hub
	game := room.Game
	seat := game.CurrentSeat
	game.Players[seat].Hand = []SBCard{sbTHoriz, {Kind: SBCardRockfall}}

	bad := []SBMessage{
		{Type: SBMsgPlace, Payload: SBPlacePayload{Index: 0, Col: 5, Row: 0}},   // 이어진 길과 무관
		{Type: SBMsgPlace, Payload: SBPlacePayload{Index: 0, Col: 0, Row: 2}},   // 시작 타일 자리
		{Type: SBMsgPlace, Payload: SBPlacePayload{Index: 9, Col: 1, Row: 2}},   // 없는 카드
		{Type: SBMsgPlace, Payload: SBPlacePayload{Index: 1, Col: 1, Row: 2}},   // 길 타일이 아님
		{Type: SBMsgAction, Payload: SBActionPayload{Index: 0, Col: 1, Row: 2}}, // 행동 카드가 아님
		{Type: SBMsgAction, Payload: SBActionPayload{Index: 1, Col: 4, Row: 4}}, // 걷어낼 타일 없음
	}
	for i, msg := range bad {
		for _, c := range clients {
			sbTakeMessages(t, c)
		}
		h.handleGameMessage(SBGameMessage{Client: clients[seat], Message: msg})
		if game.Turns != 0 || game.CurrentSeat != seat {
			t.Fatalf("%d번째 잘못된 요청이 차례를 넘겼다", i)
		}
		sawError := false
		for _, out := range sbTakeMessages(t, clients[seat]) {
			if out.Type != SBMsgError {
				continue
			}
			text, _ := asPayloadMap(t, out.Payload)["message"].(string)
			if text == "" || !hasHangul(text) {
				t.Fatalf("%d번째 오류 문구가 한글이 아니다: %q", i, text)
			}
			sawError = true
		}
		if !sawError {
			t.Fatalf("%d번째 잘못된 요청에 sb_error 가 없다", i)
		}
	}

	// 정상 배치는 통과하고 차례가 넘어간다
	h.handleGameMessage(SBGameMessage{Client: clients[seat], Message: SBMessage{
		Type: SBMsgPlace, Payload: SBPlacePayload{Index: 0, Col: 1, Row: 2}}})
	if game.Board[sbIdx(1, 2)] == nil || game.Turns != 1 {
		t.Fatalf("정상 배치가 반영되지 않았다 (turns=%d)", game.Turns)
	}
	if game.LastAction == nil || !strings.Contains(game.LastAction.Message, "(1,2)") {
		t.Fatalf("lastAction = %+v", game.LastAction)
	}
}

func hasHangul(s string) bool {
	for _, r := range s {
		if r >= 0xAC00 && r <= 0xD7A3 {
			return true
		}
	}
	return false
}
