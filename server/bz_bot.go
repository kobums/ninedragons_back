package server

import (
	"fmt"
	"math/rand"
	"strings"
	"time"
)

// ==================== 보난자 연습봇 ====================
//
// 스냅샷(bz_game_state)만 보고 반응한다. 봇도 사람과 같은 조건 — 자기
// yourHand·yourPending 만 알고 남의 손패·덱은 모른다.
//
// 판단은 네 갈래다.
//
//	① 심기 (plant)
//	   - 맞는 밭이 있으면 거기에, 없으면 밭 하나를 수확해 자리를 만든다.
//	   - **콩미터 문턱을 막 넘긴 밭은 수확하고, 문턱 직전이면 버틴다.**
//	   - 두 번째 카드는 "이미 있는 밭에 얹을 수 있을 때만" 심는다
//	     (빈 밭을 함부로 쓰면 세 번째 콩이 갈 곳이 없어진다).
//	② 거래 (trade)
//	   - 차례인 사람: 내 밭에 안 맞는 공개 카드를, 그 콩 밭을 가진 사람에게
//	     넘긴다. 먼저 "네 맨 앞 카드와 바꾸자"고 하고, 거절당하면 그냥 준다.
//	   - 차례가 아닌 사람: 내 손패에서 못 쓸 콩 중 차례인 사람 밭에 맞는 것을
//	     기부한다 (모든 거래에는 차례인 사람이 껴야 하므로 상대는 그뿐이다).
//	   - 받은 제안은 **주고받는 콩의 값어치를 비교해** 수락/거절한다.
//	     반드시 답한다 — 답하지 않으면 제안이 판에 남아 진행이 멈춘다.
//	③ 받은 카드 심기 (plant_received)
//	   - 맞는 밭에 놓고, 자리가 없으면 먼저 수확해 만든다.
//	④ 세 번째 콩밭
//	   - 초반(덱 소진 전)에 금화가 넉넉하면 산다. 후반에는 회수 못 하므로 안 산다.
//
// 같은 대기 상태에 스냅샷이 여러 번 와도(관전 입장·접속 변화 등) 한 번만
// 행동하도록 상태 식별키로 중복을 걸러낸다.

// 봇이 "생각하는" 시간 (테스트에서 짧게 낮춘다)
var (
	bzBotDelay    = 600 * time.Millisecond
	bzBotJitterMs = 600
)

// 봇 판단 손잡이 (밸런스 조정용 — 봇 품질 측정 테스트가 이 값을 읽는다)
var (
	// bzBotMatchScore 내 밭에 이미 있는 콩의 기본 값어치
	bzBotMatchScore = 2.0
	// bzBotEmptyScore 빈 밭에 새로 심을 콩의 값어치
	bzBotEmptyScore = 1.0
	// bzBotNoRoomScore 놓을 자리가 없는 콩의 값어치 (수확을 강요당한다)
	bzBotNoRoomScore = -1.5
	// bzBotCappedScore 더 오를 문턱이 없는 밭에 얹는 콩의 값어치
	bzBotCappedScore = 0.5
	// bzBotTradeMargin 제안을 수락하는 최소 이득
	bzBotTradeMargin = 0.2
	// bzBotMaxOffersPerTurn 한 차례에 봇이 만드는 제안 수 상한 (교착 방지)
	bzBotMaxOffersPerTurn = 3
	// bzBotBuyFieldCoins 세 번째 콩밭을 사기 위한 최소 금화
	bzBotBuyFieldCoins = 4
)

// bzBotPlayerView 봇이 스냅샷에서 꺼내 쓰는 좌석 정보
type bzBotPlayerView struct {
	Seat       int       `json:"seat"`
	Coins      int       `json:"coins"`
	HandCount  int       `json:"handCount"`
	FieldCount int       `json:"fieldCount"`
	Fields     []BZField `json:"fields"`
}

// bzBotOffer 봇이 보는 제안. 상세는 당사자일 때만 실려 오고, 요구한 자리에
// 실제로 무엇이 있는지(WantBeans)는 **요청받은 쪽에게만** 실려 온다 —
// 그래서 봇도 사람과 똑같이 "내가 내줄 콩"은 알고 "상대가 내줄 콩"은 모른다.
type bzBotOffer struct {
	ID          string   `json:"id"`
	FromSeat    int      `json:"fromSeat"`
	ToSeat      int      `json:"toSeat"`
	GiveHand    []BZBean `json:"giveHand"`
	GiveFlipped []BZBean `json:"giveFlipped"`
	WantHand    []int    `json:"wantHand"`
	WantBeans   []BZBean `json:"wantBeans"`
}

