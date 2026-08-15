package server

import (
	"bufio"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
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

const statsRecentKeep = 20

type statsStore struct {
	mu      sync.Mutex
	path    string
	total   int
	perGame map[string]int
	recent  []MatchRecord // 최신이 앞
	ch      chan MatchRecord
}

var stats *statsStore

// InitStats 기록 파일을 읽어 집계를 복원하고 기록 고루틴을 시작한다
func InitStats(path string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Printf("[stats] 기록 디렉터리 생성 실패: %v — 전적 기록 비활성", err)
		return
	}
	s := &statsStore{
		path:    path,
		perGame: map[string]int{},
		ch:      make(chan MatchRecord, 64),
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
	s.recent = append([]MatchRecord{rec}, s.recent...)
	if len(s.recent) > statsRecentKeep {
		s.recent = s.recent[:statsRecentKeep]
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

type statsResponse struct {
	Total   int              `json:"total"`
	PerGame []statsGameCount `json:"perGame"`
	Recent  []MatchRecord    `json:"recent"`
}

// StatsHandler GET /stats — 총 판수·게임별 판수·최근 경기.
// 프론트(ninedragons.gowoobro.com)와 API 도메인이 달라 CORS 를 연다.
func StatsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")

	resp := statsResponse{PerGame: []statsGameCount{}, Recent: []MatchRecord{}}
	if stats != nil {
		stats.mu.Lock()
		resp.Total = stats.total
		for game, count := range stats.perGame {
			resp.PerGame = append(resp.PerGame, statsGameCount{Game: game, Count: count})
		}
		resp.Recent = append(resp.Recent, stats.recent...)
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
