package server

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// ==================== 뱅! 순수 규칙 ====================
//
// 클라이언트·타이머를 모르며, 허브(bg_hub.go)가 단계 마감(차례 60초 · 대응
// 20초 · 잡화점 15초 · 손패 줄이기 15초)을 걸고 이벤트 큐(DrainEvents)를
// 방송한다.
//
// ── 상태기계 ────────────────────────────────────────────────────────────
//
//	waiting ──Start()──▶ draw(보안관 좌석부터)
//
//	draw        차례 시작 판정. 순서가 고정돼 있고 사람의 선택이 없어 한
//	            호출 안에서 끝난다 (클라이언트는 이 사이의 뒤집기를 bg_event
//	            로 본다).
//	              ① 다이너마이트 — 뒤집어 ♠2~9 면 체력 −3 + 폐기,
//	                 아니면 왼쪽 사람에게 넘긴다
//	              ② 감옥 — 뒤집어 ♥ 면 탈출(계속), 아니면 차례 통째 건너뜀
//	              ③ 카드 2장 뽑기  ─▶ turn
//
//	turn        bg_play 로 원하는 만큼 카드 사용. bg_end_turn ─▶
//	            손패 > 체력이면 discard, 아니면 다음 차례(draw)
//
//	respond     대응 창. pending{kind,bySeat,targetSeat,need,passed} 가 열려
//	            있고 targetSeat 만 bg_respond 할 수 있다. 기관총·인디언!은
//	            큐를 돌며 한 명씩, 결투는 두 사람이 교대한다.
//	            큐가 비면 ─▶ turn (차례 주인이 죽었으면 다음 차례)
//
//	store_pick  잡화점. 인원수만큼 공개하고 카드를 낸 사람부터 한 장씩 고른다.
//	            pending.kind="store" · need="pick" 으로 같은 창을 재활용한다.
//
//	discard     손패를 체력 수만큼으로 줄인다 (bg_discard) ─▶ 다음 차례
//
//	game_over   보안관 사망 → 배신자가 마지막 1인이면 배신자 승, 아니면
//	            무법자 승. 무법자·배신자 전멸 → 보안관·부관 승.
//
// ── 카드별 처리 표 ──────────────────────────────────────────────────────
//
// 카드 종류가 많아 효과를 switch 로 흩지 않는다. 아래 표의 한 줄이 곧
// bgPlayTable 의 한 항목이고, 대상 검증은 bgCards(bg_types.go)의 Target
// 규칙 하나로 끝난다. Check 는 "낼 수 있는가", Apply 는 "효과 본문"이다.
//
//	kind        이름표        대상            처리
//	──────────  ────────────  ──────────────  ────────────────────────────
//	bang        뱅!           사거리 안 1명   차례당 1장(볼캐닉이면 무제한).
//	                                          술통 자동 판정 → 대응 창(빗나감!)
//	                                          → 못 막으면 체력 −1
//	miss        빗나감!       —               차례에는 낼 수 없다 (대응 전용)
//	beer        맥주          자신            체력 +1 (최대치까지).
//	                                          생존 2인이면 효과 없음
//	saloon      주점          —               자신 제외 전원 체력 +1
//	duel        결투          거리 무관 1명   뱅!을 교대로, 못 내는 쪽 체력 −1
//	gatling     기관총        —               나머지 전원에게 뱅! (술통·빗나감!)
//	indians     인디언!       —               나머지 전원 뱅! 1장 버리기 or −1
//	stagecoach  역마차        —               2장 뽑기
//	wellsfargo  웰스파고      —               3장 뽑기
//	store       잡화점        —               인원수만큼 공개, 차례로 1장씩
//	catbalou    캣 벌로우     거리 무관 1명   대상 카드 1장 버리게 함
//	panic       강탈!         거리 1 이내     대상 카드 1장 뺏기
//	barrel      술통          자신(장비)      뱅!을 받을 때 뒤집어 ♥면 회피
//	jail        감옥          보안관 아닌 타인 그 사람 차례에 ♥면 탈출,
//	                                          아니면 차례 통째 건너뜀
//	dynamite    다이너마이트  자신(장비)      차례 시작에 ♠2~9면 −3, 아니면 왼쪽
//	mustang     야생마        자신(장비)      남이 나를 볼 때 거리 +1
//	scope       조준경        자신(장비)      내가 남을 볼 때 거리 −1
//	schofield   스코필드      자신(무기)      사거리 2
//	remington   레밍턴        자신(무기)      사거리 3
//	carabine    카빈          자신(무기)      사거리 4
//	winchester  윈체스터      자신(무기)      사거리 5
//	volcanic    볼캐닉        자신(무기)      사거리 1, 뱅! 무제한
//
// ── 탈락 보상 ───────────────────────────────────────────────────────────
//
// 무법자를 죽인 사람은 카드 3장을 뽑는다. 보안관이 부관을 죽이면 보안관은
// 손패·장비를 전부 버린다 (eliminate 참고).

// ==================== 생성 / 대기실 ====================

// NewBGGame 대기 상태의 새 게임
func NewBGGame(id string) *BGGame {
	return &BGGame{
		ID:          id,
		Players:     []*BGPlayer{},
		Phase:       BGPhaseWaiting,
		Deck:        []BGCard{},
		DiscardPile: []BGCard{},
		StoreCards:  []BGCard{},
		CurrentSeat: -1,
	}
}

