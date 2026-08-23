package server

import (
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"
)

// ==================== 스컬킹 순수 규칙 ====================
//
// 덱 구성·배분·비딩·따라내기 판정·트릭 승자·보너스·정산만 다룬다.
// 클라이언트·타이머를 모르며, 허브(kg_hub.go)가 비딩 마감(45초)·플레이 마감
// (45초)·라운드 정산 대기(5초)를 걸고 이벤트 큐(DrainEvents)를 방송한다.
//
// 서열(가위바위보): 스컬킹 > 해적 > 인어 > 숫자 이고, 인어 > 스컬킹 이다.
// 세 특수가 한 트릭에 모두 나오면 순환이 생기는데 원작대로 인어가 이긴다.

// ==================== 덱 ====================

// kgBuildDeck 65장 덱 — 숫자 4색 × 1~13(52장) + 탈출5·해적5·인어2·스컬킹1
func kgBuildDeck() []KGCard {
	deck := make([]KGCard, 0, 52+KGEscapeCount+KGPirateCount+KGMermaidCount+KGSkullKingCard)
	for _, suit := range kgSuits {
		for rank := 1; rank <= KGSuitRankMax; rank++ {
			deck = append(deck, KGCard{Kind: KGKindNumber, Suit: suit, Rank: rank})
		}
	}
	for i := 0; i < KGEscapeCount; i++ {
		deck = append(deck, KGCard{Kind: KGKindEscape})
	}
	for i := 0; i < KGPirateCount; i++ {
		deck = append(deck, KGCard{Kind: KGKindPirate})
	}
	for i := 0; i < KGMermaidCount; i++ {
		deck = append(deck, KGCard{Kind: KGKindMermaid})
	}
	for i := 0; i < KGSkullKingCard; i++ {
		deck = append(deck, KGCard{Kind: KGKindSkullKing})
	}
	return deck
}

// ==================== 서열 / 따라내기 ====================

// kgIsSpecial 특수 카드 여부 (숫자가 아닌 모든 카드)
func kgIsSpecial(c KGCard) bool { return c.Kind != KGKindNumber }

// kgCardPower 카드 한 장의 절대 강도 — 봇의 "가장 약한/강한 카드" 고르기와
// 손패 정렬에만 쓴다. 실제 트릭 승패는 kgTrickWinner 가 정한다.
//
//	탈출 0 < 색 숫자 1~13 < 검정 숫자 21~33 < 인어 100 < 해적 110 < 스컬킹 120
func kgCardPower(c KGCard) int {
	switch c.Kind {
	case KGKindEscape:
		return 0
	case KGKindMermaid:
		return 100
	case KGKindPirate:
		return 110
	case KGKindSkullKing:
		return 120
	default:
		if c.Suit == KGSuitBlack {
			return 20 + c.Rank
		}
		return c.Rank
	}
}

// kgLegalIndexes 지금 낼 수 있는 손패 인덱스 목록.
//
// 따라내기 의무: 리드 무늬가 정해졌고 그 무늬의 숫자 카드를 쥐고 있으면
// "그 무늬 숫자 카드 또는 특수 카드"만 낼 수 있다. 특수 카드는 언제나 자유이고,
// 검정(트럼프)도 리드 무늬 숫자를 갖고 있으면 낼 수 없다 (리드가 검정일 때만 예외).
func kgLegalIndexes(hand []KGCard, leadSuit KGSuit) []int {
	legal := []int{}
	if leadSuit == KGSuitNone {
		for i := range hand {
			legal = append(legal, i)
		}
		return legal
	}
	hasLead := false
	for _, c := range hand {
		if c.Kind == KGKindNumber && c.Suit == leadSuit {
			hasLead = true
			break
		}
	}
	for i, c := range hand {
		if !hasLead || kgIsSpecial(c) || c.Suit == leadSuit {
			legal = append(legal, i)
		}
	}
	return legal
}

// kgIsLegalPlay 인덱스 하나의 합법성
func kgIsLegalPlay(hand []KGCard, leadSuit KGSuit, index int) bool {
	for _, i := range kgLegalIndexes(hand, leadSuit) {
		if i == index {
			return true
		}
	}
	return false
}

