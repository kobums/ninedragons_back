package server

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"time"
)

// ==================== 카드 유틸 (순수 — 봇과 서버 검증이 공유) ====================

// mtSuits 문양 순서 (손패 정렬·최장문양 동률 판정에 사용)
var mtSuits = []string{"S", "D", "C", "H"}

// mtParseCard "S14" → ("S", 14). 조커·비정상 문자열은 ok=false.
func mtParseCard(card string) (suit string, rank int, ok bool) {
	if len(card) < 2 || card == MTJoker {
		return "", 0, false
	}
	suit = card[:1]
	if suit != "S" && suit != "D" && suit != "C" && suit != "H" {
		return "", 0, false
	}
	rank, err := strconv.Atoi(card[1:])
	if err != nil || rank < 2 || rank > 14 {
		return "", 0, false
	}
	return suit, rank, true
}

// mtIsValidCard 와이어 인코딩으로 유효한 카드인지
func mtIsValidCard(card string) bool {
	if card == MTJoker {
		return true
	}
	_, _, ok := mtParseCard(card)
	return ok
}

// mtIsScoreCard 점수 카드(10·J·Q·K·A) 여부. 조커는 점수 카드가 아니다.
func mtIsScoreCard(card string) bool {
	_, rank, ok := mtParseCard(card)
	return ok && rank >= 10
}

// mtMightyCard 마이티 카드. 기루다가 스페이드면 다이아A 로 바뀐다.
func mtMightyCard(trump string) string {
	if trump == "S" {
		return "D14"
	}
	return "S14"
}

// mtJokerCallCard 조커콜 카드. 기루다가 클로버면 스페이드3 으로 바뀐다.
func mtJokerCallCard(trump string) string {
	if trump == "C" {
		return "S3"
	}
	return "C3"
}

// mtIsTrumpSuit 입찰·키티에 쓸 수 있는 문양인지 (N = 노기루)
func mtIsTrumpSuit(suit string) bool {
	switch suit {
	case "S", "D", "C", "H", "N":
		return true
	}
	return false
}

// mtSortHand 손패 정렬: JK 맨 앞, 이후 S·D·C·H 문양별 숫자 내림차순
func mtSortHand(hand []string) {
	suitOrder := func(card string) int {
		if card == MTJoker {
			return -1
		}
		suit, _, _ := mtParseCard(card)
		for i, s := range mtSuits {
			if s == suit {
				return i
			}
		}
		return len(mtSuits)
	}
	sort.SliceStable(hand, func(i, j int) bool {
		si, sj := suitOrder(hand[i]), suitOrder(hand[j])
		if si != sj {
			return si < sj
		}
		_, ri, _ := mtParseCard(hand[i])
		_, rj, _ := mtParseCard(hand[j])
		return ri > rj
	})
}

// mtHandContains 손패에 카드가 있는지
func mtHandContains(hand []string, card string) bool {
	for _, c := range hand {
		if c == card {
			return true
		}
	}
	return false
}

// mtRemoveCard 손패에서 카드 한 장 제거 (없으면 false)
func mtRemoveCard(hand []string, card string) ([]string, bool) {
	for i, c := range hand {
		if c == card {
			return append(hand[:i:i], hand[i+1:]...), true
		}
	}
	return hand, false
}

// ==================== 입찰 우열 (순수) ====================

// mtBidRangeValid 공약 수치가 허용 범위인지 (노기루는 12부터)
func mtBidRangeValid(suit string, count int) bool {
	min := MTMinBid
	if suit == "N" {
		min = MTMinBidNT
	}
	return count >= min && count <= MTMaxBid
}

// mtBidBeats 새 공약이 현재 최고 공약을 이기는지.
// count 큰 쪽 우위, 같으면 노기루만 우위.
func mtBidBeats(suit string, count int, best *MTBid) bool {
	if best == nil {
		return true
	}
	if count != best.Count {
		return count > best.Count
	}
	return suit == "N" && best.Suit != "N"
}

// ==================== 팔로우 합법성 (순수) ====================

