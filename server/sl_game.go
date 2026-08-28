package server

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// ==================== 스플렌더 순수 규칙 ====================
//
// 덱 구성·차례 진행·비용 계산·귀족 판정·종료 판정만 다룬다. 클라이언트·
// 타이머를 모르며, 허브(sl_hub.go)가 차례 마감(60초)·버리기 마감(20초)을
// 걸고 이벤트 큐(DrainEvents)를 방송한다.
//
// 이 파일의 심장은 slPayment 다. 개발 카드 하나를 살 때 실제로 내는 값은
//
//	필요 = max(0, 정가 - 내 보너스)          ← 보너스 차감
//	지불 = min(필요, 내 그 색 토큰)
//	부족 = 필요 - 지불                        ← 황금으로 메운다
//
// 이고, 부족분 합계가 보유 황금을 넘으면 살 수 없다. 보너스는 토큰처럼
// 쓰이지만 소모되지 않는다 — 눈덩이가 굴러가는 이유다.

// NewSLGame 대기 상태의 새 게임
func NewSLGame(id string) *SLGame {
	return &SLGame{
		ID:          id,
		Players:     []*SLPlayer{},
		Phase:       SLPhaseWaiting,
		Nobles:      []SLNoble{},
		CurrentSeat: -1,
	}
}

