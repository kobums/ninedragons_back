package server

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

// ==================== 루미큐브 순수 규칙 테스트 ====================
//
// 이 게임의 전부는 (1) 세트 판정(조커 포함), (2) 등록 30점, (3) 숫자조합
// 유효성과 **거부 시 원복**, (4) 점수 정산(조커 50 · 미등록 100)이다.
// 넷 다 표 기반으로 촘촘히 고정한다.

// ruT 숫자 타일 하나 (테스트 가독성용)
func ruT(id int, c RUColor, n int) RUTile { return RUTile{ID: id, Color: c, Num: n} }

// ruJk 조커 하나
func ruJk(id int) RUTile { return RUTile{ID: id, Joker: true} }

// ruIDs 타일 목록 → ID 목록 (ru_commit 페이로드 형태)
func ruIDs(tiles ...RUTile) []int {
	out := make([]int, 0, len(tiles))
	for _, t := range tiles {
		out = append(out, t.ID)
	}
	return out
}

// ruFixture 결정적 시드로 시작된 n인 게임 (규칙 테스트는 이후 판을 직접 세운다)
func ruFixture(t *testing.T, n int) *RUGame {
	t.Helper()
	g := NewRUGame("ru-test")
	for i := 0; i < n; i++ {
		if _, err := g.AddPlayer(fmt.Sprintf("P%d", i)); err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
	}
	if err := g.Start(rand.New(rand.NewSource(20260829))); err != nil {
		t.Fatalf("Start: %v", err)
	}
	g.DrainEvents()
	return g
}

// ruSetBoard 판을 손으로 세운다 (테이블·받침대·차례 좌석 고정).
// racks 는 좌석 순서대로, 남는 좌석은 빈 받침대가 된다.
func ruSetBoard(g *RUGame, seat int, table [][]RUTile, racks ...[]RUTile) {
	g.Sets = ruCloneSets(table)
	for i, p := range g.Players {
		if i < len(racks) {
			p.Rack = append([]RUTile{}, racks[i]...)
		} else {
			p.Rack = []RUTile{}
		}
		p.Melded = false
		p.Score = 0
	}
	g.CurrentSeat = seat
	g.Phase = RUPhaseTurn
	g.PassStreak = 0
	g.Turns = 0
	g.DrainEvents()
}

// ruSnapshot 판 전체를 문자열 하나로 굳힌다 — 거부된 확정이 판을 한 톨도
// 건드리지 않았음을 증명하는 근거. 세트 안의 타일 **순서까지** 담는다.
func ruSnapshot(g *RUGame) string {
	parts := []string{fmt.Sprintf("phase=%s seat=%d turns=%d pool=%d pass=%d",
		g.Phase, g.CurrentSeat, g.Turns, len(g.Pool), g.PassStreak)}
	sets := []string{}
	for _, set := range g.Sets {
		ids := []string{}
		for _, t := range set {
			ids = append(ids, fmt.Sprint(t.ID))
		}
		sets = append(sets, "["+strings.Join(ids, ",")+"]")
	}
	parts = append(parts, "table="+strings.Join(sets, ""))
	for _, p := range g.Players {
		ids := []int{}
		for _, t := range p.Rack {
			ids = append(ids, t.ID)
		}
		sort.Ints(ids)
		parts = append(parts, fmt.Sprintf("seat%d(melded=%t score=%d rack=%v)",
			p.Seat, p.Melded, p.Score, ids))
	}
	return strings.Join(parts, " ")
}

// ==================== 타일 구성 ====================

// TestRUPoolComposition 타일 106개 = 4색 × 1~13 × 2벌(104) + 조커 2개
func TestRUPoolComposition(t *testing.T) {
	pool := ruBuildPool()
	if len(pool) != 106 {
		t.Fatalf("타일 수 = %d, want 106", len(pool))
	}
	ids := map[int]bool{}
	count := map[string]int{}
	jokers := 0
	for _, tile := range pool {
		if ids[tile.ID] {
			t.Fatalf("타일 ID 중복: %d", tile.ID)
		}
		ids[tile.ID] = true
		if tile.Joker {
			jokers++
			if tile.Num != 0 {
				t.Fatalf("조커의 num = %d, want 0", tile.Num)
			}
			continue
		}
		if tile.Num < 1 || tile.Num > RUMaxNum {
			t.Fatalf("타일 숫자 = %d", tile.Num)
		}
		count[fmt.Sprintf("%s-%d", tile.Color, tile.Num)]++
	}
	if jokers != RUJokers {
		t.Fatalf("조커 수 = %d, want %d", jokers, RUJokers)
	}
	if len(count) != len(ruColors)*RUMaxNum {
		t.Fatalf("색·숫자 조합 = %d, want %d", len(count), len(ruColors)*RUMaxNum)
	}
	for key, n := range count {
		if n != RUCopies {
			t.Fatalf("%s 벌 수 = %d, want %d", key, n, RUCopies)
		}
	}
	// 와이어 색 값은 red blue black orange 로 고정 (화면 표기만 한국어)
	wantColors := []RUColor{"red", "blue", "black", "orange"}
	for i, c := range wantColors {
		if ruColors[i] != c {
			t.Fatalf("색 %d = %q, want %q", i, ruColors[i], c)
		}
		if !hasHangul(ruColorName(c)) {
			t.Fatalf("%s 한글 표기가 없다: %q", c, ruColorName(c))
		}
	}
}

// ==================== 세트 판정 (조커 포함) ====================

