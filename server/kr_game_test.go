package server

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// krNewTestGame n 인 게임을 시작 상태로 만든다 (손패·지목권은 각 테스트가
// 결정적으로 덮어쓴다)
func krNewTestGame(t *testing.T, n int) (*KRGame, *rand.Rand) {
	t.Helper()
	g := NewKRGame("kr-test")
	for i := 0; i < n; i++ {
		if _, err := g.AddPlayer(fmt.Sprintf("P%d", i)); err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
	}
	rng := rand.New(rand.NewSource(11))
	if err := g.Start(rng); err != nil {
		t.Fatalf("Start: %v", err)
	}
	g.DrainEvents()
	return g, rng
}

// krEventText 이벤트 큐를 비우고 문구를 이어붙인다 (문구 검증용)
func krEventText(g *KRGame) string {
	msgs := []string{}
	for _, ev := range g.DrainEvents() {
		msgs = append(msgs, ev.Kind+":"+ev.Message)
	}
	return strings.Join(msgs, "\n")
}

// krFillHands 전 좌석의 손패를 같은 카드로 채운다 (결정적 시나리오용)
func krFillHands(g *KRGame, card KRCard) {
	per := krHandSize(g.Round)
	for _, p := range g.Players {
		hand := make([]KRCard, per)
		for i := range hand {
			hand[i] = card
		}
		p.Hand = hand
	}
}

// krCountHands 전 좌석의 미공개 카드 합
func krCountHands(g *KRGame) int {
	n := 0
	for _, p := range g.Players {
		n += len(p.Hand)
	}
	return n
}

// krPlayRoundSafely 이번 라운드의 남은 공개를 전부 소화한다 (인덱스 0 고정)
func krPlayRoundSafely(t *testing.T, g *KRGame) {
	t.Helper()
	guard := 0
	for g.Phase == KRPhaseRevealing {
		guard++
		if guard > 64 {
			t.Fatal("라운드가 끝나지 않는다 — 공개 카운트 이상")
		}
		cur := g.CurrentSeat
		target := -1
		for _, p := range g.Players {
			if p.Seat != cur && len(p.Hand) > 0 {
				target = p.Seat
				break
			}
		}
		if target < 0 {
			t.Fatalf("지목 가능한 대상이 없다 (cur=%d)", cur)
		}
		if err := g.Point(cur, target, 0); err != nil {
			t.Fatalf("Point(%d→%d): %v", cur, target, err)
		}
	}
}

