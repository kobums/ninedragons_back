package server

import "time"

// ==================== 스타트업스 (Startups) 타입 ====================
//
// 3~7인 카드 투자·다수 지분 게임. 규칙은 작지만 "안티(누적 판돈)"와
// 대주주 판정이 이 게임의 전부다.
//
// ───────────────── 앞면 / 뒷면 흐름 (이 게임의 심장) ─────────────────
//
// 이 흐름을 헷갈리면 게임이 통째로 틀린다. 요약하면 이렇다.
//
//	  ┌────────────────────────────┐
//	  │  덱 (뒷면 · 아무도 못 본다)      │ ← 위에 안티가 쌓인다 (deckAnte)
//	  └──────────────┬─────────────┘
//	 ① 덱에서 가져오기  │  내가 그 회사의 대주주면 그 카드는 못 가져온다.
//	 (+ 덱 위 안티 회수) │  → 자기 돈 1원을 덱 위에 얹고, 그 카드는 덱 맨
//	                   │     아래로 보낸 뒤 다시 뽑는다 (돈이 없으면 못 뽑는다)
//	                   ▼
//	  ┌────────────────────────────┐
//	  │  내 손패 (비공개)              │ ← ★ 덱에서 뽑은 카드는 여기로 온다.
//	  │  yourHand — 본인 스냅샷에만     │    남에게는 handCount 숫자만 보인다
//	  └──────────────┬─────────────┘
//	 ② 손패 1장을      │  ★ 내려놓은 카드는 "시장"에 놓인다.
//	   시장에 앞면으로   │     내 앞 앞면 더미로 오지 않는다!
//	                   ▼
//	  ┌────────────────────────────┐
//	  │  시장 (전원 공개)              │ ← 카드마다 안티가 쌓인다 (market[].ante)
//	  └──────────────┬─────────────┘
//	 ① 시장에서 가져오기 │  그 카드 위에 쌓인 안티를 전부 받는다
//	                   ▼
//	  ┌────────────────────────────┐
//	  │  내 앞 앞면 더미 (전원 공개)     │ ← ★ 여기에 쌓이는 것은 오직
//	  │  players[].faceUp           │    "시장에서 가져온 카드"뿐이다
//	  └────────────────────────────┘
//
// 정리:
//   - 앞면 더미(faceUp)는 **시장에서 가져온 카드**로만 채워진다.
//   - 덱에서 뽑은 카드는 **비공개 손패**에 머물다가 시장으로 나간다.
//   - 시장에 내려놓은 카드는 **시장에 남는다** (내 앞으로 가지 않는다).
//   - 진행 중 대주주 판정은 **앞면 더미만** 센다. 최종 정산에서만 손패를
//     공개해 앞면 더미와 합쳐 센다.
//
// ─────────── 회사 6종 (id · 표기 · 총 장수=가치 · 색 · 이모지) ───────────
//
//	geeks        긱스        3장  3원   보라 #7c3aed  🧠
//	bowwow       바우와우     4장  4원   주황 #f59e0b  🐶
//	ocean        오션        5장  5원   파랑 #0ea5e9  🌊
//	superfusion  슈퍼퓨전     6장  6원   빨강 #ef4444  ⚛️
//	gaga         가가        7장  7원   분홍 #ec4899  🎤
//	dove         더브        8장  8원   초록 #22c55e  🕊️
//
// 총 33장. 장수가 적은 회사일수록 귀하고(모으기 쉽고) 가치는 낮다.
// 색만으로 구분되지 않도록 프론트는 색·이모지·이름·총 장수를 함께 쓴다.
//
// 은닉의 심장은 su_hub.go 의 buildSUState 다. yourHand 는 본인 스냅샷에만
// 실리고 타인·관전자의 raw JSON 에는 키 자체가 없다. 시작 때 게임에서 제외한
// 3장(SUGame.Removed)은 어떤 스냅샷·이벤트·로그에도 나가지 않는다.

const (
	SUMinPlayers = 3
	SUMaxPlayers = 7

	// SUFillBotTarget su_fill_bots 가 채우는 목표 인원 — 채운 뒤 즉시 시작
	SUFillBotTarget = 4

	// SUStartMoney 시작 돈 (1원 단위 칩)
	SUStartMoney = 10

	// SUStartHand 시작 손패 (비공개 주식 카드)
	SUStartHand = 1

	// SURemovedCards 시작에 덱에서 빼서 게임에서 제외하는 장수 (아무도 못 봄)
	SURemovedCards = 3

	// SUMaxTurns 병리적 교착(전원 무일푼 + 덱 전량 대주주 벽)을 끊는 안전망.
	// 정상 판은 덱 소진으로 훨씬 먼저 끝난다.
	SUMaxTurns = 400
)

