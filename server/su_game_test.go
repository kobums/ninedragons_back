package server

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

// ==================== 스타트업스 순수 규칙 테스트 ====================
//
// 이 게임의 전부는 (1) 대주주 판정(동수면 대주주 없음), (2) 대주주는 덱에서
// 못 뽑고 안티를 얹는 규칙, (3) 안티 누적과 지급, (4) 최종 정산이다.
// 넷 다 표 기반으로 촘촘히 고정한다.

// suFixture 결정적 시드로 시작된 n인 게임 (규칙 테스트는 이후 판을 직접 세운다)
func suFixture(t *testing.T, n int) *SUGame {
	t.Helper()
	g := NewSUGame("su-test")
	for i := 0; i < n; i++ {
		if _, err := g.AddPlayer(fmt.Sprintf("P%d", i)); err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
	}
	if err := g.Start(rand.New(rand.NewSource(20260828))); err != nil {
		t.Fatalf("Start: %v", err)
	}
	g.DrainEvents()
	return g
}

// suSetBoard 판을 손으로 세운다 (덱·시장·차례 좌석 고정)
func suSetBoard(g *SUGame, seat int, deck []SUCompany, market []SUMarketCard) {
	g.Deck = append([]SUCompany{}, deck...)
	g.Market = append([]SUMarketCard{}, market...)
	g.CurrentSeat = seat
	g.StartSeat = seat
	g.Phase = SUPhaseTake
	g.DeckAnte = 0
	for _, p := range g.Players {
		p.Money = SUStartMoney
		p.FaceUp = suNewFaceUp()
		p.Hand = []SUCompany{}
	}
	g.DrainEvents()
}

// suTotalMoney 판 안의 돈 총합 — 전원 돈 + 시장 안티 + 덱 안티.
// 정산 전까지는 항상 10원 × 인원 이어야 한다 (안티는 이동만 한다).
func suTotalMoney(g *SUGame) int {
	total := g.DeckAnte
	for _, p := range g.Players {
		total += p.Money
	}
	for _, c := range g.Market {
		total += c.Ante
	}
	return total
}

// TestSUDeckComposition 덱 33장 — 회사마다 장수가 다르다 (표 고정)
func TestSUDeckComposition(t *testing.T) {
	want := map[SUCompany]int{
		SUGeeks: 3, SUBowwow: 4, SUOcean: 5,
		SUSuperfusion: 6, SUGaga: 7, SUDove: 8,
	}
	deck := suBuildDeck()
	if len(deck) != 33 || suDeckSize() != 33 {
		t.Fatalf("덱 장수 = %d / %d, want 33", len(deck), suDeckSize())
	}
	got := map[SUCompany]int{}
	for _, c := range deck {
		got[c]++
	}
	for company, n := range want {
		if got[company] != n {
			t.Fatalf("%s(%s) 장수 = %d, want %d", suName(company), company, got[company], n)
		}
		// 회사 가치 = 총 장수
		if suSize(company) != n {
			t.Fatalf("%s 가치 = %d, want %d", suName(company), suSize(company), n)
		}
	}
	if len(got) != 6 {
		t.Fatalf("회사 종류 = %d, want 6", len(got))
	}
	for _, def := range suCompanyDefs {
		if def.Name == "" || def.Color == "" || def.Emoji == "" {
			t.Fatalf("회사 표기 누락: %+v", def)
		}
		if !hasHangul(def.Name) {
			t.Fatalf("회사 이름이 한글이 아니다: %q", def.Name)
		}
	}
}

// TestSUSetup 시작 배치 — 각자 주식 카드 1장·돈 10원, 3장은 게임에서 제외
func TestSUSetup(t *testing.T) {
	for _, n := range []int{SUMinPlayers, 4, SUMaxPlayers} {
		g := suFixture(t, n)
		if g.Phase != SUPhaseTake || !g.Ready {
			t.Fatalf("%d인 시작 phase = %s", n, g.Phase)
		}
		if len(g.Removed) != SURemovedCards {
			t.Fatalf("%d인 제외 장수 = %d, want %d", n, len(g.Removed), SURemovedCards)
		}
		if len(g.Market) != 0 || g.DeckAnte != 0 {
			t.Fatalf("%d인 시작 시장/덱 안티 = %v / %d", n, g.Market, g.DeckAnte)
		}
		wantDeck := 33 - SURemovedCards - n
		if len(g.Deck) != wantDeck {
			t.Fatalf("%d인 덱 잔량 = %d, want %d", n, len(g.Deck), wantDeck)
		}
		for _, p := range g.Players {
			if p.Money != SUStartMoney {
				t.Fatalf("%d인 seat%d 돈 = %d", n, p.Seat, p.Money)
			}
			if len(p.Hand) != SUStartHand {
				t.Fatalf("%d인 seat%d 손패 = %v", n, p.Seat, p.Hand)
			}
			if len(p.FaceUp) != 6 {
				t.Fatalf("%d인 seat%d 앞면 더미 키 = %d", n, p.Seat, len(p.FaceUp))
			}
			for _, cnt := range p.FaceUp {
				if cnt != 0 {
					t.Fatalf("%d인 seat%d 시작 앞면 = %v", n, p.Seat, p.FaceUp)
				}
			}
		}
		if suTotalMoney(g) != SUStartMoney*n {
			t.Fatalf("%d인 시작 돈 총합 = %d", n, suTotalMoney(g))
		}
		// 시작 전 회사 현황판에는 대주주가 없다 (전원 0장 — 동수)
		for _, ci := range g.CompanyBoard() {
			if ci.MajoritySeat != -1 {
				t.Fatalf("시작 %s 대주주 = %d, want -1", ci.Name, ci.MajoritySeat)
			}
		}
	}
}

