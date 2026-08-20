package server

import (
	"fmt"
	"math/rand"
	"testing"
)

// ytRiggedGame playing 상태의 게임을 만든다 (규칙 단위 테스트용).
// 선공은 좌석 0, 현재 턴은 current, 주사위·남은 굴림은 지정값이다.
func ytRiggedGame(playerCount, current int, dice []int, rollsLeft int) *YTGame {
	g := NewYTGame("test")
	for i := 0; i < playerCount; i++ {
		if _, err := g.AddPlayer(fmt.Sprintf("P%d", i)); err != nil {
			panic(err)
		}
	}
	g.Phase = YTPhasePlaying
	g.Ready = true
	g.Round = 1
	g.FirstSeat = 0
	g.CurrentSeat = current
	copy(g.Dice, dice)
	g.RollsLeft = rollsLeft
	return g
}

// TestYTScoreUpper 상단 — 해당 눈의 합 (눈 × 개수)
func TestYTScoreUpper(t *testing.T) {
	dice := []int{1, 1, 2, 3, 1}
	for _, tc := range []struct{ face, want int }{
		{1, 3}, {2, 2}, {3, 3}, {4, 0}, {5, 0}, {6, 0},
	} {
		if got := ytScoreUpper(dice, tc.face); got != tc.want {
			t.Fatalf("%v 의 %d의 눈 = %d, want %d", dice, tc.face, got, tc.want)
		}
	}
	if got := ytScoreUpper([]int{6, 6, 6, 6, 6}, 6); got != 30 {
		t.Fatalf("6×5 의 6의 눈 = %d, want 30", got)
	}
}

// TestYTScoreTriple 3 of a kind — 3개 이상 동일이면 전체 합
func TestYTScoreTriple(t *testing.T) {
	for _, tc := range []struct {
		dice []int
		want int
	}{
		{[]int{3, 3, 3, 4, 5}, 18}, // 정확히 3개
		{[]int{2, 2, 2, 2, 6}, 14}, // 4개도 트리플로 인정
		{[]int{5, 5, 5, 5, 5}, 25}, // 5개도 인정
		{[]int{1, 1, 2, 2, 3}, 0},  // 최대 2개 — 0점
	} {
		if got := ytScoreTriple(tc.dice); got != tc.want {
			t.Fatalf("트리플 %v = %d, want %d", tc.dice, got, tc.want)
		}
	}
}

// TestYTScoreQuad 4 of a kind — 4개 이상 동일이면 전체 합
func TestYTScoreQuad(t *testing.T) {
	for _, tc := range []struct {
		dice []int
		want int
	}{
		{[]int{4, 4, 4, 4, 2}, 18}, // 정확히 4개
		{[]int{6, 6, 6, 6, 6}, 30}, // 5개도 인정
		{[]int{3, 3, 3, 5, 5}, 0},  // 3개뿐 — 0점
	} {
		if got := ytScoreQuad(tc.dice); got != tc.want {
			t.Fatalf("포 카드 %v = %d, want %d", tc.dice, got, tc.want)
		}
	}
}

// TestYTScoreFullHouse 풀하우스 — 정확히 3+2 조합만 25점
func TestYTScoreFullHouse(t *testing.T) {
	for _, tc := range []struct {
		dice []int
		want int
	}{
		{[]int{2, 2, 3, 3, 3}, 25},
		{[]int{6, 6, 6, 1, 1}, 25},
		{[]int{2, 2, 2, 2, 3}, 0}, // 4+1 은 아니다
		{[]int{4, 4, 4, 4, 4}, 0}, // 5개 동일도 아니다
		{[]int{1, 2, 3, 4, 5}, 0},
	} {
		if got := ytScoreFullHouse(tc.dice); got != tc.want {
			t.Fatalf("풀하우스 %v = %d, want %d", tc.dice, got, tc.want)
		}
	}
}

