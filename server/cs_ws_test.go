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

// csTestClient 공용 testConn 에 게임 메시지 타입의 waitFor 를 얹은 래퍼
type csTestClient struct {
	testConn[CSMessage]
}

func newCSTestServer(t *testing.T, grace time.Duration) (*CSHub, string, func()) {
	t.Helper()
	hub := NewCSHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeCSWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func csDial(t *testing.T, url string) *csTestClient {
	t.Helper()
	return &csTestClient{dialWS[CSMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *csTestClient) waitFor(t *testing.T, msgType CSMessageType) CSMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m CSMessage) bool { return m.Type == msgType })
}

func csPayloadMap(t *testing.T, msg CSMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// startCSGame 2인 입장 → play 진입. 각자의 세션과 첫 스냅샷을 반환.
func startCSGame(t *testing.T, url string) ([]*csTestClient, []string, []map[string]interface{}) {
	t.Helper()
	clients := []*csTestClient{csDial(t, url), csDial(t, url)}
	sessions := make([]string, 2)
	for i, c := range clients {
		c.send(t, CSMessage{Type: CSMsgJoinGame, Payload: CSJoinGamePayload{PlayerName: string(rune('A' + i))}})
		joined := csPayloadMap(t, c.waitFor(t, CSMsgPlayerJoined))
		sessions[i] = joined["sessionId"].(string)
	}

	states := make([]map[string]interface{}, 2)
	for i, c := range clients {
		states[i] = csPayloadMap(t, c.waitFor(t, CSMsgGameState))
	}
	return clients, sessions, states
}

// TestCSRollChooseStopFlow 굴림→조합 선택→정지의 전체 흐름과 재접속 복원
func TestCSRollChooseStopFlow(t *testing.T) {
	_, url, cleanup := newCSTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, sessions, states := startCSGame(t, url)
	defer func() {
		clients[0].conn.Close()
	}()

	turnIdx := 0
	if states[0]["currentSide"] != states[0]["yourSide"] {
		turnIdx = 1
	}

	// 시작 상태: 모든 컬럼이 열려 있으니 첫 굴림은 버스트가 될 수 없다
	clients[turnIdx].send(t, CSMessage{Type: CSMsgRoll})
	var rolled map[string]interface{}
	for _, c := range clients {
		ev := csPayloadMap(t, c.waitFor(t, CSMsgEvent))
		if ev["kind"] != "roll" || len(ev["dice"].([]interface{})) != 4 {
			t.Fatalf("roll 이벤트 이상: %v", ev)
		}
		rolled = csPayloadMap(t, c.waitFor(t, CSMsgGameState))
		if len(rolled["options"].([]interface{})) == 0 {
			t.Fatal("첫 굴림에 옵션이 없다 (버스트 불가 상태인데)")
		}
	}

	// 첫 옵션 그대로 전진
	firstOpt := rolled["options"].([]interface{})[0].(map[string]interface{})
	sums := []int{}
	for _, s := range firstOpt["sums"].([]interface{}) {
		sums = append(sums, int(s.(float64)))
	}
	clients[turnIdx].send(t, CSMessage{Type: CSMsgChoose, Payload: CSChoosePayload{Sums: sums}})
	for _, c := range clients {
		ev := csPayloadMap(t, c.waitFor(t, CSMsgEvent))
		if ev["kind"] != "advance" {
			t.Fatalf("kind = %v, want advance", ev["kind"])
		}
		state := csPayloadMap(t, c.waitFor(t, CSMsgGameState))
		if len(state["temp"].(map[string]interface{})) == 0 {
			t.Fatal("전진 후 임시 마커가 없다")
		}
		if state["canStop"] != true {
			t.Fatal("전진 후 canStop 이 아니다")
		}
	}

	// 정지 → 뱅킹 + 턴 교대
	clients[turnIdx].send(t, CSMessage{Type: CSMsgStop})
	for _, c := range clients {
		ev := csPayloadMap(t, c.waitFor(t, CSMsgEvent))
		if ev["kind"] != "bank" {
			t.Fatalf("kind = %v, want bank", ev["kind"])
		}
		state := csPayloadMap(t, c.waitFor(t, CSMsgGameState))
		if state["currentSide"] == states[turnIdx]["yourSide"] {
			t.Fatal("정지 후 턴이 넘어가지 않음")
		}
		if len(state["temp"].(map[string]interface{})) != 0 {
			t.Fatal("정지 후 임시 마커가 남아 있다")
		}
	}

	// 비턴(방금 플레이한) 클라이언트가 끊고 재접속 → 진행도 복원
	clients[turnIdx].conn.Close()
	clients[1-turnIdx].waitFor(t, CSMsgOpponentDisconnected)

	rejoined := csDial(t, url)
	defer rejoined.conn.Close()
	rejoined.send(t, CSMessage{Type: CSMsgRejoinGame, Payload: CSRejoinGamePayload{SessionID: sessions[turnIdx]}})
	clients[1-turnIdx].waitFor(t, CSMsgOpponentReconnected)

	state := csPayloadMap(t, rejoined.waitFor(t, CSMsgGameState))
	mySide := state["yourSide"].(string)
	progressKey := "southProgress"
	if mySide == string(CSNorth) {
		progressKey = "northProgress"
	}
	if len(state[progressKey].(map[string]interface{})) == 0 {
		t.Fatal("복원 스냅샷에 뱅킹된 진행도가 없다")
	}
}

// TestCSBotsCompleteGame 굴림·전진·정지 봇 2개가 3컬럼 완등까지 완주하는지
func TestCSBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newCSTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, _, states := startCSGame(t, url)
	defer func() {
		for _, c := range clients {
			c.conn.Close()
		}
	}()

	done := make(chan string, 2)
	for i, c := range clients {
		bot := &csBot{conn: c.conn, done: done}
		go bot.run(states[i])
	}

	select {
	case winner := <-done:
		if winner != string(CSSouth) && winner != string(CSNorth) {
			t.Fatalf("winner = %q", winner)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("20초 안에 게임이 끝나지 않았다 — 진행 불가 상태")
	}
}

// csBot 서버 연습봇 두뇌(csBrain)를 WS 클라이언트로 감싼 완주 봇
type csBot struct {
	conn  *websocket.Conn
	done  chan<- string
	brain csBrain
}

func (b *csBot) send(msg CSMessage) {
	data, _ := json.Marshal(msg)
	b.conn.WriteMessage(websocket.TextMessage, data)
}

func (b *csBot) handle(msg CSMessage) {
	if reply := b.brain.decide(msg); reply != nil {
		b.send(*reply)
	}
}

func (b *csBot) run(initial map[string]interface{}) {
	if initial != nil {
		b.handle(CSMessage{Type: CSMsgGameState, Payload: initial})
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
			var msg CSMessage
			if json.Unmarshal([]byte(line), &msg) != nil {
				continue
			}
			if msg.Type == CSMsgGameOver {
				raw, _ := json.Marshal(msg.Payload)
				var over struct {
					Winner string `json:"winner"`
				}
				json.Unmarshal(raw, &over)
				b.done <- over.Winner
				return
			}
			b.handle(msg)
		}
	}
}
