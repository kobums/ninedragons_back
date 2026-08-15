package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 테스트에서는 핸드 사이 대기를 짧게 (전원 ready 경로가 주지만 안전망)
func init() {
	tcHandEndDelay = 100 * time.Millisecond
}

// tcTestClient 공용 testConn 에 티츄 메시지 타입의 waitFor 를 얹은 래퍼
type tcTestClient struct {
	testConn[TCMessage]
}

func newTCTestServer(t *testing.T, grace time.Duration) (*TCHub, string, func()) {
	t.Helper()
	hub := NewTCHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeTCWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func tcDial(t *testing.T, url string) *tcTestClient {
	t.Helper()
	return &tcTestClient{dialWS[TCMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *tcTestClient) waitFor(t *testing.T, msgType TCMessageType) TCMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m TCMessage) bool { return m.Type == msgType })
}

func tcPayloadMap(t *testing.T, msg TCMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// tcJoin 입장하고 sessionId 를 회수한다
func tcJoin(t *testing.T, c *tcTestClient, name string) string {
	t.Helper()
	c.send(t, TCMessage{Type: TCMsgJoinGame, Payload: TCJoinGamePayload{Name: name}})
	joined := tcPayloadMap(t, c.waitFor(t, TCMsgPlayerJoined))
	return joined["sessionId"].(string)
}

// tcWaitPhase 지정한 phase 의 스냅샷이 올 때까지 소비
func (c *tcTestClient) tcWaitPhase(t *testing.T, phase string) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "tc_game_state("+phase+")", func(m TCMessage) bool {
		if m.Type != TCMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		return ok && state["phase"] == phase
	})
	return tcPayloadMap(t, msg)
}

// TestTCFourBotsCompleteGame 봇 4인이 목표 500 한 게임을 20초 안에 완주하는지
// — 가장 중요한 회귀 장치 (교착: 전원 패스 무한·소원 미해제·개 리드 등 감지).
// 좌석 0은 서버 연습봇 두뇌(tcBrain)를 WS 로 감싼 드라이버가 잡는다.
func TestTCFourBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newTCTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := tcDial(t, url)
	defer c.conn.Close()
	tcJoin(t, c, "감독")
	c.send(t, TCMessage{Type: TCMsgFillBots})

	start := time.Now()
	brain := newTCBrain()
	deadline := start.Add(20 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "state-or-over", func(m TCMessage) bool {
			return m.Type == TCMsgGameState || m.Type == TCMsgGameOver
		})
		if msg.Type == TCMsgGameOver {
			over := tcPayloadMap(t, msg)
			winner := over["winnerTeam"].(string)
			if winner != "02" && winner != "13" {
				t.Fatalf("winnerTeam = %q", winner)
			}
			scores := over["scores"].(map[string]interface{})
			s02 := int(scores["team02"].(float64))
			s13 := int(scores["team13"].(float64))
			top := s02
			if s13 > top {
				top = s13
			}
			if top < 500 {
				t.Fatalf("목표 미달로 종료: 02=%d 13=%d", s02, s13)
			}
			t.Logf("완주: winner=%s 02=%d 13=%d (%.1fs)", winner, s02, s13, time.Since(start).Seconds())
			return
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("20초 안에 게임이 끝나지 않았다 — 진행 불가 상태")
}

// TestTCHiddenHands 그랜드 단계 스냅샷에서 내 손패는 8장, 남의 손패는
// 개수(handCount)만 실리는지 — 은닉 검증 (players 항목에 hand 필드 부재).
func TestTCHiddenHands(t *testing.T) {
	_, url, cleanup := newTCTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients := []*tcTestClient{}
	for i := 0; i < TCSeats; i++ {
		c := tcDial(t, url)
		defer c.conn.Close()
		tcJoin(t, c, string(rune('A'+i)))
		clients = append(clients, c)
	}

	for i, c := range clients {
		state := c.tcWaitPhase(t, TCPhaseGrand)
		if int(state["yourSeat"].(float64)) != i {
			t.Fatalf("클라 %d: yourSeat = %v", i, state["yourSeat"])
		}
		hand := state["yourHand"].([]interface{})
		if len(hand) != 8 {
			t.Fatalf("클라 %d: 그랜드 손패 %d장, want 8", i, len(hand))
		}
		players := state["players"].([]interface{})
		if len(players) != TCSeats {
			t.Fatalf("players %d명", len(players))
		}
		for _, pRaw := range players {
			p := pRaw.(map[string]interface{})
			for _, forbidden := range []string{"hand", "yourHand", "cards"} {
				if _, has := p[forbidden]; has {
					t.Fatalf("클라 %d: 남의 손패가 샜다 (%s): %v", i, forbidden, p)
				}
			}
			if int(p["handCount"].(float64)) != 8 {
				t.Fatalf("클라 %d: handCount = %v, want 8", i, p["handCount"])
			}
		}
	}
}

