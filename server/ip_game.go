package server

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// ==================== 인디언 포커 순수 규칙 ====================
//
// 덱·안테·베팅(콜/레이즈/폴드)·쇼다운·팟 분배·탈락만 다룬다. 클라이언트·
// 타이머를 모르며, 허브(ip_hub.go)가 결과 구조체를 받아 이벤트로 번역한다.
// 간이 룰: 사이드팟 없음 — 콜 칩이 모자라면 올인이 현재 베팅으로 인정된다.

// ipDeckComposition 1~10 각 4장 = 40장 (라운드마다 새로 섞는다)
func ipDeckComposition() []int {
	deck := make([]int, 0, 40)
	for v := 1; v <= 10; v++ {
		for i := 0; i < 4; i++ {
			deck = append(deck, v)
		}
	}
	return deck
}

// NewIPGame 대기 상태의 새 게임
func NewIPGame(id string) *IPGame {
	return &IPGame{
		ID:          id,
		Players:     []*IPPlayer{},
		Phase:       IPPhaseWaiting,
		CurrentSeat: -1,
		FirstSeat:   -1,
		WinnerSeats: []int{},
	}
}

// AddPlayer 대기실 입장. 좌석 번호를 돌려준다.
func (g *IPGame) AddPlayer(name string) (int, error) {
	if g.Phase != IPPhaseWaiting {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= IPMaxPlayers {
		return -1, errors.New("자리가 없습니다 (최대 6명)")
	}
	seat := len(g.Players)
	g.Players = append(g.Players, &IPPlayer{
		Seat: seat, Name: name, Chips: IPStartChips, Alive: true,
	})
	return seat, nil
}

// RemovePlayer 대기실 퇴장. 좌석을 앞으로 당겨 빈틈을 없앤다.
func (g *IPGame) RemovePlayer(seat int) {
	if g.Phase != IPPhaseWaiting || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i, p := range g.Players {
		p.Seat = i
	}
}

// CanStart 시작 가능 여부 (호스트 명시 시작 — 2인부터)
func (g *IPGame) CanStart() bool {
	return g.Phase == IPPhaseWaiting && len(g.Players) >= IPMinPlayers
}

// Start 게임 시작 — 선을 무작위로 정하고 1라운드를 연다
func (g *IPGame) Start(rng *rand.Rand) error {
	if !g.CanStart() {
		return errors.New("2명 이상 모여야 시작할 수 있습니다")
	}
	g.Ready = true
	g.StartedAt = time.Now()
	g.Round = 1
	g.FirstSeat = rng.Intn(len(g.Players))
	g.startRound(rng)
	return nil
}

// startRound 안테·카드 배분 후 선부터 베팅을 연다.
// 호출 전 탈락 정리(NextRound)를 거쳤으므로 생존자는 항상 1칩 이상 가진다.
func (g *IPGame) startRound(rng *rand.Rand) {
	deck := ipDeckComposition()
	rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })

	g.Pot = 0
	g.CurrentBet = IPAnte
	g.RaisesLeft = IPMaxRaises
	g.RoundResult = nil

	for _, p := range g.Players {
		p.Folded = false
		p.AllIn = false
		p.Acted = false
		p.BetThisRound = 0
		p.Card = 0
		if !p.Alive {
			continue
		}
		// 안테: 전원 1칩 자동 → 팟
		p.Chips -= IPAnte
		p.BetThisRound = IPAnte
		g.Pot += IPAnte
		if p.Chips == 0 {
			p.AllIn = true // 안테로 바닥 — 이후 레이즈에 자동 올인 처리
		}
		p.Card = deck[0]
		deck = deck[1:]
	}

	g.Phase = IPPhaseBetting
	seat := g.nextActionSeat(g.FirstSeat - 1) // 선부터 탐색
	if seat < 0 {
		g.resolveShowdown() // 전원 올인 안전망 (정상 경로에선 도달하지 않음)
		return
	}
	g.CurrentSeat = seat
}

// aliveCount 탈락하지 않은 플레이어 수
func (g *IPGame) aliveCount() int {
	n := 0
	for _, p := range g.Players {
		if p.Alive {
			n++
		}
	}
	return n
}

// unfoldedCount 이번 라운드에 아직 살아 있는(폴드하지 않은) 참가자 수
func (g *IPGame) unfoldedCount() int {
	n := 0
	for _, p := range g.Players {
		if p.Alive && !p.Folded {
			n++
		}
	}
	return n
}

// nextActionSeat from 다음 좌석부터 시계 방향으로 행동이 필요한 좌석을
// 찾는다 (생존·미폴드·미올인·미응답). 없으면 -1 — 베팅 종료 신호다.
func (g *IPGame) nextActionSeat(from int) int {
	n := len(g.Players)
	for i := 1; i <= n; i++ {
		s := ((from+i)%n + n) % n
		p := g.Players[s]
		if p.Alive && !p.Folded && !p.AllIn && !p.Acted {
			return s
		}
	}
	return -1
}

