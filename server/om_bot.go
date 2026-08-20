package server

import (
	"math/rand"
	"time"
)

// ==================== 오목 연습봇 ====================

// omBotState 봇이 쓰는 최소 스냅샷
type omBotState struct {
	YourColor    string  `json:"yourColor"`
	CurrentColor string  `json:"currentColor"`
	Board        [][]int `json:"board"`
}

// omBrain 내 턴마다 모든 빈 칸을 채점해 최고점 칸에 둔다.
// 점수 = 공격(내가 두면 만드는 연속, 오픈 가중) ×2 + 수비(상대가 두면 만드는
// 연속 차단). 티어 간격을 넓게 잡아 우선순위가 자연히 성립한다:
// 내 5 완성 > 상대 5 차단 > 내 열린4 > 상대 열린4 차단 > 내 4 > 상대 4 차단 >
// 내 열린3 > 상대 열린3 차단 > … > 중앙 근접. 동점은 무작위.
type omBrain struct {
	rng *rand.Rand
}

func newOMBrain(rng *rand.Rand) *omBrain {
	return &omBrain{rng: rng}
}

func (b *omBrain) decide(msg OMMessage) *OMMessage {
	if msg.Type != OMMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[omBotState](msg.Payload)
	if !ok {
		return nil
	}
	if state.YourColor == "" || state.CurrentColor != state.YourColor {
		return nil
	}
	if len(state.Board) != OMBoardSize {
		return nil
	}

	me := omStone(OMColor(state.YourColor))
	opp := 3 - me

	bestScore := -1
	var best OMCell
	ties := 0
	for r := 0; r < OMBoardSize; r++ {
		if len(state.Board[r]) != OMBoardSize {
			return nil
		}
		for c := 0; c < OMBoardSize; c++ {
			if state.Board[r][c] != 0 {
				continue
			}
			score := omCellScore(state.Board, r, c, me, opp)
			switch {
			case score > bestScore:
				bestScore, best, ties = score, OMCell{Row: r, Col: c}, 1
			case score == bestScore:
				// 동점 무작위 선택 (reservoir sampling)
				ties++
				if b.rng.Intn(ties) == 0 {
					best = OMCell{Row: r, Col: c}
				}
			}
		}
	}
	if bestScore < 0 {
		// 빈 칸 없음 — 이미 만패로 끝난 판
		return nil
	}
	return &OMMessage{Type: OMMsgMove, Payload: OMMovePayload{Row: best.Row, Col: best.Col}}
}

// omThreatValue 연속 길이·열린 끝 개수 조합의 위협 가치.
// 티어 간 간격을 4방향 합산(최대 ×4)으로도 역전되지 않게 벌려 둔다.
func omThreatValue(length, open int) int {
	switch {
	case length >= 5:
		return 10_000_000 // 즉시 승리
	case length == 4 && open == 2:
		return 100_000 // 열린 4 — 막을 수 없는 승리 예약
	case length == 4 && open == 1:
		return 8_000 // 닫힌 4 — 즉시 승리 위협
	case length == 3 && open == 2:
		return 3_000 // 열린 3
	case length == 3 && open == 1:
		return 150
	case length == 2 && open == 2:
		return 40
	case length == 2 && open == 1:
		return 6
	default:
		return 1
	}
}

// omPlacementValue (row,col)에 stone 을 놓았을 때 만들어지는 4방향 위협 가치 합
func omPlacementValue(board [][]int, row, col, stone int) int {
	total := 0
	for _, dir := range omDirs {
		length := 1
		open := 0

		r, c := row+dir[0], col+dir[1]
		for omInBoard(r, c) && board[r][c] == stone {
			length++
			r, c = r+dir[0], c+dir[1]
		}
		if omInBoard(r, c) && board[r][c] == 0 {
			open++
		}

		r, c = row-dir[0], col-dir[1]
		for omInBoard(r, c) && board[r][c] == stone {
			length++
			r, c = r-dir[0], c-dir[1]
		}
		if omInBoard(r, c) && board[r][c] == 0 {
			open++
		}

		total += omThreatValue(length, open)
	}
	return total
}

// omCellScore 빈 칸 하나의 점수: 공격 ×2 + 수비, 마지막 자리에서 중앙 근접 가산
func omCellScore(board [][]int, row, col, me, opp int) int {
	attack := omPlacementValue(board, row, col, me)
	defense := omPlacementValue(board, row, col, opp)

	center := OMBoardSize / 2
	dr, dc := row-center, col-center
	if dr < 0 {
		dr = -dr
	}
	if dc < 0 {
		dc = -dc
	}
	// 티어 값 ×100 이라 중앙 보너스(최대 28)는 티어를 넘볼 수 없다
	return (2*attack+defense)*100 + (2*OMBoardSize - (dr + dc))
}

// spawnBot 방의 남은 자리에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *OMHub) spawnBot(room *omRoom) {
	bot := &OMClient{wsClient: newBotWSClient(), Hub: h}
	color, err := room.Game.AddPlayer(bot.Name)
	if err != nil {
		return
	}
	bot.GameID = room.Game.ID
	bot.Color = color
	room.Clients[color] = bot
	h.sessions[bot.SessionID] = bot

	h.broadcastJoined(room, color, bot.Name)

	brain := newOMBrain(rand.New(rand.NewSource(time.Now().UnixNano())))
	go runBot(bot.Send,
		brain.decide,
		func(m OMMessage) { h.gameMessage <- OMGameMessage{Client: bot, Message: m} },
		func(m OMMessage) bool { return m.Type == OMMsgGameOver || m.Type == OMMsgSessionExpired })
}

// omRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록 판단용)
func omRoomHasBot(room *omRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
