package server

import (
	"fmt"
	"testing"
	"time"
)

// TestDVFillBotsCompleteGame 1인 + dv_fill_bots → 연습봇 3이 채워지며 자동
// 시작되고, 사람(기존 완주 드라이버)이 서버 봇들과 끝까지 완주하는지
func TestDVFillBotsCompleteGame(t *testing.T) {
	_, url, cleanup := newDVTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := dvDial(t, url)
	defer c.conn.Close()
	joinDVLobby(t, c, "사람")
	c.send(t, DVMessage{Type: DVMsgFillBots})

	// 봇이 채워진 로비 스냅샷 — 정원 4와 연습봇 이름 결(연습봇1..3) 확인
	lobby := dvPayloadMap(t, c.waitFor(t, DVMsgLobbyState))
	players := lobby["players"].([]interface{})
	if len(players) != DVMaxPlayers {
		t.Fatalf("로비 인원 = %d, want %d", len(players), DVMaxPlayers)
	}
	for i, p := range players[1:] {
		name := p.(map[string]interface{})["name"].(string)
		if want := fmt.Sprintf("%s%d", botName, i+1); name != want {
			t.Fatalf("players[%d].name = %s, want %s", i+1, name, want)
		}
	}

	// 4좌석이 차며 자동 시작 → 드라이버(서버 봇과 같은 두뇌)로 완주.
	// 초기 스냅샷을 놓쳐도 봇들의 시작 타일 픽이 계속 새 스냅샷을 만들므로
	// 드라이버는 소켓에서 자연히 따라잡는다.
	done := make(chan int, 1)
	driver := &dvBot{conn: c.conn, brain: newDVBrain(), done: done}
	go driver.run(nil)

	select {
	case winner := <-done:
		if winner < 0 || winner >= DVMaxPlayers {
			t.Fatalf("winnerSeat = %d, want 0~%d", winner, DVMaxPlayers-1)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("20초 안에 게임이 끝나지 않았다 — 봇 게임 어딘가에서 멈춤")
	}
}

// TestDVFillBotsPermissions 호스트가 아니면 거부, 게임 시작 후에도 거부
func TestDVFillBotsPermissions(t *testing.T) {
	_, url, cleanup := newDVTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	host := dvDial(t, url)
	defer host.conn.Close()
	joinDVLobby(t, host, "호스트")

	guest := dvDial(t, url)
	defer guest.conn.Close()
	joinDVLobby(t, guest, "손님")

	// 호스트(최저 좌석)가 아닌 요청은 거부
	guest.send(t, DVMessage{Type: DVMsgFillBots})
	errPayload := dvPayloadMap(t, guest.waitFor(t, DVMsgError))
	if errPayload["message"] != "호스트만 봇을 채울 수 있습니다" {
		t.Fatalf("비호스트 fill_bots 에러 = %v", errPayload["message"])
	}

	// 호스트 요청 → 빈 좌석 2개가 봇으로 채워지며 자동 시작
	host.send(t, DVMessage{Type: DVMsgFillBots})
	host.waitFor(t, DVMsgGameState)

	// 시작 후에는 호스트라도 거부 (시작 전 방이 아니다)
	host.send(t, DVMessage{Type: DVMsgFillBots})
	errPayload = dvPayloadMap(t, host.waitFor(t, DVMsgError))
	if errPayload["message"] != "로비를 찾을 수 없습니다" {
		t.Fatalf("시작 후 fill_bots 에러 = %v", errPayload["message"])
	}
}
