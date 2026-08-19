package server

import (
	"time"
)

// ==================== 마이티(Mighty) 타입 ====================
//
// 5인 고정 단판 승부. 카드는 와이어 인코딩 문자열("S14", "C3", "JK")을
// 서버 내부 표현으로도 그대로 쓴다 — 변환 계층이 없어야 봇·서버·프론트가
// 같은 값을 본다.

const (
	MTPlayerCount = 5  // 좌석 수
	MTHandSize    = 10 // 시작 손패
	MTKittySize   = 3  // 키티
	MTTrickCount  = 10 // 총 트릭 수
	MTScoreTotal  = 20 // 점수 카드 총수 (10·J·Q·K·A × 4문양)

	MTJoker = "JK"

	MTMinBid   = 13 // 최소 공약 (기루다 있음)
	MTMinBidNT = 12 // 노기루 최소 공약
	MTMaxBid   = 20
)

// MTPhase 게임 진행 단계
type MTPhase string

const (
	MTPhaseWaiting  MTPhase = "waiting"
	MTPhaseBidding  MTPhase = "bidding"
	MTPhaseKitty    MTPhase = "kitty"
	MTPhaseFriend   MTPhase = "friend"
	MTPhasePlay     MTPhase = "play"
	MTPhaseGameOver MTPhase = "game_over"
)

// MTMessageType 마이티 메시지 타입
type MTMessageType string

const (
	// 클라이언트 → 서버
	MTMsgJoinGame MTMessageType = "mt_join_game"
	MTMsgFillBots MTMessageType = "mt_fill_bots"
	MTMsgRejoin   MTMessageType = "mt_rejoin"
	MTMsgBid      MTMessageType = "mt_bid"
	MTMsgPass     MTMessageType = "mt_pass"
	MTMsgKitty    MTMessageType = "mt_kitty"
	MTMsgFriend   MTMessageType = "mt_friend"
	MTMsgPlay     MTMessageType = "mt_play"
	MTMsgReact    MTMessageType = "mt_react"

	// 서버 → 클라이언트
	MTMsgPlayerJoined         MTMessageType = "mt_player_joined"
	MTMsgSpectateJoined       MTMessageType = "mt_spectate_joined"
	MTMsgGameState            MTMessageType = "mt_game_state"
	MTMsgEvent                MTMessageType = "mt_event"
	MTMsgGameOver             MTMessageType = "mt_game_over"
	MTMsgOpponentDisconnected MTMessageType = "mt_opponent_disconnected"
	MTMsgReconnected          MTMessageType = "mt_reconnected"
	MTMsgSessionExpired       MTMessageType = "mt_session_expired"
	MTMsgError                MTMessageType = "mt_error"
)

// MTClient 마이티 클라이언트 연결
type MTClient struct {
	wsClient
	Hub  *MTHub
	Seat int
}

