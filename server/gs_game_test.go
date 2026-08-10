package server

import (
	"math/rand"
	"testing"
)

// newTestGSGame 2인 입장·시작·배치까지 끝낸 play 단계 게임.
// 배치는 결정적: 양측 모두 뒷줄(남 5행, 북 0행) 4칸이 좋은 유령.
func newTestGSGame(t *testing.T) *GSGame {
	t.Helper()
	g := NewGSGame("test")
	g.AddPlayer("A")
	g.AddPlayer("B")
	if err := g.Start(rand.New(rand.NewSource(1))); err != nil {
		t.Fatalf("Start: %v", err)
	}
	south := []GSCell{{5, 1}, {5, 2}, {5, 3}, {5, 4}}
	north := []GSCell{{0, 1}, {0, 2}, {0, 3}, {0, 4}}
	if err := g.SubmitSetup(GSSouth, south); err != nil {
		t.Fatalf("SubmitSetup south: %v", err)
	}
	if err := g.SubmitSetup(GSNorth, north); err != nil {
		t.Fatalf("SubmitSetup north: %v", err)
	}
	if g.Phase != GSPhasePlay {
		t.Fatalf("phase = %s, want play", g.Phase)
	}
	return g
}

// mustMove 에러 나면 즉시 실패
func mustMove(t *testing.T, g *GSGame, side GSSide, from, to GSCell) *GSMoveResult {
	t.Helper()
	result, err := g.Move(side, from, to, false)
	if err != nil {
		t.Fatalf("Move %v→%v (%s): %v", from, to, side, err)
	}
	return result
}

func TestGSSetupBoard(t *testing.T) {
	g := NewGSGame("test")
	g.AddPlayer("A")
	g.AddPlayer("B")
	g.Start(rand.New(rand.NewSource(1)))

	if g.Phase != GSPhaseSetup {
		t.Fatalf("phase = %s, want setup", g.Phase)
	}
	if len(g.Ghosts) != 16 {
		t.Fatalf("유령 %d개, want 16", len(g.Ghosts))
	}
	// 남쪽은 행 4~5 열 1~4, 북쪽은 행 0~1 열 1~4
	for _, ghost := range g.Ghosts {
		if ghost.Side == GSSouth && (ghost.Row < 4 || ghost.Col < 1 || ghost.Col > 4) {
			t.Fatalf("남쪽 배치 위반: %+v", ghost)
		}
		if ghost.Side == GSNorth && (ghost.Row > 1 || ghost.Col < 1 || ghost.Col > 4) {
			t.Fatalf("북쪽 배치 위반: %+v", ghost)
		}
	}
}

func TestGSSubmitSetupValidation(t *testing.T) {
	g := NewGSGame("test")
	g.AddPlayer("A")
	g.AddPlayer("B")
	g.Start(rand.New(rand.NewSource(1)))

	// 3개만 제출
	if err := g.SubmitSetup(GSSouth, []GSCell{{5, 1}, {5, 2}, {5, 3}}); err == nil {
		t.Fatal("4개 미만 제출이 허용됐다")
	}
	// 중복 칸
	if err := g.SubmitSetup(GSSouth, []GSCell{{5, 1}, {5, 1}, {5, 2}, {5, 3}}); err == nil {
		t.Fatal("중복 칸이 허용됐다")
	}
	// 남의 구역
	if err := g.SubmitSetup(GSSouth, []GSCell{{0, 1}, {5, 1}, {5, 2}, {5, 3}}); err == nil {
		t.Fatal("상대 구역 칸이 허용됐다")
	}
	// 정상 제출 후 재제출 거부
	if err := g.SubmitSetup(GSSouth, []GSCell{{5, 1}, {5, 2}, {5, 3}, {5, 4}}); err != nil {
		t.Fatalf("정상 제출 실패: %v", err)
	}
	if err := g.SubmitSetup(GSSouth, []GSCell{{4, 1}, {4, 2}, {4, 3}, {4, 4}}); err == nil {
		t.Fatal("재제출이 허용됐다")
	}
	// 한쪽만 제출이면 setup 유지
	if g.Phase != GSPhaseSetup {
		t.Fatalf("phase = %s, want setup", g.Phase)
	}
	// 좋은 유령이 정확히 4개
	good := 0
	for _, ghost := range g.Ghosts {
		if ghost.Side == GSSouth && ghost.Good {
			good++
		}
	}
	if good != 4 {
		t.Fatalf("남쪽 좋은 유령 %d개, want 4", good)
	}
}

