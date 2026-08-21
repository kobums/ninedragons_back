package server

import "strconv"

// ==================== 라스베가스 연습봇 ====================
//
// 스냅샷(vg_game_state)만 보고 반응하는 그리디 봇. 자기 차례에 굴린
// 주사위에서 배치할 눈 하나를 고른다:
//   1) 그 눈을 전부 배치했을 때 그 카지노의 단독 1등이 되는 눈이 있으면,
//      그중 최고 지폐가 가장 큰 카지노의 눈을 고른다.
//   2) 없으면 가장 많은 눈을 고른다 (동수면 높은 눈).
// AFK 자동 진행(vg_hub.go)은 더 단순한 최다 눈(vgMostCommonFace)을 쓴다.

// vgBotCasino 봇이 판단에 쓰는 카지노 최소 정보
type vgBotCasino struct {
	Face   int            `json:"face"`
	Bills  []int          `json:"bills"`
	Placed map[string]int `json:"placed"` // JSON 맵 키는 문자열 좌석 번호
}

// vgBotState 봇이 상태 스냅샷에서 꺼내 쓰는 최소 정보
type vgBotState struct {
	YourSeat    int           `json:"yourSeat"`
	Phase       VGPhase       `json:"phase"`
	CurrentSeat int           `json:"currentSeat"`
	Dice        []int         `json:"dice"`
	Casinos     []vgBotCasino `json:"casinos"`
}

// vgBrain 스냅샷 기반 그리디 판단 (상태가 없어 필드도 없다)
type vgBrain struct{}

func newVGBrain() *vgBrain {
	return &vgBrain{}
}

// decide 공용 러너 계약 — vg_game_state 에만 반응한다
func (b *vgBrain) decide(msg VGMessage) *VGMessage {
	if msg.Type != VGMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[vgBotState](msg.Payload)
	if !ok {
		return nil
	}
	return b.decideState(state)
}

// decideState 자기 차례면 배치할 눈을 고른다 (아니면 nil)
func (b *vgBrain) decideState(state vgBotState) *VGMessage {
	me := state.YourSeat
	if state.Phase != VGPhasePlacing || state.CurrentSeat != me || len(state.Dice) == 0 {
		return nil
	}
	face := vgBotChooseFace(me, state.Dice, state.Casinos)
	if face == 0 {
		return nil
	}
	return &VGMessage{Type: VGMsgPlace, Payload: VGPlacePayload{Face: face}}
}

// vgBotChooseFace 그리디 눈 선택 — 배치 시 단독 1등이 되는 눈 중 최고
// 지폐가 가장 큰 카지노의 눈, 없으면 최다 눈. 빈 주사위면 0.
func vgBotChooseFace(me int, dice []int, casinos []vgBotCasino) int {
	counts := vgCounts(dice)
	bestFace, bestBill := 0, 0
	for _, c := range casinos {
		if c.Face < 1 || c.Face > 6 || counts[c.Face] == 0 || len(c.Bills) == 0 {
			continue
		}
		mine := c.Placed[strconv.Itoa(me)] + counts[c.Face]
		lead := true
		for seatKey, n := range c.Placed {
			if seatKey != strconv.Itoa(me) && n >= mine {
				lead = false
				break
			}
		}
		if !lead {
			continue
		}
		if c.Bills[0] > bestBill {
			bestFace, bestBill = c.Face, c.Bills[0]
		}
	}
	if bestFace != 0 {
		return bestFace
	}
	return vgMostCommonFace(dice)
}

// ==================== 봇 소환 ====================

// spawnVGBot 대기실의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *VGHub) spawnVGBot(room *vgRoom, name string) bool {
	bot := &VGClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runVGBot(bot)
	return true
}

// runVGBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *VGHub) runVGBot(bot *VGClient) {
	brain := newVGBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m VGMessage) { h.gameMessage <- VGGameMessage{Client: bot, Message: m} },
		func(m VGMessage) bool { return m.Type == VGMsgGameOver || m.Type == VGMsgSessionExpired })
}

// vgRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록용)
func vgRoomHasBot(room *vgRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
