package server

import (
	"time"
)

// ==================== DaVinci Code Game Types ====================

// DVTileColor 타일 색
type DVTileColor string

const (
	DVBlack DVTileColor = "black"
	DVWhite DVTileColor = "white"
)

// DVJokerValue 조커 타일의 값. 정렬 비교에서 제외되며 추리 시에도 이 값으로 선언한다.
const DVJokerValue = -1

// DVMaxPlayers 최대 인원
const DVMaxPlayers = 4

// DVPhase 게임 진행 단계. 서버가 지금 어떤 입력을 기다리는지 명시한다.
type DVPhase string

const (
	DVPhaseLobby           DVPhase = "lobby"
	DVPhaseInitialDraw     DVPhase = "initial_draw"      // 시작 타일을 색 골라 가져가는 단계 (전원 동시)
	DVPhaseJokerSetup      DVPhase = "joker_setup"       // 초기 배분 조커의 위치 선택 대기
	DVPhaseDraw            DVPhase = "draw"              // 현재 턴 플레이어의 뽑기 대기
	DVPhaseGuess           DVPhase = "guess"             // 추리 대기
	DVPhaseContinueChoice  DVPhase = "continue_choice"   // 추리 성공 후 계속/중단 선택 대기
	DVPhasePlaceDrawnJoker DVPhase = "place_drawn_joker" // 뽑은 조커의 삽입 위치 선택 대기
	DVPhaseRevealOwn       DVPhase = "reveal_own"        // 더미 소진 상태에서 추리 실패 → 공개할 자기 타일 선택 대기
	DVPhaseGameOver        DVPhase = "game_over"
)

// DVMessageType 다빈치코드 메시지 타입
type DVMessageType string

const (
	// 클라이언트 → 서버
	DVMsgJoinLobby      DVMessageType = "dv_join_lobby"
	DVMsgLeaveLobby     DVMessageType = "dv_leave_lobby"
	DVMsgStartGame      DVMessageType = "dv_start_game"
	DVMsgRejoinGame     DVMessageType = "dv_rejoin_game"
	DVMsgPlaceJoker     DVMessageType = "dv_place_joker"
	DVMsgTakeInitial    DVMessageType = "dv_take_initial"
	DVMsgDrawTile       DVMessageType = "dv_draw_tile"
	DVMsgGuess          DVMessageType = "dv_guess"
	DVMsgContinueChoice DVMessageType = "dv_continue_choice"
	DVMsgRevealOwn      DVMessageType = "dv_reveal_own"
	DVMsgReact          DVMessageType = "dv_react"

	// 서버 → 클라이언트
	DVMsgLobbyState         DVMessageType = "dv_lobby_state"
	DVMsgSpectateJoined     DVMessageType = "dv_spectate_joined"
	DVMsgGameState          DVMessageType = "dv_game_state"
	DVMsgEvent              DVMessageType = "dv_event"
	DVMsgGameOver           DVMessageType = "dv_game_over"
	DVMsgPlayerDisconnected DVMessageType = "dv_player_disconnected"
	DVMsgPlayerReconnected  DVMessageType = "dv_player_reconnected"
	DVMsgSessionExpired     DVMessageType = "dv_session_expired"
	DVMsgError              DVMessageType = "dv_error"
)

// DVTile 타일 하나. Value 는 0~11, 조커는 DVJokerValue(-1)에 Joker=true.
type DVTile struct {
	ID       int
	Color    DVTileColor
	Value    int
	Joker    bool
	Revealed bool
}

// DVPlayer 게임 참가자. 클라이언트 연결을 참조하지 않는 순수 상태다 —
// 좌석(Seat)↔연결 매핑은 허브의 dvRoom 이 담당한다.
type DVPlayer struct {
	Seat       int
	Name       string
	Tiles      []DVTile // 슬라이스 순서 = 왼쪽→오른쪽 배치 순서
	Eliminated bool
}

