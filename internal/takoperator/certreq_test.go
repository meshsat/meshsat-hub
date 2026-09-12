package takoperator

import (
	"strings"
	"testing"
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

// The purpose is checked before the username, so an unknown purpose is reported as
// an unknown purpose rather than as whatever the username rule happens to say
// about it. A reason a person cannot act on is barely better than none.
func TestAnUnknownPurposeIsReportedAsSuch(t *testing.T) {
	why := certRequestDenyReason(certReq(certTestLabel, "hubb", HubIdentityCN, someCSR))
	if !strings.Contains(why, "purpose") {
		t.Errorf("reason = %q, want it to name the purpose", why)
	}
}
