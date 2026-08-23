package server

import "time"

// ==================== 더 마인드 (The Mind) 타입 ====================
//
// 2~4인 **협력 실시간**. 세트(se)에 이어 두 번째로 **차례가 없는** 게임이다 —
// currentSeat 도, 좌석별 AFK 자동 진행도 없다. 누구든 언제든 mi_play 를
// 보내고, 허브 고루틴이 도착 순서대로 직렬 판정한다(선착 판정 모델).
//
// 규칙상 **소통이 금지**다. 그래서 이 게임에는 리액션이 없다 — mi_react
// 메시지 자체를 두지 않는다. 이모지 하나도 "지금 내"라는 신호가 되어
// 게임의 전부를 무너뜨리기 때문이다.
//
// 은닉은 하나뿐이다: yourHand 는 본인에게만 실린다. 남에게 보이는 것은
// handCount(장수)뿐이고, 관전자(viewerSeat -1)의 raw JSON 에는 yourHand
// 키 자체가 없다. 중앙 더미(pile)·생명·수리검은 전원 공개다.

const (
	MIMinPlayers = 2
	MIMaxPlayers = 4

	// MIFillBotTarget mi_fill_bots 가 채우는 목표 인원 — 채운 뒤 즉시 시작
	MIFillBotTarget = 3

	// MIDeckSize 덱 크기 — 1~100 숫자 카드 100장 (중복 없음)
	MIDeckSize = 100

	// MIStartStars 시작 수리검 (시작 생명은 인원수와 같다)
	MIStartStars = 1

	// MIBotStarProposeFrom 봇이 수리검을 제안하기 시작하는 최저 카드 기준
	MIBotStarProposeFrom = 90
	// MIBotStarAcceptFrom 봇이 수리검 제안을 수락하는 최저 카드 기준
	MIBotStarAcceptFrom = 60
)

// 시간 상수 — 전부 var 다 (테스트 init 에서 짧게 낮춘다).
// 허브 고루틴·봇 고루틴과 경합하지 않도록 테스트 도중에는 바꾸지 않는다.
var (
	// miReadyDelay 라운드 시작 전 카운트다운 (phase='ready')
	miReadyDelay = 3 * time.Second
	// miRoundEndDelay 라운드 성공 정산을 보여주는 시간 (phase='round_end')
	miRoundEndDelay = 3 * time.Second
	// miRoundCap 라운드 하나의 상한 — 넘기면 자동 진행(생명 -1)
	miRoundCap = 3 * time.Minute
	// miStarVoteWindow 수리검 만장일치를 기다리는 시간
	miStarVoteWindow = 20 * time.Second
	// miGameCap 게임 전체 상한 — 무한 게임 방지 안전장치
	miGameCap = 20 * time.Minute
)

// miFailWinnerTag 협력 실패 기록의 Winner 표기.
//
// 전적 장부(stats.go)는 Winner == "" 를 무승부로 집계한다. 더 마인드는 협력
// 게임이라 실패에 "이긴 사람"이 없지만 무승부도 아니므로, 어떤 닉네임과도
// 겹치지 않는 표식을 넣어 참가자 전원이 패배로 집계되게 한다.
// 클리어일 때는 반대로 전원 닉네임이 Winner 에 들어간다 (전원 승자).
const miFailWinnerTag = "마인드 실패"

// miMaxRoundByPlayers 인원별 최종 라운드 — 2인 12 / 3인 10 / 4인 8
func miMaxRoundByPlayers(n int) int {
	switch n {
	case 2:
		return 12
	case 3:
		return 10
	default:
		return 8
	}
}

// miLifeBonusRounds 마치면 생명 +1 인 라운드, miStarBonusRounds 는 수리검 +1
var (
	miLifeBonusRounds = map[int]bool{3: true, 6: true, 9: true}
	miStarBonusRounds = map[int]bool{2: true, 5: true, 8: true}
)

// MIPhase 게임 진행 단계.
// 차례가 없어 좌석별 대기 상태가 없다 — ready(카운트다운) → playing →
// round_end(정산) → 다음 라운드 ready 의 순환뿐이다.
type MIPhase string

const (
	MIPhaseWaiting  MIPhase = "waiting"
	MIPhaseReady    MIPhase = "ready"
	MIPhasePlaying  MIPhase = "playing"
	MIPhaseRoundEnd MIPhase = "round_end"
	MIPhaseGameOver MIPhase = "game_over"
)

