package server

import (
	"fmt"
	"math/rand"
	"testing"
)

// newAVStartedGame 역할을 지정해 1라운드 지명 상태의 게임을 만든다 (순수 로직 검증용)
func newAVStartedGame(roles []AVRole) *AVGame {
	g := NewAVGame("test")
	for i, role := range roles {
		g.Players = append(g.Players, AVPlayer{Seat: i, Name: fmt.Sprintf("p%d", i), Role: role})
	}
	g.QuestSizes = avQuestSizes(len(roles))
	g.Round = 1
	g.LeaderSeat = 0
	g.Phase = AVPhaseTeamPick
	g.Ready = true
	return g
}

// avRoles7 7인 표준 배치: 악(0=암살자,1,2) 선(3=멀린,4,5,6)
func avRoles7() []AVRole {
	return []AVRole{
		AVRoleAssassin, AVRoleEvil, AVRoleEvil,
		AVRoleMerlin, AVRoleGood, AVRoleGood, AVRoleGood,
	}
}

// avApproveAll 전원 찬성으로 팀 투표를 통과시킨다
func avApproveAll(t *testing.T, g *AVGame) {
	t.Helper()
	for _, p := range g.Players {
		if err := g.SubmitTeamVote(p.Seat, true); err != nil {
			t.Fatalf("seat%d 찬성 실패: %v", p.Seat, err)
		}
	}
	if !g.TeamVoteComplete() {
		t.Fatal("전원 투표인데 TeamVoteComplete=false")
	}
	if !g.ResolveTeamVote() {
		t.Fatal("전원 찬성인데 부결됐다")
	}
}

// avRunQuest 지명→전원 찬성→카드 제출까지 한 라운드를 돌린다.
// failers 는 실패 카드를 낼 좌석 (악 진영이어야 한다).
func avRunQuest(t *testing.T, g *AVGame, team []int, failers map[int]bool) (string, int) {
	t.Helper()
	if err := g.SubmitPick(g.LeaderSeat, team); err != nil {
		t.Fatalf("지명 실패: %v", err)
	}
	avApproveAll(t, g)
	for _, s := range team {
		if err := g.SubmitQuest(s, !failers[s]); err != nil {
			t.Fatalf("seat%d 카드 제출 실패: %v", s, err)
		}
	}
	if !g.QuestComplete() {
		t.Fatal("전원 제출인데 QuestComplete=false")
	}
	return g.ResolveQuest()
}

// TestAVQuestTable 인원별 원정 테이블·악 인원·실패 2장 규칙(7인+ 4라운드만)
func TestAVQuestTable(t *testing.T) {
	wantSizes := map[int][5]int{
		5:  {2, 3, 2, 3, 3},
		6:  {2, 3, 4, 3, 4},
		7:  {2, 3, 3, 4, 4},
		8:  {3, 4, 4, 5, 5},
		9:  {3, 4, 4, 5, 5},
		10: {3, 4, 4, 5, 5},
	}
	wantEvil := map[int]int{5: 2, 6: 2, 7: 3, 8: 3, 9: 3, 10: 4}
	for n := AVMinPlayers; n <= AVMaxPlayers; n++ {
		if got := avQuestSizes(n); got != wantSizes[n] {
			t.Fatalf("%d인 원정 테이블 = %v, want %v", n, got, wantSizes[n])
		}
		if got := avEvilCount(n); got != wantEvil[n] {
			t.Fatalf("%d인 악 인원 = %d, want %d", n, got, wantEvil[n])
		}
		for round := 1; round <= 5; round++ {
			want := 1
			if n >= 7 && round == 4 {
				want = 2
			}
			if got := avFailsNeeded(n, round); got != want {
				t.Fatalf("%d인 %d라운드 실패 기준 = %d, want %d", n, round, got, want)
			}
		}
	}
}

