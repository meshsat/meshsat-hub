// Package kofi turns a Ko-fi payment into a subscription tier (MESHSAT-989).
//
// Ko-fi is what the Hub already uses to take money, so it is what the tiers
// are billed through. It shapes the design in one important way: Ko-fi fires a
// webhook when somebody pays and never when they cancel. There is no lapse
// event to listen for and no way to ask.
//
// So a payment does not set a subscription state; it extends an expiry.
// plan_expires_at moves out by a month and a couple of days of slack, and a
// plan that stops being paid for lapses when that date passes. Nothing has to
// be cancelled, and a missed webhook costs a tenant a few days rather than
// leaving them on a tier forever.
//
// What lapsing does is narrow on purpose: the tenant drops to the free tier,
// which refuses NEW device registrations. Everything already registered keeps
// working, keeps reporting, and keeps its SOS path. Payment is not a lever we
// pull on somebody's distress call.
package kofi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/meshsat/meshsat-hub/internal/mail"
	"github.com/meshsat/meshsat-hub/internal/metrics"
	"github.com/meshsat/meshsat-hub/internal/plans"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// Period is how long one payment buys. A month plus two days: Ko-fi charges a
// membership on the same day each month and the webhook can be late, and a
// tenant should never lapse because a renewal landed a few hours after the
// clock rolled over.
const Period = 32 * 24 * time.Hour

// Payload is the subset of Ko-fi's webhook JSON this needs. Ko-fi posts it as
// a single form field named "data".
//
// Field names follow Ko-fi's documented shape. Unknown fields are ignored:
// this is a third party's payload and it will grow.
type Payload struct {
	VerificationToken       string `json:"verification_token"`
	MessageID               string `json:"message_id"`
	KofiTransactionID       string `json:"kofi_transaction_id"`
	Type                    string `json:"type"`
	IsSubscriptionPayment   bool   `json:"is_subscription_payment"`
	IsFirstSubscriptionPmnt bool   `json:"is_first_subscription_payment"`
	TierName                string `json:"tier_name"`
	Email                   string `json:"email"`
	Message                 string `json:"message"`
	Amount                  string `json:"amount"`
	Currency                string `json:"currency"`
	Timestamp               string `json:"timestamp"`
}

// TenantStore is the slice of the store this needs.
type TenantStore interface {
	GetTenant(ctx context.Context, id string) (*store.Tenant, error)
	ListTenants(ctx context.Context) ([]store.Tenant, error)
	UpdateTenant(ctx context.Context, t *store.Tenant) error
	// ApplyKofiDelivery writes the grant only if this delivery has not already
	// been applied, and reports whether it did. See store.Store for why this is
	// a compare-and-set rather than a read followed by an update.
	ApplyKofiDelivery(ctx context.Context, t *store.Tenant, deliveryKey string) (bool, error)
}

// Auditor records who was moved to which tier and why. Matches
// audit.Service.Log.
type Auditor interface {
	Log(ctx context.Context, tenantID, action, actor, detail, ip string) error
}

// Handler serves POST /api/webhook/kofi/{secret}.
type Handler struct {
	store TenantStore
	// token is the verification_token from the Ko-fi webhook settings page.
	// Empty means the endpoint refuses everything: an unconfigured payment
	// endpoint that accepts payloads is worse than one that is switched off.
	token string
	audit Auditor
	// forget drops the tenant's cached record on the other replicas so a paid
	// upgrade applies to the next request rather than at the end of a TTL.
	forget func(tenantID string)
	// tierFor maps a Ko-fi tier name to a plan. Configurable because the tier
	// names live in somebody's Ko-fi page, not in this repository.
	tierFor map[string]string
	// mail tells the customer their plan changed. nil means no relay is
	// configured and nothing is sent; the grant is unaffected either way.
	mail   mail.Sender
	hubURL string
	// receipts is the outbox that owes the customer a document (MESHSAT-998).
	// nil when no billing system is configured, in which case payments still
	// grant plans and nothing is recorded.
	receipts ReceiptStore
	// users resolves a tenant owner to an account, which is both how a payment
	// is matched by address and where the receipt is sent.
	users UserLookup
}