func TestGSMoveRules(t *testing.T) {
	g := newTestGSGame(t)
	g.CurrentSide = GSSouth

	// 대각선 거부
	if _, err := g.Move(GSSouth, GSCell{4, 1}, GSCell{3, 2}, false); err == nil {
		t.Fatal("대각선 이동이 허용됐다")
	}
	// 두 칸 거부
	if _, err := g.Move(GSSouth, GSCell{4, 1}, GSCell{2, 1}, false); err == nil {
		t.Fatal("두 칸 이동이 허용됐다")
	}
	// 내 유령 위 거부
	if _, err := g.Move(GSSouth, GSCell{5, 1}, GSCell{4, 1}, false); err == nil {
		t.Fatal("내 유령 칸으로 이동이 허용됐다")
	}
	// 남의 턴 거부
	if _, err := g.Move(GSNorth, GSCell{1, 1}, GSCell{2, 1}, false); err == nil {
		t.Fatal("남의 턴 이동이 허용됐다")
	}
	// 빈 칸에서 이동 거부
	if _, err := g.Move(GSSouth, GSCell{3, 3}, GSCell{2, 3}, false); err == nil {
		t.Fatal("유령 없는 칸 이동이 허용됐다")
	}

	// 정상 이동 후 턴 교대
	mustMove(t, g, GSSouth, GSCell{4, 1}, GSCell{3, 1})
	if g.CurrentSide != GSNorth {
		t.Fatalf("턴이 넘어가지 않았다: %s", g.CurrentSide)
	}
}

func TestGSCaptureRevealsColor(t *testing.T) {
	g := newTestGSGame(t)
	g.CurrentSide = GSSouth

	// 남쪽 나쁜 유령(4,1)을 북쪽 진영까지 걸어가 북쪽 나쁜 유령(1,1)을 잡는다
	mustMove(t, g, GSSouth, GSCell{4, 1}, GSCell{3, 1})
	mustMove(t, g, GSNorth, GSCell{1, 4}, GSCell{2, 4})
	mustMove(t, g, GSSouth, GSCell{3, 1}, GSCell{2, 1})
	mustMove(t, g, GSNorth, GSCell{2, 4}, GSCell{3, 4})
	result := mustMove(t, g, GSSouth, GSCell{2, 1}, GSCell{1, 1})

	if !result.Captured {
		t.Fatal("잡기가 판정되지 않았다")
	}
	if result.CapturedGood {
		t.Fatal("북쪽 (1,1)은 나쁜 유령이어야 한다")
	}
	if g.capturedCount(GSNorth, false) != 1 {
		t.Fatal("잡힌 수 집계가 틀렸다")
	}
	// 잡힌 유령은 보드에서 사라진다
	if g.ghostAt(1, 1) == nil || g.ghostAt(1, 1).Side != GSSouth {
		t.Fatal("잡은 유령이 그 칸을 차지해야 한다")
	}
}

// southMarch 남쪽 유령을 경로대로 옮기고, 사이사이 북쪽은 (1,4)↔(2,4) 왕복으로
// 턴을 소모한다. 북쪽 위치는 보드에서 직접 찾아 호출 간에도 이어진다.
func southMarch(t *testing.T, g *GSGame, path []GSCell) {
	t.Helper()
	for i := 0; i+1 < len(path); i++ {
		mustMove(t, g, GSSouth, path[i], path[i+1])
		if ghost := g.ghostAt(1, 4); ghost != nil && ghost.Side == GSNorth {
			mustMove(t, g, GSNorth, GSCell{1, 4}, GSCell{2, 4})
		} else {
			mustMove(t, g, GSNorth, GSCell{2, 4}, GSCell{1, 4})
		}
	}
}

