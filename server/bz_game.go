package server

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// ==================== 보난자 순수 규칙 ====================
//
// 덱 구성(104장)·배분·차례 4단계·거래·수확(콩미터)·세 번째 콩밭·덱 소진 종료·
// 최종 정산만 다룬다. 클라이언트·타이머를 모르며, 허브(bz_hub.go)가 단계별
// 마감(심기 30초 · 거래 60초 · 받은 카드 심기 20초)을 걸고 이벤트 큐
// (DrainEvents)를 방송한다.
//
// ─────────────── 손패 순서 불변 — 이 파일의 첫 번째 계약 ───────────────
//
// 손패를 만지는 곳은 딱 네 군데뿐이다.
//
//	Start           : 시작 5장을 순서대로 append
//	plantFront      : g.Players[seat].Hand[0] 만 뺀다  (맨 앞)
//	drawPhase       : 뽑은 카드를 append 한다          (맨 뒤)
//	execTrade       : 거래로 내주는 카드를 인덱스로 뺀다 (남은 상대 순서 유지)
//
// **정렬·역순·섞기·인덱스 교환은 어디에도 없다.** 새 코드를 넣을 때도
// 이 네 군데 밖에서 Hand 를 건드리지 마라. bz_game_test.go 의
// TestBZHandOrderIsImmutable 이 이 계약을 지킨다.
//
// ─────────────── 덱 소진과 종료 ───────────────
//
// 카드가 필요한데 덱이 비면 그것이 "소진 1회"다. DeckCycle 을 올리고
// 버린 더미를 섞어 덱으로 되돌린다. EndCycle(4~5인 3 · 3인 2)에 도달하면
// 되돌리지 않고 그 자리에서 게임이 끝난다. 손패는 치우고 모든 밭을 수확해
// 정산하며, 금화가 같으면 손에 든 카드가 많은 쪽이 이긴다.
//
// 카드 총량 회계: 덱 + 버린 더미 + 전원 손패 + 전원 밭 + 전원 받은 카드 +
// 공개 카드 + 전원 금화 + 세 번째 밭 값(SpentCoins) = 104장. 항상 성립한다.

// NewBZGame 대기 상태의 새 게임
func NewBZGame(id string) *BZGame {
	return &BZGame{
		ID:          id,
		Players:     []*BZPlayer{},
		Phase:       BZPhaseWaiting,
		Deck:        []BZBean{},
		Discard:     []BZBean{},
		Flipped:     []BZBean{},
		Offers:      []*BZOffer{},
		CurrentSeat: -1,
	}
}

// AddPlayer 대기실 입장. 좌석 번호를 돌려준다.
func (g *BZGame) AddPlayer(name string) (int, error) {
	if g.Phase != BZPhaseWaiting {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= BZMaxPlayers {
		return -1, fmt.Errorf("자리가 없습니다 (최대 %d명)", BZMaxPlayers)
	}
	seat := len(g.Players)
	g.Players = append(g.Players, &BZPlayer{
		Seat:    seat,
		Name:    name,
		Hand:    []BZBean{},
		Fields:  bzNewFields(),
		Pending: []BZBean{},
	})
	return seat, nil
}

// bzNewFields 시작 콩밭 2개 (빈 밭)
func bzNewFields() []BZField {
	fields := make([]BZField, BZStartFields)
	return fields
}

// RemovePlayer 대기실 퇴장. 좌석을 앞으로 당겨 빈틈을 없앤다.
func (g *BZGame) RemovePlayer(seat int) {
	if g.Phase != BZPhaseWaiting || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i, p := range g.Players {
		p.Seat = i
	}
}

// CanStart 시작 가능 여부 (호스트 명시 시작 — 3인부터)
func (g *BZGame) CanStart() bool {
	return g.Phase == BZPhaseWaiting && len(g.Players) >= BZMinPlayers
}

// bzBuildDeck 콩 카드 104장 — 콩마다 장수가 다르다
// (푸르대콩 20 · 칠리콩 18 · 메주콩 16 · 완두콩 14 · 대두 12 · 동부 10 ·
// 팥 8 · 강낭콩 6)
func bzBuildDeck() []BZBean {
	deck := make([]BZBean, 0, bzDeckSize())
	for _, def := range bzBeanDefs {
		for i := 0; i < def.Count; i++ {
			deck = append(deck, def.ID)
		}
	}
	return deck
}

// bzEndCycle 게임이 끝나는 덱 소진 횟수 — 3인은 2번째, 4~5인은 3번째
func bzEndCycle(players int) int {
	if players <= 3 {
		return 2
	}
	return 3
}

// Start 게임 시작 — 덱을 섞어 각자 손패 5장을 순서대로 나눠 준다.
// 콩밭 2개·금화 0으로 시작하고 첫 차례는 무작위 좌석이다.
func (g *BZGame) Start(rng *rand.Rand) error {
	if !g.CanStart() {
		return fmt.Errorf("%d명 이상 모여야 시작할 수 있습니다", BZMinPlayers)
	}
	n := len(g.Players)
	g.Ready = true
	g.StartedAt = time.Now()
	g.rng = rng

	deck := bzBuildDeck()
	rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })

	for _, p := range g.Players {
		p.Coins = 0
		p.BoughtField = false
		p.Fields = bzNewFields()
		p.Pending = []BZBean{}
		// 시작 손패 5장 — 나눠 준 순서가 그대로 손패 순서다
		p.Hand = append([]BZBean{}, deck[:BZStartHand]...)
		deck = deck[BZStartHand:]
	}
	g.Deck = append([]BZBean{}, deck...)
	g.Discard = []BZBean{}
	g.Flipped = []BZBean{}
	g.Offers = []*BZOffer{}
	g.NextOID = 1
	g.DeckCycle = 0
	g.EndCycle = bzEndCycle(n)
	g.SpentCoins = 0
	g.LastAction = nil
	g.Result = nil
	g.Turns = 0

	g.CurrentSeat = rng.Intn(n)
	g.emit("game_started", g.CurrentSeat, fmt.Sprintf(
		"게임 시작 — 각자 손패 %d장과 콩밭 %d개로 시작합니다. 덱 %d장 · %d번째 소진에서 종료. %s님부터 시작합니다",
		BZStartHand, BZStartFields, len(g.Deck), g.EndCycle, g.Players[g.CurrentSeat].Name))
	g.beginPlantPhase()
	return nil
}

