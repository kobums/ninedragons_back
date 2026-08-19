package server

import (
	"math/rand"
	"time"
)

// ==================== 아발론 연습봇 ====================
//
// 연습용 — 무작위 합법 행동만 한다. 리더면 자기 포함 무작위 지명, 팀 투표는
// 70% 찬성, 원정은 악봇만 60% 실패 (선봇은 항상 성공), 암살은 무작위 선
// 플레이어 지목. 부결 5회 룰 + 원정 5라운드 상한으로 유한 종료가 보장된다.
// 검증 규칙은 서버와 같은 av_game.go 를 공유하므로 봇이 서버 검증에 걸리는
// 수를 내지 않는다.

// avBotState 봇이 쓰는 최소 스냅샷
type avBotState struct {
	Phase      string `json:"phase"`
	YourSeat   int    `json:"yourSeat"`
	YourRole   string `json:"yourRole"`
	EvilSeats  []int  `json:"evilSeats"`
	Round      int    `json:"round"`
	QuestSizes [5]int `json:"questSizes"`
	LeaderSeat int    `json:"leaderSeat"`
	Players    []struct {
		Seat      int  `json:"seat"`
		OnTeam    bool `json:"onTeam"`
		VotedTeam bool `json:"votedTeam"`
		QuestDone bool `json:"questDone"`
	} `json:"players"`
}

// avBrain 아발론 봇 두뇌 (무작위 — 자체 난수원)
type avBrain struct {
	rng *rand.Rand
}

func newAVBrain() *avBrain {
	return &avBrain{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

func (b *avBrain) decide(msg AVMessage) *AVMessage {
	if msg.Type != AVMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[avBotState](msg.Payload)
	if !ok {
		return nil
	}
	return b.act(state)
}

func (b *avBrain) act(state avBotState) *AVMessage {
	onTeam, votedTeam, questDone, found := b.myView(state)

	switch state.Phase {
	case string(AVPhaseTeamPick):
		if state.LeaderSeat != state.YourSeat {
			return nil
		}
		seats := b.pickTeam(state)
		if seats == nil {
			return nil
		}
		return &AVMessage{Type: AVMsgPick, Payload: AVPickPayload{Seats: seats}}

	case string(AVPhaseTeamVote):
		if !found || votedTeam {
			return nil
		}
		approve := b.rng.Float64() < 0.7 // 70% 찬성 — 부결 루프 없이 유한 종료
		return &AVMessage{Type: AVMsgTeamVote, Payload: AVTeamVotePayload{Approve: approve}}

	case string(AVPhaseQuest):
		if !found || !onTeam || questDone {
			return nil
		}
		success := true
		if b.isEvil(state) && b.rng.Float64() < 0.6 {
			success = false // 악봇만 실패 선택 가능 (선봇은 성공 강제)
		}
		return &AVMessage{Type: AVMsgQuest, Payload: AVQuestPayload{Success: success}}

	case string(AVPhaseAssassin):
		if state.YourRole != string(AVRoleAssassin) {
			return nil
		}
		target := b.pickGoodSeat(state)
		if target < 0 {
			return nil
		}
		return &AVMessage{Type: AVMsgAssassinate, Payload: AVAssassinatePayload{Seat: target}}
	}
	return nil
}

// myView 내 좌석의 (onTeam, votedTeam, questDone) 공개 정보
func (b *avBrain) myView(state avBotState) (onTeam, votedTeam, questDone, found bool) {
	for _, p := range state.Players {
		if p.Seat == state.YourSeat {
			return p.OnTeam, p.VotedTeam, p.QuestDone, true
		}
	}
	return false, false, false, false
}

func (b *avBrain) isEvil(state avBotState) bool {
	return state.YourRole == string(AVRoleAssassin) || state.YourRole == string(AVRoleEvil)
}

// pickTeam 리더의 무작위 지명 — 자기 포함, 나머지는 무작위
func (b *avBrain) pickTeam(state avBotState) []int {
	if state.Round < 1 || state.Round > 5 {
		return nil
	}
	size := state.QuestSizes[state.Round-1]
	if size < 1 || size > len(state.Players) {
		return nil
	}
	others := []int{}
	for _, p := range state.Players {
		if p.Seat != state.YourSeat {
			others = append(others, p.Seat)
		}
	}
	b.rng.Shuffle(len(others), func(i, j int) { others[i], others[j] = others[j], others[i] })
	return append([]int{state.YourSeat}, others[:size-1]...)
}

// pickGoodSeat 악 명단에 없는 좌석 중 무작위 (암살자 시점의 선 후보)
func (b *avBrain) pickGoodSeat(state avBotState) int {
	evil := map[int]bool{}
	for _, s := range state.EvilSeats {
		evil[s] = true
	}
	cands := []int{}
	for _, p := range state.Players {
		if !evil[p.Seat] {
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
func (h *AVHub) spawnBot(room *avRoom, name string) *AVClient {
	bot := &AVClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return nil
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.runAVBot(bot)
	return bot
}

// takeoverBot 유예 만료 좌석을 이어받는 연습봇 — 이름·좌석·역할을 유지해
// 진행 중인 지명·투표·원정·암살 해소 조건이 그대로 성립한다
func (h *AVHub) takeoverBot(room *avRoom, seat int, name string) *AVClient {
	bot := &AVClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	bot.GameID = room.Game.ID
	bot.Seat = seat
	h.runAVBot(bot)
	return bot
}

// runAVBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *AVHub) runAVBot(bot *AVClient) {
	brain := newAVBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m AVMessage) { h.gameMessage <- AVGameMessage{Client: bot, Message: m} },
		func(m AVMessage) bool { return m.Type == AVMsgGameOver || m.Type == AVMsgSessionExpired })
}
