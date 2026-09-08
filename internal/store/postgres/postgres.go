// Package postgres implements store.Store on PostgreSQL (CloudNativePG on the
// Kubernetes deployment). Driver: jackc/pgx/v5 through database/sql so the
// dbwrap instrumentation and pool metrics apply unchanged.
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"

	"github.com/meshsat/meshsat-hub/internal/store/dbwrap"
)

// DB implements store.Store with PostgreSQL. The interface assertion lands
// with the last domain port (MESHSAT-903).
type DB struct {
	db    dbwrap.SQLDB
	rawDB *sql.DB // transactions and migrations need the raw handle
}

// New connects to PostgreSQL and returns a DB.
// dsn: "postgres://user:pass@host:5432/dbname?sslmode=require" (pgx URL or
// keyword=value form). slowQueryThreshold controls slow query logging (0 = off).
func New(dsn string, slowQueryThreshold time.Duration) (*DB, error) {
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: open: %w", err)
	}
	conn.SetMaxOpenConns(20)
	conn.SetMaxIdleConns(5)
	// Short lifetimes: after a CNPG switchover the -rw Service points at the
	// new primary, and pooled sockets to the demoted node must die quickly.
	conn.SetConnMaxLifetime(5 * time.Minute)
	conn.SetConnMaxIdleTime(time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.PingContext(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return &DB{
		db:    dbwrap.NewObservedDB(conn, "postgres", slowQueryThreshold),
		rawDB: conn,
	}, nil
}

// RawDB returns the underlying *sql.DB for direct queries.
func (d *DB) RawDB() *sql.DB                 { return d.rawDB }
func (d *DB) Close() error                   { return d.db.Close() }
func (d *DB) Ping(ctx context.Context) error { return d.db.PingContext(ctx) }

// Ready reports whether this connection can take writes: reachable and not a
// replica in recovery. It is the readiness probe for kubernetes mode.
func (d *DB) Ready(ctx context.Context) error {
	var inRecovery bool
	if err := d.db.QueryRowContext(ctx, "SELECT pg_is_in_recovery()").Scan(&inRecovery); err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	if inRecovery {
		return fmt.Errorf("postgres: connected to a replica (in recovery)")
	}
	return nil
}
