package server

import "time"

// ==================== 스컬킹 (Skull King) 타입 ====================
//
// 2~8인 비드 트릭테이킹(해적). prefix 는 kg 다 — sk 는 기존 게임 스컬(Skull)이
// 이미 쓰고 있어 겹치면 안 된다.
//
// 한 라운드는 두 단계다.
//   1) 비딩: 전원이 "이번 라운드에 몇 트릭을 먹겠다"를 비공개로 동시 제출한다.
//      전원 제출되면 일괄 공개한다 (45초 방치는 0으로 자동 제출).
//   2) 플레이: 라운드 번호만큼 트릭을 친다. 리드 무늬 따라내기 의무가 있고,
//      특수 카드(탈출·해적·인어·스컬킹)는 언제든 낼 수 있다.
//
// 은닉의 심장은 kg_hub.go 의 buildKGState 다. yourHand·yourBid 는 본인
// 스냅샷에만 실리고, 타인·관전자의 raw JSON 에는 키 자체가 없다.
// players[].bid 는 비딩이 끝나기 전까지 전원 -1 이다.

const (
	KGMinPlayers = 2
	KGMaxPlayers = 8

	// KGFillBotTarget kg_fill_bots 가 채우는 목표 인원 — 채운 뒤 즉시 시작
	KGFillBotTarget = 5

	// KGMaxRoundCap 라운드 상한 (원작 10라운드)
	KGMaxRoundCap = 10

	// KGDeckLimit 인원별 최대 라운드를 정하는 덱 한계.
	//
	// 스펙이 못 박은 값이다: maxRound = min(10, 66 / 인원) →
	// 8인 8라운드 · 7인 9라운드 · 6인 이하 10라운드.
	// 실제로 만들어지는 덱은 숫자 52장 + 특수 13장 = 65장이라 한계보다 한 장
	// 적지만, 최대 소요 장수는 8인×8라운드 = 64장이라 절대 모자라지 않는다
	// (7인 9라운드 63장, 6인 10라운드 60장).
	KGDeckLimit = 66

	// 덱 구성 — 숫자 4색 × 1~13 = 52장 + 특수 13장 = 65장
	KGSuitRankMax   = 13
	KGEscapeCount   = 5
	KGPirateCount   = 5
	KGMermaidCount  = 2
	KGSkullKingCard = 1
)

// KGCardKind 카드 종류 (와이어 값)
type KGCardKind string

const (
	KGKindNumber    KGCardKind = "number"
	KGKindEscape    KGCardKind = "escape"
	KGKindPirate    KGCardKind = "pirate"
	KGKindMermaid   KGCardKind = "mermaid"
	KGKindSkullKing KGCardKind = "skullking"
)

// KGSuit 숫자 카드의 무늬. 검정(해적기)은 상시 트럼프다.
type KGSuit string

const (
	KGSuitNone   KGSuit = ""
	KGSuitGreen  KGSuit = "green"  // 앵무새
	KGSuitYellow KGSuit = "yellow" // 지도
	KGSuitPurple KGSuit = "purple" // 보물상자
	KGSuitBlack  KGSuit = "black"  // 해적기 (트럼프)
)

// kgSuits 덱 생성 순서 (검정을 마지막에 둔다)
var kgSuits = []KGSuit{KGSuitGreen, KGSuitYellow, KGSuitPurple, KGSuitBlack}

// kgSuitLabel 무늬 한글 표기 (이벤트·로그 문구용)
func kgSuitLabel(suit KGSuit) string {
	switch suit {
	case KGSuitGreen:
		return "초록"
	case KGSuitYellow:
		return "노랑"
	case KGSuitPurple:
		return "보라"
	case KGSuitBlack:
		return "검정"
	default:
		return "무늬없음"
	}
}

// KGCard 카드 한 장. 특수 카드는 suit "" · rank 0 이다.
// rank 0·좌석 0 유실을 막기 위해 omitempty 를 쓰지 않는다.
type KGCard struct {
	Kind KGCardKind `json:"kind"`
	Suit KGSuit     `json:"suit"`
	Rank int        `json:"rank"`
}

// kgCardLabel 카드 한글 표기 (이벤트·로그 문구용)
func kgCardLabel(c KGCard) string {
	switch c.Kind {
	case KGKindEscape:
		return "탈출"
	case KGKindPirate:
		return "해적"
	case KGKindMermaid:
		return "인어"
	case KGKindSkullKing:
		return "스컬킹"
	default:
		return kgSuitLabel(c.Suit) + " " + kgRankText(c.Rank)
	}
}

// kgRankText 숫자 표기 (1~13)
func kgRankText(rank int) string {
	const digits = "0123456789"
	if rank <= 0 {
		return "0"
	}
	if rank < 10 {
		return string(digits[rank])
	}
	return string(digits[rank/10]) + string(digits[rank%10])
}

// KGPhase 게임 진행 단계
type KGPhase string

