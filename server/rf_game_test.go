package server

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// ==================== 리포메이션 순수 규칙 테스트 ====================
//
// 기본 쿠에서 옮겨 온 규칙(도전·차단·쿠 강제·교환)과 리포메이션 확장
// (같은 진영 공격 금지·국고 누적·횡령 도전 증명·진영 승리)을 표 기반으로
// 촘촘히 검증한다.

// rfNewTestGame n 인 게임을 시작 상태로 만든다 (카드·진영·차례는 각 테스트가
// 결정적으로 덮어쓴다)
func rfNewTestGame(t *testing.T, n int) (*RFGame, *rand.Rand) {
	t.Helper()
	g := NewRFGame("rf-test")
	for i := 0; i < n; i++ {
		if _, err := g.AddPlayer(fmt.Sprintf("P%d", i)); err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
	}
	rng := rand.New(rand.NewSource(7))
	if err := g.Start(rng); err != nil {
		t.Fatalf("Start: %v", err)
	}
	g.DrainEvents()
	return g, rng
}

func rfSetCards(g *RFGame, seat int, a, b RFRole) {
	g.Players[seat].Cards = []RFCard{{Role: a}, {Role: b}}
}

// rfSetFactions 좌석 순서대로 진영을 고정한다
func rfSetFactions(g *RFGame, factions ...RFFaction) {
	for i, f := range factions {
		if i < len(g.Players) {
			g.Players[i].Faction = f
		}
	}
}

// rfEventText 이벤트 큐를 비우고 문구를 이어붙인다 (문구 검증용)
func rfEventText(g *RFGame) string {
	msgs := []string{}
	for _, ev := range g.DrainEvents() {
		msgs = append(msgs, ev.Kind+":"+ev.Message)
	}
	return strings.Join(msgs, "\n")
}

// rfRoleMultiset 덱 + 전원 손패의 역할 개수 (카드 보존 검증용)
func rfRoleMultiset(g *RFGame) map[RFRole]int {
	m := map[RFRole]int{}
	for _, r := range g.Deck {
		m[r]++
	}
	for _, p := range g.Players {
		for _, c := range p.Cards {
			m[c.Role]++
		}
	}
	return m
}

// ==================== 덱 구성 ====================

// TestRFDeckComposition 인원별 덱 장수 — 6인 이하 역할당 3장(15장),
// 7인 이상 역할당 4장(20장). 배분 후 남은 덱도 같이 확인한다.
func TestRFDeckComposition(t *testing.T) {
	cases := []struct {
		players  int
		wantDeck int // 전체 덱
		wantLeft int // 배분 후 남은 덱
	}{
		{2, 15, 11},
		{5, 15, 5},
		{6, 15, 3},
		{7, 20, 6},
		{8, 20, 4},
		{10, 20, 0},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d인", tc.players), func(t *testing.T) {
			if got := len(rfDeckComposition(tc.players)); got != tc.wantDeck {
				t.Fatalf("덱 구성 = %d장, want %d", got, tc.wantDeck)
			}
			g, _ := rfNewTestGame(t, tc.players)
			if got := len(g.Deck); got != tc.wantLeft {
				t.Fatalf("배분 후 덱 = %d장, want %d", got, tc.wantLeft)
			}
			for _, p := range g.Players {
				if len(p.Cards) != RFCardsPerPlayer || p.Chips != RFStartChips {
					t.Fatalf("seat%d 초기 상태: 카드 %d장 칩 %d개", p.Seat, len(p.Cards), p.Chips)
				}
				if p.Faction != RFFactionLoyalist && p.Faction != RFFactionReformist {
					t.Fatalf("seat%d 진영 미배정: %q", p.Seat, p.Faction)
				}
			}
			// 진영은 절반씩 (홀수면 한쪽이 1명 많다)
			loyal := g.FactionCount(RFFactionLoyalist)
			reform := g.FactionCount(RFFactionReformist)
			if loyal+reform != tc.players || loyal != tc.players/2 {
				t.Fatalf("진영 분배 = 충성파 %d / 개혁파 %d (%d인)", loyal, reform, tc.players)
			}
			if g.Treasury != 0 {
				t.Fatalf("시작 국고 = %d, want 0", g.Treasury)
			}
		})
	}
}

