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

type Analyzer struct {
	cfg   config.AnalyzerConfig
	ob    *OrderBookAnalyzer
	tf    *TradeFlowAnalyzer
	vol   *VolumeAnalyzer
	spoof *SpoofDetector
	store *store.Store

	// Fiyat takibi (price change hesabi icin)
	prices    map[string][]pricePoint
	pricesMu  sync.RWMutex

	// Cache (DB sorgularini azaltmak icin)
	fundingCache   map[string]float64
	consolCache    map[string]bool
	cacheTime      time.Time
	cacheDuration  time.Duration

	lastEmit map[string]time.Time
	out      chan models.AnalyzerOutput
	logger   *zap.Logger
}

type pricePoint struct {
	Price float64
	Time  time.Time
}

func New(cfg config.AnalyzerConfig, s *store.Store, logger *zap.Logger) *Analyzer {
	return &Analyzer{
		cfg:           cfg,
		ob:            NewOrderBookAnalyzer(cfg.LargeOrderThresholdUSD, logger),
		tf:            NewTradeFlowAnalyzer(cfg.TradeFlowWindows, logger),
		vol:           NewVolumeAnalyzer(s, logger),
		spoof:         NewSpoofDetector(cfg.LargeOrderThresholdUSD, cfg.SpoofMaxLifetime, logger),
		store:         s,
		prices:        make(map[string][]pricePoint),
		fundingCache:  make(map[string]float64),
		consolCache:   make(map[string]bool),
		cacheDuration: 10 * time.Minute,
		lastEmit:      make(map[string]time.Time),
		out:           make(chan models.AnalyzerOutput, 100),
		logger:        logger,
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
					output := a.buildOutput(ctx, event.Symbol)
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
		// Spoof taramasi icin order book'u al
		a.ob.mu.RLock()
		book := a.ob.books[event.Symbol]
		a.ob.mu.RUnlock()
		if book != nil {
			spoofEvents := a.spoof.Scan(event.Symbol, book)
			if len(spoofEvents) > 0 {
				a.logger.Info("spoof tespit edildi",
					zap.String("symbol", event.Symbol),
					zap.Int("adet", len(spoofEvents)),
				)
			}
		}

	case models.EventTrade:
		a.tf.Process(event)

		// Hacim guncelle
		var tp tradePayload
		if err := json.Unmarshal(event.Payload, &tp); err == nil {
			a.vol.AddVolume(event.Symbol, tp.Quantity, event.Timestamp)
			// Fiyat takibi
			a.pricesMu.Lock()
			a.prices[event.Symbol] = append(a.prices[event.Symbol], pricePoint{
				Price: tp.Price,
				Time:  event.Timestamp,
			})
			// Son 15dk'dan eskilerini temizle
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

func (a *Analyzer) buildOutput(ctx context.Context, symbol string) models.AnalyzerOutput {
	obMetrics := a.ob.Metrics(symbol)

	// Spoof suspectlerini ekle
	// (son scan'den gelen suspects ob metrics icerisinde saklanmaz,
	// burada tekrar kontrol etmiyoruz — Scan cagrisinda loglanir)

	// Tum trade flow pencerelerini topla
	var allWindows []models.TradeFlowWindow
	for _, dur := range a.cfg.TradeFlowWindows {
		allWindows = append(allWindows, a.tf.Window(symbol, dur))
	}
	var tfWindow models.TradeFlowWindow
	if len(allWindows) > 0 {
		tfWindow = allWindows[0]
	}

	// Fiyat degisimi hesapla
	priceChange := a.calculatePriceChange(symbol)

	// Cache'i yenile (10dk'da bir)
	now := time.Now()
	if now.Sub(a.cacheTime) > a.cacheDuration {
		a.cacheTime = now
		a.fundingCache = make(map[string]float64)
		a.consolCache = make(map[string]bool)
	}

	// Funding rate (cache'den veya DB'den)
	fundingRate := 0.0
	if cached, ok := a.fundingCache[symbol]; ok {
		fundingRate = cached
	} else if a.store != nil {
		if rate, err := a.store.FetchFundingRate(ctx, symbol); err == nil {
			fundingRate = rate
			a.fundingCache[symbol] = rate
		}
	}

	// Konsolidasyon kontrolu (cache'den veya DB'den)
	isConsolidating := false
	if cached, ok := a.consolCache[symbol]; ok {
		isConsolidating = cached
	} else if a.store != nil {
		threshold := 0.05
		if a.cfg.ConsolidationThreshold > 0 {
			threshold = a.cfg.ConsolidationThreshold
		}
		if cons, err := a.store.IsConsolidating(ctx, symbol, a.cfg.ConsolidationDays, threshold); err == nil {
			isConsolidating = cons
			a.consolCache[symbol] = cons
		}
	}

	return models.AnalyzerOutput{
		OrderBookMetrics: obMetrics,
		TradeFlow:        tfWindow,
		TradeFlowWindows: allWindows,
		VolumeRatio:      a.vol.VolumeRatio(symbol),
		IsConsolidating:  isConsolidating,
		PriceChange:      priceChange,
		FundingRate:      fundingRate,
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

// LoadVolumes — baslangicta tum semboller icin ortalama hacim yukler.
func (a *Analyzer) LoadVolumes(ctx context.Context, symbols []string) {
	for _, sym := range symbols {
		if err := a.vol.LoadAvgVolume(ctx, sym); err != nil {
			a.logger.Warn("hacim yukleme hatasi",
				zap.String("symbol", sym),
				zap.Error(err),
			)
		}
	}
}
