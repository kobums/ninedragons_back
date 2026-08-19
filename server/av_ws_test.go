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
	// 단계 타임아웃을 짧게 — 봇 완주 테스트는 이보다 훨씬 빨리 제출하므로
	// 정상 경로에는 영향이 없고, AFK 전용 테스트가 이 값에 기대어 돈다
	avPickTimeout = 600 * time.Millisecond
	avTeamVoteTimeout = 600 * time.Millisecond
	avQuestTimeout = 600 * time.Millisecond
	avAssassinTimeout = 600 * time.Millisecond
}

// avTestClient 공용 testConn 에 게임 메시지 타입의 waitFor 를 얹은 래퍼
type avTestClient struct {
	testConn[AVMessage]
}

func newAVTestServer(t *testing.T, grace time.Duration) (*AVHub, string, func()) {
	t.Helper()
	hub := NewAVHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeAVWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func avDial(t *testing.T, url string) *avTestClient {
	t.Helper()
	return &avTestClient{dialWS[AVMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *avTestClient) waitFor(t *testing.T, msgType AVMessageType) AVMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m AVMessage) bool { return m.Type == msgType })
}

// waitState 조건을 만족하는 av_game_state 가 올 때까지 읽는다
func (c *avTestClient) waitState(t *testing.T, name string, match func(map[string]interface{}) bool) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, name, func(m AVMessage) bool {
		if m.Type != AVMsgGameState {
			return false
		}
		p, ok := m.Payload.(map[string]interface{})
		return ok && match(p)
	})
	return asPayloadMap(t, msg.Payload)
}

// waitPhase 지정한 단계의 av_game_state 가 올 때까지 읽는다
func (c *avTestClient) waitPhase(t *testing.T, phase AVPhase) map[string]interface{} {
	t.Helper()
	return c.waitState(t, "state:"+string(phase), func(p map[string]interface{}) bool {
		return p["phase"] == string(phase)
	})
}

// waitPhaseRound 지정한 (단계, 라운드)의 스냅샷이 올 때까지 읽는다
func (c *avTestClient) waitPhaseRound(t *testing.T, phase AVPhase, round int) map[string]interface{} {
	t.Helper()
	name := fmt.Sprintf("state:%s r%d", phase, round)
	return c.waitState(t, name, func(p map[string]interface{}) bool {
		return p["phase"] == string(phase) && p["round"] == float64(round)
	})
}

// avJoinRoom 입장 → av_player_joined 의 (seat, sessionId, roomCode, gameId)
func avJoinRoom(t *testing.T, c *avTestClient, name, room string) (int, string, string, string) {
	t.Helper()
	c.send(t, AVMessage{Type: AVMsgJoinGame, Payload: AVJoinGamePayload{Name: name, Room: room}})
	joined := asPayloadMap(t, c.waitFor(t, AVMsgPlayerJoined).Payload)
	return int(joined["yourSeat"].(float64)), joined["sessionId"].(string),
		joined["roomCode"].(string), joined["gameId"].(string)
}

// avJoin 공용 로비 입장
func avJoin(t *testing.T, c *avTestClient, name string) (int, string) {
	t.Helper()
	seat, session, _, _ := avJoinRoom(t, c, name, "")
	return seat, session
}

// nextMessage waitMatch 의 큐 소비를 testing.T 없이 수행한다 — 완주 테스트의
// 병렬 드라이버 고루틴은 t.Fatal 을 쓸 수 없어 에러를 돌려준다.
func (c *avTestClient) nextMessage(deadline time.Time) (AVMessage, error) {
	for len(c.queue) == 0 {
		if !time.Now().Before(deadline) {
			return AVMessage{}, fmt.Errorf("메시지 대기 시간 초과")
		}
		c.conn.SetReadDeadline(deadline)
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return AVMessage{}, err
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var msg AVMessage
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				return AVMessage{}, err
			}
			c.queue = append(c.queue, msg)
		}
	}
	msg := c.queue[0]
	c.queue = c.queue[1:]
	return msg, nil
}

type avDriveResult struct {
	over AVMessage
	err  error
}

