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

// dvTestClient 다빈치코드용 테스트 WS 클라이언트. writePump 가 여러 메시지를
// 개행으로 묶어 보내므로 큐로 풀어서 읽는다.
type dvTestClient struct {
	conn  *websocket.Conn
	queue []DVMessage
}

func newDVTestServer(t *testing.T, grace time.Duration) (*DVHub, string, func()) {
	t.Helper()
	hub := NewDVHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeDVWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func dvDial(t *testing.T, url string) *dvTestClient {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	return &dvTestClient{conn: conn}
}

func (c *dvTestClient) send(t *testing.T, msg DVMessage) {
	t.Helper()
	if err := c.conn.WriteJSON(msg); err != nil {
		t.Fatalf("send failed: %v", err)
	}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *dvTestClient) waitFor(t *testing.T, msgType DVMessageType) DVMessage {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(c.queue) == 0 {
			c.conn.SetReadDeadline(deadline)
			_, data, err := c.conn.ReadMessage()
			if err != nil {
				t.Fatalf("read failed waiting for %s: %v", msgType, err)
			}
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if line == "" {
					continue
				}
				var msg DVMessage
				if err := json.Unmarshal([]byte(line), &msg); err != nil {
					t.Fatalf("unmarshal failed: %v (%s)", err, line)
				}
				c.queue = append(c.queue, msg)
			}
		}
		for len(c.queue) > 0 {
			msg := c.queue[0]
			c.queue = c.queue[1:]
			if msg.Type == msgType {
				return msg
			}
		}
	}
	t.Fatalf("timed out waiting for %s", msgType)
	return DVMessage{}
}

func dvPayloadMap(t *testing.T, msg DVMessage) map[string]interface{} {
	t.Helper()
	m, ok := msg.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("payload is not a map: %#v", msg.Payload)
	}
	return m
}

// joinDVLobby 이름으로 로비에 입장하고 sessionId 를 돌려받는다
func joinDVLobby(t *testing.T, c *dvTestClient, name string) string {
	t.Helper()
	c.send(t, DVMessage{Type: DVMsgJoinLobby, Payload: DVJoinLobbyPayload{PlayerName: name}})
	state := dvPayloadMap(t, c.waitFor(t, DVMsgLobbyState))
	return state["sessionId"].(string)
}

// startDVThreePlayers 3인 입장 후 호스트가 시작. 각자의 세션과 첫 game_state 를 반환.
func startDVThreePlayers(t *testing.T, url string) ([]*dvTestClient, []string, []map[string]interface{}) {
	t.Helper()
	clients := []*dvTestClient{dvDial(t, url), dvDial(t, url), dvDial(t, url)}
	sessions := make([]string, 3)
	for i, c := range clients {
		sessions[i] = joinDVLobby(t, c, string(rune('A'+i)))
	}

	clients[0].send(t, DVMessage{Type: DVMsgStartGame})

	states := make([]map[string]interface{}, 3)
	for i, c := range clients {
		states[i] = dvPayloadMap(t, c.waitFor(t, DVMsgGameState))
	}
	return clients, sessions, states
}

// tilesOf game_state payload 에서 특정 좌석의 타일 배열을 꺼낸다
func dvTilesOf(t *testing.T, state map[string]interface{}, seat int) []map[string]interface{} {
	t.Helper()
	players := state["players"].([]interface{})
	for _, p := range players {
		pm := p.(map[string]interface{})
		if int(pm["seat"].(float64)) == seat {
			tiles := []map[string]interface{}{}
			for _, tile := range pm["tiles"].([]interface{}) {
				tiles = append(tiles, tile.(map[string]interface{}))
			}
			return tiles
		}
	}
	t.Fatalf("seat %d not in state", seat)
	return nil
}

