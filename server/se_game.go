package server

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"time"
)

// ==================== 세트 순수 규칙 ====================
//
// 덱 구성·세트 판정·바닥 보충·선착 claim 처리·종료 판정만 다룬다.
// 클라이언트·타이머를 모르며, 허브(se_hub.go)가 강제 종료 캡(10분)을 걸고
// 이벤트 큐(DrainEvents)를 방송한다.
//
// 차례가 없다. 판정 순서는 허브 고루틴에 도착한 순서 그대로이고, 이 파일의
// Claim 은 그 순서대로 한 번에 하나씩만 불린다 — 그래서 락이 필요 없다.
//
// 한 판의 흐름:
//
//	81장을 섞어 12장을 편다 (세트가 없으면 3장씩, 21장까지)
//	→ 누구든 se_claim{ids:[3]}
//	   · 성립      → +1점, 그 3장 제거 후 12장이 되도록 보충
//	   · 불성립·이미 사라진 카드 포함 → -1점(0 미만 없음) + 그 좌석만 5초 잠금
//	→ 덱이 비고 바닥에도 세트가 없으면 종료 (최고점 승, 동점 공동)
//	→ 안전장치로 시작 10분 뒤 강제 종료 (현재 점수로 정산)
//
// 반드시 끝난다 — 성립할 때마다 덱+바닥의 총 장수가 3장씩 줄고, 줄어들 수
// 없게 되면(덱 0 + 세트 없음) 즉시 종료 판정이 선다.

// NewSEGame 대기 상태의 새 게임
func NewSEGame(id string) *SEGame {
	return &SEGame{
		ID:      id,
		Players: []*SEPlayer{},
		Phase:   SEPhaseWaiting,
		Deck:    []SECard{},
		Board:   []SECard{},
	}
}

