package server

import (
	"errors"
	"math/rand"
	"time"
)

// NewDVGame 로비 상태의 새 게임
func NewDVGame(id string) *DVGame {
	return &DVGame{
		ID:                id,
		Players:           []*DVPlayer{},
		Phase:             DVPhaseLobby,
		CurrentSeat:       -1,
		PendingJokerTiles: map[int][]DVTile{},
		WinnerSeat:        -1,
	}
}

// newDVTileSet 전체 26장: 검정 0~11(ID 0~11), 흰색 0~11(ID 12~23),
// 조커 검정(ID 24)·흰색(ID 25)
func newDVTileSet() []DVTile {
	tiles := make([]DVTile, 0, 26)
	for v := 0; v <= 11; v++ {
		tiles = append(tiles, DVTile{ID: v, Color: DVBlack, Value: v})
	}
	for v := 0; v <= 11; v++ {
		tiles = append(tiles, DVTile{ID: 12 + v, Color: DVWhite, Value: v})
	}
	tiles = append(tiles, DVTile{ID: 24, Color: DVBlack, Value: DVJokerValue, Joker: true})
	tiles = append(tiles, DVTile{ID: 25, Color: DVWhite, Value: DVJokerValue, Joker: true})
	return tiles
}

// AddPlayer 로비 입장. 좌석 번호를 돌려준다.
func (g *DVGame) AddPlayer(name string) (int, error) {
	if g.Phase != DVPhaseLobby {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= DVMaxPlayers {
		return -1, errors.New("자리가 없습니다 (최대 4명)")
	}
	seat := len(g.Players)
	g.Players = append(g.Players, &DVPlayer{Seat: seat, Name: name})
	return seat, nil
}

// RemovePlayer 로비에서 퇴장. 좌석을 앞으로 당겨 빈틈을 없앤다.
// 게임 시작 후에는 쓰지 않는다 (그때는 ForfeitPlayer).
func (g *DVGame) RemovePlayer(seat int) {
	if g.Phase != DVPhaseLobby || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i, p := range g.Players {
		p.Seat = i
	}
}

// CanStart 시작 가능 여부
func (g *DVGame) CanStart() bool {
	return g.Phase == DVPhaseLobby && len(g.Players) >= 2
}

// Start 셔플 후 시작 타일 가져오기(initial_draw) 단계로 진입한다.
// 원작 셋업에서도 각자 원하는 타일(=원하는 색)을 가져가므로 서버가
// 나눠주지 않고 플레이어가 색을 골라 가져간다.
func (g *DVGame) Start(rng *rand.Rand) error {
	if !g.CanStart() {
		return errors.New("시작할 수 없습니다 (2명 이상 필요)")
	}

	deck := newDVTileSet()
	rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })

	// 2~3인은 4장, 4인은 3장
	handSize := 4
	if len(g.Players) == 4 {
		handSize = 3
	}
	g.InitialRemaining = map[int]int{}
	for _, p := range g.Players {
		g.InitialRemaining[p.Seat] = handSize
	}

	g.Deck = deck
	g.CurrentSeat = rng.Intn(len(g.Players))
	g.Ready = true
	g.StartedAt = time.Now()
	g.Phase = DVPhaseInitialDraw
	return nil
}

// takeFromDeck 더미에서 원하는 색의 타일을 한 장 꺼낸다. 더미는 셔플돼
// 있으므로 해당 색의 마지막 타일을 집으면 균등 랜덤과 같다.
func (g *DVGame) takeFromDeck(color DVTileColor) (*DVTile, error) {
	if color != DVBlack && color != DVWhite {
		return nil, errors.New("검은색 또는 흰색을 선택하세요")
	}
	for i := len(g.Deck) - 1; i >= 0; i-- {
		if g.Deck[i].Color != color {
			continue
		}
		tile := g.Deck[i]
		g.Deck = append(g.Deck[:i], g.Deck[i+1:]...)
		return &tile, nil
	}
	return nil, errors.New("남은 해당 색 타일이 없습니다")
}

