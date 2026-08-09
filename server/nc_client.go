package server

import (
	"log"
	"net/http"

	"github.com/google/uuid"
)

func ServeNCWs(hub *NCHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[NC] Error upgrading connection:", err)
		return
	}

	client := &NCClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[NC] ",
		func(msg NCMessage) { hub.gameMessage <- NCGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
