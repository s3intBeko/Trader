package signal

import "github.com/deep-trader/internal/models"

// Re-export for convenience
type SignalType = models.SignalType

const (
	SignalTrendFollow = models.SignalTrendFollow
	SignalPump        = models.SignalPump
	SignalDump        = models.SignalDump
	SignalNoEntry     = models.SignalNoEntry
)
