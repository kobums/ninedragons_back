package server

import (
	"errors"
	"math/rand"
	"time"
)

// NewOTGame 로비 상태의 새 게임
func NewOTGame(id string) *OTGame {
	return &OTGame{
		ID:    id,
		Names: map[OTSide]string{},
		Phase: OTPhaseLobby,
	}
}

// AddPlayer 입장. 먼저 온 사람이 남쪽.
func (g *OTGame) AddPlayer(name string) (OTSide, error) {
	if g.Phase != OTPhaseLobby {
		return "", errors.New("이미 시작된 게임입니다")
	}
	if _, ok := g.Names[OTSouth]; !ok {
		g.Names[OTSouth] = name
		return OTSouth, nil
	}
	if _, ok := g.Names[OTNorth]; !ok {
		g.Names[OTNorth] = name
		return OTNorth, nil
	}
	return "", errors.New("자리가 없습니다")
}

// IsReady 게임 시작 준비 확인
func (g *OTGame) IsReady() bool {
	return len(g.Names) == 2
}

// otBackRow 진영의 시작(뒷) 줄
func otBackRow(side OTSide) int {
	if side == OTSouth {
		return OTBoardSize - 1
	}
	return 0
}

// otTemple 진영의 사원(마스터 시작 칸). 상대 마스터가 여기 도달하면 승리.
func otTemple(side OTSide) OTCell {
	return OTCell{Row: otBackRow(side), Col: OTBoardSize / 2}
}

// Start 기물을 배치하고 카드 5장을 돌린 뒤 플레이를 시작한다. 선공은 랜덤.
func (g *OTGame) Start(rng *rand.Rand) error {
	if !g.IsReady() {
		return errors.New("시작할 수 없습니다 (2명 필요)")
	}

	id := 0
	for _, side := range []OTSide{OTSouth, OTNorth} {
		row := otBackRow(side)
		for col := 0; col < OTBoardSize; col++ {
			g.Pieces = append(g.Pieces, &OTPiece{
				ID: id, Side: side, Master: col == OTBoardSize/2, Row: row, Col: col,
			})
			id++
		}
	}

	// 16장 중 5장: 각자 2장 + 대기 1장
	perm := rng.Perm(len(otCardDeck))[:OTHandSize*2+1]
	g.Hands = map[OTSide][]string{
		OTSouth: {otCardDeck[perm[0]].Name, otCardDeck[perm[1]].Name},
		OTNorth: {otCardDeck[perm[2]].Name, otCardDeck[perm[3]].Name},
	}
	g.WaitingCard = otCardDeck[perm[4]].Name

	g.CurrentSide = OTSouth
	if rng.Intn(2) == 1 {
		g.CurrentSide = OTNorth
	}
	g.Phase = OTPhasePlay
	g.Ready = true
	g.StartedAt = time.Now()
	return nil
}

// pieceAt 해당 칸의 살아있는 기물
func (g *OTGame) pieceAt(row, col int) *OTPiece {
	for _, p := range g.Pieces {
		if !p.Captured && p.Row == row && p.Col == col {
			return p
		}
	}
	return nil
}

// otApplyOffset 남쪽 시점 오프셋을 진영에 맞게 적용한다.
// 남쪽은 위(행 감소)로 전진, 북쪽은 180도 회전(전진·좌우 반전).
func otApplyOffset(side OTSide, from OTCell, off OTOffset) OTCell {
	if side == OTSouth {
		return OTCell{Row: from.Row - off.Forward, Col: from.Col + off.Right}
	}
	return OTCell{Row: from.Row + off.Forward, Col: from.Col - off.Right}
}

func otInBoard(c OTCell) bool {
	return c.Row >= 0 && c.Row < OTBoardSize && c.Col >= 0 && c.Col < OTBoardSize
}

// hasCard side 손에 해당 카드가 있는지
func (g *OTGame) hasCard(side OTSide, card string) bool {
	for _, c := range g.Hands[side] {
		if c == card {
			return true
		}
	}
	return false
}

