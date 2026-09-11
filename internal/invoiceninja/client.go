// Package invoiceninja issues a customer receipt through a self-hosted
// Invoice Ninja company (MESHSAT-998).
//
// The Hub is not a bookkeeping system and this package does not try to make it
// one. Invoice Ninja holds the document, the number series and the VAT
// arithmetic; this is the thin client that hands it a paid subscription and
// gets back an invoice number.
//
// Four things about the target company shape this code, all of them settled by
// issuing a receipt by hand first:
//
//   - The company has inclusive_taxes on, so the amount sent is the GROSS the
//     customer paid and the VAT is derived out of it, never added to it.
//   - The company default tax rate is a web-UI prefill only, so an invoice
//     created through the API must carry tax_name1 and tax_rate1 itself, and a
//     due_date, or it silently gets neither.
//   - Numbers are assigned on mark_sent, not on save, so a draft that is never
//     sent leaves no gap in the series.
//   - Recording the payment is what emails the receipt, because the company
//     has client_manual_payment_notification on. One customer email per
//     payment, carrying the PDF.
//
// Every step is resumable. A receipt is money that was already taken, so the
// caller retries until it succeeds, and a retry must never produce a second
// invoice or a second payment -- hence ExistingInvoiceID, and hence the
// balance check before paying.
package invoiceninja

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxResponse caps what is read from the billing system. It is our own host,
// but an unbounded read from anything is how a stuck upstream becomes an OOM.
const maxResponse = 4 << 20

// Client talks to one Invoice Ninja company. The token selects the company:
// each company has its own, so pointing this at the wrong token would issue
// consumer receipts out of the wrong brand and number series.
type Client struct {
	baseURL string
	token   string
	hc      *http.Client

	// TaxName and TaxRate go on every line, because the company default is a
	// UI prefill the API does not apply.
	TaxName string
	TaxRate float64
	// CountryID is the numeric country a new customer record is created with
	// (528 = the Netherlands).
	CountryID string
	// Currency is the only currency this company invoices in. A payment in
	// anything else is refused rather than converted.
	Currency string
}

// New returns a client for the company the token belongs to.
func New(baseURL, token string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		token:     token,
		hc:        &http.Client{Timeout: timeout},
		TaxName:   "BTW 21",
		TaxRate:   21,
		CountryID: "528",
		Currency:  "EUR",
	}
}

// SetHTTPClient replaces the transport, for tests.
func (c *Client) SetHTTPClient(h *http.Client) { c.hc = h }

// Request is one paid subscription that owes the customer a document.
type Request struct {
	// CustomerRef is a stable id for the customer in the calling system. It
	// goes on the Invoice Ninja client record as its id_number, so a person
	// reconciling the two can see which tenant a document belongs to.
	CustomerRef string
	// Name is what the customer is called on the document.
	Name string
	// Email is where the receipt goes, and the key a customer record is
	// looked up by.
	Email string
	// CountryCode is where the buyer is, ISO 3166-1 alpha-2. Empty means
	// unknown, and the customer record falls back to the company's own country
	// as it always did. Whether Dutch VAT applies at all is decided upstream in
	// internal/vat; this only makes the customer record truthful instead of
	// stamping every buyer Dutch (MESHSAT-1016).
	CountryCode string
	// AmountCents is the GROSS amount paid, in minor units. The company
	// derives the VAT out of it.
	AmountCents int64
	Currency    string
	// ProductKey and Description are the invoice line.
	ProductKey  string
	Description string
	// TaxExempt issues the line with no tax at all, overriding the company's
	// rate. It exists for money that is outside the scope of VAT rather than
	// zero-rated or exempt within it -- a donation with no counter-performance
	// (internal/vat.ForDonation). The company has inclusive_taxes on, so
	// leaving the rate in place would carve 21% out of a gift and put tax on a
	// document that owes none.
	TaxExempt bool
	// PaidAt dates both the invoice and the payment.
	PaidAt time.Time
	// Reference is the payment provider's transaction id, recorded on the
	// payment so a bank line can be traced to a document.
	Reference string

	// ExistingInvoiceID resumes a receipt whose invoice was created by an
	// earlier attempt that then failed. Without it a retry would create a
	// second invoice and burn a number from the series.
	ExistingInvoiceID string
	// OnInvoiceCreated is called with the new invoice id the moment it
	// exists, before anything else is attempted. The caller persists it so a
	// crash in the next step cannot orphan the invoice. A non-nil error from
	// it aborts before the invoice is sent or paid.
	OnInvoiceCreated func(invoiceID string) error
}

