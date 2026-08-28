package server

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// ==================== 시타델 순수 규칙 ====================
//
// 클라이언트·타이머를 모르며, 허브(ct_hub.go)가 단계 마감(선택 45초 · 차례
// 60초 · 능력 30초)을 걸고 이벤트 큐(DrainEvents)를 방송한다.
//
// ── 상태기계 ────────────────────────────────────────────────────────────
//
// 단계가 많은 게임이라 진행의 뼈대를 먼저 못 박아 둔다. 상태는 여섯 개다.
//
//	waiting ──Start()──▶ pick_roles
//
//	pick_roles   직업 선택. 8장을 섞어 뒷면 1장 + 앞면 (6-n)장을 제외하고,
//	             왕관 보유자부터 PickOrder 순서로 한 장씩 집는다.
//	             CurrentSeat = 지금 고르는 좌석, CallingRole = 0.
//	             마지막 사람이 고르면 ─▶ 호출 시작 (callNext)
//
//	[호출 루프]  CallingRole 을 1 → 8 로 올리며 그 직업을 쥔 좌석을 찾는다.
//	             없으면 다음 번호로. 있으면 RoleRevealed 로 공개하고
//	               · 왕이면 다음 라운드 왕관을 그 좌석으로 (암살당해도 간다)
//	               · 암살당했으면 차례를 통째로 건너뛴다
//	               · 도둑에게 지목당했으면 금화를 전부 넘긴다
//	               · 상인 +금화 1 · 건축가 +카드 2장
//	               · 왕/주교/상인/장군은 자기 색 건물 수만큼 금화
//	             그리고 ─▶ turn
//	             8번까지 다 돌면 ─▶ endRound
//
//	turn         ① 자원: ct_gather gold(금화 2) | cards(2장 뽑기 ─▶ keep_card)
//	             ② 건설: ct_build 를 BuildsLeft 만큼 (건축가 3채)
//	             ct_end_turn ─▶ 능력이 있는 직업이면 ability, 아니면 호출 루프
//
//	keep_card    뽑은 2장 중 ct_keep 으로 1장만 손에 남기고 ─▶ turn
//
//	ability      ③ 직업 능력. 암살자·도둑·마술사·장군만 이 단계를 거친다.
//	             ct_ability 또는 ct_end_turn(사용 안 함) ─▶ 호출 루프
//
//	endRound     LastRound 면 ─▶ game_over. 아니면 왕관을 넘기고
//	             Round++ 후 다시 ─▶ pick_roles
//
//	game_over    점수 = 건물값 합 + 먼저 완성 4 + 완성 2 + 다섯 색 3
//
// 은닉의 경계도 이 상태기계를 따른다. RolePool 은 지금 고르는 좌석에게만,
// Role 은 호출로 공개되기 전까지 본인에게만, Hand·Draw 는 언제나 본인에게만
// 나간다. FaceDown(뒷면 제외 직업)은 종료까지 아무에게도 나가지 않는다.

// NewCTGame 대기 상태의 새 게임
func NewCTGame(id string) *CTGame {
	return &CTGame{
		ID:                id,
		Players:           []*CTPlayer{},
		Phase:             CTPhaseWaiting,
		Deck:              []CTCard{},
		RolePool:          []int{},
		FaceUp:            []int{},
		PickOrder:         []int{},
		CrownSeat:         -1,
		CrownNext:         -1,
		CurrentSeat:       -1,
		ThiefSeat:         -1,
		FirstCompleteSeat: -1,
	}
}