// ==================== 같은 진영 공격 금지 ====================

// TestRFSameFactionAttackRejected 강탈·암살·쿠는 같은 진영에게 쓸 수 없다.
// 거부될 때는 비용도 빠져나가지 않아야 한다. 해외원조·세금·수입·개종은
// 진영과 무관하게 통과한다.
func TestRFSameFactionAttackRejected(t *testing.T) {
	cases := []struct {
		name    string
		kind    RFActionKind
		chips   int
		same    bool
		wantErr bool
	}{
		{"쿠-같은진영", RFActCoup, RFCoupCost, true, true},
		{"쿠-다른진영", RFActCoup, RFCoupCost, false, false},
		{"암살-같은진영", RFActAssassinate, RFAssassinCost, true, true},
		{"암살-다른진영", RFActAssassinate, RFAssassinCost, false, false},
		{"강탈-같은진영", RFActSteal, 2, true, true},
		{"강탈-다른진영", RFActSteal, 2, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, rng := rfNewTestGame(t, 3)
			g.CurrentSeat = 0
			target := RFFactionReformist
			if tc.same {
				target = RFFactionLoyalist
			}
			rfSetFactions(g, RFFactionLoyalist, target, RFFactionReformist)
			rfSetCards(g, 0, RFRoleAssassin, RFRoleCaptain)
			rfSetCards(g, 1, RFRoleDuke, RFRoleDuke)
			rfSetCards(g, 2, RFRoleDuke, RFRoleDuke)
			g.Players[0].Chips = tc.chips

			err := g.DeclareAction(0, tc.kind, 1, rng)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("%s이 같은 진영에게 허용됐다", rfActionName(tc.kind))
				}
				if !strings.Contains(err.Error(), rfSameFactionMsg) {
					t.Fatalf("거부 문구 = %q, want %q 포함", err.Error(), rfSameFactionMsg)
				}
				if g.Players[0].Chips != tc.chips {
					t.Fatalf("거부됐는데 비용이 빠졌다: chips=%d, want %d", g.Players[0].Chips, tc.chips)
				}
				if g.Phase != RFPhaseAction {
					t.Fatalf("거부 뒤 phase = %s, want action", g.Phase)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s이 다른 진영에게 거부됐다: %v", rfActionName(tc.kind), err)
			}
		})
	}

	// 공격이 아닌 행동은 같은 진영이어도 통과한다
	t.Run("비공격-같은진영-허용", func(t *testing.T) {
		g, rng := rfNewTestGame(t, 3)
		rfSetFactions(g, RFFactionLoyalist, RFFactionLoyalist, RFFactionReformist)
		g.CurrentSeat = 0
		g.Players[0].Chips = 5
		if err := g.SubmitConvertOther(0, 1, rng); err != nil {
			t.Fatalf("같은 진영 개종이 거부됐다: %v", err)
		}
		if g.Players[1].Faction != RFFactionReformist {
			t.Fatalf("개종 미적용: %s", g.Players[1].Faction)
		}
		g.CurrentSeat = 1
		if err := g.DeclareAction(1, RFActAid, -1, rng); err != nil {
			t.Fatalf("해외원조가 거부됐다: %v", err)
		}
		// 해외원조 차단은 같은 진영도 가능하다
		if err := g.SubmitBlock(0, RFRoleDuke); err != nil {
			t.Fatalf("차단이 거부됐다: %v", err)
		}
	})
}

// ==================== 국고 / 개종 ====================

