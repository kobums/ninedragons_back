package server

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"
)

// ==================== 더 마인드 순수 규칙 테스트 ====================
//
// 허브·타이머·소켓 없이 규칙만 본다. 오름차순 판정, 실수 시 여러 장 동시
// 소각, 라운드 보상(생명/수리검), 수리검 만장일치가 이 파일의 네 기둥이다.

// miTestGame 지정한 손패로 playing 상태의 게임을 만든다 (라운드 r)
func miTestGame(t *testing.T, round int, hands [][]int) *MIGame {
	t.Helper()
	g := NewMIGame("test")
	for i := range hands {
		if _, err := g.AddPlayer(fmt.Sprintf("P%d", i)); err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
	}
	g.MaxRound = miMaxRoundByPlayers(len(hands))
	g.Lives = len(hands)
	g.Stars = MIStartStars
	g.Round = round
	g.Phase = MIPhasePlaying
	g.Ready = true
	g.Pile = []int{}
	for i, hand := range hands {
		sorted := append([]int{}, hand...)
		sort.Ints(sorted)
		g.Players[i].Hand = sorted
	}
	return g
}

// miHands 좌석별 남은 손패
func miHands(g *MIGame) [][]int {
	out := [][]int{}
	for _, p := range g.Players {
		out = append(out, append([]int{}, p.Hand...))
	}
	return out
}

func miSameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestMIMaxRoundTable 인원별 최종 라운드 — 2인 12 / 3인 10 / 4인 8
func TestMIMaxRoundTable(t *testing.T) {
	tests := []struct {
		players int
		want    int
	}{
		{2, 12}, {3, 10}, {4, 8},
	}
	for _, tc := range tests {
		if got := miMaxRoundByPlayers(tc.players); got != tc.want {
			t.Errorf("%d인 최종 라운드 = %d, want %d", tc.players, got, tc.want)
		}
	}
}

// TestMIDeal 라운드 배분 — 인원 × r 장, 좌석별 오름차순, 중복 없음
func TestMIDeal(t *testing.T) {
	deck := miBuildDeck()
	if len(deck) != MIDeckSize || deck[0] != 1 || deck[MIDeckSize-1] != MIDeckSize {
		t.Fatalf("덱 = %d장 (%v...)", len(deck), deck[:3])
	}

	rng := rand.New(rand.NewSource(20260823))
	for _, tc := range []struct{ players, round int }{{2, 12}, {3, 10}, {4, 8}} {
		shuffled := append([]int{}, deck...)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		hands := miDeal(shuffled, tc.players, tc.round)

		seen := map[int]bool{}
		for i, hand := range hands {
			if len(hand) != tc.round {
				t.Fatalf("%d인 %d라운드 seat%d 손패 = %d장", tc.players, tc.round, i, len(hand))
			}
			if !sort.IntsAreSorted(hand) {
				t.Fatalf("손패가 오름차순이 아니다: %v", hand)
			}
			for _, c := range hand {
				if c < 1 || c > MIDeckSize {
					t.Fatalf("범위 밖 카드 %d", c)
				}
				if seen[c] {
					t.Fatalf("중복 카드 %d", c)
				}
				seen[c] = true
			}
		}
	}
}

