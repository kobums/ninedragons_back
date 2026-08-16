package server

import (
	"time"
)

// ==================== 스파이폴 타입 ====================
//
// 3~8인 로비형 장소 추리 진행 도우미. 질문/토론은 같은 공간(음성)에서 하고
// 앱은 장소 배부·타이머·스파이 추리·투표·판정만 맡는다. 스파이 좌석의 은닉이
// 이 게임의 전부라 개인화 스냅샷(buildSPState)이 유일한 정보 통로다.

const (
	SPMinPlayers = 3 // 시작 최소 인원
	SPMaxPlayers = 8 // 좌석 수 상한

	// SPBotFillTarget 봇 채우기 상한. 봇은 연습용이라 최소 성립 인원(3)까지만
	// 채운다 — 정원(8)까지 채우는 게 아니다.
	SPBotFillTarget = 3

	// SPDefaultTimerMinutes 대기실 기본 타이머 (host 가 3/5/8분 중 선택)
	SPDefaultTimerMinutes = 5
)

// spLocations 장소 24곳 — 프론트와 공유하는 계약. 순서·표기를 바꾸지 않는다.
var spLocations = []string{
	"병원", "학교", "은행", "해변", "카지노", "영화관", "지하철", "공항",
	"우주정거장", "잠수함", "경찰서", "소방서", "유람선", "호텔", "대사관",
	"슈퍼마켓", "레스토랑", "대학교", "군부대", "놀이공원", "목욕탕",
	"결혼식장", "도서관", "야구장",
}

// SPPhase 게임 진행 단계
type SPPhase string

const (
	SPPhaseWaiting  SPPhase = "waiting"
	SPPhasePlaying  SPPhase = "playing"
	SPPhaseVoting   SPPhase = "voting"
	SPPhaseGameOver SPPhase = "game_over"
)

// SPMessageType 스파이폴 메시지 타입
type SPMessageType string

const (
	// 클라이언트 → 서버
	SPMsgJoinGame SPMessageType = "sp_join_game"
	SPMsgFillBots SPMessageType = "sp_fill_bots"
	SPMsgStart    SPMessageType = "sp_start"
	SPMsgSetTimer SPMessageType = "sp_set_timer"
	SPMsgGuess    SPMessageType = "sp_guess"
	SPMsgVote     SPMessageType = "sp_vote"
	SPMsgRejoin   SPMessageType = "sp_rejoin"

	// 서버 → 클라이언트
	SPMsgPlayerJoined         SPMessageType = "sp_player_joined"
	SPMsgGameState            SPMessageType = "sp_game_state"
	SPMsgEvent                SPMessageType = "sp_event"
	SPMsgGameOver             SPMessageType = "sp_game_over"
	SPMsgOpponentDisconnected SPMessageType = "sp_opponent_disconnected"
	SPMsgReconnected          SPMessageType = "sp_reconnected"
	SPMsgSessionExpired       SPMessageType = "sp_session_expired"
	SPMsgError                SPMessageType = "sp_error"
)

// SPClient 스파이폴 클라이언트 연결
type SPClient struct {
	wsClient
	Hub  *SPHub
	Seat int
}

// SPMessage 메시지 봉투
type SPMessage struct {
	Type    SPMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 순수 게임 상태 ====================

// SPPlayer 좌석 하나 (연결은 spRoom 이 든다)
type SPPlayer struct {
	Seat int
	Name string
}

// SPGame 스파이폴 게임 상태 (순수, 허브 비의존)
type SPGame struct {
	ID      string
	Phase   SPPhase
	Players []SPPlayer

	// TimerMinutes host 가 대기실에서 고른 타이머 (3|5|8)
	TimerMinutes int

	// SpySeat 스파이 좌석 — 절대 스냅샷으로 직접 내보내지 않는다 (은닉의 핵심).
	// 시작 전에는 -1.
	SpySeat int

	// Location 이번 판의 장소 (spLocations 중 1곳). 비스파이에게만 보인다.
	Location string

	// EndsAt playing 타이머 종료 시각 (unixMillis) — 프론트 카운트다운 기준
	EndsAt int64

	// Votes 공개 투표 장부: voter seat → 대상 seat (기권 없음)
	Votes map[int]int

	// Result 판정 결과 — game_over 에서만 채워진다
	Result *SPResultView

	Ready     bool
	StartedAt time.Time
}

// ==================== 클라이언트 → 서버 payload ====================

type SPJoinGamePayload struct {
	Name string `json:"name"`
}

type SPRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

type SPSetTimerPayload struct {
	Minutes int `json:"minutes"`
}

type SPGuessPayload struct {
	Location string `json:"location"`
}

type SPVotePayload struct {
	Target int `json:"target"`
}

// ==================== 서버 → 클라이언트 payload ====================

type SPErrorPayload struct {
	Message string `json:"message"`
}

// SPPlayerView 좌석별 공개 정보. voted 는 공개 투표의 제출 여부다 —
// 누가 스파이인지에 대한 정보는 어떤 형태로도 싣지 않는다.
type SPPlayerView struct {
	Seat      int    `json:"seat"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Bot       bool   `json:"bot"`
	Voted     bool   `json:"voted"`
}

// SPVoteView 공개 투표 한 표
type SPVoteView struct {
	Voter  int `json:"voter"`
	Target int `json:"target"`
}

// SPResultView 판정 결과 (game_over 에서만 존재 — 스파이 정체·장소 공개)
type SPResultView struct {
	Winner          string `json:"winner"` // "spy" | "citizen"
	SpySeat         int    `json:"spySeat"`
	Location        string `json:"location"`
	Reason          string `json:"reason"` // guess_right|guess_wrong|vote_caught|vote_missed
	GuessedLocation string `json:"guessedLocation"`
	TopSeat         int    `json:"topSeat"` // -1 = 단독 최다 없음(동표)
}

// SPGameStatePayload 개인화 게임 스냅샷 (은닉형).
// 비스파이 스냅샷에는 스파이 좌석 정보가 어떤 형태로도 없어야 한다.
// isSpy 는 본인 것만, location 은 비스파이에게만 채운다 (스파이·waiting 은 빈).
type SPGameStatePayload struct {
	GameID       string         `json:"gameId"`
	Phase        SPPhase        `json:"phase"`
	HostSeat     int            `json:"hostSeat"`
	YourSeat     int            `json:"yourSeat"`
	TimerMinutes int            `json:"timerMinutes"`
	EndsAt       int64          `json:"endsAt"`    // playing 에만 >0
	Locations    []string       `json:"locations"` // playing 부터 24곳
	IsSpy        bool           `json:"isSpy"`
	Location     string         `json:"location"`
	Players      []SPPlayerView `json:"players"`
	Votes        []SPVoteView   `json:"votes"`
	Result       *SPResultView  `json:"result"`
}

// SPEventPayload 연출용 이벤트.
// kind: joined|left|started|guess|vote_begin|game_over|bot_takeover
type SPEventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message,omitempty"`
}

// SPGameOverPayload 종료 발표 — 스파이 정체·장소·사유 공개
type SPGameOverPayload struct {
	Winner          string         `json:"winner"`
	SpySeat         int            `json:"spySeat"`
	Location        string         `json:"location"`
	Reason          string         `json:"reason"`
	GuessedLocation string         `json:"guessedLocation"`
	TopSeat         int            `json:"topSeat"`
	Players         []SPPlayerView `json:"players"`
}

type SPOpponentDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type SPReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}
