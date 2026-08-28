package server

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ==================== 스타트업스 순수 규칙 ====================
//
// 덱 구성·배분·가져오기(덱/시장)·내려놓기·대주주 판정·안티 누적과 지급·
// 최종 정산만 다룬다. 클라이언트·타이머를 모르며, 허브(su_hub.go)가 차례
// 마감(45초)을 걸고 이벤트 큐(DrainEvents)를 방송한다.
//
// ─────────────── 앞면/뒷면 흐름 (su_types.go 상단 그림 참조) ───────────────
//
//	덱(뒷면) ──①덱에서 가져오기──▶ 내 손패(비공개) ──②내려놓기──▶ 시장(앞면)
//	                                                                │
//	                                        ①시장에서 가져오기        │
//	  내 앞 앞면 더미(공개) ◀───────────────────────────────────────┘
//
// 시장에 내려놓은 카드는 시장에 남는다. 내 앞 앞면 더미(faceUp)에 쌓이는 것은
// 오직 시장에서 가져온 카드다. 덱에서 뽑은 카드는 비공개 손패로 간다.
//
// ==================== 안티 규칙 (구현 근거) ====================
//
// 스펙이 명시한 것:
//
//	(가) 덱에서 뽑을 때 그 회사의 대주주라 못 가져오면 자기 돈 1원을 덱 위에
//	     얹고 다시 뽑는다. 돈이 없으면 시장에서 가져와야 한다.
//	(나) 시장 카드를 가져오면 그 위에 쌓인 안티를 전부 받는다.
//
// 스펙이 비워 둔 것 — "시장 카드 위의 안티는 어디서 오는가". 와이어 계약에
// market[].ante 와 (나)의 지급 규칙이 고정돼 있으므로 공급원이 반드시 있어야
// 한다. 원작(Oink Games, Startups)의 규칙 그대로 채웠다:
//
//	(다) 덱에서 카드를 가져간 사람은 시장에 그대로 남겨 둔 카드마다 안티
//	     1원씩 얹는다 (돈이 모자라면 앞쪽부터 낼 수 있는 만큼만 — 덱에서
//	     가져오는 것 자체는 막지 않는다).
//	(라) 덱 위에 쌓인 안티는 덱에서 카드를 성공적으로 가져간 사람이 전부
//	     받는다 — (나)와 같은 원리를 덱에 적용한 것. 단 이번 차례에 자기가
//	     얹은 안티는 되가져가지 못하고 다음 사람 몫으로 덱 위에 남는다
//	     (자기 돈을 그 자리에서 회수하면 (가)의 안티가 무의미해진다).
//
// 덕분에 돈은 판 안에서 보존된다: 전원 돈 + 시장 안티 합 + 덱 안티 = 10 × 인원.
// (정산에서만 은행이 대주주에게 돈을 지급해 총액이 늘어난다.)
//
// ==================== 손패 수급과 유한 종료 보장 ====================
//
// 흐름상 손패는 "덱에서 뽑을 때만" 늘고 "내려놓을 때만" 준다 (시장에서 가져온
// 카드는 앞면 더미로 가지 손패로 오지 않는다). 그래서 손패가 빈 채로 차례를
// 맞는 일이 정상적으로 생긴다 — 이때는 ②를 건너뛰고 차례를 넘긴다.
//
//	덱에서 가져오기 : 손패 +1 → 내려놓기 −1 → 순 0 (시장 카드 +1)
//	시장에서 가져오기: 손패 그대로 → 손패가 있으면 내려놓기 −1 (시장 카드 −1+1=0)
//	                                손패가 비었으면 내려놓기 생략 (시장 카드 −1)
//
// 유한 종료의 근거:
//   - 손패는 0 또는 1이고, 한 번 0이 되면 다시 1로 늘지 않는다 (덱에서
//     가져오면 1이 됐다가 내려놓으며 곧바로 0으로 돌아간다). 즉 "시장에서
//     가져온 뒤 내려놓기까지 하는 차례"는 좌석마다 평생 최대 1번이다.
//   - 손패가 0이 된 뒤로는 시장에서 가져오는 차례가 시장을 반드시 1장 줄이고,
//     시장을 늘리는 것은 덱에서 가져오는 차례뿐이다.
//   - 따라서 (시장 차례 수) ≤ (덱 차례 수) + 인원 이고, 덱 차례 수 ≤ 덱 장수라
//     총 차례는 "덱 장수 × 2 + 인원" 을 넘지 못한다 (4인이면 26×2+4 = 56).
// 덱이 마르면 그 라운드를 마치고(첫 차례 좌석으로 한 바퀴 돌아오면) 정산한다.
// SUMaxTurns 는 전원 무일푼 + 덱 전량 대주주 벽 같은 병리적 교착의 안전망이다.

