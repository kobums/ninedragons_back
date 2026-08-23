package server

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// ==================== 순수 규칙 테스트 헬퍼 ====================

// kgNewTestGame n 인 게임을 시작 상태(1라운드 비딩)로 만든다.
// 손패·비드는 각 테스트가 결정적으로 덮어쓴다.
func kgNewTestGame(t *testing.T, n int) (*KGGame, *rand.Rand) {
	t.Helper()
	g := NewKGGame("kg-test")
	for i := 0; i < n; i++ {
		if _, err := g.AddPlayer(fmt.Sprintf("P%d", i)); err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
	}
	rng := rand.New(rand.NewSource(17))
	if err := g.Start(rng); err != nil {
		t.Fatalf("Start: %v", err)
	}
	g.DrainEvents()
	return g, rng
}

// kgEventText 이벤트 큐를 비우고 문구를 이어붙인다 (문구 검증용)
func kgEventText(g *KGGame) string {
	msgs := []string{}
	for _, ev := range g.DrainEvents() {
		msgs = append(msgs, ev.Kind+":"+ev.Message)
	}
	return strings.Join(msgs, "\n")
}

func kgNum(suit KGSuit, rank int) KGCard {
	return KGCard{Kind: KGKindNumber, Suit: suit, Rank: rank}
}

var (
	kgEscape    = KGCard{Kind: KGKindEscape}
	kgPirate    = KGCard{Kind: KGKindPirate}
	kgMermaid   = KGCard{Kind: KGKindMermaid}
	kgSkullKing = KGCard{Kind: KGKindSkullKing}
)

// kgPlays 좌석 0부터 순서대로 낸 트릭을 만든다
func kgPlays(cards ...KGCard) []KGTrickPlay {
	plays := []KGTrickPlay{}
	for i, c := range cards {
		plays = append(plays, KGTrickPlay{Seat: i, Card: c})
	}
	return plays
}

// ==================== 덱 / 최대 라운드 ====================

// TestKGDeckAndMaxRound 덱 구성과 인원별 최대 라운드 — 8인 8라운드,
// 7인 9라운드, 6인 이하 10라운드. 최대 소요 장수가 덱을 넘지 않아야 한다.
func TestKGDeckAndMaxRound(t *testing.T) {
	deck := kgBuildDeck()
	kinds := map[KGCardKind]int{}
	suits := map[KGSuit]int{}
	seen := map[KGCard]int{}
	for _, c := range deck {
		kinds[c.Kind]++
		if c.Kind == KGKindNumber {
			suits[c.Suit]++
			seen[c]++
			if c.Rank < 1 || c.Rank > KGSuitRankMax {
				t.Fatalf("숫자 카드 랭크 이상: %+v", c)
			}
		} else if c.Suit != KGSuitNone || c.Rank != 0 {
			t.Fatalf("특수 카드에 무늬·랭크가 붙었다: %+v", c)
		}
	}
	if kinds[KGKindNumber] != 52 {
		t.Fatalf("숫자 카드 = %d장, want 52", kinds[KGKindNumber])
	}
	for _, suit := range kgSuits {
		if suits[suit] != KGSuitRankMax {
			t.Fatalf("%s 무늬 = %d장, want %d", kgSuitLabel(suit), suits[suit], KGSuitRankMax)
		}
	}
	for card, n := range seen {
		if n != 1 {
			t.Fatalf("중복 숫자 카드: %+v ×%d", card, n)
		}
	}
	if kinds[KGKindEscape] != KGEscapeCount || kinds[KGKindPirate] != KGPirateCount ||
		kinds[KGKindMermaid] != KGMermaidCount || kinds[KGKindSkullKing] != KGSkullKingCard {
		t.Fatalf("특수 카드 구성 이상: %v", kinds)
	}

	want := map[int]int{2: 10, 3: 10, 4: 10, 5: 10, 6: 10, 7: 9, 8: 8}
	for n, r := range want {
		if got := kgMaxRound(n); got != r {
			t.Fatalf("%d인 최대 라운드 = %d, want %d", n, got, r)
		}
		if need := n * r; need > len(deck) {
			t.Fatalf("%d인 %d라운드에 %d장 필요 — 덱 %d장을 넘는다", n, r, need, len(deck))
		}
	}
}