// TestKRRolePoolAndDeck 인원별 역할 풀·덱 구성·1라운드 배분.
// 풀에서 인원수만큼만 뽑으므로 실제 진영 구성은 판마다 다르다 (6인만 4:2 고정).
func TestKRRolePoolAndDeck(t *testing.T) {
	want := map[int]KRRolePool{
		4: {Expedition: 3, Skeleton: 2},
		5: {Expedition: 4, Skeleton: 2},
		6: {Expedition: 4, Skeleton: 2},
		7: {Expedition: 5, Skeleton: 3},
		8: {Expedition: 6, Skeleton: 3},
	}
	for n := KRMinPlayers; n <= KRMaxPlayers; n++ {
		pool := krRolePoolFor(n)
		if pool != want[n] {
			t.Fatalf("%d인 풀 = %+v, want %+v", n, pool, want[n])
		}
		if pool.Expedition+pool.Skeleton < n {
			t.Fatalf("%d인 풀 합(%d)이 인원보다 적다", n, pool.Expedition+pool.Skeleton)
		}

		// 덱은 5N장 — 보물 N·크라켄 1·꽝 4N-1
		deck := krBuildDeck(n)
		if len(deck) != 5*n {
			t.Fatalf("%d인 덱 = %d장, want %d", n, len(deck), 5*n)
		}
		count := map[KRCard]int{}
		for _, c := range deck {
			count[c]++
		}
		if count[KRCardTreasure] != n || count[KRCardKraken] != 1 || count[KRCardBlank] != 4*n-1 {
			t.Fatalf("%d인 덱 구성 = %v", n, count)
		}

		// 1라운드 배분 — 각자 5장, 덱 전량 소진
		g, _ := krNewTestGame(t, n)
		if g.Phase != KRPhaseRevealing || g.Round != 1 || g.RevealsLeft != n {
			t.Fatalf("%d인 시작 상태: phase=%s round=%d revealsLeft=%d",
				n, g.Phase, g.Round, g.RevealsLeft)
		}
		if g.CurrentSeat < 0 || g.CurrentSeat >= n {
			t.Fatalf("%d인 시작 지목권자 = %d", n, g.CurrentSeat)
		}
		if krCountHands(g) != 5*n {
			t.Fatalf("%d인 1라운드 배분 총합 = %d, want %d", n, krCountHands(g), 5*n)
		}
		dealt := map[KRCard]int{}
		for _, p := range g.Players {
			if len(p.Hand) != krHandSize(1) {
				t.Fatalf("%d인 seat%d 손패 = %d장, want %d", n, p.Seat, len(p.Hand), krHandSize(1))
			}
			if len(p.RevealedThisRound) != 0 || p.Claim != nil {
				t.Fatalf("%d인 seat%d 시작 상태 오염: %+v", n, p.Seat, p)
			}
			for _, c := range p.Hand {
				dealt[c]++
			}
		}
		if dealt[KRCardTreasure] != n || dealt[KRCardKraken] != 1 {
			t.Fatalf("%d인 배분된 카드 구성 = %v", n, dealt)
		}

		// 역할은 풀 안에서만 나오고, 배정 수는 인원수와 같다
		roles := map[KRRole]int{}
		for _, p := range g.Players {
			roles[p.Role]++
		}
		if roles[KRRoleExpedition]+roles[KRRoleSkeleton] != n {
			t.Fatalf("%d인 역할 배정 = %v", n, roles)
		}
		if roles[KRRoleExpedition] > pool.Expedition || roles[KRRoleSkeleton] > pool.Skeleton {
			t.Fatalf("%d인 역할이 풀을 넘었다: %v vs %+v", n, roles, pool)
		}
		if g.Pool != pool {
			t.Fatalf("%d인 게임 풀 = %+v", n, g.Pool)
		}
	}
}

// TestKRPointValidation 지목 검증 — 차례·자기 자신·범위·빈 손패 좌석.
// 지목권은 방금 공개된 카드의 주인에게 넘어간다.
func TestKRPointValidation(t *testing.T) {
	g, _ := krNewTestGame(t, 4)
	krFillHands(g, KRCardBlank)
	g.CurrentSeat = 0

	if err := g.Point(1, 2, 0); err == nil {
		t.Fatal("차례 아닌 좌석의 지목이 허용됐다")
	}
	if err := g.Point(0, 0, 0); err == nil {
		t.Fatal("자기 자신 지목이 허용됐다")
	}
	if err := g.Point(0, 9, 0); err == nil {
		t.Fatal("범위 밖 좌석 지목이 허용됐다")
	}
	if err := g.Point(0, 1, 99); err == nil {
		t.Fatal("범위 밖 카드 인덱스가 허용됐다")
	}

	// 미공개 카드가 0장인 좌석은 지목 대상이 될 수 없다
	g.Players[3].Hand = []KRCard{}
	if err := g.Point(0, 3, 0); err == nil {
		t.Fatal("빈 손패 좌석 지목이 허용됐다")
	}

	// 정상 지목 — 카드가 중앙으로 빠지고 지목권이 카드 주인에게 넘어간다
	before := len(g.Players[1].Hand)
	if err := g.Point(0, 1, 0); err != nil {
		t.Fatalf("Point: %v", err)
	}
	if len(g.Players[1].Hand) != before-1 {
		t.Fatalf("공개 후 손패 = %d, want %d", len(g.Players[1].Hand), before-1)
	}
	if len(g.Players[1].RevealedThisRound) != 1 {
		t.Fatalf("revealedThisRound = %v", g.Players[1].RevealedThisRound)
	}
	if g.CurrentSeat != 1 {
		t.Fatalf("지목권 이양 실패: currentSeat = %d, want 1", g.CurrentSeat)
	}
	if g.RevealsLeft != 3 {
		t.Fatalf("revealsLeft = %d, want 3", g.RevealsLeft)
	}
	r := g.Reveal
	if r == nil || r.Seat != 1 || r.BySeat != 0 || r.Card != KRCardBlank {
		t.Fatalf("lastReveal = %+v", r)
	}
	if !strings.Contains(krEventText(g), "꽝") {
		t.Fatal("공개 이벤트 문구에 카드 표기 부재")
	}

	// 빈 손패 좌석도 지목권은 받을 수 있다 (받으면 남을 지목한다)
	g.CurrentSeat = 3
	if err := g.Point(3, 0, 0); err != nil {
		t.Fatalf("빈 손패 좌석의 지목이 거부됐다: %v", err)
	}
}

