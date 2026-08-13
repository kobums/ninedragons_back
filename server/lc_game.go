package server

import (
	"errors"
	"math/rand"
	"time"
)

// NewLCGame 로비 상태의 새 게임
func NewLCGame(id string) *LCGame {
	return &LCGame{
		ID:    id,
		Names: map[LCSide]string{},
		Phase: LCPhaseLobby,
	}
}

// AddPlayer 입장. 먼저 온 사람이 남쪽.
func (g *LCGame) AddPlayer(name string) (LCSide, error) {
	if g.Phase != LCPhaseLobby {
		return "", errors.New("이미 시작된 게임입니다")
	}
	if _, ok := g.Names[LCSouth]; !ok {
		g.Names[LCSouth] = name
		return LCSouth, nil
	}
	if _, ok := g.Names[LCNorth]; !ok {
		g.Names[LCNorth] = name
		return LCNorth, nil
	}
	return "", errors.New("자리가 없습니다")
}

// IsReady 게임 시작 준비 확인
func (g *LCGame) IsReady() bool {
	return len(g.Names) == 2
}

// lcBuildDeck 5색 × (투자 3 + 숫자 2~10) = 60장
func lcBuildDeck() []LCCard {
	deck := []LCCard{}
	id := 0
	for _, color := range lcColors {
		for i := 0; i < LCWagerCount; i++ {
			deck = append(deck, LCCard{ID: id, Color: color, Value: LCWagerValue})
			id++
		}
		for v := 2; v <= 10; v++ {
			deck = append(deck, LCCard{ID: id, Color: color, Value: v})
			id++
		}
	}
	return deck
}

// Start 덱을 섞어 8장씩 나누고 플레이를 시작한다. 선공은 랜덤.
func (g *LCGame) Start(rng *rand.Rand) error {
	if !g.IsReady() {
		return errors.New("시작할 수 없습니다 (2명 필요)")
	}

	deck := lcBuildDeck()
	rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })

	g.Hands = map[LCSide][]LCCard{
		LCSouth: append([]LCCard{}, deck[:LCHandSize]...),
		LCNorth: append([]LCCard{}, deck[LCHandSize:LCHandSize*2]...),
	}
	g.Deck = deck[LCHandSize*2:]

	g.Expeditions = map[LCSide]map[LCColor][]LCCard{
		LCSouth: {}, LCNorth: {},
	}
	g.Discards = map[LCColor][]LCCard{}

	g.CurrentSide = LCSouth
	if rng.Intn(2) == 1 {
		g.CurrentSide = LCNorth
	}
	g.Phase = LCPhasePlay
	g.Ready = true
	g.StartedAt = time.Now()
	return nil
}

// handIndex side 손패에서 카드 위치. 없으면 -1.
func (g *LCGame) handIndex(side LCSide, cardID int) int {
	for i, c := range g.Hands[side] {
		if c.ID == cardID {
			return i
		}
	}
	return -1
}

// canPlay 카드를 내 탐험대에 놓을 수 있는지.
// 투자 카드는 그 색에 숫자 카드가 나오기 전에만, 숫자 카드는 오름차순으로만.
func (g *LCGame) canPlay(side LCSide, card LCCard) error {
	pile := g.Expeditions[side][card.Color]
	if card.Value == LCWagerValue {
		for _, c := range pile {
			if c.Value != LCWagerValue {
				return errors.New("숫자 카드를 놓은 뒤에는 투자 카드를 놓을 수 없습니다")
			}
		}
		return nil
	}
	for _, c := range pile {
		if c.Value != LCWagerValue && c.Value >= card.Value {
			return errors.New("탐험대 카드는 오름차순으로만 놓을 수 있습니다")
		}
	}
	return nil
}

