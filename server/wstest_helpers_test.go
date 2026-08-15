package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// 테스트에서는 연습봇의 "생각하는 시간"을 끈다 (지연은 순수 연출)
func init() {
	botDelayBase = 0
	botDelayJitterMs = 0
	// 재대결 창도 짧게 — 만료 테스트가 기다릴 수 있는 수준으로
	rematchWindow = 700 * time.Millisecond
}

// testConn WS 통합 테스트 공용 클라이언트. writeLoop 가 여러 메시지를
// 개행으로 묶어 보내므로 큐로 풀어서 읽는다. 각 게임 테스트가 임베드해
// 자기 메시지 타입의 waitFor 만 덧붙인다.
type testConn[M any] struct {
	conn  *websocket.Conn
	queue []M
}

func dialWS[M any](t *testing.T, url string) testConn[M] {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	return testConn[M]{conn: conn}
}

func (c *testConn[M]) send(t *testing.T, msg M) {
	t.Helper()
	if err := c.conn.WriteJSON(msg); err != nil {
		t.Fatalf("send failed: %v", err)
	}
}

// waitMatch 조건을 만족하는 메시지가 올 때까지 읽는다 (불일치 메시지는 건너뜀)
func (c *testConn[M]) waitMatch(t *testing.T, name string, match func(M) bool) M {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(c.queue) == 0 {
			c.conn.SetReadDeadline(deadline)
			_, data, err := c.conn.ReadMessage()
			if err != nil {
				t.Fatalf("read failed waiting for %s: %v", name, err)
			}
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if line == "" {
					continue
				}
				var msg M
				if err := json.Unmarshal([]byte(line), &msg); err != nil {
					t.Fatalf("unmarshal failed: %v (%s)", err, line)
				}
				c.queue = append(c.queue, msg)
			}
		}
		for len(c.queue) > 0 {
			msg := c.queue[0]
			c.queue = c.queue[1:]
			if match(msg) {
				return msg
			}
		}
	}
	t.Fatalf("timed out waiting for %s", name)
	var zero M
	return zero
}

// asPayloadMap 봉투의 interface{} 페이로드를 map 으로 캐스팅
func asPayloadMap(t *testing.T, payload interface{}) map[string]interface{} {
	t.Helper()
	m, ok := payload.(map[string]interface{})
	if !ok {
		t.Fatalf("payload is not a map: %#v", payload)
	}
	return m
}