// TestKRKrakenEndsGame 크라켄 공개 즉시 해골 승 — 라운드가 남아 있어도 종료
func TestKRKrakenEndsGame(t *testing.T) {
	g, _ := krNewTestGame(t, 4)
	krFillHands(g, KRCardBlank)
	g.FoundTreasure, g.FoundKraken = 0, 0
	g.Players[1].Hand[0] = KRCardKraken
	g.CurrentSeat = 0

	if err := g.Point(0, 1, 0); err != nil {
		t.Fatalf("Point: %v", err)
	}
	if g.Phase != KRPhaseGameOver {
		t.Fatalf("phase = %s, want game_over", g.Phase)
	}
	if g.Result == nil || g.Result.Winner != string(KRRoleSkeleton) || g.Result.Reason != "kraken" {
		t.Fatalf("result = %+v, want skeleton/kraken", g.Result)
	}
	if g.FoundKraken != 1 {
		t.Fatalf("found.kraken = %d", g.FoundKraken)
	}
	if g.Round != 1 {
		t.Fatalf("1라운드 중 종료여야 한다: round=%d", g.Round)
	}
	if !strings.Contains(krEventText(g), "크라켄") {
		t.Fatal("크라켄 공개 이벤트 부재")
	}
	// 종료 뒤 행동은 전부 거부
	if err := g.Point(1, 2, 0); err == nil {
		t.Fatal("종료 뒤 지목이 허용됐다")
	}
	if err := g.SubmitClaim(1, 0, 0); err == nil {
		t.Fatal("종료 뒤 선언이 허용됐다")
	}
}

// TestKRAllTreasureWin 보물 N장 전부 공개 즉시 탐험대 승
func TestKRAllTreasureWin(t *testing.T) {
	n := 4
	g, _ := krNewTestGame(t, n)
	krFillHands(g, KRCardBlank)
	g.FoundTreasure, g.FoundKraken = 0, 0
	for _, p := range g.Players {
		p.Hand[0] = KRCardTreasure
	}
	g.CurrentSeat = 0

	// 0→1→2→3→0 순으로 각 좌석의 보물을 연다 (정확히 N번 공개)
	for _, target := range []int{1, 2, 3, 0} {
		if err := g.Point(g.CurrentSeat, target, 0); err != nil {
			t.Fatalf("Point(→%d): %v", target, err)
		}
	}
	if g.Phase != KRPhaseGameOver {
		t.Fatalf("phase = %s, want game_over", g.Phase)
	}
	if g.Result == nil || g.Result.Winner != string(KRRoleExpedition) ||
		g.Result.Reason != "all_treasure" {
		t.Fatalf("result = %+v, want expedition/all_treasure", g.Result)
	}
	if g.FoundTreasure != n {
		t.Fatalf("found.treasure = %d, want %d", g.FoundTreasure, n)
	}
	if !strings.Contains(g.Result.Message, "탐험대") {
		t.Fatalf("결과 문구 = %q", g.Result.Message)
	}
}

