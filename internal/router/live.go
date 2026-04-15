package router

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
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
// Bootstrap: baslangicta ilk depth'leri aninda gonderir (paper: primeDepthSnapshots).
type LiveRouter struct {
	cfg        config.WebSocketConfig
	symbols    []string
	conn       *websocket.Conn
	out        chan models.MarketEvent
	cancel     context.CancelFunc
	bootstrapC chan struct{} // bootstrap tamamlandiginda kapanir

	// Event buffer
	bufferMu    sync.Mutex
	depthBuffer map[string]models.MarketEvent
	depthQueue  map[string]models.MarketEvent // cursor: gonderilmemis depth'ler
	tradeBuffer []models.MarketEvent
	flushEvery  time.Duration

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
		bootstrapC:  make(chan struct{}),
		depthBuffer: make(map[string]models.MarketEvent),
		depthQueue:  make(map[string]models.MarketEvent),
		tradeBuffer: make([]models.MarketEvent, 0, 256),
		flushEvery:  flushEvery,
		logger:      logger,
	}
}

func (r *LiveRouter) buildStreamURL() string {
	// Bos baglanti ac, sonra SUBSCRIBE ile stream ekle (URL uzunluk limiti sorunu onlenir)
	return r.cfg.BinanceURL + "/stream"
}

func (r *LiveRouter) buildSubscribeMessages() [][]byte {
	var streams []string
	for _, sym := range r.symbols {
		s := strings.ToLower(sym)
		streams = append(streams, s+"@depth20@100ms", s+"@aggTrade")
	}

	// Binance SUBSCRIBE limiti: tek mesajda max 200 stream
	// Birden fazla mesaja bol
	var msgs [][]byte
	batchSize := 200
	for i := 0; i < len(streams); i += batchSize {
		end := i + batchSize
		if end > len(streams) {
			end = len(streams)
		}
		msg, _ := json.Marshal(map[string]interface{}{
			"method": "SUBSCRIBE",
			"params": streams[i:end],
			"id":     i/batchSize + 1,
		})
		msgs = append(msgs, msg)
	}
	return msgs
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

	go r.reconnectLoop(ctx)
	go r.flushLoop(ctx)

	return r.out, nil
}

// flushLoop — bootstrap bekle, sonra paper polling gibi periyodik flush.
func (r *LiveRouter) flushLoop(ctx context.Context) {
	defer close(r.out) // paper ile ayni: channel'i kapat

	// Bootstrap: ilk depth snapshot'lari gelmesini bekle, hemen gonder
	select {
	case <-r.bootstrapC:
	case <-ctx.Done():
		return
	}
	r.flushDepthOnly(ctx) // OB'yi aninda doldur (paper: primeDepthSnapshots)

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

// flushDepthOnly — sadece depth buffer'ini gonder (bootstrap icin)
func (r *LiveRouter) flushDepthOnly(ctx context.Context) {
	r.bufferMu.Lock()
	depths := r.depthBuffer
	r.depthBuffer = make(map[string]models.MarketEvent)
	r.bufferMu.Unlock()

	for _, event := range r.sortedDepths(depths) {
		select {
		case r.out <- event:
		case <-ctx.Done():
			return
		}
	}

	r.logger.Info("bootstrap: ilk depth snapshot'lari gonderildi",
		zap.Int("sembol", len(depths)),
	)
}

func (r *LiveRouter) flush(ctx context.Context) {
	r.bufferMu.Lock()

	// === DEPTH: Paper cursor mantigi ===
	// Paper: SELECT ... FROM depth_snapshots ORDER BY time ASC LIMIT 100
	// 74 sembol × 10 depth/sn = 5sn'de ~3700 record → paper sadece 100 okur
	// Cogu sembolun OB'si stale kalir → sinyal stabilitesi artar
	// Biz: depthBuffer sembol basina 1 (max 74) → hepsini gonderirsek paper'dan farkli
	// Cozum: depthQueue'da birikimli cursor — her flush max 100 depth, kalan sonraki flush'a
	depths := r.depthBuffer
	r.depthBuffer = make(map[string]models.MarketEvent)

	// Yeni depth'leri queue'ya ekle (guncel olan overwrite eder)
	for sym, event := range depths {
		r.depthQueue[sym] = event
	}

	// Queue'dan max 100 tane gonder (alfabetik sira, paper ORDER BY time ASC LIMIT 100 ile
	// ayni degil ama tutarli bir sira saglar)
	sorted := r.sortedDepths(r.depthQueue)
	sendCount := len(sorted)
	if sendCount > 100 {
		sendCount = 100
	}
	sendDepths := sorted[:sendCount]

	// Gonderilenleri queue'dan cikar
	for _, event := range sendDepths {
		delete(r.depthQueue, event.Symbol)
	}

	// === TRADE: Paper cursor mantigi ===
	// Paper: LIMIT 500 + lastTradeTime cursor → hic veri kaybetmez
	var sendTrades []models.MarketEvent
	if len(r.tradeBuffer) > 500 {
		sendTrades = r.tradeBuffer[:500]
		remaining := make([]models.MarketEvent, len(r.tradeBuffer)-500)
		copy(remaining, r.tradeBuffer[500:])
		r.tradeBuffer = remaining
	} else {
		sendTrades = r.tradeBuffer
		r.tradeBuffer = make([]models.MarketEvent, 0, 256)
	}
	r.bufferMu.Unlock()

	// Depth gonder
	for _, event := range sendDepths {
		select {
		case r.out <- event:
		case <-ctx.Done():
			return
		}
	}

	// Trade gonder
	for _, event := range sendTrades {
		select {
		case r.out <- event:
		case <-ctx.Done():
			return
		}
	}
}

// sortedDepths — depth map'ini sembol sirasinda dondurur (paper: ORDER BY symbol)
func (r *LiveRouter) sortedDepths(depths map[string]models.MarketEvent) []models.MarketEvent {
	symbols := make([]string, 0, len(depths))
	for sym := range depths {
		symbols = append(symbols, sym)
	}
	sort.Strings(symbols)

	sorted := make([]models.MarketEvent, 0, len(depths))
	for _, sym := range symbols {
		sorted = append(sorted, depths[sym])
	}
	return sorted
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
	r.logger.Info("WS baglantisi kuruluyor", zap.String("url", url))

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, url, nil)
	if err != nil {
		return err
	}
	r.conn = conn
	defer conn.Close()

	// SUBSCRIBE mesajlari gonder (URL yerine — uzunluk limiti sorunu onlenir)
	for _, msg := range r.buildSubscribeMessages() {
		if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			return fmt.Errorf("subscribe hatasi: %w", err)
		}
	}
	r.logger.Info("WS subscribe tamamlandi",
		zap.Int("sembol", len(r.symbols)),
		zap.Int("stream", len(r.symbols)*2),
	)

	bootstrapped := false

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

		r.bufferMu.Lock()
		switch event.EventType {
		case models.EventDepth:
			r.depthBuffer[event.Symbol] = event

			// Bootstrap: tum semboller icin ilk depth geldiginde flush'i tetikle
			if !bootstrapped && len(r.depthBuffer) >= len(r.symbols) {
				bootstrapped = true
				select {
				case <-r.bootstrapC:
				default:
					close(r.bootstrapC)
				}
			}
		case models.EventTrade:
			r.tradeBuffer = append(r.tradeBuffer, event)
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
