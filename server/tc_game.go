package server

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"time"
)

// ==================== 카드 ====================

var tcSuits = []byte{'G', 'B', 'U', 'R'}

// tcDeck 56장 전체 덱
func tcDeck() []string {
	deck := make([]string, 0, 56)
	for _, s := range tcSuits {
		for r := 2; r <= 14; r++ {
			deck = append(deck, string(s)+strconv.Itoa(r))
		}
	}
	return append(deck, tcMah, tcDog, tcPhx, tcDrg)
}

// tcIsSpecial 특수 카드 여부
func tcIsSpecial(card string) bool {
	return card == tcMah || card == tcDog || card == tcPhx || card == tcDrg
}

// tcCardRank 숫자 랭크. MAH=1, 일반 2..14, 그 외 특수(DOG/PHX/DRG)=0.
func tcCardRank(card string) int {
	if card == tcMah {
		return 1
	}
	if tcIsSpecial(card) {
		return 0
	}
	if len(card) < 2 {
		return 0
	}
	r, err := strconv.Atoi(card[1:])
	if err != nil || r < 2 || r > 14 {
		return 0
	}
	return r
}

// tcCardSuit 무늬 (특수/무효 카드는 0)
func tcCardSuit(card string) byte {
	if tcIsSpecial(card) || len(card) < 2 {
		return 0
	}
	s := card[0]
	for _, v := range tcSuits {
		if s == v {
			return s
		}
	}
	return 0
}

// tcValidCard 와이어 카드 코드 검증
func tcValidCard(card string) bool {
	if tcIsSpecial(card) {
		return true
	}
	return tcCardSuit(card) != 0 && tcCardRank(card) >= 2
}

// tcCardPoints 카드 점수: 5=5, 10·K=10, DRG=+25, PHX=-25
func tcCardPoints(card string) int {
	switch card {
	case tcDrg:
		return 25
	case tcPhx:
		return -25
	case tcMah, tcDog:
		return 0
	}
	switch tcCardRank(card) {
	case 5:
		return 5
	case 10, 13:
		return 10
	}
	return 0
}

// tcHandPoints 카드 묶음의 점수 합
func tcHandPoints(cards []string) int {
	sum := 0
	for _, c := range cards {
		sum += tcCardPoints(c)
	}
	return sum
}

// tcSortKey 손패 정렬 키: 특수 먼저(MAH·DOG·PHX·DRG), 숫자 오름차순
func tcSortKey(card string) int {
	switch card {
	case tcMah:
		return 0
	case tcDog:
		return 1
	case tcPhx:
		return 2
	case tcDrg:
		return 3
	}
	suit := 0
	for i, s := range tcSuits {
		if tcCardSuit(card) == s {
			suit = i
		}
	}
	return 10 + tcCardRank(card)*4 + suit
}

// tcSortHand 손패 정렬 (복사본 반환)
func tcSortHand(hand []string) []string {
	out := append([]string(nil), hand...)
	sort.Slice(out, func(i, j int) bool { return tcSortKey(out[i]) < tcSortKey(out[j]) })
	return out
}

// tcContainsCard 카드 포함 여부
func tcContainsCard(cards []string, card string) bool {
	for _, c := range cards {
		if c == card {
			return true
		}
	}
	return false
}

// tcContainsRank 자연 카드(봉황 제외)로 해당 랭크를 포함하는지 — 소원 이행 판정
func tcContainsRank(cards []string, rank int) bool {
	for _, c := range cards {
		if c != tcPhx && tcCardRank(c) == rank {
			return true
		}
	}
	return false
}

// tcRemoveCards hand 에서 cards 를 제거한 새 슬라이스
func tcRemoveCards(hand, cards []string) []string {
	out := make([]string, 0, len(hand))
	for _, c := range hand {
		if !tcContainsCard(cards, c) {
			out = append(out, c)
		}
	}
	return out
}

// ==================== 콤보 판정 ====================

