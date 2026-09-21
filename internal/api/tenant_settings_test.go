package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// MESHSAT-1117. HUB_BRIDGE_OFFLINE_TIMEOUT decided, for every tenant on the
// platform at once, how long a bridge may go quiet before it is shown offline.
// That is a property of how a particular fleet operates -- a vehicle on a city
// network and a kit on a satellite schedule do not want the same number -- and
// changing it meant editing a ConfigMap.
//
// It is a tenant setting now, with the environment variable as the default and
// the platform bounding what an owner may choose. These tests pin the three
// things that make that safe: the default still applies to a tenant that has
// chosen nothing, the bounds are enforced, and one tenant's choice does not
// reach another's.

const (
	tsTenant = "t_alpha"
	tsOther  = store.DefaultTenantID
)

// tsStore is a mockStore that actually keeps more than one tenant and honours
// an update. The shared mock holds a single tenant and its UpdateTenant is a
// no-op, which would make "one tenant's setting does not reach another" pass
// for the wrong reason.
type tsStore struct {
	*mockStore
	tenants map[string]*store.Tenant
}

func (s *tsStore) GetTenant(_ context.Context, id string) (*store.Tenant, error) {
	t, ok := s.tenants[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *t
	return &cp, nil
}

func (s *tsStore) UpdateTenant(_ context.Context, t *store.Tenant) error {
	if _, ok := s.tenants[t.ID]; !ok {
		return store.ErrNotFound
	}
	cp := *t
	s.tenants[t.ID] = &cp
	return nil
}

func tsHandler(t *testing.T) (*TenantHandler, *tsStore) {
	t.Helper()
	st := &tsStore{mockStore: &mockStore{}, tenants: map[string]*store.Tenant{}}
	for _, id := range []string{tsTenant, tsOther} {
		st.tenants[id] = &store.Tenant{ID: id, Slug: id, Name: id}
	}
	h := NewTenantHandler(st)
	h.SetBridgeOfflineTimeoutPolicy(300, 60, 86400)
	h.SetAuditRetentionPolicy(90, 30, 3650)
	return h, st
}

func tsRequest(method, body, tenantID string) (*http.Request, *httptest.ResponseRecorder) {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/api/tenant", nil)
	} else {
		r = httptest.NewRequest(method, "/api/tenant", strings.NewReader(body))
	}
	return r.WithContext(withTenant(r.Context(), tenantID)), httptest.NewRecorder()
}