// ==================== 이벤트 큐 ====================

func (g *BZGame) emit(kind string, seat int, msg string) {
	g.events = append(g.events, BZGameEvent{Kind: kind, Seat: seat, Message: msg})
}

// DrainEvents 쌓인 이벤트를 꺼내고 비운다 (허브가 방송)
func (g *BZGame) DrainEvents() []BZGameEvent {
	evs := g.events
	g.events = nil
	return evs
}

func (g *BZGame) setLastAction(seat int, msg string) {
	name := ""
	if seat >= 0 && seat < len(g.Players) {
		name = g.Players[seat].Name
	}
	g.LastAction = &BZLastAction{Seat: seat, Name: name, Message: msg}
}

// bzLive 게임이 진행 중인지 (대기·종료가 아닌 상태)
func (g *BZGame) bzLive() bool {
	return g.Ready && g.Phase != BZPhaseWaiting && g.Phase != BZPhaseGameOver
}

// ==================== 덱 ====================

// drawCard 덱 맨 위 한 장. 덱이 비면 소진 횟수를 올리고 버린 더미를 섞어
// 되돌린다. 종료 소진에 도달하면 (또는 되돌릴 카드도 없으면) ok=false 로
// 돌아오고 호출부가 정산으로 넘긴다.
func (g *BZGame) drawCard() (BZBean, bool) {
	if len(g.Deck) == 0 {
		g.DeckCycle++
		if g.DeckCycle >= g.EndCycle {
			g.emit("deck_out", -1, fmt.Sprintf(
				"덱이 %d번째로 소진돼 게임이 끝납니다", g.DeckCycle))
			return "", false
		}
		if len(g.Discard) == 0 {
			g.emit("deck_out", -1, "덱과 버린 더미가 모두 비어 게임이 끝납니다")
			return "", false
		}
		g.Deck = append([]BZBean{}, g.Discard...)
		g.Discard = []BZBean{}
		g.rng.Shuffle(len(g.Deck), func(i, j int) { g.Deck[i], g.Deck[j] = g.Deck[j], g.Deck[i] })
		g.emit("reshuffle", -1, fmt.Sprintf(
			"덱이 %d번째로 소진돼 버린 더미 %d장을 섞어 되돌립니다", g.DeckCycle, len(g.Deck)))
	}
	card := g.Deck[0]
	g.Deck = g.Deck[1:]
	return card, true
}

// ==================== 콩밭 / 수확 ====================

// bzPlantTarget 이 콩을 심을 밭을 고른다 — 같은 콩이 있는 밭이 먼저고
// (여럿이면 많이 쌓인 밭), 없으면 첫 빈 밭. 둘 다 없으면 ok=false 라
// 밭 하나를 수확해 자리를 만들어야 한다.
func bzPlantTarget(fields []BZField, bean BZBean) (int, bool) {
	best, bestCount := -1, -1
	for i, f := range fields {
		if f.Count > 0 && f.Bean == bean && f.Count > bestCount {
			best, bestCount = i, f.Count
		}
	}
	if best >= 0 {
		return best, true
	}
	for i, f := range fields {
		if f.Count == 0 {
			return i, true
		}
	}
	return -1, false
}

