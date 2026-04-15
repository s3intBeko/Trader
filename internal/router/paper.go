package router

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/deep-trader/internal/config"
	"github.com/deep-trader/internal/models"
)

// PaperRouter — collector'in DB'ye yazdigi canli verileri periyodik olarak okur.
// Collector zaten Binance WS'ten topluyor, biz sadece DB'den okuyoruz.
type PaperRouter struct {
	pool         *pgxpool.Pool
	cfg          config.AnalyzerConfig
	symbols      []string
	out          chan models.MarketEvent
	cancel       context.CancelFunc
	logger       *zap.Logger
	pollInterval time.Duration
}

func NewPaperRouter(pool *pgxpool.Pool, symbols []string, cfg config.AnalyzerConfig, logger *zap.Logger) *PaperRouter {
	pollInterval := cfg.EmitInterval
	if pollInterval == 0 {
		pollInterval = 2 * time.Second
	}

	return &PaperRouter{
		pool:         pool,
		cfg:          cfg,
		symbols:      symbols,
		out:          make(chan models.MarketEvent, 1000),
		logger:       logger,
		pollInterval: pollInterval,
	}
}

func (r *PaperRouter) Start(ctx context.Context) (<-chan models.MarketEvent, error) {
	ctx, r.cancel = context.WithCancel(ctx)

	r.logger.Info("paper router baslatiliyor (DB poll modu)",
		zap.Strings("semboller", r.symbols),
		zap.Duration("poll_araligi", r.pollInterval),
	)

	go func() {
		lastDepthTime, lastTradeTime := r.bootstrap(ctx)
		r.pollLoop(ctx, lastDepthTime, lastTradeTime)
	}()

	return r.out, nil
}

func (r *PaperRouter) bootstrap(ctx context.Context) (time.Time, time.Time) {
	lastDepthTime := r.primeDepthSnapshots(ctx)
	if lastDepthTime.IsZero() {
		lastDepthTime = time.Now()
	}

	lastTradeTime := r.fetchLatestTradeTime(ctx)
	if lastTradeTime.IsZero() {
		lastTradeTime = time.Now()
	}

	return lastDepthTime, lastTradeTime
}

func (r *PaperRouter) primeDepthSnapshots(ctx context.Context) time.Time {
	const query = `
		SELECT DISTINCT ON (symbol)
			symbol, time,
			bid_prices, bid_quantities,
			ask_prices, ask_quantities
		FROM depth_snapshots
		WHERE symbol = ANY($1)
		ORDER BY symbol, time DESC
	`

	rows, err := r.pool.Query(ctx, query, r.symbols)
	if err != nil {
		r.logger.Error("depth prime hatasi", zap.Error(err))
		return time.Time{}
	}
	defer rows.Close()

	var lastTime time.Time
	for rows.Next() {
		var (
			symbol        string
			ts            time.Time
			bidPrices     []float64
			bidQuantities []float64
			askPrices     []float64
			askQuantities []float64
		)

		if err := rows.Scan(&symbol, &ts, &bidPrices, &bidQuantities, &askPrices, &askQuantities); err != nil {
			r.logger.Error("depth prime satir okuma hatasi", zap.Error(err))
			continue
		}

		payload, err := json.Marshal(map[string]interface{}{
			"bid_prices":     bidPrices,
			"bid_quantities": bidQuantities,
			"ask_prices":     askPrices,
			"ask_quantities": askQuantities,
		})
		if err != nil {
			r.logger.Error("depth prime payload hatasi", zap.Error(err))
			continue
		}

		event := models.MarketEvent{
			Symbol:    symbol,
			Timestamp: ts,
			EventType: models.EventDepth,
			Payload:   payload,
			Source:    "paper",
		}

		select {
		case r.out <- event:
		case <-ctx.Done():
			return lastTime
		}

		if ts.After(lastTime) {
			lastTime = ts
		}
	}

	return lastTime
}

func (r *PaperRouter) fetchLatestTradeTime(ctx context.Context) time.Time {
	const query = `
		SELECT COALESCE(MAX(time), TIMESTAMPTZ 'epoch')
		FROM agg_trades
		WHERE symbol = ANY($1)
	`

	var ts time.Time
	if err := r.pool.QueryRow(ctx, query, r.symbols).Scan(&ts); err != nil {
		r.logger.Error("trade max time hatasi", zap.Error(err))
		return time.Time{}
	}
	if ts.Unix() <= 0 {
		return time.Time{}
	}
	return ts
}

