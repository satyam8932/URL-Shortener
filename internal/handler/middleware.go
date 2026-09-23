package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

// logRequests logs one structured record per request once it completes.
// Server errors are logged at error level so they stand out from normal
// traffic.
func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(recorder, r)

		level := slog.LevelInfo
		if recorder.status >= http.StatusInternalServerError {
			level = slog.LevelError
		}

		logger.LogAttrs(r.Context(), level, "http request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("route", r.Pattern),
			slog.Int("status", recorder.status),
			slog.Int("bytes", recorder.bytes),
			slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
		)
	})
}

// limitDuration cancels the request context after timeout. A handler still
// running past the server's write deadline can no longer send its response,
// so its database and Redis calls are stopped too instead of piling up
// behind a slow dependency.
func limitDuration(timeout time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// recoverPanics turns a panic in a handler into a logged 500 response instead
// of a dropped connection. http.ErrAbortHandler is re-raised because it is
// the standard library's deliberate signal to abort the response.
func recoverPanics(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			if err, ok := recovered.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(recovered)
			}

			logger.ErrorContext(r.Context(), "panic while handling request",
				slog.Any("panic", recovered),
				slog.String("stack", string(debug.Stack())),
			)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}()

		next.ServeHTTP(w, r)
	})
}

// statusRecorder wraps an http.ResponseWriter to capture the status code and
// body size for logging.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

// WriteHeader records the status code and forwards it. Only the first call
// counts, matching the behaviour of net/http.
func (r *statusRecorder) WriteHeader(status int) {
	if !r.wroteHeader {
		r.status = status
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(status)
}

// Write records the number of body bytes written and forwards them.
func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wroteHeader = true
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Unwrap exposes the underlying writer so http.ResponseController can reach
// optional interfaces such as http.Flusher through the wrapper.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
