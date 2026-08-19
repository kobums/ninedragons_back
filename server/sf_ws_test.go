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
	// day_result/execution 자동 진행을 짧게 — 완주 테스트가 60초 안에 끝나는 근거
	sfPhaseDelay = 40 * time.Millisecond
	// 밤·투표 타임아웃도 짧게 — 봇 완주 테스트는 이보다 훨씬 빨리 제출하므로
	// 정상 경로에는 영향이 없고, 타임아웃 전용 테스트가 이 값에 기대어 돈다
	sfNightTimeout = 600 * time.Millisecond
	sfVoteTimeout = 600 * time.Millisecond
}

// sfTestClient 공용 testConn 에 게임 메시지 타입의 waitFor 를 얹은 래퍼
type sfTestClient struct {
	testConn[SFMessage]
}

func newSFTestServer(t *testing.T, grace time.Duration) (*SFHub, string, func()) {
	t.Helper()
	hub := NewSFHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeSFWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func sfDial(t *testing.T, url string) *sfTestClient {
	t.Helper()
	return &sfTestClient{dialWS[SFMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *sfTestClient) waitFor(t *testing.T, msgType SFMessageType) SFMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m SFMessage) bool { return m.Type == msgType })
}

// waitPhase 지정한 단계의 sf_game_state 가 올 때까지 읽는다
func (c *sfTestClient) waitPhase(t *testing.T, phase SFPhase) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "state:"+string(phase), func(m SFMessage) bool {
		if m.Type != SFMsgGameState {
			return false
		}
		p, ok := m.Payload.(map[string]interface{})
		return ok && p["phase"] == string(phase)
	})
	return asPayloadMap(t, msg.Payload)
}

// sfJoin 입장 → sf_player_joined 의 (seat, sessionId)
func sfJoin(t *testing.T, c *sfTestClient, name string) (int, string) {
	t.Helper()
	c.send(t, SFMessage{Type: SFMsgJoinGame, Payload: SFJoinGamePayload{Name: name}})
	joined := asPayloadMap(t, c.waitFor(t, SFMsgPlayerJoined).Payload)
	return int(joined["yourSeat"].(float64)), joined["sessionId"].(string)
}

// nextMessage waitMatch 의 큐 소비를 testing.T 없이 수행한다 — 완주 테스트의
// 병렬 드라이버 고루틴은 t.Fatal 을 쓸 수 없어 에러를 돌려준다.
func (c *sfTestClient) nextMessage(deadline time.Time) (SFMessage, error) {
	for len(c.queue) == 0 {
		if !time.Now().Before(deadline) {
			return SFMessage{}, fmt.Errorf("메시지 대기 시간 초과")
		}
		c.conn.SetReadDeadline(deadline)
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return SFMessage{}, err
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var msg SFMessage
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				return SFMessage{}, err
			}
			c.queue = append(c.queue, msg)
		}
	}
	msg := c.queue[0]
	c.queue = c.queue[1:]
	return msg, nil
}

type sfDriveResult struct {
	over SFMessage
	err  error
}

// driveSF 소켓 하나를 봇 두뇌로 몰아 sf_game_over 까지 진행한다
func driveSF(c *sfTestClient, brain *sfBrain, deadline time.Time) sfDriveResult {
	for {
		msg, err := c.nextMessage(deadline)
		if err != nil {
			return sfDriveResult{err: err}
		}
		if msg.Type == SFMsgGameOver {
			return sfDriveResult{over: msg}
		}
		if reply := brain.decide(msg); reply != nil {
			if err := c.conn.WriteJSON(*reply); err != nil {
				return sfDriveResult{err: err}
			}
		}
	}
}

// sfIndexOfRole roles 에서 해당 역할의 첫 좌석
func sfIndexOfRole(roles []string, role string) int {
	for i, r := range roles {
		if r == role {
			return i
		}
	}
	return -1
}