// driveAV 소켓 하나를 봇 두뇌로 몰아 av_game_over 까지 진행한다
func driveAV(c *avTestClient, brain *avBrain, deadline time.Time) avDriveResult {
	for {
		msg, err := c.nextMessage(deadline)
		if err != nil {
			return avDriveResult{err: err}
		}
		if msg.Type == AVMsgGameOver {
			return avDriveResult{over: msg}
		}
		if reply := brain.decide(msg); reply != nil {
			if err := c.conn.WriteJSON(*reply); err != nil {
				return avDriveResult{err: err}
			}
		}
	}
}

// avCompleteGame n 좌석 전부 봇 두뇌로 구동해 60초 안에 완주한다.
// 부결 5회 룰 + 원정 5라운드 상한 + 암살 단계로 유한 종료가 보장된다.
func avCompleteGame(t *testing.T, n int) {
	t.Helper()
	_, url, cleanup := newAVTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients := make([]*avTestClient, n)
	for i := range clients {
		clients[i] = avDial(t, url)
		defer clients[i].conn.Close()
		seat, _ := avJoin(t, clients[i], fmt.Sprintf("드라이버%d", i+1))
		if seat != i {
			t.Fatalf("좌석 = %d, want %d", seat, i)
		}
	}
	clients[0].send(t, AVMessage{Type: AVMsgStart, Payload: map[string]interface{}{}})

	deadline := time.Now().Add(60 * time.Second)
	results := make(chan avDriveResult, len(clients))
	for _, c := range clients {
		go func(c *avTestClient) { results <- driveAV(c, newAVBrain(), deadline) }(c)
	}

	for i := 0; i < len(clients); i++ {
		res := <-results
		if res.err != nil {
			t.Fatalf("완주 실패: %v", res.err)
		}
		over := asPayloadMap(t, res.over.Payload)
		winner := over["winner"]
		if winner != "good" && winner != "evil" {
			t.Fatalf("승자 이상: %v", winner)
		}
		if over["reason"] == "" {
			t.Fatalf("종료 사유 없음: %v", over)
		}
		players := over["players"].([]interface{})
		if len(players) != n {
			t.Fatalf("종료 발표 인원 = %d, want %d", len(players), n)
		}
		for _, p := range players {
			pm := p.(map[string]interface{})
			if pm["role"] == "" {
				t.Fatalf("종료 후 역할 미공개: %v", pm)
			}
		}
	}
}

// TestAVFiveBotsCompleteGame 최소 인원(5)의 완주 — 실패 2장 규칙 없는 판
func TestAVFiveBotsCompleteGame(t *testing.T) {
	avCompleteGame(t, AVMinPlayers)
}

// TestAVTenBotsCompleteGame 정원(10)의 완주 — 악 4인·4라운드 실패 2장 규칙 판
func TestAVTenBotsCompleteGame(t *testing.T) {
	avCompleteGame(t, AVMaxPlayers)
}

