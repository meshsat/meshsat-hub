package stripe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The outbound half: starting a checkout, and opening the customer portal.
//
// Hand-rolled against Stripe's form-encoded REST API, for the same reason
// internal/invoiceninja is: six calls do not justify a large dependency in a
// codebase with seventeen direct ones and an explicit minimalism rule.
//
// Two things every request here does deliberately:
//
//   - automatic_tax[enabled]=false, always and explicitly. Stripe Tax is off
//     because there is no NL registration behind it, and the dashboard carries a
//     separate "use automatic tax" toggle that has been found ON. Sending the
//     field means a dashboard setting cannot quietly start putting EUR 0 VAT on
//     a VAT-registered business's transactions.
//   - metadata[tenant_id], on the session AND on the subscription it creates.
//     That is what binds a payment to a tenant by construction. Ko-fi had no
//     such field, which is why it needed a claim code typed into a message box
//     and a list of payments nobody could place.

const apiBase = "https://api.stripe.com/v1"

// Client calls Stripe. A nil *Client is safe and reports ErrNotConfigured, so
// the Hub runs with billing switched off.
type Client struct {
	key     string
	http    *http.Client
	baseURL string
	// now exists so a test can pin the attempt bucket.
	now func() time.Time
}

// NewClient returns a client, or nil when no key is configured.
func NewClient(secretKey string, timeout time.Duration) *Client {
	if strings.TrimSpace(secretKey) == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return &Client{key: secretKey, http: &http.Client{Timeout: timeout}, baseURL: apiBase, now: time.Now}
}

// SetBaseURL points the client at a test double.
func (c *Client) SetBaseURL(u string) { c.baseURL = strings.TrimRight(u, "/") }

// Live reports whether this client talks to real money. Test keys are prefixed
// sk_test_; anything else is treated as live, because guessing the safe way
// round would be the wrong guess.
func (c *Client) Live() bool {
	return c != nil && !strings.HasPrefix(c.key, "sk_test_")
}

// Error is a refusal from Stripe.
type Error struct {
	Status int
	Op     string
	Body   string
}

func (e *Error) Error() string {
	return fmt.Sprintf("stripe: %s: HTTP %d: %s", e.Op, e.Status, e.Body)
}

// Retryable reports whether trying again could plausibly work.
func (e *Error) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// Session is a hosted page for the customer to go to.
type Session struct {
	ID  string `json:"id"`
	URL string `json:"url"`
	// ClientSecret is returned only for ui_mode=embedded, where there is no
	// hosted URL to send anyone to: the form is mounted inside our own page.
	ClientSecret string `json:"client_secret"`
}

// CheckoutRequest starts a subscription.
type CheckoutRequest struct {
	TenantID string
	Email    string
	PriceID  string
	// PlanName is what the customer is buying, for the wording on the hosted
	// page. Empty is fine; the line item still names the product.
	PlanName   string
	SuccessURL string
	CancelURL  string
	// CustomerID reuses an existing Stripe customer when this tenant has one,
	// so a returning subscriber does not end up as a second customer record
	// with the same card. Invoice Ninja has the same problem in reverse and it
	// is worth not repeating here.
	CustomerID string
}

