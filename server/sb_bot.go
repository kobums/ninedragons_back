package server

import (
	"fmt"
	"math/rand"
	"time"
)

// ==================== 사보타지 연습봇 ====================
//
// 스냅샷(sb_game_state)만 보고 반응한다. 봇도 사람과 같은 조건 — 자기
// yourRole·yourHand 만 알고 남의 손패·정체·금 위치는 모른다.
//
//   - 광부 봇: ① 내 장비 수리 ② 금맥에 가까워지는 배치(목표 타일을 여는
//     배치가 최우선) ③ 남의 장비 수리 ④ 지도 ⑤ 쓸모 낮은 카드 버리기.
//   - 파괴꾼 봇: 대놓고 방해하면 바로 들키므로 확률(sbBotSabotageRate)로만
//     장비 파괴·낙석을 쓴다. 평소에는 막다른 타일로 요지를 막거나, 쓸모 있는
//     길 타일을 조용히 버려(sbBotWithholdRate) 판에서 없앤다. 목표 타일을
//     여는 배치는 절대 하지 않는다 (3분의 1 확률로 그 자리에서 진다).
//
// 같은 차례에 스냅샷이 여러 번 와도(관전 입장·접속 변화 등) 한 번만
// 행동하도록 상태 식별키로 중복을 걸러낸다.

// 봇이 "생각하는" 시간 (테스트에서 짧게 낮춘다)
var (
	sbBotDelay    = 700 * time.Millisecond
	sbBotJitterMs = 700
)

// 봇 정책 계수 (밸런스 조정 손잡이 — 봇 승률 측정 테스트가 이 값을 검증한다)
var (
	// sbBotSabotageRate 파괴꾼이 대놓고 방해(장비 파괴·낙석)할 확률
	sbBotSabotageRate = 0.4
	// sbBotWithholdRate 파괴꾼이 쓸모 있는 길 타일을 조용히 버릴 확률
	sbBotWithholdRate = 0.55
	// sbBotSelfRepairRate 파괴꾼이 자기 장비를 고칠 확률 (광부는 항상 고친다)
	sbBotSelfRepairRate = 0.5
)

// sbBotPlayerView 봇이 스냅샷에서 꺼내 쓰는 좌석 정보
type sbBotPlayerView struct {
	Seat      int     `json:"seat"`
	HandCount int     `json:"handCount"`
	Tools     SBTools `json:"tools"`
}

// sbBotState 봇이 상태 스냅샷에서 꺼내 쓰는 최소 정보
type sbBotState struct {
	YourSeat    int               `json:"yourSeat"`
	Phase       SBPhase           `json:"phase"`
	CurrentSeat int               `json:"currentSeat"`
	DeckLeft    int               `json:"deckLeft"`
	Turns       int               `json:"turns"`
	Board       []SBBoardCell     `json:"board"`
	YourRole    string            `json:"yourRole"`
	YourHand    []SBCard          `json:"yourHand"`
	Players     []sbBotPlayerView `json:"players"`
}

// sbBrain 스냅샷 기반 판단 (봇 대체 좌석도 같은 두뇌를 쓴다)
type sbBrain struct {
	rng *rand.Rand
	// lastKey 마지막으로 행동한 차례의 식별키 (중복 행동 방지)
	lastKey string
}

