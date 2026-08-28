package server

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

// ==================== 순수 규칙 표 기반 검증 ====================
//
// 비용 계산(보너스 차감·황금 대체)·토큰 10개 상한·귀족 획득·마지막 라운드
// 종료 판정이 이 파일의 과녁이다. 허브·타이머 없이 순수 상태만 다룬다.

// slGame 좌석 n개로 시작한 결정적 게임 (시드 고정)
func slGame(t *testing.T, n int, seed int64) *SLGame {
	t.Helper()
	g := NewSLGame(fmt.Sprintf("test-%d-%d", n, seed))
	for i := 0; i < n; i++ {
		if _, err := g.AddPlayer(fmt.Sprintf("P%d", i)); err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
	}
	if err := g.Start(rand.New(rand.NewSource(seed))); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return g
}

// slCard 비용 표기를 짧게 쓰기 위한 헬퍼 (d·s·e·r·o 순서)
func slCost(d, s, e, r, o int) SLGemSet {
	return SLGemSet{Diamond: d, Sapphire: s, Emerald: e, Ruby: r, Onyx: o}
}

func slTokens(d, s, e, r, o, gold int) SLTokenSet {
	return SLTokenSet{Diamond: d, Sapphire: s, Emerald: e, Ruby: r, Onyx: o, Gold: gold}
}

// TestSLDeckComposition 개발 카드 90장(40·30·20)과 귀족 타일 10장의 구성.
// id 는 1부터 겹치지 않게 붙고, 단계마다 명성 점수 범위가 다르다.
func TestSLDeckComposition(t *testing.T) {
	nextID := 0
	ids := map[int]bool{}
	tiers := []struct {
		tier      int
		want      int
		minPoints int
		maxPoints int
	}{
		{1, SLDeckTier1, 0, 1},
		{2, SLDeckTier2, 1, 3},
		{3, SLDeckTier3, 3, 5},
	}
	perGem := map[int]map[SLGem]int{1: {}, 2: {}, 3: {}}
	for _, tc := range tiers {
		deck := slBuildDeck(tc.tier, &nextID)
		if len(deck) != tc.want {
			t.Fatalf("%d단계 = %d장, want %d", tc.tier, len(deck), tc.want)
		}
		for _, c := range deck {
			if c.ID <= 0 || ids[c.ID] {
				t.Fatalf("카드 id 중복/무효: %+v", c)
			}
			ids[c.ID] = true
			if c.Tier != tc.tier {
				t.Fatalf("단계 불일치: %+v", c)
			}
			if c.Points < tc.minPoints || c.Points > tc.maxPoints {
				t.Fatalf("%d단계 명성 점수 %d (허용 %d~%d): %+v",
					tc.tier, c.Points, tc.minPoints, tc.maxPoints, c)
			}
			if !slGemValid(c.Gem) {
				t.Fatalf("보너스 색이 보석 5색이 아니다: %+v", c)
			}
			if c.Cost.total() <= 0 {
				t.Fatalf("비용이 0인 카드: %+v", c)
			}
			perGem[c.tierIdx()][c.Gem]++
		}
	}
	if nextID != SLDeckTier1+SLDeckTier2+SLDeckTier3 {
		t.Fatalf("총 장수 = %d, want 90", nextID)
	}
	// 색 대칭 — 단계마다 다섯 색이 같은 장수여야 한다
	for tier, counts := range perGem {
		want := len(counts)
		if want != len(slGems) {
			t.Fatalf("%d단계 보너스 색 종류 = %d", tier, want)
		}
		first := -1
		for gem, n := range counts {
			if first < 0 {
				first = n
			} else if n != first {
				t.Fatalf("%d단계 색 분포 불균형: %s=%d vs %d", tier, gem, n, first)
			}
		}
	}

	nobles := slAllNobles()
	if len(nobles) != 10 {
		t.Fatalf("귀족 타일 = %d장, want 10", len(nobles))
	}
	for _, nb := range nobles {
		if nb.Points != 3 {
			t.Fatalf("귀족 타일 명성 점수 = %d, want 3: %+v", nb.Points, nb)
		}
		if total := nb.Cost.total(); total != 8 && total != 9 {
			t.Fatalf("귀족 요구 합계 = %d (4+4 또는 3+3+3): %+v", total, nb)
		}
	}
}

// tierIdx 표 검증용 (1~3 그대로)
func (c SLCard) tierIdx() int { return c.Tier }

