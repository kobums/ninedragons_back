package server

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// 접속 알림. 페이지 하나를 열어도 lobby 폴링·WS 재접속으로 요청이 여러 번
// 오므로, 같은 IP 는 visitTTL 안에서 한 번만 알린다 (gowoobro_go 의
// router/visitlog.go 와 같은 방식).
const visitTTL = 5 * time.Minute

// visitSeenLimit 를 넘으면 만료된 항목을 청소한다. 크롤러가 IP 를 바꿔가며
// 훑어도 메모리가 무한히 크지 않게 막아둔다.
const visitSeenLimit = 10000

var (
	visitMu   sync.Mutex
	visitSeen = map[string]time.Time{}
)

func shouldNotifyVisit(ip string, now time.Time) bool {
	visitMu.Lock()
	defer visitMu.Unlock()

	if last, ok := visitSeen[ip]; ok && now.Sub(last) < visitTTL {
		return false
	}

	if len(visitSeen) >= visitSeenLimit {
		for key, last := range visitSeen {
			if now.Sub(last) >= visitTTL {
				delete(visitSeen, key)
			}
		}
	}

	visitSeen[ip] = now
	return true
}

// clientIP 는 nginx-proxy 뒤에서 실제 방문자 IP 를 얻는다. 백엔드 포트는
// 도커 네트워크 밖으로 열려 있지 않으므로 X-Forwarded-For 를 신뢰할 수 있다.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			xff = xff[:i]
		}
		return strings.TrimSpace(xff)
	}
	if rip := r.Header.Get("X-Real-Ip"); rip != "" {
		return strings.TrimSpace(rip)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// isInternalIP 는 사설 대역(RFC 1918)·루프백 주소인지 판정한다. 도커 네트워크
// 안 컨테이너끼리의 호출은 방문이 아니다.
func isInternalIP(address string) bool {
	addr, err := netip.ParseAddr(address)
	if err != nil {
		return false
	}
	return addr.IsPrivate() || addr.IsLoopback()
}

// isBotAgent 봇 크롤링까지 알리면 알림이 폭주하므로 사람 접속만 알린다.
// 의존성 없이 대표 패턴만 거른다 — WS 게임 접속은 어차피 봇이 못 한다.
func isBotAgent(agent string) bool {
	agent = strings.ToLower(agent)
	for _, mark := range []string{"bot", "crawler", "spider", "crawling", "curl/", "python-requests"} {
		if strings.Contains(agent, mark) {
			return true
		}
	}
	return false
}

// VisitNotify 는 핸들러 앞에서 접속을 기록 큐에 넣는 미들웨어다. 적재는 모든
// 외부 요청(봇 포함 — bot 플래그로 구분), 알림은 사람의 첫 접속(TTL 창)만.
// 기록·알림이 실패하거나 미설정이어도 요청 처리에는 영향이 없다.
func VisitNotify(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodOptions {
			if ip := clientIP(r); ip != "" && !isInternalIP(ip) {
				agent := r.Header.Get("User-Agent")
				recordVisit(visitReq{
					time:    time.Now(),
					ip:      ip,
					path:    r.URL.Path,
					agent:   agent,
					lang:    r.Header.Get("Accept-Language"),
					referer: r.Header.Get("Referer"),
					notify:  !isBotAgent(agent) && shouldNotifyVisit(ip, time.Now()),
				})
			}
		}
		next(w, r)
	}
}
