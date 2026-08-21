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

// 테스트에서는 대기 상태 마감을 짧게 낮춘다 (AFK 자동 전달·자동 판정)
func init() {
	crTurnTimeout = 150 * time.Millisecond
}

// crTestClient 공용 testConn 에 바퀴벌레 메시지 타입의 waitFor 를 얹은 래퍼
type crTestClient struct {
	testConn[CRMessage]
}

func newCRTestServer(t *testing.T, grace time.Duration) (*CRHub, string, func()) {
	t.Helper()
	hub := NewCRHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeCRWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func crDial(t *testing.T, url string) *crTestClient {
	t.Helper()
	return &crTestClient{dialWS[CRMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *crTestClient) waitFor(t *testing.T, msgType CRMessageType) CRMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m CRMessage) bool { return m.Type == msgType })
}

func crPayloadMap(t *testing.T, msg CRMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// crJoin 입장하고 cr_player_joined payload 를 돌려준다
func crJoin(t *testing.T, c *crTestClient, name, room string) map[string]interface{} {
	t.Helper()
	c.send(t, CRMessage{Type: CRMsgJoinGame, Payload: CRJoinGamePayload{Name: name, Room: room}})
	return crPayloadMap(t, c.waitFor(t, CRMsgPlayerJoined))
}

// crWaitPhase 지정한 phase 의 스냅샷이 올 때까지 소비
func (c *crTestClient) crWaitPhase(t *testing.T, phase string) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "cr_game_state("+phase+")", func(m CRMessage) bool {
		if m.Type != CRMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		return ok && state["phase"] == phase
	})
	return crPayloadMap(t, msg)
}

// crDrainMessages 소켓 없는 가상 클라이언트의 Send 버퍼를 비워 메시지로
// 돌려준다 (cr_peek 수신 검증용 — 허브 고루틴 없는 결정적 테스트 전용)
func crDrainMessages(t *testing.T, c *CRClient) []CRMessage {
	t.Helper()
	msgs := []CRMessage{}
	for {
		select {
		case data := <-c.Send:
			var m CRMessage
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatalf("unmarshal: %v (%s)", err, data)
			}
			msgs = append(msgs, m)
		default:
			return msgs
		}
	}
}

// crPeeksOf 메시지 목록에서 cr_peek 의 animal 값만 추린다
func crPeeksOf(t *testing.T, msgs []CRMessage) []string {
	t.Helper()
	animals := []string{}
	for _, m := range msgs {
		if m.Type != CRMsgPeek {
			continue
		}
		p := asPayloadMap(t, m.Payload)
		animals = append(animals, p["animal"].(string))
	}
	return animals
}

// TestCRFourBotsCompleteGame 봇을 채운 4인 게임이 30초 안에 완주하는지 —
// 가장 중요한 회귀 장치 (릴레이 교착·판정 미전환·패배 조건 누락 감지).
// 좌석 0은 서버 연습봇 두뇌(crBrain)를 WS 로 감싼 드라이버가 잡는다.
func TestCRFourBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newCRTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := crDial(t, url)
	defer c.conn.Close()
	crJoin(t, c, "감독", "")
	c.send(t, CRMessage{Type: CRMsgFillBots}) // 4인까지 채우고 즉시 시작

	start := time.Now()
	brain := newCRBrain()
	deadline := start.Add(30 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "state-peek-or-over", func(m CRMessage) bool {
			return m.Type == CRMsgGameState || m.Type == CRMsgPeek || m.Type == CRMsgGameOver
		})
		if msg.Type == CRMsgGameOver {
			over := crPayloadMap(t, msg)
			loser := int(over["loserSeat"].(float64))
			if loser < 0 || loser >= CRFillBotTarget {
				t.Fatalf("loserSeat = %d", loser)
			}
			if name, _ := over["loserName"].(string); name == "" {
				t.Fatalf("loserName 부재: %v", over)
			}
			reason, _ := over["reason"].(string)
			if reason != CRLoseFourAnimals && reason != CRLoseEmptyHand {
				t.Fatalf("reason = %q", reason)
			}
			players := over["players"].([]interface{})
			if len(players) != CRFillBotTarget {
				t.Fatalf("players 길이 = %d, want %d", len(players), CRFillBotTarget)
			}
			// 패배 조건이 최종 스냅샷과 맞아떨어져야 한다
			for _, pRaw := range players {
				p := pRaw.(map[string]interface{})
				if int(p["seat"].(float64)) != loser {
					continue
				}
				switch reason {
				case CRLoseFourAnimals:
					maxCount := 0
					for _, n := range p["display"].(map[string]interface{}) {
						if int(n.(float64)) > maxCount {
							maxCount = int(n.(float64))
						}
					}
					if maxCount < CRLoseCount {
						t.Fatalf("four_animals 인데 진열 최다 %d장: %v", maxCount, p)
					}
				case CRLoseEmptyHand:
					if int(p["handCount"].(float64)) != 0 {
						t.Fatalf("empty_hand 인데 손패 %v장", p["handCount"])
					}
				}
			}
			t.Logf("완주: loser=seat%d(%s) (%.1fs)", loser, reason, time.Since(start).Seconds())
			return
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("30초 안에 게임이 끝나지 않았다 — 진행 불가 상태")
}

