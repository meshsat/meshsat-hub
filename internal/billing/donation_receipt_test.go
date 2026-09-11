package billing

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/mail"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// The first real donation, EUR 1.00 on 2026-09-11, produced a correct document
// and a wrong email. The billing system has ONE payment template per company
// and it is written for a subscription, so the giver was told their receipt
// "shows the VAT included in the price" (it shows no tax at all), that "your
// MeshSat Hub subscription is active for the period this payment covers" (a
// gift buys nothing) and to check the Settings page (they have no account).
//
// The fix is to send a donation's copy from the Hub and switch the provider's
// off. These tests hold both halves, and the guard between them.

type fakeMailer struct {
	mu       sync.Mutex
	plain    []mail.Message
	withAtt  []mail.Message
	atts     []mail.Attachment
	to       []string
	sendErr  error
	attachEr error
}

func (f *fakeMailer) SendMessage(_ context.Context, to string, m mail.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.to, f.plain = append(f.to, to), append(f.plain, m)
	return f.sendErr
}

func (f *fakeMailer) SendMessageWith(_ context.Context, to string, m mail.Message, a mail.Attachment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.attachEr != nil {
		return f.attachEr
	}
	f.to, f.withAtt, f.atts = append(f.to, to), append(f.withAtt, m), append(f.atts, a)
	return nil
}

// subscriptionReceipt is the other kind of money: a real sale, whose receipt
// still comes from the billing system.
func subscriptionReceipt(t *testing.T, rc *memReceipts, id string) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := rc.CreateReceipt(context.Background(), &store.Receipt{
		ID: id, TenantID: "t-a", Country: "NL",
		DeliveryKey: "k-" + id, Email: "customer@example.com", Name: "A Customer",
		AmountCents: 900, Currency: "EUR", Plan: "crew", TierName: "Crew",
		PaidAt: now, NextAttemptAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
}

type fakeDocs struct {
	pdf  []byte
	err  error
	last string
}

func (f *fakeDocs) InvoicePDF(_ context.Context, invoiceID string) ([]byte, error) {
	f.last = invoiceID
	if f.err != nil {
		return nil, f.err
	}
	return f.pdf, nil
}

// The headline defect. A gift must not go out under the subscription template.
func TestADonationSuppressesTheProvidersSubscriptionEmail(t *testing.T) {
	rc := &memReceipts{}
	iss := &capturingIssuer{}
	donationReceipt(t, rc, "r-1", "NL")

	j := NewReceiptJob(rc, iss, nil)
	j.SetMailer(&fakeMailer{}, &fakeDocs{pdf: []byte("%PDF-1.4")})
	j.Once(context.Background())

	if !iss.last.SuppressReceiptEmail {
		t.Fatal("the billing system was left to mail a subscription receipt about a donation")
	}
}

// A subscription's template is correct for a subscription. Nothing here may
// stop a paying customer being sent their receipt.
func TestASubscriptionNeverSuppressesTheProvidersEmail(t *testing.T) {
	rc := &memReceipts{}
	iss := &capturingIssuer{}
	subscriptionReceipt(t, rc, "r-2")

	j := NewReceiptJob(rc, iss, nil)
	j.SetMailer(&fakeMailer{}, &fakeDocs{pdf: []byte("%PDF-1.4")})
	j.Once(context.Background())

	if iss.last.SuppressReceiptEmail {
		t.Fatal("a paying customer would receive no receipt at all")
	}
}

// The guard that matters most. Suppressing the provider's email without being
// able to send our own is money taken and the giver told nothing -- strictly
// worse than the wrong words arriving.
func TestWithNoMailerTheProvidersEmailIsLeftAlone(t *testing.T) {
	for _, tc := range []struct {
		name         string
		mailer       Mailer
		docs         DocFetcher
		wantSuppress bool
	}{
		{"neither", nil, nil, false},
		{"mailer but no pdf fetcher", &fakeMailer{}, nil, false},
		{"pdf fetcher but no mailer", nil, &fakeDocs{}, false},
		{"both", &fakeMailer{}, &fakeDocs{pdf: []byte("%PDF")}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := &memReceipts{}
			iss := &capturingIssuer{}
			donationReceipt(t, rc, "r-"+tc.name, "NL")

			j := NewReceiptJob(rc, iss, nil)
			if tc.mailer != nil || tc.docs != nil {
				j.SetMailer(tc.mailer, tc.docs)
			}
			j.Once(context.Background())

			if iss.last.SuppressReceiptEmail != tc.wantSuppress {
				t.Fatalf("SuppressReceiptEmail = %v, want %v", iss.last.SuppressReceiptEmail, tc.wantSuppress)
			}
		})
	}
}