// TestSFTenBotsCompleteGame 10좌석 전부 봇 두뇌로 구동해 60초 안에 완주한다.
// 동률 무처형이 반복돼도 밤 살해가 인원을 줄여 유한 종료 — -count=3 으로
// 셔플·투표 무작위성이 달라져도 회귀가 없음을 본다.
func TestSFTenBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newSFTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients := make([]*sfTestClient, SFMaxPlayers)
	for i := range clients {
		clients[i] = sfDial(t, url)
		defer clients[i].conn.Close()
		seat, _ := sfJoin(t, clients[i], fmt.Sprintf("드라이버%d", i+1))
		if seat != i {
			t.Fatalf("좌석 = %d, want %d", seat, i)
		}
	}
	clients[0].send(t, SFMessage{Type: SFMsgStart, Payload: map[string]interface{}{}})

	deadline := time.Now().Add(60 * time.Second)
	results := make(chan sfDriveResult, len(clients))
	for _, c := range clients {
		go func(c *sfTestClient) { results <- driveSF(c, newSFBrain(), deadline) }(c)
	}

	for i := 0; i < len(clients); i++ {
		res := <-results
		if res.err != nil {
			t.Fatalf("완주 실패: %v", res.err)
		}
		over := asPayloadMap(t, res.over.Payload)
		winner := over["winner"]
		if winner != "mafia" && winner != "citizen" {
			t.Fatalf("승자 이상: %v", winner)
		}
		players := over["players"].([]interface{})
		if len(players) != SFMaxPlayers {
			t.Fatalf("종료 발표 인원 = %d", len(players))
		}
		for _, p := range players {
			pm := p.(map[string]interface{})
			if pm["role"] == "" {
				t.Fatalf("종료 후 역할 미공개: %v", pm)
			}
		}
	}
}

