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

// 테스트에서는 대기 상태 마감을 짧게 낮춘다 (자동 플레이·핸드 전환)
func init() {
	dmTurnTimeout = 80 * time.Millisecond
	dmHandEndTimeout = 40 * time.Millisecond
}

// dmTestClient 공용 testConn 에 달무티 메시지 타입의 waitFor 를 얹은 래퍼
type dmTestClient struct {
	testConn[DMMessage]
}

func newDMTestServer(t *testing.T, grace time.Duration) (*DMHub, string, func()) {
	t.Helper()
	hub := NewDMHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeDMWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func dmDial(t *testing.T, url string) *dmTestClient {
	t.Helper()
	return &dmTestClient{dialWS[DMMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *dmTestClient) waitFor(t *testing.T, msgType DMMessageType) DMMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m DMMessage) bool { return m.Type == msgType })
}

func dmPayloadMap(t *testing.T, msg DMMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// dmJoin 입장하고 dm_player_joined payload 를 돌려준다
func dmJoin(t *testing.T, c *dmTestClient, name, room string) map[string]interface{} {
	t.Helper()
	c.send(t, DMMessage{Type: DMMsgJoinGame, Payload: DMJoinGamePayload{Name: name, Room: room}})
	return dmPayloadMap(t, c.waitFor(t, DMMsgPlayerJoined))
}

// dmWaitPhase 지정한 phase 의 스냅샷이 올 때까지 소비
func (c *dmTestClient) dmWaitPhase(t *testing.T, phase string) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "dm_game_state("+phase+")", func(m DMMessage) bool {
		if m.Type != DMMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		return ok && state["phase"] == phase
	})
	return dmPayloadMap(t, msg)
}

