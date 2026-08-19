package server

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func init() {
	// 리액션 레이트리밋을 짧게 — 연속 발신 억제는 즉시 이중 발신으로,
	// 창 경과 후 재허용은 250ms 대기로 확인한다
	reactRateLimit = 200 * time.Millisecond
}

// avSpectate 관전 입장 — av_join_game(code) → av_spectate_joined 의 payload
func avSpectate(t *testing.T, c *avTestClient, name, code string) map[string]interface{} {
	t.Helper()
	c.send(t, AVMessage{Type: AVMsgJoinGame, Payload: AVJoinGamePayload{Name: name, Room: code}})
	return asPayloadMap(t, c.waitFor(t, AVMsgSpectateJoined).Payload)
}

// TestAVSpectateFlow 관전 모드 통합 — 사설 방 시작 후 코드 입장이 관전으로
// 전환되고(spectate_joined + yourSeat -1 + 은닉 필드 부재), 관전자의 행동·
// 리액션은 전부 에러, 상한 8명 초과는 에러, 이탈 시 spectators 가 줄어든다.
func TestAVSpectateFlow(t *testing.T) {
	_, url, cleanup := newAVTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	// 사설 방 5인 시작
	clients := make([]*avTestClient, AVMinPlayers)
	clients[0] = avDial(t, url)
	defer clients[0].conn.Close()
	_, _, code, hostGame := avJoinRoom(t, clients[0], "방장", "NEW")
	for i := 1; i < AVMinPlayers; i++ {
		clients[i] = avDial(t, url)
		defer clients[i].conn.Close()
		avJoinRoom(t, clients[i], fmt.Sprintf("친구%d", i), code)
	}
	clients[0].send(t, AVMessage{Type: AVMsgStart, Payload: map[string]interface{}{}})
	for _, c := range clients {
		c.waitPhase(t, AVPhaseTeamPick)
	}

	// ---- 시작된 방의 코드로 입장 → 관전 전환 ----
	spec := avDial(t, url)
	defer spec.conn.Close()
	joined := avSpectate(t, spec, "관전자", code)
	if joined["gameId"] != hostGame || joined["roomCode"] != code {
		t.Fatalf("spectate_joined payload 이상: %v (want game=%q code=%q)", joined, hostGame, code)
	}

	// 관전 스냅샷: yourSeat -1, 개인화 필드 부재/빈 값, spectators 반영
	state := spec.waitFor(t, AVMsgGameState)
	sm := asPayloadMap(t, state.Payload)
	if sm["yourSeat"].(float64) != -1 {
		t.Fatalf("관전 스냅샷 yourSeat = %v, want -1", sm["yourSeat"])
	}
	if role, _ := sm["yourRole"].(string); role != "" {
		t.Fatalf("관전 스냅샷에 yourRole 이 있다: %q", role)
	}
	if _, leaked := sm["evilSeats"]; leaked {
		t.Fatalf("관전 스냅샷에 evilSeats 필드가 있다: %v", sm["evilSeats"])
	}
	if sm["spectators"].(float64) != 1 {
		t.Fatalf("관전 스냅샷 spectators = %v, want 1", sm["spectators"])
	}
	if got := len(sm["players"].([]interface{})); got != AVMinPlayers {
		t.Fatalf("관전 스냅샷 인원 = %d, want %d", got, AVMinPlayers)
	}

	// 참가자에게도 관전자 수가 보인다
	clients[0].waitState(t, "spectators=1 스냅샷", func(p map[string]interface{}) bool {
		return p["spectators"] == float64(1)
	})

	// ---- 관전자의 행동·리액션은 전부 에러 ----
	spec.send(t, AVMessage{Type: AVMsgPick, Payload: AVPickPayload{Seats: []int{0, 1}}})
	errMsg := asPayloadMap(t, spec.waitFor(t, AVMsgError).Payload)
	if !strings.Contains(errMsg["message"].(string), "관전자는 참여할 수 없습니다") {
		t.Fatalf("관전자 행동 에러 문구 이상: %v", errMsg)
	}
	spec.send(t, AVMessage{Type: AVMsgReact, Payload: AVReactPayload{Emoji: "👍"}})
	errMsg = asPayloadMap(t, spec.waitFor(t, AVMsgError).Payload)
	if !strings.Contains(errMsg["message"].(string), "관전자는 참여할 수 없습니다") {
		t.Fatalf("관전자 리액션 에러 문구 이상: %v", errMsg)
	}

	// ---- 좌석 보유자의 리액션은 관전자에게도 브로드캐스트된다 ----
	clients[0].send(t, AVMessage{Type: AVMsgReact, Payload: AVReactPayload{Emoji: "🔥"}})
	react := spec.waitMatch(t, "react 이벤트", func(m AVMessage) bool {
		p, ok := m.Payload.(map[string]interface{})
		return m.Type == AVMsgEvent && ok && p["kind"] == "react"
	})
	rm := asPayloadMap(t, react.Payload)
	if rm["seat"].(float64) != 0 || rm["message"] != "🔥" || rm["name"] != "방장" {
		t.Fatalf("react 이벤트 이상: %v", rm)
	}

	// ---- 관전 상한 8명 — 초과 입장은 에러 ----
	extras := make([]*avTestClient, 0, maxSpectators-1)
	for i := 1; i < maxSpectators; i++ {
		c := avDial(t, url)
		defer c.conn.Close()
		avSpectate(t, c, fmt.Sprintf("관전자%d", i+1), code)
		extras = append(extras, c)
	}
	over := avDial(t, url)
	defer over.conn.Close()
	over.send(t, AVMessage{Type: AVMsgJoinGame, Payload: AVJoinGamePayload{Name: "초과", Room: code}})
	errMsg = asPayloadMap(t, over.waitFor(t, AVMsgError).Payload)
	if !strings.Contains(errMsg["message"].(string), "관전 정원이 가득 찼습니다") {
		t.Fatalf("관전 정원 초과 에러 문구 이상: %v", errMsg)
	}

	// ---- 관전자 이탈 → 목록에서 제거되고 수가 줄어든 스냅샷이 돈다 ----
	clients[0].waitState(t, "spectators=8 스냅샷", func(p map[string]interface{}) bool {
		return p["spectators"] == float64(maxSpectators)
	})
	extras[len(extras)-1].conn.Close()
	clients[0].waitState(t, "spectators=7 스냅샷", func(p map[string]interface{}) bool {
		return p["spectators"] == float64(maxSpectators-1)
	})
}

