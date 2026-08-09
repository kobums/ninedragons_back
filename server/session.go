package server

import "time"

// sessionClient 세션 장부가 다루는 클라이언트의 최소 계약
type sessionClient interface {
	comparable
	SessionKey() string
	IsConnected() bool
	CloseConn()
}

// sessionManager 세션·유예 타이머 장부. 다섯 게임 허브가 값으로 임베드하며,
// 필드 승격 덕에 기존 h.sessions / h.grace / <-h.graceExpired 접근이 그대로
// 컴파일된다. 신원 인계(이름·슬롯 복사)와 게임 슬롯 재바인딩, 유예 만료의
// 결말(게임 종료 vs 몰수)은 게임별 허브에 남는다.
type sessionManager[C sessionClient] struct {
	// 세션 ID → 현재(또는 마지막) 클라이언트
	sessions map[string]C

	// 세션 ID → 재접속 대기 타이머
	graceTimers map[string]*time.Timer

	// 재접속 대기 시간 만료 알림
	graceExpired chan string

	// 재접속 대기 시간
	grace time.Duration
}

func newSessionManager[C sessionClient]() sessionManager[C] {
	return sessionManager[C]{
		sessions:     make(map[string]C),
		graceTimers:  make(map[string]*time.Timer),
		graceExpired: make(chan string),
		grace:        defaultDisconnectGrace,
	}
}

// isCurrent 이 연결이 세션의 현재 소유자인지
// (새 연결로 교체된 뒤 늦게 도착한 옛 연결의 disconnect 를 무시하기 위함)
func (s *sessionManager[C]) isCurrent(c C) bool {
	return s.sessions[c.SessionKey()] == c
}

// drop 세션과 유예 타이머를 함께 지운다
func (s *sessionManager[C]) drop(id string) {
	s.cancelGrace(id)
	delete(s.sessions, id)
}

// startGrace 유예 타이머 시작 — 만료되면 graceExpired 채널로 통지된다
func (s *sessionManager[C]) startGrace(id string) {
	s.graceTimers[id] = time.AfterFunc(s.grace, func() {
		s.graceExpired <- id
	})
}

// cancelGrace 유예 타이머 취소
func (s *sessionManager[C]) cancelGrace(id string) {
	if t := s.graceTimers[id]; t != nil {
		t.Stop()
		delete(s.graceTimers, id)
	}
}

// expire graceExpired 수신 공통 프롤로그. 유예 안에 재접속했으면 ok=false.
// 만료가 확정되면 세션을 지우고 대상 클라이언트를 돌려준다.
func (s *sessionManager[C]) expire(id string) (C, bool) {
	delete(s.graceTimers, id)

	c, ok := s.sessions[id]
	if !ok || c.IsConnected() {
		// 그 사이 재접속했으면 아무것도 하지 않는다
		var zero C
		return zero, false
	}
	delete(s.sessions, id)
	return c, true
}
