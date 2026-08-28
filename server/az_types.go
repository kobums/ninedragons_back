package server

import "time"

// ==================== 아줄 (Azul) 타입 ====================
//
// 2~4인 타일 드래프트 + 배치 점수. 다인 결(kr_hub/se_hub)을 그대로 따른다.
//
// 정식 한국어 용어를 고정해 쓴다 — 임의 직역 금지.
//
//	Factory display     → 진열대
//	Center              → 중앙
//	Pattern lines       → 패턴 라인
//	Wall                → 벽
//	Floor line          → 바닥 라인
//	First player marker → 선 플레이어 마커
//	라운드 3단계        → 공장 수주 → 벽 타일 붙이기 → 라운드 준비
//
// 타일 5색은 파랑·노랑·빨강·검정·하늘색이고, 와이어 값은 영문으로 고정한다
// (blue/yellow/red/black/cyan, 바닥의 선 마커는 first).
//
// 은닉이 없다. 진열대·중앙·모든 개인 보드가 전부 공개라 관전자도 참가자와
// 완전히 같은 스냅샷을 받는다 (yourSeat 만 -1). 이 게임은 정보가 아니라
// 배치 판단을 겨룬다 — 그래서 az_hub.go 의 buildAZState 에는 viewerSeat 로
// 갈라지는 분기가 yourSeat 하나뿐이다.

const (
	AZMinPlayers = 2
	AZMaxPlayers = 4

	// AZFillBotTarget az_fill_bots 가 채우는 목표 인원 — 채운 뒤 즉시 시작
	AZFillBotTarget = 3

	// AZWallSize 벽·패턴 라인의 한 변 (5×5, 패턴 라인 5줄)
	AZWallSize = 5

	// AZTilesPerColor 색당 타일 수 (5색 × 20 = 100장)
	AZTilesPerColor = 20

	// AZFactoryTiles 라운드 준비에서 진열대 하나에 올리는 타일 수
	AZFactoryTiles = 4

	// AZFloorSlots 바닥 라인 칸 수 — 넘친 타일은 즉시 버린다 (감점은 -3 유지)
	AZFloorSlots = 7

	// AZMaxRounds 무한 게임 방지 캡. 아무도 패턴 라인을 완성하지 않으면
	// 타일이 벽으로 빠져나가지 않아 원리상 끝나지 않는다 — 실전에서는
	// 발동하지 않지만(보통 5~8라운드) 규칙 밖 안전장치로 둔다.
	AZMaxRounds = 30
)

// 대기 상태 마감 타이머 — 차례 60초 무응답은 감점이 가장 적은 수로 자동
// 해소하고, 벽 타일 붙이기 정산은 5초 뒤 자동으로 다음 라운드를 연다.
// 차례 제한은 허브 필드로 복사해 가므로 테스트가 허브마다 따로 낮춘다
// (azTilingDelay 는 연출 지연이라 패키지 var 를 그대로 낮춘다).
var (
	azTurnTimeout = 60 * time.Second // drafting — 자동 수주
	azTilingDelay = 5 * time.Second  // tiling — 자동 다음 라운드
)

// AZColor 타일 색. 와이어 값은 영문 고정이고 화면 표기만 한국어다.
type AZColor string

const (
	// AZColorNone 빈 패턴 라인 (와이어에서는 "")
	AZColorNone AZColor = ""

	AZColorBlue   AZColor = "blue"   // 파랑
	AZColorYellow AZColor = "yellow" // 노랑
	AZColorRed    AZColor = "red"    // 빨강
	AZColorBlack  AZColor = "black"  // 검정
	AZColorCyan   AZColor = "cyan"   // 하늘색

	// AZColorFirst 선 플레이어 마커. 타일이 아니라 표식이라 주머니·버린
	// 타일에 섞이지 않고, 바닥 라인과 중앙 사이만 오간다.
	AZColorFirst AZColor = "first"
)

// azColors 타일 5색의 고정 나열 순서 — 벽 색 배치의 단일 기준이다
var azColors = []AZColor{AZColorBlue, AZColorYellow, AZColorRed, AZColorBlack, AZColorCyan}

// azColorIndex 색 → 나열 인덱스 (0~4). 벽 열 계산의 근거다.
var azColorIndex = map[AZColor]int{
	AZColorBlue: 0, AZColorYellow: 1, AZColorRed: 2, AZColorBlack: 3, AZColorCyan: 4,
}