// TestRFTreasuryAccumulates 개종 비용이 국고에 그대로 쌓이고, 칩이 모자라면
// 거부된다. 개종은 도전·차단 대상이 아니라 즉시 발동하고 턴이 넘어간다.
func TestRFTreasuryAccumulates(t *testing.T) {
	type step struct {
		name      string
		self      bool // true: rf_convert, false: rf_convert_other
		seat      int
		target    int
		chips     int // 행동 전 보유 칩
		wantErr   bool
		wantChips int
		wantTreas int
	}
	// 진영이 한쪽으로 몰리면 즉시 진영 승리로 끝나므로(TestRFFactionVictory
	// 에서 따로 검증) 여기서는 늘 양 진영이 남도록 대상을 고른다
	steps := []step{
		{"자기개종-칩1", true, 0, -1, 1, false, 0, 1},
		{"자기개종-칩0-거부", true, 1, -1, 0, true, 0, 1},
		{"남개종-칩2", false, 1, 0, 2, false, 0, 3},
		{"남개종-칩1-거부", false, 2, 0, 1, true, 1, 3},
		{"남개종-누적", false, 2, 3, 4, false, 2, 5},
	}

	g, rng := rfNewTestGame(t, 4)
	rfSetFactions(g, RFFactionLoyalist, RFFactionReformist, RFFactionLoyalist, RFFactionReformist)
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			g.CurrentSeat = st.seat
			g.Phase = RFPhaseAction
			g.Players[st.seat].Chips = st.chips

			var before RFFaction
			var err error
			if st.self {
				before = g.Players[st.seat].Faction
				err = g.SubmitConvert(st.seat, rng)
			} else {
				before = g.Players[st.target].Faction
				err = g.SubmitConvertOther(st.seat, st.target, rng)
			}

			if st.wantErr {
				if err == nil {
					t.Fatal("칩이 모자란 개종이 허용됐다")
				}
			} else if err != nil {
				t.Fatalf("개종 실패: %v", err)
			}
			if g.Players[st.seat].Chips != st.wantChips {
				t.Fatalf("행동자 칩 = %d, want %d", g.Players[st.seat].Chips, st.wantChips)
			}
			if g.Treasury != st.wantTreas {
				t.Fatalf("국고 = %d, want %d", g.Treasury, st.wantTreas)
			}
			if st.wantErr {
				return
			}
			flipped := g.Players[st.seat].Faction
			if !st.self {
				flipped = g.Players[st.target].Faction
			}
			if flipped != rfFlipFaction(before) {
				t.Fatalf("진영이 뒤집히지 않았다: %s → %s", before, flipped)
			}
			// 도전·차단 창 없이 곧바로 다음 차례
			if g.Phase != RFPhaseAction || g.Pending != nil {
				t.Fatalf("개종 뒤 phase=%s pending=%v, want action/nil", g.Phase, g.Pending)
			}
		})
	}

	text := rfEventText(g)
	if !strings.Contains(text, "피난처") || !strings.Contains(text, "개종") {
		t.Fatalf("개종 이벤트 문구 부재:\n%s", text)
	}
}

// ==================== 횡령 ====================

