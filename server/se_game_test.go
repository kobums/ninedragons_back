package server

import (
	"math/rand"
	"testing"
	"time"
)

// ==================== 세트 순수 규칙 테스트 ====================
//
// 이 게임의 판정은 전부 순수 함수라 표 기반으로 촘촘히 검증할 수 있다.
// 세트 판정은 두 겹으로 확인한다 — 규칙 문장 그대로 쓴 seIsSet 과,
// 좌표 합이 3의 배수인지 보는 독립 구현을 81장 덱 전조합(85,320개)에서
// 대조한다. 두 구현이 모든 조합에서 일치하면 판정에는 빈틈이 없다.

// seCard 테스트용 짧은 생성기
func seCard(shape SEShape, count int, fill SEFill, color SEColor) SECard {
	return seMakeCard(shape, count, fill, color)
}

// seModIsSet 독립 구현 — 좌표(0~2) 합이 3의 배수면 "전부 같거나 전부 다름"
func seModIsSet(a, b, c SECard) bool {
	sum := func(x, y, z int) bool { return (x+y+z)%3 == 0 }
	return sum(seShapeIndex[a.Shape], seShapeIndex[b.Shape], seShapeIndex[c.Shape]) &&
		sum(a.Count-1, b.Count-1, c.Count-1) &&
		sum(seFillIndex[a.Fill], seFillIndex[b.Fill], seFillIndex[c.Fill]) &&
		sum(seColorIndex[a.Color], seColorIndex[b.Color], seColorIndex[c.Color])
}

// seSetlessFamily 좌표가 전부 {0,1} 안에 드는 16장. 어떤 3장을 골라도 세트가
// 아니다 — 값이 둘뿐이라 "전부 다름"이 불가능하고, "전부 같음"이면 같은
// 카드가 되기 때문이다. 바닥 보충 테스트의 재료로 쓴다.
func seSetlessFamily() []SECard {
	out := []SECard{}
	for _, shape := range seShapes[:2] {
		for _, count := range seCounts[:2] {
			for _, fill := range seFills[:2] {
				for _, color := range seColors[:2] {
					out = append(out, seMakeCard(shape, count, fill, color))
				}
			}
		}
	}
	return out
}

