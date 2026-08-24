package server

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// ==================== 순수 규칙 테스트 (허브·타이머 비의존) ====================

func ekRNG() *rand.Rand { return rand.New(rand.NewSource(20260823)) }

// ekNewStarted Start 를 거친 게임 (덱·손패는 무작위)
func ekNewStarted(t *testing.T, n int, rng *rand.Rand) *EKGame {
	t.Helper()
	g := NewEKGame("t")
	for i := 0; i < n; i++ {
		if _, err := g.AddPlayer(fmt.Sprintf("P%d", i)); err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
	}
	if err := g.Start(rng); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return g
}

// ekRigged 손패·덱을 직접 꽂아 넣은 결정적 게임 (차례는 seat0, 1차례)
func ekRigged(t *testing.T, hands [][]EKCard, deck []EKCard) *EKGame {
	t.Helper()
	g := NewEKGame("rig")
	for i := range hands {
		if _, err := g.AddPlayer(fmt.Sprintf("P%d", i)); err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
	}
	g.Ready = true
	for i, h := range hands {
		g.Players[i].Hand = append([]EKCard{}, h...)
		g.Players[i].Alive = true
	}
	g.Deck = append([]EKCard{}, deck...)
	g.Discard = []EKCard{}
	g.CurrentSeat = 0
	g.TurnsLeft = 1
	g.Phase = EKPhaseTurn
	g.StateSeq++
	return g
}

func ekCount(cards []EKCard, want EKCard) int {
	n := 0
	for _, c := range cards {
		if c == want {
			n++
		}
	}
	return n
}

// TestEKDeckComposition 인원별 덱 구성 — 폭탄 n-1, 해체 1인 1장 + 잔여,
// 시작 손패 8장(해체 1 + 7). 카드 총량이 보존돼야 한다.
func TestEKDeckComposition(t *testing.T) {
	rng := ekRNG()
	for n := EKMinPlayers; n <= EKMaxPlayers; n++ {
		t.Run(fmt.Sprintf("%d인", n), func(t *testing.T) {
			g := ekNewStarted(t, n, rng)

			if got, want := ekCount(g.Deck, EKCardBomb), n-1; got != want {
				t.Fatalf("덱 폭탄 = %d, want %d", got, want)
			}
			if got, want := ekCount(g.Deck, EKCardDefuse), EKDefuseTotal-n; got != want {
				t.Fatalf("덱 해체 = %d, want %d", got, want)
			}
			if got, want := len(g.Deck), 51-7*n; got != want {
				t.Fatalf("덱 장수 = %d, want %d", got, want)
			}

			total := len(g.Deck)
			for _, p := range g.Players {
				if len(p.Hand) != EKStartHand+1 {
					t.Fatalf("seat%d 손패 = %d, want %d", p.Seat, len(p.Hand), EKStartHand+1)
				}
				if got := ekCount(p.Hand, EKCardDefuse); got != 1 {
					t.Fatalf("seat%d 시작 해체 = %d, want 1", p.Seat, got)
				}
				if got := ekCount(p.Hand, EKCardBomb); got != 0 {
					t.Fatalf("seat%d 손패에 폭탄 %d장 — 폭탄은 손에 들 수 없다", p.Seat, got)
				}
				total += len(p.Hand)
			}
			if want := len(ekBaseDeck()) + EKDefuseTotal + (n - 1); total != want {
				t.Fatalf("카드 총량 = %d, want %d", total, want)
			}
			if g.Phase != EKPhaseTurn || g.TurnsLeft != 1 {
				t.Fatalf("시작 상태 phase=%s turnsLeft=%d", g.Phase, g.TurnsLeft)
			}
			if g.BombsLeft() != n-1 {
				t.Fatalf("BombsLeft = %d, want %d", g.BombsLeft(), n-1)
			}
		})
	}
}

