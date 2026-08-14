package server

import (
	"time"
)

// ==================== Can't Stop Game Types ====================

// CSSide 진영 (보드 게임들과 표기 통일 — 남/북)
type CSSide string

const (
	CSSouth CSSide = "south"
	CSNorth CSSide = "north"
)

// csOther 반대 진영
func csOther(side CSSide) CSSide {
	if side == CSSouth {
		return CSNorth
	}
	return CSSouth
}

const (
	CSMinCol     = 2  // 주사위 두 개 합의 최소
	CSMaxCol     = 12 // 최대
	CSMarkerMax  = 3  // 한 턴에 쓸 수 있는 임시 마커 수
	CSClaimToWin = 3  // 승리에 필요한 완등 컬럼 수
	CSDiceCount  = 4
)

// csColLen 컬럼 길이: 7이 13칸으로 가장 길고 양끝(2·12)이 3칸.
func csColLen(col int) int {
	d := col - 7
	if d < 0 {
		d = -d
	}
	return 13 - 2*d
}

// CSPhase 게임 진행 단계
type CSPhase string

const (
	CSPhaseLobby    CSPhase = "lobby"
	CSPhasePlay     CSPhase = "play"
	CSPhaseGameOver CSPhase = "game_over"
)

// CSMessageType 캔트 스톱 메시지 타입
type CSMessageType string

const (
	// 클라이언트 → 서버
	CSMsgJoinGame   CSMessageType = "cs_join_game"
	CSMsgRejoinGame CSMessageType = "cs_rejoin_game"
	CSMsgRoll       CSMessageType = "cs_roll"
	CSMsgChoose     CSMessageType = "cs_choose"
	CSMsgStop       CSMessageType = "cs_stop"

	// 서버 → 클라이언트
	CSMsgPlayerJoined         CSMessageType = "cs_player_joined"
	CSMsgWaitingPlayer        CSMessageType = "cs_waiting_player"
	CSMsgGameState            CSMessageType = "cs_game_state"
	CSMsgEvent                CSMessageType = "cs_event"
	CSMsgGameOver             CSMessageType = "cs_game_over"
	CSMsgOpponentDisconnected CSMessageType = "cs_opponent_disconnected"
	CSMsgOpponentReconnected  CSMessageType = "cs_opponent_reconnected"
	CSMsgSessionExpired       CSMessageType = "cs_session_expired"
	CSMsgError                CSMessageType = "cs_error"
)

// CSOption 이번 굴림에서 고를 수 있는 전진 하나 (합 1개 또는 2개)
type CSOption struct {
	Sums []int `json:"sums"`
}

// CSGame 캔트 스톱 게임 상태 (순수, 허브 비의존)
type CSGame struct {
	ID    string
	Names map[CSSide]string
	Phase CSPhase

	// 확정(뱅킹)된 진행도: 컬럼 → 칸 수
	Progress map[CSSide]map[int]int
	// 완등된 컬럼 → 가져간 진영 (양쪽 모두에게 닫힌다)
	Claimed map[int]CSSide

	CurrentSide CSSide

	// ---- 턴 상태 ----
	// 임시 마커 위치 (이번 턴의 전진, 버스트하면 소멸)
	Temp map[int]int
	// 마지막 굴림 (조합 선택 대기 중일 때만 non-nil)
	Dice    []int
	Options []CSOption

	Winner    CSSide
	EndReason string // "claimed_three"

	Ready     bool
	StartedAt time.Time
}

// CSClient 캔트 스톱 클라이언트 연결
type CSClient struct {
	wsClient
	Hub  *CSHub
	Side CSSide
}

// CSMessage 메시지 봉투
type CSMessage struct {
	Type    CSMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type CSJoinGamePayload struct {
	PlayerName string `json:"playerName"`
}

type CSRejoinGamePayload struct {
	SessionID string `json:"sessionId"`
}

// CSChoosePayload 조합 선택 (서버가 준 Options 중 하나와 일치해야 한다)
type CSChoosePayload struct {
	Sums []int `json:"sums"`
}

// ==================== 서버 → 클라이언트 payload ====================

// CSGameStatePayload 게임 스냅샷. 완전 공개 정보라 YourSide 외에는 양측 동일하다.
type CSGameStatePayload struct {
	GameID      string  `json:"gameId"`
	YourSide    CSSide  `json:"yourSide"`
	Phase       CSPhase `json:"phase"`
	CurrentSide CSSide  `json:"currentSide"`
	SouthName   string  `json:"southName"`
	NorthName   string  `json:"northName"`

	SouthProgress map[int]int    `json:"southProgress"`
	NorthProgress map[int]int    `json:"northProgress"`
	Claimed       map[int]CSSide `json:"claimed"`

	// 현재 턴의 임시 마커·굴림·선택지 (공개 정보)
	Temp    map[int]int `json:"temp"`
	Dice    []int       `json:"dice,omitempty"`
	Options []CSOption  `json:"options,omitempty"`
	CanRoll bool        `json:"canRoll"`
	CanStop bool        `json:"canStop"`

	OpponentConnected bool `json:"opponentConnected"`
}

// CSEventPayload 연출용 이벤트
type CSEventPayload struct {
	Kind string `json:"kind"` // "roll" | "advance" | "bust" | "bank" | "claim"
	Side CSSide `json:"side"`
	Dice []int  `json:"dice,omitempty"`
	Sums []int  `json:"sums,omitempty"`
	Col  int    `json:"col,omitempty"`
}

// CSGameOverPayload 게임 종료
type CSGameOverPayload struct {
	Winner     CSSide `json:"winner"`
	WinnerName string `json:"winnerName"`
	Reason     string `json:"reason"`
	// 승자가 가져간 컬럼들
	ClaimedCols []int `json:"claimedCols"`
}
