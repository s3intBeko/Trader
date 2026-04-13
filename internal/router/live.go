package router

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/deep-trader/internal/config"
	"github.com/deep-trader/internal/models"
)

type LiveRouter struct {
	cfg     config.WebSocketConfig
	symbols []string
	conn    *websocket.Conn
	out     chan models.MarketEvent
	cancel  context.CancelFunc
	logger  *zap.Logger
}

func NewLiveRouter(cfg config.WebSocketConfig, symbols []string, logger *zap.Logger) *LiveRouter {
	return &LiveRouter{
		cfg:     cfg,
		symbols: symbols,
		out:     make(chan models.MarketEvent, 1000),
		logger:  logger,
	}
}

func (r *LiveRouter) buildStreamURL() string {
	var streams []string
	for _, sym := range r.symbols {
		s := strings.ToLower(sym)
		streams = append(streams,
			s+"@depth20@100ms",
			s+"@aggTrade",
			s+"@kline_1m",
		)
	}
	return r.cfg.BinanceURL + "/stream?streams=" + strings.Join(streams, "/")
}

// binanceStreamMsg — Binance combined stream mesaj formati
type binanceStreamMsg struct {
	Stream string          `json:"stream"`
	Data   json.RawMessage `json:"data"`
}

func (r *LiveRouter) Start(ctx context.Context) (<-chan models.MarketEvent, error) {
	ctx, r.cancel = context.WithCancel(ctx)

	go r.reconnectLoop(ctx)

	return r.out, nil
}

func (r *LiveRouter) reconnectLoop(ctx context.Context) {
	defer close(r.out)

	backoff := time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := r.connect(ctx); err != nil {
			r.logger.Warn("WS baglanti koptu, yeniden deneniyor",
				zap.Duration("backoff", backoff),
				zap.Error(err),
			)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			backoff = min(backoff*2, r.cfg.ReconnectMaxBackoff)
		} else {
			backoff = time.Second
		}
	}
}

func (r *LiveRouter) connect(ctx context.Context) error {
	url := r.buildStreamURL()
	r.logger.Info("WS baglantisi kuruluyor", zap.String("url", url))

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, url, nil)
	if err != nil {
		return err
	}
	r.conn = conn
	defer conn.Close()

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		_, msg, err := conn.ReadMessage()
		if err != nil {
			return err
		}

		var streamMsg binanceStreamMsg
		if err := json.Unmarshal(msg, &streamMsg); err != nil {
			r.logger.Warn("mesaj parse hatasi", zap.Error(err))
			continue
		}

		event, err := r.parseStreamEvent(streamMsg)
		if err != nil {
			continue
		}

		select {
		case r.out <- event:
		case <-ctx.Done():
			return nil
		}
	}
}

func (r *LiveRouter) parseStreamEvent(msg binanceStreamMsg) (models.MarketEvent, error) {
	parts := strings.Split(msg.Stream, "@")
	if len(parts) < 2 {
		return models.MarketEvent{}, fmt.Errorf("gecersiz stream: %s", msg.Stream)
	}

	symbol := strings.ToUpper(parts[0])
	streamType := parts[1]

	var eventType models.EventType
	switch {
	case streamType == "depth20":
		eventType = models.EventDepth
	case streamType == "aggTrade":
		eventType = models.EventTrade
	case strings.HasPrefix(streamType, "kline"):
		eventType = models.EventKline
	default:
		return models.MarketEvent{}, fmt.Errorf("bilinmeyen stream tipi: %s", streamType)
	}

	return models.MarketEvent{
		Symbol:    symbol,
		Timestamp: time.Now(),
		EventType: eventType,
		Payload:   msg.Data,
		Source:    "live",
	}, nil
}

func (r *LiveRouter) Stop() error {
	if r.cancel != nil {
		r.cancel()
	}
	if r.conn != nil {
		r.conn.Close()
	}
	r.logger.Info("live router durduruldu")
	return nil
}
