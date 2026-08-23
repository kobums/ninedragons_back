package server

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// ==================== 더 마인드 순수 규칙 ====================
//
// 덱 구성·라운드 배분·오름차순 판정·실수 소각·라운드 보상·수리검만 다룬다.
// 클라이언트·타이머를 모르며, 허브(mi_hub.go)가 카운트다운·라운드 캡·게임
// 캡·수리검 창을 걸고 이벤트 큐(DrainEvents)를 방송한다.
//
// 차례가 없다. 판정 순서는 허브 고루틴에 도착한 순서 그대로이고, 이 파일의
// Play 는 그 순서대로 한 번에 하나씩만 불린다 — 그래서 락이 필요 없다.
// 동시에 두 사람이 냈을 때 뒤에 온 카드가 더 작으면 그것도 실수로 판정된다.
//
// 한 판의 흐름:
//
//	라운드 r 시작 → 각자 r 장 (본인만 봄) → 3초 카운트다운(ready)
//	→ 누구든 mi_play (자기 최저 카드가 나간다)
//	   · 남이 더 작은 카드를 들고 있었다 → 그 카드들 전부 공개·소각, 생명 -1
//	     (한 번에 여러 장이 걸려도 생명은 1만 깎인다)
//	   · 손패가 전부 비면 라운드 성공 → 보상(3·6·9 생명 +1 / 2·5·8 수리검 +1)
//	→ 생명 0 이면 즉시 패배, 최종 라운드를 마치면 클리어
//
// 반드시 끝난다 — 카드를 낼 때마다 전체 손패가 최소 한 장 줄고, 아무도 내지
// 않아도 허브의 라운드 캡이 생명을 깎으며 강제로 한 장을 밀어낸다.

// NewMIGame 대기 상태의 새 게임
func NewMIGame(id string) *MIGame {
	return &MIGame{
		ID:      id,
		Players: []*MIPlayer{},
		Phase:   MIPhaseWaiting,
		Pile:    []int{},
	}
}

