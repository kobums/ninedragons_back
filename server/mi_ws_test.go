package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ==================== 더 마인드 WS 통합 테스트 ====================
//
// 시간 상수는 전부 var 라 여기 init 에서 한 번만 낮춘다 — 테스트 도중에
// 바꾸면 허브 고루틴·봇 고루틴과 경합한다(-race). 허브별로 달라야 하는
// 값(유예·라운드 캡·게임 캡·수리검 창)은 Run 전에 허브 필드로 정한다.
func init() {
	miReadyDelay = 60 * time.Millisecond
	miRoundEndDelay = 10 * time.Millisecond
	miRoundCap = 3 * time.Second
	miStarVoteWindow = 300 * time.Millisecond
	miGameCap = 90 * time.Second

	// 봇 시계 — 간격 1당 6ms (실사용 220ms). 지터 비율은 실사용 그대로 둔다.
	miBotTickPerGap = 6 * time.Millisecond
	miBotMinWait = 2 * time.Millisecond
	miBotWaitMax = 2 * time.Second
	miBotIdleTick = 4 * time.Millisecond
	miBotSettleWait = 1 * time.Second
}

// miTimings 허브별 시간 설정 (Run 전에 정한다)
type miTimings struct {
	grace    time.Duration
	ready    time.Duration
	roundEnd time.Duration
	roundCap time.Duration
	star     time.Duration
	gameCap  time.Duration
}

// miSlowTimings 타이머로 끝나면 안 되는 테스트가 쓰는 넉넉한 설정
func miSlowTimings() miTimings {
	return miTimings{
		grace:    defaultDisconnectGrace,
		ready:    miReadyDelay,
		roundEnd: miRoundEndDelay,
		roundCap: 30 * time.Second,
		star:     10 * time.Second,
		gameCap:  90 * time.Second,
	}
}

// miBotTimings 봇전 설정 — 사람 이탈을 곧바로 봇이 이어받게 유예를 짧게 둔다
func miBotTimings() miTimings {
	tm := miSlowTimings()
	tm.grace = 30 * time.Millisecond
	tm.roundCap = 5 * time.Second
	tm.gameCap = 90 * time.Second
	return tm
}

// miTestClient 공용 testConn 에 더 마인드 메시지 타입의 waitFor 를 얹은 래퍼
type miTestClient struct {
	testConn[MIMessage]
}

func newMITestServer(t *testing.T, tm miTimings) (*MIHub, string, func()) {
	t.Helper()
	hub := NewMIHub()
	hub.grace = tm.grace
	hub.readyDelay = tm.ready
	hub.roundEndDelay = tm.roundEnd
	hub.roundCap = tm.roundCap
	hub.starWindow = tm.star
	hub.gameCap = tm.gameCap
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeMIWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func miDial(t *testing.T, url string) *miTestClient {
	t.Helper()
	return &miTestClient{dialWS[MIMessage](t, url)}
}

func (c *miTestClient) waitFor(t *testing.T, msgType MIMessageType) MIMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m MIMessage) bool { return m.Type == msgType })
}

func miPayloadMap(t *testing.T, msg MIMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// miJoin 입장하고 mi_player_joined payload 를 돌려준다
func miJoin(t *testing.T, c *miTestClient, name, room string) map[string]interface{} {
	t.Helper()
	c.send(t, MIMessage{Type: MIMsgJoinGame, Payload: MIJoinGamePayload{Name: name, Room: room}})
	return miPayloadMap(t, c.waitFor(t, MIMsgPlayerJoined))
}

// miWaitPhase 지정한 phase 의 스냅샷이 올 때까지 소비
func (c *miTestClient) miWaitPhase(t *testing.T, phase MIPhase) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "mi_game_state("+string(phase)+")", func(m MIMessage) bool {
		if m.Type != MIMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		return ok && state["phase"] == string(phase)
	})
	return miPayloadMap(t, msg)
}

// miWaitRound 지정한 라운드의 playing 스냅샷
func (c *miTestClient) miWaitRound(t *testing.T, round int) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, fmt.Sprintf("playing(round %d)", round), func(m MIMessage) bool {
		if m.Type != MIMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		if !ok || state["phase"] != string(MIPhasePlaying) {
			return false
		}
		r, ok := state["round"].(float64)
		return ok && int(r) == round
	})
	return miPayloadMap(t, msg)
}

