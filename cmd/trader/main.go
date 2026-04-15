package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/deep-trader/internal/analyzer"
	"github.com/deep-trader/internal/config"
	"github.com/deep-trader/internal/executor"
	"github.com/deep-trader/internal/models"
	"github.com/deep-trader/internal/router"
	signalengine "github.com/deep-trader/internal/signal"
	"github.com/deep-trader/internal/store"
	"github.com/deep-trader/internal/web"
)

func main() {
	// Flags
	mode := flag.String("mode", "", "Calisma modu: backtest | paper | live")
	configPath := flag.String("config", "config/config.yaml", "Config dosya yolu")
	startTime := flag.String("start", "", "Backtest baslangic zamani (YYYY-MM-DD)")
	endTime := flag.String("end", "", "Backtest bitis zamani (YYYY-MM-DD)")
	flag.Parse()

	// Logger
	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "logger olusturma hatasi: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync()

	// Config
	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Fatal("config yukleme hatasi", zap.Error(err))
	}

	// Flag ile mode override
	if *mode != "" {
		cfg.Mode = *mode
	}

	logger.Info("Deep Trader baslatiliyor",
		zap.String("mod", cfg.Mode),
	)

	// Context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logger.Info("kapatma sinyali alindi", zap.String("sinyal", sig.String()))
		cancel()
	}()

	// TimescaleDB — opsiyonel (live mod DB'siz calisabilir)
	var db *store.Store
	if cfg.Database.Host != "" {
		var dbErr error
		db, dbErr = store.New(ctx, cfg.Database, logger)
		if dbErr != nil {
			if cfg.Mode == "live" {
				logger.Warn("DB baglantisi kurulamadi, DB'siz devam ediliyor", zap.Error(dbErr))
			} else {
				logger.Fatal("DB baglanti hatasi", zap.Error(dbErr))
			}
		}
	} else {
		logger.Info("DB yapilandirilmamis, DB'siz mod")
	}
	if db != nil {
		defer db.Close()
	}

	// Backtest tablosunu olustur
	if cfg.Mode == "backtest" && db != nil {
		if err := db.EnsureBacktestTable(ctx); err != nil {
			logger.Fatal("backtest tablosu olusturma hatasi", zap.Error(err))
		}
	}

	// Backtest zamani once netlestirilir; sembol evreni buna gore secilir.
	backtestStart := cfg.Backtest.StartTime
	backtestEnd := cfg.Backtest.EndTime
	if *startTime != "" {
		if t, err := parseTime(*startTime); err == nil {
			backtestStart = t
		}
	}
	if *endTime != "" {
		if t, err := parseTime(*endTime); err == nil {
			backtestEnd = t
		}
	}
	if cfg.Mode == "backtest" {
		if backtestStart.IsZero() || backtestEnd.IsZero() || !backtestEnd.After(backtestStart) {
			logger.Fatal("gecersiz backtest zamani",
				zap.Time("start", backtestStart),
				zap.Time("end", backtestEnd),
			)
		}
	}

	var backtestUniverse []models.SymbolActivation
	var symbols []string
	switch cfg.Mode {
	case "backtest":
		if db == nil {
			logger.Fatal("backtest modu icin DB gerekli")
		}
		backtestUniverse, err = db.FetchBacktestUniverse(ctx, backtestStart, backtestEnd)
		if err != nil {
			logger.Fatal("backtest sembol evreni alinamadi", zap.Error(err))
		}
		if cfg.Symbols.MaxSymbols > 0 && len(backtestUniverse) > cfg.Symbols.MaxSymbols {
			backtestUniverse = backtestUniverse[:cfg.Symbols.MaxSymbols]
		}
		for _, activation := range backtestUniverse {
			symbols = append(symbols, activation.Symbol)
		}
		if len(symbols) == 0 {
			logger.Fatal("backtest araliginda hicbir sembol icin depth verisi bulunamadi")
		}

	case "paper":
		if db == nil {
			logger.Fatal("paper modu icin DB gerekli (live modu kullanin)")
		}
		symbols, err = db.FetchActiveSymbols(ctx)
		if err != nil {
			logger.Fatal("aktif sembol listesi alinamadi", zap.Error(err))
		}
		if len(symbols) == 0 {
			logger.Fatal("hicbir sembol icin depth verisi bulunamadi")
		}
		if cfg.Symbols.MaxSymbols > 0 && len(symbols) > cfg.Symbols.MaxSymbols {
			symbols = symbols[:cfg.Symbols.MaxSymbols]
		}

	case "live":
		// Oncelik: config listesi > DB > Binance API
		if len(cfg.Symbols.List) > 0 {
			symbols = cfg.Symbols.List
			logger.Info("semboller config'den yuklendi", zap.Int("adet", len(symbols)))
		} else if db != nil {
			symbols, _ = db.FetchActiveSymbols(ctx)
		}
		if len(symbols) == 0 {
			logger.Info("DB'den sembol alinamadi, Binance API'den cekiliyor...")
			symbols, err = fetchSymbolsFromBinance(cfg.Symbols.MinMarketCapUSD, cfg.Symbols.MaxSymbols, logger)
			if err != nil {
				logger.Fatal("Binance API'den sembol alinamadi", zap.Error(err))
			}
		}
		if cfg.Symbols.MaxSymbols > 0 && len(symbols) > cfg.Symbols.MaxSymbols {
			symbols = symbols[:cfg.Symbols.MaxSymbols]
		}
	}

	logger.Info("aktif semboller yuklendi",
		zap.Int("adet", len(symbols)),
		zap.Strings("ilk_10", firstN(symbols, 10)),
	)

	// Web Dashboard
	webPort := cfg.Web.Port
	if webPort == 0 {
		webPort = 8888
	}
	leverage := cfg.Executor.Leverage
	if leverage <= 0 {
		leverage = 1
	}
	dashboardSymbols := symbols
	if cfg.Mode == "backtest" {
		dashboardSymbols = nil
	}
	dashboard := web.NewDashboard(webPort, cfg.Mode, dashboardSymbols, cfg.Executor.InitialBalanceUSD, leverage, cfg.Executor.TakerFeePct, logger)
	if err := dashboard.Start(); err != nil {
		logger.Fatal("dashboard baslama hatasi", zap.Error(err))
	}
	defer dashboard.Stop()

	// Data Router
	var dr router.DataRouter
	switch cfg.Mode {
	case "backtest":
		dr = router.NewBacktestRouter(db.Pool(), backtestUniverse, backtestStart, backtestEnd, cfg.Backtest.Speed, logger)

	case "paper":
		dr = router.NewPaperRouter(db.Pool(), symbols, cfg.Analyzer, logger)

	case "live":
		dr = router.NewLiveRouter(cfg.WebSocket, symbols, logger)

	default:
		logger.Fatal("gecersiz mod", zap.String("mod", cfg.Mode))
	}

	// Signal Engine (tracker icin erken olustur)
	se := signalengine.NewEngine(cfg.Signal, cfg.Executor.TakerFeePct, logger)

	// Router baslat
	events, err := dr.Start(ctx)
	if err != nil {
		logger.Fatal("router baslama hatasi", zap.Error(err))
	}
	defer dr.Stop()

	// Event'leri dashboard'a da ilet + fiyat guncelle
	eventsCh := make(chan models.MarketEvent, 1000)
	go func() {
		defer close(eventsCh)
		for e := range events {
			dashboard.ObserveEvent(e.Symbol, e.Timestamp)

			// Trade event'lerinden guncel fiyati cek
			if e.EventType == models.EventTrade {
				var tp struct {
					Price float64 `json:"price"`
				}
				if err := json.Unmarshal(e.Payload, &tp); err == nil && tp.Price > 0 {
					dashboard.UpdatePrice(e.Symbol, tp.Price)
					se.Tracker().UpdatePrice(e.Symbol, tp.Price, e.Timestamp)
				}
			}

			select {
			case eventsCh <- e:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Analyzer
	a := analyzer.New(cfg.Mode, cfg.Analyzer, db, logger)
	if db != nil && cfg.Mode != "backtest" {
		a.LoadVolumes(ctx, symbols)
	} else if db == nil && cfg.Mode == "live" {
		// DB yoksa Binance REST API'den funding rate, avg volume, consolidation yukle
		a.LoadMarketDataFromBinanceAPI(ctx, symbols)
	}
	analyzerOut := a.Run(ctx, eventsCh)
	signals := se.Run(ctx, analyzerOut)

	// Executor — tum modlarda PaperExecutor kullanilir
	runID := fmt.Sprintf("%s_%d", cfg.Mode, time.Now().UnixMilli())
	runMode := cfg.Mode

	logger.Info("run baslatiliyor",
		zap.String("run_id", runID),
		zap.String("run_mode", runMode),
	)

	// Paper tablolari (DB varsa)
	if db != nil {
		if err := db.EnsurePaperTables(ctx); err != nil {
			logger.Warn("paper tablolari olusturulamadi", zap.Error(err))
		}
	}

	hooks := &executor.PaperHooks{
		OnSignal: func(s models.SignalEvent) {
			dashboard.AddSignal(s)
			if db != nil {
				if err := db.SavePaperSignal(ctx, runID, runMode, s); err != nil {
					logger.Error("sinyal kayit hatasi", zap.Error(err))
				}
			}
		},
		OnPositionOpen:  func(sym string, pos *models.Position) { dashboard.UpdatePosition(sym, pos) },
		OnPositionClose: func(sym string) { dashboard.UpdatePosition(sym, nil) },
		OnTrade: func(t models.PaperTrade) {
			dashboard.AddTrade(t)
			if db != nil {
				if err := db.SavePaperTrade(ctx, runID, runMode, t); err != nil {
					logger.Error("trade kayit hatasi", zap.Error(err))
				}
			}
		},
		OnBalanceChange: func(bal float64) { dashboard.UpdateBalance(bal) },
		OnTrackPosition: func(sym, side string, sig models.SignalType, score float64, entryPrice float64, qty float64, lev int, entryTime time.Time) {
			se.Tracker().TrackPosition(sym, side, sig, score, entryPrice, qty, lev, entryTime)
		},
		OnUntrackPosition: func(sym string, reason string, eventTime time.Time) {
			se.Tracker().UntrackPosition(sym, reason, eventTime)
		},
		GetCurrentPrice: func(sym string) float64 { return dashboard.GetPrice(sym) },
	}
	exec := executor.NewPaperExecutor(cfg.Executor, hooks, logger)
	defer exec.Close()

	// Ana dongu
	logger.Info("sistem hazir, sinyaller bekleniyor...",
		zap.String("dashboard", fmt.Sprintf("http://0.0.0.0:%d", webPort)),
	)
	for {
		select {
		case sig, ok := <-signals:
			if !ok {
				logger.Info("sinyal kanali kapandi, cikiliyor")
				return
			}
			if err := exec.Execute(ctx, sig); err != nil {
				logger.Error("executor hatasi",
					zap.String("symbol", sig.Symbol),
					zap.Error(err),
				)
			}
		case <-ctx.Done():
			logger.Info("Deep Trader kapatiliyor...")
			return
		}
	}
}

// fetchSymbolsFromBinance — Binance Futures API'den aktif sembolleri getirir.
// 24h hacme gore filtreler ve siralar.
func fetchSymbolsFromBinance(minVolumeUSD float64, maxSymbols int, logger *zap.Logger) ([]string, error) {
	resp, err := http.Get("https://fapi.binance.com/fapi/v1/ticker/24hr")
	if err != nil {
		return nil, fmt.Errorf("binance API hatasi: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("response okuma hatasi: %w", err)
	}

	var tickers []struct {
		Symbol      string `json:"symbol"`
		QuoteVolume string `json:"quoteVolume"`
	}
	if err := json.Unmarshal(body, &tickers); err != nil {
		return nil, fmt.Errorf("JSON parse hatasi: %w", err)
	}

	type symbolVol struct {
		Symbol string
		Volume float64
	}

	var filtered []symbolVol
	for _, t := range tickers {
		sym := t.Symbol

		// Stablecoin ve leverage token filtrele
		if strings.HasSuffix(sym, "BUSD") || strings.HasSuffix(sym, "USDC") ||
			strings.HasSuffix(sym, "DAI") || strings.HasSuffix(sym, "TUSD") {
			continue
		}
		if strings.HasSuffix(sym, "UP") || strings.HasSuffix(sym, "DOWN") ||
			strings.Contains(sym, "BULL") || strings.Contains(sym, "BEAR") {
			continue
		}
		// Sadece USDT perpetual
		if !strings.HasSuffix(sym, "USDT") {
			continue
		}

		vol, _ := strconv.ParseFloat(t.QuoteVolume, 64)
		if vol >= minVolumeUSD {
			filtered = append(filtered, symbolVol{Symbol: sym, Volume: vol})
		}
	}

	// Hacme gore azalan sirala
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].Volume > filtered[j].Volume
	})

	if maxSymbols > 0 && len(filtered) > maxSymbols {
		filtered = filtered[:maxSymbols]
	}

	symbols := make([]string, len(filtered))
	for i, sv := range filtered {
		symbols[i] = sv.Symbol
	}

	logger.Info("Binance API'den semboller yuklendi",
		zap.Int("toplam", len(tickers)),
		zap.Int("filtrelenmis", len(symbols)),
		zap.Float64("min_volume_usd", minVolumeUSD),
	)

	return symbols, nil
}

func firstN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		t, err = time.Parse(time.RFC3339, s)
	}
	return t, err
}
