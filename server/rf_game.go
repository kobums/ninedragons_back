package server

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// ==================== 리포메이션 순수 규칙 ====================
//
// 덱·행동 10종·도전·차단·카드 제거·교환·개종·횡령·탈락·진영 승리만 다룬다.
// 클라이언트·타이머를 모르며, 허브(rf_hub.go)가 창 마감을 걸고 이벤트 큐
// (DrainEvents)를 방송한다.
//
// 진행 모델: action_declared → (도전 창) → (차단 선언 창, 해당 액션만)
// → (차단 도전 창 = 도전 창 재사용) → resolve. 각 창은 생존자 전원 통과
// 또는 마감으로 닫힌다(ForcePassWindow). 카드 제거는 당사자가 선택하되
// 비공개가 1장뿐이면 즉시 자동 공개된다.
//
// 개종(rf_convert/rf_convert_other)은 역할 주장이 아니라 즉시 발동한다.
// 횡령(rf_embezzle)은 "나는 공작이 아니다"라는 역주장이라 도전 판정이
// 뒤집혀 있다 — 공작이 있으면 도전자 승, 없으면 도전자가 카드를 잃는다.

// rfRoleCopies 인원별 역할당 카드 수 — 7인 이상은 4장으로 늘린다
func rfRoleCopies(playerCount int) int {
	if playerCount > RFSmallTableMax {
		return RFRoleCopiesLarge
	}
	return RFRoleCopiesSmall
}

// rfDeckComposition 5역할 × 인원별 장수
func rfDeckComposition(playerCount int) []RFRole {
	copies := rfRoleCopies(playerCount)
	deck := make([]RFRole, 0, len(rfAllRoles)*copies)
	for _, r := range rfAllRoles {
		for i := 0; i < copies; i++ {
			deck = append(deck, r)
		}
	}
	return deck
}

// NewRFGame 대기 상태의 새 게임
func NewRFGame(id string) *RFGame {
	return &RFGame{
		ID:          id,
		Players:     []*RFPlayer{},
		Phase:       RFPhaseWaiting,
		CurrentSeat: -1,
		LoseSeat:    -1,
	}
}

// AddPlayer 대기실 입장. 좌석 번호를 돌려준다.
func (g *RFGame) AddPlayer(name string) (int, error) {
	if g.Phase != RFPhaseWaiting {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= RFMaxPlayers {
		return -1, fmt.Errorf("자리가 없습니다 (최대 %d명)", RFMaxPlayers)
	}
	seat := len(g.Players)
	g.Players = append(g.Players, &RFPlayer{Seat: seat, Name: name, Chips: RFStartChips})
	return seat, nil
}

// RemovePlayer 대기실 퇴장. 좌석을 앞으로 당겨 빈틈을 없앤다.
func (g *RFGame) RemovePlayer(seat int) {
	if g.Phase != RFPhaseWaiting || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i, p := range g.Players {
		p.Seat = i
	}
}

// CanStart 시작 가능 여부 (호스트 명시 시작 — 2인부터)
func (g *RFGame) CanStart() bool {
	return g.Phase == RFPhaseWaiting && len(g.Players) >= RFMinPlayers
}

// Start 게임 시작 — 덱을 섞어 2장씩 나누고, 진영을 절반씩 무작위 배정하고,
// 선을 무작위로 정한다. 국고는 0에서 출발한다.
func (g *RFGame) Start(rng *rand.Rand) error {
	if !g.CanStart() {
		return fmt.Errorf("%d명 이상 모여야 시작할 수 있습니다", RFMinPlayers)
	}
	g.Ready = true
	g.StartedAt = time.Now()
	g.Treasury = 0
	g.Result = nil
	g.LastAction = nil

	n := len(g.Players)
	deck := rfDeckComposition(n)
	rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })
	for _, p := range g.Players {
		p.Chips = RFStartChips
		p.Cards = []RFCard{{Role: deck[0]}, {Role: deck[1]}}
		deck = deck[2:]
	}
	g.Deck = deck

	// 진영 절반씩 — 홀수면 개혁파가 1명 많다
	factions := make([]RFFaction, 0, n)
	for i := 0; i < n/2; i++ {
		factions = append(factions, RFFactionLoyalist)
	}
	for len(factions) < n {
		factions = append(factions, RFFactionReformist)
	}
	rng.Shuffle(len(factions), func(i, j int) { factions[i], factions[j] = factions[j], factions[i] })
	for i, p := range g.Players {
		p.Faction = factions[i]
	}

	g.CurrentSeat = rng.Intn(n)
	g.Phase = RFPhaseAction
	g.StateSeq++
	return nil
}

