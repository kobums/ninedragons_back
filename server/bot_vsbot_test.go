package server

import (
	"math/rand"
	"testing"
	"time"
)

// 봇전(vsBot) 통합 테스트 — 사람 자리는 같은 brain 을 쓰는 WS 드라이버가
// 대신 두고, 서버 내장 연습봇과의 게임이 끝까지 완주되는지 확인한다.

func TestGSVsBotCompletes(t *testing.T) {
	_, url, cleanup := newGSTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := gsDial(t, url)
	defer c.conn.Close()
	c.send(t, GSMessage{Type: GSMsgJoinGame, Payload: GSJoinGamePayload{PlayerName: "사람", VsBot: true}})
	c.waitFor(t, GSMsgPlayerJoined)

	done := make(chan string, 1)
	driver := &gsBot{conn: c.conn, done: done, brain: newGSBrain(rand.New(rand.NewSource(42)))}
	go driver.run(nil)

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("가이스터 봇전이 20초 안에 끝나지 않았다")
	}
}

func TestQDVsBotCompletes(t *testing.T) {
	_, url, cleanup := newQDTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := qdDial(t, url)
	defer c.conn.Close()
	c.send(t, QDMessage{Type: QDMsgJoinGame, Payload: QDJoinGamePayload{PlayerName: "사람", VsBot: true}})
	c.waitFor(t, QDMsgPlayerJoined)

	done := make(chan string, 1)
	driver := &qdBot{conn: c.conn, done: done}
	go driver.run(nil)

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("쿼리도 봇전이 20초 안에 끝나지 않았다")
	}
}

func TestOTVsBotCompletes(t *testing.T) {
	_, url, cleanup := newOTTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := otDial(t, url)
	defer c.conn.Close()
	c.send(t, OTMessage{Type: OTMsgJoinGame, Payload: OTJoinGamePayload{PlayerName: "사람", VsBot: true}})
	c.waitFor(t, OTMsgPlayerJoined)

	done := make(chan string, 1)
	driver := &otBot{conn: c.conn, done: done}
	go driver.run(nil)

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("오니타마 봇전이 20초 안에 끝나지 않았다")
	}
}

func TestLCVsBotCompletes(t *testing.T) {
	_, url, cleanup := newLCTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := lcDial(t, url)
	defer c.conn.Close()
	c.send(t, LCMessage{Type: LCMsgJoinGame, Payload: LCJoinGamePayload{PlayerName: "사람", VsBot: true}})
	c.waitFor(t, LCMsgPlayerJoined)

	done := make(chan bool, 1)
	driver := &lcBot{conn: c.conn, done: done}
	go driver.run(nil)

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("로스트 시티 봇전이 20초 안에 끝나지 않았다")
	}
}

func TestCSVsBotCompletes(t *testing.T) {
	_, url, cleanup := newCSTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := csDial(t, url)
	defer c.conn.Close()
	c.send(t, CSMessage{Type: CSMsgJoinGame, Payload: CSJoinGamePayload{PlayerName: "사람", VsBot: true}})
	c.waitFor(t, CSMsgPlayerJoined)

	done := make(chan string, 1)
	driver := &csBot{conn: c.conn, done: done}
	go driver.run(nil)

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("캔트 스톱 봇전이 20초 안에 끝나지 않았다")
	}
}
