package server

import (
	"math/rand"
	"strings"
	"testing"
	"time"
)

// ==================== 헬퍼 단위 테스트 ====================

func TestRoomCodeNormalize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"  ", ""},
		{"ab12", "AB12"},
		{" ab12 ", "AB12"},
		{"AB12", "AB12"},
		{"new", "NEW"},
		{"New", "NEW"},
	}
	for _, c := range cases {
		if got := normalizeRoomCode(c.in); got != c.want {
			t.Errorf("normalizeRoomCode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRoomCodeGenerateFormat(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 200; i++ {
		code := generateRoomCode(rng, nil)
		if len(code) != roomCodeLen {
			t.Fatalf("코드 길이 = %d, want %d (%q)", len(code), roomCodeLen, code)
		}
		for _, ch := range code {
			if !strings.ContainsRune(roomCodeAlphabet, ch) {
				t.Fatalf("허용 밖 문자 %q in %q", ch, code)
			}
		}
		// 혼동 문자 I/O/0/1 은 알파벳 자체에 없어야 한다
		if strings.ContainsAny(code, "IO01") {
			t.Fatalf("혼동 문자 포함 %q", code)
		}
	}
}

func TestRoomCodeGenerateAvoidsTaken(t *testing.T) {
	first := generateRoomCode(rand.New(rand.NewSource(7)), nil)
	// 같은 시드로 다시 뽑되 첫 코드를 사용 중으로 표시 — 재시도로 다른 코드
	second := generateRoomCode(rand.New(rand.NewSource(7)), map[string]bool{first: true})
	if first == second {
		t.Fatalf("사용 중 코드 %q 가 다시 발급됐다", first)
	}
	if len(second) != roomCodeLen {
		t.Fatalf("재시도 코드 길이 = %d, want %d", len(second), roomCodeLen)
	}
}

func TestRoomCodeTakenCodes(t *testing.T) {
	lobbies := map[string]*spRoom{"AB12": nil, "CD34": nil}
	taken := takenCodes(lobbies)
	if len(taken) != 2 || !taken["AB12"] || !taken["CD34"] {
		t.Fatalf("takenCodes = %v", taken)
	}
}

// ==================== 사설 방 WS 통합 (스파이폴) ====================
//
// 나머지 4종(dv/tc/mt/sf)은 허브 코드 경로가 동일 패턴이라 컴파일과
// 기존 테스트 그린으로 갈음한다.

// spJoinRoom room 필드를 포함해 입장 → sp_player_joined payload
func spJoinRoom(t *testing.T, c *spTestClient, name, room string) map[string]interface{} {
	t.Helper()
	c.send(t, SPMessage{Type: SPMsgJoinGame, Payload: SPJoinGamePayload{Name: name, Room: room}})
	return asPayloadMap(t, c.waitFor(t, SPMsgPlayerJoined).Payload)
}

// lobbyWaitingFor 로비 현황판의 현재 값 (테스트 전용 읽기)
func lobbyWaitingFor(game string) bool {
	lobbyMu.Lock()
	defer lobbyMu.Unlock()
	return lobbyWaiting[game]
}

// TestRoomCodePrivateJoinAndIsolation "NEW" 생성 → 발급 코드 형식,
// 소문자 재입장 → 같은 방, 다른 코드·공용 로비 → 서로 다른 방 (동시 3방 격리)
func TestRoomCodePrivateJoinAndIsolation(t *testing.T) {
	_, url, cleanup := newSPTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	// "NEW" — 서버가 코드를 발급해 새 사설 방을 만든다
	a := spDial(t, url)
	defer a.conn.Close()
	joinedA := spJoinRoom(t, a, "사설장", "NEW")
	code := joinedA["roomCode"].(string)
	gameA := joinedA["gameId"].(string)
	if len(code) != roomCodeLen {
		t.Fatalf("발급 코드 길이 = %d, want %d (%q)", len(code), roomCodeLen, code)
	}
	for _, ch := range code {
		if !strings.ContainsRune(roomCodeAlphabet, ch) {
			t.Fatalf("발급 코드에 허용 밖 문자 %q (%q)", ch, code)
		}
	}

	// 같은 코드 소문자 입장 → 같은 방 (다른 연결)
	b := spDial(t, url)
	defer b.conn.Close()
	joinedB := spJoinRoom(t, b, "사설객", strings.ToLower(code))
	if got := joinedB["gameId"].(string); got != gameA {
		t.Fatalf("같은 코드 입장 gameId = %s, want %s", got, gameA)
	}
	if got := joinedB["roomCode"].(string); got != code {
		t.Fatalf("소문자 입장 roomCode = %q, want %q (정규화)", got, code)
	}
	if seat := int(joinedB["yourSeat"].(float64)); seat != 1 {
		t.Fatalf("같은 방 두 번째 좌석 = %d, want 1", seat)
	}

	// 다른 코드 → 다른 방 (없던 코드는 관대하게 새로 생성)
	c := spDial(t, url)
	defer c.conn.Close()
	joinedC := spJoinRoom(t, c, "딴방장", "WXYZ")
	if got := joinedC["gameId"].(string); got == gameA {
		t.Fatalf("다른 코드가 같은 방에 입장했다 (gameId=%s)", got)
	}
	if got := joinedC["roomCode"].(string); got != "WXYZ" {
		t.Fatalf("roomCode = %q, want WXYZ", got)
	}

	// room 생략 → 기존 공용 로비 (사설 방들과 격리, roomCode 는 "")
	d := spDial(t, url)
	defer d.conn.Close()
	joinedD := spJoinRoom(t, d, "공용인", "")
	gameD := joinedD["gameId"].(string)
	if gameD == gameA || gameD == joinedC["gameId"].(string) {
		t.Fatalf("공용 로비가 사설 방과 겹쳤다 (gameId=%s)", gameD)
	}
	if got := joinedD["roomCode"].(string); got != "" {
		t.Fatalf("공용 로비 roomCode = %q, want \"\"", got)
	}

	// game_state 에도 roomCode 가 실린다 (대기실 코드 표시·재접속 복원용)
	stateA := a.waitPhase(t, SPPhaseWaiting)
	if got := stateA["roomCode"].(string); got != code {
		t.Fatalf("game_state roomCode = %q, want %q", got, code)
	}
}

// TestRoomCodePrivateStartDetachesAndKeepsPublicWaiting 사설 방 시작 시
// privateLobbies 에서 제거되고(같은 코드 재입장 → 새 방), 공용 로비의
// lobbySetWaiting 현황판은 건드리지 않는다.
func TestRoomCodePrivateStartDetachesAndKeepsPublicWaiting(t *testing.T) {
	_, url, cleanup := newSPTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	// 사설 방 3명 (스파이폴 최소 성립 인원)
	host := spDial(t, url)
	defer host.conn.Close()
	joined := spJoinRoom(t, host, "사설장", "NEW")
	code := joined["roomCode"].(string)
	gameID := joined["gameId"].(string)

	guests := []*spTestClient{}
	for i := 0; i < 2; i++ {
		g := spDial(t, url)
		defer g.conn.Close()
		spJoinRoom(t, g, "사설객", code)
		guests = append(guests, g)
	}

	// 공용 로비 대기자 1명 → 현황판 waiting
	pub := spDial(t, url)
	defer pub.conn.Close()
	spJoinRoom(t, pub, "공용대기", "")
	pub.waitPhase(t, SPPhaseWaiting)
	if !lobbyWaitingFor("spyfall") {
		t.Fatalf("공용 대기자 입장 후 현황판이 waiting 이 아니다")
	}

	// 사설 방 시작 — 진행 중에도 roomCode 가 스냅샷에 남는다 (재접속 복원)
	host.send(t, SPMessage{Type: SPMsgStart, Payload: map[string]interface{}{}})
	state := host.waitPhase(t, SPPhasePlaying)
	if got := state["roomCode"].(string); got != code {
		t.Fatalf("진행 중 game_state roomCode = %q, want %q", got, code)
	}

	// 사설 방 시작이 공용 현황판을 끄지 않는다
	if !lobbyWaitingFor("spyfall") {
		t.Fatalf("사설 방 시작이 공용 로비 현황판을 껐다")
	}

	// 시작한 방은 privateLobbies 에서 activeCodes 로 이동 — 같은 코드
	// 재입장은 좌석이 아니라 관전으로 이어진다 (관전 모드 스펙)
	late := spDial(t, url)
	defer late.conn.Close()
	late.send(t, SPMessage{Type: SPMsgJoinGame, Payload: SPJoinGamePayload{Name: "지각생", Room: code}})
	spectate := asPayloadMap(t, late.waitFor(t, SPMsgSpectateJoined).Payload)
	if got := spectate["gameId"].(string); got != gameID {
		t.Fatalf("관전 입장 gameId = %q, want %q", got, gameID)
	}
	if got := spectate["roomCode"].(string); got != code {
		t.Fatalf("관전 입장 roomCode = %q, want %q", got, code)
	}
	// 관전 스냅샷은 yourSeat -1 + 장소 은닉 (공개 정보만)
	specState := late.waitFor(t, SPMsgGameState)
	sm := asPayloadMap(t, specState.Payload)
	if sm["yourSeat"].(float64) != -1 {
		t.Fatalf("관전 스냅샷 yourSeat = %v, want -1", sm["yourSeat"])
	}
	if loc, _ := sm["location"].(string); loc != "" {
		t.Fatalf("관전 스냅샷에 location 이 노출됐다: %q", loc)
	}
}

// TestRoomCodePrivateEmptyLeaveCleanup 대기 중 전원 이탈 시 사설 방이
// 정리된다 — 같은 코드로 다시 들어오면 새 방(새 gameId)이 만들어진다.
func TestRoomCodePrivateEmptyLeaveCleanup(t *testing.T) {
	_, url, cleanup := newSPTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	a := spDial(t, url)
	joinedA := spJoinRoom(t, a, "사설장", "NEW")
	code := joinedA["roomCode"].(string)
	gameID := joinedA["gameId"].(string)

	b := spDial(t, url)
	spJoinRoom(t, b, "사설객", code)

	// 대기 중 전원 이탈 (대기 단계는 유예 없이 즉시 좌석을 비운다)
	a.conn.Close()
	b.conn.Close()

	// 정리는 unregister 처리 후 비동기로 끝난다 — 새 방이 잡힐 때까지 재시도.
	// 정리 전에 옛 방에 붙었다면 곧장 나와서 정리를 막지 않는다.
	deadline := time.Now().Add(3 * time.Second)
	for {
		c := spDial(t, url)
		joinedC := spJoinRoom(t, c, "재입장", code)
		if got := joinedC["gameId"].(string); got != gameID {
			if seat := int(joinedC["yourSeat"].(float64)); seat != 0 {
				t.Fatalf("정리 후 새 방 첫 좌석 = %d, want 0", seat)
			}
			c.conn.Close()
			return
		}
		c.conn.Close()
		if !time.Now().Before(deadline) {
			t.Fatalf("전원 이탈 후에도 사설 방(gameId=%s)이 정리되지 않았다", gameID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
