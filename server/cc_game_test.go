package server

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// ccNewTestGame n 인 게임을 시작 상태로 만든다 (차례·주사위는 각 테스트가
// 결정적으로 덮어쓴다)
func ccNewTestGame(t *testing.T, n int) (*CCGame, *rand.Rand) {
	t.Helper()
	g := NewCCGame("cc-test")
	for i := 0; i < n; i++ {
		if _, err := g.AddPlayer(fmt.Sprintf("P%d", i)); err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
	}
	rng := rand.New(rand.NewSource(7))
	if err := g.Start(rng); err != nil {
		t.Fatalf("Start: %v", err)
	}
	g.DrainEvents()
	return g, rng
}

// ccEventText 이벤트 큐를 비우고 문구를 이어붙인다 (문구 검증용)
func ccEventText(g *CCGame) string {
	msgs := []string{}
	for _, ev := range g.DrainEvents() {
		msgs = append(msgs, ev.Kind+":"+ev.Message)
	}
	return strings.Join(msgs, "\n")
}

// TestCCDeclareValidation 선언 검증 — 1~4 만 유효하다. X(0)는 선언 자체가
// 불가능하므로 X 를 굴린 차례는 구조적으로 강제 거짓 선언이 된다.
// 차례 아닌 좌석·rolling 아닌 단계의 선언도 거부된다.
func TestCCDeclareValidation(t *testing.T) {
	g, rng := ccNewTestGame(t, 3)
	g.CurrentSeat = 0
	g.Roll = CCRollX // X — 그래도 선언은 1~4 만 가능 (강제 거짓)

	if err := g.Declare(0, 0, rng); err == nil {
		t.Fatal("X(0) 선언이 허용됐다")
	}
	if err := g.Declare(0, 5, rng); err == nil {
		t.Fatal("범위 밖(5) 선언이 허용됐다")
	}
	if err := g.Declare(1, 3, rng); err == nil {
		t.Fatal("차례 아닌 좌석의 선언이 허용됐다")
	}
	if err := g.Declare(0, 3, rng); err != nil {
		t.Fatalf("Declare(3): %v", err)
	}
	if g.Phase != CCPhaseDoubtWindow || g.Declared != 3 {
		t.Fatalf("phase=%s declared=%d, want doubt_window 3", g.Phase, g.Declared)
	}
	// doubt_window 중의 재선언은 거부
	if err := g.Declare(0, 2, rng); err == nil {
		t.Fatal("의심 창 중의 재선언이 허용됐다")
	}
}

// TestCCDoubtSuccess 의심 성공 두 갈래 — 실제가 X 였던 경우와 선언≠실제.
// 선언자 최전방 말이 떨어지고 실제 값이 lastReveal 로 공개된다.
func TestCCDoubtSuccess(t *testing.T) {
	g, rng := ccNewTestGame(t, 3)

	// ---- 실제가 X (강제 거짓) ----
	g.CurrentSeat = 0
	g.Roll = CCRollX
	if err := g.Declare(0, 3, rng); err != nil {
		t.Fatalf("Declare: %v", err)
	}
	g.SubmitDoubt(1, rng)
	text := ccEventText(g)
	if !strings.Contains(text, "의심 성공") {
		t.Fatalf("의심 성공 이벤트 부재:\n%s", text)
	}
	if g.Players[0].Reserve != CCPawns-1 {
		t.Fatalf("선언자 대기 말 = %d, want %d (다리에 말이 없어 대기 말 낙하)",
			g.Players[0].Reserve, CCPawns-1)
	}
	r := g.Reveal
	if r == nil || r.Result != "doubt_success" || r.Actual != CCRollX ||
		r.Seat != 0 || r.DoubterSeat != 1 || r.Declared != 3 {
		t.Fatalf("lastReveal = %+v", r)
	}
	if g.Phase != CCPhaseRolling || g.CurrentSeat != 1 {
		t.Fatalf("턴 전환 실패: phase=%s current=%d", g.Phase, g.CurrentSeat)
	}

	// ---- 숫자를 굴렸지만 다르게 선언 ----
	g.Roll = 2
	if err := g.Declare(1, 4, rng); err != nil {
		t.Fatalf("Declare#2: %v", err)
	}
	g.SubmitDoubt(2, rng)
	if g.Players[1].Reserve != CCPawns-1 {
		t.Fatalf("선언자 대기 말 = %d, want %d", g.Players[1].Reserve, CCPawns-1)
	}
	if g.Reveal.Result != "doubt_success" || g.Reveal.Actual != 2 {
		t.Fatalf("lastReveal#2 = %+v", g.Reveal)
	}
	// 의심자는 잃지 않는다
	if g.Players[2].Reserve != CCPawns {
		t.Fatalf("의심자 대기 말 = %d, want %d", g.Players[2].Reserve, CCPawns)
	}
}

