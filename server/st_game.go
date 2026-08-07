package server

import (
	"errors"
	"math/rand"
	"sort"
	"time"
)

// 족보 카테고리 (클수록 강함)
const (
	stFormationSum      = 0 // 합계
	stFormationRun      = 1 // 런 (연속 3장)
	stFormationColor    = 2 // 컬러 (같은 색 3장)
	stFormationTriple   = 3 // 트리플 (같은 숫자 3장)
	stFormationColorRun = 4 // 컬러런 (같은 색 연속 3장)
)

// stFormation 완성된 3장의 평가 결과
type stFormation struct {
	Category int
	Sum      int
}

// beats a 가 b 를 이기는지. aFirst 는 족보·합이 모두 같을 때 a 쪽이
// 먼저 완성했는지 (공식 룰: 먼저 3장을 완성한 쪽 승리).
func (a stFormation) beats(b stFormation, aFirst bool) bool {
	if a.Category != b.Category {
		return a.Category > b.Category
	}
	if a.Sum != b.Sum {
		return a.Sum > b.Sum
	}
	return aFirst
}

// stEvaluate 3장 카드의 족보 평가
func stEvaluate(cards []STCard) stFormation {
	ranks := []int{cards[0].Rank, cards[1].Rank, cards[2].Rank}
	sort.Ints(ranks)
	sum := ranks[0] + ranks[1] + ranks[2]

	sameColor := cards[0].Color == cards[1].Color && cards[1].Color == cards[2].Color
	isRun := ranks[0]+1 == ranks[1] && ranks[1]+1 == ranks[2]
	isTriple := ranks[0] == ranks[2]

	category := stFormationSum
	switch {
	case sameColor && isRun:
		category = stFormationColorRun
	case isTriple:
		category = stFormationTriple
	case sameColor:
		category = stFormationColor
	case isRun:
		category = stFormationRun
	}
	return stFormation{Category: category, Sum: sum}
}

// stOther 반대 진영
func stOther(side STSide) STSide {
	if side == STSouth {
		return STNorth
	}
	return STSouth
}

// NewSTGame 로비 상태의 새 게임
func NewSTGame(id string) *STGame {
	stones := make([]*STStone, STStoneCount)
	for i := range stones {
		stones[i] = &STStone{
			Cards:          map[STSide][]STCard{},
			CompletedOrder: map[STSide]int{},
		}
	}
	return &STGame{
		ID:     id,
		Names:  map[STSide]string{},
		Hands:  map[STSide][]STCard{},
		Stones: stones,
		Phase:  STPhaseLobby,
	}
}

// AddPlayer 입장. 남쪽부터 채운다.
func (g *STGame) AddPlayer(name string) (STSide, error) {
	if g.Phase != STPhaseLobby {
		return "", errors.New("이미 시작된 게임입니다")
	}
	if _, ok := g.Names[STSouth]; !ok {
		g.Names[STSouth] = name
		return STSouth, nil
	}
	if _, ok := g.Names[STNorth]; !ok {
		g.Names[STNorth] = name
		return STNorth, nil
	}
	return "", errors.New("자리가 없습니다")
}

// IsReady 게임 시작 준비 확인
func (g *STGame) IsReady() bool {
	return len(g.Names) == 2
}

// Start 셔플·배분 후 게임 시작
func (g *STGame) Start(rng *rand.Rand) error {
	if !g.IsReady() {
		return errors.New("시작할 수 없습니다 (2명 필요)")
	}

	deck := make([]STCard, 0, STColorCount*STMaxRank)
	for c := 0; c < STColorCount; c++ {
		for r := 1; r <= STMaxRank; r++ {
			deck = append(deck, STCard{Color: c, Rank: r})
		}
	}
	rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })

	for _, side := range []STSide{STSouth, STNorth} {
		g.Hands[side] = append([]STCard{}, deck[:STHandSize]...)
		deck = deck[STHandSize:]
	}
	g.Deck = deck

	if rng.Intn(2) == 0 {
		g.CurrentSide = STSouth
	} else {
		g.CurrentSide = STNorth
	}
	g.Phase = STPhasePlay
	g.Ready = true
	g.StartedAt = time.Now()
	return nil
}

