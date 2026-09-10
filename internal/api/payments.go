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
	"github.com/meshsat/meshsat-hub/internal/kofi"
	"github.com/meshsat/meshsat-hub/internal/store"
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
}

// NewPaymentsHandler returns a handler over the audit log's payment records
// and the receipts outbox.
func NewPaymentsHandler(a *audit.Service, s store.Store) *PaymentsHandler {
	return &PaymentsHandler{audit: a, store: s}
}

type unmatchedPaymentResponse struct {
	RecordedAt string          `json:"recorded_at"`
	Payment    json.RawMessage `json:"payment"`
}

// ListUnmatched returns Ko-fi subscription payments that matched no tenant.
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
		if e.Action != kofi.UnmatchedAction {
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
