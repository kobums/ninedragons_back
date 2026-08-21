package server

import (
	"fmt"
	"math/rand"
	"time"
)

// ==================== 차오차오 연습봇 ====================
//
// 스냅샷(cc_game_state)만 보고 반응한다. 봇도 사람과 같은 조건 — 자기 차례의
// 주사위 값(yourRoll)만 알고 남의 주사위는 모른다.
//   - 선언: X 면 2~3 무작위(강제 거짓), 숫자면 80% 정직 / 20% +1 과장(상한 4)
//   - 의심: 선언 4 는 40%, 3 은 20%, 그 외 10% — 아니면 허용
// 같은 창에 스냅샷이 여러 번 와도(허용 쌓임·관전 입장 등) 한 번만 응답하도록
// 대기 상태 식별키(phase+endsAt+currentSeat+declared)로 중복을 걸러낸다.

// ccBotPlayerView 봇이 스냅샷에서 꺼내 쓰는 좌석 정보
type ccBotPlayerView struct {
	Seat  int  `json:"seat"`
	Alive bool `json:"alive"`
}

// ccBotState 봇이 상태 스냅샷에서 꺼내 쓰는 최소 정보
type ccBotState struct {
	YourSeat    int               `json:"yourSeat"`
	Phase       CCPhase           `json:"phase"`
	CurrentSeat int               `json:"currentSeat"`
	EndsAt      int64             `json:"endsAt"`
	YourRoll    *int              `json:"yourRoll"`
	Declared    int               `json:"declared"`
	Players     []ccBotPlayerView `json:"players"`
}

// ccBrain 스냅샷 기반 판단 (봇 대체 좌석도 같은 두뇌를 쓴다)
type ccBrain struct {
	rng *rand.Rand
	// lastKey 마지막으로 응답한 대기 상태 식별키 (중복 응답 방지)
	lastKey string
}

func newCCBrain() *ccBrain {
	return &ccBrain{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

// decide 공용 러너 계약 — cc_game_state 에만 반응한다
func (b *ccBrain) decide(msg CCMessage) *CCMessage {
	if msg.Type != CCMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[ccBotState](msg.Payload)
	if !ok {
		return nil
	}
	return b.decideState(state)
}

// handled 같은 대기 상태에 이미 응답했는지 — 처음이면 키를 기록한다
func (b *ccBrain) handled(key string) bool {
	if b.lastKey == key {
		return true
	}
	b.lastKey = key
	return false
}

func (b *ccBrain) decideState(s ccBotState) *CCMessage {
	me := s.YourSeat
	if me < 0 || me >= len(s.Players) || !s.Players[me].Alive {
		return nil
	}
	key := fmt.Sprintf("%s|%d|%d|%d", s.Phase, s.EndsAt, s.CurrentSeat, s.Declared)

	switch s.Phase {
	case CCPhaseRolling:
		if s.CurrentSeat != me || s.YourRoll == nil || b.handled(key) {
			return nil
		}
		return &CCMessage{Type: CCMsgDeclare,
			Payload: CCDeclarePayload{Value: b.chooseDeclare(*s.YourRoll)}}

	case CCPhaseDoubtWindow:
		if s.CurrentSeat == me || b.handled(key) {
			return nil
		}
		if b.rng.Float64() < ccDoubtChance(s.Declared) {
			return &CCMessage{Type: CCMsgDoubt}
		}
		return &CCMessage{Type: CCMsgAllow}
	}
	return nil
}

// chooseDeclare 선언 값 — X 는 2~3 무작위 거짓, 숫자는 80% 정직 / 20% 과장
func (b *ccBrain) chooseDeclare(roll int) int {
	if roll == CCRollX {
		return 2 + b.rng.Intn(2)
	}
	if b.rng.Float64() < 0.8 {
		return roll
	}
	if roll < 4 {
		return roll + 1
	}
	return 4
}

// ccDoubtChance 선언 값별 의심 확률 — 큰 선언일수록 의심스럽다
func ccDoubtChance(declared int) float64 {
	switch declared {
	case 4:
		return 0.4
	case 3:
		return 0.2
	}
	return 0.1
}

// ==================== 봇 소환 ====================

// spawnCCBot 대기실의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *CCHub) spawnCCBot(room *ccRoom, name string) bool {
	bot := &CCClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runCCBot(bot)
	return true
}

// takeoverCCBot 유예 만료 좌석을 이어받는 연습봇 — 이름·좌석을 유지해
// 진행 중인 선언·의심 창 허용이 그대로 이어진다
func (h *CCHub) takeoverCCBot(room *ccRoom, seat int, name string) *CCClient {
	bot := &CCClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	bot.GameID = room.Game.ID
	bot.Seat = seat
	h.runCCBot(bot)
	return bot
}

// runCCBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *CCHub) runCCBot(bot *CCClient) {
	brain := newCCBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m CCMessage) { h.gameMessage <- CCGameMessage{Client: bot, Message: m} },
		func(m CCMessage) bool { return m.Type == CCMsgGameOver || m.Type == CCMsgSessionExpired })
}

// ccRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록용)
func ccRoomHasBot(room *ccRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
