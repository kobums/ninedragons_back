package server

import (
	"math/rand"
	"testing"
)

// newTestCSGame 남 선공으로 고정한 시작 상태
func newTestCSGame(t *testing.T) *CSGame {
	t.Helper()
	g := NewCSGame("test")
	if _, err := g.AddPlayer("남이"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.AddPlayer("북이"); err != nil {
		t.Fatal(err)
	}
	if err := g.Start(rand.New(rand.NewSource(1))); err != nil {
		t.Fatal(err)
	}
	g.CurrentSide = CSSouth
	return g
}

func csOptionsContain(options []CSOption, sums ...int) bool {
	for _, opt := range options {
		if csOptionKey(opt.Sums) == csOptionKey(sums) && len(opt.Sums) == len(sums) {
			return true
		}
	}
	return false
}

func TestCSColLen(t *testing.T) {
	cases := map[int]int{2: 3, 3: 5, 6: 11, 7: 13, 8: 11, 12: 3}
	for col, want := range cases {
		if got := csColLen(col); got != want {
			t.Errorf("컬럼 %d 길이 = %d, want %d", col, got, want)
		}
	}
}

func TestCSOptionsBasic(t *testing.T) {
	g := newTestCSGame(t)

	// 1,2,3,4 → 짝짓기 (3,7), (4,6), (5,5) — 전부 열려 있으니 세 옵션
	g.Dice = []int{1, 2, 3, 4}
	options := g.computeOptions(CSSouth)
	if len(options) != 3 {
		t.Fatalf("옵션 %d개 = %v, want 3", len(options), options)
	}
	for _, want := range [][]int{{3, 7}, {4, 6}, {5, 5}} {
		if !csOptionsContain(options, want...) {
			t.Errorf("옵션에 %v 가 없음: %v", want, options)
		}
	}
}

func TestCSOptionsMarkerConstraint(t *testing.T) {
	g := newTestCSGame(t)

	// 마커 3개를 이미 다른 컬럼에 썼다 → 새 컬럼은 전진 불가
	g.Temp = map[int]int{2: 1, 3: 1, 12: 1}
	g.Dice = []int{1, 2, 3, 4} // (3,7), (4,6), (5,5)
	options := g.computeOptions(CSSouth)
	if !csOptionsContain(options, 3) {
		t.Errorf("마커 있는 3 단독 옵션이 없음: %v", options)
	}
	if csOptionsContain(options, 3, 7) || csOptionsContain(options, 4, 6) || csOptionsContain(options, 5, 5) {
		t.Errorf("마커 제약을 무시한 옵션이 있음: %v", options)
	}

	// 마커 2개 사용 중, 새 컬럼 2개 조합은 하나만 골라야 한다
	g.Temp = map[int]int{2: 1, 3: 1}
	g.Dice = []int{2, 3, 3, 6} // 짝짓기 (5,9), (5,9), (8,6)
	options = g.computeOptions(CSSouth)
	if csOptionsContain(options, 5, 9) {
		t.Errorf("마커 부족인데 두 합 동시 전진이 있음: %v", options)
	}
	if !csOptionsContain(options, 5) || !csOptionsContain(options, 9) {
		t.Errorf("단독 선택지가 없음: %v", options)
	}
}

func TestCSOptionsClaimedAndTopExcluded(t *testing.T) {
	g := newTestCSGame(t)

	// 완등된 컬럼과 임시 마커가 꼭대기인 컬럼은 전진 불가
	g.Claimed[7] = CSNorth
	g.Temp = map[int]int{5: csColLen(5)} // 5 꼭대기
	g.Dice = []int{1, 2, 3, 4}           // (3,7), (4,6), (5,5)
	options := g.computeOptions(CSSouth)
	if csOptionsContain(options, 3, 7) || csOptionsContain(options, 5, 5) {
		t.Errorf("닫힌 컬럼 옵션이 있음: %v", options)
	}
	if !csOptionsContain(options, 3) {
		t.Errorf("7이 닫혔으니 3 단독이 있어야 함: %v", options)
	}
	if !csOptionsContain(options, 4, 6) {
		t.Errorf("4+6 이 없음: %v", options)
	}
}

func TestCSChooseAdvance(t *testing.T) {
	g := newTestCSGame(t)
	g.Progress[CSSouth][6] = 2 // 확정 진행도에서 이어서 전진

	g.Dice = []int{1, 2, 3, 4}
	g.Options = g.computeOptions(CSSouth)
	if err := g.Choose(CSSouth, []int{6, 4}); err != nil {
		t.Fatal(err)
	}
	if g.Temp[6] != 3 || g.Temp[4] != 1 {
		t.Errorf("전진 결과 이상: %v", g.Temp)
	}
	if g.Dice != nil || g.Options != nil {
		t.Error("선택 후 굴림 상태가 정리되지 않음")
	}

	// 같은 합 두 번 전진 (5,5)
	g.Dice = []int{1, 2, 3, 4}
	g.Options = g.computeOptions(CSSouth)
	if err := g.Choose(CSSouth, []int{5, 5}); err != nil {
		t.Fatal(err)
	}
	if g.Temp[5] != 2 {
		t.Errorf("같은 합 두 번 전진 = %d, want 2", g.Temp[5])
	}

	// 잘못된 조합 거부
	g.Dice = []int{1, 2, 3, 4}
	g.Options = g.computeOptions(CSSouth)
	if err := g.Choose(CSSouth, []int{2, 11}); err == nil {
		t.Error("옵션에 없는 조합이 허용됨")
	}
}

func TestCSChooseCapsAtTop(t *testing.T) {
	g := newTestCSGame(t)
	// 2번 컬럼(길이 3)의 꼭대기 직전에서 (2,2) 두 번 전진 → 1칸만 오르고 초과분 소멸
	g.Temp = map[int]int{2: 2}
	g.Dice = []int{1, 1, 1, 1} // 짝짓기 전부 (2,2)
	g.Options = g.computeOptions(CSSouth)
	if err := g.Choose(CSSouth, []int{2, 2}); err != nil {
		t.Fatal(err)
	}
	if g.Temp[2] != 3 {
		t.Errorf("꼭대기 초과 전진 = %d, want 3(꼭대기)", g.Temp[2])
	}
}

func TestCSRollGuardsAndBust(t *testing.T) {
	g := newTestCSGame(t)
	rng := rand.New(rand.NewSource(7))

	// 차례 아님
	if _, err := g.Roll(CSNorth, rng); err == nil {
		t.Error("차례가 아닌데 굴림이 허용됨")
	}

	// 정상 굴림 후 조합 선택 전 재굴림 거부
	if _, err := g.Roll(CSSouth, rng); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Roll(CSSouth, rng); err == nil {
		t.Error("조합 선택 전 재굴림이 허용됨")
	}

	// 모든 컬럼을 닫으면 어떤 굴림도 버스트
	g2 := newTestCSGame(t)
	for col := CSMinCol; col <= CSMaxCol; col++ {
		g2.Claimed[col] = CSNorth
	}
	g2.Temp = map[int]int{}
	result, err := g2.Roll(CSSouth, rng)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Busted {
		t.Fatal("모든 컬럼이 닫혔는데 버스트가 아님")
	}
	if len(g2.Temp) != 0 || g2.CurrentSide != CSNorth || g2.Dice != nil {
		t.Error("버스트 후 턴 상태가 정리되지 않음")
	}
}

func TestCSStopBanksAndClaims(t *testing.T) {
	g := newTestCSGame(t)

	// 굴림 대기 상태가 아니면 정지 불가 조건들
	if _, err := g.Stop(CSSouth); err == nil {
		t.Error("전진 없이 정지가 허용됨")
	}
	g.Dice = []int{1, 1, 1, 1}
	g.Temp = map[int]int{2: 1}
	if _, err := g.Stop(CSSouth); err == nil {
		t.Error("조합 선택 전 정지가 허용됨")
	}
	g.Dice = nil

	// 정지: 진행도 확정 + 꼭대기 컬럼 완등
	g.Temp = map[int]int{2: 3, 5: 4} // 2는 꼭대기(길이 3)
	result, err := g.Stop(CSSouth)
	if err != nil {
		t.Fatal(err)
	}
	if g.Progress[CSSouth][2] != 3 || g.Progress[CSSouth][5] != 4 {
		t.Errorf("뱅킹 결과 이상: %v", g.Progress[CSSouth])
	}
	if len(result.ClaimedCols) != 1 || result.ClaimedCols[0] != 2 || g.Claimed[2] != CSSouth {
		t.Errorf("완등 처리 이상: %v / %v", result.ClaimedCols, g.Claimed)
	}
	if g.CurrentSide != CSNorth || len(g.Temp) != 0 {
		t.Error("정지 후 턴이 정리되지 않음")
	}
}

func TestCSWinAtThreeClaims(t *testing.T) {
	g := newTestCSGame(t)
	g.Claimed[2] = CSSouth
	g.Claimed[12] = CSSouth
	g.Temp = map[int]int{3: csColLen(3)} // 세 번째 완등 준비

	result, err := g.Stop(CSSouth)
	if err != nil {
		t.Fatal(err)
	}
	if !result.GameOver {
		t.Fatal("3컬럼 완등이 승리가 아님")
	}
	if g.Winner != CSSouth || g.EndReason != "claimed_three" || g.Phase != CSPhaseGameOver {
		t.Errorf("승리 상태 이상: %s %s %s", g.Winner, g.EndReason, g.Phase)
	}
	if cols := g.ClaimedBy(CSSouth); len(cols) != 3 {
		t.Errorf("완등 컬럼 = %v, want 3개", cols)
	}

	// 종료 후 굴림 거부
	if _, err := g.Roll(CSNorth, rand.New(rand.NewSource(1))); err == nil {
		t.Error("게임 종료 후 굴림이 허용됨")
	}
}
