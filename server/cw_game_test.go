package server

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// cwNewTestGame n 인 게임을 시작 상태로 만든다 (손패·임무는 각 테스트가
// 결정적으로 덮어쓴다)
func cwNewTestGame(t *testing.T, n int) (*CWGame, *rand.Rand) {
	t.Helper()
	g := NewCWGame("cw-test")
	for i := 0; i < n; i++ {
		if _, err := g.AddPlayer(fmt.Sprintf("P%d", i)); err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
	}
	rng := rand.New(rand.NewSource(7))
	if err := g.Start(rng); err != nil {
		t.Fatalf("Start: %v", err)
	}
	g.DrainEvents()
	return g, rng
}

// cwEventText 이벤트 큐를 비우고 문구를 이어붙인다 (문구 검증용)
func cwEventText(g *CWGame) string {
	msgs := []string{}
	for _, ev := range g.DrainEvents() {
		msgs = append(msgs, ev.Kind+":"+ev.Message)
	}
	return strings.Join(msgs, "\n")
}

// cwSetHands 좌석별 손패를 통째로 덮어쓰고 리드부터 트릭을 다시 연다
func cwSetHands(t *testing.T, g *CWGame, leader int, hands [][]CWCard) {
	t.Helper()
	if len(hands) != len(g.Players) {
		t.Fatalf("손패 %d벌 vs 좌석 %d개", len(hands), len(g.Players))
	}
	for i, h := range hands {
		g.Players[i].Hand = append([]CWCard{}, h...)
		g.Players[i].TokenLeft = CWTokenPerMission
		g.Players[i].Revealed = nil
	}
	g.Leader = leader
	g.beginTrick()
}

// cwCountHands 전 좌석의 남은 카드 합
func cwCountHands(g *CWGame) int {
	n := 0
	for _, p := range g.Players {
		n += len(p.Hand)
	}
	return n
}

// cwPlayAll 좌석 순서대로 index 0 카드를 낸다 (트릭 하나를 소화)
func cwPlayAll(t *testing.T, g *CWGame, seats ...int) {
	t.Helper()
	for _, seat := range seats {
		if err := g.Play(seat, 0); err != nil {
			t.Fatalf("Play(seat%d): %v", seat, err)
		}
	}
}

