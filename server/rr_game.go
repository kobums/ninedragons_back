package server

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"time"
)

// ==================== 리코셰 로봇 순수 규칙 ====================
//
// 판 생성·미끄러짐 판정·BFS 최소 횟수·외침/증명 판정·종료 판정만 다룬다.
// 클라이언트·타이머를 모르며, 허브(rr_hub.go)가 단계 마감을 걸고 이벤트
// 큐(DrainEvents)를 방송한다.
//
// 차례가 없다. 판정 순서는 허브 고루틴에 도착한 순서 그대로이고, 이 파일의
// Bid/Demo 는 그 순서대로 한 번에 하나씩만 불린다 — 그래서 락이 필요 없다.
//
// 한 목표의 흐름:
//
//	목표가 열린다 (thinking)
//	→ 누구든 rr_bid{moves} — 첫 외침이 카운트다운을 연다 (bidding)
//	→ 카운트다운 종료 → 가장 적게 외친 사람부터 증명 (demo)
//	   · 성공 → +1점, 로봇은 증명이 끝난 자리에 남는다
//	   · 실패·포기·시간 초과 → 그다음으로 적게 외친 사람에게 넘어간다
//	   · 전원 실패 → 그 목표는 넘어간다
//	→ 정산 표시 (goal_end) → 다음 목표
//	→ 목표 17개를 소진하면 종료 (최다 획득 승, 동점 공동)
//
// 반드시 끝난다 — 목표는 성공·실패에 관계없이 매번 하나씩 소진되고
// (되섞으면 종료 보증이 무너진다), 각 단계에 마감이 걸려 있으며,
// 그 위에 게임 전체 캡(30분)이 있다.

// ==================== 칸 / 방향 헬퍼 ====================

// rrIndex (행,열) → 칸 색인
func rrIndex(r, c int) int { return r*RRSize + c }

// rrRowCol 칸 색인 → (행,열)
func rrRowCol(pos uint8) (int, int) { return int(pos) / RRSize, int(pos) % RRSize }

// rrNextCell[dir][pos] 이웃 칸 색인 (판 밖이면 -1). init 에서 한 번 만든다 —
// 미끄러짐 판정의 안쪽 루프에서 나눗셈을 없애는 게 목적이다.
var rrNextCell [4][RRCellCount]int16

func init() {
	deltas := [4][2]int{{-1, 0}, {0, 1}, {1, 0}, {0, -1}} // up, right, down, left
	for dir := 0; dir < 4; dir++ {
		for pos := 0; pos < RRCellCount; pos++ {
			r, c := pos/RRSize, pos%RRSize
			nr, nc := r+deltas[dir][0], c+deltas[dir][1]
			if nr < 0 || nr >= RRSize || nc < 0 || nc >= RRSize {
				rrNextCell[dir][pos] = -1
				continue
			}
			rrNextCell[dir][pos] = int16(nr*RRSize + nc)
		}
	}
}

// rrCenterCells 중앙 2×2 진입 불가 구역의 칸 색인 4개.
// 16×16 판의 한가운데 — 프론트도 같은 좌표를 쓴다.
func rrCenterCells() [4]int {
	m := RRSize/2 - 1 // 7
	return [4]int{rrIndex(m, m), rrIndex(m, m+1), rrIndex(m+1, m), rrIndex(m+1, m+1)}
}

// rrWallCount 칸에 붙은 벽 개수
func rrWallCount(mask uint8) int {
	n := 0
	for _, bit := range rrDirBit {
		if mask&bit != 0 {
			n++
		}
	}
	return n
}

// rrAddWall 칸과 이웃 칸에 벽을 **양쪽 모두** 기록한다.
// 한쪽에만 넣으면 방향에 따라 미끄러짐이 달라지는 유령 벽이 된다.
func rrAddWall(b *RRBoard, pos, dir int) {
	b.Walls[pos] |= rrDirBit[dir]
	if n := rrNextCell[dir][pos]; n >= 0 {
		b.Walls[n] |= rrDirBit[rrOpposite[dir]]
	}
}

// rrCanAddWall 벽을 하나 더 놓아도 양쪽 칸이 3면 이상 막히지 않는지.
// 3면이 막힌 칸은 사실상 함정이라 퍼즐이 지저분해진다.
func rrCanAddWall(b *RRBoard, pos, dir int) bool {
	if b.Walls[pos]&rrDirBit[dir] != 0 {
		return false
	}
	if rrWallCount(b.Walls[pos]) >= 2 {
		return false
	}
	n := rrNextCell[dir][pos]
	if n < 0 {
		return false
	}
	if b.Blocked[n] || rrWallCount(b.Walls[n]) >= 2 {
		return false
	}
	return true
}

