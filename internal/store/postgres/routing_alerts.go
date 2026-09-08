package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// Routing and alerting domain of the Postgres store: routes, escalation
// chains, alerts and notification preferences. Semantics follow
// internal/store/mariadb; see helpers.go for the shared conventions.

// --- Routes ---

func (d *DB) CreateRoute(ctx context.Context, tenantID string, r *store.Route) error {
	if r.ID == "" {
		r.ID = fmt.Sprintf("route-%d", time.Now().UnixNano())
	}
	now := time.Now().UTC()
	r.CreatedAt = now
	r.UpdatedAt = now
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO routes (id, name, source_type, destination_type, filter, senders, enabled, created_at, updated_at, tenant_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		r.ID, r.Name, r.SourceType, r.DestinationType, r.Filter, r.Senders, r.Enabled, r.CreatedAt, r.UpdatedAt, tenantID)
	return err
}

func (d *DB) GetRoute(ctx context.Context, tenantID string, id string) (*store.Route, error) {
	var r store.Route
	err := d.db.QueryRowContext(ctx,
		"SELECT id, name, source_type, destination_type, filter, senders, enabled, created_at, updated_at FROM routes WHERE id = $1 AND tenant_id = $2",
		id, tenantID,
	).Scan(&r.ID, &r.Name, &r.SourceType, &r.DestinationType, &r.Filter, &r.Senders, &r.Enabled, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	r.CreatedAt = utc(r.CreatedAt)
	r.UpdatedAt = utc(r.UpdatedAt)
	return &r, nil
}

func (d *DB) ListRoutes(ctx context.Context, tenantID string) ([]store.Route, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT id, name, source_type, destination_type, filter, senders, enabled, created_at, updated_at FROM routes WHERE tenant_id = $1 ORDER BY name",
		tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var routes []store.Route
	for rows.Next() {
		var r store.Route
		if err := rows.Scan(&r.ID, &r.Name, &r.SourceType, &r.DestinationType, &r.Filter, &r.Senders, &r.Enabled, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.CreatedAt = utc(r.CreatedAt)
		r.UpdatedAt = utc(r.UpdatedAt)
		routes = append(routes, r)
	}
	return routes, rows.Err()
}

func (d *DB) UpdateRoute(ctx context.Context, tenantID string, r *store.Route) error {
	r.UpdatedAt = time.Now().UTC()
	_, err := d.db.ExecContext(ctx,
		"UPDATE routes SET name = $1, source_type = $2, destination_type = $3, filter = $4, senders = $5, enabled = $6, updated_at = $7 WHERE id = $8 AND tenant_id = $9",
		r.Name, r.SourceType, r.DestinationType, r.Filter, r.Senders, r.Enabled, r.UpdatedAt, r.ID, tenantID)
	return err
}

func (d *DB) DeleteRoute(ctx context.Context, tenantID string, id string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM routes WHERE id = $1 AND tenant_id = $2", id, tenantID)
	return err
}

// --- Escalation Chains ---

func (d *DB) CreateEscalationChain(ctx context.Context, tenantID string, c *store.EscalationChain) error {
	if c.ID == "" {
		c.ID = fmt.Sprintf("chain-%d", time.Now().UnixNano())
	}
	tiers, err := jsonBytes(c.Tiers)
	if err != nil {
		return err
	}
	_, err = d.db.ExecContext(ctx,
		"INSERT INTO escalation_chains (id, name, tiers, tenant_id) VALUES ($1, $2, $3, $4)",
		c.ID, c.Name, tiers, tenantID)
	return err
}

// GetEscalationChain looks a chain up by ID; an empty tenantID matches any
// tenant (the escalation engine passes the alert's own tenant, but the
// mariadb store allows the wildcard and callers may rely on it).
func (d *DB) GetEscalationChain(ctx context.Context, tenantID string, id string) (*store.EscalationChain, error) {
	var c store.EscalationChain
	var tiers []byte
	query := "SELECT id, name, tiers, created_at, updated_at FROM escalation_chains WHERE id = $1"
	args := []any{id}
	if tenantID != "" {
		query += " AND tenant_id = $2"
		args = append(args, tenantID)
	}
	if err := d.db.QueryRowContext(ctx, query, args...).Scan(&c.ID, &c.Name, &tiers, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	_ = jsonInto(tiers, &c.Tiers)
	c.CreatedAt = utc(c.CreatedAt)
	c.UpdatedAt = utc(c.UpdatedAt)
	return &c, nil
}

func (d *DB) ListEscalationChains(ctx context.Context, tenantID string) ([]store.EscalationChain, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT id, name, tiers, created_at, updated_at FROM escalation_chains WHERE tenant_id = $1 ORDER BY created_at DESC", tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var chains []store.EscalationChain
	for rows.Next() {
		var c store.EscalationChain
		var tiers []byte
		if err := rows.Scan(&c.ID, &c.Name, &tiers, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		_ = jsonInto(tiers, &c.Tiers)
		c.CreatedAt = utc(c.CreatedAt)
		c.UpdatedAt = utc(c.UpdatedAt)
		chains = append(chains, c)
	}
	return chains, rows.Err()
}

func (d *DB) DeleteEscalationChain(ctx context.Context, tenantID string, id string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM escalation_chains WHERE id = $1 AND tenant_id = $2", id, tenantID)
	return err
}

// --- Alerts ---

// acked_at is NOT NULL with the 1970 sentinel ("not acknowledged"); the
// sentinel is applied on write and stripped on read so a.AckedAt.IsZero()
// keeps its meaning. next_esc_at gets the same treatment for a zero value.

const alertColumns = "id, tenant_id, chain_id, device_imei, type, detail, state, current_tier, retries, acked_by, acked_at, next_esc_at, created_at, updated_at"

func scanAlert(row interface{ Scan(...any) error }, a *store.Alert) error {
	if err := row.Scan(
		&a.ID, &a.TenantID, &a.ChainID, &a.DeviceIMEI, &a.Type, &a.Detail, &a.State,
		&a.CurrentTier, &a.Retries, &a.AckedBy, &a.AckedAt, &a.NextEscAt, &a.CreatedAt, &a.UpdatedAt,
	); err != nil {
		return err
	}
	a.AckedAt = fromSentinel(a.AckedAt)
	a.NextEscAt = fromSentinel(a.NextEscAt)
	a.CreatedAt = utc(a.CreatedAt)
	a.UpdatedAt = utc(a.UpdatedAt)
	return nil
}

// CreateAlert stores the alert as given; the caller (escalation engine)
// assigns the ID and timestamps, as with the mariadb store.
func (d *DB) CreateAlert(ctx context.Context, tenantID string, a *store.Alert) error {
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO alerts (id, chain_id, device_imei, type, detail, state, current_tier, retries,
			acked_by, acked_at, next_esc_at, created_at, updated_at, tenant_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		a.ID, a.ChainID, a.DeviceIMEI, a.Type, a.Detail, a.State, a.CurrentTier, a.Retries,
		a.AckedBy, sentinelTime(a.AckedAt), sentinelTime(a.NextEscAt), a.CreatedAt.UTC(), a.UpdatedAt.UTC(), tenantID)
	return err
}

// GetAlert looks an alert up by ID; an empty tenantID matches any tenant.
func (d *DB) GetAlert(ctx context.Context, tenantID string, id string) (*store.Alert, error) {
	query := "SELECT " + alertColumns + " FROM alerts WHERE id = $1"
	args := []any{id}
	if tenantID != "" {
		query += " AND tenant_id = $2"
		args = append(args, tenantID)
	}
	var a store.Alert
	if err := scanAlert(d.db.QueryRowContext(ctx, query, args...), &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// ListAlerts returns the newest alerts first, at most limit rows. An empty
// tenantID means ALL tenants: the escalation engine's processAlerts calls
// ListAlerts(ctx, "", true, 100) and relies on each returned Alert carrying
// its own TenantID for the tenant-scoped calls that follow.
func (d *DB) ListAlerts(ctx context.Context, tenantID string, activeOnly bool, limit int) ([]store.Alert, error) {
	var conds []string
	var args []any
	if tenantID != "" {
		args = append(args, tenantID)
		conds = append(conds, fmt.Sprintf("tenant_id = $%d", len(args)))
	}
	if activeOnly {
		conds = append(conds, "state IN ('triggered', 'escalating')")
	}
	query := "SELECT " + alertColumns + " FROM alerts"
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, limit)
	query += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", len(args))

	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var alerts []store.Alert
	for rows.Next() {
		var a store.Alert
		if err := scanAlert(rows, &a); err != nil {
			return nil, err
		}
		alerts = append(alerts, a)
	}
	return alerts, rows.Err()
}

// UpdateAlert writes the mutable fields; an empty tenantID matches any tenant.
func (d *DB) UpdateAlert(ctx context.Context, tenantID string, a *store.Alert) error {
	query := "UPDATE alerts SET state = $1, current_tier = $2, retries = $3, acked_by = $4, acked_at = $5, next_esc_at = $6, updated_at = $7 WHERE id = $8"
	args := []any{a.State, a.CurrentTier, a.Retries, a.AckedBy, sentinelTime(a.AckedAt), sentinelTime(a.NextEscAt), a.UpdatedAt.UTC(), a.ID}
	if tenantID != "" {
		query += " AND tenant_id = $9"
		args = append(args, tenantID)
	}
	_, err := d.db.ExecContext(ctx, query, args...)
	return err
}

// --- Notification Preferences ---

func (d *DB) SaveNotificationPref(ctx context.Context, tenantID string, p *store.NotificationPref) error {
	urls, err := jsonBytes(p.URLs)
	if err != nil {
		return err
	}
	events, err := jsonBytes(p.Events)
	if err != nil {
		return err
	}
	_, err = d.db.ExecContext(ctx,
		`INSERT INTO notification_prefs (device_imei, urls, events, enabled, tenant_id)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (device_imei, tenant_id) DO UPDATE SET
			urls = EXCLUDED.urls, events = EXCLUDED.events, enabled = EXCLUDED.enabled, updated_at = now()`,
		p.DeviceIMEI, urls, events, p.Enabled, tenantID)
	return err
}

func (d *DB) GetNotificationPref(ctx context.Context, tenantID string, deviceIMEI string) (*store.NotificationPref, error) {
	var p store.NotificationPref
	var urls, events []byte
	err := d.db.QueryRowContext(ctx,
		"SELECT device_imei, urls, events, enabled, created_at, updated_at FROM notification_prefs WHERE device_imei = $1 AND tenant_id = $2",
		deviceIMEI, tenantID,
	).Scan(&p.DeviceIMEI, &urls, &events, &p.Enabled, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	_ = jsonInto(urls, &p.URLs)
	_ = jsonInto(events, &p.Events)
	p.CreatedAt = utc(p.CreatedAt)
	p.UpdatedAt = utc(p.UpdatedAt)
	return &p, nil
}

func (d *DB) ListNotificationPrefs(ctx context.Context, tenantID string) ([]store.NotificationPref, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT device_imei, urls, events, enabled, created_at, updated_at FROM notification_prefs WHERE tenant_id = $1 ORDER BY device_imei", tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var prefs []store.NotificationPref
	for rows.Next() {
		var p store.NotificationPref
		var urls, events []byte
		if err := rows.Scan(&p.DeviceIMEI, &urls, &events, &p.Enabled, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		_ = jsonInto(urls, &p.URLs)
		_ = jsonInto(events, &p.Events)
		p.CreatedAt = utc(p.CreatedAt)
		p.UpdatedAt = utc(p.UpdatedAt)
		prefs = append(prefs, p)
	}
	return prefs, rows.Err()
}

func (d *DB) DeleteNotificationPref(ctx context.Context, tenantID string, deviceIMEI string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM notification_prefs WHERE device_imei = $1 AND tenant_id = $2", deviceIMEI, tenantID)
	return err
}