// TestSFHiddenState 은닉 검증 — 비마피아 스냅샷에 mafiaSeats 필드 부재,
// 타인 role 공개 전 빈 문자열, 시민 밤 행동 에러, 경찰 조사 결과는 경찰에게만
func TestSFHiddenState(t *testing.T) {
	_, url, cleanup := newSFTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	n := SFMinPlayers
	clients := make([]*sfTestClient, n)
	for i := range clients {
		clients[i] = sfDial(t, url)
		defer clients[i].conn.Close()
		seat, _ := sfJoin(t, clients[i], fmt.Sprintf("사람%d", i+1))
		if seat != i {
			t.Fatalf("좌석 = %d, want %d", seat, i)
		}
	}
	clients[0].send(t, SFMessage{Type: SFMsgStart, Payload: map[string]interface{}{}})

	// ---- 밤 스냅샷: 역할·은닉 확인 ----
	roles := make([]string, n)
	for i, c := range clients {
		state := c.waitPhase(t, SFPhaseNight)
		role, _ := state["yourRole"].(string)
		if role == "" {
			t.Fatalf("seat%d yourRole 비어 있음", i)
		}
		roles[i] = role

		if role == string(SFRoleMafia) {
			raw, ok := state["mafiaSeats"].([]interface{})
			if !ok || len(raw) != 2 {
				t.Fatalf("seat%d 마피아 mafiaSeats 이상: %v", i, state["mafiaSeats"])
			}
		} else if _, leaked := state["mafiaSeats"]; leaked {
			t.Fatalf("seat%d(%s) 스냅샷에 mafiaSeats 필드가 있다", i, role)
		}

		for _, p := range state["players"].([]interface{}) {
			pm := p.(map[string]interface{})
			if pm["role"] != "" {
				t.Fatalf("공개 전 role 노출: %v", pm)
			}
		}
		if state["investigation"] != nil {
			t.Fatalf("seat%d 조사 전 investigation: %v", i, state["investigation"])
		}
		night, ok := state["night"].(map[string]interface{})
		if !ok || night["yourActionDone"] != false {
			t.Fatalf("seat%d night 뷰 이상: %v", i, state["night"])
		}
	}

	// 배정표: 6인 = 마2·경1·의1·시2
	counts := map[string]int{}
	for _, r := range roles {
		counts[r]++
	}
	if counts["mafia"] != 2 || counts["police"] != 1 || counts["doctor"] != 1 || counts["citizen"] != 2 {
		t.Fatalf("6인 배정 이상: %v", counts)
	}

	// 시민의 밤 행동은 에러
	citizenSeat := sfIndexOfRole(roles, "citizen")
	clients[citizenSeat].send(t, SFMessage{Type: SFMsgNightAction, Payload: SFNightActionPayload{Target: 0}})
	clients[citizenSeat].waitFor(t, SFMsgError)

	// ---- 밤 해소: 마피아 → 최저 비마피아, 경찰 → 자기 아닌 최저, 의사 → 자기 ----
	policeSeat := sfIndexOfRole(roles, "police")
	doctorSeat := sfIndexOfRole(roles, "doctor")
	// 경찰이 그 밤에 살해되면 조사 결과가 전달되지 않는 규칙이 있으므로
	// (사망자는 관전만) 마피아 표적은 경찰이 아닌 비마피아로 고정한다
	mafiaTarget := -1
	for s := 0; s < n; s++ {
		if roles[s] != "mafia" && s != policeSeat {
			mafiaTarget = s
			break
		}
	}
	policeTarget := 0
	if policeSeat == 0 {
		policeTarget = 1
	}
	for s, r := range roles {
		switch r {
		case "mafia":
			clients[s].send(t, SFMessage{Type: SFMsgNightAction, Payload: SFNightActionPayload{Target: mafiaTarget}})
		case "police":
			clients[s].send(t, SFMessage{Type: SFMsgNightAction, Payload: SFNightActionPayload{Target: policeTarget}})
		case "doctor":
			clients[s].send(t, SFMessage{Type: SFMsgNightAction, Payload: SFNightActionPayload{Target: s}})
		}
	}

	// ---- day_result: 발표 공통·조사 결과는 경찰에게만 ----
	saved := mafiaTarget == doctorSeat // 의사가 자기를 치료했으므로
	for i, c := range clients {
		state := c.waitPhase(t, SFPhaseDayResult)
		ann, ok := state["announcement"].(map[string]interface{})
		if !ok {
			t.Fatalf("seat%d 발표 없음", i)
		}
		if saved {
			if ann["kind"] != "saved" || int(ann["seat"].(float64)) != -1 {
				t.Fatalf("세이브 발표 이상 (대상 비공개여야 한다): %v", ann)
			}
		} else if ann["kind"] != "killed" || int(ann["seat"].(float64)) != mafiaTarget {
			t.Fatalf("살해 발표 이상: %v", ann)
		}

		if i == policeSeat {
			inv, ok := state["investigation"].(map[string]interface{})
			if !ok {
				t.Fatalf("경찰에게 investigation 없음: %v", state["investigation"])
			}
			if int(inv["seat"].(float64)) != policeTarget {
				t.Fatalf("조사 대상 = %v, want %d", inv["seat"], policeTarget)
			}
			if inv["isMafia"].(bool) != (roles[policeTarget] == "mafia") {
				t.Fatalf("조사 결과 오답: %v (대상 역할 %s)", inv, roles[policeTarget])
			}
		} else if state["investigation"] != nil {
			t.Fatalf("seat%d 비경찰에게 investigation 노출: %v", i, state["investigation"])
		}

		// 밤 사망자의 역할은 여전히 비공개
		for _, p := range state["players"].([]interface{}) {
			pm := p.(map[string]interface{})
			if pm["role"] != "" {
				t.Fatalf("밤 사망자 역할 노출: %v", pm)
			}
		}
	}
}