// ==================== 서열 ====================

// TestKGTrickWinner 서열 판정 — 스컬킹 > 해적 > 인어 > 숫자, 인어 > 스컬킹.
// 검정은 상시 트럼프이고, 리드도 트럼프도 아닌 숫자는 절대 이기지 못한다.
func TestKGTrickWinner(t *testing.T) {
	cases := []struct {
		name  string
		plays []KGTrickPlay
		lead  KGSuit
		want  int
	}{
		{"리드 무늬 최고", kgPlays(kgNum(KGSuitGreen, 5), kgNum(KGSuitGreen, 9)), KGSuitGreen, 1},
		{"검정 트럼프", kgPlays(kgNum(KGSuitGreen, 13), kgNum(KGSuitBlack, 2)), KGSuitGreen, 1},
		{"검정끼리는 숫자", kgPlays(kgNum(KGSuitBlack, 2), kgNum(KGSuitBlack, 11)), KGSuitBlack, 1},
		{"엉뚱한 무늬는 못 이긴다", kgPlays(kgNum(KGSuitGreen, 3), kgNum(KGSuitYellow, 13)), KGSuitGreen, 0},
		{"해적이 숫자를 이긴다", kgPlays(kgNum(KGSuitBlack, 13), kgPirate), KGSuitBlack, 1},
		{"인어가 숫자를 이긴다", kgPlays(kgNum(KGSuitBlack, 13), kgMermaid), KGSuitBlack, 1},
		{"스컬킹이 해적을 이긴다", kgPlays(kgPirate, kgSkullKing), KGSuitNone, 1},
		{"해적이 인어를 이긴다", kgPlays(kgMermaid, kgPirate), KGSuitNone, 1},
		{"인어가 스컬킹을 이긴다", kgPlays(kgSkullKing, kgMermaid), KGSuitNone, 1},
		{"셋 다 나오면 인어", kgPlays(kgPirate, kgSkullKing, kgMermaid), KGSuitNone, 2},
		{"먼저 낸 해적", kgPlays(kgPirate, kgPirate), KGSuitNone, 0},
		{"탈출은 최약", kgPlays(kgEscape, kgNum(KGSuitGreen, 1)), KGSuitGreen, 1},
		{"전원 탈출은 첫 사람", kgPlays(kgEscape, kgEscape, kgEscape), KGSuitNone, 0},
		{"탈출 리드 후 첫 숫자가 리드 무늬", kgPlays(kgEscape, kgNum(KGSuitPurple, 4), kgNum(KGSuitGreen, 12)), KGSuitPurple, 1},
	}
	for _, tc := range cases {
		if got := kgTrickWinner(tc.plays, tc.lead); got != tc.want {
			t.Fatalf("%s: 승자 인덱스 = %d, want %d", tc.name, got, tc.want)
		}
	}
	if kgTrickWinner(nil, KGSuitNone) != -1 {
		t.Fatal("빈 트릭의 승자는 -1 이어야 한다")
	}
}

// ==================== 따라내기 ====================

