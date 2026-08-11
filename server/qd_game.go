package server

import (
	"errors"
	"math/rand"
	"time"
)

// NewQDGame 로비 상태의 새 게임
func NewQDGame(id string) *QDGame {
	return &QDGame{
		ID:    id,
		Names: map[QDSide]string{},
		Phase: QDPhaseLobby,
	}
}

// AddPlayer 입장. 먼저 온 사람이 남쪽.
func (g *QDGame) AddPlayer(name string) (QDSide, error) {
	if g.Phase != QDPhaseLobby {
		return "", errors.New("이미 시작된 게임입니다")
	}
	if _, ok := g.Names[QDSouth]; !ok {
		g.Names[QDSouth] = name
		return QDSouth, nil
	}
	if _, ok := g.Names[QDNorth]; !ok {
		g.Names[QDNorth] = name
		return QDNorth, nil
	}
	return "", errors.New("자리가 없습니다")
}

// IsReady 게임 시작 준비 확인
func (g *QDGame) IsReady() bool {
	return len(g.Names) == 2
}

// qdGoalRow 진영의 목표 줄 (상대편 뒷줄)
func qdGoalRow(side QDSide) int {
	if side == QDSouth {
		return 0
	}
	return QDBoardSize - 1
}

// Start 폰을 시작 위치에 놓고 플레이를 시작한다. 선공은 랜덤.
func (g *QDGame) Start(rng *rand.Rand) error {
	if !g.IsReady() {
		return errors.New("시작할 수 없습니다 (2명 필요)")
	}

	mid := QDBoardSize / 2
	g.Pawns = map[QDSide]QDCell{
		QDSouth: {Row: QDBoardSize - 1, Col: mid},
		QDNorth: {Row: 0, Col: mid},
	}
	g.WallsLeft = map[QDSide]int{QDSouth: QDWallCount, QDNorth: QDWallCount}
	g.Walls = []QDWall{}

	g.CurrentSide = QDSouth
	if rng.Intn(2) == 1 {
		g.CurrentSide = QDNorth
	}
	g.Phase = QDPhasePlay
	g.Ready = true
	g.StartedAt = time.Now()
	return nil
}

// ==================== 벽 / 이동 판정 ====================

// qdBlocked 인접한 두 칸 사이가 벽으로 막혀 있는지.
// 가로 벽 (r,c)는 (r,c)·(r,c+1) ↔ (r+1,c)·(r+1,c+1) 사이를,
// 세로 벽 (r,c)는 (r,c)·(r+1,c) ↔ (r,c+1)·(r+1,c+1) 사이를 막는다.
func qdBlocked(walls []QDWall, from, to QDCell) bool {
	for _, w := range walls {
		if w.Orientation == "h" {
			// 위아래 이동만 막는다
			upper := from
			if to.Row < from.Row {
				upper = to
			}
			if from.Col == to.Col && from.Row != to.Row &&
				w.Row == upper.Row && (w.Col == from.Col || w.Col == from.Col-1) {
				return true
			}
		} else {
			// 좌우 이동만 막는다
			left := from
			if to.Col < from.Col {
				left = to
			}
			if from.Row == to.Row && from.Col != to.Col &&
				w.Col == left.Col && (w.Row == from.Row || w.Row == from.Row-1) {
				return true
			}
		}
	}
	return false
}

func qdInBoard(c QDCell) bool {
	return c.Row >= 0 && c.Row < QDBoardSize && c.Col >= 0 && c.Col < QDBoardSize
}

var qdDirs = []QDCell{{Row: -1}, {Row: 1}, {Col: -1}, {Col: 1}}

// LegalPawnMoves side 폰의 합법 이동 칸: 직교 1칸 + 마주보기 점프 + 벽·모서리 시 대각 우회.
func (g *QDGame) LegalPawnMoves(side QDSide) []QDCell {
	me := g.Pawns[side]
	opp := g.Pawns[qdOther(side)]
	moves := []QDCell{}

	for _, d := range qdDirs {
		next := QDCell{Row: me.Row + d.Row, Col: me.Col + d.Col}
		if !qdInBoard(next) || qdBlocked(g.Walls, me, next) {
			continue
		}
		if next != opp {
			moves = append(moves, next)
			continue
		}
		// 상대 폰과 마주봄: 직선 점프 시도
		jump := QDCell{Row: next.Row + d.Row, Col: next.Col + d.Col}
		if qdInBoard(jump) && !qdBlocked(g.Walls, next, jump) {
			moves = append(moves, jump)
			continue
		}
		// 점프 불가(뒤가 벽이거나 보드 밖): 상대 폰 기준 대각 우회
		for _, p := range qdDirs {
			// 원래 진행 방향과 수직인 방향만
			if (d.Row != 0 && p.Row != 0) || (d.Col != 0 && p.Col != 0) {
				continue
			}
			diag := QDCell{Row: next.Row + p.Row, Col: next.Col + p.Col}
			if qdInBoard(diag) && !qdBlocked(g.Walls, next, diag) {
				moves = append(moves, diag)
			}
		}
	}
	return moves
}

