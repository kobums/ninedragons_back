package server

import (
	"math/rand"
	"testing"
)

// cnNewStartedGame 사람 n명이 앉아 시작까지 마친 순수 게임
func cnNewStartedGame(t *testing.T, n int) *CNGame {
	t.Helper()
	g := NewCNGame("test")
	for i := 0; i < n; i++ {
		if _, err := g.AddPlayer("사람", false); err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
	}
	if err := g.Start(rand.New(rand.NewSource(1))); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return g
}

// cnAgentSeat 팀의 첫 요원 좌석
func cnAgentSeat(t *testing.T, g *CNGame, team CNTeam) int {
	t.Helper()
	for _, p := range g.Players {
		if p.Team == team && p.Role == CNRoleAgent {
			return p.Seat
		}
	}
	t.Fatalf("%s 요원이 없다", team)
	return -1
}

// cnIndicesOf 키 카드에서 해당 색의 칸 목록
func cnIndicesOf(g *CNGame, color CNColor) []int {
	idx := []int{}
	for i, c := range g.KeyCard {
		if c == color {
			idx = append(idx, i)
		}
	}
	return idx
}

// TestCNWordPool 단어 풀 재사용 — 25개 중복 없이, 전부 스파이폴 카테고리
// 자산에서 나오며 여러 카테고리가 섞인다.
func TestCNWordPool(t *testing.T) {
	wordCategory := map[string]string{}
	for cat, words := range spCategories {
		for _, w := range words {
			wordCategory[w] = cat
		}
	}

	rng := rand.New(rand.NewSource(7))
	words := cnPickWords(rng)
	if len(words) != CNBoardSize {
		t.Fatalf("단어 수 = %d, want %d", len(words), CNBoardSize)
	}
	seen := map[string]bool{}
	cats := map[string]bool{}
	for _, w := range words {
		if seen[w] {
			t.Fatalf("중복 단어: %q", w)
		}
		seen[w] = true
		cat, ok := wordCategory[w]
		if !ok {
			t.Fatalf("스파이폴 자산에 없는 단어: %q", w)
		}
		cats[cat] = true
	}
	if len(cats) < 2 {
		t.Fatalf("카테고리 혼합 실패: %v", cats)
	}
}

// TestCNKeyCardComposition 키 카드 구성 — 적 9(선공), 청 8, 중립 7, 암살자 1
func TestCNKeyCardComposition(t *testing.T) {
	g := cnNewStartedGame(t, 4)

	if len(g.Board) != CNBoardSize || len(g.KeyCard) != CNBoardSize {
		t.Fatalf("board=%d keyCard=%d", len(g.Board), len(g.KeyCard))
	}
	count := map[CNColor]int{}
	for _, c := range g.KeyCard {
		count[c]++
	}
	if count[CNColorRed] != CNRedWords || count[CNColorBlue] != CNBlueWords ||
		count[CNColorNeutral] != CNNeutralWords || count[CNColorAssassin] != 1 {
		t.Fatalf("키 카드 구성 = %v", count)
	}
	if g.CurrentTeam != CNTeamRed || g.Phase != CNPhaseClue {
		t.Fatalf("선공 = %s, phase = %s (want red/clue)", g.CurrentTeam, g.Phase)
	}
	if g.RedLeft != CNRedWords || g.BlueLeft != CNBlueWords {
		t.Fatalf("잔여 = 적%d/청%d", g.RedLeft, g.BlueLeft)
	}
	for i, card := range g.Board {
		if card.Revealed {
			t.Fatalf("시작부터 공개된 카드: %d", i)
		}
	}
}

