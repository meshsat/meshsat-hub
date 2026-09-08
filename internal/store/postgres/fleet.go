package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// Fleet-side tables: cost ledger, device groups, message templates, alert
// rules and provider credentials (MESHSAT-356). ID generation and ordering
// mirror internal/store/mariadb; credentials follow the sqlite store, which
// is the only production implementation of that table today (MESHSAT-961).

// --- Cost ledger ---

func (d *DB) InsertCostEntry(ctx context.Context, tenantID string, c *store.CostEntry) error {
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO cost_ledger (id, device_imei, interface_type, direction, cost_usd, message_id, detail, tenant_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		c.ID, c.DeviceIMEI, c.InterfaceType, c.Direction, c.CostUSD, c.MessageID, c.Detail, tenantID)
	return err
}

func (d *DB) ListCostEntries(ctx context.Context, tenantID string, deviceIMEI string, from, to time.Time, limit int) ([]store.CostEntry, error) {
	query := "SELECT id, device_imei, interface_type, direction, cost_usd, message_id, detail, created_at FROM cost_ledger WHERE tenant_id=$1"
	args := []any{tenantID}
	if deviceIMEI != "" {
		args = append(args, deviceIMEI)
		query += fmt.Sprintf(" AND device_imei=$%d", len(args))
	}
	if !from.IsZero() {
		args = append(args, from.UTC())
		query += fmt.Sprintf(" AND created_at >= $%d", len(args))
	}
	if !to.IsZero() {
		args = append(args, to.UTC())
		query += fmt.Sprintf(" AND created_at <= $%d", len(args))
	}
	if limit <= 0 {
		limit = 100
	}
	query += fmt.Sprintf(" ORDER BY created_at DESC LIMIT %d", limit)

	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var entries []store.CostEntry
	for rows.Next() {
		var c store.CostEntry
		if err := rows.Scan(&c.ID, &c.DeviceIMEI, &c.InterfaceType, &c.Direction, &c.CostUSD, &c.MessageID, &c.Detail, &c.CreatedAt); err != nil {
			return nil, err
		}
		c.CreatedAt = utc(c.CreatedAt)
		entries = append(entries, c)
	}
	return entries, rows.Err()
}

// AggregateCosts groups the ledger by "month" (YYYY-MM, as MariaDB's
// DATE_FORMAT) or by device (default). "day" (YYYY-MM-DD) and "interface"
// (interface_type) are accepted as well; every key is computed in UTC so the
// buckets do not depend on the session time zone.
func (d *DB) AggregateCosts(ctx context.Context, tenantID string, from, to time.Time, groupBy string) ([]store.CostAggregate, error) {
	var groupExpr string
	switch groupBy {
	case "month":
		groupExpr = "to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM')"
	case "day":
		groupExpr = "to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD')"
	case "interface":
		groupExpr = "interface_type"
	default: // "device"
		groupExpr = "device_imei"
	}
	query := fmt.Sprintf("SELECT %s AS group_key, SUM(cost_usd) AS total_usd, COUNT(*) AS cnt FROM cost_ledger WHERE tenant_id=$1", groupExpr)
	args := []any{tenantID}
	if !from.IsZero() {
		args = append(args, from.UTC())
		query += fmt.Sprintf(" AND created_at >= $%d", len(args))
	}
	if !to.IsZero() {
		args = append(args, to.UTC())
		query += fmt.Sprintf(" AND created_at <= $%d", len(args))
	}
	query += fmt.Sprintf(" GROUP BY %s ORDER BY total_usd DESC", groupExpr)

	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var aggs []store.CostAggregate
	for rows.Next() {
		var a store.CostAggregate
		if err := rows.Scan(&a.GroupKey, &a.TotalUSD, &a.Count); err != nil {
			return nil, err
		}
		aggs = append(aggs, a)
	}
	return aggs, rows.Err()
}

// --- Device groups ---