// mtLegalPlays 지금 낼 수 있는 카드 목록.
//   - 조커콜 발동 중 조커 소지자는 조커만 낼 수 있다.
//   - 리드는 아무 카드나.
//   - 팔로우는 리드 문양 의무. 단 마이티·조커는 언제나 낼 수 있고,
//     리드 문양이 없으면 아무거나.
func mtLegalPlays(hand []string, isLead bool, ledSuit string, jokerCallActive bool, trump string) []string {
	if jokerCallActive && !isLead && mtHandContains(hand, MTJoker) {
		return []string{MTJoker}
	}
	if isLead {
		return append([]string{}, hand...)
	}

	mighty := mtMightyCard(trump)
	legal := []string{}
	hasLed := false
	for _, c := range hand {
		suit, _, ok := mtParseCard(c)
		if ok && suit == ledSuit {
			hasLed = true
			legal = append(legal, c)
			continue
		}
		if c == MTJoker || c == mighty {
			legal = append(legal, c)
		}
	}
	if !hasLed {
		return append([]string{}, hand...)
	}
	return legal
}

// ==================== 트릭 승자 (순수) ====================

// mtTrickWinner 트릭 승자의 plays 인덱스.
// 우선순위: 마이티 > 조커(첫·막 트릭과 조커콜 시 제외) > 기루다 최고 > 리드 문양 최고.
// 노기루(N)면 기루다 단계를 건너뛴다. 판정은 항상 기루다를 인자로 받는다.
func mtTrickWinner(plays []MTTrickPlay, ledSuit, trump string, trickNo int, jokerCall bool) int {
	mighty := mtMightyCard(trump)
	for i, p := range plays {
		if p.Card == mighty {
			return i
		}
	}

	if !jokerCall && trickNo != 1 && trickNo != MTTrickCount {
		for i, p := range plays {
			if p.Card == MTJoker {
				return i
			}
		}
	}

	if trump != "N" {
		best, bestRank := -1, 0
		for i, p := range plays {
			suit, rank, ok := mtParseCard(p.Card)
			if ok && suit == trump && rank > bestRank {
				best, bestRank = i, rank
			}
		}
		if best >= 0 {
			return best
		}
	}

	best, bestRank := -1, 0
	for i, p := range plays {
		suit, rank, ok := mtParseCard(p.Card)
		if ok && suit == ledSuit && rank > bestRank {
			best, bestRank = i, rank
		}
	}
	if best >= 0 {
		return best
	}
	// 리드 문양 카드가 하나도 없는 극단 케이스(조커 리드 뒤 전원 이탈) — 리드가 가져간다
	return 0
}

// ==================== 게임 상태 전이 ====================

func NewMTGame(id string) *MTGame {
	return &MTGame{
		ID:             id,
		Phase:          MTPhaseWaiting,
		Players:        []MTPlayer{},
		Hands:          map[int][]string{},
		Passed:         map[int]bool{},
		TricksWon:      map[int]int{},
		CapturedPoints: map[int]int{},
		Declarer:       -1,
		FriendSeat:     -1,
		BidTurn:        -1,
		Turn:           -1,
	}
}

// AddPlayer 대기 중 좌석 배정
func (g *MTGame) AddPlayer(name string) (int, error) {
	if g.Ready {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= MTPlayerCount {
		return -1, errors.New("정원이 가득 찼습니다")
	}
	seat := len(g.Players)
	g.Players = append(g.Players, MTPlayer{Seat: seat, Name: name})
	return seat, nil
}

// RemovePlayer 대기 중 좌석 제거 (뒷좌석을 앞으로 당긴다)
func (g *MTGame) RemovePlayer(seat int) {
	if g.Ready || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i := range g.Players {
		g.Players[i].Seat = i
	}
}

// Start 5명이 모이면 배분하고 입찰을 시작한다
func (g *MTGame) Start(rng *rand.Rand) error {
	if g.Ready {
		return errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) != MTPlayerCount {
		return errors.New("5명이 모여야 시작할 수 있습니다")
	}
	g.Ready = true
	g.StartedAt = time.Now()
	g.deal(rng)
	return nil
}

// deal 53장 셔플 → 10장씩 5명 + 키티 3장, 입찰 상태 초기화
func (g *MTGame) deal(rng *rand.Rand) {
	deck := make([]string, 0, 53)
	for _, s := range mtSuits {
		for r := 2; r <= 14; r++ {
			deck = append(deck, s+strconv.Itoa(r))
		}
	}
	deck = append(deck, MTJoker)
	rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })

	for seat := 0; seat < MTPlayerCount; seat++ {
		hand := append([]string{}, deck[seat*MTHandSize:(seat+1)*MTHandSize]...)
		mtSortHand(hand)
		g.Hands[seat] = hand
	}
	g.Kitty = append([]string{}, deck[MTPlayerCount*MTHandSize:]...)

	g.Phase = MTPhaseBidding
	g.Passed = map[int]bool{}
	g.Best = nil
	g.BidTurn = 0
	g.Turn = -1
}

