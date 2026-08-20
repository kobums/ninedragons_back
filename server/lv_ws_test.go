package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 테스트에서는 자동 진행 대기를 짧게 낮춘다
func init() {
	lvAfkTimeout = 120 * time.Millisecond
	lvRoundEndDelay = 40 * time.Millisecond
}

// lvTestClient 공용 testConn 에 러브레터 메시지 타입의 waitFor 를 얹은 래퍼
type lvTestClient struct {
	testConn[LVMessage]
}

func newLVTestServer(t *testing.T, grace time.Duration) (*LVHub, string, func()) {
	t.Helper()
	hub := NewLVHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeLVWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func lvDial(t *testing.T, url string) *lvTestClient {
	t.Helper()
	return &lvTestClient{dialWS[LVMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *lvTestClient) waitFor(t *testing.T, msgType LVMessageType) LVMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m LVMessage) bool { return m.Type == msgType })
}

func lvPayloadMap(t *testing.T, msg LVMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// lvJoin 입장하고 lv_player_joined payload 를 돌려준다
func lvJoin(t *testing.T, c *lvTestClient, name, room string) map[string]interface{} {
	t.Helper()
	c.send(t, LVMessage{Type: LVMsgJoinGame, Payload: LVJoinGamePayload{Name: name, Room: room}})
	return lvPayloadMap(t, c.waitFor(t, LVMsgPlayerJoined))
}

// lvWaitPhase 지정한 phase 의 스냅샷이 올 때까지 소비
func (c *lvTestClient) lvWaitPhase(t *testing.T, phase string) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "lv_game_state("+phase+")", func(m LVMessage) bool {
		if m.Type != LVMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		return ok && state["phase"] == phase
	})
	return lvPayloadMap(t, msg)
}

// TestLVFourBotsCompleteGame 봇을 채운 4인 게임이 20초 안에 완주하는지 —
// 가장 중요한 회귀 장치 (교착·라운드 미전환·턴 미이동 감지).
// 좌석 0은 서버 연습봇 두뇌(lvBrain)를 WS 로 감싼 드라이버가 잡는다.
func TestLVFourBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newLVTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := lvDial(t, url)
	defer c.conn.Close()
	lvJoin(t, c, "감독", "")
	c.send(t, LVMessage{Type: LVMsgFillBots})

	start := time.Now()
	brain := newLVBrain()
	deadline := start.Add(20 * time.Second)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "state-or-over", func(m LVMessage) bool {
			return m.Type == LVMsgGameState || m.Type == LVMsgGameOver
		})
		if msg.Type == LVMsgGameOver {
			over := lvPayloadMap(t, msg)
			winner := int(over["winnerSeat"].(float64))
			if winner < 0 || winner >= LVMaxPlayers {
				t.Fatalf("winnerSeat = %d", winner)
			}
			tokens := over["tokens"].([]interface{})
			if len(tokens) != LVMaxPlayers {
				t.Fatalf("tokens 길이 = %d, want %d", len(tokens), LVMaxPlayers)
			}
			if got := int(tokens[winner].(float64)); got < 4 { // 4인전 목표 토큰
				t.Fatalf("승자 토큰 = %d, want >= 4", got)
			}
			t.Logf("완주: winner=seat%d tokens=%v (%.1fs)", winner, tokens, time.Since(start).Seconds())
			return
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatal("20초 안에 게임이 끝나지 않았다 — 진행 불가 상태")
}

// TestLVAfkAutoAdvance 접속만 유지한 채 아무것도 하지 않는 사람의 턴을
// AFK 타이머가 봇 두뇌로 자동 실행하는지 (endsAt 노출 포함)
func TestLVAfkAutoAdvance(t *testing.T) {
	_, url, cleanup := newLVTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := lvDial(t, url)
	defer host.conn.Close()
	lvJoin(t, host, "잠수1", "")

	guest := lvDial(t, url)
	defer guest.conn.Close()
	lvJoin(t, guest, "잠수2", "")

	host.send(t, LVMessage{Type: LVMsgStart})
	state := guest.lvWaitPhase(t, string(LVPhasePlaying))
	if ends := int64(state["endsAt"].(float64)); ends <= 0 {
		t.Fatalf("playing 스냅샷의 endsAt = %d, want unixMillis", ends)
	}

	// 전원 무행동 → AFK 이벤트(한글 문구)와 자동 실행된 플레이 확인
	afk := lvPayloadMap(t, guest.waitMatch(t, "afk-event", func(m LVMessage) bool {
		if m.Type != LVMsgEvent {
			return false
		}
		ev, ok := m.Payload.(map[string]interface{})
		return ok && ev["kind"] == "afk"
	}))
	if !strings.Contains(afk["message"].(string), "자동으로 진행") {
		t.Fatalf("AFK 문구 = %v", afk["message"])
	}
	if _, has := afk["name"]; !has {
		t.Fatalf("AFK 이벤트에 name 부재: %v", afk)
	}

	// 자동 실행 결과가 반영된 스냅샷 — 누군가의 버린 더미에 카드가 쌓였다
	guest.waitMatch(t, "auto-played state", func(m LVMessage) bool {
		if m.Type != LVMsgGameState {
			return false
		}
		state, ok := m.Payload.(map[string]interface{})
		if !ok {
			return false
		}
		total := 0
		for _, pRaw := range state["players"].([]interface{}) {
			p := pRaw.(map[string]interface{})
			total += len(p["discards"].([]interface{}))
		}
		return total >= 1
	})
}

