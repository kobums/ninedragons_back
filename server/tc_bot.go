package server

// ==================== 티츄 연습봇 ====================
//
// 스냅샷(tc_game_state)만 보고 반응한다. 합법 수 계산은 서버 검증과 같은
// tc_game.go 의 열거 로직을 재사용하므로 봇의 수는 항상 검증을 통과한다.
// 휴리스틱: 라지티츄·스몰티츄 선언 안 함, 교환은 낮은 카드 3장, 리드는 긴 콤보
// 우선, 따라갈 땐 가능한 낮은 콤보(폭탄은 소원 강제일 때만), 소원 없음,
// 용 트릭은 다음 상대(좌석+1)에게.

// tcBotState 봇이 쓰는 최소 스냅샷
type tcBotState struct {
	Phase             string       `json:"phase"`
	YourSeat          int          `json:"yourSeat"`
	YourHand          []string     `json:"yourHand"`
	GrandAnswered     bool         `json:"grandAnswered"`
	ExchangeDone      bool         `json:"exchangeDone"`
	CurrentTurn       int          `json:"currentTurn"`
	WishRank          int          `json:"wishRank"`
	DragonPendingSeat int          `json:"dragonPendingSeat"`
	LastPlay          *TCTrickPlay `json:"lastPlay"`
	Players           []struct {
		Seat  int  `json:"seat"`
		Out   bool `json:"out"`
		Ready bool `json:"ready"`
	} `json:"players"`
}

type tcBrain struct{}

func newTCBrain() *tcBrain { return &tcBrain{} }

func (b *tcBrain) decide(msg TCMessage) *TCMessage {
	if msg.Type != TCMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[tcBotState](msg.Payload)
	if !ok {
		return nil
	}
	me := state.YourSeat

	switch state.Phase {
	case TCPhaseGrand:
		if !state.GrandAnswered {
			return &TCMessage{Type: TCMsgCallGrand, Payload: TCCallGrandPayload{Call: false}}
		}

	case TCPhaseExchange:
		if !state.ExchangeDone && len(state.YourHand) >= 3 {
			low := b.lowestCards(state.YourHand, 3)
			return &TCMessage{Type: TCMsgExchange, Payload: TCExchangePayload{
				Left: low[0], Partner: low[1], Right: low[2],
			}}
		}

	case TCPhasePlay:
		if state.DragonPendingSeat == me {
			return &TCMessage{Type: TCMsgDragonGive,
				Payload: TCDragonGivePayload{Seat: (me + 1) % TCSeats}}
		}
		if state.CurrentTurn == me && len(state.YourHand) > 0 {
			return b.decidePlay(state)
		}

	case TCPhaseHandEnd:
		for _, p := range state.Players {
			if p.Seat == me && !p.Ready {
				return &TCMessage{Type: TCMsgReady}
			}
		}
	}
	return nil
}

// lowestCards 봇 기준 가치가 낮은 n장 (개·참새 → 낮은 숫자 → 봉황·용 순)
func (b *tcBrain) lowestCards(hand []string, n int) []string {
	sorted := tcSortHand(hand)
	value := func(c string) int {
		switch c {
		case tcDog:
			return 0
		case tcMah:
			return 1
		case tcPhx:
			return 20
		case tcDrg:
			return 21
		}
		return tcCardRank(c)
	}
	out := append([]string(nil), sorted...)
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if value(out[j]) < value(out[i]) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out[:n]
}

// decidePlay 현재 콤보를 열거 로직으로 재구성해 낼 수를 고른다
func (b *tcBrain) decidePlay(state tcBotState) *TCMessage {
	prev := tcComboFromTrickPlay(state.LastPlay)
	plays, forced := tcEnumerate(state.YourHand, prev, state.WishRank)
	if len(plays) == 0 {
		return &TCMessage{Type: TCMsgPass}
	}

	if prev == nil {
		// 리드: 폭탄이 아닌 것 중 긴 콤보 우선, 같으면 낮은 값
		best := b.pickLead(plays, prev)
		return &TCMessage{Type: TCMsgPlay, Payload: TCPlayPayload{Cards: best}}
	}

	// 따라가기: 폭탄이 아닌 것 중 가장 낮은 콤보. 폭탄뿐이면
	// 소원 강제일 때만 낸다 (아니면 패스로 아낀다).
	var best []string
	bestValue := 1 << 30
	bombOnly := true
	for _, p := range plays {
		combo, err := tcParseCombo(p, prev)
		if err != nil {
			continue
		}
		if tcIsBombKind(combo.Kind) {
			continue
		}
		bombOnly = false
		if combo.Value < bestValue {
			bestValue = combo.Value
			best = p
		}
	}
	if best == nil {
		if bombOnly && !forced {
			return &TCMessage{Type: TCMsgPass}
		}
		best = plays[0]
	}
	return &TCMessage{Type: TCMsgPlay, Payload: TCPlayPayload{Cards: best}}
}

