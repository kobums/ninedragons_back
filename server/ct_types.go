package server

import "time"

// ==================== 시타델 (Citadels) 타입 ====================
//
// 3~7인 역할 드래프트 + 건물 경제. 한 라운드는 "직업 선택 → 1~8번 호출"의
// 2단계이고, 호출된 사람의 차례는 "① 자원 → ② 건설 → ③ 직업 능력" 순서로
// 열린다. 누군가 건물 7채를 완성하면 그 라운드를 끝까지 진행하고 끝난다.
//
// 용어는 정식 한국어판을 그대로 쓴다 — 직업 8종은
// 암살자 · 도둑 · 마술사 · 왕 · 주교 · 상인 · 건축가 · 장군 이고
// (마법사·사제·영주로 옮기지 않는다), 건물 색은
// 귀족 · 종교 · 상업 · 군사 · 특수 다. 금화 · 왕관 · 건물 카드 · 건설 · 승점.
// 와이어에 실리는 영문 색 값(noble·religion·trade·military·unique)은 고정이며
// 화면 표기만 한국어를 쓴다.
//
// 보라(특수) 건물은 이번 판에서 특수 능력 없이 승점만 준다 (스펙의 단순화 —
// 능력 카드까지 넣으면 범위가 폭발한다).
//
// 은닉의 심장은 ct_hub.go 의 buildCTState 다. yourRole · yourHand · yourDraw ·
// pickPool 은 본인 스냅샷에만 실리고, 타인·관전자(viewerSeat -1)의 raw JSON
// 에는 키 자체가 없다. 남의 직업은 호출로 공개되기 전까지 roleRevealed 0 이며,
// 뒷면으로 제외된 직업(FaceDown)은 어떤 스냅샷에도 실리지 않는다.

const (
	CTMinPlayers = 3
	CTMaxPlayers = 7

	// CTFillBotTarget ct_fill_bots 가 채우는 목표 인원 — 채운 뒤 즉시 시작
	CTFillBotTarget = 4

	// CTHandStart / CTGoldStart 시작 손패·금화
	CTHandStart = 4
	CTGoldStart = 2

	// CTGatherGold 자원으로 금화를 고를 때 받는 금화
	CTGatherGold = 2
	// CTGatherDraw 자원으로 카드를 고를 때 뽑는 장수 (1장만 남긴다)
	CTGatherDraw = 2

	// CTBuildTarget 도시 완성 기준 — 이 채수에 먼저 닿으면 그 라운드로 끝난다
	CTBuildTarget = 7

	// CTBuildsNormal / CTBuildsArchitect 한 차례에 지을 수 있는 건물 수
	CTBuildsNormal    = 1
	CTBuildsArchitect = 3

	// CTArchitectDraw 건축가가 차례 시작에 추가로 뽑는 장수
	CTArchitectDraw = 2
	// CTMerchantGold 상인이 차례 시작에 추가로 받는 금화
	CTMerchantGold = 1

	// 점수 보너스 — 7채 먼저 완성 4점 · 7채 완성(1등 외) 2점 · 다섯 색 3점
	CTBonusFirst     = 4
	CTBonusComplete  = 2
	CTBonusAllColors = 3

	// CTMaxRounds 안전 상한. 규칙상 도시가 7채로 차지만, 전원이 금화만 받는
	// 병리적 진행에서도 판이 끝나도록 두는 방어선이다.
	CTMaxRounds = 40

	// CTRoleCount 직업 장수 (1~8번)
	CTRoleCount = 8
)

// ==================== 직업 8종 (번호 = 호출 순서) ====================

const (
	CTRoleAssassin  = 1 // 암살자
	CTRoleThief     = 2 // 도둑
	CTRoleMagician  = 3 // 마술사
	CTRoleKing      = 4 // 왕
	CTRoleBishop    = 5 // 주교
	CTRoleMerchant  = 6 // 상인
	CTRoleArchitect = 7 // 건축가
	CTRoleWarlord   = 8 // 장군
)