// nextAliveSeat seat 다음의 생존 좌석 (시계 방향)
func (g *IPGame) nextAliveSeat(seat int) int {
	n := len(g.Players)
	for i := 1; i <= n; i++ {
		s := (seat + i) % n
		if g.Players[s].Alive {
			return s
		}
	}
	return seat
}

// IPActionResult 베팅 행동 한 번의 판정 결과 (허브가 이벤트로 번역)
type IPActionResult struct {
	Seat  int
	Kind  string // "check" | "call" | "allin_call" | "raise" | "fold"
	Paid  int    // 이번 행동으로 팟에 낸 칩
	Raise int    // 레이즈 증가분 (레이즈 외 0)
	Bet   int    // 행동 후 현재 베팅 총액

	RoundEnded bool
	FoldWin    bool // 전원 폴드 종료 (쇼다운 없음 — phase round_end)
}

// actor 베팅 행동의 공통 검증 — betting 단계의 현재 차례만 허용한다
func (g *IPGame) actor(seat int) (*IPPlayer, error) {
	if g.Phase != IPPhaseBetting {
		return nil, errors.New("지금은 베팅할 수 없습니다")
	}
	if seat != g.CurrentSeat {
		return nil, errors.New("당신의 차례가 아닙니다")
	}
	return g.Players[seat], nil
}

// Call 현재 베팅에 콜 (비용 0이면 체크). 칩이 모자라면 올인 콜 —
// 사이드팟 없이 현재 베팅으로 인정한다 (연습용 간이 룰).
func (g *IPGame) Call(seat int) (*IPActionResult, error) {
	p, err := g.actor(seat)
	if err != nil {
		return nil, err
	}

	cost := g.CurrentBet - p.BetThisRound
	if cost < 0 {
		cost = 0
	}
	pay := cost
	kind := "call"
	if cost == 0 {
		kind = "check"
	}
	if pay > p.Chips {
		pay = p.Chips
		kind = "allin_call"
	}
	p.Chips -= pay
	p.BetThisRound += pay
	g.Pot += pay
	if p.Chips == 0 {
		p.AllIn = true
	}
	p.Acted = true

	res := &IPActionResult{Seat: seat, Kind: kind, Paid: pay, Bet: g.CurrentBet}
	g.advanceBetting(res)
	return res, nil
}

// Raise 현재 베팅을 amount(1~3)만큼 올리고 그 액수까지 낸다.
// 라운드 전체 3회 상한·칩 보유량을 지켜야 한다.
func (g *IPGame) Raise(seat, amount int) (*IPActionResult, error) {
	p, err := g.actor(seat)
	if err != nil {
		return nil, err
	}
	if amount < IPRaiseMin || amount > IPRaiseMax {
		return nil, errors.New("레이즈는 1~3칩만 올릴 수 있습니다")
	}
	if g.RaisesLeft <= 0 {
		return nil, errors.New("이번 라운드 레이즈 횟수를 모두 사용했습니다")
	}
	cost := g.CurrentBet + amount - p.BetThisRound
	if cost > p.Chips {
		return nil, errors.New("칩이 부족해 레이즈할 수 없습니다")
	}

	g.RaisesLeft--
	g.CurrentBet += amount
	p.Chips -= cost
	p.BetThisRound += cost
	g.Pot += cost
	if p.Chips == 0 {
		p.AllIn = true
	}
	// 레이즈에는 나머지 전원이 다시 응답해야 한다 (올인·폴드 제외)
	for _, q := range g.Players {
		if q.Seat != seat && q.Alive && !q.Folded && !q.AllIn {
			q.Acted = false
		}
	}
	p.Acted = true

	res := &IPActionResult{Seat: seat, Kind: "raise", Paid: cost, Raise: amount, Bet: g.CurrentBet}
	g.advanceBetting(res)
	return res, nil
}

// Fold 폴드 — 낸 칩을 포기하고 이번 라운드에서 빠진다. 카드는 쇼다운에도
// 공개하지 않는다 (스냅샷에서 영구 0).
func (g *IPGame) Fold(seat int) (*IPActionResult, error) {
	p, err := g.actor(seat)
	if err != nil {
		return nil, err
	}
	p.Folded = true
	p.Acted = true

	res := &IPActionResult{Seat: seat, Kind: "fold", Bet: g.CurrentBet}
	g.advanceBetting(res)
	return res, nil
}

// advanceBetting 행동 후 진행 판정: 한 명만 남으면 즉시 팟 획득(카드 미공개),
// 전원이 응답을 마쳤으면 쇼다운, 아니면 다음 차례로.
func (g *IPGame) advanceBetting(res *IPActionResult) {
	if g.unfoldedCount() == 1 {
		g.resolveFoldWin()
		res.RoundEnded = true
		res.FoldWin = true
		return
	}
	next := g.nextActionSeat(g.CurrentSeat)
	if next < 0 {
		g.resolveShowdown()
		res.RoundEnded = true
		return
	}
	g.CurrentSeat = next
}

