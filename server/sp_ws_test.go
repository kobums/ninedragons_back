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

func init() {
	// playing 타이머 1분을 100ms 로 — 완주 테스트가 수 초 안에 끝나는 근거.
	// (기본 5분 = 500ms. 은닉 테스트의 playing 중 검증도 이 창 안에서 끝난다.)
	spMinute = 100 * time.Millisecond
}

// spTestClient 공용 testConn 에 게임 메시지 타입의 waitFor 를 얹은 래퍼
type spTestClient struct {
	testConn[SPMessage]
}

func newSPTestServer(t *testing.T, grace time.Duration) (*SPHub, string, func()) {
	t.Helper()
	hub := NewSPHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeSPWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func spDial(t *testing.T, url string) *spTestClient {
	t.Helper()
	return &spTestClient{dialWS[SPMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *spTestClient) waitFor(t *testing.T, msgType SPMessageType) SPMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m SPMessage) bool { return m.Type == msgType })
}

// waitPhase 지정한 단계의 sp_game_state 가 올 때까지 읽는다
func (c *spTestClient) waitPhase(t *testing.T, phase SPPhase) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "state:"+string(phase), func(m SPMessage) bool {
		if m.Type != SPMsgGameState {
			return false
		}
		p, ok := m.Payload.(map[string]interface{})
		return ok && p["phase"] == string(phase)
	})
	return asPayloadMap(t, msg.Payload)
}

// spJoin 입장 → sp_player_joined 의 (seat, sessionId)
func spJoin(t *testing.T, c *spTestClient, name string) (int, string) {
	t.Helper()
	c.send(t, SPMessage{Type: SPMsgJoinGame, Payload: SPJoinGamePayload{Name: name}})
	joined := asPayloadMap(t, c.waitFor(t, SPMsgPlayerJoined).Payload)
	return int(joined["yourSeat"].(float64)), joined["sessionId"].(string)
}

// nextMessage waitMatch 의 큐 소비를 testing.T 없이 수행한다 — 완주 테스트의
// 병렬 드라이버 고루틴은 t.Fatal 을 쓸 수 없어 에러를 돌려준다.
func (c *spTestClient) nextMessage(deadline time.Time) (SPMessage, error) {
	for len(c.queue) == 0 {
		if !time.Now().Before(deadline) {
			return SPMessage{}, fmt.Errorf("메시지 대기 시간 초과")
		}
		c.conn.SetReadDeadline(deadline)
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return SPMessage{}, err
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var msg SPMessage
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				return SPMessage{}, err
			}
			c.queue = append(c.queue, msg)
		}
	}
	msg := c.queue[0]
	c.queue = c.queue[1:]
	return msg, nil
}

type spDriveResult struct {
	over SPMessage
	err  error
}

// driveSP 소켓 하나를 봇 두뇌로 몰아 sp_game_over 까지 진행한다
func driveSP(c *spTestClient, brain *spBrain, deadline time.Time) spDriveResult {
	for {
		msg, err := c.nextMessage(deadline)
		if err != nil {
			return spDriveResult{err: err}
		}
		if msg.Type == SPMsgGameOver {
			return spDriveResult{over: msg}
		}
		if reply := brain.decide(msg); reply != nil {
			if err := c.conn.WriteJSON(*reply); err != nil {
				return spDriveResult{err: err}
			}
		}
	}
}

