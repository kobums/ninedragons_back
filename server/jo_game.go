package server

import (
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"
)

// ==================== 저스트 원 순수 규칙 ====================
//
// 제시어 배분·단서 수집·자동 소거·판정·점수만 다룬다. 클라이언트·타이머를
// 모르며, 허브(jo_hub.go)가 단계 마감(단서 60초·추리 60초·인정 15초·정산 5초)을
// 걸고 이벤트 큐(DrainEvents)를 방송한다.
//
// 라운드 하나의 흐름:
//
//	출제자 확정(좌석 순) → 제시어 배분(출제자 제외 전원 공개)
//	→ 단서 단계: 각자 한 단어 비공개 제출 (전원 제출 또는 60초 마감)
//	→ 자동 소거: 겹친 단서·제시어와 같거나 포함 관계인 단서·빈 단서 제거
//	→ 추리 단계: 출제자가 살아남은 단서만 보고 답 제출 또는 넘김
//	→ 판정: 정규화 일치면 정답(+1). 불일치면 인정 창 15초 —
//	        출제자 외 누구든 인정하면 정답(+1), 아무도 없으면 오답(-1, 0 하한).
//	        넘김은 0점.
//	→ 정산 후 다음 라운드. 총 라운드 = 인원 × 2.
//
// 라운드는 반드시 끝난다 — 모든 대기 상태에 허브의 마감 타이머가 걸려 있고,
// 마감은 항상 "빈 단서 처리 / 넘김 / 오답 확정 / 다음 라운드"로 해소된다.

// NewJOGame 대기 상태의 새 게임
func NewJOGame(id string) *JOGame {
	return &JOGame{
		ID:          id,
		Players:     []*JOPlayer{},
		Phase:       JOPhaseWaiting,
		GuesserSeat: -1,
		Clues:       []JOClueView{},
		History:     []JOHistoryEntry{},
	}
}

