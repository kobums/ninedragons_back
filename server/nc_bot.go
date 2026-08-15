package server

// ==================== 넘버체인지 연습봇 ====================
//
// 넘버체인지는 양 팀이 라운드마다 동시에 제출하므로 봇은 nc_game_start 직후와
// 매 nc_round_result 직후에 제출한다. 보유 블록은 라운드 결과(내가 낸 블록
// 2개 제거 + 교환받은 블록 추가)로 서버 상태와 동일하게 추적한다.
//
// 히든 대응: 봇은 히든을 쓰지 않지만, 상대가 히든을 쓰면 봇의 블록 선택이
// 있어야 라운드가 진행된다(ReadyToProcess). 제출 페이로드의 SelectedBlockChoice
// 는 상대가 히든을 썼을 때만 소비되므로 봇은 매 제출에 choice=1 을 미리 실어
// 교착을 원천 차단하고, nc_use_hidden 을 받으면 nc_select_block(choice=1)도
// 한 번 더 보낸다 (이미 반영된 경우 무해한 중복).

// ncBotStart 게임 시작 페이로드에서 봇이 쓰는 최소 정보
type ncBotStart struct {
	YourTeam string `json:"yourTeam"`
}

// ncBotRoundResult 라운드 결과에서 봇이 쓰는 최소 정보
type ncBotRoundResult struct {
	Team1Block1        int `json:"team1Block1"`
	Team1Block2        int `json:"team1Block2"`
	Team2Block1        int `json:"team2Block1"`
	Team2Block2        int `json:"team2Block2"`
	Team1ReceivedBlock int `json:"team1ReceivedBlock"`
	Team2ReceivedBlock int `json:"team2ReceivedBlock"`
}

// ncBrain 매 라운드 남은 블록 중 가장 큰 2개를 제출한다 (히든 미사용)
type ncBrain struct {
	team   string
	blocks []int
}

func (b *ncBrain) decide(msg NCMessage) *NCMessage {
	switch msg.Type {
	case NCMsgGameStart:
		start, ok := botPayloadAs[ncBotStart](msg.Payload)
		if !ok {
			return nil
		}
		b.team = start.YourTeam
		b.blocks = []int{1, 2, 3, 4, 5, 6, 7, 1, 2, 3, 4, 5, 6, 7}
		return b.submit()

	case NCMsgRoundResult:
		rr, ok := botPayloadAs[ncBotRoundResult](msg.Payload)
		if !ok || b.team == "" {
			return nil
		}
		// 내가 낸 블록 2개를 빼고 교환받은 블록을 더한다
		if b.team == string(Team1) {
			b.removeBlocks(rr.Team1Block1, rr.Team1Block2)
			b.blocks = append(b.blocks, rr.Team1ReceivedBlock)
		} else {
			b.removeBlocks(rr.Team2Block1, rr.Team2Block2)
			b.blocks = append(b.blocks, rr.Team2ReceivedBlock)
		}
		return b.submit()

	case NCMsgUseHidden:
		// 상대가 히든을 썼다 — 블록 선택을 보내야 라운드가 진행된다
		return &NCMessage{Type: NCMsgSelectBlock, Payload: NCSelectBlockPayload{SelectedBlockChoice: 1}}
	}
	return nil
}

// submit 남은 블록 중 가장 큰 2개 제출 (블록 차감은 라운드 결과에서 반영)
func (b *ncBrain) submit() *NCMessage {
	if len(b.blocks) < 2 {
		return nil
	}
	block1, block2 := twoLargestBlocks(b.blocks)
	return &NCMessage{Type: NCMsgSubmitBlocks, Payload: NCSubmitBlocksPayload{
		Block1: block1,
		Block2: block2,
		// 상대가 히든을 쓸 때만 소비되는 값 — 미리 실어 교착을 막는다
		SelectedBlockChoice: 1,
	}}
}

// removeBlocks 보유 목록에서 두 블록을 한 번씩 제거
func (b *ncBrain) removeBlocks(block1, block2 int) {
	for _, target := range []int{block1, block2} {
		for i, v := range b.blocks {
			if v == target {
				b.blocks = append(b.blocks[:i], b.blocks[i+1:]...)
				break
			}
		}
	}
}

// twoLargestBlocks 목록에서 가장 큰 값 2개 (원본 비변경)
func twoLargestBlocks(blocks []int) (int, int) {
	first, second := 0, 0
	for _, v := range blocks {
		if v > first {
			first, second = v, first
		} else if v > second {
			second = v
		}
	}
	return first, second
}

// spawnBot 방의 남은 팀 자리에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *NCHub) spawnBot(room *ncRoom) {
	bot := &NCClient{wsClient: newBotWSClient(), Hub: h}
	team := Team1
	if room.Clients[Team1] != nil {
		team = Team2
	}
	room.Game.AddPlayer(bot.Name, team)
	bot.GameID = room.Game.ID
	bot.Team = team
	room.Clients[team] = bot
	h.sessions[bot.SessionID] = bot

	brain := &ncBrain{}
	go runBot(bot.Send,
		brain.decide,
		func(m NCMessage) { h.gameMessage <- NCGameMessage{Client: bot, Message: m} },
		func(m NCMessage) bool { return m.Type == NCMsgGameOver || m.Type == NCMsgSessionExpired })
}

// ncRoomHasBot 방에 연습봇이 있는지 (ntfy 억제 판단용)
func ncRoomHasBot(room *ncRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
