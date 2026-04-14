package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/deep-trader/internal/config"
	"github.com/deep-trader/internal/models"
)

type Store struct {
	pool   *pgxpool.Pool
	logger *zap.Logger
}

func New(ctx context.Context, cfg config.DatabaseConfig, logger *zap.Logger) (*Store, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("DB config parse hatasi: %w", err)
	}
	poolCfg.MaxConns = cfg.MaxConns

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("DB baglanti hatasi: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("DB ping hatasi: %w", err)
	}

	logger.Info("TimescaleDB baglantisi kuruldu",
		zap.String("host", cfg.Host),
		zap.Int("port", cfg.Port),
		zap.String("db", cfg.Name),
	)

	return &Store{pool: pool, logger: logger}, nil
}

func (s *Store) Close() {
	s.pool.Close()
}

func (s *Store) Pool() *pgxpool.Pool {
	return s.pool
}

// backfillDepthQuery — depth_snapshots tablosu array formatinda:
// bid_prices[], bid_quantities[], ask_prices[], ask_quantities[]
const backfillDepthQuery = `
	SELECT
		symbol,
		time AS timestamp,
		'depth' AS event_type,
		jsonb_build_object(
			'bid_prices', bid_prices,
			'bid_quantities', bid_quantities,
			'ask_prices', ask_prices,
			'ask_quantities', ask_quantities
		) AS payload
	FROM depth_snapshots
	WHERE symbol = ANY($1)
	  AND time BETWEEN $2 AND $3
	ORDER BY time ASC
`

// backfillTradeQuery — agg_trades tablosu:
// time, symbol, price, quantity, is_buyer_maker
const backfillTradeQuery = `
	SELECT
		symbol,
		time AS timestamp,
		'trade' AS event_type,
		jsonb_build_object(
			'price', price,
			'quantity', quantity,
			'is_buyer_maker', is_buyer_maker
		) AS payload
	FROM agg_trades
	WHERE symbol = ANY($1)
	  AND time BETWEEN $2 AND $3
	ORDER BY time ASC
`

// backfillQuery — depth + trade birlesik sorgu
const backfillQuery = `
	(
		SELECT symbol, time AS timestamp, 'depth' AS event_type,
			jsonb_build_object(
				'bid_prices', bid_prices,
				'bid_quantities', bid_quantities,
				'ask_prices', ask_prices,
				'ask_quantities', ask_quantities
			) AS payload
		FROM depth_snapshots
		WHERE symbol = ANY($1) AND time BETWEEN $2 AND $3
	)
	UNION ALL
	(
		SELECT symbol, time AS timestamp, 'trade' AS event_type,
			jsonb_build_object(
				'price', price,
				'quantity', quantity,
				'is_buyer_maker', is_buyer_maker
			) AS payload
		FROM agg_trades
		WHERE symbol = ANY($1) AND time BETWEEN $2 AND $3
	)
	ORDER BY timestamp ASC
`

