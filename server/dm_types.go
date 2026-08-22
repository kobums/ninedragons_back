package server

import "time"

// ==================== 위대한 달무티 (간이 3핸드판) 타입 ====================
//
// 4~8인 클라이밍 카드. 덱 80장(숫자 n = 그 카드가 n장, 1~12) + 조커 2장
// (13으로 표기, 와일드 — 세트에 섞으면 그 숫자 취급, 단독으로 내면 13).
// 리드가 같은 숫자 N장 세트를 내면 이후는 같은 장수의 더 낮은 숫자만 낼 수
// 있다 (낮을수록 강함 — 1이 최강). 전원 연속 패스면 마지막 제출자가 새 리드.
// 먼저 손을 턴 순서가 핸드 순위이고, 순위 점수(1등 = 인원-1점 … 꼴찌 0점)를
// 3핸드 누적해 총점 최고가 승리한다 (동점 공동 우승).

const (
	DMMinPlayers = 4
	DMMaxPlayers = 8

	// DMFillBotTarget dm_fill_bots 가 채우는 목표 인원 — 채운 뒤 즉시 시작
	DMFillBotTarget = 5

	DMHands   = 3  // 총 핸드 수
	DMMaxRank = 12 // 일반 카드 최고 숫자 (숫자 n = n장)

	// DMJoker 조커의 와이어 표기 랭크 — 세트에 섞으면 그 세트의 숫자 취급,
	// 조커만으로 내면 13(가장 약한 숫자) 취급
	DMJoker = 13
)

// DMPhase 게임 진행 단계
type DMPhase string

const (
	DMPhaseWaiting  DMPhase = "waiting"
	DMPhasePlaying  DMPhase = "playing"  // 클라이밍 진행 중 — currentSeat 의 플레이/패스 대기
	DMPhaseHandEnd  DMPhase = "hand_end" // 핸드 정산 표시 — 마감 후 다음 핸드(또는 종료)
	DMPhaseGameOver DMPhase = "game_over"
)

// DMMessageType 달무티 메시지 타입
type DMMessageType string

const (
	// 클라이언트 → 서버
	DMMsgJoinGame DMMessageType = "dm_join_game"
	DMMsgFillBots DMMessageType = "dm_fill_bots"
	DMMsgStart    DMMessageType = "dm_start"
	DMMsgRejoin   DMMessageType = "dm_rejoin"
	DMMsgPlay     DMMessageType = "dm_play"
	DMMsgPass     DMMessageType = "dm_pass"
	DMMsgReact    DMMessageType = "dm_react"

	// 서버 → 클라이언트
	DMMsgPlayerJoined       DMMessageType = "dm_player_joined"
	DMMsgSpectateJoined     DMMessageType = "dm_spectate_joined"
	DMMsgGameState          DMMessageType = "dm_game_state"
	DMMsgEvent              DMMessageType = "dm_event"
	DMMsgGameOver           DMMessageType = "dm_game_over"
	DMMsgPlayerDisconnected DMMessageType = "dm_player_disconnected"
	DMMsgPlayerReconnected  DMMessageType = "dm_player_reconnected"
	DMMsgSessionExpired     DMMessageType = "dm_session_expired"
	DMMsgError              DMMessageType = "dm_error"
)

// DMPlayer 게임 참가자 (순수 상태 — 연결 매핑은 허브의 dmRoom 담당)
type DMPlayer struct {
	Seat    int
	Name    string
	Hand    []int // 손패 랭크들 (오름차순 유지, 조커 13)
	OutRank int   // 이번 핸드에서 손을 턴 순위 (1~인원, 0 = 미확정)
	Points  int   // 3핸드 누적 점수
}

// DMTableSet 현재 테이블의 세트 — 이길 대상 (리드 대기 중이면 nil)
type DMTableSet struct {
	Rank  int `json:"rank"`  // 세트 숫자 (조커 단독은 13)
	Count int `json:"count"` // 장수
	Seat  int `json:"seat"`  // 마지막 제출자 좌석
}

// DMHandResult 핸드 정산 기록 — hand_end 스냅샷의 handResult 로 나간다
type DMHandResult struct {
	Order   []int  `json:"order"` // 순위 순 좌석 (1등부터)
	Message string `json:"message"`
}

// DMGameEvent 순수 규칙이 쌓는 연출 이벤트 — 허브가 DrainEvents 로 꺼내
// dm_event 로 방송한다 (타인의 손패 내용은 절대 담지 않는다)
type DMGameEvent struct {
	Kind    string
	Seat    int // -1 없음
	Message string
}