// TestKGFollowSuit 따라내기 의무 — 리드 무늬 숫자를 쥐고 있으면 그 무늬나
// 특수 카드만 낼 수 있다. 검정(트럼프)도 예외가 아니다.
func TestKGFollowSuit(t *testing.T) {
	hand := []KGCard{
		kgEscape,               // 0
		kgNum(KGSuitGreen, 3),  // 1
		kgNum(KGSuitYellow, 5), // 2
		kgNum(KGSuitBlack, 7),  // 3
		kgPirate,               // 4
		kgNum(KGSuitGreen, 11), // 5
		kgNum(KGSuitPurple, 2), // 6
	}

	got := kgLegalIndexes(hand, KGSuitGreen)
	want := []int{0, 1, 4, 5}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("초록 리드 합법 인덱스 = %v, want %v", got, want)
	}
	// 검정 리드에서도 검정 숫자를 쥐고 있으면 다른 색은 못 낸다
	got = kgLegalIndexes(hand, KGSuitBlack)
	want = []int{0, 3, 4}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("검정 리드 합법 인덱스 = %v, want %v", got, want)
	}
	// 보라를 쥐고 있으면 보라 리드에서도 따라내야 한다
	got = kgLegalIndexes(hand, KGSuitPurple)
	want = []int{0, 4, 6}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("보라 리드 합법 인덱스 = %v, want %v", got, want)
	}
	// 리드 무늬가 손에 없으면 전부 자유 (검정 트럼프도 이때만 낼 수 있다)
	noPurple := []KGCard{kgEscape, kgNum(KGSuitGreen, 3), kgNum(KGSuitBlack, 7)}
	if len(kgLegalIndexes(noPurple, KGSuitPurple)) != len(noPurple) {
		t.Fatal("리드 무늬가 없으면 전부 낼 수 있어야 한다")
	}
	// 리드 미정이면 전부 자유
	if len(kgLegalIndexes(hand, KGSuitNone)) != len(hand) {
		t.Fatal("리드 미정에는 전부 낼 수 있어야 한다")
	}
	if kgIsLegalPlay(hand, KGSuitGreen, 2) {
		t.Fatal("초록을 쥐고 노랑을 내는 게 통과했다")
	}
	if !kgIsLegalPlay(hand, KGSuitGreen, 4) {
		t.Fatal("특수 카드는 언제든 낼 수 있어야 한다")
	}
}

// ==================== 보너스 ====================

// TestKGTrickBonus 13 획득·인어의 스컬킹 포획·스컬킹의 해적 포획
func TestKGTrickBonus(t *testing.T) {
	cases := []struct {
		name   string
		plays  []KGTrickPlay
		winner int
		want   int
	}{
		{"색 13 +10", kgPlays(kgNum(KGSuitGreen, 13), kgNum(KGSuitBlack, 2)), 1, 10},
		{"검정 13 +20", kgPlays(kgNum(KGSuitBlack, 13), kgPirate), 1, 20},
		{"13 두 장", kgPlays(kgNum(KGSuitGreen, 13), kgNum(KGSuitBlack, 13)), 1, 30},
		{"인어가 스컬킹 포획 +50", kgPlays(kgSkullKing, kgMermaid), 1, 50},
		{"스컬킹이 해적 2명 포획 +60", kgPlays(kgPirate, kgPirate, kgSkullKing), 2, 60},
		{"스컬킹+해적+13", kgPlays(kgPirate, kgNum(KGSuitBlack, 13), kgSkullKing), 2, 50},
		{"보너스 없음", kgPlays(kgNum(KGSuitGreen, 5), kgNum(KGSuitGreen, 9)), 1, 0},
		{"인어가 없으면 스컬킹 보너스 없음", kgPlays(kgMermaid, kgSkullKing), 1, 0},
	}
	for _, tc := range cases {
		got, notes := kgTrickBonus(tc.plays, tc.winner)
		if got != tc.want {
			t.Fatalf("%s: 보너스 = %d, want %d (%v)", tc.name, got, tc.want, notes)
		}
		if tc.want > 0 && len(notes) == 0 {
			t.Fatalf("%s: 보너스 사유 문구가 비었다", tc.name)
		}
	}
	if got, _ := kgTrickBonus(kgPlays(kgSkullKing), 5); got != 0 {
		t.Fatalf("범위 밖 승자 인덱스 = %d, want 0", got)
	}
}

// ==================== 정산 ====================

