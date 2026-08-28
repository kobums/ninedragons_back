package server

import "time"

// ==================== 스플렌더 (Splendor) 타입 ====================
//
// 2~4인 엔진 빌딩. 공동 창고의 보석 토큰을 모아 개발 카드를 사고, 개발 카드가
// 주는 보너스로 다음 카드를 싸게 사는 눈덩이 구조다. 명성 점수 15점에 먼저
// 닿으면 그 라운드까지만 진행하고 끝난다.
//
// 용어는 정식 한국어판을 따른다 — 명성 점수 / 개발 카드 / 귀족 타일 /
// 다이아몬드 · 사파이어 · 에메랄드 · 루비 · 줄마노 · 황금 / 공동 창고 / 예약.
// 와이어에 실리는 영문 값(diamond·sapphire·emerald·ruby·onyx·gold)은 고정이며
// 화면 표기만 한국어를 쓴다. onyx 가 줄마노다 (오닉스로 옮기지 않는다).
//
// 은닉의 심장은 sl_hub.go 의 buildSLState 다. yourReserved 는 본인 스냅샷에만
// 실리고, 타인·관전자(viewerSeat -1)의 raw JSON 에는 키 자체가 없다. 특히
// 덱 맨 위에서 비공개로 예약한 개발 카드는 남에게 reservedCount 숫자로만
// 보이며 내용(id·비용·명성 점수)은 어떤 경로로도 새지 않는다.

const (
	SLMinPlayers = 2
	SLMaxPlayers = 4

	// SLFillBotTarget sl_fill_bots 가 채우는 목표 인원 — 채운 뒤 즉시 시작
	SLFillBotTarget = 3

	// SLWinPoints 명성 점수 목표 — 누군가 닿으면 그 라운드를 끝까지 진행한다
	SLWinPoints = 15

	// SLTokenLimit 보유 토큰 상한 (황금 포함). 넘으면 차례 끝에 버려 맞춘다.
	SLTokenLimit = 10

	// SLMaxReserved 예약 상한
	SLMaxReserved = 3

	// SLBoardSlots 단계마다 공개하는 개발 카드 수
	SLBoardSlots = 4

	// SLGoldCount 황금 토큰 수 (인원 무관)
	SLGoldCount = 5

	// SLTakeDistinct 서로 다른 색으로 가져오는 토큰 수
	SLTakeDistinct = 3
	// SLTakeSame 같은 색으로 가져오는 토큰 수
	SLTakeSame = 2
	// SLTakeSameMin 같은 색 2개를 가져오려면 공동 창고에 있어야 하는 최소 수
	SLTakeSameMin = 4

	// SLDeckTier1/2/3 단계별 개발 카드 장수 (합 90장)
	SLDeckTier1 = 40
	SLDeckTier2 = 30
	SLDeckTier3 = 20

	// SLMaxTurns 안전 상한. 규칙상 눈덩이가 굴러 15점에 닿지만, 전원이 토큰만
	// 주고받는 병리적 진행에서도 판이 끝나도록 두는 방어선이다. 도달하면
	// 그 시점의 최고 명성 점수로 승자를 가린다.
	SLMaxTurns = 400
)

// SLGem 보석 색 (와이어 값 고정 — onyx 가 줄마노다)
type SLGem string

const (
	SLDiamond  SLGem = "diamond"  // 다이아몬드
	SLSapphire SLGem = "sapphire" // 사파이어
	SLEmerald  SLGem = "emerald"  // 에메랄드
	SLRuby     SLGem = "ruby"     // 루비
	SLOnyx     SLGem = "onyx"     // 줄마노
	SLGold     SLGem = "gold"     // 황금 (만능 — 비용에는 등장하지 않는다)
)

// slGems 보석 5색 (황금 제외). 순서가 곧 색 회전의 기준이다.
var slGems = [5]SLGem{SLDiamond, SLSapphire, SLEmerald, SLRuby, SLOnyx}

// slGemLabel 보석 한글 표기 (정식 한국어판 기준)
func slGemLabel(g SLGem) string {
	switch g {
	case SLDiamond:
		return "다이아몬드"
	case SLSapphire:
		return "사파이어"
	case SLEmerald:
		return "에메랄드"
	case SLRuby:
		return "루비"
	case SLOnyx:
		return "줄마노"
	case SLGold:
		return "황금"
	}
	return "?"
}

// slGemValid 와이어로 들어온 보석 값이 보석 5색인가 (황금은 직접 가져올 수 없다)
func slGemValid(g SLGem) bool {
	for _, valid := range slGems {
		if g == valid {
			return true
		}
	}
	return false
}

