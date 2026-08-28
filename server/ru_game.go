package server

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ==================== 루미큐브 순수 규칙 ====================
//
// 타일 구성·배분·세트 판정(조커 포함)·등록 30점·숫자조합 유효성·차례 커밋·
// 최종 정산만 다룬다. 클라이언트·타이머를 모르며, 허브(ru_hub.go)가 차례
// 마감(90초)을 걸고 이벤트 큐(DrainEvents)를 방송한다.
//
// ==================== 세트 판정 (이 게임의 심장) ====================
//
//	그룹 — 색이 서로 다른 같은 숫자 3~4개          예) 빨강7 · 파랑7 · 검정7
//	연속 — 색이 같고 숫자가 이어지는 3개 이상       예) 빨강4 · 빨강5 · 빨강6
//	       13 다음은 없다 (12·13·1 은 연속이 아니다)
//
// ==================== 커밋 세트의 타일 순서는 의미가 있다 ====================
//
// 조커는 세트 안에서 어떤 타일도 대신한다. 그러면 "조커가 무엇을 대신하는가"
// 를 서버가 정해야 하는데(점수 계산·standsFor 표시에 필요하다), 규칙서의
// 표현 그대로 **"조커는 놓인 자리의 숫자로 친다"** 를 따른다.
// 즉 **받은 순서를 그대로 읽는다. 서버는 세트를 재정렬하지 않는다.**
//
//	[빨강5, 빨강6, 조커] → 5·6·7  (18점)
//	[조커, 빨강5, 빨강6] → 4·5·6  (15점)
//
// 같은 타일 집합이라도 조커의 자리가 다르면 점수가 다르고, 그래서 등록 30점
// 판정이 갈린다. 프론트는 연속을 오름차순으로, 조커를 실제 대신하는 자리에
// 끼워서 보낸다 (그룹은 색 순서). 서버가 임의로 재정렬하면 프론트가 화면에
// 보여 준 해석과 어긋나므로 절대 하지 않는다.
//
// 해석 순서:
//
//	① 받은 순서 그대로 읽어 연속이 되면 그게 답이다 (조커 포함).
//	② 아니면 그룹 (그룹은 순서가 뜻을 바꾸지 않는다 — 전부 같은 숫자다).
//	③ 조커가 없는 세트에 한해, 숫자 순서가 뒤섞여 와도 연속으로 인정한다.
//	   조커가 없으면 어느 순서로 읽든 해석이 하나뿐이라 애매함이 없다.
//	   조커가 낀 세트는 자리를 그대로 읽는 것이 유일한 해석이다.
//
// ==================== 차례 커밋 모델 ====================
//
// 서버 상태는 언제나 "차례 시작 상태"다. Commit 은 테이블 전체 배치를 통째로
// 받아 **전부 사본 위에서** 검사하고, 다섯 검사를 모두 통과한 뒤에야 실제
// 상태를 갈아끼운다. 그래서 부분 적용이 물리적으로 불가능하다 —
// "거부 시 차례 시작 상태로 원복"은 곧 "거부 시 아무것도 하지 않음"이다.
//
//	① 참조 무결성 — 모르는 타일·중복 타일·남의 타일은 없는가
//	② 테이블 보존 — 테이블에 있던 타일이 받침대로 돌아오지 않는가
//	   (이 한 줄이 "회수한 조커는 그 차례에 반드시 써야 한다"를 그대로 담는다.
//	    조커를 다른 세트로 옮기는 것은 테이블 안의 이동이라 허용되고,
//	    받침대로 빼돌리는 것만 막힌다)
//	③ 최소 1개 — 내 타일이 최소 1개는 새로 나갔는가
//	④ 전체 유효 — 테이블의 모든 세트가 유효한가
//	⑤ 등록 규칙 — 등록 전이면 기존 세트를 하나도 건드리지 않았고(숫자조합
//	   금지) 새 세트의 합이 30점 이상인가

