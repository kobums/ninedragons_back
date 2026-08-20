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

func init() {
	// AFK 타이머·라운드 전환을 짧게 — 완주·AFK 테스트가 수 초 안에 끝나는 근거.
	// 수동 조작 스텝(600ms 안에 한 수)보다 충분히 길어 결정성을 해치지 않는다.
	skAfkTimeout = 600 * time.Millisecond
	skRoundEndDelay = 40 * time.Millisecond
}

// skTestClient 공용 testConn 에 게임 메시지 타입의 waitFor 를 얹은 래퍼
type skTestClient struct {
	testConn[SKMessage]
}

func newSKTestServer(t *testing.T, grace time.Duration) (*SKHub, string, func()) {
	t.Helper()
	hub := NewSKHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeSKWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func skDial(t *testing.T, url string) *skTestClient {
	t.Helper()
	return &skTestClient{dialWS[SKMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *skTestClient) waitFor(t *testing.T, msgType SKMessageType) SKMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m SKMessage) bool { return m.Type == msgType })
}

// waitPhase 지정한 단계의 sk_game_state 가 올 때까지 읽는다
func (c *skTestClient) waitPhase(t *testing.T, phase SKPhase) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "state:"+string(phase), func(m SKMessage) bool {
		if m.Type != SKMsgGameState {
			return false
		}
		p, ok := m.Payload.(map[string]interface{})
		return ok && p["phase"] == string(phase)
	})
	return asPayloadMap(t, msg.Payload)
}

// waitTurnState phase 이면서 currentSeat 조건까지 맞는 스냅샷을 기다린다
func (c *skTestClient) waitTurnState(t *testing.T, phase SKPhase, match func(cur int) bool) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "turn:"+string(phase), func(m SKMessage) bool {
		if m.Type != SKMsgGameState {
			return false
		}
		p, ok := m.Payload.(map[string]interface{})
		if !ok || p["phase"] != string(phase) {
			return false
		}
		cur, ok := p["currentSeat"].(float64)
		return ok && match(int(cur))
	})
	return asPayloadMap(t, msg.Payload)
}

// skJoin 입장 → sk_player_joined 의 (seat, sessionId)
func skJoin(t *testing.T, c *skTestClient, name string) (int, string) {
	t.Helper()
	seat, session, _, _ := skJoinRoom(t, c, name, "")
	return seat, session
}

// skJoinRoom room 필드를 실어 입장 → (seat, sessionId, roomCode, gameId)
func skJoinRoom(t *testing.T, c *skTestClient, name, room string) (int, string, string, string) {
	t.Helper()
	c.send(t, SKMessage{Type: SKMsgJoinGame, Payload: SKJoinGamePayload{Name: name, Room: room}})
	joined := asPayloadMap(t, c.waitFor(t, SKMsgPlayerJoined).Payload)
	code, _ := joined["roomCode"].(string)
	return int(joined["yourSeat"].(float64)), joined["sessionId"].(string),
		code, joined["gameId"].(string)
}

// skRoseIndexOf 스냅샷의 yourHand 에서 장미 인덱스
func skRoseIndexOf(t *testing.T, state map[string]interface{}) int {
	t.Helper()
	hand, ok := state["yourHand"].([]interface{})
	if !ok {
		t.Fatalf("yourHand 가 배열이 아니다: %v", state["yourHand"])
	}
	for i, card := range hand {
		if card == "rose" {
			return i
		}
	}
	t.Fatalf("yourHand 에 장미가 없다: %v", hand)
	return -1
}

