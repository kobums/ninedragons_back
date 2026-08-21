package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 테스트에서는 대기 상태 마감을 짧게 낮춘다 (자동 선언·의심 창 자동 통과)
func init() {
	ccRollTimeout = 150 * time.Millisecond
	ccDoubtTimeout = 120 * time.Millisecond
}

// ccTestClient 공용 testConn 에 차오차오 메시지 타입의 waitFor 를 얹은 래퍼
type ccTestClient struct {
	testConn[CCMessage]
}

func newCCTestServer(t *testing.T, grace time.Duration) (*CCHub, string, func()) {
	t.Helper()
	hub := NewCCHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeCCWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func ccDial(t *testing.T, url string) *ccTestClient {
	t.Helper()
	return &ccTestClient{dialWS[CCMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *ccTestClient) waitFor(t *testing.T, msgType CCMessageType) CCMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m CCMessage) bool { return m.Type == msgType })
}

func ccPayloadMap(t *testing.T, msg CCMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// ccJoin 입장하고 cc_player_joined payload 를 돌려준다
func ccJoin(t *testing.T, c *ccTestClient, name, room string) map[string]interface{} {
	t.Helper()
	c.send(t, CCMessage{Type: CCMsgJoinGame, Payload: CCJoinGamePayload{Name: name, Room: room}})
	return ccPayloadMap(t, c.waitFor(t, CCMsgPlayerJoined))
}

// ccWaitPhase 지정한 phase 의 스냅샷이 올 때까지 소비
func (c *ccTestClient) ccWaitPhase(t *testing.T, phase string) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "cc_game_state("+phase+")", func(m CCMessage) bool {
		if m.Type != CCMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		return ok && state["phase"] == phase
	})
	return ccPayloadMap(t, msg)
}

// TestCCFourBotsCompleteGame 봇을 채운 4인 게임이 30초 안에 완주하는지 —
// 가장 중요한 회귀 장치 (의심 창 교착·턴 미전환·종료 판정 감지).
// 좌석 0은 서버 연습봇 두뇌(ccBrain)를 WS 로 감싼 드라이버가 잡는다.
func TestCCFourBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newCCTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := ccDial(t, url)
	defer c.conn.Close()
	ccJoin(t, c, "감독", "")
	c.send(t, CCMessage{Type: CCMsgFillBots}) // 4인까지 채우고 즉시 시작

	start := time.Now()
	brain := newCCBrain()
	deadline := start.Add(30 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "state-or-over", func(m CCMessage) bool {
			return m.Type == CCMsgGameState || m.Type == CCMsgGameOver
		})
		if msg.Type == CCMsgGameOver {
			over := ccPayloadMap(t, msg)
			winner := int(over["winnerSeat"].(float64))
			if winner < 0 || winner >= CCFillBotTarget {
				t.Fatalf("winnerSeat = %d", winner)
			}
			if name, _ := over["winnerName"].(string); name == "" {
				t.Fatalf("winnerName 부재: %v", over)
			}
			reason, _ := over["reason"].(string)
			if reason != "two_crossed" && reason != "most_crossed" {
				t.Fatalf("reason = %q", reason)
			}
			players := over["players"].([]interface{})
			if len(players) != CCFillBotTarget {
				t.Fatalf("players 길이 = %d, want %d", len(players), CCFillBotTarget)
			}
			if reason == "two_crossed" {
				for _, pRaw := range players {
					p := pRaw.(map[string]interface{})
					if int(p["seat"].(float64)) == winner &&
						int(p["crossed"].(float64)) != CCWinCrossed {
						t.Fatalf("승자 통과 수 = %v, want %d", p["crossed"], CCWinCrossed)
					}
				}
			}
			t.Logf("완주: winner=seat%d %s (%.1fs)", winner, reason, time.Since(start).Seconds())
			return
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("30초 안에 게임이 끝나지 않았다 — 진행 불가 상태")
}