// miDrain 배경으로 소켓을 비운다 (읽지 않는 연결의 버퍼 포화 방지)
func miDrain(c *miTestClient) {
	conn := c.conn
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

// miHandOf 스냅샷에서 본인 손패를 꺼낸다 (없으면 nil)
func miHandOf(state map[string]interface{}) []int {
	raw, ok := state["yourHand"].([]interface{})
	if !ok {
		return nil
	}
	hand := []int{}
	for _, v := range raw {
		hand = append(hand, int(v.(float64)))
	}
	return hand
}

// miSeatClients 허브 고루틴 없이 핸들러를 직접 부르는 결정적 테스트용 —
// 소켓 없는 사람 좌석 n개를 앉힌 방을 만든다
func miSeatClients(t *testing.T, h *MIHub, room *miRoom, n int) []*MIClient {
	t.Helper()
	clients := make([]*MIClient, n)
	for i := range clients {
		c := &MIClient{wsClient: newBotWSClient(), Hub: h}
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

// miTake 소켓 없는 클라이언트의 Send 큐에 쌓인 메시지를 전부 꺼낸다
func miTake(t *testing.T, c *MIClient) []MIMessage {
	t.Helper()
	out := []MIMessage{}
	for {
		select {
		case data := <-c.Send:
			var msg MIMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			out = append(out, msg)
		default:
			return out
		}
	}
}

// ==================== 은닉 ====================

// TestMIHiddenHand 이 게임의 유일한 은닉 계약 — yourHand 는 본인만 본다.
// 타인·관전자의 raw JSON 에는 **키 자체가 없어야** 한다. 나머지 필드는
// 전원 완전히 동일하다. 허브 고루틴 없이 결정적으로 검증한다.
func TestMIHiddenHand(t *testing.T) {
	h := NewMIHub()
	room := h.lobbyRoomFor("")
	clients := miSeatClients(t, h, room, 3)
	h.startGame(room)
	defer h.stopTimers(room)

	game := room.Game
	game.BeginPlaying()
	game.DrainEvents()

	rawOf := func(viewer int) string {
		data, err := json.Marshal(h.buildMIState(room, viewer))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(data)
	}

	// ---- 관전자의 raw JSON 에는 yourHand 키가 없다 ----
	spectatorRaw := rawOf(-1)
	if strings.Contains(spectatorRaw, "yourHand") {
		t.Fatalf("관전자 스냅샷에 yourHand 가 실렸다:\n%s", spectatorRaw)
	}
	if !strings.Contains(spectatorRaw, `"yourSeat":-1`) {
		t.Fatalf("관전자 yourSeat 가 -1 이 아니다:\n%s", spectatorRaw)
	}

	// ---- 좌석 스냅샷은 자기 yourHand 하나만 더 실린다 ----
	for _, seat := range []int{0, 1, 2} {
		handJSON, err := json.Marshal(game.Players[seat].Hand)
		if err != nil {
			t.Fatalf("marshal hand: %v", err)
		}
		mine := rawOf(seat)
		insert := fmt.Sprintf(`"yourHand":%s,`, handJSON)
		if !strings.Contains(mine, insert) {
			t.Fatalf("seat%d 스냅샷에 자기 손패가 없다 (%s):\n%s", seat, insert, mine)
		}
		want := strings.Replace(spectatorRaw,
			`"yourSeat":-1`, fmt.Sprintf(`"yourSeat":%d`, seat), 1)
		if got := strings.Replace(mine, insert, "", 1); got != want {
			t.Fatalf("seat%d 스냅샷이 yourHand 말고도 다르다:\n%s\n%s", seat, got, want)
		}
	}

	// ---- 남의 손패 숫자는 어디에도 없다 (handCount 만 공개) ----
	for _, p := range game.Players {
		if len(p.Hand) != game.Round {
			t.Fatalf("seat%d 손패 = %v (라운드 %d)", p.Seat, p.Hand, game.Round)
		}
	}
	spec := h.buildMIState(room, -1)
	for i, pv := range spec.Players {
		if pv.HandCount != len(game.Players[i].Hand) {
			t.Fatalf("seat%d handCount = %d, want %d", i, pv.HandCount, len(game.Players[i].Hand))
		}
	}

	// ---- 공개 필드 계약 ----
	for _, want := range []string{
		`"lastMistake":null`, `"result":null`, `"starVote":null`,
		`"pile":[]`, `"lastPlayed":0`, `"round":1`, `"maxRound":10`,
		`"lives":3`, `"stars":1`, `"handCount":1`,
	} {
		if !strings.Contains(spectatorRaw, want) {
			t.Fatalf("스냅샷에 %s 부재:\n%s", want, spectatorRaw)
		}
	}

	// ---- 빈 대기실 스냅샷도 빈 배열 [] (null 금지, 패닉 금지) ----
	empty := h.lobbyRoomFor("ZZZZ")
	emptyRaw, _ := json.Marshal(h.buildMIState(empty, -1))
	for _, want := range []string{
		`"players":[]`, `"pile":[]`, `"round":0`, `"hostSeat":-1`,
		`"yourSeat":-1`, `"endsAt":0`, `"phase":"waiting"`,
	} {
		if !strings.Contains(string(emptyRaw), want) {
			t.Fatalf("빈 대기실 스냅샷에 %s 부재:\n%s", want, emptyRaw)
		}
	}
	if strings.Contains(string(emptyRaw), "yourHand") {
		t.Fatalf("빈 대기실 관전 스냅샷에 yourHand 가 실렸다:\n%s", emptyRaw)
	}
	// 존재하지 않는 좌석 시점도 패닉 없이 관전자와 같게 나온다
	if _, err := json.Marshal(h.buildMIState(empty, 0)); err != nil {
		t.Fatalf("빈 대기실 seat0 시점: %v", err)
	}

	// ---- 손패를 다 낸 좌석의 yourHand 는 [] 다 (null 금지) ----
	for _, c := range clients {
		miTake(t, c)
	}
	lowest, _ := miLowestSeat(game.Players)
	h.handleGameMessage(MIGameMessage{Client: clients[lowest], Message: MIMessage{Type: MIMsgPlay}})
	if !strings.Contains(rawOf(lowest), `"yourHand":[]`) {
		t.Fatalf("빈 손패가 [] 로 나가지 않았다:\n%s", rawOf(lowest))
	}

	// 낸 카드는 전원에게 공개된다 (더미·직전 수)
	if !strings.Contains(rawOf(-1), fmt.Sprintf(`"lastPlayed":%d`, game.LastPlayed)) {
		t.Fatalf("직전 수가 공개되지 않았다:\n%s", rawOf(-1))
	}
	// 이벤트에는 좌석과 이름이 함께 실린다 (프론트 배너 근거)
	sawPlay := false
	for _, msg := range miTake(t, clients[0]) {
		if msg.Type != MIMsgEvent {
			continue
		}
		ev := miPayloadMap(t, msg)
		if ev["kind"] == "play" || ev["kind"] == "mistake" {
			if ev["name"] == nil || ev["name"] == "" {
				t.Fatalf("판정 이벤트에 name 부재: %v", ev)
			}
			if _, ok := ev["seat"].(float64); !ok {
				t.Fatalf("판정 이벤트에 seat 부재: %v", ev)
			}
			sawPlay = true
		}
	}
	if !sawPlay {
		t.Fatal("카드 이벤트가 방송되지 않았다")
	}
}

// ==================== 라운드 흐름 ====================

// miPlayInOrder 두 사람이 손패를 오름차순으로 완벽하게 낸다 (라운드 성공).
// 마지막 카드가 올라간 스냅샷을 돌려준다 — 그 스냅샷이 곧 라운드 정산이라
// 따로 기다리면 이미 지나간 메시지를 영원히 기다리게 된다.
func miPlayInOrder(t *testing.T, clients []*miTestClient,
	states []map[string]interface{}) map[string]interface{} {
	t.Helper()
	type card struct {
		value int
		who   int
	}
	cards := []card{}
	for i, state := range states {
		hand := miHandOf(state)
		if hand == nil {
			t.Fatalf("client%d 스냅샷에 yourHand 가 없다: %v", i, state)
		}
		for _, v := range hand {
			cards = append(cards, card{value: v, who: i})
		}
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].value < cards[j].value })

	last := map[string]interface{}{}
	for _, c := range cards {
		clients[c.who].send(t, MIMessage{Type: MIMsgPlay})
		// 그 카드가 더미에 올라온 스냅샷을 기다린다 (다음 카드와 뒤섞이지 않게)
		msg := clients[c.who].waitMatch(t, fmt.Sprintf("lastPlayed=%d", c.value),
			func(m MIMessage) bool {
				if m.Type != MIMsgGameState {
					return false
				}
				s, ok := m.Payload.(map[string]interface{})
				if !ok {
					return false
				}
				played, _ := s["lastPlayed"].(float64)
				return int(played) == c.value
			})
		last = miPayloadMap(t, msg)
	}
	return last
}

// TestMIRoundFlowAndRewards 라운드 순환을 와이어로 확인한다 —
// ready(카운트다운) → playing → round_end(정산) → 다음 라운드.
// 2라운드를 마치면 수리검 +1, 3라운드를 마치면 생명 +1 이 스냅샷에 실린다.
func TestMIRoundFlowAndRewards(t *testing.T) {
	_, url, cleanup := newMITestServer(t, miSlowTimings())
	defer cleanup()

	clients := make([]*miTestClient, 2)
	for i := range clients {
		clients[i] = miDial(t, url)
		defer clients[i].conn.Close()
		miJoin(t, clients[i], fmt.Sprintf("P%d", i), "")
	}
	clients[0].send(t, MIMessage{Type: MIMsgStart})

	// 1라운드 — 카운트다운 중에는 낼 수 없다
	ready := clients[0].miWaitPhase(t, MIPhaseReady)
	if int(ready["round"].(float64)) != 1 || int(ready["lives"].(float64)) != 2 ||
		int(ready["stars"].(float64)) != 1 || int(ready["maxRound"].(float64)) != 12 {
		t.Fatalf("1라운드 ready 스냅샷 = %v", ready)
	}
	if int64(ready["endsAt"].(float64)) <= time.Now().UnixMilli() {
		t.Fatalf("ready endsAt = %v (미래의 카운트다운이어야 한다)", ready["endsAt"])
	}
	clients[0].send(t, MIMessage{Type: MIMsgPlay})
	errPayload := miPayloadMap(t, clients[0].waitFor(t, MIMsgError))
	if !strings.Contains(errPayload["message"].(string), "카운트다운") {
		t.Fatalf("카운트다운 중 거절 문구 = %v", errPayload["message"])
	}

	wantStars := []int{1, 2, 2} // 1·2·3 라운드를 마친 뒤의 수리검
	wantLives := []int{2, 2, 3} // 3라운드를 마치면 생명 +1
	for round := 1; round <= 3; round++ {
		states := make([]map[string]interface{}, len(clients))
		for i, c := range clients {
			states[i] = c.miWaitRound(t, round)
			if len(miHandOf(states[i])) != round {
				t.Fatalf("%d라운드 seat%d 손패 = %v", round, i, miHandOf(states[i]))
			}
			for _, pRaw := range states[i]["players"].([]interface{}) {
				p := pRaw.(map[string]interface{})
				if int(p["handCount"].(float64)) != round {
					t.Fatalf("%d라운드 handCount = %v", round, p["handCount"])
				}
			}
		}
		end := miPlayInOrder(t, clients, states)
		if end["phase"] != string(MIPhaseRoundEnd) {
			t.Fatalf("마지막 카드 뒤 phase = %v, want round_end", end["phase"])
		}
		if int(end["round"].(float64)) != round {
			t.Fatalf("정산 라운드 = %v, want %d", end["round"], round)
		}
		if int(end["lives"].(float64)) != wantLives[round-1] ||
			int(end["stars"].(float64)) != wantStars[round-1] {
			t.Fatalf("%d라운드 정산 생명=%v 수리검=%v, want %d/%d", round,
				end["lives"], end["stars"], wantLives[round-1], wantStars[round-1])
		}
		// 완벽하게 냈으니 실수 기록이 없고, 더미에는 낸 순서대로 쌓여 있다
		if end["lastMistake"] != nil {
			t.Fatalf("실수 없이 냈는데 lastMistake = %v", end["lastMistake"])
		}
		pile, ok := end["pile"].([]interface{})
		if !ok || len(pile) != round*len(clients) {
			t.Fatalf("%d라운드 더미 = %v", round, end["pile"])
		}
		last := 0
		for _, raw := range pile {
			v := int(raw.(float64))
			if v <= last {
				t.Fatalf("더미가 오름차순이 아니다: %v", end["pile"])
			}
			last = v
		}
		if int(end["lastPlayed"].(float64)) != last {
			t.Fatalf("lastPlayed = %v, want %d", end["lastPlayed"], last)
		}
	}
}

// ==================== 방 코드 / 관전 ====================

// TestMIRoomCodeAndSpectate 방 코드 발급·코드 입장·시작 후 코드 관전 진입.
// 관전자는 yourHand 없는 스냅샷을 받고 행동은 전부 차단된다.
func TestMIRoomCodeAndSpectate(t *testing.T) {
	_, url, cleanup := newMITestServer(t, miSlowTimings())
	defer cleanup()

	host := miDial(t, url)
	defer host.conn.Close()
	joined := miJoin(t, host, "호스트", "NEW")
	code, _ := joined["roomCode"].(string)
	if len(code) != roomCodeLen {
		t.Fatalf("roomCode = %q, want %d자", code, roomCodeLen)
	}

	guest := miDial(t, url)
	defer guest.conn.Close()
	g := miJoin(t, guest, "동료", code)
	if g["roomCode"] != code || int(g["yourSeat"].(float64)) != 1 {
		t.Fatalf("코드 입장 실패: %v", g)
	}

	host.send(t, MIMessage{Type: MIMsgStart})
	state := host.miWaitPhase(t, MIPhaseReady)
	if state["roomCode"] != code || len(state["players"].([]interface{})) != 2 {
		t.Fatalf("시작 실패: %v", state)
	}
	miDrain(guest)

	// 시작된 방의 코드로 들어오면 관전자
	spec := miDial(t, url)
	defer spec.conn.Close()
	spec.send(t, MIMessage{Type: MIMsgJoinGame,
		Payload: MIJoinGamePayload{Name: "구경꾼", Room: code}})
	specJoined := miPayloadMap(t, spec.waitFor(t, MIMsgSpectateJoined))
	if specJoined["roomCode"] != code {
		t.Fatalf("관전 입장 roomCode = %v", specJoined["roomCode"])
	}

	specState := miPayloadMap(t, spec.waitFor(t, MIMsgGameState))
	if int(specState["yourSeat"].(float64)) != -1 {
		t.Fatalf("관전자 yourSeat = %v, want -1", specState["yourSeat"])
	}
	if _, ok := specState["yourHand"]; ok {
		t.Fatalf("관전자에게 yourHand 가 실렸다: %v", specState)
	}
	// 관전자도 공개 정보(장수·생명·수리검·더미)는 전부 본다
	for _, pRaw := range specState["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		if _, ok := p["handCount"].(float64); !ok {
			t.Fatalf("관전자에게 handCount 부재: %v", p)
		}
	}
	if int(specState["spectators"].(float64)) != 1 {
		t.Fatalf("관전자 수 = %v", specState["spectators"])
	}

	// 관전자는 어떤 행동도 못 한다
	for _, msgType := range []MIMessageType{MIMsgPlay, MIMsgStarPropose, MIMsgStarAccept} {
		spec.send(t, MIMessage{Type: msgType})
		errPayload := miPayloadMap(t, spec.waitFor(t, MIMsgError))
		if errPayload["message"] != spectatorDeniedMsg {
			t.Fatalf("%s 관전자 차단 문구 = %v", msgType, errPayload["message"])
		}
	}
}