// bzBotState 봇이 상태 스냅샷에서 꺼내 쓰는 최소 정보
type bzBotState struct {
	YourSeat    int               `json:"yourSeat"`
	Phase       BZPhase           `json:"phase"`
	CurrentSeat int               `json:"currentSeat"`
	DeckLeft    int               `json:"deckLeft"`
	DeckCycle   int               `json:"deckCycle"`
	Flipped     []BZBean          `json:"flipped"`
	Offers      []bzBotOffer      `json:"offers"`
	YourHand    []BZBean          `json:"yourHand"`
	YourPending []BZBean          `json:"yourPending"`
	Players     []bzBotPlayerView `json:"players"`
}

// fieldsOf 좌석의 콩밭 (없으면 nil)
func (s bzBotState) fieldsOf(seat int) []BZField {
	for _, p := range s.Players {
		if p.Seat == seat {
			return p.Fields
		}
	}
	return nil
}

// handCountOf 좌석의 손패 장수
func (s bzBotState) handCountOf(seat int) int {
	for _, p := range s.Players {
		if p.Seat == seat {
			return p.HandCount
		}
	}
	return 0
}

// mine 내 좌석 정보
func (s bzBotState) mine() (bzBotPlayerView, bool) {
	for _, p := range s.Players {
		if p.Seat == s.YourSeat {
			return p, true
		}
	}
	return bzBotPlayerView{}, false
}

// bzBeanScore 그 콩 한 장이 이 밭 구성에 얼마나 값진지.
// 이미 있는 밭에 얹을 수 있으면 값지고(문턱이 가까울수록 더), 빈 밭뿐이면
// 보통이며, 자리가 없으면 수확을 강요당하므로 음수다.
func bzBeanScore(fields []BZField, bean BZBean) float64 {
	for _, f := range fields {
		if f.Count > 0 && f.Bean == bean {
			next := bzNextThreshold(bean, f.Count)
			if next == 0 {
				return bzBotCappedScore
			}
			return bzBotMatchScore + 1.0/float64(next-f.Count)
		}
	}
	for _, f := range fields {
		if f.Count == 0 {
			return bzBotEmptyScore
		}
	}
	return bzBotNoRoomScore
}

// bzBotShouldHarvest 지금 이 밭을 수확할 만한지 —
// **문턱을 막 넘겼으면 수확하고, 다음 문턱이 코앞(1장)이면 버틴다.**
func bzBotShouldHarvest(f BZField) bool {
	if f.Count == 0 {
		return false
	}
	coins := bzCoins(f.Bean, f.Count)
	if coins == 0 {
		return false
	}
	next := bzNextThreshold(f.Bean, f.Count)
	if next == 0 {
		return true // 더 오를 칸이 없다 — 쥐고 있을 이유가 없다
	}
	return next-f.Count >= 2
}

// bzBotHarvestChoice 자리를 만들려고 밭 하나를 팔아야 할 때 고르는 밭.
// 수확 가능한 밭 중 금화가 가장 많은 밭, 같으면 가장 적게 쌓인 밭.
func bzBotHarvestChoice(fields []BZField) int {
	best := -1
	for i := range fields {
		if !bzCanHarvest(fields, i) {
			continue
		}
		if best < 0 {
			best = i
			continue
		}
		ci := bzCoins(fields[i].Bean, fields[i].Count)
		cb := bzCoins(fields[best].Bean, fields[best].Count)
		if ci != cb {
			if ci > cb {
				best = i
			}
			continue
		}
		if fields[i].Count < fields[best].Count {
			best = i
		}
	}
	return best
}

// bzBrain 스냅샷 기반 판단 (봇 대체 좌석도 같은 두뇌를 쓴다)
type bzBrain struct {
	rng *rand.Rand
	// lastKey 마지막으로 행동한 대기 상태 식별키 (중복 행동 방지)
	lastKey string
	// turnSig 지금 보고 있는 차례의 식별키 — 바뀌면 차례별 장부를 비운다
	turnSig string
	// offersMade 이번 차례에 내가 만든 제안 수 (교착 방지 상한)
	offersMade int
	// tried 이번 차례에 이미 시도한 제안 (같은 제안 반복 금지)
	tried map[string]bool
}

