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

// 테스트에서는 단계 마감과 봇의 생각 시간을 짧게 낮춘다
// (질문 5분·토론 2분·투표 45초, 마스터 봇 20~40초는 실사용 값)
func init() {
	idQuestionTimeout = 900 * time.Millisecond
	idDiscussionTimeout = 500 * time.Millisecond
	idVoteTimeout = 400 * time.Millisecond

	idBotCorrectDelay = 30 * time.Millisecond
	idBotCorrectJitterMs = 30
	idBotDiscussDelay = 20 * time.Millisecond
	idBotDiscussJitterMs = 20
	idBotVoteDelay = 10 * time.Millisecond
	idBotVoteJitterMs = 20
}

// idTestClient 공용 testConn 에 인사이더 메시지 타입의 waitFor 를 얹은 래퍼
type idTestClient struct {
	testConn[IDMessage]
}

func newIDTestServer(t *testing.T, grace time.Duration) (*IDHub, string, func()) {
	t.Helper()
	hub := NewIDHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeIDWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func idDial(t *testing.T, url string) *idTestClient {
	t.Helper()
	return &idTestClient{dialWS[IDMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *idTestClient) waitFor(t *testing.T, msgType IDMessageType) IDMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m IDMessage) bool { return m.Type == msgType })
}

func idPayloadMap(t *testing.T, msg IDMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// idJoin 입장하고 id_player_joined payload 를 돌려준다
func idJoin(t *testing.T, c *idTestClient, name, room string) map[string]interface{} {
	t.Helper()
	c.send(t, IDMessage{Type: IDMsgJoinGame, Payload: IDJoinGamePayload{Name: name, Room: room}})
	return idPayloadMap(t, c.waitFor(t, IDMsgPlayerJoined))
}

// idWaitPhase 지정한 phase 의 스냅샷이 올 때까지 소비
func (c *idTestClient) idWaitPhase(t *testing.T, phase string) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "id_game_state("+phase+")", func(m IDMessage) bool {
		if m.Type != IDMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		return ok && state["phase"] == phase
	})
	return idPayloadMap(t, msg)
}

// TestIDFiveBotsCompleteGame 봇을 채운 5인 게임이 완주하는지 — 가장 중요한
// 회귀 장치 (마스터 선언 교착·투표 미개표·종료 판정 감지).
// 좌석 0은 서버 연습봇 두뇌(idBrain)를 WS 로 감싼 드라이버가 잡는다.
func TestIDFiveBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newIDTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := idDial(t, url)
	defer c.conn.Close()
	idJoin(t, c, "감독", "")
	c.send(t, IDMessage{Type: IDMsgFillBots}) // 5인까지 채우고 즉시 시작

	start := time.Now()
	brain := newIDBrain()
	deadline := start.Add(10 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "state-or-over", func(m IDMessage) bool {
			return m.Type == IDMsgGameState || m.Type == IDMsgGameOver
		})
		if msg.Type == IDMsgGameOver {
			over := idPayloadMap(t, msg)
			winner, _ := over["winner"].(string)
			if winner != "citizens" && winner != "insider" && winner != "none" {
				t.Fatalf("winner = %q", winner)
			}
			insider := int(over["insiderSeat"].(float64))
			master := int(over["masterSeat"].(float64))
			if insider < 0 || master < 0 || insider == master {
				t.Fatalf("insiderSeat=%d masterSeat=%d", insider, master)
			}
			if word, _ := over["word"].(string); word == "" {
				t.Fatalf("종료 발표에 제시어 부재: %v", over)
			}
			players := over["players"].([]interface{})
			if len(players) != IDFillBotTarget {
				t.Fatalf("players 길이 = %d, want %d", len(players), IDFillBotTarget)
			}
			roles := map[string]int{}
			for _, pRaw := range players {
				p := pRaw.(map[string]interface{})
				role, _ := p["role"].(string)
				if role == "" {
					t.Fatalf("종료 화면에 역할 미공개: %v", p)
				}
				roles[role]++
			}
			if roles["master"] != 1 || roles["insider"] != 1 {
				t.Fatalf("종료 역할 분포 이상: %v", roles)
			}
			t.Logf("완주: winner=%s insider=seat%d (%.1fs)", winner, insider, time.Since(start).Seconds())
			return
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("10초 안에 게임이 끝나지 않았다 — 진행 불가 상태")
}