// ==================== 이벤트 큐 ====================

func (g *RFGame) emit(kind string, seat int, msg string) {
	g.events = append(g.events, RFGameEvent{Kind: kind, Seat: seat, Message: msg})
	if seat >= 0 && seat < len(g.Players) {
		g.LastAction = &RFLastAction{Seat: seat, Name: g.Players[seat].Name, Message: msg}
	}
}

// DrainEvents 쌓인 이벤트를 꺼내고 비운다 (허브가 방송)
func (g *RFGame) DrainEvents() []RFGameEvent {
	evs := g.events
	g.events = nil
	return evs
}

// ==================== 좌석 / 진영 헬퍼 ====================

func (g *RFGame) alivePlayers() []*RFPlayer {
	alive := []*RFPlayer{}
	for _, p := range g.Players {
		if p.Alive() {
			alive = append(alive, p)
		}
	}
	return alive
}

func (g *RFGame) aliveCount() int {
	return len(g.alivePlayers())
}

// FactionCount 살아 있는 해당 진영 인원 (봇·테스트가 쓰는 공개 헬퍼)
func (g *RFGame) FactionCount(f RFFaction) int {
	n := 0
	for _, p := range g.alivePlayers() {
		if p.Faction == f {
			n++
		}
	}
	return n
}

