package server

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// vgRiggedGame placing 상태의 게임을 만든다 (규칙 단위 테스트용).
// 선공·현재 차례는 좌석 0, 전원 주사위 8개, 주사위·카지노 지폐는 지정한다.
func vgRiggedGame(playerCount int, dice []int) *VGGame {
	g := NewVGGame("test")
	for i := 0; i < playerCount; i++ {
		if _, err := g.AddPlayer(fmt.Sprintf("P%d", i)); err != nil {
			panic(err)
		}
	}
	g.Ready = true
	g.Phase = VGPhasePlacing
	g.Round = 1
	g.FirstSeat = 0
	g.CurrentSeat = 0
	for _, p := range g.Players {
		p.DiceLeft = VGDiceCount
	}
	g.Dice = append([]int{}, dice...)
	return g
}

// TestVGSettleCasinoPayoutOrder 상쇄 없는 기본 지급 — 최다 배치자부터
// 큰 지폐 순, 지폐가 소진되면 나머지는 빈손
func TestVGSettleCasinoPayoutOrder(t *testing.T) {
	payout := vgSettleCasino([]int{10, 7}, map[int]int{0: 3, 1: 2, 2: 1})
	if len(payout) != 2 || payout[0] != 10 || payout[1] != 7 {
		t.Fatalf("payout = %v, want {0:10 1:7}", payout)
	}
	if _, has := payout[2]; has {
		t.Fatalf("지폐 소진 후에도 지급됐다: %v", payout)
	}

	// 지폐 1장이면 1등만 받는다
	payout = vgSettleCasino([]int{9}, map[int]int{0: 1, 1: 4})
	if len(payout) != 1 || payout[1] != 9 {
		t.Fatalf("payout = %v, want {1:9}", payout)
	}
}

// TestVGSettleCasinoTwoWayTie 2인 동수 상쇄 — 서로 전부 제외되어
// 아무도 받지 못한다 (지폐는 버려진다)
func TestVGSettleCasinoTwoWayTie(t *testing.T) {
	payout := vgSettleCasino([]int{10, 8}, map[int]int{0: 3, 1: 3})
	if len(payout) != 0 {
		t.Fatalf("2인 동수 상쇄 실패: payout = %v, want 빈 맵", payout)
	}
}

// TestVGSettleCasinoTieAmongThree 3인 중 2인 상쇄 — 동수인 최다 2인이
// 서로 제외되고, 남은 소수 배치자가 최고 지폐를 받는다
func TestVGSettleCasinoTieAmongThree(t *testing.T) {
	payout := vgSettleCasino([]int{10, 7}, map[int]int{0: 3, 1: 3, 2: 1})
	if len(payout) != 1 || payout[2] != 10 {
		t.Fatalf("3인 중 2인 상쇄 실패: payout = %v, want {2:10}", payout)
	}

	// 하위 동수 상쇄 — 1등은 단독이라 그대로 최고 지폐를 받는다
	payout = vgSettleCasino([]int{10, 7}, map[int]int{0: 4, 1: 2, 2: 2})
	if len(payout) != 1 || payout[0] != 10 {
		t.Fatalf("하위 동수 상쇄 실패: payout = %v, want {0:10}", payout)
	}
}

// TestVGSettleCasinoAllTie 전원 상쇄 — 전원 동수면 아무도 받지 못한다
func TestVGSettleCasinoAllTie(t *testing.T) {
	payout := vgSettleCasino([]int{10, 9, 8}, map[int]int{0: 2, 1: 2, 2: 2})
	if len(payout) != 0 {
		t.Fatalf("전원 상쇄 실패: payout = %v, want 빈 맵", payout)
	}
}

// TestVGSettleCasinoZeroExcluded 배치 0은 순위에 끼지 않는다 (지폐도 못 받는다)
func TestVGSettleCasinoZeroExcluded(t *testing.T) {
	payout := vgSettleCasino([]int{10, 7}, map[int]int{0: 2, 1: 0})
	if len(payout) != 1 || payout[0] != 10 {
		t.Fatalf("payout = %v, want {0:10}", payout)
	}
	if len(vgSettleCasino([]int{10}, map[int]int{})) != 0 {
		t.Fatal("빈 배치에서 지급이 발생했다")
	}
}

