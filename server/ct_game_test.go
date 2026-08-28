package server

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

// ==================== 시타델 순수 규칙 표 기반 검증 ====================
//
// 과녁은 넷이다 — 직업 호출 순서(1→8) · 암살/도둑 처리 · 건축가 3채 ·
// 점수 계산(먼저 완성 4 · 완성 2 · 다섯 색 3). 허브·타이머 없이 순수 상태만
// 다룬다.

// ctGame 좌석 n개로 시작한 결정적 게임 (시드 고정)
func ctGame(t *testing.T, n int, seed int64) *CTGame {
	t.Helper()
	g := NewCTGame(fmt.Sprintf("test-%d-%d", n, seed))
	for i := 0; i < n; i++ {
		if _, err := g.AddPlayer(fmt.Sprintf("P%d", i)); err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
	}
	if err := g.Start(rand.New(rand.NewSource(seed))); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return g
}

// ctForceRoles 직업 선택 단계를 건너뛰고 좌석마다 직업을 박은 뒤 호출을 연다.
// 표 기반 검증이 선택 순서에 휘둘리지 않게 하는 장치다.
func ctForceRoles(g *CTGame, roles map[int]int) {
	for _, p := range g.Players {
		p.Role = 0
		p.RoleRevealed = 0
		p.Killed = false
		p.Robbed = false
		p.Draw = []CTCard{}
	}
	for seat, role := range roles {
		g.Players[seat].Role = role
	}
	g.RolePool = []int{}
	g.PickIdx = len(g.PickOrder)
	g.KilledRole = 0
	g.RobbedRole = 0
	g.ThiefSeat = -1
	g.CurrentSeat = -1
	g.CallingRole = 0
	g.DrainEvents()
	g.callNext()
}

// ctAdvanceTo 목표 좌석의 차례가 열릴 때까지 앞 직업들의 차례를 흘려 보낸다
// (호출 순서는 직업 번호를 따르므로 좌석 순서와 어긋난다)
func ctAdvanceTo(t *testing.T, g *CTGame, seat int) {
	t.Helper()
	for i := 0; g.CurrentSeat != seat && i < CTRoleCount*3; i++ {
		cur := g.CurrentSeat
		if cur < 0 || (g.Phase != CTPhaseTurn && g.Phase != CTPhaseAbility) {
			break
		}
		if g.Phase == CTPhaseTurn {
			g.Gather(cur, CTGatherGoldKind)
		}
		g.EndTurn(cur)
	}
	if g.CurrentSeat != seat {
		t.Fatalf("seat%d 의 차례가 오지 않았다 (phase=%s seat=%d)", seat, g.Phase, g.CurrentSeat)
	}
	g.DrainEvents()
}

// ctCard 손패에 꽂아 넣을 카드 (표 검증용)
func ctCard(id int, name string, color CTColor, cost int) CTCard {
	return CTCard{ID: id, Name: name, Color: color, Cost: cost}
}

// ==================== 덱 구성 ====================

// TestCTDeckComposition 건물 카드 65장 — 값은 1~5, id 는 1부터 겹치지 않게,
// 다섯 색이 모두 등장하고 같은 이름은 값·색이 하나로 고정된다.
func TestCTDeckComposition(t *testing.T) {
	deck := ctBuildDeck()
	if len(deck) != CTDeckSize {
		t.Fatalf("덱 = %d장, want %d", len(deck), CTDeckSize)
	}

	ids := map[int]bool{}
	perColor := map[CTColor]int{}
	nameSpec := map[string]CTCard{}
	for _, c := range deck {
		if c.ID <= 0 || ids[c.ID] {
			t.Fatalf("카드 id 중복/무효: %+v", c)
		}
		ids[c.ID] = true
		if c.Cost < 1 || c.Cost > 5 {
			t.Fatalf("건물값이 1~5 밖: %+v", c)
		}
		if c.Name == "" || !hasHangul(c.Name) {
			t.Fatalf("건물 이름이 한글이 아니다: %+v", c)
		}
		if prev, ok := nameSpec[c.Name]; ok {
			if prev.Color != c.Color || prev.Cost != c.Cost {
				t.Fatalf("같은 이름의 값·색이 갈린다: %+v vs %+v", prev, c)
			}
		} else {
			nameSpec[c.Name] = c
		}
		perColor[c.Color]++
	}

	for _, color := range ctColors {
		if perColor[color] <= 0 {
			t.Fatalf("%s(%s) 건물이 한 장도 없다", ctColorLabel(color), color)
		}
	}
	if len(perColor) != len(ctColors) {
		t.Fatalf("색 종류 = %d, want %d (%v)", len(perColor), len(ctColors), perColor)
	}

	// 구성표의 Count 합과 실제 장수가 같아야 한다
	sum := 0
	for _, def := range ctBuildings {
		sum += def.Count
	}
	if sum != CTDeckSize {
		t.Fatalf("구성표 합 = %d, want %d", sum, CTDeckSize)
	}
}

// TestCTVocabulary 정식 한국어 용어표가 코드에 그대로 박혀 있는지.
// 마법사·사제·영주로 새면 이 테스트가 잡는다.
func TestCTVocabulary(t *testing.T) {
	roles := []struct {
		role int
		name string
	}{
		{CTRoleAssassin, "암살자"},
		{CTRoleThief, "도둑"},
		{CTRoleMagician, "마술사"},
		{CTRoleKing, "왕"},
		{CTRoleBishop, "주교"},
		{CTRoleMerchant, "상인"},
		{CTRoleArchitect, "건축가"},
		{CTRoleWarlord, "장군"},
	}
	for _, tc := range roles {
		if got := ctRoleName(tc.role); got != tc.name {
			t.Fatalf("%d번 직업 = %q, want %q", tc.role, got, tc.name)
		}
	}
	colors := []struct {
		color CTColor
		label string
	}{
		{CTNoble, "귀족"},
		{CTReligion, "종교"},
		{CTTrade, "상업"},
		{CTMilitary, "군사"},
		{CTUnique, "특수"},
	}
	for _, tc := range colors {
		if got := ctColorLabel(tc.color); got != tc.label {
			t.Fatalf("%s = %q, want %q", tc.color, got, tc.label)
		}
	}
	// 수입 색 짝 — 왕 귀족 · 주교 종교 · 상인 상업 · 장군 군사
	income := map[int]CTColor{
		CTRoleKing: CTNoble, CTRoleBishop: CTReligion,
		CTRoleMerchant: CTTrade, CTRoleWarlord: CTMilitary,
		CTRoleAssassin: "", CTRoleThief: "", CTRoleMagician: "", CTRoleArchitect: "",
	}
	for role, want := range income {
		if got := ctRoleIncomeColor(role); got != want {
			t.Fatalf("%s 수입 색 = %q, want %q", ctRoleName(role), got, want)
		}
	}
}

