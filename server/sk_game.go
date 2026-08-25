package server

import (
	"errors"
	"fmt"
	"math/rand"
	"time"
)

// ==================== 스컬 순수 게임 로직 ====================
//
// 허브 비의존. 배치(동시 1장 → 턴제 추가 배치/베팅) → 베팅(레이즈/패스) →
// 뒤집기(자기 더미 전부 → 상대 더미 선택) → 점수/제거/탈락 판정을 담당한다.
// 봇(sk_bot.go)과 서버(sk_hub.go)가 같은 검증을 공유한다.

func NewSKGame(id string) *SKGame {
	return &SKGame{
		ID:             id,
		Phase:          SKPhaseWaiting,
		CurrentSeat:    -1,
		HighBidderSeat: -1,
		ChallengerSeat: -1,
		WinnerSeat:     -1,
	}
}

// ==================== 로비 ====================

func (g *SKGame) AddPlayer(name string) (int, error) {
	if g.Ready {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= SKMaxPlayers {
		return -1, errors.New("정원이 가득 찼습니다")
	}
	seat := len(g.Players)
	g.Players = append(g.Players, SKPlayer{Seat: seat, Name: name, Alive: true})
	return seat, nil
}

// RemovePlayer 대기 중 이탈 — 좌석을 빼고 남은 좌석을 앞으로 당긴다
func (g *SKGame) RemovePlayer(seat int) {
	if g.Ready || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i := range g.Players {
		g.Players[i].Seat = i
	}
}

func (g *SKGame) CanStart() bool {
	return !g.Ready && len(g.Players) >= SKMinPlayers
}

// Start 손패(장미 3 + 해골 1)를 나눠주고 1라운드 배치로 진입한다
func (g *SKGame) Start(rng *rand.Rand) error {
	if g.Ready {
		return errors.New("이미 시작된 게임입니다")
	}
	n := len(g.Players)
	if n < SKMinPlayers || n > SKMaxPlayers {
		return fmt.Errorf("%d~%d인이 필요합니다", SKMinPlayers, SKMaxPlayers)
	}
	for i := range g.Players {
		hand := []SKCard{SKCardRose, SKCardRose, SKCardRose, SKCardSkull}
		rng.Shuffle(len(hand), func(a, b int) { hand[a], hand[b] = hand[b], hand[a] })
		g.Players[i].Hand = hand
		g.Players[i].Stack = []SKCard{}
		g.Players[i].Alive = true
		g.Players[i].Points = 0
	}
	g.LeaderSeat = 0
	g.RoundNo = 0
	g.Ready = true
	g.StartedAt = time.Now()
	g.beginRound()
	return nil
}

// beginRound 새 라운드 초기화 — 카드는 이미 손으로 돌아와 있어야 한다
func (g *SKGame) beginRound() {
	g.RoundNo++
	g.Phase = SKPhasePlacing
	g.PlacingTurns = false
	g.CurrentSeat = -1
	g.HighBid = 0
	g.HighBidderSeat = -1
	g.ChallengerSeat = -1
	g.Flipped = []SKFlippedCard{}
	g.RoundResult = nil
	for i := range g.Players {
		g.Players[i].Stack = []SKCard{}
		g.Players[i].Passed = false
		g.Players[i].Bid = 0
	}
}

// ==================== 조회 ====================

func (g *SKGame) aliveCount() int {
	n := 0
	for _, p := range g.Players {
		if p.Alive {
			n++
		}
	}
	return n
}

func (g *SKGame) firstAlive() int {
	for _, p := range g.Players {
		if p.Alive {
			return p.Seat
		}
	}
	return -1
}

// nextAlive seat 다음의 생존 좌석 (시계 방향)
func (g *SKGame) nextAlive(seat int) int {
	n := len(g.Players)
	for i := 1; i <= n; i++ {
		s := (seat + i) % n
		if g.Players[s].Alive {
			return s
		}
	}
	return seat
}

// nextBidder seat 다음의 베팅 차례 — 생존·비패스·최고 베팅자 제외 (-1 없음)
func (g *SKGame) nextBidder(seat int) int {
	n := len(g.Players)
	for i := 1; i <= n; i++ {
		s := (seat + i) % n
		p := g.Players[s]
		if p.Alive && !p.Passed && s != g.HighBidderSeat {
			return s
		}
	}
	return -1
}

// TotalStacked 전체 배치 수 (베팅 상한)
func (g *SKGame) TotalStacked() int {
	n := 0
	for _, p := range g.Players {
		n += len(p.Stack)
	}
	return n
}

// rosesFlipped 이번 뒤집기에서 공개된 장미 수
func (g *SKGame) rosesFlipped() int {
	n := 0
	for _, f := range g.Flipped {
		if f.Card == string(SKCardRose) {
			n++
		}
	}
	return n
}

// allPlacedOnce 생존자 전원이 첫 카드를 내려놓았는지 (동시 배치 완료 판정)
func (g *SKGame) allPlacedOnce() bool {
	for _, p := range g.Players {
		if p.Alive && len(p.Stack) == 0 {
			return false
		}
	}
	return true
}

// ==================== 배치 ====================

// SubmitPlace 손패 index 카드를 내 더미 맨 위에 내려놓는다.
// 동시 배치 파트에서는 전원 1장(중복 금지), 턴제 파트에서는 차례인 사람만.
func (g *SKGame) SubmitPlace(seat, index int) error {
	if g.Phase != SKPhasePlacing {
		return errors.New("지금은 배치 단계가 아닙니다")
	}
	if seat < 0 || seat >= len(g.Players) {
		return errors.New("잘못된 좌석입니다")
	}
	p := &g.Players[seat]
	if !p.Alive {
		return errors.New("탈락한 플레이어는 행동할 수 없습니다")
	}
	if g.PlacingTurns {
		if g.CurrentSeat != seat {
			return errors.New("당신의 차례가 아닙니다")
		}
	} else if len(p.Stack) > 0 {
		return errors.New("이미 카드를 내려놓았습니다")
	}
	if len(p.Hand) == 0 {
		return errors.New("손패가 없습니다 — 베팅을 선언하세요")
	}
	if index < 0 || index >= len(p.Hand) {
		return errors.New("잘못된 카드입니다")
	}

	card := p.Hand[index]
	p.Hand = append(p.Hand[:index], p.Hand[index+1:]...)
	p.Stack = append(p.Stack, card)

	if !g.PlacingTurns {
		if g.allPlacedOnce() {
			// 동시 배치 완료 → 선부터 턴제 파트
			g.PlacingTurns = true
			if g.Players[g.LeaderSeat].Alive {
				g.CurrentSeat = g.LeaderSeat
			} else {
				g.CurrentSeat = g.nextAlive(g.LeaderSeat)
			}
		}
		return nil
	}
	g.CurrentSeat = g.nextAlive(seat)
	return nil
}

// AutoPlaceAll 동시 배치 마감 — 아직 내려놓지 않은 생존자 전원 무작위 배치 (AFK)
func (g *SKGame) AutoPlaceAll(rng *rand.Rand) {
	if g.Phase != SKPhasePlacing || g.PlacingTurns {
		return
	}
	for i := range g.Players {
		p := &g.Players[i]
		if !p.Alive || len(p.Stack) > 0 || len(p.Hand) == 0 {
			continue
		}
		g.SubmitPlace(p.Seat, rng.Intn(len(p.Hand)))
	}
}

// ==================== 베팅 ====================

// SubmitBid 베팅 선언 — 배치 턴에서는 첫 선언(1 이상), 베팅 단계에서는
// 레이즈(현재보다 큰 N). 상한은 전체 배치 수이며, 상한과 같으면 즉시 도전자.
func (g *SKGame) SubmitBid(seat, count int, rng *rand.Rand) error {
	if seat < 0 || seat >= len(g.Players) {
		return errors.New("잘못된 좌석입니다")
	}
	p := &g.Players[seat]
	if !p.Alive {
		return errors.New("탈락한 플레이어는 행동할 수 없습니다")
	}
	switch g.Phase {
	case SKPhasePlacing:
		if !g.PlacingTurns || g.CurrentSeat != seat {
			return errors.New("당신의 차례가 아닙니다")
		}
		if count < 1 {
			return errors.New("베팅은 1장 이상이어야 합니다")
		}
	case SKPhaseBidding:
		if g.CurrentSeat != seat {
			return errors.New("당신의 차례가 아닙니다")
		}
		if count <= g.HighBid {
			return fmt.Errorf("현재 베팅(%d장)보다 커야 합니다", g.HighBid)
		}
	default:
		return errors.New("지금은 베팅할 수 없습니다")
	}
	total := g.TotalStacked()
	if count > total {
		return fmt.Errorf("베팅은 전체 배치 수(%d장)를 넘을 수 없습니다", total)
	}

	g.Phase = SKPhaseBidding
	g.HighBid = count
	g.HighBidderSeat = seat
	p.Bid = count

	// 전체 배치 수를 부르면 즉시 도전자
	if count == total {
		g.startChallenge(seat, rng)
		return nil
	}
	next := g.nextBidder(seat)
	if next < 0 {
		// 남은 베팅 참가자가 선언자뿐 — 즉시 도전자
		g.startChallenge(seat, rng)
		return nil
	}
	g.CurrentSeat = next
	return nil
}

// SubmitPass 베팅 포기 — 한 명(선언자) 빼고 전부 패스하면 그가 도전자
func (g *SKGame) SubmitPass(seat int, rng *rand.Rand) error {
	if g.Phase != SKPhaseBidding {
		return errors.New("지금은 베팅 단계가 아닙니다")
	}
	if seat < 0 || seat >= len(g.Players) {
		return errors.New("잘못된 좌석입니다")
	}
	p := &g.Players[seat]
	if !p.Alive {
		return errors.New("탈락한 플레이어는 행동할 수 없습니다")
	}
	if g.CurrentSeat != seat {
		return errors.New("당신의 차례가 아닙니다")
	}
	if seat == g.HighBidderSeat {
		return errors.New("최고 베팅자는 패스할 수 없습니다")
	}
	p.Passed = true

	remaining := -1
	count := 0
	for _, q := range g.Players {
		if q.Alive && !q.Passed {
			remaining = q.Seat
			count++
		}
	}
	if count == 1 {
		g.startChallenge(remaining, rng)
		return nil
	}
	g.CurrentSeat = g.nextBidder(seat)
	return nil
}

// ==================== 뒤집기 ====================

// startChallenge 도전자 확정 — 자기 더미 전부를 위에서부터 강제로 뒤집는다.
// 해골이 나오면 즉시 실패, 장미만으로 목표를 채우면 즉시 성공.
func (g *SKGame) startChallenge(seat int, rng *rand.Rand) {
	g.Phase = SKPhaseFlipping
	g.ChallengerSeat = seat
	g.CurrentSeat = seat
	for len(g.Players[seat].Stack) > 0 {
		card := g.flipTop(seat)
		if card == SKCardSkull {
			g.resolveFail(seat, rng)
			return
		}
		if g.rosesFlipped() >= g.HighBid {
			g.resolveSuccess()
			return
		}
	}
}

// flipTop 그 좌석 더미의 맨 위 카드를 공개 목록으로 옮긴다
func (g *SKGame) flipTop(seat int) SKCard {
	p := &g.Players[seat]
	card := p.Stack[len(p.Stack)-1]
	p.Stack = p.Stack[:len(p.Stack)-1]
	g.Flipped = append(g.Flipped, SKFlippedCard{Seat: seat, Card: string(card)})
	return card
}

// SubmitFlip 도전자가 상대 더미의 맨 위 카드 1장을 뒤집는다
func (g *SKGame) SubmitFlip(seat, target int, rng *rand.Rand) error {
	if g.Phase != SKPhaseFlipping {
		return errors.New("지금은 뒤집기 단계가 아닙니다")
	}
	if seat != g.ChallengerSeat {
		return errors.New("도전자만 뒤집을 수 있습니다")
	}
	if target < 0 || target >= len(g.Players) {
		return errors.New("잘못된 좌석입니다")
	}
	if target == seat {
		return errors.New("자기 더미는 이미 뒤집었습니다")
	}
	if len(g.Players[target].Stack) == 0 {
		return errors.New("그 더미에는 남은 카드가 없습니다")
	}

	card := g.flipTop(target)
	if card == SKCardSkull {
		g.resolveFail(target, rng)
		return nil
	}
	if g.rosesFlipped() >= g.HighBid {
		g.resolveSuccess()
	}
	return nil
}

// RandomFlipTarget 남은 카드가 있는 상대 더미 중 무작위 (AFK 자동 뒤집기용)
func (g *SKGame) RandomFlipTarget(rng *rand.Rand) int {
	cands := []int{}
	for _, p := range g.Players {
		if p.Seat != g.ChallengerSeat && len(p.Stack) > 0 {
			cands = append(cands, p.Seat)
		}
	}
	if len(cands) == 0 {
		return -1
	}
	return cands[rng.Intn(len(cands))]
}

// ==================== 라운드 해소 ====================

// returnCards 라운드 종료 — 남은 더미와 공개된 카드를 주인 손으로 되돌린다.
// Flipped 는 결과 화면 표시용으로 남긴다 (다음 라운드 시작 때 비움).
func (g *SKGame) returnCards() {
	for i := range g.Players {
		g.Players[i].Hand = append(g.Players[i].Hand, g.Players[i].Stack...)
		g.Players[i].Stack = []SKCard{}
	}
	for _, f := range g.Flipped {
		g.Players[f.Seat].Hand = append(g.Players[f.Seat].Hand, SKCard(f.Card))
	}
}

// resolveFail 해골 실패 — 실패자(도전자)의 카드 1장을 무작위 제거한다.
// 제거된 카드는 비공개(저장하지 않음). 0장이 되면 탈락, 1인 생존이면 승리.
// 다음 라운드 선: 실패자 (탈락 시 해골 주인, 그마저 탈락이면 다음 생존자).
func (g *SKGame) resolveFail(skullOwner int, rng *rand.Rand) {
	ch := g.ChallengerSeat
	g.returnCards()

	p := &g.Players[ch]
	if len(p.Hand) > 0 {
		i := rng.Intn(len(p.Hand))
		p.Hand = append(p.Hand[:i], p.Hand[i+1:]...)
	}

	msg := fmt.Sprintf("해골! %s님이 카드 1장을 잃었습니다", p.Name)
	if skullOwner == ch {
		msg = fmt.Sprintf("자기 해골! %s님이 카드 1장을 잃었습니다", p.Name)
	}
	if len(p.Hand) == 0 {
		p.Alive = false
		msg += " — 탈락"
	}
	g.RoundResult = &SKRoundResult{Kind: "fail", Seat: ch, Message: msg}
	g.Phase = SKPhaseRoundEnd
	g.CurrentSeat = -1

	// 1인 생존 시 그 사람 승리
	if g.aliveCount() == 1 {
		g.finishWinner(g.firstAlive())
		return
	}
	switch {
	case p.Alive:
		g.LeaderSeat = ch
	case g.Players[skullOwner].Alive:
		g.LeaderSeat = skullOwner
	default:
		g.LeaderSeat = g.nextAlive(ch)
	}
}

// resolveSuccess 장미 목표 달성 — 도전자 1점, 2점 선취 시 승리
func (g *SKGame) resolveSuccess() {
	ch := g.ChallengerSeat
	g.returnCards()

	p := &g.Players[ch]
	p.Points++
	g.RoundResult = &SKRoundResult{
		Kind: "success",
		Seat: ch,
		Message: fmt.Sprintf("성공! %s님이 장미 %d장을 모두 공개했습니다 — 1점 획득",
			p.Name, g.HighBid),
	}
	g.Phase = SKPhaseRoundEnd
	g.CurrentSeat = -1

	if p.Points >= SKWinPoints {
		g.finishWinner(ch)
		return
	}
	g.LeaderSeat = ch
}

func (g *SKGame) finishWinner(seat int) {
	g.WinnerSeat = seat
	g.Phase = SKPhaseGameOver
	g.CurrentSeat = -1
}

// NextRound round_end 발표 뒤 다음 라운드 시작 (허브 타이머가 호출)
func (g *SKGame) NextRound() {
	if g.Phase != SKPhaseRoundEnd {
		return
	}
	g.beginRound()
}
