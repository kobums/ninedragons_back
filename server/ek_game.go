package server

import (
	"errors"
	"fmt"
	"math/rand"
	"time"
)

// ==================== 익스플로딩 키튼 순수 규칙 ====================
//
// 덱 구성·차례·카드 효과·아뇨 창·되꽂기·탈락만 다룬다. 클라이언트·타이머를
// 모르며, 허브(ek_hub.go)가 마감을 걸고 이벤트 큐(DrainEvents/DrainPrivates)를
// 방송한다.
//
// 진행 모델:
//
//	turn ──(카드 냄)──> nope_window ──(전원 통과 or 마감)──> 효과 판정
//	                        ↑ 아뇨가 나오면 창을 다시 연다 (StateSeq++)
//	turn ──(뽑기)──> 폭탄이면 defuse_place, 해체 없으면 탈락
//
// 아뇨 겹침은 홀짝으로 판정한다 — NopeCount 가 짝수면 효과 발동, 홀수면 무효.
// 아뇨 카드는 유한(5장)하므로 창은 반드시 닫힌다.
//
// 종료 보장: 폭탄은 인원-1 장이고, 손에 들 수 없어 항상 덱 안에 있다
// (되꽂기로 돌아가거나 탈락과 함께 사라진다). 남은 폭탄 수 = 생존자 수 - 1
// 이라 게임 중에는 덱이 절대 비지 않고, 마지막 탈락이 곧 종료다.

// ekBaseDeck 폭탄·해체를 뺀 기본 덱 46장 (섞기 전 기준)
func ekBaseDeck() []EKCard {
	deck := []EKCard{}
	for _, spec := range ekBaseCounts {
		for i := 0; i < spec.N; i++ {
			deck = append(deck, spec.Card)
		}
	}
	return deck
}

// NewEKGame 대기 상태의 새 게임
func NewEKGame(id string) *EKGame {
	return &EKGame{
		ID:          id,
		Players:     []*EKPlayer{},
		Phase:       EKPhaseWaiting,
		Deck:        []EKCard{},
		Discard:     []EKCard{},
		CurrentSeat: -1,
		WinnerSeat:  -1,
	}
}

// AddPlayer 대기실 입장. 좌석 번호를 돌려준다.
func (g *EKGame) AddPlayer(name string) (int, error) {
	if g.Phase != EKPhaseWaiting {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= EKMaxPlayers {
		return -1, fmt.Errorf("자리가 없습니다 (최대 %d명)", EKMaxPlayers)
	}
	seat := len(g.Players)
	g.Players = append(g.Players, &EKPlayer{Seat: seat, Name: name, Hand: []EKCard{}, Alive: true})
	return seat, nil
}

// RemovePlayer 대기실 퇴장. 좌석을 앞으로 당겨 빈틈을 없앤다.
func (g *EKGame) RemovePlayer(seat int) {
	if g.Phase != EKPhaseWaiting || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i, p := range g.Players {
		p.Seat = i
	}
}

// CanStart 시작 가능 여부 (호스트 명시 시작 — 2인부터)
func (g *EKGame) CanStart() bool {
	return g.Phase == EKPhaseWaiting && len(g.Players) >= EKMinPlayers
}

// Start 게임 시작 — 해체 1장 + 기본 덱 7장을 나눠 주고, 그 뒤 폭탄 n-1
// 장과 남은 해체 6-n 장을 덱에 섞는다.
func (g *EKGame) Start(rng *rand.Rand) error {
	if !g.CanStart() {
		return fmt.Errorf("%d명 이상 모여야 시작할 수 있습니다", EKMinPlayers)
	}
	n := len(g.Players)
	g.Ready = true
	g.StartedAt = time.Now()

	base := ekBaseDeck()
	rng.Shuffle(len(base), func(i, j int) { base[i], base[j] = base[j], base[i] })

	for _, p := range g.Players {
		p.Alive = true
		hand := []EKCard{EKCardDefuse}
		hand = append(hand, base[:EKStartHand]...)
		base = base[EKStartHand:]
		p.Hand = hand
	}

	deck := append([]EKCard{}, base...)
	for i := 0; i < n-1; i++ { // 폭탄 n-1 장 — 마지막 1명이 남으면 끝난다
		deck = append(deck, EKCardBomb)
	}
	for i := 0; i < EKDefuseTotal-n; i++ { // 나눠 주고 남은 해체
		deck = append(deck, EKCardDefuse)
	}
	rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })

	g.Deck = deck
	g.Discard = []EKCard{}
	g.CurrentSeat = rng.Intn(n)
	g.TurnsLeft = 1
	g.Phase = EKPhaseTurn
	g.StateSeq++
	return nil
}

