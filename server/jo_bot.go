package server

import (
	"fmt"
	"math/rand"
	"time"
)

// ==================== 저스트 원 연습봇 ====================
//
// 스냅샷(jo_game_state)만 보고 반응한다.
//
//   - 단서: 제시어와 '같은 카테고리의 다른 단어'를 고른다 (스파이폴 9카테고리
//     자산을 그대로 쓰므로 그럴듯한 연상 단서가 된다). 같은 카테고리를 못
//     찾으면 전체 풀에서 무작위. 제시어와 소거 관계인 후보는 미리 걸러진다.
//   - 출제자: 40% 확률로 정답, 아니면 같은 카테고리의 무작위 단어를 찍는다.
//   - 인정: 오답이 제시어와 같은 카테고리면 30% 확률로 인정한다.
//
// ⚠ 출제자 봇의 정답률은 '사람 실력을 흉내 내는 시뮬레이션'이다. 사람 출제자는
// 제시어를 모르지만 봇은 알아야 40% 를 만들 수 있으므로, 허브가 봇 좌석의
// 스냅샷에만 제시어를 실어 준다(jo_hub.go buildJOStateFor 의 forBot). 사람
// 좌석·관전자 스냅샷에는 어떤 경로로도 제시어가 실리지 않는다.
//
// 같은 대기 상태에 스냅샷이 여러 번 와도(관전 입장·접속 변화·타인 제출 등)
// 라운드·단계마다 한 번만 행동하도록 상태 식별키로 중복을 걸러낸다.

// 봇이 "생각하는" 시간 (테스트에서 짧게 낮춘다)
var (
	joBotClueDelay      = 900 * time.Millisecond
	joBotClueJitterMs   = 900
	joBotGuessDelay     = 1200 * time.Millisecond
	joBotGuessJitterMs  = 900
	joBotAcceptDelay    = 600 * time.Millisecond
	joBotAcceptJitterMs = 600
)

const (
	// joBotCorrectChance 출제자 봇이 정답을 맞히는 확률 (사람 실력 시뮬레이션)
	joBotCorrectChance = 0.4
	// joBotAcceptChance 같은 카테고리 오답을 인정해 주는 확률
	joBotAcceptChance = 0.3
)

// joBotPlayerView 봇이 스냅샷에서 꺼내 쓰는 좌석 정보
type joBotPlayerView struct {
	Seat      int  `json:"seat"`
	Submitted bool `json:"submitted"`
	IsGuesser bool `json:"isGuesser"`
}

// joBotState 봇이 상태 스냅샷에서 꺼내 쓰는 최소 정보.
// Word 는 단서 제공자(와 시뮬레이션용 봇 출제자)에게만 실린다 — 키가 없으면 "".
type joBotState struct {
	YourSeat    int               `json:"yourSeat"`
	Phase       JOPhase           `json:"phase"`
	Round       int               `json:"round"`
	GuesserSeat int               `json:"guesserSeat"`
	Word        string            `json:"word"`
	Guess       string            `json:"guess"`
	Players     []joBotPlayerView `json:"players"`
}

// joBrain 스냅샷 기반 판단 (봇 대체 좌석도 같은 두뇌를 쓴다)
type joBrain struct {
	rng *rand.Rand
	// lastKey 마지막으로 행동한 대기 상태 식별키 (중복 행동 방지)
	lastKey string
}