// TestSFRejoinRestore 봇 채우기(6인 상한)·재접속 스냅샷 복원·복원 시 은닉 유지
func TestSFRejoinRestore(t *testing.T) {
	_, url, cleanup := newSFTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := sfDial(t, url)
	defer host.conn.Close()
	guest := sfDial(t, url)

	sfJoin(t, host, "호스트")
	guestSeat, guestSession := sfJoin(t, guest, "손님")
	if guestSeat != 1 {
		t.Fatalf("손님 좌석 = %d, want 1", guestSeat)
	}

	host.send(t, SFMessage{Type: SFMsgFillBots, Payload: map[string]interface{}{}})
	host.send(t, SFMessage{Type: SFMsgStart, Payload: map[string]interface{}{}})

	// 봇 채우기는 6인까지만 — 좌석 2~5 가 봇
	state := host.waitPhase(t, SFPhaseNight)
	players := state["players"].([]interface{})
	if len(players) != SFBotFillTarget {
		t.Fatalf("인원 = %d, want %d (봇은 6인까지만 채운다)", len(players), SFBotFillTarget)
	}
	for i := 2; i < SFBotFillTarget; i++ {
		pm := players[i].(map[string]interface{})
		if pm["bot"] != true {
			t.Fatalf("seat%d 봇 아님: %v", i, pm)
		}
	}
	guest.waitPhase(t, SFPhaseNight)

	// 손님 이탈 → 통지 → 재접속 → 복원
	guest.conn.Close()
	disc := asPayloadMap(t, host.waitFor(t, SFMsgOpponentDisconnected).Payload)
	if int(disc["seat"].(float64)) != 1 {
		t.Fatalf("끊김 좌석 = %v", disc["seat"])
	}

	rejoined := sfDial(t, url)
	defer rejoined.conn.Close()
	rejoined.send(t, SFMessage{Type: SFMsgRejoin, Payload: SFRejoinPayload{SessionID: guestSession}})
	recon := asPayloadMap(t, host.waitFor(t, SFMsgReconnected).Payload)
	if int(recon["seat"].(float64)) != 1 {
		t.Fatalf("재접속 좌석 = %v", recon["seat"])
	}

	restored := asPayloadMap(t, rejoined.waitFor(t, SFMsgGameState).Payload)
	if int(restored["yourSeat"].(float64)) != 1 {
		t.Fatalf("복원 좌석 = %v", restored["yourSeat"])
	}
	role, _ := restored["yourRole"].(string)
	if role == "" {
		t.Fatal("복원 스냅샷에 yourRole 이 없다")
	}
	// 복원 경로에서도 은닉 유지
	if role != string(SFRoleMafia) {
		if _, leaked := restored["mafiaSeats"]; leaked {
			t.Fatalf("비마피아 복원 스냅샷에 mafiaSeats 필드가 있다 (%s)", role)
		}
	}
}

// TestSFGraceExpiredBotTakeover 유예 만료 좌석은 연습봇으로 대체되고
// 밤·투표 해소가 막히지 않아 게임이 끝까지 진행된다.
// 이탈자는 마피아 좌석으로 고른다 — 마피아는 밤 해소의 필수 행동자이면서
// 밤 살해 대상이 될 수 없으므로, 봇 대체 전에는 게임이 진행될 수도 끝날
// 수도 없다 (bot_takeover 를 결정적으로 관측하는 근거).
func TestSFGraceExpiredBotTakeover(t *testing.T) {
	_, url, cleanup := newSFTestServer(t, 150*time.Millisecond)
	defer cleanup()

	n := SFMinPlayers
	clients := make([]*sfTestClient, n)
	for i := range clients {
		clients[i] = sfDial(t, url)
		defer clients[i].conn.Close()
		seat, _ := sfJoin(t, clients[i], fmt.Sprintf("사람%d", i+1))
		if seat != i {
			t.Fatalf("좌석 = %d, want %d", seat, i)
		}
	}
	clients[0].send(t, SFMessage{Type: SFMsgStart, Payload: map[string]interface{}{}})

	roles := make([]string, n)
	for i, c := range clients {
		state := c.waitPhase(t, SFPhaseNight)
		roles[i], _ = state["yourRole"].(string)
	}

	leaverSeat := sfIndexOfRole(roles, "mafia")
	leaverName := fmt.Sprintf("사람%d", leaverSeat+1)
	clients[leaverSeat].conn.Close()

	checker := 0
	if leaverSeat == 0 {
		checker = 1
	}

	// 검증자·이탈자를 뺀 나머지는 봇 두뇌 고루틴으로 완주시킨다
	deadline := time.Now().Add(30 * time.Second)
	results := make(chan sfDriveResult, n)
	drivers := 0
	for i, c := range clients {
		if i == leaverSeat || i == checker {
			continue
		}
		drivers++
		go func(c *sfTestClient) { results <- driveSF(c, newSFBrain(), deadline) }(c)
	}

	// 검증자는 직접 진행하면서 bot_takeover 이벤트를 확인한다
	brain := newSFBrain()
	sawTakeover := false
	for time.Now().Before(deadline) {
		msg := clients[checker].waitMatch(t, "게임 진행 메시지", func(SFMessage) bool { return true })
		if msg.Type == SFMsgEvent {
			if p, ok := msg.Payload.(map[string]interface{}); ok && p["kind"] == "bot_takeover" {
				if int(p["seat"].(float64)) != leaverSeat {
					t.Fatalf("봇 대체 좌석 = %v, want %d", p["seat"], leaverSeat)
				}
				sawTakeover = true
			}
		}
		if msg.Type == SFMsgGameState && sawTakeover {
			// 대체 좌석은 이름 유지 + bot 표기
			players := asPayloadMap(t, msg.Payload)["players"].([]interface{})
			pm := players[leaverSeat].(map[string]interface{})
			if pm["bot"] != true || pm["name"] != leaverName {
				t.Fatalf("봇 대체 표기 이상: %v", pm)
			}
		}
		if msg.Type == SFMsgGameOver {
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
			clients[checker].send(t, *reply)
		}
	}
	t.Fatal("30초 안에 game_over 를 받지 못했다")
}

