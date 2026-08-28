package server

import (
	"fmt"
	"math/rand"
	"sort"
	"time"
)

// ==================== 시타델 연습봇 ====================
//
// 스냅샷(ct_game_state)만 보고 반응한다. 봇도 사람과 같은 조건 — 자기 손패와
// 자기 직업만 알고, 남의 직업은 호출로 공개되기 전까지 모른다.
//
// 이 게임은 한 차례가 "① 자원 → ② 건설 → ③ 능력"의 여러 걸음으로 쪼개지는데
// 스냅샷에는 "자원을 이미 받았는가" 같은 진척이 실리지 않는다(와이어 계약
// 고정). 그래서 두뇌가 (라운드, 호출 중인 직업) 을 차례의 식별자로 삼아
// 자기 걸음을 센다.
//
//	turnID 가 바뀌면      → 걸음을 자원(gather)으로 되감는다
//	자원을 보냈으면        → 건설(build) 걸음
//	더 지을 게 없으면      → ct_end_turn (능력이 있으면 ability 단계가 열린다)
//
// 같은 대기 상태에서 두 번 행동하지 않도록 상태 식별키(stateKey)로 중복을
// 걸러내고, 서버가 거절(ct_error)하면 차례당 한 번만 복구를 시도한다
// (도둑의 지목이 암살당한 직업과 겹쳤을 때가 대표적이다).
//
// 판단의 축은 셋이다.
//
//	① 직업 선택 — 내 손패·금화·도시 색 분포로 이득이 큰 직업을 고른다.
//	   건축가·상인이 기본으로 세고, 선두면 주교(면역)·왕(왕관)이 올라간다.
//	② 차례      — 손패가 마르면 카드, 아니면 금화. 지을 수 있는 것 중 값이
//	   가장 큰 건물부터 짓고, 아직 없는 색이면 가산점을 준다(다섯 색 3점).
//	③ 능력      — 암살자·도둑은 선두가 골랐을 법한 직업을 노리고, 장군은
//	   선두의 싼 건물을 부수며, 마술사는 손패가 마르면 최다 손패와 바꾼다.

// 봇이 "생각하는" 시간 (테스트에서 짧게 낮춘다)
var (
	ctBotDelay    = 700 * time.Millisecond
	ctBotJitterMs = 700
)

// 봇 가치 함수 계수 (밸런스 조정 손잡이 — 봇 품질 측정 테스트가 이 값을 읽는다)
var (
	// ctBotIncomeWeight 직업 수입 색 건물 1채의 가치
	ctBotIncomeWeight = 1.3
	// ctBotColorBonus 아직 없는 색을 채울 때의 가산점 (다섯 색 3점 노림)
	ctBotColorBonus = 1.6
	// ctBotCostWeight 건물값 1의 가치 (값이 큰 건물부터 짓는 근거)
	ctBotCostWeight = 1.0
	// ctBotHandLow 이 장수 이하로 쓸 만한 손패가 줄면 카드를 뽑는다
	ctBotHandLow = 1
	// ctBotGoldRich 금화가 이만큼 쌓이면 손패를 넓히는 쪽이 낫다
	ctBotGoldRich = 8
	// ctBotLeadGap 선두와 이만큼 벌어지면 암살자·도둑의 가치가 오른다
	ctBotLeadGap = 2
	// ctBotWarlordMin 장군이 파괴를 시도하는 최소 상대 건물 수
	ctBotWarlordMin = 3
	// ctBotJitter 같은 점수의 직업 사이를 흔드는 폭 (판이 굳지 않게)
	ctBotJitter = 0.9
)

// ctBotPlayerView 봇이 스냅샷에서 꺼내 쓰는 좌석 정보
type ctBotPlayerView struct {
	Seat         int      `json:"seat"`
	Gold         int      `json:"gold"`
	HandCount    int      `json:"handCount"`
	Built        []CTCard `json:"built"`
	Score        int      `json:"score"`
	RoleRevealed int      `json:"roleRevealed"`
	Killed       bool     `json:"killed"`
	Robbed       bool     `json:"robbed"`
}

