package server

import (
	"fmt"
	"math/rand"
	"testing"
)

// mtTestGame 5명이 앉은 시작 전 게임
func mtTestGame() *MTGame {
	g := NewMTGame("mt-test")
	for i := 0; i < MTPlayerCount; i++ {
		g.AddPlayer(fmt.Sprintf("P%d", i))
	}
	return g
}

// tp 트릭 플레이 축약
func tp(seat int, card string) MTTrickPlay { return MTTrickPlay{Seat: seat, Card: card} }

// ==================== 입찰 우열 ====================

func TestMTBidBeats(t *testing.T) {
	cases := []struct {
		name  string
		suit  string
		count int
		best  *MTBid
		want  bool
	}{
		{"첫 입찰은 무조건 성립", "S", 13, nil, true},
		{"count 큰 쪽 우위", "D", 14, &MTBid{Seat: 0, Suit: "S", Count: 13}, true},
		{"같은 count 같은 급은 무효", "D", 13, &MTBid{Seat: 0, Suit: "S", Count: 13}, false},
		{"같은 count 노기루만 우위", "N", 13, &MTBid{Seat: 0, Suit: "S", Count: 13}, true},
		{"노기루끼리 같은 count 는 무효", "N", 13, &MTBid{Seat: 0, Suit: "N", Count: 13}, false},
		{"낮은 count 는 무효", "N", 12, &MTBid{Seat: 0, Suit: "S", Count: 13}, false},
	}
	for _, c := range cases {
		if got := mtBidBeats(c.suit, c.count, c.best); got != c.want {
			t.Errorf("%s: mtBidBeats(%s,%d) = %v, want %v", c.name, c.suit, c.count, got, c.want)
		}
	}

	if mtBidRangeValid("S", 12) {
		t.Error("기루다 12 는 최소 미달이어야 한다")
	}
	if !mtBidRangeValid("N", 12) {
		t.Error("노기루는 12부터 허용이어야 한다")
	}
	if mtBidRangeValid("N", 11) || mtBidRangeValid("S", 21) {
		t.Error("범위 밖 공약이 통과됐다")
	}
}