// AddPlayer 대기실 입장. 좌석 번호를 돌려준다.
func (g *CTGame) AddPlayer(name string) (int, error) {
	if g.Phase != CTPhaseWaiting {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= CTMaxPlayers {
		return -1, fmt.Errorf("자리가 없습니다 (최대 %d명)", CTMaxPlayers)
	}
	seat := len(g.Players)
	g.Players = append(g.Players, &CTPlayer{
		Seat:  seat,
		Name:  name,
		Hand:  []CTCard{},
		Built: []CTCard{},
		Draw:  []CTCard{},
	})
	return seat, nil
}

// RemovePlayer 대기실 퇴장. 좌석을 앞으로 당겨 빈틈을 없앤다.
func (g *CTGame) RemovePlayer(seat int) {
	if g.Phase != CTPhaseWaiting || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i, p := range g.Players {
		p.Seat = i
	}
}

// CanStart 시작 가능 여부 (호스트 명시 시작 — 3인부터)
func (g *CTGame) CanStart() bool {
	return g.Phase == CTPhaseWaiting && len(g.Players) >= CTMinPlayers
}

// ==================== 건물 카드 덱 ====================

// ctBuildDeck 구성표(ctBuildings)대로 건물 카드 65장을 만든다.
// id 는 1부터 이어 붙인다 (0 은 payload 에서 "지정 없음"이라 쓰지 않는다).
func ctBuildDeck() []CTCard {
	cards := []CTCard{}
	id := 0
	for _, def := range ctBuildings {
		for i := 0; i < def.Count; i++ {
			id++
			cards = append(cards, CTCard{
				ID: id, Name: def.Name, Color: def.Color, Cost: def.Cost,
			})
		}
	}
	return cards
}

// drawCards 덱 맨 위에서 n장 (덱이 모자라면 있는 만큼)
func (g *CTGame) drawCards(n int) []CTCard {
	out := []CTCard{}
	for i := 0; i < n && len(g.Deck) > 0; i++ {
		out = append(out, g.Deck[0])
		g.Deck = g.Deck[1:]
	}
	return out
}

// deckPut 쓰지 않은 카드를 덱 바닥으로 되돌린다 (덱이 마르지 않게 하는 근거)
func (g *CTGame) deckPut(cards ...CTCard) {
	g.Deck = append(g.Deck, cards...)
}

// ==================== 시작 ====================

// Start 덱을 섞고 손패·금화·왕관을 세팅한 뒤 첫 라운드의 직업 선택을 연다
func (g *CTGame) Start(rng *rand.Rand) error {
	if !g.CanStart() {
		return fmt.Errorf("%d명 이상 모여야 시작할 수 있습니다", CTMinPlayers)
	}
	g.rng = rng

	deck := ctBuildDeck()
	rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })
	g.Deck = deck

	for _, p := range g.Players {
		p.Gold = CTGoldStart
		p.Hand = g.drawCards(CTHandStart)
		p.Built = []CTCard{}
		p.Draw = []CTCard{}
	}

	g.CrownSeat = rng.Intn(len(g.Players))
	g.CrownNext = g.CrownSeat
	g.Round = 0
	g.Ready = true
	g.StartedAt = time.Now()
	g.startRound()
	return nil
}

// ==================== 라운드 / 직업 선택 ====================

// startRound 라운드를 열고 직업 선택 단계로 들어간다
func (g *CTGame) startRound() {
	g.Round++
	g.CallingRole = 0
	g.KilledRole = 0
	g.RobbedRole = 0
	g.ThiefSeat = -1
	for _, p := range g.Players {
		p.Role = 0
		p.RoleRevealed = 0
		p.Killed = false
		p.Robbed = false
		p.Draw = []CTCard{}
	}
	g.dealRoles()

	n := len(g.Players)
	g.PickOrder = make([]int, 0, n)
	for i := 0; i < n; i++ {
		g.PickOrder = append(g.PickOrder, (g.CrownSeat+i)%n)
	}
	g.PickIdx = 0
	g.CurrentSeat = g.PickOrder[0]
	g.Phase = CTPhasePickRoles
	g.StateSeq++

	crown := g.Players[g.CrownSeat]
	g.emit("round_start", -1, fmt.Sprintf(
		"%d라운드 직업 선택 — 왕관은 %s님에게 있습니다%s",
		g.Round, crown.Name, ctFaceUpText(g.FaceUp)))
}

// ctFaceUpText 앞면 제외 직업의 안내 문구 (뒷면 제외는 절대 적지 않는다)
func ctFaceUpText(faceUp []int) string {
	if len(faceUp) == 0 {
		return " (앞면으로 제외된 직업은 없습니다)"
	}
	names := []string{}
	for _, r := range faceUp {
		names = append(names, ctRoleName(r))
	}
	return fmt.Sprintf(" (앞면 제외: %s)", strings.Join(names, "·"))
}

