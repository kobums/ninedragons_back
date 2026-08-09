package server

import (
	"github.com/gorilla/websocket"
)

// wsClient 다섯 게임 클라이언트가 공유하는 연결 필드. 각 XClient 가 값으로
// 임베드하며, 필드 승격 덕분에 기존 c.SessionID / c.Send 같은 접근은 그대로
// 컴파일된다. 게임별 고유 필드(Hub, 슬롯)는 각 XClient 에 남는다.
type wsClient struct {
	ID        string
	SessionID string // 연결이 아닌 플레이어 신원 식별자 (재접속 시 유지)
	Name      string
	Conn      *websocket.Conn
	Send      chan []byte
	GameID    string
	Connected bool // 각 허브 고루틴에서만 접근
}

// SessionKey 세션 장부의 키
func (c *wsClient) SessionKey() string { return c.SessionID }

// IsConnected 연결 유지 여부 (허브 고루틴에서만 호출)
func (c *wsClient) IsConnected() bool { return c.Connected }

// CloseConn 중복 접속 강제 종료용
func (c *wsClient) CloseConn() { c.Conn.Close() }