// ctRoleNames 1~8번 직업의 정식 한국어 표기 (인덱스 0은 자리 채움)
var ctRoleNames = [CTRoleCount + 1]string{
	"", "암살자", "도둑", "마술사", "왕", "주교", "상인", "건축가", "장군",
}

// ctRoleName 직업 번호 → 한국어 이름 ("?" 는 범위 밖)
func ctRoleName(role int) string {
	if role < 1 || role > CTRoleCount {
		return "?"
	}
	return ctRoleNames[role]
}

// ctRoleValid 1~8번인가
func ctRoleValid(role int) bool { return role >= 1 && role <= CTRoleCount }

// ctRoleIncomeColor 그 직업이 금화를 걷는 건물 색 ("" 는 수입 없음).
// 왕 노랑(귀족) · 주교 파랑(종교) · 상인 초록(상업) · 장군 빨강(군사).
func ctRoleIncomeColor(role int) CTColor {
	switch role {
	case CTRoleKing:
		return CTNoble
	case CTRoleBishop:
		return CTReligion
	case CTRoleMerchant:
		return CTTrade
	case CTRoleWarlord:
		return CTMilitary
	}
	return ""
}

// ctRoleHasAbility 차례 끝에 능력 단계(ability)를 여는 직업인가.
// 암살자·도둑·마술사·장군만 대상을 골라야 하고, 나머지 넷(왕·주교·상인·
// 건축가)의 능력은 차례 시작에 자동으로 적용된다.
func ctRoleHasAbility(role int) bool {
	switch role {
	case CTRoleAssassin, CTRoleThief, CTRoleMagician, CTRoleWarlord:
		return true
	}
	return false
}

// ctBuildLimit 그 직업이 이 차례에 지을 수 있는 건물 수 (건축가만 3채)
func ctBuildLimit(role int) int {
	if role == CTRoleArchitect {
		return CTBuildsArchitect
	}
	return CTBuildsNormal
}

// ctFaceUpCount 인원별 앞면 제외 장수 — 3인 3장 · 4인 2장 · 5인 1장 ·
// 6인 이상 0장. 뒷면 제외 1장은 인원과 무관하게 항상 추가된다.
func ctFaceUpCount(n int) int {
	if c := 6 - n; c > 0 {
		return c
	}
	return 0
}

// ==================== 건물 색 ====================

// CTColor 건물 색 (와이어 영문 값 고정 — 화면 표기만 한국어)
type CTColor string

const (
	CTNoble    CTColor = "noble"    // 노랑(귀족)
	CTReligion CTColor = "religion" // 파랑(종교)
	CTTrade    CTColor = "trade"    // 초록(상업)
	CTMilitary CTColor = "military" // 빨강(군사)
	CTUnique   CTColor = "unique"   // 보라(특수)
)

// ctColors 다섯 색. 이 다섯을 모두 갖추면 3점이다.
var ctColors = [5]CTColor{CTNoble, CTReligion, CTTrade, CTMilitary, CTUnique}

// ctColorLabel 색의 한국어 표기
func ctColorLabel(c CTColor) string {
	switch c {
	case CTNoble:
		return "귀족"
	case CTReligion:
		return "종교"
	case CTTrade:
		return "상업"
	case CTMilitary:
		return "군사"
	case CTUnique:
		return "특수"
	}
	return "?"
}

// ==================== 건물 카드 ====================

// CTCard 건물 카드 한 장. Name 이 같은 건물은 한 사람이 두 번 지을 수 없다.
type CTCard struct {
	ID    int     `json:"id"`
	Name  string  `json:"name"`
	Color CTColor `json:"color"`
	Cost  int     `json:"cost"`
}

// ctBuildingDef 덱 구성표 한 줄 (이름·색·값·장수)
type ctBuildingDef struct {
	Name  string
	Color CTColor
	Cost  int
	Count int
}

