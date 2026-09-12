package takhosted

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

// The account keeper asks the operator for the OpenTAKServer accounts phones
// authenticate as. A certificate alone does not admit a phone, so these requests
// are what make an enrolled phone able to connect.

const (
	accLabel = "abcdefghij"
	accUser  = "fieldteam1"
)

func accountKeeper(t *testing.T) (*fakeAPI, *AccountKeeper, string) {
	t.Helper()
	ca := newTestCA(t, "MeshSat TAK "+accLabel+" CA")
	api := newFakeAPI(ca)
	srv := httptest.NewServer(api.handler(t))
	t.Cleanup(srv.Close)
	cl := NewClientWith(srv.Client(), srv.URL, "meshsat-tak")
	return api, NewAccountKeeper(cl, nil), userRequestName(accLabel, accUser)
}

// The object name is deterministic and derived from the opaque label, never the
// tenant id: it is visible in the namespace and must leak nothing about who the
// customer is.
func TestTheRequestNameIsDeterministicAndLeaksNoTenantIdentity(t *testing.T) {
	got := userRequestName(accLabel, accUser)
	if got != "user-"+accLabel+"-"+accUser {
		t.Errorf("name = %q, want user-<label>-<username>", got)
	}
	if got != userRequestName(accLabel, accUser) {
		t.Error("the name is not deterministic, so a retry would queue a second object")
	}
	// DNS-safe: the CRD constrains both parts to lowercase alphanumerics.
	for _, r := range got {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			t.Errorf("name %q carries %q, which is not DNS-safe", got, r)
		}
	}
}

// Asking is not waiting. The operator reconciles on a ticker, so the first call
// reports pending and the caller says "provisioning" rather than holding an HTTP
// request open for another process to tick.
func TestEnsureAsksTheOperatorAndReportsPending(t *testing.T) {
	api, k, name := accountKeeper(t)

	err := k.Ensure(context.Background(), accLabel, accUser)
	if !errors.Is(err, ErrAccountPending) {
		t.Fatalf("err = %v, want ErrAccountPending", err)
	}
	req, ok := api.userRequest(name)
	if !ok {
		t.Fatal("no request object was created, so the operator will never act")
	}
	if req.Spec.Action != ActionEnsure || req.Spec.Username != accUser || req.Spec.Label != accLabel {
		t.Errorf("spec = %+v, want label/username/Ensure", req.Spec)
	}
	if len(api.userApplies) != 1 {
		t.Errorf("applies = %v, want exactly one", api.userApplies)
	}
}

// Once the operator has applied it, asking again costs a read and no write.
func TestAnAppliedAccountIsSatisfiedWithoutWritingAgain(t *testing.T) {
	api, k, name := accountKeeper(t)
	if err := k.Ensure(context.Background(), accLabel, accUser); !errors.Is(err, ErrAccountPending) {
		t.Fatalf("first ensure: %v", err)
	}
	api.actAsOperator(name, UserApplied, "account created")

	if err := k.Ensure(context.Background(), accLabel, accUser); err != nil {
		t.Fatalf("second ensure = %v, want nil once applied", err)
	}
	if len(api.userApplies) != 1 {
		t.Errorf("applies = %v, want still one: an applied account needs no second write", api.userApplies)
	}
}

// A denial is terminal and carries the reason. Retrying it forever would bury the
// one piece of information that lets somebody fix it.
func TestADeniedRequestIsTerminalAndCarriesTheReason(t *testing.T) {
	api, k, name := accountKeeper(t)
	if err := k.Ensure(context.Background(), accLabel, accUser); !errors.Is(err, ErrAccountPending) {
		t.Fatalf("first ensure: %v", err)
	}
	api.actAsOperator(name, UserDenied, "the administrator account cannot be managed through this API")

	err := k.Ensure(context.Background(), accLabel, accUser)
	if !errors.Is(err, ErrAccountDenied) {
		t.Fatalf("err = %v, want ErrAccountDenied", err)
	}
	if !strings.Contains(err.Error(), "administrator") {
		t.Errorf("err = %v, want it to carry the operator's reason", err)
	}
}