// NewRUGame 대기 상태의 새 게임
func NewRUGame(id string) *RUGame {
	return &RUGame{
		ID:          id,
		Players:     []*RUPlayer{},
		Phase:       RUPhaseWaiting,
		Pool:        []RUTile{},
		Sets:        [][]RUTile{},
		CurrentSeat: -1,
	}
}

// AddPlayer 대기실 입장. 좌석 번호를 돌려준다.
func (g *RUGame) AddPlayer(name string) (int, error) {
	if g.Phase != RUPhaseWaiting {
		return -1, errors.New("이미 시작된 게임입니다")
	}
	if len(g.Players) >= RUMaxPlayers {
		return -1, fmt.Errorf("자리가 없습니다 (최대 %d명)", RUMaxPlayers)
	}
	seat := len(g.Players)
	g.Players = append(g.Players, &RUPlayer{
		Seat: seat,
		Name: name,
		Rack: []RUTile{},
	})
	return seat, nil
}

// RemovePlayer 대기실 퇴장. 좌석을 앞으로 당겨 빈틈을 없앤다.
func (g *RUGame) RemovePlayer(seat int) {
	if g.Phase != RUPhaseWaiting || seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Players = append(g.Players[:seat], g.Players[seat+1:]...)
	for i, p := range g.Players {
		p.Seat = i
	}
}

// CanStart 시작 가능 여부 (호스트 명시 시작 — 2인부터)
func (g *RUGame) CanStart() bool {
	return g.Phase == RUPhaseWaiting && len(g.Players) >= RUMinPlayers
}

// ==================== 타일 ====================

// ruBuildPool 타일 106개 = 4색 × 1~13 × 2벌(104) + 조커 2개.
// ID 는 1부터 붙는다 (와이어에서 타일을 가리키는 유일한 키).
func ruBuildPool() []RUTile {
	tiles := make([]RUTile, 0, len(ruColors)*RUMaxNum*RUCopies+RUJokers)
	id := 1
	for copyNo := 0; copyNo < RUCopies; copyNo++ {
		for _, c := range ruColors {
			for n := 1; n <= RUMaxNum; n++ {
				tiles = append(tiles, RUTile{ID: id, Color: c, Num: n})
				id++
			}
		}
	}
	for j := 0; j < RUJokers; j++ {
		tiles = append(tiles, RUTile{ID: id, Joker: true})
		id++
	}
	return tiles
}

// ruSortRack 받침대를 보기 좋게 정렬한다 (조커 뒤, 색·숫자 순).
// 프로토콜은 인덱스가 아니라 타일 ID 를 쓰므로 정렬은 언제든 안전하다.
func ruSortRack(rack []RUTile) {
	order := map[RUColor]int{}
	for i, c := range ruColors {
		order[c] = i
	}
	sort.SliceStable(rack, func(i, j int) bool {
		a, b := rack[i], rack[j]
		if a.Joker != b.Joker {
			return !a.Joker
		}
		if a.Color != b.Color {
			return order[a.Color] < order[b.Color]
		}
		if a.Num != b.Num {
			return a.Num < b.Num
		}
		return a.ID < b.ID
	})
}

// ruTileDesc 타일 한 개의 한글 표기 (오류 문구용 — 본인에게만 간다)
func ruTileDesc(t RUTile) string {
	if t.Joker {
		return "조커"
	}
	return fmt.Sprintf("%s%d", ruColorName(t.Color), t.Num)
}

// ruSetDesc 세트 한 벌의 한글 표기
func ruSetDesc(set []RUTile) string {
	parts := make([]string, 0, len(set))
	for _, t := range set {
		parts = append(parts, ruTileDesc(t))
	}
	return strings.Join(parts, "·")
}