// AddPlayer 대기실 입장. 좌석 번호를 돌려준다.
func (g *BGGame) AddPlayer(name string) (int, error) {
	if g.Phase != BGPhaseWaiting {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= BGMaxPlayers {
		return -1, fmt.Errorf("자리가 없습니다 (최대 %d명)", BGMaxPlayers)
	}
	seat := len(g.Players)
	g.Players = append(g.Players, &BGPlayer{
		Seat:      seat,
		Name:      name,
		Hand:      []BGCard{},
		Equipment: []BGCard{},
	})
	return seat, nil
}

// RemovePlayer 대기실 퇴장. 좌석을 앞으로 당겨 빈틈을 없앤다.
func (g *BGGame) RemovePlayer(seat int) {
	if g.Phase != BGPhaseWaiting || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i, p := range g.Players {
		p.Seat = i
	}
}

// CanStart 시작 가능 여부 (호스트 명시 시작 — 4인부터)
func (g *BGGame) CanStart() bool {
	return g.Phase == BGPhaseWaiting && len(g.Players) >= BGMinPlayers
}

// player 좌석의 참가자 (없으면 nil)
func (g *BGGame) player(seat int) *BGPlayer {
	if seat < 0 || seat >= len(g.Players) {
		return nil
	}
	return g.Players[seat]
}

// ==================== 덱 ====================

// bgBuildDeck 구성표(bgCards)대로 80장을 만든다. id 는 1부터 이어 붙이고,
// 무늬·숫자는 무늬 4종을 고르게 돌려 붙인다 — 뒤집기 판정(♥ 회피 25% ·
// ♠2~9 폭발 약 17%)이 실제 게임과 비슷한 확률을 갖도록.
func bgBuildDeck() []BGCard {
	cards := make([]BGCard, 0, BGDeckSize)
	id := 0
	for _, def := range bgCards {
		for i := 0; i < def.Count; i++ {
			cards = append(cards, BGCard{
				ID:   id + 1,
				Kind: def.Kind,
				Suit: bgSuits[id%len(bgSuits)],
				Rank: bgRanks[(id/len(bgSuits))%len(bgRanks)],
			})
			id++
		}
	}
	return cards
}

// reshuffle 덱이 마르면 버린 더미를 되섞어 되돌린다
func (g *BGGame) reshuffle() {
	if len(g.DiscardPile) == 0 {
		return
	}
	pile := g.DiscardPile
	g.DiscardPile = []BGCard{}
	if g.rng != nil {
		g.rng.Shuffle(len(pile), func(i, j int) { pile[i], pile[j] = pile[j], pile[i] })
	}
	g.Deck = append(g.Deck, pile...)
	g.emit("reshuffle", -1, "덱이 떨어져 버린 더미를 다시 섞었습니다")
}

// drawOne 덱 맨 위 한 장 (덱도 버린 더미도 비면 false)
func (g *BGGame) drawOne() (BGCard, bool) {
	if len(g.Deck) == 0 {
		g.reshuffle()
	}
	if len(g.Deck) == 0 {
		return BGCard{}, false
	}
	c := g.Deck[0]
	g.Deck = g.Deck[1:]
	return c, true
}

// drawN 덱에서 n장 (모자라면 있는 만큼)
func (g *BGGame) drawN(n int) []BGCard {
	out := []BGCard{}
	for i := 0; i < n; i++ {
		c, ok := g.drawOne()
		if !ok {
			break
		}
		out = append(out, c)
	}
	return out
}

// drawTo 좌석의 손패로 n장 뽑는다
func (g *BGGame) drawTo(p *BGPlayer, n int) int {
	got := g.drawN(n)
	p.Hand = append(p.Hand, got...)
	return len(got)
}

// toDiscard 버린 더미 맨 위로
func (g *BGGame) toDiscard(cards ...BGCard) {
	for _, c := range cards {
		if c.Kind == "" {
			continue
		}
		g.DiscardPile = append(g.DiscardPile, c)
	}
}

// discardTop 버린 더미의 맨 위 (없으면 nil)
func (g *BGGame) discardTop() *BGCard {
	if len(g.DiscardPile) == 0 {
		return nil
	}
	c := g.DiscardPile[len(g.DiscardPile)-1]
	return &c
}

// takeHand 손패에서 i번째를 떼어낸다 (범위는 호출부가 검사한다)
func (g *BGGame) takeHand(p *BGPlayer, i int) BGCard {
	c := p.Hand[i]
	p.Hand = append(p.Hand[:i], p.Hand[i+1:]...)
	return c
}

// flip "뒤집기" — 덱 맨 위를 공개하고 버린 더미로 보낸다.
// 술통·감옥·다이너마이트 판정이 모두 이 한 함수를 쓴다.
func (g *BGGame) flip(seat int, reason string) BGCard {
	c, ok := g.drawOne()
	if !ok {
		return BGCard{}
	}
	g.toDiscard(c)
	g.emit("flip", seat, fmt.Sprintf("%s 판정 — %s", reason, bgCardText(c)))
	return c
}

// bgCardText 이벤트 문구용 카드 표기 — "뱅!(♥7)"
func bgCardText(c BGCard) string {
	if c.Kind == "" {
		return "카드 없음"
	}
	return fmt.Sprintf("%s(%s%s)", bgLabel(c.Kind), c.Suit, c.Rank)
}

// ==================== 거리 (이 게임의 핵심) ====================
//
// 순수 함수로 떼어 두고 표 기반 테스트가 고정한다. 원탁에는 생존자만
// 앉아 있고(탈락자는 자리에서 빠진다), 양방향 중 짧은 쪽이 기본 거리다.

// bgBaseDistance 생존 좌석 목록(오름차순)에서 from→to 의 양방향 최단 거리.
// 둘 중 하나라도 목록에 없으면 -1.
func bgBaseDistance(alive []int, from, to int) int {
	fi, ti := -1, -1
	for i, s := range alive {
		if s == from {
			fi = i
		}
		if s == to {
			ti = i
		}
	}
	if fi < 0 || ti < 0 {
		return -1
	}
	n := len(alive)
	d := fi - ti
	if d < 0 {
		d = -d
	}
	if n-d < d {
		d = n - d
	}
	return d
}

// bgDistance 장비 보정까지 얹은 거리.
//
//	기본  = 탈락자를 뺀 원탁의 양방향 최단
//	보정  = 대상의 야생마 +1 · 보는 쪽의 조준경 −1
//	하한  = 자기 자신은 0, 그 외에는 1 (보정이 0 이하로 내려가지 않는다)
func bgDistance(alive []int, from, to int, targetMustang, viewerScope bool) int {
	base := bgBaseDistance(alive, from, to)
	if base < 0 {
		return -1
	}
	if from == to {
		return 0
	}
	if targetMustang {
		base++
	}
	if viewerScope {
		base--
	}
	if base < 1 {
		base = 1
	}
	return base
}

// aliveSeats 생존 좌석 오름차순 (원탁의 현재 자리)
func (g *BGGame) aliveSeats() []int {
	out := []int{}
	for _, p := range g.Players {
		if p.Alive {
			out = append(out, p.Seat)
		}
	}
	return out
}

// aliveCount 생존자 수
func (g *BGGame) aliveCount() int { return len(g.aliveSeats()) }

// DistanceBetween from 이 to 를 보는 거리 (장비 보정 포함, 탈락자는 -1)
func (g *BGGame) DistanceBetween(from, to int) int {
	f, t := g.player(from), g.player(to)
	if f == nil || t == nil || !f.Alive || !t.Alive {
		return -1
	}
	return bgDistance(g.aliveSeats(), from, to,
		bgHasSlot(t, bgSlotMustang), bgHasSlot(f, bgSlotScope))
}

// nextAliveSeat seat 의 왼쪽(좌석 번호 증가 방향) 첫 생존자. 없으면 -1.
func (g *BGGame) nextAliveSeat(seat int) int {
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
	return -1
}

// aliveOrderFrom seat 부터 시작해 왼쪽으로 도는 생존 좌석 목록 (seat 포함)
func (g *BGGame) aliveOrderFrom(seat int) []int {
	out := []int{}
	n := len(g.Players)
	for i := 0; i < n; i++ {
		s := (seat + i) % n
		if g.Players[s].Alive {
			out = append(out, s)
		}
	}
	return out
}

// othersFrom seat 을 뺀 생존 좌석 목록 (왼쪽부터 — 기관총·인디언! 순서)
func (g *BGGame) othersFrom(seat int) []int {
	out := []int{}
	for _, s := range g.aliveOrderFrom(seat) {
		if s != seat {
			out = append(out, s)
		}
	}
	return out
}

// ==================== 장비 ====================

// bgHasSlot 그 장비 칸이 채워져 있는가
func bgHasSlot(p *BGPlayer, slot string) bool {
	if p == nil {
		return false
	}
	for _, c := range p.Equipment {
		if d, ok := bgDef(c.Kind); ok && d.Slot == slot {
			return true
		}
	}
	return false
}

// bgWeaponRange 무기 사거리 (무기가 없으면 기본 1)
func bgWeaponRange(p *BGPlayer) int {
	r := BGDefaultRange
	if p == nil {
		return r
	}
	for _, c := range p.Equipment {
		if d, ok := bgDef(c.Kind); ok && d.Slot == bgSlotWeapon && d.Range > 0 {
			r = d.Range
		}
	}
	return r
}

// bgUnlimitedBang 볼캐닉을 들고 있는가 (뱅! 장수 제한 해제)
func bgUnlimitedBang(p *BGPlayer) bool {
	if p == nil {
		return false
	}
	for _, c := range p.Equipment {
		if d, ok := bgDef(c.Kind); ok && d.Unlimited {
			return true
		}
	}
	return false
}

// removeSlot 장비 칸을 비우고 그 카드를 돌려준다
func (g *BGGame) removeSlot(p *BGPlayer, slot string) (BGCard, bool) {
	for i, c := range p.Equipment {
		if d, ok := bgDef(c.Kind); ok && d.Slot == slot {
			p.Equipment = append(p.Equipment[:i], p.Equipment[i+1:]...)
			return c, true
		}
	}
	return BGCard{}, false
}

// BangBlocked 그 좌석이 이번 차례에 뱅!을 더 낼 수 없는가.
// 스냅샷의 yourBangUsed 와 Play 의 검증이 이 한 함수를 공유한다 —
// 볼캐닉을 들었으면 몇 장을 내도 막히지 않는다.
func (g *BGGame) BangBlocked(seat int) bool {
	p := g.player(seat)
	if p == nil {
		return true
	}
	if bgUnlimitedBang(p) {
		return false
	}
	return p.BangUsed >= 1
}

// bgHandHas 손패에 그 종류가 있는가
func bgHandHas(p *BGPlayer, kind BGKind) bool {
	if p == nil {
		return false
	}
	for _, c := range p.Hand {
		if c.Kind == kind {
			return true
		}
	}
	return false
}

// ==================== 시작 ====================

// Start 역할·덱을 섞고 체력·손패를 세팅한 뒤 보안관 좌석의 차례를 연다
func (g *BGGame) Start(rng *rand.Rand) error {
	if !g.CanStart() {
		return fmt.Errorf("%d명 이상 모여야 시작할 수 있습니다", BGMinPlayers)
	}
	n := len(g.Players)
	setup, ok := bgRoleSetup[n]
	if !ok {
		return fmt.Errorf("%d인 구성은 지원하지 않습니다", n)
	}
	g.rng = rng

	roles := append([]BGRole{}, setup...)
	rng.Shuffle(len(roles), func(i, j int) { roles[i], roles[j] = roles[j], roles[i] })

	deck := bgBuildDeck()
	rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })
	g.Deck = deck
	g.DiscardPile = []BGCard{}
	g.StoreCards = []BGCard{}

	sheriff := -1
	for i, p := range g.Players {
		p.Role = roles[i]
		p.MaxHP = BGBaseHP
		if p.Role == BGRoleSheriff {
			p.MaxHP += BGSheriffBonusHP
			sheriff = i
		}
		p.HP = p.MaxHP
		p.Alive = true
		p.Equipment = []BGCard{}
		p.BangUsed = 0
		p.Hand = g.drawN(p.MaxHP)
	}
	if sheriff < 0 { // 방어선 — 구성표에 보안관이 반드시 있다
		sheriff = 0
	}

	g.CurrentSeat = sheriff
	g.Turns = 0
	g.Ready = true
	g.StartedAt = time.Now()
	g.emit("game_started", sheriff, fmt.Sprintf(
		"%d인전 시작 — 보안관은 %s님입니다. 무법자 %d명과 배신자 1명이 숨어 있습니다",
		n, g.Players[sheriff].Name, bgCountRole(setup, BGRoleOutlaw)))
	g.startTurn()
	return nil
}