// TestIDHiddenState 은닉의 핵심 계약 — 허브 고루틴 없이 핸들러를 직접 불러
// 결정적으로 검증한다. yourRole 은 본인만, word 는 마스터·인사이더만 —
// 시민·관전자의 raw JSON 에는 필드 자체가 없어야 한다. masterSeat 은 전원
// 공개이고, 종료 후에야 전원 역할·제시어가 공개된다.
func TestIDHiddenState(t *testing.T) {
	h := NewIDHub()
	room := h.lobbyRoomFor("")
	clients := make([]*IDClient, 5)
	for i := range clients {
		c := &IDClient{wsClient: newBotWSClient(), Hub: h}
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
	// 결정적 구도: 마스터 0, 인사이더 1, 제시어 고정
	game.MasterSeat, game.InsiderSeat = 0, 1
	for _, p := range game.Players {
		switch p.Seat {
		case 0:
			p.Role = IDRoleMaster
		case 1:
			p.Role = IDRoleInsider
		default:
			p.Role = IDRoleCitizen
		}
	}
	game.Word = "비밀단어"

	rawOf := func(viewer int) string {
		data, err := json.Marshal(h.buildIDState(room, viewer))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(data)
	}

	// ---- 제시어: 마스터·인사이더에게만 (시민·관전자는 필드 부재) ----
	for _, viewer := range []int{0, 1} {
		if !strings.Contains(rawOf(viewer), `"word":"비밀단어"`) {
			t.Fatalf("viewer %d 에게 제시어가 없다:\n%s", viewer, rawOf(viewer))
		}
	}
	for _, viewer := range []int{2, 3, 4, -1} {
		raw := rawOf(viewer)
		if strings.Contains(raw, "word") {
			t.Fatalf("viewer %d 스냅샷에 word 유출:\n%s", viewer, raw)
		}
		if strings.Contains(raw, "비밀단어") {
			t.Fatalf("viewer %d 스냅샷에 제시어 문자열 유출:\n%s", viewer, raw)
		}
	}

	// ---- yourRole: 본인만 (관전자는 필드 부재) ----
	if !strings.Contains(rawOf(1), `"yourRole":"insider"`) {
		t.Fatalf("인사이더 본인에게 역할이 없다:\n%s", rawOf(1))
	}
	if !strings.Contains(rawOf(0), `"yourRole":"master"`) {
		t.Fatalf("마스터 본인에게 역할이 없다:\n%s", rawOf(0))
	}
	if !strings.Contains(rawOf(2), `"yourRole":"citizen"`) {
		t.Fatalf("시민 본인에게 역할이 없다:\n%s", rawOf(2))
	}
	rawSpec := rawOf(-1)
	if strings.Contains(rawSpec, "yourRole") {
		t.Fatalf("관전자 스냅샷에 yourRole 유출:\n%s", rawSpec)
	}
	if strings.Contains(rawSpec, `"insider"`) {
		t.Fatalf("관전자 스냅샷에 인사이더 정체 유출:\n%s", rawSpec)
	}

	// ---- 진행 중 players[].role 은 전원 "" (정체 은닉) ----
	for _, viewer := range []int{0, 1, 2, -1} {
		for _, pv := range h.buildIDState(room, viewer).Players {
			if pv.Role != "" {
				t.Fatalf("viewer %d 에게 seat%d 역할 유출: %q", viewer, pv.Seat, pv.Role)
			}
		}
	}

	// ---- masterSeat 은 전원 공개, 관전자 스냅샷도 패닉 없이 빌드된다 ----
	spec := h.buildIDState(room, -1)
	if spec.YourSeat != -1 || spec.MasterSeat != 0 || spec.CorrectSeat != -1 {
		t.Fatalf("관전자 스냅샷: yourSeat=%d masterSeat=%d correctSeat=%d",
			spec.YourSeat, spec.MasterSeat, spec.CorrectSeat)
	}
	if spec.Result != nil {
		t.Fatalf("진행 중 result = %+v, want null", spec.Result)
	}
	if !strings.Contains(rawSpec, `"masterSeat":0`) || !strings.Contains(rawSpec, `"result":null`) {
		t.Fatalf("관전자 raw 스냅샷 이상:\n%s", rawSpec)
	}
	if len(spec.Players) != 5 {
		t.Fatalf("관전자 players = %d명", len(spec.Players))
	}

	// ---- 빈 대기실 스냅샷도 빈 배열 [] (null 금지) ----
	empty := h.lobbyRoomFor("ZZZZ")
	if raw, _ := json.Marshal(h.buildIDState(empty, -1)); !strings.Contains(string(raw), `"players":[]`) {
		t.Fatalf("빈 슬라이스가 []가 아니다:\n%s", raw)
	}

	// ---- 마스터 [정답 나옴] → 토론 → 투표 ----
	h.handleGameMessage(IDGameMessage{Client: clients[2], Message: IDMessage{Type: IDMsgCorrect}})
	if game.Phase != IDPhaseQuestion {
		t.Fatalf("시민의 정답 선언이 통과했다: %s", game.Phase)
	}
	h.handleGameMessage(IDGameMessage{Client: clients[0], Message: IDMessage{Type: IDMsgCorrect}})
	if game.Phase != IDPhaseDiscussion {
		t.Fatalf("phase = %s, want discussion", game.Phase)
	}
	h.handleGameMessage(IDGameMessage{Client: clients[0], Message: IDMessage{Type: IDMsgOpenVote}})
	if game.Phase != IDPhaseVoting {
		t.Fatalf("phase = %s, want voting", game.Phase)
	}
	if h.buildIDState(room, 2).EndsAt <= 0 {
		t.Fatal("투표 단계 스냅샷의 endsAt 부재")
	}

	// ---- 투표 대상은 개표 전까지 비공개 (voted 표기만) ----
	h.handleGameMessage(IDGameMessage{Client: clients[2], Message: IDMessage{
		Type: IDMsgVote, Payload: IDVotePayload{Seat: 1}}})
	votingRaw := rawOf(3)
	if !strings.Contains(votingRaw, `"voted":true`) {
		t.Fatalf("투표 표기 부재:\n%s", votingRaw)
	}
	if strings.Contains(votingRaw, `"votes":1`) {
		t.Fatalf("개표 전 득표 유출:\n%s", votingRaw)
	}

	// ---- 나머지 전원 투표 → 개표·종료: 역할·제시어 전원 공개 ----
	for _, seat := range []int{0, 1, 3, 4} {
		target := 1
		if seat == 1 {
			target = 2
		}
		h.handleGameMessage(IDGameMessage{Client: clients[seat], Message: IDMessage{
			Type: IDMsgVote, Payload: IDVotePayload{Seat: target}}})
	}
	if game.Phase != IDPhaseGameOver {
		t.Fatalf("전원 투표 후 phase = %s", game.Phase)
	}
	if game.Result == nil || game.Result.Winner != "citizens" || game.Result.TopSeat != 1 {
		t.Fatalf("개표 결과 = %+v, want citizens/top=1", game.Result)
	}
	// 종료 스냅샷은 finishGame 이 방 을 정리하기 전 값으로 확인한다
	overSpec := h.buildIDState(room, -1)
	if overSpec.Word != "비밀단어" {
		t.Fatalf("종료 후 관전자에게 제시어 미공개: %q", overSpec.Word)
	}
	roles := map[string]int{}
	for _, pv := range overSpec.Players {
		roles[pv.Role]++
	}
	if roles["master"] != 1 || roles["insider"] != 1 || roles["citizen"] != 3 {
		t.Fatalf("종료 후 역할 공개 이상: %v", roles)
	}
	if overSpec.Result == nil || overSpec.Result.InsiderSeat != 1 {
		t.Fatalf("종료 result = %+v", overSpec.Result)
	}
}

// TestIDAfkAutoProgress 접속만 유지한 채 아무도 정답을 선언하지 않는 4인전 —
// 질문 타임 초과로 전원 패배 종료까지 자동으로 간다 (endsAt 노출·afk 이벤트).
func TestIDAfkAutoProgress(t *testing.T) {
	_, url, cleanup := newIDTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	conns := make([]*idTestClient, IDMinPlayers)
	for i := range conns {
		conns[i] = idDial(t, url)
		defer conns[i].conn.Close()
		idJoin(t, conns[i], fmt.Sprintf("잠수%d", i), "")
	}
	host := conns[0]
	host.send(t, IDMessage{Type: IDMsgStart})

	state := host.idWaitPhase(t, string(IDPhaseQuestion))
	if ends := int64(state["endsAt"].(float64)); ends <= 0 {
		t.Fatalf("질문 스냅샷의 endsAt = %d, want unixMillis", ends)
	}
	if int(state["masterSeat"].(float64)) < 0 {
		t.Fatalf("masterSeat = %v", state["masterSeat"])
	}

	// 나머지는 더 읽지 않는다 — 백그라운드로 비워 버퍼 포화만 막는다
	for _, c := range conns[1:] {
		conn := c.conn
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
	}

	sawAfk := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msg := host.waitMatch(t, "event-or-over", func(m IDMessage) bool {
			return m.Type == IDMsgEvent || m.Type == IDMsgGameOver
		})
		if msg.Type == IDMsgEvent {
			ev := idPayloadMap(t, msg)
			if ev["kind"] == "afk" {
				if !strings.Contains(ev["message"].(string), "자동") {
					t.Fatalf("afk 문구 = %v", ev["message"])
				}
				sawAfk = true
			}
			continue
		}
		over := idPayloadMap(t, msg)
		if over["winner"] != "none" {
			t.Fatalf("질문 초과 종료 winner = %v, want none", over["winner"])
		}
		if int(over["topSeat"].(float64)) != -1 {
			t.Fatalf("topSeat = %v, want -1", over["topSeat"])
		}
		if word, _ := over["word"].(string); word == "" {
			t.Fatal("종료 발표에 제시어 부재")
		}
		if !sawAfk {
			t.Fatal("afk 자동 진행 이벤트가 한 번도 없었다")
		}
		return
	}
	t.Fatal("전원 방치 게임이 5초 안에 끝나지 않았다")
}

