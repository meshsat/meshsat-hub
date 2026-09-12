package takoperator

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The account half of hosted TAK. A certificate does not admit a phone on its
// own -- EudHandlerSSL reads its common name and looks it up as an OpenTAKServer
// user -- so these accounts are what make an enrolled phone able to connect.

// userGateway stands up the fake instance gateway and hands back its client, the
// way the bootstrap tests do. The client comes from the server rather than being
// built here: there is exactly one right answer and no reason to invent another.
func userGateway(t *testing.T) (*fakeOTS, *http.Client, string) {
	t.Helper()
	ots := &fakeOTS{password: "pinned-admin-pw"}
	srv := httptest.NewServer(ots.handler())
	t.Cleanup(srv.Close)
	return ots, srv.Client(), srv.URL
}

func TestEnsureCreatesTheAccount(t *testing.T) {
	ots, cl, base := userGateway(t)

	msg, refused, err := applyUserWith(context.Background(), cl, base,
		"pinned-admin-pw", "fieldteam1", ActionEnsure)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if refused {
		t.Fatalf("ensure was refused: %s", msg)
	}
	if len(ots.adds) != 1 || ots.adds[0] != "fieldteam1" {
		t.Errorf("accounts added = %v, want [fieldteam1]", ots.adds)
	}
	if !ots.users["fieldteam1"] {
		t.Error("the account is not active on the instance")
	}
}

// Idempotent, because the intent is "this person can connect", not "a row was
// created just now". A retry after a partial failure must not become an error the
// customer sees.
func TestEnsureIsAppliedWhenTheAccountAlreadyExists(t *testing.T) {
	ots, cl, base := userGateway(t)
	ots.users = map[string]bool{"fieldteam1": true}

	msg, refused, err := applyUserWith(context.Background(), cl, base,
		"pinned-admin-pw", "fieldteam1", ActionEnsure)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if refused {
		t.Fatalf("an existing account was treated as a refusal: %s", msg)
	}
	if !strings.Contains(msg, "already exists") {
		t.Errorf("message = %q, want it to say the account already exists", msg)
	}
}

// Any OTHER 400 is terminal: only the requester can fix it, and retrying one every
// thirty seconds forever buries the reason instead of surfacing it.
func TestAnUnacceptableAccountIsRefusedRatherThanRetried(t *testing.T) {
	ots, cl, base := userGateway(t)
	ots.addStatus = 400
	ots.addBody = `{"error":"password is too short"}`

	msg, refused, err := applyUserWith(context.Background(), cl, base,
		"pinned-admin-pw", "fieldteam1", ActionEnsure)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !refused {
		t.Error("a 400 the requester must fix was not refused, so it will be retried forever")
	}
	if !strings.Contains(msg, "too short") {
		t.Errorf("message = %q, want it to carry the instance's own reason", msg)
	}
}

func TestDeactivateSwitchesTheAccountOff(t *testing.T) {
	ots, cl, base := userGateway(t)
	ots.users = map[string]bool{"fieldteam1": true}

	msg, refused, err := applyUserWith(context.Background(), cl, base,
		"pinned-admin-pw", "fieldteam1", ActionDeactivate)
	if err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if refused {
		t.Fatalf("deactivate was refused: %s", msg)
	}
	if ots.users["fieldteam1"] {
		t.Error("the account is still active")
	}
	if len(ots.users) != 1 {
		t.Error("deactivation removed the row; the EUD rows, position history and audit " +
			"trail all reference the user, so it must stay")
	}
}

// Deactivating an account that is not there is desired state already satisfied.
// Anything else would wedge a retry after a partial failure.
func TestDeactivatingAnAbsentAccountIsAlreadyDone(t *testing.T) {
	_, cl, base := userGateway(t)

	msg, refused, err := applyUserWith(context.Background(), cl, base,
		"pinned-admin-pw", "goneaway", ActionDeactivate)
	if err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if refused {
		t.Fatalf("an absent account was refused: %s", msg)
	}
	if !strings.Contains(msg, "nothing to deactivate") {
		t.Errorf("message = %q, want it to say there was nothing to do", msg)
	}
}

// The asymmetry that matters most. A refused administrator login is an
// INFRASTRUCTURE failure, not a denial: on a fresh instance bootstrapAdmin may not
// have run yet, and recording a denial would refuse the customer permanently for an
// instance that is merely not ready.
func TestARefusedAdminLoginIsRetriedNotDenied(t *testing.T) {
	_, cl, base := userGateway(t)

	msg, refused, err := applyUserWith(context.Background(), cl, base,
		"the-wrong-password", "fieldteam1", ActionEnsure)
	if !errors.Is(err, ErrAdminLoginRefused) {
		t.Fatalf("error = %v, want ErrAdminLoginRefused so the request stays Pending", err)
	}
	if refused {
		t.Errorf("a login failure was reported as a refusal (%q), which would deny the "+
			"customer permanently", msg)
	}
}

