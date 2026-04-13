package router

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/deep-trader/internal/models"
	"github.com/deep-trader/internal/store"
)

type BacktestRouter struct {
	store     *store.Store
	symbols   []string
	startTime time.Time
	endTime   time.Time
	speed     float64
	out       chan models.MarketEvent
	cancel    context.CancelFunc
	logger    *zap.Logger
}

func NewBacktestRouter(
	s *store.Store,
	symbols []string,
	startTime, endTime time.Time,
	speed float64,
	logger *zap.Logger,
) *BacktestRouter {
	return &BacktestRouter{
		store:     s,
		symbols:   symbols,
		startTime: startTime,
		endTime:   endTime,
		speed:     speed,
		out:       make(chan models.MarketEvent, 1000),
		logger:    logger,
	}
}

func (r *BacktestRouter) Start(ctx context.Context) (<-chan models.MarketEvent, error) {
	ctx, r.cancel = context.WithCancel(ctx)

	r.logger.Info("backtest router baslatiliyor",
		zap.Strings("semboller", r.symbols),
		zap.Time("baslangic", r.startTime),
		zap.Time("bitis", r.endTime),
		zap.Float64("hiz", r.speed),
	)

	go func() {
		defer close(r.out)

		err := r.store.StreamBacktestEvents(ctx, r.symbols, r.startTime, r.endTime, r.out)
		if err != nil && ctx.Err() == nil {
			r.logger.Error("backtest stream hatasi", zap.Error(err))
		}
	}()

	return r.out, nil
}

func (r *BacktestRouter) Stop() error {
	if r.cancel != nil {
		r.cancel()
	}
	r.logger.Info("backtest router durduruldu")
	return nil
}
