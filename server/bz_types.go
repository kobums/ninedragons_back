package server

import (
	"math/rand"
	"time"
)

// ==================== 보난자 (Bohnanza) 타입 ====================
//
// 3~5인 콩 심기·거래 카드 게임. 규칙 자체는 작지만 이 게임의 전부는
// **손패 순서를 절대 바꿀 수 없다**는 제약이다.
//
// ─────────────── 손패 순서 불변 (이 게임의 심장) ───────────────
//
//	    ┌──────────────────────────────────────────────┐
//	    │  손패 (본인만 내용을 본다 · yourHand)            │
//	    │                                              │
//	    │   [맨 앞] ─▶ ─▶ ─▶ ─▶ ─▶ ─▶ ─▶ ─▶ [맨 뒤]      │
//	    │      ▲                              ▲        │
//	    │      │ ① 심기는 여기서만 뺀다          │        │
//	    │      │                              │        │
//	    │      └──────────────────────  ④ 뽑기는 여기에만 붙인다
//	    └──────────────────────────────────────────────┘
//
//	  - 맨 앞 카드는 자기 차례에 **반드시** 심어야 한다.
//	  - 뽑은 카드는 **맨 뒤**로만 붙는다.
//	  - 거래로 내주는 카드는 손패 중간에서 빠질 수 있지만, 남은 카드의
//	    **상대 순서는 그대로**다.
//	  - **정렬·재배열 API 는 존재하지 않는다.** 서버에 만들지 마라.
//
// ─────────────── 차례 4단계 (순서를 바꾸지 마라) ───────────────
//
//	① plant           손패 맨 앞 카드를 심는다 (두 번째는 선택)
//	② trade           덱 위 2장 공개 → 거래·기부 (차례인 사람이 반드시 낀다)
//	③ plant_received  거래·기부로 받은 카드를 손에 못 들고 즉시 전부 심는다
//	④ draw            3장을 한 장씩 뽑아 손패 맨 뒤에 붙인다 (자동·즉시)
//
// ④ draw 는 사람이 기다리는 단계가 아니라 서버가 즉시 해소하는 전이 단계다
// (그래서 AFK 마감이 없다). 와이어 계약에 값이 있으므로 상수만 유지한다.
//
// ─────────────── 콩 8종 (총 104장) — 신판 기준 ───────────────
//
// 콩미터는 "몇 장을 수확하면 금화 몇 개"인지의 문턱값이다. 아래 표는
// **금화 1/2/3/4개를 받는 최소 장수**이며 0은 그 칸이 없다는 뜻이다.
//
//	콩          와이어        장수  금화1 금화2 금화3 금화4
//	푸르대콩    blue          20     4     6     8    10
//	칠리콩      chili         18     3     6     8     9
//	메주콩      stink         16     3     5     7     8
//	완두콩      green         14     3     5     6     7
//	대두        soy           12     2     4     6     7
//	동부        blackeyed     10     2     4     5     6
//	팥          red            8     2     3     4     5
//	강낭콩      garden         6     —     2     3     —
//
// **강낭콩만 예외다** — 2장이면 금화 2개, 3장이면 금화 3개이고 금화 1개·4개
// 칸이 아예 없다. 금화 계산은 순수 함수 bzCoins(bean, count) 하나로만 한다.

const (
	BZMinPlayers = 3
	BZMaxPlayers = 5

	// BZFillBotTarget bz_fill_bots 가 채우는 목표 인원 — 채운 뒤 즉시 시작
	BZFillBotTarget = 3

	// BZStartHand 시작 손패 장수
	BZStartHand = 5

	// BZStartFields 시작 콩밭 수, BZMaxFields 세 번째 콩밭까지 산 최대치
	BZStartFields = 2
	BZMaxFields   = 3

	// BZThirdFieldCost 세 번째 콩밭 값 (금화). 외상 불가.
	BZThirdFieldCost = 3

	// BZFlipCount 2단계에서 덱 위에서 공개하는 장수
	BZFlipCount = 2

	// BZDrawCount 4단계에서 손패 맨 뒤로 붙이는 장수
	BZDrawCount = 3

	// BZMaxOffers 방 하나에 동시에 살아있을 수 있는 제안 수 (스팸 방어)
	BZMaxOffers = 12

	// BZMaxTurns 병리적 교착의 안전망. 정상 판은 덱 소진으로 훨씬 먼저 끝난다.
	BZMaxTurns = 400
)