// resolveFoldWin 홀로 남은 참가자가 팟을 가져간다 (쇼다운 없음)
func (g *IPGame) resolveFoldWin() {
	winner := -1
	for _, p := range g.Players {
		if p.Alive && !p.Folded {
			winner = p.Seat
		}
	}
	pot := g.Pot
	g.Players[winner].Chips += pot
	g.Pot = 0
	g.RoundResult = &IPRoundResult{
		WinnerSeats: []int{winner},
		Message: fmt.Sprintf("남은 전원이 폴드 — %s님이 팟 %d칩을 가져갑니다",
			g.Players[winner].Name, pot),
	}
	g.CurrentSeat = -1
	g.Phase = IPPhaseRoundEnd
}

// resolveShowdown 생존자 카드 공개 — 최고 숫자가 팟 획득, 동점이면 균등
// 분배 (나머지는 선부터 시계 방향 순서).
func (g *IPGame) resolveShowdown() {
	n := len(g.Players)
	best := 0
	for _, p := range g.Players {
		if p.Alive && !p.Folded && p.Card > best {
			best = p.Card
		}
	}
	// 선부터 시계 방향 순서의 승자 목록 (나머지 분배 기준)
	winners := []int{}
	for i := 0; i < n; i++ {
		s := (g.FirstSeat + i) % n
		p := g.Players[s]
		if p.Alive && !p.Folded && p.Card == best {
			winners = append(winners, s)
		}
	}

	pot := g.Pot
	share := pot / len(winners)
	rem := pot % len(winners)
	for i, s := range winners {
		gain := share
		if i < rem {
			gain++ // 나머지는 선부터
		}
		g.Players[s].Chips += gain
	}
	g.Pot = 0

	sortedWinners := append([]int{}, winners...)
	sort.Ints(sortedWinners)

	// 카드 값 요약 문구: "쇼다운 — 철수 9 · 봇1 4 → 철수님이 팟 12칩을 가져갑니다"
	parts := []string{}
	for _, p := range g.Players {
		if p.Alive && !p.Folded {
			parts = append(parts, fmt.Sprintf("%s %d", p.Name, p.Card))
		}
	}
	winNames := []string{}
	for _, s := range sortedWinners {
		winNames = append(winNames, g.Players[s].Name+"님")
	}
	verb := fmt.Sprintf("%s이 팟 %d칩을 가져갑니다", strings.Join(winNames, ", "), pot)
	if len(winners) > 1 {
		verb = fmt.Sprintf("동점! %s이 팟 %d칩을 나눠 가집니다", strings.Join(winNames, ", "), pot)
	}
	g.RoundResult = &IPRoundResult{
		WinnerSeats: sortedWinners,
		Message:     fmt.Sprintf("쇼다운 — %s → %s", strings.Join(parts, " · "), verb),
	}
	g.CurrentSeat = -1
	g.Phase = IPPhaseShowdown
}

// NextRound 결과 표시 후 자동 진행 — 칩 0 탈락 정리·종료 판정·다음 라운드.
// 돌려주는 값은 이번에 탈락한 좌석들 (허브가 이벤트로 발표).
func (g *IPGame) NextRound(rng *rand.Rand) ([]int, error) {
	if g.Phase != IPPhaseShowdown && g.Phase != IPPhaseRoundEnd {
		return nil, errors.New("지금은 다음 라운드를 시작할 수 없습니다")
	}

	eliminated := []int{}
	for _, p := range g.Players {
		if p.Alive && p.Chips <= 0 {
			p.Alive = false // 안테를 낼 수 없다 — 탈락
			eliminated = append(eliminated, p.Seat)
		}
	}

	if g.Round >= IPRounds {
		g.finishByChips()
		return eliminated, nil
	}
	if g.aliveCount() <= 1 {
		g.finishLastStanding()
		return eliminated, nil
	}

	g.Round++
	g.FirstSeat = g.nextAliveSeat(g.FirstSeat) // 선 순환
	g.startRound(rng)
	return eliminated, nil
}

// finishByChips 10라운드 종료 — 최다 칩 승리 (동점 공동 승)
func (g *IPGame) finishByChips() {
	best := -1
	for _, p := range g.Players {
		if p.Chips > best {
			best = p.Chips
		}
	}
	g.WinnerSeats = []int{}
	for _, p := range g.Players {
		if p.Chips == best {
			g.WinnerSeats = append(g.WinnerSeats, p.Seat)
		}
	}
	g.EndReason = "chips"
	g.CurrentSeat = -1
	g.Phase = IPPhaseGameOver
}

// finishLastStanding 1인 생존 — 즉시 종료
func (g *IPGame) finishLastStanding() {
	g.WinnerSeats = []int{}
	for _, p := range g.Players {
		if p.Alive {
			g.WinnerSeats = append(g.WinnerSeats, p.Seat)
		}
	}
	g.EndReason = "last_standing"
	g.CurrentSeat = -1
	g.Phase = IPPhaseGameOver
}