// UserLookup is the slice of the store needed to turn a tenant's owner into an
// account. Tenant.OwnerUserID holds a user id, never an address.
type UserLookup interface {
	GetUserByID(ctx context.Context, tenantID, id string) (*store.LocalUser, error)
}

// NewHandler creates the Ko-fi webhook handler.
func NewHandler(s TenantStore, verificationToken string) *Handler {
	return &Handler{store: s, token: verificationToken, tierFor: map[string]string{}}
}

// SetAudit attaches the audit log.
// SetMailer gives the handler a way to confirm a payment to the person who made
// it, so they hear from us and not only from the payment processor.
func (h *Handler) SetMailer(s mail.Sender, hubURL string) {
	h.mail, h.hubURL = s, hubURL
}

func (h *Handler) SetAudit(a Auditor) { h.audit = a }

// SetInvalidator wires the cross-replica cache drop.
func (h *Handler) SetInvalidator(f func(tenantID string)) { h.forget = f }

// SetReceipts wires the receipt outbox. Without it a payment still grants a
// plan and no document is ever produced, which is the state this replaced.
func (h *Handler) SetReceipts(r ReceiptStore) { h.receipts = r }

// SetUserLookup wires the tenant-owner lookup used to match a payment by
// address and to address the receipt.
func (h *Handler) SetUserLookup(u UserLookup) { h.users = u }

// SetTierMapping maps Ko-fi tier names (lowercased) to plan names. A tier name
// that is not mapped falls back to matching the plan name itself, so a Ko-fi
// tier called "Crew" works with no configuration.
func (h *Handler) SetTierMapping(m map[string]string) {
	h.tierFor = map[string]string{}
	for kofiTier, plan := range m {
		if plans.Known(plan) {
			h.tierFor[strings.ToLower(strings.TrimSpace(kofiTier))] = plans.Normalise(plan)
		} else {
			slog.Warn("kofi: tier mapped to an unknown plan, ignored", "tier", kofiTier, "plan", plan)
		}
	}
}