// BZBean 콩 종류 (와이어 값 — 프론트와 고정 계약)
type BZBean string

const (
	BZBlue      BZBean = "blue"      // 푸르대콩
	BZChili     BZBean = "chili"     // 칠리콩
	BZStink     BZBean = "stink"     // 메주콩
	BZGreen     BZBean = "green"     // 완두콩
	BZSoy       BZBean = "soy"       // 대두
	BZBlackeyed BZBean = "blackeyed" // 동부
	BZRed       BZBean = "red"       // 팥
	BZGarden    BZBean = "garden"    // 강낭콩
)

// bzBeanDef 콩 한 종의 고정 정보.
// Meter[i] 는 "금화 (i+1)개를 받는 최소 장수"이고 0 은 그 칸이 없다는 뜻이다
// (강낭콩의 금화 1개·4개 칸). Color·Emoji 는 프론트 표기용 참고값이라
// 와이어에 싣지 않는다 — 와이어는 콩 이름(문자열)만 오간다.
type bzBeanDef struct {
	ID    BZBean
	Name  string
	Count int
	Meter [4]int
	Color string
	Emoji string
}

// bzBeanDefs 콩 8종 — 장수 20·18·16·14·12·10·8·6 (총 104장).
// 표의 값은 스펙 그대로다. 절대 임의로 고치지 마라.
//
// 같은 표가 프론트의 src/types/bohnanza.ts (BZ_BEAN_DEFS) 에도 있다.
// 서버는 실제 금화 지급의 근거이고 프론트는 "지금 수확하면 몇 금화"를 미리
// 보여주는 근거라, 한쪽만 고치면 화면 숫자와 실제 지급이 조용히 어긋난다.
// 고칠 일이 생기면 반드시 양쪽을 함께 고쳐라.
var bzBeanDefs = []bzBeanDef{
	{BZBlue, "푸르대콩", 20, [4]int{4, 6, 8, 10}, "#3b82f6", "🫐"},
	{BZChili, "칠리콩", 18, [4]int{3, 6, 8, 9}, "#f97316", "🌶️"},
	{BZStink, "메주콩", 16, [4]int{3, 5, 7, 8}, "#a16207", "🫘"},
	{BZGreen, "완두콩", 14, [4]int{3, 5, 6, 7}, "#22c55e", "🟢"},
	{BZSoy, "대두", 12, [4]int{2, 4, 6, 7}, "#eab308", "🌱"},
	{BZBlackeyed, "동부", 10, [4]int{2, 4, 5, 6}, "#374151", "⚫"},
	{BZRed, "팥", 8, [4]int{2, 3, 4, 5}, "#dc2626", "🔴"},
	{BZGarden, "강낭콩", 6, [4]int{0, 2, 3, 0}, "#7c3aed", "🌰"},
}

// bzBeanByID 콩 id → 정의 (조회용 색인)
var bzBeanByID = func() map[BZBean]bzBeanDef {
	m := make(map[BZBean]bzBeanDef, len(bzBeanDefs))
	for _, def := range bzBeanDefs {
		m[def.ID] = def
	}
	return m
}()

// bzName 콩 한글 표기 (이벤트·로그 문구용)
func bzName(b BZBean) string {
	if def, ok := bzBeanByID[b]; ok {
		return def.Name
	}
	return string(b)
}

// bzDeckSize 덱 전체 장수 (104장)
func bzDeckSize() int {
	n := 0
	for _, def := range bzBeanDefs {
		n += def.Count
	}
	return n
}