// qdHasPath 벽 목록을 전제로 start 에서 goalRow 까지 경로가 존재하는지 (BFS).
// 폰 위치는 무시한다 — 벽 규칙은 "경로의 존재"만 요구한다.
func qdHasPath(walls []QDWall, start QDCell, goalRow int) bool {
	visited := [QDBoardSize][QDBoardSize]bool{}
	queue := []QDCell{start}
	visited[start.Row][start.Col] = true
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.Row == goalRow {
			return true
		}
		for _, d := range qdDirs {
			next := QDCell{Row: cur.Row + d.Row, Col: cur.Col + d.Col}
			if !qdInBoard(next) || visited[next.Row][next.Col] || qdBlocked(walls, cur, next) {
				continue
			}
			visited[next.Row][next.Col] = true
			queue = append(queue, next)
		}
	}
	return false
}

// PlaceWall 벽 설치. 겹침·교차·경로 차단을 모두 검증한 뒤에만 상태를 바꾼다.
func (g *QDGame) PlaceWall(side QDSide, wall QDWall) error {
	if g.Phase != QDPhasePlay {
		return errors.New("지금은 벽을 놓을 수 없습니다")
	}
	if side != g.CurrentSide {
		return errors.New("당신의 차례가 아닙니다")
	}
	if wall.Orientation != "h" && wall.Orientation != "v" {
		return errors.New("벽 방향이 올바르지 않습니다")
	}
	if wall.Row < 0 || wall.Row > QDBoardSize-2 || wall.Col < 0 || wall.Col > QDBoardSize-2 {
		return errors.New("벽을 놓을 수 없는 위치입니다")
	}
	if g.WallsLeft[side] <= 0 {
		return errors.New("남은 벽이 없습니다")
	}

	for _, w := range g.Walls {
		if w.Orientation == wall.Orientation {
			if wall.Orientation == "h" {
				if w.Row == wall.Row && w.Col >= wall.Col-1 && w.Col <= wall.Col+1 {
					return errors.New("이미 벽이 있는 자리입니다")
				}
			} else {
				if w.Col == wall.Col && w.Row >= wall.Row-1 && w.Row <= wall.Row+1 {
					return errors.New("이미 벽이 있는 자리입니다")
				}
			}
		} else if w.Row == wall.Row && w.Col == wall.Col {
			// 같은 교차점에서 가로·세로가 교차
			return errors.New("다른 벽과 교차할 수 없습니다")
		}
	}

	// 어느 쪽 폰도 목표 줄까지의 경로가 완전히 막히면 안 된다
	candidate := append(append([]QDWall{}, g.Walls...), wall)
	for _, s := range []QDSide{QDSouth, QDNorth} {
		if !qdHasPath(candidate, g.Pawns[s], qdGoalRow(s)) {
			return errors.New("상대 또는 내 경로를 완전히 막을 수 없습니다")
		}
	}

	g.Walls = append(g.Walls, wall)
	g.WallsLeft[side]--
	g.CurrentSide = qdOther(side)
	return nil
}

// QDMoveResult 폰 이동 한 번의 결과
type QDMoveResult struct {
	From     QDCell
	GameOver bool
}

// MovePawn 폰 이동. 목표 줄 도달 시 즉시 승리.
func (g *QDGame) MovePawn(side QDSide, to QDCell) (*QDMoveResult, error) {
	if g.Phase != QDPhasePlay {
		return nil, errors.New("지금은 이동할 수 없습니다")
	}
	if side != g.CurrentSide {
		return nil, errors.New("당신의 차례가 아닙니다")
	}

	legal := false
	for _, m := range g.LegalPawnMoves(side) {
		if m == to {
			legal = true
			break
		}
	}
	if !legal {
		return nil, errors.New("그 칸으로는 이동할 수 없습니다")
	}

	result := &QDMoveResult{From: g.Pawns[side]}
	g.Pawns[side] = to

	if to.Row == qdGoalRow(side) {
		g.Winner = side
		g.EndReason = "reach_goal"
		g.Phase = QDPhaseGameOver
		result.GameOver = true
		return result, nil
	}

	g.CurrentSide = qdOther(side)
	return result, nil
}
