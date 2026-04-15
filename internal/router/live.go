package router

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/deep-trader/internal/config"
	"github.com/deep-trader/internal/models"
)

// LiveRouter — Binance WebSocket'ten canli veri alir ve paper polling davranisini
// birebir kopyalar: event'leri buffer'da biriktirir, her flush araligi (5sn):
//   - Her sembol icin EN SON depth snapshot'i gonderir (paper: DISTINCT ON symbol)
//   - Biriken TUM trade event'lerini sirali gonderir (paper: WHERE time > last_poll)
//
// Bu sayede live mod, paper moddan ayirt edilemez.
type LiveRouter struct {
	cfg           config.WebSocketConfig
	symbols       []string
	conn          *websocket.Conn
	out           chan models.MarketEvent
	cancel        context.CancelFunc

	// Event buffer
	bufferMu    sync.Mutex
	depthBuffer map[string]models.MarketEvent // symbol -> en son depth (overwrite)
	tradeBuffer []models.MarketEvent          // biriken trade'ler (append)
	flushEvery  time.Duration                 // paper poll araligi (default 5s)

	logger *zap.Logger
}

func NewLiveRouter(cfg config.WebSocketConfig, symbols []string, logger *zap.Logger) *LiveRouter {
	flushEvery := cfg.DepthInterval
	if flushEvery < time.Second {
		flushEvery = 5 * time.Second
	}

	return &LiveRouter{
		cfg:         cfg,
		symbols:     symbols,
		out:         make(chan models.MarketEvent, 1000),
		depthBuffer: make(map[string]models.MarketEvent),
		tradeBuffer: make([]models.MarketEvent, 0, 256),
		flushEvery:  flushEvery,
		logger:      logger,
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

type binanceStreamMsg struct {
	Stream string          `json:"stream"`
	Data   json.RawMessage `json:"data"`
}

type binanceDepth struct {
	Bids [][]string `json:"b"`
	Asks [][]string `json:"a"`
}

type binanceAggTrade struct {
	Price    string `json:"p"`
	Quantity string `json:"q"`
	IsMaker  bool   `json:"m"`
}

func (r *LiveRouter) Start(ctx context.Context) (<-chan models.MarketEvent, error) {
	ctx, r.cancel = context.WithCancel(ctx)

	r.logger.Info("live router baslatiliyor (buffered WebSocket, paper-compat)",
		zap.Int("sembol_sayisi", len(r.symbols)),
		zap.Duration("flush_araligi", r.flushEvery),
	)

	go r.flushLoop(ctx)
	go r.reconnectLoop(ctx)

	return r.out, nil
}

// flushLoop — paper polling davranisi: her flushEvery'de buffer'i bosalt.
// 1) Her sembolun en son depth'ini gonder (paper: DISTINCT ON symbol, time DESC)
// 2) Tum biriken trade'leri sirali gonder (paper: WHERE time > last_poll ORDER BY time)
func (r *LiveRouter) flushLoop(ctx context.Context) {
	ticker := time.NewTicker(r.flushEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			r.flush(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (r *LiveRouter) flush(ctx context.Context) {
	r.bufferMu.Lock()
	depths := r.depthBuffer
	trades := r.tradeBuffer
	r.depthBuffer = make(map[string]models.MarketEvent)
	r.tradeBuffer = make([]models.MarketEvent, 0, 256)
	r.bufferMu.Unlock()

	// Depth'leri gonder (1 per symbol, paper ile ayni)
	for _, event := range depths {
		select {
		case r.out <- event:
		case <-ctx.Done():
			return
		}
	}

	// Trade'leri sirali gonder
	for _, event := range trades {
		select {
		case r.out <- event:
		case <-ctx.Done():
			return
		}
	}
}

func (r *LiveRouter) reconnectLoop(ctx context.Context) {
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

		// Buffer'a ekle (flush goroutine'i gonderecek)
		r.bufferMu.Lock()
		switch event.EventType {
		case models.EventDepth:
			r.depthBuffer[event.Symbol] = event // son depth'i tut (overwrite)
		case models.EventTrade:
			r.tradeBuffer = append(r.tradeBuffer, event) // tum trade'leri birikir
		}
		r.bufferMu.Unlock()
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
