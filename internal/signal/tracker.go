package signal

import (
	"fmt"
	"sync"
	"time"

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
	EntryTime    time.Time

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

	// Zarar toleransi
	lightLossMax     float64 // bu yuzdenin altindaki zararlar tolere edilir (varsayilan: -5.0)
	heavyLossMax     float64 // bu yuzdenin altinda hard stop (varsayilan: -15.0)

	// Fee
	takerFeePct      float64

	// Stale pozisyon kontrolu (1 saat gecmis + -%5 ile +%5 arasi = kapat)
	staleTimeout time.Duration // 0 = devre disi
	stalePnLMax  float64       // bu yuzdenin altindaki PnL "stale" sayilir

	// Zarar sonrasi cooldown — ayni sembolde tekrar giris engeli
	lossCooldown  time.Duration            // 0 = devre disi
	cooldownUntil map[string]time.Time     // symbol -> ne zamana kadar girilmez

	// Acil cikislar (UpdatePrice'da tetiklenen)
	pendingExits map[string]*ExitDecision

	mu     sync.Mutex
	logger *zap.Logger
}

func NewSignalTracker(rules *RuleEngine, takerFeePct float64, staleTimeout time.Duration, lossCooldown time.Duration, logger *zap.Logger) *SignalTracker {
	return &SignalTracker{
		positions:        make(map[string]*PositionSignalState),
		rules:            rules,
		exitThreshold:    0.30,
		reverseThreshold: 0.60,
		decayThreshold:   0.40,
		minCycles:        6,
		lightLossMax:     -5.0,
		heavyLossMax:     -15.0,
		takerFeePct:      takerFeePct,
		staleTimeout:     staleTimeout,
		stalePnLMax:      5.0,
		lossCooldown:     lossCooldown,
		cooldownUntil:    make(map[string]time.Time),
		pendingExits:     make(map[string]*ExitDecision),
		logger:           logger,
	}
}

// TrackPosition — yeni acilan pozisyonu takibe al
func (st *SignalTracker) TrackPosition(symbol string, side string, signal models.SignalType, score float64, entryPrice float64, quantity float64, leverage int, entryTime time.Time) {
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
		EntryTime:   entryTime,
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

	// Zararda kapandiysa cooldown uygula
	if state, ok := st.positions[symbol]; ok && st.lossCooldown > 0 {
		if state.CurrentPnLPct < 0 {
			st.cooldownUntil[symbol] = time.Now().Add(st.lossCooldown)
			st.logger.Debug("cooldown eklendi",
				zap.String("symbol", symbol),
				zap.Duration("sure", st.lossCooldown),
				zap.Float64("pnl_pct", state.CurrentPnLPct),
			)
		}
	}

	delete(st.positions, symbol)
}

// IsOnCooldown — sembol cooldown'da mi
func (st *SignalTracker) IsOnCooldown(symbol string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()

	until, ok := st.cooldownUntil[symbol]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(st.cooldownUntil, symbol)
		return false
	}
	return true
}

// UpdatePrice — dis kaynaktan (trade event) guncel fiyati gunceller
// PnL hesabi fee dahil yapilir (gercekci net PnL)
// Hard stop-loss ve trailing stop tetiklenirse urgentExit kanalina sinyal gonderir
func (st *SignalTracker) UpdatePrice(symbol string, price float64) {
	st.mu.Lock()
	defer st.mu.Unlock()

	state, ok := st.positions[symbol]
	if !ok || price <= 0 || state.EntryPrice <= 0 {
		return
	}

	// Brut PnL%
	var grossPct float64
	if state.Side == "long" {
		grossPct = (price - state.EntryPrice) / state.EntryPrice
	} else {
		grossPct = (state.EntryPrice - price) / state.EntryPrice
	}

	feePct := 2 * st.takerFeePct * float64(state.Leverage)
	state.CurrentPnLPct = (grossPct*float64(state.Leverage) - feePct) * 100

	if state.CurrentPnLPct > state.PeakPnLPct {
		state.PeakPnLPct = state.CurrentPnLPct
	}

	// ANLIK KONTROL: Sadece henuz pending exit yoksa ekle (spam onleme)
	if _, alreadyPending := st.pendingExits[symbol]; !alreadyPending {
		// Hard stop-loss
		if state.CurrentPnLPct <= st.heavyLossMax {
			st.pendingExits[symbol] = &ExitDecision{
				ShouldExit: true,
				Reason: fmt.Sprintf("hard stop-loss ANLIK (PnL: %.1f%%, limit: %.1f%%)", state.CurrentPnLPct, st.heavyLossMax),
			}
		}

		// Trailing stop
		if trailing := st.checkTrailingStop(state); trailing != nil {
			st.pendingExits[symbol] = trailing
		}
	}
}