// AddPlayer 대기실 입장. 좌석 번호를 돌려준다.
func (g *SLGame) AddPlayer(name string) (int, error) {
	if g.Phase != SLPhaseWaiting {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= SLMaxPlayers {
		return -1, fmt.Errorf("자리가 없습니다 (최대 %d명)", SLMaxPlayers)
	}
	seat := len(g.Players)
	g.Players = append(g.Players, &SLPlayer{
		Seat:     seat,
		Name:     name,
		Reserved: []SLCard{},
		Nobles:   []int{},
	})
	return seat, nil
}

// RemovePlayer 대기실 퇴장. 좌석을 앞으로 당겨 빈틈을 없앤다.
func (g *SLGame) RemovePlayer(seat int) {
	if g.Phase != SLPhaseWaiting || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i, p := range g.Players {
		p.Seat = i
	}
}

// CanStart 시작 가능 여부 (호스트 명시 시작 — 2인부터)
func (g *SLGame) CanStart() bool {
	return g.Phase == SLPhaseWaiting && len(g.Players) >= SLMinPlayers
}

// ==================== 개발 카드 덱 ====================
//
// 원작의 90장(1단계 40 · 2단계 30 · 3단계 20)은 다섯 색에 대해 완전히 대칭이다
// — 같은 "비용 모양"이 색만 돌아가며 한 번씩 나온다. 그래서 모양표를
// 슬롯 오프셋으로 적어 두고 다섯 색에 돌려 쓴다.
//
//	오프셋 0 = 그 카드의 보너스 색 자신 (2·3단계에만 등장한다)
//	오프셋 k = slGems 순서로 k칸 뒤의 색
//
// 장수: 1단계 8모양 × 5색 = 40, 2단계 6모양 × 5색 = 30, 3단계 4모양 × 5색 = 20.

// slCardShape 비용 모양 하나 — cost[오프셋] = 개수
type slCardShape struct {
	points int
	cost   map[int]int
}

// slTier1Shapes 1단계 8모양. 값싸고 대부분 0점이며, 한 장만 1점(단일색 4개)이다.
var slTier1Shapes = []slCardShape{
	{points: 0, cost: map[int]int{1: 1, 2: 1, 3: 1, 4: 1}},
	{points: 0, cost: map[int]int{1: 1, 2: 2, 3: 1, 4: 1}},
	{points: 0, cost: map[int]int{1: 2, 2: 2, 3: 1}},
	{points: 0, cost: map[int]int{2: 2, 3: 2}},
	{points: 0, cost: map[int]int{1: 2, 4: 1}},
	{points: 0, cost: map[int]int{3: 3}},
	{points: 0, cost: map[int]int{1: 1, 3: 1, 4: 1}},
	{points: 1, cost: map[int]int{2: 4}},
}

// slTier2Shapes 2단계 6모양. 1~3점이고 자기 색을 비용에 요구하기 시작한다.
var slTier2Shapes = []slCardShape{
	{points: 1, cost: map[int]int{1: 3, 2: 2, 3: 2}},
	{points: 1, cost: map[int]int{2: 3, 3: 2, 4: 3}},
	{points: 2, cost: map[int]int{3: 5}},
	{points: 2, cost: map[int]int{1: 4, 2: 2, 4: 1}},
	{points: 2, cost: map[int]int{0: 3, 4: 5}},
	{points: 3, cost: map[int]int{0: 6}},
}

// slTier3Shapes 3단계 4모양. 3~5점의 큰 카드로, 보너스 없이는 감당할 수 없다.
var slTier3Shapes = []slCardShape{
	{points: 3, cost: map[int]int{1: 3, 2: 3, 3: 5, 4: 3}},
	{points: 4, cost: map[int]int{3: 7}},
	{points: 4, cost: map[int]int{0: 3, 2: 3, 3: 6}},
	{points: 5, cost: map[int]int{0: 3, 3: 7}},
}

// slBuildDeck 단계(1~3)의 카드 전량을 만든다. id 는 1부터 이어 붙인다
// (0 은 payload 에서 "지정 없음"이라 카드 id 로 쓰지 않는다).
func slBuildDeck(tier int, nextID *int) []SLCard {
	var shapes []slCardShape
	switch tier {
	case 1:
		shapes = slTier1Shapes
	case 2:
		shapes = slTier2Shapes
	default:
		shapes = slTier3Shapes
	}

	cards := []SLCard{}
	for gi, gem := range slGems {
		for _, shape := range shapes {
			cost := SLGemSet{}
			for offset, n := range shape.cost {
				cost.add(slGems[(gi+offset)%len(slGems)], n)
			}
			*nextID++
			cards = append(cards, SLCard{
				ID: *nextID, Tier: tier, Points: shape.points, Gem: gem, Cost: cost,
			})
		}
	}
	return cards
}

// slAllNobles 귀족 타일 10장 — 전부 3점. 두 색 4개씩이거나 세 색 3개씩이다.
func slAllNobles() []SLNoble {
	nobles := []SLNoble{}
	id := 0
	// 세 색 3개씩 (이웃한 세 색을 돌려가며 5장)
	for i := range slGems {
		cost := SLGemSet{}
		for k := 0; k < 3; k++ {
			cost.add(slGems[(i+k)%len(slGems)], 3)
		}
		id++
		nobles = append(nobles, SLNoble{ID: id, Points: 3, Cost: cost})
	}
	// 두 색 4개씩 (이웃한 두 색을 돌려가며 5장)
	for i := range slGems {
		cost := SLGemSet{}
		cost.add(slGems[i], 4)
		cost.add(slGems[(i+1)%len(slGems)], 4)
		id++
		nobles = append(nobles, SLNoble{ID: id, Points: 3, Cost: cost})
	}
	return nobles
}

// ==================== 시작 ====================

// Start 덱을 섞고 진열·귀족 타일·공동 창고를 세팅한 뒤 첫 차례를 연다
func (g *SLGame) Start(rng *rand.Rand) error {
	if !g.CanStart() {
		return fmt.Errorf("%d명 이상 모여야 시작할 수 있습니다", SLMinPlayers)
	}
	n := len(g.Players)

	nextID := 0
	for tier := 1; tier <= 3; tier++ {
		deck := slBuildDeck(tier, &nextID)
		rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })
		g.Decks[tier-1] = deck
		g.Board[tier-1] = []SLCard{}
	}
	for tier := 1; tier <= 3; tier++ {
		for len(g.Board[tier-1]) < SLBoardSlots {
			if !g.drawToBoard(tier) {
				break
			}
		}
	}

	nobles := slAllNobles()
	rng.Shuffle(len(nobles), func(i, j int) { nobles[i], nobles[j] = nobles[j], nobles[i] })
	want := slNobleCount(n)
	if want > len(nobles) {
		want = len(nobles)
	}
	g.Nobles = append([]SLNoble{}, nobles[:want]...)
	// 자동 획득은 "번호가 앞선 것" 하나만 고르므로 진열 순서를 id 순으로 굳힌다
	sort.Slice(g.Nobles, func(i, j int) bool { return g.Nobles[i].ID < g.Nobles[j].ID })

	per := slBankFor(n)
	g.Bank = SLTokenSet{
		Diamond: per, Sapphire: per, Emerald: per, Ruby: per, Onyx: per,
		Gold: SLGoldCount,
	}

	g.Phase = SLPhaseTurn
	g.CurrentSeat = 0
	g.Turns = 0
	g.Ready = true
	g.StartedAt = time.Now()
	g.StateSeq++
	return nil
}

