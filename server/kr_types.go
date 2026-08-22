package server

import (
	"encoding/json"
	"time"
)

// ==================== 노 터치 크라켄 (No Touch Kraken) 타입 ====================
//
// 4~8인 정체 은닉 카드 뒤집기. 탐험대 vs 해골이지만 서로의 정체를 아무도
// 모른다 (해골끼리도 모른다). 역할 풀에서 인원수만큼만 뽑아 나눠주므로 실제
// 진영 구성조차 비밀이다 — 풀 자체는 전원 공개 정보(rolePool).
//
// 은닉의 심장은 kr_hub.go 의 buildKRState 다. yourRole·yourHand 는 본인
// 스냅샷에만 실리고, 타인·관전자의 raw JSON 에는 키 자체가 없다. players[].role
// 은 game_over 전까지 전원 "" 다.
//
// 진행: 라운드 r(1~4) 시작에 남은 카드를 섞어 각자 (6-r)장 배분 → 지목권자가
// 자기 외 좌석의 미공개 카드 1장을 지목해 즉시 공개 → 지목권은 그 카드의
// 주인에게 → 라운드마다 정확히 N번 공개 → 4라운드까지.
// 승패는 공개 직후 즉시 판정한다 (크라켄=해골 승 / 보물 N장=탐험대 승 /
// 4라운드 소진=해골 승). 무승부는 없다.

const (
	KRMinPlayers = 4
	KRMaxPlayers = 8

	// KRFillBotTarget kr_fill_bots 가 채우는 목표 인원 — 채운 뒤 즉시 시작
	KRFillBotTarget = 5

	// KRRounds 총 라운드 수 (4라운드를 소진하면 해골 승 — 유한 종료 보장)
	KRRounds = 4

	// KRCardsPerPlayer 인원 N일 때 덱은 5N장 (보물 N·크라켄 1·꽝 4N-1)
	KRCardsPerPlayer = 5

	// KRHandBase 라운드 r 의 1인 배분 장수 = KRHandBase - r (1R 5장 → 4R 2장)
	KRHandBase = 6
)

// KRCard 카드 종류 (와이어 값)
type KRCard string

const (
	KRCardTreasure KRCard = "treasure"
	KRCardKraken   KRCard = "kraken"
	KRCardBlank    KRCard = "blank"
)

// krCardLabel 카드 한글 표기 (이벤트 문구용)
func krCardLabel(card KRCard) string {
	switch card {
	case KRCardTreasure:
		return "보물"
	case KRCardKraken:
		return "크라켄"
	default:
		return "꽝"
	}
}

// KRRole 배정 진영 (와이어 값 — yourRole·종료 공개 role 로만 나간다)
type KRRole string

const (
	KRRoleExpedition KRRole = "expedition"
	KRRoleSkeleton   KRRole = "skeleton"
)

// krRoleLabel 진영 한글 표기
func krRoleLabel(role string) string {
	switch role {
	case string(KRRoleExpedition):
		return "탐험대"
	case string(KRRoleSkeleton):
		return "해골단"
	default:
		return "?"
	}
}

// KRPhase 게임 진행 단계
type KRPhase string

const (
	KRPhaseWaiting   KRPhase = "waiting"
	KRPhaseRevealing KRPhase = "revealing" // 지목권자의 지목 대기 (45초 마감)
	KRPhaseRoundEnd  KRPhase = "round_end" // 라운드 정산 (5초 뒤 재배분)
	KRPhaseGameOver  KRPhase = "game_over"
)

// KRMessageType 크라켄 메시지 타입
type KRMessageType string

const (
	// 클라이언트 → 서버
	KRMsgJoinGame KRMessageType = "kr_join_game"
	KRMsgFillBots KRMessageType = "kr_fill_bots"
	KRMsgStart    KRMessageType = "kr_start"
	KRMsgRejoin   KRMessageType = "kr_rejoin"
	KRMsgPoint    KRMessageType = "kr_point"
	KRMsgClaim    KRMessageType = "kr_claim"
	KRMsgReact    KRMessageType = "kr_react"

	// 서버 → 클라이언트
	KRMsgPlayerJoined       KRMessageType = "kr_player_joined"
	KRMsgSpectateJoined     KRMessageType = "kr_spectate_joined"
	KRMsgGameState          KRMessageType = "kr_game_state"
	KRMsgEvent              KRMessageType = "kr_event"
	KRMsgGameOver           KRMessageType = "kr_game_over"
	KRMsgPlayerDisconnected KRMessageType = "kr_player_disconnected"
	KRMsgPlayerReconnected  KRMessageType = "kr_player_reconnected"
	KRMsgSessionExpired     KRMessageType = "kr_session_expired"
	KRMsgError              KRMessageType = "kr_error"
)

// KRClaim 손패 구성 선언 (디지털 보조 — 거짓말 가능, 구속력 없음).
// 꽝 수는 남은 장수에서 자동 계산하므로 담지 않는다.
// Kraken 은 0(없음)/1(있음) 정수다 — 프론트의 토글과 그대로 대응하고,
// JS 에서는 0 이 falsy 라 불리언처럼 읽어도 안전하다.
type KRClaim struct {
	Treasure int `json:"treasure"`
	Kraken   int `json:"kraken"`
}