// TestVGRoundSetup 시작 시 카지노 6곳 전부 지폐 합계 5만 이상·내림차순,
// 전원 주사위 8개, 선공이 굴린 주사위가 공개된다
func TestVGRoundSetup(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	g := NewVGGame("test")
	g.AddPlayer("A")
	g.AddPlayer("B")
	if err := g.Start(rng); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if g.Phase != VGPhasePlacing || g.Round != 1 {
		t.Fatalf("phase=%s round=%d", g.Phase, g.Round)
	}
	if len(g.Casinos) != VGCasinoCount {
		t.Fatalf("카지노 수 = %d, want %d", len(g.Casinos), VGCasinoCount)
	}
	for i, c := range g.Casinos {
		if c.Face != i+1 {
			t.Fatalf("casinos[%d].Face = %d, want %d", i, c.Face, i+1)
		}
		total := 0
		for j, b := range c.Bills {
			total += b
			if j > 0 && c.Bills[j-1] < b {
				t.Fatalf("카지노 %d 지폐가 내림차순이 아니다: %v", c.Face, c.Bills)
			}
		}
		if total < VGCasinoMinTotal {
			t.Fatalf("카지노 %d 지폐 합계 = %d, want >= %d", c.Face, total, VGCasinoMinTotal)
		}
		if c.Placed == nil || len(c.Placed) != 0 {
			t.Fatalf("카지노 %d 배치 초기값 = %v", c.Face, c.Placed)
		}
	}
	for _, p := range g.Players {
		if p.DiceLeft != VGDiceCount || p.Cash != 0 {
			t.Fatalf("seat%d diceLeft=%d cash=%d", p.Seat, p.DiceLeft, p.Cash)
		}
	}
	if len(g.Dice) != VGDiceCount {
		t.Fatalf("선공 굴림 주사위 수 = %d, want %d", len(g.Dice), VGDiceCount)
	}
	for _, d := range g.Dice {
		if d < 1 || d > 6 {
			t.Fatalf("주사위 눈 이탈: %v", g.Dice)
		}
	}
}

// TestVGPlaceFlow 배치 → 차례 이동 — 없는 눈·남의 차례·범위 밖 눈 거부,
// 배치한 만큼 주사위가 줄고 다음 차례의 굴림이 열린다
func TestVGPlaceFlow(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	g := vgRiggedGame(3, []int{1, 1, 2, 3, 3, 3, 5, 6})

	if _, err := g.Place(1, 1, rng); err == nil {
		t.Fatal("남의 차례 배치가 허용됐다")
	}
	if _, err := g.Place(0, 4, rng); err == nil {
		t.Fatal("굴리지 않은 눈의 배치가 허용됐다")
	}
	if _, err := g.Place(0, 7, rng); err == nil {
		t.Fatal("범위 밖 눈의 배치가 허용됐다")
	}

	res, err := g.Place(0, 3, rng)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	if res.Count != 3 || res.RoundEnded {
		t.Fatalf("res = %+v, want count 3", res)
	}
	if g.Casinos[2].Placed[0] != 3 {
		t.Fatalf("카지노 3 배치 = %v", g.Casinos[2].Placed)
	}
	if g.Players[0].DiceLeft != VGDiceCount-3 {
		t.Fatalf("seat0 diceLeft = %d, want %d", g.Players[0].DiceLeft, VGDiceCount-3)
	}
	if g.CurrentSeat != 1 || len(g.Dice) != VGDiceCount {
		t.Fatalf("차례 이동 실패: seat=%d dice=%v", g.CurrentSeat, g.Dice)
	}
}