// ==================== 직업 배분 ====================

// TestCTDealRoles 인원별 앞면 제외 장수 표 — 3인 3장 · 4인 2장 · 5인 1장 ·
// 6/7인 0장. 뒷면 1장은 항상 빠지고 후보에도 없다. 왕은 앞면으로 빠지지 않는다.
func TestCTDealRoles(t *testing.T) {
	table := []struct {
		players int
		faceUp  int
	}{
		{3, 3}, {4, 2}, {5, 1}, {6, 0}, {7, 0},
	}
	for _, tc := range table {
		if got := ctFaceUpCount(tc.players); got != tc.faceUp {
			t.Fatalf("%d인 앞면 제외 = %d, want %d", tc.players, got, tc.faceUp)
		}
		for seed := int64(1); seed <= 40; seed++ {
			g := ctGame(t, tc.players, seed)
			if len(g.FaceUp) != tc.faceUp {
				t.Fatalf("%d인 seed%d 앞면 제외 = %v", tc.players, seed, g.FaceUp)
			}
			wantPool := CTRoleCount - tc.faceUp - 1
			if len(g.RolePool) != wantPool {
				t.Fatalf("%d인 후보 = %d장, want %d (%v)",
					tc.players, len(g.RolePool), wantPool, g.RolePool)
			}
			if len(g.RolePool) < tc.players {
				t.Fatalf("%d인 후보가 인원보다 적다: %v", tc.players, g.RolePool)
			}
			seen := map[int]bool{g.FaceDown: true}
			for _, r := range g.FaceUp {
				if r == CTRoleKing {
					t.Fatalf("왕이 앞면으로 제외됐다 (%d인 seed%d)", tc.players, seed)
				}
				if seen[r] {
					t.Fatalf("직업이 두 번 빠졌다: %d (%v / %d)", r, g.FaceUp, g.FaceDown)
				}
				seen[r] = true
			}
			for _, r := range g.RolePool {
				if seen[r] {
					t.Fatalf("제외된 직업이 후보에 남아 있다: %d", r)
				}
				seen[r] = true
			}
			if len(seen) != CTRoleCount {
				t.Fatalf("직업 8장이 다 쓰이지 않았다: %v", seen)
			}
			if !sort.IntsAreSorted(g.RolePool) {
				t.Fatalf("후보가 오름차순이 아니다: %v", g.RolePool)
			}
		}
	}
}

// TestCTPickOrder 왕관 보유자부터 한 장씩 고르고, 마지막 사람이 고르면
// 호출이 시작된다. 남은 후보는 공개되지 않는다.
func TestCTPickOrder(t *testing.T) {
	g := ctGame(t, 4, 7)
	if g.Phase != CTPhasePickRoles {
		t.Fatalf("시작 단계 = %s", g.Phase)
	}
	if g.CurrentSeat != g.CrownSeat {
		t.Fatalf("첫 선택 좌석 = %d, 왕관 = %d", g.CurrentSeat, g.CrownSeat)
	}
	if g.Round != 1 {
		t.Fatalf("라운드 = %d", g.Round)
	}

	// 차례가 아닌 좌석·후보에 없는 직업은 거절된다
	other := (g.CrownSeat + 1) % len(g.Players)
	if err := g.PickRole(other, g.RolePool[0]); err == nil {
		t.Fatal("차례가 아닌 좌석의 선택이 통과했다")
	}
	missing := 0
	for r := 1; r <= CTRoleCount; r++ {
		found := false
		for _, p := range g.RolePool {
			if p == r {
				found = true
			}
		}
		if !found {
			missing = r
			break
		}
	}
	if err := g.PickRole(g.CurrentSeat, missing); err == nil {
		t.Fatalf("제외된 직업(%d) 선택이 통과했다", missing)
	}

	for i := 0; i < len(g.Players); i++ {
		seat := g.CurrentSeat
		want := (g.CrownSeat + i) % len(g.Players)
		if seat != want {
			t.Fatalf("%d번째 선택 좌석 = %d, want %d", i, seat, want)
		}
		before := len(g.RolePool)
		if err := g.PickRole(seat, g.RolePool[0]); err != nil {
			t.Fatalf("PickRole: %v", err)
		}
		if i < len(g.Players)-1 && len(g.RolePool) != before-1 {
			t.Fatalf("후보가 줄지 않았다: %d → %d", before, len(g.RolePool))
		}
	}
	if len(g.RolePool) != 0 {
		t.Fatalf("선택이 끝난 뒤 후보가 남았다: %v", g.RolePool)
	}
	if g.CallingRole < 1 {
		t.Fatalf("호출이 시작되지 않았다 (callingRole=%d)", g.CallingRole)
	}
	for _, p := range g.Players {
		if p.Role == 0 {
			t.Fatalf("seat%d 직업이 비었다", p.Seat)
		}
		if p.Role == g.FaceDown {
			t.Fatalf("뒷면 제외 직업(%d)을 누군가 쥐었다", g.FaceDown)
		}
	}
}

// ==================== 직업 호출 순서 ====================