// ==================== 이벤트 큐 ====================

func (g *EKGame) emit(kind string, seat int, msg string) {
	g.events = append(g.events, EKGameEvent{Kind: kind, Seat: seat, Message: msg})
}

// DrainEvents 쌓인 이벤트를 꺼내고 비운다 (허브가 방송)
func (g *EKGame) DrainEvents() []EKGameEvent {
	evs := g.events
	g.events = nil
	return evs
}

// DrainPrivates 쌓인 개인 이벤트(미래 예측)를 꺼내고 비운다.
// 허브는 이걸 해당 좌석 한 명에게만 보낸다.
func (g *EKGame) DrainPrivates() []EKPrivateEvent {
	evs := g.privates
	g.privates = nil
	return evs
}

// setLastAction 스냅샷 lastAction 갱신 (비밀 정보를 담지 않는다)
func (g *EKGame) setLastAction(seat int, msg string) {
	name := ""
	if seat >= 0 && seat < len(g.Players) {
		name = g.Players[seat].Name
	}
	g.LastAction = &EKLastActionView{Seat: seat, Name: name, Message: msg}
}

// ==================== 좌석 헬퍼 ====================

func (g *EKGame) aliveCount() int {
	n := 0
	for _, p := range g.Players {
		if p.Alive {
			n++
		}
	}
	return n
}

// nextAliveSeat seat 다음의 생존 좌석 (시계 방향)
func (g *EKGame) nextAliveSeat(seat int) int {
	n := len(g.Players)
	if n == 0 {
		return -1
	}
	for i := 1; i <= n; i++ {
		s := (seat + i) % n
		if g.Players[s].Alive {
			return s
		}
	}
	return seat
}

// seatValid 살아 있는 좌석인지
func (g *EKGame) seatAlive(seat int) bool {
	return seat >= 0 && seat < len(g.Players) && g.Players[seat].Alive
}

// BombsLeft 덱에 남은 폭탄 수. 폭탄은 손에 들 수 없어 항상 덱 안에 있고,
// 그 수는 생존자 수 - 1 로 공개 계산된다 (봇도 같은 식을 쓴다).
func (g *EKGame) BombsLeft() int {
	n := g.aliveCount() - 1
	if n < 0 {
		return 0
	}
	return n
}

// discardTop 버린 더미 맨 위 ("" 비어 있음)
func (g *EKGame) discardTop() string {
	if len(g.Discard) == 0 {
		return ""
	}
	return string(g.Discard[len(g.Discard)-1])
}

// removeAt 손패에서 인덱스 한 장을 뽑아낸다
func (p *EKPlayer) removeAt(i int) EKCard {
	card := p.Hand[i]
	p.Hand = append(p.Hand[:i], p.Hand[i+1:]...)
	return card
}

// ==================== 차례 진행 ====================

// endTurn 차례 하나를 소모한다. 남은 차례(공격 누적)가 있으면 같은 사람이
// 이어서, 없으면 다음 생존자에게 1차례를 넘긴다.
func (g *EKGame) endTurn() {
	if g.Phase == EKPhaseGameOver {
		return
	}
	g.TurnsLeft--
	if g.TurnsLeft <= 0 {
		g.CurrentSeat = g.nextAliveSeat(g.CurrentSeat)
		g.TurnsLeft = 1
	}
	g.Phase = EKPhaseTurn
	g.StateSeq++
}

