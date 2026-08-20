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

// 테스트에서는 AFK 2단계 자동 진행 대기를 짧게 낮춘다
func init() {
	cnClueTimeout = 150 * time.Millisecond
	cnGuessTimeout = 120 * time.Millisecond
}

// cnTestClient 공용 testConn 에 코드네임 메시지 타입의 waitFor 를 얹은 래퍼
type cnTestClient struct {
	testConn[CNMessage]
}

func newCNTestServer(t *testing.T, grace time.Duration) (*CNHub, string, func()) {
	t.Helper()
	hub := NewCNHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeCNWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func cnDial(t *testing.T, url string) *cnTestClient {
	t.Helper()
	return &cnTestClient{dialWS[CNMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *cnTestClient) waitFor(t *testing.T, msgType CNMessageType) CNMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m CNMessage) bool { return m.Type == msgType })
}

// cnWaitPhase 지정한 phase 의 스냅샷이 올 때까지 소비
func (c *cnTestClient) cnWaitPhase(t *testing.T, phase string) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "cn_game_state("+phase+")", func(m CNMessage) bool {
		if m.Type != CNMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		return ok && state["phase"] == phase
	})
	return asPayloadMap(t, msg.Payload)
}

// cnJoin 입장하고 cn_player_joined payload 를 돌려준다
func cnJoin(t *testing.T, c *cnTestClient, name, room string) map[string]interface{} {
	t.Helper()
	c.send(t, CNMessage{Type: CNMsgJoinGame, Payload: CNJoinGamePayload{Name: name, Room: room}})
	return asPayloadMap(t, c.waitFor(t, CNMsgPlayerJoined).Payload)
}

// nextMessage waitMatch 의 큐 소비를 testing.T 없이 수행한다 — 완주 테스트의
// 병렬 드라이버 고루틴은 t.Fatal 을 쓸 수 없어 에러를 돌려준다.
func (c *cnTestClient) nextMessage(deadline time.Time) (CNMessage, error) {
	for len(c.queue) == 0 {
		if !time.Now().Before(deadline) {
			return CNMessage{}, fmt.Errorf("메시지 대기 시간 초과")
		}
		c.conn.SetReadDeadline(deadline)
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return CNMessage{}, err
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var msg CNMessage
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				return CNMessage{}, err
			}
			c.queue = append(c.queue, msg)
		}
	}
	msg := c.queue[0]
	c.queue = c.queue[1:]
	return msg, nil
}

type cnDriveResult struct {
	over CNMessage
	err  error
}

// driveCN 소켓 하나를 봇 두뇌로 몰아 cn_game_over 까지 진행한다
func driveCN(c *cnTestClient, brain *cnBrain, deadline time.Time) cnDriveResult {
	for {
		msg, err := c.nextMessage(deadline)
		if err != nil {
			return cnDriveResult{err: err}
		}
		if msg.Type == CNMsgGameOver {
			return cnDriveResult{over: msg}
		}
		if reply := brain.decide(msg); reply != nil {
			if err := c.conn.WriteJSON(*reply); err != nil {
				return cnDriveResult{err: err}
			}
		}
	}
}

// TestCNEightBotsCompleteGame 8좌석 전부 봇 두뇌로 구동해 20초 안에 완주한다.
// 무작위 선택이라도 매 선택이 카드를 까므로 유한 종료(암살자 조기 종료 포함) —
// -count=3 으로 셔플·선택 무작위성이 달라져도 회귀가 없음을 본다.
func TestCNEightBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newCNTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients := make([]*cnTestClient, CNMaxPlayers)
	for i := range clients {
		clients[i] = cnDial(t, url)
		defer clients[i].conn.Close()
		joined := cnJoin(t, clients[i], fmt.Sprintf("드라이버%d", i+1), "")
		if seat := int(joined["yourSeat"].(float64)); seat != i {
			t.Fatalf("좌석 = %d, want %d", seat, i)
		}
	}
	clients[0].send(t, CNMessage{Type: CNMsgStart})

	deadline := time.Now().Add(20 * time.Second)
	results := make(chan cnDriveResult, len(clients))
	for _, c := range clients {
		go func(c *cnTestClient) { results <- driveCN(c, newCNBrain(), deadline) }(c)
	}

	for i := 0; i < len(clients); i++ {
		res := <-results
		if res.err != nil {
			t.Fatalf("완주 실패: %v", res.err)
		}
		over := asPayloadMap(t, res.over.Payload)
		winner := over["winner"]
		if winner != "red" && winner != "blue" {
			t.Fatalf("승자 이상: %v", winner)
		}
		if reason := over["loseReason"]; reason != "" && reason != "assassin" {
			t.Fatalf("loseReason 이상: %v", reason)
		}
		players := over["players"].([]interface{})
		if len(players) != CNMaxPlayers {
			t.Fatalf("종료 발표 인원 = %d", len(players))
		}
	}
}

