package server

import (
	"errors"
	"fmt"
	"math/rand"
	"time"
)

// ==================== 쿠 순수 규칙 ====================
//
// 덱·액션 7종·챌린지·차단·카드 제거·교환·탈락만 다룬다. 클라이언트·타이머를
// 모르며, 허브(cp_hub.go)가 창 마감을 걸고 이벤트 큐(DrainEvents)를 방송한다.
//
// 진행 모델: action_declared → (챌린지 창) → (차단 선언 창, 해당 액션만)
// → (차단 챌린지 창) → resolve. 각 창은 생존자 전원 허용 또는 마감으로
// 통과된다(ForcePassWindow). 카드 제거는 당사자가 선택하되 비공개가 1장뿐이면
// 즉시 자동 공개된다.

// cpDeckComposition 5역할 × 3 = 15장
func cpDeckComposition() []CPRole {
	deck := make([]CPRole, 0, len(cpAllRoles)*CPRoleCopies)
	for _, r := range cpAllRoles {
		for i := 0; i < CPRoleCopies; i++ {
			deck = append(deck, r)
		}
	}
	return deck
}

// NewCPGame 대기 상태의 새 게임
func NewCPGame(id string) *CPGame {
	return &CPGame{
		ID:          id,
		Players:     []*CPPlayer{},
		Phase:       CPPhaseWaiting,
		CurrentSeat: -1,
		LoseSeat:    -1,
		WinnerSeat:  -1,
	}
}

// AddPlayer 대기실 입장. 좌석 번호를 돌려준다.
func (g *CPGame) AddPlayer(name string) (int, error) {
	if g.Phase != CPPhaseWaiting {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= CPMaxPlayers {
		return -1, fmt.Errorf("자리가 없습니다 (최대 %d명)", CPMaxPlayers)
	}
	seat := len(g.Players)
	g.Players = append(g.Players, &CPPlayer{Seat: seat, Name: name, Chips: CPStartChips})
	return seat, nil
}

// RemovePlayer 대기실 퇴장. 좌석을 앞으로 당겨 빈틈을 없앤다.
func (g *CPGame) RemovePlayer(seat int) {
	if g.Phase != CPPhaseWaiting || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i, p := range g.Players {
		p.Seat = i
	}
}

// CanStart 시작 가능 여부 (호스트 명시 시작 — 2인부터)
func (g *CPGame) CanStart() bool {
	return g.Phase == CPPhaseWaiting && len(g.Players) >= CPMinPlayers
}

// Start 게임 시작 — 덱을 섞어 2장씩 나누고 선을 무작위로 정한다
func (g *CPGame) Start(rng *rand.Rand) error {
	if !g.CanStart() {
		return fmt.Errorf("%d명 이상 모여야 시작할 수 있습니다", CPMinPlayers)
	}
	g.Ready = true
	g.StartedAt = time.Now()

	deck := cpDeckComposition()
	rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })
	for _, p := range g.Players {
		p.Chips = CPStartChips
		p.Cards = []CPCard{{Role: deck[0]}, {Role: deck[1]}}
		deck = deck[2:]
	}
	g.Deck = deck
	g.CurrentSeat = rng.Intn(len(g.Players))
	g.Phase = CPPhaseAction
	g.StateSeq++
	return nil
}

// ==================== 이벤트 큐 ====================

func (g *CPGame) emit(kind string, seat int, msg string) {
	g.events = append(g.events, CPGameEvent{Kind: kind, Seat: seat, Message: msg})
}

// DrainEvents 쌓인 이벤트를 꺼내고 비운다 (허브가 방송)
func (g *CPGame) DrainEvents() []CPGameEvent {
	evs := g.events
	g.events = nil
	return evs
}

// ==================== 좌석 헬퍼 ====================

func (g *CPGame) aliveCount() int {
	n := 0
	for _, p := range g.Players {
		if p.Alive() {
			n++
		}
	}
	return n
}

// nextAliveSeat seat 다음의 생존 좌석 (시계 방향)
func (g *CPGame) nextAliveSeat(seat int) int {
	n := len(g.Players)
	for i := 1; i <= n; i++ {
		s := (seat + i) % n
		if g.Players[s].Alive() {
			return s
		}
	}
	return seat
}

// respondersExcept except 좌석을 뺀 생존자 집합 — 창이 열리는 순간 확정된다
func (g *CPGame) respondersExcept(except int) map[int]bool {
	responders := map[int]bool{}
	for _, p := range g.Players {
		if p.Alive() && p.Seat != except {
			responders[p.Seat] = true
		}
	}
	return responders
}

