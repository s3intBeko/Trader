package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	Mode      string          `mapstructure:"mode"`
	Database  DatabaseConfig  `mapstructure:"database"`
	Symbols   SymbolsConfig   `mapstructure:"symbols"`
	WebSocket WebSocketConfig `mapstructure:"websocket"`
	Analyzer  AnalyzerConfig  `mapstructure:"analyzer"`
	Signal    SignalConfig     `mapstructure:"signal"`
	Backtest  BacktestConfig  `mapstructure:"backtest"`
	Executor  ExecutorConfig  `mapstructure:"executor"`
	Web       WebConfig       `mapstructure:"web"`
}

type WebConfig struct {
	Port int `mapstructure:"port"`
}

type DatabaseConfig struct {
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	Name     string `mapstructure:"name"`
	User     string `mapstructure:"user"`
	Password string `mapstructure:"password"`
	MaxConns int32  `mapstructure:"max_conns"`
}

func (d DatabaseConfig) DSN() string {
	password := d.Password
	if strings.HasPrefix(password, "${") && strings.HasSuffix(password, "}") {
		envKey := password[2 : len(password)-1]
		password = os.Getenv(envKey)
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		d.User, password, d.Host, d.Port, d.Name)
}

type SymbolsConfig struct {
	MinMarketCapUSD float64 `mapstructure:"min_market_cap_usd"`
	ExcludeStables  bool    `mapstructure:"exclude_stables"`
	ExcludeLeverage bool    `mapstructure:"exclude_leverage"`
	MaxSymbols      int     `mapstructure:"max_symbols"`
}

type WebSocketConfig struct {
	BinanceURL          string        `mapstructure:"binance_url"`
	DepthLevel          int           `mapstructure:"depth_level"`
	DepthInterval       time.Duration `mapstructure:"depth_interval"`
	ReconnectMaxBackoff time.Duration `mapstructure:"reconnect_max_backoff"`
}

type AnalyzerConfig struct {
	LargeOrderThresholdUSD  float64         `mapstructure:"large_order_threshold_usd"`
	SpoofMaxLifetime        time.Duration   `mapstructure:"spoof_max_lifetime"`
	TradeFlowWindows        []time.Duration `mapstructure:"trade_flow_windows"`
	EmitInterval            time.Duration   `mapstructure:"emit_interval"`
	ConsolidationDays       int             `mapstructure:"consolidation_days"`
	ConsolidationThreshold  float64         `mapstructure:"consolidation_threshold"`
}

type SignalConfig struct {
	MLWeight    float64     `mapstructure:"ml_weight"`
	MLModelPath string      `mapstructure:"ml_model_path"`
	Rules       RulesConfig `mapstructure:"rules"`
}

type RulesConfig struct {
	PumpImbalanceMin  float64 `mapstructure:"pump_imbalance_min"`
	DumpImbalanceMax  float64 `mapstructure:"dump_imbalance_max"`
	BidAskRatioPump   float64 `mapstructure:"bid_ask_ratio_pump"`
	BidAskRatioDump   float64 `mapstructure:"bid_ask_ratio_dump"`
	VolumeRatioMin    float64 `mapstructure:"volume_ratio_min"`
	PriceChangeMax    float64 `mapstructure:"price_change_max"`
	SpoofPenalty      float64 `mapstructure:"spoof_penalty"`
	ConsolidationDays     int     `mapstructure:"consolidation_days"`
	ConsolidationThreshold float64 `mapstructure:"consolidation_threshold"`
}

type BacktestConfig struct {
	StartTimeStr string  `mapstructure:"start_time"`
	EndTimeStr   string  `mapstructure:"end_time"`
	Speed        float64 `mapstructure:"speed"`
	StartTime    time.Time
	EndTime      time.Time
}

type ExecutorConfig struct {
	InitialBalanceUSD float64 `mapstructure:"initial_balance_usd"`
	Leverage          int     `mapstructure:"leverage"`
	MaxPositionPct    float64 `mapstructure:"max_position_pct"`
	StopLossPct       float64 `mapstructure:"stop_loss_pct"`
	DailyLossLimitPct float64 `mapstructure:"daily_loss_limit_pct"`
	TakerFeePct       float64 `mapstructure:"taker_fee_pct"`
	MakerFeePct       float64 `mapstructure:"maker_fee_pct"`
}

func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("yaml")

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("config okuma hatasi: %w", err)
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("config parse hatasi: %w", err)
	}

	if cfg.Mode == "" {
		cfg.Mode = "paper"
	}

	// Backtest zamanlarini parse et
	if cfg.Backtest.StartTimeStr != "" {
		if t, err := time.Parse(time.RFC3339, cfg.Backtest.StartTimeStr); err == nil {
			cfg.Backtest.StartTime = t
		} else if t, err := time.Parse("2006-01-02", cfg.Backtest.StartTimeStr); err == nil {
			cfg.Backtest.StartTime = t
		}
	}
	if cfg.Backtest.EndTimeStr != "" {
		if t, err := time.Parse(time.RFC3339, cfg.Backtest.EndTimeStr); err == nil {
			cfg.Backtest.EndTime = t
		} else if t, err := time.Parse("2006-01-02", cfg.Backtest.EndTimeStr); err == nil {
			cfg.Backtest.EndTime = t
		}
	}

	return &cfg, nil
}
