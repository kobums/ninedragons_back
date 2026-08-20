package server

import (
	"fmt"
	"math/rand"
	"testing"
)

// newTestSKGame n인 게임을 만들어 시작까지 진행한다
func newTestSKGame(t *testing.T, n int) *SKGame {
	t.Helper()
	g := NewSKGame("test")
	for i := 0; i < n; i++ {
		if _, err := g.AddPlayer(fmt.Sprintf("플레이어%d", i+1)); err != nil {
			t.Fatalf("AddPlayer 실패: %v", err)
		}
	}
	if err := g.Start(rand.New(rand.NewSource(1))); err != nil {
		t.Fatalf("Start 실패: %v", err)
	}
	return g
}

// skSetHand 손패를 강제로 세팅한다 (결정적 시나리오용)
func skSetHand(g *SKGame, seat int, cards ...SKCard) {
	g.Players[seat].Hand = append([]SKCard{}, cards...)
}

// skRoseIndex 손패에서 장미의 인덱스
func skRoseIndex(t *testing.T, g *SKGame, seat int) int {
	t.Helper()
	for i, c := range g.Players[seat].Hand {
		if c == SKCardRose {
			return i
		}
	}
	t.Fatalf("seat%d 손패에 장미가 없다: %v", seat, g.Players[seat].Hand)
	return -1
}

func TestSKStartDeal(t *testing.T) {
	g := newTestSKGame(t, 3)
	if g.Phase != SKPhasePlacing {
		t.Fatalf("phase = %s, want placing", g.Phase)
	}
	if g.RoundNo != 1 || g.LeaderSeat != 0 || g.CurrentSeat != -1 {
		t.Fatalf("초기 상태 이상: round=%d leader=%d cur=%d", g.RoundNo, g.LeaderSeat, g.CurrentSeat)
	}
	for i, p := range g.Players {
		if len(p.Hand) != SKHandSize {
			t.Fatalf("seat%d 손패 = %d장, want %d", i, len(p.Hand), SKHandSize)
		}
		roses, skulls := 0, 0
		for _, c := range p.Hand {
			switch c {
			case SKCardRose:
				roses++
			case SKCardSkull:
				skulls++
			}
		}
		if roses != 3 || skulls != 1 {
			t.Fatalf("seat%d 구성 이상: 장미%d 해골%d", i, roses, skulls)
		}
	}
	// 인원 미달·초과 시작 거부
	g2 := NewSKGame("t2")
	g2.AddPlayer("a")
	g2.AddPlayer("b")
	if g2.CanStart() {
		t.Fatal("2인인데 CanStart == true")
	}
}

func TestSKPlacingFlow(t *testing.T) {
	g := newTestSKGame(t, 3)
	rng := rand.New(rand.NewSource(2))

	// 동시 배치 — 중복 배치는 거부
	if err := g.SubmitPlace(0, 0); err != nil {
		t.Fatalf("초기 배치 실패: %v", err)
	}
	if err := g.SubmitPlace(0, 0); err == nil {
		t.Fatal("동시 배치 파트에서 두 번째 배치가 허용됐다")
	}
	if g.PlacingTurns {
		t.Fatal("전원 배치 전에 턴제 파트로 넘어갔다")
	}
	g.SubmitPlace(1, 0)
	g.SubmitPlace(2, 0)
	if !g.PlacingTurns || g.CurrentSeat != 0 {
		t.Fatalf("턴제 파트 진입 실패: turns=%v cur=%d", g.PlacingTurns, g.CurrentSeat)
	}

	// 차례 아닌 배치·배팅 거부
	if err := g.SubmitPlace(1, 0); err == nil {
		t.Fatal("차례가 아닌데 배치가 허용됐다")
	}
	if err := g.SubmitBid(2, 1, rng); err == nil {
		t.Fatal("차례가 아닌데 배팅이 허용됐다")
	}

	// seat0 추가 배치 → 차례가 넘어간다
	if err := g.SubmitPlace(0, 0); err != nil {
		t.Fatalf("턴 배치 실패: %v", err)
	}
	if g.CurrentSeat != 1 || len(g.Players[0].Stack) != 2 {
		t.Fatalf("턴 배치 반영 이상: cur=%d stack0=%d", g.CurrentSeat, len(g.Players[0].Stack))
	}

	// 배팅 범위 검증 (총 배치 4장)
	if err := g.SubmitBid(1, 0, rng); err == nil {
		t.Fatal("0장 배팅이 허용됐다")
	}
	if err := g.SubmitBid(1, 5, rng); err == nil {
		t.Fatal("전체 배치 수를 넘는 배팅이 허용됐다")
	}
	if err := g.SubmitBid(1, 2, rng); err != nil {
		t.Fatalf("배팅 실패: %v", err)
	}
	if g.Phase != SKPhaseBidding || g.HighBid != 2 || g.Players[1].Bid != 2 || g.CurrentSeat != 2 {
		t.Fatalf("배팅 반영 이상: phase=%s high=%d cur=%d", g.Phase, g.HighBid, g.CurrentSeat)
	}
	// 배팅 단계에서 배치는 거부
	if err := g.SubmitPlace(2, 0); err == nil {
		t.Fatal("배팅 단계에서 배치가 허용됐다")
	}
	// 레이즈는 현재보다 커야 한다
	if err := g.SubmitBid(2, 2, rng); err == nil {
		t.Fatal("같은 수 레이즈가 허용됐다")
	}
}