// TestRUValidateSetTable 그룹·연속 판정과 점수를 표로 못박는다.
//
// 조커 해석 규칙:
//
//	① 받은 순서 그대로 읽어 연속이 되면 그게 답이다 ("조커는 놓인 자리의 숫자")
//	② 아니면 그룹·연속 중 점수가 높은 해석
func TestRUValidateSetTable(t *testing.T) {
	cases := []struct {
		name  string
		tiles []RUTile
		kind  RUSetKind
		ok    bool
		score int
	}{
		// ---- 그룹 ----
		{"그룹 3장", []RUTile{ruT(1, RURed, 7), ruT(2, RUBlue, 7), ruT(3, RUBlack, 7)},
			RUSetGroup, true, 21},
		{"그룹 4장", []RUTile{ruT(1, RURed, 7), ruT(2, RUBlue, 7), ruT(3, RUBlack, 7), ruT(4, RUOrange, 7)},
			RUSetGroup, true, 28},
		{"그룹 5장은 색이 모자라 불가", []RUTile{ruT(1, RURed, 7), ruT(2, RUBlue, 7),
			ruT(3, RUBlack, 7), ruT(4, RUOrange, 7), ruT(5, RURed, 7)}, RUSetNone, false, 0},
		{"같은 색 두 번은 그룹이 아니다", []RUTile{ruT(1, RURed, 7), ruT(2, RURed, 7), ruT(3, RUBlue, 7)},
			RUSetNone, false, 0},
		{"숫자가 다르면 그룹이 아니다", []RUTile{ruT(1, RURed, 7), ruT(2, RUBlue, 8), ruT(3, RUBlack, 7)},
			RUSetNone, false, 0},
		{"그룹 최소 3장 — 2장은 세트가 아니다", []RUTile{ruT(1, RURed, 7), ruT(2, RUBlue, 7)},
			RUSetNone, false, 0},

		// ---- 연속 ----
		{"연속 3장", []RUTile{ruT(1, RURed, 3), ruT(2, RURed, 4), ruT(3, RURed, 5)},
			RUSetRun, true, 12},
		{"연속 13까지", []RUTile{ruT(1, RURed, 11), ruT(2, RURed, 12), ruT(3, RURed, 13)},
			RUSetRun, true, 36},
		{"13 다음은 없다 (12·13·1)", []RUTile{ruT(1, RURed, 12), ruT(2, RURed, 13), ruT(3, RURed, 1)},
			RUSetNone, false, 0},
		{"색이 섞이면 연속이 아니다", []RUTile{ruT(1, RURed, 3), ruT(2, RUBlue, 4), ruT(3, RURed, 5)},
			RUSetNone, false, 0},
		{"숫자가 겹치면 연속이 아니다", []RUTile{ruT(1, RURed, 3), ruT(2, RURed, 3), ruT(3, RURed, 4)},
			RUSetNone, false, 0},
		{"이어지지 않으면 연속이 아니다", []RUTile{ruT(1, RURed, 3), ruT(2, RURed, 5), ruT(3, RURed, 7)},
			RUSetNone, false, 0},
		{"연속 13장 (1~13)", []RUTile{
			ruT(1, RURed, 1), ruT(2, RURed, 2), ruT(3, RURed, 3), ruT(4, RURed, 4),
			ruT(5, RURed, 5), ruT(6, RURed, 6), ruT(7, RURed, 7), ruT(8, RURed, 8),
			ruT(9, RURed, 9), ruT(10, RURed, 10), ruT(11, RURed, 11), ruT(12, RURed, 12),
			ruT(13, RURed, 13)}, RUSetRun, true, 91},

		// ---- 조커가 낀 그룹 ----
		{"조커 낀 그룹 3장", []RUTile{ruT(1, RURed, 7), ruT(2, RUBlue, 7), ruJk(90)},
			RUSetGroup, true, 21},
		{"조커 낀 그룹 4장", []RUTile{ruT(1, RURed, 7), ruT(2, RUBlue, 7), ruT(3, RUBlack, 7), ruJk(90)},
			RUSetGroup, true, 28},
		{"조커가 낀 그룹도 색이 겹치면 안 된다",
			[]RUTile{ruT(1, RURed, 7), ruT(2, RURed, 7), ruJk(90)}, RUSetNone, false, 0},
		{"13 뒤에는 자리가 없어 그룹으로 읽힌다 (13·13·13)",
			[]RUTile{ruT(1, RURed, 13), ruJk(90), ruJk(91)}, RUSetGroup, true, 39},

		// ---- 조커가 낀 연속 (조커는 놓인 자리의 숫자) ----
		{"조커가 가운데 빈자리를 메운다 (3·4·5)",
			[]RUTile{ruT(1, RURed, 3), ruJk(90), ruT(2, RURed, 5)}, RUSetRun, true, 12},
		{"조커가 앞자리 (2·3·4)",
			[]RUTile{ruJk(90), ruT(1, RURed, 3), ruT(2, RURed, 4)}, RUSetRun, true, 9},
		{"조커가 뒷자리 (3·4·5)",
			[]RUTile{ruT(1, RURed, 3), ruT(2, RURed, 4), ruJk(90)}, RUSetRun, true, 12},
		{"조커 2개가 뒤 (5·6·7)",
			[]RUTile{ruT(1, RURed, 5), ruJk(90), ruJk(91)}, RUSetRun, true, 18},
		{"조커 2개가 앞 (3·4·5)",
			[]RUTile{ruJk(90), ruJk(91), ruT(1, RURed, 5)}, RUSetRun, true, 12},
		{"조커 2개가 양끝 (4·5·6)",
			[]RUTile{ruJk(90), ruT(1, RURed, 5), ruJk(91)}, RUSetRun, true, 15},
		{"1 앞에는 자리가 없다 — 조커는 뒤로 (1·2·3)",
			[]RUTile{ruT(1, RURed, 1), ruJk(90), ruJk(91)}, RUSetRun, true, 6},
		{"13 다음 자리는 없다 — 12·13 뒤의 조커는 놓을 수 없다",
			[]RUTile{ruT(1, RURed, 12), ruT(2, RURed, 13), ruJk(90)}, RUSetNone, false, 0},
		{"12·13 앞에 놓으면 11로 읽힌다",
			[]RUTile{ruJk(90), ruT(1, RURed, 12), ruT(2, RURed, 13)}, RUSetRun, true, 36},
		{"조커 낀 긴 연속 (7·8·9·10·11)", []RUTile{
			ruT(1, RURed, 7), ruJk(90), ruT(2, RURed, 9), ruT(3, RURed, 10), ruJk(91)},
			RUSetRun, true, 45},

		// ---- 조커 관련 경계 ----
		{"조커만으로는 세트가 아니다", []RUTile{ruJk(90), ruJk(91), ruJk(92)}, RUSetNone, false, 0},
		{"조커 1개 + 실제 1개는 2장이라 세트가 아니다",
			[]RUTile{ruT(1, RURed, 5), ruJk(90)}, RUSetNone, false, 0},
		{"조커를 넣어도 같은 숫자가 겹치면 연속이 아니다",
			[]RUTile{ruT(1, RURed, 5), ruT(2, RURed, 5), ruJk(90)}, RUSetNone, false, 0},
		{"조커가 있어도 간격이 너무 크면 못 메운다",
			[]RUTile{ruT(1, RURed, 3), ruJk(90), ruT(2, RURed, 7)}, RUSetNone, false, 0},

		// ---- 순서가 뒤섞여 온 관대한 해석 ----
		{"순서가 뒤섞여도 연속으로 인정 (3·4·5)",
			[]RUTile{ruT(1, RURed, 5), ruT(2, RURed, 3), ruT(3, RURed, 4)}, RUSetRun, true, 12},

		// ---- 빈 세트 ----
		{"빈 세트", nil, RUSetNone, false, 0},
	}

	for _, tc := range cases {
		kind, ok := ruValidateSet(tc.tiles)
		if ok != tc.ok || kind != tc.kind {
			t.Fatalf("%s: ruValidateSet = (%q, %t), want (%q, %t)",
				tc.name, kind, ok, tc.kind, tc.ok)
		}
		if got := ruSetScore(tc.tiles); got != tc.score {
			t.Fatalf("%s: ruSetScore = %d, want %d", tc.name, got, tc.score)
		}
	}
}