// bgCountRole 구성표에서 그 역할의 수
func bgCountRole(setup []BGRole, role BGRole) int {
	n := 0
	for _, r := range setup {
		if r == role {
			n++
		}
	}
	return n
}

// ==================== 차례 ====================

// startTurn 차례 시작 — 다이너마이트·감옥 판정 후 2장 뽑기.
// 사람의 선택이 끼지 않으므로 draw 단계는 한 호출 안에서 끝난다.
func (g *BGGame) startTurn() {
	if g.Phase == BGPhaseGameOver {
		return
	}
	g.Turns++
	if g.Turns > BGMaxTurns {
		g.finishByTurnLimit()
		return
	}
	p := g.player(g.CurrentSeat)
	if p == nil || !p.Alive {
		g.nextTurn()
		return
	}
	p.BangUsed = 0
	g.Pending = nil
	g.Phase = BGPhaseDraw
	g.StateSeq++
	g.emit("turn_start", p.Seat, fmt.Sprintf("%s님의 차례입니다", p.Name))

	// ① 다이너마이트
	if bgHasSlot(p, bgSlotDynamite) {
		c := g.flip(p.Seat, "다이너마이트")
		if bgDynamiteBlows(c) {
			if card, ok := g.removeSlot(p, bgSlotDynamite); ok {
				g.toDiscard(card)
			}
			g.emit("dynamite", p.Seat, fmt.Sprintf(
				"다이너마이트가 터져 %s님이 체력 %d을 잃습니다", p.Name, BGDynamiteDamage))
			g.damage(p.Seat, BGDynamiteDamage, -1)
			if g.Phase == BGPhaseGameOver {
				return
			}
			if !p.Alive {
				g.nextTurn()
				return
			}
		} else if next := g.nextAliveSeat(p.Seat); next >= 0 && next != p.Seat {
			card, _ := g.removeSlot(p, bgSlotDynamite)
			nt := g.player(next)
			nt.Equipment = append(nt.Equipment, card)
			g.emit("dynamite", p.Seat, fmt.Sprintf(
				"다이너마이트가 터지지 않아 %s님에게 넘어갑니다", nt.Name))
		}
	}

	// ② 감옥
	if bgHasSlot(p, bgSlotJail) {
		card, _ := g.removeSlot(p, bgSlotJail)
		g.toDiscard(card)
		c := g.flip(p.Seat, "감옥")
		if !bgJailEscapes(c) {
			g.emit("jail", p.Seat, fmt.Sprintf("%s님이 감옥에 갇혀 차례를 건너뜁니다", p.Name))
			g.nextTurn()
			return
		}
		g.emit("jail", p.Seat, fmt.Sprintf("%s님이 감옥에서 빠져나왔습니다", p.Name))
	}

	// ③ 카드 2장
	got := g.drawTo(p, BGTurnDraw)
	g.emit("draw", p.Seat, fmt.Sprintf("%s님이 카드 %d장을 뽑았습니다", p.Name, got))
	g.Phase = BGPhaseTurn
	g.StateSeq++
}