// tcParseCombo 낸 카드가 어떤 콤보인지 판정한다. prev 는 봉황 싱글의
// 강도 계산에만 쓰인다 (직전 싱글 +0.5 → 2배 정수로 prev.Value+1).
func tcParseCombo(cards []string, prev *tcCombo) (*tcCombo, error) {
	n := len(cards)
	if n == 0 {
		return nil, errors.New("카드를 선택하세요")
	}
	seen := map[string]bool{}
	for _, c := range cards {
		if !tcValidCard(c) {
			return nil, fmt.Errorf("잘못된 카드: %s", c)
		}
		if seen[c] {
			return nil, fmt.Errorf("중복 카드: %s", c)
		}
		seen[c] = true
	}

	// 개: 단독으로만
	if tcContainsCard(cards, tcDog) {
		if n != 1 {
			return nil, errors.New("개는 단독으로만 낼 수 있습니다")
		}
		return &tcCombo{Kind: tcDogCombo, Cards: cards, Value: 0, Len: 1}, nil
	}
	// 용: 싱글로만
	if tcContainsCard(cards, tcDrg) && n != 1 {
		return nil, errors.New("용은 싱글로만 낼 수 있습니다")
	}

	if n == 1 {
		c := cards[0]
		combo := &tcCombo{Kind: tcSingle, Cards: cards, Len: 1}
		switch c {
		case tcDrg:
			combo.Value = 30 // 15*2: 폭탄 외 최강
		case tcPhx:
			if prev != nil && prev.Kind == tcSingle {
				if prev.Cards[0] == tcDrg {
					return nil, errors.New("봉황으로 용을 이길 수 없습니다")
				}
				combo.Value = prev.Value + 1
			} else {
				combo.Value = 3 // 리드 시 1.5
			}
		default:
			combo.Value = tcCardRank(c) * 2
		}
		return combo, nil
	}

	phx := tcContainsCard(cards, tcPhx)
	hasMah := tcContainsCard(cards, tcMah)
	// 랭크별 자연 카드 수 (MAH=1 포함, PHX 제외)
	counts := map[int]int{}
	naturals := 0
	for _, c := range cards {
		if c == tcPhx {
			continue
		}
		counts[tcCardRank(c)]++
		naturals++
	}

	// 폭탄(같은 숫자 4장) — 봉황 불가
	if n == 4 && !phx && !hasMah && len(counts) == 1 {
		for r := range counts {
			return &tcCombo{Kind: tcBomb, Cards: cards, Value: r * 2, Len: 4}, nil
		}
	}

	// 같은 무늬 스트레이트 폭탄 (5장 이상, 특수 카드 불가)
	if n >= 5 && !phx && !hasMah {
		suit := tcCardSuit(cards[0])
		sameSuit := suit != 0
		for _, c := range cards {
			if tcCardSuit(c) != suit {
				sameSuit = false
				break
			}
		}
		if sameSuit && len(counts) == n {
			if top, ok := tcConsecutiveTop(counts); ok {
				return &tcCombo{Kind: tcBombSF, Cards: cards, Value: top * 2, Len: n}, nil
			}
		}
	}

	// 페어 / 트리플 (봉황 와일드 가능, MAH 불가)
	if n == 2 || n == 3 {
		if hasMah {
			return nil, errors.New("잘못된 조합입니다")
		}
		if len(counts) == 1 && naturals >= 1 {
			for r := range counts {
				if naturals+boolInt(phx) == n {
					kind := tcPair
					if n == 3 {
						kind = tcTriple
					}
					return &tcCombo{Kind: kind, Cards: cards, Value: r * 2, Len: n}, nil
				}
			}
		}
		return nil, errors.New("잘못된 조합입니다")
	}

	// 스트레이트 (5장 이상, 랭크 모두 다름, 봉황은 한 칸 채움/연장)
	if n >= 5 && len(counts) == naturals {
		if top, ok := tcStraightTop(counts, phx); ok {
			return &tcCombo{Kind: tcStraight, Cards: cards, Value: top * 2, Len: n}, nil
		}
	}

	// 풀하우스 (5장 = 트리플+페어)
	if n == 5 && !hasMah {
		if triple, ok := tcFullhouseTriple(counts, phx); ok {
			return &tcCombo{Kind: tcFullhouse, Cards: cards, Value: triple * 2, Len: 5}, nil
		}
	}

	// 연속 페어 (2쌍 이상, 봉황은 한 장 채움)
	if n >= 4 && n%2 == 0 && !hasMah {
		if top, ok := tcPairSeqTop(counts, phx, n); ok {
			return &tcCombo{Kind: tcPairSeq, Cards: cards, Value: top * 2, Len: n}, nil
		}
	}

	return nil, errors.New("잘못된 조합입니다")
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// tcConsecutiveTop 랭크가 빈틈없이 연속이면 최고 랭크를 돌려준다
func tcConsecutiveTop(counts map[int]int) (int, bool) {
	min, max := 99, 0
	for r := range counts {
		if r < min {
			min = r
		}
		if r > max {
			max = r
		}
	}
	if max-min+1 != len(counts) {
		return 0, false
	}
	return max, true
}

// tcStraightTop 스트레이트의 최고 랭크. 봉황은 내부 한 칸을 채우거나
// 위/아래로 연장한다 (봉황은 2..14 만 대신할 수 있고, 위 연장을 우선한다).
func tcStraightTop(counts map[int]int, phx bool) (int, bool) {
	min, max := 99, 0
	for r := range counts {
		if r < min {
			min = r
		}
		if r > max {
			max = r
		}
	}
	span := max - min + 1
	if !phx {
		if span == len(counts) {
			return max, true
		}
		return 0, false
	}
	// 봉황 1장 포함: 내부 빈칸 1개를 채우거나 (빈칸은 2 이상 랭크)
	if span == len(counts)+1 {
		// 빈칸이 정확히 1개인지 확인
		missing := 0
		for r := min; r <= max; r++ {
			if counts[r] == 0 {
				missing++
			}
		}
		if missing == 1 {
			return max, true
		}
		return 0, false
	}
	// 빈칸 없이 위/아래 연장
	if span == len(counts) {
		if max < 14 {
			return max + 1, true
		}
		if min > 2 {
			return max, true
		}
		return 0, false
	}
	return 0, false
}

// tcFullhouseTriple 풀하우스의 트리플 랭크. 봉황이 있으면 3+1 또는 2+2 를
// 완성한다 (2+2+봉황은 높은 랭크를 트리플로 본다).
func tcFullhouseTriple(counts map[int]int, phx bool) (int, bool) {
	if !phx {
		if len(counts) != 2 {
			return 0, false
		}
		for r, c := range counts {
			if c == 3 {
				return r, true
			}
			if c != 2 {
				return 0, false
			}
		}
		return 0, false
	}
	if len(counts) != 2 {
		return 0, false
	}
	ranks := []int{}
	for r := range counts {
		ranks = append(ranks, r)
	}
	sort.Ints(ranks)
	a, b := ranks[0], ranks[1] // a < b
	ca, cb := counts[a], counts[b]
	switch {
	case ca == 3 && cb == 1:
		return a, true
	case ca == 1 && cb == 3:
		return b, true
	case ca == 2 && cb == 2:
		return b, true // 봉황을 높은 쪽에 붙인다
	}
	return 0, false
}

// tcPairSeqTop 연속 페어의 최고 랭크. 각 랭크 2장씩, 봉황은 한 장만 채운다.
func tcPairSeqTop(counts map[int]int, phx bool, n int) (int, bool) {
	pairs := n / 2
	if len(counts) != pairs {
		return 0, false
	}
	top, ok := tcConsecutiveTop(counts)
	if !ok {
		return 0, false
	}
	singles := 0
	for _, c := range counts {
		switch c {
		case 2:
		case 1:
			singles++
		default:
			return 0, false
		}
	}
	if singles == 0 && !phx {
		return top, true
	}
	if singles == 1 && phx {
		return top, true
	}
	return 0, false
}

// tcIsBombKind 폭탄 계열 여부
func tcIsBombKind(kind string) bool {
	return kind == tcBomb || kind == tcBombSF
}

// tcBeats next 가 prev 를 이기는지. prev 는 nil 이 아니어야 한다.
func tcBeats(prev, next *tcCombo) bool {
	if next.Kind == tcDogCombo {
		return false
	}
	nb, pb := tcIsBombKind(next.Kind), tcIsBombKind(prev.Kind)
	if nb {
		if !pb {
			return true
		}
		if next.Kind == tcBombSF && prev.Kind == tcBomb {
			return true
		}
		if next.Kind == tcBomb && prev.Kind == tcBombSF {
			return false
		}
		if next.Kind == tcBombSF && prev.Kind == tcBombSF && next.Len != prev.Len {
			return next.Len > prev.Len
		}
		return next.Value > prev.Value
	}
	if pb {
		return false
	}
	if next.Kind != prev.Kind || next.Len != prev.Len {
		return false
	}
	return next.Value > prev.Value
}

// ==================== 콤보 열거 ====================
//
// 봇의 수 계산과 서버의 소원 강제 판정이 이 열거를 공유한다.
// 카드 선택은 랭크 단위 대표(같은 랭크의 아무 카드)로 충분하다 —
// 소원 판정은 랭크 기준이고 콤보 강도도 랭크로만 정해진다.

// tcHandGroups 손패를 랭크별로 묶는다
func tcHandGroups(hand []string) (rankCards map[int][]string, phx, drg, dog, mah bool) {
	rankCards = map[int][]string{}
	for _, c := range hand {
		switch c {
		case tcPhx:
			phx = true
		case tcDrg:
			drg = true
		case tcDog:
			dog = true
		default:
			r := tcCardRank(c) // MAH 는 1
			rankCards[r] = append(rankCards[r], c)
			if c == tcMah {
				mah = true
			}
		}
	}
	return
}

// tcEnumerate 손패에서 현재 콤보(prev, nil=리드)를 상대로 낼 수 있는 모든
// 합법 수를 돌려준다. wish(2..14) 활성 시 그 랭크를 자연 카드로 포함한 수가
// 하나라도 있으면 (그 수들만, forced=true)를 돌려준다 — 소원 강제 규칙.
func tcEnumerate(hand []string, prev *tcCombo, wish int) (plays [][]string, forced bool) {
	var all [][]string
	if prev == nil {
		all = tcLeadPlays(hand)
	} else {
		all = tcFollowPlays(hand, prev)
	}
	if wish >= 2 {
		var wished [][]string
		for _, p := range all {
			if tcContainsRank(p, wish) {
				wished = append(wished, p)
			}
		}
		if len(wished) > 0 {
			return wished, true
		}
	}
	return all, false
}

// tcLeadPlays 리드에서 가능한 모든 콤보 (개 포함, 폭탄 포함)
func tcLeadPlays(hand []string) [][]string {
	rankCards, phx, drg, dog, _ := tcHandGroups(hand)
	var plays [][]string

	// 싱글
	for _, cs := range rankCards {
		plays = append(plays, []string{cs[0]})
	}
	if phx {
		plays = append(plays, []string{tcPhx})
	}
	if drg {
		plays = append(plays, []string{tcDrg})
	}
	if dog {
		plays = append(plays, []string{tcDog})
	}

	// 페어·트리플·폭탄
	for r, cs := range rankCards {
		if r < 2 {
			continue
		}
		if len(cs) >= 2 {
			plays = append(plays, []string{cs[0], cs[1]})
		}
		if phx && len(cs) >= 1 {
			plays = append(plays, []string{cs[0], tcPhx})
		}
		if len(cs) >= 3 {
			plays = append(plays, []string{cs[0], cs[1], cs[2]})
		}
		if phx && len(cs) >= 2 {
			plays = append(plays, []string{cs[0], cs[1], tcPhx})
		}
		if len(cs) == 4 {
			plays = append(plays, append([]string(nil), cs...))
		}
	}

	plays = append(plays, tcStraightPlays(rankCards, phx, 0)...)
	plays = append(plays, tcFullhousePlays(rankCards, phx, 0)...)
	plays = append(plays, tcPairSeqPlays(rankCards, phx, 0, 0)...)
	plays = append(plays, tcBombSFPlays(hand, nil)...)
	return plays
}

// tcFollowPlays prev 를 이길 수 있는 모든 수 (폭탄 포함)
func tcFollowPlays(hand []string, prev *tcCombo) [][]string {
	rankCards, phx, drg, _, _ := tcHandGroups(hand)
	var plays [][]string
	prevBomb := tcIsBombKind(prev.Kind)

	if !prevBomb {
		switch prev.Kind {
		case tcSingle:
			for r, cs := range rankCards {
				if r*2 > prev.Value {
					plays = append(plays, []string{cs[0]})
				}
			}
			if drg {
				plays = append(plays, []string{tcDrg})
			}
			if phx && prev.Cards[0] != tcDrg {
				plays = append(plays, []string{tcPhx})
			}
		case tcPair, tcTriple:
			need := prev.Len
			for r, cs := range rankCards {
				if r < 2 || r*2 <= prev.Value {
					continue
				}
				if len(cs) >= need {
					plays = append(plays, append([]string(nil), cs[:need]...))
				} else if phx && len(cs) >= need-1 {
					p := append([]string(nil), cs[:need-1]...)
					plays = append(plays, append(p, tcPhx))
				}
			}
		case tcStraight:
			for _, p := range tcStraightPlays(rankCards, phx, prev.Len) {
				if top := tcPlayStraightTop(p); top*2 > prev.Value {
					plays = append(plays, p)
				}
			}
		case tcFullhouse:
			plays = append(plays, tcFullhousePlays(rankCards, phx, prev.Value)...)
		case tcPairSeq:
			plays = append(plays, tcPairSeqPlays(rankCards, phx, prev.Len, prev.Value)...)
		}
	}

	// 폭탄은 언제나 후보 (프리브가 폭탄이면 더 센 폭탄만)
	for r, cs := range rankCards {
		if r < 2 || len(cs) != 4 {
			continue
		}
		bomb := &tcCombo{Kind: tcBomb, Value: r * 2, Len: 4}
		if tcBeats(prev, bomb) {
			plays = append(plays, append([]string(nil), cs...))
		}
	}
	plays = append(plays, tcBombSFPlays(hand, prev)...)
	return plays
}

// tcPlayStraightTop 열거된 스트레이트 카드 묶음의 최고 랭크 (봉황 연장 반영)
func tcPlayStraightTop(cards []string) int {
	counts := map[int]int{}
	phx := false
	for _, c := range cards {
		if c == tcPhx {
			phx = true
			continue
		}
		counts[tcCardRank(c)]++
	}
	top, _ := tcStraightTop(counts, phx)
	return top
}

// tcStraightPlays 가능한 스트레이트 전부. wantLen 0 이면 모든 길이(5+).
func tcStraightPlays(rankCards map[int][]string, phx bool, wantLen int) [][]string {
	var plays [][]string
	for start := 1; start <= 10; start++ {
		maxLen := 15 - start
		for length := 5; length <= maxLen; length++ {
			if wantLen != 0 && length != wantLen {
				continue
			}
			missing := 0
			missRank := 0
			for r := start; r < start+length; r++ {
				if len(rankCards[r]) == 0 {
					missing++
					missRank = r
				}
			}
			if missing == 0 {
				p := make([]string, 0, length)
				for r := start; r < start+length; r++ {
					p = append(p, rankCards[r][0])
				}
				plays = append(plays, p)
			} else if missing == 1 && phx && missRank >= 2 {
				p := make([]string, 0, length)
				for r := start; r < start+length; r++ {
					if r == missRank {
						p = append(p, tcPhx)
					} else {
						p = append(p, rankCards[r][0])
					}
				}
				plays = append(plays, p)
			}
		}
	}
	return plays
}

// tcFullhousePlays 가능한 풀하우스 전부 (minValue 초과만).
// 봉황 2+2 조합은 파싱이 높은 랭크를 트리플로 보므로 트리플>페어인 경우만 낸다.
func tcFullhousePlays(rankCards map[int][]string, phx bool, minValue int) [][]string {
	var plays [][]string
	for tr, tcs := range rankCards {
		if tr < 2 || tr*2 <= minValue {
			continue
		}
		tripleNatural := len(tcs) >= 3
		triplePhx := phx && len(tcs) == 2
		if !tripleNatural && !triplePhx {
			continue
		}
		for pr, pcs := range rankCards {
			if pr == tr || pr < 2 {
				continue
			}
			if tripleNatural {
				if len(pcs) >= 2 {
					plays = append(plays, []string{tcs[0], tcs[1], tcs[2], pcs[0], pcs[1]})
				} else if phx && len(pcs) == 1 {
					plays = append(plays, []string{tcs[0], tcs[1], tcs[2], pcs[0], tcPhx})
				}
			}
			// 봉황 트리플(2+봉황): 페어는 자연 2장, 파싱 모호성 회피를 위해 tr > pr 만
			if triplePhx && len(pcs) >= 2 && tr > pr {
				plays = append(plays, []string{tcs[0], tcs[1], tcPhx, pcs[0], pcs[1]})
			}
		}
	}
	return plays
}

// tcPairSeqPlays 가능한 연속 페어 전부. wantLen 0 이면 모든 길이.
func tcPairSeqPlays(rankCards map[int][]string, phx bool, wantLen, minValue int) [][]string {
	var plays [][]string
	for start := 2; start <= 13; start++ {
		for pairs := 2; start+pairs-1 <= 14; pairs++ {
			n := pairs * 2
			if wantLen != 0 && n != wantLen {
				continue
			}
			top := start + pairs - 1
			if minValue != 0 && top*2 <= minValue {
				continue
			}
			phxLeft := 0
			if phx {
				phxLeft = 1
			}
			p := make([]string, 0, n)
			ok := true
			for r := start; r <= top; r++ {
				cs := rankCards[r]
				switch {
				case len(cs) >= 2:
					p = append(p, cs[0], cs[1])
				case len(cs) == 1 && phxLeft > 0:
					p = append(p, cs[0], tcPhx)
					phxLeft--
				default:
					ok = false
				}
				if !ok {
					break
				}
			}
			if ok {
				plays = append(plays, p)
			}
		}
	}
	return plays
}

// tcBombSFPlays 같은 무늬 스트레이트 폭탄 전부. prev 가 있으면 이기는 것만.
func tcBombSFPlays(hand []string, prev *tcCombo) [][]string {
	var plays [][]string
	for _, suit := range tcSuits {
		byRank := map[int]string{}
		for _, c := range hand {
			if tcCardSuit(c) == suit {
				byRank[tcCardRank(c)] = c
			}
		}
		for start := 2; start <= 10; start++ {
			for length := 5; start+length-1 <= 14; length++ {
				ok := true
				p := make([]string, 0, length)
				for r := start; r < start+length; r++ {
					c, has := byRank[r]
					if !has {
						ok = false
						break
					}
					p = append(p, c)
				}
				if !ok {
					break // 이 시작점에선 더 긴 것도 불가
				}
				top := start + length - 1
				if prev != nil && !tcBeats(prev, &tcCombo{Kind: tcBombSF, Value: top * 2, Len: length}) {
					continue
				}
				plays = append(plays, p)
			}
		}
	}
	return plays
}

// ==================== 게임 상태 머신 ====================

// NewTCGame 대기실 상태의 새 게임
func NewTCGame(id string) *TCGame {
	return &TCGame{
		ID:                id,
		Target:            TCDefaultTarget,
		Phase:             TCPhaseWaiting,
		CurrentTurn:       -1,
		LastPlayerSeat:    -1,
		DragonPendingSeat: -1,
	}
}

// AddPlayer 빈 좌석에 입장 (입장 순서 = 좌석 순서)
func (g *TCGame) AddPlayer(name string) (int, error) {
	if g.Phase != TCPhaseWaiting {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	for s := 0; s < TCSeats; s++ {
		if g.Names[s] == "" {
			g.Names[s] = name
			g.PlayerCount++
			return s, nil
		}
	}
	return -1, errors.New("자리가 없습니다")
}

// RemovePlayer 대기실에서 좌석을 비우고 뒷좌석을 앞으로 당긴다
func (g *TCGame) RemovePlayer(seat int) {
	if g.Phase != TCPhaseWaiting || seat < 0 || seat >= TCSeats || g.Names[seat] == "" {
		return
	}
	for s := seat; s < TCSeats-1; s++ {
		g.Names[s] = g.Names[s+1]
	}
	g.Names[TCSeats-1] = ""
	g.PlayerCount--
}

// SetTarget 목표점수 설정 (대기실에서만)
func (g *TCGame) SetTarget(target int) error {
	if g.Phase != TCPhaseWaiting {
		return errors.New("게임 시작 후에는 바꿀 수 없습니다")
	}
	if target != 500 && target != 1000 {
		return errors.New("목표점수는 500 또는 1000 이어야 합니다")
	}
	g.Target = target
	return nil
}

// StartHand 새 핸드 시작: 셔플·배분 후 그랜드 선언 단계로
func (g *TCGame) StartHand(rng *rand.Rand) {
	if !g.Started {
		g.Started = true
		g.StartedAt = time.Now()
	}
	deck := tcDeck()
	rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })

	g.HandNo++
	g.Phase = TCPhaseGrand
	g.CurrentTurn = -1
	g.LastPlayerSeat = -1
	g.Trick = nil
	g.lastCombo = nil
	g.WishRank = 0
	g.DragonPendingSeat = -1
	g.OutCount = 0
	g.HandResult = nil
	for s := 0; s < TCSeats; s++ {
		g.Dealt[s] = append([]string(nil), deck[s*14:(s+1)*14]...)
		g.Hands[s] = append([]string(nil), g.Dealt[s]...)
		g.Won[s] = nil
		g.GrandAnswered[s] = false
		g.Tichu[s] = ""
		g.PlayedAny[s] = false
		g.ExchangeSub[s] = nil
		g.Out[s] = false
		g.OutRank[s] = 0
		g.Ready[s] = false
	}
}

