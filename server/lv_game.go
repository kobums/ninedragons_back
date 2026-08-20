package server

import (
	"errors"
	"fmt"
	"math/rand"
	"time"
)

// ==================== 러브레터 순수 규칙 ====================
//
// 카드 효과·라운드 진행·토큰 판정만 다룬다. 클라이언트·타이머를 모르며,
// 허브(lv_hub.go)가 결과 구조체를 받아 이벤트·비공개 통지로 번역한다.

// lvDeckComposition 16장: 경비병×5 사제×2 남작×2 시녀×2 왕자×2 왕×1
// 백작부인×1 공주×1
func lvDeckComposition() []int {
	return []int{1, 1, 1, 1, 1, 2, 2, 3, 3, 4, 4, 5, 5, 6, 7, 8}
}

// lvCardName 카드 값의 한글 이름 (이벤트·비공개 문구용)
func lvCardName(v int) string {
	switch v {
	case LVGuard:
		return "경비병"
	case LVPriest:
		return "사제"
	case LVBaron:
		return "남작"
	case LVHandmaid:
		return "시녀"
	case LVPrince:
		return "왕자"
	case LVKing:
		return "왕"
	case LVCountess:
		return "백작부인"
	case LVPrincess:
		return "공주"
	}
	return fmt.Sprintf("카드%d", v)
}

// lvTargetTokens 인원별 목표 토큰: 2인 7 / 3인 5 / 4인 4
func lvTargetTokens(playerCount int) int {
	switch playerCount {
	case 2:
		return 7
	case 3:
		return 5
	}
	return 4
}

// NewLVGame 대기 상태의 새 게임
func NewLVGame(id string) *LVGame {
	return &LVGame{
		ID:            id,
		Players:       []*LVPlayer{},
		Phase:         LVPhaseWaiting,
		CurrentSeat:   -1,
		WinnerSeat:    -1,
		RemovedFaceUp: []int{},
	}
}

// AddPlayer 대기실 입장. 좌석 번호를 돌려준다.
func (g *LVGame) AddPlayer(name string) (int, error) {
	if g.Phase != LVPhaseWaiting {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= LVMaxPlayers {
		return -1, errors.New("자리가 없습니다 (최대 4명)")
	}
	seat := len(g.Players)
	g.Players = append(g.Players, &LVPlayer{
		Seat: seat, Name: name, Hand: []int{}, Discards: []int{},
	})
	return seat, nil
}

// RemovePlayer 대기실 퇴장. 좌석을 앞으로 당겨 빈틈을 없앤다.
func (g *LVGame) RemovePlayer(seat int) {
	if g.Phase != LVPhaseWaiting || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i, p := range g.Players {
		p.Seat = i
	}
}

// CanStart 시작 가능 여부 (호스트 명시 시작 — 2인부터)
func (g *LVGame) CanStart() bool {
	return g.Phase == LVPhaseWaiting && len(g.Players) >= LVMinPlayers
}

// Start 게임 시작 — 목표 토큰을 정하고 첫 라운드를 연다. 선공은 무작위.
func (g *LVGame) Start(rng *rand.Rand) error {
	if !g.CanStart() {
		return errors.New("2명 이상 모여야 시작할 수 있습니다")
	}
	g.Ready = true
	g.StartedAt = time.Now()
	g.TargetTokens = lvTargetTokens(len(g.Players))
	g.RoundNo = 1
	g.startRound(rng, rng.Intn(len(g.Players)))
	return nil
}

// NextRound 라운드 사이 자동 진행 — 직전 라운드 승자가 선공 (원작 규칙)
func (g *LVGame) NextRound(rng *rand.Rand) error {
	if g.Phase != LVPhaseRoundEnd || g.RoundResult == nil {
		return errors.New("지금은 다음 라운드를 시작할 수 없습니다")
	}
	first := g.RoundResult.WinnerSeat
	g.RoundNo++
	g.RoundResult = nil
	g.startRound(rng, first)
	return nil
}

// startRound 덱 셔플·제거·배분 후 선공의 턴을 연다
func (g *LVGame) startRound(rng *rand.Rand, firstSeat int) {
	deck := lvDeckComposition()
	rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })

	// 1장 비공개 제거 (덱 소진 시 왕자 효과의 마지막 공급원)
	g.Removed = deck[0]
	g.RemovedTaken = false
	deck = deck[1:]

	// 2인전은 추가로 3장 공개 제거
	g.RemovedFaceUp = []int{}
	if len(g.Players) == 2 {
		g.RemovedFaceUp = append([]int{}, deck[:3]...)
		deck = deck[3:]
	}

	for _, p := range g.Players {
		p.Alive = true
		p.Protected = false
		p.Discards = []int{}
		p.Hand = []int{deck[0]}
		deck = deck[1:]
	}
	g.Deck = deck
	g.Phase = LVPhasePlaying
	g.beginTurn(firstSeat)
}

