package server

import (
	"time"
)

// ==================== 아발론 타입 ====================
//
// 5~10인 로비형 진행 도우미 (레지스탕스 계열). 토론은 같은 공간(음성)에서
// 하고 앱은 역할 배정·원정 지명·공개 투표·비밀 원정 카드·판정만 맡는다.
// 마피아(sf_*)와 같은 결 — 은닉이 핵심이라 개인화 스냅샷(buildAVState)이
// 유일한 정보 통로다.

const (
	AVMinPlayers = 5  // 시작 최소 인원
	AVMaxPlayers = 10 // 좌석 수 상한

	// AVBotFillTarget 봇 채우기 상한. 봇은 연습용이라 최소 성립 인원(5)까지만
	// 채운다 — 정원(10)까지 채우는 게 아니다.
	AVBotFillTarget = 5

	// AVMaxRejects 연속 부결 한도 — 5회 연속 부결이면 악 진영 즉시 승리
	AVMaxRejects = 5
)

// AVPhase 게임 진행 단계
type AVPhase string

const (
	AVPhaseWaiting  AVPhase = "waiting"
	AVPhaseTeamPick AVPhase = "team_pick"
	AVPhaseTeamVote AVPhase = "team_vote"
	AVPhaseQuest    AVPhase = "quest"
	AVPhaseAssassin AVPhase = "assassin"
	AVPhaseGameOver AVPhase = "game_over"
)

// AVRole 역할 (v1: 멀린·암살자·선 일반·악 일반)
type AVRole string

const (
	AVRoleMerlin   AVRole = "merlin"   // 선 — 악 명단을 본다, 암살 대상
	AVRoleGood     AVRole = "good"     // 선 일반
	AVRoleAssassin AVRole = "assassin" // 악 — 선 3승 시 멀린 지목
	AVRoleEvil     AVRole = "evil"     // 악 일반
)

// AVMessageType 아발론 메시지 타입
type AVMessageType string

const (
	// 클라이언트 → 서버
	AVMsgJoinGame    AVMessageType = "av_join_game"
	AVMsgFillBots    AVMessageType = "av_fill_bots"
	AVMsgStart       AVMessageType = "av_start"
	AVMsgRejoin      AVMessageType = "av_rejoin"
	AVMsgPick        AVMessageType = "av_pick"
	AVMsgTeamVote    AVMessageType = "av_team_vote"
	AVMsgQuest       AVMessageType = "av_quest"
	AVMsgAssassinate AVMessageType = "av_assassinate"

	// 서버 → 클라이언트
	AVMsgPlayerJoined         AVMessageType = "av_player_joined"
	AVMsgGameState            AVMessageType = "av_game_state"
	AVMsgEvent                AVMessageType = "av_event"
	AVMsgGameOver             AVMessageType = "av_game_over"
	AVMsgOpponentDisconnected AVMessageType = "av_opponent_disconnected"
	AVMsgReconnected          AVMessageType = "av_reconnected"
	AVMsgSessionExpired       AVMessageType = "av_session_expired"
	AVMsgError                AVMessageType = "av_error"
)

// AVClient 아발론 클라이언트 연결
type AVClient struct {
	wsClient
	Hub  *AVHub
	Seat int
}