// MIMessageType 더 마인드 메시지 타입.
// 리액션(mi_react)은 **의도적으로 없다** — 규칙상 소통 금지.
type MIMessageType string

const (
	// 클라이언트 → 서버
	MIMsgJoinGame    MIMessageType = "mi_join_game"
	MIMsgFillBots    MIMessageType = "mi_fill_bots"
	MIMsgStart       MIMessageType = "mi_start"
	MIMsgRejoin      MIMessageType = "mi_rejoin"
	MIMsgPlay        MIMessageType = "mi_play"
	MIMsgStarPropose MIMessageType = "mi_star_propose"
	MIMsgStarAccept  MIMessageType = "mi_star_accept"
	MIMsgStarDecline MIMessageType = "mi_star_decline"

	// 서버 → 클라이언트
	MIMsgPlayerJoined       MIMessageType = "mi_player_joined"
	MIMsgSpectateJoined     MIMessageType = "mi_spectate_joined"
	MIMsgGameState          MIMessageType = "mi_game_state"
	MIMsgEvent              MIMessageType = "mi_event"
	MIMsgGameOver           MIMessageType = "mi_game_over"
	MIMsgPlayerDisconnected MIMessageType = "mi_player_disconnected"
	MIMsgPlayerReconnected  MIMessageType = "mi_player_reconnected"
	MIMsgSessionExpired     MIMessageType = "mi_session_expired"
	MIMsgError              MIMessageType = "mi_error"
)

// MIPlayer 게임 참가자 (순수 상태 — 연결 매핑은 허브의 miRoom 담당).
// Hand 는 항상 오름차순이다. 낼 수 있는 카드는 Hand[0] 하나뿐이라
// 프로토콜에 카드 지정이 없다 (오조작 원천 차단).
type MIPlayer struct {
	Seat int
	Name string
	Hand []int
}

// MIGameEvent 순수 규칙이 쌓는 연출 이벤트 — 허브가 DrainEvents 로 꺼내
// mi_event 로 방송한다. 손패 숫자는 이벤트에 담지 않는다(은닉 유지) —
// 예외는 이미 공개된 카드(낸 카드·소각된 카드·수리검으로 버린 카드)다.
type MIGameEvent struct {
	Kind    string
	Seat    int // -1 없음
	Message string
}

// MIBurnedCard 실수로 소각된 카드 한 장 (전원 공개)
type MIBurnedCard struct {
	Seat int `json:"seat"`
	Card int `json:"card"`
}

// MIMistake 직전 실수 (전원 공개). 프론트의 실수 연출 근거다.
// 한 번에 여러 장이 걸려도 생명은 1만 깎인다.
type MIMistake struct {
	// Seat 실수를 저지른(카드를 낸) 좌석. 닉네임은 players[] 에 이미 있어
	// 계약대로 싣지 않는다 — 프론트는 seat 로 이름을 찾는다.
	Seat int `json:"seat"`
	// Played 실수를 유발한(방금 낸) 카드. 자동 진행이면 0 이 아니라 그 카드다.
	Played int `json:"played"`
	// Burned 공개·소각된 카드들 — 항상 [] (nil → JSON null 금지)
	Burned  []MIBurnedCard `json:"burned"`
	Message string         `json:"message"`
}

// MIStarVote 진행 중인 수리검 투표. 제안자는 자동 찬성으로 시작한다.
// 한 명이라도 거절하거나 창이 지나면 무산된다.
type MIStarVote struct {
	Proposer int `json:"proposer"`
	// Accepted 찬성한 좌석 (제안자 포함, 오름차순) — 항상 []
	Accepted []int `json:"accepted"`
	// EndsAt 무산 시각 (unixMillis)
	EndsAt int64 `json:"endsAt"`
	// Seq 발화 구분용 일련번호 (와이어에 싣지 않는다)
	Seq int `json:"-"`
}

// MIResult 종료 결과 — 협력 게임이라 승패가 아니라 클리어 여부다
type MIResult struct {
	Cleared bool   `json:"cleared"`
	Round   int    `json:"round"`
	Message string `json:"message"`
}

