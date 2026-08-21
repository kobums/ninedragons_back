package server

import (
	"errors"
	"fmt"
	"math/rand"
	"time"
)

// ==================== 바퀴벌레 포커 순수 규칙 ====================
//
// 배분·전달·릴레이·판정·패배 조건만 다룬다. 클라이언트·타이머를 모르며,
// 허브(cr_hub.go)가 마감 타이머를 걸고 이벤트 큐(DrainEvents)를 방송한다.
//
// 진행 모델: passing(전달자가 카드·대상·선언 선택) → deciding(결정권자가
// 판정 또는 넘기기) → [넘기기: 다시 deciding] → 판정으로 카드가 누군가의
// 진열에 공개되며 그 사람이 다음 전달자. 같은 동물 4장 진열 또는 전달
// 차례의 손패 0장이면 즉시 게임 종료(그 사람 패배, 나머지 전원 승리).

// crDeckComposition 8종 × 8 = 64장
func crDeckComposition() []CRAnimal {
	deck := make([]CRAnimal, 0, len(crAllAnimals)*CRCopiesPerAnimal)
	for _, a := range crAllAnimals {
		for i := 0; i < CRCopiesPerAnimal; i++ {
			deck = append(deck, a)
		}
	}
	return deck
}

// NewCRGame 대기 상태의 새 게임
func NewCRGame(id string) *CRGame {
	return &CRGame{
		ID:         id,
		Players:    []*CRPlayer{},
		Phase:      CRPhaseWaiting,
		PasserSeat: -1,
		HolderSeat: -1,
		LoserSeat:  -1,
	}
}

// AddPlayer 대기실 입장. 좌석 번호를 돌려준다.
func (g *CRGame) AddPlayer(name string) (int, error) {
	if g.Phase != CRPhaseWaiting {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= CRMaxPlayers {
		return -1, fmt.Errorf("자리가 없습니다 (최대 %d명)", CRMaxPlayers)
	}
	seat := len(g.Players)
	g.Players = append(g.Players, &CRPlayer{Seat: seat, Name: name})
	return seat, nil
}

// RemovePlayer 대기실 퇴장. 좌석을 앞으로 당겨 빈틈을 없앤다.
func (g *CRGame) RemovePlayer(seat int) {
	if g.Phase != CRPhaseWaiting || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i, p := range g.Players {
		p.Seat = i
	}
}

// CanStart 시작 가능 여부 (호스트 명시 시작 — 3인부터)
func (g *CRGame) CanStart() bool {
	return g.Phase == CRPhaseWaiting && len(g.Players) >= CRMinPlayers
}

// Start 게임 시작 — 64장을 섞어 인원수로 나눠 전부 배분한다 (나머지 제거).
// 첫 전달자는 무작위.
func (g *CRGame) Start(rng *rand.Rand) error {
	if !g.CanStart() {
		return fmt.Errorf("%d명 이상 모여야 시작할 수 있습니다", CRMinPlayers)
	}
	g.Ready = true
	g.StartedAt = time.Now()

	deck := crDeckComposition()
	rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })
	per := len(deck) / len(g.Players)
	for _, p := range g.Players {
		p.Hand = append([]CRAnimal{}, deck[:per]...)
		deck = deck[per:]
		p.Display = map[CRAnimal]int{}
	}
	g.PasserSeat = rng.Intn(len(g.Players))
	g.HolderSeat = -1
	g.Phase = CRPhasePassing
	g.StateSeq++
	return nil
}

// ==================== 이벤트 큐 ====================

func (g *CRGame) emit(kind string, seat int, msg string) {
	g.events = append(g.events, CRGameEvent{Kind: kind, Seat: seat, Message: msg})
}

// DrainEvents 쌓인 이벤트를 꺼내고 비운다 (허브가 방송)
func (g *CRGame) DrainEvents() []CRGameEvent {
	evs := g.events
	g.events = nil
	return evs
}

// ==================== 릴레이 헬퍼 ====================

// inChain seat 이 이 카드를 이미 확인하고 넘긴 좌석인지
func (g *CRGame) inChain(seat int) bool {
	for _, s := range g.Chain {
		if s == seat {
			return true
		}
	}
	return false
}

// CanRelay seat(결정권자)이 넘길 수 있는지 — 카드를 아직 안 본 다른 사람이
// 남아 있어야 한다. 마지막 남은 사람(체인에 안 낀 유일한 사람)은 넘기기
// 불가 — 강제 판정.
func (g *CRGame) CanRelay(seat int) bool {
	for _, p := range g.Players {
		if p.Seat != seat && !g.inChain(p.Seat) {
			return true
		}
	}
	return false
}

// removeFromHand 손패에서 해당 동물 1장 제거. 없으면 false.
func (p *CRPlayer) removeFromHand(a CRAnimal) bool {
	for i, c := range p.Hand {
		if c == a {
			p.Hand = append(p.Hand[:i], p.Hand[i+1:]...)
			return true
		}
	}
	return false
}

// ==================== 전달 / 넘기기 / 판정 ====================

