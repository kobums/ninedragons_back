package server

import (
	"fmt"
	"math/rand"
	"sort"
	"time"
)

// ==================== 루미큐브 연습봇 ====================
//
// 스냅샷(ru_game_state)만 보고 반응한다. 봇도 사람과 같은 조건 — 자기
// yourRack 만 알고 남의 받침대·타일더미 내용은 모른다.
//
// 판단은 네 갈래다 (스펙이 정한 순서 그대로).
//
//	① 등록 전 — 자기 타일만으로 30점 이상이 되는 세트 묶음을 찾는다.
//	   테이블은 손도 대지 않는다 (등록 차례에는 숫자조합 금지).
//	② 등록 후 — 테이블에 **얹을 수 있는 타일**을 먼저 찾고(연속 양끝 확장,
//	   그룹에 색 추가), 없으면 자기 타일만으로 새 세트를 만든다.
//	③ 그래도 한 장도 못 내면 **제한된 숫자조합 두 수**만 시도한다.
//	④ 그것도 없으면 타일더미에서 1개 가져온다.
//
// **본격적인 숫자조합 탐색은 하지 않는다** — 너무 세면 사람이 못 이긴다.
// ③은 일반 탐색이 아니라 아래 두 가지 정형(定型)만 본다. 그마저도 ②로
// 한 장도 못 낼 때만 꺼내므로, 평소에는 얹기와 새 세트만 쓰는 순한 봇이다.
//
//	㉮ 연속 나누기 — 긴 연속(6개 이상)을 두 동강 내고 그 이음매에 같은
//	   숫자의 2벌째를 끼운다. 예) 테이블 주황1~7 · 내 주황4(2벌째)
//	     → [주황1·2·3·4'] + [주황4·5·6·7]
//	㉯ 그룹에서 빌리기 — 4장짜리 그룹에서 한 장을 빼(3장이 남아 여전히
//	   유효하다) 내 타일들과 새 연속을 만든다. 예) 테이블 [빨강8·파랑8·
//	   검정8·주황8] 에서 파랑8 을 빌려 내 파랑6·파랑7 과 [파랑6·7·8]
//
// 왜 ③이 필요한가: 타일이 색·숫자마다 2벌씩이라 후반에 남는 것은 거의
// 전부 "테이블에 이미 있는 타일의 2벌째"다. 얹기만으로는 영영 못 내려놓아
// 판이 늘 타일더미 소진으로 끝난다 (측정치 93%). ㉮㉯ 두 수만 열어 줘도
// 받침대를 비우고 끝나는 판이 절반을 넘는다.
//
// 봇이 만든 배치는 보내기 전에 ruBotDryRun 으로 **서버와 똑같은 코드**
// (RUGame.Commit)에 통과시켜 본다. 그래서 봇의 판단과 서버의 판정이
// 어긋날 수 없고, 거부돼 차례가 멈추는 일이 생기지 않는다.

// 봇이 "생각하는" 시간 (테스트에서 짧게 낮춘다)
var (
	ruBotDelay    = 800 * time.Millisecond
	ruBotJitterMs = 700
)

// 봇 탐색 손잡이 (밸런스 조정 — 봇 품질 측정 테스트가 이 값을 읽는다)
var (
	// ruBotSearchNodes 세트 묶음 탐색의 노드 상한 (시간을 묶는 안전핀)
	ruBotSearchNodes = 20000
	// ruBotMaxCandidates 한 번에 들고 갈 세트 후보 수 상한 (값이 높은 순)
	ruBotMaxCandidates = 140
)

// ruBotPlayerView 봇이 스냅샷에서 꺼내 쓰는 좌석 정보
type ruBotPlayerView struct {
	Seat      int  `json:"seat"`
	RackCount int  `json:"rackCount"`
	Melded    bool `json:"melded"`
}

// ruBotState 봇이 상태 스냅샷에서 꺼내 쓰는 최소 정보
type ruBotState struct {
	YourSeat    int               `json:"yourSeat"`
	Phase       RUPhase           `json:"phase"`
	CurrentSeat int               `json:"currentSeat"`
	PoolLeft    int               `json:"poolLeft"`
	Sets        [][]RUTile        `json:"sets"`
	YourRack    []RUTile          `json:"yourRack"`
	YourMelded  bool              `json:"yourMelded"`
	Players     []ruBotPlayerView `json:"players"`
}