// Redeal 전원 패스 시 재배분
func (g *MTGame) Redeal(rng *rand.Rand) {
	g.RedealCount++
	g.deal(rng)
}

// mtBidStep 입찰 액션 하나의 결과
type mtBidStep struct {
	Awarded bool // 낙찰 — 주공 확정, kitty 단계 진입
	Redeal  bool // 전원 패스 — 재배분 필요
}

// Bid 공약 제시
func (g *MTGame) Bid(seat int, suit string, count int) (mtBidStep, error) {
	if g.Phase != MTPhaseBidding {
		return mtBidStep{}, errors.New("입찰 단계가 아닙니다")
	}
	if seat != g.BidTurn {
		return mtBidStep{}, errors.New("당신의 입찰 차례가 아닙니다")
	}
	if !mtIsTrumpSuit(suit) {
		return mtBidStep{}, errors.New("잘못된 기루다입니다")
	}
	if !mtBidRangeValid(suit, count) {
		return mtBidStep{}, fmt.Errorf("공약은 %d~%d 사이여야 합니다 (노기루는 %d부터)", MTMinBid, MTMaxBid, MTMinBidNT)
	}
	if !mtBidBeats(suit, count, g.Best) {
		return mtBidStep{}, errors.New("현재 공약보다 높아야 합니다")
	}
	g.Best = &MTBid{Seat: seat, Suit: suit, Count: count}
	return g.advanceBidding(), nil
}

// Pass 입찰 포기 (해당 딜에서 재입찰 불가)
func (g *MTGame) Pass(seat int) (mtBidStep, error) {
	if g.Phase != MTPhaseBidding {
		return mtBidStep{}, errors.New("입찰 단계가 아닙니다")
	}
	if seat != g.BidTurn {
		return mtBidStep{}, errors.New("당신의 입찰 차례가 아닙니다")
	}
	g.Passed[seat] = true
	return g.advanceBidding(), nil
}

// advanceBidding 다음 입찰자를 찾는다. 남은 사람이 최고 입찰자뿐이면 낙찰,
// 아무도 없으면(전원 패스) 재배분.
func (g *MTGame) advanceBidding() mtBidStep {
	for i := 1; i <= MTPlayerCount; i++ {
		next := (g.BidTurn + i) % MTPlayerCount
		if g.Passed[next] {
			continue
		}
		if g.Best != nil && next == g.Best.Seat {
			continue // 최고 입찰자는 남이 올릴 때까지 다시 부르지 않는다
		}
		g.BidTurn = next
		return mtBidStep{}
	}
	if g.Best == nil {
		return mtBidStep{Redeal: true}
	}
	g.award()
	return mtBidStep{Awarded: true}
}

// award 낙찰 — 주공 확정, 키티 3장을 주공 손패에 합친다 (13장)
func (g *MTGame) award() {
	g.Declarer = g.Best.Seat
	g.Trump = g.Best.Suit
	g.ContractCount = g.Best.Count
	g.BidTurn = -1

	hand := append(g.Hands[g.Declarer], g.Kitty...)
	mtSortHand(hand)
	g.Hands[g.Declarer] = hand
	g.Phase = MTPhaseKitty
}

