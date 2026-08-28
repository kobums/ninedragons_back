package server

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// ==================== 리코셰 로봇 순수 규칙 테스트 ====================
//
// 이 게임의 핵심은 퍼즐 검증이다. 미끄러짐(rrSlide)과 최소 횟수(rrSolve)가
// 틀리면 판 생성·봇·증명 판정이 한꺼번에 무너지므로, 손으로 푼 판을 정답과
// 함께 박아 두고 표로 훑는다.

// rrTestBoard 벽 없는 기본 판 — 가장자리 + 중앙 2×2 막힌 구역만 있다.
// 손으로 푼 판의 바탕이며, 여기에 테스트가 벽을 직접 얹는다.
func rrTestBoard() *RRBoard {
	b := &RRBoard{}
	for i := 0; i < RRSize; i++ {
		b.Walls[rrIndex(0, i)] |= rrWallUp
		b.Walls[rrIndex(RRSize-1, i)] |= rrWallDown
		b.Walls[rrIndex(i, 0)] |= rrWallLeft
		b.Walls[rrIndex(i, RRSize-1)] |= rrWallRight
	}
	for _, pos := range rrCenterCells() {
		b.Blocked[pos] = true
	}
	for _, pos := range rrCenterCells() {
		for dir := 0; dir < 4; dir++ {
			n := rrNextCell[dir][pos]
			if n < 0 || b.Blocked[n] {
				continue
			}
			rrAddWall(b, pos, dir)
		}
	}
	return b
}

// rrAt (행,열) → 칸 색인 (테스트 가독성용)
func rrAt(r, c int) uint8 { return uint8(rrIndex(r, c)) }

// rrRobotsAt 로봇 넷을 (행,열) 네 쌍으로 배치 — 순서는 빨강·파랑·초록·노란색
func rrRobotsAt(cells ...[2]int) [RRRobotCount]uint8 {
	var out [RRRobotCount]uint8
	for i, cell := range cells {
		out[i] = rrAt(cell[0], cell[1])
	}
	return out
}

// rrShow 칸 색인을 "행,열" 로 (실패 문구용)
func rrShow(pos uint8) string {
	r, c := rrRowCol(pos)
	return fmt.Sprintf("(%d,%d)", r, c)
}

// ==================== 미끄러짐 ====================

// TestRRSlideTable 미끄러짐 판정 표 — 가장자리·벽·다른 로봇·중앙 막힌 구역·
// 제자리(움직일 수 없음)를 한 표에서 훑는다. 이 게임의 규칙은 사실상 이게
// 전부라 가장 촘촘하게 잡아 둔다.
func TestRRSlideTable(t *testing.T) {
	// 벽을 얹은 판: (3,6) 오른쪽 벽 / (1,5) 위쪽 벽 / (1,2) 왼쪽 벽
	walled := rrTestBoard()
	rrAddWall(walled, rrIndex(3, 6), 1) // right
	rrAddWall(walled, rrIndex(1, 5), 0) // up
	rrAddWall(walled, rrIndex(1, 2), 3) // left

	far := [][2]int{{12, 12}, {12, 13}, {12, 14}} // 간섭하지 않는 대기 위치

	cases := []struct {
		name   string
		board  *RRBoard
		robots [RRRobotCount]uint8
		robot  int
		dir    RRDir
		want   [2]int
	}{
		{"가장자리까지 위로", rrTestBoard(),
			rrRobotsAt([2]int{7, 3}, far[0], far[1], far[2]), 0, RRUp, [2]int{0, 3}},
		{"가장자리까지 아래로", rrTestBoard(),
			rrRobotsAt([2]int{7, 3}, far[0], far[1], far[2]), 0, RRDown, [2]int{15, 3}},
		{"가장자리까지 왼쪽으로", rrTestBoard(),
			rrRobotsAt([2]int{7, 3}, far[0], far[1], far[2]), 0, RRLeft, [2]int{7, 0}},
		{"중앙 막힌 구역 앞에서 멈춤", rrTestBoard(),
			rrRobotsAt([2]int{7, 3}, far[0], far[1], far[2]), 0, RRRight, [2]int{7, 6}},
		{"중앙 막힌 구역을 반대편에서 만나도 멈춤", rrTestBoard(),
			rrRobotsAt([2]int{8, 14}, far[0], far[1], far[2]), 0, RRLeft, [2]int{8, 9}},
		{"다른 로봇 앞에서 멈춤", rrTestBoard(),
			rrRobotsAt([2]int{3, 3}, [2]int{3, 10}, far[1], far[2]), 0, RRRight, [2]int{3, 9}},
		{"바로 옆 로봇에 막혀 제자리", rrTestBoard(),
			rrRobotsAt([2]int{3, 3}, [2]int{3, 4}, far[1], far[2]), 0, RRRight, [2]int{3, 3}},
		{"가장자리를 등지면 제자리", rrTestBoard(),
			rrRobotsAt([2]int{0, 3}, far[0], far[1], far[2]), 0, RRUp, [2]int{0, 3}},
		{"벽 앞에서 멈춤", walled,
			rrRobotsAt([2]int{3, 3}, far[0], far[1], far[2]), 0, RRRight, [2]int{3, 6}},
		{"벽을 등지면 제자리", walled,
			rrRobotsAt([2]int{3, 6}, far[0], far[1], far[2]), 0, RRRight, [2]int{3, 6}},
		{"벽 반대편에서 오면 그 칸에 들어가 멈춘다", walled,
			rrRobotsAt([2]int{3, 12}, far[0], far[1], far[2]), 0, RRLeft, [2]int{3, 7}},
		{"위쪽 벽에 걸려 멈춤", walled,
			rrRobotsAt([2]int{5, 5}, far[0], far[1], far[2]), 0, RRUp, [2]int{1, 5}},
		{"왼쪽 벽에 걸려 멈춤", walled,
			rrRobotsAt([2]int{1, 9}, far[0], far[1], far[2]), 0, RRLeft, [2]int{1, 2}},
		{"파란색 로봇도 같은 규칙", rrTestBoard(),
			rrRobotsAt(far[0], [2]int{5, 5}, far[1], far[2]), 1, RRLeft, [2]int{5, 0}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rrSlide(tc.board, tc.robots, tc.robot, rrDirIndex(tc.dir))
			if got != rrAt(tc.want[0], tc.want[1]) {
				t.Fatalf("%s 로봇 %s → %s, want (%d,%d)",
					rrColorLabel(rrColors[tc.robot]), rrDirLabel(tc.dir),
					rrShow(got), tc.want[0], tc.want[1])
			}
		})
	}
}

