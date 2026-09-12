package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/quota"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// The customer's side of hosted TAK: turning it on, and deciding who sees the
// fleet on a map.

type fakeTAKProv struct {
	calls  int
	label  string
	err    error
	lastID string
}

func (f *fakeTAKProv) EnableTAK(_ context.Context, tenantID string) (string, error) {
	f.calls++
	f.lastID = tenantID
	if f.err != nil {
		return "", f.err
	}
	if f.label == "" {
		f.label = "abcdefghij"
	}
	return f.label, nil
}

type fakeTAKAccounts struct {
	ensured   []string
	deactived []string
	outcome   TAKAccountOutcome
	msg       string
	err       error
}

func (f *fakeTAKAccounts) Ensure(_ context.Context, label, username string) (TAKAccountOutcome, string, error) {
	f.ensured = append(f.ensured, label+"/"+username)
	return f.outcome, f.msg, f.err
}

func (f *fakeTAKAccounts) Deactivate(_ context.Context, label, username string) (TAKAccountOutcome, string, error) {
	f.deactived = append(f.deactived, label+"/"+username)
	return f.outcome, f.msg, f.err
}

// takHandlerOn builds the handler over a mock store, with the plan fixed so the
// ceiling is a known number (free is four accounts).
func takHandlerOn(t *testing.T, m *mockStore) *TenantTAKHandler {
	t.Helper()
	q := quota.New(m, func(context.Context, string) (string, error) { return "free", nil })
	// A nil audit service is handled: logAudit returns early, and a customer's
	// request must never fail because an audit write did.
	return NewTenantTAKHandler(m, nil, q, 8089)
}

func takRequest(method, target string, body string) (*http.Request, *httptest.ResponseRecorder) {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	req = req.WithContext(withTenant(req.Context(), "test-tenant"))
	return req, httptest.NewRecorder()
}

