package server

import (
	"net/http/httptest"
	"testing"
)

func TestParseAllowedOrigins(t *testing.T) {
	set := parseAllowedOrigins(" https://A.example.com/ , ,http://localhost:9003")
	if !set["https://a.example.com"] || !set["http://localhost:9003"] || len(set) != 2 {
		t.Fatalf("unexpected set: %v", set)
	}
}

func TestOriginChecks(t *testing.T) {
	InitAllowedOrigins()

	r := httptest.NewRequest("GET", "/ws/quoridor", nil)
	if !checkWSOrigin(r) {
		t.Error("Origin 없는 비브라우저 요청은 허용")
	}
	r.Header.Set("Origin", "https://evil.example.com")
	if checkWSOrigin(r) {
		t.Error("허용 목록 밖 origin 은 거부")
	}
	r.Header.Set("Origin", "https://ninedragons.gowoobro.com")
	if !checkWSOrigin(r) {
		t.Error("운영 웹 origin 은 허용")
	}

	rec := httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/lobby", nil)
	r.Header.Set("Origin", "https://evil.example.com")
	LobbyHandler(rec, r)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("허용 목록 밖 origin 에 CORS 헤더: %q", got)
	}
}
