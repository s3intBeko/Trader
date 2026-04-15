package router

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/deep-trader/internal/config"
	"github.com/deep-trader/internal/models"
)

type LiveRouter struct {
	cfg            config.WebSocketConfig
	symbols        []string
	conn           *websocket.Conn
	out            chan models.MarketEvent
	cancel         context.CancelFunc
	lastDepthEmit  map[string]time.Time
	depthThrottle  time.Duration
	logger         *zap.Logger
}

func NewLiveRouter(cfg config.WebSocketConfig, symbols []string, logger *zap.Logger) *LiveRouter {
	depthThrottle := cfg.DepthInterval
	if depthThrottle < time.Second {
		depthThrottle = 5 * time.Second
	}

	return &LiveRouter{
		cfg:           cfg,
		symbols:       symbols,
		out:           make(chan models.MarketEvent, 1000),
		lastDepthEmit: make(map[string]time.Time),
		depthThrottle: depthThrottle,
		logger:        logger,
	}
}

func (r *LiveRouter) buildStreamURL() string {
	var streams []string
	for _, sym := range r.symbols {
		s := strings.ToLower(sym)
		streams = append(streams,
			s+"@depth20@100ms",
			s+"@aggTrade",
		)
	}
	return r.cfg.BinanceURL + "/stream?streams=" + strings.Join(streams, "/")
}

// binanceStreamMsg — Binance combined stream mesaj formati
type binanceStreamMsg struct {
	Stream string          `json:"stream"`
	Data   json.RawMessage `json:"data"`
}

// binanceDepth — Binance Futures depth20 payload ("b"/"a" fields)
type binanceDepth struct {
	Bids [][]string `json:"b"`
	Asks [][]string `json:"a"`
}

// binanceAggTrade — Binance aggTrade payload
type binanceAggTrade struct {
	Price    string `json:"p"`
	Quantity string `json:"q"`
	IsMaker  bool   `json:"m"`
}

func (r *LiveRouter) Start(ctx context.Context) (<-chan models.MarketEvent, error) {
	ctx, r.cancel = context.WithCancel(ctx)

	r.logger.Info("live router baslatiliyor (WebSocket)",
		zap.Int("sembol_sayisi", len(r.symbols)),
	)

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
	r.logger.Info("WS baglantisi kuruluyor", zap.String("url", url[:80]+"..."))

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
	default:
		return models.MarketEvent{}, fmt.Errorf("bilinmeyen stream tipi: %s", streamType)
	}

	// Depth throttle — paper uyumluluk
	if eventType == models.EventDepth && r.depthThrottle > 0 {
		now := time.Now()
		if last, ok := r.lastDepthEmit[symbol]; ok && now.Sub(last) < r.depthThrottle {
			return models.MarketEvent{}, fmt.Errorf("depth throttled")
		}
		r.lastDepthEmit[symbol] = now
	}

	payload, err := r.transformPayload(eventType, msg.Data)
	if err != nil {
		return models.MarketEvent{}, fmt.Errorf("transform hatasi: %w", err)
	}

	return models.MarketEvent{
		Symbol:    symbol,
		Timestamp: time.Now(),
		EventType: eventType,
		Payload:   payload,
		Source:    "live",
	}, nil
}

func (r *LiveRouter) transformPayload(eventType models.EventType, raw json.RawMessage) (json.RawMessage, error) {
	switch eventType {
	case models.EventDepth:
		return r.transformDepth(raw)
	case models.EventTrade:
		return r.transformTrade(raw)
	default:
		return raw, nil
	}
}

func (r *LiveRouter) transformDepth(raw json.RawMessage) (json.RawMessage, error) {
	var bd binanceDepth
	if err := json.Unmarshal(raw, &bd); err != nil {
		return nil, err
	}

	bidPrices := make([]float64, 0, len(bd.Bids))
	bidQtys := make([]float64, 0, len(bd.Bids))
	for _, pair := range bd.Bids {
		if len(pair) < 2 {
			continue
		}
		p, _ := strconv.ParseFloat(pair[0], 64)
		q, _ := strconv.ParseFloat(pair[1], 64)
		bidPrices = append(bidPrices, p)
		bidQtys = append(bidQtys, q)
	}

	askPrices := make([]float64, 0, len(bd.Asks))
	askQtys := make([]float64, 0, len(bd.Asks))
	for _, pair := range bd.Asks {
		if len(pair) < 2 {
			continue
		}
		p, _ := strconv.ParseFloat(pair[0], 64)
		q, _ := strconv.ParseFloat(pair[1], 64)
		askPrices = append(askPrices, p)
		askQtys = append(askQtys, q)
	}

	return json.Marshal(map[string]interface{}{
		"bid_prices":     bidPrices,
		"bid_quantities": bidQtys,
		"ask_prices":     askPrices,
		"ask_quantities": askQtys,
	})
}

func (r *LiveRouter) transformTrade(raw json.RawMessage) (json.RawMessage, error) {
	var bt binanceAggTrade
	if err := json.Unmarshal(raw, &bt); err != nil {
		return nil, err
	}

	price, _ := strconv.ParseFloat(bt.Price, 64)
	qty, _ := strconv.ParseFloat(bt.Quantity, 64)

	return json.Marshal(map[string]interface{}{
		"price":          price,
		"quantity":       qty,
		"is_buyer_maker": bt.IsMaker,
	})
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
