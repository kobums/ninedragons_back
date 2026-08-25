package server

import (
	"errors"
	"fmt"
	"math/rand"
	"time"
)

// ==================== 사보타지 순수 규칙 ====================
//
// 역할 배분·덱 구성·차례 진행·경로 연결 판정·승패 판정만 다룬다.
// 클라이언트·타이머를 모르며, 허브(sb_hub.go)가 차례 마감(45초)을 걸고
// 이벤트 큐(DrainEvents)·개인 통지(DrainPrivates)를 방송한다.
//
// 이 파일의 심장은 sbCanPlace 다. 배치는 세 관문을 모두 통과해야 한다.
//  1. 판 범위 안이고 그 칸이 비어 있는가
//  2. 인접 4방향 각각 — 이웃 타일이 있으면 마주 보는 변의 통로 여부가
//     일치하는가 (통로↔통로 / 벽↔벽. 이웃이 없는 쪽은 자유)
//  3. 시작 타일에서 BFS 로 구한 "실제로 이어진 길"의 어느 칸과 통로로
//     맞닿는가 (막다른 타일은 내부가 막혀 있어 그 뒤로는 못 놓는다)
//
// 목표 타일만 예외다 — 뒷면이라 모양을 모르므로 이웃 규칙을 느슨하게 적용해
// 변 일치를 요구하지 않고 연결의 근거로도 삼지 않는다. 대신 길이 실제로
// 목표 타일을 향해 닿으면 그 타일을 공개하고(sbTouchedGoals) 공개하는 순간
// 사방이 뚫린 모양으로 회전 보정한다 (원작의 목표 카드는 십자다).

// NewSBGame 대기 상태의 새 게임
func NewSBGame(id string) *SBGame {
	return &SBGame{
		ID:          id,
		Players:     []*SBPlayer{},
		Phase:       SBPhaseWaiting,
		Board:       make([]*SBCell, SBCols*SBRows),
		Deck:        []SBCard{},
		CurrentSeat: -1,
		GoldIndex:   -1,
	}
}