// TestRRSlideChecksBothSidesOfWall 벽이 한쪽 칸에만 새겨져 있어도 벽이다.
//
// 프론트의 고스트 미리보기가 "이 칸의 벽 비트 + 이웃 칸의 반대편 벽 비트"를
// 함께 보므로, 서버의 rrSlide 도 같은 규칙이어야 미리보기와 서버 판정이
// 어긋나지 않는다. 정상 판은 rrAddWall 로 양쪽에 새기지만, 한쪽만 새겨진
// 판을 일부러 만들어 두 방향 모두 같은 답이 나오는지 고정한다.
func TestRRSlideChecksBothSidesOfWall(t *testing.T) {
	far := [][2]int{{12, 12}, {12, 13}, {12, 14}}

	// (a) 출발 칸 쪽에만 새긴 벽 — walls[(3,6)] 의 오른쪽 비트만 선다
	onlyNear := rrTestBoard()
	onlyNear.Walls[rrIndex(3, 6)] |= rrWallRight

	// (b) 이웃 칸 쪽에만 새긴 벽 — walls[(3,7)] 의 왼쪽 비트만 선다
	onlyFar := rrTestBoard()
	onlyFar.Walls[rrIndex(3, 7)] |= rrWallLeft

	robots := rrRobotsAt([2]int{3, 3}, far[0], far[1], far[2])
	for _, tc := range []struct {
		name  string
		board *RRBoard
	}{{"출발 칸에만 새긴 벽", onlyNear}, {"이웃 칸에만 새긴 벽", onlyFar}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rrSlide(tc.board, robots, 0, rrDirIndex(RRRight)); got != rrAt(3, 6) {
				t.Fatalf("오른쪽 → %s, want (3,6)", rrShow(got))
			}
			// 반대 방향에서 와도 같은 경계에서 멈춘다
			back := rrRobotsAt([2]int{3, 12}, far[0], far[1], far[2])
			if got := rrSlide(tc.board, back, 0, rrDirIndex(RRLeft)); got != rrAt(3, 7) {
				t.Fatalf("왼쪽 → %s, want (3,7)", rrShow(got))
			}
		})
	}

	// rrWallBetween 자체도 양쪽에서 같은 답이어야 한다
	for _, b := range []*RRBoard{onlyNear, onlyFar} {
		if !rrWallBetween(b, rrAt(3, 6), rrDirIndex(RRRight)) {
			t.Fatal("(3,6) 오른쪽이 벽으로 보이지 않는다")
		}
		if !rrWallBetween(b, rrAt(3, 7), rrDirIndex(RRLeft)) {
			t.Fatal("(3,7) 왼쪽이 벽으로 보이지 않는다")
		}
	}
}

// ==================== BFS 최소 횟수 ====================

