package server

import "time"

// ==================== 저스트 원 (Just One) 타입 ====================
//
// 3~7인 협력 단어 추리. 매 라운드 한 명이 출제자가 되고, 나머지 전원이
// 제시어를 보며 단서 한 단어씩을 비공개로 낸다. 겹친 단서는 전부 지워지므로
// "남들과 다르면서도 통하는 단어"를 골라야 한다는 게 이 게임의 전부다.
//
// 전원이 한 팀이라 승패를 함께 맞는다 — 총점이 라운드 수의 절반 이상이면
// 성공이고, 그 미만이면 실패다 (jo_hub.go 의 finishGame 이 그대로 기록한다).
//
// 은닉은 jo_hub.go 의 buildJOState 하나에 모여 있다.
//   - word     : 단서 제공자에게만. 출제자·관전자의 raw JSON 에는 키 자체가 없다.
//   - yourClue : 본인에게만. 남의 단서는 어떤 경로로도 새지 않는다.
//   - clues    : 단서 단계에는 항상 빈 배열이고, 추리 단계부터 공개된다.
//                소거된 단서는 판정이 끝난 뒤(round_end)에야 함께 보인다.
//
// 라운드가 끝난 제시어는 history 로 전원에게 공개된다 — 그래서 word 필드는
// 게임 내내 "출제자에게는 없는 키"라는 불변식을 지킬 수 있다.

const (
	JOMinPlayers = 3
	JOMaxPlayers = 7

	// JOFillBotTarget jo_fill_bots 가 채우는 목표 인원 — 채운 뒤 즉시 시작
	JOFillBotTarget = 4

	// JORoundsPerPlayer 1인당 출제 횟수 — 총 라운드 = 인원 × 2 (좌석 순 순환)
	JORoundsPerPlayer = 2

	// JOMaxClueLen 단서·답의 길이 상한 (문자 수). 띄어쓰기 없는 한 단어 권장
	JOMaxClueLen = 12
)

// JOPhase 게임 진행 단계
type JOPhase string

const (
	JOPhaseWaiting  JOPhase = "waiting"
	JOPhaseClue     JOPhase = "clue"      // 단서 비공개 제출 (60초)
	JOPhaseGuess    JOPhase = "guess"     // 출제자의 추리 (60초)
	JOPhaseJudging  JOPhase = "judging"   // 오답 인정 창 (15초)
	JOPhaseRoundEnd JOPhase = "round_end" // 라운드 정산 (5초 뒤 다음 라운드)
	JOPhaseGameOver JOPhase = "game_over"
)

// JOMessageType 저스트 원 메시지 타입
type JOMessageType string

const (
	// 클라이언트 → 서버
	JOMsgJoinGame JOMessageType = "jo_join_game"
	JOMsgFillBots JOMessageType = "jo_fill_bots"
	JOMsgStart    JOMessageType = "jo_start"
	JOMsgRejoin   JOMessageType = "jo_rejoin"
	JOMsgClue     JOMessageType = "jo_clue"
	JOMsgGuess    JOMessageType = "jo_guess"
	JOMsgPass     JOMessageType = "jo_pass"
	JOMsgAccept   JOMessageType = "jo_accept"
	JOMsgReact    JOMessageType = "jo_react"

	// 서버 → 클라이언트
	JOMsgPlayerJoined       JOMessageType = "jo_player_joined"
	JOMsgSpectateJoined     JOMessageType = "jo_spectate_joined"
	JOMsgGameState          JOMessageType = "jo_game_state"
	JOMsgEvent              JOMessageType = "jo_event"
	JOMsgGameOver           JOMessageType = "jo_game_over"
	JOMsgPlayerDisconnected JOMessageType = "jo_player_disconnected"
	JOMsgPlayerReconnected  JOMessageType = "jo_player_reconnected"
	JOMsgSessionExpired     JOMessageType = "jo_session_expired"
	JOMsgError              JOMessageType = "jo_error"
)