// DVGame 다빈치코드 게임 상태 (순수, 허브 비의존)
type DVGame struct {
	ID          string
	Players     []*DVPlayer
	Deck        []DVTile
	Phase       DVPhase
	CurrentSeat int
	// DrawnTile 이번 턴에 뽑아 아직 줄에 넣지 않은 타일
	DrawnTile *DVTile
	// DrawnJokerRevealed place_drawn_joker 단계에서 공개 삽입(추리 실패)인지
	// 비공개 삽입(중단)인지 구분
	DrawnJokerRevealed bool
	// PendingJokerTiles joker_setup 단계에서 좌석별 배치 대기 조커.
	// 줄(Tiles)에 임시로 섞어두면 배치 후 위치 이동이 상대에게 보여
	// 조커 위치가 새므로, 배치 전에는 줄 밖에 따로 보관한다.
	PendingJokerTiles map[int][]DVTile
	// InitialRemaining initial_draw 단계에서 좌석별로 아직 가져와야 하는
	// 시작 타일 수. 전원 0이 되면 다음 단계로 넘어간다.
	InitialRemaining map[int]int
	WinnerSeat       int // -1 = 미정
	Ready            bool
	StartedAt        time.Time
}

// DVClient 다빈치코드 클라이언트 연결
type DVClient struct {
	wsClient
	Hub  *DVHub
	Seat int
}

