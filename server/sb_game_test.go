package server

import (
	"encoding/json"
	"math/rand"
	"testing"
	"time"
)

// ==================== 테스트용 타일 모양 ====================

var (
	sbTCross  = sbTile(true, true, true, true, false)   // 십자
	sbTHoriz  = sbTile(false, true, false, true, false) // 가로 직선
	sbTVert   = sbTile(true, false, true, false, false) // 세로 직선
	sbTCurveL = sbTile(true, false, false, true, false) // 굽이 ┛ (위·왼)
	sbTCurveR = sbTile(true, true, false, false, false) // 굽이 ┗ (위·오른)
	sbTDeadL  = sbTile(false, false, false, true, true) // 막다른 ← (왼쪽만)
	// sbTDeadLR 좌우가 뚫려 보이지만 내부가 막힌 막다른 타일.
	// 덱에는 없지만 "변은 맞는데 길은 안 이어진다"를 검증하는 데 쓴다.
	sbTDeadLR = sbTile(false, true, false, true, true)
)

// sbPut 테스트용 강제 배치 (규칙 검증 없이 판을 원하는 모양으로 만든다)
type sbPut struct {
	col, row int
	tile     SBCard
}

// sbBuild 시작·목표 타일이 깔린 새 판에 지정한 타일들을 강제로 놓는다
func sbBuild(puts ...sbPut) []*SBCell {
	board := sbNewBoard()
	for _, p := range puts {
		board[sbIdx(p.col, p.row)] = &SBCell{
			Col: p.col, Row: p.row, Kind: SBTilePath,
			Up: p.tile.Up, Right: p.tile.Right, Down: p.tile.Down, Left: p.tile.Left,
			Dead: p.tile.Dead, GoalIndex: -1,
		}
	}
	return board
}

// sbRow2 (1,2)부터 col 까지 가로로 이어진 길 (시작 타일에서 곧게 뻗는다)
func sbRow2(to int, extra ...sbPut) []*SBCell {
	puts := []sbPut{}
	for c := 1; c <= to; c++ {
		puts = append(puts, sbPut{c, 2, sbTCross})
	}
	return sbBuild(append(puts, extra...)...)
}

// ==================== 경로 연결 판정 (핵심) ====================

