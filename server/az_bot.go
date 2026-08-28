package server

import (
	"fmt"
	"math/rand"
	"time"
)

// ==================== 아줄 연습봇 ====================
//
// 스냅샷(az_game_state)만 보고 반응한다. 아줄에는 은닉이 없으므로 봇도
// 사람과 정확히 같은 정보를 본다 — 유리함이 없다.
//
// 판단은 (출처, 색, 패턴 라인) 후보 전부를 평가해 최고점을 고르는 1수 탐색이다.
//
//   - 이번 라운드에 줄이 꽉 차면 벽에 붙였을 때의 인접 점수를 미리 계산해
//     그대로 가점하고, 완성 자체에도 가점한다.
//   - 줄이 덜 차면 완성 확률을 채운 비율로 근사해 기대 점수를 깎아 가점한다.
//   - 바닥으로 넘치는 만큼의 실제 감점을 감점표 그대로 계산해 감점한다.
//   - 세로줄·같은 색·가로줄 보너스 진행도를 약하게 가점한다.
//   - 선 플레이어 마커는 다음 라운드 선을 잡는 값어치만큼만 약하게 가점한다
//     (감점 -1은 위 감점 항목에 이미 들어가 있다).
//
// 같은 차례에 스냅샷이 여러 번 와도(관전 입장·접속 변화 등) 한 번만 두도록
// 상태 식별키로 중복을 걸러낸다.

// 봇이 "생각하는" 시간 (테스트에서 짧게 낮춘다)
var (
	azBotTakeDelay    = 700 * time.Millisecond
	azBotTakeJitterMs = 700
)

// 평가 가중치. 단위는 "점"이다 — 확정 점수(azBotCompleteWeight=1.0)를 기준으로
// 나머지를 상대적으로 잡았다. 3봇 30판 평균 점수로 조정한 값이다.
const (
	azBotCompleteWeight = 1.0  // 이번 라운드에 확정되는 벽 인접 점수
	azBotCompleteBonus  = 2.5  // 줄을 완성했다는 사실 자체의 값어치
	azBotPartialWeight  = 0.7  // 아직 덜 찬 줄의 기대 점수 (채운 비율로 감쇠)
	azBotFillWeight     = 0.45 // 패턴 라인에 한 장 더 놓는 값어치
	azBotWasteWeight    = 0.6  // 남은 빈 칸 한 칸당 미완성 위험
	azBotPenaltyWeight  = 1.6  // 바닥 감점 1점의 체감 무게
	azBotColBonusWeight = 0.9  // 세로줄(7점) 진행도 — 같은 열에 이미 있는 타일당
	azBotRowBonusWeight = 0.35 // 가로줄(2점) 진행도
	azBotColorWeight    = 0.7  // 같은 색 5장(10점) 진행도
	azBotFirstValue     = 0.8  // 선 플레이어 마커(다음 라운드 선)의 값어치
	azBotNoise          = 0.2  // 동점 후보를 갈라 주는 잡음 폭
)

// azBotBoard 봇이 평가에 쓰는 자기 개인 보드
type azBotBoard struct {
	Lines [AZWallSize]AZLine
	Wall  [AZWallSize][AZWallSize]bool
	Floor int // 바닥 라인에 놓인 장수 (선 마커 포함)
}

// azRowFilled 벽 가로줄에 이미 붙은 타일 수
func azRowFilled(wall [AZWallSize][AZWallSize]bool, row int) int {
	n := 0
	for c := 0; c < AZWallSize; c++ {
		if wall[row][c] {
			n++
		}
	}
	return n
}

// azColFilled 벽 세로줄에 이미 붙은 타일 수
func azColFilled(wall [AZWallSize][AZWallSize]bool, col int) int {
	n := 0
	for r := 0; r < AZWallSize; r++ {
		if wall[r][col] {
			n++
		}
	}
	return n
}

// azColorFilled 벽에 이미 붙은 그 색의 장수
func azColorFilled(wall [AZWallSize][AZWallSize]bool, color AZColor) int {
	n := 0
	for r := 0; r < AZWallSize; r++ {
		if azWallHasColor(wall, r, color) {
			n++
		}
	}
	return n
}

