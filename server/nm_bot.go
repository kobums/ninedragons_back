package server

import (
	"math/rand"
	"time"
)

// ==================== 6 님트 연습봇 ====================
//
// 스냅샷(nm_game_state)만 보고 반응한다. 연습용 — 무작위 합법 행동만 한다.
//   - picking: 아직 제출하지 않았으면 손패에서 무작위 1장
//   - choosing_row: 내가 chooser 면 소머리 합 최소 행 (동률은 낮은 인덱스)
// 스냅샷이 겹쳐 와 중복 제출이 나가도 서버 검증(이미 선택)이 조용히 걸러낸다
// (봇은 nm_error 에 반응하지 않으므로 반복 루프가 생기지 않는다).

// nmBotState 봇이 스냅샷에서 꺼내 쓰는 최소 정보
type nmBotState struct {
	Phase       NMPhase `json:"phase"`
	YourSeat    int     `json:"yourSeat"`
	ChooserSeat int     `json:"chooserSeat"`
	YourHand    []int   `json:"yourHand"`
	Rows        [][]int `json:"rows"`
	Players     []struct {
		Seat   int  `json:"seat"`
		Picked bool `json:"picked"`
	} `json:"players"`
}

// nmBrain 스냅샷 기반 판단 (봇 대체 좌석도 같은 두뇌를 쓴다)
type nmBrain struct {
	rng *rand.Rand
}

func newNMBrain() *nmBrain {
	return &nmBrain{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

// decide 공용 러너 계약 — nm_game_state 에만 반응한다
func (b *nmBrain) decide(msg NMMessage) *NMMessage {
	if msg.Type != NMMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[nmBotState](msg.Payload)
	if !ok {
		return nil
	}
	return b.act(state)
}

func (b *nmBrain) act(state nmBotState) *NMMessage {
	me := state.YourSeat
	if me < 0 {
		return nil // 관전 시점은 구경만
	}

	switch state.Phase {
	case NMPhasePicking:
		// 아직 제출하지 않았으면 손패에서 무작위 1장
		for _, p := range state.Players {
			if p.Seat == me && p.Picked {
				return nil
			}
		}
		if len(state.YourHand) == 0 {
			return nil
		}
		card := state.YourHand[b.rng.Intn(len(state.YourHand))]
		return &NMMessage{Type: NMMsgPick, Payload: NMPickPayload{Card: card}}

	case NMPhaseChoosingRow:
		if state.ChooserSeat != me {
			return nil
		}
		// 소머리 합 최소 행 (동률은 낮은 인덱스)
		best, bestHeads := 0, 1<<30
		for r, row := range state.Rows {
			if h := nmRowHeads(row); h < bestHeads {
				best, bestHeads = r, h
			}
		}
		return &NMMessage{Type: NMMsgChooseRow, Payload: NMChooseRowPayload{Row: best}}
	}
	return nil
}

// ==================== 봇 소환 ====================

// spawnNMBot 대기실의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *NMHub) spawnNMBot(room *nmRoom, name string) bool {
	bot := &NMClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runNMBot(bot)
	return true
}

// runNMBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *NMHub) runNMBot(bot *NMClient) {
	brain := newNMBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m NMMessage) { h.gameMessage <- NMGameMessage{Client: bot, Message: m} },
		func(m NMMessage) bool { return m.Type == NMMsgGameOver || m.Type == NMMsgSessionExpired })
}

// nmRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록용)
func nmRoomHasBot(room *nmRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
