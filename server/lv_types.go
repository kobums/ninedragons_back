package server

import "time"

// ==================== 러브레터 타입 ====================

// LVMinPlayers / LVMaxPlayers 인원 범위 (2~4인)
const (
	LVMinPlayers = 2
	LVMaxPlayers = 4
)

// 카드 값 상수 — 값이 곧 와이어 표현이다 (1~8)
const (
	LVGuard    = 1 // 경비병 ×5 — 대상+값 추측(1 제외), 맞으면 대상 탈락
	LVPriest   = 2 // 사제 ×2 — 대상의 손패를 나만 본다
	LVBaron    = 3 // 남작 ×2 — 손패 비교, 낮은 쪽 탈락 (동점 무효)
	LVHandmaid = 4 // 시녀 ×2 — 다음 내 턴까지 보호
	LVPrince   = 5 // 왕자 ×2 — 대상(자신 가능)이 손패를 버리고 새로 뽑음
	LVKing     = 6 // 왕 ×1 — 대상과 손패 교환
	LVCountess = 7 // 백작부인 ×1 — 왕/왕자와 함께 있으면 강제
	LVPrincess = 8 // 공주 ×1 — 버리면 즉시 탈락
)

// LVPhase 게임 진행 단계
type LVPhase string

const (
	LVPhaseWaiting  LVPhase = "waiting"
	LVPhasePlaying  LVPhase = "playing"
	LVPhaseRoundEnd LVPhase = "round_end"
	LVPhaseGameOver LVPhase = "game_over"
)

// LVMessageType 러브레터 메시지 타입
type LVMessageType string

const (
	// 클라이언트 → 서버
	LVMsgJoinGame LVMessageType = "lv_join_game"
	LVMsgFillBots LVMessageType = "lv_fill_bots"
	LVMsgStart    LVMessageType = "lv_start"
	LVMsgRejoin   LVMessageType = "lv_rejoin"
	LVMsgPlay     LVMessageType = "lv_play"
	LVMsgReact    LVMessageType = "lv_react"

	// 서버 → 클라이언트
	LVMsgPlayerJoined       LVMessageType = "lv_player_joined"
	LVMsgSpectateJoined     LVMessageType = "lv_spectate_joined"
	LVMsgGameState          LVMessageType = "lv_game_state"
	LVMsgEvent              LVMessageType = "lv_event"
	LVMsgPrivate            LVMessageType = "lv_private"
	LVMsgGameOver           LVMessageType = "lv_game_over"
	LVMsgPlayerDisconnected LVMessageType = "lv_player_disconnected"
	LVMsgPlayerReconnected  LVMessageType = "lv_player_reconnected"
	LVMsgSessionExpired     LVMessageType = "lv_session_expired"
	LVMsgError              LVMessageType = "lv_error"
)

// LVPlayer 게임 참가자 (순수 상태 — 연결 매핑은 허브의 lvRoom 담당)
type LVPlayer struct {
	Seat      int
	Name      string
	Hand      []int // 손패 (보통 1장, 자기 턴에 2장)
	Discards  []int // 버린 카드 더미 — 전원 공개 (카운팅 요소)
	Alive     bool  // 이번 라운드 생존 여부
	Protected bool  // 시녀 보호 중 (다음 내 턴 시작까지)
	Tokens    int   // 획득 토큰 (라운드 승수)
}

// LVRoundResult 라운드 결과 (round_end 동안 스냅샷에 노출)
type LVRoundResult struct {
	WinnerSeat int    `json:"winnerSeat"`
	Reason     string `json:"reason"` // "last_standing" | "highest_card"
}

// LVGame 러브레터 게임 상태 (순수, 허브 비의존)
type LVGame struct {
	ID      string
	Players []*LVPlayer
	Phase   LVPhase

	Deck []int
	// Removed 라운드 시작 시 비공개로 제거한 1장. 덱 소진 후 왕자 효과의
	// 마지막 뽑기 공급원이다 (원작 규칙).
	Removed      int
	RemovedTaken bool
	// RemovedFaceUp 2인전에서 공개로 제거한 3장 (전원 공개)
	RemovedFaceUp []int

	CurrentSeat  int
	TargetTokens int // 2인 7 / 3인 5 / 4인 4
	RoundNo      int
	RoundResult  *LVRoundResult // round_end~game_over 동안, 다음 라운드에 nil
	WinnerSeat   int            // 게임 승자 (-1 = 미정)
	Ready        bool
	StartedAt    time.Time
}

