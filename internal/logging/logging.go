// Package logging builds the structured logger shared by every command.
package logging

import (
	"io"
	"log/slog"

	"url_shortener/internal/buildinfo"
)

// New returns a JSON logger that writes records at or above level to w.
// Every record carries the service name and build version so logs from
// different deployments can be told apart once aggregated.
func New(w io.Writer, service string, level slog.Level) *slog.Logger {
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})

	return slog.New(handler).With(
		slog.String("service", service),
		slog.String("version", buildinfo.Version),
	)
}