// KRPlayer 게임 참가자 (순수 상태 — 연결 매핑은 허브의 krRoom 담당)
type KRPlayer struct {
	Seat int
	Name string
	// Role 배정 진영 — 스냅샷 직접 노출 금지 (본인 yourRole·종료 공개만)
	Role KRRole
	// Hand 이번 라운드에 받은 카드 중 아직 공개되지 않은 것 (본인만 내용을 본다)
	Hand []KRCard
	// RevealedThisRound 이번 라운드에 이 좌석에서 나온 카드 (전원 공개)
	RevealedThisRound []KRCard
	// Claim 본인 선언 (거짓 가능, 라운드 재배분 때 초기화). nil 이면 미선언
	Claim *KRClaim
}

// KRGameEvent 순수 규칙이 쌓는 연출 이벤트 — 허브가 DrainEvents 로 꺼내
// kr_event 로 방송한다 (역할·손패 내용은 절대 담지 않는다. 공개된 카드는
// 이미 공개 정보라 담아도 된다)
type KRGameEvent struct {
	Kind    string
	Seat    int // -1 없음
	Message string
}

// KRRolePool 인원별 역할 풀 — 전원 공개 정보. 풀에서 인원수만큼만 뽑아
// 나눠주므로 실제 진영 구성은 아무도 모른다 (6인만 정확히 4:2).
type KRRolePool struct {
	Expedition int `json:"expedition"`
	Skeleton   int `json:"skeleton"`
}

// KRFound 공개 진척 (전원 공개)
type KRFound struct {
	Treasure      int `json:"treasure"`
	TreasureTotal int `json:"treasureTotal"`
	Kraken        int `json:"kraken"`
}

// KRReveal 마지막 공개 (전원 공개)
type KRReveal struct {
	Seat    int    `json:"seat"`   // 카드 주인
	BySeat  int    `json:"bySeat"` // 지목한 사람
	Card    KRCard `json:"card"`
	Message string `json:"message"`
}

// KRResult 종료 결과 — 무승부 없음
type KRResult struct {
	Winner  string `json:"winner"` // "expedition" | "skeleton"
	Reason  string `json:"reason"` // "kraken" | "all_treasure" | "timeout"
	Message string `json:"message"`
}

// KRGame 크라켄 게임 상태 (순수, 허브 비의존)
type KRGame struct {
	ID      string
	Players []*KRPlayer
	Phase   KRPhase

	// Round 현재 라운드 (1~4, 시작 전 0)
	Round int
	// RevealsLeft 이번 라운드 남은 공개 수 (라운드 시작 시 N)
	RevealsLeft int
	// CurrentSeat 지목권자 좌석 (-1 시작 전)
	CurrentSeat int

	// Pool 이번 판의 역할 풀 (시작 시 확정 — 전원 공개)
	Pool KRRolePool

	// FoundTreasure/FoundKraken 공개 진척
	FoundTreasure int
	FoundKraken   int

	Reveal    *KRReveal // 마지막 공개 (그 전엔 nil)
	Result    *KRResult // 종료 결과 (그 전엔 nil)
	EndReason string    // "kraken" | "all_treasure" | "timeout"
	Ready     bool
	StartedAt time.Time

	// StateSeq 새 대기 상태(지목 차례·라운드 정산)가 열릴 때마다 +1 —
	// 허브가 마감 타이머를 다시 걸지 판단하는 근거
	StateSeq int
	// AfkSeq 마감 타이머 일련번호 (뒤늦은 발화 무시용 — 허브가 관리)
	AfkSeq int
	// Deadline 현재 대기 상태의 마감 시각 (unixMillis — 스냅샷 노출용)
	Deadline int64

	events []KRGameEvent
}

// KRClient 크라켄 클라이언트 연결
type KRClient struct {
	wsClient
	Hub  *KRHub
	Seat int
}

