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
	EventChargeRefunded      = "charge.refunded"
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
	ID                string            `json:"id"`
	Customer          string            `json:"customer"`
	Status            string            `json:"status"`
	CurrentPeriodEnd  int64             `json:"current_period_end"`
	CancelAtPeriodEnd bool              `json:"cancel_at_period_end"`
	Metadata          map[string]string `json:"metadata"`
	Items             struct {
		Data []struct {
			Price struct {
				ID string `json:"id"`
			} `json:"price"`
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

func (s subscription) periodEnd() time.Time {
	if s.CurrentPeriodEnd == 0 {
		return time.Time{}
	}
	return time.Unix(s.CurrentPeriodEnd, 0).UTC()
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
	ID              string `json:"id"`
	Customer        string `json:"customer"`
	Subscription    string `json:"subscription"`
	AmountPaid      int64  `json:"amount_paid"`
	Currency        string `json:"currency"`
	CustomerEmail   string `json:"customer_email"`
	CustomerName    string `json:"customer_name"`
	Charge          string `json:"charge"`
	Created         int64  `json:"created"`
	CustomerAddress struct {
		Country string `json:"country"`
	} `json:"customer_address"`
	Lines struct {
		Data []struct {
			Price struct {
				ID string `json:"id"`
			} `json:"price"`
		} `json:"data"`
	} `json:"lines"`
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
	return i.Lines.Data[0].Price.ID
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
