package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// gsTestClient 공용 testConn 에 게임 메시지 타입의 waitFor 를 얹은 래퍼
type gsTestClient struct {
	testConn[GSMessage]
}

func newGSTestServer(t *testing.T, grace time.Duration) (*GSHub, string, func()) {
	t.Helper()
	hub := NewGSHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeGSWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func gsDial(t *testing.T, url string) *gsTestClient {
	t.Helper()
	return &gsTestClient{dialWS[GSMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *gsTestClient) waitFor(t *testing.T, msgType GSMessageType) GSMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m GSMessage) bool { return m.Type == msgType })
}

func gsPayloadMap(t *testing.T, msg GSMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// startGSGame 2인 입장 → 양측 배치 제출까지. 각자의 세션과 play 진입
// 스냅샷을 반환. 배치: 양측 모두 뒷줄 4칸이 좋은 유령.
func startGSGame(t *testing.T, url string) ([]*gsTestClient, []string, []map[string]interface{}) {
	t.Helper()
	clients := []*gsTestClient{gsDial(t, url), gsDial(t, url)}
	sessions := make([]string, 2)
	for i, c := range clients {
		c.send(t, GSMessage{Type: GSMsgJoinGame, Payload: GSJoinGamePayload{PlayerName: string(rune('A' + i))}})
		joined := gsPayloadMap(t, c.waitFor(t, GSMsgPlayerJoined))
		sessions[i] = joined["sessionId"].(string)
	}

	// 첫 스냅샷(setup 단계) 수신 후 배치 제출
	setups := [][]GSCell{
		{{5, 1}, {5, 2}, {5, 3}, {5, 4}}, // south (A)
		{{0, 1}, {0, 2}, {0, 3}, {0, 4}}, // north (B)
	}
	for i, c := range clients {
		c.waitFor(t, GSMsgGameState)
		c.send(t, GSMessage{Type: GSMsgSubmitSetup, Payload: GSSubmitSetupPayload{GoodCells: setups[i]}})
	}

	states := make([]map[string]interface{}, 2)
	for i, c := range clients {
		states[i] = c.waitMatchState(t)
	}
	return clients, sessions, states
}

// waitMatchState phase 가 play 인 스냅샷이 올 때까지 소비
func (c *gsTestClient) waitMatchState(t *testing.T) map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state := gsPayloadMap(t, c.waitFor(t, GSMsgGameState))
		if state["phase"] == string(GSPhasePlay) {
			return state
		}
	}
	t.Fatal("play 단계 스냅샷을 받지 못했다")
	return nil
}

// TestGSMaskedViews 상대 유령의 정체(good)가 스냅샷에 실리지 않는지
func TestGSMaskedViews(t *testing.T) {
	_, url, cleanup := newGSTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, _, states := startGSGame(t, url)
	defer func() {
		for _, c := range clients {
			c.conn.Close()
		}
	}()

	for i, state := range states {
		mySide := state["yourSide"].(string)
		ghosts := state["ghosts"].([]interface{})
		if len(ghosts) != 16 {
			t.Fatalf("유령 %d개, want 16", len(ghosts))
		}
		myGood := 0
		for _, ghostRaw := range ghosts {
			ghost := ghostRaw.(map[string]interface{})
			_, hasGood := ghost["good"]
			if ghost["side"] == mySide {
				if !hasGood {
					t.Fatalf("클라 %d: 내 유령에 good 이 없다: %v", i, ghost)
				}
				if ghost["good"].(bool) {
					myGood++
				}
			} else if hasGood {
				t.Fatalf("클라 %d: 상대 유령 정체가 샜다: %v", i, ghost)
			}
		}
		if myGood != 4 {
			t.Fatalf("클라 %d: 좋은 유령 %d개, want 4", i, myGood)
		}
	}
}

// TestGSCapturePublicAndRejoin 잡기 색 공개·재접속 복원 검증
func TestGSCapturePublicAndRejoin(t *testing.T) {
	_, url, cleanup := newGSTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, sessions, states := startGSGame(t, url)
	defer func() {
		clients[0].conn.Close()
	}()

	// 현재 턴 클라이언트가 앞쪽 유령을 한 칸 전진
	turnIdx := 0
	if states[0]["currentSide"] != states[0]["yourSide"] {
		turnIdx = 1
	}
	forward := map[int]GSMessage{
		0: {Type: GSMsgMove, Payload: GSMovePayload{From: GSCell{4, 1}, To: GSCell{3, 1}}},
		1: {Type: GSMsgMove, Payload: GSMovePayload{From: GSCell{1, 1}, To: GSCell{2, 1}}},
	}
	clients[turnIdx].send(t, forward[turnIdx])
	for _, c := range clients {
		ev := gsPayloadMap(t, c.waitFor(t, GSMsgEvent))
		if ev["kind"] != "move" {
			t.Fatalf("kind = %v, want move", ev["kind"])
		}
	}

	// 상대(비턴) 클라이언트가 끊고 재접속 → 스냅샷 복원 + 마스킹 유지
	other := 1 - turnIdx
	clients[other].conn.Close()
	clients[turnIdx].waitFor(t, GSMsgOpponentDisconnected)

	rejoined := gsDial(t, url)
	defer rejoined.conn.Close()
	rejoined.send(t, GSMessage{Type: GSMsgRejoinGame, Payload: GSRejoinGamePayload{SessionID: sessions[other]}})
	clients[turnIdx].waitFor(t, GSMsgOpponentReconnected)

	state := gsPayloadMap(t, rejoined.waitFor(t, GSMsgGameState))
	if state["phase"] != string(GSPhasePlay) {
		t.Fatalf("복원 phase = %v", state["phase"])
	}
	mySide := state["yourSide"].(string)
	for _, ghostRaw := range state["ghosts"].([]interface{}) {
		ghost := ghostRaw.(map[string]interface{})
		if _, hasGood := ghost["good"]; ghost["side"] != mySide && hasGood {
			t.Fatalf("재접속 스냅샷에서 상대 정체가 샜다: %v", ghost)
		}
	}
}