// TestSUMajorityTable 대주주 판정 — 가장 많이 가진 한 명, 동수면 대주주 없음
func TestSUMajorityTable(t *testing.T) {
	cases := []struct {
		name   string
		counts []int
		want   int
	}{
		{"전원 0장이면 대주주 없음", []int{0, 0, 0}, -1},
		{"단독 1장이면 대주주", []int{0, 1, 0}, 1},
		{"최다 단독", []int{3, 1, 2}, 0},
		{"최다 동수면 대주주 없음", []int{2, 2, 0}, -1},
		{"최다 3인 동수면 대주주 없음", []int{2, 2, 2}, -1},
		{"동수는 최다에서만 따진다", []int{4, 2, 2}, 0},
		{"꼴찌 동수는 무관", []int{1, 5, 1}, 1},
		{"2인 동점", []int{3, 3}, -1},
		{"7인 단독 최다", []int{1, 0, 2, 1, 0, 3, 1}, 5},
		{"7인 최다 동수", []int{1, 0, 3, 1, 0, 3, 1}, -1},
	}
	for _, tc := range cases {
		if got := suMajoritySeat(tc.counts); got != tc.want {
			t.Fatalf("%s: suMajoritySeat(%v) = %d, want %d", tc.name, tc.counts, got, tc.want)
		}
	}

	// 진행 중 대주주는 "앞면" 카드만 센다 — 손패는 정산 때만 공개해 함께 센다
	g := suFixture(t, 3)
	suSetBoard(g, 0, []SUCompany{SUOcean}, nil)
	g.Players[0].FaceUp[SUGaga] = 1
	g.Players[1].Hand = []SUCompany{SUGaga, SUGaga}
	if got := g.MajoritySeat(SUGaga); got != 0 {
		t.Fatalf("앞면 1장 vs 손패 2장 대주주 = %d, want 0 (손패는 안 센다)", got)
	}
}

