package analyzer

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/deep-trader/internal/config"
	"github.com/deep-trader/internal/models"
	"github.com/deep-trader/internal/store"
)

type cachedFloat struct {
	Bucket time.Time
	Value  float64
}

type cachedBool struct {
	Bucket time.Time
	Value  bool
}

type detectedSpoof struct {
	DetectedAt time.Time
	Event      models.SpoofEvent
}

type Analyzer struct {
	mode  string
	cfg   config.AnalyzerConfig
	ob    *OrderBookAnalyzer
	tf    *TradeFlowAnalyzer
	vol   *VolumeAnalyzer
	spoof *SpoofDetector
	store *store.Store

	// Fiyat takibi (price change hesabi icin)
	prices   map[string][]pricePoint
	pricesMu sync.RWMutex

	// Event-time cache (background refresh goroutine'den de yazilir)
	fundingCache map[string]cachedFloat
	consolCache  map[string]cachedBool
	cacheMu      sync.RWMutex

	spoofEvents map[string][]detectedSpoof
	spoofMu     sync.RWMutex

	lastEmit map[string]time.Time
	out      chan models.AnalyzerOutput
	logger   *zap.Logger
}

type pricePoint struct {
	Price float64
	Time  time.Time
}

func New(mode string, cfg config.AnalyzerConfig, s *store.Store, logger *zap.Logger) *Analyzer {
	return &Analyzer{
		mode:         mode,
		cfg:          cfg,
		ob:           NewOrderBookAnalyzer(cfg.LargeOrderThresholdUSD, logger),
		tf:           NewTradeFlowAnalyzer(cfg.TradeFlowWindows, logger),
		vol:          NewVolumeAnalyzer(s, logger),
		spoof:        NewSpoofDetector(cfg.LargeOrderThresholdUSD, cfg.SpoofMaxLifetime, logger),
		store:        s,
		prices:       make(map[string][]pricePoint),
		fundingCache: make(map[string]cachedFloat),
		consolCache:  make(map[string]cachedBool),
		spoofEvents:  make(map[string][]detectedSpoof),
		lastEmit:     make(map[string]time.Time),
		out:          make(chan models.AnalyzerOutput, 100),
		logger:       logger,
	}
}