// TestCNTeamRoleAssignment 팀은 입장 순 번갈아, 스파이마스터는 사람 우선 —
// 사람이 1명뿐인 팀은 봇이 스파이마스터가 되어 그 사람이 요원으로 논다.
func TestCNTeamRoleAssignment(t *testing.T) {
	// 전원 사람 5인 (홀수 허용 — 남는 1명은 적팀 요원)
	g := NewCNGame("t1")
	for i := 0; i < 5; i++ {
		g.AddPlayer("사람", false)
	}
	wantTeams := []CNTeam{CNTeamRed, CNTeamBlue, CNTeamRed, CNTeamBlue, CNTeamRed}
	for i, p := range g.Players {
		if p.Team != wantTeams[i] {
			t.Fatalf("seat%d team = %s, want %s", i, p.Team, wantTeams[i])
		}
	}
	if g.Players[0].Role != CNRoleSpymaster || g.Players[1].Role != CNRoleSpymaster {
		t.Fatalf("스파이마스터 = %v/%v (want seat0/seat1)", g.Players[0].Role, g.Players[1].Role)
	}
	for _, s := range []int{2, 3, 4} {
		if g.Players[s].Role != CNRoleAgent {
			t.Fatalf("seat%d role = %s, want agent", s, g.Players[s].Role)
		}
	}

	// 사람 2 + 봇 4 — 각 팀 사람 1명뿐이라 봇이 스파이마스터
	g2 := NewCNGame("t2")
	g2.AddPlayer("호스트", false)
	g2.AddPlayer("친구", false)
	for i := 0; i < 4; i++ {
		g2.AddPlayer(botName, true)
	}
	if err := g2.Start(rand.New(rand.NewSource(2))); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if g2.Players[0].Role != CNRoleAgent || g2.Players[1].Role != CNRoleAgent {
		t.Fatalf("사람 역할 = %v/%v (want agent — 봇이 스파이마스터)",
			g2.Players[0].Role, g2.Players[1].Role)
	}
	if g2.SpymasterSeat(CNTeamRed) != 2 || g2.SpymasterSeat(CNTeamBlue) != 3 {
		t.Fatalf("봇 스파이마스터 = 적seat%d/청seat%d (want 2/3)",
			g2.SpymasterSeat(CNTeamRed), g2.SpymasterSeat(CNTeamBlue))
	}

	// 팀에 사람 2명 이상이면 첫 사람이 스파이마스터 (봇은 요원만)
	g3 := NewCNGame("t3")
	g3.AddPlayer(botName, true) // seat0 적팀 봇
	g3.AddPlayer("청1", false)
	g3.AddPlayer("적1", false)
	g3.AddPlayer("청2", false)
	g3.AddPlayer("적2", false)
	if g3.SpymasterSeat(CNTeamRed) != 2 { // 적팀 첫 사람
		t.Fatalf("적 스파이마스터 = seat%d, want 2", g3.SpymasterSeat(CNTeamRed))
	}
	if g3.SpymasterSeat(CNTeamBlue) != 1 {
		t.Fatalf("청 스파이마스터 = seat%d, want 1", g3.SpymasterSeat(CNTeamBlue))
	}
}

