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

// 테스트에서는 대기 상태 마감을 짧게 낮춘다 (자동 뽑기·아뇨 창 통과·
// 무작위 호의·무작위 되꽂기)
func init() {
	ekTurnTimeout = 250 * time.Millisecond
	ekNopeTimeout = 50 * time.Millisecond
	ekFavorTimeout = 100 * time.Millisecond
	ekDefuseTimeout = 100 * time.Millisecond
}

// ekTestClient 공용 testConn 에 익스플로딩 키튼 메시지 타입의 waitFor 를 얹은 래퍼
type ekTestClient struct {
	testConn[EKMessage]
}

func newEKTestServer(t *testing.T, grace time.Duration) (*EKHub, string, func()) {
	t.Helper()
	hub := NewEKHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeEKWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func ekDial(t *testing.T, url string) *ekTestClient {
	t.Helper()
	return &ekTestClient{dialWS[EKMessage](t, url)}
}

func (c *ekTestClient) waitFor(t *testing.T, msgType EKMessageType) EKMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m EKMessage) bool { return m.Type == msgType })
}

func ekPayloadMap(t *testing.T, msg EKMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// ekJoin 입장하고 ek_player_joined payload 를 돌려준다
func ekJoin(t *testing.T, c *ekTestClient, name, room string) map[string]interface{} {
	t.Helper()
	c.send(t, EKMessage{Type: EKMsgJoinGame, Payload: EKJoinGamePayload{Name: name, Room: room}})
	return ekPayloadMap(t, c.waitFor(t, EKMsgPlayerJoined))
}

// ekWaitPhase 지정한 phase 의 스냅샷이 올 때까지 소비
func (c *ekTestClient) ekWaitPhase(t *testing.T, phase string) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "ek_game_state("+phase+")", func(m EKMessage) bool {
		if m.Type != EKMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		return ok && state["phase"] == phase
	})
	return ekPayloadMap(t, msg)
}

// ekTurnCounter 스냅샷의 (currentSeat, turnsLeft) 전이를 세어 소요 차례 수를
// 잰다. 한 차례 안에서는 두 값이 그대로라 전이 = 차례 종료다.
type ekTurnCounter struct {
	started bool
	seat    int
	turns   int
	count   int
}

func (tc *ekTurnCounter) observe(state map[string]interface{}) {
	seatF, ok := state["currentSeat"].(float64)
	if !ok {
		return
	}
	turnsF, _ := state["turnsLeft"].(float64)
	seat, turns := int(seatF), int(turnsF)
	if !tc.started {
		tc.started, tc.seat, tc.turns = true, seat, turns
		return
	}
	if seat != tc.seat || turns != tc.turns {
		tc.count++
		tc.seat, tc.turns = seat, turns
	}
}

// ekRunBotGame 4인 봇전 한 판을 완주시키고 (소요 차례 수, 승자 좌석)을 돌려준다.
// 좌석 0은 서버 연습봇 두뇌(ekBrain)를 WS 로 감싼 드라이버가 잡는다.
func ekRunBotGame(t *testing.T, url string, limit time.Duration) (int, int) {
	t.Helper()
	c := ekDial(t, url)
	defer c.conn.Close()
	ekJoin(t, c, "감독", "")
	c.send(t, EKMessage{Type: EKMsgFillBots}) // 4인까지 채우고 즉시 시작

	brain := newEKBrain()
	counter := &ekTurnCounter{}
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "state-or-over", func(m EKMessage) bool {
			return m.Type == EKMsgGameState || m.Type == EKMsgGameOver || m.Type == EKMsgFuture
		})
		if msg.Type == EKMsgGameOver {
			over := ekPayloadMap(t, msg)
			winner := int(over["winnerSeat"].(float64))
			if winner < 0 || winner >= EKFillBotTarget {
				t.Fatalf("winnerSeat = %d", winner)
			}
			if name, _ := over["winnerName"].(string); name == "" {
				t.Fatalf("winnerName 부재: %v", over)
			}
			players := over["players"].([]interface{})
			if len(players) != EKFillBotTarget {
				t.Fatalf("players 길이 = %d, want %d", len(players), EKFillBotTarget)
			}
			alive := 0
			for _, pRaw := range players {
				p := pRaw.(map[string]interface{})
				if p["alive"].(bool) {
					alive++
					if int(p["seat"].(float64)) != winner {
						t.Fatalf("승자가 아닌 좌석이 생존 상태다: %v", p)
					}
				}
			}
			if alive != 1 {
				t.Fatalf("종료 시 생존자 = %d명, want 1", alive)
			}
			return counter.count, winner
		}
		if msg.Type == EKMsgGameState {
			counter.observe(ekPayloadMap(t, msg))
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatalf("%v 안에 게임이 끝나지 않았다 — 진행 불가 상태", limit)
	return 0, -1
}

