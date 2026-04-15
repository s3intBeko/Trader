package router

import (
	"testing"
	"time"

	"github.com/deep-trader/internal/models"
)

func TestPickEventCursorUsesGlobalOrdering(t *testing.T) {
	base := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)

	left := &eventCursor{current: &models.MarketEvent{
		Symbol:    "BTCUSDT",
		Timestamp: base,
		EventType: models.EventTrade,
	}}
	right := &eventCursor{current: &models.MarketEvent{
		Symbol:    "BTCUSDT",
		Timestamp: base,
		EventType: models.EventDepth,
	}}

	picked := pickEventCursor(left, right)
	if picked != right {
		t.Fatal("expected depth event to win tie-break for same timestamp and symbol")
	}

	left = &eventCursor{current: &models.MarketEvent{
		Symbol:    "ETHUSDT",
		Timestamp: base,
		EventType: models.EventDepth,
	}}
	right = &eventCursor{current: &models.MarketEvent{
		Symbol:    "BTCUSDT",
		Timestamp: base,
		EventType: models.EventTrade,
	}}

	picked = pickEventCursor(left, right)
	if picked != right {
		t.Fatal("expected lexicographically smaller symbol to win tie-break")
	}
}

func TestPaceDurationUsesBacktestSpeed(t *testing.T) {
	prev := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	current := prev.Add(10 * time.Second)

	if got := paceDuration(prev, current, 100); got != 100*time.Millisecond {
		t.Fatalf("expected 100ms pace, got %s", got)
	}
	if got := paceDuration(prev, current, 0); got != 0 {
		t.Fatalf("expected zero pace for unlimited speed, got %s", got)
	}
}