// TestCNClueAndPickFlow 힌트 기록 → 카드 선택의 정상·오류 경로.
// 맞으면 계속(최대 숫자+1회), 중립/상대 단어면 턴 종료.
func TestCNClueAndPickFlow(t *testing.T) {
	g := cnNewStartedGame(t, 4)
	redSpy := g.SpymasterSeat(CNTeamRed)
	blueSpy := g.SpymasterSeat(CNTeamBlue)
	redAgent := cnAgentSeat(t, g, CNTeamRed)
	blueAgent := cnAgentSeat(t, g, CNTeamBlue)

	// clue 단계 검증
	if err := g.GiveClue(redAgent, "바다", 2); err == nil {
		t.Fatal("요원의 힌트가 통과됐다")
	}
	if err := g.GiveClue(blueSpy, "바다", 2); err == nil {
		t.Fatal("상대 팀 스파이마스터의 힌트가 통과됐다")
	}
	if err := g.GiveClue(redSpy, "  ", 2); err == nil {
		t.Fatal("빈 힌트 단어가 통과됐다")
	}
	if err := g.GiveClue(redSpy, "바다", 0); err == nil {
		t.Fatal("숫자 0 힌트가 통과됐다")
	}
	if _, err := g.Pick(redAgent, 0); err == nil {
		t.Fatal("clue 단계의 선택이 통과됐다")
	}
	if err := g.GiveClue(redSpy, "바다", 2); err != nil {
		t.Fatalf("힌트 실패: %v", err)
	}
	if g.Phase != CNPhaseGuess || g.Clue == nil || g.Clue.Remaining != 3 {
		t.Fatalf("phase=%s clue=%+v", g.Phase, g.Clue)
	}
	if len(g.ClueHistory) != 1 || g.ClueHistory[0].Team != CNTeamRed || g.ClueHistory[0].Word != "바다" {
		t.Fatalf("clueHistory = %+v", g.ClueHistory)
	}

	// guess 단계 검증
	if _, err := g.Pick(blueAgent, 0); err == nil {
		t.Fatal("상대 팀 요원의 선택이 통과됐다")
	}
	if _, err := g.Pick(redSpy, 0); err == nil {
		t.Fatal("스파이마스터의 선택이 통과됐다")
	}
	if _, err := g.Pick(redAgent, -1); err == nil {
		t.Fatal("음수 인덱스가 통과됐다")
	}
	if _, err := g.Pick(redAgent, CNBoardSize); err == nil {
		t.Fatal("범위 밖 인덱스가 통과됐다")
	}

	// 자기 팀 단어 → 맞고 계속
	reds := cnIndicesOf(g, CNColorRed)
	res, err := g.Pick(redAgent, reds[0])
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if !res.Correct || res.TurnEnded || res.GameOver || g.Clue.Remaining != 2 {
		t.Fatalf("정답 선택 결과 = %+v, remaining=%d", res, g.Clue.Remaining)
	}
	if g.RedLeft != CNRedWords-1 {
		t.Fatalf("redLeft = %d", g.RedLeft)
	}
	if !g.Board[reds[0]].Revealed {
		t.Fatal("카드가 공개되지 않았다")
	}
	if _, err := g.Pick(redAgent, reds[0]); err == nil {
		t.Fatal("공개된 카드 재선택이 통과됐다")
	}

	// 중립 단어 → 턴 종료 (청팀 clue 단계로)
	neutral := cnIndicesOf(g, CNColorNeutral)[0]
	res, err = g.Pick(redAgent, neutral)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if res.Correct || !res.TurnEnded || g.CurrentTeam != CNTeamBlue ||
		g.Phase != CNPhaseClue || g.Clue != nil {
		t.Fatalf("중립 선택 후 상태 = %+v team=%s phase=%s", res, g.CurrentTeam, g.Phase)
	}

	// "그만" — 요원만, 자기 팀 차례에만
	if err := g.EndTurn(blueAgent); err == nil {
		t.Fatal("clue 단계의 그만이 통과됐다")
	}
	if err := g.GiveClue(blueSpy, "하늘", 1); err != nil {
		t.Fatalf("힌트 실패: %v", err)
	}
	if err := g.EndTurn(redAgent); err == nil {
		t.Fatal("상대 팀 요원의 그만이 통과됐다")
	}
	if err := g.EndTurn(blueSpy); err == nil {
		t.Fatal("스파이마스터의 그만이 통과됐다")
	}
	if err := g.EndTurn(blueAgent); err != nil {
		t.Fatalf("그만 실패: %v", err)
	}
	if g.CurrentTeam != CNTeamRed || g.Phase != CNPhaseClue {
		t.Fatalf("그만 후 team=%s phase=%s", g.CurrentTeam, g.Phase)
	}

	// 상대 팀 단어 → 상대 잔여 감소 + 턴 종료
	if err := g.GiveClue(redSpy, "노을", 1); err != nil {
		t.Fatalf("힌트 실패: %v", err)
	}
	blues := cnIndicesOf(g, CNColorBlue)
	res, err = g.Pick(redAgent, blues[0])
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if res.Correct || !res.TurnEnded || g.BlueLeft != CNBlueWords-1 || g.CurrentTeam != CNTeamBlue {
		t.Fatalf("상대 단어 선택 결과 = %+v blueLeft=%d", res, g.BlueLeft)
	}
}

// TestCNAssassinImmediateLoss 암살자 선택 → 그 팀 즉시 패배
func TestCNAssassinImmediateLoss(t *testing.T) {
	g := cnNewStartedGame(t, 4)
	redSpy := g.SpymasterSeat(CNTeamRed)
	redAgent := cnAgentSeat(t, g, CNTeamRed)

	if err := g.GiveClue(redSpy, "위험", 1); err != nil {
		t.Fatalf("힌트 실패: %v", err)
	}
	assassin := cnIndicesOf(g, CNColorAssassin)[0]
	res, err := g.Pick(redAgent, assassin)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if !res.GameOver || res.Winner != CNTeamBlue || res.LoseReason != "assassin" {
		t.Fatalf("암살자 결과 = %+v", res)
	}
	if g.Phase != CNPhaseGameOver || g.Winner != CNTeamBlue || g.LoseReason != "assassin" {
		t.Fatalf("게임 상태 = phase %s winner %s reason %q", g.Phase, g.Winner, g.LoseReason)
	}
	if _, err := g.Pick(redAgent, 0); err == nil {
		t.Fatal("종료 후 선택이 통과됐다")
	}
}

