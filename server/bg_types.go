package server

import "time"

// ==================== 뱅! (Bang!) 타입 ====================
//
// 4~7인 정체 은닉 + 거리·장비 서부극. 다인 결(kr_hub / ct_hub)을 그대로
// 복제한다 — 공용 로비 + 사설 방 코드 + 관전 + 리액션 + 재접속 유예 + 봇 대체.
//
// 용어는 정식 한국어판을 그대로 쓴다. 역할 넷은
//
//	보안관 · 부관 · 무법자 · 배신자
//
// 이고, 카드 이름표는 아래 bgCards 표의 Label 이 유일한 출처다 —
// 빗나감!(미스 아님) · 주점(술집 아님) · 기관총(개틀링 아님) · 캣 벌로우 ·
// 강탈! · 웰스파고 · 잡화점 · 술통 · 야생마 · 조준경. 와이어에 실리는 영문
// kind 값(bang miss beer saloon duel gatling indians stagecoach wellsfargo
// store catbalou panic barrel jail dynamite mustang scope schofield remington
// carabine winchester volcanic)은 고정이며 화면 표기만 한국어를 쓴다.
//
// 이 게임의 심장은 둘이다.
//
//	① 거리   — bg_game.go 의 bgBaseDistance / bgDistance 가 순수 함수로
//	           분리돼 있다. 탈락자는 원탁에서 빠지고, 양방향 중 짧은 쪽이
//	           기본이며, 대상의 야생마 +1 · 내 조준경 −1 로 보정된다.
//	② 카드   — 종류가 많아 switch 로 흩지 않고 bgCards 표 + bg_game.go 의
//	           처리 표(파일 상단 주석)로 묶었다.
//
// 은닉 계약: yourRole · yourHand 는 본인 스냅샷에만 실린다 (타인·관전자의
// raw JSON 에는 키 자체가 없다). 단 보안관의 역할은 시작부터 players[].role
// 로 전원에게 공개되고, 나머지 역할은 사망 시에만 공개된다.

const (
	BGMinPlayers = 4
	BGMaxPlayers = 7

	// BGFillBotTarget bg_fill_bots 가 채우는 목표 인원 — 채운 뒤 즉시 시작
	BGFillBotTarget = 5

	// BGBaseHP 기본 최대 체력 · BGSheriffBonusHP 보안관의 추가 체력
	BGBaseHP         = 4
	BGSheriffBonusHP = 1

	// BGDefaultRange 무기가 없을 때의 사거리
	BGDefaultRange = 1

	// BGDeckSize 기본판 덱 총 장수 (bgCards 의 Count 합 — 테스트가 지킨다)
	BGDeckSize = 80

	// BGOutlawBounty 무법자를 죽인 사람이 받는 보상 카드 수
	BGOutlawBounty = 3

	// BGDynamiteDamage 다이너마이트가 터졌을 때의 피해
	BGDynamiteDamage = 3

	// BGStagecoachDraw / BGWellsFargoDraw 역마차·웰스파고의 뽑기 장수
	BGStagecoachDraw = 2
	BGWellsFargoDraw = 3

	// BGTurnDraw 차례 시작에 뽑는 장수
	BGTurnDraw = 2

	// BGMaxTurns 안전 상한. 체력·카드가 유한해 규칙상 반드시 끝나지만,
	// 덱을 되섞어 쓰는 이상 병리적 교착이 이론상 남는다. 이 상한에 닿으면
	// 보안관 생존 여부로 판을 접는다 (bg_game.go finishByTurnLimit).
	BGMaxTurns = 240
)

// ==================== 역할 ====================

// BGRole 정체. 와이어 영문 값 고정 — 화면 표기만 한국어.
// 빈 문자열은 "아직 공개되지 않음"이다.
type BGRole string

const (
	BGRoleNone     BGRole = ""
	BGRoleSheriff  BGRole = "sheriff"  // 보안관
	BGRoleDeputy   BGRole = "deputy"   // 부관
	BGRoleOutlaw   BGRole = "outlaw"   // 무법자
	BGRoleRenegade BGRole = "renegade" // 배신자
)

