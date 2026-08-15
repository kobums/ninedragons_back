package server

import (
	"strings"
	"testing"
)

// mustParse 콤보 판정 (실패 시 즉시 실패)
func mustParse(t *testing.T, cards []string, prev *tcCombo) *tcCombo {
	t.Helper()
	combo, err := tcParseCombo(cards, prev)
	if err != nil {
		t.Fatalf("parse %v failed: %v", cards, err)
	}
	return combo
}

func TestTCParseCombo(t *testing.T) {
	cases := []struct {
		cards []string
		kind  string
		value int
	}{
		{[]string{"G7"}, tcSingle, 14},
		{[]string{"MAH"}, tcSingle, 2},
		{[]string{"DRG"}, tcSingle, 30},
		{[]string{"DOG"}, tcDogCombo, 0},
		{[]string{"G7", "B7"}, tcPair, 14},
		{[]string{"G7", "PHX"}, tcPair, 14},
		{[]string{"G7", "B7", "U7"}, tcTriple, 14},
		{[]string{"G7", "B7", "PHX"}, tcTriple, 14},
		{[]string{"G2", "B3", "U4", "R5", "G6"}, tcStraight, 12},
		{[]string{"MAH", "G2", "B3", "U4", "R5"}, tcStraight, 10},
		{[]string{"G2", "B3", "U4", "PHX", "G6"}, tcStraight, 12},                // 봉황 빈칸 채움
		{[]string{"G2", "B3", "U4", "R5", "PHX"}, tcStraight, 12},                // 봉황 위 연장
		{[]string{"G7", "B7", "U7", "G4", "B4"}, tcFullhouse, 14},                // 3+2
		{[]string{"G7", "B7", "U7", "G4", "PHX"}, tcFullhouse, 14},               // 3+1+봉황
		{[]string{"G7", "B7", "G4", "B4", "PHX"}, tcFullhouse, 14},               // 2+2+봉황 → 높은 쪽 트리플
		{[]string{"G4", "B4", "G5", "U5"}, tcPairSeq, 10},                        // 연속 2쌍
		{[]string{"G4", "B4", "G5", "PHX"}, tcPairSeq, 10},                       // 봉황 채움
		{[]string{"G4", "B4", "G5", "U5", "R6", "B6"}, tcPairSeq, 12},            // 3쌍
		{[]string{"G7", "B7", "U7", "R7"}, tcBomb, 14},                           // 폭탄
		{[]string{"G4", "G5", "G6", "G7", "G8"}, tcBombSF, 16},                   // 같은무늬 스트레이트
		{[]string{"G4", "G5", "G6", "G7", "G8", "G9"}, tcBombSF, 18},             // 6장 폭탄
		{[]string{"MAH", "G2", "B3", "U4", "R5", "G6", "B7"}, tcStraight, 14},    // 7장 스트레이트
		{[]string{"G8", "B8", "G9", "U9", "R10", "B10", "G11", "U11"}, tcPairSeq, 22}, // 4쌍
	}
	for _, c := range cases {
		combo := mustParse(t, c.cards, nil)
		if combo.Kind != c.kind || combo.Value != c.value {
			t.Errorf("%v = (%s,%d), want (%s,%d)", c.cards, combo.Kind, combo.Value, c.kind, c.value)
		}
	}

	invalid := [][]string{
		{},
		{"G7", "B8"},                         // 페어 아님
		{"DOG", "G2"},                        // 개는 단독
		{"DRG", "G14"},                       // 용은 싱글
		{"G7", "B7", "U7", "PHX"},            // 봉황 폭탄 불가
		{"G2", "B3", "U4", "R5"},             // 4장 스트레이트 불가
		{"G7", "G7"},                         // 중복 카드
		{"MAH", "PHX"},                       // 봉황은 1 대체 불가
		{"G2", "B3", "U4", "PHX", "R7"},      // 빈칸 2개
		{"G7", "B7", "U7", "R7", "G4", "B4"}, // 6장 정체불명
		{"X9"},                               // 잘못된 코드
	}
	for _, cards := range invalid {
		if _, err := tcParseCombo(cards, nil); err == nil {
			t.Errorf("%v 는 무효여야 한다", cards)
		}
	}
}