// TestEKAttackAccumulation 공격 누적 — 내가 남긴 차례에 2를 더해 다음
// 사람에게 넘긴다. 뽑기·건너뛰기는 차례를 하나만 소모한다.
func TestEKAttackAccumulation(t *testing.T) {
	rng := ekRNG()
	cases := []struct {
		name      string
		turnsLeft int
		card      EKCard
		wantSeat  int
		wantTurns int
	}{
		{"공격: 남은 1차례 → 다음 2차례", 1, EKCardAttack, 1, 2},
		{"공격: 남은 2차례 → 다음 3차례", 2, EKCardAttack, 1, 3},
		{"공격: 남은 3차례 → 다음 4차례", 3, EKCardAttack, 1, 4},
		{"건너뛰기: 남은 1차례 → 다음 사람 1차례", 1, EKCardSkip, 1, 1},
		{"건너뛰기: 남은 2차례 → 내 차례 1 남음", 2, EKCardSkip, 0, 1},
		{"건너뛰기: 남은 3차례 → 내 차례 2 남음", 3, EKCardSkip, 0, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := ekRigged(t, [][]EKCard{{tc.card}, {}, {}}, []EKCard{EKCardTaco, EKCardMelon})
			g.TurnsLeft = tc.turnsLeft

			if err := g.Play(0, 0, -1, rng); err != nil {
				t.Fatalf("Play: %v", err)
			}
			if g.Phase != EKPhaseNopeWindow {
				t.Fatalf("phase = %s, want nope_window", g.Phase)
			}
			g.ForcePassWindow(rng) // 아무도 아뇨하지 않음

			if g.CurrentSeat != tc.wantSeat || g.TurnsLeft != tc.wantTurns {
				t.Fatalf("current=%d turnsLeft=%d, want %d/%d",
					g.CurrentSeat, g.TurnsLeft, tc.wantSeat, tc.wantTurns)
			}
			if g.Phase != EKPhaseTurn {
				t.Fatalf("phase = %s, want turn", g.Phase)
			}
		})
	}
}

// TestEKAttackTurnsConsumedByDraw 공격으로 받은 2차례는 뽑기 2번으로 소모된다
func TestEKAttackTurnsConsumedByDraw(t *testing.T) {
	rng := ekRNG()
	g := ekRigged(t, [][]EKCard{{EKCardAttack}, {}, {}},
		[]EKCard{EKCardTaco, EKCardMelon, EKCardSkip})
	if err := g.Play(0, 0, -1, rng); err != nil {
		t.Fatalf("Play: %v", err)
	}
	g.ForcePassWindow(rng)
	if g.CurrentSeat != 1 || g.TurnsLeft != 2 {
		t.Fatalf("공격 직후 current=%d turnsLeft=%d", g.CurrentSeat, g.TurnsLeft)
	}
	if err := g.Draw(1, rng); err != nil {
		t.Fatalf("Draw1: %v", err)
	}
	if g.CurrentSeat != 1 || g.TurnsLeft != 1 {
		t.Fatalf("첫 뽑기 후 current=%d turnsLeft=%d, want 1/1", g.CurrentSeat, g.TurnsLeft)
	}
	if err := g.Draw(1, rng); err != nil {
		t.Fatalf("Draw2: %v", err)
	}
	if g.CurrentSeat != 2 || g.TurnsLeft != 1 {
		t.Fatalf("둘째 뽑기 후 current=%d turnsLeft=%d, want 2/1", g.CurrentSeat, g.TurnsLeft)
	}
}

