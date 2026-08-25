package server

import (
	"strconv"
	"time"
)

// ==================== 더 크루 (The Crew) 타입 ====================
//
// 3~5인 협력 트릭테이킹. 전원이 한 팀이라 승패를 함께 맞는다 — 클리어면
// 전원 승자, 실패면 전원 패자다 (cw_hub.go 의 finishGame 이 그대로 기록한다).
//
// 덱은 40장 — 4색 숫자 1~9(36장) + 로켓 1~4(4장). 로켓은 상시 트럼프다.
// 로켓 4를 쥔 사람이 사령관이며 첫 리드를 잡는다.
//
// 임무(task)는 색 숫자 카드다. 배정받은 좌석이 그 카드가 들어 있는 트릭을
// 이겨야 하고, 남이 가져가면 즉시 실패한다. 임무는 전원 공개다.
//
// 은닉은 cw_hub.go 의 buildCWState 하나에 모여 있다. yourHand 만 본인
// 스냅샷에 실리고, 타인·관전자(viewerSeat -1)의 raw JSON 에는 키 자체가 없다.
// tasks 와 players[].revealed 는 전원 공개다.

const (
	CWMinPlayers = 3
	CWMaxPlayers = 5

	// CWFillBotTarget cw_fill_bots 가 채우는 목표 인원 — 채운 뒤 즉시 시작
	CWFillBotTarget = 4

	// CWDefaultMaxMission 기본 임무 단계 수 (1임무 → … → 5임무를 마치면 클리어)
	CWDefaultMaxMission = 5

	// CWSelfTaskMinRank 카드를 쥔 사람에게 그 카드의 임무를 맡길 수 있는
	// 최소 숫자. 이보다 낮으면 그 카드로 직접 트릭을 이겨야 해서 사실상
	// 불가능하므로 카드를 쥐지 않은 좌석에 배정한다 (cwPickTasks 주석 참고).
	CWSelfTaskMinRank = 8

	// 덱 구성 — 색 1~9 네 벌 + 로켓 1~4
	CWColorMaxRank  = 9
	CWRocketMaxRank = 4
	CWDeckSize      = 4*CWColorMaxRank + CWRocketMaxRank // 40

	// CWTokenPerMission 임무마다 1인이 쓸 수 있는 통신 토큰 수
	CWTokenPerMission = 1
)

// CWSuit 카드 무늬 (와이어 값). rocket 은 상시 트럼프다.
type CWSuit string

const (
	CWSuitBlue   CWSuit = "blue"
	CWSuitGreen  CWSuit = "green"
	CWSuitPink   CWSuit = "pink"
	CWSuitYellow CWSuit = "yellow"
	CWSuitRocket CWSuit = "rocket"
)

// cwColorSuits 색 무늬 4종 (임무·통신은 색 카드만 다룬다)
var cwColorSuits = []CWSuit{CWSuitBlue, CWSuitGreen, CWSuitPink, CWSuitYellow}

// cwSuitOrder 손패 정렬 순서 (로켓을 맨 뒤로)
var cwSuitOrder = map[CWSuit]int{
	CWSuitBlue: 0, CWSuitGreen: 1, CWSuitPink: 2, CWSuitYellow: 3, CWSuitRocket: 4,
}

// cwSuitLabel 무늬 한글 표기 (이벤트·에러 문구용)
func cwSuitLabel(suit CWSuit) string {
	switch suit {
	case CWSuitBlue:
		return "파랑"
	case CWSuitGreen:
		return "초록"
	case CWSuitPink:
		return "분홍"
	case CWSuitYellow:
		return "노랑"
	case CWSuitRocket:
		return "로켓"
	default:
		return "?"
	}
}

// CWCard 카드 한 장 — 색은 1~9, 로켓은 1~4
type CWCard struct {
	Suit CWSuit `json:"suit"`
	Rank int    `json:"rank"`
}

// cwCardLabel 카드 한글 표기 ("파랑 5")
func cwCardLabel(card CWCard) string {
	return cwSuitLabel(card.Suit) + " " + strconv.Itoa(card.Rank)
}

// CWHint 통신 선언 — 공개한 카드가 그 색 안에서 차지하는 위치.
// 서버가 진실 여부를 검증한다 (거짓 선언은 에러).
type CWHint string

const (
	CWHintHighest CWHint = "highest" // 그 색 카드를 2장 이상 들고 있고 그중 최고
	CWHintLowest  CWHint = "lowest"  // 그 색 카드를 2장 이상 들고 있고 그중 최저
	CWHintOnly    CWHint = "only"    // 그 색 카드가 정확히 1장
)

// cwHintLabel 선언 한글 표기
func cwHintLabel(hint CWHint) string {
	switch hint {
	case CWHintHighest:
		return "최고"
	case CWHintLowest:
		return "최저"
	case CWHintOnly:
		return "유일"
	default:
		return "?"
	}
}

// CWPhase 게임 진행 단계
type CWPhase string

