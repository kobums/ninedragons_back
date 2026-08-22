package server

import (
	"math/rand"
	"testing"
)

// ==================== 인사이더 순수 규칙 테스트 ====================
//
// 허브·소켓 없이 IDGame 만 직접 돌린다 (결정적 rng).

func idNewGame(t *testing.T, n int) (*IDGame, *rand.Rand) {
	t.Helper()
	g := NewIDGame("test")
	for i := 0; i < n; i++ {
		if _, err := g.AddPlayer(string(rune('A' + i))); err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
	}
	rng := rand.New(rand.NewSource(42))
	if err := g.Start(rng); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return g, rng
}

// TestIDStartAssignsRoles 시작 배정 — 마스터 1·인사이더 1·나머지 시민,
// 마스터와 인사이더는 서로 다른 좌석이고 제시어가 뽑힌다
func TestIDStartAssignsRoles(t *testing.T) {
	g, _ := idNewGame(t, 5)

	if g.Phase != IDPhaseQuestion {
		t.Fatalf("phase = %s, want question", g.Phase)
	}
	if g.MasterSeat < 0 || g.InsiderSeat < 0 || g.MasterSeat == g.InsiderSeat {
		t.Fatalf("master=%d insider=%d", g.MasterSeat, g.InsiderSeat)
	}
	if g.Word == "" {
		t.Fatal("제시어가 비었다")
	}
	counts := map[IDRole]int{}
	for _, p := range g.Players {
		counts[p.Role]++
		if p.Vote != -1 {
			t.Fatalf("seat%d 초기 투표 = %d, want -1", p.Seat, p.Vote)
		}
	}
	if counts[IDRoleMaster] != 1 || counts[IDRoleInsider] != 1 || counts[IDRoleCitizen] != 3 {
		t.Fatalf("역할 분포 이상: %v", counts)
	}
	if g.Players[g.MasterSeat].Role != IDRoleMaster ||
		g.Players[g.InsiderSeat].Role != IDRoleInsider {
		t.Fatal("좌석과 역할이 어긋난다")
	}
}

// TestIDMinPlayersGuard 4인 미만은 시작할 수 없다
func TestIDMinPlayersGuard(t *testing.T) {
	g := NewIDGame("t")
	for i := 0; i < IDMinPlayers-1; i++ {
		g.AddPlayer("P")
	}
	if g.CanStart() {
		t.Fatal("3인인데 시작 가능으로 판정됐다")
	}
	if err := g.Start(rand.New(rand.NewSource(1))); err == nil {
		t.Fatal("3인 시작이 에러 없이 통과했다")
	}
	g.AddPlayer("P")
	if !g.CanStart() {
		t.Fatal("4인인데 시작 불가로 판정됐다")
	}
	// 정원 초과는 입장 거부
	for i := 0; i < IDMaxPlayers; i++ {
		g.AddPlayer("P")
	}
	if len(g.Players) != IDMaxPlayers {
		t.Fatalf("인원 = %d, want %d", len(g.Players), IDMaxPlayers)
	}
}

// TestIDPhaseFlow 단계 전환 — 마스터만 [정답 나옴]·[투표 시작]을 쓸 수 있고
// 그 순서를 벗어난 호출은 거부된다
func TestIDPhaseFlow(t *testing.T) {
	g, _ := idNewGame(t, 5)
	other := (g.MasterSeat + 1) % len(g.Players)

	if err := g.OpenVote(g.MasterSeat); err == nil {
		t.Fatal("질문 단계에서 투표 시작이 통과했다")
	}
	if err := g.MarkCorrect(other); err == nil {
		t.Fatal("마스터가 아닌 좌석의 정답 선언이 통과했다")
	}
	if err := g.MarkCorrect(g.MasterSeat); err != nil {
		t.Fatalf("MarkCorrect: %v", err)
	}
	if g.Phase != IDPhaseDiscussion {
		t.Fatalf("phase = %s, want discussion", g.Phase)
	}
	if err := g.MarkCorrect(g.MasterSeat); err == nil {
		t.Fatal("중복 정답 선언이 통과했다")
	}
	if err := g.OpenVote(other); err == nil {
		t.Fatal("마스터가 아닌 좌석의 투표 시작이 통과했다")
	}
	if err := g.OpenVote(g.MasterSeat); err != nil {
		t.Fatalf("OpenVote: %v", err)
	}
	if g.Phase != IDPhaseVoting {
		t.Fatalf("phase = %s, want voting", g.Phase)
	}
}

