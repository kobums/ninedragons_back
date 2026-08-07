package server

import (
	"time"

	"github.com/gorilla/websocket"
)

// ==================== Jekyll vs Hyde Game Types ====================

// JHRole 지킬/하이드 역할
type JHRole string

const (
	JHJekyll JHRole = "jekyll"
	JHHyde   JHRole = "hyde"
)

// JHSuit 카드 수트. 물약은 색이 없는 특수 수트로 취급한다.
type JHSuit string

const (
	JHPride  JHSuit = "pride"  // 오만 (보라)
	JHWrath  JHSuit = "wrath"  // 분노 (빨강)
	JHGreed  JHSuit = "greed"  // 탐욕 (초록)
	JHPotion JHSuit = "potion" // 물약 (무색)
)

// jhEvilSuits 악 카드 3개 수트
var jhEvilSuits = []JHSuit{JHPride, JHWrath, JHGreed}

const (
	JHEvilMaxValue    = 7  // 악 카드 숫자 1~7
	JHPotionMinValue  = 2  // 물약 2+~5+
	JHPotionMaxValue  = 5
	JHHandSize        = 10 // 라운드당 손패
	JHRounds          = 3  // 총 라운드
	JHTrackLength     = 10 // 정체성 트랙 이동 칸 수 (0=지킬 홈, 10=하이드 홈)
	JHTrackJekyllHalf = 5  // 마커가 이 값 이하면 지킬 진영 → 지킬이 선
)

// JHPhase 게임 진행 단계. 서버가 지금 어떤 입력을 기다리는지 명시한다.
type JHPhase string

const (
	JHPhaseLobby         JHPhase = "lobby"
	JHPhaseExchange      JHPhase = "exchange"       // 라운드 시작 카드 교환 (동시 제출 대기)
	JHPhaseLead          JHPhase = "lead"           // 선 플레이어의 카드 내기 대기
	JHPhaseDeclare       JHPhase = "declare"        // 물약 리드 후 색 선언 대기
	JHPhaseFollow        JHPhase = "follow"         // 팔로워의 카드 내기 대기
	JHPhasePrideSteal    JHPhase = "pride_steal"    // 오만 효과: 승자의 강탈 트릭 선택 대기
	JHPhaseGreedExchange JHPhase = "greed_exchange" // 탐욕 효과: 양쪽 동시 카드 선택 대기
	JHPhaseGameOver      JHPhase = "game_over"
)

// JHMessageType 지킬 앤 하이드 메시지 타입
type JHMessageType string

const (
	// 클라이언트 → 서버
	JHMsgJoinGame      JHMessageType = "jh_join_game"
	JHMsgRejoinGame    JHMessageType = "jh_rejoin_game"
	JHMsgExchangeCards JHMessageType = "jh_exchange_cards"
	JHMsgPlayCard      JHMessageType = "jh_play_card"
	JHMsgDeclareSuit   JHMessageType = "jh_declare_suit"
	JHMsgStealTrick    JHMessageType = "jh_steal_trick"
	JHMsgGreedCards    JHMessageType = "jh_greed_cards"

	// 서버 → 클라이언트
	JHMsgPlayerJoined         JHMessageType = "jh_player_joined"
	JHMsgWaitingPlayer        JHMessageType = "jh_waiting_player"
	JHMsgGameStart            JHMessageType = "jh_game_start"
	JHMsgGameState            JHMessageType = "jh_game_state"
	JHMsgEvent                JHMessageType = "jh_event"
	JHMsgGameOver             JHMessageType = "jh_game_over"
	JHMsgOpponentDisconnected JHMessageType = "jh_opponent_disconnected"
	JHMsgOpponentReconnected  JHMessageType = "jh_opponent_reconnected"
	JHMsgSessionExpired       JHMessageType = "jh_session_expired"
	JHMsgError                JHMessageType = "jh_error"
)