// LVClient 러브레터 클라이언트 연결
type LVClient struct {
	wsClient
	Hub  *LVHub
	Seat int
}

// LVMessage 메시지 봉투
type LVMessage struct {
	Type    LVMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type LVJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type LVRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// LVPlayPayload 카드 플레이. TargetSeat 은 좌석 0 과 "미지정"을 구분해야
// 하므로 포인터다. Guess 는 경비병(1) 전용 (2~8).
type LVPlayPayload struct {
	Card       int  `json:"card"`
	TargetSeat *int `json:"targetSeat,omitempty"`
	Guess      int  `json:"guess,omitempty"`
}

// LVReactPayload 리액션 이모지 (화이트리스트 외는 조용히 무시)
type LVReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

// LVPlayerView 수신자 무관 공개 플레이어 정보. 좌석 0 유실 방지를 위해
// int 필드에 omitempty 를 쓰지 않는다. 은닉은 yourHand 뿐 — discards 는
// 전원 공개다.
type LVPlayerView struct {
	Seat      int    `json:"seat"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Bot       bool   `json:"bot"`
	Alive     bool   `json:"alive"`
	Protected bool   `json:"protected"`
	Tokens    int    `json:"tokens"`
	Discards  []int  `json:"discards"`
}

// LVGameStatePayload 개인화된 전체 게임 스냅샷. 모든 상태 변경 후 좌석마다
// 따로 만들어 보낸다. 재접속 복원도 같은 페이로드를 쓴다.
type LVGameStatePayload struct {
	GameID   string  `json:"gameId"`
	RoomCode string  `json:"roomCode"`
	Phase    LVPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"` // 관전자는 -1
	// Spectators 관전자 수 (참가자·관전자 모두 표시용)
	Spectators int `json:"spectators"`
	// EndsAt 현재 턴의 AFK 자동 진행 마감 시각 (unixMillis, playing 외 0)
	EndsAt int64 `json:"endsAt"`
	// YourHand 내 손패 (1~2장). 관전자·타인은 필드 자체가 부재해야 하므로
	// 포인터 — 좌석 보유자는 빈 배열도 [] 로 나간다 (nil 금지).
	YourHand      *[]int         `json:"yourHand,omitempty"`
	CurrentSeat   int            `json:"currentSeat"`
	DeckCount     int            `json:"deckCount"`
	Tokens        []int          `json:"tokens"` // 좌석 순 토큰 현황
	TargetTokens  int            `json:"targetTokens"`
	Players       []LVPlayerView `json:"players"`
	RoundResult   *LVRoundResult `json:"roundResult"` // round_end 동안, 그 외 null
	RemovedFaceUp []int          `json:"removedFaceUp"`
}

// LVEventPayload 연출용 이벤트. 비밀 정보를 담지 않으며 전원에게 동일하게
// 간다. Seat/TargetSeat 은 좌석 0 유실 방지를 위해 포인터로 생략을 표현한다.
type LVEventPayload struct {
	Kind       string `json:"kind"`
	Seat       *int   `json:"seat,omitempty"`
	Name       string `json:"name,omitempty"`
	TargetSeat *int   `json:"targetSeat,omitempty"`
	Message    string `json:"message"`
}

// LVPrivatePayload 당사자에게만 가는 비공개 결과 (사제 열람·남작 비교)
type LVPrivatePayload struct {
	Kind    string `json:"kind"` // "priest" | "baron"
	Message string `json:"message"`
}

// LVGameOverPayload 게임 종료 발표
type LVGameOverPayload struct {
	WinnerSeat int            `json:"winnerSeat"`
	WinnerName string         `json:"winnerName"`
	Tokens     []int          `json:"tokens"`
	Players    []LVPlayerView `json:"players"`
}

type LVPlayerDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type LVPlayerReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type LVErrorPayload struct {
	Message string `json:"message"`
}
