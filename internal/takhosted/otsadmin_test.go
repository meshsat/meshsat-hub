package takhosted

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeOTS is enough of OpenTAKServer's admin surface to drive OTSAdmin: the
// flask-security login shape, and the /api/user/ routes behind a token.
type fakeOTS struct {
	mu sync.Mutex
	// adminPassword is what the instance will accept for `administrator`.
	adminPassword string
	// noLoginRoute reproduces the original gateway defect: /api/user/ proxied,
	// /api/login not, so the admin slice is reachable and unusable.
	noLoginRoute bool
	// loginWithoutToken answers 200 with no authentication_token.
	loginWithoutToken bool
	// existing names that /api/user/add will refuse as duplicates.
	existing map[string]bool

	paths   []string
	tokens  []string
	lastAdd map[string]string
}

func newFakeOTS(pw string) *fakeOTS {
	return &fakeOTS{adminPassword: pw, existing: map[string]bool{}}
}

func (f *fakeOTS) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/login", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.paths = append(f.paths, r.URL.Path)
		if f.noLoginRoute {
			// nginx's default-deny answers 404 for a path it does not proxy.
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<html><body>404 Not Found</body></html>`))
			return
		}
		var in struct{ Username, Password string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		w.Header().Set("Content-Type", "application/json")
		if in.Username != "administrator" || in.Password != f.adminPassword {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"response":{"errors":{"password":["Invalid password"]}}}`))
			return
		}
		if f.loginWithoutToken {
			_, _ = w.Write([]byte(`{"response":{"user":{}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"response":{"user":{"authentication_token":"tok-abc"}}}`))
	})

	mux.HandleFunc("/api/user/add", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.paths = append(f.paths, r.URL.Path)
		f.tokens = append(f.tokens, r.Header.Get("Authentication-Token"))
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.lastAdd = in
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authentication-Token") != "tok-abc" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"success":false,"error":"administrator role required"}`))
			return
		}
		if f.existing[in["username"]] {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"success":false,"error":"User ` + in["username"] + ` already exists"}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	})

	mux.HandleFunc("/api/user/deactivate", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.paths = append(f.paths, r.URL.Path)
		f.tokens = append(f.tokens, r.Header.Get("Authentication-Token"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	})

	mux.HandleFunc("/api/user/password/reset", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.paths = append(f.paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	})

	return mux
}

func (f *fakeOTS) sawPath(p string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, got := range f.paths {
		if got == p {
			return true
		}
	}
	return false
}

func (f *fakeOTS) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.paths)
}

// newAdmin wires an OTSAdmin at a test server, with the administrator password
// supplied the way the operator's config Secret would supply it.
func newAdmin(t *testing.T, f *fakeOTS, pw string) (*OTSAdmin, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	dial := func(context.Context, string, string) (*http.Client, string, error) {
		return srv.Client(), srv.URL, nil
	}
	pass := func(context.Context, string, string) (string, error) { return pw, nil }
	return NewOTSAdmin(dial, pass), srv
}

// The whole reason this type exists.
//
// A certificate is necessary and NOT sufficient: EudHandlerSSL looks the
// certificate's common name up as an OTS user and drops the connection on "User
// <x> does not exist". So creating the account is what actually admits a phone,
// and it goes through login-then-add because every /api/user/ route is gated on
// @roles_accepted("administrator").
func TestEnsureUserLogsInThenCreatesTheAccount(t *testing.T) {
	f := newFakeOTS("s3cret-generated")
	a, _ := newAdmin(t, f, "s3cret-generated")

	if err := a.EnsureUser(context.Background(), "t1", "abcdefghij", "gatephone", "PhonePass12345"); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	if !f.sawPath("/api/login") {
		t.Error("no login was attempted; /api/user/ needs an administrator token")
	}
	if !f.sawPath("/api/user/add") {
		t.Error("the account was never created")
	}
	f.mu.Lock()
	add, tokens := f.lastAdd, f.tokens
	f.mu.Unlock()
	if add["username"] != "gatephone" {
		t.Errorf("created %q, want gatephone", add["username"])
	}
	// Upstream compares password and confirm_password and refuses a mismatch.
	if add["password"] == "" || add["password"] != add["confirm_password"] {
		t.Errorf("password/confirm_password disagree: %q vs %q", add["password"], add["confirm_password"])
	}
	if len(tokens) == 0 || tokens[0] != "tok-abc" {
		t.Errorf("the session token was not carried on /api/user/add: %v", tokens)
	}
}

// An account that already exists is success from the caller's point of view: the
// intent is "this person can connect", not "I just inserted a row". The Hub's own
// tak_users table is the record; this reconciles the instance to it.
func TestAnExistingAccountIsNotAFailure(t *testing.T) {
	f := newFakeOTS("pw")
	f.existing["gatephone"] = true
	a, _ := newAdmin(t, f, "pw")

	err := a.EnsureUser(context.Background(), "t1", "abcdefghij", "gatephone", "PhonePass12345")
	if !errors.Is(err, ErrUserExists) {
		t.Errorf("got %v, want ErrUserExists so the caller can treat it as done", err)
	}
}

