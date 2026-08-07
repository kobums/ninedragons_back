package server

import (
	"fmt"
	"log"
	"math/rand"
	"time"
)

// NewNCGame 새 넘버체인지 게임 생성
func NewNCGame(id string) *NCGame {
	return &NCGame{
		ID:           id,
		Players:      make(map[TeamColor]*NCClient),
		CurrentRound: 1,
		Team1Score:   0,
		Team2Score:   0,
		AvailableBlocks: map[TeamColor][]int{
			Team1: {1, 2, 3, 4, 5, 6, 7, 1, 2, 3, 4, 5, 6, 7},
			Team2: {1, 2, 3, 4, 5, 6, 7, 1, 2, 3, 4, 5, 6, 7},
		},
		RoundHistory: []NCRoundHistory{},
		RoundSubmits: make(map[TeamColor]*NCSubmit),
		Ready:        false,
	}
}

// AddPlayer 플레이어 추가
func (g *NCGame) AddPlayer(client *NCClient, preferredTeam TeamColor) TeamColor {
	// 선호하는 팀이 비어있으면 해당 팀에 배정
	if preferredTeam != "" && g.Players[preferredTeam] == nil {
		g.Players[preferredTeam] = client
		return preferredTeam
	}

	// 선호하는 팀이 없거나 이미 차있으면 빈 팀에 배정
	if g.Players[Team1] == nil {
		g.Players[Team1] = client
		return Team1
	}
	if g.Players[Team2] == nil {
		g.Players[Team2] = client
		return Team2
	}

	// 모든 팀이 차있으면 랜덤 배정
	teams := []TeamColor{Team1, Team2}
	return teams[rand.Intn(len(teams))]
}

// IsReady 게임 시작 준비 확인
func (g *NCGame) IsReady() bool {
	return len(g.Players) == 2
}

// Start 게임 시작
func (g *NCGame) Start() {
	g.Ready = true
	g.StartedAt = time.Now()
	// 랜덤으로 시작 팀 결정
	rand.Seed(time.Now().UnixNano())
	if rand.Intn(2) == 0 {
		g.CurrentTeam = Team1
	} else {
		g.CurrentTeam = Team2
	}
	log.Printf("[NC Game %s] Started - First team: %s", g.ID, g.CurrentTeam)
}

// SubmitBlocks 블록 제출
func (g *NCGame) SubmitBlocks(team TeamColor, block1, block2 int, useHidden bool, selectedBlockChoice int) error {
	// 이번 라운드에 이미 제출한 팀은 다시 제출할 수 없다
	if g.RoundSubmits[team] != nil {
		return fmt.Errorf("already submitted this round")
	}

	// 유효성 검사 (같은 숫자 두 개를 내려면 실제로 두 개를 보유해야 한다)
	if block1 == block2 {
		if g.countBlock(team, block1) < 2 {
			return fmt.Errorf("invalid blocks")
		}
	} else if !g.hasBlock(team, block1) || !g.hasBlock(team, block2) {
		return fmt.Errorf("invalid blocks")
	}

	// 히든 찬스 검사
	if useHidden {
		if team == Team1 && g.Team1UsedHidden {
			return fmt.Errorf("hidden chance already used")
		}
		if team == Team2 && g.Team2UsedHidden {
			return fmt.Errorf("hidden chance already used")
		}
	}

	// 상대가 히든 사용 시 블록 선택 검사 (1 또는 2만 허용)
	if selectedBlockChoice != 0 && selectedBlockChoice != 1 && selectedBlockChoice != 2 {
		return fmt.Errorf("invalid block choice (must be 1 or 2)")
	}

	// 제출 저장
	g.RoundSubmits[team] = &NCSubmit{
		Block1:              block1,
		Block2:              block2,
		UseHidden:           useHidden,
		SelectedBlockChoice: selectedBlockChoice,
	}

	log.Printf("[NC Game %s] Team %s submitted blocks: %d, %d (hidden: %v, choice: %d)",
		g.ID, team, block1, block2, useHidden, selectedBlockChoice)

	return nil
}