// TestSPEightBotsCompleteGame 8좌석 전부 봇 두뇌로 구동해 완주한다.
// 봇은 playing 을 기다렸다가(추리 안 함) voting 에서 무작위 지목만 하므로
// 타이머 종료 → 전원 투표 → 판정의 결정적 경로로 유한 종료 — -count=3 으로
// 배정·투표 무작위성이 달라져도 회귀가 없음을 본다.
func TestSPEightBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newSPTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients := make([]*spTestClient, SPMaxPlayers)
	for i := range clients {
		clients[i] = spDial(t, url)
		defer clients[i].conn.Close()
		seat, _ := spJoin(t, clients[i], fmt.Sprintf("드라이버%d", i+1))
		if seat != i {
			t.Fatalf("좌석 = %d, want %d", seat, i)
		}
	}
	clients[0].send(t, SPMessage{Type: SPMsgStart, Payload: map[string]interface{}{}})

	deadline := time.Now().Add(60 * time.Second)
	results := make(chan spDriveResult, len(clients))
	for _, c := range clients {
		go func(c *spTestClient) { results <- driveSP(c, newSPBrain(), deadline) }(c)
	}

	for i := 0; i < len(clients); i++ {
		res := <-results
		if res.err != nil {
			t.Fatalf("완주 실패: %v", res.err)
		}
		over := asPayloadMap(t, res.over.Payload)
		winner := over["winner"]
		if winner != "spy" && winner != "citizen" {
			t.Fatalf("승자 이상: %v", winner)
		}
		// 봇은 추리하지 않으므로 판정은 반드시 투표 경로다
		reason := over["reason"]
		if reason != "vote_caught" && reason != "vote_missed" {
			t.Fatalf("사유 이상: %v", reason)
		}
		spySeat := int(over["spySeat"].(float64))
		if spySeat < 0 || spySeat >= SPMaxPlayers {
			t.Fatalf("스파이 좌석 이상: %d", spySeat)
		}
		location, _ := over["location"].(string)
		if !spAnyValidWord(location) {
			t.Fatalf("종료 발표 장소 이상: %q", location)
		}
		players := over["players"].([]interface{})
		if len(players) != SPMaxPlayers {
			t.Fatalf("종료 발표 인원 = %d", len(players))
		}
	}
}