// TestRFEmbezzle 횡령 — 도전이 없으면 국고 전액을 가져가고, 도전당하면
// 손패를 공개해 공작 부재를 증명한다. 공작이 있으면 도전자 승(횡령 취소),
// 없으면 도전자가 카드를 잃고 횡령이 성립하며 손패를 다시 뽑는다.
func TestRFEmbezzle(t *testing.T) {
	cases := []struct {
		name          string
		hand          [2]RFRole
		challenge     bool
		wantTreasury  int // 결말 시점 국고
		wantActorGain int // 행동자 칩 증가분 (횡령 성공 시 국고 전액)
		wantActorCard int // 행동자 남은 비공개 카드
		wantChalCard  int // 도전자 남은 비공개 카드
		wantRedraw    bool
	}{
		{"도전없음-전액획득", [2]RFRole{RFRoleCaptain, RFRoleContessa}, false, 0, 6, 2, 2, false},
		{"도전-공작없음-증명성공", [2]RFRole{RFRoleCaptain, RFRoleContessa}, true, 0, 6, 2, 1, true},
		{"도전-공작보유-도전자승", [2]RFRole{RFRoleDuke, RFRoleContessa}, true, 6, 0, 1, 2, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, rng := rfNewTestGame(t, 3)
			g.CurrentSeat = 0
			rfSetFactions(g, RFFactionLoyalist, RFFactionReformist, RFFactionReformist)
			rfSetCards(g, 0, tc.hand[0], tc.hand[1])
			rfSetCards(g, 1, RFRoleCaptain, RFRoleCaptain)
			rfSetCards(g, 2, RFRoleContessa, RFRoleContessa)
			g.Treasury = 6
			startChips := g.Players[0].Chips
			deckBefore := len(g.Deck)
			multisetBefore := rfRoleMultiset(g)

			if err := g.SubmitEmbezzle(0, rng); err != nil {
				t.Fatalf("SubmitEmbezzle: %v", err)
			}
			if g.Phase != RFPhaseChallengeWindow {
				t.Fatalf("phase = %s, want challenge_window (횡령은 도전 대상)", g.Phase)
			}
			if g.Pending.ClaimRole != rfEmbezzleClaim || g.Pending.BlockerSeat != -1 {
				t.Fatalf("pending = %+v", g.Pending)
			}

			if !tc.challenge {
				g.SubmitPass(1, rng)
				if g.Phase != RFPhaseChallengeWindow {
					t.Fatalf("한 명 통과만으로 창이 닫혔다: %s", g.Phase)
				}
				g.SubmitPass(2, rng)
			} else {
				g.SubmitChallenge(1, rng)
				text := rfEventText(g)
				if !strings.Contains(text, "손패를 모두 공개") {
					t.Fatalf("횡령 증명 공개 이벤트 부재:\n%s", text)
				}
				// 카드를 2장 가진 쪽이 잃을 카드를 고른다
				if g.Phase == RFPhaseLoseCard {
					if err := g.SubmitLoseCard(g.LoseSeat, 0, rng); err != nil {
						t.Fatalf("SubmitLoseCard: %v", err)
					}
				}
			}

			if g.Treasury != tc.wantTreasury {
				t.Fatalf("국고 = %d, want %d", g.Treasury, tc.wantTreasury)
			}
			if got := g.Players[0].Chips - startChips; got != tc.wantActorGain {
				t.Fatalf("횡령 획득 칩 = %d, want %d", got, tc.wantActorGain)
			}
			if got := len(g.Players[0].HiddenIdx()); got != tc.wantActorCard {
				t.Fatalf("행동자 카드 = %d장, want %d", got, tc.wantActorCard)
			}
			if got := len(g.Players[1].HiddenIdx()); got != tc.wantChalCard {
				t.Fatalf("도전자 카드 = %d장, want %d", got, tc.wantChalCard)
			}
			// 증명 후 재추첨: 덱 장수는 그대로고 카드 총합(멀티셋)도 보존된다
			if len(g.Deck) != deckBefore {
				t.Fatalf("덱 장수 = %d, want %d", len(g.Deck), deckBefore)
			}
			if tc.wantRedraw {
				for role, n := range multisetBefore {
					if rfRoleMultiset(g)[role] != n {
						t.Fatalf("재추첨 뒤 카드 구성이 깨졌다 (%s)", role)
					}
				}
			}
			if g.Phase != RFPhaseAction {
				t.Fatalf("횡령 결말 뒤 phase = %s, want action", g.Phase)
			}
		})
	}
}

// TestRFEmbezzleForcedCoup 칩 10개 이상이면 횡령·개종도 막히고 쿠만 남는다
func TestRFEmbezzleForcedCoup(t *testing.T) {
	g, rng := rfNewTestGame(t, 3)
	g.CurrentSeat = 0
	rfSetFactions(g, RFFactionLoyalist, RFFactionReformist, RFFactionReformist)
	g.Players[0].Chips = RFForceCoupChips
	g.Treasury = 5

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"횡령", func() error { return g.SubmitEmbezzle(0, rng) }},
		{"자기개종", func() error { return g.SubmitConvert(0, rng) }},
		{"남개종", func() error { return g.SubmitConvertOther(0, 1, rng) }},
		{"수입", func() error { return g.DeclareAction(0, RFActIncome, -1, rng) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil || !strings.Contains(err.Error(), "쿠데타만") {
				t.Fatalf("쿠 강제 미적용: %v", err)
			}
		})
	}
	if err := g.DeclareAction(0, RFActCoup, 1, rng); err != nil {
		t.Fatalf("쿠 강제 상태에서 쿠가 거부됐다: %v", err)
	}
}

