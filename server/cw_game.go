package server

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"time"
)

// ==================== 더 크루 순수 규칙 ====================
//
// 덱 배분·사령관 결정·임무 배정·트릭 진행·소통 검증·성공/실패 판정만 다룬다.
// 클라이언트·타이머를 모르며, 허브(cw_hub.go)가 카드 마감(45초)과 임무 정산
// 대기(5초)를 걸고 이벤트 큐(DrainEvents)를 방송한다.
//
// 라운드(=임무 단계) 하나의 흐름:
//
//	40장을 전원에게 균등 배분(나머지는 앞자리 좌석부터 1장씩 더)
//	→ 로켓 4 보유자가 사령관 = 첫 리드
//	→ 색 숫자 카드 중 mission 장을 뽑아 좌석에 배정 (전원 공개)
//	→ 트릭 반복: 리드 무늬 따라내기 의무, 로켓 있으면 로켓 최고가 승
//	→ 트릭이 끝날 때마다 그 안의 임무 카드를 판정
//	   · 담당자가 이겼다 → 완료
//	   · 남이 이겼다     → 즉시 실패 (wrong_winner)
//	→ 임무를 다 마치면 성공 (다음 임무 / maxMission 이면 클리어)
//	→ 카드가 다 떨어졌는데 임무가 남았으면 실패 (out_of_cards)
//
// 성공이든 실패든 라운드는 반드시 끝난다 — 카드는 매 트릭 줄어들고, 카드가
// 바닥나면 out_of_cards 로 종료된다.

// NewCWGame 대기 상태의 새 게임
func NewCWGame(id string) *CWGame {
	return &CWGame{
		ID:            id,
		Players:       []*CWPlayer{},
		Phase:         CWPhaseWaiting,
		MaxMission:    CWDefaultMaxMission,
		CommanderSeat: -1,
		CurrentSeat:   -1,
		Trick:         []CWTrickCard{},
		Tasks:         []CWTask{},
	}
}

