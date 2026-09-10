package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/audit"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// RefundsHandler is where an operator records that money went back
// (MESHSAT-1019).
//
// The money itself moves in the payment processor, by hand, because that is
// where it lives: Ko-fi never fires a webhook for a refund, and the Hub holds
// no gateway credentials. So this endpoint does not refund anybody. It records
// that a refund happened, and that record is what makes the credit note exist.
//
// Recording is deliberately an operator action rather than something inferred.
// A credit note is a legal document with a number out of a gapless series;
// producing one for an event nobody confirmed would be worse than producing
// none at all.
type RefundsHandler struct {
	audit *audit.Service
	store store.Store
}

// NewRefundsHandler returns a handler over the refunds outbox.
func NewRefundsHandler(a *audit.Service, s store.Store) *RefundsHandler {
	return &RefundsHandler{audit: a, store: s}
}

type recordRefundRequest struct {
	// AmountCents is what actually went back, in minor units. Omitted or zero
	// means the whole payment.
	AmountCents int64 `json:"amount_cents"`
	// Reason is why, in the operator's words. It reaches the audit line, never
	// the customer.
	Reason string `json:"reason"`
	// RefundedAt is when the money went back, RFC 3339. It dates the credit
	// note. Omitted means now.
	RefundedAt string `json:"refunded_at"`
}

// RecordRefund records money given back for one payment.
// @Summary      Record a refund against a payment
// @Tags         admin
// @Accept       json
// @Produce      json
// @Param        id       path  string               true  "Receipt ID"
// @Param        request  body  recordRefundRequest  true  "Refund"
// @Success      201  {object}  store.Refund
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Router       /api/admin/receipts/{id}/refund [post]
func (h *RefundsHandler) RecordRefund(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeError(w, http.StatusServiceUnavailable, "no store")
		return
	}
	var req recordRefundRequest
	if err := readJSON(w, r, &req, 4096); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := chi.URLParam(r, "id")
	receipt, err := h.store.GetReceipt(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no payment with that id")
			return
		}
		slog.Error("refunds: reading the payment failed", "receipt", id, "error", err)
		writeError(w, http.StatusInternalServerError, "could not read the payment")
		return
	}

	cents := req.AmountCents
	if cents == 0 {
		cents = receipt.AmountCents
	}
	if cents <= 0 {
		writeError(w, http.StatusBadRequest, "the refund amount must be positive")
		return
	}
	if cents > receipt.AmountCents {
		// Refusing rather than clamping: a figure larger than the payment means
		// somebody typed the wrong thing, and a credit note for more than was
		// ever charged reclaims VAT that was never declared.
		writeError(w, http.StatusBadRequest,
			"the refund of "+money(cents)+" is more than the "+money(receipt.AmountCents)+
				" "+receipt.Currency+" that was paid")
		return
	}

	refundedAt := time.Now().UTC()
	if s := strings.TrimSpace(req.RefundedAt); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			writeError(w, http.StatusBadRequest, "refunded_at must be an RFC 3339 time")
			return
		}
		refundedAt = t.UTC()
	}
	if refundedAt.Before(receipt.PaidAt) {
		writeError(w, http.StatusBadRequest, "the refund cannot be dated before the payment")
		return
	}

	rf := &store.Refund{
		TenantID:    receipt.TenantID,
		ReceiptID:   receipt.ID,
		AmountCents: cents,
		Currency:    receipt.Currency,
		Country:     receipt.Country,
		Reason:      strings.TrimSpace(req.Reason),
		RequestedBy: operatorEmail(r),
		RefundedAt:  refundedAt,
	}
	created, err := h.store.CreateRefund(r.Context(), rf)
	if err != nil {
		slog.Error("refunds: recording a refund failed", "receipt", id, "error", err)
		writeError(w, http.StatusInternalServerError, "could not record the refund")
		return
	}
	if !created {
		// One refund per payment. A second one would credit money that only
		// went back once; the operator deletes the first if it was wrong.
		writeError(w, http.StatusConflict, "this payment already has a refund recorded")
		return
	}

	slog.Info("refunds: refund recorded by an operator", "refund", rf.ID, "receipt", receipt.ID,
		"tenant", receipt.TenantID, "amount_cents", cents, "currency", receipt.Currency,
		"by", rf.RequestedBy)
	if h.audit != nil {
		_ = h.audit.Log(r.Context(), receipt.TenantID, "refund_recorded", rf.RequestedBy,
			money(cents)+" "+receipt.Currency+" refunded against payment "+receipt.ID+
				func() string {
					if rf.Reason == "" {
						return ""
					}
					return ": " + rf.Reason
				}(), clientIPFromRequest(r))
	}
	writeJSON(w, http.StatusCreated, rf)
}

