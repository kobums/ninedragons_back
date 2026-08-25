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
// (카드 45초·임무 정산 5초는 실사용 값). 값은 init 에서 한 번만 정한다 —
// 테스트 도중에 바꾸면 허브 고루틴과 경합한다(-race).
func init() {
	cwPlayTimeout = 80 * time.Millisecond
	cwRoundEndDelay = 20 * time.Millisecond

	cwBotCommDelay = 2 * time.Millisecond
	cwBotCommJitterMs = 3
	cwBotPlayDelay = 2 * time.Millisecond
	cwBotPlayJitterMs = 3
}

// cwTestClient 공용 testConn 에 더 크루 메시지 타입의 waitFor 를 얹은 래퍼
type cwTestClient struct {
	testConn[CWMessage]
}

func newCWTestServer(t *testing.T, grace time.Duration) (*CWHub, string, func()) {
	t.Helper()
	hub := NewCWHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeCWWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func cwDial(t *testing.T, url string) *cwTestClient {
	t.Helper()
	return &cwTestClient{dialWS[CWMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *cwTestClient) waitFor(t *testing.T, msgType CWMessageType) CWMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m CWMessage) bool { return m.Type == msgType })
}

func cwPayloadMap(t *testing.T, msg CWMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// cwJoin 입장하고 cw_player_joined payload 를 돌려준다
func cwJoin(t *testing.T, c *cwTestClient, name, room string) map[string]interface{} {
	t.Helper()
	c.send(t, CWMessage{Type: CWMsgJoinGame, Payload: CWJoinGamePayload{Name: name, Room: room}})
	return cwPayloadMap(t, c.waitFor(t, CWMsgPlayerJoined))
}

// cwWaitPhase 지정한 phase 의 스냅샷이 올 때까지 소비
func (c *cwTestClient) cwWaitPhase(t *testing.T, phase string) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "cw_game_state("+phase+")", func(m CWMessage) bool {
		if m.Type != CWMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		return ok && state["phase"] == phase
	})
	return cwPayloadMap(t, msg)
}