// ==================== 진영 승리 ====================

// TestRFFactionVictory 진영 승리 판정 — 탈락으로도, 개종으로도 성립한다.
// 마지막 1명만 남으면 좌석 승리(winner="seat")다.
func TestRFFactionVictory(t *testing.T) {
	cases := []struct {
		name        string
		players     int
		factions    []RFFaction
		run         func(g *RFGame, rng *rand.Rand)
		wantWinner  string
		wantSeats   []int
		wantMsgPart string
	}{
		{
			name:     "탈락으로_충성파_승리",
			players:  3,
			factions: []RFFaction{RFFactionLoyalist, RFFactionLoyalist, RFFactionReformist},
			run: func(g *RFGame, rng *rand.Rand) {
				// seat2(유일한 개혁파)를 두 번 쿠로 탈락시킨다
				rfSetCards(g, 2, RFRoleDuke, RFRoleDuke)
				g.CurrentSeat = 0
				g.Players[0].Chips = RFCoupCost
				g.DeclareAction(0, RFActCoup, 2, rng)
				g.SubmitLoseCard(2, 0, rng)
				g.CurrentSeat = 1
				g.Players[1].Chips = RFCoupCost
				g.DeclareAction(1, RFActCoup, 2, rng)
			},
			wantWinner:  string(RFFactionLoyalist),
			wantSeats:   []int{0, 1},
			wantMsgPart: "충성파 진영 승리",
		},
		{
			name:     "자기개종으로_개혁파_승리",
			players:  2,
			factions: []RFFaction{RFFactionLoyalist, RFFactionReformist},
			run: func(g *RFGame, rng *rand.Rand) {
				g.CurrentSeat = 0
				g.Players[0].Chips = 3
				g.SubmitConvert(0, rng)
			},
			wantWinner:  string(RFFactionReformist),
			wantSeats:   []int{0, 1},
			wantMsgPart: "개혁파 진영 승리",
		},
		{
			name:     "남개종으로_충성파_승리",
			players:  3,
			factions: []RFFaction{RFFactionLoyalist, RFFactionLoyalist, RFFactionReformist},
			run: func(g *RFGame, rng *rand.Rand) {
				g.CurrentSeat = 0
				g.Players[0].Chips = RFConvertOtherCost
				g.SubmitConvertOther(0, 2, rng)
			},
			wantWinner:  string(RFFactionLoyalist),
			wantSeats:   []int{0, 1, 2},
			wantMsgPart: "충성파 진영 승리",
		},
		{
			name:     "최후1인_좌석승리",
			players:  2,
			factions: []RFFaction{RFFactionLoyalist, RFFactionReformist},
			run: func(g *RFGame, rng *rand.Rand) {
				rfSetCards(g, 1, RFRoleDuke, RFRoleDuke)
				g.CurrentSeat = 0
				g.Players[0].Chips = RFCoupCost
				g.DeclareAction(0, RFActCoup, 1, rng)
				g.SubmitLoseCard(1, 0, rng)
				g.CurrentSeat = 0
				g.Players[0].Chips = RFCoupCost
				g.DeclareAction(0, RFActCoup, 1, rng)
			},
			wantWinner:  "seat",
			wantSeats:   []int{0},
			wantMsgPart: "최후의 1인",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, rng := rfNewTestGame(t, tc.players)
			rfSetFactions(g, tc.factions...)
			tc.run(g, rng)

			if g.Phase != RFPhaseGameOver {
				t.Fatalf("phase = %s, want game_over", g.Phase)
			}
			if g.Result == nil {
				t.Fatal("result 부재")
			}
			if g.Result.Winner != tc.wantWinner {
				t.Fatalf("winner = %q, want %q", g.Result.Winner, tc.wantWinner)
			}
			if len(g.Result.WinnerSeats) != len(tc.wantSeats) {
				t.Fatalf("winnerSeats = %v, want %v", g.Result.WinnerSeats, tc.wantSeats)
			}
			for i, s := range tc.wantSeats {
				if g.Result.WinnerSeats[i] != s {
					t.Fatalf("winnerSeats = %v, want %v", g.Result.WinnerSeats, tc.wantSeats)
				}
			}
			if len(g.Result.WinnerNames) != len(tc.wantSeats) {
				t.Fatalf("winnerNames = %v", g.Result.WinnerNames)
			}
			if !strings.Contains(g.Result.Message, tc.wantMsgPart) {
				t.Fatalf("결과 문구 = %q, want %q 포함", g.Result.Message, tc.wantMsgPart)
			}
			if g.CurrentSeat != -1 || g.Pending != nil {
				t.Fatalf("종료 뒤 정리 실패: current=%d pending=%v", g.CurrentSeat, g.Pending)
			}
		})
	}
}