// ruBrain 스냅샷 기반 판단 (봇 대체 좌석도 같은 두뇌를 쓴다)
type ruBrain struct {
	rng *rand.Rand
	// lastKey 마지막으로 행동한 차례 식별키 (중복 행동 방지)
	lastKey string
}

func newRUBrain() *ruBrain {
	return &ruBrain{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

// decide 공용 러너 계약 — ru_game_state 에만 반응한다
func (b *ruBrain) decide(msg RUMessage) *RUMessage {
	if msg.Type != RUMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[ruBotState](msg.Payload)
	if !ok {
		return nil
	}
	return b.decideState(state)
}

// think 사람처럼 잠깐 뜸을 들인다 (테스트에서는 var 를 낮춰 즉시 진행한다)
func (b *ruBrain) think() {
	d := ruBotDelay
	if ruBotJitterMs > 0 {
		d += time.Duration(b.rng.Intn(ruBotJitterMs)) * time.Millisecond
	}
	if d > 0 {
		time.Sleep(d)
	}
}

// stateKey 같은 차례를 식별하는 키 — 판이 조금이라도 바뀌면 달라진다
func (b *ruBrain) stateKey(s ruBotState) string {
	rackSum, tableSum, tableTiles := 0, 0, 0
	for _, t := range s.YourRack {
		rackSum += t.ID
	}
	for _, set := range s.Sets {
		for _, t := range set {
			tableSum += t.ID * (len(set) + 1)
			tableTiles++
		}
	}
	return fmt.Sprintf("%s|%d|%d|%d|%d|%d|%d|%d|%t",
		s.Phase, s.CurrentSeat, s.PoolLeft, len(s.YourRack), rackSum,
		len(s.Sets), tableTiles, tableSum, s.YourMelded)
}

func (b *ruBrain) decideState(s ruBotState) *RUMessage {
	me := s.YourSeat
	if me < 0 || me >= len(s.Players) || s.CurrentSeat != me {
		return nil
	}
	if s.Phase != RUPhaseTurn {
		return nil
	}
	key := b.stateKey(s)
	if b.lastKey == key {
		return nil
	}
	b.lastKey = key

	b.think()
	if ids, ok := b.plan(s); ok {
		return &RUMessage{Type: RUMsgCommit, Payload: RUCommitPayload{Sets: ids}}
	}
	return &RUMessage{Type: RUMsgDraw}
}

// ==================== 배치 계획 ====================

// plan 이번 차례에 확정할 테이블 전체 배치를 만든다.
// 낼 것이 없으면 ok=false — 타일더미에서 1개 가져온다.
func (b *ruBrain) plan(s ruBotState) ([][]int, bool) {
	table := ruCloneSets(s.Sets)
	rack := append([]RUTile{}, s.YourRack...)

	var work [][]RUTile
	if !s.YourMelded {
		work = b.planMeld(table, rack)
	} else {
		work = b.planLayDown(table, rack)
	}
	if work == nil {
		return nil, false
	}

	ids := ruSetIDs(work)
	// 보내기 전에 서버와 똑같은 코드로 확인한다 (봇·서버 판단 불일치 방지)
	if !ruBotDryRun(table, rack, s.YourMelded, ids) {
		return nil, false
	}
	return ids, true
}

// planMeld 등록 — 자기 타일만으로 30점 이상. 테이블은 그대로 둔다.
// 문턱(30점)은 gate 로 걸고, 그 안에서 **가장 많은 타일**이 나가도록 고른다
// (받침대를 빨리 비우는 것이 이기는 길이다).
func (b *ruBrain) planMeld(table [][]RUTile, rack []RUTile) [][]RUTile {
	cands := ruBotCandidates(rack)
	picked, _ := ruBotPickSets(cands, ruBotLayValue, ruSetScore, RUInitialMeld)
	if len(picked) == 0 {
		return nil
	}
	work := append([][]RUTile{}, table...)
	work = append(work, picked...)
	return work
}

// planLayDown 등록 후 — 테이블에 얹기와 새 세트 만들기를 섞어 최대한
// 내려놓는다. 한 장도 못 내면 그때만 제한된 숫자조합 두 수를 본다.
//
// 얹기와 새 세트는 두 순서를 모두 돌려 보고 타일이 더 많이 나가는 쪽을
// 택한다. 순서가 실제로 결과를 바꾸기 때문이다 — 받침대에 빨강7·파랑7·검정7
// 이 있고 테이블에 빨강4·5·6 이 있으면, 얹기를 먼저 하면 빨강7 하나만 나가고
// 7 그룹이 깨진다. 새 세트를 먼저 만들면 셋 다 나간다. 반대 경우도 있다.
func (b *ruBrain) planLayDown(table [][]RUTile, rack []RUTile) [][]RUTile {
	if work, placed := ruBotBestPass(table, rack); placed > 0 {
		return work
	}
	return ruBotManipulate(table, rack)
}

// ruBotBestPass 얹기·새 세트만으로(숫자조합 없이) 가장 많이 내려놓는 배치
func ruBotBestPass(table [][]RUTile, rack []RUTile) ([][]RUTile, int) {
	var best [][]RUTile
	bestPlaced := 0
	for _, extendFirst := range []bool{true, false} {
		work, left := ruBotFinish(ruCloneSets(table), append([]RUTile{}, rack...), extendFirst)
		if placed := len(rack) - len(left); placed > bestPlaced {
			best, bestPlaced = work, placed
		}
	}
	return best, bestPlaced
}

// ruBotFinish 주어진 배치에서 시작해 얹기와 새 세트로 최대한 더 내려놓는다.
// extendFirst 가 참이면 얹기를 먼저, 거짓이면 새 세트를 먼저 시도한다.
// 돌려주는 값은 (완성된 배치, 아직 못 내려놓은 타일)이다.
func ruBotFinish(work [][]RUTile, left []RUTile, extendFirst bool) ([][]RUTile, []RUTile) {
	// extend 손에 든 타일 하나를 테이블 세트에 얹는다
	extend := func() bool {
		idx, si, cand, ok := ruBotExtendAny(work, left)
		if !ok {
			return false
		}
		work[si] = cand
		left = append(left[:idx:idx], left[idx+1:]...)
		return true
	}
	// fresh 손에 든 타일만으로 새 세트를 만든다 (가장 많이 나가는 묶음)
	fresh := func() bool {
		picked, _ := ruBotPickSets(ruBotCandidates(left), ruBotLayValue, ruBotSetSize, 1)
		if len(picked) == 0 {
			return false
		}
		for _, set := range picked {
			work = append(work, set)
			left = ruRemoveTiles(left, set)
		}
		return true
	}

	first, second := extend, fresh
	if !extendFirst {
		first, second = fresh, extend
	}
	for len(left) > 0 {
		if first() {
			continue
		}
		if second() {
			continue
		}
		break
	}
	return work, left
}

// ==================== 제한된 숫자조합 (정형 두 수) ====================

// ruBotManipulate 얹기·새 세트로 한 장도 못 낼 때만 부르는 마지막 수단.
// 일반 탐색이 아니라 파일 상단에 적은 두 가지 정형만 본다.
// 한 수를 놓은 뒤에는 평소의 얹기·새 세트로 이어서 최대한 더 내려놓는다.
func ruBotManipulate(table [][]RUTile, rack []RUTile) [][]RUTile {
	if work := ruBotSplitRun(table, rack); work != nil {
		return work
	}
	return ruBotBorrowFromGroup(table, rack)
}

// ruBotSplitRun ㉮ 연속 나누기 — 6개 이상인 연속을 두 동강 내고 그 이음매에
// 같은 숫자의 2벌째를 끼운다. 두 동강 모두 3개 이상이라 유효하다.
//
//	테이블 주황1·2·3·4·5·6·7 · 내 주황4(2벌째)
//	  → [주황1·2·3·4'] + [주황4·5·6·7]
func ruBotSplitRun(table [][]RUTile, rack []RUTile) [][]RUTile {
	for si, set := range table {
		kind, ok := ruValidateSet(set)
		if !ok || kind != RUSetRun || len(set) < 6 {
			continue
		}
		for p := 3; p <= len(set)-3; p++ {
			head := append([]RUTile{}, set[:p]...)
			tail := append([]RUTile{}, set[p:]...)
			if _, ok := ruValidateSet(head); !ok {
				continue
			}
			if _, ok := ruValidateSet(tail); !ok {
				continue
			}
			for ri, x := range rack {
				// 앞 동강 뒤에 붙이기 / 뒤 동강 앞에 붙이기
				variants := [][2][]RUTile{
					{append(append([]RUTile{}, head...), x), tail},
					{head, append([]RUTile{x}, tail...)},
				}
				for _, v := range variants {
					if _, ok := ruValidateSet(v[0]); !ok {
						continue
					}
					if _, ok := ruValidateSet(v[1]); !ok {
						continue
					}
					work := ruCloneSets(table)
					work[si] = v[0]
					work = append(work, v[1])
					left := append([]RUTile{}, rack[:ri]...)
					left = append(left, rack[ri+1:]...)
					work, _ = ruBotFinish(work, left, true)
					return work
				}
			}
		}
	}
	return nil
}

// ruBotBorrowFromGroup ㉯ 그룹에서 빌리기 — 4장짜리 그룹에서 한 장을 빼
// (3장이 남아 여전히 유효하다) 내 타일들과 새 세트를 만든다.
// 빌린 타일은 반드시 테이블 어딘가에 다시 놓여야 하고(테이블 보존),
// 내 타일도 최소 1개는 나가야 하므로 둘 다 확인한 뒤에만 채택한다.
func ruBotBorrowFromGroup(table [][]RUTile, rack []RUTile) [][]RUTile {
	for si, set := range table {
		kind, ok := ruValidateSet(set)
		if !ok || kind != RUSetGroup || len(set) != 4 {
			continue
		}
		for bi, borrowed := range set {
			if borrowed.Joker { // 조커를 빌리는 수는 보지 않는다 (너무 세다)
				continue
			}
			if !ruBotBorrowWorthwhile(rack, borrowed) {
				continue
			}
			rest := append([]RUTile{}, set[:bi]...)
			rest = append(rest, set[bi+1:]...)
			if _, ok := ruValidateSet(rest); !ok {
				continue
			}
			work := ruCloneSets(table)
			work[si] = rest
			// 빌린 타일을 손패에 얹어 평소대로 내려놓는다
			left := append([]RUTile{}, rack...)
			left = append(left, borrowed)
			work, leftover := ruBotFinish(work, left, false)

			stillHeld := map[int]bool{}
			for _, t := range leftover {
				stillHeld[t.ID] = true
			}
			if stillHeld[borrowed.ID] {
				continue // 빌린 타일을 못 놓으면 테이블이 비므로 무효
			}
			if len(rack)-(len(leftover)) < 1 {
				continue // 내 타일이 하나도 안 나갔다
			}
			return work
		}
	}
	return nil
}

// ruBotBorrowWorthwhile 그 타일을 빌릴 값어치가 있는지 싸게 미리 거른다 —
// 같은 색의 이웃 숫자를 내가 2개 이상 들고 있을 때만 본격적으로 시도한다.
func ruBotBorrowWorthwhile(rack []RUTile, borrowed RUTile) bool {
	near := 0
	for _, t := range rack {
		if t.Joker || t.Color != borrowed.Color {
			continue
		}
		if d := t.Num - borrowed.Num; d >= -2 && d <= 2 && d != 0 {
			near++
		}
	}
	return near >= 2
}

// ==================== 세트 후보 생성 ====================

// ruBotLayValue 후보의 값 — **나가는 타일 수가 최우선**, 같으면 받침대에서
// 덜어내는 벌점이 큰 쪽(조커가 50점이라 자연히 먼저 나간다).
// 받침대를 비우는 것이 이기는 길이므로 점수보다 개수가 앞선다.
func ruBotLayValue(set []RUTile) int { return len(set)*100 + ruRackScore(set) }

// ruBotSetSize 후보의 타일 수 (문턱 판정용)
func ruBotSetSize(set []RUTile) int { return len(set) }

// ruBotCandidates 받침대만으로 만들 수 있는 세트 후보들.
// 같은 색·숫자가 2벌까지 있으므로 벌(layer)마다 따로 후보를 낸다.
func ruBotCandidates(rack []RUTile) [][]RUTile {
	jokers := []RUTile{}
	byNumColor := map[int]map[RUColor][]RUTile{}
	byColorNum := map[RUColor]map[int][]RUTile{}
	for _, t := range rack {
		if t.Joker {
			jokers = append(jokers, t)
			continue
		}
		if t.Num < 1 || t.Num > RUMaxNum {
			continue
		}
		if byNumColor[t.Num] == nil {
			byNumColor[t.Num] = map[RUColor][]RUTile{}
		}
		byNumColor[t.Num][t.Color] = append(byNumColor[t.Num][t.Color], t)
		if byColorNum[t.Color] == nil {
			byColorNum[t.Color] = map[int][]RUTile{}
		}
		byColorNum[t.Color][t.Num] = append(byColorNum[t.Color][t.Num], t)
	}

	out := [][]RUTile{}
	seenKey := map[string]bool{}
	add := func(set []RUTile) {
		if _, ok := ruValidateSet(set); !ok {
			return
		}
		key := ruSetKey(set)
		if seenKey[key] {
			return
		}
		seenKey[key] = true
		out = append(out, set)
	}

	for layer := 0; layer < RUCopies; layer++ {
		// ---- 그룹: 색이 서로 다른 같은 숫자 3~4개 ----
		for num := 1; num <= RUMaxNum; num++ {
			colors := []RUTile{}
			for _, c := range ruColors {
				if ts := byNumColor[num][c]; len(ts) > layer {
					colors = append(colors, ts[layer])
				}
			}
			for mask := 1; mask < 1<<len(colors); mask++ {
				pick := []RUTile{}
				for i := range colors {
					if mask&(1<<i) != 0 {
						pick = append(pick, colors[i])
					}
				}
				for j := 0; j <= len(jokers); j++ {
					if n := len(pick) + j; n < 3 || n > len(ruColors) {
						continue
					}
					set := append(append([]RUTile{}, pick...), jokers[:j]...)
					add(set)
				}
			}
		}

		// ---- 연속: 색이 같고 숫자가 이어지는 3개 이상 (오름차순으로 담는다) ----
		for _, c := range ruColors {
			nums := byColorNum[c]
			if len(nums) == 0 {
				continue
			}
			for s := 1; s <= RUMaxNum-2; s++ {
				for l := 3; s+l-1 <= RUMaxNum; l++ {
					set := make([]RUTile, 0, l)
					need, real := 0, 0
					for v := s; v <= s+l-1; v++ {
						if ts := nums[v]; len(ts) > layer {
							set = append(set, ts[layer])
							real++
							continue
						}
						need++
						if need > len(jokers) {
							break
						}
						set = append(set, jokers[need-1])
					}
					if len(set) != l || real == 0 {
						continue
					}
					add(set)
				}
			}
		}
	}
	return out
}

// ==================== 세트 묶음 고르기 ====================

// ruBotPickSets 서로 겹치지 않는 후보 묶음 중 objective 합이 가장 큰 것을
// 고른다. gate 합이 need 미만인 묶음은 아예 후보에서 뺀다 — 등록 30점 문턱이
// 여기로 들어온다(gate=세트 점수, need=30). 문턱과 목표를 분리한 덕에
// "30점은 넘기되 타일은 최대한 많이" 같은 조합이 가능하다.
//
// 탐색 노드 상한으로 시간을 묶으므로 최적해가 아닐 수 있다 —
// 봇이 너무 세지 않게 하는 데도 도움이 된다.
func ruBotPickSets(cands [][]RUTile, objective, gate func([]RUTile) int, need int) ([][]RUTile, int) {
	type scored struct {
		set  []RUTile
		val  int
		gate int
	}
	list := make([]scored, 0, len(cands))
	for _, c := range cands {
		list = append(list, scored{set: c, val: objective(c), gate: gate(c)})
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].val != list[j].val {
			return list[i].val > list[j].val
		}
		return list[i].gate > list[j].gate
	})
	if len(list) > ruBotMaxCandidates {
		list = list[:ruBotMaxCandidates]
	}

	used := map[int]bool{}
	cur := [][]RUTile{}
	var best [][]RUTile
	bestVal := -1
	nodes := 0

	var dfs func(idx, total, gateTotal int)
	dfs = func(idx, total, gateTotal int) {
		if nodes > ruBotSearchNodes {
			return
		}
		nodes++
		if gateTotal >= need && total > bestVal {
			bestVal = total
			best = append([][]RUTile{}, cur...)
		}
		for i := idx; i < len(list); i++ {
			free := true
			for _, t := range list[i].set {
				if used[t.ID] {
					free = false
					break
				}
			}
			if !free {
				continue
			}
			for _, t := range list[i].set {
				used[t.ID] = true
			}
			cur = append(cur, list[i].set)
			dfs(i+1, total+list[i].val, gateTotal+list[i].gate)
			cur = cur[:len(cur)-1]
			for _, t := range list[i].set {
				used[t.ID] = false
			}
			if nodes > ruBotSearchNodes {
				return
			}
		}
	}
	dfs(0, 0, 0)

	if bestVal < 0 {
		return nil, 0
	}
	return best, bestVal
}

