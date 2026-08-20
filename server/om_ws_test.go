package server

import (
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// omTestClient 공용 testConn 에 게임 메시지 타입의 waitFor 를 얹은 래퍼
type omTestClient struct {
	testConn[OMMessage]
}

func newOMTestServer(t *testing.T, grace time.Duration) (*OMHub, string, func()) {
	t.Helper()
	hub := NewOMHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeOMWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func omDial(t *testing.T, url string) *omTestClient {
	t.Helper()
	return &omTestClient{dialWS[OMMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *omTestClient) waitFor(t *testing.T, msgType OMMessageType) OMMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m OMMessage) bool { return m.Type == msgType })
}

func omPayloadMap(t *testing.T, msg OMMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// startOMGame 2인 입장 → play 진입. 각자의 세션과 첫 스냅샷을 반환.
func startOMGame(t *testing.T, url string) ([]*omTestClient, []string, []map[string]interface{}) {
	t.Helper()
	clients := []*omTestClient{omDial(t, url), omDial(t, url)}
	sessions := make([]string, 2)
	for i, c := range clients {
		c.send(t, OMMessage{Type: OMMsgJoinGame, Payload: OMJoinGamePayload{PlayerName: string(rune('A' + i))}})
		joined := omPayloadMap(t, c.waitFor(t, OMMsgPlayerJoined))
		sessions[i] = joined["sessionId"].(string)
	}

	states := make([]map[string]interface{}, 2)
	for i, c := range clients {
		states[i] = omPayloadMap(t, c.waitFor(t, OMMsgGameState))
	}
	return clients, sessions, states
}

// omBlackIdx yourColor 가 black 인 클라이언트 인덱스
func omBlackIdx(t *testing.T, states []map[string]interface{}) int {
	t.Helper()
	for i, s := range states {
		if s["yourColor"] == string(OMBlack) {
			return i
		}
	}
	t.Fatal("흑 클라이언트를 찾을 수 없다")
	return -1
}

// TestOMStateSnapshot 시작 스냅샷: 15×15 빈 보드, 흑 선공, lastMove null
func TestOMStateSnapshot(t *testing.T) {
	_, url, cleanup := newOMTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, _, states := startOMGame(t, url)
	defer func() {
		for _, c := range clients {
			c.conn.Close()
		}
	}()

	if states[0]["yourColor"] == states[1]["yourColor"] {
		t.Fatal("두 클라이언트의 돌 색이 같다")
	}
	for i, state := range states {
		if state["currentColor"] != string(OMBlack) {
			t.Fatalf("클라 %d: currentColor = %v, want black", i, state["currentColor"])
		}
		if state["blackName"] == "" || state["whiteName"] == "" {
			t.Fatalf("클라 %d: 이름이 비어 있다", i)
		}
		if state["moveCount"].(float64) != 0 {
			t.Fatalf("클라 %d: moveCount = %v, want 0", i, state["moveCount"])
		}
		lastMove, ok := state["lastMove"]
		if !ok || lastMove != nil {
			t.Fatalf("클라 %d: lastMove = %v, want null (키 생략 금지)", i, lastMove)
		}
		if state["opponentConnected"] != true {
			t.Fatalf("클라 %d: opponentConnected = %v", i, state["opponentConnected"])
		}
		board := state["board"].([]interface{})
		if len(board) != OMBoardSize {
			t.Fatalf("클라 %d: 보드 행 %d개, want %d", i, len(board), OMBoardSize)
		}
		for r, row := range board {
			cells := row.([]interface{})
			if len(cells) != OMBoardSize {
				t.Fatalf("클라 %d: %d행 열 %d개, want %d", i, r, len(cells), OMBoardSize)
			}
			for c, v := range cells {
				if v.(float64) != 0 {
					t.Fatalf("클라 %d: 시작 보드 (%d,%d) = %v, want 0", i, r, c, v)
				}
			}
		}
	}
}

// TestOMMoveAndRejoin 착수 이벤트·상태 반영과 재접속 복원 검증
func TestOMMoveAndRejoin(t *testing.T) {
	_, url, cleanup := newOMTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, sessions, states := startOMGame(t, url)
	black := omBlackIdx(t, states)
	white := 1 - black
	defer clients[black].conn.Close()

	clients[black].send(t, OMMessage{Type: OMMsgMove, Payload: OMMovePayload{Row: 7, Col: 7}})
	for _, c := range clients {
		ev := omPayloadMap(t, c.waitFor(t, OMMsgEvent))
		if ev["kind"] != "placed" || ev["seat"] != string(OMBlack) {
			t.Fatalf("이벤트 = %v, want placed/black", ev)
		}
		if ev["name"] == "" || ev["message"] == "" {
			t.Fatalf("이벤트에 name·message 가 없다: %v", ev)
		}
		state := omPayloadMap(t, c.waitFor(t, OMMsgGameState))
		board := state["board"].([]interface{})
		if board[7].([]interface{})[7].(float64) != 1 {
			t.Fatalf("착수가 보드에 반영되지 않음: %v", board[7])
		}
		if state["moveCount"].(float64) != 1 || state["currentColor"] != string(OMWhite) {
			t.Fatalf("착수 후 상태 이상: moveCount=%v current=%v", state["moveCount"], state["currentColor"])
		}
		lastMove := state["lastMove"].(map[string]interface{})
		if lastMove["row"].(float64) != 7 || lastMove["col"].(float64) != 7 {
			t.Fatalf("lastMove = %v, want (7,7)", lastMove)
		}
	}

	// 백이 끊고 재접속 → 착수 포함 스냅샷 복원
	clients[white].conn.Close()
	clients[black].waitFor(t, OMMsgOpponentDisconnected)

	rejoined := omDial(t, url)
	defer rejoined.conn.Close()
	rejoined.send(t, OMMessage{Type: OMMsgRejoinGame, Payload: OMRejoinGamePayload{SessionID: sessions[white]}})
	clients[black].waitFor(t, OMMsgOpponentReconnected)

	state := omPayloadMap(t, rejoined.waitFor(t, OMMsgGameState))
	if state["yourColor"] != string(OMWhite) {
		t.Fatalf("복원 yourColor = %v, want white", state["yourColor"])
	}
	board := state["board"].([]interface{})
	if board[7].([]interface{})[7].(float64) != 1 {
		t.Fatalf("복원 스냅샷에 착수가 없다: %v", board[7])
	}
}

// TestOMWinAndRematch 5목 승리 통보(line 포함)와 재대결 재시작 검증
func TestOMWinAndRematch(t *testing.T) {
	_, url, cleanup := newOMTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, _, states := startOMGame(t, url)
	defer func() {
		for _, c := range clients {
			c.conn.Close()
		}
	}()
	black := omBlackIdx(t, states)
	white := 1 - black

	// 흑 (7,3)~(7,7) 가로 5목, 백은 떨어진 자리
	moves := []struct {
		idx  int
		cell OMCell
	}{
		{black, OMCell{7, 3}}, {white, OMCell{0, 0}},
		{black, OMCell{7, 4}}, {white, OMCell{0, 2}},
		{black, OMCell{7, 5}}, {white, OMCell{0, 4}},
		{black, OMCell{7, 6}}, {white, OMCell{0, 6}},
		{black, OMCell{7, 7}},
	}
	for _, m := range moves {
		clients[m.idx].send(t, OMMessage{Type: OMMsgMove, Payload: OMMovePayload{Row: m.cell.Row, Col: m.cell.Col}})
		// 순서 보장: 양쪽 모두 상태 반영을 확인한 뒤 다음 수
		for _, c := range clients {
			c.waitFor(t, OMMsgGameState)
		}
	}

	blackName := states[black]["blackName"]
	for _, c := range clients {
		over := omPayloadMap(t, c.waitFor(t, OMMsgGameOver))
		if over["winner"] != string(OMBlack) || over["reason"] != "five" {
			t.Fatalf("game_over = %v, want black/five", over)
		}
		if over["winnerName"] != blackName {
			t.Fatalf("winnerName = %v, want %v", over["winnerName"], blackName)
		}
		line := over["line"].([]interface{})
		if len(line) != 5 {
			t.Fatalf("승리선 %d개, want 5: %v", len(line), line)
		}
		first := line[0].(map[string]interface{})
		if first["row"].(float64) != 7 || first["col"].(float64) != 3 {
			t.Fatalf("승리선 시작 = %v, want (7,3)", first)
		}
	}

	// 재대결: 흑 신청 → 백에게 offer, 백 신청 → 같은 방에서 재시작
	clients[black].send(t, OMMessage{Type: OMMsgRematch})
	clients[white].waitFor(t, OMMsgRematchOffer)
	clients[white].send(t, OMMessage{Type: OMMsgRematch})

	for _, c := range clients {
		c.waitFor(t, OMMsgPlayerJoined)
		state := omPayloadMap(t, c.waitFor(t, OMMsgGameState))
		if state["moveCount"].(float64) != 0 {
			t.Fatalf("재대결 스냅샷 moveCount = %v, want 0", state["moveCount"])
		}
		if state["currentColor"] != string(OMBlack) {
			t.Fatalf("재대결 선공 = %v, want black", state["currentColor"])
		}
	}
}

// TestOMVsBotCompletes 봇전 완주 — 사람 자리도 같은 brain 이 대신 둔다
func TestOMVsBotCompletes(t *testing.T) {
	_, url, cleanup := newOMTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := omDial(t, url)
	defer c.conn.Close()
	c.send(t, OMMessage{Type: OMMsgJoinGame, Payload: OMJoinGamePayload{PlayerName: "사람", VsBot: true}})
	joined := omPayloadMap(t, c.waitFor(t, OMMsgPlayerJoined))
	if joined["yourColor"] != string(OMBlack) {
		t.Fatalf("봇전 사람 색 = %v, want black(선공)", joined["yourColor"])
	}

	brain := newOMBrain(rand.New(rand.NewSource(42)))
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "봇전 진행", func(OMMessage) bool { return true })
		if msg.Type == OMMsgGameOver {
			return
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("오목 봇전이 30초 안에 끝나지 않았다")
}