// TestSEIsSetTable 세트 판정 표 — 성립 7케이스, 불성립 7케이스.
// 네 속성 각각이 전부 같거나 전부 달라야 하고, 하나라도 "둘만 같으면"
// 성립하지 않는다.
func TestSEIsSetTable(t *testing.T) {
	ok := []struct {
		name  string
		cards [3]SECard
	}{
		{"색만 전부 다름", [3]SECard{
			seCard(SEShapeDiamond, 1, SEFillSolid, SEColorRed),
			seCard(SEShapeDiamond, 1, SEFillSolid, SEColorGreen),
			seCard(SEShapeDiamond, 1, SEFillSolid, SEColorPurple)}},
		{"개수만 전부 다름", [3]SECard{
			seCard(SEShapeWave, 1, SEFillStripe, SEColorGreen),
			seCard(SEShapeWave, 2, SEFillStripe, SEColorGreen),
			seCard(SEShapeWave, 3, SEFillStripe, SEColorGreen)}},
		{"모양만 전부 다름", [3]SECard{
			seCard(SEShapeDiamond, 2, SEFillEmpty, SEColorPurple),
			seCard(SEShapeWave, 2, SEFillEmpty, SEColorPurple),
			seCard(SEShapeOval, 2, SEFillEmpty, SEColorPurple)}},
		{"채움만 전부 다름", [3]SECard{
			seCard(SEShapeOval, 3, SEFillSolid, SEColorRed),
			seCard(SEShapeOval, 3, SEFillStripe, SEColorRed),
			seCard(SEShapeOval, 3, SEFillEmpty, SEColorRed)}},
		{"네 속성 전부 다름", [3]SECard{
			seCard(SEShapeDiamond, 1, SEFillSolid, SEColorRed),
			seCard(SEShapeWave, 2, SEFillStripe, SEColorGreen),
			seCard(SEShapeOval, 3, SEFillEmpty, SEColorPurple)}},
		{"모양·색 같고 개수·채움 다름", [3]SECard{
			seCard(SEShapeWave, 1, SEFillSolid, SEColorRed),
			seCard(SEShapeWave, 2, SEFillStripe, SEColorRed),
			seCard(SEShapeWave, 3, SEFillEmpty, SEColorRed)}},
		{"개수만 같고 나머지 다름", [3]SECard{
			seCard(SEShapeDiamond, 2, SEFillSolid, SEColorRed),
			seCard(SEShapeWave, 2, SEFillStripe, SEColorGreen),
			seCard(SEShapeOval, 2, SEFillEmpty, SEColorPurple)}},
	}
	for _, tc := range ok {
		if !seIsSet(tc.cards[0], tc.cards[1], tc.cards[2]) {
			t.Errorf("%s: 성립해야 하는데 불성립 (%v)", tc.name, tc.cards)
		}
		// 순서가 판정을 바꾸면 안 된다
		if !seIsSet(tc.cards[2], tc.cards[0], tc.cards[1]) {
			t.Errorf("%s: 순서를 바꾸니 판정이 달라진다", tc.name)
		}
	}

	bad := []struct {
		name  string
		cards [3]SECard
	}{
		{"채움이 둘만 같음", [3]SECard{
			seCard(SEShapeDiamond, 1, SEFillSolid, SEColorRed),
			seCard(SEShapeDiamond, 1, SEFillSolid, SEColorGreen),
			seCard(SEShapeDiamond, 1, SEFillStripe, SEColorPurple)}},
		{"개수가 둘만 같음", [3]SECard{
			seCard(SEShapeDiamond, 1, SEFillSolid, SEColorRed),
			seCard(SEShapeDiamond, 2, SEFillSolid, SEColorRed),
			seCard(SEShapeDiamond, 1, SEFillSolid, SEColorGreen)}},
		{"모양이 둘만 같음", [3]SECard{
			seCard(SEShapeDiamond, 1, SEFillSolid, SEColorRed),
			seCard(SEShapeWave, 1, SEFillSolid, SEColorRed),
			seCard(SEShapeDiamond, 1, SEFillSolid, SEColorGreen)}},
		{"색이 둘만 같음", [3]SECard{
			seCard(SEShapeDiamond, 1, SEFillSolid, SEColorRed),
			seCard(SEShapeWave, 2, SEFillStripe, SEColorGreen),
			seCard(SEShapeOval, 3, SEFillEmpty, SEColorRed)}},
		{"채움만 어긋남", [3]SECard{
			seCard(SEShapeDiamond, 1, SEFillSolid, SEColorRed),
			seCard(SEShapeWave, 2, SEFillSolid, SEColorGreen),
			seCard(SEShapeOval, 3, SEFillStripe, SEColorPurple)}},
		{"모양 같은데 색이 어긋남", [3]SECard{
			seCard(SEShapeDiamond, 1, SEFillSolid, SEColorRed),
			seCard(SEShapeDiamond, 2, SEFillStripe, SEColorGreen),
			seCard(SEShapeDiamond, 3, SEFillEmpty, SEColorRed)}},
		{"개수가 3,3,2", [3]SECard{
			seCard(SEShapeOval, 3, SEFillEmpty, SEColorPurple),
			seCard(SEShapeOval, 3, SEFillEmpty, SEColorRed),
			seCard(SEShapeOval, 2, SEFillEmpty, SEColorGreen)}},
	}
	for _, tc := range bad {
		if seIsSet(tc.cards[0], tc.cards[1], tc.cards[2]) {
			t.Errorf("%s: 불성립해야 하는데 성립 (%v)", tc.name, tc.cards)
		}
	}
}

// TestSEDeckIntegrity 81장 덱 — id 0~80이 빠짐없이 한 번씩, 속성 조합도 유일.
// id 는 속성에서 유도되므로 seCardByID 로 왕복해도 같은 카드가 나와야 한다.
func TestSEDeckIntegrity(t *testing.T) {
	deck := seBuildDeck()
	if len(deck) != SEDeckSize {
		t.Fatalf("덱 크기 = %d, want %d", len(deck), SEDeckSize)
	}
	seenID := map[int]bool{}
	seenCombo := map[SECard]bool{}
	for _, c := range deck {
		if c.ID < 0 || c.ID >= SEDeckSize {
			t.Fatalf("id 범위 밖: %+v", c)
		}
		if seenID[c.ID] {
			t.Fatalf("id 중복: %+v", c)
		}
		if seenCombo[c] {
			t.Fatalf("속성 조합 중복: %+v", c)
		}
		seenID[c.ID], seenCombo[c] = true, true

		back, ok := seCardByID(c.ID)
		if !ok || back != c {
			t.Fatalf("id 왕복 실패: %+v → %+v (ok=%t)", c, back, ok)
		}
	}
	if len(seenID) != SEDeckSize {
		t.Fatalf("고유 id 수 = %d", len(seenID))
	}
	if _, ok := seCardByID(-1); ok {
		t.Fatal("id -1 이 통과했다")
	}
	if _, ok := seCardByID(SEDeckSize); ok {
		t.Fatal("id 81 이 통과했다")
	}
}

