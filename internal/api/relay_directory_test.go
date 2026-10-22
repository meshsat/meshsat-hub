package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/directory"
	"github.com/meshsat/meshsat-hub/internal/relay"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
)

// casKV is a key store a rotation can run on.
type casKV struct {
	mu sync.Mutex
	m  map[string]string
}

func (k *casKV) GetSystemConfig(_ context.Context, key string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.m[key], nil
}

func (k *casKV) SetSystemConfig(_ context.Context, key, v string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[key] = v
	return nil
}

func (k *casKV) CompareAndSetSystemConfig(_ context.Context, key, old, v string) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.m[key] != old {
		return false, nil
	}
	k.m[key] = v
	return true, nil
}

type relayDirRig struct {
	srv    *httptest.Server
	dir    *directory.SQLStore
	kv     *casKV
	anchor *directory.TrustAnchor
}

// relayDirectoryRig: the relay fixture's tenants and bridges, a directory in
// which acme has two contacts and other has one, and the Hub's signing key.
// configured=false leaves the directory off the relay.
func relayDirectoryRig(t *testing.T, configured bool) *relayDirRig {
	t.Helper()
	ctx := context.Background()
	db := relayDB(t)
	sdb, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "dir.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	sdb.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sdb.Close() })
	dir := directory.NewSQLStore(sdb)
	if err := dir.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, c := range []directory.Contact{
		{ID: "c1", TenantID: "acme", DisplayName: "One"},
		{ID: "c2", TenantID: "acme", DisplayName: "Two"},
		{ID: "c9", TenantID: "other", DisplayName: "Nine"},
	} {
		if err := dir.PutContact(ctx, &c); err != nil {
			t.Fatal(err)
		}
	}
	kv := &casKV{m: map[string]string{}}
	anchor, err := directory.LoadOrCreateTrustAnchor(ctx, kv)
	if err != nil {
		t.Fatal(err)
	}
	h := NewRelayHandler(db, tenancy.NewResolver(db, store.DefaultTenantID, 0), relay.New(relayNoBus{}, nil, relay.Options{}))
	if configured {
		h.SetDirectory(dir, anchor)
	}
	r := chi.NewRouter()
	r.Get("/api/relay/directory", h.Directory)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return &relayDirRig{srv: srv, dir: dir, kv: kv, anchor: anchor}
}

func (g *relayDirRig) get(t *testing.T, query string, hdr http.Header) (int, RelayDirectoryAnswer) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, g.srv.URL+"/api/relay/directory"+query, nil)
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var ans RelayDirectoryAnswer
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&ans); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, ans
}

// openUnder verifies an envelope under one public key.
func openUnder(t *testing.T, env *directory.Envelope, der []byte) []byte {
	t.Helper()
	payload, err := directory.OpenEnvelope(env, func(id string) []byte {
		if id == directory.KeyID(der) {
			return der
		}
		return nil
	})
	if err != nil {
		t.Fatalf("envelope does not verify under the key: %v", err)
	}
	return payload
}

type relaySnapPayload struct {
	Type     string `json:"type"`
	TenantID string `json:"tenant_id"`
	Version  int64  `json:"version"`
	Contacts []struct {
		ID string `json:"id"`
	} `json:"contacts"`
}

func TestRelayDirectoryNeedsBridgeCredentials(t *testing.T) {
	g := relayDirectoryRig(t, true)
	if code, _ := g.get(t, "", nil); code != http.StatusUnauthorized {
		t.Fatalf("no credentials: %d, want 401", code)
	}
	if code, _ := g.get(t, "", basic("phone-1", "kit-a-pw")); code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d, want 401", code)
	}
}

// A phone gets its own tenant's directory, signed by the Hub's key, and
// nothing of another tenant's.
func TestRelayDirectoryServesTheTenantsSignedSnapshot(t *testing.T) {
	g := relayDirectoryRig(t, true)
	code, ans := g.get(t, "", basic("phone-1", "phone-1-pw"))
	if code != http.StatusOK || ans.Snapshot == nil {
		t.Fatalf("phone-1: %d, snapshot %v", code, ans.Snapshot)
	}
	if len(ans.Rotations) != 0 {
		t.Fatalf("rotations without a key_id: %d", len(ans.Rotations))
	}
	var p relaySnapPayload
	if err := json.Unmarshal(openUnder(t, ans.Snapshot, g.anchor.PublicKey()), &p); err != nil {
		t.Fatal(err)
	}
	if p.TenantID != "acme" || p.Version < 2 || len(p.Contacts) != 2 {
		t.Fatalf("payload tenant %q version %d contacts %d, want acme, >=2, 2", p.TenantID, p.Version, len(p.Contacts))
	}
	for _, c := range p.Contacts {
		if c.ID == "c9" {
			t.Fatal("the other tenant's contact is in acme's snapshot")
		}
	}

	code, ans = g.get(t, "", basic("kit-b", "kit-b-pw"))
	if code != http.StatusOK || ans.Snapshot == nil {
		t.Fatalf("kit-b: %d", code)
	}
	if err := json.Unmarshal(openUnder(t, ans.Snapshot, g.anchor.PublicKey()), &p); err != nil {
		t.Fatal(err)
	}
	if p.TenantID != "other" || len(p.Contacts) != 1 || p.Contacts[0].ID != "c9" {
		t.Fatalf("kit-b got tenant %q with %d contacts", p.TenantID, len(p.Contacts))
	}
}

