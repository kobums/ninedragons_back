package server

import (
	"math/rand"
	"time"
)

// ==================== 스카이폴 연습봇 ====================
//
// 연습용 — 무작위 합법 행동만 한다. 밤엔 역할별 무작위 합법 대상(마피아는
// 비마피아 생존자, 경찰은 자기 제외 생존자, 의사는 자기 포함 생존자),
// 낮 투표는 자기 제외 생존자 중 무작위 (기권 10%). 검증 규칙은 서버와 같은
// sf_game.go 를 공유하므로 봇이 서버 검증에 걸리는 수를 내지 않는다.

// sfBotState 봇이 쓰는 최소 스냅샷
type sfBotState struct {
	Phase      string `json:"phase"`
	YourSeat   int    `json:"yourSeat"`
	YourRole   string `json:"yourRole"`
	MafiaSeats []int  `json:"mafiaSeats"`
	Players    []struct {
		Seat  int  `json:"seat"`
		Alive bool `json:"alive"`
	} `json:"players"`
	Night *struct {
		YourActionDone bool `json:"yourActionDone"`
	} `json:"night"`
	Votes []SFVoteView `json:"votes"`
}

// sfBrain 스카이폴 봇 두뇌 (무작위 — 자체 난수원)
type sfBrain struct {
	rng *rand.Rand
}

func newSFBrain() *sfBrain {
	return &sfBrain{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

func (b *sfBrain) decide(msg SFMessage) *SFMessage {
	if msg.Type != SFMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[sfBotState](msg.Payload)
	if !ok {
		return nil
	}
	return b.act(state)
}

func (b *sfBrain) act(state sfBotState) *SFMessage {
	if !b.isAlive(state, state.YourSeat) {
		return nil // 사망자는 관전만
	}

	switch state.Phase {
	case string(SFPhaseNight):
		if state.Night == nil || state.Night.YourActionDone {
			return nil
		}
		target := b.pickNightTarget(state)
		if target < 0 {
			return nil
		}
		return &SFMessage{Type: SFMsgNightAction, Payload: SFNightActionPayload{Target: target}}

	case string(SFPhaseDayVote):
		// 이미 투표했으면 다시 내지 않는다 (공개 투표 현황으로 판단)
		for _, v := range state.Votes {
			if v.Voter == state.YourSeat {
				return nil
			}
		}
		target := -1 // 기권
		if b.rng.Intn(10) != 0 {
			target = b.pickAlive(state, func(seat int) bool { return seat != state.YourSeat })
		}
		return &SFMessage{Type: SFMsgVote, Payload: SFVotePayload{Target: target}}
	}
	return nil
}

// pickNightTarget 역할별 무작위 합법 대상 (시민은 밤 행동 없음)
func (b *sfBrain) pickNightTarget(state sfBotState) int {
	switch state.YourRole {
	case string(SFRoleMafia):
		return b.pickAlive(state, func(seat int) bool {
			for _, m := range state.MafiaSeats {
				if m == seat {
					return false
				}
			}
			return true
		})
	case string(SFRolePolice):
		return b.pickAlive(state, func(seat int) bool { return seat != state.YourSeat })
	case string(SFRoleDoctor):
		return b.pickAlive(state, func(int) bool { return true }) // 자기 포함
	}
	return -1
}

func (b *sfBrain) isAlive(state sfBotState, seat int) bool {
	for _, p := range state.Players {
		if p.Seat == seat {
			return p.Alive
		}
	}
	return false
}

// pickAlive 조건을 만족하는 생존자 중 무작위 한 명 (없으면 -1)
func (b *sfBrain) pickAlive(state sfBotState, allow func(seat int) bool) int {
	cands := []int{}
	for _, p := range state.Players {
		if p.Alive && allow(p.Seat) {
			cands = append(cands, p.Seat)
		}
	}
	if len(cands) == 0 {
		return -1
	}
	return cands[b.rng.Intn(len(cands))]
}

// ==================== 봇 소환 ====================

// spawnBot 대기 방의 남은 자리에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *SFHub) spawnBot(room *sfRoom, name string) *SFClient {
	bot := &SFClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return nil
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.runSFBot(bot)
	return bot
}

// takeoverBot 유예 만료 좌석을 이어받는 연습봇 — 이름·좌석·역할을 유지해
// 진행 중인 밤·투표 해소 조건이 그대로 성립한다
func (h *SFHub) takeoverBot(room *sfRoom, seat int, name string) *SFClient {
	bot := &SFClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	bot.GameID = room.Game.ID
	bot.Seat = seat
	h.runSFBot(bot)
	return bot
}

// runSFBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *SFHub) runSFBot(bot *SFClient) {
	brain := newSFBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m SFMessage) { h.gameMessage <- SFGameMessage{Client: bot, Message: m} },
		func(m SFMessage) bool { return m.Type == SFMsgGameOver || m.Type == SFMsgSessionExpired })
}