func TestTAKStatusSaysDisabledWhenTheTenantHasNoServer(t *testing.T) {
	m := &mockStore{} // takInstance nil -> GetTAKInstance returns ErrNotFound
	h := takHandlerOn(t, m)

	req, rec := takRequest(http.MethodGet, "/api/tenant/tak", "")
	h.Status(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out takStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Enabled {
		t.Error("enabled = true with no instance")
	}
	// The meter is still reported, so the page can say "four accounts on free"
	// before anyone turns TAK on.
	if out.Plan != "free" || out.Limit != 4 {
		t.Errorf("meter = plan %q limit %d, want free/4", out.Plan, out.Limit)
	}
}

// The in-cluster address must never reach a customer. It is a service name no
// phone can resolve, publishing it says something about the cluster's innards, and
// a customer who tried to configure it would simply fail.
func TestTAKStatusNeverLeaksTheInClusterAddress(t *testing.T) {
	m := &mockStore{takInstance: &store.TAKInstance{
		TenantID: "test-tenant", Label: "abcdefghij", State: "Running",
		Phase: "Ready", Host: "tak-abcdefghij.meshsat-tak.svc:8089",
		CACertPEM: "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n",
	}}
	h := takHandlerOn(t, m)

	req, rec := takRequest(http.MethodGet, "/api/tenant/tak", "")
	h.Status(rec, req)

	body := rec.Body.String()
	for _, leak := range []string{".svc", "meshsat-tak", "abcdefghij", "BEGIN CERTIFICATE"} {
		if strings.Contains(body, leak) {
			t.Errorf("the response carries %q, which a customer must not be shown:\n%s", leak, body)
		}
	}
	var out takStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Enabled || out.Phase != "Ready" {
		t.Errorf("enabled=%v phase=%q, want true/Ready", out.Enabled, out.Phase)
	}
	if out.Port != 8089 {
		t.Errorf("port = %d, want 8089: it is the one connection detail a customer needs", out.Port)
	}
}

func TestEnablingTAKWithoutTheMachinerySaysSo(t *testing.T) {
	h := takHandlerOn(t, &mockStore{}) // no SetTAK
	req, rec := takRequest(http.MethodPost, "/api/tenant/tak", "")
	h.Enable(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 on a Hub with no hosted TAK", rec.Code)
	}
}

func TestEnablingTAKIsAcceptedAndThenIdempotent(t *testing.T) {
	m := &mockStore{}
	h := takHandlerOn(t, m)
	prov := &fakeTAKProv{}
	h.SetTAK(prov, &fakeTAKAccounts{})

	req, rec := takRequest(http.MethodPost, "/api/tenant/tak", "")
	h.Enable(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first enable = %d, want 202", rec.Code)
	}
	if prov.calls != 1 || prov.lastID != "test-tenant" {
		t.Errorf("provisioner calls = %d for %q, want 1 for test-tenant", prov.calls, prov.lastID)
	}

	// Second time, with the row now present: still fine, and 200 rather than 202,
	// because nothing new is being built. A double-click must not read as an error.
	m.takInstance = &store.TAKInstance{TenantID: "test-tenant", Label: "abcdefghij", Phase: "Ready"}
	req, rec = takRequest(http.MethodPost, "/api/tenant/tak", "")
	h.Enable(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("second enable = %d, want 200", rec.Code)
	}
}

func TestAddingAnAccountBeforeTurningTAKOnIsRefusedClearly(t *testing.T) {
	m := &mockStore{}
	h := takHandlerOn(t, m)
	h.SetTAK(&fakeTAKProv{}, &fakeTAKAccounts{})

	req, rec := takRequest(http.MethodPost, "/api/tenant/tak/users", `{"username":"fieldteam1"}`)
	h.AddUser(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "turn TAK on") {
		t.Errorf("body = %s, want it to say what to do first", rec.Body.String())
	}
}

// The username rule is OpenTAKServer's, and the refusal names it rather than
// letting the instance answer 400 to something nobody can interpret.
func TestAnIllegalUsernameIsRefusedBeforeTheOperatorIsAsked(t *testing.T) {
	m := &mockStore{takInstance: &store.TAKInstance{TenantID: "test-tenant", Label: "abcdefghij"}}
	h := takHandlerOn(t, m)
	accts := &fakeTAKAccounts{}
	h.SetTAK(&fakeTAKProv{}, accts)

	for _, bad := range []string{"field-team-1", "ab", "FieldTeam1", "field team"} {
		req, rec := takRequest(http.MethodPost, "/api/tenant/tak/users",
			`{"username":"`+bad+`"}`)
		h.AddUser(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%q gave %d, want 400", bad, rec.Code)
		}
	}
	if len(accts.ensured) != 0 {
		t.Errorf("the operator was asked for an illegal name: %v", accts.ensured)
	}
}

// The ceiling gates ADDING an account, and it must gate before the operator is
// asked -- otherwise a tenant over its plan still gets an account created on the
// instance and only the Hub's row is refused.
func TestTheAccountCeilingRefusesWithPaymentRequiredAndAsksNobody(t *testing.T) {
	m := &mockStore{
		takInstance: &store.TAKInstance{TenantID: "test-tenant", Label: "abcdefghij"},
		takUsers:    4, // free allows four
	}
	h := takHandlerOn(t, m)
	accts := &fakeTAKAccounts{}
	h.SetTAK(&fakeTAKProv{}, accts)

	req, rec := takRequest(http.MethodPost, "/api/tenant/tak/users", `{"username":"fieldteam5"}`)
	h.AddUser(rec, req)

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", rec.Code)
	}
	if len(accts.ensured) != 0 {
		t.Errorf("the operator was asked despite the ceiling: %v; the instance would then have "+
			"an account the Hub refuses to record", accts.ensured)
	}
	if !strings.Contains(rec.Body.String(), "plan") {
		t.Errorf("body = %s, want it to explain the plan ceiling", rec.Body.String())
	}
}

func TestAddingAnAccountRecordsItAndReportsProvisioning(t *testing.T) {
	m := &mockStore{takInstance: &store.TAKInstance{TenantID: "test-tenant", Label: "abcdefghij"}}
	h := takHandlerOn(t, m)
	accts := &fakeTAKAccounts{outcome: TAKAccountPending}
	h.SetTAK(&fakeTAKProv{}, accts)

	req, rec := takRequest(http.MethodPost, "/api/tenant/tak/users",
		`{"username":"fieldteam1","callsign":"FIELD 1"}`)
	h.AddUser(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 while the operator catches up: %s", rec.Code, rec.Body.String())
	}
	if len(accts.ensured) != 1 || accts.ensured[0] != "abcdefghij/fieldteam1" {
		t.Errorf("ensured = %v, want the instance label and username", accts.ensured)
	}
	if m.createdTAKUser == nil {
		t.Fatal("no tak_users row was recorded, so the Hub's authorizer would refuse the phone")
	}
	if !m.createdTAKUser.Active || m.createdTAKUser.Username != "fieldteam1" {
		t.Errorf("recorded %+v, want an active fieldteam1", m.createdTAKUser)
	}
	if m.createdTAKUser.Callsign != "FIELD 1" {
		t.Errorf("callsign = %q, want it kept", m.createdTAKUser.Callsign)
	}
}

