package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// stBot 사람전 완주용 WS 드라이버 봇 (ST에는 서버 연습봇이 없다).
// 클랜 카드를 빈 돌에 내고, 획득 가능하면 획득하고, 낼 수 없으면 패스한다.
// 양쪽이 이 전략을 쓰면 승리 또는 교착으로 반드시 game_over 에 닿는다.
type stBot struct {
	conn *websocket.Conn
	done chan<- string
}

func (b *stBot) send(msg STMessage) {
	data, _ := json.Marshal(msg)
	b.conn.WriteMessage(websocket.TextMessage, data)
}

// handle 내 차례의 스냅샷마다 다음 한 수를 보낸다
func (b *stBot) handle(msg STMessage) {
	if msg.Type != STMsgGameState {
		return
	}
	raw, _ := json.Marshal(msg.Payload)
	var st STGameStatePayload
	if json.Unmarshal(raw, &st) != nil {
		return
	}
	if st.CurrentSide != st.YourSide {
		return
	}

	switch st.Phase {
	case STPhasePlay:
		// 손패의 첫 클랜 카드를 자리가 남은 첫 돌에 낸다
		for hi, c := range st.YourHand {
			if !c.IsClan() {
				continue
			}
			for _, stone := range st.Stones {
				if stone.Owner == "" && len(stone.YourCards) < stone.Required {
					b.send(STMessage{Type: STMsgPlayCard,
						Payload: STPlayCardPayload{HandIndex: hi, StoneIndex: stone.Index}})
					return
				}
			}
			break
		}
		// 낼 수 있는 클랜 카드가 없으면 패스 (양쪽 패스면 교착 종료)
		b.send(STMessage{Type: STMsgPass})

	case STPhaseClaim:
		for _, stone := range st.Stones {
			if stone.Claimable {
				b.send(STMessage{Type: STMsgClaimStone,
					Payload: STClaimStonePayload{StoneIndex: stone.Index}})
				return
			}
		}
		b.send(STMessage{Type: STMsgEndTurn})

	case STPhaseDraw:
		// 전술 변형: 클랜 덱 우선으로 뽑는다
		deck := "clan"
		if st.DeckCount == 0 {
			deck = "tactic"
		}
		b.send(STMessage{Type: STMsgDraw, Payload: STDrawPayload{Deck: deck}})
	}
}

// run raw conn 으로 game_over 까지 진행한다. 종료 후에는 testConn 큐로 읽는다.
func (b *stBot) run(initial map[string]interface{}) {
	if initial != nil {
		b.handle(STMessage{Type: STMsgGameState, Payload: initial})
	}

	deadline := time.Now().Add(20 * time.Second)
	b.conn.SetReadDeadline(deadline)

	for time.Now().Before(deadline) {
		_, data, err := b.conn.ReadMessage()
		if err != nil {
			return
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var msg STMessage
			if json.Unmarshal([]byte(line), &msg) != nil {
				continue
			}
			if msg.Type == STMsgGameOver {
				raw, _ := json.Marshal(msg.Payload)
				var over struct {
					Winner string `json:"winner"`
				}
				json.Unmarshal(raw, &over)
				b.done <- over.Winner
				return
			}
			b.handle(msg)
		}
	}
}

// startSTGameWithMode 지정한 모드로 2인 입장 후 게임 시작
func startSTGameWithMode(t *testing.T, url, mode string) ([]*stTestClient, []string, []map[string]interface{}) {
	t.Helper()
	clients := []*stTestClient{stDial(t, url), stDial(t, url)}
	sessions := make([]string, 2)
	for i, c := range clients {
		c.send(t, STMessage{Type: STMsgJoinGame,
			Payload: STJoinGamePayload{PlayerName: string(rune('A' + i)), Mode: mode}})
		joined := stPayloadMap(t, c.waitFor(t, STMsgPlayerJoined))
		sessions[i] = joined["sessionId"].(string)
	}
	states := make([]map[string]interface{}, 2)
	for i, c := range clients {
		c.waitFor(t, STMsgGameStart)
		states[i] = stPayloadMap(t, c.waitFor(t, STMsgGameState))
	}
	return clients, sessions, states
}

