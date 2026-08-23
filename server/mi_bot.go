package server

import (
	"encoding/json"
	"math/rand"
	"sync"
	"time"
)

// ==================== 더 마인드 연습봇 ====================
//
// 차례가 없는 게임이라 봇의 구조가 다른 게임과 다르다. 다른 게임의 봇은
// 스냅샷을 받을 때만 반응하는 순수 반응형(runBot)이지만, 더 마인드에서는
// 아무도 내지 않으면 스냅샷 자체가 오지 않아 전원이 서로를 기다리며
// 교착한다. 그래서 세트(se_bot)와 같은 **두 고루틴** 구조를 쓴다.
//
//   - 수신 고루틴: 스냅샷을 받아 최신 것으로 갈아 끼우고 변경을 알린다
//     (판단하지 않는다). mi_game_over / mi_session_expired 를 보면 done 을
//     닫아 둘 다 끝낸다.
//   - 시계 고루틴: 스스로 깨어나 "지금이다" 판단을 내린다.
//
// 판단의 전부는 기다리는 시간이다. 사람이 "숫자가 크면 오래 기다린다"로
// 맞추는 감각을 그대로 흉내낸다 —
//
//	대기(ms) ≈ (내 최저 카드 - 직전에 나온 수) × miBotTickPerGap ± 지터
//
// 스냅샷이 갱신되면(누가 카드를 냈다) 간격이 줄었으므로 타이머를 처음부터
// 다시 계산한다. 지터가 겹치는 만큼 순서가 뒤집혀 실수가 나는데, 그 실수가
// 이 게임의 재미이므로 없애지 않는다.

var (
	// miBotTickPerGap 간격 1당 기다리는 시간 (테스트에서 짧게 낮춘다).
	// 이 값이 봇 품질의 핵심 계수다 — 크면 순서가 더 정확해지고
	// (지터의 절대 폭도 함께 커지지만 상대 오차는 같다) 판이 느려진다.
	miBotTickPerGap = 220 * time.Millisecond

	// miBotJitterRatio 대기 시간에 섞는 상대 오차 (±비율).
	// 이것이 봇의 "손끝 감각" 오차이자 실수의 근원이다.
	miBotJitterRatio = 0.10

	// miBotMinWait / miBotWaitMax 대기 시간의 하한·상한
	miBotMinWait = 120 * time.Millisecond
	miBotWaitMax = 30 * time.Second

	// miBotIdleTick playing 이 아닐 때(카운트다운·정산) 다시 살펴보는 주기
	miBotIdleTick = 250 * time.Millisecond

	// miBotSettleWait 카드를 보낸 뒤 새 스냅샷을 기다리는 상한
	// (중복 발사 방지 — 갱신이 오면 바로 깬다)
	miBotSettleWait = 2 * time.Second

	// miBotStarChance 최저 카드가 90 이상일 때 수리검을 제안할 확률
	miBotStarChance = 0.15
)

// miBotPlayerView 봇이 스냅샷에서 꺼내 쓰는 좌석 정보 (공개 정보만)
type miBotPlayerView struct {
	Seat      int `json:"seat"`
	HandCount int `json:"handCount"`
}

// miBotStarVoteView 봇이 보는 수리검 투표
type miBotStarVoteView struct {
	Proposer int   `json:"proposer"`
	Accepted []int `json:"accepted"`
}

// miBotState 봇이 스냅샷에서 꺼내 쓰는 최소 정보.
// YourHand 는 자기 스냅샷에만 실리는 은닉 정보다 — 봇도 남의 손패는 모른다.
type miBotState struct {
	Phase      MIPhase            `json:"phase"`
	YourSeat   int                `json:"yourSeat"`
	LastPlayed int                `json:"lastPlayed"`
	Stars      int                `json:"stars"`
	YourHand   []int              `json:"yourHand"`
	Players    []miBotPlayerView  `json:"players"`
	StarVote   *miBotStarVoteView `json:"starVote"`
}

// miBrain 스냅샷 보관소 + 판단 (봇 대체 좌석도 같은 두뇌를 쓴다).
// state 는 수신 고루틴이 쓰고 시계 고루틴이 읽으므로 mu 로 지킨다.
// rng 는 시계 고루틴에서만 쓴다.
type miBrain struct {
	rng *rand.Rand

	mu    sync.Mutex
	state miBotState
	seen  bool

	// changed 스냅샷 갱신 신호 (버퍼 1 — 시계 고루틴이 기다리는 중이면 깨운다)
	changed chan struct{}

	done     chan struct{}
	stopOnce sync.Once
}

