package server

import (
	"log"
	"net/http"

	"github.com/google/uuid"
)

// ServeTCWs /ws/tichu 웹소켓 연결 처리
func ServeTCWs(hub *TCHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[TC] Error upgrading connection:", err)
		return
	}

	client := &TCClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[TC] ",
		func(msg TCMessage) { hub.gameMessage <- TCGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