// TestIDVoteGuards 투표 유효성 — 자기 자신·중복·단계 밖 투표는 거부된다
func TestIDVoteGuards(t *testing.T) {
	g, _ := idNewGame(t, 5)
	if err := g.SubmitVote(0, 1); err == nil {
		t.Fatal("질문 단계 투표가 통과했다")
	}
	g.MarkCorrect(g.MasterSeat)
	g.OpenVote(g.MasterSeat)

	if err := g.SubmitVote(0, 0); err == nil {
		t.Fatal("자기 자신 투표가 통과했다")
	}
	if err := g.SubmitVote(0, 99); err == nil {
		t.Fatal("범위 밖 지목이 통과했다")
	}
	if err := g.SubmitVote(0, 1); err != nil {
		t.Fatalf("SubmitVote: %v", err)
	}
	if err := g.SubmitVote(0, 2); err == nil {
		t.Fatal("중복 투표가 통과했다")
	}
	if !g.Players[0].Voted() {
		t.Fatal("투표 표기가 남지 않았다")
	}
}

// TestIDInsiderCaught 인사이더가 최다 득표 — 시민+마스터 승리
func TestIDInsiderCaught(t *testing.T) {
	g, _ := idNewGame(t, 5)
	g.MarkCorrect(g.MasterSeat)
	g.OpenVote(g.MasterSeat)

	// 인사이더를 뺀 전원이 인사이더를 지목, 인사이더는 아무나 지목
	for _, p := range g.Players {
		target := g.InsiderSeat
		if p.Seat == g.InsiderSeat {
			target = (g.InsiderSeat + 1) % len(g.Players)
		}
		if err := g.SubmitVote(p.Seat, target); err != nil {
			t.Fatalf("seat%d 투표: %v", p.Seat, err)
		}
	}

	if g.Phase != IDPhaseGameOver {
		t.Fatalf("전원 투표 후 phase = %s, want game_over", g.Phase)
	}
	if g.Result == nil || g.Result.Winner != "citizens" {
		t.Fatalf("result = %+v, want citizens", g.Result)
	}
	if g.Result.TopSeat != g.InsiderSeat || g.Result.InsiderSeat != g.InsiderSeat {
		t.Fatalf("개표 결과 좌석 이상: %+v (insider=%d)", g.Result, g.InsiderSeat)
	}
	if g.EndReason != "insider_caught" {
		t.Fatalf("reason = %q", g.EndReason)
	}
	if g.Players[g.InsiderSeat].Votes != len(g.Players)-1 {
		t.Fatalf("인사이더 득표 = %d", g.Players[g.InsiderSeat].Votes)
	}
}

// TestIDInsiderEscaped 엉뚱한 사람이 최다 득표 — 인사이더 승리
func TestIDInsiderEscaped(t *testing.T) {
	g, _ := idNewGame(t, 5)
	g.MarkCorrect(g.MasterSeat)
	g.OpenVote(g.MasterSeat)

	// 인사이더도 마스터도 아닌 희생양 하나를 전원이 지목
	scapegoat := -1
	for _, p := range g.Players {
		if p.Seat != g.InsiderSeat && p.Seat != g.MasterSeat {
			scapegoat = p.Seat
			break
		}
	}
	for _, p := range g.Players {
		target := scapegoat
		if p.Seat == scapegoat {
			target = g.MasterSeat
		}
		g.SubmitVote(p.Seat, target)
	}

	if g.Result == nil || g.Result.Winner != "insider" || g.Result.TopSeat != scapegoat {
		t.Fatalf("result = %+v, want insider/top=%d", g.Result, scapegoat)
	}
	if g.EndReason != "insider_escaped" {
		t.Fatalf("reason = %q", g.EndReason)
	}
}

// idFixedGame 역할을 결정적으로 고정한 4인 게임 (마스터 0, 인사이더 insider)
func idFixedGame(t *testing.T, insider int) *IDGame {
	t.Helper()
	g := NewIDGame("fixed")
	for i := 0; i < 4; i++ {
		g.AddPlayer(string(rune('A' + i)))
	}
	if err := g.Start(rand.New(rand.NewSource(7))); err != nil {
		t.Fatalf("Start: %v", err)
	}
	g.MasterSeat, g.InsiderSeat = 0, insider
	for _, p := range g.Players {
		switch p.Seat {
		case 0:
			p.Role = IDRoleMaster
		case insider:
			p.Role = IDRoleInsider
		default:
			p.Role = IDRoleCitizen
		}
	}
	g.MarkCorrect(0)
	g.OpenVote(0)
	return g
}