// TestMIAscendingJudgement 오름차순 판정 표 — 이 게임의 전부다.
// 낸 카드보다 작은 카드를 **다른 사람이** 들고 있었을 때만 실수이고,
// 한 번에 여러 장이 걸려도 생명은 1만 깎인다.
func TestMIAscendingJudgement(t *testing.T) {
	tests := []struct {
		name       string
		hands      [][]int
		plays      []int // 카드를 내는 좌석 순서
		wantPile   []int
		wantBurned []MIBurnedCard
		wantLives  int
		wantPhase  MIPhase
	}{
		{
			name:      "오름차순으로 잘 냈다 — 실수 없음",
			hands:     [][]int{{10, 50}, {20, 60}},
			plays:     []int{0, 1, 0, 1},
			wantPile:  []int{10, 20, 50, 60},
			wantLives: 2,
			wantPhase: MIPhaseRoundEnd, // 전부 냈으니 라운드 성공
		},
		{
			name:       "남의 더 작은 카드 한 장이 걸렸다",
			hands:      [][]int{{10, 70}, {5, 80}},
			plays:      []int{0},
			wantPile:   []int{10},
			wantBurned: []MIBurnedCard{{Seat: 1, Card: 5}},
			wantLives:  1,
			wantPhase:  MIPhasePlaying,
		},
		{
			name:     "여러 사람의 여러 장이 한 번에 소각돼도 생명은 1만",
			hands:    [][]int{{50}, {5, 20, 99}, {9, 60}},
			plays:    []int{0},
			wantPile: []int{50},
			wantBurned: []MIBurnedCard{
				{Seat: 1, Card: 5}, {Seat: 2, Card: 9}, {Seat: 1, Card: 20},
			},
			wantLives: 2, // 3인 시작 생명 3 → 2
			wantPhase: MIPhasePlaying,
		},
		{
			name:      "내 손의 더 작은 카드는 소각 대상이 아니다",
			hands:     [][]int{{10, 20, 30}, {40}},
			plays:     []int{0, 0, 0},
			wantPile:  []int{10, 20, 30},
			wantLives: 2,
			wantPhase: MIPhasePlaying,
		},
		{
			name:       "동시에 냈을 때 뒤에 온 카드가 더 작으면 그것도 실수다",
			hands:      [][]int{{30, 90}, {20, 95}},
			plays:      []int{0},
			wantPile:   []int{30},
			wantBurned: []MIBurnedCard{{Seat: 1, Card: 20}},
			wantLives:  1,
			wantPhase:  MIPhasePlaying,
		},
		{
			name:       "같은 실수를 두 번 하면 생명 2가 빠진다",
			hands:      [][]int{{30, 70}, {20, 40, 60}},
			plays:      []int{0, 0}, // 30(20 소각) → 70(40·60 소각)
			wantPile:   []int{30, 70},
			wantBurned: []MIBurnedCard{{Seat: 1, Card: 40}, {Seat: 1, Card: 60}},
			wantLives:  0,
			wantPhase:  MIPhaseGameOver, // 생명 0 → 즉시 패배
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := miTestGame(t, 4, tc.hands)
			for _, seat := range tc.plays {
				if err := g.Play(seat); err != nil {
					t.Fatalf("seat%d Play: %v", seat, err)
				}
			}
			if !miSameInts(g.Pile, tc.wantPile) {
				t.Errorf("pile = %v, want %v", g.Pile, tc.wantPile)
			}
			if g.Lives != tc.wantLives {
				t.Errorf("lives = %d, want %d", g.Lives, tc.wantLives)
			}
			if g.Phase != tc.wantPhase {
				t.Errorf("phase = %s, want %s", g.Phase, tc.wantPhase)
			}
			if len(tc.wantBurned) == 0 {
				if g.LastMistake != nil {
					t.Errorf("실수가 없어야 하는데 lastMistake = %+v", g.LastMistake)
				}
				return
			}
			if g.LastMistake == nil {
				t.Fatalf("실수가 기록되지 않았다")
			}
			got := g.LastMistake.Burned
			if len(got) != len(tc.wantBurned) {
				t.Fatalf("소각 = %v, want %v", got, tc.wantBurned)
			}
			for i := range got {
				if got[i] != tc.wantBurned[i] {
					t.Fatalf("소각[%d] = %+v, want %+v", i, got[i], tc.wantBurned[i])
				}
			}
			// 소각 카드는 손패에서 실제로 사라져야 한다
			for _, b := range got {
				for _, c := range g.Players[b.Seat].Hand {
					if c == b.Card {
						t.Fatalf("소각된 %d 가 seat%d 손패에 남아 있다", b.Card, b.Seat)
					}
				}
			}
			if g.LastMistake.Message == "" {
				t.Errorf("실수 문구 부재: %+v", g.LastMistake)
			}
		})
	}
}

// TestMIRoundRewards 라운드 보상 표 — 3·6·9 를 마치면 생명 +1,
// 2·5·8 을 마치면 수리검 +1. 상한은 없다.
func TestMIRoundRewards(t *testing.T) {
	tests := []struct {
		round     int
		wantLives int // 시작 2 기준
		wantStars int // 시작 1 기준
	}{
		{1, 2, 1}, {2, 2, 2}, {3, 3, 1}, {4, 2, 1}, {5, 2, 2},
		{6, 3, 1}, {7, 2, 1}, {8, 2, 2}, {9, 3, 1}, {10, 2, 1}, {11, 2, 1},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("%d라운드", tc.round), func(t *testing.T) {
			g := miTestGame(t, tc.round, [][]int{{30}, {70}})
			if err := g.Play(0); err != nil {
				t.Fatalf("Play: %v", err)
			}
			if g.Phase != MIPhasePlaying {
				t.Fatalf("아직 카드가 남았는데 phase = %s", g.Phase)
			}
			if err := g.Play(1); err != nil {
				t.Fatalf("Play: %v", err)
			}
			if g.Phase != MIPhaseRoundEnd {
				t.Fatalf("phase = %s, want round_end", g.Phase)
			}
			if g.Lives != tc.wantLives || g.Stars != tc.wantStars {
				t.Fatalf("보상 후 생명=%d 수리검=%d, want %d/%d",
					g.Lives, g.Stars, tc.wantLives, tc.wantStars)
			}
		})
	}
}

