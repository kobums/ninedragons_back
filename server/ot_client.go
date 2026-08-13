package server

import (
	"log"
	"net/http"

	"github.com/google/uuid"
)

func ServeOTWs(hub *OTHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[OT] Error upgrading connection:", err)
		return
	}

	client := &OTClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[OT] ",
		func(msg OTMessage) { hub.gameMessage <- OTGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
