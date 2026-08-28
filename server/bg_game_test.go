package server

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// ==================== 테스트 공용 ====================

// bgCard 특정 무늬·숫자의 카드를 만든다 (뒤집기 판정을 고정하기 위해)
func bgCard(kind BGKind, suit BGSuit, rank string) BGCard {
	return BGCard{ID: bgTestID(), Kind: kind, Suit: suit, Rank: rank}
}

var bgTestSeq = 1000

func bgTestID() int {
	bgTestSeq++
	return bgTestSeq
}

// bgTestGame 규칙 검증용 결정적 판. 역할·체력을 직접 세팅하고 덱은 비운다
// (필요한 테스트가 bgStack 으로 맨 위를 고정한다).
func bgTestGame(t *testing.T, roles ...BGRole) *BGGame {
	t.Helper()
	g := NewBGGame("bg-test")
	for i, r := range roles {
		if _, err := g.AddPlayer(fmt.Sprintf("P%d", i)); err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
		p := g.Players[i]
		p.Role = r
		p.MaxHP = BGBaseHP
		if r == BGRoleSheriff {
			p.MaxHP += BGSheriffBonusHP
		}
		p.HP = p.MaxHP
		p.Alive = true
	}
	g.rng = rand.New(rand.NewSource(7))
	g.Ready = true
	g.Phase = BGPhaseTurn
	g.CurrentSeat = 0
	g.StartedAt = time.Now()
	return g
}

// bgHand 손패를 통째로 갈아 끼운다
func bgHand(g *BGGame, seat int, cards ...BGCard) {
	g.Players[seat].Hand = append([]BGCard{}, cards...)
}

// bgEquip 장비를 통째로 갈아 끼운다
func bgEquip(g *BGGame, seat int, cards ...BGCard) {
	g.Players[seat].Equipment = append([]BGCard{}, cards...)
}

// bgStack 덱 맨 위를 고정한다 (뒤집기 판정용)
func bgStack(g *BGGame, cards ...BGCard) {
	g.Deck = append([]BGCard{}, cards...)
}

// bgSeatOf 손패에서 그 종류의 인덱스 (없으면 -1)
func bgIndexOf(p *BGPlayer, kind BGKind) int {
	for i, c := range p.Hand {
		if c.Kind == kind {
			return i
		}
	}
	return -1
}

func bgSeatPtr(n int) *int { return &n }

// ==================== 덱 · 이름표 ====================

// TestBGDeckComposition 기본판 80장 — 구성표의 장수 합과 실제 덱이 같아야
// 하고, 갈색 63 · 파란색 17 의 비율도 지켜져야 한다.
func TestBGDeckComposition(t *testing.T) {
	deck := bgBuildDeck()
	if len(deck) != BGDeckSize {
		t.Fatalf("덱 = %d장, want %d", len(deck), BGDeckSize)
	}

	counts := map[BGKind]int{}
	brown, blue := 0, 0
	ids := map[int]bool{}
	for _, c := range deck {
		counts[c.Kind]++
		if ids[c.ID] {
			t.Fatalf("id 중복: %d", c.ID)
		}
		ids[c.ID] = true
		if c.Suit == "" || c.Rank == "" {
			t.Fatalf("무늬·숫자 없는 카드: %+v", c)
		}
		d, ok := bgDef(c.Kind)
		if !ok {
			t.Fatalf("구성표에 없는 카드: %s", c.Kind)
		}
		if d.Blue {
			blue++
		} else {
			brown++
		}
	}
	if brown != 63 || blue != 17 {
		t.Fatalf("갈색 %d장 · 파란색 %d장 — want 63/17", brown, blue)
	}
	for _, d := range bgCards {
		if counts[d.Kind] != d.Count {
			t.Fatalf("%s(%s) = %d장, want %d", d.Label, d.Kind, counts[d.Kind], d.Count)
		}
	}
}

// TestBGCardLabels 한국어 이름표는 정식 표기로 고정한다 —
// "미스"·"술집"·"개틀링" 같은 임의 표기가 스며들면 실패한다.
func TestBGCardLabels(t *testing.T) {
	want := map[BGKind]string{
		BGBang: "뱅!", BGMiss: "빗나감!", BGBeer: "맥주", BGSaloon: "주점",
		BGDuel: "결투", BGGatling: "기관총", BGIndians: "인디언!",
		BGStagecoach: "역마차", BGWellsFargo: "웰스파고", BGStore: "잡화점",
		BGCatBalou: "캣 벌로우", BGPanic: "강탈!",
		BGBarrel: "술통", BGJail: "감옥", BGDynamite: "다이너마이트",
		BGMustang: "야생마", BGScope: "조준경",
		BGSchofield: "스코필드", BGRemington: "레밍턴", BGCarabine: "카빈",
		BGWinchester: "윈체스터", BGVolcanic: "볼캐닉",
	}
	if len(want) != len(bgCards) {
		t.Fatalf("표 길이 = %d, 이름표 %d", len(bgCards), len(want))
	}
	for kind, label := range want {
		if got := bgLabel(kind); got != label {
			t.Fatalf("%s 이름표 = %q, want %q", kind, got, label)
		}
	}
	for _, banned := range []string{"미스", "술집", "개틀링", "패닉", "인디언언"} {
		for _, d := range bgCards {
			if d.Label == banned {
				t.Fatalf("금지 표기 사용: %s", banned)
			}
		}
	}
	// 역할 이름표도 고정
	roles := map[BGRole]string{
		BGRoleSheriff: "보안관", BGRoleDeputy: "부관",
		BGRoleOutlaw: "무법자", BGRoleRenegade: "배신자",
	}
	for r, label := range roles {
		if got := bgRoleLabel(r); got != label {
			t.Fatalf("%s 이름표 = %q, want %q", r, got, label)
		}
	}
}

// TestBGRoleSetup 인원별 역할 구성 — 4인 보안관1·무법자2·배신자1 /
// 5인 +부관1 / 6인 무법자3·부관1 / 7인 무법자3·부관2
func TestBGRoleSetup(t *testing.T) {
	cases := []struct {
		n                                 int
		sheriff, deputy, outlaw, renegade int
	}{
		{4, 1, 0, 2, 1},
		{5, 1, 1, 2, 1},
		{6, 1, 1, 3, 1},
		{7, 1, 2, 3, 1},
	}
	for _, tc := range cases {
		setup, ok := bgRoleSetup[tc.n]
		if !ok {
			t.Fatalf("%d인 구성 없음", tc.n)
		}
		if len(setup) != tc.n {
			t.Fatalf("%d인 구성 길이 = %d", tc.n, len(setup))
		}
		got := [4]int{
			bgCountRole(setup, BGRoleSheriff), bgCountRole(setup, BGRoleDeputy),
			bgCountRole(setup, BGRoleOutlaw), bgCountRole(setup, BGRoleRenegade),
		}
		want := [4]int{tc.sheriff, tc.deputy, tc.outlaw, tc.renegade}
		if got != want {
			t.Fatalf("%d인 구성 = %v, want %v", tc.n, got, want)
		}
	}
}

// TestBGStartSheriffFirst 보안관은 체력 +1 이고 언제나 선이다
func TestBGStartSheriffFirst(t *testing.T) {
	for seed := int64(0); seed < 25; seed++ {
		g := NewBGGame("s")
		for i := 0; i < 5; i++ {
			g.AddPlayer(fmt.Sprintf("P%d", i))
		}
		if err := g.Start(rand.New(rand.NewSource(seed))); err != nil {
			t.Fatalf("Start: %v", err)
		}
		sheriff := -1
		for _, p := range g.Players {
			if p.Role == BGRoleSheriff {
				sheriff = p.Seat
				if p.MaxHP != BGBaseHP+BGSheriffBonusHP {
					t.Fatalf("보안관 최대 체력 = %d", p.MaxHP)
				}
			} else if p.MaxHP != BGBaseHP {
				t.Fatalf("seat%d 최대 체력 = %d", p.Seat, p.MaxHP)
			}
			// 시작 손패는 최대 체력만큼. 단 선(보안관)은 이미 차례가 열려
			// 2장을 더 뽑은 상태다.
			want := p.MaxHP
			if p.Role == BGRoleSheriff {
				want += BGTurnDraw
			}
			if len(p.Hand) != want {
				t.Fatalf("seat%d 시작 손패 = %d장, want %d", p.Seat, len(p.Hand), want)
			}
		}
		// 보안관 좌석에서 차례가 시작된다 (감옥·다이너마이트가 없으니 그대로)
		if g.CurrentSeat != sheriff {
			t.Fatalf("seed %d: 선 = seat%d, 보안관 = seat%d", seed, g.CurrentSeat, sheriff)
		}
		if g.Phase != BGPhaseTurn {
			t.Fatalf("시작 단계 = %s", g.Phase)
		}
	}
}

// ==================== 거리 (핵심) ====================

