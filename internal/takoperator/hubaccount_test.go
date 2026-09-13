package takoperator

import (
	"os"
	"strings"
	"testing"
	"unicode"
)

// MESHSAT-1088. The first real instance was provisioned, a phone's certificate was
// accepted, the tenant was resolved, the user was authorised, the bytes were
// proxied -- and OpenTAKServer discarded every one of them, because the common name
// the front presents was not one of its user accounts and could not be made one.
//
// These tests hold the three things that have to stay true together. Each of them
// is cheap; the defect they prevent was invisible to every other test in the repo,
// because nothing exercised the front-to-OTS leg.

// upstreamRefusesHyphen mirrors OpenTAKServer's own validator, read out of the
// running image rather than written from memory:
//
//	opentakserver/UsernameValidator.check_username
//	  for character in username:
//	      if unicodedata.category(character)[0] not in ["L", "N"]
//	         and character != "_" and character != ".":
//	          return USERNAME_DISALLOWED_CHARACTERS
//
// So: letters, numbers, underscore, full stop. Nothing else.
func upstreamWouldAccept(username string) bool {
	for _, r := range username {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return username != ""
}

// THE REGRESSION TEST. If this fails, hosted TAK stores nothing for anybody,
// however healthy every instance looks.
func TestTheHubsIdentityIsAnameOpenTAKServerCanCreate(t *testing.T) {
	if !upstreamWouldAccept(HubIdentityCN) {
		t.Fatalf("HubIdentityCN = %q contains a character OpenTAKServer's username "+
			"validator refuses, so no account can exist for it -- the front will "+
			"connect, the instance will drop it, and every phone's CoT will be "+
			"discarded in silence", HubIdentityCN)
	}
	if strings.Contains(HubIdentityCN, "-") {
		t.Errorf("HubIdentityCN = %q uses a hyphen; upstream's validator rejects "+
			"category Pd, which is exactly how MESHSAT-1088 happened", HubIdentityCN)
	}
}

// And the other half: the separator must still be something the customer-facing
// pattern refuses, or a tenant could ask the operator to create, deactivate or
// impersonate the account the front connects as.
func TestATenantStillCannotTouchTheHubsIdentity(t *testing.T) {
	if usernamePattern.MatchString(HubIdentityCN) {
		t.Fatalf("the phone username pattern ACCEPTS %q, so a tenant could manage "+
			"the Hub's own account through the customer-facing API", HubIdentityCN)
	}
	// Through both customer-facing doors, explicitly.
	if why := certRequestDenyReason(certReq(certTestLabel, PurposeEUD, HubIdentityCN, someCSR)); why == "" {
		t.Error("an eud certificate was issued for the Hub's own identity")
	}
	if why := userRequestRefusal(TakUserRequestSpec{
		Label: certTestLabel, Username: HubIdentityCN, Action: ActionEnsure,
	}); why == "" {
		t.Error("a tenant could create the Hub's own OTS account through the user API")
	}
	// But the operator asking for its own is still fine.
	if why := certRequestDenyReason(certReq(certTestLabel, PurposeHub, HubIdentityCN, someCSR)); why != "" {
		t.Errorf("the Hub cannot request its own identity: %q", why)
	}
}

// The CRD carries the same name as a CEL literal, and the API server is what
// enforces it. If the two drift, every hub request is answered 422 -- the Hub logs
// "no upstream identity for tenant", the directory empties, and no phone in ANY
// tenant can connect. That exact pair has already been out of step once.
func TestTheCRDLiteralMatchesTheConstant(t *testing.T) {
	raw, err := os.ReadFile("../../k8s/tak-operator/crds/takcertificaterequest.yaml")
	if err != nil {
		t.Skipf("CRD not readable from here: %v", err)
	}
	want := "self.username == '" + HubIdentityCN + "'"
	if !strings.Contains(string(raw), want) {
		t.Fatalf("the CRD's CEL rule does not require %q.\n"+
			"The API server, not this package, is what admits a hub request: if the "+
			"CRD still names the old value, every hub certificate request is refused "+
			"with 422 and hosted TAK is dead for every tenant.\nLooked for: %s",
			HubIdentityCN, want)
	}
	// And the nginx gateway the operator itself walks through. Assert the MAP RULE,
	// not merely that the name appears somewhere: the first version of this check
	// was satisfied by a comment, so breaking the rule did not fail it. Found by
	// mutating the rule and watching the test stay green.
	cfg := nginxConfig()
	rule := `"~(^|,)CN=` + HubIdentityCN + `(,|$)"`
	if !strings.Contains(cfg, rule) {
		t.Errorf("the rendered admin gateway's ssl_client_s_dn map does not admit "+
			"CN=%s, so the operator cannot reach the instance to create the account "+
			"in the first place.\nLooked for: %s", HubIdentityCN, rule)
	}
}
