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

type tradePoint struct {
	Timestamp  time.Time
	BuyVolume  float64
	SellVolume float64
	TradeCount int
}

type tradeWindowState struct {
	Points     []tradePoint
	BuyVolume  float64
	SellVolume float64
	TradeCount int
}

type TradeFlowAnalyzer struct {
	windows []time.Duration
	state   map[string]map[time.Duration]*tradeWindowState
	mu      sync.RWMutex
	logger  *zap.Logger
}

func NewTradeFlowAnalyzer(windows []time.Duration, logger *zap.Logger) *TradeFlowAnalyzer {
	return &TradeFlowAnalyzer{
		windows: windows,
		state:   make(map[string]map[time.Duration]*tradeWindowState),
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

	if _, ok := tfa.state[e.Symbol]; !ok {
		tfa.state[e.Symbol] = make(map[time.Duration]*tradeWindowState)
	}

	for _, dur := range tfa.windows {
		window := tfa.ensureWindowLocked(e.Symbol, dur)
		point := tradePoint{
			Timestamp:  e.Timestamp,
			TradeCount: 1,
		}

		// is_buyer_maker=true: satici agresif (market sell) → SATIS baskisi
		// is_buyer_maker=false: alici agresif (market buy) → ALIS baskisi
		if tp.IsBuyerMaker {
			point.SellVolume = tp.Quantity
			window.SellVolume += tp.Quantity
		} else {
			point.BuyVolume = tp.Quantity
			window.BuyVolume += tp.Quantity
		}
		window.TradeCount++
		window.Points = append(window.Points, point)
		tfa.trimWindowLocked(window, dur, e.Timestamp)
	}
}

// ImbalanceAt — belirli pencere icin trade flow imbalance degerini dondurur.
func (tfa *TradeFlowAnalyzer) ImbalanceAt(symbol string, dur time.Duration, at time.Time) float64 {
	return tfa.WindowAt(symbol, dur, at).Imbalance
}

// WindowAt — belirli pencere icin verilen andaki sliding-window metrigini dondurur.
func (tfa *TradeFlowAnalyzer) WindowAt(symbol string, dur time.Duration, at time.Time) models.TradeFlowWindow {
	tfa.mu.Lock()
	defer tfa.mu.Unlock()

	windowMap, ok := tfa.state[symbol]
	if !ok {
		return models.TradeFlowWindow{
			Symbol:      symbol,
			WindowStart: at.Add(-dur),
			WindowEnd:   at,
			Duration:    dur,
		}
	}

	window, ok := windowMap[dur]
	if !ok {
		return models.TradeFlowWindow{
			Symbol:      symbol,
			WindowStart: at.Add(-dur),
			WindowEnd:   at,
			Duration:    dur,
		}
	}

	tfa.trimWindowLocked(window, dur, at)

	total := window.BuyVolume + window.SellVolume
	imbalance := 0.5
	avgSize := 0.0
	if total > 0 {
		imbalance = window.BuyVolume / total
	}
	if window.TradeCount > 0 {
		avgSize = total / float64(window.TradeCount)
	}

	start := at.Add(-dur)
	if len(window.Points) > 0 {
		start = window.Points[0].Timestamp
	}

	return models.TradeFlowWindow{
		Symbol:       symbol,
		WindowStart:  start,
		WindowEnd:    at,
		Duration:     dur,
		BuyVolume:    window.BuyVolume,
		SellVolume:   window.SellVolume,
		TotalVolume:  total,
		Imbalance:    imbalance,
		TradeCount:   window.TradeCount,
		AvgTradeSize: avgSize,
	}
}

func (tfa *TradeFlowAnalyzer) ensureWindowLocked(symbol string, dur time.Duration) *tradeWindowState {
	window, ok := tfa.state[symbol][dur]
	if !ok {
		window = &tradeWindowState{}
		tfa.state[symbol][dur] = window
	}
	return window
}

func (tfa *TradeFlowAnalyzer) trimWindowLocked(window *tradeWindowState, dur time.Duration, at time.Time) {
	cutoff := at.Add(-dur)
	trimIdx := 0
	for trimIdx < len(window.Points) && window.Points[trimIdx].Timestamp.Before(cutoff) {
		point := window.Points[trimIdx]
		window.BuyVolume -= point.BuyVolume
		window.SellVolume -= point.SellVolume
		window.TradeCount -= point.TradeCount
		trimIdx++
	}
	if trimIdx > 0 {
		window.Points = window.Points[trimIdx:]
	}
	if window.TradeCount < 0 {
		window.TradeCount = 0
	}
	if window.BuyVolume < 0 {
		window.BuyVolume = 0
	}
	if window.SellVolume < 0 {
		window.SellVolume = 0
	}
}