// TestBGDistanceTable 순수 거리 함수의 표 — 탈락자 제외 · 양방향 최단 ·
// 장비 보정(대상 야생마 +1 · 내 조준경 −1) · 하한 1.
func TestBGDistanceTable(t *testing.T) {
	cases := []struct {
		name    string
		alive   []int
		from    int
		to      int
		mustang bool // 대상의 야생마
		scope   bool // 보는 쪽의 조준경
		want    int
	}{
		{"자기 자신은 0", []int{0, 1, 2, 3}, 0, 0, false, false, 0},
		{"4인 이웃", []int{0, 1, 2, 3}, 0, 1, false, false, 1},
		{"4인 반대쪽 이웃", []int{0, 1, 2, 3}, 0, 3, false, false, 1},
		{"4인 맞은편", []int{0, 1, 2, 3}, 0, 2, false, false, 2},
		{"5인 두 칸", []int{0, 1, 2, 3, 4}, 0, 2, false, false, 2},
		{"5인 반대로 두 칸", []int{0, 1, 2, 3, 4}, 0, 3, false, false, 2},
		{"7인 최대 거리", []int{0, 1, 2, 3, 4, 5, 6}, 0, 3, false, false, 3},
		{"7인 반대로 최대", []int{0, 1, 2, 3, 4, 5, 6}, 0, 4, false, false, 3},
		{"6인 맞은편", []int{0, 1, 2, 3, 4, 5}, 0, 3, false, false, 3},

		// 탈락자는 자리에서 빠져 원탁이 좁아진다
		{"seat1 탈락 → 0-2 가 이웃", []int{0, 2, 3}, 0, 2, false, false, 1},
		{"seat1 탈락 → 0-3 도 이웃", []int{0, 2, 3}, 0, 3, false, false, 1},
		{"seat1·2 탈락 → 0-3 이웃", []int{0, 3}, 0, 3, false, false, 1},
		{"탈락한 대상은 -1", []int{0, 1, 3}, 0, 2, false, false, -1},
		{"탈락한 관측자도 -1", []int{1, 2, 3}, 0, 2, false, false, -1},

		// 장비 보정
		{"대상 야생마 +1", []int{0, 1, 2, 3}, 0, 1, true, false, 2},
		{"내 조준경 -1", []int{0, 1, 2, 3}, 0, 2, false, true, 1},
		{"야생마·조준경 상쇄", []int{0, 1, 2, 3}, 0, 2, true, true, 2},
		{"조준경으로도 하한 1", []int{0, 1, 2, 3}, 0, 1, false, true, 1},
		{"야생마 + 최대 거리", []int{0, 1, 2, 3, 4, 5, 6}, 0, 3, true, false, 4},
		{"탈락 + 야생마", []int{0, 2, 3}, 0, 2, true, false, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := bgDistance(tc.alive, tc.from, tc.to, tc.mustang, tc.scope)
			if got != tc.want {
				t.Fatalf("bgDistance(%v, %d→%d, 야생마=%v, 조준경=%v) = %d, want %d",
					tc.alive, tc.from, tc.to, tc.mustang, tc.scope, got, tc.want)
			}
		})
	}
}

// TestBGDistanceSymmetry 기본 거리는 대칭이다 (보정이 없을 때)
func TestBGDistanceSymmetry(t *testing.T) {
	alive := []int{0, 1, 2, 3, 4, 5, 6}
	for _, a := range alive {
		for _, b := range alive {
			if bgBaseDistance(alive, a, b) != bgBaseDistance(alive, b, a) {
				t.Fatalf("%d↔%d 거리가 비대칭", a, b)
			}
		}
	}
}

// TestBGDistanceInGame 탈락이 실제로 원탁을 좁히고, 장비가 게임 안에서도
// 거리를 바꾸는지 (DistanceBetween 경로)
func TestBGDistanceInGame(t *testing.T) {
	g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade, BGRoleDeputy)

	if d := g.DistanceBetween(0, 2); d != 2 {
		t.Fatalf("5인 0→2 = %d, want 2", d)
	}
	// seat1 탈락 → 0 과 2 가 이웃이 된다
	g.Players[1].Alive = false
	if d := g.DistanceBetween(0, 2); d != 1 {
		t.Fatalf("seat1 탈락 후 0→2 = %d, want 1", d)
	}
	if d := g.DistanceBetween(0, 1); d != -1 {
		t.Fatalf("탈락자와의 거리 = %d, want -1", d)
	}
	// 대상의 야생마
	bgEquip(g, 2, bgCard(BGMustang, BGDiamond, "8"))
	if d := g.DistanceBetween(0, 2); d != 2 {
		t.Fatalf("야생마 적용 0→2 = %d, want 2", d)
	}
	// 내 조준경이 되돌린다
	bgEquip(g, 0, bgCard(BGScope, BGClub, "A"))
	if d := g.DistanceBetween(0, 2); d != 1 {
		t.Fatalf("조준경 적용 0→2 = %d, want 1", d)
	}
	// 조준경은 나만의 것 — 반대 방향은 그대로다
	if d := g.DistanceBetween(2, 0); d != 1 {
		t.Fatalf("2→0 = %d, want 1", d)
	}
}

// TestBGWeaponRangeTable 무기별 사거리 — 뱅!이 닿는 한계를 표로 고정한다
func TestBGWeaponRangeTable(t *testing.T) {
	cases := []struct {
		weapon BGKind
		want   int
	}{
		{"", BGDefaultRange},
		{BGVolcanic, 1},
		{BGSchofield, 2},
		{BGRemington, 3},
		{BGCarabine, 4},
		{BGWinchester, 5},
	}
	for _, tc := range cases {
		p := &BGPlayer{Equipment: []BGCard{}}
		if tc.weapon != "" {
			p.Equipment = append(p.Equipment, bgCard(tc.weapon, BGSpade, "K"))
		}
		if got := bgWeaponRange(p); got != tc.want {
			t.Fatalf("%s 사거리 = %d, want %d", tc.weapon, got, tc.want)
		}
	}
}

// TestBGBangOutOfRange 사거리 밖으로는 쏠 수 없고, 사유가 한글로 온다
func TestBGBangOutOfRange(t *testing.T) {
	g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade,
		BGRoleDeputy, BGRoleOutlaw, BGRoleDeputy)
	bgHand(g, 0, bgCard(BGBang, BGSpade, "A"))

	// 7인 원탁에서 0→3 은 거리 3 — 기본 사거리 1로는 닿지 않는다
	err := g.Play(0, 0, bgSeatPtr(3), nil)
	if err == nil {
		t.Fatal("사거리 밖인데 뱅!이 통과했다")
	}
	if !hasHangul(err.Error()) || !strings.Contains(err.Error(), "사거리") {
		t.Fatalf("사유 문구 = %q", err.Error())
	}

	// 레밍턴(3)을 들면 닿는다
	bgEquip(g, 0, bgCard(BGRemington, BGHeart, "5"))
	if err := g.Play(0, 0, bgSeatPtr(3), nil); err != nil {
		t.Fatalf("레밍턴으로도 못 쏜다: %v", err)
	}
}

// ==================== 카드 효과 표 ====================