// SubmitKitty 주공의 키티 처리: 3장 버림 + 최종 기루다 확정.
// 기루다를 바꾸면 공약 count +1. 버린 점수 카드는 주공팀 득점 귀속(KittyPoints).
func (g *MTGame) SubmitKitty(seat int, discard []string, suit string) error {
	if g.Phase != MTPhaseKitty {
		return errors.New("키티 단계가 아닙니다")
	}
	if seat != g.Declarer {
		return errors.New("주공만 키티를 처리할 수 있습니다")
	}
	if len(discard) != MTKittySize {
		return errors.New("3장을 버려야 합니다")
	}
	if !mtIsTrumpSuit(suit) {
		return errors.New("잘못된 기루다입니다")
	}

	hand := append([]string{}, g.Hands[seat]...)
	for _, card := range discard {
		var ok bool
		hand, ok = mtRemoveCard(hand, card)
		if !ok {
			return fmt.Errorf("손에 없는 카드입니다: %s", card)
		}
	}

	if suit != g.Trump {
		g.ContractCount++
		g.Trump = suit
	}
	g.Hands[seat] = hand
	g.KittyDiscard = append([]string{}, discard...)
	g.KittyPoints = 0
	for _, card := range discard {
		if mtIsScoreCard(card) {
			g.KittyPoints++
		}
	}
	g.Phase = MTPhaseFriend
	return nil
}

// SetFriend 주공의 프렌드 지정. 본인 소지 카드를 지정하면 노프렌드 처리.
func (g *MTGame) SetFriend(seat int, ftype, card string) error {
	if g.Phase != MTPhaseFriend {
		return errors.New("프렌드 지정 단계가 아닙니다")
	}
	if seat != g.Declarer {
		return errors.New("주공만 프렌드를 지정할 수 있습니다")
	}

	switch ftype {
	case "card":
		if !mtIsValidCard(card) {
			return errors.New("잘못된 카드입니다")
		}
		if mtHandContains(g.Hands[seat], card) {
			// 본인 지정 카드 → 노프렌드
			ftype, card = "none", ""
		}
	case "first_trick", "none":
		card = ""
	default:
		return errors.New("잘못된 프렌드 유형입니다")
	}

	g.FriendType = ftype
	g.FriendCard = card
	g.FriendSeat = -1
	g.FriendRevealed = ftype == "none"

	g.Phase = MTPhasePlay
	g.TrickNo = 1
	g.Turn = g.Declarer
	g.Trick = []MTTrickPlay{}
	g.LedSuit = ""
	g.JokerCallActive = false
	return nil
}

// mtPlayStep 카드 플레이 하나의 결과 (허브가 이벤트를 조립하는 데 쓴다)
type mtPlayStep struct {
	JokerCall      bool // 이번 리드로 조커콜 발동
	FriendRevealed bool // 이번 플레이로 프렌드 공개
	TrickDone      bool
	TrickWinner    int
	WonTrickNo     int
	GameOver       bool
}

