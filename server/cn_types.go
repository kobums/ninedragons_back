package server

import "time"

// ==================== 코드네임 타입 ====================
//
// 4~8인 팀전 단어 추리 진행 도우미. 힌트는 음성으로 말하고 앱에도 기록해
// 히스토리를 남긴다 — 앱은 보드·턴·판정만 맡는다. 키 카드(전체 색 배치)의
// 은닉이 이 게임의 전부라 개인화 스냅샷(buildCNState)이 유일한 정보 통로다.

const (
	CNMinPlayers = 4 // 시작 최소 인원 (팀당 2)
	CNMaxPlayers = 8 // 좌석 수 상한

	// CNBotFillTarget 봇 채우기 상한. 봇은 연습용이라 6인까지만 채운다.
	CNBotFillTarget = 6

	// CNBoardSize 5×5 보드의 단어 수
	CNBoardSize = 25

	// 키 카드 구성 — 적 9(선공 고정), 청 8, 중립 7, 암살자 1
	CNRedWords     = 9
	CNBlueWords    = 8
	CNNeutralWords = 7
)

// CNTeam 팀 식별자 — 값이 곧 와이어 표현이다
type CNTeam string

const (
	CNTeamRed  CNTeam = "red"
	CNTeamBlue CNTeam = "blue"
)

// CNColor 카드 색 (키 카드·공개된 보드 카드)
type CNColor string

const (
	CNColorRed      CNColor = "red"
	CNColorBlue     CNColor = "blue"
	CNColorNeutral  CNColor = "neutral"
	CNColorAssassin CNColor = "assassin"
)

// CNRole 좌석 역할
type CNRole string

const (
	CNRoleSpymaster CNRole = "spymaster"
	CNRoleAgent     CNRole = "agent"
)

// CNPhase 게임 진행 단계
type CNPhase string

const (
	CNPhaseWaiting  CNPhase = "waiting"
	CNPhaseClue     CNPhase = "clue"  // 현재 팀 스파이마스터의 힌트 기록 대기
	CNPhaseGuess    CNPhase = "guess" // 현재 팀 요원들의 카드 선택
	CNPhaseGameOver CNPhase = "game_over"
)

// CNMessageType 코드네임 메시지 타입
type CNMessageType string

const (
	// 클라이언트 → 서버
	CNMsgJoinGame CNMessageType = "cn_join_game"
	CNMsgFillBots CNMessageType = "cn_fill_bots"
	CNMsgStart    CNMessageType = "cn_start"
	CNMsgRejoin   CNMessageType = "cn_rejoin"
	CNMsgClue     CNMessageType = "cn_clue"
	CNMsgPick     CNMessageType = "cn_pick"
	CNMsgEndTurn  CNMessageType = "cn_end_turn"
	CNMsgReact    CNMessageType = "cn_react"

	// 서버 → 클라이언트
	CNMsgPlayerJoined       CNMessageType = "cn_player_joined"
	CNMsgSpectateJoined     CNMessageType = "cn_spectate_joined"
	CNMsgGameState          CNMessageType = "cn_game_state"
	CNMsgEvent              CNMessageType = "cn_event"
	CNMsgGameOver           CNMessageType = "cn_game_over"
	CNMsgPlayerDisconnected CNMessageType = "cn_player_disconnected"
	CNMsgPlayerReconnected  CNMessageType = "cn_player_reconnected"
	CNMsgSessionExpired     CNMessageType = "cn_session_expired"
	CNMsgError              CNMessageType = "cn_error"
)

// CNPlayer 게임 참가자 (순수 상태 — 연결 매핑은 허브의 cnRoom 담당).
// Team 은 입장 순 번갈아(좌석 짝수=적, 홀수=청) 배정되고, Role 은 대기 중
// 미리보기로 갱신되다가 시작 시 확정된다.
type CNPlayer struct {
	Seat  int
	Name  string
	Team  CNTeam
	Role  CNRole
	IsBot bool // 서버 연습봇 좌석 (스파이마스터 사람 우선 규칙 판단용)
}

// CNCard 보드 카드 하나 (순수 상태 — 색은 KeyCard 가 든다)
type CNCard struct {
	Word     string
	Revealed bool
}

// CNClue 현재 턴의 힌트. Remaining 은 남은 선택 횟수(숫자+1에서 시작).
type CNClue struct {
	Word      string
	Count     int
	Remaining int
}

// CNClueEntry 힌트 히스토리 한 건 (전원 공개)
type CNClueEntry struct {
	Team  CNTeam `json:"team"`
	Word  string `json:"word"`
	Count int    `json:"count"`
}

