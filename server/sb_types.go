package server

import "time"

// ==================== 사보타지 (Saboteur) 타입 ====================
//
// 3~10인 정체 은닉 + 길 타일 배치. 광부(miner)는 시작 타일에서 목표 타일까지
// 길을 잇고, 파괴꾼(saboteur)은 그것을 막는다. 서로의 정체는 아무도 모른다.
//
// 은닉의 심장은 sb_hub.go 의 buildSBState 다. yourRole·yourHand 는 본인
// 스냅샷에만 실리고, 타인·관전자(viewerSeat -1)의 raw JSON 에는 키 자체가
// 없다. 목표 타일의 gold 는 공개 전까지 어떤 스냅샷에도 실리지 않으며
// (포인터 + omitempty), 지도 카드 결과는 sb_map 개인 이벤트로 쓴 사람에게만
// 간다. players[].role 은 game_over 전까지 전원 "" 다.
//
// 원작의 3라운드·금덩이 분배는 생략한다 — 1라운드로 승패를 가른다.
//
// ==================== 덱 구성표 (40장 고정) ====================
//
//	길 십자   U R D L                    6장
//	길 가로   L R                        8장
//	길 세로   U D                        3장
//	길 굽이   UR · RD · DL · LU          4장 (각 1)
//	길 T자    URD · RDL · DLU · LUR      4장 (각 1)
//	막다른    U · R · D · L              4장 (각 1, 내부가 막혀 길이 안 이어짐)
//	지도                                 2장
//	낙석                                 2장
//	파괴      곡괭이2 · 수레1 · 등불1    4장
//	수리      곡괭이1 · 수레1 · 등불1    3장
//	------------------------------------------
//	합계                                40장
//
// 가로로 통하는 타일(가로 직선·십자·좌우가 뚫린 T자)이 16장이라, 시작 타일
// 오른쪽 7칸(col 1~7)을 채워야 목표에 닿는 최단 경로를 광부가 감당할 수 있다.
// 이 비율이 무너지면 파괴꾼이 아무것도 안 해도 이긴다 — 봇 승률 측정
// 테스트(TestSBBotBalance)가 그 회귀를 잡는다.
//
// 매 차례 손패에서 정확히 1장이 영구히 빠지므로(내거나 버리거나) 판 전체는
// 40차례 안에 반드시 끝난다 — 유한 종료의 근거다.

const (
	SBMinPlayers = 3
	SBMaxPlayers = 10

	// SBFillBotTarget sb_fill_bots 가 채우는 목표 인원 — 채운 뒤 즉시 시작
	SBFillBotTarget = 5

	// 판 크기 — 가로 9칸 × 세로 5칸
	SBCols = 9
	SBRows = 5

	// 시작 타일 좌표 (사방이 뚫린 통로)
	SBStartCol = 0
	SBStartRow = 2

	// SBDeckSize 덱 총 장수 (위 구성표 합계)
	SBDeckSize = 40
)

// sbGoalCells 목표 타일 3장의 좌표 (뒷면으로 깔린다 — 하나만 금덩이)
var sbGoalCells = [3][2]int{{8, 0}, {8, 2}, {8, 4}}

// SBTileKind 판 위 칸의 종류 (와이어 값)
type SBTileKind string

const (
	SBTileStart SBTileKind = "start"
	SBTilePath  SBTileKind = "path" // 플레이어가 놓은 길 타일 (막다른 포함 — dead 로 구분)
	SBTileGoal  SBTileKind = "goal"
)

// SBCardKind 손패 카드 종류 (와이어 값)
type SBCardKind string

const (
	SBCardPath     SBCardKind = "path"     // 통로 타일
	SBCardDeadend  SBCardKind = "deadend"  // 막다른 타일
	SBCardMap      SBCardKind = "map"      // 지도 — 목표 1장을 나만 본다
	SBCardRockfall SBCardKind = "rockfall" // 낙석 — 놓인 길 타일 1장 제거
	SBCardBreak    SBCardKind = "break"    // 장비 파괴
	SBCardRepair   SBCardKind = "repair"   // 장비 수리
)

// SBTool 장비 3종 — 하나라도 망가지면 길 타일을 놓을 수 없다
type SBTool string

