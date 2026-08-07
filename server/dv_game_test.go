package server

import (
	"math/rand"
	"testing"
)

// dvTile 테스트용 타일 생성 축약
func dvTile(id int, color DVTileColor, value int) DVTile {
	return DVTile{ID: id, Color: color, Value: value}
}

func dvJoker(id int, color DVTileColor) DVTile {
	return DVTile{ID: id, Color: color, Value: DVJokerValue, Joker: true}
}

// newTestDVGame Start 의 셔플을 우회해 손패·더미를 고정한 게임을 만든다.
func newTestDVGame(t *testing.T, hands [][]DVTile, deck []DVTile, currentSeat int) *DVGame {
	t.Helper()
	g := NewDVGame("test")
	for i, hand := range hands {
		g.Players = append(g.Players, &DVPlayer{Seat: i, Name: "p", Tiles: hand})
		_ = hand
	}
	g.Deck = deck
	g.CurrentSeat = currentSeat
	g.Ready = true
	if len(deck) > 0 {
		g.Phase = DVPhaseDraw
	} else {
		g.Phase = DVPhaseGuess
	}
	return g
}

func TestDVTileSetComposition(t *testing.T) {
	tiles := newDVTileSet()
	if len(tiles) != 26 {
		t.Fatalf("전체 %d장, want 26", len(tiles))
	}

	counts := map[DVTileColor]map[int]int{DVBlack: {}, DVWhite: {}}
	jokers := 0
	ids := map[int]bool{}
	for _, tile := range tiles {
		if ids[tile.ID] {
			t.Fatalf("중복 ID: %d", tile.ID)
		}
		ids[tile.ID] = true
		if tile.Joker {
			jokers++
			continue
		}
		counts[tile.Color][tile.Value]++
	}
	if jokers != 2 {
		t.Fatalf("조커 %d장, want 2", jokers)
	}
	for _, color := range []DVTileColor{DVBlack, DVWhite} {
		for v := 0; v <= 11; v++ {
			if counts[color][v] != 1 {
				t.Fatalf("%s %d 이 %d장, want 1", color, v, counts[color][v])
			}
		}
	}
}

// completeInitial 전원이 시작 타일을 다 가져올 때까지 색을 골라 가져온다
// (검은색 우선, 없으면 흰색)
func completeInitial(t *testing.T, g *DVGame) {
	t.Helper()
	for guard := 0; g.Phase == DVPhaseInitialDraw && guard < 100; guard++ {
		for _, p := range g.Players {
			if g.InitialRemaining[p.Seat] <= 0 {
				continue
			}
			color := DVBlack
			if g.DeckCountByColor(DVBlack) == 0 {
				color = DVWhite
			}
			if _, err := g.TakeInitial(p.Seat, color); err != nil {
				t.Fatalf("TakeInitial: %v", err)
			}
		}
	}
	if g.Phase == DVPhaseInitialDraw {
		t.Fatal("initial_draw 가 끝나지 않았다")
	}
}

func TestDVDealCounts(t *testing.T) {
	cases := []struct {
		players  int
		handSize int
		deckSize int
	}{
		{2, 4, 18},
		{3, 4, 14},
		{4, 3, 14},
	}
	for _, c := range cases {
		g := NewDVGame("test")
		for i := 0; i < c.players; i++ {
			if _, err := g.AddPlayer("p"); err != nil {
				t.Fatalf("AddPlayer: %v", err)
			}
		}
		if err := g.Start(rand.New(rand.NewSource(1))); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if g.Phase != DVPhaseInitialDraw {
			t.Fatalf("시작 직후 phase = %s, want initial_draw", g.Phase)
		}
		completeInitial(t, g)
		for _, p := range g.Players {
			total := len(p.Tiles) + len(g.PendingJokerTiles[p.Seat])
			if total != c.handSize {
				t.Fatalf("%d인: seat %d 손패 %d장, want %d", c.players, p.Seat, total, c.handSize)
			}
		}
		if len(g.Deck) != c.deckSize {
			t.Fatalf("%d인: 더미 %d장, want %d", c.players, len(g.Deck), c.deckSize)
		}
	}
}

