package server

import (
	"fmt"
	"math"
	"math/rand"
	"time"
)

// ==================== 스컬킹 연습봇 ====================
//
// 스냅샷(kg_game_state)만 보고 반응한다. 봇도 사람과 같은 조건 — 자기
// yourHand·yourBid 만 알고 남의 손패는 모른다.
//   - 비드: 손패 강도 추정을 반올림한다. 해적·스컬킹·인어 각 1점, 검정 10 이상
//     1점, 색 12·13 은 0.5점. 0~라운드 범위로 자른다.
//   - 플레이: 아직 목표 트릭을 못 채웠으면 지금 트릭을 이길 수 있는 카드 중
//     가장 약한 것으로 "최소한으로" 이기고, 목표를 이미 채웠으면 가장 약한
//     카드(탈출 우선)를 버린다.
// 같은 대기 상태에 스냅샷이 여러 번 와도(관전 입장·접속 변화·타인 제출 등)
// 한 번만 행동하도록 상태 식별키로 중복을 걸러낸다.

// 봇이 "생각하는" 시간 (테스트에서 짧게 낮춘다)
var (
	kgBotBidDelay     = 500 * time.Millisecond
	kgBotBidJitterMs  = 500
	kgBotPlayDelay    = 700 * time.Millisecond
	kgBotPlayJitterMs = 700
)

// kgBotPlayerView 봇이 스냅샷에서 꺼내 쓰는 좌석 정보
type kgBotPlayerView struct {
	Seat         int  `json:"seat"`
	Bid          int  `json:"bid"`
	Tricks       int  `json:"tricks"`
	HandCount    int  `json:"handCount"`
	BidSubmitted bool `json:"bidSubmitted"`
}

// kgBotState 봇이 상태 스냅샷에서 꺼내 쓰는 최소 정보
type kgBotState struct {
	YourSeat    int               `json:"yourSeat"`
	Phase       KGPhase           `json:"phase"`
	Round       int               `json:"round"`
	MaxRound    int               `json:"maxRound"`
	TrickNo     int               `json:"trickNo"`
	CurrentSeat int               `json:"currentSeat"`
	LeadSuit    KGSuit            `json:"leadSuit"`
	Trick       []KGTrickPlay     `json:"trick"`
	YourHand    []KGCard          `json:"yourHand"`
	YourBid     *int              `json:"yourBid"`
	Players     []kgBotPlayerView `json:"players"`
}

// kgBrain 스냅샷 기반 판단 (봇 대체 좌석도 같은 두뇌를 쓴다)
type kgBrain struct {
	rng *rand.Rand
	// lastKey 마지막으로 행동한 대기 상태 식별키 (중복 행동 방지)
	lastKey string
}

