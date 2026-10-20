package directorytest

import (
	"context"
	"sync"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/directory"
)

// Concurrent rotation requests, arranged rather than timed (r21 R21-T1). A
// wave of callers is held at its activation, the compare-and-set that makes
// the new key current, until every caller of the wave has got there or
// returned. A caller reaches activation only after it has chosen its
// generation: the key it read as current and the pending key it stored or
// found. No caller activates before all have chosen, so every caller of a
// wave chooses the same generation by construction, whatever the scheduler
// does. Callers that start after a wave has cleaned up start the next
// generation, which is legitimate and tested apart. [MESHSAT-1584]

// generationBarrier releases its callers once each has arrived or returned.
type generationBarrier struct {
	mu      sync.Mutex
	pending int
	release chan struct{}
}

func newGenerationBarrier(n int) *generationBarrier {
	return &generationBarrier{pending: n, release: make(chan struct{})}
}

// done counts a caller out: arrived, or returned without arriving.
func (b *generationBarrier) done() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending--
	if b.pending == 0 {
		close(b.release)
	}
}

// barrierKV is one caller's view of the key store: its first activation
// waits for the rest of the wave.
type barrierKV struct {
	KeyStore
	b       *generationBarrier
	arrived bool
}

func (k *barrierKV) CompareAndSetSystemConfig(ctx context.Context, key, old, value string) (bool, error) {
	if key == signingKey && value != "" && !k.arrived {
		k.arrived = true
		k.b.done()
		<-k.b.release
	}
	return k.KeyStore.CompareAndSetSystemConfig(ctx, key, old, value)
}

type waveResult struct {
	res *directory.RotationResult
	err error
}

// wave runs n callers, each a replica started now, as one arranged wave.
func wave(t *testing.T, s *directory.SQLStore, kv KeyStore, tenants []string, n int) []waveResult {
	t.Helper()
	ctx := context.Background()
	b := newGenerationBarrier(n)
	out := make([]waveResult, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		replica, err := directory.LoadOrCreateTrustAnchor(ctx, kv)
		if err != nil {
			t.Fatal(err)
		}
		bkv := &barrierKV{KeyStore: kv, b: b}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := directory.RotateSigningKey(ctx, bkv, s, replica, tenants)
			if !bkv.arrived {
				b.done()
			}
			out[i] = waveResult{res, err}
		}(i)
	}
	wg.Wait()
	return out
}

// oneKey checks that every caller of a wave succeeded with one key, and
// that the wave added one record per tenant; it returns the key.
func oneKey(t *testing.T, results []waveResult, tenants int) string {
	t.Helper()
	keys := map[string]int{}
	records := 0
	for _, r := range results {
		if r.err != nil {
			t.Errorf("a caller of the wave: %v", r.err)
			continue
		}
		keys[r.res.NewKeyID]++
		records += r.res.Records
	}
	if len(keys) != 1 || records != tenants {
		t.Fatalf("the wave rotated to %v with %d records; want one key, %d records", keys, records, tenants)
	}
	for k := range keys {
		return k
	}
	return ""
}

// RotationOneGeneration: twelve replicas asked at once, arranged into one
// generation, all succeed with one key; each tenant gets one record and one
// version, and the key is current and the chain's tip.
func RotationOneGeneration(t *testing.T, rig Rig) {
	s, kv := rig(t)
	ctx := context.Background()
	tenants := []string{"gen-one-a", "gen-one-b"}
	for _, tn := range tenants {
		mustPut(t, s, &directory.Contact{TenantID: tn, DisplayName: "A"})
	}
	first, err := directory.LoadOrCreateTrustAnchor(ctx, kv)
	if err != nil {
		t.Fatal(err)
	}
	oldID := first.KeyID()
	key := oneKey(t, wave(t, s, kv, tenants, 12), len(tenants))
	for _, tn := range tenants {
		chain, err := s.RotationChain(ctx, tn, oldID)
		if err != nil || len(chain) != 1 || chain[0].NewKeyID != key {
			t.Fatalf("%s chain %+v %v", tn, chain, err)
		}
		if v := version(t, s, tn); v != 2 {
			t.Errorf("%s version %d, want 2 (its contact, one record)", tn, v)
		}
	}
	if err := first.Refresh(ctx, kv); err != nil || first.KeyID() != key {
		t.Errorf("current key %s (%v), the records name %s", first.KeyID(), err, key)
	}
	if v, _ := kv.GetSystemConfig(ctx, pendingKey); v != "" {
		t.Error("a pending key was left")
	}
}

// RotationNextGeneration: callers that start after a wave cleaned up start
// the next generation, as one wave of their own; a single caller after
// that starts one more. Each generation is one key, one record per tenant
// and one version, in order, and the last is current.
func RotationNextGeneration(t *testing.T, rig Rig) {
	s, kv := rig(t)
	ctx := context.Background()
	tenants := []string{"gen-next-a", "gen-next-b"}
	for _, tn := range tenants {
		mustPut(t, s, &directory.Contact{TenantID: tn, DisplayName: "A"})
	}
	first, err := directory.LoadOrCreateTrustAnchor(ctx, kv)
	if err != nil {
		t.Fatal(err)
	}
	oldID := first.KeyID()
	k1 := oneKey(t, wave(t, s, kv, tenants, 6), len(tenants))
	k2 := oneKey(t, wave(t, s, kv, tenants, 6), len(tenants))
	k3 := oneKey(t, wave(t, s, kv, tenants, 1), len(tenants))
	if k1 == k2 || k2 == k3 || k1 == k3 {
		t.Fatalf("generations %s, %s, %s are not three keys", k1, k2, k3)
	}
	for _, tn := range tenants {
		chain, err := s.RotationChain(ctx, tn, oldID)
		if err != nil || len(chain) != 3 || chain[0].NewKeyID != k1 || chain[1].NewKeyID != k2 || chain[2].NewKeyID != k3 {
			t.Fatalf("%s chain %+v %v", tn, chain, err)
		}
		if v := version(t, s, tn); v != 4 {
			t.Errorf("%s version %d, want 4 (its contact, three records)", tn, v)
		}
	}
	if err := first.Refresh(ctx, kv); err != nil || first.KeyID() != k3 {
		t.Errorf("current key %s (%v), want %s", first.KeyID(), err, k3)
	}
}
