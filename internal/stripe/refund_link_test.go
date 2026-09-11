package stripe

import (
	"fmt"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// A refunded subscription used to match no receipt, so no credit note was
// issued: the sale stayed in the books at full value with its VAT declared,
// and the only trace was a log line saying it had to be raised by hand.
//
// The cause is that Stripe removed charge.invoice, and nothing else joins the
// two: the payment intent has no invoice, and invoice_payments can only be
// filtered BY invoice -- the thing you are trying to find. The one place the
// join is still stated is invoice_payment.paid, at the moment the money
// arrives, so that is where it is recorded.
//
// The old test for this path asserted on a charge carrying "invoice", which is
// why it kept passing while production could not issue a credit note.

// refundBody is the charge.refunded Stripe actually sends now: a payment
// intent, a customer, and NO invoice.
func refundBody(id, pi string, cents int64) string {
	return fmt.Sprintf(`{"id":%q,"type":%q,"created":%d,"data":{"object":{
		"id":"ch_1","payment_intent":%q,"customer":"cus_1","amount":%d,
		"amount_refunded":%d,"currency":"eur","refunded":true}}}`,
		id, EventChargeRefunded, fixedNow.Unix(), pi, cents, cents)
}

func paymentPaidBody(id, invoice, pi string) string {
	return fmt.Sprintf(`{"id":%q,"type":%q,"created":%d,"data":{"object":{
		"id":"inpay_1","invoice":%q,"status":"paid","amount_paid":900,"currency":"eur",
		"payment":{"payment_intent":%q,"type":"payment_intent"}}}}`,
		id, EventInvoicePaymentPaid, fixedNow.Unix(), invoice, pi)
}

func issuedReceipt() store.Receipt {
	return store.Receipt{
		ID: "rcpt-1", TenantID: "t1", DeliveryKey: "stripe:inv:in_123",
		AmountCents: 900, Currency: "EUR", Country: "NL", Status: store.ReceiptIssued,
	}
}

func TestInvoicePaymentPaidRecordsThePaymentBehindTheInvoice(t *testing.T) {
	st := newTenants(store.Tenant{ID: "t1", StripeCustomerID: "cus_1"})
	h, rc, _ := newHandler(t, st)
	rc.rows = append(rc.rows, issuedReceipt())

	if w := deliver(t, h, paymentPaidBody("evt_ip", "in_123", "pi_abc")); w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if got := rc.rows[0].PaymentRef; got != "pi_abc" {
		t.Fatalf("payment_ref = %q, want the payment intent that settled the invoice", got)
	}
}

// The headline: a refund of a subscription, in the shape Stripe sends now.
func TestARefundedSubscriptionFindsItsReceiptAndIssuesACreditNote(t *testing.T) {
	st := newTenants(store.Tenant{ID: "t1", StripeCustomerID: "cus_1"})
	h, rc, rf := newHandler(t, st)
	rc.rows = append(rc.rows, issuedReceipt())

	// The money arrives; the link is recorded.
	if w := deliver(t, h, paymentPaidBody("evt_ip", "in_123", "pi_abc")); w.Code != 200 {
		t.Fatalf("invoice_payment.paid: %d", w.Code)
	}
	// It is refunded later. The charge carries no invoice.
	if w := deliver(t, h, refundBody("evt_ref", "pi_abc", 900)); w.Code != 200 {
		t.Fatalf("charge.refunded: %d: %s", w.Code, w.Body.String())
	}

	if len(rf.rows) != 1 {
		t.Fatalf("refunds written = %d, want 1; the customer gets no credit note "+
			"and the sale stays in the books at full value", len(rf.rows))
	}
	if r := rf.rows[0]; r.ReceiptID != "rcpt-1" || r.AmountCents != 900 {
		t.Errorf("refund = %+v", r)
	}
}

// Without the recorded link there is nothing to reverse. That must still be
// reported rather than guessed at -- a credit note is a numbered legal
// document and attaching one to the wrong sale is worse than attaching none.
func TestARefundWithNoRecordedLinkIsStillReportedNotGuessed(t *testing.T) {
	st := newTenants(store.Tenant{ID: "t1", StripeCustomerID: "cus_1"})
	h, rc, rf := newHandler(t, st)
	rc.rows = append(rc.rows, issuedReceipt()) // exists, but no payment_ref

	if w := deliver(t, h, refundBody("evt_ref", "pi_unknown", 900)); w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
	if len(rf.rows) != 0 {
		t.Fatalf("a credit note was raised against a receipt nothing linked to it")
	}
}

// A donation is refunded by its payment intent, as it always was. The new path
// must not shadow that.
func TestADonationRefundStillMatchesByItsPaymentIntent(t *testing.T) {
	st := newTenants(store.Tenant{ID: "t1", StripeCustomerID: "cus_1"})
	h, rc, rf := newHandler(t, st)
	rc.rows = append(rc.rows, store.Receipt{
		ID: "rcpt-d", TenantID: "t1", DeliveryKey: donationKey("pi_don", ""),
		AmountCents: 500, Currency: "EUR", Country: "NL", Status: store.ReceiptIssued,
	})

	if w := deliver(t, h, refundBody("evt_refd", "pi_don", 500)); w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
	if len(rf.rows) != 1 || rf.rows[0].ReceiptID != "rcpt-d" {
		t.Fatalf("donation refund did not match its receipt: %+v", rf.rows)
	}
}

// invoice_payment.paid is a note on a row, not an application of money. It
// must not consume the event id, or a redelivery that arrives before the
// receipt exists would look applied and the link would be lost for good.
func TestRecordingThePaymentLinkIsRetryable(t *testing.T) {
	st := newTenants(store.Tenant{ID: "t1", StripeCustomerID: "cus_1"})
	h, rc, _ := newHandler(t, st)

	// Arrives first, before the receipt exists. Answered 200, nothing recorded.
	if w := deliver(t, h, paymentPaidBody("evt_ip", "in_123", "pi_abc")); w.Code != 200 {
		t.Fatalf("early delivery: %d", w.Code)
	}
	rc.rows = append(rc.rows, issuedReceipt())

	// Stripe redelivers the same event. It must still take effect.
	if w := deliver(t, h, paymentPaidBody("evt_ip", "in_123", "pi_abc")); w.Code != 200 {
		t.Fatalf("redelivery: %d", w.Code)
	}
	if got := rc.rows[0].PaymentRef; got != "pi_abc" {
		t.Fatalf("payment_ref = %q after redelivery; the link was lost to idempotency", got)
	}
}

// An unpaid or failed invoice_payment says nothing about a settled payment.
func TestOnlyAPaidInvoicePaymentIsRecorded(t *testing.T) {
	st := newTenants(store.Tenant{ID: "t1", StripeCustomerID: "cus_1"})
	h, rc, _ := newHandler(t, st)
	rc.rows = append(rc.rows, issuedReceipt())

	body := fmt.Sprintf(`{"id":"evt_ip2","type":%q,"created":%d,"data":{"object":{
		"id":"inpay_2","invoice":"in_123","status":"open","amount_paid":0,"currency":"eur",
		"payment":{"payment_intent":"pi_open","type":"payment_intent"}}}}`,
		EventInvoicePaymentPaid, fixedNow.Unix())
	if w := deliver(t, h, body); w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
	if rc.rows[0].PaymentRef != "" {
		t.Errorf("recorded %q from an unpaid invoice payment", rc.rows[0].PaymentRef)
	}
}