// Start 게임 시작 — 타일 106개를 섞어 각자 14개씩 나눠 갖는다.
// 첫 차례는 무작위 좌석이다.
func (g *RUGame) Start(rng *rand.Rand) error {
	if !g.CanStart() {
		return fmt.Errorf("%d명 이상 모여야 시작할 수 있습니다", RUMinPlayers)
	}
	n := len(g.Players)
	g.Ready = true
	g.StartedAt = time.Now()

	pool := ruBuildPool()
	rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })

	for _, p := range g.Players {
		p.Rack = append([]RUTile{}, pool[:RUStartRack]...)
		ruSortRack(p.Rack)
		pool = pool[RUStartRack:]
		p.Melded = false
		p.Score = 0
	}
	g.Pool = append([]RUTile{}, pool...)
	g.Sets = [][]RUTile{}
	g.LastAction = nil
	g.Result = nil
	g.Turns = 0
	g.PassStreak = 0

	g.CurrentSeat = rng.Intn(n)
	g.Phase = RUPhaseTurn
	g.StateSeq++
	g.emit("started", g.CurrentSeat, fmt.Sprintf(
		"게임 시작 — 각자 타일 %d개로 시작합니다. 타일더미 %d개. %s님부터 시작합니다",
		RUStartRack, len(g.Pool), g.Players[g.CurrentSeat].Name))
	return nil
}

// ==================== 이벤트 큐 ====================

func (g *RUGame) emit(kind string, seat int, msg string) {
	g.events = append(g.events, RUGameEvent{Kind: kind, Seat: seat, Message: msg})
}

// DrainEvents 쌓인 이벤트를 꺼내고 비운다 (허브가 방송)
func (g *RUGame) DrainEvents() []RUGameEvent {
	evs := g.events
	g.events = nil
	return evs
}

func (g *RUGame) setLastAction(seat int, msg string) {
	name := ""
	if seat >= 0 && seat < len(g.Players) {
		name = g.Players[seat].Name
	}
	g.LastAction = &RULastAction{Seat: seat, Name: name, Message: msg}
}

// ==================== 세트 판정 ====================

// ruResolve 세트 하나를 해석한다.
// 돌려주는 값은 (종류, 타일별로 실제로 세는 숫자, 유효 여부)다.
// 조커의 자리에는 그 조커가 대신하는 숫자가 들어간다.
//
// **받은 순서를 그대로 읽는다 — 서버는 세트를 재정렬하지 않는다.**
// 해석 순서는 파일 상단 주석 참조.
func ruResolve(tiles []RUTile) (RUSetKind, []int, bool) {
	if len(tiles) < 3 {
		return RUSetNone, nil, false
	}
	jokers := 0
	for _, t := range tiles {
		if t.Joker {
			jokers++
		}
	}
	if jokers == len(tiles) {
		// 조커만으로는 무엇을 대신하는지 정할 수 없다
		return RUSetNone, nil, false
	}

	// ① 놓인 자리 그대로 읽어 연속이 되는가 (조커는 놓인 자리의 숫자)
	if vals, ok := ruRunInPlace(tiles); ok {
		return RUSetRun, vals, true
	}
	// ② 그룹 — 순서가 뜻을 바꾸지 않는다 (전부 같은 숫자)
	if vals, ok := ruGroupVals(tiles); ok {
		return RUSetGroup, vals, true
	}
	// ③ 조커가 없으면 숫자 순서가 뒤섞여 와도 연속으로 인정한다
	//    (해석이 하나뿐이라 애매함이 없다)
	if jokers == 0 {
		if vals, ok := ruRunAnyOrder(tiles); ok {
			return RUSetRun, vals, true
		}
	}
	return RUSetNone, nil, false
}

// ruSumInts 정수 합
func ruSumInts(vals []int) int {
	sum := 0
	for _, v := range vals {
		sum += v
	}
	return sum
}

// ruGroupVals 그룹 해석 — 색이 서로 다른 같은 숫자 3~4개.
// 조커는 아직 안 쓴 색을 대신하므로 개수 상한(색 4종)만 지키면 된다.
func ruGroupVals(tiles []RUTile) ([]int, bool) {
	n := len(tiles)
	if n < 3 || n > len(ruColors) {
		return nil, false
	}
	num := 0
	seen := map[RUColor]bool{}
	for _, t := range tiles {
		if t.Joker {
			continue
		}
		if t.Num < 1 || t.Num > RUMaxNum {
			return nil, false
		}
		if num == 0 {
			num = t.Num
		} else if t.Num != num {
			return nil, false
		}
		if seen[t.Color] {
			return nil, false // 같은 색이 두 번 — 그룹이 아니다
		}
		seen[t.Color] = true
	}
	if num == 0 {
		return nil, false
	}
	vals := make([]int, n)
	for i := range vals {
		vals[i] = num
	}
	return vals, true
}

