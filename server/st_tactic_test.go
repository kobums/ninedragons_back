package server

import (
	"math/rand"
	"testing"
)

// tactic 테스트용 전술 카드 생성
func tactic(t STTactic) STCard {
	return STCard{Tactic: t}
}

func newTacticGame(t *testing.T) *STGame {
	t.Helper()
	g := NewSTGame("tactic-test", true)
	if _, err := g.AddPlayer("남유저"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.AddPlayer("북유저"); err != nil {
		t.Fatal(err)
	}
	if err := g.Start(rand.New(rand.NewSource(7))); err != nil {
		t.Fatal(err)
	}
	return g
}

// ==================== 와일드카드 족보 평가 ====================

func TestSTWildcardFormations(t *testing.T) {
	// 조커가 컬러런을 완성한다: 색0 7,8 + 조커(→9) = 컬러런 합 24
	form := stBestFormation([]STCard{card(0, 7), card(0, 8), tactic(STTacticJoker)}, false)
	if form.Category != stFormationColorRun || form.Sum != 24 {
		t.Errorf("조커 컬러런: got (%d, %d), want (%d, 24)", form.Category, form.Sum, stFormationColorRun)
	}

	// 스파이는 7 고정: 색0 7 + 색1 7 + 스파이 = 트리플 합 21
	form = stBestFormation([]STCard{card(0, 7), card(1, 7), tactic(STTacticSpy)}, false)
	if form.Category != stFormationTriple || form.Sum != 21 {
		t.Errorf("스파이 트리플: got (%d, %d), want (%d, 21)", form.Category, form.Sum, stFormationTriple)
	}

	// 방패병은 최대 3: 색0 1,2 + 방패병(→3) = 컬러런 합 6
	form = stBestFormation([]STCard{card(0, 1), card(0, 2), tactic(STTacticShield)}, false)
	if form.Category != stFormationColorRun || form.Sum != 6 {
		t.Errorf("방패병 컬러런: got (%d, %d), want (%d, 6)", form.Category, form.Sum, stFormationColorRun)
	}

	// 눈가리개: 족보 무시, 합계만. 조커는 9로 계산
	form = stBestFormation([]STCard{card(0, 1), card(0, 2), tactic(STTacticJoker)}, true)
	if form.Category != stFormationSum || form.Sum != 12 {
		t.Errorf("눈가리개 합계: got (%d, %d), want (%d, 12)", form.Category, form.Sum, stFormationSum)
	}

	// 4장 족보 (진흙탕): 색2 3,4,5,6 = 컬러런 합 18
	form = stBestFormation([]STCard{card(2, 3), card(2, 4), card(2, 5), card(2, 6)}, false)
	if form.Category != stFormationColorRun || form.Sum != 18 {
		t.Errorf("4장 컬러런: got (%d, %d), want (%d, 18)", form.Category, form.Sum, stFormationColorRun)
	}
}

// ==================== 눈가리개·진흙탕 판정 ====================

func TestSTBlindClaimBySumOnly(t *testing.T) {
	g := newTacticGame(t)
	stone := g.Stones[0]
	stone.Blind = true

	// 남: 합 26 (족보 없음) / 북: 컬러런 1-2-3 (합 6)
	setStone(g, 0, STSouth, []STCard{card(0, 9), card(1, 9), card(2, 8)}, 1)
	setStone(g, 0, STNorth, []STCard{card(3, 1), card(3, 2), card(3, 3)}, 2)

	if !g.isClaimable(0, STSouth) {
		t.Error("눈가리개 돌은 합이 큰 쪽이 이겨야 한다")
	}
	if g.isClaimable(0, STNorth) {
		t.Error("눈가리개 돌에서 컬러런은 의미가 없어야 한다")
	}
}

func TestSTMudRequiresFourCards(t *testing.T) {
	g := newTacticGame(t)
	stone := g.Stones[2]

	// 양쪽 3장 완성 상태에서 진흙탕이 걸리면 완성이 풀린다
	setStone(g, 2, STSouth, []STCard{card(0, 7), card(0, 8), card(0, 9)}, 1)
	setStone(g, 2, STNorth, []STCard{card(1, 1), card(1, 2), card(1, 3)}, 2)

	stone.Mud = true
	g.syncCompletion(stone, STSouth)
	g.syncCompletion(stone, STNorth)

	if stone.CompletedOrder[STSouth] != 0 || stone.CompletedOrder[STNorth] != 0 {
		t.Fatal("진흙탕이 걸리면 3장 완성 상태가 풀려야 한다")
	}
	if g.isClaimable(2, STSouth) {
		t.Error("진흙탕 돌은 3장으로 획득할 수 없어야 한다")
	}

	// 4장째를 채우면 다시 완성
	stone.Cards[STSouth] = append(stone.Cards[STSouth], card(0, 6))
	g.syncCompletion(stone, STSouth)
	if stone.CompletedOrder[STSouth] == 0 {
		t.Error("4장을 채우면 완성으로 기록돼야 한다")
	}
}

// ==================== 정예병 사용 규칙 ====================

func TestSTJokerPerSideLimit(t *testing.T) {
	g := newTacticGame(t)
	side := g.CurrentSide

	g.Hands[side] = []STCard{tactic(STTacticJoker), tactic(STTacticJoker), card(0, 1)}
	if err := g.PlayCard(side, 0, 0); err != nil {
		t.Fatal(err)
	}

	// 상대 턴을 다시 내 턴으로 돌리고 두 번째 조커 시도
	g.CurrentSide = side
	g.Phase = STPhasePlay
	g.PlayedTactics[stOther(side)] = 5 // 제약을 피해 조커 제한만 검사
	if err := g.PlayCard(side, 0, 1); err == nil {
		t.Error("조커는 진영당 1장만 낼 수 있어야 한다")
	}
}

func TestSTTacticCountConstraint(t *testing.T) {
	g := newTacticGame(t)
	side := g.CurrentSide

	g.Hands[side] = []STCard{tactic(STTacticSpy), tactic(STTacticShield), card(0, 1)}

	// 첫 전술 사용은 허용 (0 == 0)
	if err := g.PlayCard(side, 0, 0); err != nil {
		t.Fatal(err)
	}
	// 상대가 전술을 쓰지 않은 채 내가 또 쓰려 하면 거부 (1 > 0)
	g.CurrentSide = side
	g.Phase = STPhasePlay
	if err := g.PlayCard(side, 0, 1); err == nil {
		t.Error("상대보다 전술 카드를 1장 초과해서 쓸 수 없어야 한다")
	}
}

// ==================== 계략 효과 ====================

func TestSTBansheeDiscards(t *testing.T) {
	g := newTacticGame(t)
	side := g.CurrentSide
	opp := stOther(side)

	setStone(g, 1, opp, []STCard{card(2, 5), card(2, 6), card(2, 7)}, 1)
	g.Hands[side] = []STCard{tactic(STTacticBanshee)}

	err := g.PlayRuse(side, STPlayRusePayload{HandIndex: 0, FromStone: 1, FromIndex: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Stones[1].Cards[opp]) != 2 {
		t.Error("밴시는 상대 카드를 1장 제거해야 한다")
	}
	if g.Stones[1].CompletedOrder[opp] != 0 {
		t.Error("카드가 빠지면 완성 기록이 풀려야 한다")
	}
	if len(g.Discard) != 2 {
		t.Errorf("버린 더미에 대상 카드와 밴시가 있어야 한다, got %d", len(g.Discard))
	}
}

func TestSTStrategistMovesOwnCard(t *testing.T) {
	g := newTacticGame(t)
	side := g.CurrentSide

	setStone(g, 1, side, []STCard{card(2, 5), card(2, 6), card(2, 7)}, 1)
	g.Hands[side] = []STCard{tactic(STTacticStrategist)}

	err := g.PlayRuse(side, STPlayRusePayload{HandIndex: 0, FromStone: 1, FromIndex: 0, ToStone: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Stones[1].Cards[side]) != 2 || len(g.Stones[3].Cards[side]) != 1 {
		t.Error("전략가는 내 카드를 다른 돌로 옮겨야 한다")
	}
	if g.Stones[1].CompletedOrder[side] != 0 {
		t.Error("카드가 빠진 돌의 완성 기록이 풀려야 한다")
	}
}

func TestSTTraitorStealsClanOnly(t *testing.T) {
	g := newTacticGame(t)
	side := g.CurrentSide
	opp := stOther(side)

	g.Stones[1].Cards[opp] = []STCard{tactic(STTacticSpy), card(2, 6)}
	g.Hands[side] = []STCard{tactic(STTacticTraitor), tactic(STTacticTraitor)}

	// 정예병(스파이)은 강탈 불가
	err := g.PlayRuse(side, STPlayRusePayload{HandIndex: 0, FromStone: 1, FromIndex: 0, ToStone: 4})
	if err == nil {
		t.Error("배신자는 클랜 카드만 데려올 수 있어야 한다")
	}

	// 클랜 카드는 강탈 가능 — 같은 돌의 내 쪽으로도 옮길 수 있다
	err = g.PlayRuse(side, STPlayRusePayload{HandIndex: 0, FromStone: 1, FromIndex: 1, ToStone: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Stones[1].Cards[opp]) != 1 || len(g.Stones[1].Cards[side]) != 1 {
		t.Error("배신자는 상대 클랜 카드를 내 쪽으로 옮겨야 한다")
	}
}

func TestSTRecruiterFlow(t *testing.T) {
	g := newTacticGame(t)
	side := g.CurrentSide
	opp := stOther(side)

	g.Hands[side] = append([]STCard{tactic(STTacticRecruiter)}, g.Hands[side][:6]...)

	if err := g.PlayRuse(side, STPlayRusePayload{HandIndex: 0}); err != nil {
		t.Fatal(err)
	}
	if g.Phase != STPhaseRecruiterDraw {
		t.Fatalf("모병관은 뽑기 단계로 가야 한다, got %s", g.Phase)
	}

	// 3장 뽑기 (클랜 2 + 전술 1)
	for _, deck := range []string{"clan", "tactic", "clan"} {
		if err := g.RecruiterDraw(side, deck); err != nil {
			t.Fatal(err)
		}
	}
	if g.Phase != STPhaseRecruiterReturn {
		t.Fatalf("3장 뽑으면 반납 단계로 가야 한다, got %s", g.Phase)
	}
	if len(g.Hands[side]) != 9 {
		t.Fatalf("손패가 9장이어야 한다, got %d", len(g.Hands[side]))
	}

	// 2장 반납
	if err := g.RecruiterReturn(side, 0); err != nil {
		t.Fatal(err)
	}
	if err := g.RecruiterReturn(side, 0); err != nil {
		t.Fatal(err)
	}

	if len(g.Hands[side]) != 7 {
		t.Errorf("반납 후 손패는 7장이어야 한다, got %d", len(g.Hands[side]))
	}
	if g.CurrentSide != opp {
		t.Error("모병관 처리 후 턴이 넘어가야 한다")
	}
}

// ==================== 드로우 선택 ====================

func TestSTDrawChoicePhase(t *testing.T) {
	g := newTacticGame(t)
	side := g.CurrentSide
	opp := stOther(side)

	if err := g.PlayCard(side, 0, 0); err != nil {
		t.Fatal(err)
	}
	if g.Phase != STPhaseDraw || g.CurrentSide != side {
		t.Fatalf("두 덱이 모두 남아 있으면 드로우 선택 단계여야 한다, got %s", g.Phase)
	}

	if err := g.DrawFrom(side, "tactic"); err != nil {
		t.Fatal(err)
	}
	last := g.Hands[side][len(g.Hands[side])-1]
	if last.IsClan() {
		t.Error("전술 덱에서 뽑으면 전술 카드가 와야 한다")
	}
	if len(g.Hands[side]) != STTacticHandSize {
		t.Errorf("손패는 %d장이어야 한다, got %d", STTacticHandSize, len(g.Hands[side]))
	}
	if g.CurrentSide != opp || g.Phase != STPhasePlay {
		t.Error("드로우 후 턴이 넘어가야 한다")
	}
}

// ==================== 선점 증명과 전술 카드 ====================

func TestSTProofConsidersUnseenTactics(t *testing.T) {
	g := newTacticGame(t)
	g.Hands[STSouth] = nil
	g.Hands[STNorth] = nil
	g.Deck = nil

	// 남: 트리플 9 (기본 게임이라면 상대 0장일 때도 선점 여지가 있지만)
	// 전술 게임에서는 미공개 조커가 컬러런을 만들 수 있어 선점 불가여야 한다
	setStone(g, 6, STSouth, []STCard{card(0, 9), card(2, 9), card(3, 9)}, 1)
	if g.isClaimable(6, STSouth) {
		t.Error("미공개 조커가 있으면 트리플 선점은 불가해야 한다")
	}

	// 최강 컬러런 7-8-9 는 조커로도 동점이 최선 → 여전히 선점 가능
	setStone(g, 4, STSouth, []STCard{card(1, 7), card(1, 8), card(1, 9)}, 2)
	if !g.isClaimable(4, STSouth) {
		t.Error("최강 컬러런은 전술 게임에서도 선점 가능해야 한다")
	}
}

func TestSTProofExcludesDiscard(t *testing.T) {
	g := newTacticGame(t)
	g.Hands[STSouth] = nil
	g.Hands[STNorth] = nil
	g.Deck = nil

	// 조커·스파이·방패병이 모두 버려졌고, 색1 7도 버려졌다면
	// 북(색1 8,9)의 컬러런 완성 수단이 사라진다
	g.Discard = []STCard{
		tactic(STTacticJoker), tactic(STTacticJoker),
		tactic(STTacticSpy), tactic(STTacticShield),
		card(1, 7),
	}
	setStone(g, 6, STSouth, []STCard{card(0, 9), card(2, 9), card(3, 9)}, 1)
	setStone(g, 6, STNorth, []STCard{card(1, 8), card(1, 9)}, 0)

	if !g.isClaimable(6, STSouth) {
		t.Error("버린 더미의 카드는 상대가 쓸 수 없으므로 선점 가능해야 한다")
	}
}

// ==================== 무작위 완주 시뮬레이션 (전술 모드) ====================

func TestSTTacticRandomGamesComplete(t *testing.T) {
	for seed := int64(0); seed < 12; seed++ {
		rng := rand.New(rand.NewSource(seed))
		g := NewSTGame("tactic-sim", true)
		g.AddPlayer("A")
		g.AddPlayer("B")
		if err := g.Start(rng); err != nil {
			t.Fatal(err)
		}

		for turns := 0; turns < 2000; turns++ {
			if g.Phase == STPhaseGameOver {
				break
			}
			side := g.CurrentSide
			switch g.Phase {
			case STPhasePlay:
				if !stSimPlay(t, g, rng, seed) {
					t.Fatalf("seed %d: play 단계에서 가능한 수가 없다", seed)
				}
			case STPhaseClaim:
				claimable := g.ClaimableStones(side)
				if len(claimable) == 0 {
					if err := g.EndTurn(side); err != nil {
						t.Fatalf("seed %d: EndTurn 실패: %v", seed, err)
					}
				} else if err := g.ClaimStone(side, claimable[rng.Intn(len(claimable))]); err != nil {
					t.Fatalf("seed %d: 획득 실패: %v", seed, err)
				}
			case STPhaseDraw:
				deck := "clan"
				if len(g.Deck) == 0 || (len(g.TacticDeck) > 0 && rng.Intn(2) == 0) {
					deck = "tactic"
				}
				if err := g.DrawFrom(side, deck); err != nil {
					t.Fatalf("seed %d: 드로우 실패: %v", seed, err)
				}
			case STPhaseRecruiterDraw:
				deck := "clan"
				if len(g.Deck) == 0 {
					deck = "tactic"
				}
				if err := g.RecruiterDraw(side, deck); err != nil {
					t.Fatalf("seed %d: 모병관 뽑기 실패: %v", seed, err)
				}
			case STPhaseRecruiterReturn:
				if err := g.RecruiterReturn(side, rng.Intn(len(g.Hands[side]))); err != nil {
					t.Fatalf("seed %d: 모병관 반납 실패: %v", seed, err)
				}
			}
		}

		if g.Phase != STPhaseGameOver {
			t.Fatalf("seed %d: 2000턴 안에 게임이 끝나지 않았다", seed)
		}
	}
}

// stSimPlay 가능한 수를 무작위 순서로 시도한다. 성공하면 true.
func stSimPlay(t *testing.T, g *STGame, rng *rand.Rand, seed int64) bool {
	t.Helper()
	side := g.CurrentSide
	opp := stOther(side)

	type move func() error
	moves := []move{}

	for h := range g.Hands[side] {
		h := h
		c := g.Hands[side][h]
		if c.IsRuse() {
			switch c.Tactic {
			case STTacticRecruiter:
				moves = append(moves, func() error {
					return g.PlayRuse(side, STPlayRusePayload{HandIndex: h})
				})
			case STTacticBanshee:
				for s := range g.Stones {
					s := s
					for i := range g.Stones[s].Cards[opp] {
						i := i
						moves = append(moves, func() error {
							return g.PlayRuse(side, STPlayRusePayload{HandIndex: h, FromStone: s, FromIndex: i})
						})
					}
				}
			case STTacticStrategist:
				for s := range g.Stones {
					s := s
					for i := range g.Stones[s].Cards[side] {
						i := i
						moves = append(moves, func() error {
							return g.PlayRuse(side, STPlayRusePayload{HandIndex: h, FromStone: s, FromIndex: i, ToStone: -1})
						})
						for d := range g.Stones {
							d := d
							moves = append(moves, func() error {
								return g.PlayRuse(side, STPlayRusePayload{HandIndex: h, FromStone: s, FromIndex: i, ToStone: d})
							})
						}
					}
				}
			case STTacticTraitor:
				for s := range g.Stones {
					s := s
					for i := range g.Stones[s].Cards[opp] {
						i := i
						for d := range g.Stones {
							d := d
							moves = append(moves, func() error {
								return g.PlayRuse(side, STPlayRusePayload{HandIndex: h, FromStone: s, FromIndex: i, ToStone: d})
							})
						}
					}
				}
			}
		} else {
			for s := range g.Stones {
				s := s
				moves = append(moves, func() error {
					return g.PlayCard(side, h, s)
				})
			}
		}
	}

	rng.Shuffle(len(moves), func(i, j int) { moves[i], moves[j] = moves[j], moves[i] })
	for _, m := range moves {
		if m() == nil {
			return true
		}
	}
	// 아무 수도 통하지 않으면 패스 (전술 룰)
	if err := g.Pass(side); err != nil {
		t.Fatalf("seed %d: 패스도 실패: %v", seed, err)
	}
	return true
}
