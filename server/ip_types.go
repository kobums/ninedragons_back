package server

import "time"

// ==================== 인디언 포커 타입 ====================
//
// 2~6인 역은닉 카드 배팅. 핵심 반전: 내 카드만 안 보이고 남의 카드는 전부
// 보인다. 은닉 방향이 보통 게임과 반대이므로 buildIPState 의 viewer 분기가
// 계약의 심장이다 (본인 좌석만 0, 쇼다운에 생존자 전원 공개, 폴드자 영구 0).

// IPMinPlayers / IPMaxPlayers 인원 범위 (2~6인)
const (
	IPMinPlayers = 2
	IPMaxPlayers = 6

	// IPFillBotTarget ip_fill_bots 가 채우는 목표 인원 — 채운 뒤 즉시 시작
	IPFillBotTarget = 4

	IPRounds     = 10 // 고정 라운드 수
	IPStartChips = 20 // 시작 칩
	IPAnte       = 1  // 라운드 시작 안테
	IPMaxRaises  = 3  // 라운드 전체 레이즈 상한
	IPRaiseMin   = 1  // 레이즈 최소 증가분
	IPRaiseMax   = 3  // 레이즈 최대 증가분
)

// IPPhase 게임 진행 단계
type IPPhase string

const (
	IPPhaseWaiting  IPPhase = "waiting"
	IPPhaseBetting  IPPhase = "betting"
	IPPhaseShowdown IPPhase = "showdown"  // 콜 완주 — 생존자 카드 공개
	IPPhaseRoundEnd IPPhase = "round_end" // 전원 폴드 — 쇼다운 없이 종료
	IPPhaseGameOver IPPhase = "game_over"
)

// IPMessageType 인디언 포커 메시지 타입
type IPMessageType string

const (
	// 클라이언트 → 서버
	IPMsgJoinGame IPMessageType = "ip_join_game"
	IPMsgFillBots IPMessageType = "ip_fill_bots"
	IPMsgStart    IPMessageType = "ip_start"
	IPMsgRejoin   IPMessageType = "ip_rejoin"
	IPMsgCall     IPMessageType = "ip_call"
	IPMsgRaise    IPMessageType = "ip_raise"
	IPMsgFold     IPMessageType = "ip_fold"
	IPMsgReact    IPMessageType = "ip_react"

	// 서버 → 클라이언트
	IPMsgPlayerJoined       IPMessageType = "ip_player_joined"
	IPMsgSpectateJoined     IPMessageType = "ip_spectate_joined"
	IPMsgGameState          IPMessageType = "ip_game_state"
	IPMsgEvent              IPMessageType = "ip_event"
	IPMsgGameOver           IPMessageType = "ip_game_over"
	IPMsgPlayerDisconnected IPMessageType = "ip_player_disconnected"
	IPMsgPlayerReconnected  IPMessageType = "ip_player_reconnected"
	IPMsgSessionExpired     IPMessageType = "ip_session_expired"
	IPMsgError              IPMessageType = "ip_error"
)

// IPPlayer 게임 참가자 (순수 상태 — 연결 매핑은 허브의 ipRoom 담당)
type IPPlayer struct {
	Seat  int
	Name  string
	Chips int
	Card  int // 이번 라운드 카드 (1~10, 미배분 0)

	Folded bool // 이번 라운드 폴드 (카드는 끝까지 비공개)
	AllIn  bool // 칩을 다 걸어 더는 행동할 수 없음 (올인 콜은 현재 베팅으로 인정)
	Acted  bool // 현재 베팅액에 응답했는지 (레이즈가 나오면 리셋)

	BetThisRound int  // 이번 라운드에 팟에 낸 칩 (안테 포함)
	Alive        bool // 탈락하지 않았는지 (안테 못 내면 탈락)
}

// IPRoundResult 라운드 결과 (showdown/round_end 동안 스냅샷에 노출)
type IPRoundResult struct {
	WinnerSeats []int  `json:"winnerSeats"`
	Message     string `json:"message"` // 카드 값 요약 포함 한글 문구
}

