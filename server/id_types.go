package server

import (
	"math/rand"
	"time"
)

// ==================== 인사이더 (Insider) 타입 ====================
//
// 4~8인 정체 은닉 스무고개 진행 도우미. 질문/답변은 음성으로 하고, 앱은
// 역할 배정·제시어·단계 타이머·비공개 투표만 담당한다.
// 역할: 마스터 1(전원 공개 — masterSeat)·인사이더 1·나머지 시민.
// 제시어는 마스터와 인사이더에게만 보인다 (은닉 스냅샷 — id_hub.go 의
// buildIDState 가 계약의 심장). 질문 5분 → (마스터 [정답 나옴]) → 토론 2분
// → 투표 → 최다 득표 공개(동표면 마스터 지목이 결정)로 승패가 갈린다.

const (
	IDMinPlayers = 4
	IDMaxPlayers = 8

	// IDFillBotTarget id_fill_bots 가 채우는 목표 인원 — 채운 뒤 즉시 시작
	IDFillBotTarget = 5
)

// IDRole 배정 역할 (와이어 값 — yourRole·종료 공개 role 로만 나간다)
type IDRole string

const (
	IDRoleMaster  IDRole = "master"
	IDRoleInsider IDRole = "insider"
	IDRoleCitizen IDRole = "citizen"
)

// IDPhase 게임 진행 단계
type IDPhase string

const (
	IDPhaseWaiting    IDPhase = "waiting"
	IDPhaseQuestion   IDPhase = "question"   // 질문 타임 5분 — 마스터의 [정답 나옴] 대기
	IDPhaseDiscussion IDPhase = "discussion" // 토론 타임 2분 — 마스터의 [투표 시작]으로 단축 가능
	IDPhaseVoting     IDPhase = "voting"     // 전원 동시 비공개 투표
	IDPhaseGameOver   IDPhase = "game_over"
)

// IDMessageType 인사이더 메시지 타입
type IDMessageType string

const (
	// 클라이언트 → 서버
	IDMsgJoinGame IDMessageType = "id_join_game"
	IDMsgFillBots IDMessageType = "id_fill_bots"
	IDMsgStart    IDMessageType = "id_start"
	IDMsgRejoin   IDMessageType = "id_rejoin"
	IDMsgCorrect  IDMessageType = "id_correct"   // 마스터 — [정답 나옴]
	IDMsgOpenVote IDMessageType = "id_open_vote" // 마스터 — 토론 단축, 투표 시작
	IDMsgVote     IDMessageType = "id_vote"
	IDMsgReact    IDMessageType = "id_react"

	// 서버 → 클라이언트
	IDMsgPlayerJoined       IDMessageType = "id_player_joined"
	IDMsgSpectateJoined     IDMessageType = "id_spectate_joined"
	IDMsgGameState          IDMessageType = "id_game_state"
	IDMsgEvent              IDMessageType = "id_event"
	IDMsgGameOver           IDMessageType = "id_game_over"
	IDMsgPlayerDisconnected IDMessageType = "id_player_disconnected"
	IDMsgPlayerReconnected  IDMessageType = "id_player_reconnected"
	IDMsgSessionExpired     IDMessageType = "id_session_expired"
	IDMsgError              IDMessageType = "id_error"
)

// IDPlayer 게임 참가자 (순수 상태 — 연결 매핑은 허브의 idRoom 담당)
type IDPlayer struct {
	Seat int
	Name string
	// Role 배정 역할 — 스냅샷에 직접 노출 금지 (본인 yourRole·종료 공개만)
	Role IDRole
	// Vote 투표 대상 좌석 (-1 미투표)
	Vote int
	// Votes 개표 후 득표 수
	Votes int
}

// Voted 투표 제출 여부 (대상은 개표 전까지 비공개)
func (p *IDPlayer) Voted() bool {
	return p.Vote >= 0
}

// IDGameEvent 순수 규칙이 쌓는 연출 이벤트 — 허브가 DrainEvents 로 꺼내
// id_event 로 방송한다 (인사이더 정체·제시어는 절대 담지 않는다)
type IDGameEvent struct {
	Kind    string
	Seat    int // -1 없음
	Message string
}

// IDResult 개표/종료 결과 — 종료 화면 공개용 (스냅샷 result, 그 전엔 null)
type IDResult struct {
	InsiderSeat int    `json:"insiderSeat"`
	TopSeat     int    `json:"topSeat"` // 최다 득표 좌석 (-1 시간 초과 종료)
	Winner      string `json:"winner"`  // "citizens" | "insider" | "none"
	Message     string `json:"message"`
}