const (
	CWPhaseWaiting  CWPhase = "waiting"
	CWPhasePlaying  CWPhase = "playing"   // 현재 좌석의 카드 대기 (45초 마감)
	CWPhaseRoundEnd CWPhase = "round_end" // 임무 성공 정산 (5초 뒤 다음 임무)
	CWPhaseGameOver CWPhase = "game_over"
)

// CWMessageType 더 크루 메시지 타입
type CWMessageType string

const (
	// 클라이언트 → 서버
	CWMsgJoinGame    CWMessageType = "cw_join_game"
	CWMsgFillBots    CWMessageType = "cw_fill_bots"
	CWMsgStart       CWMessageType = "cw_start"
	CWMsgRejoin      CWMessageType = "cw_rejoin"
	CWMsgPlay        CWMessageType = "cw_play"
	CWMsgCommunicate CWMessageType = "cw_communicate"
	CWMsgReact       CWMessageType = "cw_react"

	// 서버 → 클라이언트
	CWMsgPlayerJoined       CWMessageType = "cw_player_joined"
	CWMsgSpectateJoined     CWMessageType = "cw_spectate_joined"
	CWMsgGameState          CWMessageType = "cw_game_state"
	CWMsgEvent              CWMessageType = "cw_event"
	CWMsgGameOver           CWMessageType = "cw_game_over"
	CWMsgPlayerDisconnected CWMessageType = "cw_player_disconnected"
	CWMsgPlayerReconnected  CWMessageType = "cw_player_reconnected"
	CWMsgSessionExpired     CWMessageType = "cw_session_expired"
	CWMsgError              CWMessageType = "cw_error"
)

// CWTask 과제 카드 — 전원 공개. Seat 담당자가 이 카드가 들어 있는 트릭을
// 이겨야 한다. 좌석 0 유실을 막기 위해 omitempty 를 쓰지 않는다.
type CWTask struct {
	Suit CWSuit `json:"suit"`
	Rank int    `json:"rank"`
	Seat int    `json:"seat"`
	Done bool   `json:"done"`
}

// CWTrickCard 트릭에 놓인 카드 한 장 (전원 공개)
type CWTrickCard struct {
	Seat int    `json:"seat"`
	Card CWCard `json:"card"`
}

// CWReveal 통신으로 공개한 카드와 선언 — 전원이 계속 본다.
// 공개해도 카드는 손에 남아 있다.
type CWReveal struct {
	Card CWCard `json:"card"`
	Hint CWHint `json:"hint"`
}

// CWLastTrick 직전에 끝난 트릭 (전원 공개)
type CWLastTrick struct {
	WinnerSeat int           `json:"winnerSeat"`
	Cards      []CWTrickCard `json:"cards"`
}

// CWResult 종료 결과 — 협력 게임이라 승패를 전원이 함께 맞는다
type CWResult struct {
	Cleared bool `json:"cleared"`
	// FailedReason "" (클리어) | "wrong_winner" | "out_of_cards"
	FailedReason string `json:"failedReason"`
	// Mission 종료 시점의 임무 단계 (클리어면 maxMission)
	Mission int    `json:"mission"`
	Message string `json:"message"`
}

// CWPlayer 게임 참가자 (순수 상태 — 연결 매핑은 허브의 cwRoom 담당)
type CWPlayer struct {
	Seat int
	Name string
	// Hand 남은 손패 (본인만 내용을 본다). 통신으로 공개한 카드도 여기 남는다
	Hand []CWCard
	// TokenLeft 남은 통신 토큰 (임무마다 1로 초기화)
	TokenLeft int
	// Revealed 통신으로 공개한 카드와 선언 (전원 공개). nil 이면 미공개
	Revealed *CWReveal
}

// CWGameEvent 순수 규칙이 쌓는 연출 이벤트 — 허브가 DrainEvents 로 꺼내
// cw_event 로 방송한다 (미공개 손패는 절대 담지 않는다. 트릭에 놓인 카드와
// 임무·공개 카드는 이미 공개 정보라 담아도 된다)
type CWGameEvent struct {
	Kind    string
	Seat    int // -1 없음
	Message string
}

// CWGame 더 크루 게임 상태 (순수, 허브 비의존)
type CWGame struct {
	ID      string
	Players []*CWPlayer
	Phase   CWPhase

	// Mission 현재 임무 단계 = 과제 카드 수 (1~MaxMission, 시작 전 0)
	Mission    int
	MaxMission int

	// CommanderSeat 로켓 4를 쥔 좌석 (-1 시작 전) — 임무마다 다시 정해진다
	CommanderSeat int
	// CurrentSeat 카드를 낼 차례 (-1 시작 전)
	CurrentSeat int
	// Leader 이번 트릭의 리드 좌석
	Leader int
	// LeadSuit 리드 무늬 ("" 트릭 시작 전)
	LeadSuit CWSuit
	// Trick 이번 트릭 진행분
	Trick []CWTrickCard
	// trickOrder 이번 트릭에 카드를 낼 좌석 순서 (손패가 남은 좌석만)
	trickOrder []int

	// Tasks 이번 임무의 과제 카드 (전원 공개)
	Tasks []CWTask

	LastTrick *CWLastTrick // 직전 트릭 (그 전엔 nil)
	Result    *CWResult    // 종료 결과 (그 전엔 nil)
	EndReason string       // "cleared" | "wrong_winner" | "out_of_cards"
	Ready     bool
	StartedAt time.Time

	// StateSeq 새 대기 상태(다음 차례·임무 정산)가 열릴 때마다 +1 —
	// 허브가 마감 타이머를 다시 걸지 판단하는 근거. 통신은 대기 상태를
	// 바꾸지 않으므로 StateSeq 를 올리지 않는다 (마감이 늘어나지 않는다).
	StateSeq int
	// AfkSeq 마감 타이머 일련번호 (뒤늦은 발화 무시용 — 허브가 관리)
	AfkSeq int
	// Deadline 현재 대기 상태의 마감 시각 (unixMillis — 스냅샷 노출용)
	Deadline int64

	events []CWGameEvent
}

