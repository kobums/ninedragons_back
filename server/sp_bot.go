package server

import (
	"math/rand"
	"time"
)

// ==================== 스파이폴 연습봇 ====================
//
// 연습용 — 결정적 완주가 목표다. playing 중에는 아무것도 하지 않는다
// (스파이 봇도 추리하지 않고 타이머를 기다린다). voting 이 오면 자기를 뺀
// 무작위 한 명을 지목한다. 검증 규칙은 서버와 같은 sp_game.go 를 공유하므로
// 봇이 서버 검증에 걸리는 수를 내지 않는다.

// spBotState 봇이 쓰는 최소 스냅샷 — 은닉 계약대로 스파이 정보 없이도 돈다
type spBotState struct {
	Phase    string `json:"phase"`
	YourSeat int    `json:"yourSeat"`
	Players  []struct {
		Seat int `json:"seat"`
	} `json:"players"`
	Votes []SPVoteView `json:"votes"`
}

// spBrain 스파이폴 봇 두뇌 (무작위 — 자체 난수원)
type spBrain struct {
	rng *rand.Rand
}

func newSPBrain() *spBrain {
	return &spBrain{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

func (b *spBrain) decide(msg SPMessage) *SPMessage {
	if msg.Type != SPMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[spBotState](msg.Payload)
	if !ok {
		return nil
	}
	return b.act(state)
}

func (b *spBrain) act(state spBotState) *SPMessage {
	if state.Phase != string(SPPhaseVoting) {
		return nil
	}
	// 이미 투표했으면 다시 내지 않는다 (공개 투표 현황으로 판단)
	for _, v := range state.Votes {
		if v.Voter == state.YourSeat {
			return nil
		}
	}
	cands := []int{}
	for _, p := range state.Players {
		if p.Seat != state.YourSeat {
			cands = append(cands, p.Seat)
		}
	}
	if len(cands) == 0 {
		return nil
	}
	target := cands[b.rng.Intn(len(cands))]
	return &SPMessage{Type: SPMsgVote, Payload: SPVotePayload{Target: target}}
}

// ==================== 봇 소환 ====================

// spawnBot 대기 방의 남은 자리에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *SPHub) spawnBot(room *spRoom, name string) *SPClient {
	bot := &SPClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return nil
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.runSPBot(bot)
	return bot
}

// takeoverBot 유예 만료 좌석을 이어받는 연습봇 — 이름·좌석·배역(스파이 여부)을
// 유지해 진행 중인 투표 해소 조건이 그대로 성립한다
func (h *SPHub) takeoverBot(room *spRoom, seat int, name string) *SPClient {
	bot := &SPClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	bot.GameID = room.Game.ID
	bot.Seat = seat
	h.runSPBot(bot)
	return bot
}

// runSPBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *SPHub) runSPBot(bot *SPClient) {
	brain := newSPBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m SPMessage) { h.gameMessage <- SPGameMessage{Client: bot, Message: m} },
		func(m SPMessage) bool { return m.Type == SPMsgGameOver || m.Type == SPMsgSessionExpired })
}
