package server

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// newSPStartedGame 스파이·장소를 지정해 playing 상태의 게임을 만든다
// (순수 로직 검증용)
func newSPStartedGame(n, spySeat int, location string) *SPGame {
	g := NewSPGame("test")
	for i := 0; i < n; i++ {
		g.AddPlayer(fmt.Sprintf("p%d", i))
	}
	g.Phase = SPPhasePlaying
	g.SpySeat = spySeat
	g.Category = "장소"
	g.Location = location
	g.Ready = true
	return g
}

// TestSPLocationList 카테고리 계약 — 이름 목록 일치, 각 24단어·중복 없음
func TestSPLocationList(t *testing.T) {
	if len(spCategoryNames) != len(spCategories) {
		t.Fatalf("카테고리 이름 %d개 vs 목록 %d개", len(spCategoryNames), len(spCategories))
	}
	for _, name := range spCategoryNames {
		words, ok := spCategories[name]
		if !ok {
			t.Fatalf("카테고리 누락: %s", name)
		}
		if len(words) != 24 {
			t.Fatalf("%s 단어 수 = %d, want 24", name, len(words))
		}
		seen := map[string]bool{}
		for _, w := range words {
			if w == "" {
				t.Fatalf("%s 에 빈 단어가 있다", name)
			}
			if seen[w] {
				t.Fatalf("%s 단어 중복: %s", name, w)
			}
			seen[w] = true
		}
	}
}

