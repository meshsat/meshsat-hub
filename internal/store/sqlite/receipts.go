package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/rs/xid"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// --- Receipts (MESHSAT-998) ---
//
// The outbox that turns a payment into a document. See store.Receipt for why
// it exists and why the delivery key is unique.

const receiptCols = `id, tenant_id, delivery_key, transaction_id, email, name, amount_cents, currency,
	plan, tier_name, paid_at, status, attempts, last_error, next_attempt_at,
	invoice_number, invoice_ref, issued_at, created_at, updated_at`

func scanReceipt(sc interface{ Scan(...any) error }) (store.Receipt, error) {
	var r store.Receipt
	var paid, next, issued, created, updated string
	if err := sc.Scan(&r.ID, &r.TenantID, &r.DeliveryKey, &r.TransactionID, &r.Email, &r.Name,
		&r.AmountCents, &r.Currency, &r.Plan, &r.TierName, &paid, &r.Status, &r.Attempts,
		&r.LastError, &next, &r.InvoiceNumber, &r.InvoiceRef, &issued, &created, &updated); err != nil {
		return r, err
	}
	r.PaidAt, r.NextAttemptAt = parseTime(paid), parseTime(next)
	r.CreatedAt, r.UpdatedAt = parseTime(created), parseTime(updated)
	if issued != "" {
		t := parseTime(issued)
		r.IssuedAt = &t
	}
	return r, nil
}

// CreateReceipt inserts the row unless its delivery key is already present.
//
// The insert IS the idempotency check -- a read-then-write would let two
// replicas handling the same retried delivery both decide the row was missing
// and issue two documents for one payment.
func (d *DB) CreateReceipt(ctx context.Context, r *store.Receipt) (bool, error) {
	if r.DeliveryKey == "" {
		return false, errors.New("receipt: delivery key is required")
	}
	if r.ID == "" {
		r.ID = xid.New().String()
	}
	if r.Status == "" {
		r.Status = store.ReceiptPending
	}
	now := time.Now().UTC()
	r.CreatedAt, r.UpdatedAt = now, now
	if r.NextAttemptAt.IsZero() {
		r.NextAttemptAt = now
	}
	res, err := d.db.ExecContext(ctx, `INSERT OR IGNORE INTO receipts (`+receiptCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.TenantID, r.DeliveryKey, r.TransactionID, r.Email, r.Name, r.AmountCents, r.Currency,
		r.Plan, r.TierName, fmtTime(r.PaidAt.UTC()), r.Status, r.Attempts, r.LastError,
		fmtTime(r.NextAttemptAt.UTC()), r.InvoiceNumber, r.InvoiceRef, "", fmtTime(now), fmtTime(now))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (d *DB) GetReceiptByKey(ctx context.Context, deliveryKey string) (*store.Receipt, error) {
	r, err := scanReceipt(d.db.QueryRowContext(ctx,
		"SELECT "+receiptCols+" FROM receipts WHERE delivery_key=?", deliveryKey))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (d *DB) ListDueReceipts(ctx context.Context, now time.Time, limit int) ([]store.Receipt, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.db.QueryContext(ctx, "SELECT "+receiptCols+` FROM receipts
		WHERE status=? AND next_attempt_at<=? ORDER BY created_at, id LIMIT ?`,
		store.ReceiptPending, fmtTime(now.UTC()), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []store.Receipt
	for rows.Next() {
		r, err := scanReceipt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) SetReceiptInvoice(ctx context.Context, id, invoiceRef string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE receipts SET invoice_ref=?, updated_at=? WHERE id=?`,
		invoiceRef, fmtTime(time.Now().UTC()), id)
	return err
}

func (d *DB) MarkReceiptIssued(ctx context.Context, id, invoiceNumber, invoiceRef string, at time.Time) error {
	now := time.Now().UTC()
	_, err := d.db.ExecContext(ctx, `UPDATE receipts
		SET status=?, invoice_number=?, invoice_ref=?, issued_at=?, last_error='', updated_at=?
		WHERE id=?`,
		store.ReceiptIssued, invoiceNumber, invoiceRef, fmtTime(at.UTC()), fmtTime(now), id)
	return err
}

func (d *DB) MarkReceiptAttempt(ctx context.Context, id, errMsg string, nextAttempt time.Time) error {
	now := time.Now().UTC()
	_, err := d.db.ExecContext(ctx, `UPDATE receipts
		SET attempts=attempts+1, last_error=?, next_attempt_at=?, updated_at=? WHERE id=?`,
		errMsg, fmtTime(nextAttempt.UTC()), fmtTime(now), id)
	return err
}

func (d *DB) BlockReceipt(ctx context.Context, id, reason string) error {
	now := time.Now().UTC()
	_, err := d.db.ExecContext(ctx,
		`UPDATE receipts SET status=?, last_error=?, updated_at=? WHERE id=?`,
		store.ReceiptBlocked, reason, fmtTime(now), id)
	return err
}

// ListReceiptsByStatus returns receipts in one state, newest first. Blocked
// receipts had no listing anywhere: a payment parked for a person to look at
// was invisible to that person.
func (d *DB) ListReceiptsByStatus(ctx context.Context, status string, limit int) ([]store.Receipt, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := d.db.QueryContext(ctx, "SELECT "+receiptCols+` FROM receipts
		WHERE status=? ORDER BY created_at DESC, id DESC LIMIT ?`, status, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []store.Receipt
	for rows.Next() {
		r, err := scanReceipt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RequeueReceipt reopens a blocked receipt. Only a blocked one: an issued
// receipt must never go back to pending, or the drainer would create a second
// invoice for a payment that already has a document and a number.
func (d *DB) RequeueReceipt(ctx context.Context, id string, at time.Time) error {
	res, err := d.db.ExecContext(ctx,
		`UPDATE receipts SET status=?, next_attempt_at=?, last_error='', updated_at=?
		 WHERE id=? AND status=?`,
		store.ReceiptPending, fmtTime(at.UTC()), fmtTime(time.Now().UTC()), id, store.ReceiptBlocked)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return store.ErrNotFound
	}
	return nil
}