// TestSLPayment 비용 계산 — 보너스 차감과 황금 대체를 표로 촘촘히 본다.
func TestSLPayment(t *testing.T) {
	cases := []struct {
		name      string
		cost      SLGemSet
		bonus     SLGemSet
		tokens    SLTokenSet
		wantSpend SLGemSet
		wantGold  int
		wantOK    bool
	}{
		{
			name: "정가 그대로 낸다",
			cost: slCost(1, 1, 1, 1, 0), bonus: slCost(0, 0, 0, 0, 0),
			tokens:    slTokens(1, 1, 1, 1, 0, 0),
			wantSpend: slCost(1, 1, 1, 1, 0), wantGold: 0, wantOK: true,
		},
		{
			name: "보너스가 비용을 깎는다 — 토큰은 그만큼 덜 나간다",
			cost: slCost(3, 0, 0, 0, 0), bonus: slCost(2, 0, 0, 0, 0),
			tokens:    slTokens(1, 0, 0, 0, 0, 0),
			wantSpend: slCost(1, 0, 0, 0, 0), wantGold: 0, wantOK: true,
		},
		{
			name: "보너스가 비용을 넘으면 공짜다",
			cost: slCost(2, 2, 0, 0, 0), bonus: slCost(5, 5, 0, 0, 0),
			tokens:    slTokens(0, 0, 0, 0, 0, 0),
			wantSpend: slCost(0, 0, 0, 0, 0), wantGold: 0, wantOK: true,
		},
		{
			name: "모자란 한 자리를 황금이 메운다",
			cost: slCost(0, 0, 3, 0, 0), bonus: slCost(0, 0, 0, 0, 0),
			tokens:    slTokens(0, 0, 2, 0, 0, 1),
			wantSpend: slCost(0, 0, 2, 0, 0), wantGold: 1, wantOK: true,
		},
		{
			name: "여러 색이 모자라도 황금이 충분하면 산다",
			cost: slCost(2, 2, 2, 0, 0), bonus: slCost(1, 0, 0, 0, 0),
			tokens:    slTokens(0, 1, 0, 0, 0, 4),
			wantSpend: slCost(0, 1, 0, 0, 0), wantGold: 4, wantOK: true,
		},
		{
			name: "황금이 한 개 모자라면 못 산다",
			cost: slCost(0, 0, 0, 4, 0), bonus: slCost(0, 0, 0, 1, 0),
			tokens:    slTokens(0, 0, 0, 1, 0, 1),
			wantSpend: slCost(0, 0, 0, 1, 0), wantGold: 2, wantOK: false,
		},
		{
			name: "보너스 + 토큰 + 황금 세 겹 (3단계 큰 카드)",
			cost: slCost(3, 3, 5, 3, 0), bonus: slCost(3, 1, 2, 0, 0),
			tokens:    slTokens(0, 2, 2, 2, 0, 2),
			wantSpend: slCost(0, 2, 2, 2, 0), wantGold: 2, wantOK: true,
		},
		{
			name: "황금만으로 전부 메운다",
			cost: slCost(0, 0, 0, 0, 3), bonus: slCost(0, 0, 0, 0, 0),
			tokens:    slTokens(0, 0, 0, 0, 0, 3),
			wantSpend: slCost(0, 0, 0, 0, 0), wantGold: 3, wantOK: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			card := SLCard{ID: 1, Tier: 1, Gem: SLDiamond, Cost: tc.cost}
			spend, gold, ok := slPayment(card, tc.bonus, tc.tokens)
			if spend != tc.wantSpend || gold != tc.wantGold || ok != tc.wantOK {
				t.Fatalf("slPayment = (%+v, 황금 %d, %v), want (%+v, 황금 %d, %v)",
					spend, gold, ok, tc.wantSpend, tc.wantGold, tc.wantOK)
			}
		})
	}
}

// TestSLBuyAppliesPayment 실제 구매가 보너스 차감·황금 대체를 그대로 반영하고
// 낸 토큰이 공동 창고로 돌아가는지
func TestSLBuyAppliesPayment(t *testing.T) {
	g := slGame(t, 3, 7)
	p := g.Players[0]
	p.Cards = slCost(1, 0, 0, 0, 0)       // 다이아몬드 보너스 1
	p.Tokens = slTokens(0, 2, 0, 0, 0, 2) // 사파이어 2 + 황금 2
	card := SLCard{ID: 9001, Tier: 2, Points: 2, Gem: SLRuby,
		Cost: slCost(2, 3, 0, 0, 0)} // 다이아 2·사파이어 3
	g.Board[1] = append(g.Board[1], card)
	bankBefore := g.Bank

	if err := g.Buy(0, 9001); err != nil {
		t.Fatalf("구매 실패: %v", err)
	}
	// 다이아 2 → 보너스 1 차감 후 1 필요, 토큰 0 → 황금 1
	// 사파이어 3 → 토큰 2 지불, 황금 1
	if p.Tokens != slTokens(0, 0, 0, 0, 0, 0) {
		t.Fatalf("구매 후 보유 토큰 = %+v, want 전부 소진", p.Tokens)
	}
	if p.Points != 2 || p.Cards.Ruby != 1 {
		t.Fatalf("명성 점수 %d · 루비 보너스 %d", p.Points, p.Cards.Ruby)
	}
	wantBank := bankBefore
	wantBank.Sapphire += 2
	wantBank.Gold += 2
	if g.Bank != wantBank {
		t.Fatalf("공동 창고 = %+v, want %+v", g.Bank, wantBank)
	}

	// 못 사는 카드는 한글 오류로 거부되고 상태를 건드리지 않는다
	g.CurrentSeat = 0
	g.Phase = SLPhaseTurn
	pricey := SLCard{ID: 9002, Tier: 3, Points: 5, Gem: SLOnyx, Cost: slCost(0, 0, 0, 7, 3)}
	g.Board[2] = append(g.Board[2], pricey)
	err := g.Buy(0, 9002)
	if err == nil || !hasHangul(err.Error()) {
		t.Fatalf("살 수 없는 카드 구매 오류 = %v", err)
	}
	if p.Points != 2 {
		t.Fatalf("거부된 구매가 상태를 바꿨다: 명성 점수 %d", p.Points)
	}
}

