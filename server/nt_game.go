package server

import (
	"errors"
	"math/rand"
	"sort"
	"time"
)

// ==================== 노 땡스! 순수 규칙 ====================
//
// 덱 구성(9장 비공개 제거)·패스/가져가기·시퀀스 점수·최저점 승리만 다룬다.
// 클라이언트·타이머를 모르며, 허브(nt_hub.go)가 결과 구조체를 받아
// 이벤트로 번역한다. 탈락이 없어 전원이 끝까지 참여한다.

// NewNTGame 대기 상태의 새 게임
func NewNTGame(id string) *NTGame {
	return &NTGame{
		ID:          id,
		Players:     []*NTPlayer{},
		Phase:       NTPhaseWaiting,
		Deck:        []int{},
		CurrentSeat: -1,
		FirstSeat:   -1,
		WinnerSeats: []int{},
	}
}

// AddPlayer 대기실 입장. 좌석 번호를 돌려준다.
func (g *NTGame) AddPlayer(name string) (int, error) {
	if g.Phase != NTPhaseWaiting {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= NTMaxPlayers {
		return -1, errors.New("자리가 없습니다 (최대 7명)")
	}
	seat := len(g.Players)
	g.Players = append(g.Players, &NTPlayer{
		Seat: seat, Name: name, Chips: NTStartChips, Cards: []int{},
	})
	return seat, nil
}

// RemovePlayer 대기실 퇴장. 좌석을 앞으로 당겨 빈틈을 없앤다.
func (g *NTGame) RemovePlayer(seat int) {
	if g.Phase != NTPhaseWaiting || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i, p := range g.Players {
		p.Seat = i
	}
}

// CanStart 시작 가능 여부 (호스트 명시 시작 — 3인부터)
func (g *NTGame) CanStart() bool {
	return g.Phase == NTPhaseWaiting && len(g.Players) >= NTMinPlayers
}

// Start 게임 시작 — 3~35 카드 33장을 섞어 9장을 비공개로 제거한 24장 덱을
// 만들고, 첫 카드를 공개한 뒤 무작위 선부터 차례를 연다.
func (g *NTGame) Start(rng *rand.Rand) error {
	if !g.CanStart() {
		return errors.New("3명 이상 모여야 시작할 수 있습니다")
	}
	g.Ready = true
	g.StartedAt = time.Now()

	pool := make([]int, 0, NTCardMax-NTCardMin+1)
	for v := NTCardMin; v <= NTCardMax; v++ {
		pool = append(pool, v)
	}
	rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	// 섞인 33장의 앞 24장만 쓴다 — 무작위 9장 비공개 제거와 동치
	g.Deck = append([]int{}, pool[:NTDeckSize]...)

	g.Card = g.Deck[0]
	g.Deck = g.Deck[1:]
	g.PotChips = 0
	g.FirstSeat = rng.Intn(len(g.Players))
	g.CurrentSeat = g.FirstSeat
	g.Phase = NTPhasePlaying
	return nil
}

// NTActionResult 행동 한 번의 판정 결과 (허브가 이벤트로 번역)
type NTActionResult struct {
	Seat        int
	Kind        string // "pass" | "take"
	Card        int    // 행동 대상 공개 카드
	GainedChips int    // 가져가기로 얻은 얹힌 칩 (패스는 0)

	GameEnded bool // 덱 소진 — 게임 종료 (점수·승자 확정됨)
}

// actor 행동의 공통 검증 — playing 단계의 현재 차례만 허용한다
func (g *NTGame) actor(seat int) (*NTPlayer, error) {
	if g.Phase != NTPhasePlaying {
		return nil, errors.New("지금은 행동할 수 없습니다")
	}
	if seat != g.CurrentSeat {
		return nil, errors.New("당신의 차례가 아닙니다")
	}
	return g.Players[seat], nil
}

// Pass 칩 1개를 공개 카드 위에 얹고 다음 차례로 넘긴다 (칩 0이면 불가)
func (g *NTGame) Pass(seat int) (*NTActionResult, error) {
	p, err := g.actor(seat)
	if err != nil {
		return nil, err
	}
	if p.Chips <= 0 {
		return nil, errors.New("칩이 없어 패스할 수 없습니다 — 카드를 가져가야 합니다")
	}
	p.Chips--
	g.PotChips++
	g.CurrentSeat = (seat + 1) % len(g.Players)
	return &NTActionResult{Seat: seat, Kind: "pass", Card: g.Card}, nil
}

// Take 공개 카드와 얹힌 칩을 전부 가져간다. 새 카드를 공개하고 가져간
// 사람부터 다시 시작한다. 덱이 비었으면 게임 종료 — 점수·승자를 확정한다.
func (g *NTGame) Take(seat int) (*NTActionResult, error) {
	p, err := g.actor(seat)
	if err != nil {
		return nil, err
	}

	card := g.Card
	gained := g.PotChips
	p.Cards = append(p.Cards, card)
	sort.Ints(p.Cards)
	p.Chips += gained
	g.PotChips = 0

	res := &NTActionResult{Seat: seat, Kind: "take", Card: card, GainedChips: gained}
	if len(g.Deck) == 0 {
		g.finish()
		res.GameEnded = true
		return res, nil
	}
	g.Card = g.Deck[0]
	g.Deck = g.Deck[1:]
	g.CurrentSeat = seat // 가져간 사람부터 다시 시작
	return res, nil
}

// ntScore 노 땡스! 점수 — 카드 합(연속 시퀀스는 최솟값만 계산) − 칩.
// cards 는 오름차순 정렬을 전제한다. 최저점 승리이므로 낮을수록 좋다.
func ntScore(cards []int, chips int) int {
	sum := 0
	for i, c := range cards {
		if i == 0 || c != cards[i-1]+1 {
			sum += c // 시퀀스의 시작(최솟값)만 더한다
		}
	}
	return sum - chips
}

// finish 덱 소진 — 전원 점수 확정·최저점 승자 판정 (동점 공동 승)
func (g *NTGame) finish() {
	best := 0
	for i, p := range g.Players {
		p.Score = ntScore(p.Cards, p.Chips)
		if i == 0 || p.Score < best {
			best = p.Score
		}
	}
	g.WinnerSeats = []int{}
	for _, p := range g.Players {
		if p.Score == best {
			g.WinnerSeats = append(g.WinnerSeats, p.Seat)
		}
	}
	g.EndReason = "deck_empty"
	g.Card = 0
	g.CurrentSeat = -1
	g.Phase = NTPhaseGameOver
}
