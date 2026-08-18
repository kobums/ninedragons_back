package server

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"time"
)

// ==================== 스카이폴 순수 게임 로직 ====================
//
// 허브 비의존. 역할 배정·밤 해소(마피아 다수결·동률 무작위·의사 세이브·
// 경찰 결과)·투표 집계(동률/전원 기권 무처형)·승리 판정을 담당한다.
// 봇(sf_bot.go)과 서버(sf_hub.go)가 같은 검증을 공유한다.

func NewSFGame(id string) *SFGame {
	return &SFGame{
		ID:           id,
		Phase:        SFPhaseWaiting,
		NightActions: map[int]int{},
	}
}

// ==================== 로비 ====================

func (g *SFGame) AddPlayer(name string) (int, error) {
	if g.Ready {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= SFMaxPlayers {
		return -1, errors.New("정원이 가득 찼습니다")
	}
	seat := len(g.Players)
	g.Players = append(g.Players, SFPlayer{Seat: seat, Name: name, Alive: true})
	return seat, nil
}

// RemovePlayer 대기 중 이탈 — 좌석을 빼고 남은 좌석을 앞으로 당긴다
func (g *SFGame) RemovePlayer(seat int) {
	if g.Ready || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i := range g.Players {
		g.Players[i].Seat = i
	}
}

func (g *SFGame) CanStart() bool {
	return !g.Ready && len(g.Players) >= SFMinPlayers
}

// ==================== 역할 배정 ====================

// sfMafiaCount 인원별 마피아 수 — 경찰 1·의사 1 고정, 나머지 시민.
// 6인 마2 / 7인 마2 / 8인 마3 / 9인 마3 / 10인 마3.
func sfMafiaCount(n int) int {
	if n >= 8 {
		return 3
	}
	return 2
}

// sfAssignRoles n 인분 역할 목록을 만들어 셔플한다
func sfAssignRoles(n int, rng *rand.Rand) []SFRole {
	roles := []SFRole{}
	for i := 0; i < sfMafiaCount(n); i++ {
		roles = append(roles, SFRoleMafia)
	}
	roles = append(roles, SFRolePolice, SFRoleDoctor)
	for len(roles) < n {
		roles = append(roles, SFRoleCitizen)
	}
	rng.Shuffle(len(roles), func(i, j int) { roles[i], roles[j] = roles[j], roles[i] })
	return roles
}

// Start 역할을 배정하고 1일차 밤으로 진입한다
func (g *SFGame) Start(rng *rand.Rand) error {
	if g.Ready {
		return errors.New("이미 시작된 게임입니다")
	}
	n := len(g.Players)
	if n < SFMinPlayers || n > SFMaxPlayers {
		return fmt.Errorf("%d~%d인이 필요합니다", SFMinPlayers, SFMaxPlayers)
	}
	roles := sfAssignRoles(n, rng)
	for i := range g.Players {
		g.Players[i].Role = roles[i]
		g.Players[i].Alive = true
		g.Players[i].RoleRevealed = false
	}
	g.Phase = SFPhaseNight
	g.DayNo = 1
	g.NightActions = map[int]int{}
	g.Ready = true
	g.StartedAt = time.Now()
	return nil
}

// ==================== 조회 ====================

// MafiaSeats 마피아 좌석 목록 (사망자 포함 — 동료 표시가 유지되도록)
func (g *SFGame) MafiaSeats() []int {
	seats := []int{}
	for _, p := range g.Players {
		if p.Role == SFRoleMafia {
			seats = append(seats, p.Seat)
		}
	}
	return seats
}

func (g *SFGame) aliveCount() int {
	n := 0
	for _, p := range g.Players {
		if p.Alive {
			n++
		}
	}
	return n
}

func (g *SFGame) mafiaAlive() int {
	n := 0
	for _, p := range g.Players {
		if p.Alive && p.Role == SFRoleMafia {
			n++
		}
	}
	return n
}

// PendingNightCount 아직 밤 행동 제출이 남은 생존 역할자 수.
// 역할 목록 대신 인원수만 — 목록을 노출하면 밤 사망자의 비공개 역할이
// 추론된다 (예: 경찰 사망 다음 밤부터 목록에서 police 가 사라짐).
func (g *SFGame) PendingNightCount() int {
	n := 0
	for _, p := range g.Players {
		if !p.Alive || p.Role == SFRoleCitizen {
			continue
		}
		if _, ok := g.NightActions[p.Seat]; !ok {
			n++
		}
	}
	return n
}

// ==================== 밤 ====================

// SubmitNightAction 밤 행동 제출 (해소 전까지 덮어쓰기 허용)
func (g *SFGame) SubmitNightAction(seat, target int) error {
	if g.Phase != SFPhaseNight {
		return errors.New("지금은 밤이 아닙니다")
	}
	if seat < 0 || seat >= len(g.Players) {
		return errors.New("잘못된 좌석입니다")
	}
	actor := g.Players[seat]
	if !actor.Alive {
		return errors.New("사망자는 행동할 수 없습니다")
	}
	if target < 0 || target >= len(g.Players) {
		return errors.New("잘못된 대상입니다")
	}
	if !g.Players[target].Alive {
		return errors.New("생존자만 지목할 수 있습니다")
	}
	switch actor.Role {
	case SFRoleMafia:
		if g.Players[target].Role == SFRoleMafia {
			return errors.New("동료 마피아는 지목할 수 없습니다")
		}
	case SFRolePolice:
		if target == seat {
			return errors.New("자기 자신은 조사할 수 없습니다")
		}
	case SFRoleDoctor:
		// 자기 자신 포함 아무나 치료 가능
	default:
		return errors.New("시민은 밤 행동이 없습니다")
	}
	g.NightActions[seat] = target
	return nil
}

// NightComplete 밤 행동 역할(마피아·경찰·의사) 생존자 전원이 제출했는지
func (g *SFGame) NightComplete() bool {
	if g.Phase != SFPhaseNight {
		return false
	}
	for _, p := range g.Players {
		if p.Alive && p.Role != SFRoleCitizen {
			if _, ok := g.NightActions[p.Seat]; !ok {
				return false
			}
		}
	}
	return true
}

// ResolveNight 밤 해소 — 마피아 다수결(동률 무작위)로 살해 대상을 정하고,
// 의사 치료와 일치하면 세이브. 경찰 조사 결과를 기록한다.
// 반환값은 살해된 좌석 (-1 = 세이브로 아무도 죽지 않음).
// 사망 발생 시 승리 판정까지 반영한다 (승리 시 Phase 는 game_over).
func (g *SFGame) ResolveNight(rng *rand.Rand) int {
	tally := map[int]int{}
	doctorTarget := -1
	for _, p := range g.Players {
		if !p.Alive {
			continue
		}
		target, ok := g.NightActions[p.Seat]
		if !ok {
			continue
		}
		switch p.Role {
		case SFRoleMafia:
			tally[target]++
		case SFRoleDoctor:
			doctorTarget = target
		case SFRolePolice:
			g.Investigation = &SFInvestigationView{
				Seat:    target,
				IsMafia: g.Players[target].Role == SFRoleMafia,
			}
		}
	}

	kill := sfTopTarget(tally, rng)
	g.Phase = SFPhaseDayResult

	if kill < 0 || kill == doctorTarget {
		// 마피아 생존이 게임 지속의 전제라 kill<0 은 방어적 분기다
		g.Announcement = &SFAnnouncementView{Kind: "saved", Seat: -1}
		return -1
	}

	g.Players[kill].Alive = false
	// 조사한 경찰이 같은 밤에 살해되면 결과를 전달하지 않는다 (사망자는 관전만)
	if g.Players[kill].Role == SFRolePolice {
		g.Investigation = nil
	}
	g.Announcement = &SFAnnouncementView{Kind: "killed", Seat: kill}
	g.checkWin()
	return kill
}

// sfTopTarget 득표 최다 대상. 동률이면 무작위로 하나 고른다 (밤 전용 —
// 투표 동률은 무처형이므로 이 함수를 쓰지 않는다). 표가 없으면 -1.
func sfTopTarget(tally map[int]int, rng *rand.Rand) int {
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
	if len(cands) == 0 {
		return -1
	}
	sort.Ints(cands)
	return cands[rng.Intn(len(cands))]
}

// ==================== 낮 ====================

// BeginVote day_result → day_vote (발표 5초 후 허브 타이머가 호출)
func (g *SFGame) BeginVote() {
	if g.Phase != SFPhaseDayResult {
		return
	}
	g.Phase = SFPhaseDayVote
	g.Votes = map[int]int{}
	g.Execution = nil
}

// SubmitVote 공개 투표 제출 (-1 = 기권, 해소 전까지 덮어쓰기 허용)
func (g *SFGame) SubmitVote(voter, target int) error {
	if g.Phase != SFPhaseDayVote {
		return errors.New("지금은 투표 시간이 아닙니다")
	}
	if voter < 0 || voter >= len(g.Players) {
		return errors.New("잘못된 좌석입니다")
	}
	if !g.Players[voter].Alive {
		return errors.New("사망자는 투표할 수 없습니다")
	}
	if target != -1 {
		if target < 0 || target >= len(g.Players) {
			return errors.New("잘못된 대상입니다")
		}
		if !g.Players[target].Alive {
			return errors.New("생존자만 지목할 수 있습니다")
		}
		if target == voter {
			return errors.New("자기 자신에게는 투표할 수 없습니다")
		}
	}
	g.Votes[voter] = target
	return nil
}

// VoteComplete 생존자 전원이 투표(또는 기권)했는지
func (g *SFGame) VoteComplete() bool {
	if g.Phase != SFPhaseDayVote {
		return false
	}
	for _, p := range g.Players {
		if p.Alive {
			if _, ok := g.Votes[p.Seat]; !ok {
				return false
			}
		}
	}
	return true
}

// VoteViews 공개 투표 현황 (voter 좌석 오름차순)
func (g *SFGame) VoteViews() []SFVoteView {
	views := []SFVoteView{}
	for _, p := range g.Players {
		if target, ok := g.Votes[p.Seat]; ok {
			views = append(views, SFVoteView{Voter: p.Seat, Target: target})
		}
	}
	return views
}

// ResolveVotes 투표 집계 — 단독 최다 득표만 처형(역할 공개), 동률·전원
// 기권은 무처형. 처형 발생 시 승리 판정까지 반영한다.
func (g *SFGame) ResolveVotes() {
	if g.Phase != SFPhaseDayVote {
		return
	}
	tally := map[int]int{}
	for _, target := range g.Votes {
		if target >= 0 {
			tally[target]++
		}
	}
	g.Phase = SFPhaseExecution

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
	if len(cands) != 1 {
		g.Execution = &SFExecutionView{Seat: -1}
		return
	}

	seat := cands[0]
	g.Players[seat].Alive = false
	g.Players[seat].RoleRevealed = true
	g.Execution = &SFExecutionView{Seat: seat, Role: string(g.Players[seat].Role)}
	g.checkWin()
}

// BeginNight execution → 다음 밤 (처형 발표 5초 후 허브 타이머가 호출)
func (g *SFGame) BeginNight() {
	if g.Phase != SFPhaseExecution {
		return
	}
	g.Phase = SFPhaseNight
	g.DayNo++
	g.NightActions = map[int]int{}
	g.Announcement = nil
	g.Execution = nil
	g.Investigation = nil
	g.Votes = nil
}

// ==================== 승리 판정 ====================

// checkWin 매 사망/처형 후 호출. 마피아 0 → 시민 승,
// 마피아 수 ≥ 나머지 생존자 수 → 마피아 승. 종료 시 전원 역할 공개.
func (g *SFGame) checkWin() {
	mafia := g.mafiaAlive()
	rest := g.aliveCount() - mafia
	switch {
	case mafia == 0:
		g.finish("citizen")
	case mafia >= rest:
		g.finish("mafia")
	}
}

func (g *SFGame) finish(winner string) {
	g.Winner = winner
	g.Phase = SFPhaseGameOver
	for i := range g.Players {
		g.Players[i].RoleRevealed = true
	}
}

// sfRoleLabel 역할 한글 표기 (로그·발표 문구용)
func sfRoleLabel(role SFRole) string {
	switch role {
	case SFRoleMafia:
		return "마피아"
	case SFRolePolice:
		return "경찰"
	case SFRoleDoctor:
		return "의사"
	case SFRoleCitizen:
		return "시민"
	}
	return string(role)
}
