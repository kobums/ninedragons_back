package server

import (
	"math/rand"
	"testing"
)

// dmNewTestGame 좌석 n 개짜리 시작 전 게임
func dmNewTestGame(t *testing.T, n int) *DMGame {
	t.Helper()
	g := NewDMGame("test")
	for i := 0; i < n; i++ {
		if _, err := g.AddPlayer(string(rune('A' + i))); err != nil {
			t.Fatalf("AddPlayer(%d): %v", i, err)
		}
	}
	return g
}

// TestDMDeck 덱 구성 — 숫자 n 이 n 장(1~12) 78장 + 조커 2장 = 80장
func TestDMDeck(t *testing.T) {
	deck := dmBuildDeck()
	if len(deck) != 80 {
		t.Fatalf("덱 크기 = %d, want 80", len(deck))
	}
	counts := dmRankCounts(deck)
	total := 0
	for r := 1; r <= DMMaxRank; r++ {
		if counts[r] != r {
			t.Fatalf("숫자 %d 의 장수 = %d, want %d", r, counts[r], r)
		}
		total += counts[r]
	}
	if total != 78 {
		t.Fatalf("일반 카드 합 = %d, want 78", total)
	}
	if counts[DMJoker] != 2 {
		t.Fatalf("조커 장수 = %d, want 2", counts[DMJoker])
	}
}

// TestDMParseSet 세트 판정 — 같은 숫자만, 조커는 와일드(그 숫자 취급),
// 조커 단독은 13 취급
func TestDMParseSet(t *testing.T) {
	cases := []struct {
		name      string
		cards     []int
		wantRank  int
		wantCount int
		wantErr   bool
	}{
		{"단독", []int{7}, 7, 1, false},
		{"같은 숫자 3장", []int{5, 5, 5}, 5, 3, false},
		{"조커 섞인 세트", []int{7, 7, DMJoker}, 7, 3, false},
		{"조커 두 장 + 숫자", []int{3, DMJoker, DMJoker}, 3, 3, false},
		{"조커 단독은 13", []int{DMJoker}, DMJoker, 1, false},
		{"조커만 두 장도 13", []int{DMJoker, DMJoker}, DMJoker, 2, false},
		{"다른 숫자 혼합", []int{4, 5}, 0, 0, true},
		{"빈 제출", []int{}, 0, 0, true},
		{"범위 밖", []int{0}, 0, 0, true},
		{"범위 밖 상한", []int{14}, 0, 0, true},
	}
	for _, tc := range cases {
		rank, count, err := dmParseSet(tc.cards)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("%s: 에러 기대했으나 rank=%d count=%d", tc.name, rank, count)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if rank != tc.wantRank || count != tc.wantCount {
			t.Fatalf("%s: rank=%d count=%d, want %d/%d",
				tc.name, rank, count, tc.wantRank, tc.wantCount)
		}
	}
}

