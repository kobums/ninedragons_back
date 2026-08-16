package server

import (
	"fmt"
	"math/rand"
	"testing"
)

// newSFStartedGame 역할을 지정해 밤 1일차 상태의 게임을 만든다 (순수 로직 검증용)
func newSFStartedGame(roles []SFRole) *SFGame {
	g := NewSFGame("test")
	for i, role := range roles {
		g.Players = append(g.Players, SFPlayer{
			Seat: i, Name: fmt.Sprintf("p%d", i), Role: role, Alive: true,
		})
	}
	g.Phase = SFPhaseNight
	g.DayNo = 1
	g.NightActions = map[int]int{}
	g.Ready = true
	return g
}

// TestSFRoleAssignmentTable 6~10인 역할 배정표:
// 6인 마2·경1·의1·시2 / 7인 마2·경1·의1·시3 / 8인 마3·경1·의1·시3 /
// 9인 마3·경1·의1·시4 / 10인 마3·경1·의1·시5
func TestSFRoleAssignmentTable(t *testing.T) {
	want := map[int][4]int{ // n → [마피아, 경찰, 의사, 시민]
		6:  {2, 1, 1, 2},
		7:  {2, 1, 1, 3},
		8:  {3, 1, 1, 3},
		9:  {3, 1, 1, 4},
		10: {3, 1, 1, 5},
	}
	rng := rand.New(rand.NewSource(42))
	for n := SFMinPlayers; n <= SFMaxPlayers; n++ {
		g := NewSFGame("assign")
		for i := 0; i < n; i++ {
			if _, err := g.AddPlayer(fmt.Sprintf("p%d", i)); err != nil {
				t.Fatalf("%d인 AddPlayer 실패: %v", n, err)
			}
		}
		if err := g.Start(rng); err != nil {
			t.Fatalf("%d인 Start 실패: %v", n, err)
		}
		counts := map[SFRole]int{}
		for _, p := range g.Players {
			counts[p.Role]++
		}
		w := want[n]
		got := [4]int{counts[SFRoleMafia], counts[SFRolePolice], counts[SFRoleDoctor], counts[SFRoleCitizen]}
		if got != w {
			t.Fatalf("%d인 배정 = %v, want %v", n, got, w)
		}
		if g.Phase != SFPhaseNight || g.DayNo != 1 {
			t.Fatalf("%d인 시작 상태 이상: phase=%s dayNo=%d", n, g.Phase, g.DayNo)
		}
	}

	// 인원 미달·초과는 시작 불가
	g := NewSFGame("short")
	for i := 0; i < SFMinPlayers-1; i++ {
		g.AddPlayer(fmt.Sprintf("p%d", i))
	}
	if g.CanStart() {
		t.Fatal("5인이 CanStart=true")
	}
	if err := g.Start(rng); err == nil {
		t.Fatal("5인 시작이 허용됐다")
	}
}

// TestSFNightMafiaMajority 마피아 다수결 — 2표 대상이 살해되고, 경찰 조사
// 결과가 기록된다
func TestSFNightMafiaMajority(t *testing.T) {
	// 8인: 마(0,1,2) 경(3) 의(4) 시(5,6,7)
	g := newSFStartedGame([]SFRole{
		SFRoleMafia, SFRoleMafia, SFRoleMafia, SFRolePolice,
		SFRoleDoctor, SFRoleCitizen, SFRoleCitizen, SFRoleCitizen,
	})

	mustAct := func(seat, target int) {
		t.Helper()
		if err := g.SubmitNightAction(seat, target); err != nil {
			t.Fatalf("seat%d → %d 실패: %v", seat, target, err)
		}
	}
	mustAct(0, 5)
	mustAct(1, 5)
	mustAct(2, 6)
	mustAct(3, 0) // 경찰이 마피아 조사
	if g.NightComplete() {
		t.Fatal("의사 제출 전인데 NightComplete=true")
	}
	mustAct(4, 7) // 의사는 엉뚱한 곳 치료
	if !g.NightComplete() {
		t.Fatal("전원 제출인데 NightComplete=false")
	}

	killed := g.ResolveNight(rand.New(rand.NewSource(1)))
	if killed != 5 {
		t.Fatalf("다수결 살해 = seat%d, want seat5", killed)
	}
	if g.Players[5].Alive {
		t.Fatal("살해된 seat5 가 생존 상태")
	}
	if g.Players[5].RoleRevealed {
		t.Fatal("밤 살해자의 역할이 공개됐다")
	}
	if g.Announcement == nil || g.Announcement.Kind != "killed" || g.Announcement.Seat != 5 {
		t.Fatalf("발표 이상: %+v", g.Announcement)
	}
	if g.Investigation == nil || g.Investigation.Seat != 0 || !g.Investigation.IsMafia {
		t.Fatalf("경찰 조사 결과 이상: %+v", g.Investigation)
	}
	if g.Phase != SFPhaseDayResult {
		t.Fatalf("phase = %s, want day_result", g.Phase)
	}
}

