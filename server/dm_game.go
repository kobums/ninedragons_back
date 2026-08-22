package server

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// ==================== 달무티 순수 규칙 ====================
//
// 배분·세트 판정·클라이밍·패스 순환·핸드 정산만 다룬다. 클라이언트·타이머를
// 모르며, 허브(dm_hub.go)가 차례 마감을 걸고 이벤트 큐(DrainEvents)를 방송한다.
//
// 진행 모델: 핸드마다 덱(80장)을 균등 배분(나머지 제거) → 리드가 같은 숫자
// N장 세트 제출 → 이후는 같은 장수의 더 낮은 숫자만 (1이 최강). 전원 연속
// 패스면 마지막 제출자가 새 리드 (그가 손을 털었으면 다음 생존 좌석).
// 손을 턴 순서가 순위, 점수는 1등 = 인원-1점 … 꼴찌 0점. 3핸드 총점 최고 승.

// NewDMGame 대기 상태의 새 게임
func NewDMGame(id string) *DMGame {
	return &DMGame{
		ID:          id,
		Players:     []*DMPlayer{},
		Phase:       DMPhaseWaiting,
		CurrentSeat: -1,
		LeadSeat:    -1,
		WinnerSeats: []int{},
	}
}

// AddPlayer 대기실 입장. 좌석 번호를 돌려준다.
func (g *DMGame) AddPlayer(name string) (int, error) {
	if g.Phase != DMPhaseWaiting {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= DMMaxPlayers {
		return -1, fmt.Errorf("자리가 없습니다 (최대 %d명)", DMMaxPlayers)
	}
	seat := len(g.Players)
	g.Players = append(g.Players, &DMPlayer{Seat: seat, Name: name, Hand: []int{}})
	return seat, nil
}

// RemovePlayer 대기실 퇴장. 좌석을 앞으로 당겨 빈틈을 없앤다.
func (g *DMGame) RemovePlayer(seat int) {
	if g.Phase != DMPhaseWaiting || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i, p := range g.Players {
		p.Seat = i
	}
}

// CanStart 시작 가능 여부 (호스트 명시 시작 — 4인부터)
func (g *DMGame) CanStart() bool {
	return g.Phase == DMPhaseWaiting && len(g.Players) >= DMMinPlayers
}

// Start 게임 시작 — 첫 핸드의 리드는 무작위, 이후는 직전 핸드 1등
func (g *DMGame) Start(rng *rand.Rand) error {
	if !g.CanStart() {
		return fmt.Errorf("%d명 이상 모여야 시작할 수 있습니다", DMMinPlayers)
	}
	g.Ready = true
	g.StartedAt = time.Now()
	for _, p := range g.Players {
		p.Points = 0
	}
	g.HandNo = 1
	g.startHand(rng, rng.Intn(len(g.Players)))
	return nil
}

// ==================== 이벤트 큐 ====================

func (g *DMGame) emit(kind string, seat int, msg string) {
	g.events = append(g.events, DMGameEvent{Kind: kind, Seat: seat, Message: msg})
}

// DrainEvents 쌓인 이벤트를 꺼내고 비운다 (허브가 방송)
func (g *DMGame) DrainEvents() []DMGameEvent {
	evs := g.events
	g.events = nil
	return evs
}

// ==================== 덱 / 배분 ====================

// dmBuildDeck 달무티 덱 — 숫자 n 이 n 장(1~12) 78장 + 조커(13) 2장 = 80장
func dmBuildDeck() []int {
	deck := []int{}
	for r := 1; r <= DMMaxRank; r++ {
		for i := 0; i < r; i++ {
			deck = append(deck, r)
		}
	}
	return append(deck, DMJoker, DMJoker)
}

// startHand 새 핸드 — 셔플한 덱을 균등 배분(나머지 제거)하고 리드를 세운다
func (g *DMGame) startHand(rng *rand.Rand, lead int) {
	deck := dmBuildDeck()
	rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })

	per := len(deck) / len(g.Players)
	for i, p := range g.Players {
		hand := append([]int{}, deck[i*per:(i+1)*per]...)
		sort.Ints(hand)
		p.Hand = hand
		p.OutRank = 0
	}
	g.OutCount = 0
	g.Table = nil
	g.HandResult = nil
	g.LeadSeat = lead
	g.CurrentSeat = lead
	g.Phase = DMPhasePlaying
	g.StateSeq++
	g.emit("hand_start", lead, fmt.Sprintf("%d번째 핸드 시작 — 각 %d장, %s님이 리드입니다",
		g.HandNo, per, g.Players[lead].Name))
}