// AddPlayer 대기실 입장. 좌석 번호를 돌려준다.
func (g *MIGame) AddPlayer(name string) (int, error) {
	if g.Phase != MIPhaseWaiting {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= MIMaxPlayers {
		return -1, fmt.Errorf("자리가 없습니다 (최대 %d명)", MIMaxPlayers)
	}
	seat := len(g.Players)
	g.Players = append(g.Players, &MIPlayer{Seat: seat, Name: name, Hand: []int{}})
	return seat, nil
}

// RemovePlayer 대기실 퇴장. 좌석을 앞으로 당겨 빈틈을 없앤다.
func (g *MIGame) RemovePlayer(seat int) {
	if g.Phase != MIPhaseWaiting || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i, p := range g.Players {
		p.Seat = i
	}
}

// CanStart 시작 가능 여부
func (g *MIGame) CanStart() bool {
	return g.Phase == MIPhaseWaiting && len(g.Players) >= MIMinPlayers
}

// DrainEvents 쌓인 연출 이벤트를 꺼내 비운다 (허브가 방송한다)
func (g *MIGame) DrainEvents() []MIGameEvent {
	events := g.events
	g.events = nil
	return events
}

// pushEvent 연출 이벤트 적재 (seat -1 은 좌석 없음)
func (g *MIGame) pushEvent(kind string, seat int, message string) {
	g.events = append(g.events, MIGameEvent{Kind: kind, Seat: seat, Message: message})
}

// ==================== 덱 / 배분 (순수) ====================

// miBuildDeck 1~100 숫자 카드 100장 (중복 없음)
func miBuildDeck() []int {
	deck := make([]int, 0, MIDeckSize)
	for n := 1; n <= MIDeckSize; n++ {
		deck = append(deck, n)
	}
	return deck
}

// miDeal 라운드 r 의 배분 — 인원 × r 장을 뽑아 좌석별 오름차순 손패로 나눈다.
// 입력을 건드리지 않고 새 슬라이스를 돌려준다 (테스트 대조용 순수 함수).
func miDeal(deck []int, players, perPlayer int) [][]int {
	hands := make([][]int, players)
	for i := range hands {
		hands[i] = []int{}
	}
	pos := 0
	for i := 0; i < players; i++ {
		for j := 0; j < perPlayer && pos < len(deck); j++ {
			hands[i] = append(hands[i], deck[pos])
			pos++
		}
		sort.Ints(hands[i])
	}
	return hands
}

// miSmallerThan 손패에서 c 보다 작은 카드와 남는 카드를 갈라낸다 (순수).
// 오름차순 손패를 전제로 하되 순서에 의존하지 않는다.
func miSmallerThan(hand []int, c int) (smaller, rest []int) {
	smaller, rest = []int{}, []int{}
	for _, card := range hand {
		if card < c {
			smaller = append(smaller, card)
		} else {
			rest = append(rest, card)
		}
	}
	return smaller, rest
}

// miCardsLeft 전원 손패에 남은 총 장수
func miCardsLeft(players []*MIPlayer) int {
	n := 0
	for _, p := range players {
		n += len(p.Hand)
	}
	return n
}

// miLowestSeat 전체에서 가장 작은 카드를 든 좌석과 그 카드 (없으면 seat -1)
func miLowestSeat(players []*MIPlayer) (int, int) {
	seat, card := -1, 0
	for _, p := range players {
		if len(p.Hand) == 0 {
			continue
		}
		if seat < 0 || p.Hand[0] < card {
			seat, card = p.Seat, p.Hand[0]
		}
	}
	return seat, card
}

// miBurnLabel 소각 카드 목록의 한글 표기 ("나(12), 다(31)")
func miBurnLabel(players []*MIPlayer, burned []MIBurnedCard) string {
	parts := []string{}
	for _, b := range burned {
		name := "?"
		if b.Seat >= 0 && b.Seat < len(players) {
			name = players[b.Seat].Name
		}
		parts = append(parts, fmt.Sprintf("%s(%d)", name, b.Card))
	}
	return strings.Join(parts, ", ")
}

// ==================== 진행 ====================

// Start 게임 시작 — 생명·수리검을 세팅하고 1라운드를 편다
func (g *MIGame) Start(rng *rand.Rand) error {
	if !g.CanStart() {
		return fmt.Errorf("%d명 이상 모여야 시작할 수 있습니다", MIMinPlayers)
	}

	n := len(g.Players)
	g.MaxRound = miMaxRoundByPlayers(n)
	g.Lives = n // 시작 생명은 인원수와 같다
	g.Stars = MIStartStars
	g.Round = 0
	g.LastMistake = nil
	g.StarVote = nil
	g.Result = nil
	g.EndReason = ""
	g.Ready = true
	g.StartedAt = time.Now()

	g.pushEvent("game_started", -1, fmt.Sprintf(
		"게임 시작 — %d인 협력전, 최종 라운드는 %d 입니다. 말·채팅·손짓 없이 오름차순으로 내세요 (생명 %d, 수리검 %d)",
		n, g.MaxRound, g.Lives, g.Stars))

	g.BeginRound(rng)
	return nil
}

// BeginRound 다음 라운드를 편다 — 손패를 새로 나누고 카운트다운(ready)에 든다.
// 중앙 더미와 직전 실수는 라운드마다 초기화된다.
func (g *MIGame) BeginRound(rng *rand.Rand) {
	if g.Phase == MIPhaseGameOver {
		return
	}
	g.Round++
	g.Pile = []int{}
	g.LastPlayed = 0
	g.LastMistake = nil
	g.StarVote = nil

	deck := miBuildDeck()
	rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })
	hands := miDeal(deck, len(g.Players), g.Round)
	for i, p := range g.Players {
		p.Hand = hands[i]
	}

	g.Phase = MIPhaseReady
	g.pushEvent("round_ready", -1, fmt.Sprintf(
		"%d 라운드 — 각자 %d장. 곧 시작합니다", g.Round, g.Round))
}