// TestKRRoundRedistribution 라운드 재배분 — 라운드마다 정확히 N번 공개하고,
// 남은 카드를 전부 회수해 (6-r)장씩 다시 나눈다. 이번 라운드 공개분과 선언은
// 초기화되고, 지목권은 마지막 공개 카드의 주인이 이어받는다.
func TestKRRoundRedistribution(t *testing.T) {
	n := 5
	g, rng := krNewTestGame(t, n)
	krFillHands(g, KRCardBlank) // 꽝만 — 중도 종료 없이 4라운드를 다 쓴다

	for round := 1; round <= KRRounds; round++ {
		if g.Round != round {
			t.Fatalf("round = %d, want %d", g.Round, round)
		}
		if got, want := krCountHands(g), n*krHandSize(round); got != want {
			t.Fatalf("%d라운드 배분 총합 = %d, want %d", round, got, want)
		}
		for _, p := range g.Players {
			if len(p.Hand) != krHandSize(round) {
				t.Fatalf("%d라운드 seat%d 손패 = %d장, want %d",
					round, p.Seat, len(p.Hand), krHandSize(round))
			}
			if len(p.RevealedThisRound) != 0 {
				t.Fatalf("%d라운드 시작 시 seat%d 공개분 미초기화: %v",
					round, p.Seat, p.RevealedThisRound)
			}
			if p.Claim != nil {
				t.Fatalf("%d라운드 시작 시 seat%d 선언 미초기화: %+v", round, p.Seat, p.Claim)
			}
		}

		// 라운드 중 선언을 하나 걸어 둔다 — 다음 라운드에 초기화돼야 한다
		if err := g.SubmitClaim(g.CurrentSeat, 1, 0); err != nil {
			t.Fatalf("SubmitClaim: %v", err)
		}

		krPlayRoundSafely(t, g)

		revealed := 0
		for _, p := range g.Players {
			revealed += len(p.RevealedThisRound)
		}
		if revealed != n {
			t.Fatalf("%d라운드 공개 수 = %d, want %d", round, revealed, n)
		}
		if g.RevealsLeft != 0 {
			t.Fatalf("%d라운드 revealsLeft = %d", round, g.RevealsLeft)
		}

		if round < KRRounds {
			if g.Phase != KRPhaseRoundEnd {
				t.Fatalf("%d라운드 종료 phase = %s, want round_end", round, g.Phase)
			}
			lastOwner := g.Reveal.Seat
			g.DrainEvents()
			g.NextRound(rng)
			if g.CurrentSeat != lastOwner {
				t.Fatalf("다음 라운드 선 = %d, want %d (마지막 공개 카드 주인)",
					g.CurrentSeat, lastOwner)
			}
			if g.RevealsLeft != n {
				t.Fatalf("%d라운드 revealsLeft = %d, want %d", g.Round, g.RevealsLeft, n)
			}
			if !strings.Contains(krEventText(g), "라운드") {
				t.Fatal("라운드 시작 이벤트 부재")
			}
			continue
		}

		// 4라운드 소진 — 해골 승 (시간 초과). 미공개 N장이 남는다.
		if g.Phase != KRPhaseGameOver {
			t.Fatalf("4라운드 종료 phase = %s, want game_over", g.Phase)
		}
		if g.Result == nil || g.Result.Winner != string(KRRoleSkeleton) ||
			g.Result.Reason != "timeout" {
			t.Fatalf("result = %+v, want skeleton/timeout", g.Result)
		}
		if krCountHands(g) != n {
			t.Fatalf("종료 후 미공개 카드 = %d장, want %d", krCountHands(g), n)
		}
	}

	// NextRound 는 round_end 에서만 동작한다 (종료 뒤 호출은 무해)
	g.NextRound(rng)
	if g.Round != KRRounds {
		t.Fatalf("종료 뒤 NextRound 가 라운드를 넘겼다: %d", g.Round)
	}
}