// TestRRSolveHandChecked 손으로 푼 판 — 정답을 그대로 박아 둔다.
//
// 판은 rrTestBoard 에 벽 두 장을 얹은 것이다.
//
//	(1,5) 위쪽 벽 · (1,2) 왼쪽 벽, 빨간색 로봇은 (5,5)
//
// 벽 없는 판에서 한 번 움직이면 반드시 가장자리(또는 중앙 구역 앞)까지 가므로
// 도달 가능한 칸을 손으로 셀 수 있다 —
//
//	1회: (1,5) 위 / (15,5) 아래 / (5,0) 왼쪽 / (5,15) 오른쪽
//	2회: (1,5)에서 왼쪽 → (1,2), 그 밖에 네 귀퉁이와 가장자리 몇 곳
//	3회: (1,2)에서 아래 → (15,2)
func TestRRSolveHandChecked(t *testing.T) {
	board := rrTestBoard()
	rrAddWall(board, rrIndex(1, 5), 0) // up
	rrAddWall(board, rrIndex(1, 2), 3) // left
	robots := rrRobotsAt([2]int{5, 5}, [2]int{12, 12}, [2]int{12, 13}, [2]int{12, 14})

	cases := []struct {
		name string
		goal RRGoal
		want int
	}{
		{"이미 목표 지점에 있다 (0회)", RRGoal{Color: RRRed, R: 5, C: 5}, 0},
		{"오른쪽 한 번 (1회)", RRGoal{Color: RRRed, R: 5, C: 15}, 1},
		{"위쪽 벽에 걸려 한 번 (1회)", RRGoal{Color: RRRed, R: 1, C: 5}, 1},
		{"위 → 왼쪽 (2회)", RRGoal{Color: RRRed, R: 1, C: 2}, 2},
		{"위 → 왼쪽 → 아래 (3회)", RRGoal{Color: RRRed, R: 15, C: 2}, 3},
		{"파란색 로봇을 왼쪽 끝으로 (1회)", RRGoal{Color: RRBlue, R: 12, C: 0}, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			moves, ok := rrSolve(board, robots, tc.goal, RRMaxDepth)
			if !ok {
				t.Fatalf("해를 못 찾았다 (want %d회)", tc.want)
			}
			if len(moves) != tc.want {
				t.Fatalf("최소 횟수 = %d회 %v, want %d회", len(moves), moves, tc.want)
			}
			// 찾은 경로를 실제로 적용하면 목표 지점에 닿아야 한다
			after, err := rrApplyMoves(board, robots, moves)
			if err != nil {
				t.Fatalf("경로 적용 실패: %v", err)
			}
			ci := rrColorIndex(tc.goal.Color)
			if after[ci] != rrAt(tc.goal.R, tc.goal.C) {
				t.Fatalf("경로 끝 = %s, want (%d,%d)", rrShow(after[ci]), tc.goal.R, tc.goal.C)
			}
		})
	}
}

// TestRRSolveNoSolution 해가 없는 경우 — 중앙 막힌 구역은 어떤 로봇도 들어갈
// 수 없고, maxDepth 0 이면 제자리 말고는 아무것도 못 한다.
func TestRRSolveNoSolution(t *testing.T) {
	board := rrTestBoard()
	robots := rrRobotsAt([2]int{5, 5}, [2]int{12, 12}, [2]int{12, 13}, [2]int{12, 14})

	center := rrCenterCells()[0]
	r, c := rrRowCol(uint8(center))
	if _, ok := rrSolve(board, robots, RRGoal{Color: RRRed, R: r, C: c}, RRMaxDepth); ok {
		t.Fatal("중앙 막힌 구역에 도달했다고 답했다")
	}
	if _, ok := rrSolve(board, robots, RRGoal{Color: RRRed, R: 5, C: 15}, 0); ok {
		t.Fatal("maxDepth 0 인데 해를 찾았다")
	}
	if _, ok := rrSolve(board, robots, RRGoal{Color: "purple", R: 3, C: 3}, RRMaxDepth); ok {
		t.Fatal("없는 색을 목표로 받아들였다")
	}
}

// rrRefSolve 대조용 참조 풀이 — 반복 심화 DFS 로 최소 횟수를 구한다.
// rrSolve(BFS + 예산)와 완전히 다른 방식이라, 둘이 늘 같은 답을 내면
// 최적화(방문 맵·노드 예산)가 답을 망가뜨리지 않았다는 뜻이다.
func rrRefSolve(b *RRBoard, robots [RRRobotCount]uint8, goal RRGoal, maxDepth int) (int, bool) {
	ci := rrColorIndex(goal.Color)
	if ci < 0 {
		return 0, false
	}
	target := uint8(rrIndex(goal.R, goal.C))

	var dfs func(cur [RRRobotCount]uint8, left int) bool
	dfs = func(cur [RRRobotCount]uint8, left int) bool {
		if cur[ci] == target {
			return true
		}
		if left == 0 {
			return false
		}
		for robot := 0; robot < RRRobotCount; robot++ {
			for dir := 0; dir < 4; dir++ {
				dest := rrSlide(b, cur, robot, dir)
				if dest == cur[robot] {
					continue
				}
				next := cur
				next[robot] = dest
				if dfs(next, left-1) {
					return true
				}
			}
		}
		return false
	}

	for depth := 0; depth <= maxDepth; depth++ {
		if dfs(robots, depth) {
			return depth, true
		}
	}
	return 0, false
}

