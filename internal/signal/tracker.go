package signal

import (
	"fmt"
	"sync"

	"go.uber.org/zap"

	"github.com/deep-trader/internal/models"
)

// PositionSignalState — acik pozisyon icin sinyal gucunu ve PnL'i takip eder
type PositionSignalState struct {
	Symbol       string
	Side         string // "long" | "short"
	EntrySignal  models.SignalType
	EntryScore   float64
	EntryPrice   float64
	Quantity     float64
	Leverage     int

	// Sinyal takibi
	CurrentScore float64
	ReverseScore float64
	PeakScore    float64
	CycleCount   int

	// PnL takibi
	CurrentPnLPct float64 // Teminat uzerinden anlik PnL %
	PeakPnLPct    float64 // En yuksek PnL %
}

// ExitDecision — pozisyon kapatma karari
type ExitDecision struct {
	ShouldExit bool
	Reason     string
}

// SignalTracker — acik pozisyonlar icin sinyal gucu + PnL trailing izler
type SignalTracker struct {
	positions map[string]*PositionSignalState
	rules     *RuleEngine

	// Sinyal esikleri
	exitThreshold    float64
	reverseThreshold float64
	decayThreshold   float64
	minCycles        int

	mu     sync.Mutex
	logger *zap.Logger
}

func NewSignalTracker(rules *RuleEngine, logger *zap.Logger) *SignalTracker {
	return &SignalTracker{
		positions:        make(map[string]*PositionSignalState),
		rules:            rules,
		exitThreshold:    0.30,
		reverseThreshold: 0.60,
		decayThreshold:   0.40,
		minCycles:        6,
		logger:           logger,
	}
}

// TrackPosition — yeni acilan pozisyonu takibe al
func (st *SignalTracker) TrackPosition(symbol string, side string, signal models.SignalType, score float64, entryPrice float64, quantity float64, leverage int) {
	st.mu.Lock()
	defer st.mu.Unlock()

	st.positions[symbol] = &PositionSignalState{
		Symbol:      symbol,
		Side:        side,
		EntrySignal: signal,
		EntryScore:  score,
		EntryPrice:  entryPrice,
		Quantity:    quantity,
		Leverage:    leverage,
		CurrentScore: score,
		PeakScore:   score,
		CycleCount:  0,
	}

	st.logger.Debug("pozisyon takibe alindi",
		zap.String("symbol", symbol),
		zap.String("side", side),
		zap.Float64("giris_skoru", score),
		zap.Float64("giris_fiyat", entryPrice),
	)
}

// UntrackPosition — kapatilan pozisyonu takipten cikar
func (st *SignalTracker) UntrackPosition(symbol string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.positions, symbol)
}

// UpdatePrice — dis kaynaktan (trade event) guncel fiyati gunceller
func (st *SignalTracker) UpdatePrice(symbol string, price float64) {
	st.mu.Lock()
	defer st.mu.Unlock()

	state, ok := st.positions[symbol]
	if !ok || price <= 0 || state.EntryPrice <= 0 {
		return
	}

	var pricePct float64
	if state.Side == "long" {
		pricePct = (price - state.EntryPrice) / state.EntryPrice
	} else {
		pricePct = (state.EntryPrice - price) / state.EntryPrice
	}
	state.CurrentPnLPct = pricePct * float64(state.Leverage) * 100
	if state.CurrentPnLPct > state.PeakPnLPct {
		state.PeakPnLPct = state.CurrentPnLPct
	}
}