// dealRoles 직업 8장을 섞어 뒷면 1장 + 앞면 (6-n)장을 제외하고 나머지를
// 선택 후보(RolePool)로 남긴다.
//
// 앞면 제외에서 왕은 뺀다 — 왕이 사라지면 왕관이 영영 돌지 않아 선이 굳는다
// (정식 규칙과 같은 예외). 뒷면 제외는 어떤 직업이든 될 수 있고, 그 정체는
// 종료까지 어떤 스냅샷·이벤트에도 실리지 않는다.
func (g *CTGame) dealRoles() {
	order := make([]int, 0, CTRoleCount)
	for r := 1; r <= CTRoleCount; r++ {
		order = append(order, r)
	}
	g.rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })

	g.FaceDown = order[0]
	rest := order[1:]

	want := ctFaceUpCount(len(g.Players))
	faceUp := []int{}
	removed := map[int]bool{}
	for _, r := range rest {
		if len(faceUp) >= want {
			break
		}
		if r == CTRoleKing {
			continue
		}
		faceUp = append(faceUp, r)
		removed[r] = true
	}
	sort.Ints(faceUp)
	g.FaceUp = faceUp

	pool := []int{}
	for _, r := range rest {
		if !removed[r] {
			pool = append(pool, r)
		}
	}
	sort.Ints(pool)
	g.RolePool = pool
}

// PickRole 직업 선택 단계에서 한 장을 집는다
func (g *CTGame) PickRole(seat, role int) error {
	if g.Phase != CTPhasePickRoles {
		return errors.New("지금은 직업을 고를 수 없습니다")
	}
	if seat < 0 || seat >= len(g.Players) {
		return errors.New("잘못된 좌석입니다")
	}
	if seat != g.CurrentSeat {
		return errors.New("직업을 고를 차례가 아닙니다")
	}
	if !ctRoleValid(role) {
		return errors.New("그런 직업은 없습니다")
	}
	idx := -1
	for i, r := range g.RolePool {
		if r == role {
			idx = i
			break
		}
	}
	if idx < 0 {
		return errors.New("고를 수 있는 직업이 아닙니다")
	}

	p := g.Players[seat]
	p.Role = role
	g.RolePool = append(g.RolePool[:idx], g.RolePool[idx+1:]...)
	// 어떤 직업을 골랐는지는 절대 알리지 않는다 (호출로만 공개된다)
	g.note(seat, "role_picked", fmt.Sprintf("%s님이 직업을 골랐습니다", p.Name))

	g.PickIdx++
	if g.PickIdx < len(g.PickOrder) {
		g.CurrentSeat = g.PickOrder[g.PickIdx]
		g.StateSeq++
		return nil
	}
	// 마지막 사람까지 골랐다 — 남은 후보는 공개하지 않고 호출을 시작한다
	g.RolePool = []int{}
	g.CurrentSeat = -1
	g.CallingRole = 0
	g.emit("calling_start", -1, "직업 선택이 끝났습니다 — 1번부터 순서대로 부릅니다")
	g.callNext()
	return nil
}

// ==================== 직업 호출 ====================

// holderOf 그 직업을 쥔 좌석 (-1 없음)
func (g *CTGame) holderOf(role int) int {
	for _, p := range g.Players {
		if p.Role == role {
			return p.Seat
		}
	}
	return -1
}

// callNext CallingRole 을 1→8 로 올리며 다음 차례를 연다.
// 8번까지 다 부르면 라운드를 끝낸다.
func (g *CTGame) callNext() {
	for role := g.CallingRole + 1; role <= CTRoleCount; role++ {
		g.CallingRole = role
		seat := g.holderOf(role)
		if seat < 0 {
			continue // 제외됐거나 아무도 안 고른 직업 — 조용히 넘어간다
		}
		p := g.Players[seat]
		p.RoleRevealed = role

		// 왕관은 암살당해도 왕에게 간다 (정식 규칙)
		if role == CTRoleKing {
			g.CrownNext = seat
		}

		if g.KilledRole == role {
			p.Killed = true
			g.note(seat, "killed", fmt.Sprintf(
				"%d번 %s — %s님이 암살당해 이번 차례를 건너뜁니다",
				role, ctRoleName(role), p.Name))
			continue
		}

		g.emit("role_revealed", seat, fmt.Sprintf(
			"%d번 %s — %s님의 차례입니다", role, ctRoleName(role), p.Name))

		if g.RobbedRole == role && g.ThiefSeat >= 0 && g.ThiefSeat != seat {
			thief := g.Players[g.ThiefSeat]
			stolen := p.Gold
			p.Gold = 0
			thief.Gold += stolen
			p.Robbed = true
			g.emit("robbed", seat, fmt.Sprintf(
				"도둑이 %s를 노렸습니다 — %s님의 금화 %d개가 %s님에게 넘어갑니다",
				ctRoleName(role), p.Name, stolen, thief.Name))
		}

		g.beginTurn(p, role)
		return
	}
	g.endRound()
}