// pickLead 리드 콤보 선택: 폭탄 제외 장수 우선, 같으면 낮은 값
func (b *tcBrain) pickLead(plays [][]string, prev *tcCombo) []string {
	var best []string
	bestLen, bestValue := -1, 1<<30
	for _, p := range plays {
		combo, err := tcParseCombo(p, prev)
		if err != nil {
			continue
		}
		if tcIsBombKind(combo.Kind) {
			continue
		}
		if len(p) > bestLen || (len(p) == bestLen && combo.Value < bestValue) {
			best = p
			bestLen = len(p)
			bestValue = combo.Value
		}
	}
	if best == nil {
		best = plays[0] // 폭탄뿐이면 폭탄 리드
	}
	return best
}

// tcComboFromTrickPlay 와이어 lastPlay 를 내부 콤보로 복원한다.
// 봉황 싱글의 강도는 스냅샷의 phx 부가 필드로 정확히 복원된다.
func tcComboFromTrickPlay(p *TCTrickPlay) *tcCombo {
	if p == nil {
		return nil
	}
	combo := &tcCombo{Kind: p.Combo, Cards: p.Cards, Len: len(p.Cards)}
	switch p.Combo {
	case tcSingle:
		c := p.Cards[0]
		switch c {
		case tcDrg:
			combo.Value = 30
		case tcPhx:
			combo.Value = p.Phx
		default:
			combo.Value = tcCardRank(c) * 2
		}
	case tcPair, tcTriple, tcBomb:
		combo.Value = tcCardRank(tcFirstNatural(p.Cards)) * 2
	case tcFullhouse:
		counts := map[int]int{}
		phx := false
		for _, c := range p.Cards {
			if c == tcPhx {
				phx = true
				continue
			}
			counts[tcCardRank(c)]++
		}
		if triple, ok := tcFullhouseTriple(counts, phx); ok {
			combo.Value = triple * 2
		}
	case tcStraight, tcBombSF:
		combo.Value = tcPlayStraightTop(p.Cards) * 2
	case tcPairSeq:
		top := 0
		for _, c := range p.Cards {
			if c != tcPhx && tcCardRank(c) > top {
				top = tcCardRank(c)
			}
		}
		// 봉황이 최상단 페어의 한 장을 대신했을 수 있으므로 연속성으로 보정
		counts := map[int]int{}
		for _, c := range p.Cards {
			if c != tcPhx {
				counts[tcCardRank(c)]++
			}
		}
		phx := tcContainsCard(p.Cards, tcPhx)
		if t, ok := tcPairSeqTop(counts, phx, len(p.Cards)); ok {
			top = t
		}
		combo.Value = top * 2
	}
	return combo
}

// tcFirstNatural 봉황이 아닌 첫 카드
func tcFirstNatural(cards []string) string {
	for _, c := range cards {
		if c != tcPhx {
			return c
		}
	}
	return cards[0]
}

// spawnTCBot 대기실의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *TCHub) spawnTCBot(room *tcRoom, name string) bool {
	bot := &TCClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runTCBot(bot)
	return true
}

// runTCBot 봇 수신 루프 기동 — 응답은 허브 gameMessage 채널로 되돌린다
func (h *TCHub) runTCBot(bot *TCClient) {
	brain := newTCBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m TCMessage) { h.gameMessage <- TCGameMessage{Client: bot, Message: m} },
		func(m TCMessage) bool { return m.Type == TCMsgGameOver || m.Type == TCMsgSessionExpired })
}