func TestTCPhoenixSingleValue(t *testing.T) {
	ten := mustParse(t, []string{"G10"}, nil)
	phx := mustParse(t, []string{"PHX"}, ten)
	if phx.Value != 21 {
		t.Fatalf("봉황(10 위) value = %d, want 21", phx.Value)
	}
	if !tcBeats(ten, phx) {
		t.Fatal("봉황이 10을 이겨야 한다")
	}
	jack := mustParse(t, []string{"B11"}, phx)
	if !tcBeats(phx, jack) {
		t.Fatal("J가 봉황(10.5)을 이겨야 한다")
	}
	// 같은 10은 봉황(10.5)을 못 이긴다
	ten2 := mustParse(t, []string{"B10"}, phx)
	if tcBeats(phx, ten2) {
		t.Fatal("10은 봉황(10.5)을 못 이긴다")
	}
	// 리드 봉황 = 1.5
	lead := mustParse(t, []string{"PHX"}, nil)
	if lead.Value != 3 {
		t.Fatalf("리드 봉황 value = %d, want 3", lead.Value)
	}
	// 봉황은 용을 못 이긴다
	drg := mustParse(t, []string{"DRG"}, nil)
	if _, err := tcParseCombo([]string{"PHX"}, drg); err == nil {
		t.Fatal("봉황으로 용을 이길 수 없어야 한다")
	}
}

func TestTCBeats(t *testing.T) {
	pair5 := mustParse(t, []string{"G5", "B5"}, nil)
	pair8 := mustParse(t, []string{"G8", "B8"}, nil)
	if !tcBeats(pair5, pair8) || tcBeats(pair8, pair5) {
		t.Fatal("페어 비교 실패")
	}
	single14 := mustParse(t, []string{"G14"}, nil)
	drg := mustParse(t, []string{"DRG"}, single14)
	if !tcBeats(single14, drg) {
		t.Fatal("용은 A를 이긴다")
	}
	bomb := mustParse(t, []string{"G7", "B7", "U7", "R7"}, nil)
	if !tcBeats(drg, bomb) {
		t.Fatal("폭탄은 용을 이긴다")
	}
	sf5 := mustParse(t, []string{"G4", "G5", "G6", "G7", "G8"}, nil)
	if !tcBeats(bomb, sf5) {
		t.Fatal("스트레이트 폭탄은 4장 폭탄을 이긴다")
	}
	if tcBeats(sf5, bomb) {
		t.Fatal("4장 폭탄은 스트레이트 폭탄을 못 이긴다")
	}
	sf6 := mustParse(t, []string{"B4", "B5", "B6", "B7", "B8", "B9"}, nil)
	if !tcBeats(sf5, sf6) {
		t.Fatal("긴 스트레이트 폭탄이 이긴다")
	}
	// 길이 다른 스트레이트는 비교 불가
	st5 := mustParse(t, []string{"G2", "B3", "U4", "R5", "G6"}, nil)
	st6 := mustParse(t, []string{"B3", "G4", "B5", "U6", "R7", "G8"}, nil)
	if tcBeats(st5, st6) {
		t.Fatal("길이가 다른 스트레이트는 못 이긴다")
	}
	// 종류가 다르면 비교 불가
	if tcBeats(pair5, single14) {
		t.Fatal("싱글로 페어를 못 이긴다")
	}
}

func TestTCWishForce(t *testing.T) {
	// 소원 8, 현재 싱글 5 → 8 싱글로 이행 가능 = 강제
	prev := mustParse(t, []string{"G5"}, nil)
	plays, forced := tcEnumerate([]string{"B8", "G3"}, prev, 8)
	if !forced {
		t.Fatal("이행 가능하면 강제여야 한다")
	}
	for _, p := range plays {
		if !tcContainsRank(p, 8) {
			t.Fatalf("강제 수에 8이 없다: %v", p)
		}
	}

	// 소원 8, 현재 싱글 K → 8 싱글은 못 이기므로 강제 아님
	prevK := mustParse(t, []string{"G13"}, nil)
	_, forced = tcEnumerate([]string{"B8", "G3"}, prevK, 8)
	if forced {
		t.Fatal("이길 수 없으면 강제가 아니다")
	}

	// 소원 8, 현재 싱글 K, 8 폭탄 보유 → 폭탄 포함 강제
	plays, forced = tcEnumerate([]string{"G8", "B8", "U8", "R8", "G3"}, prevK, 8)
	if !forced {
		t.Fatal("폭탄으로 이행 가능하면 강제여야 한다")
	}
	foundBomb := false
	for _, p := range plays {
		if len(p) == 4 {
			foundBomb = true
		}
	}
	if !foundBomb {
		t.Fatal("강제 수에 8 폭탄이 있어야 한다")
	}

	// 봉황이 소원 숫자를 대신하는 것은 이행이 아니다
	prev4 := mustParse(t, []string{"G4"}, nil)
	_, forced = tcEnumerate([]string{"PHX", "G3"}, prev4, 8)
	if forced {
		t.Fatal("봉황 대체는 소원 이행이 아니다")
	}

	// 리드에서 소원 활성 + 소원 카드 보유 = 강제
	plays, forced = tcEnumerate([]string{"B8", "G3", "DOG"}, nil, 8)
	if !forced {
		t.Fatal("리드에서도 소원 강제")
	}
	for _, p := range plays {
		if !tcContainsRank(p, 8) {
			t.Fatalf("리드 강제 수에 8이 없다: %v", p)
		}
	}
}

