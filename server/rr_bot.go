package server

import (
	"encoding/json"
	"math/rand"
	"sync"
	"time"
)

// ==================== 리코셰 로봇 연습봇 ====================
//
// 차례가 없는 게임이라 봇의 구조가 다른 게임과 다르다. 다른 게임의 봇은
// 스냅샷을 받을 때만 반응하는 순수 반응형(runBot)이지만, 리코셰 로봇에서는
// 아무도 외치지 않으면 스냅샷 자체가 오지 않아 전원이 서로를 기다리며
// 교착한다 (목표 상한 5분이 지나야 겨우 넘어간다). 그래서 세트(se_bot)·
// 더 마인드(mi_bot)와 같은 **두 고루틴** 구조를 쓴다.
//
//   - 수신 고루틴: 스냅샷을 받아 최신 것으로 갈아 끼우고 변경을 알린다
//     (판단하지 않는다). rr_game_over / rr_session_expired 를 보면 done 을
//     닫아 둘 다 끝낸다.
//   - 시계 고루틴: 스스로 깨어나 "지금이다" 판단을 내린다. 판이 멈추지 않는
//     유일한 근거다.
//
// 은닉이 없어 봇도 사람과 완전히 같은 정보(벽·로봇·목표)를 본다. 봇은 그
// 스냅샷으로 rrSolve 를 직접 돌려 최소 횟수를 구한다 — 서버가 정답을 따로
// 흘려주지 않는다.
//
// **봇이 늘 최소 횟수를 즉시 외치면 사람이 이길 여지가 없다.** 그래서 둘을
// 둔다.
//
//  1. 뜸 들이기 — 최소 횟수에 비례한 대기 + 지터. 어려운 퍼즐일수록 오래
//     걸리는 사람의 감각을 그대로 옮겼다.
//  2. 하한(floor) — 이번 목표에 자기가 내려갈 수 있는 가장 적은 횟수를
//     `최소 + n` 으로 미리 뽑아 둔다. 봇마다 다르고, 남이 더 적게 외쳐도
//     하한 아래로는 절대 내려가지 않는다. 사람이 최소 횟수를 먼저 외치면
//     그대로 이긴다.

// 봇 시간 상수 (테스트 init 에서 짧게 낮춘다)
var (
	// rrBotThinkBase / rrBotThinkPerMove 뜸 들이는 시간 =
	// base + 최소 횟수 × perMove. 봇 난이도의 핵심 계수다.
	rrBotThinkBase    = 2500 * time.Millisecond
	rrBotThinkPerMove = 1800 * time.Millisecond

	// rrBotJitterRatio 대기 시간에 섞는 상대 오차 (±비율)
	rrBotJitterRatio = 0.35

	// rrBotMinWait 대기 시간의 하한
	rrBotMinWait = 200 * time.Millisecond

	// rrBotImproveDelay 남이 더 적게 외쳤을 때 되받아치기까지 뜸 들이는 시간
	rrBotImproveDelay = 4 * time.Second

	// rrBotDemoDelay 증명을 제출하기 전의 짧은 뜸 (프론트 연출용)
	rrBotDemoDelay = 800 * time.Millisecond

	// rrBotIdleTick 아무 할 일이 없을 때 다시 살펴보는 주기
	rrBotIdleTick = 250 * time.Millisecond

	// rrBotSettleWait 메시지를 보낸 뒤 새 스냅샷을 기다리는 상한
	rrBotSettleWait = 2 * time.Second
)

// rrBotMaxImprove 한 목표에 되받아칠 수 있는 최대 횟수 (핑퐁 방지)
const rrBotMaxImprove = 2

// rrBotFloorWeights 이번 목표의 하한을 "최소 횟수 + n" 으로 뽑는 가중치
// (n = 0,1,2,3). 0 이 나온 봇만 최소 횟수까지 내려간다.
//
// 이 배열이 봇 난이도의 손잡이다 — 앞쪽 값을 키우면 봇이 세지고 사람이
// 이기기 어려워진다. rr_ws_test.go 의 TestRRBotQuality 가 그 결과를 잰다.
var rrBotFloorWeights = [4]float64{0.40, 0.35, 0.20, 0.05}

