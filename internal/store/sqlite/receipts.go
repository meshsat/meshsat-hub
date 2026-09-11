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

const receiptCols = `id, tenant_id, delivery_key, transaction_id, country, email, name, amount_cents, currency,
	plan, tier_name, paid_at, status, attempts, last_error, next_attempt_at,
	invoice_number, invoice_ref, payment_ref, issued_at, created_at, updated_at`

func scanReceipt(sc interface{ Scan(...any) error }) (store.Receipt, error) {
	var r store.Receipt
	var paid, next, issued, created, updated string
	if err := sc.Scan(&r.ID, &r.TenantID, &r.DeliveryKey, &r.TransactionID, &r.Country, &r.Email, &r.Name,
		&r.AmountCents, &r.Currency, &r.Plan, &r.TierName, &paid, &r.Status, &r.Attempts,
		&r.LastError, &next, &r.InvoiceNumber, &r.InvoiceRef, &r.PaymentRef, &issued, &created, &updated); err != nil {
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
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.TenantID, r.DeliveryKey, r.TransactionID, r.Country, r.Email, r.Name, r.AmountCents, r.Currency,
		r.Plan, r.TierName, fmtTime(r.PaidAt.UTC()), r.Status, r.Attempts, r.LastError,
		fmtTime(r.NextAttemptAt.UTC()), r.InvoiceNumber, r.InvoiceRef, r.PaymentRef, "", fmtTime(now), fmtTime(now))
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
		WHERE status=? AND next_attempt_at<=? AND leased_until<? ORDER BY created_at, id LIMIT ?`,
		store.ReceiptPending, fmtTime(now.UTC()), fmtTime(now.UTC()), limit)
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
		`UPDATE receipts SET payment_ref=?, updated_at=? WHERE delivery_key=?`,
		paymentRef, fmtTime(time.Now().UTC()), deliveryKey)
	return err
}

// GetReceiptByPaymentRef finds the receipt a refunded payment belongs to.
func (d *DB) GetReceiptByPaymentRef(ctx context.Context, paymentRef string) (*store.Receipt, error) {
	if paymentRef == "" {
		return nil, store.ErrNotFound
	}
	r, err := scanReceipt(d.db.QueryRowContext(ctx,
		"SELECT "+receiptCols+" FROM receipts WHERE payment_ref=?", paymentRef))
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

// BlockReceipt parks a receipt for a person. An ISSUED receipt is never parked:
// the document exists and has a number out of a gapless series, and flipping it
// to blocked would leave a customer holding an invoice nothing will ever
// reconcile. The guard matters because two drainers touch this row -- the
// receipt issuer and the refund issuer (MESHSAT-1019).
func (d *DB) BlockReceipt(ctx context.Context, id, reason string) error {
	now := time.Now().UTC()
	_, err := d.db.ExecContext(ctx,
		`UPDATE receipts SET status=?, last_error=?, updated_at=? WHERE id=? AND status<>?`,
		store.ReceiptBlocked, reason, fmtTime(now), id, store.ReceiptIssued)
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

// ClaimReceipt leases a receipt row to one drainer. See the Postgres twin.
func (d *DB) ClaimReceipt(ctx context.Context, id string, until time.Time) (bool, error) {
	now := fmtTime(time.Now().UTC())
	res, err := d.db.ExecContext(ctx,
		`UPDATE receipts SET leased_until = ?, updated_at = ? WHERE id = ? AND leased_until < ?`,
		fmtTime(until.UTC()), now, id, now)
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
	_, err := d.db.ExecContext(ctx, `UPDATE receipts SET leased_until = '' WHERE id = ?`, id)
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
		 WHERE status = ? AND country <> '' AND issued_at >= ? AND plan <> ?
		 GROUP BY country`, store.ReceiptIssued, fmtTime(since.UTC()), store.DonationPlan)
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