func TestSKFlipSuccessAndTwoPointWin(t *testing.T) {
	g := newTestSKGame(t, 3)
	rng := rand.New(rand.NewSource(3))

	// 결정적 시나리오: 전원 장미만 배치
	for s := 0; s < 3; s++ {
		g.SubmitPlace(s, skRoseIndex(t, g, s))
	}
	g.SubmitPlace(0, skRoseIndex(t, g, 0)) // seat0 stack: 장미 2
	if err := g.SubmitBid(1, 2, rng); err != nil {
		t.Fatalf("배팅 실패: %v", err)
	}
	if err := g.SubmitPass(2, rng); err != nil {
		t.Fatalf("패스 실패: %v", err)
	}
	if err := g.SubmitPass(0, rng); err != nil {
		t.Fatalf("패스 실패: %v", err)
	}

	// 도전자 seat1 — 자기 더미(장미 1)는 자동으로 뒤집혀 있다
	if g.Phase != SKPhaseFlipping || g.ChallengerSeat != 1 {
		t.Fatalf("도전 확정 이상: phase=%s challenger=%d", g.Phase, g.ChallengerSeat)
	}
	if len(g.Flipped) != 1 || g.Flipped[0].Seat != 1 || g.Flipped[0].Card != "rose" {
		t.Fatalf("자기 더미 자동 뒤집기 이상: %v", g.Flipped)
	}

	// 자기 더미 재선택·빈 검증
	if err := g.SubmitFlip(1, 1, rng); err == nil {
		t.Fatal("자기 더미 뒤집기가 허용됐다")
	}
	if err := g.SubmitFlip(0, 2, rng); err == nil {
		t.Fatal("도전자가 아닌데 뒤집기가 허용됐다")
	}

	// 상대 장미를 뒤집어 목표 달성 → 성공
	if err := g.SubmitFlip(1, 0, rng); err != nil {
		t.Fatalf("뒤집기 실패: %v", err)
	}
	if g.Phase != SKPhaseRoundEnd || g.RoundResult == nil || g.RoundResult.Kind != "success" {
		t.Fatalf("성공 판정 이상: phase=%s result=%+v", g.Phase, g.RoundResult)
	}
	if g.Players[1].Points != 1 || g.LeaderSeat != 1 {
		t.Fatalf("점수·선 반영 이상: points=%d leader=%d", g.Players[1].Points, g.LeaderSeat)
	}
	// 카드 복귀 — 전원 손패 원상 복구
	for s := 0; s < 3; s++ {
		if len(g.Players[s].Hand) != SKHandSize || len(g.Players[s].Stack) != 0 {
			t.Fatalf("seat%d 카드 복귀 이상: hand=%d stack=%d",
				s, len(g.Players[s].Hand), len(g.Players[s].Stack))
		}
	}

	// 다음 라운드 초기화
	g.NextRound()
	if g.Phase != SKPhasePlacing || g.RoundNo != 2 || len(g.Flipped) != 0 || g.HighBid != 0 {
		t.Fatalf("다음 라운드 초기화 이상: phase=%s round=%d", g.Phase, g.RoundNo)
	}

	// 같은 시나리오로 2점째 → 게임 종료
	g.Players[1].Points = 1 // (유지되어 있어야 하지만 명시)
	for s := 0; s < 3; s++ {
		g.SubmitPlace(s, skRoseIndex(t, g, s))
	}
	g.SubmitPlace(1, skRoseIndex(t, g, 1)) // 선(seat1)이 한 장 더
	if err := g.SubmitBid(2, 1, rng); err != nil {
		t.Fatalf("배팅 실패: %v", err)
	}
	g.SubmitPass(0, rng)
	g.SubmitPass(1, rng)
	// 도전자 seat2, 자기 더미 장미 1 → 즉시 성공 → 1점. 아직 게임은 계속
	if g.Players[2].Points != 1 || g.Phase != SKPhaseRoundEnd {
		t.Fatalf("2라운드 성공 이상: points2=%d phase=%s", g.Players[2].Points, g.Phase)
	}
	g.NextRound()
	// 3라운드: seat1 이 성공해 2점 선취
	for s := 0; s < 3; s++ {
		g.SubmitPlace(s, skRoseIndex(t, g, s))
	}
	// 선 seat2 가 배팅 1 → seat0·seat1 패스 대신, seat2 는 배치하고 seat0 이 배팅
	// 단순화: seat2(선) 즉시 배팅 1 → 나머지 패스 → seat2 성공 2점? seat2 는 1점.
	// seat1 을 2점으로 만들기 위해 seat2 는 배치, seat0 배팅 1, seat1 레이즈 2,
	// seat0·seat2 패스 → 도전자 seat1(자기 더미 장미1) → 상대 장미 1장 → 성공.
	g.SubmitPlace(2, skRoseIndex(t, g, 2))
	if err := g.SubmitBid(0, 1, rng); err != nil {
		t.Fatalf("배팅 실패: %v", err)
	}
	if err := g.SubmitBid(1, 2, rng); err != nil {
		t.Fatalf("레이즈 실패: %v", err)
	}
	g.SubmitPass(2, rng)
	g.SubmitPass(0, rng)
	if g.ChallengerSeat != 1 {
		t.Fatalf("도전자 = %d, want 1", g.ChallengerSeat)
	}
	if err := g.SubmitFlip(1, 0, rng); err != nil {
		t.Fatalf("뒤집기 실패: %v", err)
	}
	if g.Phase != SKPhaseGameOver || g.WinnerSeat != 1 || g.Players[1].Points != 2 {
		t.Fatalf("2점 선취 종료 이상: phase=%s winner=%d points=%d",
			g.Phase, g.WinnerSeat, g.Players[1].Points)
	}
}

