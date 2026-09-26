package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/audit"
	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/authentik"
	"github.com/meshsat/meshsat-hub/internal/mail"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// SignupHandler turns beta approval from a script somebody has to remember to
// run into something an operator does in the Hub (MESHSAT-978).
//
// Approval used to have a second half, which never worked. The address the
// request came from was POSTed to an n8n workflow that this file's own comment
// claimed "runs the allowlist script". It does not and never did: the workflow
// on that path is a notifier that reads an authentik payload, and a body it
// does not recognise takes its skip branch and answers 200. The Hub read that
// 200 as success and told the operator the address had been sent to the edge,
// so anyone approved through the UI rather than the CLI script was left unable
// to reach a Hub that said they could. Checked against the live instance on
// 2026-09-09: no workflow anywhere on it touches an allowlist or a VPS.
//
// The edge opened at public launch (MESHSAT-995) and there is no list to be
// admitted to, so approval is now what it says it is: activate the account and
// grant a role. The signup address is still recorded, because knowing where a
// request came from is worth having, but it never admitted anybody to anything
// and now it does not pretend to.
type SignupHandler struct {
	ak     *authentik.Client
	audit  *audit.Service
	mail   mail.Sender
	hubURL string
}

// NewSignupHandler creates the handler. A nil client disables the endpoints.
func NewSignupHandler(ak *authentik.Client, a *audit.Service) *SignupHandler {
	return &SignupHandler{ak: ak, audit: a}
}

func (h *SignupHandler) ready(w http.ResponseWriter) bool {
	if h.ak == nil {
		writeError(w, http.StatusServiceUnavailable, "identity provider not configured")
		return false
	}
	return true
}

// List returns the beta requests waiting for a decision.
//
//	@Summary      List pending beta requests
//	@Tags         admin
//	@Produce      json
//	@Success      200  {object}  map[string]interface{}
//	@Failure      503  {object}  map[string]string
//	@Router       /api/admin/signups [get]
func (h *SignupHandler) List(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	users, err := h.ak.ListPending(r.Context())
	if err != nil {
		slog.Error("signups: list failed", "error", err)
		writeError(w, http.StatusBadGateway, "could not reach the identity provider")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"signups": users, "count": len(users)})
}

type approveRequest struct {
	Role string `json:"role"`
}

// Approve admits a beta request.
//
//	@Summary      Approve a beta request
//	@Tags         admin
//	@Accept       json
//	@Produce      json
//	@Param        id    path      int             true  "authentik user pk"
//	@Param        body  body      approveRequest  true  "role to grant"
//	@Success      200   {object}  map[string]interface{}
//	@Failure      400   {object}  map[string]string
//	@Router       /api/admin/signups/{id}/approve [post]
func (h *SignupHandler) Approve(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	pk, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad signup id")
		return
	}
	req := approveRequest{Role: "owner"}
	if r.ContentLength > 0 {
		if err := readJSON(w, r, &req, 1024); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if !authentik.ValidRole(req.Role) {
		writeError(w, http.StatusBadRequest, "role must be owner, operator or viewer")
		return
	}
	signupIP, email, name, probe, err := h.ak.Approve(r.Context(), pk, req.Role)
	if err != nil {
		// A refusal is not an upstream fault: it means the pk does not name a
		// MeshSat signup waiting for a decision. Say so as a 409, so an operator
		// can tell "I picked the wrong row" apart from "authentik is down".
		switch {
		case errors.Is(err, authentik.ErrNotPending):
			slog.Warn("signups: approve refused, not a pending signup", "pk", pk)
			writeError(w, http.StatusConflict, "that account is not a signup awaiting a decision")
		case errors.Is(err, authentik.ErrEmailNotVerified):
			slog.Warn("signups: approve refused, email not verified", "pk", pk)
			writeError(w, http.StatusConflict, "that address has not been verified yet")
		default:
			slog.Error("signups: approve failed", "pk", pk, "error", err)
			writeError(w, http.StatusBadGateway, "approval failed at the identity provider")
		}
		return
	}
	// A verification probe (the nightly job's own account, attribute
	// verification_probe) is approved like anyone else but not written to:
	// its address is a catch-all that lands in the operator's inbox, and five
	// of these arrived there on the day the job was built.
	if h.mail != nil && email != "" && !probe {
		msg := mail.Approved(name, h.hubURL)
		mail.SendOrLog(r.Context(), h.mail, email, msg, "signup approved")
	}
	h.log(r, "signup_approved", email, "role="+req.Role+" ip="+signupIP)
	slog.Info("signup approved", "email", email, "role", req.Role, "signup_ip", signupIP, "probe", probe)
	writeJSON(w, http.StatusOK, map[string]any{
		"email": email, "role": req.Role, "signup_ip": signupIP,
		"next": "their first sign-in creates their tenant",
	})
}

