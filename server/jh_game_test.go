package server

import (
	"fmt"
	"math/rand"
	"testing"
)

// ==================== 헬퍼 ====================

func jhEv(suit JHSuit, v int) JHCard { return JHCard{ID: -1, Suit: suit, Value: v} }
func jhPo(v int) JHCard              { return JHCard{ID: -1, Suit: JHPotion, Value: v} }

// jhAssignIDs 테스트 손패에 서로 다른 ID 부여
func jhAssignIDs(hands ...[]JHCard) {
	id := 100
	for _, hand := range hands {
		for i := range hand {
			hand[i].ID = id
			id++
		}
	}
}

// newTestJHGame 지정한 손패로 트릭테이킹 단계부터 시작하는 게임
func newTestJHGame(t *testing.T, jekyll, hyde []JHCard, leader JHRole) *JHGame {
	t.Helper()
	jhAssignIDs(jekyll, hyde)
	g := NewJHGame("test")
	g.Names[JHJekyll] = "J"
	g.Names[JHHyde] = "H"
	g.rng = rand.New(rand.NewSource(1))
	g.Ready = true
	g.Round = 1
	g.Hands[JHJekyll] = append([]JHCard{}, jekyll...)
	g.Hands[JHHyde] = append([]JHCard{}, hyde...)
	g.Tricks[JHJekyll] = []JHTrick{}
	g.Tricks[JHHyde] = []JHTrick{}
	g.Leader = leader
	g.Phase = JHPhaseLead
	return g
}

// jhIdx role 손패에서 (suit, value) 카드의 인덱스
func jhIdx(t *testing.T, g *JHGame, role JHRole, suit JHSuit, value int) int {
	t.Helper()
	for i, c := range g.Hands[role] {
		if c.Suit == suit && c.Value == value {
			return i
		}
	}
	t.Fatalf("%s 손패에 %s %d 가 없음", role, suit, value)
	return -1
}

// jhPlay (suit, value) 카드를 낸다
func jhPlay(t *testing.T, g *JHGame, role JHRole, suit JHSuit, value int) {
	t.Helper()
	if err := g.PlayCard(role, jhIdx(t, g, role, suit, value)); err != nil {
		t.Fatalf("%s 가 %s %d 를 내지 못함: %v", role, suit, value, err)
	}
}

// ==================== 덱 구성 ====================

func TestJHDeckComposition(t *testing.T) {
	deck := jhNewDeck()
	if len(deck) != 25 {
		t.Fatalf("덱은 25장이어야 함, got %d", len(deck))
	}
	suitCount := map[JHSuit]int{}
	ids := map[int]bool{}
	potionValues := map[int]bool{}
	for _, c := range deck {
		suitCount[c.Suit]++
		if ids[c.ID] {
			t.Fatalf("ID 중복: %d", c.ID)
		}
		ids[c.ID] = true
		if c.IsPotion() {
			potionValues[c.Value] = true
		} else if c.Value < 1 || c.Value > JHEvilMaxValue {
			t.Fatalf("악 카드 숫자 범위 밖: %+v", c)
		}
	}
	for _, s := range jhEvilSuits {
		if suitCount[s] != 7 {
			t.Fatalf("%s 는 7장이어야 함, got %d", s, suitCount[s])
		}
	}
	if suitCount[JHPotion] != 4 {
		t.Fatalf("물약은 4장이어야 함, got %d", suitCount[JHPotion])
	}
	for v := JHPotionMinValue; v <= JHPotionMaxValue; v++ {
		if !potionValues[v] {
			t.Fatalf("물약 %d+ 가 없음", v)
		}
	}
}

func TestJHStartDeals(t *testing.T) {
	g := NewJHGame("test")
	g.AddPlayer("J")
	g.AddPlayer("H")
	if err := g.Start(rand.New(rand.NewSource(7))); err != nil {
		t.Fatal(err)
	}
	if g.Phase != JHPhaseExchange {
		t.Fatalf("시작 직후는 교환 단계여야 함, got %s", g.Phase)
	}
	if g.Round != 1 || g.Leader != JHJekyll {
		t.Fatalf("1라운드 선은 지킬이어야 함 (round=%d leader=%s)", g.Round, g.Leader)
	}
	if len(g.Hands[JHJekyll]) != 10 || len(g.Hands[JHHyde]) != 10 {
		t.Fatalf("손패는 10장씩이어야 함")
	}
	// 두 손패의 카드 ID 는 겹치지 않아야 한다 (5장 비공개 제외)
	ids := map[int]bool{}
	for _, c := range append(append([]JHCard{}, g.Hands[JHJekyll]...), g.Hands[JHHyde]...) {
		if ids[c.ID] {
			t.Fatalf("배분 카드 중복: %+v", c)
		}
		ids[c.ID] = true
	}
}