// TestSUDeckBlockedByMajority 대주주는 그 회사 카드를 덱에서 못 가져온다 —
// 자기 돈 1원을 덱 위에 얹고 그 카드를 덱 맨 아래로 보낸 뒤 다시 뽑는다.
// 돈이 없으면 덱에서 못 가져오고 시장에서 가져와야 한다.
func TestSUDeckBlockedByMajority(t *testing.T) {
	// ---- 벽 2장을 넘어 세 번째 카드를 가져온다 ----
	g := suFixture(t, 3)
	suSetBoard(g, 0, []SUCompany{SUGaga, SUGaga, SUOcean, SUDove}, nil)
	g.Players[0].FaceUp[SUGaga] = 2 // seat0 이 가가 대주주
	g.Players[0].Hand = []SUCompany{SUBowwow}
	before := suTotalMoney(g)

	if err := g.Take(0, SUTakeDeck); err != nil {
		t.Fatalf("덱 가져오기 실패: %v", err)
	}
	if g.DeckAnte != 2 {
		t.Fatalf("덱 안티 = %d, want 2 (막힌 2장)", g.DeckAnte)
	}
	if g.Players[0].Money != SUStartMoney-2 {
		t.Fatalf("안티 지불 후 돈 = %d, want %d", g.Players[0].Money, SUStartMoney-2)
	}
	// 뽑은 카드는 비공개 손패로 간다 (앞면 더미가 아니다)
	if len(g.Players[0].Hand) != 2 || g.Players[0].Hand[1] != SUOcean {
		t.Fatalf("손패 = %v, want [bowwow ocean]", g.Players[0].Hand)
	}
	if g.Players[0].FaceUp[SUOcean] != 0 {
		t.Fatalf("덱에서 뽑은 카드가 앞면 더미로 갔다: %v", g.Players[0].FaceUp)
	}
	// 막힌 가가 2장은 덱 맨 아래로 (덱은 성공한 1장만큼만 줄었다)
	if len(g.Deck) != 3 {
		t.Fatalf("덱 잔량 = %d, want 3", len(g.Deck))
	}
	if g.Deck[0] != SUDove || g.Deck[1] != SUGaga || g.Deck[2] != SUGaga {
		t.Fatalf("덱 = %v, want [dove gaga gaga]", g.Deck)
	}
	if suTotalMoney(g) != before {
		t.Fatalf("안티는 이동만 해야 한다: 총합 %d → %d", before, suTotalMoney(g))
	}
	// 이벤트에 막힌 카드의 회사명이 새어 나가면 안 된다 (덱 정보 유출)
	for _, ev := range g.DrainEvents() {
		if strings.Contains(ev.Message, "가가") {
			t.Fatalf("이벤트에 덱 카드 회사명 유출: %q", ev.Message)
		}
	}

	// ---- 돈이 없으면 덱에서 못 가져온다 ----
	g2 := suFixture(t, 3)
	suSetBoard(g2, 0, []SUCompany{SUGaga, SUOcean},
		[]SUMarketCard{{Company: SUDove, Ante: 0}})
	g2.Players[0].FaceUp[SUGaga] = 1
	g2.Players[0].Money = 0
	g2.Players[0].Hand = []SUCompany{SUBowwow}
	if err := g2.Take(0, SUTakeDeck); err == nil {
		t.Fatal("돈 0원인데 덱 가져오기가 통과했다")
	} else if !hasHangul(err.Error()) {
		t.Fatalf("오류 문구가 한글이 아니다: %q", err.Error())
	}
	if g2.Phase != SUPhaseTake || g2.DeckAnte != 0 || len(g2.Deck) != 2 {
		t.Fatalf("거부된 요청이 판을 바꿨다: phase=%s ante=%d deck=%d",
			g2.Phase, g2.DeckAnte, len(g2.Deck))
	}
	// 시장에서는 가져올 수 있다
	if err := g2.Take(0, SUTakeMarketPrefix+"0"); err != nil {
		t.Fatalf("시장 가져오기 실패: %v", err)
	}

	// ---- 덱이 통째로 내가 대주주인 회사면 덱에서 못 가져온다 ----
	g3 := suFixture(t, 3)
	suSetBoard(g3, 0, []SUCompany{SUGaga, SUGaga},
		[]SUMarketCard{{Company: SUDove, Ante: 0}})
	g3.Players[0].FaceUp[SUGaga] = 1
	g3.Players[0].Hand = []SUCompany{SUBowwow}
	if err := g3.Take(0, SUTakeDeck); err == nil {
		t.Fatal("덱 전량이 대주주 회사인데 통과했다")
	}
	if g3.Players[0].Money != SUStartMoney {
		t.Fatalf("거부된 요청이 돈을 깎았다: %d", g3.Players[0].Money)
	}

	// ---- 대주주가 아니면 그냥 가져온다 (동수라 대주주 없음) ----
	g4 := suFixture(t, 3)
	suSetBoard(g4, 0, []SUCompany{SUGaga, SUOcean}, nil)
	g4.Players[0].FaceUp[SUGaga] = 2
	g4.Players[1].FaceUp[SUGaga] = 2 // 동수 → 대주주 없음 → 막히지 않는다
	g4.Players[0].Hand = []SUCompany{SUBowwow}
	if err := g4.Take(0, SUTakeDeck); err != nil {
		t.Fatalf("동수인데 막혔다: %v", err)
	}
	if g4.DeckAnte != 0 || g4.Players[0].Money != SUStartMoney {
		t.Fatalf("동수인데 안티를 냈다: ante=%d money=%d", g4.DeckAnte, g4.Players[0].Money)
	}
}

