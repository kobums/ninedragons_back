package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// statsWait 집계가 기대치에 도달할 때까지 폴링 (기록은 비동기)
func statsWait(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		stats.mu.Lock()
		total := stats.total
		stats.mu.Unlock()
		if total == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("집계가 %d에 도달하지 않았다", want)
}

func TestStatsRecordAndRestore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "matches.jsonl")

	InitStats(path)
	defer func() { stats = nil }()

	RecordMatch(MatchRecord{Game: "quoridor", Players: "가 vs 나", Winner: "가", Reason: "reach_goal", Duration: 42})
	RecordMatch(MatchRecord{Game: "quoridor", Players: "가 vs 나", Winner: "나", Reason: "reach_goal", Duration: 30, Bot: true})
	RecordMatch(MatchRecord{Game: "lostcities", Players: "가 vs 나", Winner: "", Reason: "score", Duration: 100})
	statsWait(t, 3)

	// 핸들러 응답 검증
	rec := httptest.NewRecorder()
	StatsHandler(rec, httptest.NewRequest("GET", "/stats", nil))
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("CORS 헤더가 없다")
	}
	var resp statsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Total != 3 {
		t.Fatalf("total = %d, want 3", resp.Total)
	}
	if len(resp.PerGame) != 2 || resp.PerGame[0].Game != "quoridor" || resp.PerGame[0].Count != 2 {
		t.Fatalf("perGame 이상: %v", resp.PerGame)
	}
	if len(resp.Recent) != 3 || resp.Recent[0].Game != "lostcities" {
		t.Fatalf("recent 순서 이상: %v", resp.Recent)
	}
	if resp.Recent[0].Winner != "" {
		t.Error("무승부 winner 가 비어 있지 않다")
	}

	// 재시작 복원: 새 스토어가 파일에서 집계를 되살린다
	InitStats(path)
	stats.mu.Lock()
	restored := stats.total
	stats.mu.Unlock()
	if restored != 3 {
		t.Fatalf("복원 total = %d, want 3", restored)
	}

	// 파일 내용도 JSONL 3줄
	data, _ := os.ReadFile(path)
	lines := 0
	for _, b := range data {
		if b == '\n' {
			lines++
		}
	}
	if lines != 3 {
		t.Fatalf("파일 %d줄, want 3", lines)
	}
}

func TestStatsDisabledSafe(t *testing.T) {
	stats = nil
	// 비활성 상태에서도 기록·조회가 안전해야 한다
	RecordMatch(MatchRecord{Game: "quoridor"})
	rec := httptest.NewRecorder()
	StatsHandler(rec, httptest.NewRequest("GET", "/stats", nil))
	var resp statsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Total != 0 {
		t.Fatalf("total = %d, want 0", resp.Total)
	}
}