// ServeHTTP handles a Ko-fi payment notification.
//
//	@Summary      Ko-fi payment webhook
//	@Description  Extends the paying tenant's subscription. The path secret identifies the endpoint and Ko-fi's verification_token authenticates the caller.
//	@Tags         webhooks
//	@Accept       x-www-form-urlencoded
//	@Produce      json
//	@Success      200  {object}  map[string]string
//	@Failure      400  {object}  map[string]string
//	@Failure      401  {object}  map[string]string
//	@Router       /api/webhook/kofi/{secret} [post]
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.token == "" {
		slog.Warn("kofi: webhook received but HUB_KOFI_VERIFICATION_TOKEN is unset, refusing")
		http.Error(w, `{"error":"not configured"}`, http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, `{"error":"bad form"}`, http.StatusBadRequest)
		return
	}
	raw := r.FormValue("data")
	if raw == "" {
		http.Error(w, `{"error":"missing data"}`, http.StatusBadRequest)
		return
	}
	if len(raw) > 64*1024 {
		http.Error(w, `{"error":"payload too large"}`, http.StatusRequestEntityTooLarge)
		return
	}

	var p Payload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
		return
	}
	// Constant time: the token is a bearer secret in a body, and a length or
	// prefix oracle on it is the whole authentication.
	if subtle.ConstantTimeCompare([]byte(p.VerificationToken), []byte(h.token)) != 1 {
		slog.Warn("kofi: verification token mismatch", "remote", r.RemoteAddr, "txn", p.KofiTransactionID)
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	ctx := r.Context()

	// Ko-fi sends one-off donations through the same webhook. Those are
	// support, not subscriptions, and must never grant a tier -- but money
	// arrived, so a document is owed for it (owner ruling, 2026-09-10:
	// everything that comes in gets a receipt rather than a judgement about
	// whether it is a taxable supply). It is recorded against whichever tenant
	// the payer can be matched to, and against the platform tenant when they
	// cannot, so the payer's own address is what the document goes to.
	if !p.IsSubscriptionPayment {
		slog.Info("kofi: one-off payment received, no tier change", "txn", p.KofiTransactionID)
		t, _, err := h.match(ctx, p)
		if err != nil || t == nil {
			t = &store.Tenant{ID: store.DefaultTenantID, Name: donorName(p)}
		}
		h.recordReceipt(ctx, t, p, DonationPlan)
		writeOK(w, "thanks")
		return
	}

	t, how, err := h.match(ctx, p)
	if err != nil || t == nil {
		// Loud and left alone. Guessing which customer a payment belongs to is
		// how one tenant ends up paying for another's fleet, so an unmatched
		// payment is an operator's job, not a heuristic's.
		//
		// But it has to be findable. Until this was recorded the only trace of
		// a payment nobody was upgraded for was a log line, which nothing
		// alerts on and nobody reads at the moment it matters (MESHSAT-1007).
		slog.Warn("kofi: subscription payment matched no tenant, left for an operator",
			"txn", p.KofiTransactionID, "tier", p.TierName, "has_message", p.Message != "",
			"error", err)
		metrics.KofiUnmatchedPaymentsTotal.Inc()
		h.recordUnmatched(ctx, p, err)
		writeOK(w, "received")
		return
	}

	// Ko-fi retries a delivery until it gets a 200, so a response lost on the
	// way back would apply the same payment twice and buy a second month for
	// nothing. deliveryKey identifies the delivery -- message id, else the
	// transaction id, else a digest of the payment -- and unlike a message_id it
	// always has a value. ApplyKofiDelivery below claims it and grants in one
	// transaction, so a replay, a retry arriving after a later payment, and two
	// replicas racing the same delivery all resolve to exactly one month.
	key := deliveryKey(p)
	plan := h.planFor(p.TierName)

	// Record that money arrived before granting anything. The receipt is for
	// the payment, not for the grant: if the grant below fails, the customer
	// has still paid and is still owed a document. The insert is guarded by a
	// unique delivery key, so a replayed delivery adds nothing.
	h.recordReceipt(ctx, t, p, plan)

	prev, prevExpiry := t.Plan, t.PlanExpiresAt
	t.KofiLastMessageID = p.MessageID

	// Remember who paid. Ko-fi carries the supporter's message only on the join
	// payment, so the claim code that matched this one will not be in next
	// month's. Binding the payer's address here is what makes the renewal
	// match, and it is written in the same update as the plan.
	if payer := strings.ToLower(strings.TrimSpace(p.Email)); payer != "" && !strings.EqualFold(t.KofiPayerEmail, payer) {
		if t.KofiPayerEmail != "" {
			slog.Info("kofi: payer address for this tenant changed", "tenant", t.ID, "matched_by", how)
		}
		t.KofiPayerEmail = payer
	}
	now := time.Now().UTC()
	from := now
	if t.PlanExpiresAt != nil && t.PlanExpiresAt.After(now) {
		// Renewals stack rather than reset, so paying early is never punished.
		from = *t.PlanExpiresAt
	}
	expires := from.Add(Period)
	t.Plan, t.PlanExpiresAt = plan, &expires

	applied, err := h.store.ApplyKofiDelivery(ctx, t, key)
	if err != nil {
		slog.Error("kofi: could not apply a payment", "tenant", t.ID, "txn", p.KofiTransactionID, "error", err)
		http.Error(w, `{"error":"update failed"}`, http.StatusInternalServerError)
		return
	}
	if !applied {
		// Another replica got this same delivery in between our read and our
		// write. Its grant stands; ours would be a second month for one payment.
		slog.Info("kofi: duplicate delivery ignored at the write", "tenant", t.ID,
			"delivery", key, "txn", p.KofiTransactionID)
		writeOK(w, "already applied")
		return
	}
	if h.forget != nil {
		h.forget(t.ID)
	}
	if h.mail != nil {
		if to := h.receiptEmail(ctx, t, p); to != "" {
			subject, body := mail.PlanChanged(h.ownerName(ctx, t), plan, plans.For(plan).Devices, expires, h.hubURL)
			mail.SendOrLog(ctx, h.mail, to, subject, body, "plan changed")
		}
	}
	slog.Info("kofi: subscription applied", "tenant", t.ID, "plan", plan, "was", prev,
		"expires", expires.Format(time.RFC3339), "matched_by", how, "txn", p.KofiTransactionID)
	if h.audit != nil {
		detail := "plan " + prev + " -> " + plan + ", expires " + expires.Format(time.RFC3339) + ", matched by " + how
		if prevExpiry != nil {
			detail += ", previous expiry " + prevExpiry.Format(time.RFC3339)
		}
		// The transaction id goes in; the verification token never does.
		_ = h.audit.Log(ctx, t.ID, "subscription_paid", "kofi:"+p.KofiTransactionID, detail, "")
	}
	writeOK(w, "applied")
}