// drawToBoard 단계 덱에서 한 장을 진열로 올린다 (덱이 비면 false)
func (g *SLGame) drawToBoard(tier int) bool {
	deck := g.Decks[tier-1]
	if len(deck) == 0 {
		return false
	}
	g.Board[tier-1] = append(g.Board[tier-1], deck[0])
	g.Decks[tier-1] = deck[1:]
	return true
}

// ==================== 이벤트 ====================

func (g *SLGame) emit(kind string, seat int, msg string) {
	g.events = append(g.events, SLGameEvent{Kind: kind, Seat: seat, Message: msg})
}

// DrainEvents 쌓인 이벤트를 꺼내 비운다 (허브가 방송한다)
func (g *SLGame) DrainEvents() []SLGameEvent {
	out := g.events
	g.events = nil
	return out
}

// note 마지막 행동 요약 기록 + 이벤트 방송
func (g *SLGame) note(seat int, kind, msg string) {
	name := ""
	if seat >= 0 && seat < len(g.Players) {
		name = g.Players[seat].Name
	}
	g.LastAction = &SLLastAction{Seat: seat, Name: name, Message: msg}
	g.emit(kind, seat, msg)
}

// ==================== 비용 계산 (이 게임의 심장) ====================

// slPayment 개발 카드 하나를 살 때 실제로 내는 값을 계산한다.
//
//	spend — 색별로 낼 보석 토큰 수 (보너스로 깎고 남은 몫)
//	gold  — 모자란 자리를 메울 황금 수
//	ok    — 보유 황금으로도 못 메우면 false
//
// 보너스(cards)는 토큰처럼 쓰이지만 소모되지 않는다.
func slPayment(card SLCard, bonus SLGemSet, tokens SLTokenSet) (spend SLGemSet, gold int, ok bool) {
	for _, gem := range slGems {
		need := card.Cost.get(gem) - bonus.get(gem)
		if need <= 0 {
			continue
		}
		pay := tokens.get(gem)
		if pay > need {
			pay = need
		}
		spend.add(gem, pay)
		gold += need - pay
	}
	return spend, gold, gold <= tokens.Gold
}

// slCanAfford 지금 살 수 있는가 (봇·프론트 강조의 판정 근거와 같은 함수)
func slCanAfford(card SLCard, p *SLPlayer) bool {
	_, _, ok := slPayment(card, p.Cards, p.Tokens)
	return ok
}

// ==================== 차례 진행 ====================

// checkTurn 차례·단계 검사
func (g *SLGame) checkTurn(seat int) (*SLPlayer, error) {
	if g.Phase != SLPhaseTurn {
		if g.Phase == SLPhaseDiscard {
			return nil, errors.New("먼저 토큰을 10개로 맞춰야 합니다")
		}
		return nil, errors.New("지금은 할 수 없습니다")
	}
	if seat < 0 || seat >= len(g.Players) {
		return nil, errors.New("잘못된 좌석입니다")
	}
	if seat != g.CurrentSeat {
		return nil, errors.New("차례가 아닙니다")
	}
	return g.Players[seat], nil
}