// attackTurn 공격 — 내 차례를 전부 끝내고 다음 사람에게 넘기되, 내가 남긴
// 차례에 2를 더해 누적한다. (남은 1차례 → 다음 2차례, 남은 2차례 → 3차례)
func (g *EKGame) attackTurn() {
	if g.Phase == EKPhaseGameOver {
		return
	}
	next := (g.TurnsLeft - 1) + 2
	g.CurrentSeat = g.nextAliveSeat(g.CurrentSeat)
	g.TurnsLeft = next
	g.Phase = EKPhaseTurn
	g.StateSeq++
}

// backToTurn 효과 처리를 마치고 차례로 복귀 (차례는 소모하지 않는다)
func (g *EKGame) backToTurn() {
	if g.Phase == EKPhaseGameOver {
		return
	}
	g.Phase = EKPhaseTurn
	g.StateSeq++
}

// finish 최후 1인 확정
func (g *EKGame) finish() {
	g.Phase = EKPhaseGameOver
	g.Pending = nil
	g.WinnerSeat = -1
	for _, p := range g.Players {
		if p.Alive {
			g.WinnerSeat = p.Seat
			break
		}
	}
	g.StateSeq++
}

// ==================== 카드 내기 / 아뇨 창 ====================

// Play 자기 차례에 기능 카드 한 장을 낸다 → 아뇨 창이 열린다.
func (g *EKGame) Play(seat, index, targetSeat int, rng *rand.Rand) error {
	if g.Phase != EKPhaseTurn {
		return errors.New("지금은 카드를 낼 수 없습니다")
	}
	if seat != g.CurrentSeat || !g.seatAlive(seat) {
		return errors.New("자기 차례가 아닙니다")
	}
	p := g.Players[seat]
	if index < 0 || index >= len(p.Hand) {
		return errors.New("없는 카드입니다")
	}
	card := p.Hand[index]

	switch {
	case card == EKCardBomb:
		return errors.New("폭탄 고양이는 낼 수 없습니다")
	case card == EKCardDefuse:
		return errors.New("해체는 폭탄을 뽑았을 때 자동으로 쓰입니다")
	case card == EKCardNope:
		return errors.New("아뇨는 다른 사람이 카드를 냈을 때만 낼 수 있습니다")
	case ekIsCat(card):
		return errors.New("고양이 카드는 같은 종류 2장을 함께 내야 합니다")
	}

	target := -1
	if card == EKCardFavor {
		if !g.seatAlive(targetSeat) || targetSeat == seat {
			return errors.New("호의할 상대를 골라주세요")
		}
		target = targetSeat
	}

	p.removeAt(index)
	g.Discard = append(g.Discard, card)

	msg := fmt.Sprintf("%s님이 %s 카드를 냈습니다", p.Name, ekCardName(card))
	if target >= 0 {
		msg = fmt.Sprintf("%s님이 %s님에게 %s 카드를 냈습니다",
			p.Name, g.Players[target].Name, ekCardName(card))
	}
	g.setLastAction(seat, msg)
	g.emit("play", seat, msg)
	g.openNopeWindow(string(card), seat, target, rng)
	return nil
}