// ReadyToProcess 라운드 처리에 필요한 제출·블록 선택이 모두 끝났는지 확인
func (g *NCGame) ReadyToProcess() bool {
	if len(g.RoundSubmits) != 2 {
		return false
	}
	team1Submit := g.RoundSubmits[Team1]
	team2Submit := g.RoundSubmits[Team2]

	// 상대가 히든을 사용했으면 블록 선택이 완료돼야 한다
	if team2Submit.UseHidden && team1Submit.SelectedBlockChoice == 0 {
		return false
	}
	if team1Submit.UseHidden && team2Submit.SelectedBlockChoice == 0 {
		return false
	}
	return true
}

// ProcessRound 라운드 처리
func (g *NCGame) ProcessRound() (*NCRoundResultPayload, error) {
	// 양 팀이 모두 제출했는지 확인
	if len(g.RoundSubmits) != 2 {
		return nil, fmt.Errorf("waiting for submissions")
	}

	team1Submit := g.RoundSubmits[Team1]
	team2Submit := g.RoundSubmits[Team2]

	// 점수·플래그를 변경하기 전에 필요한 블록 선택을 먼저 검증한다.
	// (검증 전에 상태를 바꾸면 재호출 시 점수가 중복 가산된다)
	if team2Submit.UseHidden {
		if team1Submit.SelectedBlockChoice == 0 {
			return nil, fmt.Errorf("team1 must select a block (opponent used hidden)")
		}
		if team1Submit.SelectedBlockChoice != 1 && team1Submit.SelectedBlockChoice != 2 {
			return nil, fmt.Errorf("invalid block choice")
		}
	}
	if team1Submit.UseHidden {
		if team2Submit.SelectedBlockChoice == 0 {
			return nil, fmt.Errorf("team2 must select a block (opponent used hidden)")
		}
		if team2Submit.SelectedBlockChoice != 1 && team2Submit.SelectedBlockChoice != 2 {
			return nil, fmt.Errorf("invalid block choice")
		}
	}

	// 합계 계산
	team1Total := team1Submit.Block1 + team1Submit.Block2
	team2Total := team2Submit.Block1 + team2Submit.Block2

	// 승자 결정
	var winner TeamColor
	if team1Total > team2Total {
		winner = Team1
		g.Team1Score++
	} else if team2Total > team1Total {
		winner = Team2
		g.Team2Score++
	}

	// 히든 찬스 사용 기록
	if team1Submit.UseHidden {
		g.Team1UsedHidden = true
	}
	if team2Submit.UseHidden {
		g.Team2UsedHidden = true
	}

	// 블록 교환 로직
	var team1ReceivedBlock, team2ReceivedBlock int

	// 팀2가 히든 사용 시 팀1이 선택한 블록 받기, 아니면 팀1이 더 큰 블록 받기
	if team2Submit.UseHidden {
		if team1Submit.SelectedBlockChoice == 1 {
			team1ReceivedBlock = team2Submit.Block1
		} else {
			team1ReceivedBlock = team2Submit.Block2
		}
	} else {
		team1ReceivedBlock = max(team2Submit.Block1, team2Submit.Block2)
	}

	// 팀1이 히든 사용 시 팀2가 선택한 블록 받기, 아니면 팀2가 더 큰 블록 받기
	if team1Submit.UseHidden {
		if team2Submit.SelectedBlockChoice == 1 {
			team2ReceivedBlock = team1Submit.Block1
		} else {
			team2ReceivedBlock = team1Submit.Block2
		}
	} else {
		team2ReceivedBlock = max(team1Submit.Block1, team1Submit.Block2)
	}

	// 각 팀의 블록에서 제출한 블록 제거
	g.removeBlocks(Team1, team1Submit.Block1, team1Submit.Block2)
	g.removeBlocks(Team2, team2Submit.Block1, team2Submit.Block2)

	// 교환된 블록 추가
	g.AvailableBlocks[Team1] = append(g.AvailableBlocks[Team1], team1ReceivedBlock)
	g.AvailableBlocks[Team2] = append(g.AvailableBlocks[Team2], team2ReceivedBlock)

	// 라운드 히스토리 저장
	history := NCRoundHistory{
		Round:             g.CurrentRound,
		Team1Block1:       team1Submit.Block1,
		Team1Block2:       team1Submit.Block2,
		Team1Total:        team1Total,
		Team2Block1:       team2Submit.Block1,
		Team2Block2:       team2Submit.Block2,
		Team2Total:        team2Total,
		Winner:            winner,
		Team1Hidden:       team1Submit.UseHidden,
		Team2Hidden:       team2Submit.UseHidden,
		Team1ReceivedBlock: team1ReceivedBlock,
		Team2ReceivedBlock: team2ReceivedBlock,
	}
	g.RoundHistory = append(g.RoundHistory, history)

	log.Printf("[NC Game %s] Round %d result - Team1: %d, Team2: %d, Winner: %s",
		g.ID, g.CurrentRound, team1Total, team2Total, winner)

	// 다음 라운드 준비
	g.CurrentRound++
	g.RoundSubmits = make(map[TeamColor]*NCSubmit)

	// 다음 차례는 반대 팀
	var nextTeam TeamColor
	if g.CurrentTeam == Team1 {
		nextTeam = Team2
	} else {
		nextTeam = Team1
	}
	g.CurrentTeam = nextTeam

	// 결과 페이로드 생성 (nextTeam 포함)
	result := &NCRoundResultPayload{
		Round:             g.CurrentRound - 1, // 방금 끝난 라운드 번호
		Team1Block1:       team1Submit.Block1,
		Team1Block2:       team1Submit.Block2,
		Team1Total:        team1Total,
		Team2Block1:       team2Submit.Block1,
		Team2Block2:       team2Submit.Block2,
		Team2Total:        team2Total,
		Winner:            winner,
		Team1Score:        g.Team1Score,
		Team2Score:        g.Team2Score,
		Team1Hidden:       team1Submit.UseHidden,
		Team2Hidden:       team2Submit.UseHidden,
		Team1ReceivedBlock: team1ReceivedBlock,
		Team2ReceivedBlock: team2ReceivedBlock,
		NextTeam:          nextTeam,
	}

	return result, nil
}

