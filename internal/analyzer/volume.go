package analyzer

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/deep-trader/internal/store"
)

type VolumeAnalyzer struct {
	store      *store.Store
	avgVolumes map[string]float64 // symbol -> 7g ortalama hacim (per 1m kline)
	curVolumes map[string]float64 // symbol -> guncel hacim (1dk pencere icinde)
	lastReset  map[string]time.Time
	mu         sync.RWMutex
	logger     *zap.Logger
}

func NewVolumeAnalyzer(s *store.Store, logger *zap.Logger) *VolumeAnalyzer {
	return &VolumeAnalyzer{
		store:      s,
		avgVolumes: make(map[string]float64),
		curVolumes: make(map[string]float64),
		lastReset:  make(map[string]time.Time),
		logger:     logger,
	}
}

// LoadAvgVolume — belirli sembol icin 7 gunluk ortalama hacmi DB'den yukler.
func (va *VolumeAnalyzer) LoadAvgVolume(ctx context.Context, symbol string) error {
	avg, err := va.store.FetchAvgVolume(ctx, symbol, 7)
	if err != nil {
		return err
	}

	va.mu.Lock()
	va.avgVolumes[symbol] = avg
	va.mu.Unlock()

	va.logger.Debug("ortalama hacim yuklendi",
		zap.String("symbol", symbol),
		zap.Float64("avg_7d", avg),
	)
	return nil
}

// AddVolume — guncel hacme ekleme yapar. 1dk pencere dolunca sifirlar (klines_1m ile uyumlu).
func (va *VolumeAnalyzer) AddVolume(symbol string, quantity float64, eventTime time.Time) {
	va.mu.Lock()
	defer va.mu.Unlock()

	last, ok := va.lastReset[symbol]
	if !ok || eventTime.Sub(last) >= time.Minute {
		va.curVolumes[symbol] = 0
		va.lastReset[symbol] = eventTime
	}
	va.curVolumes[symbol] += quantity
}

// VolumeRatio — guncel hacim / 7 gunluk ortalama oranini dondurur.
// > 5.0 → Hacim patladi (onemli hareket)
func (va *VolumeAnalyzer) VolumeRatio(symbol string) float64 {
	va.mu.RLock()
	defer va.mu.RUnlock()

	avg := va.avgVolumes[symbol]
	if avg == 0 {
		return 1.0
	}
	return va.curVolumes[symbol] / avg
}