// Take 토큰 가져오기 — 서로 다른 색 3개 또는 같은 색 2개.
//
// 공동 창고에 남은 색이 3가지 미만이면 그만큼만 가져올 수 있다(원작 규칙).
// 같은 색 2개는 그 색이 4개 이상 남아 있을 때만 된다.
func (g *SLGame) Take(seat int, colors []SLGem) error {
	p, err := g.checkTurn(seat)
	if err != nil {
		return err
	}
	if len(colors) == 0 {
		return errors.New("가져올 토큰을 고르세요")
	}
	for _, c := range colors {
		if !slGemValid(c) {
			return errors.New("황금은 직접 가져올 수 없습니다")
		}
	}

	// 같은 색 2개
	if len(colors) == SLTakeSame && colors[0] == colors[1] {
		gem := colors[0]
		if g.Bank.get(gem) < SLTakeSameMin {
			return fmt.Errorf("%s이(가) 공동 창고에 %d개 이상 있어야 2개를 가져올 수 있습니다",
				slGemLabel(gem), SLTakeSameMin)
		}
		g.Bank.add(gem, -SLTakeSame)
		p.Tokens.add(gem, SLTakeSame)
		g.note(seat, "take", fmt.Sprintf("%s님이 %s 2개를 가져왔습니다", p.Name, slGemLabel(gem)))
		g.afterAction(p)
		return nil
	}

	// 서로 다른 색
	if len(colors) > SLTakeDistinct {
		return fmt.Errorf("서로 다른 색은 %d개까지 가져올 수 있습니다", SLTakeDistinct)
	}
	seen := map[SLGem]bool{}
	for _, c := range colors {
		if seen[c] {
			return errors.New("같은 색을 2개 가져오려면 정확히 2개만 골라야 합니다")
		}
		seen[c] = true
		if g.Bank.get(c) < 1 {
			return fmt.Errorf("공동 창고에 %s이(가) 없습니다", slGemLabel(c))
		}
	}
	// 3개 미만이면 "남은 색이 그것뿐"일 때만 허용한다 (덜 가져오는 꼼수 방지)
	if len(colors) < SLTakeDistinct {
		avail := 0
		for _, gem := range slGems {
			if g.Bank.get(gem) > 0 {
				avail++
			}
		}
		if avail > len(colors) {
			return fmt.Errorf("서로 다른 색 %d개를 가져와야 합니다", SLTakeDistinct)
		}
	}

	names := []string{}
	for _, c := range colors {
		g.Bank.add(c, -1)
		p.Tokens.add(c, 1)
		names = append(names, slGemLabel(c))
	}
	g.note(seat, "take", fmt.Sprintf("%s님이 %s 토큰을 가져왔습니다",
		p.Name, strings.Join(names, "·")))
	g.afterAction(p)
	return nil
}

// Reserve 개발 카드 예약 + 황금 1개(공동 창고에 있으면).
// cardID 가 1 이상이면 공개 카드를, 아니면 tier(1~3) 덱 맨 위를 비공개로 쥔다.
func (g *SLGame) Reserve(seat, cardID, tier int) error {
	p, err := g.checkTurn(seat)
	if err != nil {
		return err
	}
	if len(p.Reserved) >= SLMaxReserved {
		return fmt.Errorf("예약은 최대 %d장까지 할 수 있습니다", SLMaxReserved)
	}

	var card SLCard
	hidden := false
	if cardID > 0 {
		t, idx := g.findBoard(cardID)
		if t < 0 {
			return errors.New("진열에 없는 개발 카드입니다")
		}
		card = g.Board[t-1][idx]
		g.takeFromBoard(t, idx)
	} else {
		if tier < 1 || tier > 3 {
			return errors.New("예약할 개발 카드를 고르세요")
		}
		if len(g.Decks[tier-1]) == 0 {
			return fmt.Errorf("%d단계 덱이 비었습니다", tier)
		}
		card = g.Decks[tier-1][0]
		g.Decks[tier-1] = g.Decks[tier-1][1:]
		hidden = true
	}

	p.Reserved = append(p.Reserved, card)
	gotGold := false
	if g.Bank.Gold > 0 {
		g.Bank.Gold--
		p.Tokens.Gold++
		gotGold = true
	}

	// 이벤트에는 어느 단계인지까지만 적는다 — 비공개 예약의 내용은 남에게
	// 절대 알리지 않는다 (은닉 계약)
	where := fmt.Sprintf("%d단계 공개 카드", card.Tier)
	if hidden {
		where = fmt.Sprintf("%d단계 덱 맨 위 카드", card.Tier)
	}
	msg := fmt.Sprintf("%s님이 %s를 예약했습니다", p.Name, where)
	if gotGold {
		msg = fmt.Sprintf("%s님이 %s를 예약하고 황금 1개를 가져왔습니다", p.Name, where)
	}
	g.note(seat, "reserve", msg)
	g.afterAction(p)
	return nil
}