// since at the current version answers 304; one below it the snapshot.
func TestRelayDirectorySinceAndNotModified(t *testing.T) {
	g := relayDirectoryRig(t, true)
	_, ans := g.get(t, "", basic("phone-1", "phone-1-pw"))
	var p relaySnapPayload
	if err := json.Unmarshal(openUnder(t, ans.Snapshot, g.anchor.PublicKey()), &p); err != nil {
		t.Fatal(err)
	}
	cur := g.anchor.KeyID()
	if code, _ := g.get(t, "?since="+strconv.FormatInt(p.Version, 10)+"&key_id="+cur, basic("phone-1", "phone-1-pw")); code != http.StatusNotModified {
		t.Fatalf("since=current: %d, want 304", code)
	}
	if code, ans := g.get(t, "?since="+strconv.FormatInt(p.Version-1, 10), basic("phone-1", "phone-1-pw")); code != http.StatusOK || ans.Snapshot == nil {
		t.Fatalf("since=current-1: %d, want 200 with the snapshot", code)
	}
	if code, _ := g.get(t, "?since=abc", basic("phone-1", "phone-1-pw")); code != http.StatusBadRequest {
		t.Fatalf("since=abc: %d, want 400", code)
	}
	if code, _ := g.get(t, "?since=-1", basic("phone-1", "phone-1-pw")); code != http.StatusBadRequest {
		t.Fatalf("since=-1: %d, want 400", code)
	}
}

// After a key rotation a phone that names the old key gets the record signed
// by the old key that names the new one, then the snapshot under the new key:
// what it needs to move its pin without trusting the network.
func TestRelayDirectoryRotationChainLeadsToTheNewKey(t *testing.T) {
	g := relayDirectoryRig(t, true)
	ctx := context.Background()
	oldDER := g.anchor.PublicKey()
	oldID := g.anchor.KeyID()
	_, before := g.get(t, "", basic("phone-1", "phone-1-pw"))
	var held relaySnapPayload
	if err := json.Unmarshal(openUnder(t, before.Snapshot, oldDER), &held); err != nil {
		t.Fatal(err)
	}
	if _, err := directory.RotateSigningKey(ctx, g.kv, g.dir, g.anchor, []string{"acme", "other"}); err != nil {
		t.Fatal(err)
	}
	newID := g.anchor.KeyID()
	if newID == oldID {
		t.Fatal("the rotation did not change the key")
	}

	code, ans := g.get(t, "?since="+strconv.FormatInt(held.Version, 10)+"&key_id="+oldID, basic("phone-1", "phone-1-pw"))
	if code != http.StatusOK || len(ans.Rotations) != 1 || ans.Snapshot == nil {
		t.Fatalf("after rotation: %d, %d rotations, snapshot %v", code, len(ans.Rotations), ans.Snapshot != nil)
	}
	var rot directory.RotationPayload
	if err := json.Unmarshal(openUnder(t, &ans.Rotations[0], oldDER), &rot); err != nil {
		t.Fatal(err)
	}
	if rot.TenantID != "acme" || rot.NewKeyID != newID || directory.KeyID(rot.NewKeyPKIX) != newID {
		t.Fatalf("record: tenant %q new key %s, want acme %s", rot.TenantID, rot.NewKeyID, newID)
	}
	var p relaySnapPayload
	if err := json.Unmarshal(openUnder(t, ans.Snapshot, rot.NewKeyPKIX), &p); err != nil {
		t.Fatal(err)
	}
	if p.Version <= held.Version || ans.Snapshot.KeyID != newID {
		t.Fatalf("snapshot version %d key %s, want > %d under %s", p.Version, ans.Snapshot.KeyID, held.Version, newID)
	}

	// The phone that already moved its pin gets no record again.
	if code, _ := g.get(t, "?since="+strconv.FormatInt(p.Version, 10)+"&key_id="+newID, basic("phone-1", "phone-1-pw")); code != http.StatusNotModified {
		t.Fatalf("pinned to the new key at the current version: %d, want 304", code)
	}
}

func TestRelayDirectoryOffWhenNotConfigured(t *testing.T) {
	g := relayDirectoryRig(t, false)
	if code, _ := g.get(t, "", basic("phone-1", "phone-1-pw")); code != http.StatusServiceUnavailable {
		t.Fatalf("no directory: %d, want 503", code)
	}
	if code, _ := g.get(t, "", nil); code != http.StatusUnauthorized {
		t.Fatalf("no directory, no credentials: %d, want 401", code)
	}
}
