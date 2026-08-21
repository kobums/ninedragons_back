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

// 테스트에서는 자동 진행 대기를 짧게 낮춘다
func init() {
	nmAfkTimeout = 150 * time.Millisecond
	nmRevealDelay = 30 * time.Millisecond
}

// nmTestClient 공용 testConn 에 6 님트 메시지 타입의 waitFor 를 얹은 래퍼
type nmTestClient struct {
	testConn[NMMessage]
}

func newNMTestServer(t *testing.T, grace time.Duration) (*NMHub, string, func()) {
	t.Helper()
	hub := NewNMHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeNMWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func nmDial(t *testing.T, url string) *nmTestClient {
	t.Helper()
	return &nmTestClient{dialWS[NMMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *nmTestClient) waitFor(t *testing.T, msgType NMMessageType) NMMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m NMMessage) bool { return m.Type == msgType })
}

func nmPayloadMap(t *testing.T, msg NMMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// nmJoin 입장하고 nm_player_joined payload 를 돌려준다
func nmJoin(t *testing.T, c *nmTestClient, name, room string) map[string]interface{} {
	t.Helper()
	c.send(t, NMMessage{Type: NMMsgJoinGame, Payload: NMJoinGamePayload{Name: name, Room: room}})
	return nmPayloadMap(t, c.waitFor(t, NMMsgPlayerJoined))
}

// nmWaitState 조건을 만족하는 nm_game_state 가 올 때까지 소비
func (c *nmTestClient) nmWaitState(t *testing.T, name string, cond func(map[string]interface{}) bool) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "nm_game_state("+name+")", func(m NMMessage) bool {
		if m.Type != NMMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		return ok && cond(state)
	})
	return nmPayloadMap(t, msg)
}

// TestNMSixBotsCompleteGame 봇을 채운 6인 게임이 20초 안에 완주하는지 —
// 가장 중요한 회귀 장치 (교착·트릭 미전환·행 선택 대기 미해소 감지).
// 좌석 0은 서버 연습봇 두뇌(nmBrain)를 WS 로 감싼 드라이버가 잡는다.
func TestNMSixBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newNMTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := nmDial(t, url)
	defer c.conn.Close()
	nmJoin(t, c, "감독", "")
	c.send(t, NMMessage{Type: NMMsgFillBots}) // 6인까지 채우고 즉시 시작

	start := time.Now()
	brain := newNMBrain()
	deadline := start.Add(20 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "state-or-over", func(m NMMessage) bool {
			return m.Type == NMMsgGameState || m.Type == NMMsgGameOver
		})
		if msg.Type == NMMsgGameOver {
			over := nmPayloadMap(t, msg)
			winners := over["winnerSeats"].([]interface{})
			if len(winners) < 1 {
				t.Fatalf("winnerSeats = %v", winners)
			}
			penalties := over["penalties"].([]interface{})
			if len(penalties) != NMFillBotTarget {
				t.Fatalf("penalties 길이 = %d, want %d", len(penalties), NMFillBotTarget)
			}
			// 승자는 소머리 최소여야 한다
			best := 1 << 30
			for _, pn := range penalties {
				if int(pn.(float64)) < best {
					best = int(pn.(float64))
				}
			}
			for _, wRaw := range winners {
				w := int(wRaw.(float64))
				if int(penalties[w].(float64)) != best {
					t.Fatalf("승자 seat%d 소머리 %v ≠ 최소 %d", w, penalties[w], best)
				}
			}
			if _, ok := over["winnerNames"].([]interface{}); !ok {
				t.Fatalf("winnerNames 부재: %v", over)
			}
			t.Logf("완주: winners=%v penalties=%v (%.1fs)", winners, penalties, time.Since(start).Seconds())
			return
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("20초 안에 게임이 끝나지 않았다 — 진행 불가 상태")
}