// TestSUAnteAccrualAndPayout 안티 누적과 지급
//   - 덱에서 가져가면 시장에 남겨 둔 카드마다 안티 1원씩 얹는다
//   - 시장 카드를 가져가면 그 위의 안티를 전부 받는다
//   - 덱 위 안티는 다음에 덱에서 가져간 사람이 받는다 (자기가 얹은 건 남는다)
//   - 돈이 모자라면 앞쪽부터 낼 수 있는 만큼만 얹는다
func TestSUAnteAccrualAndPayout(t *testing.T) {
	// ---- 덱에서 가져가면 시장 카드마다 안티 1원 ----
	g := suFixture(t, 3)
	suSetBoard(g, 0, []SUCompany{SUOcean, SUDove}, []SUMarketCard{
		{Company: SUGeeks, Ante: 0},
		{Company: SUGaga, Ante: 2},
		{Company: SUDove, Ante: 0},
	})
	g.Players[0].Hand = []SUCompany{SUBowwow}
	before := suTotalMoney(g)

	if err := g.Take(0, SUTakeDeck); err != nil {
		t.Fatalf("덱 가져오기 실패: %v", err)
	}
	wantAntes := []int{1, 3, 1}
	for i, want := range wantAntes {
		if g.Market[i].Ante != want {
			t.Fatalf("시장[%d] 안티 = %d, want %d (전체 %v)", i, g.Market[i].Ante, want, g.Market)
		}
	}
	if g.Players[0].Money != SUStartMoney-3 {
		t.Fatalf("안티 3원 지불 후 돈 = %d", g.Players[0].Money)
	}
	if suTotalMoney(g) != before {
		t.Fatalf("돈 총합이 바뀌었다: %d → %d", before, suTotalMoney(g))
	}

	// ---- 시장 카드를 가져가면 그 위의 안티를 전부 받는다 ----
	if err := g.Play(0, 0); err != nil { // 손패 1장을 시장에 내려놓고 차례 종료
		t.Fatalf("내려놓기 실패: %v", err)
	}
	if g.CurrentSeat != 1 || g.Phase != SUPhaseTake {
		t.Fatalf("차례 이양 실패: seat=%d phase=%s", g.CurrentSeat, g.Phase)
	}
	g.Players[1].Hand = []SUCompany{SUGeeks}
	moneyBefore := g.Players[1].Money
	if err := g.Take(1, SUTakeMarketPrefix+"1"); err != nil { // 가가(안티 3원)
		t.Fatalf("시장 가져오기 실패: %v", err)
	}
	if g.Players[1].Money != moneyBefore+3 {
		t.Fatalf("안티 수령 후 돈 = %d, want %d", g.Players[1].Money, moneyBefore+3)
	}
	// 시장에서 가져온 카드는 내 앞에 앞면으로 쌓인다 (손패가 아니다)
	if g.Players[1].FaceUp[SUGaga] != 1 {
		t.Fatalf("앞면 더미 = %v", g.Players[1].FaceUp)
	}
	if len(g.Players[1].Hand) != 1 {
		t.Fatalf("시장 카드가 손패로 갔다: %v", g.Players[1].Hand)
	}
	if suTotalMoney(g) != before {
		t.Fatalf("돈 총합이 바뀌었다: %d → %d", before, suTotalMoney(g))
	}

	// ---- 덱 위 안티는 다음 사람이 받는다 (자기가 얹은 건 덱에 남는다) ----
	g2 := suFixture(t, 3)
	suSetBoard(g2, 0, []SUCompany{SUGaga, SUOcean, SUDove}, nil)
	g2.Players[0].FaceUp[SUGaga] = 1 // seat0 이 가가 대주주 → 1장 막힘
	g2.Players[0].Hand = []SUCompany{SUBowwow}
	if err := g2.Take(0, SUTakeDeck); err != nil {
		t.Fatalf("덱 가져오기 실패: %v", err)
	}
	if g2.DeckAnte != 1 || g2.Players[0].Money != SUStartMoney-1 {
		t.Fatalf("자기가 얹은 안티를 되가져갔다: ante=%d money=%d",
			g2.DeckAnte, g2.Players[0].Money)
	}
	g2.Play(0, 0)
	g2.Players[1].Hand = []SUCompany{SUGeeks}
	money1 := g2.Players[1].Money
	if err := g2.Take(1, SUTakeDeck); err != nil {
		t.Fatalf("다음 사람 덱 가져오기 실패: %v", err)
	}
	// 덱 안티 1원 수령 − 시장 1장에 안티 1원 = ±0
	if g2.DeckAnte != 0 {
		t.Fatalf("덱 안티가 지급되지 않았다: %d", g2.DeckAnte)
	}
	if g2.Players[1].Money != money1 {
		t.Fatalf("덱 안티 1원 수령·시장 안티 1원 지불이면 돈이 그대로여야 한다: %d → %d",
			money1, g2.Players[1].Money)
	}
	if g2.Market[0].Ante != 1 {
		t.Fatalf("시장 안티 = %d, want 1", g2.Market[0].Ante)
	}

	// ---- 돈이 모자라면 앞쪽부터 낼 수 있는 만큼만 얹는다 ----
	g3 := suFixture(t, 3)
	suSetBoard(g3, 0, []SUCompany{SUOcean}, []SUMarketCard{
		{Company: SUGeeks, Ante: 0},
		{Company: SUGaga, Ante: 0},
		{Company: SUDove, Ante: 0},
	})
	g3.Players[0].Money = 2
	g3.Players[0].Hand = []SUCompany{SUBowwow}
	if err := g3.Take(0, SUTakeDeck); err != nil {
		t.Fatalf("덱 가져오기 실패: %v", err)
	}
	if g3.Players[0].Money != 0 {
		t.Fatalf("돈 = %d, want 0", g3.Players[0].Money)
	}
	got := []int{g3.Market[0].Ante, g3.Market[1].Ante, g3.Market[2].Ante}
	if got[0] != 1 || got[1] != 1 || got[2] != 0 {
		t.Fatalf("부분 안티 = %v, want [1 1 0]", got)
	}
}