// bgRoleLabel 역할의 한국어 표기
func bgRoleLabel(r BGRole) string {
	switch r {
	case BGRoleSheriff:
		return "보안관"
	case BGRoleDeputy:
		return "부관"
	case BGRoleOutlaw:
		return "무법자"
	case BGRoleRenegade:
		return "배신자"
	}
	return "?"
}

// bgRoleSetup 인원별 역할 구성.
//
//	4인 보안관1·무법자2·배신자1 / 5인 +부관1 /
//	6인 무법자3·부관1 / 7인 무법자3·부관2
//
// 목록의 첫 칸이 반드시 보안관이다 (섞은 뒤에도 보안관이 선이 되도록
// bg_game.go Start 가 보안관 좌석부터 차례를 연다).
var bgRoleSetup = map[int][]BGRole{
	4: {BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade},
	5: {BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade, BGRoleDeputy},
	6: {BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade,
		BGRoleDeputy},
	7: {BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade,
		BGRoleDeputy, BGRoleDeputy},
}

// ==================== 트럼프 무늬·숫자 ====================
//
// "뒤집기"(술통·감옥·다이너마이트)가 무늬와 숫자를 보므로 카드마다 갖는다.

// BGSuit 무늬 (와이어에도 이 기호가 그대로 실린다)
type BGSuit string

const (
	BGSpade   BGSuit = "♠"
	BGHeart   BGSuit = "♥"
	BGDiamond BGSuit = "♦"
	BGClub    BGSuit = "♣"
)

// bgSuits 무늬 네 종 (덱 생성 순서)
var bgSuits = [4]BGSuit{BGSpade, BGHeart, BGDiamond, BGClub}

// bgRanks 숫자 열세 종
var bgRanks = [13]string{"A", "2", "3", "4", "5", "6", "7", "8", "9", "10",
	"J", "Q", "K"}

// bgRankValue 숫자의 크기 (A=1 … K=13). 다이너마이트 판정(♠2~9)에 쓴다.
func bgRankValue(rank string) int {
	for i, r := range bgRanks {
		if r == rank {
			return i + 1
		}
	}
	return 0
}

// bgDynamiteBlows 다이너마이트가 터지는 뒤집기인가 — ♠ 2~9
func bgDynamiteBlows(c BGCard) bool {
	v := bgRankValue(c.Rank)
	return c.Suit == BGSpade && v >= 2 && v <= 9
}

// bgBarrelSaves 술통이 뱅!을 튕겨내는 뒤집기인가 — ♥
func bgBarrelSaves(c BGCard) bool { return c.Suit == BGHeart }

// bgJailEscapes 감옥에서 풀려나는 뒤집기인가 — ♥
func bgJailEscapes(c BGCard) bool { return c.Suit == BGHeart }

// ==================== 카드 ====================

// BGKind 카드 종류. 와이어 영문 값 고정.
type BGKind string

const (
	// 갈색 (즉시 사용, 63장)
	BGBang       BGKind = "bang"       // 뱅!
	BGMiss       BGKind = "miss"       // 빗나감!
	BGBeer       BGKind = "beer"       // 맥주
	BGSaloon     BGKind = "saloon"     // 주점
	BGDuel       BGKind = "duel"       // 결투
	BGGatling    BGKind = "gatling"    // 기관총
	BGIndians    BGKind = "indians"    // 인디언!
	BGStagecoach BGKind = "stagecoach" // 역마차
	BGWellsFargo BGKind = "wellsfargo" // 웰스파고
	BGStore      BGKind = "store"      // 잡화점
	BGCatBalou   BGKind = "catbalou"   // 캣 벌로우
	BGPanic      BGKind = "panic"      // 강탈!

	// 파란색 (장비, 17장)
	BGBarrel     BGKind = "barrel"     // 술통
	BGJail       BGKind = "jail"       // 감옥
	BGDynamite   BGKind = "dynamite"   // 다이너마이트
	BGMustang    BGKind = "mustang"    // 야생마
	BGScope      BGKind = "scope"      // 조준경
	BGSchofield  BGKind = "schofield"  // 스코필드 (사거리 2)
	BGRemington  BGKind = "remington"  // 레밍턴 (3)
	BGCarabine   BGKind = "carabine"   // 카빈 (4)
	BGWinchester BGKind = "winchester" // 윈체스터 (5)
	BGVolcanic   BGKind = "volcanic"   // 볼캐닉 (1, 뱅! 무제한)
)