// AddPlayer 대기실 입장. 좌석 번호를 돌려준다.
func (g *SBGame) AddPlayer(name string) (int, error) {
	if g.Phase != SBPhaseWaiting {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= SBMaxPlayers {
		return -1, fmt.Errorf("자리가 없습니다 (최대 %d명)", SBMaxPlayers)
	}
	seat := len(g.Players)
	g.Players = append(g.Players, &SBPlayer{
		Seat:  seat,
		Name:  name,
		Hand:  []SBCard{},
		Tools: SBTools{Pick: true, Cart: true, Lamp: true},
	})
	return seat, nil
}

// RemovePlayer 대기실 퇴장. 좌석을 앞으로 당겨 빈틈을 없앤다.
func (g *SBGame) RemovePlayer(seat int) {
	if g.Phase != SBPhaseWaiting || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i, p := range g.Players {
		p.Seat = i
	}
}

// CanStart 시작 가능 여부 (호스트 명시 시작 — 3인부터)
func (g *SBGame) CanStart() bool {
	return g.Phase == SBPhaseWaiting && len(g.Players) >= SBMinPlayers
}

// ==================== 격자 / 방향 ====================

// sbDirs 상·우·하·좌 순서의 이동량 (인덱스가 곧 방향 번호다)
var sbDirs = [4]struct{ dc, dr int }{{0, -1}, {1, 0}, {0, 1}, {-1, 0}}

// sbOpp 마주 보는 방향
func sbOpp(d int) int { return (d + 2) % 4 }

// sbDirLabel 방향 한글 표기 (오류 문구용)
func sbDirLabel(d int) string {
	return [4]string{"위", "오른", "아래", "왼"}[d]
}

// sbIdx 좌표 → 격자 인덱스
func sbIdx(col, row int) int { return row*SBCols + col }

// sbInBoard 판 범위 안인가
func sbInBoard(col, row int) bool {
	return col >= 0 && col < SBCols && row >= 0 && row < SBRows
}

// sbCardSide 카드의 d 방향 통로 여부
func sbCardSide(c SBCard, d int) bool {
	switch d {
	case 0:
		return c.Up
	case 1:
		return c.Right
	case 2:
		return c.Down
	}
	return c.Left
}

// sbCellSide 놓인 칸의 d 방향 통로 여부
func sbCellSide(c *SBCell, d int) bool {
	switch d {
	case 0:
		return c.Up
	case 1:
		return c.Right
	case 2:
		return c.Down
	}
	return c.Left
}

// sbNewBoard 시작 타일과 뒷면 목표 타일 3장이 깔린 새 판
func sbNewBoard() []*SBCell {
	board := make([]*SBCell, SBCols*SBRows)
	board[sbIdx(SBStartCol, SBStartRow)] = &SBCell{
		Col: SBStartCol, Row: SBStartRow, Kind: SBTileStart,
		Up: true, Right: true, Down: true, Left: true, GoalIndex: -1,
	}
	for i, gc := range sbGoalCells {
		board[sbIdx(gc[0], gc[1])] = &SBCell{
			Col: gc[0], Row: gc[1], Kind: SBTileGoal, GoalIndex: i,
		}
	}
	return board
}

// ==================== 경로 연결 판정 (이 게임의 핵심) ====================

// sbReachable 시작 타일에서 통로로 실제 이어진 칸 집합 (격자 인덱스 → true).
//
// 확장 규칙:
//   - 막다른 타일은 집합에 들어가되 확장하지 않는다 (내부가 막혀 있다)
//   - 목표 타일도 집합에 들어가되 확장하지 않는다 (판 끝이고, 닿았다는
//     사실만이 의미가 있다). 뒷면이라 모양을 모르므로 이웃이 목표를 향해
//     통로를 뻗기만 하면 닿은 것으로 본다 — 느슨한 이웃 규칙의 근거다.
//
// 순수 함수다 — board 를 읽기만 한다.
func sbReachable(board []*SBCell) map[int]bool {
	seen := map[int]bool{}
	start := board[sbIdx(SBStartCol, SBStartRow)]
	if start == nil {
		return seen
	}
	startIdx := sbIdx(SBStartCol, SBStartRow)
	seen[startIdx] = true
	queue := []int{startIdx}

	for len(queue) > 0 {
		idx := queue[0]
		queue = queue[1:]
		cell := board[idx]
		if cell == nil || cell.Dead || cell.Kind == SBTileGoal {
			continue // 막다른·목표 타일은 더 뻗지 않는다
		}
		for d := 0; d < 4; d++ {
			if !sbCellSide(cell, d) {
				continue
			}
			nc, nr := cell.Col+sbDirs[d].dc, cell.Row+sbDirs[d].dr
			if !sbInBoard(nc, nr) {
				continue
			}
			nIdx := sbIdx(nc, nr)
			nb := board[nIdx]
			if nb == nil || seen[nIdx] {
				continue
			}
			// 목표 타일은 느슨하게 — 향해서 통로를 뻗으면 닿은 것으로 본다
			if nb.Kind != SBTileGoal && !sbCellSide(nb, sbOpp(d)) {
				continue
			}
			seen[nIdx] = true
			queue = append(queue, nIdx)
		}
	}
	return seen
}

// sbCanPlace 길 타일 배치 가능 여부 — 순수 함수. 성립하면 nil.
//
// tile 은 이미 회전(flip)까지 적용된 최종 모양이어야 한다.
func sbCanPlace(board []*SBCell, tile SBCard, col, row int) error {
	return sbCanPlaceReach(board, sbReachable(board), tile, col, row)
}

// sbCanPlaceReach 이어진 길 집합을 미리 구해 둔 판정 (봇이 한 차례에 수십
// 개의 후보를 훑을 때 BFS 를 한 번만 돌기 위한 내부 진입점). 규칙은
// sbCanPlace 와 완전히 동일하다.
func sbCanPlaceReach(board []*SBCell, reach map[int]bool, tile SBCard, col, row int) error {
	if !sbIsTile(tile) {
		return errors.New("굴 카드가 아닙니다")
	}
	if !sbInBoard(col, row) {
		return errors.New("판 밖의 좌표입니다")
	}
	if board[sbIdx(col, row)] != nil {
		return errors.New("이미 카드가 놓인 칸입니다")
	}

	anchored := false
	for d := 0; d < 4; d++ {
		nc, nr := col+sbDirs[d].dc, row+sbDirs[d].dr
		if !sbInBoard(nc, nr) {
			continue // 판 밖 — 자유
		}
		nb := board[sbIdx(nc, nr)]
		if nb == nil {
			continue // 빈 칸 — 자유
		}
		if nb.Kind == SBTileGoal {
			// 뒷면 목표 타일은 모양을 모른다 — 변 일치를 묻지 않고
			// 연결의 근거로도 삼지 않는다 (닿음 판정은 배치 뒤에 한다)
			continue
		}
		mine := sbCardSide(tile, d)
		theirs := sbCellSide(nb, sbOpp(d))
		if mine != theirs {
			return fmt.Errorf("%s쪽 이웃 카드와 통로가 맞지 않습니다", sbDirLabel(d))
		}
		// 막다른 타일은 내부가 막혀 있어 이어진 길의 끝이다 — 그 뒤로는 못 놓는다
		if mine && !nb.Dead && reach[sbIdx(nc, nr)] {
			anchored = true
		}
	}
	if !anchored {
		return errors.New("시작 카드에서 이어진 길에 닿아야 합니다")
	}
	return nil
}

// sbTouchedGoals 시작 타일에서 이어진 길이 실제로 닿은 목표 타일 인덱스 목록
// (오름차순). 순수 함수다.
func sbTouchedGoals(board []*SBCell) []int {
	reach := sbReachable(board)
	touched := []int{}
	for i, gc := range sbGoalCells {
		cell := board[sbIdx(gc[0], gc[1])]
		if cell == nil || cell.Kind != SBTileGoal {
			continue
		}
		if reach[sbIdx(gc[0], gc[1])] {
			touched = append(touched, i)
		}
	}
	return touched
}

// sbFrontier 이어진 길이 도달한 가장 오른쪽 열 (봇의 전진 판단용).
// 목표 타일은 세지 않는다 (그 자체가 종점이라 전진 지표가 아니다).
func sbFrontier(board []*SBCell) int {
	best := -1
	for idx := range sbReachable(board) {
		cell := board[idx]
		if cell == nil || cell.Kind == SBTileGoal {
			continue
		}
		if cell.Col > best {
			best = cell.Col
		}
	}
	return best
}

// ==================== 덱 ====================

// sbBuildDeck 파일 상단 구성표대로 40장을 만든다 (셔플 전)
func sbBuildDeck() []SBCard {
	deck := make([]SBCard, 0, SBDeckSize)
	add := func(c SBCard, n int) {
		for i := 0; i < n; i++ {
			deck = append(deck, c)
		}
	}

	// 길 타일 29장 (막다른 4장 포함)
	add(sbTile(true, true, true, true, false), 6)   // 십자
	add(sbTile(false, true, false, true, false), 8) // 가로 직선
	add(sbTile(true, false, true, false, false), 3) // 세로 직선
	add(sbTile(true, true, false, false, false), 1) // 굽이 ┗
	add(sbTile(false, true, true, false, false), 1) // 굽이 ┏
	add(sbTile(false, false, true, true, false), 1) // 굽이 ┓
	add(sbTile(true, false, false, true, false), 1) // 굽이 ┛
	add(sbTile(true, true, true, false, false), 1)  // T자 ├
	add(sbTile(false, true, true, true, false), 1)  // T자 ┬
	add(sbTile(true, false, true, true, false), 1)  // T자 ┤
	add(sbTile(true, true, false, true, false), 1)  // T자 ┴
	add(sbTile(true, false, false, false, true), 1) // 막다른 ↑
	add(sbTile(false, true, false, false, true), 1) // 막다른 →
	add(sbTile(false, false, true, false, true), 1) // 막다른 ↓
	add(sbTile(false, false, false, true, true), 1) // 막다른 ←

	// 행동 카드 11장
	add(SBCard{Kind: SBCardMap}, 2)
	add(SBCard{Kind: SBCardRockfall}, 2)
	add(SBCard{Kind: SBCardBreak, Tool: SBToolPick}, 2)
	add(SBCard{Kind: SBCardBreak, Tool: SBToolCart}, 1)
	add(SBCard{Kind: SBCardBreak, Tool: SBToolLamp}, 1)
	add(SBCard{Kind: SBCardRepair, Tool: SBToolPick}, 1)
	add(SBCard{Kind: SBCardRepair, Tool: SBToolCart}, 1)
	add(SBCard{Kind: SBCardRepair, Tool: SBToolLamp}, 1)

	return deck
}

// draw 덱에서 1장 뽑아 손패에 넣는다 (덱이 비면 아무 일도 없다)
func (g *SBGame) draw(p *SBPlayer) {
	if len(g.Deck) == 0 {
		return
	}
	p.Hand = append(p.Hand, g.Deck[0])
	g.Deck = g.Deck[1:]
}

// ==================== 시작 ====================

// Start 게임 시작 — 역할 풀에서 인원수만큼 뽑아 배정하고 손패를 나눈다.
// 금덩이가 어느 목표 타일에 있는지는 서버만 안다.
func (g *SBGame) Start(rng *rand.Rand) error {
	if !g.CanStart() {
		return fmt.Errorf("%d명 이상 모여야 시작할 수 있습니다", SBMinPlayers)
	}
	n := len(g.Players)
	g.Ready = true
	g.StartedAt = time.Now()

	// 역할 풀(N+1장)에서 인원수만큼만 뽑는다 — 실제 파괴꾼 수는 아무도 모른다
	g.Pool = sbRolePoolFor(n)
	roles := make([]SBRole, 0, g.Pool.Miner+g.Pool.Saboteur)
	for i := 0; i < g.Pool.Miner; i++ {
		roles = append(roles, SBRoleMiner)
	}
	for i := 0; i < g.Pool.Saboteur; i++ {
		roles = append(roles, SBRoleSaboteur)
	}
	rng.Shuffle(len(roles), func(i, j int) { roles[i], roles[j] = roles[j], roles[i] })

	g.Board = sbNewBoard()
	g.GoldIndex = rng.Intn(len(sbGoalCells))

	deck := sbBuildDeck()
	rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })
	g.Deck = deck

	per := sbHandSize(n)
	for i, p := range g.Players {
		p.Role = roles[i]
		p.Tools = SBTools{Pick: true, Cart: true, Lamp: true}
		p.Hand = []SBCard{}
		for k := 0; k < per; k++ {
			g.draw(p)
		}
	}

	g.LastAction = nil
	g.Result = nil
	g.Turns = 0
	g.CurrentSeat = rng.Intn(n)
	g.Phase = SBPhasePlaying
	g.StateSeq++
	g.emit("game_started", g.CurrentSeat, fmt.Sprintf(
		"갱도에 내려갑니다 — 각자 %d장씩 받았습니다. %s님부터 시작합니다",
		per, g.Players[g.CurrentSeat].Name))
	return nil
}