// TestCTCallOrder 1번부터 8번까지 번호 순서대로 부른다. 아무도 안 쥔 번호는
// 조용히 건너뛰고, 마지막 번호가 끝나면 라운드가 넘어간다.
func TestCTCallOrder(t *testing.T) {
	g := ctGame(t, 4, 11)
	// 좌석 순서와 직업 번호를 일부러 어긋나게 박는다
	ctForceRoles(g, map[int]int{0: CTRoleWarlord, 1: CTRoleAssassin, 2: CTRoleKing, 3: CTRoleMerchant})

	want := []struct {
		role int
		seat int
	}{
		{CTRoleAssassin, 1},
		{CTRoleKing, 2},
		{CTRoleMerchant, 3},
		{CTRoleWarlord, 0},
	}
	for i, tc := range want {
		if g.CallingRole != tc.role || g.CurrentSeat != tc.seat {
			t.Fatalf("%d번째 호출 = %d번(seat%d), want %d번(seat%d)",
				i, g.CallingRole, g.CurrentSeat, tc.role, tc.seat)
		}
		if g.Players[tc.seat].RoleRevealed != tc.role {
			t.Fatalf("호출된 좌석의 roleRevealed = %d", g.Players[tc.seat].RoleRevealed)
		}
		// 아직 안 불린 좌석의 직업은 비공개다
		for _, p := range g.Players {
			called := false
			for j := 0; j <= i; j++ {
				if want[j].seat == p.Seat {
					called = true
				}
			}
			if !called && p.RoleRevealed != 0 {
				t.Fatalf("아직 안 불린 seat%d 의 직업이 공개됐다 (%d)", p.Seat, p.RoleRevealed)
			}
		}
		// 자원 → 차례 종료 (능력이 있으면 능력 단계를 한 번 더 닫는다)
		if err := g.Gather(tc.seat, CTGatherGoldKind); err != nil {
			t.Fatalf("Gather: %v", err)
		}
		if err := g.EndTurn(tc.seat); err != nil {
			t.Fatalf("EndTurn: %v", err)
		}
		if g.Phase == CTPhaseAbility {
			if err := g.EndTurn(tc.seat); err != nil {
				t.Fatalf("EndTurn(ability): %v", err)
			}
		}
	}
	// 8번까지 다 불렀으면 다음 라운드의 직업 선택이 열린다
	if g.Phase != CTPhasePickRoles || g.Round != 2 {
		t.Fatalf("라운드 전환 실패: phase=%s round=%d", g.Phase, g.Round)
	}
	if g.CallingRole != 0 {
		t.Fatalf("직업 선택 단계의 callingRole = %d, want 0", g.CallingRole)
	}
}

// TestCTKingCrown 왕이 호출되면 다음 라운드 왕관이 그 좌석으로 간다
// (암살당해도 왕관은 간다 — 정식 규칙)
func TestCTKingCrown(t *testing.T) {
	table := []struct {
		name   string
		killed bool
	}{
		{"왕이 무사할 때", false},
		{"왕이 암살당했을 때", true},
	}
	for _, tc := range table {
		t.Run(tc.name, func(t *testing.T) {
			g := ctGame(t, 3, 21)
			g.CrownSeat = 0
			ctForceRoles(g, map[int]int{0: CTRoleAssassin, 1: CTRoleKing, 2: CTRoleMerchant})
			if tc.killed {
				g.EndTurn(0) // 능력 단계로
				if err := g.Ability(0, CTAbilityPayload{TargetRole: CTRoleKing}); err != nil {
					t.Fatalf("암살: %v", err)
				}
			} else {
				g.Gather(0, CTGatherGoldKind)
				g.EndTurn(0)
				g.EndTurn(0)
			}
			if g.CrownNext != 1 {
				t.Fatalf("왕관 예약 = %d, want 1", g.CrownNext)
			}
			if tc.killed && !g.Players[1].Killed {
				t.Fatal("암살당한 왕의 killed 가 false")
			}
			// 남은 차례를 끝내고 라운드를 넘긴다
			for g.Phase != CTPhasePickRoles && g.Phase != CTPhaseGameOver {
				seat := g.CurrentSeat
				if seat < 0 {
					break
				}
				if g.Phase == CTPhaseTurn {
					g.Gather(seat, CTGatherGoldKind)
				}
				g.EndTurn(seat)
			}
			if g.CrownSeat != 1 {
				t.Fatalf("다음 라운드 왕관 = %d, want 1", g.CrownSeat)
			}
			if g.PickOrder[0] != 1 {
				t.Fatalf("선택 순서 첫 좌석 = %d, want 1", g.PickOrder[0])
			}
		})
	}
}

// ==================== 암살자 · 도둑 ====================

// TestCTAssassin 암살자가 지목한 직업은 차례를 통째로 건너뛴다.
// 자기 자신은 지목할 수 없다.
func TestCTAssassin(t *testing.T) {
	// 8번(장군)을 뒤에 둬야 7번이 건너뛰어진 직후의 상태를 볼 수 있다
	// (라운드가 넘어가면 killed 표식이 초기화된다)
	g := ctGame(t, 4, 31)
	ctForceRoles(g, map[int]int{
		0: CTRoleAssassin, 1: CTRoleArchitect, 2: CTRoleKing, 3: CTRoleWarlord})

	g.Gather(0, CTGatherGoldKind)
	if err := g.EndTurn(0); err != nil {
		t.Fatalf("EndTurn: %v", err)
	}
	if g.Phase != CTPhaseAbility {
		t.Fatalf("암살자 차례 뒤 단계 = %s, want ability", g.Phase)
	}
	if err := g.Ability(0, CTAbilityPayload{TargetRole: CTRoleAssassin}); err == nil {
		t.Fatal("암살자가 자신을 지목했는데 통과했다")
	}
	if err := g.Ability(0, CTAbilityPayload{TargetRole: 99}); err == nil {
		t.Fatal("없는 직업 지목이 통과했다")
	}

	goldBefore := g.Players[1].Gold
	handBefore := len(g.Players[1].Hand)
	if err := g.Ability(0, CTAbilityPayload{TargetRole: CTRoleArchitect}); err != nil {
		t.Fatalf("Ability: %v", err)
	}
	if g.KilledRole != CTRoleArchitect {
		t.Fatalf("지목된 직업 = %d", g.KilledRole)
	}

	// 4번(왕)이 먼저 불리고, 7번(건축가)은 암살당해 건너뛴다
	if g.CallingRole != CTRoleKing || g.CurrentSeat != 2 {
		t.Fatalf("왕 호출 실패: role=%d seat=%d", g.CallingRole, g.CurrentSeat)
	}
	g.Gather(2, CTGatherGoldKind)
	g.EndTurn(2)

	if !g.Players[1].Killed {
		t.Fatal("암살당한 좌석의 killed 가 false")
	}
	if g.Players[1].RoleRevealed != CTRoleArchitect {
		t.Fatalf("암살당한 좌석도 직업은 공개된다: %d", g.Players[1].RoleRevealed)
	}
	if g.Players[1].Gold != goldBefore || len(g.Players[1].Hand) != handBefore {
		t.Fatalf("암살당한 좌석이 자원을 받았다: 금화 %d→%d 손패 %d→%d",
			goldBefore, g.Players[1].Gold, handBefore, len(g.Players[1].Hand))
	}
	// 7번은 건너뛰고 8번(장군)이 이어서 불린다
	if g.CallingRole != CTRoleWarlord || g.CurrentSeat != 3 {
		t.Fatalf("장군 호출 실패: role=%d seat=%d", g.CallingRole, g.CurrentSeat)
	}
	g.Gather(3, CTGatherGoldKind)
	g.EndTurn(3)
	g.EndTurn(3) // 능력 생략
	if g.Phase != CTPhasePickRoles {
		t.Fatalf("라운드가 끝나지 않았다: %s (seat=%d role=%d)",
			g.Phase, g.CurrentSeat, g.CallingRole)
	}
	if g.Players[1].Killed {
		t.Fatal("라운드가 넘어갔는데 killed 표식이 남아 있다")
	}
}

