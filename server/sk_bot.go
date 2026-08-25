package server

import (
	"math/rand"
	"time"
)

// ==================== 스컬 연습봇 ====================
//
// 연습용 — 무작위 합법 행동만 한다.
//   - 배치: 첫 장은 무작위, 이후 차례에는 60% 추가 배치(무작위 카드)·
//     40% 베팅(1~자기 더미 수). 손패가 없으면 무조건 베팅.
//   - 베팅: 30% 레이즈(+1, 상한 내), 70% 패스.
//   - 뒤집기: 자기 더미(서버가 자동으로 먼저 뒤집음) 후 무작위 상대 더미.
// 검증 규칙은 서버와 같은 sk_game.go 를 공유하므로 봇이 서버 검증에 걸리는
// 수를 내지 않는다.

// skBotState 봇이 쓰는 최소 스냅샷
type skBotState struct {
	Phase          string   `json:"phase"`
	YourSeat       int      `json:"yourSeat"`
	CurrentSeat    int      `json:"currentSeat"`
	YourHand       []string `json:"yourHand"`
	YourStack      []string `json:"yourStack"`
	HighBid        int      `json:"highBid"`
	ChallengerSeat int      `json:"challengerSeat"`
	Players        []struct {
		Seat       int  `json:"seat"`
		Alive      bool `json:"alive"`
		StackCount int  `json:"stackCount"`
		Passed     bool `json:"passed"`
	} `json:"players"`
}

// skBrain 스컬 봇 두뇌 (무작위 — 자체 난수원)
type skBrain struct {
	rng *rand.Rand
}

func newSKBrain() *skBrain {
	return &skBrain{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

func (b *skBrain) decide(msg SKMessage) *SKMessage {
	if msg.Type != SKMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[skBotState](msg.Payload)
	if !ok {
		return nil
	}
	return b.act(state)
}

func (b *skBrain) act(state skBotState) *SKMessage {
	me := state.YourSeat
	if me < 0 || !b.isAlive(state, me) {
		return nil // 탈락자·관전 시점은 구경만
	}

	switch state.Phase {
	case string(SKPhasePlacing):
		if state.CurrentSeat == -1 {
			// 동시 배치 — 아직 안 내려놨으면 무작위 1장
			if len(state.YourStack) == 0 && len(state.YourHand) > 0 {
				return b.place(len(state.YourHand))
			}
			return nil
		}
		if state.CurrentSeat != me {
			return nil
		}
		// 턴제 파트 — 60% 추가 배치, 40%(또는 손패 소진 시) 베팅
		if len(state.YourHand) > 0 && b.rng.Intn(10) < 6 {
			return b.place(len(state.YourHand))
		}
		maxBid := len(state.YourStack)
		if maxBid < 1 {
			maxBid = 1
		}
		return &SKMessage{Type: SKMsgBid, Payload: SKBidPayload{Count: 1 + b.rng.Intn(maxBid)}}

	case string(SKPhaseBidding):
		if state.CurrentSeat != me {
			return nil
		}
		total := 0
		for _, p := range state.Players {
			total += p.StackCount
		}
		if b.rng.Intn(10) < 3 && state.HighBid+1 <= total {
			return &SKMessage{Type: SKMsgBid, Payload: SKBidPayload{Count: state.HighBid + 1}}
		}
		return &SKMessage{Type: SKMsgPass}

	case string(SKPhaseFlipping):
		if state.ChallengerSeat != me {
			return nil
		}
		cands := []int{}
		for _, p := range state.Players {
			if p.Seat != me && p.StackCount > 0 {
				cands = append(cands, p.Seat)
			}
		}
		if len(cands) == 0 {
			return nil
		}
		return &SKMessage{Type: SKMsgFlip, Payload: SKFlipPayload{Seat: cands[b.rng.Intn(len(cands))]}}
	}
	return nil
}

func (b *skBrain) place(handLen int) *SKMessage {
	return &SKMessage{Type: SKMsgPlace, Payload: SKPlacePayload{Index: b.rng.Intn(handLen)}}
}

func (b *skBrain) isAlive(state skBotState, seat int) bool {
	for _, p := range state.Players {
		if p.Seat == seat {
			return p.Alive
		}
	}
	return false
}

// ==================== 봇 소환 ====================

// spawnBot 대기 방의 남은 자리에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *SKHub) spawnBot(room *skRoom, name string) *SKClient {
	bot := &SKClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return nil
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.runSKBot(bot)
	return bot
}

// takeoverBot 유예 만료 좌석을 이어받는 연습봇 — 이름·좌석·손패를 유지해
// 진행 중인 배치·베팅·뒤집기가 그대로 이어진다
func (h *SKHub) takeoverBot(room *skRoom, seat int, name string) *SKClient {
	bot := &SKClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	bot.GameID = room.Game.ID
	bot.Seat = seat
	h.runSKBot(bot)
	return bot
}

// runSKBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *SKHub) runSKBot(bot *SKClient) {
	brain := newSKBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m SKMessage) { h.gameMessage <- SKGameMessage{Client: bot, Message: m} },
		func(m SKMessage) bool { return m.Type == SKMsgGameOver || m.Type == SKMsgSessionExpired })
}