// nextMessage waitMatch 의 큐 소비를 testing.T 없이 수행한다 — 완주 테스트의
// 병렬 드라이버 고루틴은 t.Fatal 을 쓸 수 없어 에러를 돌려준다.
func (c *skTestClient) nextMessage(deadline time.Time) (SKMessage, error) {
	for len(c.queue) == 0 {
		if !time.Now().Before(deadline) {
			return SKMessage{}, fmt.Errorf("메시지 대기 시간 초과")
		}
		c.conn.SetReadDeadline(deadline)
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return SKMessage{}, err
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var msg SKMessage
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				return SKMessage{}, err
			}
			c.queue = append(c.queue, msg)
		}
	}
	msg := c.queue[0]
	c.queue = c.queue[1:]
	return msg, nil
}

type skDriveResult struct {
	over SKMessage
	err  error
}

// driveSK 소켓 하나를 봇 두뇌로 몰아 sk_game_over 까지 진행한다
func driveSK(c *skTestClient, brain *skBrain, deadline time.Time) skDriveResult {
	for {
		msg, err := c.nextMessage(deadline)
		if err != nil {
			return skDriveResult{err: err}
		}
		if msg.Type == SKMsgGameOver {
			return skDriveResult{over: msg}
		}
		if reply := brain.decide(msg); reply != nil {
			if err := c.conn.WriteJSON(*reply); err != nil {
				return skDriveResult{err: err}
			}
		}
	}
}

// TestSKSixBotsCompleteGame 6좌석 전부 봇 두뇌로 구동해 20초 안에 완주한다.
// 실패가 카드를 줄이고(최대 24장) 성공이 점수를 쌓아 유한 종료 —
// -count=3 으로 무작위 배치·배팅이 달라져도 회귀가 없음을 본다.
func TestSKSixBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newSKTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients := make([]*skTestClient, SKMaxPlayers)
	for i := range clients {
		clients[i] = skDial(t, url)
		defer clients[i].conn.Close()
		seat, _ := skJoin(t, clients[i], fmt.Sprintf("드라이버%d", i+1))
		if seat != i {
			t.Fatalf("좌석 = %d, want %d", seat, i)
		}
	}
	clients[0].send(t, SKMessage{Type: SKMsgStart, Payload: map[string]interface{}{}})

	deadline := time.Now().Add(20 * time.Second)
	results := make(chan skDriveResult, len(clients))
	for _, c := range clients {
		go func(c *skTestClient) { results <- driveSK(c, newSKBrain(), deadline) }(c)
	}

	for i := 0; i < len(clients); i++ {
		res := <-results
		if res.err != nil {
			t.Fatalf("완주 실패: %v", res.err)
		}
		over := asPayloadMap(t, res.over.Payload)
		winner := int(over["winnerSeat"].(float64))
		if winner < 0 || winner >= SKMaxPlayers {
			t.Fatalf("승자 좌석 이상: %v", over["winnerSeat"])
		}
		if over["winnerName"] == "" {
			t.Fatalf("승자 이름이 비어 있다: %v", over)
		}
		players := over["players"].([]interface{})
		if len(players) != SKMaxPlayers {
			t.Fatalf("종료 발표 인원 = %d", len(players))
		}
	}
}

