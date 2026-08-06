package server

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"
)

// ntfy 운영자 알림
// NTFY_TOPIC이 비어 있으면 발송하지 않는다 (로컬 개발 환경 기본값).
var (
	ntfyServer = envOr("NTFY_SERVER", "https://ntfy.sh")
	ntfyTopic  = os.Getenv("NTFY_TOPIC")
	ntfyClient = &http.Client{Timeout: 5 * time.Second}
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// notify 게임 이벤트를 ntfy 토픽으로 발송한다.
// 허브 루프를 막지 않도록 비동기로 보내고, 실패해도 게임 진행에는 영향을 주지 않는다.
// 한글 제목은 HTTP 헤더(latin-1 제약)에 실을 수 없으므로 JSON publish 방식을 쓴다.
func notify(title, message string) {
	if ntfyTopic == "" {
		return
	}

	body, err := json.Marshal(map[string]string{
		"topic":   ntfyTopic,
		"title":   title,
		"message": message,
	})
	if err != nil {
		return
	}

	go func() {
		resp, err := ntfyClient.Post(ntfyServer, "application/json", bytes.NewReader(body))
		if err != nil {
			log.Printf("[ntfy] 알림 발송 실패: %v", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			log.Printf("[ntfy] 알림 발송 실패: status=%d", resp.StatusCode)
		}
	}()
}
