package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 테스트에서는 자동 진행 대기를 짧게 낮춘다
func init() {
	ntAfkTimeout = 120 * time.Millisecond
}

// ntTestClient 공용 testConn 에 노 땡스! 메시지 타입의 waitFor 를 얹은 래퍼
type ntTestClient struct {
	testConn[NTMessage]
}

func newNTTestServer(t *testing.T, grace time.Duration) (*NTHub, string, func()) {
	t.Helper()
	hub := NewNTHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeNTWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func ntDial(t *testing.T, url string) *ntTestClient {
	t.Helper()
	return &ntTestClient{dialWS[NTMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *ntTestClient) waitFor(t *testing.T, msgType NTMessageType) NTMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m NTMessage) bool { return m.Type == msgType })
}

func ntPayloadMap(t *testing.T, msg NTMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// ntJoin 입장하고 nt_player_joined payload 를 돌려준다
func ntJoin(t *testing.T, c *ntTestClient, name, room string) map[string]interface{} {
	t.Helper()
	c.send(t, NTMessage{Type: NTMsgJoinGame, Payload: NTJoinGamePayload{Name: name, Room: room}})
	return ntPayloadMap(t, c.waitFor(t, NTMsgPlayerJoined))
}

// ntWaitPhase 지정한 phase 의 스냅샷이 올 때까지 소비
func (c *ntTestClient) ntWaitPhase(t *testing.T, phase string) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "nt_game_state("+phase+")", func(m NTMessage) bool {
		if m.Type != NTMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		return ok && state["phase"] == phase
	})
	return ntPayloadMap(t, msg)
}

// TestNTFiveBotsCompleteGame 봇을 채운 5인 게임이 20초 안에 완주하는지 —
// 가장 중요한 회귀 장치 (교착·차례 미이동·덱 미소진 감지).
// 좌석 0은 서버 연습봇 두뇌(ntBrain)를 WS 로 감싼 드라이버가 잡는다.
func TestNTFiveBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newNTTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := ntDial(t, url)
	defer c.conn.Close()
	ntJoin(t, c, "감독", "")
	c.send(t, NTMessage{Type: NTMsgFillBots}) // 5인까지 채우고 즉시 시작

	start := time.Now()
	brain := newNTBrain()
	deadline := start.Add(20 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "state-or-over", func(m NTMessage) bool {
			return m.Type == NTMsgGameState || m.Type == NTMsgGameOver
		})
		if msg.Type == NTMsgGameOver {
			over := ntPayloadMap(t, msg)
			winners := over["winnerSeats"].([]interface{})
			if len(winners) < 1 {
				t.Fatalf("winnerSeats = %v", winners)
			}
			scores := over["scores"].([]interface{})
			if len(scores) != NTFillBotTarget {
				t.Fatalf("scores 길이 = %d, want %d", len(scores), NTFillBotTarget)
			}
			// 승자는 최저점이어야 한다
			best := scores[0].(float64)
			for _, sc := range scores {
				if sc.(float64) < best {
					best = sc.(float64)
				}
			}
			w := int(winners[0].(float64))
			if scores[w].(float64) != best {
				t.Fatalf("승자 seat%d 점수 %v ≠ 최저점 %v", w, scores[w], best)
			}
			if _, ok := over["winnerNames"].([]interface{}); !ok {
				t.Fatalf("winnerNames 부재: %v", over)
			}
			// 종료 payload 의 players 는 전원 칩 공개 (은닉 -1 없음)
			for _, pRaw := range over["players"].([]interface{}) {
				p := pRaw.(map[string]interface{})
				if int(p["chips"].(float64)) == -1 {
					t.Fatalf("game_over 에서 칩 미공개: %v", p)
				}
				if _, ok := p["cards"].([]interface{}); !ok {
					t.Fatalf("cards 가 배열이 아니다 (null?): %v", p)
				}
			}
			t.Logf("완주: winners=%v scores=%v (%.1fs)", winners, scores, time.Since(start).Seconds())
			return
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("20초 안에 게임이 끝나지 않았다 — 진행 불가 상태")
}