// kgTrickWinner 트릭 승자의 plays 인덱스.
//
//  1. 인어와 스컬킹이 함께 나오면 인어 (해적이 섞여도 인어가 가져간다)
//  2. 스컬킹
//  3. 해적 (먼저 낸 쪽)
//  4. 인어 (먼저 낸 쪽)
//  5. 숫자 — 검정(트럼프) 최고, 없으면 리드 무늬 최고
//  6. 전원 탈출이면 첫 번째로 낸 사람
func kgTrickWinner(plays []KGTrickPlay, leadSuit KGSuit) int {
	if len(plays) == 0 {
		return -1
	}
	first := func(kind KGCardKind) int {
		for i, p := range plays {
			if p.Card.Kind == kind {
				return i
			}
		}
		return -1
	}
	mermaid := first(KGKindMermaid)
	skullKing := first(KGKindSkullKing)
	pirate := first(KGKindPirate)

	if mermaid >= 0 && skullKing >= 0 {
		return mermaid // 인어 > 스컬킹 (순환 해소 — 원작 규칙)
	}
	if skullKing >= 0 {
		return skullKing
	}
	if pirate >= 0 {
		return pirate
	}
	if mermaid >= 0 {
		return mermaid
	}

	best, bestPower := -1, -1
	for i, p := range plays {
		if p.Card.Kind != KGKindNumber {
			continue
		}
		power := 0
		switch {
		case p.Card.Suit == KGSuitBlack:
			power = 100 + p.Card.Rank
		case p.Card.Suit == leadSuit:
			power = p.Card.Rank
		default:
			continue // 리드도 트럼프도 아닌 숫자는 이길 수 없다
		}
		if power > bestPower {
			best, bestPower = i, power
		}
	}
	if best >= 0 {
		return best
	}
	return 0 // 전원 탈출 — 첫 번째로 낸 사람이 가져간다
}

// ==================== 생성 / 대기실 ====================

// NewKGGame 대기 상태의 새 게임
func NewKGGame(id string) *KGGame {
	return &KGGame{
		ID:          id,
		Players:     []*KGPlayer{},
		Phase:       KGPhaseWaiting,
		CurrentSeat: -1,
		LeadSeat:    -1,
		Trick:       []KGTrickPlay{},
		Winners:     []int{},
		MaxRound:    KGMaxRoundCap,
	}
}

// AddPlayer 대기실 입장. 좌석 번호를 돌려준다.
func (g *KGGame) AddPlayer(name string) (int, error) {
	if g.Phase != KGPhaseWaiting {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= KGMaxPlayers {
		return -1, fmt.Errorf("자리가 없습니다 (최대 %d명)", KGMaxPlayers)
	}
	seat := len(g.Players)
	g.Players = append(g.Players, &KGPlayer{
		Seat: seat,
		Name: name,
		Hand: []KGCard{},
		Bid:  -1,
	})
	g.MaxRound = kgMaxRound(len(g.Players))
	return seat, nil
}

// RemovePlayer 대기실 퇴장. 좌석을 앞으로 당겨 빈틈을 없앤다.
func (g *KGGame) RemovePlayer(seat int) {
	if g.Phase != KGPhaseWaiting || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i, p := range g.Players {
		p.Seat = i
	}
	g.MaxRound = kgMaxRound(len(g.Players))
}

// CanStart 시작 가능 여부 (호스트 명시 시작 — 2인부터)
func (g *KGGame) CanStart() bool {
	return g.Phase == KGPhaseWaiting && len(g.Players) >= KGMinPlayers
}

// ==================== 이벤트 큐 ====================

func (g *KGGame) emit(kind string, seat int, msg string) {
	g.events = append(g.events, KGGameEvent{Kind: kind, Seat: seat, Message: msg})
}

// DrainEvents 쌓인 이벤트를 꺼내고 비운다 (허브가 방송)
func (g *KGGame) DrainEvents() []KGGameEvent {
	evs := g.events
	g.events = nil
	return evs
}

// ==================== 시작 / 라운드 ====================

// Start 게임 시작 — 인원별 최대 라운드를 확정하고 1라운드 비딩을 연다.
func (g *KGGame) Start(rng *rand.Rand) error {
	if !g.CanStart() {
		return fmt.Errorf("%d명 이상 모여야 시작할 수 있습니다", KGMinPlayers)
	}
	n := len(g.Players)
	g.Ready = true
	g.StartedAt = time.Now()
	g.MaxRound = kgMaxRound(n)
	g.StartSeat = rng.Intn(n)
	g.LastTrick = nil
	g.RoundResult = nil
	g.Winners = []int{}
	for _, p := range g.Players {
		p.Score = 0
	}
	g.Round = 1
	g.beginRound(rng)
	return nil
}

// beginRound 라운드 배분 + 비딩 개시. 라운드 r 에는 각자 r장을 받는다.
func (g *KGGame) beginRound(rng *rand.Rand) {
	n := len(g.Players)
	deck := kgBuildDeck()
	rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })

	per := g.Round
	if per*n > len(deck) { // 방어선 — maxRound 계산상 도달하지 않는다
		per = len(deck) / n
	}
	idx := 0
	for _, p := range g.Players {
		p.Hand = append([]KGCard{}, deck[idx:idx+per]...)
		kgSortHand(p.Hand)
		idx += per
		p.Bid = -1
		p.Tricks = 0
		p.Bonus = 0
	}

	g.BidsRevealed = false
	g.TrickNo = 0
	g.CurrentSeat = -1
	g.LeadSeat = -1
	g.LeadSuit = KGSuitNone
	g.Trick = []KGTrickPlay{}
	g.LastTrick = nil
	g.Phase = KGPhaseBidding
	g.StateSeq++

	g.emit("round_start", -1, fmt.Sprintf(
		"%d라운드 — 각자 %d장씩 받았습니다. 0~%d 중 몇 트릭을 먹을지 비드하세요",
		g.Round, per, per))
}