// TestSBCanPlaceTable 배치 판정의 성립·불성립을 표로 못 박는다.
// 세 관문(빈 칸·변 일치·시작 타일에서 이어진 길과의 접속)이 각각 독립적으로
// 걸리는지 확인한다.
func TestSBCanPlaceTable(t *testing.T) {
	ok := []struct {
		name     string
		board    []*SBCell
		tile     SBCard
		col, row int
	}{
		{"시작 타일 오른쪽에 가로 직선", sbBuild(), sbTHoriz, 1, 2},
		{"시작 타일 위에 세로 직선", sbBuild(), sbTVert, 0, 1},
		{"시작 타일 아래에 세로 직선", sbBuild(), sbTVert, 0, 3},
		{"시작 타일 오른쪽에 굽이 ┛", sbBuild(), sbTCurveL, 1, 2},
		{"이어진 길 끝에 십자", sbBuild(sbPut{1, 2, sbTHoriz}), sbTCross, 2, 2},
		{"이어진 길 끝에 막다른 타일", sbBuild(sbPut{1, 2, sbTHoriz}), sbTDeadL, 2, 2},
		{"십자 위로 갈라지는 세로 직선", sbBuild(sbPut{1, 2, sbTCross}), sbTVert, 1, 1},
		{"긴 길 끝에서 위로 꺾기", sbRow2(7), sbTCross, 7, 1},
		{"목표 타일 사이 칸 — 이웃 규칙이 느슨하다", sbRow2(7, sbPut{7, 1, sbTCross}), sbTHoriz, 8, 1},
		{"두 이웃과 모두 변이 맞는 자리", sbBuild(sbPut{1, 2, sbTCross}, sbPut{2, 1, sbTCross}), sbTCross, 2, 2},
	}
	for _, tc := range ok {
		if err := sbCanPlace(tc.board, tc.tile, tc.col, tc.row); err != nil {
			t.Errorf("[성립] %s: (%d,%d) 거부됨 — %v", tc.name, tc.col, tc.row, err)
		}
	}

	bad := []struct {
		name     string
		board    []*SBCell
		tile     SBCard
		col, row int
	}{
		{"판 오른쪽 밖", sbBuild(), sbTCross, SBCols, 2},
		{"판 위쪽 밖", sbBuild(), sbTCross, 0, -1},
		{"시작 타일 자리", sbBuild(), sbTCross, SBStartCol, SBStartRow},
		{"목표 타일 자리", sbBuild(), sbTCross, 8, 0},
		{"이미 놓인 칸", sbBuild(sbPut{1, 2, sbTCross}), sbTCross, 1, 2},
		{"변 불일치 — 벽이 통로와 만난다", sbBuild(), sbTVert, 1, 2},
		{"변 불일치 — 통로가 벽과 만난다", sbBuild(sbPut{1, 2, sbTHoriz}), sbTCross, 1, 1},
		{"이웃이 하나도 없는 고립 칸", sbBuild(), sbTCross, 5, 0},
		{"대각선으로만 닿은 칸", sbBuild(sbPut{1, 2, sbTCross}), sbTCross, 2, 1},
		{"막다른 타일 뒤 — 변은 맞지만 길이 안 이어진다",
			sbBuild(sbPut{1, 2, sbTDeadLR}), sbTHoriz, 2, 2},
		{"끊긴 길 옆 — 변은 맞지만 시작 타일과 무관하다",
			sbBuild(sbPut{4, 4, sbTCross}), sbTCross, 3, 4},
		{"길 타일이 아닌 카드", sbBuild(), SBCard{Kind: SBCardRockfall}, 1, 2},
	}
	for _, tc := range bad {
		if err := sbCanPlace(tc.board, tc.tile, tc.col, tc.row); err == nil {
			t.Errorf("[불성립] %s: (%d,%d) 통과됨", tc.name, tc.col, tc.row)
		}
	}
	if len(ok) < 8 || len(bad) < 8 {
		t.Fatalf("표가 부족하다: 성립 %d · 불성립 %d", len(ok), len(bad))
	}
}

// TestSBReachability 시작 타일 BFS — 막다른 타일 뒤와 끊긴 길은 집합에서
// 빠지고, 낙석으로 타일이 사라지면 그 뒤가 통째로 떨어져 나간다.
func TestSBReachability(t *testing.T) {
	// 빈 판: 시작 타일 하나뿐
	empty := sbBuild()
	if got := len(sbReachable(empty)); got != 1 {
		t.Fatalf("빈 판 도달 칸 = %d, want 1", got)
	}
	if got := sbFrontier(empty); got != SBStartCol {
		t.Fatalf("빈 판 최전선 = %d, want %d", got, SBStartCol)
	}

	// 곧게 뻗은 길: 시작 + 4칸
	road := sbRow2(4)
	reach := sbReachable(road)
	if len(reach) != 5 {
		t.Fatalf("직선 길 도달 칸 = %d, want 5", len(reach))
	}
	for c := 1; c <= 4; c++ {
		if !reach[sbIdx(c, 2)] {
			t.Fatalf("(%d,2)가 도달 집합에 없다", c)
		}
	}
	if got := sbFrontier(road); got != 4 {
		t.Fatalf("최전선 = %d, want 4", got)
	}

	// 막다른 타일 뒤: (2,2)까지만 닿고 (3,2)는 떨어진다
	blocked := sbBuild(
		sbPut{1, 2, sbTHoriz}, sbPut{2, 2, sbTDeadLR}, sbPut{3, 2, sbTHoriz})
	bReach := sbReachable(blocked)
	if !bReach[sbIdx(2, 2)] {
		t.Fatal("막다른 타일 자체는 도달 집합에 있어야 한다")
	}
	if bReach[sbIdx(3, 2)] {
		t.Fatal("막다른 타일 뒤가 도달 집합에 들어갔다")
	}

	// 끊긴 길: 붙어 있지 않으면 아무리 모양이 맞아도 도달하지 못한다
	split := sbBuild(sbPut{1, 2, sbTHoriz}, sbPut{3, 2, sbTHoriz}, sbPut{4, 2, sbTHoriz})
	sReach := sbReachable(split)
	if sReach[sbIdx(3, 2)] || sReach[sbIdx(4, 2)] {
		t.Fatal("끊긴 구간이 도달 집합에 들어갔다")
	}

	// 목표 타일 닿음 — 길이 (8,2)를 향해 뻗으면 그 타일만 닿는다
	if got := sbTouchedGoals(sbRow2(6)); len(got) != 0 {
		t.Fatalf("아직 안 닿았는데 %v", got)
	}
	touched := sbTouchedGoals(sbRow2(7))
	if len(touched) != 1 || touched[0] != 1 {
		t.Fatalf("닿은 목표 = %v, want [1]", touched)
	}

	// 낙석으로 중간 타일이 사라지면 그 뒤가 통째로 떨어진다
	cut := sbRow2(7)
	cut[sbIdx(3, 2)] = nil
	if len(sbTouchedGoals(cut)) != 0 {
		t.Fatal("길이 끊겼는데 목표에 여전히 닿아 있다")
	}
	if got := sbFrontier(cut); got != 2 {
		t.Fatalf("낙석 후 최전선 = %d, want 2", got)
	}
}