// bzCanHarvest 그 밭을 지금 수확할 수 있는지.
// **카드가 2장 이상인 밭이 있으면 1장짜리 밭은 수확할 수 없다**
// (모든 밭이 1장이면 아무거나 가능).
func bzCanHarvest(fields []BZField, idx int) bool {
	if idx < 0 || idx >= len(fields) || fields[idx].Count == 0 {
		return false
	}
	if fields[idx].Count >= 2 {
		return true
	}
	for i, f := range fields {
		if i != idx && f.Count >= 2 {
			return false
		}
	}
	return true
}

// bzSmallestHarvestable 수확 가능한 밭 중 가장 적게 쌓인 밭 (동수면 금화가
// 많은 쪽 → 그래도 같으면 앞 밭). AFK 자동 배치가 자리를 만들 때 쓴다.
func bzSmallestHarvestable(fields []BZField) int {
	best := -1
	for i := range fields {
		if !bzCanHarvest(fields, i) {
			continue
		}
		if best < 0 {
			best = i
			continue
		}
		if fields[i].Count != fields[best].Count {
			if fields[i].Count < fields[best].Count {
				best = i
			}
			continue
		}
		if bzCoins(fields[i].Bean, fields[i].Count) > bzCoins(fields[best].Bean, fields[best].Count) {
			best = i
		}
	}
	return best
}

// harvestField 밭 하나를 팔아 콩미터대로 금화를 받는다 (제약 검사는 호출부).
// 받은 금화만큼 카드는 게임에서 빠지고(금화 더미) 나머지는 버린 더미로 간다.
func (g *BZGame) harvestField(seat, idx int) int {
	p := g.Players[seat]
	f := p.Fields[idx]
	coins := bzCoins(f.Bean, f.Count)
	p.Coins += coins
	for i := 0; i < f.Count-coins; i++ {
		g.Discard = append(g.Discard, f.Bean)
	}
	p.Fields[idx] = BZField{}
	return coins
}

// Harvest 수확 — 자기 차례가 아니어도 언제든 할 수 있다.
func (g *BZGame) Harvest(seat, field int) error {
	if !g.bzLive() {
		return errors.New("지금은 수확할 수 없습니다")
	}
	if seat < 0 || seat >= len(g.Players) {
		return errors.New("잘못된 좌석입니다")
	}
	p := g.Players[seat]
	if field < 0 || field >= len(p.Fields) {
		return errors.New("잘못된 밭입니다")
	}
	if p.Fields[field].Count == 0 {
		return errors.New("빈 밭은 수확할 수 없습니다")
	}
	if !bzCanHarvest(p.Fields, field) {
		return errors.New("카드가 2장 이상인 밭이 있어 1장짜리 밭은 수확할 수 없습니다")
	}
	bean, count := p.Fields[field].Bean, p.Fields[field].Count
	coins := g.harvestField(seat, field)

	msg := fmt.Sprintf("%s님이 %s %d장을 수확해 금화 %d개를 얻었습니다",
		p.Name, bzName(bean), count, coins)
	if coins == 0 {
		msg = fmt.Sprintf("%s님이 %s %d장을 수확했지만 콩미터에 못 미쳐 금화가 없습니다",
			p.Name, bzName(bean), count)
	}
	g.emit("harvest", seat, msg)
	g.setLastAction(seat, msg)
	return nil
}

// BuyField 세 번째 콩밭 구매 — 금화 3개, 게임 중 1회, 차례가 아니어도 가능.
// 외상 불가(금화가 모자라면 못 산다).
func (g *BZGame) BuyField(seat int) error {
	if !g.bzLive() {
		return errors.New("지금은 콩밭을 살 수 없습니다")
	}
	if seat < 0 || seat >= len(g.Players) {
		return errors.New("잘못된 좌석입니다")
	}
	p := g.Players[seat]
	if len(p.Fields) >= BZMaxFields {
		return errors.New("세 번째 콩밭은 이미 샀습니다")
	}
	if p.Coins < BZThirdFieldCost {
		return fmt.Errorf("금화 %d개가 필요합니다 (외상 불가 · 지금 %d개)",
			BZThirdFieldCost, p.Coins)
	}
	p.Coins -= BZThirdFieldCost
	g.SpentCoins += BZThirdFieldCost
	p.BoughtField = true
	p.Fields = append(p.Fields, BZField{})

	msg := fmt.Sprintf("%s님이 금화 %d개를 내고 세 번째 콩밭을 샀습니다",
		p.Name, BZThirdFieldCost)
	g.emit("buy_field", seat, msg)
	g.setLastAction(seat, msg)
	return nil
}

