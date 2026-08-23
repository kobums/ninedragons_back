package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// qdTestClient 공용 testConn 에 게임 메시지 타입의 waitFor 를 얹은 래퍼
type qdTestClient struct {
	testConn[QDMessage]
}

func newQDTestServer(t *testing.T, grace time.Duration) (*QDHub, string, func()) {
	t.Helper()
	hub := NewQDHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeQDWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func qdDial(t *testing.T, url string) *qdTestClient {
	t.Helper()
	return &qdTestClient{dialWS[QDMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *qdTestClient) waitFor(t *testing.T, msgType QDMessageType) QDMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m QDMessage) bool { return m.Type == msgType })
}

func qdPayloadMap(t *testing.T, msg QDMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// startQDGame 2인 입장 → play 진입. 각자의 세션과 첫 스냅샷을 반환.
func startQDGame(t *testing.T, url string) ([]*qdTestClient, []string, []map[string]interface{}) {
	t.Helper()
	clients := []*qdTestClient{qdDial(t, url), qdDial(t, url)}
	sessions := make([]string, 2)
	for i, c := range clients {
		c.send(t, QDMessage{Type: QDMsgJoinGame, Payload: QDJoinGamePayload{PlayerName: string(rune('A' + i))}})
		joined := qdPayloadMap(t, c.waitFor(t, QDMsgPlayerJoined))
		sessions[i] = joined["sessionId"].(string)
	}

	states := make([]map[string]interface{}, 2)
	for i, c := range clients {
		states[i] = qdPayloadMap(t, c.waitFor(t, QDMsgGameState))
	}
	return clients, sessions, states
}

// TestQDStateSnapshot 시작 스냅샷: 완전 공개 정보 — yourSide 만 다르고 나머지는 동일
func TestQDStateSnapshot(t *testing.T) {
	_, url, cleanup := newQDTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, _, states := startQDGame(t, url)
	defer func() {
		for _, c := range clients {
			c.conn.Close()
		}
	}()

	if states[0]["yourSide"] == states[1]["yourSide"] {
		t.Fatal("두 클라이언트의 진영이 같다")
	}
	for i, state := range states {
		if state["phase"] != string(QDPhasePlay) {
			t.Fatalf("클라 %d: phase = %v, want play", i, state["phase"])
		}
		if state["southWalls"].(float64) != QDWallCount || state["northWalls"].(float64) != QDWallCount {
			t.Fatalf("클라 %d: 벽 수 이상", i)
		}
		legal := state["legalMoves"].([]interface{})
		if len(legal) != 3 {
			t.Fatalf("클라 %d: 시작 합법 이동 %d개, want 3", i, len(legal))
		}
	}
	// 합법 이동은 공개 정보 — 양측 스냅샷이 동일해야 한다
	a, _ := json.Marshal(states[0]["legalMoves"])
	b, _ := json.Marshal(states[1]["legalMoves"])
	if string(a) != string(b) {
		t.Fatalf("양측 legalMoves 불일치: %s vs %s", a, b)
	}
}

// TestQDWallPlaceAndRejoin 벽 설치 이벤트·상태 반영과 재접속 복원 검증
func TestQDWallPlaceAndRejoin(t *testing.T) {
	_, url, cleanup := newQDTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, sessions, states := startQDGame(t, url)
	defer func() {
		clients[0].conn.Close()
	}()

	turnIdx := 0
	if states[0]["currentSide"] != states[0]["yourSide"] {
		turnIdx = 1
	}

	clients[turnIdx].send(t, QDMessage{Type: QDMsgPlaceWall,
		Payload: QDPlaceWallPayload{Row: 4, Col: 4, Orientation: "h"}})
	for _, c := range clients {
		ev := qdPayloadMap(t, c.waitFor(t, QDMsgEvent))
		if ev["kind"] != "wall" {
			t.Fatalf("kind = %v, want wall", ev["kind"])
		}
		state := qdPayloadMap(t, c.waitFor(t, QDMsgGameState))
		if len(state["walls"].([]interface{})) != 1 {
			t.Fatalf("벽이 상태에 반영되지 않음: %v", state["walls"])
		}
	}

	// 비턴 클라이언트가 끊고 재접속 → 벽 포함 스냅샷 복원
	other := 1 - turnIdx
	clients[other].conn.Close()
	clients[turnIdx].waitFor(t, QDMsgOpponentDisconnected)

	rejoined := qdDial(t, url)
	defer rejoined.conn.Close()
	rejoined.send(t, QDMessage{Type: QDMsgRejoinGame, Payload: QDRejoinGamePayload{SessionID: sessions[other]}})
	clients[turnIdx].waitFor(t, QDMsgOpponentReconnected)

	state := qdPayloadMap(t, rejoined.waitFor(t, QDMsgGameState))
	if state["phase"] != string(QDPhasePlay) {
		t.Fatalf("복원 phase = %v", state["phase"])
	}
	if len(state["walls"].([]interface{})) != 1 {
		t.Fatalf("복원 스냅샷에 벽이 없다: %v", state["walls"])
	}
}

// TestQDBotsCompleteGame 골 라인을 향해 전진하는 봇 2개가 게임을 완주하는지
func TestQDBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newQDTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, _, states := startQDGame(t, url)
	defer func() {
		for _, c := range clients {
			c.conn.Close()
		}
	}()

	done := make(chan string, 2)
	for i, c := range clients {
		bot := &qdBot{conn: c.conn, done: done}
		go bot.run(states[i])
	}

	// 드라이버는 읽기를 놓을 때마다 알린다 — 승자를 본 쪽의 신호를 찾는다
	timeout := time.After(20 * time.Second)
	winner := ""
	for range clients {
		select {
		case w := <-done:
			if w != "" {
				winner = w
			}
		case <-timeout:
			t.Fatal("20초 안에 게임이 끝나지 않았다 — 진행 불가 상태")
		}
	}
	if winner != string(QDSouth) && winner != string(QDNorth) {
		t.Fatalf("winner = %q", winner)
	}
}

