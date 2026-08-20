package server

import (
	"errors"
	"math/rand"
	"strings"
	"time"
)

// ==================== 코드네임 순수 규칙 ====================
//
// 팀·역할 배정, 힌트·카드 선택 판정, 승패 결정만 다룬다. 클라이언트·타이머를
// 모르며, 허브(cn_hub.go)가 결과 구조체를 받아 이벤트로 번역한다.

// cnOtherTeam 상대 팀
func cnOtherTeam(team CNTeam) CNTeam {
	if team == CNTeamRed {
		return CNTeamBlue
	}
	return CNTeamRed
}

// cnTeamOf 좌석의 팀 — 입장 순 번갈아 (짝수 좌석=적, 홀수 좌석=청)
func cnTeamOf(seat int) CNTeam {
	if seat%2 == 0 {
		return CNTeamRed
	}
	return CNTeamBlue
}

// cnTeamName 팀 한글 이름 (이벤트 문구용)
func cnTeamName(team CNTeam) string {
	if team == CNTeamRed {
		return "적팀"
	}
	return "청팀"
}

// cnColorName 카드 색 한글 이름 (이벤트 문구용)
func cnColorName(color CNColor) string {
	switch color {
	case CNColorRed:
		return "적팀 단어"
	case CNColorBlue:
		return "청팀 단어"
	case CNColorNeutral:
		return "중립 단어"
	case CNColorAssassin:
		return "암살자"
	}
	return string(color)
}

// NewCNGame 대기 상태의 새 게임
func NewCNGame(id string) *CNGame {
	return &CNGame{
		ID:          id,
		Players:     []*CNPlayer{},
		Phase:       CNPhaseWaiting,
		Board:       []CNCard{},
		KeyCard:     []CNColor{},
		ClueHistory: []CNClueEntry{},
	}
}

