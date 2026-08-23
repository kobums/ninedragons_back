package server

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"
)

// 테스트에서는 잠금 시간과 봇의 훑는 간격을 짧게 낮춘다 (5초 잠금·4~12초
// 간격은 실사용 값). 값은 init 에서 한 번만 정한다 — 테스트 도중에 바꾸면
// 허브·봇 고루틴과 경합한다(-race). 강제 종료 캡은 허브 필드라 테스트마다
// 따로 정한다.
func init() {
	seClaimLock = 150 * time.Millisecond
	seBotIntervalMin = 4 * time.Millisecond
	seBotIntervalMax = 14 * time.Millisecond
}

// seTestForceEnd 강제 종료로 끝나면 안 되는 테스트가 쓰는 넉넉한 캡
const seTestForceEnd = 90 * time.Second

// seTestClient 공용 testConn 에 세트 메시지 타입의 waitFor 를 얹은 래퍼
type seTestClient struct {
	testConn[SEMessage]
}

func newSETestServer(t *testing.T, grace, forceEnd time.Duration) (*SEHub, string, func()) {
	t.Helper()
	hub := NewSEHub()
	hub.grace = grace
	hub.forceEnd = forceEnd // Run 전에 정한다 (허브 고루틴과 경합 금지)
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeSEWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func seDial(t *testing.T, url string) *seTestClient {
	t.Helper()
	return &seTestClient{dialWS[SEMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *seTestClient) waitFor(t *testing.T, msgType SEMessageType) SEMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m SEMessage) bool { return m.Type == msgType })
}

func sePayloadMap(t *testing.T, msg SEMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// seJoin 입장하고 se_player_joined payload 를 돌려준다
func seJoin(t *testing.T, c *seTestClient, name, room string) map[string]interface{} {
	t.Helper()
	c.send(t, SEMessage{Type: SEMsgJoinGame, Payload: SEJoinGamePayload{Name: name, Room: room}})
	return sePayloadMap(t, c.waitFor(t, SEMsgPlayerJoined))
}

// seWaitPhase 지정한 phase 의 스냅샷이 올 때까지 소비
func (c *seTestClient) seWaitPhase(t *testing.T, phase string) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "se_game_state("+phase+")", func(m SEMessage) bool {
		if m.Type != SEMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		return ok && state["phase"] == phase
	})
	return sePayloadMap(t, msg)
}