// rrBotState 봇이 스냅샷에서 꺼내 쓰는 정보. 전부 공개 정보다 —
// 은닉이 없는 게임이라 봇이 볼 수 없는 필드가 아예 없다.
type rrBotState struct {
	Phase     RRPhase            `json:"phase"`
	YourSeat  int                `json:"yourSeat"`
	GoalIndex int                `json:"goalIndex"`
	Walls     [][]int            `json:"walls"`
	Robots    map[RRColor]RRCell `json:"robots"`
	Goal      RRGoal             `json:"goal"`
	Bids      []RRBidView        `json:"bids"`
	DemoSeat  int                `json:"demoSeat"`
}

// myBid 내가 외친 횟수 (아직 안 외쳤으면 0)
func (s rrBotState) myBid() int {
	for _, b := range s.Bids {
		if b.Seat == s.YourSeat {
			return b.Moves
		}
	}
	return 0
}

// lowestOther 남이 외친 것 중 가장 적은 횟수 (없으면 0)
func (s rrBotState) lowestOther() int {
	low := 0
	for _, b := range s.Bids {
		if b.Seat == s.YourSeat {
			continue
		}
		if low == 0 || b.Moves < low {
			low = b.Moves
		}
	}
	return low
}

// rrBoardFromWalls 스냅샷의 벽 격자를 판으로 되돌린다.
// 중앙 2×2 진입 불가는 규칙으로 고정이라 와이어에 싣지 않고 여기서 채운다.
func rrBoardFromWalls(walls [][]int) *RRBoard {
	if len(walls) != RRSize {
		return nil
	}
	b := &RRBoard{}
	for r := 0; r < RRSize; r++ {
		if len(walls[r]) != RRSize {
			return nil
		}
		for c := 0; c < RRSize; c++ {
			b.Walls[rrIndex(r, c)] = uint8(walls[r][c])
		}
	}
	for _, pos := range rrCenterCells() {
		b.Blocked[pos] = true
	}
	return b
}

// rrRobotsFromView 스냅샷의 로봇 위치를 색 인덱스 배열로
func rrRobotsFromView(view map[RRColor]RRCell) ([RRRobotCount]uint8, bool) {
	var out [RRRobotCount]uint8
	for i, color := range rrColors {
		cell, ok := view[color]
		if !ok || cell.R < 0 || cell.R >= RRSize || cell.C < 0 || cell.C >= RRSize {
			return out, false
		}
		out[i] = uint8(rrIndex(cell.R, cell.C))
	}
	return out, true
}

// rrBrain 스냅샷 보관소 + 판단 (봇 대체 좌석도 같은 두뇌를 쓴다).
// state 는 수신 고루틴이 쓰고 시계 고루틴이 읽으므로 mu 로 지킨다.
// rng 와 plan* 필드는 시계 고루틴에서만 쓴다.
type rrBrain struct {
	rng *rand.Rand

	mu    sync.Mutex
	state rrBotState
	seen  bool

	// changed 스냅샷 갱신 신호 (버퍼 1 — 시계 고루틴이 기다리는 중이면 깨운다)
	changed chan struct{}

	done     chan struct{}
	stopOnce sync.Once

	// ---- 아래는 시계 고루틴 전용 (락 불필요) ----

	// planKey 계획을 세운 판의 지문 (목표 번호 + 로봇 배치 + 목표 지점)
	planKey string
	// planMoves BFS가 찾은 최소 경로 (못 풀었으면 nil)
	planMoves []RRMove
	// planMin 최소 횟수, planFloor 이번 목표에 내려갈 수 있는 하한
	planMin   int
	planFloor int
	// planBid 내가 외친 횟수 (0 = 아직), improves 되받아친 횟수
	planBid  int
	improves int
}