func newBZBrain() *bzBrain {
	return &bzBrain{
		rng:   rand.New(rand.NewSource(time.Now().UnixNano())),
		tried: map[string]bool{},
	}
}

// decide 공용 러너 계약 — bz_game_state 에만 반응한다
func (b *bzBrain) decide(msg BZMessage) *BZMessage {
	if msg.Type != BZMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[bzBotState](msg.Payload)
	if !ok {
		return nil
	}
	return b.decideState(state)
}

// think 사람처럼 잠깐 뜸을 들인다 (테스트에서는 var 를 낮춰 즉시 진행한다)
func (b *bzBrain) think() {
	d := bzBotDelay
	if bzBotJitterMs > 0 {
		d += time.Duration(b.rng.Intn(bzBotJitterMs)) * time.Millisecond
	}
	if d > 0 {
		time.Sleep(d)
	}
}

// stateKey 같은 대기 상태를 식별하는 키 — 판이 조금이라도 바뀌면 달라진다
func (b *bzBrain) stateKey(s bzBotState) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s|%d|%d|%d|%d|%d|%d", s.Phase, s.CurrentSeat, s.DeckLeft,
		s.DeckCycle, len(s.Flipped), len(s.YourHand), len(s.YourPending))
	for _, p := range s.Players {
		fmt.Fprintf(&sb, "|%d:%d:%d", p.Seat, p.Coins, p.HandCount)
		for _, f := range p.Fields {
			fmt.Fprintf(&sb, ",%s%d", f.Bean, f.Count)
		}
	}
	for _, o := range s.Offers {
		fmt.Fprintf(&sb, "|%s", o.ID)
	}
	return sb.String()
}

// turnSignature 차례를 식별하는 키 (거래 장부를 차례마다 비우는 근거)
func (b *bzBrain) turnSignature(s bzBotState) string {
	return fmt.Sprintf("%d|%d|%d", s.CurrentSeat, s.DeckCycle, s.DeckLeft)
}

func (b *bzBrain) decideState(s bzBotState) *BZMessage {
	me := s.YourSeat
	if me < 0 || len(s.Players) == 0 {
		return nil
	}
	if _, ok := s.mine(); !ok {
		return nil
	}
	switch s.Phase {
	case BZPhasePlant, BZPhaseTrade, BZPhasePlantReceived:
	default:
		return nil
	}

	if sig := b.turnSignature(s); sig != b.turnSig {
		b.turnSig = sig
		b.offersMade = 0
		b.tried = map[string]bool{}
	}

	key := b.stateKey(s)
	if b.lastKey == key {
		return nil
	}

	act := func(m *BZMessage) *BZMessage {
		b.lastKey = key
		b.think()
		return m
	}

	// ① 나에게 온 제안에는 반드시 답한다 (답하지 않으면 판이 멈춘다)
	for _, o := range s.Offers {
		if o.ToSeat == me {
			return act(&BZMessage{Type: BZMsgRespond,
				Payload: BZRespondPayload{OfferID: o.ID, Accept: b.acceptable(s, o)}})
		}
	}

	// ② 받은 카드는 손에 못 드니 즉시 심는다
	if s.Phase == BZPhasePlantReceived && len(s.YourPending) > 0 {
		return act(b.plantReceived(s))
	}

	if s.CurrentSeat != me {
		// ③ 차례가 아닐 때 — 차례인 사람에게 못 쓸 콩을 기부한다
		if s.Phase == BZPhaseTrade {
			if m := b.donateOffer(s); m != nil {
				return act(m)
			}
		}
		return nil
	}

	switch s.Phase {
	case BZPhasePlant:
		return act(b.plantTurn(s))
	case BZPhaseTrade:
		return act(b.tradeTurn(s))
	}
	return nil
}

