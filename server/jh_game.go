package server

import (
	"errors"
	"time"
)

// jhOther 반대 역할
func jhOther(role JHRole) JHRole {
	if role == JHJekyll {
		return JHHyde
	}
	return JHJekyll
}

// jhNewDeck 전체 25장. 악 3수트 × 1~7 + 물약 2+~5+.
func jhNewDeck() []JHCard {
	deck := make([]JHCard, 0, 25)
	id := 0
	for _, suit := range jhEvilSuits {
		for v := 1; v <= JHEvilMaxValue; v++ {
			deck = append(deck, JHCard{ID: id, Suit: suit, Value: v})
			id++
		}
	}
	for v := JHPotionMinValue; v <= JHPotionMaxValue; v++ {
		deck = append(deck, JHCard{ID: id, Suit: JHPotion, Value: v})
		id++
	}
	return deck
}

// jhPower 물약 포함 트릭의 숫자 비교값. 물약은 같은 숫자보다 반 끗 높다
// (2+는 2를 이기고 3에게 진다). 값이 겹치는 카드 쌍이 없어 동점은 불가능하다.
func jhPower(c JHCard) int {
	if c.IsPotion() {
		return c.Value*2 + 1
	}
	return c.Value * 2
}

// NewJHGame 로비 상태의 새 게임
func NewJHGame(id string) *JHGame {
	return &JHGame{
		ID:     id,
		Names:  map[JHRole]string{},
		Hands:  map[JHRole][]JHCard{},
		Tricks: map[JHRole][]JHTrick{},
		Phase:  JHPhaseLobby,
	}
}

// AddPlayer 입장. 먼저 온 사람이 지킬.
func (g *JHGame) AddPlayer(name string) (JHRole, error) {
	if g.Phase != JHPhaseLobby {
		return "", errors.New("이미 시작된 게임입니다")
	}
	if _, ok := g.Names[JHJekyll]; !ok {
		g.Names[JHJekyll] = name
		return JHJekyll, nil
	}
	if _, ok := g.Names[JHHyde]; !ok {
		g.Names[JHHyde] = name
		return JHHyde, nil
	}
	return "", errors.New("자리가 없습니다")
}

// IsReady 게임 시작 준비 확인
func (g *JHGame) IsReady() bool {
	return len(g.Names) == 2
}

// Start 1라운드 배분 후 게임 시작
func (g *JHGame) Start(rng jhRng) error {
	if !g.IsReady() {
		return errors.New("시작할 수 없습니다 (2명 필요)")
	}
	g.rng = rng
	g.Round = 1
	g.Marker = 0
	g.Ready = true
	g.StartedAt = time.Now()
	g.dealRound()
	return nil
}

// dealRound 25장 전부 섞어 10장씩 배분하고 교환 단계로 들어간다.
// 남는 5장은 그대로 버려진다 (비공개 제외).
func (g *JHGame) dealRound() {
	deck := jhNewDeck()
	g.rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })

	g.Hands[JHJekyll] = append([]JHCard{}, deck[:JHHandSize]...)
	g.Hands[JHHyde] = append([]JHCard{}, deck[JHHandSize:JHHandSize*2]...)
	g.Tricks[JHJekyll] = []JHTrick{}
	g.Tricks[JHHyde] = []JHTrick{}
	g.RankOrder = nil
	g.TableLead, g.TableFollow, g.DeclaredSuit = nil, nil, ""
	g.ExchangeSel = map[JHRole][]int{}
	g.GreedSel = nil

	// 라운드 선: 마커가 하이드 진영(트랙 왼쪽 절반)에 있으면 하이드, 아니면
	// 지킬. 1라운드는 마커가 지킬 홈(0)에 있으므로 항상 지킬이 선이다.
	if g.Marker > JHTrackJekyllHalf {
		g.Leader = JHHyde
	} else {
		g.Leader = JHJekyll
	}
	g.Phase = JHPhaseExchange
	g.emit(JHEventPayload{Kind: "round_start", Round: g.Round})
}

// ExchangeCount 이번 라운드 교환 장수 (라운드 1/2/3 → 1/2/3장)
func (g *JHGame) ExchangeCount() int {
	return g.Round
}

// potionCount 손패의 물약 개수
func potionCount(hand []JHCard) int {
	n := 0
	for _, c := range hand {
		if c.IsPotion() {
			n++
		}
	}
	return n
}

// MustIncludePotion 물약 강제 교환 규칙: 손에 물약이 2장 이상이면
// 교환에 물약을 최소 1장 포함해야 한다.
func (g *JHGame) MustIncludePotion(role JHRole) bool {
	return g.Phase == JHPhaseExchange && potionCount(g.Hands[role]) >= 2
}

