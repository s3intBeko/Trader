package analyzer

import (
	"encoding/json"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/deep-trader/internal/models"
)

// tradePayload — agg_trades JSON parse
type tradePayload struct {
	Price        float64 `json:"price"`
	Quantity     float64 `json:"quantity"`
	IsBuyerMaker bool    `json:"is_buyer_maker"`
}

type tradeBucket struct {
	BuyVolume  float64
	SellVolume float64
	TradeCount int
	Start      time.Time
}

type TradeFlowAnalyzer struct {
	windows []time.Duration
	buckets map[string]map[time.Duration]*tradeBucket
	mu      sync.RWMutex
	logger  *zap.Logger
}

func NewTradeFlowAnalyzer(windows []time.Duration, logger *zap.Logger) *TradeFlowAnalyzer {
	return &TradeFlowAnalyzer{
		windows: windows,
		buckets: make(map[string]map[time.Duration]*tradeBucket),
		logger:  logger,
	}
}

func (tfa *TradeFlowAnalyzer) Process(e models.MarketEvent) {
	var tp tradePayload
	if err := json.Unmarshal(e.Payload, &tp); err != nil {
		tfa.logger.Warn("trade parse hatasi",
			zap.String("symbol", e.Symbol),
			zap.Error(err),
		)
		return
	}

	tfa.mu.Lock()
	defer tfa.mu.Unlock()

	if _, ok := tfa.buckets[e.Symbol]; !ok {
		tfa.buckets[e.Symbol] = make(map[time.Duration]*tradeBucket)
	}

	for _, dur := range tfa.windows {
		bucket := tfa.getBucketLocked(e.Symbol, dur, e.Timestamp)

		// is_buyer_maker=true: satici agresif (market sell) → SATIS baskisi
		// is_buyer_maker=false: alici agresif (market buy) → ALIS baskisi
		if tp.IsBuyerMaker {
			bucket.SellVolume += tp.Quantity
		} else {
			bucket.BuyVolume += tp.Quantity
		}
		bucket.TradeCount++
	}
}

func (tfa *TradeFlowAnalyzer) getBucketLocked(symbol string, dur time.Duration, ts time.Time) *tradeBucket {
	b, ok := tfa.buckets[symbol][dur]
	if !ok || ts.Sub(b.Start) >= dur {
		b = &tradeBucket{Start: ts}
		tfa.buckets[symbol][dur] = b
	}
	return b
}

// Imbalance — belirli pencere icin trade flow imbalance degerini dondurur.
// > 0.70 → Guclu alis baskisi (pump sinyali)
// < 0.30 → Guclu satis baskisi (dump sinyali)
// 0.40-0.60 → Dengeli
func (tfa *TradeFlowAnalyzer) Imbalance(symbol string, dur time.Duration) float64 {
	tfa.mu.RLock()
	defer tfa.mu.RUnlock()

	b, ok := tfa.buckets[symbol][dur]
	if !ok {
		return 0.5
	}

	total := b.BuyVolume + b.SellVolume
	if total == 0 {
		return 0.5
	}
	return b.BuyVolume / total
}

// Window — belirli pencere icin TradeFlowWindow dondurur.
func (tfa *TradeFlowAnalyzer) Window(symbol string, dur time.Duration) models.TradeFlowWindow {
	tfa.mu.RLock()
	defer tfa.mu.RUnlock()

	b, ok := tfa.buckets[symbol][dur]
	if !ok {
		return models.TradeFlowWindow{Symbol: symbol, Duration: dur}
	}

	total := b.BuyVolume + b.SellVolume
	imbalance := 0.5
	avgSize := 0.0
	if total > 0 {
		imbalance = b.BuyVolume / total
	}
	if b.TradeCount > 0 {
		avgSize = total / float64(b.TradeCount)
	}

	return models.TradeFlowWindow{
		Symbol:       symbol,
		WindowStart:  b.Start,
		WindowEnd:    b.Start.Add(dur),
		Duration:     dur,
		BuyVolume:    b.BuyVolume,
		SellVolume:   b.SellVolume,
		TotalVolume:  total,
		Imbalance:    imbalance,
		TradeCount:   b.TradeCount,
		AvgTradeSize: avgSize,
	}
}