// TestSLTakeRules 토큰 가져오기 표 — 서로 다른 색 3개 / 같은 색 2개(4개 이상)
func TestSLTakeRules(t *testing.T) {
	cases := []struct {
		name    string
		bank    SLTokenSet
		colors  []SLGem
		wantErr bool
		wantGot SLTokenSet
	}{
		{
			name:    "서로 다른 색 3개",
			bank:    slTokens(5, 5, 5, 5, 5, 5),
			colors:  []SLGem{SLDiamond, SLEmerald, SLOnyx},
			wantGot: slTokens(1, 0, 1, 0, 1, 0),
		},
		{
			name:    "같은 색 2개 — 공동 창고 4개면 된다",
			bank:    slTokens(4, 0, 0, 0, 0, 5),
			colors:  []SLGem{SLDiamond, SLDiamond},
			wantGot: slTokens(2, 0, 0, 0, 0, 0),
		},
		{
			name:    "같은 색 2개 — 공동 창고 3개면 안 된다",
			bank:    slTokens(3, 0, 0, 0, 0, 5),
			colors:  []SLGem{SLDiamond, SLDiamond},
			wantErr: true,
		},
		{
			name:    "같은 색 3개는 안 된다",
			bank:    slTokens(7, 0, 0, 0, 0, 5),
			colors:  []SLGem{SLDiamond, SLDiamond, SLDiamond},
			wantErr: true,
		},
		{
			name:    "황금은 직접 못 가져온다",
			bank:    slTokens(5, 5, 5, 5, 5, 5),
			colors:  []SLGem{SLGold, SLDiamond, SLRuby},
			wantErr: true,
		},
		{
			name:    "빈 색은 못 가져온다",
			bank:    slTokens(0, 5, 5, 5, 5, 5),
			colors:  []SLGem{SLDiamond, SLSapphire, SLEmerald},
			wantErr: true,
		},
		{
			name:    "남은 색이 2가지뿐이면 2개만 가져와도 된다",
			bank:    slTokens(0, 0, 0, 2, 2, 5),
			colors:  []SLGem{SLRuby, SLOnyx},
			wantGot: slTokens(0, 0, 0, 1, 1, 0),
		},
		{
			name:    "색이 넉넉한데 2개만 가져오는 건 안 된다",
			bank:    slTokens(5, 5, 5, 5, 5, 5),
			colors:  []SLGem{SLRuby, SLOnyx},
			wantErr: true,
		},
		{
			name:    "4개는 못 가져온다",
			bank:    slTokens(5, 5, 5, 5, 5, 5),
			colors:  []SLGem{SLDiamond, SLSapphire, SLEmerald, SLRuby},
			wantErr: true,
		},
		{
			name:    "빈 선택은 오류",
			bank:    slTokens(5, 5, 5, 5, 5, 5),
			colors:  []SLGem{},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := slGame(t, 3, 11)
			g.Bank = tc.bank
			bankBefore := g.Bank
			err := g.Take(0, tc.colors)
			if tc.wantErr {
				if err == nil {
					t.Fatal("오류를 기대했는데 통과했다")
				}
				if !hasHangul(err.Error()) {
					t.Fatalf("오류 문구가 한글이 아니다: %q", err.Error())
				}
				if g.Bank != bankBefore || g.CurrentSeat != 0 {
					t.Fatalf("거부된 요청이 상태를 바꿨다: %+v seat%d", g.Bank, g.CurrentSeat)
				}
				return
			}
			if err != nil {
				t.Fatalf("통과를 기대했는데 오류: %v", err)
			}
			if g.Players[0].Tokens != tc.wantGot {
				t.Fatalf("보유 토큰 = %+v, want %+v", g.Players[0].Tokens, tc.wantGot)
			}
			for _, gem := range slGems {
				want := bankBefore.get(gem) - tc.wantGot.get(gem)
				if g.Bank.get(gem) != want {
					t.Fatalf("공동 창고 %s = %d, want %d", gem, g.Bank.get(gem), want)
				}
			}
			if g.CurrentSeat != 1 {
				t.Fatalf("차례가 넘어가지 않았다: seat%d", g.CurrentSeat)
			}
		})
	}
}

