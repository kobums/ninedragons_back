package server

// ==================== 로스트 시티 연습봇 ====================

// lcBotState 봇이 쓰는 최소 스냅샷
type lcBotState struct {
	YourSide         string               `json:"yourSide"`
	Phase            string               `json:"phase"`
	CurrentSide      string               `json:"currentSide"`
	YourHand         []LCCard             `json:"yourHand"`
	SouthExpeditions map[LCColor][]LCCard `json:"southExpeditions"`
	NorthExpeditions map[LCColor][]LCCard `json:"northExpeditions"`
}

// lcBrain 놓을 수 있는 카드가 있으면 놓고, 없으면 첫 카드를 버린다.
// 항상 덱에서 뽑아 덱 소진(게임 종료)을 보장한다.
type lcBrain struct{}

func (b *lcBrain) decide(msg LCMessage) *LCMessage {
	if msg.Type != LCMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[lcBotState](msg.Payload)
	if !ok {
		return nil
	}
	if state.Phase != string(LCPhasePlay) || state.CurrentSide != state.YourSide || len(state.YourHand) == 0 {
		return nil
	}

	mine := state.SouthExpeditions
	if state.YourSide == string(LCNorth) {
		mine = state.NorthExpeditions
	}

	// 오름차순으로 놓을 수 있는 카드 탐색 (서버 canPlay 미러)
	for _, card := range state.YourHand {
		pile := mine[card.Color]
		ok := true
		for _, c := range pile {
			if card.Value == LCWagerValue {
				if c.Value != LCWagerValue {
					ok = false
				}
			} else if c.Value != LCWagerValue && c.Value >= card.Value {
				ok = false
			}
		}
		if ok {
			return &LCMessage{Type: LCMsgMove, Payload: LCMovePayload{CardID: card.ID, Action: "play", Draw: "deck"}}
		}
	}
	return &LCMessage{Type: LCMsgMove, Payload: LCMovePayload{CardID: state.YourHand[0].ID, Action: "discard", Draw: "deck"}}
}

// spawnBot 방의 남은 자리에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *LCHub) spawnBot(room *lcRoom) {
	bot := &LCClient{wsClient: newBotWSClient(), Hub: h}
	side, err := room.Game.AddPlayer(bot.Name)
	if err != nil {
		return
	}
	bot.GameID = room.Game.ID
	bot.Side = side
	room.Clients[side] = bot
	h.sessions[bot.SessionID] = bot

	brain := &lcBrain{}
	go runBot(bot.Send,
		brain.decide,
		func(m LCMessage) { h.gameMessage <- LCGameMessage{Client: bot, Message: m} },
		func(m LCMessage) bool { return m.Type == LCMsgGameOver || m.Type == LCMsgSessionExpired })
}

// lcRoomHasBot 방에 연습봇이 있는지 (ntfy 억제 판단용)
func lcRoomHasBot(room *lcRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
