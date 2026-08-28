package server

import "time"

// ==================== 리코셰 로봇 (Ricochet Robots) 타입 ====================
//
// 1~8인 **동시 퍼즐 풀기 + 외침**. 세트(se)·더 마인드(mi)와 같은 계열로
// **차례가 없다** — currentSeat 도, 좌석별 AFK 자동 진행도 없다. 누구든 언제든
// rr_bid 를 보내고, 허브 고루틴이 도착 순서대로 직렬 판정한다(선착 판정 모델).
// 같은 횟수를 동시에 외치면 먼저 도착한 쪽이 증명권을 가진다.
//
// 은닉도 없다. 판(벽)·로봇 위치·목표·외침이 전부 공개라 관전자도 참가자와
// 똑같은 스냅샷을 받는다 (yourSeat 만 -1). 이 게임은 정보가 아니라
// "누가 더 짧은 해를 먼저 찾는가"를 겨룬다.
//
// 판은 16×16. 로봇은 고른 방향으로 **벽이나 다른 로봇에 막힐 때까지 미끄러진다**
// — 한 칸씩은 못 움직인다. 중앙 2×2는 진입 불가다.
//
// 용어(스펙 고정): 로봇 / 목표 / 외침 / 증명 / 벽.

const (
	RRMinPlayers = 1 // 혼자서도 연습할 수 있다 (차례가 없어 1인도 성립)
	RRMaxPlayers = 8

	// RRFillBotTarget rr_fill_bots 가 채우는 목표 인원 — 호스트 + 연습봇 3.
	// 채운 뒤 즉시 시작한다.
	RRFillBotTarget = 4

	// RRSize 판 한 변의 길이 (16×16)
	RRSize = 16
	// RRCellCount 칸 수
	RRCellCount = RRSize * RRSize

	// RRRobotCount 로봇 대수 (빨간색·파란색·초록색·노란색)
	RRRobotCount = 4

	// RRGoalTotal 한 판에 소진하는 목표 수
	RRGoalTotal = 17

	// RRMaxDepth rrSolve 의 기본 탐색 상한 — 넘으면 "해 없음"으로 처리한다
	RRMaxDepth = 12

	// RRMinGoalMoves / RRMaxGoalMoves 판 생성이 보장하는 최소 횟수의 범위.
	// 1수짜리는 시시하고 11수 이상은 사람이 못 푼다.
	RRMinGoalMoves = 2
	RRMaxGoalMoves = 10

	// RRMaxBid 외칠 수 있는 최대 횟수 (오타·장난 입력 방어)
	RRMaxBid = 99
)

// 시간 상수 — 테스트 init 에서 짧게 낮춘다. 허브가 New 에서 필드로 복사해
// 가므로 테스트는 허브 필드를 바꾼다 (허브 고루틴과 경합 금지).
var (
	// rrBidWindow 첫 외침이 나온 뒤의 카운트다운. 추가 외침이 와도
	// **다시 걸지 않는다** — 규칙상 60초는 한 번만 흐른다.
	rrBidWindow = 60 * time.Second

	// rrGoalCap 아무도 외치지 않는 목표를 넘기는 상한
	rrGoalCap = 5 * time.Minute

	// rrDemoCap 증명자 한 명에게 주는 시간 (초과는 실패 처리)
	rrDemoCap = 45 * time.Second

	// rrGoalEndDelay 목표 정산을 보여주는 시간
	rrGoalEndDelay = 3 * time.Second

	// rrGameCap 게임 전체 캡 — 무한 게임 방지 안전장치
	rrGameCap = 30 * time.Minute
)

// ==================== 색 / 방향 / 벽 ====================

// RRColor 로봇 색 (와이어 값)
type RRColor string

const (
	RRRed    RRColor = "red"    // 빨간색
	RRBlue   RRColor = "blue"   // 파란색
	RRGreen  RRColor = "green"  // 초록색
	RRYellow RRColor = "yellow" // 노란색
)

// rrColors 색 인덱스의 단일 기준 — 로봇 배열 순서와 같다
var rrColors = [RRRobotCount]RRColor{RRRed, RRBlue, RRGreen, RRYellow}

// rrColorIndex 색 → 인덱스(0~3). 없는 색은 -1.
func rrColorIndex(c RRColor) int {
	for i, name := range rrColors {
		if name == c {
			return i
		}
	}
	return -1
}

