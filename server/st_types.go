package server

import (
	"time"
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

// STHandSize 기본 손패 크기 (전술 변형은 7장)
const STHandSize = 6

// STTacticHandSize 전술 변형 손패 크기
const STTacticHandSize = 7

// STPhase 게임 진행 단계
type STPhase string

const (
	STPhaseLobby STPhase = "lobby"
	STPhasePlay  STPhase = "play"  // 현재 턴 플레이어의 카드 내기 대기
	STPhaseClaim STPhase = "claim" // 카드를 낸 뒤 돌 획득/턴 종료 선택 대기
	// STPhaseDraw 전술 변형: 클랜/전술 덱 중 드로우할 덱 선택 대기
	STPhaseDraw STPhase = "draw"
	// STPhaseRecruiterDraw 모병관: 3장 뽑기 진행 중
	STPhaseRecruiterDraw STPhase = "recruiter_draw"
	// STPhaseRecruiterReturn 모병관: 2장 덱 밑으로 반납 진행 중
	STPhaseRecruiterReturn STPhase = "recruiter_return"
	STPhaseGameOver        STPhase = "game_over"
)

// STTactic 전술 카드 종류. "" 는 일반 클랜 카드.
type STTactic string

const (
	STTacticNone STTactic = ""
	// 정예병 (클랜 카드처럼 돌 옆에 낸다)
	STTacticJoker  STTactic = "joker"  // 색·숫자 자유 (진영당 1장만)
	STTacticSpy    STTactic = "spy"    // 숫자 7 고정, 색 자유
	STTacticShield STTactic = "shield" // 숫자 1~3, 색 자유
	// 전투 모드 (돌 타일 위에 낸다)
	STTacticBlind STTactic = "blind" // 눈가리개: 그 돌은 합계로만 비교
	STTacticMud   STTactic = "mud"   // 진흙탕: 그 돌은 양쪽 4장 필요
	// 계략 (사용 후 버린 더미로)
	STTacticRecruiter  STTactic = "recruiter"  // 3장 뽑고 2장 덱 밑으로
	STTacticStrategist STTactic = "strategist" // 내 카드 이동 또는 버림
	STTacticBanshee    STTactic = "banshee"    // 상대 카드 버림
	STTacticTraitor    STTactic = "traitor"    // 상대 클랜 카드 강탈
)

// STMessageType 쇼텐토텐 메시지 타입
type STMessageType string

const (
	// 클라이언트 → 서버
	STMsgJoinGame        STMessageType = "st_join_game"
	STMsgRejoinGame      STMessageType = "st_rejoin_game"
	STMsgPlayCard        STMessageType = "st_play_card"
	STMsgPlayRuse        STMessageType = "st_play_ruse"
	STMsgClaimStone      STMessageType = "st_claim_stone"
	STMsgEndTurn         STMessageType = "st_end_turn"
	STMsgDraw            STMessageType = "st_draw"
	STMsgPass            STMessageType = "st_pass"
	STMsgRecruiterDraw   STMessageType = "st_recruiter_draw"
	STMsgRecruiterReturn STMessageType = "st_recruiter_return"
	STMsgRematch         STMessageType = "st_rematch"

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
	STMsgRematchOffer         STMessageType = "st_rematch_offer"
	STMsgError                STMessageType = "st_error"
)

// STCard 카드 한 장. 클랜 카드는 Color 0~5, Rank 1~9 에 Tactic 이 비고,
// 전술 카드는 Tactic 만 유효하다 (Color/Rank 0).
type STCard struct {
	Color  int      `json:"color"`
	Rank   int      `json:"rank"`
	Tactic STTactic `json:"tactic,omitempty"`
}

// IsClan 일반 클랜 카드인지
func (c STCard) IsClan() bool { return c.Tactic == STTacticNone }

// IsElite 정예병(돌 옆에 클랜 카드처럼 내는 전술)인지
func (c STCard) IsElite() bool {
	return c.Tactic == STTacticJoker || c.Tactic == STTacticSpy || c.Tactic == STTacticShield
}

// IsCombat 전투 모드(돌 타일 위에 내는 전술)인지
func (c STCard) IsCombat() bool {
	return c.Tactic == STTacticBlind || c.Tactic == STTacticMud
}

// IsRuse 계략(사용 후 버리는 전술)인지
func (c STCard) IsRuse() bool {
	return c.Tactic == STTacticRecruiter || c.Tactic == STTacticStrategist ||
		c.Tactic == STTacticBanshee || c.Tactic == STTacticTraitor
}

// STStone 국경석 하나의 상태
type STStone struct {
	Cards map[STSide][]STCard
	Owner STSide // "" = 미획득
	Blind bool   // 눈가리개: 합계로만 비교
	Mud   bool   // 진흙탕: 양쪽 4장 필요
	// CompletedOrder 각 진영이 필요 장수를 완성한 순서 (전역 카운터 값,
	// 0 = 미완성). 족보·합이 모두 같을 때 먼저 완성한 쪽이 이기는
	// 타이브레이크에 쓰인다.
	CompletedOrder map[STSide]int
}

// Required 이 돌을 차지하는 데 필요한 한쪽 카드 장수
func (s *STStone) Required() int {
	if s.Mud {
		return 4
	}
	return 3
}

// STGame 쇼텐토텐 게임 상태 (순수, 허브 비의존)
type STGame struct {
	ID          string
	Names       map[STSide]string
	TacticMode  bool
	Deck        []STCard // 클랜 덱
	TacticDeck  []STCard
	Discard     []STCard // 공개 버린 더미 (계략·밴시 등)
	Hands       map[STSide][]STCard
	Stones      []*STStone
	Phase       STPhase
	CurrentSide STSide
	Winner      STSide // "" = 미정 (무승부 종료 포함)
	EndReason   string // "five_stones" | "three_adjacent" | "stalemate" | "forfeit"
	// PlayedTactics 진영별 전술 카드 사용 수 — 상대보다 1장 초과 사용 금지
	PlayedTactics map[STSide]int
	// PassStreak 연속 패스 수 (2회면 교착 종료)
	PassStreak int
	// RecruiterDraws / RecruiterReturns 모병관 진행 상태
	RecruiterDraws   int
	RecruiterReturns int
	// completionCounter 돌 한쪽 완성 순서를 매기는 전역 카운터
	completionCounter int
	Ready             bool
	StartedAt         time.Time
}

// STClient 쇼텐토텐 클라이언트 연결
type STClient struct {
	wsClient
	Hub  *STHub
	Side STSide
}

// STMessage 메시지 봉투
type STMessage struct {
	Type    STMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type STJoinGamePayload struct {
	PlayerName string `json:"playerName"`
	Mode       string `json:"mode,omitempty"` // "basic" | "tactic" (기본 basic)
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

// STPlayRusePayload 계략 카드 사용.
// 모병관: HandIndex 만. 밴시: From*. 전략가: From* + ToStone(-1 = 버리기).
// 배신자: From*(상대 클랜 카드) + ToStone(내 쪽 목적지).
type STPlayRusePayload struct {
	HandIndex int `json:"handIndex"`
	FromStone int `json:"fromStone"`
	FromIndex int `json:"fromIndex"`
	ToStone   int `json:"toStone"`
}

// STDrawPayload 드로우할 덱 선택
type STDrawPayload struct {
	Deck string `json:"deck"` // "clan" | "tactic"
}

type STRecruiterDrawPayload struct {
	Deck string `json:"deck"` // "clan" | "tactic"
}

type STRecruiterReturnPayload struct {
	HandIndex int `json:"handIndex"`
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
	Index     int      `json:"index"`
	YourCards []STCard `json:"yourCards"`
	OppCards  []STCard `json:"oppCards"`
	Owner     string   `json:"owner"` // "you" | "opponent" | ""
	Claimable bool     `json:"claimable"`
	Blind     bool     `json:"blind"`
	Mud       bool     `json:"mud"`
	Required  int      `json:"required"`
}

// STGameStatePayload 개인화된 전체 게임 스냅샷. 모든 상태 변경 후
// 진영마다 따로 만들어 보낸다. 재접속 복원도 같은 페이로드를 쓴다.
type STGameStatePayload struct {
	GameID            string        `json:"gameId"`
	YourSide          STSide        `json:"yourSide"`
	TacticMode        bool          `json:"tacticMode"`
	Phase             STPhase       `json:"phase"`
	CurrentSide       STSide        `json:"currentSide"`
	DeckCount         int           `json:"deckCount"`
	TacticDeckCount   int           `json:"tacticDeckCount"`
	Discard           []STCard      `json:"discard,omitempty"`
	YourHand          []STCard      `json:"yourHand"`
	OpponentHandCount int           `json:"opponentHandCount"`
	Stones            []STStoneView `json:"stones"`
	SouthName         string        `json:"southName"`
	NorthName         string        `json:"northName"`
	YourStoneCount    int           `json:"yourStoneCount"`
	OppStoneCount     int           `json:"oppStoneCount"`
	YourPlayedTactics int           `json:"yourPlayedTactics"`
	OppPlayedTactics  int           `json:"oppPlayedTactics"`
	CanPass           bool          `json:"canPass"`
	RecruiterDraws    int           `json:"recruiterDraws"`
	RecruiterReturns  int           `json:"recruiterReturns"`
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