// PlayPair 같은 종류 고양이 2장 → 대상 손패에서 무작위 1장 훔치기 (아뇨 대상)
func (g *EKGame) PlayPair(seat int, indexes []int, targetSeat int, rng *rand.Rand) error {
	if g.Phase != EKPhaseTurn {
		return errors.New("지금은 카드를 낼 수 없습니다")
	}
	if seat != g.CurrentSeat || !g.seatAlive(seat) {
		return errors.New("자기 차례가 아닙니다")
	}
	p := g.Players[seat]
	if len(indexes) != EKPairSize {
		return errors.New("고양이 카드 2장을 골라주세요")
	}
	a, b := indexes[0], indexes[1]
	if a == b || a < 0 || b < 0 || a >= len(p.Hand) || b >= len(p.Hand) {
		return errors.New("없는 카드입니다")
	}
	if p.Hand[a] != p.Hand[b] || !ekIsCat(p.Hand[a]) {
		return errors.New("같은 종류의 고양이 카드 2장이어야 합니다")
	}
	if !g.seatAlive(targetSeat) || targetSeat == seat {
		return errors.New("훔칠 상대를 골라주세요")
	}

	card := p.Hand[a]
	hi, lo := a, b
	if lo > hi {
		hi, lo = lo, hi
	}
	p.removeAt(hi) // 큰 인덱스부터 빼야 앞 인덱스가 밀리지 않는다
	p.removeAt(lo)
	g.Discard = append(g.Discard, card, card)

	msg := fmt.Sprintf("%s님이 %s 2장으로 %s님의 카드를 노립니다",
		p.Name, ekCardName(card), g.Players[targetSeat].Name)
	g.setLastAction(seat, msg)
	g.emit("play_pair", seat, msg)
	g.openNopeWindow(EKPendKindPair, seat, targetSeat, rng)
	return nil
}

// openNopeWindow 아뇨 창을 연다. 응답자가 없으면(전원 탈락 등) 즉시 판정.
func (g *EKGame) openNopeWindow(kind string, bySeat, targetSeat int, rng *rand.Rand) {
	g.Pending = &EKPending{
		Kind:       kind,
		BySeat:     bySeat,
		TargetSeat: targetSeat,
		LastSeat:   bySeat,
		passed:     map[int]bool{},
	}
	g.Phase = EKPhaseNopeWindow
	g.StateSeq++
	if len(g.nopeResponders()) == 0 {
		g.resolvePending(rng)
	}
}

// nopeResponders 현재 창에서 아뇨를 낼 수 있는 좌석 — 방금 카드를 낸
// 좌석(LastSeat)만 빠진다. 아뇨 위의 아뇨는 원래 시전자도 낼 수 있다.
func (g *EKGame) nopeResponders() []int {
	seats := []int{}
	if g.Pending == nil {
		return seats
	}
	for _, p := range g.Players {
		if p.Alive && p.Seat != g.Pending.LastSeat {
			seats = append(seats, p.Seat)
		}
	}
	return seats
}

// Nope 창에 아뇨를 겹친다 — 창은 처음부터 다시 열린다 (StateSeq++).
// 뒤늦은(이미 지나간 창) 아뇨는 조용히 무시된다.
func (g *EKGame) Nope(seat int) error {
	if g.Phase != EKPhaseNopeWindow || g.Pending == nil {
		return nil
	}
	if !g.seatAlive(seat) || seat == g.Pending.LastSeat {
		return nil
	}
	p := g.Players[seat]
	idx := p.HasCard(EKCardNope)
	if idx < 0 {
		return errors.New("아뇨 카드가 없습니다")
	}
	p.removeAt(idx)
	g.Discard = append(g.Discard, EKCardNope)

	g.Pending.NopeCount++
	g.Pending.LastSeat = seat
	g.Pending.passed = map[int]bool{}

	verdict := "무효"
	if g.Pending.NopeCount%2 == 0 {
		verdict = "유효"
	}
	msg := fmt.Sprintf("%s님이 아뇨! (%d겹 — 지금은 %s)", p.Name, g.Pending.NopeCount, verdict)
	g.setLastAction(seat, msg)
	g.emit("nope", seat, msg)

	g.StateSeq++ // 창 재개방 — 허브가 마감을 새로 건다
	return nil
}

// Pass 아뇨 창 통과 동의. 응답자 전원이 통과하면 즉시 판정한다.
func (g *EKGame) Pass(seat int, rng *rand.Rand) {
	if g.Phase != EKPhaseNopeWindow || g.Pending == nil {
		return
	}
	if !g.seatAlive(seat) || seat == g.Pending.LastSeat {
		return
	}
	g.Pending.passed[seat] = true
	for _, s := range g.nopeResponders() {
		if !g.Pending.passed[s] {
			return
		}
	}
	g.resolvePending(rng)
}

