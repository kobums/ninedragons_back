package server

// ==================== 마이티 연습봇 ====================
//
// 판단은 전부 mt_game.go 의 순수 함수(mtLegalPlays·mtTrickWinner·
// mtBidBeats)를 공유한다 — 봇이 서버 검증에 걸리는 수를 내지 않는 근거.

// mtBotState 봇이 쓰는 최소 스냅샷
type mtBotState struct {
	Phase    string   `json:"phase"`
	YourSeat int      `json:"yourSeat"`
	YourHand []string `json:"yourHand"`
	Bidding  struct {
		Turn int        `json:"turn"`
		Best *MTBidView `json:"best"`
	} `json:"bidding"`
	Contract        *MTContractView `json:"contract"`
	TrickNo         int             `json:"trickNo"`
	CurrentTurn     int             `json:"currentTurn"`
	LedSuit         string          `json:"ledSuit"`
	JokerCallActive bool            `json:"jokerCallActive"`
	Trick           []MTTrickPlay   `json:"trick"`
}

// mtBrain 마이티 봇 두뇌. 재배분 횟수는 redeal 이벤트를 세어 추적한다.
type mtBrain struct {
	redeals   int
	didKitty  bool
	didFriend bool
}

func newMTBrain() *mtBrain { return &mtBrain{} }

func (b *mtBrain) decide(msg MTMessage) *MTMessage {
	switch msg.Type {
	case MTMsgEvent:
		if ev, ok := botPayloadAs[MTEventPayload](msg.Payload); ok && ev.Kind == "redeal" {
			b.redeals++
		}
		return nil
	case MTMsgGameState:
		state, ok := botPayloadAs[mtBotState](msg.Payload)
		if !ok {
			return nil
		}
		return b.act(state)
	}
	return nil
}

func (b *mtBrain) act(state mtBotState) *MTMessage {
	switch state.Phase {
	case string(MTPhaseBidding):
		if state.Bidding.Turn == state.YourSeat {
			return b.decideBid(state)
		}
	case string(MTPhaseKitty):
		if state.Contract != nil && state.Contract.Declarer == state.YourSeat && !b.didKitty {
			b.didKitty = true
			return b.decideKitty(state)
		}
	case string(MTPhaseFriend):
		if state.Contract != nil && state.Contract.Declarer == state.YourSeat && !b.didFriend {
			b.didFriend = true
			return b.decideFriend(state)
		}
	case string(MTPhasePlay):
		if state.CurrentTurn == state.YourSeat {
			return b.decidePlay(state)
		}
	}
	return nil
}

// ==================== 입찰 ====================

// mtLongestSuit 손패의 최장 문양과 길이 (동률이면 S·D·C·H 순서 우선)
func mtLongestSuit(hand []string) (string, int) {
	counts := map[string]int{}
	for _, c := range hand {
		suit, _, ok := mtParseCard(c)
		if ok {
			counts[suit]++
		}
	}
	best, bestLen := "S", 0
	for _, s := range mtSuits {
		if counts[s] > bestLen {
			best, bestLen = s, counts[s]
		}
	}
	return best, bestLen
}

// decideBid 마이티·조커·최장문양 길이를 점수화해 강하면 최소 상회 입찰, 아니면 패스.
// 전원 패스 방지: 재배분 2회째부터는 최고 공약이 없으면 최장문양 13 을 강제 입찰한다
// (봇들은 서로 손패를 모르므로 "가장 강한 봇"은 선순위 봇이 대행한다 — 같은 규칙이
// 모든 봇에 걸려 있어 첫 순번 봇의 13이 서고 나머지는 상회하지 못해 패스한다).
func (b *mtBrain) decideBid(state mtBotState) *MTMessage {
	suit, longest := mtLongestSuit(state.YourHand)
	best := state.Bidding.Best

	// 재배분 무한루프 차단용 강제 입찰
	if b.redeals >= 2 && best == nil {
		return &MTMessage{Type: MTMsgBid, Payload: MTBidPayload{Suit: suit, Count: MTMinBid}}
	}

	strength := longest
	if mtHandContains(state.YourHand, "S14") {
		strength += 3
	}
	if mtHandContains(state.YourHand, MTJoker) {
		strength += 2
	}

	count := MTMinBid
	if best != nil {
		count = best.Count + 1
	}
	// 강한 손이면서 무리하지 않는 선(14)까지만 상회 입찰
	if strength >= 8 && count <= 14 {
		return &MTMessage{Type: MTMsgBid, Payload: MTBidPayload{Suit: suit, Count: count}}
	}
	return &MTMessage{Type: MTMsgPass, Payload: map[string]interface{}{}}
}

// ==================== 키티 ====================

// decideKitty 최장문양을 기루다로 유지하고, 비점수 저카드 3장을 버린다.
// 마이티·조커·기루다·점수 카드는 최대한 남긴다.
func (b *mtBrain) decideKitty(state mtBotState) *MTMessage {
	suit, _ := mtLongestSuit(state.YourHand)
	mighty := mtMightyCard(suit)

	type cand struct {
		card string
		key  int
	}
	cands := []cand{}
	for _, c := range state.YourHand {
		if c == MTJoker || c == mighty {
			continue
		}
		cardSuit, rank, _ := mtParseCard(c)
		key := rank
		if cardSuit == suit {
			key += 100 // 기루다는 마지막에
		}
		if mtIsScoreCard(c) {
			key += 50 // 점수 카드도 가급적 남긴다
		}
		cands = append(cands, cand{card: c, key: key})
	}
	// 단순 선택 정렬 (13장 이내라 충분)
	for i := 0; i < len(cands); i++ {
		for j := i + 1; j < len(cands); j++ {
			if cands[j].key < cands[i].key {
				cands[i], cands[j] = cands[j], cands[i]
			}
		}
	}
	discard := []string{}
	for i := 0; i < MTKittySize && i < len(cands); i++ {
		discard = append(discard, cands[i].card)
	}
	return &MTMessage{Type: MTMsgKitty, Payload: MTKittyPayload{Discard: discard, Suit: suit}}
}

