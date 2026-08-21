package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 테스트에서는 AFK 자동 진행·라운드 전환 대기를 짧게 낮춘다
func init() {
	vgAfkTimeout = 120 * time.Millisecond
	vgRoundEndDelay = 40 * time.Millisecond
}

// vgTestClient 공용 testConn 에 라스베가스 메시지 타입의 waitFor 를 얹은 래퍼
type vgTestClient struct {
	testConn[VGMessage]
}

func newVGTestServer(t *testing.T, grace time.Duration) (*VGHub, string, func()) {
	t.Helper()
	hub := NewVGHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeVGWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func vgDial(t *testing.T, url string) *vgTestClient {
	t.Helper()
	return &vgTestClient{dialWS[VGMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *vgTestClient) waitFor(t *testing.T, msgType VGMessageType) VGMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m VGMessage) bool { return m.Type == msgType })
}

func vgPayloadMap(t *testing.T, msg VGMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// vgJoin 입장하고 vg_player_joined payload 를 돌려준다
func vgJoin(t *testing.T, c *vgTestClient, name, room string) map[string]interface{} {
	t.Helper()
	c.send(t, VGMessage{Type: VGMsgJoinGame, Payload: VGJoinGamePayload{Name: name, Room: room}})
	return vgPayloadMap(t, c.waitFor(t, VGMsgPlayerJoined))
}

// vgWaitPhase 지정한 phase 의 스냅샷이 올 때까지 소비
func (c *vgTestClient) vgWaitPhase(t *testing.T, phase string) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "vg_game_state("+phase+")", func(m VGMessage) bool {
		if m.Type != VGMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		return ok && state["phase"] == phase
	})
	return vgPayloadMap(t, msg)
}

// vgCheckStateShape 스냅샷 형태 회귀 장치 — dice 는 항상 배열, casinos 는
// 항상 6곳(bills 배열·placed 맵), players 의 cash/diceLeft 는 0이어도
// 실린다 (int omitempty 회귀 감지), roundResult 키는 항상 존재한다.
func vgCheckStateShape(t *testing.T, state map[string]interface{}, wantPlayers int) {
	t.Helper()
	if _, ok := state["dice"].([]interface{}); !ok {
		t.Fatalf("dice 가 배열이 아니다 (nil?): %v", state["dice"])
	}
	casinos, ok := state["casinos"].([]interface{})
	if !ok || len(casinos) != VGCasinoCount {
		t.Fatalf("casinos = %v, want %d곳", state["casinos"], VGCasinoCount)
	}
	for i, cRaw := range casinos {
		c := cRaw.(map[string]interface{})
		if int(c["face"].(float64)) != i+1 {
			t.Fatalf("casinos[%d].face = %v, want %d", i, c["face"], i+1)
		}
		if _, ok := c["bills"].([]interface{}); !ok {
			t.Fatalf("casinos[%d].bills 가 배열이 아니다: %v", i, c["bills"])
		}
		if _, ok := c["placed"].(map[string]interface{}); !ok {
			t.Fatalf("casinos[%d].placed 가 맵이 아니다: %v", i, c["placed"])
		}
	}
	players, ok := state["players"].([]interface{})
	if !ok || len(players) != wantPlayers {
		t.Fatalf("players = %v, want %d명", state["players"], wantPlayers)
	}
	for i, pRaw := range players {
		p := pRaw.(map[string]interface{})
		if int(p["seat"].(float64)) != i { // 좌석 0 유실 감지
			t.Fatalf("players[%d].seat = %v", i, p["seat"])
		}
		for _, key := range []string{"cash", "diceLeft", "connected", "bot", "name"} {
			if _, has := p[key]; !has { // int omitempty 회귀 감지
				t.Fatalf("players[%d].%s 누락: %v", i, key, p)
			}
		}
	}
	if _, has := state["roundResult"]; !has {
		t.Fatalf("roundResult 키 누락: %v", state)
	}
}

// TestVGFourBotsCompleteGame 봇을 채운 4인 게임이 20초 안에 완주하는지 —
// 가장 중요한 회귀 장치 (교착·라운드 미전환·차례 미이동·정산 미발생 감지).
// 좌석 0은 서버 연습봇 두뇌(vgBrain)를 WS 로 감싼 드라이버가 잡는다.
func TestVGFourBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newVGTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := vgDial(t, url)
	defer c.conn.Close()
	vgJoin(t, c, "감독", "")
	c.send(t, VGMessage{Type: VGMsgFillBots})

	start := time.Now()
	brain := newVGBrain()
	deadline := start.Add(20 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "state-or-over", func(m VGMessage) bool {
			return m.Type == VGMsgGameState || m.Type == VGMsgGameOver
		})
		if msg.Type == VGMsgGameOver {
			over := vgPayloadMap(t, msg)
			winners := over["winnerSeats"].([]interface{})
			if len(winners) < 1 {
				t.Fatalf("winnerSeats = %v", winners)
			}
			totals := over["totals"].([]interface{})
			if len(totals) != VGFillBotTarget {
				t.Fatalf("totals 길이 = %d, want %d", len(totals), VGFillBotTarget)
			}
			best := 0
			for _, raw := range totals {
				if v := int(raw.(float64)); v > best {
					best = v
				}
			}
			for _, w := range winners {
				if int(totals[int(w.(float64))].(float64)) != best {
					t.Fatalf("승자 총액 불일치: winners=%v totals=%v", winners, totals)
				}
			}
			t.Logf("완주: winners=%v totals=%v (%.1fs)", winners, totals, time.Since(start).Seconds())
			return
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("20초 안에 게임이 끝나지 않았다 — 진행 불가 상태")
}