// TestRUJokerPositionMatters 커밋 세트의 **타일 순서는 의미가 있다**.
// 조커가 어느 숫자를 대신하는지는 놓인 자리로 결정되므로, 같은 타일 집합이라도
// 조커의 자리가 다르면 종류·점수가 달라지고 등록 30점 판정이 갈린다.
// 서버는 절대 세트를 재정렬하지 않는다.
func TestRUJokerPositionMatters(t *testing.T) {
	r5, r6 := ruT(1, RURed, 5), ruT(2, RURed, 6)
	r9, r10 := ruT(3, RURed, 9), ruT(4, RURed, 10)
	r1 := ruT(5, RURed, 1)
	r7 := ruT(6, RURed, 7)
	j := ruJk(90)
	j2 := ruJk(91)

	cases := []struct {
		name  string
		tiles []RUTile
		kind  RUSetKind
		ok    bool
		score int
	}{
		{"조커가 뒤 — 5·6·7", []RUTile{r5, r6, j}, RUSetRun, true, 18},
		{"조커가 앞 — 4·5·6", []RUTile{j, r5, r6}, RUSetRun, true, 15},
		{"조커가 뒤 — 9·10·11 (등록 문턱을 넘는다)", []RUTile{r9, r10, j}, RUSetRun, true, 30},
		{"조커가 앞 — 8·9·10 (등록 문턱에 못 미친다)", []RUTile{j, r9, r10}, RUSetRun, true, 27},
		{"1 뒤의 조커 2개 — 1·2·3", []RUTile{r1, j, j2}, RUSetRun, true, 6},
		{"1 앞에는 자리가 없어 그룹으로 읽힌다 — 1·1·1", []RUTile{j, j2, r1}, RUSetGroup, true, 3},
		{"조커가 빈자리 — 7·8·9", []RUTile{r7, j, r9}, RUSetRun, true, 24},
		{"같은 타일이라도 자리가 어긋나면 세트가 아니다", []RUTile{j, r7, r9}, RUSetNone, false, 0},
	}
	for _, tc := range cases {
		kind, ok := ruValidateSet(tc.tiles)
		if ok != tc.ok || kind != tc.kind {
			t.Fatalf("%s: ruValidateSet = (%q, %t), want (%q, %t)",
				tc.name, kind, ok, tc.kind, tc.ok)
		}
		if got := ruSetScore(tc.tiles); got != tc.score {
			t.Fatalf("%s: ruSetScore = %d, want %d", tc.name, got, tc.score)
		}
	}

	// ---- 등록 판정이 실제로 갈린다 ----
	// 빨강9·빨강10·조커 를 뒤에 놓으면 30점(등록 성공), 앞에 놓으면 27점(실패).
	for _, tc := range []struct {
		name string
		sets [][]int
		want bool
	}{
		{"조커를 뒤에 — 30점 등록 성공", [][]int{ruIDs(r9, r10, j)}, true},
		{"조커를 앞에 — 27점 등록 실패", [][]int{ruIDs(j, r9, r10)}, false},
	} {
		g := ruFixture(t, 2)
		ruSetBoard(g, 0, nil, []RUTile{r9, r10, j}, nil)
		before := ruSnapshot(g)
		err := g.Commit(0, tc.sets)
		if tc.want != (err == nil) {
			t.Fatalf("%s: err = %v", tc.name, err)
		}
		if err != nil {
			if !strings.Contains(err.Error(), "27") {
				t.Fatalf("%s: 오류에 실제 점수가 없다: %q", tc.name, err.Error())
			}
			if got := ruSnapshot(g); got != before {
				t.Fatalf("%s: 거부됐는데 판이 바뀌었다", tc.name)
			}
		}
	}

	// ---- 스냅샷은 받은 순서를 그대로 유지한다 ----
	g := ruFixture(t, 2)
	ruSetBoard(g, 0, nil, []RUTile{r9, r10, j}, nil)
	if err := g.Commit(0, [][]int{ruIDs(r9, j, r10)}); err == nil {
		t.Fatal("9·조커·10 은 9·10·10 이라 세트가 아닌데 통과했다")
	}
	if err := g.Commit(0, [][]int{ruIDs(r9, r10, j)}); err != nil {
		t.Fatalf("등록 실패: %v", err)
	}
	gotIDs := []int{}
	for _, tile := range g.Sets[0] {
		gotIDs = append(gotIDs, tile.ID)
	}
	wantIDs := ruIDs(r9, r10, j)
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Fatalf("세트 순서가 바뀌었다: %v, want %v", gotIDs, wantIDs)
		}
	}
	view := ruTableView(g.Sets)
	if last := view[0][2]; !last.Joker || last.StandsFor == nil || *last.StandsFor != 11 {
		t.Fatalf("조커 standsFor = %+v, want 11", last)
	}
}

// TestRUJokerStandsFor 테이블에 놓인 조커에는 대신하는 숫자(standsFor)가
// 채워지고, 받침대의 조커에는 그 키가 없다
func TestRUJokerStandsFor(t *testing.T) {
	cases := []struct {
		name string
		set  []RUTile
		want []int // 조커 자리의 standsFor (앞에서부터)
	}{
		{"가운데 조커", []RUTile{ruT(1, RURed, 3), ruJk(90), ruT(2, RURed, 5)}, []int{4}},
		{"앞 조커", []RUTile{ruJk(90), ruT(1, RURed, 3), ruT(2, RURed, 4)}, []int{2}},
		{"뒤 조커 2개", []RUTile{ruT(1, RURed, 5), ruJk(90), ruJk(91)}, []int{6, 7}},
		{"그룹의 조커", []RUTile{ruT(1, RURed, 7), ruT(2, RUBlue, 7), ruJk(90)}, []int{7}},
	}
	for _, tc := range cases {
		view := ruTableView([][]RUTile{tc.set})
		got := []int{}
		for _, tile := range view[0] {
			if !tile.Joker {
				if tile.StandsFor != nil {
					t.Fatalf("%s: 숫자 타일에 standsFor 가 붙었다: %+v", tc.name, tile)
				}
				continue
			}
			if tile.StandsFor == nil {
				t.Fatalf("%s: 테이블 조커에 standsFor 가 없다", tc.name)
			}
			got = append(got, *tile.StandsFor)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("%s: 조커 수 = %d, want %d", tc.name, len(got), len(tc.want))
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%s: standsFor = %v, want %v", tc.name, got, tc.want)
			}
		}
	}

	// 받침대의 조커에는 standsFor 키 자체가 없다
	raw, _ := json.Marshal(ruJk(90))
	if strings.Contains(string(raw), "standsFor") {
		t.Fatalf("받침대 조커에 standsFor 키 유출: %s", raw)
	}
	if !strings.Contains(string(raw), `"joker":true`) || !strings.Contains(string(raw), `"num":0`) {
		t.Fatalf("조커 와이어 형태 = %s, want {joker:true, num:0}", raw)
	}
}

