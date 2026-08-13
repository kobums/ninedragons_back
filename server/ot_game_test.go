package server

import (
	"math/rand"
	"testing"
)

// newTestOTGame 손 카드·선공을 고정한 시작 상태.
// 남: 호랑이·게 / 북: 원숭이·학 / 대기: 멧돼지, 남 선공.
func newTestOTGame(t *testing.T) *OTGame {
	t.Helper()
	g := NewOTGame("test")
	if _, err := g.AddPlayer("남이"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.AddPlayer("북이"); err != nil {
		t.Fatal(err)
	}
	if err := g.Start(rand.New(rand.NewSource(1))); err != nil {
		t.Fatal(err)
	}
	g.Hands = map[OTSide][]string{
		OTSouth: {"tiger", "crab"},
		OTNorth: {"monkey", "crane"},
	}
	g.WaitingCard = "boar"
	g.CurrentSide = OTSouth
	return g
}

func TestOTStartState(t *testing.T) {
	g := newTestOTGame(t)

	if g.Phase != OTPhasePlay {
		t.Fatalf("phase = %s, want play", g.Phase)
	}
	if len(g.Pieces) != OTPieceCount*2 {
		t.Fatalf("기물 %d개, want 10", len(g.Pieces))
	}
	// 마스터는 각 뒷줄 중앙
	for _, side := range []OTSide{OTSouth, OTNorth} {
		master := g.pieceAt(otBackRow(side), OTBoardSize/2)
		if master == nil || !master.Master || master.Side != side {
			t.Errorf("%s 마스터 위치 이상", side)
		}
	}
}

func TestOTCardOrientation(t *testing.T) {
	g := newTestOTGame(t)

	// 남쪽 호랑이: 전진 2 = 행 감소. (4,2) 마스터 → (2,2)
	if _, err := g.Move(OTSouth, "tiger", OTCell{4, 2}, OTCell{2, 2}); err != nil {
		t.Fatalf("남 호랑이 전진 실패: %v", err)
	}
	// 북쪽 학: 전진 1 = 행 증가. (0,2) 마스터 → (1,2)
	if _, err := g.Move(OTNorth, "crane", OTCell{0, 2}, OTCell{1, 2}); err != nil {
		t.Fatalf("북 학 전진 실패: %v", err)
	}
	// 북쪽 원숭이 대각: 좌우도 반전된다. (0,0) 제자 → (1,1)
	g.CurrentSide = OTNorth
	if _, err := g.Move(OTNorth, "monkey", OTCell{0, 0}, OTCell{1, 1}); err != nil {
		t.Fatalf("북 원숭이 대각 실패: %v", err)
	}
}

func TestOTMoveValidation(t *testing.T) {
	g := newTestOTGame(t)

	// 차례가 아닌 진영
	if _, err := g.Move(OTNorth, "monkey", OTCell{0, 0}, OTCell{1, 1}); err == nil {
		t.Error("차례가 아닌데 이동이 허용됨")
	}
	// 손에 없는 카드
	if _, err := g.Move(OTSouth, "dragon", OTCell{4, 2}, OTCell{3, 2}); err == nil {
		t.Error("손에 없는 카드가 허용됨")
	}
	// 카드에 없는 오프셋 (호랑이로 1칸 전진)
	if _, err := g.Move(OTSouth, "tiger", OTCell{4, 2}, OTCell{3, 2}); err == nil {
		t.Error("카드에 없는 이동이 허용됨")
	}
	// 내 기물 칸으로 이동 (게 옆걸음 2칸: (4,0)→(4,2) 마스터 칸)
	if _, err := g.Move(OTSouth, "crab", OTCell{4, 0}, OTCell{4, 2}); err == nil {
		t.Error("내 기물 칸 이동이 허용됨")
	}
	// 보드 밖 (게 옆걸음: (4,4)→(4,6))
	if _, err := g.Move(OTSouth, "crab", OTCell{4, 4}, OTCell{4, 6}); err == nil {
		t.Error("보드 밖 이동이 허용됨")
	}
}

func TestOTCaptureAndCardCycle(t *testing.T) {
	g := newTestOTGame(t)

	// 남 제자를 북 제자 앞으로 옮겨 잡기 상황을 만든다
	g.pieceAt(4, 0).Row, g.pieceAt(4, 0).Col = 1, 1
	result, err := g.Move(OTSouth, "tiger", OTCell{1, 1}, OTCell{0, 1}) // 전진 2는 밖 — 대신 -1 후진? 아니, (1,1)에서 전진2 = (-1,1) 밖
	if err == nil {
		t.Fatal("보드 밖 전진이 허용됨")
	}
	_ = result

	// 게 전진 1: (1,1) → (0,1) 북 제자 잡기
	result, err = g.Move(OTSouth, "crab", OTCell{1, 1}, OTCell{0, 1})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Captured || result.CapturedMaster {
		t.Errorf("잡기 결과 이상: %+v", result)
	}
	if g.pieceAt(0, 1) == nil || g.pieceAt(0, 1).Side != OTSouth {
		t.Error("잡은 칸에 내 기물이 없다")
	}

	// 카드 순환: 쓴 게 → 대기, 대기였던 멧돼지 → 손
	if !g.hasCard(OTSouth, "boar") || g.hasCard(OTSouth, "crab") {
		t.Errorf("카드 순환 이상: 손=%v", g.Hands[OTSouth])
	}
	if g.WaitingCard != "crab" {
		t.Errorf("대기 카드 = %s, want crab", g.WaitingCard)
	}
	if g.CurrentSide != OTNorth {
		t.Error("턴이 넘어가지 않음")
	}
}

func TestOTCaptureMasterWins(t *testing.T) {
	g := newTestOTGame(t)

	// 남 제자를 북 마스터 앞에 세우고 잡는다
	g.pieceAt(4, 0).Row, g.pieceAt(4, 0).Col = 1, 2
	result, err := g.Move(OTSouth, "crab", OTCell{1, 2}, OTCell{0, 2})
	if err != nil {
		t.Fatal(err)
	}
	if !result.GameOver || !result.CapturedMaster {
		t.Errorf("마스터 잡기 결과 이상: %+v", result)
	}
	if g.Winner != OTSouth || g.EndReason != "capture_master" || g.Phase != OTPhaseGameOver {
		t.Errorf("승리 상태 이상: %s %s %s", g.Winner, g.EndReason, g.Phase)
	}
}

func TestOTReachTempleWins(t *testing.T) {
	g := newTestOTGame(t)

	// 북 마스터를 치우고 남 마스터를 사원 앞에 세운다
	g.pieceAt(0, 2).Row, g.pieceAt(0, 2).Col = 2, 4
	master := (*OTPiece)(nil)
	for _, p := range g.Pieces {
		if p.Side == OTSouth && p.Master {
			master = p
		}
	}
	master.Row, master.Col = 1, 2

	result, err := g.Move(OTSouth, "crab", OTCell{1, 2}, OTCell{0, 2})
	if err != nil {
		t.Fatal(err)
	}
	if !result.GameOver || result.Captured {
		t.Errorf("사원 도달 결과 이상: %+v", result)
	}
	if g.Winner != OTSouth || g.EndReason != "reach_temple" {
		t.Errorf("승리 상태 이상: %s %s", g.Winner, g.EndReason)
	}
}

func TestOTStudentAtTempleNoWin(t *testing.T) {
	g := newTestOTGame(t)

	// 제자가 상대 사원에 들어가도 승리가 아니다 (북 마스터는 치워둔다)
	g.pieceAt(0, 2).Row, g.pieceAt(0, 2).Col = 2, 4
	g.pieceAt(4, 0).Row, g.pieceAt(4, 0).Col = 1, 2

	result, err := g.Move(OTSouth, "crab", OTCell{1, 2}, OTCell{0, 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.GameOver || g.Phase == OTPhaseGameOver {
		t.Error("제자의 사원 도달이 승리로 처리됨")
	}
}

func TestOTPassRules(t *testing.T) {
	g := newTestOTGame(t)

	// 둘 수 있는 수가 있으면 패스 불가
	if err := g.Pass(OTSouth, "tiger"); err == nil {
		t.Error("수가 있는데 패스가 허용됨")
	}

	// 수가 전혀 없는 상황: 남 기물 5개를 0열에 세로로 세우고
	// 호랑이(전진2·후진1)·말(전진1·왼쪽1·후진1)만 들게 한다.
	// 왼쪽은 보드 밖, 전진·후진은 전부 내 기물 또는 밖.
	south, north := 0, 0
	for _, p := range g.Pieces {
		if p.Side == OTSouth {
			p.Row, p.Col = south, 0
			south++
		} else {
			p.Row, p.Col = north, 4
			north++
		}
	}
	g.Hands[OTSouth] = []string{"tiger", "horse"}

	if len(g.LegalMoves(OTSouth)) != 0 {
		t.Fatalf("합법 수가 남아 있다: %v", g.LegalMoves(OTSouth))
	}
	if err := g.Pass(OTSouth, "tiger"); err != nil {
		t.Fatalf("패스가 거부됨: %v", err)
	}
	if !g.hasCard(OTSouth, "boar") || g.WaitingCard != "tiger" {
		t.Error("패스 후 카드 순환 이상")
	}
	if g.CurrentSide != OTNorth {
		t.Error("패스 후 턴이 넘어가지 않음")
	}
}