// TestCWDeckAndDeal 덱 40장 구성·균등 배분(나머지는 앞자리부터)·사령관(로켓 4)·
// 임무 배정. 임무 수는 임무 단계와 같고 로켓은 임무가 되지 않는다.
func TestCWDeckAndDeal(t *testing.T) {
	deck := cwBuildDeck()
	if len(deck) != CWDeckSize {
		t.Fatalf("덱 = %d장, want %d", len(deck), CWDeckSize)
	}
	count := map[CWSuit]int{}
	for _, c := range deck {
		count[c.Suit]++
	}
	for _, suit := range cwColorSuits {
		if count[suit] != CWColorMaxRank {
			t.Fatalf("%s = %d장, want %d", suit, count[suit], CWColorMaxRank)
		}
	}
	if count[CWSuitRocket] != CWRocketMaxRank {
		t.Fatalf("로켓 = %d장, want %d", count[CWSuitRocket], CWRocketMaxRank)
	}
	for _, c := range cwColorDeck() {
		if c.Suit == CWSuitRocket {
			t.Fatal("임무 후보에 로켓이 섞였다")
		}
	}

	for n := CWMinPlayers; n <= CWMaxPlayers; n++ {
		g, _ := cwNewTestGame(t, n)
		if g.Phase != CWPhasePlaying || g.Mission != 1 || g.MaxMission != CWDefaultMaxMission {
			t.Fatalf("%d인 시작 상태: phase=%s mission=%d/%d",
				n, g.Phase, g.Mission, g.MaxMission)
		}

		base, extra := CWDeckSize/n, CWDeckSize%n
		seen := map[CWCard]bool{}
		for i, p := range g.Players {
			want := base
			if i < extra { // 나머지는 앞자리 좌석부터 1장씩 더
				want++
			}
			if len(p.Hand) != want {
				t.Fatalf("%d인 seat%d 손패 = %d장, want %d", n, i, len(p.Hand), want)
			}
			if p.TokenLeft != CWTokenPerMission || p.Revealed != nil {
				t.Fatalf("%d인 seat%d 소통 상태 오염: token=%d revealed=%+v",
					n, i, p.TokenLeft, p.Revealed)
			}
			for _, c := range p.Hand {
				if seen[c] {
					t.Fatalf("%d인 배분에 중복 카드: %+v", n, c)
				}
				seen[c] = true
			}
		}
		if len(seen) != CWDeckSize || cwCountHands(g) != CWDeckSize {
			t.Fatalf("%d인 배분 총합 = %d장 (고유 %d) want %d",
				n, cwCountHands(g), len(seen), CWDeckSize)
		}

		// 사령관 = 로켓 4 보유자, 첫 리드도 사령관
		hasRocket4 := false
		for _, c := range g.Players[g.CommanderSeat].Hand {
			if c.Suit == CWSuitRocket && c.Rank == CWRocketMaxRank {
				hasRocket4 = true
			}
		}
		if !hasRocket4 {
			t.Fatalf("%d인 사령관 seat%d 가 로켓 4를 들고 있지 않다", n, g.CommanderSeat)
		}
		if g.CurrentSeat != g.CommanderSeat || g.Leader != g.CommanderSeat {
			t.Fatalf("%d인 첫 리드 = %d, want 사령관 %d", n, g.CurrentSeat, g.CommanderSeat)
		}

		// 임무 — 단계 수만큼, 색 숫자 카드만, 좌석 범위 안
		if len(g.Tasks) != g.Mission {
			t.Fatalf("%d인 임무 수 = %d, want %d", n, len(g.Tasks), g.Mission)
		}
		for _, task := range g.Tasks {
			if task.Suit == CWSuitRocket {
				t.Fatalf("%d인 임무에 로켓: %+v", n, task)
			}
			if task.Rank < 1 || task.Rank > CWColorMaxRank {
				t.Fatalf("%d인 임무 숫자 이상: %+v", n, task)
			}
			if task.Seat < 0 || task.Seat >= n || task.Done {
				t.Fatalf("%d인 임무 배정 이상: %+v", n, task)
			}
		}
	}

	// 임무 단계가 오르면 임무 카드도 늘고 좌석에 골고루 배정된다
	rng := rand.New(rand.NewSource(3))
	tasks := cwPickTasks(rng, 5, 4)
	if len(tasks) != 5 {
		t.Fatalf("임무 %d개, want 5", len(tasks))
	}
	seats := map[int]bool{}
	for _, task := range tasks {
		seats[task.Seat] = true
	}
	if len(seats) != 4 {
		t.Fatalf("임무 5개가 좌석 %d곳에만 배정됐다", len(seats))
	}
}

