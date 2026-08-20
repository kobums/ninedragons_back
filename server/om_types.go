package server

import (
	"time"
)

// ==================== 오목 타입 ====================

// OMColor 돌 색. 흑이 선공이다.
type OMColor string

const (
	OMBlack OMColor = "black"
	OMWhite OMColor = "white"
)

// omOther 상대 색
func omOther(color OMColor) OMColor {
	if color == OMBlack {
		return OMWhite
	}
	return OMBlack
}

const (
	OMBoardSize = 15                        // 15×15 교차점
	OMMaxMoves  = OMBoardSize * OMBoardSize // 225수 소진 시 무승부(만패)
)

// OMPhase 게임 진행 단계 (내부용 — 와이어에는 싣지 않는다)
type OMPhase string

const (
	OMPhaseLobby    OMPhase = "lobby"
	OMPhasePlay     OMPhase = "play"
	OMPhaseGameOver OMPhase = "game_over"
)

// OMMessageType 오목 메시지 타입
type OMMessageType string

const (
	// 클라이언트 → 서버
	OMMsgJoinGame   OMMessageType = "om_join_game"
	OMMsgRejoinGame OMMessageType = "om_rejoin_game"
	OMMsgMove       OMMessageType = "om_move"
	OMMsgRematch    OMMessageType = "om_rematch"

	// 서버 → 클라이언트
	OMMsgPlayerJoined         OMMessageType = "om_player_joined"
	OMMsgWaitingPlayer        OMMessageType = "om_waiting_player"
	OMMsgGameState            OMMessageType = "om_game_state"
	OMMsgEvent                OMMessageType = "om_event"
	OMMsgGameOver             OMMessageType = "om_game_over"
	OMMsgRematchOffer         OMMessageType = "om_rematch_offer"
	OMMsgOpponentDisconnected OMMessageType = "om_opponent_disconnected"
	OMMsgOpponentReconnected  OMMessageType = "om_opponent_reconnected"
	OMMsgSessionExpired       OMMessageType = "om_session_expired"
	OMMsgError                OMMessageType = "om_error"
)

// OMCell 보드 교차점 좌표
type OMCell struct {
	Row int `json:"row"`
	Col int `json:"col"`
}

// OMGame 오목 게임 상태 (순수, 허브 비의존)
type OMGame struct {
	ID    string
	Names map[OMColor]string
	Phase OMPhase

	// Board[row][col]: 0 빈, 1 흑, 2 백
	Board     [OMBoardSize][OMBoardSize]int
	MoveCount int
	LastMove  *OMCell

	CurrentColor OMColor
	Winner       OMColor  // "" 는 무승부(또는 미정)
	EndReason    string   // "five" | "draw" | "forfeit"
	WinLine      []OMCell // 승리 5목(이상) 좌표. 무승부는 빈 슬라이스.

	Ready     bool
	StartedAt time.Time
}

// OMClient 오목 클라이언트 연결
type OMClient struct {
	wsClient
	Hub   *OMHub
	Color OMColor
}

// OMMessage 메시지 봉투
type OMMessage struct {
	Type    OMMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type OMJoinGamePayload struct {
	PlayerName string `json:"playerName"`
	// VsBot true 면 대기 슬롯을 거치지 않고 연습봇과 즉시 매칭 (사람이 흑)
	VsBot bool `json:"vsBot,omitempty"`
}

type OMRejoinGamePayload struct {
	SessionID string `json:"sessionId"`
}

// OMMovePayload 착수 좌표
type OMMovePayload struct {
	Row int `json:"row"`
	Col int `json:"col"`
}

// ==================== 서버 → 클라이언트 payload ====================

// OMGameStatePayload 게임 스냅샷. 은닉 정보가 없어 YourColor 외에는 양측 동일하다.
type OMGameStatePayload struct {
	GameID       string  `json:"gameId"`
	YourColor    OMColor `json:"yourColor"`
	CurrentColor OMColor `json:"currentColor"`
	BlackName    string  `json:"blackName"`
	WhiteName    string  `json:"whiteName"`

	// 항상 15×15 로 채워 보낸다 (nil 슬라이스 금지)
	Board     [][]int `json:"board"`
	MoveCount int     `json:"moveCount"`
	LastMove  *OMCell `json:"lastMove"` // 첫 수 전에는 null

	OpponentConnected bool `json:"opponentConnected"`
}

// OMEventPayload 연출용 이벤트
type OMEventPayload struct {
	Kind    string  `json:"kind"`           // "joined" | "placed" | "game_over"
	Seat    OMColor `json:"seat,omitempty"` // 행동한 돌 색 (무승부 종료는 생략)
	Name    string  `json:"name"`
	Message string  `json:"message"`
}

// OMGameOverPayload 게임 종료
type OMGameOverPayload struct {
	Winner     OMColor  `json:"winner"` // "" 는 무승부
	WinnerName string   `json:"winnerName"`
	Reason     string   `json:"reason"` // "five" | "draw" | "forfeit"
	Line       []OMCell `json:"line"`   // 승리 5목 좌표 (무승부·몰수는 [])
}
