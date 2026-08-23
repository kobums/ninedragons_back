package server

import (
	"fmt"
	"math/rand"
	"time"
)

// ==================== 더 크루 연습봇 ====================
//
// 스냅샷(cw_game_state)만 보고 반응한다. 봇도 사람과 같은 조건 — 자기
// yourHand 만 알고 남의 손패는 모른다. 임무(tasks)와 남의 공개 카드
// (players[].revealed)는 전원 공개라 봇도 그대로 읽는다.
//
//   - 플레이: 자기 임무 카드가 트릭에 있으면 이기려 하고(이길 수 있는 최소
//     카드), 남의 임무 카드가 있으면 일부러 지는 카드를 낸다. 임무와 무관하면
//     최약 카드. 전부 따라내기 의무 안에서 고른다.
//   - 소통: 임무(라운드)의 첫 트릭 시작 시점에 40% 확률로 자기 색 최고 카드를
//     'highest' 로 공개한다 (그 색이 한 장뿐이면 'only'). 서버 검증과 같은
//     기준으로 고르므로 거짓 선언이 나가지 않는다.
//
// 같은 대기 상태에 스냅샷이 여러 번 와도(관전 입장·접속 변화·타인 소통 등)
// 한 번만 카드를 내도록 상태 식별키로 중복을 걸러낸다.

// 봇이 "생각하는" 시간 (테스트에서 짧게 낮춘다)
var (
	cwBotCommDelay    = 500 * time.Millisecond
	cwBotCommJitterMs = 400
	cwBotPlayDelay    = 700 * time.Millisecond
	cwBotPlayJitterMs = 700
)

// cwBotCommunicateChance 임무 첫 트릭에 소통을 시도할 확률
const cwBotCommunicateChance = 0.4

// cwBotPlayerView 봇이 스냅샷에서 꺼내 쓰는 좌석 정보
type cwBotPlayerView struct {
	Seat      int `json:"seat"`
	HandCount int `json:"handCount"`
	TokenLeft int `json:"tokenLeft"`
}

// cwBotState 봇이 상태 스냅샷에서 꺼내 쓰는 최소 정보
type cwBotState struct {
	YourSeat    int               `json:"yourSeat"`
	Phase       CWPhase           `json:"phase"`
	Mission     int               `json:"mission"`
	CurrentSeat int               `json:"currentSeat"`
	LeadSuit    CWSuit            `json:"leadSuit"`
	Trick       []CWTrickCard     `json:"trick"`
	Tasks       []CWTask          `json:"tasks"`
	YourHand    []CWCard          `json:"yourHand"`
	Players     []cwBotPlayerView `json:"players"`
}

// cwBrain 스냅샷 기반 판단 (봇 대체 좌석도 같은 두뇌를 쓴다)
type cwBrain struct {
	rng *rand.Rand
	// lastKey 마지막으로 카드를 낸 대기 상태 식별키 (중복 제출 방지)
	lastKey string
	// commMission 소통을 이미 저울질한 임무 번호 (임무마다 한 번만 시도한다)
	commMission int
}