// TestGSBotsCompleteGame 무작위 합법 수 봇 2개가 게임을 완주하는지
func TestGSBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newGSTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, _, states := startGSGame(t, url)
	defer func() {
		for _, c := range clients {
			c.conn.Close()
		}
	}()

	done := make(chan string, 2)
	for i, c := range clients {
		bot := &gsBot{conn: c.conn, done: done}
		go bot.run(states[i])
	}

	select {
	case winner := <-done:
		if winner != string(GSSouth) && winner != string(GSNorth) {
			t.Fatalf("winner = %q", winner)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("20초 안에 게임이 끝나지 않았다 — 진행 불가 상태")
	}
}

// gsBotState 봇이 쓰는 최소 스냅샷
type gsBotState struct {
	YourSide    string `json:"yourSide"`
	Phase       string `json:"phase"`
	CurrentSide string `json:"currentSide"`
	Ghosts      []struct {
		Side string `json:"side"`
		Row  int    `json:"row"`
		Col  int    `json:"col"`
		Good *bool  `json:"good"`
	} `json:"ghosts"`
}

// gsBot 상태를 받을 때마다 내 턴이면 합법 수 하나를 둔다. 탈출 가능하면
// 탈출, 아니면 유령·방향을 카운터로 회전시키며 골라 왕복 교착을 피한다.
type gsBot struct {
	conn    *websocket.Conn
	done    chan<- string
	counter int
}

func (b *gsBot) send(msg GSMessage) {
	data, _ := json.Marshal(msg)
	b.conn.WriteMessage(websocket.TextMessage, data)
}

func (b *gsBot) handleState(state gsBotState) {
	if state.Phase != string(GSPhasePlay) || state.CurrentSide != state.YourSide {
		return
	}

	occupied := map[GSCell]string{}
	mine := []int{}
	for i, ghost := range state.Ghosts {
		occupied[GSCell{ghost.Row, ghost.Col}] = ghost.Side
		if ghost.Side == state.YourSide {
			mine = append(mine, i)
		}
	}
	mySide := GSSide(state.YourSide)
	b.counter++

	// 탈출 가능하면 즉시 탈출
	for _, idx := range mine {
		ghost := state.Ghosts[idx]
		if ghost.Good != nil && *ghost.Good && gsIsExit(mySide, ghost.Row, ghost.Col) {
			b.send(GSMessage{Type: GSMsgMove, Payload: GSMovePayload{From: GSCell{ghost.Row, ghost.Col}, Escape: true}})
			return
		}
	}

	dirs := [][2]int{{-1, 0}, {0, -1}, {0, 1}, {1, 0}}
	for i := range mine {
		ghost := state.Ghosts[mine[(b.counter+i)%len(mine)]]
		from := GSCell{ghost.Row, ghost.Col}
		for j := range dirs {
			d := dirs[(b.counter+j)%len(dirs)]
			to := GSCell{ghost.Row + d[0], ghost.Col + d[1]}
			if to.Row < 0 || to.Row >= GSBoardSize || to.Col < 0 || to.Col >= GSBoardSize {
				continue
			}
			if occupied[to] == state.YourSide {
				continue
			}
			b.send(GSMessage{Type: GSMsgMove, Payload: GSMovePayload{From: from, To: to}})
			return
		}
	}
}

func (b *gsBot) run(initial map[string]interface{}) {
	if initial != nil {
		raw, _ := json.Marshal(initial)
		var state gsBotState
		if json.Unmarshal(raw, &state) == nil {
			b.handleState(state)
		}
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
			var msg GSMessage
			if json.Unmarshal([]byte(line), &msg) != nil {
				continue
			}
			if msg.Type == GSMsgGameOver {
				raw, _ := json.Marshal(msg.Payload)
				var over struct {
					Winner string `json:"winner"`
				}
				json.Unmarshal(raw, &over)
				b.done <- over.Winner
				return
			}
			if msg.Type != GSMsgGameState {
				continue
			}
			raw, _ := json.Marshal(msg.Payload)
			var state gsBotState
			if json.Unmarshal(raw, &state) == nil {
				b.handleState(state)
			}
		}
	}
}
