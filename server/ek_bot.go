package server

import (
	"fmt"
	"math/rand"
	"time"
)

// ==================== 익스플로딩 키튼 연습봇 ====================
//
// 스냅샷(ek_game_state)과 자기 개인 메시지(ek_future)만 보고 반응한다.
// 봇도 사람과 같은 조건 — 자기 손패(yourHand)와 공개 정보만 안다.
//
//   - 차례: 예지로 본 맨 위가 폭탄이면 공격/건너뛰기를 최우선으로 쓰고,
//     아니면 위험도(남은 폭탄 ÷ 덱 잔량)가 높을 때 60% 확률로 쓴다.
//     고양이 짝이 있으면 30% 확률로 훔치고, 그 외에는 그냥 뽑는다.
//   - 예지: 손에 있고 아는 게 없으면 40% 확률로 사용, 결과는 기억해 둔다.
//   - 노프: 남이 낸 건너뛰기/공격에 20% 확률로 노프 (겹치기 포함).
//   - 되꽂기: 30% 확률로 맨 위 바로 아래(악랄), 아니면 무작위 위치.
//   - 부탁: 무작위 카드를 건넨다.
//
// 같은 대기 상태에 스냅샷이 여러 번 와도(통과 누적·관전 입장 등) 한 번만
// 응답하도록 대기 상태 식별키(phase+endsAt+nopeCount)로 중복을 걸러낸다.

// ekRiskThreshold 이 이상이면 "폭탄 위험이 높다"고 본다 (남은 폭탄 ÷ 덱 잔량)
const ekRiskThreshold = 0.25

// ekBotPlayerView 봇이 스냅샷에서 꺼내 쓰는 좌석 정보
type ekBotPlayerView struct {
	Seat      int  `json:"seat"`
	Alive     bool `json:"alive"`
	HandCount int  `json:"handCount"`
}

// ekBotPending 봇이 참조하는 대기 상태 요약
type ekBotPending struct {
	Kind       string `json:"kind"`
	BySeat     int    `json:"bySeat"`
	TargetSeat int    `json:"targetSeat"`
	NopeCount  int    `json:"nopeCount"`
}

// ekBotState 봇이 상태 스냅샷에서 꺼내 쓰는 최소 정보
type ekBotState struct {
	YourSeat    int               `json:"yourSeat"`
	Phase       EKPhase           `json:"phase"`
	CurrentSeat int               `json:"currentSeat"`
	TurnsLeft   int               `json:"turnsLeft"`
	EndsAt      int64             `json:"endsAt"`
	DeckLeft    int               `json:"deckLeft"`
	DiscardTop  string            `json:"discardTop"`
	Pending     *ekBotPending     `json:"pending"`
	YourHand    []EKHandCardView  `json:"yourHand"`
	Players     []ekBotPlayerView `json:"players"`
}

// ekBrain 스냅샷 기반 판단 (봇 대체 좌석도 같은 두뇌를 쓴다)
type ekBrain struct {
	rng *rand.Rand
	// lastKey 마지막으로 응답한 대기 상태 식별키 (중복 응답 방지)
	lastKey string

	// future 예지로 본 덱 맨 위 카드들 (기억). 덱 잔량이 줄면 그만큼 앞에서
	// 덜어내고, 늘어나면(폭탄 되꽂기) 또는 셔플이 보이면 통째로 버린다.
	future     []string
	futureDeck int
}