const (
	SBToolPick SBTool = "pick" // 곡괭이
	SBToolCart SBTool = "cart" // 수레
	SBToolLamp SBTool = "lamp" // 등불
)

var sbAllTools = []SBTool{SBToolPick, SBToolCart, SBToolLamp}

// sbToolLabel 장비 한글 표기
func sbToolLabel(t SBTool) string {
	switch t {
	case SBToolPick:
		return "곡괭이"
	case SBToolCart:
		return "수레"
	case SBToolLamp:
		return "등불"
	}
	return "?"
}

// sbToolValid 와이어로 들어온 장비 값 검증
func sbToolValid(t SBTool) bool {
	for _, valid := range sbAllTools {
		if t == valid {
			return true
		}
	}
	return false
}

// SBRole 배정 진영 (와이어 값 — yourRole·종료 공개 role 로만 나간다)
type SBRole string

const (
	SBRoleMiner    SBRole = "miner"
	SBRoleSaboteur SBRole = "saboteur"
)

// sbRoleLabel 진영 한글 표기
func sbRoleLabel(role string) string {
	switch role {
	case string(SBRoleMiner):
		return "광부"
	case string(SBRoleSaboteur):
		return "파괴꾼"
	}
	return "?"
}

// SBPhase 게임 진행 단계
type SBPhase string

const (
	SBPhaseWaiting  SBPhase = "waiting"
	SBPhasePlaying  SBPhase = "playing" // 차례 진행 (45초 마감 — 자동 버리기)
	SBPhaseGameOver SBPhase = "game_over"
)

// SBMessageType 사보타지 메시지 타입
type SBMessageType string

const (
	// 클라이언트 → 서버
	SBMsgJoinGame SBMessageType = "sb_join_game"
	SBMsgFillBots SBMessageType = "sb_fill_bots"
	SBMsgStart    SBMessageType = "sb_start"
	SBMsgRejoin   SBMessageType = "sb_rejoin"
	SBMsgPlace    SBMessageType = "sb_place"
	SBMsgAction   SBMessageType = "sb_action"
	SBMsgDiscard  SBMessageType = "sb_discard"
	SBMsgReact    SBMessageType = "sb_react"

	// 서버 → 클라이언트
	SBMsgPlayerJoined       SBMessageType = "sb_player_joined"
	SBMsgSpectateJoined     SBMessageType = "sb_spectate_joined"
	SBMsgGameState          SBMessageType = "sb_game_state"
	SBMsgEvent              SBMessageType = "sb_event"
	SBMsgMap                SBMessageType = "sb_map" // 개인 이벤트 (지도를 쓴 사람만)
	SBMsgGameOver           SBMessageType = "sb_game_over"
	SBMsgPlayerDisconnected SBMessageType = "sb_player_disconnected"
	SBMsgPlayerReconnected  SBMessageType = "sb_player_reconnected"
	SBMsgSessionExpired     SBMessageType = "sb_session_expired"
	SBMsgError              SBMessageType = "sb_error"
)

// SBCard 손패 카드 한 장. 길 타일은 네 방향 통로 여부로 모양을 표현하고,
// 막다른 타일은 Dead 로 구분한다 (통로는 그려져 있지만 내부에서 이어지지
// 않아 그 뒤로는 길이 뻗지 못한다). 행동 카드는 방향 필드를 쓰지 않는다.
type SBCard struct {
	Kind  SBCardKind `json:"kind"`
	Up    bool       `json:"up"`
	Right bool       `json:"right"`
	Down  bool       `json:"down"`
	Left  bool       `json:"left"`
	// Dead 막다른 타일 여부 (프론트는 끊긴 선으로 그린다)
	Dead bool `json:"dead"`
	// Tool 파괴·수리 카드가 지정하는 장비 (길 타일은 부재)
	Tool SBTool `json:"tool,omitempty"`
	// Flipable 180° 회전이 의미 있는 모양인가 (십자·직선은 false)
	Flipable bool `json:"flipable"`
}

