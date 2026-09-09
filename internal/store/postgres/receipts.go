package postgres

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
	var issued sql.NullTime
	if err := sc.Scan(&r.ID, &r.TenantID, &r.DeliveryKey, &r.TransactionID, &r.Email, &r.Name,
		&r.AmountCents, &r.Currency, &r.Plan, &r.TierName, &r.PaidAt, &r.Status, &r.Attempts,
		&r.LastError, &r.NextAttemptAt, &r.InvoiceNumber, &r.InvoiceRef, &issued,
		&r.CreatedAt, &r.UpdatedAt); err != nil {
		return r, err
	}
	r.PaidAt, r.NextAttemptAt = utc(r.PaidAt), utc(r.NextAttemptAt)
	r.CreatedAt, r.UpdatedAt = utc(r.CreatedAt), utc(r.UpdatedAt)
	r.IssuedAt = utcPtr(issued)
	return r, nil
}

// CreateReceipt inserts the row unless its delivery key is already present.
//
// ON CONFLICT DO NOTHING rather than a read-then-write: the constraint is what
// makes a replayed delivery produce no second document, and two replicas
// handling the same retry must not both find the row missing.
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
	if r.PaidAt.IsZero() {
		r.PaidAt = now
	}
	res, err := d.db.ExecContext(ctx, `INSERT INTO receipts (`+receiptCols+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
		ON CONFLICT (delivery_key) DO NOTHING`,
		r.ID, r.TenantID, r.DeliveryKey, r.TransactionID, r.Email, r.Name, r.AmountCents, r.Currency,
		r.Plan, r.TierName, r.PaidAt.UTC(), r.Status, r.Attempts, r.LastError,
		r.NextAttemptAt.UTC(), r.InvoiceNumber, r.InvoiceRef, nil, now, now)
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
		"SELECT "+receiptCols+" FROM receipts WHERE delivery_key=$1", deliveryKey))
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
		WHERE status=$1 AND next_attempt_at<=$2 ORDER BY created_at, id LIMIT $3`,
		store.ReceiptPending, now.UTC(), limit)
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
		`UPDATE receipts SET invoice_ref=$1, updated_at=$2 WHERE id=$3`,
		invoiceRef, time.Now().UTC(), id)
	return err
}

func (d *DB) MarkReceiptIssued(ctx context.Context, id, invoiceNumber, invoiceRef string, at time.Time) error {
	_, err := d.db.ExecContext(ctx, `UPDATE receipts
		SET status=$1, invoice_number=$2, invoice_ref=$3, issued_at=$4, last_error='', updated_at=$5
		WHERE id=$6`,
		store.ReceiptIssued, invoiceNumber, invoiceRef, at.UTC(), time.Now().UTC(), id)
	return err
}

func (d *DB) MarkReceiptAttempt(ctx context.Context, id, errMsg string, nextAttempt time.Time) error {
	_, err := d.db.ExecContext(ctx, `UPDATE receipts
		SET attempts=attempts+1, last_error=$1, next_attempt_at=$2, updated_at=$3 WHERE id=$4`,
		errMsg, nextAttempt.UTC(), time.Now().UTC(), id)
	return err
}

func (d *DB) BlockReceipt(ctx context.Context, id, reason string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE receipts SET status=$1, last_error=$2, updated_at=$3 WHERE id=$4`,
		store.ReceiptBlocked, reason, time.Now().UTC(), id)
	return err
}
