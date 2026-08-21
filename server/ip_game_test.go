package server

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// newStartedIPGame n인 게임을 시드 고정 rng 로 시작해 돌려준다
func newStartedIPGame(t *testing.T, n int, seed int64) (*IPGame, *rand.Rand) {
	t.Helper()
	g := NewIPGame("test")
	for i := 0; i < n; i++ {
		if _, err := g.AddPlayer(fmt.Sprintf("P%d", i)); err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
	}
	rng := rand.New(rand.NewSource(seed))
	if err := g.Start(rng); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return g, rng
}

// ipForceBetting 선·차례·카드를 강제해 결정적인 베팅 시나리오를 만든다
func ipForceBetting(g *IPGame, firstSeat int, cards []int) {
	g.FirstSeat = firstSeat
	g.CurrentSeat = firstSeat
	for i, p := range g.Players {
		p.Card = cards[i]
		p.Acted = false
	}
}

func TestIPDeckComposition(t *testing.T) {
	deck := ipDeckComposition()
	if len(deck) != 40 {
		t.Fatalf("덱 크기 = %d, want 40", len(deck))
	}
	counts := map[int]int{}
	for _, c := range deck {
		counts[c]++
	}
	for v := 1; v <= 10; v++ {
		if counts[v] != 4 {
			t.Fatalf("카드 %d 장수 = %d, want 4", v, counts[v])
		}
	}
}

func TestIPStartAnteAndDeal(t *testing.T) {
	g, _ := newStartedIPGame(t, 4, 1)

	if g.Phase != IPPhaseBetting {
		t.Fatalf("phase = %s, want betting", g.Phase)
	}
	if g.Round != 1 || g.Pot != 4 || g.CurrentBet != IPAnte || g.RaisesLeft != IPMaxRaises {
		t.Fatalf("round=%d pot=%d bet=%d raisesLeft=%d", g.Round, g.Pot, g.CurrentBet, g.RaisesLeft)
	}
	if g.CurrentSeat != g.FirstSeat {
		t.Fatalf("currentSeat=%d, want 선 %d", g.CurrentSeat, g.FirstSeat)
	}
	for _, p := range g.Players {
		if p.Chips != IPStartChips-IPAnte {
			t.Fatalf("seat%d chips = %d, want %d", p.Seat, p.Chips, IPStartChips-IPAnte)
		}
		if p.Card < 1 || p.Card > 10 {
			t.Fatalf("seat%d card = %d, want 1~10", p.Seat, p.Card)
		}
		if p.BetThisRound != IPAnte || p.Folded || !p.Alive {
			t.Fatalf("seat%d 초기 상태 이상: %+v", p.Seat, p)
		}
	}
}

// TestIPCheckAroundToShowdown 전원 체크 → 쇼다운 — 최고 카드가 팟 획득
func TestIPCheckAroundToShowdown(t *testing.T) {
	g, _ := newStartedIPGame(t, 3, 2)
	ipForceBetting(g, 0, []int{7, 3, 9})

	for i := 0; i < 3; i++ {
		res, err := g.Call(g.CurrentSeat)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if res.Kind != "check" {
			t.Fatalf("call %d kind = %s, want check (비용 0)", i, res.Kind)
		}
		if i < 2 && res.RoundEnded {
			t.Fatalf("call %d 에서 조기 종료", i)
		}
		if i == 2 && !res.RoundEnded {
			t.Fatal("마지막 체크 후 라운드가 끝나지 않았다")
		}
	}

	if g.Phase != IPPhaseShowdown {
		t.Fatalf("phase = %s, want showdown", g.Phase)
	}
	rr := g.RoundResult
	if rr == nil || len(rr.WinnerSeats) != 1 || rr.WinnerSeats[0] != 2 {
		t.Fatalf("roundResult = %+v, want 승자 seat2", rr)
	}
	if !strings.Contains(rr.Message, "9") || !strings.Contains(rr.Message, "P2") {
		t.Fatalf("카드 요약 문구 = %q", rr.Message)
	}
	if g.Players[2].Chips != 19+3 || g.Pot != 0 {
		t.Fatalf("팟 분배 실패: seat2 chips=%d pot=%d", g.Players[2].Chips, g.Pot)
	}
}

