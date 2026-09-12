package takoperator

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// This file exists because the certificate-request validator had NO tests, and
// that is exactly how the Hub's own identity came to be unrequestable.
//
// The bug: one username pattern was applied to every purpose. A phone's username
// may not contain a hyphen, because OpenTAKServer answers HTTP 400 to one. The
// Hub's identity is "meshsat-hub" and its hyphen is deliberate -- userreq.go
// relies on it to keep a tenant from managing that account through the
// customer-facing API, and each instance's nginx gateway admits exactly
// CN=meshsat-hub. So the one rule refused the very request the Hub has to make.
//
// What it looked like in production, found by enabling the front and reading the
// log rather than by any test: the API server answered 422 to every hub request,
// the Hub logged "no upstream identity for tenant", and its directory refreshed
// to `tenants:0 skipped:1`. The front was listening, correct, and empty. No phone
// in any tenant could have connected, and nothing was red.

func certReq(label, purpose, username, csr string) *TakCertificateRequest {
	req := &TakCertificateRequest{}
	req.Spec.Label = label
	req.Spec.Purpose = purpose
	req.Spec.Username = username
	req.Spec.CSRPEM = csr
	return req
}

const (
	someCSR = "-----BEGIN CERTIFICATE REQUEST-----\nx\n-----END CERTIFICATE REQUEST-----\n"
	// Ten lowercase alphanumerics, which is what labelPattern wants.
	certTestLabel = "abcdefghij"
)

// TestTheHubsOwnIdentityCanBeRequested is the regression test. If this fails,
// hosted TAK is entirely non-functional however healthy it looks.
func TestTheHubsOwnIdentityCanBeRequested(t *testing.T) {
	if why := certRequestDenyReason(certReq(certTestLabel, PurposeHub, HubIdentityCN, someCSR)); why != "" {
		t.Fatalf("the Hub cannot ask for its own upstream identity: %q\n"+
			"every tenant would be skipped from the directory and no phone could connect", why)
	}
}

func TestAHubCertificateIsOnlyEverIssuedForTheHub(t *testing.T) {
	// A hub certificate is admitted by every tenant's admin gateway, so the name
	// it carries is the one thing that must not be negotiable.
	for _, name := range []string{"phone01", "administrator", "meshsathub", "meshsat-hubbish", ""} {
		why := certRequestDenyReason(certReq(certTestLabel, PurposeHub, name, someCSR))
		if why == "" {
			t.Errorf("a hub certificate was allowed for %q", name)
		}
	}
}

func TestAPhoneStillMayNotHaveAHyphen(t *testing.T) {
	// The reason this rule exists at all: OpenTAKServer refuses such a username,
	// so a certificate carrying it would authenticate as nobody.
	for _, name := range []string{"phone-01", "Phone01", "ph", "phone.01", "a@b",
		strings.Repeat("x", 33), ""} {
		if why := certRequestDenyReason(certReq(certTestLabel, PurposeEUD, name, someCSR)); why == "" {
			t.Errorf("an eud certificate was allowed for %q", name)
		}
	}
	if why := certRequestDenyReason(certReq(certTestLabel, PurposeEUD, "phone01", someCSR)); why != "" {
		t.Errorf("an ordinary phone username was refused: %q", why)
	}
}

// A phone must not be able to obtain the Hub's name even though the hub purpose
// now accepts it: that would hand a tenant's device a certificate the admin
// gateway admits.
func TestAPhoneCannotBorrowTheHubsName(t *testing.T) {
	why := certRequestDenyReason(certReq(certTestLabel, PurposeEUD, HubIdentityCN, someCSR))
	if why == "" {
		t.Fatal("an eud certificate was issued for the Hub's own identity")
	}
	if !strings.Contains(why, "hyphen") {
		t.Errorf("the refusal should name the rule that caught it, got %q", why)
	}
}

func TestTheOtherRefusalsStillHold(t *testing.T) {
	cases := map[string]*TakCertificateRequest{
		"a label that is not ten alphanumerics": certReq("short", PurposeEUD, "phone01", someCSR),
		"an empty label":                        certReq("", PurposeEUD, "phone01", someCSR),
		"an unknown purpose":                    certReq(certTestLabel, "something", "phone01", someCSR),
		"no purpose":                            certReq(certTestLabel, "", "phone01", someCSR),
		"no CSR":                                certReq(certTestLabel, PurposeEUD, "phone01", ""),
	}
	for what, req := range cases {
		if why := certRequestDenyReason(req); why == "" {
			t.Errorf("%s was accepted", what)
		}
	}
}

