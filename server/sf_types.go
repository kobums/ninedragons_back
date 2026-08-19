package server

import (
	"time"
)

// ==================== 스카이폴(클래식 마피아) 타입 ====================
//
// 6~10인 로비형 진행 도우미. 채팅 없음 — 토론은 같은 공간(음성)에서 하고
// 앱은 역할 배정·밤 행동·투표·판정만 맡는다. 은닉이 이 게임의 전부라
// 개인화 스냅샷(buildSFState)이 유일한 정보 통로다.

const (
	SFMinPlayers = 6  // 시작 최소 인원
	SFMaxPlayers = 10 // 좌석 수 상한

	// SFBotFillTarget 봇 채우기 상한. 봇은 연습용이라 최소 성립 인원(6)까지만
	// 채운다 — 정원(10)까지 채우는 게 아니다.
	SFBotFillTarget = 6
)

// SFPhase 게임 진행 단계
type SFPhase string

const (
	SFPhaseWaiting   SFPhase = "waiting"
	SFPhaseNight     SFPhase = "night"
	SFPhaseDayResult SFPhase = "day_result"
	SFPhaseDayVote   SFPhase = "day_vote"
	SFPhaseExecution SFPhase = "execution"
	SFPhaseGameOver  SFPhase = "game_over"
)

// SFRole 역할
type SFRole string

const (
	SFRoleMafia   SFRole = "mafia"
	SFRolePolice  SFRole = "police"
	SFRoleDoctor  SFRole = "doctor"
	SFRoleCitizen SFRole = "citizen"
)

// SFMessageType 스카이폴 메시지 타입
type SFMessageType string

const (
	// 클라이언트 → 서버
	SFMsgJoinGame    SFMessageType = "sf_join_game"
	SFMsgFillBots    SFMessageType = "sf_fill_bots"
	SFMsgStart       SFMessageType = "sf_start"
	SFMsgNightAction SFMessageType = "sf_night_action"
	SFMsgVote        SFMessageType = "sf_vote"
	SFMsgRejoin      SFMessageType = "sf_rejoin"
	SFMsgReact       SFMessageType = "sf_react"

	// 서버 → 클라이언트
	SFMsgPlayerJoined         SFMessageType = "sf_player_joined"
	SFMsgSpectateJoined       SFMessageType = "sf_spectate_joined"
	SFMsgGameState            SFMessageType = "sf_game_state"
	SFMsgEvent                SFMessageType = "sf_event"
	SFMsgGameOver             SFMessageType = "sf_game_over"
	SFMsgOpponentDisconnected SFMessageType = "sf_opponent_disconnected"
	SFMsgReconnected          SFMessageType = "sf_reconnected"
	SFMsgSessionExpired       SFMessageType = "sf_session_expired"
	SFMsgError                SFMessageType = "sf_error"
)

// SFClient 스카이폴 클라이언트 연결
type SFClient struct {
	wsClient
	Hub  *SFHub
	Seat int
}