// TestMTBiddingFlow seat1 입찰 후 나머지 전원 패스 → 낙찰·키티 합류(13장)
func TestMTBiddingFlow(t *testing.T) {
	g := mtTestGame()
	if err := g.Start(rand.New(rand.NewSource(1))); err != nil {
		t.Fatal(err)
	}

	if _, err := g.Pass(0); err != nil {
		t.Fatal(err)
	}
	step, err := g.Bid(1, "H", 13)
	if err != nil || step.Awarded {
		t.Fatalf("bid: err=%v awarded=%v", err, step.Awarded)
	}
	// 패스한 좌석은 재입찰 불가
	if _, err := g.Bid(0, "S", 14); err == nil {
		t.Fatal("패스한 좌석의 재입찰이 허용됐다")
	}
	for _, seat := range []int{2, 3, 4} {
		step, err = g.Pass(seat)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !step.Awarded {
		t.Fatal("전원 패스 후에도 낙찰되지 않았다")
	}
	if g.Phase != MTPhaseKitty || g.Declarer != 1 || g.Trump != "H" || g.ContractCount != 13 {
		t.Fatalf("낙찰 상태 이상: phase=%s declarer=%d trump=%s count=%d",
			g.Phase, g.Declarer, g.Trump, g.ContractCount)
	}
	if len(g.Hands[1]) != MTHandSize+MTKittySize {
		t.Fatalf("주공 손패 %d장, want 13", len(g.Hands[1]))
	}
}

// TestMTBiddingAllPassRedeal 전원 패스 → Redeal 신호, 재배분 후 입찰 초기화
func TestMTBiddingAllPassRedeal(t *testing.T) {
	g := mtTestGame()
	rng := rand.New(rand.NewSource(2))
	if err := g.Start(rng); err != nil {
		t.Fatal(err)
	}
	var step mtBidStep
	for seat := 0; seat < MTPlayerCount; seat++ {
		var err error
		step, err = g.Pass(seat)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !step.Redeal {
		t.Fatal("전원 패스가 재배분 신호를 내지 않았다")
	}
	g.Redeal(rng)
	if g.RedealCount != 1 || g.Phase != MTPhaseBidding || g.BidTurn != 0 || len(g.Passed) != 0 {
		t.Fatalf("재배분 상태 이상: redeals=%d phase=%s turn=%d passed=%v",
			g.RedealCount, g.Phase, g.BidTurn, g.Passed)
	}
	for seat := 0; seat < MTPlayerCount; seat++ {
		if len(g.Hands[seat]) != MTHandSize {
			t.Fatalf("재배분 후 seat%d 손패 %d장", seat, len(g.Hands[seat]))
		}
	}
}

// ==================== 트릭 승자 ====================

func TestMTTrickWinner(t *testing.T) {
	cases := []struct {
		name      string
		plays     []MTTrickPlay
		ledSuit   string
		trump     string
		trickNo   int
		jokerCall bool
		want      int // 승자 좌석
	}{
		{"마이티 > 조커 > 기루다", []MTTrickPlay{tp(0, "H10"), tp(1, "D14"), tp(2, "JK"), tp(3, "S14"), tp(4, "H14")}, "H", "D", 5, false, 3},
		{"조커 > 기루다 최고", []MTTrickPlay{tp(0, "H10"), tp(1, "D14"), tp(2, "JK"), tp(3, "D13"), tp(4, "H14")}, "H", "D", 5, false, 2},
		{"기루다 > 리드 문양", []MTTrickPlay{tp(0, "H10"), tp(1, "D2"), tp(2, "H14"), tp(3, "C14"), tp(4, "H13")}, "H", "D", 5, false, 1},
		{"리드 문양 최고", []MTTrickPlay{tp(0, "H10"), tp(1, "H12"), tp(2, "H14"), tp(3, "C14"), tp(4, "H13")}, "H", "D", 5, false, 2},
		{"첫 트릭 조커 최약", []MTTrickPlay{tp(0, "H10"), tp(1, "JK"), tp(2, "H14"), tp(3, "H2"), tp(4, "H13")}, "H", "N", 1, false, 2},
		{"막(10) 트릭 조커 최약", []MTTrickPlay{tp(0, "H10"), tp(1, "JK"), tp(2, "H14"), tp(3, "H2"), tp(4, "H13")}, "H", "N", 10, false, 2},
		{"조커콜 시 조커 최약", []MTTrickPlay{tp(0, "C3"), tp(1, "JK"), tp(2, "C10"), tp(3, "C2"), tp(4, "C5")}, "C", "H", 4, true, 2},
		{"기루다 스페이드면 마이티=D14", []MTTrickPlay{tp(0, "S14"), tp(1, "D14"), tp(2, "S13"), tp(3, "S2"), tp(4, "S10")}, "S", "S", 5, false, 1},
		{"노기루면 기루다 단계 생략", []MTTrickPlay{tp(0, "H10"), tp(1, "D14"), tp(2, "H11"), tp(3, "D13"), tp(4, "H13")}, "H", "N", 5, false, 4},
		{"마이티는 첫 트릭도 최강", []MTTrickPlay{tp(0, "H10"), tp(1, "S14"), tp(2, "H14"), tp(3, "H2"), tp(4, "H13")}, "H", "D", 1, false, 1},
	}
	for _, c := range cases {
		idx := mtTrickWinner(c.plays, c.ledSuit, c.trump, c.trickNo, c.jokerCall)
		if c.plays[idx].Seat != c.want {
			t.Errorf("%s: 승자 seat%d, want seat%d", c.name, c.plays[idx].Seat, c.want)
		}
	}
}

// ==================== 팔로우 합법성 ====================

func TestMTLegalPlays(t *testing.T) {
	hand := []string{"JK", "S14", "S2", "H10", "C5"}

	// 리드 문양 팔로우 의무 — 단 마이티·조커는 예외
	legal := mtLegalPlays(hand, false, "H", false, "D")
	want := map[string]bool{"H10": true, "JK": true, "S14": true}
	if len(legal) != len(want) {
		t.Fatalf("legal = %v, want H10·JK·S14", legal)
	}
	for _, c := range legal {
		if !want[c] {
			t.Fatalf("불법 카드 허용: %s (%v)", c, legal)
		}
	}

	// 리드 문양이 없으면 아무거나
	legal = mtLegalPlays([]string{"S2", "C5"}, false, "H", false, "D")
	if len(legal) != 2 {
		t.Fatalf("보이드인데 제한됨: %v", legal)
	}

	// 조커콜 발동 중 조커 소지자는 조커 강제
	legal = mtLegalPlays(hand, false, "C", true, "D")
	if len(legal) != 1 || legal[0] != MTJoker {
		t.Fatalf("조커콜 강제 실패: %v", legal)
	}

	// 조커콜이라도 조커가 없으면 일반 팔로우
	legal = mtLegalPlays([]string{"C5", "H10"}, false, "C", true, "D")
	if len(legal) != 1 || legal[0] != "C5" {
		t.Fatalf("조커 미소지 조커콜 팔로우 실패: %v", legal)
	}

	// 리드는 아무 카드나
	legal = mtLegalPlays(hand, true, "", false, "D")
	if len(legal) != len(hand) {
		t.Fatalf("리드 제한됨: %v", legal)
	}

	// 기루다 스페이드면 마이티는 D14
	legal = mtLegalPlays([]string{"D14", "S2", "C5"}, false, "C", false, "S")
	found := false
	for _, c := range legal {
		if c == "D14" {
			found = true
		}
	}
	if !found {
		t.Fatalf("기루다 S 에서 마이티 D14 예외 누락: %v", legal)
	}
}

// ==================== 키티 ====================

func TestMTKittyTrumpChangeAndPoints(t *testing.T) {
	g := mtTestGame()
	g.Ready = true
	g.Phase = MTPhaseKitty
	g.Declarer = 2
	g.Trump = "S"
	g.ContractCount = 13
	g.Hands[2] = []string{"S14", "S13", "S12", "S11", "S10", "H2", "H3", "H4", "D5", "D6", "C7", "C10", "C14"}

	// 점수카드 2장(C10·C14) 포함 3장 버림 + 기루다 변경 → count+1
	if err := g.SubmitKitty(2, []string{"C7", "C10", "C14"}, "H"); err != nil {
		t.Fatal(err)
	}
	if g.Trump != "H" || g.ContractCount != 14 {
		t.Fatalf("기루다 변경 반영 이상: trump=%s count=%d", g.Trump, g.ContractCount)
	}
	if g.KittyPoints != 2 {
		t.Fatalf("키티 점수 %d, want 2", g.KittyPoints)
	}
	if len(g.Hands[2]) != 10 {
		t.Fatalf("버림 후 손패 %d장", len(g.Hands[2]))
	}
	if g.Phase != MTPhaseFriend {
		t.Fatalf("phase = %s, want friend", g.Phase)
	}

	// 기루다 유지면 count 그대로
	g2 := mtTestGame()
	g2.Ready = true
	g2.Phase = MTPhaseKitty
	g2.Declarer = 0
	g2.Trump = "S"
	g2.ContractCount = 13
	g2.Hands[0] = []string{"S14", "S2", "S3", "H2", "H3", "H4", "D5", "D6", "C7", "C8", "C9", "D2", "D3"}
	if err := g2.SubmitKitty(0, []string{"C7", "C8", "C9"}, "S"); err != nil {
		t.Fatal(err)
	}
	if g2.ContractCount != 13 {
		t.Fatalf("기루다 유지인데 count=%d", g2.ContractCount)
	}
}

// ==================== 프렌드 ====================

func TestMTFriendSelfCardBecomesNone(t *testing.T) {
	g := mtTestGame()
	g.Ready = true
	g.Phase = MTPhaseFriend
	g.Declarer = 0
	g.Trump = "H"
	g.Hands[0] = []string{"S14", "H2"}

	if err := g.SetFriend(0, "card", "S14"); err != nil {
		t.Fatal(err)
	}
	if g.FriendType != "none" || !g.FriendRevealed || g.FriendSeat != -1 {
		t.Fatalf("본인 카드 지정이 노프렌드가 아님: type=%s revealed=%v seat=%d",
			g.FriendType, g.FriendRevealed, g.FriendSeat)
	}
	if g.Phase != MTPhasePlay || g.Turn != 0 || g.TrickNo != 1 {
		t.Fatalf("플레이 진입 이상: phase=%s turn=%d trickNo=%d", g.Phase, g.Turn, g.TrickNo)
	}
}

// mtSetupPlay 플레이 단계 게임 조립 (트릭 1, 주공 0 선공)
func mtSetupPlay(trump string, hands map[int][]string) *MTGame {
	g := mtTestGame()
	g.Ready = true
	g.Phase = MTPhasePlay
	g.Declarer = 0
	g.Trump = trump
	g.ContractCount = 13
	g.FriendType = "none"
	g.FriendRevealed = true
	g.TrickNo = 1
	g.Turn = 0
	for seat, hand := range hands {
		g.Hands[seat] = append([]string{}, hand...)
	}
	return g
}

func TestMTFriendRevealedOnCardPlay(t *testing.T) {
	g := mtSetupPlay("D", map[int][]string{
		0: {"H10"}, 1: {"H12"}, 2: {"S14"}, 3: {"H2"}, 4: {"H13"},
	})
	g.FriendType = "card"
	g.FriendCard = "S14"
	g.FriendRevealed = false
	g.FriendSeat = -1

	for _, m := range []struct {
		seat int
		card string
	}{{0, "H10"}, {1, "H12"}} {
		if _, err := g.Play(m.seat, m.card, false, ""); err != nil {
			t.Fatal(err)
		}
	}
	step, err := g.Play(2, "S14", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if !step.FriendRevealed || !g.FriendRevealed || g.FriendSeat != 2 {
		t.Fatalf("프렌드 공개 실패: step=%v seat=%d", step.FriendRevealed, g.FriendSeat)
	}
}

// TestMTJokerCallForcesJoker 조커콜 리드 → 조커 소지자는 다른 카드를 낼 수 없다
func TestMTJokerCallForcesJoker(t *testing.T) {
	g := mtSetupPlay("H", map[int][]string{
		0: {"C3", "C4"}, 1: {"JK", "C5"}, 2: {"C6", "C7"}, 3: {"C8", "C9"}, 4: {"C10", "C11"},
	})
	// 첫 트릭에는 조커콜 무효 — 먼저 첫 트릭을 소화한다
	for _, m := range []struct {
		seat int
		card string
	}{{0, "C4"}, {1, "C5"}, {2, "C6"}, {3, "C8"}, {4, "C10"}} {
		if _, err := g.Play(m.seat, m.card, false, ""); err != nil {
			t.Fatal(err)
		}
	}
	if g.TrickNo != 2 || g.Turn != 4 { // C10 이 리드 최고
		t.Fatalf("트릭1 정리 이상: trickNo=%d turn=%d", g.TrickNo, g.Turn)
	}
	// 트릭2: seat4 리드 → ... 조커콜은 조커콜 카드 소지자(0)가 리드할 때만 가능.
	// 순서를 맞추기 위해 seat4→0 진행 후 0의 리드 트릭에서 검증하는 대신,
	// 남은 카드로 트릭2를 seat4 가 리드하고 0 이 조커콜 카드를 내는 상황은
	// 리드가 아니므로 조커콜 미발동임을 확인한다.
	if _, err := g.Play(4, "C11", false, ""); err != nil {
		t.Fatal(err)
	}
	step, err := g.Play(0, "C3", true, "") // 리드가 아닌 조커콜 선언은 무시된다
	if err != nil {
		t.Fatal(err)
	}
	if step.JokerCall || g.JokerCallActive {
		t.Fatal("리드가 아닌 조커콜이 발동됐다")
	}
	// 조커 소지자 seat1: 조커콜 미발동이므로 팔로우 자유 (C5 는 이미 냈으니 JK 도 합법)
	if _, err := g.Play(1, "JK", false, ""); err != nil {
		t.Fatal(err)
	}

	// 별도 게임으로 리드 조커콜 강제를 검증
	g2 := mtSetupPlay("H", map[int][]string{
		0: {"C3", "C4"}, 1: {"JK", "C5"}, 2: {"C6", "C7"}, 3: {"C8", "C9"}, 4: {"C10", "C11"},
	})
	g2.TrickNo = 2 // 첫 트릭 아님
	step, err = g2.Play(0, "C3", true, "")
	if err != nil {
		t.Fatal(err)
	}
	if !step.JokerCall || !g2.JokerCallActive {
		t.Fatal("조커콜이 발동되지 않았다")
	}
	if _, err := g2.Play(1, "C5", false, ""); err == nil {
		t.Fatal("조커콜인데 조커 아닌 카드가 허용됐다")
	}
	if _, err := g2.Play(1, "JK", false, ""); err != nil {
		t.Fatalf("조커콜 준수 플레이 실패: %v", err)
	}
}

// ==================== 정산 ====================

// mtPlayLastTrick 마지막(10) 트릭 한 판을 돌려 정산까지 간다
func mtPlayLastTrick(t *testing.T, g *MTGame, plays []MTTrickPlay) {
	t.Helper()
	g.TrickNo = MTTrickCount
	g.Turn = plays[0].Seat
	for i, p := range plays {
		step, err := g.Play(p.Seat, p.Card, false, "")
		if err != nil {
			t.Fatalf("play %d (%v): %v", i, p, err)
		}
		if i == len(plays)-1 && !step.GameOver {
			t.Fatal("10트릭 종료 후 게임이 끝나지 않았다")
		}
	}
}

func TestMTSettleWithKittyAndFriend(t *testing.T) {
	g := mtSetupPlay("D", map[int][]string{
		0: {"D14"}, 1: {"H10"}, 2: {"H11"}, 3: {"H12"}, 4: {"H13"},
	})
	g.ContractCount = 14
	g.KittyPoints = 2 // 키티 버림 점수 2장 → 주공팀 귀속
	g.FriendType = "card"
	g.FriendCard = "S14"
	g.FriendRevealed = true
	g.FriendSeat = 2
	// 9트릭까지의 누적: 주공 6, 프렌드 2, 수비 4 (합 12 + 키티 2 + 막트릭)
	g.CapturedPoints = map[int]int{0: 6, 1: 2, 2: 2, 3: 1, 4: 1}
	g.TricksWon = map[int]int{0: 5, 1: 1, 2: 1, 3: 1, 4: 1}

	// 막 트릭: 주공이 기루다로 점수 5장 획득 (D14·H10·H11·H12·H13)
	mtPlayLastTrick(t, g, []MTTrickPlay{tp(0, "D14"), tp(1, "H10"), tp(2, "H11"), tp(3, "H12"), tp(4, "H13")})

	r := g.Result
	if r == nil {
		t.Fatal("result 없음")
	}
	// 주공팀 = 키티2 + 주공(6+5) + 프렌드2 = 15
	if r.DeclarerPoints != 15 || r.DefenderPoints != 5 {
		t.Fatalf("점수 이상: 주공팀=%d 수비팀=%d", r.DeclarerPoints, r.DefenderPoints)
	}
	if !r.Win {
		t.Fatal("공약 14 에 15점인데 실패 처리됐다")
	}
	if r.FriendSeat != 2 || len(r.DeclarerTeam) != 2 || len(r.DefenderTeam) != 3 {
		t.Fatalf("팀 구성 이상: friend=%d decl=%v def=%v", r.FriendSeat, r.DeclarerTeam, r.DefenderTeam)
	}
}

// TestMTSettleFriendNeverRevealed 프렌드 카드가 묻혀 미공개면 주공 단독 정산 (교착 없음)
func TestMTSettleFriendNeverRevealed(t *testing.T) {
	g := mtSetupPlay("D", map[int][]string{
		0: {"D14"}, 1: {"H10"}, 2: {"H2"}, 3: {"H3"}, 4: {"H4"},
	})
	g.ContractCount = 13
	g.KittyPoints = 1
	g.FriendType = "card"
	g.FriendCard = "C10" // 키티에 버려져 아무도 내지 못하는 카드
	g.FriendRevealed = false
	g.FriendSeat = -1
	g.CapturedPoints = map[int]int{0: 8, 1: 3, 2: 3, 3: 2, 4: 2}

	mtPlayLastTrick(t, g, []MTTrickPlay{tp(0, "D14"), tp(1, "H10"), tp(2, "H2"), tp(3, "H3"), tp(4, "H4")})

	r := g.Result
	if r == nil {
		t.Fatal("result 없음")
	}
	if r.FriendSeat != -1 || len(r.DeclarerTeam) != 1 {
		t.Fatalf("미공개 프렌드가 팀에 들어갔다: friend=%d decl=%v", r.FriendSeat, r.DeclarerTeam)
	}
	// 주공 8 + 막트릭 2(D14·H10) + 키티 1 = 11 < 13 → 실패
	if r.DeclarerPoints != 11 || r.Win {
		t.Fatalf("정산 이상: 점수=%d win=%v", r.DeclarerPoints, r.Win)
	}
}

// TestMTFirstTrickFriend 첫 트릭 승자가 프렌드로 공개된다
func TestMTFirstTrickFriend(t *testing.T) {
	g := mtSetupPlay("N", map[int][]string{
		0: {"H2", "S2"}, 1: {"H14", "S3"}, 2: {"H3", "S4"}, 3: {"H4", "S5"}, 4: {"H5", "S6"},
	})
	g.FriendType = "first_trick"
	g.FriendRevealed = false
	g.FriendSeat = -1

	for _, m := range []struct {
		seat int
		card string
	}{{0, "H2"}, {1, "H14"}, {2, "H3"}, {3, "H4"}} {
		if _, err := g.Play(m.seat, m.card, false, ""); err != nil {
			t.Fatal(err)
		}
	}
	step, err := g.Play(4, "H5", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if !step.TrickDone || step.TrickWinner != 1 {
		t.Fatalf("트릭 승자 이상: %+v", step)
	}
	if !step.FriendRevealed || g.FriendSeat != 1 || !g.FriendRevealed {
		t.Fatalf("첫 트릭 프렌드 공개 실패: seat=%d", g.FriendSeat)
	}
}

// TestMTJokerLeadNeedsSuit 조커 리드는 문양 지정이 필수이며 그 문양이 리드가 된다
func TestMTJokerLeadNeedsSuit(t *testing.T) {
	g := mtSetupPlay("D", map[int][]string{
		0: {"JK", "S2"}, 1: {"H10", "S3"}, 2: {"H3", "S4"}, 3: {"H4", "S5"}, 4: {"H5", "S6"},
	})
	g.TrickNo = 3
	if _, err := g.Play(0, "JK", false, ""); err == nil {
		t.Fatal("문양 없는 조커 리드가 허용됐다")
	}
	if _, err := g.Play(0, "JK", false, "H"); err != nil {
		t.Fatal(err)
	}
	if g.LedSuit != "H" {
		t.Fatalf("조커 리드 문양 미반영: %s", g.LedSuit)
	}
	// 하트 소지자는 팔로우 의무
	if _, err := g.Play(1, "S3", false, ""); err == nil {
		t.Fatal("조커 지정 문양 팔로우 위반이 허용됐다")
	}
}