// TestEKFourBotsCompleteGame 봇을 채운 4인 게임이 60초 안에 완주하는지 —
// 가장 중요한 회귀 장치 (아뇨 창 교착·차례 미전환·종료 판정 감지).
// 폭탄이 인원-1 장이라 반드시 1명만 남는다.
func TestEKFourBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newEKTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	start := time.Now()
	turns, winner := ekRunBotGame(t, url, 60*time.Second)
	t.Logf("완주: winner=seat%d, %d차례, %.2fs", winner, turns, time.Since(start).Seconds())
	if turns <= 0 {
		t.Fatal("차례가 한 번도 진행되지 않았다")
	}
}

// TestEKBotQuality 4봇 30판의 완주율·평균 소요 차례 수 — 무한 아뇨 루프나
// 교착이 없는지를 숫자로 고정한다.
func TestEKBotQuality(t *testing.T) {
	if testing.Short() {
		t.Skip("short 모드에서는 건너뛴다")
	}
	_, url, cleanup := newEKTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	const games = 30
	total, minT, maxT := 0, 1<<30, 0
	wins := map[int]int{}
	start := time.Now()
	for i := 0; i < games; i++ {
		turns, winner := ekRunBotGame(t, url, 30*time.Second)
		total += turns
		wins[winner]++
		if turns < minT {
			minT = turns
		}
		if turns > maxT {
			maxT = turns
		}
	}
	avg := float64(total) / float64(games)
	t.Logf("4봇 %d판 완주(교착 0) — 평균 %.1f차례 (최소 %d / 최대 %d), 총 %.1fs, 좌석별 승수 %v",
		games, avg, minT, maxT, time.Since(start).Seconds(), wins)
	if avg < 5 {
		t.Fatalf("평균 차례 수 %.1f — 너무 짧다 (봇이 즉사만 하고 있다)", avg)
	}
	if avg > 200 {
		t.Fatalf("평균 차례 수 %.1f — 너무 길다 (진행이 막히고 있다)", avg)
	}
}