// TestKGSettleRound 비드 ≥ 1 은 20×비드(+보너스), 비드 0 은 10×라운드.
// 보너스는 비드를 맞힌 경우에만 가산된다.
func TestKGSettleRound(t *testing.T) {
	g, _ := kgNewTestGame(t, 4)
	g.Round = 3
	setup := []struct{ bid, tricks, bonus int }{
		{2, 2, 20}, // 적중 + 보너스 → 40 + 20
		{0, 0, 0},  // 0 적중 → 10 × 3
		{0, 1, 0},  // 0 실패 → -10 × 3
		{3, 1, 50}, // 실패 → -10 × 2 (보너스 무시)
	}
	for i, s := range setup {
		g.Players[i].Bid = s.bid
		g.Players[i].Tricks = s.tricks
		g.Players[i].Bonus = s.bonus
		g.Players[i].Score = 0
	}
	g.settleRound()

	want := []int{60, 30, -30, -20}
	if g.RoundResult == nil || len(g.RoundResult.Rows) != 4 {
		t.Fatalf("정산표 = %+v", g.RoundResult)
	}
	for i, row := range g.RoundResult.Rows {
		if row.Seat != i || row.Delta != want[i] || row.Total != want[i] {
			t.Fatalf("seat%d 정산 = %+v, want delta %d", i, row, want[i])
		}
		if row.Bid != setup[i].bid || row.Tricks != setup[i].tricks {
			t.Fatalf("seat%d 정산표에 비드·트릭 누락: %+v", i, row)
		}
	}
	if g.Phase != KGPhaseRoundEnd || g.CurrentSeat != -1 {
		t.Fatalf("정산 후 phase=%s current=%d", g.Phase, g.CurrentSeat)
	}
	if !strings.Contains(g.RoundResult.Message, "정산") {
		t.Fatalf("정산 문구 = %q", g.RoundResult.Message)
	}
	if text := kgEventText(g); !strings.Contains(text, "round_end:") {
		t.Fatalf("정산 이벤트 부재:\n%s", text)
	}
}

// ==================== 비딩 ====================

// TestKGBidding 비드 범위·중복 제출 거부·전원 제출 시 일괄 공개
func TestKGBidding(t *testing.T) {
	g, _ := kgNewTestGame(t, 3)
	if g.Phase != KGPhaseBidding || g.Round != 1 {
		t.Fatalf("시작 직후 phase=%s round=%d", g.Phase, g.Round)
	}
	for _, p := range g.Players {
		if p.Bid != -1 {
			t.Fatalf("시작 직후 seat%d 비드 = %d, want -1", p.Seat, p.Bid)
		}
		if len(p.Hand) != 1 {
			t.Fatalf("1라운드 손패 = %d장, want 1", len(p.Hand))
		}
	}
	if err := g.SubmitBid(0, 2); err == nil {
		t.Fatal("1라운드에 비드 2가 통과했다")
	}
	if err := g.SubmitBid(0, -1); err == nil {
		t.Fatal("음수 비드가 통과했다")
	}
	if err := g.SubmitBid(9, 0); err == nil {
		t.Fatal("범위 밖 좌석의 비드가 통과했다")
	}
	if err := g.SubmitBid(0, 1); err != nil {
		t.Fatalf("SubmitBid: %v", err)
	}
	if err := g.SubmitBid(0, 0); err == nil {
		t.Fatal("비드 재제출이 통과했다")
	}
	if g.BidsRevealed {
		t.Fatal("전원 제출 전에 비드가 공개됐다")
	}
	g.SubmitBid(1, 0)
	g.SubmitBid(2, 1)
	if !g.BidsRevealed || g.Phase != KGPhasePlaying {
		t.Fatalf("전원 제출 후 revealed=%v phase=%s", g.BidsRevealed, g.Phase)
	}
	if g.TrickNo != 1 || g.CurrentSeat != g.LeadSeat || g.CurrentSeat < 0 {
		t.Fatalf("플레이 개시 상태: trick=%d current=%d lead=%d",
			g.TrickNo, g.CurrentSeat, g.LeadSeat)
	}
	if text := kgEventText(g); !strings.Contains(text, "bids_revealed:") {
		t.Fatalf("비드 공개 이벤트 부재:\n%s", text)
	}
	if err := g.SubmitBid(0, 0); err == nil {
		t.Fatal("플레이 단계에서 비드가 통과했다")
	}
}

// TestKGForceBids 비딩 마감은 미제출 좌석을 0으로 채우고 공개한다
func TestKGForceBids(t *testing.T) {
	g, _ := kgNewTestGame(t, 4)
	g.SubmitBid(2, 1)
	g.DrainEvents()
	g.ForceBids()

	if !g.BidsRevealed || g.Phase != KGPhasePlaying {
		t.Fatalf("마감 후 revealed=%v phase=%s", g.BidsRevealed, g.Phase)
	}
	wants := []int{0, 0, 1, 0}
	for i, w := range wants {
		if g.Players[i].Bid != w {
			t.Fatalf("seat%d 비드 = %d, want %d", i, g.Players[i].Bid, w)
		}
	}
	text := kgEventText(g)
	if strings.Count(text, "afk:") != 3 {
		t.Fatalf("자동 제출 이벤트가 3건이 아니다:\n%s", text)
	}
	if !strings.Contains(text, "자동") {
		t.Fatalf("자동 제출 문구가 한글이 아니다:\n%s", text)
	}
}