// ==================== 이벤트 큐 ====================

func (g *SBGame) emit(kind string, seat int, msg string) {
	g.events = append(g.events, SBGameEvent{Kind: kind, Seat: seat, Message: msg})
}

// DrainEvents 쌓인 이벤트를 꺼내고 비운다 (허브가 방송)
func (g *SBGame) DrainEvents() []SBGameEvent {
	evs := g.events
	g.events = nil
	return evs
}

// DrainPrivates 쌓인 개인 통지를 꺼내고 비운다 (허브가 그 좌석에만 전송)
func (g *SBGame) DrainPrivates() []SBPrivate {
	pvs := g.privates
	g.privates = nil
	return pvs
}

// ==================== 차례 공통 ====================

// checkTurn 차례 검증 + 손패 인덱스 검증
func (g *SBGame) checkTurn(seat, index int) (*SBPlayer, error) {
	if g.Phase != SBPhasePlaying {
		return nil, errors.New("지금은 낼 수 없습니다")
	}
	if seat != g.CurrentSeat {
		return nil, errors.New("차례가 아닙니다")
	}
	if seat < 0 || seat >= len(g.Players) {
		return nil, errors.New("잘못된 좌석입니다")
	}
	p := g.Players[seat]
	if index < 0 || index >= len(p.Hand) {
		return nil, errors.New("잘못된 카드입니다")
	}
	return p, nil
}