// ==================== 판 생성 ====================

const (
	// rrWallLShapes 안쪽에 흩뿌리는 L자 벽 조각 수
	rrWallLShapes = 18
	// rrBorderWalls 가장자리에 붙는 단일 벽 수 (가장자리와 직각)
	rrBorderWalls = 8
)

// rrGenerateBoard 16×16 판을 만든다. 가장자리는 전부 벽이고, 중앙 2×2는
// 진입 불가이며, 안쪽에 L자 벽 조각이 흩어진다.
//
// 벽은 항상 양쪽 칸에 기록하고(rrAddWall), 어느 칸도 3면 이상 막히지 않게
// 제한한다(rrCanAddWall) — 로봇이 갇히는 칸을 만들지 않기 위해서다.
func rrGenerateBoard(rng *rand.Rand) *RRBoard {
	b := &RRBoard{}

	// 가장자리
	for i := 0; i < RRSize; i++ {
		b.Walls[rrIndex(0, i)] |= rrWallUp
		b.Walls[rrIndex(RRSize-1, i)] |= rrWallDown
		b.Walls[rrIndex(i, 0)] |= rrWallLeft
		b.Walls[rrIndex(i, RRSize-1)] |= rrWallRight
	}

	// 중앙 2×2 막힌 구역 — 둘레를 벽으로 두른다 (양쪽 칸에 기록)
	center := rrCenterCells()
	for _, pos := range center {
		b.Blocked[pos] = true
	}
	for _, pos := range center {
		for dir := 0; dir < 4; dir++ {
			n := rrNextCell[dir][pos]
			if n < 0 || b.Blocked[n] {
				continue
			}
			rrAddWall(b, pos, dir)
		}
	}

	// L자 벽 조각 — 벽 없는 안쪽 칸에 세로 한 면 + 가로 한 면
	placed := 0
	for tries := 0; tries < 5000 && placed < rrWallLShapes; tries++ {
		r := 1 + rng.Intn(RRSize-2)
		c := 1 + rng.Intn(RRSize-2)
		pos := rrIndex(r, c)
		if b.Blocked[pos] || b.Walls[pos] != 0 {
			continue
		}
		vertical := []int{0, 2}[rng.Intn(2)]
		horizontal := []int{1, 3}[rng.Intn(2)]
		if !rrCanAddWall(b, pos, vertical) || !rrCanAddWall(b, pos, horizontal) {
			continue
		}
		rrAddWall(b, pos, vertical)
		rrAddWall(b, pos, horizontal)
		placed++
	}

	// 가장자리에 붙는 단일 벽 (가장자리와 직각 — 미끄러짐을 끊는 장치)
	for tries, done := 0, 0; tries < 500 && done < rrBorderWalls; tries++ {
		k := 1 + rng.Intn(RRSize-2)
		var pos, dir int
		switch rng.Intn(4) {
		case 0:
			pos, dir = rrIndex(0, k), []int{1, 3}[rng.Intn(2)]
		case 1:
			pos, dir = rrIndex(RRSize-1, k), []int{1, 3}[rng.Intn(2)]
		case 2:
			pos, dir = rrIndex(k, 0), []int{0, 2}[rng.Intn(2)]
		default:
			pos, dir = rrIndex(k, RRSize-1), []int{0, 2}[rng.Intn(2)]
		}
		if !rrCanAddWall(b, pos, dir) {
			continue
		}
		rrAddWall(b, pos, dir)
		done++
	}

	return b
}

// rrPlaceRobots 로봇 4대를 서로 다른 진입 가능 칸에 놓는다
func rrPlaceRobots(b *RRBoard, rng *rand.Rand) [RRRobotCount]uint8 {
	var robots [RRRobotCount]uint8
	taken := map[int]bool{}
	for i := 0; i < RRRobotCount; i++ {
		for {
			pos := rng.Intn(RRCellCount)
			if b.Blocked[pos] || taken[pos] {
				continue
			}
			taken[pos] = true
			robots[i] = uint8(pos)
			break
		}
	}
	return robots
}

// ==================== 미끄러짐 (순수) ====================
//
// 이 게임의 전부다. 로봇은 고른 방향으로 **벽이나 다른 로봇에 막힐 때까지**
// 미끄러진다 — 한 칸씩은 움직이지 못한다. 판 가장자리는 전부 벽이므로
// 가장자리에서 멈추는 것도 같은 규칙의 결과다.
//
// 이미 그 방향으로 벽을 등지고 있거나 바로 옆에 로봇이 있으면 제자리를
// 그대로 돌려준다 (= 그 이동은 불가).

