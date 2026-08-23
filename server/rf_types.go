package server

import "time"

// ==================== 쿠: 리포메이션 타입 ====================
//
// 2~10인 블러핑 정체 은닉. 기본 쿠(cp_*)의 규칙(역할 5종·비공개 카드 2장·
// 칩·도전/차단 응답 창·칩 10개 쿠 강제·카드 2장 손실 탈락)을 그대로 옮기고
// 리포메이션 확장(진영·국고·개종·횡령·같은 진영 공격 금지·진영 승리)을
// 얹었다. 운영 중인 쿠와는 완전히 독립된 게임이라 cp_* 는 읽기만 했다.
//
// 은닉의 계약: yourRoles·yourExchange 는 본인 스냅샷에만 키가 존재한다
// (타인·관전자 raw JSON 에는 키 자체가 없다 — 포인터 슬라이스 + omitempty).
// faction 과 lostRoles(잃어서 공개된 카드)는 전원 공개다.

const (
	RFMinPlayers = 2
	RFMaxPlayers = 10

	// RFFillBotTarget rf_fill_bots 가 채우는 목표 인원 — 채운 뒤 즉시 시작
	RFFillBotTarget = 5

	RFStartChips     = 2  // 시작 칩
	RFCoupCost       = 7  // 쿠 비용
	RFAssassinCost   = 3  // 암살 비용
	RFForceCoupChips = 10 // 이 이상 보유하면 쿠 강제
	RFStealMax       = 2  // 강탈 상한 (대상 칩이 모자라면 있는 만큼)
	RFCardsPerPlayer = 2

	// 덱 장수는 인원에 맞춘다 — 6인 이하는 역할당 3장(15장),
	// 7인 이상은 역할당 4장(20장)
	RFRoleCopiesSmall = 3
	RFRoleCopiesLarge = 4
	RFSmallTableMax   = 6

	// 개종 비용 — 지불한 칩은 전액 국고로 들어간다
	RFConvertSelfCost  = 1
	RFConvertOtherCost = 2
)

// RFRole 역할 카드 (와이어 값 — 프론트와 공유)
type RFRole string

const (
	RFRoleDuke       RFRole = "duke"
	RFRoleAssassin   RFRole = "assassin"
	RFRoleCaptain    RFRole = "captain"
	RFRoleAmbassador RFRole = "ambassador"
	RFRoleContessa   RFRole = "contessa"
)

// rfAllRoles 덱 구성 순서 (셔플 전 기준)
var rfAllRoles = []RFRole{RFRoleDuke, RFRoleAssassin, RFRoleCaptain, RFRoleAmbassador, RFRoleContessa}

// rfRoleName 한글 역할명 (이벤트 문구용)
func rfRoleName(r RFRole) string {
	switch r {
	case RFRoleDuke:
		return "공작"
	case RFRoleAssassin:
		return "암살자"
	case RFRoleCaptain:
		return "사령관"
	case RFRoleAmbassador:
		return "대사"
	case RFRoleContessa:
		return "백작부인"
	}
	return string(r)
}

// RFFaction 진영 (전원 공개 정보)
type RFFaction string

const (
	RFFactionLoyalist  RFFaction = "loyalist"
	RFFactionReformist RFFaction = "reformist"
)

// rfFactionName 한글 진영명 (이벤트 문구용)
func rfFactionName(f RFFaction) string {
	switch f {
	case RFFactionLoyalist:
		return "충성파"
	case RFFactionReformist:
		return "개혁파"
	}
	return string(f)
}

// rfFlipFaction 진영 뒤집기 (개종)
func rfFlipFaction(f RFFaction) RFFaction {
	if f == RFFactionLoyalist {
		return RFFactionReformist
	}
	return RFFactionLoyalist
}

// RFActionKind 차례 행동 (와이어 값). 앞의 7종은 rf_action 으로,
// 개종 2종·횡령은 각자의 전용 메시지로 들어온다.
type RFActionKind string

