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

// Hooks — dashboard ve tracker entegrasyonu icin callback'ler
type PaperHooks struct {
	OnSignal         func(models.SignalEvent)
	OnPositionOpen   func(string, *models.Position)
	OnPositionClose  func(string)
	OnTrade          func(models.PaperTrade)
	OnBalanceChange  func(float64)
	OnTrackPosition  func(symbol, side string, signal models.SignalType, score float64, entryPrice float64, quantity float64, leverage int, entryTime time.Time)
	OnUntrackPosition func(symbol string, exitReason string)
	GetCurrentPrice  func(symbol string) float64 // dashboard'dan guncel fiyat al
}

type PaperExecutor struct {
	cfg              config.ExecutorConfig
	balance          float64            // toplam bakiye (realized)
	lockedMargin     float64            // acik pozisyonlarda kilitli teminat
	positions        map[string]*models.Position
	positionMargins  map[string]float64 // symbol -> kilitli teminat
	trades           []models.PaperTrade
	dailyPnL         float64
	lastDay          time.Time
	skippedCount     int // bakiye yetersizliginden atlanan islem sayisi
	hooks            *PaperHooks
	mu               sync.Mutex
	logger           *zap.Logger
}

func NewPaperExecutor(cfg config.ExecutorConfig, hooks *PaperHooks, logger *zap.Logger) *PaperExecutor {
	return &PaperExecutor{
		cfg:             cfg,
		balance:         cfg.InitialBalanceUSD,
		positions:       make(map[string]*models.Position),
		positionMargins: make(map[string]float64),
		hooks:           hooks,
		logger:          logger,
	}
}

// AvailableBalance — kullanilabilir bakiye (toplam - kilitli teminat)
func (pe *PaperExecutor) AvailableBalance() float64 {
	return pe.balance - pe.lockedMargin
}

