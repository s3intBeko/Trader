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
	avgVolumes map[string]float64 // symbol -> 7g ortalama hacim
	curVolumes map[string]float64 // symbol -> guncel hacim (pencere icinde)
	lastUpdate map[string]time.Time
	mu         sync.RWMutex
	logger     *zap.Logger
}

func NewVolumeAnalyzer(s *store.Store, logger *zap.Logger) *VolumeAnalyzer {
	return &VolumeAnalyzer{
		store:      s,
		avgVolumes: make(map[string]float64),
		curVolumes: make(map[string]float64),
		lastUpdate: make(map[string]time.Time),
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

// AddVolume — guncel hacme ekleme yapar.
func (va *VolumeAnalyzer) AddVolume(symbol string, quantity float64) {
	va.mu.Lock()
	va.curVolumes[symbol] += quantity
	va.mu.Unlock()
}

// ResetCurrent — guncel hacmi sifirlar (yeni pencere baslangici).
func (va *VolumeAnalyzer) ResetCurrent(symbol string) {
	va.mu.Lock()
	va.curVolumes[symbol] = 0
	va.mu.Unlock()
}

// VolumeRatio — guncel hacim / 7 gunluk ortalama oranini dondurur.
// > 5.0 → Hacim patladi (onemli hareket)
// avgVolumes DB veya Binance API tarafindan doldurulur.
func (va *VolumeAnalyzer) VolumeRatio(symbol string) float64 {
	va.mu.RLock()
	defer va.mu.RUnlock()

	avg := va.avgVolumes[symbol]
	if avg == 0 {
		return 1.0
	}
	return va.curVolumes[symbol] / avg
}