// SLGemSet 보석 5색의 개수 묶음. 개발 카드·귀족 타일의 비용과 플레이어의
// 보너스(구매한 개발 카드 수)에 쓴다 — 황금은 이 자리에 오지 않는다.
type SLGemSet struct {
	Diamond  int `json:"diamond"`
	Sapphire int `json:"sapphire"`
	Emerald  int `json:"emerald"`
	Ruby     int `json:"ruby"`
	Onyx     int `json:"onyx"`
}

func (s SLGemSet) get(g SLGem) int {
	switch g {
	case SLDiamond:
		return s.Diamond
	case SLSapphire:
		return s.Sapphire
	case SLEmerald:
		return s.Emerald
	case SLRuby:
		return s.Ruby
	case SLOnyx:
		return s.Onyx
	}
	return 0
}

func (s *SLGemSet) add(g SLGem, n int) {
	switch g {
	case SLDiamond:
		s.Diamond += n
	case SLSapphire:
		s.Sapphire += n
	case SLEmerald:
		s.Emerald += n
	case SLRuby:
		s.Ruby += n
	case SLOnyx:
		s.Onyx += n
	}
}

// total 5색 합계
func (s SLGemSet) total() int {
	return s.Diamond + s.Sapphire + s.Emerald + s.Ruby + s.Onyx
}

// SLTokenSet 보석 5색 + 황금. 공동 창고와 플레이어 보유 토큰에 쓴다.
type SLTokenSet struct {
	Diamond  int `json:"diamond"`
	Sapphire int `json:"sapphire"`
	Emerald  int `json:"emerald"`
	Ruby     int `json:"ruby"`
	Onyx     int `json:"onyx"`
	Gold     int `json:"gold"`
}

func (s SLTokenSet) get(g SLGem) int {
	switch g {
	case SLDiamond:
		return s.Diamond
	case SLSapphire:
		return s.Sapphire
	case SLEmerald:
		return s.Emerald
	case SLRuby:
		return s.Ruby
	case SLOnyx:
		return s.Onyx
	case SLGold:
		return s.Gold
	}
	return 0
}

func (s *SLTokenSet) add(g SLGem, n int) {
	switch g {
	case SLDiamond:
		s.Diamond += n
	case SLSapphire:
		s.Sapphire += n
	case SLEmerald:
		s.Emerald += n
	case SLRuby:
		s.Ruby += n
	case SLOnyx:
		s.Onyx += n
	case SLGold:
		s.Gold += n
	}
}

// total 황금을 포함한 보유 토큰 총수 (10개 상한 판정의 근거)
func (s SLTokenSet) total() int {
	return s.Diamond + s.Sapphire + s.Emerald + s.Ruby + s.Onyx + s.Gold
}

// SLPhase 게임 진행 단계
type SLPhase string

const (
	SLPhaseWaiting  SLPhase = "waiting"
	SLPhaseTurn     SLPhase = "turn"    // 차례 진행 (60초 마감 — 자동 행동)
	SLPhaseDiscard  SLPhase = "discard" // 10개 초과분 버리기 (20초 마감 — 무작위)
	SLPhaseGameOver SLPhase = "game_over"
)

// SLMessageType 스플렌더 메시지 타입
type SLMessageType string

const (
	// 클라이언트 → 서버
	SLMsgJoinGame SLMessageType = "sl_join_game"
	SLMsgFillBots SLMessageType = "sl_fill_bots"
	SLMsgStart    SLMessageType = "sl_start"
	SLMsgRejoin   SLMessageType = "sl_rejoin"
	SLMsgTake     SLMessageType = "sl_take"
	SLMsgReserve  SLMessageType = "sl_reserve"
	SLMsgBuy      SLMessageType = "sl_buy"
	SLMsgDiscard  SLMessageType = "sl_discard"
	SLMsgReact    SLMessageType = "sl_react"

	// 서버 → 클라이언트
	SLMsgPlayerJoined       SLMessageType = "sl_player_joined"
	SLMsgSpectateJoined     SLMessageType = "sl_spectate_joined"
	SLMsgGameState          SLMessageType = "sl_game_state"
	SLMsgEvent              SLMessageType = "sl_event"
	SLMsgGameOver           SLMessageType = "sl_game_over"
	SLMsgPlayerDisconnected SLMessageType = "sl_player_disconnected"
	SLMsgPlayerReconnected  SLMessageType = "sl_player_reconnected"
	SLMsgSessionExpired     SLMessageType = "sl_session_expired"
	SLMsgError              SLMessageType = "sl_error"
)