// seDrain 배경으로 소켓을 비운다 (읽지 않는 연결의 버퍼 포화 방지)
func seDrain(c *seTestClient) {
	conn := c.conn
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

// seSeatClients 허브 고루틴 없이 핸들러를 직접 부르는 결정적 테스트용 —
// 소켓 없는 사람 좌석 n개를 앉힌 방을 만든다
func seSeatClients(t *testing.T, h *SEHub, room *seRoom, n int) []*SEClient {
	t.Helper()
	clients := make([]*SEClient, n)
	for i := range clients {
		c := &SEClient{wsClient: newBotWSClient(), Hub: h}
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

// seTake 소켓 없는 클라이언트의 Send 큐에 쌓인 메시지를 전부 꺼낸다
func seTake(t *testing.T, c *SEClient) []SEMessage {
	t.Helper()
	out := []SEMessage{}
	for {
		select {
		case data := <-c.Send:
			var msg SEMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			out = append(out, msg)
		default:
			return out
		}
	}
}

// seSetIDsOnBoard 스냅샷 바닥에서 세트 3장의 id 를 고른다
func seSetIDsOnBoard(board []SECard) ([]int, bool) {
	idx, ok := seFindSet(board)
	if !ok {
		return nil, false
	}
	return []int{board[idx[0]].ID, board[idx[1]].ID, board[idx[2]].ID}, true
}

// seBoardKey 바닥의 지문 — 같은 바닥에 두 번 claim 하지 않기 위한 키
func seBoardKey(board []SECard) string {
	ids := make([]int, 0, len(board))
	for _, c := range board {
		ids = append(ids, c.ID)
	}
	sort.Ints(ids)
	return fmt.Sprint(ids)
}

// TestSEFourBotsCompleteGame 봇을 채운 4인전이 60초 안에 완주하는지 —
// 가장 중요한 회귀 장치. 차례가 없는 게임이라 "아무도 움직이지 않아 멈추는"
// 교착이 가장 무서운데, 봇이 스스로 시계를 돌리므로 반드시 진행된다.
// 종료는 강제 종료 캡이 아니라 덱 소진(세트 고갈)이어야 한다.
func TestSEFourBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newSETestServer(t, defaultDisconnectGrace, seTestForceEnd)
	defer cleanup()

	c := seDial(t, url)
	defer c.conn.Close()
	seJoin(t, c, "사람", "")
	c.send(t, SEMessage{Type: SEMsgFillBots}) // 4인까지 채우고 즉시 시작

	start := time.Now()
	rng := rand.New(rand.NewSource(20260823))
	deadline := start.Add(60 * time.Second)
	lastKey := ""
	sawClaimEvent := false

	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "state-event-or-over", func(m SEMessage) bool {
			return m.Type == SEMsgGameState || m.Type == SEMsgGameOver || m.Type == SEMsgEvent
		})

		if msg.Type == SEMsgEvent {
			ev := sePayloadMap(t, msg)
			kind, _ := ev["kind"].(string)
			if kind == "claim_ok" || kind == "claim_fail" {
				// 판정 이벤트에는 누가 했는지가 반드시 실린다
				if name, _ := ev["name"].(string); name == "" {
					t.Fatalf("판정 이벤트에 name 부재: %v", ev)
				}
				if _, ok := ev["seat"].(float64); !ok {
					t.Fatalf("판정 이벤트에 seat 부재: %v", ev)
				}
				sawClaimEvent = true
			}
			continue
		}

		if msg.Type == SEMsgGameOver {
			over := sePayloadMap(t, msg)
			if reason, _ := over["reason"].(string); reason != "deck_empty" {
				t.Fatalf("종료 사유 = %q, want deck_empty (강제 종료로 끝나면 안 된다)", reason)
			}
			if m, _ := over["message"].(string); m == "" {
				t.Fatalf("종료 문구 부재: %v", over)
			}
			winners, ok := over["winnerSeats"].([]interface{})
			if !ok || len(winners) == 0 {
				t.Fatalf("승자 좌석 = %v", over["winnerSeats"])
			}
			if names, ok := over["winnerNames"].([]interface{}); !ok || len(names) != len(winners) {
				t.Fatalf("승자 이름 = %v", over["winnerNames"])
			}
			players, ok := over["players"].([]interface{})
			if !ok || len(players) != SEFillBotTarget {
				t.Fatalf("players 길이 = %v, want %d", over["players"], SEFillBotTarget)
			}
			sets := int(over["setsFound"].(float64))
			if sets <= 0 || sets > SEDeckSize/SEClaimSize {
				t.Fatalf("setsFound = %d", sets)
			}
			total, scorers, botScore := 0, 0, 0
			for _, raw := range players {
				p := raw.(map[string]interface{})
				score := int(p["score"].(float64))
				if score < 0 {
					t.Fatalf("점수가 0 미만이다: %v", p)
				}
				if score > 0 {
					scorers++
				}
				if p["bot"] == true {
					botScore += score
				}
				total += score
			}
			if total > sets {
				t.Fatalf("점수 합 %d 이 세트 수 %d 를 넘었다", total, sets)
			}
			if !sawClaimEvent {
				t.Fatal("판정 이벤트가 한 번도 없었다")
			}
			// 봇이 실제로 판에 참여했는지 — 사람 혼자 다 가져갔다면 봇의
			// 자체 시계가 돌지 않았다는 뜻이라 회귀로 잡는다
			if botScore == 0 || scorers < 2 {
				t.Fatalf("봇이 한 세트도 못 가져갔다 (득점자 %d명, 봇 점수 %d)", scorers, botScore)
			}
			t.Logf("완주: 세트 %d개, 점수합 %d (득점자 %d명, 봇 %d점, %.1fs)",
				sets, total, scorers, botScore, time.Since(start).Seconds())
			return
		}

		// 사람 좌석은 테스트가 몬다 — 잠금 중이 아니고 바닥이 바뀌었을 때만 집는다
		state, ok := botPayloadAs[seBotState](msg.Payload)
		if !ok || state.Phase != SEPhasePlaying || state.YourSeat < 0 {
			continue
		}
		locked := int64(0)
		for _, p := range state.Players {
			if p.Seat == state.YourSeat {
				locked = p.LockedUntil
			}
		}
		if locked > time.Now().UnixMilli() {
			continue
		}
		key := seBoardKey(state.Board)
		if key == lastKey {
			continue
		}
		lastKey = key
		// 사람도 봇과 비슷한 리듬으로 훑는다 — 쉬지 않고 집으면 봇이
		// 한 번도 끼어들지 못해 봇 경로가 검증되지 않는다
		time.Sleep(seBotIntervalMax)
		// 탐색 순서를 섞어 봇들과 늘 같은 세트만 다투지 않게 한다
		if ids, ok := seBotChoose(state.Board, false, rng); ok {
			c.send(t, SEMessage{Type: SEMsgClaim, Payload: SEClaimPayload{IDs: ids}})
		}
	}
	t.Fatal("60초 안에 게임이 끝나지 않았다 — 진행 불가 상태")
}

