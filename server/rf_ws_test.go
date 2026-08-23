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

// 테스트에서는 대기 상태 마감을 짧게 낮춘다 (창 자동 통과·AFK 자동 진행)
func init() {
	rfTurnTimeout = 150 * time.Millisecond
	rfWindowTimeout = 120 * time.Millisecond
}

// rfTestClient 공용 testConn 에 리포메이션 메시지 타입의 waitFor 를 얹은 래퍼
type rfTestClient struct {
	testConn[RFMessage]
}

func newRFTestServer(t *testing.T, grace time.Duration) (*RFHub, string, func()) {
	t.Helper()
	hub := NewRFHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeRFWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func rfDial(t *testing.T, url string) *rfTestClient {
	t.Helper()
	return &rfTestClient{dialWS[RFMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *rfTestClient) waitFor(t *testing.T, msgType RFMessageType) RFMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m RFMessage) bool { return m.Type == msgType })
}

// poll 마감까지 다음 메시지 하나를 읽는다. 타임아웃은 실패가 아니라 ok=false
// 로 돌려준다 (봇 품질 측정에서 교착을 "집계"해야 하기 때문).
func (c *rfTestClient) poll(deadline time.Time) (RFMessage, bool) {
	var zero RFMessage
	for {
		if len(c.queue) > 0 {
			msg := c.queue[0]
			c.queue = c.queue[1:]
			return msg, true
		}
		if !time.Now().Before(deadline) {
			return zero, false
		}
		c.conn.SetReadDeadline(deadline)
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return zero, false
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var msg RFMessage
			if json.Unmarshal([]byte(line), &msg) != nil {
				continue
			}
			c.queue = append(c.queue, msg)
		}
	}
}

func rfPayloadMap(t *testing.T, msg RFMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// rfJoin 입장하고 rf_player_joined payload 를 돌려준다
func rfJoin(t *testing.T, c *rfTestClient, name, room string) map[string]interface{} {
	t.Helper()
	c.send(t, RFMessage{Type: RFMsgJoinGame, Payload: RFJoinGamePayload{Name: name, Room: room}})
	return rfPayloadMap(t, c.waitFor(t, RFMsgPlayerJoined))
}

// rfWaitPhase 지정한 phase 의 스냅샷이 올 때까지 소비
func (c *rfTestClient) rfWaitPhase(t *testing.T, phase string) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "rf_game_state("+phase+")", func(m RFMessage) bool {
		if m.Type != RFMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		return ok && state["phase"] == phase
	})
	return rfPayloadMap(t, msg)
}

// ==================== 완주 ====================

