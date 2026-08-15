package server

import (
	"testing"
	"time"
)

// ncPlayVsBotToGameOver 봇전을 완주시키고 마지막 game_over 이후 상태로 둔다
func ncPlayVsBotToGameOver(t *testing.T, c *ncTestClient) {
	t.Helper()
	brain := &ncBrain{}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "봇전 진행", func(NCMessage) bool { return true })
		if msg.Type == NCMsgGameOver {
			return
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("봇전이 20초 안에 끝나지 않았다")
}

// TestNCRematchVsBot 봇전 재대결: 신청 즉시 같은 방에서 재시작 (봇 자동 수락)
func TestNCRematchVsBot(t *testing.T) {
	_, url, cleanup := newNCTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := ncDial(t, url)
	defer c.conn.Close()
	c.send(t, NCMessage{Type: NCMsgJoinGame, Payload: NCJoinGamePayload{PlayerName: "사람", VsBot: true}})
	joined := ncPayloadMap(t, c.waitFor(t, NCMsgPlayerJoined))
	sessionID := joined["sessionId"].(string)

	ncPlayVsBotToGameOver(t, c)

	c.send(t, NCMessage{Type: NCMsgRematch})
	rejoined := ncPayloadMap(t, c.waitFor(t, NCMsgPlayerJoined))
	if rejoined["sessionId"].(string) != sessionID {
		t.Fatal("재대결 후 세션이 바뀌었다")
	}
	start := ncPayloadMap(t, c.waitFor(t, NCMsgGameStart))
	if start["yourTeam"] != string(Team1) {
		t.Fatalf("재대결 yourTeam = %v, want team1 (팀 유지)", start["yourTeam"])
	}
}

// TestNCRematchOfferHumans 사람전 재대결: 한쪽 신청 → 상대 제안 수신 → 수락 시 재시작
func TestNCRematchOfferHumans(t *testing.T) {
	_, url, cleanup := newNCTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients := []*ncTestClient{ncDial(t, url), ncDial(t, url)}
	defer func() {
		for _, c := range clients {
			c.conn.Close()
		}
	}()

	sessions := make([]string, 2)
	for i, c := range clients {
		c.send(t, NCMessage{Type: NCMsgJoinGame, Payload: NCJoinGamePayload{PlayerName: string(rune('A' + i))}})
		joined := ncPayloadMap(t, c.waitFor(t, NCMsgPlayerJoined))
		sessions[i] = joined["sessionId"].(string)
	}

	// 두 사람 모두 brain 드라이버로 12라운드 완주 (동일 전략 → 매 라운드 무승부)
	brains := []*ncBrain{{}, {}}
	for i, c := range clients {
		start := c.waitFor(t, NCMsgGameStart)
		if reply := brains[i].decide(start); reply != nil {
			c.send(t, *reply)
		}
	}
	for round := 0; round < 12; round++ {
		for i, c := range clients {
			rr := c.waitFor(t, NCMsgRoundResult)
			if reply := brains[i].decide(rr); reply != nil {
				c.send(t, *reply)
			}
		}
	}
	for _, c := range clients {
		c.waitFor(t, NCMsgGameOver)
	}

	// 한쪽 신청 → 상대가 제안을 받고 수락하면 같은 방에서 재시작
	clients[0].send(t, NCMessage{Type: NCMsgRematch})
	clients[1].waitFor(t, NCMsgRematchOffer)
	clients[1].send(t, NCMessage{Type: NCMsgRematch})

	teams := []TeamColor{Team1, Team2}
	for i, c := range clients {
		joined := ncPayloadMap(t, c.waitFor(t, NCMsgPlayerJoined))
		if joined["sessionId"].(string) != sessions[i] {
			t.Fatalf("클라 %d: 재대결 후 세션이 바뀌었다", i)
		}
		start := ncPayloadMap(t, c.waitFor(t, NCMsgGameStart))
		if start["yourTeam"] != string(teams[i]) {
			t.Fatalf("클라 %d: 재대결 yourTeam = %v, want %s", i, start["yourTeam"], teams[i])
		}
	}
}

// TestNCRematchWindowExpires 창이 지나면 세션 만료 통지와 함께 방·신원이 정리된다
func TestNCRematchWindowExpires(t *testing.T) {
	_, url, cleanup := newNCTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := ncDial(t, url)
	defer c.conn.Close()
	c.send(t, NCMessage{Type: NCMsgJoinGame, Payload: NCJoinGamePayload{PlayerName: "사람", VsBot: true}})
	c.waitFor(t, NCMsgPlayerJoined)

	ncPlayVsBotToGameOver(t, c)

	// 재대결 신청 없이 창(700ms) 경과 → 세션 만료
	c.waitFor(t, NCMsgSessionExpired)

	// 신원이 비워졌으므로 같은 연결로 새 봇전에 재입장할 수 있다
	c.send(t, NCMessage{Type: NCMsgJoinGame, Payload: NCJoinGamePayload{PlayerName: "사람", VsBot: true}})
	c.waitFor(t, NCMsgPlayerJoined)
}
