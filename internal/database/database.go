// Package database opens the Postgres connection pool and builds the ent
// client on top of it.
package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"url_shortener/ent"
	"url_shortener/internal/config"
)

// pingTimeout bounds the reachability check in Open. It is generous because a
// scaled-to-zero Neon compute can take a few seconds to wake up.
const pingTimeout = 15 * time.Second

// Open creates a connection pool from cfg and verifies the database is
// reachable before returning it. The caller owns the pool and must close it.
//
// Queries run in pgx's QueryExecModeExec: parameters are still sent
// separately from the SQL (no string interpolation), but no named prepared
// statements are created. Named statements are bound to a single server
// connection, which PgBouncer in transaction mode (Neon's pooler) does not
// guarantee between queries.
func Open(ctx context.Context, cfg config.Database) (*sql.DB, error) {
	pgxConfig, err := pgx.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	pgxConfig.DefaultQueryExecMode = pgx.QueryExecModeExec

	db := stdlib.OpenDB(*pgxConfig)
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	db.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)

	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return db, nil
}

// NewClient returns an ent client that runs on db. Closing the client closes
// db as well, so callers should close exactly one of the two.
func NewClient(db *sql.DB) *ent.Client {
	return ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
}
