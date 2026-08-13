package server

import (
	"math/rand"
	"testing"
)

func lcC(id int, color LCColor, value int) LCCard {
	return LCCard{ID: id, Color: color, Value: value}
}

// newTestLCGame 남 선공으로 고정하고 손패·덱을 통제한 시작 상태
func newTestLCGame(t *testing.T) *LCGame {
	t.Helper()
	g := NewLCGame("test")
	if _, err := g.AddPlayer("남이"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.AddPlayer("북이"); err != nil {
		t.Fatal(err)
	}
	if err := g.Start(rand.New(rand.NewSource(1))); err != nil {
		t.Fatal(err)
	}
	g.CurrentSide = LCSouth
	// 통제된 손패: ID 100번대(남), 200번대(북) — 덱과 절대 겹치지 않는다
	g.Hands[LCSouth] = []LCCard{
		lcC(100, "red", 0), lcC(101, "red", 0), lcC(102, "red", 2), lcC(103, "red", 5),
		lcC(104, "red", 3), lcC(105, "green", 7), lcC(106, "blue", 10), lcC(107, "yellow", 4),
	}
	g.Hands[LCNorth] = []LCCard{
		lcC(200, "white", 0), lcC(201, "white", 6), lcC(202, "green", 2), lcC(203, "green", 9),
		lcC(204, "blue", 3), lcC(205, "blue", 4), lcC(206, "yellow", 8), lcC(207, "red", 10),
	}
	return g
}

// lcMustMove 실패하면 즉시 종료
func lcMustMove(t *testing.T, g *LCGame, side LCSide, cardID int, action, draw string) *LCMoveResult {
	t.Helper()
	result, err := g.Move(side, LCMovePayload{CardID: cardID, Action: action, Draw: draw})
	if err != nil {
		t.Fatalf("move 실패 (card=%d %s→%s): %v", cardID, action, draw, err)
	}
	return result
}

func TestLCDeckComposition(t *testing.T) {
	deck := lcBuildDeck()
	if len(deck) != LCDeckSize {
		t.Fatalf("덱 %d장, want 60", len(deck))
	}
	for _, color := range lcColors {
		wagers, numbers := 0, map[int]int{}
		for _, c := range deck {
			if c.Color != color {
				continue
			}
			if c.Value == LCWagerValue {
				wagers++
			} else {
				numbers[c.Value]++
			}
		}
		if wagers != 3 {
			t.Errorf("%s 투자 카드 %d장, want 3", color, wagers)
		}
		for v := 2; v <= 10; v++ {
			if numbers[v] != 1 {
				t.Errorf("%s %d 카드 %d장, want 1", color, v, numbers[v])
			}
		}
	}
}

func TestLCAscendingRule(t *testing.T) {
	g := newTestLCGame(t)

	// 빨강 5 → 빨강 3 은 내림차순이라 거부
	lcMustMove(t, g, LCSouth, 103, "play", "deck") // red 5
	g.CurrentSide = LCSouth
	if _, err := g.Move(LCSouth, LCMovePayload{CardID: 104, Action: "play", Draw: "deck"}); err == nil {
		t.Error("내림차순 놓기가 허용됨")
	}
	// 버리기는 언제나 가능
	lcMustMove(t, g, LCSouth, 104, "discard", "deck")
}

func TestLCWagerRules(t *testing.T) {
	g := newTestLCGame(t)

	// 투자 → 투자 → 숫자 순서는 허용
	lcMustMove(t, g, LCSouth, 100, "play", "deck") // red 투자
	g.CurrentSide = LCSouth
	lcMustMove(t, g, LCSouth, 102, "play", "deck") // red 2
	g.CurrentSide = LCSouth
	// 숫자를 놓은 뒤 투자 카드는 거부
	if _, err := g.Move(LCSouth, LCMovePayload{CardID: 101, Action: "play", Draw: "deck"}); err == nil {
		t.Error("숫자 뒤 투자 카드가 허용됨")
	}
}

func TestLCDrawRules(t *testing.T) {
	g := newTestLCGame(t)

	// 빈 버림 더미에서 뽑기 거부
	if _, err := g.Move(LCSouth, LCMovePayload{CardID: 103, Action: "play", Draw: "green"}); err == nil {
		t.Error("빈 버림 더미 뽑기가 허용됨")
	}

	// 방금 버린 카드를 되가져오기 거부
	if _, err := g.Move(LCSouth, LCMovePayload{CardID: 103, Action: "discard", Draw: "red"}); err == nil {
		t.Error("방금 버린 카드 되가져오기가 허용됨")
	}

	// 남이 초록 7을 버리고 → 북이 그 카드를 버림 더미에서 가져온다 (공개 뽑기)
	lcMustMove(t, g, LCSouth, 105, "discard", "deck")
	result := lcMustMove(t, g, LCNorth, 204, "discard", "green")
	if result.DrawnFromPile == nil || result.DrawnFromPile.ID != 105 {
		t.Fatalf("버림 더미 뽑기 결과 이상: %+v", result.DrawnFromPile)
	}
	if g.handIndex(LCNorth, 105) < 0 {
		t.Error("뽑은 카드가 북 손에 없다")
	}
	if len(g.Discards["green"]) != 0 {
		t.Error("초록 버림 더미가 비지 않았다")
	}

	// 거부된 액션은 상태를 바꾸지 않는다 (턴 유지·손패 유지)
	handBefore := len(g.Hands[LCSouth])
	if _, err := g.Move(LCSouth, LCMovePayload{CardID: 106, Action: "play", Draw: "purple"}); err == nil {
		t.Error("알 수 없는 뽑기 대상이 허용됨")
	}
	if len(g.Hands[LCSouth]) != handBefore || g.CurrentSide != LCSouth {
		t.Error("거부된 액션이 상태를 바꿈")
	}
}

func TestLCTurnAndHandSize(t *testing.T) {
	g := newTestLCGame(t)

	if _, err := g.Move(LCNorth, LCMovePayload{CardID: 200, Action: "discard", Draw: "deck"}); err == nil {
		t.Error("차례가 아닌데 플레이가 허용됨")
	}

	deckBefore := len(g.Deck)
	lcMustMove(t, g, LCSouth, 107, "discard", "deck")
	if len(g.Hands[LCSouth]) != LCHandSize {
		t.Errorf("턴 후 손패 %d장, want 8", len(g.Hands[LCSouth]))
	}
	if len(g.Deck) != deckBefore-1 {
		t.Error("덱이 줄지 않았다")
	}
	if g.CurrentSide != LCNorth {
		t.Error("턴이 넘어가지 않음")
	}
}

func TestLCExpeditionScore(t *testing.T) {
	cases := []struct {
		name string
		pile []LCCard
		want int
	}{
		{"미시작", []LCCard{}, 0},
		{"숫자 하나", []LCCard{lcC(0, "red", 5)}, -15},
		{"투자 1 + 숫자", []LCCard{lcC(0, "red", 0), lcC(1, "red", 2), lcC(2, "red", 3)}, -30},
		{"흑자 탐험", []LCCard{lcC(0, "red", 7), lcC(1, "red", 8), lcC(2, "red", 9), lcC(3, "red", 10)}, 14},
		// 합 35 − 20 = 15, 투자 1 → ×2 = 30, 8장 보너스 +20 = 50
		{"8장 보너스", []LCCard{
			lcC(0, "red", 0), lcC(1, "red", 2), lcC(2, "red", 3), lcC(3, "red", 4),
			lcC(4, "red", 5), lcC(5, "red", 6), lcC(6, "red", 7), lcC(7, "red", 8),
		}, 50},
	}
	for _, c := range cases {
		if got := lcExpeditionScore(c.pile); got != c.want {
			t.Errorf("%s: score = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestLCDeckExhaustionEndsGame(t *testing.T) {
	g := newTestLCGame(t)

	// 덱을 1장만 남긴다
	g.Deck = g.Deck[:1]

	// 남쪽이 흑자 탐험을 하나 만들어둔다
	g.Expeditions[LCSouth]["blue"] = []LCCard{lcC(300, "blue", 8), lcC(301, "blue", 9), lcC(302, "blue", 10)}

	result := lcMustMove(t, g, LCSouth, 107, "discard", "deck")
	if !result.GameOver {
		t.Fatal("덱 소진이 게임을 끝내지 않음")
	}
	if g.Phase != LCPhaseGameOver || g.EndReason != "score" {
		t.Errorf("종료 상태 이상: %s %s", g.Phase, g.EndReason)
	}
	if g.Winner != LCSouth {
		t.Errorf("승자 = %q, want south (7 vs 0)", g.Winner)
	}
	if g.Score(LCSouth) != 7 || g.Score(LCNorth) != 0 {
		t.Errorf("점수 이상: 남 %d, 북 %d", g.Score(LCSouth), g.Score(LCNorth))
	}

	// 종료 후 플레이 거부
	if _, err := g.Move(LCNorth, LCMovePayload{CardID: 200, Action: "discard", Draw: "deck"}); err == nil {
		t.Error("게임 종료 후 플레이가 허용됨")
	}
}

func TestLCTieGame(t *testing.T) {
	g := newTestLCGame(t)
	g.Deck = g.Deck[:1]

	// 양쪽 다 탐험 없음 → 0:0 무승부
	result := lcMustMove(t, g, LCSouth, 107, "discard", "deck")
	if !result.GameOver {
		t.Fatal("게임이 끝나지 않음")
	}
	if g.Winner != "" {
		t.Errorf("무승부인데 승자 = %q", g.Winner)
	}
}