// TestSEPublicSnapshot 이 게임의 핵심 계약 — 은닉이 없다.
// 관전자(viewerSeat -1)의 스냅샷은 참가자의 것과 yourSeat 하나만 다르고
// 나머지 raw JSON 이 완전히 같아야 한다. 허브 고루틴 없이 핸들러를 직접
// 불러 결정적으로 검증한다.
func TestSEPublicSnapshot(t *testing.T) {
	h := NewSEHub()
	room := h.lobbyRoomFor("")
	clients := seSeatClients(t, h, room, 3)
	h.startGame(room)
	defer h.stopEndTimer(room)

	game := room.Game
	rawOf := func(viewer int) string {
		data, err := json.Marshal(h.buildSEState(room, viewer))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(data)
	}

	// ---- 관전자 스냅샷은 yourSeat 만 다르다 ----
	spectatorRaw := rawOf(-1)
	for _, seat := range []int{0, 1, 2} {
		want := strings.Replace(spectatorRaw,
			`"yourSeat":-1`, fmt.Sprintf(`"yourSeat":%d`, seat), 1)
		if rawOf(seat) != want {
			t.Fatalf("seat%d 스냅샷이 관전자 것과 yourSeat 말고도 다르다:\n%s\n%s",
				seat, rawOf(seat), spectatorRaw)
		}
	}
	if !strings.Contains(spectatorRaw, `"yourSeat":-1`) {
		t.Fatalf("관전자 yourSeat 가 -1 이 아니다:\n%s", spectatorRaw)
	}

	// ---- 공개 필드 계약 ----
	for _, want := range []string{
		`"lastClaim":null`, `"result":null`, `"setsFound":0`,
		fmt.Sprintf(`"deckLeft":%d`, SEDeckSize-len(game.Board)),
		`"score":0`, `"lockedUntil":0`, `"board":[{"id":`,
	} {
		if !strings.Contains(spectatorRaw, want) {
			t.Fatalf("스냅샷에 %s 부재:\n%s", want, spectatorRaw)
		}
	}
	spec := h.buildSEState(room, -1)
	if len(spec.Board) != len(game.Board) || spec.EndsAt <= 0 {
		t.Fatalf("바닥 %d장 / endsAt %d", len(spec.Board), spec.EndsAt)
	}
	if spec.Board[0].Shape == "" || spec.Board[0].Fill == "" ||
		spec.Board[0].Color == "" || spec.Board[0].Count == 0 {
		t.Fatalf("카드에 속성 4종이 다 실리지 않았다: %+v", spec.Board[0])
	}

	// ---- 빈 대기실 스냅샷도 빈 배열 [] (null 금지, 패닉 금지) ----
	empty := h.lobbyRoomFor("ZZZZ")
	emptyRaw, _ := json.Marshal(h.buildSEState(empty, -1))
	for _, want := range []string{
		`"players":[]`, `"board":[]`, `"deckLeft":0`, `"hostSeat":-1`,
		`"yourSeat":-1`, `"endsAt":0`, `"phase":"waiting"`,
	} {
		if !strings.Contains(string(emptyRaw), want) {
			t.Fatalf("빈 대기실 스냅샷에 %s 부재:\n%s", want, emptyRaw)
		}
	}

	// ---- 성립 claim: 점수·바닥·lastClaim 이 전원에게 똑같이 보인다 ----
	for _, c := range clients {
		seTake(t, c)
	}
	ids, ok := seSetIDsOnBoard(game.Board)
	if !ok {
		t.Fatal("시작 바닥에 세트가 없다")
	}
	h.handleGameMessage(SEGameMessage{Client: clients[1], Message: SEMessage{
		Type: SEMsgClaim, Payload: SEClaimPayload{IDs: ids}}})

	if game.Players[1].Score != 1 || game.SetsFound != 1 {
		t.Fatalf("성립 후 점수=%d setsFound=%d", game.Players[1].Score, game.SetsFound)
	}
	okRaw := rawOf(-1)
	if !strings.Contains(okRaw, `"lastClaim":{"seat":1,"name":"P1","ok":true`) {
		t.Fatalf("lastClaim 이 공개되지 않았다:\n%s", okRaw)
	}
	// 판정 이벤트에 이름이 실렸는지 (프론트 배너 근거)
	sawOK := false
	for _, msg := range seTake(t, clients[0]) {
		if msg.Type != SEMsgEvent {
			continue
		}
		ev := sePayloadMap(t, msg)
		if ev["kind"] == "claim_ok" {
			if ev["name"] != "P1" {
				t.Fatalf("claim_ok 이벤트 name = %v", ev["name"])
			}
			if int(ev["seat"].(float64)) != 1 {
				t.Fatalf("claim_ok 이벤트 seat = %v", ev["seat"])
			}
			sawOK = true
		}
	}
	if !sawOK {
		t.Fatal("claim_ok 이벤트가 방송되지 않았다")
	}

	// ---- 오답 claim: 감점·잠금이 전원에게 공개되고, 잠금 중 claim 은 에러 ----
	wrong := seWrongTriple(t, game.Board)
	h.handleGameMessage(SEGameMessage{Client: clients[0], Message: SEMessage{
		Type: SEMsgClaim, Payload: SEClaimPayload{IDs: wrong}}})
	if game.Players[0].LockedUntil == 0 {
		t.Fatal("오답인데 잠금이 걸리지 않았다")
	}
	failRaw := rawOf(-1)
	if !strings.Contains(failRaw, `"ok":false`) ||
		!strings.Contains(failRaw, fmt.Sprintf(`"lockedUntil":%d`, game.Players[0].LockedUntil)) {
		t.Fatalf("오답 결과가 공개되지 않았다:\n%s", failRaw)
	}
	// 잠금은 그 좌석에만
	if game.Players[2].LockedUntil != 0 {
		t.Fatalf("남의 오답에 seat2 가 잠겼다: %d", game.Players[2].LockedUntil)
	}

	seTake(t, clients[0])
	again, _ := seSetIDsOnBoard(game.Board)
	h.handleGameMessage(SEGameMessage{Client: clients[0], Message: SEMessage{
		Type: SEMsgClaim, Payload: SEClaimPayload{IDs: again}}})
	sawErr := false
	for _, msg := range seTake(t, clients[0]) {
		if msg.Type == SEMsgError {
			errMsg, _ := sePayloadMap(t, msg)["message"].(string)
			if !strings.Contains(errMsg, "잠금") {
				t.Fatalf("잠금 에러 문구 = %q", errMsg)
			}
			sawErr = true
		}
	}
	if !sawErr {
		t.Fatal("잠금 중 claim 이 에러로 거절되지 않았다")
	}
}