// NewSUGame 대기 상태의 새 게임
func NewSUGame(id string) *SUGame {
	return &SUGame{
		ID:          id,
		Players:     []*SUPlayer{},
		Phase:       SUPhaseWaiting,
		Deck:        []SUCompany{},
		Removed:     []SUCompany{},
		Market:      []SUMarketCard{},
		CurrentSeat: -1,
		StartSeat:   -1,
	}
}

// suNewFaceUp 회사 6종 키를 모두 가진 앞면 더미 장부 (nil → JSON null 방지)
func suNewFaceUp() map[SUCompany]int {
	m := make(map[SUCompany]int, len(suCompanyDefs))
	for _, def := range suCompanyDefs {
		m[def.ID] = 0
	}
	return m
}

// AddPlayer 대기실 입장. 좌석 번호를 돌려준다.
func (g *SUGame) AddPlayer(name string) (int, error) {
	if g.Phase != SUPhaseWaiting {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= SUMaxPlayers {
		return -1, fmt.Errorf("자리가 없습니다 (최대 %d명)", SUMaxPlayers)
	}
	seat := len(g.Players)
	g.Players = append(g.Players, &SUPlayer{
		Seat:   seat,
		Name:   name,
		Money:  SUStartMoney,
		Hand:   []SUCompany{},
		FaceUp: suNewFaceUp(),
	})
	return seat, nil
}

// RemovePlayer 대기실 퇴장. 좌석을 앞으로 당겨 빈틈을 없앤다.
func (g *SUGame) RemovePlayer(seat int) {
	if g.Phase != SUPhaseWaiting || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i, p := range g.Players {
		p.Seat = i
	}
}

// CanStart 시작 가능 여부 (호스트 명시 시작 — 3인부터)
func (g *SUGame) CanStart() bool {
	return g.Phase == SUPhaseWaiting && len(g.Players) >= SUMinPlayers
}

// suBuildDeck 주식 카드 덱 33장 — 회사마다 총 장수가 다르다
// (긱스 3 · 바우와우 4 · 오션 5 · 슈퍼퓨전 6 · 가가 7 · 더브 8)
func suBuildDeck() []SUCompany {
	deck := make([]SUCompany, 0, suDeckSize())
	for _, def := range suCompanyDefs {
		for i := 0; i < def.Size; i++ {
			deck = append(deck, def.ID)
		}
	}
	return deck
}

// Start 게임 시작 — 덱을 섞어 3장을 빼서 게임에서 제외하고, 각자 주식 카드
// 1장(비공개)과 돈 10원으로 시작한다. 첫 차례는 무작위 좌석이다.
func (g *SUGame) Start(rng *rand.Rand) error {
	if !g.CanStart() {
		return fmt.Errorf("%d명 이상 모여야 시작할 수 있습니다", SUMinPlayers)
	}
	n := len(g.Players)
	g.Ready = true
	g.StartedAt = time.Now()

	deck := suBuildDeck()
	rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })

	// 3장은 아무도 못 보게 게임에서 제외한다 (어떤 스냅샷에도 나가지 않는다)
	g.Removed = append([]SUCompany{}, deck[:SURemovedCards]...)
	deck = deck[SURemovedCards:]

	for _, p := range g.Players {
		p.Money = SUStartMoney
		p.FaceUp = suNewFaceUp()
		p.Hand = append([]SUCompany{}, deck[:SUStartHand]...)
		deck = deck[SUStartHand:]
	}
	g.Deck = append([]SUCompany{}, deck...)
	g.DeckAnte = 0
	g.Market = []SUMarketCard{}
	g.LastAction = nil
	g.Result = nil
	g.Turns = 0

	g.CurrentSeat = rng.Intn(n)
	g.StartSeat = g.CurrentSeat
	g.Phase = SUPhaseTake
	g.StateSeq++
	g.emit("game_started", g.CurrentSeat, fmt.Sprintf(
		"게임 시작 — 각자 주식 카드 1장과 돈 %d원으로 시작합니다. 덱 %d장(3장은 게임에서 제외). %s님부터 시작합니다",
		SUStartMoney, len(g.Deck), g.Players[g.CurrentSeat].Name))
	return nil
}