// spend 낸 카드를 손패에서 빼고 1장 뽑는다 (덱이 비면 못 뽑는다)
func (g *SBGame) spend(p *SBPlayer, index int) {
	p.Hand = append(p.Hand[:index], p.Hand[index+1:]...)
	g.draw(p)
}

// note 마지막 행동 요약 기록 + 이벤트 방송
func (g *SBGame) note(seat int, kind, msg string) {
	name := ""
	if seat >= 0 && seat < len(g.Players) {
		name = g.Players[seat].Name
	}
	g.LastAction = &SBLastAction{Seat: seat, Name: name, Message: msg}
	g.emit(kind, seat, msg)
}

// endTurn 차례를 넘긴다. 손패가 남은 좌석이 하나도 없으면 파괴꾼 승으로
// 끝난다 — 매 차례 손패에서 1장이 영구히 빠지므로 반드시 도달한다.
func (g *SBGame) endTurn() {
	if g.Phase == SBPhaseGameOver {
		return
	}
	g.Turns++

	n := len(g.Players)
	for step := 1; step <= n; step++ {
		next := (g.CurrentSeat + step) % n
		if len(g.Players[next].Hand) > 0 {
			g.CurrentSeat = next
			g.StateSeq++
			return
		}
	}
	g.finishWith(string(SBRoleSaboteur), "exhausted",
		"카드가 모두 떨어졌습니다 — 금맥에 닿지 못해 방해꾼의 승리입니다")
}