func (a *Analyzer) Run(ctx context.Context, in <-chan models.MarketEvent) <-chan models.AnalyzerOutput {
	go func() {
		defer close(a.out)
		for {
			select {
			case event, ok := <-in:
				if !ok {
					return
				}
				a.process(event)
				if a.shouldEmit(event.Symbol, event.Timestamp) {
					output := a.buildOutput(ctx, event.Symbol, event.Timestamp)
					select {
					case a.out <- output:
					case <-ctx.Done():
						return
					}
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return a.out
}

func (a *Analyzer) process(event models.MarketEvent) {
	switch event.EventType {
	case models.EventDepth:
		a.ob.Process(event)

		// Spoof taramasi icin order book'u al.
		a.ob.mu.RLock()
		book := a.ob.books[event.Symbol]
		a.ob.mu.RUnlock()
		if book != nil {
			spoofEvents := a.spoof.Scan(event.Symbol, book)
			if len(spoofEvents) > 0 {
				a.recordSpoofs(event.Symbol, book.Timestamp, spoofEvents)
				a.logger.Info("spoof tespit edildi",
					zap.String("symbol", event.Symbol),
					zap.Int("adet", len(spoofEvents)),
				)
			}
		}

	case models.EventTrade:
		a.tf.Process(event)

		// Hacim ve fiyat takibi.
		var tp tradePayload
		if err := json.Unmarshal(event.Payload, &tp); err == nil {
			a.vol.AddVolume(event.Symbol, tp.Quantity, event.Timestamp)
			a.pricesMu.Lock()
			a.prices[event.Symbol] = append(a.prices[event.Symbol], pricePoint{
				Price: tp.Price,
				Time:  event.Timestamp,
			})
			cutoff := event.Timestamp.Add(-15 * time.Minute)
			pts := a.prices[event.Symbol]
			idx := 0
			for idx < len(pts) && pts[idx].Time.Before(cutoff) {
				idx++
			}
			a.prices[event.Symbol] = pts[idx:]
			a.pricesMu.Unlock()
		}
	}
}

func (a *Analyzer) shouldEmit(symbol string, ts time.Time) bool {
	last, ok := a.lastEmit[symbol]
	if !ok || ts.Sub(last) >= a.cfg.EmitInterval {
		a.lastEmit[symbol] = ts
		return true
	}
	return false
}

func (a *Analyzer) buildOutput(ctx context.Context, symbol string, at time.Time) models.AnalyzerOutput {
	obMetrics := a.ob.Metrics(symbol)
	obMetrics.SpoofSuspects = a.activeSpoofs(symbol, at)
	metricAt := a.metricTime(at)

	var allWindows []models.TradeFlowWindow
	for _, dur := range a.cfg.TradeFlowWindows {
		allWindows = append(allWindows, a.tf.WindowAt(symbol, dur, at))
	}

	var tfWindow models.TradeFlowWindow
	if len(allWindows) > 0 {
		tfWindow = allWindows[0]
	}

	threshold := 0.05
	if a.cfg.ConsolidationThreshold > 0 {
		threshold = a.cfg.ConsolidationThreshold
	}

	return models.AnalyzerOutput{
		Timestamp:        at,
		OrderBookMetrics: obMetrics,
		TradeFlow:        tfWindow,
		TradeFlowWindows: allWindows,
		VolumeRatio:      a.vol.VolumeRatio(ctx, symbol, 7, metricAt),
		IsConsolidating:  a.isConsolidating(ctx, symbol, metricAt, threshold),
		PriceChange:      a.calculatePriceChange(symbol),
		FundingRate:      a.fundingRate(ctx, symbol, metricAt),
		MidPrice:         a.ob.MidPrice(symbol),
	}
}

func (a *Analyzer) calculatePriceChange(symbol string) float64 {
	a.pricesMu.RLock()
	defer a.pricesMu.RUnlock()

	pts := a.prices[symbol]
	if len(pts) < 2 {
		return 0
	}

	first := pts[0].Price
	last := pts[len(pts)-1].Price
	if first == 0 {
		return 0
	}
	return (last - first) / first
}

func (a *Analyzer) fundingRate(ctx context.Context, symbol string, at time.Time) float64 {
	bucket := at.Truncate(10 * time.Minute)

	// Cache kontrol (mutex ile — background refresh goroutine yazabilir)
	a.cacheMu.RLock()
	cached, ok := a.fundingCache[symbol]
	a.cacheMu.RUnlock()
	if ok && cached.Bucket.Equal(bucket) {
		return cached.Value
	}

	// DB varsa DB'den cek
	if a.store != nil {
		rate, err := a.store.FetchFundingRateAt(ctx, symbol, at)
		if err != nil {
			a.logger.Warn("funding rate alinamadi",
				zap.String("symbol", symbol),
				zap.Error(err),
			)
		} else {
			a.cacheMu.Lock()
			a.fundingCache[symbol] = cachedFloat{Bucket: bucket, Value: rate}
			a.cacheMu.Unlock()
			return rate
		}
	}

	// DB yok veya hata — stale cache varsa onu kullan (Binance API'den geldiginde)
	if ok {
		return cached.Value
	}
	return 0
}

func (a *Analyzer) isConsolidating(ctx context.Context, symbol string, at time.Time, threshold float64) bool {
	bucket := at.UTC().Truncate(24 * time.Hour)

	a.cacheMu.RLock()
	cached, ok := a.consolCache[symbol]
	a.cacheMu.RUnlock()
	if ok && cached.Bucket.Equal(bucket) {
		return cached.Value
	}

	if a.store != nil {
		cons, err := a.store.IsConsolidatingAt(ctx, symbol, a.cfg.ConsolidationDays, threshold, at)
		if err != nil {
			a.logger.Warn("konsolidasyon hesaplanamadi",
				zap.String("symbol", symbol),
				zap.Error(err),
			)
		} else {
			a.cacheMu.Lock()
			a.consolCache[symbol] = cachedBool{Bucket: bucket, Value: cons}
			a.cacheMu.Unlock()
			return cons
		}
	}

	// Stale cache (Binance API'den)
	if ok {
		return cached.Value
	}
	return false
}

func (a *Analyzer) metricTime(at time.Time) time.Time {
	if a.mode == "backtest" && !at.IsZero() {
		return at
	}
	return time.Now()
}

func (a *Analyzer) recordSpoofs(symbol string, detectedAt time.Time, events []models.SpoofEvent) {
	if len(events) == 0 {
		return
	}

	retention := maxDuration(a.cfg.EmitInterval*2, a.cfg.SpoofMaxLifetime*2, time.Minute)

	a.spoofMu.Lock()
	defer a.spoofMu.Unlock()

	for _, event := range events {
		a.spoofEvents[symbol] = append(a.spoofEvents[symbol], detectedSpoof{
			DetectedAt: detectedAt,
			Event:      event,
		})
	}

	cutoff := detectedAt.Add(-retention)
	current := a.spoofEvents[symbol]
	idx := 0
	for idx < len(current) && current[idx].DetectedAt.Before(cutoff) {
		idx++
	}
	a.spoofEvents[symbol] = current[idx:]
}

func (a *Analyzer) activeSpoofs(symbol string, at time.Time) []models.SpoofEvent {
	window := a.cfg.EmitInterval
	if window <= 0 {
		window = time.Second
	}
	cutoff := at.Add(-window)

	a.spoofMu.Lock()
	defer a.spoofMu.Unlock()

	current := a.spoofEvents[symbol]
	idx := 0
	for idx < len(current) && current[idx].DetectedAt.Before(cutoff) {
		idx++
	}
	current = current[idx:]
	a.spoofEvents[symbol] = current

	active := make([]models.SpoofEvent, 0, len(current))
	for _, spoof := range current {
		if !spoof.DetectedAt.After(at) {
			active = append(active, spoof.Event)
		}
	}
	return active
}

// LoadVolumes — baslangicta tum semboller icin ortalama hacmi yukler.
func (a *Analyzer) LoadVolumes(ctx context.Context, symbols []string) {
	if a.mode == "backtest" {
		return
	}
	for _, sym := range symbols {
		if err := a.vol.LoadAvgVolume(ctx, sym); err != nil {
			a.logger.Warn("hacim yukleme hatasi",
				zap.String("symbol", sym),
				zap.Error(err),
			)
		}
	}
}

func maxDuration(values ...time.Duration) time.Duration {
	var max time.Duration
	for _, value := range values {
		if value > max {
			max = value
		}
	}
	return max
}