// ruRunShape 연속 해석의 공통 전처리 — 색이 하나인지, 숫자가 겹치지 않는지
// 확인하고 시작 숫자 s 의 가능 범위 [lo, hi] 를 돌려준다.
// 창은 [s, s+n-1] 이며 1~13 을 벗어날 수 없다 (13 다음은 없다).
func ruRunShape(tiles []RUTile) (lo, hi int, seen map[int]bool, ok bool) {
	n := len(tiles)
	if n < 3 || n > RUMaxNum {
		return 0, 0, nil, false
	}
	color := RUColor("")
	minNum, maxNum := RUMaxNum+1, 0
	seen = map[int]bool{}
	found := false
	for _, t := range tiles {
		if t.Joker {
			continue
		}
		if t.Num < 1 || t.Num > RUMaxNum {
			return 0, 0, nil, false
		}
		if !found {
			color, found = t.Color, true
		} else if t.Color != color {
			return 0, 0, nil, false // 색이 섞였다 — 연속이 아니다
		}
		if seen[t.Num] {
			return 0, 0, nil, false // 같은 숫자가 두 번 — 이어지지 않는다
		}
		seen[t.Num] = true
		if t.Num < minNum {
			minNum = t.Num
		}
		if t.Num > maxNum {
			maxNum = t.Num
		}
	}
	if !found {
		return 0, 0, nil, false
	}
	lo = maxNum - n + 1
	if lo < 1 {
		lo = 1
	}
	hi = minNum
	if hi > RUMaxNum-n+1 {
		hi = RUMaxNum - n + 1
	}
	if lo > hi {
		return 0, 0, nil, false
	}
	return lo, hi, seen, true
}

// ruRunInPlace "놓인 자리 그대로" 연속인지 — i번째 타일이 s+i 를 뜻한다고
// 봤을 때 딱 맞는 s 가 있는지. 실제 타일이 하나라도 있으면 그런 s 는 많아야
// 하나라 해석이 흔들리지 않는다.
func ruRunInPlace(tiles []RUTile) ([]int, bool) {
	lo, hi, _, ok := ruRunShape(tiles)
	if !ok {
		return nil, false
	}
	for s := lo; s <= hi; s++ {
		fit := true
		for i, t := range tiles {
			if !t.Joker && t.Num != s+i {
				fit = false
				break
			}
		}
		if !fit {
			continue
		}
		vals := make([]int, len(tiles))
		for i := range vals {
			vals[i] = s + i
		}
		return vals, true
	}
	return nil, false
}

// ruRunAnyOrder 조커가 하나도 없는 세트에 한한 관대한 연속 판정 —
// 숫자가 뒤섞여 와도 빈틈 없이 이어지면 연속으로 인정한다.
// 조커가 없으면 각 타일의 값이 자기 숫자로 고정돼 해석이 하나뿐이라
// "순서가 뜻을 정한다"는 계약과 충돌하지 않는다.
func ruRunAnyOrder(tiles []RUTile) ([]int, bool) {
	lo, hi, seen, ok := ruRunShape(tiles)
	if !ok {
		return nil, false
	}
	// 조커가 없으므로 창은 실제 숫자로 꽉 차야 한다 (lo == hi 이고 빈틈 없음)
	if lo != hi || len(seen) != len(tiles) {
		return nil, false
	}
	vals := make([]int, len(tiles))
	for i, t := range tiles {
		if t.Joker { // 방어선 — 호출부가 조커 없는 세트만 넘긴다
			return nil, false
		}
		vals[i] = t.Num
	}
	return vals, true
}

