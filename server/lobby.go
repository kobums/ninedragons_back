package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync"
)

// ==================== 대기 현황 ====================
//
// 게임 선택 화면에서 "지금 상대를 기다리는 사람이 있는 게임"을 보여주기 위한
// 가벼운 장부. 각 허브가 대기 슬롯(waitingRoom)을 만들거나 비울 때마다
// 기록한다 — 허브 고루틴에서만 갱신하고, HTTP 핸들러는 잠금 후 읽는다.
// 값 갱신은 멱등이라 정리 경로가 중복 호출해도 무해하다.

var (
	lobbyMu      sync.Mutex
	lobbyWaiting = map[string]bool{}
)

// lobbySetWaiting 게임의 대기자 유무 기록
func lobbySetWaiting(game string, waiting bool) {
	lobbyMu.Lock()
	lobbyWaiting[game] = waiting
	lobbyMu.Unlock()
}

// LobbyHandler GET /lobby — 대기자가 있는 게임 목록
func LobbyHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")

	waiting := []string{}
	lobbyMu.Lock()
	for game, isWaiting := range lobbyWaiting {
		if isWaiting {
			waiting = append(waiting, game)
		}
	}
	lobbyMu.Unlock()
	sort.Strings(waiting)

	json.NewEncoder(w).Encode(map[string][]string{"waiting": waiting})
}