// TestSEIsSetExhaustiveDeck 81장 덱 완전탐색 대조 — 85,320개 조합 전부에서
// seIsSet(규칙 문장 구현)과 seModIsSet(좌표 합 구현)이 일치해야 한다.
// 세트 총수도 이론값 1080(=81*80/6)과 맞아야 한다.
func TestSEIsSetExhaustiveDeck(t *testing.T) {
	deck := seBuildDeck()
	total := 0
	for i := 0; i < len(deck)-2; i++ {
		for j := i + 1; j < len(deck)-1; j++ {
			for k := j + 1; k < len(deck); k++ {
				got := seIsSet(deck[i], deck[j], deck[k])
				want := seModIsSet(deck[i], deck[j], deck[k])
				if got != want {
					t.Fatalf("판정 불일치 (%d,%d,%d): seIsSet=%t seModIsSet=%t\n%+v\n%+v\n%+v",
						i, j, k, got, want, deck[i], deck[j], deck[k])
				}
				if got {
					total++
				}
			}
		}
	}
	if total != 1080 {
		t.Fatalf("덱 전체 세트 수 = %d, want 1080", total)
	}
}

// TestSEThirdCardIsUnique 카드 2장이 정해지면 세트를 완성하는 세 번째 카드는
// 정확히 하나다 — 판정 함수가 규칙을 제대로 구현했다는 강한 방증이다.
func TestSEThirdCardIsUnique(t *testing.T) {
	deck := seBuildDeck()
	for i := 0; i < len(deck); i += 7 { // 전조합은 위 테스트가 덮으므로 표본만
		for j := i + 1; j < len(deck); j += 11 {
			matches := 0
			for k := range deck {
				if k == i || k == j {
					continue
				}
				if seIsSet(deck[i], deck[j], deck[k]) {
					matches++
				}
			}
			if matches != 1 {
				t.Fatalf("(%d,%d) 를 완성하는 카드가 %d장", i, j, matches)
			}
		}
	}
}

// TestSEFindSetMatchesExhaustive 임의 12장 표본에서 seFindSet/seHasSet 을
// 완전탐색 카운터(seCountSets)와 대조한다. 찾았다면 정말 세트여야 하고,
// 못 찾았다면 완전탐색으로도 0개여야 한다.
func TestSEFindSetMatchesExhaustive(t *testing.T) {
	rng := rand.New(rand.NewSource(20260823))
	deck := seBuildDeck()
	setless := 0

	for trial := 0; trial < 400; trial++ {
		perm := rng.Perm(len(deck))
		board := make([]SECard, 0, SEBoardSize)
		for _, p := range perm[:SEBoardSize] {
			board = append(board, deck[p])
		}

		count := seCountSets(board)
		idx, found := seFindSet(board)
		if found != (count > 0) {
			t.Fatalf("seFindSet=%t 인데 완전탐색 세트 수=%d\n%v", found, count, board)
		}
		if found != seHasSet(board) {
			t.Fatalf("seFindSet 과 seHasSet 이 어긋났다")
		}
		if found {
			if !(idx[0] < idx[1] && idx[1] < idx[2]) {
				t.Fatalf("인덱스가 오름차순이 아니다: %v", idx)
			}
			if !seIsSet(board[idx[0]], board[idx[1]], board[idx[2]]) {
				t.Fatalf("찾은 3장이 세트가 아니다: %v", idx)
			}
		} else {
			setless++
		}
	}
	t.Logf("표본 400회 중 세트 없는 12장 = %d회", setless)
}

// TestSESetlessFamilyHasNoSet 바닥 보충 테스트의 재료 검증 — 좌표가 {0,1}에
// 갇힌 16장에는 세트가 하나도 없다.
func TestSESetlessFamilyHasNoSet(t *testing.T) {
	family := seSetlessFamily()
	if len(family) != 16 {
		t.Fatalf("재료 크기 = %d, want 16", len(family))
	}
	if n := seCountSets(family); n != 0 {
		t.Fatalf("세트 없는 재료에 세트가 %d개 있다", n)
	}
}

// TestSEAnyTwentyOneHasSet 서로 다른 21장에는 반드시 세트가 있다 —
// seReplenish 의 "물리고 재배치" 분기가 실전에서 발동하지 않는 근거다.
func TestSEAnyTwentyOneHasSet(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	deck := seBuildDeck()
	for trial := 0; trial < 300; trial++ {
		perm := rng.Perm(len(deck))
		board := make([]SECard, 0, SEBoardMax)
		for _, p := range perm[:SEBoardMax] {
			board = append(board, deck[p])
		}
		if !seHasSet(board) {
			t.Fatalf("21장인데 세트가 없다: %v", board)
		}
	}
}

