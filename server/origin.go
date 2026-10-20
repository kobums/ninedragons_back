package server

import (
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
)

// defaultAllowedOrigins ALLOWED_ORIGINS 미설정 시 허용하는 origin.
// 운영 웹(ninedragons_web)과 로컬 vite 개발/미리보기 서버(:9003).
var defaultAllowedOrigins = []string{
	"https://ninedragons.gowoobro.com",
	"http://localhost:9003",
	"http://127.0.0.1:9003",
}

var (
	allowedOriginsOnce sync.Once
	allowedOrigins     map[string]bool
)

func normalizeOrigin(o string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(o)), "/")
}

// parseAllowedOrigins 콤마 구분 목록을 정규화된 집합으로 만든다.
func parseAllowedOrigins(v string) map[string]bool {
	set := map[string]bool{}
	for _, o := range strings.Split(v, ",") {
		if o = normalizeOrigin(o); o != "" {
			set[o] = true
		}
	}
	return set
}

// InitAllowedOrigins ALLOWED_ORIGINS(콤마 구분)를 읽는다. 비어 있으면 기본값을
// 쓰고 경고를 남긴다. 여러 번 호출해도 한 번만 읽는다.
func InitAllowedOrigins() {
	allowedOriginsOnce.Do(func() {
		allowedOrigins = parseAllowedOrigins(os.Getenv("ALLOWED_ORIGINS"))
		if len(allowedOrigins) == 0 {
			allowedOrigins = parseAllowedOrigins(strings.Join(defaultAllowedOrigins, ","))
			log.Printf("WARNING: ALLOWED_ORIGINS 미설정 — 기본 origin 사용: %s", strings.Join(defaultAllowedOrigins, ", "))
		}
	})
}

// originAllowed 브라우저가 보낸 Origin 이 허용 목록에 있는지
func originAllowed(origin string) bool {
	InitAllowedOrigins()
	return allowedOrigins[normalizeOrigin(origin)]
}

// checkWSOrigin WebSocket 업그레이드 origin 검사. Origin 헤더가 없는 요청은
// 브라우저가 아닌 클라이언트(테스트·도구)이므로 cross-site 위험이 없어 허용한다
// (gorilla 기본 동작과 동일).
func checkWSOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if originAllowed(origin) {
		return true
	}
	log.Printf("websocket origin 거부: %q", origin)
	return false
}

// setCORS 허용된 origin 에만 Access-Control-Allow-Origin 을 돌려준다.
func setCORS(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "Origin")
	if origin := r.Header.Get("Origin"); origin != "" && originAllowed(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
	}
}
