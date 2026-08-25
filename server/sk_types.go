package server

import (
	"time"
)

// ==================== 스컬 타입 ====================
//
// 3~6인 베팅 심리전. 각자 원판 4장(장미 3 + 해골 1)으로 배치 → 베팅 →
// 뒤집기를 반복한다. 손패·더미 내용·제거된 카드가 전부 비공개라 개인화
// 스냅샷(buildSKState)이 유일한 정보 통로다.

const (
	SKMinPlayers = 3 // 시작 최소 인원
	SKMaxPlayers = 6 // 좌석 수 상한

	// SKBotFillTarget 봇 채우기 상한 — 정원(6)까지 채운다. 시작은 별도 sk_start.
	SKBotFillTarget = 6

	SKWinPoints = 2 // 선취 승리 점수
	SKHandSize  = 4 // 시작 손패 (장미 3 + 해골 1)
)

// SKCard 원판 카드
type SKCard string

const (
	SKCardRose  SKCard = "rose"
	SKCardSkull SKCard = "skull"
)

// SKPhase 게임 진행 단계
type SKPhase string

const (
	SKPhaseWaiting  SKPhase = "waiting"
	SKPhasePlacing  SKPhase = "placing"
	SKPhaseBidding  SKPhase = "bidding"
	SKPhaseFlipping SKPhase = "flipping"
	SKPhaseRoundEnd SKPhase = "round_end"
	SKPhaseGameOver SKPhase = "game_over"
)

// SKMessageType 스컬 메시지 타입
type SKMessageType string

const (
	// 클라이언트 → 서버
	SKMsgJoinGame SKMessageType = "sk_join_game"
	SKMsgFillBots SKMessageType = "sk_fill_bots"
	SKMsgStart    SKMessageType = "sk_start"
	SKMsgRejoin   SKMessageType = "sk_rejoin"
	SKMsgPlace    SKMessageType = "sk_place"
	SKMsgBid      SKMessageType = "sk_bid"
	SKMsgPass     SKMessageType = "sk_pass"
	SKMsgFlip     SKMessageType = "sk_flip"
	SKMsgReact    SKMessageType = "sk_react"

	// 서버 → 클라이언트
	SKMsgPlayerJoined         SKMessageType = "sk_player_joined"
	SKMsgSpectateJoined       SKMessageType = "sk_spectate_joined"
	SKMsgGameState            SKMessageType = "sk_game_state"
	SKMsgEvent                SKMessageType = "sk_event"
	SKMsgGameOver             SKMessageType = "sk_game_over"
	SKMsgOpponentDisconnected SKMessageType = "sk_opponent_disconnected"
	SKMsgReconnected          SKMessageType = "sk_reconnected"
	SKMsgSessionExpired       SKMessageType = "sk_session_expired"
	SKMsgError                SKMessageType = "sk_error"
)

// SKClient 스컬 클라이언트 연결
type SKClient struct {
	wsClient
	Hub  *SKHub
	Seat int
}