func TestTCWishEnforcedInPlay(t *testing.T) {
	g := NewTCGame("wish")
	g.Started = true
	g.Phase = TCPhasePlay
	g.Hands = [4][]string{{"G5"}, {"B8", "G3"}, {"G9"}, {"G10"}}
	g.CurrentTurn = 0
	g.LastPlayerSeat = -1

	if _, err := g.Play(0, []string{"G5"}, 0); err != nil {
		t.Fatalf("리드 실패: %v", err)
	}
	g.WishRank = 8

	// 8을 낼 수 있는데 3을 내면 거부
	if _, err := g.Play(1, []string{"G3"}, 0); err == nil {
		t.Fatal("소원 강제 위반이 허용됐다")
	}
	// 패스도 거부
	if _, err := g.Pass(1); err == nil {
		t.Fatal("소원 이행 가능 시 패스가 허용됐다")
	}
	// 8을 내면 소원 해제
	res, err := g.Play(1, []string{"B8"}, 0)
	if err != nil {
		t.Fatalf("소원 이행 실패: %v", err)
	}
	if !res.WishDone || g.WishRank != 0 {
		t.Fatal("소원이 해제되지 않았다")
	}
}

func TestTCDogAndTrickFlow(t *testing.T) {
	g := NewTCGame("dog")
	g.Started = true
	g.Phase = TCPhasePlay
	g.Hands = [4][]string{{"DOG", "G2"}, {"G3", "B3"}, {"G5", "B5"}, {"G9", "B9"}}
	g.CurrentTurn = 0
	g.LastPlayerSeat = -1

	res, err := g.Play(0, []string{"DOG"}, 0)
	if err != nil || !res.Dog {
		t.Fatalf("개 리드 실패: %v", err)
	}
	if g.CurrentTurn != 2 {
		t.Fatalf("개 후 리드 = seat%d, want seat2 (파트너)", g.CurrentTurn)
	}
	if len(g.Won[0]) != 1 || g.Won[0][0] != "DOG" {
		t.Fatal("개는 낸 사람 더미로 가야 한다")
	}

	// seat2 리드 → seat3 이 이기고 전원 패스 → seat3 트릭 획득
	if _, err := g.Play(2, []string{"G5"}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Play(3, []string{"G9"}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Pass(0); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Pass(1); err != nil {
		t.Fatal(err)
	}
	res2, err := g.Pass(2)
	if err != nil {
		t.Fatal(err)
	}
	if !res2.TrickWon || res2.WinnerSeat != 3 {
		t.Fatalf("트릭 승자 = %+v, want seat3", res2)
	}
	if g.CurrentTurn != 3 || len(g.Won[3]) != 2 {
		t.Fatalf("트릭 획득 후 상태 이상: turn=%d won=%v", g.CurrentTurn, g.Won[3])
	}
}

func TestTCDragonGive(t *testing.T) {
	g := NewTCGame("drg")
	g.Started = true
	g.Phase = TCPhasePlay
	g.Hands = [4][]string{{"G2", "B2"}, {"DRG", "B3"}, {"G5", "B7"}, {"G9", "B10"}}
	g.CurrentTurn = 0
	g.LastPlayerSeat = -1

	if _, err := g.Play(0, []string{"G2"}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Play(1, []string{"DRG"}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Pass(2); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Pass(3); err != nil {
		t.Fatal(err)
	}
	res, err := g.Pass(0)
	if err != nil {
		t.Fatal(err)
	}
	if !res.DragonPending || g.DragonPendingSeat != 1 {
		t.Fatalf("용 트릭 대기 상태가 아니다: %+v", res)
	}
	// 같은 팀에게는 못 준다
	if err := g.DragonGive(1, 3); err == nil {
		t.Fatal("같은 팀 전달이 허용됐다")
	}
	if err := g.DragonGive(1, 2); err != nil {
		t.Fatalf("용 트릭 전달 실패: %v", err)
	}
	if !tcContainsCard(g.Won[2], "DRG") || !tcContainsCard(g.Won[2], "G2") {
		t.Fatalf("트릭이 seat2 더미로 가지 않았다: %v", g.Won[2])
	}
	if g.CurrentTurn != 1 {
		t.Fatalf("용 승자가 리드해야 한다: turn=%d", g.CurrentTurn)
	}
}

func TestTCOutOfTurnBomb(t *testing.T) {
	g := NewTCGame("bomb")
	g.Started = true
	g.Phase = TCPhasePlay
	g.Hands = [4][]string{
		{"G3", "B3"},
		{"G5", "B5"},
		{"G8", "B8", "U8", "R8", "G2"},
		{"G9", "B9"},
	}
	g.CurrentTurn = 0
	g.LastPlayerSeat = -1

	// 리드 전 폭탄 불가
	if _, err := g.Play(2, []string{"G8", "B8", "U8", "R8"}, 0); err == nil {
		t.Fatal("리드 전 폭탄이 허용됐다")
	}
	if _, err := g.Play(0, []string{"G3"}, 0); err != nil {
		t.Fatal(err)
	}
	// 차례 무관 폭탄
	res, err := g.Play(2, []string{"G8", "B8", "U8", "R8"}, 0)
	if err != nil {
		t.Fatalf("차례 무관 폭탄 실패: %v", err)
	}
	if !res.OutOfTurn || res.Combo.Kind != tcBomb {
		t.Fatalf("폭탄 결과 이상: %+v", res)
	}
	if g.CurrentTurn != 3 {
		t.Fatalf("폭탄 후 차례 = seat%d, want seat3", g.CurrentTurn)
	}
	// 차례 무관 일반 수는 불가
	if _, err := g.Play(1, []string{"G5"}, 0); err == nil {
		t.Fatal("차례 아닌 일반 수가 허용됐다")
	}
}

func TestTCHandPoints(t *testing.T) {
	full := tcDeck()
	if got := tcHandPoints(full); got != 100 {
		t.Fatalf("전체 덱 점수 = %d, want 100", got)
	}
	cards := []string{"G5", "B10", "U13", "DRG", "PHX", "G2"}
	if got := tcHandPoints(cards); got != 25 {
		t.Fatalf("점수 = %d, want 25", got)
	}
}

// settleFixture 아웃 순서·더미·손패를 지정해 정산 직전 상태를 만든다
func settleFixture() *TCGame {
	g := NewTCGame("settle")
	g.Started = true
	g.Phase = TCPhasePlay
	g.Names = [4]string{"A", "B", "C", "D"}
	g.PlayerCount = 4
	return g
}

func TestTCSettleNormal(t *testing.T) {
	g := settleFixture()
	// 아웃: 0(1위), 1(2위), 2(3위), 꼴찌 3
	g.Out = [4]bool{true, true, true, false}
	g.OutRank = [4]int{1, 2, 3, 0}
	g.OutCount = 3
	g.Won = [4][]string{{"G5"}, {"G10", "B13"}, {"DRG"}, {"B5", "U5"}}
	g.Hands = [4][]string{{}, {}, {}, {"PHX"}}
	g.finishHand()

	// pts: seat0=5+10(꼴찌 더미)=15, seat2=25 → 02팀 40, 13팀 20
	// 꼴찌(3, 13팀) 손패 -25 → 상대팀(02) → 02팀 15
	if g.HandResult.Team02Delta != 15 || g.HandResult.Team13Delta != 20 {
		t.Fatalf("정산 = %+v, want 02=15 13=20", g.HandResult)
	}
	if g.Phase != TCPhaseHandEnd {
		t.Fatalf("phase = %s, want hand_end", g.Phase)
	}
}

func TestTCSettleOneTwo(t *testing.T) {
	g := settleFixture()
	// 원투: 0(1위), 2(2위) 같은 팀 → +200, 카드점수 무시
	g.Out = [4]bool{true, false, true, false}
	g.OutRank = [4]int{1, 0, 2, 0}
	g.OutCount = 2
	g.Won = [4][]string{{"G5"}, {"DRG"}, {}, {}}
	g.Hands = [4][]string{{}, {"G10"}, {}, {"B13"}}
	g.Tichu = [4]string{"tichu", "grand", "", ""} // 0 성공 +100, 1 실패 -200
	if !g.handShouldEnd() {
		t.Fatal("원투는 즉시 핸드 종료여야 한다")
	}
	g.finishHand()

	if g.HandResult.Team02Delta != 300 || g.HandResult.Team13Delta != -200 {
		t.Fatalf("정산 = %+v, want 02=300 13=-200", g.HandResult)
	}
	if !strings.Contains(g.HandResult.Detail, "원투") {
		t.Fatalf("detail = %q", g.HandResult.Detail)
	}
}

func TestTCSettleGameOver(t *testing.T) {
	g := settleFixture()
	g.Score02 = 450
	g.Score13 = 100
	g.Out = [4]bool{true, false, true, false}
	g.OutRank = [4]int{1, 0, 2, 0}
	g.OutCount = 2
	g.finishHand()

	if g.Phase != TCPhaseGameOver || g.WinnerTeam != "02" {
		t.Fatalf("phase=%s winner=%s, want game_over/02", g.Phase, g.WinnerTeam)
	}
	if g.Score02 != 650 {
		t.Fatalf("score02 = %d, want 650", g.Score02)
	}
}

func TestTCSettleTieContinues(t *testing.T) {
	g := settleFixture()
	// 동점으로 목표 도달 → 한 핸드 더
	g.Score02 = 300
	g.Score13 = 500
	g.Out = [4]bool{true, false, true, false}
	g.OutRank = [4]int{1, 0, 2, 0}
	g.OutCount = 2
	g.finishHand() // 02 +200 → 500:500
	if g.Phase != TCPhaseHandEnd {
		t.Fatalf("동점이면 계속해야 한다: phase=%s (02=%d 13=%d)", g.Phase, g.Score02, g.Score13)
	}
}

func TestTCExchangeFlow(t *testing.T) {
	g := NewTCGame("ex")
	g.Started = true
	g.Names = [4]string{"A", "B", "C", "D"}
	g.PlayerCount = 4
	g.Phase = TCPhaseExchange
	g.Hands = [4][]string{
		{"MAH", "G2", "B2", "U2"},
		{"G5", "B5", "U5", "R5"},
		{"G8", "B8", "U8", "R8"},
		{"G11", "B11", "U11", "R11"},
	}
	subs := [4]TCExchangePayload{
		{Left: "MAH", Partner: "G2", Right: "B2"},
		{Left: "G5", Partner: "B5", Right: "U5"},
		{Left: "G8", Partner: "B8", Right: "U8"},
		{Left: "G11", Partner: "B11", Right: "U11"},
	}
	for s := 0; s < 4; s++ {
		done, err := g.SubmitExchange(s, subs[s])
		if err != nil {
			t.Fatalf("seat%d 제출 실패: %v", s, err)
		}
		if done != (s == 3) {
			t.Fatalf("seat%d done=%v", s, done)
		}
	}
	if g.Phase != TCPhasePlay {
		t.Fatalf("phase = %s, want play", g.Phase)
	}
	// seat0 의 left(MAH) → seat3, seat0 의 partner(G2) → seat2, right(B2) → seat1
	if !tcContainsCard(g.Hands[3], "MAH") || !tcContainsCard(g.Hands[2], "G2") || !tcContainsCard(g.Hands[1], "B2") {
		t.Fatalf("교환 분배 이상: %v", g.Hands)
	}
	// MAH 소지자(seat3)가 선공
	if g.CurrentTurn != 3 {
		t.Fatalf("선공 = seat%d, want seat3", g.CurrentTurn)
	}
	for s := 0; s < 4; s++ {
		if len(g.Hands[s]) != 4 {
			t.Fatalf("seat%d 손패 %d장, want 4", s, len(g.Hands[s]))
		}
	}
}

func TestTCOneTwoEndsImmediately(t *testing.T) {
	g := NewTCGame("onetwo")
	g.Started = true
	g.Phase = TCPhasePlay
	g.Names = [4]string{"A", "B", "C", "D"}
	g.Hands = [4][]string{{"G3"}, {"G5", "B5"}, {"G4"}, {"G9", "B9"}}
	g.CurrentTurn = 0
	g.LastPlayerSeat = -1

	if _, err := g.Play(0, []string{"G3"}, 0); err != nil { // seat0 1위 아웃
		t.Fatal(err)
	}
	if _, err := g.Pass(1); err != nil {
		t.Fatal(err)
	}
	res, err := g.Play(2, []string{"G4"}, 0) // seat2 2위 아웃 → 원투
	if err != nil {
		t.Fatal(err)
	}
	if !res.HandEnded {
		t.Fatal("원투 피니시가 즉시 핸드를 끝내야 한다")
	}
	if g.HandResult.Team02Delta != 200 || g.HandResult.Team13Delta != 0 {
		t.Fatalf("정산 = %+v", g.HandResult)
	}
}