// ==================== 교환 ====================

func TestJHExchangeValidation(t *testing.T) {
	// 지킬 손에 물약 2장 → 물약 없이 교환 제출하면 거부
	jekyll := []JHCard{jhPo(2), jhPo(3), jhEv(JHPride, 1), jhEv(JHPride, 2), jhEv(JHPride, 3),
		jhEv(JHWrath, 1), jhEv(JHWrath, 2), jhEv(JHWrath, 3), jhEv(JHGreed, 1), jhEv(JHGreed, 2)}
	hyde := []JHCard{jhEv(JHPride, 4), jhEv(JHPride, 5), jhEv(JHPride, 6), jhEv(JHPride, 7), jhEv(JHWrath, 4),
		jhEv(JHWrath, 5), jhEv(JHWrath, 6), jhEv(JHWrath, 7), jhEv(JHGreed, 3), jhEv(JHGreed, 4)}
	g := newTestJHGame(t, jekyll, hyde, JHJekyll)
	g.Phase = JHPhaseExchange
	g.ExchangeSel = map[JHRole][]int{}
	g.Round = 2 // 2장 교환

	if !g.MustIncludePotion(JHJekyll) {
		t.Fatal("지킬은 물약 강제 교환 대상이어야 함")
	}
	if g.MustIncludePotion(JHHyde) {
		t.Fatal("하이드는 물약이 없으니 강제 대상이 아니어야 함")
	}
	if err := g.SubmitExchange(JHJekyll, []int{2, 3}); err == nil {
		t.Fatal("물약 없는 교환이 거부되지 않음")
	}
	if err := g.SubmitExchange(JHJekyll, []int{0}); err == nil {
		t.Fatal("장수가 다른 교환이 거부되지 않음")
	}
	if err := g.SubmitExchange(JHJekyll, []int{0, 0}); err == nil {
		t.Fatal("중복 인덱스 교환이 거부되지 않음")
	}
	if err := g.SubmitExchange(JHJekyll, []int{0, 2}); err != nil {
		t.Fatalf("정상 교환이 거부됨: %v", err)
	}
	if err := g.SubmitExchange(JHJekyll, []int{1, 3}); err == nil {
		t.Fatal("중복 제출이 거부되지 않음")
	}
	// 상대 제출 전에는 손패 그대로
	if len(g.Hands[JHJekyll]) != 10 || g.Phase != JHPhaseExchange {
		t.Fatal("한쪽만 제출한 상태에서 교환이 실행됨")
	}

	if err := g.SubmitExchange(JHHyde, []int{0, 1}); err != nil {
		t.Fatalf("하이드 교환이 거부됨: %v", err)
	}
	if g.Phase != JHPhaseLead {
		t.Fatalf("양쪽 제출 후 리드 단계여야 함, got %s", g.Phase)
	}
	// 지킬이 넘긴 물약 2 와 오만 1 이 하이드 손에 있어야 한다
	found := 0
	for _, c := range g.Hands[JHHyde] {
		if (c.Suit == JHPotion && c.Value == 2) || (c.Suit == JHPride && c.Value == 1) {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("교환된 카드가 하이드 손에 없음 (found=%d)", found)
	}
	if len(g.Hands[JHJekyll]) != 10 || len(g.Hands[JHHyde]) != 10 {
		t.Fatal("교환 후에도 손패는 10장씩이어야 함")
	}
}

// ==================== 팔로우 합법성 ====================

func TestJHFollowLegality(t *testing.T) {
	jekyll := []JHCard{jhEv(JHPride, 5), jhEv(JHPride, 6), jhEv(JHWrath, 3)}
	hyde := []JHCard{jhEv(JHPride, 2), jhEv(JHWrath, 7), jhPo(3)}
	g := newTestJHGame(t, jekyll, hyde, JHJekyll)

	// 색 카드 리드: 같은 색 강제 + 물약 대체 허용
	jhPlay(t, g, JHJekyll, JHPride, 5)
	legal := g.LegalPlays(JHHyde)
	// 하이드 손: 오만2(0), 분노7(1), 물약3(2) → 오만과 물약만 합법
	if len(legal) != 2 || legal[0] != 0 || legal[1] != 2 {
		t.Fatalf("색 리드 팔로우 합법 수가 틀림: %v", legal)
	}
	if err := g.PlayCard(JHHyde, 1); err == nil {
		t.Fatal("팔로우 강제 위반이 거부되지 않음")
	}
	jhPlay(t, g, JHHyde, JHPotion, 3)
	// 물약 3+ > 오만 5? 아니다: 6 > 7 거짓 → 리더 승
	if g.TrickWinner != JHJekyll {
		t.Fatalf("오만5 vs 물약3+ 는 지킬 승이어야 함, got %s", g.TrickWinner)
	}

	// 물약 리드 + 색 선언: 선언 색 보유 시 물약 회피 불가
	jekyll2 := []JHCard{jhPo(4), jhEv(JHGreed, 1)}
	hyde2 := []JHCard{jhEv(JHWrath, 2), jhPo(5), jhEv(JHGreed, 7)}
	g2 := newTestJHGame(t, jekyll2, hyde2, JHJekyll)
	jhPlay(t, g2, JHJekyll, JHPotion, 4)
	if g2.Phase != JHPhaseDeclare {
		t.Fatalf("물약 리드 후 선언 단계여야 함, got %s", g2.Phase)
	}
	if err := g2.DeclareSuit(JHHyde, JHWrath); err == nil {
		t.Fatal("상대의 색 선언이 거부되지 않음")
	}
	if err := g2.DeclareSuit(JHJekyll, JHPotion); err == nil {
		t.Fatal("물약 색 선언이 거부되지 않음")
	}
	if err := g2.DeclareSuit(JHJekyll, JHWrath); err != nil {
		t.Fatal(err)
	}
	legal2 := g2.LegalPlays(JHHyde)
	// 하이드 손: 분노2(0), 물약5(1), 탐욕7(2) → 분노만 합법 (물약 회피 불가)
	if len(legal2) != 1 || legal2[0] != 0 {
		t.Fatalf("물약 리드 선언 색 강제가 틀림: %v", legal2)
	}

	// 선언 색이 없으면 아무 카드나 (더블 물약 포함)
	jekyll3 := []JHCard{jhPo(4), jhEv(JHGreed, 1)}
	hyde3 := []JHCard{jhPo(5), jhEv(JHGreed, 7)}
	g3 := newTestJHGame(t, jekyll3, hyde3, JHJekyll)
	jhPlay(t, g3, JHJekyll, JHPotion, 4)
	g3.DeclareSuit(JHJekyll, JHWrath)
	legal3 := g3.LegalPlays(JHHyde)
	if len(legal3) != 2 {
		t.Fatalf("선언 색이 없으면 전부 합법이어야 함: %v", legal3)
	}
}

// ==================== 트릭 판정 ====================

func TestJHTrickResolutionTable(t *testing.T) {
	cases := []struct {
		name       string
		lead       JHCard
		follow     JHCard
		hydeExtra  JHCard   // 팔로우 강제에 걸리지 않도록 케이스별로 고른 여분 카드
		rankBefore []JHSuit // 트릭 전 이미 등장한 수트 (등장 순)
		leaderWins bool
	}{
		{"같은 색 높은 숫자 승", jhEv(JHPride, 5), jhEv(JHPride, 3), jhEv(JHGreed, 6), nil, true},
		{"같은 색 낮은 숫자 패", jhEv(JHPride, 3), jhEv(JHPride, 5), jhEv(JHGreed, 6), nil, false},
		{"다른 색 첫 등장 순서: 나중 색이 강함", jhEv(JHPride, 7), jhEv(JHWrath, 1), jhEv(JHGreed, 6), nil, false},
		{"기존 랭크 우위", jhEv(JHWrath, 1), jhEv(JHPride, 7), jhEv(JHGreed, 6), []JHSuit{JHPride, JHWrath}, true},
		{"물약 vs 악: 숫자만 비교 (2+ > 2)", jhPo(2), jhEv(JHGreed, 2), jhEv(JHGreed, 6), []JHSuit{JHGreed}, true},
		{"물약 vs 악: 숫자만 비교 (2+ < 3)", jhPo(2), jhEv(JHGreed, 3), jhEv(JHGreed, 6), []JHSuit{JHGreed}, false},
		{"악 vs 물약: 랭크 무시", jhEv(JHGreed, 7), jhPo(5), jhEv(JHGreed, 6), []JHSuit{JHGreed}, true},
		{"더블 물약: 높은 숫자 승", jhPo(5), jhPo(4), jhEv(JHWrath, 4), nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 손패에 여분 카드를 줘서 라운드 정산이 일어나지 않게 한다
			jekyll := []JHCard{tc.lead, jhEv(JHPride, 1)}
			hyde := []JHCard{tc.follow, tc.hydeExtra}
			g := newTestJHGame(t, jekyll, hyde, JHJekyll)
			g.RankOrder = append([]JHSuit{}, tc.rankBefore...)

			if err := g.PlayCard(JHJekyll, 0); err != nil {
				t.Fatal(err)
			}
			if g.Phase == JHPhaseDeclare {
				if err := g.DeclareSuit(JHJekyll, JHGreed); err != nil {
					t.Fatal(err)
				}
			}
			if err := g.PlayCard(JHHyde, 0); err != nil {
				t.Fatal(err)
			}

			want := JHHyde
			if tc.leaderWins {
				want = JHJekyll
			}
			if g.TrickWinner != want {
				t.Fatalf("승자가 틀림: got %s want %s", g.TrickWinner, want)
			}
			// 다음 트릭의 선은 승자
			if g.Phase == JHPhaseLead && g.Leader != want {
				t.Fatalf("다음 선이 승자가 아님: %s", g.Leader)
			}
		})
	}
}