// TestAVReactWhitelistAndRateLimit 리액션 — 대기실에서도 허용, 이벤트
// 브로드캐스트(seat·name·이모지), 화이트리스트 외·레이트리밋 초과는 조용히
// 무시, 창 경과 후 재허용.
func TestAVReactWhitelistAndRateLimit(t *testing.T) {
	_, url, cleanup := newAVTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	a := avDial(t, url)
	defer a.conn.Close()
	b := avDial(t, url)
	defer b.conn.Close()
	avJoin(t, a, "사람1")
	avJoin(t, b, "사람2")

	// waitReact 기대한 (seat, 이모지)의 react 이벤트가 올 때까지 읽는다.
	// forbidden 이모지가 먼저 브로드캐스트되면 실패 (억제 검증).
	waitReact := func(c *avTestClient, name string, wantSeat int, wantEmoji string, forbidden ...string) map[string]interface{} {
		t.Helper()
		msg := c.waitMatch(t, name, func(m AVMessage) bool {
			if m.Type != AVMsgEvent {
				return false
			}
			p, ok := m.Payload.(map[string]interface{})
			if !ok || p["kind"] != "react" {
				return false
			}
			for _, f := range forbidden {
				if p["message"] == f {
					t.Fatalf("억제됐어야 할 리액션이 브로드캐스트됐다: %v", p)
				}
			}
			return p["seat"] == float64(wantSeat) && p["message"] == wantEmoji
		})
		return asPayloadMap(t, msg.Payload)
	}

	// 대기(waiting) 중 리액션 — 이벤트로 브로드캐스트
	a.send(t, AVMessage{Type: AVMsgReact, Payload: AVReactPayload{Emoji: "👍"}})
	got := waitReact(b, "첫 리액션", 0, "👍")
	if got["name"] != "사람1" {
		t.Fatalf("react 이벤트 name 이상: %v", got)
	}

	// 레이트리밋: seat0 즉시 재발신은 무시 — 뒤이은 seat1 발신만 도착한다
	a.send(t, AVMessage{Type: AVMsgReact, Payload: AVReactPayload{Emoji: "🔥"}})
	b.send(t, AVMessage{Type: AVMsgReact, Payload: AVReactPayload{Emoji: "😂"}})
	waitReact(a, "seat1 리액션", 1, "😂", "🔥")

	// 화이트리스트 외는 무시 — 창 경과 후 seat0 재발신은 다시 허용
	b.send(t, AVMessage{Type: AVMsgReact, Payload: AVReactPayload{Emoji: "💀"}})
	time.Sleep(reactRateLimit + 50*time.Millisecond)
	a.send(t, AVMessage{Type: AVMsgReact, Payload: AVReactPayload{Emoji: "😭"}})
	waitReact(b, "창 경과 후 리액션", 0, "😭", "💀", "🔥")
}

