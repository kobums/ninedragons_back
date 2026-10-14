package server

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// ==================== IP 지오로케이션 ====================
//
// ip-api.com 무료 API (비상업, 분당 45회 제한)로 국가·지역·도시·통신사를
// 조회한다. 접속 로그 보강용이라 실패해도 빈 값으로 넘어간다. IP당 한 번만
// 조회해 캐시하고, 무료 한도 아래로 자체 제한한다. HTTPS 는 유료 플랜이라
// HTTP 를 쓴다 — 공개 IP 의 위치 메타데이터라 수용한다.

type geoInfo struct {
	Country string `json:"country"`
	Region  string `json:"regionName"`
	City    string `json:"city"`
	ISP     string `json:"isp"`
	Org     string `json:"org"`
	AS      string `json:"as"`
}

const (
	geoCacheLimit  = 10000
	geoRatePerMin  = 40 // ip-api 무료 한도(45/분) 아래로
	geoLookupFlags = "status,country,regionName,city,isp,org,as"
)

var (
	geoMu     sync.Mutex
	geoCache  = map[string]geoInfo{}
	geoWindow time.Time
	geoCount  int
	geoClient = &http.Client{Timeout: 5 * time.Second}
)

// geoAllow 분당 조회 수 제한 (geoMu 잠금은 호출자 책임)
func geoAllow(now time.Time) bool {
	if now.Sub(geoWindow) >= time.Minute {
		geoWindow = now
		geoCount = 0
	}
	if geoCount >= geoRatePerMin {
		return false
	}
	geoCount++
	return true
}

// lookupGeo IP 의 위치·통신사 정보를 조회한다. 캐시 우선이고, 실패·한도 초과
// 시 빈 값을 주되 캐시하지 않아 다음 접속 때 재시도된다. 접속 로그 고루틴에서만
// 부르므로 블로킹(최대 5초)해도 요청 처리에는 영향이 없다. 테스트에서 실제
// 호출 없이 검증할 수 있도록 변수로 뒀다.
var lookupGeo = func(ip string) geoInfo {
	geoMu.Lock()
	if g, ok := geoCache[ip]; ok {
		geoMu.Unlock()
		return g
	}
	allowed := geoAllow(time.Now())
	geoMu.Unlock()
	if !allowed {
		return geoInfo{}
	}

	resp, err := geoClient.Get("http://ip-api.com/json/" + ip + "?fields=" + geoLookupFlags + "&lang=ko")
	if err != nil {
		return geoInfo{}
	}
	defer resp.Body.Close()

	var body struct {
		Status string `json:"status"`
		geoInfo
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil || body.Status != "success" {
		return geoInfo{}
	}

	geoMu.Lock()
	if len(geoCache) >= geoCacheLimit {
		// 개인 규모에선 도달할 일이 드물다 — 단순 전체 비움으로 충분
		geoCache = map[string]geoInfo{}
	}
	geoCache[ip] = body.geoInfo
	geoMu.Unlock()
	return body.geoInfo
}
