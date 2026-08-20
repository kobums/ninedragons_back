package server

import "time"

// ==================== 요트 다이스 타입 ====================

// YTMinPlayers / YTMaxPlayers 인원 범위 (2~4인)
const (
	YTMinPlayers = 2
	YTMaxPlayers = 4
)

// YTDiceCount 주사위 개수 — 와이어의 dice/held 는 항상 이 길이다 (nil 금지)
const YTDiceCount = 5

// YTMaxRolls 턴당 최대 굴림 횟수
const YTMaxRolls = 3

// 상단(1~6의 눈) 합계 보너스 — 63점 이상이면 +35
const (
	YTUpperBonusThreshold = 63
	YTUpperBonus          = 35
)

// 점수표 카테고리 — 문자열 값이 곧 와이어 표현이다 (yt_score payload)
const (
	YTCatOnes      = "ones"
	YTCatTwos      = "twos"
	YTCatThrees    = "threes"
	YTCatFours     = "fours"
	YTCatFives     = "fives"
	YTCatSixes     = "sixes"
	YTCatTriple    = "triple"    // 3 of a kind — 전체 합
	YTCatQuad      = "quad"      // 4 of a kind — 전체 합
	YTCatFullHouse = "fullhouse" // 3+2 조합 — 25
	YTCatSmall     = "small"     // 4연속 — 30
	YTCatLarge     = "large"     // 5연속 — 40
	YTCatYacht     = "yacht"     // 5개 동일 — 50
	YTCatChance    = "chance"    // 전체 합
)

// ytCategories 점수표 순서 (상단 6 + 하단 7 = 13칸). 앞 6개가 상단이다.
// 전 칸을 기록하면 게임이 끝나므로 총 라운드 수는 이 길이와 같다.
var ytCategories = []string{
	YTCatOnes, YTCatTwos, YTCatThrees, YTCatFours, YTCatFives, YTCatSixes,
	YTCatTriple, YTCatQuad, YTCatFullHouse, YTCatSmall, YTCatLarge,
	YTCatYacht, YTCatChance,
}

// ytUpperCategoryCount 상단 칸 수 (ytCategories 앞부분)
const ytUpperCategoryCount = 6

// YTPhase 게임 진행 단계
type YTPhase string

const (
	YTPhaseWaiting  YTPhase = "waiting"
	YTPhasePlaying  YTPhase = "playing"
	YTPhaseGameOver YTPhase = "game_over"
)

// YTMessageType 요트 메시지 타입
type YTMessageType string

const (
	// 클라이언트 → 서버
	YTMsgJoinGame YTMessageType = "yt_join_game"
	YTMsgFillBots YTMessageType = "yt_fill_bots"
	YTMsgStart    YTMessageType = "yt_start"
	YTMsgRejoin   YTMessageType = "yt_rejoin"
	YTMsgRoll     YTMessageType = "yt_roll"
	YTMsgScore    YTMessageType = "yt_score"
	YTMsgReact    YTMessageType = "yt_react"

	// 서버 → 클라이언트
	YTMsgPlayerJoined       YTMessageType = "yt_player_joined"
	YTMsgSpectateJoined     YTMessageType = "yt_spectate_joined"
	YTMsgGameState          YTMessageType = "yt_game_state"
	YTMsgEvent              YTMessageType = "yt_event"
	YTMsgGameOver           YTMessageType = "yt_game_over"
	YTMsgPlayerDisconnected YTMessageType = "yt_player_disconnected"
	YTMsgPlayerReconnected  YTMessageType = "yt_player_reconnected"
	YTMsgSessionExpired     YTMessageType = "yt_session_expired"
	YTMsgError              YTMessageType = "yt_error"
)

// YTPlayer 게임 참가자 (순수 상태 — 연결 매핑은 허브의 ytRoom 담당)
type YTPlayer struct {
	Seat   int
	Name   string
	Scores map[string]int // 카테고리 → 점수, -1 = 미기록
}

// YTLastAction 직전 기록 (스냅샷 노출용 — 마지막 yt_score 의 결과)
type YTLastAction struct {
	Seat     int    `json:"seat"`
	Category string `json:"category"`
	Points   int    `json:"points"`
}

