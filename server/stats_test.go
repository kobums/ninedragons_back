package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	StatsHandler(rec, httptest.NewRequest("GET", "/stats?player=가", nil))
	var resp statsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Total != 0 {
		t.Fatalf("total = %d, want 0", resp.Total)
	}
	// 빈 목록은 null 이 아니라 [] 로 나가야 한다 (프론트 반복 버그)
	raw := rec.Body.String()
	for _, want := range []string{`"players":[]`, `"perGame":[]`, `"recent":[]`} {
		if !strings.Contains(raw, want) {
			t.Errorf("응답에 %s 가 없다: %s", want, raw)
		}
	}
	if resp.PlayerDetail == nil || resp.PlayerDetail.Plays != 0 {
		t.Fatalf("비활성 상태 playerDetail 이상: %+v", resp.PlayerDetail)
	}
}

// TestSplitPlayersFormats 14개 허브에서 확인한 실제 Players/Winner 형식 파싱
func TestSplitPlayersFormats(t *testing.T) {
	cases := []struct {
		players string
		want    []string
	}{
		{"가 vs 나", []string{"가", "나"}},                     // 1:1 (쿼리도 등)
		{"가·나 vs 다·라", []string{"가", "나", "다", "라"}},       // 팀전 (티츄·마이티)
		{"가 vs 나 vs 다", []string{"가", "나", "다"}},          // 다빈치 (전원 vs 연결)
		{"가·나·다 vs 라·마", []string{"가", "나", "다", "라", "마"}}, // 진영전 (아발론·스파이폴·스카이폴)
		{"가 vs 가", []string{"가"}},                          // 동명 중복 제거
	}
	for _, c := range cases {
		got := splitPlayers(c.players)
		if len(got) != len(c.want) {
			t.Fatalf("splitPlayers(%q) = %v, want %v", c.players, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("splitPlayers(%q) = %v, want %v", c.players, got, c.want)
			}
		}
	}

	if w := splitWinners(""); w != nil {
		t.Fatalf("무승부 winners = %v, want nil", w)
	}
	if w := splitWinners("가·나"); len(w) != 2 || w[0] != "가" || w[1] != "나" {
		t.Fatalf("팀 winners = %v", w)
	}
	if w := splitWinners("가"); len(w) != 1 || w[0] != "가" {
		t.Fatalf("단일 winners = %v", w)
	}

	if !isBotName("연습봇") || !isBotName("연습봇3") {
		t.Error("연습봇 이름을 봇으로 판정하지 못했다")
	}
	if isBotName("가") {
		t.Error("사람 이름을 봇으로 판정했다")
	}
}

// TestStatsPlayersAggregation 플레이어 집계 — 단일 승자·팀 승자·무승부·봇 제외·
// 부분 문자열 오판 방지, 그리고 load 복원까지
func TestStatsPlayersAggregation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "matches.jsonl")

	InitStats(path)
	defer func() { stats = nil }()

	RecordMatch(MatchRecord{Game: "quoridor", Players: "가 vs 나", Winner: "가", Reason: "reach_goal", Duration: 42})
	RecordMatch(MatchRecord{Game: "tichu", Players: "가·나 vs 다·라", Winner: "가·나", Reason: "target", Duration: 300})
	RecordMatch(MatchRecord{Game: "lostcities", Players: "가 vs 다", Winner: "", Reason: "score", Duration: 100})
	RecordMatch(MatchRecord{Game: "quoridor", Players: "나 vs 연습봇", Winner: "연습봇", Reason: "reach_goal", Duration: 20, Bot: true})
	// Winner "나" 는 "가나" 의 부분 문자열 — 목록 일치로만 승리 판정해야 한다
	RecordMatch(MatchRecord{Game: "onitama", Players: "가나 vs 나", Winner: "나", Reason: "master", Duration: 50})
	statsWait(t, 5)

	rec := httptest.NewRecorder()
	StatsHandler(rec, httptest.NewRequest("GET", "/stats", nil))
	var resp statsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	byName := map[string]statsPlayer{}
	for _, p := range resp.Players {
		byName[p.Name] = p
	}
	if _, ok := byName["연습봇"]; ok {
		t.Error("연습봇이 플레이어 목록에 들어갔다")
	}
	if p := byName["가"]; p.Plays != 3 || p.Wins != 2 || p.Draws != 1 {
		t.Fatalf("가 = %+v, want plays 3 wins 2 draws 1", p)
	}
	if p := byName["나"]; p.Plays != 4 || p.Wins != 2 || p.Draws != 0 {
		t.Fatalf("나 = %+v, want plays 4 wins 2 draws 0", p)
	}
	if p := byName["가나"]; p.Plays != 1 || p.Wins != 0 {
		t.Fatalf("가나 = %+v, want plays 1 wins 0 (부분 문자열 오판)", p)
	}
	if p := byName["다"]; p.Plays != 2 || p.Wins != 0 || p.Draws != 1 {
		t.Fatalf("다 = %+v, want plays 2 wins 0 draws 1", p)
	}
	// 판수순 정렬 — 나(4판)가 맨 앞
	if len(resp.Players) == 0 || resp.Players[0].Name != "나" {
		t.Fatalf("players 정렬 이상: %+v", resp.Players)
	}

	// 재시작 복원: 파일에서 플레이어 집계가 되살아난다
	InitStats(path)
	stats.mu.Lock()
	restored := stats.perPlayer["가"]
	stats.mu.Unlock()
	if restored == nil || restored.Plays != 3 || restored.Wins != 2 || restored.Draws != 1 {
		t.Fatalf("복원된 가 = %+v, want plays 3 wins 2 draws 1", restored)
	}
}