// AddPlayer 대기실 입장. 좌석 번호를 돌려준다.
func (g *JOGame) AddPlayer(name string) (int, error) {
	if g.Phase != JOPhaseWaiting {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= JOMaxPlayers {
		return -1, fmt.Errorf("자리가 없습니다 (최대 %d명)", JOMaxPlayers)
	}
	seat := len(g.Players)
	g.Players = append(g.Players, &JOPlayer{Seat: seat, Name: name})
	return seat, nil
}

// RemovePlayer 대기실 퇴장. 좌석을 앞으로 당겨 빈틈을 없앤다.
func (g *JOGame) RemovePlayer(seat int) {
	if g.Phase != JOPhaseWaiting || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i, p := range g.Players {
		p.Seat = i
	}
}

// CanStart 시작 가능 여부 (호스트 명시 시작 — 3인부터)
func (g *JOGame) CanStart() bool {
	return g.Phase == JOPhaseWaiting && len(g.Players) >= JOMinPlayers
}

// ==================== 소거 규칙 (순수 함수) ====================
//
// 이 게임의 심장. 허브·게임 상태를 전혀 모르는 순수 함수로 떼어 두고 표 기반
// 테스트로 촘촘히 검증한다.

// joNormalize 단서·제시어·답의 정규화.
// 앞뒤 공백 제거 → 내부 공백 제거 → 소문자화 (순서 고정).
func joNormalize(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Join(strings.Fields(s), "")
	return strings.ToLower(s)
}

// joClueKilledByWord 제시어와의 관계만으로 지워지는 단서인지.
// 빈 단서, 제시어와 같은 단서, 제시어를 포함하거나 제시어에 포함되는 단서다.
func joClueKilledByWord(word, clue string) bool {
	nc := joNormalize(clue)
	if nc == "" {
		return true
	}
	nw := joNormalize(word)
	if nw == "" {
		return false
	}
	return strings.Contains(nw, nc) || strings.Contains(nc, nw)
}

// joEliminate 단서 목록의 소거 판정. 입력 순서 그대로 removed 플래그를 돌려준다.
//
//  1. 빈 단서(정규화 후 빈 문자열)는 지운다
//  2. 제시어와 같거나 포함 관계인 단서는 지운다
//  3. 정규화 후 서로 겹치는 단서는 겹친 것 '전부' 지운다
//
// 2)에서 이미 지워진 단서도 3)의 중복 집계에는 그대로 참여한다 — "같은 걸 낸
// 사람이 둘"이라는 사실 자체는 제시어 관계와 무관하기 때문이다 (결과는 어차피
// 둘 다 소거라 판정이 뒤집히지 않는다).
func joEliminate(word string, clues []string) []bool {
	removed := make([]bool, len(clues))
	counts := map[string]int{}
	norms := make([]string, len(clues))

	for i, clue := range clues {
		norms[i] = joNormalize(clue)
		if norms[i] == "" {
			removed[i] = true // 1) 빈 단서
			continue
		}
		counts[norms[i]]++
	}
	for i := range clues {
		if norms[i] == "" {
			continue
		}
		if joClueKilledByWord(word, clues[i]) { // 2) 제시어 관계
			removed[i] = true
		}
		if counts[norms[i]] >= 2 { // 3) 중복
			removed[i] = true
		}
	}
	return removed
}

// joSurvivors 소거되지 않은 단서만 추린다 (항상 빈 배열 이상 — nil 금지)
func joSurvivors(clues []JOClueView) []JOClueView {
	out := []JOClueView{}
	for _, c := range clues {
		if !c.Removed {
			out = append(out, c)
		}
	}
	return out
}

// ==================== 점수 등급 ====================

// joSuccess 협력 성공 판정 — 총점이 라운드 수의 절반 이상이면 성공
func joSuccess(score, totalRounds int) bool {
	return totalRounds > 0 && score*2 >= totalRounds
}

// joGrade 총점 등급 문구의 키 (만점 / 우수 / 보통 / 재도전)
func joGrade(score, totalRounds int) string {
	switch {
	case totalRounds <= 0:
		return "재도전"
	case score >= totalRounds:
		return "만점"
	case score*4 >= totalRounds*3:
		return "우수"
	case joSuccess(score, totalRounds):
		return "보통"
	default:
		return "재도전"
	}
}

// joGradeMessage 등급별 마무리 문구
func joGradeMessage(score, totalRounds int) string {
	switch joGrade(score, totalRounds) {
	case "만점":
		return fmt.Sprintf("만점! %d라운드를 하나도 놓치지 않았습니다", totalRounds)
	case "우수":
		return fmt.Sprintf("우수 — %d점, 손발이 척척 맞았습니다", score)
	case "보통":
		return fmt.Sprintf("보통 — %d점, 절반은 넘겼습니다", score)
	default:
		return fmt.Sprintf("재도전 — %d점, 다음엔 더 통하는 단어를 골라 봅시다", score)
	}
}

// ==================== 이벤트 큐 ====================

func (g *JOGame) emit(kind string, seat int, msg string) {
	g.events = append(g.events, JOGameEvent{Kind: kind, Seat: seat, Message: msg})
}

// DrainEvents 쌓인 이벤트를 꺼내고 비운다 (허브가 방송)
func (g *JOGame) DrainEvents() []JOGameEvent {
	evs := g.events
	g.events = nil
	return evs
}

// ==================== 시작 / 라운드 ====================

// Start 게임 시작 — 라운드 수를 확정하고 1라운드 단서 단계를 연다
func (g *JOGame) Start(rng *rand.Rand) error {
	if !g.CanStart() {
		return fmt.Errorf("%d명 이상 모여야 시작할 수 있습니다", JOMinPlayers)
	}
	g.Ready = true
	g.StartedAt = time.Now()
	g.Score = 0
	g.Round = 0
	g.TotalRounds = len(g.Players) * JORoundsPerPlayer
	g.words = joPickWords(rng, g.TotalRounds)
	g.History = []JOHistoryEntry{}
	g.beginRound()
	return nil
}

// beginRound 다음 라운드의 단서 단계를 연다 (출제자는 좌석 순으로 돈다)
func (g *JOGame) beginRound() {
	n := len(g.Players)
	if n == 0 || len(g.words) == 0 {
		return
	}
	g.Round++
	g.GuesserSeat = (g.Round - 1) % n
	g.Word = g.words[(g.Round-1)%len(g.words)]
	for _, p := range g.Players {
		p.Clue = ""
		p.Submitted = false
	}
	g.Clues = []JOClueView{}
	g.Guess = ""
	g.Judged = nil
	g.Phase = JOPhaseClue
	g.StateSeq++
	g.emit("round_start", g.GuesserSeat, fmt.Sprintf(
		"%d/%d 라운드 — %s님이 출제자입니다. 나머지는 제시어를 보고 단서 한 단어를 내세요",
		g.Round, g.TotalRounds, g.Players[g.GuesserSeat].Name))
}

// clueGiverCount 단서를 낼 수 있는 사람 수 (출제자 제외)
func (g *JOGame) clueGiverCount() int {
	n := len(g.Players) - 1
	if n < 0 {
		return 0
	}
	return n
}

// SubmittedCount 단서를 낸 사람 수 (출제자 제외)
func (g *JOGame) SubmittedCount() int {
	n := 0
	for _, p := range g.Players {
		if p.Seat != g.GuesserSeat && p.Submitted {
			n++
		}
	}
	return n
}

// ==================== 단서 ====================

// SubmitClue 단서 비공개 제출. 한 라운드에 한 번만 낼 수 있다 (제출 후 잠금).
func (g *JOGame) SubmitClue(seat int, text string) error {
	if g.Phase != JOPhaseClue {
		return errors.New("지금은 단서를 낼 수 없습니다")
	}
	if seat < 0 || seat >= len(g.Players) {
		return errors.New("잘못된 좌석입니다")
	}
	if seat == g.GuesserSeat {
		return errors.New("출제자는 단서를 낼 수 없습니다")
	}
	p := g.Players[seat]
	if p.Submitted {
		return errors.New("이미 단서를 제출했습니다")
	}
	clue := strings.TrimSpace(text)
	if clue == "" {
		return errors.New("단서를 입력해 주세요")
	}
	if len([]rune(clue)) > JOMaxClueLen {
		return fmt.Errorf("단서는 %d자까지 쓸 수 있습니다", JOMaxClueLen)
	}

	p.Clue = clue
	p.Submitted = true
	// 단서 본문은 절대 이벤트에 담지 않는다 — 제출 여부만 알린다
	g.emit("clue_submitted", seat, fmt.Sprintf("%s님이 단서를 냈습니다 (%d/%d)",
		p.Name, g.SubmittedCount(), g.clueGiverCount()))

	if g.SubmittedCount() >= g.clueGiverCount() {
		g.closeClues()
	}
	return nil
}

// ForceCloseClues 단서 마감 — 미제출은 빈 단서로 처리한다 (허브 타이머)
func (g *JOGame) ForceCloseClues() {
	if g.Phase != JOPhaseClue {
		return
	}
	g.closeClues()
}

// closeClues 단서를 모아 소거 판정을 마치고 추리 단계를 연다
func (g *JOGame) closeClues() {
	views := []JOClueView{}
	texts := []string{}
	for _, p := range g.Players {
		if p.Seat == g.GuesserSeat {
			continue
		}
		views = append(views, JOClueView{Seat: p.Seat, Name: p.Name, Text: p.Clue})
		texts = append(texts, p.Clue)
	}
	removed := joEliminate(g.Word, texts)
	for i := range views {
		views[i].Removed = removed[i]
	}
	g.Clues = views

	alive := len(joSurvivors(views))
	g.Phase = JOPhaseGuess
	g.StateSeq++
	g.emit("clues_revealed", g.GuesserSeat, fmt.Sprintf(
		"단서 %d개 중 %d개가 살아남았습니다 — %s님, 답을 맞혀 보세요",
		len(views), alive, g.Players[g.GuesserSeat].Name))
}

// ==================== 추리 / 판정 ====================

// SubmitGuess 출제자의 답. 정규화 일치면 즉시 정답, 아니면 인정 창을 연다.
func (g *JOGame) SubmitGuess(seat int, text string) error {
	if g.Phase != JOPhaseGuess {
		return errors.New("지금은 답을 낼 수 없습니다")
	}
	if seat != g.GuesserSeat {
		return errors.New("출제자만 답을 낼 수 있습니다")
	}
	guess := strings.TrimSpace(text)
	if guess == "" {
		return errors.New("답을 입력해 주세요")
	}
	if len([]rune(guess)) > JOMaxClueLen {
		return fmt.Errorf("답은 %d자까지 쓸 수 있습니다", JOMaxClueLen)
	}

	g.Guess = guess
	if joNormalize(guess) == joNormalize(g.Word) {
		g.finishRound(true, false, 1, fmt.Sprintf("정답입니다 — '%s'", guess))
		return nil
	}

	g.Phase = JOPhaseJudging
	g.StateSeq++
	// 제시어는 아직 밝히지 않는다 — 인정 창이 닫힌 뒤 history 로 공개된다
	g.emit("judging", seat, fmt.Sprintf(
		"%s님의 답은 '%s' — 제시어와 다릅니다. 통했다고 보면 [정답 인정]을 눌러 주세요",
		g.Players[seat].Name, guess))
	return nil
}

// Pass 출제자가 넘긴다 — 점수 변동 없음
func (g *JOGame) Pass(seat int) error {
	if g.Phase != JOPhaseGuess {
		return errors.New("지금은 넘길 수 없습니다")
	}
	if seat != g.GuesserSeat {
		return errors.New("출제자만 넘길 수 있습니다")
	}
	g.Guess = ""
	g.finishRound(false, false, 0, "넘어갔습니다 — 점수 변동 없음")
	return nil
}

// ForcePass 추리 마감 — 출제자가 응답하지 않으면 넘긴 것으로 본다 (허브 타이머)
func (g *JOGame) ForcePass() {
	if g.Phase != JOPhaseGuess {
		return
	}
	g.Guess = ""
	g.finishRound(false, false, 0, "시간이 지나 자동으로 넘어갔습니다 — 점수 변동 없음")
}

// Accept 오답 인정 — 출제자를 뺀 누구든 한 명이면 정답 처리된다 (협력 게임)
func (g *JOGame) Accept(seat int) error {
	if g.Phase != JOPhaseJudging {
		return errors.New("지금은 인정할 수 없습니다")
	}
	if seat < 0 || seat >= len(g.Players) {
		return errors.New("잘못된 좌석입니다")
	}
	if seat == g.GuesserSeat {
		return errors.New("출제자는 자기 답을 인정할 수 없습니다")
	}
	g.finishRound(true, true, 1, fmt.Sprintf("%s님이 정답으로 인정했습니다", g.Players[seat].Name))
	return nil
}

// CloseJudging 인정 창 마감 — 아무도 인정하지 않았으면 오답이다 (허브 타이머)
func (g *JOGame) CloseJudging() {
	if g.Phase != JOPhaseJudging {
		return
	}
	g.finishRound(false, false, -1, "아무도 인정하지 않아 오답으로 처리됐습니다")
}

// finishRound 라운드 정산 — 점수를 반영하고 기록을 남긴 뒤 정산 대기로 넘어간다.
// 점수는 0 미만으로 내려가지 않는다.
func (g *JOGame) finishRound(correct, accepted bool, delta int, msg string) {
	g.Score += delta
	if g.Score < 0 {
		g.Score = 0
	}
	g.Judged = &JOJudged{Correct: correct, Accepted: accepted, Message: msg}
	g.History = append(g.History, JOHistoryEntry{
		Round: g.Round, Word: g.Word, Guess: g.Guess, Correct: correct,
	})

	g.Phase = JOPhaseRoundEnd
	g.StateSeq++
	kind := "round_wrong"
	if correct {
		kind = "round_correct"
	}
	g.emit(kind, g.GuesserSeat, fmt.Sprintf(
		"%d라운드 — 제시어는 '%s' 였습니다. %s (총점 %d/%d)",
		g.Round, g.Word, msg, g.Score, g.TotalRounds))
}

// NextRound 정산 대기가 끝나면 다음 라운드를 열거나 게임을 끝낸다 (허브 타이머)
func (g *JOGame) NextRound() {
	if g.Phase != JOPhaseRoundEnd {
		return
	}
	if g.Round >= g.TotalRounds {
		g.finish()
		return
	}
	g.beginRound()
}

// finish 마지막 라운드 뒤의 종료 처리
func (g *JOGame) finish() {
	g.Phase = JOPhaseGameOver
	g.GuesserSeat = -1
	g.StateSeq++
	g.emit("game_over", -1, fmt.Sprintf("게임 종료 — 총점 %d/%d. %s",
		g.Score, g.TotalRounds, joGradeMessage(g.Score, g.TotalRounds)))
}