// TestSEReplenish 바닥 보충 규칙 — 12장 채우기, 세트 없으면 3장씩(21장까지),
// 덱이 마르면 거기서 멈춤. 입력 슬라이스는 건드리지 않는다.
func TestSEReplenish(t *testing.T) {
	rng := rand.New(rand.NewSource(42))

	// ---- 빈 바닥 + 온전한 덱 → 12장, 세트 보장 ----
	deck := seBuildDeck()
	rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })
	board, rest, reshuffles := seReplenish([]SECard{}, deck, rng)
	if len(board) != SEBoardSize {
		t.Fatalf("첫 배치 바닥 = %d장, want %d", len(board), SEBoardSize)
	}
	if len(rest) != SEDeckSize-SEBoardSize {
		t.Fatalf("첫 배치 덱 = %d장", len(rest))
	}
	if !seHasSet(board) {
		t.Fatal("첫 배치에 세트가 없다")
	}
	if reshuffles != 0 {
		t.Fatalf("첫 배치에서 재배치 %d회", reshuffles)
	}

	// ---- 세트 있는 12장은 그대로 (덱을 축내지 않는다) ----
	same, sameDeck, _ := seReplenish(board, rest, rng)
	if len(same) != SEBoardSize || len(sameDeck) != len(rest) {
		t.Fatalf("세트 있는 12장이 바뀌었다: 바닥 %d 덱 %d", len(same), len(sameDeck))
	}

	// ---- 9장(세트 하나 가져간 뒤) → 12장으로 보충 ----
	short := append([]SECard{}, board[:9]...)
	filled, filledDeck, _ := seReplenish(short, rest, rng)
	if len(filled) < SEBoardSize {
		t.Fatalf("보충 후 바닥 = %d장", len(filled))
	}
	if len(filledDeck) != len(rest)-(len(filled)-9) {
		t.Fatalf("덱 감소량이 맞지 않는다: 덱 %d → %d, 바닥 9 → %d",
			len(rest), len(filledDeck), len(filled))
	}

	// ---- 세트 없는 12장 → 3장씩 더 편다 ----
	family := seSetlessFamily()
	setless := append([]SECard{}, family[:SEBoardSize]...)
	pool := []SECard{}
	for _, c := range seBuildDeck() {
		inBoard := false
		for _, b := range setless {
			if b.ID == c.ID {
				inBoard = true
				break
			}
		}
		if !inBoard {
			pool = append(pool, c)
		}
	}
	grown, grownDeck, _ := seReplenish(setless, pool, rng)
	if len(grown) < SEBoardSize+SEDealStep {
		t.Fatalf("세트 없는 바닥이 늘어나지 않았다: %d장", len(grown))
	}
	if len(grown) > SEBoardMax {
		t.Fatalf("바닥이 상한을 넘었다: %d장", len(grown))
	}
	if !seHasSet(grown) {
		t.Fatalf("보충 후에도 세트가 없다 (%d장, 덱 %d장)", len(grown), len(grownDeck))
	}
	if (len(grown)-SEBoardSize)%SEDealStep != 0 {
		t.Fatalf("3장 단위로 펴지 않았다: %d장", len(grown))
	}
	// 입력을 건드리지 않았는지
	if len(setless) != SEBoardSize {
		t.Fatalf("입력 바닥이 변형됐다: %d장", len(setless))
	}

	// ---- 세트가 끝내 없고 덱도 마르면 거기서 멈춘다 (무한 루프 없음) ----
	done := make(chan struct{})
	go func() {
		defer close(done)
		b, d, _ := seReplenish([]SECard{}, family, rng)
		if len(d) != 0 {
			t.Errorf("덱이 남았다: %d장", len(d))
		}
		if len(b) != len(family) {
			t.Errorf("바닥 = %d장, want %d", len(b), len(family))
		}
		if !seExhausted(b, d) {
			t.Error("덱 0 + 세트 없음인데 종료 조건이 서지 않았다")
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("seReplenish 가 끝나지 않았다 — 무한 루프")
	}
}