// ==================== 기본 쿠 규칙 (이식 검증) ====================

// TestRFChallengeOutcomes 도전 실패/성공 — 주장자가 실제 보유하면 카드를
// 교체하고 도전자가 잃으며, 블러핑이면 주장자가 잃고 액션이 취소된다.
func TestRFChallengeOutcomes(t *testing.T) {
	cases := []struct {
		name       string
		actorCards [2]RFRole
		wantText   string
		wantLose   int // 카드를 잃는 좌석
		wantChips  int // 결말 시점 행동자 칩
	}{
		{"도전실패-세금성립", [2]RFRole{RFRoleDuke, RFRoleAssassin}, "도전 실패", 1, RFStartChips + 3},
		{"도전성공-세금취소", [2]RFRole{RFRoleAssassin, RFRoleContessa}, "도전 성공", 0, RFStartChips},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, rng := rfNewTestGame(t, 3)
			g.CurrentSeat = 0
			rfSetFactions(g, RFFactionLoyalist, RFFactionReformist, RFFactionReformist)
			rfSetCards(g, 0, tc.actorCards[0], tc.actorCards[1])
			rfSetCards(g, 1, RFRoleCaptain, RFRoleCaptain)
			rfSetCards(g, 2, RFRoleContessa, RFRoleContessa)

			if err := g.DeclareAction(0, RFActTax, -1, rng); err != nil {
				t.Fatalf("DeclareAction(tax): %v", err)
			}
			g.SubmitChallenge(1, rng)
			if text := rfEventText(g); !strings.Contains(text, tc.wantText) {
				t.Fatalf("%s 이벤트 부재:\n%s", tc.wantText, text)
			}
			if g.Phase != RFPhaseLoseCard || g.LoseSeat != tc.wantLose {
				t.Fatalf("phase=%s loseSeat=%d, want lose_card seat%d", g.Phase, g.LoseSeat, tc.wantLose)
			}
			if err := g.SubmitLoseCard(tc.wantLose, 0, rng); err != nil {
				t.Fatalf("SubmitLoseCard: %v", err)
			}
			if g.Players[0].Chips != tc.wantChips {
				t.Fatalf("행동자 칩 = %d, want %d", g.Players[0].Chips, tc.wantChips)
			}
			if g.Phase != RFPhaseAction || g.CurrentSeat != 1 {
				t.Fatalf("턴 전환 실패: phase=%s current=%d", g.Phase, g.CurrentSeat)
			}
		})
	}
}

