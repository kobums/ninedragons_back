package server

// ==================== 오니타마 연습봇 ====================

// otBotState 봇이 쓰는 최소 스냅샷
type otBotState struct {
	YourSide    string        `json:"yourSide"`
	Phase       string        `json:"phase"`
	CurrentSide string        `json:"currentSide"`
	Pieces      []OTPiece     `json:"pieces"`
	SouthHand   []string      `json:"southHand"`
	NorthHand   []string      `json:"northHand"`
	LegalMoves  []OTLegalMove `json:"legalMoves"`
}

// otBrain 승리 수(마스터 잡기·사원 도달)와 잡기 수를 우선하고,
// 없으면 카운터 회전으로 합법 수를 하나 둔다. 수가 없으면 첫 카드로 패스.
type otBrain struct {
	counter int
}

func (b *otBrain) decide(msg OTMessage) *OTMessage {
	if msg.Type != OTMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[otBotState](msg.Payload)
	if !ok {
		return nil
	}
	if state.Phase != string(OTPhasePlay) || state.CurrentSide != state.YourSide {
		return nil
	}
	b.counter++

	if len(state.LegalMoves) == 0 {
		hand := state.SouthHand
		if state.YourSide == string(OTNorth) {
			hand = state.NorthHand
		}
		if len(hand) == 0 {
			return nil
		}
		return &OTMessage{Type: OTMsgPass, Payload: OTPassPayload{Card: hand[0]}}
	}

	mySide := OTSide(state.YourSide)
	temple := otTemple(otOther(mySide))
	pieceBy := map[OTCell]OTPiece{}
	for _, p := range state.Pieces {
		pieceBy[OTCell{p.Row, p.Col}] = p
	}

	pick := state.LegalMoves[b.counter%len(state.LegalMoves)]
	for _, m := range state.LegalMoves {
		target, occupied := pieceBy[m.To]
		mover := pieceBy[m.From]
		// 승리 수 최우선
		if (occupied && target.Master) || (mover.Master && m.To == temple) {
			pick = m
			break
		}
		// 잡기 수 차선
		if occupied && target.Side != mySide {
			pick = m
		}
	}
	return &OTMessage{Type: OTMsgMove, Payload: OTMovePayload{Card: pick.Card, From: pick.From, To: pick.To}}
}

// spawnBot 방의 남은 자리에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *OTHub) spawnBot(room *otRoom) {
	bot := &OTClient{wsClient: newBotWSClient(), Hub: h}
	side, err := room.Game.AddPlayer(bot.Name)
	if err != nil {
		return
	}
	bot.GameID = room.Game.ID
	bot.Side = side
	room.Clients[side] = bot
	h.sessions[bot.SessionID] = bot

	brain := &otBrain{}
	go runBot(bot.Send,
		brain.decide,
		func(m OTMessage) { h.gameMessage <- OTGameMessage{Client: bot, Message: m} },
		func(m OTMessage) bool { return m.Type == OTMsgGameOver || m.Type == OTMsgSessionExpired })
}

// otRoomHasBot 방에 연습봇이 있는지 (ntfy 억제 판단용)
func otRoomHasBot(room *otRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