// TestAVHiddenState 은닉 검증 — evilSeats 는 악 진영 + 멀린에게만 (그 외 필드
// 부재), 타인 role 종료 전 빈 문자열, 팀 투표는 전원 제출 후 일괄 공개,
// 선의 실패 카드는 에러, 원정 결과는 집계만 공개.
func TestAVHiddenState(t *testing.T) {
	_, url, cleanup := newAVTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	n := AVMinPlayers
	clients := make([]*avTestClient, n)
	for i := range clients {
		clients[i] = avDial(t, url)
		defer clients[i].conn.Close()
		seat, _ := avJoin(t, clients[i], fmt.Sprintf("사람%d", i+1))
		if seat != i {
			t.Fatalf("좌석 = %d, want %d", seat, i)
		}
	}
	clients[0].send(t, AVMessage{Type: AVMsgStart, Payload: map[string]interface{}{}})

	// ---- 지명 스냅샷: 역할·은닉 확인 ----
	roles := make([]string, n)
	leader := -1
	var evilSeatsSeen []int
	for i, c := range clients {
		state := c.waitPhase(t, AVPhaseTeamPick)
		role, _ := state["yourRole"].(string)
		if role == "" {
			t.Fatalf("seat%d yourRole 비어 있음", i)
		}
		roles[i] = role

		if role == string(AVRoleMerlin) || role == string(AVRoleAssassin) || role == string(AVRoleEvil) {
			raw, ok := state["evilSeats"].([]interface{})
			if !ok || len(raw) != 2 {
				t.Fatalf("seat%d(%s) evilSeats 이상: %v", i, role, state["evilSeats"])
			}
			evilSeatsSeen = []int{}
			for _, s := range raw {
				evilSeatsSeen = append(evilSeatsSeen, int(s.(float64)))
			}
		} else if _, leaked := state["evilSeats"]; leaked {
			t.Fatalf("seat%d(%s) 스냅샷에 evilSeats 필드가 있다", i, role)
		}

		for _, p := range state["players"].([]interface{}) {
			pm := p.(map[string]interface{})
			if pm["role"] != "" {
				t.Fatalf("종료 전 role 노출: %v", pm)
			}
		}
		if state["endsAt"].(float64) <= 0 {
			t.Fatalf("seat%d team_pick endsAt = %v, want >0", i, state["endsAt"])
		}
		if state["round"].(float64) != 1 || state["rejectCount"].(float64) != 0 {
			t.Fatalf("seat%d 시작 상태 이상: %v", i, state)
		}
		sizes := state["questSizes"].([]interface{})
		wantSizes := []float64{2, 3, 2, 3, 3}
		for j, s := range sizes {
			if s.(float64) != wantSizes[j] {
				t.Fatalf("questSizes = %v, want %v", sizes, wantSizes)
			}
		}
		if state["teamVotes"] != nil || state["lastQuest"] != nil {
			t.Fatalf("시작 스냅샷에 teamVotes/lastQuest 가 있다: %v", state)
		}
		if state["assassinTarget"].(float64) != -1 || state["winner"] != "" {
			t.Fatalf("시작 스냅샷 종료 필드 이상: %v", state)
		}
		leader = int(state["leaderSeat"].(float64))
	}

	// 배정표: 5인 = 멀린1·암살자1·악1·선2
	counts := map[string]int{}
	for _, r := range roles {
		counts[r]++
	}
	if counts["merlin"] != 1 || counts["assassin"] != 1 || counts["evil"] != 1 || counts["good"] != 2 {
		t.Fatalf("5인 배정 이상: %v", counts)
	}
	// 악 명단(evilSeats)은 실제 악 진영 좌석과 일치해야 한다
	for _, s := range evilSeatsSeen {
		if roles[s] != "assassin" && roles[s] != "evil" {
			t.Fatalf("evilSeats 에 선 좌석 %d(%s) 이 있다", s, roles[s])
		}
	}

	// ---- 지명: 선 진영 2명으로 팀 구성 (리더가 아니어도 됨) ----
	goodSeats := []int{}
	for s, r := range roles {
		if r == "merlin" || r == "good" {
			goodSeats = append(goodSeats, s)
		}
	}
	team := goodSeats[:2]
	clients[leader].send(t, AVMessage{Type: AVMsgPick, Payload: AVPickPayload{Seats: team}})

	// 전원 team_vote 진입 — onTeam 표시 확인
	for i, c := range clients {
		state := c.waitPhase(t, AVPhaseTeamVote)
		for _, p := range state["players"].([]interface{}) {
			pm := p.(map[string]interface{})
			seat := int(pm["seat"].(float64))
			want := seat == team[0] || seat == team[1]
			if pm["onTeam"].(bool) != want {
				t.Fatalf("seat%d 시점 onTeam 이상: %v", i, pm)
			}
		}
	}

	// ---- 팀 투표: 해소 전에는 제출 여부만, 표 내용은 일괄 공개 전 비밀 ----
	clients[team[0]].send(t, AVMessage{Type: AVMsgTeamVote, Payload: AVTeamVotePayload{Approve: true}})
	partial := clients[leader].waitState(t, "부분 투표 스냅샷", func(p map[string]interface{}) bool {
		if p["phase"] != string(AVPhaseTeamVote) {
			return false
		}
		players := p["players"].([]interface{})
		return players[team[0]].(map[string]interface{})["votedTeam"] == true
	})
	if partial["teamVotes"] != nil {
		t.Fatalf("해소 전에 teamVotes 가 공개됐다: %v", partial["teamVotes"])
	}

	for i, c := range clients {
		if i == team[0] {
			continue
		}
		c.send(t, AVMessage{Type: AVMsgTeamVote, Payload: AVTeamVotePayload{Approve: true}})
	}

	// 전원 제출 → quest 진입 + 표 일괄 공개
	quest := clients[leader].waitPhase(t, AVPhaseQuest)
	votes := quest["teamVotes"].([]interface{})
	if len(votes) != n {
		t.Fatalf("일괄 공개 표 수 = %d, want %d", len(votes), n)
	}
	for _, v := range votes {
		vm := v.(map[string]interface{})
		if vm["approve"] != true {
			t.Fatalf("전원 찬성인데 표 이상: %v", vm)
		}
	}

	// ---- 원정: 선의 실패 카드는 에러, 집계만 공개 ----
	// 리더가 원정대원일 수 있다 — 리더 소켓의 quest 스냅샷은 위에서 이미
	// 소비했으므로 다시 기다리면 다음 라운드 quest 까지 밀린다 (이중 소비 금지)
	for _, s := range team {
		if s != leader {
			clients[s].waitPhase(t, AVPhaseQuest)
		}
	}
	clients[team[0]].send(t, AVMessage{Type: AVMsgQuest, Payload: AVQuestPayload{Success: false}})
	errMsg := asPayloadMap(t, clients[team[0]].waitFor(t, AVMsgError).Payload)
	if !strings.Contains(errMsg["message"].(string), "실패 카드") {
		t.Fatalf("선의 실패 카드 에러 문구 이상: %v", errMsg)
	}

	clients[team[0]].send(t, AVMessage{Type: AVMsgQuest, Payload: AVQuestPayload{Success: true}})
	clients[team[1]].send(t, AVMessage{Type: AVMsgQuest, Payload: AVQuestPayload{Success: true}})

	// 라운드 2 지명 — 결과는 집계(lastQuest)만, 개인 카드는 비공개
	next := clients[leader].waitPhaseRound(t, AVPhaseTeamPick, 2)
	results := next["results"].([]interface{})
	if len(results) != 1 || results[0] != "good" {
		t.Fatalf("1라운드 결과 = %v, want [good]", results)
	}
	lastQuest := next["lastQuest"].(map[string]interface{})
	if lastQuest["fails"].(float64) != 0 || lastQuest["size"].(float64) != 2 {
		t.Fatalf("lastQuest = %v, want {fails:0,size:2}", lastQuest)
	}
}

