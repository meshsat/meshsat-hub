package audit

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

type fakeRetentionStore struct {
	tenants  []store.Tenant
	entries  map[string][]store.AuditEntry
	deleted  []string
	listErr  error
	deleteOK bool
}

func (f *fakeRetentionStore) ListTenants(context.Context) ([]store.Tenant, error) {
	return f.tenants, f.listErr
}

func (f *fakeRetentionStore) ListAuditEntriesBefore(_ context.Context, tenantID string, _ time.Time, _ int) ([]store.AuditEntry, error) {
	return f.entries[tenantID], nil
}

func (f *fakeRetentionStore) DeleteAuditEntriesBefore(_ context.Context, tenantID string, _ time.Time) (int64, error) {
	f.deleted = append(f.deleted, tenantID)
	return int64(len(f.entries[tenantID])), nil
}

type recordingSink struct {
	writes map[string]int
	fail   string
}

func (r *recordingSink) Name() string { return "recording" }
func (r *recordingSink) Write(_ context.Context, tenantID string, _ time.Time, entries []store.AuditEntry) error {
	if tenantID == r.fail {
		return errors.New("sink down")
	}
	if r.writes == nil {
		r.writes = map[string]int{}
	}
	r.writes[tenantID] += len(entries)
	return nil
}

// Every tenant is archived and purged; a tenant whose archive fails is not
// purged; tenants without old entries are purged without a sink write.
func TestPurgeAllCoversEveryTenant(t *testing.T) {
	st := &fakeRetentionStore{
		tenants: []store.Tenant{{ID: "default"}, {ID: "t_a"}, {ID: "t_b"}, {ID: "t_empty"}},
		entries: map[string][]store.AuditEntry{"default": {{ID: "1"}}, "t_a": {{ID: "2"}, {ID: "3"}}, "t_b": {{ID: "4"}}},
	}
	sink := &recordingSink{fail: "t_b"}
	purgeAll(context.Background(), st, RetentionConfig{RetentionDays: 30}, sink)
	if sink.writes["default"] != 1 || sink.writes["t_a"] != 2 || sink.writes["t_empty"] != 0 {
		t.Errorf("writes: %v", sink.writes)
	}
	want := map[string]bool{"default": true, "t_a": true, "t_empty": true}
	for _, d := range st.deleted {
		if !want[d] {
			t.Errorf("unexpected purge of %q (archive failed)", d)
		}
		delete(want, d)
	}
	if len(want) != 0 {
		t.Errorf("not purged: %v", want)
	}
}

// When the tenants table cannot be read, the default tenant is still purged.
func TestPurgeAllFallsBackToDefault(t *testing.T) {
	st := &fakeRetentionStore{listErr: errors.New("db"), entries: map[string][]store.AuditEntry{"default": {{ID: "1"}}}}
	purgeAll(context.Background(), st, RetentionConfig{RetentionDays: 1}, nil)
	if len(st.deleted) != 1 || st.deleted[0] != "default" {
		t.Errorf("deleted: %v", st.deleted)
	}
}

func TestFileSinkWritesPerTenant(t *testing.T) {
	dir := t.TempDir()
	s := FileSink{Dir: dir}
	if err := s.Write(context.Background(), "t_a", time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), []store.AuditEntry{{ID: "x"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := readFile(dir + "/t_a/audit-2026-01-02.jsonl"); err != nil {
		t.Fatal(err)
	}
}

func readFile(p string) ([]byte, error) { return os.ReadFile(p) } // #nosec G304 -- test temp dir