func TestDVStartHandSorted(t *testing.T) {
	for seed := int64(0); seed < 20; seed++ {
		g := NewDVGame("test")
		g.AddPlayer("a")
		g.AddPlayer("b")
		g.AddPlayer("c")
		if err := g.Start(rand.New(rand.NewSource(seed))); err != nil {
			t.Fatalf("Start: %v", err)
		}
		completeInitial(t, g)
		for _, p := range g.Players {
			for i := 1; i < len(p.Tiles); i++ {
				if dvTileLess(p.Tiles[i], p.Tiles[i-1]) {
					t.Fatalf("seed %d seat %d: 정렬 위반 %v", seed, p.Seat, p.Tiles)
				}
			}
		}
	}
}

func TestDVTakeInitialColorChoice(t *testing.T) {
	g := NewDVGame("test")
	g.AddPlayer("a")
	g.AddPlayer("b")
	if err := g.Start(rand.New(rand.NewSource(1))); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// 원하는 색으로 3검+1백 구성이 가능하다
	wantColors := []DVTileColor{DVBlack, DVBlack, DVBlack, DVWhite}
	for _, color := range wantColors {
		tile, err := g.TakeInitial(0, color)
		if err != nil {
			t.Fatalf("TakeInitial(%s): %v", color, err)
		}
		if tile.Color != color {
			t.Fatalf("%s 를 골랐는데 %s 가 나왔다", color, tile.Color)
		}
	}
	// 4장을 다 가져오면 추가 요청은 거부
	if _, err := g.TakeInitial(0, DVBlack); err == nil {
		t.Fatal("초과 가져오기가 허용됐다")
	}
	// 아직 상대가 안 끝났으니 initial_draw 유지
	if g.Phase != DVPhaseInitialDraw {
		t.Fatalf("phase = %s, want initial_draw", g.Phase)
	}
	for i := 0; i < 4; i++ {
		if _, err := g.TakeInitial(1, DVWhite); err != nil {
			t.Fatalf("TakeInitial seat1: %v", err)
		}
	}
	if g.Phase == DVPhaseInitialDraw {
		t.Fatal("전원 완료 후에도 initial_draw 에 머물러 있다")
	}
	// 시작 타일 단계에서는 뽑기가 거부된다
	g2 := NewDVGame("t2")
	g2.AddPlayer("a")
	g2.AddPlayer("b")
	g2.Start(rand.New(rand.NewSource(1)))
	if _, err := g2.DrawTile(g2.CurrentSeat, DVBlack); err == nil {
		t.Fatal("initial_draw 중 DrawTile 이 허용됐다")
	}
}

func TestDVTileOrdering(t *testing.T) {
	// 동값이면 검정이 왼쪽
	if !dvTileLess(dvTile(5, DVBlack, 5), dvTile(17, DVWhite, 5)) {
		t.Fatal("동값에서 검정 < 흰색이어야 한다")
	}
	if dvTileLess(dvTile(17, DVWhite, 5), dvTile(5, DVBlack, 5)) {
		t.Fatal("동값에서 흰색이 검정보다 앞이면 안 된다")
	}

	// insertTile: 조커는 정렬 비교에서 투명 취급.
	// [3검, 조커, 5백] 에 4백 삽입 → [3검, 조커, 4백, 5백]
	g := newTestDVGame(t, [][]DVTile{
		{dvTile(3, DVBlack, 3), dvJoker(24, DVBlack), dvTile(17, DVWhite, 5)},
		{dvTile(7, DVBlack, 7)},
	}, []DVTile{dvTile(16, DVWhite, 4)}, 0)

	g.insertTile(0, dvTile(16, DVWhite, 4))
	got := g.Players[0].Tiles
	wantIDs := []int{3, 24, 16, 17}
	for i, id := range wantIDs {
		if got[i].ID != id {
			t.Fatalf("삽입 결과 %v, want IDs %v", got, wantIDs)
		}
	}

	// 맨 뒤 삽입
	g.insertTile(0, dvTile(11, DVBlack, 11))
	tiles := g.Players[0].Tiles
	if tiles[len(tiles)-1].ID != 11 {
		t.Fatalf("가장 큰 타일은 맨 뒤에 가야 한다: %v", tiles)
	}
}