// TestBGCardEffectTable 카드별 처리 표를 한 줄씩 실제로 돌려 본다.
// 각 줄은 "이 카드를 내면 판이 이렇게 변한다"를 못 박는다.
func TestBGCardEffectTable(t *testing.T) {
	cases := []struct {
		name   string
		kind   BGKind
		target *int
		// setup 판을 준비한다 (손패의 0번이 검사 대상 카드가 되도록)
		setup func(g *BGGame)
		// check 효과가 적용된 뒤의 판
		check func(t *testing.T, g *BGGame)
		// wantErr 낼 수 없어야 하는 카드
		wantErr bool
	}{
		{
			name: "맥주 — 체력 +1 (최대치까지)", kind: BGBeer,
			setup: func(g *BGGame) { g.Players[0].HP = 2 },
			check: func(t *testing.T, g *BGGame) {
				if g.Players[0].HP != 3 {
					t.Fatalf("체력 = %d, want 3", g.Players[0].HP)
				}
			},
		},
		{
			name: "맥주 — 체력이 가득하면 그대로", kind: BGBeer,
			check: func(t *testing.T, g *BGGame) {
				if g.Players[0].HP != g.Players[0].MaxHP {
					t.Fatalf("체력 = %d", g.Players[0].HP)
				}
			},
		},
		{
			name: "주점 — 자신 제외 전원 +1", kind: BGSaloon,
			setup: func(g *BGGame) {
				for _, p := range g.Players {
					p.HP = 2
				}
			},
			check: func(t *testing.T, g *BGGame) {
				if g.Players[0].HP != 2 {
					t.Fatalf("자신도 회복됐다: %d", g.Players[0].HP)
				}
				for _, p := range g.Players[1:] {
					if p.HP != 3 {
						t.Fatalf("seat%d 체력 = %d, want 3", p.Seat, p.HP)
					}
				}
			},
		},
		{
			name: "역마차 — 2장", kind: BGStagecoach,
			setup: func(g *BGGame) { bgStack(g, bgFillDeck(6)...) },
			check: func(t *testing.T, g *BGGame) {
				if len(g.Players[0].Hand) != BGStagecoachDraw {
					t.Fatalf("손패 = %d장", len(g.Players[0].Hand))
				}
			},
		},
		{
			name: "웰스파고 — 3장", kind: BGWellsFargo,
			setup: func(g *BGGame) { bgStack(g, bgFillDeck(6)...) },
			check: func(t *testing.T, g *BGGame) {
				if len(g.Players[0].Hand) != BGWellsFargoDraw {
					t.Fatalf("손패 = %d장", len(g.Players[0].Hand))
				}
			},
		},
		{
			name: "빗나감! — 차례에는 낼 수 없다", kind: BGMiss, wantErr: true,
		},
		{
			name: "뱅! — 대상을 안 고르면 거절", kind: BGBang, wantErr: true,
		},
		{
			name: "캣 벌로우 — 대상 손패 1장을 버리게 한다", kind: BGCatBalou,
			target: bgSeatPtr(2),
			setup: func(g *BGGame) {
				bgHand(g, 2, bgCard(BGBang, BGClub, "3"), bgCard(BGBeer, BGHeart, "4"))
			},
			check: func(t *testing.T, g *BGGame) {
				if len(g.Players[2].Hand) != 1 {
					t.Fatalf("대상 손패 = %d장, want 1", len(g.Players[2].Hand))
				}
				if len(g.Players[0].Hand) != 0 {
					t.Fatalf("캣 벌로우는 뺏는 게 아니다: %d장", len(g.Players[0].Hand))
				}
			},
		},
		{
			name: "강탈! — 거리 1 이내에서 카드를 뺏는다", kind: BGPanic,
			target: bgSeatPtr(1),
			setup: func(g *BGGame) {
				bgHand(g, 1, bgCard(BGBang, BGClub, "3"))
			},
			check: func(t *testing.T, g *BGGame) {
				if len(g.Players[1].Hand) != 0 {
					t.Fatalf("대상 손패 = %d장", len(g.Players[1].Hand))
				}
				if len(g.Players[0].Hand) != 1 {
					t.Fatalf("내 손패 = %d장, want 1", len(g.Players[0].Hand))
				}
			},
		},
		{
			name: "강탈! — 거리 2 는 거절", kind: BGPanic, target: bgSeatPtr(2),
			setup: func(g *BGGame) {
				bgHand(g, 2, bgCard(BGBang, BGClub, "3"))
			},
			wantErr: true,
		},
		{
			name: "감옥 — 보안관은 가둘 수 없다", kind: BGJail, target: bgSeatPtr(0),
			setup:   func(g *BGGame) { g.CurrentSeat = 1 },
			wantErr: true,
		},
		{
			name: "스코필드 — 사거리 2 장비", kind: BGSchofield,
			check: func(t *testing.T, g *BGGame) {
				if bgWeaponRange(g.Players[0]) != 2 {
					t.Fatalf("사거리 = %d", bgWeaponRange(g.Players[0]))
				}
				if len(g.Players[0].Equipment) != 1 {
					t.Fatalf("장비 = %d장", len(g.Players[0].Equipment))
				}
			},
		},
		{
			name: "야생마 — 남이 보는 내 거리가 +1", kind: BGMustang,
			check: func(t *testing.T, g *BGGame) {
				if d := g.DistanceBetween(1, 0); d != 2 {
					t.Fatalf("1→0 = %d, want 2", d)
				}
			},
		},
		{
			name: "조준경 — 내가 보는 거리가 −1", kind: BGScope,
			check: func(t *testing.T, g *BGGame) {
				if d := g.DistanceBetween(0, 2); d != 1 {
					t.Fatalf("0→2 = %d, want 1", d)
				}
				if d := g.DistanceBetween(2, 0); d != 2 {
					t.Fatalf("2→0 = %d, want 2 (조준경은 내 것)", d)
				}
			},
		},
		{
			name: "술통 — 같은 칸을 두 번 채울 수 없다", kind: BGBarrel,
			setup: func(g *BGGame) {
				bgEquip(g, 0, bgCard(BGBarrel, BGSpade, "Q"))
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw,
				BGRoleRenegade, BGRoleDeputy)
			seat := 0
			if tc.setup != nil {
				tc.setup(g)
			}
			seat = g.CurrentSeat
			g.Players[seat].Hand = []BGCard{bgCard(tc.kind, BGDiamond, "9")}

			err := g.Play(seat, 0, tc.target, nil)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("%s 가 통과했다", tc.kind)
				}
				if !hasHangul(err.Error()) {
					t.Fatalf("사유가 한글이 아니다: %q", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("Play: %v", err)
			}
			if tc.check != nil {
				tc.check(t, g)
			}
		})
	}
}

// TestBGTargetCardIndexEncoding 캣 벌로우·강탈!의 targetCardIndex 규약 —
// 0 .. handCount-1 은 대상의 손패(뒷면), handCount + i 는 장비 i번째.
// 범위를 벗어나면 첫 장으로 잘라 맞춘다.
func TestBGTargetCardIndexEncoding(t *testing.T) {
	// 대상: 손패 2장(뱅!·맥주) + 장비 2개(스코필드·술통)
	setupVictim := func(g *BGGame) {
		bgHand(g, 1, bgCard(BGBang, BGClub, "3"), bgCard(BGBeer, BGHeart, "4"))
		bgEquip(g, 1, bgCard(BGSchofield, BGDiamond, "J"), bgCard(BGBarrel, BGSpade, "Q"))
	}

	cases := []struct {
		name  string
		index int
		// 뺏긴 카드의 종류와 출처
		wantKind BGKind
		wantHand int // 남은 손패
		wantEqui int // 남은 장비
	}{
		{"0 → 손패 첫 장", 0, BGBang, 1, 2},
		{"1 → 손패 둘째 장", 1, BGBeer, 1, 2},
		{"handCount+0 → 장비 첫째", 2, BGSchofield, 2, 1},
		{"handCount+1 → 장비 둘째", 3, BGBarrel, 2, 1},
		{"범위 밖은 손패 첫 장으로 자른다", 99, BGBang, 1, 2},
		{"음수도 손패 첫 장으로 자른다", -3, BGBang, 1, 2},
	}

	for _, tc := range cases {
		t.Run("강탈!/"+tc.name, func(t *testing.T) {
			g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade)
			setupVictim(g)
			bgHand(g, 0, bgCard(BGPanic, BGSpade, "5"))
			idx := tc.index
			if err := g.Play(0, 0, bgSeatPtr(1), &idx); err != nil {
				t.Fatalf("Play: %v", err)
			}
			if len(g.Players[1].Hand) != tc.wantHand ||
				len(g.Players[1].Equipment) != tc.wantEqui {
				t.Fatalf("대상 손패 %d장 · 장비 %d개, want %d/%d",
					len(g.Players[1].Hand), len(g.Players[1].Equipment),
					tc.wantHand, tc.wantEqui)
			}
			if len(g.Players[0].Hand) != 1 {
				t.Fatalf("뺏은 카드 수 = %d", len(g.Players[0].Hand))
			}
			if got := g.Players[0].Hand[0].Kind; got != tc.wantKind {
				t.Fatalf("뺏은 카드 = %s, want %s", got, tc.wantKind)
			}
		})

		t.Run("캣 벌로우/"+tc.name, func(t *testing.T) {
			g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade)
			setupVictim(g)
			bgHand(g, 0, bgCard(BGCatBalou, BGSpade, "6"))
			idx := tc.index
			if err := g.Play(0, 0, bgSeatPtr(1), &idx); err != nil {
				t.Fatalf("Play: %v", err)
			}
			if len(g.Players[1].Hand) != tc.wantHand ||
				len(g.Players[1].Equipment) != tc.wantEqui {
				t.Fatalf("대상 손패 %d장 · 장비 %d개, want %d/%d",
					len(g.Players[1].Hand), len(g.Players[1].Equipment),
					tc.wantHand, tc.wantEqui)
			}
			// 캣 벌로우는 뺏지 않고 버리게 한다
			if len(g.Players[0].Hand) != 0 {
				t.Fatalf("캣 벌로우가 카드를 가져왔다: %d장", len(g.Players[0].Hand))
			}
			top := g.discardTop()
			if top == nil || top.Kind != tc.wantKind {
				t.Fatalf("버린 더미 맨 위 = %+v, want %s", top, tc.wantKind)
			}
		})
	}

	// 가져갈 카드가 없으면 낼 수 없다
	t.Run("빈손인 대상은 거절", func(t *testing.T) {
		g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade)
		bgHand(g, 0, bgCard(BGPanic, BGSpade, "5"))
		if err := g.Play(0, 0, bgSeatPtr(1), nil); err == nil {
			t.Fatal("빈손인 대상에게 강탈!이 통과했다")
		}
	})
}

// bgFillDeck 아무 내용이나 n장 (뽑기 검증용)
func bgFillDeck(n int) []BGCard {
	out := []BGCard{}
	for i := 0; i < n; i++ {
		out = append(out, bgCard(BGBang, BGClub, "7"))
	}
	return out
}

// TestBGBangOncePerTurn 뱅!은 한 차례에 1장, 볼캐닉이면 무제한
func TestBGBangOncePerTurn(t *testing.T) {
	g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade)
	bgHand(g, 0, bgCard(BGBang, BGSpade, "2"), bgCard(BGBang, BGClub, "3"))
	bgHand(g, 1) // 빗나감! 없음 → 즉시 판정

	if err := g.Play(0, 0, bgSeatPtr(1), nil); err != nil {
		t.Fatalf("첫 뱅!: %v", err)
	}
	if g.BangBlocked(0) != true {
		t.Fatal("첫 뱅! 뒤 BangBlocked 가 false")
	}
	if err := g.Play(0, 0, bgSeatPtr(1), nil); err == nil {
		t.Fatal("두 번째 뱅!이 통과했다")
	}

	// 볼캐닉을 들면 제한이 풀린다
	bgEquip(g, 0, bgCard(BGVolcanic, BGHeart, "10"))
	if g.BangBlocked(0) != false {
		t.Fatal("볼캐닉을 들었는데 BangBlocked 가 true")
	}
	if err := g.Play(0, 0, bgSeatPtr(1), nil); err != nil {
		t.Fatalf("볼캐닉 뱅!: %v", err)
	}

	// 새 차례가 열리면 다시 1장부터
	g.Players[0].Equipment = []BGCard{}
	g.Players[0].BangUsed = 0
	if g.BangBlocked(0) != false {
		t.Fatal("새 차례에도 막혀 있다")
	}
}

