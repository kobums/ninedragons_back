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
	ipAfkTimeout = 120 * time.Millisecond
	ipRoundEndDelay = 40 * time.Millisecond
}

// ipTestClient 공용 testConn 에 인디언 포커 메시지 타입의 waitFor 를 얹은 래퍼
type ipTestClient struct {
	testConn[IPMessage]
}

func newIPTestServer(t *testing.T, grace time.Duration) (*IPHub, string, func()) {
	t.Helper()
	hub := NewIPHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeIPWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func ipDial(t *testing.T, url string) *ipTestClient {
	t.Helper()
	return &ipTestClient{dialWS[IPMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *ipTestClient) waitFor(t *testing.T, msgType IPMessageType) IPMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m IPMessage) bool { return m.Type == msgType })
}

func ipPayloadMap(t *testing.T, msg IPMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// ipJoin 입장하고 ip_player_joined payload 를 돌려준다
func ipJoin(t *testing.T, c *ipTestClient, name, room string) map[string]interface{} {
	t.Helper()
	c.send(t, IPMessage{Type: IPMsgJoinGame, Payload: IPJoinGamePayload{Name: name, Room: room}})
	return ipPayloadMap(t, c.waitFor(t, IPMsgPlayerJoined))
}

// ipWaitPhase 지정한 phase 의 스냅샷이 올 때까지 소비
func (c *ipTestClient) ipWaitPhase(t *testing.T, phase string) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "ip_game_state("+phase+")", func(m IPMessage) bool {
		if m.Type != IPMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		return ok && state["phase"] == phase
	})
	return ipPayloadMap(t, msg)
}

// TestIPFourBotsCompleteGame 봇을 채운 4인 게임이 20초 안에 완주하는지 —
// 가장 중요한 회귀 장치 (교착·라운드 미전환·차례 미이동 감지).
// 좌석 0은 서버 연습봇 두뇌(ipBrain)를 WS 로 감싼 드라이버가 잡는다.
func TestIPFourBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newIPTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := ipDial(t, url)
	defer c.conn.Close()
	ipJoin(t, c, "감독", "")
	c.send(t, IPMessage{Type: IPMsgFillBots}) // 4인까지 채우고 즉시 시작

	start := time.Now()
	brain := newIPBrain()
	deadline := start.Add(20 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "state-or-over", func(m IPMessage) bool {
			return m.Type == IPMsgGameState || m.Type == IPMsgGameOver
		})
		if msg.Type == IPMsgGameOver {
			over := ipPayloadMap(t, msg)
			winners := over["winnerSeats"].([]interface{})
			if len(winners) < 1 {
				t.Fatalf("winnerSeats = %v", winners)
			}
			chips := over["chips"].([]interface{})
			if len(chips) != IPFillBotTarget {
				t.Fatalf("chips 길이 = %d, want %d", len(chips), IPFillBotTarget)
			}
			// 승자는 최다 칩이어야 한다
			best := 0.0
			for _, ch := range chips {
				if ch.(float64) > best {
					best = ch.(float64)
				}
			}
			w := int(winners[0].(float64))
			if chips[w].(float64) != best {
				t.Fatalf("승자 seat%d 칩 %v ≠ 최다 칩 %v", w, chips[w], best)
			}
			if _, ok := over["winnerNames"].([]interface{}); !ok {
				t.Fatalf("winnerNames 부재: %v", over)
			}
			t.Logf("완주: winners=%v chips=%v (%.1fs)", winners, chips, time.Since(start).Seconds())
			return
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("20초 안에 게임이 끝나지 않았다 — 진행 불가 상태")
}

// TestIPAfkAutoCall 접속만 유지한 채 아무것도 하지 않는 사람의 차례를
// AFK 타이머가 자동 콜 처리하는지 (endsAt 노출·쇼다운 도달 포함)
func TestIPAfkAutoCall(t *testing.T) {
	_, url, cleanup := newIPTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := ipDial(t, url)
	defer host.conn.Close()
	ipJoin(t, host, "잠수1", "")

	guest := ipDial(t, url)
	defer guest.conn.Close()
	ipJoin(t, guest, "잠수2", "")

	host.send(t, IPMessage{Type: IPMsgStart})
	state := guest.ipWaitPhase(t, string(IPPhaseBetting))
	if ends := int64(state["endsAt"].(float64)); ends <= 0 {
		t.Fatalf("betting 스냅샷의 endsAt = %d, want unixMillis", ends)
	}

	// 전원 무행동 → AFK 이벤트(한글 문구)와 자동 콜 확인
	afk := ipPayloadMap(t, guest.waitMatch(t, "afk-event", func(m IPMessage) bool {
		if m.Type != IPMsgEvent {
			return false
		}
		ev, ok := m.Payload.(map[string]interface{})
		return ok && ev["kind"] == "afk"
	}))
	if !strings.Contains(afk["message"].(string), "자동으로 콜") {
		t.Fatalf("AFK 문구 = %v", afk["message"])
	}
	if _, has := afk["name"]; !has {
		t.Fatalf("AFK 이벤트에 name 부재: %v", afk)
	}

	// 두 명 다 자동 콜(체크)되면 쇼다운 — 생존자 카드가 전원 공개된다
	show := guest.ipWaitPhase(t, string(IPPhaseShowdown))
	if show["roundResult"] == nil {
		t.Fatal("showdown 스냅샷에 roundResult 부재")
	}
	for _, pRaw := range show["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		if int(p["card"].(float64)) == 0 {
			t.Fatalf("쇼다운에서 카드 미공개: %v", p)
		}
	}
}

