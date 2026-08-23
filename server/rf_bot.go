package server

import (
	"fmt"
	"math/rand"
	"time"
)

// ==================== 리포메이션 연습봇 ====================
//
// 스냅샷(rf_game_state)만 보고 반응한다. 봇도 사람과 같은 조건 — 자기
// 비공개 카드(yourRoles)만 알고 남의 카드는 모른다. 진영·국고는 공개
// 정보라 확장 판단에 그대로 쓴다.
//
// 기본 두뇌(쿠와 동일):
//   - 액션: 칩 7+면 쿠(최다 카드, 동수면 최다 칩 대상), 아니면
//     수입 60% / 세금 25% (블러핑 포함) / 해외원조 15%
//   - 도전(액션·차단 모두): 15%
//   - 차단: 실제 차단 역할 보유 시 80%, 미보유 블러핑 10%, 그 외 통과
//   - 카드 제거·교환 유지는 무작위
//
// 리포메이션 확장 판단:
//   - 같은 진영은 공격 대상에서 자동 제외한다 (서버도 막지만 헛수를 두지 않는다)
//   - 국고가 3개 이상 쌓이고 손에 공작이 없으면 40% 확률로 횡령
//   - 자기 진영이 수적으로 불리하면 20% 확률로 남을 개종 (칩 2개 필요)
//
// 같은 창에 스냅샷이 여러 번 와도(통과 쌓임·관전 입장 등) 한 번만 응답하도록
// 대기 상태 식별키(phase+endsAt+loseSeat)로 중복을 걸러낸다.

const (
	// rfBotEmbezzleFloor 봇이 횡령을 노리기 시작하는 국고 액수
	rfBotEmbezzleFloor = 3
	// rfBotEmbezzleRate 조건 충족 시 횡령 확률
	rfBotEmbezzleRate = 0.4
	// rfBotConvertRate 진영이 불리할 때 남을 개종할 확률
	rfBotConvertRate = 0.2
	// rfBotChallengeRate 도전 확률 (액션·차단 공용)
	rfBotChallengeRate = 0.15
)

// rfBotPlayerView 봇이 스냅샷에서 꺼내 쓰는 좌석 정보
type rfBotPlayerView struct {
	Seat      int       `json:"seat"`
	Alive     bool      `json:"alive"`
	Coins     int       `json:"coins"`
	Faction   RFFaction `json:"faction"`
	CardCount int       `json:"cardCount"`
}

// rfBotPending 봇이 참조하는 진행 중 액션 요약
type rfBotPending struct {
	Kind        string `json:"kind"`
	BySeat      int    `json:"bySeat"`
	TargetSeat  int    `json:"targetSeat"`
	BlockerSeat int    `json:"blockerSeat"`
	BlockRole   string `json:"blockRole"`
}

// rfBotState 봇이 상태 스냅샷에서 꺼내 쓰는 최소 정보
type rfBotState struct {
	YourSeat     int               `json:"yourSeat"`
	Phase        RFPhase           `json:"phase"`
	CurrentSeat  int               `json:"currentSeat"`
	EndsAt       int64             `json:"endsAt"`
	LoseSeat     int               `json:"loseSeat"`
	Treasury     int               `json:"treasury"`
	Pending      *rfBotPending     `json:"pending"`
	YourRoles    []string          `json:"yourRoles"`
	YourExchange []string          `json:"yourExchange"`
	Players      []rfBotPlayerView `json:"players"`
}

// rfBrain 스냅샷 기반 판단 (봇 대체 좌석도 같은 두뇌를 쓴다)
type rfBrain struct {
	rng *rand.Rand
	// lastKey 마지막으로 응답한 대기 상태 식별키 (중복 응답 방지)
	lastKey string
}