// bgTargetRule 카드가 요구하는 대상의 종류. Play 가 이 규칙 하나로
// 대상 검증을 끝내므로 카드별 if 문이 흩어지지 않는다.
type bgTargetRule int

const (
	bgTargetNone     bgTargetRule = iota // 대상 없음 (주점·기관총·인디언!·뽑기·잡화점)
	bgTargetSelf                         // 자신에게 (맥주·내 장비)
	bgTargetInRange                      // 무기 사거리 안 1명 (뱅!)
	bgTargetAny                          // 거리 무관 타인 1명 (결투·캣 벌로우)
	bgTargetDist1                        // 거리 1 이내 타인 1명 (강탈!)
	bgTargetJail                         // 보안관 아닌 타인 1명 (감옥)
	bgTargetResponse                     // 차례에 직접 낼 수 없다 (빗나감!)
)

// 장비 칸. 같은 칸을 두 번 채울 수 없고, 무기만 교체가 허용된다.
const (
	bgSlotNone     = ""
	bgSlotWeapon   = "weapon"
	bgSlotBarrel   = "barrel"
	bgSlotMustang  = "mustang"
	bgSlotScope    = "scope"
	bgSlotDynamite = "dynamite"
	bgSlotJail     = "jail"
)

// bgCardDef 카드 종류 한 줄. 이 표가 이름표·장수·대상 규칙·장비 효과의
// 단일 출처다 (효과 본문은 bg_game.go 의 처리 표가 잇는다).
type bgCardDef struct {
	Kind BGKind
	// Label 한국어 이름표 — 화면·로그·이벤트 문구가 전부 이 값을 쓴다
	Label string
	// Blue 파란색(장비)인가. false 면 갈색(즉시 사용)이다.
	Blue bool
	// Count 기본판 장수
	Count int
	// Target 대상 규칙
	Target bgTargetRule
	// Slot 장비 칸 (갈색은 bgSlotNone)
	Slot string
	// Range 무기 사거리 (무기가 아니면 0)
	Range int
	// Unlimited 이 무기를 들면 뱅!을 차례당 몇 장이든 낼 수 있는가 (볼캐닉)
	Unlimited bool
}