// rrWallBetween 칸 pos 에서 dir 방향으로 나가는 경계에 벽이 있는지.
//
// **양쪽을 함께 본다** — pos 의 dir 비트든 이웃 칸의 반대편 비트든 하나만
// 서 있어도 벽이다. rrAddWall 이 항상 양쪽에 새기므로 정상 판에서는 두 검사가
// 같은 답을 내지만, 프론트의 고스트 미리보기가 같은 규칙(양쪽 검사)으로
// 계산하기 때문에 서버도 같은 규칙이어야 판정이 어긋나지 않는다.
// 판 밖으로 나가는 경계도 벽으로 본다.
func rrWallBetween(b *RRBoard, pos uint8, dir int) bool {
	if b.Walls[pos]&rrDirBit[dir] != 0 {
		return true
	}
	next := rrNextCell[dir][pos]
	if next < 0 {
		return true
	}
	return b.Walls[next]&rrDirBit[rrOpposite[dir]] != 0
}

// rrSlide 로봇 robot 을 dir 방향으로 미끄러뜨렸을 때의 도착 칸.
// 움직일 수 없으면 출발 칸을 그대로 돌려준다.
func rrSlide(b *RRBoard, robots [RRRobotCount]uint8, robot, dir int) uint8 {
	pos := robots[robot]
	for {
		if rrWallBetween(b, pos, dir) {
			return pos
		}
		next := rrNextCell[dir][pos]
		if next < 0 || b.Blocked[next] {
			return pos
		}
		blocked := false
		for i := 0; i < RRRobotCount; i++ {
			if i != robot && robots[i] == uint8(next) {
				blocked = true
				break
			}
		}
		if blocked {
			return pos
		}
		pos = uint8(next)
	}
}

// rrApplyMoves 이동 순서를 그대로 적용한다. 제자리 이동(막힌 방향)은
// 규칙 위반이라 에러다 — 증명에서 이동을 늘려 때우는 것을 막는다.
func rrApplyMoves(b *RRBoard, robots [RRRobotCount]uint8, moves []RRMove) ([RRRobotCount]uint8, error) {
	cur := robots
	for i, mv := range moves {
		ci := rrColorIndex(mv.Robot)
		di := rrDirIndex(mv.Dir)
		if ci < 0 || di < 0 {
			return cur, fmt.Errorf("%s (%d번째)", rrDemoBadMoveMsg, i+1)
		}
		dest := rrSlide(b, cur, ci, di)
		if dest == cur[ci] {
			return cur, fmt.Errorf("%s (%d번째: %s 로봇 %s)",
				rrDemoStuckMsg, i+1, rrColorLabel(mv.Robot), rrDirLabel(mv.Dir))
		}
		cur[ci] = dest
	}
	return cur, nil
}

// ==================== BFS 최소 횟수 (순수) ====================
//
// 상태는 로봇 4대의 위치 조합이다 (16×16 칸 × 4대). 한 상태에서 뻗는 가지는
// 16개(로봇 4 × 방향 4)뿐이고, BFS 는 층 단위로 훑으므로 처음 목표에 닿는
// 순간이 곧 최소 횟수다.
//
// 상태 수는 이론상 256⁴이라 깊이가 커지면 폭발한다. 그래서 두 가지 안전장치를
// 둔다 — maxDepth(기본 RRMaxDepth=12)와 탐색 노드 예산이다. 예산을 넘기면
// "해 없음"으로 처리하고 판 생성 쪽이 다시 뽑는다.
//
// 예산은 생성용(rrGenNodeBudget)보다 풀이용(rrSolveNodeBudget)을 크게 잡는다.
// 생성이 예산 안에서 찾아낸 목표는 같은 순서로 도는 풀이가 반드시 다시 찾는다.

const (
	// rrGenNodeBudget 목표 후보를 모으는 BFS의 노드 예산
	rrGenNodeBudget = 90000
	// rrSolveNodeBudget rrSolve 의 노드 예산 (생성 예산보다 넉넉하게)
	rrSolveNodeBudget = 400000
)

// rrKey 로봇 위치 조합을 uint32 하나로 (visited 맵의 키)
func rrKey(robots [RRRobotCount]uint8) uint32 {
	return uint32(robots[0]) | uint32(robots[1])<<8 |
		uint32(robots[2])<<16 | uint32(robots[3])<<24
}

// rrUnkey uint32 키를 로봇 위치 조합으로
func rrUnkey(k uint32) [RRRobotCount]uint8 {
	return [RRRobotCount]uint8{
		uint8(k), uint8(k >> 8), uint8(k >> 16), uint8(k >> 24),
	}
}

// rrTrace BFS 역추적 한 칸 — 어디서 어떤 이동으로 왔는지
type rrTrace struct {
	parent uint32
	robot  uint8
	dir    uint8
	depth  uint8
}

