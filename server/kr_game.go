package server

import (
	"errors"
	"fmt"
	"math/rand"
	"time"
)

// ==================== 노 터치 크라켄 순수 규칙 ====================
//
// 역할 배분·덱 구성·라운드 재배분·지목 공개·선언·승패 즉시 판정만 다룬다.
// 클라이언트·타이머를 모르며, 허브(kr_hub.go)가 지목 마감(45초)과 라운드
// 정산 대기(5초)를 걸고 이벤트 큐(DrainEvents)를 방송한다.
//
// 라운드 재배분이 이 파일의 핵심이다. 라운드 r 시작에 "각자의 손에 남은
// 미공개 카드"를 전부 회수해 섞고 각자 (6-r)장씩 다시 나눈다. 공개된 카드는
// 중앙으로 빠져 다시 쓰이지 않으므로 회수량은 매 라운드 정확히 N장씩 줄고,
// 4라운드가 끝나면 총 4N장이 공개되고 N장은 끝내 열리지 않는다.
//
//	1R: 5N장 배분(덱 전량) → N장 공개 → 4N장 잔류
//	2R: 4N장 재배분        → N장 공개 → 3N장 잔류
//	3R: 3N장 재배분        → N장 공개 → 2N장 잔류
//	4R: 2N장 재배분        → N장 공개 → N장 미공개로 종료

// NewKRGame 대기 상태의 새 게임
func NewKRGame(id string) *KRGame {
	return &KRGame{
		ID:          id,
		Players:     []*KRPlayer{},
		Phase:       KRPhaseWaiting,
		CurrentSeat: -1,
	}
}

// AddPlayer 대기실 입장. 좌석 번호를 돌려준다.
func (g *KRGame) AddPlayer(name string) (int, error) {
	if g.Phase != KRPhaseWaiting {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= KRMaxPlayers {
		return -1, fmt.Errorf("자리가 없습니다 (최대 %d명)", KRMaxPlayers)
	}
	seat := len(g.Players)
	g.Players = append(g.Players, &KRPlayer{
		Seat:              seat,
		Name:              name,
		Hand:              []KRCard{},
		RevealedThisRound: []KRCard{},
	})
	return seat, nil
}

// RemovePlayer 대기실 퇴장. 좌석을 앞으로 당겨 빈틈을 없앤다.
func (g *KRGame) RemovePlayer(seat int) {
	if g.Phase != KRPhaseWaiting || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i, p := range g.Players {
		p.Seat = i
	}
}

// CanStart 시작 가능 여부 (호스트 명시 시작 — 4인부터)
func (g *KRGame) CanStart() bool {
	return g.Phase == KRPhaseWaiting && len(g.Players) >= KRMinPlayers
}

// krBuildDeck 인원 N의 덱 5N장 — 보물 N·크라켄 1·꽝 4N-1
func krBuildDeck(n int) []KRCard {
	deck := make([]KRCard, 0, n*KRCardsPerPlayer)
	for i := 0; i < n; i++ {
		deck = append(deck, KRCardTreasure)
	}
	deck = append(deck, KRCardKraken)
	for i := 0; i < 4*n-1; i++ {
		deck = append(deck, KRCardBlank)
	}
	return deck
}

// Start 게임 시작 — 역할 풀에서 인원수만큼 뽑아 배정하고 1라운드를 배분한다.
// 시작 지목권자는 무작위 좌석이다.
func (g *KRGame) Start(rng *rand.Rand) error {
	if !g.CanStart() {
		return fmt.Errorf("%d명 이상 모여야 시작할 수 있습니다", KRMinPlayers)
	}
	n := len(g.Players)
	g.Ready = true
	g.StartedAt = time.Now()

	// 역할 풀에서 인원수만큼만 뽑는다 — 실제 진영 구성은 아무도 모른다
	g.Pool = krRolePoolFor(n)
	roles := make([]KRRole, 0, g.Pool.Expedition+g.Pool.Skeleton)
	for i := 0; i < g.Pool.Expedition; i++ {
		roles = append(roles, KRRoleExpedition)
	}
	for i := 0; i < g.Pool.Skeleton; i++ {
		roles = append(roles, KRRoleSkeleton)
	}
	rng.Shuffle(len(roles), func(i, j int) { roles[i], roles[j] = roles[j], roles[i] })
	for i, p := range g.Players {
		p.Role = roles[i]
		p.Claim = nil
		p.Hand = []KRCard{}
		p.RevealedThisRound = []KRCard{}
	}

	g.FoundTreasure = 0
	g.FoundKraken = 0
	g.Reveal = nil
	g.Result = nil
	g.EndReason = ""

	g.Round = 1
	g.dealRound(rng)
	g.RevealsLeft = n
	g.CurrentSeat = rng.Intn(n)
	g.Phase = KRPhaseRevealing
	g.StateSeq++
	g.emit("round_start", g.CurrentSeat, fmt.Sprintf(
		"1라운드 — 각자 %d장씩 받았습니다. %s님부터 지목합니다",
		krHandSize(1), g.Players[g.CurrentSeat].Name))
	return nil
}

// dealRound 현재 g.Round 기준 재배분. 1라운드는 새 덱 5N장, 이후 라운드는
// 각자 손에 남은 미공개 카드를 전부 회수해 섞어 (6-r)장씩 나눈다.
// 공개된 카드는 중앙으로 빠져 회수 대상이 아니다.
func (g *KRGame) dealRound(rng *rand.Rand) {
	n := len(g.Players)
	var pool []KRCard
	if g.Round <= 1 {
		pool = krBuildDeck(n)
	} else {
		pool = make([]KRCard, 0, n*krHandSize(g.Round))
		for _, p := range g.Players {
			pool = append(pool, p.Hand...)
		}
	}
	for _, p := range g.Players {
		p.Hand = []KRCard{}
	}
	rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })

	per := krHandSize(g.Round)
	idx := 0
	for _, p := range g.Players {
		if idx+per > len(pool) { // 방어선 — 회수량은 항상 정확히 n*per 다
			per = len(pool) - idx
		}
		if per <= 0 {
			break
		}
		p.Hand = append([]KRCard{}, pool[idx:idx+per]...)
		idx += per
	}
}

