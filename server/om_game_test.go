package server

import (
	"testing"
)

// ==================== 오목 승리 판정 유닛 테스트 ====================

// newStartedOMGame 2인 입장 → 플레이 시작 상태의 게임
func newStartedOMGame(t *testing.T) *OMGame {
	t.Helper()
	g := NewOMGame("test")
	color, err := g.AddPlayer("흑돌")
	if err != nil || color != OMBlack {
		t.Fatalf("첫 입장자 = %v (err=%v), want black", color, err)
	}
	color, err = g.AddPlayer("백돌")
	if err != nil || color != OMWhite {
		t.Fatalf("둘째 입장자 = %v (err=%v), want white", color, err)
	}
	if err := g.Start(); err != nil {
		t.Fatalf("Start 실패: %v", err)
	}
	if g.CurrentColor != OMBlack {
		t.Fatalf("선공 = %v, want black", g.CurrentColor)
	}
	return g
}

// omPlayAll 흑·백 교대로 착수 (흑부터). 마지막 수까지 전부 성공해야 한다.
func omPlayAll(t *testing.T, g *OMGame, moves []OMCell) {
	t.Helper()
	color := OMBlack
	for i, m := range moves {
		if err := g.Place(color, m.Row, m.Col); err != nil {
			t.Fatalf("%d번째 수 (%d,%d) 실패: %v", i, m.Row, m.Col, err)
		}
		color = omOther(color)
	}
}

// assertOMWin 흑 승리·오목 완성·승리선 검증
func assertOMWin(t *testing.T, g *OMGame, wantLine []OMCell) {
	t.Helper()
	if g.Phase != OMPhaseGameOver {
		t.Fatalf("phase = %v, want game_over", g.Phase)
	}
	if g.Winner != OMBlack || g.EndReason != "five" {
		t.Fatalf("winner=%v reason=%v, want black/five", g.Winner, g.EndReason)
	}
	if g.CurrentColor != "" {
		t.Fatalf("종료 후 currentColor = %v, want 빈 값", g.CurrentColor)
	}
	if len(g.WinLine) != len(wantLine) {
		t.Fatalf("승리선 길이 = %d, want %d (%v)", len(g.WinLine), len(wantLine), g.WinLine)
	}
	for i, cell := range wantLine {
		if g.WinLine[i] != cell {
			t.Fatalf("승리선[%d] = %v, want %v", i, g.WinLine[i], cell)
		}
	}
}

func TestOMWinHorizontal(t *testing.T) {
	g := newStartedOMGame(t)
	omPlayAll(t, g, []OMCell{
		{7, 3}, {0, 0}, {7, 4}, {0, 2}, {7, 5}, {0, 4}, {7, 6}, {0, 6}, {7, 7},
	})
	assertOMWin(t, g, []OMCell{{7, 3}, {7, 4}, {7, 5}, {7, 6}, {7, 7}})
}

func TestOMWinVertical(t *testing.T) {
	g := newStartedOMGame(t)
	omPlayAll(t, g, []OMCell{
		{3, 7}, {14, 0}, {4, 7}, {14, 2}, {5, 7}, {14, 4}, {6, 7}, {14, 6}, {7, 7},
	})
	assertOMWin(t, g, []OMCell{{3, 7}, {4, 7}, {5, 7}, {6, 7}, {7, 7}})
}

func TestOMWinDiagonal(t *testing.T) {
	g := newStartedOMGame(t)
	omPlayAll(t, g, []OMCell{
		{3, 3}, {14, 0}, {4, 4}, {14, 2}, {5, 5}, {14, 4}, {6, 6}, {14, 6}, {7, 7},
	})
	assertOMWin(t, g, []OMCell{{3, 3}, {4, 4}, {5, 5}, {6, 6}, {7, 7}})
}

func TestOMWinAntiDiagonal(t *testing.T) {
	g := newStartedOMGame(t)
	omPlayAll(t, g, []OMCell{
		{3, 11}, {14, 0}, {4, 10}, {14, 2}, {5, 9}, {14, 4}, {6, 8}, {14, 6}, {7, 7},
	})
	assertOMWin(t, g, []OMCell{{3, 11}, {4, 10}, {5, 9}, {6, 8}, {7, 7}})
}

