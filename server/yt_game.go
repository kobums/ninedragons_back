package server

import (
	"errors"
	"math/rand"
	"strconv"
	"strings"
	"time"
)

// ==================== 요트 다이스 순수 규칙 ====================
//
// 주사위 굴림·족보 점수·턴 진행만 다룬다. 클라이언트·타이머를 모르며,
// 허브(yt_hub.go)가 결과를 이벤트로 번역한다.
//
// 점수표는 상단 6칸(1~6의 눈) + 하단 7칸 = 13칸이고 전 칸을 기록하면
// 게임이 끝난다 (총 라운드 = 13). 상단 합계 63점 이상이면 보너스 +35.

// ytTotalRounds 총 라운드 수 — 점수표 칸 수와 같다
func ytTotalRounds() int {
	return len(ytCategories)
}

// ytCategoryName 카테고리의 한글 이름 (이벤트 문구용)
func ytCategoryName(cat string) string {
	switch cat {
	case YTCatOnes:
		return "1의 눈"
	case YTCatTwos:
		return "2의 눈"
	case YTCatThrees:
		return "3의 눈"
	case YTCatFours:
		return "4의 눈"
	case YTCatFives:
		return "5의 눈"
	case YTCatSixes:
		return "6의 눈"
	case YTCatTriple:
		return "트리플"
	case YTCatQuad:
		return "포카드"
	case YTCatFullHouse:
		return "풀하우스"
	case YTCatSmall:
		return "스몰 스트레이트"
	case YTCatLarge:
		return "라지 스트레이트"
	case YTCatYacht:
		return "요트"
	case YTCatChance:
		return "찬스"
	}
	return cat
}

// ytValidCategory 카테고리 값 검증
func ytValidCategory(cat string) bool {
	for _, c := range ytCategories {
		if c == cat {
			return true
		}
	}
	return false
}

// ytDiceString 이벤트·로그용 주사위 표기 ("3-4-4-6-2")
func ytDiceString(dice []int) string {
	parts := make([]string, len(dice))
	for i, d := range dice {
		parts[i] = strconv.Itoa(d)
	}
	return strings.Join(parts, "-")
}

// ==================== 족보 점수 계산 ====================

// ytCounts 눈(1~6)별 개수 — 인덱스가 곧 눈이다 (0 미사용)
func ytCounts(dice []int) [7]int {
	var counts [7]int
	for _, d := range dice {
		if d >= 1 && d <= 6 {
			counts[d]++
		}
	}
	return counts
}

// ytSum 주사위 전체 합
func ytSum(dice []int) int {
	sum := 0
	for _, d := range dice {
		sum += d
	}
	return sum
}

// ytScoreUpper 상단 — 해당 눈의 합 (눈 × 개수)
func ytScoreUpper(dice []int, face int) int {
	return face * ytCounts(dice)[face]
}

// ytScoreTriple 3 of a kind — 같은 눈 3개 이상이면 전체 합, 아니면 0
func ytScoreTriple(dice []int) int {
	for _, n := range ytCounts(dice) {
		if n >= 3 {
			return ytSum(dice)
		}
	}
	return 0
}

// ytScoreQuad 4 of a kind — 같은 눈 4개 이상이면 전체 합, 아니면 0
func ytScoreQuad(dice []int) int {
	for _, n := range ytCounts(dice) {
		if n >= 4 {
			return ytSum(dice)
		}
	}
	return 0
}

// ytScoreFullHouse 풀하우스 — 정확히 3개+2개 조합이면 25 (5개 동일은 제외)
func ytScoreFullHouse(dice []int) int {
	has3, has2 := false, false
	for _, n := range ytCounts(dice) {
		if n == 3 {
			has3 = true
		}
		if n == 2 {
			has2 = true
		}
	}
	if has3 && has2 {
		return 25
	}
	return 0
}

// ytScoreSmallStraight 스몰 스트레이트 — 4연속(1234/2345/3456)이면 30
func ytScoreSmallStraight(dice []int) int {
	counts := ytCounts(dice)
	for start := 1; start <= 3; start++ {
		run := true
		for f := start; f < start+4; f++ {
			if counts[f] == 0 {
				run = false
				break
			}
		}
		if run {
			return 30
		}
	}
	return 0
}

