package server

import "math/rand"

// ==================== 가이스터 연습봇 ====================

// gsBotState 봇이 쓰는 최소 스냅샷
type gsBotState struct {
	YourSide    string `json:"yourSide"`
	Phase       string `json:"phase"`
	CurrentSide string `json:"currentSide"`
	YourReady   bool   `json:"yourReady"`
	Ghosts      []struct {
		Side string `json:"side"`
		Row  int    `json:"row"`
		Col  int    `json:"col"`
		Good *bool  `json:"good"`
	} `json:"ghosts"`
}

// gsBrain 배치 단계에선 좋은 유령 4개를 무작위 배치하고, 플레이 단계에선
// 탈출 가능하면 탈출, 아니면 유령·방향을 카운터로 회전시키며 합법 수를 둔다.
type gsBrain struct {
	counter int
	rng     *rand.Rand
}

func newGSBrain(rng *rand.Rand) *gsBrain {
	return &gsBrain{rng: rng}
}

func (b *gsBrain) decide(msg GSMessage) *GSMessage {
	if msg.Type != GSMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[gsBotState](msg.Payload)
	if !ok {
		return nil
	}

	// 비밀 배치: 내 8칸 중 4칸을 무작위로 좋은 유령 자리로 제출
	if state.Phase == string(GSPhaseSetup) && !state.YourReady {
		mine := []GSCell{}
		for _, ghost := range state.Ghosts {
			if ghost.Side == state.YourSide {
				mine = append(mine, GSCell{Row: ghost.Row, Col: ghost.Col})
			}
		}
		if len(mine) < GSGoodCount {
			return nil
		}
		b.rng.Shuffle(len(mine), func(i, j int) { mine[i], mine[j] = mine[j], mine[i] })
		return &GSMessage{Type: GSMsgSubmitSetup, Payload: GSSubmitSetupPayload{GoodCells: mine[:GSGoodCount]}}
	}

	if state.Phase != string(GSPhasePlay) || state.CurrentSide != state.YourSide {
		return nil
	}

	occupied := map[GSCell]string{}
	mine := []int{}
	for i, ghost := range state.Ghosts {
		occupied[GSCell{ghost.Row, ghost.Col}] = ghost.Side
		if ghost.Side == state.YourSide {
			mine = append(mine, i)
		}
	}
	if len(mine) == 0 {
		return nil
	}
	mySide := GSSide(state.YourSide)
	b.counter++

	// 탈출 가능하면 즉시 탈출
	for _, idx := range mine {
		ghost := state.Ghosts[idx]
		if ghost.Good != nil && *ghost.Good && gsIsExit(mySide, ghost.Row, ghost.Col) {
			return &GSMessage{Type: GSMsgMove, Payload: GSMovePayload{
				From: GSCell{ghost.Row, ghost.Col}, Escape: true,
			}}
		}
	}

	dirs := [][2]int{{-1, 0}, {0, -1}, {0, 1}, {1, 0}}
	for i := range mine {
		ghost := state.Ghosts[mine[(b.counter+i)%len(mine)]]
		from := GSCell{ghost.Row, ghost.Col}
		for j := range dirs {
			d := dirs[(b.counter+j)%len(dirs)]
			to := GSCell{ghost.Row + d[0], ghost.Col + d[1]}
			if to.Row < 0 || to.Row >= GSBoardSize || to.Col < 0 || to.Col >= GSBoardSize {
				continue
			}
			if occupied[to] == state.YourSide {
				continue
			}
			return &GSMessage{Type: GSMsgMove, Payload: GSMovePayload{From: from, To: to}}
		}
	}
	return nil
}

// spawnBot 방의 남은 자리에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *GSHub) spawnBot(room *gsRoom) {
	bot := &GSClient{wsClient: newBotWSClient(), Hub: h}
	side, err := room.Game.AddPlayer(bot.Name)
	if err != nil {
		return
	}
	bot.GameID = room.Game.ID
	bot.Side = side
	room.Clients[side] = bot
	h.sessions[bot.SessionID] = bot

	brain := newGSBrain(rand.New(rand.NewSource(h.rng.Int63())))
	go runBot(bot.Send,
		brain.decide,
		func(m GSMessage) { h.gameMessage <- GSGameMessage{Client: bot, Message: m} },
		func(m GSMessage) bool { return m.Type == GSMsgGameOver || m.Type == GSMsgSessionExpired })
}