// StreamBacktestEvents — backtest icin DB'den MarketEvent'leri okur ve kanala gonderir.
func (s *Store) StreamBacktestEvents(
	ctx context.Context,
	symbols []string,
	startTime, endTime time.Time,
	out chan<- models.MarketEvent,
) error {
	rows, err := s.pool.Query(ctx, backfillQuery, symbols, startTime, endTime)
	if err != nil {
		return fmt.Errorf("backtest sorgu hatasi: %w", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var (
			symbol    string
			ts        time.Time
			eventType string
			payload   json.RawMessage
		)

		if err := rows.Scan(&symbol, &ts, &eventType, &payload); err != nil {
			s.logger.Error("satir okuma hatasi", zap.Error(err))
			continue
		}

		event := models.MarketEvent{
			Symbol:    symbol,
			Timestamp: ts,
			EventType: models.EventType(eventType),
			Payload:   payload,
			Source:    "backtest",
		}

		select {
		case out <- event:
			count++
		case <-ctx.Done():
			s.logger.Info("backtest akisi iptal edildi", zap.Int("gonderilen", count))
			return ctx.Err()
		}
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("satir iterasyon hatasi: %w", err)
	}

	s.logger.Info("backtest akisi tamamlandi",
		zap.Int("toplam_event", count),
		zap.Time("baslangic", startTime),
		zap.Time("bitis", endTime),
	)

	return nil
}

// SaveBacktestResult — backtest sonucunu DB'ye yazar.
func (s *Store) SaveBacktestResult(ctx context.Context, result BacktestResult) error {
	const query = `
		INSERT INTO backtest_results (
			run_id, symbol, entry_time, exit_time, signal,
			entry_price, exit_price, pnl, confidence, params
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`

	paramsJSON, err := json.Marshal(result.Params)
	if err != nil {
		return fmt.Errorf("params JSON hatasi: %w", err)
	}

	_, err = s.pool.Exec(ctx, query,
		result.RunID,
		result.Symbol,
		result.EntryTime,
		result.ExitTime,
		result.Signal,
		result.EntryPrice,
		result.ExitPrice,
		result.PnL,
		result.Confidence,
		paramsJSON,
	)
	if err != nil {
		return fmt.Errorf("backtest sonuc yazma hatasi: %w", err)
	}

	return nil
}

// EnsureBacktestTable — backtest_results tablosunu olusturur (yoksa).
func (s *Store) EnsureBacktestTable(ctx context.Context) error {
	const ddl = `
		CREATE TABLE IF NOT EXISTS backtest_results (
			run_id       TEXT,
			symbol       TEXT,
			entry_time   TIMESTAMPTZ,
			exit_time    TIMESTAMPTZ,
			signal       TEXT,
			entry_price  DOUBLE PRECISION,
			exit_price   DOUBLE PRECISION,
			pnl          DOUBLE PRECISION,
			confidence   DOUBLE PRECISION,
			params       JSONB,
			created_at   TIMESTAMPTZ DEFAULT NOW()
		)
	`
	_, err := s.pool.Exec(ctx, ddl)
	if err != nil {
		return fmt.Errorf("backtest tablosu olusturma hatasi: %w", err)
	}
	return nil
}

// BacktestResult — backtest sonucu
type BacktestResult struct {
	RunID      string
	Symbol     string
	EntryTime  time.Time
	ExitTime   time.Time
	Signal     string
	EntryPrice float64
	ExitPrice  float64
	PnL        float64
	Confidence float64
	Params     map[string]interface{}
}

// EnsurePaperTables — paper/backtest trading icin gerekli tablolari olusturur.
func (s *Store) EnsurePaperTables(ctx context.Context) error {
	const ddl = `
		CREATE TABLE IF NOT EXISTS paper_signals (
			id              SERIAL,
			run_id          TEXT,
			run_mode        TEXT,
			time            TIMESTAMPTZ DEFAULT NOW(),
			symbol          TEXT,
			signal          TEXT,
			confidence      DOUBLE PRECISION,
			source          TEXT,
			reasons         TEXT[],
			mid_price       DOUBLE PRECISION,
			bid_ask_ratio   DOUBLE PRECISION,
			trade_imbalance DOUBLE PRECISION,
			volume_ratio    DOUBLE PRECISION,
			is_consolidating BOOLEAN,
			funding_rate    DOUBLE PRECISION
		);

		CREATE TABLE IF NOT EXISTS paper_trades (
			id              SERIAL,
			run_id          TEXT,
			run_mode        TEXT,
			symbol          TEXT,
			side            TEXT,
			signal          TEXT,
			entry_time      TIMESTAMPTZ,
			exit_time       TIMESTAMPTZ,
			entry_price     DOUBLE PRECISION,
			exit_price      DOUBLE PRECISION,
			quantity        DOUBLE PRECISION,
			pnl             DOUBLE PRECISION,
			balance_after   DOUBLE PRECISION,
			reasons         TEXT[]
		);
	`
	_, err := s.pool.Exec(ctx, ddl)
	if err != nil {
		return fmt.Errorf("paper tablolari olusturma hatasi: %w", err)
	}
	return nil
}

// SavePaperSignal — sinyal kaydeder.
func (s *Store) SavePaperSignal(ctx context.Context, runID, runMode string, sig models.SignalEvent) error {
	const query = `
		INSERT INTO paper_signals (
			run_id, run_mode, time, symbol, signal, confidence, source, reasons,
			mid_price, bid_ask_ratio, trade_imbalance, volume_ratio,
			is_consolidating, funding_rate
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
	`
	_, err := s.pool.Exec(ctx, query,
		runID, runMode,
		sig.Timestamp, sig.Symbol, string(sig.Signal), sig.Confidence,
		sig.Source, sig.Reasons,
		sig.RawMetrics.MidPrice,
		sig.RawMetrics.OrderBookMetrics.BidAskRatio,
		sig.RawMetrics.TradeFlow.Imbalance,
		sig.RawMetrics.VolumeRatio,
		sig.RawMetrics.IsConsolidating,
		sig.RawMetrics.FundingRate,
	)
	return err
}

// SavePaperTrade — tamamlanan paper trade'i kaydeder.
func (s *Store) SavePaperTrade(ctx context.Context, runID, runMode string, t models.PaperTrade, balanceAfter float64) error {
	const query = `
		INSERT INTO paper_trades (
			run_id, run_mode, symbol, side, signal, entry_time, exit_time,
			entry_price, exit_price, quantity, pnl, balance_after, reasons
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	`
	_, err := s.pool.Exec(ctx, query,
		runID, runMode,
		t.Symbol, t.Side, string(t.Signal),
		t.EntryTime, t.ExitTime,
		t.EntryPrice, t.ExitPrice,
		0.0,
		t.PnL, balanceAfter, t.Reasons,
	)
	return err
}

// FetchActiveSymbols — depth_snapshots'ta verisi olan aktif sembolleri getirir.
// Sadece son 1 saatte depth verisi olan semboller doner (collector'in topladigi $20M+ semboller).
func (s *Store) FetchActiveSymbols(ctx context.Context) ([]string, error) {
	const query = `
		SELECT DISTINCT symbol
		FROM depth_snapshots
		WHERE time > NOW() - INTERVAL '1 hour'
		ORDER BY symbol
	`

	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("aktif sembol sorgu hatasi: %w", err)
	}
	defer rows.Close()

	var symbols []string
	for rows.Next() {
		var sym string
		if err := rows.Scan(&sym); err != nil {
			continue
		}
		symbols = append(symbols, sym)
	}

	return symbols, rows.Err()
}