// CallGrand 그랜드 티츄 선언 or 패스. 전원 응답 시 교환 단계로 넘어간다.
func (g *TCGame) CallGrand(seat int, call bool) (allAnswered bool, err error) {
	if g.Phase != TCPhaseGrand {
		return false, errors.New("그랜드 선언 단계가 아닙니다")
	}
	if g.GrandAnswered[seat] {
		return false, errors.New("이미 응답했습니다")
	}
	g.GrandAnswered[seat] = true
	if call {
		g.Tichu[seat] = "grand"
	}
	for s := 0; s < TCSeats; s++ {
		if !g.GrandAnswered[s] {
			return false, nil
		}
	}
	g.Phase = TCPhaseExchange
	return true, nil
}

// CallTichu 일반 티츄 선언 (교환 단계부터 첫 카드 내기 전까지)
func (g *TCGame) CallTichu(seat int) error {
	if g.Phase != TCPhaseExchange && g.Phase != TCPhasePlay {
		return errors.New("지금은 티츄를 선언할 수 없습니다")
	}
	if g.Tichu[seat] != "" {
		return errors.New("이미 선언했습니다")
	}
	if g.PlayedAny[seat] {
		return errors.New("첫 카드를 낸 후에는 선언할 수 없습니다")
	}
	g.Tichu[seat] = "tichu"
	return nil
}