// qdBot 서버 연습봇 두뇌(qdBrain)를 WS 클라이언트로 감싼 완주 봇
type qdBot struct {
	conn  *websocket.Conn
	done  chan<- string
	brain qdBrain
}

func (b *qdBot) send(msg QDMessage) {
	data, _ := json.Marshal(msg)
	b.conn.WriteMessage(websocket.TextMessage, data)
}

func (b *qdBot) handle(msg QDMessage) {
	if reply := b.brain.decide(msg); reply != nil {
		b.send(*reply)
	}
}

func (b *qdBot) run(initial map[string]interface{}) {
	// 읽기를 놓을 때 반드시 알린다. 예전에는 game_over 를 본 경우에만
	// 알려서, 호출부가 한 명만 기다리고 잠깐 자는 사이 아직 소켓을 읽던
	// 드라이버와 본 고루틴이 같은 연결을 동시에 읽어 레이스가 났다.
	winner := ""
	defer func() { b.done <- winner }()

	if initial != nil {
		b.handle(QDMessage{Type: QDMsgGameState, Payload: initial})
	}

	deadline := time.Now().Add(20 * time.Second)
	b.conn.SetReadDeadline(deadline)

	for time.Now().Before(deadline) {
		_, data, err := b.conn.ReadMessage()
		if err != nil {
			return
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var msg QDMessage
			if json.Unmarshal([]byte(line), &msg) != nil {
				continue
			}
			if msg.Type == QDMsgGameOver {
				raw, _ := json.Marshal(msg.Payload)
				var over struct {
					Winner string `json:"winner"`
				}
				json.Unmarshal(raw, &over)
				winner = over.Winner
				return
			}
			b.handle(msg)
		}
	}
}