// TestSERoomCodeAndSpectate 방 코드 발급·코드 입장·시작 후 코드 관전 진입.
// 관전자는 yourSeat -1 만 다른 동일 스냅샷을 받고, 행동은 전부 차단된다.
func TestSERoomCodeAndSpectate(t *testing.T) {
	_, url, cleanup := newSETestServer(t, defaultDisconnectGrace, seTestForceEnd)
	defer cleanup()

	host := seDial(t, url)
	defer host.conn.Close()
	joined := seJoin(t, host, "호스트", "NEW")
	code, _ := joined["roomCode"].(string)
	if len(code) != roomCodeLen {
		t.Fatalf("roomCode = %q, want %d자", code, roomCodeLen)
	}

	guest := seDial(t, url)
	defer guest.conn.Close()
	g := seJoin(t, guest, "동료", code)
	if g["roomCode"] != code || int(g["yourSeat"].(float64)) != 1 {
		t.Fatalf("코드 입장 실패: %v", g)
	}

	host.send(t, SEMessage{Type: SEMsgStart})
	state := host.seWaitPhase(t, string(SEPhasePlaying))
	if state["roomCode"] != code || len(state["players"].([]interface{})) != 2 {
		t.Fatalf("시작 실패: %v", state)
	}
	board, ok := state["board"].([]interface{})
	if !ok || len(board) < SEBoardSize {
		t.Fatalf("시작 바닥 = %v", state["board"])
	}
	if ends := int64(state["endsAt"].(float64)); ends <= time.Now().UnixMilli() {
		t.Fatalf("endsAt = %d, want 미래의 unixMillis (강제 종료 캡)", ends)
	}
	if int(state["deckLeft"].(float64)) != SEDeckSize-len(board) {
		t.Fatalf("deckLeft = %v (바닥 %d장)", state["deckLeft"], len(board))
	}
	if state["lastClaim"] != nil || state["result"] != nil {
		t.Fatalf("시작 스냅샷 lastClaim=%v result=%v", state["lastClaim"], state["result"])
	}
	seDrain(guest)

	// 시작된 방의 코드로 들어오면 관전자
	spec := seDial(t, url)
	defer spec.conn.Close()
	spec.send(t, SEMessage{Type: SEMsgJoinGame,
		Payload: SEJoinGamePayload{Name: "구경꾼", Room: code}})
	specJoined := sePayloadMap(t, spec.waitFor(t, SEMsgSpectateJoined))
	if specJoined["roomCode"] != code {
		t.Fatalf("관전 입장 roomCode = %v", specJoined["roomCode"])
	}

	specState := sePayloadMap(t, spec.waitFor(t, SEMsgGameState))
	if int(specState["yourSeat"].(float64)) != -1 {
		t.Fatalf("관전자 yourSeat = %v, want -1", specState["yourSeat"])
	}
	// 은닉이 없다 — 관전자도 바닥·점수·잠금을 전부 본다
	specBoard, ok := specState["board"].([]interface{})
	if !ok || len(specBoard) != len(board) {
		t.Fatalf("관전자 바닥 = %v", specState["board"])
	}
	first := specBoard[0].(map[string]interface{})
	for _, key := range []string{"id", "shape", "count", "fill", "color"} {
		if _, ok := first[key]; !ok {
			t.Fatalf("관전자 카드에 %s 부재: %v", key, first)
		}
	}
	for _, pRaw := range specState["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		if _, ok := p["score"].(float64); !ok {
			t.Fatalf("관전자에게 score 부재: %v", p)
		}
		if _, ok := p["lockedUntil"].(float64); !ok {
			t.Fatalf("관전자에게 lockedUntil 부재: %v", p)
		}
	}
	if int(specState["spectators"].(float64)) != 1 {
		t.Fatalf("관전자 수 = %v", specState["spectators"])
	}

	// 관전자는 어떤 행동도 못 한다
	spec.send(t, SEMessage{Type: SEMsgClaim, Payload: SEClaimPayload{IDs: []int{0, 1, 2}}})
	errPayload := sePayloadMap(t, spec.waitFor(t, SEMsgError))
	if errPayload["message"] != spectatorDeniedMsg {
		t.Fatalf("관전자 차단 문구 = %v", errPayload["message"])
	}
}

