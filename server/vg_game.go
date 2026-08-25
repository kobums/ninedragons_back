package server

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ==================== 라스베가스 순수 규칙 ====================
//
// 지폐 배분·주사위 배치·동수 상쇄 정산만 다룬다. 클라이언트·타이머를 모르며,
// 허브(vg_hub.go)가 결과를 이벤트로 번역한다.
//
// 4라운드. 라운드 준비 때 카지노 6곳(눈 1~6)에 지폐 덱에서 합계 5만 이상이
// 될 때까지 지폐를 깐다. 각자 주사위 8개 — 차례에 전부 굴려(서버 자동)
// 한 눈을 골라 그 눈의 주사위 전부를 해당 카지노에 배치한다. 전원 소진하면
// 정산: 카지노별 배치 수 동수인 플레이어끼리는 서로 상쇄(전부 제외)하고,
// 남은 최다 배치자부터 큰 지폐 순으로 지급한다. 남는 지폐는 버린다.

// vgBillDeck 라운드 준비용 지폐 덱 (만 단위) — 원작 근사 다권종.
// 소액권이 섞여야 카지노에 여러 장이 깔려 "동수 상쇄 후 차순위" 재미가 산다.
// 라운드마다 새로 만들어 섞는다.
func vgBillDeck() []int {
	deck := []int{}
	for _, b := range []struct{ value, count int }{
		{1, 5}, {2, 5}, {3, 5}, {4, 5}, {5, 5},
		{6, 5}, {7, 5}, {8, 5}, {9, 5}, {10, 6},
	} {
		for i := 0; i < b.count; i++ {
			deck = append(deck, b.value)
		}
	}
	return deck
}

// vgCounts 눈(1~6)별 개수 — 인덱스가 곧 눈이다 (0 미사용)
func vgCounts(dice []int) [7]int {
	var counts [7]int
	for _, d := range dice {
		if d >= 1 && d <= 6 {
			counts[d]++
		}
	}
	return counts
}

// vgMostCommonFace 굴린 주사위에서 가장 많은 눈 (동수면 높은 눈).
// AFK 자동 배치가 쓴다. 빈 주사위면 0.
func vgMostCommonFace(dice []int) int {
	counts := vgCounts(dice)
	best, bestN := 0, 0
	for face := 1; face <= 6; face++ {
		if counts[face] >= bestN && counts[face] > 0 {
			best, bestN = face, counts[face]
		}
	}
	return best
}

// vgDiceString 이벤트·로그용 주사위 표기 ("1-3-3-5-6-6-6-6")
func vgDiceString(dice []int) string {
	parts := make([]string, len(dice))
	for i, d := range dice {
		parts[i] = strconv.Itoa(d)
	}
	return strings.Join(parts, "-")
}

// ==================== 동수 상쇄 정산 ====================

// vgSettleCasino 카지노 한 곳의 정산 — seat → 받는 금액 (만 단위).
// 배치 수가 같은 플레이어끼리는 서로 상쇄되어 전부 제외되고, 남은
// 최다 배치자가 최고 지폐, 차순위가 다음 지폐… 지폐가 소진될 때까지
// 지급한다. 남는 지폐는 버린다 (bills 는 내림차순 전제).
func vgSettleCasino(bills []int, placed map[int]int) map[int]int {
	byCount := map[int][]int{}
	for seat, n := range placed {
		if n > 0 {
			byCount[n] = append(byCount[n], seat)
		}
	}

	// 동수 상쇄 — 같은 개수를 배치한 좌석이 둘 이상이면 전부 제외
	type vgRank struct{ seat, count int }
	ranks := []vgRank{}
	for n, seats := range byCount {
		if len(seats) == 1 {
			ranks = append(ranks, vgRank{seat: seats[0], count: n})
		}
	}
	sort.Slice(ranks, func(i, j int) bool { return ranks[i].count > ranks[j].count })

	payout := map[int]int{}
	for i, r := range ranks {
		if i >= len(bills) {
			break
		}
		payout[r.seat] = bills[i]
	}
	return payout
}

