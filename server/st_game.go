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
	stFormationRun      = 1 // 런 (연속)
	stFormationColor    = 2 // 컬러 (같은 색)
	stFormationTriple   = 3 // 트리플/포카드 (같은 숫자)
	stFormationColorRun = 4 // 컬러런 (같은 색 연속)
)

// stFormation 완성된 조합의 평가 결과
type stFormation struct {
	Category int
	Sum      int
}

// beats a 가 b 를 이기는지. aFirst 는 족보·합이 모두 같을 때 a 쪽이
// 먼저 완성했는지 (공식 룰: 먼저 완성한 쪽 승리).
func (a stFormation) beats(b stFormation, aFirst bool) bool {
	if a.Category != b.Category {
		return a.Category > b.Category
	}
	if a.Sum != b.Sum {
		return a.Sum > b.Sum
	}
	return aFirst
}

// stEvalRanks 숫자 배열(정렬 불필요)과 색 일치 여부로 족보 평가.
// blind(눈가리개)는 합계로만 평가한다. 장수는 3장이든 4장이든 같은 규칙.
func stEvalRanks(ranks []int, sameColor, blind bool) stFormation {
	sorted := append([]int{}, ranks...)
	sort.Ints(sorted)
	sum := 0
	for _, r := range sorted {
		sum += r
	}
	if blind {
		return stFormation{stFormationSum, sum}
	}

	isRun := true
	allEqual := true
	for i := 1; i < len(sorted); i++ {
		if sorted[i] != sorted[i-1]+1 {
			isRun = false
		}
		if sorted[i] != sorted[0] {
			allEqual = false
		}
	}

	category := stFormationSum
	switch {
	case sameColor && isRun:
		category = stFormationColorRun
	case allEqual:
		category = stFormationTriple
	case sameColor:
		category = stFormationColor
	case isRun:
		category = stFormationRun
	}
	return stFormation{Category: category, Sum: sum}
}

// stWildRanks 정예병 와일드카드가 가질 수 있는 숫자 후보
func stWildRanks(t STTactic) []int {
	switch t {
	case STTacticJoker:
		return []int{1, 2, 3, 4, 5, 6, 7, 8, 9}
	case STTacticSpy:
		return []int{7}
	case STTacticShield:
		return []int{1, 2, 3}
	}
	return nil
}

// stBestFormation 정예병(조커·스파이·방패병)의 색·숫자를 가장 유리하게
// 배정했을 때의 최강 족보. 공식 룰에서 값 선언은 획득 시점에 하므로
// 항상 최적 배정과 동일하다.
func stBestFormation(cards []STCard, blind bool) stFormation {
	fixedRanks := []int{}
	sameColor := true
	firstColor := -1
	wilds := [][]int{}

	for _, c := range cards {
		if c.IsClan() {
			fixedRanks = append(fixedRanks, c.Rank)
			if firstColor == -1 {
				firstColor = c.Color
			} else if c.Color != firstColor {
				sameColor = false
			}
		} else {
			wilds = append(wilds, stWildRanks(c.Tactic))
		}
	}

	best := stFormation{Category: -1, Sum: -1}
	ranks := make([]int, len(fixedRanks), len(cards))
	copy(ranks, fixedRanks)

	var enumerate func(i int)
	enumerate = func(i int) {
		if i == len(wilds) {
			form := stEvalRanks(ranks, sameColor, blind)
			if form.Category > best.Category ||
				(form.Category == best.Category && form.Sum > best.Sum) {
				best = form
			}
			return
		}
		for _, r := range wilds[i] {
			ranks = append(ranks, r)
			enumerate(i + 1)
			ranks = ranks[:len(ranks)-1]
		}
	}
	enumerate(0)
	return best
}

// stOther 반대 진영
func stOther(side STSide) STSide {
	if side == STSouth {
		return STNorth
	}
	return STSouth
}