// swapCard 쓴 카드를 대기 카드와 교환한다 (오니타마의 카드 순환)
func (g *OTGame) swapCard(side OTSide, card string) {
	for i, c := range g.Hands[side] {
		if c == card {
			g.Hands[side][i] = g.WaitingCard
			g.WaitingCard = card
			return
		}
	}
}

// LegalMoves side 진영의 합법 수 전체 (카드 × 기물 × 오프셋)
func (g *OTGame) LegalMoves(side OTSide) []OTLegalMove {
	moves := []OTLegalMove{}
	for _, card := range g.Hands[side] {
		def := otCardByName(card)
		if def == nil {
			continue
		}
		for _, p := range g.Pieces {
			if p.Captured || p.Side != side {
				continue
			}
			from := OTCell{Row: p.Row, Col: p.Col}
			for _, off := range def.Moves {
				to := otApplyOffset(side, from, off)
				if !otInBoard(to) {
					continue
				}
				if occupant := g.pieceAt(to.Row, to.Col); occupant != nil && occupant.Side == side {
					continue
				}
				moves = append(moves, OTLegalMove{Card: card, From: from, To: to})
			}
		}
	}
	return moves
}

// OTMoveResult 이동 한 번의 결과
type OTMoveResult struct {
	Captured       bool
	CapturedMaster bool
	GameOver       bool
}

// Move 카드로 기물 이동. 상대 마스터 잡기 또는 상대 사원 도달 시 즉시 승리.
func (g *OTGame) Move(side OTSide, card string, from, to OTCell) (*OTMoveResult, error) {
	if g.Phase != OTPhasePlay {
		return nil, errors.New("지금은 이동할 수 없습니다")
	}
	if side != g.CurrentSide {
		return nil, errors.New("당신의 차례가 아닙니다")
	}
	if !g.hasCard(side, card) {
		return nil, errors.New("손에 없는 카드입니다")
	}

	piece := g.pieceAt(from.Row, from.Col)
	if piece == nil || piece.Side != side {
		return nil, errors.New("그 칸에 내 기물이 없습니다")
	}

	legal := false
	for _, m := range g.LegalMoves(side) {
		if m.Card == card && m.From == from && m.To == to {
			legal = true
			break
		}
	}
	if !legal {
		return nil, errors.New("그 카드로는 그 칸에 갈 수 없습니다")
	}

	result := &OTMoveResult{}
	if target := g.pieceAt(to.Row, to.Col); target != nil {
		target.Captured = true
		result.Captured = true
		result.CapturedMaster = target.Master
		if target.Master {
			g.Winner = side
			g.EndReason = "capture_master"
			g.Phase = OTPhaseGameOver
			result.GameOver = true
		}
	}

	piece.Row, piece.Col = to.Row, to.Col

	// 개울의 길: 내 마스터가 상대 사원에 도달
	if !result.GameOver && piece.Master && to == otTemple(otOther(side)) {
		g.Winner = side
		g.EndReason = "reach_temple"
		g.Phase = OTPhaseGameOver
		result.GameOver = true
	}

	g.swapCard(side, card)
	if !result.GameOver {
		g.CurrentSide = otOther(side)
	}
	return result, nil
}

// Pass 둘 수 있는 수가 하나도 없을 때만 카드 하나를 교환하고 턴을 넘긴다
func (g *OTGame) Pass(side OTSide, card string) error {
	if g.Phase != OTPhasePlay {
		return errors.New("지금은 패스할 수 없습니다")
	}
	if side != g.CurrentSide {
		return errors.New("당신의 차례가 아닙니다")
	}
	if !g.hasCard(side, card) {
		return errors.New("손에 없는 카드입니다")
	}
	if len(g.LegalMoves(side)) > 0 {
		return errors.New("둘 수 있는 수가 있으면 패스할 수 없습니다")
	}

	g.swapCard(side, card)
	g.CurrentSide = otOther(side)
	return nil
}
