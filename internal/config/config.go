package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

// LookupFunc resolves an environment variable. os.LookupEnv satisfies it;
// tests pass a map-backed implementation instead of mutating the process env.
type LookupFunc func(key string) (string, bool)

type API struct {
	HTTP     HTTP
	Database Database
	LogLevel slog.Level
}

type Migrate struct {
	Database Database
	LogLevel slog.Level
}

type HTTP struct {
	Addr              string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
}

type Database struct {
	URL             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

// LoadAPI reads the API configuration. The API connects through the pooled
// endpoint (DATABASE_URL_POOLED) since it holds many short-lived connections.
// All problems are reported together rather than one per restart.
func LoadAPI(lookup LookupFunc) (API, error) {
	r := reader{lookup: lookup}

	cfg := API{
		HTTP: HTTP{
			Addr:              r.optionalString("HTTP_ADDR", ":8080"),
			ReadHeaderTimeout: r.positiveDuration("HTTP_READ_HEADER_TIMEOUT", 5*time.Second),
			ReadTimeout:       r.positiveDuration("HTTP_READ_TIMEOUT", 10*time.Second),
			WriteTimeout:      r.positiveDuration("HTTP_WRITE_TIMEOUT", 10*time.Second),
			IdleTimeout:       r.positiveDuration("HTTP_IDLE_TIMEOUT", 60*time.Second),
			ShutdownTimeout:   r.positiveDuration("HTTP_SHUTDOWN_TIMEOUT", 15*time.Second),
		},
		Database: r.database("DATABASE_URL_POOLED"),
		LogLevel: r.logLevel("LOG_LEVEL", slog.LevelInfo),
	}

	return cfg, r.err()
}

// LoadMigrate reads the migration configuration. Migrations connect through
// the direct endpoint (DATABASE_URL), as Neon recommends for schema changes,
// because PgBouncer's transaction pooling does not preserve session state.
func LoadMigrate(lookup LookupFunc) (Migrate, error) {
	r := reader{lookup: lookup}

	cfg := Migrate{
		Database: r.database("DATABASE_URL"),
		LogLevel: r.logLevel("LOG_LEVEL", slog.LevelInfo),
	}

	return cfg, r.err()
}

// reader reads typed values from the environment and accumulates every error
// it encounters, so one call to err reports the whole misconfiguration.
type reader struct {
	lookup LookupFunc
	errs   []error
}

// err returns all accumulated errors joined together, or nil if there were none.
func (r *reader) err() error {
	return errors.Join(r.errs...)
}

// database reads the connection pool settings shared by every command, with
// the connection string taken from urlKey.
func (r *reader) database(urlKey string) Database {
	db := Database{
		URL:             r.requiredString(urlKey),
		MaxOpenConns:    r.positiveInt("DB_MAX_OPEN_CONNS", 10),
		MaxIdleConns:    r.positiveInt("DB_MAX_IDLE_CONNS", 10),
		ConnMaxLifetime: r.positiveDuration("DB_CONN_MAX_LIFETIME", 30*time.Minute),
		ConnMaxIdleTime: r.positiveDuration("DB_CONN_MAX_IDLE_TIME", 5*time.Minute),
	}

	if db.MaxIdleConns > db.MaxOpenConns {
		r.errs = append(r.errs, fmt.Errorf(
			"DB_MAX_IDLE_CONNS (%d) must not exceed DB_MAX_OPEN_CONNS (%d)",
			db.MaxIdleConns, db.MaxOpenConns,
		))
	}

	return db
}

// requiredString returns the value of key, recording an error if it is unset
// or empty.
func (r *reader) requiredString(key string) string {
	value, ok := r.lookup(key)
	if !ok || value == "" {
		r.errs = append(r.errs, fmt.Errorf("%s is required", key))
		return ""
	}
	return value
}

// optionalString returns the value of key, or fallback if it is unset or empty.
func (r *reader) optionalString(key, fallback string) string {
	value, ok := r.lookup(key)
	if !ok || value == "" {
		return fallback
	}
	return value
}

// positiveInt parses key as an integer greater than zero, returning fallback
// if it is unset.
func (r *reader) positiveInt(key string, fallback int) int {
	raw, ok := r.lookup(key)
	if !ok || raw == "" {
		return fallback
	}

	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		r.errs = append(r.errs, fmt.Errorf("%s must be a positive integer, got %q", key, raw))
		return fallback
	}
	return value
}

// positiveDuration parses key as a Go duration string (e.g. "5s", "2m")
// greater than zero, returning fallback if it is unset.
func (r *reader) positiveDuration(key string, fallback time.Duration) time.Duration {
	raw, ok := r.lookup(key)
	if !ok || raw == "" {
		return fallback
	}

	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		r.errs = append(r.errs, fmt.Errorf("%s must be a positive duration such as \"5s\", got %q", key, raw))
		return fallback
	}
	return value
}

// logLevel parses key as a slog level name (debug, info, warn, error),
// returning fallback if it is unset.
func (r *reader) logLevel(key string, fallback slog.Level) slog.Level {
	raw, ok := r.lookup(key)
	if !ok || raw == "" {
		return fallback
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(raw)); err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s must be one of debug, info, warn, error, got %q", key, raw))
		return fallback
	}
	return level
}
