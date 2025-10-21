package server

import (
	"errors"

	"github.com/google/uuid"
)

// NewGame 새 게임 생성
func NewGame() *Game {
	return &Game{
		ID:            uuid.New().String(),
		Players:       make(map[PlayerColor]*Client),
		CurrentRound:  1,
		BlueWins:      0,
		RedWins:       0,
		UsedTiles:     make(map[PlayerColor][]int),
		RoundTiles:    make(map[PlayerColor]*int),
		CurrentPlayer: Blue, // 기본 선공
		Ready:         false,
	}
}

// AddPlayer 플레이어 추가
func (g *Game) AddPlayer(client *Client, color PlayerColor) error {
	if g.Players[color] != nil {
		return errors.New("이미 해당 색상의 플레이어가 존재합니다")
	}

	g.Players[color] = client
	client.Color = color

	// 두 플레이어가 모두 접속하면 게임 시작
	if len(g.Players) == 2 {
		g.Ready = true
		g.UsedTiles[Blue] = []int{}
		g.UsedTiles[Red] = []int{}
	}

	return nil
}

// PlayTile 타일 플레이
func (g *Game) PlayTile(color PlayerColor, tile int) error {
	// 유효성 검증
	if tile < 1 || tile > 9 {
		return errors.New("타일은 1-9 사이여야 합니다")
	}

	// 이미 사용한 타일인지 확인
	for _, usedTile := range g.UsedTiles[color] {
		if usedTile == tile {
			return errors.New("이미 사용한 타일입니다")
		}
	}

	// 이미 이번 라운드에 타일을 냈는지 확인
	if g.RoundTiles[color] != nil {
		return errors.New("이미 이번 라운드에 타일을 냈습니다")
	}

	// 차례 확인 - 첫 번째 플레이어이거나, 상대방이 이미 타일을 낸 경우
	opponentColor := Blue
	if color == Blue {
		opponentColor = Red
	}

	// 자신의 차례가 아니고, 상대방도 아직 타일을 내지 않았으면 에러
	if g.CurrentPlayer != color && g.RoundTiles[opponentColor] == nil {
		return errors.New("당신의 차례가 아닙니다")
	}

	// 타일 저장
	g.RoundTiles[color] = &tile
	g.UsedTiles[color] = append(g.UsedTiles[color], tile)

	return nil
}

// DetermineWinner 라운드 승자 결정
func (g *Game) DetermineWinner() PlayerColor {
	blueTile := *g.RoundTiles[Blue]
	redTile := *g.RoundTiles[Red]

	// 무승부
	if blueTile == redTile {
		return ""
	}

	// 특수 규칙: 1이 9를 이김
	if blueTile == 1 && redTile == 9 {
		return Blue
	}
	if redTile == 1 && blueTile == 9 {
		return Red
	}

	// 일반 규칙: 큰 숫자가 승리
	if blueTile > redTile {
		return Blue
	}
	return Red
}

// ProcessRound 라운드 처리
func (g *Game) ProcessRound() (PlayerColor, bool) {
	// 두 플레이어가 모두 타일을 냈는지 확인
	if g.RoundTiles[Blue] == nil || g.RoundTiles[Red] == nil {
		return "", false
	}

	// 승자 결정
	winner := g.DetermineWinner()

	// 승수 업데이트
	if winner == Blue {
		g.BlueWins++
	} else if winner == Red {
		g.RedWins++
	}

	// 다음 라운드 준비
	g.CurrentRound++
	g.RoundTiles = make(map[PlayerColor]*int)

	// 승자가 다음 선공
	if winner != "" {
		g.CurrentPlayer = winner
	}

	return winner, true
}

// IsGameOver 게임 종료 확인
func (g *Game) IsGameOver() (bool, PlayerColor) {
	// 5승 먼저 하면 승리
	if g.BlueWins >= 5 {
		return true, Blue
	}
	if g.RedWins >= 5 {
		return true, Red
	}

	// 9라운드 종료
	if g.CurrentRound > 9 {
		if g.BlueWins > g.RedWins {
			return true, Blue
		} else if g.RedWins > g.BlueWins {
			return true, Red
		}
		// 동점인 경우 무승부
		return true, ""
	}

	return false, ""
}

// GetNextPlayer 다음 플레이어 가져오기
func (g *Game) GetNextPlayer() PlayerColor {
	if g.RoundTiles[g.CurrentPlayer] != nil {
		// 현재 플레이어가 타일을 냈으면 상대방 차례
		if g.CurrentPlayer == Blue {
			return Red
		}
		return Blue
	}
	return g.CurrentPlayer
}