// AddPlayer 대기실 입장. 좌석 번호를 돌려준다.
func (g *SEGame) AddPlayer(name string) (int, error) {
	if g.Phase != SEPhaseWaiting {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= SEMaxPlayers {
		return -1, fmt.Errorf("자리가 없습니다 (최대 %d명)", SEMaxPlayers)
	}
	seat := len(g.Players)
	g.Players = append(g.Players, &SEPlayer{Seat: seat, Name: name})
	return seat, nil
}

// RemovePlayer 대기실 퇴장. 좌석을 앞으로 당겨 빈틈을 없앤다.
func (g *SEGame) RemovePlayer(seat int) {
	if g.Phase != SEPhaseWaiting || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i, p := range g.Players {
		p.Seat = i
	}
}

// CanStart 시작 가능 여부 — 차례가 없어 혼자서도 연습할 수 있다
func (g *SEGame) CanStart() bool {
	return g.Phase == SEPhaseWaiting && len(g.Players) >= SEMinPlayers
}

// DrainEvents 쌓인 연출 이벤트를 꺼내 비운다 (허브가 방송한다)
func (g *SEGame) DrainEvents() []SEGameEvent {
	events := g.events
	g.events = nil
	return events
}

// pushEvent 연출 이벤트 적재 (seat -1 은 좌석 없음)
func (g *SEGame) pushEvent(kind string, seat int, message string) {
	g.events = append(g.events, SEGameEvent{Kind: kind, Seat: seat, Message: message})
}

// ==================== 카드 / 덱 ====================

// seMakeCard 속성 4종으로 카드를 만든다 — id 는 조합에서 유도된다
// (모양 27 + 개수 9 + 채움 3 + 색, 0~80).
func seMakeCard(shape SEShape, count int, fill SEFill, color SEColor) SECard {
	id := seShapeIndex[shape]*27 + (count-1)*9 + seFillIndex[fill]*3 + seColorIndex[color]
	return SECard{ID: id, Shape: shape, Count: count, Fill: fill, Color: color}
}

// seCardByID id(0~80)로 카드를 복원한다 (범위 밖이면 ok=false)
func seCardByID(id int) (SECard, bool) {
	if id < 0 || id >= SEDeckSize {
		return SECard{}, false
	}
	return seMakeCard(seShapes[id/27%3], seCounts[id/9%3], seFills[id/3%3], seColors[id%3]), true
}

// seBuildDeck 81장 덱 — 4속성 × 3값의 모든 조합. id 오름차순으로 나온다.
func seBuildDeck() []SECard {
	deck := make([]SECard, 0, SEDeckSize)
	for _, shape := range seShapes {
		for _, count := range seCounts {
			for _, fill := range seFills {
				for _, color := range seColors {
					deck = append(deck, seMakeCard(shape, count, fill, color))
				}
			}
		}
	}
	return deck
}

// seCardLabel 카드 한글 표기 ("빨강 채움 다이아몬드 2") — 로그·문구용
func seCardLabel(c SECard) string {
	return fmt.Sprintf("%s %s %s %d",
		seColorLabel(c.Color), seFillLabel(c.Fill), seShapeLabel(c.Shape), c.Count)
}

// ==================== 세트 판정 (순수) ====================

// seAllSameOrAllDiff 세 값이 전부 같거나 전부 다른지 — 규칙 문장 그대로다.
// (0~2 값이라면 (a+b+c)%3==0 과 동치이고, 테스트가 그 동치를 대조한다)
func seAllSameOrAllDiff(a, b, c int) bool {
	if a == b && b == c {
		return true
	}
	return a != b && b != c && a != c
}

// seIsSet 카드 3장이 세트인지 — 네 속성 각각이 전부 같거나 전부 달라야 한다
func seIsSet(a, b, c SECard) bool {
	return seAllSameOrAllDiff(seShapeIndex[a.Shape], seShapeIndex[b.Shape], seShapeIndex[c.Shape]) &&
		seAllSameOrAllDiff(a.Count-1, b.Count-1, c.Count-1) &&
		seAllSameOrAllDiff(seFillIndex[a.Fill], seFillIndex[b.Fill], seFillIndex[c.Fill]) &&
		seAllSameOrAllDiff(seColorIndex[a.Color], seColorIndex[b.Color], seColorIndex[c.Color])
}

// seFindSet 바닥에서 세트 하나를 완전탐색으로 찾는다 (오름차순 인덱스 3개).
// 없으면 ok=false. 바닥은 최대 21장이라 1330조합, 비용은 무시할 수준이다.
func seFindSet(board []SECard) ([3]int, bool) {
	n := len(board)
	for i := 0; i < n-2; i++ {
		for j := i + 1; j < n-1; j++ {
			for k := j + 1; k < n; k++ {
				if seIsSet(board[i], board[j], board[k]) {
					return [3]int{i, j, k}, true
				}
			}
		}
	}
	return [3]int{}, false
}

// seHasSet 바닥에 세트가 하나라도 있는지
func seHasSet(board []SECard) bool {
	_, ok := seFindSet(board)
	return ok
}

// seCountSets 바닥의 세트 개수 (완전탐색 — 대조 테스트용)
func seCountSets(board []SECard) int {
	n, count := len(board), 0
	for i := 0; i < n-2; i++ {
		for j := i + 1; j < n-1; j++ {
			for k := j + 1; k < n; k++ {
				if seIsSet(board[i], board[j], board[k]) {
					count++
				}
			}
		}
	}
	return count
}

// ==================== 바닥 보충 (순수) ====================
//
//  1. 바닥이 12장 미만이면 덱에서 12장까지 채운다
//  2. 세트가 하나도 없으면 3장씩 더 편다 (최대 21장, 덱이 마르면 멈춤)
//  3. 21장에도 세트가 없으면 전부 물리고 섞어 다시 12장을 편다
//
// 3)은 규칙대로 구현했지만 실전에서는 발동하지 않는다 — 서로 다른 21장에는
// 반드시 세트가 있기 때문이다(최대 캡셋 20장). 그래도 무한 루프를 막기 위해
// 재배치 횟수를 seMaxReshuffle 로 제한한다.
//
// 입력 슬라이스를 건드리지 않고 새 슬라이스를 돌려준다. 세 번째 반환값은
// 재배치가 몇 번 일어났는지다 (연출·로그용).
func seReplenish(board, deck []SECard, rng *rand.Rand) ([]SECard, []SECard, int) {
	b := append([]SECard{}, board...)
	d := append([]SECard{}, deck...)
	reshuffles := 0

	for {
		// 1) 기본 12장까지
		for len(b) < SEBoardSize && len(d) > 0 {
			b = append(b, d[0])
			d = d[1:]
		}
		// 2) 세트가 없으면 3장씩 (21장 상한)
		for !seHasSet(b) && len(d) > 0 && len(b) < SEBoardMax {
			n := SEDealStep
			if n > len(d) {
				n = len(d)
			}
			b = append(b, d[:n]...)
			d = d[n:]
		}
		if seHasSet(b) || len(d) == 0 || reshuffles >= seMaxReshuffle {
			return b, d, reshuffles
		}
		// 3) 물리고 재배치
		d = append(append([]SECard{}, d...), b...)
		b = []SECard{}
		rng.Shuffle(len(d), func(i, j int) { d[i], d[j] = d[j], d[i] })
		reshuffles++
	}
}

// seExhausted 종료 조건 — 덱이 비고 바닥에도 세트가 없다
func seExhausted(board, deck []SECard) bool {
	return len(deck) == 0 && !seHasSet(board)
}

// ==================== claim 판정 (순수) ====================

// seJudgeClaim 바닥과 고른 카드 id 3장으로 성립 여부를 판정한다.
// idx 는 바닥에서의 위치(오름차순), message 는 그대로 lastClaim 에 실린다.
// 이미 사라진 카드가 섞여 있으면 세트 여부를 따지지 않고 오답이다.
func seJudgeClaim(board []SECard, ids []int) ([]int, bool, string) {
	idx := make([]int, 0, SEClaimSize)
	for _, id := range ids {
		pos := -1
		for i, c := range board {
			if c.ID == id {
				pos = i
				break
			}
		}
		if pos < 0 {
			return []int{}, false, seClaimGoneMsg
		}
		idx = append(idx, pos)
	}
	sort.Ints(idx)
	if !seIsSet(board[idx[0]], board[idx[1]], board[idx[2]]) {
		return idx, false, seClaimWrongMsg
	}
	return idx, true, seClaimOKMsg
}

// seRemoveAt 오름차순 인덱스 목록을 바닥에서 제거한다 (순수)
func seRemoveAt(board []SECard, idx []int) []SECard {
	drop := map[int]bool{}
	for _, i := range idx {
		drop[i] = true
	}
	out := make([]SECard, 0, len(board))
	for i, c := range board {
		if !drop[i] {
			out = append(out, c)
		}
	}
	return out
}

// seValidateIDs claim 형식 검사 — 3장·범위(0~80)·중복 없음
func seValidateIDs(ids []int) error {
	if len(ids) != SEClaimSize {
		return fmt.Errorf("카드 %d장을 골라야 합니다", SEClaimSize)
	}
	seen := map[int]bool{}
	for _, id := range ids {
		if id < 0 || id >= SEDeckSize {
			return errors.New("없는 카드입니다")
		}
		if seen[id] {
			return errors.New("같은 카드를 두 번 고를 수 없습니다")
		}
		seen[id] = true
	}
	return nil
}

// seWinners 최고점 좌석 목록 (동점이면 공동, 좌석 오름차순)
func seWinners(players []*SEPlayer) ([]int, []string) {
	seats, names := []int{}, []string{}
	best := -1
	for _, p := range players {
		if p.Score > best {
			best = p.Score
		}
	}
	for _, p := range players {
		if p.Score == best {
			seats = append(seats, p.Seat)
			names = append(names, p.Name)
		}
	}
	return seats, names
}

// ==================== 진행 ====================

// Start 게임 시작 — 81장을 섞어 바닥을 편다
func (g *SEGame) Start(rng *rand.Rand) error {
	if !g.CanStart() {
		return fmt.Errorf("%d명 이상 모여야 시작할 수 있습니다", SEMinPlayers)
	}

	deck := seBuildDeck()
	rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })

	board, deck, _ := seReplenish([]SECard{}, deck, rng)
	g.Board, g.Deck = board, deck
	g.SetsFound = 0
	g.LastClaim = nil
	g.Result = nil
	g.EndReason = ""
	g.Phase = SEPhasePlaying
	g.Ready = true
	g.StartedAt = time.Now()

	for _, p := range g.Players {
		p.Score = 0
		p.LockedUntil = 0
	}

	g.pushEvent("board_dealt", -1,
		fmt.Sprintf("바닥에 %d장을 폈습니다 — 먼저 찾는 사람이 가져갑니다", len(g.Board)))

	// 첫 배치부터 세트가 없을 수는 사실상 없지만 계약은 지킨다
	if seExhausted(g.Board, g.Deck) {
		g.finish("deck_empty")
	}
	return nil
}

