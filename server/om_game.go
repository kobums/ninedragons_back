package server

import (
	"errors"
	"time"
)

// NewOMGame 로비 상태의 새 게임
func NewOMGame(id string) *OMGame {
	return &OMGame{
		ID:    id,
		Names: map[OMColor]string{},
		Phase: OMPhaseLobby,
	}
}

// AddPlayer 입장. 먼저 온 사람이 흑(선공). 봇전은 사람이 먼저 앉으므로 사람이 흑.
func (g *OMGame) AddPlayer(name string) (OMColor, error) {
	if g.Phase != OMPhaseLobby {
		return "", errors.New("이미 시작된 게임입니다")
	}
	if _, ok := g.Names[OMBlack]; !ok {
		g.Names[OMBlack] = name
		return OMBlack, nil
	}
	if _, ok := g.Names[OMWhite]; !ok {
		g.Names[OMWhite] = name
		return OMWhite, nil
	}
	return "", errors.New("자리가 없습니다")
}

// IsReady 게임 시작 준비 확인
func (g *OMGame) IsReady() bool {
	return len(g.Names) == 2
}

// Start 흑 선공으로 플레이를 시작한다
func (g *OMGame) Start() error {
	if !g.IsReady() {
		return errors.New("시작할 수 없습니다 (2명 필요)")
	}
	g.CurrentColor = OMBlack
	g.Phase = OMPhasePlay
	g.Ready = true
	g.StartedAt = time.Now()
	return nil
}

// omStone 색 → 보드 값 (1 흑, 2 백)
func omStone(color OMColor) int {
	if color == OMBlack {
		return 1
	}
	return 2
}

func omInBoard(row, col int) bool {
	return row >= 0 && row < OMBoardSize && col >= 0 && col < OMBoardSize
}

// omDirs 연속 판정 4방향 (가로·세로·대각 2)
var omDirs = [4][2]int{{0, 1}, {1, 0}, {1, 1}, {1, -1}}

// omRunLine (row,col) 돌을 지나는 dir 방향의 연속 동색 돌 좌표 (양끝까지 확장)
func omRunLine(board *[OMBoardSize][OMBoardSize]int, row, col int, dir [2]int) []OMCell {
	stone := board[row][col]

	// 역방향으로 연속 구간의 시작을 찾은 뒤 순방향으로 수집한다
	r, c := row, col
	for omInBoard(r-dir[0], c-dir[1]) && board[r-dir[0]][c-dir[1]] == stone {
		r, c = r-dir[0], c-dir[1]
	}
	line := []OMCell{}
	for omInBoard(r, c) && board[r][c] == stone {
		line = append(line, OMCell{Row: r, Col: c})
		r, c = r+dir[0], c+dir[1]
	}
	return line
}

// omWinLine 마지막 착수 (row,col) 로 5목 이상이 완성됐으면 그 좌표들, 아니면 nil.
// 장목(6목 이상)도 승리 — 연속 전체를 돌려준다.
func omWinLine(board *[OMBoardSize][OMBoardSize]int, row, col int) []OMCell {
	for _, dir := range omDirs {
		if line := omRunLine(board, row, col, dir); len(line) >= 5 {
			return line
		}
	}
	return nil
}

// Place 착수. 5목 이상 완성 시 즉시 승리, 225수 소진 시 무승부.
// 게임이 끝나면 CurrentColor 를 비워 어느 쪽 차례도 아니게 한다.
func (g *OMGame) Place(color OMColor, row, col int) error {
	if g.Phase != OMPhasePlay {
		return errors.New("지금은 돌을 놓을 수 없습니다")
	}
	if color != g.CurrentColor {
		return errors.New("당신의 차례가 아닙니다")
	}
	if !omInBoard(row, col) {
		return errors.New("보드 밖에는 돌을 놓을 수 없습니다")
	}
	if g.Board[row][col] != 0 {
		return errors.New("이미 돌이 있는 자리입니다")
	}

	g.Board[row][col] = omStone(color)
	g.MoveCount++
	g.LastMove = &OMCell{Row: row, Col: col}

	if line := omWinLine(&g.Board, row, col); line != nil {
		g.Winner = color
		g.EndReason = "five"
		g.WinLine = line
		g.Phase = OMPhaseGameOver
		g.CurrentColor = ""
		return nil
	}

	if g.MoveCount >= OMMaxMoves {
		g.Winner = ""
		g.EndReason = "draw"
		g.WinLine = []OMCell{}
		g.Phase = OMPhaseGameOver
		g.CurrentColor = ""
		return nil
	}

	g.CurrentColor = omOther(color)
	return nil
}
