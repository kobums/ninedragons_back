package server

import "testing"

// 히든 없이 정상 라운드: 합계가 큰 팀이 승리하고, 서로 상대의 더 큰 블록을 받는다
func TestNCProcessRoundNormalExchange(t *testing.T) {
	g := NewNCGame("test")

	if err := g.SubmitBlocks(Team1, 7, 6, false, 0); err != nil {
		t.Fatalf("team1 submit failed: %v", err)
	}
	if err := g.SubmitBlocks(Team2, 1, 2, false, 0); err != nil {
		t.Fatalf("team2 submit failed: %v", err)
	}

	result, err := g.ProcessRound()
	if err != nil {
		t.Fatalf("ProcessRound failed: %v", err)
	}

	if result.Winner != Team1 {
		t.Errorf("winner = %s, want team1", result.Winner)
	}
	if g.Team1Score != 1 || g.Team2Score != 0 {
		t.Errorf("score = %d:%d, want 1:0", g.Team1Score, g.Team2Score)
	}
	if result.Team1ReceivedBlock != 2 {
		t.Errorf("team1 received = %d, want 2 (opponent's larger block)", result.Team1ReceivedBlock)
	}
	if result.Team2ReceivedBlock != 7 {
		t.Errorf("team2 received = %d, want 7 (opponent's larger block)", result.Team2ReceivedBlock)
	}
	// 블록 수: 14 - 2(제출) + 1(수령) = 13
	if len(g.AvailableBlocks[Team1]) != 13 {
		t.Errorf("team1 blocks = %d, want 13", len(g.AvailableBlocks[Team1]))
	}
}

// 회귀: 양팀 모두 히든을 쓴 라운드에서 점수가 두 번 가산되면 안 된다
// (첫 번째 선택자 시점의 조기 ProcessRound 호출이 상태를 오염시키던 버그)
func TestNCProcessRoundBothHiddenScoresOnce(t *testing.T) {
	g := NewNCGame("test")

	if err := g.SubmitBlocks(Team1, 7, 6, true, 0); err != nil {
		t.Fatalf("team1 submit failed: %v", err)
	}
	if err := g.SubmitBlocks(Team2, 1, 2, true, 0); err != nil {
		t.Fatalf("team2 submit failed: %v", err)
	}

	// 팀1만 블록을 선택한 상태: 아직 처리되면 안 된다
	g.RoundSubmits[Team1].SelectedBlockChoice = 1
	if g.ReadyToProcess() {
		t.Fatal("ReadyToProcess should be false before team2 selects")
	}

	// 조기 호출(과거 허브 동작 재현)이 실패하더라도 상태를 바꾸면 안 된다
	if _, err := g.ProcessRound(); err == nil {
		t.Fatal("expected error before team2 selects")
	}
	if g.Team1Score != 0 || g.Team2Score != 0 {
		t.Fatalf("failed ProcessRound mutated score: %d:%d", g.Team1Score, g.Team2Score)
	}
	if g.Team1UsedHidden || g.Team2UsedHidden {
		t.Fatal("failed ProcessRound mutated hidden flags")
	}

	// 팀2도 선택 완료 후 정상 처리
	g.RoundSubmits[Team2].SelectedBlockChoice = 2
	if !g.ReadyToProcess() {
		t.Fatal("ReadyToProcess should be true after both selects")
	}
	result, err := g.ProcessRound()
	if err != nil {
		t.Fatalf("ProcessRound failed: %v", err)
	}

	if g.Team1Score != 1 {
		t.Errorf("Team1Score = %d, want exactly 1", g.Team1Score)
	}
	// 히든 선택: 팀1은 팀2의 블록1(=1), 팀2는 팀1의 블록2(=6)를 받는다
	if result.Team1ReceivedBlock != 1 {
		t.Errorf("team1 received = %d, want 1 (chosen block1)", result.Team1ReceivedBlock)
	}
	if result.Team2ReceivedBlock != 6 {
		t.Errorf("team2 received = %d, want 6 (chosen block2)", result.Team2ReceivedBlock)
	}
	if !g.Team1UsedHidden || !g.Team2UsedHidden {
		t.Error("hidden usage flags should be recorded")
	}
}

// 회귀: 상대가 히든을 먼저 쓰고 내가 제출 페이로드에 블록 선택을 포함해 보내면
// nc_select_block 없이도 라운드가 처리 가능 상태여야 한다 (교착 버그)
func TestNCReadyToProcessWithChoiceInSubmit(t *testing.T) {
	g := NewNCGame("test")

	// 팀2가 히든으로 먼저 제출
	if err := g.SubmitBlocks(Team2, 3, 4, true, 0); err != nil {
		t.Fatalf("team2 submit failed: %v", err)
	}
	// 팀1은 히든 알림을 받고 선택(choice=1)을 포함해 제출
	if err := g.SubmitBlocks(Team1, 5, 5, false, 1); err != nil {
		t.Fatalf("team1 submit failed: %v", err)
	}

	if !g.ReadyToProcess() {
		t.Fatal("ReadyToProcess should be true when choice was included in submit")
	}
	if _, err := g.ProcessRound(); err != nil {
		t.Fatalf("ProcessRound failed: %v", err)
	}
}

// 회귀: 같은 숫자 블록을 두 개 제출하려면 실제로 두 개를 보유해야 한다
func TestNCSubmitBlocksDuplicateNeedsTwoCopies(t *testing.T) {
	g := NewNCGame("test")

	// 3이 한 개만 남은 상황
	g.AvailableBlocks[Team1] = []int{1, 2, 3}
	if err := g.SubmitBlocks(Team1, 3, 3, false, 0); err == nil {
		t.Error("expected error when submitting duplicate with only one copy")
	}

	// 두 개 보유 시에는 허용
	g.AvailableBlocks[Team1] = []int{3, 3, 5}
	if err := g.SubmitBlocks(Team1, 3, 3, false, 0); err != nil {
		t.Errorf("submit with two copies should succeed: %v", err)
	}
}

// 같은 라운드에 재제출은 거부된다
func TestNCSubmitBlocksRejectResubmit(t *testing.T) {
	g := NewNCGame("test")

	if err := g.SubmitBlocks(Team1, 1, 2, false, 0); err != nil {
		t.Fatalf("first submit failed: %v", err)
	}
	if err := g.SubmitBlocks(Team1, 6, 7, false, 0); err == nil {
		t.Error("expected error on resubmit in the same round")
	}
}