func TestSKFailRemovalEliminationSurvivorWin(t *testing.T) {
	g := newTestSKGame(t, 3)
	rng := rand.New(rand.NewSource(4))

	// seat0 손패를 해골 1장으로 강제 — 자기 해골 실패로 즉시 탈락하는 시나리오
	skSetHand(g, 0, SKCardSkull)
	g.SubmitPlace(0, 0) // 해골 배치 (유일한 카드)
	g.SubmitPlace(1, skRoseIndex(t, g, 1))
	g.SubmitPlace(2, skRoseIndex(t, g, 2))

	// 선 seat0 — 손패가 없어 배치 불가, 배팅만 가능
	if err := g.SubmitPlace(0, 0); err == nil {
		t.Fatal("손패가 없는데 배치가 허용됐다")
	}
	if err := g.SubmitBid(0, 1, rng); err != nil {
		t.Fatalf("배팅 실패: %v", err)
	}
	g.SubmitPass(1, rng)
	g.SubmitPass(2, rng)

	// 도전자 seat0 → 자기 더미 해골 → 실패, 유일한 카드 제거 → 탈락
	if g.RoundResult == nil || g.RoundResult.Kind != "fail" || g.RoundResult.Seat != 0 {
		t.Fatalf("실패 판정 이상: %+v", g.RoundResult)
	}
	if g.Players[0].Alive || len(g.Players[0].Hand) != 0 {
		t.Fatalf("탈락 처리 이상: alive=%v hand=%d", g.Players[0].Alive, len(g.Players[0].Hand))
	}
	// 실패자·해골 주인 모두 탈락 → 다음 생존자가 선
	if g.Phase != SKPhaseRoundEnd || g.LeaderSeat != 1 {
		t.Fatalf("선 계승 이상: phase=%s leader=%d", g.Phase, g.LeaderSeat)
	}
	// 나머지 손패는 원상 복구 (제거 없음)
	if len(g.Players[1].Hand) != SKHandSize || len(g.Players[2].Hand) != SKHandSize {
		t.Fatalf("실패자 외 손패 복구 이상: %d, %d", len(g.Players[1].Hand), len(g.Players[2].Hand))
	}

	g.NextRound()
	// 탈락자는 행동 불가
	if err := g.SubmitPlace(0, 0); err == nil {
		t.Fatal("탈락자의 배치가 허용됐다")
	}

	// 2라운드: seat1 도 자기 해골 실패로 탈락 → seat2 단독 생존 승리
	skSetHand(g, 1, SKCardSkull)
	g.SubmitPlace(1, 0)
	g.SubmitPlace(2, skRoseIndex(t, g, 2))
	if !g.PlacingTurns || g.CurrentSeat != 1 {
		t.Fatalf("2라운드 턴 진입 이상: turns=%v cur=%d", g.PlacingTurns, g.CurrentSeat)
	}
	if err := g.SubmitBid(1, 1, rng); err != nil {
		t.Fatalf("배팅 실패: %v", err)
	}
	if err := g.SubmitPass(2, rng); err != nil {
		t.Fatalf("패스 실패: %v", err)
	}
	if g.Phase != SKPhaseGameOver || g.WinnerSeat != 2 {
		t.Fatalf("단독 생존 승리 이상: phase=%s winner=%d", g.Phase, g.WinnerSeat)
	}
	if g.Players[1].Alive {
		t.Fatal("seat1 이 탈락하지 않았다")
	}
}