// ==================== 이벤트 큐 ====================

func (g *SUGame) emit(kind string, seat int, msg string) {
	g.events = append(g.events, SUGameEvent{Kind: kind, Seat: seat, Message: msg})
}

// DrainEvents 쌓인 이벤트를 꺼내고 비운다 (허브가 방송)
func (g *SUGame) DrainEvents() []SUGameEvent {
	evs := g.events
	g.events = nil
	return evs
}

func (g *SUGame) setLastAction(seat int, msg string) {
	name := ""
	if seat >= 0 && seat < len(g.Players) {
		name = g.Players[seat].Name
	}
	g.LastAction = &SULastAction{Seat: seat, Name: name, Message: msg}
}

// ==================== 대주주 판정 ====================

// suMajoritySeat 좌석별 보유 수에서 대주주를 고른다. 가장 많이 가진 한 명,
// 동수면 대주주 없음(-1). 아무도 안 가졌으면 없음(-1).
func suMajoritySeat(counts []int) int {
	best, seat, tie := 0, -1, false
	for i, c := range counts {
		switch {
		case c > best:
			best, seat, tie = c, i, false
		case c == best && c > 0:
			tie = true
		}
	}
	if seat < 0 || best <= 0 || tie {
		return -1
	}
	return seat
}

// faceUpCounts 회사 하나의 좌석별 앞면 보유 수
func (g *SUGame) faceUpCounts(c SUCompany) []int {
	counts := make([]int, len(g.Players))
	for i, p := range g.Players {
		counts[i] = p.FaceUp[c]
	}
	return counts
}

// MajoritySeat 진행 중 대주주 — 앞면 카드만 센다 (동수면 -1).
// 정산에서만 손패를 공개해 함께 센다.
func (g *SUGame) MajoritySeat(c SUCompany) int {
	return suMajoritySeat(g.faceUpCounts(c))
}

// CompanyBoard 회사 현황판 (전원 공개)
func (g *SUGame) CompanyBoard() []SUCompanyInfo {
	board := make([]SUCompanyInfo, 0, len(suCompanyDefs))
	for _, def := range suCompanyDefs {
		board = append(board, SUCompanyInfo{
			ID:           def.ID,
			Name:         def.Name,
			Size:         def.Size,
			MajoritySeat: g.MajoritySeat(def.ID),
		})
	}
	return board
}

// ==================== 가져오기 (①) ====================

// deckPlan 덱 맨 위부터 훑어 내가 가져갈 수 있는 첫 카드를 찾는다.
// cost 는 그 앞에 놓인 "대주주 벽" 장수 = 덱 위에 얹어야 할 안티 원수다.
// ok=false 면 덱에 내가 가져갈 수 있는 카드가 하나도 없다.
func (g *SUGame) deckPlan(seat int) (cost int, ok bool) {
	for i, c := range g.Deck {
		if g.MajoritySeat(c) != seat {
			return i, true
		}
	}
	return 0, false
}

