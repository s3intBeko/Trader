package analyzer

import (
	"encoding/json"
	"sync"

	"go.uber.org/zap"

	"github.com/deep-trader/internal/models"
)

type OrderBookAnalyzer struct {
	books     map[string]*models.OrderBook
	prev      map[string]*models.OrderBookMetrics
	threshold float64 // buyuk emir esigi (USD)
	mu        sync.RWMutex
	logger    *zap.Logger
}

func NewOrderBookAnalyzer(thresholdUSD float64, logger *zap.Logger) *OrderBookAnalyzer {
	return &OrderBookAnalyzer{
		books:     make(map[string]*models.OrderBook),
		prev:      make(map[string]*models.OrderBookMetrics),
		threshold: thresholdUSD,
		logger:    logger,
	}
}

// depthPayload — DB'deki array formatini parse etmek icin
type depthPayload struct {
	BidPrices      []float64 `json:"bid_prices"`
	BidQuantities  []float64 `json:"bid_quantities"`
	AskPrices      []float64 `json:"ask_prices"`
	AskQuantities  []float64 `json:"ask_quantities"`
}

func (oba *OrderBookAnalyzer) Process(e models.MarketEvent) {
	var dp depthPayload
	if err := json.Unmarshal(e.Payload, &dp); err != nil {
		oba.logger.Warn("order book parse hatasi",
			zap.String("symbol", e.Symbol),
			zap.Error(err),
		)
		return
	}

	book := &models.OrderBook{
		Symbol:    e.Symbol,
		Timestamp: e.Timestamp,
	}

	// Array formatindan PriceLevel listesine donustur
	for i := 0; i < len(dp.BidPrices) && i < len(dp.BidQuantities); i++ {
		book.Bids = append(book.Bids, models.PriceLevel{
			Price:    dp.BidPrices[i],
			Quantity: dp.BidQuantities[i],
		})
	}
	for i := 0; i < len(dp.AskPrices) && i < len(dp.AskQuantities); i++ {
		book.Asks = append(book.Asks, models.PriceLevel{
			Price:    dp.AskPrices[i],
			Quantity: dp.AskQuantities[i],
		})
	}

	oba.mu.Lock()
	oba.books[e.Symbol] = book
	oba.mu.Unlock()
}

func (oba *OrderBookAnalyzer) Metrics(symbol string) models.OrderBookMetrics {
	oba.mu.RLock()
	book, ok := oba.books[symbol]
	prev := oba.prev[symbol]
	oba.mu.RUnlock()

	if !ok || book == nil {
		return models.OrderBookMetrics{Symbol: symbol}
	}

	totalBid := sumVolume(book.Bids)
	totalAsk := sumVolume(book.Asks)

	m := models.OrderBookMetrics{
		Symbol:         symbol,
		Timestamp:      book.Timestamp,
		TotalBidVolume: totalBid,
		TotalAskVolume: totalAsk,
	}

	if totalAsk > 0 {
		m.BidAskRatio = totalBid / totalAsk
	}

	// Delta hesapla (onceki snapshot ile karsilastirma)
	if prev != nil {
		m.BidDelta = totalBid - prev.TotalBidVolume
		m.AskDelta = totalAsk - prev.TotalAskVolume
	}

	// Buyuk emir tespiti
	m.LargeOrders = oba.detectLargeOrders(book)

	oba.mu.Lock()
	oba.prev[symbol] = &m
	oba.mu.Unlock()

	return m
}

// MidPrice — order book orta fiyatini dondurur.
func (oba *OrderBookAnalyzer) MidPrice(symbol string) float64 {
	oba.mu.RLock()
	book, ok := oba.books[symbol]
	oba.mu.RUnlock()

	if !ok || book == nil || len(book.Bids) == 0 || len(book.Asks) == 0 {
		return 0
	}

	return (book.Bids[0].Price + book.Asks[0].Price) / 2
}

func (oba *OrderBookAnalyzer) detectLargeOrders(book *models.OrderBook) []models.LargeOrder {
	var large []models.LargeOrder

	midPrice := float64(0)
	if len(book.Bids) > 0 && len(book.Asks) > 0 {
		midPrice = (book.Bids[0].Price + book.Asks[0].Price) / 2
	}

	if midPrice == 0 {
		return large
	}

	for _, bid := range book.Bids {
		usdVal := bid.Quantity * midPrice
		if usdVal >= oba.threshold {
			large = append(large, models.LargeOrder{
				Price:    bid.Price,
				Quantity: bid.Quantity,
				Side:     "bid",
				USDValue: usdVal,
			})
		}
	}

	for _, ask := range book.Asks {
		usdVal := ask.Quantity * midPrice
		if usdVal >= oba.threshold {
			large = append(large, models.LargeOrder{
				Price:    ask.Price,
				Quantity: ask.Quantity,
				Side:     "ask",
				USDValue: usdVal,
			})
		}
	}

	return large
}

func sumVolume(levels []models.PriceLevel) float64 {
	var total float64
	for _, l := range levels {
		total += l.Quantity
	}
	return total
}
