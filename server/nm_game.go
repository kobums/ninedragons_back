package server

import (
	"errors"
	"math/rand"
	"sort"
	"time"
)

// ==================== 6 님트 순수 규칙 ====================
//
// 딜·동시 선택·일괄 공개·낮은 순 배치·행 먹기·벌점 계산·종료 판정만 다룬다.
// 클라이언트·타이머를 모르며, 허브(nm_hub.go)가 결과 구조체를 받아 이벤트로
// 번역한다. 배치 규칙:
//   - 자기보다 작은 행 끝 카드 중 가장 큰 행의 끝에 붙인다.
//   - 그 행의 6번째 카드가 되면 기존 5장을 벌점으로 먹고 자기 카드가 새 행.
//   - 모든 행 끝보다 작으면 행 하나를 선택해 먹고 새 행 시작 (선택 대기).

// nmBullHeads 카드 한 장의 소머리 수 — 기본 1, 5의 배수 2, 10의 배수 3,
// 11의 배수 5, 55(5×11)는 7. 분기 순서가 곧 우선순위다 (55 → 11 → 10 → 5).
func nmBullHeads(card int) int {
	switch {
	case card == 55:
		return 7
	case card%11 == 0:
		return 5
	case card%10 == 0:
		return 3
	case card%5 == 0:
		return 2
	default:
		return 1
	}
}

// nmRowHeads 행 전체의 소머리 합 (행 먹기 벌점·봇의 최소 행 판단 기준)
func nmRowHeads(row []int) int {
	sum := 0
	for _, c := range row {
		sum += nmBullHeads(c)
	}
	return sum
}

// NewNMGame 대기 상태의 새 게임
func NewNMGame(id string) *NMGame {
	return &NMGame{
		ID:          id,
		Players:     []*NMPlayer{},
		Phase:       NMPhaseWaiting,
		ChooserSeat: -1,
		WinnerSeats: []int{},
	}
}

// AddPlayer 대기실 입장. 좌석 번호를 돌려준다.
func (g *NMGame) AddPlayer(name string) (int, error) {
	if g.Phase != NMPhaseWaiting {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= NMMaxPlayers {
		return -1, errors.New("자리가 없습니다 (최대 10명)")
	}
	seat := len(g.Players)
	g.Players = append(g.Players, &NMPlayer{Seat: seat, Name: name, Hand: []int{}})
	return seat, nil
}

// RemovePlayer 대기실 퇴장. 좌석을 앞으로 당겨 빈틈을 없앤다.
func (g *NMGame) RemovePlayer(seat int) {
	if g.Phase != NMPhaseWaiting || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i, p := range g.Players {
		p.Seat = i
	}
}

// CanStart 시작 가능 여부 (호스트 명시 시작 — 2인부터)
func (g *NMGame) CanStart() bool {
	return g.Phase == NMPhaseWaiting && len(g.Players) >= NMMinPlayers
}

// Start 게임 시작 — 1~104를 섞어 각자 10장(오름차순 정렬), 4개 행에 시작
// 카드 1장씩 놓고 1트릭 동시 선택을 연다. 최대 10인 × 10장 + 4행 = 104장.
func (g *NMGame) Start(rng *rand.Rand) error {
	if !g.CanStart() {
		return errors.New("2명 이상 모여야 시작할 수 있습니다")
	}
	deck := make([]int, NMDeckSize)
	for i := range deck {
		deck[i] = i + 1
	}
	rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })

	for _, p := range g.Players {
		p.Hand = append([]int{}, deck[:NMHandSize]...)
		sort.Ints(p.Hand)
		deck = deck[NMHandSize:]
		p.Pick = 0
		p.Penalty = 0
	}
	g.Rows = make([][]int, NMRows)
	for r := 0; r < NMRows; r++ {
		g.Rows[r] = []int{deck[0]}
		deck = deck[1:]
	}

	g.Ready = true
	g.StartedAt = time.Now()
	g.Trick = 1
	g.Picks = nil
	g.Pending = nil
	g.ChooserSeat = -1
	g.LastPlacement = nil
	g.Phase = NMPhasePicking
	return nil
}

// ==================== 동시 선택 (picking) ====================

// SubmitPick 손패의 card 를 이번 트릭 제출로 확정한다. 제출 즉시 손에서
// 빠지며(본인 스냅샷의 yourHand 반영), 변경·중복 제출은 허용하지 않는다.
func (g *NMGame) SubmitPick(seat, card int) error {
	if g.Phase != NMPhasePicking {
		return errors.New("지금은 카드를 선택할 수 없습니다")
	}
	if seat < 0 || seat >= len(g.Players) {
		return errors.New("잘못된 좌석입니다")
	}
	p := g.Players[seat]
	if p.Pick != 0 {
		return errors.New("이미 카드를 선택했습니다")
	}
	idx := -1
	for i, c := range p.Hand {
		if c == card {
			idx = i
			break
		}
	}
	if idx < 0 {
		return errors.New("손에 없는 카드입니다")
	}
	p.Hand = append(p.Hand[:idx], p.Hand[idx+1:]...)
	p.Pick = card
	return nil
}

// AllPicked 전원 제출 완료 여부 (reveal 전환 신호)
func (g *NMGame) AllPicked() bool {
	for _, p := range g.Players {
		if p.Pick == 0 {
			return false
		}
	}
	return true
}