// TestSUTurnFlow 앞면/뒷면 흐름 — 덱에서 뽑은 카드는 비공개 손패로, 시장에서
// 가져온 카드는 내 앞 앞면 더미로, 내려놓은 카드는 시장에 남는다
func TestSUTurnFlow(t *testing.T) {
	g := suFixture(t, 3)
	suSetBoard(g, 0, []SUCompany{SUOcean, SUDove}, []SUMarketCard{{Company: SUGaga, Ante: 0}})
	g.Players[0].Hand = []SUCompany{SUBowwow}

	// ① 덱 → 손패 (비공개)
	if err := g.Take(0, SUTakeDeck); err != nil {
		t.Fatalf("덱 가져오기 실패: %v", err)
	}
	if len(g.Players[0].Hand) != 2 || g.Players[0].Hand[1] != SUOcean {
		t.Fatalf("손패 = %v", g.Players[0].Hand)
	}
	if g.Phase != SUPhasePlay {
		t.Fatalf("phase = %s, want play", g.Phase)
	}
	// 가져오기 단계가 아니면 또 가져올 수 없다
	if err := g.Take(0, SUTakeDeck); err == nil {
		t.Fatal("한 차례에 두 번 가져왔다")
	}

	// ② 손패 → 시장 (앞면). 내 앞 앞면 더미로 가지 않는다
	if err := g.Play(0, 0); err != nil { // 바우와우
		t.Fatalf("내려놓기 실패: %v", err)
	}
	if len(g.Market) != 2 || g.Market[1].Company != SUBowwow || g.Market[1].Ante != 0 {
		t.Fatalf("시장 = %+v", g.Market)
	}
	if g.Players[0].FaceUp[SUBowwow] != 0 {
		t.Fatalf("내려놓은 카드가 앞면 더미로 갔다: %v", g.Players[0].FaceUp)
	}
	if len(g.Players[0].Hand) != 1 || g.Players[0].Hand[0] != SUOcean {
		t.Fatalf("남은 손패 = %v", g.Players[0].Hand)
	}
	if g.Turns != 1 || g.CurrentSeat != 1 {
		t.Fatalf("차례 = %d seat=%d", g.Turns, g.CurrentSeat)
	}

	// ① 시장 → 앞면 더미 (공개)
	g.Players[1].Hand = []SUCompany{SUGeeks}
	if err := g.Take(1, SUTakeMarketPrefix+"0"); err != nil {
		t.Fatalf("시장 가져오기 실패: %v", err)
	}
	if g.Players[1].FaceUp[SUGaga] != 1 {
		t.Fatalf("앞면 더미 = %v", g.Players[1].FaceUp)
	}
	if len(g.Players[1].Hand) != 1 || g.Players[1].Hand[0] != SUGeeks {
		t.Fatalf("시장 카드가 손패로 갔다: %v", g.Players[1].Hand)
	}
	if g.MajoritySeat(SUGaga) != 1 {
		t.Fatalf("가가 대주주 = %d, want 1", g.MajoritySeat(SUGaga))
	}
}

// TestSUEmptyHandSkipsPlay 시장에서 가져온 카드는 앞면 더미로 가므로 손패가
// 늘지 않는다. 손패가 빈 채로 차례를 맞으면 ②를 건너뛰고 차례를 넘긴다.
// 이 부등식("시장에서 가져온 차례 ≤ 덱에서 가져온 차례")이 유한 종료의 근거다.
func TestSUEmptyHandSkipsPlay(t *testing.T) {
	g := suFixture(t, 3)
	suSetBoard(g, 0, []SUCompany{SUOcean}, []SUMarketCard{{Company: SUGaga, Ante: 5}})
	g.Players[0].Hand = []SUCompany{}
	money := g.Players[0].Money

	if err := g.Take(0, SUTakeMarketPrefix+"0"); err != nil {
		t.Fatalf("손패가 빈 상태의 시장 가져오기 실패: %v", err)
	}
	if g.Phase == SUPhasePlay {
		t.Fatal("낼 카드가 없는데 내려놓기 단계로 갔다")
	}
	if g.CurrentSeat != 1 {
		t.Fatalf("차례가 넘어가지 않았다: seat=%d phase=%s", g.CurrentSeat, g.Phase)
	}
	if g.Players[0].FaceUp[SUGaga] != 1 || g.Players[0].Money != money+5 {
		t.Fatalf("앞면 더미/안티 수령 = %v / %d원", g.Players[0].FaceUp, g.Players[0].Money)
	}
	if len(g.Market) != 0 {
		t.Fatalf("시장 = %+v (가져간 만큼 줄어야 한다)", g.Market)
	}

	// 손패가 비어도 덱은 쓸 수 있고, 그때는 ②까지 정상 진행한다
	g2 := suFixture(t, 3)
	suSetBoard(g2, 0, []SUCompany{SUOcean}, nil)
	g2.Players[0].Hand = []SUCompany{}
	if err := g2.Take(0, SUTakeDeck); err != nil {
		t.Fatalf("손패가 빈 상태의 덱 가져오기 실패: %v", err)
	}
	if g2.Phase != SUPhasePlay || len(g2.Players[0].Hand) != 1 {
		t.Fatalf("phase=%s hand=%v", g2.Phase, g2.Players[0].Hand)
	}

	// 시장도 덱도 비면 아무것도 못 가져온다 (한글 오류)
	g3 := suFixture(t, 3)
	suSetBoard(g3, 0, nil, nil)
	g3.Players[0].Hand = []SUCompany{SUGeeks}
	for _, from := range []string{SUTakeDeck, SUTakeMarketPrefix + "0"} {
		err := g3.Take(0, from)
		if err == nil {
			t.Fatalf("빈 덱·빈 시장에서 %q 가 통과했다", from)
		}
		if !hasHangul(err.Error()) {
			t.Fatalf("오류 문구가 한글이 아니다: %q", err.Error())
		}
	}
}