// beginTurn 좌석의 턴 시작 — 보호 해제 후 1장 뽑아 손패 2장을 만든다
func (g *LVGame) beginTurn(seat int) {
	g.CurrentSeat = seat
	p := g.Players[seat]
	p.Protected = false // 시녀 보호는 다음 내 턴 시작까지
	if len(g.Deck) > 0 {
		p.Hand = append(p.Hand, g.Deck[0])
		g.Deck = g.Deck[1:]
	}
}

// aliveCount 이번 라운드 생존자 수
func (g *LVGame) aliveCount() int {
	n := 0
	for _, p := range g.Players {
		if p.Alive {
			n++
		}
	}
	return n
}

// validTargets 카드의 유효 대상 좌석 목록. 1·2·3·6 은 보호 중이 아닌 다른
// 생존자, 5(왕자)는 자신 포함.
func (g *LVGame) validTargets(actorSeat, card int) []int {
	targets := []int{}
	for _, p := range g.Players {
		if !p.Alive {
			continue
		}
		if p.Seat == actorSeat {
			if card == LVPrince {
				targets = append(targets, p.Seat) // 왕자는 자신 지정 가능
			}
			continue
		}
		if p.Protected {
			continue
		}
		targets = append(targets, p.Seat)
	}
	return targets
}

// LVPlayResult 플레이 한 번의 판정 결과. 허브가 이벤트·비공개 통지로
// 번역하는 데 필요한 사실만 담는다.
type LVPlayResult struct {
	ActorSeat  int
	Card       int
	TargetSeat int // -1 = 대상 없음(효과 없이 버림 포함)
	NoEffect   bool

	// 경비병
	Guess        int
	GuessCorrect bool

	// 사제 — 열람한 대상 카드 (요청자에게만 통지)
	PriestSeen int

	// 남작 — 비교 값(당사자끼리만 통지)과 패자 (-1 = 동점 무효)
	BaronActorCard  int
	BaronTargetCard int
	BaronLoserSeat  int

	// 왕자 — 대상이 버린 카드
	PrinceDiscarded int

	Eliminated []int // 이번 플레이로 탈락한 좌석들

	RoundEnded bool
	GameOver   bool
}

// Play 현재 턴 플레이어가 손패 2장 중 1장을 낸다.
// target 은 좌석 0 구분을 위해 포인터, guess 는 경비병 전용(2~8).
func (g *LVGame) Play(seat, card int, target *int, guess int) (*LVPlayResult, error) {
	if g.Phase != LVPhasePlaying {
		return nil, errors.New("지금은 카드를 낼 수 없습니다")
	}
	if seat != g.CurrentSeat {
		return nil, errors.New("당신의 차례가 아닙니다")
	}
	actor := g.Players[seat]
	if !actor.Alive {
		return nil, errors.New("탈락한 플레이어는 카드를 낼 수 없습니다")
	}
	handIdx := -1
	for i, c := range actor.Hand {
		if c == card {
			handIdx = i
			break
		}
	}
	if handIdx < 0 {
		return nil, errors.New("손에 없는 카드입니다")
	}

	// 백작부인 강제: 왕/왕자와 함께 들고 있으면 반드시 백작부인
	if card != LVCountess && lvContains(actor.Hand, LVCountess) &&
		(lvContains(actor.Hand, LVKing) || lvContains(actor.Hand, LVPrince)) {
		return nil, errors.New("왕이나 왕자와 함께 있으면 백작부인을 내야 합니다")
	}

	res := &LVPlayResult{
		ActorSeat: seat, Card: card, TargetSeat: -1,
		PriestSeen: -1, BaronLoserSeat: -1, PrinceDiscarded: -1,
		Eliminated: []int{},
	}

	// 대상 판정 — 유효 대상이 없으면 효과 없는 카드로 낸다 (허용)
	needsTarget := card == LVGuard || card == LVPriest || card == LVBaron ||
		card == LVPrince || card == LVKing
	var targetPlayer *LVPlayer
	if needsTarget {
		valid := g.validTargets(seat, card)
		if len(valid) == 0 {
			res.NoEffect = true
		} else {
			if target == nil {
				return nil, errors.New("대상을 지정해야 합니다")
			}
			if !lvContains(valid, *target) {
				return nil, errors.New("지정할 수 없는 대상입니다")
			}
			res.TargetSeat = *target
			targetPlayer = g.Players[*target]
		}
	}
	if card == LVGuard && targetPlayer != nil && (guess < 2 || guess > 8) {
		return nil, errors.New("경비병은 2~8 사이의 값만 추측할 수 있습니다")
	}

	// 낸 카드를 손패에서 버림 더미로 (공주를 스스로 내는 것도 합법 — 탈락)
	actor.Hand = append(actor.Hand[:handIdx], actor.Hand[handIdx+1:]...)
	actor.Discards = append(actor.Discards, card)

	// 효과 적용
	switch {
	case res.NoEffect || (!needsTarget && card != LVHandmaid && card != LVPrincess):
		// 백작부인(7)·효과 없는 플레이 — 아무 일도 없다
	case card == LVGuard:
		res.Guess = guess
		if lvContains(targetPlayer.Hand, guess) {
			res.GuessCorrect = true
			g.eliminate(targetPlayer, res)
		}
	case card == LVPriest:
		res.PriestSeen = targetPlayer.Hand[0]
	case card == LVBaron:
		res.BaronActorCard = actor.Hand[0]
		res.BaronTargetCard = targetPlayer.Hand[0]
		if res.BaronActorCard > res.BaronTargetCard {
			res.BaronLoserSeat = targetPlayer.Seat
			g.eliminate(targetPlayer, res)
		} else if res.BaronTargetCard > res.BaronActorCard {
			res.BaronLoserSeat = actor.Seat
			g.eliminate(actor, res)
		}
	case card == LVHandmaid:
		actor.Protected = true
	case card == LVPrince:
		discarded := targetPlayer.Hand[0]
		res.PrinceDiscarded = discarded
		targetPlayer.Hand = []int{}
		targetPlayer.Discards = append(targetPlayer.Discards, discarded)
		if discarded == LVPrincess {
			g.eliminate(targetPlayer, res)
		} else {
			targetPlayer.Hand = []int{g.drawForPrince()}
		}
	case card == LVKing:
		actor.Hand, targetPlayer.Hand = targetPlayer.Hand, actor.Hand
	case card == LVPrincess:
		g.eliminate(actor, res)
	}

	// 턴 종료 판정: 1인 생존 → 라운드 승 / 덱 소진 → 손패 비교
	if g.aliveCount() <= 1 {
		g.resolveRound("last_standing")
		res.RoundEnded = true
	} else if len(g.Deck) == 0 {
		g.resolveRound("highest_card")
		res.RoundEnded = true
	} else {
		g.beginTurn(g.nextAliveSeat(seat))
	}
	res.GameOver = g.Phase == LVPhaseGameOver
	return res, nil
}