// ==================== 이벤트 큐 ====================

func (g *KRGame) emit(kind string, seat int, msg string) {
	g.events = append(g.events, KRGameEvent{Kind: kind, Seat: seat, Message: msg})
}

// DrainEvents 쌓인 이벤트를 꺼내고 비운다 (허브가 방송)
func (g *KRGame) DrainEvents() []KRGameEvent {
	evs := g.events
	g.events = nil
	return evs
}

// ==================== 지목 / 공개 ====================

// Point 지목권자가 자기 외 좌석의 미공개 카드 1장을 지목해 즉시 공개한다.
// 미공개 카드가 0장인 좌석은 지목 대상이 될 수 없다 (지목권은 받을 수 있다).
func (g *KRGame) Point(seat, target, index int) error {
	if g.Phase != KRPhaseRevealing {
		return errors.New("지금은 지목할 수 없습니다")
	}
	if seat != g.CurrentSeat {
		return errors.New("지목 차례가 아닙니다")
	}
	if target == seat {
		return errors.New("자기 자신은 지목할 수 없습니다")
	}
	if target < 0 || target >= len(g.Players) {
		return errors.New("잘못된 좌석입니다")
	}
	tp := g.Players[target]
	if len(tp.Hand) == 0 {
		return errors.New("미공개 카드가 없는 좌석입니다")
	}
	if index < 0 || index >= len(tp.Hand) {
		return errors.New("잘못된 카드입니다")
	}
	g.revealCard(seat, target, index)
	return nil
}

// ForcePoint 지목 마감 — 무작위 대상(미공개 카드가 있는 타 좌석)의 무작위
// 카드를 자동 지목한다 (허브 타이머)
func (g *KRGame) ForcePoint(rng *rand.Rand) {
	if g.Phase != KRPhaseRevealing {
		return
	}
	seat := g.CurrentSeat
	if seat < 0 || seat >= len(g.Players) {
		return
	}
	targets := []int{}
	for _, p := range g.Players {
		if p.Seat != seat && len(p.Hand) > 0 {
			targets = append(targets, p.Seat)
		}
	}
	if len(targets) == 0 { // 구조적으로 오지 않는다 (라운드 중 잔류 카드 ≥ N+1)
		return
	}
	target := targets[rng.Intn(len(targets))]
	g.revealCard(seat, target, rng.Intn(len(g.Players[target].Hand)))
}

