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
// (실사용은 직업 선택 45초 · 차례 60초 · 직업 능력 30초)
func init() {
	ctPickTimeout = 100 * time.Millisecond
	ctTurnTimeout = 120 * time.Millisecond
	ctAbilityTimeout = 80 * time.Millisecond
	ctBotDelay = 0
	ctBotJitterMs = 0
}

// ctTestClient 공용 testConn 에 시타델 메시지 타입의 waitFor 를 얹은 래퍼
type ctTestClient struct {
	testConn[CTMessage]
}

func newCTTestServer(t *testing.T, grace time.Duration) (*CTHub, string, func()) {
	t.Helper()
	hub := NewCTHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeCTWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func ctDial(t *testing.T, url string) *ctTestClient {
	t.Helper()
	return &ctTestClient{dialWS[CTMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *ctTestClient) waitFor(t *testing.T, msgType CTMessageType) CTMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m CTMessage) bool { return m.Type == msgType })
}

func ctPayloadMap(t *testing.T, msg CTMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// ctJoin 입장하고 ct_player_joined payload 를 돌려준다
func ctJoin(t *testing.T, c *ctTestClient, name, room string) map[string]interface{} {
	t.Helper()
	c.send(t, CTMessage{Type: CTMsgJoinGame, Payload: CTJoinGamePayload{Name: name, Room: room}})
	return ctPayloadMap(t, c.waitFor(t, CTMsgPlayerJoined))
}

// ctWaitPhase 지정한 phase 의 스냅샷이 올 때까지 소비
func (c *ctTestClient) ctWaitPhase(t *testing.T, phase string) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "ct_game_state("+phase+")", func(m CTMessage) bool {
		if m.Type != CTMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		return ok && state["phase"] == phase
	})
	return ctPayloadMap(t, msg)
}