func newEKBrain() *ekBrain {
	return &ekBrain{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

// decide 공용 러너 계약 — 스냅샷과 개인 예지 결과에 반응한다
func (b *ekBrain) decide(msg EKMessage) *EKMessage {
	switch msg.Type {
	case EKMsgFuture:
		if fut, ok := botPayloadAs[EKFuturePayload](msg.Payload); ok {
			b.future = append([]string{}, fut.Cards...)
			b.futureDeck = -1 // 다음 스냅샷의 deckLeft 로 기준을 잡는다
		}
		return nil
	case EKMsgGameState:
		state, ok := botPayloadAs[ekBotState](msg.Payload)
		if !ok {
			return nil
		}
		b.syncFuture(state)
		return b.decideState(state)
	}
	return nil
}

// syncFuture 예지 기억을 현재 덱 잔량에 맞춘다
func (b *ekBrain) syncFuture(s ekBotState) {
	if len(b.future) == 0 {
		return
	}
	if b.futureDeck < 0 { // 예지 직후 첫 스냅샷 — 여기가 기준점이다
		b.futureDeck = s.DeckLeft
		return
	}
	if s.DiscardTop == string(EKCardShuffle) || s.DeckLeft > b.futureDeck {
		b.future = nil // 셔플·되꽂기로 순서가 흐트러졌다
		b.futureDeck = 0
		return
	}
	if drawn := b.futureDeck - s.DeckLeft; drawn > 0 {
		if drawn >= len(b.future) {
			b.future = nil
		} else {
			b.future = b.future[drawn:]
		}
		b.futureDeck = s.DeckLeft
	}
}

// topIsBomb 예지 기억상 덱 맨 위가 폭탄인지
func (b *ekBrain) topIsBomb() bool {
	return len(b.future) > 0 && b.future[0] == string(EKCardBomb)
}

// ekStateKey 대기 상태 식별키. 마감 시각(endsAt)만으로는 부족하다 — 노프
// 창(50ms~5초)이 닫히고 차례가 다시 열릴 때 두 마감이 같은 밀리초로 떨어지면
// 키가 겹쳐 봇이 자기 차례를 통째로 건너뛴다(무응답 → AFK 자동 뽑기). 그래서
// 실제로 무언가 바뀌면 반드시 달라지는 값들(버린 더미 맨 위·덱 잔량·차례 수·
// 손패 장수)을 함께 넣는다. 같은 대기 상태에서 스냅샷이 여러 번 와도(통과
// 누적·관전 입장) 이 값들은 그대로라 중복 응답은 계속 막힌다.
func ekStateKey(s ekBotState) string {
	nope, by, target := 0, -1, -1
	if s.Pending != nil {
		nope, by, target = s.Pending.NopeCount, s.Pending.BySeat, s.Pending.TargetSeat
	}
	hands := 0
	for _, p := range s.Players {
		hands = hands*100 + p.HandCount
	}
	return fmt.Sprintf("%s|%d|%d|%d|%d|%s|%d|%d|%d|%d|%d",
		s.Phase, s.EndsAt, s.CurrentSeat, s.TurnsLeft, s.DeckLeft, s.DiscardTop,
		nope, by, target, len(s.YourHand), hands)
}

// handled 같은 대기 상태에 이미 응답했는지 — 처음이면 키를 기록한다
func (b *ekBrain) handled(key string) bool {
	if b.lastKey == key {
		return true
	}
	b.lastKey = key
	return false
}

func (b *ekBrain) decideState(s ekBotState) *EKMessage {
	me := s.YourSeat
	if me < 0 || me >= len(s.Players) || !s.Players[me].Alive {
		return nil
	}
	key := ekStateKey(s)

	switch s.Phase {
	case EKPhaseTurn:
		if s.CurrentSeat != me || b.handled(key) {
			return nil
		}
		return b.chooseTurn(s)

	case EKPhaseNopeWindow:
		if s.Pending == nil {
			return nil
		}
		// 방금 카드를 낸 사람은 자기 카드에 노프를 겹칠 수 없다. 노프가
		// 한 번이라도 쌓이면 원래 시전자도 응답 대상이 된다 (서버가 마지막
		// 노프를 낸 좌석만 걸러낸다).
		if s.Pending.NopeCount == 0 && s.Pending.BySeat == me {
			return nil
		}
		if b.handled(key) {
			return nil
		}
		return b.chooseNope(s)

	case EKPhaseFavorWait:
		if s.Pending == nil || s.Pending.TargetSeat != me ||
			len(s.YourHand) == 0 || b.handled(key) {
			return nil
		}
		return &EKMessage{Type: EKMsgGive,
			Payload: EKGivePayload{Index: b.rng.Intn(len(s.YourHand))}}

	case EKPhaseDefusePlace:
		if s.Pending == nil || s.Pending.BySeat != me || b.handled(key) {
			return nil
		}
		pos := b.rng.Intn(s.DeckLeft + 1)
		if s.DeckLeft >= 1 && b.rng.Float64() < 0.3 {
			pos = 1 // 맨 위 바로 아래 — 다음 사람이 아니라 그 다음을 노린다
		}
		b.future = nil // 내가 되꽂았으니 예지 기억은 무효
		return &EKMessage{Type: EKMsgDefusePlace, Payload: EKDefusePlacePayload{Position: pos}}
	}
	return nil
}

// ekBombsLeft 덱에 남은 폭탄 수 — 폭탄은 손에 들 수 없으므로 항상
// (생존자 수 - 1)이다 (공개 정보라 봇도 같은 식을 쓴다)
func ekBombsLeft(s ekBotState) int {
	alive := 0
	for _, p := range s.Players {
		if p.Alive {
			alive++
		}
	}
	if alive <= 1 {
		return 0
	}
	return alive - 1
}

// ekHandIndex 손패에서 해당 종류의 첫 인덱스 (-1 없음)
func ekHandIndex(hand []EKHandCardView, kind EKCard) int {
	for i, c := range hand {
		if c.Kind == string(kind) {
			return i
		}
	}
	return -1
}

// chooseTurn 자기 차례의 행동 선택
func (b *ekBrain) chooseTurn(s ekBotState) *EKMessage {
	skip := ekHandIndex(s.YourHand, EKCardSkip)
	attack := ekHandIndex(s.YourHand, EKCardAttack)
	shuffle := ekHandIndex(s.YourHand, EKCardShuffle)
	future := ekHandIndex(s.YourHand, EKCardFuture)

	play := func(i int) *EKMessage {
		return &EKMessage{Type: EKMsgPlay, Payload: EKPlayPayload{Index: i}}
	}

	// 1) 맨 위가 폭탄인 걸 안다 — 피할 수 있으면 무조건 피한다
	if b.topIsBomb() {
		switch {
		case attack >= 0:
			return play(attack)
		case skip >= 0:
			return play(skip)
		case shuffle >= 0:
			b.future = nil
			return play(shuffle)
		}
	}

	// 2) 아는 게 없고 예지가 있으면 들여다본다
	if future >= 0 && len(b.future) == 0 && b.rng.Float64() < 0.4 {
		return play(future)
	}

	// 3) 폭탄 위험이 높으면 건너뛰기·공격을 쓴다
	risk := 0.0
	if s.DeckLeft > 0 {
		risk = float64(ekBombsLeft(s)) / float64(s.DeckLeft)
	}
	if risk >= ekRiskThreshold && (skip >= 0 || attack >= 0) && b.rng.Float64() < 0.6 {
		if attack >= 0 {
			return play(attack)
		}
		return play(skip)
	}

	// 4) 고양이 짝이 있으면 30% 확률로 훔친다
	if b.rng.Float64() < 0.3 {
		if m := b.choosePair(s); m != nil {
			return m
		}
	}

	// 5) 그냥 뽑는다 (차례 종료)
	return &EKMessage{Type: EKMsgDraw}
}

// choosePair 같은 종류 고양이 2장 + 훔칠 상대가 있으면 훔치기 메시지
func (b *ekBrain) choosePair(s ekBotState) *EKMessage {
	seen := map[string][]int{}
	for i, c := range s.YourHand {
		if !ekIsCat(EKCard(c.Kind)) {
			continue
		}
		seen[c.Kind] = append(seen[c.Kind], i)
		if len(seen[c.Kind]) < EKPairSize {
			continue
		}
		targets := []int{}
		for _, p := range s.Players {
			if p.Alive && p.Seat != s.YourSeat && p.HandCount > 0 {
				targets = append(targets, p.Seat)
			}
		}
		if len(targets) == 0 {
			return nil
		}
		target := targets[b.rng.Intn(len(targets))]
		return &EKMessage{Type: EKMsgPlayPair, Payload: EKPlayPairPayload{
			Indexes: append([]int{}, seen[c.Kind][:EKPairSize]...), TargetSeat: &target}}
	}
	return nil
}

// chooseNope 노프 창 응답 — 건너뛰기·공격만 20% 확률로 막고 나머지는 통과
func (b *ekBrain) chooseNope(s ekBotState) *EKMessage {
	kind := s.Pending.Kind
	juicy := kind == string(EKCardSkip) || kind == string(EKCardAttack)
	if juicy && ekHandIndex(s.YourHand, EKCardNope) >= 0 && b.rng.Float64() < 0.2 {
		return &EKMessage{Type: EKMsgNope}
	}
	return &EKMessage{Type: EKMsgPass}
}

// ==================== 봇 소환 ====================

// spawnEKBot 대기실의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *EKHub) spawnEKBot(room *ekRoom, name string) bool {
	bot := &EKClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runEKBot(bot)
	return true
}

// takeoverEKBot 유예 만료 좌석을 이어받는 연습봇 — 이름·좌석을 유지해
// 진행 중인 노프 창 통과·되꽂기가 그대로 이어진다
func (h *EKHub) takeoverEKBot(room *ekRoom, seat int, name string) *EKClient {
	bot := &EKClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	bot.GameID = room.Game.ID
	bot.Seat = seat
	h.runEKBot(bot)
	return bot
}

// runEKBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *EKHub) runEKBot(bot *EKClient) {
	brain := newEKBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m EKMessage) { h.gameMessage <- EKGameMessage{Client: bot, Message: m} },
		func(m EKMessage) bool { return m.Type == EKMsgGameOver || m.Type == EKMsgSessionExpired })
}

// ekRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록용)
func ekRoomHasBot(room *ekRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