// TestNTAfkAutoAction 접속만 유지한 채 아무것도 하지 않는 사람의 차례를
// AFK 타이머가 자동 처리하는지 (칩 있으면 패스 — endsAt 노출 포함)
func TestNTAfkAutoAction(t *testing.T) {
	_, url, cleanup := newNTTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := ntDial(t, url)
	defer host.conn.Close()
	ntJoin(t, host, "잠수1", "")

	guest := ntDial(t, url)
	defer guest.conn.Close()
	ntJoin(t, guest, "잠수2", "")

	third := ntDial(t, url)
	defer third.conn.Close()
	ntJoin(t, third, "잠수3", "")

	host.send(t, NTMessage{Type: NTMsgStart})
	state := guest.ntWaitPhase(t, string(NTPhasePlaying))
	if ends := int64(state["endsAt"].(float64)); ends <= 0 {
		t.Fatalf("playing 스냅샷의 endsAt = %d, want unixMillis", ends)
	}
	if int(state["card"].(float64)) < NTCardMin {
		t.Fatalf("공개 카드 = %v", state["card"])
	}

	// 전원 무행동 → AFK 이벤트(한글 문구)와 자동 패스 확인 (시작 칩 11개)
	afk := ntPayloadMap(t, guest.waitMatch(t, "afk-event", func(m NTMessage) bool {
		if m.Type != NTMsgEvent {
			return false
		}
		ev, ok := m.Payload.(map[string]interface{})
		return ok && ev["kind"] == "afk"
	}))
	if !strings.Contains(afk["message"].(string), "자동으로 패스") {
		t.Fatalf("AFK 문구 = %v", afk["message"])
	}
	if _, has := afk["name"]; !has {
		t.Fatalf("AFK 이벤트에 name 부재: %v", afk)
	}

	// 자동 패스가 반영된 스냅샷 — 얹힌 칩이 1개 이상으로 늘어난다
	potState := guest.waitMatch(t, "pot-grown", func(m NTMessage) bool {
		if m.Type != NTMsgGameState {
			return false
		}
		st, ok := m.Payload.(map[string]interface{})
		return ok && st["potChips"].(float64) >= 1
	})
	pot := ntPayloadMap(t, potState)
	if pot["phase"] != string(NTPhasePlaying) {
		t.Fatalf("자동 패스 후 phase = %v", pot["phase"])
	}
}

// TestNTChipsHiddenAndGameOverReveal 칩 은닉의 핵심 계약 — 허브 고루틴 없이
// 핸들러를 직접 불러 결정적으로 검증한다.
//   playing: viewer 본인 좌석의 chips 만 실값, 타인은 -1. 관전자는 전원 -1.
//   game_over: 전원 실값 공개 + score 확정 (그 전에는 score 0).
//   획득 카드는 언제나 전원에게 공개 ([] — null 아님).
func TestNTChipsHiddenAndGameOverReveal(t *testing.T) {
	h := NewNTHub()
	room := h.lobbyRoomFor("")
	clients := make([]*NTClient, 3)
	for i := range clients {
		c := &NTClient{wsClient: newBotWSClient(), Hub: h}
		c.Bot = false // 소켓 없는 사람 취급
		c.Name = fmt.Sprintf("P%d", i)
		seat, err := room.Game.AddPlayer(c.Name)
		if err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
		c.GameID = room.Game.ID
		c.Seat = seat
		room.Clients[seat] = c
		h.sessions[c.SessionID] = c
		clients[i] = c
	}
	h.startGame(room)

	game := room.Game
	// 결정적인 칩 분포를 강제
	chips := []int{11, 7, 3}
	for i, p := range game.Players {
		p.Chips = chips[i]
	}
	game.Players[1].Cards = []int{20, 21} // 공개 정보 확인용

	// ---- playing: 본인만 실값, 타인은 -1 ----
	for viewer := 0; viewer < 3; viewer++ {
		state := h.buildNTState(room, viewer)
		if state.YourSeat != viewer || state.Phase != NTPhasePlaying {
			t.Fatalf("viewer %d: yourSeat=%d phase=%s", viewer, state.YourSeat, state.Phase)
		}
		for _, pv := range state.Players {
			if pv.Seat == viewer {
				if pv.Chips != chips[pv.Seat] {
					t.Fatalf("viewer %d: 자기 칩 = %d, want 실값 %d", viewer, pv.Chips, chips[pv.Seat])
				}
			} else if pv.Chips != -1 {
				t.Fatalf("viewer %d: seat%d chips=%d 가 샜다, want -1", viewer, pv.Seat, pv.Chips)
			}
			if pv.Score != 0 {
				t.Fatalf("viewer %d: 진행 중 score = %d, want 0", viewer, pv.Score)
			}
			if pv.Cards == nil {
				t.Fatalf("viewer %d: seat%d cards 가 nil", viewer, pv.Seat)
			}
		}
		// 획득 카드는 누구에게나 공개
		if got := state.Players[1].Cards; len(got) != 2 || got[0] != 20 || got[1] != 21 {
			t.Fatalf("viewer %d: seat1 cards = %v, want [20 21]", viewer, got)
		}
	}
	// 관전자(-1)는 전원 -1 (타인 은닉이 관전자에게도 적용된다)
	spec := h.buildNTState(room, -1)
	if spec.YourSeat != -1 {
		t.Fatalf("관전자 yourSeat = %d", spec.YourSeat)
	}
	for _, pv := range spec.Players {
		if pv.Chips != -1 {
			t.Fatalf("관전자: seat%d chips=%d 가 샜다, want -1", pv.Seat, pv.Chips)
		}
	}

	// ---- 덱 소진 → game_over: 전원 실값 + score ----
	game.Deck = []int{}
	taker := game.CurrentSeat
	h.handleGameMessage(NTGameMessage{Client: clients[taker], Message: NTMessage{Type: NTMsgTake}})
	if game.Phase != NTPhaseGameOver {
		t.Fatalf("phase = %s, want game_over", game.Phase)
	}
	for viewer := -1; viewer < 3; viewer++ {
		state := h.buildNTState(room, viewer)
		for _, pv := range state.Players {
			p := game.Players[pv.Seat]
			if pv.Chips != p.Chips || pv.Chips == -1 {
				t.Fatalf("game_over viewer %d: seat%d chips=%d, want 공개 %d",
					viewer, pv.Seat, pv.Chips, p.Chips)
			}
			if pv.Score != ntScore(p.Cards, p.Chips) {
				t.Fatalf("game_over viewer %d: seat%d score=%d, want %d",
					viewer, pv.Seat, pv.Score, ntScore(p.Cards, p.Chips))
			}
		}
	}
}