// AddPlayer 대기실 입장. 좌석 번호를 돌려준다.
func (g *CWGame) AddPlayer(name string) (int, error) {
	if g.Phase != CWPhaseWaiting {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= CWMaxPlayers {
		return -1, fmt.Errorf("자리가 없습니다 (최대 %d명)", CWMaxPlayers)
	}
	seat := len(g.Players)
	g.Players = append(g.Players, &CWPlayer{
		Seat: seat,
		Name: name,
		Hand: []CWCard{},
	})
	return seat, nil
}

// RemovePlayer 대기실 퇴장. 좌석을 앞으로 당겨 빈틈을 없앤다.
func (g *CWGame) RemovePlayer(seat int) {
	if g.Phase != CWPhaseWaiting || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i, p := range g.Players {
		p.Seat = i
	}
}

// CanStart 시작 가능 여부 (호스트 명시 시작 — 3인부터)
func (g *CWGame) CanStart() bool {
	return g.Phase == CWPhaseWaiting && len(g.Players) >= CWMinPlayers
}

// ==================== 덱 ====================

// cwBuildDeck 40장 덱 — 4색 1~9 + 로켓 1~4
func cwBuildDeck() []CWCard {
	deck := make([]CWCard, 0, CWDeckSize)
	for _, suit := range cwColorSuits {
		for rank := 1; rank <= CWColorMaxRank; rank++ {
			deck = append(deck, CWCard{Suit: suit, Rank: rank})
		}
	}
	for rank := 1; rank <= CWRocketMaxRank; rank++ {
		deck = append(deck, CWCard{Suit: CWSuitRocket, Rank: rank})
	}
	return deck
}

// cwColorDeck 색 숫자 카드 36장 — 임무 후보 (로켓은 임무가 되지 않는다)
func cwColorDeck() []CWCard {
	deck := make([]CWCard, 0, 4*CWColorMaxRank)
	for _, suit := range cwColorSuits {
		for rank := 1; rank <= CWColorMaxRank; rank++ {
			deck = append(deck, CWCard{Suit: suit, Rank: rank})
		}
	}
	return deck
}

// cwSortHand 손패를 색·숫자 순으로 정렬한다 (로켓이 맨 뒤). 프론트의 색별
// 정렬과 인덱스 표기를 안정시킨다.
func cwSortHand(hand []CWCard) {
	sort.Slice(hand, func(i, j int) bool {
		if hand[i].Suit != hand[j].Suit {
			return cwSuitOrder[hand[i].Suit] < cwSuitOrder[hand[j].Suit]
		}
		return hand[i].Rank < hand[j].Rank
	})
}

// cwStrength 리드 무늬 기준 카드 세기. 로켓이 가장 세고, 그다음이 리드 색,
// 리드를 못 따른 카드는 승패에 관여하지 않는다.
func cwStrength(card CWCard, lead CWSuit) int {
	switch {
	case card.Suit == CWSuitRocket:
		return 200 + card.Rank
	case card.Suit == lead:
		return 100 + card.Rank
	default:
		return card.Rank
	}
}

// cwTrickWinner 트릭 승자 좌석 — 로켓이 있으면 로켓 최고, 없으면 리드 색 최고
func cwTrickWinner(trick []CWTrickCard, lead CWSuit) int {
	best, bestStrength := -1, -1
	for _, tc := range trick {
		if s := cwStrength(tc.Card, lead); s > bestStrength {
			best, bestStrength = tc.Seat, s
		}
	}
	return best
}

// cwHasSuit 손패에 그 무늬가 있는지
func cwHasSuit(hand []CWCard, suit CWSuit) bool {
	for _, c := range hand {
		if c.Suit == suit {
			return true
		}
	}
	return false
}

// cwPlayable 따라내기 의무 판정 — 리드 색이 있으면 반드시 그 색을 내야 한다
func cwPlayable(hand []CWCard, card CWCard, lead CWSuit) bool {
	if lead == "" || card.Suit == lead {
		return true
	}
	return !cwHasSuit(hand, lead)
}

// ==================== 시작 / 임무 배분 ====================

// Start 게임 시작 — 1임무를 배분한다
func (g *CWGame) Start(rng *rand.Rand) error {
	if !g.CanStart() {
		return fmt.Errorf("%d명 이상 모여야 시작할 수 있습니다", CWMinPlayers)
	}
	g.Ready = true
	g.StartedAt = time.Now()
	if g.MaxMission <= 0 {
		g.MaxMission = CWDefaultMaxMission
	}
	g.LastTrick = nil
	g.Result = nil
	g.EndReason = ""

	g.Mission = 1
	g.dealMission(rng)
	g.Phase = CWPhasePlaying
	g.StateSeq++
	g.emit("mission_start", g.CommanderSeat, fmt.Sprintf(
		"1번째 임무 — 임무 카드 %d장. 로켓 4를 쥔 %s님이 사령관으로 먼저 리드합니다",
		len(g.Tasks), g.Players[g.CommanderSeat].Name))
	return nil
}

// NextMission 임무 정산 대기가 끝나면 다음 임무를 배분한다 (허브 타이머)
func (g *CWGame) NextMission(rng *rand.Rand) {
	if g.Phase != CWPhaseRoundEnd {
		return
	}
	g.Mission++
	g.dealMission(rng)
	g.Phase = CWPhasePlaying
	g.StateSeq++
	g.emit("mission_start", g.CommanderSeat, fmt.Sprintf(
		"%d번째 임무 — 임무 카드 %d장. 로켓 4를 쥔 %s님이 사령관으로 먼저 리드합니다",
		g.Mission, len(g.Tasks), g.Players[g.CommanderSeat].Name))
}

// dealMission 40장을 다시 나누고 사령관·임무를 정한다.
// 나머지 1~2장은 앞자리 좌석부터 1장씩 더 간다.
func (g *CWGame) dealMission(rng *rand.Rand) {
	n := len(g.Players)
	deck := cwBuildDeck()
	rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })

	base, extra := len(deck)/n, len(deck)%n
	idx := 0
	for i, p := range g.Players {
		count := base
		if i < extra {
			count++
		}
		p.Hand = append([]CWCard{}, deck[idx:idx+count]...)
		cwSortHand(p.Hand)
		idx += count
		p.TokenLeft = CWTokenPerMission
		p.Revealed = nil
	}

	// 사령관 = 로켓 4 보유자 (덱 전량을 나누므로 반드시 한 명 나온다)
	g.CommanderSeat = 0
	for _, p := range g.Players {
		for _, c := range p.Hand {
			if c.Suit == CWSuitRocket && c.Rank == CWRocketMaxRank {
				g.CommanderSeat = p.Seat
			}
		}
	}

	g.Tasks = cwPickTasks(rng, g.Mission, n)
	g.LastTrick = nil
	g.Leader = g.CommanderSeat
	g.beginTrick()
}

