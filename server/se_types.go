package server

import "time"

// ==================== 세트 (Set) 타입 ====================
//
// 1~8인 실시간 패턴 인식 선착순. 플랫폼 최초로 **차례가 없는** 게임이다 —
// currentSeat 도, 좌석별 AFK 자동 진행도 없다. 누구든 언제든 se_claim 을
// 보내고, 허브 고루틴이 도착 순서대로 직렬 판정한다(선착 판정 모델).
// 그래서 이 파일에는 StateSeq·AfkSeq 같은 턴 상태기계 필드가 없고, 대신
// 무한 게임을 막는 강제 종료 시각(Deadline) 하나만 있다.
//
// 은닉도 없다. 바닥·점수·잠금이 전부 공개라 관전자도 참가자와 똑같은
// 스냅샷을 받는다 (yourSeat 만 -1). 이 게임은 정보가 아니라 속도를 겨룬다.
//
// 카드 81장 = 4속성 × 3값의 모든 조합이다. 세트는 카드 3장으로, 네 속성
// 각각이 전부 같거나 전부 다르면 성립한다.

const (
	SEMinPlayers = 1 // 혼자서도 연습할 수 있다 (차례가 없어 1인도 성립)
	SEMaxPlayers = 8

	// SEFillBotTarget se_fill_bots 가 채우는 목표 인원 — 채운 뒤 즉시 시작
	SEFillBotTarget = 4

	// SEDeckSize 덱 크기 — 4속성 × 3값 = 81장
	SEDeckSize = 81

	// SEBoardSize 기본 바닥 장수, SEBoardMax 세트가 없을 때 늘리는 상한
	SEBoardSize = 12
	SEBoardMax  = 21

	// SEDealStep 세트가 없을 때 한 번에 더 펴는 장수
	SEDealStep = 3

	// SEClaimSize claim 한 번에 고르는 카드 수
	SEClaimSize = 3

	// seMaxReshuffle 21장에도 세트가 없어 바닥을 물리고 재배치하는 최대 횟수.
	// 서로 다른 21장에는 반드시 세트가 있다는 것이 알려져 있어(최대 캡셋 20장)
	// 실전에서는 발동하지 않지만, 규칙을 그대로 구현하되 무한 루프는 막는다.
	seMaxReshuffle = 8
)

// seClaimLock 오답·사라진 카드 claim 을 낸 좌석의 잠금 시간
// (테스트 init 에서 짧게 낮춘다)
var seClaimLock = 5 * time.Second

// seForceEnd 게임 시작 후 강제 종료까지의 시간 — 무한 게임 방지 안전장치.
// 허브가 NewSEHub 에서 필드로 복사해 가므로 테스트는 허브 필드를 바꾼다.
var seForceEnd = 10 * time.Minute

// SEShape 모양 (와이어 값)
type SEShape string

const (
	SEShapeDiamond SEShape = "diamond" // 다이아몬드
	SEShapeWave    SEShape = "wave"    // 물결
	SEShapeOval    SEShape = "oval"    // 타원
)

// SEFill 채움 (와이어 값)
type SEFill string

const (
	SEFillSolid  SEFill = "solid"  // 채움
	SEFillStripe SEFill = "stripe" // 빗금
	SEFillEmpty  SEFill = "empty"  // 비움
)

// SEColor 색 (와이어 값)
type SEColor string

const (
	SEColorRed    SEColor = "red"    // 빨강
	SEColorGreen  SEColor = "green"  // 초록
	SEColorPurple SEColor = "purple" // 보라
)

// 속성 값의 나열 순서 — 카드 id 유도와 인덱스 변환의 단일 기준이다
var (
	seShapes = []SEShape{SEShapeDiamond, SEShapeWave, SEShapeOval}
	seCounts = []int{1, 2, 3}
	seFills  = []SEFill{SEFillSolid, SEFillStripe, SEFillEmpty}
	seColors = []SEColor{SEColorRed, SEColorGreen, SEColorPurple}
)

// 속성 → 인덱스(0~2). 세트 판정은 이 인덱스로만 한다.
var (
	seShapeIndex = map[SEShape]int{SEShapeDiamond: 0, SEShapeWave: 1, SEShapeOval: 2}
	seFillIndex  = map[SEFill]int{SEFillSolid: 0, SEFillStripe: 1, SEFillEmpty: 2}
	seColorIndex = map[SEColor]int{SEColorRed: 0, SEColorGreen: 1, SEColorPurple: 2}
)

