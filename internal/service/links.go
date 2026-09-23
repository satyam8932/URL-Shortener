// Package service holds the business logic. It depends on the store
// interfaces declared here, never on HTTP or on ent.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sync/singleflight"

	"url_shortener/internal/model"
)

// maxCodeAttempts bounds retries when a generated code collides with a custom
// alias. Generated codes never collide with each other, so one retry almost
// always suffices.
const maxCodeAttempts = 3

// lookupTimeout bounds a shared cache-miss lookup. It is detached from the
// first caller's context, so that caller disconnecting does not fail every
// request waiting on the same lookup.
const lookupTimeout = 5 * time.Second

type LinkStore interface {
	NextID(ctx context.Context) (int64, error)
	Create(ctx context.Context, l model.NewLink) (model.Link, error)
	FindByShortCode(ctx context.Context, code string) (model.Link, error)
	FindPermanentByURL(ctx context.Context, originalURL string) (model.Link, error)
	Delete(ctx context.Context, code string) error
}

// LinkCache holds code to URL mappings for the redirect path. It is optional
// at runtime: when it fails, lookups fall back to the store.
type LinkCache interface {
	Get(ctx context.Context, code string) (string, bool, error)
	Set(ctx context.Context, code, originalURL string, ttl time.Duration) error
	Delete(ctx context.Context, code string) error
}

type Links struct {
	store    LinkStore
	cache    LinkCache
	cacheTTL time.Duration
	logger   *slog.Logger
	now      func() time.Time
	lookups  singleflight.Group
}

func NewLinks(store LinkStore, cache LinkCache, cacheTTL time.Duration, logger *slog.Logger) *Links {
	return &Links{store: store, cache: cache, cacheTTL: cacheTTL, logger: logger, now: time.Now}
}

// CreateInput is expected to be validated by the caller.
type CreateInput struct {
	URL       string
	Alias     string        // empty means generate a code
	ExpiresIn time.Duration // zero means never expire
}

// Create returns a link for in.URL and reports whether a new one was created.
// A plain request (no alias, no expiry) reuses an existing permanent link for
// the same URL instead of creating a duplicate.
func (s *Links) Create(ctx context.Context, in CreateInput) (model.Link, bool, error) {
	if in.Alias == "" && in.ExpiresIn == 0 {
		existing, err := s.store.FindPermanentByURL(ctx, in.URL)
		if err == nil {
			return existing, false, nil
		}
		if !errors.Is(err, model.ErrNotFound) {
			return model.Link{}, false, err
		}
	}

	var expiresAt *time.Time
	if in.ExpiresIn > 0 {
		t := s.now().Add(in.ExpiresIn)
		expiresAt = &t
	}

	for range maxCodeAttempts {
		id, err := s.store.NextID(ctx)
		if err != nil {
			return model.Link{}, false, err
		}

		code := in.Alias
		if code == "" {
			code = EncodeBase62(id)
		}

		created, err := s.store.Create(ctx, model.NewLink{
			ID:          id,
			ShortCode:   code,
			OriginalURL: in.URL,
			ExpiresAt:   expiresAt,
		})
		if errors.Is(err, model.ErrShortCodeTaken) && in.Alias == "" {
			continue
		}
		if err != nil {
			return model.Link{}, false, err
		}
		return created, true, nil
	}

	return model.Link{}, false, fmt.Errorf("no free short code after %d attempts", maxCodeAttempts)
}

// Resolve returns the destination for code, or model.ErrExpired once the
// sweeper has flagged the link. Links are read through the cache.
//
// Concurrent misses for the same code share one database lookup. Without
// that, a burst of traffic to an uncached link sends every request to
// Postgres at once and they queue for the small connection pool.
func (s *Links) Resolve(ctx context.Context, code string) (string, error) {
	cached, found, err := s.cache.Get(ctx, code)
	if err != nil {
		s.logger.WarnContext(ctx, "link cache unavailable", slog.Any("error", err))
	}
	if found {
		return cached, nil
	}

	destination, err, _ := s.lookups.Do(code, func() (any, error) {
		lookupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lookupTimeout)
		defer cancel()
		return s.loadAndCache(lookupCtx, code)
	})
	if err != nil {
		return "", err
	}
	return destination.(string), nil
}

func (s *Links) loadAndCache(ctx context.Context, code string) (string, error) {
	l, err := s.store.FindByShortCode(ctx, code)
	if err != nil {
		return "", err
	}
	if l.Expired {
		return "", model.ErrExpired
	}

	if ttl := s.ttlFor(l); ttl > 0 {
		if err := s.cache.Set(ctx, code, l.OriginalURL, ttl); err != nil {
			s.logger.WarnContext(ctx, "link cache unavailable", slog.Any("error", err))
		}
	}
	return l.OriginalURL, nil
}

// ttlFor caps the cache TTL at the link's expiry, so the cache never serves a
// link past expires_at. A link that has expired but is not yet swept gets a
// zero TTL and is not cached.
func (s *Links) ttlFor(l model.Link) time.Duration {
	if l.ExpiresAt == nil {
		return s.cacheTTL
	}
	return min(s.cacheTTL, l.ExpiresAt.Sub(s.now()))
}

func (s *Links) Stats(ctx context.Context, code string) (model.Link, error) {
	return s.store.FindByShortCode(ctx, code)
}

// Delete removes the link and evicts it from the cache. A Resolve that read
// the row just before the delete can still re-cache it, so a deleted link may
// keep redirecting for at most one cache TTL.
func (s *Links) Delete(ctx context.Context, code string) error {
	if err := s.store.Delete(ctx, code); err != nil {
		return err
	}
	if err := s.cache.Delete(ctx, code); err != nil {
		s.logger.ErrorContext(ctx, "evict deleted link from cache", slog.String("code", code), slog.Any("error", err))
	}
	return nil
}