// ctBuildings 건물 카드 덱 구성표 — 합 65장. 값은 1~5.
//
//	귀족   12장 (저택3×5 · 성4×4 · 궁전5×3)
//	종교   11장 (사원1×3 · 교회2×3 · 수도원3×3 · 대성당5×2)
//	상업   18장 (여관1×3 · 시장2×4 · 무역소2×3 · 부두3×3 · 항구4×3 · 시청5×2)
//	군사   11장 (파수탑1×3 · 감옥2×3 · 병영3×3 · 요새5×2)
//	특수   13장 (유령의 거리2×2 · 병기고3×2 · 지도 제작실4×2 · 천문대5×2 ·
//	             실험실5×2 · 묘지5×2 · 대장간5×1)
//
// 보라(특수)는 이번 판에서 능력 없이 승점만 준다.
var ctBuildings = []ctBuildingDef{
	// 귀족(노랑)
	{Name: "저택", Color: CTNoble, Cost: 3, Count: 5},
	{Name: "성", Color: CTNoble, Cost: 4, Count: 4},
	{Name: "궁전", Color: CTNoble, Cost: 5, Count: 3},
	// 종교(파랑)
	{Name: "사원", Color: CTReligion, Cost: 1, Count: 3},
	{Name: "교회", Color: CTReligion, Cost: 2, Count: 3},
	{Name: "수도원", Color: CTReligion, Cost: 3, Count: 3},
	{Name: "대성당", Color: CTReligion, Cost: 5, Count: 2},
	// 상업(초록)
	{Name: "여관", Color: CTTrade, Cost: 1, Count: 3},
	{Name: "시장", Color: CTTrade, Cost: 2, Count: 4},
	{Name: "무역소", Color: CTTrade, Cost: 2, Count: 3},
	{Name: "부두", Color: CTTrade, Cost: 3, Count: 3},
	{Name: "항구", Color: CTTrade, Cost: 4, Count: 3},
	{Name: "시청", Color: CTTrade, Cost: 5, Count: 2},
	// 군사(빨강)
	{Name: "파수탑", Color: CTMilitary, Cost: 1, Count: 3},
	{Name: "감옥", Color: CTMilitary, Cost: 2, Count: 3},
	{Name: "병영", Color: CTMilitary, Cost: 3, Count: 3},
	{Name: "요새", Color: CTMilitary, Cost: 5, Count: 2},
	// 특수(보라)
	{Name: "유령의 거리", Color: CTUnique, Cost: 2, Count: 2},
	{Name: "병기고", Color: CTUnique, Cost: 3, Count: 2},
	{Name: "지도 제작실", Color: CTUnique, Cost: 4, Count: 2},
	{Name: "천문대", Color: CTUnique, Cost: 5, Count: 2},
	{Name: "실험실", Color: CTUnique, Cost: 5, Count: 2},
	{Name: "묘지", Color: CTUnique, Cost: 5, Count: 2},
	{Name: "대장간", Color: CTUnique, Cost: 5, Count: 1},
}

// CTDeckSize 덱 총 장수 (구성표의 Count 합과 같아야 한다 — 테스트가 지킨다)
const CTDeckSize = 65

// ==================== 단계 ====================

// CTPhase 게임 진행 단계 (상태기계의 뼈대 — ct_game.go 상단 주석 참고)
type CTPhase string

const (
	CTPhaseWaiting   CTPhase = "waiting"
	CTPhasePickRoles CTPhase = "pick_roles" // 직업 선택 (45초 마감 — 무작위)
	CTPhaseKeepCard  CTPhase = "keep_card"  // 뽑은 2장 중 1장 남기기 (60초)
	CTPhaseTurn      CTPhase = "turn"       // 자원 → 건설 (60초 마감)
	CTPhaseAbility   CTPhase = "ability"    // 직업 능력 (30초 마감 — 사용 안 함)
	CTPhaseGameOver  CTPhase = "game_over"
)

// ==================== 메시지 ====================

// CTMessageType 시타델 메시지 타입
type CTMessageType string