// ==================== 수리검 ====================

// TestMIStarVoteOverWire 수리검 만장일치를 와이어로 확인한다 —
// 거절하면 무산되고 수리검이 줄지 않으며, 전원 찬성하면 각자 최저 카드가
// 한 장씩 사라진다 (생명 소모 없음).
func TestMIStarVoteOverWire(t *testing.T) {
	_, url, cleanup := newMITestServer(t, miSlowTimings())
	defer cleanup()

	clients := make([]*miTestClient, 2)
	for i := range clients {
		clients[i] = miDial(t, url)
		defer clients[i].conn.Close()
		miJoin(t, clients[i], fmt.Sprintf("P%d", i), "")
	}
	clients[0].send(t, MIMessage{Type: MIMsgStart})
	// 손패는 1라운드 스냅샷에서 미리 받아 둔다 — 같은 스냅샷을 두 번
	// 기다리면 이미 지나간 메시지를 영원히 기다리게 된다
	before := 0
	for i, c := range clients {
		state := c.miWaitRound(t, 1)
		if i == 0 {
			before = len(miHandOf(state))
		}
	}

	// ---- 거절 → 무산 ----
	clients[0].send(t, MIMessage{Type: MIMsgStarPropose})
	proposed := clients[1].waitMatch(t, "starVote 열림", func(m MIMessage) bool {
		if m.Type != MIMsgGameState {
			return false
		}
		s, ok := m.Payload.(map[string]interface{})
		return ok && s["starVote"] != nil
	})
	vote := miPayloadMap(t, proposed)["starVote"].(map[string]interface{})
	if int(vote["proposer"].(float64)) != 0 {
		t.Fatalf("proposer = %v", vote["proposer"])
	}
	accepted := vote["accepted"].([]interface{})
	if len(accepted) != 1 || int(accepted[0].(float64)) != 0 {
		t.Fatalf("제안자 자동 찬성 = %v", accepted)
	}
	if int64(vote["endsAt"].(float64)) <= time.Now().UnixMilli() {
		t.Fatalf("수리검 endsAt = %v", vote["endsAt"])
	}

	clients[1].send(t, MIMessage{Type: MIMsgStarDecline})
	declined := miPayloadMap(t, clients[0].waitMatch(t, "starVote 무산", func(m MIMessage) bool {
		if m.Type != MIMsgGameState {
			return false
		}
		s, ok := m.Payload.(map[string]interface{})
		return ok && s["starVote"] == nil
	}))
	if int(declined["stars"].(float64)) != 1 {
		t.Fatalf("무산인데 수리검이 줄었다: %v", declined["stars"])
	}

	// ---- 만장일치 → 발동 ----
	clients[0].send(t, MIMessage{Type: MIMsgStarPropose})
	clients[1].waitMatch(t, "starVote 재개", func(m MIMessage) bool {
		if m.Type != MIMsgGameState {
			return false
		}
		s, ok := m.Payload.(map[string]interface{})
		return ok && s["starVote"] != nil
	})
	clients[1].send(t, MIMessage{Type: MIMsgStarAccept})

	used := miPayloadMap(t, clients[0].waitMatch(t, "수리검 발동", func(m MIMessage) bool {
		if m.Type != MIMsgGameState {
			return false
		}
		s, ok := m.Payload.(map[string]interface{})
		if !ok || s["starVote"] != nil {
			return false
		}
		stars, _ := s["stars"].(float64)
		return int(stars) == 0
	}))
	if int(used["lives"].(float64)) != 2 {
		t.Fatalf("수리검이 생명을 깎았다: %v", used["lives"])
	}
	// 1라운드라 각자 1장뿐 — 발동으로 손패가 비고 라운드가 성공한다
	if before != 1 {
		t.Fatalf("1라운드 손패 = %d장", before)
	}
	if used["phase"] != string(MIPhaseRoundEnd) {
		t.Fatalf("수리검으로 라운드가 끝나지 않았다: %v", used["phase"])
	}
	// 수리검으로 버린 카드는 더미에 쌓지 않는다 (낸 것이 아니라 버린 것)
	if pile, ok := used["pile"].([]interface{}); !ok || len(pile) != 0 {
		t.Fatalf("수리검 카드가 더미에 들어갔다: %v", used["pile"])
	}
}