// SUCompany 회사 (와이어 값)
type SUCompany string

const (
	SUGeeks       SUCompany = "geeks"
	SUBowwow      SUCompany = "bowwow"
	SUOcean       SUCompany = "ocean"
	SUSuperfusion SUCompany = "superfusion"
	SUGaga        SUCompany = "gaga"
	SUDove        SUCompany = "dove"
)

// suCompanyDef 회사 한 종의 고정 정보. Size 가 곧 총 장수이자 회사 가치다.
// Color·Emoji 는 프론트 표기용 참고값이라 와이어에 싣지 않는다
// (와이어 계약은 {id, name, size, majoritySeat} 로 고정).
type suCompanyDef struct {
	ID    SUCompany
	Name  string
	Size  int
	Color string
	Emoji string
}

// suCompanyDefs 회사 6종 — 장수 3·4·5·6·7·8 (총 33장)
var suCompanyDefs = []suCompanyDef{
	{SUGeeks, "긱스", 3, "#7c3aed", "🧠"},
	{SUBowwow, "바우와우", 4, "#f59e0b", "🐶"},
	{SUOcean, "오션", 5, "#0ea5e9", "🌊"},
	{SUSuperfusion, "슈퍼퓨전", 6, "#ef4444", "⚛️"},
	{SUGaga, "가가", 7, "#ec4899", "🎤"},
	{SUDove, "더브", 8, "#22c55e", "🕊️"},
}

// suCompanyByID 회사 id → 정의 (조회용 색인)
var suCompanyByID = func() map[SUCompany]suCompanyDef {
	m := make(map[SUCompany]suCompanyDef, len(suCompanyDefs))
	for _, def := range suCompanyDefs {
		m[def.ID] = def
	}
	return m
}()

// suName 회사 한글 표기 (이벤트·로그 문구용)
func suName(c SUCompany) string {
	if def, ok := suCompanyByID[c]; ok {
		return def.Name
	}
	return string(c)
}

// suSize 회사의 총 장수 = 회사 가치(원)
func suSize(c SUCompany) int {
	return suCompanyByID[c].Size
}

// suDeckSize 덱 전체 장수 (33장)
func suDeckSize() int {
	n := 0
	for _, def := range suCompanyDefs {
		n += def.Size
	}
	return n
}

// SUPhase 게임 진행 단계. 한 차례는 take → play 두 단계로 나뉜다.
type SUPhase string

const (
	SUPhaseWaiting  SUPhase = "waiting"
	SUPhaseTake     SUPhase = "take" // ① 카드 얻기 (덱 또는 시장)
	SUPhasePlay     SUPhase = "play" // ② 손패 1장을 시장에 내려놓기
	SUPhaseGameOver SUPhase = "game_over"
)

// SUMessageType 스타트업스 메시지 타입
type SUMessageType string

const (
	// 클라이언트 → 서버
	SUMsgJoinGame SUMessageType = "su_join_game"
	SUMsgFillBots SUMessageType = "su_fill_bots"
	SUMsgStart    SUMessageType = "su_start"
	SUMsgRejoin   SUMessageType = "su_rejoin"
	SUMsgTake     SUMessageType = "su_take"
	SUMsgPlay     SUMessageType = "su_play"
	SUMsgReact    SUMessageType = "su_react"

	// 서버 → 클라이언트
	SUMsgPlayerJoined       SUMessageType = "su_player_joined"
	SUMsgSpectateJoined     SUMessageType = "su_spectate_joined"
	SUMsgGameState          SUMessageType = "su_game_state"
	SUMsgEvent              SUMessageType = "su_event"
	SUMsgGameOver           SUMessageType = "su_game_over"
	SUMsgPlayerDisconnected SUMessageType = "su_player_disconnected"
	SUMsgPlayerReconnected  SUMessageType = "su_player_reconnected"
	SUMsgSessionExpired     SUMessageType = "su_session_expired"
	SUMsgError              SUMessageType = "su_error"
)

