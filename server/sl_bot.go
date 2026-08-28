package server

import (
	"fmt"
	"math/rand"
	"sort"
	"time"
)

// ==================== 스플렌더 연습봇 ====================
//
// 스냅샷(sl_game_state)만 보고 반응한다. 봇도 사람과 같은 조건 — 자기
// yourReserved 만 알고 남이 덱에서 비공개로 예약한 카드는 모른다.
//
// 가치 함수는 두 축이다.
//
//	① 명성 점수 — 카드가 주는 점수를 그대로 센다 (slBotPointWeight).
//	② 보너스 효율 — 그 색이 앞으로 얼마나 아쉬운가를 "수요"로 잰다.
//	   수요 = 남은 귀족 타일이 아직 요구하는 그 색의 수 (크게)
//	        + 진열의 2·3단계 카드가 요구하는 그 색의 수 (작게)
//	   3단계와 귀족 요구 색이 자연히 가중된다.
//
// 한 차례의 결정은 다음 순서다.
//
//	1. 지금 살 수 있는 카드 중 가치가 가장 높은 것을 산다 (문턱값 이상일 때).
//	2. 못 사면 목표 카드를 하나 정하고(가치 ÷ 남은 거리) 그 카드에 필요한
//	   색 토큰을 모은다. 같은 색이 2개 이상 모자라고 공동 창고에 4개 이상
//	   있으면 같은 색 2개를 가져온다.
//	3. 큰 카드가 황금 하나 차이로 손에 안 잡히면 예약해서 황금을 챙긴다.
//	4. 토큰도 못 가져오면 예약으로 차례를 쓴다.
//
// 같은 차례에 스냅샷이 여러 번 와도(관전 입장·접속 변화 등) 한 번만
// 행동하도록 상태 식별키로 중복을 걸러낸다.

// 봇이 "생각하는" 시간 (테스트에서 짧게 낮춘다)
var (
	slBotDelay    = 700 * time.Millisecond
	slBotJitterMs = 700
)

// 봇 가치 함수 계수 (밸런스 조정 손잡이 — 봇 품질 측정 테스트가 이 값을 읽는다)
var (
	// slBotPointWeight 명성 점수 1점의 가치
	slBotPointWeight = 3.0
	// slBotNobleDemand 귀족 타일이 아직 요구하는 색 1개의 가치
	slBotNobleDemand = 1.0
	// slBotCardDemand 진열의 2·3단계 카드가 요구하는 색 1개의 가치
	slBotCardDemand = 0.18
	// slBotDemandWeight 보너스 색 수요를 카드 가치에 반영하는 비율
	slBotDemandWeight = 0.8
	// slBotNobleBonus 이 카드를 사면 귀족 타일이 곧장 찾아올 때의 가산점
	slBotNobleBonus = 6.0
	// slBotBuyThreshold 이 값에 못 미치는 카드는 사지 않고 토큰을 모은다
	slBotBuyThreshold = 1.0
	// slBotReachPenalty 목표 카드를 고를 때 "아직 모자란 토큰 1개"의 감점 비율
	slBotReachPenalty = 0.9
	// slBotReservePoints 황금 한 개 차이로 놓치는 큰 카드의 최소 명성 점수
	slBotReservePoints = 3
)

// slBotPlayerView 봇이 스냅샷에서 꺼내 쓰는 좌석 정보
type slBotPlayerView struct {
	Seat          int        `json:"seat"`
	Points        int        `json:"points"`
	Cards         SLGemSet   `json:"cards"`
	Tokens        SLTokenSet `json:"tokens"`
	ReservedCount int        `json:"reservedCount"`
}

// slBotState 봇이 상태 스냅샷에서 꺼내 쓰는 최소 정보
type slBotState struct {
	YourSeat     int               `json:"yourSeat"`
	Phase        SLPhase           `json:"phase"`
	CurrentSeat  int               `json:"currentSeat"`
	Turns        int               `json:"turns"`
	LastRound    bool              `json:"lastRound"`
	Bank         SLTokenSet        `json:"bank"`
	Board        SLBoardView       `json:"board"`
	DeckLeft     SLDeckLeft        `json:"deckLeft"`
	Nobles       []SLNoble         `json:"nobles"`
	YourReserved []SLCard          `json:"yourReserved"`
	Players      []slBotPlayerView `json:"players"`
}

