package server

import (
	"time"
)

// ==================== Geister Game Types ====================

// GSSide 진영. 남쪽이 아래(행 4~5), 북쪽이 위(행 0~1)에서 시작한다.
type GSSide string

const (
	GSSouth GSSide = "south"
	GSNorth GSSide = "north"
)

// gsOther 반대 진영
func gsOther(side GSSide) GSSide {
	if side == GSSouth {
		return GSNorth
	}
	return GSSouth
}

const (
	GSBoardSize  = 6 // 6x6 보드
	GSGhostCount = 8 // 진영당 유령 수 (좋은 4 + 나쁜 4)
	GSGoodCount  = 4
)

// GSPhase 게임 진행 단계
type GSPhase string

const (
	GSPhaseLobby    GSPhase = "lobby"
	GSPhaseSetup    GSPhase = "setup" // 양측 동시 비밀 배치 대기
	GSPhasePlay     GSPhase = "play"
	GSPhaseGameOver GSPhase = "game_over"
)

// GSMessageType 가이스터 메시지 타입
type GSMessageType string

const (
	// 클라이언트 → 서버
	GSMsgJoinGame    GSMessageType = "gs_join_game"
	GSMsgRejoinGame  GSMessageType = "gs_rejoin_game"
	GSMsgSubmitSetup GSMessageType = "gs_submit_setup"
	GSMsgMove        GSMessageType = "gs_move"
	GSMsgRematch     GSMessageType = "gs_rematch"

	// 서버 → 클라이언트
	GSMsgPlayerJoined         GSMessageType = "gs_player_joined"
	GSMsgWaitingPlayer        GSMessageType = "gs_waiting_player"
	GSMsgGameState            GSMessageType = "gs_game_state"
	GSMsgEvent                GSMessageType = "gs_event"
	GSMsgGameOver             GSMessageType = "gs_game_over"
	GSMsgRematchOffer         GSMessageType = "gs_rematch_offer"
	GSMsgOpponentDisconnected GSMessageType = "gs_opponent_disconnected"
	GSMsgOpponentReconnected  GSMessageType = "gs_opponent_reconnected"
	GSMsgSessionExpired       GSMessageType = "gs_session_expired"
	GSMsgError                GSMessageType = "gs_error"
)

// GSGhost 유령 하나 (내부 상태 — 직렬화는 뷰가 담당).
// Good(정체)은 소유자만 아는 비밀 정보다.
type GSGhost struct {
	ID       int
	Side     GSSide
	Good     bool
	Row      int
	Col      int
	Captured bool
	Escaped  bool
}

// GSGame 가이스터 게임 상태 (순수, 허브 비의존)
type GSGame struct {
	ID    string
	Names map[GSSide]string
	Phase GSPhase

	Ghosts []*GSGhost
	// SetupDone 진영별 비밀 배치 제출 여부
	SetupDone map[GSSide]bool

	CurrentSide GSSide
	Winner      GSSide
	EndReason   string // "escape" | "captured_all_good" | "fed_all_evil"

	Ready     bool
	StartedAt time.Time
}

// GSClient 가이스터 클라이언트 연결
type GSClient struct {
	wsClient
	Hub  *GSHub
	Side GSSide
}

// GSMessage 메시지 봉투
type GSMessage struct {
	Type    GSMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type GSJoinGamePayload struct {
	PlayerName string `json:"playerName"`
	// VsBot true 면 대기 슬롯을 거치지 않고 연습봇과 즉시 매칭
	VsBot bool `json:"vsBot,omitempty"`
}

type GSRejoinGamePayload struct {
	SessionID string `json:"sessionId"`
}

// GSCell 보드 좌표
type GSCell struct {
	Row int `json:"row"`
	Col int `json:"col"`
}

// GSSubmitSetupPayload 비밀 배치: 내 배치 구역 8칸 중 좋은 유령을 놓을 4칸
type GSSubmitSetupPayload struct {
	GoodCells []GSCell `json:"goodCells"`
}

// GSMovePayload 이동. Escape 가 true 면 From 의 유령을 보드 밖으로 탈출시킨다
// (To 는 무시). 탈출은 내 탈출구(상대편 모서리)에 있는 좋은 유령만 가능하다.
type GSMovePayload struct {
	From   GSCell `json:"from"`
	To     GSCell `json:"to"`
	Escape bool   `json:"escape,omitempty"`
}

// ==================== 서버 → 클라이언트 payload ====================

// GSGhostView 수신자 관점의 유령. Good 은 내 유령(또는 게임 종료 후)에만 실린다.
type GSGhostView struct {
	ID       int    `json:"id"`
	Side     GSSide `json:"side"`
	Row      int    `json:"row"`
	Col      int    `json:"col"`
	Good     *bool  `json:"good,omitempty"`
	Captured bool   `json:"captured,omitempty"`
	Escaped  bool   `json:"escaped,omitempty"`
}

// GSCapturedView 잡힌 유령 (색은 공개 정보)
type GSCapturedView struct {
	Good bool `json:"good"`
}

// GSGameStatePayload 개인화된 전체 게임 스냅샷
type GSGameStatePayload struct {
	GameID      string  `json:"gameId"`
	YourSide    GSSide  `json:"yourSide"`
	Phase       GSPhase `json:"phase"`
	CurrentSide GSSide  `json:"currentSide"`
	SouthName   string  `json:"southName"`
	NorthName   string  `json:"northName"`
	// setup 단계 진행 표시 (배치 내용은 절대 미노출)
	YourReady     bool `json:"yourReady"`
	OpponentReady bool `json:"opponentReady"`
	// 보드 위 유령들 (잡힘/탈출 제외)
	Ghosts []GSGhostView `json:"ghosts"`
	// 잡힌 유령 — 누가 잡았는지 기준 (승리 조건 카운트용, 색 공개)
	CapturedBySouth   []GSCapturedView `json:"capturedBySouth"`
	CapturedByNorth   []GSCapturedView `json:"capturedByNorth"`
	OpponentConnected bool             `json:"opponentConnected"`
}

// GSEventPayload 연출용 이벤트 (비밀 정보 없음)
type GSEventPayload struct {
	Kind string  `json:"kind"` // "move" | "capture" | "escape"
	Side GSSide  `json:"side"` // 행동한 진영
	From *GSCell `json:"from,omitempty"`
	To   *GSCell `json:"to,omitempty"`
	// capture: 잡힌 유령의 색 (공개 정보)
	Good *bool `json:"good,omitempty"`
}

// GSGameOverPayload 게임 종료 — 전 유령 정체 공개
type GSGameOverPayload struct {
	Winner     GSSide        `json:"winner"`
	WinnerName string        `json:"winnerName"`
	Reason     string        `json:"reason"`
	Ghosts     []GSGhostView `json:"ghosts"`
}
