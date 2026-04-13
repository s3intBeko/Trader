package signal

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/deep-trader/internal/config"
	"github.com/deep-trader/internal/models"
)

// Ayni sembol icin ayni sinyal tekrar gonderilmemesi gereken minimum sure
const signalCooldown = 60 * time.Second

type lastSignal struct {
	Signal models.SignalType
	Time   time.Time
}

type Engine struct {
	rules      *RuleEngine
	mlWeight   float64
	lastSignals map[string]lastSignal // symbol -> son sinyal
	mu         sync.Mutex
	out        chan models.SignalEvent
	logger     *zap.Logger
}

func NewEngine(cfg config.SignalConfig, logger *zap.Logger) *Engine {
	return &Engine{
		rules:       NewRuleEngine(cfg.Rules),
		mlWeight:    cfg.MLWeight,
		lastSignals: make(map[string]lastSignal),
		out:         make(chan models.SignalEvent, 100),
		logger:      logger,
	}
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
				signal := e.evaluate(output)
				if signal.Signal == models.SignalNoEntry {
					continue
				}

				// Duplicate sinyal kontrolu
				if e.isDuplicate(signal) {
					continue
				}

				e.logger.Info("sinyal uretildi",
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

// isDuplicate — ayni sembol icin ayni sinyal cooldown suresi icinde tekrar geliyorsa engelle
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