func TestDVJokerSetupFlow(t *testing.T) {
	// 시작 타일로 조커를 가져오면 배치 대기로 빠지고, 전원이 시작 타일과
	// 조커 배치를 마쳐야 draw 로 넘어간다. 더미를 직접 구성해 seat0 의
	// 검은색 선택이 조커를 집도록 만든다 (takeFromDeck 은 뒤에서부터 집는다).
	g := NewDVGame("test")
	g.Players = append(g.Players, &DVPlayer{Seat: 0, Name: "a"}, &DVPlayer{Seat: 1, Name: "b"})
	g.Deck = []DVTile{dvTile(17, DVWhite, 5), dvJoker(24, DVBlack)}
	g.InitialRemaining = map[int]int{0: 1, 1: 1}
	g.Ready = true
	g.Phase = DVPhaseInitialDraw
	g.CurrentSeat = 0

	tile, err := g.TakeInitial(0, DVBlack)
	if err != nil {
		t.Fatalf("TakeInitial: %v", err)
	}
	if !tile.Joker {
		t.Fatalf("조커가 나와야 하는데 %+v", tile)
	}
	if len(g.PendingJokerTiles[0]) != 1 {
		t.Fatal("가져온 조커는 배치 대기로 가야 한다")
	}
	if _, err := g.TakeInitial(1, DVWhite); err != nil {
		t.Fatalf("TakeInitial seat1: %v", err)
	}
	if g.Phase != DVPhaseJokerSetup {
		t.Fatalf("phase = %s, want joker_setup", g.Phase)
	}

	if err := g.PlaceJoker(0, 24, 0); err != nil {
		t.Fatalf("PlaceJoker: %v", err)
	}
	if g.Phase != DVPhaseDraw {
		t.Fatalf("배치 완료 후 phase = %s, want draw", g.Phase)
	}
	if !g.Players[0].Tiles[0].Joker || g.Players[0].Tiles[0].Revealed {
		t.Fatalf("초기 조커는 비공개로 줄에 있어야 한다: %+v", g.Players[0].Tiles)
	}
}

func TestDVDrawTileColorChoice(t *testing.T) {
	g := newTestDVGame(t, [][]DVTile{
		{dvTile(2, DVBlack, 2)},
		{dvTile(5, DVBlack, 5)},
	}, []DVTile{dvTile(7, DVBlack, 7), dvTile(16, DVWhite, 4), dvTile(3, DVBlack, 3)}, 0)

	if g.DeckCountByColor(DVBlack) != 2 || g.DeckCountByColor(DVWhite) != 1 {
		t.Fatalf("색별 더미 수가 틀렸다: 검%d 백%d", g.DeckCountByColor(DVBlack), g.DeckCountByColor(DVWhite))
	}

	// 흰색을 고르면 흰 타일이 나온다
	tile, err := g.DrawTile(0, DVWhite)
	if err != nil {
		t.Fatalf("DrawTile: %v", err)
	}
	if tile.Color != DVWhite || tile.ID != 16 {
		t.Fatalf("흰색을 골랐는데 %+v", tile)
	}
	if g.DeckCountByColor(DVWhite) != 0 || len(g.Deck) != 2 {
		t.Fatalf("더미에서 빠지지 않았다: %+v", g.Deck)
	}

	// 다음 턴: 남은 흰색이 없으면 흰색 선택은 거부
	g.Guess(0, 1, 0, 0) // 오답으로 턴 종료
	if _, err := g.DrawTile(1, DVWhite); err == nil {
		t.Fatal("흰 타일이 없는데 흰색 선택이 허용됐다")
	}
	if _, err := g.DrawTile(1, DVBlack); err != nil {
		t.Fatalf("검은색 선택이 거부됐다: %v", err)
	}

	// 색이 아닌 값은 거부
	g2 := newTestDVGame(t, [][]DVTile{
		{dvTile(2, DVBlack, 2)},
		{dvTile(5, DVBlack, 5)},
	}, []DVTile{dvTile(7, DVBlack, 7)}, 0)
	if _, err := g2.DrawTile(0, DVTileColor("red")); err == nil {
		t.Fatal("잘못된 색이 허용됐다")
	}
}