// TestSFAfkTimeouts 접속만 유지한 채 아무것도 제출하지 않아도 밤·투표가
// 타임아웃으로 해소된다 — AFK 영구 정지 방지의 회귀 장치.
// 6인 전원이 사람 드라이버(무행동)라 밤은 무표 해소(조용한 밤), 투표는
// 0표 판정(무처형)으로 흘러 2일차 밤까지 스스로 진행되는지 본다.
func TestSFAfkTimeouts(t *testing.T) {
	_, url, cleanup := newSFTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients := make([]*sfTestClient, SFMinPlayers)
	for i := range clients {
		clients[i] = sfDial(t, url)
		defer clients[i].conn.Close()
		sfJoin(t, clients[i], fmt.Sprintf("잠수%d", i+1))
	}
	clients[0].send(t, SFMessage{Type: SFMsgStart, Payload: map[string]interface{}{}})

	// 1일차 밤 — endsAt(마감)이 실려 있어야 한다
	night := clients[0].waitPhase(t, SFPhaseNight)
	if night["endsAt"].(float64) <= 0 {
		t.Fatalf("night endsAt = %v, want >0", night["endsAt"])
	}

	// 아무도 행동하지 않는다 → 밤 타임아웃 → 무표 해소(아무도 죽지 않음)
	clients[0].waitPhase(t, SFPhaseDayResult)

	// 투표 단계도 endsAt — 아무도 투표하지 않는다 → 무처형
	vote := clients[0].waitPhase(t, SFPhaseDayVote)
	if vote["endsAt"].(float64) <= 0 {
		t.Fatalf("day_vote endsAt = %v, want >0", vote["endsAt"])
	}
	clients[0].waitPhase(t, SFPhaseExecution)

	// 2일차 밤 진입 = 한 사이클이 사람 입력 없이 완주됐다
	night2 := clients[0].waitPhase(t, SFPhaseNight)
	if night2["dayNo"].(float64) != 2 {
		t.Fatalf("dayNo = %v, want 2", night2["dayNo"])
	}
}

// TestSFPendingCountHidesRoles 밤 대기 표시가 역할 목록이 아니라 인원수만
// 노출한다 — 밤 사망자의 비공개 역할 추론(경찰 사망 → 목록에서 사라짐) 차단.
func TestSFPendingCountHidesRoles(t *testing.T) {
	_, url, cleanup := newSFTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients := make([]*sfTestClient, SFMinPlayers)
	for i := range clients {
		clients[i] = sfDial(t, url)
		defer clients[i].conn.Close()
		sfJoin(t, clients[i], fmt.Sprintf("사람%d", i+1))
	}
	clients[0].send(t, SFMessage{Type: SFMsgStart, Payload: map[string]interface{}{}})

	state := clients[0].waitPhase(t, SFPhaseNight)
	nightView, ok := state["night"].(map[string]interface{})
	if !ok {
		t.Fatal("night 뷰가 없다")
	}
	if _, leaked := nightView["pendingRoles"]; leaked {
		t.Fatal("night 뷰에 pendingRoles(역할 목록)가 있다 — 은닉 위반")
	}
	if nightView["pendingCount"].(float64) <= 0 {
		t.Fatalf("pendingCount = %v, want >0", nightView["pendingCount"])
	}
}