// ==================== 대응 창 ====================

// TestBGRespondWindowTable 대응 창(빗나감!·결투·기관총·인디언!)의 결말을
// 표로 고정한다.
func TestBGRespondWindowTable(t *testing.T) {
	t.Run("뱅! — 빗나감!으로 막으면 체력 유지", func(t *testing.T) {
		g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade)
		bgHand(g, 0, bgCard(BGBang, BGSpade, "2"))
		bgHand(g, 1, bgCard(BGMiss, BGClub, "8"))

		if err := g.Play(0, 0, bgSeatPtr(1), nil); err != nil {
			t.Fatalf("Play: %v", err)
		}
		if g.Phase != BGPhaseRespond || g.Pending == nil {
			t.Fatalf("대응 창이 열리지 않았다 (phase=%s)", g.Phase)
		}
		if g.Pending.Kind != BGPendBang || g.Pending.Need != BGNeedMiss ||
			g.Pending.BySeat != 0 || g.Pending.TargetSeat != 1 {
			t.Fatalf("pending = %+v", g.Pending)
		}
		if err := g.Respond(1, bgSeatPtr(0)); err != nil {
			t.Fatalf("Respond: %v", err)
		}
		if g.Players[1].HP != BGBaseHP {
			t.Fatalf("막았는데 체력 = %d", g.Players[1].HP)
		}
		if g.Phase != BGPhaseTurn || g.Pending != nil {
			t.Fatalf("창이 닫히지 않았다 (phase=%s)", g.Phase)
		}
	})

	t.Run("뱅! — 포기하면 체력 −1", func(t *testing.T) {
		g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade)
		bgHand(g, 0, bgCard(BGBang, BGSpade, "2"))
		bgHand(g, 1, bgCard(BGMiss, BGClub, "8"))
		g.Play(0, 0, bgSeatPtr(1), nil)
		if err := g.Respond(1, nil); err != nil {
			t.Fatalf("Respond: %v", err)
		}
		if g.Players[1].HP != BGBaseHP-1 {
			t.Fatalf("체력 = %d, want %d", g.Players[1].HP, BGBaseHP-1)
		}
	})

	t.Run("뱅! — 빗나감!이 없으면 창 없이 즉시 판정", func(t *testing.T) {
		g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade)
		bgHand(g, 0, bgCard(BGBang, BGSpade, "2"))
		bgHand(g, 1, bgCard(BGBeer, BGClub, "8"))
		g.Play(0, 0, bgSeatPtr(1), nil)
		if g.Phase != BGPhaseTurn {
			t.Fatalf("phase = %s — 물어볼 게 없으면 바로 판정해야 한다", g.Phase)
		}
		if g.Players[1].HP != BGBaseHP-1 {
			t.Fatalf("체력 = %d", g.Players[1].HP)
		}
	})

	t.Run("뱅! — 대응 카드는 종류가 맞아야 한다", func(t *testing.T) {
		g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade)
		bgHand(g, 0, bgCard(BGBang, BGSpade, "2"))
		bgHand(g, 1, bgCard(BGMiss, BGClub, "8"), bgCard(BGBeer, BGHeart, "3"))
		g.Play(0, 0, bgSeatPtr(1), nil)
		if err := g.Respond(1, bgSeatPtr(1)); err == nil {
			t.Fatal("맥주로 뱅!을 막았다")
		}
		if err := g.Respond(2, bgSeatPtr(0)); err == nil {
			t.Fatal("남의 대응 창에 끼어들 수 있었다")
		}
	})

	t.Run("술통 — ♥를 뒤집으면 회피", func(t *testing.T) {
		g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade)
		bgHand(g, 0, bgCard(BGBang, BGSpade, "2"))
		bgEquip(g, 1, bgCard(BGBarrel, BGClub, "Q"))
		bgStack(g, bgCard(BGBeer, BGHeart, "7")) // ♥ → 회피
		g.Play(0, 0, bgSeatPtr(1), nil)
		if g.Players[1].HP != BGBaseHP {
			t.Fatalf("술통이 막지 못했다: 체력 %d", g.Players[1].HP)
		}
		if g.Phase != BGPhaseTurn {
			t.Fatalf("phase = %s", g.Phase)
		}
	})

	t.Run("술통 — ♥가 아니면 그대로 맞는다", func(t *testing.T) {
		g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade)
		bgHand(g, 0, bgCard(BGBang, BGSpade, "2"))
		bgEquip(g, 1, bgCard(BGBarrel, BGClub, "Q"))
		bgStack(g, bgCard(BGBeer, BGSpade, "7")) // ♠ → 실패
		g.Play(0, 0, bgSeatPtr(1), nil)
		if g.Players[1].HP != BGBaseHP-1 {
			t.Fatalf("체력 = %d", g.Players[1].HP)
		}
	})

	t.Run("결투 — 교대로 뱅!, 못 내는 쪽이 잃는다", func(t *testing.T) {
		g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade)
		bgHand(g, 0, bgCard(BGDuel, BGSpade, "K"), bgCard(BGBang, BGSpade, "2"))
		bgHand(g, 2, bgCard(BGBang, BGClub, "5"))

		if err := g.Play(0, 0, bgSeatPtr(2), nil); err != nil {
			t.Fatalf("결투: %v", err)
		}
		// 거리 2 라도 결투는 통한다
		if g.Pending == nil || g.Pending.Kind != BGPendDuel ||
			g.Pending.Need != BGNeedBang || g.Pending.TargetSeat != 2 {
			t.Fatalf("pending = %+v", g.Pending)
		}
		// 상대가 뱅!으로 받아치면 이번엔 내가 내야 한다
		if err := g.Respond(2, bgSeatPtr(0)); err != nil {
			t.Fatalf("받아치기: %v", err)
		}
		if g.Pending == nil || g.Pending.TargetSeat != 0 {
			t.Fatalf("교대되지 않았다: %+v", g.Pending)
		}
		// 내가 포기하면 내가 잃는다
		if err := g.Respond(0, nil); err != nil {
			t.Fatalf("포기: %v", err)
		}
		if g.Players[0].HP != g.Players[0].MaxHP-1 {
			t.Fatalf("결투를 건 쪽 체력 = %d", g.Players[0].HP)
		}
		if g.Players[2].HP != BGBaseHP {
			t.Fatalf("받아친 쪽 체력 = %d", g.Players[2].HP)
		}
		if g.Phase != BGPhaseTurn {
			t.Fatalf("phase = %s", g.Phase)
		}
	})

	t.Run("결투 — 뱅!이 없으면 즉시 진다", func(t *testing.T) {
		g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade)
		bgHand(g, 0, bgCard(BGDuel, BGSpade, "K"))
		bgHand(g, 2)
		g.Play(0, 0, bgSeatPtr(2), nil)
		if g.Players[2].HP != BGBaseHP-1 {
			t.Fatalf("체력 = %d", g.Players[2].HP)
		}
		if g.Phase != BGPhaseTurn {
			t.Fatalf("phase = %s", g.Phase)
		}
	})

	t.Run("기관총 — 나머지 전원이 빗나감!으로 막는다", func(t *testing.T) {
		g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade)
		bgHand(g, 0, bgCard(BGGatling, BGSpade, "10"))
		bgHand(g, 1, bgCard(BGMiss, BGClub, "8"))
		bgHand(g, 2)
		bgHand(g, 3)

		g.Play(0, 0, nil, nil)
		// seat1 만 빗나감!이 있어 창이 열린다 (2·3은 즉시 판정)
		if g.Phase != BGPhaseRespond || g.Pending.TargetSeat != 1 {
			t.Fatalf("pending = %+v (phase=%s)", g.Pending, g.Phase)
		}
		if g.Pending.Kind != BGPendGatling {
			t.Fatalf("kind = %s", g.Pending.Kind)
		}
		if err := g.Respond(1, bgSeatPtr(0)); err != nil {
			t.Fatalf("Respond: %v", err)
		}
		if g.Players[0].HP != g.Players[0].MaxHP {
			t.Fatalf("쏜 사람도 맞았다: %d", g.Players[0].HP)
		}
		if g.Players[1].HP != BGBaseHP {
			t.Fatalf("막은 사람 체력 = %d", g.Players[1].HP)
		}
		for _, s := range []int{2, 3} {
			if g.Players[s].HP != BGBaseHP-1 {
				t.Fatalf("seat%d 체력 = %d", s, g.Players[s].HP)
			}
		}
		if g.Phase != BGPhaseTurn {
			t.Fatalf("phase = %s", g.Phase)
		}
	})

	t.Run("인디언! — 뱅!을 버리거나 체력 −1", func(t *testing.T) {
		g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade)
		bgHand(g, 0, bgCard(BGIndians, BGSpade, "A"))
		bgHand(g, 1, bgCard(BGBang, BGClub, "8"))
		bgHand(g, 2, bgCard(BGMiss, BGClub, "9")) // 빗나감!으로는 못 막는다
		bgHand(g, 3)

		g.Play(0, 0, nil, nil)
		if g.Pending == nil || g.Pending.Need != BGNeedBang {
			t.Fatalf("pending = %+v", g.Pending)
		}
		if err := g.Respond(1, bgSeatPtr(0)); err != nil {
			t.Fatalf("Respond: %v", err)
		}
		if g.Players[1].HP != BGBaseHP || len(g.Players[1].Hand) != 0 {
			t.Fatalf("seat1 체력=%d 손패=%d", g.Players[1].HP, len(g.Players[1].Hand))
		}
		if g.Players[2].HP != BGBaseHP-1 {
			t.Fatalf("빗나감!으로 막았다: seat2 체력 %d", g.Players[2].HP)
		}
		if g.Players[3].HP != BGBaseHP-1 {
			t.Fatalf("seat3 체력 = %d", g.Players[3].HP)
		}
	})
}