// ListRefunds returns refunds in one state, newest first.
// @Summary      Refunds by state
// @Tags         admin
// @Produce      json
// @Param        status  query  string  false  "pending, issued or blocked"  default(pending)
// @Param        limit   query  int     false  "Max results"                 default(100)
// @Success      200  {array}  store.Refund
// @Router       /api/admin/refunds [get]
func (h *RefundsHandler) ListRefunds(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, http.StatusOK, []store.Refund{})
		return
	}
	status := r.URL.Query().Get("status")
	switch status {
	case "":
		status = store.RefundPending
	case store.RefundPending, store.RefundIssued, store.RefundBlocked:
	default:
		writeError(w, http.StatusBadRequest, "status must be pending, issued or blocked")
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	out, err := h.store.ListRefundsByStatus(r.Context(), status, limit)
	if err != nil {
		slog.Error("refunds: listing refunds failed", "status", status, "error", err)
		writeError(w, http.StatusInternalServerError, "could not read refunds")
		return
	}
	if out == nil {
		out = []store.Refund{}
	}
	writeJSON(w, http.StatusOK, out)
}

// RequeueRefund puts a blocked refund back in the drainer's queue.
// @Summary      Retry a blocked refund
// @Tags         admin
// @Produce      json
// @Param        id  path  string  true  "Refund ID"
// @Success      200  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/admin/refunds/{id}/requeue [post]
func (h *RefundsHandler) RequeueRefund(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeError(w, http.StatusServiceUnavailable, "no store")
		return
	}
	id := chi.URLParam(r, "id")
	// Now, not on the next cycle: a person has just fixed whatever parked it
	// and is watching to see whether the document goes out.
	if err := h.store.RequeueRefund(r.Context(), id, time.Now().UTC()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Deliberately the same answer for "no such refund" and "that one
			// is not blocked": reopening an issued refund would have the
			// drainer draw a second credit note number for money that already
			// has a document.
			writeError(w, http.StatusNotFound, "no blocked refund with that id")
			return
		}
		slog.Error("refunds: requeue failed", "refund", id, "error", err)
		writeError(w, http.StatusInternalServerError, "could not requeue")
		return
	}
	if h.audit != nil {
		_ = h.audit.Log(r.Context(), store.DefaultTenantID, "refund_requeued", operatorEmail(r),
			"refund "+id, clientIPFromRequest(r))
	}
	slog.Info("refunds: blocked refund requeued by an operator", "refund", id)
	writeJSON(w, http.StatusOK, map[string]string{"status": store.RefundPending, "id": id})
}

// DeleteRefund removes a refund that produced no document, so an operator who
// recorded the wrong figure can record the right one.
// @Summary      Withdraw a refund that has no credit note yet
// @Tags         admin
// @Produce      json
// @Param        id  path  string  true  "Refund ID"
// @Success      204
// @Failure      404  {object}  map[string]string
// @Router       /api/admin/refunds/{id} [delete]
func (h *RefundsHandler) DeleteRefund(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeError(w, http.StatusServiceUnavailable, "no store")
		return
	}
	id := chi.URLParam(r, "id")
	if err := h.store.DeleteRefund(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Same answer for "no such refund" and "that one already has a
			// credit note": once the document exists, deleting this row would
			// hide it rather than unmake it.
			writeError(w, http.StatusNotFound, "no refund with that id that has not been credited yet")
			return
		}
		slog.Error("refunds: delete failed", "refund", id, "error", err)
		writeError(w, http.StatusInternalServerError, "could not withdraw the refund")
		return
	}
	if h.audit != nil {
		_ = h.audit.Log(r.Context(), store.DefaultTenantID, "refund_withdrawn", operatorEmail(r),
			"refund "+id+" withdrawn before any credit note existed", clientIPFromRequest(r))
	}
	slog.Info("refunds: refund withdrawn by an operator", "refund", id)
	w.WriteHeader(http.StatusNoContent)
}

// money renders minor units for a message a person reads.
func money(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return sign + strconv.FormatInt(cents/100, 10) + "." +
		strings.TrimPrefix(strconv.FormatInt(100+cents%100, 10), "1")
}