// DVMessage 메시지 봉투
type DVMessage struct {
	Type    DVMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// ==================== 클라이언트 → 서버 payload ====================

type DVJoinLobbyPayload struct {
	PlayerName string `json:"playerName"`
	// Room 선택 필드 — ""(생략)=공용 로비, "NEW"=새 사설 방 생성(코드 발급),
	// 4자 코드=해당 사설 방 입장 (없으면 그 코드로 새로 생성)
	Room string `json:"room,omitempty"`
}

type DVRejoinGamePayload struct {
	SessionID string `json:"sessionId"`
}

// DVPlaceJokerPayload 조커 배치. Position 은 삽입 인덱스(0 = 맨 왼쪽).
type DVPlaceJokerPayload struct {
	TileID   int `json:"tileId"`
	Position int `json:"position"`
}

// DVGuessPayload 추리. Value 는 0~11 또는 조커 추리 시 DVJokerValue(-1).
type DVGuessPayload struct {
	TargetSeat int `json:"targetSeat"`
	TileIndex  int `json:"tileIndex"`
	Value      int `json:"value"`
}

type DVContinueChoicePayload struct {
	Continue bool `json:"continue"`
}

// DVDrawTilePayload 뽑을 타일 색 선택. 원작에서 더미는 뒷면(색만 보임)으로
// 펼쳐져 있고 원하는 타일을 집는다 — 같은 색끼리는 구분이 안 되므로
// 색 선택과 동치다.
type DVDrawTilePayload struct {
	Color DVTileColor `json:"color"`
}

// DVTakeInitialPayload 시작 타일 가져오기 색 선택. 원작 셋업에서도
// 각자 원하는 타일을 가져가므로 흑/백 구성을 본인이 정한다.
type DVTakeInitialPayload struct {
	Color DVTileColor `json:"color"`
}

type DVRevealOwnPayload struct {
	TileIndex int `json:"tileIndex"`
}

// DVReactPayload 리액션 이모지 (화이트리스트 6종 외는 조용히 무시)
type DVReactPayload struct {
	Emoji string `json:"emoji"`
}

// ==================== 서버 → 클라이언트 payload ====================

// DVLobbyPlayer 로비 인원 목록 항목
type DVLobbyPlayer struct {
	Seat      int    `json:"seat"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
}

// DVLobbyStatePayload 로비 상태. 입장/퇴장/호스트 변경 시마다 전원에게 재전송.
type DVLobbyStatePayload struct {
	GameID string `json:"gameId"`
	// RoomCode 사설 방 초대 코드 (공용 로비는 "") — 대기실 코드 표시용
	// (다빈치는 player_joined 대신 lobby_state 가 그 역할을 한다)
	RoomCode  string          `json:"roomCode"`
	YourSeat  int             `json:"yourSeat"`
	SessionID string          `json:"sessionId"`
	HostSeat  int             `json:"hostSeat"`
	Players   []DVLobbyPlayer `json:"players"`
	CanStart  bool            `json:"canStart"`
}

// DVTileView 수신자 관점에서 마스킹된 타일. 남의 비공개 타일은
// Value/Joker 필드가 생략되고, 셋업 중에는 Color 까지 생략된다.
type DVTileView struct {
	ID       int         `json:"id"`
	Color    DVTileColor `json:"color,omitempty"`
	Value    *int        `json:"value,omitempty"`
	Joker    *bool       `json:"joker,omitempty"`
	Revealed bool        `json:"revealed"`
}

// DVPlayerView 수신자 관점의 플레이어 상태
type DVPlayerView struct {
	Seat       int    `json:"seat"`
	Name       string `json:"name"`
	Connected  bool   `json:"connected"`
	Eliminated bool   `json:"eliminated"`
	// InitialRemaining initial_draw 단계에서 아직 가져오지 않은 시작 타일 수
	InitialRemaining int          `json:"initialRemaining,omitempty"`
	Tiles            []DVTileView `json:"tiles"`
}

// DVGameStatePayload 개인화된 전체 게임 스냅샷. 모든 상태 변경 후
// 좌석마다 따로 만들어 보낸다. 재접속 복원도 같은 페이로드를 쓴다.
type DVGameStatePayload struct {
	GameID string `json:"gameId"`
	// RoomCode 사설 방 초대 코드 (공용 로비는 "") — 재접속 복원용
	RoomCode       string  `json:"roomCode"`
	YourSeat       int     `json:"yourSeat"`
	Phase          DVPhase `json:"phase"`
	CurrentSeat    int     `json:"currentSeat"`
	DeckCount      int     `json:"deckCount"`
	DeckBlackCount int     `json:"deckBlackCount"`
	DeckWhiteCount int     `json:"deckWhiteCount"`
	PlayerCount    int     `json:"playerCount"`
	// YourPendingJokers 내가 아직 배치하지 않은 초기 조커 (본인에게만 값 포함)
	YourPendingJokers []DVTileView   `json:"yourPendingJokers,omitempty"`
	DrawnTile         *DVTileView    `json:"drawnTile,omitempty"`
	Players           []DVPlayerView `json:"players"`
	// Spectators 관전자 수 (참가자·관전자 모두 표시용)
	Spectators int `json:"spectators"`
}

// DVEventPayload 연출용 이벤트. 비밀 정보를 담지 않으며 전원에게 동일하게 간다.
type DVEventPayload struct {
	Kind string `json:"kind"`
	// guess_made
	GuesserSeat  int   `json:"guesserSeat"`
	TargetSeat   int   `json:"targetSeat"`
	TileIndex    int   `json:"tileIndex"`
	GuessedValue *int  `json:"guessedValue,omitempty"`
	Correct      *bool `json:"correct,omitempty"`
	// game_started / tile_drawn / joker_placed / turn_stopped /
	// tile_revealed / player_eliminated / player_forfeited
	Seat  *int        `json:"seat,omitempty"`
	Color DVTileColor `json:"color,omitempty"`
	Value *int        `json:"value,omitempty"`
	// react — 발신자 이름과 이모지
	Name    string `json:"name,omitempty"`
	Message string `json:"message,omitempty"`
}

// DVGameOverPayload 게임 종료. 전 플레이어의 타일을 전부 공개해 보낸다.
type DVGameOverPayload struct {
	WinnerSeat int            `json:"winnerSeat"`
	WinnerName string         `json:"winnerName"`
	Reason     string         `json:"reason"` // "last_standing" | "forfeit_win"
	Players    []DVPlayerView `json:"players"`
}

type DVPlayerDisconnectedPayload struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	GraceSeconds int    `json:"graceSeconds"`
}

type DVPlayerReconnectedPayload struct {
	Seat int    `json:"seat"`
	Name string `json:"name"`
}

type DVErrorPayload struct {
	Message string `json:"message"`
}
