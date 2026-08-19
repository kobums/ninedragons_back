package server

import (
	"math/rand"
	"strings"
)

// ==================== 방 코드 (사설 로비) ====================
//
// 친구끼리만 모이는 사설 방의 초대 코드. join payload 의 선택 필드 room 으로
// 들어오며, 생략하면 기존 공용 로비 그대로다 (하위 호환). 다인 5종 허브
// (dv/tc/mt/sf/sp)가 이 헬퍼를 재사용한다.

const (
	// roomCodeAlphabet 대문자·숫자 — 혼동 문자 I/O/0/1 제외 (프론트와 공유)
	roomCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	roomCodeLen      = 4

	// roomCodeNew join payload 의 예약어 — 서버가 코드를 발급해 새 사설 방을
	// 만든다. 3자라 실제 코드(4자)와 충돌하지 않는다.
	roomCodeNew = "NEW"
)

// normalizeRoomCode 입력 코드 정규화 — 앞뒤 공백 제거·대문자화.
// 대소문자 무시 입력의 근거다. 빈 문자열은 공용 로비를 뜻한다.
func normalizeRoomCode(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}

// generateRoomCode 미사용 4자 코드 생성. taken 은 현재 사용 중인 코드 집합.
// 32^4(백만+) 조합이라 충돌 재시도는 사실상 즉시 끝난다.
// 각 허브의 고루틴에서 해당 허브의 rng 로만 호출한다 (동시성 규칙 유지).
func generateRoomCode(rng *rand.Rand, taken map[string]bool) string {
	for {
		b := make([]byte, roomCodeLen)
		for i := range b {
			b[i] = roomCodeAlphabet[rng.Intn(len(roomCodeAlphabet))]
		}
		if code := string(b); !taken[code] {
			return code
		}
	}
}

// takenCodes privateLobbies(code → room)의 키 집합 — generateRoomCode 입력용
func takenCodes[T any](lobbies map[string]T) map[string]bool {
	taken := make(map[string]bool, len(lobbies))
	for code := range lobbies {
		taken[code] = true
	}
	return taken
}