// TestIDAfkVoteDeadline 마스터가 정답만 선언하고 아무도 투표하지 않는 판 —
// 토론 마감 자동 투표 개시 → 투표 마감 무작위 투표 → 개표로 완주한다.
func TestIDAfkVoteDeadline(t *testing.T) {
	_, url, cleanup := newIDTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	conns := make([]*idTestClient, IDMinPlayers)
	for i := range conns {
		conns[i] = idDial(t, url)
		defer conns[i].conn.Close()
		idJoin(t, conns[i], fmt.Sprintf("잠수%d", i), "")
	}
	conns[0].send(t, IDMessage{Type: IDMsgStart})
	state := conns[0].idWaitPhase(t, string(IDPhaseQuestion))
	master := int(state["masterSeat"].(float64))

	// 마스터 좌석만 [정답 나옴] 을 누르고 이후로는 전원 방치
	conns[master].send(t, IDMessage{Type: IDMsgCorrect})
	for i, c := range conns {
		if i == 0 {
			continue
		}
		conn := c.conn
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
	}

	over := idPayloadMap(t, conns[0].waitFor(t, IDMsgGameOver))
	winner, _ := over["winner"].(string)
	if winner != "citizens" && winner != "insider" {
		t.Fatalf("투표 마감 종료 winner = %q", winner)
	}
	if int(over["topSeat"].(float64)) < 0 {
		t.Fatalf("무작위 투표 개표인데 topSeat = %v", over["topSeat"])
	}
	votes := 0
	for _, pRaw := range over["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		votes += int(p["votes"].(float64))
	}
	if votes != IDMinPlayers {
		t.Fatalf("총 득표 = %d, want %d (미제출 좌석 무작위 채움)", votes, IDMinPlayers)
	}
}

