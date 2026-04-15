package store

import (
	"context"
	"fmt"

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
func (s *Store) SavePaperTrade(ctx context.Context, runID, runMode string, t models.PaperTrade) error {
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
		t.Quantity,
		t.PnL, t.BalanceAfter, t.Reasons,
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
