package server

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

// ==================== 보난자 순수 규칙 테스트 ====================
//
// 이 게임의 전부는 (1) 손패 순서 불변, (2) 콩미터 금화 계산(강낭콩 예외),
// (3) 1장짜리 밭 수확 제한, (4) 모든 거래에 차례인 사람이 끼는 제약,
// (5) 덱 소진 종료(3인은 2회)와 동점 처리다. 다섯 다 표 기반으로 못박는다.

// bzFixture 결정적 시드로 시작된 n인 게임
func bzFixture(t *testing.T, n int) *BZGame {
	t.Helper()
	g := NewBZGame("bz-test")
	for i := 0; i < n; i++ {
		if _, err := g.AddPlayer(fmt.Sprintf("P%d", i)); err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
	}
	if err := g.Start(rand.New(rand.NewSource(20260829))); err != nil {
		t.Fatalf("Start: %v", err)
	}
	g.DrainEvents()
	return g
}

// bzSetBoard 판을 손으로 세운다 (덱·차례 좌석 고정, 밭·손패·금화 초기화)
func bzSetBoard(g *BZGame, seat int, deck []BZBean) {
	g.Deck = append([]BZBean{}, deck...)
	g.Discard = []BZBean{}
	g.Flipped = []BZBean{}
	g.Offers = []*BZOffer{}
	g.CurrentSeat = seat
	g.Phase = BZPhasePlant
	for _, p := range g.Players {
		p.Hand = []BZBean{}
		p.Pending = []BZBean{}
		p.Fields = bzNewFields()
		p.Coins = 0
		p.BoughtField = false
	}
	g.DrainEvents()
}

// bzTotalCards 판 안의 카드 총량 — 덱 + 버린 더미 + 공개 + 손패 + 받은 카드 +
// 밭 + 금화 + 세 번째 밭 값. 항상 104장이어야 한다.
func bzTotalCards(g *BZGame) int {
	total := len(g.Deck) + len(g.Discard) + len(g.Flipped) + g.SpentCoins
	for _, p := range g.Players {
		total += len(p.Hand) + len(p.Pending) + p.Coins
		for _, f := range p.Fields {
			total += f.Count
		}
	}
	return total
}