// TestCWFollowSuitAndTrump 따라내기 의무와 승자 판정 — 로켓이 있으면 로켓
// 최고, 없으면 리드 색 최고. 승자가 다음 리드를 잡는다.
func TestCWFollowSuitAndTrump(t *testing.T) {
	g, _ := cwNewTestGame(t, 4)
	// 아무도 들지 않은 카드를 임무로 둬 트릭 판정이 조기 종료되지 않게 한다
	g.Tasks = []CWTask{{Suit: CWSuitYellow, Rank: 9, Seat: 0}}
	cwSetHands(t, g, 0, [][]CWCard{
		{{Suit: CWSuitBlue, Rank: 5}, {Suit: CWSuitGreen, Rank: 2}},
		{{Suit: CWSuitBlue, Rank: 9}, {Suit: CWSuitGreen, Rank: 3}},
		{{Suit: CWSuitRocket, Rank: 1}, {Suit: CWSuitGreen, Rank: 4}},
		{{Suit: CWSuitBlue, Rank: 1}, {Suit: CWSuitGreen, Rank: 5}},
	})

	if g.CurrentSeat != 0 || g.LeadSuit != "" || len(g.Trick) != 0 {
		t.Fatalf("트릭 시작 상태: current=%d lead=%q trick=%v", g.CurrentSeat, g.LeadSuit, g.Trick)
	}
	if err := g.Play(1, 0); err == nil {
		t.Fatal("차례가 아닌 좌석의 제출이 통과했다")
	}
	if err := g.Play(0, 0); err != nil { // 파랑 5 리드
		t.Fatalf("Play: %v", err)
	}
	if g.LeadSuit != CWSuitBlue || g.CurrentSeat != 1 {
		t.Fatalf("리드 반영 실패: lead=%s current=%d", g.LeadSuit, g.CurrentSeat)
	}

	// 파랑을 들고 있으면 초록을 낼 수 없다
	if err := g.Play(1, 1); err == nil {
		t.Fatal("따라내기 의무를 어긴 제출이 통과했다")
	} else if !strings.Contains(err.Error(), "파랑") {
		t.Fatalf("따라내기 에러 문구 = %q", err.Error())
	}
	if err := g.Play(1, 0); err != nil { // 파랑 9
		t.Fatalf("Play(1): %v", err)
	}
	// 파랑이 없는 좌석은 아무거나 — 로켓을 낸다
	if err := g.Play(2, 0); err != nil {
		t.Fatalf("Play(2): %v", err)
	}
	if err := g.Play(3, 0); err != nil { // 파랑 1
		t.Fatalf("Play(3): %v", err)
	}

	if g.LastTrick == nil || g.LastTrick.WinnerSeat != 2 {
		t.Fatalf("로켓이 트릭을 못 가져갔다: %+v", g.LastTrick)
	}
	if len(g.LastTrick.Cards) != 4 {
		t.Fatalf("lastTrick 카드 = %d장", len(g.LastTrick.Cards))
	}
	if g.Leader != 2 || g.CurrentSeat != 2 {
		t.Fatalf("승자가 다음 리드를 못 잡았다: leader=%d current=%d", g.Leader, g.CurrentSeat)
	}
	if len(g.Trick) != 0 || g.LeadSuit != "" {
		t.Fatalf("새 트릭이 안 열렸다: trick=%v lead=%q", g.Trick, g.LeadSuit)
	}
	if !strings.Contains(cwEventText(g), "트릭") {
		t.Fatal("트릭 정산 이벤트 부재")
	}

	// 로켓 없는 트릭은 리드 색 최고가 이긴다 (2→3→0→1 순, 초록 5가 최고)
	cwPlayAll(t, g, 2, 3, 0, 1)
	if g.LastTrick.WinnerSeat != 3 {
		t.Fatalf("리드 색 최고가 못 이겼다: %+v", g.LastTrick)
	}

	// 카드가 바닥났는데 임무가 남았으므로 실패로 끝난다
	if g.Phase != CWPhaseGameOver {
		t.Fatalf("카드 소진 후 phase = %s", g.Phase)
	}
	if g.Result == nil || g.Result.Cleared || g.Result.FailedReason != "out_of_cards" {
		t.Fatalf("result = %+v, want out_of_cards", g.Result)
	}
}

