package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// pingerFunc adapts a function to the Pinger interface.
type pingerFunc func(ctx context.Context) error

func (f pingerFunc) PingContext(ctx context.Context) error { return f(ctx) }

func newTestRouter(db Pinger) http.Handler {
	return NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		DB:     db,
	})
}

func TestHealthEndpoints(t *testing.T) {
	healthy := pingerFunc(func(context.Context) error { return nil })
	unreachable := pingerFunc(func(context.Context) error { return errors.New("connection refused") })

	tests := []struct {
		name       string
		path       string
		db         Pinger
		wantStatus int
		wantBody   string
	}{
		{name: "liveness ignores the database", path: "/healthz", db: unreachable, wantStatus: http.StatusOK, wantBody: `{"status":"ok"}`},
		{name: "ready when database reachable", path: "/readyz", db: healthy, wantStatus: http.StatusOK, wantBody: `{"status":"ready"}`},
		{name: "not ready when database unreachable", path: "/readyz", db: unreachable, wantStatus: http.StatusServiceUnavailable, wantBody: `{"error":"database unavailable"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			newTestRouter(tt.db).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			assertJSONEqual(t, rec.Body.String(), tt.wantBody)
		})
	}
}

func TestRecoverPanicsReturns500(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	panicking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })

	rec := httptest.NewRecorder()
	recoverPanics(logger, panicking).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	assertJSONEqual(t, rec.Body.String(), `{"error":"internal server error"}`)
}

// assertJSONEqual compares two JSON documents semantically, ignoring
// formatting differences such as whitespace and key order.
func assertJSONEqual(t *testing.T, got, want string) {
	t.Helper()

	var gotValue, wantValue any
	if err := json.Unmarshal([]byte(got), &gotValue); err != nil {
		t.Fatalf("response body %q is not valid JSON: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("expected body %q is not valid JSON: %v", want, err)
	}

	gotJSON, _ := json.Marshal(gotValue)
	wantJSON, _ := json.Marshal(wantValue)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("body = %s, want %s", gotJSON, wantJSON)
	}
}
