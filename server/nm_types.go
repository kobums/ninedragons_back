package server

import "time"

// ==================== 6 님트 타입 ====================
//
// 2~10인 동시 선택 카드 게임. 핵심 반전: 전원이 동시에 카드 1장을 은닉
// 제출(picking — 제출 여부만 공개)하고, 전원 제출 시 일괄 공개(revealing)한
// 뒤 낮은 카드부터 순서대로 4개 행에 배치한다. 행의 6번째 카드가 되면 기존
// 5장을 벌점(소머리)으로 먹고, 모든 행 끝보다 작으면 행 하나를 골라 먹는다
// (choosing_row — 그 사람만). 10트릭 후 소머리 합 최소가 승리한다.

// NMMinPlayers / NMMaxPlayers 인원 범위 (2~10인)
const (
	NMMinPlayers = 2
	NMMaxPlayers = 10

	// NMFillBotTarget nm_fill_bots 가 채우는 목표 인원 — 채운 뒤 즉시 시작
	NMFillBotTarget = 6

	NMTricks      = 10  // 트릭 수 (= 손패 장수)
	NMHandSize    = 10  // 각자 배분받는 카드 수
	NMRows        = 4   // 배치 행 수
	NMRowCapacity = 5   // 행 최대 카드 수 — 6번째가 되면 행을 먹는다
	NMDeckSize    = 104 // 카드 1~104
)

// NMPhase 게임 진행 단계
type NMPhase string

const (
	NMPhaseWaiting     NMPhase = "waiting"
	NMPhasePicking     NMPhase = "picking"      // 전원 동시 선택 (picks 은닉)
	NMPhaseRevealing   NMPhase = "revealing"    // 일괄 공개 — 낮은 순 배치
	NMPhaseChoosingRow NMPhase = "choosing_row" // 최소 카드 — 먹을 행 선택 대기
	NMPhaseGameOver    NMPhase = "game_over"
)

// NMMessageType 6 님트 메시지 타입
type NMMessageType string

const (
	// 클라이언트 → 서버
	NMMsgJoinGame  NMMessageType = "nm_join_game"
	NMMsgFillBots  NMMessageType = "nm_fill_bots"
	NMMsgStart     NMMessageType = "nm_start"
	NMMsgRejoin    NMMessageType = "nm_rejoin"
	NMMsgPick      NMMessageType = "nm_pick"
	NMMsgChooseRow NMMessageType = "nm_choose_row"
	NMMsgReact     NMMessageType = "nm_react"

	// 서버 → 클라이언트
	NMMsgPlayerJoined       NMMessageType = "nm_player_joined"
	NMMsgSpectateJoined     NMMessageType = "nm_spectate_joined"
	NMMsgGameState          NMMessageType = "nm_game_state"
	NMMsgEvent              NMMessageType = "nm_event"
	NMMsgGameOver           NMMessageType = "nm_game_over"
	NMMsgPlayerDisconnected NMMessageType = "nm_player_disconnected"
	NMMsgPlayerReconnected  NMMessageType = "nm_player_reconnected"
	NMMsgSessionExpired     NMMessageType = "nm_session_expired"
	NMMsgError              NMMessageType = "nm_error"
)

// NMPlayer 게임 참가자 (순수 상태 — 연결 매핑은 허브의 nmRoom 담당)
type NMPlayer struct {
	Seat    int
	Name    string
	Hand    []int // 손패 (오름차순 정렬 유지, 선택 시 즉시 제거)
	Pick    int   // 이번 트릭에 제출한 카드 (0 = 미제출 — 카드는 1~104)
	Penalty int   // 누적 벌점 소머리 합
}

// NMPickEntry 공개된 제출 카드 한 건 (revealing 에 일괄 공개 — 낮은 순 정렬)
type NMPickEntry struct {
	Seat int `json:"seat"`
	Card int `json:"card"`
}

// NMPlacement 배치 한 건 — 스냅샷의 lastPlacement 로 노출 (연출용)
type NMPlacement struct {
	Seat int  `json:"seat"`
	Card int  `json:"card"`
	Row  int  `json:"row"`
	Ate  bool `json:"ate"` // 행을 먹었는지 (6번째·행 선택)
}

