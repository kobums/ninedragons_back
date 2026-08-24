package server

import "time"

// ==================== 익스플로딩 키튼 (Exploding Kittens) 타입 ====================
//
// 2~5인 탈락형 카드 소품. 폭탄 고양이는 인원-1 장이라 반드시 마지막 1명이
// 남는다. 자기 차례에 카드를 원하는 만큼 냈다가 마지막에 1장 뽑으면 차례가
// 끝나고, 뽑은 카드가 폭탄이면 해체로 막고 덱 아무 위치에나 비공개로
// 되꽂는다 — 해체가 없으면 탈락(방을 나가지 않고 관전으로 전환)이다.
//
// 계약의 심장은 **안돼 창**이다. 기능 카드가 나오면 nope_window 로 전환해
// 5초 동안 안돼를 받고, 안돼가 나오면 창을 다시 연다(재안돼 무제한). 창
// 상태는 쿠(cp_hub.go)와 똑같이 StateSeq/DeadlineSeq/AfkSeq 로 관리한다 —
// 안돼가 겹칠 때마다 StateSeq 가 올라 마감이 새로 걸리고, 통과 누적만으로는
// 마감이 늘어나지 않는다. 안돼 카드는 유한하므로 창은 반드시 닫힌다.

const (
	EKMinPlayers = 2
	EKMaxPlayers = 5

	// EKFillBotTarget ek_fill_bots 가 채우는 목표 인원 — 채운 뒤 즉시 시작
	EKFillBotTarget = 4

	// EKStartHand 시작 손패 중 해체 1장을 뺀 나머지 장수
	EKStartHand = 7

	// EKDefuseTotal 해체 총 장수 — 1인 1장씩 나눠 주고 남는 건 덱에 섞는다
	EKDefuseTotal = 6

	// EKFutureCount 미리보기가 들여다보는 덱 맨 위 장수
	EKFutureCount = 3

	// EKPairSize 고양이 훔치기에 필요한 같은 종류 장수
	EKPairSize = 2
)

// EKCard 카드 종류 (와이어 값 — 프론트와 공유)
type EKCard string

const (
	EKCardBomb    EKCard = "bomb"
	EKCardDefuse  EKCard = "defuse"
	EKCardAttack  EKCard = "attack"
	EKCardSkip    EKCard = "skip"
	EKCardFavor   EKCard = "favor"
	EKCardShuffle EKCard = "shuffle"
	EKCardFuture  EKCard = "future"
	EKCardNope    EKCard = "nope"

	// 고양이 카드 5종 — 기능은 없고 같은 종류 2장으로 훔치기에만 쓴다
	EKCardTaco    EKCard = "taco"
	EKCardRainbow EKCard = "rainbow"
	EKCardBeard   EKCard = "beard"
	EKCardPotato  EKCard = "potato"
	EKCardMelon   EKCard = "melon"
)

// EKPendKindPair 고양이 2장 훔치기의 pending.kind (카드 종류가 아닌 행동명)
const EKPendKindPair = "pair"

// ekCatCards 고양이 5종
var ekCatCards = []EKCard{EKCardTaco, EKCardRainbow, EKCardBeard, EKCardPotato, EKCardMelon}

// ekBaseCounts 폭탄·해체를 뺀 기본 덱 구성 (합 46장).
// 시작 손패 7장씩은 여기서 나눠 주고, 그 뒤 폭탄 n-1 장과 남은 해체
// 6-n 장을 덱에 섞는다.
var ekBaseCounts = []struct {
	Card EKCard
	N    int
}{
	{EKCardAttack, 4},
	{EKCardSkip, 4},
	{EKCardFavor, 4},
	{EKCardShuffle, 4},
	{EKCardFuture, 5},
	{EKCardNope, 5},
	{EKCardTaco, 4},
	{EKCardRainbow, 4},
	{EKCardBeard, 4},
	{EKCardPotato, 4},
	{EKCardMelon, 4},
}

// ekIsCat 기능 없는 고양이 카드인지
func ekIsCat(c EKCard) bool {
	for _, cat := range ekCatCards {
		if cat == c {
			return true
		}
	}
	return false
}