// IPGame 인디언 포커 게임 상태 (순수, 허브 비의존)
type IPGame struct {
	ID      string
	Players []*IPPlayer
	Phase   IPPhase

	Round       int // 1~10
	Pot         int
	CurrentSeat int // 베팅 차례 (-1 = 없음)
	CurrentBet  int // 이번 라운드에 맞춰야 하는 총액 (안테 포함)
	RaisesLeft  int // 이번 라운드 남은 레이즈 횟수 (전체 상한 3)
	FirstSeat   int // 이번 라운드 선 — 라운드마다 순환, 동점 나머지 분배 기준

	RoundResult *IPRoundResult // showdown/round_end 동안, 다음 라운드에 nil
	WinnerSeats []int          // 게임 승자 (동점 공동 승)
	EndReason   string         // "chips" | "last_standing"
	Ready       bool
	StartedAt   time.Time
}

// IPClient 인디언 포커 클라이언트 연결
type IPClient struct {
	wsClient
	Hub  *IPHub
	Seat int
}

// IPMessage 메시지 봉투
type IPMessage struct {
	Type    IPMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type IPJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type IPRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// IPRaisePayload 레이즈 증가분 (1~3칩)
type IPRaisePayload struct {
	Amount int `json:"amount"`
}

// IPReactPayload 리액션 이모지 (화이트리스트 외는 조용히 무시)
type IPReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

// IPPlayerView 수신자별 플레이어 정보. 좌석 0·카드 0 유실 방지를 위해
// int 필드에 omitempty 를 쓰지 않는다. 역은닉의 핵심 — card 는
// viewer 본인 좌석만 0, 타인은 실값. showdown/round_end 에는 생존자 전원
// 실값(본인 포함), 폴드자는 언제나 0.
type IPPlayerView struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	Connected    bool   `json:"connected"`
	Bot          bool   `json:"bot"`
	Alive        bool   `json:"alive"`
	Folded       bool   `json:"folded"`
	Chips        int    `json:"chips"`
	BetThisRound int    `json:"betThisRound"`
	Card         int    `json:"card"`
}

// IPGameStatePayload 개인화된 전체 게임 스냅샷. 모든 상태 변경 후 좌석마다
// 따로 만들어 보낸다. 재접속 복원도 같은 페이로드를 쓴다.
type IPGameStatePayload struct {
	GameID   string  `json:"gameId"`
	RoomCode string  `json:"roomCode"`
	Phase    IPPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"` // 관전자는 -1
	// Spectators 관전자 수 (참가자·관전자 모두 표시용)
	Spectators int `json:"spectators"`
	// EndsAt 현재 차례의 AFK 자동 콜 마감 시각 (unixMillis, betting 외 0)
	EndsAt      int64          `json:"endsAt"`
	Round       int            `json:"round"`
	Pot         int            `json:"pot"`
	CurrentSeat int            `json:"currentSeat"`
	CurrentBet  int            `json:"currentBet"`
	RaisesLeft  int            `json:"raisesLeft"`
	Players     []IPPlayerView `json:"players"`
	RoundResult *IPRoundResult `json:"roundResult"` // showdown/round_end 동안, 그 외 null
}

// IPEventPayload 연출용 이벤트. 비밀 정보를 담지 않으며 전원에게 동일하게
// 간다. Seat 은 좌석 0 유실 방지를 위해 포인터로 생략을 표현한다.
type IPEventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

// IPGameOverPayload 게임 종료 발표 (동점 공동 승 지원)
type IPGameOverPayload struct {
	WinnerSeats []int          `json:"winnerSeats"`
	WinnerNames []string       `json:"winnerNames"`
	Chips       []int          `json:"chips"` // 좌석 순 최종 칩
	Players     []IPPlayerView `json:"players"`
}

type IPPlayerDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type IPPlayerReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type IPErrorPayload struct {
	Message string `json:"message"`
}