// TestEKNopeParity 아뇨 겹침 홀짝 판정 — 짝수 겹이면 효과 유효, 홀수면 무효.
// 아뇨가 나올 때마다 창이 다시 열려(StateSeq 증가) 재아뇨를 받는다.
func TestEKNopeParity(t *testing.T) {
	rng := ekRNG()
	cases := []struct {
		name      string
		nopers    []int // 아뇨를 낸 좌석 순서
		wantSeat  int   // 판정 후 차례 좌석 (건너뛰기가 발동하면 1)
		wantNoped bool
	}{
		{"0겹 — 발동", nil, 1, false},
		{"1겹 — 무효", []int{1}, 0, true},
		{"2겹 — 발동", []int{1, 2}, 1, false},
		{"3겹 — 무효", []int{1, 2, 3}, 0, true},
		{"4겹 — 발동", []int{1, 2, 3, 1}, 1, false},
		{"5겹 — 무효", []int{1, 2, 3, 1, 2}, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hands := [][]EKCard{
				{EKCardSkip},
				{EKCardNope, EKCardNope},
				{EKCardNope, EKCardNope},
				{EKCardNope, EKCardNope},
			}
			g := ekRigged(t, hands, []EKCard{EKCardTaco, EKCardMelon})
			if err := g.Play(0, 0, -1, rng); err != nil {
				t.Fatalf("Play: %v", err)
			}

			prevSeq := g.StateSeq
			for i, seat := range tc.nopers {
				if err := g.Nope(seat); err != nil {
					t.Fatalf("Nope[%d] seat%d: %v", i, seat, err)
				}
				if g.Pending.NopeCount != i+1 {
					t.Fatalf("nopeCount = %d, want %d", g.Pending.NopeCount, i+1)
				}
				if g.StateSeq <= prevSeq {
					t.Fatalf("아뇨가 겹쳤는데 StateSeq 가 그대로 (%d) — 창이 재개방되지 않았다", g.StateSeq)
				}
				prevSeq = g.StateSeq
				if g.Phase != EKPhaseNopeWindow {
					t.Fatalf("아뇨 후 phase = %s, want nope_window", g.Phase)
				}
			}

			// 방금 아뇨를 낸 좌석은 자기 아뇨에 다시 아뇨할 수 없다
			if len(tc.nopers) > 0 {
				last := tc.nopers[len(tc.nopers)-1]
				before := g.Pending.NopeCount
				g.Nope(last)
				if g.Pending.NopeCount != before {
					t.Fatalf("마지막 아뇨 좌석이 자기 아뇨를 다시 겹쳤다")
				}
			}

			g.ForcePassWindow(rng)
			if g.CurrentSeat != tc.wantSeat {
				t.Fatalf("판정 후 current = %d, want %d (아뇨 %d겹)",
					g.CurrentSeat, tc.wantSeat, len(tc.nopers))
			}
			if g.Phase != EKPhaseTurn {
				t.Fatalf("판정 후 phase = %s, want turn", g.Phase)
			}
			if g.Pending != nil {
				t.Fatalf("판정 후 pending 이 남았다: %+v", g.Pending)
			}
			// 낸 아뇨 수만큼 버린 더미에 쌓인다 (건너뛰기 1 + 아뇨 n)
			if got, want := len(g.Discard), 1+len(tc.nopers); got != want {
				t.Fatalf("버린 더미 = %d장, want %d", got, want)
			}
			noped := strings.Contains(g.LastAction.Message, "막혔습니다")
			if noped != tc.wantNoped {
				t.Fatalf("lastAction = %q (wantNoped=%v)", g.LastAction.Message, tc.wantNoped)
			}
		})
	}
}

// TestEKNopeWindowClosesByPasses 응답자 전원이 통과하면 마감을 기다리지 않고
// 즉시 판정된다. 마지막 아뇨를 낸 좌석의 통과는 세지 않는다.
func TestEKNopeWindowClosesByPasses(t *testing.T) {
	rng := ekRNG()
	g := ekRigged(t, [][]EKCard{{EKCardSkip}, {EKCardNope}, {}}, []EKCard{EKCardTaco})
	if err := g.Play(0, 0, -1, rng); err != nil {
		t.Fatalf("Play: %v", err)
	}
	g.Pass(1, rng)
	if g.Phase != EKPhaseNopeWindow {
		t.Fatalf("한 명 통과만으로 창이 닫혔다: %s", g.Phase)
	}
	g.Pass(0, rng) // 낸 사람의 통과는 세지 않는다 (LastSeat)
	if g.Phase != EKPhaseNopeWindow {
		t.Fatalf("낸 사람의 통과가 창을 닫았다: %s", g.Phase)
	}
	g.Pass(2, rng)
	if g.Phase != EKPhaseTurn || g.CurrentSeat != 1 {
		t.Fatalf("전원 통과 후 phase=%s current=%d, want turn/1", g.Phase, g.CurrentSeat)
	}
}

