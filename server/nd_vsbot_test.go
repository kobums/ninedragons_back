package server

import (
	"testing"
	"time"
)

// 구룡투 봇전(vsBot) 통합 테스트 — 사람 자리는 같은 brain 을 쓰는 드라이버가
// 대신 둔다. writeLoop 가 여러 메시지를 한 프레임에 묶으므로 반드시 testConn
// 큐를 통해 모든 메시지를 순서대로 소비한다.

func TestNDVsBotCompletes(t *testing.T) {
	_, url, cleanup := newTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := dial(t, url)
	defer c.conn.Close()
	c.send(t, Message{Type: MsgJoinGame, Payload: JoinGamePayload{PlayerName: "사람", Color: Red, VsBot: true}})
	joined := payloadMap(t, c.waitFor(t, MsgPlayerJoined))
	if joined["yourColor"] != string(Red) {
		t.Fatalf("yourColor = %v, want red (선호 색상)", joined["yourColor"])
	}

	// 봇이 남은 색(파랑)을 맡아 선공으로 먼저 낸다
	brain := &ndBrain{}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "봇전 진행", func(Message) bool { return true })
		if msg.Type == MsgGameOver {
			return
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("구룡투 봇전이 20초 안에 끝나지 않았다")
}
