package server

import "time"

// ==================== 티츄(Tichu) 타입 ====================
//
// 4인 고정, 팀 = 좌석 0·2 vs 1·3. 시계방향 좌석 0→1→2→3.
// 카드 와이어 인코딩: "<S><R>" (S ∈ G/B/U/R, R ∈ 2..14) + 특수 "MAH"/"DOG"/"PHX"/"DRG".

// TCSeats 좌석 수 (고정 4인)
const TCSeats = 4

// TCDefaultTarget 기본 목표점수
const TCDefaultTarget = 500

// TCPhase 게임 진행 단계
const (
	TCPhaseWaiting  = "waiting"
	TCPhaseGrand    = "grand"
	TCPhaseExchange = "exchange"
	TCPhasePlay     = "play"
	TCPhaseHandEnd  = "hand_end"
	TCPhaseGameOver = "game_over"
)

// 특수 카드 코드
const (
	tcMah = "MAH" // 참새(1)
	tcDog = "DOG" // 개
	tcPhx = "PHX" // 봉황
	tcDrg = "DRG" // 용
)

// 콤보 종류 (와이어 공통)
const (
	tcSingle    = "single"
	tcPair      = "pair"
	tcTriple    = "triple"
	tcStraight  = "straight"
	tcFullhouse = "fullhouse"
	tcPairSeq   = "pairseq"
	tcBomb      = "bomb"
	tcBombSF    = "bomb_sf"
	tcDogCombo  = "dog"
)

// TCMessageType 티츄 메시지 타입
type TCMessageType string

const (
	// 클라이언트 → 서버
	TCMsgJoinGame   TCMessageType = "tc_join_game"
	TCMsgSetTarget  TCMessageType = "tc_set_target"
	TCMsgFillBots   TCMessageType = "tc_fill_bots"
	TCMsgCallGrand  TCMessageType = "tc_call_grand"
	TCMsgCallTichu  TCMessageType = "tc_call_tichu"
	TCMsgExchange   TCMessageType = "tc_exchange"
	TCMsgPlay       TCMessageType = "tc_play"
	TCMsgPass       TCMessageType = "tc_pass"
	TCMsgDragonGive TCMessageType = "tc_dragon_give"
	TCMsgReady      TCMessageType = "tc_ready"
	TCMsgRejoin     TCMessageType = "tc_rejoin"

	// 서버 → 클라이언트
	TCMsgPlayerJoined         TCMessageType = "tc_player_joined"
	TCMsgGameState            TCMessageType = "tc_game_state"
	TCMsgEvent                TCMessageType = "tc_event"
	TCMsgGameOver             TCMessageType = "tc_game_over"
	TCMsgOpponentDisconnected TCMessageType = "tc_opponent_disconnected"
	TCMsgReconnected          TCMessageType = "tc_reconnected"
	TCMsgSessionExpired       TCMessageType = "tc_session_expired"
	TCMsgError                TCMessageType = "tc_error"
)