// TestCNHiddenKeyCard 은닉 검증 — keyCard 는 스파이마스터에게만 실리고
// 요원의 raw 스냅샷에는 필드 자체가 없다. 미공개 카드의 color 는 빈 값.
func TestCNHiddenKeyCard(t *testing.T) {
	_, url, cleanup := newCNTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	n := CNMinPlayers
	clients := make([]*cnTestClient, n)
	for i := range clients {
		clients[i] = cnDial(t, url)
		defer clients[i].conn.Close()
		cnJoin(t, clients[i], fmt.Sprintf("요원%d", i+1), "")
	}
	clients[0].send(t, CNMessage{Type: CNMsgStart})

	// 4인 전원 사람: 팀 적/청/적/청, 스파이마스터 seat0(적)·seat1(청)
	wantTeam := []string{"red", "blue", "red", "blue"}
	wantRole := []string{"spymaster", "spymaster", "agent", "agent"}
	for i, c := range clients {
		state := c.cnWaitPhase(t, string(CNPhaseClue))
		if int(state["yourSeat"].(float64)) != i {
			t.Fatalf("클라 %d: yourSeat = %v", i, state["yourSeat"])
		}
		if state["yourTeam"] != wantTeam[i] || state["yourRole"] != wantRole[i] {
			t.Fatalf("클라 %d: team/role = %v/%v, want %s/%s",
				i, state["yourTeam"], state["yourRole"], wantTeam[i], wantRole[i])
		}
		if state["currentTeam"] != "red" {
			t.Fatalf("클라 %d: currentTeam = %v", i, state["currentTeam"])
		}
		if ends := int64(state["endsAt"].(float64)); ends <= 0 {
			t.Fatalf("클라 %d: endsAt = %d, want unixMillis", i, ends)
		}

		// 은닉의 핵심: keyCard 는 스파이마스터 raw 에만 존재한다
		keyRaw, has := state["keyCard"]
		if wantRole[i] == "spymaster" {
			key, ok := keyRaw.([]interface{})
			if !has || !ok || len(key) != CNBoardSize {
				t.Fatalf("클라 %d(스파이마스터): keyCard = %v", i, keyRaw)
			}
			count := map[string]int{}
			for _, colorRaw := range key {
				count[colorRaw.(string)]++
			}
			if count["red"] != CNRedWords || count["blue"] != CNBlueWords ||
				count["neutral"] != CNNeutralWords || count["assassin"] != 1 {
				t.Fatalf("클라 %d: keyCard 구성 = %v", i, count)
			}
		} else if has {
			t.Fatalf("클라 %d(요원): keyCard 유출: %v", i, keyRaw)
		}

		board := state["board"].([]interface{})
		if len(board) != CNBoardSize {
			t.Fatalf("클라 %d: board %d칸", i, len(board))
		}
		for j, cardRaw := range board {
			card := cardRaw.(map[string]interface{})
			if card["revealed"].(bool) {
				t.Fatalf("클라 %d: 시작부터 공개된 카드 %d", i, j)
			}
			if card["color"] != "" { // 미공개 카드의 색 유출 감지
				t.Fatalf("클라 %d: board[%d].color = %v, want \"\"", i, j, card["color"])
			}
			if card["word"].(string) == "" {
				t.Fatalf("클라 %d: board[%d] 단어가 비었다", i, j)
			}
		}
		if hist, ok := state["clueHistory"].([]interface{}); !ok || len(hist) != 0 {
			t.Fatalf("클라 %d: clueHistory = %v (nil 금지·시작 시 빈 배열)", i, state["clueHistory"])
		}
		if state["clue"] != nil {
			t.Fatalf("클라 %d: clue = %v, want null", i, state["clue"])
		}
		if int(state["redLeft"].(float64)) != CNRedWords || int(state["blueLeft"].(float64)) != CNBlueWords {
			t.Fatalf("클라 %d: 잔여 = %v/%v", i, state["redLeft"], state["blueLeft"])
		}
	}
}

