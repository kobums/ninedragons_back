package server

import (
	"fmt"
	"math/rand"
	"testing"
)

// lvRiggedGame 손패·덱을 지정해 playing 상태의 게임을 만든다 (규칙 단위 테스트용).
// current 좌석은 이미 뽑기를 마친 상태(손패 2장)여야 한다.
func lvRiggedGame(hands [][]int, deck []int, current int) *LVGame {
	g := NewLVGame("test")
	for i, h := range hands {
		g.Players = append(g.Players, &LVPlayer{
			Seat: i, Name: fmt.Sprintf("P%d", i),
			Hand: append([]int{}, h...), Discards: []int{}, Alive: true,
		})
	}
	g.Phase = LVPhasePlaying
	g.Ready = true
	g.TargetTokens = 99
	g.RoundNo = 1
	g.CurrentSeat = current
	g.Deck = append([]int{}, deck...)
	g.Removed = LVCountess // 왕자 마지막 뽑기 검증용 알려진 값
	g.RemovedFaceUp = []int{}
	return g
}

func lvSeat(s int) *int { return &s }

// TestLVDeckComposition 16장 구성: 1×5 2×2 3×2 4×2 5×2 6×1 7×1 8×1
func TestLVDeckComposition(t *testing.T) {
	deck := lvDeckComposition()
	if len(deck) != 16 {
		t.Fatalf("덱 %d장, want 16", len(deck))
	}
	counts := map[int]int{}
	for _, c := range deck {
		counts[c]++
	}
	want := map[int]int{1: 5, 2: 2, 3: 2, 4: 2, 5: 2, 6: 1, 7: 1, 8: 1}
	for v, n := range want {
		if counts[v] != n {
			t.Fatalf("카드 %d = %d장, want %d", v, counts[v], n)
		}
	}
}

// TestLVStartSetup 인원별 목표 토큰과 라운드 셋업 (2인은 3장 공개 제거)
func TestLVStartSetup(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, tc := range []struct {
		players, target, faceUp, deck int
	}{
		{2, 7, 3, 9},  // 16 -1 비공개 -3 공개 -2 배분 -1 선공 뽑기
		{3, 5, 0, 11}, // 16 -1 -3 -1
		{4, 4, 0, 10}, // 16 -1 -4 -1
	} {
		g := NewLVGame("t")
		for i := 0; i < tc.players; i++ {
			if _, err := g.AddPlayer(fmt.Sprintf("P%d", i)); err != nil {
				t.Fatalf("AddPlayer: %v", err)
			}
		}
		if err := g.Start(rng); err != nil {
			t.Fatalf("%d인 Start: %v", tc.players, err)
		}
		if g.TargetTokens != tc.target {
			t.Fatalf("%d인 목표 토큰 = %d, want %d", tc.players, g.TargetTokens, tc.target)
		}
		if len(g.RemovedFaceUp) != tc.faceUp {
			t.Fatalf("%d인 공개 제거 = %d장, want %d", tc.players, len(g.RemovedFaceUp), tc.faceUp)
		}
		if len(g.Deck) != tc.deck {
			t.Fatalf("%d인 덱 = %d장, want %d", tc.players, len(g.Deck), tc.deck)
		}
		if len(g.Players[g.CurrentSeat].Hand) != 2 {
			t.Fatalf("선공 손패 = %d장, want 2", len(g.Players[g.CurrentSeat].Hand))
		}
	}
}

