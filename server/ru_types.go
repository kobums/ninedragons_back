package server

import "time"

// ==================== 루미큐브 (Rummikub) 타입 ====================
//
// 2~4인 타일 조합 게임. 규칙 자체는 작지만 **"숫자조합"(테이블 재배치)
// 유효성 검사**가 이 게임의 전부다 — 여기가 신규 난이도다.
//
// 용어는 한국 공식 유통사(놀이속의세상) 표기를 그대로 쓴다. 임의 표기 금지.
//
//	Tile         → 타일
//	Set          → 세트 (그룹과 연속을 아우르는 말)
//	Group        → 그룹   — 색이 다른 같은 숫자 3~4개
//	Run          → 연속   — 색이 같고 숫자가 이어지는 3개 이상
//	Initial meld → 등록   — 첫 내려놓기 30점 이상
//	Manipulation → 숫자조합 — 테이블 위 세트를 재배치하는 것
//	Joker        → 조커
//	Pool         → 타일더미
//	Rack         → 받침대
//	색 4종        → 빨강 · 파랑 · 검정 · 주황
//
// ───────────────── 차례 커밋 모델 (이 게임의 심장) ─────────────────
//
// 재배치 게임에서 증분 프로토콜(타일 하나 옮길 때마다 메시지)은 지옥이다.
// 그래서 **서버는 차례 중간 상태를 아예 갖지 않는다.**
//
//	  ┌──────────────────────────────────────────┐
//	  │ 서버 상태 = 언제나 "차례 시작 상태"          │
//	  │   RUGame.Sets (테이블) · Players[].Rack    │
//	  └───────────────┬──────────────────────────┘
//	                  │ 차례 중 클라이언트가 자유롭게 배치를 바꾼다
//	                  │ (서버는 아무것도 모른다 — 되돌리기는 프론트 로컬)
//	                  ▼
//	  ru_commit {sets:[[tileId,...],...]}  ← 테이블 전체 배치를 통째로
//	                  │
//	                  ▼  ①테이블 전체 유효 ②내 타일 최소 1개 ③등록 전이면
//	                     30점 ④등록 차례면 숫자조합 없음 ⑤테이블 타일이
//	                     받침대로 돌아오지 않음
//	         ┌────────┴────────┐
//	    통과 │                 │ 하나라도 실패
//	         ▼                 ▼
//	   상태 갱신 · 차례 종료   **아무것도 바꾸지 않는다**
//	                          (= 차례 시작 상태 그대로. 부분 적용은 없다)
//
// 검증은 전부 사본 위에서 하고, 전부 통과한 뒤에야 실제 상태에 반영한다.
// 따라서 "거부 시 원복"은 구현상 "거부 시 아무것도 안 함"과 같다 —
// 부분 적용이 물리적으로 불가능하다. ru_game_test.go 가 이걸 못박는다.
//
// ───────────────── 은닉 ─────────────────
//
// yourRack·yourMelded 는 본인 스냅샷에만 실린다. 타인·관전자(viewerSeat -1)
// 의 raw JSON 에는 **키 자체가 없다**(포인터 + omitempty). 타일더미의 내용도
// 어디에도 나가지 않는다 — 남은 개수(poolLeft)만 공개다.

const (
	RUMinPlayers = 2
	RUMaxPlayers = 4

	// RUFillBotTarget ru_fill_bots 가 채우는 목표 인원 — 채운 뒤 즉시 시작
	RUFillBotTarget = 3

	// RUStartRack 시작 받침대 타일 수
	RUStartRack = 14

	// RUInitialMeld 등록(첫 내려놓기) 최소 점수
	RUInitialMeld = 30

	// RUJokerScore 남은 조커의 벌점 (숫자 타일은 적힌 숫자 그대로)
	RUJokerScore = 50

	// RUNoMeldPenalty 등록도 못 하고 끝난 사람의 벌점 — 타일 점수와 무관
	RUNoMeldPenalty = 100

	// RUMaxNum 타일 숫자 상한 (13 다음은 없다 — 연속은 감기지 않는다)
	RUMaxNum = 13

	// RUCopies 색·숫자마다 같은 타일이 몇 벌 있는지
	RUCopies = 2

	// RUJokers 조커 개수
	RUJokers = 2

	// RUMaxTurns 병리적 교착의 안전망. 정상 판은 받침대 비우기 또는
	// 타일더미 소진으로 훨씬 먼저 끝난다.
	RUMaxTurns = 600
)