// MTMessage 메시지 봉투
type MTMessage struct {
	Type    MTMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 순수 게임 상태 ====================

// MTPlayer 좌석 하나 (이름만 — 연결은 mtRoom 이 든다)
type MTPlayer struct {
	Seat int
	Name string
}

// MTBid 현재 최고 공약
type MTBid struct {
	Seat  int
	Suit  string // S|D|C|H|N
	Count int
}

// MTTrickPlay 트릭에 낸 카드 한 장
type MTTrickPlay struct {
	Seat int    `json:"seat"`
	Card string `json:"card"`
}

// MTLastTrick 직전 트릭 결과 (연출·재접속 복원용)
type MTLastTrick struct {
	Winner int           `json:"winner"`
	Cards  []MTTrickPlay `json:"cards"`
}

// MTGame 마이티 게임 상태 (순수, 허브 비의존)
type MTGame struct {
	ID      string
	Phase   MTPhase
	Players []MTPlayer

	Hands map[int][]string // seat → 손패 (정렬 유지)
	Kitty []string

	// ---- 입찰 ----
	BidTurn     int
	Passed      map[int]bool
	Best        *MTBid
	RedealCount int

	// ---- 공약 ----
	Declarer      int
	Trump         string // S|D|C|H|N (키티 단계에서 확정)
	ContractCount int
	KittyDiscard  []string
	KittyPoints   int // 버림 3장 중 점수 카드 수 → 주공팀 득점 귀속

	// ---- 프렌드 ----
	FriendType     string // "" | card | first_trick | none
	FriendCard     string
	FriendSeat     int // -1 = 미공개/없음
	FriendRevealed bool

	// ---- 플레이 ----
	TrickNo         int // 1..10
	Turn            int // -1 = 없음
	LedSuit         string
	JokerCallActive bool
	Trick           []MTTrickPlay
	LastTrick       *MTLastTrick
	TricksWon       map[int]int
	CapturedPoints  map[int]int // seat → 딴 점수 카드 수 (키티 제외)

	Result *MTResultView

	Ready     bool
	StartedAt time.Time
}

// ==================== 클라이언트 → 서버 payload ====================

type MTJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type MTRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

type MTBidPayload struct {
	Suit  string `json:"suit"`
	Count int    `json:"count"`
}

type MTKittyPayload struct {
	Discard []string `json:"discard"`
	Suit    string   `json:"suit"`
}

type MTFriendPayload struct {
	Type string `json:"type"` // card | first_trick | none
	Card string `json:"card,omitempty"`
}

type MTPlayPayload struct {
	Card      string `json:"card"`
	JokerCall bool   `json:"jokerCall,omitempty"`
	JokerSuit string `json:"jokerSuit,omitempty"`
}

// MTReactPayload 리액션 이모지 (화이트리스트 6종 외는 조용히 무시)
type MTReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

type MTErrorPayload struct {
	Message string `json:"message"`
}

// MTPlayerView 좌석별 공개 정보 (손패는 개수만)
type MTPlayerView struct {
	Seat           int    `json:"seat"`
	Name           string `json:"name"`
	Connected      bool   `json:"connected"`
	Bot            bool   `json:"bot"`
	HandCount      int    `json:"handCount"`
	Passed         bool   `json:"passed"`
	IsDeclarer     bool   `json:"isDeclarer"`
	FriendRevealed bool   `json:"friendRevealed"`
	TricksWon      int    `json:"tricksWon"`
	CapturedPoints int    `json:"capturedPoints"`
}

type MTBidView struct {
	Seat  int    `json:"seat"`
	Suit  string `json:"suit"`
	Count int    `json:"count"`
}

type MTBiddingView struct {
	Turn int        `json:"turn"`
	Best *MTBidView `json:"best"`
}

type MTContractView struct {
	Declarer int    `json:"declarer"`
	Suit     string `json:"suit"`
	Count    int    `json:"count"`
}

type MTFriendView struct {
	Type     string `json:"type"`
	Card     string `json:"card,omitempty"`
	Revealed bool   `json:"revealed"`
	Seat     int    `json:"seat"`
}

type MTResultView struct {
	Win            bool           `json:"win"`
	DeclarerPoints int            `json:"declarerPoints"`
	DefenderPoints int            `json:"defenderPoints"`
	Contract       MTContractView `json:"contract"`
	FriendSeat     int            `json:"friendSeat"`
	DeclarerTeam   []string       `json:"declarerTeam"`
	DefenderTeam   []string       `json:"defenderTeam"`
}

// MTGameStatePayload 개인화 게임 스냅샷 (은닉형 — 남의 손패는 개수만,
// 키티는 kitty 단계의 주공에게만)
type MTGameStatePayload struct {
	GameID string `json:"gameId"`
	// RoomCode 사설 방 초대 코드 (공용 로비는 "") — 대기실 코드 표시·재접속 복원용
	RoomCode        string         `json:"roomCode"`
	Phase           MTPhase        `json:"phase"`
	YourSeat        int            `json:"yourSeat"`
	Players         []MTPlayerView `json:"players"`
	YourHand        []string       `json:"yourHand"`
	// Spectators 관전자 수 (참가자·관전자 모두 표시용)
	Spectators int `json:"spectators"`
	Bidding         MTBiddingView  `json:"bidding"`
	Contract        *MTContractView `json:"contract"`
	YourKitty       []string       `json:"yourKitty"`
	Friend          MTFriendView   `json:"friend"`
	TrickNo         int            `json:"trickNo"`
	CurrentTurn     int            `json:"currentTurn"`
	LedSuit         string         `json:"ledSuit"`
	JokerCallActive bool           `json:"jokerCallActive"`
	Trick           []MTTrickPlay  `json:"trick"`
	LastTrick       *MTLastTrick   `json:"lastTrick"`
	Result          *MTResultView  `json:"result"`
}

// MTEventPayload 연출용 이벤트.
// kind: joined|left|bid|pass|redeal|contract|kitty_done|friend_set|
//
//	friend_revealed|play|joker_call|trick_won|bot_takeover
type MTEventPayload struct {
	Kind string `json:"kind"`
	Seat *int   `json:"seat,omitempty"`
	Name string `json:"name,omitempty"`
	// Message react(이모지) 등 연출 텍스트
	Message string `json:"message,omitempty"`
	Suit    string `json:"suit,omitempty"`
	Count      *int   `json:"count,omitempty"`
	Card       string `json:"card,omitempty"`
	FriendType string `json:"friendType,omitempty"`
	Winner     *int   `json:"winner,omitempty"`
	TrickNo    *int   `json:"trickNo,omitempty"`
}

type MTOpponentDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type MTReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}