// TestSEJudgeClaim claim 판정 표 — 성립 / 불성립 / 이미 사라진 카드.
func TestSEJudgeClaim(t *testing.T) {
	board := []SECard{
		seCard(SEShapeDiamond, 1, SEFillSolid, SEColorRed),    // 0
		seCard(SEShapeDiamond, 1, SEFillSolid, SEColorGreen),  // 1
		seCard(SEShapeDiamond, 1, SEFillSolid, SEColorPurple), // 2
		seCard(SEShapeWave, 2, SEFillStripe, SEColorGreen),    // 3
		seCard(SEShapeOval, 3, SEFillEmpty, SEColorPurple),    // 4
	}
	gone := seCard(SEShapeOval, 1, SEFillEmpty, SEColorGreen)

	cases := []struct {
		name    string
		ids     []int
		wantOK  bool
		wantMsg string
	}{
		{"색 전부 다른 세트", []int{board[0].ID, board[1].ID, board[2].ID}, true, seClaimOKMsg},
		{"네 속성 전부 다른 세트", []int{board[0].ID, board[3].ID, board[4].ID}, true, seClaimOKMsg},
		{"순서를 섞어도 성립", []int{board[4].ID, board[0].ID, board[3].ID}, true, seClaimOKMsg},
		{"불성립", []int{board[0].ID, board[1].ID, board[3].ID}, false, seClaimWrongMsg},
		{"불성립 2", []int{board[1].ID, board[2].ID, board[4].ID}, false, seClaimWrongMsg},
		{"사라진 카드 포함", []int{board[0].ID, board[1].ID, gone.ID}, false, seClaimGoneMsg},
		{"전부 사라진 카드", []int{gone.ID, gone.ID + 1, gone.ID + 2}, false, seClaimGoneMsg},
	}
	for _, tc := range cases {
		idx, ok, msg := seJudgeClaim(board, tc.ids)
		if ok != tc.wantOK || msg != tc.wantMsg {
			t.Errorf("%s: ok=%t msg=%q, want ok=%t msg=%q", tc.name, ok, msg, tc.wantOK, tc.wantMsg)
		}
		if ok {
			if len(idx) != SEClaimSize || !(idx[0] < idx[1] && idx[1] < idx[2]) {
				t.Errorf("%s: 인덱스 = %v", tc.name, idx)
			}
		}
	}

	// 제거는 오름차순 인덱스를 그대로 받아 정확히 3장만 뺀다
	idx, ok, _ := seJudgeClaim(board, []int{board[0].ID, board[1].ID, board[2].ID})
	if !ok {
		t.Fatal("성립 판정 실패")
	}
	after := seRemoveAt(board, idx)
	if len(after) != len(board)-SEClaimSize {
		t.Fatalf("제거 후 = %d장", len(after))
	}
	for _, c := range after {
		if c.ID == board[0].ID || c.ID == board[1].ID || c.ID == board[2].ID {
			t.Fatalf("가져간 카드가 남아 있다: %+v", c)
		}
	}
}

// TestSEValidateIDs claim 형식 검사 — 3장·범위·중복
func TestSEValidateIDs(t *testing.T) {
	if err := seValidateIDs([]int{0, 1, 2}); err != nil {
		t.Fatalf("정상 입력이 거부됐다: %v", err)
	}
	bad := [][]int{
		nil, {}, {0}, {0, 1}, {0, 1, 2, 3},
		{0, 0, 1}, {-1, 0, 1}, {0, 1, SEDeckSize}, {0, 1, 999},
	}
	for _, ids := range bad {
		if err := seValidateIDs(ids); err == nil {
			t.Errorf("%v 가 통과했다", ids)
		}
	}
}

// seTestGame 좌석 n개짜리 시작된 게임
func seTestGame(t *testing.T, n int, seed int64) (*SEGame, *rand.Rand) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	g := NewSEGame("test")
	for i := 0; i < n; i++ {
		if _, err := g.AddPlayer(string(rune('A' + i))); err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
	}
	if err := g.Start(rng); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return g, rng
}

