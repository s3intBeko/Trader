package analyzer

import (
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/deep-trader/internal/models"
)

type TrackedOrder struct {
	Price     float64
	Quantity  float64
	Side      string // "bid" | "ask"
	FirstSeen time.Time
	LastSeen  time.Time
	IsActive  bool
}

func (to *TrackedOrder) USDValue(midPrice float64) float64 {
	return to.Quantity * midPrice
}

type SpoofDetector struct {
	tracked   map[string]map[float64]*TrackedOrder // symbol -> price -> order
	threshold float64                               // USD esigi
	maxLife   time.Duration                         // max yasam suresi
	mu        sync.Mutex
	logger    *zap.Logger
}

func NewSpoofDetector(thresholdUSD float64, maxLifetime time.Duration, logger *zap.Logger) *SpoofDetector {
	return &SpoofDetector{
		tracked:   make(map[string]map[float64]*TrackedOrder),
		threshold: thresholdUSD,
		maxLife:   maxLifetime,
		logger:    logger,
	}
}

// Scan — order book'u tarar, kaybolan buyuk emirleri spoof olarak isaretler.
func (sd *SpoofDetector) Scan(symbol string, book *models.OrderBook) []models.SpoofEvent {
	sd.mu.Lock()
	defer sd.mu.Unlock()

	if _, ok := sd.tracked[symbol]; !ok {
		sd.tracked[symbol] = make(map[float64]*TrackedOrder)
	}

	now := book.Timestamp
	midPrice := float64(0)
	if len(book.Bids) > 0 && len(book.Asks) > 0 {
		midPrice = (book.Bids[0].Price + book.Asks[0].Price) / 2
	}
	if midPrice == 0 {
		return nil
	}

	// Tum mevcut emirleri pasif olarak isaretle
	for _, order := range sd.tracked[symbol] {
		order.IsActive = false
	}

	// Aktif buyuk emirleri guncelle
	sd.updateSide(symbol, book.Bids, "bid", now, midPrice)
	sd.updateSide(symbol, book.Asks, "ask", now, midPrice)

	// Kaybolan buyuk emirleri tespit et
	var suspects []models.SpoofEvent
	for price, order := range sd.tracked[symbol] {
		if !order.IsActive && order.USDValue(midPrice) >= sd.threshold {
			lifetime := order.LastSeen.Sub(order.FirstSeen)
			if lifetime < sd.maxLife && lifetime > 0 {
				suspects = append(suspects, models.SpoofEvent{
					Symbol:   symbol,
					Price:    price,
					Quantity: order.Quantity,
					Side:     order.Side,
					Lifetime: lifetime,
				})
				sd.logger.Debug("spoof suphelisi tespit edildi",
					zap.String("symbol", symbol),
					zap.Float64("fiyat", price),
					zap.Float64("miktar", order.Quantity),
					zap.String("taraf", order.Side),
					zap.Duration("yasam_suresi", lifetime),
				)
			}
			// Takipten cikar
			delete(sd.tracked[symbol], price)
		}
	}

	// Cok eski takip edilen emirleri temizle
	for price, order := range sd.tracked[symbol] {
		if now.Sub(order.LastSeen) > sd.maxLife*10 {
			delete(sd.tracked[symbol], price)
		}
	}

	return suspects
}

func (sd *SpoofDetector) updateSide(symbol string, levels []models.PriceLevel, side string, now time.Time, midPrice float64) {
	for _, level := range levels {
		usdVal := level.Quantity * midPrice
		if usdVal < sd.threshold {
			continue
		}

		if existing, ok := sd.tracked[symbol][level.Price]; ok {
			existing.IsActive = true
			existing.LastSeen = now
			existing.Quantity = level.Quantity
		} else {
			sd.tracked[symbol][level.Price] = &TrackedOrder{
				Price:     level.Price,
				Quantity:  level.Quantity,
				Side:      side,
				FirstSeen: now,
				LastSeen:  now,
				IsActive:  true,
			}
		}
	}
}
