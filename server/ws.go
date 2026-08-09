package server

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 512
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // 개발 환경에서는 모든 origin 허용
	},
}

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

// newWSClient ServeXWs 다섯 곳이 공유하는 연결 초기화
func newWSClient(id string, conn *websocket.Conn) wsClient {
	return wsClient{
		ID:        id,
		Conn:      conn,
		Send:      make(chan []byte, 256),
		Connected: true,
	}
}

// readLoop 수신 펌프. 메시지 타입만 게임별로 다르고 본문은 다섯 게임이
// 동일하다. 연결이 끊기면 unregister 를 호출하고 소켓을 닫는다.
func readLoop[M any](conn *websocket.Conn, logPrefix string, deliver func(M), unregister func()) {
	defer func() {
		unregister()
		conn.Close()
	}()

	conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf(logPrefix+"error: %v", err)
			}
			break
		}

		var msg M
		if err := json.Unmarshal(message, &msg); err != nil {
			log.Printf(logPrefix+"Error unmarshaling message: %v", err)
			continue
		}

		deliver(msg)
	}
}

// writeLoop 송신 펌프. 대기 중인 메시지들을 '\n' 으로 이어붙여 한 프레임에
// 보낸다 — 프론트의 개행 분리 파싱이 이 동작에 의존한다.
func writeLoop(conn *websocket.Conn, send chan []byte) {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		conn.Close()
	}()

	for {
		select {
		case message, ok := <-send:
			conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)

			// 대기 중인 메시지 추가
			n := len(send)
			for i := 0; i < n; i++ {
				w.Write([]byte{'\n'})
				w.Write(<-send)
			}

			if err := w.Close(); err != nil {
				return
			}

		case <-ticker.C:
			conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// sendTo 마샬 + 논블로킹 전송. 연결이 끊긴(재접속 대기 중) 클라이언트에게는
// 보내지 않는다. 각 허브의 sendToClient 가 nil 가드 후 이걸 호출한다.
func sendTo[M any](c *wsClient, message M, logPrefix string) {
	if !c.Connected {
		return
	}

	data, err := json.Marshal(message)
	if err != nil {
		log.Printf(logPrefix+"Error marshaling message: %v", err)
		return
	}

	select {
	case c.Send <- data:
	default:
		// 버퍼가 가득 찬 연결은 죽은 것으로 간주하고 닫는다
		// (이후 readLoop 종료 → unregister 경로에서 세션 정리가 이어진다)
		c.Connected = false
		close(c.Send)
	}
}
