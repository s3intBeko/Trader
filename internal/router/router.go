package router

import (
	"context"

	"github.com/deep-trader/internal/models"
)

// DataRouter — veri kaynagini soyutlayan katman.
// Analyzer her iki modda da ayni MarketEvent struct'ini alir.
type DataRouter interface {
	Start(ctx context.Context) (<-chan models.MarketEvent, error)
	Stop() error
}
