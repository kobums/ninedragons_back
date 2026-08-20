package server

import (
	"math/rand"
	"time"
)

// ==================== 러브레터 연습봇 ====================
//
// 스냅샷(lv_game_state)만 보고 반응한다. 백작부인 강제만 준수하고 그 외는
// 무작위 합법 수 — 낼 카드 무작위(공주는 가능하면 내지 않음), 대상은 보호
// 중이 아닌 무작위 생존자, 경비병 추측은 2~8 무작위. 서버 검증과 같은
// 대상 규칙을 따르므로 봇의 수는 항상 통과한다.

// lvBotState 봇이 상태 스냅샷에서 꺼내 쓰는 최소 정보
type lvBotState struct {
	YourSeat    int     `json:"yourSeat"`
	Phase       LVPhase `json:"phase"`
	CurrentSeat int     `json:"currentSeat"`
	YourHand    []int   `json:"yourHand"`
	Players     []struct {
		Seat      int  `json:"seat"`
		Alive     bool `json:"alive"`
		Protected bool `json:"protected"`
	} `json:"players"`
}

// lvBrain 스냅샷 기반 무작위 판단 (AFK 자동 진행도 같은 두뇌를 쓴다)
type lvBrain struct {
	rng *rand.Rand
}

func newLVBrain() *lvBrain {
	return &lvBrain{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

// decide 공용 러너 계약 — lv_game_state 에만 반응한다
func (b *lvBrain) decide(msg LVMessage) *LVMessage {
	if msg.Type != LVMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[lvBotState](msg.Payload)
	if !ok {
		return nil
	}
	return b.decideState(state)
}

// decideState 자기 턴이면 낼 카드·대상·추측을 고른다 (아니면 nil)
func (b *lvBrain) decideState(state lvBotState) *LVMessage {
	me := state.YourSeat
	if state.Phase != LVPhasePlaying || state.CurrentSeat != me || len(state.YourHand) < 2 {
		return nil
	}
	hand := state.YourHand

	// 백작부인 강제: 왕/왕자와 함께면 반드시 백작부인
	card := 0
	if lvContains(hand, LVCountess) && (lvContains(hand, LVKing) || lvContains(hand, LVPrince)) {
		card = LVCountess
	} else {
		// 공주는 가능하면 내지 않는다 (다른 카드 우선)
		candidates := []int{}
		for _, c := range hand {
			if c != LVPrincess {
				candidates = append(candidates, c)
			}
		}
		if len(candidates) == 0 {
			candidates = append([]int{}, hand...)
		}
		card = candidates[b.rng.Intn(len(candidates))]
	}

	payload := LVPlayPayload{Card: card}

	// 대상 선택 — 보호 중이 아닌 무작위 생존자 (왕자는 자신 포함)
	needsTarget := card == LVGuard || card == LVPriest || card == LVBaron ||
		card == LVPrince || card == LVKing
	if needsTarget {
		targets := []int{}
		for _, p := range state.Players {
			if !p.Alive {
				continue
			}
			if p.Seat == me {
				if card == LVPrince {
					targets = append(targets, p.Seat)
				}
				continue
			}
			if p.Protected {
				continue
			}
			targets = append(targets, p.Seat)
		}
		if len(targets) > 0 {
			target := targets[b.rng.Intn(len(targets))]
			payload.TargetSeat = &target
			if card == LVGuard {
				payload.Guess = 2 + b.rng.Intn(7) // 2~8 무작위
			}
		}
		// 대상이 없으면 효과 없는 카드로 낸다 (서버가 허용)
	}

	return &LVMessage{Type: LVMsgPlay, Payload: payload}
}

// ==================== 봇 소환 ====================

// spawnLVBot 대기실의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *LVHub) spawnLVBot(room *lvRoom, name string) bool {
	bot := &LVClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runLVBot(bot)
	return true
}

// runLVBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *LVHub) runLVBot(bot *LVClient) {
	brain := newLVBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m LVMessage) { h.gameMessage <- LVGameMessage{Client: bot, Message: m} },
		func(m LVMessage) bool { return m.Type == LVMsgGameOver || m.Type == LVMsgSessionExpired })
}

// lvRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록용)
func lvRoomHasBot(room *lvRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