// ==================== 재접속 / 봇 대체 ====================

// TestMIReconnect 재접속 3종 — 이탈 통지(mi_player_disconnected) 후 세션으로
// 돌아오면 좌석·손패가 그대로 복원되고(mi_player_reconnected), 모르는 세션은
// mi_session_expired 로 거절된다.
func TestMIReconnect(t *testing.T) {
	tm := miSlowTimings()
	tm.grace = 3 * time.Second
	_, url, cleanup := newMITestServer(t, tm)
	defer cleanup()

	conns := make([]*miTestClient, 2)
	sessions := make([]string, 2)
	for i := range conns {
		conns[i] = miDial(t, url)
		joined := miJoin(t, conns[i], fmt.Sprintf("P%d", i), "")
		sessions[i], _ = joined["sessionId"].(string)
	}
	defer conns[0].conn.Close()
	conns[0].send(t, MIMessage{Type: MIMsgStart})
	state := conns[1].miWaitRound(t, 1)
	hand := miHandOf(state)
	if len(hand) != 1 {
		t.Fatalf("1라운드 손패 = %v", hand)
	}

	// 좌석 1 이탈 → 남은 사람에게 이탈 통지
	conns[1].conn.Close()
	discon := miPayloadMap(t, conns[0].waitFor(t, MIMsgPlayerDisconnected))
	if int(discon["seat"].(float64)) != 1 || discon["name"] != "P1" {
		t.Fatalf("이탈 통지 = %v", discon)
	}
	if int(discon["graceSeconds"].(float64)) <= 0 {
		t.Fatalf("graceSeconds = %v", discon["graceSeconds"])
	}

	// 세션으로 재접속 → 좌석·손패 복원
	back := miDial(t, url)
	defer back.conn.Close()
	back.send(t, MIMessage{Type: MIMsgRejoin, Payload: MIRejoinPayload{SessionID: sessions[1]}})
	recon := miPayloadMap(t, back.waitFor(t, MIMsgPlayerReconnected))
	if int(recon["seat"].(float64)) != 1 {
		t.Fatalf("재접속 통지 = %v", recon)
	}
	restored := miPayloadMap(t, back.waitFor(t, MIMsgGameState))
	if int(restored["yourSeat"].(float64)) != 1 {
		t.Fatalf("복원 yourSeat = %v", restored["yourSeat"])
	}
	if got := miHandOf(restored); len(got) != 1 || got[0] != hand[0] {
		t.Fatalf("복원 손패 = %v, want %v", got, hand)
	}

	// 재접속한 좌석은 그대로 낼 수 있다
	back.send(t, MIMessage{Type: MIMsgPlay})
	after := miPayloadMap(t, back.waitMatch(t, "낸 뒤 스냅샷", func(m MIMessage) bool {
		if m.Type != MIMsgGameState {
			return false
		}
		s, ok := m.Payload.(map[string]interface{})
		if !ok {
			return false
		}
		last, _ := s["lastPlayed"].(float64)
		return int(last) == hand[0]
	}))
	if len(miHandOf(after)) != 0 {
		t.Fatalf("낸 뒤 손패 = %v", miHandOf(after))
	}

	// 모르는 세션은 만료 처리
	ghost := miDial(t, url)
	defer ghost.conn.Close()
	ghost.send(t, MIMessage{Type: MIMsgRejoin, Payload: MIRejoinPayload{SessionID: "없는-세션"}})
	ghost.waitFor(t, MIMsgSessionExpired)
}