// TestRFFiveBotsCompleteGame 봇을 채운 5인 게임이 60초 안에 완주하는지 —
// 가장 중요한 회귀 장치 (창 교착·도전 상호작용·턴 미전환·같은 진영 공격
// 거부로 인한 진행 불가 감지). 좌석 0은 서버 연습봇 두뇌(rfBrain)를 WS 로
// 감싼 드라이버가 잡는다.
func TestRFFiveBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newRFTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := rfDial(t, url)
	defer c.conn.Close()
	rfJoin(t, c, "감독", "")
	c.send(t, RFMessage{Type: RFMsgFillBots}) // 5인까지 채우고 즉시 시작

	start := time.Now()
	brain := newRFBrain()
	deadline := start.Add(60 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "state-or-over", func(m RFMessage) bool {
			return m.Type == RFMsgGameState || m.Type == RFMsgGameOver
		})
		if msg.Type == RFMsgGameOver {
			over := rfPayloadMap(t, msg)
			winner, _ := over["winner"].(string)
			seats := over["winnerSeats"].([]interface{})
			names := over["winnerNames"].([]interface{})
			if len(seats) == 0 || len(seats) != len(names) {
				t.Fatalf("승자 목록 이상: %v / %v", seats, names)
			}
			players := over["players"].([]interface{})
			if len(players) != RFFillBotTarget {
				t.Fatalf("players 길이 = %d, want %d", len(players), RFFillBotTarget)
			}
			winnerSet := map[int]bool{}
			for _, s := range seats {
				winnerSet[int(s.(float64))] = true
			}
			switch winner {
			case "seat":
				if len(seats) != 1 {
					t.Fatalf("최후 1인인데 승자가 %d명", len(seats))
				}
				for _, pRaw := range players {
					p := pRaw.(map[string]interface{})
					seat := int(p["seat"].(float64))
					count := int(p["cardCount"].(float64))
					if winnerSet[seat] && count < 1 {
						t.Fatalf("승자 카드 0장: %v", p)
					}
					if !winnerSet[seat] && count != 0 {
						t.Fatalf("패자 seat%d 카드 %d장 잔존", seat, count)
					}
				}
			case string(RFFactionLoyalist), string(RFFactionReformist):
				// 진영 승리 — 살아남은 전원이 승자이고 모두 같은 진영이다
				for _, pRaw := range players {
					p := pRaw.(map[string]interface{})
					seat := int(p["seat"].(float64))
					alive := p["alive"].(bool)
					if alive != winnerSet[seat] {
						t.Fatalf("진영 승리 승자/생존 불일치: %v", p)
					}
					if alive && p["faction"] != winner {
						t.Fatalf("승리 진영과 다른 생존자: %v", p)
					}
				}
			default:
				t.Fatalf("winner = %q", winner)
			}
			t.Logf("완주: winner=%s seats=%v (%.1fs)", winner, seats, time.Since(start).Seconds())
			return
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("60초 안에 게임이 끝나지 않았다 — 진행 불가 상태")
}

// ==================== 봇 품질 측정 ====================

// TestRFBotQuality 5봇 30판 — 진영 승리 대 최후 1인 승리의 비율, 평균 소요
// 차례, 교착 여부를 집계해 로그로 남긴다. 확장 규칙(개종·횡령)이 봇 판단에
// 실제로 반영되는지도 함께 본다.
func TestRFBotQuality(t *testing.T) {
	if testing.Short() {
		t.Skip("짧은 실행에서는 30판 측정을 건너뛴다")
	}
	_, url, cleanup := newRFTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	const games = 30
	var factionWins, seatWins, stalls, totalTurns, converts, embezzles int
	factionBy := map[string]int{}

	for i := 0; i < games; i++ {
		func() {
			c := rfDial(t, url)
			defer c.conn.Close()
			rfJoin(t, c, fmt.Sprintf("감독%d", i), "")
			c.send(t, RFMessage{Type: RFMsgFillBots})

			brain := newRFBrain()
			deadline := time.Now().Add(30 * time.Second)
			turns := 0
			for {
				msg, ok := c.poll(deadline)
				if !ok {
					stalls++
					t.Logf("게임 %d: 교착 (차례 %d)", i, turns)
					return
				}
				switch msg.Type {
				case RFMsgEvent:
					ev := rfPayloadMap(t, msg)
					switch ev["kind"] {
					case "action", "convert", "convert_other":
						turns++
					}
					switch ev["kind"] {
					case "convert", "convert_other":
						converts++
					case "embezzle_proof":
						embezzles++
					}
				case RFMsgGameOver:
					over := rfPayloadMap(t, msg)
					winner, _ := over["winner"].(string)
					if winner == "seat" {
						seatWins++
					} else {
						factionWins++
						factionBy[winner]++
					}
					totalTurns += turns
					return
				case RFMsgGameState:
					if reply := brain.decide(msg); reply != nil {
						c.send(t, *reply)
					}
				}
			}
		}()
	}

	finished := games - stalls
	avgTurns := 0.0
	if finished > 0 {
		avgTurns = float64(totalTurns) / float64(finished)
	}
	t.Logf("[봇 품질] 5봇 %d판 | 진영 승리 %d판 (%.0f%%) %v | 최후 1인 %d판 (%.0f%%) | 교착 %d판 | 평균 소요 차례 %.1f | 개종 %d회 | 횡령 도전 증명 %d회",
		games, factionWins, 100*float64(factionWins)/float64(games), factionBy,
		seatWins, 100*float64(seatWins)/float64(games), stalls, avgTurns, converts, embezzles)

	if stalls > 0 {
		t.Fatalf("교착 %d판 — 진행 불가 상태가 있다", stalls)
	}
	if factionWins == 0 {
		t.Fatal("30판 동안 진영 승리가 한 번도 나오지 않았다 — 확장 규칙 미작동 의심")
	}
	if avgTurns <= 0 {
		t.Fatalf("평균 소요 차례 = %.1f", avgTurns)
	}
}