// bzSameSeq 두 콩 나열이 순서까지 같은지
func bzSameSeq(a, b []BZBean) bool {
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

// ==================== 콩 8종 · 콩미터 ====================

// TestBZBeanTable 콩 8종 — 장수 합이 104장이고 표기가 전부 한글이다
func TestBZBeanTable(t *testing.T) {
	want := map[BZBean]int{
		BZBlue: 20, BZChili: 18, BZStink: 16, BZGreen: 14,
		BZSoy: 12, BZBlackeyed: 10, BZRed: 8, BZGarden: 6,
	}
	wantName := map[BZBean]string{
		BZBlue: "푸르대콩", BZChili: "칠리콩", BZStink: "메주콩", BZGreen: "완두콩",
		BZSoy: "대두", BZBlackeyed: "동부", BZRed: "팥", BZGarden: "강낭콩",
	}
	if len(bzBeanDefs) != 8 {
		t.Fatalf("콩 종류 = %d, want 8", len(bzBeanDefs))
	}
	if bzDeckSize() != 104 {
		t.Fatalf("콩 장수 합 = %d, want 104", bzDeckSize())
	}

	deck := bzBuildDeck()
	if len(deck) != 104 {
		t.Fatalf("덱 장수 = %d, want 104", len(deck))
	}
	got := map[BZBean]int{}
	for _, b := range deck {
		got[b]++
	}
	if len(got) != 8 {
		t.Fatalf("덱에 든 콩 종류 = %d, want 8", len(got))
	}
	for bean, n := range want {
		if got[bean] != n {
			t.Fatalf("%s(%s) 장수 = %d, want %d", bzName(bean), bean, got[bean], n)
		}
		if bzName(bean) != wantName[bean] {
			t.Fatalf("%s 표기 = %q, want %q", bean, bzName(bean), wantName[bean])
		}
	}
	for _, def := range bzBeanDefs {
		if !hasHangul(def.Name) || def.Color == "" || def.Emoji == "" {
			t.Fatalf("콩 표기 누락: %+v", def)
		}
	}
}

// TestBZCoinsTable 콩미터 — 장수별 금화를 콩 8종 전부에 대해 표로 고정한다.
// **강낭콩은 금화 1개·4개 칸이 없다**: 2장 → 2개, 3장 이상 → 3개로 굳는다.
func TestBZCoinsTable(t *testing.T) {
	// 인덱스가 곧 장수 (0장부터)
	table := map[BZBean][]int{
		//               0  1  2  3  4  5  6  7  8  9 10 11 12
		BZBlue:      {0, 0, 0, 0, 1, 1, 2, 2, 3, 3, 4, 4, 4},
		BZChili:     {0, 0, 0, 1, 1, 1, 2, 2, 3, 4, 4, 4, 4},
		BZStink:     {0, 0, 0, 1, 1, 2, 2, 3, 4, 4, 4, 4, 4},
		BZGreen:     {0, 0, 0, 1, 1, 2, 3, 4, 4, 4, 4, 4, 4},
		BZSoy:       {0, 0, 1, 1, 2, 2, 3, 4, 4, 4, 4, 4, 4},
		BZBlackeyed: {0, 0, 1, 1, 2, 3, 4, 4, 4, 4, 4, 4, 4},
		BZRed:       {0, 0, 1, 2, 3, 4, 4, 4, 4, 4, 4, 4, 4},
		BZGarden:    {0, 0, 2, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3},
	}
	for bean, row := range table {
		for count, want := range row {
			if got := bzCoins(bean, count); got != want {
				t.Fatalf("bzCoins(%s=%s, %d장) = %d, want %d",
					bzName(bean), bean, count, got, want)
			}
		}
	}

	// 강낭콩 예외를 따로 못박는다 — 금화 1개가 되는 장수가 아예 없다
	for count := 0; count <= 20; count++ {
		if bzCoins(BZGarden, count) == 1 {
			t.Fatalf("강낭콩 %d장에서 금화 1개가 나왔다 (금화 1개 칸이 없어야 한다)", count)
		}
		if bzCoins(BZGarden, count) == 4 {
			t.Fatalf("강낭콩 %d장에서 금화 4개가 나왔다 (금화 4개 칸이 없어야 한다)", count)
		}
	}
	if bzCoins(BZGarden, 2) != 2 || bzCoins(BZGarden, 3) != 3 || bzCoins(BZGarden, 6) != 3 {
		t.Fatalf("강낭콩 = %d/%d/%d, want 2/3/3",
			bzCoins(BZGarden, 2), bzCoins(BZGarden, 3), bzCoins(BZGarden, 6))
	}
	// 음수·모르는 콩은 0
	if bzCoins(BZRed, -1) != 0 || bzCoins(BZBean("없는콩"), 5) != 0 {
		t.Fatal("잘못된 입력이 0이 아니다")
	}
	// 금화는 절대 장수를 넘지 않는다 (수확 회계의 근거)
	for _, def := range bzBeanDefs {
		for count := 0; count <= def.Count; count++ {
			if bzCoins(def.ID, count) > count {
				t.Fatalf("%s %d장의 금화 %d개가 장수를 넘었다",
					def.Name, count, bzCoins(def.ID, count))
			}
		}
	}

	// 다음 문턱 (봇이 "문턱 직전이면 버틴다"를 판단하는 근거)
	next := []struct {
		bean  BZBean
		count int
		want  int
	}{
		{BZBlue, 0, 4}, {BZBlue, 4, 6}, {BZBlue, 9, 10}, {BZBlue, 10, 0}, {BZBlue, 15, 0},
		{BZGarden, 0, 2}, {BZGarden, 2, 3}, {BZGarden, 3, 0}, {BZGarden, 6, 0},
		{BZRed, 1, 2}, {BZRed, 4, 5}, {BZRed, 5, 0},
	}
	for _, tc := range next {
		if got := bzNextThreshold(tc.bean, tc.count); got != tc.want {
			t.Fatalf("bzNextThreshold(%s, %d) = %d, want %d",
				bzName(tc.bean), tc.count, got, tc.want)
		}
	}
}

// ==================== 시작 배치 ====================

// TestBZSetup 시작 — 각자 손패 5장·콩밭 2개·금화 0, 3인은 2번째 소진에서 끝
func TestBZSetup(t *testing.T) {
	for _, n := range []int{BZMinPlayers, 4, BZMaxPlayers} {
		g := bzFixture(t, n)
		if !g.Ready || g.Phase != BZPhasePlant {
			t.Fatalf("%d인 시작 phase = %s", n, g.Phase)
		}
		if g.CurrentSeat < 0 || g.CurrentSeat >= n {
			t.Fatalf("%d인 선 = %d", n, g.CurrentSeat)
		}
		wantCycle := 3
		if n == 3 {
			wantCycle = 2
		}
		if g.EndCycle != wantCycle {
			t.Fatalf("%d인 종료 소진 횟수 = %d, want %d", n, g.EndCycle, wantCycle)
		}
		if len(g.Deck) != 104-BZStartHand*n {
			t.Fatalf("%d인 덱 잔량 = %d, want %d", n, len(g.Deck), 104-BZStartHand*n)
		}
		for _, p := range g.Players {
			if len(p.Hand) != BZStartHand {
				t.Fatalf("%d인 seat%d 손패 = %d장", n, p.Seat, len(p.Hand))
			}
			if len(p.Fields) != BZStartFields {
				t.Fatalf("%d인 seat%d 콩밭 = %d개", n, p.Seat, len(p.Fields))
			}
			for _, f := range p.Fields {
				if f.Count != 0 || f.Bean != "" {
					t.Fatalf("%d인 seat%d 시작 밭 = %+v", n, p.Seat, p.Fields)
				}
			}
			if p.Coins != 0 || len(p.Pending) != 0 {
				t.Fatalf("%d인 seat%d 시작 금화/받은 카드 = %d / %v", n, p.Seat, p.Coins, p.Pending)
			}
		}
		if bzTotalCards(g) != 104 {
			t.Fatalf("%d인 시작 카드 총량 = %d, want 104", n, bzTotalCards(g))
		}
	}
	if bzEndCycle(3) != 2 || bzEndCycle(4) != 3 || bzEndCycle(5) != 3 {
		t.Fatalf("종료 소진 횟수 = %d/%d/%d", bzEndCycle(3), bzEndCycle(4), bzEndCycle(5))
	}
}

// ==================== 손패 순서 불변 (이 게임의 전부) ====================

// TestBZHandOrderIsImmutable 손패는 **맨 앞에서만 빠지고 맨 뒤로만 붙는다**.
// 한 차례를 통째로 돌려 순서가 한 번도 흐트러지지 않는지 본다.
func TestBZHandOrderIsImmutable(t *testing.T) {
	g := bzFixture(t, 3)
	seat := 0
	// 공개 2장(팥·팥) → 받은 카드로 심고, 뽑기 3장(동부·동부·동부).
	// 뒤쪽 채움 카드는 차례가 넘어간 뒤 덱이 마르지 않게 두는 여유분이다.
	deck := []BZBean{BZRed, BZRed, BZBlackeyed, BZBlackeyed, BZBlackeyed}
	for i := 0; i < 20; i++ {
		deck = append(deck, BZGarden)
	}
	bzSetBoard(g, seat, deck)
	p := g.Players[seat]
	p.Hand = []BZBean{BZBlue, BZChili, BZStink, BZGreen, BZSoy}
	// 다음 좌석들도 손패를 쥐고 있어야 차례가 넘어가도 판이 멈춰 선다
	for _, q := range g.Players[1:] {
		q.Hand = []BZBean{BZChili, BZChili, BZChili}
	}
	start := append([]BZBean{}, p.Hand...)

	// ① 심기 — 맨 앞 한 장만 빠진다
	if err := g.Plant(seat, false); err != nil {
		t.Fatalf("심기 실패: %v", err)
	}
	if !bzSameSeq(p.Hand, start[1:]) {
		t.Fatalf("심은 뒤 손패 = %v, want %v (맨 앞만 빠져야 한다)", p.Hand, start[1:])
	}
	if p.Fields[0].Bean != BZBlue || p.Fields[0].Count != 1 {
		t.Fatalf("밭 = %+v", p.Fields)
	}
	if g.Phase != BZPhaseTrade || len(g.Flipped) != BZFlipCount {
		t.Fatalf("2단계 진입 실패: phase=%s flipped=%v", g.Phase, g.Flipped)
	}

	// ② 거래 마감 — 아무도 안 가져간 공개 카드는 차례인 사람이 심는다
	if err := g.EndPhase(seat); err != nil {
		t.Fatalf("거래 마감 실패: %v", err)
	}
	if g.Phase != BZPhasePlantReceived || len(p.Pending) != BZFlipCount {
		t.Fatalf("3단계 진입 실패: phase=%s pending=%v", g.Phase, p.Pending)
	}
	if !bzSameSeq(p.Hand, start[1:]) {
		t.Fatalf("거래 마감이 손패를 건드렸다: %v", p.Hand)
	}

	// ③ 받은 카드 심기 — 손패는 그대로다
	for len(p.Pending) > 0 {
		if err := g.PlantReceived(seat, 0, 1); err != nil {
			t.Fatalf("받은 카드 심기 실패: %v", err)
		}
	}
	if p.Fields[1].Bean != BZRed || p.Fields[1].Count != 2 {
		t.Fatalf("받은 카드 밭 = %+v", p.Fields)
	}

	// ④ 뽑기 3장 — **맨 뒤로만** 붙는다. 앞쪽은 한 글자도 안 바뀐다.
	want := append(append([]BZBean{}, start[1:]...), BZBlackeyed, BZBlackeyed, BZBlackeyed)
	if !bzSameSeq(p.Hand, want) {
		t.Fatalf("뽑은 뒤 손패 = %v, want %v", p.Hand, want)
	}
	if g.CurrentSeat != 1 || g.Phase != BZPhasePlant {
		t.Fatalf("차례 이양 실패: seat=%d phase=%s", g.CurrentSeat, g.Phase)
	}

	// ---- 거래로 중간 카드가 빠져도 남은 상대 순서는 그대로다 ----
	cards := []BZBean{BZBlue, BZChili, BZStink, BZGreen, BZSoy}
	picked, rest := bzPickByIndexes(cards, []int{1, 3})
	if !bzSameSeq(picked, []BZBean{BZChili, BZGreen}) {
		t.Fatalf("뽑아낸 카드 = %v", picked)
	}
	if !bzSameSeq(rest, []BZBean{BZBlue, BZStink, BZSoy}) {
		t.Fatalf("남은 손패 = %v, want [blue stink soy] (상대 순서 유지)", rest)
	}
	// 빈 지목은 손패를 그대로 둔다
	if _, same := bzPickByIndexes(cards, nil); !bzSameSeq(same, cards) {
		t.Fatal("빈 지목이 손패를 바꿨다")
	}
}

// TestBZPlantOnlyFromFront 심기는 맨 앞(과 두 번째)만 — 세 번째부터는
// 심을 방법이 아예 없다 (bz_plant 에 인덱스 인자가 없다).
func TestBZPlantOnlyFromFront(t *testing.T) {
	g := bzFixture(t, 3)
	seat := 0
	bzSetBoard(g, seat, []BZBean{BZGarden, BZGarden, BZGarden, BZGarden, BZGarden})
	p := g.Players[seat]
	p.Hand = []BZBean{BZRed, BZRed, BZBlue, BZChili}

	// 두 장까지 심는다 — 둘 다 팥이라 한 밭에 쌓인다
	if err := g.Plant(seat, true); err != nil {
		t.Fatalf("두 장 심기 실패: %v", err)
	}
	if p.Fields[0].Bean != BZRed || p.Fields[0].Count != 2 {
		t.Fatalf("밭 = %+v", p.Fields)
	}
	if !bzSameSeq(p.Hand, []BZBean{BZBlue, BZChili}) {
		t.Fatalf("손패 = %v, want [blue chili]", p.Hand)
	}

	// 한 차례에 또 심을 수는 없다
	if err := g.Plant(seat, false); err == nil {
		t.Fatal("2단계인데 또 심었다")
	} else if !hasHangul(err.Error()) {
		t.Fatalf("오류 문구가 한글이 아니다: %q", err.Error())
	}

	// ---- 밭이 꽉 차면 심을 수 없다 (먼저 수확해야 한다) ----
	g2 := bzFixture(t, 3)
	bzSetBoard(g2, 0, []BZBean{BZGarden, BZGarden, BZGarden})
	q := g2.Players[0]
	q.Fields[0] = BZField{Bean: BZBlue, Count: 3}
	q.Fields[1] = BZField{Bean: BZChili, Count: 4}
	q.Hand = []BZBean{BZRed, BZRed}
	err := g2.Plant(0, false)
	if err == nil {
		t.Fatal("맞는 밭도 빈 밭도 없는데 심었다")
	}
	if !hasHangul(err.Error()) || !strings.Contains(err.Error(), "수확") {
		t.Fatalf("오류 문구 = %q", err.Error())
	}
	if len(q.Hand) != 2 || q.Fields[0].Count != 3 {
		t.Fatal("거부된 심기가 판을 바꿨다")
	}
	// 수확해 자리를 만들면 심을 수 있다
	if err := g2.Harvest(0, 0); err != nil {
		t.Fatalf("수확 실패: %v", err)
	}
	if err := g2.Plant(0, false); err != nil {
		t.Fatalf("자리를 만든 뒤에도 못 심었다: %v", err)
	}

	// ---- 두 번째 카드를 놓을 자리가 없으면 통째로 거부한다 (반쪽 적용 금지) ----
	g3 := bzFixture(t, 3)
	bzSetBoard(g3, 0, []BZBean{BZGarden, BZGarden, BZGarden})
	r := g3.Players[0]
	r.Fields[0] = BZField{Bean: BZBlue, Count: 2}
	r.Hand = []BZBean{BZRed, BZChili, BZSoy}
	if err := g3.Plant(0, true); err == nil {
		t.Fatal("두 번째 카드를 놓을 자리가 없는데 통과했다")
	}
	if len(r.Hand) != 3 || r.Fields[1].Count != 0 {
		t.Fatalf("반쪽 적용됐다: hand=%v fields=%+v", r.Hand, r.Fields)
	}
	// second 를 끄면 한 장은 심을 수 있다
	if err := g3.Plant(0, false); err != nil {
		t.Fatalf("한 장 심기 실패: %v", err)
	}

	// ---- 손패가 비면 심기를 건너뛴다 ----
	g4 := bzFixture(t, 3)
	bzSetBoard(g4, 0, []BZBean{BZGarden, BZGarden, BZGarden, BZGarden})
	g4.Players[0].Hand = []BZBean{}
	g4.beginPlantPhase()
	if g4.Phase != BZPhaseTrade {
		t.Fatalf("손패가 빈 차례의 phase = %s, want trade", g4.Phase)
	}
}

// ==================== 수확 / 콩밭 ====================

// TestBZHarvestSingleFieldRule **카드가 2장 이상인 밭이 있으면 1장짜리 밭은
// 수확할 수 없다** (모든 밭이 1장이면 아무거나 가능) — 표로 고정한다.
func TestBZHarvestSingleFieldRule(t *testing.T) {
	cases := []struct {
		name   string
		fields []BZField
		want   []bool
	}{
		{"전부 1장이면 아무거나", []BZField{{BZRed, 1}, {BZBlue, 1}}, []bool{true, true}},
		{"2장 밭이 있으면 1장 밭은 못 판다", []BZField{{BZRed, 1}, {BZBlue, 2}}, []bool{false, true}},
		{"큰 밭이 앞에 있어도 같다", []BZField{{BZRed, 3}, {BZBlue, 1}}, []bool{true, false}},
		{"빈 밭은 못 판다", []BZField{{"", 0}, {BZBlue, 1}}, []bool{false, true}},
		{"빈 밭 + 1장 + 2장", []BZField{{"", 0}, {BZBlue, 1}, {BZRed, 2}}, []bool{false, false, true}},
		{"전부 비면 아무것도 못 판다", []BZField{{"", 0}, {"", 0}}, []bool{false, false}},
		{"1장 셋", []BZField{{BZRed, 1}, {BZBlue, 1}, {BZSoy, 1}}, []bool{true, true, true}},
	}
	for _, tc := range cases {
		for i, want := range tc.want {
			if got := bzCanHarvest(tc.fields, i); got != want {
				t.Fatalf("%s: bzCanHarvest(%v, %d) = %v, want %v",
					tc.name, tc.fields, i, got, want)
			}
		}
		// 범위 밖은 항상 false
		if bzCanHarvest(tc.fields, -1) || bzCanHarvest(tc.fields, len(tc.fields)) {
			t.Fatalf("%s: 범위 밖 인덱스가 통과했다", tc.name)
		}
	}

	// 규칙 경로에서도 같은 제약이 걸린다 (한글 오류)
	g := bzFixture(t, 3)
	bzSetBoard(g, 0, []BZBean{BZGarden, BZGarden})
	p := g.Players[0]
	p.Fields[0] = BZField{Bean: BZRed, Count: 1}
	p.Fields[1] = BZField{Bean: BZBlue, Count: 4}
	err := g.Harvest(0, 0)
	if err == nil {
		t.Fatal("1장짜리 밭 수확이 통과했다")
	}
	if !hasHangul(err.Error()) {
		t.Fatalf("오류 문구가 한글이 아니다: %q", err.Error())
	}
	if err := g.Harvest(0, 1); err != nil { // 푸르대콩 4장 → 금화 1개
		t.Fatalf("수확 실패: %v", err)
	}
	if p.Coins != 1 {
		t.Fatalf("금화 = %d, want 1", p.Coins)
	}
	if len(g.Discard) != 3 { // 4장 중 금화 1장은 게임에서 빠지고 3장은 버린다
		t.Fatalf("버린 더미 = %d장, want 3", len(g.Discard))
	}
	if p.Fields[1].Count != 0 || p.Fields[1].Bean != "" {
		t.Fatalf("수확한 밭 = %+v", p.Fields[1])
	}
	// 이제 1장짜리 밭도 팔 수 있다 (금화 0개)
	if err := g.Harvest(0, 0); err != nil {
		t.Fatalf("전부 1장인데 수확이 막혔다: %v", err)
	}
	if p.Coins != 1 || len(g.Discard) != 4 {
		t.Fatalf("금화 %d개 · 버린 더미 %d장", p.Coins, len(g.Discard))
	}
	// 빈 밭은 못 판다
	if err := g.Harvest(0, 0); err == nil {
		t.Fatal("빈 밭 수확이 통과했다")
	}
}

// TestBZBuyThirdField 세 번째 콩밭 — 금화 3개, 게임 중 1회, 외상 불가,
// 차례가 아니어도 살 수 있다
func TestBZBuyThirdField(t *testing.T) {
	g := bzFixture(t, 3)
	bzSetBoard(g, 0, []BZBean{BZGarden, BZGarden})
	buyer := g.Players[2] // 차례가 아닌 좌석

	buyer.Coins = 2
	if err := g.BuyField(2); err == nil {
		t.Fatal("금화 2개로 세 번째 밭을 샀다 (외상 불가)")
	} else if !hasHangul(err.Error()) {
		t.Fatalf("오류 문구가 한글이 아니다: %q", err.Error())
	}
	if len(buyer.Fields) != BZStartFields || buyer.Coins != 2 {
		t.Fatal("거부된 구매가 판을 바꿨다")
	}

	buyer.Coins = 5
	if err := g.BuyField(2); err != nil {
		t.Fatalf("세 번째 밭 구매 실패: %v", err)
	}
	if len(buyer.Fields) != BZMaxFields || buyer.Coins != 5-BZThirdFieldCost {
		t.Fatalf("구매 후 밭 %d개 · 금화 %d개", len(buyer.Fields), buyer.Coins)
	}
	if !buyer.BoughtField || g.SpentCoins != BZThirdFieldCost {
		t.Fatalf("구매 장부 = %v / %d", buyer.BoughtField, g.SpentCoins)
	}
	if buyer.Fields[2].Count != 0 {
		t.Fatalf("새 밭이 비어 있지 않다: %+v", buyer.Fields[2])
	}

	buyer.Coins = 9
	if err := g.BuyField(2); err == nil {
		t.Fatal("네 번째 밭을 샀다 (게임 중 1회여야 한다)")
	}
	if len(buyer.Fields) != BZMaxFields {
		t.Fatalf("밭 = %d개", len(buyer.Fields))
	}
}

// ==================== 거래 ====================

// TestBZTradeNeedsCurrentSeat **모든 거래에는 차례인 사람이 반드시 낀다** —
// 남들끼리는 거래하지 못한다. 공개 카드는 차례인 사람만 내줄 수 있다.
func TestBZTradeNeedsCurrentSeat(t *testing.T) {
	g := bzFixture(t, 4)
	bzSetBoard(g, 1, []BZBean{BZRed, BZBlue, BZGarden, BZGarden})
	for _, p := range g.Players {
		p.Hand = []BZBean{BZSoy, BZChili, BZStink}
	}
	// 2단계를 연다 (공개 2장)
	g.beginTradePhase()
	g.DrainEvents()
	if g.Phase != BZPhaseTrade || len(g.Flipped) != 2 {
		t.Fatalf("2단계 준비 실패: %s %v", g.Phase, g.Flipped)
	}

	cases := []struct {
		name    string
		seat    int
		payload BZOfferPayload
		wantErr string
	}{
		{"남들끼리는 거래 못 한다", 0,
			BZOfferPayload{ToSeat: 2, GiveHand: []int{0}, WantHand: []int{0}}, "차례"},
		{"남들끼리는 기부도 못 한다", 2,
			BZOfferPayload{ToSeat: 3, GiveHand: []int{0}}, "차례"},
		{"자기 자신과는 못 한다", 1,
			BZOfferPayload{ToSeat: 1, GiveHand: []int{0}}, "자기"},
		{"없는 좌석", 1,
			BZOfferPayload{ToSeat: 9, GiveHand: []int{0}}, "상대"},
		{"공개 카드는 차례인 사람만 내준다", 0,
			BZOfferPayload{ToSeat: 1, GiveFlipped: []int{0}}, "공개 카드"},
		{"빈 제안", 1,
			BZOfferPayload{ToSeat: 0}, "골라야"},
		{"없는 손패 번호", 1,
			BZOfferPayload{ToSeat: 0, GiveHand: []int{9}}, "카드 번호"},
		{"같은 카드 두 번", 1,
			BZOfferPayload{ToSeat: 0, GiveHand: []int{1, 1}}, "두 번"},
		{"없는 공개 카드", 1,
			BZOfferPayload{ToSeat: 0, GiveFlipped: []int{5}}, "카드 번호"},
		{"없는 요구 카드", 1,
			BZOfferPayload{ToSeat: 0, WantHand: []int{7}}, "카드 번호"},
	}
	for _, tc := range cases {
		offersBefore := len(g.Offers)
		_, err := g.Offer(tc.seat, tc.payload)
		if err == nil {
			t.Fatalf("%s: 통과했다", tc.name)
		}
		if !hasHangul(err.Error()) || !strings.Contains(err.Error(), tc.wantErr) {
			t.Fatalf("%s: 오류 문구 = %q (want %q 포함)", tc.name, err.Error(), tc.wantErr)
		}
		if len(g.Offers) != offersBefore {
			t.Fatalf("%s: 거부된 제안이 남았다", tc.name)
		}
	}

	// 차례인 사람이 끼면 양방향 모두 된다
	if _, err := g.Offer(1, BZOfferPayload{ToSeat: 0, GiveFlipped: []int{0}, WantHand: []int{0}}); err != nil {
		t.Fatalf("차례인 사람 → 남 제안 실패: %v", err)
	}
	if _, err := g.Offer(2, BZOfferPayload{ToSeat: 1, GiveHand: []int{0}}); err != nil {
		t.Fatalf("남 → 차례인 사람 제안 실패: %v", err)
	}
	if len(g.Offers) != 2 {
		t.Fatalf("제안 = %d개, want 2", len(g.Offers))
	}

	// 답은 받은 사람만 할 수 있다
	if err := g.Respond(3, g.Offers[0].ID, true); err == nil {
		t.Fatal("제3자가 제안에 답했다")
	}
	if err := g.Respond(0, "없는-제안", true); err == nil {
		t.Fatal("없는 제안에 답했다")
	}
	// 제안 id 는 와이어에 문자열로 나간다
	if g.Offers[0].ID == "" || g.Offers[0].ID == g.Offers[1].ID {
		t.Fatalf("제안 id = %q / %q", g.Offers[0].ID, g.Offers[1].ID)
	}
	// 거절하면 그 제안만 사라진다
	if err := g.Respond(0, g.Offers[0].ID, false); err != nil {
		t.Fatalf("거절 실패: %v", err)
	}
	if len(g.Offers) != 1 {
		t.Fatalf("거절 후 제안 = %d개, want 1", len(g.Offers))
	}
}

// TestBZTradeExecution 거래 성사 — 받은 카드는 **손에 못 들고** 심어야 할
// 카드로 들어가고, 남은 손패의 상대 순서는 그대로다
func TestBZTradeExecution(t *testing.T) {
	g := bzFixture(t, 3)
	bzSetBoard(g, 0, []BZBean{BZRed, BZBlue, BZGarden, BZGarden, BZGarden})
	cur, other := g.Players[0], g.Players[1]
	cur.Hand = []BZBean{BZSoy, BZChili, BZStink}
	other.Hand = []BZBean{BZGreen, BZBlackeyed, BZGarden}
	g.beginTradePhase() // 공개: 팥 · 푸르대콩
	g.DrainEvents()

	before := bzTotalCards(g)
	// 차례인 사람이 공개 카드 1장 + 손패 1장을 주고, 상대 맨 앞 카드를 요구
	id, err := g.Offer(0, BZOfferPayload{
		ToSeat: 1, GiveHand: []int{1}, GiveFlipped: []int{0}, WantHand: []int{0}})
	if err != nil {
		t.Fatalf("제안 실패: %v", err)
	}
	if err := g.Respond(1, id, true); err != nil {
		t.Fatalf("수락 실패: %v", err)
	}

	// 준 카드는 상대의 "심어야 할 카드"로 (손패로 가지 않는다)
	if !bzSameSeq(other.Pending, []BZBean{BZChili, BZRed}) {
		t.Fatalf("상대가 받은 카드 = %v, want [chili red]", other.Pending)
	}
	if !bzSameSeq(other.Hand, []BZBean{BZBlackeyed, BZGarden}) {
		t.Fatalf("상대 손패 = %v, want [blackeyed garden]", other.Hand)
	}
	// 요구한 카드도 내 손패가 아니라 심어야 할 카드로 온다
	if !bzSameSeq(cur.Pending, []BZBean{BZGreen}) {
		t.Fatalf("내가 받은 카드 = %v, want [green]", cur.Pending)
	}
	if !bzSameSeq(cur.Hand, []BZBean{BZSoy, BZStink}) {
		t.Fatalf("내 손패 = %v, want [soy stink] (상대 순서 유지)", cur.Hand)
	}
	if !bzSameSeq(g.Flipped, []BZBean{BZBlue}) {
		t.Fatalf("공개 카드 = %v, want [blue]", g.Flipped)
	}
	if len(g.Offers) != 0 {
		t.Fatalf("성사 후 남은 제안 = %d개 (인덱스가 밀려 전부 파기해야 한다)", len(g.Offers))
	}
	if bzTotalCards(g) != before {
		t.Fatalf("거래로 카드 총량이 바뀌었다: %d → %d", before, bzTotalCards(g))
	}

	// ---- 기부 (want 를 비운다) ----
	id2, err := g.Offer(0, BZOfferPayload{ToSeat: 2, GiveFlipped: []int{0}})
	if err != nil {
		t.Fatalf("기부 제안 실패: %v", err)
	}
	if err := g.Respond(2, id2, true); err != nil {
		t.Fatalf("기부 수락 실패: %v", err)
	}
	if !bzSameSeq(g.Players[2].Pending, []BZBean{BZBlue}) {
		t.Fatalf("기부받은 카드 = %v", g.Players[2].Pending)
	}
	if len(g.Flipped) != 0 {
		t.Fatalf("공개 카드 = %v, want []", g.Flipped)
	}

	// ---- 거래 마감 → 3단계 → 받은 사람이 각자 심는다 ----
	if err := g.EndPhase(1); err == nil {
		t.Fatal("차례가 아닌 사람이 거래를 마감했다")
	}
	if err := g.EndPhase(0); err != nil {
		t.Fatalf("거래 마감 실패: %v", err)
	}
	if g.Phase != BZPhasePlantReceived {
		t.Fatalf("phase = %s, want plant_received", g.Phase)
	}
	// 다른 밭 콩을 같은 밭에 심을 수는 없다
	g.Players[1].Fields[0] = BZField{Bean: BZSoy, Count: 1}
	if err := g.PlantReceived(1, 0, 0); err == nil {
		t.Fatal("다른 종류 콩을 같은 밭에 심었다")
	} else if !hasHangul(err.Error()) {
		t.Fatalf("오류 문구가 한글이 아니다: %q", err.Error())
	}
	// 받은 카드가 없는 좌석은 심을 것이 없다
	g.Players[1].Fields[0] = BZField{}
	for seat := 0; seat < 3; seat++ {
		for len(g.Players[seat].Pending) > 0 {
			if err := g.PlantReceived(seat, 0, 0); err != nil {
				if err2 := g.PlantReceived(seat, 0, 1); err2 != nil {
					t.Fatalf("seat%d 받은 카드 심기 실패: %v / %v", seat, err, err2)
				}
			}
		}
	}
	// 전원이 다 심으면 4단계가 돌아 차례가 넘어간다
	if g.CurrentSeat != 1 || g.Phase != BZPhasePlant {
		t.Fatalf("차례 이양 실패: seat=%d phase=%s", g.CurrentSeat, g.Phase)
	}
	if bzTotalCards(g) != before {
		t.Fatalf("한 차례로 카드 총량이 바뀌었다: %d → %d", before, bzTotalCards(g))
	}
}

// ==================== 덱 소진 / 종료 / 동점 ====================

// TestBZDeckCycleEnds 덱이 마르면 소진 횟수를 올리고 버린 더미를 섞어
// 되돌린다. **3인은 2번째, 4~5인은 3번째 소진에서 게임이 끝난다.**
func TestBZDeckCycleEnds(t *testing.T) {
	g := bzFixture(t, 3)
	g.Deck = []BZBean{}
	g.Discard = []BZBean{BZRed, BZBlue, BZSoy}
	g.DeckCycle = 0

	// 1번째 소진 — 되돌린다
	if _, ok := g.drawCard(); !ok {
		t.Fatal("1번째 소진에서 게임이 끝났다 (3인은 2번째여야 한다)")
	}
	if g.DeckCycle != 1 {
		t.Fatalf("소진 횟수 = %d, want 1", g.DeckCycle)
	}
	if len(g.Deck) != 2 || len(g.Discard) != 0 {
		t.Fatalf("되돌린 덱 = %d장 · 버린 더미 = %d장", len(g.Deck), len(g.Discard))
	}

	// 2번째 소진 — 끝난다 (되돌리지 않는다)
	g.Deck = []BZBean{}
	g.Discard = []BZBean{BZRed, BZBlue}
	if _, ok := g.drawCard(); ok {
		t.Fatal("3인 판이 2번째 소진에서 끝나지 않았다")
	}
	if g.DeckCycle != 2 || len(g.Deck) != 0 {
		t.Fatalf("소진 횟수 = %d · 덱 = %d장", g.DeckCycle, len(g.Deck))
	}

	// 4인은 3번째까지 간다
	g4 := bzFixture(t, 4)
	for cycle := 1; cycle <= 2; cycle++ {
		g4.Deck = []BZBean{}
		g4.Discard = []BZBean{BZRed, BZBlue, BZSoy}
		if _, ok := g4.drawCard(); !ok {
			t.Fatalf("4인 판이 %d번째 소진에서 끝났다", cycle)
		}
		if g4.DeckCycle != cycle {
			t.Fatalf("4인 소진 횟수 = %d, want %d", g4.DeckCycle, cycle)
		}
	}
	g4.Deck = []BZBean{}
	g4.Discard = []BZBean{BZRed}
	if _, ok := g4.drawCard(); ok {
		t.Fatal("4인 판이 3번째 소진에서 끝나지 않았다")
	}

	// 되돌릴 카드도 없으면 그 자리에서 끝난다
	g5 := bzFixture(t, 5)
	g5.Deck, g5.Discard, g5.DeckCycle = []BZBean{}, []BZBean{}, 0
	if _, ok := g5.drawCard(); ok {
		t.Fatal("덱·버린 더미가 모두 빈데 카드가 나왔다")
	}
}

// TestBZGameEndsOnDeckOut 자동 진행만으로 3인 판이 덱 소진으로 끝나고,
// 끝날 때 모든 밭이 정산되며 카드 총량 104장이 유지되는지
func TestBZGameEndsOnDeckOut(t *testing.T) {
	g := bzFixture(t, 3)
	for step := 0; step < 20000 && g.Phase != BZPhaseGameOver; step++ {
		switch g.Phase {
		case BZPhasePlant:
			g.ForcePlant()
		case BZPhaseTrade:
			g.ForceTradeEnd()
		case BZPhasePlantReceived:
			g.ForcePlantReceived()
		default:
			t.Fatalf("멈춘 단계: %s", g.Phase)
		}
		g.DrainEvents()
		if got := bzTotalCards(g); got != 104 {
			t.Fatalf("%d단계에서 카드 총량 = %d, want 104", step, got)
		}
	}
	if g.Phase != BZPhaseGameOver {
		t.Fatalf("자동 진행이 끝나지 않았다 (turns=%d)", g.Turns)
	}
	if g.DeckCycle != 2 {
		t.Fatalf("3인 판 종료 소진 횟수 = %d, want 2", g.DeckCycle)
	}
	if g.Turns >= BZMaxTurns {
		t.Fatalf("차례 상한에 걸렸다: %d", g.Turns)
	}
	if g.Result == nil || len(g.Result.Rows) != 3 || len(g.Result.WinnerSeats) == 0 {
		t.Fatalf("정산 결과 = %+v", g.Result)
	}
	if !hasHangul(g.Result.Message) {
		t.Fatalf("정산 문구가 한글이 아니다: %q", g.Result.Message)
	}
	for _, p := range g.Players {
		for _, f := range p.Fields {
			if f.Count != 0 {
				t.Fatalf("정산 후 밭이 남았다: seat%d %+v", p.Seat, p.Fields)
			}
		}
		if len(p.Pending) != 0 {
			t.Fatalf("정산 후 받은 카드가 남았다: seat%d %v", p.Seat, p.Pending)
		}
	}
	if bzTotalCards(g) != 104 {
		t.Fatalf("종료 후 카드 총량 = %d, want 104", bzTotalCards(g))
	}
}

// TestBZSettlementAndTieBreak 정산 — 금화가 가장 많은 사람이 승리하고,
// **동점이면 손에 든 카드가 많은 사람이 이긴다.**
func TestBZSettlementAndTieBreak(t *testing.T) {
	// ---- 밭이 전부 정산돼 금화에 더해진다 ----
	g := bzFixture(t, 3)
	bzSetBoard(g, 0, nil)
	g.Players[0].Coins = 2
	g.Players[0].Fields[0] = BZField{Bean: BZRed, Count: 4}    // 팥 4장 → 금화 3개
	g.Players[0].Fields[1] = BZField{Bean: BZGarden, Count: 2} // 강낭콩 2장 → 금화 2개
	g.Players[1].Coins = 1
	g.Players[1].Fields[0] = BZField{Bean: BZBlue, Count: 3} // 문턱 미달 → 0개
	g.Players[2].Coins = 6
	g.Players[0].Hand = []BZBean{BZSoy}
	g.Players[1].Hand = []BZBean{BZSoy, BZSoy, BZSoy}
	g.Players[2].Hand = []BZBean{}

	g.settle("검증")
	wantCoins := []int{7, 1, 6}
	for i, want := range wantCoins {
		if g.Players[i].Coins != want {
			t.Fatalf("seat%d 최종 금화 = %d, want %d", i, g.Players[i].Coins, want)
		}
	}
	if g.Result == nil || len(g.Result.WinnerSeats) != 1 || g.Result.WinnerSeats[0] != 0 {
		t.Fatalf("승자 = %+v", g.Result)
	}
	if g.Result.WinnerNames[0] != "P0" {
		t.Fatalf("승자 이름 = %v", g.Result.WinnerNames)
	}
	// rows 는 금화 내림차순, 각 행에 seat·coins·handCount
	if g.Result.Rows[0].Seat != 0 || g.Result.Rows[1].Seat != 2 || g.Result.Rows[2].Seat != 1 {
		t.Fatalf("정산 표 = %+v", g.Result.Rows)
	}
	if g.Result.Rows[2].HandCount != 3 {
		t.Fatalf("손패 수가 정산 표에 없다: %+v", g.Result.Rows[2])
	}
	if g.Phase != BZPhaseGameOver {
		t.Fatalf("정산 후 phase = %s", g.Phase)
	}

	// ---- 동점이면 손에 든 카드가 많은 사람이 이긴다 ----
	g2 := bzFixture(t, 3)
	bzSetBoard(g2, 0, nil)
	for _, p := range g2.Players {
		p.Coins = 5
	}
	g2.Players[0].Hand = []BZBean{BZSoy}
	g2.Players[1].Hand = []BZBean{BZSoy, BZSoy, BZSoy, BZSoy}
	g2.Players[2].Hand = []BZBean{BZSoy, BZSoy}
	g2.settle("동점")
	if len(g2.Result.WinnerSeats) != 1 || g2.Result.WinnerSeats[0] != 1 {
		t.Fatalf("동점 승자 = %+v (손에 든 카드가 많은 seat1 이어야 한다)", g2.Result)
	}
	if g2.Result.Rows[0].Seat != 1 || g2.Result.Rows[1].Seat != 2 || g2.Result.Rows[2].Seat != 0 {
		t.Fatalf("동점 정산 표 = %+v", g2.Result.Rows)
	}

	// ---- 금화·손패까지 같으면 공동 승 ----
	g3 := bzFixture(t, 3)
	bzSetBoard(g3, 0, nil)
	for _, p := range g3.Players {
		p.Coins = 4
		p.Hand = []BZBean{BZSoy, BZSoy}
	}
	g3.settle("완전 동점")
	if len(g3.Result.WinnerSeats) != 3 {
		t.Fatalf("완전 동점인데 공동 승이 아니다: %+v", g3.Result)
	}
	if !strings.Contains(g3.Result.Message, "공동") {
		t.Fatalf("공동 승 문구 = %q", g3.Result.Message)
	}
}

// TestBZAutoPlacement AFK 자동 배치 — 맞는 밭 우선, 없으면 가장 적게 쌓인
// 밭을 수확해 자리를 만든다
func TestBZAutoPlacement(t *testing.T) {
	// 맞는 밭이 있으면 거기에
	g := bzFixture(t, 3)
	bzSetBoard(g, 0, []BZBean{BZGarden, BZGarden})
	p := g.Players[0]
	p.Fields[0] = BZField{Bean: BZRed, Count: 2}
	p.Fields[1] = BZField{Bean: BZBlue, Count: 5}
	g.autoPlantOne(0, BZRed)
	if p.Fields[0].Count != 3 || p.Fields[1].Count != 5 {
		t.Fatalf("맞는 밭에 안 심었다: %+v", p.Fields)
	}

	// 자리가 없으면 가장 적게 쌓인(수확 가능한) 밭을 판다
	g.autoPlantOne(0, BZSoy)
	if p.Fields[0].Bean != BZSoy || p.Fields[0].Count != 1 {
		t.Fatalf("자리를 만들어 심지 못했다: %+v", p.Fields)
	}
	if p.Coins != bzCoins(BZRed, 3) {
		t.Fatalf("수확 금화 = %d, want %d", p.Coins, bzCoins(BZRed, 3))
	}

	// 가장 적게 쌓인 밭 고르기 표
	cases := []struct {
		fields []BZField
		want   int
	}{
		{[]BZField{{BZRed, 3}, {BZBlue, 5}}, 0},
		{[]BZField{{BZRed, 6}, {BZBlue, 2}}, 1},
		{[]BZField{{BZRed, 1}, {BZBlue, 4}}, 1}, // 1장 밭은 못 판다
		{[]BZField{{BZRed, 1}, {BZBlue, 1}}, 0}, // 전부 1장이면 앞에서부터
		{[]BZField{{"", 0}, {"", 0}}, -1},
	}
	for _, tc := range cases {
		if got := bzSmallestHarvestable(tc.fields); got != tc.want {
			t.Fatalf("bzSmallestHarvestable(%v) = %d, want %d", tc.fields, got, tc.want)
		}
	}

	// ForcePlantReceived 는 전원분 받은 카드를 한 번에 배치한다
	g2 := bzFixture(t, 3)
	bzSetBoard(g2, 0, []BZBean{BZGarden, BZGarden, BZGarden, BZGarden, BZGarden})
	g2.Phase = BZPhasePlantReceived
	g2.Players[0].Pending = []BZBean{BZRed, BZRed}
	g2.Players[2].Pending = []BZBean{BZBlue}
	before := bzTotalCards(g2)
	g2.ForcePlantReceived()
	if g2.bzPendingTotal() != 0 {
		t.Fatal("자동 배치 후에도 받은 카드가 남았다")
	}
	if g2.Players[0].Fields[0].Count != 2 || g2.Players[2].Fields[0].Bean != BZBlue {
		t.Fatalf("자동 배치 결과 = %+v / %+v", g2.Players[0].Fields, g2.Players[2].Fields)
	}
	if bzTotalCards(g2) != before {
		t.Fatalf("자동 배치로 카드 총량이 바뀌었다: %d → %d", before, bzTotalCards(g2))
	}

	// ForcePlant 는 자리가 없어도 맨 앞 카드를 심는다
	g3 := bzFixture(t, 3)
	bzSetBoard(g3, 0, []BZBean{BZGarden, BZGarden, BZGarden, BZGarden, BZGarden})
	q := g3.Players[0]
	q.Fields[0] = BZField{Bean: BZBlue, Count: 4}
	q.Fields[1] = BZField{Bean: BZChili, Count: 3}
	q.Hand = []BZBean{BZRed, BZSoy}
	g3.ForcePlant()
	if len(q.Hand) != 1 || q.Hand[0] != BZSoy {
		t.Fatalf("자동 심기 후 손패 = %v", q.Hand)
	}
	if q.Coins == 0 {
		t.Fatal("자리를 만들려고 수확하지 않았다")
	}
	if g3.Phase != BZPhaseTrade {
		t.Fatalf("자동 심기 후 phase = %s", g3.Phase)
	}
}

// ==================== 봇 대전 ====================

// bzBotFixture 허브 고루틴 없이 결정적으로 돌리는 n인 방
func bzBotFixture(t *testing.T, n int, seed int64) (*BZHub, *bzRoom, []*BZClient) {
	t.Helper()
	h := NewBZHub()
	h.rng = rand.New(rand.NewSource(seed))
	room := h.lobbyRoomFor("")
	clients := make([]*BZClient, n)
	for i := range clients {
		c := &BZClient{wsClient: newBotWSClient(), Hub: h}
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
	h.stopPhaseTimer(room) // 타이머 없이 우리가 직접 판을 민다
	return h, room, clients
}

// bzDrain 봇 채널에 쌓인 메시지를 버린다 (버퍼 포화로 연결이 끊기지 않게)
func bzDrain(clients []*BZClient) {
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

// bzSnapshotFor 좌석 스냅샷을 실제 와이어와 같은 모양(JSON 왕복)으로 만든다
func bzSnapshotFor(t *testing.T, h *BZHub, room *bzRoom, seat int) interface{} {
	t.Helper()
	raw, err := json.Marshal(h.buildBZState(room, seat))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var payload interface{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return payload
}

// bzRunBotGame n봇 한 판을 끝까지 돌리고 (차례 수, 좌석별 최종 금화, 승자
// 좌석)을 돌려준다. 스냅샷 → 두뇌 → 허브 핸들러 경로가 실제 WS 경로와 같다.
func bzRunBotGame(t *testing.T, n int, seed int64) (turns int, coins []int, winners []int) {
	t.Helper()
	h, room, clients := bzBotFixture(t, n, seed)
	game := room.Game
	brains := make([]*bzBrain, n)
	for i := range brains {
		brains[i] = &bzBrain{
			rng:   rand.New(rand.NewSource(seed*1000 + int64(i))),
			tried: map[string]bool{},
		}
	}

	for step := 0; step < 40000 && game.Phase != BZPhaseGameOver; step++ {
		acted := false
		for seat := 0; seat < n && !acted; seat++ {
			reply := brains[seat].decide(BZMessage{
				Type: BZMsgGameState, Payload: bzSnapshotFor(t, h, room, seat)})
			if reply == nil {
				continue
			}
			h.handleGameMessage(BZGameMessage{Client: clients[seat], Message: *reply})
			h.stopPhaseTimer(room)
			acted = true
		}
		if !acted { // 아무도 못 움직이면 규칙의 자동 진행으로 민다
			switch game.Phase {
			case BZPhasePlant:
				game.ForcePlant()
			case BZPhaseTrade:
				game.ForceTradeEnd()
			case BZPhasePlantReceived:
				game.ForcePlantReceived()
			default:
				t.Fatalf("seed %d: 멈춘 단계 %s", seed, game.Phase)
			}
			game.DrainEvents()
			h.stopPhaseTimer(room)
		}
		if got := bzTotalCards(game); got != 104 {
			t.Fatalf("seed %d: 카드 총량이 %d 로 어긋났다", seed, got)
		}
		bzDrain(clients)
	}
	if game.Phase != BZPhaseGameOver {
		t.Fatalf("seed %d: %d차례에도 끝나지 않았다", seed, game.Turns)
	}

	turns = game.Turns
	for _, p := range game.Players {
		coins = append(coins, p.Coins)
	}
	if game.Result != nil {
		winners = append([]int{}, game.Result.WinnerSeats...)
	}
	return turns, coins, winners
}

// TestBZBotQuality 3봇 30판의 평균 소요 차례와 최종 금화 분포를 숫자로 남긴다.
// 덱이 104장이라 반드시 끝나야 하고, 평균 금화가 5개 미만이면 심기·수확
// 판단이 나쁜 것이다.
func TestBZBotQuality(t *testing.T) {
	const games = 30
	const seats = BZFillBotTarget

	totalTurns, minTurns, maxTurns := 0, 1<<30, 0
	wins := make([]int, seats)
	winCoins := []int{}
	allCoins := []int{}
	capHit := 0

	for i := 0; i < games; i++ {
		turns, coins, winners := bzRunBotGame(t, seats, int64(8600+i))
		totalTurns += turns
		if turns < minTurns {
			minTurns = turns
		}
		if turns > maxTurns {
			maxTurns = turns
		}
		if turns >= BZMaxTurns {
			capHit++
		}
		for _, s := range winners {
			wins[s]++
		}
		best := 0
		for _, c := range coins {
			allCoins = append(allCoins, c)
			if c > best {
				best = c
			}
		}
		winCoins = append(winCoins, best)
	}

	avgTurns := float64(totalTurns) / games
	sort.Ints(winCoins)
	sort.Ints(allCoins)
	sum := 0
	for _, c := range allCoins {
		sum += c
	}
	avgCoins := float64(sum) / float64(len(allCoins))

	t.Logf("봇 품질 %d판(%d인): 평균 소요 차례 %.1f (최소 %d · 최대 %d · 차례 상한 도달 %d판)",
		games, seats, avgTurns, minTurns, maxTurns, capHit)
	t.Logf("  승자 최종 금화: 최소 %d · 중앙 %d · 최대 %d",
		winCoins[0], winCoins[len(winCoins)/2], winCoins[len(winCoins)-1])
	t.Logf("  전체 최종 금화: 최소 %d · 중앙 %d · 최대 %d · 평균 %.2f",
		allCoins[0], allCoins[len(allCoins)/2], allCoins[len(allCoins)-1], avgCoins)
	t.Logf("  좌석별 승수: %v (총 %d판)", wins, games)

	if capHit > 0 {
		t.Fatalf("차례 상한(%d)에 걸린 판이 %d개 — 덱 소진으로 끝나야 한다", BZMaxTurns, capHit)
	}
	if avgCoins < 5 {
		t.Fatalf("평균 최종 금화 %.2f개 — 5개 미만이면 심기·수확 판단이 나쁜 것이다", avgCoins)
	}
	for seat, w := range wins {
		if w == games {
			t.Fatalf("seat%d 가 %d판을 모두 이겼다 — 선 이점이 굳어 있다", seat, games)
		}
	}
}