// validIndexSet 인덱스들이 서로 다르고 손패 범위 안인지
func validIndexSet(indices []int, handLen int) bool {
	seen := map[int]bool{}
	for _, i := range indices {
		if i < 0 || i >= handLen || seen[i] {
			return false
		}
		seen[i] = true
	}
	return true
}

// SubmitExchange 라운드 시작 교환 제출. 양쪽이 모두 제출하면 동시 교환하고
// 트릭테이킹을 시작한다.
func (g *JHGame) SubmitExchange(role JHRole, indices []int) error {
	if g.Phase != JHPhaseExchange {
		return errors.New("지금은 교환할 수 없습니다")
	}
	if _, done := g.ExchangeSel[role]; done {
		return errors.New("이미 제출했습니다")
	}
	if len(indices) != g.ExchangeCount() {
		return errors.New("교환 장수가 맞지 않습니다")
	}
	hand := g.Hands[role]
	if !validIndexSet(indices, len(hand)) {
		return errors.New("잘못된 카드 선택입니다")
	}
	if g.MustIncludePotion(role) {
		hasPotion := false
		for _, i := range indices {
			if hand[i].IsPotion() {
				hasPotion = true
				break
			}
		}
		if !hasPotion {
			return errors.New("물약이 2장 이상이면 물약을 최소 1장 넘겨야 합니다")
		}
	}

	g.ExchangeSel[role] = append([]int{}, indices...)
	if len(g.ExchangeSel) == 2 {
		g.performSwap(g.ExchangeSel)
		g.ExchangeSel = map[JHRole][]int{}
		g.Phase = JHPhaseLead
	}
	return nil
}

// performSwap 양쪽 제출 인덱스의 카드를 동시에 맞바꾼다
func (g *JHGame) performSwap(sel map[JHRole][]int) {
	give := map[JHRole][]JHCard{}
	for role, indices := range sel {
		hand := g.Hands[role]
		picked := map[int]bool{}
		for _, i := range indices {
			give[role] = append(give[role], hand[i])
			picked[i] = true
		}
		remain := make([]JHCard, 0, len(hand))
		for i, c := range hand {
			if !picked[i] {
				remain = append(remain, c)
			}
		}
		g.Hands[role] = remain
	}
	g.Hands[JHJekyll] = append(g.Hands[JHJekyll], give[JHHyde]...)
	g.Hands[JHHyde] = append(g.Hands[JHHyde], give[JHJekyll]...)
}

// registerSuit 악 수트가 이번 라운드에 처음 등장하면 랭크에 등록한다.
// 먼저 등장한 수트일수록 약하다. 물약은 랭크에 관여하지 않는다.
func (g *JHGame) registerSuit(s JHSuit) {
	if s == JHPotion {
		return
	}
	for _, r := range g.RankOrder {
		if r == s {
			return
		}
	}
	g.RankOrder = append(g.RankOrder, s)
}

// rankIndex 수트의 현재 랭크 (클수록 강함). 미등장 수트는 -1.
func (g *JHGame) rankIndex(s JHSuit) int {
	for i, r := range g.RankOrder {
		if r == s {
			return i
		}
	}
	return -1
}

// currentActor 지금 카드 입력이 필요한 역할 (lead/declare/follow 단계)
func (g *JHGame) currentActor() JHRole {
	switch g.Phase {
	case JHPhaseLead, JHPhaseDeclare:
		return g.Leader
	case JHPhaseFollow:
		return jhOther(g.Leader)
	case JHPhasePrideSteal:
		return g.TrickWinner
	}
	return ""
}

// LegalPlays role 이 지금 낼 수 있는 손패 인덱스. 카드 입력 단계가 아니거나
// 자기 차례가 아니면 빈 목록.
func (g *JHGame) LegalPlays(role JHRole) []int {
	hand := g.Hands[role]
	switch g.Phase {
	case JHPhaseLead:
		if role != g.Leader {
			return nil
		}
		all := make([]int, len(hand))
		for i := range hand {
			all[i] = i
		}
		return all
	case JHPhaseFollow:
		if role != jhOther(g.Leader) {
			return nil
		}
		lead := *g.TableLead
		if lead.IsPotion() {
			// 물약 리드: 선언 색이 있으면 반드시 그 색 (물약으로 회피 불가).
			// 선언 색이 없으면 아무 카드나.
			return jhFollowIndices(hand, g.DeclaredSuit, false)
		}
		// 색 카드 리드: 같은 색 강제지만 물약으로 대체할 수 있다.
		return jhFollowIndices(hand, lead.Suit, true)
	}
	return nil
}

// jhFollowIndices 팔로우 가능한 인덱스. required 색이 손에 있으면 그 색
// (potionAllowed 면 물약 포함)만, 없으면 전부.
func jhFollowIndices(hand []JHCard, required JHSuit, potionAllowed bool) []int {
	hasRequired := false
	for _, c := range hand {
		if c.Suit == required {
			hasRequired = true
			break
		}
	}
	indices := []int{}
	for i, c := range hand {
		if !hasRequired || c.Suit == required || (potionAllowed && c.IsPotion()) {
			indices = append(indices, i)
		}
	}
	return indices
}

