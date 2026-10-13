// Package lxmfhubtest holds the in-memory store and provider-account fakes
// the lxmfhub tests and the Python interop suite build a Hub LXMF service
// on. Test support only; nothing in production imports it.
package lxmfhubtest

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// MemStore is the slice of store.Store the LXMF service uses, in memory.
type MemStore struct {
	mu      sync.Mutex
	rows    map[string]map[string]*store.LXMFMessage // tenant -> id -> row
	held    map[string]map[string]*store.LXMFHeldMessage
	claims  map[string]bool
	tenants []store.Tenant
	seq     int
}

// NewMemStore knows the given tenants, named "Tenant <id>".
func NewMemStore(tenants ...string) *MemStore {
	m := &MemStore{rows: map[string]map[string]*store.LXMFMessage{}, held: map[string]map[string]*store.LXMFHeldMessage{}, claims: map[string]bool{}}
	for _, t := range tenants {
		m.tenants = append(m.tenants, store.Tenant{ID: t, Name: "Tenant " + t, Status: store.TenantActive})
	}
	return m
}

func (m *MemStore) InsertLXMFMessage(_ context.Context, tenantID string, row *store.LXMFMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if row.ID == "" {
		m.seq++
		row.ID = fmt.Sprintf("m%03d", m.seq)
	}
	row.TenantID = tenantID
	if m.rows[tenantID] == nil {
		m.rows[tenantID] = map[string]*store.LXMFMessage{}
	}
	if _, dup := m.rows[tenantID][row.ID]; dup {
		return store.ErrDuplicate
	}
	cp := *row
	cp.CreatedAt = time.Now()
	m.rows[tenantID][row.ID] = &cp
	return nil
}

