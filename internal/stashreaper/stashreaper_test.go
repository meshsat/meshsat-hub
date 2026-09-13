package stashreaper

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Guarded, because Run drives it from its own goroutine while the test reads
// the counts. The race detector caught this version of the test, not the code.
type fakeStore struct {
	mu      sync.Mutex
	rows    map[string]time.Time // key -> last written
	deleted []string
	listErr error
	delErr  error
}

func (f *fakeStore) deletedKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

func (f *fakeStore) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.rows[key]
	return ok
}

func (f *fakeStore) rowCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

func (f *fakeStore) set(key string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[key] = at
}

func (f *fakeStore) ListSystemConfigOlderThan(_ context.Context, prefix string, cutoff time.Time) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []string
	for k, at := range f.rows {
		if strings.HasPrefix(k, prefix) && at.Before(cutoff) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (f *fakeStore) DeleteSystemConfig(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.delErr != nil {
		return f.delErr
	}
	delete(f.rows, key)
	f.deleted = append(f.deleted, key)
	return nil
}

func at(d time.Duration) time.Time { return time.Now().Add(d) }

func prefixes() []Prefix {
	return []Prefix{{Prefix: "provision_stash:", TTL: 30 * time.Minute, Why: "bridge bundle"}}
}

// The case this package exists for: a bundle nobody ever claimed. The TTL is
// enforced only when somebody arrives to claim, and for an abandoned stash
// nobody ever does -- so it lived six months and went into every backup.
func TestAnUnclaimedBundleIsRemovedOnceItCannotBeClaimed(t *testing.T) {
	f := &fakeStore{rows: map[string]time.Time{
		"provision_stash:abandoned": at(-6 * 30 * 24 * time.Hour),
		"provision_stash:fresh":     at(-1 * time.Minute),
	}}
	New(f, prefixes(), time.Hour, nil).once(context.Background())

	if len(f.deletedKeys()) != 1 || f.deletedKeys()[0] != "provision_stash:abandoned" {
		t.Fatalf("deleted %v, want only the abandoned one", f.deletedKeys())
	}
	if !f.has("provision_stash:fresh") {
		t.Error("a stash that is still claimable was removed")
	}
}

// A row that expired moments ago belongs to somebody whose claim may be in
// flight. Taking it turns a working provision into an unexplained 404.
func TestARowThatJustExpiredIsLeftAlone(t *testing.T) {
	f := &fakeStore{rows: map[string]time.Time{
		// Past the 30 minute TTL, but inside the grace.
		"provision_stash:justnow": at(-31 * time.Minute),
	}}
	New(f, prefixes(), time.Hour, nil).once(context.Background())
	if len(f.deletedKeys()) != 0 {
		t.Errorf("deleted %v; a claim for it could still be in flight", f.deletedKeys())
	}

	// And once the grace has passed too, it goes.
	f.set("provision_stash:justnow", at(-30*time.Minute-graceAfterTTL-time.Minute))
	New(f, prefixes(), time.Hour, nil).once(context.Background())
	if len(f.deletedKeys()) != 1 {
		t.Errorf("deleted %v, want it removed once the grace has passed too", f.deletedKeys())
	}
}

func TestItOnlyTouchesThePrefixesItWasGiven(t *testing.T) {
	f := &fakeStore{rows: map[string]time.Time{
		"provision_stash:old":       at(-100 * time.Hour),
		"bridge_ca_key":             at(-100 * time.Hour),
		"credential_master_key_enc": at(-100 * time.Hour),
		"tak_enroll:old":            at(-100 * time.Hour),
	}}
	New(f, prefixes(), time.Hour, nil).once(context.Background())
	if len(f.deletedKeys()) != 1 || f.deletedKeys()[0] != "provision_stash:old" {
		t.Fatalf("deleted %v -- this sweeper must never reach the Hub's own keys", f.deletedKeys())
	}
	for _, must := range []string{"bridge_ca_key", "credential_master_key_enc", "tak_enroll:old"} {
		if !f.has(must) {
			t.Errorf("%s was removed", must)
		}
	}
}

func TestOneFailedDeleteDoesNotStallTheRest(t *testing.T) {
	f := &fakeStore{rows: map[string]time.Time{
		"provision_stash:a": at(-100 * time.Hour),
		"provision_stash:b": at(-100 * time.Hour),
	}}
	// Fail every delete; the job must not panic or give up mid-batch.
	f.delErr = errors.New("boom")
	New(f, prefixes(), time.Hour, nil).once(context.Background())
	if f.rowCount() != 2 {
		t.Errorf("rows changed despite every delete failing: %v", f.rows)
	}

	f.delErr = nil
	New(f, prefixes(), time.Hour, nil).once(context.Background())
	if len(f.deletedKeys()) != 2 {
		t.Errorf("deleted %v, want both once deletes work again", f.deletedKeys())
	}
}

func TestAFailedListIsRetriedRatherThanFatal(t *testing.T) {
	f := &fakeStore{rows: map[string]time.Time{"provision_stash:a": at(-100 * time.Hour)}}
	f.listErr = errors.New("db down")
	New(f, prefixes(), time.Hour, nil).once(context.Background())
	if len(f.deletedKeys()) != 0 {
		t.Error("deleted something despite the listing failing")
	}
	f.listErr = nil
	New(f, prefixes(), time.Hour, nil).once(context.Background())
	if len(f.deletedKeys()) != 1 {
		t.Errorf("deleted %v, want the sweep to resume on the next run", f.deletedKeys())
	}
}

func TestRunSweepsImmediatelyAndStopsWithTheContext(t *testing.T) {
	f := &fakeStore{rows: map[string]time.Time{"provision_stash:a": at(-100 * time.Hour)}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); New(f, prefixes(), time.Hour, nil).Run(ctx) }()

	deadline := time.After(2 * time.Second)
	for len(f.deletedKeys()) == 0 {
		select {
		case <-deadline:
			t.Fatal("Run did not sweep at startup; a row that expired while nobody " +
				"was leader would wait a full interval")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("Run did not return after cancel")
	}
}