// nextTurn 왼쪽 생존자에게 차례를 넘긴다
func (g *BGGame) nextTurn() {
	if g.Phase == BGPhaseGameOver {
		return
	}
	g.Pending = nil
	next := g.nextAliveSeat(g.CurrentSeat)
	if next < 0 {
		g.finishByTurnLimit()
		return
	}
	g.CurrentSeat = next
	g.startTurn()
}

// EndTurn 차례 마무리. 손패가 체력보다 많으면 줄이기 단계가 열린다.
func (g *BGGame) EndTurn(seat int) error {
	if g.Phase != BGPhaseTurn {
		return errors.New("지금은 차례를 끝낼 수 없습니다")
	}
	if seat != g.CurrentSeat {
		return errors.New("당신의 차례가 아닙니다")
	}
	p := g.player(seat)
	if p == nil {
		return errors.New("좌석을 찾을 수 없습니다")
	}
	if over := len(p.Hand) - p.HP; over > 0 {
		g.Phase = BGPhaseDiscard
		g.StateSeq++
		g.emit("discard_open", p.Seat, fmt.Sprintf(
			"%s님이 손패 %d장을 버려야 합니다 (체력 %d)", p.Name, over, p.HP))
		return nil
	}
	g.nextTurn()
	return nil
}

// DiscardCards 차례 끝 손패 줄이기. 초과분과 정확히 같은 수를 버려야 한다.
func (g *BGGame) DiscardCards(seat int, indexes []int) error {
	if g.Phase != BGPhaseDiscard {
		return errors.New("지금은 손패를 버릴 수 없습니다")
	}
	if seat != g.CurrentSeat {
		return errors.New("당신의 차례가 아닙니다")
	}
	p := g.player(seat)
	if p == nil {
		return errors.New("좌석을 찾을 수 없습니다")
	}
	need := len(p.Hand) - p.HP
	if need <= 0 {
		g.nextTurn()
		return nil
	}

	seen := map[int]bool{}
	uniq := []int{}
	for _, i := range indexes {
		if i < 0 || i >= len(p.Hand) || seen[i] {
			continue
		}
		seen[i] = true
		uniq = append(uniq, i)
	}
	if len(uniq) != need {
		return fmt.Errorf("정확히 %d장을 버려야 합니다", need)
	}

	sort.Sort(sort.Reverse(sort.IntSlice(uniq)))
	dropped := []string{}
	for _, i := range uniq {
		c := g.takeHand(p, i)
		g.toDiscard(c)
		dropped = append(dropped, bgLabel(c.Kind))
	}
	g.action(p, fmt.Sprintf("%s님이 손패 %d장을 버렸습니다 (%s)",
		p.Name, len(uniq), strings.Join(dropped, "·")))
	g.nextTurn()
	return nil
}

// ==================== 카드 사용 ====================

// bgHandler 카드 한 종류의 처리. Check 는 "낼 수 있는가", Apply 는 효과다.
// Apply 가 불릴 때 카드는 이미 손패에서 떨어져 나온 상태다 (갈색은 버린
// 더미로 갔고, 파란색은 Apply 가 장비 칸에 꽂는다).
type bgHandler struct {
	Check func(g *BGGame, p, target *BGPlayer, card BGCard) error
	Apply func(g *BGGame, p, target *BGPlayer, card BGCard, cardIndex int)
}

// bgPlayTable 카드별 처리 표 (파일 상단의 표와 1:1로 대응한다)
var bgPlayTable = map[BGKind]bgHandler{
	BGBang:       {Apply: bgApplyBang},
	BGMiss:       {Check: bgCheckResponseOnly},
	BGBeer:       {Apply: bgApplyBeer},
	BGSaloon:     {Apply: bgApplySaloon},
	BGDuel:       {Apply: bgApplyDuel},
	BGGatling:    {Apply: bgApplyGatling},
	BGIndians:    {Apply: bgApplyIndians},
	BGStagecoach: {Apply: bgApplyStagecoach},
	BGWellsFargo: {Apply: bgApplyWellsFargo},
	BGStore:      {Apply: bgApplyStore},
	BGCatBalou:   {Check: bgCheckHasCards, Apply: bgApplyCatBalou},
	BGPanic:      {Check: bgCheckHasCards, Apply: bgApplyPanic},

	BGBarrel:     bgEquipHandler,
	BGJail:       {Check: bgCheckJail, Apply: bgApplyJail},
	BGDynamite:   bgEquipHandler,
	BGMustang:    bgEquipHandler,
	BGScope:      bgEquipHandler,
	BGSchofield:  bgEquipHandler,
	BGRemington:  bgEquipHandler,
	BGCarabine:   bgEquipHandler,
	BGWinchester: bgEquipHandler,
	BGVolcanic:   bgEquipHandler,
}