// rrSolve 최소 횟수와 그 이동 순서를 BFS로 구한다.
// maxDepth 를 넘거나 노드 예산을 넘기면 ok=false ("해 없음").
// 이미 목표에 있으면 빈 슬라이스와 ok=true 다 (0수).
func rrSolve(b *RRBoard, robots [RRRobotCount]uint8, goal RRGoal, maxDepth int) ([]RRMove, bool) {
	ci := rrColorIndex(goal.Color)
	if ci < 0 || goal.R < 0 || goal.R >= RRSize || goal.C < 0 || goal.C >= RRSize {
		return nil, false
	}
	target := uint8(rrIndex(goal.R, goal.C))
	if robots[ci] == target {
		return []RRMove{}, true
	}
	if maxDepth <= 0 {
		return nil, false
	}

	start := rrKey(robots)
	seen := map[uint32]rrTrace{start: {parent: start}}
	queue := []uint32{start}
	budget := rrSolveNodeBudget

	for head := 0; head < len(queue); head++ {
		cur := queue[head]
		depth := seen[cur].depth
		if int(depth) >= maxDepth {
			continue
		}
		pos := rrUnkey(cur)
		for robot := 0; robot < RRRobotCount; robot++ {
			for dir := 0; dir < 4; dir++ {
				dest := rrSlide(b, pos, robot, dir)
				if dest == pos[robot] {
					continue // 막혀서 못 움직인다
				}
				next := pos
				next[robot] = dest
				k := rrKey(next)
				if _, ok := seen[k]; ok {
					continue
				}
				seen[k] = rrTrace{parent: cur, robot: uint8(robot),
					dir: uint8(dir), depth: depth + 1}
				if robot == ci && dest == target {
					return rrTracePath(seen, k, start), true
				}
				budget--
				if budget <= 0 {
					return nil, false
				}
				queue = append(queue, k)
			}
		}
	}
	return nil, false
}

// rrTracePath 역추적으로 이동 순서를 복원한다 (앞에서부터)
func rrTracePath(seen map[uint32]rrTrace, end, start uint32) []RRMove {
	moves := []RRMove{}
	for k := end; k != start; {
		tr := seen[k]
		moves = append(moves, RRMove{Robot: rrColors[tr.robot], Dir: rrDirs[tr.dir]})
		k = tr.parent
	}
	for i, j := 0, len(moves)-1; i < j; i, j = i+1, j-1 {
		moves[i], moves[j] = moves[j], moves[i]
	}
	return moves
}

// rrGoalDepths 각 (색, 칸)에 그 색 로봇이 처음 도달하는 최소 횟수를 BFS로 모은다.
// -1 은 예산 안에서 닿지 못한 칸이다. 판 생성이 목표 후보를 고르는 근거이며,
// 여기서 나온 깊이는 정의상 rrSolve 의 답과 같다 (같은 BFS다).
func rrGoalDepths(b *RRBoard, robots [RRRobotCount]uint8, budget int) [RRRobotCount][RRCellCount]int16 {
	var depths [RRRobotCount][RRCellCount]int16
	for ci := range depths {
		for i := range depths[ci] {
			depths[ci][i] = -1
		}
		depths[ci][robots[ci]] = 0
	}

	seen := map[uint32]bool{rrKey(robots): true}
	level := []uint32{rrKey(robots)}
	for depth := 1; depth <= RRMaxGoalMoves && len(level) > 0 && budget > 0; depth++ {
		next := make([]uint32, 0, len(level)*4)
		for _, cur := range level {
			pos := rrUnkey(cur)
			for robot := 0; robot < RRRobotCount; robot++ {
				for dir := 0; dir < 4; dir++ {
					dest := rrSlide(b, pos, robot, dir)
					if dest == pos[robot] {
						continue
					}
					moved := pos
					moved[robot] = dest
					k := rrKey(moved)
					if seen[k] {
						continue
					}
					seen[k] = true
					if depths[robot][dest] < 0 {
						depths[robot][dest] = int16(depth)
					}
					budget--
					if budget <= 0 {
						return depths
					}
					next = append(next, k)
				}
			}
		}
		level = next
	}
	return depths
}