const (
	RFActIncome       RFActionKind = "income"
	RFActAid          RFActionKind = "aid"
	RFActCoup         RFActionKind = "coup"
	RFActTax          RFActionKind = "tax"
	RFActAssassinate  RFActionKind = "assassinate"
	RFActSteal        RFActionKind = "steal"
	RFActExchange     RFActionKind = "exchange"
	RFActConvert      RFActionKind = "convert"       // 자기 진영 바꾸기 (즉시 발동)
	RFActConvertOther RFActionKind = "convert_other" // 남의 진영 바꾸기 (즉시 발동)
	RFActEmbezzle     RFActionKind = "embezzle"      // 횡령 — "나는 공작이 아니다" 주장
)

// rfActionName 한글 행동명 (이벤트 문구용)
func rfActionName(k RFActionKind) string {
	switch k {
	case RFActIncome:
		return "수입"
	case RFActAid:
		return "해외원조"
	case RFActCoup:
		return "쿠"
	case RFActTax:
		return "세금"
	case RFActAssassinate:
		return "암살"
	case RFActSteal:
		return "강탈"
	case RFActExchange:
		return "교환"
	case RFActConvert:
		return "진영 바꾸기"
	case RFActConvertOther:
		return "남의 진영 바꾸기"
	case RFActEmbezzle:
		return "횡령"
	}
	return string(k)
}

// rfActionClaim 역할 주장이 붙는 액션 → 주장 역할 (도전 대상).
// 수입·해외원조·쿠·개종은 주장이 없어 도전할 수 없다. 횡령은 "공작이
// 아니다"라는 역(逆)주장이라 별도로 다룬다(rfEmbezzleClaim).
var rfActionClaim = map[RFActionKind]RFRole{
	RFActTax:         RFRoleDuke,
	RFActAssassinate: RFRoleAssassin,
	RFActSteal:       RFRoleCaptain,
	RFActExchange:    RFRoleAmbassador,
}

// rfEmbezzleClaim 횡령이 걸고 있는 역주장 대상 역할 (공작 부재 증명)
const rfEmbezzleClaim = RFRoleDuke

// rfBlockRoles 액션별 차단 가능 역할. 해외원조는 아무나(같은 진영도 가능),
// 암살·강탈은 대상만 차단할 수 있다 (SubmitBlock 에서 판정).
var rfBlockRoles = map[RFActionKind][]RFRole{
	RFActAid:         {RFRoleDuke},
	RFActAssassinate: {RFRoleContessa},
	RFActSteal:       {RFRoleCaptain, RFRoleAmbassador},
}

// rfAttackKinds 같은 진영에게 쓸 수 없는 공격 3종
var rfAttackKinds = map[RFActionKind]bool{
	RFActCoup:        true,
	RFActAssassinate: true,
	RFActSteal:       true,
}

// rfSameFactionMsg 같은 진영 공격 거부 문구 (테스트가 문자열로 검증한다)
const rfSameFactionMsg = "같은 진영은 공격할 수 없습니다"

// RFPhase 게임 진행 단계 (와이어 값).
//
// 기본 쿠의 "차단 도전 창"은 별도 phase 를 두지 않고 challenge_window 를
// 재사용한다 — pending.blockerSeat 이 -1 이면 액션 주장 도전, 0 이상이면
// 선언된 차단에 대한 도전이다. 프론트 계약의 phase 집합을 좁게 유지한다.
type RFPhase string

const (
	RFPhaseWaiting         RFPhase = "waiting"
	RFPhaseAction          RFPhase = "action"           // 현재 차례가 행동 선택 중
	RFPhaseChallengeWindow RFPhase = "challenge_window" // 역할 주장 도전 창 (액션·차단 공용)
	RFPhaseBlockWindow     RFPhase = "block_window"     // 차단 선언 창 (해당 액션만)
	RFPhaseLoseCard        RFPhase = "lose_card"        // 당사자가 잃을 카드 선택 중
	RFPhaseExchange        RFPhase = "exchange"         // 교환 — 유지 카드 선택 중
	RFPhaseGameOver        RFPhase = "game_over"
)

// RFMessageType 리포메이션 메시지 타입
type RFMessageType string