// TestSEClaimScoringAndLock 선착 판정의 점수·잠금 규약.
// 성립 +1, 오답 -1(0 미만 없음) + 5초 잠금, 잠금 중 claim 은 에러.
func TestSEClaimScoringAndLock(t *testing.T) {
	g, rng := seTestGame(t, 2, 1)
	now := time.Now()

	// ---- 0점에서 오답 → 점수는 0에 머물고 잠금만 걸린다 ----
	wrong := seWrongTriple(t, g.Board)
	if err := g.Claim(0, wrong, now, rng); err != nil {
		t.Fatalf("오답 claim 이 에러로 돌아왔다: %v", err)
	}
	if g.Players[0].Score != 0 {
		t.Fatalf("0점에서 감점 후 = %d점 (0 미만 금지)", g.Players[0].Score)
	}
	wantLock := now.UnixMilli() + seClaimLock.Milliseconds()
	if g.Players[0].LockedUntil != wantLock {
		t.Fatalf("lockedUntil = %d, want %d", g.Players[0].LockedUntil, wantLock)
	}
	if g.LastClaim == nil || g.LastClaim.OK || g.LastClaim.Seat != 0 ||
		g.LastClaim.Name != "A" || len(g.LastClaim.IDs) != SEClaimSize {
		t.Fatalf("lastClaim = %+v", g.LastClaim)
	}
	if g.LastClaim.Message != seClaimWrongMsg {
		t.Fatalf("오답 문구 = %q", g.LastClaim.Message)
	}
	if g.SetsFound != 0 {
		t.Fatalf("오답인데 setsFound = %d", g.SetsFound)
	}

	// ---- 잠금 중 claim 은 에러 (점수·잠금 불변) ----
	// 잠금 시간은 테스트에서 짧게 낮추는 var 라, 절대값 대신 lockedUntil
	// 직전 시각을 기준으로 삼아야 값이 바뀌어도 흔들리지 않는다.
	idx, ok := seFindSet(g.Board)
	if !ok {
		t.Fatal("바닥에 세트가 없다")
	}
	good := []int{g.Board[idx[0]].ID, g.Board[idx[1]].ID, g.Board[idx[2]].ID}
	duringLock := time.UnixMilli(g.Players[0].LockedUntil - 1)
	if err := g.Claim(0, good, duringLock, rng); err == nil {
		t.Fatal("잠금 중 claim 이 통과했다")
	}
	if g.Players[0].Score != 0 || g.SetsFound != 0 {
		t.Fatalf("잠금 중 claim 이 상태를 바꿨다: %+v", g.Players[0])
	}

	// ---- 다른 좌석은 잠금과 무관 (좌석별 잠금) ----
	if g.Players[1].LockedUntil != 0 {
		t.Fatalf("남의 오답에 seat1 이 잠겼다: %d", g.Players[1].LockedUntil)
	}
	deckBefore := len(g.Deck)
	if err := g.Claim(1, good, now, rng); err != nil {
		t.Fatalf("성립 claim 이 에러: %v", err)
	}
	if g.Players[1].Score != 1 || g.SetsFound != 1 {
		t.Fatalf("성립 후 점수=%d setsFound=%d", g.Players[1].Score, g.SetsFound)
	}
	if g.LastClaim.OK != true || g.LastClaim.Seat != 1 || g.LastClaim.Message != seClaimOKMsg {
		t.Fatalf("성립 lastClaim = %+v", g.LastClaim)
	}
	if len(g.Board) != SEBoardSize {
		t.Fatalf("보충 후 바닥 = %d장", len(g.Board))
	}
	if len(g.Deck) != deckBefore-SEClaimSize {
		t.Fatalf("덱 = %d장, want %d", len(g.Deck), deckBefore-SEClaimSize)
	}
	for _, c := range g.Board {
		for _, id := range good {
			if c.ID == id {
				t.Fatalf("가져간 카드가 바닥에 남았다: %+v", c)
			}
		}
	}

	// ---- 이미 사라진 카드를 집으면 오답 (선착에서 밀린 쪽의 결말) ----
	g.Players[1].Score = 3
	if err := g.Claim(1, good, now.Add(2*time.Second), rng); err != nil {
		t.Fatalf("사라진 카드 claim 이 에러로 돌아왔다: %v", err)
	}
	if g.LastClaim.Message != seClaimGoneMsg || g.LastClaim.OK {
		t.Fatalf("사라진 카드 판정 = %+v", g.LastClaim)
	}
	if g.Players[1].Score != 2 {
		t.Fatalf("감점 후 = %d점, want 2", g.Players[1].Score)
	}

	// ---- 형식 오류는 점수를 건드리지 않는다 ----
	g.Players[0].LockedUntil = 0
	before := g.Players[0].Score
	if err := g.Claim(0, []int{1, 2}, now.Add(3*time.Second), rng); err == nil {
		t.Fatal("2장 claim 이 통과했다")
	}
	if g.Players[0].Score != before || g.Players[0].LockedUntil != 0 {
		t.Fatalf("형식 오류가 상태를 바꿨다: %+v", g.Players[0])
	}

	// ---- 없는 좌석·미진행 게임 ----
	if err := g.Claim(9, good, now, rng); err == nil {
		t.Fatal("없는 좌석의 claim 이 통과했다")
	}
	g.Phase = SEPhaseGameOver
	if err := g.Claim(0, good, now, rng); err == nil {
		t.Fatal("종료된 게임의 claim 이 통과했다")
	}
}