// TestDVLobbyStartAndMaskedViews 3인 게임 시작 후 각자의 스냅샷에서
// 자기 타일은 값이 보이고 남의 비공개 타일은 값이 감춰지는지 검증
func TestDVLobbyStartAndMaskedViews(t *testing.T) {
	_, url, cleanup := newDVTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, _, states := startDVThreePlayers(t, url)
	defer func() {
		for _, c := range clients {
			c.conn.Close()
		}
	}()

	for me := 0; me < 3; me++ {
		state := states[me]
		if int(state["yourSeat"].(float64)) != me {
			t.Fatalf("yourSeat = %v, want %d", state["yourSeat"], me)
		}
		if int(state["playerCount"].(float64)) != 3 {
			t.Fatalf("playerCount = %v, want 3", state["playerCount"])
		}

		for seat := 0; seat < 3; seat++ {
			for _, tile := range dvTilesOf(t, state, seat) {
				_, hasValue := tile["value"]
				revealed := tile["revealed"].(bool)
				if seat == me && !hasValue {
					t.Fatalf("seat %d 관점: 자기 타일에 값이 없다: %v", me, tile)
				}
				if seat != me && !revealed && hasValue {
					t.Fatalf("seat %d 관점: 남의 비공개 타일에 값이 샜다: %v", me, tile)
				}
			}
		}
	}
}

// TestDVRejoinRestoresMaskedState 게임 중 끊긴 플레이어가 세션으로 복귀해
// 개인화 스냅샷을 돌려받는지 검증
func TestDVRejoinRestoresMaskedState(t *testing.T) {
	_, url, cleanup := newDVTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients, sessions, _ := startDVThreePlayers(t, url)
	defer func() {
		clients[0].conn.Close()
		clients[1].conn.Close()
	}()

	// seat2 가 끊긴다
	clients[2].conn.Close()
	disc := dvPayloadMap(t, clients[0].waitFor(t, DVMsgPlayerDisconnected))
	if int(disc["seat"].(float64)) != 2 {
		t.Fatalf("disconnected seat = %v, want 2", disc["seat"])
	}

	// 새 연결로 재접속
	rejoined := dvDial(t, url)
	defer rejoined.conn.Close()
	rejoined.send(t, DVMessage{Type: DVMsgRejoinGame, Payload: DVRejoinGamePayload{SessionID: sessions[2]}})

	recon := dvPayloadMap(t, clients[0].waitFor(t, DVMsgPlayerReconnected))
	if int(recon["seat"].(float64)) != 2 {
		t.Fatalf("reconnected seat = %v, want 2", recon["seat"])
	}

	state := dvPayloadMap(t, rejoined.waitFor(t, DVMsgGameState))
	if int(state["yourSeat"].(float64)) != 2 {
		t.Fatalf("yourSeat = %v, want 2", state["yourSeat"])
	}
	// 복원된 스냅샷에서도 마스킹이 유지된다
	for _, tile := range dvTilesOf(t, state, 0) {
		if !tile["revealed"].(bool) {
			if _, hasValue := tile["value"]; hasValue {
				t.Fatalf("재접속 스냅샷에서 남의 비공개 타일 값이 샜다: %v", tile)
			}
		}
	}
	for _, tile := range dvTilesOf(t, state, 2) {
		if _, hasValue := tile["value"]; !hasValue {
			t.Fatalf("재접속 스냅샷에서 자기 타일 값이 없다: %v", tile)
		}
	}
}

