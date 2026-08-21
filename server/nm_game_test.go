package server

import (
	"fmt"
	"math/rand"
	"testing"
)

// nmTestGame n인 대기 게임 (순수 규칙 테스트용)
func nmTestGame(t *testing.T, n int) *NMGame {
	t.Helper()
	g := NewNMGame("test")
	for i := 0; i < n; i++ {
		if _, err := g.AddPlayer(fmt.Sprintf("P%d", i)); err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
	}
	return g
}

// TestNMBullHeads 소머리 계산 — 기본 1, 5의 배수 2, 10의 배수 3,
// 11의 배수 5, 55는 7. 덱 전체 합은 정식 규칙과 같은 171이어야 한다.
func TestNMBullHeads(t *testing.T) {
	cases := map[int]int{
		1: 1, 2: 1, 4: 1, 7: 1, 103: 1, 104: 1, // 기본
		5: 2, 15: 2, 25: 2, 95: 2, // 5의 배수
		10: 3, 20: 3, 50: 3, 100: 3, // 10의 배수 (5의 배수보다 우선)
		11: 5, 22: 5, 33: 5, 44: 5, 66: 5, 77: 5, 88: 5, 99: 5, // 11의 배수
		55: 7, // 55 = 5×11
	}
	for card, want := range cases {
		if got := nmBullHeads(card); got != want {
			t.Errorf("nmBullHeads(%d) = %d, want %d", card, got, want)
		}
	}

	total := 0
	for c := 1; c <= NMDeckSize; c++ {
		total += nmBullHeads(c)
	}
	if total != 171 {
		t.Fatalf("덱 전체 소머리 합 = %d, want 171", total)
	}

	if got := nmRowHeads([]int{30, 31, 32, 33, 34}); got != 3+1+1+5+1 {
		t.Fatalf("nmRowHeads = %d, want 11", got)
	}
}

