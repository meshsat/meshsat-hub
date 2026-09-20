package ratelimit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// MESHSAT-1118. Everything in this package was keyed on the device id alone:
// the token bucket, the daily and monthly counters, the Redis keys and the
// admin override map, which is a package-level var.
//
// The override is the one that costs money. POST /api/ratelimit/{deviceID}/override
// is owner-gated, and RequireRole is satisfied by the owner of ANY tenant
// (internal/auth/rbac.go reads only user.Roles), so any customer could lift the
// send budget from any device id on the platform. Every satellite message that
// device then sends is billed to ITS OWN tenant's Cloudloop or Twilio account,
// because each tenant brings its own provider credentials -- so the cost lands
// on the victim, not on the tenant that removed the limit.
//
// The victim in each test below is store.DefaultTenantID on purpose. The bug
// had no tenant dimension at all, so a regression collapses to one shared
// namespace; two non-default tenants would let a regression to "the default
// tenant" pass by accident, which is the trap mutation testing caught in
// internal/deadman/tenant_test.go.

const (
	tenantA = "t_alpha"
	tenantB = "t_beta"
)

// exhaust drains the limiter for one tenant's device.
func exhaust(t *testing.T, l *DeviceLimiter, tenantID, device string) {
	t.Helper()
	for i := 0; i < 20; i++ {
		if !l.Allow(tenantID, device, false) {
			return
		}
	}
	t.Fatalf("could not exhaust the limiter for %s/%s", tenantID, device)
}

func TestOneTenantCannotSpendAnothersBudget(t *testing.T) {
	// One send per day, no refill.
	l := NewDeviceLimiter(1, 0, 1, 0, nil)
	const shared = "300000000000003"

	// Tenant B burns its own budget for that device id.
	exhaust(t, l, tenantB, shared)

	// The default tenant's identically-numbered device must still be able to
	// send: the budget is per tenant, not per id.
	if !l.Allow(store.DefaultTenantID, shared, false) {
		t.Error("one tenant's sends consumed another tenant's budget for the same device id. " +
			"On this path that is a message the paying tenant cannot send.")
	}
}

// The expensive one.
func TestATenantCannotLiftAnothersSendBudget(t *testing.T) {
	l := NewDeviceLimiter(1, 0, 1, 0, nil)
	const victimDevice = "300000000000003"

	exhaust(t, l, store.DefaultTenantID, victimDevice)

	// Tenant B sets an override naming the victim's device.
	SetOverride(tenantB, victimDevice, time.Hour)
	t.Cleanup(func() { ClearOverride(tenantB, victimDevice) })

	if l.Allow(store.DefaultTenantID, victimDevice, false) {
		t.Error("another tenant lifted the send budget from this tenant's device. " +
			"Every message it now sends is billed to the victim's own carrier account.")
	}

	// And the owning tenant's own override still works, or the fix would have
	// removed the feature rather than scoped it.
	SetOverride(store.DefaultTenantID, victimDevice, time.Hour)
	t.Cleanup(func() { ClearOverride(store.DefaultTenantID, victimDevice) })
	if !l.Allow(store.DefaultTenantID, victimDevice, false) {
		t.Error("a tenant's own override no longer exempts its own device")
	}
}

// Clearing is the same hole in reverse: removing somebody else's exemption
// silently reinstates a limit they had deliberately lifted.
func TestATenantCannotClearAnothersOverride(t *testing.T) {
	l := NewDeviceLimiter(1, 0, 1, 0, nil)
	const device = "300000000000002"

	exhaust(t, l, store.DefaultTenantID, device)
	SetOverride(store.DefaultTenantID, device, time.Hour)
	t.Cleanup(func() { ClearOverride(store.DefaultTenantID, device) })

	ClearOverride(tenantB, device)

	if !l.Allow(store.DefaultTenantID, device, false) {
		t.Error("another tenant cleared this tenant's override")
	}
}

