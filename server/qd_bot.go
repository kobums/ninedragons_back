package server

// ==================== 쿼리도 연습봇 ====================

// qdBotState 봇이 쓰는 최소 스냅샷
type qdBotState struct {
	YourSide    string   `json:"yourSide"`
	Phase       string   `json:"phase"`
	CurrentSide string   `json:"currentSide"`
	LegalMoves  []QDCell `json:"legalMoves"`
}

// qdBrain 내 턴마다 합법 수 중 목표 줄에 가장 가까워지는 칸을 고른다
// (벽은 놓지 않는다). counter 는 동점 수 사이의 왕복 교착을 피하는 회전값.
type qdBrain struct {
	counter int
}

func (b *qdBrain) decide(msg QDMessage) *QDMessage {
	if msg.Type != QDMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[qdBotState](msg.Payload)
	if !ok {
		return nil
	}
	if state.Phase != string(QDPhasePlay) || state.CurrentSide != state.YourSide || len(state.LegalMoves) == 0 {
		return nil
	}

	goal := qdGoalRow(QDSide(state.YourSide))
	b.counter++

	best := state.LegalMoves[b.counter%len(state.LegalMoves)]
	bestDist := best.Row - goal
	if bestDist < 0 {
		bestDist = -bestDist
	}
	for _, m := range state.LegalMoves {
		d := m.Row - goal
		if d < 0 {
			d = -d
		}
		if d < bestDist {
			best, bestDist = m, d
		}
	}
	return &QDMessage{Type: QDMsgMove, Payload: QDMovePayload{To: best}}
}

// spawnBot 방의 남은 자리에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *QDHub) spawnBot(room *qdRoom) {
	bot := &QDClient{wsClient: newBotWSClient(), Hub: h}
	side, err := room.Game.AddPlayer(bot.Name)
	if err != nil {
		return
	}
	bot.GameID = room.Game.ID
	bot.Side = side
	room.Clients[side] = bot
	h.sessions[bot.SessionID] = bot

	brain := &qdBrain{}
	go runBot(bot.Send,
		brain.decide,
		func(m QDMessage) { h.gameMessage <- QDGameMessage{Client: bot, Message: m} },
		func(m QDMessage) bool { return m.Type == QDMsgGameOver || m.Type == QDMsgSessionExpired })
}