// TestNMHiddenState 은닉의 핵심 계약 — 허브 고루틴 없이 핸들러를 직접 불러
// 결정적으로 검증한다.
//
//	picking: yourHand 본인만 실값(타인·관전자 빈 배열), picks 필드 자체 부재,
//	         제출 여부는 picked 로만 공개.
//	revealing: picks 일괄 공개 (카드 오름차순, 전원 포함).
//	다음 트릭: picks 다시 부재, picked 리셋.
func TestNMHiddenState(t *testing.T) {
	h := NewNMHub()
	room := h.lobbyRoomFor("")
	clients := make([]*NMClient, 3)
	for i := range clients {
		c := &NMClient{wsClient: newBotWSClient(), Hub: h}
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

	// ---- picking: yourHand 본인만, picks 부재 ----
	for viewer := 0; viewer < 3; viewer++ {
		state := h.buildNMState(room, viewer)
		if state.Phase != NMPhasePicking || state.YourSeat != viewer || state.Trick != 1 {
			t.Fatalf("viewer %d: phase=%s yourSeat=%d trick=%d", viewer, state.Phase, state.YourSeat, state.Trick)
		}
		want := game.Players[viewer].Hand
		if len(state.YourHand) != len(want) {
			t.Fatalf("viewer %d: yourHand %d장, want %d", viewer, len(state.YourHand), len(want))
		}
		for i, c := range want {
			if state.YourHand[i] != c {
				t.Fatalf("viewer %d: yourHand[%d]=%d, want %d", viewer, i, state.YourHand[i], c)
			}
		}
		if state.Picks != nil {
			t.Fatalf("viewer %d: picking 중 picks 노출: %v", viewer, state.Picks)
		}
		if state.ChooserSeat != -1 || state.LastPlacement != nil {
			t.Fatalf("viewer %d: chooser=%d lastPlacement=%v", viewer, state.ChooserSeat, state.LastPlacement)
		}
		if len(state.Rows) != NMRows {
			t.Fatalf("viewer %d: rows %d개", viewer, len(state.Rows))
		}
		// JSON 직렬화에서도 picks 키 자체가 없어야 한다
		raw, _ := json.Marshal(state)
		if strings.Contains(string(raw), `"picks"`) {
			t.Fatalf("viewer %d: JSON 에 picks 키 존재: %s", viewer, raw)
		}
	}
	// 관전자(-1)는 손패가 빈 배열 (null 금지)
	spec := h.buildNMState(room, -1)
	if spec.YourSeat != -1 || spec.YourHand == nil || len(spec.YourHand) != 0 {
		t.Fatalf("관전자 스냅샷: yourSeat=%d yourHand=%v", spec.YourSeat, spec.YourHand)
	}

	// ---- 두 명 제출 — picked 만 공개, picks 는 여전히 부재 ----
	picked := map[int]int{}
	for _, s := range []int{0, 1} {
		card := game.Players[s].Hand[0]
		picked[s] = card
		h.handleGameMessage(NMGameMessage{Client: clients[s],
			Message: NMMessage{Type: NMMsgPick, Payload: NMPickPayload{Card: card}}})
	}
	if game.Phase != NMPhasePicking {
		t.Fatalf("2/3 제출 후 phase = %s", game.Phase)
	}
	for viewer := -1; viewer < 3; viewer++ {
		state := h.buildNMState(room, viewer)
		if state.Picks != nil {
			t.Fatalf("viewer %d: 부분 제출 중 picks 노출", viewer)
		}
		for _, pv := range state.Players {
			_, want := picked[pv.Seat]
			if pv.Picked != want {
				t.Fatalf("viewer %d: seat%d picked=%v, want %v", viewer, pv.Seat, pv.Picked, want)
			}
		}
	}
	// 중복 제출은 거부된다 (손패 변화 없음)
	before := len(game.Players[0].Hand)
	h.handleGameMessage(NMGameMessage{Client: clients[0],
		Message: NMMessage{Type: NMMsgPick, Payload: NMPickPayload{Card: game.Players[0].Hand[0]}}})
	if len(game.Players[0].Hand) != before || game.Players[0].Pick != picked[0] {
		t.Fatal("중복 제출이 상태를 바꿨다")
	}

	// ---- 마지막 제출 → revealing: picks 일괄 공개 (오름차순, 전원) ----
	card2 := game.Players[2].Hand[0]
	picked[2] = card2
	h.handleGameMessage(NMGameMessage{Client: clients[2],
		Message: NMMessage{Type: NMMsgPick, Payload: NMPickPayload{Card: card2}}})
	if game.Phase != NMPhaseRevealing {
		t.Fatalf("전원 제출 후 phase = %s, want revealing", game.Phase)
	}
	for viewer := -1; viewer < 3; viewer++ {
		state := h.buildNMState(room, viewer)
		if len(state.Picks) != 3 {
			t.Fatalf("viewer %d: revealing picks = %v", viewer, state.Picks)
		}
		seen := map[int]int{}
		for i, e := range state.Picks {
			if i > 0 && state.Picks[i-1].Card >= e.Card {
				t.Fatalf("viewer %d: picks 미정렬: %v", viewer, state.Picks)
			}
			seen[e.Seat] = e.Card
		}
		for s, c := range picked {
			if seen[s] != c {
				t.Fatalf("viewer %d: seat%d 공개 카드 %d, want %d", viewer, s, seen[s], c)
			}
		}
		if state.EndsAt <= 0 {
			t.Fatalf("viewer %d: revealing endsAt = %d", viewer, state.EndsAt)
		}
	}

	// ---- 공개 연출 타이머 발화 → 배치 (행 선택은 소머리 최소로 대행) ----
	h.handlePhaseFired(nmPhaseSignal{GameID: game.ID, Seq: room.AfkSeq})
	for game.Phase == NMPhaseChoosingRow {
		chooser := game.ChooserSeat
		state := h.buildNMState(room, 0)
		if state.ChooserSeat != chooser || state.Picks == nil {
			t.Fatalf("choosing_row 스냅샷: chooser=%d picks=%v", state.ChooserSeat, state.Picks)
		}
		h.handleGameMessage(NMGameMessage{Client: clients[chooser],
			Message: NMMessage{Type: NMMsgChooseRow, Payload: NMChooseRowPayload{Row: game.MinHeadsRow()}}})
	}
	if game.Phase != NMPhasePicking || game.Trick != 2 {
		t.Fatalf("배치 후 phase=%s trick=%d, want picking/2", game.Phase, game.Trick)
	}
	for viewer := -1; viewer < 3; viewer++ {
		state := h.buildNMState(room, viewer)
		if state.Picks != nil {
			t.Fatalf("viewer %d: 2트릭 진입 후 picks 잔존", viewer)
		}
		for _, pv := range state.Players {
			if pv.Picked {
				t.Fatalf("viewer %d: seat%d picked 리셋 실패", viewer, pv.Seat)
			}
		}
		if viewer >= 0 && len(state.YourHand) != NMHandSize-1 {
			t.Fatalf("viewer %d: 2트릭 손패 %d장", viewer, len(state.YourHand))
		}
	}
}

// TestNMAfkAutoPick 접속만 유지한 채 아무것도 하지 않는 두 사람의 게임을
// AFK 타이머가 무작위 제출로 자동 진행시키는지 (endsAt 노출·트릭 전환 포함)
func TestNMAfkAutoPick(t *testing.T) {
	_, url, cleanup := newNMTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := nmDial(t, url)
	defer host.conn.Close()
	nmJoin(t, host, "잠수1", "")

	guest := nmDial(t, url)
	defer guest.conn.Close()
	nmJoin(t, guest, "잠수2", "")

	host.send(t, NMMessage{Type: NMMsgStart})
	state := guest.nmWaitState(t, "picking", func(s map[string]interface{}) bool {
		return s["phase"] == string(NMPhasePicking)
	})
	if ends := int64(state["endsAt"].(float64)); ends <= 0 {
		t.Fatalf("picking 스냅샷의 endsAt = %d, want unixMillis", ends)
	}

	// 전원 무행동 → AFK 이벤트(한글 문구)와 무작위 제출 확인
	afk := nmPayloadMap(t, guest.waitMatch(t, "afk-event", func(m NMMessage) bool {
		if m.Type != NMMsgEvent {
			return false
		}
		ev, ok := m.Payload.(map[string]interface{})
		return ok && ev["kind"] == "afk"
	}))
	if !strings.Contains(afk["message"].(string), "무작위 카드") {
		t.Fatalf("AFK 문구 = %v", afk["message"])
	}
	if _, has := afk["name"]; !has {
		t.Fatalf("AFK 이벤트에 name 부재: %v", afk)
	}

	// 공개 → 배치(필요 시 행 선택도 AFK 자동) → 2트릭 picking 도달
	trick2 := guest.nmWaitState(t, "trick2", func(s map[string]interface{}) bool {
		return s["phase"] == string(NMPhasePicking) && s["trick"].(float64) == 2
	})
	if _, has := trick2["picks"]; has {
		t.Fatalf("2트릭 picking 스냅샷에 picks 존재: %v", trick2["picks"])
	}
	rows := trick2["rows"].([]interface{})
	if len(rows) != NMRows {
		t.Fatalf("rows 길이 = %d", len(rows))
	}
	total := 0
	for _, rRaw := range rows {
		total += len(rRaw.([]interface{}))
	}
	// 1트릭에 2장이 배치됐다 (행을 먹었으면 그만큼 빠진다 — 4장 이상이면 충분)
	if total < NMRows {
		t.Fatalf("행 카드 총합 = %d", total)
	}
}

// TestNMRoomCodeAndSpectate 방 코드 발급·코드 입장·시작 후 코드 관전 진입.
// 관전자 스냅샷은 yourSeat -1 에 yourHand 빈 배열 (picking 중 picks 부재).
// 관전자의 행동은 전부 차단된다.
func TestNMRoomCodeAndSpectate(t *testing.T) {
	_, url, cleanup := newNMTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := nmDial(t, url)
	defer host.conn.Close()
	joined := nmJoin(t, host, "호스트", "NEW")
	code, _ := joined["roomCode"].(string)
	if len(code) != roomCodeLen {
		t.Fatalf("roomCode = %q, want %d자", code, roomCodeLen)
	}

	guest := nmDial(t, url)
	defer guest.conn.Close()
	guestJoined := nmJoin(t, guest, "친구", code)
	if guestJoined["roomCode"] != code || int(guestJoined["yourSeat"].(float64)) != 1 {
		t.Fatalf("코드 입장 실패: %v", guestJoined)
	}

	// 호스트가 시작 (2인) — 사람만이라 AFK 자동 제출로 천천히 진행된다
	host.send(t, NMMessage{Type: NMMsgStart})
	state := host.nmWaitState(t, "picking", func(s map[string]interface{}) bool {
		return s["phase"] == string(NMPhasePicking)
	})
	if state["roomCode"] != code || len(state["players"].([]interface{})) != 2 {
		t.Fatalf("시작 실패: %v", state["players"])
	}
	if len(state["yourHand"].([]interface{})) != NMHandSize {
		t.Fatalf("호스트 손패 = %v", state["yourHand"])
	}

	// 시작된 방의 코드로 들어오면 관전자
	spec := nmDial(t, url)
	defer spec.conn.Close()
	spec.send(t, NMMessage{Type: NMMsgJoinGame, Payload: NMJoinGamePayload{Name: "구경꾼", Room: code}})
	specJoined := nmPayloadMap(t, spec.waitFor(t, NMMsgSpectateJoined))
	if specJoined["roomCode"] != code {
		t.Fatalf("관전 입장 roomCode = %v", specJoined["roomCode"])
	}

	specState := nmPayloadMap(t, spec.waitFor(t, NMMsgGameState))
	if int(specState["yourSeat"].(float64)) != -1 {
		t.Fatalf("관전자 yourSeat = %v, want -1", specState["yourSeat"])
	}
	// 관전자에게 손패는 빈 배열 (null 이면 프론트가 죽는다)
	hand, ok := specState["yourHand"].([]interface{})
	if !ok || len(hand) != 0 {
		t.Fatalf("관전자 yourHand = %v", specState["yourHand"])
	}
	if specState["phase"] == string(NMPhasePicking) {
		if _, has := specState["picks"]; has {
			t.Fatalf("관전자 picking 스냅샷에 picks 존재: %v", specState["picks"])
		}
	}

	// 관전자는 어떤 행동도 못 한다
	spec.send(t, NMMessage{Type: NMMsgPick, Payload: NMPickPayload{Card: 1}})
	errPayload := nmPayloadMap(t, spec.waitFor(t, NMMsgError))
	if errPayload["message"] != spectatorDeniedMsg {
		t.Fatalf("관전자 차단 문구 = %v", errPayload["message"])
	}
}
