package server

import "time"

// ==================== 바퀴벌레 포커 타입 ====================
//
// 3~6인 블러핑 카드 전달. 동물 8종 × 8 = 64장을 전부 나눠 갖고(나머지 제거),
// 전달자가 카드 1장을 뒤집어 대상에게 "이건 쥐다"(거짓 가능)라고 내민다.
// 대상은 참/거짓을 판정하거나, 아직 카드를 안 본 사람이 남아 있으면 몰래
// 확인 후 새 선언으로 넘긴다(릴레이). 판정에 틀리면 그 카드가 자기 앞 공개
// 진열에 쌓이고, 같은 동물 4장이 모이거나 차례에 손패가 0장이면 즉시 패배 —
// 나머지 전원 승리. 은닉의 심장: 손패는 본인만(장수는 공개), 릴레이 중인
// 카드 실물은 cr_peek 수신자(넘기기가 가능한 현재 결정권자)만 본다.

const (
	CRMinPlayers = 3
	CRMaxPlayers = 6

	// CRFillBotTarget cr_fill_bots 가 채우는 목표 인원 — 채운 뒤 즉시 시작
	CRFillBotTarget = 4

	CRAnimalKinds     = 8 // 동물 종류
	CRCopiesPerAnimal = 8 // 동물별 장수 — 8종 × 8 = 64장
	CRLoseCount       = 4 // 같은 동물이 진열에 이만큼 모이면 패배
)

// CRAnimal 동물 카드 (와이어 값 — 프론트와 공유)
type CRAnimal string

const (
	CRCockroach CRAnimal = "cockroach" // 바퀴벌레
	CRRat       CRAnimal = "rat"       // 쥐
	CRBat       CRAnimal = "bat"       // 박쥐
	CRFly       CRAnimal = "fly"       // 파리
	CRScorpion  CRAnimal = "scorpion"  // 전갈
	CRSpider    CRAnimal = "spider"    // 거미
	CRToad      CRAnimal = "toad"      // 두꺼비
	CRStinkbug  CRAnimal = "stinkbug"  // 노린재
)

// crAllAnimals 덱 구성 순서 (셔플 전 기준)
var crAllAnimals = []CRAnimal{
	CRCockroach, CRRat, CRBat, CRFly, CRScorpion, CRSpider, CRToad, CRStinkbug,
}

// crAnimalName 한글 동물명 (이벤트 문구용)
func crAnimalName(a CRAnimal) string {
	switch a {
	case CRCockroach:
		return "바퀴벌레"
	case CRRat:
		return "쥐"
	case CRBat:
		return "박쥐"
	case CRFly:
		return "파리"
	case CRScorpion:
		return "전갈"
	case CRSpider:
		return "거미"
	case CRToad:
		return "두꺼비"
	case CRStinkbug:
		return "노린재"
	}
	return string(a)
}

// crValidAnimal 와이어로 들어온 동물명 검증
func crValidAnimal(a CRAnimal) bool {
	for _, v := range crAllAnimals {
		if v == a {
			return true
		}
	}
	return false
}

// CRPhase 게임 진행 단계
type CRPhase string

const (
	CRPhaseWaiting  CRPhase = "waiting"
	CRPhasePassing  CRPhase = "passing"  // 전달자가 카드·대상·선언을 고르는 중
	CRPhaseDeciding CRPhase = "deciding" // 결정권자가 판정/넘기기를 고르는 중
	CRPhaseGameOver CRPhase = "game_over"
)

// 패배 사유 (와이어 값 — loseReason)
const (
	CRLoseFourAnimals = "four_animals" // 같은 동물 4장이 진열에 모임
	CRLoseEmptyHand   = "empty_hand"   // 전달 차례인데 손패 0장
)

// CRMessageType 바퀴벌레 포커 메시지 타입
type CRMessageType string