// ==================== 길 타일 배치 ====================

// Place 손패의 길 타일을 판에 놓는다. flip 은 180° 회전이다.
func (g *SBGame) Place(seat, index, col, row int, flip bool) error {
	p, err := g.checkTurn(seat, index)
	if err != nil {
		return err
	}
	card := p.Hand[index]
	if !sbIsTile(card) {
		return errors.New("굴 카드가 아닙니다")
	}
	if !p.Tools.sbToolsAllOK() {
		return errors.New("도구가 부서져 굴 카드를 놓을 수 없습니다")
	}
	tile := card
	if flip {
		tile = sbFlip(tile)
	}
	if err := sbCanPlace(g.Board, tile, col, row); err != nil {
		return err
	}

	g.Board[sbIdx(col, row)] = &SBCell{
		Col: col, Row: row, Kind: SBTilePath,
		Up: tile.Up, Right: tile.Right, Down: tile.Down, Left: tile.Left,
		Dead: tile.Dead, GoalIndex: -1,
	}
	g.spend(p, index)

	shape := "길"
	if tile.Dead {
		shape = "막다른 길"
	}
	g.note(seat, "place", fmt.Sprintf("%s님이 (%d,%d)에 %s 카드를 놓았습니다",
		p.Name, col, row, shape))

	g.revealTouchedGoals()
	if g.Phase == SBPhaseGameOver {
		return nil
	}
	g.endTurn()
	return nil
}

