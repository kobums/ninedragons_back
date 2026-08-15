package server

import (
	"time"
)

// ==================== Onitama Game Types ====================

// OTSide 진영. 남쪽이 아래(행 4)에서 위로, 북쪽이 위(행 0)에서 아래로 전진한다.
type OTSide string

const (
	OTSouth OTSide = "south"
	OTNorth OTSide = "north"
)

// otOther 반대 진영
func otOther(side OTSide) OTSide {
	if side == OTSouth {
		return OTNorth
	}
	return OTSouth
}

const (
	OTBoardSize  = 5 // 5x5 보드
	OTHandSize   = 2 // 손에 쥔 카드 수
	OTPieceCount = 5 // 진영당 기물 수 (마스터 1 + 제자 4)
)

// OTPhase 게임 진행 단계 (완전 공개 정보 — 배치 단계 없음)
type OTPhase string

const (
	OTPhaseLobby    OTPhase = "lobby"
	OTPhasePlay     OTPhase = "play"
	OTPhaseGameOver OTPhase = "game_over"
)

// OTMessageType 오니타마 메시지 타입
type OTMessageType string

const (
	// 클라이언트 → 서버
	OTMsgJoinGame   OTMessageType = "ot_join_game"
	OTMsgRejoinGame OTMessageType = "ot_rejoin_game"
	OTMsgMove       OTMessageType = "ot_move"
	OTMsgPass       OTMessageType = "ot_pass"

	// 서버 → 클라이언트
	OTMsgPlayerJoined         OTMessageType = "ot_player_joined"
	OTMsgWaitingPlayer        OTMessageType = "ot_waiting_player"
	OTMsgGameState            OTMessageType = "ot_game_state"
	OTMsgEvent                OTMessageType = "ot_event"
	OTMsgGameOver             OTMessageType = "ot_game_over"
	OTMsgOpponentDisconnected OTMessageType = "ot_opponent_disconnected"
	OTMsgOpponentReconnected  OTMessageType = "ot_opponent_reconnected"
	OTMsgSessionExpired       OTMessageType = "ot_session_expired"
	OTMsgError                OTMessageType = "ot_error"
)

// OTCell 보드 좌표
type OTCell struct {
	Row int `json:"row"`
	Col int `json:"col"`
}

// OTOffset 카드의 이동 오프셋 — 남쪽 시점 기준 (Forward: 상대 방향 +, Right: 오른쪽 +).
// 북쪽은 180도 회전(둘 다 부호 반전)해서 적용한다.
type OTOffset struct {
	Forward int `json:"forward"`
	Right   int `json:"right"`
}

// OTCardDef 이동 카드 정의 (16종 원판)
type OTCardDef struct {
	Name  string     `json:"name"`  // 영문 키
	Label string     `json:"label"` // 한글 표기
	Moves []OTOffset `json:"moves"`
}

// otCardDeck 원판 카드 16종. 오프셋은 남쪽 시점.
var otCardDeck = []OTCardDef{
	{Name: "tiger", Label: "호랑이", Moves: []OTOffset{{2, 0}, {-1, 0}}},
	{Name: "crab", Label: "게", Moves: []OTOffset{{1, 0}, {0, -2}, {0, 2}}},
	{Name: "monkey", Label: "원숭이", Moves: []OTOffset{{1, -1}, {1, 1}, {-1, -1}, {-1, 1}}},
	{Name: "crane", Label: "학", Moves: []OTOffset{{1, 0}, {-1, -1}, {-1, 1}}},
	{Name: "dragon", Label: "용", Moves: []OTOffset{{1, -2}, {1, 2}, {-1, -1}, {-1, 1}}},
	{Name: "elephant", Label: "코끼리", Moves: []OTOffset{{1, -1}, {1, 1}, {0, -1}, {0, 1}}},
	{Name: "mantis", Label: "사마귀", Moves: []OTOffset{{1, -1}, {1, 1}, {-1, 0}}},
	{Name: "boar", Label: "멧돼지", Moves: []OTOffset{{1, 0}, {0, -1}, {0, 1}}},
	{Name: "frog", Label: "개구리", Moves: []OTOffset{{0, -2}, {1, -1}, {-1, 1}}},
	{Name: "rabbit", Label: "토끼", Moves: []OTOffset{{1, 1}, {0, 2}, {-1, -1}}},
	{Name: "goose", Label: "거위", Moves: []OTOffset{{1, -1}, {0, -1}, {0, 1}, {-1, 1}}},
	{Name: "rooster", Label: "수탉", Moves: []OTOffset{{1, 1}, {0, 1}, {0, -1}, {-1, -1}}},
	{Name: "horse", Label: "말", Moves: []OTOffset{{1, 0}, {0, -1}, {-1, 0}}},
	{Name: "ox", Label: "황소", Moves: []OTOffset{{1, 0}, {0, 1}, {-1, 0}}},
	{Name: "eel", Label: "뱀장어", Moves: []OTOffset{{1, -1}, {0, 1}, {-1, -1}}},
	{Name: "cobra", Label: "코브라", Moves: []OTOffset{{1, 1}, {0, -1}, {-1, 1}}},
}

