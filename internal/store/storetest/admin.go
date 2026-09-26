package storetest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// testTenantSummaries: the platform directory's counts land on the tenant
// that owns the rows, and the DEFAULT tenant is the victim, deliberately: a
// query that forgot its join key would pile everything onto it.
func testTenantSummaries(t *testing.T, s store.Store) {
	ctx := context.Background()
	mk := func(id, owner string) {
		if err := s.CreateTenant(ctx, &store.Tenant{ID: id, Slug: id, Name: "Tenant " + id, Plan: "free", Status: store.TenantActive, OwnerUserID: owner}); err != nil {
			t.Fatalf("create tenant %s: %v", id, err)
		}
	}
	if _, err := s.GetTenant(ctx, store.DefaultTenantID); err != nil {
		mk(store.DefaultTenantID, "")
	}
	mk("sum-a", "ua")
	mk("sum-b", "") // owner_user_id never filled: falls back to the owner-role user
	users := []struct{ tenant, id, email, role string }{
		{"sum-a", "ua", "alice@example.org", "owner"}, {"sum-a", "ua2", "al2@example.org", "viewer"},
		{"sum-b", "ub", "bob@example.org", "owner"},
	}
	for _, u := range users {
		if err := s.CreateUser(ctx, u.tenant, &store.LocalUser{ID: u.id, Email: u.email, Role: u.role, PasswordHash: "x", Enabled: true}); err != nil {
			t.Fatalf("user %s: %v", u.id, err)
		}
	}
	for i, tn := range []string{"sum-a", "sum-a", "sum-b"} {
		if err := s.CreateDevice(ctx, tn, &store.Device{IMEI: "sum-imei-" + string(rune('0'+i)), Label: tn}); err != nil {
			t.Fatalf("device: %v", err)
		}
	}
	if err := s.CreateOrUpdateBridge(ctx, "sum-a", &store.Bridge{BridgeID: "sum-bridge-a", Label: "a"}); err != nil {
		t.Fatalf("bridge: %v", err)
	}
	rows, err := s.ListTenantSummaries(ctx)
	if err != nil {
		t.Fatalf("summaries: %v", err)
	}
	got := map[string]store.TenantSummary{}
	for _, r := range rows {
		got[r.ID] = r
	}
	a, b, def := got["sum-a"], got["sum-b"], got[store.DefaultTenantID]
	if a.OwnerEmail != "alice@example.org" || a.Users != 2 || a.Devices != 2 || a.Bridges != 1 {
		t.Errorf("sum-a: owner=%q users=%d devices=%d bridges=%d", a.OwnerEmail, a.Users, a.Devices, a.Bridges)
	}
	if b.OwnerEmail != "bob@example.org" || b.Users != 1 || b.Devices != 1 || b.Bridges != 0 {
		t.Errorf("sum-b: owner=%q users=%d devices=%d bridges=%d (owner from the role fallback)", b.OwnerEmail, b.Users, b.Devices, b.Bridges)
	}
	if def.Devices != 0 || def.Bridges != 0 || def.Users != 0 {
		t.Errorf("the DEFAULT tenant was credited with other tenants' rows: %+v", def)
	}
}

// testAuditEntriesByAction: the filter returns the asked actions of the
// asked tenant only, newest first, limit honoured.
func testAuditEntriesByAction(t *testing.T, s store.Store) {
	ctx := context.Background()
	write := func(tenantID, action string) {
		err := s.AppendAuditEntry(ctx, tenantID, func(prev *store.AuditEntry) (*store.AuditEntry, error) {
			e := &store.AuditEntry{ID: tenantID + "-" + action + "-" + time.Now().UTC().Format("150405.000000000"), Action: action, Actor: "op", Detail: "d", CreatedAt: time.Now().UTC(), HashVersion: 2}
			if prev != nil {
				e.PrevHash = prev.Hash
				if !e.CreatedAt.After(prev.CreatedAt) {
					e.CreatedAt = prev.CreatedAt.Add(time.Microsecond)
				}
			}
			e.Hash = e.ID // any distinct string: the chain is not verified here
			return e, nil
		})
		if err != nil {
			t.Fatalf("append %s/%s: %v", tenantID, action, err)
		}
	}
	for _, a := range []string{"signup_approved", "noise", "signup_rejected", "signup_approved"} {
		write(store.DefaultTenantID, a)
	}
	write("byaction-other", "signup_approved")

	got, err := s.ListAuditEntriesByAction(ctx, store.DefaultTenantID, []string{"signup_approved", "signup_rejected"}, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3 (two approved, one rejected, no noise, nothing of the other tenant)", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].CreatedAt.After(got[i-1].CreatedAt) {
			t.Errorf("not newest first at %d", i)
		}
	}
	if got[0].Action != "signup_approved" {
		t.Errorf("newest is %q, want the last approval", got[0].Action)
	}
	if lim, _ := s.ListAuditEntriesByAction(ctx, store.DefaultTenantID, []string{"signup_approved"}, 1); len(lim) != 1 {
		t.Errorf("limit 1 returned %d", len(lim))
	}
	if none, _ := s.ListAuditEntriesByAction(ctx, store.DefaultTenantID, nil, 10); len(none) != 0 {
		t.Errorf("no actions asked, got %d", len(none))
	}
}

