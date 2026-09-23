package service

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

const (
	// clickBufferSize must absorb the clicks that arrive while a flush waits
	// on the database. At ~150ms per flush to a remote Postgres, 10,000 covers
	// roughly 60,000 redirects per second.
	clickBufferSize    = 10_000
	clickFlushInterval = 200 * time.Millisecond
	clickMaxBatch      = 100
	clickFlushTimeout  = 5 * time.Second
)

type ClickStore interface {
	AddClicks(ctx context.Context, counts map[string]int64) error
}

// ClickCounter counts redirects asynchronously so click tracking never adds
// latency to the redirect itself.
type ClickCounter struct {
	store   ClickStore
	logger  *slog.Logger
	clicks  chan string
	dropped atomic.Int64
}

func NewClickCounter(store ClickStore, logger *slog.Logger) *ClickCounter {
	return &ClickCounter{
		store:  store,
		logger: logger,
		clicks: make(chan string, clickBufferSize),
	}
}

// Record never blocks. When the buffer is full the click is dropped, so a
// slow database degrades the counts instead of the redirects.
func (c *ClickCounter) Record(code string) {
	select {
	case c.clicks <- code:
	default:
		c.dropped.Add(1)
	}
}

// Run aggregates clicks per code and writes them every clickFlushInterval, or
// sooner once clickMaxBatch distinct codes are pending. When ctx is cancelled
// it flushes whatever is buffered and returns. Cancel ctx only after the HTTP
// server has stopped, otherwise clicks recorded during shutdown are lost.
func (c *ClickCounter) Run(ctx context.Context) {
	ticker := time.NewTicker(clickFlushInterval)
	defer ticker.Stop()

	pending := make(map[string]int64)
	for {
		select {
		case code := <-c.clicks:
			pending[code]++
			if len(pending) >= clickMaxBatch {
				c.flush(pending)
			}
		case <-ticker.C:
			c.flush(pending)
		case <-ctx.Done():
			c.drain(pending)
			c.flush(pending)
			return
		}
	}
}

func (c *ClickCounter) drain(pending map[string]int64) {
	for {
		select {
		case code := <-c.clicks:
			pending[code]++
		default:
			return
		}
	}
}

// flush writes and clears pending. A failed batch is logged and discarded:
// click counts are best-effort by design (ARCHITECTURE.md §7).
func (c *ClickCounter) flush(pending map[string]int64) {
	if dropped := c.dropped.Swap(0); dropped > 0 {
		c.logger.Warn("click buffer full, clicks dropped", slog.Int64("dropped", dropped))
	}
	if len(pending) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), clickFlushTimeout)
	defer cancel()

	if err := c.store.AddClicks(ctx, pending); err != nil {
		c.logger.Error("flush clicks", slog.Any("error", err), slog.Int("codes", len(pending)))
	}
	clear(pending)
}
