package service

import (
	"context"
	"log/slog"
	"time"
)

type ExpiryStore interface {
	MarkExpired(ctx context.Context, now time.Time) (int, error)
}

// ExpirySweeper periodically flags links past their expiry. Links therefore
// stay reachable for up to one interval after expiring (ARCHITECTURE.md §9).
type ExpirySweeper struct {
	store    ExpiryStore
	interval time.Duration
	logger   *slog.Logger
}

func NewExpirySweeper(store ExpiryStore, interval time.Duration, logger *slog.Logger) *ExpirySweeper {
	return &ExpirySweeper{store: store, interval: interval, logger: logger}
}

// Run sweeps immediately, then every interval until ctx is cancelled.
func (s *ExpirySweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		s.sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *ExpirySweeper) sweep(ctx context.Context) {
	marked, err := s.store.MarkExpired(ctx, time.Now())
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Error("sweep expired links", slog.Any("error", err))
		}
		return
	}
	if marked > 0 {
		s.logger.Info("marked links expired", slog.Int("count", marked))
	}
}