// BeginPlaying 카운트다운이 끝나 실제로 낼 수 있게 된다
func (g *MIGame) BeginPlaying() {
	if g.Phase != MIPhaseReady {
		return
	}
	g.Phase = MIPhasePlaying
	g.pushEvent("round_start", -1, fmt.Sprintf("%d 라운드 시작 — 지금부터 낼 수 있습니다", g.Round))
}

// Play 카드 내기 — 언제나 자기 최저 카드 한 장이다(카드 지정 없음).
// 선착 판정이라 차례 검사가 없고, 도착 순서가 곧 판정 순서다.
//
// 돌려주는 error 는 규약 위반(진행 중 아님·손패 없음)이라 상태를 건드리지
// 않고 mi_error 로만 응답한다. 실수(생명 -1)는 error 가 아니라
// lastMistake 로 나간다 — 규칙대로 진행된 결과이기 때문이다.
func (g *MIGame) Play(seat int) error {
	if g.Phase == MIPhaseReady {
		return errors.New("아직 카운트다운 중입니다")
	}
	if g.Phase != MIPhasePlaying {
		return errors.New("진행 중인 라운드가 아닙니다")
	}
	if seat < 0 || seat >= len(g.Players) {
		return errors.New("좌석을 찾을 수 없습니다")
	}
	player := g.Players[seat]
	if len(player.Hand) == 0 {
		return errors.New("낼 카드가 없습니다")
	}

	card := player.Hand[0]
	player.Hand = player.Hand[1:]
	g.Pile = append(g.Pile, card)
	g.LastPlayed = card

	// 남이 더 작은 카드를 들고 있었다면 전부 공개·소각한다.
	// 여러 장이 걸려도 생명은 1만 깎인다.
	burned := []MIBurnedCard{}
	for _, other := range g.Players {
		if other.Seat == seat || len(other.Hand) == 0 {
			continue
		}
		smaller, rest := miSmallerThan(other.Hand, card)
		other.Hand = rest
		for _, c := range smaller {
			burned = append(burned, MIBurnedCard{Seat: other.Seat, Card: c})
		}
	}
	sort.Slice(burned, func(i, j int) bool { return burned[i].Card < burned[j].Card })

	if len(burned) > 0 {
		g.Lives--
		message := fmt.Sprintf("%s님이 %d를 냈지만 더 작은 카드가 남아 있었습니다 — %s 소각, 생명 -1 (남은 생명 %d)",
			player.Name, card, miBurnLabel(g.Players, burned), g.Lives)
		g.LastMistake = &MIMistake{
			Seat: seat, Played: card,
			Burned: append([]MIBurnedCard{}, burned...), Message: message,
		}
		g.pushEvent("mistake", seat, message)
	} else {
		g.pushEvent("play", seat, fmt.Sprintf("%s님이 %d를 냈습니다 (남은 %d장)",
			player.Name, card, len(player.Hand)))
	}

	g.settle()
	return nil
}

// AutoAdvance 라운드 캡(기본 3분) 초과 — 무한 대기를 막는 안전장치.
// 전체에서 가장 작은 카드를 강제로 내보내며 정체의 대가로 생명을 1 깎는다.
// 강제로 나가는 것이 최소 카드라 추가 소각은 발생하지 않는다.
func (g *MIGame) AutoAdvance() {
	if g.Phase != MIPhasePlaying {
		return
	}
	seat, card := miLowestSeat(g.Players)
	if seat < 0 {
		return
	}
	player := g.Players[seat]
	player.Hand = player.Hand[1:]
	g.Pile = append(g.Pile, card)
	g.LastPlayed = card
	g.Lives--

	message := fmt.Sprintf("아무도 내지 않아 자동으로 진행합니다 — %s님의 %d (생명 -1, 남은 생명 %d)",
		player.Name, card, g.Lives)
	g.LastMistake = &MIMistake{
		Seat: seat, Played: card,
		Burned: []MIBurnedCard{}, Message: message,
	}
	g.pushEvent("stalled", seat, message)
	g.settle()
}