// TestRUBoardValid 테이블 전체 유효성
func TestRUBoardValid(t *testing.T) {
	good := [][]RUTile{
		{ruT(1, RURed, 3), ruT(2, RURed, 4), ruT(3, RURed, 5)},
		{ruT(4, RURed, 9), ruT(5, RUBlue, 9), ruT(6, RUBlack, 9)},
	}
	if !ruBoardValid(good) {
		t.Fatal("유효한 테이블을 무효로 판정했다")
	}
	bad := append([][]RUTile{}, good...)
	bad = append(bad, []RUTile{ruT(7, RURed, 2), ruT(8, RUBlue, 5)})
	if ruBoardValid(bad) {
		t.Fatal("무효한 세트가 섞였는데 유효로 판정했다")
	}
	if !ruBoardValid(nil) {
		t.Fatal("빈 테이블은 유효해야 한다")
	}
}

// ==================== 등록 (첫 내려놓기 30점) ====================

// TestRUInitialMeldTable 등록 조건을 표로 못박는다.
// 등록은 **자기 타일만으로** 이루어진 세트들의 합이 30점 이상이어야 한다.
func TestRUInitialMeldTable(t *testing.T) {
	// 받침대 재고 (모든 사례가 이 안에서 고른다)
	r10, r11, r12 := ruT(101, RURed, 10), ruT(102, RURed, 11), ruT(103, RURed, 12)
	r1, r2, r3 := ruT(104, RURed, 1), ruT(105, RURed, 2), ruT(106, RURed, 3)
	b9, k9, o9 := ruT(107, RUBlue, 9), ruT(108, RUBlack, 9), ruT(109, RUOrange, 9)
	joker := ruJk(110)
	rack := []RUTile{r10, r11, r12, r1, r2, r3, b9, k9, o9, joker}

	cases := []struct {
		name  string
		sets  [][]int
		ok    bool
		score int
	}{
		{"딱 30점 — 빨강10·11·12 (33점)", [][]int{ruIDs(r10, r11, r12)}, true, 33},
		{"27점은 모자란다 — 파랑9·검정9·주황9", [][]int{ruIDs(b9, k9, o9)}, false, 27},
		{"6점은 한참 모자란다 — 빨강1·2·3", [][]int{ruIDs(r1, r2, r3)}, false, 6},
		{"두 세트를 합치면 33점", [][]int{ruIDs(b9, k9, o9), ruIDs(r1, r2, r3)}, true, 33},
		{"조커를 낀 연속도 그 자리 숫자로 센다 — 11·12·13 (36점)",
			[][]int{ruIDs(r11, r12, joker)}, true, 36},
		{"세트가 아니면 등록이 아니다", [][]int{ruIDs(r10, r1, b9)}, false, 0},
	}

	for _, tc := range cases {
		g := ruFixture(t, 2)
		ruSetBoard(g, 0, nil, rack, nil)
		before := ruSnapshot(g)

		err := g.Commit(0, tc.sets)
		if tc.ok {
			if err != nil {
				t.Fatalf("%s: 등록이 거부됐다: %v", tc.name, err)
			}
			if !g.Players[0].Melded {
				t.Fatalf("%s: 등록 표시가 안 됐다", tc.name)
			}
			total := 0
			for _, set := range g.Sets {
				total += ruSetScore(set)
			}
			if total != tc.score {
				t.Fatalf("%s: 등록 점수 = %d, want %d", tc.name, total, tc.score)
			}
			continue
		}
		if err == nil {
			t.Fatalf("%s: 등록이 통과했다 (30점 미만이어야 한다)", tc.name)
		}
		if !hasHangul(err.Error()) {
			t.Fatalf("%s: 오류 문구가 한글이 아니다: %q", tc.name, err.Error())
		}
		if got := ruSnapshot(g); got != before {
			t.Fatalf("%s: 거부됐는데 판이 바뀌었다\n전: %s\n후: %s", tc.name, before, got)
		}
	}
}

// TestRUMeldTurnForbidsManipulation 등록하는 차례에는 숫자조합을 할 수 없다.
// 테이블 위 타일과 섞을 수도, 기존 세트를 헐 수도 없다.
func TestRUMeldTurnForbidsManipulation(t *testing.T) {
	// 테이블: 빨강 3·4·5 (누군가 이미 내려놓았다)
	t3, t4, t5 := ruT(1, RURed, 3), ruT(2, RURed, 4), ruT(3, RURed, 5)
	table := [][]RUTile{{t3, t4, t5}}
	// 내 받침대: 빨강 6 · 빨강 10·11·12
	m6 := ruT(10, RURed, 6)
	m10, m11, m12 := ruT(11, RURed, 10), ruT(12, RURed, 11), ruT(13, RURed, 12)
	rack := []RUTile{m6, m10, m11, m12}

	// ---- 기존 연속에 얹어 등록하려는 시도 → 거부 ----
	g := ruFixture(t, 2)
	ruSetBoard(g, 0, table, rack, nil)
	before := ruSnapshot(g)
	err := g.Commit(0, [][]int{ruIDs(t3, t4, t5, m6), ruIDs(m10, m11, m12)})
	if err == nil {
		t.Fatal("등록 차례에 기존 세트를 건드렸는데 통과했다")
	}
	if !strings.Contains(err.Error(), "숫자조합") {
		t.Fatalf("오류 문구 = %q (숫자조합 금지를 알려야 한다)", err.Error())
	}
	if got := ruSnapshot(g); got != before {
		t.Fatalf("거부됐는데 판이 바뀌었다\n전: %s\n후: %s", before, got)
	}

	// ---- 테이블을 그대로 두고 내 타일만으로 등록 → 통과 ----
	if err := g.Commit(0, [][]int{ruIDs(t3, t4, t5), ruIDs(m10, m11, m12)}); err != nil {
		t.Fatalf("정상 등록이 거부됐다: %v", err)
	}
	if !g.Players[0].Melded {
		t.Fatal("등록 표시가 안 됐다")
	}
	if len(g.Sets) != 2 {
		t.Fatalf("테이블 세트 수 = %d, want 2", len(g.Sets))
	}
	if len(g.Players[0].Rack) != 1 || g.Players[0].Rack[0].ID != m6.ID {
		t.Fatalf("받침대 = %+v, want 빨강6 하나", g.Players[0].Rack)
	}

	// ---- 등록을 마친 다음 차례에는 숫자조합이 가능하다 ----
	g.CurrentSeat = 0
	g.Phase = RUPhaseTurn
	if err := g.Commit(0, [][]int{ruIDs(t3, t4, t5, m6), ruIDs(m10, m11, m12)}); err != nil {
		t.Fatalf("등록 후 숫자조합이 거부됐다: %v", err)
	}
	if len(g.Players[0].Rack) != 0 {
		t.Fatalf("받침대 = %+v, want 빈 받침대", g.Players[0].Rack)
	}
}

// ==================== 숫자조합 + 거부 시 원복 ====================