// TestYTScoreSmallStraight 스몰 스트레이트 — 4연속이면 30점
func TestYTScoreSmallStraight(t *testing.T) {
	for _, tc := range []struct {
		dice []int
		want int
	}{
		{[]int{1, 2, 3, 4, 6}, 30}, // 1234
		{[]int{2, 5, 4, 3, 3}, 30}, // 2345 (중복 섞임)
		{[]int{3, 4, 5, 6, 6}, 30}, // 3456
		{[]int{1, 2, 3, 4, 5}, 30}, // 라지도 스몰 조건은 충족
		{[]int{1, 2, 3, 5, 6}, 0},  // 3연속뿐
		{[]int{1, 1, 2, 2, 3}, 0},
	} {
		if got := ytScoreSmallStraight(tc.dice); got != tc.want {
			t.Fatalf("스몰 스트레이트 %v = %d, want %d", tc.dice, got, tc.want)
		}
	}
}

// TestYTScoreLargeStraight 라지 스트레이트 — 5연속이면 40점
func TestYTScoreLargeStraight(t *testing.T) {
	for _, tc := range []struct {
		dice []int
		want int
	}{
		{[]int{1, 2, 3, 4, 5}, 40},
		{[]int{6, 5, 4, 3, 2}, 40}, // 순서 무관
		{[]int{1, 2, 3, 4, 6}, 0},
		{[]int{2, 2, 3, 4, 5}, 0}, // 중복이면 5연속 불가
	} {
		if got := ytScoreLargeStraight(tc.dice); got != tc.want {
			t.Fatalf("라지 스트레이트 %v = %d, want %d", tc.dice, got, tc.want)
		}
	}
}

// TestYTScoreYachtChance 요트(5개 동일 50점)·찬스(전체 합)
func TestYTScoreYachtChance(t *testing.T) {
	if got := ytScoreYacht([]int{4, 4, 4, 4, 4}); got != 50 {
		t.Fatalf("요트 = %d, want 50", got)
	}
	if got := ytScoreYacht([]int{4, 4, 4, 4, 5}); got != 0 {
		t.Fatalf("4개 동일 요트 = %d, want 0", got)
	}
	if got := ytScoreChance([]int{1, 3, 4, 6, 6}); got != 20 {
		t.Fatalf("찬스 = %d, want 20", got)
	}
}

// TestYTUpperBonus 상단 합계 63점 이상이면 보너스 +35 (경계 검증)
func TestYTUpperBonus(t *testing.T) {
	scores := map[string]int{}
	for _, cat := range ytCategories {
		scores[cat] = -1
	}
	// 상단 3+6+9+12+15+18 = 63 (각 눈 3개씩)
	for i, cat := range ytCategories[:ytUpperCategoryCount] {
		scores[cat] = (i + 1) * 3
	}
	if got := ytUpperSum(scores); got != 63 {
		t.Fatalf("upperSum = %d, want 63", got)
	}
	if got := ytBonus(scores); got != YTUpperBonus {
		t.Fatalf("bonus = %d, want %d", got, YTUpperBonus)
	}
	if got := ytTotal(scores); got != 63+35 {
		t.Fatalf("total = %d, want 98", got)
	}

	scores[YTCatOnes] = 2 // 62점 — 경계 아래
	if got := ytBonus(scores); got != 0 {
		t.Fatalf("62점 bonus = %d, want 0", got)
	}
	scores[YTCatChance] = 17 // 하단은 보너스 계산에서 제외
	if got := ytBonus(scores); got != 0 {
		t.Fatalf("하단 포함 bonus = %d, want 0", got)
	}
	if got := ytTotal(scores); got != 62+17 {
		t.Fatalf("total = %d, want 79", got)
	}
}