const (
	// 클라이언트 → 서버
	CRMsgJoinGame CRMessageType = "cr_join_game"
	CRMsgFillBots CRMessageType = "cr_fill_bots"
	CRMsgStart    CRMessageType = "cr_start"
	CRMsgRejoin   CRMessageType = "cr_rejoin"
	CRMsgPassCard CRMessageType = "cr_pass_card"
	CRMsgRelay    CRMessageType = "cr_relay"
	CRMsgJudge    CRMessageType = "cr_judge"
	CRMsgReact    CRMessageType = "cr_react"

	// 서버 → 클라이언트
	CRMsgPlayerJoined       CRMessageType = "cr_player_joined"
	CRMsgSpectateJoined     CRMessageType = "cr_spectate_joined"
	CRMsgGameState          CRMessageType = "cr_game_state"
	CRMsgEvent              CRMessageType = "cr_event"
	CRMsgPeek               CRMessageType = "cr_peek"
	CRMsgGameOver           CRMessageType = "cr_game_over"
	CRMsgPlayerDisconnected CRMessageType = "cr_player_disconnected"
	CRMsgPlayerReconnected  CRMessageType = "cr_player_reconnected"
	CRMsgSessionExpired     CRMessageType = "cr_session_expired"
	CRMsgError              CRMessageType = "cr_error"
)

// CRPlayer 게임 참가자 (순수 상태 — 연결 매핑은 허브의 crRoom 담당)
type CRPlayer struct {
	Seat    int
	Name    string
	Hand    []CRAnimal       // 손패 — 본인만 내용을 본다 (장수는 공개)
	Display map[CRAnimal]int // 공개 진열 (동물 → 개수) — 전원 공개
}

// CRGameEvent 순수 규칙이 쌓는 연출 이벤트 — 허브가 DrainEvents 로 꺼내
// cr_event 로 방송한다 (릴레이 중인 카드 실물을 담지 않는다 — 판정으로
// 공개된 뒤에만 문구에 실린다)
type CRGameEvent struct {
	Kind    string
	Seat    int // -1 없음
	Message string
}

// CRGame 바퀴벌레 포커 게임 상태 (순수, 허브 비의존)
type CRGame struct {
	ID      string
	Players []*CRPlayer
	Phase   CRPhase

	// PasserSeat 마지막으로 선언하며 카드를 내민 좌석. passing 단계에서는
	// 지금 카드를 골라야 하는 차례 좌석이다.
	PasserSeat int
	// HolderSeat 현재 결정권자 — 판정하거나 넘긴다 (deciding 외 -1)
	HolderSeat int
	// Card 릴레이 중인 카드 실물 — 스냅샷·이벤트에 절대 싣지 않는다.
	// 결정권자에게만 cr_peek 개인 이벤트로 나간다 (넘기기 가능할 때).
	Card CRAnimal
	// Claim 현재 선언 동물 (공개 — 거짓일 수 있다)
	Claim CRAnimal
	// Chain 이 카드를 확인하고 넘긴 경유 좌석들 (원 전달자 → 릴레이 순).
	// 체인에 낀 좌석에게는 다시 넘길 수 없다.
	Chain []int

	LoserSeat  int    // 패자 (-1 미정) — 나머지 전원 승리
	LoseReason string // "" | four_animals | empty_hand
	Ready      bool
	StartedAt  time.Time

	// StateSeq 응답 대기 상태(전달·결정)가 새로 열릴 때마다 +1 —
	// 허브가 마감 타이머를 다시 걸지 판단하는 근거
	StateSeq int
	// AfkSeq 마감 타이머 일련번호 (뒤늦은 발화 무시용 — 허브가 관리)
	AfkSeq int
	// Deadline 현재 대기 상태의 마감 시각 (unixMillis — 스냅샷 노출용)
	Deadline int64

	events []CRGameEvent
}

// CRClient 바퀴벌레 포커 클라이언트 연결
type CRClient struct {
	wsClient
	Hub  *CRHub
	Seat int
}