func newSBBrain() *sbBrain {
	return &sbBrain{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

// decide 공용 러너 계약 — sb_game_state 에만 반응한다
func (b *sbBrain) decide(msg SBMessage) *SBMessage {
	if msg.Type != SBMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[sbBotState](msg.Payload)
	if !ok {
		return nil
	}
	return b.decideState(state)
}

// think 사람처럼 잠깐 뜸을 들인다 (테스트에서는 var 를 낮춰 즉시 진행한다)
func (b *sbBrain) think() {
	d := sbBotDelay
	if sbBotJitterMs > 0 {
		d += time.Duration(b.rng.Intn(sbBotJitterMs)) * time.Millisecond
	}
	if d > 0 {
		time.Sleep(d)
	}
}

// decideState 자기 차례면 정확히 한 수를 결정한다
func (b *sbBrain) decideState(s sbBotState) *SBMessage {
	me := s.YourSeat
	if me < 0 || me >= len(s.Players) {
		return nil
	}
	if s.Phase != SBPhasePlaying || s.CurrentSeat != me || len(s.YourHand) == 0 {
		return nil
	}
	key := fmt.Sprintf("%s|%d|%d|%d", s.Phase, s.Turns, s.CurrentSeat, len(s.YourHand))
	if b.lastKey == key {
		return nil
	}
	b.lastKey = key

	board := sbBoardFromView(s.Board)
	var move *SBMessage
	if s.YourRole == string(SBRoleSaboteur) {
		move = b.playSaboteur(s, board)
	} else {
		move = b.playMiner(s, board)
	}
	if move == nil { // 방어선 — 버리기는 언제나 가능하다
		move = &SBMessage{Type: SBMsgDiscard,
			Payload: SBDiscardPayload{Index: b.worstCard(s)}}
	}
	b.think()
	return move
}

// ==================== 판 재구성 / 평가 ====================

// sbBoardFromView 공개 스냅샷의 판 목록을 순수 규칙이 쓰는 격자로 되돌린다
func sbBoardFromView(cells []SBBoardCell) []*SBCell {
	board := make([]*SBCell, SBCols*SBRows)
	for _, v := range cells {
		if !sbInBoard(v.Col, v.Row) {
			continue
		}
		goalIndex := -1
		for i, gc := range sbGoalCells {
			if gc[0] == v.Col && gc[1] == v.Row {
				goalIndex = i
			}
		}
		board[sbIdx(v.Col, v.Row)] = &SBCell{
			Col: v.Col, Row: v.Row, Kind: v.Kind,
			Up: v.Up, Right: v.Right, Down: v.Down, Left: v.Left,
			Dead: v.Dead, GoalIndex: goalIndex, Revealed: v.Revealed,
		}
	}
	return board
}

// sbCellGoalDist 한 칸에서 아직 뒤집히지 않은 목표 타일까지의 최소 맨해튼 거리
func sbCellGoalDist(board []*SBCell, col, row int) int {
	best := 999
	for _, gc := range sbGoalCells {
		goal := board[sbIdx(gc[0], gc[1])]
		if goal != nil && goal.Revealed {
			continue
		}
		if d := abs(col-gc[0]) + abs(row-gc[1]); d < best {
			best = d
		}
	}
	return best
}

// sbOpenScore 이어진 길의 "자라날 수 있는 끝"을 본다.
//
//	dist — 열린 길목(통로가 빈 칸을 향한 자리) 중 금맥에 가장 가까운 거리
//	ends — 열린 길목의 수 (많을수록 다음 수의 선택지가 넓다)
//
// 놓은 타일이 통로를 앞으로 뻗어야 dist 가 줄어든다. 옆으로 새거나 길목을
// 틀어막는 배치는 값이 그대로거나 나빠지므로 광부 봇이 알아서 피한다.
func sbOpenScore(board []*SBCell) (dist, ends int) {
	dist = 999
	for idx := range sbReachable(board) {
		cell := board[idx]
		if cell == nil || cell.Dead || cell.Kind == SBTileGoal {
			continue // 막다른·목표 칸에서는 더 뻗지 못한다
		}
		for d := 0; d < 4; d++ {
			if !sbCellSide(cell, d) {
				continue
			}
			nc, nr := cell.Col+sbDirs[d].dc, cell.Row+sbDirs[d].dr
			if !sbInBoard(nc, nr) || board[sbIdx(nc, nr)] != nil {
				continue
			}
			ends++
			if g := sbCellGoalDist(board, nc, nr); g < dist {
				dist = g
			}
		}
	}
	return dist, ends
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// sbCandidateCells 놓을 수 있을 법한 빈 칸 — 이어진 길에 맞닿은 칸만 훑는다
// (판 전체 45칸을 다 볼 필요가 없다)
func sbCandidateCells(board []*SBCell, reach map[int]bool) [][2]int {
	seen := map[int]bool{}
	cells := [][2]int{}
	for idx := range reach {
		cell := board[idx]
		if cell == nil || cell.Dead || cell.Kind == SBTileGoal {
			continue
		}
		for d := 0; d < 4; d++ {
			nc, nr := cell.Col+sbDirs[d].dc, cell.Row+sbDirs[d].dr
			if !sbInBoard(nc, nr) {
				continue
			}
			nIdx := sbIdx(nc, nr)
			if board[nIdx] != nil || seen[nIdx] {
				continue
			}
			seen[nIdx] = true
			cells = append(cells, [2]int{nc, nr})
		}
	}
	return cells
}

// sbMove 후보 배치 한 수와 그 평가
type sbMove struct {
	index int
	col   int
	row   int
	flip  bool
	dead  bool
	// reveals 이 수로 목표 타일이 열리는가
	reveals bool
	// dist 놓은 뒤 열린 길목에서 금맥까지의 최소 거리 (작을수록 광부에게 좋다)
	dist int
	// ends 놓은 뒤 열린 길목 수 (같은 거리면 많은 쪽이 낫다)
	ends int
}

// betterFor 광부 기준으로 이 수가 other 보다 나은가
// (금맥에 더 가깝고 → 길목이 더 넓고 → 더 오른쪽)
func (m sbMove) betterFor(other sbMove) bool {
	if m.dist != other.dist {
		return m.dist < other.dist
	}
	if m.ends != other.ends {
		return m.ends > other.ends
	}
	return m.col > other.col
}

// sbPlaceWith 판을 복사해 타일 하나를 놓아 본 결과 (원본 불변)
func sbPlaceWith(board []*SBCell, tile SBCard, col, row int) []*SBCell {
	next := make([]*SBCell, len(board))
	copy(next, board)
	next[sbIdx(col, row)] = &SBCell{
		Col: col, Row: row, Kind: SBTilePath,
		Up: tile.Up, Right: tile.Right, Down: tile.Down, Left: tile.Left,
		Dead: tile.Dead, GoalIndex: -1,
	}
	return next
}

// legalMoves 손패의 길 타일로 놓을 수 있는 모든 수를 평가해 모은다
func (b *sbBrain) legalMoves(s sbBotState, board []*SBCell) []sbMove {
	reach := sbReachable(board)
	cells := sbCandidateCells(board, reach)
	before := len(sbTouchedGoals(board))

	moves := []sbMove{}
	for i, card := range s.YourHand {
		if !sbIsTile(card) {
			continue
		}
		flips := []bool{false}
		if card.Flipable {
			flips = append(flips, true)
		}
		for _, flip := range flips {
			tile := card
			if flip {
				tile = sbFlip(tile)
			}
			for _, cell := range cells {
				if sbCanPlaceReach(board, reach, tile, cell[0], cell[1]) != nil {
					continue
				}
				after := sbPlaceWith(board, tile, cell[0], cell[1])
				dist, ends := sbOpenScore(after)
				moves = append(moves, sbMove{
					index: i, col: cell[0], row: cell[1], flip: flip, dead: tile.Dead,
					reveals: len(sbTouchedGoals(after)) > before,
					dist:    dist, ends: ends,
				})
			}
		}
	}
	return moves
}

func sbPlaceMsg(m sbMove) *SBMessage {
	return &SBMessage{Type: SBMsgPlace,
		Payload: SBPlacePayload{Index: m.index, Col: m.col, Row: m.row, Flip: m.flip}}
}

// ==================== 광부 ====================

func (b *sbBrain) playMiner(s sbBotState, board []*SBCell) *SBMessage {
	// ① 내 장비부터 고친다 — 고치지 않으면 길을 놓을 수 없다
	if m := b.repairMove(s, s.YourSeat); m != nil {
		return m
	}

	var moves []sbMove
	best := sbMove{index: -1}
	curDist, _ := sbOpenScore(board)
	if s.myTools().sbToolsAllOK() {
		moves = b.legalMoves(s, board)

		// ② 목표 타일을 여는 수가 있으면 무조건 그것부터
		for _, m := range moves {
			if !m.dead && m.reveals {
				return sbPlaceMsg(m)
			}
		}
		// ③ 금맥 쪽으로 길목을 미는 배치 (막다른 타일은 놓지 않는다)
		for _, m := range moves {
			if m.dead {
				continue
			}
			if best.index < 0 || m.betterFor(best) {
				best = m
			}
		}
		if best.index >= 0 && best.dist < curDist {
			return sbPlaceMsg(best)
		}
	}

	// ④ 동료의 장비 수리 (누가 광부인지는 모르지만 길은 누가 놓아도 좋다)
	for _, p := range s.Players {
		if p.Seat == s.YourSeat {
			continue
		}
		if m := b.repairMove(s, p.Seat); m != nil {
			return m
		}
	}

	// ⑤ 지도로 목표 타일을 살펴본다
	if m := b.mapMove(s, board); m != nil {
		return m
	}

	// ⑥ 버릴 잡카드가 남아 있으면 길 타일은 손에 쥐고 그것부터 버린다.
	// 전진하지 못하는 배치로 귀한 길 타일을 태우지 않는 것이 핵심이다.
	if idx := b.spareCard(s); idx >= 0 {
		return &SBMessage{Type: SBMsgDiscard, Payload: SBDiscardPayload{Index: idx}}
	}

	// ⑦ 손에 길 타일밖에 없다 — 그나마 금맥에 가까운 자리에 놓아 길목을 넓힌다
	if best.index >= 0 {
		return sbPlaceMsg(best)
	}
	return &SBMessage{Type: SBMsgDiscard, Payload: SBDiscardPayload{Index: b.worstCard(s)}}
}

// spareCard 광부가 아깝지 않게 버릴 수 있는 카드 (길 타일이 아닌 것 중
// 지금 쓸모가 없는 것). 없으면 -1.
func (b *sbBrain) spareCard(s sbBotState) int {
	for _, kind := range []SBCardKind{SBCardDeadend, SBCardBreak, SBCardRockfall, SBCardMap, SBCardRepair} {
		for i, c := range s.YourHand {
			if c.Kind == kind {
				return i
			}
		}
	}
	return -1
}

// ==================== 파괴꾼 ====================

func (b *sbBrain) playSaboteur(s sbBotState, board []*SBCell) *SBMessage {
	// ① 내 장비는 절반만 고친다 (파괴꾼은 길을 놓을 일이 적다)
	if !s.myTools().sbToolsAllOK() && b.rng.Float64() < sbBotSelfRepairRate {
		if m := b.repairMove(s, s.YourSeat); m != nil {
			return m
		}
	}

	// ② 대놓고 방해 — 확률로만 (매번 하면 바로 들킨다)
	if b.rng.Float64() < sbBotSabotageRate {
		if m := b.breakMove(s); m != nil {
			return m
		}
		if m := b.rockfallMove(s, board); m != nil {
			return m
		}
	}

	// 여기부터는 "대놓고"가 아닌 수다 — 전진이 아닌 배치나 버리기 (스펙대로)
	var moves []sbMove
	curDist, _ := sbOpenScore(board)
	if s.myTools().sbToolsAllOK() {
		moves = b.legalMoves(s, board)

		// ③ 막다른 타일로 금맥에 가장 가까운 길목을 틀어막는다
		// (겉보기엔 평범한 배치라 좀처럼 들키지 않는다)
		best, bestScore := sbMove{index: -1}, 999
		for _, m := range moves {
			if !m.dead {
				continue
			}
			if score := sbCellGoalDist(board, m.col, m.row); best.index < 0 || score < bestScore {
				best, bestScore = m, score
			}
		}
		if best.index >= 0 {
			return sbPlaceMsg(best)
		}
	}

	// ④ 쓸모 있는 길 타일을 가끔 조용히 버려 판에서 없앤다
	if idx := s.firstTile(false); idx >= 0 && b.rng.Float64() < sbBotWithholdRate {
		return &SBMessage{Type: SBMsgDiscard, Payload: SBDiscardPayload{Index: idx}}
	}

	// ⑤ 전진하지 않는 배치 (목표를 여는 수는 절대 두지 않는다 —
	// 3분의 1 확률로 그 자리에서 진다)
	for _, m := range moves {
		if m.reveals || m.dist < curDist {
			continue
		}
		return sbPlaceMsg(m)
	}

	// ⑥ 지도 — 어느 쪽이 금인지 알아 두면 막을 곳을 고르기 쉽다
	if m := b.mapMove(s, board); m != nil {
		return m
	}

	return &SBMessage{Type: SBMsgDiscard, Payload: SBDiscardPayload{Index: b.worstCard(s)}}
}

// ==================== 행동 카드 고르기 ====================

// repairMove 대상 좌석의 망가진 장비를 고치는 카드가 손에 있으면 그 수를 만든다
func (b *sbBrain) repairMove(s sbBotState, seat int) *SBMessage {
	tools := s.toolsOf(seat)
	for i, card := range s.YourHand {
		if card.Kind != SBCardRepair || tools.get(card.Tool) {
			continue
		}
		return &SBMessage{Type: SBMsgAction,
			Payload: SBActionPayload{Index: i, TargetSeat: seat, Tool: card.Tool}}
	}
	return nil
}

// breakMove 남의 멀쩡한 장비를 망가뜨리는 수 — 손패가 많은(=아직 일할 수
// 있는) 좌석을 고른다
func (b *sbBrain) breakMove(s sbBotState) *SBMessage {
	best, bestSeat, bestCards := -1, -1, -1
	for i, card := range s.YourHand {
		if card.Kind != SBCardBreak {
			continue
		}
		for _, p := range s.Players {
			if p.Seat == s.YourSeat || !p.Tools.get(card.Tool) {
				continue
			}
			if p.HandCount > bestCards {
				best, bestSeat, bestCards = i, p.Seat, p.HandCount
			}
		}
	}
	if best < 0 {
		return nil
	}
	return &SBMessage{Type: SBMsgAction,
		Payload: SBActionPayload{Index: best, TargetSeat: bestSeat}}
}

// rockfallMove 낙석 — 이어진 길에서 금맥에 가장 가까운 타일을 걷어낸다
func (b *sbBrain) rockfallMove(s sbBotState, board []*SBCell) *SBMessage {
	idx := -1
	for i, card := range s.YourHand {
		if card.Kind == SBCardRockfall {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}
	target := (*SBCell)(nil)
	for cellIdx := range sbReachable(board) {
		cell := board[cellIdx]
		if cell == nil || cell.Kind != SBTilePath {
			continue
		}
		if target == nil || cell.Col > target.Col {
			target = cell
		}
	}
	if target == nil {
		return nil
	}
	return &SBMessage{Type: SBMsgAction,
		Payload: SBActionPayload{Index: idx, Col: target.Col, Row: target.Row}}
}

// mapMove 지도 — 아직 뒤집히지 않은 목표 타일 하나를 몰래 본다
func (b *sbBrain) mapMove(s sbBotState, board []*SBCell) *SBMessage {
	idx := -1
	for i, card := range s.YourHand {
		if card.Kind == SBCardMap {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}
	for _, gc := range sbGoalCells {
		cell := board[sbIdx(gc[0], gc[1])]
		if cell == nil || cell.Kind != SBTileGoal || cell.Revealed {
			continue
		}
		return &SBMessage{Type: SBMsgAction,
			Payload: SBActionPayload{Index: idx, Col: gc[0], Row: gc[1]}}
	}
	return nil
}

// ==================== 손패 도우미 ====================

func (s sbBotState) toolsOf(seat int) SBTools {
	for _, p := range s.Players {
		if p.Seat == seat {
			return p.Tools
		}
	}
	return SBTools{Pick: true, Cart: true, Lamp: true}
}

func (s sbBotState) myTools() SBTools { return s.toolsOf(s.YourSeat) }

// firstTile 손패에서 첫 길 타일 (dead=true 면 막다른 타일만 찾는다)
func (s sbBotState) firstTile(dead bool) int {
	for i, c := range s.YourHand {
		if sbIsTile(c) && c.Dead == dead {
			return i
		}
	}
	return -1
}

// sbDiscardRank 버리기 우선순위 — 값이 작을수록 먼저 버린다.
// 진영에 따라 "쓸모없는 카드"의 정의가 정반대다.
func sbDiscardRank(role string, c SBCard) int {
	if role == string(SBRoleSaboteur) {
		switch c.Kind {
		case SBCardPath:
			return 0 // 길 타일을 없애는 것이 최고의 방해다
		case SBCardRepair:
			return 1
		case SBCardMap:
			return 2
		case SBCardDeadend:
			return 3
		case SBCardRockfall:
			return 4
		}
		return 5 // break
	}
	switch c.Kind {
	case SBCardDeadend:
		return 0
	case SBCardBreak:
		return 1
	case SBCardRockfall:
		return 2
	case SBCardMap:
		return 3
	case SBCardRepair:
		return 4
	}
	return 5 // path
}

// worstCard 손패에서 가장 아쉽지 않은 카드의 인덱스
func (b *sbBrain) worstCard(s sbBotState) int {
	best, bestRank := 0, 99
	for i, c := range s.YourHand {
		if r := sbDiscardRank(s.YourRole, c); r < bestRank {
			best, bestRank = i, r
		}
	}
	return best
}

// ==================== 봇 소환 ====================

// spawnSBBot 대기실의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *SBHub) spawnSBBot(room *sbRoom, name string) bool {
	bot := &SBClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runSBBot(bot)
	return true
}

// takeoverSBBot 유예 만료 좌석을 이어받는 연습봇 — 이름·좌석을 유지해
// 진행 중인 차례가 그대로 이어진다
func (h *SBHub) takeoverSBBot(room *sbRoom, seat int, name string) *SBClient {
	bot := &SBClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	bot.GameID = room.Game.ID
	bot.Seat = seat
	h.runSBBot(bot)
	return bot
}

// runSBBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *SBHub) runSBBot(bot *SBClient) {
	brain := newSBBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m SBMessage) { h.gameMessage <- SBGameMessage{Client: bot, Message: m} },
		func(m SBMessage) bool { return m.Type == SBMsgGameOver || m.Type == SBMsgSessionExpired })
}

// sbRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록용)
func sbRoomHasBot(room *sbRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