// ==================== 액션 선언 ====================

// DeclareAction 차례 액션 선언 — 검증 후 즉시 해결(수입)하거나 창을 연다.
// 칩 10개 이상이면 쿠만 허용된다 (쿠 강제).
func (g *CPGame) DeclareAction(seat int, kind CPActionKind, target int, rng *rand.Rand) error {
	if g.Phase != CPPhaseAction {
		return errors.New("지금은 액션을 선언할 수 없습니다")
	}
	if seat != g.CurrentSeat {
		return errors.New("당신의 차례가 아닙니다")
	}
	actor := g.Players[seat]
	if actor.Chips >= CPForceCoupChips && kind != CPActCoup {
		return fmt.Errorf("칩이 %d개 이상이면 쿠만 할 수 있습니다", CPForceCoupChips)
	}

	needsTarget := kind == CPActCoup || kind == CPActAssassinate || kind == CPActSteal
	if needsTarget {
		if target < 0 || target >= len(g.Players) || target == seat || !g.Players[target].Alive() {
			return errors.New("대상을 올바르게 선택하세요")
		}
	}

	switch kind {
	case CPActIncome:
		actor.Chips++
		g.emit("action", seat, fmt.Sprintf("%s님이 수입으로 1칩을 얻었습니다", actor.Name))
		g.nextTurn()

	case CPActAid:
		g.Pending = &CPPending{Kind: kind, ActorSeat: seat, TargetSeat: -1, BlockerSeat: -1}
		g.emit("action", seat, fmt.Sprintf("%s님이 해외원조로 2칩을 받으려 합니다", actor.Name))
		g.openBlockWindow(rng)

	case CPActCoup:
		if actor.Chips < CPCoupCost {
			return fmt.Errorf("쿠에는 칩 %d개가 필요합니다", CPCoupCost)
		}
		actor.Chips -= CPCoupCost
		g.Pending = &CPPending{Kind: kind, ActorSeat: seat, TargetSeat: target, BlockerSeat: -1}
		g.emit("action", seat, fmt.Sprintf("%s님이 칩 %d개로 %s님에게 쿠를 선언했습니다 — 차단·도전 불가",
			actor.Name, CPCoupCost, g.Players[target].Name))
		g.enterLose(target, cpAfterNextTurn, rng)

	case CPActTax, CPActAssassinate, CPActSteal, CPActExchange:
		if kind == CPActAssassinate {
			if actor.Chips < CPAssassinCost {
				return fmt.Errorf("암살에는 칩 %d개가 필요합니다", CPAssassinCost)
			}
			actor.Chips -= CPAssassinCost
		}
		p := &CPPending{Kind: kind, ActorSeat: seat, TargetSeat: -1,
			ClaimRole: cpActionClaim[kind], BlockerSeat: -1}
		if needsTarget {
			p.TargetSeat = target
		}
		g.Pending = p
		switch kind {
		case CPActTax:
			g.emit("action", seat, fmt.Sprintf("%s님이 세금(공작 주장)으로 3칩을 걷으려 합니다", actor.Name))
		case CPActAssassinate:
			g.emit("action", seat, fmt.Sprintf("%s님이 칩 %d개로 %s님 암살(암살자 주장)을 선언했습니다",
				actor.Name, CPAssassinCost, g.Players[target].Name))
		case CPActSteal:
			g.emit("action", seat, fmt.Sprintf("%s님이 %s님에게 강탈(사령관 주장)을 선언했습니다",
				actor.Name, g.Players[target].Name))
		case CPActExchange:
			g.emit("action", seat, fmt.Sprintf("%s님이 교환(대사 주장)을 선언했습니다", actor.Name))
		}
		g.openChallengeWindow()

	default:
		return errors.New("알 수 없는 액션입니다")
	}
	return nil
}

// ==================== 응답 창 ====================

func (g *CPGame) openChallengeWindow() {
	p := g.Pending
	p.responders = g.respondersExcept(p.ActorSeat)
	p.allowed = map[int]bool{}
	p.Message = fmt.Sprintf("%s님의 %s(%s 주장) — 도전하거나 허용하세요",
		g.Players[p.ActorSeat].Name, cpActionName(p.Kind), cpRoleName(p.ClaimRole))
	g.Phase = CPPhaseChallengeWindow
	g.StateSeq++
}

