package server

import (
	"errors"
	"fmt"
	"math/rand"
	"time"
)

// ==================== 스파이폴 순수 게임 로직 ====================
//
// 허브 비의존. 장소·스파이 배정, 스파이 추리 판정, 투표 집계·판정을 담당한다.
// 봇(sp_bot.go)과 서버(sp_hub.go)가 같은 검증을 공유한다.

func NewSPGame(id string) *SPGame {
	return &SPGame{
		ID:             id,
		Phase:          SPPhaseWaiting,
		TimerMinutes:   SPDefaultTimerMinutes,
		CategoryChoice: SPCategoryRandom,
		SpySeat:        -1,
	}
}

// ==================== 로비 ====================

func (g *SPGame) AddPlayer(name string) (int, error) {
	if g.Ready {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= SPMaxPlayers {
		return -1, errors.New("정원이 가득 찼습니다")
	}
	seat := len(g.Players)
	g.Players = append(g.Players, SPPlayer{Seat: seat, Name: name})
	return seat, nil
}

// RemovePlayer 대기 중 이탈 — 좌석을 빼고 남은 좌석을 앞으로 당긴다
func (g *SPGame) RemovePlayer(seat int) {
	if g.Ready || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i := range g.Players {
		g.Players[i].Seat = i
	}
}

func (g *SPGame) CanStart() bool {
	return !g.Ready && len(g.Players) >= SPMinPlayers
}

// SetTimer host 의 대기실 타이머 선택 (3|5|8분)
func (g *SPGame) SetTimer(minutes int) error {
	if g.Ready {
		return errors.New("이미 시작된 게임입니다")
	}
	if minutes != 3 && minutes != 5 && minutes != 8 {
		return errors.New("타이머는 3·5·8분만 고를 수 있습니다")
	}
	g.TimerMinutes = minutes
	return nil
}

// SetCategory host 의 대기실 카테고리 선택 (랜덤 또는 목록 중 하나)
func (g *SPGame) SetCategory(category string) error {
	if g.Ready {
		return errors.New("이미 시작된 게임입니다")
	}
	if category != SPCategoryRandom {
		if _, ok := spCategories[category]; !ok {
			return errors.New("없는 카테고리입니다")
		}
	}
	g.CategoryChoice = category
	return nil
}

// Words 확정 카테고리의 단어 목록 (시작 전엔 nil)
func (g *SPGame) Words() []string {
	return spCategories[g.Category]
}

// Start 장소 1곳·스파이 1명을 무작위 배정하고 playing 으로 진입한다.
// timerDur 는 허브가 spMinute(테스트 단축 var)로 환산해 넘긴다.
func (g *SPGame) Start(rng *rand.Rand, timerDur time.Duration) error {
	if g.Ready {
		return errors.New("이미 시작된 게임입니다")
	}
	n := len(g.Players)
	if n < SPMinPlayers || n > SPMaxPlayers {
		return fmt.Errorf("%d~%d인이 필요합니다", SPMinPlayers, SPMaxPlayers)
	}
	g.Category = g.CategoryChoice
	if g.Category == SPCategoryRandom {
		g.Category = spCategoryNames[rng.Intn(len(spCategoryNames))]
	}
	words := g.Words()
	g.Location = words[rng.Intn(len(words))]
	g.SpySeat = rng.Intn(n)
	g.Phase = SPPhasePlaying
	g.Ready = true
	g.StartedAt = time.Now()
	g.EndsAt = g.StartedAt.Add(timerDur).UnixMilli()
	return nil
}

// ==================== 스파이 추리 ====================

// spAnyValidWord 어떤 카테고리에든 존재하는 단어인지 (테스트·검증용)
func spAnyValidWord(word string) bool {
	for _, words := range spCategories {
		for _, w := range words {
			if w == word {
				return true
			}
		}
	}
	return false
}

// spValidWord 확정 카테고리 단어 목록과의 정확한 문자열 일치 (계약 — 근사 매칭 없음)
func (g *SPGame) spValidWord(word string) bool {
	for _, l := range g.Words() {
		if l == word {
			return true
		}
	}
	return false
}

// Guess 스파이의 장소 추리 — playing 중 스파이만. 적중이면 스파이 승,
// 오답이면 시민팀 승 (즉시 종료).
func (g *SPGame) Guess(seat int, location string) error {
	if g.Phase != SPPhasePlaying {
		return errors.New("추리는 게임 중에만 할 수 있습니다")
	}
	if seat != g.SpySeat {
		return errors.New("스파이만 장소를 추리할 수 있습니다")
	}
	if !g.spValidWord(location) {
		return errors.New("단어 목록에 없는 단어입니다")
	}
	if location == g.Location {
		g.finish("spy", "guess_right", location, -1)
	} else {
		g.finish("citizen", "guess_wrong", location, -1)
	}
	return nil
}

// ==================== 투표 ====================

// BeginVoting 타이머 종료 — playing → voting (허브 타이머가 호출)
func (g *SPGame) BeginVoting() {
	if g.Phase != SPPhasePlaying {
		return
	}
	g.Phase = SPPhaseVoting
	g.Votes = map[int]int{}
}

// SubmitVote 공개 투표 제출 (기권 없음·자기 지목 불가, 집계 전 덮어쓰기 허용)
func (g *SPGame) SubmitVote(voter, target int) error {
	if g.Phase != SPPhaseVoting {
		return errors.New("지금은 투표 시간이 아닙니다")
	}
	if voter < 0 || voter >= len(g.Players) {
		return errors.New("잘못된 좌석입니다")
	}
	if target < 0 || target >= len(g.Players) {
		return errors.New("잘못된 대상입니다")
	}
	if target == voter {
		return errors.New("자기 자신은 지목할 수 없습니다")
	}
	g.Votes[voter] = target
	return nil
}

// VoteComplete 전원(스파이 포함)이 지목을 제출했는지
func (g *SPGame) VoteComplete() bool {
	if g.Phase != SPPhaseVoting {
		return false
	}
	for _, p := range g.Players {
		if _, ok := g.Votes[p.Seat]; !ok {
			return false
		}
	}
	return true
}

// VoteViews 공개 투표 현황 (voter 좌석 오름차순)
func (g *SPGame) VoteViews() []SPVoteView {
	views := []SPVoteView{}
	for _, p := range g.Players {
		if target, ok := g.Votes[p.Seat]; ok {
			views = append(views, SPVoteView{Voter: p.Seat, Target: target})
		}
	}
	return views
}

// ResolveVotes 투표 판정 — 단독 최다 득표자가 스파이면 시민팀 승,
// 그 외 전부(오지목·동표)는 스파이 승.
func (g *SPGame) ResolveVotes() {
	if g.Phase != SPPhaseVoting {
		return
	}
	tally := map[int]int{}
	for _, target := range g.Votes {
		tally[target]++
	}

	best := 0
	cands := []int{}
	for target, n := range tally {
		if n > best {
			best = n
			cands = []int{target}
		} else if n == best {
			cands = append(cands, target)
		}
	}

	top := -1
	if len(cands) == 1 {
		top = cands[0]
	}
	if top >= 0 && top == g.SpySeat {
		g.finish("citizen", "vote_caught", "", top)
	} else {
		g.finish("spy", "vote_missed", "", top)
	}
}

// ==================== 종료 ====================

// finish 판정 확정 — Result 를 채우고 game_over 로 진입한다
func (g *SPGame) finish(winner, reason, guessedLocation string, topSeat int) {
	g.Result = &SPResultView{
		Winner:          winner,
		SpySeat:         g.SpySeat,
		Category:        g.Category,
		Location:        g.Location,
		Reason:          reason,
		GuessedLocation: guessedLocation,
		TopSeat:         topSeat,
	}
	g.Phase = SPPhaseGameOver
}

// spReasonLabel 사유 한글 표기 (로그·전적 문구용)
func spReasonLabel(reason string) string {
	switch reason {
	case "guess_right":
		return "정답 적중"
	case "guess_wrong":
		return "추리 오답"
	case "vote_caught":
		return "스파이 검거"
	case "vote_missed":
		return "지목 실패"
	}
	return reason
}