// TestCTThief 도둑은 지목한 직업의 금화를 통째로 뺏는다.
// 암살자·자신·암살당한 직업은 지목할 수 없다.
func TestCTThief(t *testing.T) {
	g := ctGame(t, 3, 41)
	ctForceRoles(g, map[int]int{0: CTRoleThief, 1: CTRoleMerchant, 2: CTRoleAssassin})

	// 2번(도둑)보다 1번(암살자)이 먼저다 — 암살자부터 처리한다
	if g.CallingRole != CTRoleAssassin || g.CurrentSeat != 2 {
		t.Fatalf("암살자 호출 실패: role=%d seat=%d", g.CallingRole, g.CurrentSeat)
	}
	g.Gather(2, CTGatherGoldKind)
	g.EndTurn(2)
	if err := g.Ability(2, CTAbilityPayload{TargetRole: CTRoleKing}); err != nil {
		t.Fatalf("암살: %v", err)
	}

	if g.CallingRole != CTRoleThief || g.CurrentSeat != 0 {
		t.Fatalf("도둑 호출 실패: role=%d seat=%d", g.CallingRole, g.CurrentSeat)
	}
	g.Gather(0, CTGatherGoldKind)
	g.EndTurn(0)

	bad := []struct {
		name string
		role int
	}{
		{"암살자", CTRoleAssassin},
		{"자기 자신", CTRoleThief},
		{"암살당한 직업", CTRoleKing},
		{"없는 직업", 0},
	}
	for _, tc := range bad {
		if err := g.Ability(0, CTAbilityPayload{TargetRole: tc.role}); err == nil {
			t.Fatalf("%s 지목이 통과했다", tc.name)
		}
	}

	thiefGold := g.Players[0].Gold
	victimGold := g.Players[1].Gold // 털리기 전의 금고
	if victimGold <= 0 {
		t.Fatalf("피해자 금화가 0이라 검증이 무의미하다")
	}
	if err := g.Ability(0, CTAbilityPayload{TargetRole: CTRoleMerchant}); err != nil {
		t.Fatalf("Ability: %v", err)
	}

	// 6번(상인) 호출 — 도둑질이 먼저 정산되고 그 뒤 상인 수당이 붙는다
	if g.CallingRole != CTRoleMerchant || g.CurrentSeat != 1 {
		t.Fatalf("상인 호출 실패: role=%d seat=%d", g.CallingRole, g.CurrentSeat)
	}
	if !g.Players[1].Robbed {
		t.Fatal("도둑질당한 좌석의 robbed 가 false")
	}
	if got := g.Players[0].Gold; got != thiefGold+victimGold {
		t.Fatalf("도둑 금화 = %d, want %d(+%d)", got, thiefGold+victimGold, victimGold)
	}
	if g.Players[1].Gold != CTMerchantGold {
		t.Fatalf("털린 뒤 상인 금화 = %d, want %d (상인 수당만)",
			g.Players[1].Gold, CTMerchantGold)
	}
}

// ==================== 자원 · 건설 ====================

// TestCTGatherAndKeep 자원 — 금화 2 또는 카드 2장 뽑아 1장 남기기.
// 자원을 받기 전에는 건설할 수 없고, 두 번 받을 수도 없다.
func TestCTGatherAndKeep(t *testing.T) {
	g := ctGame(t, 3, 51)
	ctForceRoles(g, map[int]int{0: CTRoleKing, 1: CTRoleBishop, 2: CTRoleMerchant})
	seat := g.CurrentSeat
	p := g.Players[seat]

	if err := g.Build(seat, p.Hand[0].ID); err == nil {
		t.Fatal("자원 전에 건설이 통과했다")
	}

	// 카드 2장 뽑기
	handBefore := len(p.Hand)
	deckBefore := len(g.Deck)
	if err := g.Gather(seat, CTGatherCardsKind); err != nil {
		t.Fatalf("Gather(cards): %v", err)
	}
	if g.Phase != CTPhaseKeepCard || len(p.Draw) != CTGatherDraw {
		t.Fatalf("keep_card 진입 실패: phase=%s draw=%d", g.Phase, len(p.Draw))
	}
	if len(g.Deck) != deckBefore-CTGatherDraw {
		t.Fatalf("덱 = %d, want %d", len(g.Deck), deckBefore-CTGatherDraw)
	}
	if err := g.Gather(seat, CTGatherGoldKind); err == nil {
		t.Fatal("keep_card 중 자원 재요청이 통과했다")
	}
	if err := g.Keep(seat, 9); err == nil {
		t.Fatal("범위 밖 인덱스가 통과했다")
	}
	kept := p.Draw[1]
	if err := g.Keep(seat, 1); err != nil {
		t.Fatalf("Keep: %v", err)
	}
	if len(p.Hand) != handBefore+1 || p.Hand[len(p.Hand)-1].ID != kept.ID {
		t.Fatalf("남긴 카드가 손에 없다: %+v", p.Hand)
	}
	if len(p.Draw) != 0 || g.Phase != CTPhaseTurn {
		t.Fatalf("keep 뒤 상태: draw=%d phase=%s", len(p.Draw), g.Phase)
	}
	if err := g.Gather(seat, CTGatherGoldKind); err == nil {
		t.Fatal("자원을 두 번 받았다")
	}

	// 다음 좌석은 금화 2
	g.EndTurn(seat)
	seat = g.CurrentSeat
	gold := g.Players[seat].Gold
	if err := g.Gather(seat, "보석"); err == nil {
		t.Fatal("알 수 없는 자원 종류가 통과했다")
	}
	if err := g.Gather(seat, CTGatherGoldKind); err != nil {
		t.Fatalf("Gather(gold): %v", err)
	}
	if g.Players[seat].Gold != gold+CTGatherGold {
		t.Fatalf("금화 = %d, want %d", g.Players[seat].Gold, gold+CTGatherGold)
	}
}