// rrDrawGoal 최소 횟수가 RRMinGoalMoves~RRMaxGoalMoves 인 목표를 하나 뽑는다.
// 돌려주는 두 번째 값이 그 목표의 최소 횟수다. 후보가 없으면 ok=false —
// 호출부는 로봇을 다시 배치하고 재시도한다.
//
// 깊이를 먼저 고르고 그 안에서 칸을 고른다. 얕은 깊이의 후보가 압도적으로
// 많아서, 칸을 바로 뽑으면 2~3수짜리만 나오기 때문이다.
func rrDrawGoal(b *RRBoard, robots [RRRobotCount]uint8, used map[int]bool,
	rng *rand.Rand) (RRGoal, int, bool) {
	depths := rrGoalDepths(b, robots, rrGenNodeBudget)

	byDepth := map[int][]int{}
	for ci := 0; ci < RRRobotCount; ci++ {
		for pos := 0; pos < RRCellCount; pos++ {
			d := int(depths[ci][pos])
			if d < RRMinGoalMoves || d > RRMaxGoalMoves {
				continue
			}
			key := ci*RRCellCount + pos
			if used[key] {
				continue
			}
			byDepth[d] = append(byDepth[d], key)
		}
	}
	if len(byDepth) == 0 {
		return RRGoal{}, 0, false
	}

	// 맵 순회 순서는 무작위라 rng 만으로 재현되지 않는다 — 정렬해서 고른다
	avail := make([]int, 0, len(byDepth))
	for d := range byDepth {
		avail = append(avail, d)
	}
	sort.Ints(avail)

	d := avail[rng.Intn(len(avail))]
	list := byDepth[d]
	key := list[rng.Intn(len(list))]
	ci, pos := key/RRCellCount, key%RRCellCount
	r, c := pos/RRSize, pos%RRSize
	return RRGoal{Color: rrColors[ci], R: r, C: c}, d, true
}

// ==================== 증명 판정 (순수) ====================

// rrJudgeDemo 증명을 판정한다. 이동 순서를 그대로 적용해 목표에 닿는지,
// 이동 횟수가 외친 횟수 **이하**인지 확인한다.
// 돌려주는 값은 (적용 후 로봇 위치, 성립 여부, 문구).
func rrJudgeDemo(b *RRBoard, robots [RRRobotCount]uint8, goal RRGoal,
	moves []RRMove, bid int) ([RRRobotCount]uint8, bool, string) {
	if len(moves) == 0 {
		return robots, false, rrDemoMissMsg
	}
	if len(moves) > bid {
		return robots, false, fmt.Sprintf("%s (%d회 외침, %d회 증명)",
			rrDemoOverMsg, bid, len(moves))
	}
	after, err := rrApplyMoves(b, robots, moves)
	if err != nil {
		return robots, false, err.Error()
	}
	ci := rrColorIndex(goal.Color)
	if ci < 0 || after[ci] != uint8(rrIndex(goal.R, goal.C)) {
		return robots, false, rrDemoMissMsg
	}
	return after, true, fmt.Sprintf("%s (%d회)", rrDemoOKMsg, len(moves))
}

// rrCellLabel 칸 한글 표기 — 사람이 읽는 좌표는 1부터 센다
func rrCellLabel(r, c int) string {
	return fmt.Sprintf("%d행 %d열", r+1, c+1)
}

// rrGoalLabel 목표 한글 표기
func rrGoalLabel(goal RRGoal) string {
	return fmt.Sprintf("%s 로봇 → %s", rrColorLabel(goal.Color), rrCellLabel(goal.R, goal.C))
}

// ==================== 게임 ====================

// NewRRGame 대기 상태의 새 게임
func NewRRGame(id string) *RRGame {
	return &RRGame{
		ID:        id,
		Players:   []*RRPlayer{},
		Phase:     RRPhaseWaiting,
		Bids:      []RRBid{},
		DemoOrder: []int{},
		usedGoals: map[int]bool{},
	}
}

