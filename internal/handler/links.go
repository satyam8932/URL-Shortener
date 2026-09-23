package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"url_shortener/internal/model"
)

const maxRequestBodyBytes = 8 << 10

type linkHandler struct {
	logger  *slog.Logger
	links   LinkService
	clicks  ClickRecorder
	baseURL string
}

type shortenRequest struct {
	URL       string `json:"url"`
	Alias     string `json:"alias"`
	ExpiresIn *int64 `json:"expires_in"` // seconds
}

type linkResponse struct {
	ShortCode   string     `json:"short_code"`
	ShortURL    string     `json:"short_url"`
	OriginalURL string     `json:"original_url"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

type statsResponse struct {
	ShortCode   string     `json:"short_code"`
	OriginalURL string     `json:"original_url"`
	ClickCount  int64      `json:"click_count"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	Expired     bool       `json:"expired"`
}

func (h *linkHandler) shorten(w http.ResponseWriter, r *http.Request) {
	var req shortenRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			writeError(w, http.StatusRequestEntityTooLarge, "request body is too large")
			return
		}
		writeError(w, http.StatusBadRequest, "request body must be a JSON object with a url field")
		return
	}

	input, err := req.validate()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	link, created, err := h.links.Create(r.Context(), input)
	switch {
	case errors.Is(err, model.ErrShortCodeTaken):
		writeError(w, http.StatusConflict, "alias is already taken")
		return
	case err != nil:
		h.internalError(w, r, err)
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, linkResponse{
		ShortCode:   link.ShortCode,
		ShortURL:    h.baseURL + "/" + link.ShortCode,
		OriginalURL: link.OriginalURL,
		ExpiresAt:   link.ExpiresAt,
	})
}

// redirect answers with 302, never 301: browsers cache a 301 and stop
// hitting the server, which would silently break click counting.
func (h *linkHandler) redirect(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if !isValidCode(code) {
		writeError(w, http.StatusNotFound, "link not found")
		return
	}

	destination, err := h.links.Resolve(r.Context(), code)
	if err != nil {
		h.lookupError(w, r, err)
		return
	}

	http.Redirect(w, r, destination, http.StatusFound)
	// ServeMux routes HEAD to GET handlers too; link checkers and previews
	// use HEAD, and those are not visits.
	if r.Method == http.MethodGet {
		h.clicks.Record(code)
	}
}

func (h *linkHandler) stats(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if !isValidCode(code) {
		writeError(w, http.StatusNotFound, "link not found")
		return
	}

	link, err := h.links.Stats(r.Context(), code)
	if err != nil {
		h.lookupError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, statsResponse{
		ShortCode:   link.ShortCode,
		OriginalURL: link.OriginalURL,
		ClickCount:  link.ClickCount,
		CreatedAt:   link.CreatedAt,
		ExpiresAt:   link.ExpiresAt,
		Expired:     link.Expired,
	})
}

func (h *linkHandler) delete(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if !isValidCode(code) {
		writeError(w, http.StatusNotFound, "link not found")
		return
	}

	if err := h.links.Delete(r.Context(), code); err != nil {
		h.lookupError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *linkHandler) lookupError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, model.ErrNotFound):
		writeError(w, http.StatusNotFound, "link not found")
	case errors.Is(err, model.ErrExpired):
		writeError(w, http.StatusGone, "link has expired")
	default:
		h.internalError(w, r, err)
	}
}

func (h *linkHandler) internalError(w http.ResponseWriter, r *http.Request, err error) {
	h.logger.ErrorContext(r.Context(), "request failed", slog.Any("error", err))
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(w, http.StatusServiceUnavailable, "request timed out")
		return
	}
	writeError(w, http.StatusInternalServerError, "internal server error")
}