// TestRRSolveMatchesReference 무작위 판에서 BFS 와 참조 DFS 가 같은 최소
// 횟수를 내는지. 깊이가 얕은 목표만 골라 참조 쪽이 터지지 않게 한다.
func TestRRSolveMatchesReference(t *testing.T) {
	rng := rand.New(rand.NewSource(20260828))
	checked := 0
	for i := 0; i < 12; i++ {
		board := rrGenerateBoard(rng)
		robots := rrPlaceRobots(board, rng)
		depths := rrGoalDepths(board, robots, rrGenNodeBudget)

		for ci := 0; ci < RRRobotCount && checked < 60; ci++ {
			for pos := 0; pos < RRCellCount && checked < 60; pos++ {
				d := int(depths[ci][pos])
				if d < 1 || d > 4 || rng.Intn(40) != 0 {
					continue
				}
				r, c := pos/RRSize, pos%RRSize
				goal := RRGoal{Color: rrColors[ci], R: r, C: c}

				moves, ok := rrSolve(board, robots, goal, RRMaxDepth)
				if !ok || len(moves) != d {
					t.Fatalf("BFS 최소 횟수 = %v(%d), 목표 깊이 = %d", ok, len(moves), d)
				}
				ref, refOK := rrRefSolve(board, robots, goal, 4)
				if !refOK || ref != d {
					t.Fatalf("참조 풀이 = %v(%d), BFS = %d — 두 풀이가 어긋난다",
						refOK, ref, d)
				}
				checked++
			}
		}
	}
	if checked < 20 {
		t.Fatalf("대조한 목표가 %d개뿐이다", checked)
	}
	t.Logf("BFS ↔ 참조 DFS 대조 %d건 일치", checked)
}

// ==================== 판 생성 ====================

// rrAssertBoardInvariants 판이 지켜야 할 것 — 벽은 양쪽 칸에 같이 새겨져
// 있고, 가장자리는 전부 막혀 있고, 중앙 2×2 는 진입 불가이며, 어떤 칸도
// 사방이 막혀 있지 않다 (로봇이 갇히는 칸을 만들지 않는다).
func rrAssertBoardInvariants(t *testing.T, b *RRBoard) {
	t.Helper()
	for pos := 0; pos < RRCellCount; pos++ {
		r, c := pos/RRSize, pos%RRSize
		for dir := 0; dir < 4; dir++ {
			n := rrNextCell[dir][pos]
			if n < 0 {
				if b.Walls[pos]&rrDirBit[dir] == 0 {
					t.Fatalf("(%d,%d) 판 밖 방향 %s 에 벽이 없다", r, c, rrDirLabel(rrDirs[dir]))
				}
				continue
			}
			mine := b.Walls[pos]&rrDirBit[dir] != 0
			theirs := b.Walls[n]&rrDirBit[rrOpposite[dir]] != 0
			if mine != theirs {
				t.Fatalf("(%d,%d) %s 벽이 한쪽에만 새겨졌다", r, c, rrDirLabel(rrDirs[dir]))
			}
		}
		if !b.Blocked[pos] && rrWallCount(b.Walls[pos]) >= 4 {
			t.Fatalf("(%d,%d) 사방이 막혀 로봇이 갇힌다", r, c)
		}
	}
	for _, pos := range rrCenterCells() {
		if !b.Blocked[pos] {
			t.Fatalf("중앙 %s 가 진입 가능하다", rrShow(uint8(pos)))
		}
	}
}