// ==================== 뒤집기 (술통·감옥·다이너마이트) ====================

// TestBGFlipTable 뒤집기 판정 — ♥ 회피 · ♥ 탈출 · ♠2~9 폭발
func TestBGFlipTable(t *testing.T) {
	cases := []struct {
		name string
		card BGCard
		// 각 판정의 기대
		barrel   bool
		jail     bool
		dynamite bool
	}{
		{"♥7", bgCard(BGBang, BGHeart, "7"), true, true, false},
		{"♥A", bgCard(BGBang, BGHeart, "A"), true, true, false},
		{"♠2", bgCard(BGBang, BGSpade, "2"), false, false, true},
		{"♠9", bgCard(BGBang, BGSpade, "9"), false, false, true},
		{"♠A", bgCard(BGBang, BGSpade, "A"), false, false, false},
		{"♠10", bgCard(BGBang, BGSpade, "10"), false, false, false},
		{"♠K", bgCard(BGBang, BGSpade, "K"), false, false, false},
		{"♦5", bgCard(BGBang, BGDiamond, "5"), false, false, false},
		{"♣5", bgCard(BGBang, BGClub, "5"), false, false, false},
	}
	for _, tc := range cases {
		if got := bgBarrelSaves(tc.card); got != tc.barrel {
			t.Fatalf("%s 술통 = %v, want %v", tc.name, got, tc.barrel)
		}
		if got := bgJailEscapes(tc.card); got != tc.jail {
			t.Fatalf("%s 감옥 = %v, want %v", tc.name, got, tc.jail)
		}
		if got := bgDynamiteBlows(tc.card); got != tc.dynamite {
			t.Fatalf("%s 다이너마이트 = %v, want %v", tc.name, got, tc.dynamite)
		}
	}
}

// TestBGJailSkipsTurn 감옥 — ♥면 탈출, 아니면 차례 통째로 건너뛴다
func TestBGJailSkipsTurn(t *testing.T) {
	t.Run("탈출 실패 → 차례를 건너뛴다", func(t *testing.T) {
		g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade)
		bgEquip(g, 1, bgCard(BGJail, BGSpade, "4"))
		bgStack(g, bgCard(BGBeer, BGClub, "3"), // 감옥 판정 (♣ → 실패)
			bgCard(BGBang, BGDiamond, "5"), bgCard(BGBang, BGDiamond, "6")) // seat2 의 2장
		g.CurrentSeat = 0
		g.nextTurn()

		if g.CurrentSeat != 2 {
			t.Fatalf("차례 = seat%d, want seat2 (seat1은 감옥)", g.CurrentSeat)
		}
		if len(g.Players[1].Equipment) != 0 {
			t.Fatalf("감옥이 남아 있다: %+v", g.Players[1].Equipment)
		}
		if len(g.Players[1].Hand) != 0 {
			t.Fatalf("감옥에 갇혔는데 카드를 뽑았다: %d장", len(g.Players[1].Hand))
		}
	})

	t.Run("탈출 성공 → 그대로 진행", func(t *testing.T) {
		g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade)
		bgEquip(g, 1, bgCard(BGJail, BGSpade, "4"))
		bgStack(g, bgCard(BGBeer, BGHeart, "3"), // ♥ → 탈출
			bgCard(BGBang, BGDiamond, "5"), bgCard(BGBang, BGDiamond, "6"))
		g.CurrentSeat = 0
		g.nextTurn()

		if g.CurrentSeat != 1 {
			t.Fatalf("차례 = seat%d, want seat1", g.CurrentSeat)
		}
		if len(g.Players[1].Hand) != BGTurnDraw {
			t.Fatalf("손패 = %d장", len(g.Players[1].Hand))
		}
	})
}

// TestBGDynamite 다이너마이트 — ♠2~9 면 −3, 아니면 왼쪽으로 넘어간다
func TestBGDynamite(t *testing.T) {
	t.Run("폭발", func(t *testing.T) {
		g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade)
		bgEquip(g, 1, bgCard(BGDynamite, BGHeart, "2"))
		bgStack(g, bgCard(BGBeer, BGSpade, "5"), // ♠5 → 폭발
			bgCard(BGBang, BGDiamond, "5"), bgCard(BGBang, BGDiamond, "6"))
		g.CurrentSeat = 0
		g.nextTurn()

		if g.Players[1].HP != BGBaseHP-BGDynamiteDamage {
			t.Fatalf("체력 = %d, want %d", g.Players[1].HP, BGBaseHP-BGDynamiteDamage)
		}
		if len(g.Players[1].Equipment) != 0 {
			t.Fatalf("터진 다이너마이트가 남아 있다")
		}
		if g.CurrentSeat != 1 {
			t.Fatalf("차례 = seat%d — 살아 있으면 계속한다", g.CurrentSeat)
		}
	})

	t.Run("왼쪽으로 넘어간다", func(t *testing.T) {
		g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade)
		bgEquip(g, 1, bgCard(BGDynamite, BGHeart, "2"))
		bgStack(g, bgCard(BGBeer, BGHeart, "5"), // ♥ → 무사
			bgCard(BGBang, BGDiamond, "5"), bgCard(BGBang, BGDiamond, "6"))
		g.CurrentSeat = 0
		g.nextTurn()

		if len(g.Players[1].Equipment) != 0 {
			t.Fatalf("seat1 에 남아 있다")
		}
		if !bgHasSlot(g.Players[2], bgSlotDynamite) {
			t.Fatalf("seat2 로 넘어가지 않았다")
		}
		if g.Players[1].HP != BGBaseHP {
			t.Fatalf("체력 = %d", g.Players[1].HP)
		}
	})

	t.Run("탈락한 좌석은 건너뛰고 넘긴다", func(t *testing.T) {
		g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade)
		bgEquip(g, 1, bgCard(BGDynamite, BGHeart, "2"))
		g.Players[2].Alive = false
		bgStack(g, bgCard(BGBeer, BGDiamond, "5"),
			bgCard(BGBang, BGDiamond, "5"), bgCard(BGBang, BGDiamond, "6"))
		g.CurrentSeat = 0
		g.nextTurn()

		if !bgHasSlot(g.Players[3], bgSlotDynamite) {
			t.Fatalf("탈락자를 건너뛰지 않았다")
		}
	})
}

// ==================== 탈락 보상 ====================

// TestBGEliminationRewards 무법자 처치 3장 · 보안관이 부관을 죽이면 전부 버림
func TestBGEliminationRewards(t *testing.T) {
	t.Run("무법자를 죽이면 카드 3장", func(t *testing.T) {
		g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade)
		bgStack(g, bgFillDeck(10)...)
		g.Players[1].HP = 1
		g.damage(1, 1, 0)

		if g.Players[1].Alive {
			t.Fatal("탈락하지 않았다")
		}
		if len(g.Players[0].Hand) != BGOutlawBounty {
			t.Fatalf("보상 = %d장, want %d", len(g.Players[0].Hand), BGOutlawBounty)
		}
	})

	t.Run("배신자를 죽여도 보상은 없다", func(t *testing.T) {
		g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade)
		bgStack(g, bgFillDeck(10)...)
		g.Players[3].HP = 1
		g.damage(3, 1, 0)
		if len(g.Players[0].Hand) != 0 {
			t.Fatalf("보상 = %d장, want 0", len(g.Players[0].Hand))
		}
	})

	t.Run("보안관이 부관을 죽이면 손패·장비를 전부 버린다", func(t *testing.T) {
		g := bgTestGame(t, BGRoleSheriff, BGRoleDeputy, BGRoleOutlaw, BGRoleOutlaw,
			BGRoleRenegade)
		bgHand(g, 0, bgCard(BGBang, BGSpade, "2"), bgCard(BGBeer, BGHeart, "3"))
		bgEquip(g, 0, bgCard(BGSchofield, BGClub, "J"))
		bgStack(g, bgFillDeck(10)...)
		g.Players[1].HP = 1
		g.damage(1, 1, 0)

		if len(g.Players[0].Hand) != 0 || len(g.Players[0].Equipment) != 0 {
			t.Fatalf("손패 %d장 · 장비 %d장 — 전부 버려야 한다",
				len(g.Players[0].Hand), len(g.Players[0].Equipment))
		}
	})

	t.Run("무법자가 부관을 죽여도 보안관은 멀쩡하다", func(t *testing.T) {
		g := bgTestGame(t, BGRoleSheriff, BGRoleDeputy, BGRoleOutlaw, BGRoleOutlaw,
			BGRoleRenegade)
		bgHand(g, 0, bgCard(BGBang, BGSpade, "2"))
		bgStack(g, bgFillDeck(10)...)
		g.Players[1].HP = 1
		g.damage(1, 1, 2)
		if len(g.Players[0].Hand) != 1 {
			t.Fatalf("보안관 손패 = %d장", len(g.Players[0].Hand))
		}
	})

	t.Run("탈락자는 손패·장비를 모두 버린다", func(t *testing.T) {
		g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade)
		bgHand(g, 1, bgCard(BGBang, BGSpade, "2"), bgCard(BGBeer, BGHeart, "3"))
		bgEquip(g, 1, bgCard(BGBarrel, BGClub, "Q"))
		bgStack(g, bgFillDeck(10)...)
		before := len(g.DiscardPile)
		g.Players[1].HP = 1
		g.damage(1, 1, -1)
		if len(g.Players[1].Hand) != 0 || len(g.Players[1].Equipment) != 0 {
			t.Fatal("탈락자에게 카드가 남아 있다")
		}
		if len(g.DiscardPile) != before+3 {
			t.Fatalf("버린 더미 = %d장 (before %d)", len(g.DiscardPile), before)
		}
	})
}

