package server

import (
	"encoding/json"
	"math/rand"
	"sync"
	"time"
)

// ==================== 세트 연습봇 ====================
//
// 은닉이 없어 봇도 사람과 완전히 같은 정보(바닥·점수·잠금)를 본다. 남는 것은
// 속도 차이뿐이라, 봇의 실력은 "얼마나 자주 훑어보는가"로만 조절한다.
//
// 차례가 없는 게임이라 봇의 구조도 다른 게임과 다르다. 다른 게임의 봇은
// 스냅샷을 받을 때만 반응하는 순수 반응형(runBot)이지만, 세트에서는 아무도
// 움직이지 않으면 스냅샷 자체가 오지 않아 판이 멈춰버린다. 그래서 봇은
// 두 고루틴으로 나뉜다.
//
//   - 수신 고루틴: 스냅샷을 받아 최신 것으로 갈아 끼운다 (판단하지 않는다).
//     se_game_over / se_session_expired 를 보면 done 을 닫아 둘 다 끝낸다.
//   - 시계 고루틴: 4~12초 간격으로 스스로 깨어나 마지막 스냅샷을 훑고
//     세트를 하나 집는다. 남이 멈춰도 판이 멈추지 않는 근거다.
//
// 20% 확률로 일부러 틀린 3장을 골라 감점·잠금을 실제로 겪는다 —
// 프론트의 잠금 오버레이가 봇전에서도 검증된다. 사람이 먼저 가져가 사라진
// 카드를 집는 경우는 서버가 자연히 오답 처리하므로 별도 처리가 없다.

// 봇이 바닥을 다시 훑는 간격 (테스트에서 짧게 낮춘다)
var (
	seBotIntervalMin = 4 * time.Second
	seBotIntervalMax = 12 * time.Second
)

// seBotWrongChance 일부러 오답을 내는 확률
const seBotWrongChance = 0.2

// seBotPlayerView 봇이 스냅샷에서 꺼내 쓰는 좌석 정보
type seBotPlayerView struct {
	Seat        int   `json:"seat"`
	LockedUntil int64 `json:"lockedUntil"`
}

// seBotState 봇이 스냅샷에서 꺼내 쓰는 최소 정보 (전부 공개 정보다)
type seBotState struct {
	Phase    SEPhase           `json:"phase"`
	YourSeat int               `json:"yourSeat"`
	Board    []SECard          `json:"board"`
	Players  []seBotPlayerView `json:"players"`
}

// seBrain 스냅샷 보관소 + 판단 (봇 대체 좌석도 같은 두뇌를 쓴다).
// state 는 수신 고루틴이 쓰고 시계 고루틴이 읽으므로 mu 로 지킨다.
// rng 는 시계 고루틴에서만 쓴다.
type seBrain struct {
	rng *rand.Rand

	mu    sync.Mutex
	state seBotState
	seen  bool

	done     chan struct{}
	stopOnce sync.Once
}

func newSEBrain() *seBrain {
	return &seBrain{
		rng:  rand.New(rand.NewSource(time.Now().UnixNano())),
		done: make(chan struct{}),
	}
}

// observe 최신 스냅샷으로 갈아 끼운다 (수신 고루틴)
func (b *seBrain) observe(s seBotState) {
	b.mu.Lock()
	b.state, b.seen = s, true
	b.mu.Unlock()
}

// snapshot 마지막 스냅샷 (시계 고루틴). 아직 하나도 못 받았으면 ok=false
func (b *seBrain) snapshot() (seBotState, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state, b.seen
}

// stop 두 고루틴을 함께 끝낸다 (중복 호출 안전)
func (b *seBrain) stop() {
	b.stopOnce.Do(func() { close(b.done) })
}

// pick 마지막 스냅샷을 보고 보낼 claim 을 정한다 (없으면 nil).
// 잠금 중에는 아예 보내지 않는다 — 어차피 에러로 되돌아온다.
func (b *seBrain) pick(s seBotState) *SEMessage {
	if s.Phase != SEPhasePlaying || len(s.Board) < SEClaimSize {
		return nil
	}
	me := -1
	for i, p := range s.Players {
		if p.Seat == s.YourSeat {
			me = i
			break
		}
	}
	if me < 0 {
		return nil
	}
	if s.Players[me].LockedUntil > time.Now().UnixMilli() {
		return nil
	}

	ids, ok := seBotChoose(s.Board, b.rng.Float64() < seBotWrongChance, b.rng)
	if !ok {
		return nil
	}
	return &SEMessage{Type: SEMsgClaim, Payload: SEClaimPayload{IDs: ids}}
}

