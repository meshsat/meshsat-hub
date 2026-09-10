package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/rs/xid"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// --- Refunds (MESHSAT-1019) ---
//
// The mirror of the receipts outbox: money given back owes the customer a
// credit note. See store.Refund for why the row exists and why receipt_id is
// unique.

const refundCols = `id, tenant_id, receipt_id, amount_cents, currency, country, reason, requested_by,
	refunded_at, status, attempts, last_error, next_attempt_at, credit_number, credit_ref,
	issued_at, created_at, updated_at`

func scanRefund(sc interface{ Scan(...any) error }) (store.Refund, error) {
	var r store.Refund
	var refunded, next, issued, created, updated string
	if err := sc.Scan(&r.ID, &r.TenantID, &r.ReceiptID, &r.AmountCents, &r.Currency, &r.Country,
		&r.Reason, &r.RequestedBy, &refunded, &r.Status, &r.Attempts, &r.LastError, &next,
		&r.CreditNumber, &r.CreditRef, &issued, &created, &updated); err != nil {
		return r, err
	}
	r.RefundedAt, r.NextAttemptAt = parseTime(refunded), parseTime(next)
	r.CreatedAt, r.UpdatedAt = parseTime(created), parseTime(updated)
	if issued != "" {
		t := parseTime(issued)
		r.IssuedAt = &t
	}
	return r, nil
}

