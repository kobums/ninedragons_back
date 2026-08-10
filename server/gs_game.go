package server

import (
	"errors"
	"math/rand"
	"time"
)

// NewGSGame 로비 상태의 새 게임
func NewGSGame(id string) *GSGame {
	return &GSGame{
		ID:        id,
		Names:     map[GSSide]string{},
		SetupDone: map[GSSide]bool{},
		Phase:     GSPhaseLobby,
	}
}

// AddPlayer 입장. 먼저 온 사람이 남쪽.
func (g *GSGame) AddPlayer(name string) (GSSide, error) {
	if g.Phase != GSPhaseLobby {
		return "", errors.New("이미 시작된 게임입니다")
	}
	if _, ok := g.Names[GSSouth]; !ok {
		g.Names[GSSouth] = name
		return GSSouth, nil
	}
	if _, ok := g.Names[GSNorth]; !ok {
		g.Names[GSNorth] = name
		return GSNorth, nil
	}
	return "", errors.New("자리가 없습니다")
}

// IsReady 게임 시작 준비 확인
func (g *GSGame) IsReady() bool {
	return len(g.Names) == 2
}

// gsSetupCells 진영의 배치 구역 8칸: 자기 쪽 뒷 2줄 × 가운데 4열.
// 남쪽은 행 4~5, 북쪽은 행 0~1, 열 1~4.
func gsSetupCells(side GSSide) []GSCell {
	rows := []int{4, 5}
	if side == GSNorth {
		rows = []int{0, 1}
	}
	cells := []GSCell{}
	for _, r := range rows {
		for c := 1; c <= 4; c++ {
			cells = append(cells, GSCell{Row: r, Col: c})
		}
	}
	return cells
}

// gsExits 진영의 탈출구: 상대편 뒷줄의 양 모서리 2칸.
func gsExits(side GSSide) []GSCell {
	row := 0
	if side == GSNorth {
		row = 5
	}
	return []GSCell{{Row: row, Col: 0}, {Row: row, Col: GSBoardSize - 1}}
}

// Start 배치 구역에 유령을 깔고 비밀 배치(setup) 단계로 들어간다.
// 정체(Good)는 SubmitSetup 에서 각자 정한다. 선공은 랜덤.
func (g *GSGame) Start(rng *rand.Rand) error {
	if !g.IsReady() {
		return errors.New("시작할 수 없습니다 (2명 필요)")
	}

	id := 0
	for _, side := range []GSSide{GSSouth, GSNorth} {
		for _, cell := range gsSetupCells(side) {
			g.Ghosts = append(g.Ghosts, &GSGhost{ID: id, Side: side, Row: cell.Row, Col: cell.Col})
			id++
		}
	}

	g.CurrentSide = GSSouth
	if rng.Intn(2) == 1 {
		g.CurrentSide = GSNorth
	}
	g.Phase = GSPhaseSetup
	g.Ready = true
	g.StartedAt = time.Now()
	return nil
}

// ghostAt 해당 칸의 살아있는(잡히지 않고 탈출하지 않은) 유령
func (g *GSGame) ghostAt(row, col int) *GSGhost {
	for _, ghost := range g.Ghosts {
		if !ghost.Captured && !ghost.Escaped && ghost.Row == row && ghost.Col == col {
			return ghost
		}
	}
	return nil
}

// SubmitSetup 좋은 유령 4개의 위치를 비밀 제출. 양측 완료 시 play 로.
func (g *GSGame) SubmitSetup(side GSSide, goodCells []GSCell) error {
	if g.Phase != GSPhaseSetup {
		return errors.New("지금은 배치할 수 없습니다")
	}
	if g.SetupDone[side] {
		return errors.New("이미 배치를 마쳤습니다")
	}
	if len(goodCells) != GSGoodCount {
		return errors.New("좋은 유령 4개의 위치를 골라야 합니다")
	}

	// 전부 내 배치 구역 안이고 중복이 없어야 한다
	seen := map[GSCell]bool{}
	for _, cell := range goodCells {
		if seen[cell] {
			return errors.New("같은 칸을 두 번 고를 수 없습니다")
		}
		seen[cell] = true
		ghost := g.ghostAt(cell.Row, cell.Col)
		if ghost == nil || ghost.Side != side {
			return errors.New("내 배치 구역의 칸이 아닙니다")
		}
	}

	for _, ghost := range g.Ghosts {
		if ghost.Side == side {
			ghost.Good = seen[GSCell{Row: ghost.Row, Col: ghost.Col}]
		}
	}
	g.SetupDone[side] = true

	if g.SetupDone[GSSouth] && g.SetupDone[GSNorth] {
		g.Phase = GSPhasePlay
	}
	return nil
}

