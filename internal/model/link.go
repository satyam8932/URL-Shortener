// Package model holds the domain types and errors shared across layers, so
// handlers and services never import generated ent types.
package model

import (
	"errors"
	"time"
)

var (
	ErrNotFound       = errors.New("link not found")
	ErrExpired        = errors.New("link expired")
	ErrShortCodeTaken = errors.New("short code already in use")
)

type Link struct {
	ID          int64
	ShortCode   string
	OriginalURL string
	ClickCount  int64
	CreatedAt   time.Time
	ExpiresAt   *time.Time
	Expired     bool
}

type NewLink struct {
	ID          int64
	ShortCode   string
	OriginalURL string
	ExpiresAt   *time.Time
}
