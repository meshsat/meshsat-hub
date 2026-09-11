package invoiceninja

// Credit notes: the document that reverses a sale (MESHSAT-1019).
//
// A receipt says money arrived. A credit note says some of it went back, and
// under Dutch VAT it is the instrument that reverses the VAT the invoice
// declared. It is not an edit of the invoice: the invoice has a number out of a
// gapless series and is locked once sent, so the correction is its own document
// in its own series (MSHCN{year}-{counter} on this company).
//
// The sequence below was settled by running it by hand against the live company
// first, because none of it is guessable from the API docs:
//
//  1. Create the credit. Like an invoice it is born a draft with no number.
//  2. mark_sent assigns the number, because counter_number_applied is when_sent.
//  3. Refund the payment. This is what puts the money movement in the books,
//     and it REOPENS the invoice balance -- verified, not assumed.
//  4. Apply the credit to the reopened invoice with a zero-amount payment that
//     draws on it. Without this the customer is left owing an invoice while
//     holding a credit they have already been paid out in cash.
//
// Every step is gated on state the billing system reports, so a retry after any
// failure resumes rather than repeats. That matters more here than for a
// receipt: a second credit note is a second legal document for money that only
// went back once.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CreditRequest is one refund that owes the customer a credit note.
type CreditRequest struct {
	// InvoiceID is the invoice being credited, as the billing system knows it.
	// It comes from the receipt row, which recorded it when the receipt was
	// issued.
	InvoiceID string
	// AmountCents is the GROSS amount given back, in minor units. It may be
	// less than the invoice; the company derives the VAT out of it either way.
	AmountCents int64
	Currency    string
	// ProductKey and Description are the credit line. The description names
	// the original invoice, because a credit note that does not say what it
	// corrects is not much of a correction.
	ProductKey  string
	Description string
	// RefundedAt dates the credit note: when the money actually went back.
	RefundedAt time.Time

	// ExistingCreditID resumes a refund whose credit note was created by an
	// earlier attempt that then failed.
	ExistingCreditID string
	// OnCreditCreated is called with the new credit id the moment it exists,
	// before it is numbered. The caller persists it so a crash in the next step
	// cannot orphan the document. A non-nil error aborts before it is sent.
	OnCreditCreated func(creditID string) error
}

// CreditResult is the document that was issued.
type CreditResult struct {
	CreditID     string
	CreditNumber string
	// InvoiceNumber is the invoice the credit note corrects, for the customer
	// notice and the audit line.
	InvoiceNumber string
}

// ErrInvoiceGone means the invoice this refund credits is no longer in the
// billing system. Unlike a missing invoice on the receipt path, this cannot be
// recovered by creating a new one: there is nothing to reverse, and inventing a
// credit note against an invoice that does not exist would be a fabricated
// document. A person has to look.
var ErrInvoiceGone = errors.New("invoiceninja: the invoice this refund credits no longer exists")

// IssueCreditNote creates, numbers and settles one credit note, and records the
// money going back out. Safe to call again after any failure.
func (c *Client) IssueCreditNote(ctx context.Context, req CreditRequest) (*CreditResult, error) {
	if c.baseURL == "" || c.token == "" {
		return nil, ErrNotConfigured
	}
	if req.InvoiceID == "" {
		return nil, errors.New("invoiceninja: no invoice to credit")
	}
	if req.Currency != "" && !strings.EqualFold(req.Currency, c.Currency) {
		return nil, fmt.Errorf("%w: got %s, company invoices in %s", ErrWrongCurrency, req.Currency, c.Currency)
	}
	if req.AmountCents <= 0 {
		return nil, fmt.Errorf("%w: %s", ErrBadAmount, amount(req.AmountCents))
	}

	inv, err := c.invoiceWithPayments(ctx, req.InvoiceID)
	if err != nil {
		return nil, err
	}
	if req.AmountCents > toCents(inv.Amount) {
		// Refusing rather than clamping. A figure larger than the invoice means
		// somebody typed the wrong thing, and a credit note for more than was
		// ever charged reclaims VAT that was never declared.
		return nil, fmt.Errorf("invoiceninja: refund of %s is more than invoice %s of %s",
			amount(req.AmountCents), inv.Number, amount(toCents(inv.Amount)))
	}

	credit, err := c.resumeOrCreateCredit(ctx, req, inv)
	if err != nil {
		return nil, err
	}
	if credit.StatusID == statusDraft {
		sent, err := c.markCreditSent(ctx, credit.ID)
		if err != nil {
			return nil, err
		}
		credit = sent
	}

	// The money going back out. Gated on what the payment itself reports, so a
	// retry after a later step failed does not refund twice -- the billing
	// system refuses that with a 422, but relying on being refused is not a
	// design.
	if p := refundablePayment(inv, req.AmountCents); p != nil {
		if err := c.refundPayment(ctx, p.ID, req, inv.ID); err != nil {
			return nil, err
		}
		// Re-read: refunding reopens the invoice balance, and the next step
		// needs the new figure rather than the one from before the refund.
		if inv, err = c.invoiceWithPayments(ctx, req.InvoiceID); err != nil {
			return nil, err
		}
	}

	// Settle the reopened invoice with the credit. Both sides are checked, so
	// a resumed attempt that already applied it does nothing.
	if inv.Balance > 0 && credit.Balance > 0 {
		if err := c.applyCredit(ctx, inv, credit, req); err != nil {
			return nil, err
		}
	}

	return &CreditResult{CreditID: credit.ID, CreditNumber: credit.Number, InvoiceNumber: inv.Number}, nil
}

