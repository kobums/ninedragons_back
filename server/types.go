package server

import "github.com/gorilla/websocket"

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
)

// Client 구조체
type Client struct {
	ID     string
	Name   string
	Conn   *websocket.Conn
	Hub    *Hub
	Send   chan []byte
	GameID string
	Color  PlayerColor
}

// Game 구조체
type Game struct {
	ID            string
	Players       map[PlayerColor]*Client
	CurrentRound  int
	BlueWins      int
	RedWins       int
	UsedTiles     map[PlayerColor][]int
	CurrentPlayer PlayerColor
	RoundTiles    map[PlayerColor]*int
	Ready         bool
}

// 메시지 구조체들
type Message struct {
	Type    MessageType `json:"type"`
	Payload interface{} `json:"payload,omitempty"`
}

type JoinGamePayload struct {
	PlayerName string      `json:"playerName"`
	Color      PlayerColor `json:"color"`
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

type TilePlayedPayload struct {
	Color        PlayerColor `json:"color"`
	Tile         int         `json:"tile"`
	Round        int         `json:"round"`
	NextPlayer   PlayerColor `json:"nextPlayer"`
	WaitingFor   PlayerColor `json:"waitingFor"`
	BlueTilePlayed bool      `json:"blueTilePlayed"`
	RedTilePlayed  bool      `json:"redTilePlayed"`
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
	NCMsgJoinGame       NCMessageType = "nc_join_game"
	NCMsgGameStart      NCMessageType = "nc_game_start"
	NCMsgSubmitBlocks   NCMessageType = "nc_submit_blocks"
	NCMsgSelectBlock    NCMessageType = "nc_select_block"
	NCMsgRoundResult    NCMessageType = "nc_round_result"
	NCMsgGameOver       NCMessageType = "nc_game_over"
	NCMsgError          NCMessageType = "nc_error"
	NCMsgPlayerJoined   NCMessageType = "nc_player_joined"
	NCMsgWaitingPlayer  NCMessageType = "nc_waiting_player"
	NCMsgUseHidden      NCMessageType = "nc_use_hidden"
)

// NCClient 넘버체인지 클라이언트
type NCClient struct {
	ID         string
	Name       string
	Conn       *websocket.Conn
	Hub        *NCHub
	Send       chan []byte
	GameID     string
	Team       TeamColor
}

// NCGame 넘버체인지 게임
type NCGame struct {
	ID              string
	Players         map[TeamColor]*NCClient
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
	Round             int       `json:"round"`
	Team1Block1       int       `json:"team1Block1"`
	Team1Block2       int       `json:"team1Block2"`
	Team1Total        int       `json:"team1Total"`
	Team2Block1       int       `json:"team2Block1"`
	Team2Block2       int       `json:"team2Block2"`
	Team2Total        int       `json:"team2Total"`
	Winner            TeamColor `json:"winner"`
	Team1Hidden       bool      `json:"team1Hidden"`
	Team2Hidden       bool      `json:"team2Hidden"`
	Team1ReceivedBlock int      `json:"team1ReceivedBlock"`
	Team2ReceivedBlock int      `json:"team2ReceivedBlock"`
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
	Round             int       `json:"round"`
	Team1Block1       int       `json:"team1Block1"`
	Team1Block2       int       `json:"team1Block2"`
	Team1Total        int       `json:"team1Total"`
	Team2Block1       int       `json:"team2Block1"`
	Team2Block2       int       `json:"team2Block2"`
	Team2Total        int       `json:"team2Total"`
	Winner            TeamColor `json:"winner"`
	Team1Score        int       `json:"team1Score"`
	Team2Score        int       `json:"team2Score"`
	Team1Hidden       bool      `json:"team1Hidden"`
	Team2Hidden       bool      `json:"team2Hidden"`
	Team1ReceivedBlock int      `json:"team1ReceivedBlock"`
	Team2ReceivedBlock int      `json:"team2ReceivedBlock"`
	NextTeam          TeamColor `json:"nextTeam"`
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
	YourTeam   TeamColor `json:"yourTeam"`
	FirstTeam  TeamColor `json:"firstTeam"`
	Team1Name  string    `json:"team1Name"`
	Team2Name  string    `json:"team2Name"`
}

// NCErrorPayload 에러
type NCErrorPayload struct {
	Message string `json:"message"`
}