// me 내 좌석 정보 (없으면 zero)
func (s slBotState) me() slBotPlayerView {
	for _, p := range s.Players {
		if p.Seat == s.YourSeat {
			return p
		}
	}
	return slBotPlayerView{Seat: -1}
}

// visible 지금 살 수 있을 법한 카드 전부 — 진열 공개 카드 + 내 예약 카드
func (s slBotState) visible() []SLCard {
	cards := []SLCard{}
	cards = append(cards, s.Board.Tier1...)
	cards = append(cards, s.Board.Tier2...)
	cards = append(cards, s.Board.Tier3...)
	cards = append(cards, s.YourReserved...)
	return cards
}

// stateKey 같은 대기 상태에서 두 번 행동하지 않기 위한 식별키.
//
// sl_game_state 에는 차례 번호가 없으므로(스펙 고정) 판이 실제로 달라졌는지를
// 나타내는 값들을 지문처럼 엮는다 — 공동 창고·내 토큰·내 보너스·예약 장수·
// 덱 잔량·진열 카드 id. 어느 하나라도 바뀌면 새 상태다.
func (s slBotState) stateKey() string {
	me := s.me()
	key := fmt.Sprintf("%s|%d|%+v|%+v|%+v|%d|%+v",
		s.Phase, s.CurrentSeat, s.Bank, me.Tokens, me.Cards, me.ReservedCount, s.DeckLeft)
	for _, card := range s.visible() {
		key += fmt.Sprintf(",%d", card.ID)
	}
	return key
}

// slBrain 스냅샷 기반 판단 (봇 대체 좌석도 같은 두뇌를 쓴다)
type slBrain struct {
	rng *rand.Rand
	// lastKey 마지막으로 행동한 대기 상태의 식별키 (중복 행동 방지)
	lastKey string
}