// ==================== 은닉 ====================

// TestRFHiddenState 은닉의 핵심 계약 — 허브 고루틴 없이 핸들러를 직접 불러
// 결정적으로 검증한다. yourRoles/yourExchange 는 본인 스냅샷에만 키가
// 존재하고, 타인·관전자의 raw JSON 에는 키 자체가 없어야 한다.
// 반대로 faction 과 lostRoles 는 전원에게 공개된다.
func TestRFHiddenState(t *testing.T) {
	h := NewRFHub()
	room := h.lobbyRoomFor("")
	clients := make([]*RFClient, 3)
	for i := range clients {
		c := &RFClient{wsClient: newBotWSClient(), Hub: h}
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
	game.Treasury = 4
	rfSetFactions(game, RFFactionLoyalist, RFFactionReformist, RFFactionReformist)
	rfSetCards(game, 0, RFRoleDuke, RFRoleAssassin)
	rfSetCards(game, 1, RFRoleCaptain, RFRoleCaptain)
	rfSetCards(game, 2, RFRoleContessa, RFRoleContessa)
	// 덱까지 결정적으로 — 교환으로 뽑히는 2장도 duke 라 유출 검사가 성립한다
	game.Deck = make([]RFRole, 9)
	for i := range game.Deck {
		game.Deck[i] = RFRoleDuke
	}

	rawOf := func(viewer int) string {
		data, err := json.Marshal(h.buildRFState(room, viewer))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(data)
	}
	mustHide := func(viewer int, raw string, roles ...RFRole) {
		t.Helper()
		for _, r := range roles {
			if strings.Contains(raw, string(r)) {
				t.Fatalf("viewer %d 스냅샷에 %s 유출:\n%s", viewer, r, raw)
			}
		}
	}

	// ---- action 단계: 본인 카드만 보인다 ----
	raw0 := rawOf(0)
	if !strings.Contains(raw0, `"yourRoles":["duke","assassin"]`) {
		t.Fatalf("본인 카드 부재:\n%s", raw0)
	}
	mustHide(0, raw0, RFRoleCaptain, RFRoleContessa, RFRoleAmbassador)

	raw1 := rawOf(1)
	if !strings.Contains(raw1, `"yourRoles":["captain","captain"]`) {
		t.Fatalf("본인 카드 부재:\n%s", raw1)
	}
	mustHide(1, raw1, RFRoleDuke, RFRoleAssassin, RFRoleContessa, RFRoleAmbassador)

	// 관전자(-1)에게는 yourRoles/yourExchange 키 자체가 없다
	rawSpec := rawOf(-1)
	for _, key := range []string{"yourRoles", "yourExchange"} {
		if strings.Contains(rawSpec, key) {
			t.Fatalf("관전자 스냅샷에 %s 키 존재:\n%s", key, rawSpec)
		}
	}
	mustHide(-1, rawSpec, RFRoleDuke, RFRoleAssassin, RFRoleCaptain, RFRoleContessa, RFRoleAmbassador)
	// 타인 스냅샷에도 yourExchange 키는 없다 (교환 중이 아니므로 본인도 없다)
	if strings.Contains(raw1, "yourExchange") {
		t.Fatalf("교환 중이 아닌데 yourExchange 키 존재:\n%s", raw1)
	}

	specState := h.buildRFState(room, -1)
	if specState.YourSeat != -1 || specState.DeckCount != 9 || specState.Treasury != 4 {
		t.Fatalf("관전자 스냅샷: yourSeat=%d deckCount=%d treasury=%d",
			specState.YourSeat, specState.DeckCount, specState.Treasury)
	}
	if specState.YourRoles != nil || specState.YourExchange != nil {
		t.Fatal("관전자 스냅샷에 은닉 필드가 채워졌다")
	}
	// 진영은 전원 공개
	wantFactions := []RFFaction{RFFactionLoyalist, RFFactionReformist, RFFactionReformist}
	for i, pv := range specState.Players {
		if pv.Faction != wantFactions[i] {
			t.Fatalf("seat%d faction = %q, want %q", i, pv.Faction, wantFactions[i])
		}
		if pv.LostRoles == nil {
			t.Fatalf("seat%d lostRoles 가 null 이다", i)
		}
	}
	if !strings.Contains(rawSpec, `"lostRoles":[]`) {
		t.Fatalf("빈 lostRoles 가 [] 가 아니다:\n%s", rawSpec)
	}

	// ---- 교환: 선택지는 본인에게만 ----
	h.handleGameMessage(RFGameMessage{Client: clients[0], Message: RFMessage{
		Type: RFMsgAction, Payload: RFActionPayload{Kind: string(RFActExchange)}}})
	if game.Phase != RFPhaseChallengeWindow {
		t.Fatalf("phase = %s, want challenge_window", game.Phase)
	}
	if h.buildRFState(room, 1).EndsAt <= 0 {
		t.Fatal("창 스냅샷의 endsAt 부재")
	}
	h.handleGameMessage(RFGameMessage{Client: clients[1], Message: RFMessage{Type: RFMsgPass}})
	if game.Phase != RFPhaseChallengeWindow {
		t.Fatalf("한 명 통과만으로 창이 닫혔다: %s", game.Phase)
	}
	// 통과 목록은 공개 정보다
	if pv := h.buildRFState(room, 2).Pending; pv == nil || len(pv.Passed) != 1 || pv.Passed[0] != 1 {
		t.Fatalf("pending.passed = %+v", pv)
	}
	h.handleGameMessage(RFGameMessage{Client: clients[2], Message: RFMessage{Type: RFMsgPass}})
	if game.Phase != RFPhaseExchange {
		t.Fatalf("phase = %s, want exchange", game.Phase)
	}

	raw0 = rawOf(0)
	if !strings.Contains(raw0, `"yourExchange":["duke","assassin","duke","duke"]`) {
		t.Fatalf("교환 선택지 부재:\n%s", raw0)
	}
	raw1 = rawOf(1)
	if strings.Contains(raw1, "yourExchange") {
		t.Fatalf("타인 스냅샷에 yourExchange 키 존재:\n%s", raw1)
	}
	mustHide(1, raw1, RFRoleDuke, RFRoleAssassin, RFRoleContessa)

	h.handleGameMessage(RFGameMessage{Client: clients[0], Message: RFMessage{
		Type: RFMsgExchange, Payload: RFExchangePayload{Keep: []int{2, 3}}}})
	if game.Phase != RFPhaseAction || game.CurrentSeat != 1 {
		t.Fatalf("교환 뒤 턴 전환 실패: phase=%s current=%d", game.Phase, game.CurrentSeat)
	}
	if len(game.Deck) != 9 {
		t.Fatalf("덱 장수 = %d, want 9", len(game.Deck))
	}

	// ---- 잃은 카드는 전원에게 보인다 (은닉 해제의 유일한 경로) ----
	game.Players[2].Cards[0].Revealed = true
	state1 := h.buildRFState(room, 1)
	found := false
	for _, pv := range state1.Players {
		if pv.Seat == 2 {
			if pv.CardCount != 1 || len(pv.LostRoles) != 1 || pv.LostRoles[0] != string(RFRoleContessa) {
				t.Fatalf("공개 카드 미반영: %+v", pv)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("seat2 부재")
	}
}

// ==================== 진영 승리 (허브 경로) ====================

// rfDrainSend 소켓 없는 테스트 클라이언트의 송신 큐를 비운다
func rfDrainSend(c *RFClient) []RFMessage {
	msgs := []RFMessage{}
	for {
		select {
		case data := <-c.Send:
			var m RFMessage
			if json.Unmarshal(data, &m) == nil {
				msgs = append(msgs, m)
			}
		default:
			return msgs
		}
	}
}

// TestRFFactionVictoryBroadcast 진영 승리가 rf_game_over 로 제대로 나가는지 —
// 승자 좌석·이름이 여럿이고 최종 스냅샷의 result 도 채워져 있어야 한다.
func TestRFFactionVictoryBroadcast(t *testing.T) {
	h := NewRFHub()
	room := h.lobbyRoomFor("")
	clients := make([]*RFClient, 3)
	for i := range clients {
		c := &RFClient{wsClient: newBotWSClient(), Hub: h}
		c.Bot = false
		c.Name = fmt.Sprintf("P%d", i)
		seat, _ := room.Game.AddPlayer(c.Name)
		c.GameID = room.Game.ID
		c.Seat = seat
		room.Clients[seat] = c
		h.sessions[c.SessionID] = c
		clients[i] = c
	}
	h.startGame(room)

	game := room.Game
	rfSetFactions(game, RFFactionLoyalist, RFFactionLoyalist, RFFactionReformist)
	rfSetCards(game, 0, RFRoleDuke, RFRoleDuke)
	rfSetCards(game, 1, RFRoleDuke, RFRoleDuke)
	rfSetCards(game, 2, RFRoleCaptain, RFRoleCaptain)
	game.CurrentSeat = 0
	game.Players[0].Chips = RFCoupCost
	for _, c := range clients {
		rfDrainSend(c)
	}

	// seat2(유일한 개혁파)를 쿠로 몰아낸다 — 카드 1장씩 두 번
	ts := 2
	h.handleGameMessage(RFGameMessage{Client: clients[0], Message: RFMessage{
		Type: RFMsgAction, Payload: RFActionPayload{Kind: string(RFActCoup), TargetSeat: &ts}}})
	h.handleGameMessage(RFGameMessage{Client: clients[2], Message: RFMessage{
		Type: RFMsgLoseCard, Payload: RFLoseCardPayload{Index: 0}}})
	game.CurrentSeat = 1
	game.Players[1].Chips = RFCoupCost
	h.handleGameMessage(RFGameMessage{Client: clients[1], Message: RFMessage{
		Type: RFMsgAction, Payload: RFActionPayload{Kind: string(RFActCoup), TargetSeat: &ts}}})

	if game.Phase != RFPhaseGameOver {
		t.Fatalf("phase = %s, want game_over", game.Phase)
	}

	msgs := rfDrainSend(clients[0])
	var over *RFGameOverPayload
	var lastState *RFGameStatePayload
	for _, m := range msgs {
		switch m.Type {
		case RFMsgGameOver:
			p, ok := botPayloadAs[RFGameOverPayload](m.Payload)
			if !ok {
				t.Fatalf("rf_game_over 페이로드 파싱 실패: %#v", m.Payload)
			}
			over = &p
		case RFMsgGameState:
			p, ok := botPayloadAs[RFGameStatePayload](m.Payload)
			if !ok {
				t.Fatalf("rf_game_state 페이로드 파싱 실패: %#v", m.Payload)
			}
			lastState = &p
		}
	}
	if over == nil {
		t.Fatal("rf_game_over 미수신")
	}
	if over.Winner != string(RFFactionLoyalist) {
		t.Fatalf("winner = %q, want loyalist", over.Winner)
	}
	if len(over.WinnerSeats) != 2 || over.WinnerSeats[0] != 0 || over.WinnerSeats[1] != 1 {
		t.Fatalf("winnerSeats = %v, want [0 1]", over.WinnerSeats)
	}
	if len(over.WinnerNames) != 2 || over.WinnerNames[0] != "P0" {
		t.Fatalf("winnerNames = %v", over.WinnerNames)
	}
	if !strings.Contains(over.Message, "충성파 진영 승리") {
		t.Fatalf("종료 문구 = %q", over.Message)
	}
	if lastState == nil || lastState.Result == nil {
		t.Fatal("최종 스냅샷의 result 부재")
	}
	if lastState.Result.Winner != string(RFFactionLoyalist) || len(lastState.Result.WinnerSeats) != 2 {
		t.Fatalf("스냅샷 result = %+v", lastState.Result)
	}
	if lastState.Phase != RFPhaseGameOver || lastState.CurrentSeat != -1 {
		t.Fatalf("최종 스냅샷 = phase %s current %d", lastState.Phase, lastState.CurrentSeat)
	}
}

// ==================== AFK 자동 진행 ====================

// TestRFAfkAutoProgress 접속만 유지한 채 아무것도 하지 않는 2인전 —
// 세금 선언 뒤 아무도 통과를 누르지 않아도 도전 창이 마감으로 닫히고(+3),
// 이후 전원 방치로도 자동 수입 → 쿠 강제 → 무작위 제거를 거쳐 게임이
// 끝까지 완주하는지 (endsAt 노출·afk 이벤트 포함).
func TestRFAfkAutoProgress(t *testing.T) {
	_, url, cleanup := newRFTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := rfDial(t, url)
	defer host.conn.Close()
	rfJoin(t, host, "잠수1", "")

	guest := rfDial(t, url)
	defer guest.conn.Close()
	rfJoin(t, guest, "잠수2", "")

	host.send(t, RFMessage{Type: RFMsgStart})
	state := host.rfWaitPhase(t, string(RFPhaseAction))
	if ends := int64(state["endsAt"].(float64)); ends <= 0 {
		t.Fatalf("action 스냅샷의 endsAt = %d, want unixMillis", ends)
	}
	if _, ok := state["treasury"]; !ok {
		t.Fatalf("treasury 키 부재: %v", state)
	}

	// guest 는 더 읽지 않는다 — 백그라운드로 비워 버퍼 포화만 막는다
	go func() {
		for {
			if _, _, err := guest.conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	conns := map[int]*rfTestClient{0: host, 1: guest}

	// 현재 차례 좌석으로 세금을 선언한다 (AFK 자동 수입과 경합할 수 있어
	// action 스냅샷이 올 때마다 재시도 — 하나만 창에 안착하면 된다)
	var window map[string]interface{}
	taxDeadline := time.Now().Add(10 * time.Second)
	for window == nil && time.Now().Before(taxDeadline) {
		msg := host.waitMatch(t, "rf_game_state", func(m RFMessage) bool {
			return m.Type == RFMsgGameState
		})
		st := rfPayloadMap(t, msg)
		switch st["phase"] {
		case string(RFPhaseChallengeWindow):
			window = st
		case string(RFPhaseAction):
			cur := int(st["currentSeat"].(float64))
			conns[cur].send(t, RFMessage{Type: RFMsgAction,
				Payload: RFActionPayload{Kind: string(RFActTax)}})
		}
	}
	if window == nil {
		t.Fatal("세금 도전 창이 열리지 않았다")
	}
	pending := asPayloadMap(t, window["pending"])
	actorSeat := int(pending["bySeat"].(float64))
	if pending["claimRole"] != string(RFRoleDuke) {
		t.Fatalf("claimRole = %v, want duke", pending["claimRole"])
	}
	prevCoins := 0
	for _, pRaw := range window["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		if int(p["seat"].(float64)) == actorSeat {
			prevCoins = int(p["coins"].(float64))
		}
	}
	// 아무도 통과를 누르지 않는다 — 마감 경과만으로 창이 닫히고 세금이 걷힌다
	host.waitMatch(t, "tax-resolved", func(m RFMessage) bool {
		if m.Type != RFMsgGameState {
			return false
		}
		st, ok := m.Payload.(map[string]interface{})
		if !ok {
			return false
		}
		for _, pRaw := range st["players"].([]interface{}) {
			p := pRaw.(map[string]interface{})
			if int(p["seat"].(float64)) == actorSeat {
				return int(p["coins"].(float64)) == prevCoins+3
			}
		}
		return false
	})

	// 이후 전원 방치 — 자동 수입·쿠 강제·무작위 제거로 완주해야 한다
	sawAfk := false
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		msg := host.waitMatch(t, "event-or-over", func(m RFMessage) bool {
			return m.Type == RFMsgEvent || m.Type == RFMsgGameOver
		})
		if msg.Type == RFMsgEvent {
			ev := rfPayloadMap(t, msg)
			if ev["kind"] == "afk" {
				if !strings.Contains(ev["message"].(string), "자동") {
					t.Fatalf("afk 문구 = %v", ev["message"])
				}
				sawAfk = true
			}
			continue
		}
		over := rfPayloadMap(t, msg)
		if len(over["winnerSeats"].([]interface{})) == 0 {
			t.Fatalf("승자 없음: %v", over)
		}
		if !sawAfk {
			t.Fatal("afk 자동 진행 이벤트가 한 번도 없었다")
		}
		return
	}
	t.Fatal("전원 방치 게임이 45초 안에 끝나지 않았다")
}

// ==================== 방 코드 / 관전 ====================

// TestRFRoomCodeAndSpectate 방 코드 발급·코드 입장·시작 후 코드 관전 진입.
// 관전자 스냅샷은 yourSeat -1 에 은닉 필드 부재. 행동은 전부 차단된다.
func TestRFRoomCodeAndSpectate(t *testing.T) {
	_, url, cleanup := newRFTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := rfDial(t, url)
	defer host.conn.Close()
	joined := rfJoin(t, host, "호스트", "NEW")
	code, _ := joined["roomCode"].(string)
	if len(code) != roomCodeLen {
		t.Fatalf("roomCode = %q, want %d자", code, roomCodeLen)
	}

	guest := rfDial(t, url)
	defer guest.conn.Close()
	guestJoined := rfJoin(t, guest, "친구", code)
	if guestJoined["roomCode"] != code || int(guestJoined["yourSeat"].(float64)) != 1 {
		t.Fatalf("코드 입장 실패: %v", guestJoined)
	}

	host.send(t, RFMessage{Type: RFMsgStart})
	state := host.rfWaitPhase(t, string(RFPhaseAction))
	if state["roomCode"] != code || len(state["players"].([]interface{})) != 2 {
		t.Fatalf("시작 실패: %v", state["players"])
	}
	if cards := state["yourRoles"].([]interface{}); len(cards) != RFCardsPerPlayer {
		t.Fatalf("호스트 비공개 카드 = %v", cards)
	}
	// 2인전은 진영이 하나씩 갈린다
	factions := map[string]int{}
	for _, pRaw := range state["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		factions[p["faction"].(string)]++
	}
	if factions[string(RFFactionLoyalist)] != 1 || factions[string(RFFactionReformist)] != 1 {
		t.Fatalf("2인전 진영 분배 = %v", factions)
	}

	// 시작된 방의 코드로 들어오면 관전자
	spec := rfDial(t, url)
	defer spec.conn.Close()
	spec.send(t, RFMessage{Type: RFMsgJoinGame, Payload: RFJoinGamePayload{Name: "구경꾼", Room: code}})
	specJoined := rfPayloadMap(t, spec.waitFor(t, RFMsgSpectateJoined))
	if specJoined["roomCode"] != code {
		t.Fatalf("관전 입장 roomCode = %v", specJoined["roomCode"])
	}

	specState := rfPayloadMap(t, spec.waitFor(t, RFMsgGameState))
	if int(specState["yourSeat"].(float64)) != -1 {
		t.Fatalf("관전자 yourSeat = %v, want -1", specState["yourSeat"])
	}
	if _, ok := specState["yourRoles"]; ok {
		t.Fatalf("관전자 스냅샷에 yourRoles 키 존재: %v", specState)
	}
	if _, ok := specState["yourExchange"]; ok {
		t.Fatalf("관전자 스냅샷에 yourExchange 키 존재: %v", specState)
	}
	for _, pRaw := range specState["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		if int(p["cardCount"].(float64)) < 1 {
			t.Fatalf("관전자 스냅샷 좌석 정보 이상: %v", p)
		}
		if p["faction"] == "" { // 진영은 관전자에게도 공개
			t.Fatalf("관전자에게 진영 미공개: %v", p)
		}
	}

	// 관전자는 어떤 행동도 못 한다
	spec.send(t, RFMessage{Type: RFMsgPass})
	errPayload := rfPayloadMap(t, spec.waitFor(t, RFMsgError))
	if errPayload["message"] != spectatorDeniedMsg {
		t.Fatalf("관전자 차단 문구 = %v", errPayload["message"])
	}
}

// ==================== 재접속 / 봇 대체 ====================

// TestRFReconnect 진행 중 끊긴 좌석이 세션 ID로 돌아와 손패까지 복원되는지
func TestRFReconnect(t *testing.T) {
	_, url, cleanup := newRFTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := rfDial(t, url)
	defer host.conn.Close()
	rfJoin(t, host, "호스트", "")

	guest := rfDial(t, url)
	guestJoined := rfJoin(t, guest, "친구", "")
	sessionID, _ := guestJoined["sessionId"].(string)
	if sessionID == "" {
		t.Fatal("sessionId 미발급")
	}

	host.send(t, RFMessage{Type: RFMsgStart})
	host.rfWaitPhase(t, string(RFPhaseAction))

	guest.conn.Close()
	dc := rfPayloadMap(t, host.waitFor(t, RFMsgPlayerDisconnected))
	if int(dc["seat"].(float64)) != 1 {
		t.Fatalf("연결 끊김 좌석 = %v", dc["seat"])
	}

	back := rfDial(t, url)
	defer back.conn.Close()
	back.send(t, RFMessage{Type: RFMsgRejoin, Payload: RFRejoinPayload{SessionID: sessionID}})
	rc := rfPayloadMap(t, back.waitFor(t, RFMsgPlayerReconnected))
	if int(rc["seat"].(float64)) != 1 || rc["name"] != "친구" {
		t.Fatalf("재접속 정보 = %v", rc)
	}

	state := rfPayloadMap(t, back.waitFor(t, RFMsgGameState))
	if int(state["yourSeat"].(float64)) != 1 {
		t.Fatalf("복원된 yourSeat = %v", state["yourSeat"])
	}
	roles, ok := state["yourRoles"].([]interface{})
	if !ok || len(roles) == 0 {
		t.Fatalf("재접속 후 손패 미복원: %v", state["yourRoles"])
	}
}

// TestRFBotTakeover 유예가 만료된 좌석을 연습봇이 이어받고 게임이 계속되는지
func TestRFBotTakeover(t *testing.T) {
	_, url, cleanup := newRFTestServer(t, 80*time.Millisecond)
	defer cleanup()

	host := rfDial(t, url)
	defer host.conn.Close()
	rfJoin(t, host, "호스트", "")

	guest := rfDial(t, url)
	rfJoin(t, guest, "이탈자", "")

	host.send(t, RFMessage{Type: RFMsgStart})
	host.rfWaitPhase(t, string(RFPhaseAction))
	guest.conn.Close()

	ev := rfPayloadMap(t, host.waitMatch(t, "bot_takeover", func(m RFMessage) bool {
		if m.Type != RFMsgEvent {
			return false
		}
		p, ok := m.Payload.(map[string]interface{})
		return ok && p["kind"] == "bot_takeover"
	}))
	if ev["name"] != "이탈자" {
		t.Fatalf("봇 대체 이벤트 = %v", ev)
	}
	if !strings.Contains(ev["message"].(string), "봇이 이어받았습니다") {
		t.Fatalf("봇 대체 문구 = %v", ev["message"])
	}

	// 대체된 뒤에도 스냅샷에 bot 표기가 반영된다
	state := host.waitMatch(t, "bot-flag", func(m RFMessage) bool {
		if m.Type != RFMsgGameState {
			return false
		}
		st, ok := m.Payload.(map[string]interface{})
		if !ok {
			return false
		}
		for _, pRaw := range st["players"].([]interface{}) {
			p := pRaw.(map[string]interface{})
			if int(p["seat"].(float64)) == 1 && p["bot"] == true {
				return true
			}
		}
		return false
	})
	if state.Type != RFMsgGameState {
		t.Fatal("봇 표기 스냅샷 부재")
	}
}
