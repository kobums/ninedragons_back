package server

// ==================== 요트 다이스 연습봇 ====================
//
// 스냅샷(yt_game_state)만 보고 반응하는 그리디 봇.
//   1) 턴의 첫 굴림은 무조건 전부 굴린다.
//   2) 굴림이 남았으면: 현재 주사위로 빈 칸 최고 점수를 계산해 이미 좋은
//      족보(25점 이상 — 풀하우스/스트레이트/요트급)면 즉시 기록하고, 아니면
//      최다 눈을 홀드하고 나머지를 다시 굴린다 (가장 많은 눈 유지 전략).
//   3) 굴림을 다 쓰면 빈 칸 중 최고 점수 칸에 기록한다. 전부 0점이면
//      상단 최소 칸(낮은 눈부터)에 0을 적어 칸을 희생한다.
// AFK 자동 진행(yt_hub.go)도 같은 기록 선택 함수(ytBestCategory)를 쓴다.

// ytBotStopScore 굴림을 멈추고 즉시 기록하는 점수 문턱 (풀하우스 25점급)
const ytBotStopScore = 25

// ytBotState 봇이 상태 스냅샷에서 꺼내 쓰는 최소 정보
type ytBotState struct {
	YourSeat    int     `json:"yourSeat"`
	Phase       YTPhase `json:"phase"`
	CurrentSeat int     `json:"currentSeat"`
	Dice        []int   `json:"dice"`
	RollsLeft   int     `json:"rollsLeft"`
	Players     []struct {
		Seat   int            `json:"seat"`
		Scores map[string]int `json:"scores"`
	} `json:"players"`
}

// ytBrain 스냅샷 기반 그리디 판단 (상태가 없어 필드도 없다)
type ytBrain struct{}

func newYTBrain() *ytBrain {
	return &ytBrain{}
}

// decide 공용 러너 계약 — yt_game_state 에만 반응한다
func (b *ytBrain) decide(msg YTMessage) *YTMessage {
	if msg.Type != YTMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[ytBotState](msg.Payload)
	if !ok {
		return nil
	}
	return b.decideState(state)
}

// decideState 자기 턴이면 굴림 또는 기록을 고른다 (아니면 nil)
func (b *ytBrain) decideState(state ytBotState) *YTMessage {
	me := state.YourSeat
	if state.Phase != YTPhasePlaying || state.CurrentSeat != me {
		return nil
	}
	var scores map[string]int
	for _, p := range state.Players {
		if p.Seat == me {
			scores = p.Scores
		}
	}
	if scores == nil {
		return nil
	}

	// 턴의 첫 굴림 — 홀드 없이 전부 굴린다
	if state.RollsLeft == YTMaxRolls {
		return &YTMessage{Type: YTMsgRoll, Payload: YTRollPayload{Hold: make([]bool, YTDiceCount)}}
	}
	if len(state.Dice) != YTDiceCount {
		return nil
	}

	category, points := ytBestCategory(state.Dice, scores)
	if category == "" {
		return nil // 빈 칸이 없다 — 도달 불가 안전망
	}
	if state.RollsLeft == 0 {
		return &YTMessage{Type: YTMsgScore, Payload: YTScorePayload{Category: category}}
	}

	// 이미 좋은 족보면 남은 굴림을 아끼고 즉시 기록한다
	if points >= ytBotStopScore {
		return &YTMessage{Type: YTMsgScore, Payload: YTScorePayload{Category: category}}
	}

	// 최다 눈 홀드 전략 — 개수가 같으면 높은 눈을 선호한다
	counts := ytCounts(state.Dice)
	bestFace, bestN := 1, 0
	for face := 1; face <= 6; face++ {
		if counts[face] >= bestN {
			bestFace, bestN = face, counts[face]
		}
	}
	if bestN == YTDiceCount {
		// 이미 5개 동일 — 전부 고정한 굴림은 서버가 거부하므로 즉시 기록
		return &YTMessage{Type: YTMsgScore, Payload: YTScorePayload{Category: category}}
	}
	hold := make([]bool, YTDiceCount)
	for i, d := range state.Dice {
		hold[i] = d == bestFace
	}
	return &YTMessage{Type: YTMsgRoll, Payload: YTRollPayload{Hold: hold}}
}

// ytBestCategory 빈 칸 중 현재 주사위로 최고 점수인 칸과 그 점수.
// 전부 0점이면 상단 최소 칸(낮은 눈부터)에 0을 적어 칸을 희생한다.
// 빈 칸이 없으면 ("", 0) — 호출부가 걸러낸다.
func ytBestCategory(dice []int, scores map[string]int) (string, int) {
	best, bestPoints := "", -1
	for _, cat := range ytCategories {
		if scores[cat] != -1 {
			continue
		}
		points := ytCategoryScore(cat, dice)
		if points > bestPoints {
			best, bestPoints = cat, points
		}
	}
	if best == "" {
		return "", 0
	}
	if bestPoints <= 0 {
		for _, cat := range ytCategories[:ytUpperCategoryCount] {
			if scores[cat] == -1 {
				return cat, 0
			}
		}
	}
	return best, bestPoints
}

// ==================== 봇 소환 ====================

// spawnYTBot 대기실의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *YTHub) spawnYTBot(room *ytRoom, name string) bool {
	bot := &YTClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runYTBot(bot)
	return true
}

// runYTBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *YTHub) runYTBot(bot *YTClient) {
	brain := newYTBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m YTMessage) { h.gameMessage <- YTGameMessage{Client: bot, Message: m} },
		func(m YTMessage) bool { return m.Type == YTMsgGameOver || m.Type == YTMsgSessionExpired })
}

// ytRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록용)
func ytRoomHasBot(room *ytRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
