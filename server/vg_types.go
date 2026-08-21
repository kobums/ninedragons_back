package server

import "time"

// ==================== 라스베가스 타입 ====================

// VGMinPlayers / VGMaxPlayers 인원 범위 (2~5인)
const (
	VGMinPlayers = 2
	VGMaxPlayers = 5
)

// VGDiceCount 라운드마다 각자 받는 주사위 수
const VGDiceCount = 8

// VGTotalRounds 총 라운드 수
const VGTotalRounds = 4

// VGCasinoCount 카지노 수 — 주사위 눈 1~6과 일대일
const VGCasinoCount = 6

// VGCasinoMinTotal 라운드 준비 때 카지노마다 까는 지폐 합계 하한 (만 단위)
const VGCasinoMinTotal = 5

// VGFillBotTarget fill_bots 가 채우는 목표 인원 — 채우면 즉시 시작한다
const VGFillBotTarget = 4

// VGPhase 게임 진행 단계
type VGPhase string

const (
	VGPhaseWaiting  VGPhase = "waiting"
	VGPhasePlacing  VGPhase = "placing"   // 차례대로 굴리고 배치
	VGPhaseRoundEnd VGPhase = "round_end" // 정산 결과 표시 — 타이머로 다음 라운드
	VGPhaseGameOver VGPhase = "game_over"
)

// VGMessageType 라스베가스 메시지 타입
type VGMessageType string

const (
	// 클라이언트 → 서버
	VGMsgJoinGame VGMessageType = "vg_join_game"
	VGMsgFillBots VGMessageType = "vg_fill_bots"
	VGMsgStart    VGMessageType = "vg_start"
	VGMsgRejoin   VGMessageType = "vg_rejoin"
	VGMsgPlace    VGMessageType = "vg_place"
	VGMsgReact    VGMessageType = "vg_react"

	// 서버 → 클라이언트
	VGMsgPlayerJoined       VGMessageType = "vg_player_joined"
	VGMsgSpectateJoined     VGMessageType = "vg_spectate_joined"
	VGMsgGameState          VGMessageType = "vg_game_state"
	VGMsgEvent              VGMessageType = "vg_event"
	VGMsgGameOver           VGMessageType = "vg_game_over"
	VGMsgPlayerDisconnected VGMessageType = "vg_player_disconnected"
	VGMsgPlayerReconnected  VGMessageType = "vg_player_reconnected"
	VGMsgSessionExpired     VGMessageType = "vg_session_expired"
	VGMsgError              VGMessageType = "vg_error"
)

// VGPlayer 게임 참가자 (순수 상태 — 연결 매핑은 허브의 vgRoom 담당)
type VGPlayer struct {
	Seat     int
	Name     string
	Cash     int // 누적 총액 (만 단위)
	DiceLeft int // 이번 라운드에 남은 주사위 수
}

// VGCasino 카지노 한 곳 (눈 1~6). 전부 공개 — 은닉 정보가 없다.
type VGCasino struct {
	Face   int
	Bills  []int       // 만 단위, 내림차순 (정산 때 큰 지폐부터 지급)
	Placed map[int]int // seat → 배치된 주사위 수
}

// VGRoundResult 라운드 정산 결과 (round_end 동안 스냅샷에 노출)
type VGRoundResult struct {
	Message string `json:"message"`
}

// VGGame 라스베가스 게임 상태 (순수, 허브 비의존). 주사위·지폐·배치가
// 전부 공개라 스냅샷 개인화는 yourSeat 뿐이다.
type VGGame struct {
	ID      string
	Players []*VGPlayer
	Phase   VGPhase

	Round       int // 1~4 (waiting 은 0)
	CurrentSeat int
	FirstSeat   int // 이번 라운드의 첫 배치 좌석 (라운드마다 한 칸 회전)

	Casinos []*VGCasino // 항상 6곳 — 인덱스 face-1
	Dice    []int       // 현재 차례가 굴린 주사위 (placing 외에는 빈 배열)

	RoundResult *VGRoundResult
	WinnerSeats []int // 게임 종료 시 승자 좌석들 (동점 공동 승), 그 외 []
	Ready       bool
	StartedAt   time.Time
}

// VGClient 라스베가스 클라이언트 연결
type VGClient struct {
	wsClient
	Hub  *VGHub
	Seat int
}

// VGMessage 메시지 봉투
type VGMessage struct {
	Type    VGMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type VGJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type VGRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// VGPlacePayload 배치 — 방금 굴린 주사위 중 face 눈 전부를 그 카지노에 놓는다
type VGPlacePayload struct {
	Face int `json:"face"`
}

// VGReactPayload 리액션 이모지 (화이트리스트 외는 조용히 무시)
type VGReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

// VGPlayerView 공개 플레이어 정보. 좌석 0·현금 0 유실 방지를 위해
// int 필드에 omitempty 를 쓰지 않는다.
type VGPlayerView struct {
	Seat      int    `json:"seat"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Bot       bool   `json:"bot"`
	Cash      int    `json:"cash"`
	DiceLeft  int    `json:"diceLeft"`
}

// VGCasinoView 카지노 공개 정보. bills 는 내림차순 배열, placed 는
// seat → 배치 수 맵 — 항상 배열·맵으로 나간다 (nil 금지).
type VGCasinoView struct {
	Face   int         `json:"face"`
	Bills  []int       `json:"bills"`
	Placed map[int]int `json:"placed"`
}

// VGGameStatePayload 전체 게임 스냅샷. 모든 상태 변경 후 좌석마다 만들어
// 보낸다 (개인화는 yourSeat 뿐 — 전부 공개라 관전 자유).
type VGGameStatePayload struct {
	GameID   string  `json:"gameId"`
	RoomCode string  `json:"roomCode"`
	Phase    VGPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"` // 관전자는 -1
	// Spectators 관전자 수 (참가자·관전자 모두 표시용)
	Spectators int `json:"spectators"`
	// EndsAt 현재 차례의 AFK 자동 배치 마감 시각 (unixMillis, placing 외 0)
	EndsAt      int64          `json:"endsAt"`
	Round       int            `json:"round"`
	CurrentSeat int            `json:"currentSeat"`
	Dice        []int          `json:"dice"`    // 현재 차례가 굴린 주사위, 차례 아닐 땐 []
	Casinos     []VGCasinoView `json:"casinos"` // 항상 6곳
	Players     []VGPlayerView `json:"players"`
	RoundResult *VGRoundResult `json:"roundResult"` // round_end 동안, 그 외 null
}

// VGGameOverPayload 게임 종료 발표 (동점 공동 승 지원)
type VGGameOverPayload struct {
	WinnerSeats []int          `json:"winnerSeats"`
	WinnerNames []string       `json:"winnerNames"`
	Totals      []int          `json:"totals"` // 좌석 순 최종 총액 (만 단위)
	Players     []VGPlayerView `json:"players"`
}

// VGEventPayload 연출용 이벤트 — 전원에게 동일하게 간다.
// Seat 은 좌석 0 유실 방지를 위해 포인터로 생략을 표현한다.
type VGEventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

type VGPlayerDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type VGPlayerReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type VGErrorPayload struct {
	Message string `json:"message"`
}