// TestRFBlockAndBlockChallenge 차단 성립·차단 도전. 차단 도전 창은 별도
// phase 없이 challenge_window 를 재사용하며 pending.blockerSeat 으로 구분된다.
func TestRFBlockAndBlockChallenge(t *testing.T) {
	g, rng := rfNewTestGame(t, 3)
	rfSetFactions(g, RFFactionLoyalist, RFFactionReformist, RFFactionReformist)
	rfSetCards(g, 0, RFRoleDuke, RFRoleAssassin)
	rfSetCards(g, 1, RFRoleCaptain, RFRoleCaptain)
	rfSetCards(g, 2, RFRoleContessa, RFRoleContessa) // 공작 없음 — 블러핑 차단용

	// ---- 차단 성립 (아무도 도전하지 않음) ----
	g.CurrentSeat = 0
	if err := g.DeclareAction(0, RFActAid, -1, rng); err != nil {
		t.Fatalf("DeclareAction(aid): %v", err)
	}
	if g.Phase != RFPhaseBlockWindow {
		t.Fatalf("phase = %s, want block_window (해외원조는 도전 창 없음)", g.Phase)
	}
	if err := g.SubmitBlock(1, RFRoleDuke); err != nil {
		t.Fatalf("SubmitBlock: %v", err)
	}
	if g.Phase != RFPhaseChallengeWindow || !g.blockChallenge() {
		t.Fatalf("차단 도전 창 미개방: phase=%s blocker=%d", g.Phase, g.Pending.BlockerSeat)
	}
	g.SubmitPass(0, rng)
	if g.Phase != RFPhaseChallengeWindow {
		t.Fatalf("한 명 통과만으로 창이 닫혔다: %s", g.Phase)
	}
	g.SubmitPass(2, rng)
	if g.Players[0].Chips != RFStartChips {
		t.Fatalf("차단됐는데 원조를 받았다: chips=%d", g.Players[0].Chips)
	}
	if !strings.Contains(rfEventText(g), "저지가 통과") {
		t.Fatal("차단 통과 이벤트 부재")
	}

	// ---- 블러핑 차단이 도전에 무너지면 액션이 해결된다 ----
	g.CurrentSeat = 1
	g.Phase = RFPhaseAction
	if err := g.DeclareAction(1, RFActAid, -1, rng); err != nil {
		t.Fatalf("DeclareAction(aid#2): %v", err)
	}
	if err := g.SubmitBlock(2, RFRoleDuke); err != nil { // seat2 블러핑
		t.Fatalf("SubmitBlock#2: %v", err)
	}
	g.SubmitChallenge(0, rng)
	if g.Phase != RFPhaseLoseCard || g.LoseSeat != 2 {
		t.Fatalf("phase=%s loseSeat=%d, want lose_card seat2", g.Phase, g.LoseSeat)
	}
	if err := g.SubmitLoseCard(2, 0, rng); err != nil {
		t.Fatalf("SubmitLoseCard: %v", err)
	}
	if g.Players[1].Chips != RFStartChips+2 {
		t.Fatalf("차단 무효인데 원조 미지급: chips=%d", g.Players[1].Chips)
	}
}