// miStartBotGame 사람 호스트가 봇을 채워 시작한 뒤 스스로 빠져 **완전한
// 3봇 판**을 만든다. 관전자 연결로 판을 지켜본다 (사람이 남아 있으면 그
// 좌석이 침묵해 봇의 실력을 재는 데 방해가 된다).
func miStartBotGame(t *testing.T, url string) *miTestClient {
	t.Helper()

	host := miDial(t, url)
	joined := miJoin(t, host, "호스트", "NEW")
	code, _ := joined["roomCode"].(string)
	host.send(t, MIMessage{Type: MIMsgFillBots}) // 3인까지 채우고 즉시 시작
	host.miWaitPhase(t, MIPhaseReady)

	spec := miDial(t, url)
	spec.send(t, MIMessage{Type: MIMsgJoinGame,
		Payload: MIJoinGamePayload{Name: "구경꾼", Room: code}})
	spec.waitFor(t, MIMsgSpectateJoined)

	// 호스트가 빠지면 유예 만료 후 그 좌석도 봇이 이어받는다 → 3봇
	host.conn.Close()
	return spec
}

// miWaitGameOver 종료까지 읽는다. 공용 waitMatch 는 호출마다 3초 상한이라
// 한 판(수 초~수십 초)을 한 번에 기다릴 수 없어, 메시지를 계속 소비하며
// 종료를 기다린다 (진행 중에는 스냅샷이 끊임없이 오므로 상한에 걸리지 않는다).
func miWaitGameOver(t *testing.T, spec *miTestClient, limit time.Duration) map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		msg := spec.waitMatch(t, "진행 또는 종료", func(m MIMessage) bool { return true })
		if msg.Type == MIMsgGameOver {
			return miPayloadMap(t, msg)
		}
	}
	t.Fatalf("%v 안에 게임이 끝나지 않았다", limit)
	return nil
}