// SubmitExchange 교환 카드 3장 제출. 전원 제출 시 일괄 교환하고 play 로 넘어간다.
func (g *TCGame) SubmitExchange(seat int, sub TCExchangePayload) (allDone bool, err error) {
	if g.Phase != TCPhaseExchange {
		return false, errors.New("교환 단계가 아닙니다")
	}
	if g.ExchangeSub[seat] != nil {
		return false, errors.New("이미 제출했습니다")
	}
	cards := []string{sub.Left, sub.Partner, sub.Right}
	seen := map[string]bool{}
	for _, c := range cards {
		if !tcContainsCard(g.Hands[seat], c) {
			return false, fmt.Errorf("손에 없는 카드입니다: %s", c)
		}
		if seen[c] {
			return false, errors.New("서로 다른 카드 3장을 골라야 합니다")
		}
		seen[c] = true
	}
	s := sub
	g.ExchangeSub[seat] = &s
	for i := 0; i < TCSeats; i++ {
		if g.ExchangeSub[i] == nil {
			return false, nil
		}
	}
	g.applyExchange()
	return true, nil
}

// applyExchange 전원 제출된 교환을 일괄 반영하고 MAH 소지자를 선공으로
func (g *TCGame) applyExchange() {
	for s := 0; s < TCSeats; s++ {
		sub := g.ExchangeSub[s]
		g.Hands[s] = tcRemoveCards(g.Hands[s], []string{sub.Left, sub.Partner, sub.Right})
	}
	for s := 0; s < TCSeats; s++ {
		sub := g.ExchangeSub[s]
		g.Hands[(s+3)%4] = append(g.Hands[(s+3)%4], sub.Left)
		g.Hands[(s+2)%4] = append(g.Hands[(s+2)%4], sub.Partner)
		g.Hands[(s+1)%4] = append(g.Hands[(s+1)%4], sub.Right)
	}
	g.Phase = TCPhasePlay
	for s := 0; s < TCSeats; s++ {
		if tcContainsCard(g.Hands[s], tcMah) {
			g.CurrentTurn = s
			break
		}
	}
}

