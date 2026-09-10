package postgres

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
	var issued sql.NullTime
	if err := sc.Scan(&r.ID, &r.TenantID, &r.ReceiptID, &r.AmountCents, &r.Currency, &r.Country,
		&r.Reason, &r.RequestedBy, &r.RefundedAt, &r.Status, &r.Attempts, &r.LastError,
		&r.NextAttemptAt, &r.CreditNumber, &r.CreditRef, &issued,
		&r.CreatedAt, &r.UpdatedAt); err != nil {
		return r, err
	}
	r.RefundedAt, r.NextAttemptAt = utc(r.RefundedAt), utc(r.NextAttemptAt)
	r.CreatedAt, r.UpdatedAt = utc(r.CreatedAt), utc(r.UpdatedAt)
	r.IssuedAt = utcPtr(issued)
	return r, nil
}

// CreateRefund inserts the row unless the receipt already has one. ON CONFLICT
// DO NOTHING rather than a read-then-write: the constraint is what stops two
// operators recording the same refund twice and crediting it twice.
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
	res, err := d.db.ExecContext(ctx, `INSERT INTO refunds (`+refundCols+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,NULL,$16,$17)
		ON CONFLICT (receipt_id) DO NOTHING`,
		r.ID, r.TenantID, r.ReceiptID, r.AmountCents, r.Currency, r.Country, r.Reason, r.RequestedBy,
		r.RefundedAt.UTC(), r.Status, r.Attempts, r.LastError, r.NextAttemptAt.UTC(),
		r.CreditNumber, r.CreditRef, now, now)
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
		"SELECT "+refundCols+" FROM refunds WHERE id=$1", id))
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
		"SELECT "+refundCols+" FROM refunds WHERE receipt_id=$1", receiptID))
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
		WHERE status=$1 AND next_attempt_at<=$2 AND leased_until<$2 ORDER BY created_at, id LIMIT $3`,
		store.RefundPending, now.UTC(), limit)
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
		WHERE status=$1 ORDER BY created_at DESC, id DESC LIMIT $2`, status, limit)
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
		`UPDATE refunds SET credit_ref=$1, updated_at=$2 WHERE id=$3`,
		creditRef, time.Now().UTC(), id)
	return err
}

func (d *DB) MarkRefundIssued(ctx context.Context, id, creditNumber, creditRef string, at time.Time) error {
	_, err := d.db.ExecContext(ctx, `UPDATE refunds
		SET status=$1, credit_number=$2, credit_ref=$3, issued_at=$4, last_error='', updated_at=$5
		WHERE id=$6`,
		store.RefundIssued, creditNumber, creditRef, at.UTC(), time.Now().UTC(), id)
	return err
}

func (d *DB) MarkRefundAttempt(ctx context.Context, id, errMsg string, nextAttempt time.Time) error {
	_, err := d.db.ExecContext(ctx, `UPDATE refunds
		SET attempts=attempts+1, last_error=$1, next_attempt_at=$2, updated_at=$3 WHERE id=$4`,
		errMsg, nextAttempt.UTC(), time.Now().UTC(), id)
	return err
}

func (d *DB) BlockRefund(ctx context.Context, id, reason string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE refunds SET status=$1, last_error=$2, updated_at=$3 WHERE id=$4`,
		store.RefundBlocked, reason, time.Now().UTC(), id)
	return err
}

// RequeueRefund reopens a blocked refund. Only a blocked one: an issued refund
// must never go back to pending, or the drainer would draw a second credit
// note number for money that already has a document.
func (d *DB) RequeueRefund(ctx context.Context, id string, at time.Time) error {
	res, err := d.db.ExecContext(ctx,
		`UPDATE refunds SET status=$1, next_attempt_at=$2, last_error='', updated_at=$3
		 WHERE id=$4 AND status=$5`,
		store.RefundPending, at.UTC(), time.Now().UTC(), id, store.RefundBlocked)
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
		`DELETE FROM refunds WHERE id=$1 AND status<>$2 AND credit_ref=''`, id, store.RefundIssued)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ClaimRefund leases a refund row to one drainer. The conditional UPDATE is the
// whole mechanism, exactly as for receipts: the database decides the winner, so
// two drainers cannot both draw a credit note number for one refund.
func (d *DB) ClaimRefund(ctx context.Context, id string, until time.Time) (bool, error) {
	now := time.Now().UTC()
	res, err := d.db.ExecContext(ctx,
		`UPDATE refunds SET leased_until = $1, updated_at = $2 WHERE id = $3 AND leased_until < $2`,
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

func (d *DB) ReleaseRefund(ctx context.Context, id string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE refunds SET leased_until = $1 WHERE id = $2`, time.Unix(0, 0).UTC(), id)
	return err
}

// GetReceipt reads one receipt by id.
func (d *DB) GetReceipt(ctx context.Context, id string) (*store.Receipt, error) {
	r, err := scanReceipt(d.db.QueryRowContext(ctx,
		"SELECT "+receiptCols+" FROM receipts WHERE id=$1", id))
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
		 WHERE status = $1 AND country <> '' AND issued_at >= $2
		 GROUP BY country`, store.RefundIssued, since.UTC())
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