func newKGBrain() *kgBrain {
	return &kgBrain{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

// decide 공용 러너 계약 — kg_game_state 에만 반응한다
func (b *kgBrain) decide(msg KGMessage) *KGMessage {
	if msg.Type != KGMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[kgBotState](msg.Payload)
	if !ok {
		return nil
	}
	return b.decideState(state)
}

// handled 같은 대기 상태에 이미 행동했는지 — 처음이면 키를 기록한다
func (b *kgBrain) handled(key string) bool {
	if b.lastKey == key {
		return true
	}
	b.lastKey = key
	return false
}

// think 사람처럼 잠깐 뜸을 들인다 (테스트에서는 var 를 낮춰 즉시 진행한다)
func (b *kgBrain) think(base time.Duration, jitterMs int) {
	d := base
	if jitterMs > 0 {
		d += time.Duration(b.rng.Intn(jitterMs)) * time.Millisecond
	}
	if d > 0 {
		time.Sleep(d)
	}
}

func (b *kgBrain) decideState(s kgBotState) *KGMessage {
	me := s.YourSeat
	if me < 0 || me >= len(s.Players) {
		return nil
	}

	switch s.Phase {
	case KGPhaseBidding:
		if s.YourBid != nil && *s.YourBid >= 0 {
			return nil // 이미 제출했다 (비드는 변경 불가)
		}
		if b.handled(fmt.Sprintf("bid|%d", s.Round)) {
			return nil
		}
		b.think(kgBotBidDelay, kgBotBidJitterMs)
		return &KGMessage{Type: KGMsgBid, Payload: KGBidPayload{Bid: kgBotBid(s)}}

	case KGPhasePlaying:
		if s.CurrentSeat != me || len(s.YourHand) == 0 {
			return nil
		}
		key := fmt.Sprintf("play|%d|%d|%d|%d", s.Round, s.TrickNo, len(s.Trick), len(s.YourHand))
		if b.handled(key) {
			return nil
		}
		index := b.pickPlay(s)
		if index < 0 {
			return nil
		}
		b.think(kgBotPlayDelay, kgBotPlayJitterMs)
		return &KGMessage{Type: KGMsgPlay, Payload: KGPlayPayload{Index: index}}
	}
	return nil
}

// kgBotBid 손패 강도 추정 — 해적·스컬킹·인어 각 1, 검정 10 이상 1,
// 색 12·13 은 0.5. 반올림 후 0~라운드로 자른다.
func kgBotBid(s kgBotState) int {
	strength := 0.0
	for _, c := range s.YourHand {
		switch c.Kind {
		case KGKindPirate, KGKindSkullKing, KGKindMermaid:
			strength += 1
		case KGKindNumber:
			if c.Suit == KGSuitBlack {
				if c.Rank >= 10 {
					strength += 1
				}
			} else if c.Rank >= 12 {
				strength += 0.5
			}
		}
	}
	bid := int(math.Round(strength))
	if bid < 0 {
		bid = 0
	}
	if bid > s.Round {
		bid = s.Round
	}
	return bid
}

// pickPlay 낼 카드의 손패 인덱스.
//   - 목표 트릭을 못 채웠고 리드면: 가장 강한 합법 카드
//   - 목표 트릭을 못 채웠고 후행이면: 지금 이길 수 있는 카드 중 가장 약한 것
//     (없으면 가장 약한 카드)
//   - 목표를 이미 채웠으면: 가장 약한 카드 (탈출이 최약이라 자연히 우선된다)
func (b *kgBrain) pickPlay(s kgBotState) int {
	legal := kgLegalIndexes(s.YourHand, s.LeadSuit)
	if len(legal) == 0 {
		return -1
	}

	me := s.YourSeat
	bid, tricks := 0, 0
	for _, p := range s.Players {
		if p.Seat == me {
			bid, tricks = p.Bid, p.Tricks
			break
		}
	}
	if s.YourBid != nil && *s.YourBid >= 0 {
		bid = *s.YourBid
	}

	weakest := legal[0]
	strongest := legal[0]
	for _, i := range legal {
		if kgCardPower(s.YourHand[i]) < kgCardPower(s.YourHand[weakest]) {
			weakest = i
		}
		if kgCardPower(s.YourHand[i]) > kgCardPower(s.YourHand[strongest]) {
			strongest = i
		}
	}

	if tricks >= bid { // 목표를 이미 채웠다 — 가장 약한 카드를 버린다
		return weakest
	}
	if len(s.Trick) == 0 { // 리드 — 가장 강한 카드로 밀어붙인다
		return strongest
	}

	best := -1
	for _, i := range legal {
		if !kgBotWins(s.Trick, s.LeadSuit, me, s.YourHand[i]) {
			continue
		}
		if best < 0 || kgCardPower(s.YourHand[i]) < kgCardPower(s.YourHand[best]) {
			best = i
		}
	}
	if best >= 0 {
		return best // 최소한으로 이긴다
	}
	return weakest
}

// kgBotWins 지금 이 카드를 내면 현재까지의 트릭을 가져가는지 (후행 좌석은
// 아직 모르므로 어림값이다)
func kgBotWins(trick []KGTrickPlay, leadSuit KGSuit, seat int, card KGCard) bool {
	plays := append(append([]KGTrickPlay{}, trick...), KGTrickPlay{Seat: seat, Card: card})
	lead := leadSuit
	if lead == KGSuitNone && card.Kind == KGKindNumber {
		lead = card.Suit
	}
	return kgTrickWinner(plays, lead) == len(plays)-1
}

// ==================== 봇 소환 ====================

// spawnKGBot 대기실의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *KGHub) spawnKGBot(room *kgRoom, name string) bool {
	bot := &KGClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runKGBot(bot)
	return true
}

// takeoverKGBot 유예 만료 좌석을 이어받는 연습봇 — 이름·좌석을 유지해
// 진행 중인 차례가 그대로 이어진다
func (h *KGHub) takeoverKGBot(room *kgRoom, seat int, name string) *KGClient {
	bot := &KGClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	bot.GameID = room.Game.ID
	bot.Seat = seat
	h.runKGBot(bot)
	return bot
}

// runKGBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *KGHub) runKGBot(bot *KGClient) {
	brain := newKGBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m KGMessage) { h.gameMessage <- KGGameMessage{Client: bot, Message: m} },
		func(m KGMessage) bool { return m.Type == KGMsgGameOver || m.Type == KGMsgSessionExpired })
}

// kgRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록용)
func kgRoomHasBot(room *kgRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