// TestDMClimbing 클라이밍 — 같은 장수의 더 낮은 숫자만 (낮을수록 강함),
// 리드 패스 금지, 전원 연속 패스면 마지막 제출자가 새 리드
func TestDMClimbing(t *testing.T) {
	g := dmNewTestGame(t, 4)
	g.Ready = true
	g.Phase = DMPhasePlaying
	g.HandNo = 1
	g.LeadSeat, g.CurrentSeat = 0, 0
	g.Players[0].Hand = []int{8, 8, 9, 10}
	g.Players[1].Hand = []int{6, 6, 7, 11}
	g.Players[2].Hand = []int{5, 5, DMJoker, 12}
	g.Players[3].Hand = []int{2, 3, 4, 12}

	// 리드는 패스할 수 없다
	if err := g.Pass(0); err == nil {
		t.Fatal("리드 패스가 허용됐다")
	}
	// 차례가 아닌 좌석의 제출은 거절
	if err := g.Play(1, []int{6, 6}); err == nil {
		t.Fatal("차례 아닌 좌석의 제출이 허용됐다")
	}
	// 손에 없는 카드
	if err := g.Play(0, []int{1}); err == nil {
		t.Fatal("손에 없는 카드가 허용됐다")
	}

	// 리드: 8 두 장
	if err := g.Play(0, []int{8, 8}); err != nil {
		t.Fatalf("리드 제출: %v", err)
	}
	if g.Table == nil || g.Table.Rank != 8 || g.Table.Count != 2 || g.Table.Seat != 0 {
		t.Fatalf("테이블 세트 = %+v", g.Table)
	}
	if g.CurrentSeat != 1 {
		t.Fatalf("차례 이동 실패: %d", g.CurrentSeat)
	}

	// 장수가 다르면 거절
	if err := g.Play(1, []int{6}); err == nil {
		t.Fatal("다른 장수 세트가 허용됐다")
	}
	// 더 높은(약한) 숫자는 거절 — 11 두 장은 손에 없으니 같은 6/7 조합으로 검증
	if err := g.Play(1, []int{7, 11}); err == nil {
		t.Fatal("서로 다른 숫자 두 장이 허용됐다")
	}
	// 더 낮은 숫자 두 장 — 통과
	if err := g.Play(1, []int{6, 6}); err != nil {
		t.Fatalf("낮은 세트 제출: %v", err)
	}

	// 조커 와일드로 5 두 장 (5 + 조커) — 6보다 낮으므로 통과
	if err := g.Play(2, []int{5, DMJoker}); err != nil {
		t.Fatalf("조커 와일드 제출: %v", err)
	}
	if g.Table.Rank != 5 || g.Table.Count != 2 {
		t.Fatalf("조커 와일드 판정 = %+v", g.Table)
	}
	// 같은 숫자(5)는 이길 수 없다 — 낮아야 한다
	g.Players[3].Hand = append(g.Players[3].Hand, 5, 5)
	if err := g.Play(3, []int{5, 5}); err == nil {
		t.Fatal("같은 숫자 세트가 허용됐다")
	}
	if err := g.Pass(3); err != nil {
		t.Fatalf("패스: %v", err)
	}
	if err := g.Pass(0); err != nil {
		t.Fatalf("패스: %v", err)
	}
	if err := g.Pass(1); err != nil {
		t.Fatalf("패스: %v", err)
	}
	// 전원 패스 — 마지막 제출자(seat2)가 새 리드, 테이블은 비워진다
	if g.Table != nil {
		t.Fatalf("전원 패스 후에도 테이블이 남아 있다: %+v", g.Table)
	}
	if g.LeadSeat != 2 || g.CurrentSeat != 2 {
		t.Fatalf("새 리드 = lead%d cur%d, want 2/2", g.LeadSeat, g.CurrentSeat)
	}
}

