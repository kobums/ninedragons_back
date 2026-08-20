package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 테스트에서는 AFK 자동 진행 대기를 짧게 낮춘다
func init() {
	ytAfkTimeout = 120 * time.Millisecond
}

// ytTestClient 공용 testConn 에 요트 메시지 타입의 waitFor 를 얹은 래퍼
type ytTestClient struct {
	testConn[YTMessage]
}

func newYTTestServer(t *testing.T, grace time.Duration) (*YTHub, string, func()) {
	t.Helper()
	hub := NewYTHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeYTWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func ytDial(t *testing.T, url string) *ytTestClient {
	t.Helper()
	return &ytTestClient{dialWS[YTMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *ytTestClient) waitFor(t *testing.T, msgType YTMessageType) YTMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m YTMessage) bool { return m.Type == msgType })
}

func ytPayloadMap(t *testing.T, msg YTMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// ytJoin 입장하고 yt_player_joined payload 를 돌려준다
func ytJoin(t *testing.T, c *ytTestClient, name, room string) map[string]interface{} {
	t.Helper()
	c.send(t, YTMessage{Type: YTMsgJoinGame, Payload: YTJoinGamePayload{Name: name, Room: room}})
	return ytPayloadMap(t, c.waitFor(t, YTMsgPlayerJoined))
}

// ytWaitPhase 지정한 phase 의 스냅샷이 올 때까지 소비
func (c *ytTestClient) ytWaitPhase(t *testing.T, phase string) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "yt_game_state("+phase+")", func(m YTMessage) bool {
		if m.Type != YTMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		return ok && state["phase"] == phase
	})
	return ytPayloadMap(t, msg)
}

// ytCheckStateShape 스냅샷 형태 회귀 장치 — dice/held 는 항상 5개,
// players 의 scores 는 13칸 전부 실린다 (nil/누락 금지)
func ytCheckStateShape(t *testing.T, state map[string]interface{}, wantPlayers int) {
	t.Helper()
	if dice, ok := state["dice"].([]interface{}); !ok || len(dice) != YTDiceCount {
		t.Fatalf("dice = %v, want 길이 %d 배열", state["dice"], YTDiceCount)
	}
	if held, ok := state["held"].([]interface{}); !ok || len(held) != YTDiceCount {
		t.Fatalf("held = %v, want 길이 %d 배열", state["held"], YTDiceCount)
	}
	if _, ok := state["winnerSeats"].([]interface{}); !ok {
		t.Fatalf("winnerSeats 가 배열이 아니다 (nil?): %v", state["winnerSeats"])
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
		scores, ok := p["scores"].(map[string]interface{})
		if !ok || len(scores) != len(ytCategories) {
			t.Fatalf("players[%d].scores = %v, want %d칸", i, p["scores"], len(ytCategories))
		}
		for _, cat := range ytCategories {
			if _, has := scores[cat]; !has {
				t.Fatalf("players[%d].scores 에 %s 칸 누락", i, cat)
			}
		}
		for _, key := range []string{"total", "upperSum", "bonus"} {
			if _, has := p[key]; !has { // int omitempty 회귀 감지
				t.Fatalf("players[%d].%s 누락: %v", i, key, p)
			}
		}
	}
}