// ==================== 프렌드 ====================

// decideFriend 마이티 미소지 시 마이티 카드콜, 소지 시 기루다A, 둘 다 있으면 첫 트릭
func (b *mtBrain) decideFriend(state mtBotState) *MTMessage {
	trump := state.Contract.Suit
	mighty := mtMightyCard(trump)
	if !mtHandContains(state.YourHand, mighty) {
		return &MTMessage{Type: MTMsgFriend, Payload: MTFriendPayload{Type: "card", Card: mighty}}
	}
	if trump != "N" {
		trumpAce := trump + "14"
		if !mtHandContains(state.YourHand, trumpAce) {
			return &MTMessage{Type: MTMsgFriend, Payload: MTFriendPayload{Type: "card", Card: trumpAce}}
		}
	}
	return &MTMessage{Type: MTMsgFriend, Payload: MTFriendPayload{Type: "first_trick"}}
}

// ==================== 플레이 ====================

// mtCardKey 카드의 대략적인 세기 (낮을수록 약함) — "최소 승리 카드" 선택용
func mtCardKey(card, trump string) int {
	if card == mtMightyCard(trump) {
		return 200
	}
	if card == MTJoker {
		return 150
	}
	suit, rank, _ := mtParseCard(card)
	if trump != "N" && suit == trump {
		return 100 + rank
	}
	return rank
}

// decidePlay 팔로우 의무 준수. 이길 수 있으면 최소 승리 카드, 못 이기면 최저 카드.
// 조커콜 준수(합법 수 계산이 강제). 주공 봇은 리드에서 기루다 최고를 선호.
func (b *mtBrain) decidePlay(state mtBotState) *MTMessage {
	trump := "N"
	isDeclarer := false
	if state.Contract != nil {
		trump = state.Contract.Suit
		isDeclarer = state.Contract.Declarer == state.YourSeat
	}
	isLead := len(state.Trick) == 0
	legal := mtLegalPlays(state.YourHand, isLead, state.LedSuit, state.JokerCallActive, trump)
	if len(legal) == 0 {
		return nil
	}

	sortByKey := func(cards []string) {
		for i := 0; i < len(cards); i++ {
			for j := i + 1; j < len(cards); j++ {
				if mtCardKey(cards[j], trump) < mtCardKey(cards[i], trump) {
					cards[i], cards[j] = cards[j], cards[i]
				}
			}
		}
	}

	if isLead {
		// 조커 리드는 마지막 수단 (문양 지정 필요)
		nonJoker := []string{}
		for _, c := range legal {
			if c != MTJoker {
				nonJoker = append(nonJoker, c)
			}
		}
		if len(nonJoker) == 0 {
			suit, _ := mtLongestSuit(state.YourHand)
			if suit == "" {
				suit = "S"
			}
			return &MTMessage{Type: MTMsgPlay, Payload: MTPlayPayload{Card: MTJoker, JokerSuit: suit}}
		}
		sortByKey(nonJoker)
		pick := nonJoker[len(nonJoker)-1] // 가장 센 카드로 리드
		if isDeclarer && trump != "N" {
			// 주공은 기루다 리드 선호 — 기루다 중 최고
			for i := len(nonJoker) - 1; i >= 0; i-- {
				suit, _, _ := mtParseCard(nonJoker[i])
				if suit == trump {
					pick = nonJoker[i]
					break
				}
			}
		}
		return &MTMessage{Type: MTMsgPlay, Payload: MTPlayPayload{Card: pick}}
	}

	// 팔로우: 현재까지의 트릭을 이길 수 있는 최소 카드를 찾는다
	sorted := append([]string{}, legal...)
	sortByKey(sorted)
	myIdx := len(state.Trick)
	for _, c := range sorted {
		trial := append(append([]MTTrickPlay{}, state.Trick...), MTTrickPlay{Seat: state.YourSeat, Card: c})
		if mtTrickWinner(trial, state.LedSuit, trump, state.TrickNo, state.JokerCallActive) == myIdx {
			return &MTMessage{Type: MTMsgPlay, Payload: MTPlayPayload{Card: c}}
		}
	}
	// 못 이기면 최저 카드
	return &MTMessage{Type: MTMsgPlay, Payload: MTPlayPayload{Card: sorted[0]}}
}

// ==================== 봇 소환 ====================

// spawnBot 대기 방의 남은 자리에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *MTHub) spawnBot(room *mtRoom, name string) *MTClient {
	bot := &MTClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return nil
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot

	h.runMTBot(bot)
	return bot
}

// takeoverBot 유예 만료 좌석을 이어받는 연습봇 — 이름·좌석을 유지해
// 진행 중인 공약·프렌드 참조가 그대로 성립한다
func (h *MTHub) takeoverBot(room *mtRoom, seat int, name string) *MTClient {
	bot := &MTClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	bot.GameID = room.Game.ID
	bot.Seat = seat
	h.runMTBot(bot)
	return bot
}

// runMTBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *MTHub) runMTBot(bot *MTClient) {
	brain := newMTBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m MTMessage) { h.gameMessage <- MTGameMessage{Client: bot, Message: m} },
		func(m MTMessage) bool { return m.Type == MTMsgGameOver || m.Type == MTMsgSessionExpired })
}