// ==================== 게임 진행 ====================

// NewVGGame 대기 상태의 새 게임. 카지노 6곳은 처음부터 빈 지폐·배치로
// 존재한다 (스냅샷의 casinos 는 항상 6곳 — nil 금지).
func NewVGGame(id string) *VGGame {
	casinos := []*VGCasino{}
	for face := 1; face <= VGCasinoCount; face++ {
		casinos = append(casinos, &VGCasino{Face: face, Bills: []int{}, Placed: map[int]int{}})
	}
	return &VGGame{
		ID:          id,
		Players:     []*VGPlayer{},
		Phase:       VGPhaseWaiting,
		CurrentSeat: -1,
		Casinos:     casinos,
		Dice:        []int{},
		WinnerSeats: []int{},
	}
}

// AddPlayer 대기실 입장. 좌석 번호를 돌려준다.
func (g *VGGame) AddPlayer(name string) (int, error) {
	if g.Phase != VGPhaseWaiting {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= VGMaxPlayers {
		return -1, errors.New("자리가 없습니다 (최대 5명)")
	}
	seat := len(g.Players)
	g.Players = append(g.Players, &VGPlayer{Seat: seat, Name: name})
	return seat, nil
}

// RemovePlayer 대기실 퇴장. 좌석을 앞으로 당겨 빈틈을 없앤다.
func (g *VGGame) RemovePlayer(seat int) {
	if g.Phase != VGPhaseWaiting || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i, p := range g.Players {
		p.Seat = i
	}
}

// CanStart 시작 가능 여부 (호스트 명시 시작 — 2인부터)
func (g *VGGame) CanStart() bool {
	return g.Phase == VGPhaseWaiting && len(g.Players) >= VGMinPlayers
}

// Start 게임 시작 — 선공은 무작위, 1라운드를 준비하고 첫 차례를 굴린다
func (g *VGGame) Start(rng *rand.Rand) error {
	if !g.CanStart() {
		return errors.New("2명 이상 모여야 시작할 수 있습니다")
	}
	g.Ready = true
	g.StartedAt = time.Now()
	g.Round = 1
	g.FirstSeat = rng.Intn(len(g.Players))
	g.beginRound(rng)
	return nil
}

// beginRound 라운드 준비 — 카지노 지폐 배분·주사위 8개 충전·첫 차례 굴림.
// 덱을 섞어 각 카지노에 합계 5만 이상이 될 때까지 지폐를 깐다 (내림차순 정렬).
func (g *VGGame) beginRound(rng *rand.Rand) {
	deck := vgBillDeck()
	rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })

	idx := 0
	for _, c := range g.Casinos {
		c.Bills = []int{}
		c.Placed = map[int]int{}
		total := 0
		for total < VGCasinoMinTotal && idx < len(deck) {
			c.Bills = append(c.Bills, deck[idx])
			total += deck[idx]
			idx++
		}
		sort.Sort(sort.Reverse(sort.IntSlice(c.Bills)))
	}
	for _, p := range g.Players {
		p.DiceLeft = VGDiceCount
	}
	g.RoundResult = nil
	g.Phase = VGPhasePlacing
	g.beginTurn(g.FirstSeat, rng)
}

// beginTurn 좌석의 차례 시작 — 남은 주사위 전부를 서버가 자동으로 굴린다
// (결과는 전원 공개). 표시 편의를 위해 오름차순 정렬한다.
func (g *VGGame) beginTurn(seat int, rng *rand.Rand) {
	g.CurrentSeat = seat
	g.Dice = make([]int, g.Players[seat].DiceLeft)
	for i := range g.Dice {
		g.Dice[i] = 1 + rng.Intn(6)
	}
	sort.Ints(g.Dice)
}