// TestVGSkipExhausted 주사위를 소진한 좌석은 건너뛴다
func TestVGSkipExhausted(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	g := vgRiggedGame(3, []int{2, 2, 2, 2, 2, 2, 2, 2})
	g.Players[1].DiceLeft = 0 // seat1 은 이미 소진

	if _, err := g.Place(0, 2, rng); err != nil {
		t.Fatalf("Place: %v", err)
	}
	if g.CurrentSeat != 2 {
		t.Fatalf("소진 좌석 건너뛰기 실패: currentSeat = %d, want 2", g.CurrentSeat)
	}
}

// TestVGRoundSettleTwoWayTie 전원 소진으로 정산까지 도는 전체 흐름 —
// 같은 카지노에 같은 수를 배치한 2인은 상쇄되어 현금 변화가 없다
func TestVGRoundSettleTwoWayTie(t *testing.T) {
	rng := rand.New(rand.NewSource(9))
	g := vgRiggedGame(2, []int{4})
	g.Players[0].DiceLeft = 1
	g.Players[1].DiceLeft = 1
	g.Casinos[3].Bills = []int{10} // 카지노 4

	res, err := g.Place(0, 4, rng)
	if err != nil {
		t.Fatalf("seat0 Place: %v", err)
	}
	if res.RoundEnded {
		t.Fatal("seat1 이 남았는데 라운드가 끝났다")
	}
	g.Dice = []int{4} // seat1 의 굴림을 동일 눈으로 조작
	res, err = g.Place(1, 4, rng)
	if err != nil {
		t.Fatalf("seat1 Place: %v", err)
	}
	if !res.RoundEnded || g.Phase != VGPhaseRoundEnd {
		t.Fatalf("정산 진입 실패: res=%+v phase=%s", res, g.Phase)
	}
	if g.Players[0].Cash != 0 || g.Players[1].Cash != 0 {
		t.Fatalf("동수 상쇄 후 현금 = %d/%d, want 0/0", g.Players[0].Cash, g.Players[1].Cash)
	}
	if g.RoundResult == nil || !strings.Contains(g.RoundResult.Message, "1라운드 정산") {
		t.Fatalf("roundResult = %+v", g.RoundResult)
	}
	if len(g.Dice) != 0 || g.CurrentSeat != -1 {
		t.Fatalf("round_end 상태 이상: dice=%v currentSeat=%d", g.Dice, g.CurrentSeat)
	}
}

// TestVGRoundSettlePayout 정산 지급 흐름 — 단독 배치자가 최고 지폐를 받고
// 다음 라운드에서 카지노·주사위가 새로 준비된다
func TestVGRoundSettlePayout(t *testing.T) {
	rng := rand.New(rand.NewSource(13))
	g := vgRiggedGame(2, []int{6})
	g.Players[0].DiceLeft = 1
	g.Players[1].DiceLeft = 1
	g.Casinos[5].Bills = []int{9} // 카지노 6
	g.Casinos[0].Bills = []int{7} // 카지노 1

	if _, err := g.Place(0, 6, rng); err != nil {
		t.Fatalf("seat0 Place: %v", err)
	}
	g.Dice = []int{1}
	if _, err := g.Place(1, 1, rng); err != nil {
		t.Fatalf("seat1 Place: %v", err)
	}
	if g.Players[0].Cash != 9 || g.Players[1].Cash != 7 {
		t.Fatalf("정산 현금 = %d/%d, want 9/7", g.Players[0].Cash, g.Players[1].Cash)
	}

	// 다음 라운드 — 라운드 증가·선공 회전·카지노와 주사위 재준비
	if err := g.NextRound(rng); err != nil {
		t.Fatalf("NextRound: %v", err)
	}
	if g.Round != 2 || g.Phase != VGPhasePlacing || g.FirstSeat != 1 {
		t.Fatalf("round=%d phase=%s firstSeat=%d", g.Round, g.Phase, g.FirstSeat)
	}
	for _, p := range g.Players {
		if p.DiceLeft != VGDiceCount {
			t.Fatalf("seat%d diceLeft = %d, want %d", p.Seat, p.DiceLeft, VGDiceCount)
		}
	}
	for _, c := range g.Casinos {
		if len(c.Placed) != 0 || len(c.Bills) == 0 {
			t.Fatalf("카지노 %d 재준비 실패: bills=%v placed=%v", c.Face, c.Bills, c.Placed)
		}
	}
	if g.Players[0].Cash != 9 { // 현금은 라운드를 넘어 유지된다
		t.Fatalf("현금 유실: %d", g.Players[0].Cash)
	}
}