// seWrongTriple 바닥에서 세트가 아닌 3장의 id 를 고른다
func seWrongTriple(t *testing.T, board []SECard) []int {
	t.Helper()
	for i := 0; i < len(board)-2; i++ {
		for j := i + 1; j < len(board)-1; j++ {
			for k := j + 1; k < len(board); k++ {
				if !seIsSet(board[i], board[j], board[k]) {
					return []int{board[i].ID, board[j].ID, board[k].ID}
				}
			}
		}
	}
	t.Fatal("바닥에서 불성립 3장을 못 찾았다")
	return nil
}

// TestSEGameCompletes 성립만 반복하면 반드시 끝난다 — 덱 81장이 소진되거나
// 바닥에 세트가 고갈되면 종료 판정이 선다. 무한 게임이 없다는 근거.
func TestSEGameCompletes(t *testing.T) {
	for seed := int64(1); seed <= 8; seed++ {
		g, rng := seTestGame(t, 3, seed)
		now := time.Now()

		steps := 0
		for g.Phase == SEPhasePlaying {
			steps++
			if steps > SEDeckSize {
				t.Fatalf("seed %d: %d수를 넘겨도 안 끝났다", seed, steps)
			}
			idx, ok := seFindSet(g.Board)
			if !ok {
				t.Fatalf("seed %d: 진행 중인데 바닥에 세트가 없다 (덱 %d장)", seed, len(g.Deck))
			}
			ids := []int{g.Board[idx[0]].ID, g.Board[idx[1]].ID, g.Board[idx[2]].ID}
			seat := steps % len(g.Players)
			g.Players[seat].LockedUntil = 0
			if err := g.Claim(seat, ids, now, rng); err != nil {
				t.Fatalf("seed %d: claim 에러 %v", seed, err)
			}
		}

		if g.Phase != SEPhaseGameOver {
			t.Fatalf("seed %d: phase = %s", seed, g.Phase)
		}
		if len(g.Deck) != 0 || seHasSet(g.Board) {
			t.Fatalf("seed %d: 종료했는데 덱 %d장·세트 %t", seed, len(g.Deck), seHasSet(g.Board))
		}
		if g.EndReason != "deck_empty" {
			t.Fatalf("seed %d: 종료 사유 = %q", seed, g.EndReason)
		}
		if g.Result == nil || len(g.Result.WinnerSeats) == 0 || g.Result.Message == "" {
			t.Fatalf("seed %d: result = %+v", seed, g.Result)
		}
		total := 0
		for _, p := range g.Players {
			total += p.Score
		}
		if total != g.SetsFound || g.SetsFound != steps {
			t.Fatalf("seed %d: 점수합 %d / setsFound %d / 수 %d", seed, total, g.SetsFound, steps)
		}
		// 남은 카드 총합은 81 - 3*세트 여야 한다
		if len(g.Board)+len(g.Deck) != SEDeckSize-SEClaimSize*g.SetsFound {
			t.Fatalf("seed %d: 카드 총량이 맞지 않는다 (바닥 %d + 덱 %d, 세트 %d)",
				seed, len(g.Board), len(g.Deck), g.SetsFound)
		}
	}
}

// TestSEWinnersAndForceEnd 최고점 승·동점 공동 승리, 그리고 10분 캡의
// 강제 종료가 현재 점수로 정산하는지.
func TestSEWinnersAndForceEnd(t *testing.T) {
	players := []*SEPlayer{
		{Seat: 0, Name: "가", Score: 2},
		{Seat: 1, Name: "나", Score: 5},
		{Seat: 2, Name: "다", Score: 5},
		{Seat: 3, Name: "라", Score: 0},
	}
	seats, names := seWinners(players)
	if len(seats) != 2 || seats[0] != 1 || seats[1] != 2 {
		t.Fatalf("공동 승자 좌석 = %v", seats)
	}
	if len(names) != 2 || names[0] != "나" || names[1] != "다" {
		t.Fatalf("공동 승자 이름 = %v", names)
	}

	// 전원 0점이어도 승자는 나온다 (빈 슬라이스 금지)
	zero := []*SEPlayer{{Seat: 0, Name: "가"}, {Seat: 1, Name: "나"}}
	zs, zn := seWinners(zero)
	if len(zs) != 2 || len(zn) != 2 {
		t.Fatalf("전원 0점 승자 = %v / %v", zs, zn)
	}

	// ---- 강제 종료 ----
	g, _ := seTestGame(t, 2, 3)
	g.Players[0].Score = 4
	g.Players[1].Score = 1
	g.SetsFound = 5
	g.ForceEnd()
	if g.Phase != SEPhaseGameOver || g.EndReason != "time_up" {
		t.Fatalf("강제 종료 후 phase=%s reason=%s", g.Phase, g.EndReason)
	}
	if g.Result == nil || len(g.Result.WinnerSeats) != 1 || g.Result.WinnerSeats[0] != 0 {
		t.Fatalf("강제 종료 승자 = %+v", g.Result)
	}
	// 이미 끝난 게임에 다시 걸어도 결과가 바뀌지 않는다 (SEResult 는 슬라이스를
	// 품어 비교 연산자를 못 쓰므로 내용으로 대조한다)
	before := g.Result
	beforeMsg, beforeWinners := before.Message, len(before.WinnerSeats)
	g.ForceEnd()
	if g.Result != before || g.Result.Message != beforeMsg ||
		len(g.Result.WinnerSeats) != beforeWinners {
		t.Fatalf("중복 강제 종료가 결과를 바꿨다: %+v", g.Result)
	}
}