// TestSBFlipAndShapes 180° 회전과 회전 가능 판정
func TestSBFlipAndShapes(t *testing.T) {
	if sbTCross.Flipable || sbTHoriz.Flipable || sbTVert.Flipable {
		t.Fatal("대칭 타일이 회전 가능으로 표시됐다")
	}
	if !sbTCurveL.Flipable || !sbTDeadL.Flipable {
		t.Fatal("비대칭 타일이 회전 불가로 표시됐다")
	}
	flipped := sbFlip(sbTCurveL) // 위·왼 → 아래·오른
	if !flipped.Down || !flipped.Right || flipped.Up || flipped.Left {
		t.Fatalf("회전 결과 = %+v", flipped)
	}
	if sbFlip(sbFlip(sbTCurveL)) != sbTCurveL {
		t.Fatal("두 번 회전하면 제자리여야 한다")
	}

	// 회전을 써야만 놓을 수 있는 자리 — 굽이 ┗(위·오른)는 그대로는 못 놓는다
	board := sbBuild()
	if sbCanPlace(board, sbTCurveR, 1, 2) == nil {
		t.Fatal("왼쪽이 막힌 굽이가 시작 타일 오른쪽에 그냥 놓였다")
	}
	if err := sbCanPlace(board, sbFlip(sbTCurveR), 1, 2); err != nil {
		t.Fatalf("회전한 굽이가 거부됐다: %v", err)
	}
}

// TestSBDeckComposition 덱 구성표 — 40장, 길 24장(막다른 4장)·행동 16장
func TestSBDeckComposition(t *testing.T) {
	deck := sbBuildDeck()
	if len(deck) != SBDeckSize {
		t.Fatalf("덱 = %d장, want %d", len(deck), SBDeckSize)
	}
	count := map[SBCardKind]int{}
	tools := map[SBCardKind]map[SBTool]int{
		SBCardBreak: {}, SBCardRepair: {},
	}
	for _, c := range deck {
		count[c.Kind]++
		if c.Kind == SBCardBreak || c.Kind == SBCardRepair {
			if !sbToolValid(c.Tool) {
				t.Fatalf("파괴·수리 카드에 장비가 없다: %+v", c)
			}
			tools[c.Kind][c.Tool]++
		}
		if sbIsTile(c) && c.Dead != (c.Kind == SBCardDeadend) {
			t.Fatalf("막다른 표시와 종류가 어긋난다: %+v", c)
		}
	}
	want := map[SBCardKind]int{
		SBCardPath: 25, SBCardDeadend: 4,
		SBCardMap: 2, SBCardRockfall: 2, SBCardBreak: 4, SBCardRepair: 3,
	}
	for kind, n := range want {
		if count[kind] != n {
			t.Fatalf("%s = %d장, want %d", kind, count[kind], n)
		}
	}
	// 막다른 4종은 방향이 하나씩 — 어느 쪽으로도 길이 이어지지 않는다
	if len(tools[SBCardBreak]) != 3 || len(tools[SBCardRepair]) != 3 {
		t.Fatalf("파괴·수리 카드가 장비 3종을 덮지 않는다: %v", tools)
	}
}