// TestRUCommitRejectRollback 이 게임에서 가장 중요한 회귀 장치.
// 확정 시 검사에 하나라도 걸리면 **차례 시작 상태로 통째로 되돌린다** —
// 부분 적용은 절대 없다. 잘못된 확정을 줄줄이 던지고 매번 판이 한 톨도
// 바뀌지 않았음을 스냅샷으로 못박는다.
func TestRUCommitRejectRollback(t *testing.T) {
	// 테이블: 빨강 3·4·5 · 파랑9/검정9/주황9
	t3, t4, t5 := ruT(1, RURed, 3), ruT(2, RURed, 4), ruT(3, RURed, 5)
	g9b, g9k, g9o := ruT(4, RUBlue, 9), ruT(5, RUBlack, 9), ruT(6, RUOrange, 9)
	table := [][]RUTile{{t3, t4, t5}, {g9b, g9k, g9o}}

	// seat0 받침대 (등록을 마친 상태로 세운다)
	m6 := ruT(20, RURed, 6)
	m7 := ruT(21, RURed, 7)
	m9 := ruT(22, RURed, 9)
	mJ := ruJk(23)
	rack0 := []RUTile{m6, m7, m9, mJ}
	// seat1 받침대 (남의 타일)
	other := ruT(30, RUBlue, 1)
	rack1 := []RUTile{other}

	newGame := func() *RUGame {
		g := ruFixture(t, 2)
		ruSetBoard(g, 0, table, rack0, rack1)
		g.Players[0].Melded = true
		g.Players[1].Melded = true
		g.DrainEvents()
		return g
	}

	cases := []struct {
		name string
		sets [][]int
		want string // 오류 문구에 반드시 들어갈 조각
	}{
		{"모르는 타일 ID", [][]int{ruIDs(t3, t4, t5), ruIDs(g9b, g9k, g9o), {9999, 21, 22}},
			"없는 타일"},
		{"남의 받침대 타일", [][]int{ruIDs(t3, t4, t5), ruIDs(g9b, g9k, g9o), ruIDs(m6, m7, other)},
			"없는 타일"},
		{"같은 타일 두 번", [][]int{ruIDs(t3, t4, t5), ruIDs(g9b, g9k, g9o), {m6.ID, m6.ID, m7.ID}},
			"두 번"},
		{"빈 세트", [][]int{ruIDs(t3, t4, t5), ruIDs(g9b, g9k, g9o), {}}, "빈 세트"},
		{"테이블 타일을 받침대로 빼돌리기", [][]int{ruIDs(t3, t4, t5), ruIDs(m6, m7, m9)},
			"받침대로 가져올 수 없습니다"},
		{"내 타일이 하나도 안 나갔다", [][]int{ruIDs(t3, t4, t5), ruIDs(g9b, g9k, g9o)},
			"최소 1개"},
		{"유효하지 않은 세트를 만들었다",
			[][]int{ruIDs(t3, t4, t5), ruIDs(g9b, g9k, g9o), ruIDs(m6, m9, mJ)},
			"유효하지 않습니다"},
		{"숫자조합 결과가 무너졌다 (연속을 두 동강)",
			[][]int{ruIDs(t3, t4), ruIDs(t5, m6, m7), ruIDs(g9b, g9k, g9o)},
			"유효하지 않습니다"},
		{"세트를 통째로 지웠다", [][]int{ruIDs(m6, m7, m9)}, "받침대로 가져올 수 없습니다"},
		{"아무것도 안 보냈다", [][]int{}, "받침대로 가져올 수 없습니다"},
	}

	for _, tc := range cases {
		g := newGame()
		before := ruSnapshot(g)
		err := g.Commit(0, tc.sets)
		if err == nil {
			t.Fatalf("%s: 거부돼야 할 확정이 통과했다", tc.name)
		}
		if !hasHangul(err.Error()) {
			t.Fatalf("%s: 오류 문구가 한글이 아니다: %q", tc.name, err.Error())
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: 오류 문구 = %q, want %q 포함", tc.name, err.Error(), tc.want)
		}
		if got := ruSnapshot(g); got != before {
			t.Fatalf("%s: 거부됐는데 판이 바뀌었다 (부분 적용!)\n전: %s\n후: %s",
				tc.name, before, got)
		}
		if !ruBoardValid(g.Sets) {
			t.Fatalf("%s: 거부 뒤 테이블이 무효 상태다", tc.name)
		}
	}

	// ---- 차례가 아닌 좌석 / 진행 중이 아닌 단계도 판을 못 건드린다 ----
	g := newGame()
	before := ruSnapshot(g)
	if err := g.Commit(1, [][]int{ruIDs(t3, t4, t5), ruIDs(g9b, g9k, g9o), ruIDs(other)}); err == nil {
		t.Fatal("차례가 아닌 좌석의 확정이 통과했다")
	}
	if got := ruSnapshot(g); got != before {
		t.Fatalf("차례가 아닌 확정이 판을 바꿨다\n전: %s\n후: %s", before, got)
	}
	g.Phase = RUPhaseGameOver
	if err := g.Commit(0, [][]int{ruIDs(t3, t4, t5), ruIDs(g9b, g9k, g9o), ruIDs(m6, m7, m9)}); err == nil {
		t.Fatal("종료된 게임에서 확정이 통과했다")
	}

	// ---- 정상 숫자조합은 통과한다 (연속을 헐어 다시 짜고 내 타일을 얹는다) ----
	// 테이블 빨강3·4·5 + 내 빨강6·7 → 빨강3·4·5·6·7 (한 세트로 확장)
	ok := newGame()
	if err := ok.Commit(0, [][]int{
		ruIDs(t3, t4, t5, m6, m7), ruIDs(g9b, g9k, g9o)}); err != nil {
		t.Fatalf("정상 숫자조합이 거부됐다: %v", err)
	}
	if len(ok.Players[0].Rack) != 2 {
		t.Fatalf("받침대 = %+v, want 2개", ok.Players[0].Rack)
	}
	if ok.CurrentSeat != 1 || ok.Turns != 1 {
		t.Fatalf("차례가 넘어가지 않았다: seat=%d turns=%d", ok.CurrentSeat, ok.Turns)
	}
	if ok.LastAction == nil || !hasHangul(ok.LastAction.Message) {
		t.Fatalf("lastAction = %+v", ok.LastAction)
	}
}