// TestIDRoomCodeAndSpectate 방 코드 발급·코드 입장·시작 후 코드 관전 진입.
// 관전자 스냅샷은 yourSeat -1 에 word·yourRole 부재. 행동은 전부 차단된다.
func TestIDRoomCodeAndSpectate(t *testing.T) {
	_, url, cleanup := newIDTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := idDial(t, url)
	defer host.conn.Close()
	joined := idJoin(t, host, "호스트", "NEW")
	code, _ := joined["roomCode"].(string)
	if len(code) != roomCodeLen {
		t.Fatalf("roomCode = %q, want %d자", code, roomCodeLen)
	}

	guests := make([]*idTestClient, IDMinPlayers-1)
	for i := range guests {
		guests[i] = idDial(t, url)
		defer guests[i].conn.Close()
		g := idJoin(t, guests[i], fmt.Sprintf("친구%d", i), code)
		if g["roomCode"] != code || int(g["yourSeat"].(float64)) != i+1 {
			t.Fatalf("코드 입장 실패: %v", g)
		}
	}

	host.send(t, IDMessage{Type: IDMsgStart})
	state := host.idWaitPhase(t, string(IDPhaseQuestion))
	if state["roomCode"] != code || len(state["players"].([]interface{})) != IDMinPlayers {
		t.Fatalf("시작 실패: %v", state["players"])
	}
	// 호스트(좌석 0) 시점: 역할은 항상 있고, 제시어는 마스터·인사이더일 때만
	role, _ := state["yourRole"].(string)
	if role != "master" && role != "insider" && role != "citizen" {
		t.Fatalf("yourRole = %q", role)
	}
	_, hasWord := state["word"]
	isMaster := int(state["masterSeat"].(float64)) == 0
	if (role == "master" || role == "insider") != hasWord {
		t.Fatalf("제시어 노출 불일치: role=%q hasWord=%v", role, hasWord)
	}
	if isMaster != (role == "master") {
		t.Fatalf("masterSeat 과 yourRole 불일치: %v", state)
	}

	// 시작된 방의 코드로 들어오면 관전자
	spec := idDial(t, url)
	defer spec.conn.Close()
	spec.send(t, IDMessage{Type: IDMsgJoinGame, Payload: IDJoinGamePayload{Name: "구경꾼", Room: code}})
	specJoined := idPayloadMap(t, spec.waitFor(t, IDMsgSpectateJoined))
	if specJoined["roomCode"] != code {
		t.Fatalf("관전 입장 roomCode = %v", specJoined["roomCode"])
	}

	specState := idPayloadMap(t, spec.waitFor(t, IDMsgGameState))
	if int(specState["yourSeat"].(float64)) != -1 {
		t.Fatalf("관전자 yourSeat = %v, want -1", specState["yourSeat"])
	}
	if leaked, ok := specState["word"]; ok {
		t.Fatalf("관전자에게 제시어 유출: %v", leaked)
	}
	if leaked, ok := specState["yourRole"]; ok {
		t.Fatalf("관전자에게 역할 유출: %v", leaked)
	}
	if int(specState["masterSeat"].(float64)) < 0 {
		t.Fatalf("관전자 masterSeat = %v (마스터는 공개 정보)", specState["masterSeat"])
	}
	for _, pRaw := range specState["players"].([]interface{}) {
		p := pRaw.(map[string]interface{})
		if p["role"] != "" {
			t.Fatalf("관전자에게 좌석 역할 유출: %v", p)
		}
	}

	// 관전자는 어떤 행동도 못 한다
	spec.send(t, IDMessage{Type: IDMsgCorrect})
	errPayload := idPayloadMap(t, spec.waitFor(t, IDMsgError))
	if errPayload["message"] != spectatorDeniedMsg {
		t.Fatalf("관전자 차단 문구 = %v", errPayload["message"])
	}
}

