package kofi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/plans"
	"github.com/meshsat/meshsat-hub/internal/store"
)

type memTenants struct {
	tenants  []store.Tenant
	updates  int
	failNext bool
}

func (m *memTenants) GetTenant(_ context.Context, id string) (*store.Tenant, error) {
	for i := range m.tenants {
		if m.tenants[i].ID == id {
			t := m.tenants[i]
			return &t, nil
		}
	}
	return nil, store.ErrNotFound
}

func (m *memTenants) ListTenants(context.Context) ([]store.Tenant, error) {
	out := make([]store.Tenant, len(m.tenants))
	copy(out, m.tenants)
	return out, nil
}

func (m *memTenants) UpdateTenant(_ context.Context, t *store.Tenant) error {
	if m.failNext {
		m.failNext = false
		return store.ErrNotFound
	}
	m.updates++
	for i := range m.tenants {
		if m.tenants[i].ID == t.ID {
			m.tenants[i] = *t
			return nil
		}
	}
	return store.ErrNotFound
}

const token = "kofi-verification-token"

func post(t *testing.T, h *Handler, p Payload) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(p)
	form := url.Values{"data": {string(body)}}
	req := httptest.NewRequest(http.MethodPost, "/api/webhook/kofi/s3cr3t", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func newStore() *memTenants {
	return &memTenants{tenants: []store.Tenant{
		{ID: "t-a", Slug: "a", Name: "Alpha", Plan: plans.Free, Status: store.TenantActive,
			OwnerUserID: "alice@example.com", KofiClaimCode: "AB2K9XYZ"},
		{ID: "t-b", Slug: "b", Name: "Bravo", Plan: plans.Free, Status: store.TenantActive,
			OwnerUserID: "bob@example.com", KofiClaimCode: "QQ44MMNN"},
	}}
}

func TestPayment_MatchesOnClaimCode(t *testing.T) {
	st := newStore()
	h := NewHandler(st, token)
	rr := post(t, h, Payload{
		VerificationToken: token, IsSubscriptionPayment: true,
		TierName: "Crew", Email: "someone-else@example.com",
		Message: "here you go! AB2K9XYZ", KofiTransactionID: "txn-1",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", rr.Code, rr.Body.String())
	}
	got, _ := st.GetTenant(context.Background(), "t-a")
	if got.Plan != plans.Crew {
		t.Errorf("plan = %q, want crew", got.Plan)
	}
	if got.PlanExpiresAt == nil || got.PlanExpiresAt.Before(time.Now().Add(30*24*time.Hour)) {
		t.Errorf("expiry = %v, want about a month out", got.PlanExpiresAt)
	}
	// The payer's email belongs to nobody here; the code decided, not the mail.
	other, _ := st.GetTenant(context.Background(), "t-b")
	if other.Plan != plans.Free {
		t.Errorf("the wrong tenant was upgraded: %q", other.Plan)
	}
}

func TestPayment_FallsBackToEmail(t *testing.T) {
	st := newStore()
	h := NewHandler(st, token)
	rr := post(t, h, Payload{
		VerificationToken: token, IsSubscriptionPayment: true,
		TierName: "fleet", Email: "Bob@Example.com", Message: "thanks!",
		KofiTransactionID: "txn-2",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d", rr.Code)
	}
	got, _ := st.GetTenant(context.Background(), "t-b")
	if got.Plan != plans.Fleet {
		t.Errorf("plan = %q, want fleet", got.Plan)
	}
}

// An unmatched payment must change nothing at all. Guessing which customer
// paid is how one tenant ends up funding another's fleet.
func TestPayment_UnmatchedChangesNothing(t *testing.T) {
	st := newStore()
	h := NewHandler(st, token)
	rr := post(t, h, Payload{
		VerificationToken: token, IsSubscriptionPayment: true,
		TierName: "Crew", Email: "stranger@example.com", Message: "no code here",
		KofiTransactionID: "txn-3",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (Ko-fi must not be told to retry)", rr.Code)
	}
	if st.updates != 0 {
		t.Errorf("%d tenants changed on an unmatched payment, want 0", st.updates)
	}
}

// A one-off donation is support, not a subscription, and must not grant a tier.
func TestPayment_OneOffDoesNotGrantATier(t *testing.T) {
	st := newStore()
	h := NewHandler(st, token)
	rr := post(t, h, Payload{
		VerificationToken: token, IsSubscriptionPayment: false,
		TierName: "Crew", Message: "AB2K9XYZ", KofiTransactionID: "txn-4",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d", rr.Code)
	}
	if st.updates != 0 {
		t.Errorf("a one-off donation granted a tier")
	}
}

func TestPayment_WrongTokenIsRefused(t *testing.T) {
	st := newStore()
	h := NewHandler(st, token)
	rr := post(t, h, Payload{
		VerificationToken: "not-the-token", IsSubscriptionPayment: true,
		TierName: "Crew", Message: "AB2K9XYZ",
	})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rr.Code)
	}
	if st.updates != 0 {
		t.Errorf("an unauthenticated payload changed a tenant")
	}
}

func TestPayment_UnconfiguredRefusesEverything(t *testing.T) {
	st := newStore()
	h := NewHandler(st, "")
	rr := post(t, h, Payload{VerificationToken: "", IsSubscriptionPayment: true, Message: "AB2K9XYZ"})
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rr.Code)
	}
	if st.updates != 0 {
		t.Errorf("an unconfigured endpoint changed a tenant")
	}
}

// Paying early extends rather than resets, so a supporter is never punished
// for renewing before the month is up.
func TestPayment_RenewalStacks(t *testing.T) {
	st := newStore()
	future := time.Now().UTC().Add(10 * 24 * time.Hour)
	st.tenants[0].Plan = plans.Crew
	st.tenants[0].PlanExpiresAt = &future
	h := NewHandler(st, token)

	post(t, h, Payload{
		VerificationToken: token, IsSubscriptionPayment: true,
		TierName: "Crew", Message: "AB2K9XYZ", KofiTransactionID: "txn-5",
	})
	got, _ := st.GetTenant(context.Background(), "t-a")
	want := future.Add(Period)
	if got.PlanExpiresAt == nil || got.PlanExpiresAt.Sub(want).Abs() > time.Minute {
		t.Errorf("expiry = %v, want %v (the remaining time should carry over)", got.PlanExpiresAt, want)
	}
}

func TestTierMapping(t *testing.T) {
	h := NewHandler(newStore(), token)
	h.SetTierMapping(map[string]string{"Crew Membership": "crew", "Big Fleet": "fleet", "Nonsense": "wat"})
	for _, tc := range []struct{ tier, want string }{
		{"Crew Membership", plans.Crew},
		{"crew membership", plans.Crew},
		{"Big Fleet", plans.Fleet},
		{"fleet", plans.Fleet},         // the tier name read as a plan name
		{"Nonsense", plans.Crew},       // mapped to an unknown plan: ignored, smallest paid tier
		{"Something Else", plans.Crew}, // unrecognised: never drop a payer to free
		{"free", plans.Crew},           // a paid tier is never "free"
	} {
		if got := h.planFor(tc.tier); got != tc.want {
			t.Errorf("planFor(%q) = %q, want %q", tc.tier, got, tc.want)
		}
	}
}

func TestNewClaimCode(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		c, err := NewClaimCode()
		if err != nil {
			t.Fatalf("NewClaimCode: %v", err)
		}
		if len(c) != 8 {
			t.Fatalf("code %q is %d characters, want 8", c, len(c))
		}
		if strings.ContainsAny(c, "IO01") {
			t.Errorf("code %q contains a character people mistype", c)
		}
		if !claimCodeRe.MatchString(c) {
			t.Errorf("code %q would not be found in a Ko-fi message", c)
		}
		seen[c] = true
	}
	if len(seen) < 190 {
		t.Errorf("only %d distinct codes in 200 draws", len(seen))
	}
}