// TestRUJokerRetrieval 조커 회수 — 테이블의 조커는 **테이블 안에서만** 옮길
// 수 있다. 받침대로 빼돌리는 것은 막힌다.
// 이 한 줄이 "회수한 조커는 그 차례에 반드시 써야 한다"를 그대로 담는다.
func TestRUJokerRetrieval(t *testing.T) {
	// 테이블: 빨강3 · 조커(=4) · 빨강5
	t3, tJ, t5 := ruT(1, RURed, 3), ruJk(2), ruT(3, RURed, 5)
	table := [][]RUTile{{t3, tJ, t5}}
	// 내 받침대: 조커가 대신하던 빨강4 · 파랑9·10·11
	m4 := ruT(10, RURed, 4)
	b9, b10, b11 := ruT(11, RUBlue, 9), ruT(12, RUBlue, 10), ruT(13, RUBlue, 11)
	rack := []RUTile{m4, b9, b10, b11}

	setup := func() *RUGame {
		g := ruFixture(t, 2)
		ruSetBoard(g, 0, table, rack, nil)
		g.Players[0].Melded = true
		g.DrainEvents()
		return g
	}

	// ---- 조커를 받침대로 가져가려는 시도 → 거부 ----
	g := setup()
	before := ruSnapshot(g)
	if err := g.Commit(0, [][]int{ruIDs(t3, m4, t5)}); err == nil {
		t.Fatal("조커를 받침대로 빼돌리는 확정이 통과했다")
	}
	if got := ruSnapshot(g); got != before {
		t.Fatalf("거부됐는데 판이 바뀌었다\n전: %s\n후: %s", before, got)
	}

	// ---- 실제 타일로 자리를 메우고 조커는 그 차례에 다시 쓴다 → 통과 ----
	g2 := setup()
	if err := g2.Commit(0, [][]int{ruIDs(t3, m4, t5), ruIDs(b9, b10, b11, tJ)}); err != nil {
		t.Fatalf("정상 조커 회수가 거부됐다: %v", err)
	}
	if len(g2.Sets) != 2 {
		t.Fatalf("테이블 세트 수 = %d, want 2", len(g2.Sets))
	}
	// 회수한 조커는 새 세트에서 파랑12 를 대신한다
	view := ruTableView(g2.Sets)
	last := view[1][len(view[1])-1]
	if !last.Joker || last.StandsFor == nil || *last.StandsFor != 12 {
		t.Fatalf("회수한 조커의 standsFor = %+v, want 12", last)
	}
	if len(g2.Players[0].Rack) != 0 {
		t.Fatalf("받침대 = %+v, want 비어 있음", g2.Players[0].Rack)
	}
	if g2.Phase != RUPhaseGameOver {
		t.Fatalf("받침대를 비웠는데 게임이 끝나지 않았다: phase=%s", g2.Phase)
	}
}

// ==================== 가져오기 · 넘기기 · 종료 ====================

// TestRUDrawAndPoolExhaustion 못 내면 타일더미에서 1개 가져오고 끝난다.
// 타일더미가 떨어지고 아무도 못 내면 남은 타일 점수가 가장 낮은 사람이 이긴다.
func TestRUDrawAndPoolExhaustion(t *testing.T) {
	g := ruFixture(t, 3)
	ruSetBoard(g, 0, nil, []RUTile{ruT(1, RURed, 5)}, []RUTile{ruT(2, RUBlue, 6)},
		[]RUTile{ruT(3, RUBlack, 7)})
	g.Pool = []RUTile{ruT(50, RUOrange, 12)}

	// 타일더미가 남아 있으면 1개를 가져오고 차례가 넘어간다
	if err := g.Draw(0); err != nil {
		t.Fatalf("가져오기 실패: %v", err)
	}
	if len(g.Players[0].Rack) != 2 || len(g.Pool) != 0 {
		t.Fatalf("받침대 %d개 · 타일더미 %d개", len(g.Players[0].Rack), len(g.Pool))
	}
	if g.CurrentSeat != 1 || g.Turns != 1 {
		t.Fatalf("차례 = seat%d turns=%d", g.CurrentSeat, g.Turns)
	}
	if g.PassStreak != 0 {
		t.Fatalf("가져왔는데 넘김이 쌓였다: %d", g.PassStreak)
	}
	// 가져온 타일의 정체는 이벤트에 담기지 않는다 (은닉)
	for _, ev := range g.DrainEvents() {
		if strings.Contains(ev.Message, "주황") || strings.Contains(ev.Message, "12") {
			t.Fatalf("이벤트에 가져온 타일이 새어 나갔다: %q", ev.Message)
		}
	}

	// 타일더미가 비면 넘기기다 — 전원이 연속으로 넘기면 정산
	if err := g.Draw(1); err != nil {
		t.Fatalf("넘기기 실패: %v", err)
	}
	if g.PassStreak != 1 || g.Phase != RUPhaseTurn {
		t.Fatalf("넘김 = %d phase=%s", g.PassStreak, g.Phase)
	}
	g.Draw(2)
	if g.Phase == RUPhaseGameOver {
		t.Fatal("아직 한 바퀴가 안 돌았는데 끝났다")
	}
	g.Draw(0)
	if g.Phase != RUPhaseGameOver || g.Result == nil {
		t.Fatalf("전원이 넘겼는데 정산되지 않았다: phase=%s", g.Phase)
	}
	if !strings.Contains(g.Result.Message, "타일더미 소진") {
		t.Fatalf("종료 사유 = %q", g.Result.Message)
	}

	// 차례가 아닌 좌석·종료 후 가져오기는 한글 오류
	if err := g.Draw(1); err == nil {
		t.Fatal("종료 후 가져오기가 통과했다")
	} else if !hasHangul(err.Error()) {
		t.Fatalf("오류 문구가 한글이 아니다: %q", err.Error())
	}
}

// ==================== 정산 ====================