// ctDrainConn 배경으로 소켓을 비운다 (읽지 않는 연결의 버퍼 포화 방지)
func ctDrainConn(c *ctTestClient) {
	conn := c.conn
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

// TestCTFourBotsCompleteGame 봇을 채운 4인 게임이 120초 안에 완주하는지 —
// 가장 중요한 회귀 장치 (직업 호출 교착·건설 비용 오류·종료 판정 감지).
// 좌석 0은 서버 연습봇 두뇌(ctBrain)를 WS 로 감싼 드라이버가 잡는다.
func TestCTFourBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newCTTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := ctDial(t, url)
	defer c.conn.Close()
	ctJoin(t, c, "감독", "")
	c.send(t, CTMessage{Type: CTMsgFillBots}) // 4인까지 채우고 즉시 시작

	start := time.Now()
	brain := newCTBrain()
	deadline := start.Add(120 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "state-or-over", func(m CTMessage) bool {
			return m.Type == CTMsgGameState || m.Type == CTMsgGameOver || m.Type == CTMsgError
		})
		if msg.Type == CTMsgGameOver {
			over := ctPayloadMap(t, msg)
			seats, _ := over["winnerSeats"].([]interface{})
			names, _ := over["winnerNames"].([]interface{})
			if len(seats) == 0 || len(seats) != len(names) {
				t.Fatalf("승자 = %v / %v", over["winnerSeats"], over["winnerNames"])
			}
			if m, _ := over["message"].(string); m == "" || !hasHangul(m) {
				t.Fatalf("종료 문구 = %v", over["message"])
			}
			rounds := int(over["rounds"].(float64))
			if rounds < 1 || rounds > CTMaxRounds {
				t.Fatalf("rounds = %d", rounds)
			}

			rows := over["rows"].([]interface{})
			if len(rows) != CTFillBotTarget {
				t.Fatalf("점수 내역 = %d줄", len(rows))
			}
			for _, rRaw := range rows {
				row := rRaw.(map[string]interface{})
				if _, ok := row["seat"]; !ok {
					t.Fatalf("내역에 seat 부재: %v", row)
				}
				if d, _ := row["detail"].(string); !hasHangul(d) {
					t.Fatalf("내역 문구 = %v", row["detail"])
				}
			}

			players := over["players"].([]interface{})
			if len(players) != CTFillBotTarget {
				t.Fatalf("players 길이 = %d, want %d", len(players), CTFillBotTarget)
			}
			bestBuilt, bestScore := 0, 0
			for _, pRaw := range players {
				p := pRaw.(map[string]interface{})
				built := p["built"].([]interface{})
				if len(built) > bestBuilt {
					bestBuilt = len(built)
				}
				if s := int(p["score"].(float64)); s > bestScore {
					bestScore = s
				}
				// 종료 화면에도 남의 손패 내용은 없다
				if _, leaked := p["hand"]; leaked {
					t.Fatalf("종료 화면에 손패 유출: %v", p)
				}
				if _, ok := p["handCount"]; !ok {
					t.Fatalf("handCount 부재: %v", p)
				}
			}
			if bestBuilt < CTBuildTarget {
				t.Fatalf("최다 건물 = %d채 — %d채에 못 닿고 끝났다", bestBuilt, CTBuildTarget)
			}
			t.Logf("완주: 승자 %v · 최고 승점 %d · 최다 %d채 · %d라운드 (%.1fs)",
				over["winnerNames"], bestScore, bestBuilt, rounds, time.Since(start).Seconds())
			return
		}
		if msg.Type == CTMsgError {
			continue // 두뇌가 스스로 복구한다 (도둑 지목 충돌 등)
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("120초 안에 게임이 끝나지 않았다 — 진행 불가 상태")
}

// ctTakeMessages 봇 채널에 쌓인 메시지를 모두 꺼낸다
func ctTakeMessages(t *testing.T, c *CTClient) []CTMessage {
	t.Helper()
	out := []CTMessage{}
	for {
		select {
		case data := <-c.Send:
			var msg CTMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			out = append(out, msg)
		default:
			return out
		}
	}
}

// ctRawState viewerSeat 관점의 raw JSON 스냅샷
func ctRawState(t *testing.T, h *CTHub, room *ctRoom, viewer int) string {
	t.Helper()
	data, err := json.Marshal(h.buildCTState(room, viewer))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(data)
}

// TestCTHiddenState 은닉의 핵심 계약 — 허브 고루틴 없이 핸들러를 직접 불러
// 결정적으로 검증한다.
//   - yourRole·yourHand 는 본인 스냅샷에만 (타인·관전자 raw JSON 에 키 부재)
//   - pickPool 은 지금 고르는 좌석에만
//   - yourDraw 는 keep_card 단계의 본인에게만
//   - 남의 직업은 호출로 공개되기 전까지 roleRevealed 0
//   - 뒷면으로 제외된 직업은 어떤 스냅샷에도 없다
//   - 관전자(viewerSeat -1) 스냅샷이 패닉 없이 만들어지고 빈 슬라이스는 [] 다
func TestCTHiddenState(t *testing.T) {
	h, room, clients := ctBotFixture(t, 4, 12345)
	game := room.Game

	// ---- 직업 선택 단계 ----
	if game.Phase != CTPhasePickRoles {
		t.Fatalf("시작 단계 = %s", game.Phase)
	}
	picker := game.CurrentSeat
	for _, viewer := range []int{0, 1, 2, 3, -1} {
		raw := ctRawState(t, h, room, viewer)
		hasPool := strings.Contains(raw, `"pickPool"`)
		if viewer == picker && !hasPool {
			t.Fatalf("고르는 좌석 %d 에 pickPool 부재:\n%s", viewer, raw)
		}
		if viewer != picker && hasPool {
			t.Fatalf("고르지 않는 뷰어 %d 에 pickPool 유출:\n%s", viewer, raw)
		}
		if viewer >= 0 {
			if !strings.Contains(raw, `"yourRole"`) || !strings.Contains(raw, `"yourHand"`) {
				t.Fatalf("본인 스냅샷에 yourRole/yourHand 부재:\n%s", raw)
			}
			if strings.Count(raw, `"yourRole"`) != 1 || strings.Count(raw, `"yourHand"`) != 1 {
				t.Fatalf("은닉 키가 하나가 아니다:\n%s", raw)
			}
		} else {
			for _, key := range []string{`"yourRole"`, `"yourHand"`, `"yourDraw"`, `"pickPool"`} {
				if strings.Contains(raw, key) {
					t.Fatalf("관전자 스냅샷에 %s 유출:\n%s", key, raw)
				}
			}
		}
		if strings.Contains(raw, `"yourDraw"`) {
			t.Fatalf("직업 선택 단계에 yourDraw 유출 (viewer %d)", viewer)
		}
		// 아직 아무도 호출되지 않았으니 전원 비공개다
		for _, pv := range h.buildCTState(room, viewer).Players {
			if pv.RoleRevealed != 0 {
				t.Fatalf("선택 단계에 직업이 공개됐다: seat%d=%d", pv.Seat, pv.RoleRevealed)
			}
		}
	}

	// 선택 후보에 뒷면 제외 직업은 없다
	pool := h.buildCTState(room, picker).PickPool
	if pool == nil {
		t.Fatal("고르는 좌석에 pickPool 이 없다")
	}
	for _, r := range *pool {
		if r == game.FaceDown {
			t.Fatalf("뒷면 제외 직업(%d)이 후보에 있다: %v", game.FaceDown, *pool)
		}
	}
	for _, r := range game.FaceUp {
		if r == game.FaceDown {
			t.Fatalf("뒷면 제외 직업이 앞면 공개에 섞였다: %d", game.FaceDown)
		}
	}

	// ---- 전원 직업 선택 → 호출 시작 ----
	for game.Phase == CTPhasePickRoles {
		seat := game.CurrentSeat
		state := h.buildCTState(room, seat)
		if state.PickPool == nil || len(*state.PickPool) == 0 {
			t.Fatalf("seat%d 후보가 비었다", seat)
		}
		h.handleGameMessage(CTGameMessage{Client: clients[seat], Message: CTMessage{
			Type: CTMsgPickRole, Payload: CTPickRolePayload{Role: (*state.PickPool)[0]}}})
	}
	for _, p := range game.Players {
		if p.Role == game.FaceDown {
			t.Fatalf("뒷면 제외 직업(%d)을 누군가 쥐었다", game.FaceDown)
		}
	}

	// 호출된 좌석만 직업이 공개된다
	called := game.CurrentSeat
	for _, viewer := range []int{0, 1, 2, 3, -1} {
		for _, pv := range h.buildCTState(room, viewer).Players {
			if pv.Seat == called && pv.RoleRevealed == 0 {
				t.Fatalf("호출된 seat%d 의 직업이 비공개다", called)
			}
			if pv.Seat != called && pv.RoleRevealed != 0 {
				t.Fatalf("아직 안 불린 seat%d 의 직업이 공개됐다 (%d)", pv.Seat, pv.RoleRevealed)
			}
		}
	}
	// 본인 스냅샷의 yourRole 은 실제 직업과 같고, 남은 그 값을 볼 수 없다
	mine := h.buildCTState(room, called)
	if mine.YourRole == nil || *mine.YourRole != game.Players[called].Role {
		t.Fatalf("yourRole = %v, want %d", mine.YourRole, game.Players[called].Role)
	}
	other := (called + 1) % len(game.Players)
	otherRole := game.Players[other].Role
	if h.buildCTState(room, called).Players[other].RoleRevealed == otherRole {
		t.Fatalf("남의 비공개 직업(%d)이 새어 나왔다", otherRole)
	}

	// ---- keep_card: 뽑은 2장은 본인만 본다 ----
	h.handleGameMessage(CTGameMessage{Client: clients[called], Message: CTMessage{
		Type: CTMsgGather, Payload: CTGatherPayload{Kind: CTGatherCardsKind}}})
	if game.Phase != CTPhaseKeepCard {
		t.Fatalf("keep_card 진입 실패: %s", game.Phase)
	}
	secret := game.Players[called].Draw
	if len(secret) != CTGatherDraw {
		t.Fatalf("뽑은 카드 = %d장", len(secret))
	}
	mineRaw := ctRawState(t, h, room, called)
	if !strings.Contains(mineRaw, `"yourDraw"`) {
		t.Fatalf("본인 스냅샷에 yourDraw 부재:\n%s", mineRaw)
	}
	for _, c := range secret {
		if !strings.Contains(mineRaw, fmt.Sprintf(`"id":%d`, c.ID)) {
			t.Fatalf("본인 스냅샷에 뽑은 카드(id %d)가 없다", c.ID)
		}
	}
	for _, viewer := range []int{other, -1} {
		raw := ctRawState(t, h, room, viewer)
		if strings.Contains(raw, `"yourDraw"`) {
			t.Fatalf("viewer %d 에 yourDraw 유출:\n%s", viewer, raw)
		}
		for _, c := range secret {
			if strings.Contains(raw, fmt.Sprintf(`"id":%d`, c.ID)) {
				t.Fatalf("viewer %d 에 비공개 카드(id %d) 유출:\n%s", viewer, c.ID, raw)
			}
		}
	}
	// 이벤트에도 카드 내용은 실리지 않는다
	for _, ev := range game.DrainEvents() {
		for _, c := range secret {
			if strings.Contains(ev.Message, c.Name) {
				t.Fatalf("이벤트에 뽑은 카드 이름 유출: %q", ev.Message)
			}
		}
	}

	// ---- 관전자 스냅샷은 패닉 없이 빌드되고 빈 슬라이스는 [] 다 ----
	spec := h.buildCTState(room, -1)
	if spec.YourSeat != -1 || spec.YourHand != nil || spec.YourRole != nil ||
		spec.YourDraw != nil || spec.PickPool != nil {
		t.Fatalf("관전자 스냅샷에 은닉 필드가 실렸다: %+v", spec)
	}
	for _, key := range []string{`"faceUpRemoved":`, `"players":[`, `"built":[`, `"crownSeat":`} {
		if !strings.Contains(ctRawState(t, h, room, -1), key) {
			t.Fatalf("관전자 raw 스냅샷에 %s 부재", key)
		}
	}
	empty := h.lobbyRoomFor("ZZZZ")
	raw, _ := json.Marshal(h.buildCTState(empty, -1))
	for _, key := range []string{`"players":[]`, `"faceUpRemoved":[]`,
		`"result":null`, `"lastAction":null`, `"round":0`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("빈 방 스냅샷에 %s 부재:\n%s", key, raw)
		}
	}
	for _, key := range []string{`"yourRole"`, `"yourHand"`, `"yourDraw"`, `"pickPool"`} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("시작 전 방에 %s 유출:\n%s", key, raw)
		}
	}
	json.Marshal(h.buildCTState(empty, 0)) // 좌석이 없는 방의 좌석 뷰 — 패닉 금지

	// ---- 차례가 아닌 좌석의 행동은 거부된다 ----
	for _, c := range clients {
		ctTakeMessages(t, c)
	}
	phaseBefore, seqBefore := game.Phase, game.StateSeq
	h.handleGameMessage(CTGameMessage{Client: clients[other], Message: CTMessage{
		Type: CTMsgKeep, Payload: CTKeepPayload{Index: 0}}})
	if game.Phase != phaseBefore || game.StateSeq != seqBefore {
		t.Fatal("차례가 아닌 좌석의 행동이 통과했다")
	}
	sawError := false
	for _, out := range ctTakeMessages(t, clients[other]) {
		if out.Type != CTMsgError {
			continue
		}
		text, _ := asPayloadMap(t, out.Payload)["message"].(string)
		if !hasHangul(text) {
			t.Fatalf("오류 문구가 한글이 아니다: %q", text)
		}
		sawError = true
	}
	if !sawError {
		t.Fatal("차례가 아닌 행동에 ct_error 가 없다")
	}
}