func TestUsageListingShowsOnlyTheTenantsOwnDevices(t *testing.T) {
	l := NewDeviceLimiter(5, 0, 10, 0, nil)
	l.Allow(store.DefaultTenantID, "victim-device", false)
	l.Allow(tenantB, "other-device", false)

	got := l.AllUsage(tenantB)
	if len(got) != 1 || got[0].DeviceID != "other-device" {
		t.Fatalf("tenant B sees %v, want only its own device. The listing used to return "+
			"every device on the platform and how much traffic each was sending.", got)
	}
}

// Usage for a device id the caller does not own must read as untouched rather
// than reporting the owner's real counters.
func TestUsageDoesNotReportAnotherTenantsCounters(t *testing.T) {
	l := NewDeviceLimiter(5, 0, 10, 0, nil)
	const device = "300000000000003"
	for i := 0; i < 3; i++ {
		l.Allow(store.DefaultTenantID, device, false)
	}

	if got := l.Usage(tenantB, device).DailySent; got != 0 {
		t.Errorf("tenant B reads %d sends against another tenant's device, want 0", got)
	}
	if got := l.Usage(store.DefaultTenantID, device).DailySent; got != 3 {
		t.Errorf("the owning tenant reads %d of its own sends, want 3", got)
	}
}

// The key must be unambiguous. Without escaping, tenant "a" device "b:c" and
// tenant "a:b" device "c" produce the same key, and two unrelated tenants share
// one budget.
func TestTheKeyCannotBeForgedByAColonInADeviceID(t *testing.T) {
	if scope("a", "b:c") == scope("a:b", "c") {
		t.Error("scope() collides: a device id containing the separator lets one tenant " +
			"land on another tenant's counters")
	}
}

// The SOS bypass is above all of this and is not to be weakened: an exhausted
// limiter, for a tenant that is not the device's owner, still passes an SOS.
func TestAnSOSPassesWhateverTheTenant(t *testing.T) {
	l := NewDeviceLimiter(1, 0, 1, 0, nil)
	const device = "300000000000003"
	exhaust(t, l, store.DefaultTenantID, device)

	if !l.Allow(store.DefaultTenantID, device, true) {
		t.Error("an SOS was dropped by an exhausted limiter")
	}
	if !l.Allow(tenantB, device, true) {
		t.Error("an SOS was dropped because the tenant did not match")
	}
	if !l.Allow("", device, true) {
		t.Error("an SOS was dropped because the tenant was unknown. Nothing about billing " +
			"may stand between a distress message and the constellation.")
	}
}

// --- the API surface ---

func asTenant(method, target, body, tenantID, deviceID string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("deviceID", deviceID)
	ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
	ctx = context.WithValue(ctx, auth.TenantContextKey, tenantID)
	return r.WithContext(ctx)
}

func TestTheOverrideEndpointIsScopedToTheCaller(t *testing.T) {
	l := NewDeviceLimiter(1, 0, 1, 0, nil)
	h := NewHandler(l)
	const victimDevice = "300000000000003"
	exhaust(t, l, store.DefaultTenantID, victimDevice)

	w := httptest.NewRecorder()
	h.PostOverride(w, asTenant("POST", "/api/ratelimit/"+victimDevice+"/override",
		`{"duration_hours":24}`, tenantB, victimDevice))
	t.Cleanup(func() { ClearOverride(tenantB, victimDevice) })

	if l.Allow(store.DefaultTenantID, victimDevice, false) {
		t.Errorf("an override posted by another tenant (HTTP %d) lifted this tenant's send budget",
			w.Code)
	}
}

func TestTheUsageEndpointIsScopedToTheCaller(t *testing.T) {
	l := NewDeviceLimiter(5, 0, 10, 0, nil)
	l.Allow(store.DefaultTenantID, "victim-device", false)
	l.Allow(tenantB, "own-device", false)
	h := NewHandler(l)

	w := httptest.NewRecorder()
	h.GetAllUsage(w, asTenant("GET", "/api/ratelimit", "", tenantB, ""))

	var got []DeviceUsage
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	for _, u := range got {
		if u.DeviceID == "victim-device" {
			t.Fatalf("another tenant's device id appeared in the usage listing: %v", got)
		}
	}
}