// Play 카드 한 장을 낸다
func (g *MTGame) Play(seat int, card string, jokerCall bool, jokerSuit string) (mtPlayStep, error) {
	if g.Phase != MTPhasePlay {
		return mtPlayStep{}, errors.New("플레이 단계가 아닙니다")
	}
	if seat != g.Turn {
		return mtPlayStep{}, errors.New("당신의 차례가 아닙니다")
	}
	hand := g.Hands[seat]
	if !mtHandContains(hand, card) {
		return mtPlayStep{}, errors.New("손에 없는 카드입니다")
	}

	isLead := len(g.Trick) == 0
	legal := mtLegalPlays(hand, isLead, g.LedSuit, g.JokerCallActive, g.Trump)
	if !mtHandContains(legal, card) {
		if g.JokerCallActive && card != MTJoker {
			return mtPlayStep{}, errors.New("조커콜 — 조커를 내야 합니다")
		}
		return mtPlayStep{}, errors.New("리드 문양을 따라야 합니다")
	}

	step := mtPlayStep{}
	if isLead {
		g.JokerCallActive = false
		if card == MTJoker {
			if jokerSuit != "S" && jokerSuit != "D" && jokerSuit != "C" && jokerSuit != "H" {
				return mtPlayStep{}, errors.New("조커 리드에는 문양 지정이 필요합니다")
			}
			g.LedSuit = jokerSuit
		} else {
			suit, _, _ := mtParseCard(card)
			g.LedSuit = suit
			if jokerCall {
				if card != mtJokerCallCard(g.Trump) {
					return mtPlayStep{}, errors.New("조커콜 카드로만 조커콜을 선언할 수 있습니다")
				}
				if g.TrickNo != 1 { // 첫 트릭에는 조커콜 무효
					g.JokerCallActive = true
					step.JokerCall = true
				}
			}
		}
	}

	g.Hands[seat], _ = mtRemoveCard(hand, card)
	g.Trick = append(g.Trick, MTTrickPlay{Seat: seat, Card: card})

	// 프렌드 카드가 실제로 플레이되면 공개
	if g.FriendType == "card" && !g.FriendRevealed && card == g.FriendCard {
		g.FriendRevealed = true
		g.FriendSeat = seat
		step.FriendRevealed = true
	}

	if len(g.Trick) < MTPlayerCount {
		g.Turn = (seat + 1) % MTPlayerCount
		return step, nil
	}

	// ---- 트릭 완성 ----
	winnerIdx := mtTrickWinner(g.Trick, g.LedSuit, g.Trump, g.TrickNo, g.JokerCallActive)
	winner := g.Trick[winnerIdx].Seat
	g.TricksWon[winner]++
	for _, p := range g.Trick {
		if mtIsScoreCard(p.Card) {
			g.CapturedPoints[winner]++
		}
	}
	g.LastTrick = &MTLastTrick{Winner: winner, Cards: append([]MTTrickPlay{}, g.Trick...)}
	step.TrickDone = true
	step.TrickWinner = winner
	step.WonTrickNo = g.TrickNo

	// 첫 트릭 승자 프렌드
	if g.FriendType == "first_trick" && g.TrickNo == 1 && !g.FriendRevealed {
		g.FriendRevealed = true
		if winner != g.Declarer {
			g.FriendSeat = winner
			step.FriendRevealed = true
		}
	}

	g.Trick = []MTTrickPlay{}
	g.LedSuit = ""
	g.JokerCallActive = false

	if g.TrickNo >= MTTrickCount {
		g.settle()
		step.GameOver = true
		g.Turn = -1
		return step, nil
	}
	g.TrickNo++
	g.Turn = winner
	return step, nil
}

// settle 정산 — 주공팀(주공+프렌드) 점수 카드 수(키티 버림 포함) ≥ 공약이면 성공.
// 프렌드가 끝까지 공개되지 않았으면(프렌드 카드가 키티에 묻힌 경우 등) 주공 단독.
func (g *MTGame) settle() {
	declSeats := map[int]bool{g.Declarer: true}
	friendSeat := -1
	if g.FriendRevealed && g.FriendSeat >= 0 && g.FriendSeat != g.Declarer {
		friendSeat = g.FriendSeat
		declSeats[friendSeat] = true
	}

	declarerPoints := g.KittyPoints
	declTeam, defTeam := []string{}, []string{}
	for _, p := range g.Players {
		if declSeats[p.Seat] {
			declarerPoints += g.CapturedPoints[p.Seat]
			declTeam = append(declTeam, p.Name)
		} else {
			defTeam = append(defTeam, p.Name)
		}
	}

	g.Result = &MTResultView{
		Win:            declarerPoints >= g.ContractCount,
		DeclarerPoints: declarerPoints,
		DefenderPoints: MTScoreTotal - declarerPoints,
		Contract:       MTContractView{Declarer: g.Declarer, Suit: g.Trump, Count: g.ContractCount},
		FriendSeat:     friendSeat,
		DeclarerTeam:   declTeam,
		DefenderTeam:   defTeam,
	}
	g.Phase = MTPhaseGameOver
}