// seShapeLabel 모양 한글 표기 (로그·문구용)
func seShapeLabel(s SEShape) string {
	switch s {
	case SEShapeDiamond:
		return "다이아몬드"
	case SEShapeWave:
		return "물결"
	case SEShapeOval:
		return "타원"
	default:
		return "?"
	}
}

// seFillLabel 채움 한글 표기
func seFillLabel(f SEFill) string {
	switch f {
	case SEFillSolid:
		return "채움"
	case SEFillStripe:
		return "빗금"
	case SEFillEmpty:
		return "비움"
	default:
		return "?"
	}
}

// seColorLabel 색 한글 표기
func seColorLabel(c SEColor) string {
	switch c {
	case SEColorRed:
		return "빨강"
	case SEColorGreen:
		return "초록"
	case SEColorPurple:
		return "보라"
	default:
		return "?"
	}
}

// SECard 카드 한 장. id 는 0~80 고정이며 속성 조합에서 유도된다 —
// 프론트가 id 만으로 그릴 수 있어도 계약대로 속성 4종을 항상 함께 보낸다.
type SECard struct {
	ID    int     `json:"id"`
	Shape SEShape `json:"shape"`
	Count int     `json:"count"`
	Fill  SEFill  `json:"fill"`
	Color SEColor `json:"color"`
}

// SEPhase 게임 진행 단계 — 차례가 없어 진행 중은 playing 하나뿐이다
type SEPhase string

const (
	SEPhaseWaiting  SEPhase = "waiting"
	SEPhasePlaying  SEPhase = "playing"
	SEPhaseGameOver SEPhase = "game_over"
)

// SEMessageType 세트 메시지 타입
type SEMessageType string

const (
	// 클라이언트 → 서버
	SEMsgJoinGame SEMessageType = "se_join_game"
	SEMsgFillBots SEMessageType = "se_fill_bots"
	SEMsgStart    SEMessageType = "se_start"
	SEMsgRejoin   SEMessageType = "se_rejoin"
	SEMsgClaim    SEMessageType = "se_claim"
	SEMsgReact    SEMessageType = "se_react"

	// 서버 → 클라이언트
	SEMsgPlayerJoined       SEMessageType = "se_player_joined"
	SEMsgSpectateJoined     SEMessageType = "se_spectate_joined"
	SEMsgGameState          SEMessageType = "se_game_state"
	SEMsgEvent              SEMessageType = "se_event"
	SEMsgGameOver           SEMessageType = "se_game_over"
	SEMsgPlayerDisconnected SEMessageType = "se_player_disconnected"
	SEMsgPlayerReconnected  SEMessageType = "se_player_reconnected"
	SEMsgSessionExpired     SEMessageType = "se_session_expired"
	SEMsgError              SEMessageType = "se_error"
)

// 판정 문구 — lastClaim.message 와 이벤트에 그대로 실린다
const (
	seClaimOKMsg    = "세트 성립!"
	seClaimWrongMsg = "세트가 아닙니다"
	seClaimGoneMsg  = "이미 사라진 카드가 있습니다"
)

// SEPlayer 게임 참가자 (순수 상태 — 연결 매핑은 허브의 seRoom 담당)
type SEPlayer struct {
	Seat int
	Name string
	// Score 획득 세트 수에서 감점을 뺀 점수 (0 미만으로 내려가지 않는다)
	Score int
	// LockedUntil 오답 잠금 해제 시각 (unixMillis, 0 이면 잠금 없음)
	LockedUntil int64
}

// SEGameEvent 순수 규칙이 쌓는 연출 이벤트 — 허브가 DrainEvents 로 꺼내
// se_event 로 방송한다. 은닉이 없는 게임이라 담기지 못할 정보가 없다.
type SEGameEvent struct {
	Kind    string
	Seat    int // -1 없음
	Message string
}

// SELastClaim 직전 판정 결과 (전원 공개). 프론트의 성공/실패 배너 근거다.
type SELastClaim struct {
	Seat    int    `json:"seat"`
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	IDs     []int  `json:"ids"`
	Message string `json:"message"`
}

