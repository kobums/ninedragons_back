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

// testClient 테스트용 WebSocket 클라이언트
// writePump는 여러 메시지를 개행으로 묶어 한 프레임에 보낼 수 있으므로 큐로 풀어서 읽는다
type testClient struct {
	conn  *websocket.Conn
	queue []Message
}

func newTestServer(t *testing.T, grace time.Duration) (*Hub, string, func()) {
	t.Helper()
	hub := NewHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func dial(t *testing.T, url string) *testClient {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	return &testClient{conn: conn}
}

func (c *testClient) send(t *testing.T, msg Message) {
	t.Helper()
	if err := c.conn.WriteJSON(msg); err != nil {
		t.Fatalf("send failed: %v", err)
	}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *testClient) waitFor(t *testing.T, msgType MessageType) Message {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(c.queue) == 0 {
			c.conn.SetReadDeadline(deadline)
			_, data, err := c.conn.ReadMessage()
			if err != nil {
				t.Fatalf("read failed waiting for %s: %v", msgType, err)
			}
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if line == "" {
					continue
				}
				var msg Message
				if err := json.Unmarshal([]byte(line), &msg); err != nil {
					t.Fatalf("unmarshal failed: %v (%s)", err, line)
				}
				c.queue = append(c.queue, msg)
			}
		}
		for len(c.queue) > 0 {
			msg := c.queue[0]
			c.queue = c.queue[1:]
			if msg.Type == msgType {
				return msg
			}
		}
	}
	t.Fatalf("timed out waiting for %s", msgType)
	return Message{}
}

func payloadMap(t *testing.T, msg Message) map[string]interface{} {
	t.Helper()
	m, ok := msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("payload is not a map: %#v", msg.Payload)
	}
	return m
}

// joinTwoPlayers 두 명을 입장시켜 게임을 시작하고 각자의 sessionId를 반환한다
func joinTwoPlayers(t *testing.T, url string) (*testClient, *testClient, string, string) {
	t.Helper()
	c1 := dial(t, url)
	c1.send(t, Message{Type: MsgJoinGame, Payload: JoinGamePayload{PlayerName: "A", Color: Blue}})
	session1 := payloadMap(t, c1.waitFor(t, MsgPlayerJoined))["sessionId"].(string)

	c2 := dial(t, url)
	c2.send(t, Message{Type: MsgJoinGame, Payload: JoinGamePayload{PlayerName: "B"}})
	session2 := payloadMap(t, c2.waitFor(t, MsgPlayerJoined))["sessionId"].(string)

	c1.waitFor(t, MsgGameStart)
	c2.waitFor(t, MsgGameStart)
	return c1, c2, session1, session2
}

// TestRejoinRestoresGameState 게임 도중 끊긴 플레이어가 세션으로 복귀해
// 게임 상태를 그대로 돌려받고 계속 플레이할 수 있는지 검증
func TestRejoinRestoresGameState(t *testing.T) {
	_, url, cleanup := newTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c1, c2, _, session2 := joinTwoPlayers(t, url)
	defer c1.conn.Close()

	// 파랑(선공)이 5를 낸다
	c1.send(t, Message{Type: MsgPlayTile, Payload: PlayTilePayload{Tile: 5}})
	c1.waitFor(t, MsgTilePlayed)
	c2.waitFor(t, MsgTilePlayed)

	// 빨강이 갑자기 끊긴다 (앱 전환 상황)
	c2.conn.Close()

	// 파랑은 상대 끊김 알림을 받는다
	c1.waitFor(t, MsgOpponentDisconnected)

	// 빨강이 새 연결로 재접속
	c3 := dial(t, url)
	defer c3.conn.Close()
	c3.send(t, Message{Type: MsgRejoinGame, Payload: RejoinGamePayload{SessionID: session2}})

	// 파랑은 재접속 알림을 받는다
	c1.waitFor(t, MsgOpponentReconnected)

	// 빨강은 게임 상태 스냅샷을 받는다
	state := payloadMap(t, c3.waitFor(t, MsgGameState))
	if state["yourColor"] != "red" {
		t.Errorf("yourColor = %v, want red", state["yourColor"])
	}
	if state["blueRoundTile"] != float64(5) {
		t.Errorf("blueRoundTile = %v, want 5", state["blueRoundTile"])
	}
	if state["currentPlayer"] != "red" {
		t.Errorf("currentPlayer = %v, want red", state["currentPlayer"])
	}
	if state["opponentConnected"] != true {
		t.Errorf("opponentConnected = %v, want true", state["opponentConnected"])
	}

	// 재접속한 빨강이 3을 내면 라운드가 정상 진행된다
	c3.send(t, Message{Type: MsgPlayTile, Payload: PlayTilePayload{Tile: 3}})
	result := payloadMap(t, c1.waitFor(t, MsgRoundResult))
	if result["winner"] != "blue" {
		t.Errorf("round winner = %v, want blue (5 vs 3)", result["winner"])
	}
	c3.waitFor(t, MsgRoundResult)
}

// TestGraceExpiredEndsGame 유예 시간 안에 재접속하지 않으면
// 상대방에게 알리고 게임과 세션이 정리되는지 검증
func TestGraceExpiredEndsGame(t *testing.T) {
	_, url, cleanup := newTestServer(t, 100*time.Millisecond)
	defer cleanup()

	c1, c2, _, session2 := joinTwoPlayers(t, url)
	defer c1.conn.Close()

	// 빨강이 끊기고 유예 시간이 지나도록 방치
	c2.conn.Close()
	c1.waitFor(t, MsgOpponentDisconnected)

	// 파랑은 게임 종료 에러와 세션 만료를 받는다
	errMsg := payloadMap(t, c1.waitFor(t, MsgError))
	if !strings.Contains(errMsg["message"].(string), "재접속하지 않아") {
		t.Errorf("unexpected error message: %v", errMsg["message"])
	}
	c1.waitFor(t, MsgSessionExpired)

	// 만료된 세션으로 재접속을 시도하면 session_expired를 받는다
	c3 := dial(t, url)
	defer c3.conn.Close()
	c3.send(t, Message{Type: MsgRejoinGame, Payload: RejoinGamePayload{SessionID: session2}})
	c3.waitFor(t, MsgSessionExpired)
}

// TestRejoinWithUnknownSession 알 수 없는 세션으로 재접속하면 만료 응답을 받는다
func TestRejoinWithUnknownSession(t *testing.T) {
	_, url, cleanup := newTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := dial(t, url)
	defer c.conn.Close()
	c.send(t, Message{Type: MsgRejoinGame, Payload: RejoinGamePayload{SessionID: "no-such-session"}})
	c.waitFor(t, MsgSessionExpired)
}