// TestAVRoleAssignment 5~10인 역할 배정 — 멀린 1·암살자 1·악 일반·선 일반
func TestAVRoleAssignment(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for n := AVMinPlayers; n <= AVMaxPlayers; n++ {
		g := NewAVGame("assign")
		for i := 0; i < n; i++ {
			if _, err := g.AddPlayer(fmt.Sprintf("p%d", i)); err != nil {
				t.Fatalf("%d인 AddPlayer 실패: %v", n, err)
			}
		}
		if err := g.Start(rng); err != nil {
			t.Fatalf("%d인 Start 실패: %v", n, err)
		}
		counts := map[AVRole]int{}
		for _, p := range g.Players {
			counts[p.Role]++
		}
		evil := avEvilCount(n)
		if counts[AVRoleMerlin] != 1 || counts[AVRoleAssassin] != 1 ||
			counts[AVRoleEvil] != evil-1 || counts[AVRoleGood] != n-evil-1 {
			t.Fatalf("%d인 배정 이상: %v", n, counts)
		}
		if len(g.EvilSeats()) != evil {
			t.Fatalf("%d인 EvilSeats = %v, want %d석", n, g.EvilSeats(), evil)
		}
		if g.Phase != AVPhaseTeamPick || g.Round != 1 {
			t.Fatalf("%d인 시작 상태 이상: phase=%s round=%d", n, g.Phase, g.Round)
		}
		if g.LeaderSeat < 0 || g.LeaderSeat >= n {
			t.Fatalf("%d인 시작 리더 이상: %d", n, g.LeaderSeat)
		}
	}

	// 인원 미달은 시작 불가
	g := NewAVGame("short")
	for i := 0; i < AVMinPlayers-1; i++ {
		g.AddPlayer(fmt.Sprintf("p%d", i))
	}
	if g.CanStart() {
		t.Fatal("4인이 CanStart=true")
	}
	if err := g.Start(rng); err == nil {
		t.Fatal("4인 시작이 허용됐다")
	}
}

// TestAVPickValidation 지명 검증 — 리더 외 지명·인원 불일치·중복·범위 밖 거부
func TestAVPickValidation(t *testing.T) {
	g := newAVStartedGame(avRoles7()) // 1라운드 2명
	if err := g.SubmitPick(1, []int{0, 1}); err == nil {
		t.Fatal("리더가 아닌 지명이 허용됐다")
	}
	if err := g.SubmitPick(0, []int{0, 1, 2}); err == nil {
		t.Fatal("인원 불일치 지명이 허용됐다")
	}
	if err := g.SubmitPick(0, []int{1, 1}); err == nil {
		t.Fatal("중복 좌석 지명이 허용됐다")
	}
	if err := g.SubmitPick(0, []int{0, 7}); err == nil {
		t.Fatal("범위 밖 좌석 지명이 허용됐다")
	}
	if err := g.SubmitPick(0, []int{3, 0}); err != nil {
		t.Fatalf("정상 지명이 거부됐다: %v", err)
	}
	if g.Phase != AVPhaseTeamVote || len(g.Team) != 2 || g.Team[0] != 0 || g.Team[1] != 3 {
		t.Fatalf("지명 후 상태 이상: phase=%s team=%v", g.Phase, g.Team)
	}
	// 투표 단계에서 재지명 불가
	if err := g.SubmitPick(0, []int{0, 1}); err == nil {
		t.Fatal("투표 중 재지명이 허용됐다")
	}
}

// TestAVTeamVoteMajority 과반 찬성만 승인 — 동수는 부결, 부결 시 리더 순환·
// 카운트 증가, 승인 시 카운트 리셋, 표는 해소 후 일괄 공개
func TestAVTeamVoteMajority(t *testing.T) {
	g := newAVStartedGame(avRoles7())
	if err := g.SubmitPick(0, []int{0, 1}); err != nil {
		t.Fatalf("지명 실패: %v", err)
	}

	// 해소 전에는 일괄 공개본이 없다
	for s := 0; s < 3; s++ {
		g.SubmitTeamVote(s, true)
	}
	if g.RevealedVotes != nil {
		t.Fatal("해소 전에 RevealedVotes 가 공개됐다")
	}
	if g.TeamVoteComplete() {
		t.Fatal("3/7 제출인데 TeamVoteComplete=true")
	}

	// 찬 3 : 반 4 → 부결
	for s := 3; s < 7; s++ {
		g.SubmitTeamVote(s, false)
	}
	if !g.TeamVoteComplete() {
		t.Fatal("전원 제출인데 TeamVoteComplete=false")
	}
	if g.ResolveTeamVote() {
		t.Fatal("3/7 찬성이 승인됐다")
	}
	if g.RejectCount != 1 || g.LeaderSeat != 1 || g.Phase != AVPhaseTeamPick || g.Team != nil {
		t.Fatalf("부결 후 상태 이상: reject=%d leader=%d phase=%s team=%v",
			g.RejectCount, g.LeaderSeat, g.Phase, g.Team)
	}
	if len(g.RevealedVotes) != 7 {
		t.Fatalf("일괄 공개 표 수 = %d, want 7", len(g.RevealedVotes))
	}

	// 찬 4 : 반 3 → 승인 (부결 카운트 리셋)
	if err := g.SubmitPick(1, []int{1, 2}); err != nil {
		t.Fatalf("재지명 실패: %v", err)
	}
	for s := 0; s < 7; s++ {
		g.SubmitTeamVote(s, s < 4)
	}
	if !g.ResolveTeamVote() {
		t.Fatal("4/7 찬성이 부결됐다")
	}
	if g.RejectCount != 0 || g.Phase != AVPhaseQuest {
		t.Fatalf("승인 후 상태 이상: reject=%d phase=%s", g.RejectCount, g.Phase)
	}
}