// TestRRBoardGenerationDifficulty 판 생성이 **항상 최소 2~10회짜리 목표를
// 낸다**는 보증 — 100회 반복해서 확인한다. 1회짜리는 시시하고 11회 이상은
// 사람이 못 푼다. 뽑아 낸 최소 횟수가 rrSolve 의 답과 정확히 같은지, 그
// 경로가 실제로 목표 지점에 닿는지도 함께 본다.
func TestRRBoardGenerationDifficulty(t *testing.T) {
	rng := rand.New(rand.NewSource(20260828))
	dist := map[int]int{}
	rerolls := 0

	for i := 0; i < 100; i++ {
		board := rrGenerateBoard(rng)
		rrAssertBoardInvariants(t, board)

		robots := rrPlaceRobots(board, rng)
		used := map[int]bool{}

		var goal RRGoal
		var min int
		ok := false
		for attempt := 0; attempt < 40 && !ok; attempt++ {
			goal, min, ok = rrDrawGoal(board, robots, used, rng)
			if !ok {
				robots = rrPlaceRobots(board, rng)
				rerolls++
			}
		}
		if !ok {
			t.Fatalf("%d번째 판에서 조건에 맞는 목표를 못 뽑았다", i)
		}

		if min < RRMinGoalMoves || min > RRMaxGoalMoves {
			t.Fatalf("%d번째 판 최소 횟수 = %d회 (허용 %d~%d)",
				i, min, RRMinGoalMoves, RRMaxGoalMoves)
		}
		dist[min]++

		// 목표 지점이 진입 불가 칸이면 애초에 도달할 수 없다
		if board.Blocked[rrIndex(goal.R, goal.C)] {
			t.Fatalf("%d번째 판 목표 지점이 막힌 칸이다: %v", i, goal)
		}

		moves, solved := rrSolve(board, robots, goal, RRMaxDepth)
		if !solved {
			t.Fatalf("%d번째 판: 생성은 %d회라 했는데 rrSolve 가 못 풀었다", i, min)
		}
		if len(moves) != min {
			t.Fatalf("%d번째 판: 생성 %d회 vs rrSolve %d회 — 두 경로가 어긋난다",
				i, min, len(moves))
		}
		after, err := rrApplyMoves(board, robots, moves)
		if err != nil {
			t.Fatalf("%d번째 판 경로 적용 실패: %v", i, err)
		}
		if after[rrColorIndex(goal.Color)] != rrAt(goal.R, goal.C) {
			t.Fatalf("%d번째 판 경로가 목표 지점에 닿지 않는다", i)
		}
	}
	t.Logf("100판 최소 횟수 분포 %v (로봇 재배치 %d회)", dist, rerolls)
}

// TestRRGoalUniqueness 같은 (색, 칸) 목표가 한 판에 두 번 나오지 않는다
func TestRRGoalUniqueness(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	board := rrGenerateBoard(rng)
	robots := rrPlaceRobots(board, rng)
	used := map[int]bool{}
	seen := map[RRGoal]bool{}

	for i := 0; i < RRGoalTotal; i++ {
		goal, _, ok := rrDrawGoal(board, robots, used, rng)
		if !ok {
			t.Fatalf("%d번째 목표를 못 뽑았다", i)
		}
		if seen[goal] {
			t.Fatalf("%v 목표 지점이 두 번 나왔다", goal)
		}
		seen[goal] = true
		used[rrColorIndex(goal.Color)*RRCellCount+rrIndex(goal.R, goal.C)] = true
	}
}

// ==================== 증명 판정 ====================

// TestRRJudgeDemoTable 증명 판정 표 — 정확히 맞춘 경우, 외친 횟수보다 적게
// 쓴 경우, 넘긴 경우, 목표 지점에 못 닿은 경우, 못 움직이는 방향을 낸 경우.
func TestRRJudgeDemoTable(t *testing.T) {
	board := rrTestBoard()
	rrAddWall(board, rrIndex(1, 5), 0) // up
	rrAddWall(board, rrIndex(1, 2), 3) // left
	robots := rrRobotsAt([2]int{5, 5}, [2]int{12, 12}, [2]int{12, 13}, [2]int{12, 14})
	goal := RRGoal{Color: RRRed, R: 15, C: 2} // 손으로 확인한 3회짜리

	solution := []RRMove{
		{Robot: RRRed, Dir: RRUp},
		{Robot: RRRed, Dir: RRLeft},
		{Robot: RRRed, Dir: RRDown},
	}
	oneMove := []RRMove{{Robot: RRRed, Dir: RRLeft}} // (5,0) — 목표 지점이 아니다

	cases := []struct {
		name    string
		moves   []RRMove
		bid     int
		wantOK  bool
		wantMsg string
	}{
		{"정확히 외친 횟수로 성공", solution, 3, true, rrDemoOKMsg},
		{"외친 횟수보다 적게 써도 성공", solution, 5, true, rrDemoOKMsg},
		{"외친 횟수를 넘기면 실패", solution, 2, false, rrDemoOverMsg},
		{"목표 지점에 못 닿으면 실패", oneMove, 3, false, rrDemoMissMsg},
		{"빈 증명은 실패", []RRMove{}, 3, false, rrDemoMissMsg},
		{"못 움직이는 방향은 실패",
			[]RRMove{{Robot: RRRed, Dir: RRUp}, {Robot: RRRed, Dir: RRUp}}, 3,
			false, rrDemoStuckMsg},
		{"없는 색은 실패",
			[]RRMove{{Robot: "purple", Dir: RRUp}}, 3, false, rrDemoBadMoveMsg},
		{"없는 방향은 실패",
			[]RRMove{{Robot: RRRed, Dir: "sideways"}}, 3, false, rrDemoBadMoveMsg},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			after, ok, msg := rrJudgeDemo(board, robots, goal, tc.moves, tc.bid)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v (%s), want %v", ok, msg, tc.wantOK)
			}
			if !strings.Contains(msg, tc.wantMsg) {
				t.Fatalf("문구 = %q, want %q 포함", msg, tc.wantMsg)
			}
			if !ok && after != robots {
				t.Fatal("실패한 증명이 로봇 위치를 바꿨다")
			}
			if ok && after[rrColorIndex(goal.Color)] != rrAt(goal.R, goal.C) {
				t.Fatalf("성공인데 로봇이 목표 지점에 없다: %s",
					rrShow(after[rrColorIndex(goal.Color)]))
			}
		})
	}
}