// JOClueView 단서 한 개. Removed 는 자동 소거 여부다.
// 좌석 0 유실을 막기 위해 omitempty 를 쓰지 않는다.
type JOClueView struct {
	Seat    int    `json:"seat"`
	Name    string `json:"name"`
	Text    string `json:"text"`
	Removed bool   `json:"removed"`
}

// JOJudged 이번 라운드 판정 결과 (판정 전에는 null)
type JOJudged struct {
	Correct bool `json:"correct"`
	// Accepted 오답이었지만 누군가 [정답 인정]을 눌러 정답이 된 경우 true
	Accepted bool   `json:"accepted"`
	Message  string `json:"message"`
}

// JOHistoryEntry 끝난 라운드의 기록 — 제시어는 여기서 전원 공개된다
type JOHistoryEntry struct {
	Round   int    `json:"round"`
	Word    string `json:"word"`
	Guess   string `json:"guess"`
	Correct bool   `json:"correct"`
}

// JOPlayer 게임 참가자 (순수 상태 — 연결 매핑은 허브의 joRoom 담당)
type JOPlayer struct {
	Seat int
	Name string
	// Clue 이번 라운드에 낸 단서 (본인만 본다. 미제출은 "")
	Clue string
	// Submitted 제출 잠금 여부 — 한 라운드에 한 번만 낼 수 있다
	Submitted bool
}

// JOGameEvent 순수 규칙이 쌓는 연출 이벤트 — 허브가 DrainEvents 로 꺼내
// jo_event 로 방송한다. 단서 단계의 단서 본문은 절대 담지 않는다
// (제시어는 라운드가 끝난 뒤에만 담는다).
type JOGameEvent struct {
	Kind    string
	Seat    int // -1 없음
	Message string
}

// JOGame 저스트 원 게임 상태 (순수, 허브 비의존)
type JOGame struct {
	ID      string
	Players []*JOPlayer
	Phase   JOPhase

	// Round 현재 라운드 (1~TotalRounds, 시작 전 0)
	Round int
	// TotalRounds 인원 × JORoundsPerPlayer
	TotalRounds int
	// GuesserSeat 이번 라운드 출제자 (-1 시작 전·종료 후)
	GuesserSeat int
	// Score 협력 총점 (0 미만으로 내려가지 않는다)
	Score int

	// Word 이번 라운드 제시어 (단서 제공자만 스냅샷으로 받는다)
	Word string
	// Clues 소거 판정을 마친 이번 라운드 단서 (추리 단계부터 채워진다)
	Clues []JOClueView
	// Guess 출제자가 낸 답 (넘김·미제출은 "")
	Guess string
	// Judged 이번 라운드 판정 (판정 전 nil)
	Judged *JOJudged
	// History 끝난 라운드 기록 (제시어 공개 통로)
	History []JOHistoryEntry

	// words 라운드별 제시어를 시작 시 한 번에 뽑아 둔다
	words []string

	Ready     bool
	StartedAt time.Time

	// StateSeq 새 대기 상태(단계 전환)가 열릴 때마다 +1 — 허브가 마감
	// 타이머를 다시 걸지 판단하는 근거. 같은 단계에서 단서가 하나씩 들어와도
	// StateSeq 는 오르지 않으므로 마감이 늘어나지 않는다.
	StateSeq int
	// AfkSeq 마감 타이머 일련번호 (뒤늦은 발화 무시용 — 허브가 관리)
	AfkSeq int
	// Deadline 현재 대기 상태의 마감 시각 (unixMillis — 스냅샷 노출용)
	Deadline int64

	events []JOGameEvent
}

// JOClient 저스트 원 클라이언트 연결
type JOClient struct {
	wsClient
	Hub  *JOHub
	Seat int
}

