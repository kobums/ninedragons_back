package server

import "time"

// ==================== 차오차오 (Ciao Ciao) 타입 ====================
//
// 2~4인 주사위 블러핑 다리 건너기. 각자 말 3개로 7칸 다리를 건넌다.
// 차례마다 주사위(1,2,3,4,X,X)를 컵 속에서 굴려 본인만 보고(은닉: yourRoll)
// 1~4 를 선언한다 — X 가 나왔으면 반드시 거짓 선언(X 선언 불가). 선언 직후
// 생존자 전원의 의심 창(doubt_window)이 열리고, 통과·의심 판정 뒤에야 실제
// 값이 공개된다. 이 은닉·선언·판정 상태기계(cc_game.go)와 창 마감 타이머
// (cc_hub.go)가 계약의 심장이다.

const (
	CCMinPlayers = 2
	CCMaxPlayers = 4

	// CCFillBotTarget cc_fill_bots 가 채우는 목표 인원 — 채운 뒤 즉시 시작
	CCFillBotTarget = 4

	CCPawns      = 3 // 1인당 말 수
	CCBridgeLen  = 7 // 다리 칸 수 — 이 칸을 넘어서면 통과
	CCWinCrossed = 2 // 먼저 이만큼 통과시키면 승리

	// CCRollX 주사위의 X 면 (와이어 값 — yourRoll/actual 의 0)
	CCRollX = 0
)

// ccDieName 주사위 값 표기 (이벤트 문구용 — 0 은 X)
func ccDieName(v int) string {
	if v == CCRollX {
		return "X"
	}
	return string(rune('0' + v))
}

// CCPhase 게임 진행 단계
type CCPhase string

const (
	CCPhaseWaiting     CCPhase = "waiting"
	CCPhaseRolling     CCPhase = "rolling"      // 현재 차례가 컵 속 주사위를 보고 선언 준비 중
	CCPhaseDoubtWindow CCPhase = "doubt_window" // 선언 직후 — 생존자들의 의심/허용 창
	CCPhaseGameOver    CCPhase = "game_over"
)

// CCMessageType 차오차오 메시지 타입
type CCMessageType string

const (
	// 클라이언트 → 서버
	CCMsgJoinGame CCMessageType = "cc_join_game"
	CCMsgFillBots CCMessageType = "cc_fill_bots"
	CCMsgStart    CCMessageType = "cc_start"
	CCMsgRejoin   CCMessageType = "cc_rejoin"
	CCMsgDeclare  CCMessageType = "cc_declare"
	CCMsgDoubt    CCMessageType = "cc_doubt"
	CCMsgAllow    CCMessageType = "cc_allow"
	CCMsgReact    CCMessageType = "cc_react"

	// 서버 → 클라이언트
	CCMsgPlayerJoined       CCMessageType = "cc_player_joined"
	CCMsgSpectateJoined     CCMessageType = "cc_spectate_joined"
	CCMsgGameState          CCMessageType = "cc_game_state"
	CCMsgEvent              CCMessageType = "cc_event"
	CCMsgGameOver           CCMessageType = "cc_game_over"
	CCMsgPlayerDisconnected CCMessageType = "cc_player_disconnected"
	CCMsgPlayerReconnected  CCMessageType = "cc_player_reconnected"
	CCMsgSessionExpired     CCMessageType = "cc_session_expired"
	CCMsgError              CCMessageType = "cc_error"
)

// CCPlayer 게임 참가자 (순수 상태 — 연결 매핑은 허브의 ccRoom 담당)
type CCPlayer struct {
	Seat    int
	Name    string
	Reserve int   // 아직 다리에 오르지 않은 대기 말
	Bridge  []int // 다리 위 말들의 칸 위치 (1~CCBridgeLen, 오름차순 유지)
	Crossed int   // 다리를 건넌 말 수 (득점)
}

// Alive 움직일 말이 남았는지 (대기 + 다리 위 전부 잃으면 탈락)
func (p *CCPlayer) Alive() bool {
	return p.Reserve+len(p.Bridge) > 0
}

// CCGameEvent 순수 규칙이 쌓는 연출 이벤트 — 허브가 DrainEvents 로 꺼내
// cc_event 로 방송한다 (판정 전의 실제 주사위 값은 절대 담지 않는다)
type CCGameEvent struct {
	Kind    string
	Seat    int // -1 없음
	Message string
}

// CCReveal 직전 판정의 공개 기록 — 스냅샷 lastReveal 로 나간다.
// Actual 은 의심 판정에서만 실제 값(0=X)이고, 통과(pass)는 주사위가 끝까지
// 비밀이므로 -1(미공개)이다.
type CCReveal struct {
	Seat        int    `json:"seat"`        // 선언자 좌석
	Declared    int    `json:"declared"`    // 선언 값 (1~4)
	Actual      int    `json:"actual"`      // 실제 값 (0=X, 통과는 -1 미공개)
	DoubterSeat int    `json:"doubterSeat"` // 의심자 좌석 (-1 통과)
	Result      string `json:"result"`      // "pass" | "doubt_success" | "doubt_fail"
	Message     string `json:"message"`
}

