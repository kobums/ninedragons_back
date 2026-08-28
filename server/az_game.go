package server

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ==================== 아줄 순수 규칙 ====================
//
// 타일 주머니·진열대 채우기·공장 수주·벽 타일 붙이기·바닥 감점·최종 보너스만
// 다룬다. 클라이언트·타이머를 모르며, 허브(az_hub.go)가 차례 마감 60초와
// 정산 지연 5초를 걸고 이벤트 큐(DrainEvents)를 방송한다.
//
// 한 라운드의 흐름은 규칙서의 3단계 그대로다.
//
//	공장 수주 → 벽 타일 붙이기 → 라운드 준비
//
//	1) 공장 수주(drafting): 차례마다 진열대 하나에서 같은 색 전부를 가져오고
//	   나머지는 중앙으로 민다. 또는 중앙에서 같은 색 전부를 가져온다(중앙에서
//	   처음 가져간 사람은 선 플레이어 마커를 함께 가져가 바닥 라인에 놓는다).
//	   가져온 타일은 패턴 라인 한 줄에 놓고, 넘친 만큼 바닥 라인으로 간다.
//	   진열대와 중앙이 모두 비면 이 단계가 끝난다.
//	2) 벽 타일 붙이기(tiling): 꽉 찬 패턴 라인마다 1장을 벽으로 옮겨 점수를
//	   내고 나머지는 버린다. 바닥 라인은 감점표대로 깎는다(0점 미만 없음).
//	3) 라운드 준비: 바닥을 비우고 선 마커 보유자가 다음 라운드 선이 되며
//	   진열대를 다시 채운다(주머니가 비면 버린 타일을 섞어 쓴다).
//
// 종료는 어떤 플레이어가 벽의 가로줄 하나를 완성한 라운드 끝이다. 최종
// 보너스는 완성 가로줄 2점·세로줄 7점·같은 색 5장 10점.
//
// 반드시 끝난다 — 벽으로 옮겨간 타일은 주머니로 돌아오지 않아 순환 타일이
// 라운드마다 줄고, 그래도 아무도 패턴 라인을 완성하지 않는 병리적 진행은
// AZMaxRounds 캡이 끊는다.

// NewAZGame 대기 상태의 새 게임
func NewAZGame(id string) *AZGame {
	return &AZGame{
		ID:            id,
		Players:       []*AZPlayer{},
		Phase:         AZPhaseWaiting,
		CurrentSeat:   -1,
		FirstNextSeat: -1,
		Factories:     [][]AZColor{},
		Center:        []AZColor{},
		Bag:           []AZColor{},
		Discard:       []AZColor{},
	}
}

