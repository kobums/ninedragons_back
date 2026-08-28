package server

import (
	"math/rand"
	"testing"
)

// ==================== 아줄 순수 규칙 테스트 ====================
//
// 벽 색 배치·패턴 라인 배치 규칙·인접 점수·바닥 감점표·최종 보너스를 표로
// 못박는다. 특히 가로+세로가 동시에 이어질 때 둘을 더한다는 규칙과, 벽 25칸의
// 색이 전부 고정이라는 사실이 회귀의 핵심이다.

// azWallFrom "....#" 같은 5줄 그림으로 벽을 만든다 ('#' 채움, '.' 빈 칸)
func azWallFrom(t *testing.T, rows []string) [AZWallSize][AZWallSize]bool {
	t.Helper()
	var wall [AZWallSize][AZWallSize]bool
	if len(rows) != AZWallSize {
		t.Fatalf("벽 그림은 %d줄이어야 합니다: %v", AZWallSize, rows)
	}
	for r, row := range rows {
		if len(row) != AZWallSize {
			t.Fatalf("벽 %d행은 %d칸이어야 합니다: %q", r, AZWallSize, row)
		}
		for c, ch := range row {
			wall[r][c] = ch == '#'
		}
	}
	return wall
}

// ==================== 벽 색 배치 (고정 패턴) ====================

// TestAZWallColorTable 벽 25칸의 색을 전부 못박는다. 각 행이 색 순서를 한 칸씩
// 오른쪽으로 밀어 배치한 결과다 — 이 표가 바뀌면 아줄이 아니다.
func TestAZWallColorTable(t *testing.T) {
	want := [AZWallSize][AZWallSize]AZColor{
		{AZColorBlue, AZColorYellow, AZColorRed, AZColorBlack, AZColorCyan},
		{AZColorCyan, AZColorBlue, AZColorYellow, AZColorRed, AZColorBlack},
		{AZColorBlack, AZColorCyan, AZColorBlue, AZColorYellow, AZColorRed},
		{AZColorRed, AZColorBlack, AZColorCyan, AZColorBlue, AZColorYellow},
		{AZColorYellow, AZColorRed, AZColorBlack, AZColorCyan, AZColorBlue},
	}
	for row := 0; row < AZWallSize; row++ {
		for col := 0; col < AZWallSize; col++ {
			if got := azWallColor(row, col); got != want[row][col] {
				t.Errorf("azWallColor(%d,%d) = %q(%s), want %q(%s)",
					row, col, got, azColorLabel(got),
					want[row][col], azColorLabel(want[row][col]))
			}
		}
	}
}

// TestAZWallColorOutOfRange 범위 밖은 빈 색
func TestAZWallColorOutOfRange(t *testing.T) {
	for _, tc := range [][2]int{{-1, 0}, {0, -1}, {5, 0}, {0, 5}, {9, 9}} {
		if got := azWallColor(tc[0], tc[1]); got != AZColorNone {
			t.Errorf("azWallColor(%d,%d) = %q, want 빈 색", tc[0], tc[1], got)
		}
	}
}

// TestAZWallEachColorOncePerRowAndColumn 어느 행에도, 어느 열에도 5색이
// 정확히 한 번씩 온다 (세로줄 보너스와 색 보너스가 성립하는 근거)
func TestAZWallEachColorOncePerRowAndColumn(t *testing.T) {
	for row := 0; row < AZWallSize; row++ {
		seen := map[AZColor]int{}
		for col := 0; col < AZWallSize; col++ {
			seen[azWallColor(row, col)]++
		}
		if len(seen) != AZWallSize {
			t.Errorf("%d행에 색이 %d종 — 5종이어야 합니다: %v", row, len(seen), seen)
		}
	}
	for col := 0; col < AZWallSize; col++ {
		seen := map[AZColor]int{}
		for row := 0; row < AZWallSize; row++ {
			seen[azWallColor(row, col)]++
		}
		if len(seen) != AZWallSize {
			t.Errorf("%d열에 색이 %d종 — 5종이어야 합니다: %v", col, len(seen), seen)
		}
	}
}

// TestAZWallColIsInverse azWallCol 은 azWallColor 의 역함수다
func TestAZWallColIsInverse(t *testing.T) {
	for row := 0; row < AZWallSize; row++ {
		for _, color := range azColors {
			col := azWallCol(row, color)
			if col < 0 || col >= AZWallSize {
				t.Fatalf("azWallCol(%d,%q) = %d — 범위 밖", row, color, col)
			}
			if got := azWallColor(row, col); got != color {
				t.Errorf("azWallColor(%d, azWallCol(%d,%q)) = %q, want %q",
					row, row, color, got, color)
			}
		}
	}
	if got := azWallCol(0, AZColorFirst); got != -1 {
		t.Errorf("선 마커의 벽 열 = %d, want -1", got)
	}
	if got := azWallCol(9, AZColorBlue); got != -1 {
		t.Errorf("범위 밖 행의 벽 열 = %d, want -1", got)
	}
}

// ==================== 진열대 수 / 바닥 감점표 ====================

func TestAZFactoryCountTable(t *testing.T) {
	table := map[int]int{2: 5, 3: 7, 4: 9}
	for players, want := range table {
		if got := azFactoryCount(players); got != want {
			t.Errorf("%d인 진열대 수 = %d, want %d", players, got, want)
		}
	}
	if got := azFactoryCount(1); got != 0 {
		t.Errorf("표 밖 인원의 진열대 수 = %d, want 0", got)
	}
}

