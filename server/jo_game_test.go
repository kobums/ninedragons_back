package server

import (
	"fmt"
	"math/rand"
	"testing"
)

// joTestGame n인 게임을 시작한 상태로 돌려준다 (제시어는 테스트가 덮어쓴다)
func joTestGame(t *testing.T, n int) *JOGame {
	t.Helper()
	g := NewJOGame("test")
	for i := 0; i < n; i++ {
		if _, err := g.AddPlayer(fmt.Sprintf("P%d", i)); err != nil {
			t.Fatalf("AddPlayer(%d): %v", i, err)
		}
	}
	if err := g.Start(rand.New(rand.NewSource(7))); err != nil {
		t.Fatalf("Start: %v", err)
	}
	g.DrainEvents()
	return g
}

// joSubmitAll 출제자를 뺀 좌석에 순서대로 단서를 넣는다 (좌석 오름차순)
func joSubmitAll(t *testing.T, g *JOGame, clues ...string) {
	t.Helper()
	i := 0
	for _, p := range g.Players {
		if p.Seat == g.GuesserSeat {
			continue
		}
		if i >= len(clues) {
			break
		}
		if err := g.SubmitClue(p.Seat, clues[i]); err != nil {
			t.Fatalf("SubmitClue(seat%d, %q): %v", p.Seat, clues[i], err)
		}
		i++
	}
}

// ==================== 정규화 ====================