// ==================== 외침 / 증명 순서 ====================

// rrNewStartedGame 좌석 n 개로 시작한 게임 (결정적 시드)
func rrNewStartedGame(t *testing.T, seats int, seed int64) (*RRGame, *rand.Rand) {
	t.Helper()
	g := NewRRGame("test")
	for i := 0; i < seats; i++ {
		if _, err := g.AddPlayer(fmt.Sprintf("P%d", i)); err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
	}
	rng := rand.New(rand.NewSource(seed))
	if err := g.Start(rng); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if g.Phase != RRPhaseThinking {
		t.Fatalf("시작 단계 = %s", g.Phase)
	}
	return g, rng
}

// TestRRBidRules 외침 규칙 — 첫 외침이 카운트다운을 열고, 자기 외침은 더
// 적은 횟수로만 고칠 수 있고, 같은 횟수는 먼저 도착한 쪽이 앞선다.
func TestRRBidRules(t *testing.T) {
	g, _ := rrNewStartedGame(t, 3, 11)

	if err := g.Bid(0, 0); err == nil {
		t.Fatal("0회 외침이 통과했다")
	}
	if err := g.Bid(0, RRMaxBid+1); err == nil {
		t.Fatal("상한을 넘는 외침이 통과했다")
	}

	if err := g.Bid(0, 5); err != nil {
		t.Fatalf("첫 외침: %v", err)
	}
	if g.Phase != RRPhaseBidding {
		t.Fatalf("첫 외침 뒤 단계 = %s, want bidding", g.Phase)
	}
	if err := g.Bid(1, 5); err != nil { // 같은 횟수 — 나중에 도착
		t.Fatalf("동수 외침: %v", err)
	}
	if err := g.Bid(0, 5); err == nil {
		t.Fatal("같은 횟수로 다시 외치기가 통과했다")
	}
	if err := g.Bid(0, 6); err == nil {
		t.Fatal("더 많은 횟수로 고치기가 통과했다")
	}

	sorted := g.SortedBids()
	if len(sorted) != 2 || sorted[0].Seat != 0 || sorted[1].Seat != 1 {
		t.Fatalf("동수 정렬 = %+v, want 먼저 도착한 seat0 이 앞", sorted)
	}

	// 더 적게 고치면 앞으로 나간다
	if err := g.Bid(1, 3); err != nil {
		t.Fatalf("낮추기: %v", err)
	}
	sorted = g.SortedBids()
	if sorted[0].Seat != 1 || sorted[0].Moves != 3 {
		t.Fatalf("낮춘 뒤 정렬 = %+v", sorted)
	}

	g.CloseBidding()
	if g.Phase != RRPhaseDemo {
		t.Fatalf("마감 뒤 단계 = %s, want demo", g.Phase)
	}
	if g.DemoSeat() != 1 || g.DemoBid() != 3 {
		t.Fatalf("증명자 = seat%d (%d회), want seat1 (3회)", g.DemoSeat(), g.DemoBid())
	}
	if err := g.Bid(2, 2); err == nil {
		t.Fatal("증명 단계에서 외침이 통과했다")
	}
}

// TestRRDemoOrderAndFailover 증명 순서 — 실패·포기·시간 초과는 그다음으로
// 적게 외친 사람에게 넘어가고, 아무도 성공하지 못하면 그 목표는 넘어간다.
func TestRRDemoOrderAndFailover(t *testing.T) {
	g, _ := rrNewStartedGame(t, 3, 23)

	solution, ok := rrSolve(g.Board, g.Robots, g.Goal, RRMaxDepth)
	if !ok {
		t.Fatal("시작 목표를 못 풀었다")
	}
	min := len(solution)

	// seat0 이 가장 적게, seat1 이 그다음, seat2 는 아예 많이 외친다
	mustBid(t, g, 0, min)
	mustBid(t, g, 1, min+1)
	mustBid(t, g, 2, min+2)
	g.CloseBidding()

	if g.DemoSeat() != 0 {
		t.Fatalf("첫 증명자 = seat%d, want seat0", g.DemoSeat())
	}
	// 남의 차례에 증명하면 거절
	if err := g.Demo(1, solution); err == nil {
		t.Fatal("차례가 아닌 좌석의 증명이 통과했다")
	}

	// seat0 실패 → seat1 → 포기 → seat2 → 시간 초과 → 목표가 넘어간다
	if err := g.Demo(0, []RRMove{}); err != nil {
		t.Fatalf("빈 증명: %v", err)
	}
	if g.DemoSeat() != 1 {
		t.Fatalf("실패 뒤 증명자 = seat%d, want seat1", g.DemoSeat())
	}
	if err := g.Pass(1); err != nil {
		t.Fatalf("포기: %v", err)
	}
	if g.DemoSeat() != 2 {
		t.Fatalf("포기 뒤 증명자 = seat%d, want seat2", g.DemoSeat())
	}
	g.DemoTimeout()
	if g.Phase != RRPhaseGoalEnd {
		t.Fatalf("전원 실패 뒤 단계 = %s, want goal_end", g.Phase)
	}
	if g.LastResult == nil || g.LastResult.Seat != -1 || g.LastResult.OK {
		t.Fatalf("전원 실패 결과 = %+v", g.LastResult)
	}
	for _, p := range g.Players {
		if p.Score != 0 {
			t.Fatalf("아무도 성공 못 했는데 seat%d 가 %d점", p.Seat, p.Score)
		}
	}
}

