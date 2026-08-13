package server

import (
	"time"
)

// ==================== Lost Cities Game Types ====================

// LCSide 진영 (보드 게임들과 표기 통일 — 남/북)
type LCSide string

const (
	LCSouth LCSide = "south"
	LCNorth LCSide = "north"
)

// lcOther 반대 진영
func lcOther(side LCSide) LCSide {
	if side == LCSouth {
		return LCNorth
	}
	return LCSouth
}

// LCColor 탐험지 색 5종
type LCColor string

var lcColors = []LCColor{"red", "green", "blue", "white", "yellow"}

const (
	LCHandSize   = 8  // 손패 수
	LCWagerCount = 3  // 색당 투자 카드 수
	LCWagerValue = 0  // 투자 카드의 Value
	LCDeckSize   = 60 // 5색 × (투자 3 + 숫자 2~10)
	LCExpedCost  = 20 // 탐험 시작 비용
	LCBonusSize  = 8  // 카드 8장 이상 탐험의 보너스 기준
	LCBonus      = 20
)

// LCPhase 게임 진행 단계
type LCPhase string

const (
	LCPhaseLobby    LCPhase = "lobby"
	LCPhasePlay     LCPhase = "play"
	LCPhaseGameOver LCPhase = "game_over"
)

// LCMessageType 로스트 시티 메시지 타입
type LCMessageType string

const (
	// 클라이언트 → 서버
	LCMsgJoinGame   LCMessageType = "lc_join_game"
	LCMsgRejoinGame LCMessageType = "lc_rejoin_game"
	LCMsgMove       LCMessageType = "lc_move"

	// 서버 → 클라이언트
	LCMsgPlayerJoined         LCMessageType = "lc_player_joined"
	LCMsgWaitingPlayer        LCMessageType = "lc_waiting_player"
	LCMsgGameState            LCMessageType = "lc_game_state"
	LCMsgEvent                LCMessageType = "lc_event"
	LCMsgGameOver             LCMessageType = "lc_game_over"
	LCMsgOpponentDisconnected LCMessageType = "lc_opponent_disconnected"
	LCMsgOpponentReconnected  LCMessageType = "lc_opponent_reconnected"
	LCMsgSessionExpired       LCMessageType = "lc_session_expired"
	LCMsgError                LCMessageType = "lc_error"
)

// LCCard 카드 한 장. Value 0 = 투자 카드, 2~10 = 숫자 카드.
// ID 는 덱 전체에서 유일 (공개된 카드의 ID 노출은 정보 유출이 아니다).
type LCCard struct {
	ID    int     `json:"id"`
	Color LCColor `json:"color"`
	Value int     `json:"value"`
}

// LCGame 로스트 시티 게임 상태 (순수, 허브 비의존).
// 비밀 정보는 Deck 순서와 Hands — 스냅샷에서 내 손패만 노출한다.
type LCGame struct {
	ID    string
	Names map[LCSide]string
	Phase LCPhase

	Deck        []LCCard
	Hands       map[LCSide][]LCCard
	Expeditions map[LCSide]map[LCColor][]LCCard
	Discards    map[LCColor][]LCCard

	CurrentSide LCSide
	Winner      LCSide // 무승부면 ""
	EndReason   string // "score"

	Ready     bool
	StartedAt time.Time
}

// LCClient 로스트 시티 클라이언트 연결
type LCClient struct {
	wsClient
	Hub  *LCHub
	Side LCSide
}

// LCMessage 메시지 봉투
type LCMessage struct {
	Type    LCMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type LCJoinGamePayload struct {
	PlayerName string `json:"playerName"`
}

type LCRejoinGamePayload struct {
	SessionID string `json:"sessionId"`
}

// LCMovePayload 한 턴 = 놓기/버리기 + 뽑기 (원자적으로 처리)
type LCMovePayload struct {
	CardID int    `json:"cardId"`
	Action string `json:"action"` // "play" | "discard"
	Draw   string `json:"draw"`   // "deck" | 색 이름 (버림 더미)
}

// ==================== 서버 → 클라이언트 payload ====================

// LCGameStatePayload 개인화 스냅샷 — 내 손패는 전체, 상대는 장수만
type LCGameStatePayload struct {
	GameID      string  `json:"gameId"`
	YourSide    LCSide  `json:"yourSide"`
	Phase       LCPhase `json:"phase"`
	CurrentSide LCSide  `json:"currentSide"`
	SouthName   string  `json:"southName"`
	NorthName   string  `json:"northName"`

	YourHand          []LCCard `json:"yourHand"`
	OpponentHandCount int      `json:"opponentHandCount"`
	DeckCount         int      `json:"deckCount"`

	// 공개 정보: 양측 탐험대, 버림 더미, 현재 점수
	SouthExpeditions map[LCColor][]LCCard `json:"southExpeditions"`
	NorthExpeditions map[LCColor][]LCCard `json:"northExpeditions"`
	Discards         map[LCColor][]LCCard `json:"discards"`
	SouthScore       int                  `json:"southScore"`
	NorthScore       int                  `json:"northScore"`

	OpponentConnected bool `json:"opponentConnected"`
}

// LCEventPayload 연출용 이벤트. 놓기·버리기 카드와 버림 더미에서 뽑은
// 카드는 공개 정보다. 덱에서 뽑은 카드는 비밀이라 싣지 않는다.
type LCEventPayload struct {
	Kind string  `json:"kind"` // "play" | "discard" | "draw"
	Side LCSide  `json:"side"`
	Card *LCCard `json:"card,omitempty"`
	// draw: "deck" 또는 버림 더미 색
	Source string `json:"source,omitempty"`
}

// LCGameOverPayload 게임 종료 — 점수 공개. Winner 가 "" 면 무승부.
type LCGameOverPayload struct {
	Winner     LCSide `json:"winner"`
	WinnerName string `json:"winnerName"`
	Reason     string `json:"reason"`
	SouthScore int    `json:"southScore"`
	NorthScore int    `json:"northScore"`
}