// Buy 개발 카드 구매 — 진열 공개 카드 또는 내가 예약한 카드
func (g *SLGame) Buy(seat, cardID int) error {
	p, err := g.checkTurn(seat)
	if err != nil {
		return err
	}
	if cardID <= 0 {
		return errors.New("구매할 개발 카드를 고르세요")
	}

	var card SLCard
	fromReserve := -1
	for i, c := range p.Reserved {
		if c.ID == cardID {
			card, fromReserve = c, i
			break
		}
	}
	tier, idx := -1, -1
	if fromReserve < 0 {
		tier, idx = g.findBoard(cardID)
		if tier < 0 {
			return errors.New("살 수 없는 개발 카드입니다")
		}
		card = g.Board[tier-1][idx]
	}

	spend, gold, ok := slPayment(card, p.Cards, p.Tokens)
	if !ok {
		return errors.New("토큰이 모자랍니다")
	}

	for _, gem := range slGems {
		if n := spend.get(gem); n > 0 {
			p.Tokens.add(gem, -n)
			g.Bank.add(gem, n)
		}
	}
	if gold > 0 {
		p.Tokens.Gold -= gold
		g.Bank.Gold += gold
	}

	if fromReserve >= 0 {
		p.Reserved = append(p.Reserved[:fromReserve], p.Reserved[fromReserve+1:]...)
	} else {
		g.takeFromBoard(tier, idx)
	}

	p.Cards.add(card.Gem, 1)
	p.Points += card.Points

	via := ""
	if fromReserve >= 0 {
		via = "예약해 둔 "
	}
	g.note(seat, "buy", fmt.Sprintf("%s님이 %s%d단계 %s 개발 카드를 샀습니다 (명성 점수 %d)",
		p.Name, via, card.Tier, slGemLabel(card.Gem), card.Points))
	g.afterAction(p)
	return nil
}

// Discard 10개 초과분 버리기. 정확히 초과분만큼 골라야 한다.
func (g *SLGame) Discard(seat int, colors []SLGem) error {
	if g.Phase != SLPhaseDiscard {
		return errors.New("지금은 버릴 수 없습니다")
	}
	if seat < 0 || seat >= len(g.Players) || seat != g.CurrentSeat {
		return errors.New("차례가 아닙니다")
	}
	p := g.Players[seat]
	over := p.Tokens.total() - SLTokenLimit
	if over <= 0 { // 방어선 — 여기 올 일이 없다
		g.endTurn()
		return nil
	}
	if len(colors) != over {
		return fmt.Errorf("정확히 %d개를 버려야 합니다", over)
	}

	tmp := p.Tokens
	for _, c := range colors {
		if c != SLGold && !slGemValid(c) {
			return errors.New("알 수 없는 토큰입니다")
		}
		if tmp.get(c) <= 0 {
			return fmt.Errorf("%s 토큰이 모자랍니다", slGemLabel(c))
		}
		tmp.add(c, -1)
	}

	names := []string{}
	for _, c := range colors {
		p.Tokens.add(c, -1)
		g.Bank.add(c, 1)
		names = append(names, slGemLabel(c))
	}
	g.note(seat, "discard", fmt.Sprintf("%s님이 %s 토큰을 공동 창고에 돌려놨습니다",
		p.Name, strings.Join(names, "·")))
	g.endTurn()
	return nil
}

// afterAction 행동 하나가 끝난 뒤 — 10개를 넘으면 버리기 단계로, 아니면 차례 종료
func (g *SLGame) afterAction(p *SLPlayer) {
	if p.Tokens.total() > SLTokenLimit {
		g.Phase = SLPhaseDiscard
		g.StateSeq++
		g.emit("discard_needed", p.Seat, fmt.Sprintf(
			"%s님의 토큰이 %d개입니다 — %d개를 버려 %d개로 맞춰야 합니다",
			p.Name, p.Tokens.total(), p.Tokens.total()-SLTokenLimit, SLTokenLimit))
		return
	}
	g.endTurn()
}

// ==================== 귀족 타일 ====================