// PlayCard 손패의 카드를 돌에 낸다. 낸 뒤 바로 드로우하고, 획득 가능한
// 돌이 있으면 claim 단계로, 없으면 턴을 넘긴다.
// (공식 룰은 내기 → 획득 → 드로우 순서지만, 획득 판정은 손패와 무관하므로
// 드로우를 앞당겨도 게임에 영향이 없다.)
func (g *STGame) PlayCard(side STSide, handIndex, stoneIndex int) error {
	if g.Phase != STPhasePlay {
		return errors.New("지금은 카드를 낼 수 없습니다")
	}
	if side != g.CurrentSide {
		return errors.New("당신의 차례가 아닙니다")
	}
	hand := g.Hands[side]
	if handIndex < 0 || handIndex >= len(hand) {
		return errors.New("없는 카드입니다")
	}
	if stoneIndex < 0 || stoneIndex >= STStoneCount {
		return errors.New("없는 돌입니다")
	}
	stone := g.Stones[stoneIndex]
	if stone.Owner != "" {
		return errors.New("이미 획득된 돌입니다")
	}
	if len(stone.Cards[side]) >= 3 {
		return errors.New("이 돌에는 더 낼 수 없습니다")
	}

	card := hand[handIndex]
	g.Hands[side] = append(hand[:handIndex:handIndex], hand[handIndex+1:]...)
	stone.Cards[side] = append(stone.Cards[side], card)
	if len(stone.Cards[side]) == 3 {
		g.completionCounter++
		stone.CompletedOrder[side] = g.completionCounter
	}

	// 드로우
	if len(g.Deck) > 0 {
		g.Hands[side] = append(g.Hands[side], g.Deck[len(g.Deck)-1])
		g.Deck = g.Deck[:len(g.Deck)-1]
	}

	if len(g.ClaimableStones(side)) > 0 {
		g.Phase = STPhaseClaim
	} else {
		g.nextTurn()
	}
	return nil
}

// ClaimStone 돌 획득. 획득 후 남은 획득 가능 돌이 없으면 자동으로 턴 종료.
func (g *STGame) ClaimStone(side STSide, stoneIndex int) error {
	if g.Phase != STPhaseClaim {
		return errors.New("지금은 돌을 가져올 수 없습니다")
	}
	if side != g.CurrentSide {
		return errors.New("당신의 차례가 아닙니다")
	}
	if stoneIndex < 0 || stoneIndex >= STStoneCount {
		return errors.New("없는 돌입니다")
	}
	if !g.isClaimable(stoneIndex, side) {
		return errors.New("가져올 수 없는 돌입니다")
	}

	g.Stones[stoneIndex].Owner = side

	if reason, over := g.checkVictory(side); over {
		g.Winner = side
		g.EndReason = reason
		g.Phase = STPhaseGameOver
		return nil
	}

	if len(g.ClaimableStones(side)) == 0 {
		g.nextTurn()
	}
	return nil
}

// EndTurn claim 단계에서 (남은 돌을 가져오지 않고) 턴을 넘긴다
func (g *STGame) EndTurn(side STSide) error {
	if g.Phase != STPhaseClaim {
		return errors.New("지금은 턴을 넘길 수 없습니다")
	}
	if side != g.CurrentSide {
		return errors.New("당신의 차례가 아닙니다")
	}
	g.nextTurn()
	return nil
}

// canPlay 손패와 놓을 자리가 모두 있는지
func (g *STGame) canPlay(side STSide) bool {
	if len(g.Hands[side]) == 0 {
		return false
	}
	for _, stone := range g.Stones {
		if stone.Owner == "" && len(stone.Cards[side]) < 3 {
			return true
		}
	}
	return false
}

// nextTurn 턴을 넘긴다. 새 턴 플레이어가 카드를 낼 수 없으면 획득만
// 허용하고, 그마저 없으면 자동으로 패스한다. 양쪽 모두 아무것도 할 수
// 없으면 돌 개수로 승부를 가른다 (덱 소진 후반 교착 방지용 하우스 룰).
func (g *STGame) nextTurn() {
	for attempts := 0; attempts < 2; attempts++ {
		g.CurrentSide = stOther(g.CurrentSide)
		if g.canPlay(g.CurrentSide) {
			g.Phase = STPhasePlay
			return
		}
		if len(g.ClaimableStones(g.CurrentSide)) > 0 {
			g.Phase = STPhaseClaim
			return
		}
	}

	// 교착: 돌을 많이 가진 쪽 승리, 같으면 무승부
	south, north := g.stoneCount(STSouth), g.stoneCount(STNorth)
	if south > north {
		g.Winner = STSouth
	} else if north > south {
		g.Winner = STNorth
	}
	g.EndReason = "stalemate"
	g.Phase = STPhaseGameOver
}