// Play 손패의 index 번째 카드를 낸다.
func (g *BGGame) Play(seat, index int, targetSeat, targetCardIndex *int) error {
	if g.Phase != BGPhaseTurn {
		return errors.New("지금은 카드를 낼 수 없습니다")
	}
	if seat != g.CurrentSeat {
		return errors.New("당신의 차례가 아닙니다")
	}
	p := g.player(seat)
	if p == nil || !p.Alive {
		return errors.New("탈락한 좌석입니다")
	}
	if index < 0 || index >= len(p.Hand) {
		return errors.New("없는 카드입니다")
	}
	card := p.Hand[index]
	def, ok := bgDef(card.Kind)
	if !ok {
		return errors.New("알 수 없는 카드입니다")
	}
	handler, ok := bgPlayTable[card.Kind]
	if !ok {
		return errors.New("낼 수 없는 카드입니다")
	}

	target, err := g.resolveTarget(p, def, targetSeat)
	if err != nil {
		return err
	}
	if card.Kind == BGBang && g.BangBlocked(seat) {
		return errors.New("이번 차례에는 뱅!을 더 낼 수 없습니다")
	}
	if handler.Check != nil {
		if err := handler.Check(g, p, target, card); err != nil {
			return err
		}
	}

	g.takeHand(p, index)
	if !def.Blue { // 파란색은 Apply 가 장비 칸에 꽂는다
		g.toDiscard(card)
	}
	cardIdx := 0
	if targetCardIndex != nil {
		cardIdx = *targetCardIndex
	}
	handler.Apply(g, p, target, card, cardIdx)
	return nil
}

// resolveTarget 대상 규칙(bgCards.Target) 하나로 대상 검증을 끝낸다.
// 쏠 수 없는 이유는 문구로 그대로 내려간다 (프론트가 사유를 표시한다).
func (g *BGGame) resolveTarget(p *BGPlayer, def bgCardDef, targetSeat *int) (*BGPlayer, error) {
	switch def.Target {
	case bgTargetNone:
		return nil, nil
	case bgTargetSelf:
		return p, nil
	case bgTargetResponse:
		return nil, fmt.Errorf("%s는 대응할 때만 낼 수 있습니다", def.Label)
	}

	if targetSeat == nil {
		return nil, fmt.Errorf("%s는 대상을 골라야 합니다", def.Label)
	}
	t := g.player(*targetSeat)
	if t == nil || !t.Alive {
		return nil, errors.New("이미 탈락한 대상입니다")
	}
	if t.Seat == p.Seat {
		return nil, errors.New("자기 자신은 대상이 될 수 없습니다")
	}

	switch def.Target {
	case bgTargetInRange:
		d := g.DistanceBetween(p.Seat, t.Seat)
		r := bgWeaponRange(p)
		if d < 0 || d > r {
			return nil, fmt.Errorf("사거리 밖입니다 (거리 %d · 사거리 %d)", d, r)
		}
	case bgTargetDist1:
		d := g.DistanceBetween(p.Seat, t.Seat)
		if d < 0 || d > 1 {
			return nil, fmt.Errorf("거리 1 이내만 대상이 됩니다 (거리 %d)", d)
		}
	case bgTargetJail:
		if t.Role == BGRoleSheriff {
			return nil, errors.New("보안관은 감옥에 가둘 수 없습니다")
		}
		if bgHasSlot(t, bgSlotJail) {
			return nil, errors.New("이미 감옥에 갇혀 있습니다")
		}
	}
	return t, nil
}

// ---- 개별 처리 (표의 한 줄씩) ----

// bgCheckResponseOnly 빗나감! — 차례에는 낼 수 없다
func bgCheckResponseOnly(g *BGGame, p, target *BGPlayer, card BGCard) error {
	return fmt.Errorf("%s는 대응할 때만 낼 수 있습니다", bgLabel(card.Kind))
}

// bgCheckHasCards 캣 벌로우·강탈! — 가져갈 카드가 있어야 한다
func bgCheckHasCards(g *BGGame, p, target *BGPlayer, card BGCard) error {
	if target == nil || len(target.Hand)+len(target.Equipment) == 0 {
		return errors.New("대상에게 가져갈 카드가 없습니다")
	}
	return nil
}

// bgCheckJail 감옥 — 대상 검증은 resolveTarget 이 끝냈다
func bgCheckJail(g *BGGame, p, target *BGPlayer, card BGCard) error {
	if target == nil {
		return errors.New("대상을 골라야 합니다")
	}
	return nil
}

func bgApplyBang(g *BGGame, p, target *BGPlayer, card BGCard, _ int) {
	p.BangUsed++
	g.action(p, fmt.Sprintf("%s님이 %s님에게 뱅!을 쐈습니다", p.Name, target.Name))
	g.openPending(BGPendBang, p.Seat, BGNeedMiss, []int{target.Seat})
}

func bgApplyBeer(g *BGGame, p, target *BGPlayer, card BGCard, _ int) {
	if g.aliveCount() <= 2 {
		g.action(p, fmt.Sprintf("%s님이 맥주를 마셨지만 둘만 남아 효과가 없습니다", p.Name))
		return
	}
	if p.HP >= p.MaxHP {
		g.action(p, fmt.Sprintf("%s님이 맥주를 마셨지만 체력이 이미 가득합니다", p.Name))
		return
	}
	p.HP++
	g.action(p, fmt.Sprintf("%s님이 맥주로 체력을 회복했습니다 (%d/%d)",
		p.Name, p.HP, p.MaxHP))
}

func bgApplySaloon(g *BGGame, p, target *BGPlayer, card BGCard, _ int) {
	healed := []string{}
	for _, s := range g.othersFrom(p.Seat) {
		t := g.player(s)
		if t.HP < t.MaxHP {
			t.HP++
			healed = append(healed, t.Name)
		}
	}
	if len(healed) == 0 {
		g.action(p, fmt.Sprintf("%s님이 주점을 열었지만 회복할 사람이 없습니다", p.Name))
		return
	}
	g.action(p, fmt.Sprintf("%s님이 주점을 열어 %s님의 체력을 1씩 올렸습니다",
		p.Name, strings.Join(healed, "·")))
}

func bgApplyDuel(g *BGGame, p, target *BGPlayer, card BGCard, _ int) {
	g.action(p, fmt.Sprintf("%s님이 %s님에게 결투를 걸었습니다", p.Name, target.Name))
	g.openPending(BGPendDuel, p.Seat, BGNeedBang, []int{target.Seat})
}

func bgApplyGatling(g *BGGame, p, target *BGPlayer, card BGCard, _ int) {
	g.action(p, fmt.Sprintf("%s님이 기관총을 난사했습니다", p.Name))
	g.openPending(BGPendGatling, p.Seat, BGNeedMiss, g.othersFrom(p.Seat))
}

func bgApplyIndians(g *BGGame, p, target *BGPlayer, card BGCard, _ int) {
	g.action(p, fmt.Sprintf("%s님이 인디언!을 불렀습니다", p.Name))
	g.openPending(BGPendIndians, p.Seat, BGNeedBang, g.othersFrom(p.Seat))
}

