package server

import (
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