func TestJHRankRegistration(t *testing.T) {
	jekyll := []JHCard{jhEv(JHPride, 1), jhEv(JHGreed, 1), jhEv(JHPride, 2)}
	hyde := []JHCard{jhEv(JHWrath, 1), jhEv(JHGreed, 2), jhEv(JHWrath, 2)}
	g := newTestJHGame(t, jekyll, hyde, JHJekyll)

	// 오만 리드 → 오만이 최하위, 분노 팔로우 → 분노가 중간
	jhPlay(t, g, JHJekyll, JHPride, 1)
	jhPlay(t, g, JHHyde, JHWrath, 1)
	if len(g.RankOrder) != 2 || g.RankOrder[0] != JHPride || g.RankOrder[1] != JHWrath {
		t.Fatalf("랭크 등록 순서가 틀림: %v", g.RankOrder)
	}
	// 분노(나중 등장)가 이겼어야 한다
	if g.TrickWinner != JHHyde {
		t.Fatal("나중 등장 색이 이겨야 함")
	}

	// 탐욕이 등장하면 자동으로 최강
	jhPlay(t, g, JHHyde, JHGreed, 2)
	jhPlay(t, g, JHJekyll, JHGreed, 1)
	if len(g.RankOrder) != 3 || g.RankOrder[2] != JHGreed {
		t.Fatalf("세 번째 색 등록이 틀림: %v", g.RankOrder)
	}

	// 이제 탐욕 > 분노 > 오만: 오만2 리드 vs 분노2 → 분노 승
	jhPlay(t, g, JHHyde, JHWrath, 2)
	jhPlay(t, g, JHJekyll, JHPride, 2)
	if g.TrickWinner != JHHyde {
		t.Fatal("랭크 상 분노가 오만을 이겨야 함")
	}
}