// Take ① 카드를 얻는다. from 은 "deck" 또는 "market:N".
func (g *SUGame) Take(seat int, from string) error {
	if g.Phase != SUPhaseTake {
		return errors.New("지금은 카드를 가져올 수 없습니다")
	}
	if seat != g.CurrentSeat {
		return errors.New("당신의 차례가 아닙니다")
	}
	if seat < 0 || seat >= len(g.Players) {
		return errors.New("잘못된 좌석입니다")
	}

	switch {
	case from == SUTakeDeck:
		if err := g.takeFromDeck(seat); err != nil {
			return err
		}
	case strings.HasPrefix(from, SUTakeMarketPrefix):
		idx, err := strconv.Atoi(strings.TrimPrefix(from, SUTakeMarketPrefix))
		if err != nil {
			return errors.New("잘못된 시장 카드입니다")
		}
		if err := g.takeFromMarket(seat, idx); err != nil {
			return err
		}
	default:
		return errors.New("덱 또는 시장 카드를 골라야 합니다")
	}

	// 낼 카드가 없으면 ②를 건너뛰고 차례를 넘긴다 (시장에서 가져오면 손패가
	// 늘지 않으므로 손패가 빈 채로 차례를 맞는 일은 정상이다)
	if len(g.Players[seat].Hand) == 0 {
		g.emit("skip", seat, fmt.Sprintf("%s님은 낼 카드가 없어 내려놓기를 건너뜁니다",
			g.Players[seat].Name))
		g.advanceTurn()
		return nil
	}
	g.Phase = SUPhasePlay
	g.StateSeq++
	return nil
}

// takeFromDeck 덱 맨 위 가져오기. 대주주 벽에 막히면 자기 돈 1원을 덱 위에
// 얹고 그 카드를 덱 맨 아래로 보낸 뒤 다시 뽑는다. 가져오는 데 성공하면
// 덱 위에 쌓인 안티를 전부 받고, 시장에 남겨 둔 카드마다 안티 1원을 얹는다.
// 뽑은 카드는 비공개 손패로 간다.
func (g *SUGame) takeFromDeck(seat int) error {
	p := g.Players[seat]
	if len(g.Deck) == 0 {
		return errors.New("덱이 비었습니다 — 시장에서 가져오세요")
	}
	cost, ok := g.deckPlan(seat)
	if !ok {
		return errors.New("덱에 남은 카드가 모두 내가 대주주인 회사입니다 — 시장에서 가져오세요")
	}
	if cost > p.Money {
		return errors.New("덱 위에 얹을 안티가 부족합니다 — 시장에서 가져오세요")
	}

	// pot 이 차례가 시작되기 전에 이미 덱 위에 쌓여 있던 판돈.
	// 지금 내가 얹는 안티는 다음 사람 몫으로 덱 위에 남는다
	// (자기가 얹은 돈을 그 자리에서 되가져가면 안티가 무의미해진다).
	pot := g.DeckAnte

	// 대주주 벽 — 막힌 카드마다 안티 1원을 얹고 그 카드를 덱 맨 아래로.
	// 어떤 회사였는지는 이벤트에 담지 않는다 (덱 정보 유출 금지).
	for i := 0; i < cost; i++ {
		blocked := g.Deck[0]
		g.Deck = append(append([]SUCompany{}, g.Deck[1:]...), blocked)
		p.Money--
		g.DeckAnte++
	}
	if cost > 0 {
		g.emit("ante", seat, fmt.Sprintf(
			"%s님이 대주주인 회사가 나와 덱 위에 안티 %d원을 얹었습니다 (덱 안티 %d원)",
			p.Name, cost, g.DeckAnte))
	}

	card := g.Deck[0]
	g.Deck = append([]SUCompany{}, g.Deck[1:]...)
	p.Hand = append(p.Hand, card)

	gained := pot
	p.Money += gained
	g.DeckAnte -= gained

	// 시장에 남겨 둔 카드마다 안티 1원 (돈이 모자라면 앞쪽부터 낼 수 있는 만큼)
	paid := 0
	for i := range g.Market {
		if p.Money <= 0 {
			break
		}
		p.Money--
		g.Market[i].Ante++
		paid++
	}

	msg := fmt.Sprintf("%s님이 덱에서 카드를 가져왔습니다", p.Name)
	if gained > 0 {
		msg += fmt.Sprintf(" (덱 안티 %d원 획득)", gained)
	}
	if paid > 0 {
		msg += fmt.Sprintf(" · 시장 %d장에 안티 1원씩", paid)
	}
	g.emit("take_deck", seat, msg)
	g.setLastAction(seat, msg)
	return nil
}