// RUColor 타일 색 (와이어 값 고정 — 화면 표기만 한국어)
type RUColor string

const (
	RURed    RUColor = "red"
	RUBlue   RUColor = "blue"
	RUBlack  RUColor = "black"
	RUOrange RUColor = "orange"
)

// ruColors 색 4종 (덱 생성·그룹 판정의 기준 순서)
var ruColors = []RUColor{RURed, RUBlue, RUBlack, RUOrange}

// ruColorNames 색의 한글 표기 (이벤트·로그 문구용). 와이어에는 싣지 않는다.
var ruColorNames = map[RUColor]string{
	RURed:    "빨강",
	RUBlue:   "파랑",
	RUBlack:  "검정",
	RUOrange: "주황",
}

// ruColorName 색 한글 표기
func ruColorName(c RUColor) string {
	if n, ok := ruColorNames[c]; ok {
		return n
	}
	return string(c)
}

// RUTile 타일 한 개.
//
// 조커는 {joker:true, num:0} 이다. 테이블에 놓인 조커에는 서버가 그 자리에서
// 대신하는 숫자를 StandsFor 로 채워 준다(프론트가 조커의 역할을 보여줄 수
// 있게). 받침대의 조커에는 없다 — 아직 아무것도 대신하지 않기 때문이다.
//
// ID·Num 은 0 유실을 막기 위해 omitempty 를 쓰지 않는다(조커의 num 은 0이다).
type RUTile struct {
	ID    int     `json:"id"`
	Color RUColor `json:"color"`
	Num   int     `json:"num"`
	Joker bool    `json:"joker"`
	// StandsFor 테이블에 놓인 조커가 대신하는 숫자 (그 외에는 키 부재)
	StandsFor *int `json:"standsFor,omitempty"`
}

// RUSetKind 세트의 종류
type RUSetKind string

const (
	// RUSetNone 유효한 세트가 아님
	RUSetNone RUSetKind = ""
	// RUSetGroup 그룹 — 색이 다른 같은 숫자 3~4개
	RUSetGroup RUSetKind = "group"
	// RUSetRun 연속 — 색이 같고 숫자가 이어지는 3개 이상
	RUSetRun RUSetKind = "run"
)

// ruSetKindName 세트 종류 한글 표기
func ruSetKindName(k RUSetKind) string {
	switch k {
	case RUSetGroup:
		return "그룹"
	case RUSetRun:
		return "연속"
	}
	return "세트"
}

// RUPhase 게임 진행 단계. 루미큐브의 한 차례는 쪼개지지 않는다 —
// 커밋 하나로 통째로 끝난다.
type RUPhase string

const (
	RUPhaseWaiting  RUPhase = "waiting"
	RUPhaseTurn     RUPhase = "turn" // 차례 진행 (90초 마감 — 자동으로 1개 가져가고 종료)
	RUPhaseGameOver RUPhase = "game_over"
)

// RUMessageType 루미큐브 메시지 타입
type RUMessageType string