// SLCard 개발 카드 한 장. Gem 은 사면 얻는 보너스 색이고, Cost 는 보너스를
// 빼기 전의 정가다 (실제 지불은 보너스를 깎고 황금으로 메운다).
type SLCard struct {
	ID     int      `json:"id"`
	Tier   int      `json:"tier"` // 1 · 2 · 3 단계
	Points int      `json:"points"`
	Gem    SLGem    `json:"gem"`
	Cost   SLGemSet `json:"cost"`
}

// SLNoble 귀족 타일. 요구 보너스를 모두 갖추면 차례 끝에 자동으로 찾아온다.
type SLNoble struct {
	ID     int      `json:"id"`
	Points int      `json:"points"`
	Cost   SLGemSet `json:"cost"`
}

// SLPlayer 게임 참가자 (순수 상태 — 연결 매핑은 허브의 slRoom 담당)
type SLPlayer struct {
	Seat int
	Name string
	// Tokens 보유 토큰 (황금 포함, 10개 상한)
	Tokens SLTokenSet
	// Cards 구매한 개발 카드의 색별 개수 = 보너스. 비용을 깎는다.
	Cards SLGemSet
	// Points 명성 점수 (개발 카드 + 귀족 타일)
	Points int
	// Reserved 예약한 개발 카드 — 본인만 내용을 본다
	Reserved []SLCard
	// Nobles 획득한 귀족 타일 id 목록
	Nobles []int
}

// SLGameEvent 순수 규칙이 쌓는 연출 이벤트 — 허브가 DrainEvents 로 꺼내
// sl_event 로 방송한다 (예약 카드의 내용은 절대 담지 않는다)
type SLGameEvent struct {
	Kind    string
	Seat    int // -1 없음
	Message string
}

// SLLastAction 마지막 행동 요약 (전원 공개)
type SLLastAction struct {
	Seat    int    `json:"seat"`
	Name    string `json:"name"`
	Message string `json:"message"`
}

// SLResult 종료 결과. 동점 + 개발 카드 수까지 같으면 공동 승이라 좌석이 여럿이다.
type SLResult struct {
	WinnerSeats []int    `json:"winnerSeats"`
	WinnerNames []string `json:"winnerNames"`
	Message     string   `json:"message"`
}

// SLGame 스플렌더 게임 상태 (순수, 허브 비의존)
type SLGame struct {
	ID      string
	Players []*SLPlayer
	Phase   SLPhase

	// Bank 공동 창고
	Bank SLTokenSet
	// Decks 단계별 남은 덱 (앞에서 뽑는다). 인덱스 0·1·2 = 1·2·3단계
	Decks [3][]SLCard
	// Board 단계별 진열 (각 최대 SLBoardSlots 장)
	Board [3][]SLCard
	// Nobles 남은 귀족 타일 (인원 + 1장으로 시작)
	Nobles []SLNoble

	// CurrentSeat 차례 좌석 (-1 시작 전)
	CurrentSeat int
	// Turns 지금까지 진행된 차례 수
	Turns int
	// LastRound 누군가 15점에 닿아 마지막 라운드에 들어갔는가
	LastRound bool

	LastAction *SLLastAction
	Result     *SLResult
	Ready      bool
	StartedAt  time.Time

	// StateSeq 새 대기 상태(차례·버리기)가 열릴 때마다 +1 — 허브가 마감
	// 타이머를 다시 걸지 판단하는 근거
	StateSeq int
	// AfkSeq 마감 타이머 일련번호 (뒤늦은 발화 무시용 — 허브가 관리)
	AfkSeq int
	// Deadline 현재 대기 상태의 마감 시각 (unixMillis — 스냅샷 노출용)
	Deadline int64

	events []SLGameEvent
}

// SLClient 스플렌더 클라이언트 연결
type SLClient struct {
	wsClient
	Hub  *SLHub
	Seat int
}