// nextActiveAfter seat 다음의 아웃되지 않은 좌석 (-1 = 없음)
func (g *TCGame) nextActiveAfter(seat int) int {
	for i := 1; i <= TCSeats; i++ {
		s := (seat + i) % TCSeats
		if !g.Out[s] {
			return s
		}
	}
	return -1
}

// tcPlayResult Play 가 허브에 알려주는 결과 (이벤트 발행용)
type tcPlayResult struct {
	Combo     *tcCombo
	OutOfTurn bool
	Dog       bool
	WishSet   int
	WishDone  bool
	PlayerOut bool
	OutRank   int
	HandEnded bool
	GameOver  bool
}

// Play seat 가 cards 를 낸다. wish 는 MAH 포함 시 소원 숫자(0=없음).
// 차례가 아니어도 폭탄이면 허용된다 (트릭이 비어 있지 않을 때).
func (g *TCGame) Play(seat int, cards []string, wish int) (*tcPlayResult, error) {
	if g.Phase != TCPhasePlay {
		return nil, errors.New("플레이 단계가 아닙니다")
	}
	if g.DragonPendingSeat >= 0 {
		return nil, errors.New("용 트릭 전달을 기다리는 중입니다")
	}
	if g.Out[seat] {
		return nil, errors.New("이미 아웃되었습니다")
	}
	for _, c := range cards {
		if !tcContainsCard(g.Hands[seat], c) {
			return nil, fmt.Errorf("손에 없는 카드입니다: %s", c)
		}
	}

	prev := g.lastCombo
	combo, err := tcParseCombo(cards, prev)
	if err != nil {
		return nil, err
	}

	outOfTurn := seat != g.CurrentTurn
	if outOfTurn {
		if !tcIsBombKind(combo.Kind) {
			return nil, errors.New("차례가 아닙니다")
		}
		if prev == nil {
			return nil, errors.New("리드 전에는 폭탄을 낼 수 없습니다")
		}
		if !tcBeats(prev, combo) {
			return nil, errors.New("현재 콤보를 이길 수 없습니다")
		}
	} else {
		if combo.Kind == tcDogCombo {
			if prev != nil {
				return nil, errors.New("개는 리드로만 낼 수 있습니다")
			}
		} else if prev != nil && !tcBeats(prev, combo) {
			return nil, errors.New("현재 콤보를 이길 수 없습니다")
		}
		// 소원 강제: 이행 가능한 수가 있으면 반드시 소원 숫자를 포함해야 한다
		if g.WishRank >= 2 && !tcContainsRank(cards, g.WishRank) {
			if _, forced := tcEnumerate(g.Hands[seat], prev, g.WishRank); forced {
				return nil, fmt.Errorf("소원(%d)을 포함한 수를 내야 합니다", g.WishRank)
			}
		}
	}

	res := &tcPlayResult{Combo: combo, OutOfTurn: outOfTurn}

	// 소원 선언 (MAH 포함 시에만)
	if wish != 0 {
		if !tcContainsCard(cards, tcMah) {
			return nil, errors.New("참새 없이 소원을 빌 수 없습니다")
		}
		if wish < 2 || wish > 14 {
			return nil, errors.New("소원 숫자는 2..14 여야 합니다")
		}
		g.WishRank = wish
		res.WishSet = wish
	}
	// 소원 이행
	if g.WishRank >= 2 && tcContainsRank(cards, g.WishRank) {
		g.WishRank = 0
		res.WishDone = true
	}

	g.Hands[seat] = tcRemoveCards(g.Hands[seat], cards)
	g.PlayedAny[seat] = true

	// 아웃 처리 (턴 계산 전에 반영)
	if len(g.Hands[seat]) == 0 {
		g.Out[seat] = true
		g.OutCount++
		g.OutRank[seat] = g.OutCount
		res.PlayerOut = true
		res.OutRank = g.OutCount
	}

	if combo.Kind == tcDogCombo {
		res.Dog = true
		// 개는 자기 더미로, 리드는 파트너에게 (아웃이면 그 다음 사람)
		g.Won[seat] = append(g.Won[seat], tcDog)
		g.Trick = nil
		g.lastCombo = nil
		g.LastPlayerSeat = -1
		partner := (seat + 2) % TCSeats
		if g.Out[partner] {
			partner = g.nextActiveAfter(partner)
		}
		g.CurrentTurn = partner
	} else {
		play := TCTrickPlay{Seat: seat, Cards: tcSortHand(cards), Combo: combo.Kind}
		if combo.Kind == tcSingle && cards[0] == tcPhx {
			play.Phx = combo.Value
		}
		g.Trick = append(g.Trick, play)
		g.lastCombo = combo
		g.LastPlayerSeat = seat
		g.CurrentTurn = g.nextActiveAfter(seat)
	}

	// 핸드 종료: 원투(1·2위 같은 팀) 또는 3명 아웃
	if g.handShouldEnd() {
		g.finishHand()
		res.HandEnded = true
		res.GameOver = g.Phase == TCPhaseGameOver
	}
	return res, nil
}

