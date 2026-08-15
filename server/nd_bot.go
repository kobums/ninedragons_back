package server

// ==================== 구룡투 연습봇 ====================
//
// 구룡투는 상시 상태 브로드캐스트가 없어(진행은 tile_played/round_result 로만
// 전달) 봇이 게임 흐름 메시지로 자기 차례를 판단한다:
//   - game_start: 선공(FirstPlayer)이 나면 첫 타일을 낸다
//   - tile_played: WaitingFor 가 나이고 아직 이번 라운드 타일을 안 냈으면 낸다
//   - round_result: 다음 선공(NextPlayer)이 나면 낸다

// ndBotStart 게임 시작 페이로드에서 봇이 쓰는 최소 정보
type ndBotStart struct {
	FirstPlayer string `json:"firstPlayer"`
	YourColor   string `json:"yourColor"`
}

// ndBotTilePlayed 타일 제출 브로드캐스트에서 봇이 쓰는 최소 정보
type ndBotTilePlayed struct {
	WaitingFor     string `json:"waitingFor"`
	BlueTilePlayed bool   `json:"blueTilePlayed"`
	RedTilePlayed  bool   `json:"redTilePlayed"`
}

// ndBotRoundResult 라운드 결과에서 봇이 쓰는 최소 정보
type ndBotRoundResult struct {
	NextPlayer string `json:"nextPlayer"`
}

// ndBrain 내 차례마다 남은 타일 중 중간값을 낸다.
// remaining 은 처음부터 정렬돼 있고 제거만 하므로 정렬이 유지된다.
type ndBrain struct {
	color     string
	remaining []int
}

func (b *ndBrain) decide(msg Message) *Message {
	switch msg.Type {
	case MsgGameStart:
		start, ok := botPayloadAs[ndBotStart](msg.Payload)
		if !ok {
			return nil
		}
		b.color = start.YourColor
		b.remaining = []int{1, 2, 3, 4, 5, 6, 7, 8, 9}
		if start.FirstPlayer == b.color {
			return b.play()
		}

	case MsgTilePlayed:
		tp, ok := botPayloadAs[ndBotTilePlayed](msg.Payload)
		if !ok || b.color == "" {
			return nil
		}
		myTilePlayed := tp.BlueTilePlayed
		if b.color == string(Red) {
			myTilePlayed = tp.RedTilePlayed
		}
		if tp.WaitingFor == b.color && !myTilePlayed {
			return b.play()
		}

	case MsgRoundResult:
		rr, ok := botPayloadAs[ndBotRoundResult](msg.Payload)
		if !ok || b.color == "" {
			return nil
		}
		if rr.NextPlayer == b.color {
			return b.play()
		}
	}
	return nil
}

// play 남은 타일 중 중간값을 낸다
func (b *ndBrain) play() *Message {
	if len(b.remaining) == 0 {
		return nil
	}
	idx := len(b.remaining) / 2
	tile := b.remaining[idx]
	b.remaining = append(b.remaining[:idx], b.remaining[idx+1:]...)
	return &Message{Type: MsgPlayTile, Payload: PlayTilePayload{Tile: tile}}
}

// spawnBot 방의 남은 색상 자리에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *Hub) spawnBot(room *ndRoom) {
	bot := &Client{wsClient: newBotWSClient(), Hub: h}
	color := Blue
	if room.Clients[Blue] != nil {
		color = Red
	}
	if err := room.Game.AddPlayer(bot.Name, color); err != nil {
		return
	}
	bot.GameID = room.Game.ID
	bot.Color = color
	room.Clients[color] = bot
	h.sessions[bot.SessionID] = bot

	brain := &ndBrain{}
	go runBot(bot.Send,
		brain.decide,
		func(m Message) { h.gameMessage <- GameMessage{Client: bot, Message: m} },
		func(m Message) bool { return m.Type == MsgGameOver || m.Type == MsgSessionExpired })
}

// ndRoomHasBot 방에 연습봇이 있는지 (ntfy 억제 판단용)
func ndRoomHasBot(room *ndRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