// Result is the document that was issued.
type Result struct {
	InvoiceID     string
	InvoiceNumber string
}

// Error is a non-2xx answer from the billing system.
type Error struct {
	Status int
	Op     string
	Body   string
}

func (e *Error) Error() string {
	return fmt.Sprintf("invoiceninja: %s: HTTP %d: %s", e.Op, e.Status, e.Body)
}

// Retryable reports whether trying the same call again could succeed. A 4xx
// other than 429 means the request itself is wrong, and repeating it just
// produces the same answer at the same rate.
func (e *Error) Retryable() bool {
	return e.Status >= 500 || e.Status == http.StatusTooManyRequests || e.Status == http.StatusRequestTimeout
}

// ErrNotConfigured is returned when the client has no URL or token.
var ErrNotConfigured = errors.New("invoiceninja: base URL or token is not configured")

// ErrWrongCurrency is returned for a payment in a currency this company does
// not invoice in. Never converted: a made-up exchange rate on a tax document
// is worse than no document.
var ErrWrongCurrency = errors.New("invoiceninja: payment currency does not match the company currency")

// IssueReceipt creates, numbers, pays and mails one receipt, and returns the
// invoice number. Safe to call again after any failure.
func (c *Client) IssueReceipt(ctx context.Context, req Request) (*Result, error) {
	if c.baseURL == "" || c.token == "" {
		return nil, ErrNotConfigured
	}
	if req.Currency != "" && !strings.EqualFold(req.Currency, c.Currency) {
		return nil, fmt.Errorf("%w: got %s, company invoices in %s", ErrWrongCurrency, req.Currency, c.Currency)
	}
	if req.Email == "" {
		return nil, errors.New("invoiceninja: no email address to send the receipt to")
	}
	if req.AmountCents <= 0 {
		return nil, fmt.Errorf("%w: %s", ErrBadAmount, amount(req.AmountCents))
	}

	inv, err := c.resumeOrCreate(ctx, req)
	if err != nil {
		return nil, err
	}

	// A number is assigned on send, so an invoice still in draft has none.
	if inv.StatusID == statusDraft {
		sent, err := c.markSent(ctx, inv.ID)
		if err != nil {
			return nil, err
		}
		inv = sent
	}

	// Paying is the only step that is not naturally idempotent, so it is
	// gated on the invoice still being owed. A second payment against a
	// settled invoice would create a credit balance out of nothing.
	if inv.Balance > 0 {
		if err := c.recordPayment(ctx, inv.ClientID, inv.ID, req); err != nil {
			return nil, err
		}
	}
	return &Result{InvoiceID: inv.ID, InvoiceNumber: inv.Number}, nil
}

// Invoice Ninja invoice status ids.
const (
	statusDraft = 1
	statusSent  = 2
)

type invoice struct {
	ID       string  `json:"id"`
	Number   string  `json:"number"`
	ClientID string  `json:"client_id"`
	StatusID int     `json:"status_id"`
	Balance  float64 `json:"balance"`
}

// UnmarshalJSON tolerates status_id and balance arriving as strings, which the
// v5 API does for some fields depending on the endpoint.
func (i *invoice) UnmarshalJSON(b []byte) error {
	type raw struct {
		ID       string      `json:"id"`
		Number   string      `json:"number"`
		ClientID string      `json:"client_id"`
		StatusID json.Number `json:"status_id"`
		Balance  json.Number `json:"balance"`
	}
	var r raw
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	i.ID, i.Number, i.ClientID = r.ID, r.Number, r.ClientID
	if n, err := r.StatusID.Int64(); err == nil {
		i.StatusID = int(n)
	}
	if f, err := r.Balance.Float64(); err == nil {
		i.Balance = f
	}
	return nil
}

