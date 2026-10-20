package directorytest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"sync"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/directory"
	"github.com/meshsat/meshsat-hub/internal/sealedconfig"
)

// The rotation fence across generations (MESHSAT-1584 r20 C7-R7): a caller
// delayed past another caller that finished its rotation, and past a newer
// rotation after that, must neither roll the current key back nor clear the
// newer rotation's pending key. Run on every driver, with the plain key store
// and with the sealed one production uses.

// KeyStore is what a rotation needs of the Hub's key store.
type KeyStore interface {
	GetSystemConfig(ctx context.Context, key string) (string, error)
	SetSystemConfig(ctx context.Context, key, value string) error
	CompareAndSetSystemConfig(ctx context.Context, key, old, value string) (bool, error)
}

// Rig opens a fresh directory store and an empty key store for one case.
type Rig func(t *testing.T) (*directory.SQLStore, KeyStore)

const (
	signingKey = "directory_signing_key"
	pendingKey = "directory_signing_key_next"
)

// hookedKV runs before once, just before the first compare-and-set of key
// that sets a value (clear false) or clears it (clear true); fail makes that
// compare-and-set fail instead.
type hookedKV struct {
	KeyStore
	mu     sync.Mutex
	key    string
	clear  bool
	before func()
	fail   bool
}

func (h *hookedKV) CompareAndSetSystemConfig(ctx context.Context, key, old, value string) (bool, error) {
	h.mu.Lock()
	hit := (h.before != nil || h.fail) && key == h.key && (value == "") == h.clear
	f, fail := h.before, h.fail
	if hit {
		h.before, h.fail = nil, false
	}
	h.mu.Unlock()
	if hit && fail {
		return false, errors.New("store unavailable")
	}
	if hit && f != nil {
		f()
	}
	return h.KeyStore.CompareAndSetSystemConfig(ctx, key, old, value)
}

// SealedKeyStore puts the sensitive keys behind a sealedconfig keeper over
// inner, the way the Hub's sealedStore decorator does
// (cmd/meshsat-hub/sealedstore.go; the decorator itself is tested there).
func SealedKeyStore(inner KeyStore, wrapKey []byte) KeyStore {
	return &sealedKV{inner: inner, k: sealedconfig.New(inner, wrapKey)}
}

type sealedKV struct {
	inner KeyStore
	k     *sealedconfig.Keeper
}

func (s *sealedKV) GetSystemConfig(ctx context.Context, key string) (string, error) {
	if !sealedconfig.IsSensitive(key) {
		return s.inner.GetSystemConfig(ctx, key)
	}
	v, err := s.k.Get(ctx, key)
	if errors.Is(err, sealedconfig.ErrNotFound) {
		return s.inner.GetSystemConfig(ctx, key)
	}
	return v, err
}

func (s *sealedKV) SetSystemConfig(ctx context.Context, key, value string) error {
	if !sealedconfig.IsSensitive(key) {
		return s.inner.SetSystemConfig(ctx, key, value)
	}
	return s.k.Set(ctx, key, value)
}

func (s *sealedKV) CompareAndSetSystemConfig(ctx context.Context, key, old, value string) (bool, error) {
	if !sealedconfig.IsSensitive(key) {
		return s.inner.CompareAndSetSystemConfig(ctx, key, old, value)
	}
	return s.k.CompareAndSet(ctx, key, old, value)
}

// fenceCase is one case's state.
type fenceCase struct {
	t      *testing.T
	ctx    context.Context
	s      *directory.SQLStore
	kv     KeyStore
	tenant string
	orig   string // the key the case starts from
	v0     int64
}

func newFenceCase(t *testing.T, rig Rig, tenant string) *fenceCase {
	s, kv := rig(t)
	c := &fenceCase{t: t, ctx: context.Background(), s: s, kv: kv, tenant: tenant}
	a := c.anchor()
	c.orig = a.KeyID()
	mustPut(t, s, &directory.Contact{TenantID: tenant, DisplayName: "A"})
	c.v0 = version(t, s, tenant)
	return c
}