// AddPlayer 대기실 입장. 좌석 번호를 돌려준다.
func (g *RRGame) AddPlayer(name string) (int, error) {
	if g.Phase != RRPhaseWaiting {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= RRMaxPlayers {
		return -1, fmt.Errorf("자리가 없습니다 (최대 %d명)", RRMaxPlayers)
	}
	seat := len(g.Players)
	g.Players = append(g.Players, &RRPlayer{Seat: seat, Name: name})
	return seat, nil
}

// RemovePlayer 대기실 퇴장. 좌석을 앞으로 당겨 빈틈을 없앤다.
func (g *RRGame) RemovePlayer(seat int) {
	if g.Phase != RRPhaseWaiting || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i, p := range g.Players {
		p.Seat = i
	}
}

// CanStart 시작 가능 여부 — 차례가 없어 혼자서도 연습할 수 있다
func (g *RRGame) CanStart() bool {
	return g.Phase == RRPhaseWaiting && len(g.Players) >= RRMinPlayers
}

// DrainEvents 쌓인 연출 이벤트를 꺼내 비운다 (허브가 방송한다)
func (g *RRGame) DrainEvents() []RRGameEvent {
	events := g.events
	g.events = nil
	return events
}

// pushEvent 연출 이벤트 적재 (seat -1 은 좌석 없음)
func (g *RRGame) pushEvent(kind string, seat int, message string) {
	g.events = append(g.events, RRGameEvent{Kind: kind, Seat: seat, Message: message})
}

// DemoSeat 지금 증명할 좌석 (증명 단계가 아니면 -1)
func (g *RRGame) DemoSeat() int {
	if g.Phase != RRPhaseDemo || g.DemoTurn < 0 || g.DemoTurn >= len(g.DemoOrder) {
		return -1
	}
	return g.Bids[g.DemoOrder[g.DemoTurn]].Seat
}

// DemoBid 지금 증명하는 사람이 외친 횟수 (증명 단계가 아니면 0)
func (g *RRGame) DemoBid() int {
	if g.Phase != RRPhaseDemo || g.DemoTurn < 0 || g.DemoTurn >= len(g.DemoOrder) {
		return 0
	}
	return g.Bids[g.DemoOrder[g.DemoTurn]].Moves
}

// SortedBids 적은 횟수 → 먼저 도착 순으로 정렬한 외침 목록 (스냅샷·증명 순서)
func (g *RRGame) SortedBids() []RRBid {
	out := append([]RRBid{}, g.Bids...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Moves != out[j].Moves {
			return out[i].Moves < out[j].Moves
		}
		return out[i].Seq < out[j].Seq
	})
	return out
}

// Start 게임 시작 — 판을 만들고 로봇을 놓고 첫 목표를 연다
func (g *RRGame) Start(rng *rand.Rand) error {
	if !g.CanStart() {
		return fmt.Errorf("%d명 이상 모여야 시작할 수 있습니다", RRMinPlayers)
	}

	g.Board = rrGenerateBoard(rng)
	g.Robots = rrPlaceRobots(g.Board, rng)
	g.usedGoals = map[int]bool{}
	g.GoalIndex = 0
	g.LastResult = nil
	g.Result = nil
	g.EndReason = ""
	g.Ready = true
	g.StartedAt = time.Now()
	for _, p := range g.Players {
		p.Score = 0
	}

	if !g.openGoal(rng) {
		g.finish("no_goal")
		return nil
	}
	g.Phase = RRPhaseThinking
	return nil
}

// openGoal 현재 로봇 배치에서 다음 목표를 연다. 지금 배치로 조건에 맞는
// 목표를 못 찾으면 로봇을 다시 뿌려 재시도한다 (최소 횟수 2~10 보장).
func (g *RRGame) openGoal(rng *rand.Rand) bool {
	for attempt := 0; attempt < 40; attempt++ {
		goal, min, ok := rrDrawGoal(g.Board, g.Robots, g.usedGoals, rng)
		if ok {
			g.Goal, g.MinMoves = goal, min
			g.usedGoals[rrColorIndex(goal.Color)*RRCellCount+rrIndex(goal.R, goal.C)] = true
			g.Bids = []RRBid{}
			g.DemoOrder = []int{}
			g.DemoTurn = 0
			g.bidSeq = 0
			g.pushEvent("goal_opened", -1, fmt.Sprintf("%d/%d번째 목표 지점 — %s",
				g.GoalIndex+1, RRGoalTotal, rrGoalLabel(goal)))
			return true
		}
		// 지금 배치로는 조건에 맞는 목표가 없다 — 로봇을 다시 뿌린다
		g.Robots = rrPlaceRobots(g.Board, rng)
	}
	return false
}

// Bid 외침. 차례 검사가 없다 — 누구든 언제든 보낼 수 있고, 이 함수에 도착한
// 순서가 곧 우선순위다(허브 고루틴 직렬화). 같은 횟수를 외치면 먼저 도착한
// 쪽이 앞선다.
//
// 자기 외침을 고칠 때는 **더 적은 횟수로만** 가능하다 (규칙: 더 적은 횟수를
// 외칠 수 있다). 첫 외침이 카운트다운을 연다.
func (g *RRGame) Bid(seat, moves int) error {
	if g.Phase != RRPhaseThinking && g.Phase != RRPhaseBidding {
		return errors.New("지금은 외칠 수 없습니다")
	}
	if seat < 0 || seat >= len(g.Players) {
		return errors.New("좌석을 찾을 수 없습니다")
	}
	if moves < 1 || moves > RRMaxBid {
		return fmt.Errorf("1~%d 사이로 외쳐야 합니다", RRMaxBid)
	}

	for i := range g.Bids {
		if g.Bids[i].Seat != seat {
			continue
		}
		if moves >= g.Bids[i].Moves {
			return fmt.Errorf("더 적은 횟수로만 다시 외칠 수 있습니다 (지금 %d회)",
				g.Bids[i].Moves)
		}
		g.bidSeq++
		g.Bids[i].Moves = moves
		g.Bids[i].Seq = g.bidSeq
		g.pushEvent("bid", seat, fmt.Sprintf("%s님이 %d회로 낮췄습니다",
			g.Players[seat].Name, moves))
		return nil
	}

	g.bidSeq++
	g.Bids = append(g.Bids, RRBid{Seat: seat, Moves: moves, Seq: g.bidSeq})
	first := g.Phase == RRPhaseThinking
	g.Phase = RRPhaseBidding
	if first {
		g.pushEvent("bid_first", seat, fmt.Sprintf("%s님이 %d회를 외쳤습니다 — 카운트다운 시작",
			g.Players[seat].Name, moves))
		return nil
	}
	g.pushEvent("bid", seat, fmt.Sprintf("%s님이 %d회를 외쳤습니다",
		g.Players[seat].Name, moves))
	return nil
}

