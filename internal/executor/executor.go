package executor

import (
	"context"

	"github.com/deep-trader/internal/models"
)

type Executor interface {
	Execute(ctx context.Context, signal models.SignalEvent) error
	Close() error
}
