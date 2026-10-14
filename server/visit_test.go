package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientIP(t *testing.T) {
	cases := []struct {
		name   string
		xff    string
		realIP string
		remote string
		want   string
	}{
		{"XFF 단일", "1.2.3.4", "", "172.22.0.2:1234", "1.2.3.4"},
		{"XFF 다중은 첫 항목", "1.2.3.4, 10.0.0.1", "", "172.22.0.2:1234", "1.2.3.4"},
		{"X-Real-Ip 폴백", "", "5.6.7.8", "172.22.0.2:1234", "5.6.7.8"},
		{"RemoteAddr 폴백", "", "", "9.9.9.9:5555", "9.9.9.9"},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/lobby", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if c.realIP != "" {
			r.Header.Set("X-Real-Ip", c.realIP)
		}
		if got := clientIP(r); got != c.want {
			t.Errorf("%s: clientIP = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestShouldNotifyVisitTTL(t *testing.T) {
	visitMu.Lock()
	visitSeen = map[string]time.Time{}
	visitMu.Unlock()

	now := time.Now()
	if !shouldNotifyVisit("1.2.3.4", now) {
		t.Fatal("첫 접속은 알려야 한다")
	}
	if shouldNotifyVisit("1.2.3.4", now.Add(time.Minute)) {
		t.Fatal("TTL 안 재접속은 알리지 않아야 한다")
	}
	if !shouldNotifyVisit("1.2.3.4", now.Add(visitTTL+time.Second)) {
		t.Fatal("TTL 지난 재접속은 다시 알려야 한다")
	}
	if !shouldNotifyVisit("5.6.7.8", now) {
		t.Fatal("다른 IP 는 별도로 알려야 한다")
	}
}

func TestVisitNotifyMiddleware(t *testing.T) {
	visitMu.Lock()
	visitSeen = map[string]time.Time{}
	visitMu.Unlock()

	var got []visitReq
	orig := recordVisit
	recordVisit = func(req visitReq) { got = append(got, req) }
	defer func() { recordVisit = orig }()

	handled := 0
	h := VisitNotify(func(w http.ResponseWriter, r *http.Request) { handled++ })

	send := func(ip, ua string) {
		r := httptest.NewRequest(http.MethodGet, "/ws/azul", nil)
		r.Header.Set("X-Forwarded-For", ip)
		if ua != "" {
			r.Header.Set("User-Agent", ua)
		}
		h(httptest.NewRecorder(), r)
	}

	send("1.2.3.4", "Mozilla/5.0")
	send("1.2.3.4", "Mozilla/5.0")    // TTL 안 중복 — 적재는 되고 알림만 없음
	send("172.22.0.5", "Mozilla/5.0") // 내부 IP — 적재도 알림도 없음
	send("2.3.4.5", "Googlebot/2.1")  // 봇 — 적재는 되고 알림만 없음

	if handled != 4 {
		t.Fatalf("핸들러는 항상 실행되어야 한다: %d/4", handled)
	}
	if len(got) != 3 {
		t.Fatalf("외부 요청 3건이 적재되어야 한다: %d건 %v", len(got), got)
	}
	var notified []string
	for _, req := range got {
		if req.path != "/ws/azul" {
			t.Errorf("경로 기록 오류: %q", req.path)
		}
		if req.notify {
			notified = append(notified, req.ip)
		}
	}
	if len(notified) != 1 || notified[0] != "1.2.3.4" {
		t.Fatalf("알림은 사람 첫 접속 한 번만: %v", notified)
	}
}
