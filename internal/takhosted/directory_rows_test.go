package takhosted

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
	"github.com/meshsat/meshsat-hub/internal/takfront"
)

// The refresher is the only writer of the tak_instances status columns.
//
// UpsertTAKInstance overwrites phase, host and ca_cert_pem unconditionally -- the
// store conformance suite asserts exactly that -- so these rows are only correct
// while one writer holds all three values. The refresher does: it has the custom
// resource in hand. Anything else writing a partial row would blank the CA
// certificate, and takfront identifies a phone's tenant by its certificate
// issuer, so every tenant would silently drop out of the directory.

func rowStore(t *testing.T) store.Store {
	t.Helper()
	db, err := sqlite.New(t.TempDir()+"/hub.db", 0)
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// provisioningInstance is one the operator has not finished building. readyInstance
// cannot express this, and it is the case that matters most for the rows.
func provisioningInstance(tenantID, label string) TakInstance {
	return TakInstance{
		Metadata: objectMeta{Name: instanceObjectName(label)},
		Spec:     InstanceSpec{TenantID: tenantID, Label: label, State: stateRunning},
		Status:   InstanceStat{Phase: phaseProvisioning},
	}
}

func TestARefreshRecordsTheOperatorsStateForEveryInstance(t *testing.T) {
	ca := newTestCA(t, "MeshSat TAK aaaaaaaaaa CA")
	api := newFakeAPI(ca)
	api.autoIssue = true
	api.setInstances(
		readyInstance("t-ready", "aaaaaaaaaa", ca.pemStr),
		provisioningInstance("t-waiting", "bbbbbbbbbb"),
	)
	srv := httptest.NewServer(api.handler(t))
	t.Cleanup(srv.Close)
	cl := NewClientWith(srv.Client(), srv.URL, "meshsat-tak")

	db := rowStore(t)
	r := NewDirectoryRefresher(cl, NewIdentityKeeper(cl, nil), func(*takfront.Directory) {}, nil)
	r.SetStore(db)

	ctx := context.Background()
	prime(t, r) // issuance is two-pass; see prime's comment
	if err := r.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	// The ready one carries everything the front needs, above all the CA.
	got, err := db.GetTAKInstance(ctx, "t-ready")
	if err != nil {
		t.Fatalf("no row for the ready tenant: %v", err)
	}
	if got.Label != "aaaaaaaaaa" {
		t.Errorf("label = %q, want aaaaaaaaaa", got.Label)
	}
	if got.Phase != PhaseReady {
		t.Errorf("phase = %q, want Ready", got.Phase)
	}
	if got.Host == "" {
		t.Error("host is empty; the row is meant to answer where the server is")
	}
	if got.CACertPEM != ca.pemStr {
		t.Errorf("ca_cert_pem was not recorded as the operator reported it; takfront identifies "+
			"a phone's tenant by this certificate's issuer.\n got %d bytes\nwant %d bytes",
			len(got.CACertPEM), len(ca.pemStr))
	}
	if got.State != stateRunning {
		t.Errorf("state = %q, want %q", got.State, stateRunning)
	}

	// And the one still being built has a row too. This is the assertion that keeps
	// the write where it is: before the servability check, not after it. A customer
	// asking "where is my TAK server" is almost always asking precisely while their
	// instance is in this state.
	waiting, err := db.GetTAKInstance(ctx, "t-waiting")
	if err != nil {
		t.Fatalf("no row for the provisioning tenant, so nothing can tell its owner that it "+
			"is on its way: %v", err)
	}
	if waiting.Phase != phaseProvisioning {
		t.Errorf("phase = %q, want %q", waiting.Phase, phaseProvisioning)
	}
	if waiting.CACertPEM != "" {
		t.Errorf("a provisioning instance reported no CA, so the row must not invent one: %q",
			waiting.CACertPEM)
	}
}

// A later pass carries the operator's progress through. The row is a cache, and a
// cache that never updates is worse than none: it would report Provisioning after
// the server came up.
func TestALaterRefreshUpdatesTheRecordedPhase(t *testing.T) {
	ca := newTestCA(t, "MeshSat TAK cccccccccc CA")
	api := newFakeAPI(ca)
	api.autoIssue = true
	api.setInstances(provisioningInstance("t-moving", "cccccccccc"))
	srv := httptest.NewServer(api.handler(t))
	t.Cleanup(srv.Close)
	cl := NewClientWith(srv.Client(), srv.URL, "meshsat-tak")

	db := rowStore(t)
	r := NewDirectoryRefresher(cl, NewIdentityKeeper(cl, nil), func(*takfront.Directory) {}, nil)
	r.SetStore(db)

	ctx := context.Background()
	if err := r.Refresh(ctx); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	first, err := db.GetTAKInstance(ctx, "t-moving")
	if err != nil || first.Phase != phaseProvisioning {
		t.Fatalf("first row phase = %v err = %v, want Provisioning", first, err)
	}

	// The operator finishes the job.
	api.setInstances(readyInstance("t-moving", "cccccccccc", ca.pemStr))
	prime(t, r)
	if err := r.Refresh(ctx); err != nil {
		t.Fatalf("second refresh: %v", err)
	}

	got, err := db.GetTAKInstance(ctx, "t-moving")
	if err != nil {
		t.Fatalf("row vanished: %v", err)
	}
	if got.Phase != PhaseReady {
		t.Errorf("phase = %q after the instance became Ready, want Ready", got.Phase)
	}
	if got.CACertPEM == "" {
		t.Error("the CA certificate did not arrive in the row once the operator reported it")
	}
}

// Without a store the refresher behaves exactly as it did before, because the
// directory is built from the custom resources and never from these rows.
func TestARefresherWithNoStoreStillServes(t *testing.T) {
	ca := newTestCA(t, "MeshSat TAK dddddddddd CA")
	api := newFakeAPI(ca)
	api.autoIssue = true
	api.setInstances(readyInstance("t-nostore", "dddddddddd", ca.pemStr))
	srv := httptest.NewServer(api.handler(t))
	t.Cleanup(srv.Close)
	cl := NewClientWith(srv.Client(), srv.URL, "meshsat-tak")

	var published int
	r := NewDirectoryRefresher(cl, NewIdentityKeeper(cl, nil),
		func(d *takfront.Directory) {
			if d != nil {
				published = d.Len()
			}
		}, nil)
	// Deliberately no SetStore.

	prime(t, r)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if published != 1 {
		t.Errorf("published %d tenants, want 1: the rows are optional and must not be load-bearing", published)
	}
}