// acceptable 받은 제안이 이득인지 — 받는 콩의 값어치 합에서 내주는 콩의
// 값어치 합을 뺀다. 너무 영리하게 굴지 않는다.
func (b *bzBrain) acceptable(s bzBotState, o bzBotOffer) bool {
	fields := s.fieldsOf(s.YourSeat)
	gain := 0.0
	for _, bean := range o.GiveHand {
		gain += bzBeanScore(fields, bean)
	}
	for _, bean := range o.GiveFlipped {
		gain += bzBeanScore(fields, bean)
	}
	loss := 0.0
	for _, bean := range o.WantBeans { // 내 카드니 내가 무엇을 내주는지 안다
		loss += bzBeanScore(fields, bean)
	}
	return gain-loss > bzBotTradeMargin
}

// plantReceived ③ 받은 카드 한 장을 심는다 (자리가 없으면 먼저 수확)
func (b *bzBrain) plantReceived(s bzBotState) *BZMessage {
	fields := s.fieldsOf(s.YourSeat)
	bean := s.YourPending[0]
	if idx, ok := bzPlantTarget(fields, bean); ok {
		return &BZMessage{Type: BZMsgPlantReceived,
			Payload: BZPlantReceivedPayload{CardIndex: 0, Field: idx}}
	}
	if h := bzBotHarvestChoice(fields); h >= 0 {
		return &BZMessage{Type: BZMsgHarvest, Payload: BZHarvestPayload{Field: h}}
	}
	return nil
}

// plantTurn ① 내 차례의 심기 — 밭 사기 → 문턱 넘긴 밭 수확 → 자리 만들기 →
// 맨 앞 카드 심기 순서로 한 걸음씩 나아간다.
func (b *bzBrain) plantTurn(s bzBotState) *BZMessage {
	me, _ := s.mine()
	fields := s.fieldsOf(s.YourSeat)

	// 세 번째 콩밭은 회수할 시간이 남은 초반에만 산다
	if me.FieldCount < BZMaxFields && me.Coins >= bzBotBuyFieldCoins && s.DeckCycle == 0 {
		return &BZMessage{Type: BZMsgBuyField}
	}
	// 문턱을 막 넘긴 밭은 수확한다 (문턱 직전이면 버틴다)
	for i := range fields {
		if bzBotShouldHarvest(fields[i]) && bzCanHarvest(fields, i) {
			return &BZMessage{Type: BZMsgHarvest, Payload: BZHarvestPayload{Field: i}}
		}
	}
	if len(s.YourHand) == 0 {
		return nil // 규칙이 알아서 건너뛴다
	}
	// 맨 앞 카드를 놓을 자리가 없으면 먼저 밭 하나를 판다
	if _, ok := bzPlantTarget(fields, s.YourHand[0]); !ok {
		if h := bzBotHarvestChoice(fields); h >= 0 {
			return &BZMessage{Type: BZMsgHarvest, Payload: BZHarvestPayload{Field: h}}
		}
		return nil
	}
	return &BZMessage{Type: BZMsgPlant, Payload: BZPlantPayload{Second: b.wantSecond(s)}}
}

// wantSecond 두 번째 카드까지 심을지 — 맨 앞 카드를 심은 뒤에도 **이미 있는
// 밭에 얹을 수 있을 때만** 심는다 (빈 밭을 함부로 소모하지 않는다).
func (b *bzBrain) wantSecond(s bzBotState) bool {
	if len(s.YourHand) < 2 {
		return false
	}
	fields := append([]BZField{}, s.fieldsOf(s.YourSeat)...)
	idx, ok := bzPlantTarget(fields, s.YourHand[0])
	if !ok {
		return false
	}
	fields[idx] = BZField{Bean: s.YourHand[0], Count: fields[idx].Count + 1}
	for _, f := range fields {
		if f.Count > 0 && f.Bean == s.YourHand[1] {
			return true
		}
	}
	return false
}

// tradeTurn ② 내 차례의 거래 — 내 밭에 안 맞는 공개 카드를 넘기고 마감한다.
// 내가 낸 제안이 아직 살아 있으면 답을 기다린다.
func (b *bzBrain) tradeTurn(s bzBotState) *BZMessage {
	for _, o := range s.Offers {
		if o.FromSeat == s.YourSeat {
			return nil // 답을 기다린다
		}
	}
	if b.offersMade < bzBotMaxOffersPerTurn {
		if m := b.flippedOffer(s); m != nil {
			b.offersMade++
			return m
		}
	}
	return &BZMessage{Type: BZMsgEndPhase}
}