// TestSLTokenLimit 10개 상한 — 넘으면 discard 단계로 가고, 정확히 초과분만큼
// 버려야 차례가 넘어간다.
func TestSLTokenLimit(t *testing.T) {
	g := slGame(t, 3, 3)
	p := g.Players[0]
	p.Tokens = slTokens(2, 2, 2, 1, 1, 1) // 9개
	g.Bank = slTokens(5, 5, 5, 5, 5, 5)

	if err := g.Take(0, []SLGem{SLDiamond, SLSapphire, SLEmerald}); err != nil {
		t.Fatalf("토큰 가져오기: %v", err)
	}
	if p.Tokens.total() != 12 {
		t.Fatalf("보유 토큰 = %d개", p.Tokens.total())
	}
	if g.Phase != SLPhaseDiscard || g.CurrentSeat != 0 {
		t.Fatalf("10개 초과인데 phase=%s seat=%d", g.Phase, g.CurrentSeat)
	}
	// 초과 상태에서는 다른 행동을 할 수 없다
	if err := g.Take(0, []SLGem{SLRuby, SLOnyx, SLDiamond}); err == nil || !hasHangul(err.Error()) {
		t.Fatalf("버리기 단계의 토큰 가져오기 오류 = %v", err)
	}

	// 개수가 맞지 않는 버리기는 거부
	for _, bad := range [][]SLGem{
		{SLDiamond},
		{SLDiamond, SLDiamond, SLDiamond},
		{SLRuby, SLRuby}, // 루비는 1개뿐
	} {
		if err := g.Discard(0, bad); err == nil || !hasHangul(err.Error()) {
			t.Fatalf("잘못된 버리기(%v) 오류 = %v", bad, err)
		}
		if p.Tokens.total() != 12 {
			t.Fatalf("거부된 버리기가 토큰을 건드렸다: %d개", p.Tokens.total())
		}
	}

	bankBefore := g.Bank
	if err := g.Discard(0, []SLGem{SLDiamond, SLSapphire}); err != nil {
		t.Fatalf("버리기: %v", err)
	}
	if p.Tokens.total() != SLTokenLimit {
		t.Fatalf("버린 뒤 보유 토큰 = %d개, want %d", p.Tokens.total(), SLTokenLimit)
	}
	if g.Bank.Diamond != bankBefore.Diamond+1 || g.Bank.Sapphire != bankBefore.Sapphire+1 {
		t.Fatalf("버린 토큰이 공동 창고로 안 돌아갔다: %+v", g.Bank)
	}
	if g.Phase != SLPhaseTurn || g.CurrentSeat != 1 {
		t.Fatalf("버린 뒤 phase=%s seat=%d", g.Phase, g.CurrentSeat)
	}

	// 자동 버리기도 정확히 10개로 맞춘다
	g.CurrentSeat = 2
	g.Players[2].Tokens = slTokens(3, 3, 3, 3, 0, 2)
	g.Phase = SLPhaseDiscard
	g.ForceDiscard(rand.New(rand.NewSource(5)))
	if g.Players[2].Tokens.total() != SLTokenLimit {
		t.Fatalf("자동 버리기 후 = %d개", g.Players[2].Tokens.total())
	}
	if g.Phase != SLPhaseTurn {
		t.Fatalf("자동 버리기 후 phase = %s", g.Phase)
	}
}

// TestSLReserveRules 예약 — 공개 카드·덱 맨 위·황금 1개·최대 3장
func TestSLReserveRules(t *testing.T) {
	g := slGame(t, 3, 13)
	p := g.Players[0]

	// ① 공개 카드 예약 — 진열에서 빠지고 같은 단계에서 채워진다
	target := g.Board[0][0]
	deckBefore := len(g.Decks[0])
	if err := g.Reserve(0, target.ID, 0); err != nil {
		t.Fatalf("공개 카드 예약: %v", err)
	}
	if len(p.Reserved) != 1 || p.Reserved[0].ID != target.ID {
		t.Fatalf("예약 목록 = %+v", p.Reserved)
	}
	if p.Tokens.Gold != 1 || g.Bank.Gold != SLGoldCount-1 {
		t.Fatalf("황금 = 본인 %d · 공동 창고 %d", p.Tokens.Gold, g.Bank.Gold)
	}
	if len(g.Board[0]) != SLBoardSlots || len(g.Decks[0]) != deckBefore-1 {
		t.Fatalf("진열 %d장 · 덱 %d장", len(g.Board[0]), len(g.Decks[0]))
	}
	if _, idx := g.findBoard(target.ID); idx >= 0 {
		t.Fatal("예약한 카드가 진열에 남아 있다")
	}

	// ② 덱 맨 위 예약 — 진열은 그대로, 덱만 줄어든다
	g.CurrentSeat, g.Phase = 0, SLPhaseTurn
	top := g.Decks[2][0]
	if err := g.Reserve(0, 0, 3); err != nil {
		t.Fatalf("덱 예약: %v", err)
	}
	if len(p.Reserved) != 2 || p.Reserved[1].ID != top.ID {
		t.Fatalf("덱 예약 결과 = %+v", p.Reserved)
	}
	if len(g.Board[2]) != SLBoardSlots {
		t.Fatalf("덱 예약이 진열을 건드렸다: %d장", len(g.Board[2]))
	}

	// ③ 잘못된 지정은 한글 오류
	g.CurrentSeat, g.Phase = 0, SLPhaseTurn
	for _, bad := range []SLReservePayload{{CardID: 99999}, {Tier: 9}, {}} {
		if err := g.Reserve(0, bad.CardID, bad.Tier); err == nil || !hasHangul(err.Error()) {
			t.Fatalf("잘못된 예약(%+v) 오류 = %v", bad, err)
		}
	}

	// ④ 4장째는 거부
	g.CurrentSeat, g.Phase = 0, SLPhaseTurn
	if err := g.Reserve(0, g.Board[1][0].ID, 0); err != nil {
		t.Fatalf("3장째 예약: %v", err)
	}
	g.CurrentSeat, g.Phase = 0, SLPhaseTurn
	err := g.Reserve(0, g.Board[1][0].ID, 0)
	if err == nil || !hasHangul(err.Error()) {
		t.Fatalf("상한 초과 예약 오류 = %v", err)
	}
	if len(p.Reserved) != SLMaxReserved {
		t.Fatalf("예약 장수 = %d", len(p.Reserved))
	}

	// ⑤ 황금이 없으면 예약만 되고 황금은 안 받는다
	g2 := slGame(t, 3, 17)
	g2.Bank.Gold = 0
	if err := g2.Reserve(0, g2.Board[0][0].ID, 0); err != nil {
		t.Fatalf("황금 없는 예약: %v", err)
	}
	if g2.Players[0].Tokens.Gold != 0 {
		t.Fatalf("없는 황금을 받았다: %d", g2.Players[0].Tokens.Gold)
	}
}

