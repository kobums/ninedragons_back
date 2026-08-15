package server

import (
	"math/rand"
	"testing"
	"time"
)

// 나머지 4게임의 봇전 재대결 스모크 — 완주 후 재대결 신청 시 같은 방에서
// 즉시 재시작(봇 자동 수락)되는지 확인한다. 창 만료·사람전 제안 흐름은
// 쿼리도 테스트(qd_rematch_test.go)가 대표로 검증한다.

func TestGSRematchVsBot(t *testing.T) {
	_, url, cleanup := newGSTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := gsDial(t, url)
	defer c.conn.Close()
	c.send(t, GSMessage{Type: GSMsgJoinGame, Payload: GSJoinGamePayload{PlayerName: "사람", VsBot: true}})
	c.waitFor(t, GSMsgPlayerJoined)

	brain := newGSBrain(rand.New(rand.NewSource(7)))
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "봇전 진행", func(GSMessage) bool { return true })
		if msg.Type == GSMsgGameOver {
			break
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}

	c.send(t, GSMessage{Type: GSMsgRematch})
	c.waitFor(t, GSMsgPlayerJoined)
	state := gsPayloadMap(t, c.waitFor(t, GSMsgGameState))
	if state["phase"] != string(GSPhaseSetup) {
		t.Fatalf("재대결 phase = %v, want setup", state["phase"])
	}
}

func TestOTRematchVsBot(t *testing.T) {
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
			break
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}

	c.send(t, OTMessage{Type: OTMsgRematch})
	c.waitFor(t, OTMsgPlayerJoined)
	state := otPayloadMap(t, c.waitFor(t, OTMsgGameState))
	if state["phase"] != string(OTPhasePlay) {
		t.Fatalf("재대결 phase = %v, want play", state["phase"])
	}
}

func TestLCRematchVsBot(t *testing.T) {
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
			break
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}

	c.send(t, LCMessage{Type: LCMsgRematch})
	c.waitFor(t, LCMsgPlayerJoined)
	state := lcPayloadMap(t, c.waitFor(t, LCMsgGameState))
	if state["phase"] != string(LCPhasePlay) {
		t.Fatalf("재대결 phase = %v, want play", state["phase"])
	}
}

func TestCSRematchVsBot(t *testing.T) {
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
			break
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}

	c.send(t, CSMessage{Type: CSMsgRematch})
	c.waitFor(t, CSMsgPlayerJoined)
	state := csPayloadMap(t, c.waitFor(t, CSMsgGameState))
	if state["phase"] != string(CSPhasePlay) {
		t.Fatalf("재대결 phase = %v, want play", state["phase"])
	}
}
