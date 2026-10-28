package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/directory"
	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/protocol"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
	"github.com/meshsat/meshsat-hub/internal/warnings"
)

// MESHSAT-1734.

type warningsStore struct {
	*mockStore
	rows map[string][]store.TenantWarning
}

func (s *warningsStore) ListTenantWarnings(_ context.Context, tenantID string, _ int) ([]store.TenantWarning, error) {
	return s.rows[tenantID], nil
}

func warningsCtx(tid string) context.Context {
	ctx := context.WithValue(context.Background(), auth.TenantContextKey, tid)
	return context.WithValue(ctx, auth.UserContextKey, &auth.User{ID: "u1", Email: "owner@example.org", Roles: []string{auth.RoleOwner}})
}

func TestWarningsListIsTheTenantsOwn(t *testing.T) {
	s := &warningsStore{mockStore: &mockStore{}, rows: map[string][]store.TenantWarning{
		"t1": {{Warning: store.Warning{Source: "dwd", Identifier: "a", Event: "Wind"}, Bridges: `["kit"]`, MatchedAt: time.Now()}},
	}}
	h := NewWarningsHandler(s, nil, nil)
	for tid, want := range map[string]int{"t1": 1, store.DefaultTenantID: 0} {
		rec := httptest.NewRecorder()
		h.List(rec, httptest.NewRequest(http.MethodGet, "/api/warnings", nil).WithContext(warningsCtx(tid)))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", tid, rec.Code, rec.Body)
		}
		var got []store.TenantWarning
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got) != want {
			t.Fatalf("%s: %v %s", tid, err, rec.Body)
		}
	}
}

func TestWarningsKeyAndTestInjectionWithTheRelayOff(t *testing.T) {
	h := NewWarningsHandler(&mockStore{}, nil, nil)
	rec := httptest.NewRecorder()
	h.Key(rec, httptest.NewRequest(http.MethodGet, "/api/warnings/key", nil).WithContext(warningsCtx("t1")))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatalf("key: %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	h.InjectTest(rec, httptest.NewRequest(http.MethodPost, "/api/warnings/test", strings.NewReader("<alert/>")).WithContext(warningsCtx("t1")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("inject with the relay off: %d", rec.Code)
	}
}

type recordingBus struct{ topics []string }

func (b *recordingBus) Publish(topic string, _ byte, _ bool, _ []byte) error {
	b.topics = append(b.topics, topic)
	return nil
}

type staticAccounts map[string]map[string]string

func (a staticAccounts) TenantsWith(context.Context, string) ([]string, error) {
	var out []string
	for tid := range a {
		out = append(out, tid)
	}
	return out, nil
}

func (a staticAccounts) ForTenant(_ context.Context, tenantID, provider string) (*integrations.Account, error) {
	f, ok := a[tenantID]
	if !ok {
		return nil, nil
	}
	return &integrations.Account{Provider: provider, TenantID: tenantID, Fields: f}, nil
}

func testTrustAnchor(t *testing.T) *directory.TrustAnchor {
	t.Helper()
	anchor, err := directory.LoadOrCreateTrustAnchor(context.Background(), mapKV{})
	if err != nil {
		t.Fatal(err)
	}
	return anchor
}

func TestWarningsTestInjection(t *testing.T) {
	db, err := sqlite.New(t.TempDir()+"/hub.db", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	_ = db.CreateTenant(ctx, &store.Tenant{ID: "t1", Slug: "t1", Name: "t1", Status: store.TenantActive})
	// A kit (its birth declares a mesh) and a phone (birth mode android);
	// only the kit is handed a digest.
	kitBirth, _ := json.Marshal(protocol.BridgeBirth{Mode: "direct", Interfaces: []protocol.InterfaceInfo{{Name: "mesh_0", Type: "meshtastic", Status: "online"}}})
	phoneBirth, _ := json.Marshal(protocol.BridgeBirth{Mode: "android", Interfaces: []protocol.InterfaceInfo{{Name: "ble_mesh_0", Type: "meshtastic", Status: "online"}}})
	for _, b := range []*store.Bridge{{BridgeID: "kit", LastBirth: string(kitBirth)}, {BridgeID: "msa-phone", LastBirth: string(phoneBirth)}} {
		if err := db.CreateOrUpdateBridge(ctx, "t1", b); err != nil {
			t.Fatal(err)
		}
	}
	bus := &recordingBus{}
	accounts := staticAccounts{"t1": {"enabled": "true", "areas": "NL804"}}
	svc := warnings.New(db, bus, accounts, testTrustAnchor(t), nil, nil, time.Minute, nil)
	h := NewWarningsHandler(db, svc, nil)

	rec := httptest.NewRecorder()
	h.Key(rec, httptest.NewRequest(http.MethodGet, "/api/warnings/key", nil).WithContext(warningsCtx("t1")))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"alg":"ES256"`) || !strings.Contains(rec.Body.String(), `"pkix_hex":"3059`) {
		t.Fatalf("key: %d %s", rec.Code, rec.Body)
	}

	doc := `<alert xmlns="urn:oasis:names:tc:emergency:cap:1.2"><identifier>t-1</identifier><sender>o@example.org</sender><sent>2026-10-28T12:00:00+00:00</sent>
	<status>Test</status><msgType>Alert</msgType><info><event>Relay test</event><severity>Severe</severity><headline>Relay test</headline>
	<area><geocode><valueName>EMMA_ID</valueName><value>NL804</value></geocode></area></info></alert>`
	rec = httptest.NewRecorder()
	h.InjectTest(rec, httptest.NewRequest(http.MethodPost, "/api/warnings/test", strings.NewReader(doc)).WithContext(warningsCtx("t1")))
	if rec.Code != http.StatusOK {
		t.Fatalf("inject: %d %s", rec.Code, rec.Body)
	}
	var res warnings.InjectResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || len(res.Deliveries) != 1 || len(res.Deliveries[0].Bridges) != 1 ||
		res.Deliveries[0].Bridges[0] != "kit" || len(res.Deliveries[0].Skipped) != 1 || res.Deliveries[0].Skipped[0] != "msa-phone: a phone" {
		t.Fatalf("result: %v %s", err, rec.Body)
	}
	if len(bus.topics) != 1 || bus.topics[0] != "meshsat/t1/bridge/kit/warn/in" {
		t.Fatalf("published: %v", bus.topics)
	}
	// Not a test: 400.
	rec = httptest.NewRecorder()
	h.InjectTest(rec, httptest.NewRequest(http.MethodPost, "/api/warnings/test", strings.NewReader(strings.Replace(doc, "Test</status>", "Actual</status>", 1))).WithContext(warningsCtx("t1")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an Actual document: %d %s", rec.Code, rec.Body)
	}
	// Over the body limit: 413.
	rec = httptest.NewRecorder()
	h.InjectTest(rec, httptest.NewRequest(http.MethodPost, "/api/warnings/test", strings.NewReader(strings.Repeat("x", maxTestCAPBytes+1))).WithContext(warningsCtx("t1")))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized: %d", rec.Code)
	}
}
