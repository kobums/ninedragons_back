package server

import (
	"fmt"
	"math/rand"
	"time"
)

// ==================== 노 터치 크라켄 연습봇 ====================
//
// 스냅샷(kr_game_state)만 보고 반응한다. 봇도 사람과 같은 조건 — 자기
// yourRole·yourHand 만 알고 남의 손패·정체는 모른다 (해골 봇도 다른 해골이
// 누구인지 모른다. 원작 규칙과 같다).
//   - 지목: 미공개 카드가 있는 타 좌석 중 무작위 → 그 좌석 카드 인덱스 무작위.
//     탐험대 봇만 약한 보정 — 보물을 선언한 좌석을 30% 더 자주 고른다.
//   - 선언: 라운드마다 자기 차례가 처음 오면 한 번 선언한다. 탐험대는 정직,
//     해골은 60% 정직 / 40% 거짓. 거짓 해골이 크라켄을 쥐고 있으면 "보물 1"
//     식으로 섞어 크라켄을 감춘다.
// 같은 대기 상태에 스냅샷이 여러 번 와도(관전 입장·접속 변화·타인 선언 등)
// 한 번만 지목하도록 상태 식별키로 중복을 걸러낸다.

// 봇이 "생각하는" 시간 (테스트에서 짧게 낮춘다)
var (
	krBotClaimDelay    = 600 * time.Millisecond
	krBotClaimJitterMs = 600
	krBotPointDelay    = 900 * time.Millisecond
	krBotPointJitterMs = 900
)

// krBotTreasureBias 탐험대 봇이 "보물 선언" 좌석을 더 자주 고르는 가중치
// (1.0 대비 1.3 = 30% 더 자주)
const krBotTreasureBias = 1.3

// krBotPlayerView 봇이 스냅샷에서 꺼내 쓰는 좌석 정보
type krBotPlayerView struct {
	Seat      int      `json:"seat"`
	HandCount int      `json:"handCount"`
	Claim     *KRClaim `json:"claim"`
}

// krBotState 봇이 상태 스냅샷에서 꺼내 쓰는 최소 정보
type krBotState struct {
	YourSeat    int               `json:"yourSeat"`
	Phase       KRPhase           `json:"phase"`
	Round       int               `json:"round"`
	RevealsLeft int               `json:"revealsLeft"`
	CurrentSeat int               `json:"currentSeat"`
	YourRole    string            `json:"yourRole"`
	YourHand    []KRCard          `json:"yourHand"`
	Players     []krBotPlayerView `json:"players"`
}

// krBrain 스냅샷 기반 판단 (봇 대체 좌석도 같은 두뇌를 쓴다)
type krBrain struct {
	rng *rand.Rand
	// lastKey 마지막으로 지목한 대기 상태 식별키 (중복 지목 방지)
	lastKey string
	// claimedRound 이미 선언을 마친 라운드 (라운드마다 한 번만 선언한다)
	claimedRound int
}