// ==================== 플레이 ====================

// TestKGPlayValidation 차례·인덱스·따라내기 검증
func TestKGPlayValidation(t *testing.T) {
	g, _ := kgNewTestGame(t, 3)
	g.ForceBids()
	g.DrainEvents()

	lead := g.CurrentSeat
	next := (lead + 1) % 3
	g.Players[lead].Hand = []KGCard{kgNum(KGSuitGreen, 7)}
	g.Players[next].Hand = []KGCard{kgNum(KGSuitGreen, 2), kgNum(KGSuitYellow, 9)}

	if err := g.Play(next, 0); err == nil {
		t.Fatal("차례가 아닌 좌석의 플레이가 통과했다")
	}
	if err := g.Play(lead, 5); err == nil {
		t.Fatal("범위 밖 인덱스가 통과했다")
	}
	if err := g.Play(lead, 0); err != nil {
		t.Fatalf("Play: %v", err)
	}
	if g.LeadSuit != KGSuitGreen {
		t.Fatalf("리드 무늬 = %q, want green", g.LeadSuit)
	}
	err := g.Play(next, 1)
	if err == nil {
		t.Fatal("초록을 쥐고 노랑을 내는 게 통과했다")
	}
	if !strings.Contains(err.Error(), "따라내야") {
		t.Fatalf("따라내기 에러 문구 = %q", err.Error())
	}
	if err := g.Play(next, 0); err != nil {
		t.Fatalf("Play(따라내기): %v", err)
	}
}

// TestKGTrickResolution 트릭이 차면 승자에게 트릭·보너스가 붙고
// 다음 트릭의 리드가 승자로 넘어간다
func TestKGTrickResolution(t *testing.T) {
	g, _ := kgNewTestGame(t, 3)
	g.Round = 2
	g.ForceBids()
	g.DrainEvents()

	// 결정적 구도: 좌석 순서대로 초록5 → 검정13 → 탈출. 검정 13 이 이기고 +20.
	lead := g.CurrentSeat
	s1, s2 := (lead+1)%3, (lead+2)%3
	g.Players[lead].Hand = []KGCard{kgNum(KGSuitGreen, 5), kgEscape}
	g.Players[s1].Hand = []KGCard{kgNum(KGSuitBlack, 13), kgEscape}
	g.Players[s2].Hand = []KGCard{kgEscape, kgEscape}
	g.TrickNo = 1

	for _, seat := range []int{lead, s1, s2} {
		if err := g.Play(seat, 0); err != nil {
			t.Fatalf("seat%d Play: %v", seat, err)
		}
	}
	if g.Players[s1].Tricks != 1 {
		t.Fatalf("승자 트릭 수 = %d, want 1", g.Players[s1].Tricks)
	}
	if g.Players[s1].Bonus != 20 {
		t.Fatalf("검정 13 보너스 = %d, want 20", g.Players[s1].Bonus)
	}
	if g.LastTrick == nil || g.LastTrick.WinnerSeat != s1 || len(g.LastTrick.Cards) != 3 {
		t.Fatalf("lastTrick = %+v", g.LastTrick)
	}
	if g.CurrentSeat != s1 || g.LeadSeat != s1 || g.TrickNo != 2 {
		t.Fatalf("다음 트릭 리드 = %d/%d (trick %d), want %d", g.CurrentSeat, g.LeadSeat, g.TrickNo, s1)
	}
	if len(g.Trick) != 0 || g.LeadSuit != KGSuitNone {
		t.Fatalf("트릭 정리 실패: %v / %q", g.Trick, g.LeadSuit)
	}
	if text := kgEventText(g); !strings.Contains(text, "trick_won:") ||
		!strings.Contains(text, "보너스 +20") {
		t.Fatalf("트릭 이벤트 문구 이상:\n%s", text)
	}
}

// ==================== 완주 ====================