// flippedOffer 공개 카드 중 내 밭에 안 맞는 것을, 그 콩 밭을 가진 사람에게
// 넘기는 제안. 먼저 "네 맨 앞 카드와 바꾸자"로 시도하고 그게 이미 거절됐거나
// 상대 손패가 비었으면 그냥 준다 (기부).
func (b *bzBrain) flippedOffer(s bzBotState) *BZMessage {
	mine := s.fieldsOf(s.YourSeat)
	for i, bean := range s.Flipped {
		if bzBeanScore(mine, bean) >= bzBotMatchScore {
			continue // 나한테 값진 콩은 내가 심는다
		}
		for _, p := range s.Players {
			if p.Seat == s.YourSeat {
				continue
			}
			if bzBeanScore(p.Fields, bean) < bzBotMatchScore {
				continue // 상대 밭에도 안 맞으면 받아 주지 않는다
			}
			swapKey := fmt.Sprintf("s|%d|%s", p.Seat, bean)
			if p.HandCount > 0 && !b.tried[swapKey] {
				b.tried[swapKey] = true
				return &BZMessage{Type: BZMsgOffer, Payload: BZOfferPayload{
					ToSeat:      p.Seat,
					GiveHand:    []int{},
					GiveFlipped: []int{i},
					WantHand:    []int{0}, // 상대가 반드시 심어야 하는 맨 앞 카드
				}}
			}
			giftKey := fmt.Sprintf("g|%d|%s", p.Seat, bean)
			if !b.tried[giftKey] {
				b.tried[giftKey] = true
				return &BZMessage{Type: BZMsgOffer, Payload: BZOfferPayload{
					ToSeat:      p.Seat,
					GiveHand:    []int{},
					GiveFlipped: []int{i},
					WantHand:    []int{},
				}}
			}
		}
	}
	return nil
}

// donateOffer 차례가 아닐 때의 제안 — 내 손패에서 못 쓸 콩 하나를, 그 콩
// 밭을 가진 차례인 사람에게 기부한다 (모든 거래에는 차례인 사람이 껴야
// 하므로 상대는 그뿐이다).
func (b *bzBrain) donateOffer(s bzBotState) *BZMessage {
	if b.offersMade >= 1 {
		return nil
	}
	mine := s.fieldsOf(s.YourSeat)
	target := s.fieldsOf(s.CurrentSeat)
	if target == nil || s.handCountOf(s.YourSeat) == 0 {
		return nil
	}
	for i, bean := range s.YourHand {
		if bzBeanScore(mine, bean) >= bzBotMatchScore {
			continue // 내가 쓸 콩은 안 준다
		}
		if bzBeanScore(target, bean) < bzBotMatchScore {
			continue // 상대 밭에도 안 맞으면 받아 주지 않는다
		}
		key := fmt.Sprintf("d|%d|%s", s.CurrentSeat, bean)
		if b.tried[key] {
			continue
		}
		b.tried[key] = true
		b.offersMade++
		return &BZMessage{Type: BZMsgOffer, Payload: BZOfferPayload{
			ToSeat:      s.CurrentSeat,
			GiveHand:    []int{i},
			GiveFlipped: []int{},
			WantHand:    []int{},
		}}
	}
	return nil
}

// ==================== 봇 소환 ====================

// spawnBZBot 대기실의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *BZHub) spawnBZBot(room *bzRoom, name string) bool {
	bot := &BZClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runBZBot(bot)
	return true
}

// takeoverBZBot 유예 만료 좌석을 이어받는 연습봇 — 이름·좌석을 유지해
// 진행 중인 차례가 그대로 이어진다
func (h *BZHub) takeoverBZBot(room *bzRoom, seat int, name string) *BZClient {
	bot := &BZClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	bot.GameID = room.Game.ID
	bot.Seat = seat
	h.runBZBot(bot)
	return bot
}

// runBZBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *BZHub) runBZBot(bot *BZClient) {
	brain := newBZBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m BZMessage) { h.gameMessage <- BZGameMessage{Client: bot, Message: m} },
		func(m BZMessage) bool { return m.Type == BZMsgGameOver || m.Type == BZMsgSessionExpired })
}

// bzRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록용)
func bzRoomHasBot(room *bzRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
