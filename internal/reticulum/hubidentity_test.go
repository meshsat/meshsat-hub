package reticulum

import (
	"context"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// mockIdentityStore implements IdentityStore with an in-memory map.
type mockIdentityStore struct {
	data map[string]string
}

func newMockStore() *mockIdentityStore {
	return &mockIdentityStore{data: make(map[string]string)}
}

func (m *mockIdentityStore) GetSystemConfig(_ context.Context, key string) (string, error) {
	v, ok := m.data[key]
	if !ok {
		return "", sql.ErrNoRows
	}
	return v, nil
}

func (m *mockIdentityStore) SetSystemConfig(_ context.Context, key, value string) error {
	m.data[key] = value
	return nil
}

func TestNewHubIdentity_GenerateAndPersistToDB(t *testing.T) {
	store := newMockStore()

	hi, err := NewHubIdentity(store, "", "meshsat.hub")
	if err != nil {
		t.Fatal(err)
	}
	if !hi.IsLoaded() {
		t.Fatal("identity should be loaded after generation")
	}

	// Verify keys persisted to store.
	encHex, err := store.GetSystemConfig(context.Background(), configKeyEncryptionKey)
	if err != nil {
		t.Fatal("encryption key not persisted to store:", err)
	}
	sigHex, err := store.GetSystemConfig(context.Background(), configKeySigningKey)
	if err != nil {
		t.Fatal("signing key not persisted to store:", err)
	}
	if len(encHex) != EncryptionPubLen*2 {
		t.Errorf("encryption key hex length: got %d, want %d", len(encHex), EncryptionPubLen*2)
	}
	if len(sigHex) != 64*2 {
		t.Errorf("signing key hex length: got %d, want %d", len(sigHex), 64*2)
	}

	// Verify dest hash is non-zero.
	destHash := hi.DestHash()
	if destHash == [TruncatedHashLen]byte{} {
		t.Error("dest hash should not be zero")
	}
	if hi.DestHashHex() == "" {
		t.Error("dest hash hex should not be empty")
	}
}

func TestNewHubIdentity_LoadFromDB(t *testing.T) {
	store := newMockStore()

	// Generate first.
	hi1, err := NewHubIdentity(store, "", "meshsat.hub")
	if err != nil {
		t.Fatal(err)
	}

	// Load from same store — should get identical identity.
	hi2, err := NewHubIdentity(store, "", "meshsat.hub")
	if err != nil {
		t.Fatal(err)
	}

	if hi1.DestHashHex() != hi2.DestHashHex() {
		t.Errorf("dest hash mismatch: %s != %s", hi1.DestHashHex(), hi2.DestHashHex())
	}
	if hi1.PublicKeyHex() != hi2.PublicKeyHex() {
		t.Error("public key should match after reload")
	}
}

func TestNewHubIdentity_MigrateFileToDb(t *testing.T) {
	store := newMockStore()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "identity.json")

	// Generate with file-only (nil store).
	hi1, err := NewHubIdentity(nil, filePath, "meshsat.hub")
	if err != nil {
		t.Fatal(err)
	}

	// Verify file exists.
	if _, err := os.Stat(filePath); err != nil {
		t.Fatal("identity file should exist:", err)
	}

	// Now load with both store and file — should migrate to DB.
	hi2, err := NewHubIdentity(store, filePath, "meshsat.hub")
	if err != nil {
		t.Fatal(err)
	}

	if hi1.DestHashHex() != hi2.DestHashHex() {
		t.Errorf("dest hash mismatch after migration: %s != %s", hi1.DestHashHex(), hi2.DestHashHex())
	}

	// Verify keys now in store.
	_, err = store.GetSystemConfig(context.Background(), configKeyEncryptionKey)
	if err != nil {
		t.Fatal("encryption key should be in store after migration:", err)
	}
}

func TestNewHubIdentity_PublicKeyHex(t *testing.T) {
	store := newMockStore()
	hi, err := NewHubIdentity(store, "", "meshsat.hub")
	if err != nil {
		t.Fatal(err)
	}

	pubHex := hi.PublicKeyHex()
	if len(pubHex) != IdentityKeySize*2 {
		t.Errorf("public key hex length: got %d, want %d", len(pubHex), IdentityKeySize*2)
	}

	// Verify it's valid hex.
	pubBytes, err := hex.DecodeString(pubHex)
	if err != nil {
		t.Fatal("public key hex should be valid hex:", err)
	}
	if len(pubBytes) != IdentityKeySize {
		t.Errorf("decoded public key length: got %d, want %d", len(pubBytes), IdentityKeySize)
	}
}

func TestNewHubIdentity_DifferentAppNamesDifferentDestHash(t *testing.T) {
	store1 := newMockStore()
	store2 := newMockStore()

	hi1, err := NewHubIdentity(store1, "", "meshsat.hub")
	if err != nil {
		t.Fatal(err)
	}

	hi2, err := NewHubIdentity(store2, "", "meshsat.bridge")
	if err != nil {
		t.Fatal(err)
	}

	// Different identities AND different app names → different dest hashes.
	if hi1.DestHashHex() == hi2.DestHashHex() {
		t.Error("different identities + app names should produce different dest hashes")
	}
}

func TestNewHubIdentity_FileOnlyMode(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "identity.json")

	// Generate with file-only (nil store).
	hi1, err := NewHubIdentity(nil, filePath, "meshsat.hub")
	if err != nil {
		t.Fatal(err)
	}
	if !hi1.IsLoaded() {
		t.Fatal("identity should be loaded")
	}

	// Reload from file.
	hi2, err := NewHubIdentity(nil, filePath, "meshsat.hub")
	if err != nil {
		t.Fatal(err)
	}

	if hi1.DestHashHex() != hi2.DestHashHex() {
		t.Errorf("file-only roundtrip: dest hash mismatch: %s != %s", hi1.DestHashHex(), hi2.DestHashHex())
	}
}

func TestNewHubIdentity_AppName(t *testing.T) {
	store := newMockStore()
	hi, err := NewHubIdentity(store, "", "meshsat.hub")
	if err != nil {
		t.Fatal(err)
	}
	if hi.AppName() != "meshsat.hub" {
		t.Errorf("app name: got %s, want meshsat.hub", hi.AppName())
	}
}