// ytScoreLargeStraight 라지 스트레이트 — 5연속(12345/23456)이면 40
func ytScoreLargeStraight(dice []int) int {
	counts := ytCounts(dice)
	for start := 1; start <= 2; start++ {
		run := true
		for f := start; f < start+5; f++ {
			if counts[f] == 0 {
				run = false
				break
			}
		}
		if run {
			return 40
		}
	}
	return 0
}

// ytScoreYacht 요트 — 5개 동일이면 50
func ytScoreYacht(dice []int) int {
	for _, n := range ytCounts(dice) {
		if n == YTDiceCount {
			return 50
		}
	}
	return 0
}

// ytScoreChance 찬스 — 전체 합
func ytScoreChance(dice []int) int {
	return ytSum(dice)
}

// ytCategoryScore 카테고리별 점수 판정의 단일 진입점
func ytCategoryScore(cat string, dice []int) int {
	switch cat {
	case YTCatOnes:
		return ytScoreUpper(dice, 1)
	case YTCatTwos:
		return ytScoreUpper(dice, 2)
	case YTCatThrees:
		return ytScoreUpper(dice, 3)
	case YTCatFours:
		return ytScoreUpper(dice, 4)
	case YTCatFives:
		return ytScoreUpper(dice, 5)
	case YTCatSixes:
		return ytScoreUpper(dice, 6)
	case YTCatTriple:
		return ytScoreTriple(dice)
	case YTCatQuad:
		return ytScoreQuad(dice)
	case YTCatFullHouse:
		return ytScoreFullHouse(dice)
	case YTCatSmall:
		return ytScoreSmallStraight(dice)
	case YTCatLarge:
		return ytScoreLargeStraight(dice)
	case YTCatYacht:
		return ytScoreYacht(dice)
	case YTCatChance:
		return ytScoreChance(dice)
	}
	return 0
}

// ==================== 점수표 집계 ====================

// ytUpperSum 기록된 상단 칸의 합 (미기록 -1 은 제외)
func ytUpperSum(scores map[string]int) int {
	sum := 0
	for _, cat := range ytCategories[:ytUpperCategoryCount] {
		if v := scores[cat]; v > 0 {
			sum += v
		}
	}
	return sum
}

// ytBonus 상단 합계 보너스 — 63점 이상이면 35
func ytBonus(scores map[string]int) int {
	if ytUpperSum(scores) >= YTUpperBonusThreshold {
		return YTUpperBonus
	}
	return 0
}

// ytTotal 총점 — 기록된 전 칸의 합 + 보너스
func ytTotal(scores map[string]int) int {
	sum := ytBonus(scores)
	for _, cat := range ytCategories {
		if v := scores[cat]; v > 0 {
			sum += v
		}
	}
	return sum
}

// ==================== 게임 진행 ====================

// NewYTGame 대기 상태의 새 게임. dice/held 는 처음부터 5칸이다 (nil 금지).
func NewYTGame(id string) *YTGame {
	return &YTGame{
		ID:          id,
		Players:     []*YTPlayer{},
		Phase:       YTPhaseWaiting,
		CurrentSeat: -1,
		Dice:        make([]int, YTDiceCount),
		Held:        make([]bool, YTDiceCount),
		WinnerSeats: []int{},
	}
}

