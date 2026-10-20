package directory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
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

func claimAll(context.Context, string) (bool, error) { return true, nil }

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
	res, err := RotateSigningKey(ctx, kv, claimAll, s, a, []string{"t1", "t2"})
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
	res2, err := RotateSigningKey(ctx, kv, claimAll, s, a, []string{"t1", "t2"})
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
		res, err := RotateSigningKey(ctx, kv, claimAll, s, a, []string{"t1", "t2"})
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
		res, err = RotateSigningKey(ctx, kv, func(context.Context, string) (bool, error) { return false, nil }, s, a2, []string{"t1", "t2"})
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
		kv.failKey = nextKeyConfig // clearing the pending key fails, after step 3
		_, err := RotateSigningKey(ctx, kv, claimAll, s, a, []string{"t1"})
		if err == nil {
			t.Fatal("no error")
		}
		// The first Set of nextKeyConfig (storing the pending key) also
		// failed here, so nothing happened: retry with a working store.
		kv.failKey = ""
		res, err := RotateSigningKey(ctx, kv, claimAll, s, a, []string{"t1"})
		if err != nil || res.Records != 1 {
			t.Fatalf("%+v %v", res, err)
		}
		// Now simulate the crash between step 3 and the clean-up.
		kv.data[nextKeyConfig] = kv.data[systemConfigKey]
		res2, err := RotateSigningKey(ctx, kv, claimAll, s, a, []string{"t1"})
		if err != nil || !res2.Resumed || res2.Records != 0 || res2.NewKeyID != res.NewKeyID || kv.data[nextKeyConfig] != "" {
			t.Fatalf("clean-up resume: %+v %v", res2, err)
		}
		if c, _ := s.RotationChain(ctx, "t1", res.NewKeyID); len(c) != 0 {
			t.Errorf("the clean-up wrote a record from the new key to itself: %+v", c)
		}
	})
	t.Run("someone else is rotating", func(t *testing.T) {
		s, kv, a := rotationRig(t)
		oldID := a.KeyID()
		_, err := RotateSigningKey(ctx, kv, func(context.Context, string) (bool, error) { return false, nil }, s, a, []string{"t1"})
		if !errors.Is(err, ErrRotationInProgress) || a.KeyID() != oldID || kv.data[nextKeyConfig] != "" {
			t.Fatalf("lost claim: %v", err)
		}
		if c, _ := s.RotationChain(ctx, "t1", oldID); len(c) != 0 {
			t.Errorf("lost claim wrote %d records", len(c))
		}
	})
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
	res, err := RotateSigningKey(ctx, kv, claimAll, s, a, []string{"t1"})
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