// 룰북 p.7 예시: 물약 4+ 리드 + 분노 선언, 하이드가 분노 6 →
// 랭크 리셋이 발동하고 트릭은 6 > 4+ 로 하이드 승.
func TestJHWrathResetRulebookExample(t *testing.T) {
	jekyll := []JHCard{jhPo(4), jhEv(JHPride, 3), jhEv(JHGreed, 5)}
	hyde := []JHCard{jhEv(JHWrath, 6), jhEv(JHPride, 4), jhEv(JHGreed, 2)}
	g := newTestJHGame(t, jekyll, hyde, JHJekyll)
	g.RankOrder = []JHSuit{JHWrath, JHPride} // 이미 두 색 등장한 상황

	jhPlay(t, g, JHJekyll, JHPotion, 4)
	if err := g.DeclareSuit(JHJekyll, JHWrath); err != nil {
		t.Fatal(err)
	}
	jhPlay(t, g, JHHyde, JHWrath, 6)

	if g.TrickWinner != JHHyde {
		t.Fatalf("6 > 4+ 로 하이드 승이어야 함, got %s", g.TrickWinner)
	}
	// 랭크는 리셋되고, 이번 트릭의 분노도 재등록되지 않는다
	if len(g.RankOrder) != 0 {
		t.Fatalf("분노 효과 후 랭크가 비어야 함: %v", g.RankOrder)
	}

	// 다음 트릭에 처음 나오는 색부터 다시 최하위
	jhPlay(t, g, JHHyde, JHPride, 4)
	if len(g.RankOrder) != 1 || g.RankOrder[0] != JHPride {
		t.Fatalf("리셋 후 첫 색이 최하위로 등록돼야 함: %v", g.RankOrder)
	}
}

// ==================== 오만 (트릭 강탈) ====================