const (
	// 클라이언트 → 서버
	CTMsgJoinGame CTMessageType = "ct_join_game"
	CTMsgFillBots CTMessageType = "ct_fill_bots"
	CTMsgStart    CTMessageType = "ct_start"
	CTMsgRejoin   CTMessageType = "ct_rejoin"
	CTMsgPickRole CTMessageType = "ct_pick_role"
	CTMsgGather   CTMessageType = "ct_gather"
	CTMsgKeep     CTMessageType = "ct_keep"
	CTMsgBuild    CTMessageType = "ct_build"
	CTMsgAbility  CTMessageType = "ct_ability"
	CTMsgEndTurn  CTMessageType = "ct_end_turn"
	CTMsgReact    CTMessageType = "ct_react"

	// 서버 → 클라이언트
	CTMsgPlayerJoined       CTMessageType = "ct_player_joined"
	CTMsgSpectateJoined     CTMessageType = "ct_spectate_joined"
	CTMsgGameState          CTMessageType = "ct_game_state"
	CTMsgEvent              CTMessageType = "ct_event"
	CTMsgGameOver           CTMessageType = "ct_game_over"
	CTMsgPlayerDisconnected CTMessageType = "ct_player_disconnected"
	CTMsgPlayerReconnected  CTMessageType = "ct_player_reconnected"
	CTMsgSessionExpired     CTMessageType = "ct_session_expired"
	CTMsgError              CTMessageType = "ct_error"
)

// CTGatherGoldKind / CTGatherCardsKind ct_gather 의 kind 값 (와이어 고정)
const (
	CTGatherGoldKind  = "gold"
	CTGatherCardsKind = "cards"
)

// ==================== 순수 상태 ====================

// CTPlayer 게임 참가자 (순수 상태 — 연결 매핑은 허브의 ctRoom 담당)
type CTPlayer struct {
	Seat int
	Name string
	// Gold 보유 금화
	Gold int
	// Hand 손에 든 건물 카드 — 본인만 내용을 본다
	Hand []CTCard
	// Built 완성한 도시 (전원 공개)
	Built []CTCard
	// Role 이번 라운드에 쥔 직업 (0 = 미확정) — 호출 전까지 본인만 안다
	Role int
	// RoleRevealed 호출로 공개된 직업 (0 = 비공개)
	RoleRevealed int
	// Killed / Robbed 이번 라운드에 암살·도둑질을 당했는가 (공개 후에만 true)
	Killed bool
	Robbed bool
	// Draw 자원으로 뽑아 놓고 아직 고르지 않은 2장 — 본인만 본다
	Draw []CTCard
	// CompletedRound 도시 7채를 완성한 라운드 (0 = 미완성)
	CompletedRound int
}

// CTGameEvent 순수 규칙이 쌓는 연출 이벤트 — 허브가 DrainEvents 로 꺼내
// ct_event 로 방송한다 (손패·비공개 직업의 내용은 절대 담지 않는다)
type CTGameEvent struct {
	Kind    string
	Seat    int // -1 없음
	Message string
}

// CTLastAction 마지막 행동 요약 (전원 공개)
type CTLastAction struct {
	Seat    int    `json:"seat"`
	Name    string `json:"name"`
	Message string `json:"message"`
}

// CTResultRow 최종 점수 내역 한 줄
type CTResultRow struct {
	Seat   int    `json:"seat"`
	Score  int    `json:"score"`
	Detail string `json:"detail"`
}

// CTResult 종료 결과. 동점 + 건물 수까지 같으면 공동 승이라 좌석이 여럿이다.
type CTResult struct {
	WinnerSeats []int         `json:"winnerSeats"`
	WinnerNames []string      `json:"winnerNames"`
	Rows        []CTResultRow `json:"rows"`
	Message     string        `json:"message"`
}