// TestCTBuildRules 건설 — 값만큼 금화를 내고, 같은 이름은 두 번 못 짓고,
// 보통은 1채·건축가는 3채까지.
func TestCTBuildRules(t *testing.T) {
	table := []struct {
		name  string
		role  int
		limit int
	}{
		{"보통 직업은 1채", CTRoleKing, CTBuildsNormal},
		{"건축가는 3채", CTRoleArchitect, CTBuildsArchitect},
	}
	for _, tc := range table {
		t.Run(tc.name, func(t *testing.T) {
			g := ctGame(t, 3, 61)
			ctForceRoles(g, map[int]int{0: tc.role, 1: CTRoleBishop, 2: CTRoleMerchant})
			seat := 0
			ctAdvanceTo(t, g, seat)
			p := g.Players[seat]
			p.Gold = 30
			p.Hand = []CTCard{
				ctCard(901, "여관", CTTrade, 1),
				ctCard(902, "사원", CTReligion, 1),
				ctCard(903, "저택", CTNoble, 3),
				ctCard(904, "여관", CTTrade, 1), // 같은 이름 — 두 번은 못 짓는다
				ctCard(905, "궁전", CTNoble, 5),
			}
			if err := g.Gather(seat, CTGatherGoldKind); err != nil {
				t.Fatalf("Gather: %v", err)
			}

			built := 0
			for _, id := range []int{901, 902, 903} {
				if err := g.Build(seat, id); err != nil {
					if built >= tc.limit {
						break
					}
					t.Fatalf("%d 건설 실패: %v", id, err)
				}
				built++
			}
			if built != tc.limit {
				t.Fatalf("지은 채수 = %d, want %d", built, tc.limit)
			}
			if len(p.Built) != tc.limit {
				t.Fatalf("도시 = %d채", len(p.Built))
			}
			if err := g.Build(seat, 905); err == nil {
				t.Fatalf("상한(%d채)을 넘겨 지었다", tc.limit)
			}

			// 같은 이름 금지 · 금화 부족 · 손에 없는 카드
			g2 := ctGame(t, 3, 62)
			ctForceRoles(g2, map[int]int{0: CTRoleArchitect, 1: CTRoleBishop, 2: CTRoleMerchant})
			ctAdvanceTo(t, g2, 0)
			q := g2.Players[0]
			q.Gold = 2
			q.Hand = []CTCard{
				ctCard(911, "여관", CTTrade, 1),
				ctCard(912, "여관", CTTrade, 1),
				ctCard(913, "궁전", CTNoble, 5),
			}
			g2.Gather(0, CTGatherGoldKind)
			if err := g2.Build(0, 911); err != nil {
				t.Fatalf("첫 건설: %v", err)
			}
			if err := g2.Build(0, 912); err == nil {
				t.Fatal("같은 이름을 두 번 지었다")
			}
			if err := g2.Build(0, 913); err == nil {
				t.Fatal("금화가 모자란데 지었다")
			}
			if err := g2.Build(0, 99999); err == nil {
				t.Fatal("손에 없는 카드를 지었다")
			}
			if q.Gold != 2+CTGatherGold-1 {
				t.Fatalf("건설 뒤 금화 = %d", q.Gold)
			}
		})
	}
}

// ==================== 수입 ====================

// TestCTIncome 왕 노랑 · 주교 파랑 · 상인 초록 · 장군 빨강 — 자기 색 건물
// 1채당 금화 1. 상인은 금화 1을 더 받고 건축가는 카드 2장을 더 뽑는다.
func TestCTIncome(t *testing.T) {
	city := []CTCard{
		ctCard(801, "저택", CTNoble, 3),
		ctCard(802, "성", CTNoble, 4),
		ctCard(803, "사원", CTReligion, 1),
		ctCard(804, "여관", CTTrade, 1),
		ctCard(805, "시장", CTTrade, 2),
		ctCard(806, "무역소", CTTrade, 2),
		ctCard(807, "파수탑", CTMilitary, 1),
	}
	table := []struct {
		role      string
		roleID    int
		wantGold  int
		wantDrawn int
	}{
		{"왕", CTRoleKing, 2, 0},
		{"주교", CTRoleBishop, 1, 0},
		{"상인", CTRoleMerchant, 3 + CTMerchantGold, 0},
		{"장군", CTRoleWarlord, 1, 0},
		{"암살자", CTRoleAssassin, 0, 0},
		{"건축가", CTRoleArchitect, 0, CTArchitectDraw},
	}
	for _, tc := range table {
		t.Run(tc.role, func(t *testing.T) {
			g := ctGame(t, 3, 71)
			for _, p := range g.Players {
				p.Gold = 0
			}
			g.Players[0].Built = append([]CTCard{}, city...)
			handBefore := len(g.Players[0].Hand)
			other := CTRoleThief
			if tc.roleID == CTRoleThief {
				other = CTRoleMagician
			}
			ctForceRoles(g, map[int]int{0: tc.roleID, 1: other, 2: CTRoleBishop})
			// 호출이 좌석 0에 닿을 때까지 다른 좌석의 차례를 흘려 보낸다
			for g.CurrentSeat != 0 && g.Phase != CTPhasePickRoles && g.Phase != CTPhaseGameOver {
				seat := g.CurrentSeat
				if g.Phase == CTPhaseTurn {
					g.Gather(seat, CTGatherGoldKind)
				}
				g.EndTurn(seat)
			}
			if g.CurrentSeat != 0 {
				t.Fatalf("좌석 0의 차례가 오지 않았다 (phase=%s)", g.Phase)
			}
			if got := g.Players[0].Gold; got != tc.wantGold {
				t.Fatalf("%s 수입 = %d, want %d", tc.role, got, tc.wantGold)
			}
			if got := len(g.Players[0].Hand) - handBefore; got != tc.wantDrawn {
				t.Fatalf("%s 추가 카드 = %d, want %d", tc.role, got, tc.wantDrawn)
			}
		})
	}
}

// ==================== 마술사 · 장군 ====================