// cwPickTasks 색 숫자 카드 중 count 장을 뽑아 좌석에 배정한다.
// 좌석은 섞은 순서로 돌아가며 배정해 한 사람에게 몰리지 않게 한다.
func cwPickTasks(rng *rand.Rand, count, seats int) []CWTask {
	pool := cwColorDeck()
	rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	if count > len(pool) {
		count = len(pool)
	}

	order := make([]int, seats)
	for i := range order {
		order[i] = i
	}
	rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })

	tasks := make([]CWTask, 0, count)
	for i := 0; i < count; i++ {
		tasks = append(tasks, CWTask{
			Suit: pool[i].Suit,
			Rank: pool[i].Rank,
			Seat: order[i%len(order)],
		})
	}
	return tasks
}

// beginTrick 새 트릭을 연다. 손패가 남은 좌석만 리드부터 차례로 참여한다
// (인원이 40장을 나누어 떨어뜨리지 못하는 3인전에서 마지막 트릭이 짧아진다).
func (g *CWGame) beginTrick() {
	n := len(g.Players)
	order := []int{}
	for i := 0; i < n; i++ {
		seat := (g.Leader + i) % n
		if len(g.Players[seat].Hand) > 0 {
			order = append(order, seat)
		}
	}
	g.trickOrder = order
	g.Trick = []CWTrickCard{}
	g.LeadSuit = ""
	if len(order) == 0 {
		g.CurrentSeat = -1
		return
	}
	g.CurrentSeat = order[0]
}

// ==================== 이벤트 큐 ====================

func (g *CWGame) emit(kind string, seat int, msg string) {
	g.events = append(g.events, CWGameEvent{Kind: kind, Seat: seat, Message: msg})
}

// DrainEvents 쌓인 이벤트를 꺼내고 비운다 (허브가 방송)
func (g *CWGame) DrainEvents() []CWGameEvent {
	evs := g.events
	g.events = nil
	return evs
}

// ==================== 카드 제출 ====================

// Play 자기 차례에 손패 index 의 카드를 낸다. 리드 색을 들고 있으면 반드시
// 그 색을 내야 한다.
func (g *CWGame) Play(seat, index int) error {
	if g.Phase != CWPhasePlaying {
		return errors.New("지금은 카드를 낼 수 없습니다")
	}
	if seat != g.CurrentSeat {
		return errors.New("아직 당신 차례가 아닙니다")
	}
	if seat < 0 || seat >= len(g.Players) {
		return errors.New("잘못된 좌석입니다")
	}
	p := g.Players[seat]
	if index < 0 || index >= len(p.Hand) {
		return errors.New("잘못된 카드입니다")
	}
	if !cwPlayable(p.Hand, p.Hand[index], g.LeadSuit) {
		return fmt.Errorf("%s 카드를 가지고 있어 반드시 따라내야 합니다", cwSuitLabel(g.LeadSuit))
	}
	g.playCard(seat, index)
	return nil
}

// ForcePlay 카드 마감 — 낼 수 있는 카드 중 무작위로 자동 제출한다 (허브 타이머).
// 소통은 자동으로 하지 않는다.
func (g *CWGame) ForcePlay(rng *rand.Rand) {
	if g.Phase != CWPhasePlaying {
		return
	}
	seat := g.CurrentSeat
	if seat < 0 || seat >= len(g.Players) {
		return
	}
	p := g.Players[seat]
	legal := []int{}
	for i, c := range p.Hand {
		if cwPlayable(p.Hand, c, g.LeadSuit) {
			legal = append(legal, i)
		}
	}
	if len(legal) == 0 { // 구조적으로 오지 않는다 (손패가 있으면 낼 수 있는 카드도 있다)
		return
	}
	g.playCard(seat, legal[rng.Intn(len(legal))])
}