// Reject deletes a beta request.
//
//	@Summary      Reject a beta request
//	@Tags         admin
//	@Produce      json
//	@Param        id   path      int  true  "authentik user pk"
//	@Success      200  {object}  map[string]string
//	@Failure      400  {object}  map[string]string
//	@Router       /api/admin/signups/{id}/reject [post]
func (h *SignupHandler) Reject(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	pk, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad signup id")
		return
	}
	// An optional reason, sent to the person and kept in the audit row. The
	// body is optional because the old client sent none.
	reason := ""
	if r.ContentLength != 0 {
		var req rejectRequest
		if err := readJSON(w, r, &req, 4096); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		reason = strings.Join(strings.Fields(req.Reason), " ")
		if utf8.RuneCountInString(reason) > rejectReasonMaxLen {
			writeError(w, http.StatusBadRequest, "reason must be at most 500 characters")
			return
		}
	}
	email, name, err := h.ak.Reject(r.Context(), pk)
	if err != nil {
		// Never echo err.Error() here: it carries the target account's address
		// and, for transport failures, the identity provider's URLs and status
		// lines. The operator gets the detail in the log, the client gets a
		// fixed string and an honest status.
		if errors.Is(err, authentik.ErrNotPending) {
			slog.Warn("signups: reject refused, not a pending signup", "pk", pk)
			writeError(w, http.StatusConflict, "that account is not a signup awaiting a decision")
			return
		}
		slog.Error("signups: reject failed", "pk", pk, "error", err)
		writeError(w, http.StatusBadGateway, "rejection failed at the identity provider")
		return
	}
	// Tell them (MESHSAT-1082). Rejection used to be silent: the account was
	// deleted, an audit row was written, and somebody who had signed up and
	// confirmed their address waited for an answer that was never coming.
	//
	// SendOrLog, so a relay that is down cannot turn a completed rejection into
	// an error. The decision has already been made and the account is already
	// gone; failing the request would tell the operator it had not worked.
	if h.mail != nil && email != "" {
		mail.SendOrLog(r.Context(), h.mail, email, mail.Rejected(name, reason), "signup rejected")
	}
	// reason last, because it may contain spaces and "=": a reader takes it
	// as the rest of the line (parseSignupDetail does).
	detail := ""
	if reason != "" {
		detail = "reason=" + reason
	}
	h.log(r, "signup_rejected", email, detail)
	writeJSON(w, http.StatusOK, map[string]string{"email": email, "status": "rejected"})
}

type rejectRequest struct {
	Reason string `json:"reason"`
}

const rejectReasonMaxLen = 500

// signupDecision is one row of the decision history: what an operator did
// with a request, read back from the platform's audit chain.
type signupDecision struct {
	ID        string `json:"id"`
	Action    string `json:"action"`
	Email     string `json:"email"`
	Role      string `json:"role,omitempty"`
	SignupIP  string `json:"signup_ip,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Actor     string `json:"actor"`
	IP        string `json:"ip,omitempty"`
	CreatedAt string `json:"created_at"`
}

// parseSignupDetail reads the "key=value ..." detail this handler writes.
// "reason=" takes the rest of the line.
func parseSignupDetail(detail string) (email, role, ip, reason string) {
	rest := detail
	for rest != "" {
		if strings.HasPrefix(rest, "reason=") {
			reason = strings.TrimPrefix(rest, "reason=")
			break
		}
		tok := rest
		if i := strings.IndexByte(rest, ' '); i >= 0 {
			tok, rest = rest[:i], strings.TrimLeft(rest[i+1:], " ")
		} else {
			rest = ""
		}
		k, v, _ := strings.Cut(tok, "=")
		switch k {
		case "email":
			email = v
		case "role":
			role = v
		case "ip":
			ip = v
		}
	}
	return
}

// History lists the decisions taken on account requests, newest first. It
// reads the audit chain, so it works without an identity provider and
// survives the request itself being deleted there.
//
//	@Summary      Decisions taken on account requests
//	@Tags         admin
//	@Produce      json
//	@Param        limit  query     int  false  "max rows (default 100)"
//	@Success      200    {array}   signupDecision
//	@Router       /api/admin/signups/history [get]
func (h *SignupHandler) History(w http.ResponseWriter, r *http.Request) {
	out := []signupDecision{}
	if h.audit == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	entries, err := h.audit.Store().ListAuditEntriesByAction(r.Context(), store.DefaultTenantID,
		[]string{"signup_approved", "signup_rejected"}, parseLimit(r, 100, maxListLimit))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read the decision history")
		return
	}
	for _, e := range entries {
		email, role, ip, reason := parseSignupDetail(e.Detail)
		out = append(out, signupDecision{ID: e.ID, Action: e.Action, Email: email, Role: role, SignupIP: ip, Reason: reason,
			Actor: e.Actor, IP: e.IP, CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339)})
	}
	writeJSON(w, http.StatusOK, out)
}

// SetMailer gives the handler a way to tell somebody they were approved. Until
// this existed, only the CLI approval path sent anything, so a person approved
// through the UI was never told and an operator had to message them by hand.
func (h *SignupHandler) SetMailer(s mail.Sender, hubURL string) {
	h.mail, h.hubURL = s, hubURL
}

func (h *SignupHandler) log(r *http.Request, action, subject, detail string) {
	if h.audit == nil {
		return
	}
	actor := "unknown"
	if u := hubauth.FromContext(r.Context()); u != nil && u.Email != "" {
		actor = u.Email
	}
	if detail == "" {
		detail = "email=" + subject
	} else {
		detail = "email=" + subject + " " + detail
	}
	if err := h.audit.Log(r.Context(), store.DefaultTenantID, action, actor, detail, clientIPFromRequest(r)); err != nil {
		slog.Warn("audit: failed to log "+action, "error", err)
	}
}
