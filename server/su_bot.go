package server

import (
	"fmt"
	"math"
	"math/rand"
	"time"
)

// ==================== 스타트업스 연습봇 ====================
//
// 스냅샷(su_game_state)만 보고 반응한다. 봇도 사람과 같은 조건 — 자기
// yourHand 만 알고 남의 손패·덱·게임에서 제외한 3장은 모른다.
//
// 판단은 두 갈래다.
//
//	① 가져오기 (take)
//	   - 손패가 비면 규칙상 덱에서 가져와야 하므로 덱을 고른다.
//	   - 그 외에는 시장 카드마다 "안티 × 가중치 + 회사 이득"을 매기고,
//	     덱은 "덱 안티 + 미지의 카드 값 − 시장에 얹을 안티 비용 − 대주주 벽"
//	     으로 매겨 가장 높은 쪽을 고른다.
//	   - 회사 이득은 그 카드를 앞면으로 얹었을 때 내가 단독 선두가 되는지
//	     (대주주), 동수가 되는지, 아직 멀었는지로 갈린다. 총 장수(=가치)가
//	     클수록 크다 — 내가 대주주에 가까운 회사가 자연히 우선된다.
//
//	② 내려놓기 (play)
//	   - 남이 대주주인 회사 카드를 버리고, 내가 대주주인 회사 카드는 쥔다.
//
// 같은 대기 상태에 스냅샷이 여러 번 와도(관전 입장·접속 변화 등) 한 번만
// 행동하도록 상태 식별키로 중복을 걸러낸다.

// 봇이 "생각하는" 시간 (테스트에서 짧게 낮춘다)
var (
	suBotDelay    = 700 * time.Millisecond
	suBotJitterMs = 700
)

// 봇 가치 함수 계수 (밸런스 조정 손잡이 — 봇 품질 측정 테스트가 이 값을 읽는다)
var (
	// suBotAnteWeight 카드 위에 쌓인 안티 1원의 가치
	suBotAnteWeight = 1.0
	// suBotDeckValue 덱에서 뽑는 미지의 카드 한 장의 기본 가치
	suBotDeckValue = 3.5
	// suBotAnteCost 덱을 고를 때 시장 카드 1장에 얹어야 하는 안티의 부담
	suBotAnteCost = 0.7
	// suBotBlockPenalty 내가 대주주인 회사 1종당 덱 벽(안티 낭비) 부담
	suBotBlockPenalty = 0.5
	// suBotLeadWeight 그 카드를 얹으면 단독 선두(대주주)가 될 때의 가중치
	suBotLeadWeight = 1.0
	// suBotTieWeight 그 카드를 얹으면 선두와 동수가 될 때의 가중치
	suBotTieWeight = 0.7
	// suBotChaseWeight 아직 선두에 멀 때의 가중치 (거리로 나눈다)
	suBotChaseWeight = 0.35
	// suBotKeepWeight 내가 대주주인 회사 카드를 쥐려는 정도 (버리기 감점)
	suBotKeepWeight = 1.2
	// suBotDumpWeight 남이 대주주인 회사 카드를 버리려는 정도
	suBotDumpWeight = 1.0
	// suBotEvenWeight 대주주가 없는(동수) 회사 카드의 중립 점수
	suBotEvenWeight = 0.3
	// suBotFreeTakeBonus 손패가 비어 카드를 내주지 않고 시장에서 얻기만 할 때의 가산점
	suBotFreeTakeBonus = 2.0
)

// suBotPlayerView 봇이 스냅샷에서 꺼내 쓰는 좌석 정보
type suBotPlayerView struct {
	Seat      int               `json:"seat"`
	Money     int               `json:"money"`
	HandCount int               `json:"handCount"`
	FaceUp    map[SUCompany]int `json:"faceUp"`
}