// TestAZFloorPenaltyTable 바닥 감점표 -1,-1,-2,-2,-2,-3,-3 (넘치면 -3 취급)
func TestAZFloorPenaltyTable(t *testing.T) {
	tests := []struct {
		tiles int
		want  int
	}{
		{0, 0}, {1, 1}, {2, 2}, {3, 4}, {4, 6}, {5, 8}, {6, 11}, {7, 14},
		{8, 17}, {9, 20}, // 칸을 넘긴 몫은 장당 -3
	}
	for _, tc := range tests {
		if got := azFloorPenalty(tc.tiles); got != tc.want {
			t.Errorf("azFloorPenalty(%d) = %d, want %d", tc.tiles, got, tc.want)
		}
	}
	if got := azFloorPenalty(-3); got != 0 {
		t.Errorf("음수 장수의 감점 = %d, want 0", got)
	}
}

// ==================== 인접 점수 ====================

// TestAZPlaceScoreTable 붙인 칸 기준 가로·세로 인접 점수.
// 둘 다 이어져 있으면 **둘을 더한다** — 이 게임 점수의 심장이다.
func TestAZPlaceScoreTable(t *testing.T) {
	tests := []struct {
		name     string
		rows     []string
		row, col int
		want     int
	}{
		{"외톨이 타일은 1점", []string{
			".....",
			".....",
			"..#..",
			".....",
			".....",
		}, 2, 2, 1},
		{"가로 3연결", []string{
			"..###",
			".....",
			".....",
			".....",
			".....",
		}, 0, 4, 3},
		{"가로 5연결", []string{
			"#####",
			".....",
			".....",
			".....",
			".....",
		}, 0, 2, 5},
		{"세로 3연결", []string{
			".#...",
			".#...",
			".#...",
			".....",
			".....",
		}, 2, 1, 3},
		{"가로2 + 세로2 = 4점 (합산)", []string{
			".....",
			"..#..",
			".##..",
			".....",
			".....",
		}, 2, 2, 4},
		{"가로3 + 세로2 = 5점 (합산)", []string{
			".....",
			"..#..",
			".###.",
			".....",
			".....",
		}, 2, 2, 5},
		{"가로5 + 세로5 = 10점 (합산)", []string{
			"..#..",
			"..#..",
			"#####",
			"..#..",
			"..#..",
		}, 2, 2, 10},
		{"떨어진 타일은 이어진 것이 아니다", []string{
			"#.#..",
			".....",
			".....",
			".....",
			".....",
		}, 0, 2, 1},
		{"빈 칸을 채점하면 0점", []string{
			".....",
			".....",
			".....",
			".....",
			".....",
		}, 2, 2, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wall := azWallFrom(t, tc.rows)
			if got := azPlaceScore(wall, tc.row, tc.col); got != tc.want {
				t.Errorf("azPlaceScore(%d,%d) = %d, want %d", tc.row, tc.col, got, tc.want)
			}
		})
	}
}

// ==================== 최종 보너스 ====================

// TestAZFinalBonusTable 완성 가로줄 2점 · 세로줄 7점 · 같은 색 5장 10점
func TestAZFinalBonusTable(t *testing.T) {
	// 같은 색(파랑) 5장은 대각선이다 — (0,0)(1,1)(2,2)(3,3)(4,4)
	blueDiagonal := []string{
		"#....",
		".#...",
		"..#..",
		"...#.",
		"....#",
	}
	tests := []struct {
		name                     string
		rows                     []string
		wRows, wCols, wCol, want int
	}{
		{"빈 벽", []string{".....", ".....", ".....", ".....", "....."}, 0, 0, 0, 0},
		{"가로줄 하나", []string{"#####", ".....", ".....", ".....", "....."}, 1, 0, 0, 2},
		{"세로줄 하나", []string{"#....", "#....", "#....", "#....", "#...."}, 0, 1, 0, 7},
		{"같은 색 5장(대각선)", blueDiagonal, 0, 0, 1, 10},
		{"가로 2줄", []string{"#####", "#####", ".....", ".....", "....."}, 2, 0, 0, 4},
		{"벽 전체", []string{"#####", "#####", "#####", "#####", "#####"}, 5, 5, 5, 95},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wall := azWallFrom(t, tc.rows)
			rows, cols, colors, bonus := azFinalBonus(wall)
			if rows != tc.wRows || cols != tc.wCols || colors != tc.wCol || bonus != tc.want {
				t.Errorf("azFinalBonus = 가로%d·세로%d·색%d·합%d, want 가로%d·세로%d·색%d·합%d",
					rows, cols, colors, bonus, tc.wRows, tc.wCols, tc.wCol, tc.want)
			}
		})
	}
}

// TestAZFinalBonusColorIsWallPattern 같은 색 판정은 벽 배치를 따른다 —
// 하늘색 5장은 파랑과 다른 칸에 놓인다
func TestAZFinalBonusColorIsWallPattern(t *testing.T) {
	var wall [AZWallSize][AZWallSize]bool
	for row := 0; row < AZWallSize; row++ {
		wall[row][azWallCol(row, AZColorCyan)] = true
	}
	_, _, colors, bonus := azFinalBonus(wall)
	if colors != 1 || bonus != 10 {
		t.Fatalf("하늘색 5장 보너스 = 색%d·%d점, want 색1·10점", colors, bonus)
	}
}

