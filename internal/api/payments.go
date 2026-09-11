package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/audit"
	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/vat"
)

// PaymentsHandler surfaces payments that arrived and upgraded nobody.
//
// Matching a payment to a tenant is deliberately not a guess: a claim code, the
// payer's address against the tenant owner's, or a remembered payer, and
// otherwise nothing happens. That is the right call -- guessing is how one
// tenant ends up paying for another's fleet -- but it left the money with no
// trace but a log line, which nothing alerts on and nobody reads at the moment
// it matters. This is where an operator finds it (MESHSAT-1007).
type PaymentsHandler struct {
	audit *audit.Service
	store store.Store
	// taxRate is the inclusive rate stored figures carry, so the threshold can
	// report supplies rather than the money that changed hands.
	taxRate float64
}

// NewPaymentsHandler returns a handler over the audit log's payment records
// and the receipts outbox.
func NewPaymentsHandler(a *audit.Service, s store.Store) *PaymentsHandler {
	return &PaymentsHandler{audit: a, store: s}
}

// SetTaxRate tells the threshold meter what rate to strip out of the stored
// gross. Without it the meter reports the money that changed hands rather than
// the supplies made, which is 21% too high.
func (h *PaymentsHandler) SetTaxRate(pct float64) { h.taxRate = pct }

// The audit actions an unattributable payment is recorded under. There are two
// because the audit log is an append-only SHA-256 hash chain: the entries the
// predecessor wrote cannot be rewritten, so the OLD name has to stay readable
// while the current provider writes the new one.
//
// This surface was dead from the Stripe migration until 2026-09-11. It filtered
// on the legacy name alone while internal/stripe wrote payment_unattributed, so
// GET /api/admin/payments/unmatched answered [] however much money had failed
// to be placed -- the exact opposite of what MESHSAT-1007 built it for. The
// metric was the only remaining signal. Keep both names here; a rename in
// internal/stripe must be added to this list, never substituted into it.
const (
	unattributedPaymentAction       = "payment_unattributed"
	legacyUnattributedPaymentAction = "kofi_payment_unmatched"
)

type unmatchedPaymentResponse struct {
	RecordedAt string          `json:"recorded_at"`
	Payment    json.RawMessage `json:"payment"`
}

// ListUnmatched returns payments that matched no tenant.
// @Summary      Payments that matched no tenant
// @Tags         admin
// @Produce      json
// @Param        limit  query  int  false  "Max results"  default(50)
// @Success      200  {array}  unmatchedPaymentResponse
// @Router       /api/admin/payments/unmatched [get]
func (h *PaymentsHandler) ListUnmatched(w http.ResponseWriter, r *http.Request) {
	if h.audit == nil {
		writeJSON(w, http.StatusOK, []unmatchedPaymentResponse{})
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	// Unmatched payments are recorded against the platform tenant, and they
	// are rare, so a bounded scan with the filter in Go beats a new index.
	// The scan window is wider than the page so a burst of other events on
	// the platform tenant cannot push every payment out of view.
	entries, err := h.audit.Store().ListAuditEntries(r.Context(), store.DefaultTenantID, limit*20)
	if err != nil {
		slog.Error("payments: list unmatched", "error", err)
		writeError(w, http.StatusInternalServerError, "could not read payments")
		return
	}
	out := []unmatchedPaymentResponse{}
	for _, e := range entries {
		if e.Action != unattributedPaymentAction && e.Action != legacyUnattributedPaymentAction {
			continue
		}
		out = append(out, unmatchedPaymentResponse{
			RecordedAt: e.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			Payment:    json.RawMessage(e.Detail),
		})
		if len(out) >= limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ListBlockedReceipts returns receipts parked for a person to look at.
// @Summary      Receipts a person has to look at
// @Tags         admin
// @Produce      json
// @Param        limit  query  int  false  "Max results"  default(100)
// @Success      200  {array}  store.Receipt
// @Router       /api/admin/receipts/blocked [get]
func (h *PaymentsHandler) ListBlockedReceipts(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, http.StatusOK, []store.Receipt{})
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	out, err := h.store.ListReceiptsByStatus(r.Context(), store.ReceiptBlocked, limit)
	if err != nil {
		slog.Error("payments: list blocked receipts", "error", err)
		writeError(w, http.StatusInternalServerError, "could not read receipts")
		return
	}
	if out == nil {
		out = []store.Receipt{}
	}
	writeJSON(w, http.StatusOK, out)
}

// RequeueReceipt puts a blocked receipt back in the drainer's queue.
// @Summary      Retry a blocked receipt
// @Tags         admin
// @Produce      json
// @Param        id  path  string  true  "Receipt ID"
// @Success      200  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/admin/receipts/{id}/requeue [post]
func (h *PaymentsHandler) RequeueReceipt(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeError(w, http.StatusServiceUnavailable, "no receipts store")
		return
	}
	id := chi.URLParam(r, "id")
	// Now, not on the next cycle: a person has just fixed whatever parked it
	// and is watching to see whether the document goes out.
	if err := h.store.RequeueReceipt(r.Context(), id, time.Now().UTC()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Deliberately the same answer for "no such receipt" and "that one
			// is not blocked": reopening an issued receipt would have the
			// drainer draw a second invoice number for a payment that already
			// has a document.
			writeError(w, http.StatusNotFound, "no blocked receipt with that id")
			return
		}
		slog.Error("payments: requeue receipt", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "could not requeue")
		return
	}
	if h.audit != nil {
		_ = h.audit.Log(r.Context(), store.DefaultTenantID, "receipt_requeued", operatorEmail(r), "receipt "+id, clientIPFromRequest(r))
	}
	slog.Info("payments: blocked receipt requeued by an operator", "id", id)
	writeJSON(w, http.StatusOK, map[string]string{"status": "pending", "id": id})
}