// azColorLabel 색 한글 표기 (이벤트·로그 문구용)
func azColorLabel(c AZColor) string {
	switch c {
	case AZColorBlue:
		return "파랑"
	case AZColorYellow:
		return "노랑"
	case AZColorRed:
		return "빨강"
	case AZColorBlack:
		return "검정"
	case AZColorCyan:
		return "하늘색"
	case AZColorFirst:
		return "선 플레이어 마커"
	default:
		return "?"
	}
}

// azIsTileColor 실제 타일 색인지 (선 마커·빈 색 제외)
func azIsTileColor(c AZColor) bool {
	_, ok := azColorIndex[c]
	return ok
}

// azFactoryCounts 인원별 진열대 수 — 2인 5개 / 3인 7개 / 4인 9개
var azFactoryCounts = map[int]int{2: 5, 3: 7, 4: 9}

// azFactoryCount 인원별 진열대 수 (표 밖 인원은 0 — 대기실 미리보기용)
func azFactoryCount(n int) int {
	return azFactoryCounts[n]
}

// azFloorPenalties 바닥 라인 감점표 — 1·2번째 -1, 3~5번째 -2, 6·7번째 -3.
// 칸을 넘긴 타일은 놓지 않고 버리지만, 계산 자체는 -3 취급으로 이어진다.
var azFloorPenalties = []int{1, 1, 2, 2, 2, 3, 3}

// azFloorPenalty 바닥 라인 n장의 총 감점(양수). 표를 넘긴 몫은 장당 -3이다.
func azFloorPenalty(n int) int {
	if n <= 0 {
		return 0
	}
	total := 0
	for i := 0; i < n; i++ {
		if i < len(azFloorPenalties) {
			total += azFloorPenalties[i]
			continue
		}
		total += azFloorPenalties[len(azFloorPenalties)-1]
	}
	return total
}

// ==================== 벽 색 배치 (고정 패턴) ====================

// azWallColor 벽 (row, col) 칸의 고정 색. 각 행이 색 순서를 한 칸씩 오른쪽으로
// 밀어 배치한 결과다 — 0행은 파랑·노랑·빨강·검정·하늘색, 1행은 하늘색부터
// 시작한다. 어느 행에도 5색이 한 번씩, 어느 열에도 5색이 한 번씩 온다.
// 범위 밖은 AZColorNone.
func azWallColor(row, col int) AZColor {
	if row < 0 || row >= AZWallSize || col < 0 || col >= AZWallSize {
		return AZColorNone
	}
	return azColors[((col-row)%AZWallSize+AZWallSize)%AZWallSize]
}

// azWallCol 행 row 에서 color 가 놓이는 열. 없는 색이면 -1.
// 위 배치의 역함수다 — col = (row + colorIndex) % 5.
func azWallCol(row int, color AZColor) int {
	idx, ok := azColorIndex[color]
	if !ok || row < 0 || row >= AZWallSize {
		return -1
	}
	return (row + idx) % AZWallSize
}

// ==================== 메시지 ====================

// AZPhase 게임 진행 단계
type AZPhase string

const (
	AZPhaseWaiting  AZPhase = "waiting"
	AZPhaseDrafting AZPhase = "drafting" // 공장 수주 (차례 60초 마감)
	AZPhaseTiling   AZPhase = "tiling"   // 벽 타일 붙이기 정산 (5초 뒤 다음 라운드)
	AZPhaseGameOver AZPhase = "game_over"
)

// AZMessageType 아줄 메시지 타입
type AZMessageType string

const (
	// 클라이언트 → 서버
	AZMsgJoinGame AZMessageType = "az_join_game"
	AZMsgFillBots AZMessageType = "az_fill_bots"
	AZMsgStart    AZMessageType = "az_start"
	AZMsgRejoin   AZMessageType = "az_rejoin"
	AZMsgTake     AZMessageType = "az_take"
	AZMsgReact    AZMessageType = "az_react"

	// 서버 → 클라이언트
	AZMsgPlayerJoined       AZMessageType = "az_player_joined"
	AZMsgSpectateJoined     AZMessageType = "az_spectate_joined"
	AZMsgGameState          AZMessageType = "az_game_state"
	AZMsgEvent              AZMessageType = "az_event"
	AZMsgGameOver           AZMessageType = "az_game_over"
	AZMsgPlayerDisconnected AZMessageType = "az_player_disconnected"
	AZMsgPlayerReconnected  AZMessageType = "az_player_reconnected"
	AZMsgSessionExpired     AZMessageType = "az_session_expired"
	AZMsgError              AZMessageType = "az_error"
)

// 출처 표기 — az_take 의 from 필드. "factory:N" 또는 "center".
const (
	azSourceCenter        = "center"
	azSourceFactoryPrefix = "factory:"
)