// openBlockWindow 차단 선언 창. 대상만 차단하는 액션(암살·강탈)에서 대상이
// 이미 탈락했으면 창을 건너뛰고 바로 해결한다.
func (g *CPGame) openBlockWindow(rng *rand.Rand) {
	p := g.Pending
	if (p.Kind == CPActAssassinate || p.Kind == CPActSteal) && !g.Players[p.TargetSeat].Alive() {
		g.resolveAction(rng)
		return
	}
	p.responders = g.respondersExcept(p.ActorSeat)
	p.allowed = map[int]bool{}
	switch p.Kind {
	case CPActAid:
		p.Message = fmt.Sprintf("%s님의 해외원조 — 공작 주장으로 차단하거나 허용하세요",
			g.Players[p.ActorSeat].Name)
	case CPActAssassinate:
		p.Message = fmt.Sprintf("%s님은 백작부인 주장으로 차단할 수 있습니다", g.Players[p.TargetSeat].Name)
	case CPActSteal:
		p.Message = fmt.Sprintf("%s님은 사령관·대사 주장으로 차단할 수 있습니다", g.Players[p.TargetSeat].Name)
	}
	g.Phase = CPPhaseBlockWindow
	g.StateSeq++
}

func (g *CPGame) openBlockChallengeWindow() {
	p := g.Pending
	p.responders = g.respondersExcept(p.BlockerSeat)
	p.allowed = map[int]bool{}
	p.Message = fmt.Sprintf("%s님의 차단(%s 주장) — 도전하거나 허용하세요",
		g.Players[p.BlockerSeat].Name, cpRoleName(p.BlockRole))
	g.Phase = CPPhaseBlockChallengeWindow
	g.StateSeq++
}

func (g *CPGame) inWindow() bool {
	return g.Phase == CPPhaseChallengeWindow || g.Phase == CPPhaseBlockWindow ||
		g.Phase == CPPhaseBlockChallengeWindow
}

// SubmitAllow 창 통과 동의. 응답 대상이 아니거나 이미 지나간 창의 뒤늦은
// 허용은 조용히 무시한다 (창 경쟁의 정상 경로). 전원 허용이면 즉시 통과.
func (g *CPGame) SubmitAllow(seat int, rng *rand.Rand) {
	if !g.inWindow() || g.Pending == nil {
		return
	}
	p := g.Pending
	if !p.responders[seat] || p.allowed[seat] {
		return
	}
	p.allowed[seat] = true
	for s := range p.responders {
		if !p.allowed[s] {
			return
		}
	}
	g.passWindow(rng)
}

// ForcePassWindow 마감 시각 경과 — 전원 허용과 같은 통과 처리 (허브 타이머)
func (g *CPGame) ForcePassWindow(rng *rand.Rand) {
	if g.inWindow() {
		g.passWindow(rng)
	}
}

// passWindow 현재 창의 통과 결말
func (g *CPGame) passWindow(rng *rand.Rand) {
	switch g.Phase {
	case CPPhaseChallengeWindow:
		g.afterClaimAccepted(rng)
	case CPPhaseBlockWindow:
		g.resolveAction(rng) // 아무도 차단하지 않았다
	case CPPhaseBlockChallengeWindow:
		p := g.Pending
		g.emit("block_success", p.BlockerSeat, fmt.Sprintf("%s님의 차단이 통과됐습니다 — %s이 취소됩니다",
			g.Players[p.BlockerSeat].Name, cpActionName(p.Kind)))
		g.nextTurn()
	}
}

// afterClaimAccepted 챌린지 창 통과(또는 도전 실패 뒤 재개) — 차단 가능
// 액션은 차단 창으로, 나머지는 해결로 간다.
func (g *CPGame) afterClaimAccepted(rng *rand.Rand) {
	p := g.Pending
	if p.Kind == CPActAssassinate || p.Kind == CPActSteal {
		g.openBlockWindow(rng)
		return
	}
	g.resolveAction(rng)
}

// ==================== 챌린지 ====================

