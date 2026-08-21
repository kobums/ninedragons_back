package server

import (
	"fmt"
	"math/rand"
	"time"
)

// ==================== 쿠 연습봇 ====================
//
// 스냅샷(cp_game_state)만 보고 반응한다. 봇도 사람과 같은 조건 — 자기
// 비공개 카드(yourCards)만 알고 남의 카드는 모른다.
//   - 액션: 칩 7+면 쿠(최다 카드, 동수면 최다 칩 대상), 아니면
//     수입 60% / 세금 25% (블러핑 포함) / 해외원조 15%
//   - 도전(액션·차단 모두): 15%
//   - 차단: 실제 차단 역할 보유 시 80%, 미보유 블러핑 10%, 그 외 허용
//   - 카드 제거·교환 유지는 무작위
// 같은 창에 스냅샷이 여러 번 와도(허용 쌓임·관전 입장 등) 한 번만 응답하도록
// 대기 상태 식별키(phase+endsAt+loseSeat)로 중복을 걸러낸다.

// cpBotPlayerView 봇이 스냅샷에서 꺼내 쓰는 좌석 정보
type cpBotPlayerView struct {
	Seat      int  `json:"seat"`
	Alive     bool `json:"alive"`
	Chips     int  `json:"chips"`
	CardCount int  `json:"cardCount"`
}

// cpBotPending 봇이 참조하는 진행 중 액션 요약
type cpBotPending struct {
	Kind        string `json:"kind"`
	ActorSeat   int    `json:"actorSeat"`
	TargetSeat  int    `json:"targetSeat"`
	BlockerSeat int    `json:"blockerSeat"`
	BlockRole   string `json:"blockRole"`
}

// cpBotState 봇이 상태 스냅샷에서 꺼내 쓰는 최소 정보
type cpBotState struct {
	YourSeat     int               `json:"yourSeat"`
	Phase        CPPhase           `json:"phase"`
	CurrentSeat  int               `json:"currentSeat"`
	EndsAt       int64             `json:"endsAt"`
	LoseSeat     int               `json:"loseSeat"`
	Pending      *cpBotPending     `json:"pending"`
	YourCards    []string          `json:"yourCards"`
	YourExchange []string          `json:"yourExchange"`
	Players      []cpBotPlayerView `json:"players"`
}

// cpBrain 스냅샷 기반 판단 (봇 대체 좌석도 같은 두뇌를 쓴다)
type cpBrain struct {
	rng *rand.Rand
	// lastKey 마지막으로 응답한 대기 상태 식별키 (중복 응답 방지)
	lastKey string
}