// JOMessage 메시지 봉투
type JOMessage struct {
	Type    JOMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type JOJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type JORejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// JOCluePayload 단서 제출 — 띄어쓰기 없는 한 단어 권장, 서버가 12자로 제한
type JOCluePayload struct {
	Text string `json:"text"`
}

// JOGuessPayload 출제자의 답
type JOGuessPayload struct {
	Text string `json:"text"`
}

type JOReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

// JOPlayerView 좌석별 공개 정보 — 좌석 0 유실 방지를 위해 omitempty 금지.
// Submitted 는 "냈다/안 냈다"만 알려 준다 (단서 본문은 절대 실리지 않는다).
type JOPlayerView struct {
	Seat      int    `json:"seat"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Bot       bool   `json:"bot"`
	Submitted bool   `json:"submitted"`
	IsGuesser bool   `json:"isGuesser"`
}

// JOGameStatePayload 개인화된 전체 게임 스냅샷. 모든 상태 변경 후 좌석마다
// 따로 만들어 보낸다. 재접속 복원도 같은 페이로드를 쓴다.
//
// 은닉:
//   - Word 는 단서 제공자에게만 — 출제자·관전자의 raw JSON 에는 키 자체가
//     없다 (nil 포인터 생략). 라운드가 끝난 제시어는 History 로 공개된다.
//   - YourClue 는 본인에게만 — 미제출이면 "" 다 (빈 문자열도 실려야 하므로
//     문자열 포인터로 부재를 표현한다).
//   - Clues 는 단서 단계에 항상 빈 배열이다.
type JOGameStatePayload struct {
	GameID   string  `json:"gameId"`
	RoomCode string  `json:"roomCode"`
	Phase    JOPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"` // 관전자는 -1
	// Spectators 관전자 수 (참가자·관전자 모두 표시용)
	Spectators int `json:"spectators"`
	// EndsAt 현재 대기 상태의 마감 시각 (unixMillis, 그 외 0)
	EndsAt      int64 `json:"endsAt"`
	Round       int   `json:"round"`
	TotalRounds int   `json:"totalRounds"`
	GuesserSeat int   `json:"guesserSeat"`
	Score       int   `json:"score"`
	// Word 이번 라운드 제시어 — 단서 제공자만 (출제자·관전자는 키 부재)
	Word *string `json:"word,omitempty"`
	// YourClue 본인이 낸 단서 — 본인만 (미제출 "")
	YourClue *string `json:"yourClue,omitempty"`
	// Clues 단서 목록 — 항상 [] (nil → JSON null 금지).
	// 단서 단계에는 비어 있고, 추리·인정 단계에는 살아남은 단서만,
	// 판정이 끝난 뒤(round_end)에는 소거된 단서까지 함께 담긴다.
	Clues []JOClueView `json:"clues"`
	// SubmittedCount 단서를 낸 사람 수 (출제자 제외)
	SubmittedCount int `json:"submittedCount"`
	// Guess 출제자가 낸 답 (넘김·미제출 "")
	Guess string `json:"guess"`
	// Judged 이번 라운드 판정 (판정 전 null)
	Judged  *JOJudged      `json:"judged"`
	Players []JOPlayerView `json:"players"`
	// History 끝난 라운드 기록 — 항상 [] (제시어 공개 통로)
	History []JOHistoryEntry `json:"history"`
}

// JOEventPayload 연출용 이벤트. 미공개 단서를 담지 않으며 전원에게 동일하게
// 간다. Seat 은 좌석 0 유실 방지를 위해 포인터로 생략을 표현한다.
type JOEventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

// JOGameOverPayload 게임 종료 발표 — 협력 결과라 전원이 같은 결말을 받는다
type JOGameOverPayload struct {
	// Cleared 총점이 라운드 수의 절반 이상이면 성공
	Cleared     bool   `json:"cleared"`
	Score       int    `json:"score"`
	TotalRounds int    `json:"totalRounds"`
	Grade       string `json:"grade"`
	Message     string `json:"message"`
	// History 라운드별 기록 — 항상 []
	History []JOHistoryEntry `json:"history"`
	Players []JOPlayerView   `json:"players"`
}

type JOPlayerDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type JOPlayerReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type JOErrorPayload struct {
	Message string `json:"message"`
}