// GSMoveResult 이동 한 번의 결과
type GSMoveResult struct {
	Captured     bool
	CapturedGood bool
	Escaped      bool
	GameOver     bool
}

// isExit 해당 칸이 side 의 탈출구인지
func gsIsExit(side GSSide, row, col int) bool {
	for _, e := range gsExits(side) {
		if e.Row == row && e.Col == col {
			return true
		}
	}
	return false
}

// capturedCount side 진영의 유령 중 잡힌 (좋은/나쁜) 개수
func (g *GSGame) capturedCount(side GSSide, good bool) int {
	n := 0
	for _, ghost := range g.Ghosts {
		if ghost.Side == side && ghost.Captured && ghost.Good == good {
			n++
		}
	}
	return n
}

// Move 유령 이동·잡기·탈출. escape 면 from 의 유령을 보드 밖으로 내보낸다.
func (g *GSGame) Move(side GSSide, from, to GSCell, escape bool) (*GSMoveResult, error) {
	if g.Phase != GSPhasePlay {
		return nil, errors.New("지금은 이동할 수 없습니다")
	}
	if side != g.CurrentSide {
		return nil, errors.New("당신의 차례가 아닙니다")
	}

	ghost := g.ghostAt(from.Row, from.Col)
	if ghost == nil || ghost.Side != side {
		return nil, errors.New("그 칸에 내 유령이 없습니다")
	}

	result := &GSMoveResult{}

	if escape {
		// 탈출: 내 탈출구(상대편 모서리)에 있는 좋은 유령만
		if !gsIsExit(side, from.Row, from.Col) {
			return nil, errors.New("탈출구에서만 탈출할 수 있습니다")
		}
		if !ghost.Good {
			return nil, errors.New("좋은 유령만 탈출할 수 있습니다")
		}
		ghost.Escaped = true
		g.Winner = side
		g.EndReason = "escape"
		g.Phase = GSPhaseGameOver
		result.Escaped = true
		result.GameOver = true
		return result, nil
	}

	if to.Row < 0 || to.Row >= GSBoardSize || to.Col < 0 || to.Col >= GSBoardSize {
		return nil, errors.New("보드 밖으로는 이동할 수 없습니다")
	}
	dr, dc := to.Row-from.Row, to.Col-from.Col
	if dr < 0 {
		dr = -dr
	}
	if dc < 0 {
		dc = -dc
	}
	if dr+dc != 1 {
		return nil, errors.New("상하좌우 한 칸만 이동할 수 있습니다")
	}

	target := g.ghostAt(to.Row, to.Col)
	if target != nil && target.Side == side {
		return nil, errors.New("내 유령이 있는 칸으로는 이동할 수 없습니다")
	}

	if target != nil {
		target.Captured = true
		result.Captured = true
		result.CapturedGood = target.Good

		if target.Good && g.capturedCount(target.Side, true) == GSGoodCount {
			// 상대의 좋은 유령을 모두 잡았다 → 내가 승리
			g.Winner = side
			g.EndReason = "captured_all_good"
			g.Phase = GSPhaseGameOver
			result.GameOver = true
		} else if !target.Good && g.capturedCount(target.Side, false) == GSGoodCount {
			// 상대의 나쁜 유령을 모두 잡아버렸다 → 상대가 승리 (독이 든 미끼)
			g.Winner = target.Side
			g.EndReason = "fed_all_evil"
			g.Phase = GSPhaseGameOver
			result.GameOver = true
		}
	}

	ghost.Row, ghost.Col = to.Row, to.Col

	if !result.GameOver {
		g.CurrentSide = gsOther(side)
	}
	return result, nil
}