// TestStatsPlayerQuery ?player= 상세 — 게임별 성적·본인 경기만·미존재 닉네임
func TestStatsPlayerQuery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "matches.jsonl")

	InitStats(path)
	defer func() { stats = nil }()

	RecordMatch(MatchRecord{Game: "quoridor", Players: "가 vs 나", Winner: "가", Reason: "reach_goal", Duration: 42})
	RecordMatch(MatchRecord{Game: "quoridor", Players: "가 vs 나", Winner: "나", Reason: "reach_goal", Duration: 30})
	RecordMatch(MatchRecord{Game: "tichu", Players: "가·다 vs 나·라", Winner: "가·다", Reason: "target", Duration: 300})
	RecordMatch(MatchRecord{Game: "geister", Players: "나 vs 다", Winner: "다", Reason: "escape", Duration: 60})
	statsWait(t, 4)

	rec := httptest.NewRecorder()
	StatsHandler(rec, httptest.NewRequest("GET", "/stats?player=가", nil))
	var resp statsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	d := resp.PlayerDetail
	if d == nil {
		t.Fatal("playerDetail 이 없다")
	}
	if d.Name != "가" || d.Plays != 3 || d.Wins != 2 || d.Draws != 0 {
		t.Fatalf("가 상세 = %+v, want plays 3 wins 2", d)
	}
	// 게임별 성적: quoridor 2판 1승, tichu 1판 1승 (판수순)
	if len(d.PerGame) != 2 || d.PerGame[0].Game != "quoridor" || d.PerGame[0].Plays != 2 || d.PerGame[0].Wins != 1 {
		t.Fatalf("perGame 이상: %+v", d.PerGame)
	}
	if d.PerGame[1].Game != "tichu" || d.PerGame[1].Plays != 1 || d.PerGame[1].Wins != 1 {
		t.Fatalf("perGame 이상: %+v", d.PerGame)
	}
	// 본인이 낀 판만 최신순 — geister(불참)는 빠지고 tichu 가 맨 앞
	if len(d.Recent) != 3 || d.Recent[0].Game != "tichu" || d.Recent[2].Game != "quoridor" {
		t.Fatalf("recent 이상: %+v", d.Recent)
	}
	for _, r := range d.Recent {
		if r.Game == "geister" {
			t.Fatal("불참 경기가 recent 에 들어갔다")
		}
	}
	// 기존 필드 하위 호환 유지
	if resp.Total != 4 || len(resp.Recent) != 4 {
		t.Fatalf("기존 필드 이상: total=%d recent=%d", resp.Total, len(resp.Recent))
	}

	// 미존재 닉네임 → 에러 아님, plays 0 + 빈 [] 목록
	rec = httptest.NewRecorder()
	StatsHandler(rec, httptest.NewRequest("GET", "/stats?player=없는사람", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp2 statsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp2); err != nil {
		t.Fatal(err)
	}
	d2 := resp2.PlayerDetail
	if d2 == nil || d2.Name != "없는사람" || d2.Plays != 0 || d2.Wins != 0 || d2.Draws != 0 {
		t.Fatalf("미존재 상세 이상: %+v", d2)
	}
	raw := rec.Body.String()
	if !strings.Contains(raw, `"perGame":[]`) || !strings.Contains(raw, `"recent":[]`) {
		t.Fatalf("미존재 상세의 빈 목록이 [] 가 아니다: %s", raw)
	}

	// player 없는 요청엔 playerDetail 자체가 없다
	rec = httptest.NewRecorder()
	StatsHandler(rec, httptest.NewRequest("GET", "/stats", nil))
	if strings.Contains(rec.Body.String(), "playerDetail") {
		t.Fatal("player 쿼리 없이 playerDetail 이 나갔다")
	}
}