// SKMessage 메시지 봉투
type SKMessage struct {
	Type    SKMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 순수 게임 상태 ====================

// SKPlayer 좌석 하나 (연결은 skRoom 이 든다)
type SKPlayer struct {
	Seat   int
	Name   string
	Alive  bool
	Points int

	// Hand 손패 (비공개 — 본인 스냅샷에만 실린다)
	Hand []SKCard

	// Stack 이번 라운드에 내려놓은 더미. 내려놓은 순서(끝이 맨 위)이며
	// 장수만 공개, 내용은 비공개다. 뒤집힌 카드는 여기서 빠져 Flipped 로 간다.
	Stack []SKCard

	Passed bool // 이번 라운드 베팅에서 빠졌는지
	Bid    int  // 현재 베팅 (0 = 없음)
}

// SKGame 스컬 게임 상태 (순수, 허브 비의존)
type SKGame struct {
	ID      string
	Phase   SKPhase
	RoundNo int
	Players []SKPlayer

	// LeaderSeat 이번 라운드 선 (턴제 배치의 첫 차례)
	LeaderSeat int

	// CurrentSeat 현재 차례 좌석 (-1 = 동시 배치·차례 없음)
	CurrentSeat int

	// PlacingTurns 배치 단계가 동시 1장 배치를 끝내고 턴제 파트로 넘어갔는지
	PlacingTurns bool

	HighBid        int
	HighBidderSeat int // -1 없음
	ChallengerSeat int // -1 없음

	// Flipped 이번 뒤집기에서 공개된 카드 (뒤집힌 순서 — 공개 정보)
	Flipped []SKFlippedCard

	// RoundResult 라운드 결과 — round_end/game_over 에서만 non-nil
	RoundResult *SKRoundResult

	WinnerSeat int // -1 진행 중

	// Deadline 현재 단계 마감 시각 (unixMillis) — 허브가 세팅·소비한다
	Deadline int64

	// AfkSeq 단계 타이머 일련번호 — 지나간 발화를 무시하는 가드
	AfkSeq int

	Ready     bool
	StartedAt time.Time
}

// ==================== 클라이언트 → 서버 payload ====================

type SKJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type SKRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// SKPlacePayload 손패 index 카드를 내 더미 맨 위에 내려놓는다
type SKPlacePayload struct {
	Index int `json:"index"`
}

// SKBidPayload 베팅 선언 (장미 count 장을 뒤집겠다)
type SKBidPayload struct {
	Count int `json:"count"`
}

// SKFlipPayload 그 좌석 더미의 맨 위 카드 1장을 뒤집는다
type SKFlipPayload struct {
	Seat int `json:"seat"`
}

// SKReactPayload 리액션 이모지 (화이트리스트 6종 외는 조용히 무시)
type SKReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

type SKErrorPayload struct {
	Message string `json:"message"`
}

// SKPlayerView 좌석별 공개 정보 — 카드 내용은 절대 싣지 않는다 (장수만).
type SKPlayerView struct {
	Seat       int    `json:"seat"`
	Name       string `json:"name"`
	Connected  bool   `json:"connected"`
	Bot        bool   `json:"bot"`
	Alive      bool   `json:"alive"`
	HandCount  int    `json:"handCount"`
	StackCount int    `json:"stackCount"`
	Points     int    `json:"points"`
	Passed     bool   `json:"passed"`
	Bid        int    `json:"bid"`
}

// SKFlippedCard 공개된 카드 한 장 (뒤집힌 카드만 공개 정보다)
type SKFlippedCard struct {
	Seat int    `json:"seat"`
	Card string `json:"card"` // "rose" | "skull"
}

// SKRoundResult 라운드 결과 발표
type SKRoundResult struct {
	Kind    string `json:"kind"` // "success" | "fail"
	Seat    int    `json:"seat"` // 도전자 좌석
	Message string `json:"message"`
}

// SKGameStatePayload 개인화 게임 스냅샷 (은닉형).
// yourHand/yourStack 은 본인 것만 — 관전자·타인 시점에서는 빈 배열이다.
// int 필드는 0 이 유효값이라 omitempty 를 쓰지 않는다.
type SKGameStatePayload struct {
	GameID   string  `json:"gameId"`
	RoomCode string  `json:"roomCode"`
	Phase    SKPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"`
	// Spectators 관전자 수 (참가자·관전자 모두 표시용)
	Spectators int `json:"spectators"`
	// EndsAt 현재 단계 마감 시각 (unixMillis, 없는 단계는 0)
	EndsAt      int64 `json:"endsAt"`
	CurrentSeat int   `json:"currentSeat"`

	YourHand  []string `json:"yourHand"`  // 나만 — 그 외 빈 배열
	YourStack []string `json:"yourStack"` // 내가 내려놓은 순서 — 나만

	Players []SKPlayerView `json:"players"`

	HighBid        int             `json:"highBid"`
	ChallengerSeat int             `json:"challengerSeat"` // -1 없음
	Flipped        []SKFlippedCard `json:"flipped"`
	// FlipTarget 뒤집기 단계에서 남은 장미 수 (그 외 단계 0)
	FlipTarget  int            `json:"flipTarget"`
	RoundResult *SKRoundResult `json:"roundResult"` // 없으면 null
}

// SKEventPayload 연출용 이벤트.
// kind: joined|left|started|round_begin|placed|bid|pass|challenge|flip|
// round_result|eliminated|react|bot_takeover|game_over
type SKEventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message,omitempty"`
}

// SKGameOverPayload 종료 발표 — 승자와 최종 현황
type SKGameOverPayload struct {
	WinnerSeat int            `json:"winnerSeat"`
	WinnerName string         `json:"winnerName"`
	Reason     string         `json:"reason"`
	Players    []SKPlayerView `json:"players"`
}

type SKOpponentDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type SKReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}