// ==================== 좌석 헬퍼 ====================

// activeCount 아직 손을 털지 않은 인원 수
func (g *DMGame) activeCount() int {
	n := 0
	for _, p := range g.Players {
		if p.OutRank == 0 {
			n++
		}
	}
	return n
}

// nextActiveAfter seat 다음의 미확정(손패 남은) 좌석 (시계 방향)
func (g *DMGame) nextActiveAfter(seat int) int {
	n := len(g.Players)
	for i := 1; i <= n; i++ {
		s := (seat + i) % n
		if g.Players[s].OutRank == 0 {
			return s
		}
	}
	return seat
}

// ==================== 세트 판정 (조커 와일드) ====================

// dmParseSet 제출 카드들을 세트로 판정한다. 조커(13)를 제외한 카드는 전부
// 같은 숫자여야 하고, 조커가 섞이면 그 숫자로 취급된다. 조커만으로 낸 세트는
// 13(가장 약한 숫자) 취급이다.
func dmParseSet(cards []int) (rank, count int, err error) {
	if len(cards) == 0 {
		return 0, 0, errors.New("낼 카드를 선택하세요")
	}
	rank = 0
	for _, c := range cards {
		if c < 1 || c > DMJoker {
			return 0, 0, errors.New("잘못된 카드입니다")
		}
		if c == DMJoker {
			continue
		}
		if rank == 0 {
			rank = c
		} else if rank != c {
			return 0, 0, errors.New("같은 숫자의 카드만 함께 낼 수 있습니다 (조커는 와일드)")
		}
	}
	if rank == 0 {
		rank = DMJoker // 조커 단독 — 13 취급
	}
	return rank, len(cards), nil
}

// dmRankCounts 손패의 랭크별 장수
func dmRankCounts(hand []int) map[int]int {
	counts := map[int]int{}
	for _, c := range hand {
		counts[c]++
	}
	return counts
}

// dmHandContains 손패가 cards 를 다중집합으로 포함하는지
func dmHandContains(hand, cards []int) bool {
	need := dmRankCounts(cards)
	have := dmRankCounts(hand)
	for r, n := range need {
		if have[r] < n {
			return false
		}
	}
	return true
}

// dmRemoveCards 손패에서 cards 를 다중집합으로 제거 (정렬 유지)
func dmRemoveCards(hand, cards []int) []int {
	need := dmRankCounts(cards)
	out := []int{}
	for _, c := range hand {
		if need[c] > 0 {
			need[c]--
			continue
		}
		out = append(out, c)
	}
	return out
}

// dmRepeat 랭크 r 을 n 장 나열
func dmRepeat(r, n int) []int {
	cards := []int{}
	for i := 0; i < n; i++ {
		cards = append(cards, r)
	}
	return cards
}

// ==================== 플레이 / 패스 ====================