// TestAVFiveRejectsEvilWin 연속 5회 부결이면 악 즉시 승
func TestAVFiveRejectsEvilWin(t *testing.T) {
	g := newAVStartedGame(avRoles7())
	for i := 0; i < AVMaxRejects; i++ {
		leader := g.LeaderSeat
		if err := g.SubmitPick(leader, []int{leader, (leader + 1) % 7}); err != nil {
			t.Fatalf("%d번째 지명 실패: %v", i+1, err)
		}
		for s := 0; s < 7; s++ {
			g.SubmitTeamVote(s, false)
		}
		approved := g.ResolveTeamVote()
		if approved {
			t.Fatalf("%d번째 전원 반대가 승인됐다", i+1)
		}
	}
	if g.Phase != AVPhaseGameOver || g.Winner != "evil" || g.WinReason != "부결 5연속" {
		t.Fatalf("부결 5연속 판정 이상: phase=%s winner=%s reason=%s",
			g.Phase, g.Winner, g.WinReason)
	}
}

// TestAVQuestGoodCannotFail 선 진영은 실패 카드를 낼 수 없고, 원정대 밖·
// 다른 단계 제출은 거부된다
func TestAVQuestGoodCannotFail(t *testing.T) {
	g := newAVStartedGame(avRoles7())
	if err := g.SubmitQuest(0, true); err == nil {
		t.Fatal("지명 단계 카드 제출이 허용됐다")
	}
	g.SubmitPick(0, []int{0, 3}) // 암살자 + 멀린
	avApproveAll(t, g)

	if err := g.SubmitQuest(4, true); err == nil {
		t.Fatal("원정대 밖 카드 제출이 허용됐다")
	}
	if err := g.SubmitQuest(3, false); err == nil {
		t.Fatal("선(멀린)의 실패 카드가 허용됐다")
	}
	if err := g.SubmitQuest(3, true); err != nil {
		t.Fatalf("멀린 성공 카드가 거부됐다: %v", err)
	}
	if err := g.SubmitQuest(0, false); err != nil {
		t.Fatalf("악(암살자) 실패 카드가 거부됐다: %v", err)
	}
	result, fails := g.ResolveQuest()
	if result != "evil" || fails != 1 {
		t.Fatalf("집계 이상: result=%s fails=%d", result, fails)
	}
	if g.LastQuest == nil || g.LastQuest.Fails != 1 || g.LastQuest.Size != 2 {
		t.Fatalf("LastQuest 이상: %+v", g.LastQuest)
	}
	if g.Round != 2 || g.LeaderSeat != 1 || g.Phase != AVPhaseTeamPick {
		t.Fatalf("라운드 진행 이상: round=%d leader=%d phase=%s", g.Round, g.LeaderSeat, g.Phase)
	}
}

// TestAVTwoFailRule 7인+ 4라운드는 실패 2장이어야 원정 실패 — 1장은 성공
func TestAVTwoFailRule(t *testing.T) {
	g := newAVStartedGame(avRoles7())
	// 1~3라운드: 성공 2 실패 1 로 4라운드 도달 (선2:악1)
	avRunQuest(t, g, []int{0, 3}, map[int]bool{0: true}) // 악 1승
	avRunQuest(t, g, []int{3, 4, 5}, nil)                // 선 1승
	avRunQuest(t, g, []int{3, 4, 5}, nil)                // 선 2승
	if g.Round != 4 {
		t.Fatalf("round = %d, want 4", g.Round)
	}

	// 4라운드(4명): 실패 1장 → 2장 미만이라 성공
	result, fails := avRunQuest(t, g, []int{0, 3, 4, 5}, map[int]bool{0: true})
	if result != "good" || fails != 1 {
		t.Fatalf("4라운드 실패 1장 = %s(fails=%d), want good — 2장 규칙", result, fails)
	}
	// 선 3승 → 곧바로 종료가 아니라 암살 단계
	if g.Phase != AVPhaseAssassin || g.Winner != "" {
		t.Fatalf("선 3승 후 상태 이상: phase=%s winner=%s", g.Phase, g.Winner)
	}

	// 같은 배치의 새 게임에서 4라운드 실패 2장은 원정 실패
	g2 := newAVStartedGame(avRoles7())
	avRunQuest(t, g2, []int{0, 3}, map[int]bool{0: true})
	avRunQuest(t, g2, []int{3, 4, 5}, nil)
	avRunQuest(t, g2, []int{3, 4, 5}, nil)
	result, fails = avRunQuest(t, g2, []int{0, 1, 3, 4}, map[int]bool{0: true, 1: true})
	if result != "evil" || fails != 2 {
		t.Fatalf("4라운드 실패 2장 = %s(fails=%d), want evil", result, fails)
	}
}