// TestSPCategoryAssign 카테고리 배정 — 지정 카테고리 준수, 랜덤은 목록 중 하나
func TestSPCategoryAssign(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	g := NewSPGame("c1")
	for i := 0; i < 4; i++ {
		g.AddPlayer(fmt.Sprintf("p%d", i))
	}
	if err := g.SetCategory("과일"); err != nil {
		t.Fatal(err)
	}
	if err := g.Start(rng, time.Minute); err != nil {
		t.Fatal(err)
	}
	if g.Category != "과일" {
		t.Fatalf("카테고리 = %s, want 과일", g.Category)
	}
	if !g.spValidWord(g.Location) {
		t.Fatalf("정답이 과일 목록에 없다: %q", g.Location)
	}
	if err := g.SetCategory("없는카테고리"); err == nil {
		t.Fatal("없는 카테고리가 통과했다")
	}

	g2 := NewSPGame("c2")
	for i := 0; i < 4; i++ {
		g2.AddPlayer(fmt.Sprintf("p%d", i))
	}
	if g2.CategoryChoice != SPCategoryRandom {
		t.Fatalf("기본 선택 = %s, want 랜덤", g2.CategoryChoice)
	}
	if err := g2.Start(rng, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, ok := spCategories[g2.Category]; !ok {
		t.Fatalf("랜덤 확정 카테고리 이상: %q", g2.Category)
	}
}

// TestSPStartAssignment 시작 배정 — 장소는 목록 중 1곳, 스파이는 좌석 범위 내,
// EndsAt 이 타이머만큼 미래. 인원 미달·시작 후 재시작은 에러.
func TestSPStartAssignment(t *testing.T) {
	for seed := int64(0); seed < 20; seed++ {
		g := NewSPGame("assign")
		n := SPMinPlayers + int(seed)%(SPMaxPlayers-SPMinPlayers+1)
		for i := 0; i < n; i++ {
			if _, err := g.AddPlayer(fmt.Sprintf("p%d", i)); err != nil {
				t.Fatalf("AddPlayer 실패: %v", err)
			}
		}
		if err := g.Start(rand.New(rand.NewSource(seed)), 5*time.Minute); err != nil {
			t.Fatalf("%d인 Start 실패: %v", n, err)
		}
		if g.Phase != SPPhasePlaying {
			t.Fatalf("phase = %s, want playing", g.Phase)
		}
		if g.SpySeat < 0 || g.SpySeat >= n {
			t.Fatalf("스파이 좌석 이상: %d (n=%d)", g.SpySeat, n)
		}
		if !g.spValidWord(g.Location) {
			t.Fatalf("정답이 확정 카테고리 목록에 없다: %q", g.Location)
		}
		wantEnds := time.Now().Add(5 * time.Minute).UnixMilli()
		if g.EndsAt < wantEnds-1000 || g.EndsAt > wantEnds+1000 {
			t.Fatalf("EndsAt = %d, want ≈%d", g.EndsAt, wantEnds)
		}
		if err := g.Start(rand.New(rand.NewSource(seed)), time.Minute); err == nil {
			t.Fatal("시작 후 재시작이 허용됐다")
		}
	}

	// 인원 미달은 시작 불가
	g := NewSPGame("short")
	for i := 0; i < SPMinPlayers-1; i++ {
		g.AddPlayer(fmt.Sprintf("p%d", i))
	}
	if g.CanStart() {
		t.Fatal("2인이 CanStart=true")
	}
	if err := g.Start(rand.New(rand.NewSource(1)), time.Minute); err == nil {
		t.Fatal("2인 시작이 허용됐다")
	}

	// 정원 초과 입장 불가
	g = NewSPGame("full")
	for i := 0; i < SPMaxPlayers; i++ {
		g.AddPlayer(fmt.Sprintf("p%d", i))
	}
	if _, err := g.AddPlayer("초과"); err == nil {
		t.Fatal("9번째 입장이 허용됐다")
	}
}

// TestSPSetTimer 타이머는 3·5·8분만, waiting 중에만
func TestSPSetTimer(t *testing.T) {
	g := NewSPGame("timer")
	if g.TimerMinutes != SPDefaultTimerMinutes {
		t.Fatalf("기본 타이머 = %d, want %d", g.TimerMinutes, SPDefaultTimerMinutes)
	}
	for _, m := range []int{3, 5, 8} {
		if err := g.SetTimer(m); err != nil {
			t.Fatalf("%d분 설정 실패: %v", m, err)
		}
		if g.TimerMinutes != m {
			t.Fatalf("타이머 = %d, want %d", g.TimerMinutes, m)
		}
	}
	for _, m := range []int{0, 1, 4, 10, -3} {
		if err := g.SetTimer(m); err == nil {
			t.Fatalf("%d분 설정이 허용됐다", m)
		}
	}

	g.Ready = true
	if err := g.SetTimer(3); err == nil {
		t.Fatal("시작 후 타이머 변경이 허용됐다")
	}
}

// TestSPRemovePlayerCompacts 대기 이탈 시 좌석이 앞으로 당겨진다
func TestSPRemovePlayerCompacts(t *testing.T) {
	g := NewSPGame("remove")
	for i := 0; i < 4; i++ {
		g.AddPlayer(fmt.Sprintf("p%d", i))
	}
	g.RemovePlayer(1)
	if len(g.Players) != 3 {
		t.Fatalf("인원 = %d, want 3", len(g.Players))
	}
	for i, p := range g.Players {
		if p.Seat != i {
			t.Fatalf("좌석 압축 실패: idx%d seat=%d", i, p.Seat)
		}
	}
	if g.Players[1].Name != "p2" {
		t.Fatalf("당겨진 이름 = %s, want p2", g.Players[1].Name)
	}
}

// TestSPGuessJudgement 스파이 추리 판정 — 적중은 스파이 승, 오답은 시민 승,
// 비스파이·목록 외 장소·playing 외 단계는 에러
func TestSPGuessJudgement(t *testing.T) {
	// 적중 → 스파이 승
	g := newSPStartedGame(4, 2, "병원")
	if err := g.Guess(2, "병원"); err != nil {
		t.Fatalf("적중 추리 실패: %v", err)
	}
	if g.Phase != SPPhaseGameOver {
		t.Fatalf("phase = %s, want game_over", g.Phase)
	}
	r := g.Result
	if r == nil || r.Winner != "spy" || r.Reason != "guess_right" ||
		r.SpySeat != 2 || r.Location != "병원" || r.GuessedLocation != "병원" || r.TopSeat != -1 {
		t.Fatalf("적중 결과 이상: %+v", r)
	}

	// 오답 → 시민 승
	g = newSPStartedGame(4, 0, "학교")
	if err := g.Guess(0, "은행"); err != nil {
		t.Fatalf("오답 추리 실패: %v", err)
	}
	r = g.Result
	if r == nil || r.Winner != "citizen" || r.Reason != "guess_wrong" ||
		r.Location != "학교" || r.GuessedLocation != "은행" {
		t.Fatalf("오답 결과 이상: %+v", r)
	}

	// 비스파이 추리는 에러
	g = newSPStartedGame(4, 1, "해변")
	if err := g.Guess(0, "해변"); err == nil {
		t.Fatal("비스파이의 추리가 허용됐다")
	}
	// 목록과 정확히 일치하지 않는 장소는 에러 (근사 매칭 없음)
	for _, bad := range []string{"우주 정거장", "hospital", "병원 ", ""} {
		if err := g.Guess(1, bad); err == nil {
			t.Fatalf("목록 외 장소 %q 가 허용됐다", bad)
		}
	}
	if g.Phase != SPPhasePlaying {
		t.Fatalf("에러 추리 후 phase = %s, want playing", g.Phase)
	}

	// voting 중 추리는 에러
	g.BeginVoting()
	if err := g.Guess(1, "해변"); err == nil {
		t.Fatal("voting 중 추리가 허용됐다")
	}
}

// TestSPVoteJudgement 투표 판정 — 단독 최다 && 스파이 → 시민 승.
// 그 외 전부(오지목·동표)는 스파이 승.
func TestSPVoteJudgement(t *testing.T) {
	setup := func(spySeat int) *SPGame {
		g := newSPStartedGame(4, spySeat, "카지노")
		g.BeginVoting()
		return g
	}
	mustVote := func(t *testing.T, g *SPGame, votes map[int]int) {
		t.Helper()
		for voter, target := range votes {
			if err := g.SubmitVote(voter, target); err != nil {
				t.Fatalf("seat%d → %d 실패: %v", voter, target, err)
			}
		}
		if !g.VoteComplete() {
			t.Fatal("전원 제출인데 VoteComplete=false")
		}
		g.ResolveVotes()
	}

	// 단독 최다가 스파이 → 시민 승 (vote_caught)
	g := setup(3)
	mustVote(t, g, map[int]int{0: 3, 1: 3, 2: 3, 3: 0})
	r := g.Result
	if r == nil || r.Winner != "citizen" || r.Reason != "vote_caught" || r.TopSeat != 3 || r.SpySeat != 3 {
		t.Fatalf("검거 결과 이상: %+v", r)
	}

	// 단독 최다가 비스파이 (오지목) → 스파이 승 (vote_missed)
	g = setup(3)
	mustVote(t, g, map[int]int{0: 1, 1: 0, 2: 1, 3: 1})
	r = g.Result
	if r == nil || r.Winner != "spy" || r.Reason != "vote_missed" || r.TopSeat != 1 {
		t.Fatalf("오지목 결과 이상: %+v", r)
	}

	// 동표 (2:2) → 스파이 승, topSeat -1
	g = setup(0)
	mustVote(t, g, map[int]int{0: 1, 1: 0, 2: 1, 3: 0})
	r = g.Result
	if r == nil || r.Winner != "spy" || r.Reason != "vote_missed" || r.TopSeat != -1 {
		t.Fatalf("동표 결과 이상: %+v", r)
	}

	// 스파이 자신도 최다표에 얹혀 동표를 만들면 스파이 승
	g = setup(2)
	mustVote(t, g, map[int]int{0: 2, 1: 2, 2: 3, 3: 2})
	r = g.Result
	if r == nil || r.Winner != "citizen" || r.TopSeat != 2 {
		t.Fatalf("3표 단독 최다 스파이 결과 이상: %+v", r)
	}
}

// TestSPVoteValidation 자기 지목·범위 밖·playing 중 투표 금지, 덮어쓰기 허용
func TestSPVoteValidation(t *testing.T) {
	g := newSPStartedGame(4, 1, "호텔")

	// playing 중 투표는 에러
	if err := g.SubmitVote(0, 1); err == nil {
		t.Fatal("playing 중 투표가 허용됐다")
	}

	g.BeginVoting()
	if err := g.SubmitVote(0, 0); err == nil {
		t.Fatal("자기 지목이 허용됐다")
	}
	if err := g.SubmitVote(0, -1); err == nil {
		t.Fatal("기권(-1)이 허용됐다 (스파이폴은 기권 없음)")
	}
	if err := g.SubmitVote(0, 4); err == nil {
		t.Fatal("범위 밖 지목이 허용됐다")
	}
	if err := g.SubmitVote(-1, 0); err == nil {
		t.Fatal("잘못된 좌석의 투표가 허용됐다")
	}

	// 스파이도 투표한다 + 집계 전 덮어쓰기 허용
	if err := g.SubmitVote(1, 0); err != nil {
		t.Fatalf("스파이 투표 실패: %v", err)
	}
	if err := g.SubmitVote(1, 2); err != nil {
		t.Fatalf("투표 덮어쓰기 실패: %v", err)
	}
	if g.Votes[1] != 2 {
		t.Fatalf("덮어쓴 표 = %d, want 2", g.Votes[1])
	}
	if g.VoteComplete() {
		t.Fatal("1명만 투표인데 VoteComplete=true")
	}
	for _, voter := range []int{0, 2, 3} {
		g.SubmitVote(voter, 1)
	}
	if !g.VoteComplete() {
		t.Fatal("전원 투표인데 VoteComplete=false")
	}
	views := g.VoteViews()
	if len(views) != 4 || views[0].Voter != 0 || views[1].Voter != 1 {
		t.Fatalf("투표 현황 이상: %+v", views)
	}
}