// sbTile 길 타일 카드 생성 (Flipable 은 모양에서 자동 판정)
func sbTile(up, right, down, left, dead bool) SBCard {
	kind := SBCardPath
	if dead {
		kind = SBCardDeadend
	}
	return SBCard{
		Kind: kind, Up: up, Right: right, Down: down, Left: left, Dead: dead,
		Flipable: up != down || left != right,
	}
}

// sbFlip 180° 회전 (상↔하 · 좌↔우)
func sbFlip(c SBCard) SBCard {
	c.Up, c.Down = c.Down, c.Up
	c.Left, c.Right = c.Right, c.Left
	return c
}

// sbIsTile 길 타일 카드인가 (통로·막다른)
func sbIsTile(c SBCard) bool {
	return c.Kind == SBCardPath || c.Kind == SBCardDeadend
}

// SBTools 장비 상태 — false 가 "망가짐"이다
type SBTools struct {
	Pick bool `json:"pick"`
	Cart bool `json:"cart"`
	Lamp bool `json:"lamp"`
}

// sbToolsAllOK 셋 다 멀쩡한가 (길 타일 배치 조건)
func (t SBTools) sbToolsAllOK() bool { return t.Pick && t.Cart && t.Lamp }

// get 장비 하나의 상태
func (t SBTools) get(tool SBTool) bool {
	switch tool {
	case SBToolPick:
		return t.Pick
	case SBToolCart:
		return t.Cart
	case SBToolLamp:
		return t.Lamp
	}
	return true
}

// set 장비 하나의 상태 변경
func (t *SBTools) set(tool SBTool, ok bool) {
	switch tool {
	case SBToolPick:
		t.Pick = ok
	case SBToolCart:
		t.Cart = ok
	case SBToolLamp:
		t.Lamp = ok
	}
}

// SBCell 판 위에 놓인 칸 하나 (순수 상태). Gold 는 서버만 아는 값이라
// 스냅샷에는 Revealed 인 목표 타일에서만 실린다.
type SBCell struct {
	Col, Row int
	Kind     SBTileKind
	Up       bool
	Right    bool
	Down     bool
	Left     bool
	Dead     bool
	// GoalIndex 목표 타일 0~2 (그 외 -1)
	GoalIndex int
	Revealed  bool
	Gold      bool
}

// SBPlayer 게임 참가자 (순수 상태 — 연결 매핑은 허브의 sbRoom 담당)
type SBPlayer struct {
	Seat int
	Name string
	// Role 배정 진영 — 스냅샷 직접 노출 금지 (본인 yourRole·종료 공개만)
	Role SBRole
	// Hand 손패 (본인만 내용을 본다)
	Hand []SBCard
	// Tools 장비 상태 (전원 공개)
	Tools SBTools
}

// SBGameEvent 순수 규칙이 쌓는 연출 이벤트 — 허브가 DrainEvents 로 꺼내
// sb_event 로 방송한다 (역할·손패 내용·금 위치는 절대 담지 않는다)
type SBGameEvent struct {
	Kind    string
	Seat    int // -1 없음
	Message string
}

// SBPrivate 지도 카드 결과 — 쓴 사람 한 명에게만 sb_map 으로 간다
// (은닉의 유일한 예외 경로 — 방송하지 않는다)
type SBPrivate struct {
	Seat  int
	Index int  // 목표 타일 0~2
	Gold  bool // 금덩이인가
}

// SBRolePool 인원별 역할 풀 — 전원 공개 정보. 풀에서 인원수만큼만 뽑아
// 나눠주므로 실제 진영 구성은 아무도 모른다 (크라켄과 같은 장치).
type SBRolePool struct {
	Miner    int `json:"miner"`
	Saboteur int `json:"saboteur"`
}

// SBLastAction 마지막 행동 요약 (전원 공개)
type SBLastAction struct {
	Seat    int    `json:"seat"`
	Name    string `json:"name"`
	Message string `json:"message"`
}

// SBResult 종료 결과 — 무승부 없음
type SBResult struct {
	Winner    string `json:"winner"`    // "miner" | "saboteur"
	GoldIndex int    `json:"goldIndex"` // 금덩이가 있던 목표 타일 (종료 시 공개)
	Reason    string `json:"reason"`    // "gold" | "exhausted"
	Message   string `json:"message"`
}

