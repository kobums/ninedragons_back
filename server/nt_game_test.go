package server

import (
	"fmt"
	"math/rand"
	"testing"
)

// newStartedNTGame n인 게임을 시드 고정 rng 로 시작해 돌려준다
func newStartedNTGame(t *testing.T, n int, seed int64) (*NTGame, *rand.Rand) {
	t.Helper()
	g := NewNTGame("test")
	for i := 0; i < n; i++ {
		if _, err := g.AddPlayer(fmt.Sprintf("P%d", i)); err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
	}
	rng := rand.New(rand.NewSource(seed))
	if err := g.Start(rng); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return g, rng
}

// TestNTStartDeckAndChips 덱 구성(24장 = 공개 1 + 비공개 23)·칩 배분 검증
func TestNTStartDeckAndChips(t *testing.T) {
	g, _ := newStartedNTGame(t, 5, 1)

	if g.Phase != NTPhasePlaying {
		t.Fatalf("phase = %s, want playing", g.Phase)
	}
	if len(g.Deck) != NTDeckSize-1 {
		t.Fatalf("남은 덱 = %d장, want %d (공개 카드 1장 제외)", len(g.Deck), NTDeckSize-1)
	}
	if g.Card < NTCardMin || g.Card > NTCardMax {
		t.Fatalf("공개 카드 = %d, want %d~%d", g.Card, NTCardMin, NTCardMax)
	}
	if g.PotChips != 0 {
		t.Fatalf("시작 얹힌 칩 = %d, want 0", g.PotChips)
	}
	if g.CurrentSeat != g.FirstSeat {
		t.Fatalf("currentSeat=%d, want 선 %d", g.CurrentSeat, g.FirstSeat)
	}

	// 카드 24장(공개 1 + 덱 23)은 전부 3~35 범위의 서로 다른 값
	seen := map[int]bool{g.Card: true}
	for _, c := range g.Deck {
		if c < NTCardMin || c > NTCardMax {
			t.Fatalf("덱 카드 %d, want %d~%d", c, NTCardMin, NTCardMax)
		}
		if seen[c] {
			t.Fatalf("카드 %d 중복", c)
		}
		seen[c] = true
	}
	if len(seen) != NTDeckSize {
		t.Fatalf("사용 카드 종수 = %d, want %d", len(seen), NTDeckSize)
	}

	for _, p := range g.Players {
		if p.Chips != NTStartChips {
			t.Fatalf("seat%d chips = %d, want %d", p.Seat, p.Chips, NTStartChips)
		}
		if p.Cards == nil || len(p.Cards) != 0 {
			t.Fatalf("seat%d cards = %v, want 빈 슬라이스", p.Seat, p.Cards)
		}
		if p.Score != 0 {
			t.Fatalf("seat%d score = %d, want 0", p.Seat, p.Score)
		}
	}
}

// TestNTStartRequiresThree 3인 미만 시작 거부·인원 상한 검증
func TestNTStartRequiresThree(t *testing.T) {
	g := NewNTGame("test")
	g.AddPlayer("P0")
	g.AddPlayer("P1")
	if g.CanStart() {
		t.Fatal("2인이 시작 가능으로 판정됐다")
	}
	rng := rand.New(rand.NewSource(1))
	if err := g.Start(rng); err == nil {
		t.Fatal("2인 시작이 통과했다")
	}
	g.AddPlayer("P2")
	if !g.CanStart() {
		t.Fatal("3인이 시작 불가로 판정됐다")
	}

	for i := 3; i < NTMaxPlayers; i++ {
		if _, err := g.AddPlayer(fmt.Sprintf("P%d", i)); err != nil {
			t.Fatalf("AddPlayer %d: %v", i, err)
		}
	}
	if _, err := g.AddPlayer("P7"); err == nil {
		t.Fatal("8번째 입장이 통과했다")
	}
}