// resumeOrCreate returns the invoice for this receipt: the one an earlier
// attempt created, or a new one.
func (c *Client) resumeOrCreate(ctx context.Context, req Request) (*invoice, error) {
	if req.ExistingInvoiceID != "" {
		var out struct {
			Data invoice `json:"data"`
		}
		err := c.do(ctx, http.MethodGet, "/invoices/"+url.PathEscape(req.ExistingInvoiceID), nil, &out, "get invoice")
		if err == nil {
			return &out.Data, nil
		}
		// A 404 means the invoice was deleted behind our back. Fall through
		// and create a new one rather than never issuing the receipt.
		var apiErr *Error
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
			return nil, err
		}
	}

	clientID, err := c.ensureCustomer(ctx, req)
	if err != nil {
		return nil, err
	}
	date := req.PaidAt.UTC().Format("2006-01-02")
	taxName, taxRate := c.TaxName, c.TaxRate
	if req.TaxExempt {
		taxName, taxRate = "", 0
	}
	body := map[string]any{
		"client_id": clientID,
		"date":      date,
		// Due on issue: it is already paid. Without this the invoice gets no
		// due date at all, because payment terms are a UI prefill.
		"due_date": date,
		"line_items": []map[string]any{{
			"product_key": req.ProductKey,
			"notes":       req.Description,
			// GROSS. The company has inclusive_taxes on and derives the VAT
			// out of this figure; adding tax on top would overcharge.
			"cost":      amount(req.AmountCents),
			"quantity":  1,
			"tax_name1": taxName,
			"tax_rate1": taxRate,
		}},
	}
	var out struct {
		Data invoice `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, "/invoices", body, &out, "create invoice"); err != nil {
		return nil, err
	}
	if out.Data.ID == "" {
		return nil, errors.New("invoiceninja: invoice created without an id")
	}
	if req.OnInvoiceCreated != nil {
		if err := req.OnInvoiceCreated(out.Data.ID); err != nil {
			return nil, fmt.Errorf("invoiceninja: recording the new invoice failed, stopping before it is sent: %w", err)
		}
	}
	return &out.Data, nil
}

// ensureCustomer finds the customer by email or creates one.
//
// Email is the key because it is what the receipt is sent to and what a person
// searches by. If the customer's address changes, this creates a second record
// rather than merging: the id_number on both says they are the same tenant,
// and merging customer records automatically is not a decision code should
// make on its own.
func (c *Client) ensureCustomer(ctx context.Context, req Request) (string, error) {
	var found struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	q := "/clients?per_page=2&email=" + url.QueryEscape(req.Email)
	if err := c.do(ctx, http.MethodGet, q, nil, &found, "find customer"); err != nil {
		return "", err
	}
	if len(found.Data) > 0 && found.Data[0].ID != "" {
		return found.Data[0].ID, nil
	}

	name := req.Name
	if name == "" {
		name = req.Email
	}
	body := map[string]any{
		"name":       name,
		"id_number":  req.CustomerRef,
		"country_id": c.countryIDFor(req.CountryCode),
		"contacts": []map[string]any{{
			"email":      req.Email,
			"send_email": true,
		}},
	}
	var out struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, "/clients", body, &out, "create customer"); err != nil {
		return "", err
	}
	if out.Data.ID == "" {
		return "", errors.New("invoiceninja: customer created without an id")
	}
	return out.Data.ID, nil
}

// markSent assigns the invoice number. No email goes out here: the customer's
// one email is the receipt that follows the payment.
func (c *Client) markSent(ctx context.Context, invoiceID string) (*invoice, error) {
	var out struct {
		Data []invoice `json:"data"`
	}
	body := map[string]any{"action": "mark_sent", "ids": []string{invoiceID}}
	if err := c.do(ctx, http.MethodPost, "/invoices/bulk", body, &out, "mark sent"); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, errors.New("invoiceninja: mark_sent returned no invoice")
	}
	if out.Data[0].Number == "" {
		return nil, errors.New("invoiceninja: invoice was sent but has no number")
	}
	return &out.Data[0], nil
}

// recordPayment settles the invoice, which is what mails the receipt.
func (c *Client) recordPayment(ctx context.Context, clientID, invoiceID string, req Request) error {
	body := map[string]any{
		"client_id":             clientID,
		"amount":                amount(req.AmountCents),
		"date":                  req.PaidAt.UTC().Format("2006-01-02"),
		"transaction_reference": req.Reference,
		"invoices": []map[string]any{{
			"invoice_id": invoiceID,
			"amount":     amount(req.AmountCents),
		}},
		// Explicit rather than relying on the company default, so a settings
		// change in the UI cannot silently stop customers being sent receipts.
		"email_receipt": "true",
	}
	return c.do(ctx, http.MethodPost, "/payments", body, nil, "record payment")
}

func (c *Client) do(ctx context.Context, method, path string, body, out any, op string) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("invoiceninja: %s: %w", op, err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+"/api/v1"+path, rdr)
	if err != nil {
		return fmt.Errorf("invoiceninja: %s: %w", op, err)
	}
	// The token is a company-scoped API key. It goes in a header and never
	// into a log line, a URL or an error message.
	req.Header.Set("X-API-TOKEN", c.token)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("invoiceninja: %s: %w", op, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponse))
		_ = resp.Body.Close()
	}()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return fmt.Errorf("invoiceninja: %s: reading response: %w", op, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &Error{Status: resp.StatusCode, Op: op, Body: snippet(raw)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("invoiceninja: %s: decoding response: %w", op, err)
	}
	return nil
}

// snippet keeps an error message short enough to log.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// isoNumeric maps ISO 3166-1 alpha-2 to the numeric code Invoice Ninja uses for
// country_id. Only the EU VAT area is listed: a buyer outside it never reaches
// this code, because internal/vat parks the receipt before the billing system
// is touched at all.
var isoNumeric = map[string]string{
	"AT": "040", "BE": "056", "BG": "100", "HR": "191", "CY": "196", "CZ": "203",
	"DK": "208", "EE": "233", "FI": "246", "FR": "250", "DE": "276", "GR": "300",
	"EL": "300", "HU": "348", "IE": "372", "IT": "380", "LV": "428", "LT": "440",
	"LU": "442", "MT": "470", "NL": "528", "PL": "616", "PT": "620", "RO": "642",
	"SK": "703", "SI": "705", "ES": "724", "SE": "752",
}

// countryIDFor turns the buyer's country into Invoice Ninja's numeric id,
// falling back to the configured company country when it is unknown or not one
// this code recognises. The fallback is what every customer used to get
// unconditionally.
func (c *Client) countryIDFor(alpha2 string) string {
	if id, ok := isoNumeric[strings.ToUpper(strings.TrimSpace(alpha2))]; ok {
		return id
	}
	return c.CountryID
}

// CompanyState is what the billing system says about the company this token
// acts as. Both fields are assumptions this code would otherwise make silently,
// and both have been wrong in production at least once.
type CompanyState struct {
	Name string
	// InclusiveTaxes decides whether the amount sent is the gross the customer
	// paid or a net the company adds VAT to. The whole receipt path assumes
	// gross.
	InclusiveTaxes bool
	// EmailSendingMethod decides whether outbound mail carries this company's
	// own sender or the instance-wide one. See SenderIsPerCompany.
	EmailSendingMethod string
	ReplyToEmail       string
	// ShowCurrencyCode decides which branch of Number::formatMoney the document
	// takes: false renders "EUR 9,00", true renders "9,00 EUR". FormatMoney
	// implements the false branch, which is what this company is set to, so a
	// change here would silently put the Hub's email and the PDF back out of
	// step. Hence MoneyMatchesTheDocument and the startup check on it.
	ShowCurrencyCode bool
}

// MoneyMatchesTheDocument reports whether FormatMoney still renders amounts the
// way this company's invoices and credit notes render them.
func (s *CompanyState) MoneyMatchesTheDocument() bool { return !s.ShowCurrencyCode }

// SenderIsPerCompany reports whether this company's mail will actually go out
// under its own identity.
//
// On a self-hosted instance the per-company sender comes from
// NinjaMailerJob::setSelfHostMultiMailer(), which reads env vars prefixed with
// the company id. It is only reached through the switch's `default` arm, so the
// setting has to be the EMPTY STRING. 'default' is an explicit case that
// returns early with the instance-wide mailer, and it is the class default for
// a new company -- so a company is born on the wrong branch, and one click in
// the web UI puts it back there.
//
// The consequence is not subtle and it is not loud: every receipt for this
// company would go out under whichever business owns the instance-wide
// MAIL_FROM_ADDRESS, correctly DKIM-signed for that domain, with this company's
// branding still inside the message. That is exactly how credit notes behave
// today, and credit notes are the path the Hub had to take over (MESHSAT-1019).
func (s *CompanyState) SenderIsPerCompany() bool { return s.EmailSendingMethod == "" }

// VerifyInclusiveTaxes reports whether the acting company derives VAT out of
// the amount sent rather than adding it on top.
func (c *Client) VerifyInclusiveTaxes(ctx context.Context) (bool, error) {
	st, err := c.InspectCompany(ctx)
	if err != nil {
		return false, err
	}
	return st.InclusiveTaxes, nil
}

// InspectCompany asks the billing system about the company this token acts as.
//
// A nil error means the company answered; read the fields for what it said.
func (c *Client) InspectCompany(ctx context.Context) (*CompanyState, error) {
	if c == nil || c.baseURL == "" || c.token == "" {
		return nil, ErrNotConfigured
	}
	var out struct {
		Data []struct {
			ID       string          `json:"id"`
			Settings companySettings `json:"settings"`
		} `json:"data"`
	}
	// "/companies", not "/api/v1/companies": do() prepends the prefix. With it
	// doubled the URL is an unknown path, and Invoice Ninja answers those with
	// its web app at 200 rather than a 404 -- so the status check passed and it
	// failed on the first '<' instead, which is exactly what production logged.
	if err := c.do(ctx, http.MethodGet, "/companies", nil, &out, "inspect company"); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, errors.New("invoiceninja: the token sees no company")
	}

	// This list is the ACCOUNT's companies, not the token's. Taking the first
	// one read the other company on the instance, which has inclusive taxes off
	// -- so this guard reported on a company these receipts never touch. The
	// token IS company-scoped, but only per-company endpoints enforce it:
	// GET /companies/{id} answers 401 for any company but ours (MESHSAT-1019).
	if len(out.Data) == 1 {
		return out.Data[0].Settings.state(), nil
	}
	for _, co := range out.Data {
		if co.ID == "" {
			continue
		}
		var one struct {
			Data struct {
				Settings companySettings `json:"settings"`
			} `json:"data"`
		}
		err := c.do(ctx, http.MethodGet, "/companies/"+url.PathEscape(co.ID), nil, &one, "identify company")
		if err != nil {
			var apiErr *Error
			if errors.As(err, &apiErr) && (apiErr.Status == http.StatusUnauthorized ||
				apiErr.Status == http.StatusForbidden || apiErr.Status == http.StatusNotFound) {
				continue // somebody else's company
			}
			return nil, err
		}
		return one.Data.Settings.state(), nil
	}
	// Refusing rather than guessing. A wrong answer here is worse than none:
	// a false all-clear lets every receipt add VAT on top of a price the
	// customer already paid.
	return nil, errors.New("invoiceninja: could not tell which company this token acts as")
}

type companySettings struct {
	Name               string `json:"name"`
	InclusiveTaxes     bool   `json:"inclusive_taxes"`
	EmailSendingMethod string `json:"email_sending_method"`
	ReplyToEmail       string `json:"reply_to_email"`
	ShowCurrencyCode   bool   `json:"show_currency_code"`
}

func (s companySettings) state() *CompanyState {
	return &CompanyState{
		Name:               s.Name,
		InclusiveTaxes:     s.InclusiveTaxes,
		EmailSendingMethod: s.EmailSendingMethod,
		ReplyToEmail:       s.ReplyToEmail,
		ShowCurrencyCode:   s.ShowCurrencyCode,
	}
}