// PlayCard 리드 또는 팔로우로 카드를 낸다
func (g *JHGame) PlayCard(role JHRole, handIndex int) error {
	if g.Phase != JHPhaseLead && g.Phase != JHPhaseFollow {
		return errors.New("지금은 카드를 낼 수 없습니다")
	}
	if role != g.currentActor() {
		return errors.New("당신의 차례가 아닙니다")
	}
	legal := false
	for _, i := range g.LegalPlays(role) {
		if i == handIndex {
			legal = true
			break
		}
	}
	if !legal {
		return errors.New("낼 수 없는 카드입니다")
	}

	hand := g.Hands[role]
	card := hand[handIndex]
	g.Hands[role] = append(hand[:handIndex:handIndex], hand[handIndex+1:]...)
	g.emit(JHEventPayload{Kind: "card_played", Role: role, Card: &card})

	if g.Phase == JHPhaseLead {
		g.TableLead = &card
		g.registerSuit(card.Suit)
		if card.IsPotion() {
			g.Phase = JHPhaseDeclare
		} else {
			g.Phase = JHPhaseFollow
		}
		return nil
	}

	g.TableFollow = &card
	g.registerSuit(card.Suit)
	g.resolveTrick()
	return nil
}

// DeclareSuit 물약 리드 후 색 선언
func (g *JHGame) DeclareSuit(role JHRole, suit JHSuit) error {
	if g.Phase != JHPhaseDeclare {
		return errors.New("지금은 색을 선언할 수 없습니다")
	}
	if role != g.Leader {
		return errors.New("당신의 차례가 아닙니다")
	}
	if suit != JHPride && suit != JHWrath && suit != JHGreed {
		return errors.New("악 카드 색만 선언할 수 있습니다")
	}
	g.DeclaredSuit = suit
	g.Phase = JHPhaseFollow
	g.emit(JHEventPayload{Kind: "suit_declared", Role: role, Suit: suit})
	return nil
}

// resolveTrick 팔로우까지 나온 트릭을 판정한다.
// 물약 효과는 승자 판정에 영향을 주지 않으므로(물약 트릭은 어차피 숫자
// 비교) 승자를 먼저 계산한 뒤 효과를 적용한다 — 룰북의 "효과 먼저" 해결
// 순서와 결과가 같다.
func (g *JHGame) resolveTrick() {
	lead, follow := *g.TableLead, *g.TableFollow

	var leaderWins bool
	switch {
	case lead.IsPotion() || follow.IsPotion():
		leaderWins = jhPower(lead) > jhPower(follow)
	case lead.Suit == follow.Suit:
		leaderWins = lead.Value > follow.Value
	default:
		leaderWins = g.rankIndex(lead.Suit) > g.rankIndex(follow.Suit)
	}

	winner := g.Leader
	if !leaderWins {
		winner = jhOther(g.Leader)
	}
	g.TrickWinner = winner
	g.Tricks[winner] = append(g.Tricks[winner], JHTrick{Lead: lead, Follow: follow})

	// 물약 효과: 트릭에 물약이 정확히 1장이면 함께 나온 악 카드의 수트가 효과.
	var effect JHSuit
	if lead.IsPotion() != follow.IsPotion() {
		if lead.IsPotion() {
			effect = follow.Suit
		} else {
			effect = lead.Suit
		}
	}

	g.emit(JHEventPayload{
		Kind: "trick_resolved", LeadCard: &lead, FollowCard: &follow,
		Winner: winner, Effect: effect,
		JekyllTricks: len(g.Tricks[JHJekyll]), HydeTricks: len(g.Tricks[JHHyde]),
	})

	switch effect {
	case JHWrath:
		// 랭크 리셋. 이번 트릭에 나온 색도 새 랭크를 만들지 않는다 —
		// 다음에 나오는 색부터 다시 최하위.
		g.RankOrder = nil
		g.emit(JHEventPayload{Kind: "rank_reset"})
	case JHPride:
		// 승자가 패자의 트릭 1개를 강탈. 패자가 딴 트릭이 없으면 불발.
		if len(g.Tricks[jhOther(winner)]) > 0 {
			g.Phase = JHPhasePrideSteal
			return
		}
	case JHGreed:
		// 양쪽이 손에서 2장(1장만 남았으면 1장)을 골라 동시 교환.
		// 손이 비었으면 불발. (양쪽 손 크기는 항상 같다)
		if len(g.Hands[JHJekyll]) > 0 {
			g.GreedSel = map[JHRole][]int{}
			g.Phase = JHPhaseGreedExchange
			return
		}
	}

	g.afterTrick()
}