// SubmitChallenge 역할 주장(액션·차단)에 대한 도전. 주장자가 실제 보유하면
// 공개 후 덱에 넣고 새로 뽑으며 도전자가 카드를 잃고, 미보유면 주장자가
// 카드를 잃고 액션(또는 차단)이 무효가 된다. 뒤늦은 도전은 조용히 무시.
func (g *CPGame) SubmitChallenge(seat int, rng *rand.Rand) {
	p := g.Pending
	if p == nil {
		return
	}
	switch g.Phase {
	case CPPhaseChallengeWindow:
		if !p.responders[seat] {
			return
		}
		claimant := g.Players[p.ActorSeat]
		g.emit("challenge", seat, fmt.Sprintf("%s님이 %s님의 %s 주장에 도전했습니다",
			g.Players[seat].Name, claimant.Name, cpRoleName(p.ClaimRole)))
		if claimant.HasHidden(p.ClaimRole) {
			g.proveClaim(claimant, p.ClaimRole, rng)
			g.emit("challenge_fail", seat, fmt.Sprintf(
				"도전 실패 — %s님이 실제 %s 카드를 공개하고 덱에서 새 카드를 받았습니다. %s님이 카드 1장을 잃습니다",
				claimant.Name, cpRoleName(p.ClaimRole), g.Players[seat].Name))
			g.enterLose(seat, cpAfterProceed, rng)
			return
		}
		g.emit("challenge_success", p.ActorSeat, fmt.Sprintf(
			"도전 성공 — %s님에게 %s 카드가 없었습니다. %s이 취소되고 카드 1장을 잃습니다",
			claimant.Name, cpRoleName(p.ClaimRole), cpActionName(p.Kind)))
		if p.Kind == CPActAssassinate {
			claimant.Chips += CPAssassinCost // 거짓 암살은 취소 — 지불한 비용 반환
		}
		g.enterLose(p.ActorSeat, cpAfterNextTurn, rng)

	case CPPhaseBlockChallengeWindow:
		if !p.responders[seat] {
			return
		}
		blocker := g.Players[p.BlockerSeat]
		g.emit("challenge", seat, fmt.Sprintf("%s님이 %s님의 차단(%s 주장)에 도전했습니다",
			g.Players[seat].Name, blocker.Name, cpRoleName(p.BlockRole)))
		if blocker.HasHidden(p.BlockRole) {
			g.proveClaim(blocker, p.BlockRole, rng)
			g.emit("challenge_fail", seat, fmt.Sprintf(
				"도전 실패 — %s님이 실제 %s 카드를 공개했습니다. 차단이 성립해 %s이 취소되고 %s님이 카드 1장을 잃습니다",
				blocker.Name, cpRoleName(p.BlockRole), cpActionName(p.Kind), g.Players[seat].Name))
			g.enterLose(seat, cpAfterNextTurn, rng)
			return
		}
		g.emit("challenge_success", p.BlockerSeat, fmt.Sprintf(
			"도전 성공 — %s님에게 %s 카드가 없었습니다. 차단이 무효가 되고 카드 1장을 잃습니다",
			blocker.Name, cpRoleName(p.BlockRole)))
		g.enterLose(p.BlockerSeat, cpAfterResolve, rng)
	}
}

// proveClaim 주장 입증 — 해당 카드를 덱에 넣고 섞은 뒤 새로 1장 뽑아
// 비공개 상태를 유지한다 (도전자에게 정체가 드러났으므로 교체)
func (g *CPGame) proveClaim(p *CPPlayer, role CPRole, rng *rand.Rand) {
	for i := range p.Cards {
		c := &p.Cards[i]
		if !c.Revealed && c.Role == role {
			g.Deck = append(g.Deck, role)
			rng.Shuffle(len(g.Deck), func(a, b int) { g.Deck[a], g.Deck[b] = g.Deck[b], g.Deck[a] })
			c.Role = g.Deck[0]
			g.Deck = g.Deck[1:]
			return
		}
	}
}

// ==================== 차단 ====================

// SubmitBlock 차단 선언 — 차단 챌린지 창을 연다. 해외원조는 아무나,
// 암살·강탈은 대상만 차단할 수 있다.
func (g *CPGame) SubmitBlock(seat int, role CPRole) error {
	if g.Phase != CPPhaseBlockWindow || g.Pending == nil {
		return errors.New("지금은 차단할 수 없습니다")
	}
	p := g.Pending
	if !p.responders[seat] {
		return errors.New("차단할 수 없는 좌석입니다")
	}
	if (p.Kind == CPActAssassinate || p.Kind == CPActSteal) && seat != p.TargetSeat {
		return errors.New("대상만 차단할 수 있습니다")
	}
	valid := false
	for _, r := range cpBlockRoles[p.Kind] {
		if r == role {
			valid = true
		}
	}
	if !valid {
		return errors.New("이 액션을 차단할 수 없는 역할입니다")
	}
	p.BlockerSeat = seat
	p.BlockRole = role
	g.emit("block", seat, fmt.Sprintf("%s님이 %s 주장으로 %s을 차단했습니다",
		g.Players[seat].Name, cpRoleName(role), cpActionName(p.Kind)))
	g.openBlockChallengeWindow()
	return nil
}