// ctBotState 봇이 상태 스냅샷에서 꺼내 쓰는 최소 정보
type ctBotState struct {
	YourSeat      int               `json:"yourSeat"`
	Phase         CTPhase           `json:"phase"`
	Round         int               `json:"round"`
	LastRound     bool              `json:"lastRound"`
	CrownSeat     int               `json:"crownSeat"`
	CallingRole   int               `json:"callingRole"`
	CurrentSeat   int               `json:"currentSeat"`
	FaceUpRemoved []int             `json:"faceUpRemoved"`
	PickPool      []int             `json:"pickPool"`
	YourRole      int               `json:"yourRole"`
	YourHand      []CTCard          `json:"yourHand"`
	YourDraw      []CTCard          `json:"yourDraw"`
	Players       []ctBotPlayerView `json:"players"`
}

// me 내 좌석 정보 (없으면 Seat -1)
func (s ctBotState) me() ctBotPlayerView {
	for _, p := range s.Players {
		if p.Seat == s.YourSeat {
			return p
		}
	}
	return ctBotPlayerView{Seat: -1}
}

// leader 나를 뺀 좌석 중 가장 앞선 사람 (건물 수 → 승점 순)
func (s ctBotState) leader() ctBotPlayerView {
	best := ctBotPlayerView{Seat: -1}
	for _, p := range s.Players {
		if p.Seat == s.YourSeat {
			continue
		}
		if best.Seat < 0 || len(p.Built) > len(best.Built) ||
			(len(p.Built) == len(best.Built) && p.Score > best.Score) {
			best = p
		}
	}
	return best
}

// removedSet 앞면으로 제외돼 아무도 못 쥔 직업
func (s ctBotState) removedSet() map[int]bool {
	out := map[int]bool{}
	for _, r := range s.FaceUpRemoved {
		out[r] = true
	}
	return out
}

// stateKey 같은 대기 상태에서 두 번 행동하지 않기 위한 식별키.
// 판이 실제로 달라졌는지를 나타내는 값들을 지문처럼 엮는다.
func (s ctBotState) stateKey() string {
	key := fmt.Sprintf("%s|%d|%d|%d|%d",
		s.Phase, s.Round, s.CallingRole, s.CurrentSeat, s.YourRole)
	for _, p := range s.Players {
		key += fmt.Sprintf(",%d:%d:%d:%d", p.Seat, p.Gold, p.HandCount, len(p.Built))
	}
	for _, c := range s.YourHand {
		key += fmt.Sprintf(".%d", c.ID)
	}
	for _, c := range s.YourDraw {
		key += fmt.Sprintf("/%d", c.ID)
	}
	return key
}

// 차례 안의 걸음
const (
	ctStageGather = iota
	ctStageBuild
	ctStageDone
)

// ctBrain 스냅샷 기반 판단 (봇 대체 좌석도 같은 두뇌를 쓴다)
type ctBrain struct {
	rng *rand.Rand
	// lastKey 마지막으로 행동한 대기 상태의 식별키 (중복 행동 방지)
	lastKey string
	// turnID 지금 진행 중인 내 차례의 식별자 (라운드·호출 직업)
	turnID string
	// stage turnID 안에서의 걸음 (자원 → 건설 → 종료)
	stage int
	// builds 이번 차례에 이미 보낸 건설 수 (건축가 3채 상한 관리)
	builds int
	// abilityTry 능력 지목이 거절됐을 때 다음 후보를 고르기 위한 순번
	abilityTry int
	// phase / myTurn 마지막 스냅샷의 단계와 내 차례 여부 (에러 복구 판단)
	phase  CTPhase
	myTurn bool
	// last 마지막 스냅샷 (거절 복구가 다시 판단할 근거)
	last ctBotState
	// errTurn 이미 차례를 끝내 복구한 차례 (무한 왕복 방지)
	errTurn string
}

// ctBotAbilityRetries 지목이 거절됐을 때 다음 후보로 다시 시도하는 횟수 상한
const ctBotAbilityRetries = 2