// TestSEReconnect 재접속 3종 — 이탈 통지(se_player_disconnected) 후 세션으로
// 돌아오면 좌석·점수가 그대로 복원되고(se_player_reconnected), 모르는 세션은
// se_session_expired 로 거절된다.
func TestSEReconnect(t *testing.T) {
	_, url, cleanup := newSETestServer(t, 3*time.Second, seTestForceEnd)
	defer cleanup()

	conns := make([]*seTestClient, 2)
	sessions := make([]string, 2)
	for i := range conns {
		conns[i] = seDial(t, url)
		joined := seJoin(t, conns[i], fmt.Sprintf("P%d", i), "")
		sessions[i], _ = joined["sessionId"].(string)
	}
	defer conns[0].conn.Close()
	conns[0].send(t, SEMessage{Type: SEMsgStart})
	conns[0].seWaitPhase(t, string(SEPhasePlaying))

	// 좌석 1 이탈 → 남은 사람에게 이탈 통지
	conns[1].conn.Close()
	discon := sePayloadMap(t, conns[0].waitFor(t, SEMsgPlayerDisconnected))
	if int(discon["seat"].(float64)) != 1 || discon["name"] != "P1" {
		t.Fatalf("이탈 통지 = %v", discon)
	}
	if int(discon["graceSeconds"].(float64)) <= 0 {
		t.Fatalf("graceSeconds = %v", discon["graceSeconds"])
	}

	// 세션으로 재접속 → 좌석·바닥 복원
	back := seDial(t, url)
	defer back.conn.Close()
	back.send(t, SEMessage{Type: SEMsgRejoin, Payload: SERejoinPayload{SessionID: sessions[1]}})
	recon := sePayloadMap(t, back.waitFor(t, SEMsgPlayerReconnected))
	if int(recon["seat"].(float64)) != 1 {
		t.Fatalf("재접속 통지 = %v", recon)
	}
	restored := sePayloadMap(t, back.waitFor(t, SEMsgGameState))
	if int(restored["yourSeat"].(float64)) != 1 {
		t.Fatalf("복원 yourSeat = %v", restored["yourSeat"])
	}
	if b, ok := restored["board"].([]interface{}); !ok || len(b) < SEBoardSize {
		t.Fatalf("복원 스냅샷 바닥 = %v", restored["board"])
	}

	// 재접속한 좌석은 그대로 claim 할 수 있다
	var board []SECard
	raw, _ := json.Marshal(restored["board"])
	json.Unmarshal(raw, &board)
	if ids, ok := seSetIDsOnBoard(board); ok {
		back.send(t, SEMessage{Type: SEMsgClaim, Payload: SEClaimPayload{IDs: ids}})
		after := back.waitMatch(t, "성립 스냅샷", func(m SEMessage) bool {
			if m.Type != SEMsgGameState {
				return false
			}
			s, ok := m.Payload.(map[string]interface{})
			if !ok || s["lastClaim"] == nil {
				return false
			}
			return s["lastClaim"].(map[string]interface{})["ok"] == true
		})
		claim := sePayloadMap(t, after)["lastClaim"].(map[string]interface{})
		if int(claim["seat"].(float64)) != 1 {
			t.Fatalf("복원 좌석의 claim 이 다른 좌석으로 기록됐다: %v", claim)
		}
	}

	// 모르는 세션은 만료 처리
	ghost := seDial(t, url)
	defer ghost.conn.Close()
	ghost.send(t, SEMessage{Type: SEMsgRejoin, Payload: SERejoinPayload{SessionID: "없는-세션"}})
	ghost.waitFor(t, SEMsgSessionExpired)
}