func newMIBrain() *miBrain {
	return &miBrain{
		rng:     rand.New(rand.NewSource(time.Now().UnixNano())),
		changed: make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
}

// observe 최신 스냅샷으로 갈아 끼우고 시계 고루틴을 깨운다 (수신 고루틴)
func (b *miBrain) observe(s miBotState) {
	b.mu.Lock()
	b.state, b.seen = s, true
	b.mu.Unlock()
	select {
	case b.changed <- struct{}{}:
	default:
	}
}

// snapshot 마지막 스냅샷 (시계 고루틴). 아직 하나도 못 받았으면 ok=false
func (b *miBrain) snapshot() (miBotState, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state, b.seen
}

// stop 두 고루틴을 함께 끝낸다 (중복 호출 안전)
func (b *miBrain) stop() {
	b.stopOnce.Do(func() { close(b.done) })
}

// sleep d 만큼 기다리되 스냅샷이 갱신되면 일찍 깬다.
// fired=true 는 끝까지 기다렸다는 뜻(= 이제 낼 때), alive=false 는 종료다.
func (b *miBrain) sleep(d time.Duration) (fired, alive bool) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-b.done:
		return false, false
	case <-b.changed:
		return false, true
	case <-timer.C:
		return true, true
	}
}

// waitFor 최저 카드 c 와 직전에 나온 수 사이의 간격에 비례한 대기 시간.
// 숫자가 클수록 오래 기다린다 — 사람이 쓰는 유일한 신호를 그대로 옮겼다.
func (b *miBrain) waitFor(card, lastPlayed int) time.Duration {
	gap := card - lastPlayed
	if gap < 1 {
		gap = 1
	}
	ms := float64(gap) * float64(miBotTickPerGap)
	ms *= 1 + (b.rng.Float64()*2-1)*miBotJitterRatio
	wait := time.Duration(ms)
	if wait < miBotMinWait {
		wait = miBotMinWait
	}
	if wait > miBotWaitMax {
		wait = miBotWaitMax
	}
	return wait
}

// voteReply 수리검 제안이 와 있으면 답을 정한다 (없으면 nil).
// 자기 최저 카드가 60 이상이면 찬성 — 큰 카드만 남았을 때가 수리검이
// 가장 값진 순간이기 때문이다. 손패가 비었으면 잃을 게 없어 찬성한다.
func miBotVoteReply(s miBotState) *MIMessage {
	if s.StarVote == nil || s.StarVote.Proposer == s.YourSeat {
		return nil
	}
	for _, seat := range s.StarVote.Accepted {
		if seat == s.YourSeat {
			return nil // 이미 답했다
		}
	}
	if len(s.YourHand) == 0 || s.YourHand[0] >= MIBotStarAcceptFrom {
		return &MIMessage{Type: MIMsgStarAccept}
	}
	return &MIMessage{Type: MIMsgStarDecline}
}

// ==================== 봇 소환 ====================

// spawnMIBot 대기실의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *MIHub) spawnMIBot(room *miRoom, name string) bool {
	bot := &MIClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runMIBot(bot)
	return true
}

// takeoverMIBot 유예 만료 좌석을 이어받는 연습봇 — 이름·좌석·손패를 유지한다
func (h *MIHub) takeoverMIBot(room *miRoom, seat int, name string) *MIClient {
	bot := &MIClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	bot.GameID = room.Game.ID
	bot.Seat = seat
	h.runMIBot(bot)
	return bot
}

// runMIBot 수신 고루틴과 시계 고루틴을 함께 띄운다.
// 게임 종료·세션 만료 신호를 받으면 둘 다 스스로 끝난다 (고루틴 누수 방지).
func (h *MIHub) runMIBot(bot *MIClient) {
	brain := newMIBrain()

	go func() {
		defer brain.stop()
		for data := range bot.Send {
			var msg MIMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			switch msg.Type {
			case MIMsgGameOver, MIMsgSessionExpired:
				return
			case MIMsgGameState:
				if state, ok := botPayloadAs[miBotState](msg.Payload); ok {
					brain.observe(state)
				}
			}
		}
	}()

	go h.driveMIBot(bot, brain)
}

