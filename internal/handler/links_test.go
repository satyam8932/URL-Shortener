package handler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"url_shortener/internal/model"
	"url_shortener/internal/service"
)

const testAdminToken = "test-admin-token"

type fakeLinkService struct {
	link      model.Link
	created   bool
	err       error
	gotInput  service.CreateInput
	callCount int
}

func (f *fakeLinkService) Create(_ context.Context, in service.CreateInput) (model.Link, bool, error) {
	f.callCount++
	f.gotInput = in
	return f.link, f.created, f.err
}

func (f *fakeLinkService) Resolve(context.Context, string) (string, error) {
	f.callCount++
	return f.link.OriginalURL, f.err
}

func (f *fakeLinkService) Stats(context.Context, string) (model.Link, error) {
	f.callCount++
	return f.link, f.err
}

func (f *fakeLinkService) Delete(context.Context, string) error {
	f.callCount++
	return f.err
}

type fakeClicks struct{ recorded []string }

func (f *fakeClicks) Record(code string) { f.recorded = append(f.recorded, code) }

type fakeLimiter struct {
	allow bool
	err   error
}

func (f fakeLimiter) Allow(context.Context, string) (bool, time.Duration, error) {
	return f.allow, 1500 * time.Millisecond, f.err
}

func newLinkRouter(links *fakeLinkService, clicks *fakeClicks, limiter fakeLimiter) http.Handler {
	return NewRouter(Dependencies{
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Links:          links,
		Clicks:         clicks,
		Limiter:        limiter,
		PublicBaseURL:  "https://sho.rt",
		AdminToken:     testAdminToken,
		RequestTimeout: time.Second,
	})
}

