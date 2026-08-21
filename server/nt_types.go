package server

import "time"

// ==================== 노 땡스! 타입 ====================
//
// 3~7인 푸시 유어 럭 소품. 3~35 카드 33장 중 9장을 비공개로 제거한 24장을
// 쓰고, 차례마다 칩을 얹고 패스하거나 카드+얹힌 칩을 가져간다. 핵심 은닉:
// 칩 수는 본인만 정확히 알고 타인 chips 는 스냅샷에서 -1 이다 (관전자도
// 전원 -1). 게임 종료(쇼다운)에만 전원 공개 + 점수 확정. 획득 카드는 공개.

// NTMinPlayers / NTMaxPlayers 인원 범위 (3~7인)
const (
	NTMinPlayers = 3
	NTMaxPlayers = 7

	// NTFillBotTarget nt_fill_bots 가 채우는 목표 인원 — 채운 뒤 즉시 시작
	NTFillBotTarget = 5

	NTStartChips = 11 // 시작 칩 (본인만 실값을 본다)
	NTCardMin    = 3  // 카드 최솟값
	NTCardMax    = 35 // 카드 최댓값 (3~35 = 33장)
	NTDeckSize   = 24 // 33장 중 무작위 9장 비공개 제거 후 사용 장수
)

// NTPhase 게임 진행 단계
type NTPhase string

const (
	NTPhaseWaiting  NTPhase = "waiting"
	NTPhasePlaying  NTPhase = "playing"
	NTPhaseGameOver NTPhase = "game_over"
)

// NTMessageType 노 땡스! 메시지 타입
type NTMessageType string

const (
	// 클라이언트 → 서버
	NTMsgJoinGame NTMessageType = "nt_join_game"
	NTMsgFillBots NTMessageType = "nt_fill_bots"
	NTMsgStart    NTMessageType = "nt_start"
	NTMsgRejoin   NTMessageType = "nt_rejoin"
	NTMsgPass     NTMessageType = "nt_pass"
	NTMsgTake     NTMessageType = "nt_take"
	NTMsgReact    NTMessageType = "nt_react"

	// 서버 → 클라이언트
	NTMsgPlayerJoined       NTMessageType = "nt_player_joined"
	NTMsgSpectateJoined     NTMessageType = "nt_spectate_joined"
	NTMsgGameState          NTMessageType = "nt_game_state"
	NTMsgEvent              NTMessageType = "nt_event"
	NTMsgGameOver           NTMessageType = "nt_game_over"
	NTMsgPlayerDisconnected NTMessageType = "nt_player_disconnected"
	NTMsgPlayerReconnected  NTMessageType = "nt_player_reconnected"
	NTMsgSessionExpired     NTMessageType = "nt_session_expired"
	NTMsgError              NTMessageType = "nt_error"
)

// NTPlayer 게임 참가자 (순수 상태 — 연결 매핑은 허브의 ntRoom 담당)
type NTPlayer struct {
	Seat  int
	Name  string
	Chips int   // 보유 칩 (스냅샷에서 본인 외 -1 은닉)
	Cards []int // 획득 카드 (오름차순 유지 — 공개 정보)
	Score int   // 게임 종료 시 확정 (시퀀스 합 − 칩), 그 전 0
}

// NTGame 노 땡스! 게임 상태 (순수, 허브 비의존)
type NTGame struct {
	ID      string
	Players []*NTPlayer
	Phase   NTPhase

	Deck        []int // 남은 비공개 덱 (현재 공개 카드 제외)
	Card        int   // 현재 공개 카드 (0 = 없음)
	PotChips    int   // 공개 카드 위에 얹힌 칩
	CurrentSeat int   // 행동 차례 (-1 = 없음)
	FirstSeat   int   // 시작 선 (무작위)

	WinnerSeats []int  // 최저점 승자 (동점 공동)
	EndReason   string // "deck_empty"
	Ready       bool
	StartedAt   time.Time
}

// NTClient 노 땡스! 클라이언트 연결
type NTClient struct {
	wsClient
	Hub  *NTHub
	Seat int
}

// NTMessage 메시지 봉투
type NTMessage struct {
	Type    NTMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type NTJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type NTRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// NTReactPayload 리액션 이모지 (화이트리스트 외는 조용히 무시)
type NTReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

// NTPlayerView 수신자별 플레이어 정보. 좌석 0·점수 0 유실 방지를 위해
// int 필드에 omitempty 를 쓰지 않는다. 은닉의 핵심 — chips 는 본인 좌석만
// 실값, 타인은 -1 (관전자는 전원 -1). game_over 에만 전원 실값 + score.
type NTPlayerView struct {
	Seat      int    `json:"seat"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Bot       bool   `json:"bot"`
	Chips     int    `json:"chips"` // -1 = 은닉
	Cards     []int  `json:"cards"` // 획득 카드 오름차순 (공개, 빈 배열 [])
	Score     int    `json:"score"` // game_over 에만 확정, 그 외 0
}

// NTGameStatePayload 개인화된 전체 게임 스냅샷. 모든 상태 변경 후 좌석마다
// 따로 만들어 보낸다. 재접속 복원도 같은 페이로드를 쓴다.
type NTGameStatePayload struct {
	GameID   string  `json:"gameId"`
	RoomCode string  `json:"roomCode"`
	Phase    NTPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"` // 관전자는 -1
	// Spectators 관전자 수 (참가자·관전자 모두 표시용)
	Spectators int `json:"spectators"`
	// EndsAt 현재 차례의 AFK 자동 진행 마감 시각 (unixMillis, playing 외 0)
	EndsAt      int64          `json:"endsAt"`
	CurrentSeat int            `json:"currentSeat"`
	DeckCount   int            `json:"deckCount"` // 남은 비공개 덱 장수
	Card        int            `json:"card"`      // 현재 공개 카드 (0 = 없음)
	PotChips    int            `json:"potChips"`  // 공개 카드 위에 얹힌 칩
	Players     []NTPlayerView `json:"players"`
}

// NTEventPayload 연출용 이벤트. 비밀 정보를 담지 않으며 전원에게 동일하게
// 간다. Seat 은 좌석 0 유실 방지를 위해 포인터로 생략을 표현한다.
type NTEventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

// NTGameOverPayload 게임 종료 발표 (최저점 승리, 동점 공동 승)
type NTGameOverPayload struct {
	WinnerSeats []int          `json:"winnerSeats"`
	WinnerNames []string       `json:"winnerNames"`
	Scores      []int          `json:"scores"` // 좌석 순 최종 점수
	Players     []NTPlayerView `json:"players"`
}

type NTPlayerDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type NTPlayerReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type NTErrorPayload struct {
	Message string `json:"message"`
}
