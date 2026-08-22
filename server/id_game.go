package server

import (
	"errors"
	"fmt"
	"math/rand"
	"time"
)

// ==================== 인사이더 순수 규칙 ====================
//
// 역할 배정·제시어 추출·단계 전환·비공개 투표·개표 판정만 다룬다.
// 클라이언트·타이머를 모르며, 허브(id_hub.go)가 단계 마감을 걸고 이벤트
// 큐(DrainEvents)를 방송한다.
//
// 진행 모델: question(마스터 [정답 나옴] 대기, 5분 초과 시 전원 패배) →
// discussion(2분, 마스터 [투표 시작]으로 단축) → voting(전원 동시 비공개
// 투표, 45초 미제출은 무작위) → 개표 — 최다 득표(동표면 마스터 지목이 결정)
// 가 인사이더면 시민+마스터 승리, 아니면 인사이더 승리.

// NewIDGame 대기 상태의 새 게임
func NewIDGame(id string) *IDGame {
	return &IDGame{
		ID:          id,
		Players:     []*IDPlayer{},
		Phase:       IDPhaseWaiting,
		MasterSeat:  -1,
		InsiderSeat: -1,
	}
}

// AddPlayer 대기실 입장. 좌석 번호를 돌려준다.
func (g *IDGame) AddPlayer(name string) (int, error) {
	if g.Phase != IDPhaseWaiting {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= IDMaxPlayers {
		return -1, fmt.Errorf("자리가 없습니다 (최대 %d명)", IDMaxPlayers)
	}
	seat := len(g.Players)
	g.Players = append(g.Players, &IDPlayer{Seat: seat, Name: name, Vote: -1})
	return seat, nil
}

// RemovePlayer 대기실 퇴장. 좌석을 앞으로 당겨 빈틈을 없앤다.
func (g *IDGame) RemovePlayer(seat int) {
	if g.Phase != IDPhaseWaiting || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i, p := range g.Players {
		p.Seat = i
	}
}

// CanStart 시작 가능 여부 (호스트 명시 시작 — 4인부터)
func (g *IDGame) CanStart() bool {
	return g.Phase == IDPhaseWaiting && len(g.Players) >= IDMinPlayers
}

// Start 게임 시작 — 마스터·인사이더를 무작위로 뽑고(서로 다른 좌석) 제시어를
// 추출한 뒤 질문 타임을 연다
func (g *IDGame) Start(rng *rand.Rand) error {
	if !g.CanStart() {
		return fmt.Errorf("%d명 이상 모여야 시작할 수 있습니다", IDMinPlayers)
	}
	g.Ready = true
	g.StartedAt = time.Now()

	g.MasterSeat = rng.Intn(len(g.Players))
	others := []int{}
	for _, p := range g.Players {
		if p.Seat != g.MasterSeat {
			others = append(others, p.Seat)
		}
	}
	g.InsiderSeat = others[rng.Intn(len(others))]

	for _, p := range g.Players {
		switch p.Seat {
		case g.MasterSeat:
			p.Role = IDRoleMaster
		case g.InsiderSeat:
			p.Role = IDRoleInsider
		default:
			p.Role = IDRoleCitizen
		}
		p.Vote = -1
		p.Votes = 0
	}
	g.Word = idPickWord(rng)
	g.Result = nil
	g.EndReason = ""

	g.Phase = IDPhaseQuestion
	g.StateSeq++
	return nil
}

// ==================== 이벤트 큐 ====================

func (g *IDGame) emit(kind string, seat int, msg string) {
	g.events = append(g.events, IDGameEvent{Kind: kind, Seat: seat, Message: msg})
}

// DrainEvents 쌓인 이벤트를 꺼내고 비운다 (허브가 방송)
func (g *IDGame) DrainEvents() []IDGameEvent {
	evs := g.events
	g.events = nil
	return evs
}

// ==================== 단계 전환 ====================

// MarkCorrect 마스터의 [정답 나옴] 선언 — 질문 타임을 닫고 토론 타임을 연다
func (g *IDGame) MarkCorrect(seat int) error {
	if g.Phase != IDPhaseQuestion {
		return errors.New("지금은 정답 선언을 할 수 없습니다")
	}
	if seat != g.MasterSeat {
		return errors.New("마스터만 정답 선언을 할 수 있습니다")
	}
	g.Phase = IDPhaseDiscussion
	g.StateSeq++
	g.emit("correct", seat, fmt.Sprintf(
		"정답이 나왔습니다! 마스터 %s님의 선언 — 정답자가 인사이더인지 음성으로 토론하세요",
		g.Players[seat].Name))
	return nil
}

// OpenVote 마스터의 [투표 시작] — 토론 타임을 단축하고 투표를 연다
func (g *IDGame) OpenVote(seat int) error {
	if g.Phase != IDPhaseDiscussion {
		return errors.New("지금은 투표를 시작할 수 없습니다")
	}
	if seat != g.MasterSeat {
		return errors.New("마스터만 투표를 시작할 수 있습니다")
	}
	g.openVoteNow()
	return nil
}

// ForceOpenVote 토론 타임 마감 — 마스터 선언 없이도 투표를 연다 (허브 타이머)
func (g *IDGame) ForceOpenVote() {
	if g.Phase == IDPhaseDiscussion {
		g.openVoteNow()
	}
}

func (g *IDGame) openVoteNow() {
	for _, p := range g.Players {
		p.Vote = -1
		p.Votes = 0
	}
	g.Phase = IDPhaseVoting
	g.StateSeq++
	g.emit("vote_open", g.MasterSeat,
		"투표 시작 — 인사이더로 의심되는 사람을 비공개로 지목하세요")
}

// ForceQuestionTimeout 질문 타임 5분 초과 — 정답을 맞히지 못해 인사이더를
// 포함한 전원 패배로 종료한다 (허브 타이머)
func (g *IDGame) ForceQuestionTimeout() {
	if g.Phase != IDPhaseQuestion {
		return
	}
	msg := "제한 시간 안에 정답이 나오지 않았습니다 — 전원 패배"
	g.Result = &IDResult{InsiderSeat: g.InsiderSeat, TopSeat: -1, Winner: "none", Message: msg}
	g.EndReason = "timeout"
	g.emit("timeout", -1, msg)
	g.finish()
}

// ==================== 투표 / 개표 ====================

// SubmitVote 비공개 투표 — 전원(마스터 포함)이 인사이더로 의심되는 1인을
// 지목한다. 자기 자신 지목·중복 투표는 거부. 전원 제출 시 즉시 개표한다.
func (g *IDGame) SubmitVote(seat, target int) error {
	if g.Phase != IDPhaseVoting {
		return errors.New("지금은 투표할 수 없습니다")
	}
	if seat < 0 || seat >= len(g.Players) {
		return errors.New("잘못된 좌석입니다")
	}
	p := g.Players[seat]
	if p.Vote >= 0 {
		return errors.New("이미 투표했습니다")
	}
	if target < 0 || target >= len(g.Players) {
		return errors.New("잘못된 지목입니다")
	}
	if target == seat {
		return errors.New("자기 자신에게는 투표할 수 없습니다")
	}
	p.Vote = target
	g.emit("voted", seat, fmt.Sprintf("%s님이 투표했습니다 (%d/%d)",
		p.Name, g.votedCount(), len(g.Players)))
	if g.votedCount() == len(g.Players) {
		g.tally()
	}
	return nil
}

// ForceVoteDeadline 투표 마감 — 미제출 좌석을 무작위 투표(자기 제외)로 채우고
// 개표한다 (허브 타이머)
func (g *IDGame) ForceVoteDeadline(rng *rand.Rand) {
	if g.Phase != IDPhaseVoting {
		return
	}
	for _, p := range g.Players {
		if p.Vote >= 0 {
			continue
		}
		targets := []int{}
		for _, q := range g.Players {
			if q.Seat != p.Seat {
				targets = append(targets, q.Seat)
			}
		}
		p.Vote = targets[rng.Intn(len(targets))]
	}
	g.tally()
}

func (g *IDGame) votedCount() int {
	n := 0
	for _, p := range g.Players {
		if p.Vote >= 0 {
			n++
		}
	}
	return n
}

// tally 개표 — 최다 득표자 공개 (동표면 마스터의 지목이 후보 중에서 결정).
// 최다 득표자가 인사이더면 시민+마스터 승리, 아니면 인사이더 승리.
func (g *IDGame) tally() {
	for _, p := range g.Players {
		p.Votes = 0
	}
	for _, p := range g.Players {
		if p.Vote >= 0 && p.Vote < len(g.Players) {
			g.Players[p.Vote].Votes++
		}
	}
	max := 0
	for _, p := range g.Players {
		if p.Votes > max {
			max = p.Votes
		}
	}
	candidates := []int{}
	for _, p := range g.Players {
		if p.Votes == max {
			candidates = append(candidates, p.Seat)
		}
	}
	top := candidates[0]
	if len(candidates) > 1 {
		masterVote := g.Players[g.MasterSeat].Vote
		for _, s := range candidates {
			if s == masterVote {
				top = s
				break
			}
		}
	}

	insider := g.Players[g.InsiderSeat]
	topPlayer := g.Players[top]
	var winner, msg string
	if top == g.InsiderSeat {
		winner = "citizens"
		g.EndReason = "insider_caught"
		msg = fmt.Sprintf("최다 득표 — %s님 (%d표). 인사이더 적발! 시민과 마스터의 승리입니다",
			topPlayer.Name, topPlayer.Votes)
	} else {
		winner = "insider"
		g.EndReason = "insider_escaped"
		msg = fmt.Sprintf("최다 득표 — %s님 (%d표). 인사이더가 아니었습니다 — 인사이더 %s님의 승리!",
			topPlayer.Name, topPlayer.Votes, insider.Name)
	}
	g.Result = &IDResult{InsiderSeat: g.InsiderSeat, TopSeat: top, Winner: winner, Message: msg}
	g.emit("vote_result", top, msg)
	g.finish()
}

func (g *IDGame) finish() {
	g.Phase = IDPhaseGameOver
}