// TestCNAfkTwoStage AFK 2단계 — 스파이마스터가 힌트를 안 내면 봇 힌트("힌트"),
// 요원들이 카드를 안 고르면 턴 종료 처리 (적 → 청 교대 확인)
func TestCNAfkTwoStage(t *testing.T) {
	_, url, cleanup := newCNTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients := make([]*cnTestClient, CNMinPlayers)
	for i := range clients {
		clients[i] = cnDial(t, url)
		defer clients[i].conn.Close()
		cnJoin(t, clients[i], fmt.Sprintf("잠수%d", i+1), "")
	}
	clients[0].send(t, CNMessage{Type: CNMsgStart})
	watcher := clients[3] // 청팀 요원 — 전원 무행동으로 관찰만 한다

	// 1단계: 적 스파이마스터 힌트 무응답 → 봇 힌트로 guess 진입
	afk1 := asPayloadMap(t, watcher.waitMatch(t, "afk-clue", func(m CNMessage) bool {
		if m.Type != CNMsgEvent {
			return false
		}
		ev, ok := m.Payload.(map[string]interface{})
		return ok && ev["kind"] == "afk"
	}).Payload)
	if !strings.Contains(afk1["message"].(string), "자동 힌트") {
		t.Fatalf("1단계 AFK 문구 = %v", afk1["message"])
	}
	state := watcher.cnWaitPhase(t, string(CNPhaseGuess))
	clue, ok := state["clue"].(map[string]interface{})
	if !ok || clue["word"] != "힌트" {
		t.Fatalf("봇 힌트 = %v", state["clue"])
	}
	if count := int(clue["count"].(float64)); count < 1 || count > 2 {
		t.Fatalf("봇 힌트 숫자 = %d, want 1~2", count)
	}
	if remaining := int(clue["remaining"].(float64)); remaining != int(clue["count"].(float64))+1 {
		t.Fatalf("remaining = %d (count=%v)", remaining, clue["count"])
	}
	if hist := state["clueHistory"].([]interface{}); len(hist) != 1 {
		t.Fatalf("clueHistory = %v", state["clueHistory"])
	}

	// 2단계: 적 요원 선택 무응답 → 턴 종료 처리, 청팀 clue 단계로
	afk2 := asPayloadMap(t, watcher.waitMatch(t, "afk-guess", func(m CNMessage) bool {
		if m.Type != CNMsgEvent {
			return false
		}
		ev, ok := m.Payload.(map[string]interface{})
		return ok && ev["kind"] == "afk" && strings.Contains(ev["message"].(string), "턴을 넘깁니다")
	}).Payload)
	if !strings.Contains(afk2["message"].(string), "적팀") {
		t.Fatalf("2단계 AFK 문구 = %v", afk2["message"])
	}
	state = watcher.waitMatch(t, "blue-clue", func(m CNMessage) bool {
		if m.Type != CNMsgGameState {
			return false
		}
		s, ok := m.Payload.(map[string]interface{})
		return ok && s["phase"] == string(CNPhaseClue) && s["currentTeam"] == "blue"
	}).Payload.(map[string]interface{})
	if state["clue"] != nil {
		t.Fatalf("턴 교대 후 clue = %v, want null", state["clue"])
	}
	if ends := int64(state["endsAt"].(float64)); ends <= 0 {
		t.Fatalf("clue 단계 endsAt = %d", ends)
	}
}

// TestCNFillBotsAndBotSpymaster 봇 채우기(6인, 자동 시작 없음)와 역할 규칙 —
// 사람이 1명뿐인 팀은 봇이 스파이마스터가 되어 시작 즉시 무작위 힌트를 낸다.
// 봇전은 순식간에 끝날 수 있으므로 호스트 자신의 수신 스트림으로만 검증한다.
func TestCNFillBotsAndBotSpymaster(t *testing.T) {
	_, url, cleanup := newCNTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := cnDial(t, url)
	defer host.conn.Close()
	cnJoin(t, host, "호스트", "")
	guest := cnDial(t, url)
	defer guest.conn.Close()
	cnJoin(t, guest, "친구", "")

	// 봇 채우기 — 6인까지 채우고 시작하지 않는다 (호스트 명시 시작)
	host.send(t, CNMessage{Type: CNMsgFillBots})
	state := host.waitMatch(t, "filled-waiting", func(m CNMessage) bool {
		if m.Type != CNMsgGameState {
			return false
		}
		s, ok := m.Payload.(map[string]interface{})
		if !ok || s["phase"] != string(CNPhaseWaiting) {
			return false
		}
		players, ok := s["players"].([]interface{})
		return ok && len(players) == CNBotFillTarget
	}).Payload.(map[string]interface{})
	if board := state["board"].([]interface{}); len(board) != 0 {
		t.Fatalf("waiting board = %d칸, want 0 (빈 배열)", len(board))
	}
	for _, pRaw := range state["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		seat := int(p["seat"].(float64))
		wantBot := seat >= 2
		if p["bot"].(bool) != wantBot {
			t.Fatalf("seat%d bot = %v", seat, p["bot"])
		}
		// 사람이 1명뿐인 팀 — 봇(seat2/3)이 스파이마스터, 사람은 요원
		wantRole := "agent"
		if seat == 2 || seat == 3 {
			wantRole = "spymaster"
		}
		if p["role"] != wantRole {
			t.Fatalf("seat%d role = %v, want %s", seat, p["role"], wantRole)
		}
	}

	// 호스트 시작 → 봇 스파이마스터가 즉시 무작위 힌트("힌트 N=1~2")를 낸다
	host.send(t, CNMessage{Type: CNMsgStart})
	guessState := host.cnWaitPhase(t, string(CNPhaseGuess))
	clue, ok := guessState["clue"].(map[string]interface{})
	if !ok || clue["word"] != "힌트" {
		t.Fatalf("봇 스파이마스터 힌트 = %v", guessState["clue"])
	}
	if count := int(clue["count"].(float64)); count < 1 || count > 2 {
		t.Fatalf("봇 힌트 숫자 = %d, want 1~2", count)
	}
}

