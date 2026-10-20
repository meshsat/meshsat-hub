package directory

import (
	"context"
	"testing"
)

// The review's reproduction of r20 C7-R7. One change: activation now goes
// through CompareAndSetSystemConfig, so the wrapper runs its hook before
// that write as well as before SetSystemConfig; without it the delayed
// caller would never be overtaken and the test would prove nothing.
type reviewLateActivation struct {
	*fakeStore
	before func()
}

func (s *reviewLateActivation) hook(key string) {
	if key == systemConfigKey && s.before != nil {
		f := s.before
		s.before = nil
		f()
	}
}

func (s *reviewLateActivation) SetSystemConfig(ctx context.Context, key, value string) error {
	s.hook(key)
	return s.fakeStore.SetSystemConfig(ctx, key, value)
}

func (s *reviewLateActivation) CompareAndSetSystemConfig(ctx context.Context, key, old, value string) (bool, error) {
	s.hook(key)
	return s.fakeStore.CompareAndSetSystemConfig(ctx, key, old, value)
}

func TestReviewR20StaleRotationCannotRollBackCurrentKey(t *testing.T) {
	ctx := context.Background()
	dir, kv, first := rotationRig(t)
	original := first.KeyID()
	wrapped := &reviewLateActivation{fakeStore: kv}
	latest := ""
	wrapped.before = func() {
		second, err := LoadOrCreateTrustAnchor(ctx, kv)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = RotateSigningKey(ctx, kv, dir, second, []string{"t1"}); err != nil {
			t.Fatal(err)
		}
		next, err := RotateSigningKey(ctx, kv, dir, second, []string{"t1"})
		if err != nil {
			t.Fatal(err)
		}
		latest = next.NewKeyID
	}
	_, err := RotateSigningKey(ctx, wrapped, dir, first, []string{"t1"})
	if err != nil {
		t.Logf("stale caller rejected: %v", err)
	}
	current, err := LoadOrCreateTrustAnchor(ctx, kv)
	if err != nil {
		t.Fatal(err)
	}
	chain, err := dir.RotationChain(ctx, "t1", original)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("chain_length=%d current_matches_latest=%v chain_tip_matches_latest=%v", len(chain), current.KeyID() == latest, chain[len(chain)-1].NewKeyID == latest)
	if current.KeyID() != latest {
		t.Fatal("late activation rolled current key back behind completed rotation chain")
	}
}