func TestDVGuessCorrectThenContinueChoice(t *testing.T) {
	g := newTestDVGame(t, [][]DVTile{
		{dvTile(2, DVBlack, 2), dvTile(20, DVWhite, 8)},
		{dvTile(5, DVBlack, 5), dvTile(21, DVWhite, 9)},
	}, []DVTile{dvTile(7, DVBlack, 7), dvTile(3, DVBlack, 3)}, 0)

	if _, err := g.DrawTile(0, DVBlack); err != nil {
		t.Fatalf("DrawTile: %v", err)
	}
	if g.Phase != DVPhaseGuess {
		t.Fatalf("phase = %s, want guess", g.Phase)
	}

	// 성공 → 공개 + continue_choice
	result, err := g.Guess(0, 1, 0, 5)
	if err != nil {
		t.Fatalf("Guess: %v", err)
	}
	if !result.Correct || !g.Players[1].Tiles[0].Revealed {
		t.Fatal("정답인데 공개되지 않았다")
	}
	if g.Phase != DVPhaseContinueChoice {
		t.Fatalf("phase = %s, want continue_choice", g.Phase)
	}

	// 계속 → 다시 guess
	if err := g.ContinueChoice(0, true); err != nil {
		t.Fatalf("ContinueChoice: %v", err)
	}
	if g.Phase != DVPhaseGuess {
		t.Fatalf("phase = %s, want guess", g.Phase)
	}

	// 또 성공 후 중단 → 뽑은 타일 비공개 삽입 + 턴 이양
	if _, err := g.Guess(0, 1, 1, 9); err != nil {
		t.Fatalf("Guess 2: %v", err)
	}
	// 상대 타일 전부 공개 → 탈락 → 2인이므로 즉시 게임 종료
	if g.Phase != DVPhaseGameOver || g.WinnerSeat != 0 {
		t.Fatalf("phase=%s winner=%d, want game_over/0", g.Phase, g.WinnerSeat)
	}
}

func TestDVContinueStopInsertsDrawnHidden(t *testing.T) {
	g := newTestDVGame(t, [][]DVTile{
		{dvTile(2, DVBlack, 2)},
		{dvTile(5, DVBlack, 5), dvTile(21, DVWhite, 9)},
	}, []DVTile{dvTile(7, DVBlack, 7)}, 0)

	g.DrawTile(0, DVBlack)
	result, err := g.Guess(0, 1, 0, 5)
	if err != nil || !result.Correct {
		t.Fatalf("Guess: %v correct=%v", err, result.Correct)
	}
	if g.Phase != DVPhaseContinueChoice {
		t.Fatalf("phase = %s", g.Phase)
	}
	if err := g.ContinueChoice(0, false); err != nil {
		t.Fatalf("ContinueChoice: %v", err)
	}
	// 뽑은 7검이 2검 오른쪽에 비공개로 삽입
	tiles := g.Players[0].Tiles
	if len(tiles) != 2 || tiles[1].ID != 7 || tiles[1].Revealed {
		t.Fatalf("중단 후 줄 상태가 틀렸다: %+v", tiles)
	}
	if g.CurrentSeat != 1 || g.Phase != DVPhaseGuess {
		// 더미가 비었으므로 다음 턴은 draw 없이 guess
		t.Fatalf("seat=%d phase=%s, want 1/guess", g.CurrentSeat, g.Phase)
	}
}