// revealCard 카드 1장을 공개하고 즉시 승패를 판정한다.
// 지목권은 방금 공개된 카드의 주인에게 넘어간다.
func (g *KRGame) revealCard(by, target, index int) {
	tp := g.Players[target]
	card := tp.Hand[index]
	tp.Hand = append(tp.Hand[:index], tp.Hand[index+1:]...)
	tp.RevealedThisRound = append(tp.RevealedThisRound, card)

	switch card {
	case KRCardTreasure:
		g.FoundTreasure++
	case KRCardKraken:
		g.FoundKraken++
	}
	g.RevealsLeft--
	g.CurrentSeat = target // 지목권 이양 (라운드가 끝나도 다음 라운드 선이 된다)

	msg := fmt.Sprintf("%s님이 %s님의 카드를 열었습니다 — %s",
		g.Players[by].Name, tp.Name, krCardLabel(card))
	g.Reveal = &KRReveal{Seat: target, BySeat: by, Card: card, Message: msg}
	g.emit("reveal", target, msg)

	// ---- 승패 즉시 판정 (무승부 없음) ----
	n := len(g.Players)
	if card == KRCardKraken {
		g.finishWith(string(KRRoleSkeleton), "kraken",
			fmt.Sprintf("크라켄이 깨어났습니다! %s님의 카드에서 크라켄이 나와 해골단이 승리합니다",
				tp.Name))
		return
	}
	if g.FoundTreasure >= n {
		g.finishWith(string(KRRoleExpedition), "all_treasure",
			fmt.Sprintf("보물 %d개를 모두 찾았습니다! 탐험대의 승리입니다", n))
		return
	}

	if g.RevealsLeft > 0 {
		g.StateSeq++ // 새 지목 차례 — 허브가 45초 마감을 다시 건다
		return
	}
	if g.Round >= KRRounds {
		g.finishWith(string(KRRoleSkeleton), "timeout",
			fmt.Sprintf("%d라운드가 모두 끝났습니다 — 보물 %d/%d개에 그쳐 해골단이 승리합니다",
				KRRounds, g.FoundTreasure, n))
		return
	}
	g.Phase = KRPhaseRoundEnd
	g.StateSeq++
	g.emit("round_end", -1, fmt.Sprintf(
		"%d라운드 종료 — 보물 %d/%d개를 찾았습니다. 남은 카드를 모아 다시 나눕니다",
		g.Round, g.FoundTreasure, n))
}

// NextRound 라운드 정산 대기가 끝나면 남은 카드를 회수·재배분한다 (허브 타이머).
// 이번 라운드 공개분과 선언은 초기화된다. 지목권은 마지막으로 공개된 카드의
// 주인이 그대로 이어받는다.
func (g *KRGame) NextRound(rng *rand.Rand) {
	if g.Phase != KRPhaseRoundEnd {
		return
	}
	g.Round++
	g.dealRound(rng)
	for _, p := range g.Players {
		p.RevealedThisRound = []KRCard{}
		p.Claim = nil // 선언은 라운드 재배분 때 초기화
	}
	g.RevealsLeft = len(g.Players)
	g.Phase = KRPhaseRevealing
	g.StateSeq++

	seat := g.CurrentSeat
	name := ""
	if seat >= 0 && seat < len(g.Players) {
		name = g.Players[seat].Name
	}
	g.emit("round_start", seat, fmt.Sprintf(
		"%d라운드 — 남은 카드를 섞어 각자 %d장씩 다시 받았습니다. %s님부터 지목합니다",
		g.Round, krHandSize(g.Round), name))
}

// ==================== 선언 ====================

// SubmitClaim 자기 손패 구성을 공개 선언한다 (거짓 허용, 구속력 없음).
// 꽝 수는 남은 장수에서 자동 계산하므로 담지 않는다. 남은 장수보다 많이
// 선언하는 것만 막는다 — 그 외 거짓은 규칙상 자유다.
func (g *KRGame) SubmitClaim(seat, treasure, kraken int) error {
	if g.Phase != KRPhaseRevealing && g.Phase != KRPhaseRoundEnd {
		return errors.New("지금은 선언할 수 없습니다")
	}
	if seat < 0 || seat >= len(g.Players) {
		return errors.New("잘못된 좌석입니다")
	}
	if treasure < 0 || kraken < 0 || kraken > 1 {
		return errors.New("잘못된 선언입니다")
	}
	p := g.Players[seat]
	if treasure+kraken > len(p.Hand) {
		return fmt.Errorf("남은 카드 %d장보다 많이 선언할 수 없습니다", len(p.Hand))
	}
	p.Claim = &KRClaim{Treasure: treasure, Kraken: kraken}

	krakenText := ""
	if kraken > 0 {
		krakenText = " · 크라켄 있음"
	}
	g.emit("claim", seat, fmt.Sprintf("%s님의 선언 — 남은 %d장 중 보물 %d장%s",
		p.Name, len(p.Hand), treasure, krakenText))
	return nil
}

// ==================== 종료 ====================

func (g *KRGame) finishWith(winner, reason, msg string) {
	g.Result = &KRResult{Winner: winner, Reason: reason, Message: msg}
	g.EndReason = reason
	g.Phase = KRPhaseGameOver
	g.RevealsLeft = 0
	g.StateSeq++
}