// TestBuildStateSpectatorView 6게임 buildXXState(viewerSeat -1) — 게임 시작
// 상태에서 패닉 없이 공개 정보만 담긴 스냅샷을 만드는지 (관전 스냅샷의 근거).
// 개인화 필드는 빈 값/부재, 슬라이스 필드는 JSON null 이 되지 않아야 한다.
func TestBuildStateSpectatorView(t *testing.T) {
	t.Run("avalon", func(t *testing.T) {
		h := NewAVHub()
		game := NewAVGame("g")
		for i := 0; i < AVMinPlayers; i++ {
			game.AddPlayer(fmt.Sprintf("p%d", i))
		}
		if err := game.Start(h.rng); err != nil {
			t.Fatal(err)
		}
		st := h.buildAVState(&avRoom{Game: game, Clients: map[int]*AVClient{}}, -1)
		if st.YourSeat != -1 || st.YourRole != "" || st.EvilSeats != nil {
			t.Fatalf("관전 스냅샷 개인화 필드 이상: %+v", st)
		}
		if st.Results == nil || st.Team == nil {
			t.Fatal("results/team 이 nil — JSON null 로 나간다")
		}
	})

	t.Run("skyfall", func(t *testing.T) {
		h := NewSFHub()
		game := NewSFGame("g")
		for i := 0; i < SFMinPlayers; i++ {
			game.AddPlayer(fmt.Sprintf("p%d", i))
		}
		if err := game.Start(h.rng); err != nil {
			t.Fatal(err)
		}
		st := h.buildSFState(&sfRoom{Game: game, Clients: map[int]*SFClient{}}, -1)
		if st.YourSeat != -1 || st.YourRole != "" || st.MafiaSeats != nil || st.Investigation != nil {
			t.Fatalf("관전 스냅샷 개인화 필드 이상: %+v", st)
		}
	})

	t.Run("spyfall", func(t *testing.T) {
		h := NewSPHub()
		game := NewSPGame("g")
		for i := 0; i < SPMinPlayers; i++ {
			game.AddPlayer(fmt.Sprintf("p%d", i))
		}
		if err := game.Start(h.rng, 3*time.Minute); err != nil {
			t.Fatal(err)
		}
		st := h.buildSPState(&spRoom{Game: game, Clients: map[int]*SPClient{}}, -1)
		if st.YourSeat != -1 || st.IsSpy || st.Location != "" {
			t.Fatalf("관전 스냅샷 개인화 필드 이상: %+v", st)
		}
	})

	t.Run("mighty", func(t *testing.T) {
		h := NewMTHub()
		game := NewMTGame("g")
		for i := 0; i < MTPlayerCount; i++ {
			game.AddPlayer(fmt.Sprintf("p%d", i))
		}
		if err := game.Start(h.rng); err != nil {
			t.Fatal(err)
		}
		st := h.buildMTState(&mtRoom{Game: game, Clients: map[int]*MTClient{}}, -1)
		if st.YourSeat != -1 || st.YourKitty != nil {
			t.Fatalf("관전 스냅샷 개인화 필드 이상: %+v", st)
		}
		if st.YourHand == nil || len(st.YourHand) != 0 {
			t.Fatalf("관전 스냅샷 yourHand = %v, want 빈 배열", st.YourHand)
		}
	})

	t.Run("tichu", func(t *testing.T) {
		h := NewTCHub()
		game := NewTCGame("g")
		for i := 0; i < TCSeats; i++ {
			game.AddPlayer(fmt.Sprintf("p%d", i))
		}
		game.StartHand(h.rng)
		st := h.buildTCState(&tcRoom{Game: game, Clients: map[int]*TCClient{}}, -1)
		if st.YourSeat != -1 || st.ExchangeDone || st.GrandAnswered {
			t.Fatalf("관전 스냅샷 개인화 필드 이상: %+v", st)
		}
		if st.YourHand == nil || len(st.YourHand) != 0 {
			t.Fatalf("관전 스냅샷 yourHand = %v, want 빈 배열", st.YourHand)
		}
		// 남의 손패는 개수만 (그랜드 단계 8장 표기)
		for _, p := range st.Players {
			if p.HandCount == 0 {
				t.Fatalf("players handCount 이상: %+v", p)
			}
		}
	})

	t.Run("davinci", func(t *testing.T) {
		h := NewDVHub()
		game := NewDVGame("g")
		for i := 0; i < DVMaxPlayers; i++ {
			game.AddPlayer(fmt.Sprintf("p%d", i))
		}
		if err := game.Start(h.rng); err != nil {
			t.Fatal(err)
		}
		st := h.buildDVState(&dvRoom{Game: game, Clients: map[int]*DVClient{}}, -1)
		if st.YourSeat != -1 || st.YourPendingJokers != nil {
			t.Fatalf("관전 스냅샷 개인화 필드 이상: %+v", st)
		}
		// 비공개 타일 값은 관전자에게도 감춘다
		for _, p := range st.Players {
			for _, tile := range p.Tiles {
				if !tile.Revealed && tile.Value != nil {
					t.Fatalf("관전 스냅샷에 비공개 타일 값 노출: %+v", tile)
				}
			}
		}
	})
}