// bzCoins 콩미터 — count 장을 수확했을 때 받는 금화 수 (못 미치면 0).
//
// 이 게임 점수의 전부이므로 **순수 함수 하나로만** 계산한다.
// 위에서부터(금화 4개 칸부터) 내려오며 문턱을 만족하는 첫 칸을 쓴다.
// 강낭콩은 금화 1개·4개 칸이 없어(Meter 0) 자연스럽게 건너뛴다 —
// 2장 → 금화 2개, 3장 이상 → 금화 3개로 굳는다.
func bzCoins(bean BZBean, count int) int {
	def, ok := bzBeanByID[bean]
	if !ok || count <= 0 {
		return 0
	}
	for coins := 4; coins >= 1; coins-- {
		if t := def.Meter[coins-1]; t > 0 && count >= t {
			return coins
		}
	}
	return 0
}

// bzNextThreshold count 보다 큰 다음 문턱 장수 (더 오를 칸이 없으면 0).
// 봇이 "문턱 직전이면 버틴다"를 판단하는 근거다.
func bzNextThreshold(bean BZBean, count int) int {
	def, ok := bzBeanByID[bean]
	if !ok {
		return 0
	}
	for coins := 1; coins <= 4; coins++ {
		if t := def.Meter[coins-1]; t > 0 && t > count {
			return t
		}
	}
	return 0
}

// BZPhase 게임 진행 단계. 한 차례는 plant → trade → plant_received → draw 다.
type BZPhase string

const (
	BZPhaseWaiting BZPhase = "waiting"
	BZPhasePlant   BZPhase = "plant" // ① 손패 맨 앞 카드 심기
	BZPhaseTrade   BZPhase = "trade" // ② 2장 공개 + 거래·기부
	// BZPhasePlantReceived ③ 받은 카드 즉시 심기 (받은 사람 전원이 대상)
	BZPhasePlantReceived BZPhase = "plant_received"
	// BZPhaseDraw ④ 3장 뽑기 — 서버가 즉시 해소하는 전이 단계 (AFK 마감 없음)
	BZPhaseDraw     BZPhase = "draw"
	BZPhaseGameOver BZPhase = "game_over"
)

// BZMessageType 보난자 메시지 타입
type BZMessageType string

const (
	// 클라이언트 → 서버
	BZMsgJoinGame      BZMessageType = "bz_join_game"
	BZMsgFillBots      BZMessageType = "bz_fill_bots"
	BZMsgStart         BZMessageType = "bz_start"
	BZMsgRejoin        BZMessageType = "bz_rejoin"
	BZMsgPlant         BZMessageType = "bz_plant"
	BZMsgHarvest       BZMessageType = "bz_harvest"
	BZMsgBuyField      BZMessageType = "bz_buy_field"
	BZMsgOffer         BZMessageType = "bz_offer"
	BZMsgRespond       BZMessageType = "bz_respond"
	BZMsgPlantReceived BZMessageType = "bz_plant_received"
	BZMsgEndPhase      BZMessageType = "bz_end_phase"
	BZMsgReact         BZMessageType = "bz_react"

	// 서버 → 클라이언트
	BZMsgPlayerJoined       BZMessageType = "bz_player_joined"
	BZMsgSpectateJoined     BZMessageType = "bz_spectate_joined"
	BZMsgGameState          BZMessageType = "bz_game_state"
	BZMsgEvent              BZMessageType = "bz_event"
	BZMsgGameOver           BZMessageType = "bz_game_over"
	BZMsgPlayerDisconnected BZMessageType = "bz_player_disconnected"
	BZMsgPlayerReconnected  BZMessageType = "bz_player_reconnected"
	BZMsgSessionExpired     BZMessageType = "bz_session_expired"
	BZMsgError              BZMessageType = "bz_error"
)