// DMGame 달무티 게임 상태 (순수, 허브 비의존)
type DMGame struct {
	ID      string
	Players []*DMPlayer
	Phase   DMPhase

	HandNo      int // 현재 핸드 번호 (1~DMHands)
	CurrentSeat int // 플레이/패스 차례 좌석 (-1 없음)
	LeadSeat    int // 현재 트릭의 리드 좌석
	Table       *DMTableSet
	HandResult  *DMHandResult

	// NextLeadSeat 다음 핸드의 리드 (직전 핸드 1등)
	NextLeadSeat int
	// OutCount 이번 핸드에서 손을 턴 인원 수
	OutCount int

	// WinnerSeats 3핸드 총점 최고 좌석들 (동점 공동)
	WinnerSeats []int
	Ready       bool
	StartedAt   time.Time

	// StateSeq 대기 상태(차례·핸드 정산)가 새로 열릴 때마다 +1 —
	// 허브가 마감 타이머를 다시 걸지 판단하는 근거
	StateSeq int
	// AfkSeq 마감 타이머 일련번호 (뒤늦은 발화 무시용 — 허브가 관리)
	AfkSeq int
	// Deadline 현재 대기 상태의 마감 시각 (unixMillis — 스냅샷 노출용)
	Deadline int64

	events []DMGameEvent
}

// DMClient 달무티 클라이언트 연결
type DMClient struct {
	wsClient
	Hub  *DMHub
	Seat int
}

// DMMessage 메시지 봉투
type DMMessage struct {
	Type    DMMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type DMJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type DMRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// DMPlayPayload 낼 카드 — 랭크 배열 (조커는 13, 세트에 섞으면 그 숫자 취급)
type DMPlayPayload struct {
	Cards []int `json:"cards"`
}

type DMReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

// DMPlayerView 좌석별 공개 정보 — 좌석 0·점수 0 유실 방지를 위해 omitempty 금지
type DMPlayerView struct {
	Seat      int    `json:"seat"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Bot       bool   `json:"bot"`
	HandCount int    `json:"handCount"` // 남은 손패 장수 (공개 정보)
	Out       bool   `json:"out"`       // 이번 핸드 순위 확정 여부
	Rank      int    `json:"rank"`      // 핸드 내 순위 (0 = 미확정)
	Points    int    `json:"points"`    // 3핸드 누적 점수
}

// DMGameStatePayload 개인화된 전체 게임 스냅샷. 모든 상태 변경 후 좌석마다
// 따로 만들어 보낸다. 재접속 복원도 같은 페이로드를 쓴다.
// 은닉: yourHand 는 본인 스냅샷에만 실린다 — 빈 손이어도 빈 배열 [](nil 금지),
// 타인·관전자의 raw JSON 에는 필드 자체가 없다 (포인터 nil 생략).
type DMGameStatePayload struct {
	GameID   string  `json:"gameId"`
	RoomCode string  `json:"roomCode"`
	Phase    DMPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"` // 관전자는 -1
	// Spectators 관전자 수 (참가자·관전자 모두 표시용)
	Spectators int `json:"spectators"`
	// EndsAt 현재 대기 상태(차례·핸드 정산)의 마감 시각 (unixMillis, 그 외 0)
	EndsAt      int64 `json:"endsAt"`
	HandNo      int   `json:"handNo"` // 1~DMHands
	CurrentSeat int   `json:"currentSeat"`
	LeadSeat    int   `json:"leadSeat"`
	// TableSet 현재 이길 대상 세트 (리드 대기 중이면 null)
	TableSet *DMTableSet `json:"tableSet"`
	// YourHand 본인만: 오름차순 랭크 배열 (빈 손은 []) — 타인·관전자 필드 부재
	YourHand   *[]int         `json:"yourHand,omitempty"`
	Players    []DMPlayerView `json:"players"`
	HandResult *DMHandResult  `json:"handResult"` // hand_end 정산 (그 외 null)
}

// DMEventPayload 연출용 이벤트. 타인의 손패 내용을 담지 않으며 전원에게
// 동일하게 간다. Seat 은 좌석 0 유실 방지를 위해 포인터로 생략을 표현한다.
type DMEventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

// DMGameOverPayload 게임 종료 발표 (3핸드 총점 최고 — 동점 공동 우승)
type DMGameOverPayload struct {
	WinnerSeats []int          `json:"winnerSeats"`
	WinnerNames []string       `json:"winnerNames"`
	Players     []DMPlayerView `json:"players"`
	Message     string         `json:"message"`
}

type DMPlayerDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type DMPlayerReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type DMErrorPayload struct {
	Message string `json:"message"`
}
