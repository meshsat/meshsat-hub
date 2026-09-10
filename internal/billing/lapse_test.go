package billing

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/plans"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// memPlans is the narrow fake the lapse job needs: list and update, nothing
// that could grant a plan.
type memPlans struct {
	mu      sync.Mutex
	tenants []store.Tenant
}

func (m *memPlans) ListTenants(context.Context) ([]store.Tenant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.Tenant, len(m.tenants))
	copy(out, m.tenants)
	return out, nil
}

func (m *memPlans) UpdateTenant(_ context.Context, t *store.Tenant) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.tenants {
		if m.tenants[i].ID == t.ID {
			m.tenants[i] = *t
			return nil
		}
	}
	return store.ErrNotFound
}

func (m *memPlans) get(id string) store.Tenant {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.tenants {
		if m.tenants[i].ID == id {
			return m.tenants[i]
		}
	}
	return store.Tenant{}
}

// A lapse drops the tier and nothing else. This is the commercial half of the
// SOS invariant: see TestSOSSurvivesTheQuota in internal/quota for the other.
func TestLapse_DowngradesOnlyExpiredPaidPlans(t *testing.T) {
	past0 := time.Now().UTC().Add(-time.Hour)
	st := &memPlans{tenants: []store.Tenant{
		{ID: "t-a", Plan: plans.Crew, PlanExpiresAt: &past0},
		{ID: "t-b", Plan: plans.Free},
	}}
	past := time.Now().UTC().Add(-time.Hour)
	future := time.Now().UTC().Add(time.Hour)
	st.tenants[0].Plan, st.tenants[0].PlanExpiresAt = plans.Crew, &past
	st.tenants[1].Plan, st.tenants[1].PlanExpiresAt = plans.Fleet, &future
	_ = past0
	st.tenants = append(st.tenants,
		store.Tenant{ID: "t-custom", Plan: plans.Custom, PlanExpiresAt: &past},
		store.Tenant{ID: store.DefaultTenantID, Plan: plans.Beta, PlanExpiresAt: &past},
	)

	forgotten := map[string]bool{}
	j := NewLapseJob(st, nil, func(id string) { forgotten[id] = true })
	j.Once(context.Background())

	a := st.get("t-a")
	if a.Plan != plans.Free || a.PlanExpiresAt != nil {
		t.Errorf("expired crew = %q/%v, want free/nil", a.Plan, a.PlanExpiresAt)
	}
	if !forgotten["t-a"] {
		t.Error("the lapse was not announced to the other replicas")
	}
	b := st.get("t-b")
	if b.Plan != plans.Fleet {
		t.Errorf("an unexpired plan was lapsed: %q", b.Plan)
	}
	c := st.get("t-custom")
	if c.Plan != plans.Free {
		t.Errorf("expired custom = %q, want free", c.Plan)
	}
	d := st.get(store.DefaultTenantID)
	if d.Plan != plans.Beta {
		t.Errorf("the platform tenant was lapsed: %q", d.Plan)
	}
}