// TakeInitial 시작 타일 한 장을 색 골라 가져온다. 턴과 무관하게 전원이
// 동시에 진행하며, 전원이 다 가져오면 조커 배치 또는 본게임으로 넘어간다.
func (g *DVGame) TakeInitial(seat int, color DVTileColor) (*DVTile, error) {
	if g.Phase != DVPhaseInitialDraw {
		return nil, errors.New("지금은 시작 타일을 가져올 수 없습니다")
	}
	if g.InitialRemaining[seat] <= 0 {
		return nil, errors.New("이미 시작 타일을 모두 가져왔습니다")
	}
	tile, err := g.takeFromDeck(color)
	if err != nil {
		return nil, err
	}
	// 조커는 위치를 소유자가 골라야 하므로 줄 밖에 보관
	if tile.Joker {
		g.PendingJokerTiles[seat] = append(g.PendingJokerTiles[seat], *tile)
	} else {
		g.insertTile(seat, *tile)
	}
	g.InitialRemaining[seat]--
	if g.InitialRemaining[seat] == 0 {
		delete(g.InitialRemaining, seat)
	}
	g.finishInitialIfDone()
	return tile, nil
}

// finishInitialIfDone 전원이 시작 타일을 다 가져왔으면 다음 단계로
func (g *DVGame) finishInitialIfDone() {
	if g.Phase != DVPhaseInitialDraw || len(g.InitialRemaining) != 0 {
		return
	}
	if len(g.PendingJokerTiles) > 0 {
		g.Phase = DVPhaseJokerSetup
	} else {
		g.Phase = DVPhaseDraw
	}
}

// dvTileLess 배치 순서 비교: 값 오름차순, 동값이면 검정 < 흰색
func dvTileLess(a, b DVTile) bool {
	if a.Value != b.Value {
		return a.Value < b.Value
	}
	return a.Color == DVBlack && b.Color == DVWhite
}

// insertTile 결정론적 자동 삽입: 왼쪽부터 훑어 새 타일보다 큰 첫 번째
// 비조커 타일 앞에 넣는다. 조커는 정렬 비교에서 투명 취급.
func (g *DVGame) insertTile(seat int, tile DVTile) {
	p := g.Players[seat]
	pos := len(p.Tiles)
	for i, t := range p.Tiles {
		if t.Joker {
			continue
		}
		if dvTileLess(tile, t) {
			pos = i
			break
		}
	}
	p.Tiles = append(p.Tiles, DVTile{})
	copy(p.Tiles[pos+1:], p.Tiles[pos:])
	p.Tiles[pos] = tile
}

// insertTileAt 위치를 지정해 삽입 (조커 전용)
func (g *DVGame) insertTileAt(seat, pos int, tile DVTile) error {
	p := g.Players[seat]
	if pos < 0 || pos > len(p.Tiles) {
		return errors.New("잘못된 위치입니다")
	}
	p.Tiles = append(p.Tiles, DVTile{})
	copy(p.Tiles[pos+1:], p.Tiles[pos:])
	p.Tiles[pos] = tile
	return nil
}

// PlaceJoker 조커 배치. joker_setup(초기 배분)과 place_drawn_joker(뽑은 조커)
// 두 단계에서 쓰인다.
func (g *DVGame) PlaceJoker(seat, tileID, position int) error {
	switch g.Phase {
	case DVPhaseJokerSetup:
		pending := g.PendingJokerTiles[seat]
		idx := -1
		for i, t := range pending {
			if t.ID == tileID {
				idx = i
				break
			}
		}
		if idx == -1 {
			return errors.New("배치할 조커가 없습니다")
		}
		tile := pending[idx]
		if err := g.insertTileAt(seat, position, tile); err != nil {
			return err
		}
		pending = append(pending[:idx], pending[idx+1:]...)
		if len(pending) == 0 {
			delete(g.PendingJokerTiles, seat)
		} else {
			g.PendingJokerTiles[seat] = pending
		}
		if len(g.PendingJokerTiles) == 0 {
			g.Phase = DVPhaseDraw
		}
		return nil

	case DVPhasePlaceDrawnJoker:
		if seat != g.CurrentSeat {
			return errors.New("당신의 차례가 아닙니다")
		}
		if g.DrawnTile == nil || !g.DrawnTile.Joker || g.DrawnTile.ID != tileID {
			return errors.New("배치할 조커가 없습니다")
		}
		tile := *g.DrawnTile
		tile.Revealed = g.DrawnJokerRevealed
		if err := g.insertTileAt(seat, position, tile); err != nil {
			return err
		}
		g.DrawnTile = nil
		g.DrawnJokerRevealed = false
		// 공개 삽입(추리 실패)이었다면 이 조커로 전 타일 공개가 될 수 있다
		if g.checkElimination(seat) && g.checkVictory() {
			return nil
		}
		g.endTurn()
		return nil

	default:
		return errors.New("지금은 조커를 배치할 수 없습니다")
	}
}