// slNobleMet 귀족 타일의 요구 보너스를 모두 충족했는가
func slNobleMet(noble SLNoble, bonus SLGemSet) bool {
	for _, gem := range slGems {
		if bonus.get(gem) < noble.Cost.get(gem) {
			return false
		}
	}
	return true
}

// awardNoble 차례 끝의 귀족 방문. 여러 장에 해당하면 번호가 앞선 것 하나만
// 자동으로 가져가고 그 사실을 이벤트로 알린다 (스펙의 단순화).
func (g *SLGame) awardNoble(p *SLPlayer) {
	best := -1
	extra := 0
	for i, noble := range g.Nobles {
		if !slNobleMet(noble, p.Cards) {
			continue
		}
		if best < 0 {
			best = i
			continue
		}
		extra++
	}
	if best < 0 {
		return
	}
	noble := g.Nobles[best]
	g.Nobles = append(g.Nobles[:best], g.Nobles[best+1:]...)
	p.Nobles = append(p.Nobles, noble.ID)
	p.Points += noble.Points

	msg := fmt.Sprintf("%s님에게 귀족 타일 %d번이 찾아왔습니다 (명성 점수 %d)",
		p.Name, noble.ID, noble.Points)
	if extra > 0 {
		msg += fmt.Sprintf(" — 조건을 만족한 귀족 타일이 %d장이라 번호가 앞선 한 장만 갑니다", extra+1)
	}
	g.emit("noble", p.Seat, msg)
}

// ==================== 차례 종료 / 승패 ====================

// endTurn 귀족 판정 → 마지막 라운드 판정 → 다음 좌석.
//
// 종료 판정: 누군가 15점에 닿으면 LastRound 를 켜고 그 라운드를 끝까지
// 진행한다. 차례가 좌석 0으로 돌아오는 순간이 라운드의 끝이므로, 그때
// 모두가 같은 횟수의 차례를 가졌음이 보장된다.
func (g *SLGame) endTurn() {
	if g.Phase == SLPhaseGameOver {
		return
	}
	seat := g.CurrentSeat
	if seat >= 0 && seat < len(g.Players) {
		p := g.Players[seat]
		g.awardNoble(p)
		if !g.LastRound && p.Points >= SLWinPoints {
			g.LastRound = true
			g.emit("last_round", seat, fmt.Sprintf(
				"%s님이 명성 점수 %d점에 닿았습니다 — 이번 라운드까지만 진행합니다",
				p.Name, SLWinPoints))
		}
	}

	g.Turns++
	n := len(g.Players)
	if n == 0 { // 방어선
		return
	}
	next := (seat + 1) % n

	if g.LastRound && next == 0 {
		g.finish("명성 점수 목표에 닿아 마지막 라운드가 끝났습니다")
		return
	}
	if g.Turns >= SLMaxTurns {
		g.finish("차례 상한에 닿아 경기를 마칩니다")
		return
	}

	g.CurrentSeat = next
	g.Phase = SLPhaseTurn
	g.StateSeq++
}

// finish 승패 판정 — 최고 명성 점수, 동점이면 개발 카드 수가 적은 쪽,
// 그래도 같으면 공동 승
func (g *SLGame) finish(reason string) {
	g.Phase = SLPhaseGameOver
	g.CurrentSeat = -1
	g.Deadline = 0
	g.StateSeq++

	bestPoints, fewestCards := -1, 1<<30
	for _, p := range g.Players {
		cards := p.Cards.total()
		if p.Points > bestPoints || (p.Points == bestPoints && cards < fewestCards) {
			bestPoints, fewestCards = p.Points, cards
		}
	}

	seats, names := []int{}, []string{}
	for _, p := range g.Players {
		if p.Points == bestPoints && p.Cards.total() == fewestCards {
			seats = append(seats, p.Seat)
			names = append(names, p.Name)
		}
	}

	msg := fmt.Sprintf("%s — %s님이 명성 점수 %d점으로 승리했습니다",
		reason, strings.Join(names, "·"), bestPoints)
	if len(seats) > 1 {
		msg = fmt.Sprintf("%s — %s님이 명성 점수 %d점·개발 카드 %d장으로 공동 승리했습니다",
			reason, strings.Join(names, "·"), bestPoints, fewestCards)
	}
	g.Result = &SLResult{WinnerSeats: seats, WinnerNames: names, Message: msg}
	g.emit("game_over", -1, msg)
}

