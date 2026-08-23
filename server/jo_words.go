package server

import "math/rand"

// ==================== 저스트 원 제시어 풀 ====================
//
// 제시어는 코드네임과 같은 자산을 쓴다 — cnWordPool() 이 모아 주는 스파이폴
// 9카테고리 216단어다. cn_words.go / sp_types.go 는 건드리지 않고 호출·읽기만
// 한다 (같은 패키지라 export 승격이 필요 없다).
//
// 저스트 원에만 필요한 건 "카테고리 조회"다. 연습봇이 제시어와 같은 카테고리의
// 다른 단어를 골라야 그럴듯한 연상 단서가 되기 때문이다. 216단어 선형 탐색은
// 무시할 수 있는 비용이라 공유 색인(초기화 경합거리)을 두지 않는다.

// joWordPool 제시어 후보 전체 (결정적 순서 — 셔플은 호출부의 rng)
func joWordPool() []string {
	return cnWordPool()
}

// joPickWords 라운드 수만큼 제시어를 중복 없이 뽑는다.
// 풀(216단어)이 최대 라운드(7인 14라운드)보다 훨씬 크므로 부족할 일은 없지만,
// 방어적으로 부족하면 앞에서부터 다시 채운다.
func joPickWords(rng *rand.Rand, count int) []string {
	pool := joWordPool()
	if count <= 0 || len(pool) == 0 {
		return []string{}
	}
	rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })

	words := make([]string, 0, count)
	for i := 0; i < count; i++ {
		words = append(words, pool[i%len(pool)])
	}
	return words
}

// joCategoryOf 단어가 속한 스파이폴 카테고리 이름 (못 찾으면 "").
// spCategoryNames 순서로 훑어 map 순회의 비결정성을 피한다.
func joCategoryOf(word string) string {
	for _, category := range spCategoryNames {
		for _, w := range spCategories[category] {
			if w == word {
				return category
			}
		}
	}
	return ""
}

// joCategoryWords 카테고리의 단어 사본 (없는 카테고리는 빈 배열)
func joCategoryWords(category string) []string {
	return append([]string{}, spCategories[category]...)
}

// joRelatedWord word 와 같은 카테고리의 '다른' 단어 하나를 고른다.
// 같은 카테고리를 못 찾으면 전체 풀에서 무작위로 고른다.
//
// 소거 규칙에 바로 걸리는 후보(제시어와 같거나 포함 관계인 단어)는 미리
// 걸러낸다 — 연습봇이 스스로 지워질 단서를 내지 않게 하는 장치다.
// 고를 후보가 하나도 없으면 "" 를 돌려준다.
func joRelatedWord(rng *rand.Rand, word string) string {
	candidates := pickJOCandidates(joCategoryWords(joCategoryOf(word)), word)
	if len(candidates) == 0 {
		candidates = pickJOCandidates(joWordPool(), word)
	}
	if len(candidates) == 0 {
		return ""
	}
	return candidates[rng.Intn(len(candidates))]
}

// pickJOCandidates 제시어와 소거 관계가 없는 후보만 남긴다
func pickJOCandidates(words []string, word string) []string {
	out := []string{}
	for _, w := range words {
		if joClueKilledByWord(word, w) {
			continue
		}
		out = append(out, w)
	}
	return out
}