// TestEKDefusePlacePosition 폭탄 되꽂기 위치 표 — 0=맨 위 … len=맨 아래,
// 범위 밖은 잘라 붙인다. 위치는 이벤트 문구에 새어 나가지 않는다.
func TestEKDefusePlacePosition(t *testing.T) {
	rng := ekRNG()
	base := []EKCard{EKCardTaco, EKCardMelon, EKCardBeard}
	cases := []struct {
		name string
		pos  int
		want []EKCard
	}{
		{"맨 위", 0, []EKCard{EKCardBomb, EKCardTaco, EKCardMelon, EKCardBeard}},
		{"맨 위 바로 아래", 1, []EKCard{EKCardTaco, EKCardBomb, EKCardMelon, EKCardBeard}},
		{"가운데", 2, []EKCard{EKCardTaco, EKCardMelon, EKCardBomb, EKCardBeard}},
		{"맨 아래", 3, []EKCard{EKCardTaco, EKCardMelon, EKCardBeard, EKCardBomb}},
		{"범위 초과는 맨 아래로", 99, []EKCard{EKCardTaco, EKCardMelon, EKCardBeard, EKCardBomb}},
		{"음수는 맨 위로", -7, []EKCard{EKCardBomb, EKCardTaco, EKCardMelon, EKCardBeard}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deck := append([]EKCard{EKCardBomb}, base...)
			g := ekRigged(t, [][]EKCard{{EKCardDefuse}, {}, {}}, deck)

			if err := g.Draw(0, rng); err != nil {
				t.Fatalf("Draw: %v", err)
			}
			if g.Phase != EKPhaseDefusePlace || g.Pending == nil || g.Pending.BySeat != 0 {
				t.Fatalf("해체 후 phase=%s pending=%+v", g.Phase, g.Pending)
			}
			if len(g.Players[0].Hand) != 0 {
				t.Fatalf("해체가 손에 남았다: %v", g.Players[0].Hand)
			}
			// 되꽂기 전 덱에는 폭탄이 없다 (손에도 없다 — 잠시 공중에 있다)
			if ekCount(g.Deck, EKCardBomb) != 0 {
				t.Fatalf("되꽂기 전 덱에 폭탄이 있다: %v", g.Deck)
			}

			if err := g.DefusePlace(0, tc.pos); err != nil {
				t.Fatalf("DefusePlace: %v", err)
			}
			if fmt.Sprint(g.Deck) != fmt.Sprint(tc.want) {
				t.Fatalf("덱 = %v, want %v", g.Deck, tc.want)
			}
			if g.CurrentSeat != 1 || g.Phase != EKPhaseTurn {
				t.Fatalf("되꽂기 후 current=%d phase=%s, want 1/turn", g.CurrentSeat, g.Phase)
			}
			for _, ev := range g.DrainEvents() {
				if strings.Contains(ev.Message, fmt.Sprint(tc.pos)) && tc.pos != 0 {
					t.Fatalf("되꽂기 위치가 이벤트에 유출됐다: %q", ev.Message)
				}
			}
		})
	}
}

// TestEKDefuseAutoPlace 되꽂기 방치 — 무작위 위치로 자동 처리되고 차례가 넘어간다
func TestEKDefuseAutoPlace(t *testing.T) {
	rng := ekRNG()
	g := ekRigged(t, [][]EKCard{{EKCardDefuse}, {}, {}},
		[]EKCard{EKCardBomb, EKCardTaco, EKCardMelon})
	if err := g.Draw(0, rng); err != nil {
		t.Fatalf("Draw: %v", err)
	}
	g.AutoDefusePlace(rng)
	if ekCount(g.Deck, EKCardBomb) != 1 || len(g.Deck) != 3 {
		t.Fatalf("자동 되꽂기 실패: %v", g.Deck)
	}
	if g.Phase != EKPhaseTurn || g.CurrentSeat != 1 {
		t.Fatalf("자동 되꽂기 후 phase=%s current=%d", g.Phase, g.CurrentSeat)
	}
}

// TestEKElimination 탈락 처리 — 해체 없이 폭탄을 뽑으면 alive=false 로
// 남고(방을 나가지 않는다) 손패는 버린 더미로, 그 폭탄은 덱에서 사라진다.
// 마지막 1명이 남으면 종료.
func TestEKElimination(t *testing.T) {
	rng := ekRNG()
	g := ekRigged(t,
		[][]EKCard{{EKCardTaco}, {EKCardMelon}, {EKCardBeard}},
		[]EKCard{EKCardBomb, EKCardSkip, EKCardBomb})

	if err := g.Draw(0, rng); err != nil {
		t.Fatalf("Draw: %v", err)
	}
	if len(g.Players) != 3 {
		t.Fatalf("탈락자가 목록에서 빠졌다: %d명", len(g.Players))
	}
	if g.Players[0].Alive {
		t.Fatal("폭탄을 맞았는데 생존 상태다")
	}
	if len(g.Players[0].Hand) != 0 {
		t.Fatalf("탈락자 손패가 남았다: %v", g.Players[0].Hand)
	}
	if g.discardTop() != string(EKCardBomb) {
		t.Fatalf("버린 더미 맨 위 = %q, want bomb", g.discardTop())
	}
	if g.aliveCount() != 2 || g.BombsLeft() != 1 {
		t.Fatalf("생존 %d명 / 남은 폭탄 %d", g.aliveCount(), g.BombsLeft())
	}
	if g.CurrentSeat != 1 || g.TurnsLeft != 1 || g.Phase != EKPhaseTurn {
		t.Fatalf("탈락 후 current=%d turnsLeft=%d phase=%s", g.CurrentSeat, g.TurnsLeft, g.Phase)
	}
	// 탈락자는 더 이상 행동할 수 없다
	if err := g.Draw(0, rng); err == nil {
		t.Fatal("탈락자가 뽑을 수 있었다")
	}

	if err := g.Draw(1, rng); err != nil { // skip 카드를 뽑고 차례 종료
		t.Fatalf("Draw seat1: %v", err)
	}
	if g.CurrentSeat != 2 {
		t.Fatalf("current = %d, want 2", g.CurrentSeat)
	}
	if err := g.Draw(2, rng); err != nil { // 두 번째 폭탄
		t.Fatalf("Draw seat2: %v", err)
	}
	if g.Phase != EKPhaseGameOver || g.WinnerSeat != 1 {
		t.Fatalf("종료 실패: phase=%s winner=%d", g.Phase, g.WinnerSeat)
	}
	if g.aliveCount() != 1 {
		t.Fatalf("생존자 = %d명", g.aliveCount())
	}
}