const (
	KGPhaseWaiting  KGPhase = "waiting"
	KGPhaseBidding  KGPhase = "bidding"   // 전원 동시 비공개 제출 (45초 마감)
	KGPhasePlaying  KGPhase = "playing"   // 트릭 진행 (좌석별 45초 마감)
	KGPhaseRoundEnd KGPhase = "round_end" // 라운드 정산 (5초 뒤 다음 라운드)
	KGPhaseGameOver KGPhase = "game_over"
)

// KGMessageType 스컬킹 메시지 타입
type KGMessageType string

const (
	// 클라이언트 → 서버
	KGMsgJoinGame KGMessageType = "kg_join_game"
	KGMsgFillBots KGMessageType = "kg_fill_bots"
	KGMsgStart    KGMessageType = "kg_start"
	KGMsgRejoin   KGMessageType = "kg_rejoin"
	KGMsgBid      KGMessageType = "kg_bid"
	KGMsgPlay     KGMessageType = "kg_play"
	KGMsgReact    KGMessageType = "kg_react"

	// 서버 → 클라이언트
	KGMsgPlayerJoined       KGMessageType = "kg_player_joined"
	KGMsgSpectateJoined     KGMessageType = "kg_spectate_joined"
	KGMsgGameState          KGMessageType = "kg_game_state"
	KGMsgEvent              KGMessageType = "kg_event"
	KGMsgGameOver           KGMessageType = "kg_game_over"
	KGMsgPlayerDisconnected KGMessageType = "kg_player_disconnected"
	KGMsgPlayerReconnected  KGMessageType = "kg_player_reconnected"
	KGMsgSessionExpired     KGMessageType = "kg_session_expired"
	KGMsgError              KGMessageType = "kg_error"
)

// KGTrickPlay 트릭에 낸 카드 한 장 (전원 공개)
type KGTrickPlay struct {
	Seat int    `json:"seat"`
	Card KGCard `json:"card"`
}

// KGLastTrick 직전 트릭 결과 (연출·재접속 복원용)
type KGLastTrick struct {
	WinnerSeat int           `json:"winnerSeat"`
	Cards      []KGTrickPlay `json:"cards"`
}

// KGRoundRow 라운드 정산표 한 줄
type KGRoundRow struct {
	Seat   int `json:"seat"`
	Bid    int `json:"bid"`
	Tricks int `json:"tricks"`
	Delta  int `json:"delta"`
	Total  int `json:"total"`
}

// KGRoundResult 라운드 정산 (전원 공개, 그 전엔 null)
type KGRoundResult struct {
	Rows    []KGRoundRow `json:"rows"`
	Message string       `json:"message"`
}

// KGPlayer 게임 참가자 (순수 상태 — 연결 매핑은 허브의 kgRoom 담당)
type KGPlayer struct {
	Seat int
	Name string
	// Hand 이번 라운드의 남은 손패 (본인만 내용을 본다)
	Hand []KGCard
	// Bid 이번 라운드 비드 (-1 미제출). 비딩 중에는 스냅샷에 실리지 않는다.
	Bid int
	// Tricks 이번 라운드에 딴 트릭 수
	Tricks int
	// Bonus 이번 라운드에 쌓은 보너스 (비드를 맞혔을 때만 가산된다)
	Bonus int
	// Score 누계 점수
	Score int
}

// KGGameEvent 순수 규칙이 쌓는 연출 이벤트 — 허브가 DrainEvents 로 꺼내
// kg_event 로 방송한다 (손패·미공개 비드는 절대 담지 않는다)
type KGGameEvent struct {
	Kind    string
	Seat    int // -1 없음
	Message string
}

// KGGame 스컬킹 게임 상태 (순수, 허브 비의존)
type KGGame struct {
	ID      string
	Players []*KGPlayer
	Phase   KGPhase

	// Round 현재 라운드 (1~MaxRound, 시작 전 0)
	Round int
	// MaxRound 인원별 최대 라운드 (min(10, 66/인원))
	MaxRound int
	// TrickNo 이번 라운드의 트릭 번호 (1~Round)
	TrickNo int
	// CurrentSeat 지금 낼 차례 (-1 없음). 비딩 단계에서는 -1 이다.
	CurrentSeat int
	// LeadSeat 이번 트릭을 리드한 좌석 (-1 없음)
	LeadSeat int
	// LeadSuit 이번 트릭의 리드 무늬 — 첫 숫자 카드가 정한다 ("" 미정)
	LeadSuit KGSuit
	// Trick 이번 트릭에 나온 카드들 (전원 공개)
	Trick []KGTrickPlay
	// LastTrick 직전 트릭 결과 (그 전엔 nil)
	LastTrick *KGLastTrick
	// RoundResult 직전 라운드 정산 (그 전엔 nil)
	RoundResult *KGRoundResult
	// BidsRevealed 이번 라운드의 비드가 공개됐는지
	BidsRevealed bool
	// StartSeat 1라운드 선 좌석 (라운드마다 한 칸씩 밀린다)
	StartSeat int
	// Winners 종료 시 공동 1위 좌석들 (그 전엔 빈 슬라이스)
	Winners []int

	Ready     bool
	StartedAt time.Time

	// StateSeq 새 대기 상태(비딩 개시·차례 이동·라운드 정산)가 열릴 때마다 +1 —
	// 허브가 마감 타이머를 다시 걸지 판단하는 근거
	StateSeq int
	// AfkSeq 마감 타이머 일련번호 (뒤늦은 발화 무시용 — 허브가 관리)
	AfkSeq int
	// Deadline 현재 대기 상태의 마감 시각 (unixMillis — 스냅샷 노출용)
	Deadline int64

	events []KGGameEvent
}