func newCWBrain() *cwBrain {
	return &cwBrain{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

// decide 공용 러너 계약 — cw_game_state 에만 반응한다
func (b *cwBrain) decide(msg CWMessage) *CWMessage {
	if msg.Type != CWMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[cwBotState](msg.Payload)
	if !ok {
		return nil
	}
	return b.decideState(state)
}

// handled 같은 대기 상태에 이미 카드를 냈는지 — 처음이면 키를 기록한다
func (b *cwBrain) handled(key string) bool {
	if b.lastKey == key {
		return true
	}
	b.lastKey = key
	return false
}

// think 사람처럼 잠깐 뜸을 들인다 (테스트에서는 var 를 낮춰 즉시 진행한다)
func (b *cwBrain) think(base time.Duration, jitterMs int) {
	d := base
	if jitterMs > 0 {
		d += time.Duration(b.rng.Intn(jitterMs)) * time.Millisecond
	}
	if d > 0 {
		time.Sleep(d)
	}
}

func (b *cwBrain) decideState(s cwBotState) *CWMessage {
	me := s.YourSeat
	if me < 0 || me >= len(s.Players) {
		return nil
	}
	if s.Phase != CWPhasePlaying {
		return nil
	}

	// 소통은 트릭 시작 시점에만 가능하다 — 임무의 첫 트릭에서 한 번 저울질한다
	if len(s.Trick) == 0 && b.commMission != s.Mission {
		b.commMission = s.Mission
		if s.Players[me].TokenLeft > 0 && b.rng.Float64() < cwBotCommunicateChance {
			if index, hint := cwBotPickCommunicate(s.YourHand); index >= 0 {
				b.think(cwBotCommDelay, cwBotCommJitterMs)
				return &CWMessage{Type: CWMsgCommunicate,
					Payload: CWCommunicatePayload{Index: index, Hint: hint}}
			}
		}
	}

	if s.CurrentSeat != me || len(s.YourHand) == 0 {
		return nil
	}
	key := fmt.Sprintf("%d|%d|%d|%d", s.Mission, len(s.YourHand), len(s.Trick), s.CurrentSeat)
	if b.handled(key) {
		return nil
	}
	index := cwBotPickPlay(s)
	if index < 0 {
		return nil
	}
	b.think(cwBotPlayDelay, cwBotPlayJitterMs)
	return &CWMessage{Type: CWMsgPlay, Payload: CWPlayPayload{Index: index}}
}

// cwBotPickCommunicate 공개할 카드와 선언을 고른다 — 색 카드가 2장 이상인
// 색의 최고 카드를 'highest' 로, 그런 색이 없으면 한 장뿐인 색을 'only' 로.
// 서버 검증과 같은 기준이라 거짓 선언이 나가지 않는다.
func cwBotPickCommunicate(hand []CWCard) (int, CWHint) {
	count := map[CWSuit]int{}
	for _, c := range hand {
		if c.Suit != CWSuitRocket {
			count[c.Suit]++
		}
	}

	bestIndex, bestRank := -1, -1
	for i, c := range hand {
		if c.Suit == CWSuitRocket || count[c.Suit] < 2 {
			continue
		}
		// 그 색의 최고 카드인지 확인
		top := true
		for _, other := range hand {
			if other.Suit == c.Suit && other.Rank > c.Rank {
				top = false
				break
			}
		}
		if top && c.Rank > bestRank {
			bestIndex, bestRank = i, c.Rank
		}
	}
	if bestIndex >= 0 {
		return bestIndex, CWHintHighest
	}

	for i, c := range hand {
		if c.Suit != CWSuitRocket && count[c.Suit] == 1 {
			return i, CWHintOnly
		}
	}
	return -1, ""
}

// cwBotPickPlay 낼 카드의 손패 인덱스 — 따라내기 의무 안에서 고른다.
// 자기 임무 카드가 트릭에 있으면 이길 수 있는 최소 카드, 남의 임무 카드가
// 있으면 확실히 지는 카드, 그 외에는 최약 카드.
func cwBotPickPlay(s cwBotState) int {
	legal := []int{}
	for i, c := range s.YourHand {
		if cwPlayable(s.YourHand, c, s.LeadSuit) {
			legal = append(legal, i)
		}
	}
	if len(legal) == 0 {
		return -1
	}

	mine, others := false, false
	for _, tc := range s.Trick {
		for _, t := range s.Tasks {
			if t.Done || t.Suit != tc.Card.Suit || t.Rank != tc.Card.Rank {
				continue
			}
			if t.Seat == s.YourSeat {
				mine = true
			} else {
				others = true
			}
		}
	}

	best := -1 // 지금 트릭을 이기고 있는 카드의 세기
	for _, tc := range s.Trick {
		if v := cwStrength(tc.Card, s.LeadSuit); v > best {
			best = v
		}
	}

	if mine {
		// 이길 수 있는 카드 중 가장 약한 것 (센 카드는 아껴 둔다)
		pick, pickStrength := -1, 0
		for _, i := range legal {
			v := cwStrength(s.YourHand[i], s.LeadSuit)
			if v > best && (pick < 0 || v < pickStrength) {
				pick, pickStrength = i, v
			}
		}
		if pick >= 0 {
			return pick
		}
	}
	if others {
		// 확실히 지는 카드 중 가장 약한 것
		pick, pickStrength := -1, 0
		for _, i := range legal {
			v := cwStrength(s.YourHand[i], s.LeadSuit)
			if v < best && (pick < 0 || v < pickStrength) {
				pick, pickStrength = i, v
			}
		}
		if pick >= 0 {
			return pick
		}
	}

	// 임무와 무관하거나 어쩔 수 없을 때 — 최약 카드
	weak, weakStrength := legal[0], cwStrength(s.YourHand[legal[0]], s.LeadSuit)
	for _, i := range legal[1:] {
		if v := cwStrength(s.YourHand[i], s.LeadSuit); v < weakStrength {
			weak, weakStrength = i, v
		}
	}
	return weak
}

// ==================== 봇 소환 ====================

// spawnCWBot 대기실의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *CWHub) spawnCWBot(room *cwRoom, name string) bool {
	bot := &CWClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runCWBot(bot)
	return true
}

// takeoverCWBot 유예 만료 좌석을 이어받는 연습봇 — 이름·좌석을 유지해
// 진행 중인 트릭이 그대로 이어진다
func (h *CWHub) takeoverCWBot(room *cwRoom, seat int, name string) *CWClient {
	bot := &CWClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	bot.GameID = room.Game.ID
	bot.Seat = seat
	h.runCWBot(bot)
	return bot
}

// runCWBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *CWHub) runCWBot(bot *CWClient) {
	brain := newCWBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m CWMessage) { h.gameMessage <- CWGameMessage{Client: bot, Message: m} },
		func(m CWMessage) bool { return m.Type == CWMsgGameOver || m.Type == CWMsgSessionExpired })
}

// cwRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록용)
func cwRoomHasBot(room *cwRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