// TestIPReverseHidden 역은닉의 핵심 계약 — 허브 고루틴 없이 핸들러를 직접
// 불러 결정적으로 검증한다.
//   betting: viewer 본인 좌석의 card 만 0, 타인은 실값. 관전자는 전원 실값.
//   showdown: 생존자 전원 실값 (본인 포함).
//   폴드자: 어느 단계·어느 시점에도 전원에게 0 (본인·관전자 포함).
func TestIPReverseHidden(t *testing.T) {
	h := NewIPHub()
	room := h.lobbyRoomFor("")
	clients := make([]*IPClient, 3)
	for i := range clients {
		c := &IPClient{wsClient: newBotWSClient(), Hub: h}
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
	cards := []int{7, 3, 9}
	for i, p := range game.Players {
		p.Card = cards[i]
	}

	// ---- betting: 본인만 0, 타인은 실값 ----
	for viewer := 0; viewer < 3; viewer++ {
		state := h.buildIPState(room, viewer)
		if state.YourSeat != viewer || state.Phase != IPPhaseBetting {
			t.Fatalf("viewer %d: yourSeat=%d phase=%s", viewer, state.YourSeat, state.Phase)
		}
		for _, pv := range state.Players {
			if pv.Seat == viewer {
				if pv.Card != 0 {
					t.Fatalf("viewer %d: 자기 카드가 샜다 (card=%d)", viewer, pv.Card)
				}
			} else if pv.Card != cards[pv.Seat] {
				t.Fatalf("viewer %d: seat%d card=%d, want 실값 %d",
					viewer, pv.Seat, pv.Card, cards[pv.Seat])
			}
		}
	}
	// 관전자(-1)는 전원 실값
	spec := h.buildIPState(room, -1)
	if spec.YourSeat != -1 {
		t.Fatalf("관전자 yourSeat = %d", spec.YourSeat)
	}
	for _, pv := range spec.Players {
		if pv.Card != cards[pv.Seat] {
			t.Fatalf("관전자: seat%d card=%d, want %d", pv.Seat, pv.Card, cards[pv.Seat])
		}
	}

	// ---- 전원 체크 → showdown: 생존자 전원 실값 (본인 포함) ----
	for game.Phase == IPPhaseBetting {
		cur := game.CurrentSeat
		h.handleGameMessage(IPGameMessage{Client: clients[cur], Message: IPMessage{Type: IPMsgCall}})
	}
	if game.Phase != IPPhaseShowdown {
		t.Fatalf("phase = %s, want showdown", game.Phase)
	}
	for viewer := 0; viewer < 3; viewer++ {
		state := h.buildIPState(room, viewer)
		for _, pv := range state.Players {
			if pv.Card != cards[pv.Seat] {
				t.Fatalf("showdown viewer %d: seat%d card=%d, want 실값 %d (본인 포함 공개)",
					viewer, pv.Seat, pv.Card, cards[pv.Seat])
			}
		}
		if state.RoundResult == nil || len(state.RoundResult.WinnerSeats) == 0 {
			t.Fatalf("showdown viewer %d: roundResult 부재", viewer)
		}
	}

	// ---- 2라운드: 폴드자는 누구에게나 0 유지 ----
	h.handleRoundFired(ipRoundSignal{GameID: game.ID, Round: 1})
	if game.Phase != IPPhaseBetting || game.Round != 2 {
		t.Fatalf("2라운드 진입 실패: phase=%s round=%d", game.Phase, game.Round)
	}
	for i, p := range game.Players {
		p.Card = cards[i] // 다시 결정적인 카드로
	}
	folder := game.CurrentSeat
	h.handleGameMessage(IPGameMessage{Client: clients[folder], Message: IPMessage{Type: IPMsgFold}})

	// 폴드 직후(betting)에도 폴드자 카드는 0
	for viewer := -1; viewer < 3; viewer++ {
		state := h.buildIPState(room, viewer)
		for _, pv := range state.Players {
			if pv.Seat == folder && pv.Card != 0 {
				t.Fatalf("betting viewer %d: 폴드자 seat%d 카드가 샜다 (card=%d)",
					viewer, folder, pv.Card)
			}
		}
	}

	// 남은 둘이 체크 → showdown — 폴드자만 0, 생존자는 전원 실값
	for game.Phase == IPPhaseBetting {
		cur := game.CurrentSeat
		h.handleGameMessage(IPGameMessage{Client: clients[cur], Message: IPMessage{Type: IPMsgCall}})
	}
	if game.Phase != IPPhaseShowdown {
		t.Fatalf("2라운드 phase = %s, want showdown", game.Phase)
	}
	for viewer := -1; viewer < 3; viewer++ {
		state := h.buildIPState(room, viewer)
		for _, pv := range state.Players {
			if pv.Seat == folder {
				if pv.Card != 0 {
					t.Fatalf("showdown viewer %d: 폴드자 카드가 샜다 (card=%d)", viewer, pv.Card)
				}
				if !pv.Folded {
					t.Fatalf("showdown viewer %d: folded 플래그 유실", viewer)
				}
			} else if pv.Card != cards[pv.Seat] {
				t.Fatalf("showdown viewer %d: 생존자 seat%d card=%d, want %d",
					viewer, pv.Seat, pv.Card, cards[pv.Seat])
			}
		}
	}
}

// TestIPRoomCodeAndSpectate 방 코드 발급·코드 입장·시작 후 코드 관전 진입.
// 관전자 스냅샷은 yourSeat -1 에 전원 카드 공개 (자기 카드가 없으므로
// 은닉 위반 아님). 관전자의 행동은 전부 차단된다.
func TestIPRoomCodeAndSpectate(t *testing.T) {
	_, url, cleanup := newIPTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := ipDial(t, url)
	defer host.conn.Close()
	joined := ipJoin(t, host, "호스트", "NEW")
	code, _ := joined["roomCode"].(string)
	if len(code) != roomCodeLen {
		t.Fatalf("roomCode = %q, want %d자", code, roomCodeLen)
	}

	guest := ipDial(t, url)
	defer guest.conn.Close()
	guestJoined := ipJoin(t, guest, "친구", code)
	if guestJoined["roomCode"] != code || int(guestJoined["yourSeat"].(float64)) != 1 {
		t.Fatalf("코드 입장 실패: %v", guestJoined)
	}

	// 호스트가 시작 (2인) — 사람만이라 AFK 자동 콜로 천천히 진행된다
	host.send(t, IPMessage{Type: IPMsgStart})
	state := host.ipWaitPhase(t, string(IPPhaseBetting))
	if state["roomCode"] != code || len(state["players"].([]interface{})) != 2 {
		t.Fatalf("시작 실패: %v", state["players"])
	}

	// 시작된 방의 코드로 들어오면 관전자
	spec := ipDial(t, url)
	defer spec.conn.Close()
	spec.send(t, IPMessage{Type: IPMsgJoinGame, Payload: IPJoinGamePayload{Name: "구경꾼", Room: code}})
	specJoined := ipPayloadMap(t, spec.waitFor(t, IPMsgSpectateJoined))
	if specJoined["roomCode"] != code {
		t.Fatalf("관전 입장 roomCode = %v", specJoined["roomCode"])
	}

	specState := ipPayloadMap(t, spec.waitFor(t, IPMsgGameState))
	if int(specState["yourSeat"].(float64)) != -1 {
		t.Fatalf("관전자 yourSeat = %v, want -1", specState["yourSeat"])
	}
	// 관전자에게는 폴드하지 않은 생존자 전원의 카드가 보인다
	for _, pRaw := range specState["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		if p["alive"].(bool) && !p["folded"].(bool) && int(p["card"].(float64)) == 0 {
			t.Fatalf("관전자에게 카드가 가려졌다: %v", p)
		}
	}

	// 관전자는 어떤 행동도 못 한다
	spec.send(t, IPMessage{Type: IPMsgCall})
	errPayload := ipPayloadMap(t, spec.waitFor(t, IPMsgError))
	if errPayload["message"] != spectatorDeniedMsg {
		t.Fatalf("관전자 차단 문구 = %v", errPayload["message"])
	}
}