// nextSeatWithDice from 다음 좌석부터 시계 방향으로 주사위가 남은 좌석.
// 전원 소진이면 -1 (정산 시점).
func (g *VGGame) nextSeatWithDice(from int) int {
	n := len(g.Players)
	for i := 1; i <= n; i++ {
		seat := (from + i) % n
		if g.Players[seat].DiceLeft > 0 {
			return seat
		}
	}
	return -1
}

// VGPlaceResult 배치 한 번의 결과
type VGPlaceResult struct {
	Seat       int
	Face       int
	Count      int  // 배치한 주사위 수
	RoundEnded bool // 전원 소진 — 정산까지 끝난 상태
}

// Place 방금 굴린 주사위 중 face 눈 전부를 해당 카지노에 배치하고 차례를
// 넘긴다. 주사위를 소진한 사람은 건너뛰며, 전원 소진하면 즉시 정산한다.
func (g *VGGame) Place(seat, face int, rng *rand.Rand) (*VGPlaceResult, error) {
	if g.Phase != VGPhasePlacing {
		return nil, errors.New("지금은 배치할 수 없습니다")
	}
	if seat != g.CurrentSeat {
		return nil, errors.New("당신의 차례가 아닙니다")
	}
	if face < 1 || face > 6 {
		return nil, errors.New("눈은 1~6 이어야 합니다")
	}
	count := vgCounts(g.Dice)[face]
	if count == 0 {
		return nil, errors.New("방금 굴린 주사위에 그 눈이 없습니다")
	}

	g.Casinos[face-1].Placed[seat] += count
	g.Players[seat].DiceLeft -= count
	res := &VGPlaceResult{Seat: seat, Face: face, Count: count}

	next := g.nextSeatWithDice(seat)
	if next < 0 {
		g.settleRound()
		res.RoundEnded = true
	} else {
		g.beginTurn(next, rng)
	}
	return res, nil
}

// settleRound 라운드 정산 — 카지노별 동수 상쇄 후 큰 지폐부터 지급하고
// 결과 문구를 만든다. round_end 로 전환한다 (다음 라운드는 허브 타이머).
func (g *VGGame) settleRound() {
	gains := make([]int, len(g.Players))
	for _, c := range g.Casinos {
		for seat, amount := range vgSettleCasino(c.Bills, c.Placed) {
			gains[seat] += amount
		}
	}

	parts := []string{}
	for _, p := range g.Players {
		p.Cash += gains[p.Seat]
		parts = append(parts, fmt.Sprintf("%s +%d만 달러", p.Name, gains[p.Seat]))
	}
	g.RoundResult = &VGRoundResult{
		Message: fmt.Sprintf("%d라운드 정산 — %s", g.Round, strings.Join(parts, ", ")),
	}
	g.Phase = VGPhaseRoundEnd
	g.CurrentSeat = -1
	g.Dice = []int{}
}

// NextRound round_end 에서 다음 라운드로 — 4라운드를 마쳤으면 게임 종료.
// 첫 배치 좌석은 라운드마다 한 칸 회전한다.
func (g *VGGame) NextRound(rng *rand.Rand) error {
	if g.Phase != VGPhaseRoundEnd {
		return errors.New("지금은 다음 라운드로 넘어갈 수 없습니다")
	}
	if g.Round >= VGTotalRounds {
		g.finish()
		return nil
	}
	g.Round++
	g.FirstSeat = (g.FirstSeat + 1) % len(g.Players)
	g.beginRound(rng)
	return nil
}

// finish 4라운드 정산 완료 — 총액 최고 좌석(들)이 승자 (동점 공동 승)
func (g *VGGame) finish() {
	g.Phase = VGPhaseGameOver
	best := -1
	g.WinnerSeats = []int{}
	for _, p := range g.Players {
		if p.Cash > best {
			best = p.Cash
			g.WinnerSeats = []int{p.Seat}
		} else if p.Cash == best {
			g.WinnerSeats = append(g.WinnerSeats, p.Seat)
		}
	}
}