// tcPassResult Pass 결과
type tcPassResult struct {
	TrickWon      bool
	WinnerSeat    int
	DragonPending bool
}

// Pass 차례를 넘긴다. 전원 패스면 마지막 낸 사람이 트릭을 가져간다.
func (g *TCGame) Pass(seat int) (*tcPassResult, error) {
	if g.Phase != TCPhasePlay {
		return nil, errors.New("플레이 단계가 아닙니다")
	}
	if g.DragonPendingSeat >= 0 {
		return nil, errors.New("용 트릭 전달을 기다리는 중입니다")
	}
	if seat != g.CurrentTurn {
		return nil, errors.New("차례가 아닙니다")
	}
	if g.lastCombo == nil {
		return nil, errors.New("리드는 패스할 수 없습니다")
	}
	if g.WishRank >= 2 {
		if _, forced := tcEnumerate(g.Hands[seat], g.lastCombo, g.WishRank); forced {
			return nil, fmt.Errorf("소원(%d)을 이행할 수 있어 패스할 수 없습니다", g.WishRank)
		}
	}

	res := &tcPassResult{WinnerSeat: -1}
	// 다음 응답자: 마지막 낸 사람에게 닿으면 트릭 종료 (아웃 좌석은 건너뜀)
	next := -1
	for i := 1; i <= TCSeats; i++ {
		s := (seat + i) % TCSeats
		if s == g.LastPlayerSeat || !g.Out[s] {
			next = s
			break
		}
	}
	if next != g.LastPlayerSeat {
		g.CurrentTurn = next
		return res, nil
	}

	winner := g.LastPlayerSeat
	res.WinnerSeat = winner
	if g.lastCombo.Kind == tcSingle && g.lastCombo.Cards[0] == tcDrg {
		// 용으로 먹은 트릭은 상대에게 넘겨야 한다
		g.DragonPendingSeat = winner
		g.CurrentTurn = -1
		res.DragonPending = true
	} else {
		g.awardTrick(winner)
		res.TrickWon = true
		g.setLeadAfterTrick(winner)
	}
	return res, nil
}

