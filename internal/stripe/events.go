package stripe

import (
	"encoding/json"
	"strings"
	"time"
)

// The subset of Stripe's event JSON this reads.
//
// Deliberately partial. Stripe's objects carry a great deal that is none of the
// Hub's business, and decoding only what is acted on means a change to the rest
// of the shape cannot break the Hub. Everything here is read after the
// signature has already verified the raw bytes.

// Event is the envelope every delivery arrives in.
type Event struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
	Created int64 `json:"created"`
}

// The event types this acts on. Anything else is acknowledged and ignored:
// answering 2xx to an event we do not handle is correct, because Stripe would
// otherwise retry it for three days.
const (
	EventCheckoutCompleted   = "checkout.session.completed"
	EventSubscriptionCreated = "customer.subscription.created"
	EventSubscriptionUpdated = "customer.subscription.updated"
	EventSubscriptionDeleted = "customer.subscription.deleted"
	EventInvoicePaid         = "invoice.paid"
	EventInvoiceFailed       = "invoice.payment_failed"
	EventChargeRefunded      = "charge.refunded"
	// EventInvoicePaymentPaid is the only place Stripe still joins an invoice
	// to the payment that settled it. charge.invoice is gone, and neither the
	// charge nor the payment intent points back at an invoice, so without this
	// a refunded subscription matches no receipt and issues no credit note.
	EventInvoicePaymentPaid = "invoice_payment.paid"
)

// checkoutSession is data.object for checkout.session.completed.
// MetadataKind is the metadata key the Hub stamps on a session to say what it
// is FOR, and KindDonation is its one value today.
//
// It exists so a session with no tenant can be told apart from a mistake. The
// Hub creates every Checkout session it will ever be asked about, so a session
// carrying neither a tenant nor this marker is genuinely unaccounted for and
// belongs in the unattributed list. Without the marker an anonymous donation
// and a misrouted payment are the same event, and one of them would have to be
// guessed at.
const (
	MetadataKind = "kind"
	KindDonation = "donation"
)

type checkoutSession struct {
	ID              string            `json:"id"`
	Customer        string            `json:"customer"`
	Subscription    string            `json:"subscription"`
	PaymentIntent   string            `json:"payment_intent"`
	Mode            string            `json:"mode"`
	AmountTotal     int64             `json:"amount_total"`
	Currency        string            `json:"currency"`
	CustomerEmail   string            `json:"customer_email"`
	Metadata        map[string]string `json:"metadata"`
	CustomerDetails struct {
		Email   string `json:"email"`
		Name    string `json:"name"`
		Address struct {
			Country string `json:"country"`
		} `json:"address"`
	} `json:"customer_details"`
}

// email prefers the address Stripe collected at checkout over the one we
// suggested, because the customer may have corrected it.
func (s checkoutSession) email() string {
	if e := strings.TrimSpace(s.CustomerDetails.Email); e != "" {
		return e
	}
	return strings.TrimSpace(s.CustomerEmail)
}

// country is the buyer's, collected at checkout. This is the field Ko-fi never
// sent, and its absence is why every donation from a stranger parked.
func (s checkoutSession) country() string {
	return strings.ToUpper(strings.TrimSpace(s.CustomerDetails.Address.Country))
}

// subscription is data.object for the customer.subscription.* events.
type subscription struct {
	ID       string `json:"id"`
	Customer string `json:"customer"`
	Status   string `json:"status"`
	// CurrentPeriodEnd was top level until Stripe moved the billing period
	// onto the items in the basil API versions, which is what this endpoint is
	// pinned to. Kept for an event replayed at an older version; read
	// periodEnd(), never this.
	CurrentPeriodEnd  int64             `json:"current_period_end"`
	CancelAtPeriodEnd bool              `json:"cancel_at_period_end"`
	Metadata          map[string]string `json:"metadata"`
	Items             struct {
		Data []struct {
			Price struct {
				ID string `json:"id"`
			} `json:"price"`
			// Where the period actually lives now.
			CurrentPeriodEnd int64 `json:"current_period_end"`
		} `json:"data"`
	} `json:"items"`
}

// priceID is the first item's price. The Hub sells one line per subscription;
// a second would be a product decision nobody has made, and picking one
// silently would be a guess about money.
func (s subscription) priceID() string {
	if len(s.Items.Data) == 0 {
		return ""
	}
	return s.Items.Data[0].Price.ID
}

// periodEnd is when Stripe says this period ends, in whichever shape the event
// arrived in.
//
// Getting this wrong is quiet rather than loud: onSubscription falls back to
// now + billing.Period when it is zero, so a plan still works and still
// expires -- just on a date the Hub invented rather than the one Stripe
// charges on. The first real subscription was granted "until 2026-10-16"
// against a true period end of 2026-10-11, because the top-level field had
// moved onto the items and nothing noticed. Everything downstream of the
// expiry -- the lapse job, the warning email, the date shown to the customer
// -- was working from that invented date.
func (s subscription) periodEnd() time.Time {
	end := s.CurrentPeriodEnd
	if len(s.Items.Data) > 0 && s.Items.Data[0].CurrentPeriodEnd != 0 {
		end = s.Items.Data[0].CurrentPeriodEnd
	}
	if end == 0 {
		return time.Time{}
	}
	return time.Unix(end, 0).UTC()
}