// rrColorLabel 색 한글 표기 (로그·문구용)
func rrColorLabel(c RRColor) string {
	switch c {
	case RRRed:
		return "빨간색"
	case RRBlue:
		return "파란색"
	case RRGreen:
		return "초록색"
	case RRYellow:
		return "노란색"
	default:
		return "?"
	}
}

// RRDir 이동 방향 (와이어 값)
type RRDir string

const (
	RRUp    RRDir = "up"
	RRRight RRDir = "right"
	RRDown  RRDir = "down"
	RRLeft  RRDir = "left"
)

// rrDirs 방향 인덱스의 단일 기준 — 벽 비트와 순서가 같다
var rrDirs = [4]RRDir{RRUp, RRRight, RRDown, RRLeft}

// rrDirIndex 방향 → 인덱스(0~3). 없는 방향은 -1.
func rrDirIndex(d RRDir) int {
	for i, name := range rrDirs {
		if name == d {
			return i
		}
	}
	return -1
}

// rrDirLabel 방향 한글 표기
func rrDirLabel(d RRDir) string {
	switch d {
	case RRUp:
		return "위"
	case RRRight:
		return "오른쪽"
	case RRDown:
		return "아래"
	case RRLeft:
		return "왼쪽"
	default:
		return "?"
	}
}

// 벽 비트마스크 — 칸마다 상하좌우 벽 여부를 한 바이트에 담는다.
// 스냅샷의 walls[r][c] 가 이 값이며, 프론트는 칸 경계의 굵은 선으로 그린다.
const (
	rrWallUp    uint8 = 1
	rrWallRight uint8 = 2
	rrWallDown  uint8 = 4
	rrWallLeft  uint8 = 8
)

// rrDirBit 방향 인덱스 → 벽 비트
var rrDirBit = [4]uint8{rrWallUp, rrWallRight, rrWallDown, rrWallLeft}

// rrOpposite 반대 방향 인덱스 (벽은 양쪽 칸에 동시에 기록한다)
var rrOpposite = [4]int{2, 3, 0, 1}

// RRBoard 판 — 벽과 진입 불가 칸. 로봇 위치는 담지 않는다(순수 판).
// 칸은 r*RRSize+c 로 색인한다.
type RRBoard struct {
	Walls   [RRCellCount]uint8
	Blocked [RRCellCount]bool
}

// RRCell 좌표 (와이어 값)
type RRCell struct {
	R int `json:"r"`
	C int `json:"c"`
}

// RRGoal 목표 — 그 색 로봇이 그 칸에 도착해야 한다 (전원 공개)
type RRGoal struct {
	Color RRColor `json:"color"`
	R     int     `json:"r"`
	C     int     `json:"c"`
}

// RRMove 이동 하나 — 로봇 하나를 한 방향으로 (막힐 때까지 미끄러진다)
type RRMove struct {
	Robot RRColor `json:"robot"`
	Dir   RRDir   `json:"dir"`
}

// ==================== 게임 상태 ====================

// RRPhase 게임 진행 단계
type RRPhase string

const (
	RRPhaseWaiting  RRPhase = "waiting"   // 대기실
	RRPhaseThinking RRPhase = "thinking"  // 목표가 열렸고 아직 외침이 없다
	RRPhaseBidding  RRPhase = "bidding"   // 첫 외침 후 카운트다운
	RRPhaseDemo     RRPhase = "demo"      // 가장 적게 외친 사람이 증명한다
	RRPhaseGoalEnd  RRPhase = "goal_end"  // 목표 정산 표시
	RRPhaseGameOver RRPhase = "game_over" // 종료
)

// RRMessageType 리코셰 로봇 메시지 타입
type RRMessageType string

const (
	// 클라이언트 → 서버
	RRMsgJoinGame RRMessageType = "rr_join_game"
	RRMsgFillBots RRMessageType = "rr_fill_bots"
	RRMsgStart    RRMessageType = "rr_start"
	RRMsgRejoin   RRMessageType = "rr_rejoin"
	RRMsgBid      RRMessageType = "rr_bid"
	RRMsgDemo     RRMessageType = "rr_demo"
	RRMsgPass     RRMessageType = "rr_pass"
	RRMsgReact    RRMessageType = "rr_react"

	// 서버 → 클라이언트
	RRMsgPlayerJoined       RRMessageType = "rr_player_joined"
	RRMsgSpectateJoined     RRMessageType = "rr_spectate_joined"
	RRMsgGameState          RRMessageType = "rr_game_state"
	RRMsgEvent              RRMessageType = "rr_event"
	RRMsgGameOver           RRMessageType = "rr_game_over"
	RRMsgPlayerDisconnected RRMessageType = "rr_player_disconnected"
	RRMsgPlayerReconnected  RRMessageType = "rr_player_reconnected"
	RRMsgSessionExpired     RRMessageType = "rr_session_expired"
	RRMsgError              RRMessageType = "rr_error"
)