// bgCards 기본판 80장 구성표. 갈색 63 + 파란색 17.
//
//	갈색  뱅!25 · 빗나감!12 · 맥주6 · 주점1 · 결투3 · 기관총1 · 인디언!2 ·
//	      역마차2 · 웰스파고1 · 잡화점2 · 캣 벌로우4 · 강탈!4
//	파랑  술통2 · 감옥3 · 다이너마이트1 · 야생마2 · 조준경1 ·
//	      스코필드3 · 레밍턴1 · 카빈1 · 윈체스터1 · 볼캐닉2
var bgCards = []bgCardDef{
	// ---- 갈색 (즉시 사용) ----
	{Kind: BGBang, Label: "뱅!", Count: 25, Target: bgTargetInRange},
	{Kind: BGMiss, Label: "빗나감!", Count: 12, Target: bgTargetResponse},
	{Kind: BGBeer, Label: "맥주", Count: 6, Target: bgTargetSelf},
	{Kind: BGSaloon, Label: "주점", Count: 1, Target: bgTargetNone},
	{Kind: BGDuel, Label: "결투", Count: 3, Target: bgTargetAny},
	{Kind: BGGatling, Label: "기관총", Count: 1, Target: bgTargetNone},
	{Kind: BGIndians, Label: "인디언!", Count: 2, Target: bgTargetNone},
	{Kind: BGStagecoach, Label: "역마차", Count: 2, Target: bgTargetNone},
	{Kind: BGWellsFargo, Label: "웰스파고", Count: 1, Target: bgTargetNone},
	{Kind: BGStore, Label: "잡화점", Count: 2, Target: bgTargetNone},
	{Kind: BGCatBalou, Label: "캣 벌로우", Count: 4, Target: bgTargetAny},
	{Kind: BGPanic, Label: "강탈!", Count: 4, Target: bgTargetDist1},

	// ---- 파란색 (장비) ----
	{Kind: BGBarrel, Label: "술통", Blue: true, Count: 2,
		Target: bgTargetSelf, Slot: bgSlotBarrel},
	{Kind: BGJail, Label: "감옥", Blue: true, Count: 3,
		Target: bgTargetJail, Slot: bgSlotJail},
	{Kind: BGDynamite, Label: "다이너마이트", Blue: true, Count: 1,
		Target: bgTargetSelf, Slot: bgSlotDynamite},
	{Kind: BGMustang, Label: "야생마", Blue: true, Count: 2,
		Target: bgTargetSelf, Slot: bgSlotMustang},
	{Kind: BGScope, Label: "조준경", Blue: true, Count: 1,
		Target: bgTargetSelf, Slot: bgSlotScope},
	{Kind: BGSchofield, Label: "스코필드", Blue: true, Count: 3,
		Target: bgTargetSelf, Slot: bgSlotWeapon, Range: 2},
	{Kind: BGRemington, Label: "레밍턴", Blue: true, Count: 1,
		Target: bgTargetSelf, Slot: bgSlotWeapon, Range: 3},
	{Kind: BGCarabine, Label: "카빈", Blue: true, Count: 1,
		Target: bgTargetSelf, Slot: bgSlotWeapon, Range: 4},
	{Kind: BGWinchester, Label: "윈체스터", Blue: true, Count: 1,
		Target: bgTargetSelf, Slot: bgSlotWeapon, Range: 5},
	{Kind: BGVolcanic, Label: "볼캐닉", Blue: true, Count: 2,
		Target: bgTargetSelf, Slot: bgSlotWeapon, Range: 1, Unlimited: true},
}

// bgDefIndex kind → 구성표 한 줄 (init 에서 한 번 만든다)
var bgDefIndex = func() map[BGKind]bgCardDef {
	out := make(map[BGKind]bgCardDef, len(bgCards))
	for _, d := range bgCards {
		out[d.Kind] = d
	}
	return out
}()

// bgDef 카드 종류의 구성표 한 줄 (없으면 두 번째 값 false)
func bgDef(kind BGKind) (bgCardDef, bool) {
	d, ok := bgDefIndex[kind]
	return d, ok
}

// bgLabel 카드 종류의 한국어 이름표 ("?" 는 미등록)
func bgLabel(kind BGKind) string {
	if d, ok := bgDefIndex[kind]; ok {
		return d.Label
	}
	return "?"
}

// BGCard 카드 한 장. 무늬·숫자는 뒤집기 판정에 쓰이므로 늘 함께 다닌다.
type BGCard struct {
	ID   int    `json:"id"`
	Kind BGKind `json:"kind"`
	Suit BGSuit `json:"suit"`
	Rank string `json:"rank"`
}

// ==================== 단계 ====================

// BGPhase 진행 단계 (상태기계는 bg_game.go 상단 주석 참고)
type BGPhase string

const (
	BGPhaseWaiting   BGPhase = "waiting"
	BGPhaseDraw      BGPhase = "draw"       // 차례 시작 판정 + 2장 뽑기 (자동)
	BGPhaseTurn      BGPhase = "turn"       // 카드 사용 (60초 마감)
	BGPhaseRespond   BGPhase = "respond"    // 대응 창 (20초 마감)
	BGPhaseStorePick BGPhase = "store_pick" // 잡화점 고르기 (15초)
	BGPhaseDiscard   BGPhase = "discard"    // 손패 줄이기 (15초)
	BGPhaseGameOver  BGPhase = "game_over"
)

// 대응 창이 요구하는 카드 (pending.need 의 와이어 값)
const (
	BGNeedMiss = "miss" // 빗나감!
	BGNeedBang = "bang" // 뱅!
	BGNeedPick = "pick" // 잡화점에서 한 장 고르기
)