// ekCardName 한글 카드명 (이벤트 문구용)
func ekCardName(c EKCard) string {
	switch c {
	case EKCardBomb:
		return "폭탄 고양이"
	case EKCardDefuse:
		return "해체"
	case EKCardAttack:
		return "공격"
	case EKCardSkip:
		return "건너뛰기"
	case EKCardFavor:
		return "호의"
	case EKCardShuffle:
		return "섞기"
	case EKCardFuture:
		return "미리보기"
	case EKCardNope:
		return "안돼"
	case EKCardTaco:
		return "타코냥이"
	case EKCardRainbow:
		return "무지개냥이"
	case EKCardBeard:
		return "수염냥이"
	case EKCardPotato:
		return "털감자냥이"
	case EKCardMelon:
		return "수박냥이"
	}
	return string(c)
}

// ekPendingName pending.kind 의 한글 이름 (고양이 짝은 카드가 아니다)
func ekPendingName(kind string) string {
	if kind == EKPendKindPair {
		return "고양이 짝 훔치기"
	}
	return ekCardName(EKCard(kind))
}

// EKPhase 게임 진행 단계
type EKPhase string

const (
	EKPhaseWaiting     EKPhase = "waiting"
	EKPhaseTurn        EKPhase = "turn"         // 차례 — 카드를 내거나 뽑는다
	EKPhaseNopeWindow  EKPhase = "nope_window"  // 낸 카드에 안돼를 받는 창
	EKPhaseFavorWait   EKPhase = "favor_wait"   // 호의 대상이 줄 카드를 고르는 중
	EKPhaseDefusePlace EKPhase = "defuse_place" // 해체로 막은 폭탄을 되꽂는 중
	EKPhaseGameOver    EKPhase = "game_over"
)

// EKMessageType 익스플로딩 키튼 메시지 타입
type EKMessageType string

const (
	// 클라이언트 → 서버
	EKMsgJoinGame    EKMessageType = "ek_join_game"
	EKMsgFillBots    EKMessageType = "ek_fill_bots"
	EKMsgStart       EKMessageType = "ek_start"
	EKMsgRejoin      EKMessageType = "ek_rejoin"
	EKMsgPlay        EKMessageType = "ek_play"
	EKMsgPlayPair    EKMessageType = "ek_play_pair"
	EKMsgDraw        EKMessageType = "ek_draw"
	EKMsgNope        EKMessageType = "ek_nope"
	EKMsgPass        EKMessageType = "ek_pass"
	EKMsgGive        EKMessageType = "ek_give"
	EKMsgDefusePlace EKMessageType = "ek_defuse_place"
	EKMsgReact       EKMessageType = "ek_react"

	// 서버 → 클라이언트
	EKMsgPlayerJoined       EKMessageType = "ek_player_joined"
	EKMsgSpectateJoined     EKMessageType = "ek_spectate_joined"
	EKMsgGameState          EKMessageType = "ek_game_state"
	EKMsgEvent              EKMessageType = "ek_event"
	EKMsgFuture             EKMessageType = "ek_future" // 개인 이벤트 — 그 사람에게만
	EKMsgGameOver           EKMessageType = "ek_game_over"
	EKMsgPlayerDisconnected EKMessageType = "ek_player_disconnected"
	EKMsgPlayerReconnected  EKMessageType = "ek_player_reconnected"
	EKMsgSessionExpired     EKMessageType = "ek_session_expired"
	EKMsgError              EKMessageType = "ek_error"
)

// EKPlayer 게임 참가자 (순수 상태 — 연결 매핑은 허브의 ekRoom 담당).
// 탈락해도 목록에서 빼지 않는다 (Alive=false 로 남아 관전 전환).
type EKPlayer struct {
	Seat  int
	Name  string
	Hand  []EKCard
	Alive bool
}

// HasCard 손패에 해당 종류가 있는 첫 인덱스 (-1 없음)
func (p *EKPlayer) HasCard(c EKCard) int {
	for i, card := range p.Hand {
		if card == c {
			return i
		}
	}
	return -1
}