// TestNTSequenceScore 시퀀스 점수 유닛 — 연속 시퀀스는 최솟값만 계산
func TestNTSequenceScore(t *testing.T) {
	cases := []struct {
		cards []int
		chips int
		want  int
	}{
		{[]int{}, 11, -11},                 // 카드 없음 — 칩만큼 마이너스
		{[]int{22, 23, 24}, 0, 22},         // 22·23·24 = 22점 (스펙 예시)
		{[]int{22, 23, 24}, 5, 17},         // 시퀀스 − 칩
		{[]int{35}, 11, 24},                // 단일 카드
		{[]int{3, 5, 6, 30}, 2, 36},        // 3 + 5(5·6 시퀀스) + 30 − 2
		{[]int{3, 4, 5, 33, 34, 35}, 0, 36},// 두 시퀀스: 3 + 33
		{[]int{7, 9, 11}, 0, 27},           // 연속 아님 — 전부 합산
	}
	for _, c := range cases {
		if got := ntScore(c.cards, c.chips); got != c.want {
			t.Fatalf("ntScore(%v, %d) = %d, want %d", c.cards, c.chips, got, c.want)
		}
	}
}

// TestNTPassMovesChipAndTurn 패스는 칩 1개를 얹고 차례를 넘긴다
func TestNTPassMovesChipAndTurn(t *testing.T) {
	g, _ := newStartedNTGame(t, 3, 2)
	first := g.CurrentSeat

	res, err := g.Pass(first)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if res.Kind != "pass" || res.Card != g.Card || res.GainedChips != 0 {
		t.Fatalf("pass 결과 = %+v", res)
	}
	if g.Players[first].Chips != NTStartChips-1 || g.PotChips != 1 {
		t.Fatalf("칩 이동 실패: chips=%d pot=%d", g.Players[first].Chips, g.PotChips)
	}
	if g.CurrentSeat != (first+1)%3 {
		t.Fatalf("차례 = %d, want %d", g.CurrentSeat, (first+1)%3)
	}

	// 차례가 아닌 좌석의 행동은 거부된다
	if _, err := g.Pass(first); err == nil {
		t.Fatal("차례가 아닌 좌석의 패스가 통과했다")
	}
	if _, err := g.Take(first); err == nil {
		t.Fatal("차례가 아닌 좌석의 가져가기가 통과했다")
	}
}

// TestNTPassWithoutChipsRejected 칩 0이면 패스 불가 — 가져가기만 합법
func TestNTPassWithoutChipsRejected(t *testing.T) {
	g, _ := newStartedNTGame(t, 3, 3)
	seat := g.CurrentSeat
	g.Players[seat].Chips = 0

	if _, err := g.Pass(seat); err == nil {
		t.Fatal("칩 0 패스가 통과했다")
	}
	if _, err := g.Take(seat); err != nil {
		t.Fatalf("칩 0 가져가기가 거부됐다: %v", err)
	}
}