// nextAliveSeat seat 다음의 생존 좌석 (시계 방향)
func (g *RFGame) nextAliveSeat(seat int) int {
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
func (g *RFGame) respondersExcept(except int) map[int]bool {
	responders := map[int]bool{}
	for _, p := range g.Players {
		if p.Alive() && p.Seat != except {
			responders[p.Seat] = true
		}
	}
	return responders
}

// rfRoleNames 역할 목록의 한글 표기 (횡령 증명 공개 문구용)
func rfRoleNames(roles []RFRole) string {
	names := make([]string, 0, len(roles))
	for _, r := range roles {
		names = append(names, rfRoleName(r))
	}
	return strings.Join(names, ", ")
}

// ==================== 차례 검증 ====================

// checkTurn 행동 가능한 차례인지 + 쿠 강제 위반 여부
func (g *RFGame) checkTurn(seat int, kind RFActionKind) error {
	if g.Phase != RFPhaseAction {
		return errors.New("지금은 행동할 수 없습니다")
	}
	if seat < 0 || seat >= len(g.Players) || seat != g.CurrentSeat {
		return errors.New("당신의 차례가 아닙니다")
	}
	if g.Players[seat].Chips >= RFForceCoupChips && kind != RFActCoup {
		return fmt.Errorf("칩이 %d개 이상이면 쿠만 할 수 있습니다", RFForceCoupChips)
	}
	return nil
}

// ==================== 액션 선언 ====================

// DeclareAction 차례 액션 선언 (수입·해외원조·세금·암살·강탈·교환·쿠).
// 같은 진영에게는 강탈·암살·쿠를 쓸 수 없다.
func (g *RFGame) DeclareAction(seat int, kind RFActionKind, target int, rng *rand.Rand) error {
	if err := g.checkTurn(seat, kind); err != nil {
		return err
	}
	actor := g.Players[seat]

	needsTarget := rfAttackKinds[kind]
	if needsTarget {
		if target < 0 || target >= len(g.Players) || target == seat || !g.Players[target].Alive() {
			return errors.New("대상을 올바르게 선택하세요")
		}
		if g.Players[target].Faction == actor.Faction {
			return errors.New(rfSameFactionMsg)
		}
	}

	switch kind {
	case RFActIncome:
		actor.Chips++
		g.emit("action", seat, fmt.Sprintf("%s님이 수입으로 1칩을 얻었습니다", actor.Name))
		g.nextTurn()

	case RFActAid:
		g.Pending = &RFPending{Kind: kind, ActorSeat: seat, TargetSeat: -1, BlockerSeat: -1}
		g.emit("action", seat, fmt.Sprintf("%s님이 해외원조로 2칩을 받으려 합니다", actor.Name))
		g.openBlockWindow(rng)

	case RFActCoup:
		if actor.Chips < RFCoupCost {
			return fmt.Errorf("쿠에는 칩 %d개가 필요합니다", RFCoupCost)
		}
		actor.Chips -= RFCoupCost
		g.Pending = &RFPending{Kind: kind, ActorSeat: seat, TargetSeat: target, BlockerSeat: -1}
		g.emit("action", seat, fmt.Sprintf("%s님이 칩 %d개로 %s님에게 쿠를 선언했습니다 — 차단·도전 불가",
			actor.Name, RFCoupCost, g.Players[target].Name))
		g.enterLose(target, rfAfterNextTurn, rng)

	case RFActTax, RFActAssassinate, RFActSteal, RFActExchange:
		if kind == RFActAssassinate {
			if actor.Chips < RFAssassinCost {
				return fmt.Errorf("암살에는 칩 %d개가 필요합니다", RFAssassinCost)
			}
			actor.Chips -= RFAssassinCost
		}
		p := &RFPending{Kind: kind, ActorSeat: seat, TargetSeat: -1,
			ClaimRole: rfActionClaim[kind], BlockerSeat: -1}
		if needsTarget {
			p.TargetSeat = target
		}
		g.Pending = p
		switch kind {
		case RFActTax:
			g.emit("action", seat, fmt.Sprintf("%s님이 세금(공작 주장)으로 3칩을 걷으려 합니다", actor.Name))
		case RFActAssassinate:
			g.emit("action", seat, fmt.Sprintf("%s님이 칩 %d개로 %s님 암살(암살자 주장)을 선언했습니다",
				actor.Name, RFAssassinCost, g.Players[target].Name))
		case RFActSteal:
			g.emit("action", seat, fmt.Sprintf("%s님이 %s님에게 강탈(사령관 주장)을 선언했습니다",
				actor.Name, g.Players[target].Name))
		case RFActExchange:
			g.emit("action", seat, fmt.Sprintf("%s님이 교환(대사 주장)을 선언했습니다", actor.Name))
		}
		g.openChallengeWindow()

	default:
		return errors.New("알 수 없는 액션입니다")
	}
	return nil
}

// ==================== 개종 (즉시 발동 — 도전·차단 대상 아님) ====================

// SubmitConvert 자기 진영 바꾸기 — 칩 1개를 국고에 넣고 진영을 뒤집는다
func (g *RFGame) SubmitConvert(seat int, rng *rand.Rand) error {
	if err := g.checkTurn(seat, RFActConvert); err != nil {
		return err
	}
	actor := g.Players[seat]
	if actor.Chips < RFConvertSelfCost {
		return fmt.Errorf("진영을 바꾸려면 칩 %d개가 필요합니다", RFConvertSelfCost)
	}
	actor.Chips -= RFConvertSelfCost
	g.Treasury += RFConvertSelfCost
	before := actor.Faction
	actor.Faction = rfFlipFaction(before)
	g.emit("convert", seat, fmt.Sprintf("%s님이 칩 %d개를 국고에 넣고 %s에서 %s로 개종했습니다 (국고 %d)",
		actor.Name, RFConvertSelfCost, rfFactionName(before), rfFactionName(actor.Faction), g.Treasury))
	g.nextTurn()
	return nil
}

// SubmitConvertOther 남의 진영 바꾸기 — 칩 2개를 국고에 넣고 대상 진영을
// 뒤집는다 (같은 진영 대상도 가능하다 — 공격이 아니다)
func (g *RFGame) SubmitConvertOther(seat, target int, rng *rand.Rand) error {
	if err := g.checkTurn(seat, RFActConvertOther); err != nil {
		return err
	}
	actor := g.Players[seat]
	if target < 0 || target >= len(g.Players) || target == seat || !g.Players[target].Alive() {
		return errors.New("대상을 올바르게 선택하세요")
	}
	if actor.Chips < RFConvertOtherCost {
		return fmt.Errorf("남의 진영을 바꾸려면 칩 %d개가 필요합니다", RFConvertOtherCost)
	}
	actor.Chips -= RFConvertOtherCost
	g.Treasury += RFConvertOtherCost
	victim := g.Players[target]
	before := victim.Faction
	victim.Faction = rfFlipFaction(before)
	g.emit("convert_other", seat, fmt.Sprintf(
		"%s님이 칩 %d개를 국고에 넣고 %s님을 %s에서 %s로 개종시켰습니다 (국고 %d)",
		actor.Name, RFConvertOtherCost, victim.Name,
		rfFactionName(before), rfFactionName(victim.Faction), g.Treasury))
	g.nextTurn()
	return nil
}

// ==================== 횡령 ====================

// SubmitEmbezzle 횡령 선언 — 국고 전액을 가져온다. 주장은 "나는 공작이
// 아니다"이며 도전 창이 열린다 (차단은 불가).
func (g *RFGame) SubmitEmbezzle(seat int, rng *rand.Rand) error {
	if err := g.checkTurn(seat, RFActEmbezzle); err != nil {
		return err
	}
	actor := g.Players[seat]
	g.Pending = &RFPending{Kind: RFActEmbezzle, ActorSeat: seat, TargetSeat: -1,
		ClaimRole: rfEmbezzleClaim, BlockerSeat: -1}
	g.emit("action", seat, fmt.Sprintf(
		"%s님이 횡령을 선언했습니다 — \"나는 공작이 아니다\"라며 국고 %d칩을 가져가려 합니다",
		actor.Name, g.Treasury))
	g.openChallengeWindow()
	return nil
}

// ==================== 응답 창 ====================

func (g *RFGame) openChallengeWindow() {
	p := g.Pending
	p.responders = g.respondersExcept(p.ActorSeat)
	p.passed = map[int]bool{}
	if p.Kind == RFActEmbezzle {
		p.Message = fmt.Sprintf("%s님의 횡령(공작 부재 주장) — 도전하거나 통과하세요",
			g.Players[p.ActorSeat].Name)
	} else {
		p.Message = fmt.Sprintf("%s님의 %s(%s 주장) — 도전하거나 통과하세요",
			g.Players[p.ActorSeat].Name, rfActionName(p.Kind), rfRoleName(p.ClaimRole))
	}
	g.Phase = RFPhaseChallengeWindow
	g.StateSeq++
}

// openBlockWindow 차단 선언 창. 대상만 차단하는 액션(암살·강탈)에서 대상이
// 이미 탈락했으면 창을 건너뛰고 바로 해결한다.
func (g *RFGame) openBlockWindow(rng *rand.Rand) {
	p := g.Pending
	if (p.Kind == RFActAssassinate || p.Kind == RFActSteal) && !g.Players[p.TargetSeat].Alive() {
		g.resolveAction(rng)
		return
	}
	p.responders = g.respondersExcept(p.ActorSeat)
	p.passed = map[int]bool{}
	switch p.Kind {
	case RFActAid:
		p.Message = fmt.Sprintf("%s님의 해외원조 — 공작 주장으로 차단하거나 통과하세요",
			g.Players[p.ActorSeat].Name)
	case RFActAssassinate:
		p.Message = fmt.Sprintf("%s님은 백작부인 주장으로 차단할 수 있습니다", g.Players[p.TargetSeat].Name)
	case RFActSteal:
		p.Message = fmt.Sprintf("%s님은 사령관·대사 주장으로 차단할 수 있습니다", g.Players[p.TargetSeat].Name)
	}
	g.Phase = RFPhaseBlockWindow
	g.StateSeq++
}

// openBlockChallengeWindow 선언된 차단에 대한 도전 창. phase 는 도전 창을
// 재사용하고 pending.BlockerSeat 으로 구분한다.
func (g *RFGame) openBlockChallengeWindow() {
	p := g.Pending
	p.responders = g.respondersExcept(p.BlockerSeat)
	p.passed = map[int]bool{}
	p.Message = fmt.Sprintf("%s님의 차단(%s 주장) — 도전하거나 통과하세요",
		g.Players[p.BlockerSeat].Name, rfRoleName(p.BlockRole))
	g.Phase = RFPhaseChallengeWindow
	g.StateSeq++
}

func (g *RFGame) inWindow() bool {
	return g.Phase == RFPhaseChallengeWindow || g.Phase == RFPhaseBlockWindow
}

// blockChallenge 현재 도전 창이 "차단에 대한 도전"인지
func (g *RFGame) blockChallenge() bool {
	return g.Phase == RFPhaseChallengeWindow && g.Pending != nil && g.Pending.BlockerSeat >= 0
}

// SubmitPass 창 통과 동의. 응답 대상이 아니거나 이미 지나간 창의 뒤늦은
// 통과는 조용히 무시한다 (창 경쟁의 정상 경로). 전원 통과면 즉시 닫힌다.
func (g *RFGame) SubmitPass(seat int, rng *rand.Rand) {
	if !g.inWindow() || g.Pending == nil {
		return
	}
	p := g.Pending
	if !p.responders[seat] || p.passed[seat] {
		return
	}
	p.passed[seat] = true
	for s := range p.responders {
		if !p.passed[s] {
			return
		}
	}
	g.passWindow(rng)
}

// ForcePassWindow 마감 시각 경과 — 전원 통과와 같은 처리 (허브 타이머)
func (g *RFGame) ForcePassWindow(rng *rand.Rand) {
	if g.inWindow() {
		g.passWindow(rng)
	}
}

// passWindow 현재 창의 통과 결말
func (g *RFGame) passWindow(rng *rand.Rand) {
	switch g.Phase {
	case RFPhaseChallengeWindow:
		if g.blockChallenge() {
			p := g.Pending
			g.emit("block_success", p.BlockerSeat, fmt.Sprintf("%s님의 차단이 통과됐습니다 — %s이 취소됩니다",
				g.Players[p.BlockerSeat].Name, rfActionName(p.Kind)))
			g.nextTurn()
			return
		}
		g.afterClaimAccepted(rng)
	case RFPhaseBlockWindow:
		g.resolveAction(rng) // 아무도 차단하지 않았다
	}
}

// afterClaimAccepted 도전 창 통과(또는 도전 실패 뒤 재개) — 차단 가능
// 액션은 차단 창으로, 나머지는 해결로 간다.
func (g *RFGame) afterClaimAccepted(rng *rand.Rand) {
	p := g.Pending
	if p.Kind == RFActAssassinate || p.Kind == RFActSteal {
		g.openBlockWindow(rng)
		return
	}
	g.resolveAction(rng)
}

// ==================== 도전 ====================

// SubmitChallenge 역할 주장(액션·차단)에 대한 도전. 뒤늦은 도전은 조용히
// 무시한다.
func (g *RFGame) SubmitChallenge(seat int, rng *rand.Rand) {
	p := g.Pending
	if p == nil || g.Phase != RFPhaseChallengeWindow || !p.responders[seat] {
		return
	}
	if g.blockChallenge() {
		g.challengeBlock(seat, rng)
		return
	}
	if p.Kind == RFActEmbezzle {
		g.challengeEmbezzle(seat, rng)
		return
	}
	g.challengeAction(seat, rng)
}

// challengeAction 액션의 역할 주장에 대한 도전 (기본 쿠 규칙 그대로)
func (g *RFGame) challengeAction(seat int, rng *rand.Rand) {
	p := g.Pending
	claimant := g.Players[p.ActorSeat]
	g.emit("challenge", seat, fmt.Sprintf("%s님이 %s님의 %s 주장에 도전했습니다",
		g.Players[seat].Name, claimant.Name, rfRoleName(p.ClaimRole)))
	if claimant.HasHidden(p.ClaimRole) {
		g.proveClaim(claimant, p.ClaimRole, rng)
		g.emit("challenge_fail", seat, fmt.Sprintf(
			"도전 실패 — %s님이 실제 %s 카드를 공개하고 덱에서 새 카드를 받았습니다. %s님이 카드 1장을 잃습니다",
			claimant.Name, rfRoleName(p.ClaimRole), g.Players[seat].Name))
		g.enterLose(seat, rfAfterProceed, rng)
		return
	}
	g.emit("challenge_success", p.ActorSeat, fmt.Sprintf(
		"도전 성공 — %s님에게 %s 카드가 없었습니다. %s이 취소되고 카드 1장을 잃습니다",
		claimant.Name, rfRoleName(p.ClaimRole), rfActionName(p.Kind)))
	if p.Kind == RFActAssassinate {
		claimant.Chips += RFAssassinCost // 거짓 암살은 취소 — 지불한 비용 반환
	}
	g.enterLose(p.ActorSeat, rfAfterNextTurn, rng)
}

// challengeEmbezzle 횡령의 역주장("나는 공작이 아니다")에 대한 도전.
// 횡령자는 비공개 카드를 전부 공개해 공작 부재를 증명한다.
//   - 공작이 있으면 도전자 승 — 횡령이 취소되고 횡령자가 카드 1장을 잃는다
//   - 없으면 증명 성공 — 공개한 카드를 덱에 섞고 같은 장수를 다시 뽑은 뒤
//     도전자가 카드 1장을 잃고 횡령이 그대로 해결된다
func (g *RFGame) challengeEmbezzle(seat int, rng *rand.Rand) {
	p := g.Pending
	claimant := g.Players[p.ActorSeat]
	g.emit("challenge", seat, fmt.Sprintf("%s님이 %s님의 횡령(공작 부재 주장)에 도전했습니다",
		g.Players[seat].Name, claimant.Name))

	revealed := claimant.HiddenRoles()
	g.emit("embezzle_proof", p.ActorSeat, fmt.Sprintf("%s님이 손패를 모두 공개했습니다 — [%s]",
		claimant.Name, rfRoleNames(revealed)))

	if claimant.HasHidden(rfEmbezzleClaim) {
		g.emit("challenge_success", p.ActorSeat, fmt.Sprintf(
			"도전 성공 — %s님에게 공작이 있었습니다. 횡령이 취소되고 카드 1장을 잃습니다", claimant.Name))
		g.enterLose(p.ActorSeat, rfAfterNextTurn, rng)
		return
	}
	g.redrawHand(claimant, rng)
	g.emit("challenge_fail", seat, fmt.Sprintf(
		"도전 실패 — %s님에게 공작이 없었습니다. 공개한 카드를 덱에 섞고 같은 장수를 다시 뽑았으며 %s님이 카드 1장을 잃습니다",
		claimant.Name, g.Players[seat].Name))
	g.enterLose(seat, rfAfterProceed, rng)
}

// challengeBlock 선언된 차단의 역할 주장에 대한 도전
func (g *RFGame) challengeBlock(seat int, rng *rand.Rand) {
	p := g.Pending
	blocker := g.Players[p.BlockerSeat]
	g.emit("challenge", seat, fmt.Sprintf("%s님이 %s님의 차단(%s 주장)에 도전했습니다",
		g.Players[seat].Name, blocker.Name, rfRoleName(p.BlockRole)))
	if blocker.HasHidden(p.BlockRole) {
		g.proveClaim(blocker, p.BlockRole, rng)
		g.emit("challenge_fail", seat, fmt.Sprintf(
			"도전 실패 — %s님이 실제 %s 카드를 공개했습니다. 차단이 성립해 %s이 취소되고 %s님이 카드 1장을 잃습니다",
			blocker.Name, rfRoleName(p.BlockRole), rfActionName(p.Kind), g.Players[seat].Name))
		g.enterLose(seat, rfAfterNextTurn, rng)
		return
	}
	g.emit("challenge_success", p.BlockerSeat, fmt.Sprintf(
		"도전 성공 — %s님에게 %s 카드가 없었습니다. 차단이 무효가 되고 카드 1장을 잃습니다",
		blocker.Name, rfRoleName(p.BlockRole)))
	g.enterLose(p.BlockerSeat, rfAfterResolve, rng)
}

// proveClaim 주장 입증 — 해당 카드를 덱에 넣고 섞은 뒤 새로 1장 뽑아
// 비공개 상태를 유지한다 (도전자에게 정체가 드러났으므로 교체)
func (g *RFGame) proveClaim(p *RFPlayer, role RFRole, rng *rand.Rand) {
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

// redrawHand 손패 전체 교체 — 공개한 비공개 카드를 전부 덱에 섞고 같은
// 장수를 다시 뽑는다 (횡령 증명 뒤 처리, 대사 교환과 같은 결)
func (g *RFGame) redrawHand(p *RFPlayer, rng *rand.Rand) {
	hidden := p.HiddenIdx()
	for _, idx := range hidden {
		g.Deck = append(g.Deck, p.Cards[idx].Role)
	}
	rng.Shuffle(len(g.Deck), func(a, b int) { g.Deck[a], g.Deck[b] = g.Deck[b], g.Deck[a] })
	for _, idx := range hidden {
		p.Cards[idx].Role = g.Deck[0]
		g.Deck = g.Deck[1:]
	}
}

// ==================== 차단 ====================

// SubmitBlock 차단 선언 — 차단 도전 창을 연다. 해외원조는 아무나(같은
// 진영도 가능), 암살·강탈은 대상만 차단할 수 있다.
func (g *RFGame) SubmitBlock(seat int, role RFRole) error {
	if g.Phase != RFPhaseBlockWindow || g.Pending == nil {
		return errors.New("지금은 차단할 수 없습니다")
	}
	p := g.Pending
	if !p.responders[seat] {
		return errors.New("차단할 수 없는 좌석입니다")
	}
	if (p.Kind == RFActAssassinate || p.Kind == RFActSteal) && seat != p.TargetSeat {
		return errors.New("대상만 차단할 수 있습니다")
	}
	valid := false
	for _, r := range rfBlockRoles[p.Kind] {
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
		g.Players[seat].Name, rfRoleName(role), rfActionName(p.Kind)))
	g.openBlockChallengeWindow()
	return nil
}

// ==================== 카드 제거 ====================

// enterLose seat 이 카드 1장을 잃는다. 비공개가 1장뿐이면 즉시 자동 공개하고
// 진행을 이어간다 (선택의 여지가 없다). 2장이면 lose_card 로 당사자 선택.
func (g *RFGame) enterLose(seat int, after rfAfter, rng *rand.Rand) {
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
	g.Phase = RFPhaseLoseCard
	g.StateSeq++
}

// reveal 카드를 공개로 뒤집는다 — 잃은 카드는 전원에게 보인다
func (g *RFGame) reveal(p *RFPlayer, cardIdx int) {
	p.Cards[cardIdx].Revealed = true
	g.emit("card_lost", p.Seat, fmt.Sprintf("%s님이 [%s] 카드를 잃었습니다",
		p.Name, rfRoleName(p.Cards[cardIdx].Role)))
	if !p.Alive() {
		g.emit("eliminated", p.Seat, fmt.Sprintf("%s님(%s)이 카드를 모두 잃고 탈락했습니다",
			p.Name, rfFactionName(p.Faction)))
	}
}

// SubmitLoseCard 당사자의 제거 카드 선택 — index 는 비공개 카드 목록(yourRoles) 기준
func (g *RFGame) SubmitLoseCard(seat, index int, rng *rand.Rand) error {
	if g.Phase != RFPhaseLoseCard || seat != g.LoseSeat {
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

// continueAfter 카드 제거가 끝난 뒤의 진행 — 승부가 났으면 종료
func (g *RFGame) continueAfter(after rfAfter, rng *rand.Rand) {
	if g.checkEnd() {
		return
	}
	switch after {
	case rfAfterProceed:
		g.afterClaimAccepted(rng)
	case rfAfterResolve:
		g.resolveAction(rng)
	default:
		g.nextTurn()
	}
}

// ==================== 해결 ====================

// resolveAction 창을 모두 통과한 액션의 효과 적용
func (g *RFGame) resolveAction(rng *rand.Rand) {
	p := g.Pending
	actor := g.Players[p.ActorSeat]
	switch p.Kind {
	case RFActAid:
		actor.Chips += 2
		g.emit("resolve", p.ActorSeat, fmt.Sprintf("%s님이 해외원조로 2칩을 받았습니다", actor.Name))
		g.nextTurn()

	case RFActTax:
		actor.Chips += 3
		g.emit("resolve", p.ActorSeat, fmt.Sprintf("%s님이 세금으로 3칩을 걷었습니다", actor.Name))
		g.nextTurn()

	case RFActEmbezzle:
		amount := g.Treasury
		g.Treasury = 0
		actor.Chips += amount
		g.emit("resolve", p.ActorSeat, fmt.Sprintf("%s님이 국고 %d칩을 횡령했습니다 (국고 0)",
			actor.Name, amount))
		g.nextTurn()

	case RFActSteal:
		target := g.Players[p.TargetSeat]
		if target.Alive() {
			amt := RFStealMax
			if target.Chips < amt {
				amt = target.Chips
			}
			target.Chips -= amt
			actor.Chips += amt
			g.emit("resolve", p.ActorSeat, fmt.Sprintf("%s님이 %s님에게서 %d칩을 강탈했습니다",
				actor.Name, target.Name, amt))
		}
		g.nextTurn()

	case RFActAssassinate:
		target := g.Players[p.TargetSeat]
		if !target.Alive() {
			g.nextTurn()
			return
		}
		g.emit("resolve", p.ActorSeat, fmt.Sprintf("암살 성공 — %s님이 카드 1장을 잃습니다", target.Name))
		g.enterLose(p.TargetSeat, rfAfterNextTurn, rng)

	case RFActExchange:
		// 덱이 마르는 대형 판(9~10인)을 위해 뽑는 장수는 남은 만큼으로 줄인다
		draw := 2
		if draw > len(g.Deck) {
			draw = len(g.Deck)
		}
		g.ExchangeCards = append(actor.HiddenRoles(), g.Deck[:draw]...)
		g.Deck = g.Deck[draw:]
		p.Message = fmt.Sprintf("%s님이 유지할 카드를 고르는 중입니다", actor.Name)
		g.emit("resolve", p.ActorSeat, fmt.Sprintf("%s님이 덱에서 %d장을 뽑아 교환을 시작합니다",
			actor.Name, draw))
		g.Phase = RFPhaseExchange
		g.StateSeq++
	}
}

// SubmitExchange 교환 — yourExchange(비공개 + 뽑은 카드) 중 유지할 카드를
// 비공개 장수만큼 고른다. 나머지는 덱에 반납하고 섞는다.
func (g *RFGame) SubmitExchange(seat int, keep []int, rng *rand.Rand) error {
	if g.Phase != RFPhaseExchange || g.Pending == nil || seat != g.Pending.ActorSeat {
		return errors.New("지금은 교환 선택을 할 수 없습니다")
	}
	actor := g.Players[seat]
	hidden := actor.HiddenIdx()
	if len(keep) != len(hidden) {
		return fmt.Errorf("유지할 카드를 %d장 선택해야 합니다", len(hidden))
	}
	seen := map[int]bool{}
	for _, idx := range keep {
		if idx < 0 || idx >= len(g.ExchangeCards) || seen[idx] {
			return errors.New("잘못된 카드 선택입니다")
		}
		seen[idx] = true
	}
	for i, idx := range keep {
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

// AutoExchange 교환 방치 마감 — 무작위 유지 (허브 타이머)
func (g *RFGame) AutoExchange(rng *rand.Rand) {
	if g.Phase != RFPhaseExchange || g.Pending == nil {
		return
	}
	actor := g.Players[g.Pending.ActorSeat]
	keep := len(actor.HiddenIdx())
	if keep > len(g.ExchangeCards) {
		keep = len(g.ExchangeCards)
	}
	g.SubmitExchange(actor.Seat, rng.Perm(len(g.ExchangeCards))[:keep], rng)
}

// ==================== 턴 전환 / 종료 ====================

func (g *RFGame) nextTurn() {
	g.Pending = nil
	g.LoseSeat = -1
	g.ExchangeCards = nil
	if g.checkEnd() {
		return
	}
	g.CurrentSeat = g.nextAliveSeat(g.CurrentSeat)
	g.Phase = RFPhaseAction
	g.StateSeq++
}

// checkEnd 승부 판정 — 마지막 1명이면 그 좌석의 승리, 살아남은 전원이 같은
// 진영이면 그 진영 전원 승리. 종료했으면 true.
func (g *RFGame) checkEnd() bool {
	if !g.Ready || g.Phase == RFPhaseGameOver {
		return g.Phase == RFPhaseGameOver
	}
	alive := g.alivePlayers()
	if len(alive) <= 1 {
		g.finishLastStanding(alive)
		return true
	}
	faction := alive[0].Faction
	for _, p := range alive[1:] {
		if p.Faction != faction {
			return false
		}
	}
	g.finishFaction(faction, alive)
	return true
}

// finishLastStanding 최후 1인 승리
func (g *RFGame) finishLastStanding(alive []*RFPlayer) {
	seats := []int{}
	names := []string{}
	msg := "생존자가 없습니다"
	if len(alive) == 1 {
		seats = append(seats, alive[0].Seat)
		names = append(names, alive[0].Name)
		msg = fmt.Sprintf("%s님(%s)이 최후의 1인으로 승리했습니다!",
			alive[0].Name, rfFactionName(alive[0].Faction))
	}
	g.Result = &RFResult{Winner: "seat", WinnerSeats: seats, WinnerNames: names, Message: msg}
	g.emit("game_over", -1, msg)
	g.closeGame()
}

// finishFaction 진영 승리 — 살아남은 전원이 같은 진영이 된 순간 성립한다
// (개종으로도, 탈락으로도 성립할 수 있다)
func (g *RFGame) finishFaction(faction RFFaction, alive []*RFPlayer) {
	seats := []int{}
	names := []string{}
	for _, p := range alive {
		seats = append(seats, p.Seat)
		names = append(names, p.Name)
	}
	sort.Ints(seats)
	msg := fmt.Sprintf("살아남은 %d명이 모두 %s입니다 — %s 진영 승리!",
		len(alive), rfFactionName(faction), rfFactionName(faction))
	g.Result = &RFResult{Winner: string(faction), WinnerSeats: seats, WinnerNames: names, Message: msg}
	g.emit("faction_win", -1, msg)
	g.closeGame()
}

func (g *RFGame) closeGame() {
	g.Pending = nil
	g.LoseSeat = -1
	g.ExchangeCards = nil
	g.CurrentSeat = -1
	g.Phase = RFPhaseGameOver
}

// ==================== 자동 진행 보조 ====================

// AutoAttackTarget 자동 진행(쿠 강제 AFK)·봇 공용 공격 대상 선정 —
// 같은 진영은 제외하고, 최다 비공개 카드·동수면 최다 칩을 고른다.
// 공격할 수 있는 상대가 없으면 -1.
func (g *RFGame) AutoAttackTarget(seat int) int {
	if seat < 0 || seat >= len(g.Players) {
		return -1
	}
	me := g.Players[seat]
	best := -1
	for _, p := range g.Players {
		if p.Seat == seat || !p.Alive() || p.Faction == me.Faction {
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