// Checkout creates a Checkout session for a subscription.
func (c *Client) Checkout(ctx context.Context, req CheckoutRequest) (*Session, error) {
	if c == nil {
		return nil, ErrNotConfigured
	}
	if strings.TrimSpace(req.TenantID) == "" || strings.TrimSpace(req.PriceID) == "" {
		return nil, fmt.Errorf("stripe: a tenant and a price are required")
	}
	f := url.Values{}
	f.Set("mode", "subscription")
	f.Set("line_items[0][price]", req.PriceID)
	f.Set("line_items[0][quantity]", "1")
	f.Set("success_url", req.SuccessURL)
	f.Set("cancel_url", req.CancelURL)
	// The buyer's country, which decides whether Dutch VAT applies at all.
	// Ko-fi never sent one, so every donation from a stranger parked.
	f.Set("billing_address_collection", "required")
	f.Set("automatic_tax[enabled]", "false")
	f.Set("metadata[tenant_id]", req.TenantID)
	// Copied onto the subscription, so every later subscription event carries
	// the tenant without a lookup.
	f.Set("subscription_data[metadata][tenant_id]", req.TenantID)
	// The hosted page's logo, colours and title come from the ACCOUNT, and this
	// account is the operating entity's rather than the product's -- so a
	// customer subscribing to MeshSat Hub sees the entity's name at the top.
	// Account branding cannot be set through the API by the account itself
	// ("you may only use it on connected accounts"), so it is a dashboard job.
	//
	// What IS controllable per session is the wording, and the line item, which
	// carries the product name. Both are used to say plainly whose page this is
	// and what is being bought -- the same reasoning as the branded email: a
	// customer who cannot tell who is charging them has a reason to stop.
	f.Set("custom_text[submit][message]",
		"MeshSat Hub "+tierLabel(req.PriceID, req.PlanName)+". Your plan starts as soon as this "+
			"goes through, and you can cancel it yourself from Settings at any time. "+
			"The price includes VAT, and a receipt follows by email.")
	if req.CustomerID != "" {
		f.Set("customer", req.CustomerID)
	} else if req.Email != "" {
		f.Set("customer_email", req.Email)
	}
	var s Session
	if err := c.post(ctx, "/checkout/sessions", f,
		idempotency("checkout", req.TenantID, req.PriceID, c.attemptBucket()), &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// DonationRequest starts a one-off payment for an amount the donor chooses.
type DonationRequest struct {
	TenantID   string // may be empty: a donation from somebody with no account
	Email      string
	PriceID    string // a price with a customer-chosen amount
	SuccessURL string
	CancelURL  string
	// ReturnURL, when set, switches the session to ui_mode=embedded: Stripe
	// renders only the payment form and we host it, so the page around it can
	// say what the money is for. Mutually exclusive with SuccessURL/CancelURL --
	// Stripe refuses a session carrying both.
	ReturnURL string
}

// Donation creates a Checkout session for a one-off payment. It buys no tier;
// it buys a document, which is the owner's ruling on donations.
func (c *Client) Donation(ctx context.Context, req DonationRequest) (*Session, error) {
	if c == nil {
		return nil, ErrNotConfigured
	}
	if strings.TrimSpace(req.PriceID) == "" {
		return nil, fmt.Errorf("stripe: a price is required")
	}
	f := url.Values{}
	f.Set("mode", "payment")
	f.Set("line_items[0][price]", req.PriceID)
	f.Set("line_items[0][quantity]", "1")
	if req.ReturnURL != "" {
		// Embedded: Stripe draws the form, we draw everything around it.
		// The value is DERIVED from the pinned version, not hardcoded -- see
		// embeddedUIMode.
		f.Set("ui_mode", embeddedUIMode())
		f.Set("return_url", req.ReturnURL)
	} else {
		f.Set("success_url", req.SuccessURL)
		f.Set("cancel_url", req.CancelURL)
	}
	f.Set("billing_address_collection", "required")
	f.Set("automatic_tax[enabled]", "false")
	// The button says what the transaction is. Stripe's default is "Pay", which
	// is what you write for a purchase; this is a gift that buys nothing, and
	// telling somebody they are paying for something is the wrong word at the
	// last moment before they part with money.
	f.Set("submit_type", "donate")
	f.Set("custom_text[submit][message]",
		"MeshSat is open source and this buys no subscription, no features and no support. "+
			"It pays for the satellite airtime, the hardware and the servers that keep the "+
			"network answering when the usual ones are not.")
	// Says what this session is for. A donation session may legitimately carry
	// no tenant; without this marker the webhook could not tell that from a
	// payment that lost its tenant, and would have to guess at one of them.
	f.Set("metadata["+MetadataKind+"]", KindDonation)
	if req.TenantID != "" {
		f.Set("metadata[tenant_id]", req.TenantID)
	}
	if req.Email != "" {
		f.Set("customer_email", req.Email)
	}
	// Deliberately NOT stable, unlike Checkout's key. A donation is repeatable:
	// the same person may give twice, for the same amount, from the same
	// account. A key derived from tenant and price would make Stripe answer the
	// second one with the FIRST session for 24 hours, so a genuine second gift
	// would silently reuse a session that is already paid or expired.
	//
	// Nothing is lost by that. do() performs a single request with no retry
	// loop, so there is no automatic retry to deduplicate, and an unpaid
	// Checkout session has no effect on anything and simply expires. A
	// double-clicked button costs one abandoned session, not one extra charge.
	var s Session
	if err := c.post(ctx, "/checkout/sessions", f, idempotency("donation", req.TenantID, strconv.FormatInt(time.Now().UnixNano(), 36)), &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Portal opens Stripe's own customer portal: cancel, change card, read
// invoices. It is the self-serve surface Ko-fi never had, and building an
// equivalent in the Hub would mean holding card details.
func (c *Client) Portal(ctx context.Context, customerID, returnURL string) (*Session, error) {
	if c == nil {
		return nil, ErrNotConfigured
	}
	if strings.TrimSpace(customerID) == "" {
		return nil, fmt.Errorf("stripe: this tenant has no Stripe customer yet")
	}
	f := url.Values{}
	f.Set("customer", customerID)
	f.Set("return_url", returnURL)
	var s Session
	if err := c.post(ctx, "/billing_portal/sessions", f, "", &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Subscription reads a subscription back. Used by the reconciler, which exists
// because a webhook can be missed and a customer should not have to notice.
func (c *Client) Subscription(ctx context.Context, id string) (status string, periodEnd time.Time, priceID string, err error) {
	if c == nil {
		return "", time.Time{}, "", ErrNotConfigured
	}
	var sub subscription
	if err := c.get(ctx, "/subscriptions/"+url.PathEscape(id), &sub); err != nil {
		return "", time.Time{}, "", err
	}
	return sub.Status, sub.periodEnd(), sub.priceID(), nil
}

// attemptBucket scopes an idempotency key to one ATTEMPT rather than to a
// tenant forever.
//
// A key of tenant+price alone looked safe and was not. Stripe remembers a key
// for 24 hours and refuses it if the parameters differ, so a customer who
// opened Checkout, wandered off and pressed Subscribe again the same day got
// either the first session back -- by then expired -- or a flat 400 that the
// Hub reported as "the payment provider could not be reached". The Subscribe
// button simply stopped working for the rest of the day, for that customer, on
// that plan.
//
// Fifteen minutes is chosen against what the key is actually for: a double
// click or a browser retry arrives within seconds and should get the SAME
// session back, while somebody genuinely starting again does so minutes later
// and should get a fresh one. It is also far inside Stripe's 24-hour session
// expiry, so a deduplicated response is never a dead link.
//
// Donations deliberately do not use this: an anonymous giver has no tenant, so
// a time bucket would be the whole key and two strangers donating in the same
// quarter hour would be handed each other's session.
func (c *Client) attemptBucket() string {
	return strconv.FormatInt(c.now().UTC().Truncate(15*time.Minute).Unix(), 36)
}

// idempotency builds a key so a retried create does not make a second object.
// Empty means "no key", which is right for reads and for the portal, where a
// second session is harmless.
func idempotency(parts ...string) string {
	var out []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	if len(out) < 2 {
		return ""
	}
	return "meshsat-" + strings.Join(out, "-")
}

func (c *Client) post(ctx context.Context, path string, form url.Values, idemKey string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	return c.do(req, path, out)
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	return c.do(req, path, out)
}

func (c *Client) do(req *http.Request, op string, out any) error {
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Stripe-Version", apiVersion)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("stripe: %s: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Bounded: a refusal body is small, and an unbounded read from a remote
	// service is a way to be handed a very large one.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("stripe: %s: reading the response: %w", op, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &Error{Status: resp.StatusCode, Op: op, Body: strings.TrimSpace(string(body))}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("stripe: %s: unreadable response: %w", op, err)
	}
	return nil
}

// apiVersion is pinned so Stripe cannot change a payload shape underneath a
// running Hub. Raising it is a deliberate act with a changelog to read first.
const apiVersion = "2025-08-27.basil"

// uiModeByVersion is Stripe's spelling of the embedded Checkout mode at each API
// version. Stripe renamed the value and the versions REFUSE each other, which
// makes this the sharpest edge on the pin. Verified against the live API on
// 2026-09-11, all four combinations:
//
//	2025-08-27.basil   ui_mode=embedded      OK
//	                   ui_mode=embedded_page "you must upgrade"
//	2026-08-26.dahlia  ui_mode=embedded      "no longer supported, use embedded_page"
//	                   ui_mode=embedded_page OK
//
// Deriving it means raising apiVersion no longer silently breaks the donation
// page: add the new version here and the page keeps working. A comment and a
// test would only have told somebody AFTER they broke it.
var uiModeByVersion = map[string]string{
	"2025-08-27.basil":  "embedded",
	"2026-08-26.dahlia": "embedded_page",
}

// embeddedUIMode returns the spelling the pinned version accepts.
//
// An unmapped version falls forward to the newer spelling and says so: Stripe
// moves in one direction, so the newer name is the better guess, and either
// wrong answer fails loudly with a 400 from Stripe rather than quietly. The
// donation page degrades to hosted Checkout on that error, so a donor still
// meets something payable.
func embeddedUIMode() string {
	if v, ok := uiModeByVersion[apiVersion]; ok {
		return v
	}
	slog.Warn("stripe: no embedded ui_mode recorded for the pinned API version; "+
		"guessing the newer spelling. If donations start failing, this is why -- "+
		"add the version to uiModeByVersion.",
		"api_version", apiVersion, "using", "embedded_page")
	return "embedded_page"
}

// tierLabel is the plan a customer is buying, phrased for the checkout page.
func tierLabel(priceID, plan string) string {
	if p := strings.TrimSpace(plan); p != "" {
		return strings.ToUpper(p[:1]) + p[1:]
	}
	return "subscription"
}