// PassCard 전달자의 카드 전달 — 손에서 1장 뒤집어 대상 지정 + 동물 선언
// (거짓 가능). 카드가 릴레이에 실리고 대상이 결정권자가 된다.
func (g *CRGame) PassCard(seat int, card CRAnimal, target int, claim CRAnimal) error {
	if g.Phase != CRPhasePassing {
		return errors.New("지금은 카드를 전달할 수 없습니다")
	}
	if seat != g.PasserSeat {
		return errors.New("당신의 전달 차례가 아닙니다")
	}
	if !crValidAnimal(card) || !crValidAnimal(claim) {
		return errors.New("알 수 없는 동물입니다")
	}
	if target < 0 || target >= len(g.Players) || target == seat {
		return errors.New("대상을 올바르게 선택하세요")
	}
	passer := g.Players[seat]
	if !passer.removeFromHand(card) {
		return errors.New("손패에 없는 카드입니다")
	}

	g.Card = card
	g.Claim = claim
	g.Chain = []int{seat}
	g.HolderSeat = target
	g.Phase = CRPhaseDeciding
	g.StateSeq++

	g.emit("pass", seat, fmt.Sprintf("%s님이 %s님에게 카드를 내밀며 \"이건 %s다!\" — 판정하거나 몰래 보고 넘기세요",
		passer.Name, g.Players[target].Name, crAnimalName(claim)))
	return nil
}

// Relay 넘기기 — 결정권자가 실물을 몰래 확인(cr_peek)한 뒤 아직 카드를
// 안 본 다른 사람에게 새 선언으로 전달한다 (같은 카드 릴레이).
func (g *CRGame) Relay(seat, target int, claim CRAnimal) error {
	if g.Phase != CRPhaseDeciding {
		return errors.New("지금은 넘길 수 없습니다")
	}
	if seat != g.HolderSeat {
		return errors.New("당신이 결정할 차례가 아닙니다")
	}
	if !g.CanRelay(seat) {
		return errors.New("카드를 안 본 사람이 없어 넘길 수 없습니다 — 판정만 가능합니다")
	}
	if !crValidAnimal(claim) {
		return errors.New("알 수 없는 동물입니다")
	}
	if target < 0 || target >= len(g.Players) || target == seat {
		return errors.New("대상을 올바르게 선택하세요")
	}
	if g.inChain(target) {
		return errors.New("이미 카드를 본 사람에게는 넘길 수 없습니다")
	}

	holder := g.Players[seat]
	g.Chain = append(g.Chain, seat)
	g.PasserSeat = seat
	g.Claim = claim
	g.HolderSeat = target
	g.StateSeq++

	g.emit("relay", seat, fmt.Sprintf("%s님이 몰래 확인하고 %s님에게 넘기며 \"이건 %s다!\"",
		holder.Name, g.Players[target].Name, crAnimalName(claim)))
	return nil
}

// Judge 판정 — truth true = "참"(선언이 실물과 같다), false = "거짓" 선언.
// 틀리면 카드가 자기 진열로 가고 자신이 다음 전달자, 맞히면 카드가 마지막
// 전달자의 진열로 가고 그 사람이 다시 전달자. 판정 순간 실물이 공개된다.
func (g *CRGame) Judge(seat int, truth bool) error {
	if g.Phase != CRPhaseDeciding {
		return errors.New("지금은 판정할 수 없습니다")
	}
	if seat != g.HolderSeat {
		return errors.New("당신이 결정할 차례가 아닙니다")
	}

	actual := g.Card == g.Claim
	correct := truth == actual
	call := "참"
	if !truth {
		call = "거짓"
	}
	holder := g.Players[seat]
	card := g.Card

	recv := seat
	if correct {
		recv = g.PasserSeat
		g.emit("judge_correct", seat, fmt.Sprintf(
			"%s님의 \"%s이다\" 판정 적중! 실물은 %s — 카드가 %s님의 진열에 놓입니다",
			holder.Name, call, crAnimalName(card), g.Players[recv].Name))
	} else {
		g.emit("judge_wrong", seat, fmt.Sprintf(
			"%s님의 \"%s이다\" 판정 실패! 실물은 %s — 카드가 자기 진열에 놓입니다",
			holder.Name, call, crAnimalName(card)))
	}

	receiver := g.Players[recv]
	receiver.Display[card]++

	// 릴레이 해제 — 실물은 진열로 공개됐다
	g.Card = ""
	g.Claim = ""
	g.Chain = nil
	g.HolderSeat = -1

	// 패배 조건 1: 같은 동물 4장이 진열에 모임 (즉시 게임 종료)
	if receiver.Display[card] >= CRLoseCount {
		g.finish(recv, CRLoseFourAnimals, fmt.Sprintf(
			"%s님의 진열에 %s %d장이 모여 패배했습니다!",
			receiver.Name, crAnimalName(card), CRLoseCount))
		return nil
	}
	g.beginPassing(recv)
	return nil
}

// beginPassing seat 이 다음 전달자가 된다.
// 패배 조건 2: 차례인데 손패가 0장이면 그 사람 패배.
func (g *CRGame) beginPassing(seat int) {
	g.PasserSeat = seat
	if len(g.Players[seat].Hand) == 0 {
		g.finish(seat, CRLoseEmptyHand, fmt.Sprintf(
			"%s님이 전달할 카드가 없어 패배했습니다!", g.Players[seat].Name))
		return
	}
	g.Phase = CRPhasePassing
	g.StateSeq++
}

// finish 패자 확정 — 나머지 전원 승리로 즉시 종료
func (g *CRGame) finish(loser int, reason, msg string) {
	g.LoserSeat = loser
	g.LoseReason = reason
	g.HolderSeat = -1
	g.Card = ""
	g.Claim = ""
	g.Chain = nil
	g.Phase = CRPhaseGameOver
	g.emit("defeated", loser, msg)
}