func newCPBrain() *cpBrain {
	return &cpBrain{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

// decide 공용 러너 계약 — cp_game_state 에만 반응한다
func (b *cpBrain) decide(msg CPMessage) *CPMessage {
	if msg.Type != CPMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[cpBotState](msg.Payload)
	if !ok {
		return nil
	}
	return b.decideState(state)
}

// handled 같은 대기 상태에 이미 응답했는지 — 처음이면 키를 기록한다
func (b *cpBrain) handled(key string) bool {
	if b.lastKey == key {
		return true
	}
	b.lastKey = key
	return false
}

func (b *cpBrain) decideState(s cpBotState) *CPMessage {
	me := s.YourSeat
	if me < 0 || me >= len(s.Players) || !s.Players[me].Alive {
		return nil
	}
	key := fmt.Sprintf("%s|%d|%d", s.Phase, s.EndsAt, s.LoseSeat)

	switch s.Phase {
	case CPPhaseAction:
		if s.CurrentSeat != me || b.handled(key) {
			return nil
		}
		return b.chooseAction(s)

	case CPPhaseChallengeWindow:
		if s.Pending == nil || s.Pending.ActorSeat == me || b.handled(key) {
			return nil
		}
		if b.rng.Float64() < 0.15 {
			return &CPMessage{Type: CPMsgChallenge}
		}
		return &CPMessage{Type: CPMsgAllow}

	case CPPhaseBlockWindow:
		if s.Pending == nil || s.Pending.ActorSeat == me || b.handled(key) {
			return nil
		}
		if m := b.chooseBlock(s); m != nil {
			return m
		}
		return &CPMessage{Type: CPMsgAllow}

	case CPPhaseBlockChallengeWindow:
		if s.Pending == nil || s.Pending.BlockerSeat == me || b.handled(key) {
			return nil
		}
		if b.rng.Float64() < 0.15 {
			return &CPMessage{Type: CPMsgChallenge}
		}
		return &CPMessage{Type: CPMsgAllow}

	case CPPhaseLoseCard:
		if s.LoseSeat != me || len(s.YourCards) == 0 || b.handled(key) {
			return nil
		}
		return &CPMessage{Type: CPMsgLose,
			Payload: CPLosePayload{Index: b.rng.Intn(len(s.YourCards))}}

	case CPPhaseExchangePick:
		if s.Pending == nil || s.Pending.ActorSeat != me || len(s.YourExchange) == 0 || b.handled(key) {
			return nil
		}
		keep := s.Players[me].CardCount
		if keep <= 0 || keep > len(s.YourExchange) {
			return nil
		}
		return &CPMessage{Type: CPMsgExchangeKeep,
			Payload: CPExchangeKeepPayload{Indices: b.rng.Perm(len(s.YourExchange))[:keep]}}
	}
	return nil
}

// chooseAction 자기 차례의 액션 선택
func (b *cpBrain) chooseAction(s cpBotState) *CPMessage {
	me := s.YourSeat
	my := s.Players[me]
	if my.Chips >= CPCoupCost {
		target := -1
		for _, p := range s.Players {
			if p.Seat == me || !p.Alive {
				continue
			}
			if target < 0 {
				target = p.Seat
				continue
			}
			t := s.Players[target]
			if p.CardCount > t.CardCount || (p.CardCount == t.CardCount && p.Chips > t.Chips) {
				target = p.Seat
			}
		}
		if target >= 0 {
			ts := target
			return &CPMessage{Type: CPMsgAction,
				Payload: CPActionPayload{Kind: string(CPActCoup), TargetSeat: &ts}}
		}
	}
	kind := CPActIncome
	switch r := b.rng.Float64(); {
	case r < 0.6:
		kind = CPActIncome
	case r < 0.85:
		kind = CPActTax // 공작 미보유 블러핑 포함
	default:
		kind = CPActAid
	}
	return &CPMessage{Type: CPMsgAction, Payload: CPActionPayload{Kind: string(kind)}}
}

// chooseBlock 차단 창 — 차단할 수 없거나 차단하지 않기로 하면 nil (→허용)
func (b *cpBrain) chooseBlock(s cpBotState) *CPMessage {
	me := s.YourSeat
	p := s.Pending
	var candidates []string
	switch CPActionKind(p.Kind) {
	case CPActAid:
		candidates = []string{string(CPRoleDuke)}
	case CPActAssassinate:
		if p.TargetSeat != me {
			return nil
		}
		candidates = []string{string(CPRoleContessa)}
	case CPActSteal:
		if p.TargetSeat != me {
			return nil
		}
		candidates = []string{string(CPRoleCaptain), string(CPRoleAmbassador)}
	default:
		return nil
	}
	holding := ""
	for _, cand := range candidates {
		for _, mine := range s.YourCards {
			if mine == cand {
				holding = cand
			}
		}
	}
	if holding != "" {
		if b.rng.Float64() < 0.8 {
			return &CPMessage{Type: CPMsgBlock, Payload: CPBlockPayload{Role: holding}}
		}
		return nil
	}
	if b.rng.Float64() < 0.1 { // 블러핑 차단
		return &CPMessage{Type: CPMsgBlock, Payload: CPBlockPayload{Role: candidates[0]}}
	}
	return nil
}

// ==================== 봇 소환 ====================

// spawnCPBot 대기실의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *CPHub) spawnCPBot(room *cpRoom, name string) bool {
	bot := &CPClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runCPBot(bot)
	return true
}

// takeoverCPBot 유예 만료 좌석을 이어받는 연습봇 — 이름·좌석을 유지해
// 진행 중인 창 허용·카드 제거가 그대로 이어진다
func (h *CPHub) takeoverCPBot(room *cpRoom, seat int, name string) *CPClient {
	bot := &CPClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	bot.GameID = room.Game.ID
	bot.Seat = seat
	h.runCPBot(bot)
	return bot
}

// runCPBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *CPHub) runCPBot(bot *CPClient) {
	brain := newCPBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m CPMessage) { h.gameMessage <- CPGameMessage{Client: bot, Message: m} },
		func(m CPMessage) bool { return m.Type == CPMsgGameOver || m.Type == CPMsgSessionExpired })
}

// cpRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록용)
func cpRoomHasBot(room *cpRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
