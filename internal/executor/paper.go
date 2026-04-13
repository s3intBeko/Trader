package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/deep-trader/internal/config"
	"github.com/deep-trader/internal/models"
)

// Hooks — dashboard entegrasyonu icin callback'ler
type PaperHooks struct {
	OnSignal         func(models.SignalEvent)
	OnPositionOpen   func(string, *models.Position)
	OnPositionClose  func(string)
	OnTrade          func(models.PaperTrade)
	OnBalanceChange  func(float64)
}

type PaperExecutor struct {
	cfg       config.ExecutorConfig
	balance   float64
	positions map[string]*models.Position
	trades    []models.PaperTrade
	dailyPnL  float64
	lastDay   time.Time
	hooks     *PaperHooks
	mu        sync.Mutex
	logger    *zap.Logger
}

func NewPaperExecutor(cfg config.ExecutorConfig, hooks *PaperHooks, logger *zap.Logger) *PaperExecutor {
	return &PaperExecutor{
		cfg:       cfg,
		balance:   cfg.InitialBalanceUSD,
		positions: make(map[string]*models.Position),
		hooks:     hooks,
		logger:    logger,
	}
}

func (pe *PaperExecutor) Execute(ctx context.Context, signal models.SignalEvent) error {
	pe.mu.Lock()
	defer pe.mu.Unlock()

	now := time.Now()

	// Hook: sinyal geldi
	if pe.hooks != nil && pe.hooks.OnSignal != nil {
		pe.hooks.OnSignal(signal)
	}

	// Gun degismis mi
	if !pe.lastDay.IsZero() && now.Day() != pe.lastDay.Day() {
		pe.dailyPnL = 0
	}
	pe.lastDay = now

	// Gunluk kayip limiti
	if pe.dailyPnL < 0 && -pe.dailyPnL >= pe.balance*pe.cfg.DailyLossLimitPct {
		pe.logger.Warn("gunluk kayip limiti asildi",
			zap.Float64("gunluk_pnl", pe.dailyPnL),
		)
		return nil
	}

	pos, hasPos := pe.positions[signal.Symbol]

	if hasPos {
		// Pozisyon kapat
		exitPrice := getMidPrice(signal)
		if exitPrice == 0 {
			return nil
		}

		// Brut PnL
		var grossPnL float64
		if pos.Side == "long" {
			grossPnL = pos.Quantity * (exitPrice - pos.EntryPrice)
		} else {
			grossPnL = pos.Quantity * (pos.EntryPrice - exitPrice)
		}

		// Fee hesapla (pozisyon buyuklugu uzerinden, giris + cikis)
		posNotional := pos.Quantity * pos.EntryPrice
		exitNotional := pos.Quantity * exitPrice
		entryFee := posNotional * pe.cfg.TakerFeePct
		exitFee := exitNotional * pe.cfg.TakerFeePct
		totalFee := entryFee + exitFee

		// Net PnL = brut - fee
		pnl := grossPnL - totalFee

		pe.balance += pnl
		pe.dailyPnL += pnl

		trade := models.PaperTrade{
			Symbol:     signal.Symbol,
			EntryTime:  pos.EntryTime,
			ExitTime:   now,
			EntryPrice: pos.EntryPrice,
			ExitPrice:  exitPrice,
			Side:       pos.Side,
			PnL:        pnl,
			Signal:     pos.Signal,
			Reasons:    signal.Reasons,
		}
		pe.trades = append(pe.trades, trade)
		delete(pe.positions, signal.Symbol)

		// Hooks
		if pe.hooks != nil {
			if pe.hooks.OnPositionClose != nil {
				pe.hooks.OnPositionClose(signal.Symbol)
			}
			if pe.hooks.OnTrade != nil {
				pe.hooks.OnTrade(trade)
			}
			if pe.hooks.OnBalanceChange != nil {
				pe.hooks.OnBalanceChange(pe.balance)
			}
		}

		pe.logger.Info("PAPER: pozisyon kapatildi",
			zap.String("symbol", signal.Symbol),
			zap.Float64("brut_pnl", grossPnL),
			zap.Float64("fee", totalFee),
			zap.Float64("net_pnl", pnl),
			zap.Float64("bakiye", pe.balance),
		)
		return nil
	}

	// Yeni pozisyon
	if signal.Signal == models.SignalNoEntry {
		return nil
	}

	midPrice := getMidPrice(signal)
	if midPrice == 0 {
		return nil
	}

	leverage := pe.cfg.Leverage
	if leverage <= 0 {
		leverage = 1
	}

	margin := pe.balance * pe.cfg.MaxPositionPct          // teminat ($1000)
	posSize := margin * float64(leverage)                  // pozisyon ($5000)
	side := "long"
	if signal.Signal == models.SignalDump {
		side = "short"
	}

	newPos := &models.Position{
		Symbol:     signal.Symbol,
		Side:       side,
		EntryPrice: midPrice,
		EntryTime:  now,
		Quantity:   posSize / midPrice,
		Signal:     signal.Signal,
	}
	pe.positions[signal.Symbol] = newPos

	// Hook: pozisyon acildi
	if pe.hooks != nil && pe.hooks.OnPositionOpen != nil {
		pe.hooks.OnPositionOpen(signal.Symbol, newPos)
	}

	pe.logger.Info("PAPER: pozisyon acildi",
		zap.String("symbol", signal.Symbol),
		zap.String("taraf", side),
		zap.Float64("fiyat", midPrice),
		zap.Float64("teminat_usd", margin),
		zap.Float64("pozisyon_usd", posSize),
		zap.Int("kaldirac", leverage),
	)

	return nil
}

func (pe *PaperExecutor) Close() error {
	pe.mu.Lock()
	defer pe.mu.Unlock()

	// Trade gecmisini dosyaya yaz
	if len(pe.trades) > 0 {
		data, err := json.MarshalIndent(pe.trades, "", "  ")
		if err == nil {
			filename := fmt.Sprintf("paper_trades_%d.json", time.Now().Unix())
			os.WriteFile(filename, data, 0644)
			pe.logger.Info("paper trade gecmisi kaydedildi",
				zap.String("dosya", filename),
				zap.Int("islem_sayisi", len(pe.trades)),
			)
		}
	}

	totalPnL := 0.0
	for _, t := range pe.trades {
		totalPnL += t.PnL
	}

	pe.logger.Info("paper trading tamamlandi",
		zap.Float64("son_bakiye", pe.balance),
		zap.Float64("toplam_pnl", totalPnL),
		zap.Int("toplam_islem", len(pe.trades)),
	)

	return nil
}
