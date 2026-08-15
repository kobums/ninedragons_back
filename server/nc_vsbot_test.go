package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ncTestClient 공용 testConn 에 게임 메시지 타입의 waitFor 를 얹은 래퍼
type ncTestClient struct {
	testConn[NCMessage]
}

func newNCTestServer(t *testing.T, grace time.Duration) (*NCHub, string, func()) {
	t.Helper()
	hub := NewNCHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeNCWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func ncDial(t *testing.T, url string) *ncTestClient {
	t.Helper()
	return &ncTestClient{dialWS[NCMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *ncTestClient) waitFor(t *testing.T, msgType NCMessageType) NCMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m NCMessage) bool { return m.Type == msgType })
}

func ncPayloadMap(t *testing.T, msg NCMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// TestNCVsBotCompletes 봇전 join → 12라운드 완주(game_over 수신) 검증.
// 사람 자리도 같은 brain 이 대신 두므로 매 라운드 무승부 → rounds_complete.
func TestNCVsBotCompletes(t *testing.T) {
	_, url, cleanup := newNCTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := ncDial(t, url)
	defer c.conn.Close()
	c.send(t, NCMessage{Type: NCMsgJoinGame, Payload: NCJoinGamePayload{PlayerName: "사람", Team: Team2, VsBot: true}})
	joined := ncPayloadMap(t, c.waitFor(t, NCMsgPlayerJoined))
	if joined["yourTeam"] != string(Team2) {
		t.Fatalf("yourTeam = %v, want team2 (선호 팀)", joined["yourTeam"])
	}

	brain := &ncBrain{}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "봇전 진행", func(NCMessage) bool { return true })
		if msg.Type == NCMsgGameOver {
			over := ncPayloadMap(t, msg)
			if over["reason"] != "rounds_complete" {
				t.Fatalf("reason = %v, want rounds_complete", over["reason"])
			}
			return
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("넘버체인지 봇전이 20초 안에 끝나지 않았다")
}

// TestNCVsBotOpponentHiddenChoice 사람이 히든을 쓰면 봇의 블록 선택
// (제출에 선탑재된 choice=1)으로 라운드가 교착 없이 진행된다
func TestNCVsBotOpponentHiddenChoice(t *testing.T) {
	_, url, cleanup := newNCTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := ncDial(t, url)
	defer c.conn.Close()
	c.send(t, NCMessage{Type: NCMsgJoinGame, Payload: NCJoinGamePayload{PlayerName: "사람", VsBot: true}})
	joined := ncPayloadMap(t, c.waitFor(t, NCMsgPlayerJoined))
	if joined["yourTeam"] != string(Team1) {
		t.Fatalf("yourTeam = %v, want team1", joined["yourTeam"])
	}
	c.waitFor(t, NCMsgGameStart)

	c.send(t, NCMessage{Type: NCMsgSubmitBlocks,
		Payload: NCSubmitBlocksPayload{Block1: 1, Block2: 2, UseHidden: true}})

	rr := ncPayloadMap(t, c.waitFor(t, NCMsgRoundResult))
	if rr["round"].(float64) != 1 {
		t.Fatalf("round = %v, want 1", rr["round"])
	}
	if rr["team1Hidden"] != true {
		t.Fatal("team1Hidden = false, want true")
	}
	// 봇(팀2)의 선택 choice=1 → 팀1의 블록1(=1)을 받는다
	if rr["team2ReceivedBlock"].(float64) != 1 {
		t.Fatalf("team2ReceivedBlock = %v, want 1 (봇의 choice=1)", rr["team2ReceivedBlock"])
	}
}
