package analyzer

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/deep-trader/internal/config"
	"github.com/deep-trader/internal/models"
)

func TestAnalyzerInjectsRecentSpoofsIntoOutput(t *testing.T) {
	cfg := config.AnalyzerConfig{
		LargeOrderThresholdUSD: 1000,
		SpoofMaxLifetime:       30 * time.Second,
		EmitInterval:           5 * time.Second,
	}
	a := New("backtest", cfg, nil, zap.NewNop())

	base := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	a.process(depthEvent("BTCUSDT", base, []float64{99}, []float64{100}, []float64{101}, []float64{100}))
	a.process(depthEvent("BTCUSDT", base.Add(time.Second), []float64{99}, []float64{100}, []float64{101}, []float64{100}))
	a.process(depthEvent("BTCUSDT", base.Add(2*time.Second), []float64{99}, []float64{1}, []float64{101}, []float64{100}))

	out := a.buildOutput(context.Background(), "BTCUSDT", base.Add(2*time.Second))
	if len(out.OrderBookMetrics.SpoofSuspects) != 1 {
		t.Fatalf("expected 1 spoof suspect, got %d", len(out.OrderBookMetrics.SpoofSuspects))
	}

	expired := a.buildOutput(context.Background(), "BTCUSDT", base.Add(8*time.Second))
	if len(expired.OrderBookMetrics.SpoofSuspects) != 0 {
		t.Fatalf("expected spoof suspect to expire outside emit window, got %d", len(expired.OrderBookMetrics.SpoofSuspects))
	}
}

func TestAnalyzerMetricTimeUsesEventTimeInPaperMode(t *testing.T) {
	a := New("paper", config.AnalyzerConfig{}, nil, zap.NewNop())
	eventTime := time.Date(2026, 4, 15, 12, 34, 56, 0, time.UTC)

	if got := a.metricTime(eventTime); !got.Equal(eventTime) {
		t.Fatalf("expected metric time %s, got %s", eventTime, got)
	}
}

func depthEvent(symbol string, ts time.Time, bidPrices, bidQuantities, askPrices, askQuantities []float64) models.MarketEvent {
	payload, err := json.Marshal(map[string]interface{}{
		"bid_prices":     bidPrices,
		"bid_quantities": bidQuantities,
		"ask_prices":     askPrices,
		"ask_quantities": askQuantities,
	})
	if err != nil {
		panic(err)
	}

	return models.MarketEvent{
		Symbol:    symbol,
		Timestamp: ts,
		EventType: models.EventDepth,
		Payload:   payload,
		Source:    "test",
	}
}