const (
	// 클라이언트 → 서버
	RUMsgJoinGame RUMessageType = "ru_join_game"
	RUMsgFillBots RUMessageType = "ru_fill_bots"
	RUMsgStart    RUMessageType = "ru_start"
	RUMsgRejoin   RUMessageType = "ru_rejoin"
	RUMsgCommit   RUMessageType = "ru_commit"
	RUMsgDraw     RUMessageType = "ru_draw"
	RUMsgReact    RUMessageType = "ru_react"

	// 서버 → 클라이언트
	RUMsgPlayerJoined       RUMessageType = "ru_player_joined"
	RUMsgSpectateJoined     RUMessageType = "ru_spectate_joined"
	RUMsgGameState          RUMessageType = "ru_game_state"
	RUMsgEvent              RUMessageType = "ru_event"
	RUMsgGameOver           RUMessageType = "ru_game_over"
	RUMsgPlayerDisconnected RUMessageType = "ru_player_disconnected"
	RUMsgPlayerReconnected  RUMessageType = "ru_player_reconnected"
	RUMsgSessionExpired     RUMessageType = "ru_session_expired"
	RUMsgError              RUMessageType = "ru_error"
)

// RUPlayer 게임 참가자 (순수 상태 — 연결 매핑은 허브의 ruRoom 담당)
type RUPlayer struct {
	Seat int
	Name string
	// Rack 받침대 (비공개 — 본인만 내용을 본다)
	Rack []RUTile
	// Melded 등록(첫 내려놓기 30점)을 마쳤는지
	Melded bool
	// Score 최종 정산 점수 (정산 전에는 0)
	Score int
}

// RUGameEvent 순수 규칙이 쌓는 연출 이벤트 — 허브가 DrainEvents 로 꺼내
// ru_event 로 방송한다. 남의 받침대 내용·타일더미 내용은 절대 담지 않는다.
type RUGameEvent struct {
	Kind    string
	Seat    int // -1 없음
	Message string
}

// RULastAction 마지막 행동 요약 (전원 공개)
type RULastAction struct {
	Seat    int    `json:"seat"`
	Name    string `json:"name"`
	Message string `json:"message"`
}

// RUResultRow 최종 정산 한 줄. Detail 은 남은 타일·벌점을 담은 한글 설명.
type RUResultRow struct {
	Seat   int    `json:"seat"`
	Score  int    `json:"score"`
	Detail string `json:"detail"`
}

// RUResult 종료 결과. 남은 타일 점수가 같으면 공동 승이라 좌석이 여럿이다.
type RUResult struct {
	WinnerSeats []int         `json:"winnerSeats"`
	WinnerNames []string      `json:"winnerNames"`
	Rows        []RUResultRow `json:"rows"`
	Message     string        `json:"message"`
}

// RUGame 루미큐브 게임 상태 (순수, 허브 비의존).
//
// 서버 상태는 **언제나 차례 시작 상태**다. 차례 중간 배치는 클라이언트만
// 갖고 있고, ru_commit 이 통과할 때만 여기로 넘어온다.
type RUGame struct {
	ID      string
	Players []*RUPlayer
	Phase   RUPhase

	// Pool 타일더미 (앞이 맨 위). 아무도 내용을 못 본다.
	Pool []RUTile
	// Sets 테이블 위의 세트들 (전원 공개). 항상 전부 유효하다.
	Sets [][]RUTile

	// CurrentSeat 현재 차례 좌석 (-1 시작 전)
	CurrentSeat int
	// Turns 지금까지 끝난 차례 수
	Turns int
	// PassStreak 타일더미가 빈 뒤 연속으로 넘긴 차례 수 — 인원수에 도달하면
	// 아무도 못 내는 것이므로 남은 타일 점수로 승부를 가린다
	PassStreak int

	LastAction *RULastAction // 마지막 행동 (그 전엔 nil)
	Result     *RUResult     // 종료 결과 (그 전엔 nil)
	Ready      bool
	StartedAt  time.Time

	// StateSeq 새 대기 상태(차례)가 열릴 때마다 +1 — 허브가 마감 타이머를
	// 다시 걸지 판단하는 근거
	StateSeq int
	// AfkSeq 마감 타이머 일련번호 (뒤늦은 발화 무시용 — 허브가 관리)
	AfkSeq int
	// Deadline 현재 차례의 마감 시각 (unixMillis — 스냅샷 노출용)
	Deadline int64

	events []RUGameEvent
}