// ==================== ① 심기 ====================

// beginPlantPhase 차례 시작 — 손패가 비었으면 심을 것이 없어 바로 2단계로.
func (g *BZGame) beginPlantPhase() {
	g.Phase = BZPhasePlant
	g.StateSeq++
	if len(g.Players[g.CurrentSeat].Hand) == 0 {
		g.emit("skip", g.CurrentSeat, fmt.Sprintf(
			"%s님은 손패가 없어 심기를 건너뜁니다", g.Players[g.CurrentSeat].Name))
		g.beginTradePhase()
	}
}

// plantFront 손패 **맨 앞** 카드를 밭에 심는다. 손패에서 빠지는 유일한
// 지점(맨 앞)이다.
func (g *BZGame) plantFront(seat, field int) BZBean {
	p := g.Players[seat]
	bean := p.Hand[0]
	p.Hand = p.Hand[1:]
	g.plantInto(seat, field, bean)
	return bean
}

// plantInto 콩 한 장을 밭에 놓는다 (밭 적합성 검사는 호출부)
func (g *BZGame) plantInto(seat, field int, bean BZBean) {
	f := &g.Players[seat].Fields[field]
	f.Bean = bean
	f.Count++
}

// Plant ① 손패 맨 앞 카드를 심는다. second 가 참이면 두 번째 카드까지 심는다
// (세 번째부터는 못 심는다). 심을 밭은 규칙이 정한다 — 같은 콩 밭이 먼저고
// 없으면 빈 밭이다. 둘 다 없으면 먼저 밭을 수확해 자리를 만들어야 한다.
//
// 두 장을 심는 경우 **둘 다 놓을 자리가 있어야** 진행한다 (반쪽 적용 금지).
func (g *BZGame) Plant(seat int, second bool) error {
	if g.Phase != BZPhasePlant {
		return errors.New("지금은 콩을 심을 수 없습니다")
	}
	if seat != g.CurrentSeat {
		return errors.New("당신의 차례가 아닙니다")
	}
	p := g.Players[seat]
	if len(p.Hand) == 0 {
		return errors.New("심을 카드가 없습니다")
	}

	// 두 장을 심을 수 있는지 먼저 모의로 확인한다 (반쪽 적용 방지)
	sim := append([]BZField{}, p.Fields...)
	idx0, ok := bzPlantTarget(sim, p.Hand[0])
	if !ok {
		return errors.New("빈 밭도 맞는 밭도 없습니다 — 먼저 밭 하나를 수확해 자리를 만드세요")
	}
	sim[idx0] = BZField{Bean: p.Hand[0], Count: sim[idx0].Count + 1}

	plantSecond := second && len(p.Hand) >= 2
	idx1 := -1
	if plantSecond {
		idx1, ok = bzPlantTarget(sim, p.Hand[1])
		if !ok {
			return errors.New("두 번째 카드를 놓을 밭이 없습니다 — 먼저 밭 하나를 수확하세요")
		}
	}

	planted := []string{}
	bean0 := g.plantFront(seat, idx0)
	planted = append(planted, fmt.Sprintf("%s(%d번 밭)", bzName(bean0), idx0+1))
	if plantSecond {
		bean1 := g.plantFront(seat, idx1)
		planted = append(planted, fmt.Sprintf("%s(%d번 밭)", bzName(bean1), idx1+1))
	}

	msg := fmt.Sprintf("%s님이 %s을(를) 심었습니다", p.Name, strings.Join(planted, " · "))
	g.emit("plant", seat, msg)
	g.setLastAction(seat, msg)

	g.beginTradePhase()
	return nil
}

// ==================== ② 2장 뒤집기 + 거래·기부 ====================

// beginTradePhase 덱 위 2장을 공개하고 거래를 연다.
// 공개 도중 덱이 종료 소진에 닿으면 그 자리에서 정산한다.
func (g *BZGame) beginTradePhase() {
	g.Flipped = []BZBean{}
	g.Offers = []*BZOffer{}
	for i := 0; i < BZFlipCount; i++ {
		card, ok := g.drawCard()
		if !ok {
			g.settle("덱 소진")
			return
		}
		g.Flipped = append(g.Flipped, card)
	}
	names := []string{}
	for _, b := range g.Flipped {
		names = append(names, bzName(b))
	}
	g.emit("flip", g.CurrentSeat, fmt.Sprintf("공개 카드: %s", strings.Join(names, " · ")))
	g.Phase = BZPhaseTrade
	g.StateSeq++
}