// TestIPRaiseFlow 레이즈 후 나머지 전원이 다시 콜해야 쇼다운으로 간다
func TestIPRaiseFlow(t *testing.T) {
	g, _ := newStartedIPGame(t, 3, 3)
	ipForceBetting(g, 0, []int{10, 2, 5})

	res, err := g.Raise(0, 2) // 베팅 1 → 3
	if err != nil {
		t.Fatalf("raise: %v", err)
	}
	if res.Kind != "raise" || res.Bet != 3 || res.Paid != 2 || g.RaisesLeft != 2 {
		t.Fatalf("raise 결과 = %+v raisesLeft=%d", res, g.RaisesLeft)
	}
	if g.CurrentSeat != 1 {
		t.Fatalf("레이즈 후 차례 = %d, want 1", g.CurrentSeat)
	}

	res, err = g.Call(1)
	if err != nil || res.Kind != "call" || res.Paid != 2 {
		t.Fatalf("seat1 call = %+v err=%v", res, err)
	}
	res, err = g.Call(2)
	if err != nil || !res.RoundEnded {
		t.Fatalf("seat2 call = %+v err=%v, want 쇼다운", res, err)
	}

	// 팟 = 안테 3 + 레이즈 콜 2×3 = 9, 승자 seat0 (카드 10)
	if g.Phase != IPPhaseShowdown || g.RoundResult.WinnerSeats[0] != 0 {
		t.Fatalf("phase=%s winners=%v", g.Phase, g.RoundResult.WinnerSeats)
	}
	if g.Players[0].Chips != 19-2+9 {
		t.Fatalf("승자 칩 = %d, want %d", g.Players[0].Chips, 19-2+9)
	}
}

// TestIPRaiseValidation 레이즈 범위·횟수 상한·차례 검증
func TestIPRaiseValidation(t *testing.T) {
	g, _ := newStartedIPGame(t, 2, 4)
	ipForceBetting(g, 0, []int{5, 6})

	if _, err := g.Raise(0, 0); err == nil {
		t.Fatal("레이즈 0칩이 통과했다")
	}
	if _, err := g.Raise(0, 4); err == nil {
		t.Fatal("레이즈 4칩이 통과했다")
	}
	if _, err := g.Call(1); err == nil {
		t.Fatal("차례가 아닌 좌석의 콜이 통과했다")
	}

	// 레이즈 3회 소진 후 4회째는 에러
	if _, err := g.Raise(0, 1); err != nil {
		t.Fatalf("raise1: %v", err)
	}
	if _, err := g.Raise(1, 1); err != nil {
		t.Fatalf("raise2: %v", err)
	}
	if _, err := g.Raise(0, 1); err != nil {
		t.Fatalf("raise3: %v", err)
	}
	if g.RaisesLeft != 0 {
		t.Fatalf("raisesLeft = %d, want 0", g.RaisesLeft)
	}
	if _, err := g.Raise(1, 1); err == nil {
		t.Fatal("4번째 레이즈가 통과했다")
	}
}

// TestIPFoldWin 한 명만 남으면 즉시 팟 획득 (round_end — 쇼다운 없음)
func TestIPFoldWin(t *testing.T) {
	g, _ := newStartedIPGame(t, 3, 5)
	ipForceBetting(g, 0, []int{1, 2, 3})

	if _, err := g.Fold(0); err != nil {
		t.Fatalf("fold0: %v", err)
	}
	res, err := g.Fold(1)
	if err != nil {
		t.Fatalf("fold1: %v", err)
	}
	if !res.RoundEnded || !res.FoldWin {
		t.Fatalf("fold 결과 = %+v, want FoldWin", res)
	}
	if g.Phase != IPPhaseRoundEnd {
		t.Fatalf("phase = %s, want round_end", g.Phase)
	}
	rr := g.RoundResult
	if len(rr.WinnerSeats) != 1 || rr.WinnerSeats[0] != 2 || !strings.Contains(rr.Message, "폴드") {
		t.Fatalf("roundResult = %+v", rr)
	}
	if g.Players[2].Chips != 19+3 {
		t.Fatalf("승자 칩 = %d, want 22", g.Players[2].Chips)
	}
}

