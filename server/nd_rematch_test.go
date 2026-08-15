package server

import (
	"testing"
	"time"
)

// ndPlayVsBotToGameOver 봇전을 완주시키고 마지막 game_over 이후 상태로 둔다
func ndPlayVsBotToGameOver(t *testing.T, c *testClient) {
	t.Helper()
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
	t.Fatal("봇전이 20초 안에 끝나지 않았다")
}

// TestNDRematchVsBot 봇전 재대결: 신청 즉시 같은 방에서 재시작 (봇 자동 수락)
func TestNDRematchVsBot(t *testing.T) {
	_, url, cleanup := newTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := dial(t, url)
	defer c.conn.Close()
	c.send(t, Message{Type: MsgJoinGame, Payload: JoinGamePayload{PlayerName: "사람", VsBot: true}})
	joined := payloadMap(t, c.waitFor(t, MsgPlayerJoined))
	sessionID := joined["sessionId"].(string)

	ndPlayVsBotToGameOver(t, c)

	c.send(t, Message{Type: MsgRematch})
	rejoined := payloadMap(t, c.waitFor(t, MsgPlayerJoined))
	if rejoined["sessionId"].(string) != sessionID {
		t.Fatal("재대결 후 세션이 바뀌었다")
	}
	start := payloadMap(t, c.waitFor(t, MsgGameStart))
	if start["yourColor"] != string(Blue) {
		t.Fatalf("재대결 yourColor = %v, want blue (색상 유지)", start["yourColor"])
	}
}

// TestNDRematchOfferHumans 사람전 재대결: 한쪽 신청 → 상대 제안 수신 → 수락 시 재시작
func TestNDRematchOfferHumans(t *testing.T) {
	_, url, cleanup := newTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c1, c2, session1, session2 := joinTwoPlayers(t, url)
	defer c1.conn.Close()
	defer c2.conn.Close()

	// 파랑(선공)이 매 라운드 이기는 대본으로 5선승 완주 (1 vs 9 특수 규칙 회피)
	blueTiles := []int{5, 6, 7, 8, 9}
	redTiles := []int{4, 3, 2, 1, 5}
	for i := 0; i < 5; i++ {
		c1.send(t, Message{Type: MsgPlayTile, Payload: PlayTilePayload{Tile: blueTiles[i]}})
		c2.waitFor(t, MsgTilePlayed)
		c2.send(t, Message{Type: MsgPlayTile, Payload: PlayTilePayload{Tile: redTiles[i]}})
		c1.waitFor(t, MsgRoundResult)
		c2.waitFor(t, MsgRoundResult)
	}
	over := payloadMap(t, c1.waitFor(t, MsgGameOver))
	if over["winner"] != string(Blue) {
		t.Fatalf("winner = %v, want blue", over["winner"])
	}
	c2.waitFor(t, MsgGameOver)

	// 한쪽 신청 → 상대가 제안을 받고 수락하면 같은 방에서 재시작
	c1.send(t, Message{Type: MsgRematch})
	c2.waitFor(t, MsgRematchOffer)
	c2.send(t, Message{Type: MsgRematch})

	sessions := []string{session1, session2}
	colors := []PlayerColor{Blue, Red}
	for i, c := range []*testClient{c1, c2} {
		joined := payloadMap(t, c.waitFor(t, MsgPlayerJoined))
		if joined["sessionId"].(string) != sessions[i] {
			t.Fatalf("클라 %d: 재대결 후 세션이 바뀌었다", i)
		}
		start := payloadMap(t, c.waitFor(t, MsgGameStart))
		if start["yourColor"] != string(colors[i]) {
			t.Fatalf("클라 %d: 재대결 yourColor = %v, want %s", i, start["yourColor"], colors[i])
		}
		if start["firstPlayer"] != string(Blue) {
			t.Fatalf("클라 %d: firstPlayer = %v, want blue", i, start["firstPlayer"])
		}
	}

	// 새 판이 실제로 진행 가능한지: 파랑이 첫 타일을 내면 양쪽에 브로드캐스트
	c1.send(t, Message{Type: MsgPlayTile, Payload: PlayTilePayload{Tile: 1}})
	c1.waitFor(t, MsgTilePlayed)
	c2.waitFor(t, MsgTilePlayed)
}

// TestNDRematchWindowExpires 창이 지나면 세션 만료 통지와 함께 방·신원이 정리된다
func TestNDRematchWindowExpires(t *testing.T) {
	_, url, cleanup := newTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := dial(t, url)
	defer c.conn.Close()
	c.send(t, Message{Type: MsgJoinGame, Payload: JoinGamePayload{PlayerName: "사람", VsBot: true}})
	c.waitFor(t, MsgPlayerJoined)

	ndPlayVsBotToGameOver(t, c)

	// 재대결 신청 없이 창(700ms) 경과 → 세션 만료
	c.waitFor(t, MsgSessionExpired)

	// 신원이 비워졌으므로 같은 연결로 새 봇전에 재입장할 수 있다
	c.send(t, Message{Type: MsgJoinGame, Payload: JoinGamePayload{PlayerName: "사람", VsBot: true}})
	c.waitFor(t, MsgPlayerJoined)
}