// TestRUSettlementTable 점수 정산 표.
//
//	패자는 남은 타일의 숫자 합이 마이너스, 조커는 50점.
//	그 합계가 그대로 승자의 플러스 점수.
//	등록도 못 하고 끝난 사람은 타일과 무관하게 벌점 100점.
//
//	좌석  등록  남은 타일              벌점   점수
//	 0     O    (없음 — 받침대를 비움)   0    +165
//	 1     O    빨강5 · 파랑10 · 조커    65     −65
//	 2     X    빨강1                   100   −100
func TestRUSettlementTable(t *testing.T) {
	g := ruFixture(t, 3)
	ruSetBoard(g, 0, nil,
		nil,
		[]RUTile{ruT(1, RURed, 5), ruT(2, RUBlue, 10), ruJk(3)},
		[]RUTile{ruT(4, RURed, 1)})
	g.Players[0].Melded = true
	g.Players[1].Melded = true
	g.Players[2].Melded = false // 등록 실패

	g.settle([]int{0}, "받침대 비우기")

	wantScore := []int{165, -65, -100}
	for i, want := range wantScore {
		if g.Players[i].Score != want {
			t.Fatalf("seat%d 점수 = %d, want %d", i, g.Players[i].Score, want)
		}
	}
	if g.Result == nil || len(g.Result.WinnerSeats) != 1 || g.Result.WinnerSeats[0] != 0 {
		t.Fatalf("승자 = %+v", g.Result)
	}
	if g.Result.WinnerNames[0] != "P0" {
		t.Fatalf("승자 이름 = %v", g.Result.WinnerNames)
	}
	// rows 는 점수 내림차순
	if len(g.Result.Rows) != 3 ||
		g.Result.Rows[0].Seat != 0 || g.Result.Rows[1].Seat != 1 || g.Result.Rows[2].Seat != 2 {
		t.Fatalf("정산 표 = %+v", g.Result.Rows)
	}
	for _, row := range g.Result.Rows {
		if !hasHangul(row.Detail) {
			t.Fatalf("정산 설명이 한글이 아니다: %q", row.Detail)
		}
	}
	if !strings.Contains(g.Result.Rows[1].Detail, "조커") {
		t.Fatalf("조커 벌점 설명이 없다: %q", g.Result.Rows[1].Detail)
	}
	if !strings.Contains(g.Result.Rows[2].Detail, "등록") {
		t.Fatalf("미등록 벌점 설명이 없다: %q", g.Result.Rows[2].Detail)
	}
	if !hasHangul(g.Result.Message) {
		t.Fatalf("정산 문구가 한글이 아니다: %q", g.Result.Message)
	}

	// ---- 조커 벌점 표 ----
	scoreCases := []struct {
		name string
		rack []RUTile
		want int
	}{
		{"빈 받침대", nil, 0},
		{"숫자 타일만", []RUTile{ruT(1, RURed, 13), ruT(2, RUBlue, 1)}, 14},
		{"조커 1개", []RUTile{ruJk(1)}, 50},
		{"조커 2개 + 숫자", []RUTile{ruJk(1), ruJk(2), ruT(3, RUBlack, 7)}, 107},
	}
	for _, tc := range scoreCases {
		if got := ruRackScore(tc.rack); got != tc.want {
			t.Fatalf("%s: ruRackScore = %d, want %d", tc.name, got, tc.want)
		}
	}

	// ---- 타일더미 소진 종료: 남은 타일 점수가 가장 낮은 사람이 이긴다 ----
	g2 := ruFixture(t, 3)
	ruSetBoard(g2, 0, nil,
		[]RUTile{ruT(1, RURed, 12), ruT(2, RUBlue, 13)}, // 25점
		[]RUTile{ruT(3, RUBlack, 2)},                    // 2점 ← 최소
		[]RUTile{ruT(4, RUOrange, 9)})                   // 9점
	for _, p := range g2.Players {
		p.Melded = true
	}
	g2.settle(g2.lowestSeats(), "타일더미 소진")
	if len(g2.Result.WinnerSeats) != 1 || g2.Result.WinnerSeats[0] != 1 {
		t.Fatalf("최소 점수 승자 = %+v", g2.Result.WinnerSeats)
	}
	if g2.Players[1].Score != 34 { // 25 + 9
		t.Fatalf("승자 점수 = %d, want 34", g2.Players[1].Score)
	}

	// ---- 미등록은 타일이 적어도 100점 벌점이라 최소가 될 수 없다 ----
	g3 := ruFixture(t, 2)
	ruSetBoard(g3, 0, nil,
		[]RUTile{ruT(1, RURed, 1)},  // 미등록 → 100점
		[]RUTile{ruT(2, RUBlue, 9)}) // 등록 → 9점
	g3.Players[0].Melded = false
	g3.Players[1].Melded = true
	g3.settle(g3.lowestSeats(), "타일더미 소진")
	if len(g3.Result.WinnerSeats) != 1 || g3.Result.WinnerSeats[0] != 1 {
		t.Fatalf("미등록자가 이겼다: %+v", g3.Result.WinnerSeats)
	}
	if g3.Players[0].Score != -RUNoMeldPenalty || g3.Players[1].Score != RUNoMeldPenalty {
		t.Fatalf("점수 = %d / %d", g3.Players[0].Score, g3.Players[1].Score)
	}

	// ---- 동점이면 공동 승 ----
	g4 := ruFixture(t, 2)
	ruSetBoard(g4, 0, nil, []RUTile{ruT(1, RURed, 5)}, []RUTile{ruT(2, RUBlue, 5)})
	for _, p := range g4.Players {
		p.Melded = true
	}
	g4.settle(g4.lowestSeats(), "타일더미 소진")
	if len(g4.Result.WinnerSeats) != 2 {
		t.Fatalf("동점인데 공동 승이 아니다: %+v", g4.Result)
	}
	if !strings.Contains(g4.Result.Message, "공동") {
		t.Fatalf("공동 승 문구 = %q", g4.Result.Message)
	}
}

// TestRUSetup 시작 배치 — 각자 14개, 타일더미는 나머지
func TestRUSetup(t *testing.T) {
	for _, n := range []int{RUMinPlayers, 3, RUMaxPlayers} {
		g := ruFixture(t, n)
		if g.Phase != RUPhaseTurn || !g.Ready {
			t.Fatalf("%d인 시작 phase = %s", n, g.Phase)
		}
		if len(g.Sets) != 0 {
			t.Fatalf("%d인 시작 테이블 = %v", n, g.Sets)
		}
		if want := 106 - RUStartRack*n; len(g.Pool) != want {
			t.Fatalf("%d인 타일더미 = %d, want %d", n, len(g.Pool), want)
		}
		ids := map[int]bool{}
		for _, p := range g.Players {
			if len(p.Rack) != RUStartRack {
				t.Fatalf("%d인 seat%d 받침대 = %d개", n, p.Seat, len(p.Rack))
			}
			if p.Melded {
				t.Fatalf("%d인 seat%d 가 시작부터 등록 상태다", n, p.Seat)
			}
			for _, tile := range p.Rack {
				if ids[tile.ID] {
					t.Fatalf("타일 ID %d 가 두 곳에 있다", tile.ID)
				}
				ids[tile.ID] = true
			}
		}
		for _, tile := range g.Pool {
			if ids[tile.ID] {
				t.Fatalf("타일 ID %d 가 받침대와 타일더미에 동시에 있다", tile.ID)
			}
			ids[tile.ID] = true
		}
		if len(ids) != 106 {
			t.Fatalf("%d인 전체 타일 = %d, want 106", n, len(ids))
		}
		if g.CurrentSeat < 0 || g.CurrentSeat >= n {
			t.Fatalf("%d인 선 좌석 = %d", n, g.CurrentSeat)
		}
	}

	// 인원 경계
	g := NewRUGame("ru-limit")
	for i := 0; i < RUMaxPlayers; i++ {
		if _, err := g.AddPlayer(fmt.Sprintf("P%d", i)); err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
	}
	if _, err := g.AddPlayer("초과"); err == nil {
		t.Fatal("정원을 넘겨 앉았다")
	} else if !hasHangul(err.Error()) {
		t.Fatalf("오류 문구가 한글이 아니다: %q", err.Error())
	}
	solo := NewRUGame("ru-solo")
	solo.AddPlayer("혼자")
	if solo.CanStart() {
		t.Fatal("1인인데 시작 가능하다")
	}
}

// ==================== 봇 대전 ====================

// ruBotFixture 허브 고루틴 없이 결정적으로 돌리는 n인 방
func ruBotFixture(t *testing.T, n int, seed int64) (*RUHub, *ruRoom, []*RUClient) {
	t.Helper()
	h := NewRUHub()
	h.rng = rand.New(rand.NewSource(seed))
	room := h.lobbyRoomFor("")
	clients := make([]*RUClient, n)
	for i := range clients {
		c := &RUClient{wsClient: newBotWSClient(), Hub: h}
		c.Bot = false // 소켓 없는 사람 취급
		c.Name = fmt.Sprintf("P%d", i)
		seat, err := room.Game.AddPlayer(c.Name)
		if err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
		c.GameID, c.Seat = room.Game.ID, seat
		room.Clients[seat] = c
		h.sessions[c.SessionID] = c
		clients[i] = c
	}
	h.startGame(room)
	h.stopPhaseTimer(room) // 타이머 없이 우리가 직접 차례를 민다
	return h, room, clients
}