// TestCCDoubtFail 의심 실패 — 선언=실제면 의심자 말이 떨어지고 선언자는
// 선언 값만큼 전진한다.
func TestCCDoubtFail(t *testing.T) {
	g, rng := ccNewTestGame(t, 3)
	g.CurrentSeat = 0
	g.Roll = 3
	if err := g.Declare(0, 3, rng); err != nil {
		t.Fatalf("Declare: %v", err)
	}
	g.SubmitDoubt(2, rng)
	text := ccEventText(g)
	if !strings.Contains(text, "의심 실패") {
		t.Fatalf("의심 실패 이벤트 부재:\n%s", text)
	}
	if g.Players[2].Reserve != CCPawns-1 {
		t.Fatalf("의심자 대기 말 = %d, want %d", g.Players[2].Reserve, CCPawns-1)
	}
	if len(g.Players[0].Bridge) != 1 || g.Players[0].Bridge[0] != 3 {
		t.Fatalf("선언자 전진 실패: bridge=%v, want [3]", g.Players[0].Bridge)
	}
	if g.Players[0].Reserve != CCPawns-1 {
		t.Fatalf("선언자 대기 말 = %d, want %d (새 말 진입)", g.Players[0].Reserve, CCPawns-1)
	}
	r := g.Reveal
	if r == nil || r.Result != "doubt_fail" || r.Actual != 3 || r.DoubterSeat != 2 {
		t.Fatalf("lastReveal = %+v", r)
	}
	if g.Phase != CCPhaseRolling || g.CurrentSeat != 1 {
		t.Fatalf("턴 전환 실패: phase=%s current=%d", g.Phase, g.CurrentSeat)
	}

	// 뒤늦은 의심·허용은 조용히 무시된다 (rolling 단계)
	g.SubmitDoubt(0, rng)
	g.SubmitAllow(0, rng)
	if g.Phase != CCPhaseRolling || g.Players[1].Reserve != CCPawns {
		t.Fatalf("뒤늦은 응답이 상태를 바꿨다: phase=%s", g.Phase)
	}
}