// CNGame 코드네임 게임 상태 (순수, 허브 비의존)
type CNGame struct {
	ID      string
	Players []*CNPlayer
	Phase   CNPhase

	// Board 25장 단어 카드. KeyCard 는 각 칸의 실제 색 — 절대 스냅샷으로
	// 직접 내보내지 않는다 (스파이마스터에게만 keyCard 필드로 나간다).
	Board   []CNCard
	KeyCard []CNColor

	CurrentTeam CNTeam
	Clue        *CNClue // clue 단계 nil, guess 단계에 존재
	ClueHistory []CNClueEntry

	RedLeft  int // 적팀의 미공개 팀 단어 수
	BlueLeft int

	Winner     CNTeam // 게임 승자 ("" = 미정)
	LoseReason string // "assassin" | ""

	Ready     bool
	StartedAt time.Time
}

// CNClient 코드네임 클라이언트 연결
type CNClient struct {
	wsClient
	Hub  *CNHub
	Seat int
}

// CNMessage 메시지 봉투
type CNMessage struct {
	Type    CNMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type CNJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type CNRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// CNCluePayload 스파이마스터의 힌트 기록 (단어+숫자)
type CNCluePayload struct {
	Word  string `json:"word"`
	Count int    `json:"count"`
}

// CNPickPayload 요원의 카드 선택 (0~24)
type CNPickPayload struct {
	Index int `json:"index"`
}

// CNReactPayload 리액션 이모지 (화이트리스트 외는 조용히 무시)
type CNReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

// CNPlayerView 수신자 무관 공개 플레이어 정보. 좌석 0 유실 방지를 위해
// int 필드에 omitempty 를 쓰지 않는다. 역할·팀은 공개 정보다 — 은닉은
// keyCard 뿐이다.
type CNPlayerView struct {
	Seat      int    `json:"seat"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Bot       bool   `json:"bot"`
	Team      CNTeam `json:"team"`
	Role      CNRole `json:"role"`
}

// CNCardView 보드 카드 공개 뷰 — 미공개 카드의 color 는 반드시 빈 값이다
type CNCardView struct {
	Word     string  `json:"word"`
	Revealed bool    `json:"revealed"`
	Color    CNColor `json:"color"` // "" 미공개, 공개 시 실제 색
}

// CNClueView 현재 힌트 (guess 단계에만 존재, 그 외 null)
type CNClueView struct {
	Word      string `json:"word"`
	Count     int    `json:"count"`
	Remaining int    `json:"remaining"`
}

// CNGameStatePayload 개인화된 전체 게임 스냅샷. 모든 상태 변경 후 좌석마다
// 따로 만들어 보낸다. 재접속 복원도 같은 페이로드를 쓴다.
// 은닉의 핵심: keyCard 는 스파이마스터에게만 실린다 (요원·관전자는 필드
// 자체가 부재 — omitempty + nil).
type CNGameStatePayload struct {
	GameID   string  `json:"gameId"`
	RoomCode string  `json:"roomCode"`
	Phase    CNPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"` // 관전자는 -1
	// Spectators 관전자 수 (참가자·관전자 모두 표시용)
	Spectators int `json:"spectators"`
	// EndsAt 현재 단계의 AFK 자동 진행 마감 시각 (unixMillis, clue/guess 외 0)
	EndsAt      int64  `json:"endsAt"`
	CurrentTeam CNTeam `json:"currentTeam"`
	YourTeam    CNTeam `json:"yourTeam"` // 관전자는 ""
	YourRole    CNRole `json:"yourRole"` // 관전자·시작 전은 미리보기 값 그대로
	// KeyCard 전체 색 배치 25칸 — 스파이마스터에게만 (그 외 필드 부재)
	KeyCard     []CNColor      `json:"keyCard,omitempty"`
	Board       []CNCardView   `json:"board"` // waiting 은 [] (nil 금지)
	Clue        *CNClueView    `json:"clue"`  // guess 단계 외 null
	ClueHistory []CNClueEntry  `json:"clueHistory"`
	RedLeft     int            `json:"redLeft"`
	BlueLeft    int            `json:"blueLeft"`
	Players     []CNPlayerView `json:"players"`
	Winner      CNTeam         `json:"winner"`     // "" = 미정
	LoseReason  string         `json:"loseReason"` // "assassin" | ""
}

// CNEventPayload 연출용 이벤트. 비밀 정보를 담지 않으며 전원에게 동일하게
// 간다. kind: joined|left|game_started|clue|pick|turn_end|afk|react|
// bot_takeover|game_over
type CNEventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

// CNGameOverPayload 게임 종료 발표 — 승리 팀·사유 공개
type CNGameOverPayload struct {
	Winner     CNTeam         `json:"winner"`
	LoseReason string         `json:"loseReason"`
	RedLeft    int            `json:"redLeft"`
	BlueLeft   int            `json:"blueLeft"`
	Players    []CNPlayerView `json:"players"`
}

type CNPlayerDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type CNPlayerReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type CNErrorPayload struct {
	Message string `json:"message"`
}