// stPlayToGameOver 두 드라이버 봇으로 사람전을 완주시킨다
func stPlayToGameOver(t *testing.T, clients []*stTestClient, states []map[string]interface{}) {
	t.Helper()
	done := make(chan string, 2)
	for i, c := range clients {
		bot := &stBot{conn: c.conn, done: done}
		go bot.run(states[i])
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("사람전 완주 실패")
	}
	// 두 드라이버 모두 game_over 를 읽고 종료할 때까지 잠깐 대기
	time.Sleep(100 * time.Millisecond)
}

// TestSTRematchOfferHumans 사람전 재대결: 한쪽 신청 → 상대 제안 수신 → 수락 시 재시작
func TestSTRematchOfferHumans(t *testing.T) {
	_, url, cleanup := newSTTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, sessions, states := startSTGame(t, url)
	defer func() {
		for _, c := range clients {
			c.conn.Close()
		}
	}()

	stPlayToGameOver(t, clients, states)

	clients[0].send(t, STMessage{Type: STMsgRematch})
	clients[1].waitFor(t, STMsgRematchOffer)

	clients[1].send(t, STMessage{Type: STMsgRematch})
	for i, c := range clients {
		rejoined := stPayloadMap(t, c.waitFor(t, STMsgPlayerJoined))
		if rejoined["sessionId"].(string) != sessions[i] {
			t.Fatalf("클라 %d: 재대결 후 세션이 바뀌었다", i)
		}
		state := stPayloadMap(t, c.waitFor(t, STMsgGameState))
		if state["phase"] != string(STPhasePlay) {
			t.Fatalf("재대결 phase = %v, want play", state["phase"])
		}
		if state["tacticMode"] != false {
			t.Fatalf("기본 모드 재대결인데 tacticMode = %v", state["tacticMode"])
		}
	}
}

// TestSTRematchKeepsTacticMode 전술 변형 게임의 재대결은 전술 모드를 유지한다
func TestSTRematchKeepsTacticMode(t *testing.T) {
	_, url, cleanup := newSTTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, _, states := startSTGameWithMode(t, url, "tactic")
	defer func() {
		for _, c := range clients {
			c.conn.Close()
		}
	}()

	stPlayToGameOver(t, clients, states)

	clients[0].send(t, STMessage{Type: STMsgRematch})
	clients[1].waitFor(t, STMsgRematchOffer)

	clients[1].send(t, STMessage{Type: STMsgRematch})
	for i, c := range clients {
		c.waitFor(t, STMsgPlayerJoined)
		state := stPayloadMap(t, c.waitFor(t, STMsgGameState))
		if state["phase"] != string(STPhasePlay) {
			t.Fatalf("클라 %d: 재대결 phase = %v, want play", i, state["phase"])
		}
		if state["tacticMode"] != true {
			t.Fatalf("클라 %d: 재대결에서 전술 모드가 풀렸다", i)
		}
	}
}

// TestSTRematchWindowExpires 창이 지나면 세션 만료 통지와 함께 방이 정리되고,
// 같은 연결로 새 게임에 다시 입장할 수 있다 (신원 클리어 확인)
func TestSTRematchWindowExpires(t *testing.T) {
	_, url, cleanup := newSTTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, _, states := startSTGame(t, url)
	defer func() {
		for _, c := range clients {
			c.conn.Close()
		}
	}()

	stPlayToGameOver(t, clients, states)

	// 재대결 신청 없이 창(700ms) 경과 → 양쪽 모두 세션 만료
	for _, c := range clients {
		c.waitFor(t, STMsgSessionExpired)
	}

	// 신원이 비워졌으므로 같은 연결로 새 게임 입장이 가능해야 한다
	clients[0].send(t, STMessage{Type: STMsgJoinGame,
		Payload: STJoinGamePayload{PlayerName: "다시온사람"}})
	clients[0].waitFor(t, STMsgPlayerJoined)
	clients[0].waitFor(t, STMsgWaitingPlayer)
}