// TestKGForceCompleteGame 전원 방치(자동 비드·자동 플레이)만으로 2·5·8인
// 게임이 끝까지 완주하는지 — 라운드 수·트릭 수·손패 소진·우승자 확정을 함께 본다.
func TestKGForceCompleteGame(t *testing.T) {
	for _, n := range []int{2, 5, 7, 8} {
		g, rng := kgNewTestGame(t, n)
		wantMax := kgMaxRound(n)
		if g.MaxRound != wantMax {
			t.Fatalf("%d인 maxRound = %d, want %d", n, g.MaxRound, wantMax)
		}

		guard := 0
		for g.Phase != KGPhaseGameOver {
			guard++
			if guard > 5000 {
				t.Fatalf("%d인 게임이 진행 불가 상태에 빠졌다 (phase=%s round=%d)",
					n, g.Phase, g.Round)
			}
			switch g.Phase {
			case KGPhaseBidding:
				if len(g.Players[0].Hand) != g.Round {
					t.Fatalf("%d인 %d라운드 손패 = %d장", n, g.Round, len(g.Players[0].Hand))
				}
				g.ForceBids()
			case KGPhasePlaying:
				g.ForcePlay()
			case KGPhaseRoundEnd:
				tricks := 0
				for _, p := range g.Players {
					tricks += p.Tricks
					if len(p.Hand) != 0 {
						t.Fatalf("%d인 %d라운드 종료 후 seat%d 손패 %d장 잔류",
							n, g.Round, p.Seat, len(p.Hand))
					}
				}
				if tricks != g.Round {
					t.Fatalf("%d인 %d라운드 트릭 합 = %d, want %d", n, g.Round, tricks, g.Round)
				}
				g.NextRound(rng)
			}
			g.DrainEvents()
		}

		if g.Round != wantMax {
			t.Fatalf("%d인 종료 라운드 = %d, want %d", n, g.Round, wantMax)
		}
		if len(g.Winners) == 0 {
			t.Fatalf("%d인 종료에 우승자가 없다", n)
		}
		best := g.BestScore()
		for _, seat := range g.Winners {
			if g.Players[seat].Score != best {
				t.Fatalf("%d인 우승자 점수 불일치: seat%d %d vs %d",
					n, seat, g.Players[seat].Score, best)
			}
		}
		if g.WinnerNames() == "" {
			t.Fatalf("%d인 우승자 이름이 비었다", n)
		}
	}
}

// TestKGTieWinners 총점이 같으면 공동 우승이다
func TestKGTieWinners(t *testing.T) {
	g, _ := kgNewTestGame(t, 3)
	g.Round = g.MaxRound
	g.Phase = KGPhaseRoundEnd
	g.Players[0].Score = 40
	g.Players[1].Score = 40
	g.Players[2].Score = 10
	g.NextRound(rand.New(rand.NewSource(1)))

	if g.Phase != KGPhaseGameOver {
		t.Fatalf("마지막 라운드 후 phase = %s", g.Phase)
	}
	if fmt.Sprint(g.Winners) != "[0 1]" {
		t.Fatalf("공동 우승 = %v, want [0 1]", g.Winners)
	}
	if text := kgEventText(g); !strings.Contains(text, "공동 우승") {
		t.Fatalf("공동 우승 문구 부재:\n%s", text)
	}
}

// ==================== 대기실 ====================

// TestKGLobbySeats 입장·퇴장 시 좌석 압축과 최대 라운드 재계산
func TestKGLobbySeats(t *testing.T) {
	g := NewKGGame("kg-lobby")
	if g.CanStart() {
		t.Fatal("빈 대기실이 시작 가능하다")
	}
	for i := 0; i < KGMaxPlayers; i++ {
		if _, err := g.AddPlayer(fmt.Sprintf("P%d", i)); err != nil {
			t.Fatalf("AddPlayer(%d): %v", i, err)
		}
	}
	if g.MaxRound != 8 {
		t.Fatalf("8인 maxRound = %d, want 8", g.MaxRound)
	}
	if _, err := g.AddPlayer("초과"); err == nil {
		t.Fatal("정원 초과 입장이 통과했다")
	}
	g.RemovePlayer(0)
	if len(g.Players) != 7 || g.Players[0].Name != "P1" || g.Players[0].Seat != 0 {
		t.Fatalf("좌석 압축 실패: %d명 / %+v", len(g.Players), g.Players[0])
	}
	if g.MaxRound != 9 {
		t.Fatalf("7인 maxRound = %d, want 9", g.MaxRound)
	}
	if !g.CanStart() {
		t.Fatal("7인이 시작 불가로 나온다")
	}
}