// TestMIClearAndDefeat 종료 판정 — 최종 라운드를 마치면 클리어,
// 생명이 0 이 되면 즉시 패배 (라운드 성공보다 패배가 먼저다).
func TestMIClearAndDefeat(t *testing.T) {
	t.Run("최종 라운드 클리어", func(t *testing.T) {
		g := miTestGame(t, 12, [][]int{{30}, {70}}) // 2인 최종 라운드 12
		g.Play(0)
		g.Play(1)
		if g.Phase != MIPhaseGameOver {
			t.Fatalf("phase = %s, want game_over", g.Phase)
		}
		if g.Result == nil || !g.Result.Cleared || g.Result.Round != 12 {
			t.Fatalf("result = %+v", g.Result)
		}
		if g.EndReason != "cleared" {
			t.Fatalf("endReason = %q", g.EndReason)
		}
	})

	t.Run("생명 0 은 라운드 성공보다 먼저다", func(t *testing.T) {
		// seat0 이 70을 내며 seat1 의 30·40 을 태운다 → 생명 1→0.
		// 그 순간 전원 손패가 비지만 라운드 성공이 아니라 패배다.
		g := miTestGame(t, 4, [][]int{{70}, {30, 40}})
		g.Lives = 1
		g.Play(0)
		if g.Phase != MIPhaseGameOver {
			t.Fatalf("phase = %s, want game_over", g.Phase)
		}
		if g.Result == nil || g.Result.Cleared {
			t.Fatalf("result = %+v, want 실패", g.Result)
		}
		if g.EndReason != "no_lives" {
			t.Fatalf("endReason = %q", g.EndReason)
		}
	})

	t.Run("게임 캡은 실패로 정산한다", func(t *testing.T) {
		g := miTestGame(t, 5, [][]int{{10}, {20}})
		g.ForceEnd()
		if g.Result == nil || g.Result.Cleared || g.EndReason != "time_up" {
			t.Fatalf("result = %+v reason = %q", g.Result, g.EndReason)
		}
	})
}

// TestMIAutoAdvance 라운드 캡 초과 — 전체 최저 카드를 강제로 내보내고
// 정체의 대가로 생명을 1 깎는다. 최저 카드라 추가 소각은 없다.
func TestMIAutoAdvance(t *testing.T) {
	g := miTestGame(t, 3, [][]int{{40, 80}, {15, 90}, {33}})
	g.AutoAdvance()

	if g.LastPlayed != 15 || !miSameInts(g.Pile, []int{15}) {
		t.Fatalf("자동 진행 후 lastPlayed=%d pile=%v", g.LastPlayed, g.Pile)
	}
	if g.Lives != 2 {
		t.Fatalf("생명 = %d, want 2", g.Lives)
	}
	if g.LastMistake == nil || g.LastMistake.Seat != 1 || g.LastMistake.Played != 15 {
		t.Fatalf("자동 진행 기록 = %+v", g.LastMistake)
	}
	if len(g.LastMistake.Burned) != 0 {
		t.Fatalf("최저 카드였는데 소각이 있다: %v", g.LastMistake.Burned)
	}
	if !miSameInts(miHands(g)[1], []int{90}) {
		t.Fatalf("seat1 손패 = %v, want [90]", miHands(g)[1])
	}

	// playing 이 아니면 아무 일도 없다
	g.Phase = MIPhaseRoundEnd
	before := g.Lives
	g.AutoAdvance()
	if g.Lives != before {
		t.Fatalf("round_end 에서 자동 진행이 돌았다")
	}
}

