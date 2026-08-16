package server

import (
	"log"
	"net/http"

	"github.com/google/uuid"
)

func ServeSFWs(hub *SFHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[SF] Error upgrading connection:", err)
		return
	}

	client := &SFClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[SF] ",
		func(msg SFMessage) { hub.gameMessage <- SFGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
