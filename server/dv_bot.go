package server

// ==================== 다빈치코드 연습봇 ====================
//
// 스냅샷(dv_game_state)만 보고 반응한다. dv_botgame_test.go 의 프로토콜
// 완주 드라이버 판단을 그대로 승격한 무작위+합법 두뇌 — 추리 값을 -1~11 로
// 순환시키므로 언젠가는 맞고, 틀리면 자기 타일이 공개되므로 게임은 반드시
// 유한하게 끝난다. 적중 후에는 항상 멈춰 뽑은 타일을 비공개로 지킨다.

// dvBotState 봇이 상태 스냅샷에서 꺼내 쓰는 최소 정보
type dvBotState struct {
	YourSeat          int    `json:"yourSeat"`
	Phase             string `json:"phase"`
	CurrentSeat       int    `json:"currentSeat"`
	DeckBlackCount    int    `json:"deckBlackCount"`
	DeckWhiteCount    int    `json:"deckWhiteCount"`
	YourPendingJokers []struct {
		ID int `json:"id"`
	} `json:"yourPendingJokers"`
	DrawnTile *struct {
		ID int `json:"id"`
	} `json:"drawnTile"`
	Players []struct {
		Seat             int  `json:"seat"`
		Eliminated       bool `json:"eliminated"`
		InitialRemaining int  `json:"initialRemaining"`
		Tiles            []struct {
			Revealed bool `json:"revealed"`
		} `json:"tiles"`
	} `json:"players"`
}

// dvBrain 스냅샷 기반 판단. guessCounter 로 추리 값을 순환시킨다.
type dvBrain struct {
	guessCounter int
}

func newDVBrain() *dvBrain { return &dvBrain{} }

// decide 공용 러너 계약 — dv_game_state 에만 반응한다
func (b *dvBrain) decide(msg DVMessage) *DVMessage {
	if msg.Type != DVMsgGameState {
		return nil
	}
	state, ok := botPayloadAs[dvBotState](msg.Payload)
	if !ok {
		return nil
	}
	return b.decideState(state)
}

// decideState 자기 차례·단계에 맞는 행동 하나를 고른다 (할 일 없으면 nil)
func (b *dvBrain) decideState(state dvBotState) *DVMessage {
	me := state.YourSeat

	// 시작 타일 가져오기는 턴과 무관하게 처리한다
	if state.Phase == string(DVPhaseInitialDraw) {
		for _, p := range state.Players {
			if p.Seat == me && p.InitialRemaining > 0 {
				return &DVMessage{Type: DVMsgTakeInitial,
					Payload: DVTakeInitialPayload{Color: b.pickColor(state)}}
			}
		}
		return nil
	}

	// 초기 조커 배치도 턴과 무관하게 처리한다
	if state.Phase == string(DVPhaseJokerSetup) {
		if len(state.YourPendingJokers) > 0 {
			return &DVMessage{Type: DVMsgPlaceJoker, Payload: DVPlaceJokerPayload{
				TileID: state.YourPendingJokers[0].ID, Position: 0,
			}}
		}
		return nil
	}

	if state.CurrentSeat != me {
		return nil
	}

	switch state.Phase {
	case string(DVPhaseDraw):
		return &DVMessage{Type: DVMsgDrawTile,
			Payload: DVDrawTilePayload{Color: b.pickColor(state)}}

	case string(DVPhaseGuess):
		// 첫 번째 상대의 첫 비공개 타일을 -1~11 순환 값으로 추리
		for _, p := range state.Players {
			if p.Seat == me || p.Eliminated {
				continue
			}
			for idx, tile := range p.Tiles {
				if !tile.Revealed {
					value := (b.guessCounter % 13) - 1
					b.guessCounter++
					return &DVMessage{Type: DVMsgGuess, Payload: DVGuessPayload{
						TargetSeat: p.Seat, TileIndex: idx, Value: value,
					}}
				}
			}
		}

	case string(DVPhaseContinueChoice):
		return &DVMessage{Type: DVMsgContinueChoice,
			Payload: DVContinueChoicePayload{Continue: false}}

	case string(DVPhasePlaceDrawnJoker):
		if state.DrawnTile != nil {
			return &DVMessage{Type: DVMsgPlaceJoker, Payload: DVPlaceJokerPayload{
				TileID: state.DrawnTile.ID, Position: 0,
			}}
		}

	case string(DVPhaseRevealOwn):
		for _, p := range state.Players {
			if p.Seat != me {
				continue
			}
			for idx, tile := range p.Tiles {
				if !tile.Revealed {
					return &DVMessage{Type: DVMsgRevealOwn,
						Payload: DVRevealOwnPayload{TileIndex: idx}}
				}
			}
		}
	}
	return nil
}

// pickColor 뽑을 색 — 검정 우선, 검정이 다 떨어졌으면 흰색
func (b *dvBrain) pickColor(state dvBotState) DVTileColor {
	if state.DeckBlackCount == 0 {
		return DVWhite
	}
	return DVBlack
}

// ==================== 봇 소환 ====================

// spawnDVBot 로비(시작 전 방)의 빈 좌석에 연습봇을 앉힌다 (허브 고루틴에서 호출)
func (h *DVHub) spawnDVBot(room *dvRoom, name string) bool {
	bot := &DVClient{wsClient: newBotWSClient(), Hub: h}
	bot.Name = name
	seat, err := room.Game.AddPlayer(name)
	if err != nil {
		return false
	}
	bot.GameID = room.Game.ID
	bot.Seat = seat
	room.Clients[seat] = bot
	h.sessions[bot.SessionID] = bot
	h.runDVBot(bot)
	return true
}

// runDVBot 공용 러너 기동 — 게임 종료·세션 만료 신호에 스스로 끝난다
func (h *DVHub) runDVBot(bot *DVClient) {
	brain := newDVBrain()
	go runBot(bot.Send,
		brain.decide,
		func(m DVMessage) { h.gameMessage <- DVGameMessage{Client: bot, Message: m} },
		func(m DVMessage) bool { return m.Type == DVMsgGameOver || m.Type == DVMsgSessionExpired })
}

// dvRoomHasBot 방에 연습봇이 있는지 (ntfy 억제·전적 기록용)
func dvRoomHasBot(room *dvRoom) bool {
	for _, c := range room.Clients {
		if c != nil && c.Bot {
			return true
		}
	}
	return false
}

// dvHumanRemains 세션이 살아있는(접속 중이거나 재접속 유예 중인) 사람이
// 남아 있는지. 전원 몰수로 봇만 남은 방을 정리하는 판단 근거다.
func (h *DVHub) dvHumanRemains(room *dvRoom) bool {
	for _, c := range room.Clients {
		if c != nil && !c.Bot && h.sessions[c.SessionID] == c {
			return true
		}
	}
	return false
}