// CloseBidding 카운트다운 종료 — 적게 외친 사람부터 증명 순서를 세운다
func (g *RRGame) CloseBidding() {
	if g.Phase != RRPhaseBidding {
		return
	}
	sorted := g.SortedBids()
	if len(sorted) == 0 { // 도달 불가 (bidding 은 외침이 하나 이상일 때만)
		g.resolveGoal(rrNobodyBid)
		return
	}

	// 정렬 결과를 g.Bids 자체에 반영해 DemoOrder 를 단순한 색인으로 둔다
	g.Bids = sorted
	g.DemoOrder = make([]int, len(sorted))
	for i := range sorted {
		g.DemoOrder[i] = i
	}
	g.DemoTurn = 0
	g.Phase = RRPhaseDemo

	seat := g.Bids[0].Seat
	g.pushEvent("bid_closed", seat, fmt.Sprintf("외치기 마감 — %s님이 %d회로 증명합니다",
		g.Players[seat].Name, g.Bids[0].Moves))
}

// Demo 증명 제출. 증명자만 보낼 수 있다. 성공하면 로봇은 증명이 끝난
// 자리에 그대로 남는다 (다음 목표는 그 배치에서 이어진다).
func (g *RRGame) Demo(seat int, moves []RRMove) error {
	if g.Phase != RRPhaseDemo {
		return errors.New("지금은 증명할 수 없습니다")
	}
	if seat != g.DemoSeat() {
		return errors.New("지금 증명할 차례가 아닙니다")
	}
	if len(moves) > RRMaxBid {
		return fmt.Errorf("이동은 %d개를 넘을 수 없습니다", RRMaxBid)
	}

	bid := g.DemoBid()
	after, ok, message := rrJudgeDemo(g.Board, g.Robots, g.Goal, moves, bid)
	player := g.Players[seat]
	if !ok {
		g.LastResult = &RRLastResult{Seat: seat, Name: player.Name, OK: false,
			Moves: len(moves), Message: message}
		g.pushEvent("demo_fail", seat, fmt.Sprintf("%s님 증명 실패 — %s", player.Name, message))
		g.advanceDemo()
		return nil
	}

	g.Robots = after
	player.Score++
	g.LastResult = &RRLastResult{Seat: seat, Name: player.Name, OK: true,
		Moves: len(moves), Message: message}
	g.pushEvent("demo_ok", seat, fmt.Sprintf("%s님 증명 성공! %s (%d회, 현재 %d점)",
		player.Name, rrGoalLabel(g.Goal), len(moves), player.Score))
	g.closeGoal()
	return nil
}

// Pass 증명 포기 — 다음으로 적게 외친 사람에게 넘긴다
func (g *RRGame) Pass(seat int) error {
	if g.Phase != RRPhaseDemo {
		return errors.New("지금은 포기할 수 없습니다")
	}
	if seat != g.DemoSeat() {
		return errors.New("지금 증명할 차례가 아닙니다")
	}
	g.failDemo(seat, rrDemoPassMsg)
	return nil
}

// DemoTimeout 증명 제한 시간 초과 — 실패와 같이 다룬다
func (g *RRGame) DemoTimeout() {
	if g.Phase != RRPhaseDemo {
		return
	}
	g.failDemo(g.DemoSeat(), rrDemoTimeUpMsg)
}

// GoalTimeout 아무도 외치지 못한 목표를 넘긴다 (thinking 상한)
func (g *RRGame) GoalTimeout() {
	if g.Phase != RRPhaseThinking {
		return
	}
	g.resolveGoal(rrNobodyBid)
}