// TestCRAfkAutoProgress 접속만 유지한 채 아무것도 하지 않는 사람 좌석 —
// AFK 마감이 전달(무작위 카드·대상·실물 선언)과 판정(50/50)을 자동으로
// 해소해 게임이 끝까지 완주하는지 (endsAt 노출·afk 이벤트 포함).
func TestCRAfkAutoProgress(t *testing.T) {
	_, url, cleanup := newCRTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := crDial(t, url)
	defer host.conn.Close()
	crJoin(t, host, "잠수", "")
	host.send(t, CRMessage{Type: CRMsgFillBots}) // 사람 1 + 봇 3 즉시 시작

	state := host.crWaitPhase(t, string(CRPhasePassing))
	if ends := int64(state["endsAt"].(float64)); ends <= 0 {
		t.Fatalf("passing 스냅샷의 endsAt = %d, want unixMillis", ends)
	}

	// 이후 전원 방치 — 봇은 스스로 놀고 사람 차례는 AFK 자동 진행된다
	sawAfk := false
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		msg := host.waitMatch(t, "event-or-over", func(m CRMessage) bool {
			return m.Type == CRMsgEvent || m.Type == CRMsgGameOver
		})
		if msg.Type == CRMsgEvent {
			ev := crPayloadMap(t, msg)
			if ev["kind"] == "afk" {
				if !strings.Contains(ev["message"].(string), "자동") {
					t.Fatalf("afk 문구 = %v", ev["message"])
				}
				sawAfk = true
			}
			continue
		}
		over := crPayloadMap(t, msg)
		if int(over["loserSeat"].(float64)) < 0 {
			t.Fatalf("loserSeat = %v", over["loserSeat"])
		}
		if !sawAfk {
			t.Fatal("afk 자동 진행 이벤트가 한 번도 없었다")
		}
		return
	}
	t.Fatal("방치 게임이 45초 안에 끝나지 않았다")
}