func newRFBrain() *rfBrain {
	return &rfBrain{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

// decide 공용 러너 계약 — rf_game_state 에만 반응한다
func (b *rfBrain) decide(msg RFMessage) *RFMessage {
	if msg.Type != RFMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[rfBotState](msg.Payload)
	if !ok {
		return nil
	}
	return b.decideState(state)
}

// handled 같은 대기 상태에 이미 응답했는지 — 처음이면 키를 기록한다
func (b *rfBrain) handled(key string) bool {
	if b.lastKey == key {
		return true
	}
	b.lastKey = key
	return false
}

func (b *rfBrain) decideState(s rfBotState) *RFMessage {
	me := s.YourSeat
	if me < 0 || me >= len(s.Players) || !s.Players[me].Alive {
		return nil
	}
	key := fmt.Sprintf("%s|%d|%d", s.Phase, s.EndsAt, s.LoseSeat)

	switch s.Phase {
	case RFPhaseAction:
		if s.CurrentSeat != me || b.handled(key) {
			return nil
		}
		return b.chooseAction(s)

	case RFPhaseChallengeWindow:
		if s.Pending == nil || b.handled(key) {
			return nil
		}
		// 차단 도전 창이면 차단자가, 액션 도전 창이면 행동자가 응답 제외 대상
		if s.Pending.BlockerSeat >= 0 {
			if s.Pending.BlockerSeat == me {
				return nil
			}
		} else if s.Pending.BySeat == me {
			return nil
		}
		if b.rng.Float64() < rfBotChallengeRate {
			return &RFMessage{Type: RFMsgChallenge}
		}
		return &RFMessage{Type: RFMsgPass}

	case RFPhaseBlockWindow:
		if s.Pending == nil || s.Pending.BySeat == me || b.handled(key) {
			return nil
		}
		if m := b.chooseBlock(s); m != nil {
			return m
		}
		return &RFMessage{Type: RFMsgPass}

	case RFPhaseLoseCard:
		if s.LoseSeat != me || len(s.YourRoles) == 0 || b.handled(key) {
			return nil
		}
		return &RFMessage{Type: RFMsgLoseCard,
			Payload: RFLoseCardPayload{Index: b.rng.Intn(len(s.YourRoles))}}

	case RFPhaseExchange:
		if s.Pending == nil || s.Pending.BySeat != me || len(s.YourExchange) == 0 || b.handled(key) {
			return nil
		}
		keep := s.Players[me].CardCount
		if keep <= 0 || keep > len(s.YourExchange) {
			return nil
		}
		return &RFMessage{Type: RFMsgExchange,
			Payload: RFExchangePayload{Keep: b.rng.Perm(len(s.YourExchange))[:keep]}}
	}
	return nil
}

// rfBotEnemyTarget 같은 진영을 뺀 최우선 대상 — 최다 비공개 카드,
// 동수면 최다 칩. 공격(또는 개종)할 상대가 없으면 -1.
func rfBotEnemyTarget(s rfBotState) int {
	me := s.YourSeat
	myFaction := s.Players[me].Faction
	target := -1
	for _, p := range s.Players {
		if p.Seat == me || !p.Alive || p.Faction == myFaction {
			continue
		}
		if target < 0 {
			target = p.Seat
			continue
		}
		t := s.Players[target]
		if p.CardCount > t.CardCount || (p.CardCount == t.CardCount && p.Coins > t.Coins) {
			target = p.Seat
		}
	}
	return target
}

// rfBotFactionCounts 살아 있는 내 진영 / 상대 진영 인원
func rfBotFactionCounts(s rfBotState) (mine, theirs int) {
	myFaction := s.Players[s.YourSeat].Faction
	for _, p := range s.Players {
		if !p.Alive {
			continue
		}
		if p.Faction == myFaction {
			mine++
		} else {
			theirs++
		}
	}
	return mine, theirs
}

// chooseAction 자기 차례의 행동 선택
func (b *rfBrain) chooseAction(s rfBotState) *RFMessage {
	me := s.YourSeat
	my := s.Players[me]
	enemy := rfBotEnemyTarget(s)

	// 칩 10+ 는 쿠 강제 — 같은 진영은 대상에서 빠진다
	if my.Coins >= RFForceCoupChips {
		if enemy < 0 {
			return nil // 공격할 상대가 없다 (곧 진영 승리로 끝난다)
		}
		ts := enemy
		return &RFMessage{Type: RFMsgAction,
			Payload: RFActionPayload{Kind: string(RFActCoup), TargetSeat: &ts}}
	}

	// 국고가 쌓였고 손에 공작이 없으면 횡령 (증명 가능한 정직한 주장)
	if s.Treasury >= rfBotEmbezzleFloor && !rfBotHasRole(s.YourRoles, RFRoleDuke) &&
		b.rng.Float64() < rfBotEmbezzleRate {
		return &RFMessage{Type: RFMsgEmbezzle}
	}

	// 진영이 수적으로 불리하면 상대를 끌어온다 (칩이 부족하면 하지 않는다)
	if mine, theirs := rfBotFactionCounts(s); mine < theirs &&
		my.Coins >= RFConvertOtherCost && enemy >= 0 &&
		b.rng.Float64() < rfBotConvertRate {
		ts := enemy
		return &RFMessage{Type: RFMsgConvertOther, Payload: RFConvertOtherPayload{TargetSeat: &ts}}
	}

	if my.Coins >= RFCoupCost && enemy >= 0 {
		ts := enemy
		return &RFMessage{Type: RFMsgAction,
			Payload: RFActionPayload{Kind: string(RFActCoup), TargetSeat: &ts}}
	}

	kind := RFActIncome
	switch r := b.rng.Float64(); {
	case r < 0.6:
		kind = RFActIncome
	case r < 0.85:
		kind = RFActTax // 공작 미보유 블러핑 포함
	default:
		kind = RFActAid
	}
	return &RFMessage{Type: RFMsgAction, Payload: RFActionPayload{Kind: string(kind)}}
}

// rfBotHasRole 손패에 해당 역할이 있는지
func rfBotHasRole(hand []string, role RFRole) bool {
	for _, r := range hand {
		if r == string(role) {
			return true
		}
	}
	return false
}

// chooseBlock 차단 창 — 차단할 수 없거나 차단하지 않기로 하면 nil (→통과)
func (b *rfBrain) chooseBlock(s rfBotState) *RFMessage {
	me := s.YourSeat
	p := s.Pending
	var candidates []string
	switch RFActionKind(p.Kind) {
	case RFActAid:
		candidates = []string{string(RFRoleDuke)} // 같은 진영의 원조도 막을 수 있다
	case RFActAssassinate:
		if p.TargetSeat != me {
			return nil
		}
		candidates = []string{string(RFRoleContessa)}
	case RFActSteal:
		if p.TargetSeat != me {
			return nil
		}
		candidates = []string{string(RFRoleCaptain), string(RFRoleAmbassador)}
	default:
		return nil
	}
	holding := ""
	for _, cand := range candidates {
		for _, mine := range s.YourRoles {
			if mine == cand {
				holding = cand
			}
		}
	}
	if holding != "" {
		if b.rng.Float64() < 0.8 {
			return &RFMessage{Type: RFMsgBlock, Payload: RFBlockPayload{Role: holding}}
		}
		return nil
	}
	if b.rng.Float64() < 0.1 { // 블러핑 차단
		return &RFMessage{Type: RFMsgBlock, Payload: RFBlockPayload{Role: candidates[0]}}
	}
	return nil
}

// ==================== 봇 소환 ====================

// spawnRFBot 대기실의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *RFHub) spawnRFBot(room *rfRoom, name string) bool {
	bot := &RFClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runRFBot(bot)
	return true
}

// takeoverRFBot 유예 만료 좌석을 이어받는 연습봇 — 이름·좌석을 유지해
// 진행 중인 창 통과·카드 제거가 그대로 이어진다
func (h *RFHub) takeoverRFBot(room *rfRoom, seat int, name string) *RFClient {
	bot := &RFClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	bot.GameID = room.Game.ID
	bot.Seat = seat
	h.runRFBot(bot)
	return bot
}

// runRFBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *RFHub) runRFBot(bot *RFClient) {
	brain := newRFBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m RFMessage) { h.gameMessage <- RFGameMessage{Client: bot, Message: m} },
		func(m RFMessage) bool { return m.Type == RFMsgGameOver || m.Type == RFMsgSessionExpired })
}

// rfRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록용)
func rfRoomHasBot(room *rfRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