// SUTakeDeck su_take 의 from 값 — 덱 맨 위
const SUTakeDeck = "deck"

// SUTakeMarketPrefix su_take 의 from 값 — "market:N" (시장 인덱스)
const SUTakeMarketPrefix = "market:"

// SUMarketCard 시장에 놓인 주식 카드 한 장 (전원 공개).
// Ante 는 그 카드 위에 쌓인 안티 — 가져가는 사람이 전부 받는다.
type SUMarketCard struct {
	Company SUCompany `json:"company"`
	Ante    int       `json:"ante"`
}

// SUPlayer 게임 참가자 (순수 상태 — 연결 매핑은 허브의 suRoom 담당)
type SUPlayer struct {
	Seat int
	Name string
	// Money 돈 (1원 단위 칩)
	Money int
	// Hand 비공개 손패 — 덱에서 뽑은 카드가 여기로 온다 (본인만 내용을 본다)
	Hand []SUCompany
	// FaceUp 내 앞에 앞면으로 쌓인 카드 — 시장에서 가져온 카드만 들어온다.
	// 회사 6종 키를 항상 모두 갖는다 (nil → JSON null 방지)
	FaceUp map[SUCompany]int
}

// SUGameEvent 순수 규칙이 쌓는 연출 이벤트 — 허브가 DrainEvents 로 꺼내
// su_event 로 방송한다. 덱에서 뽑은 카드·손패 내용·제외 3장은 절대 담지
// 않는다 (대주주 벽에 막힌 카드의 회사명도 담지 않는다 — 덱 정보 유출).
type SUGameEvent struct {
	Kind    string
	Seat    int // -1 없음
	Message string
}

// SUCompanyInfo 회사 현황판 한 줄 (전원 공개).
// MajoritySeat 은 대주주 좌석, 없으면 -1 (동수면 대주주 없음).
// 좌석 0 유실을 막기 위해 omitempty 를 쓰지 않는다.
type SUCompanyInfo struct {
	ID           SUCompany `json:"id"`
	Name         string    `json:"name"`
	Size         int       `json:"size"`
	MajoritySeat int       `json:"majoritySeat"`
}

// SULastAction 마지막 행동 요약 (전원 공개)
type SULastAction struct {
	Seat    int    `json:"seat"`
	Name    string `json:"name"`
	Message string `json:"message"`
}

// SUResultRow 최종 정산 한 줄. Detail 은 대주주 회사·정산액을 담은 한글 설명.
type SUResultRow struct {
	Seat   int    `json:"seat"`
	Money  int    `json:"money"`
	Detail string `json:"detail"`
}

// SUResult 종료 결과. 최종 돈이 같으면 공동 승이라 좌석이 여럿이다.
type SUResult struct {
	Rows        []SUResultRow `json:"rows"`
	WinnerSeats []int         `json:"winnerSeats"`
	WinnerNames []string      `json:"winnerNames"`
	Message     string        `json:"message"`
}

// SUGame 스타트업스 게임 상태 (순수, 허브 비의존)
type SUGame struct {
	ID      string
	Players []*SUPlayer
	Phase   SUPhase

	// Deck 주식 카드 덱 (앞이 맨 위). 아무도 내용을 못 본다.
	Deck []SUCompany
	// Removed 시작에 빼서 게임에서 제외한 3장 — 어떤 스냅샷·이벤트에도
	// 나가지 않는다 (검증용으로만 보관)
	Removed []SUCompany
	// DeckAnte 덱 위에 쌓인 안티
	DeckAnte int
	// Market 시장 (앞이 먼저 놓인 카드)
	Market []SUMarketCard

	// CurrentSeat 현재 차례 좌석 (-1 시작 전)
	CurrentSeat int
	// StartSeat 첫 차례 좌석 — 덱 소진 뒤 "그 라운드를 마치는" 기준점
	StartSeat int
	// Turns 지금까지 끝난 차례 수
	Turns int

	LastAction *SULastAction // 마지막 행동 (그 전엔 nil)
	Result     *SUResult     // 종료 결과 (그 전엔 nil)
	Ready      bool
	StartedAt  time.Time

	// StateSeq 새 대기 상태(가져오기·내려놓기)가 열릴 때마다 +1 —
	// 허브가 마감 타이머를 다시 걸지 판단하는 근거
	StateSeq int
	// AfkSeq 마감 타이머 일련번호 (뒤늦은 발화 무시용 — 허브가 관리)
	AfkSeq int
	// Deadline 현재 대기 상태의 마감 시각 (unixMillis — 스냅샷 노출용)
	Deadline int64

	events []SUGameEvent
}

