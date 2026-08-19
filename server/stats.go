package server

import (
	"bufio"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ==================== 전적 기록 ====================
//
// 경기 결과를 JSONL 파일에 append 하고 메모리 집계(총 판수·게임별·최근 N건)를
// 유지한다. 요구가 단순해 DB 를 두지 않는다 — 파일 한 줄 = 경기 한 판이라
// 재시작 시 파일을 다시 읽어 집계를 복원한다. 기록은 전용 고루틴이 처리하므로
// 허브 고루틴은 채널에 넣기만 한다 (가득 차면 버린다 — 전적은 재미 요소다).

// MatchRecord 경기 한 판의 기록
type MatchRecord struct {
	Game     string    `json:"game"`
	Players  string    `json:"players"`
	Winner   string    `json:"winner"` // 승자 닉네임, "" 는 무승부
	Reason   string    `json:"reason"`
	Duration int       `json:"durationSec"`
	Bot      bool      `json:"bot"` // 연습봇전 여부
	PlayedAt time.Time `json:"playedAt"`
}

const (
	statsRecentKeep       = 20 // /stats recent 보관 수
	statsTopPlayers       = 50 // /stats players 상위 노출 수
	statsPlayerRecentKeep = 20 // playerDetail recent 보관 수
)

// playerAgg 플레이어 한 명의 누적 성적
type playerAgg struct {
	Plays int
	Wins  int
	Draws int
}

type statsStore struct {
	mu        sync.Mutex
	path      string
	total     int
	perGame   map[string]int
	recent    []MatchRecord // 최신이 앞
	records   []MatchRecord // 전체 기록, 오래된 것이 앞 (개인 규모라 전량 메모리)
	perPlayer map[string]*playerAgg
	ch        chan MatchRecord
}

// ==================== Players/Winner 파싱 ====================
//
// 14개 허브의 RecordMatch 호출부에서 확인한 실제 형식:
//   - 1:1        "A vs B"           (구룡투·지킬·로스트시티·가이스터·캔트스톱·
//                                    쿼리도·쇼텐토텐·오니타마·넘버체인지)
//   - 팀·진영전  "A·B vs C·D"       (티츄·마이티·아발론·스파이폴·스카이폴 —
//                                    팀원은 "·" 연결, Winner 도 "A·B" 형태)
//   - 다빈치     "A vs B vs C"      (전원 " vs " 연결, Winner 는 단일)
//   - 무승부     Winner == ""
// 진영 구분자는 " vs ", 진영 내 팀원 구분자는 "·" 다.

// splitPlayers Players 문자열을 개별 닉네임 목록으로 분해한다 (중복 제거)
func splitPlayers(players string) []string {
	var names []string
	seen := map[string]bool{}
	for _, side := range strings.Split(players, " vs ") {
		for _, name := range strings.Split(side, "·") {
			name = strings.TrimSpace(name)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}

// splitWinners Winner 문자열을 승자 닉네임 목록으로 분해한다 ("" 는 무승부 → nil)
func splitWinners(winner string) []string {
	if winner == "" {
		return nil
	}
	var names []string
	for _, name := range strings.Split(winner, "·") {
		name = strings.TrimSpace(name)
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

// isBotName 연습봇 닉네임 여부. 봇 이름은 bot.go 의 botName("연습봇") 그대로
// 또는 "연습봇1"처럼 번호가 붙는다 (av/tc/sp/mt/sf 허브의 spawnBot 확인).
func isBotName(name string) bool {
	return strings.HasPrefix(name, botName)
}

var stats *statsStore

// InitStats 기록 파일을 읽어 집계를 복원하고 기록 고루틴을 시작한다
func InitStats(path string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Printf("[stats] 기록 디렉터리 생성 실패: %v — 전적 기록 비활성", err)
		return
	}
	s := &statsStore{
		path:      path,
		perGame:   map[string]int{},
		perPlayer: map[string]*playerAgg{},
		ch:        make(chan MatchRecord, 64),
	}
	s.load()
	go s.run()
	stats = s
	log.Printf("[stats] 전적 기록 활성 (%s, 누적 %d판)", path, s.total)
}

// load 기존 JSONL 을 읽어 집계 복원 (깨진 줄은 건너뛴다)
func (s *statsStore) load() {
	f, err := os.Open(s.path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var rec MatchRecord
		if json.Unmarshal(scanner.Bytes(), &rec) != nil {
			continue
		}
		s.apply(rec)
	}
}

// apply 집계 반영 (mu 잠금은 호출자 책임 — load 시점엔 단일 고루틴)
func (s *statsStore) apply(rec MatchRecord) {
	s.total++
	s.perGame[rec.Game]++
	s.records = append(s.records, rec)
	s.recent = append([]MatchRecord{rec}, s.recent...)
	if len(s.recent) > statsRecentKeep {
		s.recent = s.recent[:statsRecentKeep]
	}

	// 플레이어 집계 (연습봇 제외)
	winners := map[string]bool{}
	for _, name := range splitWinners(rec.Winner) {
		winners[name] = true
	}
	draw := rec.Winner == ""
	for _, name := range splitPlayers(rec.Players) {
		if isBotName(name) {
			continue
		}
		agg := s.perPlayer[name]
		if agg == nil {
			agg = &playerAgg{}
			s.perPlayer[name] = agg
		}
		agg.Plays++
		switch {
		case draw:
			agg.Draws++
		case winners[name]:
			agg.Wins++
		}
	}
}

// run 기록 고루틴: 파일 append + 집계 갱신
func (s *statsStore) run() {
	for rec := range s.ch {
		data, err := json.Marshal(rec)
		if err != nil {
			continue
		}
		f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			log.Printf("[stats] 기록 실패: %v", err)
			continue
		}
		f.Write(append(data, '\n'))
		f.Close()

		s.mu.Lock()
		s.apply(rec)
		s.mu.Unlock()
	}
}

// RecordMatch 경기 결과 기록 (허브 고루틴에서 호출 — 논블로킹)
func RecordMatch(rec MatchRecord) {
	if stats == nil {
		return
	}
	rec.PlayedAt = time.Now()
	select {
	case stats.ch <- rec:
	default:
		log.Printf("[stats] 기록 버림 (큐 가득)")
	}
}

// matchSeconds 경기 소요 초
func matchSeconds(startedAt time.Time) int {
	if startedAt.IsZero() {
		return 0
	}
	return int(time.Since(startedAt).Seconds())
}

// ==================== /stats 응답 ====================

type statsGameCount struct {
	Game  string `json:"game"`
	Count int    `json:"count"`
}

type statsPlayer struct {
	Name  string `json:"name"`
	Plays int    `json:"plays"`
	Wins  int    `json:"wins"`
	Draws int    `json:"draws"`
}

type statsPlayerGame struct {
	Game  string `json:"game"`
	Plays int    `json:"plays"`
	Wins  int    `json:"wins"`
}

type statsPlayerDetail struct {
	Name    string            `json:"name"`
	Plays   int               `json:"plays"`
	Wins    int               `json:"wins"`
	Draws   int               `json:"draws"`
	PerGame []statsPlayerGame `json:"perGame"`
	Recent  []MatchRecord     `json:"recent"`
}

type statsResponse struct {
	Total        int                `json:"total"`
	PerGame      []statsGameCount   `json:"perGame"`
	Recent       []MatchRecord      `json:"recent"`
	Players      []statsPlayer      `json:"players"`
	PlayerDetail *statsPlayerDetail `json:"playerDetail,omitempty"`
}

// topPlayers 판수순 상위 플레이어 (mu 잠금은 호출자 책임)
func (s *statsStore) topPlayers(limit int) []statsPlayer {
	players := []statsPlayer{}
	for name, agg := range s.perPlayer {
		players = append(players, statsPlayer{Name: name, Plays: agg.Plays, Wins: agg.Wins, Draws: agg.Draws})
	}
	sort.Slice(players, func(i, j int) bool {
		if players[i].Plays != players[j].Plays {
			return players[i].Plays > players[j].Plays
		}
		return players[i].Name < players[j].Name
	})
	if len(players) > limit {
		players = players[:limit]
	}
	return players
}

// playerDetail 특정 플레이어 상세 (mu 잠금은 호출자 책임).
// 미등록 닉네임(연습봇 포함)은 plays 0 인 빈 상세를 돌려준다 — 에러 아님.
func (s *statsStore) playerDetail(name string) *statsPlayerDetail {
	detail := &statsPlayerDetail{Name: name, PerGame: []statsPlayerGame{}, Recent: []MatchRecord{}}
	agg := s.perPlayer[name]
	if agg == nil {
		return detail
	}
	detail.Plays = agg.Plays
	detail.Wins = agg.Wins
	detail.Draws = agg.Draws

	// 전체 기록을 훑어 게임별 성적과 그 플레이어의 최근 경기를 모은다
	// (records 는 오래된 것이 앞 — 최근 경기는 뒤에서부터)
	perGame := map[string]*statsPlayerGame{}
	for i := len(s.records) - 1; i >= 0; i-- {
		rec := s.records[i]
		joined := false
		for _, p := range splitPlayers(rec.Players) {
			if p == name {
				joined = true
				break
			}
		}
		if !joined {
			continue
		}
		g := perGame[rec.Game]
		if g == nil {
			g = &statsPlayerGame{Game: rec.Game}
			perGame[rec.Game] = g
		}
		g.Plays++
		for _, w := range splitWinners(rec.Winner) {
			if w == name {
				g.Wins++
				break
			}
		}
		if len(detail.Recent) < statsPlayerRecentKeep {
			detail.Recent = append(detail.Recent, rec)
		}
	}
	for _, g := range perGame {
		detail.PerGame = append(detail.PerGame, *g)
	}
	sort.Slice(detail.PerGame, func(i, j int) bool {
		if detail.PerGame[i].Plays != detail.PerGame[j].Plays {
			return detail.PerGame[i].Plays > detail.PerGame[j].Plays
		}
		return detail.PerGame[i].Game < detail.PerGame[j].Game
	})
	return detail
}

// StatsHandler GET /stats — 총 판수·게임별 판수·최근 경기·플레이어 집계.
// ?player=NAME 이면 그 플레이어의 상세(playerDetail)를 함께 준다.
// 프론트(ninedragons.gowoobro.com)와 API 도메인이 달라 CORS 를 연다.
func StatsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")

	player := r.URL.Query().Get("player")
	resp := statsResponse{PerGame: []statsGameCount{}, Recent: []MatchRecord{}, Players: []statsPlayer{}}
	if player != "" {
		// 비활성 상태에서도 쿼리가 오면 빈 상세로 답한다
		resp.PlayerDetail = &statsPlayerDetail{Name: player, PerGame: []statsPlayerGame{}, Recent: []MatchRecord{}}
	}
	if stats != nil {
		stats.mu.Lock()
		resp.Total = stats.total
		for game, count := range stats.perGame {
			resp.PerGame = append(resp.PerGame, statsGameCount{Game: game, Count: count})
		}
		resp.Recent = append(resp.Recent, stats.recent...)
		resp.Players = stats.topPlayers(statsTopPlayers)
		if player != "" {
			resp.PlayerDetail = stats.playerDetail(player)
		}
		stats.mu.Unlock()
		sort.Slice(resp.PerGame, func(i, j int) bool {
			if resp.PerGame[i].Count != resp.PerGame[j].Count {
				return resp.PerGame[i].Count > resp.PerGame[j].Count
			}
			return resp.PerGame[i].Game < resp.PerGame[j].Game
		})
	}
	json.NewEncoder(w).Encode(resp)
}