// CCGame 차오차오 게임 상태 (순수, 허브 비의존)
type CCGame struct {
	ID      string
	Players []*CCPlayer
	Phase   CCPhase

	CurrentSeat int // 차례 좌석 (-1 없음)
	// Roll 현재 차례의 주사위 값 (1~4, 0=X) — rolling·doubt_window 동안
	// 유지되며 스냅샷에는 본인 차례 rolling 에만 yourRoll 로 실린다
	Roll     int
	Declared int       // 선언 값 (doubt_window 동안, 그 외 0)
	Reveal   *CCReveal // 직전 판정 기록 (스냅샷 lastReveal)

	// responders 의심 창에서 응답해야 하는 좌석 (창이 열릴 때의 생존자 기준)
	responders map[int]bool
	// allowed 허용을 누른 좌석 — 전원 허용이면 창 통과
	allowed map[int]bool

	WinnerSeat int    // -1 미정
	EndReason  string // "two_crossed" | "most_crossed"
	Ready      bool
	StartedAt  time.Time

	// StateSeq 응답 대기 상태(rolling·doubt_window)가 새로 열릴 때마다 +1 —
	// 허브가 마감 타이머를 다시 걸지 판단하는 근거
	StateSeq int
	// AfkSeq 마감 타이머 일련번호 (뒤늦은 발화 무시용 — 허브가 관리)
	AfkSeq int
	// Deadline 현재 대기 상태의 마감 시각 (unixMillis — 스냅샷 노출용)
	Deadline int64

	events []CCGameEvent
}

// CCClient 차오차오 클라이언트 연결
type CCClient struct {
	wsClient
	Hub  *CCHub
	Seat int
}

// CCMessage 메시지 봉투
type CCMessage struct {
	Type    CCMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type CCJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type CCRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// CCDeclarePayload 선언 값 — 1~4 만 유효 (X 는 선언할 수 없다)
type CCDeclarePayload struct {
	Value int `json:"value"`
}

type CCReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

// CCPlayerView 좌석별 공개 정보 — 좌석 0·말 0 유실 방지를 위해 omitempty 금지
type CCPlayerView struct {
	Seat      int    `json:"seat"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Bot       bool   `json:"bot"`
	Alive     bool   `json:"alive"`
	PawnsLeft int    `json:"pawnsLeft"` // 아직 다리에 오르지 않은 대기 말
	OnBridge  []int  `json:"onBridge"`  // 다리 위 말 위치 (빈 배열 보장)
	Crossed   int    `json:"crossed"`
}

// CCGameStatePayload 개인화된 전체 게임 스냅샷. 모든 상태 변경 후 좌석마다
// 따로 만들어 보낸다. 재접속 복원도 같은 페이로드를 쓴다.
// 은닉: yourRoll 은 차례 본인의 rolling 스냅샷에만 실린다 — 타인·관전자의
// raw JSON 에는 필드 자체가 없다 (포인터 nil 생략).
type CCGameStatePayload struct {
	GameID   string  `json:"gameId"`
	RoomCode string  `json:"roomCode"`
	Phase    CCPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"` // 관전자는 -1
	// Spectators 관전자 수 (참가자·관전자 모두 표시용)
	Spectators int `json:"spectators"`
	// EndsAt 현재 대기 상태(선언·의심 창)의 마감 시각 (unixMillis, 그 외 0)
	EndsAt      int64 `json:"endsAt"`
	CurrentSeat int   `json:"currentSeat"`
	// YourRoll 본인 차례 rolling 에만: 1~4 또는 0=X (타인·관전자 부재)
	YourRoll   *int           `json:"yourRoll,omitempty"`
	Declared   int            `json:"declared"`   // 선언 값 (doubt_window 동안, 그 외 0)
	LastReveal *CCReveal      `json:"lastReveal"` // 직전 판정 (없으면 null)
	Players    []CCPlayerView `json:"players"`
	BridgeLen  int            `json:"bridgeLen"`
}

// CCEventPayload 연출용 이벤트. 판정 전의 실제 주사위 값을 담지 않으며
// 전원에게 동일하게 간다. Seat 은 좌석 0 유실 방지를 위해 포인터로 생략을
// 표현한다.
type CCEventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

// CCGameOverPayload 게임 종료 발표
type CCGameOverPayload struct {
	WinnerSeat int            `json:"winnerSeat"`
	WinnerName string         `json:"winnerName"`
	Reason     string         `json:"reason"` // "two_crossed" | "most_crossed"
	Players    []CCPlayerView `json:"players"`
}

type CCPlayerDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type CCPlayerReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type CCErrorPayload struct {
	Message string `json:"message"`
}