// TestCCPassAdvanceAndCross 통과 전진 — 전원 허용이면 최전방 말이 전진하고
// 주사위는 비공개(-1)로 남는다. 다리 끝을 넘으면 통과, 2개 통과로 승리.
func TestCCPassAdvanceAndCross(t *testing.T) {
	g, rng := ccNewTestGame(t, 2)

	// 새 말 진입 — 다리 위에 말이 없으면 선언 값 칸에 놓인다
	g.CurrentSeat = 0
	g.Roll = CCRollX // X 로 블러핑했지만 아무도 의심하지 않는다
	if err := g.Declare(0, 4, rng); err != nil {
		t.Fatalf("Declare: %v", err)
	}
	g.SubmitAllow(1, rng)
	if len(g.Players[0].Bridge) != 1 || g.Players[0].Bridge[0] != 4 {
		t.Fatalf("새 말 진입 실패: bridge=%v, want [4]", g.Players[0].Bridge)
	}
	r := g.Reveal
	if r == nil || r.Result != "pass" || r.Actual != -1 || r.DoubterSeat != -1 {
		t.Fatalf("통과인데 주사위가 공개됐다: %+v", r)
	}
	if g.Phase != CCPhaseRolling || g.CurrentSeat != 1 {
		t.Fatalf("턴 전환 실패: phase=%s current=%d", g.Phase, g.CurrentSeat)
	}

	// 최전방 말 전진 — 뒤 말이 아니라 가장 앞 말이 움직인다
	g.CurrentSeat = 0
	g.Players[0].Bridge = []int{2, 4}
	g.Roll = 3
	if err := g.Declare(0, 3, rng); err != nil {
		t.Fatalf("Declare#2: %v", err)
	}
	g.SubmitAllow(1, rng)
	if fmt.Sprint(g.Players[0].Bridge) != "[2 7]" {
		t.Fatalf("최전방 전진 실패: bridge=%v, want [2 7]", g.Players[0].Bridge)
	}

	// 다리 끝(7)을 넘으면 통과
	g.CurrentSeat = 0
	g.Roll = 1
	if err := g.Declare(0, 1, rng); err != nil {
		t.Fatalf("Declare#3: %v", err)
	}
	g.SubmitAllow(1, rng)
	if g.Players[0].Crossed != 1 || fmt.Sprint(g.Players[0].Bridge) != "[2]" {
		t.Fatalf("통과 실패: crossed=%d bridge=%v", g.Players[0].Crossed, g.Players[0].Bridge)
	}
	if !strings.Contains(ccEventText(g), "다리를 건넜습니다") {
		t.Fatal("통과 이벤트 부재")
	}

	// 두 번째 통과 — 즉시 승리
	g.CurrentSeat = 0
	g.Players[0].Bridge = []int{7}
	g.Roll = 2
	if err := g.Declare(0, 2, rng); err != nil {
		t.Fatalf("Declare#4: %v", err)
	}
	g.SubmitAllow(1, rng)
	if g.Phase != CCPhaseGameOver || g.WinnerSeat != 0 || g.EndReason != "two_crossed" {
		t.Fatalf("승리 판정 실패: phase=%s winner=%d reason=%s",
			g.Phase, g.WinnerSeat, g.EndReason)
	}
	if err := g.Declare(1, 1, rng); err == nil {
		t.Fatal("종료 뒤 선언이 허용됐다")
	}
}

// TestCCEliminationAndMostCrossed 낙하 탈락과 결말 — 말을 다 잃으면 탈락하고,
// 혼자 남은 생존자는 의심할 사람이 없어 선언이 즉시 통과된다. 마지막 말이
// 2개 미만 통과로 다리를 건너 전원 탈락이 되면 통과 수 최다 승.
func TestCCEliminationAndMostCrossed(t *testing.T) {
	g, rng := ccNewTestGame(t, 2)

	// seat1 은 마지막 말 하나 — 의심 실패로 낙하해 탈락한다
	g.Players[1].Reserve = 1
	g.Players[1].Bridge = []int{}
	g.CurrentSeat = 0
	g.Roll = 2
	if err := g.Declare(0, 2, rng); err != nil {
		t.Fatalf("Declare: %v", err)
	}
	g.SubmitDoubt(1, rng)
	text := ccEventText(g)
	if !strings.Contains(text, "탈락") {
		t.Fatalf("탈락 이벤트 부재:\n%s", text)
	}
	if g.Players[1].Alive() {
		t.Fatal("말을 다 잃었는데 생존 상태다")
	}
	// 게임은 계속된다 — 혼자 남은 seat0 의 차례
	if g.Phase != CCPhaseRolling || g.CurrentSeat != 0 {
		t.Fatalf("phase=%s current=%d, want rolling seat0", g.Phase, g.CurrentSeat)
	}

	// 혼자 남은 생존자의 마지막 말이 통과 1개로 다리를 건너면 전원 탈락 —
	// 통과 수 최다(1 vs 0)로 seat0 승리
	g.Players[0].Reserve = 0
	g.Players[0].Bridge = []int{7}
	g.Players[0].Crossed = 0
	g.Roll = 3
	if err := g.Declare(0, 3, rng); err != nil {
		t.Fatalf("Declare#2: %v", err)
	}
	// 응답자가 없으므로 창 없이 즉시 통과됐어야 한다
	if g.Phase != CCPhaseGameOver || g.WinnerSeat != 0 || g.EndReason != "most_crossed" {
		t.Fatalf("결말 실패: phase=%s winner=%d reason=%s", g.Phase, g.WinnerSeat, g.EndReason)
	}
	if g.Players[0].Crossed != 1 {
		t.Fatalf("crossed = %d, want 1", g.Players[0].Crossed)
	}
}
