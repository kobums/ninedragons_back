package server

import (
	"fmt"
	"math"
	"math/rand"
	"time"
)

// ==================== 뱅! 연습봇 ====================
//
// 스냅샷(bg_game_state)만 보고 반응한다. 봇도 사람과 같은 조건 — 자기 손패와
// 자기 역할만 알고, 남의 역할은 보안관(시작부터 공개)과 사망자만 안다.
//
// ── 역할별 목표 ─────────────────────────────────────────────────────────
//
// 이 게임의 봇 품질은 "누구를 쏘는가"로 갈린다. 관측 가능한 사실 셋으로
// 의심 장부를 쌓고(스냅샷의 pending 이 "누가 누구를 공격했다"를 그대로
// 알려준다), 역할별로 다른 점수를 매긴다.
//
//	atkSheriff[s]  s 가 보안관을 공격했다  → 무법자·배신자 의심
//	atkMe[s]       s 가 나를 공격했다      → 내 적
//	bySheriff[s]   보안관이 s 를 공격했다  → 보안관이 무법자로 본 좌석
//
//	보안관  나를 쏜 좌석을 우선 조준한다 (= 무법자·배신자)
//	부관    보안관을 쏜 좌석을 조준하고, 보안관은 절대 쏘지 않는다
//	무법자  보안관을 최우선 조준한다 (사거리 밖이면 나를 쏜 좌석)
//	배신자  둘만 남기 전까지는 보안관을 살려 두고 무법자를 친다.
//	        둘만 남으면 보안관을 노린다
//
// 조준 기록(Aims)은 테스트가 "역할별 목표를 실제로 노리는가"를 숫자로
// 재는 계측 지점이다.
//
// ── 차례의 순서 ─────────────────────────────────────────────────────────
//
//	① 뽑기 카드(웰스파고·역마차·잡화점) — 손패를 먼저 넓힌다
//	② 위급하면 맥주
//	③ 장비 (더 긴 무기 · 조준경 · 야생마 · 술통)
//	④ 감옥 → 주 대상
//	⑤ 강탈!·캣 벌로우 → 주 대상의 카드를 턴다
//	⑥ 뱅! → 주 대상 (사거리 안일 때, 차례당 1장 · 볼캐닉이면 무제한)
//	⑦ 결투 → 거리 무관이라 사거리 밖 대상에 쓴다
//	⑧ 기관총·인디언! → 뱅!이 닿지 않을 때의 광역기
//	⑨ 차례 종료
//
// 방어: 빗나감!이 있으면 대부분 쓴다 (체력이 낮으면 무조건). 결투의 뱅!은
// 언제나 받아친다 — 못 내면 그 자리에서 체력이 깎이기 때문이다.

// 봇이 "생각하는" 시간 (테스트에서 짧게 낮춘다)
var (
	bgBotDelay    = 700 * time.Millisecond
	bgBotJitterMs = 700
)

// 봇 조준 가중치 (밸런스 조정 손잡이 — 봇 품질 측정 테스트가 이 값을 읽는다)
var (
	// bgAimSheriff 무법자가 보안관에게 얹는 가산점 (최우선 목표)
	bgAimSheriff = 12.0
	// bgAimSuspect 의심 장부 1회당 가산점
	bgAimSuspect = 3.0
	// bgAimHostile 나를 공격한 횟수 1회당 가산점
	bgAimHostile = 2.5
	// bgAimFriend 아군으로 보이는 좌석에 얹는 감점
	bgAimFriend = 8.0
	// bgAimFinish 체력 1인 상대를 마무리할 때의 가산점
	bgAimFinish = 2.0
	// bgAimDistance 거리 1당 감점 (가까운 쪽을 선호)
	bgAimDistance = 0.6
	// bgMissKeepHP 이 체력 이하면 빗나감!을 무조건 쓴다
	bgMissKeepHP = 2
	// bgMissUseRate 여유가 있을 때 빗나감!을 쓸 확률
	bgMissUseRate = 0.85
	// bgIndiansUseRate 여유가 있을 때 인디언!에 뱅!을 버릴 확률
	bgIndiansUseRate = 0.7
	// bgAreaMinFoes 기관총·인디언!을 쓰기 위한 최소 생존 상대 수
	bgAreaMinFoes = 2
)