// TestRFStealAssassinExchange 강탈 상한·거짓 암살 비용 반환·교환 선택
func TestRFStealAssassinExchange(t *testing.T) {
	g, rng := rfNewTestGame(t, 3)
	rfSetFactions(g, RFFactionLoyalist, RFFactionReformist, RFFactionReformist)
	rfSetCards(g, 0, RFRoleCaptain, RFRoleAmbassador)
	rfSetCards(g, 1, RFRoleDuke, RFRoleDuke)
	rfSetCards(g, 2, RFRoleDuke, RFRoleDuke)
	g.CurrentSeat = 0
	g.Players[1].Chips = 1 // 강탈 대상 칩 부족

	if err := g.DeclareAction(0, RFActSteal, 1, rng); err != nil {
		t.Fatalf("DeclareAction(steal): %v", err)
	}
	g.SubmitPass(1, rng)
	g.SubmitPass(2, rng)
	if g.Phase != RFPhaseBlockWindow {
		t.Fatalf("phase = %s, want block_window", g.Phase)
	}
	g.SubmitPass(1, rng)
	g.SubmitPass(2, rng)
	if g.Players[0].Chips != RFStartChips+1 || g.Players[1].Chips != 0 {
		t.Fatalf("강탈 결과: actor=%d target=%d, want +1/0", g.Players[0].Chips, g.Players[1].Chips)
	}

	// 거짓 암살이 도전에 무너지면 3칩을 돌려받는다
	g.CurrentSeat = 2
	g.Phase = RFPhaseAction
	g.Players[2].Chips = RFAssassinCost
	if err := g.DeclareAction(2, RFActAssassinate, 0, rng); err != nil {
		t.Fatalf("DeclareAction(assassinate): %v", err)
	}
	g.SubmitChallenge(0, rng)
	if g.Phase != RFPhaseLoseCard || g.LoseSeat != 2 {
		t.Fatalf("phase=%s loseSeat=%d, want lose_card seat2", g.Phase, g.LoseSeat)
	}
	if g.Players[2].Chips != RFAssassinCost {
		t.Fatalf("거짓 암살 취소 시 비용 미반환: chips=%d", g.Players[2].Chips)
	}
	if err := g.SubmitLoseCard(2, 0, rng); err != nil {
		t.Fatalf("SubmitLoseCard: %v", err)
	}

	// 교환 — 덱 2장을 뽑아 4장 중 2장 유지 (덱 장수 불변)
	g.CurrentSeat = 0
	g.Phase = RFPhaseAction
	g.Deck = []RFRole{RFRoleContessa, RFRoleAssassin, RFRoleDuke, RFRoleDuke, RFRoleDuke}
	if err := g.DeclareAction(0, RFActExchange, -1, rng); err != nil {
		t.Fatalf("DeclareAction(exchange): %v", err)
	}
	g.SubmitPass(1, rng)
	g.SubmitPass(2, rng)
	if g.Phase != RFPhaseExchange {
		t.Fatalf("phase = %s, want exchange", g.Phase)
	}
	want := []RFRole{RFRoleCaptain, RFRoleAmbassador, RFRoleContessa, RFRoleAssassin}
	for i, r := range want {
		if g.ExchangeCards[i] != r {
			t.Fatalf("교환 선택지[%d] = %s, want %s", i, g.ExchangeCards[i], r)
		}
	}
	if err := g.SubmitExchange(0, []int{0}, rng); err == nil {
		t.Fatal("1장 유지가 허용됐다 (2장 필요)")
	}
	if err := g.SubmitExchange(0, []int{1, 1}, rng); err == nil {
		t.Fatal("중복 인덱스가 허용됐다")
	}
	if err := g.SubmitExchange(1, []int{0, 1}, rng); err == nil {
		t.Fatal("타인의 교환 선택이 허용됐다")
	}
	if err := g.SubmitExchange(0, []int{2, 3}, rng); err != nil {
		t.Fatalf("SubmitExchange: %v", err)
	}
	roles := g.Players[0].HiddenRoles()
	if len(roles) != 2 || roles[0] != RFRoleContessa || roles[1] != RFRoleAssassin {
		t.Fatalf("교환 후 손패 = %v", roles)
	}
	if len(g.Deck) != 5 {
		t.Fatalf("덱 장수 = %d, want 5 (2장 반납)", len(g.Deck))
	}
}

// TestRFAutoAttackTargetSkipsAllies 자동 진행·봇 공용 대상 선정은 같은
// 진영을 건너뛴다
func TestRFAutoAttackTargetSkipsAllies(t *testing.T) {
	g, _ := rfNewTestGame(t, 4)
	rfSetFactions(g, RFFactionLoyalist, RFFactionLoyalist, RFFactionReformist, RFFactionReformist)
	g.Players[2].Cards[0].Revealed = true // 카드 1장
	g.Players[3].Chips = 9                // 카드 2장·최다 칩

	if got := g.AutoAttackTarget(0); got != 3 {
		t.Fatalf("대상 = seat%d, want seat3 (최다 카드·최다 칩 적)", got)
	}
	// 상대 진영을 전부 탈락시키면 대상이 없다
	g.Players[2].Cards[1].Revealed = true
	g.Players[3].Cards[0].Revealed = true
	g.Players[3].Cards[1].Revealed = true
	if got := g.AutoAttackTarget(0); got != -1 {
		t.Fatalf("대상 = seat%d, want -1", got)
	}
}
