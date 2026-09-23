package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// mapLookup adapts a map to LookupFunc so tests never touch the process env.
func mapLookup(env map[string]string) LookupFunc {
	return func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	}
}

const testAdminToken = "0123456789abcdef0123456789abcdef"

func TestLoadAPIDefaults(t *testing.T) {
	cfg, err := LoadAPI(mapLookup(map[string]string{
		"DATABASE_URL_POOLED": "postgres://pooled",
		"ADMIN_TOKEN":         testAdminToken,
		"REDIS_URL":           "redis://localhost:6379/0",
	}))
	if err != nil {
		t.Fatalf("LoadAPI() error = %v", err)
	}

	if cfg.Database.URL != "postgres://pooled" {
		t.Errorf("Database.URL = %q, want the pooled URL", cfg.Database.URL)
	}
	if cfg.HTTP.Addr != ":8080" {
		t.Errorf("HTTP.Addr = %q, want %q", cfg.HTTP.Addr, ":8080")
	}
	if cfg.HTTP.ShutdownTimeout != 15*time.Second {
		t.Errorf("HTTP.ShutdownTimeout = %v, want %v", cfg.HTTP.ShutdownTimeout, 15*time.Second)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want %v", cfg.LogLevel, slog.LevelInfo)
	}
	if cfg.PublicBaseURL != "http://localhost:8080" {
		t.Errorf("PublicBaseURL = %q, want %q", cfg.PublicBaseURL, "http://localhost:8080")
	}
}

func TestLoadAPIOverrides(t *testing.T) {
	cfg, err := LoadAPI(mapLookup(map[string]string{
		"DATABASE_URL_POOLED": "postgres://pooled",
		"ADMIN_TOKEN":         testAdminToken,
		"REDIS_URL":           "redis://localhost:6379/0",
		"PUBLIC_BASE_URL":     "https://sho.rt/",
		"HTTP_ADDR":           ":9090",
		"HTTP_WRITE_TIMEOUT":  "3s",
		"DB_MAX_OPEN_CONNS":   "20",
		"LOG_LEVEL":           "debug",
	}))
	if err != nil {
		t.Fatalf("LoadAPI() error = %v", err)
	}

	if cfg.HTTP.Addr != ":9090" {
		t.Errorf("HTTP.Addr = %q, want %q", cfg.HTTP.Addr, ":9090")
	}
	if cfg.HTTP.WriteTimeout != 3*time.Second {
		t.Errorf("HTTP.WriteTimeout = %v, want %v", cfg.HTTP.WriteTimeout, 3*time.Second)
	}
	if cfg.Database.MaxOpenConns != 20 {
		t.Errorf("Database.MaxOpenConns = %d, want 20", cfg.Database.MaxOpenConns)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want %v", cfg.LogLevel, slog.LevelDebug)
	}
	if cfg.PublicBaseURL != "https://sho.rt" {
		t.Errorf("PublicBaseURL = %q, want trailing slash trimmed", cfg.PublicBaseURL)
	}
}

func TestLoadAPIReportsEveryError(t *testing.T) {
	_, err := LoadAPI(mapLookup(map[string]string{
		"HTTP_READ_TIMEOUT": "soon",
		"DB_MAX_OPEN_CONNS": "-1",
		"LOG_LEVEL":         "loud",
		"ADMIN_TOKEN":       "short",
		"PUBLIC_BASE_URL":   "sho.rt",
	}))
	if err == nil {
		t.Fatal("LoadAPI() error = nil, want an error")
	}

	for _, key := range []string{"DATABASE_URL_POOLED", "REDIS_URL", "HTTP_READ_TIMEOUT", "DB_MAX_OPEN_CONNS", "LOG_LEVEL", "ADMIN_TOKEN", "PUBLIC_BASE_URL"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error %q does not mention %s", err, key)
		}
	}
}

func TestLoadAPIRejectsMoreIdleThanOpenConns(t *testing.T) {
	_, err := LoadAPI(mapLookup(map[string]string{
		"DATABASE_URL_POOLED": "postgres://pooled",
		"ADMIN_TOKEN":         testAdminToken,
		"REDIS_URL":           "redis://localhost:6379/0",
		"DB_MAX_OPEN_CONNS":   "5",
		"DB_MAX_IDLE_CONNS":   "6",
	}))
	if err == nil || !strings.Contains(err.Error(), "DB_MAX_IDLE_CONNS") {
		t.Fatalf("LoadAPI() error = %v, want an error about DB_MAX_IDLE_CONNS", err)
	}
}

func TestLoadMigrateUsesDirectURL(t *testing.T) {
	cfg, err := LoadMigrate(mapLookup(map[string]string{
		"DATABASE_URL":        "postgres://direct",
		"DATABASE_URL_POOLED": "postgres://pooled",
	}))
	if err != nil {
		t.Fatalf("LoadMigrate() error = %v", err)
	}
	if cfg.Database.URL != "postgres://direct" {
		t.Errorf("Database.URL = %q, want the direct URL", cfg.Database.URL)
	}
}