func TestSKFailRemovesOneCardAndFailerLeads(t *testing.T) {
	g := newTestSKGame(t, 3)
	rng := rand.New(rand.NewSource(5))

	// seat2 더미 맨 위가 해골이 되도록 강제
	skSetHand(g, 2, SKCardSkull, SKCardRose, SKCardRose, SKCardRose)
	g.SubmitPlace(0, skRoseIndex(t, g, 0))
	g.SubmitPlace(1, skRoseIndex(t, g, 1))
	g.SubmitPlace(2, 0) // 해골

	// 선 seat0 이 전체 배치 수(3)를 선언 → 즉시 도전자
	if err := g.SubmitBid(0, 3, rng); err != nil {
		t.Fatalf("배팅 실패: %v", err)
	}
	if g.Phase != SKPhaseFlipping || g.ChallengerSeat != 0 {
		t.Fatalf("즉시 도전 이상: phase=%s challenger=%d", g.Phase, g.ChallengerSeat)
	}
	// 자기 더미(장미 1) 자동 공개 후 seat2 해골을 뒤집어 실패
	if err := g.SubmitFlip(0, 2, rng); err != nil {
		t.Fatalf("뒤집기 실패: %v", err)
	}
	if g.RoundResult == nil || g.RoundResult.Kind != "fail" {
		t.Fatalf("실패 판정 이상: %+v", g.RoundResult)
	}
	// 실패자 seat0: 4장 → 3장 (무작위 1장 제거, 내용은 비공개 — 저장 안 함)
	if len(g.Players[0].Hand) != SKHandSize-1 {
		t.Fatalf("실패자 손패 = %d장, want %d", len(g.Players[0].Hand), SKHandSize-1)
	}
	// 해골 주인 seat2 는 온전히 복구
	if len(g.Players[2].Hand) != SKHandSize {
		t.Fatalf("해골 주인 손패 = %d장, want %d", len(g.Players[2].Hand), SKHandSize)
	}
	// 실패자가 생존 → 다음 라운드 선
	if !g.Players[0].Alive || g.LeaderSeat != 0 {
		t.Fatalf("실패자 선 이상: alive=%v leader=%d", g.Players[0].Alive, g.LeaderSeat)
	}
}

func TestSKAutoPlaceAll(t *testing.T) {
	g := newTestSKGame(t, 4)
	rng := rand.New(rand.NewSource(6))
	g.SubmitPlace(1, 0) // 한 명만 수동 배치
	g.AutoPlaceAll(rng)
	if !g.PlacingTurns || g.CurrentSeat != g.LeaderSeat {
		t.Fatalf("자동 배치 후 턴 진입 이상: turns=%v cur=%d", g.PlacingTurns, g.CurrentSeat)
	}
	for s := 0; s < 4; s++ {
		if len(g.Players[s].Stack) != 1 || len(g.Players[s].Hand) != SKHandSize-1 {
			t.Fatalf("seat%d 자동 배치 이상: stack=%d hand=%d",
				s, len(g.Players[s].Stack), len(g.Players[s].Hand))
		}
	}
}