// TestSLBuyReserved 예약해 둔 카드는 내 것이고 남은 못 산다
func TestSLBuyReserved(t *testing.T) {
	g := slGame(t, 3, 19)
	card := SLCard{ID: 9100, Tier: 1, Points: 0, Gem: SLEmerald, Cost: slCost(0, 0, 0, 1, 0)}
	g.Players[0].Reserved = []SLCard{card}
	g.Players[0].Tokens = slTokens(0, 0, 0, 1, 0, 0)

	// 남의 예약 카드는 살 수 없다
	g.CurrentSeat = 1
	if err := g.Buy(1, 9100); err == nil || !hasHangul(err.Error()) {
		t.Fatalf("남의 예약 카드 구매 오류 = %v", err)
	}

	g.CurrentSeat = 0
	if err := g.Buy(0, 9100); err != nil {
		t.Fatalf("내 예약 카드 구매: %v", err)
	}
	if len(g.Players[0].Reserved) != 0 || g.Players[0].Cards.Emerald != 1 {
		t.Fatalf("예약 구매 결과: 예약 %d장 · 에메랄드 보너스 %d",
			len(g.Players[0].Reserved), g.Players[0].Cards.Emerald)
	}
}

// TestSLNobleAward 귀족 타일 획득 — 요구 보너스를 모두 채우면 차례 끝에 자동으로
// 오고, 여러 장에 해당하면 번호가 앞선 한 장만 온다.
func TestSLNobleAward(t *testing.T) {
	nobleA := SLNoble{ID: 3, Points: 3, Cost: slCost(3, 3, 3, 0, 0)}
	nobleB := SLNoble{ID: 7, Points: 3, Cost: slCost(4, 4, 0, 0, 0)}

	cases := []struct {
		name       string
		nobles     []SLNoble
		bonus      SLGemSet
		wantNobles []int
		wantPoints int
		wantLeft   int
	}{
		{
			name:   "요구를 못 채우면 안 온다",
			nobles: []SLNoble{nobleA, nobleB}, bonus: slCost(3, 2, 3, 0, 0),
			wantNobles: []int{}, wantPoints: 0, wantLeft: 2,
		},
		{
			name:   "딱 맞추면 온다",
			nobles: []SLNoble{nobleA, nobleB}, bonus: slCost(3, 3, 3, 0, 0),
			wantNobles: []int{3}, wantPoints: 3, wantLeft: 1,
		},
		{
			name:   "넘쳐도 온다",
			nobles: []SLNoble{nobleB}, bonus: slCost(5, 6, 0, 0, 0),
			wantNobles: []int{7}, wantPoints: 3, wantLeft: 0,
		},
		{
			name:   "둘 다 해당하면 번호가 앞선 것 하나만",
			nobles: []SLNoble{nobleA, nobleB}, bonus: slCost(4, 4, 3, 0, 0),
			wantNobles: []int{3}, wantPoints: 3, wantLeft: 1,
		},
		{
			name:   "귀족이 없으면 아무 일도 없다",
			nobles: []SLNoble{}, bonus: slCost(9, 9, 9, 9, 9),
			wantNobles: []int{}, wantPoints: 0, wantLeft: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := slGame(t, 3, 23)
			g.Nobles = append([]SLNoble{}, tc.nobles...)
			p := g.Players[0]
			p.Cards = tc.bonus
			p.Points = 0
			g.DrainEvents()

			g.awardNoble(p)

			if len(p.Nobles) != len(tc.wantNobles) {
				t.Fatalf("획득 귀족 = %v, want %v", p.Nobles, tc.wantNobles)
			}
			for i, id := range tc.wantNobles {
				if p.Nobles[i] != id {
					t.Fatalf("획득 귀족 = %v, want %v", p.Nobles, tc.wantNobles)
				}
			}
			if p.Points != tc.wantPoints {
				t.Fatalf("명성 점수 = %d, want %d", p.Points, tc.wantPoints)
			}
			if len(g.Nobles) != tc.wantLeft {
				t.Fatalf("남은 귀족 = %d장, want %d", len(g.Nobles), tc.wantLeft)
			}
			if len(tc.wantNobles) > 0 {
				events := g.DrainEvents()
				if len(events) == 0 || events[0].Kind != "noble" {
					t.Fatalf("귀족 이벤트가 없다: %+v", events)
				}
				if !hasHangul(events[0].Message) {
					t.Fatalf("귀족 이벤트 문구 = %q", events[0].Message)
				}
			}
		})
	}

	// 여러 장 해당 시 "한 장만"이라는 사실을 이벤트로 알린다
	g := slGame(t, 3, 29)
	g.Nobles = []SLNoble{nobleA, nobleB}
	g.Players[0].Cards = slCost(4, 4, 3, 0, 0)
	g.DrainEvents()
	g.awardNoble(g.Players[0])
	events := g.DrainEvents()
	if len(events) == 0 || !strings.Contains(events[0].Message, "귀족 타일") ||
		!strings.Contains(events[0].Message, "한 장") {
		t.Fatalf("복수 해당 안내 문구 = %+v", events)
	}
}

