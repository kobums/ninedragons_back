package server

import (
	"math/rand"
	"testing"
	"time"
)

// 봇전(vsBot) 통합 테스트 — 사람 자리는 같은 brain 을 쓰는 드라이버가 대신
// 둔다. writeLoop 가 여러 메시지를 한 프레임에 묶으므로 반드시 testConn 큐를
// 통해 모든 메시지를 순서대로 소비한다 (소켓 직접 읽기와 섞으면 유실된다).

func TestGSVsBotCompletes(t *testing.T) {
	_, url, cleanup := newGSTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := gsDial(t, url)
	defer c.conn.Close()
	c.send(t, GSMessage{Type: GSMsgJoinGame, Payload: GSJoinGamePayload{PlayerName: "사람", VsBot: true}})
	c.waitFor(t, GSMsgPlayerJoined)

	brain := newGSBrain(rand.New(rand.NewSource(42)))
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "봇전 진행", func(GSMessage) bool { return true })
		if msg.Type == GSMsgGameOver {
			return
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("가이스터 봇전이 20초 안에 끝나지 않았다")
}

func TestQDVsBotCompletes(t *testing.T) {
	_, url, cleanup := newQDTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := qdDial(t, url)
	defer c.conn.Close()
	c.send(t, QDMessage{Type: QDMsgJoinGame, Payload: QDJoinGamePayload{PlayerName: "사람", VsBot: true}})
	c.waitFor(t, QDMsgPlayerJoined)

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
	t.Fatal("쿼리도 봇전이 20초 안에 끝나지 않았다")
}

func TestOTVsBotCompletes(t *testing.T) {
	_, url, cleanup := newOTTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := otDial(t, url)
	defer c.conn.Close()
	c.send(t, OTMessage{Type: OTMsgJoinGame, Payload: OTJoinGamePayload{PlayerName: "사람", VsBot: true}})
	c.waitFor(t, OTMsgPlayerJoined)

	brain := &otBrain{}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "봇전 진행", func(OTMessage) bool { return true })
		if msg.Type == OTMsgGameOver {
			return
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("오니타마 봇전이 20초 안에 끝나지 않았다")
}

func TestLCVsBotCompletes(t *testing.T) {
	_, url, cleanup := newLCTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := lcDial(t, url)
	defer c.conn.Close()
	c.send(t, LCMessage{Type: LCMsgJoinGame, Payload: LCJoinGamePayload{PlayerName: "사람", VsBot: true}})
	c.waitFor(t, LCMsgPlayerJoined)

	brain := &lcBrain{}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "봇전 진행", func(LCMessage) bool { return true })
		if msg.Type == LCMsgGameOver {
			return
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("로스트 시티 봇전이 20초 안에 끝나지 않았다")
}

func TestCSVsBotCompletes(t *testing.T) {
	_, url, cleanup := newCSTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := csDial(t, url)
	defer c.conn.Close()
	c.send(t, CSMessage{Type: CSMsgJoinGame, Payload: CSJoinGamePayload{PlayerName: "사람", VsBot: true}})
	c.waitFor(t, CSMsgPlayerJoined)

	brain := &csBrain{}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "봇전 진행", func(CSMessage) bool { return true })
		if msg.Type == CSMsgGameOver {
			return
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("캔트 스톱 봇전이 20초 안에 끝나지 않았다")
}