// bgCardValue 손패 정리·잡화점 선택에 쓰는 카드 가치
var bgCardValue = map[BGKind]float64{
	BGBang: 3.4, BGMiss: 3.6, BGBeer: 2.8, BGSaloon: 2.0,
	BGDuel: 3.0, BGGatling: 3.8, BGIndians: 3.0,
	BGStagecoach: 2.6, BGWellsFargo: 3.2, BGStore: 2.4,
	BGCatBalou: 2.7, BGPanic: 2.9,
	BGBarrel: 3.5, BGJail: 2.2, BGDynamite: 0.4,
	BGMustang: 3.1, BGScope: 3.1,
	BGSchofield: 3.0, BGRemington: 3.6, BGCarabine: 3.9,
	BGWinchester: 4.1, BGVolcanic: 4.3,
}

// bgValueOf 카드 가치 (미등록은 1.0)
func bgValueOf(c BGCard) float64 {
	if v, ok := bgCardValue[c.Kind]; ok {
		return v
	}
	return 1.0
}

// bgBotPlayerView 봇이 스냅샷에서 꺼내 쓰는 좌석 정보
type bgBotPlayerView struct {
	Seat            int      `json:"seat"`
	Alive           bool     `json:"alive"`
	HP              int      `json:"hp"`
	MaxHP           int      `json:"maxHp"`
	HandCount       int      `json:"handCount"`
	Equipment       []BGCard `json:"equipment"`
	Role            BGRole   `json:"role"`
	DistanceFromYou int      `json:"distanceFromYou"`
}

// bgBotState 봇이 상태 스냅샷에서 꺼내 쓰는 최소 정보
type bgBotState struct {
	YourSeat    int               `json:"yourSeat"`
	Phase       BGPhase           `json:"phase"`
	CurrentSeat int               `json:"currentSeat"`
	DeckLeft    int               `json:"deckLeft"`
	Pending     *BGPending        `json:"pending"`
	StoreCards  []BGCard          `json:"storeCards"`
	YourRole    BGRole            `json:"yourRole"`
	YourHand    []BGCard          `json:"yourHand"`
	Players     []bgBotPlayerView `json:"players"`

	// YourBangUsed 서버가 내려주는 뱅! 제한 판정 (볼캐닉 반영).
	// 있으면 이 값이 우선이고, 없으면 두뇌의 로컬 장부로 폴백한다.
	YourBangUsed *bool `json:"yourBangUsed"`
}

// me 내 좌석 정보 (없으면 Seat -1)
func (s bgBotState) me() bgBotPlayerView {
	for _, p := range s.Players {
		if p.Seat == s.YourSeat {
			return p
		}
	}
	return bgBotPlayerView{Seat: -1}
}

// seat 좌석 정보 (없으면 Seat -1)
func (s bgBotState) seat(n int) bgBotPlayerView {
	for _, p := range s.Players {
		if p.Seat == n {
			return p
		}
	}
	return bgBotPlayerView{Seat: -1}
}

// sheriffSeat 공개된 보안관 좌석 (-1 없음)
func (s bgBotState) sheriffSeat() int {
	for _, p := range s.Players {
		if p.Role == BGRoleSheriff {
			return p.Seat
		}
	}
	return -1
}

// aliveCount 생존자 수
func (s bgBotState) aliveCount() int {
	n := 0
	for _, p := range s.Players {
		if p.Alive {
			n++
		}
	}
	return n
}

// findHand 손패에서 그 종류의 첫 인덱스 (-1 없음)
func (s bgBotState) findHand(kind BGKind) int {
	for i, c := range s.YourHand {
		if c.Kind == kind {
			return i
		}
	}
	return -1
}

// myRange 내 무기 사거리
func (s bgBotState) myRange() int {
	me := s.me()
	r := BGDefaultRange
	for _, c := range me.Equipment {
		if d, ok := bgDef(c.Kind); ok && d.Slot == bgSlotWeapon && d.Range > 0 {
			r = d.Range
		}
	}
	return r
}

// hasEquip 내가 그 장비 칸을 채우고 있는가
func (v bgBotPlayerView) hasEquip(slot string) bool {
	for _, c := range v.Equipment {
		if d, ok := bgDef(c.Kind); ok && d.Slot == slot {
			return true
		}
	}
	return false
}