func TestGSEscapeRules(t *testing.T) {
	g := newTestGSGame(t)
	g.CurrentSide = GSSouth

	// 길을 막는 나쁜 유령(4,1)을 옆으로 치우고,
	// 좋은 유령(5,1)을 왼쪽 열로 북쪽 모서리(0,0)까지 행진시킨다
	southMarch(t, g, []GSCell{{4, 1}, {3, 1}, {2, 1}})
	southMarch(t, g, []GSCell{{5, 1}, {4, 1}, {4, 0}, {3, 0}})

	// 탈출구 아닌 곳에서 탈출 시도 → 거부 (에러라 턴 소모 없음)
	if _, err := g.Move(GSSouth, GSCell{3, 0}, GSCell{}, true); err == nil {
		t.Fatal("탈출구 아닌 곳에서 탈출이 허용됐다")
	}

	southMarch(t, g, []GSCell{{3, 0}, {2, 0}, {1, 0}, {0, 0}})

	// 좋은 유령이 탈출구(0,0) 도착 → 탈출!
	result, err := g.Move(GSSouth, GSCell{0, 0}, GSCell{}, true)
	if err != nil {
		t.Fatalf("탈출 실패: %v", err)
	}
	if !result.Escaped || !result.GameOver {
		t.Fatal("탈출이 승리로 이어지지 않았다")
	}
	if g.Winner != GSSouth || g.EndReason != "escape" {
		t.Fatalf("winner=%s reason=%s", g.Winner, g.EndReason)
	}
}

func TestGSEvilGhostCannotEscape(t *testing.T) {
	g := newTestGSGame(t)
	g.CurrentSide = GSSouth

	// 남쪽 나쁜 유령(4,1)을 (0,0)까지 보낸 뒤 탈출 시도
	southMarch(t, g, []GSCell{{4, 1}, {3, 1}, {3, 0}, {2, 0}, {1, 0}, {0, 0}})
	if _, err := g.Move(GSSouth, GSCell{0, 0}, GSCell{}, true); err == nil {
		t.Fatal("나쁜 유령의 탈출이 허용됐다")
	}
}

func TestGSCapturedAllGoodWins(t *testing.T) {
	g := newTestGSGame(t)
	// 북쪽 좋은 유령 4개(0행)를 강제로 3개 잡힌 상태로 만들고 마지막 하나를 잡는다
	captured := 0
	var last *GSGhost
	for _, ghost := range g.Ghosts {
		if ghost.Side == GSNorth && ghost.Good {
			if captured < 3 {
				ghost.Captured = true
				captured++
			} else {
				last = ghost
			}
		}
	}
	// 남쪽 유령을 마지막 좋은 유령 옆으로 순간이동시켜 잡는다
	attacker := g.ghostAt(4, 1)
	attacker.Row, attacker.Col = last.Row+1, last.Col
	g.CurrentSide = GSSouth

	result := mustMove(t, g, GSSouth, GSCell{last.Row + 1, last.Col}, GSCell{last.Row, last.Col})
	if !result.GameOver || g.Winner != GSSouth || g.EndReason != "captured_all_good" {
		t.Fatalf("gameOver=%v winner=%s reason=%s", result.GameOver, g.Winner, g.EndReason)
	}
}

func TestGSFedAllEvilWins(t *testing.T) {
	g := newTestGSGame(t)
	// 북쪽 나쁜 유령(1행) 3개를 잡힌 상태로, 마지막 하나를 남쪽이 잡으면 북쪽 승리
	captured := 0
	var last *GSGhost
	for _, ghost := range g.Ghosts {
		if ghost.Side == GSNorth && !ghost.Good {
			if captured < 3 {
				ghost.Captured = true
				captured++
			} else {
				last = ghost
			}
		}
	}
	attacker := g.ghostAt(4, 1)
	attacker.Row, attacker.Col = last.Row+1, last.Col
	g.CurrentSide = GSSouth

	result := mustMove(t, g, GSSouth, GSCell{last.Row + 1, last.Col}, GSCell{last.Row, last.Col})
	if !result.GameOver || g.Winner != GSNorth || g.EndReason != "fed_all_evil" {
		t.Fatalf("gameOver=%v winner=%s reason=%s (미끼를 다 먹였으면 북쪽 승리)", result.GameOver, g.Winner, g.EndReason)
	}
}

func TestGSMoveBeforeSetupRejected(t *testing.T) {
	g := NewGSGame("test")
	g.AddPlayer("A")
	g.AddPlayer("B")
	g.Start(rand.New(rand.NewSource(1)))
	if _, err := g.Move(g.CurrentSide, GSCell{4, 1}, GSCell{3, 1}, false); err == nil {
		t.Fatal("배치 전 이동이 허용됐다")
	}
}
