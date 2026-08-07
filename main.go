package main

import (
	"log"
	"net/http"
	"ninedragons/server"
)

func main() {
	// 구룡투 게임 허브
	hub := server.NewHub()
	go hub.Run()

	// 넘버체인지 게임 허브
	ncHub := server.NewNCHub()
	go ncHub.Run()

	// 다빈치코드 게임 허브
	dvHub := server.NewDVHub()
	go dvHub.Run()

	// 쇼텐토텐 게임 허브
	stHub := server.NewSTHub()
	go stHub.Run()

	// 지킬앤하이드 게임 허브
	jhHub := server.NewJHHub()
	go jhHub.Run()

	// 구룡투 WebSocket 엔드포인트
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		server.ServeWs(hub, w, r)
	})

	// 넘버체인지 WebSocket 엔드포인트
	http.HandleFunc("/ws/numberchange", func(w http.ResponseWriter, r *http.Request) {
		server.ServeNCWs(ncHub, w, r)
	})

	// 다빈치코드 WebSocket 엔드포인트
	http.HandleFunc("/ws/davinci", func(w http.ResponseWriter, r *http.Request) {
		server.ServeDVWs(dvHub, w, r)
	})

	// 쇼텐토텐 WebSocket 엔드포인트
	http.HandleFunc("/ws/schottentotten", func(w http.ResponseWriter, r *http.Request) {
		server.ServeSTWs(stHub, w, r)
	})

	// 지킬앤하이드 WebSocket 엔드포인트
	http.HandleFunc("/ws/jekyllhyde", func(w http.ResponseWriter, r *http.Request) {
		server.ServeJHWs(jhHub, w, r)
	})

	log.Println("Server starting on :8003")
	log.Println("  - Nine Dragons: /ws")
	log.Println("  - Number Change: /ws/numberchange")
	log.Println("  - DaVinci Code: /ws/davinci")
	log.Println("  - Schotten Totten: /ws/schottentotten")
	log.Println("  - Jekyll vs Hyde: /ws/jekyllhyde")
	if err := http.ListenAndServe(":8003", nil); err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}