// NewSTGame 로비 상태의 새 게임
func NewSTGame(id string, tacticMode bool) *STGame {
	stones := make([]*STStone, STStoneCount)
	for i := range stones {
		stones[i] = &STStone{
			Cards:          map[STSide][]STCard{},
			CompletedOrder: map[STSide]int{},
		}
	}
	return &STGame{
		ID:            id,
		Names:         map[STSide]string{},
		TacticMode:    tacticMode,
		Hands:         map[STSide][]STCard{},
		Stones:        stones,
		PlayedTactics: map[STSide]int{STSouth: 0, STNorth: 0},
		Phase:         STPhaseLobby,
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

// HandTarget 손패 목표 장수
func (g *STGame) HandTarget() int {
	if g.TacticMode {
		return STTacticHandSize
	}
	return STHandSize
}

// newSTTacticDeck 전술 카드 10장
func newSTTacticDeck() []STCard {
	return []STCard{
		{Tactic: STTacticJoker}, {Tactic: STTacticJoker},
		{Tactic: STTacticSpy}, {Tactic: STTacticShield},
		{Tactic: STTacticBlind}, {Tactic: STTacticMud},
		{Tactic: STTacticRecruiter}, {Tactic: STTacticStrategist},
		{Tactic: STTacticBanshee}, {Tactic: STTacticTraitor},
	}
}

// Start 셔플·배분 후 게임 시작. 시작 손패는 클랜 카드만 받는다.
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

	target := g.HandTarget()
	for _, side := range []STSide{STSouth, STNorth} {
		g.Hands[side] = append([]STCard{}, deck[:target]...)
		deck = deck[target:]
	}
	g.Deck = deck

	if g.TacticMode {
		tactics := newSTTacticDeck()
		rng.Shuffle(len(tactics), func(i, j int) { tactics[i], tactics[j] = tactics[j], tactics[i] })
		g.TacticDeck = tactics
	}

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

// syncCompletion 돌 한쪽의 완성 상태를 갱신한다. 필요 장수를 채우는 순간
// 전역 카운터를 부여하고, (카드 이동·진흙탕 등으로) 미달이 되면 되돌린다.
func (g *STGame) syncCompletion(stone *STStone, side STSide) {
	if len(stone.Cards[side]) >= stone.Required() {
		if stone.CompletedOrder[side] == 0 {
			g.completionCounter++
			stone.CompletedOrder[side] = g.completionCounter
		}
	} else {
		stone.CompletedOrder[side] = 0
	}
}

// removeFromHand 손패에서 카드 한 장 제거
func (g *STGame) removeFromHand(side STSide, index int) STCard {
	hand := g.Hands[side]
	card := hand[index]
	g.Hands[side] = append(hand[:index:index], hand[index+1:]...)
	return card
}

// hasJokerOnBorder side 진영이 국경에 이미 조커를 냈는지 (진영당 1장 제한)
func (g *STGame) hasJokerOnBorder(side STSide) bool {
	for _, stone := range g.Stones {
		for _, c := range stone.Cards[side] {
			if c.Tactic == STTacticJoker {
				return true
			}
		}
	}
	return false
}

// canUseTactic 전술 카드 사용 제약: 상대보다 1장 초과 사용 금지
func (g *STGame) canUseTactic(side STSide) bool {
	return g.PlayedTactics[side] <= g.PlayedTactics[stOther(side)]
}

// PlayCard 손패의 카드(클랜·정예병·전투 모드)를 돌에 낸다.
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
	card := hand[handIndex]
	stone := g.Stones[stoneIndex]
	if stone.Owner != "" {
		return errors.New("이미 획득된 돌입니다")
	}
	if card.IsRuse() {
		return errors.New("계략 카드는 돌에 낼 수 없습니다")
	}

	if !card.IsClan() && !g.canUseTactic(side) {
		return errors.New("전술 카드는 상대보다 1장 초과해서 쓸 수 없습니다")
	}

	if card.IsCombat() {
		if card.Tactic == STTacticBlind && stone.Blind {
			return errors.New("이미 눈가리개가 걸린 돌입니다")
		}
		if card.Tactic == STTacticMud && stone.Mud {
			return errors.New("이미 진흙탕이 걸린 돌입니다")
		}
		g.removeFromHand(side, handIndex)
		if card.Tactic == STTacticBlind {
			stone.Blind = true
		} else {
			stone.Mud = true
			// 4장 기준으로 완성 상태 재계산
			g.syncCompletion(stone, STSouth)
			g.syncCompletion(stone, STNorth)
		}
		g.PlayedTactics[side]++
	} else {
		// 클랜 또는 정예병
		if len(stone.Cards[side]) >= stone.Required() {
			return errors.New("이 돌에는 더 낼 수 없습니다")
		}
		if card.Tactic == STTacticJoker && g.hasJokerOnBorder(side) {
			return errors.New("조커는 진영당 1장만 낼 수 있습니다")
		}
		g.removeFromHand(side, handIndex)
		stone.Cards[side] = append(stone.Cards[side], card)
		g.syncCompletion(stone, side)
		if !card.IsClan() {
			g.PlayedTactics[side]++
		}
	}

	g.PassStreak = 0
	g.afterAction()
	return nil
}

// PlayRuse 계략 카드를 사용한다.
func (g *STGame) PlayRuse(side STSide, p STPlayRusePayload) error {
	if g.Phase != STPhasePlay {
		return errors.New("지금은 카드를 낼 수 없습니다")
	}
	if side != g.CurrentSide {
		return errors.New("당신의 차례가 아닙니다")
	}
	hand := g.Hands[side]
	if p.HandIndex < 0 || p.HandIndex >= len(hand) {
		return errors.New("없는 카드입니다")
	}
	card := hand[p.HandIndex]
	if !card.IsRuse() {
		return errors.New("계략 카드가 아닙니다")
	}
	if !g.canUseTactic(side) {
		return errors.New("전술 카드는 상대보다 1장 초과해서 쓸 수 없습니다")
	}
	opp := stOther(side)

	switch card.Tactic {
	case STTacticRecruiter:
		if len(g.Deck) == 0 && len(g.TacticDeck) == 0 {
			return errors.New("뽑을 덱이 없습니다")
		}
		g.removeFromHand(side, p.HandIndex)
		g.Discard = append(g.Discard, card)
		g.PlayedTactics[side]++
		g.PassStreak = 0
		g.RecruiterDraws = 3
		g.RecruiterReturns = 2
		g.Phase = STPhaseRecruiterDraw
		return nil

	case STTacticBanshee:
		if _, err := g.stoneSideCard(p.FromStone, opp, p.FromIndex); err != nil {
			return err
		}
		g.removeFromHand(side, p.HandIndex)
		g.Discard = append(g.Discard, g.removeFromStone(p.FromStone, opp, p.FromIndex))
		g.Discard = append(g.Discard, card)

	case STTacticStrategist:
		if _, err := g.stoneSideCard(p.FromStone, side, p.FromIndex); err != nil {
			return err
		}
		if p.ToStone == -1 {
			// 버리기
			g.removeFromHand(side, p.HandIndex)
			g.Discard = append(g.Discard, g.removeFromStone(p.FromStone, side, p.FromIndex))
			g.Discard = append(g.Discard, card)
		} else {
			if err := g.checkDest(p.ToStone, side, p.FromStone); err != nil {
				return err
			}
			g.removeFromHand(side, p.HandIndex)
			moved := g.removeFromStone(p.FromStone, side, p.FromIndex)
			dest := g.Stones[p.ToStone]
			dest.Cards[side] = append(dest.Cards[side], moved)
			g.syncCompletion(dest, side)
			g.Discard = append(g.Discard, card)
		}

	case STTacticTraitor:
		target, err := g.stoneSideCard(p.FromStone, opp, p.FromIndex)
		if err != nil {
			return err
		}
		if !target.IsClan() {
			return errors.New("배신자는 클랜 카드만 데려올 수 있습니다")
		}
		if err := g.checkDest(p.ToStone, side, -1); err != nil {
			return err
		}
		g.removeFromHand(side, p.HandIndex)
		moved := g.removeFromStone(p.FromStone, opp, p.FromIndex)
		dest := g.Stones[p.ToStone]
		dest.Cards[side] = append(dest.Cards[side], moved)
		g.syncCompletion(dest, side)
		g.Discard = append(g.Discard, card)
	}

	g.PlayedTactics[side]++
	g.PassStreak = 0
	g.afterAction()
	return nil
}

// stoneSideCard 검증: 미획득 돌의 해당 진영 카드
func (g *STGame) stoneSideCard(stoneIndex int, side STSide, cardIndex int) (STCard, error) {
	if stoneIndex < 0 || stoneIndex >= STStoneCount {
		return STCard{}, errors.New("없는 돌입니다")
	}
	stone := g.Stones[stoneIndex]
	if stone.Owner != "" {
		return STCard{}, errors.New("획득된 돌의 카드는 건드릴 수 없습니다")
	}
	cards := stone.Cards[side]
	if cardIndex < 0 || cardIndex >= len(cards) {
		return STCard{}, errors.New("없는 카드입니다")
	}
	return cards[cardIndex], nil
}

// checkDest 이동 목적지 검증 (미획득·자리 여유·출발지와 다른 돌)
func (g *STGame) checkDest(toStone int, side STSide, fromStone int) error {
	if toStone < 0 || toStone >= STStoneCount {
		return errors.New("없는 돌입니다")
	}
	if toStone == fromStone {
		return errors.New("같은 돌로는 옮길 수 없습니다")
	}
	dest := g.Stones[toStone]
	if dest.Owner != "" {
		return errors.New("획득된 돌에는 옮길 수 없습니다")
	}
	if len(dest.Cards[side]) >= dest.Required() {
		return errors.New("옮길 자리가 없습니다")
	}
	return nil
}

// removeFromStone 돌에서 카드를 빼고 완성 상태를 갱신한다
func (g *STGame) removeFromStone(stoneIndex int, side STSide, cardIndex int) STCard {
	stone := g.Stones[stoneIndex]
	cards := stone.Cards[side]
	card := cards[cardIndex]
	stone.Cards[side] = append(cards[:cardIndex:cardIndex], cards[cardIndex+1:]...)
	g.syncCompletion(stone, side)
	return card
}

// RecruiterDraw 모병관: 지정한 덱에서 1장 뽑는다 (총 3회)
func (g *STGame) RecruiterDraw(side STSide, deck string) error {
	if g.Phase != STPhaseRecruiterDraw || side != g.CurrentSide {
		return errors.New("지금은 뽑을 수 없습니다")
	}
	switch deck {
	case "clan":
		if len(g.Deck) == 0 {
			return errors.New("클랜 덱이 비었습니다")
		}
		g.Hands[side] = append(g.Hands[side], g.Deck[len(g.Deck)-1])
		g.Deck = g.Deck[:len(g.Deck)-1]
	case "tactic":
		if len(g.TacticDeck) == 0 {
			return errors.New("전술 덱이 비었습니다")
		}
		g.Hands[side] = append(g.Hands[side], g.TacticDeck[len(g.TacticDeck)-1])
		g.TacticDeck = g.TacticDeck[:len(g.TacticDeck)-1]
	default:
		return errors.New("덱을 선택하세요")
	}
	g.RecruiterDraws--
	if g.RecruiterDraws == 0 || (len(g.Deck) == 0 && len(g.TacticDeck) == 0) {
		g.RecruiterDraws = 0
		g.Phase = STPhaseRecruiterReturn
	}
	return nil
}

// RecruiterReturn 모병관: 손패에서 1장을 해당 덱 밑으로 반납한다 (총 2회)
func (g *STGame) RecruiterReturn(side STSide, handIndex int) error {
	if g.Phase != STPhaseRecruiterReturn || side != g.CurrentSide {
		return errors.New("지금은 반납할 수 없습니다")
	}
	hand := g.Hands[side]
	if handIndex < 0 || handIndex >= len(hand) {
		return errors.New("없는 카드입니다")
	}
	card := g.removeFromHand(side, handIndex)
	if card.IsClan() {
		g.Deck = append([]STCard{card}, g.Deck...)
	} else {
		g.TacticDeck = append([]STCard{card}, g.TacticDeck...)
	}
	g.RecruiterReturns--
	if g.RecruiterReturns == 0 {
		g.afterAction()
	}
	return nil
}

// Pass 클랜 카드를 낼 수 없을 때 턴을 넘긴다 (전술 변형 공식 룰)
func (g *STGame) Pass(side STSide) error {
	if g.Phase != STPhasePlay || side != g.CurrentSide {
		return errors.New("지금은 패스할 수 없습니다")
	}
	if g.clanPlayable(side) {
		return errors.New("낼 수 있는 클랜 카드가 있으면 패스할 수 없습니다")
	}
	g.PassStreak++
	if g.PassStreak >= 2 {
		g.endByStalemate()
		return nil
	}
	g.nextTurn()
	return nil
}

// DrawFrom 드로우 단계에서 덱을 골라 1장 뽑고 턴을 넘긴다
func (g *STGame) DrawFrom(side STSide, deck string) error {
	if g.Phase != STPhaseDraw || side != g.CurrentSide {
		return errors.New("지금은 뽑을 수 없습니다")
	}
	switch deck {
	case "clan":
		if len(g.Deck) == 0 {
			return errors.New("클랜 덱이 비었습니다")
		}
		g.Hands[side] = append(g.Hands[side], g.Deck[len(g.Deck)-1])
		g.Deck = g.Deck[:len(g.Deck)-1]
	case "tactic":
		if len(g.TacticDeck) == 0 {
			return errors.New("전술 덱이 비었습니다")
		}
		g.Hands[side] = append(g.Hands[side], g.TacticDeck[len(g.TacticDeck)-1])
		g.TacticDeck = g.TacticDeck[:len(g.TacticDeck)-1]
	default:
		return errors.New("덱을 선택하세요")
	}
	g.nextTurn()
	return nil
}

// afterAction 카드 사용이 끝난 뒤: 획득 가능하면 claim 단계, 아니면 드로우로
func (g *STGame) afterAction() {
	if len(g.ClaimableStones(g.CurrentSide)) > 0 {
		g.Phase = STPhaseClaim
	} else {
		g.drawStep()
	}
}

// drawStep 손패 보충. 두 덱 모두 뽑을 수 있으면 선택 단계로, 아니면 자동.
func (g *STGame) drawStep() {
	side := g.CurrentSide
	if len(g.Hands[side]) >= g.HandTarget() {
		g.nextTurn()
		return
	}
	clanOK := len(g.Deck) > 0
	tacticOK := g.TacticMode && len(g.TacticDeck) > 0
	switch {
	case clanOK && tacticOK:
		g.Phase = STPhaseDraw
	case clanOK:
		g.Hands[side] = append(g.Hands[side], g.Deck[len(g.Deck)-1])
		g.Deck = g.Deck[:len(g.Deck)-1]
		g.nextTurn()
	case tacticOK:
		g.Hands[side] = append(g.Hands[side], g.TacticDeck[len(g.TacticDeck)-1])
		g.TacticDeck = g.TacticDeck[:len(g.TacticDeck)-1]
		g.nextTurn()
	default:
		g.nextTurn()
	}
}

// ClaimStone 돌 획득. 획득 후 남은 획득 가능 돌이 없으면 드로우로 넘어간다.
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
	g.PassStreak = 0

	if reason, over := g.checkVictory(side); over {
		g.Winner = side
		g.EndReason = reason
		g.Phase = STPhaseGameOver
		return nil
	}

	if len(g.ClaimableStones(side)) == 0 {
		g.drawStep()
	}
	return nil
}