// TestIDTieBreakByMaster 동표는 마스터의 지목이 결정한다 (2번 2표 vs 1번 2표
// 동표에서 마스터가 찍은 2번이 최다 득표자가 된다)
func TestIDTieBreakByMaster(t *testing.T) {
	g := idFixedGame(t, 2)

	g.SubmitVote(0, 2) // 마스터 → 2
	g.SubmitVote(1, 2) // 2번 2표
	g.SubmitVote(2, 1)
	g.SubmitVote(3, 1) // 1번 2표 — 동표

	if g.Result == nil {
		t.Fatal("전원 투표 후 개표되지 않았다")
	}
	if g.Result.TopSeat != 2 {
		t.Fatalf("동표 결정 실패: top = %d, want 2 (마스터 지목)", g.Result.TopSeat)
	}
	if g.Result.Winner != "citizens" {
		t.Fatalf("winner = %q, want citizens (2번이 인사이더)", g.Result.Winner)
	}

	// 마스터가 동표 후보 밖을 찍으면 후보 중 낮은 좌석이 남는다 (결정성 보장)
	g2 := idFixedGame(t, 3)
	g2.SubmitVote(0, 3)
	g2.SubmitVote(1, 2)
	g2.SubmitVote(2, 1)
	g2.SubmitVote(3, 1) // 1번 2표, 2번 1표, 3번 1표
	if g2.Result == nil || g2.Result.TopSeat != 1 {
		t.Fatalf("단독 최다 득표 판정 실패: %+v", g2.Result)
	}
	if g2.Result.Winner != "insider" {
		t.Fatalf("winner = %q, want insider", g2.Result.Winner)
	}
}

// TestIDQuestionTimeoutAllLose 질문 타임 초과 — 인사이더 포함 전원 패배
func TestIDQuestionTimeoutAllLose(t *testing.T) {
	g, _ := idNewGame(t, 5)
	g.ForceQuestionTimeout()

	if g.Phase != IDPhaseGameOver {
		t.Fatalf("phase = %s, want game_over", g.Phase)
	}
	if g.Result == nil || g.Result.Winner != "none" || g.Result.TopSeat != -1 {
		t.Fatalf("result = %+v, want none/top=-1", g.Result)
	}
	if g.EndReason != "timeout" {
		t.Fatalf("reason = %q", g.EndReason)
	}
	if g.Result.InsiderSeat != g.InsiderSeat {
		t.Fatal("종료 결과에 인사이더 좌석이 없다")
	}
}

// TestIDForceVoteDeadline 투표 마감 — 미제출 좌석은 자기 제외 무작위로 채워
// 반드시 개표까지 간다 (교착 방지)
func TestIDForceVoteDeadline(t *testing.T) {
	g, rng := idNewGame(t, 6)
	g.MarkCorrect(g.MasterSeat)
	g.OpenVote(g.MasterSeat)
	g.SubmitVote(g.MasterSeat, g.InsiderSeat) // 한 명만 제출

	g.ForceVoteDeadline(rng)

	if g.Phase != IDPhaseGameOver || g.Result == nil {
		t.Fatalf("마감 후 phase = %s result = %+v", g.Phase, g.Result)
	}
	total := 0
	for _, p := range g.Players {
		if p.Vote < 0 || p.Vote == p.Seat {
			t.Fatalf("seat%d 투표 = %d (자기 자신·미투표 금지)", p.Seat, p.Vote)
		}
		total += p.Votes
	}
	if total != len(g.Players) {
		t.Fatalf("총 득표 = %d, want %d", total, len(g.Players))
	}
	if g.Result.Winner != "citizens" && g.Result.Winner != "insider" {
		t.Fatalf("winner = %q", g.Result.Winner)
	}
}

// TestIDPickWord 제시어 풀 — 코드네임 공용 풀에서 뽑히고 비지 않는다
func TestIDPickWord(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	pool := map[string]bool{}
	for _, w := range cnWordPool() {
		pool[w] = true
	}
	for i := 0; i < 50; i++ {
		w := idPickWord(rng)
		if w == "" || !pool[w] {
			t.Fatalf("제시어 %q 가 공용 단어 풀 밖이다", w)
		}
	}
}