func TestJHPrideSteal(t *testing.T) {
	jekyll := []JHCard{jhPo(5), jhEv(JHGreed, 1)}
	hyde := []JHCard{jhEv(JHPride, 3), jhEv(JHGreed, 2)}
	g := newTestJHGame(t, jekyll, hyde, JHJekyll)
	// 하이드가 이미 딴 트릭 2개
	g.Tricks[JHHyde] = []JHTrick{
		{Lead: jhEv(JHWrath, 1), Follow: jhEv(JHWrath, 2)},
		{Lead: jhEv(JHWrath, 3), Follow: jhEv(JHWrath, 4)},
	}

	jhPlay(t, g, JHJekyll, JHPotion, 5)
	g.DeclareSuit(JHJekyll, JHPride)
	jhPlay(t, g, JHHyde, JHPride, 3)

	// 물약 5+ > 오만 3 → 지킬 승, 오만 효과로 강탈 단계
	if g.TrickWinner != JHJekyll || g.Phase != JHPhasePrideSteal {
		t.Fatalf("지킬 승 + 강탈 단계여야 함 (winner=%s phase=%s)", g.TrickWinner, g.Phase)
	}
	if err := g.StealTrick(JHHyde, 0); err == nil {
		t.Fatal("패자의 강탈이 거부되지 않음")
	}
	if err := g.StealTrick(JHJekyll, 5); err == nil {
		t.Fatal("범위 밖 트릭 강탈이 거부되지 않음")
	}
	if err := g.StealTrick(JHJekyll, 1); err != nil {
		t.Fatal(err)
	}
	// 지킬: 현재 트릭 1 + 강탈 1 = 2, 하이드: 1
	if len(g.Tricks[JHJekyll]) != 2 || len(g.Tricks[JHHyde]) != 1 {
		t.Fatalf("강탈 후 트릭 수가 틀림: J=%d H=%d", len(g.Tricks[JHJekyll]), len(g.Tricks[JHHyde]))
	}
	if g.Phase != JHPhaseLead || g.Leader != JHJekyll {
		t.Fatalf("강탈 후 승자가 선으로 이어가야 함 (phase=%s leader=%s)", g.Phase, g.Leader)
	}
}

func TestJHPrideStealNoTargets(t *testing.T) {
	// 패자가 딴 트릭이 없으면 오만 효과 불발
	jekyll := []JHCard{jhPo(5), jhEv(JHGreed, 1)}
	hyde := []JHCard{jhEv(JHPride, 3), jhEv(JHGreed, 2)}
	g := newTestJHGame(t, jekyll, hyde, JHJekyll)

	jhPlay(t, g, JHJekyll, JHPotion, 5)
	g.DeclareSuit(JHJekyll, JHPride)
	jhPlay(t, g, JHHyde, JHPride, 3)

	if g.Phase != JHPhaseLead {
		t.Fatalf("강탈 대상이 없으면 바로 다음 트릭이어야 함, got %s", g.Phase)
	}
}

// ==================== 탐욕 (손패 교환) ====================

