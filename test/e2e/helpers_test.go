//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	hubauth "github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/store"
	"github.com/cubeos-app/meshsat-hub/internal/store/sqlite"

	"github.com/cubeos-app/meshsat-hub/internal/api"
	"github.com/cubeos-app/meshsat-hub/internal/audit"
	"github.com/go-chi/chi/v5"
)

// testHarness holds a fully wired E2E test environment with real SQLite store
// and chi router serving the key API handlers.
type testHarness struct {
	server *httptest.Server
	store  store.Store
	router *chi.Mux
	audit  *audit.Service
}

// newHarness creates a testHarness with an in-memory SQLite store,
// migrated schema, and all key handlers mounted on a chi router.
func newHarness(t *testing.T) *testHarness {
	t.Helper()

	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	r := chi.NewRouter()

	// Inject default tenant context (simulates auth=none mode).
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := context.WithValue(req.Context(), hubauth.TenantContextKey, store.DefaultTenantID)
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})

	// Devices
	devH := api.NewDeviceHandler(s)
	r.Get("/api/devices", devH.ListDevices)
	r.Post("/api/devices", devH.CreateDevice)
	r.Get("/api/devices/{imei}", devH.GetDevice)
	r.Put("/api/devices/{imei}", devH.UpdateDevice)
	r.Delete("/api/devices/{imei}", devH.DeleteDevice)

	// Messages
	msgH := api.NewMessageHandler(s)
	r.Get("/api/messages", msgH.ListMessages)
	r.Get("/api/messages/{id}", msgH.GetMessage)

	// Positions
	posH := api.NewPositionHandler(s)
	r.Get("/api/positions/latest", posH.AllLatestPositions)
	r.Get("/api/devices/{imei}/position", posH.LatestPosition)
	r.Get("/api/devices/{imei}/positions", posH.ListPositions)

	// Audit
	auditSvc := audit.New(s)
	auditH := api.NewAuditHandler(auditSvc)
	r.Get("/api/audit", auditH.ListEntries)
	r.Get("/api/audit/verify", auditH.VerifyChain)

	srv := httptest.NewServer(r)

	h := &testHarness{
		server: srv,
		store:  s,
		router: r,
		audit:  auditSvc,
	}

	t.Cleanup(func() { h.close() })
	return h
}

func (h *testHarness) close() {
	h.server.Close()
	h.store.Close()
}