func bgApplyStagecoach(g *BGGame, p, target *BGPlayer, card BGCard, _ int) {
	n := g.drawTo(p, BGStagecoachDraw)
	g.action(p, fmt.Sprintf("%s님이 역마차로 카드 %d장을 뽑았습니다", p.Name, n))
}

func bgApplyWellsFargo(g *BGGame, p, target *BGPlayer, card BGCard, _ int) {
	n := g.drawTo(p, BGWellsFargoDraw)
	g.action(p, fmt.Sprintf("%s님이 웰스파고로 카드 %d장을 뽑았습니다", p.Name, n))
}

// bgApplyStore 잡화점 — 인원수만큼 공개하고 낸 사람부터 한 장씩 고른다
func bgApplyStore(g *BGGame, p, target *BGPlayer, card BGCard, _ int) {
	g.StoreCards = g.drawN(g.aliveCount())
	g.action(p, fmt.Sprintf("%s님이 잡화점을 열어 카드 %d장을 펼쳤습니다",
		p.Name, len(g.StoreCards)))
	if len(g.StoreCards) == 0 {
		return
	}
	g.Pending = &BGPending{
		Kind: BGPendStore, BySeat: p.Seat, TargetSeat: p.Seat,
		Need: BGNeedPick, Passed: []int{}, Queue: g.aliveOrderFrom(p.Seat),
	}
	g.advanceStore()
}

func bgApplyCatBalou(g *BGGame, p, target *BGPlayer, card BGCard, cardIndex int) {
	taken, from, ok := g.takeFromTarget(target, cardIndex)
	if !ok {
		g.action(p, fmt.Sprintf("%s님의 캣 벌로우가 빗나갔습니다", p.Name))
		return
	}
	g.toDiscard(taken)
	g.action(p, fmt.Sprintf("%s님이 캣 벌로우로 %s님의 %s 1장을 버리게 했습니다",
		p.Name, target.Name, from))
}

func bgApplyPanic(g *BGGame, p, target *BGPlayer, card BGCard, cardIndex int) {
	taken, from, ok := g.takeFromTarget(target, cardIndex)
	if !ok {
		g.action(p, fmt.Sprintf("%s님의 강탈!이 빗나갔습니다", p.Name))
		return
	}
	p.Hand = append(p.Hand, taken)
	g.action(p, fmt.Sprintf("%s님이 강탈!로 %s님의 %s 1장을 빼앗았습니다",
		p.Name, target.Name, from))
}

// bgEquipHandler 파란색 장비 공용 처리. 무기는 교체가 허용되고
// 나머지 칸은 이미 차 있으면 낼 수 없다.
var bgEquipHandler = bgHandler{
	Check: func(g *BGGame, p, target *BGPlayer, card BGCard) error {
		def, _ := bgDef(card.Kind)
		if def.Slot == bgSlotWeapon {
			return nil
		}
		if bgHasSlot(p, def.Slot) {
			return fmt.Errorf("%s는 이미 장착돼 있습니다", def.Label)
		}
		return nil
	},
	Apply: func(g *BGGame, p, target *BGPlayer, card BGCard, _ int) {
		def, _ := bgDef(card.Kind)
		if def.Slot == bgSlotWeapon {
			if old, ok := g.removeSlot(p, bgSlotWeapon); ok {
				g.toDiscard(old)
			}
		}
		p.Equipment = append(p.Equipment, card)
		if def.Range > 0 {
			g.action(p, fmt.Sprintf("%s님이 %s를 장착했습니다 (사거리 %d)",
				p.Name, def.Label, def.Range))
			return
		}
		g.action(p, fmt.Sprintf("%s님이 %s를 장착했습니다", p.Name, def.Label))
	},
}

func bgApplyJail(g *BGGame, p, target *BGPlayer, card BGCard, _ int) {
	target.Equipment = append(target.Equipment, card)
	g.action(p, fmt.Sprintf("%s님이 %s님을 감옥에 가뒀습니다", p.Name, target.Name))
}

// takeFromTarget 대상의 [손패 … 장비] 한 축에서 idx 번째를 떼어낸다.
// 범위를 벗어나면 첫 장으로 잘라 맞춘다. 두 번째 값은 "손패"/"장비" 표기.
func (g *BGGame) takeFromTarget(t *BGPlayer, idx int) (BGCard, string, bool) {
	total := len(t.Hand) + len(t.Equipment)
	if total == 0 {
		return BGCard{}, "", false
	}
	if idx < 0 || idx >= total {
		idx = 0
	}
	if idx < len(t.Hand) {
		c := t.Hand[idx]
		t.Hand = append(t.Hand[:idx], t.Hand[idx+1:]...)
		return c, "손패", true
	}
	e := idx - len(t.Hand)
	c := t.Equipment[e]
	t.Equipment = append(t.Equipment[:e], t.Equipment[e+1:]...)
	return c, "장비", true
}

// ==================== 대응 창 ====================

// openPending 대응 창을 열고 첫 대상으로 넘어간다
func (g *BGGame) openPending(kind string, by int, need string, targets []int) {
	g.Pending = &BGPending{
		Kind: kind, BySeat: by, TargetSeat: by, Need: need,
		Passed: []int{}, Queue: append([]int{}, targets...),
	}
	g.advancePending()
}

// advancePending 큐에서 다음 대상을 꺼낸다. 스스로 판정할 수 있는 대상은
// (술통이 튕겨내거나 낼 카드가 아예 없으면) 묻지 않고 바로 처리한다 —
// 결과가 같으므로 정보가 새지 않고 판이 빨리 굴러간다.
func (g *BGGame) advancePending() {
	pd := g.Pending
	if pd == nil || g.Phase == BGPhaseGameOver {
		return
	}
	for len(pd.Queue) > 0 {
		next := pd.Queue[0]
		pd.Queue = pd.Queue[1:]
		t := g.player(next)
		if t == nil || !t.Alive {
			continue
		}
		pd.TargetSeat = next
		if g.autoResolve() {
			if g.Phase == BGPhaseGameOver {
				return
			}
			continue
		}
		g.Phase = BGPhaseRespond
		g.StateSeq++
		return
	}
	g.finishPending()
}

// finishPending 창을 닫고 차례로 돌아간다 (차례 주인이 죽었으면 다음 차례)
func (g *BGGame) finishPending() {
	g.Pending = nil
	if g.Phase == BGPhaseGameOver {
		return
	}
	cur := g.player(g.CurrentSeat)
	if cur == nil || !cur.Alive {
		g.nextTurn()
		return
	}
	g.Phase = BGPhaseTurn
	g.StateSeq++
}

