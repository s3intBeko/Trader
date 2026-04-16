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

// LiveRouter — Binance WebSocket'ten canli veri alir.
// Sembolleri gruplara boler, her grup icin ayri WS baglantisi acar.
// Event buffering ile paper polling davranisini birebir kopyalar.
// Hot symbol update destekler — AddSymbols/RemoveSymbols ile calisirken guncellenir.
type LiveRouter struct {
	cfg        config.WebSocketConfig
	symbols    []string
	out        chan models.MarketEvent
	cancel     context.CancelFunc
	ctx        context.Context
	bootstrapC chan struct{}

	// Aktif sembol takibi — hot update icin
	activeSymbols map[string]bool
	symbolsMu     sync.RWMutex

	// Event buffer
	bufferMu    sync.Mutex
	depthBuffer map[string]models.MarketEvent
	depthQueue  map[string]models.MarketEvent
	tradeAgg    map[string]*tradeAggregation
	flushEvery  time.Duration

	logger *zap.Logger
}

const symbolsPerConnection = 10

// tradeAggregation — sembol basina birikimli trade verisi.
// Bireysel trade event tutmak yerine aggregate tutarak sabit bellek kullanimi saglar.
// Paper'da DB cursor sonsuza buyumez — biz de bellekte buyutmuyoruz.
type tradeAggregation struct {
	BuyQty    float64
	SellQty   float64
	LastPrice float64
	Count     int
}

func NewLiveRouter(cfg config.WebSocketConfig, symbols []string, logger *zap.Logger) *LiveRouter {
	flushEvery := cfg.DepthInterval
	if flushEvery < time.Second {
		flushEvery = 5 * time.Second
	}

	active := make(map[string]bool, len(symbols))
	for _, s := range symbols {
		active[s] = true
	}

	return &LiveRouter{
		cfg:           cfg,
		symbols:       symbols,
		out:           make(chan models.MarketEvent, 5000),
		bootstrapC:    make(chan struct{}),
		activeSymbols: active,
		depthBuffer:   make(map[string]models.MarketEvent),
		depthQueue:    make(map[string]models.MarketEvent),
		tradeAgg:      make(map[string]*tradeAggregation),
		flushEvery:    flushEvery,
		logger:        logger,
	}
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
	r.ctx = ctx

	// Sembolleri gruplara bol
	groups := r.splitSymbolGroups()

	r.logger.Info("live router baslatiliyor (multi-connection, paper-compat)",
		zap.Int("sembol_sayisi", len(r.symbols)),
		zap.Int("baglanti_sayisi", len(groups)),
		zap.Int("sembol_per_conn", symbolsPerConnection),
		zap.Duration("flush_araligi", r.flushEvery),
	)

	// Her grup icin ayri WS goroutine baslat
	for i, group := range groups {
		go r.reconnectLoop(ctx, group, i)
	}

	go r.flushLoop(ctx)

	return r.out, nil
}

func (r *LiveRouter) splitSymbolGroups() [][]string {
	var groups [][]string
	for i := 0; i < len(r.symbols); i += symbolsPerConnection {
		end := i + symbolsPerConnection
		if end > len(r.symbols) {
			end = len(r.symbols)
		}
		groups = append(groups, r.symbols[i:end])
	}
	return groups
}