func newCTBrain() *ctBrain {
	return &ctBrain{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

// decide 공용 러너 계약 — ct_game_state 와 ct_error 에만 반응한다
func (b *ctBrain) decide(msg CTMessage) *CTMessage {
	switch msg.Type {
	case CTMsgGameState:
		state, ok := botPayloadAs[ctBotState](msg.Payload)
		if !ok {
			return nil
		}
		return b.decideState(state)
	case CTMsgError:
		return b.recover()
	}
	return nil
}

// recover 서버가 거절했을 때의 복구.
//
// 암살자·도둑의 지목이 막혔으면(도둑이 암살당한 직업을 골랐을 때가 대표적)
// 다음 후보로 최대 ctBotAbilityRetries 번 다시 지목한다 — abilityTry 가
// 매번 오르므로 반드시 끝난다. 그래도 안 되거나 다른 단계의 거절이면
// 차례를 끝내 판을 민다 (차례당 한 번만 — 무한 왕복 방지).
func (b *ctBrain) recover() *CTMessage {
	if !b.myTurn {
		return nil
	}
	if b.phase == CTPhaseAbility && b.abilityTry > 0 && b.abilityTry <= ctBotAbilityRetries {
		if move := b.abilityMove(b.last, b.last.me()); move != nil {
			return move
		}
	}
	if b.errTurn == b.turnID {
		return nil
	}
	b.errTurn = b.turnID
	switch b.phase {
	case CTPhaseAbility, CTPhaseTurn:
		return &CTMessage{Type: CTMsgEndTurn}
	}
	return nil
}

// think 사람처럼 잠깐 뜸을 들인다 (테스트에서는 var 를 낮춰 즉시 진행한다)
func (b *ctBrain) think() {
	d := ctBotDelay
	if ctBotJitterMs > 0 {
		d += time.Duration(b.rng.Intn(ctBotJitterMs)) * time.Millisecond
	}
	if d > 0 {
		time.Sleep(d)
	}
}

// decideState 내 차례면 정확히 한 수를 결정한다
func (b *ctBrain) decideState(s ctBotState) *CTMessage {
	me := s.me()
	b.phase = s.Phase
	b.myTurn = me.Seat >= 0 && s.CurrentSeat == me.Seat
	b.last = s
	if !b.myTurn {
		return nil
	}
	switch s.Phase {
	case CTPhasePickRoles, CTPhaseKeepCard, CTPhaseTurn, CTPhaseAbility:
	default:
		return nil
	}

	key := s.stateKey()
	if b.lastKey == key {
		return nil
	}
	b.lastKey = key

	var move *CTMessage
	switch s.Phase {
	case CTPhasePickRoles:
		move = b.pickMove(s, me)
	case CTPhaseKeepCard:
		move = b.keepMove(s, me)
	case CTPhaseTurn:
		move = b.turnMove(s, me)
	case CTPhaseAbility:
		move = b.abilityMove(s, me)
	}
	if move == nil { // 방어선 — 어떤 상황에서도 판은 굴러가야 한다
		if s.Phase == CTPhaseTurn || s.Phase == CTPhaseAbility {
			move = &CTMessage{Type: CTMsgEndTurn}
		}
	}
	if move != nil {
		b.think()
	}
	return move
}

// ==================== ① 직업 선택 ====================

// pickMove 후보 중 지금 판에서 이득이 가장 큰 직업을 고른다
func (b *ctBrain) pickMove(s ctBotState, me ctBotPlayerView) *CTMessage {
	if len(s.PickPool) == 0 {
		return nil
	}
	best, bestScore := 0, 0.0
	for _, role := range s.PickPool {
		score := b.roleValue(s, me, role) + b.rng.Float64()*ctBotJitter
		if best == 0 || score > bestScore {
			best, bestScore = role, score
		}
	}
	if best == 0 {
		best = s.PickPool[0]
	}
	return &CTMessage{Type: CTMsgPickRole, Payload: CTPickRolePayload{Role: best}}
}

// ctRoleBase 직업의 기본 가치
var ctRoleBase = map[int]float64{
	CTRoleAssassin:  4.0,
	CTRoleThief:     3.5,
	CTRoleMagician:  3.0,
	CTRoleKing:      5.0,
	CTRoleBishop:    4.5,
	CTRoleMerchant:  6.0,
	CTRoleArchitect: 6.5,
	CTRoleWarlord:   4.0,
}

// roleValue 내 손패·금화·도시로 그 직업이 이번 라운드에 얼마나 이득인가
func (b *ctBrain) roleValue(s ctBotState, me ctBotPlayerView, role int) float64 {
	value := ctRoleBase[role]
	hand := len(s.YourHand)
	useful := len(ctUsefulHand(s.YourHand, me.Built))

	// 수입 색 건물이 많을수록 그 직업이 값지다
	if color := ctRoleIncomeColor(role); color != "" {
		value += float64(ctCountColor(me.Built, color)) * ctBotIncomeWeight
	}

	leader := s.leader()
	leadGap := 0
	if leader.Seat >= 0 {
		leadGap = len(leader.Built) - len(me.Built)
	}
	amLeading := leadGap <= 0

	switch role {
	case CTRoleArchitect:
		// 손패와 금화가 받쳐줄 때 3채가 폭발한다
		value += float64(minInt(useful, CTBuildsArchitect))
		if me.Gold >= 6 {
			value += 3
		}
		if hand <= 1 {
			value += 2 // 카드 2장을 그냥 얹어준다
		}
	case CTRoleMerchant:
		value += 1 // 금화 1 추가
	case CTRoleKing:
		value += 1.5 // 다음 라운드 왕관 = 먼저 고르기
	case CTRoleBishop:
		if amLeading {
			value += 3 // 장군의 파괴에서 면역 — 선두일수록 값지다
		}
	case CTRoleMagician:
		if hand <= 1 {
			value += 4
		}
		if maxHand := ctMaxOtherHand(s); maxHand-hand >= 3 {
			value += 3
		}
	case CTRoleAssassin:
		if leadGap >= ctBotLeadGap {
			value += 2
		}
	case CTRoleThief:
		if leadGap >= ctBotLeadGap {
			value += 1.5
		}
		if me.Gold <= 1 {
			value += 2
		}
	case CTRoleWarlord:
		if leadGap >= ctBotLeadGap && me.Gold >= 3 {
			value += 2
		}
	}
	return value
}

// ctMaxOtherHand 남들 중 가장 많은 손패 장수
func ctMaxOtherHand(s ctBotState) int {
	best := 0
	for _, p := range s.Players {
		if p.Seat != s.YourSeat && p.HandCount > best {
			best = p.HandCount
		}
	}
	return best
}

// ctUsefulHand 아직 같은 이름을 짓지 않아 실제로 지을 수 있는 손패
func ctUsefulHand(hand []CTCard, built []CTCard) []CTCard {
	out := []CTCard{}
	seen := map[string]bool{}
	for _, c := range hand {
		if ctHasName(built, c.Name) || seen[c.Name] {
			continue
		}
		seen[c.Name] = true
		out = append(out, c)
	}
	return out
}

// ==================== ② 차례 (자원 → 건설) ====================

func (b *ctBrain) turnMove(s ctBotState, me ctBotPlayerView) *CTMessage {
	id := fmt.Sprintf("%d-%d", s.Round, s.CallingRole)
	if b.turnID != id {
		b.turnID = id
		b.stage = ctStageGather
		b.builds = 0
		b.abilityTry = 0
	}

	switch b.stage {
	case ctStageGather:
		b.stage = ctStageBuild
		return b.gatherMove(s, me)
	case ctStageBuild:
		if move := b.buildMove(s, me); move != nil {
			return move
		}
		b.stage = ctStageDone
		return &CTMessage{Type: CTMsgEndTurn}
	}
	return &CTMessage{Type: CTMsgEndTurn}
}

// gatherMove 손패가 마르면 카드, 아니면 금화.
// 금화만 두둑하고 지을 카드가 없으면 카드로 선택지를 넓힌다.
func (b *ctBrain) gatherMove(s ctBotState, me ctBotPlayerView) *CTMessage {
	useful := ctUsefulHand(s.YourHand, me.Built)
	kind := CTGatherGoldKind
	switch {
	case len(useful) <= ctBotHandLow:
		kind = CTGatherCardsKind
	case me.Gold >= ctBotGoldRich && len(useful) <= 3:
		kind = CTGatherCardsKind
	}
	return &CTMessage{Type: CTMsgGather, Payload: CTGatherPayload{Kind: kind}}
}

// buildMove 지을 수 있는 것 중 값이 가장 큰 건물 (없는 색이면 가산점)
func (b *ctBrain) buildMove(s ctBotState, me ctBotPlayerView) *CTMessage {
	limit := ctBuildLimit(s.YourRole)
	if b.builds >= limit {
		return nil
	}
	best, bestScore := 0, 0.0
	for _, c := range ctUsefulHand(s.YourHand, me.Built) {
		if c.Cost > me.Gold {
			continue
		}
		score := float64(c.Cost) * ctBotCostWeight
		if ctCountColor(me.Built, c.Color) == 0 {
			score += ctBotColorBonus
		}
		if best == 0 || score > bestScore {
			best, bestScore = c.ID, score
		}
	}
	if best == 0 {
		return nil
	}
	b.builds++
	return &CTMessage{Type: CTMsgBuild, Payload: CTBuildPayload{CardID: best}}
}

// keepMove 뽑은 2장 중 남길 것 — 이미 지은 이름은 버리고, 없는 색과 값을 본다
func (b *ctBrain) keepMove(s ctBotState, me ctBotPlayerView) *CTMessage {
	if len(s.YourDraw) == 0 {
		return nil
	}
	best, bestScore := 0, 0.0
	for i, c := range s.YourDraw {
		score := float64(c.Cost) * 0.6
		if ctCountColor(me.Built, c.Color) == 0 {
			score += ctBotColorBonus
		}
		if ctHasName(me.Built, c.Name) {
			score -= 10 // 같은 이름은 두 번 못 짓는다
		}
		if c.Cost > me.Gold+4 {
			score -= 1 // 당분간 감당 못 할 값
		}
		if i == 0 || score > bestScore {
			best, bestScore = i, score
		}
	}
	return &CTMessage{Type: CTMsgKeep, Payload: CTKeepPayload{Index: best}}
}

// ==================== ③ 직업 능력 ====================

func (b *ctBrain) abilityMove(s ctBotState, me ctBotPlayerView) *CTMessage {
	switch s.YourRole {
	case CTRoleAssassin:
		return b.targetMove(s, me, false)
	case CTRoleThief:
		return b.targetMove(s, me, true)
	case CTRoleMagician:
		return b.magicianMove(s, me)
	case CTRoleWarlord:
		return b.warlordMove(s, me)
	}
	return &CTMessage{Type: CTMsgEndTurn}
}

// ctAssassinOrder 암살자가 노릴 직업 순서 (선두가 아직 여유로울 때)
var ctAssassinOrder = []int{CTRoleArchitect, CTRoleMerchant, CTRoleKing, CTRoleWarlord,
	CTRoleBishop, CTRoleMagician, CTRoleThief}

// ctAssassinLeadOrder 선두가 도시를 거의 채웠을 때 — 면역(주교)과 왕관(왕)부터
var ctAssassinLeadOrder = []int{CTRoleBishop, CTRoleKing, CTRoleArchitect, CTRoleMerchant,
	CTRoleWarlord, CTRoleMagician, CTRoleThief}

// ctThiefOrder 도둑이 노릴 직업 순서 — 금화가 두둑할 법한 직업부터
// (암살자·도둑 자신은 규칙상 지목할 수 없어 목록에 없다)
var ctThiefOrder = []int{CTRoleMerchant, CTRoleArchitect, CTRoleKing, CTRoleWarlord,
	CTRoleBishop, CTRoleMagician}

// targetMove 암살자·도둑의 지목. 앞면으로 제외된 직업과 내 직업은 뺀다.
// 서버가 거절하면(도둑이 암살당한 직업을 골랐을 때) 복구가 차례를 끝낸다.
func (b *ctBrain) targetMove(s ctBotState, me ctBotPlayerView, thief bool) *CTMessage {
	order := ctAssassinOrder
	if thief {
		order = ctThiefOrder
	} else if leader := s.leader(); leader.Seat >= 0 &&
		len(leader.Built) >= CTBuildTarget-ctBotLeadGap {
		order = ctAssassinLeadOrder
	}

	removed := s.removedSet()
	pick := 0
	skipped := 0
	for _, role := range order {
		if removed[role] || role == s.YourRole {
			continue
		}
		if skipped < b.abilityTry {
			skipped++
			continue
		}
		pick = role
		break
	}
	if pick == 0 {
		return &CTMessage{Type: CTMsgEndTurn}
	}
	b.abilityTry++
	return &CTMessage{Type: CTMsgAbility, Payload: CTAbilityPayload{TargetRole: pick}}
}

// magicianMove 손패가 마르면 최다 손패와 통째로 바꾸고, 아니면 못 쓸 카드를
// 버려 새로 뽑는다. 둘 다 아니면 능력을 쓰지 않는다.
func (b *ctBrain) magicianMove(s ctBotState, me ctBotPlayerView) *CTMessage {
	hand := len(s.YourHand)
	bestSeat, bestHand := -1, hand+1
	for _, p := range s.Players {
		if p.Seat == me.Seat {
			continue
		}
		if p.HandCount > bestHand {
			bestSeat, bestHand = p.Seat, p.HandCount
		}
	}
	if bestSeat >= 0 && bestHand-hand >= 2 {
		seat := bestSeat
		return &CTMessage{Type: CTMsgAbility, Payload: CTAbilityPayload{TargetSeat: &seat}}
	}

	// 이미 지은 이름이라 영영 못 쓰는 카드를 버린다
	drop := []int{}
	for i, c := range s.YourHand {
		if ctHasName(me.Built, c.Name) {
			drop = append(drop, i)
		}
	}
	if len(drop) > 0 {
		return &CTMessage{Type: CTMsgAbility, Payload: CTAbilityPayload{Discard: drop}}
	}
	return &CTMessage{Type: CTMsgEndTurn}
}

// warlordMove 선두의 건물 하나를 부순다 — 값이 싼 것부터(비용 = 건물값-1).
// 주교(공개된)와 도시를 완성한 좌석은 건드릴 수 없다.
func (b *ctBrain) warlordMove(s ctBotState, me ctBotPlayerView) *CTMessage {
	type victim struct {
		seat   int
		cardID int
		cost   int
		built  int
	}
	best := victim{seat: -1}
	for _, p := range s.Players {
		if p.Seat == me.Seat || len(p.Built) < ctBotWarlordMin {
			continue
		}
		if p.RoleRevealed == CTRoleBishop && !p.Killed {
			continue
		}
		if len(p.Built) >= CTBuildTarget {
			continue
		}
		cards := append([]CTCard{}, p.Built...)
		sort.SliceStable(cards, func(i, j int) bool { return cards[i].Cost < cards[j].Cost })
		for _, c := range cards {
			cost := c.Cost - 1
			if cost < 0 {
				cost = 0
			}
			if cost > me.Gold {
				continue
			}
			if best.seat < 0 || len(p.Built) > best.built ||
				(len(p.Built) == best.built && cost < best.cost) {
				best = victim{seat: p.Seat, cardID: c.ID, cost: cost, built: len(p.Built)}
			}
			break // 그 좌석에서는 가장 싼 것 하나만 후보로
		}
	}
	if best.seat < 0 {
		return &CTMessage{Type: CTMsgEndTurn}
	}
	seat := best.seat
	return &CTMessage{Type: CTMsgAbility,
		Payload: CTAbilityPayload{TargetSeat: &seat, CardID: best.cardID}}
}

// minInt 작은 쪽
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ==================== 봇 소환 ====================

// spawnCTBot 대기실의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *CTHub) spawnCTBot(room *ctRoom, name string) bool {
	bot := &CTClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runCTBot(bot)
	return true
}

// takeoverCTBot 유예 만료 좌석을 이어받는 연습봇 — 이름·좌석을 유지해
// 진행 중인 차례가 그대로 이어진다
func (h *CTHub) takeoverCTBot(room *ctRoom, seat int, name string) *CTClient {
	bot := &CTClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	bot.GameID = room.Game.ID
	bot.Seat = seat
	h.runCTBot(bot)
	return bot
}

// runCTBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *CTHub) runCTBot(bot *CTClient) {
	brain := newCTBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m CTMessage) { h.gameMessage <- CTGameMessage{Client: bot, Message: m} },
		func(m CTMessage) bool { return m.Type == CTMsgGameOver || m.Type == CTMsgSessionExpired })
}

// ctRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록용)
func ctRoomHasBot(room *ctRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
