package server

import (
	"log"
	"net/http"

	"github.com/google/uuid"
)

func ServeWs(hub *Hub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}

	client := &Client{
		wsClient: newWSClient(uuid.New().String(), conn),
		Hub:      hub,
	}

	client.Hub.register <- client

	go writeLoop(conn, client.Send)
	go readLoop(conn, "",
		func(msg Message) { hub.gameMessage <- GameMessage{Client: client, Message: msg} },
		func() { hub.unregister <- client })
}
