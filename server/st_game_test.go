package server

import (
	"math/rand"
	"testing"
)

// card 테스트용 카드 축약 생성
func card(color, rank int) STCard {
	return STCard{Color: color, Rank: rank}
}

// ==================== 족보 평가 ====================

func TestSTEvaluateCategories(t *testing.T) {
	cases := []struct {
		name     string
		cards    []STCard
		category int
		sum      int
	}{
		{"컬러런", []STCard{card(0, 7), card(0, 9), card(0, 8)}, stFormationColorRun, 24},
		{"트리플", []STCard{card(0, 5), card(1, 5), card(2, 5)}, stFormationTriple, 15},
		{"컬러", []STCard{card(3, 1), card(3, 5), card(3, 9)}, stFormationColor, 15},
		{"런", []STCard{card(0, 3), card(1, 4), card(2, 5)}, stFormationRun, 12},
		{"합계", []STCard{card(0, 1), card(1, 5), card(2, 9)}, stFormationSum, 15},
	}
	for _, tc := range cases {
		got := stEvaluate(tc.cards)
		if got.Category != tc.category || got.Sum != tc.sum {
			t.Errorf("%s: got (category=%d, sum=%d), want (%d, %d)",
				tc.name, got.Category, got.Sum, tc.category, tc.sum)
		}
	}
}

func TestSTFormationBeats(t *testing.T) {
	colorRun := stFormation{stFormationColorRun, 6} // 1-2-3 컬러런
	bigSum := stFormation{stFormationSum, 27}       // 최대 합계

	// 낮은 컬러런이 최대 합계를 이긴다 (카테고리 우선)
	if !colorRun.beats(bigSum, false) {
		t.Error("컬러런이 합계 족보에 져서는 안 된다")
	}
	// 같은 카테고리는 합으로 가른다
	a, b := stFormation{stFormationRun, 15}, stFormation{stFormationRun, 12}
	if !a.beats(b, false) || b.beats(a, true) {
		t.Error("같은 카테고리는 합이 큰 쪽이 이겨야 한다")
	}
	// 카테고리·합 모두 같으면 먼저 완성한 쪽이 이긴다
	c := stFormation{stFormationRun, 15}
	if !a.beats(c, true) || a.beats(c, false) {
		t.Error("완전 동점은 먼저 완성한 쪽이 이겨야 한다")
	}
}

// ==================== 게임 셋업 ====================