// ForcePassWindow 마감 경과 — 전원 통과와 같은 판정 (허브가 호출)
func (g *EKGame) ForcePassWindow(rng *rand.Rand) {
	if g.Phase != EKPhaseNopeWindow || g.Pending == nil {
		return
	}
	g.resolvePending(rng)
}

// resolvePending 아뇨 홀짝 판정 후 효과를 발동하거나 버린다
func (g *EKGame) resolvePending(rng *rand.Rand) {
	p := g.Pending
	g.Pending = nil
	if p == nil {
		return
	}
	if p.NopeCount%2 == 1 { // 홀수 겹 — 무효
		msg := fmt.Sprintf("%s님의 %s가 아뇨 %d겹으로 막혔습니다",
			g.Players[p.BySeat].Name, ekPendingName(p.Kind), p.NopeCount)
		g.setLastAction(p.BySeat, msg)
		g.emit("noped", p.BySeat, msg)
		g.backToTurn()
		return
	}
	if p.NopeCount > 0 {
		g.emit("nope_undone", p.BySeat, fmt.Sprintf("아뇨가 %d겹으로 상쇄돼 %s가 발동합니다",
			p.NopeCount, ekPendingName(p.Kind)))
	}
	g.applyEffect(p, rng)
}

// applyEffect 아뇨를 통과한 카드의 효과를 발동한다
func (g *EKGame) applyEffect(p *EKPending, rng *rand.Rand) {
	actor := g.Players[p.BySeat]

	switch p.Kind {
	case string(EKCardSkip):
		g.emit("skip", p.BySeat, fmt.Sprintf("%s님이 뽑지 않고 차례를 넘겼습니다", actor.Name))
		g.endTurn()

	case string(EKCardAttack):
		g.attackTurn()
		g.emit("attack", p.BySeat, fmt.Sprintf("%s님의 공격 — %s님이 %d차례를 연달아 갖습니다",
			actor.Name, g.Players[g.CurrentSeat].Name, g.TurnsLeft))

	case string(EKCardShuffle):
		rng.Shuffle(len(g.Deck), func(i, j int) { g.Deck[i], g.Deck[j] = g.Deck[j], g.Deck[i] })
		g.emit("shuffle", p.BySeat, fmt.Sprintf("%s님이 덱을 섞었습니다", actor.Name))
		g.backToTurn()

	case string(EKCardFuture):
		n := EKFutureCount
		if n > len(g.Deck) {
			n = len(g.Deck)
		}
		g.privates = append(g.privates, EKPrivateEvent{
			Seat:  p.BySeat,
			Cards: append([]EKCard{}, g.Deck[:n]...),
		})
		g.emit("future", p.BySeat, fmt.Sprintf("%s님이 덱 맨 위 %d장을 들여다봤습니다", actor.Name, n))
		g.backToTurn()

	case string(EKCardFavor):
		if !g.seatAlive(p.TargetSeat) {
			g.emit("favor_empty", p.BySeat, "호의할 상대가 없어 무산됐습니다")
			g.backToTurn()
			return
		}
		if len(g.Players[p.TargetSeat].Hand) == 0 {
			g.emit("favor_empty", p.BySeat, fmt.Sprintf("%s님이 줄 카드가 없어 호의이 무산됐습니다",
				g.Players[p.TargetSeat].Name))
			g.backToTurn()
			return
		}
		g.Pending = &EKPending{Kind: string(EKCardFavor), BySeat: p.BySeat,
			TargetSeat: p.TargetSeat, LastSeat: -1}
		g.Phase = EKPhaseFavorWait
		g.StateSeq++
		g.emit("favor_wait", p.TargetSeat, fmt.Sprintf("%s님이 %s님에게 줄 카드를 고릅니다",
			g.Players[p.TargetSeat].Name, actor.Name))

	case EKPendKindPair:
		if !g.seatAlive(p.TargetSeat) {
			g.emit("steal_empty", p.BySeat, "훔칠 상대가 없어 무산됐습니다")
			g.backToTurn()
			return
		}
		target := g.Players[p.TargetSeat]
		if len(target.Hand) == 0 {
			g.emit("steal_empty", p.BySeat, fmt.Sprintf("%s님의 손이 비어 훔치지 못했습니다", target.Name))
			g.backToTurn()
			return
		}
		card := target.removeAt(rng.Intn(len(target.Hand)))
		actor.Hand = append(actor.Hand, card)
		// 훔친 카드 종류는 두 사람만 안다 — 이벤트에 싣지 않는다
		msg := fmt.Sprintf("%s님이 %s님의 카드 1장을 가져갔습니다", actor.Name, target.Name)
		g.setLastAction(p.BySeat, msg)
		g.emit("steal", p.BySeat, msg)
		g.backToTurn()

	default:
		g.backToTurn()
	}
}