// The giver gets the document, from us, with the PDF on it.
func TestTheHubSendsTheDonorTheirReceiptWithThePDF(t *testing.T) {
	rc := &memReceipts{}
	iss := &capturingIssuer{}
	mlr := &fakeMailer{}
	docs := &fakeDocs{pdf: []byte("%PDF-1.4 donation")}
	donationReceipt(t, rc, "r-3", "NL")

	j := NewReceiptJob(rc, iss, nil)
	j.SetMailer(mlr, docs)
	j.Once(context.Background())

	if len(mlr.withAtt) != 1 {
		t.Fatalf("sent %d messages with an attachment, want 1", len(mlr.withAtt))
	}
	if mlr.to[0] != "giver@example.com" {
		t.Errorf("sent to %q", mlr.to[0])
	}
	if docs.last != "inv-1" {
		t.Errorf("fetched the PDF for %q, want the invoice just issued", docs.last)
	}
	att := mlr.atts[0]
	if string(att.Content) != "%PDF-1.4 donation" {
		t.Error("the attachment is not the PDF that was fetched")
	}
	if att.ContentType != "application/pdf" {
		t.Errorf("content type %q", att.ContentType)
	}
	// Named after the document, so filing it does not mean opening it.
	if att.Filename != "MSH2026-0001.pdf" {
		t.Errorf("attachment filename %q", att.Filename)
	}
	body := mlr.withAtt[0].Text + mlr.withAtt[0].HTML
	if !strings.Contains(body, "MSH2026-0001") {
		t.Error("the message does not name the receipt it carries")
	}
}

// A billing system that cannot render the PDF must not cost the giver their
// notice. The words carry the facts on their own.
func TestAMissingPDFStillSendsTheWords(t *testing.T) {
	rc := &memReceipts{}
	iss := &capturingIssuer{}
	mlr := &fakeMailer{}
	donationReceipt(t, rc, "r-4", "NL")

	j := NewReceiptJob(rc, iss, nil)
	j.SetMailer(mlr, &fakeDocs{err: errors.New("pdf render failed")})
	j.Once(context.Background())

	if len(mlr.plain) != 1 {
		t.Fatalf("sent %d plain messages, want 1", len(mlr.plain))
	}
	if !strings.Contains(mlr.plain[0].Text, "MSH2026-0001") {
		t.Error("the fallback notice does not name the receipt")
	}
}

// A subscription's receipt still comes from the billing system, so the Hub
// must not send a second one about the same money.
func TestTheHubSendsNothingForASubscription(t *testing.T) {
	rc := &memReceipts{}
	mlr := &fakeMailer{}
	subscriptionReceipt(t, rc, "r-5")

	j := NewReceiptJob(rc, &capturingIssuer{}, nil)
	j.SetMailer(mlr, &fakeDocs{pdf: []byte("%PDF")})
	j.Once(context.Background())

	if len(mlr.plain)+len(mlr.withAtt) != 0 {
		t.Fatal("the customer would receive two receipts for one payment")
	}
}

// pdfName reaches a mail header, so it takes nothing it has not checked.
func TestPDFNameKeepsOnlySafeCharacters(t *testing.T) {
	for in, want := range map[string]string{
		"MSH2026-0001":     "MSH2026-0001.pdf",
		"MSH 2026/0001":    "MSH20260001.pdf",
		"a\"b;c\nd":        "abcd.pdf",
		"":                 "receipt.pdf",
		"../../etc/passwd": "etcpasswd.pdf",
		"\r\nBcc: x@y.z":   "Bccxyz.pdf",
	} {
		if got := pdfName(in); got != want {
			t.Errorf("pdfName(%q) = %q, want %q", in, got, want)
		}
	}
}
