package analyzer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"
)

// binancePremiumIndex — /fapi/v1/premiumIndex response
type binancePremiumIndex struct {
	Symbol          string `json:"symbol"`
	LastFundingRate string `json:"lastFundingRate"`
}

// binanceKline — /fapi/v1/klines response (array of arrays)
// [openTime, open, high, low, close, volume, closeTime, quoteVolume, ...]
type binanceKline struct {
	High   float64
	Low    float64
	Volume float64
}

// LoadMarketDataFromBinanceAPI — DB yokken Binance REST API'den
// funding rate, ortalama hacim ve konsolidasyon verisini yukler.
// Bir background goroutine ile periyodik refresh yapar.
func (a *Analyzer) LoadMarketDataFromBinanceAPI(ctx context.Context, symbols []string) {
	a.logger.Info("Binance API'den market verisi yukleniyor...",
		zap.Int("sembol_sayisi", len(symbols)),
	)

	a.refreshMarketData(ctx, symbols)

	// Periyodik refresh: funding 10dk, klines 1 saat
	go func() {
		fundingTicker := time.NewTicker(10 * time.Minute)
		klineTicker := time.NewTicker(1 * time.Hour)
		defer fundingTicker.Stop()
		defer klineTicker.Stop()

		for {
			select {
			case <-fundingTicker.C:
				a.refreshFundingRates(ctx, symbols)
			case <-klineTicker.C:
				a.refreshKlineData(ctx, symbols)
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (a *Analyzer) refreshMarketData(ctx context.Context, symbols []string) {
	a.refreshFundingRates(ctx, symbols)
	a.refreshKlineData(ctx, symbols)
}

// refreshFundingRates — tum sembollerin funding rate'ini tek API call ile ceker
func (a *Analyzer) refreshFundingRates(ctx context.Context, symbols []string) {
	rates, err := fetchBinanceFundingRates(ctx)
	if err != nil {
		a.logger.Error("Binance funding rate hatasi", zap.Error(err))
		return
	}

	now := time.Now()
	bucket := now.Truncate(10 * time.Minute)
	count := 0

	a.cacheMu.Lock()
	for _, sym := range symbols {
		if rate, ok := rates[sym]; ok {
			a.fundingCache[sym] = cachedFloat{
				Bucket: bucket,
				Value:  rate,
			}
			count++
		}
	}
	a.cacheMu.Unlock()

	a.logger.Info("Binance funding rates yuklendi",
		zap.Int("yuklenen", count),
		zap.Int("toplam", len(rates)),
	)
}

// refreshKlineData — her sembol icin 7 gunluk daily kline'dan
// ortalama hacim + konsolidasyon hesaplar
func (a *Analyzer) refreshKlineData(ctx context.Context, symbols []string) {
	now := time.Now()
	consolBucket := now.UTC().Truncate(24 * time.Hour)
	volBucket := now.Truncate(time.Hour)

	threshold := 0.05
	if a.cfg.ConsolidationThreshold > 0 {
		threshold = a.cfg.ConsolidationThreshold
	}

	days := 7
	if a.cfg.ConsolidationDays > 0 {
		days = a.cfg.ConsolidationDays
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, 10) // max 10 concurrent requests

	for _, sym := range symbols {
		wg.Add(1)
		sem <- struct{}{}
		go func(symbol string) {
			defer wg.Done()
			defer func() { <-sem }()

			klines, err := fetchBinanceDailyKlines(ctx, symbol, days)
			if err != nil {
				a.logger.Debug("Binance kline hatasi",
					zap.String("symbol", symbol),
					zap.Error(err),
				)
				return
			}

			if len(klines) == 0 {
				return
			}

			// Ortalama hacim: toplam volume / gun sayisi / 1440 (dakika/gun)
			totalVol := 0.0
			maxHigh := 0.0
			minLow := math.MaxFloat64
			for _, k := range klines {
				totalVol += k.Volume
				if k.High > maxHigh {
					maxHigh = k.High
				}
				if k.Low < minLow && k.Low > 0 {
					minLow = k.Low
				}
			}

			avgPerMinute := totalVol / float64(len(klines)) / 1440.0

			// Konsolidasyon: (maxHigh - minLow) / minLow < threshold
			isConsol := false
			if minLow > 0 && minLow < math.MaxFloat64 {
				rangePct := (maxHigh - minLow) / minLow
				isConsol = rangePct < threshold
			}

			// Cache'lere yaz (mutex ile — concurrent goroutine'ler)
			a.vol.mu.Lock()
			a.vol.avgVolumes[symbol] = avgVolumeCacheEntry{
				Bucket: volBucket,
				Value:  avgPerMinute,
			}
			a.vol.mu.Unlock()

			a.cacheMu.Lock()
			a.consolCache[symbol] = cachedBool{
				Bucket: consolBucket,
				Value:  isConsol,
			}
			a.cacheMu.Unlock()
		}(sym)
	}

	wg.Wait()

	a.logger.Info("Binance kline verisi yuklendi",
		zap.Int("sembol_sayisi", len(symbols)),
	)
}

// fetchBinanceFundingRates — GET /fapi/v1/premiumIndex
func fetchBinanceFundingRates(ctx context.Context) (map[string]float64, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://fapi.binance.com/fapi/v1/premiumIndex", nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("premiumIndex hatasi: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var indices []binancePremiumIndex
	if err := json.Unmarshal(body, &indices); err != nil {
		return nil, fmt.Errorf("premiumIndex parse hatasi: %w", err)
	}

	rates := make(map[string]float64, len(indices))
	for _, idx := range indices {
		rate, _ := strconv.ParseFloat(idx.LastFundingRate, 64)
		rates[idx.Symbol] = rate
	}

	return rates, nil
}

// fetchBinanceDailyKlines — GET /fapi/v1/klines?symbol=X&interval=1d&limit=N
func fetchBinanceDailyKlines(ctx context.Context, symbol string, days int) ([]binanceKline, error) {
	url := fmt.Sprintf("https://fapi.binance.com/fapi/v1/klines?symbol=%s&interval=1d&limit=%d", symbol, days)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("klines hatasi: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Binance klines: [[openTime, open, high, low, close, volume, ...], ...]
	var raw [][]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("klines parse hatasi: %w", err)
	}

	klines := make([]binanceKline, 0, len(raw))
	for _, row := range raw {
		if len(row) < 6 {
			continue
		}
		var highStr, lowStr, volStr string
		json.Unmarshal(row[2], &highStr)
		json.Unmarshal(row[3], &lowStr)
		json.Unmarshal(row[5], &volStr)

		high, _ := strconv.ParseFloat(highStr, 64)
		low, _ := strconv.ParseFloat(lowStr, 64)
		vol, _ := strconv.ParseFloat(volStr, 64)

		klines = append(klines, binanceKline{High: high, Low: low, Volume: vol})
	}

	return klines, nil
}