// ==================== 호의 (favor_wait) ====================

// Give 호의 대상이 카드 1장을 골라 시전자에게 건넨다.
// 카드 종류는 두 사람만 안다 — 이벤트에 싣지 않는다.
func (g *EKGame) Give(seat, index int) error {
	if g.Phase != EKPhaseFavorWait || g.Pending == nil {
		return errors.New("지금은 카드를 건넬 수 없습니다")
	}
	if seat != g.Pending.TargetSeat {
		return errors.New("호의받은 사람만 카드를 건넬 수 있습니다")
	}
	giver := g.Players[seat]
	if len(giver.Hand) == 0 {
		g.Pending = nil
		g.backToTurn()
		return nil
	}
	if index < 0 || index >= len(giver.Hand) {
		return errors.New("없는 카드입니다")
	}
	receiver := g.Players[g.Pending.BySeat]
	card := giver.removeAt(index)
	receiver.Hand = append(receiver.Hand, card)

	msg := fmt.Sprintf("%s님이 %s님에게 카드 1장을 건넸습니다", giver.Name, receiver.Name)
	g.setLastAction(seat, msg)
	g.emit("favor_give", seat, msg)

	g.Pending = nil
	g.backToTurn()
	return nil
}

// AutoGive 호의 방치 — 무작위 카드를 건넨다 (허브 마감 경로)
func (g *EKGame) AutoGive(rng *rand.Rand) {
	if g.Phase != EKPhaseFavorWait || g.Pending == nil {
		return
	}
	seat := g.Pending.TargetSeat
	n := len(g.Players[seat].Hand)
	if n == 0 {
		g.Pending = nil
		g.backToTurn()
		return
	}
	g.emit("afk", seat, fmt.Sprintf("%s님이 고르지 않아 자동으로 무작위 카드를 건넵니다",
		g.Players[seat].Name))
	g.Give(seat, rng.Intn(n))
}

// ==================== 뽑기 / 폭탄 / 되꽂기 / 탈락 ====================

// Draw 덱에서 1장 뽑아 차례를 끝낸다. 폭탄이면 해체로 막거나 탈락한다.
func (g *EKGame) Draw(seat int, rng *rand.Rand) error {
	if g.Phase != EKPhaseTurn {
		return errors.New("지금은 뽑을 수 없습니다")
	}
	if seat != g.CurrentSeat || !g.seatAlive(seat) {
		return errors.New("자기 차례가 아닙니다")
	}
	p := g.Players[seat]
	if len(g.Deck) == 0 { // 규칙상 오지 않는 경로 — 교착 방지용 방어
		g.emit("deck_empty", seat, "덱이 비어 차례를 넘깁니다")
		g.endTurn()
		return nil
	}

	card := g.Deck[0]
	g.Deck = g.Deck[1:]

	if card != EKCardBomb {
		p.Hand = append(p.Hand, card)
		msg := fmt.Sprintf("%s님이 카드를 1장 뽑고 차례를 마쳤습니다", p.Name)
		g.setLastAction(seat, msg)
		g.emit("draw", seat, msg)
		g.endTurn()
		return nil
	}

	// 폭탄
	if idx := p.HasCard(EKCardDefuse); idx >= 0 {
		p.removeAt(idx)
		g.Discard = append(g.Discard, EKCardDefuse)
		msg := fmt.Sprintf("%s님이 폭탄 고양이를 해체로 막았습니다!", p.Name)
		g.setLastAction(seat, msg)
		g.emit("defuse", seat, msg)

		g.Pending = &EKPending{Kind: string(EKCardDefuse), BySeat: seat, TargetSeat: -1, LastSeat: -1}
		g.Phase = EKPhaseDefusePlace
		g.StateSeq++
		return nil
	}

	g.eliminate(seat)
	return nil
}