// ==================== 종료 판정 ====================

// TestBGGameOverTable 종료 조건 표
func TestBGGameOverTable(t *testing.T) {
	cases := []struct {
		name string
		// roles 좌석 순 역할
		roles []BGRole
		// kill 순서대로 탈락시킬 좌석
		kill []int
		// wantWinner "" 는 아직 안 끝남
		wantWinner string
		// wantSeats 승자 좌석
		wantSeats []int
	}{
		{
			name:       "무법자·배신자 전멸 → 보안관 진영 승",
			roles:      []BGRole{BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade, BGRoleDeputy},
			kill:       []int{1, 2, 3},
			wantWinner: "sheriff", wantSeats: []int{0, 4},
		},
		{
			name:       "보안관 사망 + 여럿 생존 → 무법자 승",
			roles:      []BGRole{BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade},
			kill:       []int{0},
			wantWinner: "outlaw", wantSeats: []int{1, 2},
		},
		{
			name:       "무법자 전멸 뒤 보안관 사망 → 배신자 단독 생존 → 배신자 승",
			roles:      []BGRole{BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade},
			kill:       []int{1, 2, 0},
			wantWinner: "renegade", wantSeats: []int{3},
		},
		{
			name:       "보안관 사망 + 배신자·부관 생존 → 무법자 승 (배신자 단독 아님)",
			roles:      []BGRole{BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade, BGRoleDeputy},
			kill:       []int{1, 2, 0},
			wantWinner: "outlaw", wantSeats: []int{1, 2},
		},
		{
			name:       "무법자만 죽어서는 끝나지 않는다 (배신자 생존)",
			roles:      []BGRole{BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade},
			kill:       []int{1, 2},
			wantWinner: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := bgTestGame(t, tc.roles...)
			bgStack(g, bgFillDeck(40)...)
			for _, seat := range tc.kill {
				if g.Phase == BGPhaseGameOver {
					break
				}
				g.Players[seat].HP = 1
				g.damage(seat, 1, -1)
			}
			if tc.wantWinner == "" {
				if g.Phase == BGPhaseGameOver {
					t.Fatalf("아직 끝나면 안 된다: %+v", g.Result)
				}
				return
			}
			if g.Phase != BGPhaseGameOver || g.Result == nil {
				t.Fatalf("끝나지 않았다 (phase=%s)", g.Phase)
			}
			if g.Result.Winner != tc.wantWinner {
				t.Fatalf("승리 진영 = %s, want %s", g.Result.Winner, tc.wantWinner)
			}
			if fmt.Sprint(g.Result.WinnerSeats) != fmt.Sprint(tc.wantSeats) {
				t.Fatalf("승자 좌석 = %v, want %v", g.Result.WinnerSeats, tc.wantSeats)
			}
			if !hasHangul(g.Result.Message) {
				t.Fatalf("종료 문구 = %q", g.Result.Message)
			}
		})
	}
}

// TestBGCurrentPlayerDeathAdvances 차례 주인이 대응 중에 죽으면 판이 넘어간다
func TestBGCurrentPlayerDeathAdvances(t *testing.T) {
	g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade)
	bgStack(g, bgFillDeck(20)...)
	g.CurrentSeat = 1
	g.Players[1].HP = 1
	bgHand(g, 1, bgCard(BGDuel, BGSpade, "K"))
	bgHand(g, 2, bgCard(BGBang, BGClub, "5"))

	// 결투를 걸었는데 상대가 받아치고 내가 못 내면 내가 죽는다
	if err := g.Play(1, 0, bgSeatPtr(2), nil); err != nil {
		t.Fatalf("결투: %v", err)
	}
	if err := g.Respond(2, bgSeatPtr(0)); err != nil {
		t.Fatalf("받아치기: %v", err)
	}
	if g.Players[1].Alive {
		t.Fatal("결투를 건 쪽이 살아 있다")
	}
	if g.Phase == BGPhaseGameOver {
		return // 종료도 정상 결말이다
	}
	if g.CurrentSeat == 1 {
		t.Fatal("죽은 좌석에 차례가 머물러 있다")
	}
	if g.Pending != nil {
		t.Fatalf("대응 창이 남아 있다: %+v", g.Pending)
	}
}

// ==================== 잡화점 · 손패 줄이기 ====================

// TestBGStorePick 잡화점 — 인원수만큼 공개, 낸 사람부터 차례로 1장씩
func TestBGStorePick(t *testing.T) {
	g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade)
	bgHand(g, 0, bgCard(BGStore, BGSpade, "9"))
	bgStack(g, bgFillDeck(10)...)

	if err := g.Play(0, 0, nil, nil); err != nil {
		t.Fatalf("잡화점: %v", err)
	}
	if g.Phase != BGPhaseStorePick {
		t.Fatalf("phase = %s", g.Phase)
	}
	if len(g.StoreCards) != 4 {
		t.Fatalf("공개분 = %d장, want 4", len(g.StoreCards))
	}
	if g.Pending == nil || g.Pending.Kind != BGPendStore || g.Pending.Need != BGNeedPick {
		t.Fatalf("pending = %+v", g.Pending)
	}

	order := []int{0, 1, 2, 3}
	for i, seat := range order {
		if g.Pending.TargetSeat != seat {
			t.Fatalf("%d번째 고르는 좌석 = %d, want %d", i, g.Pending.TargetSeat, seat)
		}
		if err := g.Pick(seat, 0); err != nil {
			t.Fatalf("Pick(seat%d): %v", seat, err)
		}
	}
	if g.Phase != BGPhaseTurn {
		t.Fatalf("phase = %s — 다 고르면 차례로 돌아온다", g.Phase)
	}
	if len(g.StoreCards) != 0 {
		t.Fatalf("공개분이 남았다: %d장", len(g.StoreCards))
	}
	for _, p := range g.Players {
		want := 1
		if p.Seat == 0 {
			want = 1 // 잡화점 카드는 이미 냈다
		}
		if len(p.Hand) != want {
			t.Fatalf("seat%d 손패 = %d장, want %d", p.Seat, len(p.Hand), want)
		}
	}
}

// TestBGStoreSkipsDead 탈락자는 잡화점 순번에서 빠진다
func TestBGStoreSkipsDead(t *testing.T) {
	g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade)
	g.Players[1].Alive = false
	bgHand(g, 0, bgCard(BGStore, BGSpade, "9"))
	bgStack(g, bgFillDeck(10)...)
	g.Play(0, 0, nil, nil)

	if len(g.StoreCards) != 3 {
		t.Fatalf("공개분 = %d장, want 3 (생존자 수)", len(g.StoreCards))
	}
	for _, seat := range []int{0, 2, 3} {
		if g.Pending.TargetSeat != seat {
			t.Fatalf("고르는 좌석 = %d, want %d", g.Pending.TargetSeat, seat)
		}
		g.Pick(seat, 0)
	}
	if g.Phase != BGPhaseTurn {
		t.Fatalf("phase = %s", g.Phase)
	}
}