// TestMIBotTakeover 유예 만료 좌석을 봇이 이어받는지.
// 차례가 없어도 손패를 든 좌석이 침묵하면 라운드가 끝나지 않으므로
// 다른 게임과 같은 봇 대체를 그대로 둔다.
func TestMIBotTakeover(t *testing.T) {
	_, url, cleanup := newMITestServer(t, miBotTimings())
	defer cleanup()

	spec := miStartBotGame(t, url)
	defer spec.conn.Close()

	ev := miPayloadMap(t, spec.waitMatch(t, "bot_takeover", func(m MIMessage) bool {
		if m.Type != MIMsgEvent {
			return false
		}
		p, ok := m.Payload.(map[string]interface{})
		return ok && p["kind"] == "bot_takeover"
	}))
	if int(ev["seat"].(float64)) != 0 {
		t.Fatalf("봇 대체 좌석 = %v, want 0", ev["seat"])
	}
	if ev["name"] == nil || ev["name"] == "" {
		t.Fatalf("봇 대체 이벤트에 name 부재: %v", ev)
	}

	// 이어받은 봇이 실제로 카드를 낸다 (스스로 시계를 돌리는 근거)
	state := miPayloadMap(t, spec.waitMatch(t, "봇 좌석이 냈다", func(m MIMessage) bool {
		if m.Type != MIMsgGameState {
			return false
		}
		s, ok := m.Payload.(map[string]interface{})
		if !ok {
			return false
		}
		players, ok := s["players"].([]interface{})
		if !ok || len(players) == 0 {
			return false
		}
		seat0 := players[0].(map[string]interface{})
		return seat0["bot"] == true && int(seat0["handCount"].(float64)) == 0
	}))
	if _, ok := state["yourHand"]; ok {
		t.Fatalf("관전자에게 yourHand 가 실렸다: %v", state)
	}
}