// TestNMPlacementRules 배치 규칙 3종 — 자기보다 작은 행 끝 중 가장 큰 행,
// 6번째 카드 행 먹기, 최소 카드의 행 선택 (검증 포함).
func TestNMPlacementRules(t *testing.T) {
	// ---- 자기보다 작은 행 끝 카드 중 가장 큰 행에 붙는다 ----
	g := nmTestGame(t, 2)
	g.Ready = true
	g.Phase = NMPhaseRevealing
	g.Rows = [][]int{{10}, {20}, {30}, {40}}
	g.Pending = []NMPickEntry{{Seat: 0, Card: 25}}

	placement, needChoice := g.PlaceNext()
	if needChoice || placement == nil {
		t.Fatalf("PlaceNext: placement=%v needChoice=%v", placement, needChoice)
	}
	if placement.Row != 1 || placement.Ate || placement.Card != 25 || placement.Seat != 0 {
		t.Fatalf("25는 끝 20인 1행에 붙어야 한다: %+v", placement)
	}
	if len(g.Rows[1]) != 2 || g.Rows[1][1] != 25 {
		t.Fatalf("1행 = %v, want [20 25]", g.Rows[1])
	}
	if g.Players[0].Penalty != 0 {
		t.Fatalf("일반 배치에 벌점이 붙었다: %d", g.Players[0].Penalty)
	}

	// ---- 행의 6번째 카드 — 기존 5장을 벌점으로 먹고 새 행 ----
	g = nmTestGame(t, 2)
	g.Ready = true
	g.Phase = NMPhaseRevealing
	g.Rows = [][]int{{10}, {20}, {30, 31, 32, 33, 34}, {90}}
	g.Pending = []NMPickEntry{{Seat: 1, Card: 35}}

	placement, needChoice = g.PlaceNext()
	if needChoice || placement == nil || placement.Row != 2 || !placement.Ate {
		t.Fatalf("35는 2행 6번째 — 먹어야 한다: %+v (needChoice=%v)", placement, needChoice)
	}
	if want := 3 + 1 + 1 + 5 + 1; g.Players[1].Penalty != want {
		t.Fatalf("먹은 벌점 = %d, want %d", g.Players[1].Penalty, want)
	}
	if len(g.Rows[2]) != 1 || g.Rows[2][0] != 35 {
		t.Fatalf("2행 = %v, want [35]", g.Rows[2])
	}

	// ---- 모든 행 끝보다 작은 카드 — 행 선택 대기 ----
	g = nmTestGame(t, 2)
	g.Ready = true
	g.Phase = NMPhaseRevealing
	g.Rows = [][]int{{10}, {20}, {30}, {40}}
	g.Pending = []NMPickEntry{{Seat: 0, Card: 5}, {Seat: 1, Card: 45}}

	placement, needChoice = g.PlaceNext()
	if !needChoice || placement != nil {
		t.Fatalf("5는 행 선택이 필요하다: placement=%+v", placement)
	}
	if g.Phase != NMPhaseChoosingRow || g.ChooserSeat != 0 {
		t.Fatalf("phase=%s chooser=%d, want choosing_row/0", g.Phase, g.ChooserSeat)
	}
	if _, err := g.ChooseRow(1, 0); err == nil {
		t.Fatal("chooser 가 아닌 좌석의 행 선택이 통과됐다")
	}
	if _, err := g.ChooseRow(0, 4); err == nil {
		t.Fatal("범위 밖 행(4) 선택이 통과됐다")
	}
	placement, err := g.ChooseRow(0, 3)
	if err != nil {
		t.Fatalf("ChooseRow: %v", err)
	}
	if placement.Row != 3 || !placement.Ate || placement.Card != 5 {
		t.Fatalf("행 선택 배치 = %+v", placement)
	}
	if g.Players[0].Penalty != 3 { // {40} = 소머리 3
		t.Fatalf("선택한 행 벌점 = %d, want 3", g.Players[0].Penalty)
	}
	if len(g.Rows[3]) != 1 || g.Rows[3][0] != 5 {
		t.Fatalf("3행 = %v, want [5]", g.Rows[3])
	}
	if g.Phase != NMPhaseRevealing || g.ChooserSeat != -1 {
		t.Fatalf("선택 후 phase=%s chooser=%d, want revealing/-1", g.Phase, g.ChooserSeat)
	}
	// 남은 45는 이어서 정상 배치된다 (끝 30인 2행)
	placement, needChoice = g.PlaceNext()
	if needChoice || placement == nil || placement.Row != 2 || placement.Ate {
		t.Fatalf("45 배치 = %+v (needChoice=%v)", placement, needChoice)
	}

	// ---- MinHeadsRow — 소머리 합 최소 행, 동률은 낮은 인덱스 ----
	g.Rows = [][]int{{55}, {2, 3}, {10}, {4}}
	if got := g.MinHeadsRow(); got != 3 {
		t.Fatalf("MinHeadsRow = %d, want 3 (소머리 1)", got)
	}
	g.Rows = [][]int{{2}, {3}, {10}, {55}}
	if got := g.MinHeadsRow(); got != 0 {
		t.Fatalf("MinHeadsRow 동률 = %d, want 0 (낮은 인덱스)", got)
	}
}