// TestSEBotTakeover 유예 만료 좌석을 봇이 이어받는지 — 차례가 없어 진행이
// 막히지는 않지만, 빈 좌석이 남지 않도록 90초 봇 대체는 그대로 둔다.
func TestSEBotTakeover(t *testing.T) {
	_, url, cleanup := newSETestServer(t, 120*time.Millisecond, seTestForceEnd)
	defer cleanup()

	conns := make([]*seTestClient, 2)
	for i := range conns {
		conns[i] = seDial(t, url)
		defer conns[i].conn.Close()
		seJoin(t, conns[i], fmt.Sprintf("P%d", i), "")
	}
	conns[0].send(t, SEMessage{Type: SEMsgStart})
	conns[0].seWaitPhase(t, string(SEPhasePlaying))

	// 좌석 1 이탈 → 유예 만료 → 봇 대체
	conns[1].conn.Close()
	ev := sePayloadMap(t, conns[0].waitMatch(t, "bot_takeover", func(m SEMessage) bool {
		if m.Type != SEMsgEvent {
			return false
		}
		p, ok := m.Payload.(map[string]interface{})
		return ok && p["kind"] == "bot_takeover"
	}))
	if int(ev["seat"].(float64)) != 1 {
		t.Fatalf("봇 대체 좌석 = %v, want 1", ev["seat"])
	}
	if ev["name"] == nil || ev["name"] == "" {
		t.Fatalf("봇 대체 이벤트에 name 부재: %v", ev)
	}

	// 이어받은 봇이 실제로 판을 굴린다 (스스로 시계를 돌리는 근거)
	state := sePayloadMap(t, conns[0].waitMatch(t, "봇 좌석 판정", func(m SEMessage) bool {
		if m.Type != SEMsgGameState {
			return false
		}
		s, ok := m.Payload.(map[string]interface{})
		if !ok || s["lastClaim"] == nil {
			return false
		}
		return int(s["lastClaim"].(map[string]interface{})["seat"].(float64)) == 1
	}))
	players := state["players"].([]interface{})
	bot := players[1].(map[string]interface{})
	if bot["bot"] != true {
		t.Fatalf("좌석 1이 봇으로 표시되지 않았다: %v", bot)
	}
}

