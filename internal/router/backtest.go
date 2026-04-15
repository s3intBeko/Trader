package router

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/deep-trader/internal/models"
)

var errSkipBacktestEvent = errors.New("skip backtest event")

// BacktestRouter — gecmis veriyi zaman dilimlerine bolerek zaman sirali okur.
// Depth ve trade event'leri tek bir kronolojik akis halinde uretilir.
type BacktestRouter struct {
	pool            *pgxpool.Pool
	universe        []models.SymbolActivation
	symbols         []string
	activationTimes map[string]time.Time
	startTime       time.Time
	endTime         time.Time
	speed           float64
	out             chan models.MarketEvent
	cancel          context.CancelFunc
	logger          *zap.Logger
}

func NewBacktestRouter(
	pool *pgxpool.Pool,
	universe []models.SymbolActivation,
	startTime, endTime time.Time,
	speed float64,
	logger *zap.Logger,
) *BacktestRouter {
	symbols := make([]string, 0, len(universe))
	activationTimes := make(map[string]time.Time, len(universe))
	for _, activation := range universe {
		symbols = append(symbols, activation.Symbol)
		activationTimes[activation.Symbol] = activation.ActivationTime
	}

	return &BacktestRouter{
		pool:            pool,
		universe:        universe,
		symbols:         symbols,
		activationTimes: activationTimes,
		startTime:       startTime,
		endTime:         endTime,
		speed:           speed,
		out:             make(chan models.MarketEvent, 5000),
		logger:          logger,
	}
}

func (r *BacktestRouter) Start(ctx context.Context) (<-chan models.MarketEvent, error) {
	ctx, r.cancel = context.WithCancel(ctx)

	r.logger.Info("backtest router baslatiliyor (zaman sirali chunk modu)",
		zap.Strings("semboller", r.symbols),
		zap.Time("baslangic", r.startTime),
		zap.Time("bitis", r.endTime),
		zap.Float64("speed", r.speed),
	)

	go r.streamChunks(ctx)

	return r.out, nil
}