// RUClient 루미큐브 클라이언트 연결
type RUClient struct {
	wsClient
	Hub  *RUHub
	Seat int
}

// RUMessage 메시지 봉투
type RUMessage struct {
	Type    RUMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type RUJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type RURejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// RUCommitPayload 차례 종료 시의 **테이블 전체 배치**. 부분 이동 메시지는
// 없다 — 재배치 게임에서 증분 프로토콜은 지옥이다.
//
// Sets 는 세트별 타일 ID 목록이다. 연속(run)은 오름차순으로 담아 주면
// 조커가 그 자리 숫자를 그대로 받는다 (순서가 뒤섞여 있어도 판정은
// 통과하지만, 조커의 standsFor 는 서버가 점수가 높은 쪽으로 정한다).
type RUCommitPayload struct {
	Sets [][]int `json:"sets"`
}

type RUReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

// RUPlayerView 좌석별 공개 정보 — 좌석 0·타일 0개 유실 방지를 위해
// omitempty 금지. 받침대의 **내용**은 여기에 없다 (개수만).
type RUPlayerView struct {
	Seat      int    `json:"seat"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Bot       bool   `json:"bot"`
	RackCount int    `json:"rackCount"`
	Melded    bool   `json:"melded"`
	Score     int    `json:"score"`
}

// RUGameStatePayload 개인화된 전체 게임 스냅샷. 모든 상태 변경 후 좌석마다
// 따로 만들어 보낸다. 재접속 복원도 같은 페이로드를 쓴다.
//
// 은닉: YourRack·YourMelded 는 본인에게만 실린다 — 타인·관전자(viewerSeat
// -1)의 raw JSON 에는 키 자체가 없다. 빈 받침대도 [] 로 보내야 하므로
// 슬라이스 포인터로 부재를 표현한다. sets·poolLeft·players 는 전원 공개다.
// 타일더미의 내용은 어떤 스냅샷에도 없다.
type RUGameStatePayload struct {
	GameID   string  `json:"gameId"`
	RoomCode string  `json:"roomCode"`
	Phase    RUPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"` // 관전자는 -1
	// Spectators 관전자 수
	Spectators int `json:"spectators"`
	// EndsAt 현재 차례의 마감 시각 (unixMillis, 그 외 0)
	EndsAt      int64 `json:"endsAt"`
	CurrentSeat int   `json:"currentSeat"`
	// PoolLeft 타일더미에 남은 타일 수 (내용은 공개하지 않는다)
	PoolLeft int `json:"poolLeft"`
	// Sets 테이블 (전원 공개). 조커에는 standsFor 가 채워져 있다.
	Sets [][]RUTile `json:"sets"`
	// YourRack 본인의 받침대 — 본인에게만 (관전자·시작 전 부재)
	YourRack *[]RUTile `json:"yourRack,omitempty"`
	// YourMelded 본인의 등록 여부 — 본인에게만
	YourMelded *bool          `json:"yourMelded,omitempty"`
	Players    []RUPlayerView `json:"players"`
	LastAction *RULastAction  `json:"lastAction"` // 그 전엔 null
	Result     *RUResult      `json:"result"`     // 종료 결과 (그 전엔 null)
}

// RUEventPayload 연출용 이벤트. 받침대 내용·타일더미 내용은 담지 않으며
// 전원에게 동일하게 간다. Seat 은 좌석 0 유실 방지를 위해 포인터다.
type RUEventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

// RUGameOverPayload 게임 종료 발표 — 정산 내역이 함께 간다
type RUGameOverPayload struct {
	WinnerSeats []int          `json:"winnerSeats"`
	WinnerNames []string       `json:"winnerNames"`
	Rows        []RUResultRow  `json:"rows"`
	Message     string         `json:"message"`
	Turns       int            `json:"turns"`
	Players     []RUPlayerView `json:"players"`
}

type RUPlayerDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type RUPlayerReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type RUErrorPayload struct {
	Message string `json:"message"`
}