// ==================== 테이블에 얹기 ====================

// ruBotExtendAny 받침대 타일 하나를 테이블 세트에 얹을 수 있으면 그 결과를
// 돌려준다. 앞·뒤에만 붙이고, **놓인 자리 그대로 읽어 유효한 배열만** 채택한다
// (연속은 오름차순, 그룹은 그대로). 뒤섞인 순서로도 판정은 통과하지만
// 그러면 프론트가 "주황7·6·5·4·1·2·3" 같은 세트를 그리게 되고, 조커가 끼면
// standsFor 가 화면의 자리와 어긋난다.
// 돌려주는 값은 (받침대 인덱스, 세트 번호, 새 세트, 성공 여부)다.
func ruBotExtendAny(sets [][]RUTile, rack []RUTile) (int, int, []RUTile, bool) {
	for ri, t := range rack {
		for si, set := range sets {
			for _, at := range []int{0, len(set)} {
				cand := make([]RUTile, 0, len(set)+1)
				cand = append(cand, set[:at]...)
				cand = append(cand, t)
				cand = append(cand, set[at:]...)
				if ruBotWellOrdered(cand) {
					return ri, si, cand, true
				}
			}
		}
	}
	return 0, 0, nil, false
}

// ruBotWellOrdered 놓인 자리 그대로 읽어 유효한 세트인지 —
// 오름차순 연속이거나 그룹이면 참
func ruBotWellOrdered(set []RUTile) bool {
	if len(set) < 3 {
		return false
	}
	if _, ok := ruRunInPlace(set); ok {
		return true
	}
	_, ok := ruGroupVals(set)
	return ok
}