// ekHubRoom 허브 고루틴 없이 결정적으로 검증하기 위한 방 준비.
// 사람 취급 클라이언트 n명을 앉히고 게임을 시작한다.
func ekHubRoom(t *testing.T, n int) (*EKHub, *ekRoom, []*EKClient) {
	t.Helper()
	h := NewEKHub()
	room := h.lobbyRoomFor("")
	clients := make([]*EKClient, n)
	for i := range clients {
		c := &EKClient{wsClient: newBotWSClient(), Hub: h}
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
	return h, room, clients
}

// ekRig 손패·덱을 결정적으로 갈아 끼운다 (허브 고루틴 없는 테스트 전용)
func ekRig(g *EKGame, hands [][]EKCard, deck []EKCard) {
	for i, hand := range hands {
		g.Players[i].Hand = append([]EKCard{}, hand...)
	}
	g.Deck = append([]EKCard{}, deck...)
	g.Discard = []EKCard{}
	g.CurrentSeat = 0
	g.TurnsLeft = 1
	g.Phase = EKPhaseTurn
	g.StateSeq++
}

// ekDrain 클라이언트 Send 채널에 쌓인 메시지를 raw JSON 과 함께 꺼낸다
func ekDrain(t *testing.T, c *EKClient) []string {
	t.Helper()
	out := []string{}
	for {
		select {
		case data := <-c.Send:
			out = append(out, string(data))
		default:
			return out
		}
	}
}

// TestEKHiddenState 은닉의 핵심 계약 — 허브 고루틴 없이 핸들러를 직접 불러
// 결정적으로 검증한다.
//   - yourHand 는 본인 스냅샷에만. 타인·관전자의 raw JSON 에는 키 자체가 없다.
//   - 덱 내용과 폭탄 위치는 어떤 스냅샷에도 실리지 않는다 (deckLeft 만).
//   - ek_future 는 미래 예측를 쓴 사람에게만 간다.
func TestEKHiddenState(t *testing.T) {
	h, room, clients := ekHubRoom(t, 3)
	game := room.Game
	ekRig(game,
		[][]EKCard{
			{EKCardFuture, EKCardDefuse},
			{EKCardNope},
			{EKCardTaco},
		},
		[]EKCard{EKCardBomb, EKCardSkip, EKCardAttack, EKCardMelon})

	rawOf := func(viewer int) string {
		data, err := json.Marshal(h.buildEKState(room, viewer))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(data)
	}

	// ---- yourHand 는 본인 것만. 남의 손패는 어떤 시점에서도 보이지 않는다 ----
	mine := map[int]string{
		0: `"yourHand":[{"kind":"future"},{"kind":"defuse"}]`,
		1: `"yourHand":[{"kind":"nope"}]`,
		2: `"yourHand":[{"kind":"taco"}]`,
	}
	for viewer, want := range mine {
		if raw := rawOf(viewer); !strings.Contains(raw, want) {
			t.Fatalf("viewer %d 본인 손패 부재:\n%s", viewer, raw)
		}
	}
	// 관전자(-1)에게는 키 자체가 없다
	if raw := rawOf(-1); strings.Contains(raw, "yourHand") {
		t.Fatalf("관전자 스냅샷에 yourHand 유출:\n%s", raw)
	}
	// 남의 카드 종류는 새어 나가지 않는다 (손패 수만 공개)
	leak := map[int][]string{
		0:  {"nope", "taco"},
		1:  {"future", "defuse", "taco"},
		2:  {"future", "defuse", "nope"},
		-1: {"future", "defuse", "nope", "taco"},
	}
	for viewer, kinds := range leak {
		raw := rawOf(viewer)
		for _, k := range kinds {
			if strings.Contains(raw, `"kind":"`+k+`"`) {
				t.Fatalf("viewer %d 스냅샷에 남의 카드 %q 유출:\n%s", viewer, k, raw)
			}
		}
	}
	// 본인이 빈손이면 null 이 아니라 []
	game.Players[1].Hand = []EKCard{}
	if raw := rawOf(1); !strings.Contains(raw, `"yourHand":[]`) {
		t.Fatalf("빈 손패가 []가 아니다:\n%s", raw)
	}
	game.Players[1].Hand = []EKCard{EKCardNope}

	// ---- 덱 내용·폭탄 위치는 어디에도 없다 ----
	for _, viewer := range []int{0, 1, 2, -1} {
		raw := rawOf(viewer)
		if strings.Contains(raw, "bomb") {
			t.Fatalf("viewer %d 스냅샷에 폭탄 정보 유출:\n%s", viewer, raw)
		}
		if strings.Contains(raw, `"deck":`) || strings.Contains(raw, `"discard":`) {
			t.Fatalf("viewer %d 스냅샷에 덱/버린 더미 내용 유출:\n%s", viewer, raw)
		}
		if !strings.Contains(raw, `"deckLeft":4`) {
			t.Fatalf("viewer %d 스냅샷의 deckLeft 이상:\n%s", viewer, raw)
		}
	}
	// 계약된 키 집합 그대로인지 (관전자는 yourHand 만 빠진다)
	var specMap map[string]interface{}
	if err := json.Unmarshal([]byte(rawOf(-1)), &specMap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []string{"gameId", "roomCode", "phase", "hostSeat", "yourSeat", "spectators",
		"endsAt", "currentSeat", "turnsLeft", "deckLeft", "discardTop", "pending",
		"players", "lastAction", "result"}
	if len(specMap) != len(want) {
		t.Fatalf("관전자 스냅샷 키 = %d개 %v, want %d개", len(specMap), specMap, len(want))
	}
	for _, k := range want {
		if _, ok := specMap[k]; !ok {
			t.Fatalf("관전자 스냅샷에 %q 부재", k)
		}
	}
	spec := h.buildEKState(room, -1) // 관전자 시점에서 패닉하지 않는다
	if spec.YourSeat != -1 || spec.YourHand != nil || len(spec.Players) != 3 {
		t.Fatalf("관전자 스냅샷: yourSeat=%d hand=%v", spec.YourSeat, spec.YourHand)
	}

	// ---- ek_future 는 그 사람에게만 ----
	for _, c := range clients {
		ekDrain(t, c)
	}
	h.handleGameMessage(EKGameMessage{Client: clients[0], Message: EKMessage{
		Type: EKMsgPlay, Payload: EKPlayPayload{Index: 0}}})
	if game.Phase != EKPhaseNopeWindow {
		t.Fatalf("미래 예측를 냈는데 phase=%s", game.Phase)
	}
	h.handleGameMessage(EKGameMessage{Client: clients[1], Message: EKMessage{Type: EKMsgPass}})
	h.handleGameMessage(EKGameMessage{Client: clients[2], Message: EKMessage{Type: EKMsgPass}})
	if game.Phase != EKPhaseTurn || game.CurrentSeat != 0 {
		t.Fatalf("미래 예측 후 phase=%s current=%d", game.Phase, game.CurrentSeat)
	}

	got0 := strings.Join(ekDrain(t, clients[0]), "\n")
	if !strings.Contains(got0, `"type":"ek_future"`) {
		t.Fatalf("미래 예측 결과가 본인에게 오지 않았다:\n%s", got0)
	}
	if !strings.Contains(got0, `"cards":["bomb","skip","attack"]`) {
		t.Fatalf("미래 예측 결과 내용 이상:\n%s", got0)
	}
	for i := 1; i < 3; i++ {
		got := strings.Join(ekDrain(t, clients[i]), "\n")
		if strings.Contains(got, "ek_future") || strings.Contains(got, "bomb") {
			t.Fatalf("seat%d 에게 미래 예측 결과가 샜다:\n%s", i, got)
		}
	}
}

// TestEKNopeWindowStateMachine 아뇨 창 상태기계 — 쿠(cp_hub)와 같은
// StateSeq/DeadlineSeq/AfkSeq 관리. 통과가 쌓이는 동안에는 마감이 늘어나지
// 않고, 아뇨가 겹치면 창이 새로 열려 마감이 다시 걸린다.
func TestEKNopeWindowStateMachine(t *testing.T) {
	h, room, clients := ekHubRoom(t, 3)
	game := room.Game
	ekRig(game,
		[][]EKCard{
			{EKCardSkip},
			{EKCardNope, EKCardNope},
			{EKCardNope},
		},
		[]EKCard{EKCardTaco, EKCardMelon})
	h.afterProgress(room)

	play := func(c *EKClient, m EKMessage) { h.handleGameMessage(EKGameMessage{Client: c, Message: m}) }

	play(clients[0], EKMessage{Type: EKMsgPlay, Payload: EKPlayPayload{Index: 0}})
	if game.Phase != EKPhaseNopeWindow || room.DeadlineSeq != game.StateSeq {
		t.Fatalf("창 개방 phase=%s DeadlineSeq=%d StateSeq=%d",
			game.Phase, room.DeadlineSeq, game.StateSeq)
	}
	if game.Deadline <= 0 {
		t.Fatal("아뇨 창 스냅샷의 endsAt(Deadline) 부재")
	}
	afk1, seq1, ends1 := game.AfkSeq, game.StateSeq, game.Deadline

	// 통과 한 명 — 같은 창이므로 마감이 늘어나지 않는다
	play(clients[1], EKMessage{Type: EKMsgPass})
	if game.Phase != EKPhaseNopeWindow {
		t.Fatalf("한 명 통과만으로 창이 닫혔다: %s", game.Phase)
	}
	if game.AfkSeq != afk1 || game.StateSeq != seq1 || game.Deadline != ends1 {
		t.Fatalf("통과가 마감을 갱신했다: afk=%d seq=%d ends=%d", game.AfkSeq, game.StateSeq, game.Deadline)
	}

	// 아뇨 겹치기 — 창이 새로 열려 마감이 다시 걸린다
	play(clients[2], EKMessage{Type: EKMsgNope})
	if game.Pending == nil || game.Pending.NopeCount != 1 {
		t.Fatalf("아뇨 반영 실패: %+v", game.Pending)
	}
	if game.StateSeq <= seq1 || game.AfkSeq <= afk1 || room.DeadlineSeq != game.StateSeq {
		t.Fatalf("아뇨 후 창 재개방 실패: seq=%d afk=%d deadlineSeq=%d",
			game.StateSeq, game.AfkSeq, room.DeadlineSeq)
	}
	// 앞 창의 통과 기록은 초기화된다 — seat1 이 다시 응답해야 한다
	play(clients[0], EKMessage{Type: EKMsgPass}) // 시전자도 이제 응답자다
	if game.Phase != EKPhaseNopeWindow {
		t.Fatalf("응답이 남았는데 창이 닫혔다: %s", game.Phase)
	}
	play(clients[1], EKMessage{Type: EKMsgNope}) // 2겹 — 다시 유효
	if game.Pending.NopeCount != 2 {
		t.Fatalf("2겹 실패: %+v", game.Pending)
	}

	// 마지막 아뇨 좌석(seat1)을 뺀 전원 통과 → 짝수 겹이므로 건너뛰기 발동
	play(clients[0], EKMessage{Type: EKMsgPass})
	play(clients[2], EKMessage{Type: EKMsgPass})
	if game.Phase != EKPhaseTurn || game.CurrentSeat != 1 {
		t.Fatalf("2겹 판정 실패: phase=%s current=%d, want turn/1", game.Phase, game.CurrentSeat)
	}
	if game.Pending != nil {
		t.Fatalf("판정 후 pending 잔존: %+v", game.Pending)
	}
	// 창이 닫힌 뒤의 뒤늦은 아뇨·통과는 조용히 무시된다 (에러가 아니다)
	ekDrain(t, clients[0])
	h.handleGameMessage(EKGameMessage{Client: clients[0], Message: EKMessage{Type: EKMsgNope}})
	h.handleGameMessage(EKGameMessage{Client: clients[2], Message: EKMessage{Type: EKMsgPass}})
	if got := strings.Join(ekDrain(t, clients[0]), "\n"); strings.Contains(got, "ek_error") {
		t.Fatalf("뒤늦은 아뇨가 에러를 냈다:\n%s", got)
	}
	if game.Phase != EKPhaseTurn || game.CurrentSeat != 1 {
		t.Fatalf("뒤늦은 응답이 상태를 흔들었다: phase=%s current=%d", game.Phase, game.CurrentSeat)
	}
}

// TestEKFavorPendingVisible favor_wait 동안 pending 이 유지돼야 프론트가
// pending.targetSeat === yourSeat 로 "줄 사람"을 판별할 수 있다.
// defuse_place 도 같은 방식으로 pending.bySeat 을 남긴다.
func TestEKFavorPendingVisible(t *testing.T) {
	h, room, clients := ekHubRoom(t, 3)
	game := room.Game
	ekRig(game,
		[][]EKCard{{EKCardFavor}, {EKCardTaco, EKCardMelon}, {}},
		[]EKCard{EKCardBomb, EKCardBeard})
	h.afterProgress(room)

	send := func(c *EKClient, m EKMessage) { h.handleGameMessage(EKGameMessage{Client: c, Message: m}) }
	seat1 := 1
	send(clients[0], EKMessage{Type: EKMsgPlay,
		Payload: EKPlayPayload{Index: 0, TargetSeat: &seat1}})
	send(clients[1], EKMessage{Type: EKMsgPass})
	send(clients[2], EKMessage{Type: EKMsgPass})

	if game.Phase != EKPhaseFavorWait {
		t.Fatalf("phase = %s, want favor_wait", game.Phase)
	}
	for _, viewer := range []int{0, 1, 2, -1} {
		st := h.buildEKState(room, viewer)
		if st.Pending == nil {
			t.Fatalf("viewer %d: favor_wait 인데 pending 이 null 이다", viewer)
		}
		if st.Pending.Kind != string(EKCardFavor) || st.Pending.BySeat != 0 || st.Pending.TargetSeat != 1 {
			t.Fatalf("viewer %d: pending = %+v", viewer, st.Pending)
		}
		if st.EndsAt <= 0 {
			t.Fatalf("viewer %d: favor_wait 의 endsAt 부재", viewer)
		}
	}

	send(clients[1], EKMessage{Type: EKMsgGive, Payload: EKGivePayload{Index: 0}})
	if game.Phase != EKPhaseTurn || len(game.Players[0].Hand) != 1 {
		t.Fatalf("건네기 후 phase=%s hand=%v", game.Phase, game.Players[0].Hand)
	}
	if h.buildEKState(room, 0).Pending != nil {
		t.Fatal("건네기 후 pending 이 남았다")
	}

	// 되꽂기 대기도 pending 으로 알린다 (kind=defuse, bySeat=막은 사람)
	game.Players[0].Hand = []EKCard{EKCardDefuse}
	send(clients[0], EKMessage{Type: EKMsgDraw})
	if game.Phase != EKPhaseDefusePlace {
		t.Fatalf("phase = %s, want defuse_place", game.Phase)
	}
	st := h.buildEKState(room, -1)
	if st.Pending == nil || st.Pending.Kind != string(EKCardDefuse) || st.Pending.BySeat != 0 {
		t.Fatalf("되꽂기 pending = %+v", st.Pending)
	}
	send(clients[0], EKMessage{Type: EKMsgDefusePlace, Payload: EKDefusePlacePayload{Position: 1}})
	if game.Phase != EKPhaseTurn || game.CurrentSeat != 1 {
		t.Fatalf("되꽂기 후 phase=%s current=%d", game.Phase, game.CurrentSeat)
	}
	// 되꽂기 위치는 어떤 스냅샷·이벤트에도 없다
	for _, c := range clients {
		if got := strings.Join(ekDrain(t, c), "\n"); strings.Contains(got, "bomb") {
			t.Fatalf("되꽂기 이후 메시지에 폭탄 정보 유출:\n%s", got)
		}
	}
}

// TestEKEliminatedBecomesSpectator 탈락자는 방을 나가지 않고 관전으로
// 전환된다 — 좌석·연결은 유지되고 alive=false 로만 남는다.
func TestEKEliminatedBecomesSpectator(t *testing.T) {
	h, room, clients := ekHubRoom(t, 3)
	game := room.Game
	ekRig(game,
		[][]EKCard{{EKCardTaco}, {EKCardMelon}, {EKCardBeard}},
		[]EKCard{EKCardBomb, EKCardSkip, EKCardAttack})
	h.afterProgress(room)

	h.handleGameMessage(EKGameMessage{Client: clients[0], Message: EKMessage{Type: EKMsgDraw}})

	if room.Clients[0] != clients[0] {
		t.Fatal("탈락자가 방에서 빠졌다")
	}
	if room.Spectators[clients[0]] {
		t.Fatal("탈락자가 순수 관전자 목록에 들어갔다")
	}
	st := h.buildEKState(room, 0)
	if len(st.Players) != 3 || st.Players[0].Alive {
		t.Fatalf("탈락자 좌석 뷰 = %+v", st.Players)
	}
	if st.YourHand == nil || len(*st.YourHand) != 0 {
		t.Fatalf("탈락자 손패 = %v, want []", st.YourHand)
	}
	if st.CurrentSeat != 1 {
		t.Fatalf("탈락 후 current = %d", st.CurrentSeat)
	}

	// 탈락자의 행동은 규칙이 거절한다 (관전자 차단 문구가 아니라 게임 에러)
	ekDrain(t, clients[0])
	h.handleGameMessage(EKGameMessage{Client: clients[0], Message: EKMessage{Type: EKMsgDraw}})
	got := strings.Join(ekDrain(t, clients[0]), "\n")
	if !strings.Contains(got, "ek_error") || strings.Contains(got, spectatorDeniedMsg) {
		t.Fatalf("탈락자 행동 응답 = %s", got)
	}
	// 리액션은 여전히 가능하다 (좌석 보유자)
	h.handleGameMessage(EKGameMessage{Client: clients[0], Message: EKMessage{
		Type: EKMsgReact, Payload: EKReactPayload{Emoji: "😭"}}})
	if got := strings.Join(ekDrain(t, clients[1]), "\n"); !strings.Contains(got, "😭") {
		t.Fatalf("탈락자 리액션이 전달되지 않았다:\n%s", got)
	}
}

// TestEKRoomCodeAndSpectate 방 코드 발급·코드 입장·시작 후 코드 관전 진입.
// 관전자 스냅샷은 yourSeat -1 에 yourHand 부재. 행동은 전부 차단된다.
func TestEKRoomCodeAndSpectate(t *testing.T) {
	_, url, cleanup := newEKTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := ekDial(t, url)
	defer host.conn.Close()
	joined := ekJoin(t, host, "호스트", "NEW")
	code, _ := joined["roomCode"].(string)
	if len(code) != roomCodeLen {
		t.Fatalf("roomCode = %q, want %d자", code, roomCodeLen)
	}

	guest := ekDial(t, url)
	defer guest.conn.Close()
	guestJoined := ekJoin(t, guest, "친구", code)
	if guestJoined["roomCode"] != code || int(guestJoined["yourSeat"].(float64)) != 1 {
		t.Fatalf("코드 입장 실패: %v", guestJoined)
	}

	host.send(t, EKMessage{Type: EKMsgStart})
	state := host.ekWaitPhase(t, string(EKPhaseTurn))
	if state["roomCode"] != code || len(state["players"].([]interface{})) != 2 {
		t.Fatalf("시작 실패: %v", state["players"])
	}
	hand, ok := state["yourHand"].([]interface{})
	if !ok || len(hand) != EKStartHand+1 {
		t.Fatalf("본인 손패 = %v", state["yourHand"])
	}
	if int(state["deckLeft"].(float64)) != 51-7*2 {
		t.Fatalf("deckLeft = %v", state["deckLeft"])
	}
	if state["discardTop"] != "" || state["pending"] != nil {
		t.Fatalf("시작 스냅샷 discardTop=%v pending=%v", state["discardTop"], state["pending"])
	}

	// 시작된 방의 코드로 들어오면 관전자
	spec := ekDial(t, url)
	defer spec.conn.Close()
	spec.send(t, EKMessage{Type: EKMsgJoinGame,
		Payload: EKJoinGamePayload{Name: "구경꾼", Room: code}})
	specJoined := ekPayloadMap(t, spec.waitFor(t, EKMsgSpectateJoined))
	if specJoined["roomCode"] != code {
		t.Fatalf("관전 입장 roomCode = %v", specJoined["roomCode"])
	}

	specState := ekPayloadMap(t, spec.waitFor(t, EKMsgGameState))
	if int(specState["yourSeat"].(float64)) != -1 {
		t.Fatalf("관전자 yourSeat = %v, want -1", specState["yourSeat"])
	}
	if leaked, ok := specState["yourHand"]; ok {
		t.Fatalf("관전자에게 손패 유출: %v", leaked)
	}
	if int(specState["spectators"].(float64)) != 1 {
		t.Fatalf("관전자 수 = %v", specState["spectators"])
	}
	for _, pRaw := range specState["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		if int(p["handCount"].(float64)) != EKStartHand+1 || !p["alive"].(bool) {
			t.Fatalf("관전자 스냅샷 좌석 정보 이상: %v", p)
		}
	}

	// 관전자는 어떤 행동도 못 한다
	spec.send(t, EKMessage{Type: EKMsgDraw})
	errPayload := ekPayloadMap(t, spec.waitFor(t, EKMsgError))
	if errPayload["message"] != spectatorDeniedMsg {
		t.Fatalf("관전자 차단 문구 = %v", errPayload["message"])
	}
}

// TestEKAfkAutoProgress 접속만 유지한 채 아무것도 하지 않는 2인전 —
// 자동 뽑기·자동 되꽂기만으로 끝까지 완주하는지 (endsAt 노출·afk 이벤트 포함)
func TestEKAfkAutoProgress(t *testing.T) {
	_, url, cleanup := newEKTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := ekDial(t, url)
	defer host.conn.Close()
	ekJoin(t, host, "잠수1", "")

	guest := ekDial(t, url)
	defer guest.conn.Close()
	ekJoin(t, guest, "잠수2", "")

	host.send(t, EKMessage{Type: EKMsgStart})
	state := host.ekWaitPhase(t, string(EKPhaseTurn))
	if ends := int64(state["endsAt"].(float64)); ends <= 0 {
		t.Fatalf("turn 스냅샷의 endsAt = %d, want unixMillis", ends)
	}

	// guest 는 더 읽지 않는다 — 백그라운드로 비워 버퍼 포화만 막는다
	go func() {
		for {
			if _, _, err := guest.conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	sawAfk := false
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		msg := host.waitMatch(t, "event-or-over", func(m EKMessage) bool {
			return m.Type == EKMsgEvent || m.Type == EKMsgGameOver
		})
		if msg.Type == EKMsgEvent {
			ev := ekPayloadMap(t, msg)
			if ev["kind"] == "afk" {
				if !strings.Contains(ev["message"].(string), "자동") {
					t.Fatalf("afk 문구 = %v", ev["message"])
				}
				sawAfk = true
			}
			continue
		}
		over := ekPayloadMap(t, msg)
		if int(over["winnerSeat"].(float64)) < 0 {
			t.Fatalf("winnerSeat = %v", over["winnerSeat"])
		}
		if !sawAfk {
			t.Fatal("afk 자동 진행 이벤트가 한 번도 없었다")
		}
		return
	}
	t.Fatal("전원 방치 게임이 60초 안에 끝나지 않았다")
}

// TestEKReconnect 재접속 3종 — 끊김 알림, 세션 복원(손패 그대로), 만료 세션 거절
func TestEKReconnect(t *testing.T) {
	_, url, cleanup := newEKTestServer(t, 10*time.Second)
	defer cleanup()

	host := ekDial(t, url)
	defer host.conn.Close()
	ekJoin(t, host, "호스트", "")

	guest := ekDial(t, url)
	guestJoined := ekJoin(t, guest, "친구", "")
	sessionID, _ := guestJoined["sessionId"].(string)
	if sessionID == "" {
		t.Fatal("sessionId 부재")
	}

	host.send(t, EKMessage{Type: EKMsgStart})
	host.ekWaitPhase(t, string(EKPhaseTurn))
	before := guest.ekWaitPhase(t, string(EKPhaseTurn))
	beforeHand := len(before["yourHand"].([]interface{}))

	// ① 끊김 알림
	guest.conn.Close()
	dis := ekPayloadMap(t, host.waitFor(t, EKMsgPlayerDisconnected))
	if int(dis["seat"].(float64)) != 1 || int(dis["graceSeconds"].(float64)) != 10 {
		t.Fatalf("끊김 알림 = %v", dis)
	}

	// ② 세션 복원 — 손패가 그대로 돌아온다
	back := ekDial(t, url)
	defer back.conn.Close()
	back.send(t, EKMessage{Type: EKMsgRejoin, Payload: EKRejoinPayload{SessionID: sessionID}})
	re := ekPayloadMap(t, host.waitFor(t, EKMsgPlayerReconnected))
	if int(re["seat"].(float64)) != 1 || re["name"] != "친구" {
		t.Fatalf("재접속 알림 = %v", re)
	}
	restored := ekPayloadMap(t, back.waitFor(t, EKMsgGameState))
	if int(restored["yourSeat"].(float64)) != 1 {
		t.Fatalf("복원 yourSeat = %v", restored["yourSeat"])
	}
	hand, ok := restored["yourHand"].([]interface{})
	if !ok || len(hand) == 0 || len(hand) > beforeHand {
		t.Fatalf("복원 손패 = %v (끊기기 전 %d장)", restored["yourHand"], beforeHand)
	}

	// ③ 만료(없는) 세션 거절
	ghost := ekDial(t, url)
	defer ghost.conn.Close()
	ghost.send(t, EKMessage{Type: EKMsgRejoin, Payload: EKRejoinPayload{SessionID: "없는-세션"}})
	ghost.waitFor(t, EKMsgSessionExpired)
}

// TestEKBotTakeover 유예 만료 좌석은 연습봇이 이어받고 게임은 계속된다
func TestEKBotTakeover(t *testing.T) {
	_, url, cleanup := newEKTestServer(t, 80*time.Millisecond)
	defer cleanup()

	host := ekDial(t, url)
	defer host.conn.Close()
	ekJoin(t, host, "감독", "")

	guest := ekDial(t, url)
	ekJoin(t, guest, "이탈자", "")

	host.send(t, EKMessage{Type: EKMsgStart})
	host.ekWaitPhase(t, string(EKPhaseTurn))

	guest.conn.Close()

	ev := ekPayloadMap(t, host.waitMatch(t, "bot_takeover", func(m EKMessage) bool {
		if m.Type != EKMsgEvent {
			return false
		}
		p, ok := m.Payload.(map[string]interface{})
		return ok && p["kind"] == "bot_takeover"
	}))
	if ev["name"] != "이탈자" || int(ev["seat"].(float64)) != 1 {
		t.Fatalf("봇 대체 이벤트 = %v", ev)
	}

	// 봇이 이어받았으므로 게임은 계속 진행돼 끝난다
	brain := newEKBrain()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		msg := host.waitMatch(t, "state-or-over", func(m EKMessage) bool {
			return m.Type == EKMsgGameState || m.Type == EKMsgGameOver || m.Type == EKMsgFuture
		})
		if msg.Type == EKMsgGameOver {
			over := ekPayloadMap(t, msg)
			if int(over["winnerSeat"].(float64)) < 0 {
				t.Fatalf("winnerSeat = %v", over["winnerSeat"])
			}
			return
		}
		if reply := brain.decide(msg); reply != nil {
			host.send(t, *reply)
		}
	}
	t.Fatal("봇 대체 후 게임이 끝나지 않았다")
}
