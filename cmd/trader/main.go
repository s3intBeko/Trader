package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
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

	// TimescaleDB
	db, err := store.New(ctx, cfg.Database, logger)
	if err != nil {
		logger.Fatal("DB baglanti hatasi", zap.Error(err))
	}
	defer db.Close()

	// Backtest tablosunu olustur
	if cfg.Mode == "backtest" {
		if err := db.EnsureBacktestTable(ctx); err != nil {
			logger.Fatal("backtest tablosu olusturma hatasi", zap.Error(err))
		}
	}

	// Sembol listesi — DB'den depth verisi olan aktif sembolleri cek
	symbols, err := db.FetchActiveSymbols(ctx)
	if err != nil {
		logger.Fatal("aktif sembol listesi alinamadi", zap.Error(err))
	}
	if len(symbols) == 0 {
		logger.Fatal("hicbir sembol icin depth verisi bulunamadi")
	}

	// Max sembol limiti (config'den)
	if cfg.Symbols.MaxSymbols > 0 && len(symbols) > cfg.Symbols.MaxSymbols {
		symbols = symbols[:cfg.Symbols.MaxSymbols]
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
	dashboard := web.NewDashboard(webPort, cfg.Mode, symbols, cfg.Executor.InitialBalanceUSD, leverage, cfg.Executor.TakerFeePct, logger)
	if err := dashboard.Start(); err != nil {
		logger.Fatal("dashboard baslama hatasi", zap.Error(err))
	}
	defer dashboard.Stop()

	// Data Router
	var dr router.DataRouter
	switch cfg.Mode {
	case "backtest":
		startT := cfg.Backtest.StartTime
		endT := cfg.Backtest.EndTime

		if *startTime != "" {
			if t, err := parseTime(*startTime); err == nil {
				startT = t
			}
		}
		if *endTime != "" {
			if t, err := parseTime(*endTime); err == nil {
				endT = t
			}
		}

		dr = router.NewBacktestRouter(db.Pool(), symbols, startT, endT, logger)

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
			dashboard.IncrementEvents()

			// Trade event'lerinden guncel fiyati cek
			if e.EventType == models.EventTrade {
				var tp struct {
					Price float64 `json:"price"`
				}
				if err := json.Unmarshal(e.Payload, &tp); err == nil && tp.Price > 0 {
					dashboard.UpdatePrice(e.Symbol, tp.Price)
					se.Tracker().UpdatePrice(e.Symbol, tp.Price)
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
	a := analyzer.New(cfg.Analyzer, db, logger)
	a.LoadVolumes(ctx, symbols)
	analyzerOut := a.Run(ctx, eventsCh)
	signals := se.Run(ctx, analyzerOut)

	// Executor — backtest ve paper ayni PaperExecutor kullanir (v3.1 ozellikleri)
	var exec executor.Executor

	if cfg.Mode == "backtest" || cfg.Mode == "paper" {
		// Paper tablolari olustur
		if err := db.EnsurePaperTables(ctx); err != nil {
			logger.Fatal("paper tablolari olusturma hatasi", zap.Error(err))
		}

		// Run ID ve mode
		runID := fmt.Sprintf("%s_%d", cfg.Mode, time.Now().UnixMilli())
		runMode := cfg.Mode

		logger.Info("run baslatiliyor",
			zap.String("run_id", runID),
			zap.String("run_mode", runMode),
		)

		hooks := &executor.PaperHooks{
			OnSignal: func(s models.SignalEvent) {
				dashboard.AddSignal(s)
				if err := db.SavePaperSignal(ctx, runID, runMode, s); err != nil {
					logger.Error("sinyal kayit hatasi", zap.Error(err))
				}
			},
			OnPositionOpen:  func(sym string, pos *models.Position) { dashboard.UpdatePosition(sym, pos) },
			OnPositionClose: func(sym string) { dashboard.UpdatePosition(sym, nil) },
			OnTrade: func(t models.PaperTrade) {
				dashboard.AddTrade(t)
				if err := db.SavePaperTrade(ctx, runID, runMode, t, 0); err != nil {
					logger.Error("trade kayit hatasi", zap.Error(err))
				}
			},
			OnBalanceChange:   func(bal float64) { dashboard.UpdateBalance(bal) },
			OnTrackPosition:   func(sym, side string, sig models.SignalType, score float64, entryPrice float64, qty float64, lev int, entryTime time.Time) { se.Tracker().TrackPosition(sym, side, sig, score, entryPrice, qty, lev, entryTime) },
			OnUntrackPosition: func(sym string) { se.Tracker().UntrackPosition(sym) },
			GetCurrentPrice:   func(sym string) float64 { return dashboard.GetPrice(sym) },
		}
		exec = executor.NewPaperExecutor(cfg.Executor, hooks, logger)
	} else if cfg.Mode == "live" {
		logger.Fatal("live mod henuz desteklenmiyor")
	}
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