// TestMIThreeBotsCompleteGame 3봇 판이 60초 안에 **반드시 끝나는지** —
// 가장 중요한 회귀 장치다. 차례가 없는 게임이라 "아무도 내지 않아 멈추는"
// 교착이 가장 무서운데, 봇이 스스로 시계를 돌리므로 판은 반드시 진행된다.
// 종료는 생명 소진(no_lives) 또는 최종 라운드 클리어여야 하며, 게임 캡
// (time_up)으로 끝나면 봇이 판을 굴리지 못했다는 뜻이라 실패로 잡는다.
func TestMIThreeBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newMITestServer(t, miBotTimings())
	defer cleanup()

	spec := miStartBotGame(t, url)
	defer spec.conn.Close()

	start := time.Now()
	deadline := start.Add(60 * time.Second)
	sawPlay, maxRound := 0, 0

	for time.Now().Before(deadline) {
		msg := spec.waitMatch(t, "state-event-or-over", func(m MIMessage) bool {
			return m.Type == MIMsgGameState || m.Type == MIMsgGameOver || m.Type == MIMsgEvent
		})

		if msg.Type == MIMsgEvent {
			ev := miPayloadMap(t, msg)
			if ev["kind"] == "play" {
				if name, _ := ev["name"].(string); name == "" {
					t.Fatalf("카드 이벤트에 name 부재: %v", ev)
				}
				if _, ok := ev["seat"].(float64); !ok {
					t.Fatalf("카드 이벤트에 seat 부재: %v", ev)
				}
				sawPlay++
			}
			continue
		}

		if msg.Type == MIMsgGameState {
			s := miPayloadMap(t, msg)
			if r, ok := s["round"].(float64); ok && int(r) > maxRound {
				maxRound = int(r)
			}
			if _, ok := s["yourHand"]; ok {
				t.Fatalf("관전자에게 yourHand 가 실렸다: %v", s)
			}
			continue
		}

		over := miPayloadMap(t, msg)
		reason, _ := over["reason"].(string)
		if reason != "no_lives" && reason != "cleared" {
			t.Fatalf("종료 사유 = %q — 봇이 판을 굴리지 못했다", reason)
		}
		if m, _ := over["message"].(string); m == "" {
			t.Fatalf("종료 문구 부재: %v", over)
		}
		cleared, _ := over["cleared"].(bool)
		if cleared != (reason == "cleared") {
			t.Fatalf("cleared=%v reason=%q 가 어긋난다", cleared, reason)
		}
		round := int(over["round"].(float64))
		if round < 1 || round > int(over["maxRound"].(float64)) {
			t.Fatalf("도달 라운드 = %d (최종 %v)", round, over["maxRound"])
		}
		players, ok := over["players"].([]interface{})
		if !ok || len(players) != MIFillBotTarget {
			t.Fatalf("players 길이 = %v, want %d", over["players"], MIFillBotTarget)
		}
		for _, raw := range players {
			p := raw.(map[string]interface{})
			if p["bot"] != true {
				t.Fatalf("3봇 판인데 봇이 아닌 좌석이 있다: %v", p)
			}
		}
		if sawPlay == 0 {
			t.Fatal("봇이 카드를 한 장도 내지 않았다 — 자체 시계가 돌지 않는다")
		}
		t.Logf("완주: %s (도달 %d/%v 라운드, 낸 카드 %d장, %.1fs)",
			reason, round, over["maxRound"], sawPlay, time.Since(start).Seconds())
		return
	}
	t.Fatal("60초 안에 게임이 끝나지 않았다 — 진행 불가 상태")
}

// TestMIGameCap 무한 게임 방지 캡 — 아무도 내지 않아도 제한 시간이 지나면
// 실패로 정산하고 끝난다.
func TestMIGameCap(t *testing.T) {
	tm := miSlowTimings()
	tm.gameCap = 200 * time.Millisecond
	_, url, cleanup := newMITestServer(t, tm)
	defer cleanup()

	clients := make([]*miTestClient, 2)
	for i := range clients {
		clients[i] = miDial(t, url)
		defer clients[i].conn.Close()
		miJoin(t, clients[i], fmt.Sprintf("P%d", i), "")
	}
	miDrain(clients[1])
	clients[0].send(t, MIMessage{Type: MIMsgStart})

	over := miPayloadMap(t, clients[0].waitFor(t, MIMsgGameOver))
	if over["reason"] != "time_up" {
		t.Fatalf("종료 사유 = %v, want time_up", over["reason"])
	}
	if over["cleared"] != false {
		t.Fatalf("캡 종료가 클리어로 기록됐다: %v", over)
	}
	if m, _ := over["message"].(string); !strings.Contains(m, "제한 시간") {
		t.Fatalf("캡 종료 문구 = %q", m)
	}
	if int(over["lives"].(float64)) != 2 {
		t.Fatalf("생명 = %v, want 2", over["lives"])
	}
}