// TestSKHiddenState 은닉 검증 — 스냅샷에는 본인 손패(yourHand/yourStack)만
// 실리고, players 항목에는 카드 내용 필드 자체가 없다. 빈 슬라이스는 []
// (null 금지), int 필드는 0 이어도 실린다.
func TestSKHiddenState(t *testing.T) {
	_, url, cleanup := newSKTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	n := SKMinPlayers
	clients := make([]*skTestClient, n)
	for i := range clients {
		clients[i] = skDial(t, url)
		defer clients[i].conn.Close()
		seat, _ := skJoin(t, clients[i], fmt.Sprintf("사람%d", i+1))
		if seat != i {
			t.Fatalf("좌석 = %d, want %d", seat, i)
		}
	}
	clients[0].send(t, SKMessage{Type: SKMsgStart, Payload: map[string]interface{}{}})

	for i, c := range clients {
		state := c.waitPhase(t, SKPhasePlacing)

		// 내 손패는 4장 (장미 3 + 해골 1)
		hand, ok := state["yourHand"].([]interface{})
		if !ok || len(hand) != SKHandSize {
			t.Fatalf("seat%d yourHand 이상: %v", i, state["yourHand"])
		}
		roses, skulls := 0, 0
		for _, card := range hand {
			switch card {
			case "rose":
				roses++
			case "skull":
				skulls++
			}
		}
		if roses != 3 || skulls != 1 {
			t.Fatalf("seat%d 손패 구성 이상: %v", i, hand)
		}
		// 내 더미는 빈 배열 (null 금지)
		if stack, ok := state["yourStack"].([]interface{}); !ok || len(stack) != 0 {
			t.Fatalf("seat%d yourStack 이상: %v", i, state["yourStack"])
		}
		if flipped, ok := state["flipped"].([]interface{}); !ok || len(flipped) != 0 {
			t.Fatalf("seat%d flipped 이상 (null 금지): %v", i, state["flipped"])
		}

		// int 필드는 0/-1 이어도 실린다
		if state["challengerSeat"].(float64) != -1 || state["currentSeat"].(float64) != -1 {
			t.Fatalf("seat%d 좌석 필드 이상: %v / %v", i, state["challengerSeat"], state["currentSeat"])
		}
		if hb, ok := state["highBid"].(float64); !ok || hb != 0 {
			t.Fatalf("seat%d highBid 이상: %v", i, state["highBid"])
		}
		if state["roundResult"] != nil {
			t.Fatalf("seat%d 시작 직후 roundResult: %v", i, state["roundResult"])
		}

		// players 에는 타인 카드 내용 필드가 아예 없어야 한다 (장수만)
		players := state["players"].([]interface{})
		if len(players) != n {
			t.Fatalf("players = %d명, want %d", len(players), n)
		}
		for _, p := range players {
			pm := p.(map[string]interface{})
			for _, leak := range []string{"hand", "stack", "cards", "yourHand", "yourStack"} {
				if _, exists := pm[leak]; exists {
					t.Fatalf("players 항목에 카드 내용 필드 %q 가 있다 — 은닉 위반: %v", leak, pm)
				}
			}
			if pm["handCount"].(float64) != float64(SKHandSize) {
				t.Fatalf("handCount 이상: %v", pm)
			}
			if _, exists := pm["stackCount"]; !exists {
				t.Fatalf("stackCount(0) 가 생략됐다 — omitempty 금지: %v", pm)
			}
		}
	}
}