// bzUniqueIndexes 인덱스 배열이 범위 안에 있고 중복이 없는지
func bzUniqueIndexes(idx []int, n int) error {
	seen := map[int]bool{}
	for _, i := range idx {
		if i < 0 || i >= n {
			return errors.New("잘못된 카드 번호입니다")
		}
		if seen[i] {
			return errors.New("같은 카드를 두 번 지목했습니다")
		}
		seen[i] = true
	}
	return nil
}

// Offer 거래 제안. want 를 비우면 기부다.
//
// **모든 거래에는 차례인 사람이 반드시 낀다** — 남들끼리는 거래하지 못한다.
// 공개 카드는 차례인 사람만 내줄 수 있다 (그의 카드다).
// 요구(wantHand)는 상대 손패의 **자리**로 지목한다. 남의 손패 내용은 보이지
// 않지만 0번은 상대가 다음 차례에 반드시 심어야 하는 맨 앞 카드다.
func (g *BZGame) Offer(seat int, req BZOfferPayload) (string, error) {
	if g.Phase != BZPhaseTrade {
		return "", errors.New("지금은 거래할 수 없습니다")
	}
	if seat < 0 || seat >= len(g.Players) {
		return "", errors.New("잘못된 좌석입니다")
	}
	if req.ToSeat < 0 || req.ToSeat >= len(g.Players) {
		return "", errors.New("잘못된 상대입니다")
	}
	if req.ToSeat == seat {
		return "", errors.New("자기 자신과는 거래할 수 없습니다")
	}
	if seat != g.CurrentSeat && req.ToSeat != g.CurrentSeat {
		return "", errors.New("모든 거래에는 차례인 사람이 끼어야 합니다")
	}
	if len(req.GiveFlipped) > 0 && seat != g.CurrentSeat {
		return "", errors.New("공개 카드는 차례인 사람만 내줄 수 있습니다")
	}
	if len(req.GiveHand)+len(req.GiveFlipped)+len(req.WantHand) == 0 {
		return "", errors.New("주고받을 카드를 골라야 합니다")
	}
	if err := bzUniqueIndexes(req.GiveHand, len(g.Players[seat].Hand)); err != nil {
		return "", err
	}
	if err := bzUniqueIndexes(req.GiveFlipped, len(g.Flipped)); err != nil {
		return "", err
	}
	if err := bzUniqueIndexes(req.WantHand, len(g.Players[req.ToSeat].Hand)); err != nil {
		return "", err
	}
	if len(g.Offers) >= BZMaxOffers {
		return "", errors.New("진행 중인 제안이 너무 많습니다")
	}

	offer := &BZOffer{
		ID:          fmt.Sprintf("of%d", g.NextOID),
		FromSeat:    seat,
		ToSeat:      req.ToSeat,
		GiveHand:    append([]int{}, req.GiveHand...),
		GiveFlipped: append([]int{}, req.GiveFlipped...),
		WantHand:    append([]int{}, req.WantHand...),
	}
	g.NextOID++
	g.Offers = append(g.Offers, offer)

	kind := "거래"
	if len(offer.WantHand) == 0 {
		kind = "기부"
	}
	msg := fmt.Sprintf("%s님이 %s님에게 %s를 제안했습니다 (%d장 주고 %d장 요구)",
		g.Players[seat].Name, g.Players[req.ToSeat].Name, kind,
		len(offer.GiveHand)+len(offer.GiveFlipped), len(offer.WantHand))
	g.emit("offer", seat, msg)
	g.setLastAction(seat, msg)
	return offer.ID, nil
}

// findOffer 제안 조회
func (g *BZGame) findOffer(id string) *BZOffer {
	for _, o := range g.Offers {
		if o.ID == id {
			return o
		}
	}
	return nil
}

// Respond 제안 수락·거절. 받은 사람만 답할 수 있다.
func (g *BZGame) Respond(seat int, offerID string, accept bool) error {
	if g.Phase != BZPhaseTrade {
		return errors.New("지금은 거래할 수 없습니다")
	}
	offer := g.findOffer(offerID)
	if offer == nil {
		return errors.New("이미 끝난 제안입니다")
	}
	if seat != offer.ToSeat {
		return errors.New("당신에게 온 제안이 아닙니다")
	}
	if !accept {
		g.dropOffer(offerID)
		msg := fmt.Sprintf("%s님이 %s님의 제안을 거절했습니다",
			g.Players[seat].Name, g.Players[offer.FromSeat].Name)
		g.emit("decline", seat, msg)
		g.setLastAction(seat, msg)
		return nil
	}
	return g.execTrade(offer)
}

// dropOffer 제안 하나를 지운다
func (g *BZGame) dropOffer(id string) {
	kept := make([]*BZOffer, 0, len(g.Offers))
	for _, o := range g.Offers {
		if o.ID != id {
			kept = append(kept, o)
		}
	}
	g.Offers = kept
}