// Reaping abandoned identity requests (MESHSAT-1076). Requests are named per
// replica so two Hub replicas cannot fight over one certificate, which means a
// replaced pod leaves its object behind -- and a Deployment is rolled on every
// deploy. These assertions are about the two directions of the mistake: leaving
// clutter forever, and deleting something somebody is waiting for.
func TestAbandonedHubRequestsAreReapedAndNothingElseIs(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	aged := func(purpose, phase string, age time.Duration) *TakCertificateRequest {
		r := certReq(certTestLabel, purpose, HubIdentityCN, someCSR)
		if purpose == PurposeEUD {
			r.Spec.Username = "phone01"
		}
		r.Status.Phase = phase
		r.CreationTimestamp = metav1.NewTime(now.Add(-age))
		return r
	}

	for _, tc := range []struct {
		what string
		req  *TakCertificateRequest
		want bool
	}{
		{"an hours-old issued hub request is abandoned", aged(PurposeHub, CertIssued, 2*time.Hour), true},
		{"an hours-old denied hub request is abandoned", aged(PurposeHub, CertDenied, 2*time.Hour), true},
		{"a fresh issued hub request is NOT (the Hub collects within one refresh)",
			aged(PurposeHub, CertIssued, 3*time.Minute), false},
		{"a hub request just under the TTL is NOT", aged(PurposeHub, CertIssued, hubRequestTTL-time.Minute), false},
		{"a PENDING hub request is never reaped, however old -- it is the operator's own backlog",
			aged(PurposeHub, "", 48*time.Hour), false},
		{"an EUD request is never reaped on a timer; enrolment has its own lifecycle",
			aged(PurposeEUD, CertIssued, 48*time.Hour), false},
	} {
		if got := abandonedHubRequest(tc.req, now); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.what, got, tc.want)
		}
	}

	// No creation timestamp means no age to judge by. Leaving it is the safe
	// direction: clutter is cheap, deleting a certificate somebody waits for is not.
	noStamp := certReq(certTestLabel, PurposeHub, HubIdentityCN, someCSR)
	noStamp.Status.Phase = CertIssued
	if abandonedHubRequest(noStamp, now) {
		t.Error("a request with no creation timestamp was reaped")
	}
	if abandonedHubRequest(nil, now) {
		t.Error("a nil request was reaped")
	}
}

// The age arithmetic is done in UTC against the object's own timestamp. This
// estate has already deleted things early once by reading a UTC timestamp as local
// (the backup unwedge CronJob, where time.mktime shifted everything two hours), so
// the boundary is asserted explicitly.
func TestTheReapBoundaryDoesNotShiftWithTheLocalZone(t *testing.T) {
	req := certReq(certTestLabel, PurposeHub, HubIdentityCN, someCSR)
	req.Status.Phase = CertIssued
	created := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
	req.CreationTimestamp = metav1.NewTime(created)

	// Just inside the window, expressed in a zone three hours ahead: still young.
	tooSoon := created.Add(hubRequestTTL - time.Minute).In(time.FixedZone("UTC+3", 3*3600))
	if abandonedHubRequest(req, tooSoon) {
		t.Error("reaped a request that is still inside its TTL when the clock is in another zone")
	}
	past := created.Add(hubRequestTTL + time.Minute).In(time.FixedZone("UTC-7", -7*3600))
	if !abandonedHubRequest(req, past) {
		t.Error("failed to reap an expired request when the clock is in another zone")
	}
}

// The purpose is checked before the username, so an unknown purpose is reported as
// an unknown purpose rather than as whatever the username rule happens to say
// about it. A reason a person cannot act on is barely better than none.
func TestAnUnknownPurposeIsReportedAsSuch(t *testing.T) {
	why := certRequestDenyReason(certReq(certTestLabel, "hubb", HubIdentityCN, someCSR))
	if !strings.Contains(why, "purpose") {
		t.Errorf("reason = %q, want it to name the purpose", why)
	}
}