// TestLVGuard 경비병: 적중 시 대상 탈락, 빗나가면 유지, 1 추측은 거부
func TestLVGuard(t *testing.T) {
	// 적중 → 2인전이라 즉시 라운드 종료 (last_standing)
	g := lvRiggedGame([][]int{{1, 4}, {5}}, []int{3, 3}, 0)
	res, err := g.Play(0, 1, lvSeat(1), 5)
	if err != nil {
		t.Fatalf("Play: %v", err)
	}
	if !res.GuessCorrect || len(res.Eliminated) != 1 || res.Eliminated[0] != 1 {
		t.Fatalf("적중 탈락 실패: %+v", res)
	}
	if !res.RoundEnded || g.RoundResult == nil || g.RoundResult.WinnerSeat != 0 ||
		g.RoundResult.Reason != "last_standing" {
		t.Fatalf("라운드 결과 = %+v", g.RoundResult)
	}
	if g.Players[0].Tokens != 1 {
		t.Fatalf("승자 토큰 = %d, want 1", g.Players[0].Tokens)
	}

	// 빗나감 → 대상 유지, 턴 이동 (다음 좌석은 뽑아서 2장)
	g = lvRiggedGame([][]int{{1, 4}, {5}}, []int{3, 3}, 0)
	res, err = g.Play(0, 1, lvSeat(1), 8)
	if err != nil {
		t.Fatalf("Play: %v", err)
	}
	if res.GuessCorrect || !g.Players[1].Alive {
		t.Fatalf("빗나갔는데 탈락: %+v", res)
	}
	if g.CurrentSeat != 1 || len(g.Players[1].Hand) != 2 {
		t.Fatalf("턴 이동 실패: current=%d hand=%v", g.CurrentSeat, g.Players[1].Hand)
	}

	// 1(경비병) 추측은 금지, 범위 밖도 금지
	g = lvRiggedGame([][]int{{1, 4}, {5}}, []int{3, 3}, 0)
	if _, err := g.Play(0, 1, lvSeat(1), 1); err == nil {
		t.Fatal("경비병 1 추측이 허용됐다")
	}
	if _, err := g.Play(0, 1, lvSeat(1), 9); err == nil {
		t.Fatal("경비병 9 추측이 허용됐다")
	}
}

// TestLVCountessForced 손에 백작부인+왕/왕자면 반드시 백작부인
func TestLVCountessForced(t *testing.T) {
	g := lvRiggedGame([][]int{{7, 5}, {2}}, []int{3, 3}, 0)
	if _, err := g.Play(0, 5, lvSeat(1), 0); err == nil {
		t.Fatal("백작부인 강제를 어기고 왕자를 냈다")
	}
	res, err := g.Play(0, 7, nil, 0)
	if err != nil {
		t.Fatalf("백작부인 플레이 실패: %v", err)
	}
	if res.Card != LVCountess || len(g.Players[0].Discards) != 1 {
		t.Fatalf("백작부인이 버려지지 않았다: %+v", g.Players[0].Discards)
	}
}

// TestLVBaron 남작: 낮은 쪽 탈락, 동점 무효
func TestLVBaron(t *testing.T) {
	g := lvRiggedGame([][]int{{3, 6}, {5}}, []int{2, 2}, 0)
	res, err := g.Play(0, 3, lvSeat(1), 0)
	if err != nil {
		t.Fatalf("Play: %v", err)
	}
	if res.BaronLoserSeat != 1 || g.Players[1].Alive {
		t.Fatalf("남작 패자 = %d (alive=%v), want 1 탈락", res.BaronLoserSeat, g.Players[1].Alive)
	}
	if res.BaronActorCard != 6 || res.BaronTargetCard != 5 {
		t.Fatalf("비교 값 = %d vs %d", res.BaronActorCard, res.BaronTargetCard)
	}

	// 동점 — 아무도 탈락하지 않는다
	g = lvRiggedGame([][]int{{3, 5}, {5}}, []int{2, 2}, 0)
	res, err = g.Play(0, 3, lvSeat(1), 0)
	if err != nil {
		t.Fatalf("Play: %v", err)
	}
	if res.BaronLoserSeat != -1 || !g.Players[0].Alive || !g.Players[1].Alive {
		t.Fatalf("동점인데 탈락 발생: %+v", res)
	}
}