// TestVGAfkAutoAdvance 접속만 유지한 채 아무것도 하지 않는 사람의 차례를
// AFK 타이머가 최다 눈 자동 배치로 진행하는지 (endsAt 노출 포함)
func TestVGAfkAutoAdvance(t *testing.T) {
	_, url, cleanup := newVGTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := vgDial(t, url)
	defer host.conn.Close()
	vgJoin(t, host, "잠수1", "")

	guest := vgDial(t, url)
	defer guest.conn.Close()
	vgJoin(t, guest, "잠수2", "")

	host.send(t, VGMessage{Type: VGMsgStart})
	state := guest.vgWaitPhase(t, string(VGPhasePlacing))
	if ends := int64(state["endsAt"].(float64)); ends <= 0 {
		t.Fatalf("placing 스냅샷의 endsAt = %d, want unixMillis", ends)
	}
	vgCheckStateShape(t, state, 2)
	if round := int(state["round"].(float64)); round != 1 {
		t.Fatalf("시작 round = %d, want 1", round)
	}
	if dice := state["dice"].([]interface{}); len(dice) != VGDiceCount {
		t.Fatalf("선공 굴림 주사위 수 = %d, want %d", len(dice), VGDiceCount)
	}

	// 전원 무행동 → AFK 이벤트(한글 문구)와 자동 배치 확인
	afk := vgPayloadMap(t, guest.waitMatch(t, "afk-event", func(m VGMessage) bool {
		if m.Type != VGMsgEvent {
			return false
		}
		ev, ok := m.Payload.(map[string]interface{})
		return ok && ev["kind"] == "afk"
	}))
	if !strings.Contains(afk["message"].(string), "자동으로 배치") {
		t.Fatalf("AFK 문구 = %v", afk["message"])
	}
	if _, has := afk["name"]; !has {
		t.Fatalf("AFK 이벤트에 name 부재: %v", afk)
	}

	// 자동 배치가 반영된 스냅샷 — 누군가의 주사위가 줄었다
	guest.waitMatch(t, "auto-placed state", func(m VGMessage) bool {
		if m.Type != VGMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		if !ok {
			return false
		}
		placedTotal := 0
		for _, pRaw := range state["players"].([]interface{}) {
			p := pRaw.(map[string]interface{})
			placedTotal += VGDiceCount - int(p["diceLeft"].(float64))
		}
		return placedTotal >= 1
	})
}

// TestVGRoomCodeAndSpectate 방 코드 발급·코드 입장·시작 후 코드 관전 진입.
// 관전자 스냅샷은 yourSeat -1 이며 주사위·지폐·배치는 전원 공개라 그대로 보인다.
func TestVGRoomCodeAndSpectate(t *testing.T) {
	_, url, cleanup := newVGTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := vgDial(t, url)
	defer host.conn.Close()
	joined := vgJoin(t, host, "호스트", "NEW")
	code, _ := joined["roomCode"].(string)
	if len(code) != roomCodeLen {
		t.Fatalf("roomCode = %q, want %d자", code, roomCodeLen)
	}

	guest := vgDial(t, url)
	defer guest.conn.Close()
	guestJoined := vgJoin(t, guest, "친구", code)
	if guestJoined["roomCode"] != code || int(guestJoined["yourSeat"].(float64)) != 1 {
		t.Fatalf("코드 입장 실패: %v", guestJoined)
	}

	// 호스트가 봇을 채우면 4인이 되며 즉시 시작
	host.send(t, VGMessage{Type: VGMsgFillBots})
	state := host.vgWaitPhase(t, string(VGPhasePlacing))
	if state["roomCode"] != code {
		t.Fatalf("시작 스냅샷 roomCode = %v", state["roomCode"])
	}
	vgCheckStateShape(t, state, VGFillBotTarget)

	// 시작된 방의 코드로 들어오면 관전자
	spec := vgDial(t, url)
	defer spec.conn.Close()
	spec.send(t, VGMessage{Type: VGMsgJoinGame, Payload: VGJoinGamePayload{Name: "구경꾼", Room: code}})
	specJoined := vgPayloadMap(t, spec.waitFor(t, VGMsgSpectateJoined))
	if specJoined["roomCode"] != code {
		t.Fatalf("관전 입장 roomCode = %v", specJoined["roomCode"])
	}

	specState := vgPayloadMap(t, spec.waitFor(t, VGMsgGameState))
	if int(specState["yourSeat"].(float64)) != -1 {
		t.Fatalf("관전자 yourSeat = %v, want -1", specState["yourSeat"])
	}
	vgCheckStateShape(t, specState, VGFillBotTarget)

	// 관전자는 어떤 행동도 못 한다
	spec.send(t, VGMessage{Type: VGMsgPlace, Payload: VGPlacePayload{Face: 1}})
	errPayload := vgPayloadMap(t, spec.waitFor(t, VGMsgError))
	if errPayload["message"] != spectatorDeniedMsg {
		t.Fatalf("관전자 차단 문구 = %v", errPayload["message"])
	}
}