// takeFromMarket 시장 카드 가져오기 — 그 위에 쌓인 안티를 전부 받고,
// 카드는 내 앞에 앞면으로 쌓인다 (전원 공개, 손패로 오지 않는다).
func (g *SUGame) takeFromMarket(seat, idx int) error {
	p := g.Players[seat]
	if len(g.Market) == 0 {
		return errors.New("시장에 카드가 없습니다")
	}
	if idx < 0 || idx >= len(g.Market) {
		return errors.New("잘못된 시장 카드입니다")
	}

	card := g.Market[idx]
	g.Market = append(g.Market[:idx], g.Market[idx+1:]...)
	p.FaceUp[card.Company]++
	p.Money += card.Ante

	msg := fmt.Sprintf("%s님이 시장의 %s 카드를 가져왔습니다", p.Name, suName(card.Company))
	if card.Ante > 0 {
		msg += fmt.Sprintf(" (안티 %d원 획득)", card.Ante)
	}
	g.emit("take_market", seat, msg)
	g.setLastAction(seat, msg)
	return nil
}

// ==================== 내려놓기 (②) ====================

// Play ② 손패 1장을 골라 시장에 앞면으로 내려놓는다.
// 내려놓은 카드는 시장에 남는다 (내 앞 앞면 더미로 가지 않는다).
func (g *SUGame) Play(seat, index int) error {
	if g.Phase != SUPhasePlay {
		return errors.New("지금은 카드를 내려놓을 수 없습니다")
	}
	if seat != g.CurrentSeat {
		return errors.New("당신의 차례가 아닙니다")
	}
	p := g.Players[seat]
	if index < 0 || index >= len(p.Hand) {
		return errors.New("잘못된 카드입니다")
	}

	card := p.Hand[index]
	p.Hand = append(p.Hand[:index], p.Hand[index+1:]...)
	g.Market = append(g.Market, SUMarketCard{Company: card, Ante: 0})

	msg := fmt.Sprintf("%s님이 %s 카드를 시장에 내려놓았습니다", p.Name, suName(card))
	g.emit("play", seat, msg)
	g.setLastAction(seat, msg)

	g.advanceTurn()
	return nil
}

// advanceTurn 차례를 넘긴다. 덱이 마른 뒤 첫 차례 좌석으로 한 바퀴 돌아오면
// 그 라운드를 마친 것이므로 정산한다.
func (g *SUGame) advanceTurn() {
	n := len(g.Players)
	if n == 0 {
		return
	}
	g.Turns++
	g.CurrentSeat = (g.CurrentSeat + 1) % n

	if len(g.Deck) == 0 && g.CurrentSeat == g.StartSeat {
		g.emit("deck_empty", -1, "덱이 떨어져 이번 라운드를 마치고 정산합니다")
		g.settle("덱 소진")
		return
	}
	if g.Turns >= SUMaxTurns {
		g.settle("차례 상한")
		return
	}
	g.Phase = SUPhaseTake
	g.StateSeq++
}

// ==================== AFK 자동 진행 (허브 타이머) ====================

// ForceTake 가져오기 마감 — 시장 최상단(0번) 또는 덱에서 자동으로 가져온다.
// 손패가 비었으면 덱을 먼저 시도한다 (규칙상 덱에서 가져와야 한다).
func (g *SUGame) ForceTake(rng *rand.Rand) {
	if g.Phase != SUPhaseTake {
		return
	}
	seat := g.CurrentSeat
	if seat < 0 || seat >= len(g.Players) {
		return
	}
	p := g.Players[seat]

	// 시장 최상단을 먼저 (안티를 챙기고 덱 안티 지출을 아낀다), 없으면 덱
	order := []string{SUTakeDeck, SUTakeMarketPrefix + "0"}
	if len(g.Market) > 0 {
		order = []string{SUTakeMarketPrefix + "0", SUTakeDeck}
	}
	for _, from := range order {
		if g.Take(seat, from) == nil {
			return
		}
	}
	// 덱도 시장도 못 쓰는 병리적 상황 — 차례를 넘겨 교착을 끊는다
	g.emit("skip", seat, fmt.Sprintf("%s님이 가져올 수 있는 카드가 없어 차례를 넘깁니다", p.Name))
	g.advanceTurn()
}