// TestSLLastRound 15점에 닿으면 그 라운드를 끝까지 진행하고 끝난다 —
// 좌석마다 같은 횟수의 차례를 가졌음이 보장돼야 한다.
func TestSLLastRound(t *testing.T) {
	cases := []struct {
		name string
		// hitSeat 15점에 닿는 좌석
		hitSeat int
		// wantMoreTurns 그 뒤로 더 진행돼야 하는 차례 수
		wantMoreTurns int
	}{
		{name: "좌석 0이 먼저 닿으면 나머지 2명이 더 한다", hitSeat: 0, wantMoreTurns: 2},
		{name: "좌석 1이 닿으면 좌석 2가 한 번 더", hitSeat: 1, wantMoreTurns: 1},
		{name: "마지막 좌석이 닿으면 그 자리에서 끝", hitSeat: 2, wantMoreTurns: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := slGame(t, 3, 31)
			// 한 차례 진행 — 10개 상한에 걸리지 않게 매번 판을 되돌린다
			step := func() {
				seat := g.CurrentSeat
				g.Bank = slTokens(9, 9, 9, 9, 9, 5)
				g.Players[seat].Tokens = SLTokenSet{}
				if err := g.Take(seat, []SLGem{SLDiamond, SLSapphire, SLEmerald}); err != nil {
					t.Fatalf("진행: %v", err)
				}
			}
			for g.CurrentSeat != tc.hitSeat { // hitSeat 차례까지 아무 일 없이
				step()
			}
			g.Players[tc.hitSeat].Points = SLWinPoints
			step()

			more := 0
			for g.Phase != SLPhaseGameOver {
				if more > 5 {
					t.Fatal("마지막 라운드가 끝나지 않는다")
				}
				step()
				more++
			}
			if more != tc.wantMoreTurns {
				t.Fatalf("도달 후 추가 차례 = %d, want %d", more, tc.wantMoreTurns)
			}
			if !g.LastRound {
				t.Fatal("lastRound 가 켜지지 않았다")
			}
			if g.Turns%len(g.Players) != 0 {
				t.Fatalf("차례 수 %d 가 인원 %d 의 배수가 아니다 (불공평한 종료)",
					g.Turns, len(g.Players))
			}
		})
	}
}

// TestSLWinnerDecision 종료 판정 표 — 최고 명성 점수, 동점이면 개발 카드가
// 적은 쪽, 그래도 같으면 공동 승
func TestSLWinnerDecision(t *testing.T) {
	cases := []struct {
		name string
		// 좌석별 (명성 점수, 개발 카드 수)
		points []int
		cards  []int
		want   []int
	}{
		{name: "최고점 단독", points: []int{15, 12, 9}, cards: []int{10, 9, 8}, want: []int{0}},
		{name: "동점이면 개발 카드가 적은 쪽", points: []int{15, 15, 3}, cards: []int{11, 9, 2}, want: []int{1}},
		{name: "점수·카드까지 같으면 공동 승", points: []int{16, 16, 5}, cards: []int{10, 10, 4}, want: []int{0, 1}},
		{name: "세 명 공동 승", points: []int{15, 15, 15}, cards: []int{9, 9, 9}, want: []int{0, 1, 2}},
		{name: "카드가 적어도 점수가 낮으면 진다", points: []int{15, 14}, cards: []int{12, 3}, want: []int{0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := slGame(t, len(tc.points), 37)
			for i, p := range g.Players {
				p.Points = tc.points[i]
				p.Cards = SLGemSet{Diamond: tc.cards[i]}
			}
			g.finish("검증")
			if g.Result == nil {
				t.Fatal("결과가 없다")
			}
			got := append([]int{}, g.Result.WinnerSeats...)
			sort.Ints(got)
			if len(got) != len(tc.want) {
				t.Fatalf("승자 = %v, want %v", got, tc.want)
			}
			for i, seat := range tc.want {
				if got[i] != seat {
					t.Fatalf("승자 = %v, want %v", got, tc.want)
				}
			}
			if len(g.Result.WinnerNames) != len(tc.want) || !hasHangul(g.Result.Message) {
				t.Fatalf("결과 = %+v", g.Result)
			}
		})
	}
}