// TestDMHandScoringAndLead 핸드 정산 — 순위 점수(1등 = 인원-1점 … 꼴찌 0점)
// 누적과 다음 핸드 리드(직전 핸드 1등) 계승
func TestDMHandScoringAndLead(t *testing.T) {
	g := dmNewTestGame(t, 4)
	g.Ready = true
	g.Phase = DMPhasePlaying
	g.HandNo = 1
	g.LeadSeat, g.CurrentSeat = 0, 0
	// seat1 → seat0 → seat2 순으로 손을 털고 seat3 이 마지막
	g.Players[0].Hand = []int{4}
	g.Players[1].Hand = []int{2}
	g.Players[2].Hand = []int{6}
	g.Players[3].Hand = []int{9, 10}

	if err := g.Play(0, []int{4}); err != nil { // seat0 1등
		t.Fatalf("play0: %v", err)
	}
	if g.Players[0].OutRank != 1 {
		t.Fatalf("seat0 순위 = %d", g.Players[0].OutRank)
	}
	if g.CurrentSeat != 1 {
		t.Fatalf("차례 = %d", g.CurrentSeat)
	}
	if err := g.Play(1, []int{2}); err != nil { // seat1 2등
		t.Fatalf("play1: %v", err)
	}
	// 남은 사람은 seat2·seat3 — seat2 가 손을 털면 핸드 종료
	if g.CurrentSeat != 2 {
		t.Fatalf("차례 = %d", g.CurrentSeat)
	}
	if err := g.Play(2, []int{6}); err == nil {
		t.Fatal("2보다 높은 숫자가 허용됐다")
	}
	if err := g.Pass(2); err != nil {
		t.Fatalf("pass2: %v", err)
	}
	if err := g.Pass(3); err != nil {
		t.Fatalf("pass3: %v", err)
	}
	// 전원 패스 — 마지막 제출자 seat1 은 아웃이므로 다음 생존 좌석 seat2 리드
	if g.LeadSeat != 2 || g.CurrentSeat != 2 {
		t.Fatalf("아웃 제출자 후 리드 = lead%d cur%d, want 2/2", g.LeadSeat, g.CurrentSeat)
	}
	if err := g.Play(2, []int{6}); err != nil { // seat2 3등 → 핸드 종료
		t.Fatalf("play2: %v", err)
	}
	if g.Phase != DMPhaseHandEnd {
		t.Fatalf("phase = %s, want hand_end", g.Phase)
	}
	if g.HandResult == nil {
		t.Fatal("handResult 부재")
	}
	wantOrder := []int{0, 1, 2, 3}
	for i, s := range wantOrder {
		if g.HandResult.Order[i] != s {
			t.Fatalf("순위 순서 = %v, want %v", g.HandResult.Order, wantOrder)
		}
	}
	wantPts := []int{3, 2, 1, 0} // 인원 4 → 1등 3점 … 꼴찌 0점
	for s, want := range wantPts {
		if g.Players[s].Points != want {
			t.Fatalf("seat%d 점수 = %d, want %d", s, g.Players[s].Points, want)
		}
	}

	// 다음 핸드 리드는 직전 핸드 1등
	rng := rand.New(rand.NewSource(7))
	g.AdvanceHand(rng)
	if g.Phase != DMPhasePlaying || g.HandNo != 2 {
		t.Fatalf("다음 핸드 전이 실패: phase=%s handNo=%d", g.Phase, g.HandNo)
	}
	if g.LeadSeat != 0 || g.CurrentSeat != 0 {
		t.Fatalf("2번째 핸드 리드 = %d, want 0 (직전 1등)", g.LeadSeat)
	}
	if len(g.Players[0].Hand) != 80/4 {
		t.Fatalf("배분 장수 = %d, want %d", len(g.Players[0].Hand), 80/4)
	}
	for _, p := range g.Players {
		if p.OutRank != 0 {
			t.Fatalf("seat%d 순위가 초기화되지 않았다: %d", p.Seat, p.OutRank)
		}
	}
}