// TestSEBotChoose 봇의 카드 선택 — 정직 모드는 반드시 세트를,
// 오답 모드는 세트가 아닌 3장을 고른다. 바닥이 3장 미만이면 고르지 않는다.
func TestSEBotChoose(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	deck := seBuildDeck()
	rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })
	board, _, _ := seReplenish([]SECard{}, deck, rng)

	byID := map[int]SECard{}
	for _, c := range board {
		byID[c.ID] = c
	}

	for trial := 0; trial < 50; trial++ {
		ids, ok := seBotChoose(board, false, rng)
		if !ok || len(ids) != SEClaimSize {
			t.Fatalf("정직 모드가 고르지 못했다: %v", ids)
		}
		if !seIsSet(byID[ids[0]], byID[ids[1]], byID[ids[2]]) {
			t.Fatalf("정직 모드가 세트가 아닌 3장을 골랐다: %v", ids)
		}
		if ids[0] == ids[1] || ids[1] == ids[2] || ids[0] == ids[2] {
			t.Fatalf("같은 카드를 두 번 골랐다: %v", ids)
		}

		wrongIDs, ok := seBotChoose(board, true, rng)
		if !ok {
			t.Fatal("오답 모드가 고르지 못했다")
		}
		if seIsSet(byID[wrongIDs[0]], byID[wrongIDs[1]], byID[wrongIDs[2]]) {
			t.Fatalf("오답 모드가 세트를 골랐다: %v", wrongIDs)
		}
	}

	if _, ok := seBotChoose(board[:2], false, rng); ok {
		t.Fatal("2장짜리 바닥에서 골랐다")
	}
	// 세트가 없는 바닥에서는 정직 모드가 포기한다
	if _, ok := seBotChoose(seSetlessFamily(), false, rng); ok {
		t.Fatal("세트 없는 바닥에서 세트를 골랐다")
	}
}

// TestSELobbyRules 대기실 규약 — 1인부터 시작 가능, 8인 상한, 좌석 압축
func TestSELobbyRules(t *testing.T) {
	g := NewSEGame("lobby")
	if g.CanStart() {
		t.Fatal("빈 대기실이 시작 가능하다")
	}
	for i := 0; i < SEMaxPlayers; i++ {
		if _, err := g.AddPlayer(string(rune('A' + i))); err != nil {
			t.Fatalf("%d번째 입장 실패: %v", i, err)
		}
		if !g.CanStart() {
			t.Fatalf("%d명인데 시작 불가", i+1)
		}
	}
	if _, err := g.AddPlayer("초과"); err == nil {
		t.Fatal("정원 초과 입장이 통과했다")
	}

	g.RemovePlayer(0)
	if len(g.Players) != SEMaxPlayers-1 {
		t.Fatalf("퇴장 후 인원 = %d", len(g.Players))
	}
	for i, p := range g.Players {
		if p.Seat != i {
			t.Fatalf("좌석이 압축되지 않았다: %d번째가 seat%d", i, p.Seat)
		}
	}

	rng := rand.New(rand.NewSource(5))
	if err := g.Start(rng); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := g.AddPlayer("난입"); err == nil {
		t.Fatal("시작된 게임에 입장이 통과했다")
	}
	for _, p := range g.Players {
		if p.Score != 0 || p.LockedUntil != 0 {
			t.Fatalf("시작 시 점수·잠금 초기화 실패: %+v", p)
		}
	}
	if g.Deadline != 0 {
		t.Fatalf("순수 규칙이 마감을 스스로 걸었다: %d (허브 담당)", g.Deadline)
	}
}