// TestYTFourBotsCompleteGame 봇을 채운 4인 게임이 20초 안에 완주하는지 —
// 가장 중요한 회귀 장치 (교착·라운드 미전환·턴 미이동 감지).
// 좌석 0은 서버 연습봇 두뇌(ytBrain)를 WS 로 감싼 드라이버가 잡는다.
func TestYTFourBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newYTTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := ytDial(t, url)
	defer c.conn.Close()
	ytJoin(t, c, "감독", "")
	c.send(t, YTMessage{Type: YTMsgFillBots})

	start := time.Now()
	brain := newYTBrain()
	deadline := start.Add(20 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "state-or-over", func(m YTMessage) bool {
			return m.Type == YTMsgGameState || m.Type == YTMsgGameOver
		})
		if msg.Type == YTMsgGameOver {
			over := ytPayloadMap(t, msg)
			winners := over["winnerSeats"].([]interface{})
			if len(winners) < 1 {
				t.Fatalf("winnerSeats = %v", winners)
			}
			totals := over["totals"].([]interface{})
			if len(totals) != YTMaxPlayers {
				t.Fatalf("totals 길이 = %d, want %d", len(totals), YTMaxPlayers)
			}
			best := 0
			for _, raw := range totals {
				if v := int(raw.(float64)); v > best {
					best = v
				}
			}
			for _, w := range winners {
				if int(totals[int(w.(float64))].(float64)) != best {
					t.Fatalf("승자 총점 불일치: winners=%v totals=%v", winners, totals)
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

// TestYTAfkAutoAdvance 접속만 유지한 채 아무것도 하지 않는 사람의 턴을
// AFK 타이머가 굴림·기록으로 자동 진행하는지 (endsAt 노출 포함)
func TestYTAfkAutoAdvance(t *testing.T) {
	_, url, cleanup := newYTTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := ytDial(t, url)
	defer host.conn.Close()
	ytJoin(t, host, "잠수1", "")

	guest := ytDial(t, url)
	defer guest.conn.Close()
	ytJoin(t, guest, "잠수2", "")

	host.send(t, YTMessage{Type: YTMsgStart})
	state := guest.ytWaitPhase(t, string(YTPhasePlaying))
	if ends := int64(state["endsAt"].(float64)); ends <= 0 {
		t.Fatalf("playing 스냅샷의 endsAt = %d, want unixMillis", ends)
	}
	ytCheckStateShape(t, state, 2)
	if round := int(state["round"].(float64)); round != 1 {
		t.Fatalf("시작 round = %d, want 1", round)
	}

	// 전원 무행동 → AFK 이벤트(한글 문구)와 자동 굴림·기록 확인
	afk := ytPayloadMap(t, guest.waitMatch(t, "afk-event", func(m YTMessage) bool {
		if m.Type != YTMsgEvent {
			return false
		}
		ev, ok := m.Payload.(map[string]interface{})
		return ok && ev["kind"] == "afk"
	}))
	if !strings.Contains(afk["message"].(string), "자동으로 진행") {
		t.Fatalf("AFK 문구 = %v", afk["message"])
	}
	if _, has := afk["name"]; !has {
		t.Fatalf("AFK 이벤트에 name 부재: %v", afk)
	}

	// 자동 진행 결과가 반영된 스냅샷 — lastAction 이 채워지고 점수가 기록됐다
	guest.waitMatch(t, "auto-scored state", func(m YTMessage) bool {
		if m.Type != YTMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		if !ok {
			return false
		}
		last, ok := state["lastAction"].(map[string]interface{})
		if !ok {
			return false
		}
		recorded := 0
		for _, pRaw := range state["players"].([]interface{}) {
			p := pRaw.(map[string]interface{})
			for _, v := range p["scores"].(map[string]interface{}) {
				if v.(float64) >= 0 {
					recorded++
				}
			}
		}
		return recorded >= 1 && last["category"] != ""
	})
}

// TestYTRoomCodeAndSpectate 방 코드 발급·코드 입장·시작 후 코드 관전 진입.
// 관전자 스냅샷은 yourSeat -1 이며 주사위·점수표는 전원 공개라 그대로 보인다.
func TestYTRoomCodeAndSpectate(t *testing.T) {
	_, url, cleanup := newYTTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := ytDial(t, url)
	defer host.conn.Close()
	joined := ytJoin(t, host, "호스트", "NEW")
	code, _ := joined["roomCode"].(string)
	if len(code) != roomCodeLen {
		t.Fatalf("roomCode = %q, want %d자", code, roomCodeLen)
	}

	guest := ytDial(t, url)
	defer guest.conn.Close()
	guestJoined := ytJoin(t, guest, "친구", code)
	if guestJoined["roomCode"] != code || int(guestJoined["yourSeat"].(float64)) != 1 {
		t.Fatalf("코드 입장 실패: %v", guestJoined)
	}

	// 호스트가 봇을 채우면 4인이 되며 즉시 시작
	host.send(t, YTMessage{Type: YTMsgFillBots})
	state := host.ytWaitPhase(t, string(YTPhasePlaying))
	if state["roomCode"] != code {
		t.Fatalf("시작 스냅샷 roomCode = %v", state["roomCode"])
	}
	ytCheckStateShape(t, state, YTMaxPlayers)

	// 시작된 방의 코드로 들어오면 관전자
	spec := ytDial(t, url)
	defer spec.conn.Close()
	spec.send(t, YTMessage{Type: YTMsgJoinGame, Payload: YTJoinGamePayload{Name: "구경꾼", Room: code}})
	specJoined := ytPayloadMap(t, spec.waitFor(t, YTMsgSpectateJoined))
	if specJoined["roomCode"] != code {
		t.Fatalf("관전 입장 roomCode = %v", specJoined["roomCode"])
	}

	specState := ytPayloadMap(t, spec.waitFor(t, YTMsgGameState))
	if int(specState["yourSeat"].(float64)) != -1 {
		t.Fatalf("관전자 yourSeat = %v, want -1", specState["yourSeat"])
	}
	ytCheckStateShape(t, specState, YTMaxPlayers)

	// 관전자는 어떤 행동도 못 한다
	spec.send(t, YTMessage{Type: YTMsgRoll, Payload: YTRollPayload{Hold: make([]bool, YTDiceCount)}})
	errPayload := ytPayloadMap(t, spec.waitFor(t, YTMsgError))
	if errPayload["message"] != spectatorDeniedMsg {
		t.Fatalf("관전자 차단 문구 = %v", errPayload["message"])
	}
}