// driveMIBot 시계 고루틴 — 이 게임에서 판이 멈추지 않는 유일한 근거다.
func (h *MIHub) driveMIBot(bot *MIClient, brain *miBrain) {
	for {
		state, ok := brain.snapshot()

		// 아직 낼 수 없는 국면(대기·카운트다운·정산)이거나 손패가 비었다 —
		// 갱신을 기다리되 신호를 놓쳐도 멈추지 않게 짧게 폴링한다
		if !ok || state.Phase != MIPhasePlaying || len(state.YourHand) == 0 {
			// 손패가 비어도 수리검 투표에는 답해야 판이 막히지 않는다
			if ok && state.Phase == MIPhasePlaying {
				if reply := miBotVoteReply(state); reply != nil {
					if !h.deliverMIBot(bot, brain, *reply, miBotVoted(state.YourSeat)) {
						return
					}
					continue
				}
			}
			if _, alive := brain.sleep(miBotIdleTick); !alive {
				return
			}
			continue
		}

		// 제안이 와 있으면 먼저 답한다 (전원이 답해야 발동·무산이 갈린다)
		if reply := miBotVoteReply(state); reply != nil {
			if !h.deliverMIBot(bot, brain, *reply, miBotVoted(state.YourSeat)) {
				return
			}
			continue
		}

		card := state.YourHand[0]
		fired, alive := brain.sleep(brain.waitFor(card, state.LastPlayed))
		if !alive {
			return
		}
		if !fired {
			continue // 스냅샷이 바뀌었다 — 간격을 다시 계산한다
		}

		// 기다린 사이에 판이 달라졌는지 확인한다 (달라졌으면 다시 계산)
		now, ok := brain.snapshot()
		if !ok || now.Phase != MIPhasePlaying || len(now.YourHand) == 0 ||
			now.YourHand[0] != card || now.LastPlayed != state.LastPlayed ||
			now.StarVote != nil {
			continue
		}

		reply, settled := MIMessage{Type: MIMsgPlay}, miBotPlayed(card)
		if now.Stars > 0 && card >= MIBotStarProposeFrom &&
			brain.rng.Float64() < miBotStarChance {
			reply, settled = MIMessage{Type: MIMsgStarPropose}, miBotProposed
		}
		if !h.deliverMIBot(bot, brain, reply, settled) {
			return
		}
	}
}

// ==================== 발사 후 정착 조건 ====================
//
// 봇이 메시지를 보낸 뒤 **그 결과가 스냅샷에 반영될 때까지** 기다리지 않으면,
// 아직 갱신되지 않은 옛 스냅샷을 보고 같은 판단을 한 번 더 내려 카드를 두 장
// 연달아 밀어낸다 (더 크루에서 봇이 협력을 못 하던 전형적인 자멸 경로다).

// miBotPlayed 방금 낸 카드가 손패에서 빠졌으면 정착
func miBotPlayed(card int) func(miBotState) bool {
	return func(s miBotState) bool {
		return s.Phase != MIPhasePlaying || len(s.YourHand) == 0 || s.YourHand[0] != card
	}
}

// miBotProposed 제안이 실제로 열렸으면(또는 판이 넘어갔으면) 정착
func miBotProposed(s miBotState) bool {
	return s.StarVote != nil || s.Phase != MIPhasePlaying
}

// miBotVoted 내 표가 반영됐거나 투표가 끝났으면 정착
func miBotVoted(seat int) func(miBotState) bool {
	return func(s miBotState) bool {
		if s.StarVote == nil {
			return true
		}
		for _, accepted := range s.StarVote.Accepted {
			if accepted == seat {
				return true
			}
		}
		return false
	}
}

// deliverMIBot 허브로 메시지를 보내고 그 결과가 스냅샷에 반영될 때까지
// 기다린다 (상한 miBotSettleWait). 살아 있으면 true.
func (h *MIHub) deliverMIBot(bot *MIClient, brain *miBrain, msg MIMessage,
	settled func(miBotState) bool) bool {
	select {
	case h.gameMessage <- MIGameMessage{Client: bot, Message: msg}:
	case <-brain.done:
		return false
	}

	deadline := time.Now().Add(miBotSettleWait)
	for {
		if s, ok := brain.snapshot(); ok && settled(s) {
			return true
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return true // 반영이 안 왔다 — 거절당했을 수 있으니 다시 판단한다
		}
		if _, alive := brain.sleep(remaining); !alive {
			return false
		}
	}
}

// miRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록용)
func miRoomHasBot(room *miRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