func (r *LiveRouter) flushLoop(ctx context.Context) {
	defer close(r.out)

	// Bootstrap: ilk depth'leri bekle (max 10sn, sonra devam et)
	select {
	case <-r.bootstrapC:
	case <-time.After(10 * time.Second):
		r.logger.Warn("bootstrap timeout, mevcut depth'lerle devam ediliyor")
	case <-ctx.Done():
		return
	}
	r.flushDepthOnly(ctx)

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

	// Depth cursor: yeni depth'leri queue'ya ekle
	for sym, event := range r.depthBuffer {
		r.depthQueue[sym] = event
	}
	r.depthBuffer = make(map[string]models.MarketEvent)

	// Queue'dan max 100 gonder
	sorted := r.sortedDepths(r.depthQueue)
	sendCount := len(sorted)
	if sendCount > 100 {
		sendCount = 100
	}
	sendDepths := sorted[:sendCount]
	for _, event := range sendDepths {
		delete(r.depthQueue, event.Symbol)
	}

	// Trade: aggregate'den event'lere donustur (sabit bellek, veri kaybi yok)
	// Her sembol icin max 2 event: 1 buy + 1 sell (toplam max 148 event)
	var sendTrades []models.MarketEvent
	for sym, agg := range r.tradeAgg {
		ts := time.Now()
		if agg.BuyQty > 0 {
			payload, _ := json.Marshal(map[string]interface{}{
				"price": agg.LastPrice, "quantity": agg.BuyQty, "is_buyer_maker": false,
			})
			sendTrades = append(sendTrades, models.MarketEvent{
				Symbol: sym, Timestamp: ts, EventType: models.EventTrade, Payload: payload, Source: "live",
			})
		}
		if agg.SellQty > 0 {
			payload, _ := json.Marshal(map[string]interface{}{
				"price": agg.LastPrice, "quantity": agg.SellQty, "is_buyer_maker": true,
			})
			sendTrades = append(sendTrades, models.MarketEvent{
				Symbol: sym, Timestamp: ts, EventType: models.EventTrade, Payload: payload, Source: "live",
			})
		}
	}
	r.tradeAgg = make(map[string]*tradeAggregation)
	r.bufferMu.Unlock()

	for _, event := range sendDepths {
		select {
		case r.out <- event:
		case <-ctx.Done():
			return
		}
	}

	for _, event := range sendTrades {
		select {
		case r.out <- event:
		case <-ctx.Done():
			return
		}
	}
}

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