// ruValidateSet 세트 판정 (스펙이 요구하는 순수 함수).
// 그룹이면 RUSetGroup, 연속이면 RUSetRun, 아니면 (RUSetNone, false).
func ruValidateSet(tiles []RUTile) (RUSetKind, bool) {
	kind, _, ok := ruResolve(tiles)
	return kind, ok
}

// ruSetScore 세트의 점수 — 조커는 그 자리에서 대신하는 숫자로 센다.
// 유효하지 않은 세트는 0점이다.
func ruSetScore(tiles []RUTile) int {
	_, vals, ok := ruResolve(tiles)
	if !ok {
		return 0
	}
	return ruSumInts(vals)
}

// ruBoardValid 테이블 전체가 유효한지
func ruBoardValid(sets [][]RUTile) bool {
	for _, set := range sets {
		if _, ok := ruValidateSet(set); !ok {
			return false
		}
	}
	return true
}

// ruTableView 테이블을 와이어용으로 복사하면서 조커에 standsFor 를 채운다.
// 유효하지 않은 세트는 (정상 흐름에서는 생기지 않지만) standsFor 없이 나간다.
func ruTableView(sets [][]RUTile) [][]RUTile {
	out := make([][]RUTile, 0, len(sets))
	for _, set := range sets {
		_, vals, ok := ruResolve(set)
		copied := make([]RUTile, 0, len(set))
		for i, t := range set {
			tile := t
			tile.StandsFor = nil
			if ok && tile.Joker {
				v := vals[i]
				tile.StandsFor = &v
			}
			copied = append(copied, tile)
		}
		out = append(out, copied)
	}
	return out
}

// ==================== 차례 커밋 ====================

// ruSetKey 세트를 타일 ID 다중집합으로 식별하는 키 (순서 무시).
// "원래 그대로인 세트"를 가려내는 데 쓴다.
func ruSetKey(set []RUTile) string {
	ids := make([]int, 0, len(set))
	for _, t := range set {
		ids = append(ids, t.ID)
	}
	sort.Ints(ids)
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.Itoa(id))
	}
	return strings.Join(parts, ",")
}

// ruSplitUntouched 확정 배치에서 "원래 그대로인 세트"를 걷어내고 새로 놓인
// 세트만 돌려준다. 원래 세트가 하나라도 헐렸으면 ok=false — 그게 숫자조합이다.
func ruSplitUntouched(orig, built [][]RUTile) ([][]RUTile, bool) {
	need := map[string]int{}
	for _, set := range orig {
		need[ruSetKey(set)]++
	}
	fresh := [][]RUTile{}
	for _, set := range built {
		key := ruSetKey(set)
		if need[key] > 0 {
			need[key]--
			continue
		}
		fresh = append(fresh, set)
	}
	for _, n := range need {
		if n > 0 {
			return nil, false
		}
	}
	return fresh, true
}