// SLMessage 메시지 봉투
type SLMessage struct {
	Type    SLMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type SLJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type SLRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// SLTakePayload 토큰 가져오기. 서로 다른 색 3개는 ["diamond","ruby","onyx"],
// 같은 색 2개는 ["ruby","ruby"] 로 보낸다.
type SLTakePayload struct {
	Colors []SLGem `json:"colors"`
}

// SLReservePayload 예약. 공개 카드는 cardId(1 이상), 덱 맨 위는 tier(1~3).
// 카드 id 는 1부터 시작하므로 0 은 "지정 없음"을 뜻한다.
type SLReservePayload struct {
	CardID int `json:"cardId"`
	Tier   int `json:"tier"`
}

// SLBuyPayload 구매. 공개 카드 또는 내가 예약한 카드의 id.
type SLBuyPayload struct {
	CardID int `json:"cardId"`
}

// SLDiscardPayload 10개 초과분 버리기 — 버릴 토큰 색을 개수만큼 나열한다
type SLDiscardPayload struct {
	Colors []SLGem `json:"colors"`
}

type SLReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

// SLBoardView 단계별 공개 진열. 빈 단계도 [] 로 나간다 (nil → null 금지).
type SLBoardView struct {
	Tier1 []SLCard `json:"tier1"`
	Tier2 []SLCard `json:"tier2"`
	Tier3 []SLCard `json:"tier3"`
}

// SLDeckLeft 단계별 덱 잔량 (뒷면이라 내용은 아무도 모른다)
type SLDeckLeft struct {
	Tier1 int `json:"tier1"`
	Tier2 int `json:"tier2"`
	Tier3 int `json:"tier3"`
}

// SLPlayerView 좌석별 공개 정보. 좌석 0·점수 0·예약 0 유실을 막기 위해
// omitempty 를 쓰지 않는다. 예약은 장수만 공개하고 내용은 담지 않는다.
type SLPlayerView struct {
	Seat          int        `json:"seat"`
	Name          string     `json:"name"`
	Connected     bool       `json:"connected"`
	Bot           bool       `json:"bot"`
	Points        int        `json:"points"`
	Cards         SLGemSet   `json:"cards"`
	Tokens        SLTokenSet `json:"tokens"`
	ReservedCount int        `json:"reservedCount"`
	Nobles        []int      `json:"nobles"`
}

// SLGameStatePayload 개인화된 전체 게임 스냅샷. 모든 상태 변경 후 좌석마다
// 따로 만들어 보낸다. 재접속 복원도 같은 페이로드를 쓴다.
// 은닉: yourReserved 는 본인에게만 실린다 — 타인·관전자(viewerSeat -1)의
// raw JSON 에는 키 자체가 없다. 빈 예약도 [] 로 보내야 하므로 슬라이스
// 포인터로 부재를 표현한다.
type SLGameStatePayload struct {
	GameID   string  `json:"gameId"`
	RoomCode string  `json:"roomCode"`
	Phase    SLPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"` // 관전자는 -1
	// Spectators 관전자 수
	Spectators int `json:"spectators"`
	// EndsAt 현재 대기 상태의 마감 시각 (unixMillis, 그 외 0)
	EndsAt      int64 `json:"endsAt"`
	CurrentSeat int   `json:"currentSeat"`
	// LastRound 마지막 라운드 진행 중인가 (누군가 15점에 닿았다)
	LastRound bool `json:"lastRound"`
	// Bank 공동 창고
	Bank     SLTokenSet  `json:"bank"`
	Board    SLBoardView `json:"board"`
	DeckLeft SLDeckLeft  `json:"deckLeft"`
	Nobles   []SLNoble   `json:"nobles"`
	// YourReserved 본인 예약 카드 — 본인에게만 (관전자·타인 부재).
	// 덱에서 비공개로 예약한 카드도 여기에만 실린다.
	YourReserved *[]SLCard      `json:"yourReserved,omitempty"`
	Players      []SLPlayerView `json:"players"`
	LastAction   *SLLastAction  `json:"lastAction"` // 그 전엔 null
	Result       *SLResult      `json:"result"`     // 종료 결과 (그 전엔 null)
}

// SLEventPayload 연출용 이벤트. 예약 카드의 내용을 담지 않으며 전원에게
// 동일하게 간다. Seat 은 좌석 0 유실 방지를 위해 포인터로 생략을 표현한다.
type SLEventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

// SLGameOverPayload 게임 종료 발표
type SLGameOverPayload struct {
	WinnerSeats []int          `json:"winnerSeats"`
	WinnerNames []string       `json:"winnerNames"`
	Message     string         `json:"message"`
	Turns       int            `json:"turns"`
	Players     []SLPlayerView `json:"players"`
}

type SLPlayerDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type SLPlayerReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type SLErrorPayload struct {
	Message string `json:"message"`
}

// ==================== 공동 창고 인원표 ====================
//
// 보석 5색 각각의 개수. 황금은 인원과 무관하게 항상 5개다.
//
//	2인 4개 / 3인 5개 / 4인 7개
var slBankPerColor = map[int]int{2: 4, 3: 5, 4: 7}

// slBankFor 인원별 보석 1색 개수 (표 밖 인원은 4 — 대기실 미리보기 방어)
func slBankFor(n int) int {
	if v, ok := slBankPerColor[n]; ok {
		return v
	}
	return 4
}

// slNobleCount 공개하는 귀족 타일 수 = 인원 + 1
func slNobleCount(n int) int { return n + 1 }
