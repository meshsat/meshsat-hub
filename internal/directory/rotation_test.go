package directory

import (
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func rotationRig(t *testing.T) (*SQLStore, *fakeStore, *TrustAnchor) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "dir.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	s := NewSQLStore(db)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	kv := newFakeStore()
	a, err := LoadOrCreateTrustAnchor(context.Background(), kv)
	if err != nil {
		t.Fatal(err)
	}
	return s, kv, a
}

// openRotation verifies a record under the key it says it rotates away from
// and returns its payload.
func openRotation(t *testing.T, r Rotation, oldPKIX []byte) RotationPayload {
	t.Helper()
	payload, err := OpenEnvelope(&r.Envelope, func(id string) []byte {
		if id == KeyID(oldPKIX) {
			return oldPKIX
		}
		return nil
	})
	if err != nil {
		t.Fatalf("record %d does not verify under the old key: %v", r.Version, err)
	}
	var p RotationPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRotateSigningKeyWritesASignedRecordPerTenant(t *testing.T) {
	s, kv, a := rotationRig(t)
	ctx := context.Background()
	if err := s.PutContact(ctx, &Contact{TenantID: "t1", DisplayName: "A"}); err != nil {
		t.Fatal(err)
	}
	oldPKIX, oldStored := a.PublicKey(), kv.data[systemConfigKey]
	res, err := RotateSigningKey(ctx, kv, s, a, []string{"t1", "t2"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Records != 2 || res.OldKeyID != KeyID(oldPKIX) || res.NewKeyID != a.KeyID() || res.NewKeyID == res.OldKeyID {
		t.Fatalf("result %+v", res)
	}
	if kv.data[systemConfigKey] == oldStored || kv.data[nextKeyConfig] != "" {
		t.Error("the new key is not current, or the pending one was not cleared")
	}
	for tenant, wantVersion := range map[string]int64{"t1": 2, "t2": 1} {
		chain, err := s.RotationChain(ctx, tenant, res.OldKeyID)
		if err != nil || len(chain) != 1 {
			t.Fatalf("%s chain: %v %v", tenant, chain, err)
		}
		p := openRotation(t, chain[0], oldPKIX)
		if p.Type != PayloadRotation || p.TenantID != tenant || p.Version != wantVersion || p.NewKeyID != res.NewKeyID ||
			KeyID(p.NewKeyPKIX) != res.NewKeyID {
			t.Errorf("%s record %+v", tenant, p)
		}
		if v, _ := s.CurrentVersion(ctx, tenant); v != wantVersion {
			t.Errorf("%s version %d, want %d (the record takes a slot)", tenant, v, wantVersion)
		}
	}
	// What the anchor signs now verifies under the new key only.
	env, _ := a.Seal([]byte(`{}`))
	if env.KeyID != res.NewKeyID {
		t.Errorf("sealing with %s after the rotation", env.KeyID)
	}
	// A second rotation extends the chain from the first key.
	res2, err := RotateSigningKey(ctx, kv, s, a, []string{"t1", "t2"})
	if err != nil {
		t.Fatal(err)
	}
	chain, _ := s.RotationChain(ctx, "t1", res.OldKeyID)
	if len(chain) != 2 || chain[0].NewKeyID != res.NewKeyID || chain[1].OldKeyID != res.NewKeyID || chain[1].NewKeyID != res2.NewKeyID {
		t.Fatalf("two-step chain: %+v", chain)
	}
	if c, _ := s.RotationChain(ctx, "t1", res2.NewKeyID); len(c) != 0 {
		t.Errorf("chain from the newest key: %+v", c)
	}
	if c, _ := s.RotationChain(ctx, "t1", "unknown"); len(c) != 0 {
		t.Errorf("chain from an unknown key: %+v", c)
	}
}

// TestRotateSigningKeyResumesWhereItStopped: a crash at any step leaves a
// state the next call finishes, with the key its records already name and
// no second record for a tenant.
func TestRotateSigningKeyResumesWhereItStopped(t *testing.T) {
	ctx := context.Background()
	t.Run("before the key became current", func(t *testing.T) {
		s, kv, a := rotationRig(t)
		oldID := a.KeyID()
		kv.failKey = systemConfigKey
		res, err := RotateSigningKey(ctx, kv, s, a, []string{"t1", "t2"})
		if err == nil || res.Records != 2 || a.KeyID() != oldID {
			t.Fatalf("crash at step 3: %+v %v, key %s", res, err, a.KeyID())
		}
		named := res.NewKeyID
		kv.failKey = ""
		// A restarted replica: a fresh anchor from the store.
		a2, err := LoadOrCreateTrustAnchor(ctx, kv)
		if err != nil || a2.KeyID() != oldID {
			t.Fatalf("after the crash the stored key is %s, want the old %s (%v)", a2.KeyID(), oldID, err)
		}
		res, err = RotateSigningKey(ctx, kv, s, a2, []string{"t1", "t2"})
		if err != nil || !res.Resumed || res.Records != 0 || res.NewKeyID != named || a2.KeyID() != named {
			t.Fatalf("resume: %+v %v", res, err)
		}
		for _, tenant := range []string{"t1", "t2"} {
			if c, _ := s.RotationChain(ctx, tenant, oldID); len(c) != 1 {
				t.Errorf("%s has %d records", tenant, len(c))
			}
		}
	})
	t.Run("after the key became current", func(t *testing.T) {
		s, kv, a := rotationRig(t)
		kv.failClear = nextKeyConfig // clearing the pending key fails, after step 3
		res, err := RotateSigningKey(ctx, kv, s, a, []string{"t1"})
		if err == nil || res.Records != 1 || a.KeyID() != res.NewKeyID {
			t.Fatalf("crash at the clean-up: %+v %v", res, err)
		}
		kv.failClear = ""
		res2, err := RotateSigningKey(ctx, kv, s, a, []string{"t1"})
		if err != nil || !res2.Resumed || res2.Records != 0 || res2.NewKeyID != res.NewKeyID || kv.data[nextKeyConfig] != "" {
			t.Fatalf("clean-up resume: %+v %v", res2, err)
		}
		if c, _ := s.RotationChain(ctx, "t1", res.NewKeyID); len(c) != 0 {
			t.Errorf("the clean-up wrote a record from the new key to itself: %+v", c)
		}
	})
	t.Run("another caller's key is pending", func(t *testing.T) {
		// A caller that stored its key and died before anything else:
		// the next one, on another replica, carries on with that key.
		s, kv, a := rotationRig(t)
		oldID := a.KeyID()
		_, pemBytes, err := generateECPrivateKeyPEM()
		if err != nil {
			t.Fatal(err)
		}
		kv.data[nextKeyConfig] = string(pemBytes)
		want, _ := parseECPrivateKeyPEM(pemBytes)
		wantPKIX, _ := x509.MarshalPKIXPublicKey(&want.PublicKey)
		a2, _ := LoadOrCreateTrustAnchor(ctx, kv)
		res, err := RotateSigningKey(ctx, kv, s, a2, []string{"t1"})
		if err != nil || !res.Resumed || res.NewKeyID != KeyID(wantPKIX) || a2.KeyID() != res.NewKeyID {
			t.Fatalf("resume with the stored key: %+v %v", res, err)
		}
		if c, _ := s.RotationChain(ctx, "t1", oldID); len(c) != 1 || c[0].NewKeyID != res.NewKeyID {
			t.Errorf("chain %+v", c)
		}
	})
}

// TestRotateSigningKeyRecoversAFailedPendingWrite (r19 C7-R5): a caller
// whose pending key was never stored leaves nothing behind; the next caller,
// on another replica and at once, rotates.
func TestRotateSigningKeyRecoversAFailedPendingWrite(t *testing.T) {
	ctx := context.Background()
	s, kv, a := rotationRig(t)
	oldID := a.KeyID()
	kv.failKey = nextKeyConfig
	if _, err := RotateSigningKey(ctx, kv, s, a, []string{"t1", "t2"}); err == nil {
		t.Fatal("failure not injected")
	}
	if kv.data[nextKeyConfig] != "" || a.KeyID() != oldID {
		t.Fatalf("a failed write left pending %q, key %s", kv.data[nextKeyConfig], a.KeyID())
	}
	if c, _ := s.RotationChain(ctx, "t1", oldID); len(c) != 0 {
		t.Fatalf("records before the pending key was stored: %+v", c)
	}
	kv.failKey = ""
	a2, err := LoadOrCreateTrustAnchor(ctx, kv)
	if err != nil {
		t.Fatal(err)
	}
	res, err := RotateSigningKey(ctx, kv, s, a2, []string{"t1", "t2"})
	if err != nil || res.Records != 2 || a2.KeyID() != res.NewKeyID || kv.data[nextKeyConfig] != "" {
		t.Fatalf("retry: %+v %v", res, err)
	}
}

// TestRotateSigningKeyConcurrentCallersConvergeOnOneKey (r19 C7-R5): many
// replicas asked at once rotate to one key with one record per tenant; a
// caller either takes part in that rotation or is told one is under way.
func TestRotateSigningKeyConcurrentCallersConvergeOnOneKey(t *testing.T) {
	ctx := context.Background()
	s, kv, a := rotationRig(t)
	oldID := a.KeyID()
	type out struct {
		res *RotationResult
		err error
	}
	results := make(chan out, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		replica, err := LoadOrCreateTrustAnchor(ctx, kv)
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := RotateSigningKey(ctx, kv, s, replica, []string{"t1", "t2"})
			results <- out{res, err}
		}()
	}
	wg.Wait()
	close(results)
	keys := map[string]int{}
	records := 0
	for o := range results {
		switch {
		case o.err == nil:
			keys[o.res.NewKeyID]++
			records += o.res.Records
		case errors.Is(o.err, ErrRotationInProgress):
		default:
			t.Errorf("caller failed: %v", o.err)
		}
	}
	if len(keys) != 1 || records != 2 {
		t.Fatalf("callers rotated to %v with %d records; want one key, 2 records", keys, records)
	}
	for _, tenant := range []string{"t1", "t2"} {
		c, _ := s.RotationChain(ctx, tenant, oldID)
		if len(c) != 1 || keys[c[0].NewKeyID] == 0 {
			t.Errorf("%s chain %+v", tenant, c)
		}
	}
	if err := a.Refresh(ctx, kv); err != nil {
		t.Fatal(err)
	}
	if keys[a.KeyID()] == 0 || kv.data[nextKeyConfig] != "" {
		t.Errorf("current key %s, pending %q", a.KeyID(), kv.data[nextKeyConfig])
	}
}

// TestRotateSigningKeyNeverRunsFromAReplacedKey: a caller that read the key
// before another rotation finished, and finds nothing pending after it, does
// not rotate from the old key (its records would be skipped and the new key
// made current with no record naming it).
func TestRotateSigningKeyNeverRunsFromAReplacedKey(t *testing.T) {
	ctx := context.Background()
	s, kv, stale := rotationRig(t)
	oldID := stale.KeyID()
	other, _ := LoadOrCreateTrustAnchor(ctx, kv)
	var done *RotationResult
	kv.onCAS = func(key, value string) {
		if key != nextKeyConfig || value == "" || done != nil {
			return
		}
		kv.mu.Lock()
		kv.onCAS = nil // the other rotation runs whole, here
		kv.mu.Unlock()
		res, err := RotateSigningKey(ctx, kv, s, other, []string{"t1"})
		if err != nil {
			t.Errorf("the other rotation: %v", err)
		}
		done = res
	}
	_, err := RotateSigningKey(ctx, kv, s, stale, []string{"t1"})
	if !errors.Is(err, ErrRotationInProgress) || done == nil {
		t.Fatalf("the stale caller: %v (other %+v)", err, done)
	}
	if c, _ := s.RotationChain(ctx, "t1", oldID); len(c) != 1 || c[0].NewKeyID != done.NewKeyID {
		t.Errorf("chain %+v", c)
	}
	if kv.data[nextKeyConfig] != "" {
		t.Error("the stale caller left its key pending")
	}
	if a2, _ := LoadOrCreateTrustAnchor(ctx, kv); a2.KeyID() != done.NewKeyID {
		t.Errorf("current %s, want the other rotation's %s", a2.KeyID(), done.NewKeyID)
	}
}

// TestRefreshFollowsAnotherReplicasRotation: a replica that did not rotate
// signs with the new key after a refresh, and an unreadable stored key is an
// error, never a new key.
func TestRefreshFollowsAnotherReplicasRotation(t *testing.T) {
	s, kv, a := rotationRig(t)
	ctx := context.Background()
	other, err := LoadOrCreateTrustAnchor(ctx, kv)
	if err != nil {
		t.Fatal(err)
	}
	res, err := RotateSigningKey(ctx, kv, s, a, []string{"t1"})
	if err != nil {
		t.Fatal(err)
	}
	if other.KeyID() != res.OldKeyID {
		t.Fatal("the other replica changed before refreshing")
	}
	if err := other.Refresh(ctx, kv); err != nil || other.KeyID() != res.NewKeyID {
		t.Fatalf("refresh: %v, key %s", err, other.KeyID())
	}
	kv.data[systemConfigKey] = "not a key"
	if err := other.Refresh(ctx, kv); err == nil || other.KeyID() != res.NewKeyID {
		t.Fatalf("an unreadable stored key: %v, key %s", err, other.KeyID())
	}
	if kv.data[systemConfigKey] != "not a key" {
		t.Error("refresh replaced an unreadable stored key")
	}
}

// TestRotateSigningKeyLoserCarriesOnWithTheWinnersKey: two callers find no
// key pending; the other one stores its key first, so this one's
// compare-and-set loses and it finishes the rotation with the other key.
func TestRotateSigningKeyLoserCarriesOnWithTheWinnersKey(t *testing.T) {
	ctx := context.Background()
	s, kv, a := rotationRig(t)
	oldID := a.KeyID()
	_, winner, err := generateECPrivateKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	kv.onCAS = func(key, value string) {
		if key != nextKeyConfig || value == "" {
			return
		}
		kv.mu.Lock()
		kv.onCAS = nil
		kv.data[nextKeyConfig] = string(winner) // the other caller's write lands first
		kv.mu.Unlock()
	}
	res, err := RotateSigningKey(ctx, kv, s, a, []string{"t1"})
	w, _ := parseECPrivateKeyPEM(winner)
	wPKIX, _ := x509.MarshalPKIXPublicKey(&w.PublicKey)
	if err != nil || !res.Resumed || res.NewKeyID != KeyID(wPKIX) || a.KeyID() != res.NewKeyID {
		t.Fatalf("the losing caller: %+v %v; want the winner's key %s", res, err, KeyID(wPKIX))
	}
	if c, _ := s.RotationChain(ctx, "t1", oldID); len(c) != 1 || c[0].NewKeyID != res.NewKeyID {
		t.Errorf("chain %+v", c)
	}
}