// IsConsolidating — son N gunde fiyat belirli bir bant icinde mi kontrol eder.
// high/low farki < %5 ise konsolidasyon var (flat).
func (s *Store) IsConsolidating(ctx context.Context, symbol string, days int, threshold float64) (bool, error) {
	const query = `
		SELECT
			COALESCE(MAX(high), 0) AS max_high,
			COALESCE(MIN(low), 0)  AS min_low
		FROM klines_1d
		WHERE symbol = $1
		  AND time >= NOW() - make_interval(days => $2)
	`

	var maxHigh, minLow float64
	err := s.pool.QueryRow(ctx, query, symbol, days).Scan(&maxHigh, &minLow)
	if err != nil {
		return false, fmt.Errorf("konsolidasyon sorgu hatasi: %w", err)
	}

	if minLow == 0 {
		return false, nil
	}

	rangePct := (maxHigh - minLow) / minLow
	return rangePct < threshold, nil
}

// FetchAvgVolume — son N gunluk ortalama hacmi getirir.
func (s *Store) FetchAvgVolume(ctx context.Context, symbol string, days int) (float64, error) {
	const query = `
		SELECT COALESCE(AVG(volume), 0)
		FROM klines_1m
		WHERE symbol = $1
		  AND time >= NOW() - make_interval(days => $2)
	`

	var avg float64
	err := s.pool.QueryRow(ctx, query, symbol, days).Scan(&avg)
	if err != nil {
		return 0, fmt.Errorf("ortalama hacim sorgu hatasi: %w", err)
	}

	return avg, nil
}

// FetchFundingRate — son funding rate degerini getirir.
func (s *Store) FetchFundingRate(ctx context.Context, symbol string) (float64, error) {
	const query = `
		SELECT COALESCE(funding_rate, 0)
		FROM funding_rates
		WHERE symbol = $1
		ORDER BY time DESC
		LIMIT 1
	`

	var rate float64
	err := s.pool.QueryRow(ctx, query, symbol).Scan(&rate)
	if err != nil {
		return 0, fmt.Errorf("funding rate sorgu hatasi: %w", err)
	}

	return rate, nil
}