// kgSortHand 손패를 보기 좋게 정렬한다 (탈출 → 색 숫자 → 검정 → 특수)
func kgSortHand(hand []KGCard) {
	for i := 1; i < len(hand); i++ {
		for j := i; j > 0 && kgCardPower(hand[j-1]) > kgCardPower(hand[j]); j-- {
			hand[j-1], hand[j] = hand[j], hand[j-1]
		}
	}
}

// ==================== 비딩 ====================

// SubmitBid 비드 제출 (0~라운드). 제출 후 변경은 불가하고, 전원 제출되면
// 즉시 일괄 공개하며 플레이 단계로 넘어간다.
func (g *KGGame) SubmitBid(seat, bid int) error {
	if g.Phase != KGPhaseBidding {
		return errors.New("지금은 비드할 수 없습니다")
	}
	if seat < 0 || seat >= len(g.Players) {
		return errors.New("잘못된 좌석입니다")
	}
	p := g.Players[seat]
	if p.Bid >= 0 {
		return errors.New("이미 비드를 제출했습니다")
	}
	if bid < 0 || bid > g.Round {
		return fmt.Errorf("비드는 0~%d 사이여야 합니다", g.Round)
	}
	p.Bid = bid
	g.emit("bid_submitted", seat, fmt.Sprintf("%s님이 비드를 제출했습니다 (%d/%d명)",
		p.Name, g.bidCount(), len(g.Players)))
	if g.bidCount() >= len(g.Players) {
		g.revealBids()
	}
	return nil
}

// ForceBids 비딩 마감 — 미제출 좌석을 전부 0으로 채우고 공개한다 (허브 타이머)
func (g *KGGame) ForceBids() {
	if g.Phase != KGPhaseBidding {
		return
	}
	for _, p := range g.Players {
		if p.Bid < 0 {
			p.Bid = 0
			g.emit("afk", p.Seat, fmt.Sprintf("%s님이 비드하지 않아 0으로 자동 제출합니다", p.Name))
		}
	}
	g.revealBids()
}

// bidCount 제출된 비드 수
func (g *KGGame) bidCount() int {
	n := 0
	for _, p := range g.Players {
		if p.Bid >= 0 {
			n++
		}
	}
	return n
}

// revealBids 비드 일괄 공개 → 플레이 단계 개시.
// 라운드 선은 StartSeat 에서 라운드마다 한 칸씩 밀린다.
func (g *KGGame) revealBids() {
	n := len(g.Players)
	g.BidsRevealed = true
	g.Phase = KGPhasePlaying
	g.TrickNo = 1
	g.LeadSeat = (g.StartSeat + g.Round - 1) % n
	g.CurrentSeat = g.LeadSeat
	g.LeadSuit = KGSuitNone
	g.Trick = []KGTrickPlay{}
	g.StateSeq++

	parts := []string{}
	for _, p := range g.Players {
		parts = append(parts, fmt.Sprintf("%s %d", p.Name, p.Bid))
	}
	g.emit("bids_revealed", -1, fmt.Sprintf("비드 공개 — %s. %s님부터 시작합니다",
		strings.Join(parts, " · "), g.Players[g.LeadSeat].Name))
}

// ==================== 플레이 ====================

