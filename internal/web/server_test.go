package web

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/deep-trader/internal/models"
)

func TestDashboardUsesSimulatedTimeInBacktestMode(t *testing.T) {
	d := NewDashboard(0, "backtest", nil, 1000, 10, 0.0004, zap.NewNop())
	start := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)

	d.ObserveEvent("BTCUSDT", start)
	d.UpdatePosition("BTCUSDT", &models.Position{
		Symbol:     "BTCUSDT",
		EntryPrice: 100,
		EntryTime:  start,
		Quantity:   1,
		Side:       "long",
	})
	d.UpdatePrice("BTCUSDT", 101)
	d.ObserveEvent("BTCUSDT", start.Add(15*time.Second))

	statusReq := httptest.NewRequest("GET", "/api/status", nil)
	statusRec := httptest.NewRecorder()
	d.handleStatus(statusRec, statusReq)

	var status map[string]interface{}
	if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil {
		t.Fatalf("status decode failed: %v", err)
	}

	if got := status["uptime"]; got != "15s" {
		t.Fatalf("expected uptime 15s, got %v", got)
	}
	if got := int(status["symbol_count"].(float64)); got != 1 {
		t.Fatalf("expected symbol_count 1, got %d", got)
	}
	if got := status["last_event"]; got != start.Add(15*time.Second).Format(time.RFC3339) {
		t.Fatalf("expected simulated last_event, got %v", got)
	}

	posReq := httptest.NewRequest("GET", "/api/positions", nil)
	posRec := httptest.NewRecorder()
	d.handlePositions(posRec, posReq)

	var positions map[string]positionWithPnL
	if err := json.Unmarshal(posRec.Body.Bytes(), &positions); err != nil {
		t.Fatalf("positions decode failed: %v", err)
	}

	if got := positions["BTCUSDT"].Duration; got != "15s" {
		t.Fatalf("expected simulated position duration 15s, got %s", got)
	}
}