// AutoDraw 차례 방치 — 자동으로 1장 뽑는다 (허브 마감 경로)
func (g *EKGame) AutoDraw(rng *rand.Rand) {
	if g.Phase != EKPhaseTurn || !g.seatAlive(g.CurrentSeat) {
		return
	}
	seat := g.CurrentSeat
	g.emit("afk", seat, fmt.Sprintf("%s님이 오래 응답하지 않아 자동으로 1장 뽑습니다",
		g.Players[seat].Name))
	g.Draw(seat, rng)
}

// DefusePlace 막아 낸 폭탄을 덱 position 위치에 비공개로 되꽂는다.
// 0=맨 위 … len(deck)=맨 아래. 위치는 이벤트·스냅샷 어디에도 싣지 않는다.
func (g *EKGame) DefusePlace(seat, position int) error {
	if g.Phase != EKPhaseDefusePlace || g.Pending == nil {
		return errors.New("지금은 폭탄을 되꽂을 수 없습니다")
	}
	if seat != g.Pending.BySeat {
		return errors.New("폭탄을 막은 사람만 위치를 정할 수 있습니다")
	}
	pos := position
	if pos < 0 {
		pos = 0
	}
	if pos > len(g.Deck) {
		pos = len(g.Deck)
	}

	deck := make([]EKCard, 0, len(g.Deck)+1)
	deck = append(deck, g.Deck[:pos]...)
	deck = append(deck, EKCardBomb)
	deck = append(deck, g.Deck[pos:]...)
	g.Deck = deck

	g.Pending = nil
	msg := fmt.Sprintf("%s님이 폭탄을 덱 어딘가에 되꽂았습니다", g.Players[seat].Name)
	g.setLastAction(seat, msg)
	g.emit("defuse_place", seat, msg)
	g.endTurn() // 폭탄을 뽑았으므로 차례는 끝난다
	return nil
}

// AutoDefusePlace 되꽂기 방치 — 무작위 위치 (허브 마감 경로)
func (g *EKGame) AutoDefusePlace(rng *rand.Rand) {
	if g.Phase != EKPhaseDefusePlace || g.Pending == nil {
		return
	}
	seat := g.Pending.BySeat
	g.emit("afk", seat, fmt.Sprintf("%s님이 위치를 정하지 않아 자동으로 무작위 위치에 되꽂습니다",
		g.Players[seat].Name))
	g.DefusePlace(seat, rng.Intn(len(g.Deck)+1))
}

// eliminate 탈락 처리 — 방을 나가지 않고 관전으로 전환된다 (Alive=false 유지).
// 손패와 폭탄은 버린 더미로 가고, 그 폭탄은 덱에서 영구히 사라진다.
func (g *EKGame) eliminate(seat int) {
	p := g.Players[seat]
	p.Alive = false
	g.Discard = append(g.Discard, p.Hand...)
	g.Discard = append(g.Discard, EKCardBomb) // 맨 위는 폭탄 — 무슨 일이 있었는지 보인다
	p.Hand = []EKCard{}

	msg := fmt.Sprintf("%s님이 폭탄 고양이에 당해 탈락했습니다 (관전 전환)", p.Name)
	g.setLastAction(seat, msg)
	g.emit("eliminated", seat, msg)

	if g.aliveCount() <= 1 {
		g.finish()
		return
	}
	// 탈락자가 남긴 차례는 사라진다 — 다음 생존자가 1차례부터 시작
	g.CurrentSeat = g.nextAliveSeat(seat)
	g.TurnsLeft = 1
	g.Phase = EKPhaseTurn
	g.StateSeq++
}