func (r *PaperRouter) pollLoop(ctx context.Context, lastDepthTime, lastTradeTime time.Time) {
	defer close(r.out)

	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			newDepthTime := r.pollDepth(ctx, lastDepthTime)
			if !newDepthTime.IsZero() {
				lastDepthTime = newDepthTime
			}

			newTradeTime := r.pollTrades(ctx, lastTradeTime)
			if !newTradeTime.IsZero() {
				lastTradeTime = newTradeTime
			}

		case <-ctx.Done():
			return
		}
	}
}

// pollDepth — her sembol icin en son depth snapshot'i okur.
func (r *PaperRouter) pollDepth(ctx context.Context, since time.Time) time.Time {
	const query = `
		SELECT DISTINCT ON (symbol)
			symbol, time,
			bid_prices, bid_quantities,
			ask_prices, ask_quantities
		FROM depth_snapshots
		WHERE symbol = ANY($1)
		  AND time > $2
		ORDER BY symbol, time DESC
	`

	rows, err := r.pool.Query(ctx, query, r.symbols, since)
	if err != nil {
		r.logger.Error("depth poll hatasi", zap.Error(err))
		return time.Time{}
	}
	defer rows.Close()

	var lastTime time.Time
	for rows.Next() {
		var (
			symbol        string
			ts            time.Time
			bidPrices     []float64
			bidQuantities []float64
			askPrices     []float64
			askQuantities []float64
		)

		if err := rows.Scan(&symbol, &ts, &bidPrices, &bidQuantities, &askPrices, &askQuantities); err != nil {
			r.logger.Error("depth satir okuma hatasi", zap.Error(err))
			continue
		}

		payload, err := json.Marshal(map[string]interface{}{
			"bid_prices":     bidPrices,
			"bid_quantities": bidQuantities,
			"ask_prices":     askPrices,
			"ask_quantities": askQuantities,
		})
		if err != nil {
			r.logger.Error("depth payload hatasi", zap.Error(err))
			continue
		}

		event := models.MarketEvent{
			Symbol:    symbol,
			Timestamp: ts,
			EventType: models.EventDepth,
			Payload:   payload,
			Source:    "paper",
		}

		select {
		case r.out <- event:
		case <-ctx.Done():
			return lastTime
		}

		if ts.After(lastTime) {
			lastTime = ts
		}
	}

	return lastTime
}

// pollTrades — son okunan zamandan itibaren yeni agg_trades satirlarini okur.
func (r *PaperRouter) pollTrades(ctx context.Context, since time.Time) time.Time {
	const query = `
		SELECT symbol, time, price, quantity, is_buyer_maker
		FROM agg_trades
		WHERE symbol = ANY($1)
		  AND time > $2
		ORDER BY time ASC, symbol ASC
	`

	rows, err := r.pool.Query(ctx, query, r.symbols, since)
	if err != nil {
		r.logger.Error("trade poll hatasi", zap.Error(err))
		return time.Time{}
	}
	defer rows.Close()

	var lastTime time.Time
	for rows.Next() {
		var (
			symbol       string
			ts           time.Time
			price        float64
			quantity     float64
			isBuyerMaker bool
		)

		if err := rows.Scan(&symbol, &ts, &price, &quantity, &isBuyerMaker); err != nil {
			r.logger.Error("trade satir okuma hatasi", zap.Error(err))
			continue
		}

		payload, err := json.Marshal(map[string]interface{}{
			"price":          price,
			"quantity":       quantity,
			"is_buyer_maker": isBuyerMaker,
		})
		if err != nil {
			r.logger.Error("trade payload hatasi", zap.Error(err))
			continue
		}

		event := models.MarketEvent{
			Symbol:    symbol,
			Timestamp: ts,
			EventType: models.EventTrade,
			Payload:   payload,
			Source:    "paper",
		}

		select {
		case r.out <- event:
		case <-ctx.Done():
			return lastTime
		}

		lastTime = ts
	}

	return lastTime
}

func (r *PaperRouter) Stop() error {
	if r.cancel != nil {
		r.cancel()
	}
	r.logger.Info("paper router durduruldu")
	return nil
}
