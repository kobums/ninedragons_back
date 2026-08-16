package server

import (
	"log"
	"net/http"

	"github.com/google/uuid"
)

func ServeSPWs(hub *SPHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[SP] Error upgrading connection:", err)
		return
	}

	client := &SPClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[SP] ",
		func(msg SPMessage) { hub.gameMessage <- SPGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
