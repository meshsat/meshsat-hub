// Package dbwrap provides an instrumented wrapper around database/sql operations.
package dbwrap

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/observability"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	dbQueryDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "meshsat_hub_db_query_duration_seconds",
		Help:    "Duration of database queries in seconds.",
		Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
	}, []string{"operation", "store"})

	dbQueriesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_db_queries_total",
		Help: "Total database queries by operation, store, and status.",
	}, []string{"operation", "store", "status"})

	dbSlowQueries = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_db_slow_queries_total",
		Help: "Total slow database queries.",
	}, []string{"store"})
)

// SQLDB defines the subset of *sql.DB methods used by store implementations.
type SQLDB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	PingContext(ctx context.Context) error
	Close() error
	Stats() sql.DBStats
}

// ObservedDB wraps a SQLDB with Prometheus metrics and slow query logging.
type ObservedDB struct {
	inner         SQLDB
	storeName     string
	slowThreshold time.Duration
}

// NewObservedDB wraps a SQLDB with instrumentation.
// storeName should be "sqlite" or "mariadb".
// slowThreshold is the duration above which a query is logged as slow (0 = disabled).
func NewObservedDB(db SQLDB, storeName string, slowThreshold time.Duration) *ObservedDB {
	return &ObservedDB{
		inner:         db,
		storeName:     storeName,
		slowThreshold: slowThreshold,
	}
}

func (o *ObservedDB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	start := time.Now()
	result, err := o.inner.ExecContext(ctx, query, args...)
	o.record("exec", start, err)
	return result, err
}

func (o *ObservedDB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	start := time.Now()
	rows, err := o.inner.QueryContext(ctx, query, args...)
	o.record("query", start, err)
	return rows, err
}

func (o *ObservedDB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	start := time.Now()
	row := o.inner.QueryRowContext(ctx, query, args...)
	o.record("queryrow", start, nil)
	return row
}

func (o *ObservedDB) PingContext(ctx context.Context) error {
	return o.inner.PingContext(ctx)
}

func (o *ObservedDB) Close() error {
	return o.inner.Close()
}

func (o *ObservedDB) Stats() sql.DBStats {
	return o.inner.Stats()
}

// Inner returns the underlying SQLDB for operations that need the raw connection.
func (o *ObservedDB) Inner() SQLDB {
	return o.inner
}

func (o *ObservedDB) record(operation string, start time.Time, err error) {
	elapsed := time.Since(start)
	status := "ok"
	if err != nil {
		status = "error"
		observability.RecordError(observability.ErrDatabase, o.storeName)
	}

	dbQueryDuration.WithLabelValues(operation, o.storeName).Observe(elapsed.Seconds())
	dbQueriesTotal.WithLabelValues(operation, o.storeName, status).Inc()

	if o.slowThreshold > 0 && elapsed > o.slowThreshold {
		dbSlowQueries.WithLabelValues(o.storeName).Inc()
		slog.Warn("slow query detected",
			"store", o.storeName,
			"operation", operation,
			"duration_ms", elapsed.Milliseconds(),
		)
	}
}

// PoolStatsCollector periodically exports sql.DBStats as Prometheus metrics.
// Call with a cancel-able context; it runs until the context is done.
func PoolStatsCollector(ctx context.Context, db SQLDB, interval time.Duration) {
	openConns := promauto.NewGauge(prometheus.GaugeOpts{
		Name: "meshsat_hub_db_pool_open_connections",
		Help: "Number of open database connections.",
	})
	idleConns := promauto.NewGauge(prometheus.GaugeOpts{
		Name: "meshsat_hub_db_pool_idle_connections",
		Help: "Number of idle database connections.",
	})
	waitCount := promauto.NewGauge(prometheus.GaugeOpts{
		Name: "meshsat_hub_db_pool_wait_count_total",
		Help: "Total number of connections waited for.",
	})
	waitDuration := promauto.NewGauge(prometheus.GaugeOpts{
		Name: "meshsat_hub_db_pool_wait_duration_seconds_total",
		Help: "Total time blocked waiting for a connection.",
	})

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			stats := db.Stats()
			openConns.Set(float64(stats.OpenConnections))
			idleConns.Set(float64(stats.Idle))
			waitCount.Set(float64(stats.WaitCount))
			waitDuration.Set(stats.WaitDuration.Seconds())
		}
	}
}