// JHCard 카드 하나. 악 카드는 Value 1~7, 물약은 2~5 이며 비교 시
// 같은 숫자보다 반 끗 높게 취급한다 (2+는 2를 이기고 3에게 진다).
type JHCard struct {
	ID    int    `json:"id"`
	Suit  JHSuit `json:"suit"`
	Value int    `json:"value"`
}

// IsPotion 물약 여부
func (c JHCard) IsPotion() bool { return c.Suit == JHPotion }

// JHTrick 완성된 트릭 (2장, 공개 정보)
type JHTrick struct {
	Lead   JHCard `json:"lead"`
	Follow JHCard `json:"follow"`
}

// JHRoundResult 라운드 정산 결과
type JHRoundResult struct {
	Round        int `json:"round"`
	JekyllTricks int `json:"jekyllTricks"`
	HydeTricks   int `json:"hydeTricks"`
	Moved        int `json:"moved"`
	Marker       int `json:"marker"`
}

// JHGame 게임 상태 (순수, 허브 비의존)
type JHGame struct {
	ID    string
	Names map[JHRole]string
	Phase JHPhase
	Round int // 1~3

	Hands  map[JHRole][]JHCard
	Tricks map[JHRole][]JHTrick

	// Marker 정체성 마커 위치. 0 = 지킬 홈(시작), JHTrackLength = 하이드 홈.
	// 하이드 쪽으로만 단조 증가한다.
	Marker int

	// RankOrder 이번 라운드에 등장한 악 수트, 등장 순서대로.
	// index 0 = 최하위 랭크. 분노 물약 효과로 비워질 수 있다.
	RankOrder []JHSuit

	Leader       JHRole  // 현재 트릭의 선
	TableLead    *JHCard // 테이블에 놓인 리드 카드
	TableFollow  *JHCard // 테이블에 놓인 팔로우 카드 (효과 처리 중에도 유지)
	DeclaredSuit JHSuit  // 물약 리드 시 선언한 색 ("" = 미선언)

	// ExchangeSel 라운드 시작 교환의 제출 상태 (역할 → 손패 인덱스)
	ExchangeSel map[JHRole][]int
	// GreedSel 탐욕 효과 교환의 제출 상태
	GreedSel map[JHRole][]int

	TrickWinner JHRole // 방금 끝난 트릭의 승자 (오만/탐욕 처리 중 참조)

	RoundResults []JHRoundResult
	Winner       JHRole // "" = 미정
	EndReason    string // "corrupted"(하이드 홈 도달) | "survived"(3라운드 버팀) | "forfeit"

	Ready     bool
	StartedAt time.Time

	rng           jhRng
	pendingEvents []JHEventPayload
}

// jhRng 셔플용 난수 인터페이스 (math/rand *Rand 가 만족)
type jhRng interface {
	Shuffle(n int, swap func(i, j int))
}

// JHClient 지킬 앤 하이드 클라이언트 연결
type JHClient struct {
	ID        string
	SessionID string // 연결이 아닌 플레이어 신원 식별자 (재접속 시 유지)
	Name      string
	Conn      *websocket.Conn
	Hub       *JHHub
	Send      chan []byte
	GameID    string
	Role      JHRole
	Connected bool // JHHub 고루틴에서만 접근
}