// ==================== 보조 ====================

// ruCloneSets 세트 배치를 깊은 복사한다 (standsFor 는 떼어낸다 —
// 규칙 판정은 조커의 표시값을 보지 않는다)
func ruCloneSets(sets [][]RUTile) [][]RUTile {
	out := make([][]RUTile, 0, len(sets))
	for _, set := range sets {
		copied := make([]RUTile, 0, len(set))
		for _, t := range set {
			tile := t
			tile.StandsFor = nil
			copied = append(copied, tile)
		}
		out = append(out, copied)
	}
	return out
}

// ruSetIDs 세트 배치를 타일 ID 목록으로 (ru_commit 페이로드 형태)
func ruSetIDs(sets [][]RUTile) [][]int {
	out := make([][]int, 0, len(sets))
	for _, set := range sets {
		ids := make([]int, 0, len(set))
		for _, t := range set {
			ids = append(ids, t.ID)
		}
		out = append(out, ids)
	}
	return out
}

// ruRemoveTiles 받침대에서 지정한 타일들을 뺀다
func ruRemoveTiles(rack []RUTile, gone []RUTile) []RUTile {
	drop := map[int]bool{}
	for _, t := range gone {
		drop[t.ID] = true
	}
	out := make([]RUTile, 0, len(rack))
	for _, t := range rack {
		if !drop[t.ID] {
			out = append(out, t)
		}
	}
	return out
}

