package ratelimit

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestLimiter returns a limiter on an in-memory Redis whose clock the test
// controls through server.SetTime.
func newTestLimiter(t *testing.T, perMinute, burst int) (*Limiter, *miniredis.Miniredis) {
	t.Helper()
	server := miniredis.RunT(t)
	server.SetTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return New(client, perMinute, burst), server
}

func allow(t *testing.T, l *Limiter, key string) (bool, time.Duration) {
	t.Helper()
	ok, retryAfter, err := l.Allow(context.Background(), key)
	if err != nil {
		t.Fatalf("Allow() error = %v", err)
	}
	return ok, retryAfter
}

func TestAllowUpToBurstThenReject(t *testing.T) {
	l, _ := newTestLimiter(t, 60, 3)

	for i := range 3 {
		if ok, _ := allow(t, l, "1.2.3.4"); !ok {
			t.Fatalf("request %d rejected, want allowed", i+1)
		}
	}

	ok, retryAfter := allow(t, l, "1.2.3.4")
	if ok {
		t.Fatal("request over burst allowed, want rejected")
	}
	if retryAfter != time.Second {
		t.Errorf("retryAfter = %v, want 1s", retryAfter)
	}
}

func TestAllowRefillsOverTime(t *testing.T) {
	l, server := newTestLimiter(t, 60, 1)

	allow(t, l, "1.2.3.4")
	if ok, _ := allow(t, l, "1.2.3.4"); ok {
		t.Fatal("second request allowed, want rejected")
	}

	server.SetTime(time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC))
	if ok, _ := allow(t, l, "1.2.3.4"); !ok {
		t.Error("request after refill rejected, want allowed")
	}
}

func TestAllowKeepsKeysIndependent(t *testing.T) {
	l, _ := newTestLimiter(t, 60, 1)

	allow(t, l, "1.1.1.1")
	if ok, _ := allow(t, l, "2.2.2.2"); !ok {
		t.Error("other client rejected, want allowed")
	}
}

func TestAllowIsExactUnderConcurrency(t *testing.T) {
	const burst = 20
	l, _ := newTestLimiter(t, 60, burst)

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 200 {
		wg.Go(func() {
			if ok, _, err := l.Allow(context.Background(), "1.2.3.4"); err == nil && ok {
				allowed.Add(1)
			}
		})
	}
	wg.Wait()

	if got := allowed.Load(); got != burst {
		t.Errorf("allowed = %d, want exactly %d", got, burst)
	}
}

func TestIdleBucketsExpire(t *testing.T) {
	l, server := newTestLimiter(t, 60, 5)

	allow(t, l, "1.2.3.4")
	if !server.Exists(keyPrefix + "1.2.3.4") {
		t.Fatal("bucket not stored")
	}

	server.FastForward(5 * time.Second)
	if server.Exists(keyPrefix + "1.2.3.4") {
		t.Error("idle bucket kept, want expired")
	}
}