// stateKey 같은 대기 상태에서 두 번 행동하지 않기 위한 식별키
func (s bgBotState) stateKey() string {
	key := fmt.Sprintf("%s|%d|%d", s.Phase, s.CurrentSeat, s.DeckLeft)
	if s.Pending != nil {
		key += fmt.Sprintf("|%s:%d:%d:%d",
			s.Pending.Kind, s.Pending.BySeat, s.Pending.TargetSeat, len(s.Pending.Passed))
	}
	for _, p := range s.Players {
		key += fmt.Sprintf(",%d:%v:%d:%d:%d", p.Seat, p.Alive, p.HP, p.HandCount, len(p.Equipment))
	}
	for _, c := range s.YourHand {
		key += fmt.Sprintf(".%d", c.ID)
	}
	for _, c := range s.StoreCards {
		key += fmt.Sprintf("/%d", c.ID)
	}
	return key
}

// bgAim 봇이 고른 공격 대상 한 건 (봇 품질 계측용 — 테스트가 읽는다)
type bgAim struct {
	Kind BGKind
	Role BGRole // 조준한 봇의 역할
	From int
	To   int
}

// bgBrain 스냅샷 기반 판단 (봇 대체 좌석도 같은 두뇌를 쓴다)
type bgBrain struct {
	rng *rand.Rand
	// lastKey 마지막으로 행동한 대기 상태의 식별키 (중복 행동 방지)
	lastKey string

	// ---- 의심 장부 (관측 가능한 공격만으로 쌓는다) ----
	atkSheriff map[int]int
	atkMe      map[int]int
	bySheriff  map[int]int
	// seenPending 같은 대응 창을 두 번 세지 않기 위한 지문
	seenPending map[string]bool

	// bangUsed 이번 차례에 낸 뱅! 수 (스냅샷에 없어 스스로 센다)
	bangUsed int
	// myTurn 직전 스냅샷에서 내 차례였는가 (차례 전환 감지 → bangUsed 초기화)
	myTurn bool
	// errGuard 거절 복구를 차례당 한 번으로 묶는 표식
	errGuard string
	// phase / pendingMine 마지막 스냅샷의 단계와 내 응답 차례 여부
	phase       BGPhase
	pendingMine bool

	// Aims 조준 기록 (계측 전용 — 게임 진행에는 쓰이지 않는다)
	Aims []bgAim
}

func newBGBrain() *bgBrain {
	return &bgBrain{
		rng:         rand.New(rand.NewSource(time.Now().UnixNano())),
		atkSheriff:  map[int]int{},
		atkMe:       map[int]int{},
		bySheriff:   map[int]int{},
		seenPending: map[string]bool{},
	}
}

// decide 공용 러너 계약 — bg_game_state 와 bg_error 에만 반응한다
func (b *bgBrain) decide(msg BGMessage) *BGMessage {
	switch msg.Type {
	case BGMsgGameState:
		state, ok := botPayloadAs[bgBotState](msg.Payload)
		if !ok {
			return nil
		}
		return b.decideState(state)
	case BGMsgError:
		return b.recover()
	}
	return nil
}

// recover 서버가 거절했을 때의 복구. 대응 중이면 포기하고, 차례 중이면
// 차례를 끝낸다 (차례당 한 번만 — 무한 왕복 방지).
func (b *bgBrain) recover() *BGMessage {
	if b.pendingMine && b.phase == BGPhaseRespond {
		return &BGMessage{Type: BGMsgRespond, Payload: BGRespondPayload{}}
	}
	if b.pendingMine && b.phase == BGPhaseStorePick {
		return &BGMessage{Type: BGMsgPick, Payload: BGPickPayload{Index: 0}}
	}
	if !b.myTurn {
		return nil
	}
	if b.errGuard == string(b.phase) {
		return nil
	}
	b.errGuard = string(b.phase)
	if b.phase == BGPhaseTurn {
		return &BGMessage{Type: BGMsgEndTurn}
	}
	return nil
}

// think 사람처럼 잠깐 뜸을 들인다 (테스트에서는 var 를 낮춰 즉시 진행한다)
func (b *bgBrain) think() {
	d := bgBotDelay
	if bgBotJitterMs > 0 {
		d += time.Duration(b.rng.Intn(bgBotJitterMs)) * time.Millisecond
	}
	if d > 0 {
		time.Sleep(d)
	}
}