// SEResult 종료 결과 — 최고점 승, 동점이면 공동 승리
type SEResult struct {
	WinnerSeats []int    `json:"winnerSeats"`
	WinnerNames []string `json:"winnerNames"`
	Message     string   `json:"message"`
}

// SEGame 세트 게임 상태 (순수, 허브 비의존).
// 차례가 없으므로 CurrentSeat·StateSeq·AfkSeq 가 없다.
type SEGame struct {
	ID      string
	Players []*SEPlayer
	Phase   SEPhase

	// Deck 남은 덱 (앞에서 뽑는다)
	Deck []SECard
	// Board 바닥에 펼쳐진 카드 (전원 공개)
	Board []SECard
	// SetsFound 이번 판에서 성립한 세트 총수
	SetsFound int

	LastClaim *SELastClaim // 직전 판정 (그 전엔 nil)
	Result    *SEResult    // 종료 결과 (그 전엔 nil)
	EndReason string       // "deck_empty" | "time_up"
	Ready     bool
	StartedAt time.Time

	// EndSeq 강제 종료 타이머 일련번호 (뒤늦은 발화 무시용 — 허브가 관리)
	EndSeq int
	// Deadline 강제 종료 시각 (unixMillis — 스냅샷 노출용)
	Deadline int64

	events []SEGameEvent
}

// SEClient 세트 클라이언트 연결
type SEClient struct {
	wsClient
	Hub  *SEHub
	Seat int
}

// SEMessage 메시지 봉투
type SEMessage struct {
	Type    SEMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type SEJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 관대하게 새로 생성)
	Room string `json:"room,omitempty"`
}

type SERejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// SEClaimPayload 세트 선언 — 바닥 카드 id 3장
type SEClaimPayload struct {
	IDs []int `json:"ids"`
}

type SEReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

// SEPlayerView 좌석별 공개 정보 — 좌석 0·점수 0 유실 방지를 위해 omitempty 금지.
// 은닉이 없으므로 이 구조체가 곧 전원이 보는 좌석 정보 전부다.
type SEPlayerView struct {
	Seat      int    `json:"seat"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Bot       bool   `json:"bot"`
	Score     int    `json:"score"`
	// LockedUntil 오답 잠금 해제 시각 (unixMillis, 0 이면 잠금 없음)
	LockedUntil int64 `json:"lockedUntil"`
}

// SEGameStatePayload 전체 게임 스냅샷. 모든 상태 변경 후 방송한다.
// 재접속 복원도 같은 페이로드를 쓴다.
//
// 은닉이 없다 — 관전자(viewerSeat -1)도 참가자와 완전히 같은 스냅샷을 받고,
// 다른 값은 yourSeat 하나뿐이다.
type SEGameStatePayload struct {
	GameID   string  `json:"gameId"`
	RoomCode string  `json:"roomCode"`
	Phase    SEPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"` // 관전자는 -1
	// Spectators 관전자 수 (참가자·관전자 모두 표시용)
	Spectators int `json:"spectators"`
	// EndsAt 강제 종료 시각 (unixMillis, 진행 중이 아니면 0)
	EndsAt int64 `json:"endsAt"`
	// Board 바닥 카드 — 전원 공개, 항상 [] (nil → JSON null 금지)
	Board     []SECard `json:"board"`
	DeckLeft  int      `json:"deckLeft"`
	SetsFound int      `json:"setsFound"`
	// Players 좌석 정보 — 항상 []
	Players   []SEPlayerView `json:"players"`
	LastClaim *SELastClaim   `json:"lastClaim"` // 그 전엔 null
	Result    *SEResult      `json:"result"`    // 종료 결과 (그 전엔 null)
}

// SEEventPayload 연출용 이벤트. 전원에게 동일하게 간다.
// Seat 은 좌석 0 유실 방지를 위해 포인터로 생략을 표현한다.
type SEEventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

// SEGameOverPayload 게임 종료 발표
type SEGameOverPayload struct {
	WinnerSeats []int          `json:"winnerSeats"`
	WinnerNames []string       `json:"winnerNames"`
	Reason      string         `json:"reason"` // "deck_empty" | "time_up"
	Message     string         `json:"message"`
	SetsFound   int            `json:"setsFound"`
	Players     []SEPlayerView `json:"players"`
}

type SEPlayerDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type SEPlayerReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type SEErrorPayload struct {
	Message string `json:"message"`
}