// TestNMPickAndGameFlow 딜·동시 선택 검증·자동 제출·트릭 전환·10트릭 종료
// (소머리 최소 승)까지 순수 규칙 한 판 완주.
func TestNMPickAndGameFlow(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	g := nmTestGame(t, 3)
	if g.CanStart() != true {
		t.Fatal("3인 게임을 시작할 수 없다")
	}
	if err := g.Start(rng); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// 딜: 각자 10장(오름차순), 4개 행에 1장씩, 1트릭 picking
	if g.Phase != NMPhasePicking || g.Trick != 1 {
		t.Fatalf("phase=%s trick=%d", g.Phase, g.Trick)
	}
	seen := map[int]bool{}
	for _, p := range g.Players {
		if len(p.Hand) != NMHandSize {
			t.Fatalf("seat%d 손패 = %d장", p.Seat, len(p.Hand))
		}
		for i, c := range p.Hand {
			if c < 1 || c > NMDeckSize || seen[c] {
				t.Fatalf("카드 중복/범위 밖: %d", c)
			}
			seen[c] = true
			if i > 0 && p.Hand[i-1] >= c {
				t.Fatalf("seat%d 손패 미정렬: %v", p.Seat, p.Hand)
			}
		}
	}
	if len(g.Rows) != NMRows {
		t.Fatalf("행 수 = %d", len(g.Rows))
	}
	for r, row := range g.Rows {
		if len(row) != 1 || seen[row[0]] {
			t.Fatalf("%d행 시작 카드 이상: %v", r, row)
		}
		seen[row[0]] = true
	}

	// 선택 검증: 손에 없는 카드·중복 제출 거부, 제출 즉시 손에서 제거
	if err := g.SubmitPick(0, 999); err == nil {
		t.Fatal("손에 없는 카드가 통과됐다")
	}
	first := g.Players[0].Hand[0]
	if err := g.SubmitPick(0, first); err != nil {
		t.Fatalf("SubmitPick: %v", err)
	}
	if len(g.Players[0].Hand) != NMHandSize-1 || g.Players[0].Pick != first {
		t.Fatalf("제출 후 손패 %d장, pick=%d", len(g.Players[0].Hand), g.Players[0].Pick)
	}
	if err := g.SubmitPick(0, g.Players[0].Hand[0]); err == nil {
		t.Fatal("중복 제출이 통과됐다")
	}
	if g.AllPicked() {
		t.Fatal("미제출자가 있는데 AllPicked")
	}

	// AFK 자동 제출 — 미제출 좌석만 채운다
	auto := g.AutoPickAll(rng)
	if len(auto) != 2 || !g.AllPicked() {
		t.Fatalf("AutoPickAll = %v, allPicked=%v", auto, g.AllPicked())
	}

	// 일괄 공개는 카드 오름차순
	g.StartReveal()
	if g.Phase != NMPhaseRevealing || len(g.Picks) != 3 || len(g.Pending) != 3 {
		t.Fatalf("reveal: phase=%s picks=%d pending=%d", g.Phase, len(g.Picks), len(g.Pending))
	}
	for i := 1; i < len(g.Picks); i++ {
		if g.Picks[i-1].Card >= g.Picks[i].Card {
			t.Fatalf("picks 미정렬: %v", g.Picks)
		}
	}

	// 10트릭 완주 — 매 트릭 무작위 제출, 행 선택은 소머리 최소
	resolve := func() bool {
		for {
			placement, needChoice := g.PlaceNext()
			if needChoice {
				if _, err := g.ChooseRow(g.ChooserSeat, g.MinHeadsRow()); err != nil {
					t.Fatalf("ChooseRow(자동): %v", err)
				}
				continue
			}
			if placement == nil {
				return g.FinishTrick()
			}
		}
	}
	if over := resolve(); over || g.Trick != 2 || g.Phase != NMPhasePicking {
		t.Fatalf("1트릭 후: over=%v trick=%d phase=%s", over, g.Trick, g.Phase)
	}
	if g.Picks != nil || g.LastPlacement != nil {
		t.Fatal("트릭 전환 후 picks/lastPlacement 미초기화")
	}
	for _, p := range g.Players {
		if p.Pick != 0 {
			t.Fatalf("seat%d pick 미초기화", p.Seat)
		}
	}

	over := false
	for trick := 2; trick <= NMTricks; trick++ {
		if len(g.AutoPickAll(rng)) != 3 {
			t.Fatalf("%d트릭 자동 제출 실패", trick)
		}
		g.StartReveal()
		over = resolve()
	}
	if !over || g.Phase != NMPhaseGameOver {
		t.Fatalf("10트릭 후 over=%v phase=%s", over, g.Phase)
	}
	for _, p := range g.Players {
		if len(p.Hand) != 0 {
			t.Fatalf("seat%d 손패 잔량 %d", p.Seat, len(p.Hand))
		}
	}

	// 승자는 소머리 최소 (동점 공동)
	best := 1 << 30
	for _, p := range g.Players {
		if p.Penalty < best {
			best = p.Penalty
		}
	}
	if len(g.WinnerSeats) == 0 {
		t.Fatal("승자 없음")
	}
	for _, s := range g.WinnerSeats {
		if g.Players[s].Penalty != best {
			t.Fatalf("승자 seat%d 소머리 %d ≠ 최소 %d", s, g.Players[s].Penalty, best)
		}
	}
	for _, p := range g.Players {
		if p.Penalty == best {
			found := false
			for _, s := range g.WinnerSeats {
				if s == p.Seat {
					found = true
				}
			}
			if !found {
				t.Fatalf("동점 seat%d 가 공동 승에서 빠졌다", p.Seat)
			}
		}
	}
}
