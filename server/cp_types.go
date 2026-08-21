package server

import "time"

// ==================== 쿠 (Coup) 타입 ====================
//
// 2~6인 블러핑 정체 은닉. 각자 비공개 카드 2장(은닉: 본인만) + 칩 2개.
// 역할 주장(액션·차단)에 누구나 도전할 수 있고, 도전·차단·차단 도전의 세
// "응답 창"은 생존자 전원이 허용하거나 시간이 지나야 통과된다 — 이 상태
// 기계(cp_game.go)와 창 마감 타이머(cp_hub.go)가 계약의 심장이다.

const (
	CPMinPlayers = 2
	CPMaxPlayers = 6

	// CPFillBotTarget cp_fill_bots 가 채우는 목표 인원 — 채운 뒤 즉시 시작
	CPFillBotTarget = 4

	CPStartChips     = 2  // 시작 칩
	CPCoupCost       = 7  // 쿠 비용
	CPAssassinCost   = 3  // 암살 비용
	CPForceCoupChips = 10 // 이 이상 보유하면 쿠 강제
	CPStealMax       = 2  // 강탈 상한 (대상 칩이 모자라면 있는 만큼)
	CPRoleCopies     = 3  // 역할별 카드 수 — 5역할 × 3 = 15장
	CPCardsPerPlayer = 2
)

// CPRole 역할 카드 (와이어 값 — 프론트와 공유)
type CPRole string

const (
	CPRoleDuke       CPRole = "duke"
	CPRoleAssassin   CPRole = "assassin"
	CPRoleCaptain    CPRole = "captain"
	CPRoleAmbassador CPRole = "ambassador"
	CPRoleContessa   CPRole = "contessa"
)

// cpAllRoles 덱 구성 순서 (셔플 전 기준)
var cpAllRoles = []CPRole{CPRoleDuke, CPRoleAssassin, CPRoleCaptain, CPRoleAmbassador, CPRoleContessa}

// cpRoleName 한글 역할명 (이벤트 문구용)
func cpRoleName(r CPRole) string {
	switch r {
	case CPRoleDuke:
		return "공작"
	case CPRoleAssassin:
		return "암살자"
	case CPRoleCaptain:
		return "사령관"
	case CPRoleAmbassador:
		return "대사"
	case CPRoleContessa:
		return "백작부인"
	}
	return string(r)
}

// CPActionKind 차례 액션 7종 (와이어 값)
type CPActionKind string

const (
	CPActIncome      CPActionKind = "income"
	CPActAid         CPActionKind = "aid"
	CPActCoup        CPActionKind = "coup"
	CPActTax         CPActionKind = "tax"
	CPActAssassinate CPActionKind = "assassinate"
	CPActSteal       CPActionKind = "steal"
	CPActExchange    CPActionKind = "exchange"
)

// cpActionName 한글 액션명 (이벤트 문구용)
func cpActionName(k CPActionKind) string {
	switch k {
	case CPActIncome:
		return "수입"
	case CPActAid:
		return "해외원조"
	case CPActCoup:
		return "쿠"
	case CPActTax:
		return "세금"
	case CPActAssassinate:
		return "암살"
	case CPActSteal:
		return "강탈"
	case CPActExchange:
		return "교환"
	}
	return string(k)
}

// cpActionClaim 역할 주장이 붙는 액션 → 주장 역할 (챌린지 대상).
// 수입·해외원조·쿠는 주장이 없어 도전할 수 없다.
var cpActionClaim = map[CPActionKind]CPRole{
	CPActTax:         CPRoleDuke,
	CPActAssassinate: CPRoleAssassin,
	CPActSteal:       CPRoleCaptain,
	CPActExchange:    CPRoleAmbassador,
}

// cpBlockRoles 액션별 차단 가능 역할. 해외원조는 아무나, 암살·강탈은
// 대상만 차단할 수 있다 (SubmitBlock 에서 판정).
var cpBlockRoles = map[CPActionKind][]CPRole{
	CPActAid:         {CPRoleDuke},
	CPActAssassinate: {CPRoleContessa},
	CPActSteal:       {CPRoleCaptain, CPRoleAmbassador},
}

// CPPhase 게임 진행 단계
type CPPhase string

const (
	CPPhaseWaiting              CPPhase = "waiting"
	CPPhaseAction               CPPhase = "action"                 // 현재 차례가 액션 선택 중
	CPPhaseChallengeWindow      CPPhase = "challenge_window"       // 액션의 역할 주장에 도전 창
	CPPhaseBlockWindow          CPPhase = "block_window"           // 차단 선언 창 (해당 액션만)
	CPPhaseBlockChallengeWindow CPPhase = "block_challenge_window" // 차단의 역할 주장에 도전 창
	CPPhaseLoseCard             CPPhase = "lose_card"              // 당사자가 잃을 카드 선택 중
	CPPhaseExchangePick         CPPhase = "exchange_pick"          // 교환 — 유지 카드 선택 중
	CPPhaseGameOver             CPPhase = "game_over"
)