// TestBGDiscardPhase 손패를 체력 수만큼으로 줄인다
func TestBGDiscardPhase(t *testing.T) {
	g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade)
	g.Players[0].HP = 2
	bgHand(g, 0,
		bgCard(BGBang, BGSpade, "2"), bgCard(BGBeer, BGHeart, "3"),
		bgCard(BGMiss, BGClub, "4"), bgCard(BGDuel, BGDiamond, "5"))
	bgStack(g, bgFillDeck(10)...)

	if err := g.EndTurn(0); err != nil {
		t.Fatalf("EndTurn: %v", err)
	}
	if g.Phase != BGPhaseDiscard {
		t.Fatalf("phase = %s", g.Phase)
	}
	if err := g.DiscardCards(0, []int{0}); err == nil {
		t.Fatal("한 장만 버려도 통과했다")
	}
	if err := g.DiscardCards(0, []int{0, 1, 2}); err == nil {
		t.Fatal("세 장을 버려도 통과했다")
	}
	if err := g.DiscardCards(0, []int{0, 2}); err != nil {
		t.Fatalf("DiscardCards: %v", err)
	}
	if len(g.Players[0].Hand) != 2 {
		t.Fatalf("손패 = %d장, want 2", len(g.Players[0].Hand))
	}
	// 남은 두 장이 맥주·결투인가 (0번·2번을 버렸다)
	if g.Players[0].Hand[0].Kind != BGBeer || g.Players[0].Hand[1].Kind != BGDuel {
		t.Fatalf("남은 손패 = %+v", g.Players[0].Hand)
	}
	if g.CurrentSeat != 1 {
		t.Fatalf("차례 = seat%d", g.CurrentSeat)
	}
}

// TestBGEndTurnNoDiscard 손패가 체력 이하면 줄이기 없이 넘어간다
func TestBGEndTurnNoDiscard(t *testing.T) {
	g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade)
	bgHand(g, 0, bgCard(BGBang, BGSpade, "2"))
	bgStack(g, bgFillDeck(10)...)
	if err := g.EndTurn(0); err != nil {
		t.Fatalf("EndTurn: %v", err)
	}
	if g.Phase != BGPhaseTurn || g.CurrentSeat != 1 {
		t.Fatalf("phase=%s seat=%d", g.Phase, g.CurrentSeat)
	}
}

// ==================== 덱 되섞기 ====================

// TestBGReshuffle 덱이 마르면 버린 더미를 되섞는다
func TestBGReshuffle(t *testing.T) {
	g := bgTestGame(t, BGRoleSheriff, BGRoleOutlaw, BGRoleOutlaw, BGRoleRenegade)
	g.Deck = []BGCard{}
	g.DiscardPile = bgFillDeck(5)
	if n := g.drawTo(g.Players[0], 3); n != 3 {
		t.Fatalf("뽑은 장수 = %d", n)
	}
	if len(g.Deck) != 2 {
		t.Fatalf("덱 = %d장", len(g.Deck))
	}
	// 덱도 버린 더미도 비면 조용히 멈춘다 (패닉 없음)
	g.Deck, g.DiscardPile = []BGCard{}, []BGCard{}
	if n := g.drawTo(g.Players[0], 3); n != 0 {
		t.Fatalf("빈 덱에서 %d장을 뽑았다", n)
	}
}

// ==================== 은닉 (순수 스냅샷) ====================

// TestBGHiddenRoles 보안관은 시작부터 공개, 나머지는 사망 시 공개.
// yourRole·yourHand 는 본인 raw JSON 에만 실린다.
func TestBGHiddenRoles(t *testing.T) {
	h, room, _ := bgBotFixture(t, 5, 20240828)
	game := room.Game

	sheriff := -1
	for _, p := range game.Players {
		if p.Role == BGRoleSheriff {
			sheriff = p.Seat
		}
	}
	if sheriff < 0 {
		t.Fatal("보안관이 없다")
	}

	for _, viewer := range []int{0, 1, 2, 3, 4, -1} {
		raw := bgRawState(t, h, room, viewer)
		if viewer >= 0 {
			for _, key := range []string{`"yourRole"`, `"yourHand"`, `"yourBangUsed"`} {
				if strings.Count(raw, key) != 1 {
					t.Fatalf("본인 스냅샷의 %s 개수 = %d:\n%s",
						key, strings.Count(raw, key), raw)
				}
			}
		} else {
			for _, key := range []string{`"yourRole"`, `"yourHand"`, `"yourBangUsed"`} {
				if strings.Contains(raw, key) {
					t.Fatalf("관전자 스냅샷에 %s 유출:\n%s", key, raw)
				}
			}
		}

		state := h.buildBGState(room, viewer)
		revealed := 0
		for _, pv := range state.Players {
			if pv.Role == BGRoleNone {
				continue
			}
			revealed++
			if pv.Seat != sheriff {
				t.Fatalf("보안관 아닌 seat%d 의 역할이 공개됐다: %s", pv.Seat, pv.Role)
			}
			if pv.Role != BGRoleSheriff {
				t.Fatalf("보안관 좌석의 role = %s", pv.Role)
			}
		}
		if revealed != 1 {
			t.Fatalf("공개된 역할 수 = %d, want 1 (보안관만)", revealed)
		}
	}

	// 죽으면 그 좌석의 역할이 공개된다
	victim := (sheriff + 1) % len(game.Players)
	game.Players[victim].HP = 1
	game.damage(victim, 1, -1)
	for _, viewer := range []int{0, -1} {
		for _, pv := range h.buildBGState(room, viewer).Players {
			if pv.Seat == victim && pv.Role == BGRoleNone {
				t.Fatalf("사망자 seat%d 의 역할이 아직 비공개다", victim)
			}
		}
	}
}

// TestBGSnapshotShape 빈 슬라이스는 [] · 좌석 필드는 생략되지 않는다 ·
// 관전자(viewerSeat -1) 스냅샷도 패닉 없이 만들어진다
func TestBGSnapshotShape(t *testing.T) {
	h, room, _ := bgBotFixture(t, 5, 99)

	for _, viewer := range []int{0, -1} {
		raw := bgRawState(t, h, room, viewer)
		if strings.Contains(raw, "null,") && strings.Contains(raw, `"storeCards":null`) {
			t.Fatalf("빈 슬라이스가 null 로 나갔다:\n%s", raw)
		}
		for _, key := range []string{`"storeCards":[`, `"players":[`, `"equipment":[`} {
			if !strings.Contains(raw, key) {
				t.Fatalf("%s 가 [] 로 나가지 않았다:\n%s", key, raw)
			}
		}
		for _, key := range []string{`"seat":`, `"hp":`, `"maxHp":`, `"handCount":`,
			`"distanceFromYou":`, `"currentSeat":`, `"yourSeat":`, `"hostSeat":`} {
			if !strings.Contains(raw, key) {
				t.Fatalf("%s 키가 빠졌다 (omitempty 금지):\n%s", key, raw)
			}
		}
	}

	// 관전자는 거리를 알 수 없다
	for _, pv := range h.buildBGState(room, -1).Players {
		if pv.DistanceFromYou != -1 {
			t.Fatalf("관전자에게 거리 %d 가 노출됐다", pv.DistanceFromYou)
		}
	}
	// 본인 좌석의 거리는 0, 남은 1 이상
	me := h.buildBGState(room, 0)
	for _, pv := range me.Players {
		if pv.Seat == 0 && pv.DistanceFromYou != 0 {
			t.Fatalf("내 거리 = %d, want 0", pv.DistanceFromYou)
		}
		if pv.Seat != 0 && pv.DistanceFromYou < 1 {
			t.Fatalf("seat%d 거리 = %d", pv.Seat, pv.DistanceFromYou)
		}
	}
}

// bgRawState viewerSeat 관점의 raw JSON 스냅샷
func bgRawState(t *testing.T, h *BGHub, room *bgRoom, viewer int) string {
	t.Helper()
	data, err := json.Marshal(h.buildBGState(room, viewer))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(data)
}