// Commit 차례 확정 — 테이블 전체 배치를 통째로 받는다.
//
// 검사는 전부 사본 위에서 하고, 다섯 검사를 모두 통과한 뒤에야 실제 상태를
// 갈아끼운다. 하나라도 어긋나면 **아무것도 바꾸지 않고** 오류만 돌려준다
// (= 차례 시작 상태 그대로. 부분 적용은 물리적으로 불가능하다).
func (g *RUGame) Commit(seat int, sets [][]int) error {
	if g.Phase != RUPhaseTurn {
		return errors.New("지금은 타일을 낼 수 없습니다")
	}
	if seat != g.CurrentSeat {
		return errors.New("당신의 차례가 아닙니다")
	}
	if seat < 0 || seat >= len(g.Players) {
		return errors.New("잘못된 좌석입니다")
	}
	p := g.Players[seat]

	// ---- ① 참조 무결성 ----
	tableByID := map[int]RUTile{}
	for _, set := range g.Sets {
		for _, t := range set {
			tableByID[t.ID] = t
		}
	}
	rackByID := map[int]RUTile{}
	for _, t := range p.Rack {
		rackByID[t.ID] = t
	}

	used := map[int]bool{}
	built := make([][]RUTile, 0, len(sets))
	fromRack := 0
	for _, ids := range sets {
		if len(ids) == 0 {
			return errors.New("빈 세트는 놓을 수 없습니다")
		}
		set := make([]RUTile, 0, len(ids))
		for _, id := range ids {
			if used[id] {
				return errors.New("같은 타일을 두 번 놓을 수 없습니다")
			}
			used[id] = true
			if t, ok := tableByID[id]; ok {
				set = append(set, t)
				continue
			}
			if t, ok := rackByID[id]; ok {
				set = append(set, t)
				fromRack++
				continue
			}
			return errors.New("내 받침대에도 테이블에도 없는 타일입니다")
		}
		built = append(built, set)
	}

	// ---- ② 테이블 보존 (조커를 받침대로 빼돌릴 수 없다) ----
	for id := range tableByID {
		if !used[id] {
			return errors.New("테이블의 타일을 받침대로 가져올 수 없습니다")
		}
	}

	// ---- ③ 내 타일이 최소 1개는 새로 나갔는가 ----
	if fromRack == 0 {
		return errors.New("내 타일을 최소 1개는 내려놓아야 합니다")
	}

	// ---- ④ 테이블 전체가 유효한가 ----
	for i, set := range built {
		if _, ok := ruValidateSet(set); !ok {
			return fmt.Errorf("%d번째 세트가 유효하지 않습니다 (%s)", i+1, ruSetDesc(set))
		}
	}

	// ---- ⑤ 등록 규칙 ----
	meldScore := 0
	melding := !p.Melded
	if melding {
		fresh, ok := ruSplitUntouched(g.Sets, built)
		if !ok {
			return errors.New("등록하는 차례에는 숫자조합을 할 수 없습니다 (테이블 위 세트를 그대로 두세요)")
		}
		for _, set := range fresh {
			meldScore += ruSetScore(set)
		}
		if meldScore < RUInitialMeld {
			return fmt.Errorf("등록은 %d점 이상이어야 합니다 (지금 %d점)", RUInitialMeld, meldScore)
		}
	}

	// ==================== 여기부터 반영 ====================
	manipulated := !melding && !ruOnlyExtended(g.Sets, built)

	g.Sets = built
	rack := make([]RUTile, 0, len(p.Rack))
	for _, t := range p.Rack {
		if !used[t.ID] {
			rack = append(rack, t)
		}
	}
	p.Rack = rack
	g.PassStreak = 0

	if melding {
		p.Melded = true
		g.emit("meld", seat, fmt.Sprintf("%s님이 %d점으로 등록했습니다", p.Name, meldScore))
	}

	msg := fmt.Sprintf("%s님이 타일 %d개를 내려놓았습니다 (남은 타일 %d개)",
		p.Name, fromRack, len(p.Rack))
	if manipulated {
		msg = fmt.Sprintf("%s님이 숫자조합으로 타일 %d개를 내려놓았습니다 (남은 타일 %d개)",
			p.Name, fromRack, len(p.Rack))
	}
	kind := "commit"
	if manipulated {
		kind = "manipulate"
	}
	g.emit(kind, seat, msg)
	g.setLastAction(seat, msg)

	if len(p.Rack) == 0 {
		g.Turns++
		g.emit("rummikub", seat, fmt.Sprintf("%s님이 받침대를 비우고 \"루미큐브!\"를 외쳤습니다", p.Name))
		g.settle([]int{seat}, "받침대 비우기")
		return nil
	}
	g.advanceTurn()
	return nil
}

// ruOnlyExtended 원래 세트가 하나도 헐리지 않았는지 — 각 원래 세트의 타일이
// 확정 배치의 **한 세트 안에 모여** 있으면 "얹기"일 뿐 숫자조합이 아니다.
// 문구를 고르는 데만 쓰고 규칙 판정에는 쓰지 않는다.
func ruOnlyExtended(orig, built [][]RUTile) bool {
	owner := map[int]int{} // 타일 ID → 확정 배치의 세트 번호
	for i, set := range built {
		for _, t := range set {
			owner[t.ID] = i
		}
	}
	for _, set := range orig {
		home, first := -1, true
		for _, t := range set {
			idx, ok := owner[t.ID]
			if !ok {
				return false
			}
			if first {
				home, first = idx, false
			} else if idx != home {
				return false // 원래 한 세트였던 타일이 흩어졌다 = 숫자조합
			}
		}
	}
	return true
}