// TestEKAttackedPlayerEliminated 공격으로 쌓인 차례는 탈락과 함께 사라진다
func TestEKAttackedPlayerEliminated(t *testing.T) {
	rng := ekRNG()
	g := ekRigged(t, [][]EKCard{{EKCardAttack}, {}, {}},
		[]EKCard{EKCardBomb, EKCardTaco, EKCardMelon})
	if err := g.Play(0, 0, -1, rng); err != nil {
		t.Fatalf("Play: %v", err)
	}
	g.ForcePassWindow(rng)
	if g.CurrentSeat != 1 || g.TurnsLeft != 2 {
		t.Fatalf("공격 후 current=%d turnsLeft=%d", g.CurrentSeat, g.TurnsLeft)
	}
	if err := g.Draw(1, rng); err != nil { // 해체 없음 → 탈락
		t.Fatalf("Draw: %v", err)
	}
	if g.Players[1].Alive {
		t.Fatal("탈락하지 않았다")
	}
	if g.CurrentSeat != 2 || g.TurnsLeft != 1 {
		t.Fatalf("탈락 후 current=%d turnsLeft=%d, want 2/1", g.CurrentSeat, g.TurnsLeft)
	}
}

// TestEKFavor 호의 — 대상이 카드를 고를 때까지 favor_wait, 방치는 무작위
func TestEKFavor(t *testing.T) {
	rng := ekRNG()
	g := ekRigged(t,
		[][]EKCard{{EKCardFavor}, {EKCardTaco, EKCardMelon}, {}},
		[]EKCard{EKCardBeard})

	if err := g.Play(0, 0, 1, rng); err != nil {
		t.Fatalf("Play: %v", err)
	}
	g.ForcePassWindow(rng)
	if g.Phase != EKPhaseFavorWait || g.Pending == nil || g.Pending.TargetSeat != 1 {
		t.Fatalf("phase=%s pending=%+v, want favor_wait/target1", g.Phase, g.Pending)
	}
	// 시전자는 대신 낼 수 없다
	if err := g.Give(0, 0); err == nil {
		t.Fatal("호의받지 않은 사람이 카드를 건넸다")
	}
	if err := g.Give(1, 1); err != nil { // 멜론을 건넨다
		t.Fatalf("Give: %v", err)
	}
	if len(g.Players[0].Hand) != 1 || g.Players[0].Hand[0] != EKCardMelon {
		t.Fatalf("받은 손패 = %v", g.Players[0].Hand)
	}
	if len(g.Players[1].Hand) != 1 {
		t.Fatalf("건넨 쪽 손패 = %v", g.Players[1].Hand)
	}
	if g.Phase != EKPhaseTurn || g.CurrentSeat != 0 {
		t.Fatalf("호의 후 phase=%s current=%d — 차례는 유지된다", g.Phase, g.CurrentSeat)
	}
	// 건넨 카드 종류는 이벤트에 실리지 않는다
	for _, ev := range g.DrainEvents() {
		if ev.Kind == "favor_give" && strings.Contains(ev.Message, ekCardName(EKCardMelon)) {
			t.Fatalf("건넨 카드가 유출됐다: %q", ev.Message)
		}
	}

	// 대상 손이 비면 무산된다
	g2 := ekRigged(t, [][]EKCard{{EKCardFavor}, {}, {}}, []EKCard{EKCardBeard})
	if err := g2.Play(0, 0, 1, rng); err != nil {
		t.Fatalf("Play: %v", err)
	}
	g2.ForcePassWindow(rng)
	if g2.Phase != EKPhaseTurn {
		t.Fatalf("빈손 상대 호의이 창을 남겼다: %s", g2.Phase)
	}

	// 방치 → 무작위 카드
	g3 := ekRigged(t, [][]EKCard{{EKCardFavor}, {EKCardTaco}, {}}, []EKCard{EKCardBeard})
	if err := g3.Play(0, 0, 1, rng); err != nil {
		t.Fatalf("Play: %v", err)
	}
	g3.ForcePassWindow(rng)
	g3.AutoGive(rng)
	if len(g3.Players[0].Hand) != 1 || len(g3.Players[1].Hand) != 0 {
		t.Fatalf("자동 건네기 실패: %v / %v", g3.Players[0].Hand, g3.Players[1].Hand)
	}
}