// THE test for the generation check. After Ensure is applied, asking to
// Deactivate must not be satisfied by the stale "Applied" status -- that would
// report a teammate switched off while they were still connecting.
func TestAStaleStatusIsNotTrustedWhenTheDesiredStateChanges(t *testing.T) {
	api, k, name := accountKeeper(t)
	ctx := context.Background()

	if err := k.Ensure(ctx, accLabel, accUser); !errors.Is(err, ErrAccountPending) {
		t.Fatalf("ensure: %v", err)
	}
	api.actAsOperator(name, UserApplied, "account created")
	if err := k.Ensure(ctx, accLabel, accUser); err != nil {
		t.Fatalf("ensure after apply: %v", err)
	}

	// Now ask for the opposite. The status still says Applied.
	err := k.Deactivate(ctx, accLabel, accUser)
	if err == nil {
		t.Fatal("Deactivate returned nil while the status described the PREVIOUS desired " +
			"state; the account is still able to connect and the caller was told otherwise")
	}
	if !errors.Is(err, ErrAccountPending) {
		t.Fatalf("err = %v, want ErrAccountPending", err)
	}

	req, ok := api.userRequest(name)
	if !ok {
		t.Fatal("the request object vanished")
	}
	if req.Spec.Action != ActionDeactivate {
		t.Errorf("spec action = %q, want Deactivate", req.Spec.Action)
	}
	if req.Metadata.Generation < 2 {
		t.Errorf("generation = %d, want it bumped by the spec change", req.Metadata.Generation)
	}
	if got := strings.Join(api.userApplies, ","); !strings.Contains(got, ":Deactivate") {
		t.Errorf("applies = %v, want a Deactivate write", api.userApplies)
	}

	// Ask AGAIN while the operator is still catching up. This is the case the
	// generation check exists for, and the only one that reaches it: the spec now
	// says Deactivate, so the action matches, and the ONLY thing standing between
	// the caller and a false "already done" is observedGeneration still being 1
	// while generation is 2.
	//
	// Without that comparison this returns nil, and a customer is told their
	// teammate can no longer connect while that teammate is still connecting.
	if err := k.Deactivate(ctx, accLabel, accUser); !errors.Is(err, ErrAccountPending) {
		t.Fatalf("asking again before the operator acted = %v, want ErrAccountPending; a stale "+
			"Applied status was read as the answer to the question just asked", err)
	}

	// And once the operator catches up, it is satisfied.
	api.actAsOperator(name, UserApplied, "account deactivated")
	if err := k.Deactivate(ctx, accLabel, accUser); err != nil {
		t.Errorf("deactivate after the operator caught up = %v, want nil", err)
	}
}

// Deactivation keeps the request object. Deleting it would remove the Hub's record
// of the desired state and leave the account exactly as it was: able to connect.
func TestDeactivateKeepsTheRequestObject(t *testing.T) {
	api, k, name := accountKeeper(t)
	if err := k.Deactivate(context.Background(), accLabel, accUser); !errors.Is(err, ErrAccountPending) {
		t.Fatalf("deactivate: %v", err)
	}
	req, ok := api.userRequest(name)
	if !ok {
		t.Fatal("the request object was not kept")
	}
	if req.Spec.Action != ActionDeactivate {
		t.Errorf("action = %q, want Deactivate", req.Spec.Action)
	}
}

// A username OpenTAKServer would refuse is refused here, before the API server is
// touched, so the error names the rule instead of arriving as an admission failure.
func TestAnIllegalUsernameIsRefusedWithoutTouchingTheAPI(t *testing.T) {
	api, k, _ := accountKeeper(t)

	for _, bad := range []string{"field-team-1", "ab", "FieldTeam", strings.Repeat("a", 33)} {
		err := k.Ensure(context.Background(), accLabel, bad)
		if !errors.Is(err, ErrBadUsername) {
			t.Errorf("Ensure(%q) = %v, want ErrBadUsername", bad, err)
		}
	}
	if len(api.userApplies) != 0 {
		t.Errorf("the API was written to for an illegal username: %v", api.userApplies)
	}
}

// A tenant with no instance label cannot have accounts, and that is a programming
// error worth naming rather than a request to send.
func TestAMissingLabelIsRefused(t *testing.T) {
	api, k, _ := accountKeeper(t)
	if err := k.Ensure(context.Background(), "", accUser); err == nil {
		t.Error("an empty label was accepted")
	}
	if len(api.userApplies) != 0 {
		t.Errorf("the API was written to with no label: %v", api.userApplies)
	}
}

// Nobody having asked is the ordinary state of most accounts, not an error.
func TestStateOfAnAccountNobodyAskedForIsEmpty(t *testing.T) {
	_, k, _ := accountKeeper(t)

	st, err := k.State(context.Background(), accLabel, accUser)
	if err != nil {
		t.Fatalf("State = %v, want nil for an account nobody asked for", err)
	}
	if st.Phase != "" || st.Current {
		t.Errorf("state = %+v, want the zero value", st)
	}
}

// While the operator catches up, State must say so rather than presenting the
// previous answer as the current one.
func TestStateSaysSoWhileTheOperatorHasNotCaughtUp(t *testing.T) {
	api, k, name := accountKeeper(t)
	ctx := context.Background()
	if err := k.Ensure(ctx, accLabel, accUser); !errors.Is(err, ErrAccountPending) {
		t.Fatalf("ensure: %v", err)
	}

	st, err := k.State(ctx, accLabel, accUser)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Current {
		t.Error("State reported current with no status written yet")
	}
	if st.Action != ActionEnsure {
		t.Errorf("action = %q, want Ensure", st.Action)
	}

	api.actAsOperator(name, UserApplied, "account created")
	st, err = k.State(ctx, accLabel, accUser)
	if err != nil {
		t.Fatalf("State after apply: %v", err)
	}
	if !st.Current || st.Phase != UserApplied {
		t.Errorf("state = %+v, want Applied and current", st)
	}
}