// CTGame 시타델 게임 상태 (순수, 허브 비의존)
type CTGame struct {
	ID      string
	Players []*CTPlayer
	Phase   CTPhase

	// Deck 남은 건물 카드 (앞에서 뽑고 뒤로 되돌린다)
	Deck []CTCard

	// Round 라운드 번호 (1부터)
	Round int
	// CrownSeat 이번 라운드 왕관(선) 좌석
	CrownSeat int
	// CrownNext 다음 라운드 왕관 좌석 (왕이 호출되면 그 좌석으로 옮겨진다)
	CrownNext int

	// RolePool 직업 선택 단계에서 아직 고를 수 있는 직업 (오름차순)
	RolePool []int
	// FaceUp 앞면으로 제외된 직업 (전원 공개)
	FaceUp []int
	// FaceDown 뒷면으로 제외된 직업 — 어떤 스냅샷·이벤트에도 실리지 않는다
	FaceDown int
	// PickOrder 직업을 고르는 좌석 순서 (왕관 보유자부터)
	PickOrder []int
	// PickIdx PickOrder 에서 지금 고를 차례의 인덱스
	PickIdx int

	// CallingRole 지금 호출 중인 직업 번호 (0 = 직업 선택 단계)
	CallingRole int
	// CurrentSeat 지금 행동할 좌석 (-1 없음)
	CurrentSeat int

	// KilledRole / RobbedRole 이번 라운드에 지목된 직업 (0 = 없음) — 지목은
	// 공개 선언이라 이벤트로 전원에게 알린다
	KilledRole int
	RobbedRole int
	// ThiefSeat 도둑을 쥔 좌석 (-1 없음) — 빼앗은 금화가 갈 곳
	ThiefSeat int

	// 진행 중인 차례의 진척 (자원 → 건설 → 능력)
	GatherDone  bool
	BuildsLeft  int
	AbilityUsed bool

	// LastRound 누군가 7채를 완성해 이번 라운드로 끝나는가
	LastRound bool
	// FirstCompleteSeat 7채를 가장 먼저 완성한 좌석 (-1 없음)
	FirstCompleteSeat int

	LastAction *CTLastAction
	Result     *CTResult
	Ready      bool
	StartedAt  time.Time

	// StateSeq 새 대기 상태가 열릴 때마다 +1 — 허브가 마감 타이머를 다시
	// 걸지 판단하는 근거
	StateSeq int
	// AfkSeq 마감 타이머 일련번호 (뒤늦은 발화 무시용 — 허브가 관리)
	AfkSeq int
	// Deadline 현재 대기 상태의 마감 시각 (unixMillis — 스냅샷 노출용)
	Deadline int64

	events []CTGameEvent
	rng    ctRand
}

// ctRand 게임이 쓰는 난수원의 최소 계약 (허브 고루틴에서만 호출)
type ctRand interface {
	Intn(n int) int
	Shuffle(n int, swap func(i, j int))
}

// CTClient 시타델 클라이언트 연결
type CTClient struct {
	wsClient
	Hub  *CTHub
	Seat int
}