// revealTouchedGoals 길이 닿은 목표 타일을 공개한다. 공개하는 순간 사방이
// 뚫린 모양으로 회전 보정해 이후 이웃 규칙과 어긋나지 않게 한다.
// 금덩이가 나오면 그 자리에서 광부 승리다.
func (g *SBGame) revealTouchedGoals() {
	for _, gi := range sbTouchedGoals(g.Board) {
		gc := sbGoalCells[gi]
		cell := g.Board[sbIdx(gc[0], gc[1])]
		if cell == nil || cell.Revealed {
			continue
		}
		cell.Revealed = true
		cell.Up, cell.Right, cell.Down, cell.Left = true, true, true, true
		cell.Gold = gi == g.GoldIndex

		if cell.Gold {
			g.emit("goal_revealed", -1, fmt.Sprintf(
				"(%d,%d) 목적지 카드가 열렸습니다 — 금덩이입니다!", gc[0], gc[1]))
			g.finishWith(string(SBRoleMiner), "gold", fmt.Sprintf(
				"(%d,%d)의 금맥에 길이 닿았습니다 — 광부의 승리입니다", gc[0], gc[1]))
			return
		}
		g.emit("goal_revealed", -1, fmt.Sprintf(
			"(%d,%d) 목적지 카드가 열렸습니다 — 돌덩이입니다", gc[0], gc[1]))
	}
}

// ==================== 행동 카드 ====================

// Action 행동 카드를 낸다 (지도·낙석·파괴·수리)
func (g *SBGame) Action(seat, index int, req SBActionPayload) error {
	p, err := g.checkTurn(seat, index)
	if err != nil {
		return err
	}
	card := p.Hand[index]

	switch card.Kind {
	case SBCardMap:
		if err := g.playMap(p, req); err != nil {
			return err
		}
	case SBCardRockfall:
		if err := g.playRockfall(p, req); err != nil {
			return err
		}
	case SBCardBreak:
		if err := g.playBreak(p, card, req); err != nil {
			return err
		}
	case SBCardRepair:
		if err := g.playRepair(p, card, req); err != nil {
			return err
		}
	default:
		return errors.New("행동 카드가 아닙니다")
	}

	g.spend(p, index)
	g.endTurn()
	return nil
}

// playMap 지도 — 목표 타일 1장을 낸 사람만 몰래 본다
func (g *SBGame) playMap(p *SBPlayer, req SBActionPayload) error {
	if !sbInBoard(req.Col, req.Row) {
		return errors.New("판 밖의 좌표입니다")
	}
	cell := g.Board[sbIdx(req.Col, req.Row)]
	if cell == nil || cell.Kind != SBTileGoal {
		return errors.New("목적지 카드만 들여다볼 수 있습니다")
	}
	if cell.Revealed {
		return errors.New("이미 공개된 목적지 카드입니다")
	}
	// 결과는 방송하지 않는다 — 허브가 이 좌석에만 sb_map 으로 보낸다
	g.privates = append(g.privates, SBPrivate{
		Seat: p.Seat, Index: cell.GoalIndex, Gold: cell.GoalIndex == g.GoldIndex,
	})
	g.note(p.Seat, "map", fmt.Sprintf("%s님이 (%d,%d) 목적지 카드를 몰래 살펴봤습니다",
		p.Name, req.Col, req.Row))
	return nil
}

// playRockfall 낙석 — 놓인 길 타일 1장을 걷어낸다 (시작·목표 타일은 불가)
func (g *SBGame) playRockfall(p *SBPlayer, req SBActionPayload) error {
	if !sbInBoard(req.Col, req.Row) {
		return errors.New("판 밖의 좌표입니다")
	}
	cell := g.Board[sbIdx(req.Col, req.Row)]
	if cell == nil {
		return errors.New("걷어낼 카드가 없습니다")
	}
	if cell.Kind != SBTilePath {
		return errors.New("시작·목적지 카드는 걷어낼 수 없습니다")
	}
	g.Board[sbIdx(req.Col, req.Row)] = nil
	g.note(p.Seat, "rockfall", fmt.Sprintf("%s님이 (%d,%d)의 카드를 낙석으로 걷어냈습니다",
		p.Name, req.Col, req.Row))
	return nil
}