// TestLVHandmaidProtection 시녀 보호: 대상 지정 불가, 전원 보호면 효과 없이 허용
func TestLVHandmaidProtection(t *testing.T) {
	g := lvRiggedGame([][]int{{4, 1}, {2}, {3}}, []int{5, 5, 5}, 0)
	if _, err := g.Play(0, 4, nil, 0); err != nil {
		t.Fatalf("시녀 플레이 실패: %v", err)
	}
	if !g.Players[0].Protected {
		t.Fatal("시녀 보호가 걸리지 않았다")
	}

	// seat1 이 보호 중인 seat0 을 지목하면 거부
	if _, err := g.Play(1, g.Players[1].Hand[0], lvSeat(0), 5); err == nil {
		t.Fatal("보호 중인 좌석 지목이 허용됐다")
	}

	// 전원 보호 → 대상 없이 효과 없는 카드로 낸다 (허용)
	g2 := lvRiggedGame([][]int{{1, 2}, {3}, {4}}, []int{5, 5, 5}, 0)
	g2.Players[1].Protected = true
	g2.Players[2].Protected = true
	res, err := g2.Play(0, 1, nil, 0)
	if err != nil {
		t.Fatalf("전원 보호 플레이 실패: %v", err)
	}
	if !res.NoEffect || res.TargetSeat != -1 {
		t.Fatalf("효과 없는 플레이가 아니다: %+v", res)
	}

	// 보호는 자기 턴 시작에 풀린다
	g3 := lvRiggedGame([][]int{{4, 1}, {2}}, []int{5, 5, 5}, 0)
	g3.Play(0, 4, nil, 0)                      // seat0 보호
	g3.Play(1, g3.Players[1].Hand[0], nil, 0) // seat1 은 대상 없음 → 효과 없이 턴 종료
	if g3.Players[0].Protected {
		t.Fatal("자기 턴이 돌아왔는데 보호가 유지된다")
	}
}

// TestLVPrince 왕자: 공주를 버리면 탈락, 덱 소진 시 비공개 제거 카드를 뽑는다
func TestLVPrince(t *testing.T) {
	g := lvRiggedGame([][]int{{5, 1}, {8}}, []int{3, 3}, 0)
	res, err := g.Play(0, 5, lvSeat(1), 0)
	if err != nil {
		t.Fatalf("Play: %v", err)
	}
	if res.PrinceDiscarded != 8 || g.Players[1].Alive {
		t.Fatalf("공주 버림 탈락 실패: %+v", res)
	}

	// 자신 지정 + 덱 소진 → 비공개 제거 카드(백작부인=7)를 뽑고 손패 비교로 종료
	g = lvRiggedGame([][]int{{5, 1}, {6}}, []int{}, 0)
	res, err = g.Play(0, 5, lvSeat(0), 0)
	if err != nil {
		t.Fatalf("Play: %v", err)
	}
	if !g.RemovedTaken || res.PrinceDiscarded != 1 {
		t.Fatalf("제거 카드 뽑기 실패: taken=%v res=%+v", g.RemovedTaken, res)
	}
	if !res.RoundEnded || g.RoundResult.Reason != "highest_card" ||
		g.RoundResult.WinnerSeat != 0 { // seat0 손패 7 > seat1 손패 6
		t.Fatalf("덱 소진 비교 결과 = %+v", g.RoundResult)
	}
}

// TestLVKingSwap 왕: 손패 교환
func TestLVKingSwap(t *testing.T) {
	g := lvRiggedGame([][]int{{6, 2}, {8}}, []int{3, 3}, 0)
	if _, err := g.Play(0, 6, lvSeat(1), 0); err != nil {
		t.Fatalf("Play: %v", err)
	}
	if g.Players[0].Hand[0] != 8 || g.Players[1].Hand[0] != 2 {
		t.Fatalf("교환 실패: p0=%v p1=%v", g.Players[0].Hand, g.Players[1].Hand)
	}
}

// TestLVPrincessSelfElimination 공주를 스스로 내면 즉시 탈락
func TestLVPrincessSelfElimination(t *testing.T) {
	g := lvRiggedGame([][]int{{8, 1}, {4}}, []int{3, 3}, 0)
	res, err := g.Play(0, 8, nil, 0)
	if err != nil {
		t.Fatalf("Play: %v", err)
	}
	if g.Players[0].Alive || len(res.Eliminated) != 1 || res.Eliminated[0] != 0 {
		t.Fatalf("공주 자멸 실패: %+v", res)
	}
	if g.RoundResult == nil || g.RoundResult.WinnerSeat != 1 {
		t.Fatalf("남은 1인 승리 실패: %+v", g.RoundResult)
	}
}