const (
	// 클라이언트 → 서버
	RFMsgJoinGame     RFMessageType = "rf_join_game"
	RFMsgFillBots     RFMessageType = "rf_fill_bots"
	RFMsgStart        RFMessageType = "rf_start"
	RFMsgRejoin       RFMessageType = "rf_rejoin"
	RFMsgAction       RFMessageType = "rf_action"
	RFMsgConvert      RFMessageType = "rf_convert"
	RFMsgConvertOther RFMessageType = "rf_convert_other"
	RFMsgEmbezzle     RFMessageType = "rf_embezzle"
	RFMsgChallenge    RFMessageType = "rf_challenge"
	RFMsgBlock        RFMessageType = "rf_block"
	RFMsgPass         RFMessageType = "rf_pass"
	RFMsgLoseCard     RFMessageType = "rf_lose_card"
	RFMsgExchange     RFMessageType = "rf_exchange"
	RFMsgReact        RFMessageType = "rf_react"

	// 서버 → 클라이언트
	RFMsgPlayerJoined       RFMessageType = "rf_player_joined"
	RFMsgSpectateJoined     RFMessageType = "rf_spectate_joined"
	RFMsgGameState          RFMessageType = "rf_game_state"
	RFMsgEvent              RFMessageType = "rf_event"
	RFMsgGameOver           RFMessageType = "rf_game_over"
	RFMsgPlayerDisconnected RFMessageType = "rf_player_disconnected"
	RFMsgPlayerReconnected  RFMessageType = "rf_player_reconnected"
	RFMsgSessionExpired     RFMessageType = "rf_session_expired"
	RFMsgError              RFMessageType = "rf_error"
)

// RFCard 카드 한 장. 잃으면 공개(Revealed)로 뒤집혀 전원에게 보인다.
type RFCard struct {
	Role     RFRole
	Revealed bool
}

// RFPlayer 게임 참가자 (순수 상태 — 연결 매핑은 허브의 rfRoom 담당)
type RFPlayer struct {
	Seat    int
	Name    string
	Chips   int
	Faction RFFaction // 전원 공개 — 개종으로 뒤집힌다
	Cards   []RFCard  // 2장 고정 — 공개 여부만 바뀐다
}

// HiddenIdx 비공개 카드의 실제 슬롯 인덱스 목록 (rf_lose_card 의 index 는
// 이 목록 기준 — yourRoles 와 같은 순서다)
func (p *RFPlayer) HiddenIdx() []int {
	idx := []int{}
	for i, c := range p.Cards {
		if !c.Revealed {
			idx = append(idx, i)
		}
	}
	return idx
}

// HiddenRoles 비공개 카드 역할 목록 (본인 스냅샷·교환 선택지용)
func (p *RFPlayer) HiddenRoles() []RFRole {
	roles := []RFRole{}
	for _, c := range p.Cards {
		if !c.Revealed {
			roles = append(roles, c.Role)
		}
	}
	return roles
}

// LostRoles 잃어서 공개된 카드 역할 목록 (전원 공개)
func (p *RFPlayer) LostRoles() []string {
	roles := []string{}
	for _, c := range p.Cards {
		if c.Revealed {
			roles = append(roles, string(c.Role))
		}
	}
	return roles
}

// Alive 비공개 카드가 1장이라도 남았는지 (2장 다 잃으면 탈락)
func (p *RFPlayer) Alive() bool {
	return len(p.HiddenIdx()) > 0
}

// HasHidden 비공개 카드 중 해당 역할 보유 여부 (도전 판정)
func (p *RFPlayer) HasHidden(role RFRole) bool {
	for _, c := range p.Cards {
		if !c.Revealed && c.Role == role {
			return true
		}
	}
	return false
}

// rfAfter 카드 제거(lose_card)가 끝난 뒤의 진행 방향
type rfAfter int

const (
	rfAfterNextTurn rfAfter = iota // 턴 종료 (액션 취소·효과 완료)
	rfAfterProceed                 // 주장 입증(도전 실패) → 차단 창 또는 해결 재개
	rfAfterResolve                 // 차단 무효(차단 도전 성공) → 액션 해결
)

// RFPending 선언된 액션과 응답 창의 진행 상태
type RFPending struct {
	Kind        RFActionKind
	ActorSeat   int
	TargetSeat  int    // -1 없음
	ClaimRole   RFRole // 액션의 주장 역할 ("" 주장 없음)
	BlockerSeat int    // -1 없음 (0 이상이면 challenge_window 는 차단 도전 창)
	BlockRole   RFRole // 차단의 주장 역할 ("" 없음)
	Message     string // 현재 대기 상황 요약 (스냅샷 pending.message)

	// responders 현재 창에서 응답해야 하는 좌석 (창이 열릴 때의 생존자 기준)
	responders map[int]bool
	// passed 통과(허용)를 누른 좌석 — 전원 통과면 창이 닫힌다
	passed map[int]bool
}