// TestEKPlayPair 고양이 짝 훔치기와 잘못된 조합 거절
func TestEKPlayPair(t *testing.T) {
	rng := ekRNG()
	hands := [][]EKCard{
		{EKCardTaco, EKCardMelon, EKCardTaco, EKCardSkip},
		{EKCardBeard},
		{},
	}
	g := ekRigged(t, hands, []EKCard{EKCardPotato})

	bad := []struct {
		name    string
		indexes []int
		target  int
	}{
		{"같은 인덱스 두 번", []int{0, 0}, 1},
		{"다른 종류", []int{0, 1}, 1},
		{"고양이가 아님", []int{3, 3}, 1},
		{"자기 자신 대상", []int{0, 2}, 0},
		{"없는 상대", []int{0, 2}, 9},
		{"장수 부족", []int{0}, 1},
	}
	for _, tc := range bad {
		if err := g.PlayPair(0, tc.indexes, tc.target, rng); err == nil {
			t.Fatalf("%s: 통과됐다", tc.name)
		}
	}

	if err := g.PlayPair(0, []int{0, 2}, 1, rng); err != nil {
		t.Fatalf("PlayPair: %v", err)
	}
	if g.Pending == nil || g.Pending.Kind != EKPendKindPair {
		t.Fatalf("pending = %+v, want pair", g.Pending)
	}
	g.ForcePassWindow(rng)

	if fmt.Sprint(g.Players[0].Hand) != fmt.Sprint([]EKCard{EKCardMelon, EKCardSkip, EKCardBeard}) {
		t.Fatalf("훔친 뒤 손패 = %v", g.Players[0].Hand)
	}
	if len(g.Players[1].Hand) != 0 {
		t.Fatalf("빼앗긴 손패 = %v", g.Players[1].Hand)
	}
	if g.discardTop() != string(EKCardTaco) || len(g.Discard) != 2 {
		t.Fatalf("버린 더미 = %v", g.Discard)
	}
	// 훔친 카드 종류는 이벤트에 실리지 않는다
	for _, ev := range g.DrainEvents() {
		if ev.Kind == "steal" && strings.Contains(ev.Message, ekCardName(EKCardBeard)) {
			t.Fatalf("훔친 카드가 유출됐다: %q", ev.Message)
		}
	}
}

// TestEKIllegalPlays 낼 수 없는 카드·차례가 아닌 사람의 행동 거절
func TestEKIllegalPlays(t *testing.T) {
	rng := ekRNG()
	hands := [][]EKCard{
		{EKCardDefuse, EKCardNope, EKCardTaco, EKCardFavor},
		{EKCardSkip},
		{},
	}
	cases := []struct {
		name   string
		seat   int
		index  int
		target int
	}{
		{"해체는 낼 수 없다", 0, 0, -1},
		{"아뇨는 자기 차례에 낼 수 없다", 0, 1, -1},
		{"고양이 한 장은 낼 수 없다", 0, 2, -1},
		{"호의은 상대가 필요하다", 0, 3, -1},
		{"자기 자신에게 호의", 0, 3, 0},
		{"없는 인덱스", 0, 99, -1},
		{"차례가 아닌 사람", 1, 0, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := ekRigged(t, hands, []EKCard{EKCardMelon})
			if err := g.Play(tc.seat, tc.index, tc.target, rng); err == nil {
				t.Fatal("통과됐다")
			}
		})
	}

	g := ekRigged(t, hands, []EKCard{EKCardMelon})
	if err := g.Draw(1, rng); err == nil {
		t.Fatal("차례가 아닌 사람이 뽑았다")
	}
	// 아뇨 창 중에는 뽑을 수 없다
	if err := g.Play(0, 3, 1, rng); err != nil {
		t.Fatalf("Play favor: %v", err)
	}
	if err := g.Draw(0, rng); err == nil {
		t.Fatal("아뇨 창 중에 뽑았다")
	}
	// 아뇨 카드가 없으면 에러
	if err := g.Nope(2); err == nil {
		t.Fatal("아뇨 카드 없이 아뇨했다")
	}
	// 낸 사람의 아뇨는 조용히 무시된다 (에러 아님)
	if err := g.Nope(0); err != nil {
		t.Fatalf("낸 사람 아뇨가 에러를 냈다: %v", err)
	}
	if g.Pending.NopeCount != 0 {
		t.Fatalf("낸 사람 아뇨가 먹혔다: %d", g.Pending.NopeCount)
	}
}