// TestKRClaim 선언 — 거짓 허용, 남은 장수 초과만 거부, 라운드 재배분 때 초기화
func TestKRClaim(t *testing.T) {
	g, rng := krNewTestGame(t, 4)
	krFillHands(g, KRCardBlank) // 실제로는 전부 꽝
	g.CurrentSeat = 0

	// 거짓 선언 — 꽝만 들었는데 보물 2·크라켄 1 이라 우긴다 (규칙상 자유)
	if err := g.SubmitClaim(0, 2, 1); err != nil {
		t.Fatalf("거짓 선언이 거부됐다: %v", err)
	}
	c := g.Players[0].Claim
	if c == nil || c.Treasure != 2 || c.Kraken != 1 {
		t.Fatalf("claim = %+v", c)
	}
	if !strings.Contains(krEventText(g), "선언") {
		t.Fatal("선언 이벤트 부재")
	}

	// 남은 장수(5장)보다 많은 선언은 거부
	if err := g.SubmitClaim(0, 5, 1); err == nil {
		t.Fatal("남은 장수를 넘는 선언이 허용됐다")
	}
	if err := g.SubmitClaim(0, -1, 0); err == nil {
		t.Fatal("음수 선언이 허용됐다")
	}
	if err := g.SubmitClaim(0, 0, 2); err == nil {
		t.Fatal("크라켄 2 선언이 허용됐다")
	}
	// 덮어쓰기는 허용 (마음이 바뀔 수 있다)
	if err := g.SubmitClaim(0, 0, 0); err != nil {
		t.Fatalf("재선언이 거부됐다: %v", err)
	}
	if g.Players[0].Claim.Treasure != 0 || g.Players[0].Claim.Kraken != 0 {
		t.Fatalf("재선언 반영 실패: %+v", g.Players[0].Claim)
	}

	// 라운드 재배분 때 초기화
	krPlayRoundSafely(t, g)
	if g.Phase != KRPhaseRoundEnd {
		t.Fatalf("phase = %s, want round_end", g.Phase)
	}
	if err := g.SubmitClaim(0, 0, 0); err != nil {
		t.Fatalf("라운드 정산 중 선언이 거부됐다: %v", err)
	}
	g.NextRound(rng)
	for _, p := range g.Players {
		if p.Claim != nil {
			t.Fatalf("재배분 후 seat%d 선언 미초기화: %+v", p.Seat, p.Claim)
		}
	}
}

// TestKRForcePoint AFK 자동 지목 — 무작위 대상·무작위 카드로 라운드가 진행된다
func TestKRForcePoint(t *testing.T) {
	g, rng := krNewTestGame(t, 4)
	krFillHands(g, KRCardBlank)
	g.CurrentSeat = 2

	before := krCountHands(g)
	g.ForcePoint(rng)
	if krCountHands(g) != before-1 {
		t.Fatalf("자동 지목 후 미공개 = %d, want %d", krCountHands(g), before-1)
	}
	if g.Reveal == nil || g.Reveal.BySeat != 2 || g.Reveal.Seat == 2 {
		t.Fatalf("자동 지목 lastReveal = %+v", g.Reveal)
	}
	if g.CurrentSeat != g.Reveal.Seat {
		t.Fatalf("자동 지목 후 지목권 = %d, want %d", g.CurrentSeat, g.Reveal.Seat)
	}

	// 라운드 전체를 자동 지목만으로 소화해도 정확히 N번에 끝난다
	for g.Phase == KRPhaseRevealing {
		g.ForcePoint(rng)
	}
	if g.Phase != KRPhaseRoundEnd {
		t.Fatalf("자동 지목만으로 라운드가 안 끝났다: %s", g.Phase)
	}
	if krCountHands(g) != 4*krHandSize(1)-4 {
		t.Fatalf("자동 지목 후 미공개 총합 = %d", krCountHands(g))
	}
}