func TestDVGuessWrongRevealsDrawnTile(t *testing.T) {
	g := newTestDVGame(t, [][]DVTile{
		{dvTile(2, DVBlack, 2)},
		{dvTile(5, DVBlack, 5), dvTile(21, DVWhite, 9)},
	}, []DVTile{dvTile(7, DVBlack, 7), dvTile(3, DVBlack, 3)}, 0)

	g.DrawTile(0, DVBlack) // 3검
	result, err := g.Guess(0, 1, 0, 4)
	if err != nil {
		t.Fatalf("Guess: %v", err)
	}
	if result.Correct {
		t.Fatal("오답이어야 한다")
	}
	// 뽑은 3검이 공개로 삽입되고 턴이 넘어간다
	tiles := g.Players[0].Tiles
	if len(tiles) != 2 || tiles[1].ID != 3 || !tiles[1].Revealed {
		t.Fatalf("실패 후 줄 상태: %+v", tiles)
	}
	if g.CurrentSeat != 1 || g.Phase != DVPhaseDraw {
		t.Fatalf("seat=%d phase=%s, want 1/draw", g.CurrentSeat, g.Phase)
	}
}

func TestDVGuessJoker(t *testing.T) {
	g := newTestDVGame(t, [][]DVTile{
		{dvTile(2, DVBlack, 2)},
		{dvJoker(24, DVBlack), dvTile(5, DVBlack, 5)},
	}, []DVTile{dvTile(7, DVBlack, 7)}, 0)

	g.DrawTile(0, DVBlack)
	// 조커를 5로 추리 → 실패
	result, err := g.Guess(0, 1, 0, 5)
	if err != nil {
		t.Fatalf("Guess: %v", err)
	}
	if result.Correct {
		t.Fatal("조커를 숫자로 추리하면 실패해야 한다")
	}

	// 다음 판: 조커를 조커로 추리 → 성공
	g2 := newTestDVGame(t, [][]DVTile{
		{dvTile(2, DVBlack, 2)},
		{dvJoker(24, DVBlack), dvTile(5, DVBlack, 5)},
	}, []DVTile{dvTile(7, DVBlack, 7)}, 0)
	g2.DrawTile(0, DVBlack)
	result, err = g2.Guess(0, 1, 0, DVJokerValue)
	if err != nil {
		t.Fatalf("Guess: %v", err)
	}
	if !result.Correct || !g2.Players[1].Tiles[0].Revealed {
		t.Fatal("조커 추리 성공이 공개로 이어져야 한다")
	}
}

func TestDVDrawnJokerPlacement(t *testing.T) {
	// 뽑은 타일이 조커일 때: 실패 → 공개 위치 선택, 중단 → 비공개 위치 선택
	g := newTestDVGame(t, [][]DVTile{
		{dvTile(2, DVBlack, 2), dvTile(20, DVWhite, 8)},
		{dvTile(5, DVBlack, 5), dvTile(21, DVWhite, 9)},
	}, []DVTile{dvJoker(25, DVWhite)}, 0)

	g.DrawTile(0, DVWhite)
	if _, err := g.Guess(0, 1, 0, 3); err != nil { // 오답
		t.Fatalf("Guess: %v", err)
	}
	if g.Phase != DVPhasePlaceDrawnJoker || !g.DrawnJokerRevealed {
		t.Fatalf("phase=%s revealed=%v, want place_drawn_joker/true", g.Phase, g.DrawnJokerRevealed)
	}
	if err := g.PlaceJoker(0, 25, 1); err != nil {
		t.Fatalf("PlaceJoker: %v", err)
	}
	tiles := g.Players[0].Tiles
	if tiles[1].ID != 25 || !tiles[1].Revealed {
		t.Fatalf("실패 조커는 공개로 삽입: %+v", tiles)
	}
	if g.CurrentSeat != 1 {
		t.Fatalf("턴이 넘어가야 한다: seat=%d", g.CurrentSeat)
	}
}