// TestIPAllInCall 칩이 모자란 콜은 올인 — 현재 베팅으로 인정 (간이 룰)
func TestIPAllInCall(t *testing.T) {
	g, _ := newStartedIPGame(t, 2, 6)
	ipForceBetting(g, 0, []int{4, 10})
	g.Players[1].Chips = 1 // 안테 뒤 1칩만 남은 상황을 강제

	if _, err := g.Raise(0, 3); err != nil { // 베팅 1 → 4, 콜 비용 3
		t.Fatalf("raise: %v", err)
	}
	res, err := g.Call(1)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Kind != "allin_call" || res.Paid != 1 {
		t.Fatalf("올인 콜 결과 = %+v", res)
	}
	if !g.Players[1].AllIn || g.Players[1].BetThisRound != 2 {
		t.Fatalf("seat1 올인 상태 이상: %+v", g.Players[1])
	}
	// 올인 콜도 쇼다운에 참여한다 — 카드 10으로 승리해 팟 전체 획득
	if g.Phase != IPPhaseShowdown || g.RoundResult.WinnerSeats[0] != 1 {
		t.Fatalf("phase=%s winners=%v", g.Phase, g.RoundResult.WinnerSeats)
	}
	if g.Players[1].Chips != 2+3+1 { // 안테 2 + 레이즈 3 + 올인 1
		t.Fatalf("승자 칩 = %d, want 6", g.Players[1].Chips)
	}
}

