package server

import (
	"math/rand"
	"time"
)

// ==================== 코드네임 연습봇 ====================
//
// 스냅샷(cn_game_state)만 보고 반응한다. 봇은 요원 전담이 원칙이지만
// 사람이 1명뿐인 팀에서는 스파이마스터를 맡는다 (assignRoles 규칙) —
// 그 경우 무작위 힌트("힌트 N", N=1~2)를 즉시 낸다. 요원 봇은 미공개
// 카드를 무작위로 1장 선택한다 (남은 횟수가 없으면 턴 종료).

// cnBotClueValue 봇 스파이마스터의 무작위 힌트 (AFK 자동 힌트도 같은 값)
func cnBotClueValue(rng *rand.Rand) (string, int) {
	return "힌트", 1 + rng.Intn(2)
}

// cnBotState 봇이 상태 스냅샷에서 꺼내 쓰는 최소 정보
type cnBotState struct {
	YourSeat    int     `json:"yourSeat"`
	Phase       CNPhase `json:"phase"`
	CurrentTeam CNTeam  `json:"currentTeam"`
	YourTeam    CNTeam  `json:"yourTeam"`
	YourRole    CNRole  `json:"yourRole"`
	Clue        *struct {
		Remaining int `json:"remaining"`
	} `json:"clue"`
	Board []struct {
		Revealed bool `json:"revealed"`
	} `json:"board"`
}

// cnBrain 스냅샷 기반 무작위 판단 (AFK 자동 힌트도 같은 값 생성기를 쓴다)
type cnBrain struct {
	rng *rand.Rand
}

func newCNBrain() *cnBrain {
	return &cnBrain{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

// decide 공용 러너 계약 — cn_game_state 에만 반응한다
func (b *cnBrain) decide(msg CNMessage) *CNMessage {
	if msg.Type != CNMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[cnBotState](msg.Payload)
	if !ok {
		return nil
	}
	return b.decideState(state)
}

// decideState 자기 팀 차례의 자기 역할 행동만 고른다 (아니면 nil)
func (b *cnBrain) decideState(state cnBotState) *CNMessage {
	if state.YourTeam == "" || state.YourTeam != state.CurrentTeam {
		return nil
	}

	switch {
	case state.Phase == CNPhaseClue && state.YourRole == CNRoleSpymaster:
		word, count := cnBotClueValue(b.rng)
		return &CNMessage{Type: CNMsgClue, Payload: CNCluePayload{Word: word, Count: count}}

	case state.Phase == CNPhaseGuess && state.YourRole == CNRoleAgent:
		if state.Clue == nil || state.Clue.Remaining <= 0 {
			return &CNMessage{Type: CNMsgEndTurn}
		}
		unrevealed := []int{}
		for i, card := range state.Board {
			if !card.Revealed {
				unrevealed = append(unrevealed, i)
			}
		}
		if len(unrevealed) == 0 {
			return &CNMessage{Type: CNMsgEndTurn}
		}
		pick := unrevealed[b.rng.Intn(len(unrevealed))]
		return &CNMessage{Type: CNMsgPick, Payload: CNPickPayload{Index: pick}}
	}
	return nil
}

// ==================== 봇 소환 ====================

// spawnCNBot 대기실의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *CNHub) spawnCNBot(room *cnRoom, name string) bool {
	bot := &CNClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name, true)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runCNBot(bot)
	return true
}

// runCNBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *CNHub) runCNBot(bot *CNClient) {
	brain := newCNBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m CNMessage) { h.gameMessage <- CNGameMessage{Client: bot, Message: m} },
		func(m CNMessage) bool { return m.Type == CNMsgGameOver || m.Type == CNMsgSessionExpired })
}

// cnRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록용)
func cnRoomHasBot(room *cnRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