// TestSUEndsWhenDeckEmpty 덱이 떨어지면 그 라운드를 마치고 정산한다
func TestSUEndsWhenDeckEmpty(t *testing.T) {
	g := suFixture(t, 3)
	suSetBoard(g, 0, []SUCompany{SUOcean}, []SUMarketCard{
		{Company: SUGaga, Ante: 0}, {Company: SUDove, Ante: 0},
	})
	for _, p := range g.Players {
		p.Hand = []SUCompany{SUGeeks}
	}

	// seat0 이 마지막 덱 카드를 가져간다 → 덱 0장
	if err := g.Take(0, SUTakeDeck); err != nil {
		t.Fatalf("덱 가져오기 실패: %v", err)
	}
	if len(g.Deck) != 0 {
		t.Fatalf("덱 잔량 = %d", len(g.Deck))
	}
	g.Play(0, 0)
	if g.Phase == SUPhaseGameOver {
		t.Fatal("덱이 마르자마자 끝났다 — 그 라운드는 마쳐야 한다")
	}
	// seat1, seat2 가 라운드를 마치면 정산
	for _, seat := range []int{1, 2} {
		if g.CurrentSeat != seat {
			t.Fatalf("차례 = %d, want %d", g.CurrentSeat, seat)
		}
		if err := g.Take(seat, SUTakeMarketPrefix+"0"); err != nil {
			t.Fatalf("seat%d 시장 가져오기 실패: %v", seat, err)
		}
		if g.Phase == SUPhasePlay {
			g.Play(seat, 0)
		}
	}
	if g.Phase != SUPhaseGameOver || g.Result == nil {
		t.Fatalf("라운드를 마쳤는데 정산되지 않았다: phase=%s", g.Phase)
	}
	if len(g.Result.Rows) != 3 || len(g.Result.WinnerSeats) == 0 {
		t.Fatalf("정산 결과 = %+v", g.Result)
	}
	if !hasHangul(g.Result.Message) {
		t.Fatalf("정산 문구가 한글이 아니다: %q", g.Result.Message)
	}
}

