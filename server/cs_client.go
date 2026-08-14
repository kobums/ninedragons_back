package server

import (
	"log"
	"net/http"

	"github.com/google/uuid"
)

func ServeCSWs(hub *CSHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[CS] Error upgrading connection:", err)
		return
	}

	client := &CSClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[CS] ",
		func(msg CSMessage) { hub.gameMessage <- CSGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