// TestSBRolePools 인원별 역할 풀 — 풀은 항상 N+1 장이라 실제 파괴꾼 수가
// 확정되지 않는다 (크라켄과 같은 장치)
func TestSBRolePools(t *testing.T) {
	wantSab := map[int]int{3: 1, 4: 1, 5: 2, 6: 2, 7: 3, 8: 3, 9: 3, 10: 3}
	for n := SBMinPlayers; n <= SBMaxPlayers; n++ {
		pool := sbRolePoolFor(n)
		if pool.Saboteur != wantSab[n] {
			t.Fatalf("%d인 파괴꾼 풀 = %d, want %d", n, pool.Saboteur, wantSab[n])
		}
		if pool.Miner+pool.Saboteur != n+1 {
			t.Fatalf("%d인 풀 합 = %d, want %d (구성 불확정의 근거)",
				n, pool.Miner+pool.Saboteur, n+1)
		}
	}
	if got := sbRolePoolFor(2); got != (SBRolePool{}) {
		t.Fatalf("표 밖 인원 풀 = %+v, want 0:0", got)
	}

	// 실제 배분은 풀보다 적거나 같다 — 남은 1장이 무엇인지 아무도 모른다
	g := sbNewTestGame(t, 5)
	sab := 0
	for _, p := range g.Players {
		if p.Role == SBRoleSaboteur {
			sab++
		}
	}
	if sab > sbRolePoolFor(5).Saboteur {
		t.Fatalf("배분된 파괴꾼 %d명이 풀 %d명을 넘었다", sab, sbRolePoolFor(5).Saboteur)
	}
}

// sbNewTestGame 결정적 난수로 시작된 n인 게임
func sbNewTestGame(t *testing.T, n int) *SBGame {
	t.Helper()
	g := NewSBGame("test")
	for i := 0; i < n; i++ {
		if _, err := g.AddPlayer(string(rune('A' + i))); err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
	}
	if err := g.Start(rand.New(rand.NewSource(7))); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return g
}

// TestSBStartDeal 시작 배분 — 손패 장수·장비 상태·금 위치가 규칙대로인가
func TestSBStartDeal(t *testing.T) {
	g := sbNewTestGame(t, 5)
	if g.Phase != SBPhasePlaying || !g.Ready {
		t.Fatalf("phase = %s", g.Phase)
	}
	per := sbHandSize(5)
	for _, p := range g.Players {
		if len(p.Hand) != per {
			t.Fatalf("seat%d 손패 = %d장, want %d", p.Seat, len(p.Hand), per)
		}
		if !p.Tools.sbToolsAllOK() {
			t.Fatalf("seat%d 장비가 처음부터 망가졌다: %+v", p.Seat, p.Tools)
		}
	}
	if len(g.Deck) != SBDeckSize-5*per {
		t.Fatalf("덱 잔량 = %d", len(g.Deck))
	}
	if g.GoldIndex < 0 || g.GoldIndex >= len(sbGoalCells) {
		t.Fatalf("금 위치 = %d", g.GoldIndex)
	}
	golds := 0
	for i, gc := range sbGoalCells {
		cell := g.Board[sbIdx(gc[0], gc[1])]
		if cell == nil || cell.Kind != SBTileGoal {
			t.Fatalf("목표 타일 %d 부재", i)
		}
		if cell.Revealed {
			t.Fatalf("목표 타일 %d 가 처음부터 공개됐다", i)
		}
		if i == g.GoldIndex {
			golds++
		}
	}
	if golds != 1 {
		t.Fatalf("금덩이 = %d개, want 1", golds)
	}
}