func newTestGame(t *testing.T) *STGame {
	t.Helper()
	g := NewSTGame("test")
	if _, err := g.AddPlayer("남유저"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.AddPlayer("북유저"); err != nil {
		t.Fatal(err)
	}
	if err := g.Start(rand.New(rand.NewSource(42))); err != nil {
		t.Fatal(err)
	}
	return g
}

func TestSTStartDeals(t *testing.T) {
	g := newTestGame(t)
	if len(g.Hands[STSouth]) != STHandSize || len(g.Hands[STNorth]) != STHandSize {
		t.Fatalf("손패는 각 %d장이어야 한다", STHandSize)
	}
	if len(g.Deck) != 54-2*STHandSize {
		t.Fatalf("덱은 %d장이어야 한다, got %d", 54-2*STHandSize, len(g.Deck))
	}
	if g.Phase != STPhasePlay {
		t.Fatalf("시작 단계는 play 여야 한다, got %s", g.Phase)
	}
}

// setStone 테스트용으로 돌 상태를 직접 구성한다
func setStone(g *STGame, index int, side STSide, cards []STCard, order int) {
	stone := g.Stones[index]
	stone.Cards[side] = cards
	if len(cards) == 3 {
		stone.CompletedOrder[side] = order
	}
}

// ==================== 획득 판정: 양쪽 완성 ====================

func TestSTClaimBothComplete(t *testing.T) {
	g := newTestGame(t)

	// 남: 컬러런 / 북: 합계 → 남 획득 가능, 북 불가
	setStone(g, 0, STSouth, []STCard{card(0, 1), card(0, 2), card(0, 3)}, 1)
	setStone(g, 0, STNorth, []STCard{card(1, 9), card(2, 9), card(3, 8)}, 2)

	if !g.isClaimable(0, STSouth) {
		t.Error("컬러런은 획득 가능해야 한다")
	}
	if g.isClaimable(0, STNorth) {
		t.Error("진 조합으로 획득할 수 없어야 한다")
	}
}

func TestSTClaimTieBreakByCompletionOrder(t *testing.T) {
	g := newTestGame(t)

	// 완전 동점 (같은 런, 같은 합): 먼저 완성한 남쪽이 이긴다
	setStone(g, 2, STSouth, []STCard{card(0, 4), card(1, 5), card(2, 6)}, 1)
	setStone(g, 2, STNorth, []STCard{card(3, 4), card(4, 5), card(5, 6)}, 2)

	if !g.isClaimable(2, STSouth) {
		t.Error("동점이면 먼저 완성한 쪽이 획득 가능해야 한다")
	}
	if g.isClaimable(2, STNorth) {
		t.Error("동점에서 늦게 완성한 쪽은 획득 불가여야 한다")
	}
}

// ==================== 획득 판정: 선점 증명 ====================

// buildProofGame 증명 테스트용: 손패·덱을 비우고 돌만 직접 구성
func buildProofGame(t *testing.T) *STGame {
	g := newTestGame(t)
	g.Hands[STSouth] = nil
	g.Hands[STNorth] = nil
	g.Deck = nil
	return g
}

func TestSTProofClaimUnbeatable(t *testing.T) {
	g := buildProofGame(t)

	// 남: 최강 컬러런 7-8-9. 상대는 어떤 완성으로도 동점(다른 색 7-8-9)이
	// 최선이고, 동점은 먼저 완성한 남이 이긴다 → 상대 0장이어도 선점 가능
	setStone(g, 4, STSouth, []STCard{card(0, 7), card(0, 8), card(0, 9)}, 1)

	if !g.isClaimable(4, STSouth) {
		t.Error("최강 컬러런은 상대가 0장이어도 선점 가능해야 한다")
	}
}

func TestSTProofClaimBeatablePotential(t *testing.T) {
	g := buildProofGame(t)

	// 남: 합계 조합 (약함). 상대 슬롯이 비어 있으면 무조건 선점 불가
	setStone(g, 3, STSouth, []STCard{card(0, 2), card(1, 5), card(2, 9)}, 1)
	if g.isClaimable(3, STSouth) {
		t.Error("약한 조합은 상대 완성 전에 선점할 수 없어야 한다")
	}

	// 북이 2장 깔린 상태에서 남은 한 장으로 남을 이길 수 있으면 선점 불가
	setStone(g, 5, STSouth, []STCard{card(0, 9), card(1, 9), card(2, 8)}, 2) // 합 26
	setStone(g, 5, STNorth, []STCard{card(3, 9), card(4, 9)}, 0)
	// 북은 다섯 번째 9(색5)로 트리플 완성 가능 → 선점 불가
	if g.isClaimable(5, STSouth) {
		t.Error("상대가 트리플을 완성할 수 있으면 선점 불가여야 한다")
	}
}

func TestSTProofUsesOnlyBoardCards(t *testing.T) {
	g := buildProofGame(t)

	// 북: 색1의 8,9 두 장 → 색1의 7이 남으면 컬러런 완성 가능.
	// 남: 트리플 9 셋 (색0,2,3의 9) — 컬러런에만 진다.
	setStone(g, 6, STSouth, []STCard{card(0, 9), card(2, 9), card(3, 9)}, 1)
	setStone(g, 6, STNorth, []STCard{card(1, 8), card(1, 9)}, 0)

	// 색1의 7이 보드에 없다 → 남 손패에 있더라도 증명에는 못 쓴다 → 선점 불가
	g.Hands[STSouth] = []STCard{card(1, 7)}
	if g.isClaimable(6, STSouth) {
		t.Error("내 손패의 카드는 증명 근거가 될 수 없다 (선점 불가여야 한다)")
	}

	// 같은 상황에서 색1의 7이 다른 돌에 공개돼 있으면 증명 성립 → 선점 가능
	g.Hands[STSouth] = nil
	setStone(g, 0, STNorth, []STCard{card(1, 7)}, 0)
	if !g.isClaimable(6, STSouth) {
		t.Error("컬러런 완성 카드가 보드에 공개됐으면 선점 가능해야 한다")
	}
}

// ==================== 승리 판정 ====================

func TestSTVictoryThreeAdjacent(t *testing.T) {
	g := newTestGame(t)
	g.Stones[3].Owner = STSouth
	g.Stones[4].Owner = STSouth
	g.Stones[5].Owner = STSouth
	reason, over := g.checkVictory(STSouth)
	if !over || reason != "three_adjacent" {
		t.Errorf("인접 3개는 즉시 승리여야 한다, got (%s, %v)", reason, over)
	}
}

func TestSTVictoryFiveScattered(t *testing.T) {
	g := newTestGame(t)
	for _, i := range []int{0, 2, 4, 6, 8} {
		g.Stones[i].Owner = STSouth
	}
	reason, over := g.checkVictory(STSouth)
	if !over || reason != "five_stones" {
		t.Errorf("흩어진 5개는 승리여야 한다, got (%s, %v)", reason, over)
	}
}

func TestSTNoVictory(t *testing.T) {
	g := newTestGame(t)
	g.Stones[0].Owner = STSouth
	g.Stones[2].Owner = STSouth
	g.Stones[4].Owner = STNorth
	if _, over := g.checkVictory(STSouth); over {
		t.Error("돌 2개로는 승리가 아니어야 한다")
	}
}

// ==================== 턴 흐름 ====================

func TestSTPlayCardFlow(t *testing.T) {
	g := newTestGame(t)
	first := g.CurrentSide
	second := stOther(first)

	deckBefore := len(g.Deck)
	if err := g.PlayCard(first, 0, 0); err != nil {
		t.Fatal(err)
	}
	if len(g.Hands[first]) != STHandSize {
		t.Error("카드를 낸 뒤 드로우해서 손패는 다시 6장이어야 한다")
	}
	if len(g.Deck) != deckBefore-1 {
		t.Error("덱이 1장 줄어야 한다")
	}
	if len(g.Stones[0].Cards[first]) != 1 {
		t.Error("돌 0에 카드가 놓여야 한다")
	}
	// 시작 직후에는 획득 가능한 돌이 없으므로 바로 턴이 넘어간다
	if g.CurrentSide != second || g.Phase != STPhasePlay {
		t.Errorf("턴이 상대에게 넘어가야 한다, got side=%s phase=%s", g.CurrentSide, g.Phase)
	}

	// 차례가 아닌 쪽이 내면 에러
	if err := g.PlayCard(first, 0, 0); err == nil {
		t.Error("차례가 아닌 플레이어의 카드 내기는 거부돼야 한다")
	}
}

func TestSTPlayCardValidation(t *testing.T) {
	g := newTestGame(t)
	side := g.CurrentSide

	if err := g.PlayCard(side, -1, 0); err == nil {
		t.Error("잘못된 손패 인덱스는 거부돼야 한다")
	}
	if err := g.PlayCard(side, 0, 9); err == nil {
		t.Error("잘못된 돌 인덱스는 거부돼야 한다")
	}

	g.Stones[1].Owner = STNorth
	if err := g.PlayCard(side, 0, 1); err == nil {
		t.Error("획득된 돌에는 낼 수 없어야 한다")
	}

	g.Stones[2].Cards[side] = []STCard{card(0, 1), card(0, 2), card(0, 3)}
	if err := g.PlayCard(side, 0, 2); err == nil {
		t.Error("3장이 찬 돌에는 낼 수 없어야 한다")
	}
}

func TestSTClaimPhaseAndAutoEndTurn(t *testing.T) {
	g := newTestGame(t)
	side := g.CurrentSide
	opp := stOther(side)

	// 낼 카드가 최강 컬러런을 완성하도록 손패·돌을 조작
	g.Hands[side] = []STCard{card(0, 9)}
	setStone(g, 0, side, []STCard{card(0, 7), card(0, 8)}, 0)

	if err := g.PlayCard(side, 0, 0); err != nil {
		t.Fatal(err)
	}
	if g.Phase != STPhaseClaim || g.CurrentSide != side {
		t.Fatalf("획득 가능한 돌이 생기면 claim 단계여야 한다, got %s", g.Phase)
	}

	if err := g.ClaimStone(side, 0); err != nil {
		t.Fatal(err)
	}
	if g.Stones[0].Owner != side {
		t.Error("돌 0의 주인이 바뀌어야 한다")
	}
	// 남은 획득 가능 돌이 없으므로 자동으로 턴 종료
	if g.CurrentSide != opp || g.Phase != STPhasePlay {
		t.Errorf("획득 후 자동으로 턴이 넘어가야 한다, got side=%s phase=%s", g.CurrentSide, g.Phase)
	}
}

func TestSTEndTurnWithoutClaiming(t *testing.T) {
	g := newTestGame(t)
	side := g.CurrentSide
	opp := stOther(side)

	g.Hands[side] = []STCard{card(0, 9)}
	setStone(g, 0, side, []STCard{card(0, 7), card(0, 8)}, 0)

	if err := g.PlayCard(side, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := g.EndTurn(side); err != nil {
		t.Fatal(err)
	}
	if g.CurrentSide != opp || g.Phase != STPhasePlay {
		t.Error("획득을 포기하고 턴을 넘길 수 있어야 한다")
	}
	if g.Stones[0].Owner != "" {
		t.Error("획득하지 않은 돌은 주인이 없어야 한다")
	}
}

func TestSTDeckExhaustion(t *testing.T) {
	g := newTestGame(t)
	side := g.CurrentSide
	g.Deck = nil

	handBefore := len(g.Hands[side])
	if err := g.PlayCard(side, 0, 0); err != nil {
		t.Fatal(err)
	}
	if len(g.Hands[side]) != handBefore-1 {
		t.Error("덱이 비면 드로우 없이 손패만 줄어야 한다")
	}
}

func TestSTStalemate(t *testing.T) {
	g := newTestGame(t)
	side := g.CurrentSide

	// 양쪽 손패를 비우고 덱도 소진 → 다음 턴 전환에서 교착 종료
	g.Deck = nil
	g.Hands[STSouth] = nil
	g.Hands[STNorth] = nil
	g.Stones[0].Owner = STSouth
	g.Stones[1].Owner = STSouth
	g.Stones[3].Owner = STNorth

	// 마지막 플레이어가 카드를 낼 수 없는 상황을 nextTurn 으로 직접 유도
	g.CurrentSide = side
	g.nextTurn()

	if g.Phase != STPhaseGameOver || g.EndReason != "stalemate" {
		t.Fatalf("교착 시 게임이 끝나야 한다, got phase=%s reason=%s", g.Phase, g.EndReason)
	}
	if g.Winner != STSouth {
		t.Errorf("돌을 많이 가진 남쪽이 이겨야 한다, got %s", g.Winner)
	}
}

// ==================== 전체 판 시뮬레이션 ====================

// TestSTRandomGamesComplete 무작위 플레이로도 게임이 항상 종료되는지 확인
func TestSTRandomGamesComplete(t *testing.T) {
	for seed := int64(0); seed < 20; seed++ {
		rng := rand.New(rand.NewSource(seed))
		g := NewSTGame("sim")
		g.AddPlayer("A")
		g.AddPlayer("B")
		if err := g.Start(rng); err != nil {
			t.Fatal(err)
		}

		for turns := 0; turns < 500; turns++ {
			if g.Phase == STPhaseGameOver {
				break
			}
			side := g.CurrentSide
			switch g.Phase {
			case STPhasePlay:
				// 가능한 아무 수나 둔다
				played := false
				for si := 0; si < STStoneCount && !played; si++ {
					stone := g.Stones[si]
					if stone.Owner != "" || len(stone.Cards[side]) >= 3 {
						continue
					}
					hi := rng.Intn(len(g.Hands[side]))
					if err := g.PlayCard(side, hi, si); err != nil {
						t.Fatalf("seed %d: 유효해야 할 수가 거부됨: %v", seed, err)
					}
					played = true
				}
				if !played {
					t.Fatalf("seed %d: play 단계인데 둘 수 있는 수가 없다", seed)
				}
			case STPhaseClaim:
				claimable := g.ClaimableStones(side)
				if len(claimable) == 0 {
					if err := g.EndTurn(side); err != nil {
						t.Fatalf("seed %d: EndTurn 실패: %v", seed, err)
					}
				} else if err := g.ClaimStone(side, claimable[0]); err != nil {
					t.Fatalf("seed %d: 획득 실패: %v", seed, err)
				}
			}
		}

		if g.Phase != STPhaseGameOver {
			t.Fatalf("seed %d: 500턴 안에 게임이 끝나지 않았다", seed)
		}
	}
}
