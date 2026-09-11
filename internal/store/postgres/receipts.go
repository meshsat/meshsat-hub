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

const receiptCols = `id, tenant_id, delivery_key, transaction_id, country, email, name, amount_cents, currency,
	plan, tier_name, paid_at, status, attempts, last_error, next_attempt_at,
	invoice_number, invoice_ref, payment_ref, issued_at, created_at, updated_at`

func scanReceipt(sc interface{ Scan(...any) error }) (store.Receipt, error) {
	var r store.Receipt
	var issued sql.NullTime
	if err := sc.Scan(&r.ID, &r.TenantID, &r.DeliveryKey, &r.TransactionID, &r.Country, &r.Email, &r.Name,
		&r.AmountCents, &r.Currency, &r.Plan, &r.TierName, &r.PaidAt, &r.Status, &r.Attempts,
		&r.LastError, &r.NextAttemptAt, &r.InvoiceNumber, &r.InvoiceRef, &r.PaymentRef, &issued,
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
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)
		ON CONFLICT (delivery_key) DO NOTHING`,
		r.ID, r.TenantID, r.DeliveryKey, r.TransactionID, r.Country, r.Email, r.Name, r.AmountCents, r.Currency,
		r.Plan, r.TierName, r.PaidAt.UTC(), r.Status, r.Attempts, r.LastError,
		r.NextAttemptAt.UTC(), r.InvoiceNumber, r.InvoiceRef, r.PaymentRef, nil, now, now)
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
		WHERE status=$1 AND next_attempt_at<=$2 AND leased_until<$2 ORDER BY created_at, id LIMIT $3`,
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

// SetReceiptPaymentRef records which provider payment settled this receipt, so
// a refund can find the document to reverse. Keyed on the delivery key because
// the caller is a webhook that knows the invoice, not our row id.
func (d *DB) SetReceiptPaymentRef(ctx context.Context, deliveryKey, paymentRef string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE receipts SET payment_ref=$1, updated_at=$2 WHERE delivery_key=$3`,
		paymentRef, time.Now().UTC(), deliveryKey)
	return err
}

// GetReceiptByPaymentRef finds the receipt a refunded payment belongs to.
func (d *DB) GetReceiptByPaymentRef(ctx context.Context, paymentRef string) (*store.Receipt, error) {
	if paymentRef == "" {
		return nil, store.ErrNotFound
	}
	r, err := scanReceipt(d.db.QueryRowContext(ctx,
		"SELECT "+receiptCols+" FROM receipts WHERE payment_ref=$1", paymentRef))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
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

// BlockReceipt parks a receipt for a person. An ISSUED receipt is never parked:
// the document exists and has a number out of a gapless series, and flipping it
// to blocked would leave a customer holding an invoice nothing will ever
// reconcile. The guard matters because two drainers touch this row -- the
// receipt issuer and the refund issuer (MESHSAT-1019).
func (d *DB) BlockReceipt(ctx context.Context, id, reason string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE receipts SET status=$1, last_error=$2, updated_at=$3 WHERE id=$4 AND status<>$5`,
		store.ReceiptBlocked, reason, time.Now().UTC(), id, store.ReceiptIssued)
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
		WHERE status=$1 ORDER BY created_at DESC, id DESC LIMIT $2`, status, limit)
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
		`UPDATE receipts SET status=$1, next_attempt_at=$2, last_error='', updated_at=$3
		 WHERE id=$4 AND status=$5`,
		store.ReceiptPending, at.UTC(), time.Now().UTC(), id, store.ReceiptBlocked)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ClaimReceipt leases a receipt row to one drainer. The conditional UPDATE is
// the whole mechanism: the database decides the winner, so two drainers cannot
// both believe they may draw an invoice number for the same payment.
func (d *DB) ClaimReceipt(ctx context.Context, id string, until time.Time) (bool, error) {
	now := time.Now().UTC()
	res, err := d.db.ExecContext(ctx,
		`UPDATE receipts SET leased_until = $1, updated_at = $2 WHERE id = $3 AND leased_until < $2`,
		until.UTC(), now, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// ReleaseReceipt drops the lease so a receipt due again shortly is not held for
// the rest of it.
func (d *DB) ReleaseReceipt(ctx context.Context, id string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE receipts SET leased_until = $1 WHERE id = $2`, time.Unix(0, 0).UTC(), id)
	return err
}

// CrossBorderSalesSince totals issued receipts by country since an instant. See
// store.Store: this is the measurement the flat Dutch rate depends on.
func (d *DB) CrossBorderSalesSince(ctx context.Context, since time.Time) (map[string]int64, error) {
	// Donations are excluded. The Article 59c threshold measures cross-border
	// B2C SUPPLIES, and a voluntary contribution with no counter-performance is
	// not a supply at all (internal/vat.ForDonation) -- it carries no VAT and
	// belongs in no member state's column. Counting it would report distance
	// travelled toward a limit that the money cannot move.
	rows, err := d.db.QueryContext(ctx,
		`SELECT country, COALESCE(SUM(amount_cents), 0) FROM receipts
		 WHERE status = $1 AND country <> '' AND issued_at >= $2 AND plan <> $3
		 GROUP BY country`, store.ReceiptIssued, since.UTC(), store.DonationPlan)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var c string
		var cents int64
		if err := rows.Scan(&c, &cents); err != nil {
			return nil, err
		}
		out[c] = cents
	}
	return out, rows.Err()
}