func writeOK(w http.ResponseWriter, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
}

// planFor maps a Ko-fi tier name to a plan: the configured mapping first, then
// the tier name read as a plan name, then crew as the smallest paid tier. The
// last fallback is deliberate -- somebody paid, and dropping them to free
// because a tier was renamed in Ko-fi is the wrong way to be wrong.
func (h *Handler) planFor(tierName string) string {
	key := strings.ToLower(strings.TrimSpace(tierName))
	if p, ok := h.tierFor[key]; ok {
		return p
	}
	// Only the paid tiers we sell, read as plan names. Deliberately not
	// plans.Known: that table also holds custom and beta, both unlimited, so a
	// Ko-fi tier somebody names "Custom" would buy an uncapped fleet for the
	// price of Crew. Those two are operator-set and must stay that way.
	if key == plans.Crew || key == plans.Fleet {
		return plans.Normalise(key)
	}
	slog.Warn("kofi: unrecognised tier name, granting the smallest paid tier", "tier", tierName)
	return plans.Crew
}

var claimCodeRe = regexp.MustCompile(`\b[A-Z0-9]{8}\b`)

// ErrNoMatch is returned when a payment cannot be attributed.
var ErrNoMatch = errors.New("kofi: no tenant matched")

// ErrAmbiguous is returned when a payment could belong to more than one
// tenant. Never guessed at.
var ErrAmbiguous = errors.New("kofi: payment matches more than one tenant")

// match finds the tenant a payment belongs to: the claim code in the Ko-fi
// message first, then the payer's email against the tenant owner's.
//
// The claim code exists because email is fragile here. People pay from a
// personal address, from a company card, or through a partner's account, and a
// payment attributed to the wrong tenant is worse than one attributed to none.
func (h *Handler) match(ctx context.Context, p Payload) (*store.Tenant, string, error) {
	tenants, err := h.store.ListTenants(ctx)
	if err != nil {
		return nil, "", err
	}
	live := func(t store.Tenant) bool { return t.DeletedAt == nil }
	email := strings.ToLower(strings.TrimSpace(p.Email))

	// 1. The claim code in the supporter's message. Only the join payment
	//    carries one, so this is what binds a payer to a tenant the first time.
	for _, code := range claimCodeRe.FindAllString(strings.ToUpper(p.Message), -1) {
		for i := range tenants {
			if live(tenants[i]) && tenants[i].KofiClaimCode != "" && tenants[i].KofiClaimCode == code {
				t := tenants[i]
				return &t, "claim code", nil
			}
		}
	}

	if email == "" {
		return nil, "", ErrNoMatch
	}

	// 2. The address a previous payment for this tenant came from. This is the
	//    one that carries renewals: Ko-fi sends message null every month after
	//    the join, so without it a subscriber whose Ko-fi address differs from
	//    their account address would match once and then lapse while paying.
	if t, err := unique(tenants, func(t store.Tenant) bool {
		return live(t) && t.KofiPayerEmail != "" && strings.EqualFold(t.KofiPayerEmail, email)
	}); err == nil && t != nil {
		return t, "remembered payer", nil
	} else if err != nil {
		return nil, "", err
	}

	// 3. The account owner's own address, for the straightforward case where
	//    somebody pays from the address they signed up with.
	//
	//    This used to compare the payer's address to Tenant.OwnerUserID, which
	//    holds a user id (internal/api/oidc.go assigns it from the created
	//    user), never an address. The comparison could not be true for any
	//    account, so a first-time payer who omitted the claim code was
	//    silently left unmatched. The owner has to be looked up.
	t, err := unique(tenants, func(t store.Tenant) bool {
		return live(t) && h.ownerEmail(ctx, t) == email
	})
	if err != nil || t == nil {
		if err == nil {
			err = ErrNoMatch
		}
		return nil, "", err
	}
	return t, "email", nil
}