// CTMessage 메시지 봉투
type CTMessage struct {
	Type    CTMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type CTJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type CTRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// CTPickRolePayload 직업 선택 (1~8번)
type CTPickRolePayload struct {
	Role int `json:"role"`
}

// CTGatherPayload 자원 — "gold"(금화 2) 또는 "cards"(카드 2장 뽑아 1장 남기기)
type CTGatherPayload struct {
	Kind string `json:"kind"`
}

// CTKeepPayload 뽑은 2장 중 남길 카드의 인덱스
type CTKeepPayload struct {
	Index int `json:"index"`
}

// CTBuildPayload 건설할 손패 카드의 id
type CTBuildPayload struct {
	CardID int `json:"cardId"`
}

// CTAbilityPayload 직업 능력.
//   - 암살자·도둑: targetRole
//   - 마술사: targetSeat(손패 교환) 또는 discard(버릴 손패 인덱스 목록)
//   - 장군: targetSeat + cardId(파괴할 건물)
//
// targetSeat 는 좌석 0 을 "지정 없음"과 구분해야 하므로 포인터로 받는다.
type CTAbilityPayload struct {
	TargetRole int   `json:"targetRole,omitempty"`
	TargetSeat *int  `json:"targetSeat,omitempty"`
	CardID     int   `json:"cardId,omitempty"`
	Discard    []int `json:"discard,omitempty"`
}

type CTReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

// CTPlayerView 좌석별 공개 정보. 좌석 0·금화 0·승점 0 유실을 막기 위해
// omitempty 를 쓰지 않는다. 손패는 장수만 공개하고 내용은 담지 않으며,
// 직업은 호출로 공개되기 전까지 roleRevealed 0 이다.
type CTPlayerView struct {
	Seat      int    `json:"seat"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Bot       bool   `json:"bot"`
	// Gold 보유 금화
	Gold int `json:"gold"`
	// HandCount 손패 장수 (내용은 본인만 본다)
	HandCount int `json:"handCount"`
	// Built 완성한 도시 (전원 공개)
	Built []CTCard `json:"built"`
	// Score 지금까지의 승점 (건물값 합 + 확정된 보너스)
	Score int `json:"score"`
	// RoleRevealed 호출로 공개된 직업 번호 (0 = 비공개)
	RoleRevealed int `json:"roleRevealed"`
	// Killed / Robbed 이번 라운드 암살·도둑질 피해 (공개된 뒤에만 true)
	Killed bool `json:"killed"`
	Robbed bool `json:"robbed"`
}

// CTGameStatePayload 개인화된 전체 게임 스냅샷. 모든 상태 변경 후 좌석마다
// 따로 만들어 보낸다. 재접속 복원도 같은 페이로드를 쓴다.
//
// 은닉: yourRole · yourHand · yourDraw · pickPool 은 본인에게만 실린다 —
// 타인·관전자(viewerSeat -1)의 raw JSON 에는 키 자체가 없다. 빈 손패도 [] 로
// 보내야 하므로 슬라이스 포인터로 부재를 표현한다. 뒷면으로 제외된 직업은
// 어떤 필드에도 실리지 않는다.
type CTGameStatePayload struct {
	GameID   string  `json:"gameId"`
	RoomCode string  `json:"roomCode"`
	Phase    CTPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"` // 관전자는 -1
	// Spectators 관전자 수
	Spectators int `json:"spectators"`
	// EndsAt 현재 대기 상태의 마감 시각 (unixMillis, 그 외 0)
	EndsAt int64 `json:"endsAt"`
	// Round 라운드 번호 (시작 전 0)
	Round int `json:"round"`
	// LastRound 누군가 7채를 완성해 이번 라운드로 끝나는가
	LastRound bool `json:"lastRound"`
	// CrownSeat 왕관(선) 좌석
	CrownSeat int `json:"crownSeat"`
	// CallingRole 지금 호출 중인 직업 (0 = 직업 선택 단계)
	CallingRole int `json:"callingRole"`
	// CurrentSeat 지금 행동할 좌석 (-1 없음)
	CurrentSeat int `json:"currentSeat"`
	// FaceUpRemoved 앞면으로 제외된 직업 (전원 공개, 빈 경우 [])
	FaceUpRemoved []int `json:"faceUpRemoved"`
	// PickPool 지금 고를 수 있는 직업 — 고르는 사람에게만
	PickPool *[]int `json:"pickPool,omitempty"`
	// YourRole 본인 직업 (0 = 미확정) — 본인에게만
	YourRole *int `json:"yourRole,omitempty"`
	// YourHand 본인 손패 — 본인에게만
	YourHand *[]CTCard `json:"yourHand,omitempty"`
	// YourDraw keep_card 단계에서 고를 2장 — 본인에게만
	YourDraw   *[]CTCard      `json:"yourDraw,omitempty"`
	Players    []CTPlayerView `json:"players"`
	LastAction *CTLastAction  `json:"lastAction"` // 그 전엔 null
	Result     *CTResult      `json:"result"`     // 종료 결과 (그 전엔 null)
}

// CTEventPayload 연출용 이벤트. 손패·비공개 직업의 내용을 담지 않으며
// 전원에게 동일하게 간다. Seat 은 좌석 0 유실 방지를 위해 포인터로 생략을
// 표현한다.
type CTEventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

// CTGameOverPayload 게임 종료 발표
type CTGameOverPayload struct {
	WinnerSeats []int          `json:"winnerSeats"`
	WinnerNames []string       `json:"winnerNames"`
	Message     string         `json:"message"`
	Rounds      int            `json:"rounds"`
	Rows        []CTResultRow  `json:"rows"`
	Players     []CTPlayerView `json:"players"`
}

type CTPlayerDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type CTPlayerReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type CTErrorPayload struct {
	Message string `json:"message"`
}