// dmDrain 읽지 않는 연결의 버퍼 포화를 막는 백그라운드 소비
func dmDrain(c *dmTestClient) {
	go func() {
		for {
			if _, _, err := c.conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

// TestDMFiveBotsCompleteGame 봇을 채운 5인 3핸드 게임이 30초 안에 완주하는지 —
// 가장 중요한 회귀 장치 (클라이밍 교착·리드 미전환·핸드 전환 감지).
// 좌석 0은 서버 연습봇 두뇌(dmBrain)를 WS 로 감싼 드라이버가 잡는다.
func TestDMFiveBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newDMTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := dmDial(t, url)
	defer c.conn.Close()
	dmJoin(t, c, "감독", "")
	c.send(t, DMMessage{Type: DMMsgFillBots}) // 5인까지 채우고 즉시 시작

	start := time.Now()
	brain := newDMBrain()
	seenHands := map[float64]bool{}
	deadline := start.Add(30 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "state-or-over", func(m DMMessage) bool {
			return m.Type == DMMsgGameState || m.Type == DMMsgGameOver
		})
		if msg.Type == DMMsgGameOver {
			over := dmPayloadMap(t, msg)
			seats, _ := over["winnerSeats"].([]interface{})
			if len(seats) == 0 {
				t.Fatalf("winnerSeats 비어 있음: %v", over)
			}
			names, _ := over["winnerNames"].([]interface{})
			if len(names) != len(seats) {
				t.Fatalf("winnerNames = %v, seats = %v", names, seats)
			}
			players := over["players"].([]interface{})
			if len(players) != DMFillBotTarget {
				t.Fatalf("players 길이 = %d, want %d", len(players), DMFillBotTarget)
			}
			// 총점은 3핸드 × (4+3+2+1+0) = 30점이 정확히 분배된다
			total, best, withCards := 0, -1, 0
			for _, pRaw := range players {
				p := pRaw.(map[string]interface{})
				pts := int(p["points"].(float64))
				total += pts
				if pts > best {
					best = pts
				}
				if !p["out"].(bool) || int(p["rank"].(float64)) == 0 {
					t.Fatalf("종료 후 순위 미확정 좌석: %v", p)
				}
				// 마지막 한 명(꼴찌)만 카드를 남긴 채 핸드가 끝난다
				if int(p["handCount"].(float64)) > 0 {
					withCards++
					if int(p["rank"].(float64)) != DMFillBotTarget {
						t.Fatalf("꼴찌가 아닌데 손패가 남았다: %v", p)
					}
				}
			}
			if withCards > 1 {
				t.Fatalf("손패가 남은 좌석이 %d명이다", withCards)
			}
			if total != DMHands*10 {
				t.Fatalf("총점 합 = %d, want %d", total, DMHands*10)
			}
			for _, sRaw := range seats {
				s := int(sRaw.(float64))
				p := players[s].(map[string]interface{})
				if int(p["points"].(float64)) != best {
					t.Fatalf("승자 seat%d 점수 = %v, want %d", s, p["points"], best)
				}
			}
			if len(seenHands) != DMHands {
				t.Fatalf("진행한 핸드 = %v, want %d핸드", seenHands, DMHands)
			}
			t.Logf("완주: winners=%v (%.1fs)", seats, time.Since(start).Seconds())
			return
		}
		state := dmPayloadMap(t, msg)
		if hn, ok := state["handNo"].(float64); ok && hn > 0 {
			seenHands[hn] = true
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("30초 안에 게임이 끝나지 않았다 — 진행 불가 상태")
}

// TestDMHiddenState 은닉의 핵심 계약 — 허브 고루틴 없이 핸들러를 직접 불러
// 결정적으로 검증한다. yourHand 는 본인 스냅샷에만 실리고(빈 손도 빈 배열 []),
// 타인·관전자의 raw JSON 에는 필드 자체가 없어야 한다. handCount 는 공개다.
func TestDMHiddenState(t *testing.T) {
	h := NewDMHub()
	room := h.lobbyRoomFor("")

	// 시작 전에도 관전자 스냅샷이 만들어져야 한다 (패닉 금지)
	if empty := h.buildDMState(room, -1); empty.YourSeat != -1 || len(empty.Players) != 0 {
		t.Fatalf("빈 방 관전자 스냅샷 = %+v", empty)
	}

	clients := make([]*DMClient, DMMinPlayers)
	for i := range clients {
		c := &DMClient{wsClient: newBotWSClient(), Hub: h}
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
	defer h.stopPhaseTimer(room)

	game := room.Game
	// 결정적 손패로 갈아끼운다
	game.Players[0].Hand = []int{7, 7, 9}
	game.Players[1].Hand = []int{5, DMJoker, 11}
	game.Players[2].Hand = []int{3, 3, 12}
	game.Players[3].Hand = []int{2, 6, 10}
	game.LeadSeat, game.CurrentSeat = 0, 0
	game.Table = nil

	rawOf := func(viewer int) string {
		data, err := json.Marshal(h.buildDMState(room, viewer))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(data)
	}

	// ---- yourHand 는 본인에게만 (타인·관전자는 필드 부재) ----
	if raw0 := rawOf(0); !strings.Contains(raw0, `"yourHand":[7,7,9]`) {
		t.Fatalf("본인 손패 부재:\n%s", raw0)
	}
	for _, viewer := range []int{-1} {
		if raw := rawOf(viewer); strings.Contains(raw, "yourHand") {
			t.Fatalf("관전자 스냅샷에 yourHand 유출:\n%s", raw)
		}
	}
	// 타인 스냅샷에는 그 사람의 손패만 실리고 남의 손패는 없다
	raw1 := rawOf(1)
	if !strings.Contains(raw1, `"yourHand":[5,13,11]`) && !strings.Contains(raw1, `"yourHand":[5,11,13]`) {
		t.Fatalf("seat1 손패 스냅샷 이상:\n%s", raw1)
	}
	if strings.Contains(raw1, "7,7,9") {
		t.Fatalf("seat1 스냅샷에 남의 손패 유출:\n%s", raw1)
	}
	// handCount 는 공개 정보
	if !strings.Contains(rawOf(-1), `"handCount":3`) {
		t.Fatalf("handCount 미공개:\n%s", rawOf(-1))
	}
	spec := h.buildDMState(room, -1)
	if spec.YourSeat != -1 || spec.YourHand != nil || len(spec.Players) != DMMinPlayers {
		t.Fatalf("관전자 스냅샷: yourSeat=%d hand=%v", spec.YourSeat, spec.YourHand)
	}
	if spec.HandNo != 1 || spec.TableSet != nil || spec.HandResult != nil {
		t.Fatalf("초기 스냅샷 = handNo%d table=%v result=%v", spec.HandNo, spec.TableSet, spec.HandResult)
	}
	if spec.EndsAt <= 0 {
		t.Fatal("playing 스냅샷의 endsAt 부재")
	}

	// ---- 제출·클라이밍이 스냅샷에 반영된다 ----
	h.handleGameMessage(DMGameMessage{Client: clients[0], Message: DMMessage{
		Type: DMMsgPlay, Payload: DMPlayPayload{Cards: []int{7, 7}}}})
	if game.Table == nil || game.Table.Rank != 7 || game.Table.Count != 2 {
		t.Fatalf("테이블 세트 = %+v", game.Table)
	}
	if !strings.Contains(rawOf(2), `"tableSet":{"rank":7,"count":2,"seat":0}`) {
		t.Fatalf("tableSet 공개 실패:\n%s", rawOf(2))
	}
	// 조커 와일드로 5 두 장 — 7보다 낮으므로 통과
	h.handleGameMessage(DMGameMessage{Client: clients[1], Message: DMMessage{
		Type: DMMsgPlay, Payload: DMPlayPayload{Cards: []int{5, DMJoker}}}})
	if game.Table.Rank != 5 || game.Table.Seat != 1 {
		t.Fatalf("조커 와일드 반영 실패: %+v", game.Table)
	}
	// 못 이기는 제출은 에러 (테이블은 그대로)
	h.handleGameMessage(DMGameMessage{Client: clients[2], Message: DMMessage{
		Type: DMMsgPlay, Payload: DMPlayPayload{Cards: []int{12}}}})
	if game.Table.Rank != 5 || game.CurrentSeat != 2 {
		t.Fatalf("무효 제출이 반영됐다: table=%+v cur=%d", game.Table, game.CurrentSeat)
	}
	h.handleGameMessage(DMGameMessage{Client: clients[2], Message: DMMessage{
		Type: DMMsgPlay, Payload: DMPlayPayload{Cards: []int{3, 3}}}})
	if game.Table.Rank != 3 {
		t.Fatalf("3 두 장 제출 실패: %+v", game.Table)
	}

	// ---- 손을 다 털면 빈 손 — yourHand 는 빈 배열 [] (null 금지) ----
	game.Players[3].Hand = []int{}
	game.Players[3].OutRank = 1
	raw3 := rawOf(3)
	if !strings.Contains(raw3, `"yourHand":[]`) {
		t.Fatalf("빈 손이 []가 아니다:\n%s", raw3)
	}
	if strings.Contains(raw3, `"yourHand":null`) {
		t.Fatalf("빈 손이 null 이다:\n%s", raw3)
	}
	if !strings.Contains(raw3, `"out":true`) || !strings.Contains(raw3, `"rank":1`) {
		t.Fatalf("순위 확정 표기 부재:\n%s", raw3)
	}
	// 좌석 0·점수 0 은 omitempty 없이 항상 실린다
	if !strings.Contains(raw3, `"seat":0`) || !strings.Contains(raw3, `"points":0`) {
		t.Fatalf("좌석 0·점수 0 유실:\n%s", raw3)
	}
}

// TestDMAfkAutoProgress 접속만 유지한 채 아무것도 하지 않는 4인전 —
// 자동 최소 플레이(없으면 패스)와 핸드 자동 전환만으로 3핸드를 완주하는지
// (endsAt 노출·afk 이벤트 포함).
func TestDMAfkAutoProgress(t *testing.T) {
	_, url, cleanup := newDMTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := dmDial(t, url)
	defer host.conn.Close()
	dmJoin(t, host, "잠수1", "")

	for i := 2; i <= DMMinPlayers; i++ {
		guest := dmDial(t, url)
		defer guest.conn.Close()
		dmJoin(t, guest, fmt.Sprintf("잠수%d", i), "")
		dmDrain(guest) // 더 읽지 않는다 — 백그라운드로 비워 버퍼 포화만 막는다
	}

	host.send(t, DMMessage{Type: DMMsgStart})
	state := host.dmWaitPhase(t, string(DMPhasePlaying))
	if ends := int64(state["endsAt"].(float64)); ends <= 0 {
		t.Fatalf("playing 스냅샷의 endsAt = %d, want unixMillis", ends)
	}

	// 전원 방치 — 자동 플레이·패스와 핸드 자동 전환으로 완주해야 한다
	sawAfk, sawHandEnd := false, false
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		msg := host.waitMatch(t, "event-or-over", func(m DMMessage) bool {
			return m.Type == DMMsgEvent || m.Type == DMMsgGameOver
		})
		if msg.Type == DMMsgEvent {
			ev := dmPayloadMap(t, msg)
			switch ev["kind"] {
			case "afk":
				if !strings.Contains(ev["message"].(string), "자동") {
					t.Fatalf("afk 문구 = %v", ev["message"])
				}
				sawAfk = true
			case "hand_end":
				if !strings.Contains(ev["message"].(string), "순위") {
					t.Fatalf("hand_end 문구 = %v", ev["message"])
				}
				sawHandEnd = true
			}
			continue
		}
		over := dmPayloadMap(t, msg)
		if seats, _ := over["winnerSeats"].([]interface{}); len(seats) == 0 {
			t.Fatalf("winnerSeats = %v", over["winnerSeats"])
		}
		if !sawAfk {
			t.Fatal("afk 자동 진행 이벤트가 한 번도 없었다")
		}
		if !sawHandEnd {
			t.Fatal("핸드 정산 이벤트가 한 번도 없었다")
		}
		return
	}
	t.Fatal("전원 방치 게임이 45초 안에 끝나지 않았다")
}

// TestDMRoomCodeAndSpectate 방 코드 발급·코드 입장·시작 후 코드 관전 진입.
// 관전자 스냅샷은 yourSeat -1 에 yourHand 부재. 행동은 전부 차단된다.
func TestDMRoomCodeAndSpectate(t *testing.T) {
	_, url, cleanup := newDMTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := dmDial(t, url)
	defer host.conn.Close()
	joined := dmJoin(t, host, "호스트", "NEW")
	code, _ := joined["roomCode"].(string)
	if len(code) != roomCodeLen {
		t.Fatalf("roomCode = %q, want %d자", code, roomCodeLen)
	}

	for i := 2; i <= DMMinPlayers; i++ {
		guest := dmDial(t, url)
		defer guest.conn.Close()
		guestJoined := dmJoin(t, guest, fmt.Sprintf("친구%d", i), code)
		if guestJoined["roomCode"] != code || int(guestJoined["yourSeat"].(float64)) != i-1 {
			t.Fatalf("코드 입장 실패: %v", guestJoined)
		}
		dmDrain(guest)
	}

	host.send(t, DMMessage{Type: DMMsgStart})
	state := host.dmWaitPhase(t, string(DMPhasePlaying))
	if state["roomCode"] != code || len(state["players"].([]interface{})) != DMMinPlayers {
		t.Fatalf("시작 실패: %v", state["players"])
	}
	if int(state["handNo"].(float64)) != 1 {
		t.Fatalf("handNo = %v, want 1", state["handNo"])
	}
	hand, ok := state["yourHand"].([]interface{})
	if !ok || len(hand) != 80/DMMinPlayers {
		t.Fatalf("본인 손패 = %v, want %d장", state["yourHand"], 80/DMMinPlayers)
	}

	// 시작된 방의 코드로 들어오면 관전자
	spec := dmDial(t, url)
	defer spec.conn.Close()
	spec.send(t, DMMessage{Type: DMMsgJoinGame, Payload: DMJoinGamePayload{Name: "구경꾼", Room: code}})
	specJoined := dmPayloadMap(t, spec.waitFor(t, DMMsgSpectateJoined))
	if specJoined["roomCode"] != code {
		t.Fatalf("관전 입장 roomCode = %v", specJoined["roomCode"])
	}

	specState := dmPayloadMap(t, spec.waitFor(t, DMMsgGameState))
	if int(specState["yourSeat"].(float64)) != -1 {
		t.Fatalf("관전자 yourSeat = %v, want -1", specState["yourSeat"])
	}
	if leaked, ok := specState["yourHand"]; ok {
		t.Fatalf("관전자에게 손패 유출: %v", leaked)
	}
	if int(specState["spectators"].(float64)) != 1 {
		t.Fatalf("관전자 수 = %v", specState["spectators"])
	}
	for _, pRaw := range specState["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		if _, ok := p["handCount"]; !ok {
			t.Fatalf("handCount 부재: %v", p)
		}
		if _, ok := p["points"]; !ok {
			t.Fatalf("points 부재: %v", p)
		}
	}

	// 관전자는 어떤 행동도 못 한다
	spec.send(t, DMMessage{Type: DMMsgPass})
	errPayload := dmPayloadMap(t, spec.waitFor(t, DMMsgError))
	if errPayload["message"] != spectatorDeniedMsg {
		t.Fatalf("관전자 차단 문구 = %v", errPayload["message"])
	}
}

// TestDMReconnect 진행 중 이탈 → 재접속 3종 신호와 좌석 복원 (봇 대체 전).
// 유예를 짧게 줘 만료 시 봇 대체까지 이어지는지도 함께 본다.
func TestDMReconnect(t *testing.T) {
	_, url, cleanup := newDMTestServer(t, 300*time.Millisecond)
	defer cleanup()

	host := dmDial(t, url)
	defer host.conn.Close()
	dmJoin(t, host, "호스트", "")

	guests := []*dmTestClient{}
	sessions := []string{}
	for i := 2; i <= DMMinPlayers; i++ {
		g := dmDial(t, url)
		joined := dmJoin(t, g, fmt.Sprintf("친구%d", i), "") // 같은 공용 로비
		guests = append(guests, g)
		sessions = append(sessions, joined["sessionId"].(string))
	}
	host.send(t, DMMessage{Type: DMMsgStart})
	host.dmWaitPhase(t, string(DMPhasePlaying))
	for _, g := range guests[1:] {
		dmDrain(g)
	}

	// 첫 손님이 끊긴다 → 전원에게 dm_player_disconnected
	victim, victimSession := guests[0], sessions[0]
	victim.conn.Close()
	dc := dmPayloadMap(t, host.waitFor(t, DMMsgPlayerDisconnected))
	if int(dc["seat"].(float64)) != 1 || dc["name"] != "친구2" {
		t.Fatalf("연결 끊김 알림 = %v", dc)
	}

	// 유예 안에 재접속 → dm_player_reconnected + 손패 복원
	back := dmDial(t, url)
	defer back.conn.Close()
	back.send(t, DMMessage{Type: DMMsgRejoin, Payload: DMRejoinPayload{SessionID: victimSession}})
	rc := dmPayloadMap(t, host.waitFor(t, DMMsgPlayerReconnected))
	if int(rc["seat"].(float64)) != 1 {
		t.Fatalf("재접속 알림 = %v", rc)
	}
	restored := dmPayloadMap(t, back.waitFor(t, DMMsgGameState))
	if int(restored["yourSeat"].(float64)) != 1 {
		t.Fatalf("복원 yourSeat = %v", restored["yourSeat"])
	}
	if _, ok := restored["yourHand"].([]interface{}); !ok {
		t.Fatalf("복원 스냅샷에 손패 부재: %v", restored)
	}

	// 없는 세션으로 재접속하면 dm_session_expired
	ghost := dmDial(t, url)
	defer ghost.conn.Close()
	ghost.send(t, DMMessage{Type: DMMsgRejoin, Payload: DMRejoinPayload{SessionID: "없는세션"}})
	ghost.waitFor(t, DMMsgSessionExpired)

	// 유예 만료 → 봇 대체 이벤트 (게임은 계속된다)
	back.conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msg := host.waitMatch(t, "bot_takeover", func(m DMMessage) bool {
			return m.Type == DMMsgEvent || m.Type == DMMsgGameOver
		})
		if msg.Type == DMMsgGameOver {
			return // 그 사이 끝났으면 봇 대체를 볼 필요가 없다
		}
		ev := dmPayloadMap(t, msg)
		if ev["kind"] == "bot_takeover" {
			if int(ev["seat"].(float64)) != 1 {
				t.Fatalf("봇 대체 좌석 = %v", ev["seat"])
			}
			return
		}
	}
	t.Fatal("유예 만료 후 봇 대체가 일어나지 않았다")
}