// AutoPickAll 동시 선택 마감(AFK) — 미제출 전원 무작위 카드 제출.
// 돌려주는 값은 자동 제출된 좌석들 (허브가 이벤트로 발표).
func (g *NMGame) AutoPickAll(rng *rand.Rand) []int {
	seats := []int{}
	for _, p := range g.Players {
		if p.Pick != 0 || len(p.Hand) == 0 {
			continue
		}
		if g.SubmitPick(p.Seat, p.Hand[rng.Intn(len(p.Hand))]) == nil {
			seats = append(seats, p.Seat)
		}
	}
	return seats
}

// ==================== 일괄 공개 · 배치 (revealing) ====================

// StartReveal 전원 제출 확정 — 제출을 카드 오름차순으로 공개하고 배치
// 대기열(Pending)을 만든다. 배치는 허브가 PlaceNext 로 하나씩 진행한다.
func (g *NMGame) StartReveal() {
	if g.Phase != NMPhasePicking {
		return
	}
	g.Picks = []NMPickEntry{}
	for _, p := range g.Players {
		g.Picks = append(g.Picks, NMPickEntry{Seat: p.Seat, Card: p.Pick})
	}
	sort.Slice(g.Picks, func(i, j int) bool { return g.Picks[i].Card < g.Picks[j].Card })
	g.Pending = append([]NMPickEntry{}, g.Picks...)
	g.LastPlacement = nil
	g.ChooserSeat = -1
	g.Phase = NMPhaseRevealing
}

// targetRow card 가 붙을 행 — 끝 카드가 card 보다 작은 행 중 끝 카드가
// 가장 큰 행. 없으면 -1 (모든 행 끝보다 작다 — 행 선택 필요).
func (g *NMGame) targetRow(card int) int {
	best, bestEnd := -1, -1
	for r, row := range g.Rows {
		end := row[len(row)-1]
		if end < card && end > bestEnd {
			best, bestEnd = r, end
		}
	}
	return best
}

// PlaceNext 배치 대기열 맨 앞(최소 카드)을 하나 배치한다.
//   - (placement, false): 배치됨 — 6번째면 행을 먹고 새 행 (Ate=true)
//   - (nil, true): 모든 행 끝보다 작다 — choosing_row 로 전환, ChooseRow 대기
//   - (nil, false): 대기열 소진 — 트릭 마무리 차례
func (g *NMGame) PlaceNext() (*NMPlacement, bool) {
	if g.Phase != NMPhaseRevealing || len(g.Pending) == 0 {
		return nil, false
	}
	entry := g.Pending[0]
	row := g.targetRow(entry.Card)
	if row < 0 {
		g.ChooserSeat = entry.Seat
		g.Phase = NMPhaseChoosingRow
		return nil, true
	}
	g.Pending = g.Pending[1:]
	ate := false
	if len(g.Rows[row]) >= NMRowCapacity {
		// 6번째 카드 — 기존 5장을 벌점으로 먹고 자기 카드가 새 행
		g.Players[entry.Seat].Penalty += nmRowHeads(g.Rows[row])
		g.Rows[row] = []int{entry.Card}
		ate = true
	} else {
		g.Rows[row] = append(g.Rows[row], entry.Card)
	}
	g.LastPlacement = &NMPlacement{Seat: entry.Seat, Card: entry.Card, Row: row, Ate: ate}
	return g.LastPlacement, false
}

// ChooseRow 최소 카드의 행 선택 — 선택한 행을 벌점으로 먹고 자기 카드가
// 새 행이 된다. 처리 후 revealing 으로 복귀해 남은 배치를 이어간다.
func (g *NMGame) ChooseRow(seat, row int) (*NMPlacement, error) {
	if g.Phase != NMPhaseChoosingRow {
		return nil, errors.New("지금은 행을 선택할 수 없습니다")
	}
	if seat != g.ChooserSeat {
		return nil, errors.New("행을 선택할 차례가 아닙니다")
	}
	if row < 0 || row >= NMRows {
		return nil, errors.New("잘못된 행입니다 (0~3)")
	}
	entry := g.Pending[0]
	g.Pending = g.Pending[1:]
	g.Players[seat].Penalty += nmRowHeads(g.Rows[row])
	g.Rows[row] = []int{entry.Card}
	g.LastPlacement = &NMPlacement{Seat: seat, Card: entry.Card, Row: row, Ate: true}
	g.ChooserSeat = -1
	g.Phase = NMPhaseRevealing
	return g.LastPlacement, nil
}

// MinHeadsRow 소머리 합 최소 행 (동률은 낮은 인덱스) — 봇·AFK 자동 선택용
func (g *NMGame) MinHeadsRow() int {
	best, bestHeads := 0, 1<<30
	for r, row := range g.Rows {
		if h := nmRowHeads(row); h < bestHeads {
			best, bestHeads = r, h
		}
	}
	return best
}

// FinishTrick 배치 대기열 소진 후 트릭 마무리 — 10트릭이면 종료 판정
// (소머리 최소 승, 동점 공동). true 를 돌려주면 game_over 다.
func (g *NMGame) FinishTrick() bool {
	for _, p := range g.Players {
		p.Pick = 0
	}
	g.Picks = nil
	g.Pending = nil
	if g.Trick >= NMTricks {
		g.finish()
		return true
	}
	g.Trick++
	g.LastPlacement = nil
	g.Phase = NMPhasePicking
	return false
}

// finish 소머리 합 최소가 승리 (동점 공동 승)
func (g *NMGame) finish() {
	best := 1 << 30
	for _, p := range g.Players {
		if p.Penalty < best {
			best = p.Penalty
		}
	}
	g.WinnerSeats = []int{}
	for _, p := range g.Players {
		if p.Penalty == best {
			g.WinnerSeats = append(g.WinnerSeats, p.Seat)
		}
	}
	g.ChooserSeat = -1
	g.Phase = NMPhaseGameOver
}