// Play seat 가 cards 세트를 낸다. 리드는 아무 세트, 팔로우는 같은 장수의
// 더 낮은 숫자만 (낮을수록 강함). 손을 다 털면 순위가 확정된다.
func (g *DMGame) Play(seat int, cards []int) error {
	if g.Phase != DMPhasePlaying {
		return errors.New("지금은 카드를 낼 수 없습니다")
	}
	if seat != g.CurrentSeat {
		return errors.New("당신의 차례가 아닙니다")
	}
	p := g.Players[seat]
	rank, count, err := dmParseSet(cards)
	if err != nil {
		return err
	}
	if !dmHandContains(p.Hand, cards) {
		return errors.New("손에 없는 카드입니다")
	}
	if g.Table != nil {
		if count != g.Table.Count {
			return fmt.Errorf("같은 장수(%d장)의 세트만 낼 수 있습니다", g.Table.Count)
		}
		if rank >= g.Table.Rank {
			return fmt.Errorf("%d보다 낮은 숫자만 낼 수 있습니다 (낮을수록 강함)", g.Table.Rank)
		}
	}

	p.Hand = dmRemoveCards(p.Hand, cards)
	g.Table = &DMTableSet{Rank: rank, Count: count, Seat: seat}

	jokers := 0
	for _, c := range cards {
		if c == DMJoker {
			jokers++
		}
	}
	msg := fmt.Sprintf("%s님이 %d를 %d장 냈습니다", p.Name, rank, count)
	if rank == DMJoker {
		msg = fmt.Sprintf("%s님이 조커를 %d장 냈습니다 (13 취급)", p.Name, count)
	} else if jokers > 0 {
		msg += fmt.Sprintf(" (조커 %d장 포함)", jokers)
	}
	g.emit("played", seat, msg)

	if len(p.Hand) == 0 {
		g.OutCount++
		p.OutRank = g.OutCount
		g.emit("out", seat, fmt.Sprintf("%s님이 %d등으로 손을 모두 털었습니다!", p.Name, p.OutRank))
	}
	if g.activeCount() <= 1 {
		g.finishHand()
		return nil
	}
	g.CurrentSeat = g.nextActiveAfter(seat)
	g.StateSeq++
	return nil
}

// Pass 차례를 넘긴다. 리드(테이블이 빈 상태)는 패스할 수 없다.
// 차례가 마지막 제출자에게 되돌아오면(전원 연속 패스) 그가 새 리드를 잡는다
// — 제출자가 이미 손을 털었으면 다음 생존 좌석이 리드.
func (g *DMGame) Pass(seat int) error {
	if g.Phase != DMPhasePlaying {
		return errors.New("지금은 패스할 수 없습니다")
	}
	if seat != g.CurrentSeat {
		return errors.New("당신의 차례가 아닙니다")
	}
	if g.Table == nil {
		return errors.New("리드는 패스할 수 없습니다 — 세트를 내세요")
	}
	g.emit("pass", seat, fmt.Sprintf("%s님이 패스했습니다", g.Players[seat].Name))

	// 다음 응답자: 마지막 제출자에게 닿으면 트릭 종료 (손 턴 좌석은 건너뜀)
	n := len(g.Players)
	next := -1
	for i := 1; i <= n; i++ {
		s := (seat + i) % n
		if s == g.Table.Seat || g.Players[s].OutRank == 0 {
			next = s
			break
		}
	}
	if next != g.Table.Seat {
		g.CurrentSeat = next
		g.StateSeq++
		return nil
	}

	lead := g.Table.Seat
	if g.Players[lead].OutRank > 0 {
		lead = g.nextActiveAfter(lead)
	}
	g.emit("trick_won", lead, fmt.Sprintf("전원 패스 — %s님이 새 리드를 잡습니다", g.Players[lead].Name))
	g.Table = nil
	g.LeadSeat = lead
	g.CurrentSeat = lead
	g.StateSeq++
	return nil
}

// ==================== AFK / 봇 공용 선택 ====================

// AutoPlay AFK 자동 진행용 선택 — 리드면 최다 장수 세트(동수면 높은 숫자,
// 약한 것부터 소진), 팔로우면 유효한 것 중 가장 높은(약한) 숫자 세트.
// 낼 수 없으면 nil(패스). 상태를 바꾸지 않는다.
func (g *DMGame) AutoPlay(seat int) []int {
	p := g.Players[seat]
	if g.Table == nil {
		return dmLeadChoice(p.Hand)
	}
	return dmFollowChoice(p.Hand, g.Table.Rank, g.Table.Count)
}