// TestCWTrickWinnerTable 승자 판정 표 — 로켓끼리는 숫자 큰 쪽,
// 리드를 못 따른 색은 아무리 커도 승패에 관여하지 않는다.
func TestCWTrickWinnerTable(t *testing.T) {
	cases := []struct {
		name  string
		lead  CWSuit
		trick []CWTrickCard
		want  int
	}{
		{"리드 색 최고", CWSuitBlue, []CWTrickCard{
			{Seat: 0, Card: CWCard{CWSuitBlue, 3}},
			{Seat: 1, Card: CWCard{CWSuitBlue, 8}},
			{Seat: 2, Card: CWCard{CWSuitGreen, 9}},
		}, 1},
		{"로켓 트럼프", CWSuitBlue, []CWTrickCard{
			{Seat: 0, Card: CWCard{CWSuitBlue, 9}},
			{Seat: 1, Card: CWCard{CWSuitRocket, 1}},
			{Seat: 2, Card: CWCard{CWSuitBlue, 8}},
		}, 1},
		{"로켓끼리는 숫자", CWSuitRocket, []CWTrickCard{
			{Seat: 0, Card: CWCard{CWSuitRocket, 2}},
			{Seat: 1, Card: CWCard{CWSuitRocket, 4}},
			{Seat: 2, Card: CWCard{CWSuitRocket, 3}},
		}, 1},
		{"리드 미추종은 무력", CWSuitPink, []CWTrickCard{
			{Seat: 0, Card: CWCard{CWSuitPink, 1}},
			{Seat: 1, Card: CWCard{CWSuitYellow, 9}},
			{Seat: 2, Card: CWCard{CWSuitGreen, 9}},
		}, 0},
	}
	for _, tc := range cases {
		if got := cwTrickWinner(tc.trick, tc.lead); got != tc.want {
			t.Fatalf("%s: 승자 = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestCWTaskSuccess 임무 카드를 담당자가 가져가면 완료 — 마지막 임무 카드를
// 마치면 다음 임무 단계로 넘어간다 (round_end → NextMission).
func TestCWTaskSuccess(t *testing.T) {
	g, rng := cwNewTestGame(t, 3)
	g.Tasks = []CWTask{{Suit: CWSuitBlue, Rank: 5, Seat: 1}}
	cwSetHands(t, g, 0, [][]CWCard{
		{{Suit: CWSuitBlue, Rank: 5}, {Suit: CWSuitGreen, Rank: 1}},
		{{Suit: CWSuitBlue, Rank: 9}, {Suit: CWSuitGreen, Rank: 2}},
		{{Suit: CWSuitBlue, Rank: 1}, {Suit: CWSuitGreen, Rank: 3}},
	})

	cwPlayAll(t, g, 0, 1, 2)
	if !g.Tasks[0].Done {
		t.Fatalf("임무 미완료: %+v", g.Tasks[0])
	}
	if g.Phase != CWPhaseRoundEnd {
		t.Fatalf("임무 성공 후 phase = %s, want round_end", g.Phase)
	}
	if g.Result != nil {
		t.Fatalf("중간 임무 성공에 result 가 실렸다: %+v", g.Result)
	}
	text := cwEventText(g)
	if !strings.Contains(text, "임무 완료") || !strings.Contains(text, "성공") {
		t.Fatalf("임무 성공 이벤트 문구 = %q", text)
	}

	// 다음 임무 — 임무 카드가 하나 늘고 40장이 다시 배분된다
	g.NextMission(rng)
	if g.Mission != 2 || len(g.Tasks) != 2 || g.Phase != CWPhasePlaying {
		t.Fatalf("다음 임무 상태: mission=%d tasks=%d phase=%s",
			g.Mission, len(g.Tasks), g.Phase)
	}
	if cwCountHands(g) != CWDeckSize {
		t.Fatalf("다음 임무 배분 = %d장", cwCountHands(g))
	}
	for _, p := range g.Players {
		if p.TokenLeft != CWTokenPerMission || p.Revealed != nil {
			t.Fatalf("seat%d 소통 토큰 미초기화: token=%d revealed=%+v",
				p.Seat, p.TokenLeft, p.Revealed)
		}
	}
	if g.CurrentSeat != g.CommanderSeat {
		t.Fatalf("다음 임무 첫 리드 = %d, want 사령관 %d", g.CurrentSeat, g.CommanderSeat)
	}

	// round_end 가 아니면 NextMission 은 아무 일도 하지 않는다
	before := g.Mission
	g.NextMission(rng)
	if g.Mission != before {
		t.Fatalf("playing 중 NextMission 이 임무를 넘겼다: %d", g.Mission)
	}
}

// TestCWTaskWrongWinner 임무 카드를 담당자가 아닌 사람이 가져가면 즉시 실패
func TestCWTaskWrongWinner(t *testing.T) {
	g, _ := cwNewTestGame(t, 3)
	g.Tasks = []CWTask{{Suit: CWSuitBlue, Rank: 5, Seat: 2}}
	cwSetHands(t, g, 0, [][]CWCard{
		{{Suit: CWSuitBlue, Rank: 5}, {Suit: CWSuitGreen, Rank: 1}},
		{{Suit: CWSuitBlue, Rank: 9}, {Suit: CWSuitGreen, Rank: 2}},
		{{Suit: CWSuitBlue, Rank: 1}, {Suit: CWSuitGreen, Rank: 3}},
	})

	cwPlayAll(t, g, 0, 1, 2)
	if g.Phase != CWPhaseGameOver {
		t.Fatalf("phase = %s, want game_over", g.Phase)
	}
	if g.Result == nil || g.Result.Cleared || g.Result.FailedReason != "wrong_winner" {
		t.Fatalf("result = %+v, want wrong_winner", g.Result)
	}
	if g.Result.Mission != 1 {
		t.Fatalf("실패 임무 단계 = %d", g.Result.Mission)
	}
	if g.Tasks[0].Done {
		t.Fatal("실패한 임무가 완료로 표시됐다")
	}
	if !strings.Contains(cwEventText(g), "임무 실패") {
		t.Fatal("임무 실패 이벤트 부재")
	}

	// 종료 뒤 행동은 전부 거부
	if err := g.Play(0, 0); err == nil {
		t.Fatal("종료 뒤 제출이 허용됐다")
	}
	if err := g.Communicate(0, 0, CWHintOnly); err == nil {
		t.Fatal("종료 뒤 소통이 허용됐다")
	}
}

// TestCWClearAllMissions 마지막 임무를 마치면 클리어로 게임이 끝난다
func TestCWClearAllMissions(t *testing.T) {
	g, _ := cwNewTestGame(t, 3)
	g.MaxMission = 1
	g.Tasks = []CWTask{{Suit: CWSuitPink, Rank: 7, Seat: 0}}
	cwSetHands(t, g, 0, [][]CWCard{
		{{Suit: CWSuitPink, Rank: 7}, {Suit: CWSuitGreen, Rank: 9}},
		{{Suit: CWSuitPink, Rank: 2}, {Suit: CWSuitGreen, Rank: 2}},
		{{Suit: CWSuitPink, Rank: 1}, {Suit: CWSuitGreen, Rank: 3}},
	})

	cwPlayAll(t, g, 0, 1, 2)
	if g.Phase != CWPhaseGameOver {
		t.Fatalf("phase = %s, want game_over", g.Phase)
	}
	if g.Result == nil || !g.Result.Cleared || g.Result.FailedReason != "" {
		t.Fatalf("result = %+v, want cleared", g.Result)
	}
	if g.EndReason != "cleared" || g.Result.Mission != 1 {
		t.Fatalf("endReason=%q mission=%d", g.EndReason, g.Result.Mission)
	}
	if !strings.Contains(g.Result.Message, "귀환") {
		t.Fatalf("클리어 문구 = %q", g.Result.Message)
	}
}

// TestCWOutOfCards 카드가 다 떨어졌는데 임무가 남으면 실패
func TestCWOutOfCards(t *testing.T) {
	g, _ := cwNewTestGame(t, 3)
	g.Tasks = []CWTask{{Suit: CWSuitYellow, Rank: 4, Seat: 0}} // 아무도 안 든 카드
	cwSetHands(t, g, 0, [][]CWCard{
		{{Suit: CWSuitBlue, Rank: 5}},
		{{Suit: CWSuitBlue, Rank: 9}},
		{{Suit: CWSuitBlue, Rank: 1}},
	})

	cwPlayAll(t, g, 0, 1, 2)
	if g.Phase != CWPhaseGameOver {
		t.Fatalf("phase = %s, want game_over", g.Phase)
	}
	if g.Result == nil || g.Result.FailedReason != "out_of_cards" {
		t.Fatalf("result = %+v, want out_of_cards", g.Result)
	}
	if !strings.Contains(g.Result.Message, "카드가 모두 떨어졌") {
		t.Fatalf("실패 문구 = %q", g.Result.Message)
	}
}

// TestCWUnevenDeal 3인전은 40장이 나누어떨어지지 않는다 — 카드가 남은
// 좌석만 참여하는 짧은 트릭으로 라운드가 반드시 끝난다.
func TestCWUnevenDeal(t *testing.T) {
	g, _ := cwNewTestGame(t, 3)
	g.Tasks = []CWTask{{Suit: CWSuitYellow, Rank: 9, Seat: 0}}
	cwSetHands(t, g, 0, [][]CWCard{
		{{Suit: CWSuitBlue, Rank: 5}, {Suit: CWSuitBlue, Rank: 6}},
		{{Suit: CWSuitBlue, Rank: 9}},
		{{Suit: CWSuitBlue, Rank: 1}},
	})

	cwPlayAll(t, g, 0, 1, 2)
	if g.LastTrick.WinnerSeat != 1 {
		t.Fatalf("첫 트릭 승자 = %d", g.LastTrick.WinnerSeat)
	}
	// 손패가 남은 좌석은 seat0 뿐 — 혼자 내고 혼자 가져간다
	if len(g.trickOrder) != 1 || g.trickOrder[0] != 0 || g.CurrentSeat != 0 {
		t.Fatalf("짧은 트릭 구성 = %v (current=%d)", g.trickOrder, g.CurrentSeat)
	}
	if err := g.Play(0, 0); err != nil {
		t.Fatalf("Play: %v", err)
	}
	if g.Phase != CWPhaseGameOver || g.Result.FailedReason != "out_of_cards" {
		t.Fatalf("phase=%s result=%+v", g.Phase, g.Result)
	}
}

// TestCWCommunicateValidation 소통 검증 — 색 숫자 카드만, 로켓 불가,
// 선언이 실제로 참이어야 하고, 토큰은 1회, 트릭 시작 시점에만.
func TestCWCommunicateValidation(t *testing.T) {
	g, _ := cwNewTestGame(t, 3)
	g.Tasks = []CWTask{{Suit: CWSuitYellow, Rank: 9, Seat: 0}}
	cwSetHands(t, g, 0, [][]CWCard{
		{ // seat0: 파랑 3·7, 분홍 4(유일), 로켓 2
			{Suit: CWSuitBlue, Rank: 3}, {Suit: CWSuitBlue, Rank: 7},
			{Suit: CWSuitPink, Rank: 4}, {Suit: CWSuitRocket, Rank: 2},
		},
		{{Suit: CWSuitBlue, Rank: 9}, {Suit: CWSuitGreen, Rank: 2}},
		{{Suit: CWSuitBlue, Rank: 1}, {Suit: CWSuitGreen, Rank: 3}},
	})

	// 로켓은 공개 불가
	if err := g.Communicate(0, 3, CWHintHighest); err == nil {
		t.Fatal("로켓 공개가 허용됐다")
	} else if !strings.Contains(err.Error(), "로켓") {
		t.Fatalf("로켓 에러 문구 = %q", err.Error())
	}
	// 거짓 선언은 전부 거부
	if err := g.Communicate(0, 0, CWHintHighest); err == nil {
		t.Fatal("최고가 아닌 카드의 highest 선언이 허용됐다")
	}
	if err := g.Communicate(0, 1, CWHintLowest); err == nil {
		t.Fatal("최저가 아닌 카드의 lowest 선언이 허용됐다")
	}
	if err := g.Communicate(0, 0, CWHintOnly); err == nil {
		t.Fatal("2장 들고 있는 색의 only 선언이 허용됐다")
	}
	if err := g.Communicate(0, 2, CWHintHighest); err == nil {
		t.Fatal("한 장뿐인 색의 highest 선언이 허용됐다")
	}
	if err := g.Communicate(0, 2, CWHint("biggest")); err == nil {
		t.Fatal("알 수 없는 선언이 허용됐다")
	}
	if err := g.Communicate(0, 99, CWHintOnly); err == nil {
		t.Fatal("범위 밖 인덱스가 허용됐다")
	}
	if g.Players[0].Revealed != nil || g.Players[0].TokenLeft != CWTokenPerMission {
		t.Fatalf("거부된 소통이 상태를 바꿨다: %+v", g.Players[0])
	}

	// 참인 선언 — 카드는 손에 남고 전원이 본다
	before := len(g.Players[0].Hand)
	if err := g.Communicate(0, 2, CWHintOnly); err != nil {
		t.Fatalf("참인 only 선언이 거부됐다: %v", err)
	}
	rev := g.Players[0].Revealed
	if rev == nil || rev.Card != (CWCard{Suit: CWSuitPink, Rank: 4}) || rev.Hint != CWHintOnly {
		t.Fatalf("revealed = %+v", rev)
	}
	if len(g.Players[0].Hand) != before {
		t.Fatalf("공개한 카드가 손에서 빠졌다: %d → %d", before, len(g.Players[0].Hand))
	}
	if g.Players[0].TokenLeft != 0 {
		t.Fatalf("토큰이 안 줄었다: %d", g.Players[0].TokenLeft)
	}
	if !strings.Contains(cwEventText(g), "소통") {
		t.Fatal("소통 이벤트 부재")
	}

	// 토큰은 1회뿐
	if err := g.Communicate(0, 1, CWHintHighest); err == nil {
		t.Fatal("두 번째 소통이 허용됐다")
	}

	// 참인 highest — seat1 은 파랑 9 한 장뿐이라 only 만 가능
	if err := g.Communicate(1, 0, CWHintHighest); err == nil {
		t.Fatal("한 장뿐인 색의 highest 가 허용됐다")
	}
	if err := g.Communicate(1, 0, CWHintOnly); err != nil {
		t.Fatalf("참인 only 가 거부됐다: %v", err)
	}

	// 트릭이 시작되면 소통 불가
	if err := g.Play(0, 0); err != nil {
		t.Fatalf("Play: %v", err)
	}
	if err := g.Communicate(2, 0, CWHintOnly); err == nil {
		t.Fatal("트릭 진행 중 소통이 허용됐다")
	} else if !strings.Contains(err.Error(), "트릭") {
		t.Fatalf("트릭 중 소통 에러 문구 = %q", err.Error())
	}
}

// TestCWCommunicateHighestLowest 2장 이상인 색에서만 최고/최저 선언이 참이다
func TestCWCommunicateHighestLowest(t *testing.T) {
	g, _ := cwNewTestGame(t, 3)
	g.Tasks = []CWTask{{Suit: CWSuitYellow, Rank: 9, Seat: 0}}
	cwSetHands(t, g, 0, [][]CWCard{
		{{Suit: CWSuitGreen, Rank: 2}, {Suit: CWSuitGreen, Rank: 6}, {Suit: CWSuitGreen, Rank: 8}},
		{{Suit: CWSuitBlue, Rank: 9}},
		{{Suit: CWSuitBlue, Rank: 1}},
	})

	if err := g.Communicate(0, 2, CWHintHighest); err != nil {
		t.Fatalf("참인 highest 가 거부됐다: %v", err)
	}
	g.Players[0].TokenLeft = CWTokenPerMission // 토큰을 되돌려 lowest 도 검증한다
	if err := g.Communicate(0, 0, CWHintLowest); err != nil {
		t.Fatalf("참인 lowest 가 거부됐다: %v", err)
	}
	if g.Players[0].Revealed.Hint != CWHintLowest {
		t.Fatalf("마지막 공개 = %+v", g.Players[0].Revealed)
	}

	// 공개한 카드를 내면 공개 표식이 사라진다 (손에 없는 정보를 남기지 않는다)
	if err := g.Play(0, 0); err != nil {
		t.Fatalf("Play: %v", err)
	}
	if g.Players[0].Revealed != nil {
		t.Fatalf("낸 카드가 계속 공개돼 있다: %+v", g.Players[0].Revealed)
	}
}

// TestCWForcePlay AFK 자동 제출 — 낼 수 있는 카드 중 무작위로 내며,
// 자동 제출만으로도 라운드가 반드시 끝난다.
func TestCWForcePlay(t *testing.T) {
	g, rng := cwNewTestGame(t, 4)

	// 따라내기 의무를 지키는지: 리드 색을 든 좌석은 그 색만 낸다
	g.Tasks = []CWTask{{Suit: CWSuitYellow, Rank: 9, Seat: 0}}
	cwSetHands(t, g, 0, [][]CWCard{
		{{Suit: CWSuitBlue, Rank: 5}, {Suit: CWSuitGreen, Rank: 2}},
		{{Suit: CWSuitBlue, Rank: 9}, {Suit: CWSuitGreen, Rank: 3}},
		{{Suit: CWSuitBlue, Rank: 2}, {Suit: CWSuitGreen, Rank: 4}},
		{{Suit: CWSuitBlue, Rank: 1}, {Suit: CWSuitGreen, Rank: 5}},
	})
	g.ForcePlay(rng)
	lead := g.LeadSuit
	if lead == "" {
		t.Fatal("자동 제출이 리드를 못 잡았다")
	}
	for i := 0; i < 3; i++ { // 남은 세 좌석 — 네 번째 제출에서 트릭이 정산된다
		g.ForcePlay(rng)
	}
	if g.LastTrick == nil || len(g.LastTrick.Cards) != 4 {
		t.Fatalf("자동 제출로 트릭이 안 끝났다: %+v", g.LastTrick)
	}
	for _, tc := range g.LastTrick.Cards {
		if tc.Card.Suit != lead { // 전원 리드 색을 들고 있으므로 반드시 따라내야 한다
			t.Fatalf("자동 제출이 따라내기 의무를 어겼다: lead=%s card=%+v", lead, tc.Card)
		}
	}

	// 40장 전량 배분 상태에서도 자동 제출만으로 라운드가 끝난다
	g2, rng2 := cwNewTestGame(t, 4)
	guard := 0
	for g2.Phase == CWPhasePlaying {
		guard++
		if guard > CWDeckSize+8 {
			t.Fatal("자동 제출만으로 라운드가 끝나지 않는다")
		}
		g2.ForcePlay(rng2)
	}
	if g2.Phase != CWPhaseRoundEnd && g2.Phase != CWPhaseGameOver {
		t.Fatalf("자동 제출 종료 phase = %s", g2.Phase)
	}
}

// TestCWBotPickPlay 봇 판단 — 자기 임무 카드는 이기려 하고 남의 임무 카드는
// 일부러 흘린다 (전부 따라내기 의무 안에서).
func TestCWBotPickPlay(t *testing.T) {
	hand := []CWCard{
		{Suit: CWSuitBlue, Rank: 2}, {Suit: CWSuitBlue, Rank: 8}, {Suit: CWSuitGreen, Rank: 9},
	}

	// 트릭에 자기 임무 카드(파랑 4)가 있으면 이길 수 있는 최소 카드를 낸다
	mine := cwBotState{
		YourSeat: 1, Phase: CWPhasePlaying, CurrentSeat: 1, LeadSuit: CWSuitBlue,
		Trick:    []CWTrickCard{{Seat: 0, Card: CWCard{CWSuitBlue, 4}}},
		Tasks:    []CWTask{{Suit: CWSuitBlue, Rank: 4, Seat: 1}},
		YourHand: hand,
	}
	if got := cwBotPickPlay(mine); got != 1 {
		t.Fatalf("자기 임무 카드를 안 가져간다: index=%d", got)
	}

	// 남의 임무 카드면 지는 카드를 낸다
	others := mine
	others.Tasks = []CWTask{{Suit: CWSuitBlue, Rank: 4, Seat: 2}}
	if got := cwBotPickPlay(others); got != 0 {
		t.Fatalf("남의 임무를 흘리지 않는다: index=%d", got)
	}

	// 임무와 무관하면 최약 카드 (따라내기 의무 안에서)
	plain := mine
	plain.Tasks = []CWTask{{Suit: CWSuitPink, Rank: 1, Seat: 2}}
	if got := cwBotPickPlay(plain); got != 0 {
		t.Fatalf("최약 카드를 안 낸다: index=%d", got)
	}

	// 완료된 임무는 더 이상 고려하지 않는다
	done := mine
	done.Tasks = []CWTask{{Suit: CWSuitBlue, Rank: 4, Seat: 1, Done: true}}
	if got := cwBotPickPlay(done); got != 0 {
		t.Fatalf("완료된 임무를 다시 노린다: index=%d", got)
	}
}

// TestCWBotPickCommunicate 봇 소통 선택은 서버 검증을 항상 통과한다
func TestCWBotPickCommunicate(t *testing.T) {
	hands := [][]CWCard{
		{{Suit: CWSuitBlue, Rank: 2}, {Suit: CWSuitBlue, Rank: 8}, {Suit: CWSuitRocket, Rank: 1}},
		{{Suit: CWSuitPink, Rank: 5}, {Suit: CWSuitRocket, Rank: 4}},
		{{Suit: CWSuitRocket, Rank: 1}, {Suit: CWSuitRocket, Rank: 2}}, // 색 카드 없음
	}
	wantIndex := []int{1, 0, -1}
	wantHint := []CWHint{CWHintHighest, CWHintOnly, ""}

	for i, hand := range hands {
		index, hint := cwBotPickCommunicate(hand)
		if index != wantIndex[i] || hint != wantHint[i] {
			t.Fatalf("hand %d: (%d, %q), want (%d, %q)", i, index, hint, wantIndex[i], wantHint[i])
		}
		if index < 0 {
			continue
		}
		// 서버 검증을 그대로 통과해야 한다
		g, _ := cwNewTestGame(t, 3)
		g.Tasks = []CWTask{{Suit: CWSuitYellow, Rank: 9, Seat: 0}}
		g.Players[0].Hand = append([]CWCard{}, hand...)
		g.Players[0].TokenLeft = CWTokenPerMission
		g.Trick = []CWTrickCard{}
		if err := g.Communicate(0, index, hint); err != nil {
			t.Fatalf("hand %d: 봇 선언이 서버 검증에 걸렸다: %v", i, err)
		}
	}
}
