package server

import "strconv"

// ==================== 캔트 스톱 연습봇 ====================

// csBotState 봇이 쓰는 최소 스냅샷
type csBotState struct {
	YourSide    string         `json:"yourSide"`
	Phase       string         `json:"phase"`
	CurrentSide string         `json:"currentSide"`
	Temp        map[string]int `json:"temp"`
	Options     []CSOption     `json:"options"`
	CanRoll     bool           `json:"canRoll"`
	CanStop     bool           `json:"canStop"`
}

// csBrain 조합 대기면 첫 옵션을 고르고, 아니면 "마커 3개를 다 썼거나
// 임시 마커가 꼭대기에 닿았으면 정지, 그 외엔 굴림" 전략.
type csBrain struct{}

func (b *csBrain) decide(msg CSMessage) *CSMessage {
	if msg.Type != CSMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[csBotState](msg.Payload)
	if !ok {
		return nil
	}
	if state.Phase != string(CSPhasePlay) || state.CurrentSide != state.YourSide {
		return nil
	}

	if len(state.Options) > 0 {
		return &CSMessage{Type: CSMsgChoose, Payload: CSChoosePayload{Sums: state.Options[0].Sums}}
	}
	if state.CanStop {
		atTop := false
		for colStr, pos := range state.Temp {
			if col, err := strconv.Atoi(colStr); err == nil && pos >= csColLen(col) {
				atTop = true
			}
		}
		if atTop || len(state.Temp) >= CSMarkerMax {
			return &CSMessage{Type: CSMsgStop}
		}
	}
	if state.CanRoll {
		return &CSMessage{Type: CSMsgRoll}
	}
	return nil
}

// spawnBot 방의 남은 자리에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *CSHub) spawnBot(room *csRoom) {
	bot := &CSClient{wsClient: newBotWSClient(), Hub: h}
	side, err := room.Game.AddPlayer(bot.Name)
	if err != nil {
		return
	}
	bot.GameID = room.Game.ID
	bot.Side = side
	room.Clients[side] = bot
	h.sessions[bot.SessionID] = bot

	brain := &csBrain{}
	go runBot(bot.Send,
		brain.decide,
		func(m CSMessage) { h.gameMessage <- CSGameMessage{Client: bot, Message: m} },
		func(m CSMessage) bool { return m.Type == CSMsgGameOver || m.Type == CSMsgSessionExpired })
}

// csRoomHasBot 방에 연습봇이 있는지 (ntfy 억제 판단용)
func csRoomHasBot(room *csRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
