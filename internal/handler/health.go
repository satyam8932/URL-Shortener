package handler

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// readinessTimeout bounds the database ping in the readiness probe so a hung
// connection is reported as not ready instead of stalling the probe.
const readinessTimeout = 2 * time.Second

// healthResponse is the body returned by the health endpoints.
type healthResponse struct {
	Status string `json:"status"`
}

// handleLiveness reports that the process is up and serving HTTP. It checks no
// dependencies on purpose: a database outage should mark the instance not
// ready, not get it restarted.
func handleLiveness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}

// handleReadiness reports whether the instance can serve traffic, which
// requires a reachable database.
func handleReadiness(logger *slog.Logger, db Pinger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
		defer cancel()

		if err := db.PingContext(ctx); err != nil {
			logger.WarnContext(ctx, "readiness check failed", slog.Any("error", err))
			writeError(w, http.StatusServiceUnavailable, "database unavailable")
			return
		}

		writeJSON(w, http.StatusOK, healthResponse{Status: "ready"})
	})
}