// TestRRDemoSuccessKeepsRobots 증명에 성공하면 점수가 오르고 로봇은 증명이
// 끝난 자리에 남는다 — 다음 목표는 그 배치에서 이어진다.
func TestRRDemoSuccessKeepsRobots(t *testing.T) {
	g, rng := rrNewStartedGame(t, 2, 31)

	solution, ok := rrSolve(g.Board, g.Robots, g.Goal, RRMaxDepth)
	if !ok {
		t.Fatal("시작 목표를 못 풀었다")
	}
	before := g.Robots
	goal := g.Goal

	mustBid(t, g, 1, len(solution))
	g.CloseBidding()
	if err := g.Demo(1, solution); err != nil {
		t.Fatalf("증명: %v", err)
	}

	if g.Players[1].Score != 1 || g.Players[0].Score != 0 {
		t.Fatalf("점수 = %d/%d", g.Players[0].Score, g.Players[1].Score)
	}
	if g.Robots == before {
		t.Fatal("증명 성공인데 로봇이 그대로다")
	}
	if g.Robots[rrColorIndex(goal.Color)] != rrAt(goal.R, goal.C) {
		t.Fatal("목표 색 로봇이 목표 지점에 없다")
	}
	if g.Phase != RRPhaseGoalEnd {
		t.Fatalf("성공 뒤 단계 = %s", g.Phase)
	}

	g.NextGoal(rng)
	if g.Phase != RRPhaseThinking || g.GoalIndex != 1 {
		t.Fatalf("다음 목표 단계 = %s (index %d)", g.Phase, g.GoalIndex)
	}
	if len(g.Bids) != 0 || g.DemoSeat() != -1 {
		t.Fatalf("다음 목표에 외침이 남아 있다: %+v", g.Bids)
	}
	if g.MinMoves < RRMinGoalMoves || g.MinMoves > RRMaxGoalMoves {
		t.Fatalf("다음 목표 최소 횟수 = %d회", g.MinMoves)
	}
}

// TestRRGoalTimeout 아무도 외치지 않은 목표는 상한이 지나면 넘어간다
func TestRRGoalTimeout(t *testing.T) {
	g, _ := rrNewStartedGame(t, 2, 41)
	g.GoalTimeout()
	if g.Phase != RRPhaseGoalEnd {
		t.Fatalf("단계 = %s, want goal_end", g.Phase)
	}
	if g.LastResult == nil || !strings.Contains(g.LastResult.Message, "외치지") {
		t.Fatalf("결과 문구 = %+v", g.LastResult)
	}
}

// mustBid 외침이 통과해야 하는 자리
func mustBid(t *testing.T, g *RRGame, seat, moves int) {
	t.Helper()
	if err := g.Bid(seat, moves); err != nil {
		t.Fatalf("seat%d 가 %d회를 외치지 못했다: %v", seat, moves, err)
	}
}

// ==================== 한 판 완주 ====================