// KRMessage 메시지 봉투
type KRMessage struct {
	Type    KRMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type KRJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type KRRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// KRPointPayload 지목 — 자기 외 좌석의 미공개 카드 1장.
// 좌석 0·인덱스 0 유실을 막기 위해 omitempty 를 쓰지 않는다.
type KRPointPayload struct {
	TargetSeat int `json:"targetSeat"`
	CardIndex  int `json:"cardIndex"`
}

// KRClaimPayload 손패 구성 선언. 프론트의 크라켄 토글이 true/false 로 오든
// 0/1 로 오든 같게 받아들인다 (와이어 관대성 — 서버 출력은 항상 0/1 정수).
type KRClaimPayload struct {
	Treasure int `json:"treasure"`
	Kraken   int `json:"kraken"`
}

// UnmarshalJSON 크라켄 필드의 불리언/정수 양쪽 표기를 흡수한다
func (p *KRClaimPayload) UnmarshalJSON(data []byte) error {
	var raw struct {
		Treasure json.Number     `json:"treasure"`
		Kraken   json.RawMessage `json:"kraken"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw.Treasure != "" {
		n, err := raw.Treasure.Int64()
		if err != nil {
			return err
		}
		p.Treasure = int(n)
	}
	switch string(raw.Kraken) {
	case "", "null", "false", "0":
		p.Kraken = 0
	case "true":
		p.Kraken = 1
	default:
		var n int
		if err := json.Unmarshal(raw.Kraken, &n); err != nil {
			return err
		}
		p.Kraken = n
	}
	return nil
}

type KRReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

// KRPlayerView 좌석별 공개 정보 — 좌석 0·장수 0 유실 방지를 위해 omitempty 금지.
// RevealedThisRound 는 항상 [] 로 나간다 (nil → JSON null 금지).
type KRPlayerView struct {
	Seat              int      `json:"seat"`
	Name              string   `json:"name"`
	Connected         bool     `json:"connected"`
	Bot               bool     `json:"bot"`
	HandCount         int      `json:"handCount"`
	RevealedThisRound []KRCard `json:"revealedThisRound"`
	Claim             *KRClaim `json:"claim"` // 미선언은 null
	Role              string   `json:"role"`  // "" 진행 중 | 종료 후 전원 공개
}

// KRGameStatePayload 개인화된 전체 게임 스냅샷. 모든 상태 변경 후 좌석마다
// 따로 만들어 보낸다. 재접속 복원도 같은 페이로드를 쓴다.
// 은닉: yourRole·yourHand 는 본인에게만 실린다 — 타인·관전자(viewerSeat -1)의
// raw JSON 에는 키 자체가 없다. rolePool 은 전원 공개다.
type KRGameStatePayload struct {
	GameID   string  `json:"gameId"`
	RoomCode string  `json:"roomCode"`
	Phase    KRPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"` // 관전자는 -1
	// Spectators 관전자 수 (참가자·관전자 모두 표시용)
	Spectators int `json:"spectators"`
	// EndsAt 현재 대기 상태(지목·라운드 정산)의 마감 시각 (unixMillis, 그 외 0)
	EndsAt      int64      `json:"endsAt"`
	Round       int        `json:"round"`
	RevealsLeft int        `json:"revealsLeft"`
	CurrentSeat int        `json:"currentSeat"`
	RolePool    KRRolePool `json:"rolePool"`
	// YourRole 본인 진영 — 본인에게만 (관전자·시작 전 부재)
	YourRole string `json:"yourRole,omitempty"`
	// YourHand 본인의 미공개 손패 — 본인에게만 (관전자 부재).
	// 빈 손패도 [] 로 나가야 하므로 슬라이스 포인터로 부재를 표현한다.
	YourHand   *[]KRCard      `json:"yourHand,omitempty"`
	Found      KRFound        `json:"found"`
	Players    []KRPlayerView `json:"players"`
	LastReveal *KRReveal      `json:"lastReveal"` // 그 전엔 null
	Result     *KRResult      `json:"result"`     // 종료 결과 (그 전엔 null)
}

// KREventPayload 연출용 이벤트. 역할·미공개 손패를 담지 않으며 전원에게
// 동일하게 간다. Seat 은 좌석 0 유실 방지를 위해 포인터로 생략을 표현한다.
type KREventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

// KRGameOverPayload 게임 종료 발표 — 전원 역할 공개
type KRGameOverPayload struct {
	Winner  string         `json:"winner"` // "expedition" | "skeleton"
	Reason  string         `json:"reason"` // "kraken" | "all_treasure" | "timeout"
	Message string         `json:"message"`
	Round   int            `json:"round"`
	Found   KRFound        `json:"found"`
	Players []KRPlayerView `json:"players"`
}

type KRPlayerDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type KRPlayerReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type KRErrorPayload struct {
	Message string `json:"message"`
}

// ==================== 역할 풀 표 ====================
//
// 인원 N 일 때 풀에서 N장만 뽑아 나눠준다 — 풀 합이 N 보다 크면 실제 진영
// 구성은 아무도 모른다 (6인만 풀 합 6 이라 정확히 4:2).
//
//	4인 3:2(풀 5) / 5인 4:2(풀 6) / 6인 4:2(풀 6) / 7인 5:3(풀 8) / 8인 6:3(풀 9)
var krRolePools = map[int]KRRolePool{
	4: {Expedition: 3, Skeleton: 2},
	5: {Expedition: 4, Skeleton: 2},
	6: {Expedition: 4, Skeleton: 2},
	7: {Expedition: 5, Skeleton: 3},
	8: {Expedition: 6, Skeleton: 3},
}

// krRolePoolFor 인원별 역할 풀 (표 밖 인원은 zero — 대기실 미리보기용)
func krRolePoolFor(n int) KRRolePool {
	return krRolePools[n]
}

// krHandSize 라운드 r 의 1인 배분 장수 (1R 5장 → 2R 4장 → 3R 3장 → 4R 2장)
func krHandSize(round int) int {
	return KRHandBase - round
}