// Play 손패 인덱스 한 장을 낸다. 트릭이 차면 승자를 가리고 다음 트릭 또는
// 라운드 정산으로 넘어간다.
func (g *KGGame) Play(seat, index int) error {
	if g.Phase != KGPhasePlaying {
		return errors.New("지금은 카드를 낼 수 없습니다")
	}
	if seat != g.CurrentSeat {
		return errors.New("당신의 차례가 아닙니다")
	}
	p := g.Players[seat]
	if index < 0 || index >= len(p.Hand) {
		return errors.New("잘못된 카드입니다")
	}
	if !kgIsLegalPlay(p.Hand, g.LeadSuit, index) {
		return fmt.Errorf("리드 무늬(%s)를 따라내야 합니다", kgSuitLabel(g.LeadSuit))
	}
	g.playCard(seat, index)
	return nil
}

// ForcePlay 플레이 마감 — 가장 약한 합법 카드를 자동으로 낸다 (허브 타이머)
func (g *KGGame) ForcePlay() {
	if g.Phase != KGPhasePlaying {
		return
	}
	seat := g.CurrentSeat
	if seat < 0 || seat >= len(g.Players) {
		return
	}
	p := g.Players[seat]
	legal := kgLegalIndexes(p.Hand, g.LeadSuit)
	if len(legal) == 0 {
		return
	}
	pick := legal[0]
	for _, i := range legal {
		if kgCardPower(p.Hand[i]) < kgCardPower(p.Hand[pick]) {
			pick = i
		}
	}
	g.playCard(seat, pick)
}

// playCard 검증이 끝난 한 장을 실제로 내려놓는다
func (g *KGGame) playCard(seat, index int) {
	p := g.Players[seat]
	card := p.Hand[index]
	p.Hand = append(p.Hand[:index], p.Hand[index+1:]...)
	g.Trick = append(g.Trick, KGTrickPlay{Seat: seat, Card: card})

	// 리드 무늬는 첫 숫자 카드가 정한다 (탈출로 리드하면 다음 숫자 카드가 정함)
	if g.LeadSuit == KGSuitNone && card.Kind == KGKindNumber {
		g.LeadSuit = card.Suit
	}
	g.emit("played", seat, fmt.Sprintf("%s님이 %s을(를) 냈습니다", p.Name, kgCardLabel(card)))

	if len(g.Trick) < len(g.Players) {
		g.CurrentSeat = (seat + 1) % len(g.Players)
		g.StateSeq++
		return
	}
	g.resolveTrick()
}

// resolveTrick 트릭 승자 판정 + 보너스 적립. 보너스는 라운드 정산에서
// 비드를 맞힌 경우에만 가산된다.
func (g *KGGame) resolveTrick() {
	idx := kgTrickWinner(g.Trick, g.LeadSuit)
	winnerSeat := g.Trick[idx].Seat
	winner := g.Players[winnerSeat]
	winner.Tricks++

	bonus, notes := kgTrickBonus(g.Trick, idx)
	winner.Bonus += bonus

	g.LastTrick = &KGLastTrick{WinnerSeat: winnerSeat,
		Cards: append([]KGTrickPlay{}, g.Trick...)}

	msg := fmt.Sprintf("%d트릭 — %s님이 %s(으)로 가져갔습니다",
		g.TrickNo, winner.Name, kgCardLabel(g.Trick[idx].Card))
	if bonus > 0 {
		msg += fmt.Sprintf(" (보너스 +%d: %s)", bonus, strings.Join(notes, ", "))
	}
	g.emit("trick_won", winnerSeat, msg)

	g.Trick = []KGTrickPlay{}
	g.LeadSuit = KGSuitNone

	if g.TrickNo >= g.Round {
		g.settleRound()
		return
	}
	g.TrickNo++
	g.LeadSeat = winnerSeat
	g.CurrentSeat = winnerSeat
	g.StateSeq++
}