// TestIDReconnect 재접속 3종 — 이탈 통지(id_player_disconnected) 후 세션으로
// 돌아오면 좌석·역할이 그대로 복원되고(id_player_reconnected), 모르는 세션은
// id_session_expired 로 거절된다.
func TestIDReconnect(t *testing.T) {
	_, url, cleanup := newIDTestServer(t, 3*time.Second)
	defer cleanup()

	conns := make([]*idTestClient, IDMinPlayers)
	sessions := make([]string, IDMinPlayers)
	for i := range conns {
		conns[i] = idDial(t, url)
		joined := idJoin(t, conns[i], fmt.Sprintf("P%d", i), "")
		sessions[i], _ = joined["sessionId"].(string)
	}
	defer conns[0].conn.Close()
	conns[0].send(t, IDMessage{Type: IDMsgStart})
	before := conns[0].idWaitPhase(t, string(IDPhaseQuestion))
	beforeRole, _ := before["yourRole"].(string)

	// 좌석 1 이탈 → 남은 사람에게 이탈 통지
	conns[1].conn.Close()
	discon := idPayloadMap(t, conns[0].waitFor(t, IDMsgPlayerDisconnected))
	if int(discon["seat"].(float64)) != 1 || discon["name"] != "P1" {
		t.Fatalf("이탈 통지 = %v", discon)
	}
	if int(discon["graceSeconds"].(float64)) <= 0 {
		t.Fatalf("graceSeconds = %v", discon["graceSeconds"])
	}

	// 세션으로 재접속 → 좌석·역할 복원
	back := idDial(t, url)
	defer back.conn.Close()
	back.send(t, IDMessage{Type: IDMsgRejoin, Payload: IDRejoinPayload{SessionID: sessions[1]}})
	recon := idPayloadMap(t, back.waitFor(t, IDMsgPlayerReconnected))
	if int(recon["seat"].(float64)) != 1 {
		t.Fatalf("재접속 통지 = %v", recon)
	}
	restored := back.idWaitPhase(t, string(IDPhaseQuestion))
	if int(restored["yourSeat"].(float64)) != 1 {
		t.Fatalf("복원 yourSeat = %v", restored["yourSeat"])
	}
	if r, _ := restored["yourRole"].(string); r == "" {
		t.Fatal("복원 스냅샷에 yourRole 부재")
	}
	if beforeRole == "" {
		t.Fatal("시작 스냅샷에 yourRole 부재")
	}

	// 모르는 세션은 만료 처리
	ghost := idDial(t, url)
	defer ghost.conn.Close()
	ghost.send(t, IDMessage{Type: IDMsgRejoin, Payload: IDRejoinPayload{SessionID: "없는-세션"}})
	ghost.waitFor(t, IDMsgSessionExpired)
}