// SUClient 스타트업스 클라이언트 연결
type SUClient struct {
	wsClient
	Hub  *SUHub
	Seat int
}

// SUMessage 메시지 봉투
type SUMessage struct {
	Type    SUMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type SUJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type SURejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// SUTakePayload 카드 얻기 — "deck" 또는 "market:N"
type SUTakePayload struct {
	From string `json:"from"`
}

// SUPlayPayload 손패에서 시장에 낼 카드. 인덱스 0 유실을 막기 위해
// omitempty 를 쓰지 않는다.
type SUPlayPayload struct {
	Index int `json:"index"`
}

type SUReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

// SUPlayerView 좌석별 공개 정보 — 좌석 0·돈 0·장수 0 유실 방지를 위해
// omitempty 금지. FaceUp 은 회사 6종 키를 항상 모두 갖는다.
type SUPlayerView struct {
	Seat      int               `json:"seat"`
	Name      string            `json:"name"`
	Connected bool              `json:"connected"`
	Bot       bool              `json:"bot"`
	Money     int               `json:"money"`
	HandCount int               `json:"handCount"`
	FaceUp    map[SUCompany]int `json:"faceUp"`
}

// SUGameStatePayload 개인화된 전체 게임 스냅샷. 모든 상태 변경 후 좌석마다
// 따로 만들어 보낸다. 재접속 복원도 같은 페이로드를 쓴다.
//
// 은닉: YourHand 는 본인에게만 실린다 — 타인·관전자(viewerSeat -1)의 raw
// JSON 에는 키 자체가 없다. 빈 손패도 [] 로 보내야 하므로 슬라이스 포인터로
// 부재를 표현한다. market·faceUp·companies 는 전원 공개다.
// 게임에서 제외한 3장은 이 스냅샷 어디에도 없다.
type SUGameStatePayload struct {
	GameID   string  `json:"gameId"`
	RoomCode string  `json:"roomCode"`
	Phase    SUPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"` // 관전자는 -1
	// Spectators 관전자 수
	Spectators int `json:"spectators"`
	// EndsAt 현재 대기 상태(가져오기·내려놓기)의 마감 시각 (unixMillis, 그 외 0)
	EndsAt      int64 `json:"endsAt"`
	CurrentSeat int   `json:"currentSeat"`
	// DeckLeft 덱에 남은 장수, DeckAnte 덱 위에 쌓인 안티
	DeckLeft int `json:"deckLeft"`
	DeckAnte int `json:"deckAnte"`
	// Market 시장 카드와 그 위의 안티 (빈 시장도 [])
	Market []SUMarketCard `json:"market"`
	// Companies 회사 현황판 (총 장수=가치, 대주주 좌석)
	Companies []SUCompanyInfo `json:"companies"`
	// YourHand 본인의 비공개 손패 — 본인에게만 (관전자·시작 전 부재)
	YourHand   *[]SUCompany   `json:"yourHand,omitempty"`
	Players    []SUPlayerView `json:"players"`
	LastAction *SULastAction  `json:"lastAction"` // 그 전엔 null
	Result     *SUResult      `json:"result"`     // 종료 결과 (그 전엔 null)
}

// SUEventPayload 연출용 이벤트. 손패 내용·덱 내용·제외 3장은 담지 않으며
// 전원에게 동일하게 간다. Seat 은 좌석 0 유실 방지를 위해 포인터다.
type SUEventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

// SUGameOverPayload 게임 종료 발표 — 정산 내역과 최종 보유가 함께 간다
// (정산 시점에 손패를 전부 공개해 faceUp 에 합치므로 players[].faceUp 이
// 최종 보유 수다)
type SUGameOverPayload struct {
	Rows        []SUResultRow   `json:"rows"`
	WinnerSeats []int           `json:"winnerSeats"`
	WinnerNames []string        `json:"winnerNames"`
	Message     string          `json:"message"`
	Turns       int             `json:"turns"`
	Companies   []SUCompanyInfo `json:"companies"`
	Players     []SUPlayerView  `json:"players"`
}

type SUPlayerDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type SUPlayerReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type SUErrorPayload struct {
	Message string `json:"message"`
}