// TestSPHiddenState 은닉 검증 — 비스파이 스냅샷에 스파이 좌석 정보 부재,
// 스파이 location 빈 문자열, 비스파이의 sp_guess 에러, 스파이의 sp_vote 는
// playing 중 에러·voting 중 허용. 투표 검거로 시민 승까지 확인한다.
func TestSPHiddenState(t *testing.T) {
	_, url, cleanup := newSPTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	n := 4
	clients := make([]*spTestClient, n)
	for i := range clients {
		clients[i] = spDial(t, url)
		defer clients[i].conn.Close()
		seat, _ := spJoin(t, clients[i], fmt.Sprintf("사람%d", i+1))
		if seat != i {
			t.Fatalf("좌석 = %d, want %d", seat, i)
		}
	}
	clients[0].send(t, SPMessage{Type: SPMsgStart, Payload: map[string]interface{}{}})

	// ---- playing 스냅샷: 배정·은닉 확인 ----
	spySeat := -1
	theLocation := ""
	for i, c := range clients {
		state := c.waitPhase(t, SPPhasePlaying)

		// 비스파이 스냅샷에 스파이 좌석 정보가 어떤 형태로도 없어야 한다
		if _, leaked := state["spySeat"]; leaked {
			t.Fatalf("seat%d 스냅샷에 spySeat 필드가 있다", i)
		}
		if state["result"] != nil {
			t.Fatalf("seat%d 진행 중 result 노출: %v", i, state["result"])
		}

		locs, ok := state["locations"].([]interface{})
		if !ok || len(locs) != 24 {
			t.Fatalf("seat%d 장소 목록 이상: %v", i, state["locations"])
		}
		if state["endsAt"].(float64) <= 0 {
			t.Fatalf("seat%d playing endsAt 이상: %v", i, state["endsAt"])
		}

		isSpy, _ := state["isSpy"].(bool)
		location, _ := state["location"].(string)
		if isSpy {
			if spySeat >= 0 {
				t.Fatalf("스파이가 2명이다: seat%d, seat%d", spySeat, i)
			}
			spySeat = i
			if location != "" {
				t.Fatalf("스파이 스냅샷에 location 이 있다: %q", location)
			}
		} else {
			if location == "" {
				t.Fatalf("seat%d 비스파이 location 이 비어 있다", i)
			}
			if theLocation == "" {
				theLocation = location
			} else if location != theLocation {
				t.Fatalf("비스파이 장소 불일치: %q vs %q", location, theLocation)
			}
		}
	}
	if spySeat < 0 {
		t.Fatal("스파이가 없다")
	}
	if !spAnyValidWord(theLocation) {
		t.Fatalf("배부 장소가 목록에 없다: %q", theLocation)
	}

	nonSpy := (spySeat + 1) % n

	// 비스파이의 sp_guess 는 에러 (정답 장소를 대도 안 된다)
	clients[nonSpy].send(t, SPMessage{Type: SPMsgGuess, Payload: SPGuessPayload{Location: theLocation}})
	clients[nonSpy].waitFor(t, SPMsgError)

	// 스파이의 sp_vote 는 playing 중에는 에러
	clients[spySeat].send(t, SPMessage{Type: SPMsgVote, Payload: SPVotePayload{Target: nonSpy}})
	clients[spySeat].waitFor(t, SPMsgError)

	// ---- 타이머 종료 → voting ----
	for i, c := range clients {
		state := c.waitPhase(t, SPPhaseVoting)
		// voting 에도 타임아웃 마감 시각이 실린다 (AFK 영구 정지 방지)
		if state["endsAt"].(float64) <= 0 {
			t.Fatalf("seat%d voting endsAt = %v, want >0", i, state["endsAt"])
		}
		if _, leaked := state["spySeat"]; leaked {
			t.Fatalf("seat%d voting 스냅샷에 spySeat 필드가 있다", i)
		}
		location, _ := state["location"].(string)
		if i == spySeat && location != "" {
			t.Fatalf("voting 중 스파이 location 노출: %q", location)
		}
		if i != spySeat && location != theLocation {
			t.Fatalf("voting 중 seat%d location = %q, want %q", i, location, theLocation)
		}
	}

	// voting 중에도 sp_guess 는 에러 (추리는 playing 전용)
	clients[spySeat].send(t, SPMessage{Type: SPMsgGuess, Payload: SPGuessPayload{Location: "병원"}})
	clients[spySeat].waitFor(t, SPMsgError)

	// 전원 투표 — 비스파이 3명은 스파이를, 스파이는 타인을 지목 → 단독 최다
	// 득표자가 스파이이므로 시민 승 (스파이의 sp_vote 허용 검증 포함)
	clients[spySeat].send(t, SPMessage{Type: SPMsgVote, Payload: SPVotePayload{Target: nonSpy}})
	for i, c := range clients {
		if i == spySeat {
			continue
		}
		c.send(t, SPMessage{Type: SPMsgVote, Payload: SPVotePayload{Target: spySeat}})
	}

	for i, c := range clients {
		over := asPayloadMap(t, c.waitFor(t, SPMsgGameOver).Payload)
		if over["winner"] != "citizen" || over["reason"] != "vote_caught" {
			t.Fatalf("seat%d 판정 이상: winner=%v reason=%v", i, over["winner"], over["reason"])
		}
		if int(over["spySeat"].(float64)) != spySeat {
			t.Fatalf("seat%d 종료 발표 스파이 = %v, want %d", i, over["spySeat"], spySeat)
		}
		if over["location"] != theLocation {
			t.Fatalf("seat%d 종료 발표 장소 = %v, want %q", i, over["location"], theLocation)
		}
		if int(over["topSeat"].(float64)) != spySeat {
			t.Fatalf("seat%d topSeat = %v, want %d", i, over["topSeat"], spySeat)
		}
	}
}

