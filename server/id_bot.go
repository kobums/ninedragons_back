package server

import (
	"fmt"
	"math/rand"
	"time"
)

// ==================== 인사이더 연습봇 ====================
//
// 스냅샷(id_game_state)만 보고 반응한다. 봇도 사람과 같은 조건 — 자기
// yourRole·word 만 알고 남의 역할은 모른다 (인사이더 봇도 추리 정보를 더
// 받지 않는다).
//   - 마스터 봇: 질문 타임 20~40초 뒤 [정답 나옴], 토론 뒤 [투표 시작]
//   - 투표 봇: 자기 제외 무작위 지목 (인사이더 봇도 자기 자신은 못 찍는다)
// 같은 대기 상태에 스냅샷이 여러 번 와도(관전 입장·접속 변화 등) 한 번만
// 응답하도록 상태 식별키(phase+endsAt+masterSeat)로 중복을 걸러낸다.

// 봇이 "생각하는" 단계별 시간 (테스트에서 짧게 낮춘다).
// 마스터 봇의 정답 선언은 질문 타임 20~40초 사이에 무작위로 일어난다.
var (
	idBotCorrectDelay    = 20 * time.Second
	idBotCorrectJitterMs = 20000
	idBotDiscussDelay    = 15 * time.Second
	idBotDiscussJitterMs = 10000
	idBotVoteDelay       = 1 * time.Second
	idBotVoteJitterMs    = 1500
)

// idBotPlayerView 봇이 스냅샷에서 꺼내 쓰는 좌석 정보
type idBotPlayerView struct {
	Seat  int  `json:"seat"`
	Voted bool `json:"voted"`
}

// idBotState 봇이 상태 스냅샷에서 꺼내 쓰는 최소 정보
type idBotState struct {
	YourSeat   int               `json:"yourSeat"`
	Phase      IDPhase           `json:"phase"`
	MasterSeat int               `json:"masterSeat"`
	EndsAt     int64             `json:"endsAt"`
	Players    []idBotPlayerView `json:"players"`
}

// idBrain 스냅샷 기반 판단 (봇 대체 좌석도 같은 두뇌를 쓴다)
type idBrain struct {
	rng *rand.Rand
	// lastKey 마지막으로 응답한 대기 상태 식별키 (중복 응답 방지)
	lastKey string
}

func newIDBrain() *idBrain {
	return &idBrain{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

// decide 공용 러너 계약 — id_game_state 에만 반응한다
func (b *idBrain) decide(msg IDMessage) *IDMessage {
	if msg.Type != IDMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[idBotState](msg.Payload)
	if !ok {
		return nil
	}
	return b.decideState(state)
}

// handled 같은 대기 상태에 이미 응답했는지 — 처음이면 키를 기록한다
func (b *idBrain) handled(key string) bool {
	if b.lastKey == key {
		return true
	}
	b.lastKey = key
	return false
}

// think 사람처럼 뜸을 들인다 — 마스터의 정답 선언 타이밍이 곧 게임의 리듬이라
// 지연 자체가 규칙의 일부다 (테스트에서는 var 를 낮춰 즉시 진행한다)
func (b *idBrain) think(base time.Duration, jitterMs int) {
	d := base
	if jitterMs > 0 {
		d += time.Duration(b.rng.Intn(jitterMs)) * time.Millisecond
	}
	if d > 0 {
		time.Sleep(d)
	}
}

func (b *idBrain) decideState(s idBotState) *IDMessage {
	me := s.YourSeat
	if me < 0 || me >= len(s.Players) {
		return nil
	}
	key := fmt.Sprintf("%s|%d|%d", s.Phase, s.EndsAt, s.MasterSeat)

	switch s.Phase {
	case IDPhaseQuestion:
		// 질문/답변은 음성으로 오가므로 봇은 시간만 흘려보내다 정답을 선언한다
		if s.MasterSeat != me || b.handled(key) {
			return nil
		}
		b.think(idBotCorrectDelay, idBotCorrectJitterMs)
		return &IDMessage{Type: IDMsgCorrect}

	case IDPhaseDiscussion:
		if s.MasterSeat != me || b.handled(key) {
			return nil
		}
		b.think(idBotDiscussDelay, idBotDiscussJitterMs)
		return &IDMessage{Type: IDMsgOpenVote}

	case IDPhaseVoting:
		if s.Players[me].Voted || b.handled(key) {
			return nil
		}
		b.think(idBotVoteDelay, idBotVoteJitterMs)
		return &IDMessage{Type: IDMsgVote, Payload: IDVotePayload{Seat: b.pickTarget(s)}}
	}
	return nil
}

// pickTarget 무작위 지목 — 자기 자신은 제외한다 (인사이더 봇도 마찬가지라
// 자기 표로 스스로를 노출하지 않는다)
func (b *idBrain) pickTarget(s idBotState) int {
	targets := []int{}
	for _, p := range s.Players {
		if p.Seat != s.YourSeat {
			targets = append(targets, p.Seat)
		}
	}
	if len(targets) == 0 {
		return -1
	}
	return targets[b.rng.Intn(len(targets))]
}

// ==================== 봇 소환 ====================

// spawnIDBot 대기실의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *IDHub) spawnIDBot(room *idRoom, name string) bool {
	bot := &IDClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runIDBot(bot)
	return true
}

// takeoverIDBot 유예 만료 좌석을 이어받는 연습봇 — 이름·좌석을 유지해
// 진행 중인 마스터 선언·투표가 그대로 이어진다
func (h *IDHub) takeoverIDBot(room *idRoom, seat int, name string) *IDClient {
	bot := &IDClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	bot.GameID = room.Game.ID
	bot.Seat = seat
	h.runIDBot(bot)
	return bot
}

// runIDBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *IDHub) runIDBot(bot *IDClient) {
	brain := newIDBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m IDMessage) { h.gameMessage <- IDGameMessage{Client: bot, Message: m} },
		func(m IDMessage) bool { return m.Type == IDMsgGameOver || m.Type == IDMsgSessionExpired })
}

// idRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록용)
func idRoomHasBot(room *idRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