// AddPlayer 대기실 입장. 좌석 번호를 돌려준다.
func (g *AZGame) AddPlayer(name string) (int, error) {
	if g.Phase != AZPhaseWaiting {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= AZMaxPlayers {
		return -1, fmt.Errorf("자리가 없습니다 (최대 %d명)", AZMaxPlayers)
	}
	seat := len(g.Players)
	g.Players = append(g.Players, &AZPlayer{Seat: seat, Name: name, Floor: []AZColor{}})
	return seat, nil
}

// RemovePlayer 대기실 퇴장. 좌석을 앞으로 당겨 빈틈을 없앤다.
func (g *AZGame) RemovePlayer(seat int) {
	if g.Phase != AZPhaseWaiting || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i, p := range g.Players {
		p.Seat = i
	}
}

// CanStart 시작 가능 여부
func (g *AZGame) CanStart() bool {
	return g.Phase == AZPhaseWaiting && len(g.Players) >= AZMinPlayers
}

// DrainEvents 쌓인 연출 이벤트를 꺼내 비운다 (허브가 방송한다)
func (g *AZGame) DrainEvents() []AZGameEvent {
	events := g.events
	g.events = nil
	return events
}

// pushEvent 연출 이벤트 적재 (seat -1 은 좌석 없음)
func (g *AZGame) pushEvent(kind string, seat int, message string) {
	g.events = append(g.events, AZGameEvent{Kind: kind, Seat: seat, Message: message})
}

// ==================== 타일 / 주머니 ====================

// azBuildBag 타일 100장 (5색 × 20장). 색 순서대로 나오므로 반드시 섞어 쓴다.
func azBuildBag() []AZColor {
	bag := make([]AZColor, 0, len(azColors)*AZTilesPerColor)
	for _, c := range azColors {
		for i := 0; i < AZTilesPerColor; i++ {
			bag = append(bag, c)
		}
	}
	return bag
}

// azCountColor 타일 묶음에서 특정 색의 장수
func azCountColor(tiles []AZColor, color AZColor) int {
	n := 0
	for _, t := range tiles {
		if t == color {
			n++
		}
	}
	return n
}

// azRemoveColor 특정 색을 뺀 나머지 (원본을 건드리지 않는다)
func azRemoveColor(tiles []AZColor, color AZColor) []AZColor {
	out := make([]AZColor, 0, len(tiles))
	for _, t := range tiles {
		if t != color {
			out = append(out, t)
		}
	}
	return out
}

// azDistinctColors 타일 묶음에 있는 색 목록 (azColors 나열 순서 고정)
func azDistinctColors(tiles []AZColor) []AZColor {
	out := []AZColor{}
	for _, c := range azColors {
		if azCountColor(tiles, c) > 0 {
			out = append(out, c)
		}
	}
	return out
}

// drawTile 주머니에서 한 장. 주머니가 비면 버린 타일을 섞어 되돌린다.
// 둘 다 비면 ok=false (진열대를 덜 채운 채 라운드가 열린다).
func (g *AZGame) drawTile(rng *rand.Rand) (AZColor, bool) {
	if len(g.Bag) == 0 {
		if len(g.Discard) == 0 {
			return AZColorNone, false
		}
		g.Bag = append([]AZColor{}, g.Discard...)
		g.Discard = []AZColor{}
		rng.Shuffle(len(g.Bag), func(i, j int) { g.Bag[i], g.Bag[j] = g.Bag[j], g.Bag[i] })
	}
	t := g.Bag[0]
	g.Bag = g.Bag[1:]
	return t, true
}

// fillFactories 라운드 준비 — 진열대를 인원수에 맞게 새로 만들고 4장씩 채운다.
// 채운 총 장수를 돌려준다 (0이면 타일이 완전히 소진된 것이다).
func (g *AZGame) fillFactories(rng *rand.Rand) int {
	n := azFactoryCount(len(g.Players))
	g.Factories = make([][]AZColor, 0, n)
	dealt := 0
	for i := 0; i < n; i++ {
		f := []AZColor{}
		for j := 0; j < AZFactoryTiles; j++ {
			t, ok := g.drawTile(rng)
			if !ok {
				break
			}
			f = append(f, t)
			dealt++
		}
		g.Factories = append(g.Factories, f)
	}
	return dealt
}

// ==================== 벽 / 점수 (순수) ====================

// azWallHasColor 행 row 의 color 칸이 이미 채워져 있는지
func azWallHasColor(wall [AZWallSize][AZWallSize]bool, row int, color AZColor) bool {
	col := azWallCol(row, color)
	if col < 0 {
		return false
	}
	return wall[row][col]
}

// azPlaceScore 벽에 타일을 붙인 직후의 점수. wall 은 (row,col)이 이미 채워진
// 상태여야 한다.
//
// 가로로 이어진 타일이 있으면 그 길이, 세로도 마찬가지이며 **둘 다 있으면
// 둘을 더한다**. 둘 다 없으면 1점.
func azPlaceScore(wall [AZWallSize][AZWallSize]bool, row, col int) int {
	if row < 0 || row >= AZWallSize || col < 0 || col >= AZWallSize || !wall[row][col] {
		return 0
	}
	h := 1
	for c := col - 1; c >= 0 && wall[row][c]; c-- {
		h++
	}
	for c := col + 1; c < AZWallSize && wall[row][c]; c++ {
		h++
	}
	v := 1
	for r := row - 1; r >= 0 && wall[r][col]; r-- {
		v++
	}
	for r := row + 1; r < AZWallSize && wall[r][col]; r++ {
		v++
	}
	switch {
	case h > 1 && v > 1:
		return h + v
	case h > 1:
		return h
	case v > 1:
		return v
	default:
		return 1
	}
}

// azCompletedRows 완성된 가로줄 수
func azCompletedRows(wall [AZWallSize][AZWallSize]bool) int {
	n := 0
	for r := 0; r < AZWallSize; r++ {
		full := true
		for c := 0; c < AZWallSize; c++ {
			if !wall[r][c] {
				full = false
				break
			}
		}
		if full {
			n++
		}
	}
	return n
}

// azCompletedCols 완성된 세로줄 수
func azCompletedCols(wall [AZWallSize][AZWallSize]bool) int {
	n := 0
	for c := 0; c < AZWallSize; c++ {
		full := true
		for r := 0; r < AZWallSize; r++ {
			if !wall[r][c] {
				full = false
				break
			}
		}
		if full {
			n++
		}
	}
	return n
}

// azCompletedColors 5장 전부를 붙인 색의 수
func azCompletedColors(wall [AZWallSize][AZWallSize]bool) int {
	n := 0
	for _, color := range azColors {
		full := true
		for r := 0; r < AZWallSize; r++ {
			if !azWallHasColor(wall, r, color) {
				full = false
				break
			}
		}
		if full {
			n++
		}
	}
	return n
}

// azFinalBonus 최종 보너스 — 완성 가로줄 2점, 세로줄 7점, 같은 색 5장 10점
func azFinalBonus(wall [AZWallSize][AZWallSize]bool) (rows, cols, colors, bonus int) {
	rows = azCompletedRows(wall)
	cols = azCompletedCols(wall)
	colors = azCompletedColors(wall)
	return rows, cols, colors, rows*2 + cols*7 + colors*10
}

// ==================== 패턴 라인 배치 규칙 (순수) ====================

// azCanPlace 패턴 라인 배치 가능 여부. line 이 AZLineTargetFloor(-1)면 전부
// 바닥 라인으로 가므로 언제나 허용된다 — 그래서 합법 수는 항상 하나 이상이다.
//
//   - 그 줄에 이미 다른 색이 있으면 못 놓는다.
//   - 그 줄에 대응하는 벽의 칸이 이미 채워져 있으면 못 놓는다.
//   - 이미 꽉 찬 줄에는 더 놓지 못한다.
func azCanPlace(p *AZPlayer, line int, color AZColor) error {
	if line == AZLineTargetFloor {
		return nil
	}
	if line < 0 || line >= AZWallSize {
		return errors.New("없는 패턴 라인입니다")
	}
	if !azIsTileColor(color) {
		return errors.New("없는 색입니다")
	}
	if azWallHasColor(p.Wall, line, color) {
		return fmt.Errorf("%d번 패턴 라인의 %s 칸은 벽에 이미 채워져 있습니다",
			line+1, azColorLabel(color))
	}
	cur := p.Lines[line]
	if cur.Color != AZColorNone && cur.Color != color {
		return fmt.Errorf("%d번 패턴 라인에는 이미 %s 타일이 있습니다",
			line+1, azColorLabel(cur.Color))
	}
	if cur.Count >= line+1 {
		return fmt.Errorf("%d번 패턴 라인은 이미 가득 찼습니다", line+1)
	}
	return nil
}

// ==================== 출처 표기 ====================

// azFactorySource 진열대 i 의 from 표기
func azFactorySource(i int) string {
	return fmt.Sprintf("%s%d", azSourceFactoryPrefix, i)
}

// azParseSource from 표기 해석 — "center" 또는 "factory:N"
func azParseSource(from string) (isCenter bool, index int, ok bool) {
	if from == azSourceCenter {
		return true, -1, true
	}
	if !strings.HasPrefix(from, azSourceFactoryPrefix) {
		return false, -1, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(from, azSourceFactoryPrefix))
	if err != nil || n < 0 {
		return false, -1, false
	}
	return false, n, true
}

// azSourceTiles 출처의 현재 타일 묶음 (없는 출처면 ok=false)
func (g *AZGame) azSourceTiles(from string) ([]AZColor, bool) {
	isCenter, idx, ok := azParseSource(from)
	if !ok {
		return nil, false
	}
	if isCenter {
		return g.Center, true
	}
	if idx >= len(g.Factories) {
		return nil, false
	}
	return g.Factories[idx], true
}

// ==================== 수 열거 / 결과 미리보기 (순수) ====================

// AZMove 수 하나 — (출처, 색, 패턴 라인). Line 이 -1 이면 전부 바닥 라인.
type AZMove struct {
	From  string
	Color AZColor
	Line  int
}

// azMoveOutcome 수를 뒀을 때의 결과 미리보기. AFK 자동 수와 봇 평가가
// 같은 계산을 쓰도록 한 곳에 모았다.
type azMoveOutcome struct {
	Took         int  // 가져오는 타일 수
	Placed       int  // 패턴 라인에 놓이는 수
	Overflow     int  // 바닥 라인으로 넘치는 수
	FloorAdd     int  // 실제로 바닥 칸을 차지하는 수 (선 마커 포함, 칸 상한 반영)
	PenaltyDelta int  // 이 수로 늘어나는 감점 (양수)
	TakesFirst   bool // 선 플레이어 마커를 함께 가져오는지
	Completes    bool // 이번 수로 패턴 라인이 꽉 차는지
	Row          int  // Completes 일 때 벽에 붙는 행
	Col          int  // Completes 일 때 벽에 붙는 열
}

// azEvalMove 수의 결과를 미리 계산한다 (상태를 바꾸지 않는다).
// 규칙상 불가능한 수면 ok=false.
func azEvalMove(g *AZGame, seat int, mv AZMove) (azMoveOutcome, bool) {
	if seat < 0 || seat >= len(g.Players) {
		return azMoveOutcome{}, false
	}
	p := g.Players[seat]
	tiles, ok := g.azSourceTiles(mv.From)
	if !ok {
		return azMoveOutcome{}, false
	}
	took := azCountColor(tiles, mv.Color)
	if took == 0 {
		return azMoveOutcome{}, false
	}
	if err := azCanPlace(p, mv.Line, mv.Color); err != nil {
		return azMoveOutcome{}, false
	}

	out := azMoveOutcome{Took: took, Row: -1, Col: -1}
	out.TakesFirst = mv.From == azSourceCenter && g.CenterHasFirst

	out.Overflow = took
	if mv.Line != AZLineTargetFloor {
		space := mv.Line + 1 - p.Lines[mv.Line].Count
		out.Placed = took
		if out.Placed > space {
			out.Placed = space
		}
		out.Overflow = took - out.Placed
		if p.Lines[mv.Line].Count+out.Placed == mv.Line+1 {
			out.Completes = true
			out.Row = mv.Line
			out.Col = azWallCol(mv.Line, mv.Color)
		}
	}

	add := out.Overflow
	if out.TakesFirst {
		add++
	}
	free := AZFloorSlots - len(p.Floor)
	if free < 0 {
		free = 0
	}
	if add > free {
		add = free
	}
	out.FloorAdd = add
	out.PenaltyDelta = azFloorPenalty(len(p.Floor)+add) - azFloorPenalty(len(p.Floor))
	return out, true
}

// azLegalMoves 좌석의 합법 수 전부 (진열대 → 중앙, 색은 나열 순서, 줄은
// 0~4 뒤에 -1). 타일이 남아 있는 한 -1(전부 바닥) 덕에 반드시 하나 이상이다.
func azLegalMoves(g *AZGame, seat int) []AZMove {
	moves := []AZMove{}
	if seat < 0 || seat >= len(g.Players) {
		return moves
	}
	p := g.Players[seat]

	sources := make([]string, 0, len(g.Factories)+1)
	for i := range g.Factories {
		sources = append(sources, azFactorySource(i))
	}
	sources = append(sources, azSourceCenter)

	for _, from := range sources {
		tiles, ok := g.azSourceTiles(from)
		if !ok {
			continue
		}
		for _, color := range azDistinctColors(tiles) {
			for line := 0; line < AZWallSize; line++ {
				if azCanPlace(p, line, color) == nil {
					moves = append(moves, AZMove{From: from, Color: color, Line: line})
				}
			}
			moves = append(moves, AZMove{From: from, Color: color, Line: AZLineTargetFloor})
		}
	}
	return moves
}

// azSafestMove 감점이 가장 적은 수 (AFK 자동 진행의 근거).
// 감점이 같으면 패턴 라인에 더 많이 놓는 수, 그다음 짧은 줄을 고른다.
func azSafestMove(g *AZGame, seat int) (AZMove, bool) {
	best, bestOut, found := AZMove{}, azMoveOutcome{}, false
	for _, mv := range azLegalMoves(g, seat) {
		out, ok := azEvalMove(g, seat, mv)
		if !ok {
			continue
		}
		if !found {
			best, bestOut, found = mv, out, true
			continue
		}
		if out.PenaltyDelta != bestOut.PenaltyDelta {
			if out.PenaltyDelta < bestOut.PenaltyDelta {
				best, bestOut = mv, out
			}
			continue
		}
		if out.Placed != bestOut.Placed {
			if out.Placed > bestOut.Placed {
				best, bestOut = mv, out
			}
			continue
		}
		if mv.Line >= 0 && (best.Line < 0 || mv.Line < best.Line) {
			best, bestOut = mv, out
		}
	}
	return best, found
}

// ==================== 진행 ====================

// Start 게임 시작 — 100장을 섞어 진열대를 채우고 선 마커를 중앙에 놓는다
func (g *AZGame) Start(rng *rand.Rand) error {
	if !g.CanStart() {
		return fmt.Errorf("%d명 이상 모여야 시작할 수 있습니다", AZMinPlayers)
	}

	bag := azBuildBag()
	rng.Shuffle(len(bag), func(i, j int) { bag[i], bag[j] = bag[j], bag[i] })
	g.Bag = bag
	g.Discard = []AZColor{}

	for _, p := range g.Players {
		p.Score = 0
		p.Lines = [AZWallSize]AZLine{}
		p.Wall = [AZWallSize][AZWallSize]bool{}
		p.Floor = []AZColor{}
	}

	g.Round = 1
	g.CurrentSeat = 0
	g.FirstNextSeat = -1
	g.Center = []AZColor{}
	g.CenterHasFirst = true
	g.LastAction = nil
	g.RoundResult = nil
	g.Result = nil
	g.Bonuses = nil
	g.EndReason = ""
	g.pendingEnd = false
	g.fillFactories(rng)

	g.Phase = AZPhaseDrafting
	g.Ready = true
	g.StartedAt = time.Now()
	g.StateSeq++

	g.pushEvent("round_start", -1, fmt.Sprintf(
		"1라운드 시작 — 진열대 %d개에 타일 4장씩을 올렸습니다", len(g.Factories)))
	return nil
}

// addFloorTile 바닥 라인에 타일 한 장. 칸(7)을 넘긴 타일은 놓지 않고 버린다.
func (g *AZGame) addFloorTile(p *AZPlayer, color AZColor) {
	if len(p.Floor) < AZFloorSlots {
		p.Floor = append(p.Floor, color)
		return
	}
	g.Discard = append(g.Discard, color)
}

// addFirstMarker 선 플레이어 마커를 바닥 라인에 놓는다. 마커는 타일이 아니라
// 표식이라 버려지면 안 되므로, 칸이 없으면 실제 타일 한 장을 대신 버린다.
func (g *AZGame) addFirstMarker(p *AZPlayer) {
	if len(p.Floor) < AZFloorSlots {
		p.Floor = append(p.Floor, AZColorFirst)
		return
	}
	for i := len(p.Floor) - 1; i >= 0; i-- {
		if p.Floor[i] != AZColorFirst {
			g.Discard = append(g.Discard, p.Floor[i])
			p.Floor[i] = AZColorFirst
			return
		}
	}
}

// draftDone 진열대와 중앙이 모두 비었는지 (선 마커만 남은 중앙은 빈 것으로
// 본다 — 마커만으로는 가져갈 수 없다)
func (g *AZGame) draftDone() bool {
	for _, f := range g.Factories {
		if len(f) > 0 {
			return false
		}
	}
	return len(g.Center) == 0
}

// Take 공장 수주 — 진열대 하나 또는 중앙에서 같은 색 전부를 가져와
// 패턴 라인 한 줄(또는 전부 바닥 라인)에 놓는다.
//
// 돌려주는 error 는 규약 위반(차례 아님·없는 색·배치 불가)이라 상태를
// 건드리지 않고 az_error 로만 응답한다.
func (g *AZGame) Take(seat int, from string, color AZColor, line int) error {
	if g.Phase != AZPhaseDrafting {
		return errors.New("공장 수주 단계가 아닙니다")
	}
	if seat < 0 || seat >= len(g.Players) {
		return errors.New("좌석을 찾을 수 없습니다")
	}
	if seat != g.CurrentSeat {
		return errors.New("아직 차례가 아닙니다")
	}
	if !azIsTileColor(color) {
		return errors.New("없는 색입니다")
	}
	p := g.Players[seat]
	if err := azCanPlace(p, line, color); err != nil {
		return err
	}

	isCenter, idx, ok := azParseSource(from)
	if !ok {
		return errors.New("없는 출처입니다")
	}

	took, gotFirst, label := 0, false, ""
	if isCenter {
		took = azCountColor(g.Center, color)
		if took == 0 {
			return fmt.Errorf("중앙에 %s 타일이 없습니다", azColorLabel(color))
		}
		g.Center = azRemoveColor(g.Center, color)
		if g.CenterHasFirst {
			g.CenterHasFirst = false
			g.FirstNextSeat = seat
			gotFirst = true
		}
		label = "중앙"
	} else {
		if idx >= len(g.Factories) {
			return errors.New("없는 진열대입니다")
		}
		took = azCountColor(g.Factories[idx], color)
		if took == 0 {
			return fmt.Errorf("%d번 진열대에 %s 타일이 없습니다", idx+1, azColorLabel(color))
		}
		rest := azRemoveColor(g.Factories[idx], color)
		g.Factories[idx] = []AZColor{}
		g.Center = append(g.Center, rest...)
		label = fmt.Sprintf("%d번 진열대", idx+1)
	}

	placed, overflow := 0, took
	if line != AZLineTargetFloor {
		space := line + 1 - p.Lines[line].Count
		placed = took
		if placed > space {
			placed = space
		}
		overflow = took - placed
		p.Lines[line].Color = color
		p.Lines[line].Count += placed
	}

	// 선 마커를 먼저 놓아야 넘친 타일에 칸을 뺏기지 않는다
	if gotFirst {
		g.addFirstMarker(p)
	}
	for i := 0; i < overflow; i++ {
		g.addFloorTile(p, color)
	}

	where := "바닥 라인"
	if line != AZLineTargetFloor {
		where = fmt.Sprintf("%d번 패턴 라인", line+1)
	}
	msg := fmt.Sprintf("%s에서 %s %d장을 가져와 %s에 놓았습니다",
		label, azColorLabel(color), took, where)
	if line != AZLineTargetFloor && overflow > 0 {
		msg += fmt.Sprintf(" (%d장은 바닥 라인)", overflow)
	}
	if gotFirst {
		msg += " · 선 플레이어 마커를 가져갔습니다"
	}
	g.LastAction = &AZAction{Seat: seat, Name: p.Name, Message: msg}
	g.pushEvent("take", seat, msg)

	if g.draftDone() {
		g.tileWall()
		return nil
	}
	g.CurrentSeat = (g.CurrentSeat + 1) % len(g.Players)
	g.StateSeq++
	return nil
}

// ForceMove 차례 60초 무응답 해소 — 감점이 가장 적은 수를 자동으로 둔다.
// 둘 수가 있으면 true.
func (g *AZGame) ForceMove() bool {
	if g.Phase != AZPhaseDrafting {
		return false
	}
	mv, ok := azSafestMove(g, g.CurrentSeat)
	if !ok {
		return false
	}
	return g.Take(g.CurrentSeat, mv.From, mv.Color, mv.Line) == nil
}

// tileWall 벽 타일 붙이기 — 꽉 찬 패턴 라인마다 오른쪽 끝 1장을 벽으로 옮겨
// 점수를 내고 나머지는 버린다. 이어서 바닥 라인 감점을 매기고(0점 미만 없음)
// 바닥을 비운다. 정산 결과는 tiling 단계 스냅샷으로 나간다.
func (g *AZGame) tileWall() {
	rows := []AZRoundRow{}
	for _, p := range g.Players {
		gained := 0
		for row := 0; row < AZWallSize; row++ {
			line := p.Lines[row]
			if line.Color == AZColorNone || line.Count < row+1 {
				continue
			}
			col := azWallCol(row, line.Color)
			if col < 0 {
				continue
			}
			p.Wall[row][col] = true
			pts := azPlaceScore(p.Wall, row, col)
			gained += pts
			// 옮긴 1장을 뺀 나머지는 버린다
			for i := 0; i < line.Count-1; i++ {
				g.Discard = append(g.Discard, line.Color)
			}
			p.Lines[row] = AZLine{}
			g.pushEvent("wall_tile", p.Seat, fmt.Sprintf(
				"%s님이 %d번 줄의 %s 타일을 벽에 붙여 %d점을 얻었습니다",
				p.Name, row+1, azColorLabel(line.Color), pts))
		}

		penalty := azFloorPenalty(len(p.Floor))
		for _, t := range p.Floor {
			if t != AZColorFirst { // 선 마커는 타일이 아니라 버린 타일에 섞지 않는다
				g.Discard = append(g.Discard, t)
			}
		}
		p.Floor = []AZColor{}

		p.Score += gained - penalty
		if p.Score < 0 {
			p.Score = 0
		}
		rows = append(rows, AZRoundRow{Seat: p.Seat, Gained: gained,
			Penalty: penalty, Total: p.Score})
	}

	// 종료 판정 — 가로줄을 하나라도 완성한 사람이 있으면 이 라운드로 끝난다
	for _, p := range g.Players {
		if azCompletedRows(p.Wall) > 0 {
			g.pendingEnd = true
			g.EndReason = "row_complete"
			break
		}
	}

	summary := []string{}
	for _, r := range rows {
		summary = append(summary, fmt.Sprintf("%s +%d-%d=%d",
			g.Players[r.Seat].Name, r.Gained, r.Penalty, r.Total))
	}
	message := fmt.Sprintf("%d라운드 정산 — %s", g.Round, strings.Join(summary, " · "))
	g.RoundResult = &AZRoundResult{Rows: rows, Message: message}

	g.Phase = AZPhaseTiling
	g.CurrentSeat = -1
	g.StateSeq++
	g.pushEvent("round_end", -1, message)
}

// AdvanceRound 라운드 준비 — 정산을 보여준 뒤 다음 라운드를 열거나 게임을
// 끝낸다. 허브의 정산 지연 타이머가 부른다.
func (g *AZGame) AdvanceRound(rng *rand.Rand) {
	if g.Phase != AZPhaseTiling {
		return
	}
	if g.pendingEnd {
		g.finish(g.EndReason)
		return
	}
	if g.Round >= AZMaxRounds { // 병리적 진행 방지 캡 (실전 미발동)
		g.finish("round_cap")
		return
	}

	g.Round++
	if g.FirstNextSeat >= 0 && g.FirstNextSeat < len(g.Players) {
		g.CurrentSeat = g.FirstNextSeat
	} else if g.CurrentSeat < 0 {
		g.CurrentSeat = 0
	}
	g.FirstNextSeat = -1
	g.Center = []AZColor{}
	g.CenterHasFirst = true
	g.RoundResult = nil
	g.LastAction = nil

	if dealt := g.fillFactories(rng); dealt == 0 {
		g.finish("tiles_exhausted")
		return
	}

	g.Phase = AZPhaseDrafting
	g.StateSeq++
	g.pushEvent("round_start", g.CurrentSeat, fmt.Sprintf(
		"%d라운드 시작 — %s님이 선입니다", g.Round, g.Players[g.CurrentSeat].Name))
}

// azWinners 최고점 좌석 (동점이면 완성 가로줄이 많은 쪽, 그래도 같으면 공동)
func azWinners(players []*AZPlayer) ([]int, []string) {
	seats, names := []int{}, []string{}
	bestScore, bestRows := -1, -1
	for _, p := range players {
		rows := azCompletedRows(p.Wall)
		if p.Score > bestScore || (p.Score == bestScore && rows > bestRows) {
			bestScore, bestRows = p.Score, rows
		}
	}
	for _, p := range players {
		if p.Score == bestScore && azCompletedRows(p.Wall) == bestRows {
			seats = append(seats, p.Seat)
			names = append(names, p.Name)
		}
	}
	return seats, names
}

// finish 최종 보너스를 얹고 종료 판정을 세운다
func (g *AZGame) finish(reason string) {
	g.Bonuses = []AZBonusRow{}
	for _, p := range g.Players {
		rows, cols, colors, bonus := azFinalBonus(p.Wall)
		p.Score += bonus
		if p.Score < 0 {
			p.Score = 0
		}
		g.Bonuses = append(g.Bonuses, AZBonusRow{Seat: p.Seat, Name: p.Name,
			Rows: rows, Cols: cols, Colors: colors, Bonus: bonus, Score: p.Score})
		if bonus > 0 {
			g.pushEvent("final_bonus", p.Seat, fmt.Sprintf(
				"%s님 최종 보너스 +%d점 (가로줄 %d·세로줄 %d·같은 색 %d)",
				p.Name, bonus, rows, cols, colors))
		}
	}

	seats, names := azWinners(g.Players)
	tail := "가로줄이 완성돼 게임이 끝났습니다"
	switch reason {
	case "tiles_exhausted":
		tail = "타일이 모두 소진돼 게임이 끝났습니다"
	case "round_cap":
		tail = "라운드 상한에 닿아 게임이 끝났습니다"
	}

	message := fmt.Sprintf("%s — 승자 없음", tail)
	if len(names) > 0 {
		best := g.Players[seats[0]].Score
		if len(names) == 1 {
			message = fmt.Sprintf("%s — %s님 승리! (%d점)", tail, names[0], best)
		} else {
			message = fmt.Sprintf("%s — %d명 공동 승리! (%d점)", tail, len(names), best)
		}
	}

	g.Phase = AZPhaseGameOver
	g.CurrentSeat = -1
	g.EndReason = reason
	g.Result = &AZResult{WinnerSeats: seats, WinnerNames: names, Message: message}
	g.StateSeq++
	g.pushEvent("game_over", -1, message)
}

// azScoreboard 좌석별 점수 (로그용, 좌석 오름차순)
func azScoreboard(players []*AZPlayer) []int {
	scores := make([]int, 0, len(players))
	seats := make([]int, 0, len(players))
	for _, p := range players {
		seats = append(seats, p.Seat)
	}
	sort.Ints(seats)
	for range seats {
		scores = append(scores, 0)
	}
	for _, p := range players {
		if p.Seat >= 0 && p.Seat < len(scores) {
			scores[p.Seat] = p.Score
		}
	}
	return scores
}