// TestSPRejoinRestore 타이머 선택·봇 채우기(3인 상한)·재접속 스냅샷 복원·복원
// 시 은닉 유지
func TestSPRejoinRestore(t *testing.T) {
	_, url, cleanup := newSPTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := spDial(t, url)
	defer host.conn.Close()
	guest := spDial(t, url)

	spJoin(t, host, "호스트")
	guestSeat, guestSession := spJoin(t, guest, "손님")
	if guestSeat != 1 {
		t.Fatalf("손님 좌석 = %d, want 1", guestSeat)
	}

	// host 의 타이머 선택 (8분 — 테스트 단축 스케일로 playing 창을 넓힌다)
	host.send(t, SPMessage{Type: SPMsgSetTimer, Payload: SPSetTimerPayload{Minutes: 8}})
	host.waitMatch(t, "timerMinutes=8", func(m SPMessage) bool {
		if m.Type != SPMsgGameState {
			return false
		}
		p, ok := m.Payload.(map[string]interface{})
		return ok && p["timerMinutes"] == float64(8)
	})

	host.send(t, SPMessage{Type: SPMsgFillBots, Payload: map[string]interface{}{}})
	host.send(t, SPMessage{Type: SPMsgStart, Payload: map[string]interface{}{}})

	// 봇 채우기는 3인까지만 — 좌석 2 가 봇
	state := host.waitPhase(t, SPPhasePlaying)
	players := state["players"].([]interface{})
	if len(players) != SPBotFillTarget {
		t.Fatalf("인원 = %d, want %d (봇은 3인까지만 채운다)", len(players), SPBotFillTarget)
	}
	if players[2].(map[string]interface{})["bot"] != true {
		t.Fatalf("seat2 봇 아님: %v", players[2])
	}
	if state["timerMinutes"] != float64(8) {
		t.Fatalf("시작 후 타이머 = %v, want 8", state["timerMinutes"])
	}
	guest.waitPhase(t, SPPhasePlaying)

	// 손님 이탈 → 통지 → 재접속 → 복원
	guest.conn.Close()
	disc := asPayloadMap(t, host.waitFor(t, SPMsgOpponentDisconnected).Payload)
	if int(disc["seat"].(float64)) != 1 {
		t.Fatalf("끊김 좌석 = %v", disc["seat"])
	}

	rejoined := spDial(t, url)
	defer rejoined.conn.Close()
	rejoined.send(t, SPMessage{Type: SPMsgRejoin, Payload: SPRejoinPayload{SessionID: guestSession}})
	recon := asPayloadMap(t, host.waitFor(t, SPMsgReconnected).Payload)
	if int(recon["seat"].(float64)) != 1 {
		t.Fatalf("재접속 좌석 = %v", recon["seat"])
	}

	restored := asPayloadMap(t, rejoined.waitFor(t, SPMsgGameState).Payload)
	if int(restored["yourSeat"].(float64)) != 1 {
		t.Fatalf("복원 좌석 = %v", restored["yourSeat"])
	}
	locs, ok := restored["locations"].([]interface{})
	if !ok || len(locs) != 24 {
		t.Fatalf("복원 장소 목록 이상: %v", restored["locations"])
	}
	// 복원 경로에서도 은닉 유지
	if _, leaked := restored["spySeat"]; leaked {
		t.Fatal("복원 스냅샷에 spySeat 필드가 있다")
	}
	isSpy, _ := restored["isSpy"].(bool)
	location, _ := restored["location"].(string)
	if isSpy && location != "" {
		t.Fatalf("복원된 스파이 스냅샷에 location 이 있다: %q", location)
	}
	if !isSpy && !spAnyValidWord(location) {
		t.Fatalf("복원된 비스파이 location 이상: %q", location)
	}
}