// decideState 내 차례(또는 내 대응 차례)면 정확히 한 수를 결정한다
func (b *bgBrain) decideState(s bgBotState) *BGMessage {
	me := s.me()
	if me.Seat < 0 {
		return nil
	}
	b.observe(s)

	wasMyTurn := b.myTurn
	b.phase = s.Phase
	b.myTurn = s.CurrentSeat == me.Seat
	b.pendingMine = s.Pending != nil && s.Pending.TargetSeat == me.Seat
	if b.myTurn && !wasMyTurn { // 새 차례 — 뱅! 장부 초기화
		b.bangUsed = 0
		b.errGuard = ""
	}

	var mine bool
	switch s.Phase {
	case BGPhaseTurn, BGPhaseDiscard:
		mine = b.myTurn
	case BGPhaseRespond, BGPhaseStorePick:
		mine = b.pendingMine
	}
	if !mine || !me.Alive {
		return nil
	}

	key := s.stateKey()
	if b.lastKey == key {
		return nil
	}
	b.lastKey = key

	var move *BGMessage
	switch s.Phase {
	case BGPhaseTurn:
		move = b.turnMove(s, me)
	case BGPhaseRespond:
		move = b.respondMove(s, me)
	case BGPhaseStorePick:
		move = b.pickMove(s)
	case BGPhaseDiscard:
		move = b.discardMove(s, me)
	}
	if move == nil && s.Phase == BGPhaseTurn { // 방어선 — 판은 굴러가야 한다
		move = &BGMessage{Type: BGMsgEndTurn}
	}
	if move != nil {
		b.think()
	}
	return move
}

// ==================== 의심 장부 ====================

// observe 스냅샷의 pending 에서 "누가 누구를 공격했다"를 읽어 장부에 쌓는다.
// 같은 창을 두 번 세지 않도록 지문으로 걸러낸다.
func (b *bgBrain) observe(s bgBotState) {
	pd := s.Pending
	if pd == nil || pd.Kind == BGPendStore {
		return
	}
	sheriff := s.sheriffSeat()
	fp := fmt.Sprintf("%s:%d:%d:%d", pd.Kind, pd.BySeat, pd.TargetSeat, len(pd.Passed))
	if b.seenPending[fp] {
		return
	}
	b.seenPending[fp] = true
	if pd.BySeat == pd.TargetSeat {
		return
	}
	if pd.TargetSeat == sheriff && sheriff >= 0 {
		b.atkSheriff[pd.BySeat]++
	}
	if pd.TargetSeat == s.YourSeat {
		b.atkMe[pd.BySeat]++
	}
	if pd.BySeat == sheriff && sheriff >= 0 {
		b.bySheriff[pd.TargetSeat]++
	}
}

// ==================== 조준 ====================

// targetScore 역할별 목표를 점수로 옮긴다. -Inf 는 "절대 고르지 않는다".
func (b *bgBrain) targetScore(s bgBotState, me, t bgBotPlayerView) float64 {
	if !t.Alive || t.Seat == me.Seat {
		return math.Inf(-1)
	}
	sheriff := s.sheriffSeat()
	score := 0.0

	switch s.YourRole {
	case BGRoleOutlaw:
		// 목표는 오직 보안관. 사거리 밖이면 나를 쏜 좌석(부관 의심)이 다음이다.
		if t.Seat == sheriff {
			score += bgAimSheriff
		}
		score += float64(b.atkMe[t.Seat]) * bgAimHostile

	case BGRoleDeputy:
		// 보안관은 절대 쏘지 않는다. 보안관을 쏜 좌석이 최우선.
		if t.Seat == sheriff {
			return math.Inf(-1)
		}
		score += float64(b.atkSheriff[t.Seat]) * bgAimSuspect
		score += float64(b.bySheriff[t.Seat]) * bgAimSuspect
		score += float64(b.atkMe[t.Seat]) * bgAimHostile

	case BGRoleSheriff:
		// 나를 쏜 좌석이 곧 무법자·배신자다.
		score += float64(b.atkMe[t.Seat]) * bgAimHostile * 2
		score += float64(b.atkSheriff[t.Seat]) * bgAimSuspect

	case BGRoleRenegade:
		// 둘만 남기 전까지는 보안관을 살려 두고 무법자를 친다.
		if s.aliveCount() <= 2 {
			if t.Seat == sheriff {
				score += bgAimSheriff
			} else {
				score -= bgAimFriend
			}
		} else {
			if t.Seat == sheriff {
				score -= bgAimFriend
			}
			score += float64(b.atkSheriff[t.Seat]) * bgAimSuspect
			score += float64(b.bySheriff[t.Seat]) * bgAimSuspect
			score += float64(b.atkMe[t.Seat]) * bgAimHostile
		}
	}

	if t.HP == 1 {
		score += bgAimFinish
	}
	if t.DistanceFromYou > 0 {
		score -= float64(t.DistanceFromYou) * bgAimDistance
	}
	return score
}

