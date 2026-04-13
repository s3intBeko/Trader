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

type Engine struct {
	rules       *RuleEngine
	tracker     *SignalTracker
	mlWeight    float64
	lastSignals map[string]lastSignal
	mu          sync.Mutex
	out         chan models.SignalEvent
	logger      *zap.Logger
}

func NewEngine(cfg config.SignalConfig, logger *zap.Logger) *Engine {
	rules := NewRuleEngine(cfg.Rules)
	return &Engine{
		rules:       rules,
		tracker:     NewSignalTracker(rules, logger),
		mlWeight:    cfg.MLWeight,
		lastSignals: make(map[string]lastSignal),
		out:         make(chan models.SignalEvent, 100),
		logger:      logger,
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
				signal := e.evaluate(output)
				if signal.Signal == models.SignalNoEntry {
					continue
				}

				if e.isDuplicate(signal) {
					continue
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