func newJOBrain() *joBrain {
	return &joBrain{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

// decide 공용 러너 계약 — jo_game_state 에만 반응한다
func (b *joBrain) decide(msg JOMessage) *JOMessage {
	if msg.Type != JOMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[joBotState](msg.Payload)
	if !ok {
		return nil
	}
	return b.decideState(state)
}

// handled 같은 대기 상태에 이미 행동했는지 — 처음이면 키를 기록한다
func (b *joBrain) handled(key string) bool {
	if b.lastKey == key {
		return true
	}
	b.lastKey = key
	return false
}

// think 사람처럼 잠깐 뜸을 들인다 (테스트에서는 var 를 낮춰 즉시 진행한다)
func (b *joBrain) think(base time.Duration, jitterMs int) {
	d := base
	if jitterMs > 0 {
		d += time.Duration(b.rng.Intn(jitterMs)) * time.Millisecond
	}
	if d > 0 {
		time.Sleep(d)
	}
}

func (b *joBrain) decideState(s joBotState) *JOMessage {
	me := s.YourSeat
	if me < 0 || me >= len(s.Players) {
		return nil
	}

	switch s.Phase {
	case JOPhaseClue:
		if me == s.GuesserSeat || s.Players[me].Submitted || s.Word == "" {
			return nil
		}
		if b.handled(fmt.Sprintf("clue|%d", s.Round)) {
			return nil
		}
		clue := joRelatedWord(b.rng, s.Word)
		if clue == "" {
			return nil
		}
		b.think(joBotClueDelay, joBotClueJitterMs)
		return &JOMessage{Type: JOMsgClue, Payload: JOCluePayload{Text: clue}}

	case JOPhaseGuess:
		if me != s.GuesserSeat {
			return nil
		}
		if b.handled(fmt.Sprintf("guess|%d", s.Round)) {
			return nil
		}
		b.think(joBotGuessDelay, joBotGuessJitterMs)
		// 제시어가 실리지 않은 스냅샷(사람 좌석을 흉내 내는 드라이버)이면
		// 찍을 근거가 없으니 넘긴다
		if s.Word == "" {
			return &JOMessage{Type: JOMsgPass}
		}
		return &JOMessage{Type: JOMsgGuess, Payload: JOGuessPayload{Text: joBotGuess(b.rng, s.Word)}}

	case JOPhaseJudging:
		if me == s.GuesserSeat || s.Word == "" || s.Guess == "" {
			return nil
		}
		if b.handled(fmt.Sprintf("accept|%d", s.Round)) {
			return nil
		}
		if !joBotWillAccept(b.rng, s.Word, s.Guess) {
			return nil
		}
		b.think(joBotAcceptDelay, joBotAcceptJitterMs)
		return &JOMessage{Type: JOMsgAccept}
	}
	return nil
}

// joBotGuess 출제자 봇의 답 — 40% 는 정답, 나머지는 같은 카테고리의 다른 단어.
// 사람 출제자의 적중률을 흉내 내는 시뮬레이션이다 (파일 머리말 참고).
func joBotGuess(rng *rand.Rand, word string) string {
	if rng.Float64() < joBotCorrectChance {
		return word
	}
	if other := joRelatedWord(rng, word); other != "" {
		return other
	}
	return word
}

// joBotWillAccept 오답을 인정할지 — 답이 제시어와 같은 카테고리일 때만
// 30% 확률로 인정한다 (엉뚱한 답은 인정하지 않는다)
func joBotWillAccept(rng *rand.Rand, word, guess string) bool {
	category := joCategoryOf(word)
	if category == "" || joCategoryOf(guess) != category {
		return false
	}
	return rng.Float64() < joBotAcceptChance
}

// ==================== 봇 소환 ====================

// spawnJOBot 대기실의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *JOHub) spawnJOBot(room *joRoom, name string) bool {
	bot := &JOClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runJOBot(bot)
	return true
}

// takeoverJOBot 유예 만료 좌석을 이어받는 연습봇 — 이름·좌석을 유지해
// 진행 중인 라운드가 그대로 이어진다
func (h *JOHub) takeoverJOBot(room *joRoom, seat int, name string) *JOClient {
	bot := &JOClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	bot.GameID = room.Game.ID
	bot.Seat = seat
	h.runJOBot(bot)
	return bot
}

// runJOBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *JOHub) runJOBot(bot *JOClient) {
	brain := newJOBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m JOMessage) { h.gameMessage <- JOGameMessage{Client: bot, Message: m} },
		func(m JOMessage) bool { return m.Type == JOMsgGameOver || m.Type == JOMsgSessionExpired })
}

// joRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록용)
func joRoomHasBot(room *joRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