// TestDVGraceExpiryForfeitsAndGameContinues 유예 만료 시 몰수 후 남은
// 인원으로 게임이 계속되고, 한 명만 남으면 종료되는지 검증
func TestDVGraceExpiryForfeitsAndGameContinues(t *testing.T) {
	_, url, cleanup := newDVTestServer(t, 100*time.Millisecond)
	defer cleanup()

	clients, _, _ := startDVThreePlayers(t, url)
	defer func() {
		clients[0].conn.Close()
	}()

	// seat2 이탈 → 유예 만료 → 몰수, 게임은 2인으로 계속
	clients[2].conn.Close()
	for {
		event := dvPayloadMap(t, clients[0].waitFor(t, DVMsgEvent))
		if event["kind"] == "player_forfeited" {
			if int(event["seat"].(float64)) != 2 {
				t.Fatalf("forfeited seat = %v, want 2", event["seat"])
			}
			break
		}
	}
	state := dvPayloadMap(t, clients[0].waitFor(t, DVMsgGameState))
	if state["phase"] == string(DVPhaseGameOver) {
		t.Fatal("2명이 남았는데 게임이 끝났다")
	}
	for _, tile := range dvTilesOf(t, state, 2) {
		if !tile["revealed"].(bool) {
			t.Fatalf("몰수된 좌석의 타일이 공개되지 않았다: %v", tile)
		}
	}

	// seat1 도 이탈 → 몰수 → seat0 승리로 종료
	clients[1].conn.Close()
	over := dvPayloadMap(t, clients[0].waitFor(t, DVMsgGameOver))
	if int(over["winnerSeat"].(float64)) != 0 {
		t.Fatalf("winnerSeat = %v, want 0", over["winnerSeat"])
	}
	if over["reason"] != "forfeit_win" {
		t.Fatalf("reason = %v, want forfeit_win", over["reason"])
	}
	// 종료 페이로드는 전 타일이 공개 값 포함이어야 한다
	for _, p := range over["players"].([]interface{}) {
		for _, tileRaw := range p.(map[string]interface{})["tiles"].([]interface{}) {
			tile := tileRaw.(map[string]interface{})
			if _, hasValue := tile["value"]; !hasValue {
				t.Fatalf("game_over 에 값 없는 타일: %v", tile)
			}
		}
	}
}

// TestDVDoubleJoinIgnored 입장 버튼 연타로 같은 연결이 join 을 두 번 보내도
// 좌석은 하나만 생긴다
func TestDVDoubleJoinIgnored(t *testing.T) {
	_, url, cleanup := newDVTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c1 := dvDial(t, url)
	defer c1.conn.Close()
	c1.send(t, DVMessage{Type: DVMsgJoinLobby, Payload: DVJoinLobbyPayload{PlayerName: "연타"}})
	c1.send(t, DVMessage{Type: DVMsgJoinLobby, Payload: DVJoinLobbyPayload{PlayerName: "연타"}})
	c1.waitFor(t, DVMsgLobbyState)

	// 두 번째 클라이언트가 본 로비에는 연타 1명 + 자신 = 2명이어야 한다
	c2 := dvDial(t, url)
	defer c2.conn.Close()
	c2.send(t, DVMessage{Type: DVMsgJoinLobby, Payload: DVJoinLobbyPayload{PlayerName: "정상"}})
	state := dvPayloadMap(t, c2.waitFor(t, DVMsgLobbyState))
	players := state["players"].([]interface{})
	if len(players) != 2 {
		t.Fatalf("로비 인원 %d명, want 2 (연타 입장은 한 번만 반영)", len(players))
	}
}

// TestDVLobbyLeaveReassignsSeats 로비 이탈 시 좌석이 당겨지고 호스트가 승계되는지
func TestDVLobbyLeaveReassignsSeats(t *testing.T) {
	_, url, cleanup := newDVTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c1 := dvDial(t, url)
	c2 := dvDial(t, url)
	defer c2.conn.Close()
	joinDVLobby(t, c1, "A")
	joinDVLobby(t, c2, "B")

	// 호스트(seat0)가 나가면 B 가 seat0 호스트가 된다
	c1.conn.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("호스트 승계 로비 상태를 받지 못했다")
		}
		state := dvPayloadMap(t, c2.waitFor(t, DVMsgLobbyState))
		players := state["players"].([]interface{})
		if len(players) != 1 {
			continue
		}
		if int(state["yourSeat"].(float64)) != 0 || int(state["hostSeat"].(float64)) != 0 {
			t.Fatalf("좌석 승계 실패: %v", state)
		}
		break
	}
}