func TestJHGreedExchange(t *testing.T) {
	jekyll := []JHCard{jhPo(5), jhEv(JHPride, 1), jhEv(JHPride, 2), jhEv(JHPride, 3)}
	hyde := []JHCard{jhEv(JHGreed, 4), jhEv(JHWrath, 5), jhEv(JHWrath, 6), jhEv(JHWrath, 7)}
	g := newTestJHGame(t, jekyll, hyde, JHJekyll)

	jhPlay(t, g, JHJekyll, JHPotion, 5)
	g.DeclareSuit(JHJekyll, JHGreed)
	jhPlay(t, g, JHHyde, JHGreed, 4)

	if g.Phase != JHPhaseGreedExchange || g.GreedPickCount() != 2 {
		t.Fatalf("탐욕 교환 단계(2장)여야 함 (phase=%s pick=%d)", g.Phase, g.GreedPickCount())
	}
	if err := g.SubmitGreed(JHJekyll, []int{0}); err == nil {
		t.Fatal("장수가 다른 탐욕 제출이 거부되지 않음")
	}
	if err := g.SubmitGreed(JHJekyll, []int{0, 1}); err != nil {
		t.Fatal(err)
	}
	if g.Phase != JHPhaseGreedExchange {
		t.Fatal("한쪽 제출만으로 교환이 실행됨")
	}
	if err := g.SubmitGreed(JHHyde, []int{1, 2}); err != nil {
		t.Fatal(err)
	}
	// 교환 후: 지킬은 하이드가 고른 분노6·분노7 을, 하이드는 지킬이 고른
	// 오만1·오만2 를 받는다
	if g.Phase != JHPhaseLead {
		t.Fatalf("탐욕 교환 후 다음 트릭이어야 함, got %s", g.Phase)
	}
	count := 0
	for _, c := range g.Hands[JHJekyll] {
		if c.Suit == JHWrath && (c.Value == 6 || c.Value == 7) {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("탐욕 교환 결과가 틀림 (지킬 손: %v)", g.Hands[JHJekyll])
	}
	if len(g.Hands[JHJekyll]) != 3 || len(g.Hands[JHHyde]) != 3 {
		t.Fatal("탐욕 교환 후 손패 크기가 달라짐")
	}
}

func TestJHGreedExchangeOneCard(t *testing.T) {
	// 트릭 후 손에 1장씩 남으면 1장 교환
	jekyll := []JHCard{jhPo(5), jhEv(JHPride, 1)}
	hyde := []JHCard{jhEv(JHGreed, 4), jhEv(JHWrath, 5)}
	g := newTestJHGame(t, jekyll, hyde, JHJekyll)

	jhPlay(t, g, JHJekyll, JHPotion, 5)
	g.DeclareSuit(JHJekyll, JHGreed)
	jhPlay(t, g, JHHyde, JHGreed, 4)

	if g.Phase != JHPhaseGreedExchange || g.GreedPickCount() != 1 {
		t.Fatalf("1장 교환이어야 함 (phase=%s pick=%d)", g.Phase, g.GreedPickCount())
	}
	g.SubmitGreed(JHJekyll, []int{0})
	if err := g.SubmitGreed(JHHyde, []int{0}); err != nil {
		t.Fatal(err)
	}
	if g.Hands[JHJekyll][0].Suit != JHWrath || g.Hands[JHHyde][0].Suit != JHPride {
		t.Fatal("1장 교환 결과가 틀림")
	}
}

func TestJHGreedLastTrickNoEffect(t *testing.T) {
	// 마지막 트릭(손이 비게 됨)에서는 탐욕 불발 → 바로 정산
	jekyll := []JHCard{jhPo(5)}
	hyde := []JHCard{jhEv(JHGreed, 4)}
	g := newTestJHGame(t, jekyll, hyde, JHJekyll)
	// 이전 트릭들을 채워 정산이 6-4 가 되게 한다 (이동 2칸, 게임 계속 조건은 아래에서 확인)
	for i := 0; i < 5; i++ {
		g.Tricks[JHJekyll] = append(g.Tricks[JHJekyll], JHTrick{Lead: jhEv(JHPride, 1), Follow: jhEv(JHPride, 2)})
	}
	for i := 0; i < 4; i++ {
		g.Tricks[JHHyde] = append(g.Tricks[JHHyde], JHTrick{Lead: jhEv(JHWrath, 1), Follow: jhEv(JHWrath, 2)})
	}

	jhPlay(t, g, JHJekyll, JHPotion, 5)
	g.DeclareSuit(JHJekyll, JHGreed)
	jhPlay(t, g, JHHyde, JHGreed, 4)

	// 탐욕 불발 → 정산: 지킬 6, 하이드 4 → 2칸 이동, 2라운드 시작
	if g.Marker != 2 {
		t.Fatalf("마커가 2여야 함, got %d", g.Marker)
	}
	if g.Round != 2 || g.Phase != JHPhaseExchange {
		t.Fatalf("2라운드 교환 단계여야 함 (round=%d phase=%s)", g.Round, g.Phase)
	}
}

// ==================== 정산 / 승리 ====================

func TestJHSettlement(t *testing.T) {
	cases := []struct {
		name         string
		markerBefore int
		round        int
		jekyllTricks int // 마지막 트릭 전까지; 마지막 트릭은 지킬이 이긴다
		wantMarker   int
		wantPhase    JHPhase
		wantWinner   JHRole
		wantLeader   JHRole // 다음 라운드 선 (게임이 계속될 때)
	}{
		{"동률이면 이동 없음", 3, 1, 4, 3, JHPhaseExchange, "", JHJekyll},
		{"차이만큼 하이드 쪽 이동", 0, 1, 7, 6, JHPhaseExchange, "", JHHyde},
		{"지킬이 크게 이겨도 하이드 쪽 이동", 0, 1, 9, 10, JHPhaseGameOver, JHHyde, ""},
		{"하이드 홈 도달 즉시 하이드 승", 8, 2, 6, 10, JHPhaseGameOver, JHHyde, ""},
		{"3라운드 버티면 지킬 승", 8, 3, 4, 8, JHPhaseGameOver, JHJekyll, ""},
		// 트릭 차이는 |2t-10| 이므로 항상 짝수다
		{"마커 경계: 5는 지킬 진영", 3, 1, 5, 5, JHPhaseExchange, "", JHJekyll},
		{"마커 경계: 6은 하이드 진영", 4, 1, 5, 6, JHPhaseExchange, "", JHHyde},
		{"마커 경계: 7은 하이드 진영", 3, 1, 6, 7, JHPhaseExchange, "", JHHyde},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jekyll := []JHCard{jhEv(JHPride, 7)}
			hyde := []JHCard{jhEv(JHPride, 1)}
			g := newTestJHGame(t, jekyll, hyde, JHJekyll)
			g.Round = tc.round
			g.Marker = tc.markerBefore
			for i := 0; i < tc.jekyllTricks; i++ {
				g.Tricks[JHJekyll] = append(g.Tricks[JHJekyll], JHTrick{Lead: jhEv(JHWrath, 1), Follow: jhEv(JHWrath, 2)})
			}
			for i := 0; i < 9-tc.jekyllTricks; i++ {
				g.Tricks[JHHyde] = append(g.Tricks[JHHyde], JHTrick{Lead: jhEv(JHGreed, 1), Follow: jhEv(JHGreed, 2)})
			}

			jhPlay(t, g, JHJekyll, JHPride, 7)
			jhPlay(t, g, JHHyde, JHPride, 1)

			if g.Marker != tc.wantMarker {
				t.Fatalf("마커 위치: got %d want %d", g.Marker, tc.wantMarker)
			}
			if g.Phase != tc.wantPhase {
				t.Fatalf("단계: got %s want %s", g.Phase, tc.wantPhase)
			}
			if g.Winner != tc.wantWinner {
				t.Fatalf("승자: got %q want %q", g.Winner, tc.wantWinner)
			}
			if tc.wantPhase == JHPhaseExchange && g.Leader != tc.wantLeader {
				t.Fatalf("다음 라운드 선: got %s want %s", g.Leader, tc.wantLeader)
			}
		})
	}
}