// failDemo 증명 실패 처리 — 결과를 남기고 다음 증명자로 넘긴다
func (g *RRGame) failDemo(seat int, message string) {
	name := ""
	if seat >= 0 && seat < len(g.Players) {
		name = g.Players[seat].Name
	}
	g.LastResult = &RRLastResult{Seat: seat, Name: name, OK: false, Moves: 0, Message: message}
	g.pushEvent("demo_fail", seat, fmt.Sprintf("%s님 — %s", name, message))
	g.advanceDemo()
}

// advanceDemo 다음으로 적게 외친 사람에게 증명권을 넘긴다. 남은 사람이 없으면
// 그 목표는 아무도 못 가져간 채로 넘어간다.
func (g *RRGame) advanceDemo() {
	g.DemoTurn++
	if g.DemoTurn >= len(g.DemoOrder) {
		g.resolveGoal(rrNobodySolved)
		return
	}
	seat := g.DemoSeat()
	g.pushEvent("demo_turn", seat, fmt.Sprintf("%s님이 %d회로 증명합니다",
		g.Players[seat].Name, g.DemoBid()))
}

// resolveGoal 아무도 못 가져간 목표를 닫는다
func (g *RRGame) resolveGoal(message string) {
	g.LastResult = &RRLastResult{Seat: -1, Name: "", OK: false, Moves: 0, Message: message}
	g.pushEvent("goal_passed", -1, fmt.Sprintf("%d/%d번째 목표 지점 — %s (최소 %d회였습니다)",
		g.GoalIndex+1, RRGoalTotal, message, g.MinMoves))
	g.closeGoal()
}

// closeGoal 목표 정산 단계로 넘어간다 (허브가 마감을 걸고 NextGoal 을 부른다)
func (g *RRGame) closeGoal() {
	g.Phase = RRPhaseGoalEnd
}

// NextGoal 다음 목표를 연다. 성공·실패에 관계없이 목표는 하나씩 소진된다 —
// 되섞으면 종료 보증(17개 소진)이 무너지기 때문이다.
func (g *RRGame) NextGoal(rng *rand.Rand) {
	if g.Phase != RRPhaseGoalEnd {
		return
	}
	g.GoalIndex++
	if g.GoalIndex >= RRGoalTotal {
		g.finish("goals_done")
		return
	}
	if !g.openGoal(rng) {
		g.finish("no_goal")
		return
	}
	g.Phase = RRPhaseThinking
}

// ForceEnd 강제 종료 — 게임 전체 캡. 현재 점수 그대로 정산한다.
func (g *RRGame) ForceEnd() {
	if g.Phase == RRPhaseGameOver || g.Phase == RRPhaseWaiting {
		return
	}
	g.finish("time_up")
}

// rrWinners 최다 획득 좌석 목록 (동점이면 공동, 좌석 오름차순)
func rrWinners(players []*RRPlayer) ([]int, []string) {
	seats, names := []int{}, []string{}
	best := -1
	for _, p := range players {
		if p.Score > best {
			best = p.Score
		}
	}
	for _, p := range players {
		if p.Score == best {
			seats = append(seats, p.Seat)
			names = append(names, p.Name)
		}
	}
	return seats, names
}

// GoalsPlayed 지금까지 소진한 목표 수. 17개를 다 쓰면 GoalIndex 가
// RRGoalTotal 까지 올라가므로 그대로 세면 되고, 도중에 끝나면 진행 중이던
// 목표까지 센다.
func (g *RRGame) GoalsPlayed() int {
	if !g.Ready {
		return 0
	}
	if g.GoalIndex >= RRGoalTotal {
		return RRGoalTotal
	}
	return g.GoalIndex + 1
}

// finish 종료 판정 — 최다 획득 승, 동점이면 공동 승리
func (g *RRGame) finish(reason string) {
	seats, names := rrWinners(g.Players)
	tail := fmt.Sprintf("목표 토큰 %d개를 모두 소진했습니다", RRGoalTotal)
	switch reason {
	case "time_up":
		tail = "제한 시간이 끝났습니다"
	case "no_goal":
		tail = "더 낼 수 있는 목표 지점이 없습니다"
	}

	message := fmt.Sprintf("%s — 승자 없음", tail)
	if len(names) > 0 {
		best := g.Players[seats[0]].Score
		if len(names) == 1 {
			message = fmt.Sprintf("%s — %s님 승리! (목표 토큰 %d개 획득)", tail, names[0], best)
		} else {
			message = fmt.Sprintf("%s — %d명 공동 승리! (각 목표 토큰 %d개 획득)",
				tail, len(names), best)
		}
	}

	g.Phase = RRPhaseGameOver
	g.EndReason = reason
	g.Result = &RRResult{WinnerSeats: seats, WinnerNames: names, Message: message}
	g.pushEvent("game_over", -1, message)
}