// TestCTRejections 와이어로 들어온 잘못된 요청이 한글 오류로 되돌아오고
// 판을 건드리지 않는지
func TestCTRejections(t *testing.T) {
	h, room, clients := ctBotFixture(t, 4, 777)
	game := room.Game

	// 직업 선택 단계에서의 거절
	picker := game.CurrentSeat
	badPick := []CTMessage{
		{Type: CTMsgPickRole, Payload: CTPickRolePayload{Role: 0}},     // 없는 번호
		{Type: CTMsgPickRole, Payload: CTPickRolePayload{Role: 99}},    // 범위 밖
		{Type: CTMsgGather, Payload: CTGatherPayload{Kind: "gold"}},    // 단계 위반
		{Type: CTMsgBuild, Payload: CTBuildPayload{CardID: 1}},         // 단계 위반
		{Type: CTMsgKeep, Payload: CTKeepPayload{Index: 0}},            // 단계 위반
		{Type: CTMsgAbility, Payload: CTAbilityPayload{TargetRole: 1}}, // 단계 위반
		{Type: CTMsgEndTurn}, // 단계 위반
	}
	ctExpectRejections(t, h, game, clients[picker], badPick, "선택")

	// 후보에 없는 직업
	absent := 0
	inPool := map[int]bool{}
	for _, r := range game.RolePool {
		inPool[r] = true
	}
	for r := 1; r <= CTRoleCount; r++ {
		if !inPool[r] {
			absent = r
			break
		}
	}
	if absent > 0 {
		ctExpectRejections(t, h, game, clients[picker], []CTMessage{
			{Type: CTMsgPickRole, Payload: CTPickRolePayload{Role: absent}}}, "제외된 직업")
	}

	// 선택을 끝내고 차례 단계로
	for game.Phase == CTPhasePickRoles {
		seat := game.CurrentSeat
		h.handleGameMessage(CTGameMessage{Client: clients[seat], Message: CTMessage{
			Type: CTMsgPickRole, Payload: CTPickRolePayload{Role: game.RolePool[0]}}})
	}
	seat := game.CurrentSeat
	badTurn := []CTMessage{
		{Type: CTMsgGather, Payload: CTGatherPayload{Kind: "보석"}},      // 알 수 없는 자원
		{Type: CTMsgGather, Payload: CTGatherPayload{}},                // 빈 종류
		{Type: CTMsgBuild, Payload: CTBuildPayload{CardID: 0}},         // 자원 전 건설
		{Type: CTMsgKeep, Payload: CTKeepPayload{Index: 0}},            // 고를 때가 아님
		{Type: CTMsgPickRole, Payload: CTPickRolePayload{Role: 1}},     // 선택 단계가 아님
		{Type: CTMsgAbility, Payload: CTAbilityPayload{TargetRole: 2}}, // 능력 단계가 아님
	}
	ctExpectRejections(t, h, game, clients[seat], badTurn, "차례")

	// 정상 요청은 통과한다
	before := game.StateSeq
	h.handleGameMessage(CTGameMessage{Client: clients[seat], Message: CTMessage{
		Type: CTMsgGather, Payload: CTGatherPayload{Kind: CTGatherGoldKind}}})
	if game.StateSeq == before {
		t.Fatal("정상 자원 요청이 반영되지 않았다")
	}
	if game.LastAction == nil || !hasHangul(game.LastAction.Message) {
		t.Fatalf("lastAction = %+v", game.LastAction)
	}
	// 손에 없는 카드 건설
	ctExpectRejections(t, h, game, clients[seat], []CTMessage{
		{Type: CTMsgBuild, Payload: CTBuildPayload{CardID: 99999}}}, "없는 카드")
}