// CreateRefund inserts the row unless the receipt already has one. Like the
// receipts outbox, the insert IS the check: a read-then-write lets two
// operators recording the same refund both find it missing.
func (d *DB) CreateRefund(ctx context.Context, r *store.Refund) (bool, error) {
	if r.ReceiptID == "" {
		return false, errors.New("refund: receipt id is required")
	}
	if r.ID == "" {
		r.ID = xid.New().String()
	}
	if r.Status == "" {
		r.Status = store.RefundPending
	}
	now := time.Now().UTC()
	r.CreatedAt, r.UpdatedAt = now, now
	if r.NextAttemptAt.IsZero() {
		r.NextAttemptAt = now
	}
	if r.RefundedAt.IsZero() {
		r.RefundedAt = now
	}
	res, err := d.db.ExecContext(ctx, `INSERT OR IGNORE INTO refunds (`+refundCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.TenantID, r.ReceiptID, r.AmountCents, r.Currency, r.Country, r.Reason, r.RequestedBy,
		fmtTime(r.RefundedAt.UTC()), r.Status, r.Attempts, r.LastError, fmtTime(r.NextAttemptAt.UTC()),
		r.CreditNumber, r.CreditRef, "", fmtTime(now), fmtTime(now))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (d *DB) GetRefund(ctx context.Context, id string) (*store.Refund, error) {
	r, err := scanRefund(d.db.QueryRowContext(ctx,
		"SELECT "+refundCols+" FROM refunds WHERE id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (d *DB) GetRefundByReceipt(ctx context.Context, receiptID string) (*store.Refund, error) {
	r, err := scanRefund(d.db.QueryRowContext(ctx,
		"SELECT "+refundCols+" FROM refunds WHERE receipt_id=?", receiptID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (d *DB) ListDueRefunds(ctx context.Context, now time.Time, limit int) ([]store.Refund, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.db.QueryContext(ctx, "SELECT "+refundCols+` FROM refunds
		WHERE status=? AND next_attempt_at<=? AND leased_until<? ORDER BY created_at, id LIMIT ?`,
		store.RefundPending, fmtTime(now.UTC()), fmtTime(now.UTC()), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []store.Refund
	for rows.Next() {
		r, err := scanRefund(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) ListRefundsByStatus(ctx context.Context, status string, limit int) ([]store.Refund, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := d.db.QueryContext(ctx, "SELECT "+refundCols+` FROM refunds
		WHERE status=? ORDER BY created_at DESC, id DESC LIMIT ?`, status, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []store.Refund
	for rows.Next() {
		r, err := scanRefund(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) SetRefundCredit(ctx context.Context, id, creditRef string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE refunds SET credit_ref=?, updated_at=? WHERE id=?`,
		creditRef, fmtTime(time.Now().UTC()), id)
	return err
}

func (d *DB) MarkRefundIssued(ctx context.Context, id, creditNumber, creditRef string, at time.Time) error {
	now := time.Now().UTC()
	_, err := d.db.ExecContext(ctx, `UPDATE refunds
		SET status=?, credit_number=?, credit_ref=?, issued_at=?, last_error='', updated_at=?
		WHERE id=?`,
		store.RefundIssued, creditNumber, creditRef, fmtTime(at.UTC()), fmtTime(now), id)
	return err
}

func (d *DB) MarkRefundAttempt(ctx context.Context, id, errMsg string, nextAttempt time.Time) error {
	now := time.Now().UTC()
	_, err := d.db.ExecContext(ctx, `UPDATE refunds
		SET attempts=attempts+1, last_error=?, next_attempt_at=?, updated_at=? WHERE id=?`,
		errMsg, fmtTime(nextAttempt.UTC()), fmtTime(now), id)
	return err
}

func (d *DB) BlockRefund(ctx context.Context, id, reason string) error {
	now := time.Now().UTC()
	_, err := d.db.ExecContext(ctx,
		`UPDATE refunds SET status=?, last_error=?, updated_at=? WHERE id=?`,
		store.RefundBlocked, reason, fmtTime(now), id)
	return err
}

// RequeueRefund reopens a blocked refund. Only a blocked one: an issued refund
// must never go back to pending, or the drainer would draw a second credit
// note number for money that already has a document.
func (d *DB) RequeueRefund(ctx context.Context, id string, at time.Time) error {
	res, err := d.db.ExecContext(ctx,
		`UPDATE refunds SET status=?, next_attempt_at=?, last_error='', updated_at=?
		 WHERE id=? AND status=?`,
		store.RefundPending, fmtTime(at.UTC()), fmtTime(time.Now().UTC()), id, store.RefundBlocked)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// DeleteRefund removes a refund that produced no document. The credit_ref
// guard is the point: once the billing system has a credit note, deleting this
// row would hide it rather than unmake it.
func (d *DB) DeleteRefund(ctx context.Context, id string) error {
	res, err := d.db.ExecContext(ctx,
		`DELETE FROM refunds WHERE id=? AND status<>? AND credit_ref=''`, id, store.RefundIssued)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (d *DB) ClaimRefund(ctx context.Context, id string, until time.Time) (bool, error) {
	now := fmtTime(time.Now().UTC())
	res, err := d.db.ExecContext(ctx,
		`UPDATE refunds SET leased_until = ?, updated_at = ? WHERE id = ? AND leased_until < ?`,
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

func (d *DB) ReleaseRefund(ctx context.Context, id string) error {
	_, err := d.db.ExecContext(ctx, `UPDATE refunds SET leased_until = '' WHERE id = ?`, id)
	return err
}

// GetReceipt reads one receipt by id.
func (d *DB) GetReceipt(ctx context.Context, id string) (*store.Receipt, error) {
	r, err := scanReceipt(d.db.QueryRowContext(ctx,
		"SELECT "+receiptCols+" FROM receipts WHERE id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// RefundsByCountrySince totals issued refunds by country. The threshold meter
// subtracts these: a sale that was given back is not a supply to another member
// state and counting it would report a distance to the threshold the business
// never travelled.
func (d *DB) RefundsByCountrySince(ctx context.Context, since time.Time) (map[string]int64, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT country, COALESCE(SUM(amount_cents), 0) FROM refunds
		 WHERE status = ? AND country <> '' AND issued_at >= ?
		 GROUP BY country`, store.RefundIssued, fmtTime(since.UTC()))
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