// TestCTMagician 손패를 통째로 바꾸거나, 원하는 만큼 버리고 그 수만큼 새로 뽑는다
func TestCTMagician(t *testing.T) {
	t.Run("손패 교환", func(t *testing.T) {
		g := ctGame(t, 3, 81)
		ctForceRoles(g, map[int]int{0: CTRoleMagician, 1: CTRoleKing, 2: CTRoleBishop})
		g.Players[0].Hand = []CTCard{ctCard(921, "여관", CTTrade, 1)}
		g.Players[1].Hand = []CTCard{
			ctCard(931, "궁전", CTNoble, 5),
			ctCard(932, "성", CTNoble, 4),
			ctCard(933, "요새", CTMilitary, 5),
		}
		g.Gather(0, CTGatherGoldKind)
		g.EndTurn(0)
		if g.Phase != CTPhaseAbility {
			t.Fatalf("능력 단계 진입 실패: %s", g.Phase)
		}
		self := 0
		if err := g.Ability(0, CTAbilityPayload{TargetSeat: &self}); err == nil {
			t.Fatal("자신과 손패를 바꿨다")
		}
		bad := 9
		if err := g.Ability(0, CTAbilityPayload{TargetSeat: &bad}); err == nil {
			t.Fatal("없는 좌석과 손패를 바꿨다")
		}
		target := 1
		if err := g.Ability(0, CTAbilityPayload{TargetSeat: &target}); err != nil {
			t.Fatalf("Ability: %v", err)
		}
		if len(g.Players[0].Hand) != 3 || len(g.Players[1].Hand) != 1 {
			t.Fatalf("교환 실패: %d장 / %d장", len(g.Players[0].Hand), len(g.Players[1].Hand))
		}
		if g.Players[0].Hand[0].ID != 931 || g.Players[1].Hand[0].ID != 921 {
			t.Fatalf("교환 내용이 다르다: %+v / %+v", g.Players[0].Hand, g.Players[1].Hand)
		}
	})

	t.Run("버리고 새로 뽑기", func(t *testing.T) {
		g := ctGame(t, 3, 82)
		ctForceRoles(g, map[int]int{0: CTRoleMagician, 1: CTRoleKing, 2: CTRoleBishop})
		g.Players[0].Hand = []CTCard{
			ctCard(941, "여관", CTTrade, 1),
			ctCard(942, "사원", CTReligion, 1),
			ctCard(943, "저택", CTNoble, 3),
		}
		g.Gather(0, CTGatherGoldKind)
		g.EndTurn(0)
		if err := g.Ability(0, CTAbilityPayload{Discard: []int{0, 0}}); err == nil {
			t.Fatal("같은 카드를 두 번 버렸다")
		}
		if err := g.Ability(0, CTAbilityPayload{Discard: []int{9}}); err == nil {
			t.Fatal("손에 없는 카드를 버렸다")
		}
		if err := g.Ability(0, CTAbilityPayload{}); err == nil {
			t.Fatal("대상 없는 마술사 능력이 통과했다")
		}
		deckBefore := len(g.Deck)
		if err := g.Ability(0, CTAbilityPayload{Discard: []int{0, 2}}); err != nil {
			t.Fatalf("Ability: %v", err)
		}
		hand := g.Players[0].Hand
		if len(hand) != 3 {
			t.Fatalf("버린 만큼 뽑지 않았다: %d장", len(hand))
		}
		if hand[0].ID != 942 {
			t.Fatalf("남긴 카드가 다르다: %+v", hand)
		}
		for _, c := range hand[1:] {
			if c.ID == 941 || c.ID == 943 {
				t.Fatalf("버린 카드가 그대로 돌아왔다: %+v", hand)
			}
		}
		if len(g.Deck) != deckBefore {
			t.Fatalf("덱 장수 = %d, want %d (버린 만큼 되돌아간다)", len(g.Deck), deckBefore)
		}
	})
}

// TestCTWarlord 장군의 파괴 — 비용은 건물값 - 1, 주교는 면역,
// 도시를 완성한 좌석은 건드릴 수 없다.
func TestCTWarlord(t *testing.T) {
	setup := func(t *testing.T, bishopSeat int) *CTGame {
		t.Helper()
		g := ctGame(t, 3, 91)
		roles := map[int]int{0: CTRoleWarlord, 1: CTRoleKing, 2: CTRoleMerchant}
		if bishopSeat > 0 {
			roles[bishopSeat] = CTRoleBishop
		}
		g.Players[1].Built = []CTCard{
			ctCard(951, "여관", CTTrade, 1),
			ctCard(952, "저택", CTNoble, 3),
			ctCard(953, "궁전", CTNoble, 5),
		}
		g.Players[2].Built = []CTCard{ctCard(961, "사원", CTReligion, 1)}
		ctForceRoles(g, roles)
		for g.CurrentSeat != 0 && g.Phase == CTPhaseTurn {
			seat := g.CurrentSeat
			g.Gather(seat, CTGatherGoldKind)
			g.EndTurn(seat)
			if g.Phase == CTPhaseAbility {
				g.EndTurn(seat)
			}
		}
		g.Players[0].Gold = 10
		g.Gather(0, CTGatherGoldKind)
		g.EndTurn(0)
		return g
	}

	t.Run("비용은 건물값-1", func(t *testing.T) {
		g := setup(t, 0)
		gold := g.Players[0].Gold
		target := 1
		if err := g.Ability(0, CTAbilityPayload{TargetSeat: &target, CardID: 952}); err != nil {
			t.Fatalf("Ability: %v", err)
		}
		if g.Players[0].Gold != gold-2 {
			t.Fatalf("파괴 비용 = %d, want 2 (건물값 3 - 1)", gold-g.Players[0].Gold)
		}
		if len(g.Players[1].Built) != 2 {
			t.Fatalf("도시 = %d채", len(g.Players[1].Built))
		}
		for _, c := range g.Players[1].Built {
			if c.ID == 952 {
				t.Fatal("파괴된 건물이 남아 있다")
			}
		}
	})

	t.Run("주교는 면역", func(t *testing.T) {
		g := setup(t, 1)
		target := 1
		if err := g.Ability(0, CTAbilityPayload{TargetSeat: &target, CardID: 952}); err == nil {
			t.Fatal("주교의 건물이 파괴됐다")
		}
	})

	t.Run("금화 부족·잘못된 대상", func(t *testing.T) {
		g := setup(t, 0)
		g.Players[0].Gold = 1
		target := 1
		if err := g.Ability(0, CTAbilityPayload{TargetSeat: &target, CardID: 953}); err == nil {
			t.Fatal("금화가 모자란데 파괴했다")
		}
		if err := g.Ability(0, CTAbilityPayload{TargetSeat: &target, CardID: 99999}); err == nil {
			t.Fatal("그 도시에 없는 건물을 파괴했다")
		}
		if err := g.Ability(0, CTAbilityPayload{CardID: 951}); err == nil {
			t.Fatal("대상 좌석 없이 파괴했다")
		}
	})

	t.Run("완성한 도시는 못 건드린다", func(t *testing.T) {
		g := setup(t, 0)
		city := []CTCard{}
		for i := 0; i < CTBuildTarget; i++ {
			city = append(city, ctCard(970+i, fmt.Sprintf("건물%d", i), CTTrade, 2))
		}
		g.Players[1].Built = city
		target := 1
		if err := g.Ability(0, CTAbilityPayload{TargetSeat: &target, CardID: 970}); err == nil {
			t.Fatalf("건물 %d채를 완성한 도시가 파괴됐다", CTBuildTarget)
		}
	})
}

