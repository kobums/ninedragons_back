package server

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"time"
)

// ==================== 아발론 순수 게임 로직 ====================
//
// 허브 비의존. 역할 배정·원정 테이블·팀 투표 집계(과반 승인, 연속 5부결 =
// 악 승)·원정 카드 집계(실패 기준 — 7인+ 4라운드만 2장)·승리 판정 3경로
// (원정 3승 악 / 부결 5연속 악 / 선 3승 → 암살 적중·빗나감)를 담당한다.
// 봇(av_bot.go)과 서버(av_hub.go)가 같은 검증을 공유한다.

func NewAVGame(id string) *AVGame {
	return &AVGame{
		ID:             id,
		Phase:          AVPhaseWaiting,
		AssassinTarget: -1,
	}
}

// ==================== 로비 ====================

func (g *AVGame) AddPlayer(name string) (int, error) {
	if g.Ready {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= AVMaxPlayers {
		return -1, errors.New("정원이 가득 찼습니다")
	}
	seat := len(g.Players)
	g.Players = append(g.Players, AVPlayer{Seat: seat, Name: name})
	return seat, nil
}

// RemovePlayer 대기 중 이탈 — 좌석을 빼고 남은 좌석을 앞으로 당긴다
func (g *AVGame) RemovePlayer(seat int) {
	if g.Ready || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i := range g.Players {
		g.Players[i].Seat = i
	}
}

func (g *AVGame) CanStart() bool {
	return !g.Ready && len(g.Players) >= AVMinPlayers
}

// ==================== 테이블 (인원별 규칙) ====================

// avEvilCount 인원별 악 진영 수 — 5~6인 2 / 7~9인 3 / 10인 4
func avEvilCount(n int) int {
	switch {
	case n >= 10:
		return 4
	case n >= 7:
		return 3
	default:
		return 2
	}
}

// avQuestSizes 라운드 1~5 원정대 인원 테이블
func avQuestSizes(n int) [5]int {
	switch n {
	case 5:
		return [5]int{2, 3, 2, 3, 3}
	case 6:
		return [5]int{2, 3, 4, 3, 4}
	case 7:
		return [5]int{2, 3, 3, 4, 4}
	default: // 8~10인
		return [5]int{3, 4, 4, 5, 5}
	}
}

// avFailsNeeded 원정 실패에 필요한 실패 카드 장수 —
// 7인 이상의 4라운드만 2장, 그 외는 1장
func avFailsNeeded(n, round int) int {
	if n >= 7 && round == 4 {
		return 2
	}
	return 1
}

// avAssignRoles n 인분 역할 목록(멀린 1·암살자 1·악 일반·선 일반)을 셔플한다
func avAssignRoles(n int, rng *rand.Rand) []AVRole {
	roles := []AVRole{AVRoleMerlin, AVRoleAssassin}
	for i := 2; i < avEvilCount(n)+1; i++ { // 암살자 포함 악 evilCount 명
		roles = append(roles, AVRoleEvil)
	}
	for len(roles) < n {
		roles = append(roles, AVRoleGood)
	}
	rng.Shuffle(len(roles), func(i, j int) { roles[i], roles[j] = roles[j], roles[i] })
	return roles
}

// avIsEvilRole 악 진영 역할인지 (멀린은 선 — 악 명단을 볼 뿐이다)
func avIsEvilRole(role AVRole) bool {
	return role == AVRoleAssassin || role == AVRoleEvil
}

// Start 역할을 배정하고 1라운드 지명 단계로 진입한다 (시작 리더 무작위)
func (g *AVGame) Start(rng *rand.Rand) error {
	if g.Ready {
		return errors.New("이미 시작된 게임입니다")
	}
	n := len(g.Players)
	if n < AVMinPlayers || n > AVMaxPlayers {
		return fmt.Errorf("%d~%d인이 필요합니다", AVMinPlayers, AVMaxPlayers)
	}
	roles := avAssignRoles(n, rng)
	for i := range g.Players {
		g.Players[i].Role = roles[i]
	}
	g.QuestSizes = avQuestSizes(n)
	g.Round = 1
	g.RejectCount = 0
	g.LeaderSeat = rng.Intn(n)
	g.Phase = AVPhaseTeamPick
	g.Ready = true
	g.StartedAt = time.Now()
	return nil
}

// ==================== 조회 ====================

// EvilSeats 악 진영 좌석 목록 (오름차순) — 악 진영 + 멀린에게만 노출된다
func (g *AVGame) EvilSeats() []int {
	seats := []int{}
	for _, p := range g.Players {
		if avIsEvilRole(p.Role) {
			seats = append(seats, p.Seat)
		}
	}
	return seats
}

// AssassinSeat 암살자 좌석 (미시작이면 -1)
func (g *AVGame) AssassinSeat() int {
	for _, p := range g.Players {
		if p.Role == AVRoleAssassin {
			return p.Seat
		}
	}
	return -1
}

// OnTeam 좌석이 현재 원정대에 있는지
func (g *AVGame) OnTeam(seat int) bool {
	for _, s := range g.Team {
		if s == seat {
			return true
		}
	}
	return false
}

// CurrentQuestSize 현재 라운드의 원정대 인원
func (g *AVGame) CurrentQuestSize() int {
	if g.Round < 1 || g.Round > 5 {
		return 0
	}
	return g.QuestSizes[g.Round-1]
}

func (g *AVGame) winsOf(side string) int {
	n := 0
	for _, r := range g.Results {
		if r == side {
			n++
		}
	}
	return n
}

// ==================== 지명 (team_pick) ====================

// SubmitPick 리더의 원정대 지명 — 정확한 인원·중복 없는 유효 좌석이어야 한다.
// 성공 시 team_vote 로 전환한다.
func (g *AVGame) SubmitPick(seat int, seats []int) error {
	if g.Phase != AVPhaseTeamPick {
		return errors.New("지금은 원정대 지명 시간이 아닙니다")
	}
	if seat != g.LeaderSeat {
		return errors.New("리더만 원정대를 지명할 수 있습니다")
	}
	size := g.CurrentQuestSize()
	if len(seats) != size {
		return fmt.Errorf("원정대는 정확히 %d명이어야 합니다", size)
	}
	seen := map[int]bool{}
	for _, s := range seats {
		if s < 0 || s >= len(g.Players) {
			return errors.New("잘못된 좌석입니다")
		}
		if seen[s] {
			return errors.New("같은 좌석을 중복 지명할 수 없습니다")
		}
		seen[s] = true
	}
	team := append([]int{}, seats...)
	sort.Ints(team)
	g.Team = team
	g.Phase = AVPhaseTeamVote
	g.TeamVotes = map[int]bool{}
	g.RevealedVotes = nil
	return nil
}

// AutoPick 지명 타임아웃용 무작위 합법 지명 — 리더 포함 무작위 구성
func (g *AVGame) AutoPick(rng *rand.Rand) []int {
	size := g.CurrentQuestSize()
	others := []int{}
	for _, p := range g.Players {
		if p.Seat != g.LeaderSeat {
			others = append(others, p.Seat)
		}
	}
	rng.Shuffle(len(others), func(i, j int) { others[i], others[j] = others[j], others[i] })
	team := append([]int{g.LeaderSeat}, others[:size-1]...)
	return team
}

// ==================== 팀 투표 (team_vote) ====================

// SubmitTeamVote 공개 찬반 제출 (해소 전까지 덮어쓰기 허용).
// 개별 표는 전원 제출 후 일괄 공개된다 — 해소 전에는 제출 여부만.
func (g *AVGame) SubmitTeamVote(seat int, approve bool) error {
	if g.Phase != AVPhaseTeamVote {
		return errors.New("지금은 팀 투표 시간이 아닙니다")
	}
	if seat < 0 || seat >= len(g.Players) {
		return errors.New("잘못된 좌석입니다")
	}
	g.TeamVotes[seat] = approve
	return nil
}

// TeamVoteComplete 전원이 투표했는지
func (g *AVGame) TeamVoteComplete() bool {
	return g.Phase == AVPhaseTeamVote && len(g.TeamVotes) == len(g.Players)
}

// AutoCompleteTeamVote 투표 타임아웃 — 미제출자는 찬성 처리 (진행 보장:
// 반대 처리하면 AFK 가 부결 루프를 만든다)
func (g *AVGame) AutoCompleteTeamVote() {
	for _, p := range g.Players {
		if _, ok := g.TeamVotes[p.Seat]; !ok {
			g.TeamVotes[p.Seat] = true
		}
	}
}

// ResolveTeamVote 팀 투표 해소 — 과반 찬성이면 quest 로 (부결 카운트 리셋),
// 아니면 다음 리더로 team_pick (연속 5부결 = 악 즉시 승).
// 해소 시점에 표를 일괄 공개한다 (RevealedVotes).
func (g *AVGame) ResolveTeamVote() (approved bool) {
	if g.Phase != AVPhaseTeamVote {
		return false
	}
	views := []AVTeamVoteView{}
	approves := 0
	for _, p := range g.Players {
		if v, ok := g.TeamVotes[p.Seat]; ok {
			views = append(views, AVTeamVoteView{Voter: p.Seat, Approve: v})
			if v {
				approves++
			}
		}
	}
	g.RevealedVotes = views

	if approves*2 > len(g.Players) {
		g.RejectCount = 0
		g.Phase = AVPhaseQuest
		g.QuestCards = map[int]bool{}
		return true
	}

	g.RejectCount++
	if g.RejectCount >= AVMaxRejects {
		g.finish("evil", "부결 5연속")
		return false
	}
	g.advanceLeader()
	g.Phase = AVPhaseTeamPick
	g.Team = nil
	return false
}

func (g *AVGame) advanceLeader() {
	g.LeaderSeat = (g.LeaderSeat + 1) % len(g.Players)
}

// ==================== 원정 (quest) ====================

// SubmitQuest 원정 카드 제출 — 원정대원만, 선 진영은 실패 카드를 낼 수 없다.
// 개인 제출은 끝까지 비밀이며 집계(실패 N장)만 공개된다.
func (g *AVGame) SubmitQuest(seat int, success bool) error {
	if g.Phase != AVPhaseQuest {
		return errors.New("지금은 원정 시간이 아닙니다")
	}
	if seat < 0 || seat >= len(g.Players) {
		return errors.New("잘못된 좌석입니다")
	}
	if !g.OnTeam(seat) {
		return errors.New("원정대원만 카드를 낼 수 있습니다")
	}
	if !success && !avIsEvilRole(g.Players[seat].Role) {
		return errors.New("선의 세력은 실패 카드를 낼 수 없습니다")
	}
	g.QuestCards[seat] = success
	return nil
}

// QuestComplete 원정대 전원이 카드를 냈는지
func (g *AVGame) QuestComplete() bool {
	if g.Phase != AVPhaseQuest {
		return false
	}
	for _, s := range g.Team {
		if _, ok := g.QuestCards[s]; !ok {
			return false
		}
	}
	return true
}

// AutoCompleteQuest 원정 타임아웃 — 미제출 원정대원은 성공 카드 처리
func (g *AVGame) AutoCompleteQuest() {
	for _, s := range g.Team {
		if _, ok := g.QuestCards[s]; !ok {
			g.QuestCards[s] = true
		}
	}
}

// ResolveQuest 원정 집계 — 실패 기준 충족이면 악 1승, 아니면 선 1승.
// 악 3승 → 악 승 / 선 3승 → 암살 단계 / 그 외 다음 라운드 지명으로.
func (g *AVGame) ResolveQuest() (result string, fails int) {
	if g.Phase != AVPhaseQuest {
		return "", 0
	}
	for _, s := range g.Team {
		if !g.QuestCards[s] {
			fails++
		}
	}
	result = "good"
	if fails >= avFailsNeeded(len(g.Players), g.Round) {
		result = "evil"
	}
	g.LastQuest = &AVQuestView{Fails: fails, Size: len(g.Team)}
	g.Results = append(g.Results, result)

	switch {
	case g.winsOf("evil") >= 3:
		g.finish("evil", "원정 3승")
	case g.winsOf("good") >= 3:
		// 선 3승은 곧바로 승리가 아니다 — 암살자가 멀린을 지목할 마지막 기회
		g.Phase = AVPhaseAssassin
	default:
		g.Round++
		g.advanceLeader()
		g.Phase = AVPhaseTeamPick
		g.Team = nil
		g.QuestCards = nil
	}
	return result, fails
}

// ==================== 암살 (assassin) ====================

// SubmitAssassinate 암살자의 멀린 지목 — 적중 시 악 역전승, 빗나가면 선 최종 승
func (g *AVGame) SubmitAssassinate(seat, target int) error {
	if g.Phase != AVPhaseAssassin {
		return errors.New("지금은 암살 시간이 아닙니다")
	}
	if seat < 0 || seat >= len(g.Players) || g.Players[seat].Role != AVRoleAssassin {
		return errors.New("암살자만 지목할 수 있습니다")
	}
	if target < 0 || target >= len(g.Players) {
		return errors.New("잘못된 대상입니다")
	}
	if avIsEvilRole(g.Players[target].Role) {
		return errors.New("선의 세력만 지목할 수 있습니다")
	}
	g.AssassinTarget = target
	if g.Players[target].Role == AVRoleMerlin {
		g.finish("evil", "암살 적중")
	} else {
		g.finish("good", "암살 빗나감")
	}
	return nil
}

// RandomGoodSeat 암살 타임아웃용 무작위 선 플레이어 좌석
func (g *AVGame) RandomGoodSeat(rng *rand.Rand) int {
	cands := []int{}
	for _, p := range g.Players {
		if !avIsEvilRole(p.Role) {
			cands = append(cands, p.Seat)
		}
	}
	if len(cands) == 0 {
		return -1
	}
	return cands[rng.Intn(len(cands))]
}

// ==================== 종료 ====================

func (g *AVGame) finish(winner, reason string) {
	g.Winner = winner
	g.WinReason = reason
	g.Phase = AVPhaseGameOver
}

// avRoleLabel 역할 한글 표기 (로그·발표 문구용 — 원시 코드 노출 금지)
func avRoleLabel(role AVRole) string {
	switch role {
	case AVRoleMerlin:
		return "멀린"
	case AVRoleAssassin:
		return "암살자"
	case AVRoleGood:
		return "선의 세력"
	case AVRoleEvil:
		return "악의 세력"
	}
	return string(role)
}

// avSideLabel 진영 한글 표기
func avSideLabel(side string) string {
	if side == "evil" {
		return "악의 세력"
	}
	return "선의 세력"
}