// primaryTarget 지금 가장 치고 싶은 좌석 (-1 없음).
// within 이 0보다 크면 그 거리 안의 좌석만 고른다.
func (b *bgBrain) primaryTarget(s bgBotState, within int) int {
	me := s.me()
	best, bestScore := -1, math.Inf(-1)
	for _, t := range s.Players {
		if within > 0 && (t.DistanceFromYou < 0 || t.DistanceFromYou > within) {
			continue
		}
		score := b.targetScore(s, me, t)
		if math.IsInf(score, -1) {
			continue
		}
		score += b.rng.Float64() * 0.4 // 같은 점수를 흔든다
		if best < 0 || score > bestScore {
			best, bestScore = t.Seat, score
		}
	}
	return best
}

// aim 조준을 기록하고 그 좌석을 돌려준다 (계측 지점)
func (b *bgBrain) aim(s bgBotState, kind BGKind, to int) int {
	b.Aims = append(b.Aims, bgAim{Kind: kind, Role: s.YourRole, From: s.YourSeat, To: to})
	return to
}

// ==================== ① 차례 ====================

func (b *bgBrain) turnMove(s bgBotState, me bgBotPlayerView) *BGMessage {
	play := func(i int) *BGMessage {
		return &BGMessage{Type: BGMsgPlay, Payload: BGPlayPayload{Index: i}}
	}
	playAt := func(i, seat int) *BGMessage {
		t := seat
		return &BGMessage{Type: BGMsgPlay, Payload: BGPlayPayload{Index: i, TargetSeat: &t}}
	}
	playCard := func(i, seat, cardIdx int) *BGMessage {
		t, c := seat, cardIdx
		return &BGMessage{Type: BGMsgPlay,
			Payload: BGPlayPayload{Index: i, TargetSeat: &t, TargetCardIndex: &c}}
	}

	// ① 손패를 먼저 넓힌다
	for _, kind := range []BGKind{BGWellsFargo, BGStagecoach, BGStore} {
		if i := s.findHand(kind); i >= 0 {
			return play(i)
		}
	}

	// ② 위급하면 맥주 (둘만 남으면 효과가 없으니 쓰지 않는다)
	if me.HP <= 1 && s.aliveCount() > 2 {
		if i := s.findHand(BGBeer); i >= 0 {
			return play(i)
		}
	}

	// ③ 장비
	if mv := b.equipMove(s, me); mv != nil {
		return mv
	}

	target := b.primaryTarget(s, 0)
	sheriff := s.sheriffSeat()

	// ④ 감옥 — 보안관은 가둘 수 없다
	if i := s.findHand(BGJail); i >= 0 {
		jailTarget := target
		if jailTarget == sheriff {
			jailTarget = b.bestOther(s, sheriff)
		}
		if jailTarget >= 0 && !s.seat(jailTarget).hasEquip(bgSlotJail) {
			return playAt(i, b.aim(s, BGJail, jailTarget))
		}
	}

	// ⑤ 강탈!(거리 1) · 캣 벌로우(거리 무관) — 위협적인 장비를 먼저 턴다
	if i := s.findHand(BGPanic); i >= 0 {
		if seat, cardIdx := b.robTarget(s, 1); seat >= 0 {
			return playCard(i, b.aim(s, BGPanic, seat), cardIdx)
		}
	}
	if i := s.findHand(BGCatBalou); i >= 0 {
		if seat, cardIdx := b.robTarget(s, 0); seat >= 0 {
			return playCard(i, b.aim(s, BGCatBalou, seat), cardIdx)
		}
	}

	// ⑥ 뱅! — 사거리 안에서만
	if i := s.findHand(BGBang); i >= 0 && b.bangLeft(s, me) {
		if seat := b.primaryTarget(s, s.myRange()); seat >= 0 {
			b.bangUsed++
			return playAt(i, b.aim(s, BGBang, seat))
		}
	}

	// ⑦ 결투 — 거리 무관이라 사거리 밖 대상에 쓴다
	if i := s.findHand(BGDuel); i >= 0 && target >= 0 {
		if s.findHand(BGBang) >= 0 || me.HP >= 3 {
			return playAt(i, b.aim(s, BGDuel, target))
		}
	}

	// ⑧ 광역기 — 뱅!이 닿지 않을 때만 (아군도 함께 맞는다)
	foes := s.aliveCount() - 1
	if foes >= bgAreaMinFoes {
		if i := s.findHand(BGGatling); i >= 0 {
			return play(i)
		}
		if i := s.findHand(BGIndians); i >= 0 && me.HP >= 2 {
			return play(i)
		}
	}
	if i := s.findHand(BGSaloon); i >= 0 && me.HP >= me.MaxHP {
		// 나만 못 받는 카드라 체력이 가득할 때 손패 정리 겸 흘려보낸다
		if s.aliveCount() <= 2 {
			return play(i)
		}
	}

	return &BGMessage{Type: BGMsgEndTurn}
}