func newKRBrain() *krBrain {
	return &krBrain{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

// decide 공용 러너 계약 — kr_game_state 에만 반응한다
func (b *krBrain) decide(msg KRMessage) *KRMessage {
	if msg.Type != KRMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[krBotState](msg.Payload)
	if !ok {
		return nil
	}
	return b.decideState(state)
}

// handled 같은 대기 상태에 이미 지목했는지 — 처음이면 키를 기록한다
func (b *krBrain) handled(key string) bool {
	if b.lastKey == key {
		return true
	}
	b.lastKey = key
	return false
}

// think 사람처럼 잠깐 뜸을 들인다 (테스트에서는 var 를 낮춰 즉시 진행한다)
func (b *krBrain) think(base time.Duration, jitterMs int) {
	d := base
	if jitterMs > 0 {
		d += time.Duration(b.rng.Intn(jitterMs)) * time.Millisecond
	}
	if d > 0 {
		time.Sleep(d)
	}
}

func (b *krBrain) decideState(s krBotState) *KRMessage {
	me := s.YourSeat
	if me < 0 || me >= len(s.Players) {
		return nil
	}
	if s.Phase != KRPhaseRevealing || s.CurrentSeat != me {
		return nil
	}

	// 자기 차례가 오면 라운드에 한 번 손패를 선언하고 (거짓 가능) 지목한다
	if b.claimedRound != s.Round {
		b.claimedRound = s.Round
		if claim := b.buildClaim(s); claim != nil {
			b.think(krBotClaimDelay, krBotClaimJitterMs)
			return &KRMessage{Type: KRMsgClaim, Payload: *claim}
		}
	}

	key := fmt.Sprintf("%s|%d|%d|%d", s.Phase, s.Round, s.RevealsLeft, s.CurrentSeat)
	if b.handled(key) {
		return nil
	}
	target, index := b.pickPoint(s)
	if target < 0 {
		return nil
	}
	b.think(krBotPointDelay, krBotPointJitterMs)
	return &KRMessage{Type: KRMsgPoint,
		Payload: KRPointPayload{TargetSeat: target, CardIndex: index}}
}

// buildClaim 손패 선언 — 탐험대는 정직, 해골은 40% 거짓.
// 거짓 해골이 크라켄을 쥐고 있으면 "보물 1" 식으로 섞어 크라켄을 감춘다.
func (b *krBrain) buildClaim(s krBotState) *KRClaimPayload {
	hand := len(s.YourHand)
	if hand == 0 {
		return nil
	}
	treasure, kraken := 0, 0
	for _, c := range s.YourHand {
		switch c {
		case KRCardTreasure:
			treasure++
		case KRCardKraken:
			kraken = 1
		}
	}
	// 해골 봇의 40% 거짓 (탐험대는 언제나 정직 선언)
	if s.YourRole == string(KRRoleSkeleton) && b.rng.Float64() < 0.4 {
		if kraken == 1 {
			treasure, kraken = 1, 0 // 크라켄을 감추고 보물 1장이라 우긴다
		} else if treasure < hand {
			treasure++ // 없는 보물을 하나 더 있다고 우긴다
		}
	}
	if treasure+kraken > hand {
		treasure = hand - kraken
	}
	if treasure < 0 {
		treasure = 0
	}
	return &KRClaimPayload{Treasure: treasure, Kraken: kraken}
}

// pickPoint 지목 대상·카드 선택 — 미공개 카드가 있는 타 좌석 중 무작위.
// 탐험대 봇만 보물 선언 좌석에 30% 가중치를 더 준다 (약한 보정).
func (b *krBrain) pickPoint(s krBotState) (int, int) {
	type cand struct {
		seat   int
		cards  int
		weight float64
	}
	cands := []cand{}
	total := 0.0
	for _, p := range s.Players {
		if p.Seat == s.YourSeat || p.HandCount <= 0 {
			continue
		}
		w := 1.0
		if s.YourRole == string(KRRoleExpedition) && p.Claim != nil && p.Claim.Treasure > 0 {
			w = krBotTreasureBias
		}
		cands = append(cands, cand{seat: p.Seat, cards: p.HandCount, weight: w})
		total += w
	}
	if len(cands) == 0 {
		return -1, -1
	}
	roll := b.rng.Float64() * total
	for _, c := range cands {
		roll -= c.weight
		if roll <= 0 {
			return c.seat, b.rng.Intn(c.cards)
		}
	}
	last := cands[len(cands)-1]
	return last.seat, b.rng.Intn(last.cards)
}

// ==================== 봇 소환 ====================

// spawnKRBot 대기실의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *KRHub) spawnKRBot(room *krRoom, name string) bool {
	bot := &KRClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runKRBot(bot)
	return true
}

// takeoverKRBot 유예 만료 좌석을 이어받는 연습봇 — 이름·좌석을 유지해
// 진행 중인 지목권이 그대로 이어진다
func (h *KRHub) takeoverKRBot(room *krRoom, seat int, name string) *KRClient {
	bot := &KRClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	bot.GameID = room.Game.ID
	bot.Seat = seat
	h.runKRBot(bot)
	return bot
}

// runKRBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *KRHub) runKRBot(bot *KRClient) {
	brain := newKRBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m KRMessage) { h.gameMessage <- KRGameMessage{Client: bot, Message: m} },
		func(m KRMessage) bool { return m.Type == KRMsgGameOver || m.Type == KRMsgSessionExpired })
}

// krRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록용)
func krRoomHasBot(room *krRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
