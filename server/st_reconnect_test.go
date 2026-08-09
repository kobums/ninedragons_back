package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stTestClient 공용 testConn 에 게임 메시지 타입의 waitFor 를 얹은 래퍼
type stTestClient struct {
	testConn[STMessage]
}

func newSTTestServer(t *testing.T, grace time.Duration) (*STHub, string, func()) {
	t.Helper()
	hub := NewSTHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeSTWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func stDial(t *testing.T, url string) *stTestClient {
	t.Helper()
	return &stTestClient{dialWS[STMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *stTestClient) waitFor(t *testing.T, msgType STMessageType) STMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m STMessage) bool { return m.Type == msgType })
}

func stPayloadMap(t *testing.T, msg STMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// startSTGame 2인 입장 후 게임 시작. 각자의 세션 ID와 첫 game_state 를 반환.
func startSTGame(t *testing.T, url string) ([]*stTestClient, []string, []map[string]interface{}) {
	t.Helper()
	clients := []*stTestClient{stDial(t, url), stDial(t, url)}
	sessions := make([]string, 2)
	for i, c := range clients {
		c.send(t, STMessage{Type: STMsgJoinGame, Payload: STJoinGamePayload{PlayerName: string(rune('A' + i))}})
		joined := stPayloadMap(t, c.waitFor(t, STMsgPlayerJoined))
		sessions[i] = joined["sessionId"].(string)
	}
	states := make([]map[string]interface{}, 2)
	for i, c := range clients {
		c.waitFor(t, STMsgGameStart)
		states[i] = stPayloadMap(t, c.waitFor(t, STMsgGameState))
	}
	return clients, sessions, states
}

// TestSTMatchStartAndHiddenHands 2인 매칭 시작 후 스냅샷에서 내 손패는
// 보이고 상대 손패는 개수만 보이는지 검증
func TestSTMatchStartAndHiddenHands(t *testing.T) {
	_, url, cleanup := newSTTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, _, states := startSTGame(t, url)
	defer func() {
		for _, c := range clients {
			c.conn.Close()
		}
	}()

	for i, state := range states {
		hand := state["yourHand"].([]interface{})
		if len(hand) != STHandSize {
			t.Fatalf("클라 %d: 손패 %d장, want %d", i, len(hand), STHandSize)
		}
		if int(state["opponentHandCount"].(float64)) != STHandSize {
			t.Fatalf("클라 %d: 상대 손패 개수가 틀리다", i)
		}
		if _, hasOppHand := state["opponentHand"]; hasOppHand {
			t.Fatalf("클라 %d: 상대 손패가 스냅샷에 샜다", i)
		}
		stones := state["stones"].([]interface{})
		if len(stones) != STStoneCount {
			t.Fatalf("클라 %d: 돌 %d개, want %d", i, len(stones), STStoneCount)
		}
	}

	// 서로 상대 진영이어야 한다
	if states[0]["yourSide"] == states[1]["yourSide"] {
		t.Fatal("두 플레이어의 진영이 같다")
	}
}

// TestSTPlayCardFlowOverWs 선공이 카드를 내면 양쪽에 이벤트·스냅샷이
// 브로드캐스트되고 턴이 넘어가는지 검증
func TestSTPlayCardFlowOverWs(t *testing.T) {
	_, url, cleanup := newSTTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, _, states := startSTGame(t, url)
	defer func() {
		for _, c := range clients {
			c.conn.Close()
		}
	}()

	// 선공 클라이언트 찾기
	firstIdx := 0
	if states[0]["currentSide"] != states[0]["yourSide"] {
		firstIdx = 1
	}
	first := clients[firstIdx]
	second := clients[1-firstIdx]

	first.send(t, STMessage{Type: STMsgPlayCard, Payload: STPlayCardPayload{HandIndex: 0, StoneIndex: 4}})

	event := stPayloadMap(t, second.waitFor(t, STMsgEvent))
	if event["kind"] != "card_played" || int(event["stoneIndex"].(float64)) != 4 {
		t.Fatalf("card_played 이벤트가 아니다: %v", event)
	}

	state := stPayloadMap(t, second.waitFor(t, STMsgGameState))
	if state["currentSide"] != state["yourSide"] {
		t.Fatalf("턴이 상대에게 넘어가야 한다: %v", state["currentSide"])
	}
	stones := state["stones"].([]interface{})
	stone4 := stones[4].(map[string]interface{})
	oppCards := stone4["oppCards"].([]interface{})
	if len(oppCards) != 1 {
		t.Fatalf("돌 4에 상대 카드 1장이 보여야 한다: %v", stone4)
	}

	// 차례가 아닌 쪽이 내면 에러
	first.send(t, STMessage{Type: STMsgPlayCard, Payload: STPlayCardPayload{HandIndex: 0, StoneIndex: 0}})
	first.waitFor(t, STMsgError)
}

// TestSTRejoinRestoresState 게임 중 끊긴 플레이어가 세션으로 복귀해
// 개인화 스냅샷을 돌려받는지 검증
func TestSTRejoinRestoresState(t *testing.T) {
	_, url, cleanup := newSTTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, sessions, states := startSTGame(t, url)
	defer clients[0].conn.Close()

	mySide := states[1]["yourSide"]

	// 클라 1 이 끊긴다
	clients[1].conn.Close()
	clients[0].waitFor(t, STMsgOpponentDisconnected)

	// 새 연결로 재접속
	rejoined := stDial(t, url)
	defer rejoined.conn.Close()
	rejoined.send(t, STMessage{Type: STMsgRejoinGame, Payload: STRejoinGamePayload{SessionID: sessions[1]}})

	clients[0].waitFor(t, STMsgOpponentReconnected)

	state := stPayloadMap(t, rejoined.waitFor(t, STMsgGameState))
	if state["yourSide"] != mySide {
		t.Fatalf("재접속 후 진영이 바뀌었다: %v → %v", mySide, state["yourSide"])
	}
	if len(state["yourHand"].([]interface{})) != STHandSize {
		t.Fatal("재접속 스냅샷에 손패가 없다")
	}
}

// TestSTGraceExpiryEndsGame 유예 만료 시 상대에게 세션 만료가 통보되는지 검증
func TestSTGraceExpiryEndsGame(t *testing.T) {
	_, url, cleanup := newSTTestServer(t, 100*time.Millisecond)
	defer cleanup()

	clients, _, _ := startSTGame(t, url)
	defer clients[0].conn.Close()

	clients[1].conn.Close()
	clients[0].waitFor(t, STMsgOpponentDisconnected)
	clients[0].waitFor(t, STMsgSessionExpired)
}