// bzPickByIndexes 인덱스로 카드를 뽑아내고 남은 슬라이스를 돌려준다.
// **남은 카드의 상대 순서는 그대로다** — 손패 순서 불변의 근거.
func bzPickByIndexes(cards []BZBean, idx []int) (picked, rest []BZBean) {
	take := map[int]bool{}
	for _, i := range idx {
		take[i] = true
	}
	picked = []BZBean{}
	rest = []BZBean{}
	for i, c := range cards {
		if take[i] {
			picked = append(picked, c)
		} else {
			rest = append(rest, c)
		}
	}
	return picked, rest
}

// execTrade 거래 성사. 오간 카드는 **손에 들지 못하고** 받은 사람의
// "심어야 할 카드"(Pending)로 들어간다.
//
// 거래가 한 번 성사되면 손패·공개 카드의 인덱스가 밀리므로 남은 제안은
// 전부 파기한다 (모든 제안에는 차례인 사람이 끼므로 어차피 전부 영향권이다).
func (g *BZGame) execTrade(offer *BZOffer) error {
	from := g.Players[offer.FromSeat]
	to := g.Players[offer.ToSeat]

	// 그 사이 판이 바뀌었을 수 있으니 다시 검사한다
	if err := bzUniqueIndexes(offer.GiveHand, len(from.Hand)); err != nil {
		g.dropOffer(offer.ID)
		return errors.New("제안한 카드가 더 이상 없습니다")
	}
	if err := bzUniqueIndexes(offer.GiveFlipped, len(g.Flipped)); err != nil {
		g.dropOffer(offer.ID)
		return errors.New("공개 카드가 더 이상 없습니다")
	}
	if err := bzUniqueIndexes(offer.WantHand, len(to.Hand)); err != nil {
		g.dropOffer(offer.ID)
		return errors.New("요구한 카드가 더 이상 없습니다")
	}

	giveHand, restFrom := bzPickByIndexes(from.Hand, offer.GiveHand)
	giveFlip, restFlip := bzPickByIndexes(g.Flipped, offer.GiveFlipped)
	wantHand, restTo := bzPickByIndexes(to.Hand, offer.WantHand)

	from.Hand = restFrom
	to.Hand = restTo
	g.Flipped = restFlip

	to.Pending = append(to.Pending, giveHand...)
	to.Pending = append(to.Pending, giveFlip...)
	from.Pending = append(from.Pending, wantHand...)

	given := bzBeanList(append(append([]BZBean{}, giveHand...), giveFlip...))
	got := bzBeanList(wantHand)
	msg := fmt.Sprintf("%s님 → %s님 기부 성사 (%s)", from.Name, to.Name, given)
	if len(wantHand) > 0 {
		msg = fmt.Sprintf("%s님과 %s님이 거래했습니다 (%s ↔ %s)", from.Name, to.Name, given, got)
	}
	g.emit("trade", offer.ToSeat, msg)
	g.setLastAction(offer.ToSeat, msg)

	g.Offers = []*BZOffer{} // 인덱스가 밀렸으므로 남은 제안은 전부 파기
	return nil
}

// bzBeanList 콩 목록의 한글 표기
func bzBeanList(cards []BZBean) string {
	if len(cards) == 0 {
		return "없음"
	}
	names := make([]string, 0, len(cards))
	for _, c := range cards {
		names = append(names, bzName(c))
	}
	return strings.Join(names, "·")
}

// EndPhase 현재 단계 종료 — 2단계 거래 마감이다 (차례인 사람만).
// 아무도 안 가져간 공개 카드는 차례인 사람이 심는다.
func (g *BZGame) EndPhase(seat int) error {
	if g.Phase != BZPhaseTrade {
		return errors.New("지금은 단계를 종료할 수 없습니다")
	}
	if seat != g.CurrentSeat {
		return errors.New("거래는 차례인 사람만 마감할 수 있습니다")
	}
	g.closeTrade()
	return nil
}

// closeTrade 거래 마감 — 남은 공개 카드는 차례인 사람이 받아 심는다.
func (g *BZGame) closeTrade() {
	g.Offers = []*BZOffer{}
	cur := g.Players[g.CurrentSeat]
	if len(g.Flipped) > 0 {
		cur.Pending = append(cur.Pending, g.Flipped...)
		g.emit("trade_end", g.CurrentSeat, fmt.Sprintf(
			"거래 마감 — 남은 공개 카드 %s은(는) %s님이 심습니다",
			bzBeanList(g.Flipped), cur.Name))
		g.Flipped = []BZBean{}
	} else {
		g.emit("trade_end", g.CurrentSeat, "거래를 마감했습니다")
	}
	g.beginReceivedPhase()
}