// CPMessageType 쿠 메시지 타입
type CPMessageType string

const (
	// 클라이언트 → 서버
	CPMsgJoinGame     CPMessageType = "cp_join_game"
	CPMsgFillBots     CPMessageType = "cp_fill_bots"
	CPMsgStart        CPMessageType = "cp_start"
	CPMsgRejoin       CPMessageType = "cp_rejoin"
	CPMsgAction       CPMessageType = "cp_action"
	CPMsgAllow        CPMessageType = "cp_allow"
	CPMsgChallenge    CPMessageType = "cp_challenge"
	CPMsgBlock        CPMessageType = "cp_block"
	CPMsgLose         CPMessageType = "cp_lose"
	CPMsgExchangeKeep CPMessageType = "cp_exchange_keep"
	CPMsgReact        CPMessageType = "cp_react"

	// 서버 → 클라이언트
	CPMsgPlayerJoined       CPMessageType = "cp_player_joined"
	CPMsgSpectateJoined     CPMessageType = "cp_spectate_joined"
	CPMsgGameState          CPMessageType = "cp_game_state"
	CPMsgEvent              CPMessageType = "cp_event"
	CPMsgGameOver           CPMessageType = "cp_game_over"
	CPMsgPlayerDisconnected CPMessageType = "cp_player_disconnected"
	CPMsgPlayerReconnected  CPMessageType = "cp_player_reconnected"
	CPMsgSessionExpired     CPMessageType = "cp_session_expired"
	CPMsgError              CPMessageType = "cp_error"
)

// CPCard 카드 한 장. 잃으면 공개(Revealed)로 뒤집혀 전원에게 보인다.
type CPCard struct {
	Role     CPRole
	Revealed bool
}

// CPPlayer 게임 참가자 (순수 상태 — 연결 매핑은 허브의 cpRoom 담당)
type CPPlayer struct {
	Seat  int
	Name  string
	Chips int
	Cards []CPCard // 2장 고정 — 공개 여부만 바뀐다
}

// HiddenIdx 비공개 카드의 실제 슬롯 인덱스 목록 (cp_lose 의 index 는
// 이 목록 기준 — yourCards 와 같은 순서다)
func (p *CPPlayer) HiddenIdx() []int {
	idx := []int{}
	for i, c := range p.Cards {
		if !c.Revealed {
			idx = append(idx, i)
		}
	}
	return idx
}

// HiddenRoles 비공개 카드 역할 목록 (본인 스냅샷·교환 선택지용)
func (p *CPPlayer) HiddenRoles() []CPRole {
	roles := []CPRole{}
	for _, c := range p.Cards {
		if !c.Revealed {
			roles = append(roles, c.Role)
		}
	}
	return roles
}

// Alive 비공개 카드가 1장이라도 남았는지 (2장 다 잃으면 탈락)
func (p *CPPlayer) Alive() bool {
	return len(p.HiddenIdx()) > 0
}

// HasHidden 비공개 카드 중 해당 역할 보유 여부 (챌린지 판정)
func (p *CPPlayer) HasHidden(role CPRole) bool {
	for _, c := range p.Cards {
		if !c.Revealed && c.Role == role {
			return true
		}
	}
	return false
}

// cpAfter 카드 제거(lose_card)가 끝난 뒤의 진행 방향
type cpAfter int

const (
	cpAfterNextTurn cpAfter = iota // 턴 종료 (액션 취소·효과 완료)
	cpAfterProceed                 // 주장 입증(도전 실패) → 차단 창 또는 해결 재개
	cpAfterResolve                 // 차단 무효(차단 도전 성공) → 액션 해결
)

// CPPending 선언된 액션과 응답 창의 진행 상태
type CPPending struct {
	Kind        CPActionKind
	ActorSeat   int
	TargetSeat  int    // -1 없음
	ClaimRole   CPRole // 액션의 주장 역할 ("" 주장 없음)
	BlockerSeat int    // -1 없음
	BlockRole   CPRole // 차단의 주장 역할 ("" 없음)
	Message     string // 현재 대기 상황 요약 (스냅샷 pending.message)

	// responders 현재 창에서 응답해야 하는 좌석 (창이 열릴 때의 생존자 기준)
	responders map[int]bool
	// allowed 허용을 누른 좌석 — 전원 허용이면 창 통과
	allowed map[int]bool
}

// CPGameEvent 순수 규칙이 쌓는 연출 이벤트 — 허브가 DrainEvents 로 꺼내
// cp_event 로 방송한다 (비밀 정보를 담지 않는다)
type CPGameEvent struct {
	Kind    string
	Seat    int // -1 없음
	Message string
}

