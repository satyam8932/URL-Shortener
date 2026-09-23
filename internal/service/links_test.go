package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"url_shortener/internal/model"
)

// fakeLinkStore is an in-memory LinkStore that mirrors the database's
// uniqueness rule on short codes.
type fakeLinkStore struct {
	nextID int64
	links  map[string]model.Link
}

func newFakeLinkStore() *fakeLinkStore {
	return &fakeLinkStore{nextID: 1000, links: make(map[string]model.Link)}
}

func (s *fakeLinkStore) NextID(context.Context) (int64, error) {
	s.nextID++
	return s.nextID, nil
}

func (s *fakeLinkStore) Create(_ context.Context, l model.NewLink) (model.Link, error) {
	if _, taken := s.links[l.ShortCode]; taken {
		return model.Link{}, model.ErrShortCodeTaken
	}
	created := model.Link{ID: l.ID, ShortCode: l.ShortCode, OriginalURL: l.OriginalURL, ExpiresAt: l.ExpiresAt}
	s.links[l.ShortCode] = created
	return created, nil
}

func (s *fakeLinkStore) FindByShortCode(_ context.Context, code string) (model.Link, error) {
	l, ok := s.links[code]
	if !ok {
		return model.Link{}, model.ErrNotFound
	}
	return l, nil
}

func (s *fakeLinkStore) FindPermanentByURL(_ context.Context, originalURL string) (model.Link, error) {
	for _, l := range s.links {
		if l.OriginalURL == originalURL && l.ExpiresAt == nil {
			return l, nil
		}
	}
	return model.Link{}, model.ErrNotFound
}

func (s *fakeLinkStore) Delete(_ context.Context, code string) error {
	if _, ok := s.links[code]; !ok {
		return model.ErrNotFound
	}
	delete(s.links, code)
	return nil
}

// fakeLinkCache is an in-memory LinkCache. Setting err makes every call fail,
// as an unreachable Redis would.
type fakeLinkCache struct {
	mu   sync.Mutex
	urls map[string]string
	ttls map[string]time.Duration
	err  error
}

func newFakeLinkCache() *fakeLinkCache {
	return &fakeLinkCache{urls: make(map[string]string), ttls: make(map[string]time.Duration)}
}

func (c *fakeLinkCache) Get(_ context.Context, code string) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return "", false, c.err
	}
	url, ok := c.urls[code]
	return url, ok, nil
}

func (c *fakeLinkCache) Set(_ context.Context, code, originalURL string, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.urls[code] = originalURL
	c.ttls[code] = ttl
	return nil
}

func (c *fakeLinkCache) Delete(_ context.Context, code string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	delete(c.urls, code)
	return nil
}

const testCacheTTL = 10 * time.Minute

func newTestLinks(store LinkStore) (*Links, *fakeLinkCache) {
	cache := newFakeLinkCache()
	return NewLinks(store, cache, testCacheTTL, slog.New(slog.NewTextHandler(io.Discard, nil))), cache
}