func TestAddingAnAccountThatAlreadyExistsIsNotAnError(t *testing.T) {
	m := &mockStore{
		takInstance: &store.TAKInstance{TenantID: "test-tenant", Label: "abcdefghij"},
		takUser:     &store.TAKUser{TenantID: "test-tenant", Username: "fieldteam1", Active: true},
	}
	h := takHandlerOn(t, m)
	accts := &fakeTAKAccounts{outcome: TAKAccountApplied}
	h.SetTAK(&fakeTAKProv{}, accts)

	req, rec := takRequest(http.MethodPost, "/api/tenant/tak/users", `{"username":"fieldteam1"}`)
	h.AddUser(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for an account already there", rec.Code)
	}
	if m.createdTAKUser != nil {
		t.Error("a second row was created for an existing account")
	}
}

// Re-adding somebody who was removed cannot work yet: OpenTAKServer's activate
// route is unverified, so the endpoint says so instead of appearing to succeed and
// leaving them unable to connect.
func TestReAddingARemovedAccountIsRefusedWithTheReason(t *testing.T) {
	m := &mockStore{
		takInstance: &store.TAKInstance{TenantID: "test-tenant", Label: "abcdefghij"},
		takUser:     &store.TAKUser{TenantID: "test-tenant", Username: "fieldteam1", Active: false},
	}
	h := takHandlerOn(t, m)
	h.SetTAK(&fakeTAKProv{}, &fakeTAKAccounts{outcome: TAKAccountApplied})

	req, rec := takRequest(http.MethodPost, "/api/tenant/tak/users", `{"username":"fieldteam1"}`)
	h.AddUser(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "different username") {
		t.Errorf("body = %s, want it to say what the customer can do instead", rec.Body.String())
	}
}

func TestAnOperatorRefusalIsReportedWithItsReason(t *testing.T) {
	m := &mockStore{takInstance: &store.TAKInstance{TenantID: "test-tenant", Label: "abcdefghij"}}
	h := takHandlerOn(t, m)
	h.SetTAK(&fakeTAKProv{}, &fakeTAKAccounts{
		outcome: TAKAccountRefused,
		msg:     "the administrator account cannot be managed through this API",
	})

	req, rec := takRequest(http.MethodPost, "/api/tenant/tak/users", `{"username":"administrator"}`)
	h.AddUser(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "administrator") {
		t.Errorf("body = %s, want the operator's reason", rec.Body.String())
	}
	if m.createdTAKUser != nil {
		t.Error("a refused account was still recorded")
	}
}