func serve(handler http.Handler, method, path, body string, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for key, values := range header {
		req.Header[key] = values
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestShortenValidation(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"not json", `url=https://example.com`},
		{"unknown field", `{"url":"https://example.com","slug":"x"}`},
		{"missing url", `{}`},
		{"relative url", `{"url":"/just/a/path"}`},
		{"unsupported scheme", `{"url":"ftp://example.com/file"}`},
		{"too long url", `{"url":"https://example.com/` + strings.Repeat("a", maxURLLength) + `"}`},
		{"alias too short", `{"url":"https://example.com","alias":"ab"}`},
		{"alias bad chars", `{"url":"https://example.com","alias":"a/b.c"}`},
		{"alias reserved", `{"url":"https://example.com","alias":"healthz"}`},
		{"zero expiry", `{"url":"https://example.com","expires_in":0}`},
		{"negative expiry", `{"url":"https://example.com","expires_in":-5}`},
		{"expiry over one year", `{"url":"https://example.com","expires_in":31536001}`},
		{"expiry that overflows a duration", `{"url":"https://example.com","expires_in":18446744074}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			links := &fakeLinkService{}
			rec := serve(newLinkRouter(links, &fakeClicks{}, fakeLimiter{allow: true}), http.MethodPost, "/shorten", tt.body, nil)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			if links.callCount != 0 {
				t.Error("service called with invalid input")
			}
		})
	}
}

func TestShorten(t *testing.T) {
	expiresAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		body       string
		service    *fakeLinkService
		wantStatus int
		wantBody   string
		wantInput  service.CreateInput
	}{
		{
			name:       "new link",
			body:       `{"url":"https://example.com"}`,
			service:    &fakeLinkService{created: true, link: model.Link{ShortCode: "4c92", OriginalURL: "https://example.com"}},
			wantStatus: http.StatusCreated,
			wantBody:   `{"short_code":"4c92","short_url":"https://sho.rt/4c92","original_url":"https://example.com"}`,
			wantInput:  service.CreateInput{URL: "https://example.com"},
		},
		{
			name:       "existing link",
			body:       `{"url":"https://example.com"}`,
			service:    &fakeLinkService{created: false, link: model.Link{ShortCode: "4c92", OriginalURL: "https://example.com"}},
			wantStatus: http.StatusOK,
			wantBody:   `{"short_code":"4c92","short_url":"https://sho.rt/4c92","original_url":"https://example.com"}`,
			wantInput:  service.CreateInput{URL: "https://example.com"},
		},
		{
			name:       "alias with expiry",
			body:       `{"url":"https://example.com","alias":"promo","expires_in":3600}`,
			service:    &fakeLinkService{created: true, link: model.Link{ShortCode: "promo", OriginalURL: "https://example.com", ExpiresAt: &expiresAt}},
			wantStatus: http.StatusCreated,
			wantBody:   `{"short_code":"promo","short_url":"https://sho.rt/promo","original_url":"https://example.com","expires_at":"2026-01-01T00:00:00Z"}`,
			wantInput:  service.CreateInput{URL: "https://example.com", Alias: "promo", ExpiresIn: time.Hour},
		},
		{
			name:       "alias taken",
			body:       `{"url":"https://example.com","alias":"promo"}`,
			service:    &fakeLinkService{err: model.ErrShortCodeTaken},
			wantStatus: http.StatusConflict,
			wantBody:   `{"error":"alias is already taken"}`,
			wantInput:  service.CreateInput{URL: "https://example.com", Alias: "promo"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(newLinkRouter(tt.service, &fakeClicks{}, fakeLimiter{allow: true}), http.MethodPost, "/shorten", tt.body, nil)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			assertJSONEqual(t, rec.Body.String(), tt.wantBody)
			if tt.service.gotInput != tt.wantInput {
				t.Errorf("service input = %+v, want %+v", tt.service.gotInput, tt.wantInput)
			}
		})
	}
}

func TestShortenRejectsOversizedBody(t *testing.T) {
	body := `{"url":"https://example.com/` + strings.Repeat("a", maxRequestBodyBytes) + `"}`
	rec := serve(newLinkRouter(&fakeLinkService{}, &fakeClicks{}, fakeLimiter{allow: true}), http.MethodPost, "/shorten", body, nil)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestShortenRateLimited(t *testing.T) {
	links := &fakeLinkService{}
	rec := serve(newLinkRouter(links, &fakeClicks{}, fakeLimiter{allow: false}), http.MethodPost, "/shorten", `{"url":"https://example.com"}`, nil)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if got := rec.Header().Get("Retry-After"); got != "2" {
		t.Errorf("Retry-After = %q, want %q", got, "2")
	}
	if links.callCount != 0 {
		t.Error("service called while rate limited")
	}
}

func TestShortenAllowedWhenLimiterUnavailable(t *testing.T) {
	links := &fakeLinkService{created: true, link: model.Link{ShortCode: "4c92"}}
	limiter := fakeLimiter{allow: false, err: errors.New("connection refused")}
	rec := serve(newLinkRouter(links, &fakeClicks{}, limiter), http.MethodPost, "/shorten", `{"url":"https://example.com"}`, nil)

	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
}

func TestRedirect(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		service     *fakeLinkService
		wantStatus  int
		wantClicked bool
	}{
		{"found", "/4c92", &fakeLinkService{link: model.Link{OriginalURL: "https://example.com"}}, http.StatusFound, true},
		{"not found", "/4c92", &fakeLinkService{err: model.ErrNotFound}, http.StatusNotFound, false},
		{"expired", "/4c92", &fakeLinkService{err: model.ErrExpired}, http.StatusGone, false},
		{"invalid code skips lookup", "/favicon.ico", &fakeLinkService{}, http.StatusNotFound, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clicks := &fakeClicks{}
			rec := serve(newLinkRouter(tt.service, clicks, fakeLimiter{}), http.MethodGet, tt.path, "", nil)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantStatus == http.StatusFound && rec.Header().Get("Location") != "https://example.com" {
				t.Errorf("Location = %q, want %q", rec.Header().Get("Location"), "https://example.com")
			}
			if clicked := len(clicks.recorded) == 1; clicked != tt.wantClicked {
				t.Errorf("click recorded = %v, want %v", clicked, tt.wantClicked)
			}
			if tt.path == "/favicon.ico" && tt.service.callCount != 0 {
				t.Error("service called for an invalid code")
			}
		})
	}
}

func TestRedirectHeadIsNotAClick(t *testing.T) {
	clicks := &fakeClicks{}
	links := &fakeLinkService{link: model.Link{OriginalURL: "https://example.com"}}
	rec := serve(newLinkRouter(links, clicks, fakeLimiter{}), http.MethodHead, "/4c92", "", nil)

	if rec.Code != http.StatusFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if len(clicks.recorded) != 0 {
		t.Error("HEAD request recorded as a click")
	}
}

func TestStats(t *testing.T) {
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	links := &fakeLinkService{link: model.Link{ShortCode: "4c92", OriginalURL: "https://example.com", ClickCount: 7, CreatedAt: createdAt}}

	rec := serve(newLinkRouter(links, &fakeClicks{}, fakeLimiter{}), http.MethodGet, "/4c92/stats", "", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	assertJSONEqual(t, rec.Body.String(),
		`{"short_code":"4c92","original_url":"https://example.com","click_count":7,"created_at":"2026-01-01T00:00:00Z","expired":false}`)
}

func TestDelete(t *testing.T) {
	tests := []struct {
		name       string
		auth       string
		service    *fakeLinkService
		wantStatus int
	}{
		{"no token", "", &fakeLinkService{}, http.StatusUnauthorized},
		{"wrong token", "Bearer nope", &fakeLinkService{}, http.StatusUnauthorized},
		{"deleted", "Bearer " + testAdminToken, &fakeLinkService{}, http.StatusNoContent},
		{"scheme is case-insensitive", "bearer " + testAdminToken, &fakeLinkService{}, http.StatusNoContent},
		{"not found", "Bearer " + testAdminToken, &fakeLinkService{err: model.ErrNotFound}, http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			header := http.Header{}
			if tt.auth != "" {
				header.Set("Authorization", tt.auth)
			}
			rec := serve(newLinkRouter(tt.service, &fakeClicks{}, fakeLimiter{}), http.MethodDelete, "/4c92", "", header)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantStatus == http.StatusUnauthorized && tt.service.callCount != 0 {
				t.Error("service called without a valid token")
			}
		})
	}
}