// ==================== 패턴 라인 배치 규칙 ====================

// azFixture 결정적 테스트용 2인 게임 — 시작 후 진열대·중앙을 손으로 채운다
func azFixture(t *testing.T, names ...string) *AZGame {
	t.Helper()
	g := NewAZGame("test-azul")
	for _, n := range names {
		if _, err := g.AddPlayer(n); err != nil {
			t.Fatalf("AddPlayer(%s): %v", n, err)
		}
	}
	if err := g.Start(rand.New(rand.NewSource(1))); err != nil {
		t.Fatalf("Start: %v", err)
	}
	g.DrainEvents()
	// 진열대·중앙을 비우고 테스트가 원하는 타일만 올린다
	for i := range g.Factories {
		g.Factories[i] = []AZColor{}
	}
	g.Center = []AZColor{}
	g.CenterHasFirst = true
	return g
}

// azKeepDrafting 마지막 진열대에 타일을 남겨 이번 수로 라운드가 끝나지 않게
// 한다. 수주 한 번의 결과(패턴 라인·바닥 라인)만 보려는 테스트용이다 —
// 진열대와 중앙이 다 비면 곧바로 벽 타일 붙이기가 돌아 줄이 비워진다.
func azKeepDrafting(g *AZGame) {
	last := len(g.Factories) - 1
	g.Factories[last] = []AZColor{AZColorYellow, AZColorYellow, AZColorYellow, AZColorYellow}
}