// RFGameEvent 순수 규칙이 쌓는 연출 이벤트 — 허브가 DrainEvents 로 꺼내
// rf_event 로 방송한다 (비밀 정보를 담지 않는다)
type RFGameEvent struct {
	Kind    string
	Seat    int // -1 없음
	Message string
}

// RFLastAction 마지막 행동 요약 (스냅샷 lastAction)
type RFLastAction struct {
	Seat    int
	Name    string
	Message string
}

// RFResult 승부 결과. 최후 1인이면 Winner="seat", 진영 승리면 진영 값.
type RFResult struct {
	Winner      string // "seat" | "loyalist" | "reformist"
	WinnerSeats []int
	WinnerNames []string
	Message     string
}

// RFGame 리포메이션 게임 상태 (순수, 허브 비의존)
type RFGame struct {
	ID      string
	Players []*RFPlayer
	Phase   RFPhase
	Deck    []RFRole

	// Treasury 국고 — 개종 비용이 쌓이고 횡령이 전액을 가져간다
	Treasury int

	CurrentSeat int        // 행동 차례 좌석 (창 동안에도 유지, -1 없음)
	Pending     *RFPending // 진행 중인 액션 (action 단계·종료 시 nil)
	LoseSeat    int        // 카드 선택 중인 좌석 (-1 없음)
	LoseAfter   rfAfter    // 제거 후 진행 방향
	// ExchangeCards 교환 선택지 (본인 비공개 + 덱에서 뽑은 만큼) — exchange 동안만
	ExchangeCards []RFRole

	LastAction *RFLastAction // 마지막 행동 요약 (없으면 nil)
	Result     *RFResult     // 종료 결과 (진행 중 nil)

	Ready     bool
	StartedAt time.Time

	// StateSeq 응답 대기 상태(턴·창·제거·교환)가 새로 열릴 때마다 +1 —
	// 허브가 마감 타이머를 다시 걸지 판단하는 근거
	StateSeq int
	// AfkSeq 마감 타이머 일련번호 (뒤늦은 발화 무시용 — 허브가 관리)
	AfkSeq int
	// Deadline 현재 대기 상태의 마감 시각 (unixMillis — 스냅샷 노출용)
	Deadline int64

	events []RFGameEvent
}

// RFClient 리포메이션 클라이언트 연결
type RFClient struct {
	wsClient
	Hub  *RFHub
	Seat int
}

