package analyzer

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/deep-trader/internal/store"
)

type avgVolumeCacheEntry struct {
	Bucket time.Time
	Value  float64
}

type VolumeAnalyzer struct {
	store      *store.Store
	avgVolumes map[string]avgVolumeCacheEntry // symbol -> zaman bucket'li ortalama hacim
	curVolumes map[string]float64             // symbol -> guncel hacim (1dk pencere icinde)
	lastReset  map[string]time.Time
	mu         sync.RWMutex
	logger     *zap.Logger
}

func NewVolumeAnalyzer(s *store.Store, logger *zap.Logger) *VolumeAnalyzer {
	return &VolumeAnalyzer{
		store:      s,
		avgVolumes: make(map[string]avgVolumeCacheEntry),
		curVolumes: make(map[string]float64),
		lastReset:  make(map[string]time.Time),
		logger:     logger,
	}
}

// LoadAvgVolume — live/paper icin mevcut saat bucket'inda ortalama hacmi pre-warm eder.
func (va *VolumeAnalyzer) LoadAvgVolume(ctx context.Context, symbol string) error {
	_, err := va.avgVolumeAt(ctx, symbol, 7, time.Now())
	return err
}

// AddVolume — guncel hacme ekleme yapar.
// Paper (main branch) ile ayni davranis: reset YOK, surekli birikir.
// Bu, VolumeRatio'nun zamanla artmasini saglar → TREND_FOLLOW ve PUMP
// sinyallerinin volume kriterlerini karsilamasini kolaylastirir.
func (va *VolumeAnalyzer) AddVolume(symbol string, quantity float64, eventTime time.Time) {
	va.mu.Lock()
	va.curVolumes[symbol] += quantity
	va.mu.Unlock()
}

// VolumeRatio — guncel hacim / 7 gunluk ortalama oranini dondurur.
func (va *VolumeAnalyzer) VolumeRatio(ctx context.Context, symbol string, days int, at time.Time) float64 {
	va.mu.RLock()
	current := va.curVolumes[symbol]
	entry, ok := va.avgVolumes[symbol]
	va.mu.RUnlock()

	// Dogrudan cache'den oku — avgVolumeAt'in karmasik bucket/DB mantigi yerine
	// Binance API veya DB startup'ta doldurur, sonra stale kullanilir
	avg := 0.0
	if ok {
		avg = entry.Value
	} else if va.store != nil {
		// DB varsa DB'den cek (paper mode)
		var err error
		avg, err = va.avgVolumeAt(ctx, symbol, days, at)
		if err != nil {
			va.logger.Warn("ortalama hacim hesaplanamadi",
				zap.String("symbol", symbol),
				zap.Error(err),
			)
		}
	}

	if avg == 0 {
		if current > 0 {
			va.logger.Warn("VolumeRatio: avg=0 ama current>0",
				zap.String("symbol", symbol),
				zap.Float64("current", current),
				zap.Bool("cache_ok", ok),
			)
		}
		return 1.0
	}
	return current / avg
}

func (va *VolumeAnalyzer) currentVolume(symbol string) float64 {
	va.mu.RLock()
	defer va.mu.RUnlock()
	return va.curVolumes[symbol]
}

func (va *VolumeAnalyzer) avgVolumeAt(ctx context.Context, symbol string, days int, at time.Time) (float64, error) {
	if at.IsZero() {
		at = time.Now()
	}

	bucket := at.Truncate(time.Hour)

	va.mu.RLock()
	entry, ok := va.avgVolumes[symbol]
	va.mu.RUnlock()
	if ok && entry.Bucket.Equal(bucket) {
		return entry.Value, nil
	}

	// DB varsa DB'den cek
	if va.store != nil {
		avg, err := va.store.FetchAvgVolumeAt(ctx, symbol, days, at)
		if err != nil {
			// DB hatasi — stale cache varsa kullan
			if ok {
				return entry.Value, nil
			}
			return 0, err
		}

		va.mu.Lock()
		va.avgVolumes[symbol] = avgVolumeCacheEntry{
			Bucket: bucket,
			Value:  avg,
		}
		va.mu.Unlock()

		va.logger.Debug("ortalama hacim yuklendi",
			zap.String("symbol", symbol),
			zap.Float64("avg_7d", avg),
			zap.Time("bucket", bucket),
		)
		return avg, nil
	}

	// DB yok — stale cache varsa kullan (Binance API'den yuklenmis olabilir)
	if ok {
		return entry.Value, nil
	}
	return 0, nil
}