// TestSEForceEndCap 무한 게임 방지 캡 — 아무도 움직이지 않아도 제한 시간이
// 지나면 현재 점수로 정산하고 끝난다. 차례가 없어 AFK 자동 진행이 없는
// 이 게임의 유일한 종료 보증 타이머다. 1인 연습판도 함께 확인한다.
func TestSEForceEndCap(t *testing.T) {
	_, url, cleanup := newSETestServer(t, defaultDisconnectGrace, 150*time.Millisecond)
	defer cleanup()

	c := seDial(t, url)
	defer c.conn.Close()
	joined := seJoin(t, c, "혼자", "")
	if int(joined["yourSeat"].(float64)) != 0 {
		t.Fatalf("yourSeat = %v", joined["yourSeat"])
	}

	c.send(t, SEMessage{Type: SEMsgStart}) // 1인도 시작할 수 있다
	state := c.seWaitPhase(t, string(SEPhasePlaying))
	if len(state["players"].([]interface{})) != 1 {
		t.Fatalf("1인 연습판 인원 = %v", state["players"])
	}

	over := sePayloadMap(t, c.waitFor(t, SEMsgGameOver))
	if over["reason"] != "time_up" {
		t.Fatalf("종료 사유 = %v, want time_up", over["reason"])
	}
	if int(over["setsFound"].(float64)) != 0 {
		t.Fatalf("아무도 안 움직였는데 setsFound = %v", over["setsFound"])
	}
	winners, ok := over["winnerSeats"].([]interface{})
	if !ok || len(winners) != 1 || int(winners[0].(float64)) != 0 {
		t.Fatalf("강제 종료 승자 = %v", over["winnerSeats"])
	}
	if m, _ := over["message"].(string); !strings.Contains(m, "제한 시간") {
		t.Fatalf("강제 종료 문구 = %q", m)
	}
}

// TestSEMatchRecordFormat 전적 표기 — 동점 공동 승리는 "·" 로 이어야
// 전적 장부(splitWinners)가 전원을 승자로 읽는다. Winner "" 는 무승부다.
func TestSEMatchRecordFormat(t *testing.T) {
	all := []string{"가", "나", "다"}
	players := strings.Join(all, " vs ")
	winner := strings.Join([]string{"나", "다"}, "·")

	parsed := splitPlayers(players)
	if len(parsed) != 3 {
		t.Fatalf("참가자 파싱 = %v", parsed)
	}
	winners := splitWinners(winner)
	if len(winners) != 2 || winners[0] != "나" || winners[1] != "다" {
		t.Fatalf("공동 승자 파싱 = %v", winners)
	}
	if splitWinners("") != nil {
		t.Fatal("빈 Winner 는 무승부여야 한다")
	}
}