// ==================== 진열 조회 ====================

// findBoard 카드 id 로 진열에서 찾는다 (tier 1~3, 못 찾으면 tier -1)
func (g *SLGame) findBoard(cardID int) (tier, idx int) {
	for t := 1; t <= 3; t++ {
		for i, c := range g.Board[t-1] {
			if c.ID == cardID {
				return t, i
			}
		}
	}
	return -1, -1
}

// takeFromBoard 진열에서 한 장을 떼고 같은 단계 덱에서 채운다
func (g *SLGame) takeFromBoard(tier, idx int) {
	row := g.Board[tier-1]
	g.Board[tier-1] = append(row[:idx], row[idx+1:]...)
	g.drawToBoard(tier)
}

// ==================== AFK 자동 진행 ====================

// ForceAction 차례 마감 자동 행동 — 토큰 3색 우선, 없으면 구매, 없으면 예약.
// 셋 다 불가능하면 차례만 넘긴다 (판이 멈추지 않게 하는 방어선).
func (g *SLGame) ForceAction(rng *rand.Rand) {
	if g.Phase != SLPhaseTurn {
		return
	}
	seat := g.CurrentSeat
	if seat < 0 || seat >= len(g.Players) {
		return
	}
	p := g.Players[seat]

	// ① 서로 다른 색 토큰 (남은 색이 3가지 미만이면 있는 만큼)
	avail := []SLGem{}
	for _, gem := range slGems {
		if g.Bank.get(gem) > 0 {
			avail = append(avail, gem)
		}
	}
	if len(avail) > 0 {
		take := avail
		if len(take) > SLTakeDistinct {
			rng.Shuffle(len(take), func(i, j int) { take[i], take[j] = take[j], take[i] })
			take = take[:SLTakeDistinct]
		}
		if g.Take(seat, take) == nil {
			return
		}
	}

	// ② 구매 — 살 수 있는 것 중 명성 점수가 높은 카드
	best, bestPoints := 0, -1
	for _, c := range p.Reserved {
		if slCanAfford(c, p) && c.Points > bestPoints {
			best, bestPoints = c.ID, c.Points
		}
	}
	for t := 1; t <= 3; t++ {
		for _, c := range g.Board[t-1] {
			if slCanAfford(c, p) && c.Points > bestPoints {
				best, bestPoints = c.ID, c.Points
			}
		}
	}
	if best > 0 && g.Buy(seat, best) == nil {
		return
	}

	// ③ 예약 — 진열 첫 카드, 없으면 덱 맨 위
	if len(p.Reserved) < SLMaxReserved {
		for t := 1; t <= 3; t++ {
			if len(g.Board[t-1]) > 0 && g.Reserve(seat, g.Board[t-1][0].ID, 0) == nil {
				return
			}
		}
		for t := 1; t <= 3; t++ {
			if g.Reserve(seat, 0, t) == nil {
				return
			}
		}
	}

	// ④ 아무것도 못 한다 — 차례만 넘긴다
	g.note(seat, "pass", fmt.Sprintf("%s님이 할 수 있는 행동이 없어 차례를 넘깁니다", p.Name))
	g.endTurn()
}

// ForceDiscard 버리기 마감 자동 처리 — 무작위로 버려 10개를 맞춘다
func (g *SLGame) ForceDiscard(rng *rand.Rand) {
	if g.Phase != SLPhaseDiscard {
		return
	}
	seat := g.CurrentSeat
	if seat < 0 || seat >= len(g.Players) {
		return
	}
	p := g.Players[seat]

	pool := []SLGem{}
	for _, gem := range append(slGems[:], SLGold) {
		for i := 0; i < p.Tokens.get(gem); i++ {
			pool = append(pool, gem)
		}
	}
	rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })

	over := p.Tokens.total() - SLTokenLimit
	if over <= 0 || over > len(pool) {
		g.endTurn()
		return
	}
	if err := g.Discard(seat, pool[:over]); err != nil { // 방어선
		g.endTurn()
	}
}
