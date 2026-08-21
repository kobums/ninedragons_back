package server

import (
	"math/rand"
	"time"
)

// ==================== 인디언 포커 연습봇 ====================
//
// 스냅샷(ip_game_state)만 보고 반응한다. 봇도 사람과 같은 조건 — 자기
// 카드는 스냅샷에서 0으로 가려져 모르고, 보이는 상대 카드로만 판단한다.
// 보이는 상대 최고 카드 M 기준:
//   M ≤ 5  → 60% 레이즈(+1) / 40% 콜  (상대가 약하다 — 공격)
//   M ≤ 7  → 70% 콜 / 30% 폴드
//   M ≥ 8  → 40% 콜 / 60% 폴드        (상대가 강하다 — 접는다)
// 레이즈 상한·칩 보유량을 지키고, 콜 비용 0(첫 베팅)이면 폴드 대신 콜(체크).

// ipBotPlayerView 봇이 스냅샷에서 꺼내 쓰는 좌석 정보
type ipBotPlayerView struct {
	Seat         int  `json:"seat"`
	Alive        bool `json:"alive"`
	Folded       bool `json:"folded"`
	Chips        int  `json:"chips"`
	BetThisRound int  `json:"betThisRound"`
	Card         int  `json:"card"`
}

// ipBotState 봇이 상태 스냅샷에서 꺼내 쓰는 최소 정보
type ipBotState struct {
	YourSeat    int               `json:"yourSeat"`
	Phase       IPPhase           `json:"phase"`
	CurrentSeat int               `json:"currentSeat"`
	CurrentBet  int               `json:"currentBet"`
	RaisesLeft  int               `json:"raisesLeft"`
	Players     []ipBotPlayerView `json:"players"`
}

// ipBrain 스냅샷 기반 판단 (봇 대체 좌석도 같은 두뇌를 쓴다)
type ipBrain struct {
	rng *rand.Rand
}

func newIPBrain() *ipBrain {
	return &ipBrain{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

// decide 공용 러너 계약 — ip_game_state 에만 반응한다
func (b *ipBrain) decide(msg IPMessage) *IPMessage {
	if msg.Type != IPMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[ipBotState](msg.Payload)
	if !ok {
		return nil
	}
	return b.decideState(state)
}

// decideState 자기 차례면 콜/레이즈/폴드를 고른다 (아니면 nil)
func (b *ipBrain) decideState(state ipBotState) *IPMessage {
	me := state.YourSeat
	if state.Phase != IPPhaseBetting || state.CurrentSeat != me {
		return nil
	}

	var mine *ipBotPlayerView
	maxSeen := 0 // 보이는 상대 최고 카드 M
	for i := range state.Players {
		p := &state.Players[i]
		if p.Seat == me {
			mine = p
			continue
		}
		if p.Alive && !p.Folded && p.Card > maxSeen {
			maxSeen = p.Card
		}
	}
	if mine == nil {
		return nil
	}
	callCost := state.CurrentBet - mine.BetThisRound
	if callCost < 0 {
		callCost = 0
	}

	const (
		actCall = iota
		actRaise
		actFold
	)
	act := actCall
	r := b.rng.Float64()
	switch {
	case maxSeen <= 5:
		if r < 0.6 {
			act = actRaise
		}
	case maxSeen <= 7:
		if r >= 0.7 {
			act = actFold
		}
	default:
		if r >= 0.4 {
			act = actFold
		}
	}

	// 첫 베팅(콜 비용 0)은 폴드 대신 콜(체크)
	if act == actFold && callCost == 0 {
		act = actCall
	}
	// 레이즈 상한·칩 준수 — 못 올리면 콜로 강등
	if act == actRaise && (state.RaisesLeft <= 0 || mine.Chips < callCost+1) {
		act = actCall
	}

	switch act {
	case actRaise:
		return &IPMessage{Type: IPMsgRaise, Payload: IPRaisePayload{Amount: 1}}
	case actFold:
		return &IPMessage{Type: IPMsgFold}
	}
	return &IPMessage{Type: IPMsgCall} // 콜은 언제나 합법 (부족하면 올인 콜)
}

// ==================== 봇 소환 ====================

// spawnIPBot 대기실의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *IPHub) spawnIPBot(room *ipRoom, name string) bool {
	bot := &IPClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runIPBot(bot)
	return true
}

// runIPBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *IPHub) runIPBot(bot *IPClient) {
	brain := newIPBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m IPMessage) { h.gameMessage <- IPGameMessage{Client: bot, Message: m} },
		func(m IPMessage) bool { return m.Type == IPMsgGameOver || m.Type == IPMsgSessionExpired })
}

// ipRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록용)
func ipRoomHasBot(room *ipRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