// MIGame 더 마인드 게임 상태 (순수, 허브 비의존).
// 차례가 없으므로 CurrentSeat·AfkSeq 가 없다.
type MIGame struct {
	ID      string
	Players []*MIPlayer
	Phase   MIPhase

	Round    int
	MaxRound int
	Lives    int
	Stars    int

	// Pile 중앙에 쌓인 카드 (낸 순서대로, 전원 공개)
	Pile []int
	// LastPlayed 직전에 나온 수 (0 = 아직 없음)
	LastPlayed int

	StarVote    *MIStarVote
	LastMistake *MIMistake
	Result      *MIResult
	EndReason   string // "cleared" | "no_lives" | "time_up"

	Ready     bool
	StartedAt time.Time

	// StateSeq 단계 마감 타이머 일련번호 (지나간 발화 무시용 — 허브가 관리)
	StateSeq int
	// StarSeq 수리검 투표 일련번호
	StarSeq int
	// EndSeq 게임 캡 타이머 일련번호
	EndSeq int
	// Deadline 현재 단계의 마감 시각 (unixMillis — 스냅샷 노출용)
	Deadline int64

	events []MIGameEvent
}

// MIClient 더 마인드 클라이언트 연결
type MIClient struct {
	wsClient
	Hub  *MIHub
	Seat int
}

// MIMessage 메시지 봉투
type MIMessage struct {
	Type    MIMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type MIJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 관대하게 새로 생성)
	Room string `json:"room,omitempty"`
}

type MIRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// mi_play / mi_star_* 에는 payload 가 없다 — 낼 수 있는 카드는 최저 하나뿐이라
// 카드를 지정할 이유가 없고, 지정할 수 없으면 오조작도 없다.

// ==================== 서버 → 클라이언트 payload ====================

// MIPlayerView 좌석별 공개 정보 — 좌석 0·장수 0 유실 방지를 위해 omitempty 금지.
// 손패의 **숫자는 절대 실리지 않는다**. 공개되는 것은 장수뿐이다.
type MIPlayerView struct {
	Seat      int    `json:"seat"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Bot       bool   `json:"bot"`
	HandCount int    `json:"handCount"`
}

// MIGameStatePayload 전체 게임 스냅샷. 모든 상태 변경 후 방송한다.
// 재접속 복원도 같은 페이로드를 쓴다.
//
// 은닉: YourHand 만 본인에게 실린다 — 타인·관전자(viewerSeat -1)의 raw JSON
// 에는 yourHand 키 자체가 없다(포인터 + omitempty). 나머지는 전원 동일.
type MIGameStatePayload struct {
	GameID   string  `json:"gameId"`
	RoomCode string  `json:"roomCode"`
	Phase    MIPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"` // 관전자는 -1
	// Spectators 관전자 수
	Spectators int `json:"spectators"`
	// EndsAt 현재 단계의 마감 시각 (unixMillis, 없으면 0)
	EndsAt int64 `json:"endsAt"`

	Round    int `json:"round"`
	MaxRound int `json:"maxRound"`
	Lives    int `json:"lives"`
	Stars    int `json:"stars"`
	// LastPlayed 직전에 나온 수 (0 = 없음)
	LastPlayed int `json:"lastPlayed"`
	// Pile 중앙에 쌓인 순서대로 — 항상 [] (nil → JSON null 금지)
	Pile []int `json:"pile"`

	// YourHand 본인 손패(오름차순) — 본인에게만. 관전자·타인은 키 부재.
	// 빈 손도 []( null 금지)로 나가야 하므로 포인터가 가리키는 값은 항상 비-nil.
	YourHand *[]int `json:"yourHand,omitempty"`

	// Players 좌석 정보 — 항상 []
	Players     []MIPlayerView `json:"players"`
	StarVote    *MIStarVote    `json:"starVote"`    // 없으면 null
	LastMistake *MIMistake     `json:"lastMistake"` // 없으면 null
	Result      *MIResult      `json:"result"`      // 종료 전엔 null
}

// MIEventPayload 연출용 이벤트. 전원에게 동일하게 간다.
// Seat 은 좌석 0 유실 방지를 위해 포인터로 생략을 표현한다.
type MIEventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

// MIGameOverPayload 게임 종료 발표
type MIGameOverPayload struct {
	Cleared  bool   `json:"cleared"`
	Reason   string `json:"reason"` // "cleared" | "no_lives" | "time_up"
	Round    int    `json:"round"`
	MaxRound int    `json:"maxRound"`
	Lives    int    `json:"lives"`
	Stars    int    `json:"stars"`
	Message  string `json:"message"`
	// Players 최종 좌석 정보 — 손패 숫자는 여기에도 실리지 않는다
	Players []MIPlayerView `json:"players"`
}

type MIPlayerDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type MIPlayerReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type MIErrorPayload struct {
	Message string `json:"message"`
}