// TestLVShowdownTiebreak 덱 소진 비교: 동점이면 버린 카드 합으로 판정
func TestLVShowdownTiebreak(t *testing.T) {
	g := lvRiggedGame([][]int{{4, 3}, {3}}, []int{}, 0)
	g.Players[1].Discards = []int{1} // seat1 합 1, seat0 은 시녀(4)를 버려 합 4
	res, err := g.Play(0, 4, nil, 0)
	if err != nil {
		t.Fatalf("Play: %v", err)
	}
	if !res.RoundEnded || g.RoundResult.Reason != "highest_card" || g.RoundResult.WinnerSeat != 0 {
		t.Fatalf("동점 판정 = %+v", g.RoundResult)
	}
}

// TestLVTokensAndNextRound 목표 도달 전에는 round_end → NextRound 로 승자 선공,
// 목표 도달 시 game_over
func TestLVTokensAndNextRound(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	g := lvRiggedGame([][]int{{1, 4}, {5}}, []int{3, 3}, 0)
	g.TargetTokens = 2
	if _, err := g.Play(0, 1, lvSeat(1), 5); err != nil { // 적중 → 라운드 승
		t.Fatalf("Play: %v", err)
	}
	if g.Phase != LVPhaseRoundEnd {
		t.Fatalf("phase = %s, want round_end", g.Phase)
	}
	if err := g.NextRound(rng); err != nil {
		t.Fatalf("NextRound: %v", err)
	}
	if g.Phase != LVPhasePlaying || g.RoundNo != 2 || g.RoundResult != nil {
		t.Fatalf("다음 라운드 상태 = phase %s round %d", g.Phase, g.RoundNo)
	}
	if g.CurrentSeat != 0 { // 직전 라운드 승자 선공
		t.Fatalf("선공 = seat%d, want 0", g.CurrentSeat)
	}
	if len(g.Players[0].Hand) != 2 || len(g.Players[1].Hand) != 1 {
		t.Fatalf("배분 실패: p0=%v p1=%v", g.Players[0].Hand, g.Players[1].Hand)
	}

	// 두 번째 토큰 → game_over
	g.Players[0].Hand = []int{1, 4}
	g.Players[1].Hand = []int{5}
	g.Players[1].Protected = false
	if _, err := g.Play(0, 1, lvSeat(1), 5); err != nil {
		t.Fatalf("Play: %v", err)
	}
	if g.Phase != LVPhaseGameOver || g.WinnerSeat != 0 {
		t.Fatalf("게임 종료 실패: phase=%s winner=%d", g.Phase, g.WinnerSeat)
	}
}

// TestLVTurnOrderSkipsEliminated 탈락한 좌석은 턴에서 건너뛴다
func TestLVTurnOrderSkipsEliminated(t *testing.T) {
	g := lvRiggedGame([][]int{{1, 4}, {5}, {6}}, []int{2, 2, 2}, 0)
	if _, err := g.Play(0, 1, lvSeat(1), 5); err != nil { // seat1 탈락
		t.Fatalf("Play: %v", err)
	}
	if g.Players[1].Alive {
		t.Fatal("seat1 이 탈락하지 않았다")
	}
	if g.CurrentSeat != 2 {
		t.Fatalf("턴 = seat%d, want 2 (탈락 좌석 건너뜀)", g.CurrentSeat)
	}
	// 탈락자의 손패는 공개 더미로 이동
	if len(g.Players[1].Hand) != 0 || !lvContains(g.Players[1].Discards, 5) {
		t.Fatalf("탈락자 손패 처리 실패: hand=%v discards=%v",
			g.Players[1].Hand, g.Players[1].Discards)
	}
}
