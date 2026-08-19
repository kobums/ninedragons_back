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

// SPCategoryRandom 대기실 기본 선택 — 시작 시 카테고리를 무작위로 정한다
const SPCategoryRandom = "랜덤"

// spCategoryNames 카테고리 표시 순서 (프론트와 공유하는 계약)
var spCategoryNames = []string{"장소", "직업", "과일", "음식", "동물", "스포츠", "영화", "나라", "브랜드"}

// spCategories 카테고리별 단어 24개 — 프론트와 공유하는 계약. 순서·표기 불변.
var spCategories = map[string][]string{
	"장소": {
		"병원", "학교", "은행", "해변", "카지노", "영화관", "지하철", "공항",
		"우주정거장", "잠수함", "경찰서", "소방서", "유람선", "호텔", "대사관",
		"슈퍼마켓", "레스토랑", "대학교", "군부대", "놀이공원", "목욕탕",
		"결혼식장", "도서관", "야구장",
	},
	"직업": {
		"의사", "교사", "소방관", "경찰관", "요리사", "프로그래머", "가수", "배우",
		"화가", "농부", "어부", "파일럿", "간호사", "변호사", "판사", "군인",
		"기자", "사진작가", "미용사", "택시기사", "우주비행사", "마술사",
		"운동선수", "건축가",
	},
	"과일": {
		"사과", "배", "포도", "수박", "참외", "딸기", "바나나", "오렌지",
		"귤", "복숭아", "자두", "체리", "키위", "망고", "파인애플", "레몬",
		"멜론", "블루베리", "석류", "감", "무화과", "코코넛", "용과", "리치",
	},
	"음식": {
		"김치찌개", "된장찌개", "불고기", "비빔밥", "삼겹살", "치킨", "피자",
		"햄버거", "짜장면", "짬뽕", "탕수육", "초밥", "라면", "떡볶이", "김밥",
		"순대", "파스타", "스테이크", "카레", "냉면", "삼계탕", "갈비탕",
		"족발", "보쌈",
	},
	"동물": {
		"강아지", "고양이", "호랑이", "사자", "코끼리", "기린", "원숭이", "판다",
		"토끼", "여우", "늑대", "곰", "하마", "악어", "펭귄", "돌고래",
		"고래", "상어", "독수리", "부엉이", "뱀", "거북이", "캥거루", "낙타",
	},
	"스포츠": {
		"축구", "야구", "농구", "배구", "테니스", "탁구", "배드민턴", "골프",
		"수영", "마라톤", "양궁", "태권도", "유도", "복싱", "스키",
		"스케이트", "컬링", "볼링", "당구", "클라이밍", "사이클", "펜싱",
		"서핑", "요가",
	},
	"영화": {
		"타이타닉", "어벤져스", "기생충", "인터스텔라", "인셉션", "라라랜드",
		"겨울왕국", "토이스토리", "해리포터", "반지의제왕", "스타워즈",
		"쥬라기공원", "매트릭스", "터미네이터", "어바웃타임", "노트북",
		"알라딘", "라이온킹", "미니언즈", "극한직업", "명량", "부산행",
		"올드보이", "아바타",
	},
	"나라": {
		"한국", "일본", "중국", "미국", "영국", "프랑스", "독일", "이탈리아",
		"스페인", "캐나다", "브라질", "호주", "인도", "태국", "베트남",
		"필리핀", "러시아", "멕시코", "이집트", "터키", "스위스", "네덜란드",
		"그리스", "아르헨티나",
	},
	"브랜드": {
		"삼성", "애플", "나이키", "아디다스", "스타벅스", "맥도날드",
		"코카콜라", "구글", "넷플릭스", "유튜브", "샤넬", "구찌", "루이비통",
		"이케아", "레고", "페라리", "벤츠", "BMW", "현대", "기아",
		"배달의민족", "카카오", "네이버", "무신사",
	},
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
	SPMsgJoinGame    SPMessageType = "sp_join_game"
	SPMsgFillBots    SPMessageType = "sp_fill_bots"
	SPMsgStart       SPMessageType = "sp_start"
	SPMsgSetTimer    SPMessageType = "sp_set_timer"
	SPMsgSetCategory SPMessageType = "sp_set_category"
	SPMsgGuess       SPMessageType = "sp_guess"
	SPMsgVote        SPMessageType = "sp_vote"
	SPMsgRejoin      SPMessageType = "sp_rejoin"
	SPMsgReact       SPMessageType = "sp_react"

	// 서버 → 클라이언트
	SPMsgPlayerJoined         SPMessageType = "sp_player_joined"
	SPMsgSpectateJoined       SPMessageType = "sp_spectate_joined"
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

	// CategoryChoice 대기실에서 host 가 고른 카테고리 (기본 랜덤)
	CategoryChoice string
	// Category 시작 시 확정된 카테고리 (랜덤이면 시작 때 추첨)
	Category string

	// Location 이번 판의 정답 단어 (확정 카테고리 목록 중 1개). 비스파이에게만 보인다.
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
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type SPRejoinPayload struct {
	SessionID string `json:"sessionId"`
}

type SPSetTimerPayload struct {
	Minutes int `json:"minutes"`
}

type SPSetCategoryPayload struct {
	Category string `json:"category"`
}

type SPGuessPayload struct {
	Location string `json:"location"`
}

type SPVotePayload struct {
	Target int `json:"target"`
}

// SPReactPayload 리액션 이모지 (화이트리스트 6종 외는 조용히 무시)
type SPReactPayload struct {
	Emoji string `json:"emoji"`
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
	Category        string `json:"category"`
	Location        string `json:"location"`
	Reason          string `json:"reason"` // guess_right|guess_wrong|vote_caught|vote_missed
	GuessedLocation string `json:"guessedLocation"`
	TopSeat         int    `json:"topSeat"` // -1 = 단독 최다 없음(동표)
}

// SPGameStatePayload 개인화 게임 스냅샷 (은닉형).
// 비스파이 스냅샷에는 스파이 좌석 정보가 어떤 형태로도 없어야 한다.
// isSpy 는 본인 것만, location 은 비스파이에게만 채운다 (스파이·waiting 은 빈).
type SPGameStatePayload struct {
	GameID string `json:"gameId"`
	// RoomCode 사설 방 초대 코드 (공용 로비는 "") — 대기실 코드 표시·재접속 복원용
	RoomCode     string  `json:"roomCode"`
	Phase        SPPhase `json:"phase"`
	HostSeat     int     `json:"hostSeat"`
	YourSeat     int     `json:"yourSeat"`
	TimerMinutes int     `json:"timerMinutes"`
	// CategoryChoice 대기실 host 선택 (waiting 표시용), Category 는 확정 카테고리
	CategoryChoice string         `json:"categoryChoice"`
	Category       string         `json:"category"`
	EndsAt         int64          `json:"endsAt"`    // playing 에만 >0
	Locations      []string       `json:"locations"` // playing 부터 확정 카테고리 단어 24개
	IsSpy          bool           `json:"isSpy"`
	Location       string         `json:"location"`
	Players        []SPPlayerView `json:"players"`
	// Spectators 관전자 수 (참가자·관전자 모두 표시용)
	Spectators int `json:"spectators"`
	Votes          []SPVoteView   `json:"votes"`
	Result         *SPResultView  `json:"result"`
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
	Category        string         `json:"category"`
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