// SBGame 사보타지 게임 상태 (순수, 허브 비의존)
type SBGame struct {
	ID      string
	Players []*SBPlayer
	Phase   SBPhase

	// Board 길이 SBCols*SBRows 의 격자. nil 은 빈 칸이다.
	Board []*SBCell
	// Deck 남은 덱 (앞에서 뽑는다)
	Deck []SBCard
	// GoldIndex 금덩이가 있는 목표 타일 0~2 — 서버만 안다.
	// 공개 전에는 어떤 스냅샷에도 실리지 않는다.
	GoldIndex int

	// Pool 이번 판의 역할 풀 (시작 시 확정 — 전원 공개)
	Pool SBRolePool

	// CurrentSeat 차례 좌석 (-1 시작 전)
	CurrentSeat int
	// Turns 지금까지 진행된 차례 수 (덱이 유한해 40을 넘지 않는다)
	Turns int

	LastAction *SBLastAction
	Result     *SBResult
	Ready      bool
	StartedAt  time.Time

	// StateSeq 새 대기 상태(차례)가 열릴 때마다 +1 — 허브가 마감 타이머를
	// 다시 걸지 판단하는 근거
	StateSeq int
	// AfkSeq 마감 타이머 일련번호 (뒤늦은 발화 무시용 — 허브가 관리)
	AfkSeq int
	// Deadline 현재 차례의 마감 시각 (unixMillis — 스냅샷 노출용)
	Deadline int64

	events   []SBGameEvent
	privates []SBPrivate
}

// SBClient 사보타지 클라이언트 연결
type SBClient struct {
	wsClient
	Hub  *SBHub
	Seat int
}