// TestMIStarUnanimous 수리검 — 만장일치일 때만 발동하고, 한 명이라도
// 거절하거나 창이 지나면 무산된다. 발동하면 전원이 최저 카드 1장을 버리고
// 생명은 소모하지 않는다.
func TestMIStarUnanimous(t *testing.T) {
	now := time.Now()

	t.Run("만장일치 발동", func(t *testing.T) {
		g := miTestGame(t, 3, [][]int{{20, 70}, {35, 80}, {44, 95}})
		if err := g.ProposeStar(0, now, 20*time.Second); err != nil {
			t.Fatalf("ProposeStar: %v", err)
		}
		if g.StarVote == nil || g.StarVote.Proposer != 0 ||
			!miSameInts(g.StarVote.Accepted, []int{0}) {
			t.Fatalf("제안 직후 투표 = %+v", g.StarVote)
		}
		if g.StarVote.EndsAt <= now.UnixMilli() {
			t.Fatalf("endsAt = %d (미래여야 한다)", g.StarVote.EndsAt)
		}

		if err := g.AcceptStar(1); err != nil {
			t.Fatalf("AcceptStar: %v", err)
		}
		if g.StarVote == nil {
			t.Fatal("2/3 에서 발동해버렸다 — 만장일치가 아니다")
		}
		if err := g.AcceptStar(1); err == nil {
			t.Fatal("중복 찬성이 거절되지 않았다")
		}

		lives, stars := g.Lives, g.Stars
		if err := g.AcceptStar(2); err != nil {
			t.Fatalf("AcceptStar: %v", err)
		}
		if g.StarVote != nil {
			t.Fatalf("만장일치인데 투표가 남아 있다: %+v", g.StarVote)
		}
		if g.Stars != stars-1 {
			t.Fatalf("수리검 = %d, want %d", g.Stars, stars-1)
		}
		if g.Lives != lives {
			t.Fatalf("수리검은 생명을 쓰지 않는다: %d → %d", lives, g.Lives)
		}
		want := [][]int{{70}, {80}, {95}}
		for i, hand := range miHands(g) {
			if !miSameInts(hand, want[i]) {
				t.Fatalf("seat%d 손패 = %v, want %v", i, hand, want[i])
			}
		}
		// 버린 카드는 중앙 더미에 쌓지 않는다 (낸 것이 아니라 버린 것)
		if len(g.Pile) != 0 || g.LastPlayed != 0 {
			t.Fatalf("수리검 카드가 더미에 들어갔다: pile=%v last=%d", g.Pile, g.LastPlayed)
		}
	})

	t.Run("한 명이라도 거절하면 무산", func(t *testing.T) {
		g := miTestGame(t, 3, [][]int{{20, 70}, {35, 80}, {44, 95}})
		g.ProposeStar(0, now, 20*time.Second)
		g.AcceptStar(1)
		if err := g.DeclineStar(2); err != nil {
			t.Fatalf("DeclineStar: %v", err)
		}
		if g.StarVote != nil {
			t.Fatalf("거절했는데 투표가 남아 있다: %+v", g.StarVote)
		}
		if g.Stars != MIStartStars {
			t.Fatalf("무산인데 수리검이 줄었다: %d", g.Stars)
		}
		if err := g.AcceptStar(1); err == nil {
			t.Fatal("없는 투표에 찬성이 통과했다")
		}
	})

	t.Run("시간이 지나면 무산 (지나간 발화는 무시)", func(t *testing.T) {
		g := miTestGame(t, 3, [][]int{{20}, {35}, {44}})
		g.ProposeStar(0, now, 20*time.Second)
		seq := g.StarVote.Seq
		if g.ExpireStar(seq + 1) {
			t.Fatal("다른 일련번호의 만료가 먹혔다")
		}
		if !g.ExpireStar(seq) {
			t.Fatal("만료가 처리되지 않았다")
		}
		if g.StarVote != nil || g.Stars != MIStartStars {
			t.Fatalf("만료 후 투표=%+v 수리검=%d", g.StarVote, g.Stars)
		}
		if g.ExpireStar(seq) {
			t.Fatal("두 번째 만료가 또 처리됐다")
		}
	})

	t.Run("수리검이 없으면 제안할 수 없다", func(t *testing.T) {
		g := miTestGame(t, 3, [][]int{{20}, {35}})
		g.Stars = 0
		if err := g.ProposeStar(0, now, 20*time.Second); err == nil {
			t.Fatal("수리검 0 인데 제안이 통과했다")
		}
		g.Stars = 1
		if err := g.ProposeStar(0, now, 20*time.Second); err != nil {
			t.Fatalf("ProposeStar: %v", err)
		}
		if err := g.ProposeStar(1, now, 20*time.Second); err == nil {
			t.Fatal("중복 제안이 통과했다")
		}
	})

	t.Run("수리검으로 라운드가 끝날 수도 있다", func(t *testing.T) {
		g := miTestGame(t, 2, [][]int{{20}, {35}}) // 2라운드 마치면 수리검 +1
		g.ProposeStar(0, now, 20*time.Second)
		g.AcceptStar(1)
		if g.Phase != MIPhaseRoundEnd {
			t.Fatalf("phase = %s, want round_end", g.Phase)
		}
		// 수리검 1 사용 → 0, 2라운드 보상 +1 → 1
		if g.Stars != 1 {
			t.Fatalf("수리검 = %d, want 1", g.Stars)
		}
	})
}

