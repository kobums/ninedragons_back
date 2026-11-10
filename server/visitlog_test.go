package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const chromeMacUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.6478.127 Safari/537.36"

func stubGeo(t *testing.T, g geoInfo) *int {
	t.Helper()
	calls := 0
	orig := lookupGeo
	lookupGeo = func(ip string) geoInfo { calls++; return g }
	t.Cleanup(func() { lookupGeo = orig })
	return &calls
}

func TestBuildVisitRecord(t *testing.T) {
	calls := stubGeo(t, geoInfo{
		Country: "대한민국", Region: "서울특별시", City: "서울",
		ISP: "Korea Telecom", Org: "KT", AS: "AS4766",
	})

	rec := buildVisitRecord(visitReq{
		time:  time.Now(),
		ip:    "1.2.3.4",
		path:  "/ws/azul",
		agent: chromeMacUA,
		lang:  "ko-KR,ko;q=0.9",
	})

	if rec.OS != "macOS 10.15.7" || rec.Browser != "Chrome 126" || rec.Device != "데스크톱" {
		t.Errorf("UA 파싱 오류: os=%q browser=%q device=%q", rec.OS, rec.Browser, rec.Device)
	}
	if rec.Country != "대한민국" || rec.City != "서울" || rec.ISP != "Korea Telecom" || rec.AS != "AS4766" {
		t.Errorf("지오 정보 누락: %+v", rec)
	}
	if rec.Bot {
		t.Error("브라우저 UA 가 봇으로 판정됨")
	}
	if rec.Agent != chromeMacUA || rec.Lang != "ko-KR,ko;q=0.9" {
		t.Error("원문 UA·언어가 보존되어야 한다")
	}
	if *calls != 1 {
		t.Errorf("지오 조회는 한 번: %d", *calls)
	}
}

func TestBuildVisitRecordBotSkipsGeo(t *testing.T) {
	calls := stubGeo(t, geoInfo{Country: "미국"})

	rec := buildVisitRecord(visitReq{
		time:  time.Now(),
		ip:    "2.3.4.5",
		path:  "/lobby",
		agent: "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
	})

	if !rec.Bot {
		t.Error("Googlebot 은 봇으로 판정되어야 한다")
	}
	if *calls != 0 || rec.Country != "" {
		t.Errorf("봇은 지오 조회를 생략해야 한다: calls=%d country=%q", *calls, rec.Country)
	}
}

func TestVisitMessage(t *testing.T) {
	full := VisitRecord{
		IP: "1.2.3.4", Path: "/ws/azul",
		Country: "대한민국", Region: "서울특별시", City: "서울", ISP: "KT",
		Device: "데스크톱", OS: "macOS 10.15.7", Browser: "Chrome 126",
	}
	want := "1.2.3.4\n대한민국 · 서울특별시 서울 · KT\n데스크톱 · macOS 10.15.7 · Chrome 126\n/ws/azul"
	if got := visitMessage(full); got != want {
		t.Errorf("visitMessage 전체 = %q, want %q", got, want)
	}

	// 지오·UA 를 모르면 그 줄 자체가 빠져야 한다 (빈 "·" 줄 금지)
	bare := VisitRecord{IP: "1.2.3.4", Path: "/lobby"}
	if got := visitMessage(bare); got != "1.2.3.4\n/lobby" {
		t.Errorf("visitMessage 최소 = %q", got)
	}
}

func TestPruneVisitLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "visits.jsonl")
	now := time.Now()
	cutoff := now.Add(-visitRetention)

	line := func(ip string, at time.Time) string {
		data, err := json.Marshal(VisitRecord{Time: at, IP: ip, Path: "/lobby"})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	old1 := line("1.1.1.1", cutoff.Add(-time.Hour))
	old2 := line("2.2.2.2", cutoff.Add(-100*24*time.Hour))
	fresh1 := line("3.3.3.3", cutoff.Add(time.Hour))
	fresh2 := line("4.4.4.4", now)
	broken := `{"time":"깨진 줄`

	// 마지막 줄은 개행 없이 끝난다 (쓰다 만 줄 흉내)
	content := strings.Join([]string{old1, fresh1, broken, old2, fresh2}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, err := pruneVisitLog(path, cutoff)
	if err != nil {
		t.Fatalf("pruneVisitLog: %v", err)
	}
	if removed != 2 {
		t.Errorf("삭제 건수 = %d, want 2", removed)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// 순서 유지, 해석 못 한 줄은 보존, 끝은 개행으로 마무리
	want := fresh1 + "\n" + broken + "\n" + fresh2 + "\n"
	if string(got) != want {
		t.Errorf("정리 후 내용 =\n%s\nwant\n%s", got, want)
	}

	// 임시 파일이 남으면 안 된다
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("디렉터리에 파일 %d개 — 임시 파일이 남았다", len(entries))
	}

	// 지울 것이 없으면 원본을 건드리지 않는다
	if removed, err := pruneVisitLog(path, cutoff); err != nil || removed != 0 {
		t.Errorf("재실행: removed=%d err=%v", removed, err)
	}
	if again, _ := os.ReadFile(path); string(again) != want {
		t.Error("지울 것이 없는데 내용이 바뀌었다")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("재실행 후 파일 %d개 — 임시 파일이 남았다", len(entries))
	}
}

func TestPruneVisitLogMissingFile(t *testing.T) {
	removed, err := pruneVisitLog(filepath.Join(t.TempDir(), "visits.jsonl"), time.Now())
	if removed != 0 || err != nil {
		t.Errorf("파일이 없으면 조용히 넘어가야 한다: removed=%d err=%v", removed, err)
	}
}