// TestAVEvilThreeQuestWins 원정 3승이면 악 즉시 승 (암살 단계 없음)
func TestAVEvilThreeQuestWins(t *testing.T) {
	g := newAVStartedGame(avRoles7())
	avRunQuest(t, g, []int{0, 3}, map[int]bool{0: true})
	avRunQuest(t, g, []int{1, 3, 4}, map[int]bool{1: true})
	avRunQuest(t, g, []int{2, 3, 4}, map[int]bool{2: true})
	if g.Phase != AVPhaseGameOver || g.Winner != "evil" || g.WinReason != "원정 3승" {
		t.Fatalf("악 3승 판정 이상: phase=%s winner=%s reason=%s", g.Phase, g.Winner, g.WinReason)
	}
}

// TestAVAssassination 선 3승 → 암살 단계: 적중 시 악 역전승, 빗나가면 선 승.
// 암살자 외 지목·악 진영 지목은 거부된다.
func TestAVAssassination(t *testing.T) {
	toAssassin := func() *AVGame {
		g := newAVStartedGame(avRoles7())
		avRunQuest(t, g, []int{3, 4}, nil)
		avRunQuest(t, g, []int{3, 4, 5}, nil)
		avRunQuest(t, g, []int{3, 4, 5}, nil)
		if g.Phase != AVPhaseAssassin {
			t.Fatalf("선 3승 후 phase = %s, want assassin", g.Phase)
		}
		return g
	}

	// 검증: 암살자만, 선 진영만 지목 가능
	g := toAssassin()
	if err := g.SubmitAssassinate(1, 3); err == nil {
		t.Fatal("악 일반의 암살 지목이 허용됐다")
	}
	if err := g.SubmitAssassinate(0, 1); err == nil {
		t.Fatal("악 진영 지목이 허용됐다")
	}

	// 적중 (멀린 = seat3) → 악 역전승
	if err := g.SubmitAssassinate(0, 3); err != nil {
		t.Fatalf("암살 지목 실패: %v", err)
	}
	if g.Winner != "evil" || g.WinReason != "암살 적중" || g.AssassinTarget != 3 {
		t.Fatalf("암살 적중 판정 이상: winner=%s reason=%s target=%d",
			g.Winner, g.WinReason, g.AssassinTarget)
	}

	// 빗나감 → 선 최종 승
	g = toAssassin()
	if err := g.SubmitAssassinate(0, 4); err != nil {
		t.Fatalf("암살 지목 실패: %v", err)
	}
	if g.Winner != "good" || g.WinReason != "암살 빗나감" || g.AssassinTarget != 4 {
		t.Fatalf("암살 빗나감 판정 이상: winner=%s reason=%s target=%d",
			g.Winner, g.WinReason, g.AssassinTarget)
	}
}

// TestAVAutoActions AFK 자동 행동 — 무작위 합법 지명(리더 포함), 미제출 찬성,
// 미제출 성공, 무작위 선 지목이 전부 합법 수인지
func TestAVAutoActions(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	g := newAVStartedGame(avRoles7())

	// AutoPick — 리더 포함 정확한 인원, 서버 검증 통과
	seats := g.AutoPick(rng)
	if len(seats) != 2 || seats[0] != g.LeaderSeat {
		t.Fatalf("AutoPick = %v (리더 %d 포함 2명이어야 한다)", seats, g.LeaderSeat)
	}
	if err := g.SubmitPick(g.LeaderSeat, seats); err != nil {
		t.Fatalf("AutoPick 결과가 검증에 걸렸다: %v", err)
	}

	// AutoCompleteTeamVote — 미제출 전원 찬성 → 승인
	g.SubmitTeamVote(3, false)
	g.AutoCompleteTeamVote()
	if !g.TeamVoteComplete() {
		t.Fatal("자동 완성 후에도 TeamVoteComplete=false")
	}
	if !g.ResolveTeamVote() {
		t.Fatal("6찬성 1반대가 부결됐다")
	}

	// AutoCompleteQuest — 미제출 성공 처리 → 실패 0장
	g.AutoCompleteQuest()
	result, fails := g.ResolveQuest()
	if result != "good" || fails != 0 {
		t.Fatalf("자동 원정 집계 이상: result=%s fails=%d", result, fails)
	}

	// RandomGoodSeat — 항상 선 진영 좌석
	for i := 0; i < 20; i++ {
		seat := g.RandomGoodSeat(rng)
		if avIsEvilRole(g.Players[seat].Role) {
			t.Fatalf("RandomGoodSeat 가 악 좌석 %d 을 골랐다", seat)
		}
	}
}
