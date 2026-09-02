package server

import (
	"sort"
	"time"
)

// 좌석 방(room.Clients: 좌석 → 클라이언트)을 다루는 공용 헬퍼.
// 각 허브의 hostSeat / xxHumanCount / stopPhaseTimer / clearGameSessions 가
// 프리픽스만 다른 같은 본문이라 여기로 모았다. 허브 쪽 메서드는 이름을 남기고
// 본문만 위임한다 — 호출부 400여 곳은 무수정. 허브 고루틴에서만 호출한다.

// seatClient 좌석 클라이언트 포인터의 최소 계약. 각 XXClient 가 wsClient 를
// 임베드하므로 *XXClient 가 그대로 만족한다.
type seatClient[T any] interface {
	*T
	IsConnected() bool
	IsBot() bool
}

// hostSeatOf 접속 중인 사람 좌석 중 가장 낮은 번호 — 시작·봇 채움 권한의
// 주인. 사람이 아무도 없으면 -1.
func hostSeatOf[T any, PT seatClient[T]](clients map[int]PT) int {
	seats := []int{}
	for seat, c := range clients {
		if c != nil && c.IsConnected() && !c.IsBot() {
			seats = append(seats, seat)
		}
	}
	if len(seats) == 0 {
		return -1
	}
	sort.Ints(seats)
	return seats[0]
}

// humanCountOf 봇이 아닌 좌석 수. 끊긴 사람도 센다 (유예 중이라 자리는 남는다).
func humanCountOf[K comparable, T any, PT seatClient[T]](clients map[K]PT) int {
	n := 0
	for _, c := range clients {
		if c != nil && !c.IsBot() {
			n++
		}
	}
	return n
}

// stopTimer 대기 상태 마감 타이머를 멈추고 비운다 (이미 없으면 아무것도 안 한다)
func stopTimer(timer **time.Timer) {
	if *timer != nil {
		(*timer).Stop()
		*timer = nil
	}
}

// clearRoomSessions 방의 모든 좌석 세션을 장부에서 지운다 (게임 종료 정리).
// 세션과 유예 타이머를 함께 지우므로 끊긴 채 끝난 좌석의 타이머도 정리된다.
func clearRoomSessions[K comparable, C sessionClient](s *sessionManager[C], clients map[K]C) {
	var none C
	for _, c := range clients {
		if c == none {
			continue
		}
		s.drop(c.SessionKey())
	}
}
