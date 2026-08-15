package server

import (
	"time"
)

// ==================== Quoridor Game Types ====================

// QDSide 진영. 남쪽 폰이 (8,4)에서 행 0을 향해, 북쪽 폰이 (0,4)에서 행 8을 향해 간다.
type QDSide string

const (
	QDSouth QDSide = "south"
	QDNorth QDSide = "north"
)

// qdOther 반대 진영
func qdOther(side QDSide) QDSide {
	if side == QDSouth {
		return QDNorth
	}
	return QDSouth
}

const (
	QDBoardSize = 9  // 9x9 보드
	QDWallCount = 10 // 진영당 벽 개수
)

// QDPhase 게임 진행 단계 (배치 단계 없음 — 완전 공개 정보 게임)
type QDPhase string

const (
	QDPhaseLobby    QDPhase = "lobby"
	QDPhasePlay     QDPhase = "play"
	QDPhaseGameOver QDPhase = "game_over"
)

// QDMessageType 쿼리도 메시지 타입
type QDMessageType string

const (
	// 클라이언트 → 서버
	QDMsgJoinGame   QDMessageType = "qd_join_game"
	QDMsgRejoinGame QDMessageType = "qd_rejoin_game"
	QDMsgMove       QDMessageType = "qd_move"
	QDMsgPlaceWall  QDMessageType = "qd_place_wall"
	QDMsgRematch    QDMessageType = "qd_rematch"

	// 서버 → 클라이언트
	QDMsgPlayerJoined         QDMessageType = "qd_player_joined"
	QDMsgWaitingPlayer        QDMessageType = "qd_waiting_player"
	QDMsgGameState            QDMessageType = "qd_game_state"
	QDMsgEvent                QDMessageType = "qd_event"
	QDMsgGameOver             QDMessageType = "qd_game_over"
	QDMsgRematchOffer         QDMessageType = "qd_rematch_offer"
	QDMsgOpponentDisconnected QDMessageType = "qd_opponent_disconnected"
	QDMsgOpponentReconnected  QDMessageType = "qd_opponent_reconnected"
	QDMsgSessionExpired       QDMessageType = "qd_session_expired"
	QDMsgError                QDMessageType = "qd_error"
)

// QDCell 보드 좌표
type QDCell struct {
	Row int `json:"row"`
	Col int `json:"col"`
}

// QDWall 벽 하나. 앵커 (Row, Col)은 0~7 범위의 교차점 좌표다.
//   - 가로("h"): 행 Row 와 Row+1 사이, 열 Col·Col+1 두 칸에 걸친다
//   - 세로("v"): 열 Col 과 Col+1 사이, 행 Row·Row+1 두 칸에 걸친다
type QDWall struct {
	Row         int    `json:"row"`
	Col         int    `json:"col"`
	Orientation string `json:"orientation"` // "h" | "v"
}

// QDGame 쿼리도 게임 상태 (순수, 허브 비의존)
type QDGame struct {
	ID    string
	Names map[QDSide]string
	Phase QDPhase

	Pawns     map[QDSide]QDCell
	Walls     []QDWall
	WallsLeft map[QDSide]int

	CurrentSide QDSide
	Winner      QDSide
	EndReason   string // "reach_goal"

	Ready     bool
	StartedAt time.Time
}

// QDClient 쿼리도 클라이언트 연결
type QDClient struct {
	wsClient
	Hub  *QDHub
	Side QDSide
}

// QDMessage 메시지 봉투
type QDMessage struct {
	Type    QDMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type QDJoinGamePayload struct {
	PlayerName string `json:"playerName"`
	// VsBot true 면 대기 슬롯을 거치지 않고 연습봇과 즉시 매칭
	VsBot bool `json:"vsBot,omitempty"`
}

type QDRejoinGamePayload struct {
	SessionID string `json:"sessionId"`
}

// QDMovePayload 폰 이동 (출발 칸은 서버가 아는 내 폰 위치)
type QDMovePayload struct {
	To QDCell `json:"to"`
}

// QDPlaceWallPayload 벽 설치
type QDPlaceWallPayload struct {
	Row         int    `json:"row"`
	Col         int    `json:"col"`
	Orientation string `json:"orientation"`
}

// ==================== 서버 → 클라이언트 payload ====================

// QDGameStatePayload 게임 스냅샷. 완전 공개 정보라 YourSide 외에는 양측 동일하다.
type QDGameStatePayload struct {
	GameID      string  `json:"gameId"`
	YourSide    QDSide  `json:"yourSide"`
	Phase       QDPhase `json:"phase"`
	CurrentSide QDSide  `json:"currentSide"`
	SouthName   string  `json:"southName"`
	NorthName   string  `json:"northName"`

	SouthPawn  QDCell   `json:"southPawn"`
	NorthPawn  QDCell   `json:"northPawn"`
	SouthWalls int      `json:"southWalls"`
	NorthWalls int      `json:"northWalls"`
	Walls      []QDWall `json:"walls"`
	// 현재 차례 진영의 합법 폰 이동 칸 (점프·대각 우회 포함 — 공개 정보)
	LegalMoves []QDCell `json:"legalMoves"`

	OpponentConnected bool `json:"opponentConnected"`
}

// QDEventPayload 연출용 이벤트
type QDEventPayload struct {
	Kind string  `json:"kind"` // "move" | "wall"
	Side QDSide  `json:"side"` // 행동한 진영
	From *QDCell `json:"from,omitempty"`
	To   *QDCell `json:"to,omitempty"`
	Wall *QDWall `json:"wall,omitempty"`
}

// QDGameOverPayload 게임 종료
type QDGameOverPayload struct {
	Winner     QDSide `json:"winner"`
	WinnerName string `json:"winnerName"`
	Reason     string `json:"reason"`
}