// Claim 세트 선언 — 선착 판정. 허브 고루틴이 직렬로 부르므로 도착 순서가
// 곧 판정 순서다.
//
// 돌려주는 error 는 규약 위반(진행 중 아님·잠금 중·형식 오류)이라 점수와
// 잠금을 건드리지 않고 se_error 로만 응답한다. 오답 처리(-1점·잠금)는
// error 가 아니라 lastClaim.ok=false 로 나간다.
func (g *SEGame) Claim(seat int, ids []int, now time.Time, rng *rand.Rand) error {
	if g.Phase != SEPhasePlaying {
		return errors.New("진행 중인 게임이 아닙니다")
	}
	if seat < 0 || seat >= len(g.Players) {
		return errors.New("좌석을 찾을 수 없습니다")
	}
	player := g.Players[seat]

	nowMs := now.UnixMilli()
	if player.LockedUntil > nowMs {
		left := (player.LockedUntil - nowMs + 999) / 1000
		return fmt.Errorf("잠금 중입니다 — %d초 뒤 다시 시도하세요", left)
	}
	if err := seValidateIDs(ids); err != nil {
		return err
	}

	idx, ok, message := seJudgeClaim(g.Board, ids)
	g.LastClaim = &SELastClaim{
		Seat:    seat,
		Name:    player.Name,
		OK:      ok,
		IDs:     append([]int{}, ids...),
		Message: message,
	}

	if !ok {
		if player.Score > 0 {
			player.Score--
		}
		player.LockedUntil = nowMs + seClaimLock.Milliseconds()
		g.pushEvent("claim_fail", seat, fmt.Sprintf("%s — %.0f초 잠금 (현재 %d점)",
			message, seClaimLock.Seconds(), player.Score))
		return nil
	}

	taken := []SECard{g.Board[idx[0]], g.Board[idx[1]], g.Board[idx[2]]}
	player.Score++
	g.SetsFound++
	g.Board = seRemoveAt(g.Board, idx)

	board, deck, reshuffles := seReplenish(g.Board, g.Deck, rng)
	g.Board, g.Deck = board, deck

	g.pushEvent("claim_ok", seat, fmt.Sprintf("%s / %s / %s — 세트 성립! (현재 %d점)",
		seCardLabel(taken[0]), seCardLabel(taken[1]), seCardLabel(taken[2]), player.Score))
	if reshuffles > 0 {
		g.pushEvent("reshuffled", -1, "바닥에 세트가 없어 카드를 다시 폈습니다")
	} else if len(g.Board) > SEBoardSize {
		g.pushEvent("board_extended", -1,
			fmt.Sprintf("바닥에 세트가 없어 %d장까지 폈습니다", len(g.Board)))
	}

	if seExhausted(g.Board, g.Deck) {
		g.finish("deck_empty")
	}
	return nil
}