// eliminate 탈락 처리 — 남은 손패를 공개 버림 더미로 옮긴다
func (g *LVGame) eliminate(p *LVPlayer, res *LVPlayResult) {
	if !p.Alive {
		return
	}
	p.Alive = false
	p.Discards = append(p.Discards, p.Hand...)
	p.Hand = []int{}
	res.Eliminated = append(res.Eliminated, p.Seat)
}

// drawForPrince 왕자 효과의 다시 뽑기 — 덱이 비었으면 비공개 제거 카드를
// 준다 (원작 규칙). 같은 턴에 두 번 일어날 수 없어 RemovedTaken 은 안전망이다.
func (g *LVGame) drawForPrince() int {
	if len(g.Deck) > 0 {
		c := g.Deck[0]
		g.Deck = g.Deck[1:]
		return c
	}
	g.RemovedTaken = true
	return g.Removed
}

// nextAliveSeat seat 다음의 생존 좌석 (시계 방향)
func (g *LVGame) nextAliveSeat(seat int) int {
	n := len(g.Players)
	for i := 1; i <= n; i++ {
		s := (seat + i) % n
		if g.Players[s].Alive {
			return s
		}
	}
	return seat
}

// resolveRound 라운드 승자 판정과 토큰 지급. 덱 소진 비교는 손패 값이
// 높은 쪽, 동점이면 버린 카드 합, 그래도 동점이면 낮은 좌석 (결정적 판정).
func (g *LVGame) resolveRound(reason string) {
	winner := -1
	bestCard, bestSum := -1, -1
	for _, p := range g.Players {
		if !p.Alive {
			continue
		}
		card := -1
		if len(p.Hand) > 0 {
			card = p.Hand[0]
		}
		sum := 0
		for _, d := range p.Discards {
			sum += d
		}
		if card > bestCard || (card == bestCard && sum > bestSum) {
			winner = p.Seat
			bestCard, bestSum = card, sum
		}
	}
	if winner < 0 {
		winner = 0 // 도달 불가 안전망
	}

	g.Players[winner].Tokens++
	g.RoundResult = &LVRoundResult{WinnerSeat: winner, Reason: reason}
	if g.Players[winner].Tokens >= g.TargetTokens {
		g.Phase = LVPhaseGameOver
		g.WinnerSeat = winner
		return
	}
	g.Phase = LVPhaseRoundEnd
}

// lvContains 정수 슬라이스 포함 여부
func lvContains(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
