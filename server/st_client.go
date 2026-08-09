package server

import (
	"log"
	"net/http"

	"github.com/google/uuid"
)

func ServeSTWs(hub *STHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[ST] Error upgrading connection:", err)
		return
	}

	client := &STClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[ST] ",
		func(msg STMessage) { hub.gameMessage <- STGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