// ==================== 봇 두뇌 ====================

// TestKGBotBid 손패 강도 추정 — 해적·스컬킹·인어 각 1, 검정 10 이상 1,
// 색 12·13 은 0.5. 라운드 범위로 잘린다.
func TestKGBotBid(t *testing.T) {
	hand := []KGCard{
		kgPirate, kgSkullKing, kgMermaid, // 3
		kgNum(KGSuitBlack, 10),  // 1
		kgNum(KGSuitBlack, 3),   // 0
		kgNum(KGSuitGreen, 13),  // 0.5
		kgNum(KGSuitYellow, 12), // 0.5
		kgNum(KGSuitPurple, 4),  // 0
		kgEscape,                // 0
	}
	if got := kgBotBid(kgBotState{YourHand: hand, Round: 9}); got != 5 {
		t.Fatalf("강도 5.0 손패의 비드 = %d, want 5", got)
	}
	if got := kgBotBid(kgBotState{YourHand: hand, Round: 3}); got != 3 {
		t.Fatalf("라운드 상한으로 잘려야 한다: %d", got)
	}
	weak := []KGCard{kgEscape, kgNum(KGSuitGreen, 2), kgNum(KGSuitYellow, 5)}
	if got := kgBotBid(kgBotState{YourHand: weak, Round: 3}); got != 0 {
		t.Fatalf("약한 손패의 비드 = %d, want 0", got)
	}
}

// TestKGBotPickPlay 목표를 못 채웠으면 최소한으로 이기고, 채웠으면 최약 카드
func TestKGBotPickPlay(t *testing.T) {
	b := newKGBrain()

	// 목표 미달 + 후행: 초록 9 를 이기는 가장 약한 카드(초록 10)를 고른다
	s := kgBotState{
		YourSeat: 1, Phase: KGPhasePlaying, Round: 3, CurrentSeat: 1,
		LeadSuit: KGSuitGreen,
		Trick:    []KGTrickPlay{{Seat: 0, Card: kgNum(KGSuitGreen, 9)}},
		YourHand: []KGCard{kgNum(KGSuitGreen, 2), kgNum(KGSuitGreen, 10), kgNum(KGSuitGreen, 13)},
		Players:  []kgBotPlayerView{{Seat: 0}, {Seat: 1, Bid: 2, Tricks: 0}},
	}
	if got := b.pickPlay(s); got != 1 {
		t.Fatalf("최소한으로 이기기 = 인덱스 %d, want 1", got)
	}

	// 목표를 이미 채웠으면 가장 약한 카드 (탈출 우선)
	s.Players[1].Tricks = 2
	s.YourHand = []KGCard{kgNum(KGSuitGreen, 13), kgEscape, kgNum(KGSuitGreen, 2)}
	if got := b.pickPlay(s); got != 1 {
		t.Fatalf("최약 카드 = 인덱스 %d, want 1 (탈출)", got)
	}

	// 이길 수 없으면 가장 약한 카드를 버린다
	s.Players[1].Tricks = 0
	s.Trick = []KGTrickPlay{{Seat: 0, Card: kgSkullKing}}
	s.LeadSuit = KGSuitNone
	s.YourHand = []KGCard{kgNum(KGSuitGreen, 13), kgNum(KGSuitGreen, 2)}
	if got := b.pickPlay(s); got != 1 {
		t.Fatalf("포기 시 최약 카드 = 인덱스 %d, want 1", got)
	}

	// 리드일 때 목표 미달이면 가장 강한 카드
	s.Trick = []KGTrickPlay{}
	s.YourHand = []KGCard{kgNum(KGSuitGreen, 2), kgPirate, kgEscape}
	if got := b.pickPlay(s); got != 1 {
		t.Fatalf("리드 최강 카드 = 인덱스 %d, want 1 (해적)", got)
	}
}
