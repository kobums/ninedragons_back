package server

import (
	"log"
	"net/http"

	"github.com/google/uuid"
)

func ServeAVWs(hub *AVHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[AV] Error upgrading connection:", err)
		return
	}

	client := &AVClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
		Seat:     -1,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[AV] ",
		func(msg AVMessage) { hub.gameMessage <- AVGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