// otCardByName 이름으로 카드 정의 찾기
func otCardByName(name string) *OTCardDef {
	for i := range otCardDeck {
		if otCardDeck[i].Name == name {
			return &otCardDeck[i]
		}
	}
	return nil
}

// OTPiece 기물 하나 (전부 공개 정보)
type OTPiece struct {
	ID       int    `json:"id"`
	Side     OTSide `json:"side"`
	Master   bool   `json:"master"`
	Row      int    `json:"row"`
	Col      int    `json:"col"`
	Captured bool   `json:"captured,omitempty"`
}

// OTGame 오니타마 게임 상태 (순수, 허브 비의존)
type OTGame struct {
	ID    string
	Names map[OTSide]string
	Phase OTPhase

	Pieces []*OTPiece
	// Hands 진영별 손 카드 2장 + 대기 카드 1장 (전부 공개)
	Hands       map[OTSide][]string
	WaitingCard string

	CurrentSide OTSide
	Winner      OTSide
	EndReason   string // "capture_master" | "reach_temple"

	Ready     bool
	StartedAt time.Time
}

// OTClient 오니타마 클라이언트 연결
type OTClient struct {
	wsClient
	Hub  *OTHub
	Side OTSide
}

// OTMessage 메시지 봉투
type OTMessage struct {
	Type    OTMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type OTJoinGamePayload struct {
	PlayerName string `json:"playerName"`
	// VsBot true 면 대기 슬롯을 거치지 않고 연습봇과 즉시 매칭
	VsBot bool `json:"vsBot,omitempty"`
}

type OTRejoinGamePayload struct {
	SessionID string `json:"sessionId"`
}

// OTMovePayload 카드로 기물 이동
type OTMovePayload struct {
	Card string `json:"card"`
	From OTCell `json:"from"`
	To   OTCell `json:"to"`
}

// OTPassPayload 둘 수 있는 수가 없을 때 카드만 교환
type OTPassPayload struct {
	Card string `json:"card"`
}

// ==================== 서버 → 클라이언트 payload ====================

// OTLegalMove 현재 차례 진영의 합법 수 하나
type OTLegalMove struct {
	Card string `json:"card"`
	From OTCell `json:"from"`
	To   OTCell `json:"to"`
}

// OTGameStatePayload 게임 스냅샷. 완전 공개 정보라 YourSide 외에는 양측 동일하다.
type OTGameStatePayload struct {
	GameID      string  `json:"gameId"`
	YourSide    OTSide  `json:"yourSide"`
	Phase       OTPhase `json:"phase"`
	CurrentSide OTSide  `json:"currentSide"`
	SouthName   string  `json:"southName"`
	NorthName   string  `json:"northName"`

	Pieces      []OTPiece `json:"pieces"`
	SouthHand   []string  `json:"southHand"`
	NorthHand   []string  `json:"northHand"`
	WaitingCard string    `json:"waitingCard"`
	// 현재 차례 진영의 합법 수 전체. 비어 있으면 패스(카드 교환)만 가능.
	LegalMoves []OTLegalMove `json:"legalMoves"`

	OpponentConnected bool `json:"opponentConnected"`
}

// OTEventPayload 연출용 이벤트
type OTEventPayload struct {
	Kind string  `json:"kind"` // "move" | "capture" | "pass"
	Side OTSide  `json:"side"` // 행동한 진영
	Card string  `json:"card"`
	From *OTCell `json:"from,omitempty"`
	To   *OTCell `json:"to,omitempty"`
	// capture: 잡힌 기물이 마스터였는지
	Master *bool `json:"master,omitempty"`
}

// OTGameOverPayload 게임 종료
type OTGameOverPayload struct {
	Winner     OTSide `json:"winner"`
	WinnerName string `json:"winnerName"`
	Reason     string `json:"reason"`
}