// TestYTRollMechanics 굴림 3회 제한·첫 굴림 홀드 무시·홀드 유지·전부 고정 거부
func TestYTRollMechanics(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	g := ytRiggedGame(2, 0, []int{0, 0, 0, 0, 0}, YTMaxRolls)

	// 굴리기 전에는 기록할 수 없다
	if _, err := g.Score(0, YTCatChance); err == nil {
		t.Fatal("굴림 전 기록이 허용됐다")
	}

	// 첫 굴림 — 홀드를 보내도 무시되고 전부 굴린다
	if err := g.Roll(0, rng, []bool{true, true, true, true, true}); err != nil {
		t.Fatalf("첫 굴림: %v", err)
	}
	if g.RollsLeft != 2 {
		t.Fatalf("rollsLeft = %d, want 2", g.RollsLeft)
	}
	for i, d := range g.Dice {
		if d < 1 || d > 6 {
			t.Fatalf("dice[%d] = %d, want 1~6", i, d)
		}
	}
	for i, held := range g.Held {
		if held {
			t.Fatalf("첫 굴림 후 held[%d] = true, want false", i)
		}
	}

	// 두 번째 굴림 — 홀드한 주사위는 유지된다
	kept := []int{g.Dice[0], g.Dice[2]}
	if err := g.Roll(0, rng, []bool{true, false, true, false, false}); err != nil {
		t.Fatalf("두 번째 굴림: %v", err)
	}
	if g.Dice[0] != kept[0] || g.Dice[2] != kept[1] {
		t.Fatalf("홀드한 주사위가 바뀌었다: %v (kept %v)", g.Dice, kept)
	}
	if !g.Held[0] || g.Held[1] || !g.Held[2] {
		t.Fatalf("held = %v", g.Held)
	}

	// 전부 고정한 굴림은 거부된다
	if err := g.Roll(0, rng, []bool{true, true, true, true, true}); err == nil {
		t.Fatal("전부 고정 굴림이 허용됐다")
	}
	// 홀드 길이가 5가 아니면 거부된다
	if err := g.Roll(0, rng, []bool{true, false}); err == nil {
		t.Fatal("홀드 2개 굴림이 허용됐다")
	}

	// 세 번째 굴림까지 쓰면 더 굴릴 수 없다
	if err := g.Roll(0, rng, []bool{false, false, false, false, false}); err != nil {
		t.Fatalf("세 번째 굴림: %v", err)
	}
	if g.RollsLeft != 0 {
		t.Fatalf("rollsLeft = %d, want 0", g.RollsLeft)
	}
	if err := g.Roll(0, rng, []bool{false, false, false, false, false}); err == nil {
		t.Fatal("네 번째 굴림이 허용됐다")
	}

	// 다른 좌석은 굴릴 수 없다
	if err := g.Roll(1, rng, nil); err == nil {
		t.Fatal("남의 턴 굴림이 허용됐다")
	}
}

// TestYTScoreFlow 기록 → 턴 이동 → 라운드 증가, 중복 기록·잘못된 칸 거부
func TestYTScoreFlow(t *testing.T) {
	g := ytRiggedGame(3, 0, []int{2, 2, 3, 3, 3}, 2)

	if _, err := g.Score(1, YTCatChance); err == nil {
		t.Fatal("남의 턴 기록이 허용됐다")
	}
	if _, err := g.Score(0, "royal"); err == nil {
		t.Fatal("없는 칸 기록이 허용됐다")
	}

	res, err := g.Score(0, YTCatFullHouse)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if res.Points != 25 || g.Players[0].Scores[YTCatFullHouse] != 25 {
		t.Fatalf("풀하우스 기록 = %d, want 25", res.Points)
	}
	if g.LastAction == nil || g.LastAction.Seat != 0 ||
		g.LastAction.Category != YTCatFullHouse || g.LastAction.Points != 25 {
		t.Fatalf("lastAction = %+v", g.LastAction)
	}
	if g.CurrentSeat != 1 || g.RollsLeft != YTMaxRolls || g.Round != 1 {
		t.Fatalf("턴 이동 실패: seat=%d rollsLeft=%d round=%d", g.CurrentSeat, g.RollsLeft, g.Round)
	}
	for i, held := range g.Held {
		if held {
			t.Fatalf("턴 시작 held[%d] = true", i)
		}
	}

	// 나머지 두 좌석이 기록하면 라운드가 넘어간다
	for seat := 1; seat <= 2; seat++ {
		g.RollsLeft = 2 // 굴림을 마친 상태로 만든다
		if _, err := g.Score(seat, YTCatChance); err != nil {
			t.Fatalf("seat%d Score: %v", seat, err)
		}
	}
	if g.Round != 2 || g.CurrentSeat != 0 {
		t.Fatalf("라운드 전환 실패: round=%d seat=%d", g.Round, g.CurrentSeat)
	}

	// 이미 기록한 칸은 거부된다 (0점 다시 쓰기 방지)
	g.RollsLeft = 2
	if _, err := g.Score(0, YTCatFullHouse); err == nil {
		t.Fatal("중복 기록이 허용됐다")
	}
}