func TestCreateGeneratesCodeFromID(t *testing.T) {
	links, _ := newTestLinks(newFakeLinkStore())

	link, created, err := links.Create(context.Background(), CreateInput{URL: "https://example.com"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if !created {
		t.Error("created = false, want true")
	}
	if want := EncodeBase62(link.ID); link.ShortCode != want {
		t.Errorf("ShortCode = %q, want %q", link.ShortCode, want)
	}
}

func TestCreateIsIdempotentForPermanentLinks(t *testing.T) {
	links, _ := newTestLinks(newFakeLinkStore())
	ctx := context.Background()

	first, _, err := links.Create(ctx, CreateInput{URL: "https://example.com"})
	if err != nil {
		t.Fatalf("first Create() error = %v", err)
	}
	second, created, err := links.Create(ctx, CreateInput{URL: "https://example.com"})
	if err != nil {
		t.Fatalf("second Create() error = %v", err)
	}

	if created {
		t.Error("second created = true, want false")
	}
	if second.ShortCode != first.ShortCode {
		t.Errorf("second ShortCode = %q, want %q", second.ShortCode, first.ShortCode)
	}
}

func TestCreateWithExpiryAlwaysCreatesNewLink(t *testing.T) {
	links, _ := newTestLinks(newFakeLinkStore())
	ctx := context.Background()

	permanent, _, _ := links.Create(ctx, CreateInput{URL: "https://example.com"})
	expiring, created, err := links.Create(ctx, CreateInput{URL: "https://example.com", ExpiresIn: time.Hour})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if !created || expiring.ShortCode == permanent.ShortCode {
		t.Errorf("got existing link %q, want a new one", expiring.ShortCode)
	}
	if expiring.ExpiresAt == nil {
		t.Error("ExpiresAt = nil, want a time")
	}
}

func TestCreateWithAlias(t *testing.T) {
	links, _ := newTestLinks(newFakeLinkStore())
	ctx := context.Background()

	link, _, err := links.Create(ctx, CreateInput{URL: "https://example.com", Alias: "promo"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if link.ShortCode != "promo" {
		t.Errorf("ShortCode = %q, want %q", link.ShortCode, "promo")
	}

	_, _, err = links.Create(ctx, CreateInput{URL: "https://other.example", Alias: "promo"})
	if !errors.Is(err, model.ErrShortCodeTaken) {
		t.Errorf("duplicate alias error = %v, want %v", err, model.ErrShortCodeTaken)
	}
}

func TestCreateSkipsCodeTakenByAlias(t *testing.T) {
	store := newFakeLinkStore()
	links, _ := newTestLinks(store)
	ctx := context.Background()

	collidingCode := EncodeBase62(store.nextID + 2)
	if _, _, err := links.Create(ctx, CreateInput{URL: "https://alias.example", Alias: collidingCode}); err != nil {
		t.Fatalf("Create() alias error = %v", err)
	}

	link, _, err := links.Create(ctx, CreateInput{URL: "https://example.com"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if link.ShortCode == collidingCode {
		t.Errorf("generated code %q collides with alias", link.ShortCode)
	}
}

func TestResolve(t *testing.T) {
	store := newFakeLinkStore()
	store.links["live"] = model.Link{ShortCode: "live", OriginalURL: "https://example.com"}
	store.links["gone"] = model.Link{ShortCode: "gone", OriginalURL: "https://example.com", Expired: true}
	links, _ := newTestLinks(store)
	ctx := context.Background()

	if url, err := links.Resolve(ctx, "live"); err != nil || url != "https://example.com" {
		t.Errorf("Resolve(live) = %q, %v, want the destination", url, err)
	}
	if _, err := links.Resolve(ctx, "gone"); !errors.Is(err, model.ErrExpired) {
		t.Errorf("Resolve(gone) error = %v, want %v", err, model.ErrExpired)
	}
	if _, err := links.Resolve(ctx, "missing"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("Resolve(missing) error = %v, want %v", err, model.ErrNotFound)
	}
}

func TestResolveReadsThroughCache(t *testing.T) {
	store := newFakeLinkStore()
	store.links["4c92"] = model.Link{ShortCode: "4c92", OriginalURL: "https://example.com"}
	links, cache := newTestLinks(store)
	ctx := context.Background()

	if _, err := links.Resolve(ctx, "4c92"); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if cache.urls["4c92"] != "https://example.com" || cache.ttls["4c92"] != testCacheTTL {
		t.Fatalf("cache = %q with TTL %v, want the URL with TTL %v", cache.urls["4c92"], cache.ttls["4c92"], testCacheTTL)
	}

	delete(store.links, "4c92")
	if url, err := links.Resolve(ctx, "4c92"); err != nil || url != "https://example.com" {
		t.Errorf("Resolve() from cache = %q, %v, want the cached URL", url, err)
	}
}

func TestResolveCapsCacheTTLAtExpiry(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	soon := now.Add(time.Minute)
	past := now.Add(-time.Minute)

	store := newFakeLinkStore()
	store.links["soon"] = model.Link{ShortCode: "soon", OriginalURL: "https://example.com", ExpiresAt: &soon}
	store.links["unswept"] = model.Link{ShortCode: "unswept", OriginalURL: "https://example.com", ExpiresAt: &past}
	links, cache := newTestLinks(store)
	links.now = func() time.Time { return now }
	ctx := context.Background()

	_, _ = links.Resolve(ctx, "soon")
	if cache.ttls["soon"] != time.Minute {
		t.Errorf("TTL = %v, want %v", cache.ttls["soon"], time.Minute)
	}

	_, _ = links.Resolve(ctx, "unswept")
	if _, cached := cache.urls["unswept"]; cached {
		t.Error("link past its expiry was cached")
	}
}

func TestResolveFallsBackWhenCacheFails(t *testing.T) {
	store := newFakeLinkStore()
	store.links["4c92"] = model.Link{ShortCode: "4c92", OriginalURL: "https://example.com"}
	links, cache := newTestLinks(store)
	cache.err = errors.New("connection refused")

	if url, err := links.Resolve(context.Background(), "4c92"); err != nil || url != "https://example.com" {
		t.Errorf("Resolve() = %q, %v, want the stored URL", url, err)
	}
}

func TestDeleteEvictsCache(t *testing.T) {
	store := newFakeLinkStore()
	store.links["4c92"] = model.Link{ShortCode: "4c92", OriginalURL: "https://example.com"}
	links, cache := newTestLinks(store)
	ctx := context.Background()

	_, _ = links.Resolve(ctx, "4c92")
	if _, cached := cache.urls["4c92"]; !cached {
		t.Fatal("link not cached before Delete")
	}
	if err := links.Delete(ctx, "4c92"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, cached := cache.urls["4c92"]; cached {
		t.Error("link still cached after Delete")
	}
}

// blockingStore holds every lookup until release is closed, so a test can
// pile up concurrent cache misses.
type blockingStore struct {
	*fakeLinkStore
	lookups atomic.Int64
	release chan struct{}
}

func (s *blockingStore) FindByShortCode(ctx context.Context, code string) (model.Link, error) {
	s.lookups.Add(1)
	<-s.release
	return s.fakeLinkStore.FindByShortCode(ctx, code)
}

func TestResolveSharesConcurrentMisses(t *testing.T) {
	store := &blockingStore{fakeLinkStore: newFakeLinkStore(), release: make(chan struct{})}
	store.links["4c92"] = model.Link{ShortCode: "4c92", OriginalURL: "https://example.com"}
	links, _ := newTestLinks(store)

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if url, err := links.Resolve(context.Background(), "4c92"); err != nil || url != "https://example.com" {
				t.Errorf("Resolve() = %q, %v, want the stored URL", url, err)
			}
		})
	}
	time.Sleep(50 * time.Millisecond)
	close(store.release)
	wg.Wait()

	if got := store.lookups.Load(); got != 1 {
		t.Errorf("store lookups = %d, want 1", got)
	}
}