// ==================== 가져오기 / 넘기기 ====================

// Draw 못 내겠으니 타일더미에서 1개 가져오고 차례를 끝낸다.
// 타일더미가 비었으면 가져올 것이 없으므로 차례를 넘긴다 —
// 전원이 연속으로 넘기면 남은 타일 점수로 승부를 가린다.
func (g *RUGame) Draw(seat int) error {
	if g.Phase != RUPhaseTurn {
		return errors.New("지금은 타일을 가져올 수 없습니다")
	}
	if seat != g.CurrentSeat {
		return errors.New("당신의 차례가 아닙니다")
	}
	if seat < 0 || seat >= len(g.Players) {
		return errors.New("잘못된 좌석입니다")
	}
	p := g.Players[seat]

	if len(g.Pool) > 0 {
		// 가져온 타일의 정체는 어떤 이벤트·로그에도 담지 않는다
		tile := g.Pool[0]
		g.Pool = append([]RUTile{}, g.Pool[1:]...)
		p.Rack = append(p.Rack, tile)
		ruSortRack(p.Rack)
		g.PassStreak = 0
		msg := fmt.Sprintf("%s님이 타일더미에서 1개를 가져왔습니다 (타일더미 %d개 남음)",
			p.Name, len(g.Pool))
		g.emit("draw", seat, msg)
		g.setLastAction(seat, msg)
	} else {
		g.PassStreak++
		msg := fmt.Sprintf("%s님이 낼 것이 없어 차례를 넘겼습니다 (타일더미가 비었습니다)", p.Name)
		g.emit("pass", seat, msg)
		g.setLastAction(seat, msg)
	}
	g.advanceTurn()
	return nil
}

// advanceTurn 차례를 넘긴다. 타일더미가 빈 뒤 전원이 연속으로 넘기면
// 아무도 못 내는 것이므로 남은 타일 점수로 승부를 가린다.
func (g *RUGame) advanceTurn() {
	n := len(g.Players)
	if n == 0 {
		return
	}
	g.Turns++

	if len(g.Pool) == 0 && g.PassStreak >= n {
		g.emit("pool_empty", -1,
			"타일더미가 떨어지고 아무도 내지 못해 남은 타일 점수로 승부를 가립니다")
		g.settle(g.lowestSeats(), "타일더미 소진")
		return
	}
	if g.Turns >= RUMaxTurns {
		g.settle(g.lowestSeats(), "차례 상한")
		return
	}

	g.CurrentSeat = (g.CurrentSeat + 1) % n
	g.Phase = RUPhaseTurn
	g.StateSeq++
}

// ==================== AFK 자동 진행 (허브 타이머) ====================

// ForceTurn 차례 마감 — 타일 1개를 가져가고 차례를 끝낸다
// (재배치가 오래 걸리는 게임이라 자동 내려놓기는 하지 않는다).
func (g *RUGame) ForceTurn(rng *rand.Rand) {
	if g.Phase != RUPhaseTurn {
		return
	}
	seat := g.CurrentSeat
	if seat < 0 || seat >= len(g.Players) {
		return
	}
	g.Draw(seat)
}

// ==================== 정산 ====================

// ruTileScore 타일 한 개의 점수 — 조커는 50점
func ruTileScore(t RUTile) int {
	if t.Joker {
		return RUJokerScore
	}
	return t.Num
}

// ruRackScore 받침대에 남은 타일의 점수 합 (조커 50점)
func ruRackScore(rack []RUTile) int {
	sum := 0
	for _, t := range rack {
		sum += ruTileScore(t)
	}
	return sum
}

// ruRackJokers 받침대에 남은 조커 수
func ruRackJokers(rack []RUTile) int {
	n := 0
	for _, t := range rack {
		if t.Joker {
			n++
		}
	}
	return n
}

