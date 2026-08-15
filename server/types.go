package server

import (
	"time"
)

// Player 색상
type PlayerColor string

const (
	Blue PlayerColor = "blue"
	Red  PlayerColor = "red"
)

// 메시지 타입
type MessageType string

const (
	MsgJoinGame      MessageType = "join_game"
	MsgGameStart     MessageType = "game_start"
	MsgPlayTile      MessageType = "play_tile"
	MsgTilePlayed    MessageType = "tile_played"
	MsgRoundResult   MessageType = "round_result"
	MsgGameOver      MessageType = "game_over"
	MsgTimeout       MessageType = "timeout"
	MsgError         MessageType = "error"
	MsgPlayerJoined  MessageType = "player_joined"
	MsgWaitingPlayer MessageType = "waiting_player"

	// 재대결 관련
	MsgRematch      MessageType = "rematch"
	MsgRematchOffer MessageType = "rematch_offer"

	// 재접속 관련
	MsgRejoinGame           MessageType = "rejoin_game"
	MsgGameState            MessageType = "game_state"
	MsgOpponentDisconnected MessageType = "opponent_disconnected"
	MsgOpponentReconnected  MessageType = "opponent_reconnected"
	MsgSessionExpired       MessageType = "session_expired"
)

// Client 구조체
type Client struct {
	wsClient
	Hub   *Hub
	Color PlayerColor
}

// Game 구조체 (순수 상태 — 연결은 ndRoom 이 든다)
type Game struct {
	ID            string
	Names         map[PlayerColor]string
	CurrentRound  int
	BlueWins      int
	RedWins       int
	UsedTiles     map[PlayerColor][]int
	CurrentPlayer PlayerColor
	RoundTiles    map[PlayerColor]*int
	Ready         bool
	StartedAt     time.Time
}

// 메시지 구조체들
type Message struct {
	Type    MessageType `json:"type"`
	Payload interface{} `json:"payload,omitempty"`
}

type JoinGamePayload struct {
	PlayerName string      `json:"playerName"`
	Color      PlayerColor `json:"color"`
	// VsBot true 면 대기 슬롯을 거치지 않고 연습봇과 즉시 매칭
	VsBot bool `json:"vsBot,omitempty"`
}

type PlayTilePayload struct {
	Tile int `json:"tile"`
}

type RoundResultPayload struct {
	Round      int         `json:"round"`
	BlueTile   int         `json:"blueTile"`
	RedTile    int         `json:"redTile"`
	Winner     PlayerColor `json:"winner"`
	BlueWins   int         `json:"blueWins"`
	RedWins    int         `json:"redWins"`
	NextPlayer PlayerColor `json:"nextPlayer"`
}

type GameOverPayload struct {
	Winner   PlayerColor `json:"winner"`
	BlueWins int         `json:"blueWins"`
	RedWins  int         `json:"redWins"`
}

type GameStartPayload struct {
	FirstPlayer PlayerColor `json:"firstPlayer"`
	YourColor   PlayerColor `json:"yourColor"`
	BlueName    string      `json:"blueName"`
	RedName     string      `json:"redName"`
}

type ErrorPayload struct {
	Message string `json:"message"`
}

type RejoinGamePayload struct {
	SessionID string `json:"sessionId"`
}

type OpponentDisconnectedPayload struct {
	Message      string `json:"message"`
	GraceSeconds int    `json:"graceSeconds"`
}

// RoundHistoryEntry 완료된 라운드 기록 (재접속 상태 복원용)
type RoundHistoryEntry struct {
	Round    int         `json:"round"`
	BlueTile int         `json:"blueTile"`
	RedTile  int         `json:"redTile"`
	Winner   PlayerColor `json:"winner"`
}

// GameStatePayload 재접속한 플레이어에게 보내는 게임 전체 상태
type GameStatePayload struct {
	GameID            string              `json:"gameId"`
	YourColor         PlayerColor         `json:"yourColor"`
	CurrentRound      int                 `json:"currentRound"`
	BlueWins          int                 `json:"blueWins"`
	RedWins           int                 `json:"redWins"`
	BlueName          string              `json:"blueName"`
	RedName           string              `json:"redName"`
	CurrentPlayer     PlayerColor         `json:"currentPlayer"`
	BlueUsedTiles     []int               `json:"blueUsedTiles"`
	RedUsedTiles      []int               `json:"redUsedTiles"`
	BlueRoundTile     *int                `json:"blueRoundTile"`
	RedRoundTile      *int                `json:"redRoundTile"`
	RoundHistory      []RoundHistoryEntry `json:"roundHistory"`
	OpponentConnected bool                `json:"opponentConnected"`
}