func (r *BacktestRouter) streamChunks(ctx context.Context) {
	defer close(r.out)

	chunkDuration := 30 * time.Minute
	current := r.startTime
	totalEvents := 0
	var prevEventTime time.Time

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

		depthRows, err := r.queryDepthChunk(ctx, current, chunkEnd)
		if err != nil {
			r.logger.Error("backtest depth sorgu hatasi", zap.Error(err))
			current = chunkEnd
			continue
		}
		tradeRows, err := r.queryTradeChunk(ctx, current, chunkEnd)
		if err != nil {
			depthRows.Close()
			r.logger.Error("backtest trade sorgu hatasi", zap.Error(err))
			current = chunkEnd
			continue
		}

		depthCursor := newEventCursor(depthRows, r.scanDepthEvent, r.logger)
		tradeCursor := newEventCursor(tradeRows, r.scanTradeEvent, r.logger)

		chunkCount := 0
		for depthCursor.current != nil || tradeCursor.current != nil {
			nextCursor := pickEventCursor(depthCursor, tradeCursor)
			if nextCursor == nil || nextCursor.current == nil {
				break
			}

			event := *nextCursor.current
			if delay := paceDuration(prevEventTime, event.Timestamp, r.speed); delay > 0 {
				timer := time.NewTimer(delay)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					depthCursor.Close()
					tradeCursor.Close()
					return
				}
			}

			select {
			case r.out <- event:
			case <-ctx.Done():
				depthCursor.Close()
				tradeCursor.Close()
				return
			}

			prevEventTime = event.Timestamp
			totalEvents++
			chunkCount++
			nextCursor.Advance()
		}

		depthCursor.Close()
		tradeCursor.Close()

		if chunkCount > 0 && totalEvents%10000 < chunkCount {
			r.logger.Info("backtest ilerleme",
				zap.Time("zaman", current),
				zap.Int("toplam_event", totalEvents),
				zap.Int("chunk_event", chunkCount),
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

func (r *BacktestRouter) queryDepthChunk(ctx context.Context, start, end time.Time) (pgx.Rows, error) {
	const query = `
		SELECT symbol, snapshot_time, bid_prices, bid_quantities, ask_prices, ask_quantities
		FROM (
			SELECT DISTINCT ON (symbol, time_bucket('5 seconds', time))
				symbol,
				time AS snapshot_time,
				bid_prices,
				bid_quantities,
				ask_prices,
				ask_quantities
			FROM depth_snapshots
			WHERE symbol = ANY($1)
			  AND time >= $2
			  AND time < $3
			ORDER BY symbol, time_bucket('5 seconds', time), time DESC
		) depth
		ORDER BY snapshot_time ASC, symbol ASC
	`
	return r.pool.Query(ctx, query, r.symbols, start, end)
}

func (r *BacktestRouter) queryTradeChunk(ctx context.Context, start, end time.Time) (pgx.Rows, error) {
	const query = `
		SELECT symbol, time, price, quantity, is_buyer_maker
		FROM agg_trades
		WHERE symbol = ANY($1)
		  AND time >= $2
		  AND time < $3
		ORDER BY time ASC, symbol ASC
	`
	return r.pool.Query(ctx, query, r.symbols, start, end)
}

func (r *BacktestRouter) scanDepthEvent(rows pgx.Rows) (models.MarketEvent, error) {
	var (
		symbol        string
		ts            time.Time
		bidPrices     []float64
		bidQuantities []float64
		askPrices     []float64
		askQuantities []float64
	)
	if err := rows.Scan(&symbol, &ts, &bidPrices, &bidQuantities, &askPrices, &askQuantities); err != nil {
		return models.MarketEvent{}, err
	}

	payload, err := json.Marshal(map[string]interface{}{
		"bid_prices":     bidPrices,
		"bid_quantities": bidQuantities,
		"ask_prices":     askPrices,
		"ask_quantities": askQuantities,
	})
	if err != nil {
		return models.MarketEvent{}, err
	}

	return models.MarketEvent{
		Symbol:    symbol,
		Timestamp: ts,
		EventType: models.EventDepth,
		Payload:   payload,
		Source:    "backtest",
	}, nil
}

func (r *BacktestRouter) scanTradeEvent(rows pgx.Rows) (models.MarketEvent, error) {
	var (
		symbol       string
		ts           time.Time
		price        float64
		quantity     float64
		isBuyerMaker bool
	)
	if err := rows.Scan(&symbol, &ts, &price, &quantity, &isBuyerMaker); err != nil {
		return models.MarketEvent{}, err
	}

	activationTime, ok := r.activationTimes[symbol]
	if ok && ts.Before(activationTime) {
		return models.MarketEvent{}, errSkipBacktestEvent
	}

	payload, err := json.Marshal(map[string]interface{}{
		"price":          price,
		"quantity":       quantity,
		"is_buyer_maker": isBuyerMaker,
	})
	if err != nil {
		return models.MarketEvent{}, err
	}

	return models.MarketEvent{
		Symbol:    symbol,
		Timestamp: ts,
		EventType: models.EventTrade,
		Payload:   payload,
		Source:    "backtest",
	}, nil
}

func (r *BacktestRouter) Stop() error {
	if r.cancel != nil {
		r.cancel()
	}
	r.logger.Info("backtest router durduruldu")
	return nil
}

type eventCursor struct {
	rows    pgx.Rows
	scan    func(pgx.Rows) (models.MarketEvent, error)
	current *models.MarketEvent
	logger  *zap.Logger
}

func newEventCursor(rows pgx.Rows, scan func(pgx.Rows) (models.MarketEvent, error), logger *zap.Logger) *eventCursor {
	cursor := &eventCursor{
		rows:   rows,
		scan:   scan,
		logger: logger,
	}
	cursor.Advance()
	return cursor
}

func (c *eventCursor) Advance() {
	if c.rows == nil {
		c.current = nil
		return
	}

	for c.rows.Next() {
		event, err := c.scan(c.rows)
		if err != nil {
			if !errors.Is(err, errSkipBacktestEvent) {
				c.logger.Warn("backtest event parse hatasi", zap.Error(err))
			}
			continue
		}
		c.current = &event
		return
	}

	if err := c.rows.Err(); err != nil {
		c.logger.Warn("backtest cursor hatasi", zap.Error(err))
	}
	c.rows.Close()
	c.rows = nil
	c.current = nil
}

func (c *eventCursor) Close() {
	if c.rows != nil {
		c.rows.Close()
		c.rows = nil
	}
	c.current = nil
}

func pickEventCursor(left, right *eventCursor) *eventCursor {
	if left == nil || left.current == nil {
		return right
	}
	if right == nil || right.current == nil {
		return left
	}
	if eventLess(*left.current, *right.current) {
		return left
	}
	return right
}

func eventLess(a, b models.MarketEvent) bool {
	if a.Timestamp.Before(b.Timestamp) {
		return true
	}
	if b.Timestamp.Before(a.Timestamp) {
		return false
	}
	if a.Symbol != b.Symbol {
		return a.Symbol < b.Symbol
	}
	return string(a.EventType) <= string(b.EventType)
}

func paceDuration(prev, current time.Time, speed float64) time.Duration {
	if speed <= 0 || prev.IsZero() || !current.After(prev) {
		return 0
	}
	return time.Duration(float64(current.Sub(prev)) / speed)
}
