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

// lcTestClient 공용 testConn 에 게임 메시지 타입의 waitFor 를 얹은 래퍼
type lcTestClient struct {
	testConn[LCMessage]
}

func newLCTestServer(t *testing.T, grace time.Duration) (*LCHub, string, func()) {
	t.Helper()
	hub := NewLCHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeLCWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func lcDial(t *testing.T, url string) *lcTestClient {
	t.Helper()
	return &lcTestClient{dialWS[LCMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *lcTestClient) waitFor(t *testing.T, msgType LCMessageType) LCMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m LCMessage) bool { return m.Type == msgType })
}

func lcPayloadMap(t *testing.T, msg LCMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// startLCGame 2인 입장 → play 진입. 각자의 세션과 첫 스냅샷을 반환.
func startLCGame(t *testing.T, url string) ([]*lcTestClient, []string, []map[string]interface{}) {
	t.Helper()
	clients := []*lcTestClient{lcDial(t, url), lcDial(t, url)}
	sessions := make([]string, 2)
	for i, c := range clients {
		c.send(t, LCMessage{Type: LCMsgJoinGame, Payload: LCJoinGamePayload{PlayerName: string(rune('A' + i))}})
		joined := lcPayloadMap(t, c.waitFor(t, LCMsgPlayerJoined))
		sessions[i] = joined["sessionId"].(string)
	}

	states := make([]map[string]interface{}, 2)
	for i, c := range clients {
		states[i] = lcPayloadMap(t, c.waitFor(t, LCMsgGameState))
	}
	return clients, sessions, states
}

// TestLCMaskedHands 손패 은닉: 내 손패만 실리고 상대는 장수뿐
func TestLCMaskedHands(t *testing.T) {
	_, url, cleanup := newLCTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, _, states := startLCGame(t, url)
	defer func() {
		for _, c := range clients {
			c.conn.Close()
		}
	}()

	handIDs := []map[float64]bool{{}, {}}
	for i, state := range states {
		hand := state["yourHand"].([]interface{})
		if len(hand) != LCHandSize {
			t.Fatalf("클라 %d: 손패 %d장, want 8", i, len(hand))
		}
		for _, cardRaw := range hand {
			handIDs[i][cardRaw.(map[string]interface{})["id"].(float64)] = true
		}
		if int(state["opponentHandCount"].(float64)) != LCHandSize {
			t.Fatalf("클라 %d: 상대 장수 이상", i)
		}
		if int(state["deckCount"].(float64)) != LCDeckSize-LCHandSize*2 {
			t.Fatalf("클라 %d: 덱 %v장, want 44", i, state["deckCount"])
		}
		// 상대 손패 필드 자체가 없어야 한다
		for _, key := range []string{"southHand", "northHand", "hands"} {
			if _, exists := state[key]; exists {
				t.Fatalf("클라 %d: 손패가 %s 로 샜다", i, key)
			}
		}
	}
	// 두 손패는 겹치지 않는다
	for id := range handIDs[0] {
		if handIDs[1][id] {
			t.Fatalf("카드 %v 가 양쪽 손에 있다", id)
		}
	}
}

// TestLCDiscardPublicAndRejoin 버리기 공개·덱 뽑기 비공개·재접속 복원 검증
func TestLCDiscardPublicAndRejoin(t *testing.T) {
	_, url, cleanup := newLCTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, sessions, states := startLCGame(t, url)
	defer func() {
		clients[0].conn.Close()
	}()

	turnIdx := 0
	if states[0]["currentSide"] != states[0]["yourSide"] {
		turnIdx = 1
	}

	// 첫 손패 카드를 버리고 덱에서 뽑는다
	first := states[turnIdx]["yourHand"].([]interface{})[0].(map[string]interface{})
	cardID := int(first["id"].(float64))
	clients[turnIdx].send(t, LCMessage{Type: LCMsgMove, Payload: LCMovePayload{
		CardID: cardID, Action: "discard", Draw: "deck",
	}})

	for _, c := range clients {
		ev := lcPayloadMap(t, c.waitFor(t, LCMsgEvent))
		if ev["kind"] != "discard" {
			t.Fatalf("kind = %v, want discard", ev["kind"])
		}
		// 버린 카드는 공개
		if int(ev["card"].(map[string]interface{})["id"].(float64)) != cardID {
			t.Fatal("버린 카드가 이벤트에 공개되지 않음")
		}
		draw := lcPayloadMap(t, c.waitFor(t, LCMsgEvent))
		if draw["kind"] != "draw" || draw["source"] != "deck" {
			t.Fatalf("draw 이벤트 이상: %v", draw)
		}
		// 덱에서 뽑은 카드는 비공개
		if _, exists := draw["card"]; exists {
			t.Fatal("덱에서 뽑은 카드가 이벤트에 샜다")
		}
	}

	// 비턴 클라이언트가 끊고 재접속 → 버림 더미 포함 복원
	other := 1 - turnIdx
	clients[other].conn.Close()
	clients[turnIdx].waitFor(t, LCMsgOpponentDisconnected)

	rejoined := lcDial(t, url)
	defer rejoined.conn.Close()
	rejoined.send(t, LCMessage{Type: LCMsgRejoinGame, Payload: LCRejoinGamePayload{SessionID: sessions[other]}})
	clients[turnIdx].waitFor(t, LCMsgOpponentReconnected)

	state := lcPayloadMap(t, rejoined.waitFor(t, LCMsgGameState))
	if state["phase"] != string(LCPhasePlay) {
		t.Fatalf("복원 phase = %v", state["phase"])
	}
	total := 0
	for _, pileRaw := range state["discards"].(map[string]interface{}) {
		total += len(pileRaw.([]interface{}))
	}
	if total != 1 {
		t.Fatalf("복원 버림 더미 %d장, want 1", total)
	}
}

// TestLCBotsCompleteGame 봇 2개가 덱을 소진해 게임을 완주하는지
func TestLCBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newLCTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, _, states := startLCGame(t, url)
	defer func() {
		for _, c := range clients {
			c.conn.Close()
		}
	}()

	done := make(chan bool, 2)
	for i, c := range clients {
		bot := &lcBot{conn: c.conn, done: done}
		go bot.run(states[i])
	}

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("20초 안에 게임이 끝나지 않았다 — 진행 불가 상태")
	}
}

// lcBot 서버 연습봇 두뇌(lcBrain)를 WS 클라이언트로 감싼 완주 봇
type lcBot struct {
	conn  *websocket.Conn
	done  chan<- bool
	brain lcBrain
}

func (b *lcBot) send(msg LCMessage) {
	data, _ := json.Marshal(msg)
	b.conn.WriteMessage(websocket.TextMessage, data)
}

func (b *lcBot) handle(msg LCMessage) {
	if reply := b.brain.decide(msg); reply != nil {
		b.send(*reply)
	}
}

func (b *lcBot) run(initial map[string]interface{}) {
	if initial != nil {
		b.handle(LCMessage{Type: LCMsgGameState, Payload: initial})
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
			var msg LCMessage
			if json.Unmarshal([]byte(line), &msg) != nil {
				continue
			}
			if msg.Type == LCMsgGameOver {
				b.done <- true
				return
			}
			b.handle(msg)
		}
	}
}