// ruBotDryRun 봇이 만든 배치를 **서버와 똑같은 코드**로 미리 검사한다.
// 이 관문 덕에 봇의 확정이 거부돼 차례가 멈추는 일이 없다.
func ruBotDryRun(table [][]RUTile, rack []RUTile, melded bool, sets [][]int) bool {
	g := NewRUGame("ru-bot-dryrun")
	g.Players = []*RUPlayer{{
		Seat:   0,
		Name:   "봇",
		Rack:   append([]RUTile{}, rack...),
		Melded: melded,
	}}
	g.Sets = ruCloneSets(table)
	g.Phase = RUPhaseTurn
	g.CurrentSeat = 0
	g.Ready = true
	return g.Commit(0, sets) == nil
}

// ==================== 봇 소환 ====================

// spawnRUBot 대기실의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *RUHub) spawnRUBot(room *ruRoom, name string) bool {
	bot := &RUClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runRUBot(bot)
	return true
}

// takeoverRUBot 유예 만료 좌석을 이어받는 연습봇 — 이름·좌석을 유지해
// 진행 중인 차례가 그대로 이어진다
func (h *RUHub) takeoverRUBot(room *ruRoom, seat int, name string) *RUClient {
	bot := &RUClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	bot.GameID = room.Game.ID
	bot.Seat = seat
	h.runRUBot(bot)
	return bot
}

// runRUBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *RUHub) runRUBot(bot *RUClient) {
	brain := newRUBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m RUMessage) { h.gameMessage <- RUGameMessage{Client: bot, Message: m} },
		func(m RUMessage) bool { return m.Type == RUMsgGameOver || m.Type == RUMsgSessionExpired })
}

// ruRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록용)
func ruRoomHasBot(room *ruRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}
