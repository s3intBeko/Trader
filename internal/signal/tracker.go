package signal

import (
	"fmt"
	"sync"

	"go.uber.org/zap"

	"github.com/deep-trader/internal/models"
)

// PositionSignalState — acik pozisyon icin sinyal gucunu takip eder
type PositionSignalState struct {
	Symbol        string
	Side          string  // "long" | "short"
	EntrySignal   models.SignalType
	EntryScore    float64
	CurrentScore  float64 // Bizim yondeki guncel skor
	ReverseScore  float64 // Ters yondeki guncel skor
	PeakScore     float64 // En yuksek skor (giris dahil)
	CycleCount    int     // Kac kez degerlendirildi
}

// ExitDecision — pozisyon kapatma karari
type ExitDecision struct {
	ShouldExit bool
	Reason     string
}

// SignalTracker — acik pozisyonlar icin sinyal gucunu surekli izler
type SignalTracker struct {
	positions map[string]*PositionSignalState
	rules     *RuleEngine

	// Esikler
	exitThreshold    float64 // Skor bu degerin altina duserse cik
	reverseThreshold float64 // Ters skor bu degerin ustune cikarsa hemen cik
	decayThreshold   float64 // Peak'ten bu kadar duserse cik
	minCycles        int     // Minimum bekleme suresi (cycle sayisi)

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
		minCycles:        6,  // en az 6 cycle (30sn) bekle
		logger:           logger,
	}
}

// TrackPosition — yeni acilan pozisyonu takibe al
func (st *SignalTracker) TrackPosition(symbol string, side string, signal models.SignalType, score float64) {
	st.mu.Lock()
	defer st.mu.Unlock()

	st.positions[symbol] = &PositionSignalState{
		Symbol:      symbol,
		Side:        side,
		EntrySignal: signal,
		EntryScore:  score,
		CurrentScore: score,
		PeakScore:   score,
		CycleCount:  0,
	}

	st.logger.Debug("pozisyon takibe alindi",
		zap.String("symbol", symbol),
		zap.String("side", side),
		zap.Float64("giris_skoru", score),
	)
}

// UntrackPosition — kapatilan pozisyonu takipten cikar
func (st *SignalTracker) UntrackPosition(symbol string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.positions, symbol)
}

// Evaluate — acik pozisyon icin guncel analyzer ciktisini degerlendir
func (st *SignalTracker) Evaluate(symbol string, out models.AnalyzerOutput) *ExitDecision {
	st.mu.Lock()
	defer st.mu.Unlock()

	state, ok := st.positions[symbol]
	if !ok {
		return nil // Bu sembolde acik pozisyon yok
	}

	state.CycleCount++

	// Kural motorundan guncel skorlari al
	pumpScore, dumpScore := st.calculateScores(out)

	// Bizim yon ve ters yon skorlarini belirle
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

	// Peak guncelle
	if ourScore > state.PeakScore {
		state.PeakScore = ourScore
	}

	st.logger.Debug("sinyal gucu degerlendirmesi",
		zap.String("symbol", symbol),
		zap.String("side", state.Side),
		zap.Float64("bizim_skor", ourScore),
		zap.Float64("ters_skor", reverseScore),
		zap.Float64("peak", state.PeakScore),
		zap.Int("cycle", state.CycleCount),
	)

	// KARAR 1: Ters sinyal cok guclu → HEMEN CIK (minCycles beklenmez)
	if reverseScore >= st.reverseThreshold {
		return &ExitDecision{
			ShouldExit: true,
			Reason: "ters sinyal guclu (skor: " + formatFloat(reverseScore) + ")",
		}
	}

	// Minimum bekleme suresi — erken cikisi onle
	if state.CycleCount < st.minCycles {
		return &ExitDecision{ShouldExit: false}
	}

	// KARAR 2: Bizim sinyal cok zayifladi → CIK
	if ourScore < st.exitThreshold {
		return &ExitDecision{
			ShouldExit: true,
			Reason: "sinyal zayifladi (skor: " + formatFloat(ourScore) + ", esik: " + formatFloat(st.exitThreshold) + ")",
		}
	}

	// KARAR 3: Peak'ten cok dustu → CIK (momentum kaybedildi)
	if state.PeakScore > 0 && (state.PeakScore - ourScore) >= st.decayThreshold {
		return &ExitDecision{
			ShouldExit: true,
			Reason: "momentum kaybi (peak: " + formatFloat(state.PeakScore) + " → suan: " + formatFloat(ourScore) + ")",
		}
	}

	// Sinyal hala guclu → TUT
	return &ExitDecision{ShouldExit: false}
}

// HasPosition — bu sembolde acik pozisyon var mi
func (st *SignalTracker) HasPosition(symbol string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	_, ok := st.positions[symbol]
	return ok
}

// calculateScores — analyzer ciktisindandan pump ve dump skorlarini hesaplar
func (st *SignalTracker) calculateScores(out models.AnalyzerOutput) (pumpScore, dumpScore float64) {
	ob := out.OrderBookMetrics
	tf := out.TradeFlow
	cfg := st.rules.cfg

	// PUMP skoru
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

	// DUMP skoru
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

func formatFloat(f float64) string {
	return fmt.Sprintf("%.2f", f)
}
