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

// otTestClient 공용 testConn 에 게임 메시지 타입의 waitFor 를 얹은 래퍼
type otTestClient struct {
	testConn[OTMessage]
}

func newOTTestServer(t *testing.T, grace time.Duration) (*OTHub, string, func()) {
	t.Helper()
	hub := NewOTHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeOTWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func otDial(t *testing.T, url string) *otTestClient {
	t.Helper()
	return &otTestClient{dialWS[OTMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *otTestClient) waitFor(t *testing.T, msgType OTMessageType) OTMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m OTMessage) bool { return m.Type == msgType })
}

func otPayloadMap(t *testing.T, msg OTMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// startOTGame 2인 입장 → play 진입. 각자의 세션과 첫 스냅샷을 반환.
func startOTGame(t *testing.T, url string) ([]*otTestClient, []string, []map[string]interface{}) {
	t.Helper()
	clients := []*otTestClient{otDial(t, url), otDial(t, url)}
	sessions := make([]string, 2)
	for i, c := range clients {
		c.send(t, OTMessage{Type: OTMsgJoinGame, Payload: OTJoinGamePayload{PlayerName: string(rune('A' + i))}})
		joined := otPayloadMap(t, c.waitFor(t, OTMsgPlayerJoined))
		sessions[i] = joined["sessionId"].(string)
	}

	states := make([]map[string]interface{}, 2)
	for i, c := range clients {
		states[i] = otPayloadMap(t, c.waitFor(t, OTMsgGameState))
	}
	return clients, sessions, states
}

// TestOTStateSnapshot 시작 스냅샷: 카드 5장 전부 공개, 양측 동일 정보
func TestOTStateSnapshot(t *testing.T) {
	_, url, cleanup := newOTTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, _, states := startOTGame(t, url)
	defer func() {
		for _, c := range clients {
			c.conn.Close()
		}
	}()

	if states[0]["yourSide"] == states[1]["yourSide"] {
		t.Fatal("두 클라이언트의 진영이 같다")
	}
	seen := map[string]bool{}
	for i, state := range states {
		if state["phase"] != string(OTPhasePlay) {
			t.Fatalf("클라 %d: phase = %v, want play", i, state["phase"])
		}
		if len(state["pieces"].([]interface{})) != 10 {
			t.Fatalf("클라 %d: 기물 수 이상", i)
		}
		// 카드 5장이 전부 서로 다른지
		cards := []string{state["waitingCard"].(string)}
		for _, h := range []string{"southHand", "northHand"} {
			for _, c := range state[h].([]interface{}) {
				cards = append(cards, c.(string))
			}
		}
		if len(cards) != 5 {
			t.Fatalf("클라 %d: 카드 %d장, want 5", i, len(cards))
		}
		for _, c := range cards {
			if i == 0 && seen[c] {
				t.Fatalf("카드 중복: %s", c)
			}
			seen[c] = true
		}
		if len(state["legalMoves"].([]interface{})) == 0 {
			t.Fatalf("클라 %d: 시작 합법 수가 없다", i)
		}
	}
}

// TestOTMoveCycleAndRejoin 이동 후 카드 순환·재접속 복원 검증
func TestOTMoveCycleAndRejoin(t *testing.T) {
	_, url, cleanup := newOTTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, sessions, states := startOTGame(t, url)
	defer func() {
		clients[0].conn.Close()
	}()

	turnIdx := 0
	if states[0]["currentSide"] != states[0]["yourSide"] {
		turnIdx = 1
	}

	// 서버가 준 합법 수 중 첫 수를 그대로 둔다
	first := states[turnIdx]["legalMoves"].([]interface{})[0].(map[string]interface{})
	usedCard := first["card"].(string)
	toCell := func(v interface{}) OTCell {
		m := v.(map[string]interface{})
		return OTCell{Row: int(m["row"].(float64)), Col: int(m["col"].(float64))}
	}
	clients[turnIdx].send(t, OTMessage{Type: OTMsgMove, Payload: OTMovePayload{
		Card: usedCard, From: toCell(first["from"]), To: toCell(first["to"]),
	}})

	for _, c := range clients {
		ev := otPayloadMap(t, c.waitFor(t, OTMsgEvent))
		if ev["kind"] != "move" {
			t.Fatalf("kind = %v, want move", ev["kind"])
		}
		state := otPayloadMap(t, c.waitFor(t, OTMsgGameState))
		// 쓴 카드가 대기 카드로 갔는지 (오니타마 핵심 순환)
		if state["waitingCard"].(string) != usedCard {
			t.Fatalf("카드 순환 이상: waiting=%v, want %s", state["waitingCard"], usedCard)
		}
	}

	// 비턴 클라이언트가 끊고 재접속 → 스냅샷 복원
	other := 1 - turnIdx
	clients[other].conn.Close()
	clients[turnIdx].waitFor(t, OTMsgOpponentDisconnected)

	rejoined := otDial(t, url)
	defer rejoined.conn.Close()
	rejoined.send(t, OTMessage{Type: OTMsgRejoinGame, Payload: OTRejoinGamePayload{SessionID: sessions[other]}})
	clients[turnIdx].waitFor(t, OTMsgOpponentReconnected)

	state := otPayloadMap(t, rejoined.waitFor(t, OTMsgGameState))
	if state["phase"] != string(OTPhasePlay) {
		t.Fatalf("복원 phase = %v", state["phase"])
	}
	if state["waitingCard"].(string) != usedCard {
		t.Fatal("복원 스냅샷의 카드 순환 상태가 다르다")
	}
}

// TestOTBotsCompleteGame 합법 수 봇 2개가 게임을 완주하는지
func TestOTBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newOTTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, _, states := startOTGame(t, url)
	defer func() {
		for _, c := range clients {
			c.conn.Close()
		}
	}()

	done := make(chan string, 2)
	for i, c := range clients {
		bot := &otBot{conn: c.conn, done: done}
		go bot.run(states[i])
	}

	select {
	case winner := <-done:
		if winner != string(OTSouth) && winner != string(OTNorth) {
			t.Fatalf("winner = %q", winner)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("20초 안에 게임이 끝나지 않았다 — 진행 불가 상태")
	}
}

// otBot 서버 연습봇 두뇌(otBrain)를 WS 클라이언트로 감싼 완주 봇
type otBot struct {
	conn  *websocket.Conn
	done  chan<- string
	brain otBrain
}

func (b *otBot) send(msg OTMessage) {
	data, _ := json.Marshal(msg)
	b.conn.WriteMessage(websocket.TextMessage, data)
}

func (b *otBot) handle(msg OTMessage) {
	if reply := b.brain.decide(msg); reply != nil {
		b.send(*reply)
	}
}

func (b *otBot) run(initial map[string]interface{}) {
	if initial != nil {
		b.handle(OTMessage{Type: OTMsgGameState, Payload: initial})
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
			var msg OTMessage
			if json.Unmarshal([]byte(line), &msg) != nil {
				continue
			}
			if msg.Type == OTMsgGameOver {
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
