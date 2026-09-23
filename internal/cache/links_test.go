package cache

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestCache(t *testing.T) (*Links, *miniredis.Miniredis) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return NewLinks(client), server
}

func TestLinksRoundTrip(t *testing.T) {
	cache, server := newTestCache(t)
	ctx := context.Background()

	if _, found, err := cache.Get(ctx, "4c92"); err != nil || found {
		t.Fatalf("Get() before Set = found %v, err %v; want a miss", found, err)
	}

	if err := cache.Set(ctx, "4c92", "https://example.com", time.Minute); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	url, found, err := cache.Get(ctx, "4c92")
	if err != nil || !found || url != "https://example.com" {
		t.Fatalf("Get() = %q, %v, %v; want the cached URL", url, found, err)
	}

	server.FastForward(time.Minute)
	if _, found, _ := cache.Get(ctx, "4c92"); found {
		t.Error("Get() after TTL = found, want a miss")
	}
}

func TestLinksDelete(t *testing.T) {
	cache, _ := newTestCache(t)
	ctx := context.Background()

	_ = cache.Set(ctx, "4c92", "https://example.com", time.Minute)
	if err := cache.Delete(ctx, "4c92"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, found, _ := cache.Get(ctx, "4c92"); found {
		t.Error("Get() after Delete = found, want a miss")
	}
}

func TestLinksReportsUnavailableRedis(t *testing.T) {
	cache, server := newTestCache(t)
	server.Close()

	if _, _, err := cache.Get(context.Background(), "4c92"); err == nil {
		t.Error("Get() error = nil, want an error when Redis is down")
	}
}