// TestEKFuturePrivate 미래 예측는 개인 큐에만 쌓이고 방송 이벤트에는 카드가 없다
func TestEKFuturePrivate(t *testing.T) {
	rng := ekRNG()
	deck := []EKCard{EKCardBomb, EKCardTaco, EKCardMelon, EKCardBeard}
	g := ekRigged(t, [][]EKCard{{EKCardFuture}, {}, {}}, deck)

	if err := g.Play(0, 0, -1, rng); err != nil {
		t.Fatalf("Play: %v", err)
	}
	g.ForcePassWindow(rng)

	priv := g.DrainPrivates()
	if len(priv) != 1 || priv[0].Seat != 0 {
		t.Fatalf("개인 이벤트 = %+v", priv)
	}
	if fmt.Sprint(priv[0].Cards) != fmt.Sprint(deck[:EKFutureCount]) {
		t.Fatalf("미래 예측 결과 = %v, want %v", priv[0].Cards, deck[:EKFutureCount])
	}
	if len(g.DrainPrivates()) != 0 {
		t.Fatal("개인 큐가 비워지지 않았다")
	}
	for _, ev := range g.DrainEvents() {
		if strings.Contains(ev.Message, ekCardName(EKCardBomb)) {
			t.Fatalf("미래 예측 결과가 방송 이벤트로 샜다: %q", ev.Message)
		}
	}
	if g.Phase != EKPhaseTurn || g.CurrentSeat != 0 {
		t.Fatalf("미래 예측 후 phase=%s current=%d — 차례는 유지된다", g.Phase, g.CurrentSeat)
	}

	// 덱이 3장 미만이면 있는 만큼만
	g2 := ekRigged(t, [][]EKCard{{EKCardFuture}, {}, {}}, []EKCard{EKCardTaco})
	if err := g2.Play(0, 0, -1, rng); err != nil {
		t.Fatalf("Play: %v", err)
	}
	g2.ForcePassWindow(rng)
	if p := g2.DrainPrivates(); len(p) != 1 || len(p[0].Cards) != 1 {
		t.Fatalf("짧은 덱 미래 예측 = %+v", p)
	}
}

// TestEKShuffle 섞기은 덱 구성을 바꾸지 않고 순서만 바꾼다
func TestEKShuffle(t *testing.T) {
	rng := ekRNG()
	deck := []EKCard{}
	for i := 0; i < 20; i++ {
		deck = append(deck, ekBaseDeck()[i])
	}
	g := ekRigged(t, [][]EKCard{{EKCardShuffle}, {}, {}}, deck)
	if err := g.Play(0, 0, -1, rng); err != nil {
		t.Fatalf("Play: %v", err)
	}
	g.ForcePassWindow(rng)
	if len(g.Deck) != len(deck) {
		t.Fatalf("섞기 후 덱 장수 = %d, want %d", len(g.Deck), len(deck))
	}
	before, after := map[EKCard]int{}, map[EKCard]int{}
	for _, c := range deck {
		before[c]++
	}
	for _, c := range g.Deck {
		after[c]++
	}
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("섞기이 덱 구성을 바꿨다")
	}
	if g.CurrentSeat != 0 || g.Phase != EKPhaseTurn {
		t.Fatalf("섞기 후 current=%d phase=%s", g.CurrentSeat, g.Phase)
	}
}

// ==================== 무작위 완주 시뮬레이션 ====================