// TestSPGraceExpiredBotTakeover 유예 만료 좌석은 연습봇으로 대체되고
// (이름·좌석 유지) 투표 해소가 막히지 않아 게임이 끝까지 진행된다.
// 전원 투표가 판정의 전제라, 이탈 좌석의 봇 대체 없이는 게임이 끝날 수 없다
// (bot_takeover 를 결정적으로 관측하는 근거).
func TestSPGraceExpiredBotTakeover(t *testing.T) {
	_, url, cleanup := newSPTestServer(t, 150*time.Millisecond)
	defer cleanup()

	n := SPMinPlayers
	clients := make([]*spTestClient, n)
	for i := range clients {
		clients[i] = spDial(t, url)
		defer clients[i].conn.Close()
		seat, _ := spJoin(t, clients[i], fmt.Sprintf("사람%d", i+1))
		if seat != i {
			t.Fatalf("좌석 = %d, want %d", seat, i)
		}
	}
	clients[0].send(t, SPMessage{Type: SPMsgStart, Payload: map[string]interface{}{}})
	for _, c := range clients {
		c.waitPhase(t, SPPhasePlaying)
	}

	leaverSeat := 2
	leaverName := fmt.Sprintf("사람%d", leaverSeat+1)
	clients[leaverSeat].conn.Close()

	// 검증자·이탈자를 뺀 나머지는 봇 두뇌 고루틴으로 완주시킨다
	deadline := time.Now().Add(30 * time.Second)
	results := make(chan spDriveResult, n)
	drivers := 0
	for i, c := range clients {
		if i == leaverSeat || i == 0 {
			continue
		}
		drivers++
		go func(c *spTestClient) { results <- driveSP(c, newSPBrain(), deadline) }(c)
	}

	// 검증자(seat0)는 직접 진행하면서 bot_takeover 이벤트를 확인한다
	brain := newSPBrain()
	sawTakeover := false
	for time.Now().Before(deadline) {
		msg := clients[0].waitMatch(t, "게임 진행 메시지", func(SPMessage) bool { return true })
		if msg.Type == SPMsgEvent {
			if p, ok := msg.Payload.(map[string]interface{}); ok && p["kind"] == "bot_takeover" {
				if int(p["seat"].(float64)) != leaverSeat {
					t.Fatalf("봇 대체 좌석 = %v, want %d", p["seat"], leaverSeat)
				}
				sawTakeover = true
			}
		}
		if msg.Type == SPMsgGameState && sawTakeover {
			// 대체 좌석은 이름 유지 + bot 표기
			players := asPayloadMap(t, msg.Payload)["players"].([]interface{})
			pm := players[leaverSeat].(map[string]interface{})
			if pm["bot"] != true || pm["name"] != leaverName {
				t.Fatalf("봇 대체 표기 이상: %v", pm)
			}
		}
		if msg.Type == SPMsgGameOver {
			if !sawTakeover {
				t.Fatal("bot_takeover 이벤트 없이 게임이 끝났다")
			}
			for i := 0; i < drivers; i++ {
				if res := <-results; res.err != nil {
					t.Fatalf("드라이버 완주 실패: %v", res.err)
				}
			}
			return
		}
		if reply := brain.decide(msg); reply != nil {
			clients[0].send(t, *reply)
		}
	}
	t.Fatal("30초 안에 game_over 를 받지 못했다")
}

// TestSPVoteTimeout 투표 타임아웃 — 접속만 유지한 채 아무도 투표하지 않아도
// 마감(spVoteTimeoutMinutes×spMinute) 후 제출된 표만으로 판정된다.
// 무표는 단독 최다가 없으므로 미검거 = 스파이 승(vote_missed).
func TestSPVoteTimeout(t *testing.T) {
	_, url, cleanup := newSPTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients := make([]*spTestClient, 3)
	for i := range clients {
		clients[i] = spDial(t, url)
		defer clients[i].conn.Close()
		spJoin(t, clients[i], fmt.Sprintf("잠수%d", i+1))
	}
	clients[0].send(t, SPMessage{Type: SPMsgStart, Payload: map[string]interface{}{}})

	// 타이머 종료 → voting (endsAt 이 투표 마감 시각으로 실린다)
	state := clients[0].waitPhase(t, SPPhaseVoting)
	if state["endsAt"].(float64) <= 0 {
		t.Fatalf("voting endsAt = %v, want >0", state["endsAt"])
	}

	// 아무도 투표하지 않는다 — 타임아웃 판정 대기
	over := clients[0].waitFor(t, SPMsgGameOver)
	payload := over.Payload.(map[string]interface{})
	if payload["winner"].(string) != "spy" {
		t.Fatalf("무투표 타임아웃 승자 = %v, want spy", payload["winner"])
	}
	if payload["reason"].(string) != "vote_missed" {
		t.Fatalf("사유 = %v, want vote_missed", payload["reason"])
	}
	// 나머지 클라이언트도 종료를 받는다
	clients[1].waitFor(t, SPMsgGameOver)
	clients[2].waitFor(t, SPMsgGameOver)
}