// TestAZCanPlaceRules 패턴 라인 배치 3규칙 — 다른 색 금지 · 벽에 이미 있으면
// 금지 · 가득 찬 줄 금지. -1(전부 바닥)은 언제나 허용된다.
func TestAZCanPlaceRules(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(p *AZPlayer)
		line    int
		color   AZColor
		wantErr bool
	}{
		{"빈 줄에 놓기", func(p *AZPlayer) {}, 2, AZColorBlue, false},
		{"같은 색이 있는 줄에 더 놓기", func(p *AZPlayer) {
			p.Lines[2] = AZLine{Color: AZColorBlue, Count: 1}
		}, 2, AZColorBlue, false},
		{"다른 색이 있는 줄은 금지", func(p *AZPlayer) {
			p.Lines[2] = AZLine{Color: AZColorRed, Count: 1}
		}, 2, AZColorBlue, true},
		{"벽의 그 칸이 이미 채워졌으면 금지", func(p *AZPlayer) {
			p.Wall[2][azWallCol(2, AZColorBlue)] = true
		}, 2, AZColorBlue, true},
		{"가득 찬 줄은 금지", func(p *AZPlayer) {
			p.Lines[2] = AZLine{Color: AZColorBlue, Count: 3}
		}, 2, AZColorBlue, true},
		{"없는 줄 번호는 금지", func(p *AZPlayer) {}, 7, AZColorBlue, true},
		{"바닥 라인(-1)은 언제나 허용", func(p *AZPlayer) {
			p.Lines[2] = AZLine{Color: AZColorRed, Count: 3}
		}, AZLineTargetFloor, AZColorBlue, false},
		{"선 마커 색은 패턴 라인에 못 놓는다", func(p *AZPlayer) {}, 2, AZColorFirst, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &AZPlayer{Floor: []AZColor{}}
			tc.setup(p)
			err := azCanPlace(p, tc.line, tc.color)
			if (err != nil) != tc.wantErr {
				t.Fatalf("azCanPlace = %v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// TestAZTakeFactoryPushesRestToCenter 진열대에서 같은 색 전부를 가져오면
// 나머지는 중앙으로 밀린다
func TestAZTakeFactoryPushesRestToCenter(t *testing.T) {
	g := azFixture(t, "가", "나")
	g.Factories[0] = []AZColor{AZColorBlue, AZColorBlue, AZColorRed, AZColorYellow}

	if err := g.Take(0, "factory:0", AZColorBlue, 1); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if len(g.Factories[0]) != 0 {
		t.Fatalf("진열대가 비지 않았습니다: %v", g.Factories[0])
	}
	if azCountColor(g.Center, AZColorRed) != 1 || azCountColor(g.Center, AZColorYellow) != 1 {
		t.Fatalf("나머지가 중앙으로 밀리지 않았습니다: %v", g.Center)
	}
	p := g.Players[0]
	if p.Lines[1].Color != AZColorBlue || p.Lines[1].Count != 2 {
		t.Fatalf("2번 패턴 라인 = %+v, want 파랑 2장", p.Lines[1])
	}
	if len(p.Floor) != 0 {
		t.Fatalf("바닥 라인이 비어 있어야 합니다: %v", p.Floor)
	}
	if g.CurrentSeat != 1 {
		t.Fatalf("차례가 넘어가지 않았습니다: %d", g.CurrentSeat)
	}
}

// TestAZTakeOverflowsToFloor 줄이 넘치면 넘친 만큼 바닥 라인으로 간다
func TestAZTakeOverflowsToFloor(t *testing.T) {
	g := azFixture(t, "가", "나")
	g.Factories[0] = []AZColor{AZColorRed, AZColorRed, AZColorRed, AZColorRed}
	g.CenterHasFirst = false // 선 마커 감점을 섞지 않는다
	azKeepDrafting(g)

	if err := g.Take(0, "factory:0", AZColorRed, 1); err != nil {
		t.Fatalf("Take: %v", err)
	}
	p := g.Players[0]
	if p.Lines[1].Count != 2 {
		t.Fatalf("2번 패턴 라인 장수 = %d, want 2", p.Lines[1].Count)
	}
	if len(p.Floor) != 2 {
		t.Fatalf("바닥 라인 = %v, want 빨강 2장", p.Floor)
	}
	for _, c := range p.Floor {
		if c != AZColorRed {
			t.Fatalf("바닥 라인 색 = %q, want red", c)
		}
	}
}

// TestAZTakeAllToFloor line=-1 이면 전부 바닥 라인으로 간다
func TestAZTakeAllToFloor(t *testing.T) {
	g := azFixture(t, "가", "나")
	g.Factories[0] = []AZColor{AZColorBlack, AZColorBlack, AZColorBlack, AZColorCyan}
	g.CenterHasFirst = false

	if err := g.Take(0, "factory:0", AZColorBlack, AZLineTargetFloor); err != nil {
		t.Fatalf("Take: %v", err)
	}
	p := g.Players[0]
	if len(p.Floor) != 3 {
		t.Fatalf("바닥 라인 = %v, want 검정 3장", p.Floor)
	}
	for i := range p.Lines {
		if p.Lines[i].Count != 0 {
			t.Fatalf("패턴 라인 %d 가 비어 있지 않습니다: %+v", i, p.Lines[i])
		}
	}
}

// TestAZFloorSlotOverflowIsDiscarded 바닥 라인 7칸을 넘긴 타일은 놓지 않고
// 버린다 (감점은 -3 취급 상한에서 멈춘다)
func TestAZFloorSlotOverflowIsDiscarded(t *testing.T) {
	g := azFixture(t, "가", "나")
	g.CenterHasFirst = false
	p := g.Players[0]
	for i := 0; i < 6; i++ {
		p.Floor = append(p.Floor, AZColorRed)
	}
	g.Factories[0] = []AZColor{AZColorBlue, AZColorBlue, AZColorBlue, AZColorBlue}
	azKeepDrafting(g)
	before := len(g.Discard)

	if err := g.Take(0, "factory:0", AZColorBlue, AZLineTargetFloor); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if len(p.Floor) != AZFloorSlots {
		t.Fatalf("바닥 라인 장수 = %d, want %d", len(p.Floor), AZFloorSlots)
	}
	if len(g.Discard)-before != 3 {
		t.Fatalf("버린 타일 증가 = %d, want 3", len(g.Discard)-before)
	}
}

// TestAZCenterFirstMarker 중앙에서 처음 가져간 사람이 선 플레이어 마커를
// 함께 가져가 바닥 라인에 놓는다 (감점 1). 두 번째 사람은 받지 않는다.
func TestAZCenterFirstMarker(t *testing.T) {
	g := azFixture(t, "가", "나")
	g.Center = []AZColor{AZColorBlue, AZColorRed, AZColorRed}
	g.CenterHasFirst = true
	azKeepDrafting(g)

	if err := g.Take(0, "center", AZColorBlue, 0); err != nil {
		t.Fatalf("Take: %v", err)
	}
	p0 := g.Players[0]
	if len(p0.Floor) != 1 || p0.Floor[0] != AZColorFirst {
		t.Fatalf("바닥 라인 = %v, want 선 플레이어 마커 1개", p0.Floor)
	}
	if g.CenterHasFirst {
		t.Fatal("선 마커가 중앙에 남아 있습니다")
	}
	if g.FirstNextSeat != 0 {
		t.Fatalf("firstNextSeat = %d, want 0", g.FirstNextSeat)
	}

	if err := g.Take(1, "center", AZColorRed, 1); err != nil {
		t.Fatalf("Take(두 번째): %v", err)
	}
	if len(g.Players[1].Floor) != 0 {
		t.Fatalf("두 번째 사람 바닥 라인 = %v, want 비어 있음", g.Players[1].Floor)
	}
	if azFloorPenalty(len(p0.Floor)) != 1 {
		t.Fatalf("선 마커 감점 = %d, want 1", azFloorPenalty(len(p0.Floor)))
	}
}

// TestAZTakeErrors 규약 위반은 상태를 건드리지 않고 에러만 돌려준다
func TestAZTakeErrors(t *testing.T) {
	tests := []struct {
		name  string
		seat  int
		from  string
		color AZColor
		line  int
	}{
		{"차례가 아님", 1, "factory:0", AZColorBlue, 0},
		{"없는 좌석", 5, "factory:0", AZColorBlue, 0},
		{"없는 출처", 0, "factory", AZColorBlue, 0},
		{"없는 진열대", 0, "factory:99", AZColorBlue, 0},
		{"진열대에 없는 색", 0, "factory:0", AZColorCyan, 0},
		{"중앙에 없는 색", 0, "center", AZColorBlue, 0},
		{"선 마커 색은 못 가져온다", 0, "factory:0", AZColorFirst, 0},
		{"다른 색이 있는 줄", 0, "factory:0", AZColorBlue, 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := azFixture(t, "가", "나")
			g.Factories[0] = []AZColor{AZColorBlue, AZColorBlue, AZColorRed, AZColorRed}
			g.Players[0].Lines[3] = AZLine{Color: AZColorRed, Count: 1}
			before := len(g.Factories[0])

			if err := g.Take(tc.seat, tc.from, tc.color, tc.line); err == nil {
				t.Fatal("에러를 기대했지만 성공했습니다")
			}
			if len(g.Factories[0]) != before {
				t.Fatalf("실패한 수가 진열대를 건드렸습니다: %v", g.Factories[0])
			}
			if g.CurrentSeat != 0 {
				t.Fatalf("실패한 수가 차례를 넘겼습니다: %d", g.CurrentSeat)
			}
		})
	}
}

// ==================== 벽 타일 붙이기 정산 ====================

// TestAZTileWallScoring 꽉 찬 패턴 라인만 벽으로 옮기고, 나머지는 버린다.
// 점수는 인접 규칙 그대로 붙는다.
func TestAZTileWallScoring(t *testing.T) {
	g := azFixture(t, "가", "나")
	p := g.Players[0]
	// 3번 줄(3칸)을 빨강으로 채운다 → 벽 (2, azWallCol(2,red)=4)
	p.Lines[2] = AZLine{Color: AZColorRed, Count: 3}
	// 그 왼쪽 칸을 미리 채워 가로 2연결을 만든다
	p.Wall[2][3] = true
	// 4번 줄은 덜 찼으니 그대로 남는다
	p.Lines[3] = AZLine{Color: AZColorBlue, Count: 2}
	discardBefore := len(g.Discard)

	g.tileWall()

	col := azWallCol(2, AZColorRed)
	if col != 4 {
		t.Fatalf("2행 빨강 열 = %d, want 4", col)
	}
	if !p.Wall[2][col] {
		t.Fatal("벽에 타일이 붙지 않았습니다")
	}
	if p.Score != 2 {
		t.Fatalf("점수 = %d, want 2 (가로 2연결)", p.Score)
	}
	if p.Lines[2].Count != 0 || p.Lines[2].Color != AZColorNone {
		t.Fatalf("옮긴 줄이 비워지지 않았습니다: %+v", p.Lines[2])
	}
	if p.Lines[3].Count != 2 || p.Lines[3].Color != AZColorBlue {
		t.Fatalf("덜 찬 줄이 남아야 합니다: %+v", p.Lines[3])
	}
	if len(g.Discard)-discardBefore != 2 {
		t.Fatalf("버린 타일 증가 = %d, want 2 (옮긴 1장 제외한 나머지)",
			len(g.Discard)-discardBefore)
	}
	if g.Phase != AZPhaseTiling {
		t.Fatalf("phase = %q, want tiling", g.Phase)
	}
	if g.RoundResult == nil || len(g.RoundResult.Rows) != 2 {
		t.Fatalf("라운드 정산이 없습니다: %+v", g.RoundResult)
	}
	if g.RoundResult.Rows[0].Gained != 2 || g.RoundResult.Rows[0].Penalty != 0 {
		t.Fatalf("정산 행 = %+v, want 획득2·감점0", g.RoundResult.Rows[0])
	}
}

// TestAZTileWallCombinesRowAndColumn 가로·세로가 동시에 이어지면 합산한다
func TestAZTileWallCombinesRowAndColumn(t *testing.T) {
	g := azFixture(t, "가", "나")
	p := g.Players[0]
	col := azWallCol(2, AZColorBlue) // = 2
	p.Wall[2][col-1] = true
	p.Wall[2][col+1] = true
	p.Wall[1][col] = true
	p.Lines[2] = AZLine{Color: AZColorBlue, Count: 3}

	g.tileWall()

	if p.Score != 5 {
		t.Fatalf("점수 = %d, want 5 (가로3 + 세로2)", p.Score)
	}
}

// TestAZTileWallFloorPenalty 바닥 감점은 표대로, 점수는 0 아래로 안 내려간다
func TestAZTileWallFloorPenalty(t *testing.T) {
	tests := []struct {
		name    string
		start   int
		floor   int
		gainRow bool
		want    int
	}{
		{"감점만 있으면 0에서 멈춘다", 0, 3, false, 0},
		{"기존 점수에서 깎는다", 10, 3, false, 6},
		{"획득과 감점을 함께 반영", 5, 2, true, 4},
		{"바닥 7장은 -14", 20, 7, false, 6},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := azFixture(t, "가", "나")
			p := g.Players[0]
			p.Score = tc.start
			for i := 0; i < tc.floor; i++ {
				p.Floor = append(p.Floor, AZColorRed)
			}
			if tc.gainRow {
				p.Lines[0] = AZLine{Color: AZColorBlue, Count: 1} // 외톨이 1점
			}
			g.tileWall()
			if p.Score != tc.want {
				t.Fatalf("점수 = %d, want %d", p.Score, tc.want)
			}
			if len(p.Floor) != 0 {
				t.Fatalf("바닥 라인이 비워지지 않았습니다: %v", p.Floor)
			}
		})
	}
}

// TestAZFirstMarkerIsNotDiscarded 선 마커는 타일이 아니라 버린 타일에 섞이지
// 않는다 (다음 라운드 준비에서 중앙으로 돌아간다)
func TestAZFirstMarkerIsNotDiscarded(t *testing.T) {
	g := azFixture(t, "가", "나")
	p := g.Players[0]
	p.Floor = []AZColor{AZColorFirst, AZColorRed}
	g.FirstNextSeat = 0
	before := len(g.Discard)

	g.tileWall()

	if len(g.Discard)-before != 1 {
		t.Fatalf("버린 타일 증가 = %d, want 1 (선 마커 제외)", len(g.Discard)-before)
	}
	for _, c := range g.Discard {
		if c == AZColorFirst {
			t.Fatal("선 마커가 버린 타일에 섞였습니다")
		}
	}

	g.AdvanceRound(rand.New(rand.NewSource(2)))
	if !g.CenterHasFirst {
		t.Fatal("다음 라운드 중앙에 선 마커가 없습니다")
	}
	if g.CurrentSeat != 0 {
		t.Fatalf("다음 라운드 선 = seat%d, want seat0", g.CurrentSeat)
	}
	if g.FirstNextSeat != -1 {
		t.Fatalf("firstNextSeat 초기화 실패: %d", g.FirstNextSeat)
	}
}

// TestAZGameEndsOnCompletedRow 가로줄을 완성한 라운드로 게임이 끝나고
// 최종 보너스가 얹힌다
func TestAZGameEndsOnCompletedRow(t *testing.T) {
	g := azFixture(t, "가", "나")
	p := g.Players[0]
	p.Score = 10
	// 0행의 하늘색 칸(열 4)만 비워 두고 나머지를 채운다
	for col := 0; col < AZWallSize-1; col++ {
		p.Wall[0][col] = true
	}
	p.Lines[0] = AZLine{Color: AZColorCyan, Count: 1}

	g.tileWall()
	if g.Phase != AZPhaseTiling {
		t.Fatalf("phase = %q, want tiling (정산을 먼저 보여준다)", g.Phase)
	}

	g.AdvanceRound(rand.New(rand.NewSource(3)))
	if g.Phase != AZPhaseGameOver {
		t.Fatalf("phase = %q, want game_over", g.Phase)
	}
	if g.EndReason != "row_complete" {
		t.Fatalf("사유 = %q, want row_complete", g.EndReason)
	}
	// 가로줄 완성으로 붙는 점수 5점 + 최종 보너스 가로줄 2점
	if p.Score != 17 {
		t.Fatalf("최종 점수 = %d, want 17 (10 + 인접5 + 보너스2)", p.Score)
	}
	if len(g.Bonuses) != 2 || g.Bonuses[0].Rows != 1 || g.Bonuses[0].Bonus != 2 {
		t.Fatalf("보너스 내역 = %+v", g.Bonuses)
	}
	if g.Result == nil || len(g.Result.WinnerSeats) != 1 || g.Result.WinnerSeats[0] != 0 {
		t.Fatalf("결과 = %+v, want seat0 단독 승", g.Result)
	}
}

// TestAZWinnerTieBreakByRows 동점이면 완성 가로줄이 많은 쪽이 이긴다
func TestAZWinnerTieBreakByRows(t *testing.T) {
	players := []*AZPlayer{
		{Seat: 0, Name: "가", Score: 30, Floor: []AZColor{}},
		{Seat: 1, Name: "나", Score: 30, Floor: []AZColor{}},
	}
	for col := 0; col < AZWallSize; col++ {
		players[1].Wall[0][col] = true
	}
	seats, names := azWinners(players)
	if len(seats) != 1 || seats[0] != 1 || names[0] != "나" {
		t.Fatalf("승자 = %v/%v, want seat1 단독", seats, names)
	}

	// 완성 가로줄까지 같으면 공동 승리
	for col := 0; col < AZWallSize; col++ {
		players[0].Wall[0][col] = true
	}
	seats, _ = azWinners(players)
	if len(seats) != 2 {
		t.Fatalf("승자 = %v, want 공동 2명", seats)
	}
}

// ==================== 라운드 준비 / 주머니 ====================

// TestAZBagRefillFromDiscard 주머니가 비면 버린 타일을 섞어 채운다
func TestAZBagRefillFromDiscard(t *testing.T) {
	g := azFixture(t, "가", "나")
	g.Bag = []AZColor{}
	g.Discard = []AZColor{}
	for i := 0; i < 24; i++ {
		g.Discard = append(g.Discard, AZColorBlack)
	}

	dealt := g.fillFactories(rand.New(rand.NewSource(4)))
	if dealt != 20 { // 2인 5개 × 4장
		t.Fatalf("채운 타일 = %d, want 20", dealt)
	}
	if len(g.Discard) != 0 {
		t.Fatalf("버린 타일이 남았습니다: %d장", len(g.Discard))
	}
	if len(g.Bag) != 4 {
		t.Fatalf("주머니 = %d장, want 4", len(g.Bag))
	}
}

// TestAZTilesExhaustedEndsGame 주머니와 버린 타일이 모두 마르면 게임이 끝난다
func TestAZTilesExhaustedEndsGame(t *testing.T) {
	g := azFixture(t, "가", "나")
	g.Bag = []AZColor{}
	g.Discard = []AZColor{}
	g.Phase = AZPhaseTiling
	g.RoundResult = &AZRoundResult{Rows: []AZRoundRow{}, Message: "정산"}

	g.AdvanceRound(rand.New(rand.NewSource(5)))
	if g.Phase != AZPhaseGameOver {
		t.Fatalf("phase = %q, want game_over", g.Phase)
	}
	if g.EndReason != "tiles_exhausted" {
		t.Fatalf("사유 = %q, want tiles_exhausted", g.EndReason)
	}
}

// TestAZStartDealsFactories 시작하면 인원수에 맞는 진열대에 4장씩 깔린다
func TestAZStartDealsFactories(t *testing.T) {
	for _, n := range []int{2, 3, 4} {
		g := NewAZGame("t")
		for i := 0; i < n; i++ {
			if _, err := g.AddPlayer(string(rune('가' + i))); err != nil {
				t.Fatalf("AddPlayer: %v", err)
			}
		}
		if err := g.Start(rand.New(rand.NewSource(int64(n)))); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if len(g.Factories) != azFactoryCount(n) {
			t.Fatalf("%d인 진열대 = %d개, want %d개", n, len(g.Factories), azFactoryCount(n))
		}
		for i, f := range g.Factories {
			if len(f) != AZFactoryTiles {
				t.Fatalf("%d인 %d번 진열대 = %d장, want %d장", n, i, len(f), AZFactoryTiles)
			}
		}
		if !g.CenterHasFirst || len(g.Center) != 0 {
			t.Fatalf("%d인 시작 중앙 = %v (선 마커 %v), want 빈 중앙 + 선 마커",
				n, g.Center, g.CenterHasFirst)
		}
		if len(g.Bag) != 100-azFactoryCount(n)*AZFactoryTiles {
			t.Fatalf("%d인 주머니 = %d장", n, len(g.Bag))
		}
	}
}

// ==================== 합법 수 / AFK 자동 수 ====================

// TestAZLegalMovesAlwaysHasFloorOption 타일이 남아 있으면 합법 수는 반드시
// 하나 이상이다 (-1 전부 바닥 덕분에 교착이 없다)
func TestAZLegalMovesAlwaysHasFloorOption(t *testing.T) {
	g := azFixture(t, "가", "나")
	g.Factories[0] = []AZColor{AZColorBlue, AZColorBlue, AZColorBlue, AZColorBlue}
	p := g.Players[0]
	// 파랑을 놓을 수 있는 줄을 전부 막는다
	for row := 0; row < AZWallSize; row++ {
		p.Wall[row][azWallCol(row, AZColorBlue)] = true
	}

	moves := azLegalMoves(g, 0)
	if len(moves) != 1 || moves[0].Line != AZLineTargetFloor {
		t.Fatalf("합법 수 = %+v, want 바닥 라인 하나", moves)
	}
}

// TestAZSafestMoveMinimizesPenalty AFK 자동 수는 감점이 가장 적은 수를 고른다
func TestAZSafestMoveMinimizesPenalty(t *testing.T) {
	g := azFixture(t, "가", "나")
	g.CenterHasFirst = false
	// 0번 진열대는 검정 4장(대부분 바닥행), 1번은 빨강 1장(1번 줄에 딱 맞는다)
	g.Factories[0] = []AZColor{AZColorBlack, AZColorBlack, AZColorBlack, AZColorBlack}
	g.Factories[1] = []AZColor{AZColorRed, AZColorCyan, AZColorCyan, AZColorCyan}

	mv, ok := azSafestMove(g, 0)
	if !ok {
		t.Fatal("자동 수를 찾지 못했습니다")
	}
	out, ok := azEvalMove(g, 0, mv)
	if !ok {
		t.Fatalf("자동 수 평가 실패: %+v", mv)
	}
	if out.PenaltyDelta != 0 {
		t.Fatalf("자동 수 감점 = %d (%+v), want 0", out.PenaltyDelta, mv)
	}
	if out.Placed == 0 {
		t.Fatalf("감점이 같으면 패턴 라인에 더 많이 놓는 수를 골라야 합니다: %+v", mv)
	}
}

// TestAZForceMoveAdvances ForceMove 는 실제로 수를 두고 차례를 넘긴다
func TestAZForceMoveAdvances(t *testing.T) {
	g := azFixture(t, "가", "나")
	g.Factories[0] = []AZColor{AZColorBlue, AZColorRed, AZColorRed, AZColorYellow}

	if !g.ForceMove() {
		t.Fatal("ForceMove 가 수를 두지 못했습니다")
	}
	if g.CurrentSeat != 1 {
		t.Fatalf("차례 = %d, want 1", g.CurrentSeat)
	}
	if g.LastAction == nil || g.LastAction.Seat != 0 {
		t.Fatalf("lastAction = %+v", g.LastAction)
	}
}

// TestAZEvalMovePreview 수의 결과 미리보기(넘침·감점·완성)가 실제 결과와 같다
func TestAZEvalMovePreview(t *testing.T) {
	g := azFixture(t, "가", "나")
	g.CenterHasFirst = false
	g.Factories[0] = []AZColor{AZColorRed, AZColorRed, AZColorRed, AZColorBlue}

	out, ok := azEvalMove(g, 0, AZMove{From: "factory:0", Color: AZColorRed, Line: 1})
	if !ok {
		t.Fatal("평가 실패")
	}
	if out.Took != 3 || out.Placed != 2 || out.Overflow != 1 {
		t.Fatalf("미리보기 = %+v, want 가져오기3·배치2·넘침1", out)
	}
	if out.PenaltyDelta != 1 {
		t.Fatalf("감점 미리보기 = %d, want 1", out.PenaltyDelta)
	}
	if !out.Completes || out.Row != 1 || out.Col != azWallCol(1, AZColorRed) {
		t.Fatalf("완성 미리보기 = %+v", out)
	}

	if err := g.Take(0, "factory:0", AZColorRed, 1); err != nil {
		t.Fatalf("Take: %v", err)
	}
	p := g.Players[0]
	if p.Lines[1].Count != 2 || len(p.Floor) != 1 {
		t.Fatalf("실제 결과가 미리보기와 다릅니다: 줄%+v 바닥%v", p.Lines[1], p.Floor)
	}
}

// ==================== 봇 품질 ====================

// azBotBoardOf 순수 상태를 봇 평가용 보드로 옮긴다 (측정 하니스용)
func azBotBoardOf(p *AZPlayer) azBotBoard {
	return azBotBoard{Lines: p.Lines, Wall: p.Wall, Floor: len(p.Floor)}
}

// azPlayBotGame 봇끼리 한 판을 끝까지 둔다 (허브·소켓 없이 순수 규칙만).
// 최종 점수와 라운드 수를 돌려준다.
func azPlayBotGame(t *testing.T, seed int64, players int) ([]int, int) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	g := NewAZGame("bench")
	for i := 0; i < players; i++ {
		if _, err := g.AddPlayer(string(rune('가' + i))); err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
	}
	if err := g.Start(rng); err != nil {
		t.Fatalf("Start: %v", err)
	}

	rounds := 0
	for step := 0; g.Phase != AZPhaseGameOver; step++ {
		if step > 20000 {
			t.Fatalf("판이 끝나지 않습니다 (phase=%s round=%d)", g.Phase, g.Round)
		}
		g.DrainEvents()
		switch g.Phase {
		case AZPhaseDrafting:
			seat := g.CurrentSeat
			mv, ok := azBotChoose(g.Factories, g.Center, g.CenterHasFirst,
				azBotBoardOf(g.Players[seat]), rng)
			if !ok {
				t.Fatalf("봇이 둘 수를 찾지 못했습니다 (round=%d seat=%d)", g.Round, seat)
			}
			if err := g.Take(seat, mv.From, mv.Color, mv.Line); err != nil {
				t.Fatalf("봇이 불법 수를 냈습니다 %+v: %v", mv, err)
			}
		case AZPhaseTiling:
			rounds = g.Round
			g.AdvanceRound(rng)
		}
	}

	scores := []int{}
	for _, p := range g.Players {
		scores = append(scores, p.Score)
	}
	return scores, rounds
}