// anchor is a replica starting now: the current key as stored.
func (c *fenceCase) anchor() *directory.TrustAnchor {
	c.t.Helper()
	a, err := directory.LoadOrCreateTrustAnchor(c.ctx, c.kv)
	if err != nil {
		c.t.Fatal(err)
	}
	return a
}

func (c *fenceCase) rotate(kv KeyStore, a *directory.TrustAnchor) (*directory.RotationResult, error) {
	return directory.RotateSigningKey(c.ctx, kv, c.s, a, []string{c.tenant})
}

func (c *fenceCase) mustRotate(a *directory.TrustAnchor) *directory.RotationResult {
	c.t.Helper()
	res, err := c.rotate(c.kv, a)
	if err != nil {
		c.t.Fatal(err)
	}
	return res
}

func (c *fenceCase) pending() string {
	v, err := c.kv.GetSystemConfig(c.ctx, pendingKey)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		c.t.Fatal(err)
	}
	return v
}

// consistent checks the end state: the stored current key is the newest
// tip of the tenant's chain, the chain names the keys given in order, one
// version per transition, and a Bridge that trusts the tip can verify the
// next snapshot.
func (c *fenceCase) consistent(chainKeys ...string) {
	c.t.Helper()
	chain, err := c.s.RotationChain(c.ctx, c.tenant, c.orig)
	if err != nil {
		c.t.Fatal(err)
	}
	var got []string
	for _, r := range chain {
		got = append(got, r.NewKeyID)
	}
	if len(got) != len(chainKeys) {
		c.t.Fatalf("chain %v, want %v", got, chainKeys)
	}
	for i := range got {
		if got[i] != chainKeys[i] {
			c.t.Fatalf("chain %v, want %v", got, chainKeys)
		}
	}
	tip := chainKeys[len(chainKeys)-1]
	if cur := c.anchor().KeyID(); cur != tip {
		c.t.Fatalf("current key %s, the chain's tip is %s: rolled back", cur, tip)
	}
	if v := version(c.t, c.s, c.tenant); v != c.v0+int64(len(chainKeys)) {
		c.t.Errorf("version %d, want %d (one per transition)", v, c.v0+int64(len(chainKeys)))
	}
	// The tip's public key, from the record that names it.
	var p directory.RotationPayload
	if err := json.Unmarshal(chain[len(chain)-1].Envelope.Payload, &p); err != nil {
		c.t.Fatal(err)
	}
	snap, err := c.s.Snapshot(c.ctx, c.tenant)
	if err != nil {
		c.t.Fatal(err)
	}
	env, err := c.anchor().SealSnapshot(snap)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := directory.OpenEnvelope(env, func(id string) []byte {
		if id == tip {
			return p.NewKeyPKIX
		}
		return nil
	}); err != nil {
		c.t.Fatalf("a Bridge that trusts the tip cannot verify the next snapshot: %v", err)
	}
}