// 대응 창의 종류 (pending.kind 의 와이어 값)
const (
	BGPendBang    = "bang"
	BGPendGatling = "gatling"
	BGPendIndians = "indians"
	BGPendDuel    = "duel"
	BGPendStore   = "store"
)

// ==================== 메시지 ====================

// BGMessageType 뱅! 메시지 타입
type BGMessageType string

const (
	// 클라이언트 → 서버
	BGMsgJoinGame BGMessageType = "bg_join_game"
	BGMsgFillBots BGMessageType = "bg_fill_bots"
	BGMsgStart    BGMessageType = "bg_start"
	BGMsgRejoin   BGMessageType = "bg_rejoin"
	BGMsgPlay     BGMessageType = "bg_play"
	BGMsgRespond  BGMessageType = "bg_respond"
	BGMsgPick     BGMessageType = "bg_pick"
	BGMsgDiscard  BGMessageType = "bg_discard"
	BGMsgEndTurn  BGMessageType = "bg_end_turn"
	BGMsgReact    BGMessageType = "bg_react"

	// 서버 → 클라이언트
	BGMsgPlayerJoined       BGMessageType = "bg_player_joined"
	BGMsgSpectateJoined     BGMessageType = "bg_spectate_joined"
	BGMsgGameState          BGMessageType = "bg_game_state"
	BGMsgEvent              BGMessageType = "bg_event"
	BGMsgGameOver           BGMessageType = "bg_game_over"
	BGMsgPlayerDisconnected BGMessageType = "bg_player_disconnected"
	BGMsgPlayerReconnected  BGMessageType = "bg_player_reconnected"
	BGMsgSessionExpired     BGMessageType = "bg_session_expired"
	BGMsgError              BGMessageType = "bg_error"
)

// BGClient 뱅! 클라이언트 연결
type BGClient struct {
	wsClient
	Hub  *BGHub
	Seat int
}

