package tenancy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

// The purge job destroys customer data and had no tests at all before a hosted
// TAK teardown was added to it. These cover the new step and the contract it
// has to honour, which the job's own comment states: a failure leaves the
// tenant intact for the next run rather than half-erasing an account.

type fakeTAKDeleter struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (f *fakeTAKDeleter) DeleteTAKInstanceForTenant(_ context.Context, tenantID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, tenantID)
	return f.err
}

func (f *fakeTAKDeleter) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.calls...)
}

func (f *fakeTAKDeleter) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// purgeFixture gives a store holding one tenant that is long past its grace
// period and owns one device, so "were the rows destroyed" is a question with an
// unambiguous answer.
func purgeFixture(t *testing.T) (store.Store, string) {
	t.Helper()
	db, err := sqlite.New(t.TempDir()+"/hub.db", 0)
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	const tenantID = "t-closed"
	if err := db.CreateTenant(ctx, &store.Tenant{
		ID: tenantID, Slug: tenantID, Name: "a closed account", Status: store.TenantActive,
	}); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if err := db.CreateDevice(ctx, tenantID, &store.Device{
		IMEI: "300434", Label: "KIT", Type: "rockblock",
	}); err != nil {
		t.Fatalf("device: %v", err)
	}
	// Sixty days ago, so it is due whatever the grace period is set to.
	if err := db.SoftDeleteTenant(ctx, tenantID, time.Now().UTC().Add(-60*24*time.Hour)); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	return db, tenantID
}

func deviceCount(t *testing.T, db store.Store, tenantID string) int {
	t.Helper()
	ds, err := db.ListDevices(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("list devices: %v", err)
	}
	return len(ds)
}

// The happy path: the tenant's TAK server is torn down and its rows are gone.
func TestAPurgeDestroysTheTenantsTAKServerAndThenItsRows(t *testing.T) {
	db, tenantID := purgeFixture(t)
	del := &fakeTAKDeleter{}

	j := NewPurgeJob(db, nil, 0)
	j.SetTAKInstances(del)
	j.once(context.Background())

	if got := del.seen(); len(got) != 1 || got[0] != tenantID {
		t.Errorf("the TAK teardown was asked for %v, want exactly [%s]; a purge that skips it "+
			"leaves a live OpenTAKServer holding a closed account's history", got, tenantID)
	}
	if n := deviceCount(t, db, tenantID); n != 0 {
		t.Errorf("%d device rows survived the purge, want 0", n)
	}
}

// The contract that matters: if the server cannot be destroyed, NOTHING is
// destroyed, and the next run tries again. Purging the rows anyway would leave a
// running server for an account with no record of it in the Hub -- the
// half-erasure the job promises not to perform.
func TestAFailedTAKTeardownLeavesEveryRowIntactAndRetries(t *testing.T) {
	db, tenantID := purgeFixture(t)
	del := &fakeTAKDeleter{}
	del.fail(errors.New("the API server said no"))

	j := NewPurgeJob(db, nil, 0)
	j.SetTAKInstances(del)
	j.once(context.Background())

	if n := deviceCount(t, db, tenantID); n != 1 {
		t.Fatalf("%d device rows after a FAILED teardown, want 1 left intact: the tenant was "+
			"half-erased, with its TAK server still running", n)
	}
	if got := del.seen(); len(got) != 1 {
		t.Errorf("teardown attempts = %d, want 1", len(got))
	}

	// Next run, with the teardown now working: it is retried and the purge
	// completes. A tenant must not be stranded by one transient failure.
	del.fail(nil)
	j.once(context.Background())

	if got := del.seen(); len(got) != 2 {
		t.Errorf("teardown attempts = %d after a second run, want 2: the failure was not retried", len(got))
	}
	if n := deviceCount(t, db, tenantID); n != 0 {
		t.Errorf("%d device rows survived the second purge, want 0", n)
	}
}

// Hosted TAK is optional, and a Hub without it must purge exactly as before.
func TestAPurgeWithoutHostedTAKIsUnchanged(t *testing.T) {
	db, tenantID := purgeFixture(t)

	j := NewPurgeJob(db, nil, 0) // no SetTAKInstances
	j.once(context.Background())

	if n := deviceCount(t, db, tenantID); n != 0 {
		t.Errorf("%d device rows survived a purge with no TAK deleter attached, want 0", n)
	}
}

// A tenant still inside its grace period is not touched, and its TAK server is
// not torn down either -- the teardown must sit behind the same cutoff as the
// erasure, not run early on a tenant that can still be recovered.
func TestATenantInsideItsGracePeriodKeepsItsServer(t *testing.T) {
	db, tenantID := purgeFixture(t)
	ctx := context.Background()
	// Move the deletion to just now, well inside the 30-day grace.
	if err := db.SoftDeleteTenant(ctx, tenantID, time.Now().UTC()); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	del := &fakeTAKDeleter{}

	j := NewPurgeJob(db, nil, 0)
	j.SetTAKInstances(del)
	j.once(ctx)

	if got := del.seen(); len(got) != 0 {
		t.Errorf("the TAK server of a recoverable tenant was torn down: %v", got)
	}
	if n := deviceCount(t, db, tenantID); n != 1 {
		t.Errorf("%d device rows for a tenant inside its grace period, want 1", n)
	}
}
