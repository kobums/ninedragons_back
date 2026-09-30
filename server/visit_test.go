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

	var got []string
	orig := sendVisitNotify
	sendVisitNotify = func(ip, path string) { got = append(got, ip+" "+path) }
	defer func() { sendVisitNotify = orig }()

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
	send("1.2.3.4", "Mozilla/5.0")    // TTL 안 중복 — 알림 없음
	send("172.22.0.5", "Mozilla/5.0") // 내부 IP — 알림 없음
	send("2.3.4.5", "Googlebot/2.1")  // 봇 — 알림 없음
	send("3.4.5.6", "curl/8.0")       // curl — 알림 없음

	if handled != 5 {
		t.Fatalf("핸들러는 항상 실행되어야 한다: %d/5", handled)
	}
	if len(got) != 1 || got[0] != "1.2.3.4 /ws/azul" {
		t.Fatalf("알림은 사람 첫 접속 한 번만: %v", got)
	}
}