// TestAVRoomCode 사설 방 — "NEW"로 코드 발급, 코드 입장은 같은 방, 공용
// 로비와 분리
func TestAVRoomCode(t *testing.T) {
	_, url, cleanup := newAVTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := avDial(t, url)
	defer host.conn.Close()
	hostSeat, _, code, hostGame := avJoinRoom(t, host, "방장", "NEW")
	if hostSeat != 0 {
		t.Fatalf("방장 좌석 = %d, want 0", hostSeat)
	}
	if len(code) != roomCodeLen {
		t.Fatalf("발급 코드 = %q, want %d자", code, roomCodeLen)
	}

	// 소문자 입력도 같은 방 (정규화)
	friend := avDial(t, url)
	defer friend.conn.Close()
	friendSeat, _, friendCode, friendGame := avJoinRoom(t, friend, "친구", strings.ToLower(code))
	if friendSeat != 1 || friendCode != code || friendGame != hostGame {
		t.Fatalf("친구 입장 이상: seat=%d code=%q game=%q (want 1, %q, %q)",
			friendSeat, friendCode, friendGame, code, hostGame)
	}

	// 공용 로비는 별도의 방
	pub := avDial(t, url)
	defer pub.conn.Close()
	pubSeat, _, pubCode, pubGame := avJoinRoom(t, pub, "일반", "")
	if pubSeat != 0 || pubCode != "" || pubGame == hostGame {
		t.Fatalf("공용 로비 분리 실패: seat=%d code=%q game=%q", pubSeat, pubCode, pubGame)
	}
}