// TestLVHiddenHands 스냅샷 은닉: 내 yourHand 만 실리고 (빈 배열도 []),
// players 항목에는 어떤 손패 필드도 없다. discards 는 전원 공개.
func TestLVHiddenHands(t *testing.T) {
	_, url, cleanup := newLVTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	clients := []*lvTestClient{}
	for i := 0; i < 3; i++ {
		c := lvDial(t, url)
		defer c.conn.Close()
		lvJoin(t, c, fmt.Sprintf("궁정%d", i), "")
		clients = append(clients, c)
	}
	clients[0].send(t, LVMessage{Type: LVMsgStart})

	for i, c := range clients {
		state := c.lvWaitPhase(t, string(LVPhasePlaying))
		if int(state["yourSeat"].(float64)) != i {
			t.Fatalf("클라 %d: yourSeat = %v", i, state["yourSeat"])
		}
		hand, has := state["yourHand"].([]interface{})
		if !has || len(hand) < 1 || len(hand) > 2 {
			t.Fatalf("클라 %d: yourHand = %v", i, state["yourHand"])
		}
		if _, ok := state["removedFaceUp"].([]interface{}); !ok {
			t.Fatalf("클라 %d: removedFaceUp 이 배열이 아니다 (nil?): %v", i, state["removedFaceUp"])
		}
		if tokens := state["tokens"].([]interface{}); len(tokens) != 3 {
			t.Fatalf("클라 %d: tokens = %v", i, state["tokens"])
		}
		players := state["players"].([]interface{})
		if len(players) != 3 {
			t.Fatalf("players %d명", len(players))
		}
		for j, pRaw := range players {
			p := pRaw.(map[string]interface{})
			for _, forbidden := range []string{"yourHand", "hand", "cards"} {
				if _, leaked := p[forbidden]; leaked {
					t.Fatalf("클라 %d: 남의 손패가 샜다 (%s): %v", i, forbidden, p)
				}
			}
			if int(p["seat"].(float64)) != j { // 좌석 0 유실 감지
				t.Fatalf("클라 %d: players[%d].seat = %v", i, j, p["seat"])
			}
			if _, ok := p["discards"].([]interface{}); !ok {
				t.Fatalf("클라 %d: discards 가 배열이 아니다 (nil?): %v", i, p["discards"])
			}
		}
	}
}