// seBotFindSet 바닥에서 세트 하나를 찾되 탐색 순서를 섞는다 — 여러 봇이
// 매번 같은 세트만 집어 서로 부딪히지 않게 하는 장치다.
func seBotFindSet(board []SECard, rng *rand.Rand) ([3]int, bool) {
	order := rng.Perm(len(board))
	n := len(order)
	for i := 0; i < n-2; i++ {
		for j := i + 1; j < n-1; j++ {
			for k := j + 1; k < n; k++ {
				a, b2, c := order[i], order[j], order[k]
				if seIsSet(board[a], board[b2], board[c]) {
					return [3]int{a, b2, c}, true
				}
			}
		}
	}
	return [3]int{}, false
}

// seBotChoose 봇이 집을 카드 id 3장을 고른다.
// wrong 이면 일부러 세트가 아닌 3장을 고른다 (못 찾으면 정직하게 간다).
func seBotChoose(board []SECard, wrong bool, rng *rand.Rand) ([]int, bool) {
	if len(board) < SEClaimSize {
		return nil, false
	}
	if wrong {
		for try := 0; try < 20; try++ {
			p := rng.Perm(len(board))[:SEClaimSize]
			if !seIsSet(board[p[0]], board[p[1]], board[p[2]]) {
				return []int{board[p[0]].ID, board[p[1]].ID, board[p[2]].ID}, true
			}
		}
	}
	idx, ok := seBotFindSet(board, rng)
	if !ok {
		return nil, false
	}
	return []int{board[idx[0]].ID, board[idx[1]].ID, board[idx[2]].ID}, true
}

// seBotInterval 다음으로 바닥을 훑을 때까지의 간격
func seBotInterval(rng *rand.Rand) time.Duration {
	lo, hi := seBotIntervalMin, seBotIntervalMax
	if hi <= lo {
		return lo
	}
	return lo + time.Duration(rng.Int63n(int64(hi-lo)))
}

// ==================== 봇 소환 ====================

// spawnSEBot 대기실의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *SEHub) spawnSEBot(room *seRoom, name string) bool {
	bot := &SEClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runSEBot(bot)
	return true
}

// takeoverSEBot 유예 만료 좌석을 이어받는 연습봇 — 이름·좌석·점수를 유지한다
func (h *SEHub) takeoverSEBot(room *seRoom, seat int, name string) *SEClient {
	bot := &SEClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	bot.GameID = room.Game.ID
	bot.Seat = seat
	h.runSEBot(bot)
	return bot
}

// runSEBot 수신 고루틴과 시계 고루틴을 함께 띄운다.
// 게임 종료·세션 만료 신호를 받으면 둘 다 스스로 끝난다 (고루틴 누수 방지).
func (h *SEHub) runSEBot(bot *SEClient) {
	brain := newSEBrain()

	go func() {
		defer brain.stop()
		for data := range bot.Send {
			var msg SEMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			switch msg.Type {
			case SEMsgGameOver, SEMsgSessionExpired:
				return
			case SEMsgGameState:
				if state, ok := botPayloadAs[seBotState](msg.Payload); ok {
					brain.observe(state)
				}
			}
		}
	}()

	go func() {
		for {
			timer := time.NewTimer(seBotInterval(brain.rng))
			select {
			case <-brain.done:
				timer.Stop()
				return
			case <-timer.C:
			}

			state, ok := brain.snapshot()
			if !ok {
				continue
			}
			reply := brain.pick(state)
			if reply == nil {
				continue
			}
			select {
			case h.gameMessage <- SEGameMessage{Client: bot, Message: *reply}:
			case <-brain.done:
				return
			}
		}
	}()
}

// seRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록용)
func seRoomHasBot(room *seRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