// TestAVRejoinRestore 봇 채우기(5인 상한)·재접속 스냅샷 복원·복원 시 은닉 유지
func TestAVRejoinRestore(t *testing.T) {
	_, url, cleanup := newAVTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := avDial(t, url)
	defer host.conn.Close()
	guest := avDial(t, url)

	avJoin(t, host, "호스트")
	guestSeat, guestSession := avJoin(t, guest, "손님")
	if guestSeat != 1 {
		t.Fatalf("손님 좌석 = %d, want 1", guestSeat)
	}

	host.send(t, AVMessage{Type: AVMsgFillBots, Payload: map[string]interface{}{}})
	host.send(t, AVMessage{Type: AVMsgStart, Payload: map[string]interface{}{}})

	// 봇 채우기는 5인까지만 — 좌석 2~4 가 봇
	state := host.waitPhase(t, AVPhaseTeamPick)
	players := state["players"].([]interface{})
	if len(players) != AVBotFillTarget {
		t.Fatalf("인원 = %d, want %d (봇은 5인까지만 채운다)", len(players), AVBotFillTarget)
	}
	for i := 2; i < AVBotFillTarget; i++ {
		pm := players[i].(map[string]interface{})
		if pm["bot"] != true {
			t.Fatalf("seat%d 봇 아님: %v", i, pm)
		}
	}
	guest.waitPhase(t, AVPhaseTeamPick)

	// 손님 이탈 → 통지 → 재접속 → 복원
	guest.conn.Close()
	disc := asPayloadMap(t, host.waitFor(t, AVMsgOpponentDisconnected).Payload)
	if int(disc["seat"].(float64)) != 1 {
		t.Fatalf("끊김 좌석 = %v", disc["seat"])
	}

	rejoined := avDial(t, url)
	defer rejoined.conn.Close()
	rejoined.send(t, AVMessage{Type: AVMsgRejoin, Payload: AVRejoinPayload{SessionID: guestSession}})
	recon := asPayloadMap(t, host.waitFor(t, AVMsgReconnected).Payload)
	if int(recon["seat"].(float64)) != 1 {
		t.Fatalf("재접속 좌석 = %v", recon["seat"])
	}

	restored := asPayloadMap(t, rejoined.waitFor(t, AVMsgGameState).Payload)
	if int(restored["yourSeat"].(float64)) != 1 {
		t.Fatalf("복원 좌석 = %v", restored["yourSeat"])
	}
	role, _ := restored["yourRole"].(string)
	if role == "" {
		t.Fatal("복원 스냅샷에 yourRole 이 없다")
	}
	// 복원 경로에서도 은닉 유지 — 선 일반에게는 evilSeats 필드 자체가 없다
	if role == string(AVRoleGood) {
		if _, leaked := restored["evilSeats"]; leaked {
			t.Fatalf("선 일반 복원 스냅샷에 evilSeats 필드가 있다 (%s)", role)
		}
	} else {
		raw, ok := restored["evilSeats"].([]interface{})
		if !ok || len(raw) != 2 {
			t.Fatalf("%s 복원 스냅샷 evilSeats 이상: %v", role, restored["evilSeats"])
		}
	}
}