// TestDMAutoChoice 자동 선택(AFK·봇 공용) — 리드는 최다 장수 세트 중 최고
// 숫자, 팔로우는 유효한 것 중 최고 숫자. 조커는 세트 완성에만 쓴다.
func TestDMAutoChoice(t *testing.T) {
	// 리드: 3장짜리 세트가 최다 — 동수면 높은(약한) 숫자
	lead := dmLeadChoice([]int{2, 5, 5, 5, 9, 9, 9, 11})
	if len(lead) != 3 || lead[0] != 9 {
		t.Fatalf("리드 선택 = %v, want 9 세 장", lead)
	}
	if got := dmLeadChoice([]int{4}); len(got) != 1 || got[0] != 4 {
		t.Fatalf("한 장 리드 = %v", got)
	}
	if got := dmLeadChoice([]int{}); got != nil {
		t.Fatalf("빈 손 리드 = %v, want nil", got)
	}

	// 팔로우: 8 두 장을 이겨야 한다 — 유효한 것 중 최고(약한) 숫자 7
	follow := dmFollowChoice([]int{3, 3, 7, 7, 9, 9}, 8, 2)
	if len(follow) != 2 || follow[0] != 7 || follow[1] != 7 {
		t.Fatalf("팔로우 선택 = %v, want 7 두 장", follow)
	}
	// 조커 없이 완성되는 세트를 우선한다 (조커는 아낀다)
	keep := dmFollowChoice([]int{6, 6, 7, DMJoker}, 8, 2)
	if len(keep) != 2 || keep[0] != 6 || keep[1] != 6 {
		t.Fatalf("조커 아끼기 실패 = %v, want 6 두 장", keep)
	}
	// 부족할 때만 조커로 장수를 채운다
	wild := dmFollowChoice([]int{7, DMJoker, 11, 11}, 8, 2)
	if len(wild) != 2 || wild[0] != 7 || wild[1] != DMJoker {
		t.Fatalf("조커 세트 완성 실패 = %v, want [7 13]", wild)
	}
	if rank, count, err := dmParseSet(wild); err != nil || rank != 7 || count != 2 {
		t.Fatalf("조커 세트가 규칙상 무효: rank=%d count=%d err=%v", rank, count, err)
	}
	// 유효한 수가 없으면 nil (패스)
	if got := dmFollowChoice([]int{9, 9, 10}, 5, 2); got != nil {
		t.Fatalf("무효 상황 선택 = %v, want nil", got)
	}
	// 최강(1) 앞에서는 아무것도 못 낸다
	if got := dmFollowChoice([]int{2, 2, DMJoker}, 1, 2); got != nil {
		t.Fatalf("1 앞에서 선택 = %v, want nil", got)
	}
}

// TestDMThreeHandGame 5인 3핸드 완주 — 자동 선택만으로 돌려 순위·총점·
// 종료 판정(동점 공동 우승 포함)이 성립하는지 검증한다
func TestDMThreeHandGame(t *testing.T) {
	for seed := int64(1); seed <= 5; seed++ {
		rng := rand.New(rand.NewSource(seed))
		g := dmNewTestGame(t, 5)
		if err := g.Start(rng); err != nil {
			t.Fatalf("Start: %v", err)
		}

		steps := 0
		for g.Phase != DMPhaseGameOver {
			steps++
			if steps > 5000 {
				t.Fatalf("seed %d: 진행 불가 — phase=%s hand=%d cur=%d table=%+v",
					seed, g.Phase, g.HandNo, g.CurrentSeat, g.Table)
			}
			if g.Phase == DMPhaseHandEnd {
				if g.HandResult == nil || len(g.HandResult.Order) != 5 {
					t.Fatalf("seed %d: 핸드 정산 이상 %+v", seed, g.HandResult)
				}
				g.AdvanceHand(rng)
				continue
			}
			seat := g.CurrentSeat
			if cards := g.AutoPlay(seat); cards != nil {
				if err := g.Play(seat, cards); err != nil {
					t.Fatalf("seed %d: 자동 제출 거절 %v (%v)", seed, err, cards)
				}
				continue
			}
			if err := g.Pass(seat); err != nil {
				t.Fatalf("seed %d: 자동 패스 거절 %v", seed, err)
			}
		}

		if g.HandNo != DMHands {
			t.Fatalf("seed %d: 완주 핸드 수 = %d, want %d", seed, g.HandNo, DMHands)
		}
		// 3핸드 × (4+3+2+1+0) = 30점이 정확히 분배된다
		total, best := 0, -1
		for _, p := range g.Players {
			total += p.Points
			if p.Points > best {
				best = p.Points
			}
		}
		if total != DMHands*10 {
			t.Fatalf("seed %d: 총점 합 = %d, want %d", seed, total, DMHands*10)
		}
		if len(g.WinnerSeats) == 0 {
			t.Fatalf("seed %d: 승자 부재", seed)
		}
		for _, s := range g.WinnerSeats {
			if g.Players[s].Points != best {
				t.Fatalf("seed %d: 승자 seat%d 점수 = %d, want %d",
					seed, s, g.Players[s].Points, best)
			}
		}
		for _, p := range g.Players {
			if p.Points == best {
				found := false
				for _, s := range g.WinnerSeats {
					if s == p.Seat {
						found = true
					}
				}
				if !found {
					t.Fatalf("seed %d: 동점 seat%d 가 공동 우승에서 빠졌다", seed, p.Seat)
				}
			}
		}
	}
}

