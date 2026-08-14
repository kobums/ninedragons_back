package server

import (
	"errors"
	"math/rand"
	"sort"
	"time"
)

// NewCSGame 로비 상태의 새 게임
func NewCSGame(id string) *CSGame {
	return &CSGame{
		ID:    id,
		Names: map[CSSide]string{},
		Phase: CSPhaseLobby,
	}
}

// AddPlayer 입장. 먼저 온 사람이 남쪽.
func (g *CSGame) AddPlayer(name string) (CSSide, error) {
	if g.Phase != CSPhaseLobby {
		return "", errors.New("이미 시작된 게임입니다")
	}
	if _, ok := g.Names[CSSouth]; !ok {
		g.Names[CSSouth] = name
		return CSSouth, nil
	}
	if _, ok := g.Names[CSNorth]; !ok {
		g.Names[CSNorth] = name
		return CSNorth, nil
	}
	return "", errors.New("자리가 없습니다")
}

// IsReady 게임 시작 준비 확인
func (g *CSGame) IsReady() bool {
	return len(g.Names) == 2
}

// Start 플레이를 시작한다. 선공은 랜덤.
func (g *CSGame) Start(rng *rand.Rand) error {
	if !g.IsReady() {
		return errors.New("시작할 수 없습니다 (2명 필요)")
	}

	g.Progress = map[CSSide]map[int]int{CSSouth: {}, CSNorth: {}}
	g.Claimed = map[int]CSSide{}
	g.Temp = map[int]int{}

	g.CurrentSide = CSSouth
	if rng.Intn(2) == 1 {
		g.CurrentSide = CSNorth
	}
	g.Phase = CSPhasePlay
	g.Ready = true
	g.StartedAt = time.Now()
	return nil
}

// markerPos 이번 턴 기준 컬럼의 현재 위치 (임시 마커 우선, 없으면 확정 진행도)
func (g *CSGame) markerPos(side CSSide, col int) int {
	if v, ok := g.Temp[col]; ok {
		return v
	}
	return g.Progress[side][col]
}

// playable side 가 이번 굴림으로 col 을 한 칸 전진할 수 있는지
func (g *CSGame) playable(side CSSide, col int) bool {
	if _, claimed := g.Claimed[col]; claimed {
		return false
	}
	if g.markerPos(side, col) >= csColLen(col) {
		return false
	}
	if _, has := g.Temp[col]; !has && len(g.Temp) >= CSMarkerMax {
		return false
	}
	return true
}

// csOptionKey 정렬한 합들의 중복 제거용 키
func csOptionKey(sums []int) [2]int {
	key := [2]int{sums[0], -1}
	if len(sums) == 2 {
		key[1] = sums[1]
	}
	return key
}

// computeOptions 4개 주사위의 세 가지 짝짓기에서 고를 수 있는 전진 목록.
// 한 짝짓기에서 두 합이 모두 가능하면 둘 다 써야 하고(원작 규칙),
// 마커가 모자라 하나만 쓸 수 있으면 어느 쪽을 쓸지 고른다.
func (g *CSGame) computeOptions(side CSSide) []CSOption {
	d := g.Dice
	pairings := [][2]int{
		{d[0] + d[1], d[2] + d[3]},
		{d[0] + d[2], d[1] + d[3]},
		{d[0] + d[3], d[1] + d[2]},
	}

	seen := map[[2]int]bool{}
	options := []CSOption{}
	add := func(sums ...int) {
		sort.Ints(sums)
		key := csOptionKey(sums)
		if seen[key] {
			return
		}
		seen[key] = true
		options = append(options, CSOption{Sums: sums})
	}

	for _, p := range pairings {
		s1, s2 := p[0], p[1]
		p1, p2 := g.playable(side, s1), g.playable(side, s2)
		switch {
		case p1 && p2:
			newCols := 0
			if _, has := g.Temp[s1]; !has {
				newCols++
			}
			if s2 != s1 {
				if _, has := g.Temp[s2]; !has {
					newCols++
				}
			}
			if len(g.Temp)+newCols <= CSMarkerMax {
				add(s1, s2)
			} else {
				// 새 마커 자리가 하나뿐 — 어느 합을 쓸지 골라야 한다
				add(s1)
				add(s2)
			}
		case p1:
			add(s1)
		case p2:
			add(s2)
		}
	}
	return options
}