// DrainPendingExits — UpdatePrice'da tetiklenen acil cikislari dondurur ve temizler.
// Sadece hala acik pozisyonu olan semboller icin cikis dondurur.
func (st *SignalTracker) DrainPendingExits() map[string]*ExitDecision {
	st.mu.Lock()
	defer st.mu.Unlock()

	if len(st.pendingExits) == 0 {
		return nil
	}

	// Sadece hala tracker'da olan pozisyonlari dondur
	exits := make(map[string]*ExitDecision)
	for sym, decision := range st.pendingExits {
		if _, ok := st.positions[sym]; ok {
			exits[sym] = decision
			// Pozisyonu tracker'dan sil — tekrar pending exit uretilmesin
			delete(st.positions, sym)
		}
	}
	st.pendingExits = make(map[string]*ExitDecision)

	if len(exits) == 0 {
		return nil
	}
	return exits
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
	// KARAR 0: Hard stop-loss → -%15 altinda HEMEN CIK
	// ══════════════════════════════════════════════════
	if state.CurrentPnLPct <= st.heavyLossMax {
		return &ExitDecision{
			ShouldExit: true,
			Reason: fmt.Sprintf("hard stop-loss (PnL: %.1f%%, limit: %.1f%%)", state.CurrentPnLPct, st.heavyLossMax),
		}
	}

	// ══════════════════════════════════════════════════
	// KARAR 1: Kademeli trailing stop (PnL bazli)
	// Peak kar buyudukce koruma sikilasir — karda ise oncelikli
	// ══════════════════════════════════════════════════
	if trailing := st.checkTrailingStop(state); trailing != nil {
		return trailing
	}

	// ══════════════════════════════════════════════════
	// KARAR 1.5: Stale pozisyon kontrolu
	// 1 saat gecmis + PnL -%5 ile +%5 arasi = teminati serbest birak
	// ══════════════════════════════════════════════════
	if st.staleTimeout > 0 && !state.EntryTime.IsZero() {
		eventTime := out.OrderBookMetrics.Timestamp
		if eventTime.IsZero() {
			eventTime = time.Now()
		}
		elapsed := eventTime.Sub(state.EntryTime)
		if elapsed >= st.staleTimeout &&
			state.CurrentPnLPct > -st.stalePnLMax &&
			state.CurrentPnLPct < st.stalePnLMax {
			return &ExitDecision{
				ShouldExit: true,
				Reason: fmt.Sprintf("stale pozisyon (%s gecti, PnL: %.1f%% < ±%.0f%%)",
					elapsed.Truncate(time.Second), state.CurrentPnLPct, st.stalePnLMax),
			}
		}
	}

	// ══════════════════════════════════════════════════
	// KARAR 2: Ters sinyal cok guclu
	// Ama hafif zarardaysak tolere et — toparlanma sansi %63
	// ══════════════════════════════════════════════════
	if reverseScore >= st.reverseThreshold {
		// Karda veya agir zararda ise hemen cik
		if state.CurrentPnLPct >= 0 || state.CurrentPnLPct <= st.lightLossMax {
			return &ExitDecision{
				ShouldExit: true,
				Reason: fmt.Sprintf("ters sinyal guclu (skor: %.2f) | PnL: %.1f%%", reverseScore, state.CurrentPnLPct),
			}
		}
		// Hafif zararda (0 ile -%5 arasi) → tolere et, bekle
		st.logger.Debug("ters sinyal var ama hafif zararda, tolere ediliyor",
			zap.String("symbol", symbol),
			zap.Float64("pnl_pct", state.CurrentPnLPct),
			zap.Float64("ters_skor", reverseScore),
		)
	}

	// Minimum bekleme suresi
	if state.CycleCount < st.minCycles {
		return &ExitDecision{ShouldExit: false}
	}

	// ══════════════════════════════════════════════════
	// KARAR 3: Sinyal zayifladi
	// Hafif zarardaysa tolere et
	// ══════════════════════════════════════════════════
	if ourScore < st.exitThreshold {
		if state.CurrentPnLPct >= 0 || state.CurrentPnLPct <= st.lightLossMax {
			return &ExitDecision{
				ShouldExit: true,
				Reason: fmt.Sprintf("sinyal zayifladi (skor: %.2f) | PnL: %.1f%%", ourScore, state.CurrentPnLPct),
			}
		}
		// Hafif zararda → bekle
	}

	// ══════════════════════════════════════════════════
	// KARAR 4: Sinyal momentum kaybi
	// Hafif zarardaysa tolere et
	// ══════════════════════════════════════════════════
	if state.PeakScore > 0 && (state.PeakScore-ourScore) >= st.decayThreshold {
		if state.CurrentPnLPct >= 0 || state.CurrentPnLPct <= st.lightLossMax {
			return &ExitDecision{
				ShouldExit: true,
				Reason: fmt.Sprintf("momentum kaybi (skor peak: %.2f → %.2f) | PnL: %.1f%%", state.PeakScore, ourScore, state.CurrentPnLPct),
			}
		}
		// Hafif zararda → bekle
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