// bangLeft 이번 차례에 뱅!을 더 낼 수 있는가.
// 서버가 yourBangUsed 를 실어 주면 그 판정을 그대로 믿고,
// 없으면 볼캐닉 + 로컬 장부로 스스로 센다.
func (b *bgBrain) bangLeft(s bgBotState, me bgBotPlayerView) bool {
	if s.YourBangUsed != nil {
		return !*s.YourBangUsed
	}
	for _, c := range me.Equipment {
		if d, ok := bgDef(c.Kind); ok && d.Unlimited {
			return true
		}
	}
	return b.bangUsed < 1
}

// bestOther 특정 좌석을 뺀 최선의 대상
func (b *bgBrain) bestOther(s bgBotState, skip int) int {
	me := s.me()
	best, bestScore := -1, math.Inf(-1)
	for _, t := range s.Players {
		if t.Seat == skip {
			continue
		}
		score := b.targetScore(s, me, t)
		if math.IsInf(score, -1) {
			continue
		}
		if best < 0 || score > bestScore {
			best, bestScore = t.Seat, score
		}
	}
	return best
}

// robTarget 강탈!·캣 벌로우가 노릴 좌석과 카드 자리.
// within 0 은 거리 무관. 장비를 든 상대의 장비 칸을 먼저 노리고,
// 없으면 손패 첫 장을 눈감고 집는다.
func (b *bgBrain) robTarget(s bgBotState, within int) (int, int) {
	me := s.me()
	best, bestScore, bestIdx := -1, math.Inf(-1), 0
	for _, t := range s.Players {
		if within > 0 && (t.DistanceFromYou < 0 || t.DistanceFromYou > within) {
			continue
		}
		if t.HandCount+len(t.Equipment) == 0 {
			continue
		}
		score := b.targetScore(s, me, t)
		if math.IsInf(score, -1) {
			continue
		}
		idx := 0
		for i, c := range t.Equipment { // 무기·술통 같은 위협을 먼저 턴다
			if d, ok := bgDef(c.Kind); ok && (d.Slot == bgSlotWeapon || d.Slot == bgSlotBarrel) {
				idx = t.HandCount + i
				score += 2
				break
			}
		}
		if best < 0 || score > bestScore {
			best, bestScore, bestIdx = t.Seat, score, idx
		}
	}
	return best, bestIdx
}