// ekSimulate 무작위 합법 수로 한 판을 끝까지 돌린다. 교착·무한 아뇨 루프가
// 있으면 스텝 상한에 걸려 실패한다. 소요 차례 수를 돌려준다.
func ekSimulate(t *testing.T, n int, rng *rand.Rand) (turns int, winner int) {
	t.Helper()
	g := ekNewStarted(t, n, rng)

	prevSeat, prevTurns := g.CurrentSeat, g.TurnsLeft
	const maxSteps = 20000
	for step := 0; step < maxSteps; step++ {
		if g.Phase == EKPhaseGameOver {
			// 덱은 게임 중 절대 비지 않는다 (폭탄이 항상 최소 1장 남는다)
			return turns, g.WinnerSeat
		}
		g.DrainEvents()
		g.DrainPrivates()

		switch g.Phase {
		case EKPhaseTurn:
			if len(g.Deck) == 0 {
				t.Fatalf("게임 중에 덱이 비었다 (생존 %d명)", g.aliveCount())
			}
			seat := g.CurrentSeat
			if !ekSimPlay(g, seat, rng) {
				if err := g.Draw(seat, rng); err != nil {
					t.Fatalf("Draw: %v", err)
				}
			}
		case EKPhaseNopeWindow:
			ekSimNopeWindow(g, rng)
		case EKPhaseFavorWait:
			g.AutoGive(rng)
		case EKPhaseDefusePlace:
			g.AutoDefusePlace(rng)
		}

		if g.CurrentSeat != prevSeat || g.TurnsLeft != prevTurns {
			turns++
			prevSeat, prevTurns = g.CurrentSeat, g.TurnsLeft
		}
	}
	t.Fatalf("%d스텝 안에 끝나지 않았다 — 교착 (phase=%s 생존=%d)",
		maxSteps, g.Phase, g.aliveCount())
	return 0, -1
}

// ekSimPlay 40% 확률로 합법적인 카드 한 장(또는 고양이 짝)을 낸다
func ekSimPlay(g *EKGame, seat int, rng *rand.Rand) bool {
	if rng.Float64() >= 0.4 {
		return false
	}
	p := g.Players[seat]
	targets := []int{}
	for _, o := range g.Players {
		if o.Alive && o.Seat != seat {
			targets = append(targets, o.Seat)
		}
	}
	// 고양이 짝
	byKind := map[EKCard][]int{}
	for i, c := range p.Hand {
		if ekIsCat(c) {
			byKind[c] = append(byKind[c], i)
		}
	}
	if len(targets) > 0 {
		for _, idx := range byKind {
			if len(idx) >= EKPairSize && rng.Float64() < 0.5 {
				return g.PlayPair(seat, idx[:EKPairSize], targets[rng.Intn(len(targets))], rng) == nil
			}
		}
	}
	playable := []int{}
	for i, c := range p.Hand {
		if c == EKCardBomb || c == EKCardDefuse || c == EKCardNope || ekIsCat(c) {
			continue
		}
		if c == EKCardFavor && len(targets) == 0 {
			continue
		}
		playable = append(playable, i)
	}
	if len(playable) == 0 {
		return false
	}
	i := playable[rng.Intn(len(playable))]
	target := -1
	if p.Hand[i] == EKCardFavor {
		target = targets[rng.Intn(len(targets))]
	}
	return g.Play(seat, i, target, rng) == nil
}

// ekSimNopeWindow 응답자들이 25% 확률로 아뇨를 겹치고 나머지는 통과한다
func ekSimNopeWindow(g *EKGame, rng *rand.Rand) {
	for _, seat := range g.nopeResponders() {
		if g.Players[seat].HasCard(EKCardNope) >= 0 && rng.Float64() < 0.25 {
			g.Nope(seat)
			return // 창이 다시 열렸다 — 다음 루프에서 새 응답자를 본다
		}
	}
	for _, seat := range append([]int{}, g.nopeResponders()...) {
		g.Pass(seat, rng)
		if g.Phase != EKPhaseNopeWindow {
			return
		}
	}
}

// TestEKRandomPlayAlwaysEnds 인원별 무작위 대국 완주 — 아뇨를 마구 겹쳐도
// 카드가 유한해 창이 닫히고, 폭탄이 인원-1 장이라 반드시 1명만 남는다.
func TestEKRandomPlayAlwaysEnds(t *testing.T) {
	rng := ekRNG()
	for n := EKMinPlayers; n <= EKMaxPlayers; n++ {
		total, games := 0, 20
		for i := 0; i < games; i++ {
			turns, winner := ekSimulate(t, n, rng)
			if winner < 0 || winner >= n {
				t.Fatalf("%d인전 승자 = %d", n, winner)
			}
			total += turns
		}
		t.Logf("%d인 무작위 %d판 완주 — 평균 %.1f차례", n, games, float64(total)/float64(games))
	}
}