// SBMessage 메시지 봉투
type SBMessage struct {
	Type    SBMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type SBJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type SBRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// SBPlacePayload 길 타일 배치. 좌석 0·인덱스 0·좌표 0 유실을 막기 위해
// omitempty 를 쓰지 않는다.
type SBPlacePayload struct {
	Index int  `json:"index"`
	Col   int  `json:"col"`
	Row   int  `json:"row"`
	Flip  bool `json:"flip"`
}

// SBActionPayload 행동 카드. 카드 종류에 따라 쓰는 필드가 다르다.
//   - map:      col·row (들여다볼 목표 타일 좌표)
//   - rockfall: col·row (걷어낼 길 타일 좌표)
//   - break:    targetSeat (카드에 장비가 적혀 있으면 tool 은 무시)
//   - repair:   targetSeat (동상)
type SBActionPayload struct {
	Index      int    `json:"index"`
	TargetSeat int    `json:"targetSeat"`
	Col        int    `json:"col"`
	Row        int    `json:"row"`
	Tool       SBTool `json:"tool,omitempty"`
}

type SBDiscardPayload struct {
	Index int `json:"index"`
}

type SBReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

// SBBoardCell 판 위 칸 하나의 공개 표현. 목표 타일의 Gold 는 공개 전까지
// 키 자체가 없다 (포인터 + omitempty) — 은닉 계약의 한 축이다.
type SBBoardCell struct {
	Col      int        `json:"col"`
	Row      int        `json:"row"`
	Kind     SBTileKind `json:"kind"`
	Up       bool       `json:"up"`
	Right    bool       `json:"right"`
	Down     bool       `json:"down"`
	Left     bool       `json:"left"`
	Dead     bool       `json:"dead"`
	Revealed bool       `json:"revealed"`
	Gold     *bool      `json:"gold,omitempty"`
}

// SBPlayerView 좌석별 공개 정보 — 좌석 0·장수 0 유실 방지를 위해 omitempty 금지
type SBPlayerView struct {
	Seat      int     `json:"seat"`
	Name      string  `json:"name"`
	Connected bool    `json:"connected"`
	Bot       bool    `json:"bot"`
	HandCount int     `json:"handCount"`
	Tools     SBTools `json:"tools"`
	Role      string  `json:"role"` // "" 진행 중 | 종료 후 전원 공개
}

// SBGameStatePayload 개인화된 전체 게임 스냅샷. 모든 상태 변경 후 좌석마다
// 따로 만들어 보낸다. 재접속 복원도 같은 페이로드를 쓴다.
// 은닉: yourRole·yourHand 는 본인에게만 실린다 — 타인·관전자(viewerSeat -1)의
// raw JSON 에는 키 자체가 없다. rolePool 은 전원 공개다.
type SBGameStatePayload struct {
	GameID   string  `json:"gameId"`
	RoomCode string  `json:"roomCode"`
	Phase    SBPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"` // 관전자는 -1
	// Spectators 관전자 수
	Spectators int `json:"spectators"`
	// EndsAt 현재 차례의 마감 시각 (unixMillis, 그 외 0)
	EndsAt      int64      `json:"endsAt"`
	CurrentSeat int        `json:"currentSeat"`
	DeckLeft    int        `json:"deckLeft"`
	Turns       int        `json:"turns"`
	RolePool    SBRolePool `json:"rolePool"`
	// Board 놓인 칸만 담는다 (빈 칸은 목록에 없다). 항상 [] 이상
	Board []SBBoardCell `json:"board"`
	// YourRole 본인 진영 — 본인에게만 (관전자·시작 전 부재)
	YourRole string `json:"yourRole,omitempty"`
	// YourHand 본인 손패 — 본인에게만 (관전자 부재).
	// 빈 손패도 [] 로 나가야 하므로 슬라이스 포인터로 부재를 표현한다.
	YourHand   *[]SBCard      `json:"yourHand,omitempty"`
	Players    []SBPlayerView `json:"players"`
	LastAction *SBLastAction  `json:"lastAction"` // 그 전엔 null
	Result     *SBResult      `json:"result"`     // 종료 결과 (그 전엔 null)
}

// SBEventPayload 연출용 이벤트. 역할·손패·금 위치를 담지 않으며 전원에게
// 동일하게 간다. Seat 은 좌석 0 유실 방지를 위해 포인터로 생략을 표현한다.
type SBEventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

// SBMapPayload 지도 카드 결과 — 쓴 사람에게만 간다
type SBMapPayload struct {
	Index int  `json:"index"`
	Gold  bool `json:"gold"`
}

// SBGameOverPayload 게임 종료 발표 — 전원 역할·금 위치 공개
type SBGameOverPayload struct {
	Winner    string         `json:"winner"` // "miner" | "saboteur"
	Reason    string         `json:"reason"` // "gold" | "exhausted"
	Message   string         `json:"message"`
	GoldIndex int            `json:"goldIndex"`
	Turns     int            `json:"turns"`
	Board     []SBBoardCell  `json:"board"`
	Players   []SBPlayerView `json:"players"`
}

type SBPlayerDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type SBPlayerReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type SBErrorPayload struct {
	Message string `json:"message"`
}

// ==================== 역할 풀 표 ====================
//
// 인원 N 일 때 풀은 항상 N+1 장이고 그중 N 장만 뽑아 나눠준다 — 남은 1장이
// 무엇인지 아무도 모르므로 실제 파괴꾼 수는 표의 값이거나 그보다 하나 적다.
//
//	3~4인 파괴꾼 1 / 5~6인 2 / 7~8인 3 / 9~10인 3 (나머지 광부)
var sbRolePools = map[int]SBRolePool{
	3:  {Miner: 3, Saboteur: 1},
	4:  {Miner: 4, Saboteur: 1},
	5:  {Miner: 4, Saboteur: 2},
	6:  {Miner: 5, Saboteur: 2},
	7:  {Miner: 5, Saboteur: 3},
	8:  {Miner: 6, Saboteur: 3},
	9:  {Miner: 7, Saboteur: 3},
	10: {Miner: 8, Saboteur: 3},
}

// sbRolePoolFor 인원별 역할 풀 (표 밖 인원은 zero — 대기실 미리보기용)
func sbRolePoolFor(n int) SBRolePool {
	return sbRolePools[n]
}

// sbHandSize 인원별 1인 손패 장수 (3~5인 6장 / 6~7인 5장 / 8~10인 4장)
func sbHandSize(n int) int {
	switch {
	case n <= 5:
		return 6
	case n <= 7:
		return 5
	default:
		return 4
	}
}
