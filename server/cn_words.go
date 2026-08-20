package server

import "math/rand"

// ==================== 코드네임 단어 풀 ====================
//
// 스파이폴 카테고리 자산(sp_types.go 의 spCategories)을 그대로 재사용한다 —
// 같은 패키지라 export 승격 없이 읽기만 한다 (sp 동작 불변). 9개 카테고리
// 216개 단어(장소·직업·과일·음식·동물·스포츠·영화·나라·브랜드)에서 중복 없이
// 25개를 무작위 추출하므로 매 판 여러 카테고리가 자연히 섞인다.

// cnWordPool 전체 후보 단어 (중복 제거, 결정적 순서 — 셔플은 호출부의 rng).
// map 순회의 비결정성을 피하려고 spCategoryNames 순서로 모은다.
func cnWordPool() []string {
	seen := map[string]bool{}
	pool := []string{}
	for _, category := range spCategoryNames {
		for _, w := range spCategories[category] {
			if seen[w] {
				continue
			}
			seen[w] = true
			pool = append(pool, w)
		}
	}
	return pool
}

// cnPickWords 보드용 25개 단어를 중복 없이 무작위 추출한다
func cnPickWords(rng *rand.Rand) []string {
	pool := cnWordPool()
	rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	return append([]string{}, pool[:CNBoardSize]...)
}

// cnDealKeyCard 키 카드 25칸 — 적 9(선공), 청 8, 중립 7, 암살자 1 을 섞는다
func cnDealKeyCard(rng *rand.Rand) []CNColor {
	key := []CNColor{}
	for i := 0; i < CNRedWords; i++ {
		key = append(key, CNColorRed)
	}
	for i := 0; i < CNBlueWords; i++ {
		key = append(key, CNColorBlue)
	}
	for i := 0; i < CNNeutralWords; i++ {
		key = append(key, CNColorNeutral)
	}
	key = append(key, CNColorAssassin)
	rng.Shuffle(len(key), func(i, j int) { key[i], key[j] = key[j], key[i] })
	return key
}