// kgTrickBonus 트릭 승자가 챙기는 보너스와 한글 사유.
//   - 색 13 획득 +10 / 검정 13 획득 +20 (그 트릭에 포함돼 있으면)
//   - 인어로 스컬킹을 잡으면 +50
//   - 스컬킹으로 해적을 잡으면 해적 1장당 +30
func kgTrickBonus(plays []KGTrickPlay, winnerIdx int) (int, []string) {
	bonus := 0
	notes := []string{}
	if winnerIdx < 0 || winnerIdx >= len(plays) {
		return 0, notes
	}
	for _, p := range plays {
		if p.Card.Kind != KGKindNumber || p.Card.Rank != KGSuitRankMax {
			continue
		}
		if p.Card.Suit == KGSuitBlack {
			bonus += 20
			notes = append(notes, "검정 13 +20")
		} else {
			bonus += 10
			notes = append(notes, kgSuitLabel(p.Card.Suit)+" 13 +10")
		}
	}
	switch plays[winnerIdx].Card.Kind {
	case KGKindMermaid:
		for _, p := range plays {
			if p.Card.Kind == KGKindSkullKing {
				bonus += 50
				notes = append(notes, "인어가 스컬킹 포획 +50")
			}
		}
	case KGKindSkullKing:
		pirates := 0
		for _, p := range plays {
			if p.Card.Kind == KGKindPirate {
				pirates++
			}
		}
		if pirates > 0 {
			bonus += 30 * pirates
			notes = append(notes, fmt.Sprintf("스컬킹이 해적 %d명 포획 +%d", pirates, 30*pirates))
		}
	}
	return bonus, notes
}

// ==================== 정산 ====================

// settleRound 라운드 점수 정산.
//   - 비드 ≥ 1: 정확히 맞히면 20×비드 + 보너스, 틀리면 -10×|비드-실제|
//   - 비드 = 0: 맞히면 10×라운드, 틀리면 -10×라운드
func (g *KGGame) settleRound() {
	rows := []KGRoundRow{}
	parts := []string{}
	for _, p := range g.Players {
		hit := p.Tricks == p.Bid
		delta := 0
		switch {
		case p.Bid == 0 && hit:
			delta = 10 * g.Round
		case p.Bid == 0:
			delta = -10 * g.Round
		case hit:
			delta = 20 * p.Bid
		default:
			delta = -10 * kgAbs(p.Bid-p.Tricks)
		}
		if hit {
			delta += p.Bonus
		}
		p.Score += delta
		rows = append(rows, KGRoundRow{Seat: p.Seat, Bid: p.Bid, Tricks: p.Tricks,
			Delta: delta, Total: p.Score})
		parts = append(parts, fmt.Sprintf("%s %d/%d %+d", p.Name, p.Tricks, p.Bid, delta))
	}

	msg := fmt.Sprintf("%d라운드 정산 — %s", g.Round, strings.Join(parts, " · "))
	g.RoundResult = &KGRoundResult{Rows: rows, Message: msg}
	g.Phase = KGPhaseRoundEnd
	g.CurrentSeat = -1
	g.StateSeq++
	g.emit("round_end", -1, msg)
}

// NextRound 라운드 정산 대기가 끝나면 다음 라운드를 열거나 게임을 끝낸다
// (허브 타이머).
func (g *KGGame) NextRound(rng *rand.Rand) {
	if g.Phase != KGPhaseRoundEnd {
		return
	}
	if g.Round >= g.MaxRound {
		g.finish()
		return
	}
	g.Round++
	g.beginRound(rng)
}

// finish 마지막 라운드 후 총점 최고가 승리 (동점 공동)
func (g *KGGame) finish() {
	best := 0
	for i, p := range g.Players {
		if i == 0 || p.Score > best {
			best = p.Score
		}
	}
	winners := []int{}
	names := []string{}
	for _, p := range g.Players {
		if p.Score == best {
			winners = append(winners, p.Seat)
			names = append(names, p.Name)
		}
	}
	g.Winners = winners
	g.Phase = KGPhaseGameOver
	g.CurrentSeat = -1
	g.StateSeq++

	if len(winners) > 1 {
		g.emit("game_over", -1, fmt.Sprintf("게임 종료 — %s님이 %d점으로 공동 우승했습니다",
			strings.Join(names, "·"), best))
		return
	}
	g.emit("game_over", -1, fmt.Sprintf("게임 종료 — %s님이 %d점으로 우승했습니다",
		strings.Join(names, "·"), best))
}

// WinnerNames 공동 1위 이름 (· 로 이음)
func (g *KGGame) WinnerNames() string {
	names := []string{}
	for _, seat := range g.Winners {
		if seat >= 0 && seat < len(g.Players) {
			names = append(names, g.Players[seat].Name)
		}
	}
	return strings.Join(names, "·")
}

// BestScore 현재 최고 점수 (참가자가 없으면 0)
func (g *KGGame) BestScore() int {
	best := 0
	for i, p := range g.Players {
		if i == 0 || p.Score > best {
			best = p.Score
		}
	}
	return best
}

func kgAbs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