// TCMessage 메시지 봉투
type TCMessage struct {
	Type    TCMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// TCClient 티츄 클라이언트 연결
type TCClient struct {
	wsClient
	Hub  *TCHub
	Seat int
}

// tcCombo 판정된 콤보. Value 는 부동소수점 회피를 위해 2배 정수다
// (일반 랭크 r → r*2, 봉황 싱글 → 직전 싱글값+1). Len 은 카드 장수로
// straight/pairseq/bomb_sf 의 길이 비교에 쓴다.
type tcCombo struct {
	Kind  string
	Cards []string
	Value int
	Len   int
}

// TCGame 티츄 게임 상태 (순수, 허브 비의존)
type TCGame struct {
	ID     string
	Target int

	// 좌석별 이름 ("" = 빈 좌석)
	Names       [TCSeats]string
	PlayerCount int

	// 누적 점수
	Score02 int
	Score13 int

	// ---- 핸드 상태 ----
	HandNo int
	Phase  string
	// Dealt 이 핸드에 배분된 14장 (그랜드 단계에선 앞 8장만 보인다)
	Dealt [TCSeats][]string
	// Hands 현재 손패
	Hands [TCSeats][]string
	// Won 좌석별로 먹은 트릭 더미
	Won [TCSeats][]string

	GrandAnswered [TCSeats]bool
	Tichu         [TCSeats]string // ''|'tichu'|'grand'
	PlayedAny     [TCSeats]bool
	ExchangeSub   [TCSeats]*TCExchangePayload

	Out      [TCSeats]bool
	OutRank  [TCSeats]int
	OutCount int

	CurrentTurn    int
	Trick          []TCTrickPlay
	lastCombo      *tcCombo
	LastPlayerSeat int

	WishRank          int // 0 = 없음, 2..14
	DragonPendingSeat int // -1 = 없음

	Ready      [TCSeats]bool
	HandResult *TCHandResult
	WinnerTeam string // ''|'02'|'13'

	Started   bool
	StartedAt time.Time
}

// ==================== 클라이언트 → 서버 payload ====================

type TCJoinGamePayload struct {
	Name string `json:"name"`
}

type TCSetTargetPayload struct {
	Target int `json:"target"`
}

type TCCallGrandPayload struct {
	Call bool `json:"call"`
}

// TCExchangePayload 좌석 (seat+3)%4 에게 left, (seat+2)%4 에게 partner,
// (seat+1)%4 에게 right 카드를 전달한다.
type TCExchangePayload struct {
	Left    string `json:"left"`
	Partner string `json:"partner"`
	Right   string `json:"right"`
}

type TCPlayPayload struct {
	Cards []string `json:"cards"`
	Wish  int      `json:"wish,omitempty"`
}

type TCDragonGivePayload struct {
	Seat int `json:"seat"`
}

type TCRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// ==================== 서버 → 클라이언트 payload ====================

// TCPlayerView 좌석 하나의 공개 정보 (손패는 개수만)
type TCPlayerView struct {
	Seat      int    `json:"seat"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Bot       bool   `json:"bot"`
	HandCount int    `json:"handCount"`
	Tichu     string `json:"tichu"`
	Out       bool   `json:"out"`
	OutRank   int    `json:"outRank"`
	Ready     bool   `json:"ready"`
}

// TCScores 팀 누적 점수
type TCScores struct {
	Team02 int `json:"team02"`
	Team13 int `json:"team13"`
}

// TCTrickPlay 트릭에 낸 수 하나. Phx 는 봉황 싱글의 내부 강도(2배 정수)로,
// 스펙 필드에 더한 부가 정보다 — 봇이 봉황 반값 싱글의 세기를 추적하는 데 쓴다.
type TCTrickPlay struct {
	Seat  int      `json:"seat"`
	Cards []string `json:"cards"`
	Combo string   `json:"combo"`
	Phx   int      `json:"phx,omitempty"`
}

// TCHandResult 핸드 정산 결과
type TCHandResult struct {
	Team02Delta int    `json:"team02Delta"`
	Team13Delta int    `json:"team13Delta"`
	Detail      string `json:"detail"`
}

// TCGameStatePayload 개인화된 전체 게임 스냅샷 (은닉형 — 남의 손패는 개수만)
type TCGameStatePayload struct {
	GameID            string         `json:"gameId"`
	Phase             string         `json:"phase"`
	TargetScore       int            `json:"targetScore"`
	YourSeat          int            `json:"yourSeat"`
	Players           []TCPlayerView `json:"players"`
	YourHand          []string       `json:"yourHand"`
	ExchangeDone      bool           `json:"exchangeDone"`
	GrandAnswered     bool           `json:"grandAnswered"`
	Scores            TCScores       `json:"scores"`
	HandNo            int            `json:"handNo"`
	CurrentTurn       int            `json:"currentTurn"`
	Trick             []TCTrickPlay  `json:"trick"`
	LastPlay          *TCTrickPlay   `json:"lastPlay"`
	WishRank          int            `json:"wishRank"`
	DragonPendingSeat int            `json:"dragonPendingSeat"`
	HandResult        *TCHandResult  `json:"handResult"`
	WinnerTeam        string         `json:"winnerTeam"`
}

// TCEventPayload 연출용 이벤트 — kind: joined/left/grand/tichu/exchanged/
// play/pass/trick_won/bomb/dog/wish/wish_done/player_out/dragon_given/
// hand_end/bot_takeover
type TCEventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Message string `json:"message"`
}

// TCGameOverPayload 게임 종료
type TCGameOverPayload struct {
	WinnerTeam string   `json:"winnerTeam"`
	Scores     TCScores `json:"scores"`
}

type TCPlayerJoinedPayload struct {
	GameID    string `json:"gameId"`
	SessionID string `json:"sessionId"`
	YourSeat  int    `json:"yourSeat"`
	Name      string `json:"name"`
}

type TCOpponentDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type TCReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type TCErrorPayload struct {
	Message string `json:"message"`
}