// ==================== 점수 계산 ====================

// TestCTScore 점수 표 — 건물값 합 + 먼저 완성 4 + 완성(1등 외) 2 + 다섯 색 3
func TestCTScore(t *testing.T) {
	seven := func(colors ...CTColor) []CTCard {
		out := []CTCard{}
		for i, c := range colors {
			out = append(out, ctCard(1000+i, fmt.Sprintf("건물%d", i), c, 3))
		}
		return out
	}
	allFive := seven(CTNoble, CTReligion, CTTrade, CTMilitary, CTUnique,
		CTNoble, CTReligion) // 7채 · 다섯 색
	fourColors := seven(CTNoble, CTReligion, CTTrade, CTMilitary, CTNoble,
		CTReligion, CTTrade) // 7채 · 네 색
	small := seven(CTNoble, CTReligion, CTTrade) // 3채 · 세 색

	table := []struct {
		name      string
		seat      int
		firstSeat int
		built     []CTCard
		want      int
		wantParts []string
	}{
		{"7채 먼저 완성 + 다섯 색", 0, 0, allFive,
			21 + CTBonusFirst + CTBonusAllColors,
			[]string{"건물값 21", "먼저 완성 +4", "다섯 색 +3"}},
		{"7채 완성(1등 아님) + 다섯 색", 1, 0, allFive,
			21 + CTBonusComplete + CTBonusAllColors,
			[]string{"건물값 21", "완성 +2", "다섯 색 +3"}},
		{"7채 완성(1등 아님) · 네 색뿐", 1, 0, fourColors,
			21 + CTBonusComplete,
			[]string{"건물값 21", "완성 +2"}},
		{"미완성 도시", 2, 0, small, 9, []string{"건물값 9"}},
		{"빈 도시", 2, -1, []CTCard{}, 0, []string{"건물값 0"}},
	}
	for _, tc := range table {
		t.Run(tc.name, func(t *testing.T) {
			p := &CTPlayer{Seat: tc.seat, Name: "P", Built: tc.built}
			score, detail := ctScore(p, tc.firstSeat)
			if score != tc.want {
				t.Fatalf("점수 = %d, want %d (%s)", score, tc.want, detail)
			}
			for _, part := range tc.wantParts {
				if !strings.Contains(detail, part) {
					t.Fatalf("내역에 %q 없음: %q", part, detail)
				}
			}
			if !hasHangul(detail) {
				t.Fatalf("내역이 한글이 아니다: %q", detail)
			}
		})
	}
}

// TestCTFinish 7채를 완성하면 그 라운드를 끝까지 진행하고 종료한다
func TestCTFinish(t *testing.T) {
	g := ctGame(t, 3, 101)
	ctForceRoles(g, map[int]int{0: CTRoleArchitect, 1: CTRoleWarlord, 2: CTRoleKing})
	// 좌석 0이 6채를 이미 지어 뒀고, 이번 차례에 한 채를 더 올려 완성한다
	p := g.Players[0]
	p.Built = []CTCard{
		ctCard(1101, "여관", CTTrade, 1),
		ctCard(1102, "사원", CTReligion, 1),
		ctCard(1103, "저택", CTNoble, 3),
		ctCard(1104, "파수탑", CTMilitary, 1),
		ctCard(1105, "천문대", CTUnique, 5),
		ctCard(1106, "시장", CTTrade, 2),
	}
	p.Gold = 10
	p.Hand = []CTCard{ctCard(1107, "성", CTNoble, 4)}

	// 4번(왕) → 7번(건축가) → 8번(장군) 순서라 왕부터 흘려 보낸다
	for g.CurrentSeat != 0 && g.Phase == CTPhaseTurn {
		seat := g.CurrentSeat
		g.Gather(seat, CTGatherGoldKind)
		g.EndTurn(seat)
		if g.Phase == CTPhaseAbility {
			g.EndTurn(seat)
		}
	}
	g.Gather(0, CTGatherGoldKind)
	if err := g.Build(0, 1107); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !g.LastRound || g.FirstCompleteSeat != 0 {
		t.Fatalf("마지막 라운드 판정 실패: lastRound=%v first=%d", g.LastRound, g.FirstCompleteSeat)
	}
	if g.Phase == CTPhaseGameOver {
		t.Fatal("완성 즉시 끝났다 — 그 라운드는 끝까지 진행해야 한다")
	}
	g.EndTurn(0)

	// 남은 8번(장군)까지 부르고 나서야 끝난다
	for g.Phase != CTPhaseGameOver {
		seat := g.CurrentSeat
		if seat < 0 {
			t.Fatalf("진행이 막혔다: phase=%s", g.Phase)
		}
		if g.Phase == CTPhaseTurn {
			g.Gather(seat, CTGatherGoldKind)
		}
		g.EndTurn(seat)
	}
	if g.Result == nil || len(g.Result.WinnerSeats) == 0 {
		t.Fatalf("결과 = %+v", g.Result)
	}
	if len(g.Result.Rows) != len(g.Players) {
		t.Fatalf("점수 내역 = %d줄, want %d", len(g.Result.Rows), len(g.Players))
	}
	if g.Result.WinnerSeats[0] != 0 {
		t.Fatalf("승자 = %v, want [0]", g.Result.WinnerSeats)
	}
	want, _ := ctScore(g.Players[0], 0)
	if g.Result.Rows[0].Score != want {
		t.Fatalf("승자 점수 = %d, want %d", g.Result.Rows[0].Score, want)
	}
	if !hasHangul(g.Result.Message) {
		t.Fatalf("종료 문구 = %q", g.Result.Message)
	}
}

// ==================== AFK 자동 진행 ====================

