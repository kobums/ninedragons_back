package main

import (
	"log"
	"net/http"
	"ninedragons/server"
)

func main() {
	// 게임 하나 = 허브 하나 + WS 엔드포인트 하나. 새 게임은 여기에
	// 엔트리 하나만 추가하면 된다.
	hub := server.NewHub()
	ncHub := server.NewNCHub()
	dvHub := server.NewDVHub()
	stHub := server.NewSTHub()
	jhHub := server.NewJHHub()
	gsHub := server.NewGSHub()
	qdHub := server.NewQDHub()
	otHub := server.NewOTHub()
	lcHub := server.NewLCHub()

	endpoints := []struct {
		name    string
		path    string
		run     func()
		handler http.HandlerFunc
	}{
		{"Nine Dragons", "/ws", hub.Run,
			func(w http.ResponseWriter, r *http.Request) { server.ServeWs(hub, w, r) }},
		{"Number Change", "/ws/numberchange", ncHub.Run,
			func(w http.ResponseWriter, r *http.Request) { server.ServeNCWs(ncHub, w, r) }},
		{"DaVinci Code", "/ws/davinci", dvHub.Run,
			func(w http.ResponseWriter, r *http.Request) { server.ServeDVWs(dvHub, w, r) }},
		{"Schotten Totten", "/ws/schottentotten", stHub.Run,
			func(w http.ResponseWriter, r *http.Request) { server.ServeSTWs(stHub, w, r) }},
		{"Jekyll vs Hyde", "/ws/jekyllhyde", jhHub.Run,
			func(w http.ResponseWriter, r *http.Request) { server.ServeJHWs(jhHub, w, r) }},
		{"Geister", "/ws/geister", gsHub.Run,
			func(w http.ResponseWriter, r *http.Request) { server.ServeGSWs(gsHub, w, r) }},
		{"Quoridor", "/ws/quoridor", qdHub.Run,
			func(w http.ResponseWriter, r *http.Request) { server.ServeQDWs(qdHub, w, r) }},
		{"Onitama", "/ws/onitama", otHub.Run,
			func(w http.ResponseWriter, r *http.Request) { server.ServeOTWs(otHub, w, r) }},
		{"Lost Cities", "/ws/lostcities", lcHub.Run,
			func(w http.ResponseWriter, r *http.Request) { server.ServeLCWs(lcHub, w, r) }},
	}

	log.Println("Server starting on :8003")
	for _, ep := range endpoints {
		go ep.run()
		http.HandleFunc(ep.path, ep.handler)
		log.Printf("  - %s: %s", ep.name, ep.path)
	}

	if err := http.ListenAndServe(":8003", nil); err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}