func TestJONormalize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"앞뒤 공백 제거", "  사과  ", "사과"},
		{"내부 공백 제거", "빨간 사과", "빨간사과"},
		{"앞뒤+내부+소문자", " Red  Apple ", "redapple"},
		{"탭·개행도 공백", "\t사과\n", "사과"},
		{"소문자화", "AbC", "abc"},
		{"빈 문자열", "", ""},
		{"공백만", "   ", ""},
		{"전부 결합", "  A p P l E \t", "apple"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := joNormalize(tc.in); got != tc.want {
				t.Fatalf("joNormalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// ==================== 소거 규칙 ====================

func TestJOEliminate(t *testing.T) {
	cases := []struct {
		name  string
		word  string
		clues []string
		want  []bool
	}{
		{"전부 생존", "사과", []string{"빨강", "노랑", "달다"}, []bool{false, false, false}},
		{"중복 둘은 둘 다 소거", "사과", []string{"빨강", "빨강", "달다"}, []bool{true, true, false}},
		{"중복 셋은 셋 다 소거", "사과", []string{"빨강", "빨강", "빨강"}, []bool{true, true, true}},
		{"정규화 후 중복 (앞뒤 공백)", "사과", []string{"달다", "  달다 "}, []bool{true, true}},
		{"정규화 후 중복 (내부 공백)", "사과", []string{"빨간색", "빨 간 색"}, []bool{true, true}},
		{"정규화 후 중복 (대소문자)", "사과", []string{"Apple", "apple"}, []bool{true, true}},
		{"제시어와 완전 일치", "사과", []string{"사과", "달다"}, []bool{true, false}},
		{"제시어와 일치 (공백 정규화)", "사과", []string{"사 과", "달다"}, []bool{true, false}},
		{"제시어와 일치 (대소문자)", "sagwa", []string{"SaGwa"}, []bool{true}},
		{"단서가 제시어를 포함", "사과", []string{"사과주스", "달다"}, []bool{true, false}},
		{"단서가 제시어에 포함", "사과주스", []string{"사과", "달다"}, []bool{true, false}},
		{"한 글자 포함 관계", "사과", []string{"과", "포도"}, []bool{true, false}},
		{"빈 단서", "사과", []string{"", "달다"}, []bool{true, false}},
		{"공백뿐인 단서", "사과", []string{"   ", "달다"}, []bool{true, false}},
		{"빈 단서 여럿은 중복 집계 안 함", "사과", []string{"", "", "달다"}, []bool{true, true, false}},
		{"제시어 관계와 중복이 겹칠 때", "사과", []string{"사과", "사과", "달다"}, []bool{true, true, false}},
		{"단서 없음", "사과", []string{}, []bool{}},
		{"제시어가 비어도 터지지 않는다", "", []string{"가", "가", "나"}, []bool{true, true, false}},
		{"서로 다른 포함 관계는 소거 대상 아님", "동물", []string{"강아지", "강아지풀"}, []bool{false, false}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := joEliminate(tc.word, tc.clues)
			if len(got) != len(tc.clues) {
				t.Fatalf("길이 = %d, want %d", len(got), len(tc.clues))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("joEliminate(%q, %v) = %v, want %v", tc.word, tc.clues, got, tc.want)
				}
			}
		})
	}
}

func TestJOClueKilledByWord(t *testing.T) {
	cases := []struct {
		word, clue string
		want       bool
	}{
		{"사과", "사과", true},
		{"사과", " 사 과 ", true},
		{"사과", "사과주스", true},
		{"사과주스", "사과", true},
		{"사과", "과", true},
		{"사과", "", true},
		{"사과", "  ", true},
		{"사과", "빨강", false},
		{"", "빨강", false},
	}
	for _, tc := range cases {
		if got := joClueKilledByWord(tc.word, tc.clue); got != tc.want {
			t.Fatalf("joClueKilledByWord(%q, %q) = %t, want %t", tc.word, tc.clue, got, tc.want)
		}
	}
}

func TestJOSurvivors(t *testing.T) {
	clues := []JOClueView{
		{Seat: 1, Text: "가", Removed: true},
		{Seat: 2, Text: "나"},
		{Seat: 3, Text: "다", Removed: true},
	}
	alive := joSurvivors(clues)
	if len(alive) != 1 || alive[0].Seat != 2 {
		t.Fatalf("joSurvivors = %+v", alive)
	}
	// 전부 소거돼도 nil 이 아니라 빈 배열이어야 한다 (JSON null 금지)
	empty := joSurvivors([]JOClueView{{Removed: true}})
	if empty == nil || len(empty) != 0 {
		t.Fatalf("전멸 시 = %#v, want 빈 배열", empty)
	}
	if joSurvivors(nil) == nil {
		t.Fatal("nil 입력에도 빈 배열이어야 한다")
	}
}

// ==================== 점수 등급 ====================

func TestJOGradeAndSuccess(t *testing.T) {
	cases := []struct {
		score, total int
		wantGrade    string
		wantSuccess  bool
	}{
		{8, 8, "만점", true},
		{7, 8, "우수", true},
		{6, 8, "우수", true},
		{5, 8, "보통", true},
		{4, 8, "보통", true}, // 절반 정확히 — 성공
		{3, 8, "재도전", false},
		{0, 8, "재도전", false},
		{6, 6, "만점", true},
		{3, 6, "보통", true},
		{2, 6, "재도전", false},
		{0, 0, "재도전", false}, // 시작 전 방어
	}
	for _, tc := range cases {
		if got := joGrade(tc.score, tc.total); got != tc.wantGrade {
			t.Fatalf("joGrade(%d,%d) = %q, want %q", tc.score, tc.total, got, tc.wantGrade)
		}
		if got := joSuccess(tc.score, tc.total); got != tc.wantSuccess {
			t.Fatalf("joSuccess(%d,%d) = %t, want %t", tc.score, tc.total, got, tc.wantSuccess)
		}
		if msg := joGradeMessage(tc.score, tc.total); msg == "" {
			t.Fatalf("등급 문구가 비었다 (%d/%d)", tc.score, tc.total)
		}
	}
}

// ==================== 라운드 진행 ====================

// TestJORoundFlow 한 라운드의 전 구간 — 단서 수집 → 소거 → 정답 → 정산
func TestJORoundFlow(t *testing.T) {
	g := joTestGame(t, 4)
	if g.TotalRounds != 8 {
		t.Fatalf("4인 총 라운드 = %d, want 8", g.TotalRounds)
	}
	if g.Round != 1 || g.GuesserSeat != 0 || g.Phase != JOPhaseClue {
		t.Fatalf("시작 상태 = R%d guesser%d %s", g.Round, g.GuesserSeat, g.Phase)
	}
	g.Word = "사과"

	// 출제자는 단서를 못 낸다
	if err := g.SubmitClue(0, "빨강"); err == nil {
		t.Fatal("출제자의 단서 제출이 통과했다")
	}
	// 두 명만 내면 아직 단서 단계
	joSubmitAll(t, g, "빨강", "빨강")
	if g.Phase != JOPhaseClue || g.SubmittedCount() != 2 {
		t.Fatalf("2명 제출 후 = %s (%d명)", g.Phase, g.SubmittedCount())
	}
	// 재제출은 잠긴다
	if err := g.SubmitClue(1, "노랑"); err == nil {
		t.Fatal("재제출이 통과했다")
	}
	// 마지막 한 명이 내면 즉시 추리 단계
	if err := g.SubmitClue(3, "동그라미"); err != nil {
		t.Fatalf("SubmitClue: %v", err)
	}
	if g.Phase != JOPhaseGuess {
		t.Fatalf("전원 제출 후 phase = %s", g.Phase)
	}
	if len(g.Clues) != 3 {
		t.Fatalf("단서 수 = %d", len(g.Clues))
	}
	if !g.Clues[0].Removed || !g.Clues[1].Removed || g.Clues[2].Removed {
		t.Fatalf("소거 결과 = %+v", g.Clues)
	}
	if len(joSurvivors(g.Clues)) != 1 {
		t.Fatalf("생존 단서 = %+v", joSurvivors(g.Clues))
	}

	// 출제자가 아닌 사람은 답을 못 낸다
	if err := g.SubmitGuess(1, "사과"); err == nil {
		t.Fatal("비출제자의 답이 통과했다")
	}
	// 정답 — 정규화 일치 (공백·대소문자 무시)
	if err := g.SubmitGuess(0, " 사 과 "); err != nil {
		t.Fatalf("SubmitGuess: %v", err)
	}
	if g.Phase != JOPhaseRoundEnd || g.Score != 1 {
		t.Fatalf("정답 후 = %s score=%d", g.Phase, g.Score)
	}
	if g.Judged == nil || !g.Judged.Correct || g.Judged.Accepted {
		t.Fatalf("judged = %+v", g.Judged)
	}
	if len(g.History) != 1 || g.History[0].Word != "사과" || !g.History[0].Correct {
		t.Fatalf("history = %+v", g.History)
	}

	// 다음 라운드 — 출제자는 좌석 순으로 돈다
	g.NextRound()
	if g.Round != 2 || g.GuesserSeat != 1 || g.Phase != JOPhaseClue {
		t.Fatalf("2라운드 = R%d guesser%d %s", g.Round, g.GuesserSeat, g.Phase)
	}
	if g.Judged != nil || g.Guess != "" || len(g.Clues) != 0 {
		t.Fatalf("라운드 상태가 초기화되지 않았다: judged=%+v guess=%q clues=%v",
			g.Judged, g.Guess, g.Clues)
	}
	for _, p := range g.Players {
		if p.Submitted || p.Clue != "" {
			t.Fatalf("seat%d 단서가 남아 있다: %+v", p.Seat, p)
		}
	}
}

// TestJOJudgingAndScore 오답 → 인정 창 → 인정/미인정, 넘김, 0점 하한
func TestJOJudgingAndScore(t *testing.T) {
	g := joTestGame(t, 4)
	g.Word = "사과"
	joSubmitAll(t, g, "빨강", "노랑", "동그라미")
	if g.Phase != JOPhaseGuess {
		t.Fatalf("phase = %s", g.Phase)
	}

	// 오답 → 인정 창
	if err := g.SubmitGuess(0, "포도"); err != nil {
		t.Fatalf("SubmitGuess: %v", err)
	}
	if g.Phase != JOPhaseJudging || g.Guess != "포도" {
		t.Fatalf("오답 후 = %s guess=%q", g.Phase, g.Guess)
	}
	// 출제자는 자기 답을 인정할 수 없다
	if err := g.Accept(0); err == nil {
		t.Fatal("출제자의 자기 인정이 통과했다")
	}
	// 한 명이면 충분하다 (협력 게임)
	if err := g.Accept(2); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if g.Score != 1 || g.Judged == nil || !g.Judged.Correct || !g.Judged.Accepted {
		t.Fatalf("인정 후 score=%d judged=%+v", g.Score, g.Judged)
	}
	if !g.History[0].Correct {
		t.Fatalf("인정 라운드가 기록에 오답으로 남았다: %+v", g.History[0])
	}

	// 2라운드 — 아무도 인정하지 않으면 오답 (-1)
	g.NextRound()
	g.Word = "바나나"
	joSubmitAll(t, g, "노랑", "길다", "원숭이")
	if err := g.SubmitGuess(1, "레몬"); err != nil {
		t.Fatalf("SubmitGuess: %v", err)
	}
	g.CloseJudging()
	if g.Score != 0 || g.Judged.Correct || g.Judged.Accepted {
		t.Fatalf("미인정 후 score=%d judged=%+v", g.Score, g.Judged)
	}

	// 3라운드 — 오답이 이어져도 0 미만으로 내려가지 않는다
	g.NextRound()
	g.Word = "호랑이"
	joSubmitAll(t, g, "줄무늬", "맹수", "산")
	if err := g.SubmitGuess(2, "고양이"); err != nil {
		t.Fatalf("SubmitGuess: %v", err)
	}
	g.CloseJudging()
	if g.Score != 0 {
		t.Fatalf("0점 하한이 깨졌다: score=%d", g.Score)
	}

	// 4라운드 — 넘김은 0점, 답은 빈 문자열
	g.NextRound()
	g.Word = "축구"
	joSubmitAll(t, g, "공", "골대", "월드컵")
	if err := g.Pass(3); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if g.Score != 0 || g.Guess != "" || g.Judged.Correct {
		t.Fatalf("넘김 후 score=%d guess=%q judged=%+v", g.Score, g.Guess, g.Judged)
	}
	if len(g.History) != 4 {
		t.Fatalf("history 길이 = %d", len(g.History))
	}
}

// TestJOAutoCloseAndFinish 마감 자동 처리(빈 단서·자동 넘김)와 마지막 라운드 종료
func TestJOAutoCloseAndFinish(t *testing.T) {
	g := joTestGame(t, 3)
	if g.TotalRounds != 6 {
		t.Fatalf("3인 총 라운드 = %d, want 6", g.TotalRounds)
	}

	// 아무도 단서를 내지 않고 마감 → 빈 단서로 전부 소거
	g.ForceCloseClues()
	if g.Phase != JOPhaseGuess {
		t.Fatalf("단서 마감 후 phase = %s", g.Phase)
	}
	if len(g.Clues) != 2 {
		t.Fatalf("단서 수 = %d", len(g.Clues))
	}
	for _, c := range g.Clues {
		if !c.Removed || c.Text != "" {
			t.Fatalf("미제출이 빈 단서로 처리되지 않았다: %+v", c)
		}
	}
	if len(joSurvivors(g.Clues)) != 0 {
		t.Fatalf("생존 단서가 있다: %+v", joSurvivors(g.Clues))
	}

	// 출제자도 무응답 → 자동 넘김 (0점)
	g.ForcePass()
	if g.Phase != JOPhaseRoundEnd || g.Score != 0 {
		t.Fatalf("자동 넘김 후 = %s score=%d", g.Phase, g.Score)
	}

	// 남은 라운드를 마감만으로 소진 → 반드시 끝난다
	for i := 0; i < 20 && g.Phase != JOPhaseGameOver; i++ {
		switch g.Phase {
		case JOPhaseClue:
			g.ForceCloseClues()
		case JOPhaseGuess:
			g.ForcePass()
		case JOPhaseJudging:
			g.CloseJudging()
		case JOPhaseRoundEnd:
			g.NextRound()
		}
	}
	if g.Phase != JOPhaseGameOver {
		t.Fatalf("마감만으로 끝나지 않았다: %s (R%d)", g.Phase, g.Round)
	}
	if g.Round != g.TotalRounds || len(g.History) != g.TotalRounds {
		t.Fatalf("종료 시 R%d history=%d, want %d", g.Round, len(g.History), g.TotalRounds)
	}
	if g.GuesserSeat != -1 {
		t.Fatalf("종료 후 guesserSeat = %d", g.GuesserSeat)
	}
	if joSuccess(g.Score, g.TotalRounds) {
		t.Fatalf("전원 방치인데 성공 판정 (score=%d)", g.Score)
	}
}

// TestJOGuesserRotation 출제자가 좌석 순으로 정확히 두 바퀴 돈다
func TestJOGuesserRotation(t *testing.T) {
	g := joTestGame(t, 5)
	if g.TotalRounds != 10 {
		t.Fatalf("5인 총 라운드 = %d", g.TotalRounds)
	}
	counts := map[int]int{}
	for g.Phase != JOPhaseGameOver {
		if g.Phase == JOPhaseClue {
			if want := (g.Round - 1) % 5; g.GuesserSeat != want {
				t.Fatalf("R%d 출제자 = %d, want %d", g.Round, g.GuesserSeat, want)
			}
			counts[g.GuesserSeat]++
		}
		switch g.Phase {
		case JOPhaseClue:
			g.ForceCloseClues()
		case JOPhaseGuess:
			g.ForcePass()
		case JOPhaseJudging:
			g.CloseJudging()
		case JOPhaseRoundEnd:
			g.NextRound()
		}
	}
	for seat := 0; seat < 5; seat++ {
		if counts[seat] != JORoundsPerPlayer {
			t.Fatalf("seat%d 출제 횟수 = %d, want %d", seat, counts[seat], JORoundsPerPlayer)
		}
	}
}

// TestJOClueValidation 단서·답 입력 검증 (길이·빈 값·단계)
func TestJOClueValidation(t *testing.T) {
	g := joTestGame(t, 3)
	g.Word = "사과"

	if err := g.SubmitClue(1, "   "); err == nil {
		t.Fatal("공백뿐인 단서가 통과했다")
	}
	long := ""
	for i := 0; i <= JOMaxClueLen; i++ {
		long += "가"
	}
	if err := g.SubmitClue(1, long); err == nil {
		t.Fatalf("%d자 단서가 통과했다", len([]rune(long)))
	}
	exact := ""
	for i := 0; i < JOMaxClueLen; i++ {
		exact += "가"
	}
	if err := g.SubmitClue(1, exact); err != nil {
		t.Fatalf("%d자 단서가 거부됐다: %v", JOMaxClueLen, err)
	}
	// 잘못된 좌석
	if err := g.SubmitClue(99, "가"); err == nil {
		t.Fatal("범위 밖 좌석의 단서가 통과했다")
	}
	// 단서 단계에는 답을 낼 수 없다
	if err := g.SubmitGuess(0, "사과"); err == nil {
		t.Fatal("단서 단계의 답이 통과했다")
	}
	if err := g.Accept(1); err == nil {
		t.Fatal("단서 단계의 인정이 통과했다")
	}

	g.ForceCloseClues()
	if err := g.SubmitGuess(0, "  "); err == nil {
		t.Fatal("공백뿐인 답이 통과했다")
	}
	if err := g.SubmitGuess(0, long); err == nil {
		t.Fatal("길이 초과 답이 통과했다")
	}
	if err := g.SubmitClue(1, "가"); err == nil {
		t.Fatal("추리 단계의 단서가 통과했다")
	}
}

// TestJOLobbySeats 대기실 입·퇴장과 좌석 압축
func TestJOLobbySeats(t *testing.T) {
	g := NewJOGame("lobby")
	if g.CanStart() {
		t.Fatal("빈 대기실이 시작 가능하다")
	}
	for i := 0; i < JOMaxPlayers; i++ {
		if _, err := g.AddPlayer(fmt.Sprintf("P%d", i)); err != nil {
			t.Fatalf("AddPlayer(%d): %v", i, err)
		}
	}
	if _, err := g.AddPlayer("초과"); err == nil {
		t.Fatalf("정원(%d) 초과 입장이 통과했다", JOMaxPlayers)
	}
	g.RemovePlayer(0)
	if len(g.Players) != JOMaxPlayers-1 {
		t.Fatalf("퇴장 후 인원 = %d", len(g.Players))
	}
	for i, p := range g.Players {
		if p.Seat != i {
			t.Fatalf("좌석 압축 실패: %d번째 = seat%d", i, p.Seat)
		}
	}
	if err := g.Start(rand.New(rand.NewSource(1))); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := g.AddPlayer("난입"); err == nil {
		t.Fatal("시작된 게임에 입장이 통과했다")
	}
}

// ==================== 제시어 풀 헬퍼 ====================

func TestJOWordHelpers(t *testing.T) {
	pool := joWordPool()
	if len(pool) < 100 {
		t.Fatalf("제시어 풀 크기 = %d", len(pool))
	}

	// 모든 제시어는 카테고리를 찾을 수 있어야 한다 (봇 단서의 근거)
	for _, w := range pool {
		if joCategoryOf(w) == "" {
			t.Fatalf("카테고리를 못 찾는 제시어: %q", w)
		}
	}
	if joCategoryOf("존재하지않는단어") != "" {
		t.Fatal("없는 단어에 카테고리가 붙었다")
	}
	if len(joCategoryWords("없는카테고리")) != 0 {
		t.Fatal("없는 카테고리에 단어가 있다")
	}
	// 사본이라 원본이 오염되지 않는다
	words := joCategoryWords(spCategoryNames[0])
	if len(words) == 0 {
		t.Fatal("카테고리 단어가 비었다")
	}
	words[0] = "오염"
	if spCategories[spCategoryNames[0]][0] == "오염" {
		t.Fatal("joCategoryWords 가 원본을 노출했다")
	}

	// 라운드 수만큼 뽑고, 최대 인원(7인 14라운드)에서도 중복이 없다
	rng := rand.New(rand.NewSource(3))
	picked := joPickWords(rng, JOMaxPlayers*JORoundsPerPlayer)
	if len(picked) != JOMaxPlayers*JORoundsPerPlayer {
		t.Fatalf("뽑은 제시어 수 = %d", len(picked))
	}
	seen := map[string]bool{}
	for _, w := range picked {
		if seen[w] {
			t.Fatalf("제시어 중복: %q", w)
		}
		seen[w] = true
	}
	if got := joPickWords(rng, 0); len(got) != 0 || got == nil {
		t.Fatalf("0개 요청 = %#v, want 빈 배열", got)
	}
}

// TestJOBotWordChoice 봇의 단서·답 고르기 — 같은 카테고리에서, 소거되지 않을 단어로
func TestJOBotWordChoice(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	for _, word := range []string{"사과", "축구", "호랑이", "김치찌개"} {
		category := joCategoryOf(word)
		for i := 0; i < 50; i++ {
			clue := joRelatedWord(rng, word)
			if clue == "" {
				t.Fatalf("%q 의 연상 단서를 못 골랐다", word)
			}
			if joCategoryOf(clue) != category {
				t.Fatalf("%q(%s) 의 단서 %q 가 다른 카테고리(%s)",
					word, category, clue, joCategoryOf(clue))
			}
			if joClueKilledByWord(word, clue) {
				t.Fatalf("%q 의 단서 %q 는 곧바로 소거된다", word, clue)
			}
		}
	}

	// 출제자 봇은 정답과 오답을 섞어 낸다 (사람 실력 시뮬레이션)
	correct := 0
	const tries = 400
	for i := 0; i < tries; i++ {
		if joBotGuess(rng, "사과") == "사과" {
			correct++
		}
	}
	if correct == 0 || correct == tries {
		t.Fatalf("출제자 봇 정답 수 = %d/%d (섞여야 한다)", correct, tries)
	}

	// 인정은 같은 카테고리 오답에만 걸린다
	for i := 0; i < 200; i++ {
		if joBotWillAccept(rng, "사과", "축구") {
			t.Fatal("다른 카테고리 오답을 인정했다")
		}
	}
	accepted := 0
	for i := 0; i < 400; i++ {
		if joBotWillAccept(rng, "사과", "포도") {
			accepted++
		}
	}
	if accepted == 0 {
		t.Fatal("같은 카테고리 오답을 한 번도 인정하지 않았다")
	}
}