// settle 카드가 빠져나간 뒤의 공통 정산 — 생명 0 즉시 패배가 라운드 성공보다
// 먼저다(규칙: 생명 0 → 즉시 패배).
func (g *MIGame) settle() {
	if g.Lives <= 0 {
		g.finish(false, "no_lives")
		return
	}
	if miCardsLeft(g.Players) == 0 {
		g.completeRound()
	}
}

// completeRound 라운드 성공 — 보상을 주고 정산 단계(round_end)로 든다.
// 최종 라운드였다면 그대로 클리어다.
func (g *MIGame) completeRound() {
	g.Phase = MIPhaseRoundEnd
	g.StarVote = nil

	rewards := []string{}
	if miLifeBonusRounds[g.Round] {
		g.Lives++
		rewards = append(rewards, "생명 +1")
	}
	if miStarBonusRounds[g.Round] {
		g.Stars++
		rewards = append(rewards, "수리검 +1")
	}

	tail := ""
	if len(rewards) > 0 {
		tail = " — 보상 " + strings.Join(rewards, ", ")
	}
	g.pushEvent("round_clear", -1, fmt.Sprintf("%d 라운드 성공!%s (생명 %d, 수리검 %d)",
		g.Round, tail, g.Lives, g.Stars))

	if g.Round >= g.MaxRound {
		g.finish(true, "cleared")
	}
}

// ==================== 수리검 ====================

// ProposeStar 수리검 제안 — 제안자는 자동 찬성으로 시작한다.
// 나머지 전원이 찬성하면 발동, 한 명이라도 거절하거나 창이 지나면 무산.
func (g *MIGame) ProposeStar(seat int, now time.Time, window time.Duration) error {
	if g.Phase != MIPhasePlaying {
		return errors.New("진행 중인 라운드가 아닙니다")
	}
	if seat < 0 || seat >= len(g.Players) {
		return errors.New("좌석을 찾을 수 없습니다")
	}
	if g.Stars <= 0 {
		return errors.New("남은 수리검이 없습니다")
	}
	if g.StarVote != nil {
		return errors.New("이미 수리검 제안이 진행 중입니다")
	}
	if miCardsLeft(g.Players) == 0 {
		return errors.New("낼 카드가 없습니다")
	}

	g.StarSeq++
	g.StarVote = &MIStarVote{
		Proposer: seat,
		Accepted: []int{seat},
		EndsAt:   now.Add(window).UnixMilli(),
		Seq:      g.StarSeq,
	}
	g.pushEvent("star_proposed", seat, fmt.Sprintf(
		"%s님이 수리검을 제안했습니다 — 전원 찬성하면 각자 최저 카드 1장을 버립니다 (%.0f초)",
		g.Players[seat].Name, window.Seconds()))

	// 1인 남은 상황은 없지만(최소 2인), 계약은 지킨다
	g.tryResolveStar()
	return nil
}

// AcceptStar 수리검 찬성. 전원이 모이면 즉시 발동한다.
func (g *MIGame) AcceptStar(seat int) error {
	if g.StarVote == nil {
		return errors.New("진행 중인 수리검 제안이 없습니다")
	}
	if seat < 0 || seat >= len(g.Players) {
		return errors.New("좌석을 찾을 수 없습니다")
	}
	for _, s := range g.StarVote.Accepted {
		if s == seat {
			return errors.New("이미 찬성했습니다")
		}
	}
	g.StarVote.Accepted = append(g.StarVote.Accepted, seat)
	sort.Ints(g.StarVote.Accepted)
	g.pushEvent("star_accepted", seat, fmt.Sprintf("%s님이 수리검에 찬성했습니다 (%d/%d)",
		g.Players[seat].Name, len(g.StarVote.Accepted), len(g.Players)))
	g.tryResolveStar()
	return nil
}