// TestRRFullGameEndsAfterAllGoals 목표 17개를 소진하면 반드시 끝난다.
// 매 목표의 최소 횟수가 2~10 범위 안이고, 최소 경로로 증명하면 항상
// 성공하는지도 함께 본다. 되섞지 않고 하나씩 소진하는 것이 종료 보증이다.
func TestRRFullGameEndsAfterAllGoals(t *testing.T) {
	g, rng := rrNewStartedGame(t, 2, 20260828)

	goals := 0
	for step := 0; step < 500 && g.Phase != RRPhaseGameOver; step++ {
		switch g.Phase {
		case RRPhaseThinking, RRPhaseBidding:
			moves, ok := rrSolve(g.Board, g.Robots, g.Goal, RRMaxDepth)
			if !ok {
				t.Fatalf("%d번째 목표를 못 풀었다", g.GoalIndex)
			}
			if len(moves) != g.MinMoves {
				t.Fatalf("%d번째 목표: rrSolve %d회 vs 생성 %d회",
					g.GoalIndex, len(moves), g.MinMoves)
			}
			if g.MinMoves < RRMinGoalMoves || g.MinMoves > RRMaxGoalMoves {
				t.Fatalf("%d번째 목표 최소 횟수 = %d회", g.GoalIndex, g.MinMoves)
			}
			mustBid(t, g, 0, len(moves))
			g.CloseBidding()
		case RRPhaseDemo:
			moves, _ := rrSolve(g.Board, g.Robots, g.Goal, RRMaxDepth)
			if err := g.Demo(g.DemoSeat(), moves); err != nil {
				t.Fatalf("증명: %v", err)
			}
			if g.LastResult == nil || !g.LastResult.OK {
				t.Fatalf("최소 경로 증명이 실패했다: %+v", g.LastResult)
			}
			goals++
		case RRPhaseGoalEnd:
			g.NextGoal(rng)
		}
		g.DrainEvents()
	}

	if g.Phase != RRPhaseGameOver {
		t.Fatalf("17개를 소진했는데 끝나지 않았다 (단계 %s, 목표 %d)", g.Phase, g.GoalIndex)
	}
	if goals != RRGoalTotal {
		t.Fatalf("소진한 목표 = %d개, want %d개", goals, RRGoalTotal)
	}
	if g.EndReason != "goals_done" {
		t.Fatalf("종료 사유 = %q, want goals_done", g.EndReason)
	}
	if g.GoalsPlayed() != RRGoalTotal {
		t.Fatalf("GoalsPlayed = %d", g.GoalsPlayed())
	}
	if g.Players[0].Score != RRGoalTotal {
		t.Fatalf("전부 가져간 좌석의 점수 = %d", g.Players[0].Score)
	}
	if g.Result == nil || len(g.Result.WinnerSeats) != 1 || g.Result.WinnerSeats[0] != 0 {
		t.Fatalf("승자 = %+v", g.Result)
	}
}

// TestRRForceEndSettlesScores 전체 캡이 걸리면 현재 점수로 정산한다
func TestRRForceEndSettlesScores(t *testing.T) {
	g, _ := rrNewStartedGame(t, 3, 53)
	g.Players[2].Score = 2
	g.ForceEnd()

	if g.Phase != RRPhaseGameOver || g.EndReason != "time_up" {
		t.Fatalf("단계 %s / 사유 %q", g.Phase, g.EndReason)
	}
	if g.Result == nil || len(g.Result.WinnerSeats) != 1 || g.Result.WinnerSeats[0] != 2 {
		t.Fatalf("승자 = %+v", g.Result)
	}
	if !strings.Contains(g.Result.Message, "제한 시간") {
		t.Fatalf("문구 = %q", g.Result.Message)
	}
}

// TestRRWinnersTie 동점은 공동 승리 (좌석 오름차순)
func TestRRWinnersTie(t *testing.T) {
	players := []*RRPlayer{
		{Seat: 0, Name: "가", Score: 3},
		{Seat: 1, Name: "나", Score: 1},
		{Seat: 2, Name: "다", Score: 3},
	}
	seats, names := rrWinners(players)
	if len(seats) != 2 || seats[0] != 0 || seats[1] != 2 {
		t.Fatalf("공동 승자 좌석 = %v", seats)
	}
	if len(names) != 2 || names[0] != "가" || names[1] != "다" {
		t.Fatalf("공동 승자 이름 = %v", names)
	}
}

// TestRRBoardRoundTrip 스냅샷의 벽 격자로 판을 되돌려도 미끄러짐이 같은지 —
// 봇이 와이어에서 판을 복원해 rrSolve 를 돌리는 경로의 근거다.
func TestRRBoardRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(97))
	board := rrGenerateBoard(rng)
	robots := rrPlaceRobots(board, rng)

	walls := make([][]int, 0, RRSize)
	for r := 0; r < RRSize; r++ {
		row := make([]int, 0, RRSize)
		for c := 0; c < RRSize; c++ {
			row = append(row, int(board.Walls[rrIndex(r, c)]))
		}
		walls = append(walls, row)
	}
	restored := rrBoardFromWalls(walls)
	if restored == nil {
		t.Fatal("판 복원 실패")
	}

	for robot := 0; robot < RRRobotCount; robot++ {
		for dir := 0; dir < 4; dir++ {
			want := rrSlide(board, robots, robot, dir)
			got := rrSlide(restored, robots, robot, dir)
			if want != got {
				t.Fatalf("%s %s: 원본 %s vs 복원 %s",
					rrColorLabel(rrColors[robot]), rrDirLabel(rrDirs[dir]),
					rrShow(want), rrShow(got))
			}
		}
	}
	if rrBoardFromWalls([][]int{{1}}) != nil {
		t.Fatal("잘못된 크기의 벽 격자를 받아들였다")
	}
}