func TestDVEmptyDeckTurn(t *testing.T) {
	g := newTestDVGame(t, [][]DVTile{
		{dvTile(2, DVBlack, 2), dvTile(20, DVWhite, 8)},
		{dvTile(5, DVBlack, 5), dvTile(21, DVWhite, 9)},
	}, []DVTile{}, 0)

	// 더미가 비어 있으므로 phase 는 곧장 guess
	if g.Phase != DVPhaseGuess {
		t.Fatalf("phase = %s, want guess", g.Phase)
	}
	if _, err := g.DrawTile(0, DVBlack); err == nil {
		t.Fatal("빈 더미에서 뽑기는 거부돼야 한다")
	}

	// 실패 → 공개할 자기 타일 선택
	result, err := g.Guess(0, 1, 0, 4)
	if err != nil || result.Correct {
		t.Fatalf("Guess: %v", err)
	}
	if g.Phase != DVPhaseRevealOwn {
		t.Fatalf("phase = %s, want reveal_own", g.Phase)
	}
	if err := g.RevealOwn(0, 1); err != nil {
		t.Fatalf("RevealOwn: %v", err)
	}
	if !g.Players[0].Tiles[1].Revealed {
		t.Fatal("선택한 타일이 공개돼야 한다")
	}
	if g.CurrentSeat != 1 || g.Phase != DVPhaseGuess {
		t.Fatalf("seat=%d phase=%s, want 1/guess", g.CurrentSeat, g.Phase)
	}
}

func TestDVEliminationAndVictory(t *testing.T) {
	// 3인: seat1 을 탈락시켜도 게임은 계속, seat2 까지 탈락하면 seat0 승리
	g := newTestDVGame(t, [][]DVTile{
		{dvTile(2, DVBlack, 2), dvTile(20, DVWhite, 8)},
		{dvTile(5, DVBlack, 5)},
		{dvTile(9, DVBlack, 9)},
	}, []DVTile{}, 0)

	result, err := g.Guess(0, 1, 0, 5)
	if err != nil || !result.Correct {
		t.Fatalf("Guess: %v", err)
	}
	if !result.Eliminated || !g.Players[1].Eliminated {
		t.Fatal("전 타일 공개면 탈락이어야 한다")
	}
	if result.GameOver || g.Phase == DVPhaseGameOver {
		t.Fatal("3인 중 1명 탈락으로 끝나면 안 된다")
	}

	// 계속 추리해서 seat2 도 탈락시키면 승리
	if err := g.ContinueChoice(0, true); err != nil {
		t.Fatalf("ContinueChoice: %v", err)
	}
	result, err = g.Guess(0, 2, 0, 9)
	if err != nil || !result.Correct {
		t.Fatalf("Guess: %v", err)
	}
	if !result.GameOver || g.Phase != DVPhaseGameOver || g.WinnerSeat != 0 {
		t.Fatalf("gameOver=%v phase=%s winner=%d", result.GameOver, g.Phase, g.WinnerSeat)
	}
}

func TestDVTurnSkipsEliminated(t *testing.T) {
	g := newTestDVGame(t, [][]DVTile{
		{dvTile(2, DVBlack, 2), dvTile(20, DVWhite, 8)},
		{dvTile(5, DVBlack, 5)},
		{dvTile(9, DVBlack, 9), dvTile(22, DVWhite, 10)},
	}, []DVTile{}, 0)

	// seat1 탈락
	g.Guess(0, 1, 0, 5)
	g.ContinueChoice(0, false)
	// seat0 → (seat1 스킵) → seat2
	if g.CurrentSeat != 2 {
		t.Fatalf("seat=%d, want 2 (탈락자 스킵)", g.CurrentSeat)
	}
}