// ==================== 카드 제거 ====================

// enterLose seat 이 카드 1장을 잃는다. 비공개가 1장뿐이면 즉시 자동 공개하고
// 진행을 이어간다 (선택의 여지가 없다). 2장이면 lose_card 로 당사자 선택.
func (g *CPGame) enterLose(seat int, after cpAfter, rng *rand.Rand) {
	victim := g.Players[seat]
	hidden := victim.HiddenIdx()
	if len(hidden) == 0 {
		g.continueAfter(after, rng)
		return
	}
	if len(hidden) == 1 {
		g.reveal(victim, hidden[0])
		g.continueAfter(after, rng)
		return
	}
	g.LoseSeat = seat
	g.LoseAfter = after
	if g.Pending != nil {
		g.Pending.Message = fmt.Sprintf("%s님이 잃을 카드를 선택하는 중입니다", victim.Name)
	}
	g.Phase = CPPhaseLoseCard
	g.StateSeq++
}

// reveal 카드를 공개로 뒤집는다 — 잃은 카드는 전원에게 보인다
func (g *CPGame) reveal(p *CPPlayer, cardIdx int) {
	p.Cards[cardIdx].Revealed = true
	g.emit("card_lost", p.Seat, fmt.Sprintf("%s님이 [%s] 카드를 잃었습니다",
		p.Name, cpRoleName(p.Cards[cardIdx].Role)))
	if !p.Alive() {
		g.emit("eliminated", p.Seat, fmt.Sprintf("%s님이 카드를 모두 잃고 탈락했습니다", p.Name))
	}
}

// SubmitLose 당사자의 제거 카드 선택 — index 는 비공개 카드 목록(yourCards) 기준
func (g *CPGame) SubmitLose(seat, index int, rng *rand.Rand) error {
	if g.Phase != CPPhaseLoseCard || seat != g.LoseSeat {
		return errors.New("지금은 카드를 제거할 수 없습니다")
	}
	victim := g.Players[seat]
	hidden := victim.HiddenIdx()
	if index < 0 || index >= len(hidden) {
		return errors.New("잘못된 카드 선택입니다")
	}
	g.LoseSeat = -1
	g.reveal(victim, hidden[index])
	g.continueAfter(g.LoseAfter, rng)
	return nil
}

// continueAfter 카드 제거가 끝난 뒤의 진행 — 탈락으로 승부가 났으면 종료
func (g *CPGame) continueAfter(after cpAfter, rng *rand.Rand) {
	if g.aliveCount() <= 1 {
		g.finish()
		return
	}
	switch after {
	case cpAfterProceed:
		g.afterClaimAccepted(rng)
	case cpAfterResolve:
		g.resolveAction(rng)
	default:
		g.nextTurn()
	}
}

// ==================== 해결 ====================

// resolveAction 창을 모두 통과한 액션의 효과 적용
func (g *CPGame) resolveAction(rng *rand.Rand) {
	p := g.Pending
	actor := g.Players[p.ActorSeat]
	switch p.Kind {
	case CPActAid:
		actor.Chips += 2
		g.emit("resolve", p.ActorSeat, fmt.Sprintf("%s님이 해외원조로 2칩을 받았습니다", actor.Name))
		g.nextTurn()

	case CPActTax:
		actor.Chips += 3
		g.emit("resolve", p.ActorSeat, fmt.Sprintf("%s님이 세금으로 3칩을 걷었습니다", actor.Name))
		g.nextTurn()

	case CPActSteal:
		target := g.Players[p.TargetSeat]
		if target.Alive() {
			amt := CPStealMax
			if target.Chips < amt {
				amt = target.Chips
			}
			target.Chips -= amt
			actor.Chips += amt
			g.emit("resolve", p.ActorSeat, fmt.Sprintf("%s님이 %s님에게서 %d칩을 강탈했습니다",
				actor.Name, target.Name, amt))
		}
		g.nextTurn()

	case CPActAssassinate:
		target := g.Players[p.TargetSeat]
		if !target.Alive() {
			g.nextTurn()
			return
		}
		g.emit("resolve", p.ActorSeat, fmt.Sprintf("암살 성공 — %s님이 카드 1장을 잃습니다", target.Name))
		g.enterLose(p.TargetSeat, cpAfterNextTurn, rng)

	case CPActExchange:
		g.ExchangeCards = append(actor.HiddenRoles(), g.Deck[0], g.Deck[1])
		g.Deck = g.Deck[2:]
		p.Message = fmt.Sprintf("%s님이 유지할 카드를 고르는 중입니다", actor.Name)
		g.emit("resolve", p.ActorSeat, fmt.Sprintf("%s님이 덱에서 2장을 뽑아 교환을 시작합니다", actor.Name))
		g.Phase = CPPhaseExchangePick
		g.StateSeq++
	}
}

