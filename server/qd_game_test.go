package server

import (
	"math/rand"
	"testing"
)

// newTestQDGame 남 선공으로 고정한 시작 상태
func newTestQDGame(t *testing.T) *QDGame {
	t.Helper()
	g := NewQDGame("test")
	if _, err := g.AddPlayer("남이"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.AddPlayer("북이"); err != nil {
		t.Fatal(err)
	}
	if err := g.Start(rand.New(rand.NewSource(1))); err != nil {
		t.Fatal(err)
	}
	g.CurrentSide = QDSouth
	return g
}

func qdMovesContain(moves []QDCell, cell QDCell) bool {
	for _, m := range moves {
		if m == cell {
			return true
		}
	}
	return false
}

func TestQDStartState(t *testing.T) {
	g := newTestQDGame(t)

	if g.Phase != QDPhasePlay {
		t.Fatalf("시작 후 phase = %s, want play", g.Phase)
	}
	if g.Pawns[QDSouth] != (QDCell{Row: 8, Col: 4}) {
		t.Errorf("남 폰 시작 위치 = %v", g.Pawns[QDSouth])
	}
	if g.Pawns[QDNorth] != (QDCell{Row: 0, Col: 4}) {
		t.Errorf("북 폰 시작 위치 = %v", g.Pawns[QDNorth])
	}
	if g.WallsLeft[QDSouth] != QDWallCount || g.WallsLeft[QDNorth] != QDWallCount {
		t.Errorf("벽 개수 = %d/%d, want 10/10", g.WallsLeft[QDSouth], g.WallsLeft[QDNorth])
	}
}

func TestQDPawnMoveRules(t *testing.T) {
	g := newTestQDGame(t)

	// 시작 위치 (8,4): 위·좌·우 3칸만 가능 (아래는 보드 밖)
	moves := g.LegalPawnMoves(QDSouth)
	if len(moves) != 3 {
		t.Fatalf("시작 합법 이동 %d개 = %v, want 3", len(moves), moves)
	}
	for _, want := range []QDCell{{7, 4}, {8, 3}, {8, 5}} {
		if !qdMovesContain(moves, want) {
			t.Errorf("합법 이동에 %v 이 없음", want)
		}
	}

	// 대각선·원거리 거부
	if _, err := g.MovePawn(QDSouth, QDCell{Row: 7, Col: 3}); err == nil {
		t.Error("대각선 이동이 허용됨")
	}
	if _, err := g.MovePawn(QDSouth, QDCell{Row: 6, Col: 4}); err == nil {
		t.Error("2칸 이동이 허용됨")
	}

	// 정상 이동 후 턴 교대
	if _, err := g.MovePawn(QDSouth, QDCell{Row: 7, Col: 4}); err != nil {
		t.Fatal(err)
	}
	if g.CurrentSide != QDNorth {
		t.Error("이동 후 턴이 북으로 넘어가지 않음")
	}

	// 차례가 아닌 진영의 이동 거부
	if _, err := g.MovePawn(QDSouth, QDCell{Row: 6, Col: 4}); err == nil {
		t.Error("차례가 아닌데 이동이 허용됨")
	}
}

func TestQDWallBlocksMovement(t *testing.T) {
	g := newTestQDGame(t)

	// 가로 벽 (7,4): (8,4)→(7,4) 와 (8,5)→(7,5) 를 막는다
	g.Walls = []QDWall{{Row: 7, Col: 4, Orientation: "h"}}
	moves := g.LegalPawnMoves(QDSouth)
	if qdMovesContain(moves, QDCell{Row: 7, Col: 4}) {
		t.Error("가로 벽 너머로 이동이 허용됨")
	}

	// 가로 벽 (7,3) 도 (8,4)→(7,4) 를 막는다 (벽은 두 칸에 걸침)
	g.Walls = []QDWall{{Row: 7, Col: 3, Orientation: "h"}}
	if qdMovesContain(g.LegalPawnMoves(QDSouth), QDCell{Row: 7, Col: 4}) {
		t.Error("두 칸 걸친 가로 벽 너머로 이동이 허용됨")
	}

	// 세로 벽 (7,4): (8,4)→(8,5) 를 막는다
	g.Walls = []QDWall{{Row: 7, Col: 4, Orientation: "v"}}
	if qdMovesContain(g.LegalPawnMoves(QDSouth), QDCell{Row: 8, Col: 5}) {
		t.Error("세로 벽 너머로 이동이 허용됨")
	}
}

func TestQDJumpStraight(t *testing.T) {
	g := newTestQDGame(t)
	g.Pawns[QDSouth] = QDCell{Row: 4, Col: 4}
	g.Pawns[QDNorth] = QDCell{Row: 3, Col: 4}

	moves := g.LegalPawnMoves(QDSouth)
	if !qdMovesContain(moves, QDCell{Row: 2, Col: 4}) {
		t.Error("마주보기 직선 점프가 없음")
	}
	if qdMovesContain(moves, QDCell{Row: 3, Col: 4}) {
		t.Error("상대 폰 칸으로 이동이 허용됨")
	}
}

func TestQDJumpDiagonal(t *testing.T) {
	g := newTestQDGame(t)
	g.Pawns[QDSouth] = QDCell{Row: 4, Col: 4}
	g.Pawns[QDNorth] = QDCell{Row: 3, Col: 4}

	// 상대 뒤가 벽: 직선 점프 대신 대각 우회
	g.Walls = []QDWall{{Row: 2, Col: 4, Orientation: "h"}}
	moves := g.LegalPawnMoves(QDSouth)
	if qdMovesContain(moves, QDCell{Row: 2, Col: 4}) {
		t.Error("벽 너머 직선 점프가 허용됨")
	}
	if !qdMovesContain(moves, QDCell{Row: 3, Col: 3}) || !qdMovesContain(moves, QDCell{Row: 3, Col: 5}) {
		t.Errorf("대각 우회가 없음: %v", moves)
	}

	// 대각 한쪽도 벽이면 반대쪽만
	g.Walls = append(g.Walls, QDWall{Row: 2, Col: 4, Orientation: "v"}) // (3,4)→(3,5) 차단
	moves = g.LegalPawnMoves(QDSouth)
	if qdMovesContain(moves, QDCell{Row: 3, Col: 5}) {
		t.Error("벽으로 막힌 대각 우회가 허용됨")
	}
	if !qdMovesContain(moves, QDCell{Row: 3, Col: 3}) {
		t.Error("열린 대각 우회가 없음")
	}
}

func TestQDJumpDiagonalAtEdge(t *testing.T) {
	g := newTestQDGame(t)
	// 북 폰이 자기 뒷줄(행 0)에 있어 직선 점프가 보드 밖
	g.Pawns[QDSouth] = QDCell{Row: 1, Col: 4}
	g.Pawns[QDNorth] = QDCell{Row: 0, Col: 4}

	moves := g.LegalPawnMoves(QDSouth)
	if !qdMovesContain(moves, QDCell{Row: 0, Col: 3}) || !qdMovesContain(moves, QDCell{Row: 0, Col: 5}) {
		t.Errorf("보드 끝 대각 우회가 없음: %v", moves)
	}
}

func TestQDWallPlacementValidation(t *testing.T) {
	g := newTestQDGame(t)

	// 정상 설치: 벽 수 감소 + 턴 교대
	if err := g.PlaceWall(QDSouth, QDWall{Row: 4, Col: 4, Orientation: "h"}); err != nil {
		t.Fatal(err)
	}
	if g.WallsLeft[QDSouth] != QDWallCount-1 {
		t.Errorf("남 벽 수 = %d, want %d", g.WallsLeft[QDSouth], QDWallCount-1)
	}
	if g.CurrentSide != QDNorth {
		t.Error("벽 설치 후 턴이 넘어가지 않음")
	}

	// 같은 자리 겹침
	if err := g.PlaceWall(QDNorth, QDWall{Row: 4, Col: 4, Orientation: "h"}); err == nil {
		t.Error("같은 자리 겹침이 허용됨")
	}
	// 한 칸 밀린 겹침 (걸치는 두 칸이 겹침)
	if err := g.PlaceWall(QDNorth, QDWall{Row: 4, Col: 5, Orientation: "h"}); err == nil {
		t.Error("한 칸 밀린 겹침이 허용됨")
	}
	// 같은 교차점 교차
	if err := g.PlaceWall(QDNorth, QDWall{Row: 4, Col: 4, Orientation: "v"}); err == nil {
		t.Error("가로·세로 교차가 허용됨")
	}
	// 범위 밖 앵커
	if err := g.PlaceWall(QDNorth, QDWall{Row: 8, Col: 0, Orientation: "h"}); err == nil {
		t.Error("범위 밖 벽이 허용됨")
	}
	if err := g.PlaceWall(QDNorth, QDWall{Row: -1, Col: 0, Orientation: "v"}); err == nil {
		t.Error("음수 앵커 벽이 허용됨")
	}
	// 잘못된 방향
	if err := g.PlaceWall(QDNorth, QDWall{Row: 2, Col: 2, Orientation: "x"}); err == nil {
		t.Error("잘못된 방향이 허용됨")
	}
	// 남은 벽 없음
	g.WallsLeft[QDNorth] = 0
	if err := g.PlaceWall(QDNorth, QDWall{Row: 2, Col: 2, Orientation: "h"}); err == nil {
		t.Error("벽이 없는데 설치가 허용됨")
	}
}

func TestQDWallCannotBlockPath(t *testing.T) {
	g := newTestQDGame(t)

	// 행 4/5 경계를 계단형으로 거의 봉쇄한 상태 (열 8 통로만 남음)
	g.Walls = []QDWall{
		{Row: 4, Col: 0, Orientation: "h"},
		{Row: 4, Col: 2, Orientation: "h"},
		{Row: 4, Col: 4, Orientation: "h"},
		{Row: 4, Col: 6, Orientation: "h"},
		{Row: 3, Col: 7, Orientation: "h"},
	}

	// 마지막 통로를 막는 벽은 거부
	if err := g.PlaceWall(QDSouth, QDWall{Row: 4, Col: 7, Orientation: "v"}); err == nil {
		t.Fatal("경로를 완전히 막는 벽이 허용됨")
	}
	// 거부된 시도는 상태를 바꾸지 않는다
	if len(g.Walls) != 5 || g.WallsLeft[QDSouth] != QDWallCount || g.CurrentSide != QDSouth {
		t.Error("거부된 벽 설치가 상태를 바꿈")
	}

	// 통로를 막지 않는 벽은 허용
	if err := g.PlaceWall(QDSouth, QDWall{Row: 6, Col: 0, Orientation: "v"}); err != nil {
		t.Fatalf("무해한 벽이 거부됨: %v", err)
	}
}

func TestQDReachGoalWins(t *testing.T) {
	g := newTestQDGame(t)
	g.Pawns[QDSouth] = QDCell{Row: 1, Col: 6}

	result, err := g.MovePawn(QDSouth, QDCell{Row: 0, Col: 6})
	if err != nil {
		t.Fatal(err)
	}
	if !result.GameOver {
		t.Error("목표 줄 도달이 GameOver 가 아님")
	}
	if g.Winner != QDSouth || g.EndReason != "reach_goal" || g.Phase != QDPhaseGameOver {
		t.Errorf("승리 상태 이상: winner=%s reason=%s phase=%s", g.Winner, g.EndReason, g.Phase)
	}

	// 종료 후 이동 거부
	if _, err := g.MovePawn(QDNorth, QDCell{Row: 1, Col: 4}); err == nil {
		t.Error("게임 종료 후 이동이 허용됨")
	}
}
