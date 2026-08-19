package server

import "time"

// ==================== 리액션 이모지 / 관전 공용 헬퍼 ====================
//
// 다인 6종 허브(dv/tc/mt/sf/sp/av)가 공유한다.
// 리액션은 상태를 저장하지 않는 연출용 이벤트다 — 각 허브의 {prefix}_react
// 를 받아 기존 이벤트 채널({prefix}_event, kind: "react")로 되쏜다.

// reactWhitelist 허용 이모지 6종 — 그 외는 조용히 무시한다
var reactWhitelist = map[string]bool{
	"👍": true,
	"👎": true,
	"😂": true,
	"😮": true,
	"🔥": true,
	"😭": true,
}

// reactAllowed 화이트리스트 판정
func reactAllowed(emoji string) bool {
	return reactWhitelist[emoji]
}

// reactRateLimit 좌석당 리액션 최소 간격 — 초과는 조용히 무시한다
// (테스트에서 짧게 낮춘다)
var reactRateLimit = 2 * time.Second

// reactPass 좌석의 레이트리밋을 판정하고 통과 시 장부를 갱신한다.
// last 는 각 room 의 LastReact 맵 — 호출부가 nil 이 아니게 만들어 넘긴다.
func reactPass(last map[int]time.Time, seat int, now time.Time) bool {
	if t, ok := last[seat]; ok && now.Sub(t) < reactRateLimit {
		return false
	}
	last[seat] = now
	return true
}

// maxSpectators 방 하나의 관전자 상한
const maxSpectators = 8

// 관전 관련 공용 문구 — 6허브가 같은 문구를 쓴다
const (
	spectatorFullMsg   = "관전 정원이 가득 찼습니다"
	spectatorDeniedMsg = "관전자는 참여할 수 없습니다"
)