// RotationFence runs the cases, each on its own rig.
func RotationFence(t *testing.T, rig Rig) {
	t.Run("delayed activation after a newer rotation", func(t *testing.T) {
		c := newFenceCase(t, rig, "fence-delayed")
		a := c.anchor()
		var mid, latest string
		hk := &hookedKV{KeyStore: c.kv, key: signingKey, before: func() {
			b := c.anchor()
			mid = c.mustRotate(b).NewKeyID    // finishes a's rotation
			latest = c.mustRotate(b).NewKeyID // and a newer one
		}}
		_, err := c.rotate(hk, a)
		if !errors.Is(err, directory.ErrRotationInProgress) {
			t.Fatalf("the delayed caller: %v; want ErrRotationInProgress", err)
		}
		if a.KeyID() != latest {
			t.Errorf("the delayed caller's anchor holds %s, the store %s", a.KeyID(), latest)
		}
		if p := c.pending(); p != "" {
			t.Error("a pending key was left")
		}
		c.consistent(mid, latest)
	})
	t.Run("a stale clean-up leaves a newer pending key", func(t *testing.T) {
		c := newFenceCase(t, rig, "fence-cleanup")
		a := c.anchor()
		newer := pemKey(t)
		var first string
		hk := &hookedKV{KeyStore: c.kv, key: pendingKey, clear: true, before: func() {
			b := c.anchor()
			res := c.mustRotate(b) // finds a's key current: only the clean-up
			first = res.NewKeyID
			// A newer rotation stores its key and has not finished yet.
			if won, err := c.kv.CompareAndSetSystemConfig(c.ctx, pendingKey, "", newer); err != nil || !won {
				t.Fatalf("store the newer pending key: %v %v", won, err)
			}
		}}
		res, err := c.rotate(hk, a)
		if err != nil || res.NewKeyID != first {
			t.Fatalf("the stale caller: %+v %v", res, err)
		}
		if c.pending() != newer {
			t.Fatal("the stale caller cleared the newer rotation's pending key")
		}
		latest := c.mustRotate(c.anchor()).NewKeyID // the newer one resumes
		c.consistent(first, latest)
	})
	t.Run("an activation that failed, finished elsewhere, then a newer one", func(t *testing.T) {
		c := newFenceCase(t, rig, "fence-failed")
		a := c.anchor()
		hk := &hookedKV{KeyStore: c.kv, key: signingKey, fail: true}
		if _, err := c.rotate(hk, a); err == nil || errors.Is(err, directory.ErrRotationInProgress) {
			t.Fatalf("the failed activation: %v", err)
		}
		restarted := c.anchor() // a replica started after the failure
		mid := c.mustRotate(restarted)
		if !mid.Resumed {
			t.Errorf("the restarted caller did not resume the stored key: %+v", mid)
		}
		latest := c.mustRotate(restarted).NewKeyID
		// The failed caller's anchor is still on the first key; asked
		// again it follows the store and rotates from the newest key.
		again, err := c.rotate(c.kv, a)
		if err != nil {
			t.Fatal(err)
		}
		c.consistent(mid.NewKeyID, latest, again.NewKeyID)
	})
	t.Run("a caller between activation and clean-up joins the generation", func(t *testing.T) {
		c := newFenceCase(t, rig, "fence-between")
		a := c.anchor()
		var late *directory.RotationResult
		hk := &hookedKV{KeyStore: c.kv, key: pendingKey, clear: true, before: func() {
			late = c.mustRotate(c.anchor()) // the key is current, the pending one not cleared yet
		}}
		res, err := c.rotate(hk, a)
		if err != nil || late == nil || !late.Resumed || late.NewKeyID != res.NewKeyID || late.Records != 0 {
			t.Fatalf("the first caller %+v %v; the late one %+v: it must finish this generation, not start one", res, err, late)
		}
		if p := c.pending(); p != "" {
			t.Error("a pending key was left")
		}
		c.consistent(res.NewKeyID)
	})
	t.Run("two callers resume one generation", func(t *testing.T) {
		c := newFenceCase(t, rig, "fence-same")
		a := c.anchor()
		var other string
		hk := &hookedKV{KeyStore: c.kv, key: signingKey, before: func() {
			other = c.mustRotate(c.anchor()).NewKeyID
		}}
		res, err := c.rotate(hk, a)
		if err != nil || res.NewKeyID != other || !res.Resumed || a.KeyID() != other {
			t.Fatalf("the second caller of one generation: %+v %v; the first made %s current", res, err, other)
		}
		if p := c.pending(); p != "" {
			t.Error("a pending key was left")
		}
		c.consistent(other)
	})
}

func pemKey(t *testing.T) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}

// memRig gives each case the run's directory store and a fresh in-memory key
// store; the cases use their own tenants.
func memRig(s *directory.SQLStore) Rig {
	return func(*testing.T) (*directory.SQLStore, KeyStore) { return s, &memKV{m: map[string]string{}} }
}