func (d *DB) CreateDeviceGroup(ctx context.Context, tenantID string, g *store.DeviceGroup) error {
	_, err := d.db.ExecContext(ctx,
		"INSERT INTO device_groups (id, name, description, color, tenant_id) VALUES ($1, $2, $3, $4, $5)",
		g.ID, g.Name, g.Description, g.Color, tenantID)
	return err
}

func (d *DB) GetDeviceGroup(ctx context.Context, tenantID string, id string) (*store.DeviceGroup, error) {
	var g store.DeviceGroup
	err := d.db.QueryRowContext(ctx,
		"SELECT id, name, description, color, created_at, updated_at FROM device_groups WHERE id=$1 AND tenant_id=$2",
		id, tenantID).Scan(&g.ID, &g.Name, &g.Description, &g.Color, &g.CreatedAt, &g.UpdatedAt)
	if err != nil {
		return nil, err
	}
	g.CreatedAt, g.UpdatedAt = utc(g.CreatedAt), utc(g.UpdatedAt)
	_ = d.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM device_group_members WHERE group_id=$1 AND tenant_id=$2",
		id, tenantID).Scan(&g.MemberCount)
	return &g, nil
}

func (d *DB) ListDeviceGroups(ctx context.Context, tenantID string) ([]store.DeviceGroup, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT g.id, g.name, g.description, g.color, g.created_at, g.updated_at,
		 (SELECT COUNT(*) FROM device_group_members m WHERE m.group_id=g.id AND m.tenant_id=g.tenant_id)
		 FROM device_groups g WHERE g.tenant_id=$1 ORDER BY g.name`, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var groups []store.DeviceGroup
	for rows.Next() {
		var g store.DeviceGroup
		if err := rows.Scan(&g.ID, &g.Name, &g.Description, &g.Color, &g.CreatedAt, &g.UpdatedAt, &g.MemberCount); err != nil {
			return nil, err
		}
		g.CreatedAt, g.UpdatedAt = utc(g.CreatedAt), utc(g.UpdatedAt)
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

func (d *DB) UpdateDeviceGroup(ctx context.Context, tenantID string, g *store.DeviceGroup) error {
	_, err := d.db.ExecContext(ctx,
		"UPDATE device_groups SET name=$1, description=$2, color=$3, updated_at=now() WHERE id=$4 AND tenant_id=$5",
		g.Name, g.Description, g.Color, g.ID, tenantID)
	return err
}

func (d *DB) DeleteDeviceGroup(ctx context.Context, tenantID string, id string) error {
	_, _ = d.db.ExecContext(ctx, "DELETE FROM device_group_members WHERE group_id=$1 AND tenant_id=$2", id, tenantID)
	_, err := d.db.ExecContext(ctx, "DELETE FROM device_groups WHERE id=$1 AND tenant_id=$2", id, tenantID)
	return err
}

// AddDeviceToGroup is idempotent (MariaDB INSERT IGNORE).
func (d *DB) AddDeviceToGroup(ctx context.Context, tenantID string, groupID, deviceIMEI string) error {
	_, err := d.db.ExecContext(ctx,
		"INSERT INTO device_group_members (group_id, device_imei, tenant_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING",
		groupID, deviceIMEI, tenantID)
	return err
}

func (d *DB) RemoveDeviceFromGroup(ctx context.Context, tenantID string, groupID, deviceIMEI string) error {
	_, err := d.db.ExecContext(ctx,
		"DELETE FROM device_group_members WHERE group_id=$1 AND device_imei=$2 AND tenant_id=$3",
		groupID, deviceIMEI, tenantID)
	return err
}

func (d *DB) ListDevicesInGroup(ctx context.Context, tenantID string, groupID string) ([]store.Device, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT d.imei, d.label, d.type, d.notes, d.last_seen, d.created_at, d.updated_at
		 FROM devices d JOIN device_group_members m ON d.imei=m.device_imei AND d.tenant_id=m.tenant_id
		 WHERE m.group_id=$1 AND m.tenant_id=$2 ORDER BY d.label`, groupID, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var devices []store.Device
	for rows.Next() {
		var dev store.Device
		if err := rows.Scan(&dev.IMEI, &dev.Label, &dev.Type, &dev.Notes, &dev.LastSeen, &dev.CreatedAt, &dev.UpdatedAt); err != nil {
			return nil, err
		}
		dev.LastSeen = fromSentinel(dev.LastSeen)
		dev.CreatedAt, dev.UpdatedAt = utc(dev.CreatedAt), utc(dev.UpdatedAt)
		devices = append(devices, dev)
	}
	return devices, rows.Err()
}