func (r *LiveRouter) reconnectLoop(ctx context.Context, symbols []string, connID int) {
	backoff := time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := r.connectGroup(ctx, symbols, connID); err != nil {
			r.logger.Warn("WS baglanti koptu",
				zap.Int("conn", connID),
				zap.Int("sembol", len(symbols)),
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

func (r *LiveRouter) connectGroup(ctx context.Context, symbols []string, connID int) error {
	url := r.cfg.BinanceURL + "/stream"

	dialer := websocket.Dialer{
		ReadBufferSize:  65536, // 64KB (default 4KB — depth20 mesajlari buyuk)
		WriteBufferSize: 4096,
	}
	conn, _, err := dialer.DialContext(ctx, url, nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	// SUBSCRIBE
	var streams []string
	for _, sym := range symbols {
		s := strings.ToLower(sym)
		streams = append(streams, s+"@depth20@100ms", s+"@aggTrade")
	}
	subMsg, _ := json.Marshal(map[string]interface{}{
		"method": "SUBSCRIBE",
		"params": streams,
		"id":     connID + 1,
	})
	if err := conn.WriteMessage(websocket.TextMessage, subMsg); err != nil {
		return fmt.Errorf("subscribe hatasi: %w", err)
	}

	r.logger.Info("WS baglanti kuruldu",
		zap.Int("conn", connID),
		zap.Int("sembol", len(symbols)),
		zap.Int("stream", len(streams)),
		zap.String("ilk", symbols[0]),
		zap.String("son", symbols[len(symbols)-1]),
	)

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
			continue
		}

		// Subscribe response'u atla
		if streamMsg.Stream == "" {
			continue
		}

		event, err := r.parseStreamEvent(streamMsg)
		if err != nil {
			continue
		}

		// Aktif sembol kontrolu — cikarilan semboller ignore edilir
		r.symbolsMu.RLock()
		isActive := r.activeSymbols[event.Symbol]
		r.symbolsMu.RUnlock()
		if !isActive {
			continue
		}

		r.bufferMu.Lock()
		switch event.EventType {
		case models.EventDepth:
			r.depthBuffer[event.Symbol] = event
			// Bootstrap kontrolu
			r.symbolsMu.RLock()
			activeCount := len(r.activeSymbols)
			r.symbolsMu.RUnlock()
			// %80 esik — bazi semboller depth gondermeyebilir (dusuk likidite)
			if len(r.depthBuffer) >= activeCount*80/100 {
				select {
				case <-r.bootstrapC:
				default:
					close(r.bootstrapC)
				}
			}
		case models.EventTrade:
			// Trade'i aggregate'e ekle (bireysel event tutmak yerine)
			var tp struct {
				Price    float64 `json:"price"`
				Quantity float64 `json:"quantity"`
				IsMaker  bool    `json:"is_buyer_maker"`
			}
			if json.Unmarshal(event.Payload, &tp) == nil {
				agg, ok := r.tradeAgg[event.Symbol]
				if !ok {
					agg = &tradeAggregation{}
					r.tradeAgg[event.Symbol] = agg
				}
				if tp.IsMaker {
					agg.SellQty += tp.Quantity
				} else {
					agg.BuyQty += tp.Quantity
				}
				agg.LastPrice = tp.Price
				agg.Count++
			}
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

// AddSymbols — calisirken yeni semboller ekler.
// Her yeni sembol grubu icin ayri WS baglantisi acar.
// Analyzer'in yeni semboller icin market data yuklemesi ayrica gerekir.
func (r *LiveRouter) AddSymbols(added []string) {
	if len(added) == 0 {
		return
	}

	// activeSymbols'a ekle
	r.symbolsMu.Lock()
	for _, sym := range added {
		r.activeSymbols[sym] = true
	}
	r.symbols = append(r.symbols, added...)
	r.symbolsMu.Unlock()

	// Yeni semboller icin WS baglantilari ac (10'arli gruplar)
	groups := make([][]string, 0)
	for i := 0; i < len(added); i += symbolsPerConnection {
		end := i + symbolsPerConnection
		if end > len(added) {
			end = len(added)
		}
		groups = append(groups, added[i:end])
	}

	for i, group := range groups {
		connID := 100 + i // mevcut connection ID'lerle carpismasin
		go r.reconnectLoop(r.ctx, group, connID)
	}

	r.logger.Info("yeni semboller eklendi",
		zap.Int("eklenen", len(added)),
		zap.Int("yeni_baglanti", len(groups)),
		zap.Int("toplam_sembol", len(r.symbols)),
	)
}

// RemoveSymbols — calisirken semboller cikarir.
// Cikarilan sembollerden gelen event'ler ignore edilir.
// Acik pozisyonlarin kapatilmasi caller'in sorumlulugunda.
func (r *LiveRouter) RemoveSymbols(removed []string) {
	if len(removed) == 0 {
		return
	}

	r.symbolsMu.Lock()
	for _, sym := range removed {
		delete(r.activeSymbols, sym)
	}
	// symbols listesinden de cikar
	newSymbols := make([]string, 0, len(r.symbols))
	removedSet := make(map[string]bool, len(removed))
	for _, sym := range removed {
		removedSet[sym] = true
	}
	for _, sym := range r.symbols {
		if !removedSet[sym] {
			newSymbols = append(newSymbols, sym)
		}
	}
	r.symbols = newSymbols
	r.symbolsMu.Unlock()

	// Buffer'dan temizle
	r.bufferMu.Lock()
	for _, sym := range removed {
		delete(r.depthBuffer, sym)
		delete(r.depthQueue, sym)
		delete(r.tradeAgg, sym)
	}
	r.bufferMu.Unlock()

	r.logger.Info("semboller cikarildi",
		zap.Int("cikarilan", len(removed)),
		zap.Int("kalan_sembol", len(newSymbols)),
		zap.Strings("cikarilan_liste", removed),
	)
}

// ActiveSymbols — su an aktif sembol listesini dondurur.
func (r *LiveRouter) ActiveSymbols() []string {
	r.symbolsMu.RLock()
	defer r.symbolsMu.RUnlock()
	result := make([]string, len(r.symbols))
	copy(result, r.symbols)
	return result
}

func (r *LiveRouter) Stop() error {
	if r.cancel != nil {
		r.cancel()
	}
	r.logger.Info("live router durduruldu")
	return nil
}