// TestNTTakeGetsCardAndPot 가져가기 — 카드+얹힌 칩 획득, 새 카드 공개,
// 가져간 사람부터 다시 시작. 획득 카드는 오름차순 유지.
func TestNTTakeGetsCardAndPot(t *testing.T) {
	g, _ := newStartedNTGame(t, 3, 4)

	// 두 명이 패스해 팟을 2로 만든다
	for i := 0; i < 2; i++ {
		if _, err := g.Pass(g.CurrentSeat); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	taker := g.CurrentSeat
	card := g.Card
	nextCard := g.Deck[0]
	deckBefore := len(g.Deck)
	chipsBefore := g.Players[taker].Chips

	res, err := g.Take(taker)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	if res.Kind != "take" || res.Card != card || res.GainedChips != 2 || res.GameEnded {
		t.Fatalf("take 결과 = %+v", res)
	}
	p := g.Players[taker]
	if len(p.Cards) != 1 || p.Cards[0] != card {
		t.Fatalf("획득 카드 = %v, want [%d]", p.Cards, card)
	}
	if p.Chips != chipsBefore+2 || g.PotChips != 0 {
		t.Fatalf("칩 획득 실패: chips=%d pot=%d", p.Chips, g.PotChips)
	}
	if g.Card != nextCard || len(g.Deck) != deckBefore-1 {
		t.Fatalf("새 카드 공개 실패: card=%d deck=%d", g.Card, len(g.Deck))
	}
	if g.CurrentSeat != taker {
		t.Fatalf("차례 = %d, want 가져간 사람 %d", g.CurrentSeat, taker)
	}

	// 한 장 더 가져가면 오름차순으로 쌓인다
	card2 := g.Card
	if _, err := g.Take(taker); err != nil {
		t.Fatalf("take2: %v", err)
	}
	if len(p.Cards) != 2 || p.Cards[0] > p.Cards[1] {
		t.Fatalf("정렬 실패: %v", p.Cards)
	}
	has := p.Cards[0] == card2 || p.Cards[1] == card2
	if !has {
		t.Fatalf("두 번째 카드 %d 미획득: %v", card2, p.Cards)
	}
}

// TestNTDeckEmptyEndsGameLowestWins 덱 소진 — 점수 확정·최저점 승리 (동점 공동)
func TestNTDeckEmptyEndsGameLowestWins(t *testing.T) {
	g, _ := newStartedNTGame(t, 3, 5)

	// 결정적인 종반을 강제: 덱을 비우고 마지막 공개 카드만 남긴다
	g.Deck = []int{}
	g.Card = 20
	g.PotChips = 3
	taker := g.CurrentSeat
	g.Players[0].Cards = []int{5, 6, 7} // 시퀀스 5 − 칩 11 = -6
	g.Players[1].Cards = []int{30}      // 30 − 11 = 19
	g.Players[2].Cards = []int{}

	res, err := g.Take(taker)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	if !res.GameEnded || g.Phase != NTPhaseGameOver || g.EndReason != "deck_empty" {
		t.Fatalf("종료 실패: res=%+v phase=%s reason=%s", res, g.Phase, g.EndReason)
	}
	if g.Card != 0 || g.CurrentSeat != -1 || g.PotChips != 0 {
		t.Fatalf("종료 상태 이상: card=%d seat=%d pot=%d", g.Card, g.CurrentSeat, g.PotChips)
	}

	// 전원 점수는 ntScore 와 일치해야 한다
	for _, p := range g.Players {
		if want := ntScore(p.Cards, p.Chips); p.Score != want {
			t.Fatalf("seat%d score = %d, want %d", p.Seat, p.Score, want)
		}
	}
	// 승자는 최저점 (동점이면 공동 승)
	best := g.Players[0].Score
	for _, p := range g.Players {
		if p.Score < best {
			best = p.Score
		}
	}
	if len(g.WinnerSeats) == 0 {
		t.Fatal("winnerSeats 비어 있음")
	}
	for _, s := range g.WinnerSeats {
		if g.Players[s].Score != best {
			t.Fatalf("승자 seat%d score=%d ≠ 최저점 %d", s, g.Players[s].Score, best)
		}
	}
	for _, p := range g.Players {
		if p.Score == best {
			found := false
			for _, s := range g.WinnerSeats {
				if s == p.Seat {
					found = true
				}
			}
			if !found {
				t.Fatalf("최저점 seat%d 가 승자에서 누락: %v", p.Seat, g.WinnerSeats)
			}
		}
	}

	// 종료 후 행동은 전부 거부된다
	if _, err := g.Pass(0); err == nil {
		t.Fatal("game_over 중 패스가 통과했다")
	}
	if _, err := g.Take(0); err == nil {
		t.Fatal("game_over 중 가져가기가 통과했다")
	}
}

// TestNTTieSharedWin 동점 공동 승 검증 (칩·카드를 대칭으로 강제)
func TestNTTieSharedWin(t *testing.T) {
	g, _ := newStartedNTGame(t, 3, 6)
	g.Deck = []int{}
	g.Card = 30
	g.PotChips = 0
	taker := g.CurrentSeat
	for _, p := range g.Players {
		p.Cards = []int{}
		p.Chips = 11
	}
	// 가져가는 좌석만 30점 카드를 받아 지고, 나머지 둘은 -11 동점 공동 승
	if _, err := g.Take(taker); err != nil {
		t.Fatalf("take: %v", err)
	}
	if len(g.WinnerSeats) != 2 {
		t.Fatalf("winnerSeats = %v, want 공동 2명", g.WinnerSeats)
	}
	for _, s := range g.WinnerSeats {
		if s == taker {
			t.Fatalf("마지막 카드를 가져간 seat%d 가 승자다: %v", taker, g.WinnerSeats)
		}
	}
}