// TestSKRoomCodeSpectate 방 코드 발급·대소문자 무시 입장·시작 후 코드 입장의
// 관전 전환·관전자 행동 차단·리액션 중계를 본다.
func TestSKRoomCodeSpectate(t *testing.T) {
	_, url, cleanup := newSKTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := skDial(t, url)
	defer host.conn.Close()
	_, _, code, hostGame := skJoinRoom(t, host, "방장", "NEW")
	if len(code) != roomCodeLen {
		t.Fatalf("발급 코드 = %q, want %d자", code, roomCodeLen)
	}

	// 소문자 코드로도 같은 방에 입장한다
	guests := make([]*skTestClient, 2)
	for i := range guests {
		guests[i] = skDial(t, url)
		defer guests[i].conn.Close()
		seat, _, gcode, gameID := skJoinRoom(t, guests[i], fmt.Sprintf("손님%d", i+1), strings.ToLower(code))
		if seat != i+1 || gcode != code || gameID != hostGame {
			t.Fatalf("코드 입장 이상: seat=%d code=%q game=%q", seat, gcode, gameID)
		}
	}

	host.send(t, SKMessage{Type: SKMsgStart, Payload: map[string]interface{}{}})
	state := host.waitPhase(t, SKPhasePlacing)
	if state["roomCode"] != code {
		t.Fatalf("시작 후 스냅샷 roomCode = %v, want %q", state["roomCode"], code)
	}

	// 시작된 방의 코드로 들어오면 관전자로 전환된다
	spec := skDial(t, url)
	defer spec.conn.Close()
	spec.send(t, SKMessage{Type: SKMsgJoinGame, Payload: SKJoinGamePayload{Name: "관전자", Room: code}})
	joined := asPayloadMap(t, spec.waitFor(t, SKMsgSpectateJoined).Payload)
	if joined["gameId"] != hostGame || joined["roomCode"] != code {
		t.Fatalf("spectate_joined payload 이상: %v", joined)
	}

	// 관전 스냅샷: yourSeat -1, 손패·더미는 빈 배열 (buildState(-1) 안전)
	specState := asPayloadMap(t, spec.waitFor(t, SKMsgGameState).Payload)
	if specState["yourSeat"].(float64) != -1 {
		t.Fatalf("관전자 yourSeat = %v, want -1", specState["yourSeat"])
	}
	if hand, ok := specState["yourHand"].([]interface{}); !ok || len(hand) != 0 {
		t.Fatalf("관전자 yourHand 이상 (빈 배열이어야 한다): %v", specState["yourHand"])
	}
	if stack, ok := specState["yourStack"].([]interface{}); !ok || len(stack) != 0 {
		t.Fatalf("관전자 yourStack 이상: %v", specState["yourStack"])
	}
	if specState["spectators"].(float64) != 1 {
		t.Fatalf("관전자 수 = %v, want 1", specState["spectators"])
	}

	// 관전자의 게임 행동은 전부 차단된다
	spec.send(t, SKMessage{Type: SKMsgBid, Payload: SKBidPayload{Count: 1}})
	errPayload := asPayloadMap(t, spec.waitFor(t, SKMsgError).Payload)
	if errPayload["message"] != spectatorDeniedMsg {
		t.Fatalf("관전자 차단 문구 = %v", errPayload["message"])
	}

	// 좌석 보유자의 리액션은 관전자에게도 중계된다
	host.send(t, SKMessage{Type: SKMsgReact, Payload: SKReactPayload{Emoji: "👍"}})
	react := spec.waitMatch(t, "react 이벤트", func(m SKMessage) bool {
		if m.Type != SKMsgEvent {
			return false
		}
		p, ok := m.Payload.(map[string]interface{})
		return ok && p["kind"] == "react"
	})
	rp := asPayloadMap(t, react.Payload)
	if rp["message"] != "👍" || int(rp["seat"].(float64)) != 0 {
		t.Fatalf("리액션 중계 이상: %v", rp)
	}
}