// ctExpectRejections 잘못된 요청 묶음이 전부 한글 오류로 되돌아오고 판을
// 건드리지 않는지
func ctExpectRejections(t *testing.T, h *CTHub, game *CTGame, client *CTClient,
	msgs []CTMessage, label string) {
	t.Helper()
	for i, msg := range msgs {
		ctTakeMessages(t, client)
		seqBefore, phaseBefore, seatBefore := game.StateSeq, game.Phase, game.CurrentSeat
		h.handleGameMessage(CTGameMessage{Client: client, Message: msg})
		if game.StateSeq != seqBefore || game.Phase != phaseBefore || game.CurrentSeat != seatBefore {
			t.Fatalf("%s %d번째 잘못된 요청(%s)이 판을 바꿨다", label, i, msg.Type)
		}
		sawError := false
		for _, out := range ctTakeMessages(t, client) {
			if out.Type != CTMsgError {
				continue
			}
			text, _ := asPayloadMap(t, out.Payload)["message"].(string)
			if text == "" || !hasHangul(text) {
				t.Fatalf("%s %d번째 오류 문구가 한글이 아니다: %q", label, i, text)
			}
			sawError = true
		}
		if !sawError {
			t.Fatalf("%s %d번째 잘못된 요청(%s)에 ct_error 가 없다", label, i, msg.Type)
		}
	}
}