// A hyphen is refused BEFORE any request, because OpenTAKServer answers a bare
// HTTP 400 for it and "field-team-1 is not a legal TAK username" is not something
// a caller should have to infer from that.
func TestAnIllegalUsernameIsRefusedWithoutContactingTheInstance(t *testing.T) {
	f := newFakeOTS("pw")
	a, _ := newAdmin(t, f, "pw")

	for _, bad := range []string{"field-team-1", "Alice", "ab", "has space", strings.Repeat("x", 33), ""} {
		err := a.EnsureUser(context.Background(), "t1", "abcdefghij", bad, "PhonePass12345")
		if !errors.Is(err, ErrBadUsername) {
			t.Errorf("username %q gave %v, want ErrBadUsername", bad, err)
		}
	}
	if n := f.requests(); n != 0 {
		t.Errorf("%d requests were made for usernames that could never be accepted", n)
	}
}

// The defect the gateway actually shipped with: /api/user/ proxied and /api/login
// not, so the admin slice was reachable and unusable. The error has to name that,
// or the next person debugs the wrong layer.
func TestAGatewayWithNoLoginRouteSaysSo(t *testing.T) {
	f := newFakeOTS("pw")
	f.noLoginRoute = true
	a, _ := newAdmin(t, f, "pw")

	err := a.EnsureUser(context.Background(), "t1", "abcdefghij", "gatephone", "PhonePass12345")
	if err == nil {
		t.Fatal("a gateway that does not proxy /api/login produced no error")
	}
	if !strings.Contains(err.Error(), "/api/login") {
		t.Errorf("error %q does not name the unproxied path", err)
	}
	if f.sawPath("/api/user/add") {
		t.Error("it tried to create a user without a token")
	}
}

// A wrong administrator password is its own error: on a fresh instance it usually
// means the operator has not yet replaced OpenTAKServer's default, which it does
// once the pod is Ready.
func TestAWrongAdministratorPasswordIsDistinguishable(t *testing.T) {
	f := newFakeOTS("the-real-one")
	a, _ := newAdmin(t, f, "the-wrong-one")

	err := a.EnsureUser(context.Background(), "t1", "abcdefghij", "gatephone", "PhonePass12345")
	if !errors.Is(err, ErrAdminLogin) {
		t.Errorf("got %v, want ErrAdminLogin", err)
	}
}

// A 200 with no token is not a success. Treating it as one would send an
// unauthenticated request to /api/user/add and fail further along, where the cause
// is harder to see.
func TestALoginWithoutATokenIsRefused(t *testing.T) {
	f := newFakeOTS("pw")
	f.loginWithoutToken = true
	a, _ := newAdmin(t, f, "pw")

	err := a.EnsureUser(context.Background(), "t1", "abcdefghij", "gatephone", "PhonePass12345")
	if !errors.Is(err, ErrAdminLogin) {
		t.Errorf("got %v, want ErrAdminLogin", err)
	}
	if f.sawPath("/api/user/add") {
		t.Error("it proceeded without a token")
	}
}

// Deactivation, not deletion: the EUD rows, the position history and the audit
// trail all reference the user, and removing a teammate is not a request to
// rewrite the map's history.
func TestDeactivateSwitchesTheAccountOffRatherThanDeletingIt(t *testing.T) {
	f := newFakeOTS("pw")
	a, _ := newAdmin(t, f, "pw")

	if err := a.DeactivateUser(context.Background(), "t1", "abcdefghij", "gatephone"); err != nil {
		t.Fatalf("DeactivateUser: %v", err)
	}
	if !f.sawPath("/api/user/deactivate") {
		t.Error("deactivate was not called")
	}
	if f.sawPath("/api/user/delete") {
		t.Error("it deleted the account; deactivation keeps the history")
	}
	f.mu.Lock()
	tokens := f.tokens
	f.mu.Unlock()
	if len(tokens) == 0 || tokens[len(tokens)-1] != "tok-abc" {
		t.Errorf("the session token was not carried: %v", tokens)
	}
}

// OpenTAKServer refuses ':' and '@' in a password. Catching it here names the
// rule; leaving it to the API produces a 400 whose body nobody reads.
func TestAPasswordOpenTAKServerWouldRefuseIsCaughtLocally(t *testing.T) {
	f := newFakeOTS("pw")
	a, _ := newAdmin(t, f, "pw")

	for _, bad := range []string{"has:colon", "has@at"} {
		err := a.ResetUserPassword(context.Background(), "t1", "abcdefghij", "gatephone", bad)
		if err == nil {
			t.Errorf("password %q was accepted", bad)
			continue
		}
		if !strings.Contains(err.Error(), "':'") && !strings.Contains(err.Error(), "'@'") {
			t.Errorf("error %q does not name the refused characters", err)
		}
	}
	if n := f.requests(); n != 0 {
		t.Errorf("%d requests were made for passwords that could never be accepted", n)
	}
}

// A successful reset goes through login and hits the admin reset route.
func TestResetPasswordUsesTheAdminRoute(t *testing.T) {
	f := newFakeOTS("pw")
	a, _ := newAdmin(t, f, "pw")

	if err := a.ResetUserPassword(context.Background(), "t1", "abcdefghij", "gatephone", "NewPass123456"); err != nil {
		t.Fatalf("ResetUserPassword: %v", err)
	}
	if !f.sawPath("/api/user/password/reset") {
		t.Error("the admin reset route was not used")
	}
}