// playCard 검증을 마친 제출 처리. 트릭이 다 차면 즉시 정산한다.
func (g *CWGame) playCard(seat, index int) {
	p := g.Players[seat]
	card := p.Hand[index]
	p.Hand = append(p.Hand[:index], p.Hand[index+1:]...)

	// 공개했던 카드를 냈다면 공개 표식을 거둔다 (손에 없는 카드를 계속
	// 보여주면 정보가 어긋난다)
	if p.Revealed != nil && p.Revealed.Card == card {
		p.Revealed = nil
	}

	if g.LeadSuit == "" {
		g.LeadSuit = card.Suit
	}
	g.Trick = append(g.Trick, CWTrickCard{Seat: seat, Card: card})
	g.emit("play", seat, fmt.Sprintf("%s님이 %s를 냈습니다", p.Name, cwCardLabel(card)))

	if len(g.Trick) >= len(g.trickOrder) {
		g.resolveTrick()
		return
	}
	g.CurrentSeat = g.trickOrder[len(g.Trick)]
	g.StateSeq++ // 새 차례 — 허브가 45초 마감을 다시 건다
}

// resolveTrick 트릭 정산 — 승자를 가리고 그 안의 임무 카드를 판정한다.
// 임무 카드를 담당자가 아닌 사람이 가져가면 즉시 실패다.
func (g *CWGame) resolveTrick() {
	winner := cwTrickWinner(g.Trick, g.LeadSuit)
	cards := append([]CWTrickCard{}, g.Trick...)
	g.LastTrick = &CWLastTrick{WinnerSeat: winner, Cards: cards}
	g.Trick = []CWTrickCard{}
	g.LeadSuit = ""
	g.trickOrder = nil

	winnerName := ""
	if winner >= 0 && winner < len(g.Players) {
		winnerName = g.Players[winner].Name
	}
	g.emit("trick_won", winner, fmt.Sprintf("%s님이 이번 트릭을 가져갔습니다", winnerName))

	for _, tc := range cards {
		for i := range g.Tasks {
			task := &g.Tasks[i]
			if task.Done || task.Suit != tc.Card.Suit || task.Rank != tc.Card.Rank {
				continue
			}
			owner := ""
			if task.Seat >= 0 && task.Seat < len(g.Players) {
				owner = g.Players[task.Seat].Name
			}
			if task.Seat == winner {
				task.Done = true
				g.emit("task_done", task.Seat, fmt.Sprintf(
					"임무 완료 — %s님이 %s를 가져갔습니다 (%d/%d)",
					owner, cwCardLabel(tc.Card), g.doneTasks(), len(g.Tasks)))
				continue
			}
			g.failMission("wrong_winner", fmt.Sprintf(
				"임무 실패 — %s는 %s님의 임무였는데 %s님이 가져갔습니다",
				cwCardLabel(tc.Card), owner, winnerName))
			return
		}
	}

	if g.doneTasks() >= len(g.Tasks) {
		g.completeMission()
		return
	}
	if g.handsEmpty() {
		g.failMission("out_of_cards", fmt.Sprintf(
			"임무 실패 — 카드가 모두 떨어졌는데 임무 %d개가 남았습니다",
			len(g.Tasks)-g.doneTasks()))
		return
	}

	g.Leader = winner
	g.beginTrick()
	g.StateSeq++ // 새 트릭의 첫 차례
}

// doneTasks 완료된 임무 수
func (g *CWGame) doneTasks() int {
	n := 0
	for _, t := range g.Tasks {
		if t.Done {
			n++
		}
	}
	return n
}

// handsEmpty 전 좌석의 손패가 비었는지
func (g *CWGame) handsEmpty() bool {
	for _, p := range g.Players {
		if len(p.Hand) > 0 {
			return false
		}
	}
	return true
}