// TestSBToolsBlockPlacement 장비가 하나라도 망가지면 길 타일을 놓을 수 없다
// (행동 카드와 버리기는 그대로 가능하다)
func TestSBToolsBlockPlacement(t *testing.T) {
	g := sbNewTestGame(t, 3)
	seat := g.CurrentSeat
	p := g.Players[seat]
	p.Hand = []SBCard{sbTHoriz, {Kind: SBCardRepair, Tool: SBToolPick}}
	p.Tools.set(SBToolPick, false)

	if err := g.Place(seat, 0, 1, 2, false); err == nil {
		t.Fatal("장비가 망가졌는데 배치가 통과했다")
	}
	// 수리는 가능하다 — 자기 자신 대상
	if err := g.Action(seat, 1, SBActionPayload{Index: 1, TargetSeat: seat}); err != nil {
		t.Fatalf("수리 실패: %v", err)
	}
	if !p.Tools.Pick {
		t.Fatal("수리 후에도 곡괭이가 망가져 있다")
	}
	// 이미 멀쩡한 장비는 못 고친다 / 이미 망가진 장비는 또 못 망가뜨린다
	g.CurrentSeat = seat
	p.Hand = []SBCard{{Kind: SBCardRepair, Tool: SBToolPick}, {Kind: SBCardBreak, Tool: SBToolCart}}
	if err := g.Action(seat, 0, SBActionPayload{TargetSeat: seat}); err == nil {
		t.Fatal("멀쩡한 장비를 고쳤다")
	}
	if err := g.Action(seat, 1, SBActionPayload{TargetSeat: seat}); err != nil {
		t.Fatalf("파괴 실패: %v", err)
	}
	if p.Tools.Cart {
		t.Fatal("수레가 안 망가졌다")
	}
}

// TestSBRockfallAndMap 낙석은 놓인 길 타일만 걷어내고, 지도는 방송되지 않는
// 개인 통지로만 나간다
func TestSBRockfallAndMap(t *testing.T) {
	g := sbNewTestGame(t, 3)
	seat := g.CurrentSeat
	p := g.Players[seat]
	p.Hand = []SBCard{sbTHoriz}
	if err := g.Place(seat, 0, 1, 2, false); err != nil {
		t.Fatalf("배치 실패: %v", err)
	}
	g.CurrentSeat = seat
	p.Hand = []SBCard{{Kind: SBCardRockfall}, {Kind: SBCardRockfall}, {Kind: SBCardMap}}

	// 시작·목표 타일은 걷어낼 수 없다
	if err := g.Action(seat, 0, SBActionPayload{Col: SBStartCol, Row: SBStartRow}); err == nil {
		t.Fatal("시작 타일이 걷혔다")
	}
	if err := g.Action(seat, 0, SBActionPayload{Col: 8, Row: 0}); err == nil {
		t.Fatal("목표 타일이 걷혔다")
	}
	if err := g.Action(seat, 0, SBActionPayload{Col: 1, Row: 2}); err != nil {
		t.Fatalf("낙석 실패: %v", err)
	}
	if g.Board[sbIdx(1, 2)] != nil {
		t.Fatal("낙석 후에도 타일이 남아 있다")
	}

	// 지도 — 결과는 이벤트가 아니라 개인 통지다
	g.CurrentSeat = seat
	g.DrainEvents()
	mapIdx := -1
	for i, c := range p.Hand {
		if c.Kind == SBCardMap {
			mapIdx = i
		}
	}
	if mapIdx < 0 {
		t.Fatal("지도 카드가 손에서 사라졌다")
	}
	gc := sbGoalCells[g.GoldIndex]
	if err := g.Action(seat, mapIdx, SBActionPayload{Col: gc[0], Row: gc[1]}); err != nil {
		t.Fatalf("지도 실패: %v", err)
	}
	privates := g.DrainPrivates()
	if len(privates) != 1 || privates[0].Seat != seat ||
		privates[0].Index != g.GoldIndex || !privates[0].Gold {
		t.Fatalf("지도 개인 통지 = %+v", privates)
	}
	for _, ev := range g.DrainEvents() {
		if ev.Kind == "map" && (containsRune(ev.Message, '금') || containsRune(ev.Message, '돌')) {
			t.Fatalf("지도 결과가 방송에 새어 나갔다: %s", ev.Message)
		}
	}
	// 이미 공개된 목표 타일은 볼 수 없다
	g.CurrentSeat = seat
	g.Board[sbIdx(gc[0], gc[1])].Revealed = true
	p.Hand = append(p.Hand, SBCard{Kind: SBCardMap})
	if err := g.Action(seat, len(p.Hand)-1, SBActionPayload{Col: gc[0], Row: gc[1]}); err == nil {
		t.Fatal("공개된 목표 타일을 또 들여다봤다")
	}
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}