func newRRBrain() *rrBrain {
	return &rrBrain{
		rng:     rand.New(rand.NewSource(time.Now().UnixNano())),
		changed: make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
}

// observe 최신 스냅샷으로 갈아 끼우고 시계 고루틴을 깨운다 (수신 고루틴)
func (b *rrBrain) observe(s rrBotState) {
	b.mu.Lock()
	b.state, b.seen = s, true
	b.mu.Unlock()
	select {
	case b.changed <- struct{}{}:
	default:
	}
}

// snapshot 마지막 스냅샷 (시계 고루틴). 아직 하나도 못 받았으면 ok=false
func (b *rrBrain) snapshot() (rrBotState, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state, b.seen
}

// stop 두 고루틴을 함께 끝낸다 (중복 호출 안전)
func (b *rrBrain) stop() {
	b.stopOnce.Do(func() { close(b.done) })
}

// sleep d 만큼 기다리되 스냅샷이 갱신되면 일찍 깬다.
// fired=true 는 끝까지 기다렸다는 뜻, alive=false 는 종료다.
func (b *rrBrain) sleep(d time.Duration) (fired, alive bool) {
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

// jitter 대기 시간에 상대 오차를 섞는다
func (b *rrBrain) jitter(d time.Duration) time.Duration {
	ms := float64(d) * (1 + (b.rng.Float64()*2-1)*rrBotJitterRatio)
	out := time.Duration(ms)
	if out < rrBotMinWait {
		out = rrBotMinWait
	}
	return out
}

// thinkWait 뜸 들이는 시간 — 최소 횟수가 클수록 오래 걸린다.
// 사람이 어려운 퍼즐에 더 오래 매달리는 감각을 그대로 옮겼다.
func (b *rrBrain) thinkWait(min int) time.Duration {
	return b.jitter(rrBotThinkBase + time.Duration(min)*rrBotThinkPerMove)
}

// planFor 이번 목표의 계획을 세운다 (이미 세웠으면 그대로 둔다).
// 판이 바뀌면(목표 번호·로봇 배치·목표 지점) 새로 푼다.
func (b *rrBrain) planFor(s rrBotState) {
	robots, ok := rrRobotsFromView(s.Robots)
	if !ok {
		return
	}
	key := rrPlanKey(s.GoalIndex, robots, s.Goal)
	if b.planKey == key {
		return
	}
	b.planKey = key
	b.planBid = 0
	b.improves = 0
	b.planMoves = nil
	b.planMin = 0

	board := rrBoardFromWalls(s.Walls)
	if board == nil {
		return
	}
	moves, solved := rrSolve(board, robots, s.Goal, RRMaxDepth)
	if !solved || len(moves) == 0 {
		return // 못 풀었다 — 이번 목표는 외치지 않는다
	}
	b.planMoves = moves
	b.planMin = len(moves)
	b.planFloor = b.planMin + rrBotPickFloor(b.rng)
}

// rrPlanKey 계획 지문 — 같은 판이면 다시 풀지 않기 위한 키
func rrPlanKey(goalIndex int, robots [RRRobotCount]uint8, goal RRGoal) string {
	buf := make([]byte, 0, 16)
	buf = append(buf, byte(goalIndex))
	for _, p := range robots {
		buf = append(buf, p)
	}
	buf = append(buf, byte(rrColorIndex(goal.Color)+1), byte(goal.R), byte(goal.C))
	return string(buf)
}

// rrBotPickFloor 하한 가산치를 가중치대로 뽑는다 (0~3)
func rrBotPickFloor(rng *rand.Rand) int {
	roll, acc := rng.Float64(), 0.0
	for n, w := range rrBotFloorWeights {
		acc += w
		if roll < acc {
			return n
		}
	}
	return len(rrBotFloorWeights) - 1
}

// ==================== 봇 소환 ====================

// spawnRRBot 대기실의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *RRHub) spawnRRBot(room *rrRoom, name string) bool {
	bot := &RRClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runRRBot(bot)
	return true
}

// takeoverRRBot 유예 만료 좌석을 이어받는 연습봇 — 이름·좌석·점수를 유지한다.
// 이어받은 좌석에 이미 걸린 외침이 있으면 그 횟수 안에서 증명한다.
func (h *RRHub) takeoverRRBot(room *rrRoom, seat int, name string) *RRClient {
	bot := &RRClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	bot.GameID = room.Game.ID
	bot.Seat = seat
	h.runRRBot(bot)
	return bot
}

// runRRBot 수신 고루틴과 시계 고루틴을 함께 띄운다.
// 게임 종료·세션 만료 신호를 받으면 둘 다 스스로 끝난다 (고루틴 누수 방지).
func (h *RRHub) runRRBot(bot *RRClient) {
	brain := newRRBrain()

	go func() {
		defer brain.stop()
		for data := range bot.Send {
			var msg RRMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			switch msg.Type {
			case RRMsgGameOver, RRMsgSessionExpired:
				return
			case RRMsgGameState:
				if state, ok := botPayloadAs[rrBotState](msg.Payload); ok {
					brain.observe(state)
				}
			}
		}
	}()

	go h.driveRRBot(bot, brain)
}

// driveRRBot 시계 고루틴 — 이 게임에서 판이 멈추지 않는 유일한 근거다.
func (h *RRHub) driveRRBot(bot *RRClient, brain *rrBrain) {
	for {
		state, ok := brain.snapshot()
		if !ok || state.YourSeat < 0 {
			if _, alive := brain.sleep(rrBotIdleTick); !alive {
				return
			}
			continue
		}

		switch state.Phase {
		case RRPhaseThinking, RRPhaseBidding:
			if !h.rrBotBidStep(bot, brain, state) {
				return
			}
		case RRPhaseDemo:
			if !h.rrBotDemoStep(bot, brain, state) {
				return
			}
		default:
			if _, alive := brain.sleep(rrBotIdleTick); !alive {
				return
			}
		}
	}
}

// rrBotBidStep 외치기 단계 한 걸음. 살아 있으면 true.
func (h *RRHub) rrBotBidStep(bot *RRClient, brain *rrBrain, state rrBotState) bool {
	brain.planFor(state)

	// 못 푼 목표는 외치지 않는다 (사람이 가져가게 둔다)
	if brain.planMoves == nil {
		_, alive := brain.sleep(rrBotIdleTick)
		return alive
	}

	// 아직 안 외쳤다 — 뜸을 들이고 하한+α 로 외친다
	if brain.planBid == 0 {
		fired, alive := brain.sleep(brain.thinkWait(brain.planMin))
		if !alive {
			return false
		}
		if !fired {
			return true // 스냅샷이 바뀌었다 — 다시 판단한다
		}
		now, ok := brain.snapshot()
		if !ok || (now.Phase != RRPhaseThinking && now.Phase != RRPhaseBidding) ||
			now.GoalIndex != state.GoalIndex || now.myBid() != 0 {
			return true
		}
		// 하한 그대로이거나 한 번 더 얹어서 — 늘 최소를 부르지 않게 하는 장치
		bid := brain.planFloor + brain.rng.Intn(2)
		brain.planBid = bid
		return h.deliverRRBot(bot, brain,
			RRMessage{Type: RRMsgBid, Payload: RRBidPayload{Moves: bid}},
			rrBotBidSettled(state.YourSeat, bid, state.GoalIndex))
	}

	// 남이 더 적게 외쳤다 — 하한까지만 되받아친다
	low := state.lowestOther()
	if low > 0 && low < brain.planBid && brain.improves < rrBotMaxImprove {
		next := low - 1
		if next < brain.planFloor {
			next = brain.planFloor
		}
		if next < brain.planBid {
			fired, alive := brain.sleep(brain.jitter(rrBotImproveDelay))
			if !alive {
				return false
			}
			if !fired {
				return true
			}
			now, ok := brain.snapshot()
			if !ok || (now.Phase != RRPhaseThinking && now.Phase != RRPhaseBidding) ||
				now.GoalIndex != state.GoalIndex || now.myBid() != brain.planBid {
				return true
			}
			brain.planBid = next
			brain.improves++
			return h.deliverRRBot(bot, brain,
				RRMessage{Type: RRMsgBid, Payload: RRBidPayload{Moves: next}},
				rrBotBidSettled(state.YourSeat, next, state.GoalIndex))
		}
	}

	_, alive := brain.sleep(rrBotIdleTick)
	return alive
}

// rrBotDemoStep 증명 단계 한 걸음. 내 차례가 아니면 기다린다.
// BFS 경로는 최소 횟수라 자기가 외친 횟수 이하이므로 항상 성공한다 —
// 못 풀었거나 남의 외침을 이어받아 경로가 더 길면 포기한다.
func (h *RRHub) rrBotDemoStep(bot *RRClient, brain *rrBrain, state rrBotState) bool {
	if state.DemoSeat != state.YourSeat {
		_, alive := brain.sleep(rrBotIdleTick)
		return alive
	}

	brain.planFor(state)

	fired, alive := brain.sleep(brain.jitter(rrBotDemoDelay))
	if !alive {
		return false
	}
	if !fired {
		return true
	}
	now, ok := brain.snapshot()
	if !ok || now.Phase != RRPhaseDemo || now.DemoSeat != now.YourSeat {
		return true
	}

	bid := now.myBid()
	if brain.planMoves == nil || len(brain.planMoves) > bid {
		return h.deliverRRBot(bot, brain, RRMessage{Type: RRMsgPass},
			rrBotDemoSettled(now.YourSeat, now.GoalIndex))
	}
	return h.deliverRRBot(bot, brain,
		RRMessage{Type: RRMsgDemo, Payload: RRDemoPayload{
			Moves: append([]RRMove{}, brain.planMoves...)}},
		rrBotDemoSettled(now.YourSeat, now.GoalIndex))
}

// ==================== 발사 후 정착 조건 ====================
//
// 봇이 메시지를 보낸 뒤 **그 결과가 스냅샷에 반영될 때까지** 기다리지 않으면,
// 아직 갱신되지 않은 옛 스냅샷을 보고 같은 판단을 한 번 더 내려 같은 외침을
// 두 번 보낸다 (두 번째는 "더 적은 횟수로만" 에러로 튕기지만, 그 사이 판단이
// 꼬인다).

// rrBotBidSettled 내 외침이 그 값으로 반영됐으면(또는 판이 넘어갔으면) 정착
func rrBotBidSettled(seat, moves, goalIndex int) func(rrBotState) bool {
	return func(s rrBotState) bool {
		if s.GoalIndex != goalIndex || s.Phase == RRPhaseDemo ||
			s.Phase == RRPhaseGoalEnd || s.Phase == RRPhaseGameOver {
			return true
		}
		for _, b := range s.Bids {
			if b.Seat == seat {
				return b.Moves <= moves
			}
		}
		return false
	}
}

// rrBotDemoSettled 증명권이 넘어갔거나(성공·실패 모두) 판이 넘어갔으면 정착
func rrBotDemoSettled(seat, goalIndex int) func(rrBotState) bool {
	return func(s rrBotState) bool {
		return s.GoalIndex != goalIndex || s.Phase != RRPhaseDemo || s.DemoSeat != seat
	}
}

// deliverRRBot 허브로 메시지를 보내고 그 결과가 스냅샷에 반영될 때까지
// 기다린다 (상한 rrBotSettleWait). 살아 있으면 true.
func (h *RRHub) deliverRRBot(bot *RRClient, brain *rrBrain, msg RRMessage,
	settled func(rrBotState) bool) bool {
	select {
	case h.gameMessage <- RRGameMessage{Client: bot, Message: msg}:
	case <-brain.done:
		return false
	}

	deadline := time.Now().Add(rrBotSettleWait)
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

// rrRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록용)
func rrRoomHasBot(room *rrRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
