package server

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// crNewTestGame n 인 게임을 시작 상태로 만든다 (손패·차례는 각 테스트가
// 결정적으로 덮어쓴다)
func crNewTestGame(t *testing.T, n int) (*CRGame, *rand.Rand) {
	t.Helper()
	g := NewCRGame("cr-test")
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

func crSetHand(g *CRGame, seat int, animals ...CRAnimal) {
	g.Players[seat].Hand = append([]CRAnimal{}, animals...)
}

// crEventText 이벤트 큐를 비우고 문구를 이어붙인다 (문구 검증용)
func crEventText(g *CRGame) string {
	msgs := []string{}
	for _, ev := range g.DrainEvents() {
		msgs = append(msgs, ev.Kind+":"+ev.Message)
	}
	return strings.Join(msgs, "\n")
}

// TestCRDealAndStart 배분 — 64장을 인원수로 나눠 전부 배분하고 나머지는
// 제거한다 (3인 21장·4인 16장·5인 12장·6인 10장). 시작은 passing 단계.
func TestCRDealAndStart(t *testing.T) {
	wants := map[int]int{3: 21, 4: 16, 5: 12, 6: 10}
	for n, want := range wants {
		g, _ := crNewTestGame(t, n)
		if g.Phase != CRPhasePassing {
			t.Fatalf("%d인 phase = %s, want passing", n, g.Phase)
		}
		if g.PasserSeat < 0 || g.PasserSeat >= n {
			t.Fatalf("%d인 passerSeat = %d", n, g.PasserSeat)
		}
		if g.HolderSeat != -1 || g.LoserSeat != -1 {
			t.Fatalf("%d인 초기값 이상: holder=%d loser=%d", n, g.HolderSeat, g.LoserSeat)
		}
		for _, p := range g.Players {
			if len(p.Hand) != want {
				t.Fatalf("%d인 seat%d 손패 = %d장, want %d", n, p.Seat, len(p.Hand), want)
			}
			if p.Display == nil || len(p.Display) != 0 {
				t.Fatalf("%d인 seat%d 진열 초기화 이상: %v", n, p.Seat, p.Display)
			}
		}
	}
	// 2인은 시작 불가
	g := NewCRGame("cr-two")
	g.AddPlayer("A")
	g.AddPlayer("B")
	if g.CanStart() {
		t.Fatal("2인이 시작 가능으로 판정됐다")
	}
}

// TestCRJudgeTrueFalse 판정 참/거짓 — 맞히면 카드가 마지막 전달자의 진열로
// 가고 그 사람이 다시 전달자, 틀리면 자기 진열에 쌓이고 자신이 전달자가 된다.
func TestCRJudgeTrueFalse(t *testing.T) {
	g, _ := crNewTestGame(t, 3)
	g.PasserSeat = 0
	crSetHand(g, 0, CRRat, CRBat)
	crSetHand(g, 1, CRToad, CRToad)
	crSetHand(g, 2, CRSpider, CRFly)

	// 실물 rat, 선언 rat (참) — seat1 이 "참" 판정 → 적중
	if err := g.PassCard(0, CRRat, 1, CRRat); err != nil {
		t.Fatalf("PassCard: %v", err)
	}
	if g.Phase != CRPhaseDeciding || g.HolderSeat != 1 || g.Claim != CRRat {
		t.Fatalf("전달 후 상태 이상: phase=%s holder=%d claim=%s", g.Phase, g.HolderSeat, g.Claim)
	}
	if len(g.Players[0].Hand) != 1 {
		t.Fatalf("전달자 손패 = %d장, want 1", len(g.Players[0].Hand))
	}
	if err := g.Judge(1, true); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	text := crEventText(g)
	if !strings.Contains(text, "judge_correct") || !strings.Contains(text, "쥐") {
		t.Fatalf("판정 적중 이벤트 이상:\n%s", text)
	}
	if g.Players[0].Display[CRRat] != 1 {
		t.Fatalf("적중 시 카드가 전달자 진열로 가지 않았다: %v", g.Players[0].Display)
	}
	if g.Phase != CRPhasePassing || g.PasserSeat != 0 {
		t.Fatalf("적중 후 전달자 이상: phase=%s passer=%d, want seat0", g.Phase, g.PasserSeat)
	}

	// 실물 bat, 선언 rat (거짓) — seat2 가 "참" 판정 → 실패
	if err := g.PassCard(0, CRBat, 2, CRRat); err != nil {
		t.Fatalf("PassCard: %v", err)
	}
	if err := g.Judge(2, true); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	text = crEventText(g)
	if !strings.Contains(text, "judge_wrong") || !strings.Contains(text, "박쥐") {
		t.Fatalf("판정 실패 이벤트 이상:\n%s", text)
	}
	if g.Players[2].Display[CRBat] != 1 {
		t.Fatalf("실패 시 카드가 자기 진열로 가지 않았다: %v", g.Players[2].Display)
	}
	if g.Phase != CRPhasePassing || g.PasserSeat != 2 {
		t.Fatalf("실패 후 전달자 이상: phase=%s passer=%d, want seat2", g.Phase, g.PasserSeat)
	}
	// 릴레이 상태가 해제됐다
	if g.HolderSeat != -1 || g.Claim != "" || g.Card != "" || g.Chain != nil {
		t.Fatalf("릴레이 해제 실패: holder=%d claim=%q card=%q chain=%v",
			g.HolderSeat, g.Claim, g.Card, g.Chain)
	}
}

// TestCRRelayChain 릴레이 체인 — 넘긴 사람은 체인에 쌓이고, 체인에 낀
// 사람에게는 다시 넘길 수 없으며, 마지막 남은 사람은 넘기기 불가(강제 판정).
func TestCRRelayChain(t *testing.T) {
	g, _ := crNewTestGame(t, 4)
	g.PasserSeat = 0
	crSetHand(g, 0, CRScorpion)
	if err := g.PassCard(0, CRScorpion, 1, CRFly); err != nil {
		t.Fatalf("PassCard: %v", err)
	}

	// seat1 → seat2 넘기기 (새 선언)
	if !g.CanRelay(1) {
		t.Fatal("seat1 넘기기 가능해야 한다")
	}
	if err := g.Relay(1, 2, CRToad); err != nil {
		t.Fatalf("Relay(1→2): %v", err)
	}
	if g.PasserSeat != 1 || g.HolderSeat != 2 || g.Claim != CRToad {
		t.Fatalf("릴레이 후 상태 이상: passer=%d holder=%d claim=%s", g.PasserSeat, g.HolderSeat, g.Claim)
	}
	if len(g.Chain) != 2 || g.Chain[0] != 0 || g.Chain[1] != 1 {
		t.Fatalf("체인 = %v, want [0 1]", g.Chain)
	}

	// 체인에 낀 seat0 에게는 넘길 수 없다
	if err := g.Relay(2, 0, CRRat); err == nil {
		t.Fatal("체인에 낀 좌석으로 넘기기가 허용됐다")
	}
	// seat2 → seat3 (마지막 안 본 사람)
	if err := g.Relay(2, 3, CRRat); err != nil {
		t.Fatalf("Relay(2→3): %v", err)
	}

	// seat3 은 카드를 안 본 사람이 없어 넘기기 불가 — 강제 판정
	if g.CanRelay(3) {
		t.Fatal("마지막 남은 사람이 넘기기 가능으로 판정됐다")
	}
	if err := g.Relay(3, 0, CRRat); err == nil {
		t.Fatal("마지막 남은 사람의 넘기기가 허용됐다")
	}
	// 실물 scorpion, 선언 rat — "거짓" 판정 → 적중, 카드는 seat2(마지막 전달자) 진열로
	if err := g.Judge(3, false); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if g.Players[2].Display[CRScorpion] != 1 {
		t.Fatalf("적중 시 마지막 전달자 진열 미반영: %v", g.Players[2].Display)
	}
	if g.PasserSeat != 2 || g.Phase != CRPhasePassing {
		t.Fatalf("적중 후 전달자 = seat%d phase=%s, want seat2 passing", g.PasserSeat, g.Phase)
	}
}

// TestCRFourAnimalsLose 같은 동물 4장이 진열에 모이면 즉시 패배 — 나머지
// 전원 승리 (게임 종료).
func TestCRFourAnimalsLose(t *testing.T) {
	g, _ := crNewTestGame(t, 3)
	g.PasserSeat = 0
	crSetHand(g, 0, CRFly, CRRat)
	g.Players[1].Display[CRFly] = 3 // 이미 파리 3장

	// 실물 fly, 선언 fly (참) — seat1 이 "거짓" 판정 → 실패 → 자기 진열에 4장째
	if err := g.PassCard(0, CRFly, 1, CRFly); err != nil {
		t.Fatalf("PassCard: %v", err)
	}
	if err := g.Judge(1, false); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if g.Phase != CRPhaseGameOver {
		t.Fatalf("phase = %s, want game_over", g.Phase)
	}
	if g.LoserSeat != 1 || g.LoseReason != CRLoseFourAnimals {
		t.Fatalf("패자 = seat%d 사유=%s, want seat1 %s", g.LoserSeat, g.LoseReason, CRLoseFourAnimals)
	}
	if g.Players[1].Display[CRFly] != 4 {
		t.Fatalf("진열 파리 = %d장, want 4", g.Players[1].Display[CRFly])
	}
	text := crEventText(g)
	if !strings.Contains(text, "defeated") || !strings.Contains(text, "파리") {
		t.Fatalf("패배 이벤트 이상:\n%s", text)
	}
}

// TestCREmptyHandLose 전달 차례인데 손패가 0장이면 그 사람 패배.
func TestCREmptyHandLose(t *testing.T) {
	g, _ := crNewTestGame(t, 3)
	g.PasserSeat = 0
	crSetHand(g, 0, CRStinkbug) // 마지막 1장
	crSetHand(g, 1, CRToad)
	crSetHand(g, 2, CRToad)

	if err := g.PassCard(0, CRStinkbug, 1, CRStinkbug); err != nil {
		t.Fatalf("PassCard: %v", err)
	}
	// seat1 적중("참") → 카드는 seat0 진열, seat0 이 다시 전달자 — 손패 0장 패배
	if err := g.Judge(1, true); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if g.Phase != CRPhaseGameOver {
		t.Fatalf("phase = %s, want game_over", g.Phase)
	}
	if g.LoserSeat != 0 || g.LoseReason != CRLoseEmptyHand {
		t.Fatalf("패자 = seat%d 사유=%s, want seat0 %s", g.LoserSeat, g.LoseReason, CRLoseEmptyHand)
	}
	text := crEventText(g)
	if !strings.Contains(text, "전달할 카드가 없어") {
		t.Fatalf("손패 소진 패배 이벤트 이상:\n%s", text)
	}
}

// TestCRValidation 반칙 거부 — 차례 아님·손패에 없는 카드·자기 대상·
// 미지의 동물·단계 위반은 전부 에러다.
func TestCRValidation(t *testing.T) {
	g, _ := crNewTestGame(t, 3)
	g.PasserSeat = 0
	crSetHand(g, 0, CRRat, CRBat)
	crSetHand(g, 1, CRToad)

	if err := g.PassCard(1, CRToad, 0, CRToad); err == nil {
		t.Fatal("차례 아닌 전달이 허용됐다")
	}
	if err := g.PassCard(0, CRSpider, 1, CRRat); err == nil {
		t.Fatal("손패에 없는 카드 전달이 허용됐다")
	}
	if err := g.PassCard(0, CRRat, 0, CRRat); err == nil {
		t.Fatal("자기 자신 대상 전달이 허용됐다")
	}
	if err := g.PassCard(0, CRRat, 9, CRRat); err == nil {
		t.Fatal("범위 밖 대상 전달이 허용됐다")
	}
	if err := g.PassCard(0, CRRat, 1, CRAnimal("dragon")); err == nil {
		t.Fatal("미지의 동물 선언이 허용됐다")
	}
	if err := g.Judge(1, true); err == nil {
		t.Fatal("passing 단계의 판정이 허용됐다")
	}
	if err := g.Relay(1, 2, CRRat); err == nil {
		t.Fatal("passing 단계의 넘기기가 허용됐다")
	}

	if err := g.PassCard(0, CRRat, 1, CRBat); err != nil {
		t.Fatalf("PassCard: %v", err)
	}
	if err := g.Judge(2, true); err == nil {
		t.Fatal("결정권자 아닌 판정이 허용됐다")
	}
	if err := g.Relay(2, 0, CRRat); err == nil {
		t.Fatal("결정권자 아닌 넘기기가 허용됐다")
	}
	if err := g.PassCard(0, CRBat, 2, CRBat); err == nil {
		t.Fatal("deciding 단계의 전달이 허용됐다")
	}
	if err := g.Relay(1, 2, CRAnimal("dragon")); err == nil {
		t.Fatal("미지의 동물 릴레이 선언이 허용됐다")
	}
}