// DeckCountByColor 더미에 남은 색별 타일 수
func (g *DVGame) DeckCountByColor(color DVTileColor) int {
	count := 0
	for _, t := range g.Deck {
		if t.Color == color {
			count++
		}
	}
	return count
}

// DrawTile 더미에서 원하는 색의 타일을 한 장 뽑는다. 원작에서 더미는
// 뒷면(색만 보임)으로 펼쳐져 있어 원하는 타일을 집을 수 있는데, 같은 색
// 뒷면끼리는 구분이 안 되므로 색 선택과 동치다.
// 값은 허브가 본인에게만 보여준다.
func (g *DVGame) DrawTile(seat int, color DVTileColor) (*DVTile, error) {
	if g.Phase != DVPhaseDraw {
		return nil, errors.New("지금은 타일을 뽑을 수 없습니다")
	}
	if seat != g.CurrentSeat {
		return nil, errors.New("당신의 차례가 아닙니다")
	}
	tile, err := g.takeFromDeck(color)
	if err != nil {
		return nil, err
	}
	g.DrawnTile = tile
	g.Phase = DVPhaseGuess
	return tile, nil
}

// DVGuessResult 추리 한 번의 결과
type DVGuessResult struct {
	Correct    bool
	TargetSeat int
	TileIndex  int
	Tile       DVTile // 공개됐다면 그 타일 (Correct=true 일 때만 의미 있음)
	Eliminated bool   // 이 추리로 target 이 탈락했는지
	GameOver   bool
}

// Guess 상대 비공개 타일 추리. value 는 0~11 또는 조커(-1).
func (g *DVGame) Guess(seat, targetSeat, tileIndex, value int) (*DVGuessResult, error) {
	if g.Phase != DVPhaseGuess {
		return nil, errors.New("지금은 추리할 수 없습니다")
	}
	if seat != g.CurrentSeat {
		return nil, errors.New("당신의 차례가 아닙니다")
	}
	if targetSeat == seat {
		return nil, errors.New("자신의 타일은 추리할 수 없습니다")
	}
	if targetSeat < 0 || targetSeat >= len(g.Players) {
		return nil, errors.New("없는 플레이어입니다")
	}
	target := g.Players[targetSeat]
	if target.Eliminated {
		return nil, errors.New("이미 탈락한 플레이어입니다")
	}
	if tileIndex < 0 || tileIndex >= len(target.Tiles) {
		return nil, errors.New("없는 타일입니다")
	}
	tile := target.Tiles[tileIndex]
	if tile.Revealed {
		return nil, errors.New("이미 공개된 타일입니다")
	}
	if value != DVJokerValue && (value < 0 || value > 11) {
		return nil, errors.New("추리 값은 0~11 또는 조커여야 합니다")
	}

	correct := value == tile.Value // 조커는 Value 가 -1 이므로 같은 비교로 처리된다
	result := &DVGuessResult{Correct: correct, TargetSeat: targetSeat, TileIndex: tileIndex}

	if correct {
		target.Tiles[tileIndex].Revealed = true
		result.Tile = target.Tiles[tileIndex]
		result.Eliminated = g.checkElimination(targetSeat)
		if result.Eliminated && g.checkVictory() {
			result.GameOver = true
			return result, nil
		}
		g.Phase = DVPhaseContinueChoice
		return result, nil
	}

	// 실패: 뽑은 타일을 공개 상태로 자기 줄에 넣는다
	if g.DrawnTile != nil {
		if g.DrawnTile.Joker {
			// 공개 삽입이지만 위치는 소유자가 고른다
			g.DrawnJokerRevealed = true
			g.Phase = DVPhasePlaceDrawnJoker
			return result, nil
		}
		drawn := *g.DrawnTile
		drawn.Revealed = true
		g.insertTile(seat, drawn)
		g.DrawnTile = nil
		if g.checkElimination(seat) && g.checkVictory() {
			result.GameOver = true
			return result, nil
		}
		g.endTurn()
		return result, nil
	}

	// 더미 소진 턴: 공개할 자기 타일을 골라야 한다
	g.Phase = DVPhaseRevealOwn
	return result, nil
}