func (pe *PaperExecutor) Execute(ctx context.Context, signal models.SignalEvent) error {
	pe.mu.Lock()
	defer pe.mu.Unlock()

	// Event zamani kullan (backtest icin dogru gun takibi)
	eventTime := signal.Timestamp
	if eventTime.IsZero() {
		eventTime = time.Now()
	}

	// Hook: sinyal geldi
	if pe.hooks != nil && pe.hooks.OnSignal != nil {
		pe.hooks.OnSignal(signal)
	}

	// Gun degismis mi (event zamanina gore)
	if !pe.lastDay.IsZero() && eventTime.YearDay() != pe.lastDay.YearDay() {
		pe.dailyPnL = 0
	}
	pe.lastDay = eventTime

	// Gunluk kayip limiti
	if pe.dailyPnL < 0 && -pe.dailyPnL >= pe.balance*pe.cfg.DailyLossLimitPct {
		pe.logger.Warn("gunluk kayip limiti asildi",
			zap.Float64("gunluk_pnl", pe.dailyPnL),
		)
		return nil
	}

	pos, hasPos := pe.positions[signal.Symbol]

	// Cikis sinyali geldiyse pozisyonu kapat
	if signal.IsExit && hasPos {
		// Pozisyon kapat — once guncel fiyati dene, yoksa midPrice
		exitPrice := 0.0
		if pe.hooks != nil && pe.hooks.GetCurrentPrice != nil {
			exitPrice = pe.hooks.GetCurrentPrice(signal.Symbol)
		}
		if exitPrice == 0 {
			exitPrice = getMidPrice(signal)
		}
		if exitPrice == 0 {
			pe.logger.Warn("cikis fiyati bulunamadi, pozisyon kapatilamiyor",
				zap.String("symbol", signal.Symbol),
			)
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

		// Likidasyon siniri — zarar teminati asamaz
		margin := pe.positionMargins[signal.Symbol]
		if pnl < -margin {
			pnl = -margin // max zarar = teminat (likidasyon)
		}
		pe.lockedMargin -= margin
		pe.balance += pnl
		pe.dailyPnL += pnl
		delete(pe.positionMargins, signal.Symbol)

		trade := models.PaperTrade{
			Symbol:     signal.Symbol,
			EntryTime:  pos.EntryTime,
			ExitTime:   eventTime,
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

		// Tracker'dan cikar
		if pe.hooks != nil && pe.hooks.OnUntrackPosition != nil {
			pe.hooks.OnUntrackPosition(signal.Symbol, signal.ExitReason)
		}

		pe.logger.Info("PAPER: pozisyon kapatildi",
			zap.String("symbol", signal.Symbol),
			zap.String("sebep", signal.ExitReason),
			zap.Float64("brut_pnl", grossPnL),
			zap.Float64("fee", totalFee),
			zap.Float64("net_pnl", pnl),
			zap.Float64("bakiye", pe.balance),
			zap.Float64("kullanilabilir", pe.AvailableBalance()),
			zap.Float64("kilitli_teminat", pe.lockedMargin),
		)
		return nil
	}

	// Cikis sinyali geldi ama pozisyon yok — yoksay
	if signal.IsExit {
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

	// Max pozisyon kontrolu
	if pe.cfg.MaxPositions > 0 && len(pe.positions) >= pe.cfg.MaxPositions {
		pe.skippedCount++
		pe.logger.Warn("PAPER: max pozisyon limitine ulasildi",
			zap.String("symbol", signal.Symbol),
			zap.Int("acik", len(pe.positions)),
			zap.Int("limit", pe.cfg.MaxPositions),
		)
		return nil
	}

	available := pe.AvailableBalance()
	margin := pe.balance * pe.cfg.MaxPositionPct          // teminat

	// Bakiye kontrolu — yetersizse pozisyon acma
	if margin > available {
		pe.skippedCount++
		pe.logger.Warn("PAPER: bakiye yetersiz, pozisyon acilamiyor",
			zap.String("symbol", signal.Symbol),
			zap.String("sinyal", string(signal.Signal)),
			zap.Float64("gerekli_teminat", margin),
			zap.Float64("kullanilabilir", available),
			zap.Float64("kilitli_teminat", pe.lockedMargin),
			zap.Int("acik_pozisyon", len(pe.positions)),
			zap.Int("toplam_atlanan", pe.skippedCount),
		)
		return nil
	}

	posSize := margin * float64(leverage)                  // pozisyon
	side := "long"
	if signal.Signal == models.SignalDump {
		side = "short"
	}

	// Teminati kilitle
	pe.lockedMargin += margin
	pe.positionMargins[signal.Symbol] = margin

	newPos := &models.Position{
		Symbol:     signal.Symbol,
		Side:       side,
		EntryPrice: midPrice,
		EntryTime:  eventTime,
		Quantity:   posSize / midPrice,
		Signal:     signal.Signal,
	}
	pe.positions[signal.Symbol] = newPos

	// Hooks: pozisyon acildi + tracker'a bildir
	if pe.hooks != nil {
		if pe.hooks.OnPositionOpen != nil {
			pe.hooks.OnPositionOpen(signal.Symbol, newPos)
		}
		if pe.hooks.OnTrackPosition != nil {
			pe.hooks.OnTrackPosition(signal.Symbol, side, signal.Signal, signal.Confidence, midPrice, newPos.Quantity, leverage, eventTime)
		}
	}

	pe.logger.Info("PAPER: pozisyon acildi",
		zap.String("symbol", signal.Symbol),
		zap.String("taraf", side),
		zap.Float64("fiyat", midPrice),
		zap.Float64("teminat_usd", margin),
		zap.Float64("pozisyon_usd", posSize),
		zap.Int("kaldirac", leverage),
		zap.Float64("kullanilabilir_kalan", pe.AvailableBalance()),
		zap.Int("acik_pozisyon", len(pe.positions)),
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