// TestCTForceProgress 단계별 마감 자동 행동만으로 판이 굴러가는지
func TestCTForceProgress(t *testing.T) {
	g := ctGame(t, 4, 111)
	rounds := 0
	for step := 0; step < 4000 && g.Phase != CTPhaseGameOver; step++ {
		switch g.Phase {
		case CTPhasePickRoles:
			if g.CurrentSeat == g.PickOrder[0] {
				rounds++
			}
			g.ForcePick()
		case CTPhaseTurn:
			g.ForceTurn()
		case CTPhaseKeepCard:
			g.ForceKeep()
		case CTPhaseAbility:
			g.ForceAbility()
		default:
			t.Fatalf("알 수 없는 단계: %s", g.Phase)
		}
		g.DrainEvents()
	}
	if g.Phase != CTPhaseGameOver {
		t.Fatalf("전원 방치 판이 끝나지 않았다 (라운드 %d)", g.Round)
	}
	if g.Round > CTMaxRounds {
		t.Fatalf("라운드 상한을 넘었다: %d", g.Round)
	}
	t.Logf("전원 방치 완주: %d라운드 · 승자 %v", g.Round, g.Result.WinnerNames)
}

// ==================== 봇 품질 측정 ====================

// ctBotFixture 허브 고루틴 없이 결정적으로 돌리기 위한 방
func ctBotFixture(t *testing.T, n int, seed int64) (*CTHub, *ctRoom, []*CTClient) {
	t.Helper()
	h := NewCTHub()
	h.rng = rand.New(rand.NewSource(seed))
	room := h.lobbyRoomFor("")
	clients := make([]*CTClient, n)
	for i := range clients {
		c := &CTClient{wsClient: newBotWSClient(), Hub: h}
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
	h.stopPhaseTimer(room) // 타이머 없이 우리가 직접 단계를 민다
	return h, room, clients
}

// ctDrain 봇 채널에 쌓인 메시지를 버린다 (버퍼 포화로 연결이 끊기지 않게)
func ctDrain(clients []*CTClient) {
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

// ctForcePhase 단계별 자동 진행 (봇이 막혔을 때의 방어선)
func ctForcePhase(g *CTGame) {
	switch g.Phase {
	case CTPhasePickRoles:
		g.ForcePick()
	case CTPhaseTurn:
		g.ForceTurn()
	case CTPhaseKeepCard:
		g.ForceKeep()
	case CTPhaseAbility:
		g.ForceAbility()
	}
	g.DrainEvents()
}

// ctRunBotGame n봇 한 판을 끝까지 돌리고 (라운드 수, 좌석별 승점, 승자 좌석)
// 을 돌려준다. 스냅샷 → 두뇌 → 허브 핸들러 경로가 실제 WS 경로와 같다.
func ctRunBotGame(t *testing.T, n int, seed int64) (rounds int, scores []int, winners []int) {
	t.Helper()
	h, room, clients := ctBotFixture(t, n, seed)
	game := room.Game
	brains := make([]*ctBrain, n)
	for i := range brains {
		brains[i] = &ctBrain{rng: rand.New(rand.NewSource(seed*1000 + int64(i)))}
	}

	for step := 0; step < CTMaxRounds*CTRoleCount*40 && game.Phase != CTPhaseGameOver; step++ {
		seat := game.CurrentSeat
		if seat < 0 || seat >= n {
			ctForcePhase(game)
			continue
		}
		raw, err := json.Marshal(h.buildCTState(room, seat))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var payload interface{}
		json.Unmarshal(raw, &payload)

		beforeSeq := game.StateSeq
		if reply := brains[seat].decide(CTMessage{Type: CTMsgGameState, Payload: payload}); reply != nil {
			h.handleGameMessage(CTGameMessage{Client: clients[seat], Message: *reply})
		}
		h.stopPhaseTimer(room)
		if game.StateSeq == beforeSeq { // 봇이 막히면 규칙의 자동 진행으로 민다
			ctForcePhase(game)
		}
		ctDrain(clients)
	}
	if game.Phase != CTPhaseGameOver {
		t.Fatalf("seed %d: %d라운드에도 끝나지 않았다", seed, game.Round)
	}

	rounds = game.Round
	for _, p := range game.Players {
		score, _ := ctScore(p, game.FirstCompleteSeat)
		scores = append(scores, score)
	}
	if game.Result != nil {
		winners = append([]int{}, game.Result.WinnerSeats...)
	}
	return rounds, scores, winners
}

// TestCTBotQuality 4봇 30판의 평균 라운드 수와 승점 분포를 숫자로 남긴다.
// 평균 라운드가 20을 넘거나 평균 승점이 15점 미만이면 가치 함수가 무너진 것이다.
func TestCTBotQuality(t *testing.T) {
	const games = 30
	const seats = CTFillBotTarget

	totalRounds, minRounds, maxRounds := 0, 1<<30, 0
	wins := make([]int, seats)
	winScores := []int{}
	allScores := []int{}
	slowGames := 0

	for i := 0; i < games; i++ {
		rounds, scores, winners := ctRunBotGame(t, seats, int64(2000+i))
		totalRounds += rounds
		if rounds < minRounds {
			minRounds = rounds
		}
		if rounds > maxRounds {
			maxRounds = rounds
		}
		if rounds > 20 {
			slowGames++
		}
		for _, s := range winners {
			wins[s]++
		}
		best := 0
		for _, sc := range scores {
			allScores = append(allScores, sc)
			if sc > best {
				best = sc
			}
		}
		winScores = append(winScores, best)
	}

	avgRounds := float64(totalRounds) / games
	sort.Ints(winScores)
	sort.Ints(allScores)
	sum := 0
	for _, s := range allScores {
		sum += s
	}
	avgScore := float64(sum) / float64(len(allScores))

	t.Logf("봇 품질 %d판(%d봇): 평균 라운드 %.1f (최소 %d · 최대 %d · 20라운드 초과 %d판)",
		games, seats, avgRounds, minRounds, maxRounds, slowGames)
	t.Logf("  승자 승점 분포: 최소 %d · 중앙 %d · 최대 %d",
		winScores[0], winScores[len(winScores)/2], winScores[len(winScores)-1])
	t.Logf("  전체 승점 분포: 최소 %d · 중앙 %d · 최대 %d · 평균 %.1f",
		allScores[0], allScores[len(allScores)/2], allScores[len(allScores)-1], avgScore)
	t.Logf("  좌석별 승수: %v (총 %d판)", wins, games)

	if avgRounds > 20 {
		t.Fatalf("평균 라운드 %.1f — 20을 넘으면 가치 함수를 손봐야 한다", avgRounds)
	}
	if avgScore < 15 {
		t.Fatalf("평균 승점 %.1f — 15점 미만이면 가치 함수를 손봐야 한다", avgScore)
	}
	for seat, w := range wins {
		if w == games {
			t.Fatalf("seat%d 가 %d판을 모두 이겼다 — 선 이점이 굳어 있다", seat, games)
		}
	}
}
