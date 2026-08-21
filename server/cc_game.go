package server

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"time"
)

// ==================== 차오차오 순수 규칙 ====================
//
// 주사위 은닉·선언·의심 판정·말 전진/낙하/통과만 다룬다. 클라이언트·타이머를
// 모르며, 허브(cc_hub.go)가 창 마감을 걸고 이벤트 큐(DrainEvents)를 방송한다.
//
// 진행 모델: rolling(서버가 굴려 본인만 봄) → cc_declare(1~4, X 는 강제 거짓)
// → doubt_window → 전원 허용/마감이면 통과(선언 값만큼 최전방 말 전진),
// 첫 의심이 들어오면 즉시 판정 — 거짓(실제 X 또는 선언≠실제)이면 선언자
// 최전방 말 낙하, 참이면 의심자 말 낙하 + 선언자 전진. 실제 값은 의심 판정
// 뒤에만 공개된다 (통과된 선언의 주사위는 끝까지 비밀).

// NewCCGame 대기 상태의 새 게임
func NewCCGame(id string) *CCGame {
	return &CCGame{
		ID:          id,
		Players:     []*CCPlayer{},
		Phase:       CCPhaseWaiting,
		CurrentSeat: -1,
		WinnerSeat:  -1,
	}
}

// AddPlayer 대기실 입장. 좌석 번호를 돌려준다.
func (g *CCGame) AddPlayer(name string) (int, error) {
	if g.Phase != CCPhaseWaiting {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= CCMaxPlayers {
		return -1, fmt.Errorf("자리가 없습니다 (최대 %d명)", CCMaxPlayers)
	}
	seat := len(g.Players)
	g.Players = append(g.Players, &CCPlayer{
		Seat: seat, Name: name, Reserve: CCPawns, Bridge: []int{}})
	return seat, nil
}

// RemovePlayer 대기실 퇴장. 좌석을 앞으로 당겨 빈틈을 없앤다.
func (g *CCGame) RemovePlayer(seat int) {
	if g.Phase != CCPhaseWaiting || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i, p := range g.Players {
		p.Seat = i
	}
}

// CanStart 시작 가능 여부 (호스트 명시 시작 — 2인부터)
func (g *CCGame) CanStart() bool {
	return g.Phase == CCPhaseWaiting && len(g.Players) >= CCMinPlayers
}

// Start 게임 시작 — 말을 나눠 주고 선을 무작위로 정한 뒤 첫 주사위를 굴린다
func (g *CCGame) Start(rng *rand.Rand) error {
	if !g.CanStart() {
		return fmt.Errorf("%d명 이상 모여야 시작할 수 있습니다", CCMinPlayers)
	}
	g.Ready = true
	g.StartedAt = time.Now()
	for _, p := range g.Players {
		p.Reserve = CCPawns
		p.Bridge = []int{}
		p.Crossed = 0
	}
	g.CurrentSeat = rng.Intn(len(g.Players))
	g.startTurn(rng)
	return nil
}

// ==================== 이벤트 큐 ====================

func (g *CCGame) emit(kind string, seat int, msg string) {
	g.events = append(g.events, CCGameEvent{Kind: kind, Seat: seat, Message: msg})
}

// DrainEvents 쌓인 이벤트를 꺼내고 비운다 (허브가 방송)
func (g *CCGame) DrainEvents() []CCGameEvent {
	evs := g.events
	g.events = nil
	return evs
}

// ==================== 좌석 헬퍼 ====================

func (g *CCGame) aliveCount() int {
	n := 0
	for _, p := range g.Players {
		if p.Alive() {
			n++
		}
	}
	return n
}

// nextAliveSeat seat 다음의 생존 좌석 (시계 방향)
func (g *CCGame) nextAliveSeat(seat int) int {
	n := len(g.Players)
	for i := 1; i <= n; i++ {
		s := (seat + i) % n
		if g.Players[s].Alive() {
			return s
		}
	}
	return seat
}

// ==================== 차례 / 주사위 ====================

// ccRollDie 컵 속 주사위 — 면은 1,2,3,4,X,X (X 는 0)
func ccRollDie(rng *rand.Rand) int {
	if f := rng.Intn(6); f < 4 {
		return f + 1
	}
	return CCRollX
}

// startTurn 새 차례 — 서버가 주사위를 굴려 두고(스냅샷에서 본인만 본다)
// 선언을 기다린다
func (g *CCGame) startTurn(rng *rand.Rand) {
	g.Roll = ccRollDie(rng)
	g.Declared = 0
	g.responders = nil
	g.allowed = nil
	g.Phase = CCPhaseRolling
	g.StateSeq++
	cur := g.Players[g.CurrentSeat]
	g.emit("turn", cur.Seat, fmt.Sprintf("%s님의 차례 — 컵 속에서 주사위를 굴렸습니다", cur.Name))
}

// Declare 선언 — 1~4 만 유효하다. X(0)는 선언할 수 없으므로 X 를 굴린
// 차례는 자동으로 거짓 선언이 된다 (숫자를 굴렸어도 다르게 선언 가능).
// 생존 응답자가 없으면(1인 잔존) 창 없이 즉시 통과된다.
func (g *CCGame) Declare(seat, value int, rng *rand.Rand) error {
	if g.Phase != CCPhaseRolling {
		return errors.New("지금은 선언할 수 없습니다")
	}
	if seat != g.CurrentSeat {
		return errors.New("당신의 차례가 아닙니다")
	}
	if value < 1 || value > 4 {
		return errors.New("1~4 사이 값을 선언하세요")
	}
	g.Declared = value
	g.emit("declared", seat, fmt.Sprintf("%s님이 [%d]를 선언했습니다 — 의심하거나 믿으세요",
		g.Players[seat].Name, value))

	g.responders = map[int]bool{}
	for _, p := range g.Players {
		if p.Alive() && p.Seat != seat {
			g.responders[p.Seat] = true
		}
	}
	if len(g.responders) == 0 { // 혼자 남은 생존자 — 의심할 사람이 없다
		g.resolvePass(rng)
		return nil
	}
	g.allowed = map[int]bool{}
	g.Phase = CCPhaseDoubtWindow
	g.StateSeq++
	return nil
}

// SubmitAllow 의심 창 통과 동의. 응답 대상이 아니거나 이미 지나간 창의
// 뒤늦은 허용은 조용히 무시한다 (창 경쟁의 정상 경로). 전원 허용이면 통과.
func (g *CCGame) SubmitAllow(seat int, rng *rand.Rand) {
	if g.Phase != CCPhaseDoubtWindow {
		return
	}
	if !g.responders[seat] || g.allowed[seat] {
		return
	}
	g.allowed[seat] = true
	for s := range g.responders {
		if !g.allowed[s] {
			return
		}
	}
	g.resolvePass(rng)
}

// ForcePassWindow 마감 시각 경과 — 전원 허용과 같은 통과 처리 (허브 타이머)
func (g *CCGame) ForcePassWindow(rng *rand.Rand) {
	if g.Phase == CCPhaseDoubtWindow {
		g.resolvePass(rng)
	}
}

// SubmitDoubt 의심 선언 — 첫 의심이 즉시 판정된다. 거짓(실제 X 또는
// 선언≠실제)이면 선언자 최전방 말 낙하, 참이면 의심자 말 낙하 + 선언자
// 전진. 실제 값은 이 판정에서만 공개된다. 뒤늦은 의심은 조용히 무시.
func (g *CCGame) SubmitDoubt(seat int, rng *rand.Rand) {
	if g.Phase != CCPhaseDoubtWindow || !g.responders[seat] {
		return
	}
	declarer := g.Players[g.CurrentSeat]
	doubter := g.Players[seat]
	g.emit("doubt", seat, fmt.Sprintf("%s님이 %s님의 선언을 의심합니다!",
		doubter.Name, declarer.Name))

	lie := g.Roll == CCRollX || g.Roll != g.Declared
	if lie {
		msg := fmt.Sprintf("의심 성공 — 실제 주사위는 [%s]였습니다. %s님의 말이 다리에서 떨어집니다",
			ccDieName(g.Roll), declarer.Name)
		g.Reveal = &CCReveal{Seat: declarer.Seat, Declared: g.Declared, Actual: g.Roll,
			DoubterSeat: seat, Result: "doubt_success", Message: msg}
		g.emit("reveal", declarer.Seat, msg)
		g.fall(declarer)
	} else {
		msg := fmt.Sprintf("의심 실패 — 실제 주사위도 [%s]였습니다. %s님의 말이 떨어지고 %s님은 %d칸 전진합니다",
			ccDieName(g.Roll), doubter.Name, declarer.Name, g.Declared)
		g.Reveal = &CCReveal{Seat: declarer.Seat, Declared: g.Declared, Actual: g.Roll,
			DoubterSeat: seat, Result: "doubt_fail", Message: msg}
		g.emit("reveal", seat, msg)
		g.fall(doubter)
		g.advance(declarer, g.Declared)
	}
	g.finishOrNext(rng)
}

// resolvePass 의심 창 통과 — 선언 값만큼 전진. 주사위 실제 값은 공개하지
// 않는다 (lastReveal.actual = -1, 성공한 블러핑은 비밀로 남는다).
func (g *CCGame) resolvePass(rng *rand.Rand) {
	declarer := g.Players[g.CurrentSeat]
	msg := fmt.Sprintf("모두가 믿었습니다 — %s님이 %d칸 전진합니다", declarer.Name, g.Declared)
	g.Reveal = &CCReveal{Seat: declarer.Seat, Declared: g.Declared, Actual: -1,
		DoubterSeat: -1, Result: "pass", Message: msg}
	g.emit("pass", declarer.Seat, msg)
	g.advance(declarer, g.Declared)
	g.finishOrNext(rng)
}

// ==================== 말 전진 / 낙하 ====================

// advance 최전방 말(가장 앞 칸)을 steps 만큼 전진 — 다리 위에 말이 없으면
// 대기 말이 새로 진입해 steps 칸에 놓인다. 다리 끝(CCBridgeLen)을 넘으면 통과.
func (g *CCGame) advance(p *CCPlayer, steps int) {
	pos := 0
	if len(p.Bridge) > 0 {
		pos = p.Bridge[len(p.Bridge)-1] // 오름차순 유지 — 마지막이 최전방
		p.Bridge = p.Bridge[:len(p.Bridge)-1]
	} else {
		if p.Reserve == 0 {
			return // 움직일 말이 없다 (생존 검사로 도달하지 않는 방어선)
		}
		p.Reserve--
	}
	pos += steps
	if pos > CCBridgeLen {
		p.Crossed++
		g.emit("crossed", p.Seat, fmt.Sprintf("%s님의 말이 다리를 건넜습니다! (통과 %d개)",
			p.Name, p.Crossed))
		return
	}
	p.Bridge = append(p.Bridge, pos)
	sort.Ints(p.Bridge)
	g.emit("advance", p.Seat, fmt.Sprintf("%s님의 말이 %d칸으로 전진했습니다", p.Name, pos))
}

// fall 최전방 말 낙하 제거 (부활 없음) — 다리 위에 말이 없으면 대기 말이
// 하나 사라진다. 말이 다 떨어지면 탈락.
func (g *CCGame) fall(p *CCPlayer) {
	if len(p.Bridge) > 0 {
		pos := p.Bridge[len(p.Bridge)-1]
		p.Bridge = p.Bridge[:len(p.Bridge)-1]
		g.emit("fall", p.Seat, fmt.Sprintf("%s님의 말이 %d칸에서 떨어졌습니다", p.Name, pos))
	} else if p.Reserve > 0 {
		p.Reserve--
		g.emit("fall", p.Seat, fmt.Sprintf("%s님의 대기 말 하나가 사라졌습니다", p.Name))
	}
	if !p.Alive() {
		g.emit("eliminated", p.Seat, fmt.Sprintf("%s님이 말을 모두 잃고 탈락했습니다", p.Name))
	}
}

// ==================== 턴 전환 / 종료 ====================

// finishOrNext 판정 뒤의 공통 마무리 — 2개 통과 승리·전원 탈락 판정 후
// 다음 생존 좌석의 새 차례를 연다
func (g *CCGame) finishOrNext(rng *rand.Rand) {
	for _, p := range g.Players {
		if p.Crossed >= CCWinCrossed {
			g.finish(p.Seat, "two_crossed")
			return
		}
	}
	if g.aliveCount() == 0 {
		// 전원 탈락 — 통과 수 최다 승 (동수면 낮은 좌석)
		best := 0
		for _, p := range g.Players {
			if p.Crossed > g.Players[best].Crossed {
				best = p.Seat
			}
		}
		g.finish(best, "most_crossed")
		return
	}
	g.CurrentSeat = g.nextAliveSeat(g.CurrentSeat)
	g.startTurn(rng)
}

func (g *CCGame) finish(winner int, reason string) {
	g.WinnerSeat = winner
	g.EndReason = reason
	g.CurrentSeat = -1
	g.Roll = 0
	g.Declared = 0
	g.responders = nil
	g.allowed = nil
	g.Phase = CCPhaseGameOver
}