// TestSBGoalRevealAndWin 길이 목표 타일에 닿으면 그 타일만 열리고, 금이면
// 그 자리에서 광부 승리다. 돌덩이면 게임은 계속된다.
func TestSBGoalRevealAndWin(t *testing.T) {
	for _, goldIndex := range []int{0, 1, 2} {
		g := sbNewTestGame(t, 3)
		g.GoldIndex = goldIndex
		g.Board = sbRow2(6)
		seat := g.CurrentSeat
		g.Players[seat].Hand = []SBCard{sbTCross}
		if err := g.Place(seat, 0, 7, 2, false); err != nil {
			t.Fatalf("배치 실패: %v", err)
		}

		mid := g.Board[sbIdx(8, 2)]
		if !mid.Revealed {
			t.Fatal("길이 닿은 목표 타일이 안 열렸다")
		}
		for _, other := range [][2]int{{8, 0}, {8, 4}} {
			if g.Board[sbIdx(other[0], other[1])].Revealed {
				t.Fatalf("닿지 않은 (%d,%d) 목표 타일이 열렸다", other[0], other[1])
			}
		}
		if goldIndex == 1 {
			if g.Phase != SBPhaseGameOver || g.Result == nil ||
				g.Result.Winner != string(SBRoleMiner) || g.Result.Reason != "gold" {
				t.Fatalf("금을 찾았는데 result = %+v (phase %s)", g.Result, g.Phase)
			}
			if g.Result.GoldIndex != 1 {
				t.Fatalf("종료 goldIndex = %d", g.Result.GoldIndex)
			}
		} else {
			if g.Phase != SBPhasePlaying {
				t.Fatalf("돌덩이인데 게임이 끝났다: %+v", g.Result)
			}
			if mid.Gold {
				t.Fatal("돌덩이 타일에 금 표시가 붙었다")
			}
		}
		// 공개된 목표 타일은 사방이 뚫린 모양으로 회전 보정된다
		if !mid.Up || !mid.Right || !mid.Down || !mid.Left {
			t.Fatalf("공개된 목표 타일 모양 = %+v", mid)
		}
	}
}

// TestSBExhaustedSaboteurWin 카드가 다 떨어지면 파괴꾼 승리 — 매 차례 손패가
// 1장씩 영구히 줄어드니 반드시 도달한다
func TestSBExhaustedSaboteurWin(t *testing.T) {
	g := sbNewTestGame(t, 3)
	g.Deck = nil
	for _, p := range g.Players {
		p.Hand = []SBCard{{Kind: SBCardMap}}
	}
	guard := 0
	for g.Phase == SBPhasePlaying && guard < 20 {
		seat := g.CurrentSeat
		if err := g.Discard(seat, 0); err != nil {
			t.Fatalf("버리기 실패: %v", err)
		}
		guard++
	}
	if g.Phase != SBPhaseGameOver {
		t.Fatalf("손패를 다 썼는데 안 끝났다 (%d차례)", guard)
	}
	if g.Result.Winner != string(SBRoleSaboteur) || g.Result.Reason != "exhausted" {
		t.Fatalf("result = %+v", g.Result)
	}
	if guard != 3 {
		t.Fatalf("차례 수 = %d, want 3", guard)
	}
}

