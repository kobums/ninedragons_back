package server

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// cpNewTestGame n 인 게임을 시작 상태로 만든다 (카드·차례는 각 테스트가
// 결정적으로 덮어쓴다)
func cpNewTestGame(t *testing.T, n int) (*CPGame, *rand.Rand) {
	t.Helper()
	g := NewCPGame("cp-test")
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

func cpSetCards(g *CPGame, seat int, a, b CPRole) {
	g.Players[seat].Cards = []CPCard{{Role: a}, {Role: b}}
}

// cpEventText 이벤트 큐를 비우고 문구를 이어붙인다 (문구 검증용)
func cpEventText(g *CPGame) string {
	msgs := []string{}
	for _, ev := range g.DrainEvents() {
		msgs = append(msgs, ev.Kind+":"+ev.Message)
	}
	return strings.Join(msgs, "\n")
}

// TestCPChallengeFail 도전 실패 — 주장자가 실제 카드를 보유하면 공개 후
// 덱에 넣고 새로 뽑으며(장수 불변), 도전자가 카드 1장을 잃고 액션은
// 그대로 진행된다 (세금 +3).
func TestCPChallengeFail(t *testing.T) {
	g, rng := cpNewTestGame(t, 3)
	g.CurrentSeat = 0
	cpSetCards(g, 0, CPRoleDuke, CPRoleAssassin)
	cpSetCards(g, 1, CPRoleCaptain, CPRoleCaptain)
	cpSetCards(g, 2, CPRoleContessa, CPRoleContessa)

	if err := g.DeclareAction(0, CPActTax, -1, rng); err != nil {
		t.Fatalf("DeclareAction(tax): %v", err)
	}
	if g.Phase != CPPhaseChallengeWindow {
		t.Fatalf("phase = %s, want challenge_window", g.Phase)
	}

	g.SubmitChallenge(1, rng)
	text := cpEventText(g)
	if !strings.Contains(text, "도전 실패") {
		t.Fatalf("도전 실패 이벤트 부재:\n%s", text)
	}
	// 주장 입증 — 도전자(2장)가 잃을 카드를 고른다
	if g.Phase != CPPhaseLoseCard || g.LoseSeat != 1 {
		t.Fatalf("phase=%s loseSeat=%d, want lose_card seat1", g.Phase, g.LoseSeat)
	}
	if len(g.Players[0].HiddenIdx()) != 2 {
		t.Fatalf("입증한 주장자의 카드 장수가 변했다: %d", len(g.Players[0].HiddenIdx()))
	}

	if err := g.SubmitLose(1, 0, rng); err != nil {
		t.Fatalf("SubmitLose: %v", err)
	}
	// 제거 후 액션 재개 — 세금이 그대로 걷힌다
	if g.Players[0].Chips != CPStartChips+3 {
		t.Fatalf("세금 미적용: chips=%d, want %d", g.Players[0].Chips, CPStartChips+3)
	}
	if len(g.Players[1].HiddenIdx()) != 1 {
		t.Fatalf("도전자 카드 장수 = %d, want 1", len(g.Players[1].HiddenIdx()))
	}
	if g.Phase != CPPhaseAction || g.CurrentSeat != 1 {
		t.Fatalf("턴 전환 실패: phase=%s current=%d", g.Phase, g.CurrentSeat)
	}
	if len(g.Deck) != 15-6 {
		t.Fatalf("덱 장수 = %d, want %d (입증 교체는 넣고 뽑아 불변)", len(g.Deck), 15-6)
	}
}

// TestCPChallengeSuccess 도전 성공 — 주장자가 블러핑이면 카드 1장을 잃고
// 액션이 취소된다 (세금 없음).
func TestCPChallengeSuccess(t *testing.T) {
	g, rng := cpNewTestGame(t, 3)
	g.CurrentSeat = 0
	cpSetCards(g, 0, CPRoleAssassin, CPRoleContessa) // 공작 없음
	cpSetCards(g, 1, CPRoleCaptain, CPRoleCaptain)
	cpSetCards(g, 2, CPRoleDuke, CPRoleDuke)

	if err := g.DeclareAction(0, CPActTax, -1, rng); err != nil {
		t.Fatalf("DeclareAction(tax): %v", err)
	}
	g.SubmitChallenge(1, rng)
	text := cpEventText(g)
	if !strings.Contains(text, "도전 성공") {
		t.Fatalf("도전 성공 이벤트 부재:\n%s", text)
	}
	if g.Phase != CPPhaseLoseCard || g.LoseSeat != 0 {
		t.Fatalf("phase=%s loseSeat=%d, want lose_card seat0 (블러핑 주장자)", g.Phase, g.LoseSeat)
	}
	if err := g.SubmitLose(0, 1, rng); err != nil {
		t.Fatalf("SubmitLose: %v", err)
	}
	if g.Players[0].Chips != CPStartChips {
		t.Fatalf("취소된 세금이 걷혔다: chips=%d", g.Players[0].Chips)
	}
	if len(g.Players[0].HiddenIdx()) != 1 {
		t.Fatalf("주장자 카드 장수 = %d, want 1", len(g.Players[0].HiddenIdx()))
	}
	if g.Phase != CPPhaseAction || g.CurrentSeat != 1 {
		t.Fatalf("턴 전환 실패: phase=%s current=%d", g.Phase, g.CurrentSeat)
	}
}

// TestCPBlockPasses 차단 성립 — 해외원조를 공작 주장으로 차단하고 차단
// 챌린지 창을 전원 허용으로 통과하면 원조가 취소된다.
func TestCPBlockPasses(t *testing.T) {
	g, rng := cpNewTestGame(t, 3)
	g.CurrentSeat = 0

	if err := g.DeclareAction(0, CPActAid, -1, rng); err != nil {
		t.Fatalf("DeclareAction(aid): %v", err)
	}
	if g.Phase != CPPhaseBlockWindow {
		t.Fatalf("phase = %s, want block_window (해외원조는 챌린지 창 없음)", g.Phase)
	}
	if err := g.SubmitBlock(1, CPRoleDuke); err != nil {
		t.Fatalf("SubmitBlock: %v", err)
	}
	if g.Phase != CPPhaseBlockChallengeWindow {
		t.Fatalf("phase = %s, want block_challenge_window", g.Phase)
	}
	if g.Pending.BlockerSeat != 1 || g.Pending.BlockRole != CPRoleDuke {
		t.Fatalf("차단 정보 = seat%d %s", g.Pending.BlockerSeat, g.Pending.BlockRole)
	}

	// 차단자를 뺀 생존자(0·2) 전원이 허용 → 차단 성립
	g.SubmitAllow(0, rng)
	if g.Phase != CPPhaseBlockChallengeWindow {
		t.Fatalf("한 명 허용만으로 창이 닫혔다: %s", g.Phase)
	}
	g.SubmitAllow(2, rng)
	if g.Players[0].Chips != CPStartChips {
		t.Fatalf("차단됐는데 원조를 받았다: chips=%d", g.Players[0].Chips)
	}
	if g.Phase != CPPhaseAction || g.CurrentSeat != 1 {
		t.Fatalf("턴 전환 실패: phase=%s current=%d", g.Phase, g.CurrentSeat)
	}
	if !strings.Contains(cpEventText(g), "저지가 통과") {
		t.Fatal("차단 통과 이벤트 부재")
	}
}

// TestCPBlockChallenge 차단 챌린지 양방향 — 블러핑 차단은 무효(액션 해결),
// 실제 보유 차단은 성립(액션 취소·도전자 카드 손실).
func TestCPBlockChallenge(t *testing.T) {
	g, rng := cpNewTestGame(t, 3)
	cpSetCards(g, 0, CPRoleDuke, CPRoleAssassin)
	cpSetCards(g, 1, CPRoleCaptain, CPRoleCaptain)
	cpSetCards(g, 2, CPRoleContessa, CPRoleContessa) // 공작 없음 — 블러핑 차단용

	// ---- 블러핑 차단이 도전에 무너지는 경우 ----
	g.CurrentSeat = 1
	if err := g.DeclareAction(1, CPActAid, -1, rng); err != nil {
		t.Fatalf("DeclareAction(aid): %v", err)
	}
	if err := g.SubmitBlock(2, CPRoleDuke); err != nil { // seat2 블러핑
		t.Fatalf("SubmitBlock: %v", err)
	}
	g.SubmitChallenge(0, rng) // seat0 이 차단에 도전
	if g.Phase != CPPhaseLoseCard || g.LoseSeat != 2 {
		t.Fatalf("phase=%s loseSeat=%d, want lose_card seat2 (블러핑 차단자)", g.Phase, g.LoseSeat)
	}
	if err := g.SubmitLose(2, 0, rng); err != nil {
		t.Fatalf("SubmitLose: %v", err)
	}
	// 차단 무효 → 원조 해결
	if g.Players[1].Chips != CPStartChips+2 {
		t.Fatalf("차단 무효인데 원조 미지급: chips=%d", g.Players[1].Chips)
	}
	if g.Phase != CPPhaseAction || g.CurrentSeat != 2 {
		t.Fatalf("턴 전환 실패: phase=%s current=%d", g.Phase, g.CurrentSeat)
	}
	g.DrainEvents()

	// ---- 실제 보유 차단이 도전을 이기는 경우 ----
	if err := g.DeclareAction(2, CPActAid, -1, rng); err != nil {
		t.Fatalf("DeclareAction(aid#2): %v", err)
	}
	if err := g.SubmitBlock(0, CPRoleDuke); err != nil { // seat0 은 실제 공작 보유
		t.Fatalf("SubmitBlock#2: %v", err)
	}
	g.SubmitChallenge(1, rng) // seat1 도전 — 실패해야 한다
	text := cpEventText(g)
	if !strings.Contains(text, "도전 실패") {
		t.Fatalf("차단 입증 이벤트 부재:\n%s", text)
	}
	if g.Phase != CPPhaseLoseCard || g.LoseSeat != 1 {
		t.Fatalf("phase=%s loseSeat=%d, want lose_card seat1 (도전자)", g.Phase, g.LoseSeat)
	}
	if err := g.SubmitLose(1, 0, rng); err != nil {
		t.Fatalf("SubmitLose#2: %v", err)
	}
	// 차단 성립 → 원조 취소
	if g.Players[2].Chips != CPStartChips {
		t.Fatalf("차단 성립인데 원조 지급: chips=%d", g.Players[2].Chips)
	}
	if len(g.Players[0].HiddenIdx()) != 2 {
		t.Fatalf("입증한 차단자의 카드 장수가 변했다: %d", len(g.Players[0].HiddenIdx()))
	}
	if g.Phase != CPPhaseAction || g.CurrentSeat != 0 {
		t.Fatalf("턴 전환 실패: phase=%s current=%d", g.Phase, g.CurrentSeat)
	}
}

// TestCPForcedCoupAndElimination 쿠 강제(칩 10+)·쿠 비용·카드 2장 손실
// 탈락·최후 1인 승리까지의 결말.
func TestCPForcedCoupAndElimination(t *testing.T) {
	g, rng := cpNewTestGame(t, 2)
	g.CurrentSeat = 0

	// 칩 부족 쿠는 거부
	if err := g.DeclareAction(0, CPActCoup, 1, rng); err == nil {
		t.Fatal("칩 2개로 쿠가 허용됐다")
	}

	// 칩 10+ 는 쿠 외 전부 거부
	g.Players[0].Chips = CPForceCoupChips
	if err := g.DeclareAction(0, CPActIncome, -1, rng); err == nil ||
		!strings.Contains(err.Error(), "쿠데타만") {
		t.Fatalf("쿠 강제 미적용 (income): %v", err)
	}
	if err := g.DeclareAction(0, CPActTax, -1, rng); err == nil {
		t.Fatal("쿠 강제 미적용 (tax)")
	}

	// 쿠 — 대상(2장)이 잃을 카드를 고른다
	if err := g.DeclareAction(0, CPActCoup, 1, rng); err != nil {
		t.Fatalf("DeclareAction(coup): %v", err)
	}
	if g.Players[0].Chips != CPForceCoupChips-CPCoupCost {
		t.Fatalf("쿠 비용 미지불: chips=%d", g.Players[0].Chips)
	}
	if g.Phase != CPPhaseLoseCard || g.LoseSeat != 1 {
		t.Fatalf("phase=%s loseSeat=%d, want lose_card seat1", g.Phase, g.LoseSeat)
	}
	if err := g.SubmitLose(1, 1, rng); err != nil {
		t.Fatalf("SubmitLose: %v", err)
	}
	if len(g.Players[1].HiddenIdx()) != 1 || !g.Players[1].Alive() {
		t.Fatalf("쿠 뒤 대상 상태: hidden=%d alive=%v", len(g.Players[1].HiddenIdx()), g.Players[1].Alive())
	}
	if g.Phase != CPPhaseAction || g.CurrentSeat != 1 {
		t.Fatalf("턴 전환 실패: phase=%s current=%d", g.Phase, g.CurrentSeat)
	}

	// 두 번째 쿠 — 남은 1장은 선택 없이 즉시 공개되고 탈락·게임 종료
	g.CurrentSeat = 0
	g.Players[0].Chips = CPForceCoupChips
	g.DrainEvents()
	if err := g.DeclareAction(0, CPActCoup, 1, rng); err != nil {
		t.Fatalf("DeclareAction(coup#2): %v", err)
	}
	if g.Phase != CPPhaseGameOver || g.WinnerSeat != 0 {
		t.Fatalf("phase=%s winner=%d, want game_over winner 0", g.Phase, g.WinnerSeat)
	}
	if g.Players[1].Alive() {
		t.Fatal("2장 다 잃은 좌석이 생존 상태다")
	}
	text := cpEventText(g)
	if !strings.Contains(text, "탈락") {
		t.Fatalf("탈락 이벤트 부재:\n%s", text)
	}
}

// TestCPAssassinateDoubleLoss 암살 연쇄 — 대상이 도전에 실패해 1장을 잃고,
// 이어 블러핑 백작부인 차단까지 무너지면 마지막 장도 잃고 탈락한다
// (도전 실패 손실 + 차단 도전 손실 이중 타격).
func TestCPAssassinateDoubleLoss(t *testing.T) {
	g, rng := cpNewTestGame(t, 3)
	g.CurrentSeat = 0
	cpSetCards(g, 0, CPRoleAssassin, CPRoleDuke)
	cpSetCards(g, 1, CPRoleCaptain, CPRoleCaptain) // 백작부인 없음
	cpSetCards(g, 2, CPRoleContessa, CPRoleContessa)
	g.Players[0].Chips = 3

	if err := g.DeclareAction(0, CPActAssassinate, 1, rng); err != nil {
		t.Fatalf("DeclareAction(assassinate): %v", err)
	}
	if g.Players[0].Chips != 0 {
		t.Fatalf("암살 비용 미지불: chips=%d", g.Players[0].Chips)
	}

	// 대상이 암살자 주장에 도전 — 실패 (실제 보유)
	g.SubmitChallenge(1, rng)
	if g.Phase != CPPhaseLoseCard || g.LoseSeat != 1 {
		t.Fatalf("phase=%s loseSeat=%d, want lose_card seat1", g.Phase, g.LoseSeat)
	}
	if err := g.SubmitLose(1, 0, rng); err != nil {
		t.Fatalf("SubmitLose: %v", err)
	}

	// 도전 실패 후 재개 — 대상(1장 남음)의 차단 창
	if g.Phase != CPPhaseBlockWindow {
		t.Fatalf("phase = %s, want block_window", g.Phase)
	}
	// 대상이 아닌 좌석의 차단은 거부
	if err := g.SubmitBlock(2, CPRoleContessa); err == nil {
		t.Fatal("대상이 아닌 좌석의 암살 차단이 허용됐다")
	}
	// 대상이 블러핑 백작부인 차단
	if err := g.SubmitBlock(1, CPRoleContessa); err != nil {
		t.Fatalf("SubmitBlock: %v", err)
	}
	// 액터가 차단에 도전 — 블러핑이라 마지막 장을 즉시 잃고 탈락,
	// 암살 해결 시점엔 대상이 이미 죽어 추가 손실 없이 턴 종료
	g.SubmitChallenge(0, rng)
	if g.Players[1].Alive() {
		t.Fatal("이중 손실 뒤에도 생존 상태다")
	}
	if g.Phase != CPPhaseAction || g.CurrentSeat != 2 {
		t.Fatalf("phase=%s current=%d, want action seat2", g.Phase, g.CurrentSeat)
	}
}

// TestCPStealAndAssassinRefund 강탈 효과(최대 2칩·부족하면 있는 만큼)와
// 거짓 암살 취소 시 비용 반환.
func TestCPStealAndAssassinRefund(t *testing.T) {
	g, rng := cpNewTestGame(t, 3)
	g.CurrentSeat = 0
	cpSetCards(g, 0, CPRoleCaptain, CPRoleCaptain)
	cpSetCards(g, 1, CPRoleDuke, CPRoleDuke)
	cpSetCards(g, 2, CPRoleDuke, CPRoleDuke)
	g.Players[1].Chips = 1 // 강탈 대상 칩 부족

	if err := g.DeclareAction(0, CPActSteal, 1, rng); err != nil {
		t.Fatalf("DeclareAction(steal): %v", err)
	}
	// 챌린지 창 통과 → 차단 창 통과 (전원 허용)
	g.SubmitAllow(1, rng)
	g.SubmitAllow(2, rng)
	if g.Phase != CPPhaseBlockWindow {
		t.Fatalf("phase = %s, want block_window", g.Phase)
	}
	g.SubmitAllow(1, rng)
	g.SubmitAllow(2, rng)
	if g.Players[0].Chips != CPStartChips+1 || g.Players[1].Chips != 0 {
		t.Fatalf("강탈 결과: actor=%d target=%d, want +1/0", g.Players[0].Chips, g.Players[1].Chips)
	}
	if g.Phase != CPPhaseAction || g.CurrentSeat != 1 {
		t.Fatalf("턴 전환 실패: phase=%s current=%d", g.Phase, g.CurrentSeat)
	}

	// 거짓 암살(암살자 미보유)이 도전에 무너지면 3칩을 돌려받는다
	g.CurrentSeat = 2
	g.Players[2].Chips = 3
	if err := g.DeclareAction(2, CPActAssassinate, 0, rng); err != nil {
		t.Fatalf("DeclareAction(assassinate): %v", err)
	}
	g.SubmitChallenge(0, rng)
	if g.Phase != CPPhaseLoseCard || g.LoseSeat != 2 {
		t.Fatalf("phase=%s loseSeat=%d, want lose_card seat2", g.Phase, g.LoseSeat)
	}
	if g.Players[2].Chips != 3 {
		t.Fatalf("거짓 암살 취소 시 비용 미반환: chips=%d", g.Players[2].Chips)
	}
}

// TestCPExchange 교환 — 덱 2장을 뽑아 4장 중 2장을 유지하고 나머지를
// 반납한다 (덱 장수 불변). 잘못된 선택은 거부된다.
func TestCPExchange(t *testing.T) {
	g, rng := cpNewTestGame(t, 2)
	g.CurrentSeat = 0
	cpSetCards(g, 0, CPRoleAmbassador, CPRoleDuke)
	cpSetCards(g, 1, CPRoleCaptain, CPRoleCaptain)
	g.Deck = []CPRole{CPRoleContessa, CPRoleAssassin,
		CPRoleDuke, CPRoleDuke, CPRoleDuke, CPRoleDuke, CPRoleDuke,
		CPRoleDuke, CPRoleDuke, CPRoleDuke, CPRoleDuke} // 11장 (2인전)

	if err := g.DeclareAction(0, CPActExchange, -1, rng); err != nil {
		t.Fatalf("DeclareAction(exchange): %v", err)
	}
	if g.Phase != CPPhaseChallengeWindow {
		t.Fatalf("phase = %s, want challenge_window", g.Phase)
	}
	g.SubmitAllow(1, rng) // 유일한 응답자 허용 → 즉시 통과
	if g.Phase != CPPhaseExchangePick {
		t.Fatalf("phase = %s, want exchange_pick", g.Phase)
	}
	want := []CPRole{CPRoleAmbassador, CPRoleDuke, CPRoleContessa, CPRoleAssassin}
	if len(g.ExchangeCards) != 4 {
		t.Fatalf("교환 선택지 = %v", g.ExchangeCards)
	}
	for i, r := range want {
		if g.ExchangeCards[i] != r {
			t.Fatalf("교환 선택지[%d] = %s, want %s", i, g.ExchangeCards[i], r)
		}
	}

	if err := g.SubmitExchangeKeep(0, []int{0}, rng); err == nil {
		t.Fatal("1장 유지가 허용됐다 (2장 필요)")
	}
	if err := g.SubmitExchangeKeep(0, []int{1, 1}, rng); err == nil {
		t.Fatal("중복 인덱스가 허용됐다")
	}
	if err := g.SubmitExchangeKeep(1, []int{0, 1}, rng); err == nil {
		t.Fatal("타인의 교환 선택이 허용됐다")
	}

	if err := g.SubmitExchangeKeep(0, []int{2, 3}, rng); err != nil {
		t.Fatalf("SubmitExchangeKeep: %v", err)
	}
	roles := g.Players[0].HiddenRoles()
	if len(roles) != 2 || roles[0] != CPRoleContessa || roles[1] != CPRoleAssassin {
		t.Fatalf("교환 후 손패 = %v", roles)
	}
	if len(g.Deck) != 11 {
		t.Fatalf("덱 장수 = %d, want 11 (2장 반납)", len(g.Deck))
	}
	if g.Phase != CPPhaseAction || g.CurrentSeat != 1 {
		t.Fatalf("턴 전환 실패: phase=%s current=%d", g.Phase, g.CurrentSeat)
	}
}