func newSLBrain() *slBrain {
	return &slBrain{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

// decide 공용 러너 계약 — sl_game_state 에만 반응한다
func (b *slBrain) decide(msg SLMessage) *SLMessage {
	if msg.Type != SLMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[slBotState](msg.Payload)
	if !ok {
		return nil
	}
	return b.decideState(state)
}

// think 사람처럼 잠깐 뜸을 들인다 (테스트에서는 var 를 낮춰 즉시 진행한다)
func (b *slBrain) think() {
	d := slBotDelay
	if slBotJitterMs > 0 {
		d += time.Duration(b.rng.Intn(slBotJitterMs)) * time.Millisecond
	}
	if d > 0 {
		time.Sleep(d)
	}
}

// decideState 자기 차례면 정확히 한 수를 결정한다
func (b *slBrain) decideState(s slBotState) *SLMessage {
	me := s.me()
	if me.Seat < 0 || s.CurrentSeat != me.Seat {
		return nil
	}
	if s.Phase != SLPhaseTurn && s.Phase != SLPhaseDiscard {
		return nil
	}
	key := s.stateKey()
	if b.lastKey == key {
		return nil
	}
	b.lastKey = key

	var move *SLMessage
	if s.Phase == SLPhaseDiscard {
		move = b.discardMove(s, me)
	} else {
		move = b.turnMove(s, me)
	}
	if move == nil { // 방어선 — 어떤 상황에서도 한 수는 낸다
		move = b.anyMove(s, me)
	}
	b.think()
	return move
}

// ==================== 가치 함수 ====================

// slShortfall 카드 하나를 사기까지 아직 모자란 것.
//
//	short  — 색별로 더 모아야 하는 보석 토큰 수 (보너스·보유 토큰을 뺀 값)
//	total  — 모자란 토큰의 합
//	gold   — 황금으로 메워야 하는 수 (= total 중 지금 황금으로 감당되는 몫)
//	ok     — 지금 당장 살 수 있는가
func slShortfall(card SLCard, me slBotPlayerView) (short SLGemSet, total int, ok bool) {
	for _, gem := range slGems {
		need := card.Cost.get(gem) - me.Cards.get(gem)
		if need <= 0 {
			continue
		}
		lack := need - me.Tokens.get(gem)
		if lack > 0 {
			short.add(gem, lack)
			total += lack
		}
	}
	return short, total, total <= me.Tokens.Gold
}

// demand 그 색 보너스가 앞으로 얼마나 아쉬운가.
// 귀족 타일이 아직 요구하는 몫을 크게, 진열 2·3단계 카드의 요구를 작게 센다.
func (s slBotState) demand(me slBotPlayerView, gem SLGem) float64 {
	score := 0.0
	for _, noble := range s.Nobles {
		if lack := noble.Cost.get(gem) - me.Cards.get(gem); lack > 0 {
			score += float64(lack) * slBotNobleDemand
		}
	}
	for _, card := range append(append([]SLCard{}, s.Board.Tier2...), s.Board.Tier3...) {
		if lack := card.Cost.get(gem) - me.Cards.get(gem); lack > 0 {
			score += float64(lack) * slBotCardDemand
		}
	}
	return score
}

// completesNoble 이 카드를 사면 귀족 타일이 곧장 찾아오는가
func (s slBotState) completesNoble(me slBotPlayerView, card SLCard) bool {
	after := me.Cards
	after.add(card.Gem, 1)
	for _, noble := range s.Nobles {
		if slNobleMet(noble, after) {
			return true
		}
	}
	return false
}

// cardValue 카드 한 장의 가치 — 명성 점수 + 보너스 효율 + 귀족 가산점
func (s slBotState) cardValue(me slBotPlayerView, card SLCard) float64 {
	value := float64(card.Points) * slBotPointWeight
	value += s.demand(me, card.Gem) * slBotDemandWeight
	if s.completesNoble(me, card) {
		value += slBotNobleBonus
	}
	return value
}

// ==================== 차례 ====================

func (b *slBrain) turnMove(s slBotState, me slBotPlayerView) *SLMessage {
	// ① 지금 살 수 있는 카드 중 가장 값진 것
	bestBuy, bestBuyValue := -1, 0.0
	for _, card := range s.visible() {
		if _, _, ok := slShortfall(card, me); !ok {
			continue
		}
		if v := s.cardValue(me, card); bestBuy < 0 || v > bestBuyValue {
			bestBuy, bestBuyValue = card.ID, v
		}
	}
	// 마지막 라운드에는 명성 점수가 붙는 카드라면 문턱 없이 산다
	threshold := slBotBuyThreshold
	if s.LastRound {
		threshold = 0
	}
	if bestBuy > 0 && bestBuyValue >= threshold {
		return &SLMessage{Type: SLMsgBuy, Payload: SLBuyPayload{CardID: bestBuy}}
	}

	// ② 목표 카드 — 가치가 높고 남은 거리가 짧은 것
	goal, hasGoal := s.pickGoal(me)

	// ③ 큰 카드가 황금 하나 차이면 예약해서 황금을 챙긴다 (남에게도 못 넘어간다)
	if me.ReservedCount < SLMaxReserved && s.Bank.Gold > 0 {
		for _, card := range append(append([]SLCard{}, s.Board.Tier2...), s.Board.Tier3...) {
			_, total, _ := slShortfall(card, me)
			if card.Points >= slBotReservePoints && total-me.Tokens.Gold == 1 {
				return &SLMessage{Type: SLMsgReserve, Payload: SLReservePayload{CardID: card.ID}}
			}
		}
	}

	// ④ 목표 카드에 필요한 색 토큰을 모은다
	if hasGoal {
		if msg := b.takeToward(s, me, goal); msg != nil {
			return msg
		}
	}
	// ⑤ 목표가 없거나 필요한 색이 동나면 수요가 큰 색으로 아무거나 모은다
	if msg := b.takeAny(s, me); msg != nil {
		return msg
	}

	// ⑥ 토큰도 못 가져온다 — 예약으로 차례를 쓴다
	if msg := b.reserveMove(s, me); msg != nil {
		return msg
	}
	// ⑦ 문턱을 못 넘은 카드라도 살 수 있으면 산다
	if bestBuy > 0 {
		return &SLMessage{Type: SLMsgBuy, Payload: SLBuyPayload{CardID: bestBuy}}
	}
	return nil
}

// slNetCost 보너스를 뺀 뒤 실제로 쌓아야 하는 토큰 총수. 이 값이 10을 넘으면
// 보유 상한(10개) 때문에 토큰만 모아서는 영원히 살 수 없다 — 보너스를 더
// 쌓기 전에는 목표로 삼으면 안 되는 카드다.
func slNetCost(card SLCard, bonus SLGemSet) int {
	total := 0
	for _, gem := range slGems {
		if need := card.Cost.get(gem) - bonus.get(gem); need > 0 {
			total += need
		}
	}
	return total
}

// pickGoal 목표 카드 하나 — 가치 ÷ (1 + 남은 거리).
// 지금 보너스로는 토큰 상한 안에 값을 못 치르는 카드는 후보에서 뺀다
// (그런 카드를 쫓으면 모았다 버렸다를 반복하며 판이 늘어진다).
func (s slBotState) pickGoal(me slBotPlayerView) (SLCard, bool) {
	best, bestScore, found := SLCard{}, 0.0, false
	fallback, fallbackScore, hasFallback := SLCard{}, 0.0, false
	for _, card := range s.visible() {
		_, total, _ := slShortfall(card, me)
		score := s.cardValue(me, card) / (1 + float64(total)*slBotReachPenalty)
		if slNetCost(card, me.Cards) > SLTokenLimit {
			if !hasFallback || score > fallbackScore {
				fallback, fallbackScore, hasFallback = card, score, true
			}
			continue
		}
		if !found || score > bestScore {
			best, bestScore, found = card, score, true
		}
	}
	if found {
		return best, true
	}
	return fallback, hasFallback
}

// takeToward 목표 카드에 모자란 색을 가져온다.
// 같은 색이 2개 이상 모자라고 공동 창고에 4개 이상 있으면 같은 색 2개를 쥔다.
func (b *slBrain) takeToward(s slBotState, me slBotPlayerView, goal SLCard) *SLMessage {
	short, _, _ := slShortfall(goal, me)

	// 같은 색 2개 — 한 색이 크게 모자랄 때 가장 효율이 좋다
	for _, gem := range slGems {
		if short.get(gem) >= SLTakeSame && s.Bank.get(gem) >= SLTakeSameMin {
			return &SLMessage{Type: SLMsgTake,
				Payload: SLTakePayload{Colors: []SLGem{gem, gem}}}
		}
	}

	// 서로 다른 색 — 모자란 색을 많이 모자란 순으로
	wanted := []SLGem{}
	for _, gem := range slGems {
		if short.get(gem) > 0 && s.Bank.get(gem) > 0 {
			wanted = append(wanted, gem)
		}
	}
	sort.SliceStable(wanted, func(i, j int) bool {
		return short.get(wanted[i]) > short.get(wanted[j])
	})
	if len(wanted) == 0 {
		return nil
	}
	return b.takeDistinct(s, wanted)
}

// takeAny 목표와 무관하게 수요가 큰 색부터 가져온다
func (b *slBrain) takeAny(s slBotState, me slBotPlayerView) *SLMessage {
	wanted := []SLGem{}
	for _, gem := range slGems {
		if s.Bank.get(gem) > 0 {
			wanted = append(wanted, gem)
		}
	}
	if len(wanted) == 0 {
		return nil
	}
	sort.SliceStable(wanted, func(i, j int) bool {
		return s.demand(me, wanted[i]) > s.demand(me, wanted[j])
	})
	return b.takeDistinct(s, wanted)
}

// takeDistinct 규칙에 맞게 서로 다른 색을 최대 3개까지 고른다.
// 공동 창고에 남은 색이 3가지 미만이면 남은 색을 전부 골라야 통과한다.
func (b *slBrain) takeDistinct(s slBotState, prefer []SLGem) *SLMessage {
	avail := []SLGem{}
	for _, gem := range slGems {
		if s.Bank.get(gem) > 0 {
			avail = append(avail, gem)
		}
	}
	if len(avail) == 0 {
		return nil
	}
	want := SLTakeDistinct
	if len(avail) < want {
		want = len(avail)
	}

	picked := []SLGem{}
	seen := map[SLGem]bool{}
	for _, gem := range prefer {
		if len(picked) == want {
			break
		}
		if seen[gem] || s.Bank.get(gem) <= 0 {
			continue
		}
		seen[gem] = true
		picked = append(picked, gem)
	}
	for _, gem := range avail { // 모자라면 남은 색으로 채운다
		if len(picked) == want {
			break
		}
		if seen[gem] {
			continue
		}
		seen[gem] = true
		picked = append(picked, gem)
	}
	if len(picked) == 0 {
		return nil
	}
	return &SLMessage{Type: SLMsgTake, Payload: SLTakePayload{Colors: picked}}
}

// reserveMove 예약 — 3단계부터 가치가 높은 카드를 쥔다. 진열이 비면 덱 맨 위.
func (b *slBrain) reserveMove(s slBotState, me slBotPlayerView) *SLMessage {
	if me.ReservedCount >= SLMaxReserved {
		return nil
	}
	best, bestValue := -1, 0.0
	for _, card := range append(append([]SLCard{}, s.Board.Tier3...), s.Board.Tier2...) {
		if v := s.cardValue(me, card); best < 0 || v > bestValue {
			best, bestValue = card.ID, v
		}
	}
	if best < 0 && len(s.Board.Tier1) > 0 {
		best = s.Board.Tier1[0].ID
	}
	if best > 0 {
		return &SLMessage{Type: SLMsgReserve, Payload: SLReservePayload{CardID: best}}
	}
	for _, tier := range []int{3, 2, 1} {
		if s.DeckLeft.tier(tier) > 0 {
			return &SLMessage{Type: SLMsgReserve, Payload: SLReservePayload{Tier: tier}}
		}
	}
	return nil
}

// tier 단계별 덱 잔량
func (d SLDeckLeft) tier(t int) int {
	switch t {
	case 1:
		return d.Tier1
	case 2:
		return d.Tier2
	case 3:
		return d.Tier3
	}
	return 0
}

// anyMove 마지막 방어선 — 무엇이든 규칙에 맞는 한 수를 만든다
func (b *slBrain) anyMove(s slBotState, me slBotPlayerView) *SLMessage {
	if msg := b.takeAny(s, me); msg != nil {
		return msg
	}
	for _, card := range s.visible() {
		if _, _, ok := slShortfall(card, me); ok {
			return &SLMessage{Type: SLMsgBuy, Payload: SLBuyPayload{CardID: card.ID}}
		}
	}
	return b.reserveMove(s, me)
}

// ==================== 10개 초과 버리기 ====================

// discardMove 목표 카드에 쓸모가 적은 토큰부터 버린다 (황금은 마지막까지 쥔다)
func (b *slBrain) discardMove(s slBotState, me slBotPlayerView) *SLMessage {
	over := me.Tokens.total() - SLTokenLimit
	if over <= 0 {
		return nil
	}
	short := SLGemSet{}
	if goal, ok := s.pickGoal(me); ok {
		short, _, _ = slShortfall(goal, me)
	}

	// 색별 쓸모: 목표에 모자란 몫 + 전체 수요. 낮은 것부터 버린다.
	type slot struct {
		gem   SLGem
		worth float64
	}
	pool := []slot{}
	for _, gem := range slGems {
		worth := float64(short.get(gem))*2 + s.demand(me, gem)*0.2
		for i := 0; i < me.Tokens.get(gem); i++ {
			pool = append(pool, slot{gem: gem, worth: worth})
		}
	}
	for i := 0; i < me.Tokens.Gold; i++ {
		pool = append(pool, slot{gem: SLGold, worth: 99}) // 황금은 만능이라 가장 아깝다
	}
	sort.SliceStable(pool, func(i, j int) bool { return pool[i].worth < pool[j].worth })
	if len(pool) < over {
		return nil
	}

	colors := []SLGem{}
	for _, sl := range pool[:over] {
		colors = append(colors, sl.gem)
	}
	return &SLMessage{Type: SLMsgDiscard, Payload: SLDiscardPayload{Colors: colors}}
}

// ==================== 봇 소환 ====================

// spawnSLBot 대기실의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *SLHub) spawnSLBot(room *slRoom, name string) bool {
	bot := &SLClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runSLBot(bot)
	return true
}

// takeoverSLBot 유예 만료 좌석을 이어받는 연습봇 — 이름·좌석을 유지해
// 진행 중인 차례가 그대로 이어진다
func (h *SLHub) takeoverSLBot(room *slRoom, seat int, name string) *SLClient {
	bot := &SLClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	bot.GameID = room.Game.ID
	bot.Seat = seat
	h.runSLBot(bot)
	return bot
}

// runSLBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *SLHub) runSLBot(bot *SLClient) {
	brain := newSLBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m SLMessage) { h.gameMessage <- SLGameMessage{Client: bot, Message: m} },
		func(m SLMessage) bool { return m.Type == SLMsgGameOver || m.Type == SLMsgSessionExpired })
}

// slRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록용)
func slRoomHasBot(room *slRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