// TestNTRoomCodeAndSpectate 방 코드 발급·코드 입장·시작 후 코드 관전 진입.
// 관전자 스냅샷은 yourSeat -1 에 전원 칩 -1 (은닉 유지). 관전자의 행동은
// 전부 차단된다.
func TestNTRoomCodeAndSpectate(t *testing.T) {
	_, url, cleanup := newNTTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := ntDial(t, url)
	defer host.conn.Close()
	joined := ntJoin(t, host, "호스트", "NEW")
	code, _ := joined["roomCode"].(string)
	if len(code) != roomCodeLen {
		t.Fatalf("roomCode = %q, want %d자", code, roomCodeLen)
	}

	guest := ntDial(t, url)
	defer guest.conn.Close()
	guestJoined := ntJoin(t, guest, "친구1", code)
	if guestJoined["roomCode"] != code || int(guestJoined["yourSeat"].(float64)) != 1 {
		t.Fatalf("코드 입장 실패: %v", guestJoined)
	}

	third := ntDial(t, url)
	defer third.conn.Close()
	ntJoin(t, third, "친구2", code)

	// 호스트가 시작 (3인) — 사람만이라 AFK 자동 진행으로 천천히 흘러간다
	host.send(t, NTMessage{Type: NTMsgStart})
	state := host.ntWaitPhase(t, string(NTPhasePlaying))
	if state["roomCode"] != code || len(state["players"].([]interface{})) != 3 {
		t.Fatalf("시작 실패: %v", state["players"])
	}

	// 시작된 방의 코드로 들어오면 관전자
	spec := ntDial(t, url)
	defer spec.conn.Close()
	spec.send(t, NTMessage{Type: NTMsgJoinGame, Payload: NTJoinGamePayload{Name: "구경꾼", Room: code}})
	specJoined := ntPayloadMap(t, spec.waitFor(t, NTMsgSpectateJoined))
	if specJoined["roomCode"] != code {
		t.Fatalf("관전 입장 roomCode = %v", specJoined["roomCode"])
	}

	specState := ntPayloadMap(t, spec.waitFor(t, NTMsgGameState))
	if int(specState["yourSeat"].(float64)) != -1 {
		t.Fatalf("관전자 yourSeat = %v, want -1", specState["yourSeat"])
	}
	// 관전자에게도 칩은 전원 은닉(-1)이고 획득 카드는 배열로 보인다
	for _, pRaw := range specState["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		if int(p["chips"].(float64)) != -1 {
			t.Fatalf("관전자에게 칩이 샜다: %v", p)
		}
		if _, ok := p["cards"].([]interface{}); !ok {
			t.Fatalf("cards 가 배열이 아니다 (null?): %v", p)
		}
	}

	// 관전자는 어떤 행동도 못 한다
	spec.send(t, NTMessage{Type: NTMsgPass})
	errPayload := ntPayloadMap(t, spec.waitFor(t, NTMsgError))
	if errPayload["message"] != spectatorDeniedMsg {
		t.Fatalf("관전자 차단 문구 = %v", errPayload["message"])
	}
}