// completeMission 이번 임무 성공 — 마지막 임무였으면 클리어, 아니면 정산 대기
func (g *CWGame) completeMission() {
	if g.Mission >= g.MaxMission {
		g.Result = &CWResult{
			Cleared: true,
			Mission: g.Mission,
			Message: fmt.Sprintf("임무 %d단계를 모두 완수했습니다 — 우주선이 무사히 귀환했습니다!", g.MaxMission),
		}
		g.EndReason = "cleared"
		g.Phase = CWPhaseGameOver
		g.CurrentSeat = -1
		g.StateSeq++
		return
	}
	g.Phase = CWPhaseRoundEnd
	g.CurrentSeat = -1
	g.StateSeq++
	g.emit("mission_clear", -1, fmt.Sprintf(
		"%d번째 임무 성공! 잠시 후 %d번째 임무(임무 카드 %d장)를 시작합니다",
		g.Mission, g.Mission+1, g.Mission+1))
}

// failMission 임무 실패 — 협력 게임이라 그대로 게임 종료다
func (g *CWGame) failMission(reason, msg string) {
	g.Result = &CWResult{
		Cleared:      false,
		FailedReason: reason,
		Mission:      g.Mission,
		Message:      msg,
	}
	g.EndReason = reason
	g.Phase = CWPhaseGameOver
	g.CurrentSeat = -1
	g.StateSeq++
	g.emit("mission_fail", -1, msg)
}

// ==================== 소통 ====================

// Communicate 손패의 색 숫자 카드 한 장을 공개하며 그 색 안에서의 위치를
// 선언한다. 서버가 다음을 전부 검증한다:
//
//	· 트릭 시작 시점(트릭에 카드가 하나도 없을 때)에만
//	· 임무마다 1회 (소통 토큰)
//	· 색 숫자 카드만 — 로켓 불가
//	· 선언이 실제로 참일 것 (highest/lowest 는 그 색을 2장 이상 들고 있을 때만,
//	  1장뿐이면 only 로만 알릴 수 있다)
//
// 공개한 카드는 손에 남고 전원이 계속 본다.
func (g *CWGame) Communicate(seat, index int, hint CWHint) error {
	if g.Phase != CWPhasePlaying {
		return errors.New("지금은 소통할 수 없습니다")
	}
	if len(g.Trick) != 0 {
		return errors.New("트릭이 시작된 뒤에는 소통할 수 없습니다")
	}
	if seat < 0 || seat >= len(g.Players) {
		return errors.New("잘못된 좌석입니다")
	}
	p := g.Players[seat]
	if p.TokenLeft <= 0 {
		return errors.New("이번 임무의 소통 토큰을 이미 썼습니다")
	}
	if index < 0 || index >= len(p.Hand) {
		return errors.New("잘못된 카드입니다")
	}
	card := p.Hand[index]
	if card.Suit == CWSuitRocket {
		return errors.New("로켓 카드는 공개할 수 없습니다")
	}

	count, highest, lowest := 0, 0, CWColorMaxRank+1
	for _, c := range p.Hand {
		if c.Suit != card.Suit {
			continue
		}
		count++
		if c.Rank > highest {
			highest = c.Rank
		}
		if c.Rank < lowest {
			lowest = c.Rank
		}
	}

	switch hint {
	case CWHintHighest:
		if count < 2 {
			return fmt.Errorf("%s 카드가 한 장뿐입니다 — '유일'로 알려야 합니다", cwSuitLabel(card.Suit))
		}
		if card.Rank != highest {
			return fmt.Errorf("%s 중 가장 높은 카드가 아닙니다", cwSuitLabel(card.Suit))
		}
	case CWHintLowest:
		if count < 2 {
			return fmt.Errorf("%s 카드가 한 장뿐입니다 — '유일'로 알려야 합니다", cwSuitLabel(card.Suit))
		}
		if card.Rank != lowest {
			return fmt.Errorf("%s 중 가장 낮은 카드가 아닙니다", cwSuitLabel(card.Suit))
		}
	case CWHintOnly:
		if count != 1 {
			return fmt.Errorf("%s 카드를 %d장 들고 있어 '유일'이 아닙니다", cwSuitLabel(card.Suit), count)
		}
	default:
		return errors.New("잘못된 선언입니다")
	}

	p.Revealed = &CWReveal{Card: card, Hint: hint}
	p.TokenLeft--
	g.emit("communicate", seat, fmt.Sprintf("%s님의 소통 — %s (그 색 중 %s)",
		p.Name, cwCardLabel(card), cwHintLabel(hint)))
	return nil
}