// YTGame 요트 게임 상태 (순수, 허브 비의존). 주사위는 전원 공개 —
// 은닉 정보가 전혀 없어 스냅샷 개인화는 yourSeat 뿐이다.
type YTGame struct {
	ID      string
	Players []*YTPlayer
	Phase   YTPhase

	CurrentSeat int
	FirstSeat   int // 라운드 순환 기준 좌석 (선공)
	Round       int // 1~13 (waiting 은 0)

	Dice      []int  // 항상 5개 — 게임 첫 굴림 전에는 0 (미굴림 표식)
	Held      []bool // 항상 5개 — 현재 턴의 홀드 상태
	RollsLeft int    // 현재 턴의 남은 굴림 (3~0)

	LastAction  *YTLastAction
	WinnerSeats []int // 게임 종료 시 승자 좌석들 (동점 공동 승), 그 외 []
	Ready       bool
	StartedAt   time.Time
}

// YTClient 요트 클라이언트 연결
type YTClient struct {
	wsClient
	Hub  *YTHub
	Seat int
}

// YTMessage 메시지 봉투
type YTMessage struct {
	Type    YTMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type YTJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type YTRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// YTRollPayload 주사위 굴리기. Hold 는 주사위 5개 각각의 고정 여부 —
// 턴의 첫 굴림에서는 무시된다 (전부 굴림).
type YTRollPayload struct {
	Hold []bool `json:"hold"`
}

// YTScorePayload 점수 기록 — category 는 ytCategories 의 값
type YTScorePayload struct {
	Category string `json:"category"`
}

// YTReactPayload 리액션 이모지 (화이트리스트 외는 조용히 무시)
type YTReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

// YTPlayerView 공개 플레이어 정보. 좌석 0·점수 0 유실 방지를 위해 int 필드에
// omitempty 를 쓰지 않는다. scores 는 13칸 전부 실린다 (-1 = 미기록).
type YTPlayerView struct {
	Seat      int            `json:"seat"`
	Name      string         `json:"name"`
	Connected bool           `json:"connected"`
	Bot       bool           `json:"bot"`
	Total     int            `json:"total"`
	UpperSum  int            `json:"upperSum"`
	Bonus     int            `json:"bonus"`
	Scores    map[string]int `json:"scores"`
}

// YTGameStatePayload 전체 게임 스냅샷. 모든 상태 변경 후 좌석마다 만들어
// 보낸다 (개인화는 yourSeat 뿐 — 주사위는 전원 공개라 관전 자유).
type YTGameStatePayload struct {
	GameID   string  `json:"gameId"`
	RoomCode string  `json:"roomCode"`
	Phase    YTPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"` // 관전자는 -1
	// Spectators 관전자 수 (참가자·관전자 모두 표시용)
	Spectators int `json:"spectators"`
	// EndsAt 현재 턴의 AFK 자동 진행 마감 시각 (unixMillis, playing 외 0)
	EndsAt      int64          `json:"endsAt"`
	CurrentSeat int            `json:"currentSeat"`
	Round       int            `json:"round"`
	Dice        []int          `json:"dice"`      // 항상 5개
	Held        []bool         `json:"held"`      // 항상 5개
	RollsLeft   int            `json:"rollsLeft"` // 0~3
	Players     []YTPlayerView `json:"players"`
	LastAction  *YTLastAction  `json:"lastAction"`  // 직전 기록, 없으면 null
	WinnerSeats []int          `json:"winnerSeats"` // 종료 전 []
}

// YTEventPayload 연출용 이벤트 — 전원에게 동일하게 간다.
// Seat 은 좌석 0 유실 방지를 위해 포인터로 생략을 표현한다.
type YTEventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

// YTGameOverPayload 게임 종료 발표 (동점 공동 승 지원)
type YTGameOverPayload struct {
	WinnerSeats []int          `json:"winnerSeats"`
	WinnerNames []string       `json:"winnerNames"`
	Totals      []int          `json:"totals"` // 좌석 순 최종 총점
	Players     []YTPlayerView `json:"players"`
}

type YTPlayerDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type YTPlayerReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type YTErrorPayload struct {
	Message string `json:"message"`
}