// TestSBTurnOrder 손패가 빈 좌석은 차례를 건너뛴다
func TestSBTurnOrder(t *testing.T) {
	g := sbNewTestGame(t, 4)
	g.Deck = nil
	g.CurrentSeat = 0
	for _, p := range g.Players {
		p.Hand = []SBCard{{Kind: SBCardMap}}
	}
	g.Players[1].Hand = []SBCard{}
	g.Players[2].Hand = []SBCard{}
	if err := g.Discard(0, 0); err != nil {
		t.Fatalf("버리기 실패: %v", err)
	}
	if g.CurrentSeat != 3 {
		t.Fatalf("빈 손패 좌석을 안 건너뛰었다: current=%d", g.CurrentSeat)
	}
}

// TestSBGoldHiddenInSnapshot 목표 타일의 gold 는 공개 전까지 어떤 스냅샷에도
// 실리지 않는다 (본인·타인·관전자 모두 raw JSON 에 키 부재)
func TestSBGoldHiddenInSnapshot(t *testing.T) {
	h := NewSBHub()
	room := h.lobbyRoomFor("")
	for i := 0; i < 3; i++ {
		c := &SBClient{wsClient: newBotWSClient(), Hub: h}
		c.Bot = false
		c.Name = string(rune('A' + i))
		seat, _ := room.Game.AddPlayer(c.Name)
		c.GameID, c.Seat = room.Game.ID, seat
		room.Clients[seat] = c
		h.sessions[c.SessionID] = c
	}
	h.startGame(room)
	game := room.Game

	for _, viewer := range []int{0, 1, 2, -1} {
		raw, err := json.Marshal(h.buildSBState(room, viewer))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if containsSub(string(raw), `"gold"`) {
			t.Fatalf("viewer %d 스냅샷에 gold 키 유출:\n%s", viewer, raw)
		}
	}

	// 돌덩이 목표 타일이 열리면 그 타일만 gold:false 로 나간다
	rock := (game.GoldIndex + 1) % len(sbGoalCells)
	gc := sbGoalCells[rock]
	cell := game.Board[sbIdx(gc[0], gc[1])]
	cell.Revealed, cell.Gold = true, false
	raw, _ := json.Marshal(h.buildSBState(room, -1))
	if !containsSub(string(raw), `"gold":false`) {
		t.Fatalf("공개된 돌덩이에 gold:false 가 없다:\n%s", raw)
	}
	if containsSub(string(raw), `"gold":true`) {
		t.Fatalf("아직 안 열린 금이 새어 나갔다:\n%s", raw)
	}
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ==================== 봇 품질 측정 ====================

// sbBotSimResult 봇 대전 30판의 집계
type sbBotSimResult struct {
	Games      int
	MinerWins  int
	SabWins    int
	TotalTurns int
	Rejected   int
	MaxTurns   int
}

// sbRunBotSeries 5봇 게임을 games 판 돌려 승률·평균 차례를 잰다.
// 실제 허브 경로(handleGameMessage → 순수 규칙 → buildSBState)를 그대로 쓰고
// 봇 두뇌도 실물이다 — 연결만 없어서 전송이 버려질 뿐이다.
func sbRunBotSeries(t *testing.T, games int) sbBotSimResult {
	t.Helper()
	// 차례 마감 타이머가 끼어들지 않게 아주 길게 잡는다 (허브 고루틴 없이 돈다)
	saved := sbTurnTimeout
	sbTurnTimeout = time.Hour
	defer func() { sbTurnTimeout = saved }()

	out := sbBotSimResult{Games: games}
	for gameNo := 0; gameNo < games; gameNo++ {
		h := NewSBHub()
		h.rng = rand.New(rand.NewSource(int64(gameNo)*7919 + 101))
		room := h.lobbyRoomFor("")

		clients := make([]*SBClient, SBFillBotTarget)
		brains := make([]*sbBrain, SBFillBotTarget)
		for i := range clients {
			c := &SBClient{wsClient: newBotWSClient(), Hub: h}
			// 소켓 없이 직접 구동한다 — 전송은 sendTo 에서 버려진다
			c.Connected = false
			c.Name = botName + string(rune('1'+i))
			seat, err := room.Game.AddPlayer(c.Name)
			if err != nil {
				t.Fatalf("AddPlayer: %v", err)
			}
			c.GameID, c.Seat = room.Game.ID, seat
			room.Clients[seat] = c
			h.sessions[c.SessionID] = c
			clients[i] = c
			brains[i] = &sbBrain{rng: rand.New(rand.NewSource(int64(gameNo*100+i) + 5))}
		}
		h.startGame(room)
		game := room.Game

		guard := 0
		for game.Phase == SBPhasePlaying && guard < SBDeckSize+10 {
			seat := game.CurrentSeat
			before := game.Turns
			state, ok := botPayloadAs[sbBotState](h.buildSBState(room, seat))
			if !ok {
				t.Fatal("봇 스냅샷 변환 실패")
			}
			if msg := brains[seat].decideState(state); msg != nil {
				h.handleGameMessage(SBGameMessage{Client: clients[seat], Message: *msg})
			}
			if game.Phase == SBPhasePlaying && game.Turns == before {
				// 봇이 낼 수 없는 수를 냈거나 판단을 포기했다 — 강제로 버린다
				out.Rejected++
				h.handleGameMessage(SBGameMessage{Client: clients[seat],
					Message: SBMessage{Type: SBMsgDiscard, Payload: SBDiscardPayload{Index: 0}}})
			}
			guard++
		}
		if game.Phase != SBPhaseGameOver || game.Result == nil {
			t.Fatalf("%d번째 판이 %d차례 안에 끝나지 않았다", gameNo, guard)
		}
		if game.Result.Winner == string(SBRoleMiner) {
			out.MinerWins++
		} else {
			out.SabWins++
		}
		out.TotalTurns += game.Turns
		if game.Turns > out.MaxTurns {
			out.MaxTurns = game.Turns
		}
	}
	return out
}

// TestSBBotBalance 5봇 30판의 광부·파괴꾼 승률과 평균 소요 차례. 한쪽이
// 90% 이상 이기면 봇 정책이나 덱 구성이 무너진 것이므로 실패시킨다
// (더 크루에서 봇이 협력을 못 해 전멸하던 전례의 회귀 장치).
func TestSBBotBalance(t *testing.T) {
	sbSilenceBotDelay(t)
	const games = 30
	res := sbRunBotSeries(t, games)

	minerRate := float64(res.MinerWins) / float64(res.Games) * 100
	sabRate := float64(res.SabWins) / float64(res.Games) * 100
	avgTurns := float64(res.TotalTurns) / float64(res.Games)
	t.Logf("5봇 %d판 — 광부 %d승(%.1f%%) · 파괴꾼 %d승(%.1f%%) | 평균 %.1f차례 (최대 %d) | 반려 %d수",
		res.Games, res.MinerWins, minerRate, res.SabWins, sabRate,
		avgTurns, res.MaxTurns, res.Rejected)

	if minerRate >= 90 || sabRate >= 90 {
		t.Fatalf("한쪽이 압도한다 — 광부 %.1f%% · 파괴꾼 %.1f%%", minerRate, sabRate)
	}
	if avgTurns < 5 {
		t.Fatalf("평균 %.1f차례는 너무 짧다 — 진행이 망가졌을 수 있다", avgTurns)
	}
	if res.Rejected > res.Games {
		t.Fatalf("봇이 낼 수 없는 수를 %d번 냈다 (판당 1회 초과)", res.Rejected)
	}
}

// sbSilenceBotDelay 봇의 "생각하는 시간"을 끈다 (지연은 순수 연출)
func sbSilenceBotDelay(t *testing.T) {
	t.Helper()
	delay, jitter := sbBotDelay, sbBotJitterMs
	sbBotDelay, sbBotJitterMs = 0, 0
	t.Cleanup(func() { sbBotDelay, sbBotJitterMs = delay, jitter })
}