// TestLVPriestPrivateOnlyRequester 사제 열람 결과(lv_private)는 요청자에게만
// 간다. 허브 고루틴 없이 핸들러를 직접 불러 결정적으로 검증한다.
func TestLVPriestPrivateOnlyRequester(t *testing.T) {
	h := NewLVHub()
	room := h.lobbyRoomFor("")
	clients := make([]*LVClient, 3)
	for i := range clients {
		c := &LVClient{wsClient: newBotWSClient(), Hub: h}
		c.Bot = false // 소켓 없는 사람 취급 (Send 채널로 수신 검증)
		c.Name = fmt.Sprintf("P%d", i)
		seat, err := room.Game.AddPlayer(c.Name)
		if err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
		c.GameID = room.Game.ID
		c.Seat = seat
		room.Clients[seat] = c
		h.sessions[c.SessionID] = c
		clients[i] = c
	}
	h.startGame(room)

	game := room.Game
	cur := game.CurrentSeat
	tgt := (cur + 1) % 3
	game.Players[cur].Hand = []int{LVPriest, LVHandmaid}
	game.Players[tgt].Hand = []int{LVPrincess}

	for _, c := range clients { // 시작 메시지 비우기
		lvDrainMessages(c)
	}

	target := tgt
	h.handleGameMessage(LVGameMessage{Client: clients[cur], Message: LVMessage{
		Type:    LVMsgPlay,
		Payload: LVPlayPayload{Card: LVPriest, TargetSeat: &target},
	}})

	for i, c := range clients {
		privates := []LVPrivatePayload{}
		for _, m := range lvDrainMessages(c) {
			if m.Type != LVMsgPrivate {
				continue
			}
			raw, _ := json.Marshal(m.Payload)
			var p LVPrivatePayload
			json.Unmarshal(raw, &p)
			privates = append(privates, p)
		}
		if i == cur {
			if len(privates) != 1 || privates[0].Kind != "priest" {
				t.Fatalf("요청자 lv_private = %+v, want priest 1건", privates)
			}
			if !strings.Contains(privates[0].Message, "공주") {
				t.Fatalf("사제 열람 문구 = %q", privates[0].Message)
			}
		} else if len(privates) != 0 {
			t.Fatalf("seat%d(비요청자)에게 lv_private 유출: %+v", i, privates)
		}
	}
}

// lvDrainMessages 봇형 클라이언트의 Send 채널에 쌓인 메시지를 전부 꺼낸다
func lvDrainMessages(c *LVClient) []LVMessage {
	msgs := []LVMessage{}
	for {
		select {
		case data := <-c.Send:
			var m LVMessage
			if err := json.Unmarshal(data, &m); err == nil {
				msgs = append(msgs, m)
			}
		default:
			return msgs
		}
	}
}

// TestLVRoomCodeAndSpectate 방 코드 발급·코드 입장·시작 후 코드 관전 진입.
// 관전자 스냅샷은 yourSeat -1 에 yourHand 부재 (공개 정보만).
func TestLVRoomCodeAndSpectate(t *testing.T) {
	_, url, cleanup := newLVTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := lvDial(t, url)
	defer host.conn.Close()
	joined := lvJoin(t, host, "호스트", "NEW")
	code, _ := joined["roomCode"].(string)
	if len(code) != roomCodeLen {
		t.Fatalf("roomCode = %q, want %d자", code, roomCodeLen)
	}

	guest := lvDial(t, url)
	defer guest.conn.Close()
	guestJoined := lvJoin(t, guest, "친구", code)
	if guestJoined["roomCode"] != code || int(guestJoined["yourSeat"].(float64)) != 1 {
		t.Fatalf("코드 입장 실패: %v", guestJoined)
	}

	// 호스트가 봇을 채우면 4인이 되며 즉시 시작
	host.send(t, LVMessage{Type: LVMsgFillBots})
	state := host.lvWaitPhase(t, string(LVPhasePlaying))
	if state["roomCode"] != code || len(state["players"].([]interface{})) != LVMaxPlayers {
		t.Fatalf("봇 채움 시작 실패: %v", state["players"])
	}

	// 시작된 방의 코드로 들어오면 관전자
	spec := lvDial(t, url)
	defer spec.conn.Close()
	spec.send(t, LVMessage{Type: LVMsgJoinGame, Payload: LVJoinGamePayload{Name: "구경꾼", Room: code}})
	specJoined := lvPayloadMap(t, spec.waitFor(t, LVMsgSpectateJoined))
	if specJoined["roomCode"] != code {
		t.Fatalf("관전 입장 roomCode = %v", specJoined["roomCode"])
	}

	specState := lvPayloadMap(t, spec.waitFor(t, LVMsgGameState))
	if int(specState["yourSeat"].(float64)) != -1 {
		t.Fatalf("관전자 yourSeat = %v, want -1", specState["yourSeat"])
	}
	if _, leaked := specState["yourHand"]; leaked {
		t.Fatalf("관전자에게 yourHand 유출: %v", specState["yourHand"])
	}
	if len(specState["players"].([]interface{})) != LVMaxPlayers {
		t.Fatalf("관전자 스냅샷 players = %v", specState["players"])
	}

	// 관전자는 어떤 행동도 못 한다
	spec.send(t, LVMessage{Type: LVMsgPlay, Payload: LVPlayPayload{Card: 1}})
	errPayload := lvPayloadMap(t, spec.waitFor(t, LVMsgError))
	if errPayload["message"] != spectatorDeniedMsg {
		t.Fatalf("관전자 차단 문구 = %v", errPayload["message"])
	}
}
