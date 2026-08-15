package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// mtTestClient 공용 testConn 에 게임 메시지 타입의 waitFor 를 얹은 래퍼
type mtTestClient struct {
	testConn[MTMessage]
}

func newMTTestServer(t *testing.T, grace time.Duration) (*MTHub, string, func()) {
	t.Helper()
	hub := NewMTHub()
	hub.grace = grace
	go hub.Run()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeMTWs(hub, w, r)
	}))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	return hub, wsURL, ts.Close
}

func mtDial(t *testing.T, url string) *mtTestClient {
	t.Helper()
	return &mtTestClient{dialWS[MTMessage](t, url)}
}

// waitFor 지정한 타입의 메시지가 올 때까지 읽는다 (다른 타입은 건너뜀)
func (c *mtTestClient) waitFor(t *testing.T, msgType MTMessageType) MTMessage {
	t.Helper()
	return c.waitMatch(t, string(msgType), func(m MTMessage) bool { return m.Type == msgType })
}

// waitEvent 지정한 kind 의 mt_event 가 올 때까지 읽는다
func (c *mtTestClient) waitEvent(t *testing.T, kind string) map[string]interface{} {
	t.Helper()
	msg := c.waitMatch(t, "mt_event:"+kind, func(m MTMessage) bool {
		if m.Type != MTMsgEvent {
			return false
		}
		p, ok := m.Payload.(map[string]interface{})
		return ok && p["kind"] == kind
	})
	return asPayloadMap(t, msg.Payload)
}

func mtPayloadMap(t *testing.T, msg MTMessage) map[string]interface{} {
	t.Helper()
	return asPayloadMap(t, msg.Payload)
}

// mtJoin 입장 → mt_player_joined 의 (seat, sessionId)
func mtJoin(t *testing.T, c *mtTestClient, name string) (int, string) {
	t.Helper()
	c.send(t, MTMessage{Type: MTMsgJoinGame, Payload: MTJoinGamePayload{Name: name}})
	joined := mtPayloadMap(t, c.waitFor(t, MTMsgPlayerJoined))
	return int(joined["yourSeat"].(float64)), joined["sessionId"].(string)
}

// driveToGameOver 서버 연습봇 두뇌(mtBrain)로 소켓을 몰아 게임을 완주한다.
// 메시지는 testConn 큐로만 소비한다.
func (c *mtTestClient) driveToGameOver(t *testing.T, limit time.Duration) map[string]interface{} {
	t.Helper()
	brain := newMTBrain()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		msg := c.waitMatch(t, "게임 진행 메시지", func(MTMessage) bool { return true })
		if msg.Type == MTMsgGameOver {
			return mtPayloadMap(t, msg)
		}
		if reply := brain.decide(msg); reply != nil {
			c.send(t, *reply)
		}
	}
	t.Fatalf("%s 안에 game_over 를 받지 못했다 — 진행 불가 상태", limit)
	return nil
}

// TestMTBotsCompleteGame 사람 1명(봇 두뇌로 구동) + 연습봇 4명이 20초 안에 완주.
// 전원 패스 재배분이 나와도 강제 입찰 규칙으로 무한 재배분 없이 끝나야 한다.
// -count=3 으로 돌려 셔플 시드가 달라져도 회귀가 없음을 본다.
func TestMTBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newMTTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := mtDial(t, url)
	defer c.conn.Close()

	seat, _ := mtJoin(t, c, "드라이버")
	if seat != 0 {
		t.Fatalf("호스트 좌석 = %d, want 0", seat)
	}
	c.send(t, MTMessage{Type: MTMsgFillBots, Payload: map[string]interface{}{}})

	result := c.driveToGameOver(t, 20*time.Second)

	// 결과 페이로드: 양팀 점수·공약·프렌드 공개
	declPoints := int(result["declarerPoints"].(float64))
	defPoints := int(result["defenderPoints"].(float64))
	if declPoints+defPoints != MTScoreTotal {
		t.Fatalf("점수 합 %d+%d != %d", declPoints, defPoints, MTScoreTotal)
	}
	contract := result["contract"].(map[string]interface{})
	if int(contract["count"].(float64)) < MTMinBidNT {
		t.Fatalf("공약 이상: %v", contract)
	}
	declTeam := result["declarerTeam"].([]interface{})
	defTeam := result["defenderTeam"].([]interface{})
	if len(declTeam)+len(defTeam) != MTPlayerCount {
		t.Fatalf("팀 구성 이상: decl=%v def=%v", declTeam, defTeam)
	}
	if _, ok := result["win"].(bool); !ok {
		t.Fatalf("win 필드 없음: %v", result)
	}
}

