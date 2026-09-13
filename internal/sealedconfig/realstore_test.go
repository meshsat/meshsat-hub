package sealedconfig

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	hubcrypto "github.com/meshsat/meshsat-hub/internal/crypto"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

// Against a REAL driver, not a map.
//
// The map-backed tests prove the logic; this proves the thing that actually
// ships -- that a PEM with newlines, a hex key and an empty row all survive the
// round trip through a real system_config table, and that Preflight migrates
// them without touching the plaintext rows. The blast radius if this is wrong
// is every tenant's carrier credentials, so it is worth a real database.
func TestPreflightMigratesRealRowsAndLeavesThePlaintextAlone(t *testing.T) {
	ctx := context.Background()
	s, err := sqlite.New(filepath.Join(t.TempDir(), "hub.db"), 0)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// The real shapes, taken from what production actually holds.
	seed := map[string]string{
		"bridge_ca_key": "-----BEGIN EC PRIVATE KEY-----\n" +
			"MHcCAQEEIB9vC2lDVQ9v0000000000000000000000000000oAoGCCqGSM49\n" +
			"-----END EC PRIVATE KEY-----\n",
		"directory_signing_key": "-----BEGIN EC PRIVATE KEY-----\nMHcCAQEE\n-----END EC PRIVATE KEY-----\n",
		"credential_master_key": strings.Repeat("ab", 32),
		"reticulum_signing_key": strings.Repeat("cd", 64),
		// reticulum_encryption_key deliberately absent: a key that is not
		// stored yet must not be invented by the migration.
	}
	for k, v := range seed {
		if err := s.SetSystemConfig(ctx, k, v); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}
	// A non-sensitive row must pass through untouched.
	if err := s.SetSystemConfig(ctx, "mqtt_public_url", "wss://mqtt-hub.meshsat.net/mqtt"); err != nil {
		t.Fatalf("seed url: %v", err)
	}

	wrap, err := hubcrypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	k := New(s, wrap)

	migrated, err := Preflight(ctx, k, nil)
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if migrated != len(seed) {
		t.Errorf("migrated %d, want %d (one of the five is deliberately absent)", migrated, len(seed))
	}

	for key, want := range seed {
		// The sealed row exists, is not the plaintext, and carries no PEM marker.
		sealed, err := s.GetSystemConfig(ctx, key+Suffix)
		if err != nil || sealed == "" {
			t.Errorf("%s%s: not written (%v)", key, Suffix, err)
			continue
		}
		if sealed == want || strings.Contains(sealed, "PRIVATE KEY") {
			t.Errorf("%s%s: stored in the clear", key, Suffix)
		}
		// The plaintext row is untouched -- blanking is a separate step.
		if plain, _ := s.GetSystemConfig(ctx, key); plain != want {
			t.Errorf("%s: plaintext row changed to %q", key, plain)
		}
		// And the value reads back byte for byte, newlines and all.
		got, err := k.Get(ctx, key)
		if err != nil || got != want {
			t.Errorf("%s: round trip gave %q, %v", key, got, err)
		}
	}

	if _, err := k.Get(ctx, "reticulum_encryption_key"); err != ErrNotFound {
		t.Errorf("an absent key gave %v, want ErrNotFound -- anything else and the "+
			"caller will not generate the key it is supposed to generate", err)
	}

	// A second Preflight is a no-op.
	again, err := Preflight(ctx, k, nil)
	if err != nil || again != 0 {
		t.Errorf("second Preflight: migrated=%d err=%v, want a no-op on every restart", again, err)
	}

	// The wrong wrap key must refuse, not report absence.
	other, _ := hubcrypto.GenerateKey()
	if _, err := Preflight(ctx, New(s, other), nil); err == nil {
		t.Fatal("Preflight accepted a wrong wrap key -- the process would start and " +
			"regenerate every one of these")
	}

	// And once the plaintext rows are blanked, the sealed ones still open.
	for key := range seed {
		if err := s.SetSystemConfig(ctx, key, ""); err != nil {
			t.Fatalf("blank %s: %v", key, err)
		}
	}
	for key, want := range seed {
		got, err := k.Get(ctx, key)
		if err != nil || got != want {
			t.Errorf("%s after blanking: %q, %v", key, got, err)
		}
	}
}