// autoResolve 지금 대상이 물어볼 것 없이 판정되면 처리하고 true.
// 술통은 뱅! 계열(need=miss)에만 적용된다.
func (g *BGGame) autoResolve() bool {
	pd := g.Pending
	t := g.player(pd.TargetSeat)
	if t == nil || !t.Alive {
		return true
	}
	if pd.Need == BGNeedMiss && bgHasSlot(t, bgSlotBarrel) {
		c := g.flip(t.Seat, "술통")
		if bgBarrelSaves(c) {
			g.emit("barrel", t.Seat, fmt.Sprintf("%s님이 술통으로 뱅!을 튕겨냈습니다", t.Name))
			pd.Passed = append(pd.Passed, t.Seat)
			return true
		}
	}
	if !bgHandHas(t, BGKind(pd.Need)) {
		g.pendingFail(t)
		return true
	}
	return false
}

// pendingFail 대응하지 못한 대상의 결말 — 체력 −1
func (g *BGGame) pendingFail(t *BGPlayer) {
	pd := g.Pending
	pd.Passed = append(pd.Passed, t.Seat)
	switch pd.Kind {
	case BGPendIndians:
		g.emit("hit", t.Seat, fmt.Sprintf(
			"%s님이 뱅!을 내지 못해 체력 1을 잃습니다", t.Name))
	case BGPendDuel:
		g.emit("hit", t.Seat, fmt.Sprintf(
			"%s님이 결투에서 뱅!을 내지 못해 체력 1을 잃습니다", t.Name))
	default:
		g.emit("hit", t.Seat, fmt.Sprintf(
			"%s님이 빗나감!을 내지 못해 체력 1을 잃습니다", t.Name))
	}
	g.damage(t.Seat, 1, pd.BySeat)
}

// Respond 대응 창의 응답. index 생략(nil)은 포기다.
func (g *BGGame) Respond(seat int, index *int) error {
	if g.Phase != BGPhaseRespond || g.Pending == nil {
		return errors.New("지금은 대응할 수 없습니다")
	}
	pd := g.Pending
	if seat != pd.TargetSeat {
		return errors.New("당신이 대응할 차례가 아닙니다")
	}
	p := g.player(seat)
	if p == nil || !p.Alive {
		return errors.New("탈락한 좌석입니다")
	}

	if index == nil { // 포기
		g.pendingFail(p)
		if g.Phase == BGPhaseGameOver {
			return nil
		}
		g.advancePending()
		return nil
	}

	i := *index
	if i < 0 || i >= len(p.Hand) {
		return errors.New("없는 카드입니다")
	}
	need := BGKind(pd.Need)
	if p.Hand[i].Kind != need {
		return fmt.Errorf("%s 카드로만 대응할 수 있습니다", bgLabel(need))
	}
	card := g.takeHand(p, i)
	g.toDiscard(card)

	if pd.Kind == BGPendDuel {
		g.emit("respond", p.Seat, fmt.Sprintf("%s님이 결투에서 뱅!으로 받아쳤습니다", p.Name))
		pd.BySeat, pd.TargetSeat = pd.TargetSeat, pd.BySeat
		if g.autoResolve() {
			if g.Phase == BGPhaseGameOver {
				return nil
			}
			g.advancePending() // 큐가 비어 있어 창을 닫는다
			return nil
		}
		g.Phase = BGPhaseRespond
		g.StateSeq++
		return nil
	}

	if pd.Kind == BGPendIndians {
		g.emit("respond", p.Seat, fmt.Sprintf("%s님이 뱅!을 버려 인디언!을 넘겼습니다", p.Name))
	} else {
		g.emit("respond", p.Seat, fmt.Sprintf("%s님이 빗나감!으로 막았습니다", p.Name))
	}
	pd.Passed = append(pd.Passed, p.Seat)
	g.advancePending()
	return nil
}

// ==================== 잡화점 ====================

// advanceStore 다음 고를 사람으로 넘긴다. 공개분이 떨어지면 창을 닫는다.
func (g *BGGame) advanceStore() {
	pd := g.Pending
	if pd == nil || g.Phase == BGPhaseGameOver {
		return
	}
	for len(pd.Queue) > 0 && len(g.StoreCards) > 0 {
		next := pd.Queue[0]
		pd.Queue = pd.Queue[1:]
		t := g.player(next)
		if t == nil || !t.Alive {
			continue
		}
		pd.TargetSeat = next
		g.Phase = BGPhaseStorePick
		g.StateSeq++
		return
	}
	for _, c := range g.StoreCards { // 남은 공개분은 버린다
		g.toDiscard(c)
	}
	g.StoreCards = []BGCard{}
	g.finishPending()
}

// Pick 잡화점 공개분에서 한 장 고른다
func (g *BGGame) Pick(seat, index int) error {
	if g.Phase != BGPhaseStorePick || g.Pending == nil {
		return errors.New("지금은 카드를 고를 수 없습니다")
	}
	pd := g.Pending
	if seat != pd.TargetSeat {
		return errors.New("당신이 고를 차례가 아닙니다")
	}
	if index < 0 || index >= len(g.StoreCards) {
		return errors.New("없는 카드입니다")
	}
	p := g.player(seat)
	if p == nil || !p.Alive {
		return errors.New("탈락한 좌석입니다")
	}
	c := g.StoreCards[index]
	g.StoreCards = append(g.StoreCards[:index], g.StoreCards[index+1:]...)
	p.Hand = append(p.Hand, c)
	pd.Passed = append(pd.Passed, seat)
	g.emit("store_pick", seat, fmt.Sprintf("%s님이 잡화점에서 카드 1장을 골랐습니다", p.Name))
	g.advanceStore()
	return nil
}

// ==================== 피해 · 탈락 ====================

// damage 체력을 깎고 0 이하면 탈락시킨다. bySeat 은 가해자(-1 없음).
func (g *BGGame) damage(seat, amount, bySeat int) {
	p := g.player(seat)
	if p == nil || !p.Alive || amount <= 0 {
		return
	}
	p.HP -= amount
	if p.HP > 0 {
		g.emit("damage", p.Seat, fmt.Sprintf("%s님의 체력이 %d 남았습니다", p.Name, p.HP))
		return
	}
	p.HP = 0
	g.eliminate(p, bySeat)
}

