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

// refreshKlineData — her sembol icin:
// - 1m klines (7 gun, gunluk 1440 mum) → AVG(volume) — paper DB ile birebir ayni
// - 1d klines (7 adet) → konsolidasyon (high/low range)
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
	sem := make(chan struct{}, 5) // rate limit icin 5 concurrent (7 gun × sembol cagrisi)

	for _, sym := range symbols {
		wg.Add(1)
		sem <- struct{}{}
		go func(symbol string) {
			defer wg.Done()
			defer func() { <-sem }()

			// 1. 1m klines — gun gun cek, AVG(volume) hesapla
			// Paper DB: SELECT AVG(volume) FROM klines_1m WHERE time >= NOW()-7d
			// Binance: 7 × GET /fapi/v1/klines?interval=1m&limit=1440&startTime=...
			var totalVol float64
			var totalCandles int
			for d := days; d > 0; d-- {
				dayStart := now.AddDate(0, 0, -d)
				startMs := dayStart.UnixMilli()
				klines, err := fetchBinanceKlinesWithStart(ctx, symbol, "1m", 1440, startMs)
				if err != nil {
					a.logger.Debug("Binance 1m kline hatasi",
						zap.String("symbol", symbol),
						zap.Int("gun", d),
						zap.Error(err),
					)
					continue
				}
				for _, k := range klines {
					totalVol += k.Volume
					totalCandles++
				}
			}

			if totalCandles > 0 {
				avgPerMinute := totalVol / float64(totalCandles)

				a.vol.mu.Lock()
				a.vol.avgVolumes[symbol] = avgVolumeCacheEntry{
					Bucket: volBucket,
					Value:  avgPerMinute,
				}
				// Pre-seed curVolumes: paper 14+ saat birikiyor → VolumeRatio 50-200x
				// Baslangicta 60dk'lik birikimi simule et → VolumeRatio ~60x
				// Bu, PUMP'in +0.30 volume bonusunu ve TREND_FOLLOW'in +0.15 bonusunu aktif eder
				if a.vol.curVolumes[symbol] == 0 {
					a.vol.curVolumes[symbol] = avgPerMinute * 60 // 60dk birikim
				}
				a.vol.mu.Unlock()
			}

			// 2. Daily klines → konsolidasyon (max high - min low)
			dailyKlines, err := fetchBinanceKlines(ctx, symbol, "1d", days)
			if err != nil {
				a.logger.Debug("Binance daily kline hatasi",
					zap.String("symbol", symbol),
					zap.Error(err),
				)
				return
			}
			if len(dailyKlines) > 0 {
				maxHigh := 0.0
				minLow := math.MaxFloat64
				for _, k := range dailyKlines {
					if k.High > maxHigh {
						maxHigh = k.High
					}
					if k.Low < minLow && k.Low > 0 {
						minLow = k.Low
					}
				}
				isConsol := false
				if minLow > 0 && minLow < math.MaxFloat64 {
					rangePct := (maxHigh - minLow) / minLow
					isConsol = rangePct < threshold
				}

				a.cacheMu.Lock()
				a.consolCache[symbol] = cachedBool{
					Bucket: consolBucket,
					Value:  isConsol,
				}
				a.cacheMu.Unlock()
			}
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

// fetchBinanceKlinesWithStart — startTime parametreli kline cagrisi
func fetchBinanceKlinesWithStart(ctx context.Context, symbol, interval string, limit int, startTimeMs int64) ([]binanceKline, error) {
	url := fmt.Sprintf("https://fapi.binance.com/fapi/v1/klines?symbol=%s&interval=%s&limit=%d&startTime=%d", symbol, interval, limit, startTimeMs)
	return doBinanceKlinesRequest(ctx, url)
}

// fetchBinanceKlines — GET /fapi/v1/klines?symbol=X&interval=INTERVAL&limit=N
func fetchBinanceKlines(ctx context.Context, symbol, interval string, limit int) ([]binanceKline, error) {
	url := fmt.Sprintf("https://fapi.binance.com/fapi/v1/klines?symbol=%s&interval=%s&limit=%d", symbol, interval, limit)
	return doBinanceKlinesRequest(ctx, url)
}

func doBinanceKlinesRequest(ctx context.Context, url string) ([]binanceKline, error) {

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