// BGMessage 메시지 봉투
type BGMessage struct {
	Type    BGMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type BGJoinGamePayload struct {
	Name string `json:"name"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type BGRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

// BGPlayPayload 손패의 index 번째 카드를 낸다.
//
// TargetSeat 는 좌석 0 을 "지정 없음"과 구분해야 하므로 포인터로 받는다.
// TargetCardIndex 는 캣 벌로우·강탈!이 집을 대상 카드의 자리다 — 대상의
// [손패 … 장비] 를 이어 붙인 한 축의 인덱스로, handCount 이상이면 장비를
// 가리킨다 (손패 내용은 공개되지 않으므로 손패 쪽 지목은 사실상 눈감고 집기).
type BGPlayPayload struct {
	Index           int  `json:"index"`
	TargetSeat      *int `json:"targetSeat,omitempty"`
	TargetCardIndex *int `json:"targetCardIndex,omitempty"`
}

// BGRespondPayload 대응 창의 응답. Index 생략은 포기다.
type BGRespondPayload struct {
	Index *int `json:"index,omitempty"`
}

// BGPickPayload 잡화점 공개분에서 고를 카드의 인덱스
type BGPickPayload struct {
	Index int `json:"index"`
}

// BGDiscardPayload 차례 끝 손패 줄이기 — 버릴 손패 인덱스 목록
type BGDiscardPayload struct {
	Indexes []int `json:"indexes"`
}

type BGReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 순수 상태 ====================

// BGPlayer 참가자 (순수 상태 — 연결 매핑은 허브의 bgRoom 담당)
type BGPlayer struct {
	Seat int
	Name string
	// Role 정체 — 보안관은 시작부터 공개, 나머지는 사망 시 공개
	Role BGRole
	// HP / MaxHP 체력 (보안관만 +1)
	HP    int
	MaxHP int
	// Alive 생존 여부. 탈락자는 거리 계산의 원탁에서 빠진다.
	Alive bool
	// Hand 손패 — 본인만 내용을 본다
	Hand []BGCard
	// Equipment 장비 (전원 공개)
	Equipment []BGCard
	// BangUsed 이번 차례에 낸 뱅! 장수 (볼캐닉이면 상한 없음)
	BangUsed int
}

// BGPending 대응 창. Queue 는 기관총·인디언!이 남은 대상을 도는 내부 장부라
// 와이어에 싣지 않는다 (passed 로 이미 끝난 좌석만 공개한다).
type BGPending struct {
	// Kind bang | gatling | indians | duel | store
	Kind string `json:"kind"`
	// BySeat 지금 요구하는 쪽 (결투는 마지막에 뱅!을 낸 쪽)
	BySeat int `json:"bySeat"`
	// TargetSeat 지금 응답해야 하는 좌석
	TargetSeat int `json:"targetSeat"`
	// Need miss | bang | pick
	Need string `json:"need"`
	// Passed 이미 처리가 끝난 좌석 (막았든 맞았든)
	Passed []int `json:"passed"`
	// Queue 아직 차례가 오지 않은 대상 (와이어 비노출)
	Queue []int `json:"-"`
}

// BGGameEvent 순수 규칙이 쌓는 연출 이벤트 — 허브가 DrainEvents 로 꺼내
// bg_event 로 방송한다 (손패·비공개 역할의 내용은 담지 않는다)
type BGGameEvent struct {
	Kind    string
	Seat    int // -1 없음
	Message string
}

// BGLastAction 마지막 행동 요약 (전원 공개)
type BGLastAction struct {
	Seat    int    `json:"seat"`
	Name    string `json:"name"`
	Message string `json:"message"`
}

// BGResult 종료 결과. Winner 는 진영 — sheriff | outlaw | renegade.
type BGResult struct {
	Winner      string   `json:"winner"`
	WinnerSeats []int    `json:"winnerSeats"`
	WinnerNames []string `json:"winnerNames"`
	Message     string   `json:"message"`
}

// BGGame 뱅! 게임 상태 (순수, 허브 비의존)
type BGGame struct {
	ID      string
	Players []*BGPlayer
	Phase   BGPhase

	// Deck 남은 덱 (앞에서 뽑는다). 마르면 버린 더미를 되섞는다.
	Deck []BGCard
	// DiscardPile 버린 더미 (마지막 장이 공개된 맨 위)
	DiscardPile []BGCard

	// CurrentSeat 지금 차례인 좌석 (-1 없음)
	CurrentSeat int
	// Turns 지금까지 열린 차례 수 (안전 상한 판정용)
	Turns int

	// Pending 열려 있는 대응 창 (없으면 nil)
	Pending *BGPending
	// StoreCards 잡화점으로 공개된 카드
	StoreCards []BGCard

	LastAction *BGLastAction
	Result     *BGResult
	Ready      bool
	StartedAt  time.Time

	// StateSeq 새 대기 상태가 열릴 때마다 +1 — 허브가 마감 타이머를 다시
	// 걸지 판단하는 근거
	StateSeq int
	// AfkSeq 마감 타이머 일련번호 (뒤늦은 발화 무시용 — 허브가 관리)
	AfkSeq int
	// Deadline 현재 대기 상태의 마감 시각 (unixMillis — 스냅샷 노출용)
	Deadline int64

	events []BGGameEvent
	rng    bgRand
}

// bgRand 게임이 쓰는 난수원의 최소 계약 (허브 고루틴에서만 호출)
type bgRand interface {
	Intn(n int) int
	Shuffle(n int, swap func(i, j int))
}

// ==================== 서버 → 클라이언트 payload ====================

// BGPlayerView 좌석별 공개 정보. 좌석 0·체력 0·거리 0 유실을 막기 위해
// omitempty 를 쓰지 않는다. 손패는 장수만 공개하고 내용은 담지 않는다.
type BGPlayerView struct {
	Seat      int    `json:"seat"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Bot       bool   `json:"bot"`
	// Alive 생존 여부
	Alive bool `json:"alive"`
	// HP / MaxHP 체력
	HP    int `json:"hp"`
	MaxHP int `json:"maxHp"`
	// HandCount 손패 장수 (내용은 본인만 본다)
	HandCount int `json:"handCount"`
	// Equipment 장비 (전원 공개)
	Equipment []BGCard `json:"equipment"`
	// Role 공개된 역할. 보안관은 시작부터, 나머지는 사망 시.
	// 아직 비공개면 빈 문자열이다.
	Role BGRole `json:"role"`
	// DistanceFromYou 뷰어 기준 거리 (장비 보정 포함). 자기 자신 0,
	// 관전자·탈락자처럼 정의되지 않으면 -1.
	DistanceFromYou int `json:"distanceFromYou"`
}

// BGGameStatePayload 개인화된 전체 게임 스냅샷. 모든 상태 변경 후 좌석마다
// 따로 만들어 보낸다. 재접속 복원도 같은 페이로드를 쓴다.
//
// 은닉: YourRole · YourHand 는 본인에게만 실린다 — 타인·관전자
// (viewerSeat -1)의 raw JSON 에는 키 자체가 없다. 빈 손패도 [] 로 보내야
// 하므로 슬라이스 포인터로 부재를 표현한다.
type BGGameStatePayload struct {
	GameID   string  `json:"gameId"`
	RoomCode string  `json:"roomCode"`
	Phase    BGPhase `json:"phase"`
	HostSeat int     `json:"hostSeat"`
	YourSeat int     `json:"yourSeat"` // 관전자는 -1
	// Spectators 관전자 수
	Spectators int `json:"spectators"`
	// EndsAt 현재 대기 상태의 마감 시각 (unixMillis, 그 외 0)
	EndsAt int64 `json:"endsAt"`
	// CurrentSeat 지금 차례인 좌석 (-1 없음)
	CurrentSeat int `json:"currentSeat"`
	// DeckLeft 덱에 남은 장수
	DeckLeft int `json:"deckLeft"`
	// DiscardTop 버린 더미의 맨 위 (없으면 null)
	DiscardTop *BGCard `json:"discardTop"`
	// Pending 열린 대응 창 (없으면 null)
	Pending *BGPending `json:"pending"`
	// StoreCards 잡화점 공개분 (빈 경우 [])
	StoreCards []BGCard `json:"storeCards"`
	// YourRole 본인 역할 — 본인에게만
	YourRole *BGRole `json:"yourRole,omitempty"`
	// YourHand 본인 손패 — 본인에게만
	YourHand *[]BGCard `json:"yourHand,omitempty"`
	// YourBangUsed 뱅!을 더 낼 수 없는가 — 본인에게만.
	//
	// 이름 그대로의 "이번 차례에 뱅!을 냈는가"가 아니라 **판정 결과**다:
	// 이미 한 장을 냈고 볼캐닉이 없으면 true, 볼캐닉을 들었으면 몇 장을
	// 내도 false. 프론트가 이 값 하나로 뱅! 버튼을 잠그면 서버 판정과
	// 정확히 일치한다.
	YourBangUsed *bool          `json:"yourBangUsed,omitempty"`
	Players      []BGPlayerView `json:"players"`
	LastAction   *BGLastAction  `json:"lastAction"` // 그 전엔 null
	Result       *BGResult      `json:"result"`     // 종료 결과 (그 전엔 null)
}

// BGEventPayload 연출용 이벤트. 손패·비공개 역할의 내용을 담지 않으며
// 전원에게 동일하게 간다. Seat 은 좌석 0 유실 방지를 위해 포인터다.
type BGEventPayload struct {
	Kind    string `json:"kind"`
	Seat    *int   `json:"seat,omitempty"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

// BGGameOverPayload 게임 종료 발표 (전원 역할 공개)
type BGGameOverPayload struct {
	Winner      string         `json:"winner"`
	WinnerSeats []int          `json:"winnerSeats"`
	WinnerNames []string       `json:"winnerNames"`
	Message     string         `json:"message"`
	Turns       int            `json:"turns"`
	Players     []BGPlayerView `json:"players"`
}

type BGPlayerDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type BGPlayerReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type BGErrorPayload struct {
	Message string `json:"message"`
}