// lcExpeditionScore 탐험대 하나의 점수:
// (숫자 합 − 20) × (1 + 투자 카드 수), 카드 8장 이상이면 +20. 미시작은 0.
func lcExpeditionScore(pile []LCCard) int {
	if len(pile) == 0 {
		return 0
	}
	sum, wagers := 0, 0
	for _, c := range pile {
		if c.Value == LCWagerValue {
			wagers++
		} else {
			sum += c.Value
		}
	}
	score := (sum - LCExpedCost) * (1 + wagers)
	if len(pile) >= LCBonusSize {
		score += LCBonus
	}
	return score
}

// Score side 진영의 총점
func (g *LCGame) Score(side LCSide) int {
	total := 0
	for _, color := range lcColors {
		total += lcExpeditionScore(g.Expeditions[side][color])
	}
	return total
}

// LCMoveResult 한 턴의 결과
type LCMoveResult struct {
	Card          LCCard  // 놓거나 버린 카드 (공개)
	DrawnFromPile *LCCard // 버림 더미에서 뽑았으면 그 카드 (공개), 덱이면 nil
	GameOver      bool
}

// Move 한 턴: 카드 놓기/버리기 + 덱 또는 버림 더미에서 뽑기.
// 모든 검증을 통과한 뒤에만 상태를 바꾼다. 덱이 바닥나면 즉시 종료·채점.
func (g *LCGame) Move(side LCSide, payload LCMovePayload) (*LCMoveResult, error) {
	if g.Phase != LCPhasePlay {
		return nil, errors.New("지금은 플레이할 수 없습니다")
	}
	if side != g.CurrentSide {
		return nil, errors.New("당신의 차례가 아닙니다")
	}

	idx := g.handIndex(side, payload.CardID)
	if idx < 0 {
		return nil, errors.New("손에 없는 카드입니다")
	}
	card := g.Hands[side][idx]

	switch payload.Action {
	case "play":
		if err := g.canPlay(side, card); err != nil {
			return nil, err
		}
	case "discard":
		// 항상 가능
	default:
		return nil, errors.New("알 수 없는 행동입니다")
	}

	if payload.Draw != "deck" {
		drawColor := LCColor(payload.Draw)
		valid := false
		for _, c := range lcColors {
			if c == drawColor {
				valid = true
			}
		}
		if !valid {
			return nil, errors.New("알 수 없는 뽑기 대상입니다")
		}
		if payload.Action == "discard" && card.Color == drawColor {
			return nil, errors.New("방금 버린 카드를 다시 가져올 수 없습니다")
		}
		if len(g.Discards[drawColor]) == 0 {
			return nil, errors.New("그 버림 더미는 비어 있습니다")
		}
	} else if len(g.Deck) == 0 {
		return nil, errors.New("덱이 비어 있습니다")
	}

	// ---- 검증 끝, 상태 변경 ----
	g.Hands[side] = append(g.Hands[side][:idx], g.Hands[side][idx+1:]...)
	if payload.Action == "play" {
		g.Expeditions[side][card.Color] = append(g.Expeditions[side][card.Color], card)
	} else {
		g.Discards[card.Color] = append(g.Discards[card.Color], card)
	}

	result := &LCMoveResult{Card: card}
	if payload.Draw == "deck" {
		drawn := g.Deck[len(g.Deck)-1]
		g.Deck = g.Deck[:len(g.Deck)-1]
		g.Hands[side] = append(g.Hands[side], drawn)
	} else {
		drawColor := LCColor(payload.Draw)
		pile := g.Discards[drawColor]
		drawn := pile[len(pile)-1]
		g.Discards[drawColor] = pile[:len(pile)-1]
		g.Hands[side] = append(g.Hands[side], drawn)
		result.DrawnFromPile = &drawn
	}

	// 덱의 마지막 카드를 뽑으면 게임 종료 — 채점
	if len(g.Deck) == 0 {
		g.Phase = LCPhaseGameOver
		g.EndReason = "score"
		south, north := g.Score(LCSouth), g.Score(LCNorth)
		if south > north {
			g.Winner = LCSouth
		} else if north > south {
			g.Winner = LCNorth
		} // 동점이면 Winner "" (무승부)
		result.GameOver = true
		return result, nil
	}

	g.CurrentSide = lcOther(side)
	return result, nil
}