// testSupportGrants: one active grant per tenant, revoked and expired ones
// are invisible, opening and failures are recorded, and a grant is the
// tenant's own.
func testSupportGrants(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, id := range []string{"grant-a", "grant-b"} {
		if err := s.CreateTenant(ctx, &store.Tenant{ID: id, Slug: id, Name: id, Plan: "free", Status: store.TenantActive}); err != nil {
			t.Fatalf("tenant: %v", err)
		}
	}
	if _, err := s.GetActiveSupportGrant(ctx, "grant-a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no grant yet: err=%v, want ErrNotFound", err)
	}
	now := time.Now().UTC()
	first := &store.SupportGrant{PINHash: "h1", CreatedByEmail: "owner@a", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := s.CreateSupportGrant(ctx, "grant-a", first); err != nil {
		t.Fatalf("create: %v", err)
	}
	second := &store.SupportGrant{PINHash: "h2", CreatedByEmail: "owner@a", CreatedAt: now.Add(time.Second), ExpiresAt: now.Add(2 * time.Hour)}
	if err := s.CreateSupportGrant(ctx, "grant-a", second); err != nil {
		t.Fatalf("create second: %v", err)
	}
	g, err := s.GetActiveSupportGrant(ctx, "grant-a")
	if err != nil || g.ID != second.ID || g.PINHash != "h2" {
		t.Fatalf("active grant = %+v, %v; want the second (the first is revoked by it)", g, err)
	}
	if g.TenantID != "grant-a" || g.Opened() || !g.Active(time.Now()) {
		t.Errorf("fresh grant: tenant=%q opened=%v active=%v", g.TenantID, g.Opened(), g.Active(time.Now()))
	}
	if _, err := s.GetActiveSupportGrant(ctx, "grant-b"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a grant for grant-a is visible on grant-b: err=%v", err)
	}
	// Failures count, opening sticks to the first opener.
	if n, err := s.BumpSupportGrantFailures(ctx, "grant-a", g.ID); err != nil || n != 1 {
		t.Fatalf("bump: %d %v", n, err)
	}
	if n, _ := s.BumpSupportGrantFailures(ctx, "grant-a", g.ID); n != 2 {
		t.Fatalf("second bump: %d", n)
	}
	if _, err := s.BumpSupportGrantFailures(ctx, "grant-b", g.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bumping through the wrong tenant: err=%v, want ErrNotFound", err)
	}
	opened := now.Add(2 * time.Second)
	if err := s.MarkSupportGrantUsed(ctx, "grant-a", g.ID, "op@platform", opened); err != nil {
		t.Fatalf("mark used: %v", err)
	}
	if err := s.MarkSupportGrantUsed(ctx, "grant-a", g.ID, "someone-else", opened.Add(time.Minute)); err != nil {
		t.Fatalf("second mark used: %v", err)
	}
	g, _ = s.GetActiveSupportGrant(ctx, "grant-a")
	if !g.Opened() || g.UsedByEmail != "op@platform" || g.FailedAttempts != 2 {
		t.Errorf("after open: opened=%v by=%q failures=%d", g.Opened(), g.UsedByEmail, g.FailedAttempts)
	}
	if err := s.MarkSupportGrantUsed(ctx, "grant-b", g.ID, "x", opened); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("mark used through the wrong tenant: err=%v", err)
	}
	// Revoke ends it; an expired one never shows.
	if err := s.RevokeSupportGrant(ctx, "grant-a", g.ID, time.Now().UTC()); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := s.GetActiveSupportGrant(ctx, "grant-a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoked grant still active: err=%v", err)
	}
	expired := &store.SupportGrant{PINHash: "h3", CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)}
	if err := s.CreateSupportGrant(ctx, "grant-a", expired); err != nil {
		t.Fatalf("create expired: %v", err)
	}
	if _, err := s.GetActiveSupportGrant(ctx, "grant-a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired grant reported active: err=%v", err)
	}
	// The PIN hash never leaves in an export.
	exp, err := s.ExportTenant(ctx, "grant-a")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	for _, row := range exp["support_grants"] {
		if v, _ := row["pin_hash"].(string); v == "h1" || v == "h2" || v == "h3" || !strings.Contains(v, "redacted") {
			t.Errorf("export carries a pin hash: %q", v)
		}
	}
	if len(exp["support_grants"]) == 0 {
		t.Errorf("support_grants missing from the export: the table is not tenant-scoped in the catalogue")
	}
}