// bgBotFixture 허브 고루틴 없이 n인 판을 시작시킨다 (결정적 시드)
func bgBotFixture(t *testing.T, n int, seed int64) (*BGHub, *bgRoom, []*BGClient) {
	t.Helper()
	h := NewBGHub()
	h.rng = rand.New(rand.NewSource(seed))
	room := h.lobbyRoomFor("")
	clients := make([]*BGClient, n)
	for i := range clients {
		c := &BGClient{wsClient: newBotWSClient(), Hub: h}
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

// ==================== 봇 품질 측정 ====================

// bgBotSimResult 5봇 대전의 집계
type bgBotSimResult struct {
	Games      int
	Wins       map[string]int
	TotalTurns int
	MaxTurns   int
	Forced     int

	// 조준 계측 — 역할별 목표를 실제로 노렸는가
	OutlawAims        int
	OutlawAtSheriff   int
	SheriffAims       int
	SheriffAtEnemy    int
	DeputyAims        int
	DeputyAtSheriff   int
	DeputyAtEnemy     int
	RenegadeAims      int
	RenegadeAtOutlaw  int
	RenegadeAtSheriff int
}

// bgActingSeat 지금 응답해야 하는 좌석 (-1 없음)
func bgActingSeat(g *BGGame) int {
	switch g.Phase {
	case BGPhaseTurn, BGPhaseDiscard:
		return g.CurrentSeat
	case BGPhaseRespond, BGPhaseStorePick:
		if g.Pending == nil {
			return -1
		}
		return g.Pending.TargetSeat
	}
	return -1
}

// bgForcePhase 단계별 강제 진행 (봇이 막혔을 때의 방어선)
func bgForcePhase(g *BGGame) {
	switch g.Phase {
	case BGPhaseTurn:
		g.ForceTurn()
	case BGPhaseRespond:
		g.ForceRespond()
	case BGPhaseStorePick:
		g.ForcePick()
	case BGPhaseDiscard:
		g.ForceDiscard()
	}
}

// bgDigest 판이 실제로 움직였는지 판단하는 지문
func bgDigest(g *BGGame) string {
	s := fmt.Sprintf("%s|%d|%d|%d|%d", g.Phase, g.CurrentSeat, g.Turns,
		len(g.Deck), len(g.DiscardPile))
	for _, p := range g.Players {
		s += fmt.Sprintf(",%v:%d:%d:%d", p.Alive, p.HP, len(p.Hand), len(p.Equipment))
	}
	if g.Pending != nil {
		s += fmt.Sprintf("|%s:%d:%d", g.Pending.Kind, g.Pending.TargetSeat,
			len(g.Pending.Passed))
	}
	return s
}

// bgRunBotSeries 5봇 게임을 games 판 돌려 진영별 승률·평균 차례·조준 정확도를
// 잰다. 실제 허브 경로(handleGameMessage → 순수 규칙 → buildBGState)를 그대로
// 쓰고 봇 두뇌도 실물이다 — 연결만 없어서 전송이 버려질 뿐이다.
func bgRunBotSeries(t *testing.T, games int) bgBotSimResult {
	t.Helper()
	// 마감 타이머가 끼어들지 않게 아주 길게 잡는다 (허브 고루틴 없이 돈다)
	saved := [4]time.Duration{bgTurnTimeout, bgRespondTimeout, bgStoreTimeout, bgDiscardTimeout}
	bgTurnTimeout, bgRespondTimeout = time.Hour, time.Hour
	bgStoreTimeout, bgDiscardTimeout = time.Hour, time.Hour
	defer func() {
		bgTurnTimeout, bgRespondTimeout = saved[0], saved[1]
		bgStoreTimeout, bgDiscardTimeout = saved[2], saved[3]
	}()

	out := bgBotSimResult{Games: games, Wins: map[string]int{}}
	for gameNo := 0; gameNo < games; gameNo++ {
		h := NewBGHub()
		h.rng = rand.New(rand.NewSource(int64(gameNo)*7919 + 101))
		room := h.lobbyRoomFor("")

		clients := make([]*BGClient, BGFillBotTarget)
		brains := make([]*bgBrain, BGFillBotTarget)
		for i := range clients {
			c := &BGClient{wsClient: newBotWSClient(), Hub: h}
			c.Connected = false // 소켓 없이 직접 구동 — 전송은 sendTo 에서 버려진다
			c.Name = fmt.Sprintf("%s%d", botName, i+1)
			seat, err := room.Game.AddPlayer(c.Name)
			if err != nil {
				t.Fatalf("AddPlayer: %v", err)
			}
			c.GameID, c.Seat = room.Game.ID, seat
			room.Clients[seat] = c
			h.sessions[c.SessionID] = c
			clients[i] = c
			b := newBGBrain()
			b.rng = rand.New(rand.NewSource(int64(gameNo*100 + i)))
			brains[i] = b
		}
		h.startGame(room)
		game := room.Game

		guard := 0
		for game.Phase != BGPhaseGameOver && guard < 6000 {
			h.stopPhaseTimer(room) // 1시간짜리 타이머가 쌓이지 않게
			seat := bgActingSeat(game)
			if seat < 0 {
				bgForcePhase(game)
				guard++
				continue
			}
			before := bgDigest(game)
			state, ok := botPayloadAs[bgBotState](h.buildBGState(room, seat))
			if !ok {
				t.Fatal("봇 스냅샷 변환 실패")
			}
			if msg := brains[seat].decideState(state); msg != nil {
				h.handleGameMessage(BGGameMessage{Client: clients[seat], Message: *msg})
			}
			if game.Phase != BGPhaseGameOver && bgDigest(game) == before {
				out.Forced++
				bgForcePhase(game)
			}
			guard++
		}
		h.stopPhaseTimer(room)
		if game.Phase != BGPhaseGameOver || game.Result == nil {
			t.Fatalf("%d번째 판이 %d걸음 안에 끝나지 않았다 (phase=%s)",
				gameNo, guard, game.Phase)
		}

		out.Wins[game.Result.Winner]++
		out.TotalTurns += game.Turns
		if game.Turns > out.MaxTurns {
			out.MaxTurns = game.Turns
		}

		// 조준 계측 — 실제 역할과 대조한다
		roleOf := map[int]BGRole{}
		for _, p := range game.Players {
			roleOf[p.Seat] = p.Role
		}
		for _, b := range brains {
			for _, aim := range b.Aims {
				target := roleOf[aim.To]
				switch aim.Role {
				case BGRoleOutlaw:
					out.OutlawAims++
					if target == BGRoleSheriff {
						out.OutlawAtSheriff++
					}
				case BGRoleSheriff:
					out.SheriffAims++
					if target == BGRoleOutlaw || target == BGRoleRenegade {
						out.SheriffAtEnemy++
					}
				case BGRoleDeputy:
					out.DeputyAims++
					if target == BGRoleSheriff {
						out.DeputyAtSheriff++
					}
					if target == BGRoleOutlaw || target == BGRoleRenegade {
						out.DeputyAtEnemy++
					}
				case BGRoleRenegade:
					out.RenegadeAims++
					if target == BGRoleOutlaw {
						out.RenegadeAtOutlaw++
					}
					if target == BGRoleSheriff {
						out.RenegadeAtSheriff++
					}
				}
			}
		}
	}
	return out
}

// bgPct 백분율 (분모 0이면 0)
func bgPct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b) * 100
}

// TestBGBotBalance 5봇 30판의 진영별 승률·평균 차례와 역할별 조준 정확도.
// 한 진영이 90% 이상 이기면 봇 정책이나 규칙이 무너진 것이므로 실패시킨다.
func TestBGBotBalance(t *testing.T) {
	bgSilenceBotDelay(t)
	const games = 30
	res := bgRunBotSeries(t, games)

	sheriff := bgPct(res.Wins["sheriff"], res.Games)
	outlaw := bgPct(res.Wins["outlaw"], res.Games)
	renegade := bgPct(res.Wins["renegade"], res.Games)
	avgTurns := float64(res.TotalTurns) / float64(res.Games)

	t.Logf("5봇 %d판 — 보안관 진영 %d승(%.1f%%) · 무법자 %d승(%.1f%%) · 배신자 %d승(%.1f%%)",
		res.Games, res.Wins["sheriff"], sheriff, res.Wins["outlaw"], outlaw,
		res.Wins["renegade"], renegade)
	t.Logf("평균 %.1f차례 (최대 %d) | 강제 진행 %d회", avgTurns, res.MaxTurns, res.Forced)
	t.Logf("조준 — 무법자→보안관 %d/%d(%.1f%%) · 보안관→적 %d/%d(%.1f%%) · "+
		"부관→적 %d/%d(%.1f%%, 보안관 오사 %d) · 배신자→무법자 %d/%d(%.1f%%, 보안관 %d)",
		res.OutlawAtSheriff, res.OutlawAims, bgPct(res.OutlawAtSheriff, res.OutlawAims),
		res.SheriffAtEnemy, res.SheriffAims, bgPct(res.SheriffAtEnemy, res.SheriffAims),
		res.DeputyAtEnemy, res.DeputyAims, bgPct(res.DeputyAtEnemy, res.DeputyAims),
		res.DeputyAtSheriff,
		res.RenegadeAtOutlaw, res.RenegadeAims, bgPct(res.RenegadeAtOutlaw, res.RenegadeAims),
		res.RenegadeAtSheriff)

	for _, f := range []struct {
		name string
		rate float64
	}{{"보안관 진영", sheriff}, {"무법자", outlaw}, {"배신자", renegade}} {
		if f.rate >= 90 {
			t.Fatalf("%s가 압도한다 — %.1f%%", f.name, f.rate)
		}
	}
	if avgTurns < 5 {
		t.Fatalf("평균 %.1f차례는 너무 짧다 — 진행이 망가졌을 수 있다", avgTurns)
	}
	if res.Forced > res.Games*3 {
		t.Fatalf("봇이 판을 못 밀어 %d번 강제 진행했다 (판당 3회 초과)", res.Forced)
	}

	// 역할별 목표를 실제로 노리는가
	if res.OutlawAims > 0 && bgPct(res.OutlawAtSheriff, res.OutlawAims) < 50 {
		t.Fatalf("무법자 봇이 보안관을 안 노린다 — %.1f%%",
			bgPct(res.OutlawAtSheriff, res.OutlawAims))
	}
	if res.DeputyAtSheriff > 0 {
		t.Fatalf("부관 봇이 보안관을 %d번 조준했다", res.DeputyAtSheriff)
	}
	if res.SheriffAims > 0 && bgPct(res.SheriffAtEnemy, res.SheriffAims) < 50 {
		t.Fatalf("보안관 봇이 적을 못 고른다 — %.1f%%",
			bgPct(res.SheriffAtEnemy, res.SheriffAims))
	}
}

// bgSilenceBotDelay 봇의 "생각하는 시간"을 끈다 (지연은 순수 연출)
func bgSilenceBotDelay(t *testing.T) {
	t.Helper()
	delay, jitter := bgBotDelay, bgBotJitterMs
	bgBotDelay, bgBotJitterMs = 0, 0
	t.Cleanup(func() { bgBotDelay, bgBotJitterMs = delay, jitter })
}
