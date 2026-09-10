package stripe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
}

// NewClient returns a client, or nil when no key is configured.
func NewClient(secretKey string, timeout time.Duration) *Client {
	if strings.TrimSpace(secretKey) == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return &Client{key: secretKey, http: &http.Client{Timeout: timeout}, baseURL: apiBase}
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
}

// CheckoutRequest starts a subscription.
type CheckoutRequest struct {
	TenantID   string
	Email      string
	PriceID    string
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
	if req.CustomerID != "" {
		f.Set("customer", req.CustomerID)
	} else if req.Email != "" {
		f.Set("customer_email", req.Email)
	}
	var s Session
	if err := c.post(ctx, "/checkout/sessions", f, idempotency("checkout", req.TenantID, req.PriceID), &s); err != nil {
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
	f.Set("success_url", req.SuccessURL)
	f.Set("cancel_url", req.CancelURL)
	f.Set("billing_address_collection", "required")
	f.Set("automatic_tax[enabled]", "false")
	if req.TenantID != "" {
		f.Set("metadata[tenant_id]", req.TenantID)
	}
	if req.Email != "" {
		f.Set("customer_email", req.Email)
	}
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