// TestCNWinByAllWords 자기 팀 단어를 다 까면 승리 — 상대가 마지막 단어를
// 까줘도 그 단어의 팀이 승리한다.
func TestCNWinByAllWords(t *testing.T) {
	g := cnNewStartedGame(t, 4)
	redSpy := g.SpymasterSeat(CNTeamRed)
	blueSpy := g.SpymasterSeat(CNTeamBlue)
	redAgent := cnAgentSeat(t, g, CNTeamRed)
	blueAgent := cnAgentSeat(t, g, CNTeamBlue)

	// 적팀이 8개까지 연속 정답 (힌트 9 → 선택 최대 10회)
	if err := g.GiveClue(redSpy, "전부", 9); err != nil {
		t.Fatalf("힌트 실패: %v", err)
	}
	reds := cnIndicesOf(g, CNColorRed)
	for i := 0; i < CNRedWords-1; i++ {
		res, err := g.Pick(redAgent, reds[i])
		if err != nil {
			t.Fatalf("Pick %d: %v", i, err)
		}
		if !res.Correct || res.TurnEnded || res.GameOver {
			t.Fatalf("연속 정답 %d 결과 = %+v", i, res)
		}
	}
	if g.RedLeft != 1 {
		t.Fatalf("redLeft = %d, want 1", g.RedLeft)
	}

	// 적팀이 "그만" → 청팀 차례에 청 요원이 적팀 마지막 단어를 까면 적팀 승리
	if err := g.EndTurn(redAgent); err != nil {
		t.Fatalf("그만 실패: %v", err)
	}
	if err := g.GiveClue(blueSpy, "실수", 1); err != nil {
		t.Fatalf("힌트 실패: %v", err)
	}
	res, err := g.Pick(blueAgent, reds[CNRedWords-1])
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if !res.GameOver || res.Winner != CNTeamRed || res.LoseReason != "" || res.Correct {
		t.Fatalf("마지막 단어 결과 = %+v", res)
	}
	if g.Phase != CNPhaseGameOver || g.Winner != CNTeamRed || g.RedLeft != 0 {
		t.Fatalf("게임 상태 = phase %s winner %s redLeft %d", g.Phase, g.Winner, g.RedLeft)
	}
}

// TestCNRemainingExhaustEndsTurn 숫자+1회를 다 쓰면 자동으로 턴이 넘어간다
func TestCNRemainingExhaustEndsTurn(t *testing.T) {
	g := cnNewStartedGame(t, 4)
	redSpy := g.SpymasterSeat(CNTeamRed)
	redAgent := cnAgentSeat(t, g, CNTeamRed)

	if err := g.GiveClue(redSpy, "둘", 1); err != nil { // 선택 2회
		t.Fatalf("힌트 실패: %v", err)
	}
	reds := cnIndicesOf(g, CNColorRed)
	if res, err := g.Pick(redAgent, reds[0]); err != nil || res.TurnEnded {
		t.Fatalf("1번째 정답: res=%+v err=%v", res, err)
	}
	res, err := g.Pick(redAgent, reds[1])
	if err != nil {
		t.Fatalf("2번째 정답: %v", err)
	}
	if !res.Correct || !res.TurnEnded || g.CurrentTeam != CNTeamBlue || g.Phase != CNPhaseClue {
		t.Fatalf("소진 후 상태 = %+v team=%s phase=%s", res, g.CurrentTeam, g.Phase)
	}
}

// TestCNStartValidation 4인 미만 시작 불가, 시작 후 입장 불가
func TestCNStartValidation(t *testing.T) {
	g := NewCNGame("t")
	for i := 0; i < 3; i++ {
		g.AddPlayer("사람", false)
	}
	if g.CanStart() {
		t.Fatal("3인 시작이 허용됐다")
	}
	if err := g.Start(rand.New(rand.NewSource(1))); err == nil {
		t.Fatal("3인 Start 가 통과됐다")
	}
	g.AddPlayer("사람", false)
	if err := g.Start(rand.New(rand.NewSource(1))); err != nil {
		t.Fatalf("4인 Start 실패: %v", err)
	}
	if _, err := g.AddPlayer("지각생", false); err == nil {
		t.Fatal("시작 후 입장이 통과됐다")
	}
}