// TestSFNightTieRandom 마피아 동률은 무작위 — 여러 시드에서 두 대상 모두 나온다
func TestSFNightTieRandom(t *testing.T) {
	killedSet := map[int]bool{}
	for seed := int64(0); seed < 50; seed++ {
		g := newSFStartedGame([]SFRole{
			SFRoleMafia, SFRoleMafia, SFRolePolice,
			SFRoleDoctor, SFRoleCitizen, SFRoleCitizen,
		})
		g.SubmitNightAction(0, 4)
		g.SubmitNightAction(1, 5)
		g.SubmitNightAction(2, 0)
		g.SubmitNightAction(3, 3)
		killed := g.ResolveNight(rand.New(rand.NewSource(seed)))
		if killed != 4 && killed != 5 {
			t.Fatalf("동률 살해 = seat%d, want 4 또는 5", killed)
		}
		killedSet[killed] = true
	}
	if len(killedSet) != 2 {
		t.Fatalf("50개 시드에서 동률 무작위가 한쪽으로만 쏠렸다: %v", killedSet)
	}
}

// TestSFDoctorSave 의사 치료가 살해 대상과 일치하면 아무도 죽지 않는다
func TestSFDoctorSave(t *testing.T) {
	g := newSFStartedGame([]SFRole{
		SFRoleMafia, SFRoleMafia, SFRolePolice,
		SFRoleDoctor, SFRoleCitizen, SFRoleCitizen,
	})
	g.SubmitNightAction(0, 4)
	g.SubmitNightAction(1, 4)
	g.SubmitNightAction(2, 1) // 경찰이 마피아 조사
	g.SubmitNightAction(3, 4) // 의사가 살해 대상 치료

	killed := g.ResolveNight(rand.New(rand.NewSource(7)))
	if killed != -1 {
		t.Fatalf("세이브인데 killed = %d", killed)
	}
	if !g.Players[4].Alive {
		t.Fatal("세이브된 seat4 가 사망 상태")
	}
	// 세이브 발표는 대상 비공개
	if g.Announcement == nil || g.Announcement.Kind != "saved" || g.Announcement.Seat != -1 {
		t.Fatalf("세이브 발표 이상: %+v", g.Announcement)
	}
	if g.Investigation == nil || g.Investigation.Seat != 1 || !g.Investigation.IsMafia {
		t.Fatalf("경찰 조사 결과 이상: %+v", g.Investigation)
	}
}

// TestSFNightActionValidation 밤 행동 검증 — 시민 행동 금지, 마피아의 동료
// 지목 금지, 경찰 자기 조사 금지, 사망자 지목 금지
func TestSFNightActionValidation(t *testing.T) {
	g := newSFStartedGame([]SFRole{
		SFRoleMafia, SFRoleMafia, SFRolePolice,
		SFRoleDoctor, SFRoleCitizen, SFRoleCitizen,
	})
	g.Players[5].Alive = false

	if err := g.SubmitNightAction(4, 0); err == nil {
		t.Fatal("시민의 밤 행동이 허용됐다")
	}
	if err := g.SubmitNightAction(0, 1); err == nil {
		t.Fatal("마피아의 동료 지목이 허용됐다")
	}
	if err := g.SubmitNightAction(2, 2); err == nil {
		t.Fatal("경찰의 자기 조사가 허용됐다")
	}
	if err := g.SubmitNightAction(0, 5); err == nil {
		t.Fatal("사망자 지목이 허용됐다")
	}
	if err := g.SubmitNightAction(5, 0); err == nil {
		t.Fatal("사망자의 행동이 허용됐다")
	}
	if err := g.SubmitNightAction(3, 3); err != nil {
		t.Fatalf("의사 자가 치료가 거부됐다: %v", err)
	}
}