// suBotState 봇이 상태 스냅샷에서 꺼내 쓰는 최소 정보
type suBotState struct {
	YourSeat    int               `json:"yourSeat"`
	Phase       SUPhase           `json:"phase"`
	CurrentSeat int               `json:"currentSeat"`
	DeckLeft    int               `json:"deckLeft"`
	DeckAnte    int               `json:"deckAnte"`
	Market      []SUMarketCard    `json:"market"`
	Companies   []SUCompanyInfo   `json:"companies"`
	YourHand    []SUCompany       `json:"yourHand"`
	Players     []suBotPlayerView `json:"players"`
}

// faceUpOf 좌석의 그 회사 앞면 보유 수
func (s suBotState) faceUpOf(seat int, c SUCompany) int {
	for _, p := range s.Players {
		if p.Seat == seat {
			return p.FaceUp[c]
		}
	}
	return 0
}

// bestOther 나를 뺀 좌석 중 그 회사 앞면 보유 최대치
func (s suBotState) bestOther(c SUCompany) int {
	best := 0
	for _, p := range s.Players {
		if p.Seat == s.YourSeat {
			continue
		}
		if p.FaceUp[c] > best {
			best = p.FaceUp[c]
		}
	}
	return best
}

// myMoney 내 돈
func (s suBotState) myMoney() int {
	for _, p := range s.Players {
		if p.Seat == s.YourSeat {
			return p.Money
		}
	}
	return 0
}

// suBrain 스냅샷 기반 판단 (봇 대체 좌석도 같은 두뇌를 쓴다)
type suBrain struct {
	rng *rand.Rand
	// lastKey 마지막으로 행동한 대기 상태 식별키 (중복 행동 방지)
	lastKey string
}

