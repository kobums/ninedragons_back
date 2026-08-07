package server

import (
	"time"

	"github.com/gorilla/websocket"
)

// ==================== Schotten Totten Game Types ====================

// STSide 국경의 남/북 진영
type STSide string

const (
	STSouth STSide = "south"
	STNorth STSide = "north"
)

// STStoneCount 국경석 개수
const STStoneCount = 9

// STColorCount 클랜 카드 색상 수 (0~5)
const STColorCount = 6

// STMaxRank 클랜 카드 최대 숫자 (1~9)
const STMaxRank = 9

// STHandSize 손패 크기
const STHandSize = 6

// STPhase 게임 진행 단계
type STPhase string

const (
	STPhaseLobby    STPhase = "lobby"
	STPhasePlay     STPhase = "play"  // 현재 턴 플레이어의 카드 내기 대기
	STPhaseClaim    STPhase = "claim" // 카드를 낸 뒤 돌 획득/턴 종료 선택 대기
	STPhaseGameOver STPhase = "game_over"
)

// STMessageType 쇼텐토텐 메시지 타입
type STMessageType string

const (
	// 클라이언트 → 서버
	STMsgJoinGame   STMessageType = "st_join_game"
	STMsgRejoinGame STMessageType = "st_rejoin_game"
	STMsgPlayCard   STMessageType = "st_play_card"
	STMsgClaimStone STMessageType = "st_claim_stone"
	STMsgEndTurn    STMessageType = "st_end_turn"

	// 서버 → 클라이언트
	STMsgPlayerJoined         STMessageType = "st_player_joined"
	STMsgWaitingPlayer        STMessageType = "st_waiting_player"
	STMsgGameStart            STMessageType = "st_game_start"
	STMsgGameState            STMessageType = "st_game_state"
	STMsgEvent                STMessageType = "st_event"
	STMsgGameOver             STMessageType = "st_game_over"
	STMsgOpponentDisconnected STMessageType = "st_opponent_disconnected"
	STMsgOpponentReconnected  STMessageType = "st_opponent_reconnected"
	STMsgSessionExpired       STMessageType = "st_session_expired"
	STMsgError                STMessageType = "st_error"
)

// STCard 클랜 카드. Color 0~5, Rank 1~9.
type STCard struct {
	Color int `json:"color"`
	Rank  int `json:"rank"`
}

// STStone 국경석 하나의 상태
type STStone struct {
	Cards map[STSide][]STCard
	Owner STSide // "" = 미획득
	// CompletedOrder 각 진영이 세 번째 카드를 완성한 순서 (전역 카운터 값,
	// 0 = 미완성). 족보·합이 모두 같을 때 먼저 완성한 쪽이 이기는
	// 타이브레이크에 쓰인다.
	CompletedOrder map[STSide]int
}

// STGame 쇼텐토텐 게임 상태 (순수, 허브 비의존)
type STGame struct {
	ID          string
	Names       map[STSide]string
	Deck        []STCard
	Hands       map[STSide][]STCard
	Stones      []*STStone
	Phase       STPhase
	CurrentSide STSide
	Winner      STSide // "" = 미정 (무승부 종료 포함)
	EndReason   string // "five_stones" | "three_adjacent" | "stalemate" | "forfeit"
	// completionCounter 돌 한쪽 3장 완성 순서를 매기는 전역 카운터
	completionCounter int
	Ready             bool
	StartedAt         time.Time
}

// STClient 쇼텐토텐 클라이언트 연결
type STClient struct {
	ID        string
	SessionID string // 연결이 아닌 플레이어 신원 식별자 (재접속 시 유지)
	Name      string
	Conn      *websocket.Conn
	Hub       *STHub
	Send      chan []byte
	GameID    string
	Side      STSide
	Connected bool // STHub 고루틴에서만 접근
}

// STMessage 메시지 봉투
type STMessage struct {
	Type    STMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type STJoinGamePayload struct {
	PlayerName string `json:"playerName"`
}

type STRejoinGamePayload struct {
	SessionID string `json:"sessionId"`
}

// STPlayCardPayload 카드 내기. HandIndex 는 손패 인덱스, StoneIndex 는 0~8.
type STPlayCardPayload struct {
	HandIndex  int `json:"handIndex"`
	StoneIndex int `json:"stoneIndex"`
}

type STClaimStonePayload struct {
	StoneIndex int `json:"stoneIndex"`
}

// ==================== 서버 → 클라이언트 payload ====================

// STGameStartPayload 게임 시작
type STGameStartPayload struct {
	YourSide  STSide `json:"yourSide"`
	FirstSide STSide `json:"firstSide"`
	SouthName string `json:"southName"`
	NorthName string `json:"northName"`
}

// STStoneView 돌 하나의 공개 상태. 돌에 놓인 카드는 모두 공개 정보다.
type STStoneView struct {
	Index      int      `json:"index"`
	YourCards  []STCard `json:"yourCards"`
	OppCards   []STCard `json:"oppCards"`
	Owner      string   `json:"owner"` // "you" | "opponent" | ""
	Claimable  bool     `json:"claimable"`
}

// STGameStatePayload 개인화된 전체 게임 스냅샷. 모든 상태 변경 후
// 진영마다 따로 만들어 보낸다. 재접속 복원도 같은 페이로드를 쓴다.
type STGameStatePayload struct {
	GameID            string        `json:"gameId"`
	YourSide          STSide        `json:"yourSide"`
	Phase             STPhase       `json:"phase"`
	CurrentSide       STSide        `json:"currentSide"`
	DeckCount         int           `json:"deckCount"`
	YourHand          []STCard      `json:"yourHand"`
	OpponentHandCount int           `json:"opponentHandCount"`
	Stones            []STStoneView `json:"stones"`
	SouthName         string        `json:"southName"`
	NorthName         string        `json:"northName"`
	YourStoneCount    int           `json:"yourStoneCount"`
	OppStoneCount     int           `json:"oppStoneCount"`
	OpponentConnected bool          `json:"opponentConnected"`
}

// STEventPayload 연출용 이벤트. 비밀 정보를 담지 않으며 전원에게 동일하게 간다.
type STEventPayload struct {
	Kind string `json:"kind"` // "card_played" | "stone_claimed" | "turn_passed"
	Side STSide `json:"side"`
	// card_played
	StoneIndex int     `json:"stoneIndex,omitempty"`
	Card       *STCard `json:"card,omitempty"`
}

// STGameOverPayload 게임 종료
type STGameOverPayload struct {
	Winner     string `json:"winner"` // "south" | "north" | "" (무승부)
	Reason     string `json:"reason"` // "five_stones" | "three_adjacent" | "stalemate" | "forfeit"
	SouthName  string `json:"southName"`
	NorthName  string `json:"northName"`
	SouthCount int    `json:"southCount"`
	NorthCount int    `json:"northCount"`
}

type STOpponentDisconnectedPayload struct {
	Message      string `json:"message"`
	GraceSeconds int    `json:"graceSeconds"`
}

type STErrorPayload struct {
	Message string `json:"message"`
}