// TestDMBotBrain 봇 두뇌 — 리드는 반드시 제출(패스 불가), 팔로우는 유효한
// 최고 숫자 또는 패스, 차례가 아니면 무응답, 같은 상태 중복 응답 금지
func TestDMBotBrain(t *testing.T) {
	b := newDMBrain()
	lead := dmBotState{YourSeat: 1, Phase: DMPhasePlaying, HandNo: 1, CurrentSeat: 1,
		EndsAt: 100, YourHand: []int{2, 6, 6, 6, 11}}
	reply := b.decideState(lead)
	if reply == nil || reply.Type != DMMsgPlay {
		t.Fatalf("리드 응답 = %+v, want dm_play", reply)
	}
	cards := reply.Payload.(DMPlayPayload).Cards
	if len(cards) != 3 || cards[0] != 6 {
		t.Fatalf("리드 세트 = %v, want 6 세 장", cards)
	}
	// 같은 대기 상태에 두 번 응답하지 않는다
	if again := b.decideState(lead); again != nil {
		t.Fatalf("중복 응답 = %+v", again)
	}
	// 차례가 아니면 무응답
	if got := b.decideState(dmBotState{YourSeat: 1, Phase: DMPhasePlaying,
		CurrentSeat: 2, EndsAt: 200, YourHand: []int{3}}); got != nil {
		t.Fatalf("차례 아닌데 응답 = %+v", got)
	}
	// hand_end 에는 응답하지 않는다
	if got := b.decideState(dmBotState{YourSeat: 1, Phase: DMPhaseHandEnd,
		CurrentSeat: 1, EndsAt: 300, YourHand: []int{3}}); got != nil {
		t.Fatalf("hand_end 응답 = %+v", got)
	}
	// 낼 수 없는 팔로우는 반드시 패스
	pass := b.decideState(dmBotState{YourSeat: 1, Phase: DMPhasePlaying, HandNo: 1,
		CurrentSeat: 1, EndsAt: 400, TableSet: &DMTableSet{Rank: 2, Count: 2, Seat: 0},
		YourHand: []int{9, 9, 10}})
	if pass == nil || pass.Type != DMMsgPass {
		t.Fatalf("무효 팔로우 응답 = %+v, want dm_pass", pass)
	}
	// 유효한 팔로우는 제출 또는 패스 — 제출이면 규칙을 만족해야 한다
	seen := map[DMMessageType]bool{}
	for i := 0; i < 200; i++ {
		b2 := newDMBrain()
		got := b2.decideState(dmBotState{YourSeat: 1, Phase: DMPhasePlaying, HandNo: 1,
			CurrentSeat: 1, EndsAt: int64(500 + i),
			TableSet: &DMTableSet{Rank: 9, Count: 2, Seat: 0},
			YourHand: []int{4, 4, 7, 7, 12}})
		if got == nil {
			t.Fatal("유효 팔로우에 무응답")
		}
		seen[got.Type] = true
		if got.Type == DMMsgPlay {
			c := got.Payload.(DMPlayPayload).Cards
			rank, count, err := dmParseSet(c)
			if err != nil || count != 2 || rank >= 9 {
				t.Fatalf("봇 제출이 규칙 위반: %v (rank=%d count=%d err=%v)", c, rank, count, err)
			}
			if rank != 7 {
				t.Fatalf("봇 제출 = %v, want 유효한 것 중 최고 숫자 7", c)
			}
		}
	}
	if !seen[DMMsgPlay] || !seen[DMMsgPass] {
		t.Fatalf("봇이 제출·패스를 섞지 않는다: %v", seen)
	}
}