// ownerEmail is the tenant owner's account address, lowercased, or "" when
// there is no owner, no lookup wired, or the account has gone.
//
// One query per tenant per unmatched payment. That is fine at this size and
// the alternative -- an index keyed on an address that can change -- is a
// cache to keep correct for a code path that runs a few times a month.
// ownerName is the display name of the tenant's owner, for greeting them by
// name in mail. Empty is fine: the templates fall back to a plain "Hello,".
func (h *Handler) ownerName(ctx context.Context, t *store.Tenant) string {
	if h.users == nil || t == nil || t.OwnerUserID == "" {
		return ""
	}
	u, err := h.users.GetUserByID(ctx, t.ID, t.OwnerUserID)
	if err != nil || u == nil {
		return ""
	}
	return u.Name
}

func (h *Handler) ownerEmail(ctx context.Context, t store.Tenant) string {
	if h.users == nil || t.OwnerUserID == "" {
		return ""
	}
	u, err := h.users.GetUserByID(ctx, t.ID, t.OwnerUserID)
	if err != nil || u == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(u.Email))
}

// unique returns the single tenant matching pred. Two matches is not a tie to
// break: attributing a payment to the wrong customer is worse than attributing
// it to nobody, so it returns ErrAmbiguous and an operator decides.
func unique(tenants []store.Tenant, pred func(store.Tenant) bool) (*store.Tenant, error) {
	var found *store.Tenant
	for i := range tenants {
		if !pred(tenants[i]) {
			continue
		}
		if found != nil {
			return nil, ErrAmbiguous
		}
		t := tenants[i]
		found = &t
	}
	return found, nil
}

// claimAlphabet omits the characters people mistype when copying a code off a
// screen into a Ko-fi message box: I/1, O/0, and the letters that look like
// them.
const claimAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// NewClaimCode returns a fresh 8-character claim code.
func NewClaimCode() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, 8)
	for i, v := range b {
		out[i] = claimAlphabet[int(v)%len(claimAlphabet)]
	}
	return string(out), nil
}

// donorName is what a one-off donor is called on their receipt when no tenant
// owns them. Ko-fi gives us an address and nothing else, so the local part is
// the closest thing to a name there is.
func donorName(p Payload) string {
	if i := strings.IndexByte(p.Email, '@'); i > 0 {
		return p.Email[:i]
	}
	if p.Email != "" {
		return p.Email
	}
	return "Ko-fi supporter"
}

// UnmatchedAction is the audit action a payment nobody could be found for is
// recorded under, and what the admin listing filters on.
const UnmatchedAction = "kofi_payment_unmatched"

// unmatchedPayment is what an operator needs to attribute a payment by hand:
// who paid, how much, for which tier, and what they wrote in the message that
// should have carried a claim code.
type unmatchedPayment struct {
	TransactionID string `json:"transaction_id"`
	Tier          string `json:"tier,omitempty"`
	Amount        string `json:"amount,omitempty"`
	Currency      string `json:"currency,omitempty"`
	PayerEmail    string `json:"payer_email,omitempty"`
	Message       string `json:"message,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// recordUnmatched writes the payment to the audit log of the platform tenant,
// which is the one surface a platform admin already has and already trusts --
// it is hash-chained, retained and exported like every other entry.
func (h *Handler) recordUnmatched(ctx context.Context, p Payload, matchErr error) {
	if h.audit == nil {
		return
	}
	reason := "no claim code, payer address, or remembered payer matched a tenant"
	if matchErr != nil {
		reason = matchErr.Error()
	}
	// The message can be anything a stranger typed; keep it bounded.
	msg := p.Message
	if len(msg) > 500 {
		msg = msg[:500]
	}
	detail, err := json.Marshal(unmatchedPayment{
		TransactionID: p.KofiTransactionID,
		Tier:          p.TierName,
		Amount:        p.Amount,
		Currency:      p.Currency,
		PayerEmail:    p.Email,
		Message:       msg,
		Reason:        reason,
	})
	if err != nil {
		slog.Warn("kofi: could not record an unmatched payment", "txn", p.KofiTransactionID, "error", err)
		return
	}
	if err := h.audit.Log(ctx, store.DefaultTenantID, UnmatchedAction, "kofi_webhook", string(detail), ""); err != nil {
		slog.Error("kofi: an unmatched payment was not recorded; it exists only in the log now",
			"txn", p.KofiTransactionID, "error", err)
	}
}