// awardTrick 테이블의 트릭 카드를 전부 to 의 더미로 옮긴다
func (g *TCGame) awardTrick(to int) {
	for _, p := range g.Trick {
		g.Won[to] = append(g.Won[to], p.Cards...)
	}
	g.Trick = nil
	g.lastCombo = nil
	g.LastPlayerSeat = -1
}

// setLeadAfterTrick 트릭 승자가 다음 리드 (아웃이면 그 다음 사람)
func (g *TCGame) setLeadAfterTrick(winner int) {
	if g.Out[winner] {
		g.CurrentTurn = g.nextActiveAfter(winner)
	} else {
		g.CurrentTurn = winner
	}
}

// DragonGive 용으로 먹은 트릭을 상대팀 좌석에게 전달
func (g *TCGame) DragonGive(seat, target int) error {
	if g.Phase != TCPhasePlay || g.DragonPendingSeat != seat {
		return errors.New("용 트릭 전달 차례가 아닙니다")
	}
	if target < 0 || target >= TCSeats || (target-seat)%2 == 0 {
		return errors.New("상대팀 좌석을 골라야 합니다")
	}
	g.DragonPendingSeat = -1
	g.awardTrick(target)
	g.setLeadAfterTrick(seat)
	return nil
}

// handShouldEnd 원투 피니시 또는 3명 아웃
func (g *TCGame) handShouldEnd() bool {
	if g.OutCount >= 3 {
		return true
	}
	if g.OutCount == 2 {
		var first, second = -1, -1
		for s := 0; s < TCSeats; s++ {
			switch g.OutRank[s] {
			case 1:
				first = s
			case 2:
				second = s
			}
		}
		return (first+second)%2 == 0 // 같은 팀 (0·2 또는 1·3)
	}
	return false
}

