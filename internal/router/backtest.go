package router

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/deep-trader/internal/models"
)

// BacktestRouter — gecmis veriyi zaman dilimlerine bolerek chunk chunk okur.
// UNION ALL + ORDER BY yerine ayri sorgularla calisir (performans).
type BacktestRouter struct {
	pool      *pgxpool.Pool
	symbols   []string
	startTime time.Time
	endTime   time.Time
	out       chan models.MarketEvent
	cancel    context.CancelFunc
	logger    *zap.Logger
}

func NewBacktestRouter(
	pool *pgxpool.Pool,
	symbols []string,
	startTime, endTime time.Time,
	logger *zap.Logger,
) *BacktestRouter {
	return &BacktestRouter{
		pool:      pool,
		symbols:   symbols,
		startTime: startTime,
		endTime:   endTime,
		out:       make(chan models.MarketEvent, 5000),
		logger:    logger,
	}
}

func (r *BacktestRouter) Start(ctx context.Context) (<-chan models.MarketEvent, error) {
	ctx, r.cancel = context.WithCancel(ctx)

	r.logger.Info("backtest router baslatiliyor (chunk modu)",
		zap.Strings("semboller", r.symbols),
		zap.Time("baslangic", r.startTime),
		zap.Time("bitis", r.endTime),
	)

	go r.streamChunks(ctx)

	return r.out, nil
}

func (r *BacktestRouter) streamChunks(ctx context.Context) {
	defer close(r.out)

	// 30 dakikalik dilimlerle ilerle
	chunkDuration := 30 * time.Minute
	current := r.startTime
	totalEvents := 0

	for current.Before(r.endTime) {
		select {
		case <-ctx.Done():
			return
		default:
		}

		chunkEnd := current.Add(chunkDuration)
		if chunkEnd.After(r.endTime) {
			chunkEnd = r.endTime
		}

		// Depth snapshots
		depthCount := r.pollDepthChunk(ctx, current, chunkEnd)

		// Trades
		tradeCount := r.pollTradeChunk(ctx, current, chunkEnd)

		totalEvents += depthCount + tradeCount

		if (depthCount+tradeCount) > 0 && totalEvents%10000 < (depthCount+tradeCount) {
			r.logger.Info("backtest ilerleme",
				zap.Time("zaman", current),
				zap.Int("toplam_event", totalEvents),
				zap.Int("chunk_depth", depthCount),
				zap.Int("chunk_trade", tradeCount),
			)
		}

		current = chunkEnd
	}

	r.logger.Info("backtest tamamlandi",
		zap.Int("toplam_event", totalEvents),
		zap.Time("baslangic", r.startTime),
		zap.Time("bitis", r.endTime),
	)
}

func (r *BacktestRouter) pollDepthChunk(ctx context.Context, start, end time.Time) int {
	// Her sembol icin 5 saniyede 1 snapshot al (10/sn yerine 0.2/sn = 50x azalma)
	const query = `
		SELECT DISTINCT ON (symbol, time_bucket('5 seconds', time))
			symbol, time, bid_prices, bid_quantities, ask_prices, ask_quantities
		FROM depth_snapshots
		WHERE symbol = ANY($1) AND time >= $2 AND time < $3
		ORDER BY symbol, time_bucket('5 seconds', time), time DESC
	`

	rows, err := r.pool.Query(ctx, query, r.symbols, start, end)
	if err != nil {
		r.logger.Error("backtest depth sorgu hatasi", zap.Error(err))
		return 0
	}
	defer rows.Close()

	count := 0
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
			continue
		}

		payload, _ := json.Marshal(map[string]interface{}{
			"bid_prices": bidPrices, "bid_quantities": bidQuantities,
			"ask_prices": askPrices, "ask_quantities": askQuantities,
		})

		select {
		case r.out <- models.MarketEvent{
			Symbol: symbol, Timestamp: ts, EventType: models.EventDepth,
			Payload: payload, Source: "backtest",
		}:
			count++
		case <-ctx.Done():
			return count
		}
	}
	return count
}

func (r *BacktestRouter) pollTradeChunk(ctx context.Context, start, end time.Time) int {
	// 1 saniyelik bucket'lara aggregate et (her trade yerine ozet)
	const query = `
		SELECT symbol,
			time_bucket('1 second', time) as time,
			sum(price * quantity) / NULLIF(sum(quantity), 0) as price,
			sum(quantity) as quantity,
			sum(CASE WHEN is_buyer_maker THEN quantity ELSE 0 END) >
			sum(CASE WHEN NOT is_buyer_maker THEN quantity ELSE 0 END) as is_buyer_maker
		FROM agg_trades
		WHERE symbol = ANY($1) AND time >= $2 AND time < $3
		GROUP BY symbol, time_bucket('1 second', time)
		ORDER BY time
	`

	rows, err := r.pool.Query(ctx, query, r.symbols, start, end)
	if err != nil {
		r.logger.Error("backtest trade sorgu hatasi", zap.Error(err))
		return 0
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var (
			symbol       string
			ts           time.Time
			price        float64
			quantity     float64
			isBuyerMaker bool
		)
		if err := rows.Scan(&symbol, &ts, &price, &quantity, &isBuyerMaker); err != nil {
			continue
		}

		payload, _ := json.Marshal(map[string]interface{}{
			"price": price, "quantity": quantity, "is_buyer_maker": isBuyerMaker,
		})

		select {
		case r.out <- models.MarketEvent{
			Symbol: symbol, Timestamp: ts, EventType: models.EventTrade,
			Payload: payload, Source: "backtest",
		}:
			count++
		case <-ctx.Done():
			return count
		}
	}
	return count
}

func (r *BacktestRouter) Stop() error {
	if r.cancel != nil {
		r.cancel()
	}
	r.logger.Info("backtest router durduruldu")
	return nil
}