// TestCTAfkAutoProgress 접속만 유지한 채 아무도 행동하지 않는 판 —
// 네 단계의 마감이 모두 자동으로 해소되며 라운드가 넘어가는지.
// (마감만으로 끝까지 완주하는지는 순수 규칙 테스트 TestCTForceProgress 가 본다)
func TestCTAfkAutoProgress(t *testing.T) {
	_, url, cleanup := newCTTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	conns := make([]*ctTestClient, CTMinPlayers)
	for i := range conns {
		conns[i] = ctDial(t, url)
		defer conns[i].conn.Close()
		ctJoin(t, conns[i], fmt.Sprintf("잠수%d", i), "")
	}
	host := conns[0]
	host.send(t, CTMessage{Type: CTMsgStart})

	state := host.ctWaitPhase(t, string(CTPhasePickRoles))
	if ends := int64(state["endsAt"].(float64)); ends <= 0 {
		t.Fatalf("선택 스냅샷의 endsAt = %d, want unixMillis", ends)
	}
	if int(state["round"].(float64)) != 1 || state["lastRound"] != false {
		t.Fatalf("시작 스냅샷 = %v", state)
	}
	if int(state["callingRole"].(float64)) != 0 {
		t.Fatalf("선택 단계 callingRole = %v", state["callingRole"])
	}
	if crown := int(state["crownSeat"].(float64)); crown < 0 || crown >= CTMinPlayers {
		t.Fatalf("crownSeat = %v", state["crownSeat"])
	}
	faceUp, ok := state["faceUpRemoved"].([]interface{})
	if !ok || len(faceUp) != ctFaceUpCount(CTMinPlayers) {
		t.Fatalf("앞면 제외 = %v, want %d장", state["faceUpRemoved"], ctFaceUpCount(CTMinPlayers))
	}
	for _, pRaw := range state["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		if int(p["gold"].(float64)) != CTGoldStart {
			t.Fatalf("시작 금화 = %v", p["gold"])
		}
		if int(p["handCount"].(float64)) != CTHandStart {
			t.Fatalf("시작 손패 = %v", p["handCount"])
		}
		if built, ok := p["built"].([]interface{}); !ok || len(built) != 0 {
			t.Fatalf("시작 도시가 []가 아니다: %v", p["built"])
		}
		if int(p["roleRevealed"].(float64)) != 0 || p["killed"] != false || p["robbed"] != false {
			t.Fatalf("시작 좌석 상태 = %v", p)
		}
	}

	for _, c := range conns[1:] {
		ctDrainConn(c)
	}

	// 네 단계의 마감이 모두 발화하는지 (직업 선택 · 차례 · 카드 · 능력)
	sawRound2 := false
	sawAfk := 0
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) && (!sawRound2 || sawAfk < 4) {
		msg := host.waitMatch(t, "event-or-state", func(m CTMessage) bool {
			return m.Type == CTMsgEvent || m.Type == CTMsgGameState || m.Type == CTMsgGameOver
		})
		switch msg.Type {
		case CTMsgEvent:
			ev := ctPayloadMap(t, msg)
			if ev["kind"] == "afk" {
				text, _ := ev["message"].(string)
				if !strings.Contains(text, "자동") && !strings.Contains(text, "않아") {
					t.Fatalf("afk 문구 = %q", text)
				}
				if ev["name"] == nil || ev["name"] == "" {
					t.Fatalf("afk 이벤트에 name 부재: %v", ev)
				}
				sawAfk++
			}
		case CTMsgGameState:
			st := ctPayloadMap(t, msg)
			if int(st["round"].(float64)) >= 2 {
				sawRound2 = true
			}
		case CTMsgGameOver:
			sawRound2 = true
		}
	}
	if !sawRound2 {
		t.Fatal("마감 자동 진행만으로 2라운드에 닿지 못했다")
	}
	if sawAfk < 4 {
		t.Fatalf("afk 자동 진행 이벤트 = %d회 — 단계 마감이 걸리지 않는다", sawAfk)
	}
}