// EKPending 안돼 창(과 그 뒤로 이어지는 대기 상태)의 진행 상태.
//
//	nope_window   — 낸 카드/짝의 효과가 발동 대기 중. NopeCount 가 짝수면 유효.
//	favor_wait    — Kind="favor", TargetSeat 이 줄 카드를 고르는 중.
//	defuse_place  — Kind="defuse", BySeat 이 폭탄을 되꽂는 중.
type EKPending struct {
	Kind       string // 카드 종류 또는 "pair"
	BySeat     int    // 카드를 낸 좌석
	TargetSeat int    // -1 없음
	NopeCount  int    // 겹친 안돼 수 — 짝수면 효과 유효

	// LastSeat 가장 최근에 카드를 낸 좌석 — 이 좌석은 자기 카드에
	// 안돼를 겹칠 수 없다 (현재 창의 응답자에서 제외된다)
	LastSeat int

	// passed 현재 창에서 통과(ek_pass)를 누른 좌석. 안돼가 나오면 비워
	// 창을 처음부터 다시 연다.
	passed map[int]bool
}

// EKGameEvent 순수 규칙이 쌓는 연출 이벤트 — 허브가 DrainEvents 로 꺼내
// ek_event 로 방송한다 (비밀 정보를 담지 않는다)
type EKGameEvent struct {
	Kind    string
	Seat    int // -1 없음
	Message string
}

// EKPrivateEvent 미리보기 결과 — 그 좌석에게만 ek_future 로 간다.
// 절대 방송하지 않는다 (덱 내용 은닉의 유일한 예외 경로).
type EKPrivateEvent struct {
	Seat  int
	Cards []EKCard
}

// EKGame 익스플로딩 키튼 게임 상태 (순수, 허브 비의존)
type EKGame struct {
	ID      string
	Players []*EKPlayer
	Phase   EKPhase

	// Deck 남은 덱 (맨 앞이 맨 위). 내용은 어떤 스냅샷에도 실리지 않는다.
	Deck []EKCard
	// Discard 버린 더미 — 맨 위(마지막)만 공개된다
	Discard []EKCard

	CurrentSeat int // 차례 좌석 (창 동안에도 유지, -1 없음)
	TurnsLeft   int // 현재 좌석이 남긴 차례 수 (공격 누적 — 현재 차례 포함)

	Pending *EKPending // 안돼 창·호의 대기·되꽂기 대기 (그 외 nil)

	WinnerSeat int // 최후 1인 (-1 미정)
	Ready      bool
	StartedAt  time.Time

	LastAction *EKLastActionView // 마지막 행동 요약 (스냅샷 lastAction)

	// StateSeq 응답 대기 상태(차례·안돼 창·호의·되꽂기)가 새로 열릴 때마다
	// +1 — 허브가 마감 타이머를 다시 걸지 판단하는 근거 (cp 와 동일)
	StateSeq int
	// AfkSeq 마감 타이머 일련번호 (뒤늦은 발화 무시용 — 허브가 관리)
	AfkSeq int
	// Deadline 현재 대기 상태의 마감 시각 (unixMillis — 스냅샷 노출용)
	Deadline int64

	events   []EKGameEvent
	privates []EKPrivateEvent
}

// EKClient 익스플로딩 키튼 클라이언트 연결
type EKClient struct {
	wsClient
	Hub  *EKHub
	Seat int
}

