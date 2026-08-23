package server

import (
	"math/rand"
	"testing"
	"time"
)

// TestJHRematchOfferHumans 사람전 재대결: 한쪽 신청 → 상대 제안 수신 → 수락 시
// 같은 방에서 재시작. 역할은 AddPlayer 규칙에 따라 다시 배정된다 (바뀌어도 무방).
func TestJHRematchOfferHumans(t *testing.T) {
	_, url, cleanup := newJHTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, sessions, states := startJHGame(t, url)
	defer func() {
		for _, c := range clients {
			c.conn.Close()
		}
	}()

	// 두 드라이버(raw conn)로 완주 — 종료 후에는 다시 testConn 큐로 읽는다
	done := make(chan string, 2)
	for i, c := range clients {
		bot := &jhWsBot{conn: c.conn, rng: rand.New(rand.NewSource(int64(i + 7))), done: done}
		go bot.run(states[i])
	}
	for range clients {
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Fatal("사람전 완주 실패")
		}
	}

	clients[0].send(t, JHMessage{Type: JHMsgRematch})
	clients[1].waitFor(t, JHMsgRematchOffer)

	clients[1].send(t, JHMessage{Type: JHMsgRematch})
	roles := make([]string, 2)
	for i, c := range clients {
		joined := jhPayloadMap(t, c.waitFor(t, JHMsgPlayerJoined))
		if joined["sessionId"].(string) != sessions[i] {
			t.Fatalf("client %d: 재대결 후 세션이 바뀌었다", i)
		}
		roles[i] = joined["yourRole"].(string)

		state := jhPayloadMap(t, c.waitFor(t, JHMsgGameState))
		if state["phase"] != string(JHPhaseExchange) {
			t.Fatalf("client %d: 재대결 phase = %v, want exchange", i, state["phase"])
		}
		hand, ok := state["yourHand"].([]interface{})
		if !ok || len(hand) != JHHandSize {
			t.Fatalf("client %d: 재대결 손패가 10장이 아님: %v", i, state["yourHand"])
		}
	}
	if roles[0] == roles[1] {
		t.Fatal("재대결 후 두 클라이언트의 역할이 같음")
	}
}

// TestJHRematchWindowExpires 창이 지나면 세션 만료 통지와 함께 방이 정리된다
func TestJHRematchWindowExpires(t *testing.T) {
	_, url, cleanup := newJHTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, _, states := startJHGame(t, url)
	defer func() {
		for _, c := range clients {
			c.conn.Close()
		}
	}()

	done := make(chan string, 2)
	for i, c := range clients {
		bot := &jhWsBot{conn: c.conn, rng: rand.New(rand.NewSource(int64(i + 7))), done: done}
		go bot.run(states[i])
	}
	for range clients {
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Fatal("사람전 완주 실패")
		}
	}

	// 재대결 신청 없이 창(700ms) 경과 → 양쪽 모두 세션 만료
	for _, c := range clients {
		c.waitFor(t, JHMsgSessionExpired)
	}

	// 만료 후 재대결 신청은 무시된다 (방이 이미 없음)
	clients[0].send(t, JHMessage{Type: JHMsgRematch})
	time.Sleep(100 * time.Millisecond)
}