// SubmitExchangeKeep 교환 — yourExchange(비공개 + 뽑은 2장) 중 유지할
// 카드를 비공개 장수만큼 고른다. 나머지는 덱에 반납하고 섞는다.
func (g *CPGame) SubmitExchangeKeep(seat int, indices []int, rng *rand.Rand) error {
	if g.Phase != CPPhaseExchangePick || g.Pending == nil || seat != g.Pending.ActorSeat {
		return errors.New("지금은 교환 선택을 할 수 없습니다")
	}
	actor := g.Players[seat]
	hidden := actor.HiddenIdx()
	if len(indices) != len(hidden) {
		return fmt.Errorf("유지할 카드를 %d장 선택해야 합니다", len(hidden))
	}
	seen := map[int]bool{}
	for _, idx := range indices {
		if idx < 0 || idx >= len(g.ExchangeCards) || seen[idx] {
			return errors.New("잘못된 카드 선택입니다")
		}
		seen[idx] = true
	}
	for i, idx := range indices {
		actor.Cards[hidden[i]].Role = g.ExchangeCards[idx]
	}
	for idx, role := range g.ExchangeCards {
		if !seen[idx] {
			g.Deck = append(g.Deck, role)
		}
	}
	rng.Shuffle(len(g.Deck), func(a, b int) { g.Deck[a], g.Deck[b] = g.Deck[b], g.Deck[a] })
	g.ExchangeCards = nil
	g.emit("exchange_done", seat, fmt.Sprintf("%s님이 교환을 마쳤습니다", actor.Name))
	g.nextTurn()
	return nil
}

// AutoExchangeKeep 교환 방치 마감 — 무작위 유지 (허브 타이머)
func (g *CPGame) AutoExchangeKeep(rng *rand.Rand) {
	if g.Phase != CPPhaseExchangePick || g.Pending == nil {
		return
	}
	actor := g.Players[g.Pending.ActorSeat]
	keep := len(actor.HiddenIdx())
	g.SubmitExchangeKeep(actor.Seat, rng.Perm(len(g.ExchangeCards))[:keep], rng)
}

// ==================== 턴 전환 / 종료 ====================

func (g *CPGame) nextTurn() {
	g.Pending = nil
	g.LoseSeat = -1
	g.ExchangeCards = nil
	if g.aliveCount() <= 1 {
		g.finish()
		return
	}
	g.CurrentSeat = g.nextAliveSeat(g.CurrentSeat)
	g.Phase = CPPhaseAction
	g.StateSeq++
}

func (g *CPGame) finish() {
	g.Pending = nil
	g.LoseSeat = -1
	g.ExchangeCards = nil
	g.WinnerSeat = -1
	for _, p := range g.Players {
		if p.Alive() {
			g.WinnerSeat = p.Seat
		}
	}
	g.CurrentSeat = -1
	g.Phase = CPPhaseGameOver
}

// AutoCoupTarget 자동 진행(쿠 강제 AFK)용 대상 — 최다 비공개 카드,
// 동수면 최다 칩 (연습봇도 같은 기준을 쓴다)
func (g *CPGame) AutoCoupTarget(seat int) int {
	best := -1
	for _, p := range g.Players {
		if p.Seat == seat || !p.Alive() {
			continue
		}
		if best < 0 {
			best = p.Seat
			continue
		}
		b := g.Players[best]
		if len(p.HiddenIdx()) > len(b.HiddenIdx()) ||
			(len(p.HiddenIdx()) == len(b.HiddenIdx()) && p.Chips > b.Chips) {
			best = p.Seat
		}
	}
	return best
}