// AZLineTargetFloor az_take 의 line 값 중 "전부 바닥 라인"
const AZLineTargetFloor = -1

// ==================== 순수 상태 ====================

// AZLine 패턴 라인 한 줄. 한 줄에는 한 색만 쌓을 수 있어 색·장수로 충분하다.
// Color 가 AZColorNone("") 이면 빈 줄이다. 스냅샷에도 이 모양 그대로 나간다.
type AZLine struct {
	Color AZColor `json:"color"`
	Count int     `json:"count"`
}

// AZPlayer 게임 참가자 (순수 상태 — 연결 매핑은 허브의 azRoom 담당).
// 은닉이 없어 이 구조체의 모든 값이 그대로 전원에게 공개된다.
type AZPlayer struct {
	Seat  int
	Name  string
	Score int

	// Lines 패턴 라인 5줄 (0번 줄 1칸 ~ 4번 줄 5칸)
	Lines [AZWallSize]AZLine
	// Wall 벽 채움 여부. 칸의 색은 azWallColor 로 고정돼 있다.
	Wall [AZWallSize][AZWallSize]bool
	// Floor 바닥 라인 (선 마커는 AZColorFirst). 최대 AZFloorSlots 장.
	Floor []AZColor
}

// AZGameEvent 순수 규칙이 쌓는 연출 이벤트 — 허브가 DrainEvents 로 꺼내
// az_event 로 방송한다. 은닉이 없는 게임이라 담기지 못할 정보가 없다.
type AZGameEvent struct {
	Kind    string
	Seat    int // -1 없음
	Message string
}

// AZAction 직전 수주 (전원 공개). 프론트의 진행 배너 근거다.
type AZAction struct {
	Seat    int    `json:"seat"`
	Name    string `json:"name"`
	Message string `json:"message"`
}

// AZRoundRow 라운드 정산 한 줄 (좌석별 획득·감점·누계)
type AZRoundRow struct {
	Seat    int `json:"seat"`
	Gained  int `json:"gained"`
	Penalty int `json:"penalty"`
	Total   int `json:"total"`
}

// AZRoundResult 벽 타일 붙이기 정산 결과 (tiling 단계에서 노출)
type AZRoundResult struct {
	Rows    []AZRoundRow `json:"rows"`
	Message string       `json:"message"`
}

// AZBonusRow 최종 보너스 내역 — 가로줄 2점·세로줄 7점·같은 색 5장 10점
type AZBonusRow struct {
	Seat   int    `json:"seat"`
	Name   string `json:"name"`
	Rows   int    `json:"rows"`
	Cols   int    `json:"cols"`
	Colors int    `json:"colors"`
	Bonus  int    `json:"bonus"`
	Score  int    `json:"score"`
}

// AZResult 종료 결과 — 최고점 승, 동점이면 완성 가로줄이 많은 쪽, 그래도
// 같으면 공동 승리
type AZResult struct {
	WinnerSeats []int    `json:"winnerSeats"`
	WinnerNames []string `json:"winnerNames"`
	Message     string   `json:"message"`
}

// AZGame 아줄 게임 상태 (순수, 허브 비의존)
type AZGame struct {
	ID      string
	Players []*AZPlayer
	Phase   AZPhase

	// Round 현재 라운드 (1부터, 시작 전 0)
	Round int
	// CurrentSeat 수주 차례 (-1 시작 전)
	CurrentSeat int
	// FirstNextSeat 선 플레이어 마커를 가져간 좌석 = 다음 라운드 선 (-1 미정)
	FirstNextSeat int

	// Factories 진열대별 남은 타일
	Factories [][]AZColor
	// Center 중앙에 밀려난 타일
	Center []AZColor
	// CenterHasFirst 선 플레이어 마커가 아직 중앙에 있는지
	CenterHasFirst bool

	// Bag 주머니 / Discard 버린 타일 (주머니가 비면 섞어 되돌린다)
	Bag     []AZColor
	Discard []AZColor

	LastAction  *AZAction      // 직전 수주 (그 전엔 nil)
	RoundResult *AZRoundResult // 직전 라운드 정산 (그 전엔 nil)
	Result      *AZResult      // 종료 결과 (그 전엔 nil)
	Bonuses     []AZBonusRow   // 최종 보너스 내역 (종료 시에만)
	EndReason   string         // "row_complete" | "tiles_exhausted" | "round_cap"
	Ready       bool
	StartedAt   time.Time

	// pendingEnd 이번 라운드로 끝나는지 (가로줄 완성·타일 소진). tiling 정산을
	// 보여준 뒤 AdvanceRound 에서 실제 종료로 넘어간다.
	pendingEnd bool

	// StateSeq 새 대기 상태(차례·정산)가 열릴 때마다 +1 — 허브가 마감
	// 타이머를 다시 걸지 판단하는 근거
	StateSeq int
	// AfkSeq 마감 타이머 일련번호 (뒤늦은 발화 무시용 — 허브가 관리)
	AfkSeq int
	// Deadline 현재 대기 상태의 마감 시각 (unixMillis — 스냅샷 노출용)
	Deadline int64

	events []AZGameEvent
}