func TestDVForfeitDuringOwnTurn(t *testing.T) {
	g := newTestDVGame(t, [][]DVTile{
		{dvTile(2, DVBlack, 2), dvTile(20, DVWhite, 8)},
		{dvTile(5, DVBlack, 5), dvTile(21, DVWhite, 9)},
		{dvTile(9, DVBlack, 9)},
	}, []DVTile{dvTile(7, DVBlack, 7)}, 0)

	g.DrawTile(0, DVBlack) // 7검을 든 상태에서 몰수
	g.ForfeitPlayer(0)

	p := g.Players[0]
	if !p.Eliminated {
		t.Fatal("몰수는 탈락이어야 한다")
	}
	for _, tile := range p.Tiles {
		if !tile.Revealed {
			t.Fatalf("몰수 시 전 타일 공개: %+v", p.Tiles)
		}
	}
	// 들고 있던 7검도 공개로 줄에 들어가야 한다
	if len(p.Tiles) != 3 {
		t.Fatalf("뽑은 타일 포함 3장이어야 한다: %+v", p.Tiles)
	}
	if g.DrawnTile != nil {
		t.Fatal("DrawnTile 이 정리돼야 한다")
	}
	// 턴이 seat1 로 넘어가고 게임은 계속 (남은 2인)
	if g.Phase == DVPhaseGameOver {
		t.Fatal("2명이 남았으니 게임은 계속돼야 한다")
	}
	if g.CurrentSeat != 1 {
		t.Fatalf("seat=%d, want 1", g.CurrentSeat)
	}

	// 남은 한 명도 몰수되면 종료
	g.ForfeitPlayer(1)
	if g.Phase != DVPhaseGameOver || g.WinnerSeat != 2 {
		t.Fatalf("phase=%s winner=%d, want game_over/2", g.Phase, g.WinnerSeat)
	}
}

func TestDVInvalidActions(t *testing.T) {
	g := newTestDVGame(t, [][]DVTile{
		{dvTile(2, DVBlack, 2)},
		{dvTile(5, DVBlack, 5), dvTile(21, DVWhite, 9)},
	}, []DVTile{dvTile(7, DVBlack, 7), dvTile(3, DVBlack, 3)}, 0)

	// 남의 턴에 뽑기
	if _, err := g.DrawTile(1, DVBlack); err == nil {
		t.Fatal("남의 턴 뽑기는 거부")
	}
	// draw 단계에서 추리
	if _, err := g.Guess(0, 1, 0, 5); err == nil {
		t.Fatal("draw 단계 추리는 거부")
	}

	g.DrawTile(0, DVBlack)

	// 자기 자신 추리
	if _, err := g.Guess(0, 0, 0, 2); err == nil {
		t.Fatal("자기 타일 추리는 거부")
	}
	// 범위 밖 값
	if _, err := g.Guess(0, 1, 0, 12); err == nil {
		t.Fatal("12 추리는 거부")
	}
	// 없는 타일
	if _, err := g.Guess(0, 1, 5, 3); err == nil {
		t.Fatal("없는 인덱스 추리는 거부")
	}
	// 남의 턴 추리
	if _, err := g.Guess(1, 0, 0, 2); err == nil {
		t.Fatal("남의 턴 추리는 거부")
	}

	// 공개된 타일 재추리
	g.Guess(0, 1, 0, 5) // 성공 → 공개
	g.ContinueChoice(0, true)
	if _, err := g.Guess(0, 1, 0, 5); err == nil {
		t.Fatal("공개 타일 재추리는 거부")
	}

	// continue_choice 아닌데 선택
	if err := g.ContinueChoice(0, false); err == nil {
		t.Fatal("guess 단계에서 continue 선택은 거부")
	}

	// 로비 아닌데 시작
	if err := g.Start(rand.New(rand.NewSource(1))); err == nil {
		t.Fatal("진행 중 게임 재시작은 거부")
	}
}
