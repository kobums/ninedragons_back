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
//   - 플레이: cwBotPickPlay 참고. 핵심은 임무 카드를 언제 흘리느냐다 —
//     담당자가 가져갈 수 있는 자리에만 넣고, 그 외에는 손에 쥔다.
//     말을 못 하는 게임이라 봇끼리 관습을 공유한다: **담당자는 트릭을
//     이기려 하고, 카드를 쥔 사람은 담당자가 이기는 트릭에 얹는다.**
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

// cwBotTaskOwner 아직 못 끝낸 임무 중 이 카드를 맡은 좌석 (임무 카드가 아니면 -1)
func cwBotTaskOwner(tasks []CWTask, card CWCard) int {
	for _, t := range tasks {
		if !t.Done && t.Suit == card.Suit && t.Rank == card.Rank {
			return t.Seat
		}
	}
	return -1
}

// cwBotPickPlay 낼 카드의 손패 인덱스.
//
// 협력 게임이라 "무엇을 내지 않을지"가 더 중요하다. 임무 카드는 담당자가 그
// 카드가 든 트릭을 이겨야 완료되므로, 아무 때나 흘리면 그 순간 판이 끝난다.
// 그래서 임무 카드(내 것·남의 것 모두)는 기본적으로 손에 쥐고 있고, 완료로
// 이어지는 자리에서만 낸다.
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

	owner := func(i int) int { return cwBotTaskOwner(s.Tasks, s.YourHand[i]) }
	strength := func(i int) int { return cwStrength(s.YourHand[i], s.LeadSuit) }
	// 조건을 만족하는 후보 중 가장 약한 카드 (센 카드는 아껴 둔다)
	pickWeakest := func(ok func(int) bool) int {
		pick, pickStrength := -1, 0
		for _, i := range legal {
			if !ok(i) {
				continue
			}
			if v := strength(i); pick < 0 || v < pickStrength {
				pick, pickStrength = i, v
			}
		}
		return pick
	}

	// 지금 트릭을 이기고 있는 카드의 세기와 그 주인
	best, bestSeat := -1, -1
	for _, tc := range s.Trick {
		if v := cwStrength(tc.Card, s.LeadSuit); v > best {
			best, bestSeat = v, tc.Seat
		}
	}

	// 내가 이 트릭의 마지막 주자인가 — 카드가 남은 좌석 중 아직 안 낸 사람이
	// 나뿐이면 승자가 확정되므로 과감하게 둘 수 있다. (인원이 나누어떨어지지
	// 않는 판은 트릭 참가자가 줄어들어 len(Players) 로 세면 틀린다.)
	played := map[int]bool{}
	for _, tc := range s.Trick {
		played[tc.Seat] = true
	}
	waiting := 0
	for _, p := range s.Players {
		if p.HandCount > 0 && !played[p.Seat] {
			waiting++
		}
	}
	last := waiting <= 1

	// 트릭에 이미 깔린 임무 카드가 누구 것인지
	mineInTrick, othersInTrick := false, false
	trickTaskOwners := map[int]bool{}
	for _, tc := range s.Trick {
		o := cwBotTaskOwner(s.Tasks, tc.Card)
		switch {
		case o < 0:
			continue
		case o == s.YourSeat:
			mineInTrick = true
		default:
			othersInTrick = true
		}
		trickTaskOwners[o] = true
	}

	// 한 트릭에 담당자가 다른 임무 카드가 둘 들어가면 누가 이기든 한쪽은
	// 반드시 깨진다 — 그런 자리에는 임무 카드를 절대 보태지 않는다.
	compatible := func(o int) bool {
		for other := range trickTaskOwners {
			if other != o {
				return false
			}
		}
		return true
	}

	// 1) 내 임무 카드가 트릭에 있다 — 최소한으로 이겨서 완료한다.
	//    이때 남의 임무 카드로 이기면 그 임무가 깨지므로 후보에서 뺀다.
	if mineInTrick {
		canWin := func(i int) bool {
			o := owner(i)
			return strength(i) > best && (o < 0 || o == s.YourSeat)
		}
		// 마지막 주자가 아니면 값싸게 이겨 봐야 뒤에서 뒤집힌다 — 뒤집기 어려운
		// 카드(로켓·그 무늬 최고 숫자)로 확실히 가져온다
		if !last {
			if p := pickWeakest(func(i int) bool {
				if !canWin(i) {
					return false
				}
				c := s.YourHand[i]
				return c.Suit == CWSuitRocket ||
					(c.Suit == s.LeadSuit && c.Rank == CWColorMaxRank)
			}); p >= 0 {
				return p
			}
		}
		if p := pickWeakest(canWin); p >= 0 {
			return p
		}
	}

	// 2) 내 임무 카드로 지금 이길 수 있다 — 마지막 주자면 확실하고,
	//    아니면 뒤집히기 어려운 센 카드일 때만 건다
	if p := pickWeakest(func(i int) bool {
		if owner(i) != s.YourSeat || strength(i) <= best {
			return false
		}
		if !compatible(s.YourSeat) {
			return false
		}
		return last || s.YourHand[i].Suit == CWSuitRocket || s.YourHand[i].Rank >= 8
	}); p >= 0 {
		return p
	}

	// 임무 카드는 담당자 손에 있으란 법이 없다. 그래서 봇끼리는 약속을 하나
	// 공유한다 — **담당자는 트릭을 이기려 하고, 카드를 쥔 사람은 담당자가
	// 이기고 있을 때 그 카드를 흘린다.** 말을 못 하는 게임이라 이 관습이
	// 사실상 유일한 합 맞추는 수단이다.
	myTaskPending, iHoldMyTask := false, false
	for _, t := range s.Tasks {
		if !t.Done && t.Seat == s.YourSeat {
			myTaskPending = true
		}
	}
	for i := range s.YourHand {
		if cwBotTaskOwner(s.Tasks, s.YourHand[i]) == s.YourSeat {
			iHoldMyTask = true
		}
	}

	// 3) 남의 임무 카드를 쥐고 있다 — 담당자가 가져갈 수 있는 트릭에 흘린다.
	//
	//    아껴 두면 안전해 보이지만 정반대다. 끝까지 쥐고 있으면 마지막 트릭에
	//    따라내기 의무로 억지로 나가고, 그 판은 아무도 통제하지 못해 거의 항상
	//    엉뚱한 사람이 가져간다. 그래서 담당자가 **이미 이기고 있거나 아직 낼
	//    차례가 남아 있을 때**(위 관습대로 이기려 든다) 내 카드가 승자를 바꾸지
	//    않는 선에서 미리 넘긴다.
	handCount := map[int]int{}
	for _, p := range s.Players {
		handCount[p.Seat] = p.HandCount
	}
	//    다만 "아직 안 냈다"는 것만으로는 부족하다 — 이미 로켓이 깔린 트릭은
	//    담당자가 넘기 어려우므로 그때는 참는다.
	// 손패가 얼마 안 남았으면 더 기다릴 여유가 없다. 끝까지 쥐고 있으면 마지막
	// 트릭에 따라내기 의무로 강제로 나가고 그 판은 통제가 안 되므로, 다소
	// 위험해도 담당자가 가져갈 가능성이 있는 자리에 먼저 흘린다.
	endgame := len(s.YourHand) <= 3

	dumpOK := func(i int) bool {
		o := owner(i)
		if o < 0 || o == s.YourSeat || !compatible(o) {
			return false
		}
		if o == bestSeat {
			if strength(i) >= best {
				return false // 내가 뺏어 버리면 임무가 깨진다
			}
			if last || endgame {
				return true // 승자 확정이거나, 더 미룰 수 없다
			}
			// 뒤에 낼 사람이 뒤집을 수 있다. 담당자가 로켓이나 그 무늬 최고 숫자로
			// 이기고 있을 때만 건다 (8로는 9에게 넘어간다).
			return true
		}
		if played[o] || handCount[o] <= 0 {
			return false // 담당자가 이 트릭에서 가져갈 방법이 없다
		}
		if best < 0 {
			// 남의 임무 카드로 리드하면 안 된다. 담당자가 그 무늬를 쥐고 있으면
			// 따라내기 의무로 낮은 카드를 강제로 내야 해서 오히려 못 이긴다.
			return false
		}
		if strength(i) >= best {
			return false
		}
		return best < 200 || endgame // 로켓이 깔렸으면 넘기 어렵다 (막판은 감수)
	}
	if p := pickWeakest(dumpOK); p >= 0 {
		return p
	}

	// 3-2) 내 임무인데 그 카드가 내 손에 없다 — 누군가 넘겨줄 수 있도록
	//      값싸게 이겨 둔다. 남의 임무 카드가 깔린 트릭이면 이기면 안 된다
	if myTaskPending && !iHoldMyTask && !othersInTrick && len(s.Trick) > 0 {
		if p := pickWeakest(func(i int) bool {
			return strength(i) > best && owner(i) < 0
		}); p >= 0 {
			return p
		}
	}

	// 4) 남의 임무 카드가 트릭에 있다 — 내가 이기면 그 임무가 깨진다.
	//    확실히 지는 카드로, 되도록 임무가 아닌 카드를 낸다
	if othersInTrick {
		if p := pickWeakest(func(i int) bool {
			return strength(i) < best && owner(i) < 0
		}); p >= 0 {
			return p
		}
		if p := pickWeakest(func(i int) bool { return strength(i) < best }); p >= 0 {
			return p
		}
	}

	// 5) 내가 리드다
	if len(s.Trick) == 0 {
		// 내 임무 카드가 세면 지금이 완료할 기회다
		if p := pickWeakest(func(i int) bool {
			return owner(i) == s.YourSeat &&
				(s.YourHand[i].Suit == CWSuitRocket || s.YourHand[i].Rank >= 8)
		}); p >= 0 {
			return p
		}
		// 내 임무 카드를 남이 쥐고 있다 — 센 색 카드로 리드해 이겨 두면 쥔
		// 사람이 그 위에 얹어 줄 수 있다. **로켓은 쓰지 않는다** — 임무 카드가
		// 트릭에 나왔을 때 트럼프로 걷어오는 게 가장 확실한 완료 수단이라
		// 아껴 둬야 한다.
		if myTaskPending && !iHoldMyTask {
			pick, pickStrength := -1, -1
			for _, i := range legal {
				if owner(i) >= 0 || s.YourHand[i].Suit == CWSuitRocket {
					continue
				}
				if v := strength(i); v > pickStrength {
					pick, pickStrength = i, v
				}
			}
			if pick >= 0 {
				return pick
			}
		}
	}

	// 6) 임무 카드는 손에 남기고 중립 카드 중 최약. 임무 카드밖에 없으면 어쩔 수 없다
	if p := pickWeakest(func(i int) bool { return owner(i) < 0 }); p >= 0 {
		return p
	}
	return pickWeakest(func(int) bool { return true })
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