// invoiceEnvelope is an invoice plus the payments made against it.
//
// Balance and Amount arrive as JSON numbers and are held as float64, which is
// the one place this package tolerates one. They are only ever compared, never
// used to compute a figure that reaches a document: every amount written to the
// billing system comes from the caller's integer minor units.
type invoiceEnvelope struct {
	invoice
	Amount   float64
	Payments []payment
}

type payment struct {
	ID       string
	Amount   float64
	Refunded float64
}

// UnmarshalJSON tolerates the v5 API sending numbers as strings, exactly as the
// invoice decoder does.
func (e *invoiceEnvelope) UnmarshalJSON(b []byte) error {
	if err := json.Unmarshal(b, &e.invoice); err != nil {
		return err
	}
	var raw struct {
		Amount   json.Number `json:"amount"`
		Payments []struct {
			ID       string      `json:"id"`
			Amount   json.Number `json:"amount"`
			Refunded json.Number `json:"refunded"`
		} `json:"payments"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	e.Amount = number(raw.Amount)
	e.Payments = nil
	for _, p := range raw.Payments {
		e.Payments = append(e.Payments, payment{
			ID: p.ID, Amount: number(p.Amount), Refunded: number(p.Refunded),
		})
	}
	return nil
}

func number(n json.Number) float64 {
	f, err := n.Float64()
	if err != nil {
		return 0
	}
	return f
}

// toCents turns a figure the billing system reported into minor units. Rounding
// is to the nearest cent because the value came back through JSON as a decimal
// number; it is used for gates ("is there anything left to refund?"), never to
// decide what a document says.
func toCents(f float64) int64 { return int64(math.Round(f * minorUnits)) }

func (c *Client) invoiceWithPayments(ctx context.Context, id string) (*invoiceEnvelope, error) {
	var out struct {
		Data invoiceEnvelope `json:"data"`
	}
	path := "/invoices/" + url.PathEscape(id) + "?include=payments"
	if err := c.do(ctx, http.MethodGet, path, nil, &out, "get invoice for credit"); err != nil {
		var apiErr *Error
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return nil, ErrInvoiceGone
		}
		return nil, err
	}
	if out.Data.ID == "" {
		return nil, ErrInvoiceGone
	}
	return &out.Data, nil
}

// refundablePayment picks the payment to take the money back out of: the first
// with enough left un-refunded. Returns nil when there is none, which is a
// normal outcome on a resumed attempt.
func refundablePayment(inv *invoiceEnvelope, cents int64) *payment {
	for i := range inv.Payments {
		p := &inv.Payments[i]
		if toCents(p.Amount-p.Refunded) >= cents {
			return p
		}
	}
	return nil
}

func (c *Client) resumeOrCreateCredit(ctx context.Context, req CreditRequest, inv *invoiceEnvelope) (*invoice, error) {
	if req.ExistingCreditID != "" {
		var out struct {
			Data invoice `json:"data"`
		}
		err := c.do(ctx, http.MethodGet, "/credits/"+url.PathEscape(req.ExistingCreditID), nil, &out, "get credit")
		if err == nil {
			return &out.Data, nil
		}
		var apiErr *Error
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
			return nil, err
		}
		// A 404 means it was deleted behind our back. Fall through and create
		// one, rather than never producing the document.
	}

	date := req.RefundedAt.UTC().Format("2006-01-02")
	notes := req.Description
	if inv.Number != "" {
		notes = "Refund of invoice " + inv.Number + " -- " + req.Description
	}
	body := map[string]any{
		"client_id": inv.ClientID,
		"date":      date,
		"line_items": []map[string]any{{
			"product_key": req.ProductKey,
			"notes":       notes,
			// GROSS, exactly as on the invoice. The company has inclusive taxes
			// on, so this figure carries its own VAT and the credit note
			// reverses the same split the invoice charged.
			"cost":      amount(req.AmountCents),
			"quantity":  1,
			"tax_name1": c.TaxName,
			"tax_rate1": c.TaxRate,
		}},
	}
	var out struct {
		Data invoice `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, "/credits", body, &out, "create credit"); err != nil {
		return nil, err
	}
	if out.Data.ID == "" {
		return nil, errors.New("invoiceninja: credit created without an id")
	}
	if req.OnCreditCreated != nil {
		if err := req.OnCreditCreated(out.Data.ID); err != nil {
			return nil, fmt.Errorf("invoiceninja: recording the new credit note failed, stopping before it is sent: %w", err)
		}
	}
	return &out.Data, nil
}