// TestCTRoomCodeAndSpectate 방 코드 발급·코드 입장·시작 후 코드 관전 진입.
// 관전자 스냅샷은 yourSeat -1 에 은닉 필드 전부 부재. 행동은 차단된다.
func TestCTRoomCodeAndSpectate(t *testing.T) {
	_, url, cleanup := newCTTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := ctDial(t, url)
	defer host.conn.Close()
	joined := ctJoin(t, host, "호스트", "NEW")
	code, _ := joined["roomCode"].(string)
	if len(code) != roomCodeLen {
		t.Fatalf("roomCode = %q, want %d자", code, roomCodeLen)
	}

	guests := make([]*ctTestClient, CTMinPlayers-1)
	for i := range guests {
		guests[i] = ctDial(t, url)
		defer guests[i].conn.Close()
		g := ctJoin(t, guests[i], fmt.Sprintf("친구%d", i), code)
		if g["roomCode"] != code || int(g["yourSeat"].(float64)) != i+1 {
			t.Fatalf("코드 입장 실패: %v", g)
		}
	}

	host.send(t, CTMessage{Type: CTMsgStart})
	state := host.ctWaitPhase(t, string(CTPhasePickRoles))
	if state["roomCode"] != code || len(state["players"].([]interface{})) != CTMinPlayers {
		t.Fatalf("시작 실패: %v", state["players"])
	}
	for _, c := range guests {
		ctDrainConn(c)
	}

	// 시작된 방의 코드로 들어오면 관전자
	spec := ctDial(t, url)
	defer spec.conn.Close()
	spec.send(t, CTMessage{Type: CTMsgJoinGame, Payload: CTJoinGamePayload{Name: "구경꾼", Room: code}})
	specJoined := ctPayloadMap(t, spec.waitFor(t, CTMsgSpectateJoined))
	if specJoined["roomCode"] != code {
		t.Fatalf("관전 입장 roomCode = %v", specJoined["roomCode"])
	}

	specState := ctPayloadMap(t, spec.waitFor(t, CTMsgGameState))
	if int(specState["yourSeat"].(float64)) != -1 {
		t.Fatalf("관전자 yourSeat = %v, want -1", specState["yourSeat"])
	}
	for _, key := range []string{"yourRole", "yourHand", "yourDraw", "pickPool"} {
		if leaked, ok := specState[key]; ok {
			t.Fatalf("관전자에게 %s 유출: %v", key, leaked)
		}
	}
	if specState["players"] == nil || specState["faceUpRemoved"] == nil {
		t.Fatalf("관전자에게 공개 정보가 없다: %v", specState)
	}

	// 관전자는 어떤 행동도 못 한다
	spec.send(t, CTMessage{Type: CTMsgPickRole, Payload: CTPickRolePayload{Role: 1}})
	errPayload := ctPayloadMap(t, spec.waitFor(t, CTMsgError))
	if errPayload["message"] != spectatorDeniedMsg {
		t.Fatalf("관전자 차단 문구 = %v", errPayload["message"])
	}
}

