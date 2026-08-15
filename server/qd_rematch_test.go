package server

import (
	"testing"
	"time"
)

// qdPlayVsBotToGameOver 봇전을 완주시키고 마지막 game_over 이후 상태로 둔다
func qdPlayVsBotToGameOver(t *testing.T, c *qdTestClient) {
	t.Helper()
	brain := &qdBrain{}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "봇전 진행", func(QDMessage) bool { return true })
		if msg.Type == QDMsgGameOver {
			return
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("봇전이 20초 안에 끝나지 않았다")
}

// TestQDRematchVsBot 봇전 재대결: 신청 즉시 같은 방에서 재시작 (봇 자동 수락)
func TestQDRematchVsBot(t *testing.T) {
	_, url, cleanup := newQDTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := qdDial(t, url)
	defer c.conn.Close()
	c.send(t, QDMessage{Type: QDMsgJoinGame, Payload: QDJoinGamePayload{PlayerName: "사람", VsBot: true}})
	joined := qdPayloadMap(t, c.waitFor(t, QDMsgPlayerJoined))
	sessionID := joined["sessionId"].(string)

	qdPlayVsBotToGameOver(t, c)

	c.send(t, QDMessage{Type: QDMsgRematch})
	rejoined := qdPayloadMap(t, c.waitFor(t, QDMsgPlayerJoined))
	if rejoined["sessionId"].(string) != sessionID {
		t.Fatal("재대결 후 세션이 바뀌었다")
	}
	state := qdPayloadMap(t, c.waitFor(t, QDMsgGameState))
	if state["phase"] != string(QDPhasePlay) {
		t.Fatalf("재대결 phase = %v, want play", state["phase"])
	}
}

// TestQDRematchOfferHumans 사람전 재대결: 한쪽 신청 → 상대 제안 수신 → 수락 시 재시작
func TestQDRematchOfferHumans(t *testing.T) {
	_, url, cleanup := newQDTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, _, states := startQDGame(t, url)
	defer func() {
		for _, c := range clients {
			c.conn.Close()
		}
	}()

	// 두 드라이버(raw conn)로 완주 — 종료 후에는 다시 testConn 큐로 읽는다
	done := make(chan string, 2)
	for i, c := range clients {
		bot := &qdBot{conn: c.conn, done: done}
		go bot.run(states[i])
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("사람전 완주 실패")
	}
	// 두 드라이버 모두 game_over 를 읽고 종료할 때까지 잠깐 대기
	time.Sleep(100 * time.Millisecond)

	clients[0].send(t, QDMessage{Type: QDMsgRematch})
	clients[1].waitFor(t, QDMsgRematchOffer)

	clients[1].send(t, QDMessage{Type: QDMsgRematch})
	for _, c := range clients {
		c.waitFor(t, QDMsgPlayerJoined)
		state := qdPayloadMap(t, c.waitFor(t, QDMsgGameState))
		if state["phase"] != string(QDPhasePlay) {
			t.Fatalf("재대결 phase = %v, want play", state["phase"])
		}
	}
}

// TestQDRematchWindowExpires 창이 지나면 세션 만료 통지와 함께 방이 정리된다
func TestQDRematchWindowExpires(t *testing.T) {
	hub, url, cleanup := newQDTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := qdDial(t, url)
	defer c.conn.Close()
	c.send(t, QDMessage{Type: QDMsgJoinGame, Payload: QDJoinGamePayload{PlayerName: "사람", VsBot: true}})
	c.waitFor(t, QDMsgPlayerJoined)

	qdPlayVsBotToGameOver(t, c)

	// 재대결 신청 없이 창(700ms) 경과 → 세션 만료
	c.waitFor(t, QDMsgSessionExpired)

	// 만료 후 재대결 신청은 무시된다 (방이 이미 없음)
	c.send(t, QDMessage{Type: QDMsgRematch})
	time.Sleep(100 * time.Millisecond)
	_ = hub
}