// BZField 콩밭 하나 (전원 공개). 빈 밭은 Bean "" · Count 0 이다.
// 좌석·장수 0 유실을 막기 위해 omitempty 를 쓰지 않는다.
type BZField struct {
	Bean  BZBean `json:"bean"`
	Count int    `json:"count"`
}

// BZPlayer 게임 참가자 (순수 상태 — 연결 매핑은 허브의 bzRoom 담당)
type BZPlayer struct {
	Seat int
	Name string

	// Hand 손패 — **순서가 곧 진실이다**. 맨 앞에서만 빼고 맨 뒤로만 붙인다
	// (거래로 내주는 카드만 중간에서 빠지며, 남은 카드의 상대 순서는 유지된다).
	Hand []BZBean

	// Fields 콩밭 (2개로 시작, 세 번째를 사면 3개). 같은 종류만 심을 수 있다.
	Fields []BZField

	// Pending 거래·기부로 받아 **손에 못 들고 즉시 심어야 하는** 카드
	Pending []BZBean

	// Coins 금화 (= 게임에서 영구히 빠진 카드 장수)
	Coins int

	// BoughtField 세 번째 콩밭을 샀는지 (게임 중 1회)
	BoughtField bool
}

// BZOffer 진행 중인 거래 제안. 카드는 **인덱스**로 잡아 둔다 —
// 거래가 한 번 성사되면 손패·공개 카드의 인덱스가 밀리므로 그때 전부 파기한다
// (모든 제안에는 차례인 사람이 끼므로 어차피 전부 영향을 받는다).
type BZOffer struct {
	// ID 와이어에는 문자열로 나간다 (프론트 계약)
	ID       string
	FromSeat int
	ToSeat   int
	// GiveHand 제안자 손패 인덱스, GiveFlipped 공개 카드 인덱스
	// (공개 카드는 차례인 사람만 내줄 수 있다)
	GiveHand    []int
	GiveFlipped []int
	// WantHand 상대 손패 인덱스 — 0번은 상대가 다음 차례에 반드시 심어야 하는
	// "맨 앞 카드"라 이 게임에서 가장 값진 요구다. 손패 내용은 안 보이므로
	// 자리로 지목하고, 요구받은 쪽은 실제 콩을 보고 판단한다.
	WantHand []int
}

// BZGameEvent 순수 규칙이 쌓는 연출 이벤트 — 허브가 DrainEvents 로 꺼내
// bz_event 로 방송한다. 남의 손패 내용·덱 내용은 절대 담지 않는다.
type BZGameEvent struct {
	Kind    string
	Seat    int // -1 없음
	Message string
}

// BZLastAction 마지막 행동 요약 (전원 공개)
type BZLastAction struct {
	Seat    int    `json:"seat"`
	Name    string `json:"name"`
	Message string `json:"message"`
}

// BZResultRow 최종 정산 한 줄. 금화가 같으면 손에 든 카드가 많은 쪽이 이긴다.
type BZResultRow struct {
	Seat      int `json:"seat"`
	Coins     int `json:"coins"`
	HandCount int `json:"handCount"`
}

// BZResult 종료 결과. 금화·손패까지 같으면 공동 승이라 좌석이 여럿이다.
type BZResult struct {
	Rows        []BZResultRow `json:"rows"`
	WinnerSeats []int         `json:"winnerSeats"`
	WinnerNames []string      `json:"winnerNames"`
	Message     string        `json:"message"`
}