// Evaluate — acik pozisyon icin guncel analyzer ciktisini degerlendir
func (st *SignalTracker) Evaluate(symbol string, out models.AnalyzerOutput) *ExitDecision {
	st.mu.Lock()
	defer st.mu.Unlock()

	state, ok := st.positions[symbol]
	if !ok {
		return nil
	}

	state.CycleCount++

	// Sinyal skorlarini hesapla
	pumpScore, dumpScore := st.calculateScores(out)

	var ourScore, reverseScore float64
	if state.Side == "long" {
		ourScore = pumpScore
		reverseScore = dumpScore
	} else {
		ourScore = dumpScore
		reverseScore = pumpScore
	}

	state.CurrentScore = ourScore
	state.ReverseScore = reverseScore
	if ourScore > state.PeakScore {
		state.PeakScore = ourScore
	}

	// PnL zaten UpdatePrice ile anlik guncelleniyor (trade event'lerinden)

	st.logger.Debug("pozisyon degerlendirmesi",
		zap.String("symbol", symbol),
		zap.String("side", state.Side),
		zap.Float64("bizim_skor", ourScore),
		zap.Float64("ters_skor", reverseScore),
		zap.Float64("pnl_pct", state.CurrentPnLPct),
		zap.Float64("peak_pnl_pct", state.PeakPnLPct),
		zap.Int("cycle", state.CycleCount),
	)

	// ══════════════════════════════════════════════════
	// KARAR 1: Ters sinyal cok guclu → HEMEN CIK
	// ══════════════════════════════════════════════════
	if reverseScore >= st.reverseThreshold {
		return &ExitDecision{
			ShouldExit: true,
			Reason: fmt.Sprintf("ters sinyal guclu (skor: %.2f) | PnL: %.1f%%", reverseScore, state.CurrentPnLPct),
		}
	}

	// ══════════════════════════════════════════════════
	// KARAR 2: Kademeli trailing stop (PnL bazli)
	// Peak kar buyudukce koruma sikilasir
	// ══════════════════════════════════════════════════
	if trailing := st.checkTrailingStop(state); trailing != nil {
		return trailing
	}

	// Minimum bekleme suresi
	if state.CycleCount < st.minCycles {
		return &ExitDecision{ShouldExit: false}
	}

	// ══════════════════════════════════════════════════
	// KARAR 3: Sinyal zayifladi
	// ══════════════════════════════════════════════════
	if ourScore < st.exitThreshold {
		return &ExitDecision{
			ShouldExit: true,
			Reason: fmt.Sprintf("sinyal zayifladi (skor: %.2f) | PnL: %.1f%%", ourScore, state.CurrentPnLPct),
		}
	}

	// ══════════════════════════════════════════════════
	// KARAR 4: Sinyal momentum kaybi
	// ══════════════════════════════════════════════════
	if state.PeakScore > 0 && (state.PeakScore-ourScore) >= st.decayThreshold {
		return &ExitDecision{
			ShouldExit: true,
			Reason: fmt.Sprintf("momentum kaybi (skor peak: %.2f → %.2f) | PnL: %.1f%%", state.PeakScore, ourScore, state.CurrentPnLPct),
		}
	}

	return &ExitDecision{ShouldExit: false}
}

// checkTrailingStop — kademeli trailing stop kontrolu
//
//	PnL %0-3   → trailing yok, nefes alsin
//	PnL %3-8   → peak'ten %50 geri cekilirse kapat
//	PnL %8-15  → peak'ten %35 geri cekilirse kapat
//	PnL %15+   → peak'ten %25 geri cekilirse kapat
func (st *SignalTracker) checkTrailingStop(state *PositionSignalState) *ExitDecision {
	peak := state.PeakPnLPct
	current := state.CurrentPnLPct

	// Peak %3'un altindaysa trailing aktif degil
	if peak < 3.0 {
		return nil
	}

	var trailingPct float64
	var band string

	switch {
	case peak >= 15.0:
		trailingPct = 0.25 // peak'ten %25 geri cekilme
		band = "yuksek kar"
	case peak >= 8.0:
		trailingPct = 0.35 // peak'ten %35 geri cekilme
		band = "orta kar"
	default: // peak >= 3.0
		trailingPct = 0.50 // peak'ten %50 geri cekilme
		band = "dusuk kar"
	}

	// Trailing floor: peak'in (1 - trailingPct) kadarini koru
	floor := peak * (1.0 - trailingPct)

	if current <= floor {
		return &ExitDecision{
			ShouldExit: true,
			Reason: fmt.Sprintf("trailing stop [%s] (peak: %.1f%% → suan: %.1f%%, floor: %.1f%%)",
				band, peak, current, floor),
		}
	}

	return nil
}

// HasPosition — bu sembolde acik pozisyon var mi
func (st *SignalTracker) HasPosition(symbol string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	_, ok := st.positions[symbol]
	return ok
}

func (st *SignalTracker) calculateScores(out models.AnalyzerOutput) (pumpScore, dumpScore float64) {
	ob := out.OrderBookMetrics
	tf := out.TradeFlow
	cfg := st.rules.cfg

	if tf.Imbalance >= cfg.PumpImbalanceMin {
		pumpScore += 0.35
	}
	if ob.BidAskRatio >= cfg.BidAskRatioPump {
		pumpScore += 0.25
	}
	if out.VolumeRatio >= cfg.VolumeRatioMin && out.PriceChange <= cfg.PriceChangeMax {
		pumpScore += 0.30
	}
	if ob.BidDelta > 0 && ob.AskDelta < 0 {
		pumpScore += 0.10
	}
	if len(ob.SpoofSuspects) > 0 {
		pumpScore -= cfg.SpoofPenalty
	}

	if tf.Imbalance <= cfg.DumpImbalanceMax {
		dumpScore += 0.35
	}
	if ob.BidAskRatio <= cfg.BidAskRatioDump {
		dumpScore += 0.25
	}
	if ob.BidDelta < 0 && ob.AskDelta > 0 {
		dumpScore += 0.25
	}
	if len(ob.SpoofSuspects) > 0 {
		dumpScore -= cfg.SpoofPenalty
	}

	return pumpScore, dumpScore
}