// cwDrain 배경으로 소켓을 비운다 (읽지 않는 연결의 버퍼 포화 방지)
func cwDrain(c *cwTestClient) {
	conn := c.conn
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

// cwSeatClients 허브 고루틴 없이 핸들러를 직접 부르는 결정적 테스트용 —
// 소켓 없는 사람 좌석 n개를 앉힌 방을 만든다
func cwSeatClients(t *testing.T, h *CWHub, room *cwRoom, n int) []*CWClient {
	t.Helper()
	clients := make([]*CWClient, n)
	for i := range clients {
		c := &CWClient{wsClient: newBotWSClient(), Hub: h}
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

// TestCWFourBotsCompleteGame 봇을 채운 4인 협력전이 60초 안에 완주하는지 —
// 가장 중요한 회귀 장치. 라운드는 성공이든 실패든 반드시 끝나야 한다
// (카드 소진 = out_of_cards 실패 판정). 좌석 0은 서버 연습봇 두뇌(cwBrain)를
// WS 로 감싼 드라이버가 잡는다.
func TestCWFourBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newCWTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := cwDial(t, url)
	defer c.conn.Close()
	cwJoin(t, c, "기장", "")
	c.send(t, CWMessage{Type: CWMsgFillBots}) // 4인까지 채우고 즉시 시작

	start := time.Now()
	brain := newCWBrain()
	deadline := start.Add(60 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "state-or-over", func(m CWMessage) bool {
			return m.Type == CWMsgGameState || m.Type == CWMsgGameOver
		})
		if msg.Type == CWMsgGameOver {
			over := cwPayloadMap(t, msg)
			cleared, ok := over["cleared"].(bool)
			if !ok {
				t.Fatalf("종료 payload 에 cleared 부재: %v", over)
			}
			reason, _ := over["failedReason"].(string)
			if cleared && reason != "" {
				t.Fatalf("클리어인데 failedReason = %q", reason)
			}
			if !cleared && reason != "wrong_winner" && reason != "out_of_cards" {
				t.Fatalf("failedReason = %q", reason)
			}
			if m, _ := over["message"].(string); m == "" {
				t.Fatalf("종료 문구 부재: %v", over)
			}
			mission := int(over["mission"].(float64))
			if mission < 1 || mission > CWDefaultMaxMission {
				t.Fatalf("mission = %d", mission)
			}
			if int(over["maxMission"].(float64)) != CWDefaultMaxMission {
				t.Fatalf("maxMission = %v", over["maxMission"])
			}
			tasks := over["tasks"].([]interface{})
			if len(tasks) != mission {
				t.Fatalf("종료 임무 수 = %d, want %d", len(tasks), mission)
			}
			doneCount := 0
			for _, raw := range tasks {
				task := raw.(map[string]interface{})
				if task["suit"] == string(CWSuitRocket) {
					t.Fatalf("임무에 로켓: %v", task)
				}
				if _, ok := task["seat"].(float64); !ok {
					t.Fatalf("임무에 담당 좌석 부재: %v", task)
				}
				if task["done"] == true {
					doneCount++
				}
			}
			if cleared && doneCount != len(tasks) {
				t.Fatalf("클리어인데 미완료 임무가 있다: %v", tasks)
			}
			players := over["players"].([]interface{})
			if len(players) != CWFillBotTarget {
				t.Fatalf("players 길이 = %d, want %d", len(players), CWFillBotTarget)
			}
			t.Logf("완주: cleared=%t reason=%s mission=%d (%.1fs)",
				cleared, reason, mission, time.Since(start).Seconds())
			return
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("60초 안에 게임이 끝나지 않았다 — 진행 불가 상태")
}

// TestCWHiddenState 은닉의 핵심 계약 — 허브 고루틴 없이 핸들러를 직접 불러
// 결정적으로 검증한다. yourHand 는 본인 스냅샷에만 실리고, 타인·관전자의
// raw JSON 에는 키 자체가 없어야 한다. tasks 와 players[].revealed 는
// 전원 공개다.
func TestCWHiddenState(t *testing.T) {
	h := NewCWHub()
	room := h.lobbyRoomFor("")
	clients := cwSeatClients(t, h, room, 4)
	h.startGame(room)

	game := room.Game
	// 결정적 구도: 임무는 파랑 5(담당 seat1) 하나, 손패는 좌석마다 2장씩
	game.Tasks = []CWTask{{Suit: CWSuitBlue, Rank: 5, Seat: 1}}
	cwSetHands(t, game, 0, [][]CWCard{
		{{Suit: CWSuitBlue, Rank: 5}, {Suit: CWSuitPink, Rank: 4}},
		{{Suit: CWSuitBlue, Rank: 9}, {Suit: CWSuitPink, Rank: 2}},
		{{Suit: CWSuitBlue, Rank: 1}, {Suit: CWSuitPink, Rank: 3}},
		{{Suit: CWSuitBlue, Rank: 2}, {Suit: CWSuitPink, Rank: 7}},
	})

	rawOf := func(viewer int) string {
		data, err := json.Marshal(h.buildCWState(room, viewer))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(data)
	}

	// ---- yourHand: 본인만 (관전자는 키 자체 부재) ----
	raw0 := rawOf(0)
	if !strings.Contains(raw0, `"yourHand":[{"suit":"blue","rank":5}`) {
		t.Fatalf("본인 손패가 스냅샷에 없다:\n%s", raw0)
	}
	if strings.Count(raw0, `"yourHand"`) != 1 {
		t.Fatalf("yourHand 키가 본인 것 하나가 아니다:\n%s", raw0)
	}
	rawSpec := rawOf(-1)
	if strings.Contains(rawSpec, `"yourHand"`) {
		t.Fatalf("관전자 스냅샷에 yourHand 키 유출:\n%s", rawSpec)
	}
	// 남의 손패는 타인 스냅샷에도 실리지 않는다 — seat3 의 분홍 7은 본인만 본다
	if strings.Contains(raw0, `"suit":"pink","rank":7`) {
		t.Fatalf("타인 스냅샷에 남의 손패 유출:\n%s", raw0)
	}
	if strings.Contains(rawSpec, `"rank":7`) {
		t.Fatalf("관전자 스냅샷에 손패 유출:\n%s", rawSpec)
	}
	// 손패를 전부 소진한 좌석은 [] 로 나간다 (nil → null 금지)
	game.Players[2].Hand = []CWCard{}
	if !strings.Contains(rawOf(2), `"yourHand":[]`) {
		t.Fatalf("빈 손패가 []가 아니다:\n%s", rawOf(2))
	}
	cwSetHands(t, game, 0, [][]CWCard{
		{{Suit: CWSuitBlue, Rank: 5}, {Suit: CWSuitPink, Rank: 4}},
		{{Suit: CWSuitBlue, Rank: 9}, {Suit: CWSuitPink, Rank: 2}},
		{{Suit: CWSuitBlue, Rank: 1}, {Suit: CWSuitPink, Rank: 3}},
		{{Suit: CWSuitBlue, Rank: 2}, {Suit: CWSuitPink, Rank: 7}},
	})

	// ---- tasks 는 전원 공개 ----
	wantTask := `"tasks":[{"suit":"blue","rank":5,"seat":1,"done":false}]`
	for _, viewer := range []int{0, 1, 2, 3, -1} {
		if !strings.Contains(rawOf(viewer), wantTask) {
			t.Fatalf("viewer %d 에게 임무가 안 보인다:\n%s", viewer, rawOf(viewer))
		}
	}

	// ---- 공개 정보와 관전자 스냅샷 (패닉 없이 빌드된다) ----
	spec := h.buildCWState(room, -1)
	if spec.YourSeat != -1 || spec.YourHand != nil {
		t.Fatalf("관전자 스냅샷: yourSeat=%d yourHand=%v", spec.YourSeat, spec.YourHand)
	}
	if spec.Mission != 1 || spec.MaxMission != CWDefaultMaxMission || spec.CurrentSeat != 0 {
		t.Fatalf("공개 정보 이상: mission=%d/%d current=%d",
			spec.Mission, spec.MaxMission, spec.CurrentSeat)
	}
	if !strings.Contains(rawSpec, `"result":null`) ||
		!strings.Contains(rawSpec, `"lastTrick":null`) ||
		!strings.Contains(rawSpec, `"trick":[]`) ||
		!strings.Contains(rawSpec, `"revealed":null`) ||
		!strings.Contains(rawSpec, `"leadSuit":""`) ||
		!strings.Contains(rawSpec, `"commanderSeat":`) ||
		!strings.Contains(rawSpec, `"tokenLeft":1`) {
		t.Fatalf("관전자 raw 스냅샷 이상:\n%s", rawSpec)
	}

	// ---- 빈 대기실 스냅샷도 빈 배열 [] (null 금지) ----
	empty := h.lobbyRoomFor("ZZZZ")
	emptyRaw, _ := json.Marshal(h.buildCWState(empty, -1))
	for _, want := range []string{`"players":[]`, `"tasks":[]`, `"trick":[]`, `"commanderSeat":-1`} {
		if !strings.Contains(string(emptyRaw), want) {
			t.Fatalf("빈 대기실 스냅샷에 %s 부재:\n%s", want, emptyRaw)
		}
	}

	// ---- 통신: 서버가 검증하고, 통과분은 전원 공개 ----
	// 거짓 선언은 거부되고 상태가 바뀌지 않는다 (파랑은 한 장뿐이라 highest 불가)
	h.handleGameMessage(CWGameMessage{Client: clients[0], Message: CWMessage{
		Type: CWMsgCommunicate, Payload: CWCommunicatePayload{Index: 0, Hint: CWHintHighest}}})
	if game.Players[0].Revealed != nil {
		t.Fatalf("한 장뿐인 색의 highest 선언이 통과했다: %+v", game.Players[0].Revealed)
	}
	// 로켓은 공개할 수 없다 (손패 끝에 임시로 하나 얹어 확인)
	game.Players[0].Hand = append(game.Players[0].Hand, CWCard{Suit: CWSuitRocket, Rank: 3})
	h.handleGameMessage(CWGameMessage{Client: clients[0], Message: CWMessage{
		Type: CWMsgCommunicate, Payload: CWCommunicatePayload{Index: 2, Hint: CWHintOnly}}})
	if game.Players[0].Revealed != nil {
		t.Fatalf("로켓 공개가 통과했다: %+v", game.Players[0].Revealed)
	}
	game.Players[0].Hand = game.Players[0].Hand[:2]
	h.handleGameMessage(CWGameMessage{Client: clients[0], Message: CWMessage{
		Type: CWMsgCommunicate, Payload: CWCommunicatePayload{Index: 1, Hint: CWHintOnly}}})
	if game.Players[0].Revealed == nil || game.Players[0].TokenLeft != 0 {
		t.Fatalf("참인 통신이 반영되지 않았다: %+v", game.Players[0])
	}
	wantRevealed := `"revealed":{"card":{"suit":"pink","rank":4},"hint":"only"}`
	for _, viewer := range []int{0, 2, -1} {
		if !strings.Contains(rawOf(viewer), wantRevealed) {
			t.Fatalf("viewer %d 에게 공개 카드가 안 보인다:\n%s", viewer, rawOf(viewer))
		}
	}

	// ---- 제출: 차례가 아닌 좌석은 거부 ----
	h.handleGameMessage(CWGameMessage{Client: clients[2], Message: CWMessage{
		Type: CWMsgPlay, Payload: CWPlayPayload{Index: 0}}})
	if len(game.Trick) != 0 || game.CurrentSeat != 0 {
		t.Fatalf("차례 아닌 좌석의 제출이 통과했다: trick=%v current=%d",
			game.Trick, game.CurrentSeat)
	}

	// ---- 트릭 진행: 파랑 5는 seat1 의 임무 — seat1 이 파랑 9로 가져간다 ----
	for _, seat := range []int{0, 1, 2, 3} {
		h.handleGameMessage(CWGameMessage{Client: clients[seat], Message: CWMessage{
			Type: CWMsgPlay, Payload: CWPlayPayload{Index: 0}}})
	}
	if !game.Tasks[0].Done {
		t.Fatalf("임무가 완료되지 않았다: %+v", game.Tasks[0])
	}
	if game.LastTrick == nil || game.LastTrick.WinnerSeat != 1 {
		t.Fatalf("lastTrick = %+v", game.LastTrick)
	}
	// lastTrick 은 전원 공개
	if !strings.Contains(rawOf(-1), `"lastTrick":{"winnerSeat":1`) {
		t.Fatalf("lastTrick 이 공개 정보가 아니다:\n%s", rawOf(-1))
	}
	if game.Phase != CWPhaseRoundEnd {
		t.Fatalf("임무 성공 후 phase = %s", game.Phase)
	}

	// ---- 다음 임무 실패 → 종료 스냅샷도 손패 은닉을 지킨다 ----
	game.Phase = CWPhasePlaying
	game.Tasks = []CWTask{{Suit: CWSuitPink, Rank: 3, Seat: 0}}
	cwSetHands(t, game, 0, [][]CWCard{
		{{Suit: CWSuitPink, Rank: 1}},
		{{Suit: CWSuitPink, Rank: 9}},
		{{Suit: CWSuitPink, Rank: 3}},
		{{Suit: CWSuitPink, Rank: 2}},
	})
	for _, seat := range []int{0, 1, 2, 3} {
		if game.Phase != CWPhasePlaying {
			break
		}
		game.Play(seat, 0)
	}
	if game.Phase != CWPhaseGameOver {
		t.Fatalf("phase = %s, want game_over", game.Phase)
	}
	if game.Result == nil || game.Result.Cleared || game.Result.FailedReason != "wrong_winner" {
		t.Fatalf("result = %+v, want wrong_winner", game.Result)
	}
	overRaw := rawOf(-1)
	if strings.Contains(overRaw, `"yourHand"`) {
		t.Fatal("종료 스냅샷에서 손패 은닉이 깨졌다")
	}
	if !strings.Contains(overRaw, `"failedReason":"wrong_winner"`) {
		t.Fatalf("종료 result 가 공개 정보가 아니다:\n%s", overRaw)
	}
}

// TestCWAfkAutoProgress 접속만 유지한 채 아무도 카드를 내지 않는 3인전 —
// 자동 제출과 임무 자동 배분만으로 게임이 끝까지 완주하는지
// (endsAt 노출·afk 이벤트 포함). 카드는 매 트릭 줄어드니 반드시 끝난다.
func TestCWAfkAutoProgress(t *testing.T) {
	_, url, cleanup := newCWTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	conns := make([]*cwTestClient, CWMinPlayers)
	for i := range conns {
		conns[i] = cwDial(t, url)
		defer conns[i].conn.Close()
		cwJoin(t, conns[i], fmt.Sprintf("잠수%d", i), "")
	}
	host := conns[0]
	host.send(t, CWMessage{Type: CWMsgStart})

	state := host.cwWaitPhase(t, string(CWPhasePlaying))
	if ends := int64(state["endsAt"].(float64)); ends <= 0 {
		t.Fatalf("제출 스냅샷의 endsAt = %d, want unixMillis", ends)
	}
	if int(state["mission"].(float64)) != 1 ||
		int(state["maxMission"].(float64)) != CWDefaultMaxMission {
		t.Fatalf("시작 스냅샷 = mission %v/%v", state["mission"], state["maxMission"])
	}
	commander := int(state["commanderSeat"].(float64))
	if commander < 0 || commander >= CWMinPlayers {
		t.Fatalf("commanderSeat = %d", commander)
	}
	if int(state["currentSeat"].(float64)) != commander {
		t.Fatalf("첫 리드 = %v, want 사령관 %d", state["currentSeat"], commander)
	}
	hand, ok := state["yourHand"].([]interface{})
	if !ok || len(hand) < CWDeckSize/CWMinPlayers {
		t.Fatalf("본인 손패 = %v", state["yourHand"])
	}
	if tasks, ok := state["tasks"].([]interface{}); !ok || len(tasks) != 1 {
		t.Fatalf("1임무 tasks = %v", state["tasks"])
	}
	if trick, ok := state["trick"].([]interface{}); !ok || len(trick) != 0 {
		t.Fatalf("시작 trick = %v (빈 배열이어야 한다)", state["trick"])
	}
	for _, pRaw := range state["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		if int(p["tokenLeft"].(float64)) != CWTokenPerMission {
			t.Fatalf("시작 토큰 = %v", p["tokenLeft"])
		}
		if p["revealed"] != nil {
			t.Fatalf("시작 공개 카드 = %v", p["revealed"])
		}
	}

	// 나머지는 더 읽지 않는다 — 백그라운드로 비워 버퍼 포화만 막는다
	for _, c := range conns[1:] {
		cwDrain(c)
	}

	sawAfk := false
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		msg := host.waitMatch(t, "event-or-over", func(m CWMessage) bool {
			return m.Type == CWMsgEvent || m.Type == CWMsgGameOver
		})
		if msg.Type == CWMsgEvent {
			ev := cwPayloadMap(t, msg)
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
		over := cwPayloadMap(t, msg)
		if _, ok := over["cleared"].(bool); !ok {
			t.Fatalf("종료 payload = %v", over)
		}
		if !sawAfk {
			t.Fatal("afk 자동 진행 이벤트가 한 번도 없었다")
		}
		return
	}
	t.Fatal("전원 방치 게임이 45초 안에 끝나지 않았다")
}

// TestCWRoomCodeAndSpectate 방 코드 발급·코드 입장·시작 후 코드 관전 진입.
// 관전자 스냅샷은 yourSeat -1 에 yourHand 부재지만 임무는 그대로 본다.
// 행동은 전부 차단된다.
func TestCWRoomCodeAndSpectate(t *testing.T) {
	_, url, cleanup := newCWTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := cwDial(t, url)
	defer host.conn.Close()
	joined := cwJoin(t, host, "호스트", "NEW")
	code, _ := joined["roomCode"].(string)
	if len(code) != roomCodeLen {
		t.Fatalf("roomCode = %q, want %d자", code, roomCodeLen)
	}

	guests := make([]*cwTestClient, CWMinPlayers-1)
	for i := range guests {
		guests[i] = cwDial(t, url)
		defer guests[i].conn.Close()
		g := cwJoin(t, guests[i], fmt.Sprintf("동료%d", i), code)
		if g["roomCode"] != code || int(g["yourSeat"].(float64)) != i+1 {
			t.Fatalf("코드 입장 실패: %v", g)
		}
	}

	host.send(t, CWMessage{Type: CWMsgStart})
	state := host.cwWaitPhase(t, string(CWPhasePlaying))
	if state["roomCode"] != code || len(state["players"].([]interface{})) != CWMinPlayers {
		t.Fatalf("시작 실패: %v", state["players"])
	}
	if _, ok := state["yourHand"].([]interface{}); !ok {
		t.Fatalf("본인 손패 부재: %v", state)
	}
	for _, c := range guests {
		cwDrain(c)
	}

	// 시작된 방의 코드로 들어오면 관전자
	spec := cwDial(t, url)
	defer spec.conn.Close()
	spec.send(t, CWMessage{Type: CWMsgJoinGame, Payload: CWJoinGamePayload{Name: "구경꾼", Room: code}})
	specJoined := cwPayloadMap(t, spec.waitFor(t, CWMsgSpectateJoined))
	if specJoined["roomCode"] != code {
		t.Fatalf("관전 입장 roomCode = %v", specJoined["roomCode"])
	}

	specState := cwPayloadMap(t, spec.waitFor(t, CWMsgGameState))
	if int(specState["yourSeat"].(float64)) != -1 {
		t.Fatalf("관전자 yourSeat = %v, want -1", specState["yourSeat"])
	}
	if leaked, ok := specState["yourHand"]; ok {
		t.Fatalf("관전자에게 손패 유출: %v", leaked)
	}
	if tasks, ok := specState["tasks"].([]interface{}); !ok || len(tasks) == 0 {
		t.Fatalf("관전자에게 임무가 안 보인다: %v", specState["tasks"])
	}
	if int(specState["spectators"].(float64)) != 1 {
		t.Fatalf("관전자 수 = %v", specState["spectators"])
	}
	for _, pRaw := range specState["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		if int(p["handCount"].(float64)) < 0 {
			t.Fatalf("관전자 handCount = %v", p["handCount"])
		}
		if _, ok := p["tokenLeft"].(float64); !ok {
			t.Fatalf("관전자에게 tokenLeft 부재: %v", p)
		}
	}

	// 관전자는 어떤 행동도 못 한다
	spec.send(t, CWMessage{Type: CWMsgPlay, Payload: CWPlayPayload{Index: 0}})
	errPayload := cwPayloadMap(t, spec.waitFor(t, CWMsgError))
	if errPayload["message"] != spectatorDeniedMsg {
		t.Fatalf("관전자 차단 문구 = %v", errPayload["message"])
	}
}

// TestCWReconnect 재접속 3종 — 이탈 통지(cw_player_disconnected) 후 세션으로
// 돌아오면 좌석·손패가 그대로 복원되고(cw_player_reconnected), 모르는 세션은
// cw_session_expired 로 거절된다.
func TestCWReconnect(t *testing.T) {
	_, url, cleanup := newCWTestServer(t, 3*time.Second)
	defer cleanup()

	conns := make([]*cwTestClient, CWMinPlayers)
	sessions := make([]string, CWMinPlayers)
	for i := range conns {
		conns[i] = cwDial(t, url)
		joined := cwJoin(t, conns[i], fmt.Sprintf("P%d", i), "")
		sessions[i], _ = joined["sessionId"].(string)
	}
	defer conns[0].conn.Close()
	conns[0].send(t, CWMessage{Type: CWMsgStart})
	before := conns[0].cwWaitPhase(t, string(CWPhasePlaying))
	if _, ok := before["yourHand"].([]interface{}); !ok {
		t.Fatal("시작 스냅샷에 yourHand 부재")
	}
	for _, c := range conns[2:] {
		cwDrain(c)
	}

	// 좌석 1 이탈 → 남은 사람에게 이탈 통지
	conns[1].conn.Close()
	discon := cwPayloadMap(t, conns[0].waitFor(t, CWMsgPlayerDisconnected))
	if int(discon["seat"].(float64)) != 1 || discon["name"] != "P1" {
		t.Fatalf("이탈 통지 = %v", discon)
	}
	if int(discon["graceSeconds"].(float64)) <= 0 {
		t.Fatalf("graceSeconds = %v", discon["graceSeconds"])
	}

	// 세션으로 재접속 → 좌석·손패 복원
	back := cwDial(t, url)
	defer back.conn.Close()
	back.send(t, CWMessage{Type: CWMsgRejoin, Payload: CWRejoinPayload{SessionID: sessions[1]}})
	recon := cwPayloadMap(t, back.waitFor(t, CWMsgPlayerReconnected))
	if int(recon["seat"].(float64)) != 1 {
		t.Fatalf("재접속 통지 = %v", recon)
	}
	restored := cwPayloadMap(t, back.waitFor(t, CWMsgGameState))
	if int(restored["yourSeat"].(float64)) != 1 {
		t.Fatalf("복원 yourSeat = %v", restored["yourSeat"])
	}
	if _, ok := restored["yourHand"].([]interface{}); !ok {
		t.Fatalf("복원 스냅샷에 yourHand 부재: %v", restored)
	}

	// 모르는 세션은 만료 처리
	ghost := cwDial(t, url)
	defer ghost.conn.Close()
	ghost.send(t, CWMessage{Type: CWMsgRejoin, Payload: CWRejoinPayload{SessionID: "없는-세션"}})
	ghost.waitFor(t, CWMsgSessionExpired)
}

// TestCWBotTakeover 유예 만료 좌석을 봇이 이어받아 게임이 멈추지 않는지 —
// 트릭 차례가 이탈 좌석에 막히지 않고 완주한다.
func TestCWBotTakeover(t *testing.T) {
	_, url, cleanup := newCWTestServer(t, 120*time.Millisecond)
	defer cleanup()

	conns := make([]*cwTestClient, CWMinPlayers)
	for i := range conns {
		conns[i] = cwDial(t, url)
		defer conns[i].conn.Close()
		cwJoin(t, conns[i], fmt.Sprintf("P%d", i), "")
	}
	conns[0].send(t, CWMessage{Type: CWMsgStart})
	conns[0].cwWaitPhase(t, string(CWPhasePlaying))
	for _, c := range conns[2:] {
		cwDrain(c)
	}

	// 좌석 1 이탈 → 유예 만료 → 봇 대체
	conns[1].conn.Close()
	sawTakeover := false
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		msg := conns[0].waitMatch(t, "event-or-over", func(m CWMessage) bool {
			return m.Type == CWMsgEvent || m.Type == CWMsgGameOver
		})
		if msg.Type == CWMsgEvent {
			ev := cwPayloadMap(t, msg)
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
		over := cwPayloadMap(t, msg)
		if _, ok := over["cleared"].(bool); !ok {
			t.Fatalf("종료 payload = %v", over)
		}
		return
	}
	t.Fatal("봇 대체 후 게임이 45초 안에 끝나지 않았다")
}

// TestCWCoopMatchRecord 협력 전적 표기 — 클리어면 전원이 Winner 에 들어가고,
// 실패면 어떤 닉네임과도 겹치지 않는 표식이 들어가 전원 패자로 집계된다
// (Winner "" 는 전적 장부에서 무승부라 쓰면 안 된다).
func TestCWCoopMatchRecord(t *testing.T) {
	crew := []string{"가", "나", "다", "라"}
	players := strings.Join(crew, "·")

	// 클리어 — 전원 승자
	winners := map[string]bool{}
	for _, name := range splitWinners(players) {
		winners[name] = true
	}
	for _, name := range splitPlayers(players) {
		if !winners[name] {
			t.Fatalf("클리어인데 %s 가 승자가 아니다", name)
		}
	}

	// 실패 — 전원 패자 (무승부 아님)
	if cwFailWinnerTag == "" {
		t.Fatal("실패 표기가 빈 문자열이면 무승부로 집계된다")
	}
	lost := splitWinners(cwFailWinnerTag)
	if len(lost) == 0 {
		t.Fatalf("실패 표기 파싱 = %v", lost)
	}
	for _, name := range splitPlayers(players) {
		for _, w := range lost {
			if w == name {
				t.Fatalf("실패 표기가 참가자 닉네임 %s 와 겹친다", name)
			}
		}
	}
}