// BZGame 보난자 게임 상태 (순수, 허브 비의존)
type BZGame struct {
	ID      string
	Players []*BZPlayer
	Phase   BZPhase

	// Deck 덱 (앞이 맨 위). Discard 버린 더미 — 덱이 마르면 섞어 되돌린다.
	Deck    []BZBean
	Discard []BZBean

	// DeckCycle 덱을 몇 번 소진했는지 (0~3). EndCycle 에 도달하면 게임 끝.
	DeckCycle int
	// EndCycle 게임이 끝나는 소진 횟수 — 3인은 2, 4~5인은 3
	EndCycle int

	// Flipped 2단계에서 공개한 카드 (전원 공개)
	Flipped []BZBean

	// Offers 진행 중인 거래 제안
	Offers  []*BZOffer
	NextOID int

	// SpentCoins 세 번째 콩밭 값으로 나가 게임에서 영구히 빠진 카드 장수.
	// 카드 총량 회계(104장)를 맞추는 항이며 와이어에는 싣지 않는다.
	SpentCoins int

	CurrentSeat int // 현재 차례 좌석 (-1 시작 전)
	Turns       int // 지금까지 끝난 차례 수

	LastAction *BZLastAction // 마지막 행동 (그 전엔 nil)
	Result     *BZResult     // 종료 결과 (그 전엔 nil)
	Ready      bool
	StartedAt  time.Time

	// StateSeq 새 대기 상태(심기·거래·받은 카드 심기)가 열릴 때마다 +1 —
	// 허브가 마감 타이머를 다시 걸지 판단하는 근거
	StateSeq int
	// AfkSeq 마감 타이머 일련번호 (뒤늦은 발화 무시용 — 허브가 관리)
	AfkSeq int
	// Deadline 현재 대기 상태의 마감 시각 (unixMillis — 스냅샷 노출용)
	Deadline int64

	// rng 덱 셔플·자동 진행용. 허브 고루틴에서만 쓰이므로 게임에 물려 둔다.
	rng *rand.Rand

	events []BZGameEvent
}

// BZClient 보난자 클라이언트 연결
type BZClient struct {
	wsClient
	Hub  *BZHub
	Seat int
}