// ==================== 봇 게임 (불변식 검증) ====================

// jhCheckInvariants 게임 진행 중 항상 성립해야 하는 조건
func jhCheckInvariants(t *testing.T, g *JHGame, prevMarker int) {
	t.Helper()
	// 리더만 카드를 낸 시점(declare/follow)에는 1장 차이가 정상
	handDiff := len(g.Hands[JHJekyll]) - len(g.Hands[JHHyde])
	if handDiff < 0 {
		handDiff = -handDiff
	}
	midTrick := g.Phase == JHPhaseDeclare || g.Phase == JHPhaseFollow
	if (midTrick && handDiff > 1) || (!midTrick && handDiff != 0) {
		t.Fatalf("손패 크기 불일치: J=%d H=%d (phase=%s)",
			len(g.Hands[JHJekyll]), len(g.Hands[JHHyde]), g.Phase)
	}
	if g.Marker < prevMarker || g.Marker > JHTrackLength {
		t.Fatalf("마커가 역행하거나 범위 밖: %d → %d", prevMarker, g.Marker)
	}
	// 카드 보존: 손 + 테이블 + 트릭 = 20장, ID 중복 없음
	if g.Phase != JHPhaseGameOver && g.Phase != JHPhaseLobby {
		ids := map[int]bool{}
		total := 0
		add := func(c JHCard) {
			if ids[c.ID] {
				t.Fatalf("카드 ID 중복: %d (phase=%s)", c.ID, g.Phase)
			}
			ids[c.ID] = true
			total++
		}
		for _, role := range []JHRole{JHJekyll, JHHyde} {
			for _, c := range g.Hands[role] {
				add(c)
			}
			for _, trick := range g.Tricks[role] {
				add(trick.Lead)
				add(trick.Follow)
			}
		}
		// 효과 처리 단계에서는 테이블 카드가 이미 승자 트릭 더미에 들어가
		// 있으므로(UI 표시용으로만 유지) 트릭 진행 중일 때만 센다
		if midTrick {
			if g.TableLead != nil {
				add(*g.TableLead)
			}
			if g.TableFollow != nil {
				add(*g.TableFollow)
			}
		}
		if total != 20 {
			t.Fatalf("카드 총수가 20이 아님: %d (phase=%s)", total, g.Phase)
		}
	}
	if len(g.RankOrder) > 3 {
		t.Fatalf("랭크에 4개 이상 등록: %v", g.RankOrder)
	}
}