// IsGameOver 게임 종료 확인
func (g *NCGame) IsGameOver() (bool, string) {
	// 7점 먼저 획득
	if g.Team1Score >= 7 {
		return true, "score_limit"
	}
	if g.Team2Score >= 7 {
		return true, "score_limit"
	}

	// 12라운드 완료
	if g.CurrentRound > 12 {
		return true, "rounds_complete"
	}

	return false, ""
}

// GetWinner 승자 결정
func (g *NCGame) GetWinner() TeamColor {
	if g.Team1Score > g.Team2Score {
		return Team1
	} else if g.Team2Score > g.Team1Score {
		return Team2
	}
	return "" // 무승부
}

// hasBlock 팀이 해당 블록을 가지고 있는지 확인
func (g *NCGame) hasBlock(team TeamColor, block int) bool {
	for _, b := range g.AvailableBlocks[team] {
		if b == block {
			return true
		}
	}
	return false
}

// countBlock 팀이 보유한 해당 숫자 블록 개수
func (g *NCGame) countBlock(team TeamColor, block int) int {
	count := 0
	for _, b := range g.AvailableBlocks[team] {
		if b == block {
			count++
		}
	}
	return count
}

// removeBlocks 블록 제거 (한 번만)
func (g *NCGame) removeBlocks(team TeamColor, block1, block2 int) {
	blocks := g.AvailableBlocks[team]
	newBlocks := []int{}
	removed1 := false
	removed2 := false

	for _, b := range blocks {
		if b == block1 && !removed1 {
			removed1 = true
			continue
		}
		if b == block2 && !removed2 {
			removed2 = true
			continue
		}
		newBlocks = append(newBlocks, b)
	}

	g.AvailableBlocks[team] = newBlocks
}

// max 두 수 중 큰 값 반환
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