// beginTurn 호출된 좌석의 차례를 연다 — 직업의 자동 능력과 색 수입을 먼저
// 정산하고 자원 단계를 기다린다
func (g *CTGame) beginTurn(p *CTPlayer, role int) {
	g.CurrentSeat = p.Seat
	g.GatherDone = false
	g.BuildsLeft = ctBuildLimit(role)
	g.AbilityUsed = false

	switch role {
	case CTRoleMerchant:
		p.Gold += CTMerchantGold
		g.emit("ability", p.Seat, fmt.Sprintf(
			"상인 능력 — %s님이 금화 %d개를 더 받습니다", p.Name, CTMerchantGold))
	case CTRoleArchitect:
		drawn := g.drawCards(CTArchitectDraw)
		p.Hand = append(p.Hand, drawn...)
		g.emit("ability", p.Seat, fmt.Sprintf(
			"건축가 능력 — %s님이 건물 카드 %d장을 더 뽑고 이 차례에 %d채까지 지을 수 있습니다",
			p.Name, len(drawn), CTBuildsArchitect))
	}

	if color := ctRoleIncomeColor(role); color != "" {
		if n := ctCountColor(p.Built, color); n > 0 {
			p.Gold += n
			g.emit("income", p.Seat, fmt.Sprintf(
				"%s 수입 — %s님이 %s 건물 %d채로 금화 %d개를 받습니다",
				ctRoleName(role), p.Name, ctColorLabel(color), n, n))
		}
	}

	g.Phase = CTPhaseTurn
	g.StateSeq++
}

// ctCountColor 도시에서 그 색 건물의 수
func ctCountColor(built []CTCard, color CTColor) int {
	n := 0
	for _, c := range built {
		if c.Color == color {
			n++
		}
	}
	return n
}

// ==================== 차례 — ① 자원 ====================