// TestOMWinOverline 장목(6목 이상)도 승리 — 프리스타일 규칙
func TestOMWinOverline(t *testing.T) {
	g := newStartedOMGame(t)
	// 흑: 4,5,6 / 8,9 를 깔아 두고 마지막에 7 을 채워 6목 완성
	omPlayAll(t, g, []OMCell{
		{7, 4}, {0, 0}, {7, 5}, {0, 2}, {7, 6}, {0, 4}, {7, 8}, {0, 6}, {7, 9}, {0, 8}, {7, 7},
	})
	assertOMWin(t, g, []OMCell{{7, 4}, {7, 5}, {7, 6}, {7, 7}, {7, 8}, {7, 9}})
}

// TestOMDraw 225수를 5목 없이 소진하면 무승부(만패)
func TestOMDraw(t *testing.T) {
	g := newStartedOMGame(t)

	// 어느 방향으로도 연속이 2를 넘지 않는 주기-4 패턴으로 보드를 가른다.
	// 최종 배치의 부분집합엔 5목이 있을 수 없으므로 착수 순서는 자유다.
	blacks := []OMCell{}
	whites := []OMCell{}
	for r := 0; r < OMBoardSize; r++ {
		for c := 0; c < OMBoardSize; c++ {
			if (c+2*(r%2))%4 < 2 {
				blacks = append(blacks, OMCell{Row: r, Col: c})
			} else {
				whites = append(whites, OMCell{Row: r, Col: c})
			}
		}
	}
	if len(blacks) != 113 || len(whites) != 112 {
		t.Fatalf("패턴 분할 이상: 흑 %d, 백 %d", len(blacks), len(whites))
	}

	for i, b := range blacks {
		if err := g.Place(OMBlack, b.Row, b.Col); err != nil {
			t.Fatalf("흑 %d수 (%d,%d) 실패: %v", i, b.Row, b.Col, err)
		}
		if i < len(whites) {
			w := whites[i]
			if err := g.Place(OMWhite, w.Row, w.Col); err != nil {
				t.Fatalf("백 %d수 (%d,%d) 실패: %v", i, w.Row, w.Col, err)
			}
		}
	}

	if g.Phase != OMPhaseGameOver {
		t.Fatalf("phase = %v, want game_over", g.Phase)
	}
	if g.Winner != "" || g.EndReason != "draw" {
		t.Fatalf("winner=%q reason=%q, want 무승부/draw", g.Winner, g.EndReason)
	}
	if g.MoveCount != OMMaxMoves {
		t.Fatalf("moveCount = %d, want %d", g.MoveCount, OMMaxMoves)
	}
	if g.WinLine == nil || len(g.WinLine) != 0 {
		t.Fatalf("무승부 승리선 = %v, want 빈 슬라이스", g.WinLine)
	}
}

// TestOMPlaceErrors 착수 검증: 차례·중복·보드 밖
func TestOMPlaceErrors(t *testing.T) {
	g := newStartedOMGame(t)

	if err := g.Place(OMWhite, 7, 7); err == nil {
		t.Fatal("백이 선공으로 두는데 에러가 없다")
	}
	if err := g.Place(OMBlack, -1, 7); err == nil {
		t.Fatal("보드 밖 착수인데 에러가 없다")
	}
	if err := g.Place(OMBlack, 7, OMBoardSize); err == nil {
		t.Fatal("보드 밖 착수인데 에러가 없다")
	}
	if err := g.Place(OMBlack, 7, 7); err != nil {
		t.Fatalf("정상 착수 실패: %v", err)
	}
	if err := g.Place(OMWhite, 7, 7); err == nil {
		t.Fatal("이미 돌이 있는 자리인데 에러가 없다")
	}
	if g.MoveCount != 1 {
		t.Fatalf("moveCount = %d, want 1 (거부된 수가 반영됨)", g.MoveCount)
	}
}