// azBotCanPlace 봇 평가용 배치 가능 여부 (azCanPlace 와 같은 규칙을
// AZPlayer 없이 판정한다 — 스냅샷만으로 후보를 세우기 위함)
func azBotCanPlace(me azBotBoard, line int, color AZColor) bool {
	if line == AZLineTargetFloor {
		return true
	}
	if line < 0 || line >= AZWallSize || !azIsTileColor(color) {
		return false
	}
	if azWallHasColor(me.Wall, line, color) {
		return false
	}
	cur := me.Lines[line]
	if cur.Color != AZColorNone && cur.Color != color {
		return false
	}
	return cur.Count < line+1
}

// azBotValue 후보 하나의 평가값. 규칙 위반 후보는 호출 전에 걸러 온다.
func azBotValue(me azBotBoard, took int, takesFirst bool, color AZColor, line int) float64 {
	placed, overflow := 0, took
	if line != AZLineTargetFloor {
		space := line + 1 - me.Lines[line].Count
		placed = took
		if placed > space {
			placed = space
		}
		overflow = took - placed
	}

	add := overflow
	if takesFirst {
		add++
	}
	free := AZFloorSlots - me.Floor
	if free < 0 {
		free = 0
	}
	if add > free {
		add = free
	}
	penalty := azFloorPenalty(me.Floor+add) - azFloorPenalty(me.Floor)

	value := -azBotPenaltyWeight * float64(penalty)
	if takesFirst {
		value += azBotFirstValue
	}
	if line == AZLineTargetFloor {
		return value
	}

	capacity := line + 1
	after := me.Lines[line].Count + placed
	col := azWallCol(line, color)
	wall := me.Wall
	wall[line][col] = true
	pts := float64(azPlaceScore(wall, line, col))

	if after == capacity {
		value += azBotCompleteWeight*pts + azBotCompleteBonus
		value += azBotColBonusWeight * float64(azColFilled(me.Wall, col))
		value += azBotRowBonusWeight * float64(azRowFilled(me.Wall, line))
		value += azBotColorWeight * float64(azColorFilled(me.Wall, color))
		return value
	}

	frac := float64(after) / float64(capacity)
	value += azBotPartialWeight * pts * frac
	value += azBotFillWeight * float64(placed)
	// 덜 찬 줄은 다음 차례에 못 채우면 헛수고다 — 남은 빈 칸만큼 위험을 뺀다
	value -= azBotWasteWeight * float64(capacity-after)
	return value
}

// azBotChoose 진열대·중앙과 자기 보드만으로 최선의 수를 고른다 (순수).
// rng 가 nil 이 아니면 동점 후보를 잡음으로 갈라 같은 판이 매번 똑같아지지
// 않게 한다. 타일이 남아 있으면 -1(전부 바닥)이 항상 후보라 반드시 고른다.
func azBotChoose(factories [][]AZColor, center []AZColor, centerHasFirst bool,
	me azBotBoard, rng *rand.Rand) (AZMove, bool) {
	best, bestValue, found := AZMove{}, 0.0, false

	consider := func(from string, tiles []AZColor, takesFirst bool) {
		for _, color := range azDistinctColors(tiles) {
			took := azCountColor(tiles, color)
			for line := AZLineTargetFloor; line < AZWallSize; line++ {
				if !azBotCanPlace(me, line, color) {
					continue
				}
				value := azBotValue(me, took, takesFirst, color, line)
				if rng != nil {
					value += (rng.Float64() - 0.5) * azBotNoise
				}
				if !found || value > bestValue {
					best, bestValue, found = AZMove{From: from, Color: color, Line: line}, value, true
				}
			}
		}
	}

	for i, f := range factories {
		consider(azFactorySource(i), f, false)
	}
	consider(azSourceCenter, center, centerHasFirst)
	return best, found
}

// ==================== 스냅샷 → 판단 ====================

// azBotPlayerView 봇이 스냅샷에서 꺼내 쓰는 좌석 정보
type azBotPlayerView struct {
	Seat  int       `json:"seat"`
	Lines []AZLine  `json:"lines"`
	Wall  [][]bool  `json:"wall"`
	Floor []AZColor `json:"floor"`
}