// AddPlayer 대기실 입장. 좌석 번호를 돌려준다.
func (g *YTGame) AddPlayer(name string) (int, error) {
	if g.Phase != YTPhaseWaiting {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= YTMaxPlayers {
		return -1, errors.New("자리가 없습니다 (최대 4명)")
	}
	scores := map[string]int{}
	for _, cat := range ytCategories {
		scores[cat] = -1
	}
	seat := len(g.Players)
	g.Players = append(g.Players, &YTPlayer{Seat: seat, Name: name, Scores: scores})
	return seat, nil
}

// RemovePlayer 대기실 퇴장. 좌석을 앞으로 당겨 빈틈을 없앤다.
func (g *YTGame) RemovePlayer(seat int) {
	if g.Phase != YTPhaseWaiting || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i, p := range g.Players {
		p.Seat = i
	}
}

// CanStart 시작 가능 여부 (호스트 명시 시작 — 2인부터)
func (g *YTGame) CanStart() bool {
	return g.Phase == YTPhaseWaiting && len(g.Players) >= YTMinPlayers
}

// Start 게임 시작 — 선공은 무작위, 1라운드 첫 턴을 연다
func (g *YTGame) Start(rng *rand.Rand) error {
	if !g.CanStart() {
		return errors.New("2명 이상 모여야 시작할 수 있습니다")
	}
	g.Ready = true
	g.StartedAt = time.Now()
	g.Phase = YTPhasePlaying
	g.Round = 1
	g.FirstSeat = rng.Intn(len(g.Players))
	g.beginTurn(g.FirstSeat)
	return nil
}

// beginTurn 좌석의 턴 시작 — 굴림 3회 충전, 홀드 해제.
// 주사위 값은 유지된다 (테이블 위에 남아있는 그림 — 첫 굴림에서 전부 굴린다).
func (g *YTGame) beginTurn(seat int) {
	g.CurrentSeat = seat
	g.RollsLeft = YTMaxRolls
	for i := range g.Held {
		g.Held[i] = false
	}
}

// Roll 주사위 굴리기. 턴의 첫 굴림은 hold 를 무시하고 전부 굴린다.
// 이후 굴림은 hold[i]=true 인 주사위를 고정한다 (전부 고정은 거부).
func (g *YTGame) Roll(seat int, rng *rand.Rand, hold []bool) error {
	if g.Phase != YTPhasePlaying {
		return errors.New("지금은 주사위를 굴릴 수 없습니다")
	}
	if seat != g.CurrentSeat {
		return errors.New("당신의 차례가 아닙니다")
	}
	if g.RollsLeft <= 0 {
		return errors.New("남은 굴림이 없습니다 — 점수를 기록해 주세요")
	}
	if g.RollsLeft == YTMaxRolls {
		// 턴의 첫 굴림 — 홀드할 대상이 없으므로 전부 굴린다
		for i := range g.Held {
			g.Held[i] = false
		}
		for i := range g.Dice {
			g.Dice[i] = 1 + rng.Intn(6)
		}
	} else {
		if len(hold) != YTDiceCount {
			return errors.New("홀드 정보는 주사위 5개 전부를 보내야 합니다")
		}
		all := true
		for _, h := range hold {
			if !h {
				all = false
				break
			}
		}
		if all {
			return errors.New("주사위를 전부 고정한 채 굴릴 수 없습니다")
		}
		copy(g.Held, hold)
		for i := range g.Dice {
			if !g.Held[i] {
				g.Dice[i] = 1 + rng.Intn(6)
			}
		}
	}
	g.RollsLeft--
	return nil
}

// YTScoreResult 점수 기록 한 번의 결과
type YTScoreResult struct {
	Seat     int
	Category string
	Points   int
	GameOver bool
}

// Score 현재 주사위로 빈 칸 하나에 점수를 기록하고 턴을 넘긴다.
// 족보가 안 돼도 0점 기록이 가능하다 (칸 희생).
func (g *YTGame) Score(seat int, category string) (*YTScoreResult, error) {
	if g.Phase != YTPhasePlaying {
		return nil, errors.New("지금은 점수를 기록할 수 없습니다")
	}
	if seat != g.CurrentSeat {
		return nil, errors.New("당신의 차례가 아닙니다")
	}
	if g.RollsLeft == YTMaxRolls {
		return nil, errors.New("먼저 주사위를 굴려야 합니다")
	}
	if !ytValidCategory(category) {
		return nil, errors.New("알 수 없는 점수 칸입니다")
	}
	p := g.Players[seat]
	if p.Scores[category] != -1 {
		return nil, errors.New("이미 기록한 칸입니다")
	}

	points := ytCategoryScore(category, g.Dice)
	p.Scores[category] = points
	g.LastAction = &YTLastAction{Seat: seat, Category: category, Points: points}
	res := &YTScoreResult{Seat: seat, Category: category, Points: points}

	next := (seat + 1) % len(g.Players)
	if next == g.FirstSeat {
		g.Round++ // 선공에게 돌아오면 다음 라운드
	}
	if g.Round > ytTotalRounds() {
		g.finish()
		res.GameOver = true
	} else {
		g.beginTurn(next)
	}
	return res, nil
}

// finish 전 칸 기록 완료 — 총점 최고 좌석(들)이 승자 (동점 공동 승)
func (g *YTGame) finish() {
	g.Phase = YTPhaseGameOver
	best := -1
	g.WinnerSeats = []int{}
	for _, p := range g.Players {
		total := ytTotal(p.Scores)
		if total > best {
			best = total
			g.WinnerSeats = []int{p.Seat}
		} else if total == best {
			g.WinnerSeats = append(g.WinnerSeats, p.Seat)
		}
	}
}