// BZMessage 메시지 봉투
type BZMessage struct {
	Type    BZMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type BZJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type BZRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// BZPlantPayload ① 심기 — Second 가 참이면 두 번째 카드까지 심는다
type BZPlantPayload struct {
	Second bool `json:"second"`
}

// BZHarvestPayload 수확할 밭 번호. 인덱스 0 유실을 막기 위해 omitempty 금지.
type BZHarvestPayload struct {
	Field int `json:"field"`
}

// BZOfferPayload 거래 제안. Want 를 비우면 기부다.
// 인덱스 배열은 빈 값이 정상이라 omitempty 를 쓰지 않는다.
type BZOfferPayload struct {
	ToSeat      int   `json:"toSeat"`
	GiveHand    []int `json:"giveHand"`
	GiveFlipped []int `json:"giveFlipped"`
	WantHand    []int `json:"wantHand"`
}

type BZRespondPayload struct {
	OfferID string `json:"offerId"`
	Accept  bool   `json:"accept"`
}

// BZPlantReceivedPayload ③ 받은 카드 심기 — 어느 카드를 어느 밭에.
// 인덱스 0 유실을 막기 위해 omitempty 금지.
type BZPlantReceivedPayload struct {
	CardIndex int `json:"cardIndex"`
	Field     int `json:"field"`
}

type BZReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

// BZPlayerView 좌석별 공개 정보 — 좌석 0·금화 0·장수 0 유실 방지를 위해
// omitempty 금지. 콩밭은 전원 공개, 손패는 장수만 나간다.
type BZPlayerView struct {
	Seat       int       `json:"seat"`
	Name       string    `json:"name"`
	Connected  bool      `json:"connected"`
	Bot        bool      `json:"bot"`
	Coins      int       `json:"coins"`
	HandCount  int       `json:"handCount"`
	FieldCount int       `json:"fieldCount"`
	Fields     []BZField `json:"fields"`
}

// BZOfferView 진행 중 제안 한 줄. 시야에 따라 실리는 키가 다르다.
//
//	제3자·관전자 : id · fromSeat · toSeat 뿐 (상세는 키 자체가 없다)
//	당사자 둘     : + giveHand · giveFlipped (콩 종류) · wantHand (인덱스)
//	요청받은 사람 : + wantBeans (그 자리에 실제로 뭐가 있는지)
//
// **wantBeans 를 제안자에게 주면 은닉이 깨진다** — "네 2번 카드를 달라"는
// 제안을 반복하며 상대 손패를 수락 없이 훑어낼 수 있기 때문이다. 그래서
// 요구는 당사자에게도 **자리(인덱스)로만** 보이고, 그 자리에 무엇이 있는지는
// **카드의 주인에게만** 실린다.
type BZOfferView struct {
	ID       string `json:"id"`
	FromSeat int    `json:"fromSeat"`
	ToSeat   int    `json:"toSeat"`
	// GiveHand·GiveFlipped 제안자가 내주는 카드 (당사자 둘에게만)
	GiveHand    *[]BZBean `json:"giveHand,omitempty"`
	GiveFlipped *[]BZBean `json:"giveFlipped,omitempty"`
	// WantHand 요구한 상대 손패의 0-based 자리 (당사자 둘에게만)
	WantHand *[]int `json:"wantHand,omitempty"`
	// WantBeans 그 자리에 실제로 있는 콩 — **요청받은 사람에게만**
	WantBeans *[]BZBean `json:"wantBeans,omitempty"`
}

// BZGameStatePayload 개인화된 전체 게임 스냅샷. 모든 상태 변경 후 좌석마다
// 따로 만들어 보낸다. 재접속 복원도 같은 페이로드를 쓴다.
//
// 은닉: YourHand·YourPending 은 본인에게만 실린다 — 타인·관전자
// (viewerSeat -1)의 raw JSON 에는 키 자체가 없다. 빈 손패도 [] 로 보내야
// 하므로 슬라이스 포인터로 부재를 표현한다.
// fields·coins·flipped 는 전원 공개다.
type BZGameStatePayload struct {
	GameID   string  `json:"gameId"`
	RoomCode string  `json:"roomCode"`
	Phase    BZPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"` // 관전자는 -1
	// Spectators 관전자 수
	Spectators int `json:"spectators"`
	// EndsAt 현재 대기 상태의 마감 시각 (unixMillis, 그 외 0)
	EndsAt      int64 `json:"endsAt"`
	CurrentSeat int   `json:"currentSeat"`
	// DeckLeft 덱에 남은 장수, DeckCycle 덱을 몇 번 소진했는지 (0~3)
	DeckLeft  int `json:"deckLeft"`
	DeckCycle int `json:"deckCycle"`
	// Flipped 2단계 공개 카드 (전원 공개, 빈 경우도 [])
	Flipped []BZBean `json:"flipped"`
	// Offers 진행 중 제안 (빈 경우도 [])
	Offers []BZOfferView `json:"offers"`
	// YourHand 본인 손패 — **순서가 곧 진실이다**. 본인에게만.
	YourHand *[]BZBean `json:"yourHand,omitempty"`
	// YourPending 본인이 심어야 할 받은 카드 — 본인에게만.
	YourPending *[]BZBean      `json:"yourPending,omitempty"`
	Players     []BZPlayerView `json:"players"`
	LastAction  *BZLastAction  `json:"lastAction"` // 그 전엔 null
	Result      *BZResult      `json:"result"`     // 종료 결과 (그 전엔 null)
}

// BZEventPayload 연출용 이벤트. 남의 손패 내용·덱 내용은 담지 않으며
// 전원에게 동일하게 간다. Seat 은 좌석 0 유실 방지를 위해 포인터다.
type BZEventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

// BZGameOverPayload 게임 종료 발표 — 정산 표와 최종 상태가 함께 간다
type BZGameOverPayload struct {
	Rows        []BZResultRow  `json:"rows"`
	WinnerSeats []int          `json:"winnerSeats"`
	WinnerNames []string       `json:"winnerNames"`
	Message     string         `json:"message"`
	Turns       int            `json:"turns"`
	Players     []BZPlayerView `json:"players"`
}

type BZPlayerDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type BZPlayerReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type BZErrorPayload struct {
	Message string `json:"message"`
}