// TestVGGameOverAndTie 4라운드 뒤 총액 최고 승리 (동점 공동 승)
func TestVGGameOverAndTie(t *testing.T) {
	rng := rand.New(rand.NewSource(17))
	g := vgRiggedGame(3, []int{})
	g.Phase = VGPhaseRoundEnd
	g.Round = VGTotalRounds
	g.Players[0].Cash = 30
	g.Players[1].Cash = 25
	g.Players[2].Cash = 30

	if err := g.NextRound(rng); err != nil {
		t.Fatalf("NextRound: %v", err)
	}
	if g.Phase != VGPhaseGameOver {
		t.Fatalf("phase = %s, want game_over", g.Phase)
	}
	if len(g.WinnerSeats) != 2 || g.WinnerSeats[0] != 0 || g.WinnerSeats[1] != 2 {
		t.Fatalf("동점 공동 승 실패: winnerSeats = %v", g.WinnerSeats)
	}
}

// TestVGMostCommonFace AFK 자동 배치용 최다 눈 (동수면 높은 눈)
func TestVGMostCommonFace(t *testing.T) {
	if got := vgMostCommonFace([]int{1, 2, 2, 3, 3, 3, 6, 6}); got != 3 {
		t.Fatalf("최다 눈 = %d, want 3", got)
	}
	if got := vgMostCommonFace([]int{2, 2, 5, 5}); got != 5 {
		t.Fatalf("동수 최다 눈 = %d, want 5 (높은 눈 우선)", got)
	}
	if got := vgMostCommonFace([]int{}); got != 0 {
		t.Fatalf("빈 주사위 = %d, want 0", got)
	}
}

// TestVGBotChooseFace 봇 눈 선택 — 단독 1등이 되는 눈 중 최고 지폐 우선,
// 없으면 최다 눈
func TestVGBotChooseFace(t *testing.T) {
	casinos := []vgBotCasino{
		{Face: 1, Bills: []int{6}, Placed: map[string]int{}},
		{Face: 2, Bills: []int{10}, Placed: map[string]int{"1": 5}}, // 5개에 밀려 1등 불가
		{Face: 3, Bills: []int{9}, Placed: map[string]int{}},
		{Face: 4, Bills: []int{8}, Placed: map[string]int{}},
		{Face: 5, Bills: []int{7}, Placed: map[string]int{}},
		{Face: 6, Bills: []int{6}, Placed: map[string]int{}},
	}
	// 2는 1등 불가 → 1등 가능한 3(지폐 9)이 1(지폐 6)보다 우선
	if got := vgBotChooseFace(0, []int{1, 2, 2, 2, 3}, casinos); got != 3 {
		t.Fatalf("choose = %d, want 3", got)
	}

	// 동수(>=)면 1등이 아니다 — 이미 상대가 2개면 내 2개로는 리드 불가
	blocked := []vgBotCasino{
		{Face: 1, Bills: []int{10}, Placed: map[string]int{"1": 2}},
		{Face: 2, Bills: []int{10}, Placed: map[string]int{"1": 9}},
	}
	// 어느 눈도 1등 불가 → 최다 눈(1)로 폴백
	if got := vgBotChooseFace(0, []int{1, 1, 2}, blocked); got != 1 {
		t.Fatalf("폴백 choose = %d, want 1", got)
	}

	// 내 기존 배치가 합산된다 — 이미 2개 놓았으면 +1개로 상대 2개를 넘는다
	stacked := []vgBotCasino{
		{Face: 4, Bills: []int{9}, Placed: map[string]int{"0": 2, "1": 2}},
	}
	if got := vgBotChooseFace(0, []int{4}, stacked); got != 4 {
		t.Fatalf("합산 choose = %d, want 4", got)
	}
}
