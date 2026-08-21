package server

import (
	"fmt"
	"math/rand"
	"time"
)

// ==================== 바퀴벌레 포커 연습봇 ====================
//
// 스냅샷(cr_game_state)만 보고 반응한다. 봇도 사람과 같은 조건 — 자기
// 손패(yourHand)와 자기에게 온 cr_peek(릴레이 실물)만 알고 남의 손패는
// 모른다.
//   - 전달: 무작위 카드·무작위 대상, 60% 실물 선언 / 40% 거짓 선언
//   - 결정: 55% 판정(참/거짓 반반) / 45% 넘기기(가능할 때 — 실물은 cr_peek
//     로 이미 받았고, 넘길 때의 선언도 60% 실물 / 40% 거짓)
// 같은 대기 상태에 스냅샷이 여러 번 와도(관전 입장 등) 한 번만 응답하도록
// 대기 상태 식별키(phase+endsAt+passer+holder+체인 길이)로 중복을 걸러낸다.

// crBotPlayerView 봇이 스냅샷에서 꺼내 쓰는 좌석 정보
type crBotPlayerView struct {
	Seat int `json:"seat"`
}

// crBotState 봇이 상태 스냅샷에서 꺼내 쓰는 최소 정보
type crBotState struct {
	YourSeat   int               `json:"yourSeat"`
	Phase      CRPhase           `json:"phase"`
	EndsAt     int64             `json:"endsAt"`
	PasserSeat int               `json:"passerSeat"`
	HolderSeat int               `json:"holderSeat"`
	Chain      []int             `json:"chain"`
	YourHand   []string          `json:"yourHand"`
	Players    []crBotPlayerView `json:"players"`
}

// crBrain 스냅샷 기반 판단 (봇 대체 좌석도 같은 두뇌를 쓴다)
type crBrain struct {
	rng *rand.Rand
	// lastKey 마지막으로 응답한 대기 상태 식별키 (중복 응답 방지)
	lastKey string
	// peek 마지막으로 받은 릴레이 실물 (cr_peek) — 넘길 때의 선언 근거
	peek string
}

func newCRBrain() *crBrain {
	return &crBrain{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

// decide 공용 러너 계약 — cr_peek 은 실물을 기억만 하고, cr_game_state 에만
// 응답한다
func (b *crBrain) decide(msg CRMessage) *CRMessage {
	if msg.Type == CRMsgPeek {
		if p, ok := botPayloadAs[CRPeekPayload](msg.Payload); ok {
			b.peek = p.Animal
		}
		return nil
	}
	if msg.Type != CRMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[crBotState](msg.Payload)
	if !ok {
		return nil
	}
	return b.decideState(state)
}

// handled 같은 대기 상태에 이미 응답했는지 — 처음이면 키를 기록한다
func (b *crBrain) handled(key string) bool {
	if b.lastKey == key {
		return true
	}
	b.lastKey = key
	return false
}

func (b *crBrain) decideState(s crBotState) *CRMessage {
	me := s.YourSeat
	if me < 0 || me >= len(s.Players) {
		return nil
	}
	key := fmt.Sprintf("%s|%d|%d|%d|%d", s.Phase, s.EndsAt, s.PasserSeat, s.HolderSeat, len(s.Chain))

	switch s.Phase {
	case CRPhasePassing:
		if s.PasserSeat != me || len(s.YourHand) == 0 || b.handled(key) {
			return nil
		}
		card := s.YourHand[b.rng.Intn(len(s.YourHand))]
		target := b.randomTarget(s, nil)
		if target < 0 {
			return nil
		}
		return &CRMessage{Type: CRMsgPassCard, Payload: CRPassCardPayload{
			Card: card, TargetSeat: target, Claim: b.claimFor(card)}}

	case CRPhaseDeciding:
		if s.HolderSeat != me || b.handled(key) {
			return nil
		}
		// 넘기기 45% — 카드를 안 본 사람이 남아 있을 때만
		if target := b.randomTarget(s, s.Chain); target >= 0 && b.rng.Float64() < 0.45 {
			base := b.peek
			if base == "" { // 방어 — peek 을 못 받았으면 무작위 동물 기준
				base = string(crAllAnimals[b.rng.Intn(len(crAllAnimals))])
			}
			return &CRMessage{Type: CRMsgRelay, Payload: CRRelayPayload{
				TargetSeat: target, Claim: b.claimFor(base)}}
		}
		// 판정 — 참/거짓 반반
		return &CRMessage{Type: CRMsgJudge, Payload: CRJudgePayload{Truth: b.rng.Intn(2) == 0}}
	}
	return nil
}

// randomTarget 나와 exclude(체인)를 제외한 무작위 좌석 (-1 = 후보 없음)
func (b *crBrain) randomTarget(s crBotState, exclude []int) int {
	excluded := map[int]bool{s.YourSeat: true}
	for _, seat := range exclude {
		excluded[seat] = true
	}
	candidates := []int{}
	for _, p := range s.Players {
		if !excluded[p.Seat] {
			candidates = append(candidates, p.Seat)
		}
	}
	if len(candidates) == 0 {
		return -1
	}
	return candidates[b.rng.Intn(len(candidates))]
}

// claimFor 실물 기준 선언 — 60% 실물 그대로 / 40% 다른 동물로 거짓
func (b *crBrain) claimFor(card string) string {
	if b.rng.Float64() < 0.6 {
		return card
	}
	for {
		lie := string(crAllAnimals[b.rng.Intn(len(crAllAnimals))])
		if lie != card {
			return lie
		}
	}
}

// ==================== 봇 소환 ====================

// spawnCRBot 대기실의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *CRHub) spawnCRBot(room *crRoom, name string) bool {
	bot := &CRClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runCRBot(bot)
	return true
}

// takeoverCRBot 유예 만료 좌석을 이어받는 연습봇 — 이름·좌석을 유지해
// 진행 중인 전달·판정이 그대로 이어진다
func (h *CRHub) takeoverCRBot(room *crRoom, seat int, name string) *CRClient {
	bot := &CRClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	bot.GameID = room.Game.ID
	bot.Seat = seat
	h.runCRBot(bot)
	return bot
}

// runCRBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *CRHub) runCRBot(bot *CRClient) {
	brain := newCRBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m CRMessage) { h.gameMessage <- CRGameMessage{Client: bot, Message: m} },
		func(m CRMessage) bool { return m.Type == CRMsgGameOver || m.Type == CRMsgSessionExpired })
}

// crRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록용)
func crRoomHasBot(room *crRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
