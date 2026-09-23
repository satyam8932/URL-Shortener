// Package handler is the HTTP transport layer: routing, request decoding,
// response encoding and middleware. It translates between HTTP and the
// service layer and holds no business logic of its own.
package handler

import (
	"context"
	"log/slog"
	"net/http"
)

// Pinger reports whether a dependency is reachable. *sql.DB satisfies it.
type Pinger interface {
	PingContext(ctx context.Context) error
}

// Dependencies are the collaborators the HTTP layer needs. They are passed in
// explicitly so handlers can be tested with fakes and nothing is global.
type Dependencies struct {
	Logger *slog.Logger
	DB     Pinger
}

// NewRouter registers every route and wraps them in the middleware applied to
// all requests. Logging is outermost so it records the 500 status written by
// panic recovery.
func NewRouter(deps Dependencies) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", handleLiveness)
	mux.Handle("GET /readyz", handleReadiness(deps.Logger, deps.DB))

	return logRequests(deps.Logger, recoverPanics(deps.Logger, mux))
}
