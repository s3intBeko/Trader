package executor

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/deep-trader/internal/config"
	"github.com/deep-trader/internal/models"
	"github.com/deep-trader/internal/store"
)

type BacktestExecutor struct {
	store     *store.Store
	cfg       config.ExecutorConfig
	runID     string
	positions map[string]*models.Position
	balance   float64
	dailyPnL  float64
	lastDay   time.Time
	logger    *zap.Logger
}

func NewBacktestExecutor(s *store.Store, cfg config.ExecutorConfig, logger *zap.Logger) *BacktestExecutor {
	return &BacktestExecutor{
		store:     s,
		cfg:       cfg,
		runID:     fmt.Sprintf("bt_%d", time.Now().UnixMilli()),
		positions: make(map[string]*models.Position),
		balance:   cfg.InitialBalanceUSD,
		logger:    logger,
	}
}

func (be *BacktestExecutor) Execute(ctx context.Context, signal models.SignalEvent) error {
	// Gun degismis mi kontrol et (daily loss limit)
	if !be.lastDay.IsZero() && signal.Timestamp.Day() != be.lastDay.Day() {
		be.dailyPnL = 0
	}
	be.lastDay = signal.Timestamp

	// Gunluk kayip limitini kontrol et
	if be.dailyPnL < 0 && -be.dailyPnL >= be.balance*be.cfg.DailyLossLimitPct {
		be.logger.Warn("gunluk kayip limiti asildi, islem yapilmiyor",
			zap.String("symbol", signal.Symbol),
			zap.Float64("gunluk_pnl", be.dailyPnL),
		)
		return nil
	}

	// Mevcut pozisyon var mi?
	pos, hasPos := be.positions[signal.Symbol]

	if hasPos {
		// Pozisyon kapat
		exitPrice := signal.RawMetrics.OrderBookMetrics.TotalBidVolume // TODO: gercek fiyat kullan
		if midPrice := getMidPrice(signal); midPrice > 0 {
			exitPrice = midPrice
		}

		pnl := be.calculatePnL(pos, exitPrice)
		be.balance += pnl
		be.dailyPnL += pnl

		result := store.BacktestResult{
			RunID:      be.runID,
			Symbol:     signal.Symbol,
			EntryTime:  pos.EntryTime,
			ExitTime:   signal.Timestamp,
			Signal:     string(pos.Signal),
			EntryPrice: pos.EntryPrice,
			ExitPrice:  exitPrice,
			PnL:        pnl,
			Confidence: signal.Confidence,
			Params:     map[string]interface{}{"balance": be.balance},
		}

		if err := be.store.SaveBacktestResult(ctx, result); err != nil {
			be.logger.Error("backtest sonuc yazma hatasi", zap.Error(err))
		}

		delete(be.positions, signal.Symbol)
		be.logger.Info("pozisyon kapatildi",
			zap.String("symbol", signal.Symbol),
			zap.Float64("pnl", pnl),
			zap.Float64("bakiye", be.balance),
		)
		return nil
	}

	// Yeni pozisyon ac
	if signal.Signal == models.SignalNoEntry {
		return nil
	}

	posSize := be.balance * be.cfg.MaxPositionPct
	midPrice := getMidPrice(signal)
	if midPrice == 0 {
		return nil
	}

	side := "long"
	if signal.Signal == models.SignalDump {
		side = "short"
	}

	be.positions[signal.Symbol] = &models.Position{
		Symbol:     signal.Symbol,
		Side:       side,
		EntryPrice: midPrice,
		EntryTime:  signal.Timestamp,
		Quantity:   posSize / midPrice,
		Signal:     signal.Signal,
	}

	be.logger.Info("pozisyon acildi",
		zap.String("symbol", signal.Symbol),
		zap.String("taraf", side),
		zap.Float64("fiyat", midPrice),
		zap.Float64("buyukluk_usd", posSize),
	)

	return nil
}

func (be *BacktestExecutor) Close() error {
	be.logger.Info("backtest tamamlandi",
		zap.String("run_id", be.runID),
		zap.Float64("son_bakiye", be.balance),
		zap.Float64("baslangic_bakiye", be.cfg.InitialBalanceUSD),
		zap.Float64("toplam_pnl", be.balance-be.cfg.InitialBalanceUSD),
	)
	return nil
}

func (be *BacktestExecutor) calculatePnL(pos *models.Position, exitPrice float64) float64 {
	if pos.Side == "long" {
		return pos.Quantity * (exitPrice - pos.EntryPrice)
	}
	return pos.Quantity * (pos.EntryPrice - exitPrice)
}

func getMidPrice(signal models.SignalEvent) float64 {
	return signal.RawMetrics.MidPrice
}