// markCreditSent assigns the credit note number.
func (c *Client) markCreditSent(ctx context.Context, creditID string) (*invoice, error) {
	var out struct {
		Data []invoice `json:"data"`
	}
	body := map[string]any{"action": "mark_sent", "ids": []string{creditID}}
	if err := c.do(ctx, http.MethodPost, "/credits/bulk", body, &out, "mark credit sent"); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, errors.New("invoiceninja: mark_sent returned no credit note")
	}
	if out.Data[0].Number == "" {
		return nil, errors.New("invoiceninja: credit note was sent but has no number")
	}
	return &out.Data[0], nil
}

// refundPayment records the money going back. gateway_refund is false because
// the money moves in the payment processor by hand, not through a gateway this
// billing system holds credentials for -- asking it to call one would fail, and
// worse, could take the money back twice if one were ever configured.
//
// send_email is false as well: the customer's notice comes from the Hub with
// the credit note attached, because this company's credit mail goes out under
// the other company's sender (see the notes in internal/refunds).
func (c *Client) refundPayment(ctx context.Context, paymentID string, req CreditRequest, invoiceID string) error {
	body := map[string]any{
		"id":     paymentID,
		"amount": amount(req.AmountCents),
		"date":   req.RefundedAt.UTC().Format("2006-01-02"),
		"invoices": []map[string]any{{
			"invoice_id": invoiceID,
			"amount":     amount(req.AmountCents),
		}},
		"gateway_refund": false,
		"send_email":     false,
	}
	return c.do(ctx, http.MethodPost, "/payments/refund", body, nil, "refund payment")
}

// applyCredit settles the reopened invoice out of the credit note. It is a
// payment of zero: no money moves, the credit is what pays.
func (c *Client) applyCredit(ctx context.Context, inv *invoiceEnvelope, credit *invoice, req CreditRequest) error {
	// Never apply more than either side has left.
	cents := req.AmountCents
	if b := toCents(inv.Balance); b < cents {
		cents = b
	}
	if b := toCents(credit.Balance); b < cents {
		cents = b
	}
	if cents <= 0 {
		return nil
	}
	body := map[string]any{
		"client_id": inv.ClientID,
		"amount":    0,
		"date":      req.RefundedAt.UTC().Format("2006-01-02"),
		"credits":   []map[string]any{{"credit_id": credit.ID, "amount": amount(cents)}},
		"invoices":  []map[string]any{{"invoice_id": inv.ID, "amount": amount(cents)}},
		// This one must not mail: it is a bookkeeping entry, not a receipt.
		"email_receipt": "false",
	}
	return c.do(ctx, http.MethodPost, "/payments", body, nil, "apply credit")
}

// CreditPDF fetches the rendered credit note. The Hub attaches it to the notice
// it sends the customer.
func (c *Client) CreditPDF(ctx context.Context, creditID string) ([]byte, error) {
	return c.documentPDF(ctx, "credits", creditID, "credit pdf")
}

// documentPDF downloads one rendered document. Shared by CreditPDF and
// InvoicePDF: both send the customer a PDF the Hub fetched itself, and the
// checks below are the same for either.
func (c *Client) documentPDF(ctx context.Context, collection, id, op string) ([]byte, error) {
	if c.baseURL == "" || c.token == "" {
		return nil, ErrNotConfigured
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/api/v1/"+collection+"/"+url.PathEscape(id)+"/download", nil)
	if err != nil {
		return nil, fmt.Errorf("invoiceninja: %s: %w", op, err)
	}
	req.Header.Set("X-API-TOKEN", c.token)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("invoiceninja: %s: %w", op, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxPDF))
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPDF))
	if err != nil {
		return nil, fmt.Errorf("invoiceninja: %s: reading: %w", op, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &Error{Status: resp.StatusCode, Op: op, Body: snippet(body)}
	}
	// This host answers an unknown path with its web application at 200, so a
	// wrong URL comes back as HTML rather than an error. Check what arrived.
	if !strings.HasPrefix(string(body), "%PDF") {
		return nil, fmt.Errorf("invoiceninja: %s: the response is not a PDF (%d bytes)", op, len(body))
	}
	return body, nil
}

// maxPDF caps a rendered document. A credit note is one page; anything near
// this is a bug upstream, and an unbounded read of it would be ours.
const maxPDF = 16 << 20