// IDGame 인사이더 게임 상태 (순수, 허브 비의존)
type IDGame struct {
	ID      string
	Players []*IDPlayer
	Phase   IDPhase

	MasterSeat int // 마스터 좌석 (-1 시작 전) — 전원 공개
	// InsiderSeat 인사이더 좌석 (-1 시작 전) — 종료 공개 전 노출 금지
	InsiderSeat int
	// Word 제시어 — 마스터·인사이더에게만 (종료 후 전원 공개)
	Word string

	Result    *IDResult // 종료 결과 (그 전엔 nil)
	EndReason string    // "insider_caught" | "insider_escaped" | "timeout"
	Ready     bool
	StartedAt time.Time

	// StateSeq 대기 상태(question·discussion·voting)가 새로 열릴 때마다 +1 —
	// 허브가 마감 타이머를 다시 걸지 판단하는 근거
	StateSeq int
	// AfkSeq 마감 타이머 일련번호 (뒤늦은 발화 무시용 — 허브가 관리)
	AfkSeq int
	// Deadline 현재 대기 상태의 마감 시각 (unixMillis — 스냅샷 노출용)
	Deadline int64

	events []IDGameEvent
}

// IDClient 인사이더 클라이언트 연결
type IDClient struct {
	wsClient
	Hub  *IDHub
	Seat int
}

// IDMessage 메시지 봉투
type IDMessage struct {
	Type    IDMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type IDJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type IDRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// IDVotePayload 인사이더로 의심되는 좌석 지목 (자기 자신 제외)
type IDVotePayload struct {
	Seat int `json:"seat"`
}

type IDReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

// IDPlayerView 좌석별 공개 정보 — 좌석 0·득표 0 유실 방지를 위해 omitempty 금지
type IDPlayerView struct {
	Seat      int    `json:"seat"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Bot       bool   `json:"bot"`
	Voted     bool   `json:"voted"` // 투표 제출 여부 (대상은 비공개)
	Role      string `json:"role"`  // "" 진행 중 | 종료 후 전원 공개
	Votes     int    `json:"votes"` // 개표 후 득표 수 (그 외 0)
}

// IDGameStatePayload 개인화된 전체 게임 스냅샷. 모든 상태 변경 후 좌석마다
// 따로 만들어 보낸다. 재접속 복원도 같은 페이로드를 쓴다.
// 은닉: yourRole 은 본인에게만, word 는 마스터·인사이더에게만 실린다 —
// 그 외·관전자의 raw JSON 에는 필드 자체가 없다 (빈 문자열 생략).
// masterSeat 은 전원 공개 정보다.
type IDGameStatePayload struct {
	GameID   string  `json:"gameId"`
	RoomCode string  `json:"roomCode"`
	Phase    IDPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"` // 관전자는 -1
	// Spectators 관전자 수 (참가자·관전자 모두 표시용)
	Spectators int `json:"spectators"`
	// EndsAt 현재 단계(질문·토론·투표)의 마감 시각 (unixMillis, 그 외 0)
	EndsAt     int64 `json:"endsAt"`
	MasterSeat int   `json:"masterSeat"` // 전원 공개 (-1 시작 전)
	// YourRole 본인 역할 — 본인에게만 (관전자·시작 전 부재)
	YourRole string `json:"yourRole,omitempty"`
	// Word 제시어 — 마스터·인사이더에게만, 종료 후 전원 (그 외 부재)
	Word string `json:"word,omitempty"`
	// CorrectSeat 예약 필드 — 정답 외친 사람은 앱이 모른다 (항상 -1)
	CorrectSeat int            `json:"correctSeat"`
	Players     []IDPlayerView `json:"players"`
	Result      *IDResult      `json:"result"` // 종료 결과 (그 전엔 null)
}

// IDEventPayload 연출용 이벤트. 인사이더 정체·제시어를 담지 않으며 전원에게
// 동일하게 간다. Seat 은 좌석 0 유실 방지를 위해 포인터로 생략을 표현한다.
type IDEventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

// IDGameOverPayload 게임 종료 발표 — 전원 역할·제시어 공개
type IDGameOverPayload struct {
	Winner      string         `json:"winner"` // "citizens" | "insider" | "none"
	InsiderSeat int            `json:"insiderSeat"`
	MasterSeat  int            `json:"masterSeat"`
	TopSeat     int            `json:"topSeat"` // -1 시간 초과 종료
	Word        string         `json:"word"`
	Players     []IDPlayerView `json:"players"`
}

type IDPlayerDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type IDPlayerReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type IDErrorPayload struct {
	Message string `json:"message"`
}

// ==================== 제시어 풀 (id_words 헬퍼) ====================

// idPickWord 제시어 1개 무작위 추출 — 코드네임의 공용 단어 풀(cn_words 의
// cnWordPool, 스파이폴 카테고리 자산)을 수정 없이 호출만 해서 재사용한다.
func idPickWord(rng *rand.Rand) string {
	pool := cnWordPool()
	return pool[rng.Intn(len(pool))]
}