// ContinueChoice 추리 성공 후 계속(true)/중단(false)
func (g *DVGame) ContinueChoice(seat int, cont bool) error {
	if g.Phase != DVPhaseContinueChoice {
		return errors.New("지금은 선택할 수 없습니다")
	}
	if seat != g.CurrentSeat {
		return errors.New("당신의 차례가 아닙니다")
	}

	if cont {
		g.Phase = DVPhaseGuess
		return nil
	}

	// 중단: 뽑은 타일을 비공개로 넣고 턴 종료
	if g.DrawnTile != nil {
		if g.DrawnTile.Joker {
			g.DrawnJokerRevealed = false
			g.Phase = DVPhasePlaceDrawnJoker
			return nil
		}
		g.insertTile(seat, *g.DrawnTile)
		g.DrawnTile = nil
	}
	g.endTurn()
	return nil
}

// RevealOwn 더미 소진 상태에서 추리 실패 시 공개할 자기 타일 선택
func (g *DVGame) RevealOwn(seat, tileIndex int) error {
	if g.Phase != DVPhaseRevealOwn {
		return errors.New("지금은 타일을 공개할 수 없습니다")
	}
	if seat != g.CurrentSeat {
		return errors.New("당신의 차례가 아닙니다")
	}
	p := g.Players[seat]
	if tileIndex < 0 || tileIndex >= len(p.Tiles) {
		return errors.New("없는 타일입니다")
	}
	if p.Tiles[tileIndex].Revealed {
		return errors.New("이미 공개된 타일입니다")
	}
	p.Tiles[tileIndex].Revealed = true
	if g.checkElimination(seat) && g.checkVictory() {
		return nil
	}
	g.endTurn()
	return nil
}

// ForfeitPlayer 몰수(유예 만료 등): 전 타일 공개 + 탈락. 게임은 이어진다.
func (g *DVGame) ForfeitPlayer(seat int) {
	if g.Phase == DVPhaseLobby || g.Phase == DVPhaseGameOver {
		return
	}
	if seat < 0 || seat >= len(g.Players) {
		return
	}
	p := g.Players[seat]
	if p.Eliminated {
		return
	}

	// 시작 타일을 다 못 가져간 상태의 몰수: 남은 몫은 포기 처리
	delete(g.InitialRemaining, seat)
	g.finishInitialIfDone()

	// 배치 대기 중이던 초기 조커는 줄 끝에 공개로 붙인다
	for _, t := range g.PendingJokerTiles[seat] {
		t.Revealed = true
		p.Tiles = append(p.Tiles, t)
	}
	delete(g.PendingJokerTiles, seat)
	if g.Phase == DVPhaseJokerSetup && len(g.PendingJokerTiles) == 0 {
		g.Phase = DVPhaseDraw
	}

	for i := range p.Tiles {
		p.Tiles[i].Revealed = true
	}
	p.Eliminated = true

	// 몰수당한 사람이 현재 턴이면 들고 있던 타일을 공개로 처분하고 턴을 넘긴다
	if seat == g.CurrentSeat {
		if g.DrawnTile != nil {
			drawn := *g.DrawnTile
			drawn.Revealed = true
			p.Tiles = append(p.Tiles, drawn)
			g.DrawnTile = nil
			g.DrawnJokerRevealed = false
		}
		if !g.checkVictory() {
			g.endTurn()
		}
		return
	}
	g.checkVictory()
}

// checkElimination 전 타일이 공개됐으면 탈락 처리
func (g *DVGame) checkElimination(seat int) bool {
	p := g.Players[seat]
	if p.Eliminated {
		return true
	}
	for _, t := range p.Tiles {
		if !t.Revealed {
			return false
		}
	}
	p.Eliminated = true
	return true
}

// aliveSeats 아직 비공개 타일을 가진(미탈락) 좌석들
func (g *DVGame) aliveSeats() []int {
	seats := []int{}
	for _, p := range g.Players {
		if !p.Eliminated {
			seats = append(seats, p.Seat)
		}
	}
	return seats
}

// checkVictory 생존자가 1명이면 게임 종료 처리
func (g *DVGame) checkVictory() bool {
	alive := g.aliveSeats()
	if len(alive) != 1 {
		return false
	}
	g.WinnerSeat = alive[0]
	g.Phase = DVPhaseGameOver
	return true
}

// endTurn 탈락자를 건너뛰고 다음 좌석으로. 더미가 비었으면 뽑기 없이 바로 추리.
func (g *DVGame) endTurn() {
	n := len(g.Players)
	for i := 1; i <= n; i++ {
		next := (g.CurrentSeat + i) % n
		if !g.Players[next].Eliminated {
			g.CurrentSeat = next
			break
		}
	}
	if len(g.Deck) > 0 {
		g.Phase = DVPhaseDraw
	} else {
		g.Phase = DVPhaseGuess
	}
}
