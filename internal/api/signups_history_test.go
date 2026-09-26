package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/audit"
	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/mail"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

func TestParseSignupDetail(t *testing.T) {
	cases := []struct{ in, email, role, ip, reason string }{
		{"email=a@b.c role=owner ip=203.0.113.9", "a@b.c", "owner", "203.0.113.9", ""},
		{"email=a@b.c", "a@b.c", "", "", ""},
		{"email=a@b.c reason=no callsign = not covered", "a@b.c", "", "", "no callsign = not covered"},
		{"", "", "", "", ""},
	}
	for _, c := range cases {
		e, r, ip, why := parseSignupDetail(c.in)
		if e != c.email || r != c.role || ip != c.ip || why != c.reason {
			t.Errorf("%q: got %q %q %q %q", c.in, e, r, ip, why)
		}
	}
}

// The decision history is the platform chain read back: only the two signup
// actions, newest first, parsed into fields, and it works with no identity
// provider configured at all.
func TestSignupHistoryReadsThePlatformChain(t *testing.T) {
	st, err := sqlite.New(":memory:", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	aud := audit.New(st)
	ctx := context.Background()
	for _, row := range [][3]string{
		{"signup_approved", "one@example.org", "email=one@example.org role=owner ip=203.0.113.9"},
		{"tenant_created", "one@example.org", "slug=one"},
		{"signup_rejected", "two@example.org", "email=two@example.org reason=Not a field team, and the address bounced"},
		{"signup_approved", "three@example.org", "email=three@example.org role=viewer ip=203.0.113.10"},
	} {
		if err := aud.Log(ctx, store.DefaultTenantID, row[0], "admin@platform.example", row[2], "198.51.100.1"); err != nil {
			t.Fatal(err)
		}
	}
	if err := aud.Log(ctx, "some-customer", "signup_approved", "x", "email=leak@example.org role=owner", ""); err != nil {
		t.Fatal(err)
	}
	h := NewSignupHandler(nil, aud) // no authentik client: history still answers
	req := httptest.NewRequest("GET", "/api/admin/signups/history", nil)
	req = req.WithContext(withUser(req.Context(), &hubauth.User{ID: "a", Email: "admin@platform.example", PlatformAdmin: true}))
	w := httptest.NewRecorder()
	h.History(w, req)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var rows []signupDecision
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows: %+v", len(rows), rows)
	}
	if rows[0].Email != "three@example.org" || rows[0].Role != "viewer" || rows[0].SignupIP != "203.0.113.10" || rows[0].Actor != "admin@platform.example" || rows[0].IP != "198.51.100.1" {
		t.Errorf("newest row: %+v", rows[0])
	}
	if rows[1].Action != "signup_rejected" || rows[1].Reason != "Not a field team, and the address bounced" || rows[1].Email != "two@example.org" {
		t.Errorf("rejected row: %+v", rows[1])
	}
	if strings.Contains(w.Body.String(), "leak@example.org") {
		t.Errorf("another tenant's chain leaked into the platform's history")
	}
	req = httptest.NewRequest("GET", "/api/admin/signups/history?limit=1", nil)
	w = httptest.NewRecorder()
	h.History(w, req)
	_ = json.Unmarshal(w.Body.Bytes(), &rows)
	if len(rows) != 1 {
		t.Errorf("limit=1 gave %d rows", len(rows))
	}
	// No audit service at all: an empty list, not an error.
	w = httptest.NewRecorder()
	NewSignupHandler(nil, nil).History(w, httptest.NewRequest("GET", "/api/admin/signups/history", nil))
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Errorf("no audit: %d %s", w.Code, w.Body.String())
	}
}

// The reason an operator gives reaches the person, escaped, and is absent
// when none was given.
func TestRejectedMailCarriesTheReasonWhenGiven(t *testing.T) {
	m := mail.Rejected("Alice", "No callsign on file & the <use> is not covered")
	if !strings.Contains(m.Text, "The reason given: No callsign on file & the <use> is not covered") {
		t.Errorf("text lacks the reason:\n%s", m.Text)
	}
	if !strings.Contains(m.HTML, "The reason given: No callsign on file &amp; the &lt;use&gt; is not covered") {
		t.Errorf("html lacks the escaped reason")
	}
	if strings.Contains(mail.Rejected("Alice", "").Text, "reason given") {
		t.Errorf("a mail with no reason mentions one")
	}
}