// TestCCHiddenState 은닉의 핵심 계약 — 허브 고루틴 없이 핸들러를 직접 불러
// 결정적으로 검증한다. yourRoll 은 차례 본인의 rolling 스냅샷에만 실리고,
// 타인·관전자의 raw JSON 에는 필드 자체가 없어야 한다. 실제 값은 의심 판정
// 뒤 lastReveal.actual 로만 공개되며, 통과된 선언의 주사위는 -1(비밀)이다.
func TestCCHiddenState(t *testing.T) {
	h := NewCCHub()
	room := h.lobbyRoomFor("")
	clients := make([]*CCClient, 3)
	for i := range clients {
		c := &CCClient{wsClient: newBotWSClient(), Hub: h}
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
	game.CurrentSeat = 0
	game.Roll = 3 // 결정적 주사위

	rawOf := func(viewer int) string {
		data, err := json.Marshal(h.buildCCState(room, viewer))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(data)
	}

	// ---- rolling: yourRoll 은 차례 본인에게만 (타인·관전자는 필드 부재) ----
	raw0 := rawOf(0)
	if !strings.Contains(raw0, `"yourRoll":3`) {
		t.Fatalf("본인 주사위 부재:\n%s", raw0)
	}
	for _, viewer := range []int{1, 2, -1} {
		if raw := rawOf(viewer); strings.Contains(raw, "yourRoll") {
			t.Fatalf("viewer %d 스냅샷에 yourRoll 유출:\n%s", viewer, raw)
		}
	}
	rawSpec := rawOf(-1)
	if !strings.Contains(rawSpec, `"onBridge":[]`) {
		t.Fatalf("빈 슬라이스가 []가 아니다:\n%s", rawSpec)
	}
	specState := h.buildCCState(room, -1)
	if specState.YourSeat != -1 || specState.BridgeLen != CCBridgeLen {
		t.Fatalf("관전자 스냅샷: yourSeat=%d bridgeLen=%d", specState.YourSeat, specState.BridgeLen)
	}

	// ---- 선언(실제 3, 선언 4 — 거짓): 의심 창에서도 주사위는 계속 비밀 ----
	h.handleGameMessage(CCGameMessage{Client: clients[0], Message: CCMessage{
		Type: CCMsgDeclare, Payload: CCDeclarePayload{Value: 4}}})
	if game.Phase != CCPhaseDoubtWindow || game.Declared != 4 {
		t.Fatalf("phase=%s declared=%d, want doubt_window 4", game.Phase, game.Declared)
	}
	if h.buildCCState(room, 1).EndsAt <= 0 {
		t.Fatal("의심 창 스냅샷의 endsAt 부재")
	}
	for _, viewer := range []int{0, 1, -1} { // rolling 이 아니므로 본인에게도 없다
		if raw := rawOf(viewer); strings.Contains(raw, "yourRoll") {
			t.Fatalf("의심 창 viewer %d 스냅샷에 yourRoll 유출:\n%s", viewer, raw)
		}
	}
	if !strings.Contains(rawOf(1), `"declared":4`) {
		t.Fatal("선언 값은 공개 정보다")
	}

	// ---- 의심 판정: 실제 값이 그제서야 lastReveal 로 공개된다 ----
	h.handleGameMessage(CCGameMessage{Client: clients[1], Message: CCMessage{Type: CCMsgAllow}})
	if game.Phase != CCPhaseDoubtWindow {
		t.Fatalf("한 명 허용만으로 창이 닫혔다: %s", game.Phase)
	}
	h.handleGameMessage(CCGameMessage{Client: clients[2], Message: CCMessage{Type: CCMsgDoubt}})
	if game.Reveal == nil || game.Reveal.Result != "doubt_success" || game.Reveal.Actual != 3 {
		t.Fatalf("의심 판정 공개 실패: %+v", game.Reveal)
	}
	if game.Players[0].Reserve != CCPawns-1 {
		t.Fatalf("선언자 말 낙하 실패: reserve=%d", game.Players[0].Reserve)
	}
	if !strings.Contains(rawOf(1), `"actual":3`) {
		t.Fatal("판정 후 실제 값 미공개")
	}
	if game.Phase != CCPhaseRolling || game.CurrentSeat != 1 {
		t.Fatalf("턴 전환 실패: phase=%s current=%d", game.Phase, game.CurrentSeat)
	}

	// ---- 통과된 선언(실제 X): 주사위는 끝까지 비밀 (actual -1) ----
	game.Roll = CCRollX
	h.handleGameMessage(CCGameMessage{Client: clients[1], Message: CCMessage{
		Type: CCMsgDeclare, Payload: CCDeclarePayload{Value: 2}}})
	h.handleGameMessage(CCGameMessage{Client: clients[0], Message: CCMessage{Type: CCMsgAllow}})
	h.handleGameMessage(CCGameMessage{Client: clients[2], Message: CCMessage{Type: CCMsgAllow}})
	if game.Reveal == nil || game.Reveal.Result != "pass" || game.Reveal.Actual != -1 {
		t.Fatalf("통과인데 주사위가 공개됐다: %+v", game.Reveal)
	}
	if fmt.Sprint(game.Players[1].Bridge) != "[2]" {
		t.Fatalf("통과 전진 실패: bridge=%v", game.Players[1].Bridge)
	}
	if !strings.Contains(rawOf(2), `"actual":-1`) {
		t.Fatal("통과 lastReveal 의 actual 이 -1 이 아니다")
	}
}

// TestCCAfkAutoProgress 접속만 유지한 채 아무것도 하지 않는 2인전 —
// 자동 선언(주사위 값 그대로, X 면 무작위 1~4)과 의심 창 마감 통과만으로
// 게임이 끝까지 완주하는지 (endsAt 노출·afk 이벤트 포함).
func TestCCAfkAutoProgress(t *testing.T) {
	_, url, cleanup := newCCTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := ccDial(t, url)
	defer host.conn.Close()
	ccJoin(t, host, "잠수1", "")

	guest := ccDial(t, url)
	defer guest.conn.Close()
	ccJoin(t, guest, "잠수2", "")

	host.send(t, CCMessage{Type: CCMsgStart})
	state := host.ccWaitPhase(t, string(CCPhaseRolling))
	if ends := int64(state["endsAt"].(float64)); ends <= 0 {
		t.Fatalf("rolling 스냅샷의 endsAt = %d, want unixMillis", ends)
	}

	// guest 는 더 읽지 않는다 — 백그라운드로 비워 버퍼 포화만 막는다
	go func() {
		for {
			if _, _, err := guest.conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	// 전원 방치 — 자동 선언·창 마감 통과로 완주해야 한다
	sawAfk := false
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		msg := host.waitMatch(t, "event-or-over", func(m CCMessage) bool {
			return m.Type == CCMsgEvent || m.Type == CCMsgGameOver
		})
		if msg.Type == CCMsgEvent {
			ev := ccPayloadMap(t, msg)
			if ev["kind"] == "afk" {
				if !strings.Contains(ev["message"].(string), "자동") {
					t.Fatalf("afk 문구 = %v", ev["message"])
				}
				sawAfk = true
			}
			continue
		}
		over := ccPayloadMap(t, msg)
		if int(over["winnerSeat"].(float64)) < 0 {
			t.Fatalf("winnerSeat = %v", over["winnerSeat"])
		}
		if !sawAfk {
			t.Fatal("afk 자동 진행 이벤트가 한 번도 없었다")
		}
		return
	}
	t.Fatal("전원 방치 게임이 45초 안에 끝나지 않았다")
}

// TestCCRoomCodeAndSpectate 방 코드 발급·코드 입장·시작 후 코드 관전 진입.
// 관전자 스냅샷은 yourSeat -1 에 yourRoll 부재. 행동은 전부 차단된다.
func TestCCRoomCodeAndSpectate(t *testing.T) {
	_, url, cleanup := newCCTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := ccDial(t, url)
	defer host.conn.Close()
	joined := ccJoin(t, host, "호스트", "NEW")
	code, _ := joined["roomCode"].(string)
	if len(code) != roomCodeLen {
		t.Fatalf("roomCode = %q, want %d자", code, roomCodeLen)
	}

	guest := ccDial(t, url)
	defer guest.conn.Close()
	guestJoined := ccJoin(t, guest, "친구", code)
	if guestJoined["roomCode"] != code || int(guestJoined["yourSeat"].(float64)) != 1 {
		t.Fatalf("코드 입장 실패: %v", guestJoined)
	}

	host.send(t, CCMessage{Type: CCMsgStart})
	state := host.ccWaitPhase(t, string(CCPhaseRolling))
	if state["roomCode"] != code || len(state["players"].([]interface{})) != 2 {
		t.Fatalf("시작 실패: %v", state["players"])
	}
	if int(state["bridgeLen"].(float64)) != CCBridgeLen {
		t.Fatalf("bridgeLen = %v", state["bridgeLen"])
	}
	// 차례 본인이면 yourRoll 이 있고, 아니면 필드 자체가 없다
	cur := int(state["currentSeat"].(float64))
	roll, hasRoll := state["yourRoll"]
	if cur == 0 && !hasRoll {
		t.Fatal("차례 본인 스냅샷에 yourRoll 부재")
	}
	if cur != 0 && hasRoll {
		t.Fatalf("차례 아닌 좌석에 yourRoll 유출: %v", roll)
	}

	// 시작된 방의 코드로 들어오면 관전자
	spec := ccDial(t, url)
	defer spec.conn.Close()
	spec.send(t, CCMessage{Type: CCMsgJoinGame, Payload: CCJoinGamePayload{Name: "구경꾼", Room: code}})
	specJoined := ccPayloadMap(t, spec.waitFor(t, CCMsgSpectateJoined))
	if specJoined["roomCode"] != code {
		t.Fatalf("관전 입장 roomCode = %v", specJoined["roomCode"])
	}

	specState := ccPayloadMap(t, spec.waitFor(t, CCMsgGameState))
	if int(specState["yourSeat"].(float64)) != -1 {
		t.Fatalf("관전자 yourSeat = %v, want -1", specState["yourSeat"])
	}
	if leaked, ok := specState["yourRoll"]; ok {
		t.Fatalf("관전자에게 주사위 유출: %v", leaked)
	}
	for _, pRaw := range specState["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		total := int(p["pawnsLeft"].(float64)) + len(p["onBridge"].([]interface{})) +
			int(p["crossed"].(float64))
		if total != CCPawns {
			t.Fatalf("관전자 스냅샷 좌석 정보 이상: %v", p)
		}
	}

	// 관전자는 어떤 행동도 못 한다
	spec.send(t, CCMessage{Type: CCMsgAllow})
	errPayload := ccPayloadMap(t, spec.waitFor(t, CCMsgError))
	if errPayload["message"] != spectatorDeniedMsg {
		t.Fatalf("관전자 차단 문구 = %v", errPayload["message"])
	}
}
