package service

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type fakeClickStore struct {
	mu     sync.Mutex
	totals map[string]int64
}

func (s *fakeClickStore) AddClicks(_ context.Context, counts map[string]int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for code, delta := range counts {
		s.totals[code] += delta
	}
	return nil
}

func (s *fakeClickStore) total(code string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.totals[code]
}

func newTestClickCounter() (*ClickCounter, *fakeClickStore) {
	store := &fakeClickStore{totals: make(map[string]int64)}
	return NewClickCounter(store, slog.New(slog.NewTextHandler(io.Discard, nil))), store
}

func TestClickCounterCountsConcurrentClicks(t *testing.T) {
	counter, store := newTestClickCounter()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		counter.Run(ctx)
		close(done)
	}()

	const clicks = 50
	var wg sync.WaitGroup
	for range clicks {
		wg.Go(func() { counter.Record("abc") })
	}
	wg.Wait()

	deadline := time.Now().Add(2 * time.Second)
	for store.total("abc") < clicks && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if got := store.total("abc"); got != clicks {
		t.Errorf("click_count = %d, want %d", got, clicks)
	}
}

func TestClickCounterFlushesBufferOnShutdown(t *testing.T) {
	counter, store := newTestClickCounter()
	for range 10 {
		counter.Record("abc")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	counter.Run(ctx)

	if got := store.total("abc"); got != 10 {
		t.Errorf("click_count = %d, want 10", got)
	}
}

func TestClickCounterDropsWhenBufferFull(t *testing.T) {
	counter, store := newTestClickCounter()
	for range clickBufferSize + 5 {
		counter.Record("abc")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	counter.Run(ctx)

	if got := store.total("abc"); got != clickBufferSize {
		t.Errorf("click_count = %d, want %d", got, clickBufferSize)
	}
}