// TestSLStartSetup 인원별 공동 창고·귀족 타일·진열 세팅
func TestSLStartSetup(t *testing.T) {
	for _, n := range []int{2, 3, 4} {
		g := slGame(t, n, int64(41+n))
		per := slBankFor(n)
		want := slTokens(per, per, per, per, per, SLGoldCount)
		if g.Bank != want {
			t.Fatalf("%d인 공동 창고 = %+v, want %+v", n, g.Bank, want)
		}
		if len(g.Nobles) != slNobleCount(n) {
			t.Fatalf("%d인 귀족 타일 = %d장, want %d", n, len(g.Nobles), slNobleCount(n))
		}
		for tier := 1; tier <= 3; tier++ {
			if len(g.Board[tier-1]) != SLBoardSlots {
				t.Fatalf("%d인 %d단계 진열 = %d장", n, tier, len(g.Board[tier-1]))
			}
		}
		if len(g.Decks[0])+SLBoardSlots != SLDeckTier1 {
			t.Fatalf("1단계 덱 잔량 = %d", len(g.Decks[0]))
		}
		if g.Phase != SLPhaseTurn || g.CurrentSeat != 0 || !g.Ready {
			t.Fatalf("시작 상태: phase=%s seat=%d ready=%v", g.Phase, g.CurrentSeat, g.Ready)
		}
		// 귀족 타일은 번호 오름차순으로 진열된다 (자동 획득의 "앞선 번호" 근거)
		for i := 1; i < len(g.Nobles); i++ {
			if g.Nobles[i-1].ID >= g.Nobles[i].ID {
				t.Fatalf("귀족 진열이 번호순이 아니다: %+v", g.Nobles)
			}
		}
	}
	// 1인은 시작할 수 없다
	g := NewSLGame("solo")
	g.AddPlayer("혼자")
	if err := g.Start(rand.New(rand.NewSource(1))); err == nil || !hasHangul(err.Error()) {
		t.Fatalf("1인 시작 오류 = %v", err)
	}
}

// TestSLForceAction AFK 자동 행동이 언제나 판을 앞으로 민다
func TestSLForceAction(t *testing.T) {
	rng := rand.New(rand.NewSource(101))

	// ① 공동 창고가 넉넉하면 토큰을 가져온다
	g := slGame(t, 3, 43)
	g.ForceAction(rng)
	if g.Players[0].Tokens.total() != SLTakeDistinct || g.CurrentSeat != 1 {
		t.Fatalf("자동 토큰: %+v seat%d", g.Players[0].Tokens, g.CurrentSeat)
	}

	// ② 공동 창고가 비면 살 수 있는 카드를 산다
	g = slGame(t, 3, 47)
	g.Bank = slTokens(0, 0, 0, 0, 0, 0)
	cheap := SLCard{ID: 9200, Tier: 1, Points: 1, Gem: SLRuby, Cost: slCost(0, 0, 0, 0, 1)}
	g.Board[0] = []SLCard{cheap}
	g.Players[0].Tokens = slTokens(0, 0, 0, 0, 1, 0)
	g.ForceAction(rng)
	if g.Players[0].Points != 1 {
		t.Fatalf("자동 구매가 안 됐다: 명성 점수 %d", g.Players[0].Points)
	}

	// ③ 토큰도 구매도 못 하면 예약한다
	g = slGame(t, 3, 53)
	g.Bank = slTokens(0, 0, 0, 0, 0, 0)
	g.Players[0].Cards = SLGemSet{}
	g.ForceAction(rng)
	if len(g.Players[0].Reserved) != 1 {
		t.Fatalf("자동 예약이 안 됐다: %+v", g.Players[0].Reserved)
	}

	// ④ 아무것도 못 하면 차례만 넘긴다 (판이 멈추지 않는다)
	g = slGame(t, 3, 59)
	g.Bank = slTokens(0, 0, 0, 0, 0, 0)
	for tier := 0; tier < 3; tier++ {
		g.Board[tier] = []SLCard{}
		g.Decks[tier] = []SLCard{}
	}
	before := g.Turns
	g.ForceAction(rng)
	if g.Turns != before+1 || g.CurrentSeat != 1 {
		t.Fatalf("무행동 차례 넘김 실패: turns %d→%d seat%d", before, g.Turns, g.CurrentSeat)
	}
}

// ==================== 봇 품질 측정 ====================