// TestSKAfkThreeStages AFK 3단계 회귀 — (1) 배팅 차례 방치는 자동 패스,
// (2) 뒤집기 방치는 무작위 더미 자동 뒤집기, (3) 배치 방치는 무작위 자동
// 배치로 해소되어 접속만 유지한 좌석이 게임을 영구 정지시키지 못한다.
func TestSKAfkThreeStages(t *testing.T) {
	_, url, cleanup := newSKTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	n := SKMinPlayers
	clients := make([]*skTestClient, n)
	for i := range clients {
		clients[i] = skDial(t, url)
		defer clients[i].conn.Close()
		seat, _ := skJoin(t, clients[i], fmt.Sprintf("사람%d", i+1))
		if seat != i {
			t.Fatalf("좌석 = %d, want %d", seat, i)
		}
	}
	clients[0].send(t, SKMessage{Type: SKMsgStart, Payload: map[string]interface{}{}})

	// ---- 1라운드는 수동으로 결정적으로 만든다 (전원 장미만 배치) ----
	for _, c := range clients {
		state := c.waitTurnState(t, SKPhasePlacing, func(cur int) bool { return cur == -1 })
		if state["endsAt"].(float64) <= 0 {
			t.Fatalf("placing endsAt = %v, want >0", state["endsAt"])
		}
		c.send(t, SKMessage{Type: SKMsgPlace, Payload: SKPlacePayload{Index: skRoseIndexOf(t, state)}})
	}
	// 턴제 파트 (선 = seat0): A 장미 → B 장미 → C 장미 → A 장미
	// 최종 더미: A=장미3, B=장미2, C=장미2 (총 7장)
	turnRose := func(c *skTestClient, seat int) {
		state := c.waitTurnState(t, SKPhasePlacing, func(cur int) bool { return cur == seat })
		c.send(t, SKMessage{Type: SKMsgPlace, Payload: SKPlacePayload{Index: skRoseIndexOf(t, state)}})
	}
	turnRose(clients[0], 0)
	turnRose(clients[1], 1)
	turnRose(clients[2], 2)
	turnRose(clients[0], 0)

	// B 차례에 장미 5장 선언 → 배팅 단계로
	clients[1].waitTurnState(t, SKPhasePlacing, func(cur int) bool { return cur == 1 })
	clients[1].send(t, SKMessage{Type: SKMsgBid, Payload: SKBidPayload{Count: 5}})

	// ---- 단계 1: 배팅 방치 → C·A 자동 패스 → B 가 도전자 ----
	flipping := clients[0].waitPhase(t, SKPhaseFlipping)
	if flipping["endsAt"].(float64) <= 0 {
		t.Fatalf("flipping endsAt = %v, want >0", flipping["endsAt"])
	}
	if int(flipping["challengerSeat"].(float64)) != 1 || int(flipping["highBid"].(float64)) != 5 {
		t.Fatalf("도전 확정 이상: %v / %v", flipping["challengerSeat"], flipping["highBid"])
	}
	players := flipping["players"].([]interface{})
	for _, s := range []int{0, 2} {
		if players[s].(map[string]interface{})["passed"] != true {
			t.Fatalf("seat%d 자동 패스 안 됨: %v", s, players[s])
		}
	}
	// 자기 더미(장미 2)는 자동으로 먼저 뒤집혀 있다 — 남은 목표 3장
	if got := flipping["flipped"].([]interface{}); len(got) != 2 {
		t.Fatalf("자기 더미 자동 뒤집기 = %d장, want 2", len(got))
	}
	if int(flipping["flipTarget"].(float64)) != 3 {
		t.Fatalf("flipTarget = %v, want 3", flipping["flipTarget"])
	}

	// ---- 단계 2: 뒤집기 방치 → 무작위 상대 더미 자동 뒤집기 (전부 장미 → 성공) ----
	roundEnd := clients[0].waitPhase(t, SKPhaseRoundEnd)
	rr, ok := roundEnd["roundResult"].(map[string]interface{})
	if !ok || rr["kind"] != "success" || int(rr["seat"].(float64)) != 1 {
		t.Fatalf("자동 뒤집기 성공 판정 이상: %v", roundEnd["roundResult"])
	}
	if roundEnd["players"].([]interface{})[1].(map[string]interface{})["points"].(float64) != 1 {
		t.Fatalf("도전 성공 점수 미반영: %v", roundEnd["players"])
	}

	// ---- 단계 3: 2라운드 배치 방치 → 전원 자동 배치 후 턴제 파트 진입 ----
	auto := clients[0].waitTurnState(t, SKPhasePlacing, func(cur int) bool { return cur != -1 })
	if int(auto["currentSeat"].(float64)) != 1 { // 성공한 도전자가 다음 라운드 선
		t.Fatalf("2라운드 선 = %v, want 1", auto["currentSeat"])
	}
	for _, p := range auto["players"].([]interface{}) {
		pm := p.(map[string]interface{})
		if pm["stackCount"].(float64) != 1 {
			t.Fatalf("자동 배치 안 된 좌석: %v", pm)
		}
	}
	if auto["endsAt"].(float64) <= 0 {
		t.Fatalf("자동 배치 후 endsAt = %v, want >0", auto["endsAt"])
	}
}
