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

// A Ko-fi membership renewal carries no message, so it carries no claim code.
// This is the whole subscription lifecycle: somebody joins with the code in
// their message, then Ko-fi charges them every month with message null.
//
// Before the payer was remembered, the second payment matched nothing and the
// tenant lapsed to free on day 32 while their card was still being charged.
// This test is the reason kofi_payer_email exists.
func TestRenewal_WithoutAMessageStillMatches(t *testing.T) {
	st := newStore()
	h := NewHandler(st, token)
	ctx := context.Background()

	// The supporter pays from a personal address, not the one on the account.
	const payer = "jo.example@example.com"

	// 1. Join: the message carries the claim code.
	rr := post(t, h, Payload{
		VerificationToken: token, IsSubscriptionPayment: true, IsFirstSubscriptionPmnt: true,
		TierName: "Crew", Email: payer, Message: "joining! AB2K9XYZ",
		KofiTransactionID: "join-1",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("join: %d %s", rr.Code, rr.Body.String())
	}
	got, _ := st.GetTenant(ctx, "t-a")
	if got.Plan != plans.Crew {
		t.Fatalf("join did not set the plan: %q", got.Plan)
	}
	if !strings.EqualFold(got.KofiPayerEmail, payer) {
		t.Fatalf("the payer was not remembered: %q", got.KofiPayerEmail)
	}
	firstExpiry := got.PlanExpiresAt

	// 2. Renewal a month later: exactly Ko-fi's documented shape, message null.
	rr = post(t, h, Payload{
		VerificationToken: token, IsSubscriptionPayment: true, IsFirstSubscriptionPmnt: false,
		TierName: "Crew", Email: payer, Message: "",
		KofiTransactionID: "renew-1",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("renewal: %d %s", rr.Code, rr.Body.String())
	}
	got, _ = st.GetTenant(ctx, "t-a")
	if got.Plan != plans.Crew {
		t.Errorf("the renewal dropped the plan to %q", got.Plan)
	}
	if got.PlanExpiresAt == nil || firstExpiry == nil || !got.PlanExpiresAt.After(*firstExpiry) {
		t.Errorf("the renewal did not extend the expiry: %v -> %v", firstExpiry, got.PlanExpiresAt)
	}

	// 3. A renewal from an address nobody has ever paid with still matches
	//    nothing, so remembering a payer has not opened a way in.
	before, _ := st.GetTenant(ctx, "t-b")
	post(t, h, Payload{
		VerificationToken: token, IsSubscriptionPayment: true,
		TierName: "Fleet", Email: "someone-else@example.org", Message: "",
		KofiTransactionID: "renew-stranger",
	})
	after, _ := st.GetTenant(ctx, "t-b")
	if after.Plan != before.Plan {
		t.Errorf("a stranger's renewal changed a tenant: %q -> %q", before.Plan, after.Plan)
	}
}

// Two tenants that have somehow ended up with the same payer address are not a
// tie to break. Charging the wrong customer's plan is worse than charging none.
func TestRenewal_AmbiguousPayerMatchesNothing(t *testing.T) {
	st := newStore()
	st.tenants[0].KofiPayerEmail = "shared@example.com"
	st.tenants[1].KofiPayerEmail = "shared@example.com"
	h := NewHandler(st, token)

	rr := post(t, h, Payload{
		VerificationToken: token, IsSubscriptionPayment: true,
		TierName: "Crew", Email: "shared@example.com", Message: "",
		KofiTransactionID: "ambiguous-1",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}
	if st.updates != 0 {
		t.Errorf("an ambiguous payment changed %d tenants, want 0", st.updates)
	}
}

// The claim code still wins over a remembered payer, so an operator can move a
// subscription to the right tenant by asking the supporter to send one payment
// with the code in the message.
func TestClaimCodeOutranksRememberedPayer(t *testing.T) {
	st := newStore()
	st.tenants[0].KofiPayerEmail = "jo@example.com" // t-a remembers this payer
	h := NewHandler(st, token)

	// Same payer, but the message names t-b's code.
	post(t, h, Payload{
		VerificationToken: token, IsSubscriptionPayment: true,
		TierName: "Fleet", Email: "jo@example.com", Message: "moving this to QQ44MMNN",
		KofiTransactionID: "move-1",
	})
	a, _ := st.GetTenant(context.Background(), "t-a")
	b, _ := st.GetTenant(context.Background(), "t-b")
	if b.Plan != plans.Fleet {
		t.Errorf("the claim code did not win: t-b is %q", b.Plan)
	}
	if a.Plan != plans.Free {
		t.Errorf("the wrong tenant was upgraded: t-a is %q", a.Plan)
	}
}

// Ko-fi retries a delivery until it gets a 200. If our response is lost after
// we have already applied the payment, the retry must not buy a second month.
func TestDuplicateDelivery_DoesNotExtendTwice(t *testing.T) {
	st := newStore()
	h := NewHandler(st, token)
	p := Payload{
		VerificationToken: token, IsSubscriptionPayment: true, TierName: "Crew",
		Email: "jo.example@example.com", Message: "AB2K9XYZ",
		MessageID: "71aca96f-fae6-4e38-94aa-17adc1cb6c69", KofiTransactionID: "txn-dup",
	}
	if rr := post(t, h, p); rr.Code != http.StatusOK {
		t.Fatalf("first delivery: %d", rr.Code)
	}
	first, _ := st.GetTenant(context.Background(), "t-a")
	if first.PlanExpiresAt == nil {
		t.Fatal("first delivery set no expiry")
	}
	firstExpiry := *first.PlanExpiresAt
	updatesAfterFirst := st.updates

	// Ko-fi retries the same message_id.
	if rr := post(t, h, p); rr.Code != http.StatusOK {
		t.Fatalf("retry must still be 200 or Ko-fi keeps retrying: %d", rr.Code)
	}
	again, _ := st.GetTenant(context.Background(), "t-a")
	if !again.PlanExpiresAt.Equal(firstExpiry) {
		t.Errorf("a retried delivery extended the expiry again: %v -> %v", firstExpiry, again.PlanExpiresAt)
	}
	if st.updates != updatesAfterFirst {
		t.Errorf("a retried delivery wrote to the store again (%d writes)", st.updates-updatesAfterFirst)
	}

	// A genuine next month's payment, different message_id, does extend.
	p.MessageID, p.KofiTransactionID, p.Message = "a-new-delivery-id", "txn-month-2", ""
	if rr := post(t, h, p); rr.Code != http.StatusOK {
		t.Fatalf("month 2: %d", rr.Code)
	}
	month2, _ := st.GetTenant(context.Background(), "t-a")
	if !month2.PlanExpiresAt.After(firstExpiry) {
		t.Errorf("the next month's payment did not extend: %v", month2.PlanExpiresAt)
	}
}