// TestCRHiddenState 은닉의 핵심 계약 — 허브 고루틴 없이 핸들러를 직접 불러
// 결정적으로 검증한다. yourHand 는 본인 스냅샷에만 실리고, 릴레이 중인 카드
// 실물은 어느 스냅샷 raw JSON 에도 없으며(cr_peek 수신자만 본다), 넘기기가
// 불가능한 마지막 결정권자는 cr_peek 을 받지 못한다.
func TestCRHiddenState(t *testing.T) {
	h := NewCRHub()
	room := h.lobbyRoomFor("")
	clients := make([]*CRClient, 3)
	for i := range clients {
		c := &CRClient{wsClient: newBotWSClient(), Hub: h}
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
	game.PasserSeat = 0
	crSetHand(game, 0, CRRat, CRBat)
	crSetHand(game, 1, CRToad, CRToad)
	crSetHand(game, 2, CRSpider, CRFly)

	rawOf := func(viewer int) string {
		data, err := json.Marshal(h.buildCRState(room, viewer))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(data)
	}
	mustHide := func(viewer int, raw string, animals ...CRAnimal) {
		t.Helper()
		for _, a := range animals {
			if strings.Contains(raw, string(a)) {
				t.Fatalf("viewer %d 스냅샷에 %s 유출:\n%s", viewer, a, raw)
			}
		}
	}

	// ---- passing 단계: 손패는 본인만 ----
	raw0 := rawOf(0)
	if !strings.Contains(raw0, `"yourHand":["rat","bat"]`) {
		t.Fatalf("본인 손패 부재:\n%s", raw0)
	}
	mustHide(0, raw0, CRToad, CRSpider, CRFly, CRCockroach, CRScorpion, CRStinkbug)

	raw1 := rawOf(1)
	if !strings.Contains(raw1, `"yourHand":["toad","toad"]`) {
		t.Fatalf("본인 손패 부재:\n%s", raw1)
	}
	mustHide(1, raw1, CRRat, CRBat, CRSpider, CRFly)

	// 관전자(-1)에게는 yourHand 필드 자체가 없고 동물명이 하나도 없다
	rawSpec := rawOf(-1)
	if strings.Contains(rawSpec, "yourHand") {
		t.Fatalf("관전자 스냅샷에 yourHand 필드:\n%s", rawSpec)
	}
	mustHide(-1, rawSpec, crAllAnimals...)
	specState := h.buildCRState(room, -1)
	if specState.YourSeat != -1 || len(specState.Players) != 3 {
		t.Fatalf("관전자 스냅샷: yourSeat=%d players=%d", specState.YourSeat, len(specState.Players))
	}

	// ---- 전달: 실물 rat 은 스냅샷 어디에도 없고, 결정권자만 cr_peek 으로 본다 ----
	for _, c := range clients {
		crDrainMessages(t, c) // 이전 방송을 비운다
	}
	h.handleGameMessage(CRGameMessage{Client: clients[0], Message: CRMessage{
		Type: CRMsgPassCard, Payload: CRPassCardPayload{Card: "rat", TargetSeat: 1, Claim: "toad"}}})
	if game.Phase != CRPhaseDeciding || game.HolderSeat != 1 {
		t.Fatalf("전달 후 상태: phase=%s holder=%d", game.Phase, game.HolderSeat)
	}
	if h.buildCRState(room, 0).EndsAt <= 0 {
		t.Fatal("deciding 스냅샷의 endsAt 부재")
	}

	// 결정권자(seat1)만 실물 rat 을 받는다
	if peeks := crPeeksOf(t, crDrainMessages(t, clients[1])); len(peeks) != 1 || peeks[0] != "rat" {
		t.Fatalf("결정권자 cr_peek = %v, want [rat]", peeks)
	}
	if peeks := crPeeksOf(t, crDrainMessages(t, clients[0])); len(peeks) != 0 {
		t.Fatalf("전달자에게 cr_peek 유출: %v", peeks)
	}
	if peeks := crPeeksOf(t, crDrainMessages(t, clients[2])); len(peeks) != 0 {
		t.Fatalf("제3자에게 cr_peek 유출: %v", peeks)
	}

	// 실물 rat 은 어느 스냅샷에도 없다 (선언 toad·체인·장수만 공개)
	for _, viewer := range []int{0, 1, 2, -1} {
		raw := rawOf(viewer)
		if strings.Contains(raw, "rat") {
			t.Fatalf("viewer %d 스냅샷에 릴레이 실물 유출:\n%s", viewer, raw)
		}
	}
	state1 := h.buildCRState(room, 1)
	if state1.Claim != "toad" || len(state1.Chain) != 1 || state1.Chain[0] != 0 {
		t.Fatalf("공개 정보 이상: claim=%s chain=%v", state1.Claim, state1.Chain)
	}

	// ---- 넘기기: 마지막 결정권자는 cr_peek 없이 강제 판정 ----
	h.handleGameMessage(CRGameMessage{Client: clients[1], Message: CRMessage{
		Type: CRMsgRelay, Payload: CRRelayPayload{TargetSeat: 2, Claim: "bat"}}})
	if game.HolderSeat != 2 || len(game.Chain) != 2 {
		t.Fatalf("릴레이 후 상태: holder=%d chain=%v", game.HolderSeat, game.Chain)
	}
	// seat2 는 체인 밖 사람이 없어 넘기기 불가 — 실물을 볼 수 없다
	if peeks := crPeeksOf(t, crDrainMessages(t, clients[2])); len(peeks) != 0 {
		t.Fatalf("강제 판정 좌석에 cr_peek 유출: %v", peeks)
	}
	h.handleGameMessage(CRGameMessage{Client: clients[2], Message: CRMessage{
		Type: CRMsgRelay, Payload: CRRelayPayload{TargetSeat: 0, Claim: "fly"}}})
	if game.Phase != CRPhaseDeciding || game.HolderSeat != 2 {
		t.Fatal("마지막 결정권자의 넘기기가 허용됐다")
	}

	// ---- 판정: 실물이 진열로 공개된다 (은닉 해제의 유일한 경로) ----
	// 선언 bat vs 실물 rat — "거짓" 판정 적중 → 카드는 seat1(마지막 전달자) 진열로
	h.handleGameMessage(CRGameMessage{Client: clients[2], Message: CRMessage{
		Type: CRMsgJudge, Payload: CRJudgePayload{Truth: false}}})
	if game.Phase != CRPhasePassing || game.PasserSeat != 1 {
		t.Fatalf("판정 후 상태: phase=%s passer=%d", game.Phase, game.PasserSeat)
	}
	if game.Players[1].Display[CRRat] != 1 {
		t.Fatalf("진열 미반영: %v", game.Players[1].Display)
	}
	if !strings.Contains(rawOf(-1), `"rat":1`) {
		t.Fatalf("공개된 진열이 관전자 스냅샷에 없다:\n%s", rawOf(-1))
	}
}

// TestCRRoomCodeAndSpectate 방 코드 발급·코드 입장·시작 후 코드 관전 진입.
// 관전자 스냅샷은 yourSeat -1 에 yourHand 부재. 행동은 전부 차단된다.
func TestCRRoomCodeAndSpectate(t *testing.T) {
	_, url, cleanup := newCRTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := crDial(t, url)
	defer host.conn.Close()
	joined := crJoin(t, host, "호스트", "NEW")
	code, _ := joined["roomCode"].(string)
	if len(code) != roomCodeLen {
		t.Fatalf("roomCode = %q, want %d자", code, roomCodeLen)
	}

	guest1 := crDial(t, url)
	defer guest1.conn.Close()
	if g := crJoin(t, guest1, "친구1", code); g["roomCode"] != code || int(g["yourSeat"].(float64)) != 1 {
		t.Fatalf("코드 입장 실패: %v", g)
	}
	guest2 := crDial(t, url)
	defer guest2.conn.Close()
	if g := crJoin(t, guest2, "친구2", code); int(g["yourSeat"].(float64)) != 2 {
		t.Fatalf("코드 입장 실패: %v", g)
	}

	// 게스트는 더 읽지 않는다 — 백그라운드로 비워 버퍼 포화만 막는다
	for _, g := range []*crTestClient{guest1, guest2} {
		conn := g.conn
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
	}

	host.send(t, CRMessage{Type: CRMsgStart})
	state := host.crWaitPhase(t, string(CRPhasePassing))
	if state["roomCode"] != code || len(state["players"].([]interface{})) != 3 {
		t.Fatalf("시작 실패: %v", state["players"])
	}
	if hand := state["yourHand"].([]interface{}); len(hand) != 64/3 {
		t.Fatalf("호스트 손패 = %d장, want %d", len(hand), 64/3)
	}

	// 시작된 방의 코드로 들어오면 관전자
	spec := crDial(t, url)
	defer spec.conn.Close()
	spec.send(t, CRMessage{Type: CRMsgJoinGame, Payload: CRJoinGamePayload{Name: "구경꾼", Room: code}})
	specJoined := crPayloadMap(t, spec.waitFor(t, CRMsgSpectateJoined))
	if specJoined["roomCode"] != code {
		t.Fatalf("관전 입장 roomCode = %v", specJoined["roomCode"])
	}

	specState := crPayloadMap(t, spec.waitFor(t, CRMsgGameState))
	if int(specState["yourSeat"].(float64)) != -1 {
		t.Fatalf("관전자 yourSeat = %v, want -1", specState["yourSeat"])
	}
	if _, leaked := specState["yourHand"]; leaked {
		t.Fatalf("관전자에게 yourHand 유출: %v", specState["yourHand"])
	}
	if len(specState["players"].([]interface{})) != 3 {
		t.Fatalf("관전자 스냅샷 players = %v", specState["players"])
	}

	// 관전자는 어떤 행동도 못 한다
	spec.send(t, CRMessage{Type: CRMsgJudge, Payload: CRJudgePayload{Truth: true}})
	errPayload := crPayloadMap(t, spec.waitFor(t, CRMsgError))
	if errPayload["message"] != spectatorDeniedMsg {
		t.Fatalf("관전자 차단 문구 = %v", errPayload["message"])
	}
}
