package server

import (
	"fmt"
	"math/rand"
	"sort"
	"time"
)

// ==================== 달무티 연습봇 ====================
//
// 스냅샷(dm_game_state)만 보고 반응한다. 봇도 사람과 같은 조건 — 자기 손패
// (yourHand)만 알고 남의 손패는 장수(handCount)만 안다.
//   - 리드: 최다 장수 세트 중 가장 높은(약한) 숫자 — 약한 카드부터 소진
//   - 팔로우: 유효한 것 중 가장 높은(약한) 숫자, 40%는 아껴 두고 패스
//   - 조커는 세트 완성(장수 채우기)에만 쓴다
// 같은 차례에 스냅샷이 여러 번 와도(관전 입장·리액션 등) 한 번만 응답하도록
// 대기 상태 식별키(phase+endsAt+currentSeat+handNo+테이블)로 중복을 걸러낸다.

// dmBotPass 팔로우에서 손을 아끼는 확률
const dmBotPass = 0.4

// dmBotState 봇이 상태 스냅샷에서 꺼내 쓰는 최소 정보
type dmBotState struct {
	YourSeat    int         `json:"yourSeat"`
	Phase       DMPhase     `json:"phase"`
	HandNo      int         `json:"handNo"`
	CurrentSeat int         `json:"currentSeat"`
	EndsAt      int64       `json:"endsAt"`
	TableSet    *DMTableSet `json:"tableSet"`
	YourHand    []int       `json:"yourHand"`
}

// dmBrain 스냅샷 기반 판단 (봇 대체 좌석도 같은 두뇌를 쓴다)
type dmBrain struct {
	rng *rand.Rand
	// lastKey 마지막으로 응답한 대기 상태 식별키 (중복 응답 방지)
	lastKey string
}

func newDMBrain() *dmBrain {
	return &dmBrain{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

// decide 공용 러너 계약 — dm_game_state 에만 반응한다
func (b *dmBrain) decide(msg DMMessage) *DMMessage {
	if msg.Type != DMMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[dmBotState](msg.Payload)
	if !ok {
		return nil
	}
	return b.decideState(state)
}

// handled 같은 대기 상태에 이미 응답했는지 — 처음이면 키를 기록한다
func (b *dmBrain) handled(key string) bool {
	if b.lastKey == key {
		return true
	}
	b.lastKey = key
	return false
}

func (b *dmBrain) decideState(s dmBotState) *DMMessage {
	if s.Phase != DMPhasePlaying || s.YourSeat < 0 || s.CurrentSeat != s.YourSeat {
		return nil
	}
	if len(s.YourHand) == 0 {
		return nil
	}
	table := "lead"
	if s.TableSet != nil {
		table = fmt.Sprintf("%d/%d", s.TableSet.Rank, s.TableSet.Count)
	}
	key := fmt.Sprintf("%s|%d|%d|%d|%s", s.Phase, s.EndsAt, s.CurrentSeat, s.HandNo, table)
	if b.handled(key) {
		return nil
	}

	// 리드는 패스할 수 없다 — 반드시 세트를 낸다
	if s.TableSet == nil {
		cards := dmLeadChoice(s.YourHand)
		if cards == nil { // 손패가 있는 한 도달하지 않는 방어선
			return nil
		}
		return &DMMessage{Type: DMMsgPlay, Payload: DMPlayPayload{Cards: cards}}
	}

	cards := dmFollowChoice(s.YourHand, s.TableSet.Rank, s.TableSet.Count)
	if cards == nil || b.rng.Float64() < dmBotPass {
		return &DMMessage{Type: DMMsgPass}
	}
	sort.Ints(cards)
	return &DMMessage{Type: DMMsgPlay, Payload: DMPlayPayload{Cards: cards}}
}

// ==================== 봇 소환 ====================

// spawnDMBot 대기실의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *DMHub) spawnDMBot(room *dmRoom, name string) bool {
	bot := &DMClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runDMBot(bot)
	return true
}

// takeoverDMBot 유예 만료 좌석을 이어받는 연습봇 — 이름·좌석을 유지해
// 진행 중인 차례가 그대로 이어진다
func (h *DMHub) takeoverDMBot(room *dmRoom, seat int, name string) *DMClient {
	bot := &DMClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	bot.GameID = room.Game.ID
	bot.Seat = seat
	h.runDMBot(bot)
	return bot
}

// runDMBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *DMHub) runDMBot(bot *DMClient) {
	brain := newDMBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m DMMessage) { h.gameMessage <- DMGameMessage{Client: bot, Message: m} },
		func(m DMMessage) bool { return m.Type == DMMsgGameOver || m.Type == DMMsgSessionExpired })
}

// dmRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록용)
func dmRoomHasBot(room *dmRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