// TestAZBotQuality 3봇 30판의 평균 점수와 라운드 수를 숫자로 남긴다.
// 사람은 보통 40~70점이므로 평균 20점 미만이면 평가 함수가 잘못된 것이다.
func TestAZBotQuality(t *testing.T) {
	const games = 30
	totalScore, totalRounds, best, worst := 0, 0, 0, 1<<31-1
	seats := 0

	for i := 0; i < games; i++ {
		scores, rounds := azPlayBotGame(t, int64(9000+i), 3)
		totalRounds += rounds
		for _, s := range scores {
			totalScore += s
			seats++
			if s > best {
				best = s
			}
			if s < worst {
				worst = s
			}
		}
	}

	avgScore := float64(totalScore) / float64(seats)
	avgRounds := float64(totalRounds) / float64(games)
	t.Logf("[봇 품질] 3봇 %d판 | 평균 점수 %.1f (최고 %d · 최저 %d) | 평균 라운드 %.1f",
		games, avgScore, best, worst, avgRounds)

	if avgScore < 20 {
		t.Fatalf("평균 점수 %.1f — 20점 미만이면 평가 함수를 고쳐야 합니다", avgScore)
	}
	if avgRounds < 3 || avgRounds > 12 {
		t.Fatalf("평균 라운드 %.1f — 정상 범위(3~12)를 벗어났습니다", avgRounds)
	}
}