// ForceEnd 강제 종료 — 시작 10분 캡. 현재 점수 그대로 정산한다.
func (g *SEGame) ForceEnd() {
	if g.Phase != SEPhasePlaying {
		return
	}
	g.finish("time_up")
}

// finish 종료 판정 — 최고점 승, 동점이면 공동 승리
func (g *SEGame) finish(reason string) {
	seats, names := seWinners(g.Players)
	tail := "덱과 바닥이 모두 소진됐습니다"
	if reason == "time_up" {
		tail = "제한 시간이 끝났습니다"
	}

	message := fmt.Sprintf("%s — 세트 %d개, 승자 없음", tail, g.SetsFound)
	if len(names) > 0 {
		best := g.Players[seats[0]].Score
		if len(names) == 1 {
			message = fmt.Sprintf("%s — %s님 승리! (%d점, 세트 %d개)",
				tail, names[0], best, g.SetsFound)
		} else {
			message = fmt.Sprintf("%s — %d명 공동 승리! (%d점, 세트 %d개)",
				tail, len(names), best, g.SetsFound)
		}
	}

	g.Phase = SEPhaseGameOver
	g.EndReason = reason
	g.Result = &SEResult{WinnerSeats: seats, WinnerNames: names, Message: message}
	g.pushEvent("game_over", -1, message)
}