// Removing a teammate marks the row inactive. That is what actually stops the
// phone: the Hub's own authorizer reads this row and refuses the connection before
// the instance is reached, so it takes effect immediately whatever the operator
// has got to.
func TestRemovingAnAccountMarksTheRowInactiveRatherThanDeletingIt(t *testing.T) {
	m := &mockStore{
		takInstance: &store.TAKInstance{TenantID: "test-tenant", Label: "abcdefghij"},
		takUser:     &store.TAKUser{TenantID: "test-tenant", Username: "fieldteam1", Active: true},
	}
	h := takHandlerOn(t, m)
	accts := &fakeTAKAccounts{outcome: TAKAccountApplied}
	h.SetTAK(&fakeTAKProv{}, accts)

	req, rec := takRequest(http.MethodDelete, "/api/tenant/tak/users/fieldteam1", "")
	req = withChiURLParam(req, "username", "fieldteam1")
	req = req.WithContext(withTenant(req.Context(), "test-tenant"))
	h.RemoveUser(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(accts.deactived) != 1 {
		t.Errorf("deactivated = %v, want one call", accts.deactived)
	}
	if m.updatedTAKUser == nil {
		t.Fatal("the row was not updated, so the Hub would keep admitting the phone")
	}
	if m.updatedTAKUser.Active {
		t.Error("the row is still active after a removal")
	}
}

func TestRemovingAnAccountThatIsNotThereIs404(t *testing.T) {
	m := &mockStore{takInstance: &store.TAKInstance{TenantID: "test-tenant", Label: "abcdefghij"}}
	h := takHandlerOn(t, m)
	h.SetTAK(&fakeTAKProv{}, &fakeTAKAccounts{})

	req, rec := takRequest(http.MethodDelete, "/api/tenant/tak/users/nobody", "")
	req = withChiURLParam(req, "username", "nobody")
	req = req.WithContext(withTenant(req.Context(), "test-tenant"))
	h.RemoveUser(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestListingAccountsNeverCarriesAnEnrollmentToken(t *testing.T) {
	m := &mockStore{takUserList: []*store.TAKUser{
		{
			TenantID: "test-tenant", Username: "fieldteam1", Callsign: "FIELD 1",
			Active: true, CertSerial: "4242",
			// Set deliberately: it must not appear in the response.
			EnrollTokenHash: "e3b0c44298fc1c149afbf4c8996fb924",
		},
	}}
	h := takHandlerOn(t, m)

	req, rec := takRequest(http.MethodGet, "/api/tenant/tak/users", "")
	h.ListUsers(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "e3b0c442") {
		t.Errorf("the enrollment token hash reached the response:\n%s", rec.Body.String())
	}
	var out []takUserResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 || out[0].Username != "fieldteam1" || out[0].CertSerial != "4242" {
		t.Errorf("listed %+v, want the one account with its serial", out)
	}
}

// The role gate, exercised through the real RequireRole and the routes as main.go
// mounts them -- so this tests the wiring, not only the handler.
func TestOnlyAnOwnerMayChangeTAK(t *testing.T) {
	m := &mockStore{takInstance: &store.TAKInstance{TenantID: "test-tenant", Label: "abcdefghij"}}
	h := takHandlerOn(t, m)
	h.SetTAK(&fakeTAKProv{}, &fakeTAKAccounts{outcome: TAKAccountPending})

	r := chi.NewRouter()
	r.Route("/api/tenant", func(r chi.Router) {
		r.With(hubauth.RequireRole(hubauth.RoleViewer)).Get("/tak", h.Status)
		r.With(hubauth.RequireRole(hubauth.RoleOwner)).Post("/tak", h.Enable)
		r.With(hubauth.RequireRole(hubauth.RoleViewer)).Get("/tak/users", h.ListUsers)
		r.With(hubauth.RequireRole(hubauth.RoleOwner)).Post("/tak/users", h.AddUser)
		r.With(hubauth.RequireRole(hubauth.RoleOwner)).Delete("/tak/users/{username}", h.RemoveUser)
	})

	cases := []struct {
		name       string
		role       string
		method     string
		target     string
		body       string
		wantDenied bool
	}{
		{"a viewer may look", hubauth.RoleViewer, http.MethodGet, "/api/tenant/tak", "", false},
		{"an operator may not turn it on", hubauth.RoleOperator, http.MethodPost, "/api/tenant/tak", "", true},
		{"an owner may turn it on", hubauth.RoleOwner, http.MethodPost, "/api/tenant/tak", "", false},
		{"an operator may not add an account", hubauth.RoleOperator, http.MethodPost,
			"/api/tenant/tak/users", `{"username":"fieldteam1"}`, true},
		{"an operator may not remove one", hubauth.RoleOperator, http.MethodDelete,
			"/api/tenant/tak/users/fieldteam1", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req *http.Request
			if tc.body == "" {
				req = httptest.NewRequest(tc.method, tc.target, nil)
			} else {
				req = httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
			}
			ctx := withTenant(req.Context(), "test-tenant")
			ctx = withUser(ctx, &hubauth.User{
				ID: "u1", Email: "someone@example.test",
				Roles: []string{tc.role}, TenantID: "test-tenant",
			})
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req.WithContext(ctx))

			denied := rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized
			if denied != tc.wantDenied {
				t.Errorf("status = %d (denied=%v), want denied=%v", rec.Code, denied, tc.wantDenied)
			}
		})
	}
}
