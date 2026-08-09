package server

import (
	"log"
	"net/http"

	"github.com/google/uuid"
)

func ServeJHWs(hub *JHHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[JH] Error upgrading connection:", err)
		return
	}

	client := &JHClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[JH] ",
		func(msg JHMessage) { hub.gameMessage <- JHGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