// sbResolveTool 카드에 적힌 장비를 쓴다. 카드가 장비를 지정하지 않은
// (구버전·확장) 경우에만 payload 의 tool 을 본다.
func sbResolveTool(card SBCard, req SBActionPayload) (SBTool, error) {
	if sbToolValid(card.Tool) {
		return card.Tool, nil
	}
	if sbToolValid(req.Tool) {
		return req.Tool, nil
	}
	return "", errors.New("도구를 지정해야 합니다")
}

// playBreak 장비 파괴 — 대상의 멀쩡한 장비 하나를 망가뜨린다
func (g *SBGame) playBreak(p *SBPlayer, card SBCard, req SBActionPayload) error {
	tool, err := sbResolveTool(card, req)
	if err != nil {
		return err
	}
	if req.TargetSeat < 0 || req.TargetSeat >= len(g.Players) {
		return errors.New("잘못된 좌석입니다")
	}
	target := g.Players[req.TargetSeat]
	if !target.Tools.get(tool) {
		return fmt.Errorf("%s님의 %s은(는) 이미 망가져 있습니다", target.Name, sbToolLabel(tool))
	}
	target.Tools.set(tool, false)
	g.note(p.Seat, "break", fmt.Sprintf("%s님이 %s님의 %s을(를) 망가뜨렸습니다",
		p.Name, target.Name, sbToolLabel(tool)))
	return nil
}

// playRepair 수리 — 자기 또는 남의 망가진 장비 하나를 고친다
func (g *SBGame) playRepair(p *SBPlayer, card SBCard, req SBActionPayload) error {
	tool, err := sbResolveTool(card, req)
	if err != nil {
		return err
	}
	if req.TargetSeat < 0 || req.TargetSeat >= len(g.Players) {
		return errors.New("잘못된 좌석입니다")
	}
	target := g.Players[req.TargetSeat]
	if target.Tools.get(tool) {
		return fmt.Errorf("%s님의 %s은(는) 멀쩡합니다", target.Name, sbToolLabel(tool))
	}
	target.Tools.set(tool, true)
	g.note(p.Seat, "repair", fmt.Sprintf("%s님이 %s님의 %s을(를) 고쳤습니다",
		p.Name, target.Name, sbToolLabel(tool)))
	return nil
}

// ==================== 버리기 ====================

// Discard 낼 게 없을 때 손패 1장을 버린다 (항상 가능)
func (g *SBGame) Discard(seat, index int) error {
	p, err := g.checkTurn(seat, index)
	if err != nil {
		return err
	}
	g.spend(p, index)
	g.note(seat, "discard", fmt.Sprintf("%s님이 카드 1장을 버렸습니다", p.Name))
	g.endTurn()
	return nil
}

// ForceDiscard 차례 마감 — 무작위 카드 1장을 자동으로 버린다 (허브 타이머)
func (g *SBGame) ForceDiscard(rng *rand.Rand) {
	if g.Phase != SBPhasePlaying {
		return
	}
	seat := g.CurrentSeat
	if seat < 0 || seat >= len(g.Players) {
		return
	}
	p := g.Players[seat]
	if len(p.Hand) == 0 { // 구조적으로 오지 않는다 (빈 손패는 차례를 건너뛴다)
		g.endTurn()
		return
	}
	g.Discard(seat, rng.Intn(len(p.Hand)))
}

// ==================== 종료 ====================

func (g *SBGame) finishWith(winner, reason, msg string) {
	g.Result = &SBResult{
		Winner: winner, GoldIndex: g.GoldIndex, Reason: reason, Message: msg,
	}
	g.Phase = SBPhaseGameOver
	g.StateSeq++
}
