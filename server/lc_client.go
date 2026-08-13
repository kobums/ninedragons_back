package server

import (
	"log"
	"net/http"

	"github.com/google/uuid"
)

func ServeLCWs(hub *LCHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[LC] Error upgrading connection:", err)
		return
	}

	client := &LCClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[LC] ",
		func(msg LCMessage) { hub.gameMessage <- LCGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
