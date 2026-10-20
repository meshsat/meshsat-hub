package main

import (
	"context"
	"path/filepath"
	"testing"

	hubcrypto "github.com/meshsat/meshsat-hub/internal/crypto"
	"github.com/meshsat/meshsat-hub/internal/sealedconfig"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

// The decorator's compare-and-set seals a sensitive key, and passes any other
// key through (MESHSAT-1584 r19 C7-R5: the rotation's pending key is one).
func TestSealedStoreCompareAndSet(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(filepath.Join(t.TempDir(), "hub.db"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	wrap, err := hubcrypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	s := newSealedStore(db, sealedconfig.New(db, wrap))
	if won, err := s.CompareAndSetSystemConfig(ctx, "directory_signing_key_next", "", "pem"); err != nil || !won {
		t.Fatalf("sensitive: won=%v %v", won, err)
	}
	if _, err := db.GetSystemConfig(ctx, "directory_signing_key_next"); err == nil {
		t.Error("the pending key went in as plaintext")
	}
	if v, err := s.GetSystemConfig(ctx, "directory_signing_key_next"); err != nil || v != "pem" {
		t.Errorf("read back %q %v", v, err)
	}
	if won, _ := s.CompareAndSetSystemConfig(ctx, "directory_signing_key_next", "", "other"); won {
		t.Error("a second key replaced the pending one")
	}
	if won, err := s.CompareAndSetSystemConfig(ctx, "plain_key", "", "x"); err != nil || !won {
		t.Fatalf("plain: won=%v %v", won, err)
	}
	if v, _ := db.GetSystemConfig(ctx, "plain_key"); v != "x" {
		t.Errorf("plain key %q", v)
	}
}