// TestSFVoteExecution 단독 최다 득표는 처형되고 역할이 공개된다
func TestSFVoteExecution(t *testing.T) {
	g := newSFStartedGame([]SFRole{
		SFRoleMafia, SFRoleMafia, SFRolePolice,
		SFRoleDoctor, SFRoleCitizen, SFRoleCitizen,
	})
	g.Phase = SFPhaseDayResult
	g.BeginVote()
	if g.Phase != SFPhaseDayVote {
		t.Fatalf("BeginVote 후 phase = %s", g.Phase)
	}

	votes := map[int]int{0: 5, 1: 5, 2: 0, 3: 0, 4: 0, 5: -1}
	for voter, target := range votes {
		if err := g.SubmitVote(voter, target); err != nil {
			t.Fatalf("seat%d 투표 실패: %v", voter, err)
		}
	}
	if !g.VoteComplete() {
		t.Fatal("전원 투표인데 VoteComplete=false")
	}

	g.ResolveVotes()
	if g.Execution == nil || g.Execution.Seat != 0 || g.Execution.Role != string(SFRoleMafia) {
		t.Fatalf("처형 결과 이상: %+v", g.Execution)
	}
	if g.Players[0].Alive || !g.Players[0].RoleRevealed {
		t.Fatal("처형자 상태 이상 (사망·역할 공개여야 한다)")
	}
	if g.Phase != SFPhaseExecution {
		t.Fatalf("phase = %s, want execution", g.Phase)
	}
	if g.Winner != "" {
		t.Fatalf("마피아 1명 생존인데 승부가 났다: %s", g.Winner)
	}

	// 다음 밤으로 — 일차 증가, 낮 상태 초기화
	g.BeginNight()
	if g.Phase != SFPhaseNight || g.DayNo != 2 {
		t.Fatalf("BeginNight 후 phase=%s dayNo=%d", g.Phase, g.DayNo)
	}
	if g.Announcement != nil || g.Execution != nil || g.Investigation != nil || g.Votes != nil {
		t.Fatal("밤 시작 시 낮 상태가 초기화되지 않았다")
	}
}

// TestSFVoteTieAndAllAbstain 동률·전원 기권은 무처형
func TestSFVoteTieAndAllAbstain(t *testing.T) {
	setup := func() *SFGame {
		g := newSFStartedGame([]SFRole{
			SFRoleMafia, SFRoleMafia, SFRolePolice,
			SFRoleDoctor, SFRoleCitizen, SFRoleCitizen,
		})
		g.Phase = SFPhaseDayResult
		g.BeginVote()
		return g
	}

	// 동률 (2:2)
	g := setup()
	for voter, target := range map[int]int{0: 4, 1: 4, 2: 5, 3: 5, 4: -1, 5: -1} {
		g.SubmitVote(voter, target)
	}
	g.ResolveVotes()
	if g.Execution == nil || g.Execution.Seat != -1 || g.Execution.Role != "" {
		t.Fatalf("동률 무처형 이상: %+v", g.Execution)
	}
	if g.aliveCount() != 6 {
		t.Fatal("무처형인데 사망자가 생겼다")
	}

	// 전원 기권
	g = setup()
	for voter := 0; voter < 6; voter++ {
		g.SubmitVote(voter, -1)
	}
	g.ResolveVotes()
	if g.Execution == nil || g.Execution.Seat != -1 {
		t.Fatalf("전원 기권 무처형 이상: %+v", g.Execution)
	}
}