// dmLeadChoice 리드 선택 — 최다 장수 세트, 동수면 가장 높은(약한) 숫자.
// 조커도 자기 랭크(13)의 세트로만 센다 (와일드 합성은 리드에 쓰지 않는다).
func dmLeadChoice(hand []int) []int {
	counts := dmRankCounts(hand)
	bestRank, bestCount := 0, 0
	for r := 1; r <= DMJoker; r++ {
		if c := counts[r]; c > 0 && c >= bestCount {
			bestRank, bestCount = r, c
		}
	}
	if bestCount == 0 {
		return nil
	}
	return dmRepeat(bestRank, bestCount)
}

// dmFollowChoice 팔로우 선택 — 테이블보다 낮은 숫자 중 가장 높은(약한) 세트.
// 조커 없이 완성되는 세트를 우선하고, 부족할 때만 조커로 장수를 채운다
// (조커는 세트 완성에만). 유효한 수가 없으면 nil.
func dmFollowChoice(hand []int, tableRank, tableCount int) []int {
	counts := dmRankCounts(hand)
	jokers := counts[DMJoker]
	maxR := tableRank - 1
	if maxR > DMMaxRank {
		maxR = DMMaxRank
	}
	for r := maxR; r >= 1; r-- {
		if counts[r] >= tableCount {
			return dmRepeat(r, tableCount)
		}
	}
	for r := maxR; r >= 1; r-- {
		if counts[r] >= 1 && counts[r]+jokers >= tableCount {
			cards := dmRepeat(r, counts[r]) // 첫 루프에서 걸러져 counts[r] < tableCount
			for len(cards) < tableCount {
				cards = append(cards, DMJoker)
			}
			return cards
		}
	}
	return nil
}

// ==================== 핸드 정산 / 진행 ====================

// finishHand 핸드 정산 — 마지막 생존자에게 꼴찌 순위를 주고 순위 점수를
// 누적한다 (1등 = 인원-1점 … 꼴찌 0점). 다음 핸드 리드는 이번 핸드 1등.
func (g *DMGame) finishHand() {
	n := len(g.Players)
	for _, p := range g.Players {
		if p.OutRank == 0 {
			g.OutCount++
			p.OutRank = g.OutCount
			g.emit("out", p.Seat, fmt.Sprintf("%s님이 마지막 %d등입니다", p.Name, p.OutRank))
		}
	}

	order := make([]int, n)
	for _, p := range g.Players {
		p.Points += n - p.OutRank
		order[p.OutRank-1] = p.Seat
	}
	names := []string{}
	for _, s := range order {
		names = append(names, g.Players[s].Name)
	}
	msg := fmt.Sprintf("%d번째 핸드 종료 — 순위: %s", g.HandNo, strings.Join(names, " → "))
	g.HandResult = &DMHandResult{Order: append([]int{}, order...), Message: msg}
	g.NextLeadSeat = order[0]
	g.Table = nil
	g.CurrentSeat = -1
	g.Phase = DMPhaseHandEnd
	g.StateSeq++
	g.emit("hand_end", order[0], msg)
}

// AdvanceHand hand_end 마감 — 다음 핸드를 열거나(직전 1등 리드) 3핸드를 다
// 쳤으면 총점 최고(동점 공동)를 확정하고 게임을 끝낸다.
func (g *DMGame) AdvanceHand(rng *rand.Rand) {
	if g.Phase != DMPhaseHandEnd {
		return
	}
	if g.HandNo < DMHands {
		g.HandNo++
		g.startHand(rng, g.NextLeadSeat)
		return
	}

	best := -1
	for _, p := range g.Players {
		if p.Points > best {
			best = p.Points
		}
	}
	g.WinnerSeats = []int{}
	for _, p := range g.Players {
		if p.Points == best {
			g.WinnerSeats = append(g.WinnerSeats, p.Seat)
		}
	}
	g.CurrentSeat = -1
	g.LeadSeat = -1
	g.Phase = DMPhaseGameOver
}
