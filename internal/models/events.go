package models

import (
	"encoding/json"
	"time"
)

// EventType tanimlari
type EventType string

const (
	EventDepth   EventType = "depth"
	EventTrade   EventType = "trade"
	EventKline   EventType = "kline"
	EventMetrics EventType = "metrics"
)

// MarketEvent — sistemdeki tum veri bu struct uzerinden akar.
// Analyzer bu struct'i alir, verinin nereden geldigini bilmez.
type MarketEvent struct {
	Symbol    string          `json:"symbol"`
	Timestamp time.Time       `json:"timestamp"`
	EventType EventType       `json:"event_type"`
	Payload   json.RawMessage `json:"payload"`
	Source    string          `json:"source"` // "live" | "backtest"
}

// PriceLevel — order book'taki tek bir fiyat seviyesi
type PriceLevel struct {
	Price    float64 `json:"price"`
	Quantity float64 `json:"quantity"`
}

// OrderBook — derinlik verisi
type OrderBook struct {
	Symbol       string       `json:"symbol"`
	Timestamp    time.Time    `json:"timestamp"`
	Bids         []PriceLevel `json:"bids"`
	Asks         []PriceLevel `json:"asks"`
	LastUpdateID int64        `json:"last_update_id"`
}

// LargeOrder — buyuk emir tespiti
type LargeOrder struct {
	Price    float64 `json:"price"`
	Quantity float64 `json:"quantity"`
	Side     string  `json:"side"` // "bid" | "ask"
	USDValue float64 `json:"usd_value"`
}

// SpoofEvent — spoof suphelisi
type SpoofEvent struct {
	Symbol   string        `json:"symbol"`
	Price    float64       `json:"price"`
	Quantity float64       `json:"quantity"`
	Side     string        `json:"side"`
	Lifetime time.Duration `json:"lifetime"`
}

// OrderBookMetrics — hesaplanan order book metrikleri
type OrderBookMetrics struct {
	Symbol         string       `json:"symbol"`
	Timestamp      time.Time    `json:"timestamp"`
	TotalBidVolume float64      `json:"total_bid_volume"`
	TotalAskVolume float64      `json:"total_ask_volume"`
	BidAskRatio    float64      `json:"bid_ask_ratio"`
	BidDelta       float64      `json:"bid_delta"`
	AskDelta       float64      `json:"ask_delta"`
	LargeOrders    []LargeOrder `json:"large_orders"`
	SpoofSuspects  []SpoofEvent `json:"spoof_suspects"`
}

// AggTrade — gerceklesen islem
type AggTrade struct {
	Symbol    string    `json:"symbol"`
	Timestamp time.Time `json:"timestamp"`
	Price     float64   `json:"price"`
	Quantity  float64   `json:"quantity"`
	IsMaker   bool      `json:"is_maker"`
}

// TradeFlowWindow — islem akisi metrikleri
type TradeFlowWindow struct {
	Symbol      string        `json:"symbol"`
	WindowStart time.Time     `json:"window_start"`
	WindowEnd   time.Time     `json:"window_end"`
	Duration    time.Duration `json:"duration"`
	BuyVolume   float64       `json:"buy_volume"`
	SellVolume  float64       `json:"sell_volume"`
	TotalVolume float64       `json:"total_volume"`
	Imbalance   float64       `json:"imbalance"` // BuyVolume / TotalVolume [0..1]
	TradeCount  int           `json:"trade_count"`
	AvgTradeSize float64      `json:"avg_trade_size"`
}

// SignalType enum
type SignalType string

const (
	SignalTrendFollow SignalType = "TREND_FOLLOW"
	SignalPump        SignalType = "PUMP"
	SignalDump        SignalType = "DUMP"
	SignalNoEntry     SignalType = "NO_ENTRY"
)

// AnalyzerOutput — analyzer'in ciktisi
type AnalyzerOutput struct {
	OrderBookMetrics OrderBookMetrics `json:"order_book_metrics"`
	TradeFlow        TradeFlowWindow  `json:"trade_flow"`
	VolumeRatio      float64          `json:"volume_ratio"`
	IsConsolidating  bool             `json:"is_consolidating"`
	PriceChange      float64          `json:"price_change"`
	FundingRate      float64          `json:"funding_rate"`
	MidPrice         float64          `json:"mid_price"`
}

// SignalEvent — motor ciktisi
type SignalEvent struct {
	Symbol     string         `json:"symbol"`
	Timestamp  time.Time      `json:"timestamp"`
	Signal     SignalType     `json:"signal"`
	Confidence float64        `json:"confidence"`
	Source     string         `json:"source"` // "rules" | "ml" | "hybrid"
	Reasons    []string       `json:"reasons"`
	RawMetrics AnalyzerOutput `json:"raw_metrics"`
	IsExit     bool           `json:"is_exit"`      // true = pozisyon kapatma sinyali
	ExitReason string         `json:"exit_reason"`   // cikis sebebi
}

// Position — acik pozisyon
type Position struct {
	Symbol     string     `json:"symbol"`
	Side       string     `json:"side"` // "long" | "short"
	EntryPrice float64    `json:"entry_price"`
	EntryTime  time.Time  `json:"entry_time"`
	Quantity   float64    `json:"quantity"`
	Signal     SignalType `json:"signal"`
}

// PaperTrade — paper trading sonucu
type PaperTrade struct {
	Symbol     string     `json:"symbol"`
	EntryTime  time.Time  `json:"entry_time"`
	ExitTime   time.Time  `json:"exit_time"`
	EntryPrice float64    `json:"entry_price"`
	ExitPrice  float64    `json:"exit_price"`
	Side       string     `json:"side"`
	PnL        float64    `json:"pnl"`
	Signal     SignalType `json:"signal"`
	Reasons    []string   `json:"reasons"`
}