// ==================== ③ 받은 카드 심기 ====================

// bzPendingTotal 아직 심지 않은 받은 카드가 남았는지
func (g *BZGame) bzPendingTotal() int {
	n := 0
	for _, p := range g.Players {
		n += len(p.Pending)
	}
	return n
}

// beginReceivedPhase 받은 카드가 있으면 3단계를 열고, 없으면 바로 4단계로.
func (g *BZGame) beginReceivedPhase() {
	if g.bzPendingTotal() == 0 {
		g.drawPhase()
		return
	}
	g.Phase = BZPhasePlantReceived
	g.StateSeq++
}

// PlantReceived ③ 받은 카드 한 장을 밭에 심는다. 받은 카드는 손에 못 들고
// 즉시 전부 심어야 하므로, 자기 차례가 아니어도 받은 사람이 직접 심는다.
func (g *BZGame) PlantReceived(seat, cardIndex, field int) error {
	if g.Phase != BZPhasePlantReceived {
		return errors.New("지금은 받은 카드를 심을 수 없습니다")
	}
	if seat < 0 || seat >= len(g.Players) {
		return errors.New("잘못된 좌석입니다")
	}
	p := g.Players[seat]
	if len(p.Pending) == 0 {
		return errors.New("심어야 할 받은 카드가 없습니다")
	}
	if cardIndex < 0 || cardIndex >= len(p.Pending) {
		return errors.New("잘못된 카드 번호입니다")
	}
	if field < 0 || field >= len(p.Fields) {
		return errors.New("잘못된 밭입니다")
	}
	bean := p.Pending[cardIndex]
	f := p.Fields[field]
	if f.Count > 0 && f.Bean != bean {
		return errors.New("같은 종류의 콩만 한 밭에 심을 수 있습니다")
	}

	p.Pending = append(p.Pending[:cardIndex], p.Pending[cardIndex+1:]...)
	g.plantInto(seat, field, bean)

	msg := fmt.Sprintf("%s님이 받은 %s을(를) %d번 밭에 심었습니다",
		p.Name, bzName(bean), field+1)
	g.emit("plant_received", seat, msg)
	g.setLastAction(seat, msg)

	if g.bzPendingTotal() == 0 {
		g.drawPhase()
	}
	return nil
}

// autoPlantOne 받은 카드 한 장을 자동으로 심는다 — 맞는 밭 우선, 없으면
// 가장 적게 쌓인 밭을 수확해 자리를 만든다 (AFK 마감·자동 배치 경로).
func (g *BZGame) autoPlantOne(seat int, bean BZBean) {
	p := g.Players[seat]
	idx, ok := bzPlantTarget(p.Fields, bean)
	if !ok {
		h := bzSmallestHarvestable(p.Fields)
		if h < 0 { // 밭이 전부 비어 있는데 자리가 없을 수는 없다 (방어선)
			return
		}
		hb, hc := p.Fields[h].Bean, p.Fields[h].Count
		coins := g.harvestField(seat, h)
		g.emit("harvest", seat, fmt.Sprintf(
			"%s님이 자리를 만들려고 %s %d장을 수확했습니다 (금화 %d개)",
			p.Name, bzName(hb), hc, coins))
		idx, ok = bzPlantTarget(p.Fields, bean)
		if !ok {
			return
		}
	}
	g.plantInto(seat, idx, bean)
}

// ==================== ④ 카드 3장 뽑기 ====================

// drawPhase 한 장씩 뽑아 손패 **맨 뒤**에 붙인다. 뽑는 도중 덱이 종료 소진에
// 닿으면 그 자리에서 정산한다.
func (g *BZGame) drawPhase() {
	g.Phase = BZPhaseDraw
	p := g.Players[g.CurrentSeat]
	for i := 0; i < BZDrawCount; i++ {
		card, ok := g.drawCard()
		if !ok {
			g.settle("덱 소진")
			return
		}
		p.Hand = append(p.Hand, card) // 맨 뒤로만 붙인다
	}
	g.advanceTurn()
}

// advanceTurn 다음 좌석으로 차례를 넘긴다
func (g *BZGame) advanceTurn() {
	n := len(g.Players)
	if n == 0 {
		return
	}
	g.Turns++
	g.CurrentSeat = (g.CurrentSeat + 1) % n
	if g.Turns >= BZMaxTurns {
		g.settle("차례 상한")
		return
	}
	g.beginPlantPhase()
}

// ==================== AFK 자동 진행 (허브 타이머) ====================

