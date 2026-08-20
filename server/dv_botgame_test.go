package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// dvBot 상태 스냅샷에 반응해 규칙에 맞는 아무 수나 두는 프로토콜 드라이버.
// 판단은 서버 연습봇과 같은 dvBrain(dv_bot.go)을 그대로 쓴다 — 드라이버는
// 그 위의 WS 껍데기일 뿐이다.
type dvBot struct {
	conn  *websocket.Conn
	brain *dvBrain
	done  chan<- int
}

func (b *dvBot) send(msg DVMessage) {
	data, _ := json.Marshal(msg)
	b.conn.WriteMessage(websocket.TextMessage, data)
}

func (b *dvBot) handleState(state dvBotState) {
	if reply := b.brain.decideState(state); reply != nil {
		b.send(*reply)
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
		bot := &dvBot{conn: c.conn, brain: newDVBrain(), done: done}
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
