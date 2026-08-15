package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func lobbyList(t *testing.T) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	LobbyHandler(rec, httptest.NewRequest("GET", "/lobby", nil))
	var resp struct {
		Waiting []string `json:"waiting"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Waiting
}

func lobbyHas(list []string, game string) bool {
	for _, g := range list {
		if g == game {
			return true
		}
	}
	return false
}

// TestLobbyWaiting 대기자 입장 시 /lobby 에 나타나고 매칭되면 사라진다
func TestLobbyWaiting(t *testing.T) {
	_, url, cleanup := newQDTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c1 := qdDial(t, url)
	defer c1.conn.Close()
	c1.send(t, QDMessage{Type: QDMsgJoinGame, Payload: QDJoinGamePayload{PlayerName: "대기자"}})
	c1.waitFor(t, QDMsgPlayerJoined)

	if !lobbyHas(lobbyList(t), "quoridor") {
		t.Fatal("대기자가 있는데 /lobby 에 quoridor 가 없다")
	}

	c2 := qdDial(t, url)
	defer c2.conn.Close()
	c2.send(t, QDMessage{Type: QDMsgJoinGame, Payload: QDJoinGamePayload{PlayerName: "합류자"}})
	c2.waitFor(t, QDMsgPlayerJoined)

	if lobbyHas(lobbyList(t), "quoridor") {
		t.Fatal("매칭됐는데 /lobby 에 quoridor 가 남아 있다")
	}
}

// TestLobbyBotGameNotWaiting 봇전은 대기 슬롯을 쓰지 않으므로 표시되지 않는다
func TestLobbyBotGameNotWaiting(t *testing.T) {
	_, url, cleanup := newOTTestServer(t, defaultDisconnectGrace)
	defer cleanup()

	c := otDial(t, url)
	defer c.conn.Close()
	c.send(t, OTMessage{Type: OTMsgJoinGame, Payload: OTJoinGamePayload{PlayerName: "사람", VsBot: true}})
	c.waitFor(t, OTMsgPlayerJoined)

	if lobbyHas(lobbyList(t), "onitama") {
		t.Fatal("봇전인데 /lobby 에 onitama 가 있다")
	}
}