// finishHand 핸드 정산: 점수 계산·누적, hand_end 또는 game_over 로 전이
func (g *TCGame) finishHand() {
	// 테이블에 남은 트릭은 마지막 낸 사람에게
	if g.LastPlayerSeat >= 0 && len(g.Trick) > 0 {
		g.awardTrick(g.LastPlayerSeat)
	}

	first := -1
	for s := 0; s < TCSeats; s++ {
		if g.OutRank[s] == 1 {
			first = s
		}
	}

	d02, d13 := 0, 0
	detail := ""

	oneTwo := g.OutCount == 2
	if oneTwo {
		if first%2 == 0 {
			d02 += 200
			detail = "원투 피니시! 0·2팀 +200"
		} else {
			d13 += 200
			detail = "원투 피니시! 1·3팀 +200"
		}
	} else {
		loser := -1
		for s := 0; s < TCSeats; s++ {
			if !g.Out[s] {
				loser = s
			}
		}
		// 꼴찌의 남은 손패는 상대팀에게, 꼴찌가 먹은 트릭은 1위에게
		pts := [TCSeats]int{}
		for s := 0; s < TCSeats; s++ {
			pts[s] = tcHandPoints(g.Won[s])
		}
		pts[first] += pts[loser]
		pts[loser] = 0
		loserHand := tcHandPoints(g.Hands[loser])
		p02 := pts[0] + pts[2]
		p13 := pts[1] + pts[3]
		if loser%2 == 0 {
			p13 += loserHand
		} else {
			p02 += loserHand
		}
		d02 += p02
		d13 += p13
		detail = fmt.Sprintf("카드점수 0·2팀 %d : 1·3팀 %d", p02, p13)
	}

	// 티츄/그랜드 보너스 (원투에도 적용)
	for s := 0; s < TCSeats; s++ {
		if g.Tichu[s] == "" {
			continue
		}
		bonus := 100
		label := "티츄"
		if g.Tichu[s] == "grand" {
			bonus = 200
			label = "그랜드 티츄"
		}
		if g.OutRank[s] != 1 {
			bonus = -bonus
		}
		if s%2 == 0 {
			d02 += bonus
		} else {
			d13 += bonus
		}
		verb := "성공"
		if bonus < 0 {
			verb = "실패"
		}
		detail += fmt.Sprintf(" · %s %s %s %+d", g.Names[s], label, verb, bonus)
	}

	g.Score02 += d02
	g.Score13 += d13
	g.HandResult = &TCHandResult{Team02Delta: d02, Team13Delta: d13, Detail: detail}
	g.CurrentTurn = -1

	// 목표점수 도달 → 종료 (동점이면 한 핸드 더)
	if (g.Score02 >= g.Target || g.Score13 >= g.Target) && g.Score02 != g.Score13 {
		g.Phase = TCPhaseGameOver
		if g.Score02 > g.Score13 {
			g.WinnerTeam = "02"
		} else {
			g.WinnerTeam = "13"
		}
		return
	}
	g.Phase = TCPhaseHandEnd
	for s := 0; s < TCSeats; s++ {
		g.Ready[s] = false
	}
}

// SetReady hand_end 에서 다음 핸드 준비. 전원 준비되면 true.
func (g *TCGame) SetReady(seat int) (allReady bool, changed bool) {
	if g.Phase != TCPhaseHandEnd || g.Ready[seat] {
		return false, false
	}
	g.Ready[seat] = true
	for s := 0; s < TCSeats; s++ {
		if !g.Ready[s] {
			return false, true
		}
	}
	return true, true
}