// TestMIRoundCapAutoAdvance 라운드 캡 — 아무도 내지 않으면 자동으로 한 장이
// 나가고 생명이 1 깎인다 (무한 대기 방지).
func TestMIRoundCapAutoAdvance(t *testing.T) {
	tm := miSlowTimings()
	tm.roundCap = 150 * time.Millisecond
	_, url, cleanup := newMITestServer(t, tm)
	defer cleanup()

	clients := make([]*miTestClient, 2)
	for i := range clients {
		clients[i] = miDial(t, url)
		defer clients[i].conn.Close()
		miJoin(t, clients[i], fmt.Sprintf("P%d", i), "")
	}
	miDrain(clients[1])
	clients[0].send(t, MIMessage{Type: MIMsgStart})

	stalled := miPayloadMap(t, clients[0].waitMatch(t, "자동 진행", func(m MIMessage) bool {
		if m.Type != MIMsgGameState {
			return false
		}
		s, ok := m.Payload.(map[string]interface{})
		return ok && s["lastMistake"] != nil
	}))
	mistake := stalled["lastMistake"].(map[string]interface{})
	if burned, ok := mistake["burned"].([]interface{}); !ok || len(burned) != 0 {
		t.Fatalf("자동 진행에 소각이 있다: %v", mistake["burned"])
	}
	if int(mistake["played"].(float64)) <= 0 {
		t.Fatalf("자동 진행 카드 = %v", mistake["played"])
	}
	if int(stalled["lives"].(float64)) != 1 {
		t.Fatalf("자동 진행 후 생명 = %v, want 1", stalled["lives"])
	}
}

// ==================== 전적 ====================

// TestMIMatchRecordFormat 협력 전적 표기 — 클리어면 전원이 Winner 에 들어가
// 전원 승자가 되고, 실패면 어떤 닉네임과도 겹치지 않는 표식으로 전원 패자가
// 된다. Winner "" 는 무승부로 집계되므로 절대 쓰지 않는다.
func TestMIMatchRecordFormat(t *testing.T) {
	crew := []string{"가", "나", "다"}
	players := strings.Join(crew, "·")

	parsed := splitPlayers(players)
	if len(parsed) != 3 {
		t.Fatalf("참가자 파싱 = %v", parsed)
	}

	// 클리어 — 전원 승자
	winners := splitWinners(strings.Join(crew, "·"))
	if len(winners) != 3 {
		t.Fatalf("클리어 승자 파싱 = %v", winners)
	}

	// 실패 — 전원 패자 (표식이 참가자 누구와도 겹치지 않아야 한다)
	failWinners := splitWinners(miFailWinnerTag)
	if len(failWinners) != 1 || failWinners[0] != miFailWinnerTag {
		t.Fatalf("실패 표식 파싱 = %v", failWinners)
	}
	for _, name := range parsed {
		if name == miFailWinnerTag {
			t.Fatalf("실패 표식이 참가자 닉네임과 겹친다: %q", name)
		}
	}
	if splitWinners("") != nil {
		t.Fatal("빈 Winner 는 무승부여야 한다 — 협력 실패에 쓰면 안 된다")
	}
	if isBotName(miFailWinnerTag) {
		t.Fatal("실패 표식이 봇 닉네임으로 읽힌다")
	}
}

// ==================== 봇 품질 측정 ====================
//
// 더 크루에서 봇이 협력을 못 해 1단계에서 전멸하던 전례가 있어, 봇이 실제로
// 게임을 진행시키는지 숫자로 남긴다. 기본 실행에서는 건너뛰고
// MI_BOT_RUNS=30 처럼 판 수를 주면 돈다 (필요하면 MI_BOT_TICK 으로 대기
// 계수를 ms 단위로 바꾼다).
//
//	MI_BOT_RUNS=30 go test ./server -run TestMIBotQuality -timeout 20m -v

func TestMIBotQuality(t *testing.T) {
	runs, _ := strconv.Atoi(os.Getenv("MI_BOT_RUNS"))
	if runs <= 0 {
		t.Skip("MI_BOT_RUNS 가 없어 건너뜀 (봇 품질 측정 전용)")
	}
	if tick, _ := strconv.Atoi(os.Getenv("MI_BOT_TICK")); tick > 0 {
		miBotTickPerGap = time.Duration(tick) * time.Millisecond
	}

	tm := miBotTimings()
	tm.gameCap = 120 * time.Second
	_, url, cleanup := newMITestServer(t, tm)
	defer cleanup()

	dist := map[int]int{}
	cleared, sum, worst := 0, 0, 0
	for i := 0; i < runs; i++ {
		spec := miStartBotGame(t, url)
		over := miWaitGameOver(t, spec, 120*time.Second)
		spec.conn.Close()

		round := int(over["round"].(float64))
		dist[round]++
		sum += round
		if over["cleared"] == true {
			cleared++
		}
		if worst == 0 || round < worst {
			worst = round
		}
	}

	rounds := []int{}
	for r := range dist {
		rounds = append(rounds, r)
	}
	sort.Ints(rounds)
	parts := []string{}
	for _, r := range rounds {
		parts = append(parts, fmt.Sprintf("%d라운드:%d판", r, dist[r]))
	}
	t.Logf("3봇 %d판 (대기 계수 %v) — 클리어 %d판(%.0f%%), 평균 도달 %.2f라운드, 최저 %d라운드",
		runs, miBotTickPerGap, cleared, float64(cleared)*100/float64(runs),
		float64(sum)/float64(runs), worst)
	t.Logf("도달 라운드 분포 — %s", strings.Join(parts, " / "))

	if worst <= 1 {
		t.Errorf("1라운드에서 끝난 판이 있다 — 봇이 협력하지 못한다")
	}
}