// checkTurn 차례·단계 검사 (turn 단계 전용)
func (g *CTGame) checkTurn(seat int) (*CTPlayer, error) {
	if g.Phase != CTPhaseTurn {
		switch g.Phase {
		case CTPhaseKeepCard:
			return nil, errors.New("먼저 뽑은 카드 중 남길 것을 골라야 합니다")
		case CTPhaseAbility:
			return nil, errors.New("지금은 직업 능력 단계입니다")
		case CTPhasePickRoles:
			return nil, errors.New("지금은 직업 선택 단계입니다")
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

// Gather 자원 — 금화 2 받기 또는 건물 카드 2장 뽑아 1장 남기기
func (g *CTGame) Gather(seat int, kind string) error {
	p, err := g.checkTurn(seat)
	if err != nil {
		return err
	}
	if g.GatherDone {
		return errors.New("이번 차례의 자원은 이미 받았습니다")
	}

	switch kind {
	case CTGatherGoldKind:
		p.Gold += CTGatherGold
		g.GatherDone = true
		g.note(seat, "gather", fmt.Sprintf("%s님이 금화 %d개를 받았습니다",
			p.Name, CTGatherGold))
		g.StateSeq++
		return nil

	case CTGatherCardsKind:
		drawn := g.drawCards(CTGatherDraw)
		switch len(drawn) {
		case 0:
			// 덱이 완전히 말랐다 — 금화로 대신한다 (판이 멈추지 않게 하는 방어선)
			p.Gold += CTGatherGold
			g.GatherDone = true
			g.note(seat, "gather", fmt.Sprintf(
				"덱에 남은 건물 카드가 없어 %s님이 금화 %d개를 받았습니다",
				p.Name, CTGatherGold))
		case 1:
			p.Hand = append(p.Hand, drawn[0])
			g.GatherDone = true
			g.note(seat, "gather", fmt.Sprintf(
				"덱에 한 장뿐이라 %s님이 건물 카드 1장을 그대로 손에 넣었습니다", p.Name))
		default:
			p.Draw = drawn
			g.Phase = CTPhaseKeepCard
			g.note(seat, "gather", fmt.Sprintf(
				"%s님이 건물 카드 %d장을 뽑았습니다 — 1장만 남깁니다", p.Name, len(drawn)))
		}
		g.StateSeq++
		return nil
	}
	return errors.New("금화 또는 건물 카드 중 하나를 골라야 합니다")
}

// Keep 뽑은 2장 중 남길 카드를 고른다 (나머지는 덱 바닥으로)
func (g *CTGame) Keep(seat, index int) error {
	if g.Phase != CTPhaseKeepCard {
		return errors.New("지금은 카드를 고를 때가 아닙니다")
	}
	if seat < 0 || seat >= len(g.Players) || seat != g.CurrentSeat {
		return errors.New("차례가 아닙니다")
	}
	p := g.Players[seat]
	if index < 0 || index >= len(p.Draw) {
		return errors.New("고를 수 없는 카드입니다")
	}

	keep := p.Draw[index]
	rest := []CTCard{}
	for i, c := range p.Draw {
		if i != index {
			rest = append(rest, c)
		}
	}
	p.Hand = append(p.Hand, keep)
	p.Draw = []CTCard{}
	g.deckPut(rest...)

	g.GatherDone = true
	g.Phase = CTPhaseTurn
	// 어떤 카드를 남겼는지는 알리지 않는다 (손패는 본인만 본다)
	g.note(seat, "keep", fmt.Sprintf("%s님이 건물 카드 1장을 손에 넣었습니다", p.Name))
	g.StateSeq++
	return nil
}

// ==================== 차례 — ② 건설 ====================

// ctHasName 같은 이름의 건물을 이미 지었는가
func ctHasName(built []CTCard, name string) bool {
	for _, c := range built {
		if c.Name == name {
			return true
		}
	}
	return false
}

// Build 손패의 건물 하나를 값만큼 금화를 내고 짓는다.
// 건축가는 한 차례에 3채까지, 나머지는 1채까지다.
func (g *CTGame) Build(seat, cardID int) error {
	p, err := g.checkTurn(seat)
	if err != nil {
		return err
	}
	if !g.GatherDone {
		return errors.New("먼저 금화나 건물 카드를 받아야 합니다")
	}
	if g.BuildsLeft <= 0 {
		return errors.New("이번 차례에 더 지을 수 없습니다")
	}
	if cardID <= 0 {
		return errors.New("지을 건물을 고르세요")
	}

	idx := -1
	for i, c := range p.Hand {
		if c.ID == cardID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return errors.New("손에 없는 건물 카드입니다")
	}
	card := p.Hand[idx]
	if ctHasName(p.Built, card.Name) {
		return fmt.Errorf("%s은(는) 이미 지었습니다", card.Name)
	}
	if p.Gold < card.Cost {
		return fmt.Errorf("금화가 %d개 모자랍니다", card.Cost-p.Gold)
	}

	p.Gold -= card.Cost
	p.Hand = append(p.Hand[:idx], p.Hand[idx+1:]...)
	p.Built = append(p.Built, card)
	g.BuildsLeft--

	g.note(seat, "built", fmt.Sprintf("%s님이 %s %s(%d)을(를) 지었습니다 (%d채)",
		p.Name, ctColorLabel(card.Color), card.Name, card.Cost, len(p.Built)))

	if len(p.Built) >= CTBuildTarget && p.CompletedRound == 0 {
		p.CompletedRound = g.Round
		if g.FirstCompleteSeat < 0 {
			g.FirstCompleteSeat = seat
			g.LastRound = true
			g.emit("last_round", seat, fmt.Sprintf(
				"%s님이 건물 %d채를 가장 먼저 완성했습니다 — 이번 라운드까지만 진행합니다",
				p.Name, CTBuildTarget))
		} else {
			g.emit("completed", seat, fmt.Sprintf(
				"%s님도 건물 %d채를 완성했습니다", p.Name, CTBuildTarget))
		}
	}
	g.StateSeq++
	return nil
}

// ==================== 차례 — ③ 직업 능력 ====================

// EndTurn 차례 마무리. turn 단계에서 능력이 남아 있으면 ability 로 넘어가고,
// 능력이 없거나 이미 썼으면 다음 직업을 부른다.
func (g *CTGame) EndTurn(seat int) error {
	if g.Phase != CTPhaseTurn && g.Phase != CTPhaseAbility {
		return errors.New("지금은 차례를 끝낼 수 없습니다")
	}
	if seat < 0 || seat >= len(g.Players) || seat != g.CurrentSeat {
		return errors.New("차례가 아닙니다")
	}
	p := g.Players[seat]
	if g.Phase == CTPhaseTurn && ctRoleHasAbility(p.Role) && !g.AbilityUsed {
		g.Phase = CTPhaseAbility
		g.StateSeq++
		g.emit("ability_open", seat, fmt.Sprintf(
			"%s님의 %s 능력 차례입니다", p.Name, ctRoleName(p.Role)))
		return nil
	}
	if g.Phase == CTPhaseAbility {
		g.note(seat, "ability", fmt.Sprintf("%s님이 %s 능력을 쓰지 않았습니다",
			p.Name, ctRoleName(p.Role)))
	}
	g.finishTurn()
	return nil
}

// Ability 직업 능력 사용 (ability 단계 전용).
//
//	암살자 targetRole (2~8) — 그 직업의 차례를 통째로 건너뛰게 한다
//	도둑   targetRole (3~8, 암살당한 직업 제외) — 그 차례에 금화를 전부 뺏는다
//	마술사 targetSeat(손패 통째 교환) 또는 discard(버리고 그 수만큼 새로 뽑기)
//	장군   targetSeat + cardId — 금화(건물값-1)를 내고 남의 건물 1채 파괴
func (g *CTGame) Ability(seat int, payload CTAbilityPayload) error {
	if g.Phase != CTPhaseAbility {
		return errors.New("지금은 직업 능력을 쓸 수 없습니다")
	}
	if seat < 0 || seat >= len(g.Players) || seat != g.CurrentSeat {
		return errors.New("차례가 아닙니다")
	}
	p := g.Players[seat]

	switch p.Role {
	case CTRoleAssassin:
		if err := g.useAssassin(p, payload); err != nil {
			return err
		}
	case CTRoleThief:
		if err := g.useThief(p, payload); err != nil {
			return err
		}
	case CTRoleMagician:
		if err := g.useMagician(p, payload); err != nil {
			return err
		}
	case CTRoleWarlord:
		if err := g.useWarlord(p, payload); err != nil {
			return err
		}
	default:
		return errors.New("고를 대상이 있는 직업이 아닙니다")
	}

	g.AbilityUsed = true
	g.finishTurn()
	return nil
}

func (g *CTGame) useAssassin(p *CTPlayer, payload CTAbilityPayload) error {
	role := payload.TargetRole
	if !ctRoleValid(role) {
		return errors.New("지목할 직업을 고르세요")
	}
	if role == CTRoleAssassin {
		return errors.New("암살자는 자신을 지목할 수 없습니다")
	}
	g.KilledRole = role
	g.note(p.Seat, "ability", fmt.Sprintf("암살자가 %s를 지목했습니다", ctRoleName(role)))
	return nil
}

func (g *CTGame) useThief(p *CTPlayer, payload CTAbilityPayload) error {
	role := payload.TargetRole
	if !ctRoleValid(role) {
		return errors.New("지목할 직업을 고르세요")
	}
	if role == CTRoleAssassin || role == CTRoleThief {
		return errors.New("암살자와 도둑 자신은 지목할 수 없습니다")
	}
	if role == g.KilledRole {
		return errors.New("암살당한 직업은 지목할 수 없습니다")
	}
	g.RobbedRole = role
	g.ThiefSeat = p.Seat
	g.note(p.Seat, "ability", fmt.Sprintf("도둑이 %s를 노렸습니다", ctRoleName(role)))
	return nil
}

func (g *CTGame) useMagician(p *CTPlayer, payload CTAbilityPayload) error {
	if payload.TargetSeat != nil {
		target := *payload.TargetSeat
		if target < 0 || target >= len(g.Players) {
			return errors.New("잘못된 좌석입니다")
		}
		if target == p.Seat {
			return errors.New("자신과는 손패를 바꿀 수 없습니다")
		}
		other := g.Players[target]
		mine, theirs := len(p.Hand), len(other.Hand)
		p.Hand, other.Hand = other.Hand, p.Hand
		// 장수만 알린다 — 손패 내용은 본인들만 본다
		g.note(p.Seat, "ability", fmt.Sprintf(
			"마술사 %s님이 %s님과 손패를 통째로 바꿨습니다 (%d장 ↔ %d장)",
			p.Name, other.Name, mine, theirs))
		return nil
	}

	if len(payload.Discard) == 0 {
		return errors.New("바꿀 상대나 버릴 카드를 고르세요")
	}
	seen := map[int]bool{}
	for _, idx := range payload.Discard {
		if idx < 0 || idx >= len(p.Hand) {
			return errors.New("손에 없는 카드입니다")
		}
		if seen[idx] {
			return errors.New("같은 카드를 두 번 고를 수 없습니다")
		}
		seen[idx] = true
	}
	kept, dropped := []CTCard{}, []CTCard{}
	for i, c := range p.Hand {
		if seen[i] {
			dropped = append(dropped, c)
		} else {
			kept = append(kept, c)
		}
	}
	p.Hand = kept
	g.deckPut(dropped...)
	drawn := g.drawCards(len(dropped))
	p.Hand = append(p.Hand, drawn...)
	g.note(p.Seat, "ability", fmt.Sprintf(
		"마술사 %s님이 건물 카드 %d장을 버리고 %d장을 새로 뽑았습니다",
		p.Name, len(dropped), len(drawn)))
	return nil
}

func (g *CTGame) useWarlord(p *CTPlayer, payload CTAbilityPayload) error {
	if payload.TargetSeat == nil {
		return errors.New("파괴할 건물의 주인을 고르세요")
	}
	target := *payload.TargetSeat
	if target < 0 || target >= len(g.Players) {
		return errors.New("잘못된 좌석입니다")
	}
	other := g.Players[target]
	if other.Role == CTRoleBishop && !other.Killed {
		return errors.New("주교의 건물은 파괴할 수 없습니다")
	}
	if len(other.Built) >= CTBuildTarget {
		return fmt.Errorf("건물 %d채를 완성한 도시는 파괴할 수 없습니다", CTBuildTarget)
	}
	if payload.CardID <= 0 {
		return errors.New("파괴할 건물을 고르세요")
	}
	idx := -1
	for i, c := range other.Built {
		if c.ID == payload.CardID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return errors.New("그 도시에 없는 건물입니다")
	}
	card := other.Built[idx]
	cost := card.Cost - 1
	if cost < 0 {
		cost = 0
	}
	if p.Gold < cost {
		return fmt.Errorf("금화가 %d개 모자랍니다", cost-p.Gold)
	}

	p.Gold -= cost
	other.Built = append(other.Built[:idx], other.Built[idx+1:]...)
	g.deckPut(card)
	g.note(p.Seat, "destroyed", fmt.Sprintf(
		"장군 %s님이 금화 %d개를 내고 %s님의 %s %s(%d)을(를) 파괴했습니다",
		p.Name, cost, other.Name, ctColorLabel(card.Color), card.Name, card.Cost))
	return nil
}

// ==================== 차례 종료 / 라운드 종료 ====================

// finishTurn 차례를 닫고 다음 직업을 부른다
func (g *CTGame) finishTurn() {
	if g.Phase == CTPhaseGameOver {
		return
	}
	g.CurrentSeat = -1
	g.GatherDone = false
	g.BuildsLeft = 0
	g.AbilityUsed = false
	g.callNext()
}

// endRound 1~8번 호출이 모두 끝났다 — 종료 판정 후 왕관을 넘기고 다음 라운드
func (g *CTGame) endRound() {
	g.CurrentSeat = -1
	g.CallingRole = 0

	if g.LastRound {
		g.finish(fmt.Sprintf("건물 %d채가 완성돼 라운드를 마쳤습니다", CTBuildTarget))
		return
	}
	if g.Round >= CTMaxRounds {
		g.finish("라운드 상한에 닿아 경기를 마칩니다")
		return
	}
	if g.CrownNext >= 0 && g.CrownNext < len(g.Players) {
		g.CrownSeat = g.CrownNext
	}
	g.CrownNext = g.CrownSeat
	g.startRound()
}

// ==================== 점수 ====================

// ctScore 좌석 하나의 승점과 내역.
//
//	건물값 합 + 7채 먼저 완성 4점 + 7채 완성(1등 외) 2점 + 다섯 색 3점
func ctScore(p *CTPlayer, firstSeat int) (int, string) {
	base := 0
	for _, c := range p.Built {
		base += c.Cost
	}
	parts := []string{fmt.Sprintf("건물값 %d", base)}
	score := base

	if len(p.Built) >= CTBuildTarget {
		if p.Seat == firstSeat {
			score += CTBonusFirst
			parts = append(parts, fmt.Sprintf("먼저 완성 +%d", CTBonusFirst))
		} else {
			score += CTBonusComplete
			parts = append(parts, fmt.Sprintf("완성 +%d", CTBonusComplete))
		}
	}

	all := true
	for _, color := range ctColors {
		if ctCountColor(p.Built, color) == 0 {
			all = false
			break
		}
	}
	if all {
		score += CTBonusAllColors
		parts = append(parts, fmt.Sprintf("다섯 색 +%d", CTBonusAllColors))
	}
	return score, strings.Join(parts, " · ")
}

// finish 승패 판정 — 최고 승점, 동점이면 건물 수가 많은 쪽, 그래도 같으면 공동 승
func (g *CTGame) finish(reason string) {
	g.Phase = CTPhaseGameOver
	g.CurrentSeat = -1
	g.CallingRole = 0
	g.Deadline = 0
	g.StateSeq++

	rows := []CTResultRow{}
	bestScore, mostBuilt := -1, -1
	for _, p := range g.Players {
		score, detail := ctScore(p, g.FirstCompleteSeat)
		rows = append(rows, CTResultRow{Seat: p.Seat, Score: score, Detail: detail})
		built := len(p.Built)
		if score > bestScore || (score == bestScore && built > mostBuilt) {
			bestScore, mostBuilt = score, built
		}
	}

	seats, names := []int{}, []string{}
	for i, p := range g.Players {
		if rows[i].Score == bestScore && len(p.Built) == mostBuilt {
			seats = append(seats, p.Seat)
			names = append(names, p.Name)
		}
	}

	msg := fmt.Sprintf("%s — %s님이 승점 %d점으로 승리했습니다",
		reason, strings.Join(names, "·"), bestScore)
	if len(seats) > 1 {
		msg = fmt.Sprintf("%s — %s님이 승점 %d점·건물 %d채로 공동 승리했습니다",
			reason, strings.Join(names, "·"), bestScore, mostBuilt)
	}
	g.Result = &CTResult{
		WinnerSeats: seats, WinnerNames: names, Rows: rows, Message: msg,
	}
	g.emit("game_over", -1, msg)
}

// ==================== 이벤트 ====================

func (g *CTGame) emit(kind string, seat int, msg string) {
	g.events = append(g.events, CTGameEvent{Kind: kind, Seat: seat, Message: msg})
}

// DrainEvents 쌓인 이벤트를 꺼내 비운다 (허브가 방송한다)
func (g *CTGame) DrainEvents() []CTGameEvent {
	out := g.events
	g.events = nil
	return out
}

// note 마지막 행동 요약 기록 + 이벤트 방송
func (g *CTGame) note(seat int, kind, msg string) {
	name := ""
	if seat >= 0 && seat < len(g.Players) {
		name = g.Players[seat].Name
	}
	g.LastAction = &CTLastAction{Seat: seat, Name: name, Message: msg}
	g.emit(kind, seat, msg)
}

// ==================== AFK 자동 진행 ====================

// ForcePick 직업 선택 마감 — 남은 후보에서 무작위로 집는다
func (g *CTGame) ForcePick() {
	if g.Phase != CTPhasePickRoles || len(g.RolePool) == 0 {
		return
	}
	seat := g.CurrentSeat
	if seat < 0 || seat >= len(g.Players) {
		return
	}
	role := g.RolePool[g.rng.Intn(len(g.RolePool))]
	g.PickRole(seat, role)
}

// ForceTurn 차례 마감 — 자원을 안 받았으면 금화 2를 받고 차례를 끝낸다
// (능력은 쓰지 않는다)
func (g *CTGame) ForceTurn() {
	if g.Phase != CTPhaseTurn {
		return
	}
	seat := g.CurrentSeat
	if seat < 0 || seat >= len(g.Players) {
		return
	}
	p := g.Players[seat]
	if !g.GatherDone {
		p.Gold += CTGatherGold
		g.GatherDone = true
		g.note(seat, "gather", fmt.Sprintf("%s님이 자동으로 금화 %d개를 받았습니다",
			p.Name, CTGatherGold))
	}
	g.finishTurn()
}

// ForceKeep 카드 고르기 마감 — 첫 장을 남기고 차례를 끝낸다
func (g *CTGame) ForceKeep() {
	if g.Phase != CTPhaseKeepCard {
		return
	}
	seat := g.CurrentSeat
	if seat < 0 || seat >= len(g.Players) {
		return
	}
	if err := g.Keep(seat, 0); err != nil {
		return
	}
	g.finishTurn()
}

// ForceAbility 능력 마감 — 쓰지 않고 차례를 끝낸다
func (g *CTGame) ForceAbility() {
	if g.Phase != CTPhaseAbility {
		return
	}
	seat := g.CurrentSeat
	if seat < 0 || seat >= len(g.Players) {
		return
	}
	p := g.Players[seat]
	g.note(seat, "ability", fmt.Sprintf("%s님이 %s 능력을 쓰지 않았습니다",
		p.Name, ctRoleName(p.Role)))
	g.finishTurn()
}
