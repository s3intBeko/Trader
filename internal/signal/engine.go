package signal

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/deep-trader/internal/config"
	"github.com/deep-trader/internal/models"
)

const signalCooldown = 60 * time.Second

type lastSignal struct {
	Signal models.SignalType
	Time   time.Time
}

// pendingConfirm — sinyal onay bekliyor
type pendingConfirm struct {
	Signal       models.SignalEvent
	FirstSeen    time.Time // event timestamp (backtest uyumlu)
	ConfirmCount int
}

type Engine struct {
	rules            *RuleEngine
	tracker          *SignalTracker
	mlWeight         float64
	confirmDelay     time.Duration // sinyal onay gecikmesi (0 = aninda gir)
	lastSignals      map[string]lastSignal
	pendingSignals   map[string]*pendingConfirm // symbol -> onay bekleyen sinyal
	mu               sync.Mutex
	out              chan models.SignalEvent
	logger           *zap.Logger
}

func NewEngine(cfg config.SignalConfig, takerFeePct float64, logger *zap.Logger) *Engine {
	rules := NewRuleEngine(cfg.Rules)
	return &Engine{
		rules:          rules,
		tracker:        NewSignalTracker(rules, takerFeePct, cfg.StaleTimeout, cfg.LossCooldown, cfg.HardStopCooldown1, cfg.HardStopCooldown2, logger),
		mlWeight:       cfg.MLWeight,
		confirmDelay:   cfg.ConfirmDelay,
		lastSignals:    make(map[string]lastSignal),
		pendingSignals: make(map[string]*pendingConfirm),
		out:            make(chan models.SignalEvent, 100),
		logger:         logger,
	}
}

// Tracker — dis erisim icin (executor'dan pozisyon bildir)
func (e *Engine) Tracker() *SignalTracker {
	return e.tracker
}

func (e *Engine) Run(ctx context.Context, in <-chan models.AnalyzerOutput) <-chan models.SignalEvent {
	go func() {
		defer close(e.out)
		for {
			select {
			case output, ok := <-in:
				if !ok {
					return
				}

				// 0. Acil cikislar (hard stop-loss, trailing stop — UpdatePrice'da tetiklenmis)
				if pendingExits := e.tracker.DrainPendingExits(); len(pendingExits) > 0 {
					for sym, decision := range pendingExits {
						exitSignal := models.SignalEvent{
							Symbol:     sym,
							Timestamp:  time.Now(),
							Signal:     models.SignalNoEntry,
							Source:     "tracker-urgent",
							Reasons:    []string{decision.Reason},
							RawMetrics: output,
							IsExit:     true,
							ExitReason: decision.Reason,
						}
						e.logger.Warn("ACIL cikis karari",
							zap.String("symbol", sym),
							zap.String("sebep", decision.Reason),
						)
						select {
						case e.out <- exitSignal:
						case <-ctx.Done():
							return
						}
					}
				}

				symbol := output.OrderBookMetrics.Symbol

				// 1. Acik pozisyon varsa: sinyal gucunu degerlendir
				if e.tracker.HasPosition(symbol) {
					decision := e.tracker.Evaluate(symbol, output)
					if decision != nil && decision.ShouldExit {
						exitSignal := models.SignalEvent{
							Symbol:     symbol,
							Timestamp:  time.Now(),
							Signal:     models.SignalNoEntry,
							Source:     "tracker",
							Reasons:    []string{decision.Reason},
							RawMetrics: output,
							IsExit:     true,
							ExitReason: decision.Reason,
						}

						e.logger.Info("cikis karari",
							zap.String("symbol", symbol),
							zap.String("sebep", decision.Reason),
						)

						select {
						case e.out <- exitSignal:
						case <-ctx.Done():
							return
						}
					}
					continue // Acik pozisyon varsa yeni giris sinyali uretme
				}

				// 2. Acik pozisyon yoksa: yeni giris sinyali degerlendir
				// Cooldown kontrolu
				if e.tracker.IsOnCooldown(symbol) {
					continue
				}

				signal := e.evaluate(output)
				if signal.Signal == models.SignalNoEntry {
					// Sinyal kayboldu — pending varsa iptal et
					delete(e.pendingSignals, symbol)
					continue
				}

				// Confirmation kontrolu — sinyal kac kez ust uste gelmeli
				// NOT: isDuplicate kontrolu confirm'den SONRA yapilir
				// confirm_delay / emit_interval = gerekli tekrar sayisi
				if e.confirmDelay > 0 {
					pending, exists := e.pendingSignals[symbol]
					if !exists || pending.Signal.Signal != signal.Signal {
						e.pendingSignals[symbol] = &pendingConfirm{
							Signal:       signal,
							FirstSeen:    output.OrderBookMetrics.Timestamp,
							ConfirmCount: 1,
						}
						continue
					}

					pending.ConfirmCount++
					pending.Signal = signal

					// Gerekli tekrar sayisi: confirm_delay / emit_interval (minimum 2)
					// 5s delay / 5s emit = 1 tekrar, 10s/5s = 2, 20s/5s = 4
					requiredCount := int(e.confirmDelay.Seconds() / 5) // emit ~5s
					if requiredCount < 1 {
						requiredCount = 1
					}

					if pending.ConfirmCount <= requiredCount {
						continue
					}

					signal = pending.Signal
					delete(e.pendingSignals, symbol)

					e.logger.Info("sinyal onaylandi",
						zap.String("symbol", symbol),
						zap.String("sinyal", string(signal.Signal)),
						zap.Int("onay_sayisi", pending.ConfirmCount),
						zap.Int("gerekli", requiredCount),
					)
				}

				e.logger.Info("giris sinyali",
					zap.String("symbol", signal.Symbol),
					zap.String("sinyal", string(signal.Signal)),
					zap.Float64("guven", signal.Confidence),
					zap.Float64("fiyat", signal.RawMetrics.MidPrice),
					zap.Strings("sebepler", signal.Reasons),
				)

				select {
				case e.out <- signal:
				case <-ctx.Done():
					return
				}

			case <-ctx.Done():
				return
			}
		}
	}()
	return e.out
}

func (e *Engine) isDuplicate(signal models.SignalEvent) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	last, ok := e.lastSignals[signal.Symbol]
	if ok && last.Signal == signal.Signal && time.Since(last.Time) < signalCooldown {
		return true
	}

	e.lastSignals[signal.Symbol] = lastSignal{
		Signal: signal.Signal,
		Time:   time.Now(),
	}
	return false
}

func (e *Engine) evaluate(out models.AnalyzerOutput) models.SignalEvent {
	ruleSignal, ruleConf, reasons := e.rules.Evaluate(out)

	return models.SignalEvent{
		Symbol:     out.OrderBookMetrics.Symbol,
		Timestamp:  time.Now(),
		Signal:     ruleSignal,
		Confidence: ruleConf,
		Source:     "rules",
		Reasons:    reasons,
		RawMetrics: out,
	}
}