// CRMessage 메시지 봉투
type CRMessage struct {
	Type    CRMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type CRJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type CRRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// CRPassCardPayload 전달자의 카드 전달 — card 는 내 손패의 동물명(실물),
// claim 은 선언 동물명 (거짓 가능)
type CRPassCardPayload struct {
	Card       string `json:"card"`
	TargetSeat int    `json:"targetSeat"`
	Claim      string `json:"claim"`
}

// CRRelayPayload 넘기기 — cr_peek 로 실물을 확인한 뒤 새 선언으로 전달
type CRRelayPayload struct {
	TargetSeat int    `json:"targetSeat"`
	Claim      string `json:"claim"`
}

// CRJudgePayload 판정 — truth true = "참" 선언, false = "거짓" 선언
type CRJudgePayload struct {
	Truth bool `json:"truth"`
}

// CRReactPayload 리액션 이모지 (화이트리스트 외는 조용히 무시)
type CRReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

// CRPlayerView 수신자 무관 공개 플레이어 정보. 손패는 장수(handCount)만 —
// 내용은 절대 싣지 않는다. 진열(display)은 전원 공개. 좌석 0 유실 방지를
// 위해 int 필드에 omitempty 를 쓰지 않는다.
type CRPlayerView struct {
	Seat      int            `json:"seat"`
	Name      string         `json:"name"`
	Connected bool           `json:"connected"`
	Bot       bool           `json:"bot"`
	HandCount int            `json:"handCount"`
	Display   map[string]int `json:"display"` // 동물 → 개수 (빈 맵 보장)
}

// CRGameStatePayload 개인화된 전체 게임 스냅샷. 모든 상태 변경 후 좌석마다
// 따로 만들어 보낸다. 재접속 복원도 같은 페이로드를 쓴다.
// 은닉: yourHand 는 본인에게만 — 타인·관전자는 필드 자체가 부재.
// 릴레이 중인 카드 실물은 스냅샷 어디에도 없다 (cr_peek 전용).
type CRGameStatePayload struct {
	GameID   string  `json:"gameId"`
	RoomCode string  `json:"roomCode"`
	Phase    CRPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"` // 관전자는 -1
	// Spectators 관전자 수 (참가자·관전자 모두 표시용)
	Spectators int `json:"spectators"`
	// EndsAt 현재 대기 상태(전달·결정)의 마감 시각 (unixMillis, 그 외 0)
	EndsAt     int64  `json:"endsAt"`
	PasserSeat int    `json:"passerSeat"`
	HolderSeat int    `json:"holderSeat"` // 현재 결정권자 (-1 없음)
	Claim      string `json:"claim"`      // 선언 동물 ("" 없음)
	Chain      []int  `json:"chain"`      // 경유 좌석들 (빈 배열 보장)
	// YourHand 내 손패. 관전자·타인은 필드 자체가 부재해야 하므로 포인터 —
	// 좌석 보유자는 빈 손패도 [] 로 나간다 (nil 금지).
	YourHand   *[]string      `json:"yourHand,omitempty"`
	Players    []CRPlayerView `json:"players"`
	LoserSeat  int            `json:"loserSeat"`  // -1 미정
	LoseReason string         `json:"loseReason"` // "" 미정
}

// CRPeekPayload 릴레이 카드 실물 — 넘기기가 가능한 현재 결정권자에게만
// 가는 개인 이벤트다 (마지막 남은 사람은 강제 판정이라 받지 못한다)
type CRPeekPayload struct {
	Animal string `json:"animal"`
}

// CREventPayload 연출용 이벤트. 릴레이 중인 카드 실물을 담지 않으며 전원에게
// 동일하게 간다. Seat/TargetSeat 은 좌석 0 유실 방지를 위해 포인터.
type CREventPayload struct {
	Kind       string `json:"kind"`
	Seat       *int   `json:"seat,omitempty"`
	Name       string `json:"name,omitempty"`
	TargetSeat *int   `json:"targetSeat,omitempty"`
	Message    string `json:"message"`
}

// CRGameOverPayload 게임 종료 발표 — 패자 1인, 나머지 전원 승리
type CRGameOverPayload struct {
	LoserSeat int            `json:"loserSeat"`
	LoserName string         `json:"loserName"`
	Reason    string         `json:"reason"` // four_animals | empty_hand
	Players   []CRPlayerView `json:"players"`
}

type CRPlayerDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type CRPlayerReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type CRErrorPayload struct {
	Message string `json:"message"`
}
