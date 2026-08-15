package server

import (
	"log"
	"net/http"

	"github.com/google/uuid"
)

func ServeMTWs(hub *MTHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[MT] Error upgrading connection:", err)
		return
	}

	client := &MTClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[MT] ",
		func(msg MTMessage) { hub.gameMessage <- MTGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