// TestIPTieSplitsPotFromFirstSeat 동점 균등 분배 — 나머지는 선부터
func TestIPTieSplitsPotFromFirstSeat(t *testing.T) {
	g, _ := newStartedIPGame(t, 3, 7)
	ipForceBetting(g, 1, []int{8, 3, 8}) // 선=seat1, 동점 seat0·seat2

	for i := 0; i < 3; i++ {
		if _, err := g.Call(g.CurrentSeat); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	rr := g.RoundResult
	if len(rr.WinnerSeats) != 2 || rr.WinnerSeats[0] != 0 || rr.WinnerSeats[1] != 2 {
		t.Fatalf("winnerSeats = %v, want [0 2]", rr.WinnerSeats)
	}
	// 팟 3 을 2명이 분배: 몫 1 + 나머지 1 — 선(seat1)부터 시계 방향이므로
	// seat2 가 먼저 나머지를 받는다: seat2=+2, seat0=+1
	if g.Players[2].Chips != 19+2 || g.Players[0].Chips != 19+1 {
		t.Fatalf("분배 결과: seat0=%d seat2=%d, want 20/21",
			g.Players[0].Chips, g.Players[2].Chips)
	}
}

// TestIPEliminationAndLastStanding 칩 0 탈락과 1인 생존 즉시 종료
func TestIPEliminationAndLastStanding(t *testing.T) {
	g, rng := newStartedIPGame(t, 3, 8)
	ipForceBetting(g, 0, []int{9, 2, 3})
	for i := 0; i < 3; i++ {
		if _, err := g.Call(g.CurrentSeat); err != nil {
			t.Fatalf("call: %v", err)
		}
	}

	// 다음 라운드 전에 두 명의 칩을 강제로 비운다 → 탈락 → 1인 생존 종료
	g.Players[1].Chips = 0
	g.Players[2].Chips = 0
	eliminated, err := g.NextRound(rng)
	if err != nil {
		t.Fatalf("NextRound: %v", err)
	}
	if len(eliminated) != 2 {
		t.Fatalf("eliminated = %v, want 2명", eliminated)
	}
	if g.Phase != IPPhaseGameOver || g.EndReason != "last_standing" {
		t.Fatalf("phase=%s reason=%s", g.Phase, g.EndReason)
	}
	if len(g.WinnerSeats) != 1 || g.WinnerSeats[0] != 0 {
		t.Fatalf("winnerSeats = %v, want [0]", g.WinnerSeats)
	}
}

// TestIPTenRoundsChipWinner 10라운드 종료 시 최다 칩 승리 (동점 공동 승)
func TestIPTenRoundsChipWinner(t *testing.T) {
	g, rng := newStartedIPGame(t, 3, 9)
	g.Round = IPRounds // 마지막 라운드 상황을 강제
	ipForceBetting(g, 0, []int{9, 2, 3})
	for i := 0; i < 3; i++ {
		if _, err := g.Call(g.CurrentSeat); err != nil {
			t.Fatalf("call: %v", err)
		}
	}

	// seat0 22칩, 나머지 19칩 → seat1 을 22칩으로 맞춰 공동 승 확인
	g.Players[1].Chips = g.Players[0].Chips
	if _, err := g.NextRound(rng); err != nil {
		t.Fatalf("NextRound: %v", err)
	}
	if g.Phase != IPPhaseGameOver || g.EndReason != "chips" {
		t.Fatalf("phase=%s reason=%s", g.Phase, g.EndReason)
	}
	if len(g.WinnerSeats) != 2 || g.WinnerSeats[0] != 0 || g.WinnerSeats[1] != 1 {
		t.Fatalf("winnerSeats = %v, want [0 1]", g.WinnerSeats)
	}
}

// TestIPNextRoundRotatesFirstSeat 선 순환과 라운드 리셋 (안테·레이즈 횟수)
func TestIPNextRoundRotatesFirstSeat(t *testing.T) {
	g, rng := newStartedIPGame(t, 3, 10)
	ipForceBetting(g, 0, []int{9, 2, 3})
	if _, err := g.Raise(0, 1); err != nil {
		t.Fatalf("raise: %v", err)
	}
	for g.Phase == IPPhaseBetting {
		if _, err := g.Call(g.CurrentSeat); err != nil {
			t.Fatalf("call: %v", err)
		}
	}
	if _, err := g.NextRound(rng); err != nil {
		t.Fatalf("NextRound: %v", err)
	}
	if g.Round != 2 || g.FirstSeat != 1 || g.CurrentSeat != 1 {
		t.Fatalf("round=%d firstSeat=%d currentSeat=%d, want 2/1/1",
			g.Round, g.FirstSeat, g.CurrentSeat)
	}
	if g.RaisesLeft != IPMaxRaises || g.CurrentBet != IPAnte || g.Pot != 3 || g.RoundResult != nil {
		t.Fatalf("라운드 리셋 실패: raisesLeft=%d bet=%d pot=%d rr=%v",
			g.RaisesLeft, g.CurrentBet, g.Pot, g.RoundResult)
	}
	for _, p := range g.Players {
		if p.Folded || p.Acted || p.Card < 1 || p.BetThisRound != IPAnte {
			t.Fatalf("seat%d 리셋 실패: %+v", p.Seat, p)
		}
	}
}

// TestIPActionPhaseGuard betting 외 단계의 행동은 전부 거부된다
func TestIPActionPhaseGuard(t *testing.T) {
	g, _ := newStartedIPGame(t, 2, 11)
	ipForceBetting(g, 0, []int{5, 6})
	if _, err := g.Call(0); err != nil {
		t.Fatalf("call: %v", err)
	}
	if _, err := g.Call(1); err != nil {
		t.Fatalf("call: %v", err)
	}
	if g.Phase != IPPhaseShowdown {
		t.Fatalf("phase = %s", g.Phase)
	}
	if _, err := g.Call(0); err == nil {
		t.Fatal("showdown 중 콜이 통과했다")
	}
	if _, err := g.Fold(0); err == nil {
		t.Fatal("showdown 중 폴드가 통과했다")
	}
}