// slBotFixture 허브 고루틴 없이 봇을 돌리기 위한 방 (소켓 없는 사람 취급)
func slBotFixture(t *testing.T, n int, seed int64) (*SLHub, *slRoom, []*SLClient) {
	t.Helper()
	h := NewSLHub()
	h.rng = rand.New(rand.NewSource(seed))
	room := h.lobbyRoomFor("")
	clients := make([]*SLClient, n)
	for i := range clients {
		c := &SLClient{wsClient: newBotWSClient(), Hub: h}
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

// slDrain 봇 채널에 쌓인 메시지를 버린다 (버퍼 포화로 연결이 끊기지 않게)
func slDrain(clients []*SLClient) {
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

// slRunBotGame 3봇 한 판을 끝까지 돌리고 (차례 수, 좌석별 명성 점수, 승자 좌석)
// 을 돌려준다. 스냅샷 → 두뇌 → 허브 핸들러 경로가 실제 WS 경로와 같다.
func slRunBotGame(t *testing.T, n int, seed int64) (turns int, points []int, winners []int) {
	t.Helper()
	h, room, clients := slBotFixture(t, n, seed)
	game := room.Game
	brains := make([]*slBrain, n)
	for i := range brains {
		brains[i] = &slBrain{rng: rand.New(rand.NewSource(seed*1000 + int64(i)))}
	}

	for step := 0; step < SLMaxTurns*4 && game.Phase != SLPhaseGameOver; step++ {
		seat := game.CurrentSeat
		if seat < 0 || seat >= n {
			break
		}
		raw, err := json.Marshal(h.buildSLState(room, seat))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var payload interface{}
		json.Unmarshal(raw, &payload)

		beforeSeq := game.StateSeq
		if reply := brains[seat].decide(SLMessage{Type: SLMsgGameState, Payload: payload}); reply != nil {
			h.handleGameMessage(SLGameMessage{Client: clients[seat], Message: *reply})
		}
		h.stopPhaseTimer(room)
		if game.StateSeq == beforeSeq { // 봇이 막히면 규칙의 자동 진행으로 민다
			if game.Phase == SLPhaseDiscard {
				game.ForceDiscard(h.rng)
			} else {
				game.ForceAction(h.rng)
			}
			game.DrainEvents()
		}
		slDrain(clients)
	}
	if game.Phase != SLPhaseGameOver {
		t.Fatalf("seed %d: %d차례에도 끝나지 않았다", seed, game.Turns)
	}

	turns = game.Turns
	for _, p := range game.Players {
		points = append(points, p.Points)
	}
	if game.Result != nil {
		winners = append([]int{}, game.Result.WinnerSeats...)
	}
	return turns, points, winners
}

// TestSLBotQuality 3봇 30판의 평균 소요 차례와 명성 점수 분포를 숫자로 남긴다.
// 100차례를 넘거나 특정 좌석이 매번 이기면 가치 함수가 무너진 것이다.
func TestSLBotQuality(t *testing.T) {
	const games = 30
	const seats = 3

	totalTurns, minTurns, maxTurns := 0, 1<<30, 0
	wins := make([]int, seats)
	winPoints := []int{}
	allPoints := []int{}
	slowGames := 0

	for i := 0; i < games; i++ {
		turns, points, winners := slRunBotGame(t, seats, int64(1000+i))
		totalTurns += turns
		if turns < minTurns {
			minTurns = turns
		}
		if turns > maxTurns {
			maxTurns = turns
		}
		if turns > 100 {
			slowGames++
		}
		for _, s := range winners {
			wins[s]++
		}
		best := 0
		for _, pt := range points {
			allPoints = append(allPoints, pt)
			if pt > best {
				best = pt
			}
		}
		winPoints = append(winPoints, best)
	}

	avgTurns := float64(totalTurns) / games
	sort.Ints(winPoints)
	sort.Ints(allPoints)
	sum := 0
	for _, p := range allPoints {
		sum += p
	}
	t.Logf("봇 품질 %d판(%d인): 평균 소요 차례 %.1f (최소 %d · 최대 %d · 100차례 초과 %d판)",
		games, seats, avgTurns, minTurns, maxTurns, slowGames)
	t.Logf("  승자 명성 점수 분포: 최소 %d · 중앙 %d · 최대 %d",
		winPoints[0], winPoints[len(winPoints)/2], winPoints[len(winPoints)-1])
	t.Logf("  전체 명성 점수 분포: 최소 %d · 중앙 %d · 최대 %d · 평균 %.1f",
		allPoints[0], allPoints[len(allPoints)/2], allPoints[len(allPoints)-1],
		float64(sum)/float64(len(allPoints)))
	t.Logf("  좌석별 승수: %v (총 %d판)", wins, games)

	if avgTurns > 100 {
		t.Fatalf("평균 소요 차례 %.1f — 100을 넘으면 가치 함수를 손봐야 한다", avgTurns)
	}
	for seat, w := range wins {
		if w == games {
			t.Fatalf("seat%d 가 %d판을 모두 이겼다 — 선 이점이 굳어 있다", seat, games)
		}
	}
	if winPoints[0] < SLWinPoints {
		t.Fatalf("승자 최소 명성 점수 = %d — 15점에 못 닿고 끝난 판이 있다", winPoints[0])
	}
}