// TestIDBotTakeover 유예 만료 좌석을 봇이 이어받아 게임이 멈추지 않는지 —
// 마스터가 이탈해도 봇 마스터가 정답을 선언해 완주한다.
func TestIDBotTakeover(t *testing.T) {
	_, url, cleanup := newIDTestServer(t, 120*time.Millisecond)
	defer cleanup()

	conns := make([]*idTestClient, IDMinPlayers)
	for i := range conns {
		conns[i] = idDial(t, url)
		defer conns[i].conn.Close()
		idJoin(t, conns[i], fmt.Sprintf("P%d", i), "")
	}
	conns[0].send(t, IDMessage{Type: IDMsgStart})
	state := conns[0].idWaitPhase(t, string(IDPhaseQuestion))
	master := int(state["masterSeat"].(float64))

	// 마스터가 아닌 좌석이 관찰자 (마스터 좌석은 무작위라 골라서 잡는다)
	observer := 0
	if master == 0 {
		observer = 1
	}
	// 마스터 좌석 이탈 → 유예 만료 → 봇 대체
	conns[master].conn.Close()
	sawTakeover := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msg := conns[observer].waitMatch(t, "event-or-over", func(m IDMessage) bool {
			return m.Type == IDMsgEvent || m.Type == IDMsgGameOver
		})
		if msg.Type == IDMsgEvent {
			ev := idPayloadMap(t, msg)
			if ev["kind"] == "bot_takeover" {
				if int(ev["seat"].(float64)) != master {
					t.Fatalf("봇 대체 좌석 = %v, want %d", ev["seat"], master)
				}
				if ev["name"] == nil || ev["name"] == "" {
					t.Fatalf("봇 대체 이벤트에 name 부재: %v", ev)
				}
				sawTakeover = true
			}
			continue
		}
		if !sawTakeover {
			t.Fatal("봇 대체 없이 게임이 끝났다")
		}
		over := idPayloadMap(t, msg)
		if _, ok := over["winner"].(string); !ok {
			t.Fatalf("종료 payload = %v", over)
		}
		return
	}
	t.Fatal("봇 대체 후 게임이 5초 안에 끝나지 않았다")
}