// eliminate 탈락 처리 — 손패·장비를 전부 버리고 정체를 공개한다.
//
// 탈락 보상: 무법자를 죽인 사람은 카드 3장을 뽑고, 보안관이 부관을 죽이면
// 보안관은 손패와 장비를 전부 버린다.
func (g *BGGame) eliminate(p *BGPlayer, bySeat int) {
	p.Alive = false
	p.HP = 0
	g.toDiscard(p.Hand...)
	p.Hand = []BGCard{}
	g.toDiscard(p.Equipment...)
	p.Equipment = []BGCard{}

	if g.Pending != nil { // 남은 대응 큐에서 뺀다
		q := []int{}
		for _, s := range g.Pending.Queue {
			if s != p.Seat {
				q = append(q, s)
			}
		}
		g.Pending.Queue = q
	}

	g.emit("eliminated", p.Seat, fmt.Sprintf("%s님이 탈락했습니다 — 정체는 %s였습니다",
		p.Name, bgRoleLabel(p.Role)))

	killer := g.player(bySeat)
	if killer != nil && killer.Alive && killer.Seat != p.Seat {
		if p.Role == BGRoleOutlaw {
			n := g.drawTo(killer, BGOutlawBounty)
			g.emit("bounty", killer.Seat, fmt.Sprintf(
				"%s님이 무법자를 처치해 카드 %d장을 뽑았습니다", killer.Name, n))
		}
		if p.Role == BGRoleDeputy && killer.Role == BGRoleSheriff {
			g.toDiscard(killer.Hand...)
			killer.Hand = []BGCard{}
			g.toDiscard(killer.Equipment...)
			killer.Equipment = []BGCard{}
			g.emit("penalty", killer.Seat, fmt.Sprintf(
				"보안관 %s님이 부관을 쏴 손패와 장비를 전부 버립니다", killer.Name))
		}
	}
	g.checkGameOver()
}

// ==================== 종료 판정 ====================

// checkGameOver 보안관 사망 → 배신자가 마지막 1인이면 배신자 승, 아니면
// 무법자 승. 무법자·배신자 전멸 → 보안관·부관 승.
func (g *BGGame) checkGameOver() {
	if g.Phase == BGPhaseGameOver {
		return
	}
	sheriffAlive := false
	alive := []int{}
	badAlive := 0
	for _, p := range g.Players {
		if !p.Alive {
			continue
		}
		alive = append(alive, p.Seat)
		switch p.Role {
		case BGRoleSheriff:
			sheriffAlive = true
		case BGRoleOutlaw, BGRoleRenegade:
			badAlive++
		}
	}

	if !sheriffAlive {
		if len(alive) == 1 && g.Players[alive[0]].Role == BGRoleRenegade {
			g.finish("renegade", []BGRole{BGRoleRenegade},
				"보안관이 쓰러지고 배신자만 살아남았습니다 — 배신자 승리")
			return
		}
		g.finish("outlaw", []BGRole{BGRoleOutlaw},
			"보안관이 쓰러졌습니다 — 무법자 승리")
		return
	}
	if badAlive == 0 {
		g.finish("sheriff", []BGRole{BGRoleSheriff, BGRoleDeputy},
			"무법자와 배신자가 모두 쓰러졌습니다 — 보안관 진영 승리")
	}
}

// finish 승리 진영을 확정한다. 승자 좌석은 생사와 무관하게 그 진영 전원이다.
func (g *BGGame) finish(winner string, roles []BGRole, message string) {
	seats, names := []int{}, []string{}
	want := map[BGRole]bool{}
	for _, r := range roles {
		want[r] = true
	}
	for _, p := range g.Players {
		if want[p.Role] {
			seats = append(seats, p.Seat)
			names = append(names, p.Name)
		}
	}
	g.Phase = BGPhaseGameOver
	g.Pending = nil
	g.StoreCards = []BGCard{}
	g.StateSeq++
	g.Result = &BGResult{
		Winner: winner, WinnerSeats: seats, WinnerNames: names, Message: message,
	}
	g.emit("game_over", -1, message)
}

// finishByTurnLimit 안전 상한. 규칙상 닿을 일이 거의 없지만, 닿으면
// 보안관 생존 여부로 판을 접는다.
func (g *BGGame) finishByTurnLimit() {
	if g.Phase == BGPhaseGameOver {
		return
	}
	for _, p := range g.Players {
		if p.Alive && p.Role == BGRoleSheriff {
			g.finish("sheriff", []BGRole{BGRoleSheriff, BGRoleDeputy},
				fmt.Sprintf("%d차례가 지나 보안관이 마을을 지켜냈습니다", BGMaxTurns))
			return
		}
	}
	g.finish("outlaw", []BGRole{BGRoleOutlaw},
		fmt.Sprintf("%d차례가 지나 보안관이 마을을 잃었습니다", BGMaxTurns))
}

// ==================== AFK 자동 진행 ====================

// ForceTurn 차례 무응답 — 그냥 차례를 끝낸다 (뽑기는 이미 자동으로 끝났다)
func (g *BGGame) ForceTurn() {
	if g.Phase != BGPhaseTurn {
		return
	}
	g.EndTurn(g.CurrentSeat)
}

// ForceRespond 대응 무응답 — 포기
func (g *BGGame) ForceRespond() {
	if g.Phase != BGPhaseRespond || g.Pending == nil {
		return
	}
	g.Respond(g.Pending.TargetSeat, nil)
}

// ForcePick 잡화점 무응답 — 첫 장
func (g *BGGame) ForcePick() {
	if g.Phase != BGPhaseStorePick || g.Pending == nil {
		return
	}
	g.Pick(g.Pending.TargetSeat, 0)
}

// ForceDiscard 손패 줄이기 무응답 — 앞에서부터
func (g *BGGame) ForceDiscard() {
	if g.Phase != BGPhaseDiscard {
		return
	}
	p := g.player(g.CurrentSeat)
	if p == nil {
		return
	}
	need := len(p.Hand) - p.HP
	if need <= 0 {
		g.nextTurn()
		return
	}
	idx := make([]int, 0, need)
	for i := 0; i < need; i++ {
		idx = append(idx, i)
	}
	g.DiscardCards(p.Seat, idx)
}

// ==================== 이벤트 ====================

// emit 연출 이벤트를 쌓는다 (허브가 DrainEvents 로 꺼내 방송한다)
func (g *BGGame) emit(kind string, seat int, message string) {
	g.events = append(g.events, BGGameEvent{Kind: kind, Seat: seat, Message: message})
}

// action 전원 공개 행동 — lastAction 갱신 + 이벤트
func (g *BGGame) action(p *BGPlayer, message string) {
	g.LastAction = &BGLastAction{Seat: p.Seat, Name: p.Name, Message: message}
	g.emit("action", p.Seat, message)
}

// DrainEvents 쌓인 이벤트를 꺼내 비운다
func (g *BGGame) DrainEvents() []BGGameEvent {
	out := g.events
	g.events = nil
	return out
}