// ForcePlay 내려놓기 마감 — 손패 무작위 1장을 시장에 낸다
func (g *SUGame) ForcePlay(rng *rand.Rand) {
	if g.Phase != SUPhasePlay {
		return
	}
	seat := g.CurrentSeat
	if seat < 0 || seat >= len(g.Players) {
		return
	}
	p := g.Players[seat]
	if len(p.Hand) == 0 {
		g.advanceTurn()
		return
	}
	g.Play(seat, rng.Intn(len(p.Hand)))
}

// ForceTurn 차례 마감 — 자동으로 가져오고 손패 무작위 1장을 낸다 (한 차례를
// 통째로 해소한다)
func (g *SUGame) ForceTurn(rng *rand.Rand) {
	g.ForceTake(rng)
	if g.Phase == SUPhasePlay {
		g.ForcePlay(rng)
	}
}

// ==================== 최종 정산 ====================

// settle 손패를 전부 공개해 앞면 더미에 합친 뒤 회사마다 대주주를 정하고,
// 대주주는 다른 사람들의 그 회사 카드 1장당 회사 가치만큼 받는다.
// 대주주가 없으면(동수 포함) 아무도 못 받는다.
// 최종 돈이 가장 많은 사람이 승리, 동점이면 공동 승.
func (g *SUGame) settle(reason string) {
	n := len(g.Players)

	// 손패 공개 — 앞면 더미에 합친다 (이후 faceUp 이 최종 보유 수다)
	for _, p := range g.Players {
		for _, c := range p.Hand {
			p.FaceUp[c]++
		}
		p.Hand = []SUCompany{}
	}

	gains := make([]int, n)
	details := make([][]string, n)
	for _, def := range suCompanyDefs {
		counts := g.faceUpCounts(def.ID)
		maj := suMajoritySeat(counts)
		if maj < 0 {
			continue
		}
		others := 0
		for i, c := range counts {
			if i != maj {
				others += c
			}
		}
		pay := others * def.Size
		gains[maj] += pay
		details[maj] = append(details[maj], fmt.Sprintf(
			"%s 대주주 (내 %d장 · 남 %d장 × %d원 = %d원)",
			def.Name, counts[maj], others, def.Size, pay))
	}

	rows := make([]SUResultRow, 0, n)
	for i, p := range g.Players {
		p.Money += gains[i]
		detail := "대주주인 회사 없음 · 정산 0원"
		if len(details[i]) > 0 {
			detail = strings.Join(details[i], " · ") + fmt.Sprintf(" · 정산 +%d원", gains[i])
		}
		rows = append(rows, SUResultRow{Seat: p.Seat, Money: p.Money, Detail: detail})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Money != rows[j].Money {
			return rows[i].Money > rows[j].Money
		}
		return rows[i].Seat < rows[j].Seat
	})

	best := 0
	for i, p := range g.Players {
		if i == 0 || p.Money > best {
			best = p.Money
		}
	}
	winnerSeats := []int{}
	winnerNames := []string{}
	for _, p := range g.Players {
		if p.Money == best {
			winnerSeats = append(winnerSeats, p.Seat)
			winnerNames = append(winnerNames, p.Name)
		}
	}

	msg := fmt.Sprintf("정산 완료 — %s님이 %d원으로 승리했습니다",
		strings.Join(winnerNames, "·"), best)
	if len(winnerNames) > 1 {
		msg = fmt.Sprintf("정산 완료 — %s님이 %d원으로 공동 승리했습니다",
			strings.Join(winnerNames, "·"), best)
	}
	if reason != "" {
		msg = fmt.Sprintf("%s (%s)", msg, reason)
	}

	g.Result = &SUResult{
		Rows:        rows,
		WinnerSeats: winnerSeats,
		WinnerNames: winnerNames,
		Message:     msg,
	}
	g.Phase = SUPhaseGameOver
	g.StateSeq++
	g.emit("settle", -1, msg)
}