// NMGame 6 님트 게임 상태 (순수, 허브 비의존)
type NMGame struct {
	ID      string
	Players []*NMPlayer
	Phase   NMPhase

	Trick int     // 1~10
	Rows  [][]int // 4개 행 (각 행 1~5장, 시작 카드 1장)

	// Picks 이번 트릭 제출 전체 (reveal 시점에 카드 오름차순으로 확정).
	// picking 중에는 비어 있다 — 은닉은 스냅샷이 아니라 여기서부터 지킨다.
	Picks []NMPickEntry
	// Pending 아직 배치되지 않은 제출 (Picks 의 앞부분부터 소진)
	Pending []NMPickEntry

	ChooserSeat   int          // 행 선택 대기 좌석 (-1 = 없음)
	LastPlacement *NMPlacement // 마지막 배치 (트릭 시작 시 nil)

	WinnerSeats []int // 게임 승자 (소머리 최소 — 동점 공동 승)
	Ready       bool
	StartedAt   time.Time
}

// NMClient 6 님트 클라이언트 연결
type NMClient struct {
	wsClient
	Hub  *NMHub
	Seat int
}

// NMMessage 메시지 봉투
type NMMessage struct {
	Type    NMMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type NMJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type NMRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// NMPickPayload 동시 선택 제출 카드 (1~104, 자기 손패에 있어야 한다)
type NMPickPayload struct {
	Card int `json:"card"`
}

// NMChooseRowPayload 먹을 행 선택 (0~3)
type NMChooseRowPayload struct {
	Row int `json:"row"`
}

// NMReactPayload 리액션 이모지 (화이트리스트 외는 조용히 무시)
type NMReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

// NMPlayerView 좌석별 공개 정보. picking 중 제출 카드는 절대 싣지 않는다 —
// 제출 여부(picked)만 공개한다. penalty 는 누적 소머리 합.
type NMPlayerView struct {
	Seat      int    `json:"seat"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Bot       bool   `json:"bot"`
	Picked    bool   `json:"picked"`
	Penalty   int    `json:"penalty"`
}

// NMGameStatePayload 개인화된 전체 게임 스냅샷. 모든 상태 변경 후 좌석마다
// 따로 만들어 보낸다. 재접속 복원도 같은 페이로드를 쓴다.
// 은닉: yourHand 는 본인만 실값(타인·관전자는 빈 배열 []), picks 는
// revealing/choosing_row 에만 존재한다 (picking 중엔 필드 자체가 부재).
type NMGameStatePayload struct {
	GameID   string  `json:"gameId"`
	RoomCode string  `json:"roomCode"`
	Phase    NMPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"` // 관전자는 -1
	// Spectators 관전자 수 (참가자·관전자 모두 표시용)
	Spectators int `json:"spectators"`
	// EndsAt 현재 단계의 자동 진행 마감 시각 (unixMillis, waiting/game_over 0)
	EndsAt int64 `json:"endsAt"`
	Trick  int   `json:"trick"`
	// Rows 4개 행 — 공개 정보. nil 이 JSON null 로 나가지 않게 빈 배열 보장.
	Rows     [][]int `json:"rows"`
	YourHand []int   `json:"yourHand"`
	// Picks revealing 에 일괄 공개 (카드 오름차순). picking 중엔 부재.
	Picks         []NMPickEntry  `json:"picks,omitempty"`
	Players       []NMPlayerView `json:"players"`
	ChooserSeat   int            `json:"chooserSeat"` // -1 = 없음
	LastPlacement *NMPlacement   `json:"lastPlacement"`
}

// NMEventPayload 연출용 이벤트. 비밀 정보를 담지 않으며 전원에게 동일하게
// 간다. Seat 은 좌석 0 유실 방지를 위해 포인터로 생략을 표현한다.
type NMEventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

// NMGameOverPayload 게임 종료 발표 (소머리 최소 승 — 동점 공동 승)
type NMGameOverPayload struct {
	WinnerSeats []int          `json:"winnerSeats"`
	WinnerNames []string       `json:"winnerNames"`
	Penalties   []int          `json:"penalties"` // 좌석 순 최종 소머리 합
	Players     []NMPlayerView `json:"players"`
}

type NMPlayerDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type NMPlayerReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type NMErrorPayload struct {
	Message string `json:"message"`
}