// TestAZBotNeverPlacesIllegally 봇의 선택은 언제나 규칙에 맞는다
func TestAZBotNeverPlacesIllegally(t *testing.T) {
	rng := rand.New(rand.NewSource(777))
	for i := 0; i < 5; i++ {
		g := NewAZGame("legal")
		g.AddPlayer("가")
		g.AddPlayer("나")
		if err := g.Start(rng); err != nil {
			t.Fatalf("Start: %v", err)
		}
		for step := 0; g.Phase != AZPhaseGameOver && step < 5000; step++ {
			g.DrainEvents()
			if g.Phase == AZPhaseTiling {
				g.AdvanceRound(rng)
				continue
			}
			seat := g.CurrentSeat
			mv, ok := azBotChoose(g.Factories, g.Center, g.CenterHasFirst,
				azBotBoardOf(g.Players[seat]), rng)
			if !ok {
				t.Fatalf("봇이 수를 못 찾음 (round=%d)", g.Round)
			}
			if err := azCanPlace(g.Players[seat], mv.Line, mv.Color); err != nil {
				t.Fatalf("봇이 불법 배치를 골랐습니다 %+v: %v", mv, err)
			}
			if err := g.Take(seat, mv.From, mv.Color, mv.Line); err != nil {
				t.Fatalf("봇 수 거부 %+v: %v", mv, err)
			}
		}
	}
}
