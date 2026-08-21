package server

// ==================== 노 땡스! 연습봇 ====================
//
// 스냅샷(nt_game_state)만 보고 반응한다. 봇도 사람과 같은 조건 — 타인
// 칩은 -1 로 가려져 모르고, 공개 정보(카드·얹힌 칩·자기 칩)로만 판단한다.
// 판단 규칙 (카드값 − 얹힌 칩 = 실질 비용):
//   실질 비용 ≤ 10             → 가져가기 (싸다)
//   내 연속 시퀀스에 붙는 카드  → 실질 비용 ≤ 16 까지 가져가기
//   그 외                       → 패스 (칩이 없으면 가져가기 — 유일한 합법 수)

// ntBotPlayerView 봇이 스냅샷에서 꺼내 쓰는 좌석 정보
type ntBotPlayerView struct {
	Seat  int   `json:"seat"`
	Chips int   `json:"chips"` // 자기 좌석만 실값 (타인 -1)
	Cards []int `json:"cards"`
}

// ntBotState 봇이 상태 스냅샷에서 꺼내 쓰는 최소 정보
type ntBotState struct {
	YourSeat    int               `json:"yourSeat"`
	Phase       NTPhase           `json:"phase"`
	CurrentSeat int               `json:"currentSeat"`
	Card        int               `json:"card"`
	PotChips    int               `json:"potChips"`
	Players     []ntBotPlayerView `json:"players"`
}

// ntBrain 스냅샷 기반 판단 (봇 대체 좌석도 같은 두뇌를 쓴다)
type ntBrain struct{}

func newNTBrain() *ntBrain {
	return &ntBrain{}
}

// decide 공용 러너 계약 — nt_game_state 에만 반응한다
func (b *ntBrain) decide(msg NTMessage) *NTMessage {
	if msg.Type != NTMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[ntBotState](msg.Payload)
	if !ok {
		return nil
	}
	return b.decideState(state)
}

// decideState 자기 차례면 패스/가져가기를 고른다 (아니면 nil)
func (b *ntBrain) decideState(state ntBotState) *NTMessage {
	me := state.YourSeat
	if state.Phase != NTPhasePlaying || state.CurrentSeat != me || state.Card == 0 {
		return nil
	}

	var mine *ntBotPlayerView
	for i := range state.Players {
		if state.Players[i].Seat == me {
			mine = &state.Players[i]
			break
		}
	}
	if mine == nil {
		return nil
	}

	cost := state.Card - state.PotChips // 실질 비용 (얹힌 칩만큼 상쇄)
	adjacent := false
	for _, c := range mine.Cards {
		if c == state.Card-1 || c == state.Card+1 {
			adjacent = true // 내 시퀀스에 붙는 카드 — 점수 증가가 작다
			break
		}
	}

	take := cost <= 10 || (adjacent && cost <= 16)
	if !take && mine.Chips > 0 {
		return &NTMessage{Type: NTMsgPass}
	}
	return &NTMessage{Type: NTMsgTake} // 칩 0이면 가져가기가 유일한 합법 수
}

// ==================== 봇 소환 ====================

// spawnNTBot 대기실의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *NTHub) spawnNTBot(room *ntRoom, name string) bool {
	bot := &NTClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runNTBot(bot)
	return true
}

// runNTBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *NTHub) runNTBot(bot *NTClient) {
	brain := newNTBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m NTMessage) { h.gameMessage <- NTGameMessage{Client: bot, Message: m} },
		func(m NTMessage) bool { return m.Type == NTMsgGameOver || m.Type == NTMsgSessionExpired })
}

// ntRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록용)
func ntRoomHasBot(room *ntRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