// TestYTZeroScoreAllowed 족보가 안 되는 칸에도 0점 기록이 가능하다 (칸 희생)
func TestYTZeroScoreAllowed(t *testing.T) {
	g := ytRiggedGame(2, 0, []int{1, 2, 3, 5, 6}, 0)
	res, err := g.Score(0, YTCatYacht)
	if err != nil {
		t.Fatalf("0점 기록: %v", err)
	}
	if res.Points != 0 || g.Players[0].Scores[YTCatYacht] != 0 {
		t.Fatalf("요트 0점 기록 = %d", res.Points)
	}
}

// TestYTGameOverAndTie 마지막 칸 기록으로 종료·총점 승자 판정 (동점 공동 승)
func TestYTGameOverAndTie(t *testing.T) {
	g := ytRiggedGame(2, 0, []int{1, 1, 2, 2, 3}, 2)
	// 두 좌석 모두 찬스만 남기고 전 칸 0점으로 채운다
	for _, p := range g.Players {
		for _, cat := range ytCategories {
			if cat != YTCatChance {
				p.Scores[cat] = 0
			}
		}
	}
	g.Round = ytTotalRounds()

	res, err := g.Score(0, YTCatChance) // 합 9
	if err != nil {
		t.Fatalf("seat0 Score: %v", err)
	}
	if res.GameOver || g.Phase != YTPhasePlaying {
		t.Fatal("seat0 기록만으로 게임이 끝났다")
	}

	g.RollsLeft = 2 // seat1 도 굴림을 마친 상태
	res, err = g.Score(1, YTCatChance) // 같은 주사위 — 합 9 (동점)
	if err != nil {
		t.Fatalf("seat1 Score: %v", err)
	}
	if !res.GameOver || g.Phase != YTPhaseGameOver {
		t.Fatalf("게임 종료 실패: phase=%s", g.Phase)
	}
	if len(g.WinnerSeats) != 2 || g.WinnerSeats[0] != 0 || g.WinnerSeats[1] != 1 {
		t.Fatalf("동점 공동 승 실패: winnerSeats=%v", g.WinnerSeats)
	}
}

// TestYTBestCategory 봇 기록 선택 — 최고 점수 칸, 전부 0점이면 상단 최소 칸
func TestYTBestCategory(t *testing.T) {
	scores := map[string]int{}
	for _, cat := range ytCategories {
		scores[cat] = -1
	}

	// 풀하우스(25) > 트리플(합 13) > 찬스(13)
	cat, points := ytBestCategory([]int{2, 2, 3, 3, 3}, scores)
	if cat != YTCatFullHouse || points != 25 {
		t.Fatalf("best = %s(%d), want fullhouse(25)", cat, points)
	}

	// 풀하우스가 이미 기록됐으면 다음 최고 점수 칸
	scores[YTCatFullHouse] = 25
	cat, points = ytBestCategory([]int{2, 2, 3, 3, 3}, scores)
	if cat != YTCatTriple || points != 13 {
		t.Fatalf("best = %s(%d), want triple(13)", cat, points)
	}

	// 빈 칸이 전부 0점인 손이면 상단 최소 칸에 0 (ones 부터 희생).
	// 양수가 나올 수 있는 twos/fours/sixes/chance 는 기록된 상태로 만든다.
	for _, c := range ytCategories {
		scores[c] = -1
	}
	for _, c := range []string{YTCatTwos, YTCatFours, YTCatSixes, YTCatChance} {
		scores[c] = 0
	}
	cat, points = ytBestCategory([]int{2, 2, 4, 4, 6}, scores)
	if cat != YTCatOnes || points != 0 {
		t.Fatalf("0점 희생 = %s(%d), want ones(0)", cat, points)
	}
}