type TilePlayedPayload struct {
	Color          PlayerColor `json:"color"`
	Tile           int         `json:"tile"`
	Round          int         `json:"round"`
	NextPlayer     PlayerColor `json:"nextPlayer"`
	WaitingFor     PlayerColor `json:"waitingFor"`
	BlueTilePlayed bool        `json:"blueTilePlayed"`
	RedTilePlayed  bool        `json:"redTilePlayed"`
}

// ==================== NumberChange Game Types ====================

// TeamColor 팀 색상
type TeamColor string

const (
	Team1 TeamColor = "team1"
	Team2 TeamColor = "team2"
)

// NCMessageType 넘버체인지 메시지 타입
type NCMessageType string

const (
	NCMsgJoinGame      NCMessageType = "nc_join_game"
	NCMsgGameStart     NCMessageType = "nc_game_start"
	NCMsgSubmitBlocks  NCMessageType = "nc_submit_blocks"
	NCMsgSelectBlock   NCMessageType = "nc_select_block"
	NCMsgRoundResult   NCMessageType = "nc_round_result"
	NCMsgGameOver      NCMessageType = "nc_game_over"
	NCMsgError         NCMessageType = "nc_error"
	NCMsgPlayerJoined  NCMessageType = "nc_player_joined"
	NCMsgWaitingPlayer NCMessageType = "nc_waiting_player"
	NCMsgUseHidden     NCMessageType = "nc_use_hidden"

	// 재대결 관련
	NCMsgRematch      NCMessageType = "nc_rematch"
	NCMsgRematchOffer NCMessageType = "nc_rematch_offer"

	// 재접속 관련
	NCMsgRejoinGame           NCMessageType = "nc_rejoin_game"
	NCMsgGameState            NCMessageType = "nc_game_state"
	NCMsgOpponentDisconnected NCMessageType = "nc_opponent_disconnected"
	NCMsgOpponentReconnected  NCMessageType = "nc_opponent_reconnected"
	NCMsgSessionExpired       NCMessageType = "nc_session_expired"
)

// NCClient 넘버체인지 클라이언트
type NCClient struct {
	wsClient
	Hub  *NCHub
	Team TeamColor
}

// NCGame 넘버체인지 게임
// NCGame (순수 상태 — 연결은 ncRoom 이 든다)
type NCGame struct {
	ID              string
	Names           map[TeamColor]string
	CurrentRound    int
	Team1Score      int
	Team2Score      int
	AvailableBlocks map[TeamColor][]int // 각 팀의 남은 블록
	RoundHistory    []NCRoundHistory
	CurrentTeam     TeamColor
	RoundSubmits    map[TeamColor]*NCSubmit
	Team1UsedHidden bool
	Team2UsedHidden bool
	Ready           bool
	StartedAt       time.Time
}

// NCSubmit 라운드 제출 정보
type NCSubmit struct {
	Block1              int
	Block2              int
	UseHidden           bool
	SelectedBlockChoice int // 히든 사용 시 선택 (1: 상대 블록1, 2: 상대 블록2)
}

// NCRoundHistory 라운드 히스토리
type NCRoundHistory struct {
	Round              int       `json:"round"`
	Team1Block1        int       `json:"team1Block1"`
	Team1Block2        int       `json:"team1Block2"`
	Team1Total         int       `json:"team1Total"`
	Team2Block1        int       `json:"team2Block1"`
	Team2Block2        int       `json:"team2Block2"`
	Team2Total         int       `json:"team2Total"`
	Winner             TeamColor `json:"winner"`
	Team1Hidden        bool      `json:"team1Hidden"`
	Team2Hidden        bool      `json:"team2Hidden"`
	Team1ReceivedBlock int       `json:"team1ReceivedBlock"`
	Team2ReceivedBlock int       `json:"team2ReceivedBlock"`
}

// NCMessage 넘버체인지 메시지
type NCMessage struct {
	Type    NCMessageType `json:"type"`
	Payload interface{}   `json:"payload,omitempty"`
}