// TestCTReconnect 재접속 3종 — 이탈 통지(ct_player_disconnected) 후 세션으로
// 돌아오면 좌석·손패가 그대로 복원되고(ct_player_reconnected), 모르는 세션은
// ct_session_expired 로 거절된다.
func TestCTReconnect(t *testing.T) {
	_, url, cleanup := newCTTestServer(t, 3*time.Second)
	defer cleanup()

	conns := make([]*ctTestClient, CTMinPlayers)
	sessions := make([]string, CTMinPlayers)
	for i := range conns {
		conns[i] = ctDial(t, url)
		joined := ctJoin(t, conns[i], fmt.Sprintf("P%d", i), "")
		sessions[i], _ = joined["sessionId"].(string)
	}
	defer conns[0].conn.Close()
	conns[0].send(t, CTMessage{Type: CTMsgStart})
	conns[0].ctWaitPhase(t, string(CTPhasePickRoles))
	for _, c := range conns[2:] {
		ctDrainConn(c)
	}

	// 좌석 1 이탈 → 남은 사람에게 이탈 통지
	conns[1].conn.Close()
	discon := ctPayloadMap(t, conns[0].waitFor(t, CTMsgPlayerDisconnected))
	if int(discon["seat"].(float64)) != 1 || discon["name"] != "P1" {
		t.Fatalf("이탈 통지 = %v", discon)
	}
	if int(discon["graceSeconds"].(float64)) <= 0 {
		t.Fatalf("graceSeconds = %v", discon["graceSeconds"])
	}

	// 세션으로 재접속 → 좌석·손패 복원
	back := ctDial(t, url)
	defer back.conn.Close()
	back.send(t, CTMessage{Type: CTMsgRejoin, Payload: CTRejoinPayload{SessionID: sessions[1]}})
	recon := ctPayloadMap(t, back.waitFor(t, CTMsgPlayerReconnected))
	if int(recon["seat"].(float64)) != 1 {
		t.Fatalf("재접속 통지 = %v", recon)
	}
	restored := ctPayloadMap(t, back.waitFor(t, CTMsgGameState))
	if int(restored["yourSeat"].(float64)) != 1 {
		t.Fatalf("복원 yourSeat = %v", restored["yourSeat"])
	}
	if _, ok := restored["yourHand"].([]interface{}); !ok {
		t.Fatalf("복원 스냅샷에 yourHand 부재: %v", restored)
	}
	if _, ok := restored["yourRole"]; !ok {
		t.Fatalf("복원 스냅샷에 yourRole 부재: %v", restored)
	}

	// 모르는 세션은 만료 처리
	ghost := ctDial(t, url)
	defer ghost.conn.Close()
	ghost.send(t, CTMessage{Type: CTMsgRejoin, Payload: CTRejoinPayload{SessionID: "없는-세션"}})
	ghost.waitFor(t, CTMsgSessionExpired)
}

