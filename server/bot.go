package server

import (
	"encoding/json"
	"math/rand"
	"time"

	"github.com/google/uuid"
)

// ==================== 연습봇 공용 러너 ====================
//
// 봇은 실제 WS 없이 허브에 붙는 가상 클라이언트다. 허브가 sendTo 로
// wsClient.Send 채널에 넣어주는 메시지(JSON 한 건씩)를 이 러너가 소비하고,
// 게임별 brain 이 결정한 응답을 허브의 gameMessage 채널로 되돌린다.
// 허브 고루틴은 봇을 일반 클라이언트와 똑같이 다룬다.

const botName = "연습봇"

// rematchWindow 게임 종료 후 재대결을 기다리는 시간. 창이 지나면 방·세션을
// 정리한다 (테스트에서는 짧게 낮춘다).
var rematchWindow = 60 * time.Second

// 사람처럼 잠깐 생각하는 시간 (테스트에서는 0으로 낮춘다)
var (
	botDelayBase     = 350 * time.Millisecond
	botDelayJitterMs = 450
)

// newBotWSClient 실제 소켓 없는 봇 연결. Connected=true 라 sendTo 가
// 정상 전달하고, readLoop 가 없으니 unregister 경로는 타지 않는다.
func newBotWSClient() wsClient {
	return wsClient{
		ID:        "bot-" + uuid.New().String(),
		SessionID: uuid.New().String(),
		Name:      botName,
		Send:      make(chan []byte, 256),
		Connected: true,
		Bot:       true,
	}
}

// runBot 봇 수신 루프. 게임 종료·세션 만료 신호를 받으면 스스로 끝난다
// (방이 정리된 뒤에는 메시지가 오지 않으므로 고루틴 누수 방지의 핵심).
//   - decide: 수신 메시지 하나를 보고 응답을 결정 (nil 이면 무응답)
//   - deliver: 허브 gameMessage 채널로 응답 전달
//   - isDone: 봇 종료 신호 (xx_game_over / xx_session_expired)
func runBot[M any](send chan []byte, decide func(M) *M, deliver func(M), isDone func(M) bool) {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	for data := range send {
		var msg M
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if isDone(msg) {
			return
		}
		if reply := decide(msg); reply != nil {
			delay := botDelayBase
			if botDelayJitterMs > 0 {
				delay += time.Duration(rng.Intn(botDelayJitterMs)) * time.Millisecond
			}
			if delay > 0 {
				time.Sleep(delay)
			}
			deliver(*reply)
		}
	}
}

// botPayloadAs 메시지 payload 를 게임별 상태 구조체로 변환
func botPayloadAs[T any](payload interface{}) (T, bool) {
	var out T
	raw, err := json.Marshal(payload)
	if err != nil {
		return out, false
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, false
	}
	return out, true
}