// penaltyOf 좌석의 벌점.
// 등록도 못 하고 끝난 사람은 타일 점수와 무관하게 100점이다.
func (g *RUGame) penaltyOf(p *RUPlayer) int {
	if !p.Melded {
		return RUNoMeldPenalty
	}
	return ruRackScore(p.Rack)
}

// lowestSeats 벌점이 가장 낮은 좌석들 (동점이면 여럿)
func (g *RUGame) lowestSeats() []int {
	best, seats := 0, []int{}
	for i, p := range g.Players {
		pen := g.penaltyOf(p)
		if i == 0 || pen < best {
			best, seats = pen, []int{p.Seat}
			continue
		}
		if pen == best {
			seats = append(seats, p.Seat)
		}
	}
	return seats
}

// settle 최종 정산.
//
//	패자 — 남은 타일의 숫자 합이 마이너스 점수 (조커는 50점).
//	       등록도 못 했으면 타일과 무관하게 −100점.
//	승자 — 패자들의 벌점 합계가 그대로 플러스 점수.
//	       공동 승이면 각자 같은 합계를 받는다.
func (g *RUGame) settle(winners []int, reason string) {
	n := len(g.Players)
	isWinner := make([]bool, n)
	winnerSeats := []int{}
	winnerNames := []string{}
	for _, s := range winners {
		if s >= 0 && s < n {
			isWinner[s] = true
		}
	}
	for _, p := range g.Players {
		if isWinner[p.Seat] {
			winnerSeats = append(winnerSeats, p.Seat)
			winnerNames = append(winnerNames, p.Name)
		}
	}

	pot := 0
	pens := make([]int, n)
	for i, p := range g.Players {
		pens[i] = g.penaltyOf(p)
		if !isWinner[i] {
			pot += pens[i]
		}
	}

	rows := make([]RUResultRow, 0, n)
	for i, p := range g.Players {
		var detail string
		switch {
		case isWinner[i] && len(p.Rack) == 0:
			p.Score = pot
			detail = fmt.Sprintf("받침대를 비웠습니다 (루미큐브!) · 남은 사람의 타일 %d점 획득", pot)
		case isWinner[i]:
			p.Score = pot
			detail = fmt.Sprintf("남은 타일 %d개 · %d점으로 최소 · 남은 사람의 타일 %d점 획득",
				len(p.Rack), pens[i], pot)
		case !p.Melded:
			p.Score = -pens[i]
			detail = fmt.Sprintf("등록하지 못했습니다 · 벌점 %d점 (남은 타일 %d개)",
				RUNoMeldPenalty, len(p.Rack))
		default:
			p.Score = -pens[i]
			detail = fmt.Sprintf("남은 타일 %d개 · %d점", len(p.Rack), pens[i])
			if j := ruRackJokers(p.Rack); j > 0 {
				detail += fmt.Sprintf(" (조커 %d개 포함 · 개당 %d점)", j, RUJokerScore)
			}
		}
		rows = append(rows, RUResultRow{Seat: p.Seat, Score: p.Score, Detail: detail})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Score != rows[j].Score {
			return rows[i].Score > rows[j].Score
		}
		return rows[i].Seat < rows[j].Seat
	})

	msg := fmt.Sprintf("%s님이 %d점으로 승리했습니다", strings.Join(winnerNames, "·"), pot)
	if len(winnerNames) > 1 {
		msg = fmt.Sprintf("%s님이 %d점으로 공동 승리했습니다", strings.Join(winnerNames, "·"), pot)
	}
	if len(winnerNames) == 0 { // 방어선 — 좌석 없는 판
		msg = "게임이 종료됐습니다"
	}
	if reason != "" {
		msg = fmt.Sprintf("%s (%s)", msg, reason)
	}

	g.Result = &RUResult{
		WinnerSeats: winnerSeats,
		WinnerNames: winnerNames,
		Rows:        rows,
		Message:     msg,
	}
	g.Phase = RUPhaseGameOver
	g.StateSeq++
	g.emit("settle", -1, msg)
}
