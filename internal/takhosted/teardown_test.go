package takhosted

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/tenancy"
)

// The Client satisfies the interface the purge job asks for. If this stops
// compiling, the teardown is no longer reachable from a purge and a closed
// account keeps its TAK server.
var _ tenancy.TAKInstanceDeleter = (*Client)(nil)

func teardownClient(t *testing.T, api *fakeAPI) *Client {
	t.Helper()
	srv := httptest.NewServer(api.handler(t))
	t.Cleanup(srv.Close)
	return NewClientWith(srv.Client(), srv.URL, "meshsat-tak")
}

// The whole point of the teardown, and it is an ORDER, not a pair of calls.
//
// A TakInstance deleted without the purge marker keeps its CNPG Database and
// DatabaseRole: they carry a reclaim policy of retain, and only the marker
// switches them to delete. Delete first and the object is gone, so there is
// nothing left to mark -- the customer's TAK database would survive the erasure
// permanently, with nothing in the Hub recording that it exists.
func TestATenantsTAKServerIsMarkedForDataDestructionBeforeItIsDeleted(t *testing.T) {
	ca := newTestCA(t, "MeshSat TAK aaaaaaaaaa CA")
	api := newFakeAPI(ca)
	api.setInstances(readyInstance("t-closed", "aaaaaaaaaa", ca.pemStr))
	cl := teardownClient(t, api)

	if err := cl.DeleteTAKInstanceForTenant(context.Background(), "t-closed"); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	want := []string{
		"patch:tak-aaaaaaaaaa:" + PurgeAnnotation + "=true",
		"delete:tak-aaaaaaaaaa",
	}
	if got := api.ops(); !reflect.DeepEqual(got, want) {
		t.Errorf("operations were\n  %v\nwant\n  %v\nthe marker must be set BEFORE the delete, "+
			"or the tenant's database is retained for good", got, want)
	}
	if got := api.instanceNames(); len(got) != 0 {
		t.Errorf("the instance survived the teardown: %v", got)
	}
}

// If the marker cannot be written, the instance must NOT be deleted. Reporting
// the failure is what keeps the purge job from destroying the tenant's rows,
// so the account stays whole and the next run tries again.
func TestATeardownThatCannotMarkTheInstanceRefusesToDeleteIt(t *testing.T) {
	ca := newTestCA(t, "MeshSat TAK bbbbbbbbbb CA")
	api := newFakeAPI(ca)
	api.setInstances(readyInstance("t-closed", "bbbbbbbbbb", ca.pemStr))
	api.instPatchErr = true
	cl := teardownClient(t, api)

	err := cl.DeleteTAKInstanceForTenant(context.Background(), "t-closed")
	if err == nil {
		t.Fatal("a teardown whose marker write failed reported success; the purge would then " +
			"destroy the tenant's rows and leave its TAK server running")
	}
	for _, op := range api.ops() {
		if strings.HasPrefix(op, "delete:") {
			t.Errorf("an UNMARKED instance was deleted: %v", api.ops())
		}
	}
	if got := api.instanceNames(); len(got) != 1 {
		t.Errorf("instances present = %v, want the one still there for a retry", got)
	}
}

// Most tenants never turn TAK on, and a retry after a partial failure finds
// nothing left. Both are success: anything else would wedge the purge job.
func TestATeardownForATenantWithNoTAKServerSucceeds(t *testing.T) {
	ca := newTestCA(t, "MeshSat TAK cccccccccc CA")
	api := newFakeAPI(ca)
	api.setInstances(readyInstance("somebody-else", "cccccccccc", ca.pemStr))
	cl := teardownClient(t, api)

	if err := cl.DeleteTAKInstanceForTenant(context.Background(), "t-no-tak"); err != nil {
		t.Fatalf("teardown of a tenant with no instance returned %v, want nil", err)
	}
	if got := api.ops(); len(got) != 0 {
		t.Errorf("operations were %v, want none", got)
	}
	if got := api.instanceNames(); len(got) != 1 {
		t.Errorf("another tenant's instance was touched: %v", got)
	}
}

// Only the named tenant's instance is destroyed. This is a purge, so getting it
// wrong destroys a paying customer's server.
func TestOnlyTheNamedTenantsInstanceIsDestroyed(t *testing.T) {
	ca := newTestCA(t, "MeshSat TAK dddddddddd CA")
	api := newFakeAPI(ca)
	api.setInstances(
		readyInstance("t-closed", "dddddddddd", ca.pemStr),
		readyInstance("t-paying", "eeeeeeeeee", ca.pemStr),
	)
	cl := teardownClient(t, api)

	if err := cl.DeleteTAKInstanceForTenant(context.Background(), "t-closed"); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	got := api.instanceNames()
	if len(got) != 1 || got[0] != "tak-eeeeeeeeee" {
		t.Errorf("instances left = %v, want only the other tenant's tak-eeeeeeeeee", got)
	}
	for _, op := range api.ops() {
		if strings.Contains(op, "eeeeeeeeee") {
			t.Errorf("the other tenant's instance was operated on: %v", api.ops())
		}
	}
}

// An empty tenant id is refused before anything is listed. A bug upstream that
// passed one through must not turn into "match every instance".
func TestAnEmptyTenantIDIsRefusedBeforeAnythingIsTouched(t *testing.T) {
	ca := newTestCA(t, "MeshSat TAK ffffffffff CA")
	api := newFakeAPI(ca)
	api.setInstances(readyInstance("t-paying", "ffffffffff", ca.pemStr))
	cl := teardownClient(t, api)

	if err := cl.DeleteTAKInstanceForTenant(context.Background(), ""); err == nil {
		t.Error("an empty tenant id was accepted")
	}
	if got := api.ops(); len(got) != 0 {
		t.Errorf("operations were %v, want none", got)
	}
	if got := api.instanceNames(); len(got) != 1 {
		t.Errorf("instances left = %v, want the untouched one", got)
	}
}

// A failure to LIST must be reported, not read as "this tenant has no server".
// Silently succeeding would let the purge destroy the rows while the server ran
// on.
func TestATeardownReportsAListFailureRatherThanAssumingNothingExists(t *testing.T) {
	ca := newTestCA(t, "MeshSat TAK gggggggggg CA")
	api := newFakeAPI(ca)
	api.setInstances(readyInstance("t-closed", "gggggggggg", ca.pemStr))
	api.listErr = true
	cl := teardownClient(t, api)

	if err := cl.DeleteTAKInstanceForTenant(context.Background(), "t-closed"); err == nil {
		t.Error("a failed list reported success, which would let the purge proceed")
	}
}

// The purge annotation is duplicated rather than imported, because this package
// must not reach the operator's CA material (see boundary_test.go). A duplicated
// constant drifts, so this reads the operator's source as a FILE -- no import --
// and fails when the two no longer agree.
//
// Getting this wrong is invisible at runtime: the Hub would mark instances with a
// string the operator ignores, every teardown would report success, and every
// closed account would keep its database.
func TestThePurgeAnnotationStillMatchesTheOperators(t *testing.T) {
	path := filepath.Join("..", "takoperator", "types.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	want := `PurgeAnnotation = "` + PurgeAnnotation + `"`
	if !strings.Contains(string(src), want) {
		t.Errorf("%s does not declare %s.\nThe operator owns the authoritative copy and this "+
			"package keeps its own because it may not import that one. They have drifted: the Hub "+
			"would mark instances with a key the operator does not read, so every purge would "+
			"report success and keep the customer's TAK database.", path, want)
	}
}
