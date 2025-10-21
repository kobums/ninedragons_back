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