// TestSUSettlementTable 최종 정산 — 손패를 전부 공개해 회사별 보유 수를 세고,
// 회사마다 대주주 1명(동수면 없음)이 다른 사람들의 그 회사 카드 1장당
// 회사 가치만큼 받는다.
//
//	회사(가치)      seat0  seat1  seat2   대주주   남의 장수   지급
//	긱스(3)           2      1      0     seat0        1        +3
//	바우와우(4)        1      1      0     없음(동수)    -         0
//	오션(5)           1      0      3     seat2        1        +5
//	슈퍼퓨전(6)        0      0      0     없음          -         0
//	가가(7)           1      2      1     seat1        2       +14
//	더브(8)           1      0      0     seat0        0        +0
//
//	시작 돈: seat0 5원 · seat1 7원 · seat2 9원
//	최종:    seat0 8원 · seat1 21원 · seat2 14원 → seat1 승
func TestSUSettlementTable(t *testing.T) {
	g := suFixture(t, 3)
	suSetBoard(g, 0, nil, nil)

	set := func(seat int, faceUp map[SUCompany]int, hand []SUCompany) {
		for c, n := range faceUp {
			g.Players[seat].FaceUp[c] = n
		}
		g.Players[seat].Hand = append([]SUCompany{}, hand...)
	}
	// 손패도 정산에 포함되는지 보려고 일부를 손패에 둔다
	set(0, map[SUCompany]int{SUGeeks: 2, SUBowwow: 1, SUOcean: 1, SUGaga: 1}, []SUCompany{SUDove})
	set(1, map[SUCompany]int{SUGeeks: 1, SUBowwow: 1, SUGaga: 1}, []SUCompany{SUGaga})
	set(2, map[SUCompany]int{SUOcean: 2, SUGaga: 1}, []SUCompany{SUOcean})
	g.Players[0].Money, g.Players[1].Money, g.Players[2].Money = 5, 7, 9

	g.settle("검증")

	wantMoney := []int{8, 21, 14}
	for i, want := range wantMoney {
		if g.Players[i].Money != want {
			t.Fatalf("seat%d 최종 돈 = %d, want %d", i, g.Players[i].Money, want)
		}
	}
	// 손패는 정산 때 공개돼 앞면 더미에 합쳐진다
	for _, p := range g.Players {
		if len(p.Hand) != 0 {
			t.Fatalf("seat%d 손패가 남았다: %v", p.Seat, p.Hand)
		}
	}
	if g.Players[0].FaceUp[SUDove] != 1 || g.Players[1].FaceUp[SUGaga] != 2 ||
		g.Players[2].FaceUp[SUOcean] != 3 {
		t.Fatalf("손패가 앞면 더미에 합쳐지지 않았다: %v / %v / %v",
			g.Players[0].FaceUp, g.Players[1].FaceUp, g.Players[2].FaceUp)
	}
	// 회사 현황판의 대주주 (동수인 바우와우·아무도 없는 슈퍼퓨전은 -1)
	wantMajority := map[SUCompany]int{
		SUGeeks: 0, SUBowwow: -1, SUOcean: 2, SUSuperfusion: -1, SUGaga: 1, SUDove: 0,
	}
	for _, ci := range g.CompanyBoard() {
		if ci.MajoritySeat != wantMajority[ci.ID] {
			t.Fatalf("%s 대주주 = %d, want %d", ci.Name, ci.MajoritySeat, wantMajority[ci.ID])
		}
	}
	if g.Result == nil || len(g.Result.WinnerSeats) != 1 || g.Result.WinnerSeats[0] != 1 {
		t.Fatalf("승자 = %+v", g.Result)
	}
	if g.Result.WinnerNames[0] != "P1" {
		t.Fatalf("승자 이름 = %v", g.Result.WinnerNames)
	}
	// rows 는 돈 내림차순
	if len(g.Result.Rows) != 3 ||
		g.Result.Rows[0].Seat != 1 || g.Result.Rows[1].Seat != 2 || g.Result.Rows[2].Seat != 0 {
		t.Fatalf("정산 표 = %+v", g.Result.Rows)
	}
	for _, row := range g.Result.Rows {
		if !hasHangul(row.Detail) {
			t.Fatalf("정산 설명이 한글이 아니다: %q", row.Detail)
		}
	}
	if !strings.Contains(g.Result.Rows[2].Detail, "긱스") {
		t.Fatalf("seat0 정산 설명에 긱스 대주주가 없다: %q", g.Result.Rows[2].Detail)
	}

	// ---- 대주주가 없으면 아무도 못 받는다 (전 회사 동수) ----
	g2 := suFixture(t, 2+1)
	suSetBoard(g2, 0, nil, nil)
	for _, def := range suCompanyDefs {
		g2.Players[0].FaceUp[def.ID] = 1
		g2.Players[1].FaceUp[def.ID] = 1
		g2.Players[2].FaceUp[def.ID] = 1
	}
	for _, p := range g2.Players {
		p.Money = 10
	}
	g2.settle("동수")
	for _, p := range g2.Players {
		if p.Money != 10 {
			t.Fatalf("동수인데 정산이 일어났다: seat%d %d원", p.Seat, p.Money)
		}
	}
	if len(g2.Result.WinnerSeats) != 3 {
		t.Fatalf("전원 동점인데 공동 승이 아니다: %+v", g2.Result)
	}
	if !strings.Contains(g2.Result.Message, "공동") {
		t.Fatalf("공동 승 문구 = %q", g2.Result.Message)
	}
}

// ==================== 봇 대전 ====================

// suBotFixture 허브 고루틴 없이 결정적으로 돌리는 n인 방
func suBotFixture(t *testing.T, n int, seed int64) (*SUHub, *suRoom, []*SUClient) {
	t.Helper()
	h := NewSUHub()
	h.rng = rand.New(rand.NewSource(seed))
	room := h.lobbyRoomFor("")
	clients := make([]*SUClient, n)
	for i := range clients {
		c := &SUClient{wsClient: newBotWSClient(), Hub: h}
		c.Bot = false // 소켓 없는 사람 취급
		c.Name = fmt.Sprintf("P%d", i)
		seat, err := room.Game.AddPlayer(c.Name)
		if err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
		c.GameID, c.Seat = room.Game.ID, seat
		room.Clients[seat] = c
		h.sessions[c.SessionID] = c
		clients[i] = c
	}
	h.startGame(room)
	h.stopPhaseTimer(room) // 타이머 없이 우리가 직접 차례를 민다
	return h, room, clients
}

// suDrain 봇 채널에 쌓인 메시지를 버린다 (버퍼 포화로 연결이 끊기지 않게)
func suDrain(clients []*SUClient) {
	for _, c := range clients {
		drained := false
		for !drained {
			select {
			case <-c.Send:
			default:
				drained = true
			}
		}
	}
}