// CPGame 쿠 게임 상태 (순수, 허브 비의존)
type CPGame struct {
	ID      string
	Players []*CPPlayer
	Phase   CPPhase
	Deck    []CPRole

	CurrentSeat int        // 액션 차례 좌석 (창 동안에도 유지, -1 없음)
	Pending     *CPPending // 진행 중인 액션 (action 단계·종료 시 nil)
	LoseSeat    int        // 카드 선택 중인 좌석 (-1 없음)
	LoseAfter   cpAfter    // 제거 후 진행 방향
	// ExchangeCards 교환 선택지 (본인 비공개 + 덱 2장) — exchange_pick 동안만
	ExchangeCards []CPRole

	WinnerSeat int // 최후 1인 (-1 미정)
	Ready      bool
	StartedAt  time.Time

	// StateSeq 응답 대기 상태(턴·창·제거·교환)가 새로 열릴 때마다 +1 —
	// 허브가 마감 타이머를 다시 걸지 판단하는 근거
	StateSeq int
	// AfkSeq 마감 타이머 일련번호 (뒤늦은 발화 무시용 — 허브가 관리)
	AfkSeq int
	// Deadline 현재 대기 상태의 마감 시각 (unixMillis — 스냅샷 노출용)
	Deadline int64

	events []CPGameEvent
}

// CPClient 쿠 클라이언트 연결
type CPClient struct {
	wsClient
	Hub  *CPHub
	Seat int
}

// CPMessage 메시지 봉투
type CPMessage struct {
	Type    CPMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type CPJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type CPRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// CPActionPayload 액션 선언. TargetSeat 은 좌석 0과 생략을 구분하기 위한
// 포인터다 (coup/assassinate/steal 만 사용).
type CPActionPayload struct {
	Kind       string `json:"kind"`
	TargetSeat *int   `json:"targetSeat,omitempty"`
}

// CPBlockPayload 차단 선언 (duke/contessa/captain/ambassador)
type CPBlockPayload struct {
	Role string `json:"role"`
}

// CPLosePayload 제거할 내 카드 선택 — index 는 yourCards(비공개 목록) 기준
type CPLosePayload struct {
	Index int `json:"index"`
}

// CPExchangeKeepPayload 교환 시 유지할 카드 — indices 는 yourExchange 기준
type CPExchangeKeepPayload struct {
	Indices []int `json:"indices"`
}

type CPReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

// CPPlayerView 좌석별 공개 정보 — 비공개 카드는 장수(cardCount)만,
// 내용은 절대 싣지 않는다. 좌석 0·칩 0 유실 방지를 위해 omitempty 금지.
type CPPlayerView struct {
	Seat      int      `json:"seat"`
	Name      string   `json:"name"`
	Connected bool     `json:"connected"`
	Bot       bool     `json:"bot"`
	Alive     bool     `json:"alive"`
	Chips     int      `json:"chips"`
	Revealed  []string `json:"revealed"` // 공개로 뒤집힌 카드 role (빈 배열 보장)
	CardCount int      `json:"cardCount"`
}

// CPPendingView 진행 중인 액션 요약 (전원 공통 — 주장 역할은 공개 정보다)
type CPPendingView struct {
	Kind        string `json:"kind"`
	ActorSeat   int    `json:"actorSeat"`
	TargetSeat  int    `json:"targetSeat"`  // -1 없음
	BlockerSeat int    `json:"blockerSeat"` // -1 없음
	BlockRole   string `json:"blockRole"`   // "" 없음
	Message     string `json:"message"`
}

// CPGameStatePayload 개인화된 전체 게임 스냅샷. 모든 상태 변경 후 좌석마다
// 따로 만들어 보낸다. 재접속 복원도 같은 페이로드를 쓴다.
// 은닉: yourCards/yourExchange 는 본인에게만 내용이 실린다 (타인·관전자는
// 빈 배열 — null 이 아니라 []).
type CPGameStatePayload struct {
	GameID   string  `json:"gameId"`
	RoomCode string  `json:"roomCode"`
	Phase    CPPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"` // 관전자는 -1
	// Spectators 관전자 수 (참가자·관전자 모두 표시용)
	Spectators int `json:"spectators"`
	// EndsAt 현재 대기 상태(턴·창·제거·교환)의 마감 시각 (unixMillis, 그 외 0)
	EndsAt       int64          `json:"endsAt"`
	CurrentSeat  int            `json:"currentSeat"`
	Pending      *CPPendingView `json:"pending"`  // 진행 중 액션 (없으면 null)
	LoseSeat     int            `json:"loseSeat"` // 카드 선택 중인 좌석 (-1 없음)
	YourCards    []string       `json:"yourCards"`
	YourExchange []string       `json:"yourExchange"`
	Players      []CPPlayerView `json:"players"`
	DeckCount    int            `json:"deckCount"`
}

// CPEventPayload 연출용 이벤트. 비밀 정보를 담지 않으며 전원에게 동일하게
// 간다. Seat 은 좌석 0 유실 방지를 위해 포인터로 생략을 표현한다.
type CPEventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

// CPGameOverPayload 게임 종료 발표 (최후 1인)
type CPGameOverPayload struct {
	WinnerSeat int            `json:"winnerSeat"`
	WinnerName string         `json:"winnerName"`
	Players    []CPPlayerView `json:"players"`
}

type CPPlayerDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type CPPlayerReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type CPErrorPayload struct {
	Message string `json:"message"`
}
