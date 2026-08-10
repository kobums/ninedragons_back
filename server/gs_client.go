package server

import (
	"log"
	"net/http"

	"github.com/google/uuid"
)

func ServeGSWs(hub *GSHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[GS] Error upgrading connection:", err)
		return
	}

	client := &GSClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[GS] ",
		func(msg GSMessage) { hub.gameMessage <- GSGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