// EndTurn claim 단계에서 (남은 돌을 가져오지 않고) 드로우로 넘어간다
func (g *STGame) EndTurn(side STSide) error {
	if g.Phase != STPhaseClaim {
		return errors.New("지금은 턴을 넘길 수 없습니다")
	}
	if side != g.CurrentSide {
		return errors.New("당신의 차례가 아닙니다")
	}
	g.drawStep()
	return nil
}

// clanPlayable 클랜 카드를 낼 수 있는지 (손패의 클랜 카드 + 빈 자리)
func (g *STGame) clanPlayable(side STSide) bool {
	hasClan := false
	for _, c := range g.Hands[side] {
		if c.IsClan() {
			hasClan = true
			break
		}
	}
	if !hasClan {
		return false
	}
	for _, stone := range g.Stones {
		if stone.Owner == "" && len(stone.Cards[side]) < stone.Required() {
			return true
		}
	}
	return false
}

// anyPlayable 어떤 카드든 낼 수 있는지 (자동 패스 판정용)
func (g *STGame) anyPlayable(side STSide) bool {
	if g.clanPlayable(side) {
		return true
	}
	if !g.TacticMode {
		return false
	}
	canTactic := g.canUseTactic(side)
	opp := stOther(side)
	for _, c := range g.Hands[side] {
		if c.IsClan() || !canTactic {
			continue
		}
		switch {
		case c.IsElite():
			if c.Tactic == STTacticJoker && g.hasJokerOnBorder(side) {
				continue
			}
			for _, stone := range g.Stones {
				if stone.Owner == "" && len(stone.Cards[side]) < stone.Required() {
					return true
				}
			}
		case c.IsCombat():
			for _, stone := range g.Stones {
				if stone.Owner != "" {
					continue
				}
				if c.Tactic == STTacticBlind && !stone.Blind {
					return true
				}
				if c.Tactic == STTacticMud && !stone.Mud {
					return true
				}
			}
		case c.Tactic == STTacticRecruiter:
			if len(g.Deck) > 0 || len(g.TacticDeck) > 0 {
				return true
			}
		case c.Tactic == STTacticStrategist:
			for _, stone := range g.Stones {
				if stone.Owner == "" && len(stone.Cards[side]) > 0 {
					return true
				}
			}
		case c.Tactic == STTacticBanshee:
			for _, stone := range g.Stones {
				if stone.Owner == "" && len(stone.Cards[opp]) > 0 {
					return true
				}
			}
		case c.Tactic == STTacticTraitor:
			hasTarget := false
			for _, stone := range g.Stones {
				if stone.Owner != "" {
					continue
				}
				for _, sc := range stone.Cards[opp] {
					if sc.IsClan() {
						hasTarget = true
					}
				}
			}
			if !hasTarget {
				continue
			}
			for _, stone := range g.Stones {
				if stone.Owner == "" && len(stone.Cards[side]) < stone.Required() {
					return true
				}
			}
		}
	}
	return false
}