// SFMessage 메시지 봉투
type SFMessage struct {
	Type    SFMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 순수 게임 상태 ====================

// SFPlayer 좌석 하나 (연결은 sfRoom 이 든다)
type SFPlayer struct {
	Seat  int
	Name  string
	Role  SFRole
	Alive bool

	// RoleRevealed 처형(또는 게임 종료)으로 역할이 공개됐는지.
	// 밤 살해자의 역할은 종료 전까지 비공개다.
	RoleRevealed bool
}

// SFGame 스카이폴 게임 상태 (순수, 허브 비의존)
type SFGame struct {
	ID      string
	Phase   SFPhase
	DayNo   int
	Players []SFPlayer

	// NightActions 밤 행동 제출 장부: 행동자 seat → 대상 seat.
	// 역할별 의미(마피아=암살, 경찰=조사, 의사=치료)는 행동자의 역할이 정한다.
	NightActions map[int]int

	// Investigation 경찰의 직전 조사 결과 — 낮 동안 유지, 다음 밤에 비운다
	Investigation *SFInvestigationView

	// Announcement 밤 결과 발표 — day_result 부터 다음 밤 전까지 유지
	Announcement *SFAnnouncementView

	// Votes 공개 투표 장부: voter seat → 대상 seat (-1 = 기권)
	Votes map[int]int

	// Execution 처형 결과 — execution 단계에서 설정, 다음 밤에 비운다
	Execution *SFExecutionView

	// Deadline 밤 행동·낮 투표 마감 시각 (unixMillis) — 허브가 세팅·소비한다
	Deadline int64

	Winner string // ""|"mafia"|"citizen"

	Ready     bool
	StartedAt time.Time
}

// ==================== 클라이언트 → 서버 payload ====================

type SFJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type SFRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

type SFNightActionPayload struct {
	Target int `json:"target"`
}

type SFVotePayload struct {
	Target int `json:"target"` // -1 = 기권
}

// SFReactPayload 리액션 이모지 (화이트리스트 6종 외는 조용히 무시)
type SFReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

type SFErrorPayload struct {
	Message string `json:"message"`
}

// SFPlayerView 좌석별 공개 정보. role 은 처형·게임 종료로 공개된 경우에만
// 채워진다 — 그 전에는 빈 문자열 (은닉의 핵심).
type SFPlayerView struct {
	Seat      int    `json:"seat"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Bot       bool   `json:"bot"`
	Alive     bool   `json:"alive"`
	Role      string `json:"role"`
}

// SFNightView 밤 단계 진행 정보 (night 단계에만 실린다)
type SFNightView struct {
	YourActionDone bool `json:"yourActionDone"`
	// PendingCount 아직 제출하지 않은 생존 역할자 수 — 역할 목록을 노출하면
	// 밤 사망자의 비공개 역할이 추론되므로 (경찰 사망 → 목록에서 사라짐)
	// 인원수만 공개한다
	PendingCount int `json:"pendingCount"`
}

// SFInvestigationView 경찰 조사 결과 — 경찰 본인에게만 실린다
type SFInvestigationView struct {
	Seat    int  `json:"seat"`
	IsMafia bool `json:"isMafia"`
}

// SFAnnouncementView 밤 결과 발표. saved 는 대상 비공개(seat -1).
type SFAnnouncementView struct {
	Kind string `json:"kind"` // "killed" | "saved"
	Seat int    `json:"seat"`
}

// SFVoteView 공개 투표 한 표
type SFVoteView struct {
	Voter  int `json:"voter"`
	Target int `json:"target"` // -1 = 기권
}

// SFExecutionView 처형 결과. 무처형이면 {seat: -1} (role 생략).
type SFExecutionView struct {
	Seat int    `json:"seat"`
	Role string `json:"role,omitempty"`
}

// SFGameStatePayload 개인화 게임 스냅샷 (은닉형).
// 비마피아 스냅샷에는 mafiaSeats 필드 자체가 없어야 한다 (omitempty).
type SFGameStatePayload struct {
	GameID string `json:"gameId"`
	// RoomCode 사설 방 초대 코드 (공용 로비는 "") — 대기실 코드 표시·재접속 복원용
	RoomCode string  `json:"roomCode"`
	Phase    SFPhase `json:"phase"`
	DayNo    int     `json:"dayNo"`
	// EndsAt 밤 행동·낮 투표의 마감 시각 (unixMillis, 그 외 단계 0) —
	// 접속 유지 AFK 가 게임을 영구 정지시키지 않게 허브가 타임아웃을 건다
	EndsAt        int64                `json:"endsAt"`
	HostSeat      int                  `json:"hostSeat"`
	YourSeat      int                  `json:"yourSeat"`
	YourRole      string               `json:"yourRole"`
	MafiaSeats    []int                `json:"mafiaSeats,omitempty"` // 마피아에게만
	Players       []SFPlayerView       `json:"players"`
	// Spectators 관전자 수 (참가자·관전자 모두 표시용)
	Spectators int `json:"spectators"`
	Night         *SFNightView         `json:"night,omitempty"` // night 단계만
	Investigation *SFInvestigationView `json:"investigation"`   // 경찰에게만, 그 외 null
	Announcement  *SFAnnouncementView  `json:"announcement"`
	Votes         []SFVoteView         `json:"votes"`
	Execution     *SFExecutionView     `json:"execution"`
	Winner        string               `json:"winner"`
}

// SFEventPayload 연출용 이벤트.
// kind: joined|left|started|night_begin|day_result|vote_begin|executed|
// no_execution|bot_takeover|game_over
type SFEventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message,omitempty"`
}

// SFGameOverPayload 종료 발표 — 승리 진영과 전원 역할 공개
type SFGameOverPayload struct {
	Winner  string         `json:"winner"`
	DayNo   int            `json:"dayNo"`
	Players []SFPlayerView `json:"players"`
}

type SFOpponentDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type SFReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}