func (m *MemStore) ListLXMFMessages(_ context.Context, tenantID string, q store.LXMFQuery) ([]store.LXMFMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.LXMFMessage
	for _, r := range m.rows[tenantID] {
		if (q.Direction == "" || r.Direction == q.Direction) && (q.State == "" || r.State == q.State) && (q.Before.IsZero() || r.CreatedAt.Before(q.Before)) {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (m *MemStore) UpdateLXMFMessageState(_ context.Context, tenantID, id string, upd store.LXMFStateUpdate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rows[tenantID][id]
	if r == nil {
		return store.ErrNotFound
	}
	r.State = upd.State
	if upd.Hash != "" {
		r.Hash = upd.Hash
	}
	if upd.Method != "" {
		r.Method = upd.Method
	}
	if upd.Detail != "" {
		r.Detail = upd.Detail
	}
	return nil
}

func (m *MemStore) CountLXMFMessages(_ context.Context, tenantID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows[tenantID]), nil
}

func (m *MemStore) PruneLXMFMessages(context.Context, string, int) (int, error) { return 0, nil }

func (m *MemStore) InsertLXMFHeld(_ context.Context, tenantID string, h *store.LXMFHeldMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.held[tenantID] == nil {
		m.held[tenantID] = map[string]*store.LXMFHeldMessage{}
	}
	if _, dup := m.held[tenantID][h.ID]; dup {
		return store.ErrDuplicate
	}
	cp := *h
	cp.TenantID = tenantID
	if cp.Size == 0 {
		cp.Size = len(cp.Data)
	}
	if cp.ReceivedAt.IsZero() {
		cp.ReceivedAt = time.Now()
	}
	m.held[tenantID][h.ID] = &cp
	return nil
}

func (m *MemStore) ListLXMFHeldFor(_ context.Context, tenantID, destination string) ([]store.LXMFHeldMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []store.LXMFHeldMessage{}
	for _, h := range m.held[tenantID] {
		if h.Destination == destination {
			cp := *h
			cp.Data = nil
			out = append(out, cp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Size != out[j].Size {
			return out[i].Size < out[j].Size
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (m *MemStore) GetLXMFHeld(_ context.Context, tenantID, id string) (*store.LXMFHeldMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := m.held[tenantID][id]
	if h == nil {
		return nil, store.ErrNotFound
	}
	cp := *h
	return &cp, nil
}

func (m *MemStore) DeleteLXMFHeld(_ context.Context, tenantID, destination string, ids []string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, id := range ids {
		if h := m.held[tenantID][id]; h != nil && h.Destination == destination {
			delete(m.held[tenantID], id)
			n++
		}
	}
	return n, nil
}

func (m *MemStore) LXMFHeldBytes(_ context.Context, tenantID string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, h := range m.held[tenantID] {
		n += int64(h.Size)
	}
	return n, nil
}

func (m *MemStore) PruneLXMFHeld(_ context.Context, tenantID string, cutoff time.Time, maxBytes int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	var all []*store.LXMFHeldMessage
	for id, h := range m.held[tenantID] {
		if h.ReceivedAt.Before(cutoff) {
			delete(m.held[tenantID], id)
			n++
			continue
		}
		all = append(all, h)
	}
	if maxBytes > 0 {
		sort.Slice(all, func(i, j int) bool { return all[i].ReceivedAt.After(all[j].ReceivedAt) })
		var running int64
		for _, h := range all {
			running += int64(h.Size)
			if running > maxBytes {
				delete(m.held[tenantID], h.ID)
				n++
			}
		}
	}
	return n, nil
}

func (m *MemStore) ClaimOnce(_ context.Context, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.claims[key] {
		return false, nil
	}
	m.claims[key] = true
	return true, nil
}

func (m *MemStore) ListTenants(context.Context) ([]store.Tenant, error) { return m.tenants, nil }

func (m *MemStore) GetTenant(_ context.Context, id string) (*store.Tenant, error) {
	for i := range m.tenants {
		if m.tenants[i].ID == id {
			return &m.tenants[i], nil
		}
	}
	return nil, store.ErrNotFound
}

// States maps message id to state for one tenant.
func (m *MemStore) States(tenantID string) map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for id, r := range m.rows[tenantID] {
		out[id] = r.State
	}
	return out
}

// Inbox lists one tenant's received messages.
func (m *MemStore) Inbox(tenantID string) []store.LXMFMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.LXMFMessage
	for _, r := range m.rows[tenantID] {
		if r.Direction == store.LXMFDirectionIn {
			out = append(out, *r)
		}
	}
	return out
}

// HeldCount is how many messages one tenant's propagation node holds.
func (m *MemStore) HeldCount(tenantID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.held[tenantID])
}

// MemAccounts is the provider-account slice in memory. OnSet mimics the
// bus: every replica reloads the tenant after a save.
type MemAccounts struct {
	mu    sync.Mutex
	rows  map[string]map[string]string
	OnSet func(tenantID string)
}

// NewMemAccounts is empty.
func NewMemAccounts() *MemAccounts { return &MemAccounts{rows: map[string]map[string]string{}} }

func (a *MemAccounts) ForTenant(_ context.Context, tenantID, provider string) (*integrations.Account, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f := a.rows[tenantID+"/"+provider]
	if f == nil {
		return nil, nil
	}
	cp := map[string]string{}
	for k, v := range f {
		cp[k] = v
	}
	return &integrations.Account{Provider: provider, TenantID: tenantID, Fields: cp}, nil
}

func (a *MemAccounts) Set(_ context.Context, tenantID, provider string, fields map[string]string) (*integrations.Account, error) {
	a.mu.Lock()
	k := tenantID + "/" + provider
	if a.rows[k] == nil {
		a.rows[k] = map[string]string{}
	}
	for f, v := range fields {
		a.rows[k][f] = strings.TrimSpace(v)
	}
	a.mu.Unlock()
	if a.OnSet != nil {
		a.OnSet(tenantID)
	}
	return a.ForTenant(context.Background(), tenantID, provider)
}