// ruDrain 봇 채널에 쌓인 메시지를 버린다 (버퍼 포화로 연결이 끊기지 않게)
func ruDrain(clients []*RUClient) {
	for _, c := range clients {
		drained := false
		for !drained {
			select {
			case <-c.Send:
			default:
				drained = true
			}
		}
	}
}

// ruBotGameResult 봇 한 판의 결과 요약 (봇 품질 측정용)
type ruBotGameResult struct {
	Turns      int
	RackScores []int // 좌석별 남은 타일 점수 (등록 여부와 무관한 순수 타일 점수)
	Melded     []bool
	Winners    []int
	PoolLeft   int
	ByPool     bool // 타일더미 소진으로 끝났는가
}

// ruRunBotGame n봇 한 판을 끝까지 돌린다.
// 스냅샷 → 두뇌 → 허브 핸들러 경로가 실제 WS 경로와 같다.
func ruRunBotGame(t *testing.T, n int, seed int64) ruBotGameResult {
	t.Helper()
	h, room, clients := ruBotFixture(t, n, seed)
	game := room.Game
	brains := make([]*ruBrain, n)
	for i := range brains {
		brains[i] = &ruBrain{rng: rand.New(rand.NewSource(seed*1000 + int64(i)))}
	}

	for step := 0; step < RUMaxTurns*3 && game.Phase != RUPhaseGameOver; step++ {
		seat := game.CurrentSeat
		if seat < 0 || seat >= n {
			break
		}
		raw, err := json.Marshal(h.buildRUState(room, seat))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var payload interface{}
		json.Unmarshal(raw, &payload)

		beforeSeq := game.StateSeq
		if reply := brains[seat].decide(RUMessage{Type: RUMsgGameState, Payload: payload}); reply != nil {
			h.handleGameMessage(RUGameMessage{Client: clients[seat], Message: *reply})
		}
		h.stopPhaseTimer(room)
		if game.StateSeq == beforeSeq && game.Phase != RUPhaseGameOver {
			// 봇이 막히면 규칙의 자동 진행으로 민다
			game.ForceTurn(h.rng)
			game.DrainEvents()
		}
		// 판 위의 타일은 늘지도 줄지도 않는다
		if got := ruCountTiles(game); got != 106 {
			t.Fatalf("seed %d: 타일 총량 = %d, want 106", seed, got)
		}
		if !ruBoardValid(game.Sets) {
			t.Fatalf("seed %d: 테이블이 무효 상태가 됐다", seed)
		}
		ruDrain(clients)
	}
	if game.Phase != RUPhaseGameOver {
		t.Fatalf("seed %d: %d차례에도 끝나지 않았다", seed, game.Turns)
	}

	res := ruBotGameResult{Turns: game.Turns, PoolLeft: len(game.Pool)}
	for _, p := range game.Players {
		res.RackScores = append(res.RackScores, ruRackScore(p.Rack))
		res.Melded = append(res.Melded, p.Melded)
	}
	if game.Result != nil {
		res.Winners = append([]int{}, game.Result.WinnerSeats...)
		res.ByPool = strings.Contains(game.Result.Message, "타일더미 소진")
	}
	return res
}

// ruCountTiles 판 위의 모든 타일 수 (타일더미 + 테이블 + 전원 받침대)
func ruCountTiles(g *RUGame) int {
	n := len(g.Pool)
	for _, set := range g.Sets {
		n += len(set)
	}
	for _, p := range g.Players {
		n += len(p.Rack)
	}
	return n
}

// TestRUBotQuality 3봇 30판의 평균 차례 수·남은 타일 점수 분포·타일더미
// 소진 비율을 숫자로 남긴다. 타일더미 소진으로 끝나는 판이 절반을 넘으면
// 봇이 너무 못 내려놓는 것이다.
func TestRUBotQuality(t *testing.T) {
	const games = 30
	const seats = RUFillBotTarget

	totalTurns, minTurns, maxTurns := 0, 1<<30, 0
	wins := make([]int, seats)
	byPool := 0
	noMeld := 0
	leftovers := []int{}
	loserLeft := []int{}
	capHit := 0

	for i := 0; i < games; i++ {
		res := ruRunBotGame(t, seats, int64(8600+i))
		totalTurns += res.Turns
		if res.Turns < minTurns {
			minTurns = res.Turns
		}
		if res.Turns > maxTurns {
			maxTurns = res.Turns
		}
		if res.Turns >= RUMaxTurns {
			capHit++
		}
		if res.ByPool {
			byPool++
		}
		winner := map[int]bool{}
		for _, s := range res.Winners {
			wins[s]++
			winner[s] = true
		}
		for seat, score := range res.RackScores {
			leftovers = append(leftovers, score)
			if !winner[seat] {
				loserLeft = append(loserLeft, score)
			}
		}
		for _, m := range res.Melded {
			if !m {
				noMeld++
			}
		}
	}

	avgTurns := float64(totalTurns) / games
	sort.Ints(leftovers)
	sort.Ints(loserLeft)
	sum := 0
	for _, v := range loserLeft {
		sum += v
	}
	poolRate := float64(byPool) / games * 100

	t.Logf("봇 품질 %d판(%d인): 평균 차례 %.1f (최소 %d · 최대 %d · 차례 상한 도달 %d판)",
		games, seats, avgTurns, minTurns, maxTurns, capHit)
	t.Logf("  타일더미 소진으로 끝난 판: %d/%d (%.1f%%) · 받침대 비우기 %d판",
		byPool, games, poolRate, games-byPool)
	t.Logf("  남은 타일 점수(전원) 분포: 최소 %d · 25%% %d · 중앙 %d · 75%% %d · 최대 %d",
		leftovers[0], leftovers[len(leftovers)/4], leftovers[len(leftovers)/2],
		leftovers[len(leftovers)*3/4], leftovers[len(leftovers)-1])
	t.Logf("  패자 남은 타일 점수: 중앙 %d · 평균 %.1f",
		loserLeft[len(loserLeft)/2], float64(sum)/float64(len(loserLeft)))
	t.Logf("  좌석별 승수: %v (총 %d판) · 등록 실패 좌석 %d개/%d",
		wins, games, noMeld, games*seats)

	if capHit > 0 {
		t.Fatalf("차례 상한(%d)에 걸린 판이 %d개 — 정상적으로 끝나야 한다", RUMaxTurns, capHit)
	}
	if poolRate > 50 {
		t.Fatalf("타일더미 소진으로 끝난 비율 %.1f%% — 절반을 넘으면 봇이 너무 못 낸다", poolRate)
	}
	if avgTurns > 200 {
		t.Fatalf("평균 차례 %.1f — 한 판이 너무 길다", avgTurns)
	}
	for seat, w := range wins {
		if w == games {
			t.Fatalf("seat%d 가 %d판을 모두 이겼다 — 선 이점이 굳어 있다", seat, games)
		}
	}
	if noMeld == games*seats {
		t.Fatal("아무도 등록하지 못했다 — 봇의 등록 탐색이 동작하지 않는다")
	}
}