// 판정 문구 — lastResult.message 와 이벤트에 그대로 실린다
const (
	rrDemoOKMsg      = "증명 성공!"
	rrDemoMissMsg    = "목표 지점에 닿지 못했습니다"
	rrDemoOverMsg    = "외친 횟수를 넘겼습니다"
	rrDemoStuckMsg   = "움직일 수 없는 방향입니다"
	rrDemoBadMoveMsg = "잘못된 이동입니다"
	rrDemoPassMsg    = "증명을 포기했습니다"
	rrDemoTimeUpMsg  = "증명 시간이 끝났습니다"
	rrNobodySolved   = "아무도 성공하지 못했습니다"
	rrNobodyBid      = "아무도 이동 횟수를 외치지 못했습니다"
)

// RRPlayer 게임 참가자 (순수 상태 — 연결 매핑은 허브의 rrRoom 담당)
type RRPlayer struct {
	Seat int
	Name string
	// Score 획득한 목표 카드 수
	Score int
}

// RRBid 외침 하나. Seq 는 도착 순서로, 같은 횟수를 외쳤을 때
// **먼저 도착한 쪽이 앞선다**는 선착 판정의 근거다.
type RRBid struct {
	Seat  int
	Moves int
	Seq   int
}

// RRGameEvent 순수 규칙이 쌓는 연출 이벤트 — 허브가 DrainEvents 로 꺼내
// rr_event 로 방송한다. 은닉이 없는 게임이라 담기지 못할 정보가 없다.
type RRGameEvent struct {
	Kind    string
	Seat    int // -1 없음
	Message string
}

// RRLastResult 직전 증명 판정 (전원 공개). 프론트의 성공/실패 배너 근거다.
// 아무도 성공하지 못한 목표는 Seat -1 로 실린다.
type RRLastResult struct {
	Seat    int    `json:"seat"`
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Moves   int    `json:"moves"`
	Message string `json:"message"`
}

// RRResult 종료 결과 — 최다 획득 승, 동점이면 공동 승리
type RRResult struct {
	WinnerSeats []int    `json:"winnerSeats"`
	WinnerNames []string `json:"winnerNames"`
	Message     string   `json:"message"`
}

// RRGame 리코셰 로봇 게임 상태 (순수, 허브 비의존).
// 차례가 없으므로 CurrentSeat·AfkSeq 가 없다. 단계 마감을 다시 걸기 위한
// StateSeq 와 전체 캡용 EndSeq 만 둔다.
type RRGame struct {
	ID      string
	Players []*RRPlayer
	Phase   RRPhase

	// Board 벽 배치 (한 판 내내 고정)
	Board *RRBoard
	// Robots 로봇 위치 (색 인덱스 → 칸). 증명이 성립하면 그 결과가 남는다.
	Robots [RRRobotCount]uint8

	// Goal 지금 걸린 목표, GoalIndex 몇 번째 목표인지(0-based)
	Goal      RRGoal
	GoalIndex int
	// MinMoves 지금 목표의 최소 횟수 (서버 내부 검증·로그용 — 스냅샷에는 싣지
	// 않는다. 정답을 흘리면 게임이 성립하지 않는다)
	MinMoves int
	// usedGoals 이미 나온 (색,칸) 조합 — 같은 목표를 두 번 내지 않는다
	usedGoals map[int]bool

	// Bids 도착 순으로 쌓은 외침. 스냅샷은 적은 횟수 → 먼저 도착 순으로 정렬한다.
	Bids   []RRBid
	bidSeq int

	// DemoOrder 증명 순서 (적게 외친 사람부터), DemoTurn 지금 증명할 차례의 위치
	DemoOrder []int
	DemoTurn  int

	LastResult *RRLastResult // 직전 목표 정산 (그 전엔 nil)
	Result     *RRResult     // 종료 결과 (그 전엔 nil)
	EndReason  string        // "goals_done" | "time_up" | "no_goal"
	Ready      bool
	StartedAt  time.Time

	// StateSeq 단계 마감 타이머 일련번호 (뒤늦은 발화 무시용 — 허브가 관리)
	StateSeq int
	// EndSeq 전체 캡 타이머 일련번호
	EndSeq int
	// Deadline 현재 단계의 마감 시각 (unixMillis — 스냅샷 노출용)
	Deadline int64

	events []RRGameEvent
}