// A tenant that has chosen nothing must read as 0 and be told the platform
// default, so the UI can show the number actually in force instead of one it
// hardcoded.
func TestATenantThatHasChosenNothingGetsThePlatformDefault(t *testing.T) {
	h, _ := tsHandler(t)

	r, w := tsRequest("GET", "", tsTenant)
	h.Get(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	var got tenantResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.BridgeOfflineTimeout != 0 {
		t.Errorf("bridge_offline_timeout = %d, want 0 (meaning: use the default)", got.BridgeOfflineTimeout)
	}
	if got.BridgeOfflineTimeoutDefault != 300 {
		t.Errorf("the response does not carry the platform default (%d); the UI would have "+
			"to hardcode a number that can drift from the ConfigMap", got.BridgeOfflineTimeoutDefault)
	}
	if got.BridgeOfflineTimeoutMin != 60 || got.BridgeOfflineTimeoutMax != 86400 {
		t.Errorf("bounds = %d..%d, want 60..86400", got.BridgeOfflineTimeoutMin, got.BridgeOfflineTimeoutMax)
	}
}

func TestAnOwnerCanSetAndClearTheTimeout(t *testing.T) {
	h, st := tsHandler(t)

	r, w := tsRequest("PUT", `{"name":"Alpha","bridge_offline_timeout":900}`, tsTenant)
	h.Update(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("set: %d %s", w.Code, w.Body.String())
	}
	tn, _ := st.GetTenant(context.Background(), tsTenant)
	if tn.BridgeOfflineTimeout != 900 {
		t.Fatalf("stored %d, want 900", tn.BridgeOfflineTimeout)
	}

	// 0 puts it back on the platform default.
	r, w = tsRequest("PUT", `{"name":"Alpha","bridge_offline_timeout":0}`, tsTenant)
	h.Update(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", w.Code, w.Body.String())
	}
	tn, _ = st.GetTenant(context.Background(), tsTenant)
	if tn.BridgeOfflineTimeout != 0 {
		t.Errorf("stored %d after clearing, want 0", tn.BridgeOfflineTimeout)
	}
}

// The value the owner chose has to come back out again, or the form reopens
// showing the platform default and the next save silently discards it.
func TestTheChosenTimeoutIsReturned(t *testing.T) {
	h, _ := tsHandler(t)

	r, w := tsRequest("PUT", `{"name":"Alpha","bridge_offline_timeout":900}`, tsTenant)
	h.Update(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("set: %d %s", w.Code, w.Body.String())
	}
	var put tenantResponse
	if err := json.Unmarshal(w.Body.Bytes(), &put); err != nil {
		t.Fatalf("decode put: %v", err)
	}
	if put.BridgeOfflineTimeout != 900 {
		t.Errorf("the update replied with %d, want 900", put.BridgeOfflineTimeout)
	}

	r, w = tsRequest("GET", "", tsTenant)
	h.Get(w, r)
	var got tenantResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if got.BridgeOfflineTimeout != 900 {
		t.Errorf("reading the tenant back gives %d, want 900. The form would reopen on the "+
			"platform default and the next save would discard the owner's choice.",
			got.BridgeOfflineTimeout)
	}
}

// The field is a pointer for a reason: renaming the tenant must not silently
// reset a timeout the owner chose.
func TestRenamingTheTenantLeavesTheTimeoutAlone(t *testing.T) {
	h, st := tsHandler(t)

	r, w := tsRequest("PUT", `{"name":"Alpha","bridge_offline_timeout":900}`, tsTenant)
	h.Update(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("set: %d %s", w.Code, w.Body.String())
	}

	// A name-only update, which is exactly what the old UI sent.
	r, w = tsRequest("PUT", `{"name":"Alpha Renamed"}`, tsTenant)
	h.Update(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("rename: %d %s", w.Code, w.Body.String())
	}
	tn, _ := st.GetTenant(context.Background(), tsTenant)
	if tn.BridgeOfflineTimeout != 900 {
		t.Errorf("the timeout is %d after a name-only save, want 900. A plain int field "+
			"would have reset it to the default on every rename.", tn.BridgeOfflineTimeout)
	}
	if tn.Name != "Alpha Renamed" {
		t.Errorf("name = %q", tn.Name)
	}
}

func TestTheTimeoutIsBoundedByThePlatform(t *testing.T) {
	h, st := tsHandler(t)

	for _, tc := range []struct {
		name string
		secs int
	}{
		{"below the floor", 5},
		{"above the ceiling", 999999},
		{"negative", -1},
	} {
		r, w := tsRequest("PUT", `{"name":"Alpha","bridge_offline_timeout":`+itoa(tc.secs)+`}`, tsTenant)
		h.Update(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s (%d): got %d, want 400", tc.name, tc.secs, w.Code)
		}
	}
	tn, _ := st.GetTenant(context.Background(), tsTenant)
	if tn.BridgeOfflineTimeout != 0 {
		t.Errorf("a refused value was stored anyway: %d", tn.BridgeOfflineTimeout)
	}
}

// One tenant's choice must not reach another's, which is the whole point of
// moving this out of a global environment variable.
func TestOneTenantsTimeoutDoesNotReachAnother(t *testing.T) {
	h, st := tsHandler(t)

	r, w := tsRequest("PUT", `{"name":"Alpha","bridge_offline_timeout":900}`, tsTenant)
	h.Update(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("set: %d %s", w.Code, w.Body.String())
	}

	other, _ := st.GetTenant(context.Background(), tsOther)
	if other.BridgeOfflineTimeout != 0 {
		t.Errorf("the other tenant's timeout became %d; a setting written by one tenant "+
			"landed on another", other.BridgeOfflineTimeout)
	}

	r, w = tsRequest("GET", "", tsOther)
	h.Get(w, r)
	var got tenantResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.BridgeOfflineTimeout != 0 {
		t.Errorf("the other tenant reads %d", got.BridgeOfflineTimeout)
	}
}

// A Hub that never had the policy wired must not offer the setting as a range
// of 0..0, which would refuse every value an owner could type.
func TestWithNoPolicyConfiguredTheSettingIsNotOffered(t *testing.T) {
	st := &tsStore{mockStore: &mockStore{}, tenants: map[string]*store.Tenant{
		tsTenant: {ID: tsTenant, Slug: tsTenant, Name: "a"},
	}}
	h := NewTenantHandler(st) // no SetBridgeOfflineTimeoutPolicy

	r, w := tsRequest("GET", "", tsTenant)
	h.Get(w, r)
	var got tenantResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.BridgeOfflineTimeoutMax != 0 {
		t.Errorf("max = %d with no policy set, want 0 so the UI can tell it is not offered",
			got.BridgeOfflineTimeoutMax)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

var _ = hubauth.TenantIDFromContext

// --- audit retention (tranche 2b) ---

func TestAuditRetentionDefaultsAndBounds(t *testing.T) {
	h, _ := tsHandler(t)

	r, w := tsRequest("GET", "", tsTenant)
	h.Get(w, r)
	var got tenantResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.AuditRetentionDays != 0 {
		t.Errorf("audit_retention_days = %d, want 0 (use the default)", got.AuditRetentionDays)
	}
	if got.AuditRetentionDefault != 90 || got.AuditRetentionMin != 30 || got.AuditRetentionMax != 3650 {
		t.Errorf("policy = %d/%d..%d, want 90/30..3650",
			got.AuditRetentionDefault, got.AuditRetentionMin, got.AuditRetentionMax)
	}
}

func TestAnOwnerCanSetAuditRetentionWithinTheBounds(t *testing.T) {
	h, st := tsHandler(t)

	r, w := tsRequest("PUT", `{"name":"Alpha","audit_retention_days":365}`, tsTenant)
	h.Update(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("set: %d %s", w.Code, w.Body.String())
	}
	tn, _ := st.GetTenant(context.Background(), tsTenant)
	if tn.AuditRetentionDays != 365 {
		t.Fatalf("stored %d, want 365", tn.AuditRetentionDays)
	}

	var put tenantResponse
	_ = json.Unmarshal(w.Body.Bytes(), &put)
	if put.AuditRetentionDays != 365 {
		t.Errorf("the update replied with %d, want 365", put.AuditRetentionDays)
	}
}

// The floor is the point of the clamp: a tenant must not be able to shorten
// retention until the evidence of a security event in their own account is
// gone before anyone looks at it.
func TestAuditRetentionCannotBeShortenedBelowTheFloor(t *testing.T) {
	h, st := tsHandler(t)

	for _, days := range []int{1, 29, -5, 99999} {
		r, w := tsRequest("PUT", `{"name":"Alpha","audit_retention_days":`+itoa(days)+`}`, tsTenant)
		h.Update(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%d days: got %d, want 400", days, w.Code)
		}
	}
	tn, _ := st.GetTenant(context.Background(), tsTenant)
	if tn.AuditRetentionDays != 0 {
		t.Errorf("a refused value was stored anyway: %d", tn.AuditRetentionDays)
	}
}

// The two settings are independent: saving one must not clear the other.
func TestTheTwoTenantSettingsDoNotClobberEachOther(t *testing.T) {
	h, st := tsHandler(t)

	r, w := tsRequest("PUT", `{"name":"Alpha","bridge_offline_timeout":900}`, tsTenant)
	h.Update(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("set timeout: %d %s", w.Code, w.Body.String())
	}
	r, w = tsRequest("PUT", `{"name":"Alpha","audit_retention_days":365}`, tsTenant)
	h.Update(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("set retention: %d %s", w.Code, w.Body.String())
	}

	tn, _ := st.GetTenant(context.Background(), tsTenant)
	if tn.BridgeOfflineTimeout != 900 {
		t.Errorf("saving the retention reset the offline timeout to %d", tn.BridgeOfflineTimeout)
	}
	if tn.AuditRetentionDays != 365 {
		t.Errorf("retention = %d, want 365", tn.AuditRetentionDays)
	}
}

// Same pointer semantics as the offline timeout, and it needs its own test:
// a rename must not silently reset the retention an owner chose, and a test
// that saves the two settings in sequence cannot see that.
func TestRenamingTheTenantLeavesTheAuditRetentionAlone(t *testing.T) {
	h, st := tsHandler(t)

	r, w := tsRequest("PUT", `{"name":"Alpha","audit_retention_days":365}`, tsTenant)
	h.Update(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("set: %d %s", w.Code, w.Body.String())
	}

	r, w = tsRequest("PUT", `{"name":"Alpha Renamed"}`, tsTenant)
	h.Update(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("rename: %d %s", w.Code, w.Body.String())
	}

	tn, _ := st.GetTenant(context.Background(), tsTenant)
	if tn.AuditRetentionDays != 365 {
		t.Errorf("the retention is %d after a name-only save, want 365. A plain int field "+
			"would silently return every tenant to the platform default on any unrelated "+
			"save -- and shortening retention destroys audit history.", tn.AuditRetentionDays)
	}
}

// The send budget is the tenant owner's to set (owner ruling, 21 Sep 2026):
// every tenant pays its own carrier for the messages the Hub sends to its
// devices, so a limit on them is a safety net for the tenant's own bill, not
// something the platform sells. Until then only a platform admin could raise it.
func TestAnOwnerSetsTheirOwnSendBudget(t *testing.T) {
	h, st := tsHandler(t)
	h.SetSendCapPolicy(100, 100000, 0, 3000000)
	var forgotten []string
	h.SetStatusInvalidator(func(id string) { forgotten = append(forgotten, id) })

	r, w := tsRequest("PUT", `{"name":"acme","ratelimit_daily_cap":1000,"ratelimit_monthly_cap":20000}`, tsTenant)
	h.Update(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("an owner raising their own budget was refused: %d %s", w.Code, w.Body.String())
	}
	var got tenantResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.RatelimitDailyCap != 1000 || got.RatelimitMonthlyCap != 20000 {
		t.Errorf("response carries %d / %d, want 1000 / 20000", got.RatelimitDailyCap, got.RatelimitMonthlyCap)
	}
	if got.RatelimitDailyDefault != 100 || got.RatelimitDailyMax != 100000 || got.RatelimitMonthlyMax != 3000000 {
		t.Errorf("the response does not carry the platform default and ceiling: %+v", got)
	}
	if st.tenants[tsTenant].RatelimitDailyCap != 1000 {
		t.Errorf("stored daily cap = %d", st.tenants[tsTenant].RatelimitDailyCap)
	}
	if st.tenants[tsOther].RatelimitDailyCap != 0 {
		t.Errorf("another tenant's budget moved to %d", st.tenants[tsOther].RatelimitDailyCap)
	}
	// The limiter caches a budget for 30 s per replica; the change must drop it.
	if len(forgotten) != 1 || forgotten[0] != tsTenant {
		t.Errorf("cached budget not dropped for the tenant: %v", forgotten)
	}

	// A save that does not touch the budget leaves it, and drops no cache.
	forgotten = nil
	r, w = tsRequest("PUT", `{"name":"acme renamed"}`, tsTenant)
	h.Update(w, r)
	if st.tenants[tsTenant].RatelimitDailyCap != 1000 {
		t.Errorf("a rename reset the budget to %d", st.tenants[tsTenant].RatelimitDailyCap)
	}
	if len(forgotten) != 0 {
		t.Errorf("a rename dropped the budget cache: %v", forgotten)
	}

	// 0 returns to the platform default.
	r, w = tsRequest("PUT", `{"name":"acme","ratelimit_daily_cap":0}`, tsTenant)
	h.Update(w, r)
	if w.Code != http.StatusOK || st.tenants[tsTenant].RatelimitDailyCap != 0 {
		t.Errorf("0 did not clear the budget: %d, stored %d", w.Code, st.tenants[tsTenant].RatelimitDailyCap)
	}
}

// Below the platform default is ALLOWED (owner ruling, 21 Sep 2026): a tenant
// pays its own carrier, and a low limit is how it protects its own bill. What
// is refused is nonsense: a negative number, or one past the typo ceiling.
func TestASendBudgetOutsideTheBoundsIsRefused(t *testing.T) {
	h, st := tsHandler(t)
	h.SetSendCapPolicy(100, 100000, 0, 3000000)
	for _, body := range []string{
		`{"name":"acme","ratelimit_daily_cap":-1}`,
		`{"name":"acme","ratelimit_daily_cap":100001}`,
		`{"name":"acme","ratelimit_monthly_cap":-5}`,
		`{"name":"acme","ratelimit_monthly_cap":3000001}`,
	} {
		r, w := tsRequest("PUT", body, tsTenant)
		h.Update(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s -> %d, want 400", body, w.Code)
		}
	}
	if got := st.tenants[tsTenant]; got.RatelimitDailyCap != 0 || got.RatelimitMonthlyCap != 0 {
		t.Errorf("a refused request still wrote %d / %d", got.RatelimitDailyCap, got.RatelimitMonthlyCap)
	}

	r, w := tsRequest("PUT", `{"name":"acme","ratelimit_daily_cap":20,"ratelimit_monthly_cap":300}`, tsTenant)
	h.Update(w, r)
	if w.Code != http.StatusOK || st.tenants[tsTenant].RatelimitDailyCap != 20 || st.tenants[tsTenant].RatelimitMonthlyCap != 300 {
		t.Errorf("a limit below the platform default was not accepted: %d %s", w.Code, w.Body.String())
	}
}