// JHMessage 메시지 봉투
type JHMessage struct {
	Type    JHMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type JHJoinGamePayload struct {
	PlayerName string `json:"playerName"`
}

type JHRejoinGamePayload struct {
	SessionID string `json:"sessionId"`
}

// JHExchangeCardsPayload 라운드 시작 교환 제출. Indices 는 손패 인덱스.
type JHExchangeCardsPayload struct {
	Indices []int `json:"indices"`
}

type JHPlayCardPayload struct {
	HandIndex int `json:"handIndex"`
}

type JHDeclareSuitPayload struct {
	Suit JHSuit `json:"suit"`
}

// JHStealTrickPayload 오만 강탈. TrickIndex 는 상대(패자) 트릭 더미 인덱스.
type JHStealTrickPayload struct {
	TrickIndex int `json:"trickIndex"`
}

type JHGreedCardsPayload struct {
	Indices []int `json:"indices"`
}

// ==================== 서버 → 클라이언트 payload ====================

type JHGameStartPayload struct {
	YourRole   JHRole `json:"yourRole"`
	JekyllName string `json:"jekyllName"`
	HydeName   string `json:"hydeName"`
}

// JHGameStatePayload 개인화된 전체 게임 스냅샷. 모든 상태 변경 후
// 역할마다 따로 만들어 보낸다. 재접속 복원도 같은 페이로드를 쓴다.
type JHGameStatePayload struct {
	GameID     string  `json:"gameId"`
	YourRole   JHRole  `json:"yourRole"`
	Phase      JHPhase `json:"phase"`
	Round      int     `json:"round"`
	Marker     int     `json:"marker"`
	JekyllName string  `json:"jekyllName"`
	HydeName   string  `json:"hydeName"`

	RankOrder    []JHSuit `json:"rankOrder"`
	Leader       JHRole   `json:"leader"`
	TableLead    *JHCard  `json:"tableLead,omitempty"`
	TableFollow  *JHCard  `json:"tableFollow,omitempty"`
	DeclaredSuit JHSuit   `json:"declaredSuit,omitempty"`

	YourHand     []JHCard `json:"yourHand"`
	OppHandCount int      `json:"oppHandCount"`

	YourTricks []JHTrick `json:"yourTricks"`
	OppTricks  []JHTrick `json:"oppTricks"`

	// YourTurn 지금 내 입력이 필요한지. 동시 제출 단계에서는 아직 제출하지
	// 않은 쪽이 true 다.
	YourTurn bool `json:"yourTurn"`
	// LegalIndices 내 입력이 카드 선택일 때 낼 수 있는 손패 인덱스
	LegalIndices []int `json:"legalIndices,omitempty"`

	ExchangeCount     int  `json:"exchangeCount"`
	MustIncludePotion bool `json:"mustIncludePotion"`
	YouSubmitted      bool `json:"youSubmitted"`
	OppSubmitted      bool `json:"oppSubmitted"`
	GreedPickCount    int  `json:"greedPickCount"`

	RoundResults      []JHRoundResult `json:"roundResults"`
	OpponentConnected bool            `json:"opponentConnected"`
}

// JHEventPayload 연출용 이벤트. 비밀 정보를 담지 않으며 양쪽에 동일하게 간다.
type JHEventPayload struct {
	Kind string `json:"kind"` // "round_start" | "card_played" | "suit_declared" | "trick_resolved" | "rank_reset" | "trick_stolen" | "greed_exchanged" | "round_result"
	Role JHRole `json:"role,omitempty"`
	// card_played / trick_resolved
	Card       *JHCard `json:"card,omitempty"`
	LeadCard   *JHCard `json:"leadCard,omitempty"`
	FollowCard *JHCard `json:"followCard,omitempty"`
	Winner     JHRole  `json:"winner,omitempty"`
	Effect     JHSuit  `json:"effect,omitempty"` // 발동한 물약 효과의 악 수트
	// suit_declared
	Suit JHSuit `json:"suit,omitempty"`
	// trick_stolen
	TrickIndex int `json:"trickIndex,omitempty"`
	// round_start / round_result
	Round        int `json:"round,omitempty"`
	JekyllTricks int `json:"jekyllTricks,omitempty"`
	HydeTricks   int `json:"hydeTricks,omitempty"`
	Moved        int `json:"moved,omitempty"`
	Marker       int `json:"marker,omitempty"`
}

type JHGameOverPayload struct {
	Winner       JHRole          `json:"winner"`
	Reason       string          `json:"reason"` // "corrupted" | "survived" | "forfeit"
	Marker       int             `json:"marker"`
	JekyllName   string          `json:"jekyllName"`
	HydeName     string          `json:"hydeName"`
	RoundResults []JHRoundResult `json:"roundResults"`
}

type JHOpponentDisconnectedPayload struct {
	Message      string `json:"message"`
	GraceSeconds int    `json:"graceSeconds"`
}

type JHErrorPayload struct {
	Message string `json:"message"`
}