// operatorEmail names who did it in the audit trail. An admin action with no
// name in it is only half a record.
func operatorEmail(r *http.Request) string {
	if u := hubauth.FromContext(r.Context()); u != nil && u.Email != "" {
		return u.Email
	}
	return "unknown"
}

// vatThresholdResponse is the figure the flat Dutch rate depends on.
type vatThresholdResponse struct {
	Year int `json:"year"`
	// CrossBorderCents is the gross of issued receipts to EU consumers OUTSIDE
	// the Netherlands this calendar year. Domestic sales are excluded: the
	// threshold is about supplies to other member states, and counting our own
	// would raise a false alarm on the busiest possible month.
	CrossBorderCents int64 `json:"cross_border_cents"`
	// RefundedCents is what was given back and therefore never supplied. It is
	// already subtracted from CrossBorderCents; it is reported separately so a
	// person can see why the figure moved down (MESHSAT-1019).
	RefundedCents  int64            `json:"refunded_cents"`
	ThresholdCents int64            `json:"threshold_cents"`
	PercentUsed    float64          `json:"percent_used"`
	ByCountry      map[string]int64 `json:"by_country"`
	// Note says in words what the number means, because the person reading it
	// at 80% needs to know what to do, not just that a bar is filling.
	Note string `json:"note"`
}

// VATThreshold reports progress toward the EU cross-border threshold.
// @Summary      Progress toward the EU VAT threshold
// @Tags         admin
// @Produce      json
// @Success      200  {object}  vatThresholdResponse
// @Router       /api/admin/vat/threshold [get]
func (h *PaymentsHandler) VATThreshold(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeError(w, http.StatusServiceUnavailable, "no store")
		return
	}
	year := time.Now().UTC().Year()
	since := time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC)
	byCountry, err := h.store.CrossBorderSalesSince(r.Context(), since)
	if err != nil {
		slog.Error("vat: reading cross-border sales failed", "error", err)
		writeError(w, http.StatusInternalServerError, "could not read sales")
		return
	}
	// A sale that was given back is not a supply to another member state.
	// Counting it would report a distance to the threshold the business never
	// travelled, and the threshold is what the flat Dutch rate rests on.
	refunded, err := h.store.RefundsByCountrySince(r.Context(), since)
	if err != nil {
		slog.Error("vat: reading refunds failed", "error", err)
		writeError(w, http.StatusInternalServerError, "could not read refunds")
		return
	}
	out := vatThresholdResponse{
		Year: year, ThresholdCents: vat.Threshold, ByCountry: map[string]int64{},
	}
	// Everything stored is the GROSS the customer paid. The threshold is
	// measured on supplies excluding VAT, so strip it before totalling -- the
	// meter used to report the gross and therefore read 21% high.
	exVAT := func(cents int64) int64 { return vat.NetOfInclusive(cents, h.taxRate) }
	for c, cents := range byCountry {
		if !vat.CountsTowardThreshold(c) {
			continue
		}
		net := exVAT(cents) - exVAT(refunded[c])
		if net < 0 {
			// A refund of a sale from an earlier year, or a figure a person
			// should look at. Never let it pull the total below what was
			// actually supplied.
			net = 0
		}
		out.ByCountry[c] = net
		out.CrossBorderCents += net
	}
	for c, cents := range refunded {
		if vat.CountsTowardThreshold(c) {
			out.RefundedCents += exVAT(cents)
		}
	}
	out.PercentUsed = float64(out.CrossBorderCents) / float64(vat.Threshold) * 100
	out.Note = "Cross-border B2C sales to other EU member states this calendar year, " +
		"excluding VAT and net of refunds. " +
		"Dutch 21% may be charged on these while the total stays under the threshold. " +
		"Above it, the customer's own country rate applies and OSS registration is required."
	writeJSON(w, http.StatusOK, out)
}