// DeclineStar 수리검 거절 — 한 명이라도 거절하면 무산된다
func (g *MIGame) DeclineStar(seat int) error {
	if g.StarVote == nil {
		return errors.New("진행 중인 수리검 제안이 없습니다")
	}
	if seat < 0 || seat >= len(g.Players) {
		return errors.New("좌석을 찾을 수 없습니다")
	}
	g.StarVote = nil
	g.pushEvent("star_declined", seat, fmt.Sprintf(
		"%s님이 수리검을 거절했습니다 — 제안이 무산됐습니다", g.Players[seat].Name))
	return nil
}

// ExpireStar 수리검 창 만료 (허브 타이머). 지나간 발화는 seq 로 걸러낸다.
func (g *MIGame) ExpireStar(seq int) bool {
	if g.StarVote == nil || g.StarVote.Seq != seq {
		return false
	}
	g.StarVote = nil
	g.pushEvent("star_expired", -1, "수리검 제안 시간이 지나 무산됐습니다")
	return true
}

// tryResolveStar 만장일치면 발동한다
func (g *MIGame) tryResolveStar() {
	if g.StarVote == nil || len(g.StarVote.Accepted) < len(g.Players) {
		return
	}
	g.resolveStar()
}

// resolveStar 수리검 발동 — 전원이 자기 최저 카드 1장을 공개하고 버린다.
// 생명은 소모하지 않는다.
//
// 버린 카드는 중앙 더미(pile)에 쌓지 않고 lastPlayed 도 건드리지 않는다.
// 규칙 문장이 "낸다"가 아니라 "공개하고 버린다"이고, 더미에 얹으면 남은
// 손패가 더미 꼭대기보다 작아지는 모순이 생기기 때문이다
// (A [30,40] · B [50,60] 이면 30·50 이 나가고 40 이 남는다).
// 공개는 이벤트 문구로 한다 — 전원·관전자가 같은 문구를 받는다.
func (g *MIGame) resolveStar() {
	revealed := []MIBurnedCard{}
	for _, p := range g.Players {
		if len(p.Hand) == 0 {
			continue
		}
		card := p.Hand[0]
		p.Hand = p.Hand[1:]
		revealed = append(revealed, MIBurnedCard{Seat: p.Seat, Card: card})
	}
	g.Stars--
	g.StarVote = nil

	sort.Slice(revealed, func(i, j int) bool { return revealed[i].Card < revealed[j].Card })
	g.pushEvent("star_used", -1, fmt.Sprintf(
		"수리검 발동! 전원이 최저 카드를 버렸습니다 — %s (남은 수리검 %d)",
		miBurnLabel(g.Players, revealed), g.Stars))

	g.settle()
}

// ==================== 종료 ====================

// ForceEnd 게임 캡(기본 20분) 초과 — 실패로 정산한다
func (g *MIGame) ForceEnd() {
	if g.Phase == MIPhaseGameOver {
		return
	}
	g.finish(false, "time_up")
}

// finish 종료 판정. 협력 게임이라 승패가 아니라 클리어 여부다.
func (g *MIGame) finish(cleared bool, reason string) {
	g.Phase = MIPhaseGameOver
	g.EndReason = reason
	g.StarVote = nil

	var message string
	switch {
	case cleared:
		message = fmt.Sprintf("최종 %d 라운드까지 완주했습니다 — 클리어! (남은 생명 %d, 수리검 %d)",
			g.MaxRound, g.Lives, g.Stars)
	case reason == "time_up":
		message = fmt.Sprintf("제한 시간이 끝났습니다 — %d/%d 라운드에서 종료", g.Round, g.MaxRound)
	default:
		message = fmt.Sprintf("생명이 모두 떨어졌습니다 — %d/%d 라운드에서 실패", g.Round, g.MaxRound)
	}

	g.Result = &MIResult{Cleared: cleared, Round: g.Round, Message: message}
	g.pushEvent("game_over", -1, message)
}