// A lapse drops the tier and nothing else. This is the commercial half of the
// SOS invariant: see TestSOSSurvivesTheQuota in internal/quota for the other.
func TestLapse_DowngradesOnlyExpiredPaidPlans(t *testing.T) {
	st := newStore()
	past := time.Now().UTC().Add(-time.Hour)
	future := time.Now().UTC().Add(time.Hour)
	st.tenants[0].Plan, st.tenants[0].PlanExpiresAt = plans.Crew, &past
	st.tenants[1].Plan, st.tenants[1].PlanExpiresAt = plans.Fleet, &future
	st.tenants = append(st.tenants,
		store.Tenant{ID: "t-custom", Plan: plans.Custom, PlanExpiresAt: &past},
		store.Tenant{ID: store.DefaultTenantID, Plan: plans.Beta, PlanExpiresAt: &past},
	)

	forgotten := map[string]bool{}
	j := NewLapseJob(st, nil, func(id string) { forgotten[id] = true })
	j.Once(context.Background())

	a, _ := st.GetTenant(context.Background(), "t-a")
	if a.Plan != plans.Free || a.PlanExpiresAt != nil {
		t.Errorf("expired crew = %q/%v, want free/nil", a.Plan, a.PlanExpiresAt)
	}
	if !forgotten["t-a"] {
		t.Error("the lapse was not announced to the other replicas")
	}
	b, _ := st.GetTenant(context.Background(), "t-b")
	if b.Plan != plans.Fleet {
		t.Errorf("an unexpired plan was lapsed: %q", b.Plan)
	}
	c, _ := st.GetTenant(context.Background(), "t-custom")
	if c.Plan != plans.Free {
		t.Errorf("expired custom = %q, want free", c.Plan)
	}
	d, _ := st.GetTenant(context.Background(), store.DefaultTenantID)
	if d.Plan != plans.Beta {
		t.Errorf("the platform tenant was lapsed: %q", d.Plan)
	}
}
