package signal

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/deep-trader/internal/config"
	"github.com/deep-trader/internal/models"
)

func TestEngineConfirmDelayUsesElapsedTime(t *testing.T) {
	cfg := config.SignalConfig{
		ConfirmDelay: 10 * time.Second,
		Rules: config.RulesConfig{
			PumpImbalanceMin: 0.70,
			DumpImbalanceMax: 0.30,
			BidAskRatioPump:  2.0,
			BidAskRatioDump:  0.50,
			VolumeRatioMin:   5.0,
			PriceChangeMax:   0.15,
		},
	}

	engine := NewEngine(cfg, 0.0004, zap.NewNop())
	in := make(chan models.AnalyzerOutput, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := engine.Run(ctx, in)

	base := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	in <- pumpOutput("BTCUSDT", base)
	in <- pumpOutput("BTCUSDT", base.Add(10*time.Second))
	close(in)

	select {
	case signal, ok := <-out:
		if !ok {
			t.Fatal("expected confirmed signal, got closed channel")
		}
		if signal.Signal != models.SignalPump {
			t.Fatalf("expected pump signal, got %s", signal.Signal)
		}
		if !signal.Timestamp.Equal(base.Add(10 * time.Second)) {
			t.Fatalf("expected second event timestamp, got %s", signal.Timestamp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for confirmed signal")
	}
}

func pumpOutput(symbol string, ts time.Time) models.AnalyzerOutput {
	return models.AnalyzerOutput{
		Timestamp: ts,
		OrderBookMetrics: models.OrderBookMetrics{
			Symbol:      symbol,
			Timestamp:   ts,
			BidAskRatio: 2.5,
			BidDelta:    100,
			AskDelta:    -50,
		},
		TradeFlow: models.TradeFlowWindow{
			Symbol:    symbol,
			Duration:  30 * time.Second,
			Imbalance: 0.80,
		},
		TradeFlowWindows: []models.TradeFlowWindow{
			{Symbol: symbol, Duration: 30 * time.Second, Imbalance: 0.80},
		},
		VolumeRatio: 6.0,
		PriceChange: 0.01,
		MidPrice:    100,
	}
}