func (d *DB) ListGroupsForDevice(ctx context.Context, tenantID string, deviceIMEI string) ([]store.DeviceGroup, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT g.id, g.name, g.description, g.color, g.created_at, g.updated_at
		 FROM device_groups g JOIN device_group_members m ON g.id=m.group_id AND g.tenant_id=m.tenant_id
		 WHERE m.device_imei=$1 AND m.tenant_id=$2 ORDER BY g.name`, deviceIMEI, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var groups []store.DeviceGroup
	for rows.Next() {
		var g store.DeviceGroup
		if err := rows.Scan(&g.ID, &g.Name, &g.Description, &g.Color, &g.CreatedAt, &g.UpdatedAt); err != nil {
			return nil, err
		}
		g.CreatedAt, g.UpdatedAt = utc(g.CreatedAt), utc(g.UpdatedAt)
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

// --- Message templates ---

func (d *DB) CreateMessageTemplate(ctx context.Context, tenantID string, t *store.MessageTemplate) error {
	if t.ID == "" {
		t.ID = fmt.Sprintf("tmpl-%d", time.Now().UnixNano())
	}
	now := time.Now().UTC()
	t.CreatedAt = now
	t.UpdatedAt = now
	vars, _ := json.Marshal(t.Variables)
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO message_templates (id, name, body, variables, created_at, updated_at, tenant_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		t.ID, t.Name, t.Body, string(vars), t.CreatedAt, t.UpdatedAt, tenantID)
	return err
}

func (d *DB) GetMessageTemplate(ctx context.Context, tenantID string, id string) (*store.MessageTemplate, error) {
	var t store.MessageTemplate
	var vars string
	err := d.db.QueryRowContext(ctx,
		"SELECT id, name, body, variables, created_at, updated_at FROM message_templates WHERE id=$1 AND tenant_id=$2",
		id, tenantID,
	).Scan(&t.ID, &t.Name, &t.Body, &vars, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(vars), &t.Variables)
	t.CreatedAt, t.UpdatedAt = utc(t.CreatedAt), utc(t.UpdatedAt)
	return &t, nil
}

func (d *DB) ListMessageTemplates(ctx context.Context, tenantID string) ([]store.MessageTemplate, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT id, name, body, variables, created_at, updated_at FROM message_templates WHERE tenant_id=$1 ORDER BY name",
		tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var templates []store.MessageTemplate
	for rows.Next() {
		var t store.MessageTemplate
		var vars string
		if err := rows.Scan(&t.ID, &t.Name, &t.Body, &vars, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(vars), &t.Variables)
		t.CreatedAt, t.UpdatedAt = utc(t.CreatedAt), utc(t.UpdatedAt)
		templates = append(templates, t)
	}
	return templates, rows.Err()
}

func (d *DB) UpdateMessageTemplate(ctx context.Context, tenantID string, t *store.MessageTemplate) error {
	t.UpdatedAt = time.Now().UTC()
	vars, _ := json.Marshal(t.Variables)
	_, err := d.db.ExecContext(ctx,
		"UPDATE message_templates SET name=$1, body=$2, variables=$3, updated_at=$4 WHERE id=$5 AND tenant_id=$6",
		t.Name, t.Body, string(vars), t.UpdatedAt, t.ID, tenantID)
	return err
}

func (d *DB) DeleteMessageTemplate(ctx context.Context, tenantID string, id string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM message_templates WHERE id=$1 AND tenant_id=$2", id, tenantID)
	return err
}

// --- Alert rules (MESHSAT-313) ---

const alertRuleColumns = "id, name, condition_type, condition_params, chain_id, device_filter, enabled, last_evaluated, created_at, updated_at, tenant_id"

func scanAlertRule(s rowScanner) (*store.AlertRule, error) {
	var r store.AlertRule
	var lastEval sql.NullTime
	if err := s.Scan(&r.ID, &r.Name, &r.ConditionType, &r.ConditionParams, &r.ChainID, &r.DeviceFilter,
		&r.Enabled, &lastEval, &r.CreatedAt, &r.UpdatedAt, &r.TenantID); err != nil {
		return nil, err
	}
	if lastEval.Valid {
		r.LastEvaluated = lastEval.Time.UTC()
	}
	r.CreatedAt, r.UpdatedAt = utc(r.CreatedAt), utc(r.UpdatedAt)
	return &r, nil
}

func (d *DB) CreateAlertRule(ctx context.Context, tenantID string, r *store.AlertRule) error {
	if r.ID == "" {
		r.ID = fmt.Sprintf("arule-%d", time.Now().UnixNano())
	}
	now := time.Now().UTC()
	r.CreatedAt = now
	r.UpdatedAt = now
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO alert_rules (id, name, condition_type, condition_params, chain_id, device_filter, enabled, created_at, updated_at, tenant_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		r.ID, r.Name, r.ConditionType, r.ConditionParams, r.ChainID, r.DeviceFilter,
		r.Enabled, r.CreatedAt, r.UpdatedAt, tenantID)
	return err
}

func (d *DB) GetAlertRule(ctx context.Context, tenantID string, id string) (*store.AlertRule, error) {
	row := d.db.QueryRowContext(ctx,
		"SELECT "+alertRuleColumns+" FROM alert_rules WHERE id=$1 AND tenant_id=$2", id, tenantID)
	r, err := scanAlertRule(row)
	if err != nil {
		return nil, err
	}
	// MariaDB's GetAlertRule does not populate TenantID; keep the same shape.
	r.TenantID = ""
	return r, nil
}

// ListAlertRules returns the tenant's rules ordered by name. An empty
// tenantID returns rules across all tenants (used by the evaluator), with
// TenantID populated.
func (d *DB) ListAlertRules(ctx context.Context, tenantID string) ([]store.AlertRule, error) {
	query := "SELECT " + alertRuleColumns + " FROM alert_rules"
	var args []any
	if tenantID != "" {
		query += " WHERE tenant_id=$1"
		args = append(args, tenantID)
	}
	query += " ORDER BY name"
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var rules []store.AlertRule
	for rows.Next() {
		r, err := scanAlertRule(rows)
		if err != nil {
			return nil, err
		}
		rules = append(rules, *r)
	}
	return rules, rows.Err()
}

// UpdateAlertRule writes every column, including last_evaluated: a zero
// LastEvaluated is stored as NULL (MariaDB stores the zero DATETIME).
func (d *DB) UpdateAlertRule(ctx context.Context, tenantID string, r *store.AlertRule) error {
	r.UpdatedAt = time.Now().UTC()
	var lastEval any
	if !r.LastEvaluated.IsZero() {
		lastEval = r.LastEvaluated.UTC()
	}
	_, err := d.db.ExecContext(ctx,
		`UPDATE alert_rules SET name=$1, condition_type=$2, condition_params=$3, chain_id=$4, device_filter=$5,
		 enabled=$6, last_evaluated=$7, updated_at=$8 WHERE id=$9 AND tenant_id=$10`,
		r.Name, r.ConditionType, r.ConditionParams, r.ChainID, r.DeviceFilter,
		r.Enabled, lastEval, r.UpdatedAt, r.ID, tenantID)
	return err
}

func (d *DB) DeleteAlertRule(ctx context.Context, tenantID string, id string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM alert_rules WHERE id=$1 AND tenant_id=$2", id, tenantID)
	return err
}

// --- Credential management (MESHSAT-356) ---

const credentialColumns = `id, tenant_id, provider, name, cred_type, encrypted_data, cert_not_after,
	cert_subject, cert_issuer, cert_fingerprint, target_scope, target_bridge_id, status, version,
	distributed_at, created_at, updated_at`

func scanCredential(s rowScanner) (*store.Credential, error) {
	var c store.Credential
	var notAfter, distAt sql.NullTime
	if err := s.Scan(&c.ID, &c.TenantID, &c.Provider, &c.Name, &c.CredType, &c.EncryptedData,
		&notAfter, &c.CertSubject, &c.CertIssuer, &c.CertFingerprint,
		&c.TargetScope, &c.TargetBridgeID, &c.Status, &c.Version,
		&distAt, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.CertNotAfter = utcPtr(notAfter)
	c.DistributedAt = utcPtr(distAt)
	c.CreatedAt, c.UpdatedAt = utc(c.CreatedAt), utc(c.UpdatedAt)
	return &c, nil
}

func (d *DB) queryCredentials(ctx context.Context, where string, args ...any) ([]store.Credential, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT "+credentialColumns+" FROM credentials "+where, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var creds []store.Credential
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		creds = append(creds, *c)
	}
	return creds, rows.Err()
}

func (d *DB) CreateCredential(ctx context.Context, tenantID string, c *store.Credential) error {
	now := time.Now().UTC()
	c.CreatedAt = now
	c.UpdatedAt = now
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO credentials (id, tenant_id, provider, name, cred_type, encrypted_data,
		 cert_not_after, cert_subject, cert_issuer, cert_fingerprint, target_scope, target_bridge_id,
		 status, version, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		c.ID, tenantID, c.Provider, c.Name, c.CredType, c.EncryptedData,
		nullTimePtr(c.CertNotAfter), c.CertSubject, c.CertIssuer, c.CertFingerprint,
		c.TargetScope, c.TargetBridgeID, c.Status, c.Version, now, now)
	return err
}

func (d *DB) GetCredential(ctx context.Context, tenantID string, id string) (*store.Credential, error) {
	row := d.db.QueryRowContext(ctx,
		"SELECT "+credentialColumns+" FROM credentials WHERE id=$1 AND tenant_id=$2", id, tenantID)
	return scanCredential(row)
}

func (d *DB) ListCredentials(ctx context.Context, tenantID string) ([]store.Credential, error) {
	return d.queryCredentials(ctx, "WHERE tenant_id=$1 ORDER BY provider, name", tenantID)
}

func (d *DB) UpdateCredential(ctx context.Context, tenantID string, c *store.Credential) error {
	c.UpdatedAt = time.Now().UTC()
	_, err := d.db.ExecContext(ctx,
		`UPDATE credentials SET provider=$1, name=$2, cred_type=$3, encrypted_data=$4,
		 cert_not_after=$5, cert_subject=$6, cert_issuer=$7, cert_fingerprint=$8,
		 target_scope=$9, target_bridge_id=$10, status=$11, version=$12, updated_at=$13
		 WHERE id=$14 AND tenant_id=$15`,
		c.Provider, c.Name, c.CredType, c.EncryptedData,
		nullTimePtr(c.CertNotAfter), c.CertSubject, c.CertIssuer, c.CertFingerprint,
		c.TargetScope, c.TargetBridgeID, c.Status, c.Version,
		c.UpdatedAt, c.ID, tenantID)
	return err
}

func (d *DB) DeleteCredential(ctx context.Context, tenantID string, id string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM credentials WHERE id=$1 AND tenant_id=$2", id, tenantID)
	return err
}

// ListExpiringCredentials returns, across all tenants, the active/expiring
// credentials whose certificate expires at or before "before".
func (d *DB) ListExpiringCredentials(ctx context.Context, before time.Time) ([]store.Credential, error) {
	return d.queryCredentials(ctx,
		`WHERE cert_not_after IS NOT NULL AND cert_not_after <= $1 AND status IN ('active', 'expiring')
		 ORDER BY cert_not_after ASC`, before.UTC())
}
