package database

import (
	"fmt"

	"github.com/redis/go-redis/v9"

	"url_shortener/internal/config"
)

// OpenRedis returns a client for cfg without contacting the server, so the
// caller decides whether an unreachable Redis is fatal.
//
// Retries are disabled: every caller falls back when Redis fails, and
// go-redis's default dial and command retries would otherwise add seconds to
// each request during an outage.
func OpenRedis(cfg config.Redis) (*redis.Client, error) {
	options, err := redis.ParseURL(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	options.DialTimeout = cfg.Timeout
	options.ReadTimeout = cfg.Timeout
	options.WriteTimeout = cfg.Timeout
	options.DialerRetries = 1
	options.MaxRetries = -1

	return redis.NewClient(options), nil
}
