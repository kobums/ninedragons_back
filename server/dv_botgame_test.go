package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// dvBotState 봇이 상태 스냅샷에서 꺼내 쓰는 최소 정보
type dvBotState struct {
	YourSeat          int    `json:"yourSeat"`
	Phase             string `json:"phase"`
	CurrentSeat       int    `json:"currentSeat"`
	YourPendingJokers []struct {
		ID int `json:"id"`
	} `json:"yourPendingJokers"`
	DrawnTile *struct {
		ID int `json:"id"`
	} `json:"drawnTile"`
	Players []struct {
		Seat       int  `json:"seat"`
		Eliminated bool `json:"eliminated"`
		Tiles      []struct {
			Revealed bool `json:"revealed"`
		} `json:"tiles"`
	} `json:"players"`
}

// dvBot 상태 스냅샷에 반응해 규칙에 맞는 아무 수나 두는 봇.
type dvBot struct {
	conn         *websocket.Conn
	guessCounter int
	done         chan<- int
}

func (b *dvBot) send(msg DVMessage) {
	data, _ := json.Marshal(msg)
	b.conn.WriteMessage(websocket.TextMessage, data)
}

func (b *dvBot) handleState(state dvBotState) {
	me := state.YourSeat

	// 초기 조커 배치는 턴과 무관하게 처리한다
	if state.Phase == string(DVPhaseJokerSetup) {
		if len(state.YourPendingJokers) > 0 {
			b.send(DVMessage{Type: DVMsgPlaceJoker, Payload: DVPlaceJokerPayload{
				TileID: state.YourPendingJokers[0].ID, Position: 0,
			}})
		}
		return
	}

	if state.CurrentSeat != me {
		return
	}

	switch state.Phase {
	case string(DVPhaseDraw):
		b.send(DVMessage{Type: DVMsgDrawTile})

	case string(DVPhaseGuess):
		// 첫 번째 상대의 첫 비공개 타일을 -1~11 순환 값으로 추리
	guessLoop:
		for _, p := range state.Players {
			if p.Seat == me || p.Eliminated {
				continue
			}
			for idx, tile := range p.Tiles {
				if !tile.Revealed {
					value := (b.guessCounter % 13) - 1
					b.guessCounter++
					b.send(DVMessage{Type: DVMsgGuess, Payload: DVGuessPayload{
						TargetSeat: p.Seat, TileIndex: idx, Value: value,
					}})
					break guessLoop
				}
			}
		}

	case string(DVPhaseContinueChoice):
		b.send(DVMessage{Type: DVMsgContinueChoice, Payload: DVContinueChoicePayload{Continue: false}})

	case string(DVPhasePlaceDrawnJoker):
		if state.DrawnTile != nil {
			b.send(DVMessage{Type: DVMsgPlaceJoker, Payload: DVPlaceJokerPayload{
				TileID: state.DrawnTile.ID, Position: 0,
			}})
		}

	case string(DVPhaseRevealOwn):
		for _, p := range state.Players {
			if p.Seat != me {
				continue
			}
			for idx, tile := range p.Tiles {
				if !tile.Revealed {
					b.send(DVMessage{Type: DVMsgRevealOwn, Payload: DVRevealOwnPayload{TileIndex: idx}})
					break
				}
			}
		}
	}
}

// run 초기 스냅샷을 먼저 처리하고, 이후 서버 메시지에 반응한다.
// 게임이 끝나면 done 에 승자 좌석을 보낸다.
func (b *dvBot) run(initial map[string]interface{}) {
	if initial != nil {
		raw, _ := json.Marshal(initial)
		var state dvBotState
		if json.Unmarshal(raw, &state) == nil {
			b.handleState(state)
		}
	}

	deadline := time.Now().Add(15 * time.Second)
	b.conn.SetReadDeadline(deadline)

	for time.Now().Before(deadline) {
		_, data, err := b.conn.ReadMessage()
		if err != nil {
			return // 게임 종료 후 서버가 닫으면 여기로 나온다
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var msg DVMessage
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				continue
			}

			switch msg.Type {
			case DVMsgGameOver:
				raw, _ := json.Marshal(msg.Payload)
				var over struct {
					WinnerSeat int `json:"winnerSeat"`
				}
				json.Unmarshal(raw, &over)
				b.done <- over.WinnerSeat
				return

			case DVMsgGameState:
				raw, _ := json.Marshal(msg.Payload)
				var state dvBotState
				if json.Unmarshal(raw, &state) == nil {
					b.handleState(state)
				}
			}
		}
	}
}

// TestDVBotsCompleteGame 3인 봇이 프로토콜만으로 게임을 끝까지 완주하는지.
// 상태머신 어디에서도 막히지 않고 승자가 나와야 한다.
func TestDVBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newDVTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, _, states := startDVThreePlayers(t, url)
	defer func() {
		for _, c := range clients {
			c.conn.Close()
		}
	}()

	done := make(chan int, 3)
	for i, c := range clients {
		bot := &dvBot{conn: c.conn, done: done}
		go bot.run(states[i])
	}

	select {
	case winner := <-done:
		if winner < 0 || winner > 2 {
			t.Fatalf("winnerSeat = %d, want 0~2", winner)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("15초 안에 게임이 끝나지 않았다 — 상태머신 어딘가에서 멈춤")
	}
}