// equipMove 장비를 갖춘다 — 더 긴 무기 · 조준경 · 야생마 · 술통 순.
// 다이너마이트는 스스로 지지 않는다 (손패 상한에 걸리면 버려진다).
func (b *bgBrain) equipMove(s bgBotState, me bgBotPlayerView) *BGMessage {
	play := func(i int) *BGMessage {
		return &BGMessage{Type: BGMsgPlay, Payload: BGPlayPayload{Index: i}}
	}
	cur := s.myRange()
	bestWeapon, bestRange := -1, cur
	for i, c := range s.YourHand {
		d, ok := bgDef(c.Kind)
		if !ok || d.Slot != bgSlotWeapon {
			continue
		}
		if d.Range > bestRange || (d.Unlimited && d.Range >= bestRange) {
			bestWeapon, bestRange = i, d.Range
		}
	}
	if bestWeapon >= 0 {
		return play(bestWeapon)
	}
	for _, kind := range []BGKind{BGScope, BGMustang, BGBarrel} {
		d, _ := bgDef(kind)
		if me.hasEquip(d.Slot) {
			continue
		}
		if i := s.findHand(kind); i >= 0 {
			return play(i)
		}
	}
	return nil
}

// ==================== ② 대응 창 ====================

func (b *bgBrain) respondMove(s bgBotState, me bgBotPlayerView) *BGMessage {
	pd := s.Pending
	if pd == nil {
		return nil
	}
	pass := &BGMessage{Type: BGMsgRespond, Payload: BGRespondPayload{}}
	i := s.findHand(BGKind(pd.Need))
	if i < 0 {
		return pass
	}
	use := &BGMessage{Type: BGMsgRespond, Payload: BGRespondPayload{Index: &i}}

	// 결투에서 못 내면 그 자리에서 체력이 깎인다 — 언제나 받아친다
	if pd.Kind == BGPendDuel {
		return use
	}
	if me.HP <= bgMissKeepHP {
		return use
	}
	rate := bgMissUseRate
	if pd.Kind == BGPendIndians {
		rate = bgIndiansUseRate
	}
	if b.rng.Float64() < rate {
		return use
	}
	return pass
}

// ==================== ③ 잡화점 ====================

func (b *bgBrain) pickMove(s bgBotState) *BGMessage {
	if len(s.StoreCards) == 0 {
		return nil
	}
	best, bestScore := 0, math.Inf(-1)
	for i, c := range s.StoreCards {
		score := bgValueOf(c) + b.rng.Float64()*0.2
		if i == 0 || score > bestScore {
			best, bestScore = i, score
		}
	}
	return &BGMessage{Type: BGMsgPick, Payload: BGPickPayload{Index: best}}
}

// ==================== ④ 손패 줄이기 ====================

// discardMove 가치가 낮은 카드부터 버린다 (정확히 초과분만큼)
func (b *bgBrain) discardMove(s bgBotState, me bgBotPlayerView) *BGMessage {
	need := len(s.YourHand) - me.HP
	if need <= 0 {
		return &BGMessage{Type: BGMsgDiscard, Payload: BGDiscardPayload{Indexes: []int{}}}
	}
	type ranked struct {
		idx   int
		value float64
	}
	list := make([]ranked, 0, len(s.YourHand))
	for i, c := range s.YourHand {
		list = append(list, ranked{idx: i, value: bgValueOf(c)})
	}
	for i := 1; i < len(list); i++ { // 가치 오름차순 (삽입 정렬 — 손패는 짧다)
		for j := i; j > 0 && list[j].value < list[j-1].value; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
	idx := make([]int, 0, need)
	for i := 0; i < need && i < len(list); i++ {
		idx = append(idx, list[i].idx)
	}
	return &BGMessage{Type: BGMsgDiscard, Payload: BGDiscardPayload{Indexes: idx}}
}

// ==================== 봇 소환 ====================

// spawnBGBot 대기실의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *BGHub) spawnBGBot(room *bgRoom, name string) bool {
	bot := &BGClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runBGBot(bot)
	return true
}

// takeoverBGBot 유예 만료 좌석을 이어받는 연습봇 — 이름·좌석을 유지해
// 진행 중인 차례가 그대로 이어진다
func (h *BGHub) takeoverBGBot(room *bgRoom, seat int, name string) *BGClient {
	bot := &BGClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	bot.GameID = room.Game.ID
	bot.Seat = seat
	h.runBGBot(bot)
	return bot
}

// runBGBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *BGHub) runBGBot(bot *BGClient) {
	brain := newBGBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m BGMessage) { h.gameMessage <- BGGameMessage{Client: bot, Message: m} },
		func(m BGMessage) bool { return m.Type == BGMsgGameOver || m.Type == BGMsgSessionExpired })
}

// bgRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록용)
func bgRoomHasBot(room *bgRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