// endByStalemate 교착: 돌을 많이 가진 쪽 승리, 같으면 무승부
func (g *STGame) endByStalemate() {
	south, north := g.stoneCount(STSouth), g.stoneCount(STNorth)
	if south > north {
		g.Winner = STSouth
	} else if north > south {
		g.Winner = STNorth
	}
	g.EndReason = "stalemate"
	g.Phase = STPhaseGameOver
}

// nextTurn 턴을 넘긴다. 새 턴 플레이어가 아무것도 할 수 없으면 획득만
// 허용하고, 그마저 없으면 자동으로 패스한다. 양쪽 모두 아무것도 할 수
// 없으면 돌 개수로 승부를 가른다.
func (g *STGame) nextTurn() {
	for attempts := 0; attempts < 2; attempts++ {
		g.CurrentSide = stOther(g.CurrentSide)
		if g.anyPlayable(g.CurrentSide) {
			g.Phase = STPhasePlay
			return
		}
		if len(g.ClaimableStones(g.CurrentSide)) > 0 {
			g.Phase = STPhaseClaim
			return
		}
	}
	g.endByStalemate()
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
// 자기 쪽이 필요 장수를 완성해야 하고, 상대가 완성했으면 족보를 직접
// 비교하며, 못 채웠으면 "공개된 카드만으로는 상대가 어떤 카드를 놓아도
// 이길 수 없음"이 증명돼야 한다 (선점 증명, 공식 룰).
func (g *STGame) isClaimable(stoneIndex int, side STSide) bool {
	stone := g.Stones[stoneIndex]
	if stone.Owner != "" {
		return false
	}
	required := stone.Required()
	mine := stone.Cards[side]
	if len(mine) != required {
		return false
	}
	myFormation := stBestFormation(mine, stone.Blind)

	opp := stOther(side)
	oppCards := stone.Cards[opp]

	if len(oppCards) == required {
		myFirst := stone.CompletedOrder[side] < stone.CompletedOrder[opp]
		return myFormation.beats(stBestFormation(oppCards, stone.Blind), myFirst)
	}

	// 선점 증명: 공개되지 않은 모든 카드(양쪽 손패·양 덱 포함 — 내 손패는
	// 증명 근거로 쓸 수 없으므로 상대가 쓸 수 있는 것으로 취급)로 상대의
	// 남은 슬롯을 채우는 모든 경우를 검사한다. 전술 변형에서는 아직
	// 공개되지 않은 정예병(조커·스파이·방패병)도 상대가 쓸 수 있다.
	pool := g.proofPool(opp)
	// 내가 먼저 완성했으므로 상대는 동점으로는 이길 수 없다.
	return !stCanBeat(oppCards, pool, required, stone.Blind, myFormation)
}

// proofPool 선점 증명에서 상대가 동원할 수 있다고 봐야 하는 카드 전체
func (g *STGame) proofPool(oppSide STSide) []STCard {
	visible := map[STCard]bool{}
	jokersVisible := 0
	spyVisible := false
	shieldVisible := false

	markVisible := func(c STCard) {
		switch c.Tactic {
		case STTacticNone:
			visible[c] = true
		case STTacticJoker:
			jokersVisible++
		case STTacticSpy:
			spyVisible = true
		case STTacticShield:
			shieldVisible = true
		}
	}
	for _, stone := range g.Stones {
		for _, cards := range stone.Cards {
			for _, c := range cards {
				markVisible(c)
			}
		}
	}
	for _, c := range g.Discard {
		markVisible(c)
	}

	pool := []STCard{}
	for c := 0; c < STColorCount; c++ {
		for r := 1; r <= STMaxRank; r++ {
			card := STCard{Color: c, Rank: r}
			if !visible[card] {
				pool = append(pool, card)
			}
		}
	}
	if g.TacticMode {
		// 상대는 조커를 진영당 1장만 쓸 수 있다
		if jokersVisible < 2 && !g.hasJokerOnBorder(oppSide) {
			pool = append(pool, STCard{Tactic: STTacticJoker})
		}
		if !spyVisible {
			pool = append(pool, STCard{Tactic: STTacticSpy})
		}
		if !shieldVisible {
			pool = append(pool, STCard{Tactic: STTacticShield})
		}
	}
	return pool
}

// stCanBeat existing 을 pool 의 카드로 required 장까지 채워 target 을
// 엄격히 이기는 조합을 만들 수 있는지 (동점은 target 승리로 간주)
func stCanBeat(existing []STCard, pool []STCard, required int, blind bool, target stFormation) bool {
	need := required - len(existing)
	cards := make([]STCard, len(existing), required)
	copy(cards, existing)

	var choose func(start int) bool
	choose = func(start int) bool {
		if len(cards) == required {
			return stBestFormation(cards, blind).beats(target, false)
		}
		for i := start; i < len(pool); i++ {
			cards = append(cards, pool[i])
			if choose(i + 1) {
				cards = cards[:len(cards)-1]
				return true
			}
			cards = cards[:len(cards)-1]
		}
		return false
	}
	if need <= 0 {
		return stBestFormation(cards, blind).beats(target, false)
	}
	return choose(0)
}
