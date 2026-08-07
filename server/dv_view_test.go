package server

import "testing"

// TestDVJokerInfoHidden 조커 보유·위치가 남에게 새지 않는지 뷰 단위로 검증
func TestDVJokerInfoHidden(t *testing.T) {
	g := NewDVGame("test")
	g.Players = append(g.Players,
		&DVPlayer{Seat: 0, Name: "a", Tiles: []DVTile{dvTile(3, DVBlack, 3)}},
		&DVPlayer{Seat: 1, Name: "b", Tiles: []DVTile{dvTile(17, DVWhite, 5)}},
	)
	g.PendingJokerTiles[0] = []DVTile{dvJoker(24, DVBlack)}
	g.Deck = []DVTile{dvTile(7, DVBlack, 7)}
	g.Ready = true
	g.Phase = DVPhaseJokerSetup
	g.CurrentSeat = 0

	hub := NewDVHub()
	room := &dvRoom{Game: g, Clients: map[int]*DVClient{}}

	// 배치 당사자는 실제 단계와 자신의 조커를 본다
	own := hub.buildDVState(room, 0)
	if own.Phase != DVPhaseJokerSetup || len(own.YourPendingJokers) != 1 {
		t.Fatalf("당사자 뷰가 틀렸다: phase=%s pending=%d", own.Phase, len(own.YourPendingJokers))
	}

	// 남에게는 시작 타일 단계로 위장되고, 셋업 중 상대 줄은 색까지 감춘다
	other := hub.buildDVState(room, 1)
	if other.Phase != DVPhaseInitialDraw {
		t.Fatalf("조커 배치 단계가 남에게 %s 로 보였다, want initial_draw", other.Phase)
	}
	if len(other.YourPendingJokers) != 0 {
		t.Fatal("남의 조커가 보였다")
	}
	for _, pv := range other.Players {
		if pv.Seat != 0 {
			continue
		}
		for _, tile := range pv.Tiles {
			if tile.Color != "" || tile.Value != nil || tile.Joker != nil {
				t.Fatalf("셋업 중 상대 줄 정보가 샜다: %+v", tile)
			}
		}
	}

	// 뽑은 조커: 남에게는 계속/중단 고민 중으로 위장되고 값이 안 보인다
	g.PendingJokerTiles = map[int][]DVTile{}
	joker := dvJoker(25, DVWhite)
	g.DrawnTile = &joker
	g.DrawnJokerRevealed = false
	g.Phase = DVPhasePlaceDrawnJoker

	other = hub.buildDVState(room, 1)
	if other.Phase != DVPhaseContinueChoice {
		t.Fatalf("비공개 조커 배치가 남에게 %s 로 보였다, want continue_choice", other.Phase)
	}
	if other.DrawnTile == nil || other.DrawnTile.Joker != nil || other.DrawnTile.Value != nil {
		t.Fatalf("뽑은 조커 정보가 샜다: %+v", other.DrawnTile)
	}

	// 실패로 공개될 조커도 줄에 놓이기 전에는 남에게 알리지 않는다
	g.DrawnJokerRevealed = true
	other = hub.buildDVState(room, 1)
	if other.Phase != DVPhaseGuess {
		t.Fatalf("공개 조커 배치가 남에게 %s 로 보였다, want guess", other.Phase)
	}
	if other.DrawnTile.Joker != nil || other.DrawnTile.Value != nil || other.DrawnTile.Revealed {
		t.Fatalf("뽑은 조커 정보가 샜다: %+v", other.DrawnTile)
	}

	// 당사자는 실제 단계·값을 그대로 본다
	own = hub.buildDVState(room, 0)
	if own.Phase != DVPhasePlaceDrawnJoker || own.DrawnTile.Joker == nil || !*own.DrawnTile.Joker {
		t.Fatalf("당사자 뷰가 틀렸다: %+v", own)
	}

	// 본게임 중 남의 비공개 타일 ID 는 위치 기반 가짜 ID 다 (추적 차단)
	g.DrawnTile = nil
	g.DrawnJokerRevealed = false
	g.Phase = DVPhaseDraw
	other = hub.buildDVState(room, 1)
	for _, pv := range other.Players {
		if pv.Seat != 0 {
			continue
		}
		if pv.Tiles[0].ID == 3 {
			t.Fatal("남의 비공개 타일에 실제 ID 가 노출됐다")
		}
		if pv.Tiles[0].Color != DVBlack {
			t.Fatal("본게임에서는 색이 보여야 한다")
		}
	}
}