// ForcePlant 심기 마감 — 맨 앞 카드만 심는다 (자리가 없으면 가장 적게 쌓인
// 밭을 수확해 만든다)
func (g *BZGame) ForcePlant() {
	if g.Phase != BZPhasePlant {
		return
	}
	seat := g.CurrentSeat
	p := g.Players[seat]
	if len(p.Hand) == 0 {
		g.beginTradePhase()
		return
	}
	if err := g.Plant(seat, false); err == nil {
		return
	}
	// 자리가 없어 거부된 경우 — 자동으로 수확해 자리를 만들고 심는다
	bean := p.Hand[0]
	p.Hand = p.Hand[1:]
	g.autoPlantOne(seat, bean)
	msg := fmt.Sprintf("%s님이 자동으로 %s을(를) 심었습니다", p.Name, bzName(bean))
	g.emit("plant", seat, msg)
	g.setLastAction(seat, msg)
	g.beginTradePhase()
}

// ForceTradeEnd 거래 마감 — 남은 제안을 파기하고 2단계를 닫는다
func (g *BZGame) ForceTradeEnd() {
	if g.Phase != BZPhaseTrade {
		return
	}
	g.closeTrade()
}

// ForcePlantReceived 받은 카드 심기 마감 — 남은 카드를 전원분 자동 배치한다
// (맞는 밭 우선, 없으면 가장 적게 쌓인 밭을 수확한 뒤 심는다)
func (g *BZGame) ForcePlantReceived() {
	if g.Phase != BZPhasePlantReceived {
		return
	}
	for _, p := range g.Players {
		for len(p.Pending) > 0 {
			bean := p.Pending[0]
			p.Pending = p.Pending[1:]
			g.autoPlantOne(p.Seat, bean)
		}
	}
	g.emit("plant_received", -1, "받은 카드를 자동으로 심었습니다")
	g.drawPhase()
}

// ==================== 최종 정산 ====================

// settle 손패는 치우고 모든 밭을 수확해 정산한다. 금화가 가장 많은 사람이
// 승리하고, 동점이면 **손에 든 카드가 많은 사람**이 이긴다.
//
// 손패는 실제로 비우지 않는다 — 동점 판정과 스냅샷의 handCount 근거로
// 남겨 두고 게임이 끝났으므로 더 쓰이지 않는다.
func (g *BZGame) settle(reason string) {
	if len(g.Players) == 0 { // 방어선 — 좌석 없는 판은 정산할 것이 없다
		g.Phase = BZPhaseGameOver
		g.StateSeq++
		return
	}
	for _, p := range g.Players {
		// 심어야 할 카드가 남아 있으면 밭에 얹은 뒤 함께 판다
		for len(p.Pending) > 0 {
			bean := p.Pending[0]
			p.Pending = p.Pending[1:]
			g.autoPlantOne(p.Seat, bean)
		}
		for i := range p.Fields {
			if p.Fields[i].Count > 0 {
				g.harvestField(p.Seat, i) // 최종 수확은 1장 밭 제약을 받지 않는다
			}
		}
	}
	if len(g.Flipped) > 0 {
		g.Discard = append(g.Discard, g.Flipped...)
		g.Flipped = []BZBean{}
	}
	g.Offers = []*BZOffer{}

	rows := make([]BZResultRow, 0, len(g.Players))
	for _, p := range g.Players {
		rows = append(rows, BZResultRow{Seat: p.Seat, Coins: p.Coins, HandCount: len(p.Hand)})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Coins != rows[j].Coins {
			return rows[i].Coins > rows[j].Coins
		}
		if rows[i].HandCount != rows[j].HandCount {
			return rows[i].HandCount > rows[j].HandCount
		}
		return rows[i].Seat < rows[j].Seat
	})

	bestCoins, bestHand := rows[0].Coins, rows[0].HandCount
	winnerSeats := []int{}
	winnerNames := []string{}
	for _, p := range g.Players {
		if p.Coins == bestCoins && len(p.Hand) == bestHand {
			winnerSeats = append(winnerSeats, p.Seat)
			winnerNames = append(winnerNames, p.Name)
		}
	}

	msg := fmt.Sprintf("정산 완료 — %s님이 금화 %d개로 승리했습니다",
		strings.Join(winnerNames, "·"), bestCoins)
	if len(winnerNames) > 1 {
		msg = fmt.Sprintf("정산 완료 — %s님이 금화 %d개로 공동 승리했습니다",
			strings.Join(winnerNames, "·"), bestCoins)
	}
	if reason != "" {
		msg = fmt.Sprintf("%s (%s)", msg, reason)
	}

	g.Result = &BZResult{
		Rows:        rows,
		WinnerSeats: winnerSeats,
		WinnerNames: winnerNames,
		Message:     msg,
	}
	g.Phase = BZPhaseGameOver
	g.StateSeq++
	g.emit("settle", -1, msg)
}