// RRClient 리코셰 로봇 클라이언트 연결
type RRClient struct {
	wsClient
	Hub  *RRHub
	Seat int
}

// RRMessage 메시지 봉투
type RRMessage struct {
	Type    RRMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type RRJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 관대하게 새로 생성)
	Room string `json:"room,omitempty"`
}

type RRRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// RRBidPayload 외침 — 몇 번 만에 풀 수 있는지
type RRBidPayload struct {
	Moves int `json:"moves"`
}

// RRDemoPayload 증명 제출 — 이동 순서를 그대로 보낸다
type RRDemoPayload struct {
	Moves []RRMove `json:"moves"`
}

type RRReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

// RRPlayerView 좌석별 공개 정보 — 좌석 0·점수 0 유실 방지를 위해 omitempty 금지.
// 은닉이 없으므로 이 구조체가 곧 전원이 보는 좌석 정보 전부다.
type RRPlayerView struct {
	Seat      int    `json:"seat"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Bot       bool   `json:"bot"`
	Score     int    `json:"score"`
}

// RRBidView 외침 하나 (전원 공개). 적은 횟수 → 먼저 도착 순으로 정렬해 싣는다.
type RRBidView struct {
	Seat  int `json:"seat"`
	Moves int `json:"moves"`
}

// RRGameStatePayload 전체 게임 스냅샷. 모든 상태 변경 후 방송한다.
// 재접속 복원도 같은 페이로드를 쓴다.
//
// **은닉이 없다** — 관전자(viewerSeat -1)도 참가자와 완전히 같은 스냅샷을 받고,
// 다른 값은 yourSeat 하나뿐이다. 판·로봇·목표·외침이 전부 공개다.
// 다만 최소 횟수(MinMoves)는 정답이라 서버에만 둔다.
type RRGameStatePayload struct {
	GameID   string  `json:"gameId"`
	RoomCode string  `json:"roomCode"`
	Phase    RRPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"` // 관전자는 -1
	// Spectators 관전자 수 (참가자·관전자 모두 표시용)
	Spectators int `json:"spectators"`
	// EndsAt 현재 단계의 마감 시각 (unixMillis, 마감이 없으면 0)
	EndsAt int64 `json:"endsAt"`

	// GoalIndex 진행 중인 목표 번호(0-based), GoalTotal 전체 목표 수
	GoalIndex int `json:"goalIndex"`
	GoalTotal int `json:"goalTotal"`

	// Walls 칸마다 상하좌우 벽 비트마스크 — 항상 16×16 (nil → JSON null 금지)
	Walls [][]int `json:"walls"`
	// Robots 색 → 위치 (전원 공개)
	Robots map[RRColor]RRCell `json:"robots"`
	// Goal 지금 걸린 목표 (전원 공개)
	Goal RRGoal `json:"goal"`
	// Bids 외침 현황 — 적은 순 정렬, 전원 공개. 항상 []
	Bids []RRBidView `json:"bids"`
	// DemoSeat 지금 증명할 사람 (없으면 -1)
	DemoSeat int `json:"demoSeat"`

	// Players 좌석 정보 — 항상 []
	Players    []RRPlayerView `json:"players"`
	LastResult *RRLastResult  `json:"lastResult"` // 그 전엔 null
	Result     *RRResult      `json:"result"`     // 종료 결과 (그 전엔 null)
}

// RREventPayload 연출용 이벤트. 전원에게 동일하게 간다.
// Seat 은 좌석 0 유실 방지를 위해 포인터로 생략을 표현한다.
type RREventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

// RRGameOverPayload 게임 종료 발표
type RRGameOverPayload struct {
	WinnerSeats []int          `json:"winnerSeats"`
	WinnerNames []string       `json:"winnerNames"`
	Reason      string         `json:"reason"` // "goals_done" | "time_up" | "no_goal"
	Message     string         `json:"message"`
	GoalsPlayed int            `json:"goalsPlayed"`
	Players     []RRPlayerView `json:"players"`
}

type RRPlayerDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type RRPlayerReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type RRErrorPayload struct {
	Message string `json:"message"`
}