// EKMessage 메시지 봉투
type EKMessage struct {
	Type    EKMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type EKJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type EKRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// EKPlayPayload 카드 한 장 내기. TargetSeat 은 좌석 0과 생략을 구분하기
// 위한 포인터다 (favor 만 사용).
type EKPlayPayload struct {
	Index      int  `json:"index"`
	TargetSeat *int `json:"targetSeat,omitempty"`
}

// EKPlayPairPayload 같은 종류 고양이 2장으로 훔치기
type EKPlayPairPayload struct {
	Indexes    []int `json:"indexes"`
	TargetSeat *int  `json:"targetSeat,omitempty"`
}

// EKGivePayload 호의 대상이 건넬 카드 — index 는 yourHand 기준
type EKGivePayload struct {
	Index int `json:"index"`
}

// EKDefusePlacePayload 폭탄 되꽂기 위치 — 0=맨 위 … len(deck)=맨 아래.
// 위치는 본인만 알고 어떤 스냅샷·이벤트에도 실리지 않는다.
type EKDefusePlacePayload struct {
	Position int `json:"position"`
}

type EKReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

// EKPlayerView 좌석별 공개 정보 — 손패는 장수(handCount)만, 내용은 절대
// 싣지 않는다. 좌석 0·손패 0 유실 방지를 위해 omitempty 금지.
type EKPlayerView struct {
	Seat      int    `json:"seat"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Bot       bool   `json:"bot"`
	HandCount int    `json:"handCount"`
	Alive     bool   `json:"alive"`
}

// EKPendingView 안돼 창 대상 요약 (전원 공통 — 무엇을 냈는지는 공개 정보다).
// favor_wait·defuse_place 동안에는 응답해야 하는 좌석을 알리는 데 쓰인다.
type EKPendingView struct {
	Kind       string `json:"kind"`
	BySeat     int    `json:"bySeat"`
	TargetSeat int    `json:"targetSeat"` // -1 없음
	NopeCount  int    `json:"nopeCount"`
}

// EKHandCardView 손패 카드 한 장 (본인 스냅샷에만 실린다)
type EKHandCardView struct {
	Kind string `json:"kind"`
}

// EKLastActionView 마지막 행동 요약 (비밀 정보를 담지 않는다)
type EKLastActionView struct {
	Seat    int    `json:"seat"`
	Name    string `json:"name"`
	Message string `json:"message"`
}

// EKResultView 종료 결과 (게임 중에는 null)
type EKResultView struct {
	WinnerSeat int    `json:"winnerSeat"`
	WinnerName string `json:"winnerName"`
	Message    string `json:"message"`
}

// EKGameStatePayload 개인화된 전체 게임 스냅샷. 모든 상태 변경 후 좌석마다
// 따로 만들어 보낸다. 재접속 복원도 같은 페이로드를 쓴다.
//
// 은닉:
//   - YourHand 는 본인에게만 실린다. 포인터라 타인·관전자의 raw JSON 에는
//     키 자체가 없다 (본인이 빈손이면 null 이 아니라 []).
//   - 덱 내용과 폭탄 위치는 어디에도 없다 — deckLeft 장수만 공개된다.
//   - 미리보기 결과는 이 스냅샷이 아니라 ek_future 개인 메시지로만 간다.
type EKGameStatePayload struct {
	GameID   string  `json:"gameId"`
	RoomCode string  `json:"roomCode"`
	Phase    EKPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"` // 관전자는 -1
	// Spectators 관전자 수 (참가자·관전자 모두 표시용)
	Spectators int `json:"spectators"`
	// EndsAt 현재 대기 상태의 마감 시각 (unixMillis, 그 외 0)
	EndsAt      int64 `json:"endsAt"`
	CurrentSeat int   `json:"currentSeat"`
	TurnsLeft   int   `json:"turnsLeft"`
	DeckLeft    int   `json:"deckLeft"`
	// DiscardTop 버린 더미 맨 위 카드 종류 ("" 비어 있음)
	DiscardTop string            `json:"discardTop"`
	Pending    *EKPendingView    `json:"pending"`
	YourHand   *[]EKHandCardView `json:"yourHand,omitempty"`
	Players    []EKPlayerView    `json:"players"`
	LastAction *EKLastActionView `json:"lastAction"`
	Result     *EKResultView     `json:"result"`
}

// EKEventPayload 연출용 이벤트. 비밀 정보를 담지 않으며 전원에게 동일하게
// 간다. Seat 은 좌석 0 유실 방지를 위해 포인터로 생략을 표현한다.
type EKEventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

// EKFuturePayload 미리보기 결과 — 덱 맨 위부터 최대 3장. 그 사람에게만 간다.
type EKFuturePayload struct {
	Cards []string `json:"cards"`
}

// EKGameOverPayload 게임 종료 발표 (최후 1인)
type EKGameOverPayload struct {
	WinnerSeat int            `json:"winnerSeat"`
	WinnerName string         `json:"winnerName"`
	Players    []EKPlayerView `json:"players"`
}

type EKPlayerDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type EKPlayerReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type EKErrorPayload struct {
	Message string `json:"message"`
}
