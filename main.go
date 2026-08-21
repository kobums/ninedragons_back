package main

import (
	"log"
	"net/http"
	"ninedragons/server"
	"os"
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
	csHub := server.NewCSHub()
	tcHub := server.NewTCHub()
	mtHub := server.NewMTHub()
	sfHub := server.NewSFHub()
	spHub := server.NewSPHub()
	avHub := server.NewAVHub()
	lvHub := server.NewLVHub()
	omHub := server.NewOMHub()
	skHub := server.NewSKHub()
	cnHub := server.NewCNHub()
	ytHub := server.NewYTHub()
	ipHub := server.NewIPHub()

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
		{"Can't Stop", "/ws/cantstop", csHub.Run,
			func(w http.ResponseWriter, r *http.Request) { server.ServeCSWs(csHub, w, r) }},
		{"Tichu", "/ws/tichu", tcHub.Run,
			func(w http.ResponseWriter, r *http.Request) { server.ServeTCWs(tcHub, w, r) }},
		{"Mighty", "/ws/mighty", mtHub.Run,
			func(w http.ResponseWriter, r *http.Request) { server.ServeMTWs(mtHub, w, r) }},
		{"Skyfall", "/ws/skyfall", sfHub.Run,
			func(w http.ResponseWriter, r *http.Request) { server.ServeSFWs(sfHub, w, r) }},
		{"Spyfall", "/ws/spyfall", spHub.Run,
			func(w http.ResponseWriter, r *http.Request) { server.ServeSPWs(spHub, w, r) }},
		{"Avalon", "/ws/avalon", avHub.Run,
			func(w http.ResponseWriter, r *http.Request) { server.ServeAVWs(avHub, w, r) }},
		{"Love Letter", "/ws/loveletter", lvHub.Run,
			func(w http.ResponseWriter, r *http.Request) { server.ServeLVWs(lvHub, w, r) }},
		{"Omok", "/ws/omok", omHub.Run,
			func(w http.ResponseWriter, r *http.Request) { server.ServeOMWs(omHub, w, r) }},
		{"Skull", "/ws/skull", skHub.Run,
			func(w http.ResponseWriter, r *http.Request) { server.ServeSKWs(skHub, w, r) }},
		{"Codenames", "/ws/codenames", cnHub.Run,
			func(w http.ResponseWriter, r *http.Request) { server.ServeCNWs(cnHub, w, r) }},
		{"Yacht Dice", "/ws/yacht", ytHub.Run,
			func(w http.ResponseWriter, r *http.Request) { server.ServeYTWs(ytHub, w, r) }},
		{"Indian Poker", "/ws/indianpoker", ipHub.Run,
			func(w http.ResponseWriter, r *http.Request) { server.ServeIPWs(ipHub, w, r) }},
	}

	// 전적 기록 (JSONL). 경로는 볼륨 마운트 지점 아래로.
	statsPath := os.Getenv("STATS_DB")
	if statsPath == "" {
		statsPath = "stats/matches.jsonl"
	}
	server.InitStats(statsPath)
	http.HandleFunc("/stats", server.StatsHandler)
	http.HandleFunc("/lobby", server.LobbyHandler)

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