// CSRollResult 굴림 한 번의 결과
type CSRollResult struct {
	Dice   []int
	Busted bool
}

// Roll 주사위 4개를 굴린다. 쓸 수 있는 조합이 없으면 버스트 —
// 이번 턴의 전진을 모두 잃고 턴이 넘어간다.
func (g *CSGame) Roll(side CSSide, rng *rand.Rand) (*CSRollResult, error) {
	if g.Phase != CSPhasePlay {
		return nil, errors.New("지금은 굴릴 수 없습니다")
	}
	if side != g.CurrentSide {
		return nil, errors.New("당신의 차례가 아닙니다")
	}
	if g.Dice != nil {
		return nil, errors.New("먼저 조합을 골라야 합니다")
	}

	dice := make([]int, CSDiceCount)
	for i := range dice {
		dice[i] = rng.Intn(6) + 1
	}
	g.Dice = dice

	options := g.computeOptions(side)
	result := &CSRollResult{Dice: dice}

	if len(options) == 0 {
		// 버스트: 임시 전진 소멸, 턴 종료
		g.Temp = map[int]int{}
		g.Dice = nil
		g.Options = nil
		g.CurrentSide = csOther(side)
		result.Busted = true
		return result, nil
	}

	g.Options = options
	return result, nil
}

// Choose 굴림의 조합 하나를 골라 전진한다
func (g *CSGame) Choose(side CSSide, sums []int) error {
	if g.Phase != CSPhasePlay {
		return errors.New("지금은 고를 수 없습니다")
	}
	if side != g.CurrentSide {
		return errors.New("당신의 차례가 아닙니다")
	}
	if g.Dice == nil {
		return errors.New("먼저 주사위를 굴려야 합니다")
	}

	sorted := append([]int{}, sums...)
	sort.Ints(sorted)
	matched := false
	for _, opt := range g.Options {
		if csOptionKey(opt.Sums) == csOptionKey(sorted) && len(opt.Sums) == len(sorted) {
			matched = true
			break
		}
	}
	if !matched {
		return errors.New("고를 수 없는 조합입니다")
	}

	for _, sum := range sorted {
		pos := g.markerPos(side, sum)
		if pos < csColLen(sum) {
			g.Temp[sum] = pos + 1
		}
		// 꼭대기에 이미 닿았으면 그 합의 추가 전진은 소멸 (원작 규칙)
	}

	g.Dice = nil
	g.Options = nil
	return nil
}

// CSStopResult 정지(뱅킹) 결과
type CSStopResult struct {
	ClaimedCols []int
	GameOver    bool
}

// Stop 이번 턴의 전진을 확정하고 턴을 넘긴다. 꼭대기에 닿은 컬럼은
// 완등 — 양쪽 모두에게 닫힌다. 3개를 완등하면 즉시 승리.
func (g *CSGame) Stop(side CSSide) (*CSStopResult, error) {
	if g.Phase != CSPhasePlay {
		return nil, errors.New("지금은 멈출 수 없습니다")
	}
	if side != g.CurrentSide {
		return nil, errors.New("당신의 차례가 아닙니다")
	}
	if g.Dice != nil {
		return nil, errors.New("먼저 조합을 골라야 합니다")
	}
	if len(g.Temp) == 0 {
		return nil, errors.New("한 번은 전진한 뒤에 멈출 수 있습니다")
	}

	result := &CSStopResult{ClaimedCols: []int{}}
	for col, pos := range g.Temp {
		g.Progress[side][col] = pos
		if pos >= csColLen(col) {
			g.Claimed[col] = side
			result.ClaimedCols = append(result.ClaimedCols, col)
		}
	}
	sort.Ints(result.ClaimedCols)
	g.Temp = map[int]int{}

	mine := 0
	for _, owner := range g.Claimed {
		if owner == side {
			mine++
		}
	}
	if mine >= CSClaimToWin {
		g.Winner = side
		g.EndReason = "claimed_three"
		g.Phase = CSPhaseGameOver
		result.GameOver = true
		return result, nil
	}

	g.CurrentSide = csOther(side)
	return result, nil
}

// ClaimedBy side 가 완등한 컬럼 목록 (정렬)
func (g *CSGame) ClaimedBy(side CSSide) []int {
	cols := []int{}
	for col, owner := range g.Claimed {
		if owner == side {
			cols = append(cols, col)
		}
	}
	sort.Ints(cols)
	return cols
}