// jhBotAct 현재 단계에서 무작위 합법 수를 둔다
func jhBotAct(t *testing.T, g *JHGame, rng *rand.Rand) {
	t.Helper()
	switch g.Phase {
	case JHPhaseExchange:
		for _, role := range []JHRole{JHJekyll, JHHyde} {
			if _, done := g.ExchangeSel[role]; done {
				continue
			}
			indices := jhRandomExchange(g, role, rng)
			if err := g.SubmitExchange(role, indices); err != nil {
				t.Fatalf("봇 교환 실패: %v (indices=%v hand=%v)", err, indices, g.Hands[role])
			}
			return
		}
	case JHPhaseLead, JHPhaseFollow:
		actor := g.currentActor()
		legal := g.LegalPlays(actor)
		if len(legal) == 0 {
			t.Fatalf("합법 수가 없음 (phase=%s actor=%s hand=%v)", g.Phase, actor, g.Hands[actor])
		}
		if err := g.PlayCard(actor, legal[rng.Intn(len(legal))]); err != nil {
			t.Fatalf("봇 카드 내기 실패: %v", err)
		}
	case JHPhaseDeclare:
		suit := jhEvilSuits[rng.Intn(len(jhEvilSuits))]
		if err := g.DeclareSuit(g.Leader, suit); err != nil {
			t.Fatalf("봇 색 선언 실패: %v", err)
		}
	case JHPhasePrideSteal:
		loser := jhOther(g.TrickWinner)
		idx := rng.Intn(len(g.Tricks[loser]))
		if err := g.StealTrick(g.TrickWinner, idx); err != nil {
			t.Fatalf("봇 강탈 실패: %v", err)
		}
	case JHPhaseGreedExchange:
		for _, role := range []JHRole{JHJekyll, JHHyde} {
			if _, done := g.GreedSel[role]; done {
				continue
			}
			indices := rng.Perm(len(g.Hands[role]))[:g.GreedPickCount()]
			if err := g.SubmitGreed(role, indices); err != nil {
				t.Fatalf("봇 탐욕 교환 실패: %v", err)
			}
			return
		}
	default:
		t.Fatalf("봇이 처리할 수 없는 단계: %s", g.Phase)
	}
}

// jhRandomExchange 물약 강제 규칙을 지키는 무작위 교환 선택
func jhRandomExchange(g *JHGame, role JHRole, rng *rand.Rand) []int {
	hand := g.Hands[role]
	need := g.ExchangeCount()
	perm := rng.Perm(len(hand))
	indices := perm[:need]
	if g.MustIncludePotion(role) {
		hasPotion := false
		for _, i := range indices {
			if hand[i].IsPotion() {
				hasPotion = true
			}
		}
		if !hasPotion {
			// 첫 선택을 아무 물약으로 교체
			for i, c := range hand {
				if c.IsPotion() {
					indices[0] = i
					break
				}
			}
		}
	}
	return indices
}

func TestJHBotGames(t *testing.T) {
	hydeWins, jekyllWins := 0, 0
	for seed := int64(0); seed < 300; seed++ {
		rng := rand.New(rand.NewSource(seed))
		g := NewJHGame(fmt.Sprintf("bot-%d", seed))
		g.AddPlayer("J")
		g.AddPlayer("H")
		if err := g.Start(rng); err != nil {
			t.Fatal(err)
		}

		prevMarker := 0
		for steps := 0; g.Phase != JHPhaseGameOver; steps++ {
			if steps > 500 {
				t.Fatalf("seed %d: 게임이 끝나지 않음 (phase=%s round=%d)", seed, g.Phase, g.Round)
			}
			jhBotAct(t, g, rng)
			jhCheckInvariants(t, g, prevMarker)
			prevMarker = g.Marker
		}

		// 종료 상태 검증
		if g.Winner == "" || g.EndReason == "" {
			t.Fatalf("seed %d: 승자/사유 미기록", seed)
		}
		if g.Winner == JHHyde && g.Marker < JHTrackLength {
			t.Fatalf("seed %d: 하이드 승인데 마커 미도달 (%d)", seed, g.Marker)
		}
		if g.Winner == JHJekyll && (g.Marker >= JHTrackLength || len(g.RoundResults) != JHRounds) {
			t.Fatalf("seed %d: 지킬 승 조건 위반 (marker=%d rounds=%d)", seed, g.Marker, len(g.RoundResults))
		}
		for _, r := range g.RoundResults {
			if r.JekyllTricks+r.HydeTricks != 10 {
				t.Fatalf("seed %d: 라운드 트릭 합이 10이 아님: %+v", seed, r)
			}
		}
		if g.Winner == JHHyde {
			hydeWins++
		} else {
			jekyllWins++
		}
	}
	// 무작위 플레이에서 양쪽 모두 승리가 나오는지 (엔진이 한쪽으로 고장나지 않았는지)
	if hydeWins == 0 || jekyllWins == 0 {
		t.Fatalf("승리 분포 이상: 지킬 %d, 하이드 %d", jekyllWins, hydeWins)
	}
	t.Logf("봇 게임 300판: 지킬 %d승, 하이드 %d승", jekyllWins, hydeWins)
}