// TestCNRoomCodeAndSpectate 방 코드 발급·코드 입장·시작 후 코드 관전 진입.
// 관전자 스냅샷은 yourSeat -1 에 keyCard 부재 (공개 보드만).
// 4인 전원 사람(무행동)이라 게임이 끝나지 않아 관전 진입이 결정적이다.
func TestCNRoomCodeAndSpectate(t *testing.T) {
	_, url, cleanup := newCNTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := cnDial(t, url)
	defer host.conn.Close()
	joined := cnJoin(t, host, "호스트", "NEW")
	code, _ := joined["roomCode"].(string)
	if len(code) != roomCodeLen {
		t.Fatalf("roomCode = %q, want %d자", code, roomCodeLen)
	}

	guests := make([]*cnTestClient, 3)
	for i := range guests {
		guests[i] = cnDial(t, url)
		defer guests[i].conn.Close()
		guestJoined := cnJoin(t, guests[i], fmt.Sprintf("친구%d", i+1), code)
		if guestJoined["roomCode"] != code || int(guestJoined["yourSeat"].(float64)) != i+1 {
			t.Fatalf("코드 입장 실패: %v", guestJoined)
		}
	}

	host.send(t, CNMessage{Type: CNMsgStart})
	state := host.cnWaitPhase(t, string(CNPhaseClue))
	if state["roomCode"] != code || len(state["players"].([]interface{})) != 4 {
		t.Fatalf("시작 실패: %v", state["players"])
	}

	// 시작된 방의 코드로 들어오면 관전자
	spec := cnDial(t, url)
	defer spec.conn.Close()
	spec.send(t, CNMessage{Type: CNMsgJoinGame, Payload: CNJoinGamePayload{Name: "구경꾼", Room: code}})
	specJoined := asPayloadMap(t, spec.waitFor(t, CNMsgSpectateJoined).Payload)
	if specJoined["roomCode"] != code {
		t.Fatalf("관전 입장 roomCode = %v", specJoined["roomCode"])
	}

	specState := asPayloadMap(t, spec.waitFor(t, CNMsgGameState).Payload)
	if int(specState["yourSeat"].(float64)) != -1 {
		t.Fatalf("관전자 yourSeat = %v, want -1", specState["yourSeat"])
	}
	if specState["yourTeam"] != "" || specState["yourRole"] != "" {
		t.Fatalf("관전자 team/role = %v/%v, want 빈 값", specState["yourTeam"], specState["yourRole"])
	}
	if _, leaked := specState["keyCard"]; leaked {
		t.Fatalf("관전자에게 keyCard 유출: %v", specState["keyCard"])
	}
	if board := specState["board"].([]interface{}); len(board) != CNBoardSize {
		t.Fatalf("관전자 board = %d칸", len(board))
	}
	if len(specState["players"].([]interface{})) != 4 {
		t.Fatalf("관전자 스냅샷 players = %v", specState["players"])
	}

	// 관전자는 어떤 행동도 못 한다
	spec.send(t, CNMessage{Type: CNMsgPick, Payload: CNPickPayload{Index: 0}})
	errPayload := asPayloadMap(t, spec.waitFor(t, CNMsgError).Payload)
	if errPayload["message"] != spectatorDeniedMsg {
		t.Fatalf("관전자 차단 문구 = %v", errPayload["message"])
	}

	// 좌석 보유자의 리액션은 이벤트로 전파된다 (화이트리스트 이모지)
	guests[0].send(t, CNMessage{Type: CNMsgReact, Payload: CNReactPayload{Emoji: "🔥"}})
	react := asPayloadMap(t, host.waitMatch(t, "react-event", func(m CNMessage) bool {
		if m.Type != CNMsgEvent {
			return false
		}
		ev, ok := m.Payload.(map[string]interface{})
		return ok && ev["kind"] == "react"
	}).Payload)
	if react["message"] != "🔥" || int(react["seat"].(float64)) != 1 {
		t.Fatalf("리액션 이벤트 = %v", react)
	}
}