// AVMessage 메시지 봉투
type AVMessage struct {
	Type    AVMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 순수 게임 상태 ====================

// AVPlayer 좌석 하나 (연결은 avRoom 이 든다)
type AVPlayer struct {
	Seat int
	Name string
	Role AVRole
}

// AVGame 아발론 게임 상태 (순수, 허브 비의존)
type AVGame struct {
	ID      string
	Phase   AVPhase
	Players []AVPlayer

	Round      int    // 1..5
	QuestSizes [5]int // 라운드별 원정대 인원 (인원수 테이블)

	// Results 라운드별 원정 결과 ("good"|"evil")
	Results []string

	LeaderSeat  int
	RejectCount int // 연속 부결 수 (0..4) — 승인 시 리셋

	// Team 제안 중(team_vote)·확정(quest) 원정대 좌석 (오름차순)
	Team []int

	// TeamVotes 팀 투표 장부: voter seat → 찬성 여부.
	// 해소 전에는 제출 여부(votedTeam)만 공개한다 — 눈치 투표 방지.
	TeamVotes map[int]bool

	// RevealedVotes 해소된 팀 투표의 일괄 공개본 — 다음 지명 확정 전까지 유지
	RevealedVotes []AVTeamVoteView

	// QuestCards 원정 카드 장부: seat → 성공 여부. 개인 제출은 끝까지 비밀 —
	// 스냅샷에는 제출 여부(questDone)와 집계(lastQuest)만 나간다.
	QuestCards map[int]bool

	// LastQuest 직전 원정의 공개 집계 (실패 N장 / 원정대 인원)
	LastQuest *AVQuestView

	// AssassinTarget 암살자가 지목한 좌석 (-1 = 미지목)
	AssassinTarget int

	Winner    string // ""|"good"|"evil"
	WinReason string // 원정 3승 / 부결 5연속 / 암살 적중 / 암살 빗나감

	// Deadline 현재 단계 마감 시각 (unixMillis) — 허브가 세팅·소비한다
	Deadline int64

	// Seq 단계 전환 일련번호 — 같은 (phase, round)가 반복돼도(부결 후 재지명)
	// 지나간 타이머 발화를 구분하는 근거. scheduleDeadline 이 올린다.
	Seq int

	Ready     bool
	StartedAt time.Time
}

// ==================== 클라이언트 → 서버 payload ====================

type AVJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type AVRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

type AVPickPayload struct {
	Seats []int `json:"seats"`
}

type AVTeamVotePayload struct {
	Approve bool `json:"approve"`
}

type AVQuestPayload struct {
	Success bool `json:"success"`
}

type AVAssassinatePayload struct {
	Seat int `json:"seat"`
}

// ==================== 서버 → 클라이언트 payload ====================

type AVErrorPayload struct {
	Message string `json:"message"`
}

// AVPlayerView 좌석별 공개 정보. role 은 게임 종료 후에만 채워진다 —
// 그 전에는 빈 문자열 (은닉의 핵심). 원정 카드는 제출 여부(questDone)만.
type AVPlayerView struct {
	Seat      int    `json:"seat"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Bot       bool   `json:"bot"`
	OnTeam    bool   `json:"onTeam"`
	VotedTeam bool   `json:"votedTeam"`
	QuestDone bool   `json:"questDone"`
	Role      string `json:"role"`
}

// AVTeamVoteView 팀 투표 한 표 — 전원 제출 후 일괄 공개용
type AVTeamVoteView struct {
	Voter   int  `json:"voter"`
	Approve bool `json:"approve"`
}

// AVQuestView 원정 집계 — 누가 냈는지는 끝까지 비공개, 장수만 공개
type AVQuestView struct {
	Fails int `json:"fails"`
	Size  int `json:"size"`
}

// AVGameStatePayload 개인화 게임 스냅샷 (은닉형).
// evilSeats 는 악 진영 + 멀린에게만 — 그 외 스냅샷에는 필드 자체가 없어야
// 한다 (omitempty).
type AVGameStatePayload struct {
	GameID string `json:"gameId"`
	// RoomCode 사설 방 초대 코드 (공용 로비는 "") — 대기실 코드 표시·재접속 복원용
	RoomCode string  `json:"roomCode"`
	Phase    AVPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"`
	// EndsAt 현재 단계 마감 시각 (unixMillis, waiting/game_over 는 0) —
	// 접속 유지 AFK 가 게임을 영구 정지시키지 않게 허브가 타임아웃을 건다
	EndsAt    int64  `json:"endsAt"`
	YourRole  string `json:"yourRole"`
	EvilSeats []int  `json:"evilSeats,omitempty"` // 악 진영 + 멀린에게만

	Players []AVPlayerView `json:"players"`

	Round      int      `json:"round"`
	QuestSizes [5]int   `json:"questSizes"`
	Results    []string `json:"results"`

	LeaderSeat  int   `json:"leaderSeat"`
	RejectCount int   `json:"rejectCount"`
	Team        []int `json:"team"`

	// TeamVotes 해소 후 일괄 공개 — 해소 전에는 null (제출 여부는 votedTeam 만)
	TeamVotes []AVTeamVoteView `json:"teamVotes"`

	LastQuest *AVQuestView `json:"lastQuest"`

	Winner         string `json:"winner"`
	AssassinTarget int    `json:"assassinTarget"` // -1|seat (game_over 에서)
}

// AVEventPayload 연출용 이벤트.
// kind: joined|left|started|team_pick|team_proposed|team_approved|
// team_rejected|quest_result|assassin_begin|bot_takeover|game_over
type AVEventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message,omitempty"`
}

// AVGameOverPayload 종료 발표 — 승리 진영과 전원 역할 공개
type AVGameOverPayload struct {
	Winner         string         `json:"winner"`
	Reason         string         `json:"reason"`
	AssassinTarget int            `json:"assassinTarget"`
	Players        []AVPlayerView `json:"players"`
}

type AVOpponentDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type AVReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}