// TestTCRejoinRestoresSnapshot 재접속 시 개인화 스냅샷이 복원되는지
func TestTCRejoinRestoresSnapshot(t *testing.T) {
	_, url, cleanup := newTCTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients := []*tcTestClient{}
	sessions := []string{}
	for i := 0; i < TCSeats; i++ {
		c := tcDial(t, url)
		sessions = append(sessions, tcJoin(t, c, string(rune('A'+i))))
		clients = append(clients, c)
	}
	for _, c := range clients {
		c.tcWaitPhase(t, TCPhaseGrand)
	}

	// seat2 연결 끊김 → 다른 클라이언트에 통지
	clients[2].conn.Close()
	disc := tcPayloadMap(t, clients[0].waitFor(t, TCMsgOpponentDisconnected))
	if int(disc["seat"].(float64)) != 2 {
		t.Fatalf("끊긴 좌석 = %v, want 2", disc["seat"])
	}

	// 재접속 → 스냅샷 복원 + 은닉 유지
	rejoined := tcDial(t, url)
	defer rejoined.conn.Close()
	defer clients[0].conn.Close()
	defer clients[1].conn.Close()
	defer clients[3].conn.Close()
	rejoined.send(t, TCMessage{Type: TCMsgRejoin, Payload: TCRejoinPayload{SessionID: sessions[2]}})
	clients[0].waitFor(t, TCMsgReconnected)

	state := rejoined.tcWaitPhase(t, TCPhaseGrand)
	if int(state["yourSeat"].(float64)) != 2 {
		t.Fatalf("복원 yourSeat = %v, want 2", state["yourSeat"])
	}
	if len(state["yourHand"].([]interface{})) != 8 {
		t.Fatalf("복원 손패 %d장", len(state["yourHand"].([]interface{})))
	}
	for _, pRaw := range state["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		if _, has := p["hand"]; has {
			t.Fatalf("재접속 스냅샷에서 남의 손패가 샜다: %v", p)
		}
	}
}

// TestTCBotTakeover 유예 만료 시 좌석이 봇으로 대체되고 게임이 계속되는지
func TestTCBotTakeover(t *testing.T) {
	_, url, cleanup := newTCTestServer(t, 150*time.Millisecond)
	defer cleanup()

	clients := []*tcTestClient{}
	sessions := []string{}
	for i := 0; i < TCSeats; i++ {
		c := tcDial(t, url)
		sessions = append(sessions, tcJoin(t, c, string(rune('A'+i))))
		clients = append(clients, c)
	}
	for _, c := range clients {
		c.tcWaitPhase(t, TCPhaseGrand)
	}

	clients[3].conn.Close()
	defer clients[0].conn.Close()
	defer clients[1].conn.Close()
	defer clients[2].conn.Close()

	// 봇 대체 이벤트 대기
	clients[0].waitMatch(t, "bot_takeover", func(m TCMessage) bool {
		if m.Type != TCMsgEvent {
			return false
		}
		ev, ok := m.Payload.(map[string]interface{})
		return ok && ev["kind"] == "bot_takeover"
	})

	// 스냅샷에 봇 플래그 반영 + 봇이 그랜드에 응답해 게임이 진행된다
	state := clients[0].waitMatch(t, "bot-flag state", func(m TCMessage) bool {
		if m.Type != TCMsgGameState {
			return false
		}
		st, ok := m.Payload.(map[string]interface{})
		if !ok {
			return false
		}
		players := st["players"].([]interface{})
		p3 := players[3].(map[string]interface{})
		return p3["bot"] == true
	})
	_ = state

	// 만료된 세션으로는 재접속 불가
	late := tcDial(t, url)
	defer late.conn.Close()
	late.send(t, TCMessage{Type: TCMsgRejoin, Payload: TCRejoinPayload{SessionID: sessions[3]}})
	late.waitFor(t, TCMsgSessionExpired)
}