// GreedPickCount 탐욕 교환에서 각자 골라야 하는 장수
func (g *JHGame) GreedPickCount() int {
	n := len(g.Hands[JHJekyll])
	if n > 2 {
		n = 2
	}
	return n
}

// StealTrick 오만 효과: 승자가 패자의 트릭 더미에서 하나를 빼앗는다
func (g *JHGame) StealTrick(role JHRole, trickIndex int) error {
	if g.Phase != JHPhasePrideSteal {
		return errors.New("지금은 트릭을 빼앗을 수 없습니다")
	}
	if role != g.TrickWinner {
		return errors.New("당신의 차례가 아닙니다")
	}
	loser := jhOther(role)
	if trickIndex < 0 || trickIndex >= len(g.Tricks[loser]) {
		return errors.New("없는 트릭입니다")
	}

	stolen := g.Tricks[loser][trickIndex]
	g.Tricks[loser] = append(g.Tricks[loser][:trickIndex:trickIndex], g.Tricks[loser][trickIndex+1:]...)
	g.Tricks[role] = append(g.Tricks[role], stolen)
	g.emit(JHEventPayload{
		Kind: "trick_stolen", Role: role, TrickIndex: trickIndex,
		JekyllTricks: len(g.Tricks[JHJekyll]), HydeTricks: len(g.Tricks[JHHyde]),
	})

	g.afterTrick()
	return nil
}

// SubmitGreed 탐욕 효과 교환 제출. 양쪽이 모두 제출하면 동시 교환한다.
func (g *JHGame) SubmitGreed(role JHRole, indices []int) error {
	if g.Phase != JHPhaseGreedExchange {
		return errors.New("지금은 교환할 수 없습니다")
	}
	if _, done := g.GreedSel[role]; done {
		return errors.New("이미 제출했습니다")
	}
	if len(indices) != g.GreedPickCount() {
		return errors.New("교환 장수가 맞지 않습니다")
	}
	if !validIndexSet(indices, len(g.Hands[role])) {
		return errors.New("잘못된 카드 선택입니다")
	}

	g.GreedSel[role] = append([]int{}, indices...)
	if len(g.GreedSel) == 2 {
		g.performSwap(g.GreedSel)
		g.GreedSel = nil
		g.emit(JHEventPayload{Kind: "greed_exchanged"})
		g.afterTrick()
	}
	return nil
}

// afterTrick 트릭(및 물약 효과) 종료 후 진행. 손패가 다 떨어졌으면 라운드
// 정산, 아니면 승자가 다음 트릭의 선이 된다.
func (g *JHGame) afterTrick() {
	if len(g.Hands[JHJekyll]) == 0 && len(g.Hands[JHHyde]) == 0 {
		g.settleRound()
		return
	}
	g.Leader = g.TrickWinner
	g.TableLead, g.TableFollow, g.DeclaredSuit = nil, nil, ""
	g.Phase = JHPhaseLead
}

// settleRound 악의 진행: 트릭 수 차이만큼 마커를 하이드 쪽으로 옮긴다.
// 누가 이겼든 방향은 항상 하이드 쪽이다.
func (g *JHGame) settleRound() {
	j, h := len(g.Tricks[JHJekyll]), len(g.Tricks[JHHyde])
	moved := j - h
	if moved < 0 {
		moved = -moved
	}
	g.Marker += moved
	if g.Marker > JHTrackLength {
		g.Marker = JHTrackLength
	}

	result := JHRoundResult{
		Round: g.Round, JekyllTricks: j, HydeTricks: h,
		Moved: moved, Marker: g.Marker,
	}
	g.RoundResults = append(g.RoundResults, result)
	g.emit(JHEventPayload{
		Kind: "round_result", Round: g.Round,
		JekyllTricks: j, HydeTricks: h, Moved: moved, Marker: g.Marker,
	})

	// 하이드 홈 도달 즉시 하이드 승리
	if g.Marker >= JHTrackLength {
		g.Winner = JHHyde
		g.EndReason = "corrupted"
		g.Phase = JHPhaseGameOver
		return
	}
	// 3라운드를 버티면 지킬 승리
	if g.Round >= JHRounds {
		g.Winner = JHJekyll
		g.EndReason = "survived"
		g.Phase = JHPhaseGameOver
		return
	}
	g.Round++
	g.dealRound()
}

// emit 연출용 이벤트를 쌓는다. 허브가 액션 처리 후 DrainEvents 로 가져간다.
func (g *JHGame) emit(ev JHEventPayload) {
	g.pendingEvents = append(g.pendingEvents, ev)
}

// DrainEvents 쌓인 이벤트를 비우면서 반환
func (g *JHGame) DrainEvents() []JHEventPayload {
	events := g.pendingEvents
	g.pendingEvents = nil
	return events
}