// An unexpected status is retried too: a 503 from third-party code is not a
// customer error.
func TestAnUnexpectedStatusIsRetriedRatherThanDenied(t *testing.T) {
	ots, cl, base := userGateway(t)
	ots.addStatus = 503
	ots.addBody = `{"error":"service unavailable"}`

	_, refused, err := applyUserWith(context.Background(), cl, base,
		"pinned-admin-pw", "fieldteam1", ActionEnsure)
	if err == nil {
		t.Fatal("a 503 returned no error, so the request would be recorded as applied")
	}
	if refused {
		t.Error("a 503 was reported as a refusal")
	}
}

// The generated password exists only because OpenTAKServer requires a user row to
// have one. It must never leave the operator: not in the status, not in an error,
// nowhere the Hub or a customer can read it.
func TestTheGeneratedPasswordNeverLeavesTheOperator(t *testing.T) {
	ots, cl, base := userGateway(t)

	msg, _, err := applyUserWith(context.Background(), cl, base,
		"pinned-admin-pw", "fieldteam1", ActionEnsure)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(ots.addedPasswords) != 1 {
		t.Fatalf("passwords seen by the instance = %d, want 1", len(ots.addedPasswords))
	}
	pw := ots.addedPasswords[0]
	if pw == "" {
		t.Fatal("no password was sent, so the account could not have been created")
	}
	if strings.Contains(msg, pw) {
		t.Error("the status message carries the generated password")
	}
	// OpenTAKServer's endpoints refuse these two characters, which is why
	// randomValue produces base64url rather than anything punctuation-bearing.
	if strings.ContainsAny(pw, ":@") {
		t.Errorf("the generated password contains ':' or '@', which the instance refuses: %q", pw)
	}
	if len(pw) < 32 {
		t.Errorf("the generated password is only %d characters", len(pw))
	}
}

// Two accounts must not share a password, so nothing derives a predictable
// credential from a retry or from another tenant's account.
func TestEachEnsureGeneratesItsOwnPassword(t *testing.T) {
	ots, cl, base := userGateway(t)
	for _, name := range []string{"fieldone", "fieldtwo"} {
		if _, _, err := applyUserWith(context.Background(), cl, base,
			"pinned-admin-pw", name, ActionEnsure); err != nil {
			t.Fatalf("ensure %s: %v", name, err)
		}
	}
	if len(ots.addedPasswords) != 2 {
		t.Fatalf("passwords = %d, want 2", len(ots.addedPasswords))
	}
	if ots.addedPasswords[0] == ots.addedPasswords[1] {
		t.Error("two accounts were given the same generated password")
	}
}

// An action the CRD does not allow, reaching here because somebody added one to
// the schema and not to the code, refuses itself by name rather than silently
// doing nothing.
func TestAnUnsupportedActionRefusesItselfByName(t *testing.T) {
	_, cl, base := userGateway(t)

	msg, refused, err := applyUserWith(context.Background(), cl, base,
		"pinned-admin-pw", "fieldteam1", "Delete")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !refused {
		t.Error("an unsupported action was not refused")
	}
	if !strings.Contains(msg, "Delete") {
		t.Errorf("message = %q, want it to name the action", msg)
	}
}

// The refusals that need no instance at all, including the one that is a security
// check: "administrator" passes the username pattern, and that account's password
// is the operator's own.
func TestUserRequestRefusals(t *testing.T) {
	const goodLabel = "abcdefghij"
	cases := []struct {
		name string
		spec TakUserRequestSpec
		want string
	}{
		{"a good request is accepted",
			TakUserRequestSpec{Label: goodLabel, Username: "fieldteam1", Action: ActionEnsure}, ""},
		{"the administrator account is refused",
			TakUserRequestSpec{Label: goodLabel, Username: otsAdminUsername, Action: ActionEnsure},
			"administrator"},
		{"a hyphenated username is refused, because OpenTAKServer refuses it",
			TakUserRequestSpec{Label: goodLabel, Username: "field-team-1", Action: ActionEnsure},
			"hyphens"},
		{"the Hub identity cannot be asked for, covered by the hyphen rule",
			TakUserRequestSpec{Label: goodLabel, Username: HubIdentityCN, Action: ActionEnsure},
			"hyphens"},
		{"a short username is refused",
			TakUserRequestSpec{Label: goodLabel, Username: "ab", Action: ActionEnsure}, "3 to 32"},
		{"a bad label is refused",
			TakUserRequestSpec{Label: "TENANT", Username: "fieldteam1", Action: ActionEnsure}, "label"},
		{"an unknown action is refused",
			TakUserRequestSpec{Label: goodLabel, Username: "fieldteam1", Action: "Suspend"}, "action"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := userRequestRefusal(tc.spec)
			if tc.want == "" {
				if got != "" {
					t.Errorf("refused an acceptable request: %s", got)
				}
				return
			}
			if got == "" {
				t.Fatalf("accepted a request that must be refused: %+v", tc.spec)
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("reason = %q, want it to mention %q", got, tc.want)
			}
		})
	}
}