// stoneCount 진영이 획득한 돌 개수
func (g *STGame) stoneCount(side STSide) int {
	count := 0
	for _, stone := range g.Stones {
		if stone.Owner == side {
			count++
		}
	}
	return count
}

// checkVictory 인접 3개 또는 5개 획득 여부
func (g *STGame) checkVictory(side STSide) (string, bool) {
	adjacent := 0
	for _, stone := range g.Stones {
		if stone.Owner == side {
			adjacent++
			if adjacent >= 3 {
				return "three_adjacent", true
			}
		} else {
			adjacent = 0
		}
	}
	if g.stoneCount(side) >= 5 {
		return "five_stones", true
	}
	return "", false
}

// ClaimableStones side 가 지금 획득을 주장할 수 있는 돌 목록
func (g *STGame) ClaimableStones(side STSide) []int {
	claimable := []int{}
	for i := range g.Stones {
		if g.isClaimable(i, side) {
			claimable = append(claimable, i)
		}
	}
	return claimable
}

// isClaimable side 가 stoneIndex 돌을 가져올 수 있는지.
// 자기 쪽 3장이 완성돼 있어야 하고, 상대가 3장을 채웠으면 족보를 직접
// 비교하며, 못 채웠으면 "공개된 카드만으로는 상대가 어떤 카드를 놓아도
// 이길 수 없음"이 증명돼야 한다 (선점 증명, 공식 룰).
func (g *STGame) isClaimable(stoneIndex int, side STSide) bool {
	stone := g.Stones[stoneIndex]
	if stone.Owner != "" {
		return false
	}
	mine := stone.Cards[side]
	if len(mine) != 3 {
		return false
	}
	myFormation := stEvaluate(mine)

	opp := stOther(side)
	oppCards := stone.Cards[opp]

	if len(oppCards) == 3 {
		myFirst := stone.CompletedOrder[side] < stone.CompletedOrder[opp]
		return myFormation.beats(stEvaluate(oppCards), myFirst)
	}

	// 선점 증명: 보드에 공개되지 않은 모든 카드(상대 손패·덱·내 손패 포함 —
	// 내 손패는 증명 근거로 쓸 수 없으므로 상대가 쓸 수 있는 것으로 취급)로
	// 상대의 남은 슬롯을 채우는 모든 경우를 검사한다.
	pool := g.unseenCards()
	// 내가 먼저 완성했으므로 상대는 동점으로는 이길 수 없다 → 상대가
	// 엄격히 강한 조합을 만들 수 있으면 증명 실패.
	return !stCanBeat(oppCards, pool, myFormation)
}

// unseenCards 어느 돌에도 공개되지 않은 카드 전체
func (g *STGame) unseenCards() []STCard {
	onBoard := map[STCard]bool{}
	for _, stone := range g.Stones {
		for _, cards := range stone.Cards {
			for _, c := range cards {
				onBoard[c] = true
			}
		}
	}
	pool := []STCard{}
	for c := 0; c < STColorCount; c++ {
		for r := 1; r <= STMaxRank; r++ {
			card := STCard{Color: c, Rank: r}
			if !onBoard[card] {
				pool = append(pool, card)
			}
		}
	}
	return pool
}

// stCanBeat existing(0~2장)을 pool 의 카드로 3장까지 채워 target 을
// 엄격히 이기는 조합을 만들 수 있는지 (동점은 target 승리로 간주)
func stCanBeat(existing []STCard, pool []STCard, target stFormation) bool {
	need := 3 - len(existing)
	cards := make([]STCard, 3)
	copy(cards, existing)

	switch need {
	case 0:
		return stEvaluate(cards).beats(target, false)
	case 1:
		for _, c := range pool {
			cards[2] = c
			if stEvaluate(cards).beats(target, false) {
				return true
			}
		}
	case 2:
		for i := 0; i < len(pool); i++ {
			for j := i + 1; j < len(pool); j++ {
				cards[1], cards[2] = pool[i], pool[j]
				if stEvaluate(cards).beats(target, false) {
					return true
				}
			}
		}
	case 3:
		for i := 0; i < len(pool); i++ {
			for j := i + 1; j < len(pool); j++ {
				for k := j + 1; k < len(pool); k++ {
					cards[0], cards[1], cards[2] = pool[i], pool[j], pool[k]
					if stEvaluate(cards).beats(target, false) {
						return true
					}
				}
			}
		}
	}
	return false
}