// KGClient 스컬킹 클라이언트 연결
type KGClient struct {
	wsClient
	Hub  *KGHub
	Seat int
}

// KGMessage 메시지 봉투
type KGMessage struct {
	Type    KGMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type KGJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type KGRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// KGBidPayload 비드 제출 — 0 유실을 막기 위해 omitempty 를 쓰지 않는다
type KGBidPayload struct {
	Bid int `json:"bid"`
}

// KGPlayPayload 손패 인덱스 — 인덱스 0 유실을 막기 위해 omitempty 금지
type KGPlayPayload struct {
	Index int `json:"index"`
}

type KGReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

// KGPlayerView 좌석별 공개 정보 — 좌석 0·점수 0 유실 방지를 위해 omitempty 금지.
// Bid 는 비딩 진행 중 전원 -1 이고, 공개 후에야 실제 값이 실린다.
type KGPlayerView struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	Connected    bool   `json:"connected"`
	Bot          bool   `json:"bot"`
	Bid          int    `json:"bid"`
	Tricks       int    `json:"tricks"`
	Score        int    `json:"score"`
	HandCount    int    `json:"handCount"`
	BidSubmitted bool   `json:"bidSubmitted"`
}

// KGGameStatePayload 개인화된 전체 게임 스냅샷. 모든 상태 변경 후 좌석마다
// 따로 만들어 보낸다. 재접속 복원도 같은 페이로드를 쓴다.
// 은닉: yourHand·yourBid 는 본인에게만 실린다 — 타인·관전자(viewerSeat -1)의
// raw JSON 에는 키 자체가 없다.
type KGGameStatePayload struct {
	GameID   string  `json:"gameId"`
	RoomCode string  `json:"roomCode"`
	Phase    KGPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"` // 관전자는 -1
	// Spectators 관전자 수 (참가자·관전자 모두 표시용)
	Spectators int `json:"spectators"`
	// EndsAt 현재 대기 상태(비딩·플레이·정산)의 마감 시각 (unixMillis, 그 외 0)
	EndsAt      int64  `json:"endsAt"`
	Round       int    `json:"round"`
	MaxRound    int    `json:"maxRound"`
	TrickNo     int    `json:"trickNo"`
	CurrentSeat int    `json:"currentSeat"`
	LeadSuit    KGSuit `json:"leadSuit"`
	// Trick 이번 트릭 진행분 — 항상 [] 로 나간다 (nil → JSON null 금지)
	Trick []KGTrickPlay `json:"trick"`
	// YourHand 본인 손패 — 본인에게만 (관전자 부재).
	// 빈 손패도 [] 로 나가야 하므로 슬라이스 포인터로 부재를 표현한다.
	YourHand *[]KGCard `json:"yourHand,omitempty"`
	// YourBid 본인 비드 — 본인에게만 (관전자 부재). 미제출은 -1 이라
	// 값으로 부재를 표현할 수 없어 포인터를 쓴다.
	YourBid     *int           `json:"yourBid,omitempty"`
	Players     []KGPlayerView `json:"players"`
	LastTrick   *KGLastTrick   `json:"lastTrick"`   // 그 전엔 null
	RoundResult *KGRoundResult `json:"roundResult"` // 그 전엔 null
}

// KGEventPayload 연출용 이벤트. 손패·미공개 비드를 담지 않으며 전원에게
// 동일하게 간다. Seat 은 좌석 0 유실 방지를 위해 포인터로 생략을 표현한다.
type KGEventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

// KGGameOverPayload 게임 종료 발표 — 총점 순위 (동점은 공동 우승)
type KGGameOverPayload struct {
	// Winners 공동 1위 좌석 (항상 1개 이상, 빈 슬라이스 금지)
	Winners []int `json:"winners"`
	// WinnerNames 공동 1위 이름 (· 로 이음)
	WinnerNames string         `json:"winnerNames"`
	Message     string         `json:"message"`
	Round       int            `json:"round"`
	MaxRound    int            `json:"maxRound"`
	Players     []KGPlayerView `json:"players"`
}

type KGPlayerDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type KGPlayerReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type KGErrorPayload struct {
	Message string `json:"message"`
}

// ==================== 인원별 최대 라운드 ====================

// kgMaxRound 인원 n 의 최대 라운드 — min(10, 66/n).
//
//	2~6인 10라운드 / 7인 9라운드 / 8인 8라운드
func kgMaxRound(n int) int {
	if n <= 0 {
		return KGMaxRoundCap
	}
	r := KGDeckLimit / n
	if r > KGMaxRoundCap {
		r = KGMaxRoundCap
	}
	if r < 1 {
		r = 1
	}
	return r
}
