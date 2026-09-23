// Package ratelimit implements a token bucket limiter per client, stored in
// Redis so every API instance shares the same limits.
package ratelimit

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const keyPrefix = "ratelimit:"

//go:embed tokenbucket.lua
var tokenBucketSource string

var tokenBucket = redis.NewScript(tokenBucketSource)

type Limiter struct {
	client *redis.Client
	rate   float64 // tokens added per second
	burst  int
}

// New allows each key perMinute requests per minute on average, with bursts
// of up to burst requests.
func New(client *redis.Client, perMinute, burst int) *Limiter {
	return &Limiter{
		client: client,
		rate:   float64(perMinute) / 60,
		burst:  burst,
	}
}

// Allow consumes a token for key. When none is left it reports how long until
// the next token is available.
func (l *Limiter) Allow(ctx context.Context, key string) (bool, time.Duration, error) {
	result, err := tokenBucket.Run(ctx, l.client, []string{keyPrefix + key}, l.rate, l.burst).Int64Slice()
	if err != nil {
		return false, 0, fmt.Errorf("run token bucket: %w", err)
	}

	allowed, retryAfter := result[0] == 1, time.Duration(result[1])*time.Millisecond
	return allowed, retryAfter, nil
}