// suRunBotGame n봇 한 판을 끝까지 돌리고 (차례 수, 좌석별 최종 돈, 승자 좌석)
// 을 돌려준다. 스냅샷 → 두뇌 → 허브 핸들러 경로가 실제 WS 경로와 같다.
func suRunBotGame(t *testing.T, n int, seed int64) (turns int, moneys []int, winners []int) {
	t.Helper()
	h, room, clients := suBotFixture(t, n, seed)
	game := room.Game
	brains := make([]*suBrain, n)
	for i := range brains {
		brains[i] = &suBrain{rng: rand.New(rand.NewSource(seed*1000 + int64(i)))}
	}

	startMoney := suTotalMoney(game)
	for step := 0; step < SUMaxTurns*4 && game.Phase != SUPhaseGameOver; step++ {
		seat := game.CurrentSeat
		if seat < 0 || seat >= n {
			break
		}
		raw, err := json.Marshal(h.buildSUState(room, seat))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var payload interface{}
		json.Unmarshal(raw, &payload)

		beforeSeq := game.StateSeq
		if reply := brains[seat].decide(SUMessage{Type: SUMsgGameState, Payload: payload}); reply != nil {
			h.handleGameMessage(SUGameMessage{Client: clients[seat], Message: *reply})
		}
		h.stopPhaseTimer(room)
		if game.StateSeq == beforeSeq { // 봇이 막히면 규칙의 자동 진행으로 민다
			if game.Phase == SUPhasePlay {
				game.ForcePlay(h.rng)
			} else {
				game.ForceTake(h.rng)
			}
			game.DrainEvents()
		}
		// 안티는 이동만 한다 — 정산 전까지 판 안의 돈 총합은 그대로다
		if game.Phase != SUPhaseGameOver && suTotalMoney(game) != startMoney {
			t.Fatalf("seed %d: 돈 총합이 %d → %d 로 바뀌었다",
				seed, startMoney, suTotalMoney(game))
		}
		suDrain(clients)
	}
	if game.Phase != SUPhaseGameOver {
		t.Fatalf("seed %d: %d차례에도 끝나지 않았다", seed, game.Turns)
	}

	turns = game.Turns
	for _, p := range game.Players {
		moneys = append(moneys, p.Money)
	}
	if game.Result != nil {
		winners = append([]int{}, game.Result.WinnerSeats...)
	}
	return turns, moneys, winners
}

// TestSUBotQuality 4봇 30판의 평균 소요 차례와 최종 돈 분포를 숫자로 남긴다.
// 덱이 33장뿐이라 반드시 끝나야 하고, 특정 좌석이 매번 이기면 선 이점이
// 굳은 것이다.
func TestSUBotQuality(t *testing.T) {
	const games = 30
	const seats = SUFillBotTarget

	totalTurns, minTurns, maxTurns := 0, 1<<30, 0
	wins := make([]int, seats)
	winMoney := []int{}
	allMoney := []int{}
	capHit := 0

	for i := 0; i < games; i++ {
		turns, moneys, winners := suRunBotGame(t, seats, int64(4200+i))
		totalTurns += turns
		if turns < minTurns {
			minTurns = turns
		}
		if turns > maxTurns {
			maxTurns = turns
		}
		if turns >= SUMaxTurns {
			capHit++
		}
		for _, s := range winners {
			wins[s]++
		}
		best := 0
		for _, m := range moneys {
			allMoney = append(allMoney, m)
			if m > best {
				best = m
			}
		}
		winMoney = append(winMoney, best)
	}

	avgTurns := float64(totalTurns) / games
	sort.Ints(winMoney)
	sort.Ints(allMoney)
	sum := 0
	for _, m := range allMoney {
		sum += m
	}
	t.Logf("봇 품질 %d판(%d인): 평균 소요 차례 %.1f (최소 %d · 최대 %d · 차례 상한 도달 %d판)",
		games, seats, avgTurns, minTurns, maxTurns, capHit)
	t.Logf("  승자 최종 돈 분포: 최소 %d원 · 중앙 %d원 · 최대 %d원",
		winMoney[0], winMoney[len(winMoney)/2], winMoney[len(winMoney)-1])
	t.Logf("  전체 최종 돈 분포: 최소 %d원 · 중앙 %d원 · 최대 %d원 · 평균 %.1f원",
		allMoney[0], allMoney[len(allMoney)/2], allMoney[len(allMoney)-1],
		float64(sum)/float64(len(allMoney)))
	t.Logf("  좌석별 승수: %v (총 %d판)", wins, games)

	if capHit > 0 {
		t.Fatalf("차례 상한(%d)에 걸린 판이 %d개 — 덱 소진으로 끝나야 한다", SUMaxTurns, capHit)
	}
	if avgTurns > 120 {
		t.Fatalf("평균 소요 차례 %.1f — 덱 33장짜리 게임이 너무 길다", avgTurns)
	}
	for seat, w := range wins {
		if w == games {
			t.Fatalf("seat%d 가 %d판을 모두 이겼다 — 선 이점이 굳어 있다", seat, games)
		}
	}
	if allMoney[len(allMoney)-1] <= SUStartMoney {
		t.Fatalf("최고 최종 돈 = %d원 — 정산이 한 번도 일어나지 않았다", allMoney[len(allMoney)-1])
	}
}