// CWClient 더 크루 클라이언트 연결
type CWClient struct {
	wsClient
	Hub  *CWHub
	Seat int
}

// CWMessage 메시지 봉투
type CWMessage struct {
	Type    CWMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type CWJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type CWRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// CWPlayPayload 카드 제출 — 손패 인덱스. 인덱스 0 유실을 막기 위해
// omitempty 를 쓰지 않는다.
type CWPlayPayload struct {
	Index int `json:"index"`
}

// CWCommunicatePayload 통신 — 손패 인덱스와 선언.
// 선언은 서버가 진실 여부를 검증한다 (거짓이면 에러).
type CWCommunicatePayload struct {
	Index int    `json:"index"`
	Hint  CWHint `json:"hint"`
}

type CWReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

// CWPlayerView 좌석별 공개 정보 — 좌석 0·장수 0 유실 방지를 위해 omitempty 금지.
// Revealed 는 통신으로 공개한 카드라 전원 공개다 (미공개는 null).
type CWPlayerView struct {
	Seat      int       `json:"seat"`
	Name      string    `json:"name"`
	Connected bool      `json:"connected"`
	Bot       bool      `json:"bot"`
	HandCount int       `json:"handCount"`
	TokenLeft int       `json:"tokenLeft"`
	Revealed  *CWReveal `json:"revealed"`
}

// CWGameStatePayload 개인화된 전체 게임 스냅샷. 모든 상태 변경 후 좌석마다
// 따로 만들어 보낸다. 재접속 복원도 같은 페이로드를 쓴다.
// 은닉: yourHand 만 본인에게 실린다 — 타인·관전자(viewerSeat -1)의 raw JSON
// 에는 키 자체가 없다. tasks·trick·players[].revealed 는 전원 공개다.
type CWGameStatePayload struct {
	GameID   string  `json:"gameId"`
	RoomCode string  `json:"roomCode"`
	Phase    CWPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"` // 관전자는 -1
	// Spectators 관전자 수 (참가자·관전자 모두 표시용)
	Spectators int `json:"spectators"`
	// EndsAt 현재 대기 상태(차례·임무 정산)의 마감 시각 (unixMillis, 그 외 0)
	EndsAt        int64  `json:"endsAt"`
	Mission       int    `json:"mission"`
	MaxMission    int    `json:"maxMission"`
	CommanderSeat int    `json:"commanderSeat"`
	CurrentSeat   int    `json:"currentSeat"`
	LeadSuit      CWSuit `json:"leadSuit"`
	// Trick 이번 트릭 진행분 — 항상 [] (nil → JSON null 금지)
	Trick []CWTrickCard `json:"trick"`
	// Tasks 과제 카드 — 전원 공개, 항상 []
	Tasks []CWTask `json:"tasks"`
	// YourHand 본인 손패 — 본인에게만 (관전자 부재).
	// 빈 손패도 [] 로 나가야 하므로 슬라이스 포인터로 부재를 표현한다.
	YourHand  *[]CWCard      `json:"yourHand,omitempty"`
	Players   []CWPlayerView `json:"players"`
	LastTrick *CWLastTrick   `json:"lastTrick"` // 그 전엔 null
	Result    *CWResult      `json:"result"`    // 종료 결과 (그 전엔 null)
}

// CWEventPayload 연출용 이벤트. 미공개 손패를 담지 않으며 전원에게 동일하게
// 간다. Seat 은 좌석 0 유실 방지를 위해 포인터로 생략을 표현한다.
type CWEventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

// CWGameOverPayload 게임 종료 발표 — 협력 결과라 전원이 같은 결말을 받는다
type CWGameOverPayload struct {
	Cleared      bool           `json:"cleared"`
	FailedReason string         `json:"failedReason"`
	Mission      int            `json:"mission"`
	MaxMission   int            `json:"maxMission"`
	Message      string         `json:"message"`
	Tasks        []CWTask       `json:"tasks"`
	Players      []CWPlayerView `json:"players"`
}

type CWPlayerDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type CWPlayerReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type CWErrorPayload struct {
	Message string `json:"message"`
}