// TestCTBotTakeover 유예 만료 좌석을 봇이 이어받아 게임이 멈추지 않는지
func TestCTBotTakeover(t *testing.T) {
	_, url, cleanup := newCTTestServer(t, 120*time.Millisecond)
	defer cleanup()

	conns := make([]*ctTestClient, CTMinPlayers)
	for i := range conns {
		conns[i] = ctDial(t, url)
		defer conns[i].conn.Close()
		ctJoin(t, conns[i], fmt.Sprintf("P%d", i), "")
	}
	conns[0].send(t, CTMessage{Type: CTMsgStart})
	conns[0].ctWaitPhase(t, string(CTPhasePickRoles))
	for _, c := range conns[2:] {
		ctDrainConn(c)
	}

	// 좌석 1 이탈 → 유예 만료 → 봇 대체
	conns[1].conn.Close()
	sawTakeover := false
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		msg := conns[0].waitMatch(t, "event-or-state", func(m CTMessage) bool {
			return m.Type == CTMsgEvent || m.Type == CTMsgGameState || m.Type == CTMsgGameOver
		})
		if msg.Type == CTMsgEvent {
			ev := ctPayloadMap(t, msg)
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
			continue
		}
		// 봇이 이어받은 뒤에도 판이 계속 굴러간다
		if msg.Type == CTMsgGameOver {
			return
		}
		st := ctPayloadMap(t, msg)
		if int(st["round"].(float64)) >= 2 {
			return
		}
	}
	t.Fatal("봇 대체 후에도 판이 굴러가지 않았다")
}

// TestCTReactAndLobby 리액션은 좌석 보유자만·화이트리스트만, 대기 현황판은
// 사람이 대기할 때만 켜진다
func TestCTReactAndLobby(t *testing.T) {
	_, url, cleanup := newCTTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	conns := make([]*ctTestClient, CTMinPlayers)
	for i := range conns {
		conns[i] = ctDial(t, url)
		defer conns[i].conn.Close()
		ctJoin(t, conns[i], fmt.Sprintf("가%d", i), "")
	}

	conns[0].send(t, CTMessage{Type: CTMsgReact, Payload: CTReactPayload{Emoji: "🔥"}})
	ev := ctPayloadMap(t, conns[1].waitMatch(t, "react", func(m CTMessage) bool {
		if m.Type != CTMsgEvent {
			return false
		}
		p, ok := m.Payload.(map[string]interface{})
		return ok && p["kind"] == "react"
	}))
	if ev["message"] != "🔥" || ev["name"] != "가0" || int(ev["seat"].(float64)) != 0 {
		t.Fatalf("리액션 이벤트 = %v", ev)
	}

	// 화이트리스트 밖 이모지는 조용히 무시된다 — 다음에 오는 것은 시작 스냅샷이다
	conns[0].send(t, CTMessage{Type: CTMsgReact, Payload: CTReactPayload{Emoji: "💀"}})
	conns[0].send(t, CTMessage{Type: CTMsgStart})
	state := conns[0].ctWaitPhase(t, string(CTPhasePickRoles))
	if int(state["hostSeat"].(float64)) != 0 {
		t.Fatalf("hostSeat = %v", state["hostSeat"])
	}
}