// TestMIPlayGuards 규약 위반은 상태를 건드리지 않고 error 로만 돌려준다
func TestMIPlayGuards(t *testing.T) {
	g := miTestGame(t, 2, [][]int{{10, 20}, {30, 40}})

	g.Phase = MIPhaseReady
	if err := g.Play(0); err == nil {
		t.Fatal("카운트다운 중인데 카드가 나갔다")
	}
	g.Phase = MIPhasePlaying

	if err := g.Play(9); err == nil {
		t.Fatal("없는 좌석의 카드가 나갔다")
	}
	g.Players[0].Hand = []int{}
	if err := g.Play(0); err == nil {
		t.Fatal("빈 손패인데 카드가 나갔다")
	}
	if len(g.Pile) != 0 {
		t.Fatalf("거절된 요청이 더미를 건드렸다: %v", g.Pile)
	}
}

// TestMIStartAndBeginRound 시작·라운드 전환 — 생명은 인원수, 수리검 1,
// 라운드마다 손패가 새로 배분되고 중앙 더미는 초기화된다.
func TestMIStartAndBeginRound(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	g := NewMIGame("start")
	for i := 0; i < 4; i++ {
		g.AddPlayer(fmt.Sprintf("P%d", i))
	}
	if err := g.Start(rng); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if g.Lives != 4 || g.Stars != MIStartStars || g.MaxRound != 8 {
		t.Fatalf("시작 상태 생명=%d 수리검=%d 최종=%d", g.Lives, g.Stars, g.MaxRound)
	}
	if g.Phase != MIPhaseReady || g.Round != 1 {
		t.Fatalf("phase=%s round=%d, want ready/1", g.Phase, g.Round)
	}
	for _, p := range g.Players {
		if len(p.Hand) != 1 {
			t.Fatalf("1라운드 손패 = %v", p.Hand)
		}
	}

	g.BeginPlaying()
	if g.Phase != MIPhasePlaying {
		t.Fatalf("phase = %s, want playing", g.Phase)
	}

	// 라운드 2 — 손패 2장씩, 더미 초기화
	g.Pile = []int{5}
	g.LastPlayed = 5
	g.LastMistake = &MIMistake{Seat: 0, Burned: []MIBurnedCard{}}
	g.BeginRound(rng)
	if g.Round != 2 || g.Phase != MIPhaseReady {
		t.Fatalf("round=%d phase=%s", g.Round, g.Phase)
	}
	if len(g.Pile) != 0 || g.LastPlayed != 0 || g.LastMistake != nil {
		t.Fatalf("라운드 전환에 더미·실수가 남았다: pile=%v last=%d mistake=%+v",
			g.Pile, g.LastPlayed, g.LastMistake)
	}
	for _, p := range g.Players {
		if len(p.Hand) != 2 {
			t.Fatalf("2라운드 손패 = %v", p.Hand)
		}
	}

	// 2명 미만은 시작할 수 없다
	solo := NewMIGame("solo")
	solo.AddPlayer("혼자")
	if err := solo.Start(rng); err == nil {
		t.Fatal("1인이 시작됐다")
	}
}

// TestMISmallerThan 소각 대상 분리 (순수 헬퍼)
func TestMISmallerThan(t *testing.T) {
	tests := []struct {
		hand        []int
		card        int
		wantSmaller []int
		wantRest    []int
	}{
		{[]int{5, 20, 99}, 50, []int{5, 20}, []int{99}},
		{[]int{60, 70}, 50, []int{}, []int{60, 70}},
		{[]int{1, 2, 3}, 50, []int{1, 2, 3}, []int{}},
		{[]int{50}, 50, []int{}, []int{50}}, // 같은 수는 없지만 경계는 확인한다
		{[]int{}, 50, []int{}, []int{}},
	}
	for _, tc := range tests {
		smaller, rest := miSmallerThan(tc.hand, tc.card)
		if !miSameInts(smaller, tc.wantSmaller) || !miSameInts(rest, tc.wantRest) {
			t.Errorf("miSmallerThan(%v, %d) = %v/%v, want %v/%v",
				tc.hand, tc.card, smaller, rest, tc.wantSmaller, tc.wantRest)
		}
	}
}
