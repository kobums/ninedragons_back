package server

import (
	"log"
	"net/http"

	"github.com/google/uuid"
)

func ServeQDWs(hub *QDHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[QD] Error upgrading connection:", err)
		return
	}

	client := &QDClient{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "[QD] ",
		func(msg QDMessage) { hub.gameMessage <- QDGameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