// TestAVGraceExpiredBotTakeover 유예 만료 좌석은 연습봇으로 대체되고 지명·
// 투표·원정 해소가 막히지 않아 게임이 끝까지 진행된다
func TestAVGraceExpiredBotTakeover(t *testing.T) {
	_, url, cleanup := newAVTestServer(t, 150*time.Millisecond)
	defer cleanup()

	n := AVMinPlayers
	clients := make([]*avTestClient, n)
	for i := range clients {
		clients[i] = avDial(t, url)
		defer clients[i].conn.Close()
		seat, _ := avJoin(t, clients[i], fmt.Sprintf("사람%d", i+1))
		if seat != i {
			t.Fatalf("좌석 = %d, want %d", seat, i)
		}
	}
	clients[0].send(t, AVMessage{Type: AVMsgStart, Payload: map[string]interface{}{}})
	for _, c := range clients {
		c.waitPhase(t, AVPhaseTeamPick)
	}

	leaverSeat := n - 1
	leaverName := fmt.Sprintf("사람%d", leaverSeat+1)
	clients[leaverSeat].conn.Close()
	checker := 0

	// 검증자·이탈자를 뺀 나머지는 봇 두뇌 고루틴으로 완주시킨다
	deadline := time.Now().Add(30 * time.Second)
	results := make(chan avDriveResult, n)
	drivers := 0
	for i, c := range clients {
		if i == leaverSeat || i == checker {
			continue
		}
		drivers++
		go func(c *avTestClient) { results <- driveAV(c, newAVBrain(), deadline) }(c)
	}

	// 검증자는 직접 진행하면서 bot_takeover 이벤트를 확인한다
	brain := newAVBrain()
	sawTakeover := false
	for time.Now().Before(deadline) {
		msg := clients[checker].waitMatch(t, "게임 진행 메시지", func(AVMessage) bool { return true })
		if msg.Type == AVMsgEvent {
			if p, ok := msg.Payload.(map[string]interface{}); ok && p["kind"] == "bot_takeover" {
				if int(p["seat"].(float64)) != leaverSeat {
					t.Fatalf("봇 대체 좌석 = %v, want %d", p["seat"], leaverSeat)
				}
				sawTakeover = true
			}
		}
		if msg.Type == AVMsgGameState && sawTakeover {
			// 대체 좌석은 이름 유지 + bot 표기
			players := asPayloadMap(t, msg.Payload)["players"].([]interface{})
			pm := players[leaverSeat].(map[string]interface{})
			if pm["bot"] != true || pm["name"] != leaverName {
				t.Fatalf("봇 대체 표기 이상: %v", pm)
			}
		}
		if msg.Type == AVMsgGameOver {
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

// TestAVAfkTimeouts 접속만 유지한 채 아무것도 제출하지 않아도 4단계 타임아웃
// (지명 자동·투표 찬성·원정 성공·암살 무작위)으로 게임이 끝까지 진행된다 —
// AFK 영구 정지 방지의 회귀 장치. 자동 원정은 전부 성공이므로 3라운드 만에
// 선 3승 → 암살 단계 → 무작위 지목으로 종료된다.
func TestAVAfkTimeouts(t *testing.T) {
	_, url, cleanup := newAVTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients := make([]*avTestClient, AVMinPlayers)
	for i := range clients {
		clients[i] = avDial(t, url)
		defer clients[i].conn.Close()
		avJoin(t, clients[i], fmt.Sprintf("잠수%d", i+1))
	}
	clients[0].send(t, AVMessage{Type: AVMsgStart, Payload: map[string]interface{}{}})
	watcher := clients[0]

	// 1라운드 지명 — endsAt(마감)이 실려 있어야 한다
	pick := watcher.waitPhase(t, AVPhaseTeamPick)
	if pick["endsAt"].(float64) <= 0 {
		t.Fatalf("team_pick endsAt = %v, want >0", pick["endsAt"])
	}

	// 리더 무행동 → 자동 지명 → 투표 단계 (endsAt 갱신)
	vote := watcher.waitPhase(t, AVPhaseTeamVote)
	if vote["endsAt"].(float64) <= 0 {
		t.Fatalf("team_vote endsAt = %v, want >0", vote["endsAt"])
	}
	if len(vote["team"].([]interface{})) != 2 {
		t.Fatalf("자동 지명 팀 = %v, want 2명", vote["team"])
	}

	// 전원 무투표 → 전원 찬성 처리 → 원정 단계
	quest := watcher.waitPhase(t, AVPhaseQuest)
	if quest["endsAt"].(float64) <= 0 {
		t.Fatalf("quest endsAt = %v, want >0", quest["endsAt"])
	}

	// 미제출 성공 처리 → 선 1승, 2라운드로
	round2 := watcher.waitPhaseRound(t, AVPhaseTeamPick, 2)
	results := round2["results"].([]interface{})
	if len(results) != 1 || results[0] != "good" {
		t.Fatalf("1라운드 자동 결과 = %v, want [good]", results)
	}

	// 같은 흐름으로 3라운드까지 — 선 3승이면 암살 단계
	watcher.waitPhaseRound(t, AVPhaseTeamPick, 3)
	assassin := watcher.waitPhase(t, AVPhaseAssassin)
	if assassin["endsAt"].(float64) <= 0 {
		t.Fatalf("assassin endsAt = %v, want >0", assassin["endsAt"])
	}
	if got := len(assassin["results"].([]interface{})); got != 3 {
		t.Fatalf("암살 단계 진입 시 결과 수 = %d, want 3", got)
	}

	// 암살자 무행동 → 무작위 선 지목 → 종료 (적중/빗나감 모두 정상)
	over := asPayloadMap(t, watcher.waitFor(t, AVMsgGameOver).Payload)
	winner := over["winner"]
	if winner != "good" && winner != "evil" {
		t.Fatalf("승자 이상: %v", winner)
	}
	target := int(over["assassinTarget"].(float64))
	if target < 0 || target >= AVMinPlayers {
		t.Fatalf("암살 대상 이상: %d", target)
	}
	for _, p := range over["players"].([]interface{}) {
		pm := p.(map[string]interface{})
		if pm["role"] == "" {
			t.Fatalf("종료 후 역할 미공개: %v", pm)
		}
	}
}