func newSUBrain() *suBrain {
	return &suBrain{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

// decide 공용 러너 계약 — su_game_state 에만 반응한다
func (b *suBrain) decide(msg SUMessage) *SUMessage {
	if msg.Type != SUMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[suBotState](msg.Payload)
	if !ok {
		return nil
	}
	return b.decideState(state)
}

// think 사람처럼 잠깐 뜸을 들인다 (테스트에서는 var 를 낮춰 즉시 진행한다)
func (b *suBrain) think() {
	d := suBotDelay
	if suBotJitterMs > 0 {
		d += time.Duration(b.rng.Intn(suBotJitterMs)) * time.Millisecond
	}
	if d > 0 {
		time.Sleep(d)
	}
}

// stateKey 같은 대기 상태를 식별하는 키 — 판이 조금이라도 바뀌면 달라진다
func (b *suBrain) stateKey(s suBotState) string {
	faceUp := 0
	for _, p := range s.Players {
		for _, def := range suCompanyDefs {
			faceUp += p.FaceUp[def.ID] * (def.Size + p.Seat)
		}
	}
	return fmt.Sprintf("%s|%d|%d|%d|%d|%d|%d|%d",
		s.Phase, s.CurrentSeat, s.DeckLeft, s.DeckAnte, len(s.Market),
		len(s.YourHand), s.myMoney(), faceUp)
}

func (b *suBrain) decideState(s suBotState) *SUMessage {
	me := s.YourSeat
	if me < 0 || me >= len(s.Players) || s.CurrentSeat != me {
		return nil
	}
	if s.Phase != SUPhaseTake && s.Phase != SUPhasePlay {
		return nil
	}
	key := b.stateKey(s)
	if b.lastKey == key {
		return nil
	}
	b.lastKey = key

	if s.Phase == SUPhaseTake {
		b.think()
		return &SUMessage{Type: SUMsgTake, Payload: SUTakePayload{From: b.pickTake(s)}}
	}
	if len(s.YourHand) == 0 {
		return nil
	}
	b.think()
	return &SUMessage{Type: SUMsgPlay, Payload: SUPlayPayload{Index: b.pickPlay(s)}}
}

// companyGain 그 회사 카드 한 장을 내 앞에 앞면으로 얹었을 때의 이득.
// 총 장수(=가치)가 클수록, 내가 선두에 가까울수록 크다.
func (b *suBrain) companyGain(s suBotState, c SUCompany) float64 {
	size := float64(suSize(c))
	mine := s.faceUpOf(s.YourSeat, c)
	other := s.bestOther(c)
	after := mine + 1
	switch {
	case after > other:
		return size * suBotLeadWeight
	case after == other:
		return size * suBotTieWeight
	default:
		return size * suBotChaseWeight / float64(other-after+1)
	}
}

// pickTake ① 어디서 카드를 가져올지 — "deck" 또는 "market:N"
func (b *suBrain) pickTake(s suBotState) string {
	// 손패가 비면 시장에서 가져와도 낼 카드가 없어 ②를 건너뛴다 —
	// 카드를 내주지 않고 얻기만 하므로 시장이 그만큼 더 달다
	freeTake := 0.0
	if len(s.YourHand) == 0 {
		freeTake = suBotFreeTakeBonus
	}

	bestIdx, bestScore := -1, math.Inf(-1)
	for i, card := range s.Market {
		score := float64(card.Ante)*suBotAnteWeight + b.companyGain(s, card.Company) + freeTake
		score += b.rng.Float64() * 0.01
		if score > bestScore {
			bestScore, bestIdx = score, i
		}
	}

	deckScore := math.Inf(-1)
	if s.DeckLeft > 0 {
		blocked := 0
		for _, ci := range s.Companies {
			if ci.MajoritySeat == s.YourSeat {
				blocked++
			}
		}
		deckScore = float64(s.DeckAnte)*suBotAnteWeight + suBotDeckValue -
			float64(len(s.Market))*suBotAnteCost -
			float64(blocked)*suBotBlockPenalty
		// 안티를 낼 돈이 없으면 덱은 무리다 (대주주 벽에 막힐 수 있다)
		if blocked > 0 && s.myMoney() <= 0 {
			deckScore = math.Inf(-1)
		}
	}

	if bestIdx < 0 {
		return SUTakeDeck
	}
	if deckScore > bestScore {
		return SUTakeDeck
	}
	return fmt.Sprintf("%s%d", SUTakeMarketPrefix, bestIdx)
}

// pickPlay ② 손패에서 시장에 낼 카드 — 남이 대주주인 회사를 버리고,
// 내가 대주주인 회사는 쥔다
func (b *suBrain) pickPlay(s suBotState) int {
	best, bestScore := 0, math.Inf(-1)
	for i, c := range s.YourHand {
		size := float64(suSize(c))
		mine := s.faceUpOf(s.YourSeat, c)
		other := s.bestOther(c)

		var score float64
		switch {
		case mine > other:
			score = -size * suBotKeepWeight
		case other > mine:
			score = size * suBotDumpWeight
		default:
			score = size * suBotEvenWeight
		}
		score += b.rng.Float64() * 0.01
		if score > bestScore {
			bestScore, best = score, i
		}
	}
	return best
}

// ==================== 봇 소환 ====================

// spawnSUBot 대기실의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *SUHub) spawnSUBot(room *suRoom, name string) bool {
	bot := &SUClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runSUBot(bot)
	return true
}

// takeoverSUBot 유예 만료 좌석을 이어받는 연습봇 — 이름·좌석을 유지해
// 진행 중인 차례가 그대로 이어진다
func (h *SUHub) takeoverSUBot(room *suRoom, seat int, name string) *SUClient {
	bot := &SUClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	bot.GameID = room.Game.ID
	bot.Seat = seat
	h.runSUBot(bot)
	return bot
}

// runSUBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *SUHub) runSUBot(bot *SUClient) {
	brain := newSUBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m SUMessage) { h.gameMessage <- SUGameMessage{Client: bot, Message: m} },
		func(m SUMessage) bool { return m.Type == SUMsgGameOver || m.Type == SUMsgSessionExpired })
}

// suRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록용)
func suRoomHasBot(room *suRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