// RFMessage 메시지 봉투
type RFMessage struct {
	Type    RFMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type RFJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type RFRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// RFActionPayload 액션 선언. TargetSeat 은 좌석 0과 생략을 구분하기 위한
// 포인터다 (coup/assassinate/steal 만 사용).
type RFActionPayload struct {
	Kind       string `json:"kind"`
	TargetSeat *int   `json:"targetSeat,omitempty"`
}

// RFConvertOtherPayload 남의 진영 바꾸기 대상 (좌석 0 구분을 위해 포인터)
type RFConvertOtherPayload struct {
	TargetSeat *int `json:"targetSeat,omitempty"`
}

// RFBlockPayload 차단 선언 (duke/contessa/captain/ambassador)
type RFBlockPayload struct {
	Role string `json:"role"`
}

// RFLoseCardPayload 제거할 내 카드 선택 — index 는 yourRoles(비공개 목록) 기준
type RFLoseCardPayload struct {
	Index int `json:"index"`
}

// RFExchangePayload 교환 시 유지할 카드 — keep 은 yourExchange 기준
type RFExchangePayload struct {
	Keep []int `json:"keep"`
}

type RFReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

// RFPlayerView 좌석별 공개 정보 — 비공개 카드는 장수(cardCount)만, 내용은
// 절대 싣지 않는다. 진영(faction)과 잃은 카드(lostRoles)는 전원 공개다.
// 좌석 0·칩 0 유실 방지를 위해 int 필드는 omitempty 금지.
type RFPlayerView struct {
	Seat      int       `json:"seat"`
	Name      string    `json:"name"`
	Connected bool      `json:"connected"`
	Bot       bool      `json:"bot"`
	Coins     int       `json:"coins"`
	Alive     bool      `json:"alive"`
	Faction   RFFaction `json:"faction"`
	LostRoles []string  `json:"lostRoles"` // 잃어서 공개된 카드 (빈 배열 보장)
	CardCount int       `json:"cardCount"`
}

// RFPendingView 진행 중인 액션 요약 (전원 공통 — 주장 역할은 공개 정보다).
// blockerSeat·message 는 계약에 더한 보조 필드다 (차단 도전 창의 주체와
// 대기 문구를 프론트가 알아야 한다).
type RFPendingView struct {
	Kind        string `json:"kind"`
	BySeat      int    `json:"bySeat"`
	TargetSeat  int    `json:"targetSeat"`  // -1 없음
	ClaimRole   string `json:"claimRole"`   // "" 없음
	BlockRole   string `json:"blockRole"`   // "" 없음
	BlockerSeat int    `json:"blockerSeat"` // -1 없음
	Passed      []int  `json:"passed"`      // 통과를 누른 좌석 (빈 배열 보장)
	Message     string `json:"message"`
}

// RFLastActionView 마지막 행동 요약
type RFLastActionView struct {
	Seat    int    `json:"seat"`
	Name    string `json:"name"`
	Message string `json:"message"`
}

// RFResultView 승부 결과 (진영 승리 표현 포함)
type RFResultView struct {
	Winner      string   `json:"winner"` // "seat" | "loyalist" | "reformist"
	WinnerSeats []int    `json:"winnerSeats"`
	WinnerNames []string `json:"winnerNames"`
	Message     string   `json:"message"`
}

// RFGameStatePayload 개인화된 전체 게임 스냅샷. 모든 상태 변경 후 좌석마다
// 따로 만들어 보낸다. 재접속 복원도 같은 페이로드를 쓴다.
//
// 은닉: YourRoles/YourExchange 는 포인터 슬라이스라 본인이 아닌 뷰어에게는
// 키 자체가 사라진다(omitempty). 본인에게는 비어 있어도 [] 로 실린다.
// loseSeat·deckCount 는 계약에 더한 보조 필드다.
type RFGameStatePayload struct {
	GameID   string  `json:"gameId"`
	RoomCode string  `json:"roomCode"`
	Phase    RFPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"` // 관전자는 -1
	// Spectators 관전자 수 (참가자·관전자 모두 표시용)
	Spectators int `json:"spectators"`
	// EndsAt 현재 대기 상태(턴·창·제거·교환)의 마감 시각 (unixMillis, 그 외 0)
	EndsAt      int64 `json:"endsAt"`
	CurrentSeat int   `json:"currentSeat"`
	Treasury    int   `json:"treasury"`

	YourRoles    *[]string `json:"yourRoles,omitempty"`    // 본인만 — 살아 있는 카드
	YourExchange *[]string `json:"yourExchange,omitempty"` // exchange 단계 본인만

	Pending    *RFPendingView    `json:"pending"`
	Players    []RFPlayerView    `json:"players"`
	LastAction *RFLastActionView `json:"lastAction"`
	Result     *RFResultView     `json:"result"`

	LoseSeat  int `json:"loseSeat"` // 카드 선택 중인 좌석 (-1 없음)
	DeckCount int `json:"deckCount"`
}

// RFEventPayload 연출용 이벤트. 비밀 정보를 담지 않으며 전원에게 동일하게
// 간다. Seat 은 좌석 0 유실 방지를 위해 포인터로 생략을 표현한다.
type RFEventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

// RFGameOverPayload 게임 종료 발표 (최후 1인 또는 진영 승리)
type RFGameOverPayload struct {
	Winner      string         `json:"winner"`
	WinnerSeats []int          `json:"winnerSeats"`
	WinnerNames []string       `json:"winnerNames"`
	Message     string         `json:"message"`
	Players     []RFPlayerView `json:"players"`
}

type RFPlayerDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type RFPlayerReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type RFErrorPayload struct {
	Message string `json:"message"`
}
