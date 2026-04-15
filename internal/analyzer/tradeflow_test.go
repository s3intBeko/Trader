package analyzer

import (
	"encoding/json"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/deep-trader/internal/models"
)

func TestTradeFlowAnalyzerUsesSlidingWindow(t *testing.T) {
	tfa := NewTradeFlowAnalyzer([]time.Duration{30 * time.Second}, zap.NewNop())
	base := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)

	tfa.Process(tradeEvent("BTCUSDT", base, 100, 1, false))
	tfa.Process(tradeEvent("BTCUSDT", base.Add(10*time.Second), 101, 2, true))
	tfa.Process(tradeEvent("BTCUSDT", base.Add(35*time.Second), 102, 3, false))

	window := tfa.WindowAt("BTCUSDT", 30*time.Second, base.Add(35*time.Second))
	if window.TradeCount != 2 {
		t.Fatalf("expected 2 trades in window, got %d", window.TradeCount)
	}
	if window.BuyVolume != 3 {
		t.Fatalf("expected buy volume 3, got %.2f", window.BuyVolume)
	}
	if window.SellVolume != 2 {
		t.Fatalf("expected sell volume 2, got %.2f", window.SellVolume)
	}
	if window.Imbalance != 0.6 {
		t.Fatalf("expected imbalance 0.6, got %.2f", window.Imbalance)
	}
}

func tradeEvent(symbol string, ts time.Time, price, quantity float64, isBuyerMaker bool) models.MarketEvent {
	payload, err := json.Marshal(map[string]interface{}{
		"price":          price,
		"quantity":       quantity,
		"is_buyer_maker": isBuyerMaker,
	})
	if err != nil {
		panic(err)
	}

	return models.MarketEvent{
		Symbol:    symbol,
		Timestamp: ts,
		EventType: models.EventTrade,
		Payload:   payload,
		Source:    "test",
	}
}