// TestSFVoteValidation 사망자 투표 금지·사망자 지목 금지·기권(-1) 허용
func TestSFVoteValidation(t *testing.T) {
	g := newSFStartedGame([]SFRole{
		SFRoleMafia, SFRoleMafia, SFRolePolice,
		SFRoleDoctor, SFRoleCitizen, SFRoleCitizen,
	})
	g.Players[5].Alive = false
	g.Phase = SFPhaseDayResult
	g.BeginVote()

	if err := g.SubmitVote(5, 0); err == nil {
		t.Fatal("사망자의 투표가 허용됐다")
	}
	if err := g.SubmitVote(0, 5); err == nil {
		t.Fatal("사망자 지목이 허용됐다")
	}
	if err := g.SubmitVote(0, -1); err != nil {
		t.Fatalf("기권이 거부됐다: %v", err)
	}
	// 생존자(0~4) 전원 제출 시 완성 — 사망자 5는 제외
	for voter := 1; voter <= 4; voter++ {
		g.SubmitVote(voter, -1)
	}
	if !g.VoteComplete() {
		t.Fatal("생존자 전원 제출인데 VoteComplete=false")
	}
}

// TestSFWinConditions 마피아 0 → 시민 승, 마피아 ≥ 나머지 → 마피아 승,
// 종료 시 전원 역할 공개
func TestSFWinConditions(t *testing.T) {
	// 시민 승: 마지막 마피아 처형
	g := newSFStartedGame([]SFRole{
		SFRoleMafia, SFRolePolice, SFRoleDoctor,
		SFRoleCitizen, SFRoleCitizen, SFRoleCitizen,
	})
	g.Phase = SFPhaseDayResult
	g.BeginVote()
	for voter := 0; voter < 6; voter++ {
		g.SubmitVote(voter, 0)
	}
	g.ResolveVotes()
	if g.Winner != "citizen" || g.Phase != SFPhaseGameOver {
		t.Fatalf("시민 승 판정 실패: winner=%s phase=%s", g.Winner, g.Phase)
	}
	for _, p := range g.Players {
		if !p.RoleRevealed {
			t.Fatalf("종료 후 seat%d 역할 미공개", p.Seat)
		}
	}

	// 마피아 승: 밤 살해로 마피아 수 ≥ 나머지 생존자 수
	g = newSFStartedGame([]SFRole{
		SFRoleMafia, SFRoleMafia, SFRolePolice,
		SFRoleDoctor, SFRoleCitizen, SFRoleCitizen,
	})
	g.Players[4].Alive = false
	g.Players[5].Alive = false
	// 생존: 마(0,1) 경(2) 의(3) — 경찰 살해 시 2 vs 1
	g.SubmitNightAction(0, 2)
	g.SubmitNightAction(1, 2)
	g.SubmitNightAction(2, 0)
	g.SubmitNightAction(3, 3)
	if !g.NightComplete() {
		t.Fatal("사망자 제외 전원 제출인데 NightComplete=false")
	}
	killed := g.ResolveNight(rand.New(rand.NewSource(3)))
	if killed != 2 {
		t.Fatalf("살해 = seat%d, want seat2", killed)
	}
	if g.Winner != "mafia" || g.Phase != SFPhaseGameOver {
		t.Fatalf("마피아 승 판정 실패: winner=%s phase=%s", g.Winner, g.Phase)
	}
}

// TestSFNightCompleteSkipsDead 사망한 역할자는 해소 조건에서 빠진다 —
// 교착 없는 진행의 근거
func TestSFNightCompleteSkipsDead(t *testing.T) {
	g := newSFStartedGame([]SFRole{
		SFRoleMafia, SFRoleMafia, SFRolePolice,
		SFRoleDoctor, SFRoleCitizen, SFRoleCitizen,
	})
	g.Players[2].Alive = false // 경찰 사망
	g.Players[3].Alive = false // 의사 사망

	g.SubmitNightAction(0, 4)
	if g.NightComplete() {
		t.Fatal("마피아 1명 미제출인데 NightComplete=true")
	}
	g.SubmitNightAction(1, 4)
	if !g.NightComplete() {
		t.Fatal("생존 역할자 전원 제출인데 NightComplete=false")
	}
	killed := g.ResolveNight(rand.New(rand.NewSource(5)))
	if killed != 4 {
		t.Fatalf("살해 = seat%d, want seat4", killed)
	}
	// 생존: 마2 vs 시1 → 마피아 승
	if g.Winner != "mafia" {
		t.Fatalf("winner = %s, want mafia", g.Winner)
	}
}