// live reports whether this subscription should be granting a plan right now.
// past_due and unpaid deliberately still count: the customer's card failed and
// Stripe is retrying, and taking a fleet's ceiling away mid-retry would be a
// worse answer than waiting for Stripe to give up and send the deleted event.
func (s subscription) live() bool {
	switch s.Status {
	case "active", "trialing", "past_due", "unpaid":
		return true
	}
	return false
}

// invoice is data.object for invoice.paid. This is the document trigger: an
// invoice is paid once per period, which is exactly when a receipt is owed.
type invoice struct {
	ID       string `json:"id"`
	Customer string `json:"customer"`
	// Subscription was top level until Stripe API 2026-08-26 (dahlia), which
	// moved it under parent.subscription_details. Kept so an event from an
	// older API version still parses; read subscriptionID(), never this.
	Subscription string `json:"subscription"`
	// Parent is where dahlia put the subscription AND the metadata this Hub
	// set on it. That metadata is what binds an invoice to a tenant, and not
	// reading it is what left the first real subscription payment with no
	// document: onInvoicePaid could only fall back to the customer binding,
	// which on a FIRST subscription does not exist yet -- Stripe sends
	// invoice.paid before checkout.session.completed, so the binding is
	// written a second after the invoice needed it. Every first-time
	// subscriber would have lost their first VAT document that way.
	Parent struct {
		SubscriptionDetails struct {
			Subscription string            `json:"subscription"`
			Metadata     map[string]string `json:"metadata"`
		} `json:"subscription_details"`
	} `json:"parent"`
	AmountPaid   int64 `json:"amount_paid"`
	AmountDue    int64 `json:"amount_due"`
	AttemptCount int   `json:"attempt_count"`
	// NextPaymentAttempt is unix seconds, or 0 when Stripe has given up
	// retrying. Zero is the interesting case: it means this is the last word
	// on the payment, not a step on the way.
	NextPaymentAttempt int64  `json:"next_payment_attempt"`
	Currency           string `json:"currency"`
	CustomerEmail      string `json:"customer_email"`
	CustomerName       string `json:"customer_name"`
	Charge             string `json:"charge"`
	Created            int64  `json:"created"`
	CustomerAddress    struct {
		Country string `json:"country"`
	} `json:"customer_address"`
	Lines struct {
		Data []struct {
			// Price is the pre-dahlia shape; Pricing is where the same price
			// id lives from 2026-08-26 onwards. Read priceID(), not either.
			Price struct {
				ID string `json:"id"`
			} `json:"price"`
			Pricing struct {
				PriceDetails struct {
					Price string `json:"price"`
				} `json:"price_details"`
			} `json:"pricing"`
		} `json:"data"`
	} `json:"lines"`
}

// metadata is the tenant-bearing metadata this Hub set on the subscription,
// which Stripe copies onto every invoice raised for it.
//
// It is trustworthy for the same reason the Checkout session's metadata is:
// the signature is verified over the raw body before any of this is decoded,
// and the value was written by this Hub when it created the session. A
// customer cannot set it.
func (i invoice) metadata() map[string]string {
	return i.Parent.SubscriptionDetails.Metadata
}

// subscriptionID reads whichever shape this event arrived in.
func (i invoice) subscriptionID() string {
	if s := i.Parent.SubscriptionDetails.Subscription; s != "" {
		return s
	}
	return i.Subscription
}

func (i invoice) country() string {
	return strings.ToUpper(strings.TrimSpace(i.CustomerAddress.Country))
}

func (i invoice) paidAt() time.Time {
	if i.Created == 0 {
		return time.Now().UTC()
	}
	return time.Unix(i.Created, 0).UTC()
}

func (i invoice) priceID() string {
	if len(i.Lines.Data) == 0 {
		return ""
	}
	l := i.Lines.Data[0]
	if p := strings.TrimSpace(l.Pricing.PriceDetails.Price); p != "" {
		return p
	}
	return l.Price.ID
}

// invoicePayment is data.object for invoice_payment.paid. It exists to carry
// one fact: which payment settled which invoice.
type invoicePayment struct {
	ID      string `json:"id"`
	Invoice string `json:"invoice"`
	Status  string `json:"status"`
	Payment struct {
		PaymentIntent string `json:"payment_intent"`
	} `json:"payment"`
}

// charge is data.object for charge.refunded. Stripe sends the whole charge with
// its running refunded total, not the delta, so a partial refund followed by
// another arrives as two events with a growing AmountRefunded.
type charge struct {
	ID             string `json:"id"`
	Invoice        string `json:"invoice"`
	PaymentIntent  string `json:"payment_intent"`
	Customer       string `json:"customer"`
	Amount         int64  `json:"amount"`
	AmountRefunded int64  `json:"amount_refunded"`
	Currency       string `json:"currency"`
	Refunded       bool   `json:"refunded"`
}