// TestMTHiddenStateAndRejoin 은닉(남의 손패는 개수만·키티 미노출)과
// 재접속 스냅샷 복원을 검증한다
func TestMTHiddenStateAndRejoin(t *testing.T) {
	_, url, cleanup := newMTTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := mtDial(t, url)
	defer host.conn.Close()
	other := mtDial(t, url)

	mtJoin(t, host, "호스트")
	otherSeat, otherSession := mtJoin(t, other, "손님")
	if otherSeat != 1 {
		t.Fatalf("손님 좌석 = %d, want 1", otherSeat)
	}

	// host 가 봇 3명을 채워 게임 시작 (선입찰 seat0 = host 사람이라 입찰 대기로 멈춘다)
	host.send(t, MTMessage{Type: MTMsgFillBots, Payload: map[string]interface{}{}})

	state := mtPayloadMap(t, host.waitMatch(t, "bidding state", func(m MTMessage) bool {
		if m.Type != MTMsgGameState {
			return false
		}
		p, ok := m.Payload.(map[string]interface{})
		return ok && p["phase"] == string(MTPhaseBidding)
	}))

	// 은닉: 내 손패는 10장, 남의 손패는 개수만
	if got := len(state["yourHand"].([]interface{})); got != MTHandSize {
		t.Fatalf("내 손패 %d장, want %d", got, MTHandSize)
	}
	players := state["players"].([]interface{})
	if len(players) != MTPlayerCount {
		t.Fatalf("플레이어 %d명", len(players))
	}
	for i, p := range players {
		pm := p.(map[string]interface{})
		if int(pm["handCount"].(float64)) != MTHandSize {
			t.Fatalf("seat%d handCount = %v", i, pm["handCount"])
		}
		if _, leaked := pm["hand"]; leaked {
			t.Fatalf("seat%d 손패가 노출됐다", i)
		}
	}
	// 키티는 kitty 단계의 주공에게만 — 입찰 중엔 아무에게도 없다
	if state["yourKitty"] != nil {
		t.Fatalf("입찰 중 키티 노출: %v", state["yourKitty"])
	}

	// 손님 연결 끊김 → host 에 통지 → 재접속 → 복원 스냅샷
	other.conn.Close()
	disc := mtPayloadMap(t, host.waitFor(t, MTMsgOpponentDisconnected))
	if int(disc["seat"].(float64)) != 1 {
		t.Fatalf("끊김 좌석 = %v", disc["seat"])
	}

	rejoined := mtDial(t, url)
	defer rejoined.conn.Close()
	rejoined.send(t, MTMessage{Type: MTMsgRejoin, Payload: MTRejoinPayload{SessionID: otherSession}})
	recon := mtPayloadMap(t, host.waitFor(t, MTMsgReconnected))
	if int(recon["seat"].(float64)) != 1 {
		t.Fatalf("재접속 좌석 = %v", recon["seat"])
	}

	restored := mtPayloadMap(t, rejoined.waitFor(t, MTMsgGameState))
	if restored["phase"] != string(MTPhaseBidding) {
		t.Fatalf("복원 phase = %v", restored["phase"])
	}
	if int(restored["yourSeat"].(float64)) != 1 {
		t.Fatalf("복원 좌석 = %v", restored["yourSeat"])
	}
	if got := len(restored["yourHand"].([]interface{})); got != MTHandSize {
		t.Fatalf("복원 손패 %d장", got)
	}
}

// TestMTGraceExpiredBotTakeover 유예 만료 좌석은 연습봇으로 대체되고
// 게임은 끝까지 진행된다
func TestMTGraceExpiredBotTakeover(t *testing.T) {
	_, url, cleanup := newMTTestServer(t, 150*time.Millisecond)
	defer cleanup()

	host := mtDial(t, url)
	defer host.conn.Close()
	other := mtDial(t, url)

	mtJoin(t, host, "호스트")
	mtJoin(t, other, "이탈자")
	host.send(t, MTMessage{Type: MTMsgFillBots, Payload: map[string]interface{}{}})

	// 게임 시작 스냅샷 확인 후 손님 이탈
	host.waitMatch(t, "bidding state", func(m MTMessage) bool {
		if m.Type != MTMsgGameState {
			return false
		}
		p, ok := m.Payload.(map[string]interface{})
		return ok && p["phase"] == string(MTPhaseBidding)
	})
	other.conn.Close()

	// host 는 봇 두뇌로 완주하면서 bot_takeover 이벤트를 확인한다
	brain := newMTBrain()
	deadline := time.Now().Add(20 * time.Second)
	sawTakeover := false
	for time.Now().Before(deadline) {
		msg := host.waitMatch(t, "게임 진행 메시지", func(MTMessage) bool { return true })
		if msg.Type == MTMsgEvent {
			if p, ok := msg.Payload.(map[string]interface{}); ok && p["kind"] == "bot_takeover" {
				if int(p["seat"].(float64)) != 1 {
					t.Fatalf("봇 대체 좌석 = %v", p["seat"])
				}
				sawTakeover = true
			}
		}
		if msg.Type == MTMsgGameState && sawTakeover {
			// 대체 좌석은 이름 유지 + bot 표기
			players := mtPayloadMap(t, msg)["players"].([]interface{})
			pm := players[1].(map[string]interface{})
			if pm["bot"] != true || pm["name"] != "이탈자" {
				t.Fatalf("봇 대체 표기 이상: %v", pm)
			}
		}
		if msg.Type == MTMsgGameOver {
			if !sawTakeover {
				t.Fatal("bot_takeover 이벤트 없이 게임이 끝났다")
			}
			return
		}
		if reply := brain.decide(msg); reply != nil {
			host.send(t, *reply)
		}
	}
	t.Fatal("20초 안에 game_over 를 받지 못했다")
}
