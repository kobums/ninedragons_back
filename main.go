package main

import (
	"log"
	"net/http"
	"ninedragons/server"
)

func main() {
	hub := server.NewHub()
	go hub.Run()

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		server.ServeWs(hub, w, r)
	})

	log.Println("Server starting on :8003")
	if err := http.ListenAndServe(":8003", nil); err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}