// AZClient 아줄 클라이언트 연결
type AZClient struct {
	wsClient
	Hub  *AZHub
	Seat int
}

// AZMessage 메시지 봉투
type AZMessage struct {
	Type    AZMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type AZJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 관대하게 새로 생성)
	Room string `json:"room,omitempty"`
}

type AZRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// AZTakePayload 공장 수주 — 출처·색·놓을 패턴 라인.
// From 은 "factory:N" 또는 "center", Line 은 0~4 또는 -1(전부 바닥 라인).
// 좌석 0·라인 0 유실을 막기 위해 omitempty 를 쓰지 않는다.
type AZTakePayload struct {
	From  string  `json:"from"`
	Color AZColor `json:"color"`
	Line  int     `json:"line"`
}

type AZReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

// AZPlayerView 좌석별 공개 정보 — 좌석 0·점수 0 유실 방지를 위해 omitempty 금지.
// 은닉이 없으므로 이 구조체가 곧 전원이 보는 좌석 정보 전부다.
// Lines·Wall·Floor 는 항상 배열로 나간다 (nil → JSON null 금지).
type AZPlayerView struct {
	Seat      int       `json:"seat"`
	Name      string    `json:"name"`
	Connected bool      `json:"connected"`
	Bot       bool      `json:"bot"`
	Score     int       `json:"score"`
	Lines     []AZLine  `json:"lines"`
	Wall      [][]bool  `json:"wall"`
	Floor     []AZColor `json:"floor"`
}

// AZGameStatePayload 전체 게임 스냅샷. 모든 상태 변경 후 방송한다.
// 재접속 복원도 같은 페이로드를 쓴다.
//
// 은닉이 없다 — 관전자(viewerSeat -1)도 참가자와 완전히 같은 스냅샷을 받고,
// 다른 값은 yourSeat 하나뿐이다.
type AZGameStatePayload struct {
	GameID   string  `json:"gameId"`
	RoomCode string  `json:"roomCode"`
	Phase    AZPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"` // 관전자는 -1
	// Spectators 관전자 수 (참가자·관전자 모두 표시용)
	Spectators int `json:"spectators"`
	// EndsAt 현재 대기 상태(차례·정산)의 마감 시각 (unixMillis, 그 외 0)
	EndsAt        int64 `json:"endsAt"`
	Round         int   `json:"round"`
	CurrentSeat   int   `json:"currentSeat"`
	FirstNextSeat int   `json:"firstNextSeat"`
	// Factories 진열대별 남은 타일 — 항상 [][] (nil → JSON null 금지)
	Factories [][]AZColor `json:"factories"`
	// Center 중앙 타일 — 항상 []
	Center         []AZColor `json:"center"`
	CenterHasFirst bool      `json:"centerHasFirst"`
	BagLeft        int       `json:"bagLeft"`
	DiscardLeft    int       `json:"discardLeft"`
	// Players 좌석 정보 — 항상 []
	Players     []AZPlayerView `json:"players"`
	LastAction  *AZAction      `json:"lastAction"`  // 그 전엔 null
	RoundResult *AZRoundResult `json:"roundResult"` // 그 전엔 null
	Result      *AZResult      `json:"result"`      // 종료 결과 (그 전엔 null)
}

// AZEventPayload 연출용 이벤트. 전원에게 동일하게 간다.
// Seat 은 좌석 0 유실 방지를 위해 포인터로 생략을 표현한다.
type AZEventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

// AZGameOverPayload 게임 종료 발표 — 최종 보너스 내역까지 함께 보낸다
type AZGameOverPayload struct {
	WinnerSeats []int          `json:"winnerSeats"`
	WinnerNames []string       `json:"winnerNames"`
	Reason      string         `json:"reason"`
	Message     string         `json:"message"`
	Round       int            `json:"round"`
	Bonuses     []AZBonusRow   `json:"bonuses"`
	Players     []AZPlayerView `json:"players"`
}

type AZPlayerDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type AZPlayerReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type AZErrorPayload struct {
	Message string `json:"message"`
}
