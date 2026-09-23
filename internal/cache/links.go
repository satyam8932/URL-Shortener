// Package cache is the Redis read-through cache for the redirect hot path.
package cache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const keyPrefix = "link:"

// Links maps short codes to original URLs.
type Links struct {
	client *redis.Client
}

func NewLinks(client *redis.Client) *Links {
	return &Links{client: client}
}

// Get reports whether code is cached, returning its original URL if so.
func (c *Links) Get(ctx context.Context, code string) (string, bool, error) {
	url, err := c.client.Get(ctx, keyPrefix+code).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get cached link: %w", err)
	}
	return url, true, nil
}

func (c *Links) Set(ctx context.Context, code, originalURL string, ttl time.Duration) error {
	if err := c.client.Set(ctx, keyPrefix+code, originalURL, ttl).Err(); err != nil {
		return fmt.Errorf("cache link: %w", err)
	}
	return nil
}

func (c *Links) Delete(ctx context.Context, code string) error {
	if err := c.client.Del(ctx, keyPrefix+code).Err(); err != nil {
		return fmt.Errorf("evict cached link: %w", err)
	}
	return nil
}