// AddPlayer 대기실 입장. 좌석 번호를 돌려준다. 팀은 입장 순 번갈아
// (적,청,적,청...) 배정되고 역할 미리보기도 함께 갱신된다.
func (g *CNGame) AddPlayer(name string, bot bool) (int, error) {
	if g.Phase != CNPhaseWaiting {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= CNMaxPlayers {
		return -1, errors.New("자리가 없습니다 (최대 8명)")
	}
	seat := len(g.Players)
	g.Players = append(g.Players, &CNPlayer{
		Seat: seat, Name: name, Team: cnTeamOf(seat), IsBot: bot,
	})
	g.assignRoles()
	return seat, nil
}

// RemovePlayer 대기실 퇴장. 좌석을 앞으로 당기고 팀·역할을 다시 배정한다.
func (g *CNGame) RemovePlayer(seat int) {
	if g.Phase != CNPhaseWaiting || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i, p := range g.Players {
		p.Seat = i
		p.Team = cnTeamOf(i)
	}
	g.assignRoles()
}

// teamSeats 팀의 좌석 목록 (좌석 오름차순)
func (g *CNGame) teamSeats(team CNTeam) []int {
	seats := []int{}
	for _, p := range g.Players {
		if p.Team == team {
			seats = append(seats, p.Seat)
		}
	}
	return seats
}

// assignRoles 팀별 스파이마스터 1명(나머지는 요원)을 배정한다.
// 스파이마스터는 사람 우선(팀 내 첫 사람 좌석). 봇은 요원만 맡되,
// 사람이 1명뿐인 팀은 그 사람이 요원으로 놀 수 있게 봇이 스파이마스터가
// 된다 (사람이 없는 팀은 첫 좌석). 대기 중 미리보기와 시작 확정이 같은
// 경로를 쓴다.
func (g *CNGame) assignRoles() {
	for _, team := range []CNTeam{CNTeamRed, CNTeamBlue} {
		seats := g.teamSeats(team)
		if len(seats) == 0 {
			continue
		}
		humans := []int{}
		bots := []int{}
		for _, s := range seats {
			if g.Players[s].IsBot {
				bots = append(bots, s)
			} else {
				humans = append(humans, s)
			}
		}
		spymaster := seats[0]
		switch {
		case len(humans) >= 2:
			spymaster = humans[0]
		case len(humans) == 1 && len(bots) > 0:
			spymaster = bots[0] // 유일한 사람은 요원으로 남긴다
		case len(humans) == 1:
			spymaster = humans[0]
		}
		for _, s := range seats {
			if s == spymaster {
				g.Players[s].Role = CNRoleSpymaster
			} else {
				g.Players[s].Role = CNRoleAgent
			}
		}
	}
}

// SpymasterSeat 팀의 스파이마스터 좌석 (-1 = 없음)
func (g *CNGame) SpymasterSeat(team CNTeam) int {
	for _, p := range g.Players {
		if p.Team == team && p.Role == CNRoleSpymaster {
			return p.Seat
		}
	}
	return -1
}

// CanStart 시작 가능 여부 — 4인부터 (입장 순 번갈아 배정이라 팀당 2 보장)
func (g *CNGame) CanStart() bool {
	return g.Phase == CNPhaseWaiting && len(g.Players) >= CNMinPlayers
}

// Start 게임 시작 — 보드·키 카드를 깔고 적팀(9단어) 선공으로 힌트 단계를 연다
func (g *CNGame) Start(rng *rand.Rand) error {
	if !g.CanStart() {
		return errors.New("4명 이상 모여야 시작할 수 있습니다")
	}
	g.Ready = true
	g.StartedAt = time.Now()
	g.assignRoles() // 시작 시점 구성으로 역할 확정

	words := cnPickWords(rng)
	g.Board = make([]CNCard, 0, CNBoardSize)
	for _, w := range words {
		g.Board = append(g.Board, CNCard{Word: w})
	}
	g.KeyCard = cnDealKeyCard(rng)
	g.RedLeft = CNRedWords
	g.BlueLeft = CNBlueWords
	g.CurrentTeam = CNTeamRed
	g.Clue = nil
	g.ClueHistory = []CNClueEntry{}
	g.Phase = CNPhaseClue
	return nil
}

// GiveClue 현재 팀 스파이마스터의 힌트 기록 (단어+숫자).
// 음성으로 말한 힌트를 앱에도 남겨 히스토리가 된다. 선택 횟수는 숫자+1.
func (g *CNGame) GiveClue(seat int, word string, count int) error {
	if g.Phase != CNPhaseClue {
		return errors.New("지금은 힌트를 낼 수 없습니다")
	}
	p := g.playerAt(seat)
	if p == nil || p.Role != CNRoleSpymaster || p.Team != g.CurrentTeam {
		return errors.New("현재 팀의 스파이마스터만 힌트를 낼 수 있습니다")
	}
	word = strings.TrimSpace(word)
	if word == "" {
		return errors.New("힌트 단어를 입력해야 합니다")
	}
	if count < 1 || count > 9 {
		return errors.New("힌트 숫자는 1~9 사이여야 합니다")
	}
	g.Clue = &CNClue{Word: word, Count: count, Remaining: count + 1}
	g.ClueHistory = append(g.ClueHistory, CNClueEntry{Team: g.CurrentTeam, Word: word, Count: count})
	g.Phase = CNPhaseGuess
	return nil
}

// CNPickResult 카드 선택 한 번의 판정 결과. 허브가 이벤트로 번역한다.
type CNPickResult struct {
	Seat  int
	Index int
	Word  string
	Color CNColor

	Correct    bool // 자기 팀 단어를 맞혔다 (턴 계속 여부와 별개)
	TurnEnded  bool
	GameOver   bool
	Winner     CNTeam // GameOver 일 때만
	LoseReason string // "assassin" | ""
}

// Pick 현재 팀 요원의 카드 선택 (팀당 아무나 탭 → 확정).
// 맞으면 계속(최대 숫자+1회), 중립/상대 단어면 턴 종료, 암살자면 즉시 패배.
func (g *CNGame) Pick(seat, index int) (*CNPickResult, error) {
	if g.Phase != CNPhaseGuess {
		return nil, errors.New("지금은 카드를 선택할 수 없습니다")
	}
	p := g.playerAt(seat)
	if p == nil || p.Team != g.CurrentTeam {
		return nil, errors.New("당신 팀의 차례가 아닙니다")
	}
	if p.Role != CNRoleAgent {
		return nil, errors.New("스파이마스터는 카드를 선택할 수 없습니다")
	}
	if index < 0 || index >= len(g.Board) {
		return nil, errors.New("잘못된 카드 위치입니다")
	}
	if g.Board[index].Revealed {
		return nil, errors.New("이미 공개된 카드입니다")
	}

	g.Board[index].Revealed = true
	color := g.KeyCard[index]
	res := &CNPickResult{Seat: seat, Index: index, Word: g.Board[index].Word, Color: color}

	switch color {
	case CNColorAssassin:
		g.finish(cnOtherTeam(g.CurrentTeam), "assassin")
		res.GameOver, res.Winner, res.LoseReason = true, g.Winner, g.LoseReason
	case CNColorRed, CNColorBlue:
		if color == CNColorRed {
			g.RedLeft--
		} else {
			g.BlueLeft--
		}
		picked := CNTeamRed
		if color == CNColorBlue {
			picked = CNTeamBlue
		}
		if g.RedLeft == 0 || g.BlueLeft == 0 {
			// 어느 팀 단어든 다 까지면 그 팀 승리 (상대가 까줘도 승리)
			g.finish(picked, "")
			res.Correct = picked == g.CurrentTeam
			res.GameOver, res.Winner = true, g.Winner
			return res, nil
		}
		if picked == g.CurrentTeam {
			res.Correct = true
			g.Clue.Remaining--
			if g.Clue.Remaining <= 0 {
				g.endTurn()
				res.TurnEnded = true
			}
		} else {
			g.endTurn()
			res.TurnEnded = true
		}
	case CNColorNeutral:
		g.endTurn()
		res.TurnEnded = true
	}
	return res, nil
}

// EndTurn 현재 팀 요원의 "그만" — 남은 선택을 포기하고 턴을 넘긴다
func (g *CNGame) EndTurn(seat int) error {
	if g.Phase != CNPhaseGuess {
		return errors.New("지금은 턴을 넘길 수 없습니다")
	}
	p := g.playerAt(seat)
	if p == nil || p.Team != g.CurrentTeam || p.Role != CNRoleAgent {
		return errors.New("현재 팀의 요원만 턴을 넘길 수 있습니다")
	}
	g.endTurn()
	return nil
}

// ForceEndTurn AFK 자동 진행용 — guess 단계의 턴을 강제로 넘긴다
func (g *CNGame) ForceEndTurn() bool {
	if g.Phase != CNPhaseGuess {
		return false
	}
	g.endTurn()
	return true
}

// endTurn 턴 교대 — 힌트를 비우고 상대 팀 힌트 단계로
func (g *CNGame) endTurn() {
	g.CurrentTeam = cnOtherTeam(g.CurrentTeam)
	g.Clue = nil
	g.Phase = CNPhaseClue
}

// finish 게임 종료 확정
func (g *CNGame) finish(winner CNTeam, loseReason string) {
	g.Winner = winner
	g.LoseReason = loseReason
	g.Clue = nil
	g.Phase = CNPhaseGameOver
}

// playerAt 좌석의 플레이어 (범위 밖 nil)
func (g *CNGame) playerAt(seat int) *CNPlayer {
	if seat < 0 || seat >= len(g.Players) {
		return nil
	}
	return g.Players[seat]
}
