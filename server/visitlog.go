package server

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mileusna/useragent"
)

// ==================== 접속 로그 ====================
//
// 모든 외부 요청을 JSONL 파일에 한 줄씩 적재한다 (gowoobro_go 의 iplog_tb 에
// 해당). 전적 기록(stats.go)과 같은 방식 — DB 없이 파일 append, 전용 고루틴,
// 재시작해도 파일이 남는다. 지오 조회(ip-api.com)가 최대 5초 걸릴 수 있으므로
// 요청 핸들러는 채널에 넣기만 하고 적재·조회·알림은 전부 이 고루틴이 한다.

// VisitRecord 접속 한 건의 기록. 비어 있는 필드는 줄을 짧게 유지하려고 뺀다.
type VisitRecord struct {
	Time    time.Time `json:"time"`
	IP      string    `json:"ip"`
	Path    string    `json:"path"`
	Country string    `json:"country,omitempty"`
	Region  string    `json:"region,omitempty"` // 도/광역시 단위
	City    string    `json:"city,omitempty"`
	ISP     string    `json:"isp,omitempty"` // 통신사/회선 (KT, SKB …)
	Org     string    `json:"org,omitempty"`
	AS      string    `json:"as,omitempty"` // 네트워크 AS 번호·이름
	OS      string    `json:"os,omitempty"`
	Browser string    `json:"browser,omitempty"`
	Device  string    `json:"device,omitempty"` // iPhone, 모바일/데스크톱 등
	Bot     bool      `json:"bot,omitempty"`
	Agent   string    `json:"agent,omitempty"` // 원문 User-Agent (파싱 못 한 정보 보존)
	Lang    string    `json:"lang,omitempty"`  // Accept-Language
	Referer string    `json:"referer,omitempty"`
}

// visitReq 미들웨어가 채널에 넣는 원재료 — 요청에서 추출만 해온다
type visitReq struct {
	time    time.Time
	ip      string
	path    string
	agent   string
	lang    string
	referer string
	notify  bool // 사람 첫 접속(TTL 창) — ntfy 발송 대상
}

var visitCh chan visitReq

// InitVisitLog 접속 로그 고루틴을 시작한다. 디렉터리가 없으면 만든다.
func InitVisitLog(path string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Printf("[visit] 로그 디렉터리 생성 실패: %v — 접속 로그 비활성", err)
		return
	}
	ch := make(chan visitReq, 256)
	go runVisitLog(path, ch)
	visitCh = ch
	log.Printf("[visit] 접속 로그 활성 (%s)", path)
}

// recordVisit 접속 한 건을 기록 큐에 넣는다 (논블로킹). 접속 로그가 비활성
// (디렉터리 실패·테스트)이어도 알림은 보낸다. 테스트 스터빙용 변수.
var recordVisit = func(req visitReq) {
	if visitCh == nil {
		if req.notify {
			go sendVisitNotify(buildVisitRecord(req))
		}
		return
	}
	select {
	case visitCh <- req:
	default:
		log.Printf("[visit] 기록 버림 (큐 가득)")
	}
}

// runVisitLog 기록 고루틴: 지오 조회 → 파일 append → 알림
func runVisitLog(path string, ch chan visitReq) {
	for req := range ch {
		rec := buildVisitRecord(req)

		if data, err := json.Marshal(rec); err == nil {
			f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				log.Printf("[visit] 기록 실패: %v", err)
			} else {
				f.Write(append(data, '\n'))
				f.Close()
			}
		}

		// 라이브러리가 봇으로 판정한 경우(isBotAgent 보다 넓다)도 알림은 거른다
		if req.notify && !rec.Bot {
			sendVisitNotify(rec)
		}
	}
}

// buildVisitRecord UA 파싱과 지오 조회로 기록을 완성한다. 봇은 지오 조회를
// 생략한다 — 크롤러는 IP 가 다양해 무료 한도만 축낸다.
func buildVisitRecord(req visitReq) VisitRecord {
	rec := VisitRecord{
		Time:    req.time,
		IP:      req.ip,
		Path:    req.path,
		Agent:   req.agent,
		Lang:    req.lang,
		Referer: req.referer,
	}

	ua := useragent.Parse(req.agent)
	rec.OS = strings.TrimSpace(ua.OS + " " + ua.OSVersion)
	// 전체 버전(126.0.6478.127)은 노이즈라 메이저 버전까지만 (gowoobro_go 와 동일)
	version := ua.Version
	if idx := strings.Index(version, "."); idx > 0 {
		version = version[:idx]
	}
	rec.Browser = strings.TrimSpace(ua.Name + " " + version)
	rec.Bot = ua.Bot || isBotAgent(req.agent)
	switch {
	case ua.Device != "":
		rec.Device = ua.Device
	case ua.Mobile:
		rec.Device = "모바일"
	case ua.Tablet:
		rec.Device = "태블릿"
	case ua.Desktop:
		rec.Device = "데스크톱"
	}

	if !rec.Bot {
		geo := lookupGeo(req.ip)
		rec.Country = geo.Country
		rec.Region = geo.Region
		rec.City = geo.City
		rec.ISP = geo.ISP
		rec.Org = geo.Org
		rec.AS = geo.AS
	}
	return rec
}

// joinParts 빈 문자열을 건너뛰고 구분자로 잇는다
func joinParts(sep string, parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}

// visitMessage 알림 본문: IP → 위치·통신사 → 기기 → 경로 순으로 아는 만큼만
func visitMessage(rec VisitRecord) string {
	lines := []string{rec.IP}
	if geo := joinParts(" · ", rec.Country, joinParts(" ", rec.Region, rec.City), rec.ISP); geo != "" {
		lines = append(lines, geo)
	}
	if dev := joinParts(" · ", rec.Device, rec.OS, rec.Browser); dev != "" {
		lines = append(lines, dev)
	}
	lines = append(lines, rec.Path)
	return strings.Join(lines, "\n")
}

// sendVisitNotify 테스트에서 실제 발송 없이 검증할 수 있도록 변수로 뒀다.
// notify 자체가 비동기라 기록 고루틴을 오래 막지 않는다.
var sendVisitNotify = func(rec VisitRecord) {
	notify("나인드래곤즈 접속", visitMessage(rec))
}