// NCJoinGamePayload 게임 참가
type NCJoinGamePayload struct {
	PlayerName string    `json:"playerName"`
	Team       TeamColor `json:"team,omitempty"`
	// VsBot true 면 대기 슬롯을 거치지 않고 연습봇과 즉시 매칭
	VsBot bool `json:"vsBot,omitempty"`
}

// NCSubmitBlocksPayload 블록 제출
type NCSubmitBlocksPayload struct {
	Block1              int  `json:"block1"`
	Block2              int  `json:"block2"`
	UseHidden           bool `json:"useHidden,omitempty"`
	SelectedBlockChoice int  `json:"selectedBlockChoice,omitempty"` // 히든 사용 시 선택 (1 또는 2)
}

// NCSelectBlockPayload 블록 선택 (이미 제출한 후)
type NCSelectBlockPayload struct {
	SelectedBlockChoice int `json:"selectedBlockChoice"` // 히든 사용 시 선택 (1 또는 2)
}

// NCRoundResultPayload 라운드 결과
type NCRoundResultPayload struct {
	Round              int       `json:"round"`
	Team1Block1        int       `json:"team1Block1"`
	Team1Block2        int       `json:"team1Block2"`
	Team1Total         int       `json:"team1Total"`
	Team2Block1        int       `json:"team2Block1"`
	Team2Block2        int       `json:"team2Block2"`
	Team2Total         int       `json:"team2Total"`
	Winner             TeamColor `json:"winner"`
	Team1Score         int       `json:"team1Score"`
	Team2Score         int       `json:"team2Score"`
	Team1Hidden        bool      `json:"team1Hidden"`
	Team2Hidden        bool      `json:"team2Hidden"`
	Team1ReceivedBlock int       `json:"team1ReceivedBlock"`
	Team2ReceivedBlock int       `json:"team2ReceivedBlock"`
	NextTeam           TeamColor `json:"nextTeam"`
}

// NCGameOverPayload 게임 종료
type NCGameOverPayload struct {
	Winner     TeamColor `json:"winner"`
	Team1Score int       `json:"team1Score"`
	Team2Score int       `json:"team2Score"`
	Reason     string    `json:"reason"` // "score_limit", "rounds_complete", "overtime"
}

// NCGameStartPayload 게임 시작
type NCGameStartPayload struct {
	YourTeam  TeamColor `json:"yourTeam"`
	FirstTeam TeamColor `json:"firstTeam"`
	Team1Name string    `json:"team1Name"`
	Team2Name string    `json:"team2Name"`
}

// NCErrorPayload 에러
type NCErrorPayload struct {
	Message string `json:"message"`
}

// NCRejoinGamePayload 재접속 요청
type NCRejoinGamePayload struct {
	SessionID string `json:"sessionId"`
}

// NCOpponentDisconnectedPayload 상대 연결 끊김 알림
type NCOpponentDisconnectedPayload struct {
	Message      string `json:"message"`
	GraceSeconds int    `json:"graceSeconds"`
}

// NCGameStatePayload 재접속한 플레이어에게 보내는 게임 전체 상태
type NCGameStatePayload struct {
	GameID                      string           `json:"gameId"`
	YourTeam                    TeamColor        `json:"yourTeam"`
	CurrentRound                int              `json:"currentRound"`
	Team1Score                  int              `json:"team1Score"`
	Team2Score                  int              `json:"team2Score"`
	Team1Name                   string           `json:"team1Name"`
	Team2Name                   string           `json:"team2Name"`
	CurrentTeam                 TeamColor        `json:"currentTeam"`
	YourBlocks                  []int            `json:"yourBlocks"`
	OpponentBlocks              []int            `json:"opponentBlocks"`
	RoundHistory                []NCRoundHistory `json:"roundHistory"`
	YourUsedHidden              bool             `json:"yourUsedHidden"`
	OpponentUsedHidden          bool             `json:"opponentUsedHidden"`
	YouSubmitted                bool             `json:"youSubmitted"`
	OpponentSubmitted           bool             `json:"opponentSubmitted"`
	OpponentUsedHiddenThisRound bool             `json:"opponentUsedHiddenThisRound"`
	YourBlockChoiceMade         bool             `json:"yourBlockChoiceMade"`
	OpponentConnected           bool             `json:"opponentConnected"`
}