// azBotState 봇이 상태 스냅샷에서 꺼내 쓰는 최소 정보
type azBotState struct {
	YourSeat       int               `json:"yourSeat"`
	Phase          AZPhase           `json:"phase"`
	Round          int               `json:"round"`
	CurrentSeat    int               `json:"currentSeat"`
	Factories      [][]AZColor       `json:"factories"`
	Center         []AZColor         `json:"center"`
	CenterHasFirst bool              `json:"centerHasFirst"`
	Players        []azBotPlayerView `json:"players"`
}

// azBoardOf 스냅샷 좌석 정보를 평가용 보드로 옮긴다
func azBoardOf(v azBotPlayerView) azBotBoard {
	board := azBotBoard{Floor: len(v.Floor)}
	for i := 0; i < AZWallSize && i < len(v.Lines); i++ {
		board.Lines[i] = v.Lines[i]
	}
	for r := 0; r < AZWallSize && r < len(v.Wall); r++ {
		for c := 0; c < AZWallSize && c < len(v.Wall[r]); c++ {
			board.Wall[r][c] = v.Wall[r][c]
		}
	}
	return board
}

// azBrain 스냅샷 기반 판단 (봇 대체 좌석도 같은 두뇌를 쓴다)
type azBrain struct {
	rng *rand.Rand
	// lastKey 마지막으로 수를 둔 차례의 식별키 (중복 수주 방지)
	lastKey string
}

func newAZBrain() *azBrain {
	return &azBrain{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

// decide 공용 러너 계약 — az_game_state 에만 반응한다
func (b *azBrain) decide(msg AZMessage) *AZMessage {
	if msg.Type != AZMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[azBotState](msg.Payload)
	if !ok {
		return nil
	}
	return b.decideState(state)
}

// handled 같은 차례에 이미 뒀는지 — 처음이면 키를 기록한다
func (b *azBrain) handled(key string) bool {
	if b.lastKey == key {
		return true
	}
	b.lastKey = key
	return false
}

// think 사람처럼 잠깐 뜸을 들인다 (테스트에서는 var 를 낮춰 즉시 진행한다)
func (b *azBrain) think(base time.Duration, jitterMs int) {
	d := base
	if jitterMs > 0 {
		d += time.Duration(b.rng.Intn(jitterMs)) * time.Millisecond
	}
	if d > 0 {
		time.Sleep(d)
	}
}

func (b *azBrain) decideState(s azBotState) *AZMessage {
	me := s.YourSeat
	if me < 0 || me >= len(s.Players) {
		return nil
	}
	if s.Phase != AZPhaseDrafting || s.CurrentSeat != me {
		return nil
	}

	// 같은 차례의 스냅샷 중복 — 남은 타일 수까지 넣어 차례를 특정한다
	left := len(s.Center)
	for _, f := range s.Factories {
		left += len(f)
	}
	if b.handled(fmt.Sprintf("%d|%d|%d|%d", s.Round, s.CurrentSeat, left, len(s.Players[me].Floor))) {
		return nil
	}

	mv, ok := azBotChoose(s.Factories, s.Center, s.CenterHasFirst, azBoardOf(s.Players[me]), b.rng)
	if !ok {
		return nil
	}
	b.think(azBotTakeDelay, azBotTakeJitterMs)
	return &AZMessage{Type: AZMsgTake,
		Payload: AZTakePayload{From: mv.From, Color: mv.Color, Line: mv.Line}}
}

// ==================== 봇 소환 ====================

// spawnAZBot 대기실의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *AZHub) spawnAZBot(room *azRoom, name string) bool {
	bot := &AZClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runAZBot(bot)
	return true
}

// takeoverAZBot 유예 만료 좌석을 이어받는 연습봇 — 이름·좌석을 유지해
// 진행 중인 차례가 그대로 이어진다
func (h *AZHub) takeoverAZBot(room *azRoom, seat int, name string) *AZClient {
	bot := &AZClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	bot.GameID = room.Game.ID
	bot.Seat = seat
	h.runAZBot(bot)
	return bot
}

// runAZBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *AZHub) runAZBot(bot *AZClient) {
	brain := newAZBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m AZMessage) { h.gameMessage <- AZGameMessage{Client: bot, Message: m} },
		func(m AZMessage) bool { return m.Type == AZMsgGameOver || m.Type == AZMsgSessionExpired })
}

// azRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록용)
func azRoomHasBot(room *azRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
