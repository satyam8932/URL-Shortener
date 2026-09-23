// Package handler is the HTTP transport layer: routing, request validation,
// response encoding and middleware. Business logic lives in service.
package handler

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"url_shortener/internal/model"
	"url_shortener/internal/service"
)

type Pinger interface {
	PingContext(ctx context.Context) error
}

type LinkService interface {
	Create(ctx context.Context, in service.CreateInput) (model.Link, bool, error)
	Resolve(ctx context.Context, code string) (string, error)
	Stats(ctx context.Context, code string) (model.Link, error)
	Delete(ctx context.Context, code string) error
}

type ClickRecorder interface {
	Record(code string)
}

type RateLimiter interface {
	Allow(ctx context.Context, key string) (bool, time.Duration, error)
}

type Dependencies struct {
	Logger         *slog.Logger
	DB             Pinger
	Links          LinkService
	Clicks         ClickRecorder
	Limiter        RateLimiter
	ClientIPHeader string
	PublicBaseURL  string
	AdminToken     string
	RequestTimeout time.Duration
}

// NewRouter registers every route. The deadline wraps logging so the logger
// sees the same request value the mux annotates with its route pattern, and
// logging wraps panic recovery so it records the 500 that recovery writes.
func NewRouter(deps Dependencies) http.Handler {
	links := &linkHandler{
		logger:  deps.Logger,
		links:   deps.Links,
		clicks:  deps.Clicks,
		baseURL: deps.PublicBaseURL,
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", handleLiveness)
	mux.Handle("GET /readyz", handleReadiness(deps.Logger, deps.DB))

	mux.Handle("POST /shorten", rateLimit(deps.Logger, deps.Limiter, deps.ClientIPHeader, http.HandlerFunc(links.shorten)))
	mux.HandleFunc("GET /{code}", links.redirect)
	mux.HandleFunc("GET /{code}/stats", links.stats)
	mux.Handle("DELETE /{code}", requireAdmin(deps.AdminToken, http.HandlerFunc(links.delete)))

	return limitDuration(deps.RequestTimeout, logRequests(deps.Logger, recoverPanics(deps.Logger, mux)))
}
