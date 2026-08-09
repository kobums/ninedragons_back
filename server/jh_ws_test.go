package server

import (
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// jhTestClient 공용 testConn 에 게임 메시지 타입의 waitFor 를 얹은 래퍼
type jhTestClient struct {
	testConn[JHMessage]
}

func newJHTestServer(t *testing.T, grace time.Duration) (*JHHub, string, func()) {
	t.Helper()
	hub := NewJHHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeJHWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func jhDial(t *testing.T, url string) *jhTestClient {
	t.Helper()
	return &jhTestClient{dialWS[JHMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *jhTestClient) waitFor(t *testing.T, msgType JHMessageType) JHMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m JHMessage) bool { return m.Type == msgType })
}

func jhPayloadMap(t *testing.T, msg JHMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// startJHGame 2인 입장 후 게임 시작. 각자의 세션 ID와 첫 game_state 를 반환.
func startJHGame(t *testing.T, url string) ([]*jhTestClient, []string, []map[string]interface{}) {
	t.Helper()
	clients := []*jhTestClient{jhDial(t, url), jhDial(t, url)}
	sessions := make([]string, 2)
	for i, c := range clients {
		c.send(t, JHMessage{Type: JHMsgJoinGame, Payload: JHJoinGamePayload{PlayerName: string(rune('A' + i))}})
		joined := jhPayloadMap(t, c.waitFor(t, JHMsgPlayerJoined))
		sessions[i] = joined["sessionId"].(string)
	}
	states := make([]map[string]interface{}, 2)
	for i, c := range clients {
		c.waitFor(t, JHMsgGameStart)
		states[i] = jhPayloadMap(t, c.waitFor(t, JHMsgGameState))
	}
	return clients, sessions, states
}

// TestJHMatchStartAndHiddenHands 매칭 시작 스냅샷에서 내 손패는 보이고
// 상대 손패는 개수만 보이는지, 교환 단계로 시작하는지 검증
func TestJHMatchStartAndHiddenHands(t *testing.T) {
	_, url, cleanup := newJHTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, _, states := startJHGame(t, url)
	defer func() {
		for _, c := range clients {
			c.conn.Close()
		}
	}()

	for i, state := range states {
		if state["phase"] != string(JHPhaseExchange) {
			t.Fatalf("client %d: 시작 단계가 exchange 가 아님: %v", i, state["phase"])
		}
		hand, ok := state["yourHand"].([]interface{})
		if !ok || len(hand) != JHHandSize {
			t.Fatalf("client %d: 손패가 10장이 아님: %v", i, state["yourHand"])
		}
		if state["oppHandCount"].(float64) != JHHandSize {
			t.Fatalf("client %d: 상대 손패 개수가 틀림", i)
		}
	}
	if states[0]["yourRole"] == states[1]["yourRole"] {
		t.Fatal("두 클라이언트의 역할이 같음")
	}
}

// jhBotState 봇이 스냅샷에서 꺼내 쓰는 최소 정보
type jhBotState struct {
	Phase             string `json:"phase"`
	YourTurn          bool   `json:"yourTurn"`
	LegalIndices      []int  `json:"legalIndices"`
	ExchangeCount     int    `json:"exchangeCount"`
	GreedPickCount    int    `json:"greedPickCount"`
	MustIncludePotion bool   `json:"mustIncludePotion"`
	YourHand          []struct {
		Suit string `json:"suit"`
	} `json:"yourHand"`
	OppTricks []struct{} `json:"oppTricks"`
}

// jhWsBot 상태 스냅샷에 반응해 규칙에 맞는 아무 수나 두는 봇
type jhWsBot struct {
	conn *websocket.Conn
	rng  *rand.Rand
	done chan<- string
}

func (b *jhWsBot) send(msg JHMessage) {
	data, _ := json.Marshal(msg)
	b.conn.WriteMessage(websocket.TextMessage, data)
}

func (b *jhWsBot) handleState(state jhBotState) {
	if !state.YourTurn {
		return
	}
	switch state.Phase {
	case string(JHPhaseExchange):
		indices := b.pickIndices(state, state.ExchangeCount, state.MustIncludePotion)
		b.send(JHMessage{Type: JHMsgExchangeCards, Payload: JHExchangeCardsPayload{Indices: indices}})

	case string(JHPhaseLead), string(JHPhaseFollow):
		if len(state.LegalIndices) == 0 {
			return
		}
		idx := state.LegalIndices[b.rng.Intn(len(state.LegalIndices))]
		b.send(JHMessage{Type: JHMsgPlayCard, Payload: JHPlayCardPayload{HandIndex: idx}})

	case string(JHPhaseDeclare):
		suit := jhEvilSuits[b.rng.Intn(len(jhEvilSuits))]
		b.send(JHMessage{Type: JHMsgDeclareSuit, Payload: JHDeclareSuitPayload{Suit: suit}})

	case string(JHPhasePrideSteal):
		if len(state.OppTricks) > 0 {
			b.send(JHMessage{Type: JHMsgStealTrick, Payload: JHStealTrickPayload{
				TrickIndex: b.rng.Intn(len(state.OppTricks)),
			}})
		}

	case string(JHPhaseGreedExchange):
		indices := b.pickIndices(state, state.GreedPickCount, false)
		b.send(JHMessage{Type: JHMsgGreedCards, Payload: JHGreedCardsPayload{Indices: indices}})
	}
}

// pickIndices 손패에서 무작위 n장 선택. needPotion 이면 물약 1장을 보장한다.
func (b *jhWsBot) pickIndices(state jhBotState, n int, needPotion bool) []int {
	perm := b.rng.Perm(len(state.YourHand))
	indices := perm[:n]
	if needPotion {
		hasPotion := false
		for _, i := range indices {
			if state.YourHand[i].Suit == string(JHPotion) {
				hasPotion = true
			}
		}
		if !hasPotion {
			for i, c := range state.YourHand {
				if c.Suit == string(JHPotion) {
					indices[0] = i
					break
				}
			}
		}
	}
	return indices
}

// run 초기 스냅샷을 먼저 처리하고, 이후 서버 메시지에 반응한다.
// 게임이 끝나면 done 에 승자 역할을 보낸다.
func (b *jhWsBot) run(initial map[string]interface{}) {
	if initial != nil {
		raw, _ := json.Marshal(initial)
		var state jhBotState
		if json.Unmarshal(raw, &state) == nil {
			b.handleState(state)
		}
	}

	deadline := time.Now().Add(15 * time.Second)
	b.conn.SetReadDeadline(deadline)

	for time.Now().Before(deadline) {
		_, data, err := b.conn.ReadMessage()
		if err != nil {
			return // 게임 종료 후 서버가 닫으면 여기로 나온다
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var msg JHMessage
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				continue
			}

			switch msg.Type {
			case JHMsgGameOver:
				raw, _ := json.Marshal(msg.Payload)
				var over struct {
					Winner string `json:"winner"`
				}
				json.Unmarshal(raw, &over)
				b.done <- over.Winner
				return

			case JHMsgGameState:
				raw, _ := json.Marshal(msg.Payload)
				var state jhBotState
				if json.Unmarshal(raw, &state) == nil {
					b.handleState(state)
				}
			}
		}
	}
}

// TestJHBotsCompleteGame 2인 봇이 WebSocket 프로토콜만으로 게임을 끝까지
// 완주하는지. 상태머신 어디에서도 막히지 않고 승자가 나와야 한다.
func TestJHBotsCompleteGame(t *testing.T) {
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
		bot := &jhWsBot{conn: c.conn, rng: rand.New(rand.NewSource(int64(i + 42))), done: done}
		go bot.run(states[i])
	}

	select {
	case winner := <-done:
		if winner != string(JHJekyll) && winner != string(JHHyde) {
			t.Fatalf("winner = %q, want jekyll or hyde", winner)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("15초 안에 게임이 끝나지 않았다 — 상태머신 어딘가에서 멈춤")
	}
}

// TestJHReconnectRestoresState 게임 중 연결이 끊긴 플레이어가 세션 ID로
// 재접속하면 진행 중 상태 스냅샷을 그대로 복원받는지
func TestJHReconnectRestoresState(t *testing.T) {
	_, url, cleanup := newJHTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, sessions, _ := startJHGame(t, url)
	defer clients[1].conn.Close()

	// 0번 연결 끊기 → 1번이 끊김 알림을 받는다
	clients[0].conn.Close()
	clients[1].waitFor(t, JHMsgOpponentDisconnected)

	// 세션 ID로 재접속
	rejoined := jhDial(t, url)
	defer rejoined.conn.Close()
	rejoined.send(t, JHMessage{Type: JHMsgRejoinGame, Payload: JHRejoinGamePayload{SessionID: sessions[0]}})

	state := jhPayloadMap(t, rejoined.waitFor(t, JHMsgGameState))
	if state["phase"] != string(JHPhaseExchange) {
		t.Fatalf("복원된 단계가 틀림: %v", state["phase"])
	}
	hand, ok := state["yourHand"].([]interface{})
	if !ok || len(hand) != JHHandSize {
		t.Fatalf("복원된 손패가 10장이 아님: %v", state["yourHand"])
	}
	clients[1].waitFor(t, JHMsgOpponentReconnected)
}

// TestJHGraceExpiredEndsGame 유예 시간 안에 재접속하지 않으면 상대가
// 세션 만료 통지를 받고 게임이 정리되는지
func TestJHGraceExpiredEndsGame(t *testing.T) {
	_, url, cleanup := newJHTestServer(t, 100*time.Millisecond)
	defer cleanup()

	clients, _, _ := startJHGame(t, url)
	defer clients[1].conn.Close()

	clients[0].conn.Close()
	clients[1].waitFor(t, JHMsgOpponentDisconnected)
	clients[1].waitFor(t, JHMsgSessionExpired)
}
