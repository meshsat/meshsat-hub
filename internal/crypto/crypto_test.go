package crypto

import (
	"bytes"
	"testing"
)

func TestGenerateKey(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != KeySize {
		t.Errorf("expected %d bytes, got %d", KeySize, len(key))
	}

	// Two keys should be different.
	key2, _ := GenerateKey()
	if bytes.Equal(key, key2) {
		t.Error("two generated keys should not be identical")
	}
}

func TestKeyHash(t *testing.T) {
	key, _ := GenerateKey()
	h := KeyHash(key)
	if len(h) != 64 { // SHA-256 = 32 bytes = 64 hex chars
		t.Errorf("expected 64 hex chars, got %d", len(h))
	}

	// Same key should produce same hash.
	h2 := KeyHash(key)
	if h != h2 {
		t.Error("same key should produce same hash")
	}
}

func TestEncryptDecryptRoundtrip(t *testing.T) {
	key, _ := GenerateKey()
	plaintext := []byte("All clear, moving to checkpoint B")

	ciphertext, err := Encrypt(key, plaintext)
	if err != nil {
		t.Fatal(err)
	}

	// Ciphertext should be larger than plaintext by overhead.
	if len(ciphertext) != len(plaintext)+Overhead {
		t.Errorf("ciphertext size: expected %d, got %d", len(plaintext)+Overhead, len(ciphertext))
	}

	// Decrypt.
	decrypted, err := Decrypt(key, ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Errorf("decrypted doesn't match: got %q", decrypted)
	}
}

func TestEncryptProducesUniqueCiphertexts(t *testing.T) {
	key, _ := GenerateKey()
	plaintext := []byte("test message")

	ct1, _ := Encrypt(key, plaintext)
	ct2, _ := Encrypt(key, plaintext)

	// Different nonces → different ciphertexts.
	if bytes.Equal(ct1, ct2) {
		t.Error("two encryptions of same plaintext should produce different ciphertexts")
	}
}

func TestDecryptWrongKey(t *testing.T) {
	key1, _ := GenerateKey()
	key2, _ := GenerateKey()
	plaintext := []byte("secret")

	ct, _ := Encrypt(key1, plaintext)

	_, err := Decrypt(key2, ct)
	if err == nil {
		t.Error("expected error decrypting with wrong key")
	}
}

func TestDecryptTooShort(t *testing.T) {
	key, _ := GenerateKey()
	_, err := Decrypt(key, make([]byte, Overhead-1))
	if err == nil {
		t.Error("expected error for too-short ciphertext")
	}
}

func TestDecryptTampered(t *testing.T) {
	key, _ := GenerateKey()
	ct, _ := Encrypt(key, []byte("original"))

	// Tamper with ciphertext.
	ct[len(ct)-1] ^= 0xFF

	_, err := Decrypt(key, ct)
	if err == nil {
		t.Error("expected error for tampered ciphertext")
	}
}

func TestEncryptEmpty(t *testing.T) {
	key, _ := GenerateKey()
	ct, err := Encrypt(key, []byte{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ct) != Overhead {
		t.Errorf("empty plaintext ciphertext: expected %d bytes, got %d", Overhead, len(ct))
	}
	pt, err := Decrypt(key, ct)
	if err != nil {
		t.Fatal(err)
	}
	if len(pt) != 0 {
		t.Errorf("expected empty plaintext, got %d bytes", len(pt))
	}
}

// --- KeyStore tests ---

func TestKeyStore_GenerateAndRetrieve(t *testing.T) {
	ks := NewKeyStore()

	entry, key, err := ks.GenerateAndStore("dev1", "decrypt")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Version != 1 {
		t.Errorf("expected version 1, got %d", entry.Version)
	}
	if entry.Mode != "decrypt" {
		t.Errorf("expected mode decrypt, got %s", entry.Mode)
	}
	if entry.KeyHex == "" {
		t.Error("expected key material in decrypt mode")
	}
	if len(key) != KeySize {
		t.Errorf("expected %d byte key, got %d", KeySize, len(key))
	}

	// Retrieve latest.
	latest, err := ks.GetLatest("dev1")
	if err != nil {
		t.Fatal(err)
	}
	if latest.Version != 1 {
		t.Errorf("expected version 1, got %d", latest.Version)
	}
}

func TestKeyStore_KeyRotation(t *testing.T) {
	ks := NewKeyStore()

	_, _, _ = ks.GenerateAndStore("dev1", "decrypt")
	_, _, _ = ks.GenerateAndStore("dev1", "decrypt")
	entry, _, _ := ks.GenerateAndStore("dev1", "decrypt")

	if entry.Version != 3 {
		t.Errorf("expected version 3 after 3 rotations, got %d", entry.Version)
	}

	latest, _ := ks.GetLatest("dev1")
	if latest.Version != 3 {
		t.Errorf("latest should be version 3, got %d", latest.Version)
	}

	// Retrieve specific version.
	v1, _ := ks.GetVersion("dev1", 1)
	if v1.Version != 1 {
		t.Errorf("expected version 1, got %d", v1.Version)
	}
}

func TestKeyStore_PassthroughMode(t *testing.T) {
	ks := NewKeyStore()

	entry, _, _ := ks.GenerateAndStore("dev1", "passthrough")
	if entry.KeyHex != "" {
		t.Error("passthrough mode should not store key material")
	}

	_, err := ks.DecryptMessage("dev1", make([]byte, Overhead+10))
	if err == nil {
		t.Error("expected error decrypting in passthrough mode")
	}
}

func TestKeyStore_EncryptDecrypt(t *testing.T) {
	ks := NewKeyStore()
	_, _, _ = ks.GenerateAndStore("dev1", "decrypt")

	plaintext := []byte("SOS from checkpoint alpha")
	ct, err := ks.EncryptMessage("dev1", plaintext)
	if err != nil {
		t.Fatal(err)
	}

	pt, err := ks.DecryptMessage("dev1", ct)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pt, plaintext) {
		t.Error("roundtrip mismatch")
	}
}

func TestKeyStore_NoKey(t *testing.T) {
	ks := NewKeyStore()
	_, err := ks.GetLatest("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent device")
	}
}

func TestKeyStore_ListVersionsRedactsKey(t *testing.T) {
	ks := NewKeyStore()
	_, _, _ = ks.GenerateAndStore("dev1", "decrypt")
	_, _, _ = ks.GenerateAndStore("dev1", "decrypt")

	versions := ks.ListVersions("dev1")
	if len(versions) != 2 {
		t.Fatalf("expected 2 versions, got %d", len(versions))
	}
	for _, v := range versions {
		if v.KeyHex != "" {
			t.Errorf("version %d should have key redacted", v.Version)
		}
	}
}

func TestKeyStore_StoreExternalKey(t *testing.T) {
	ks := NewKeyStore()
	key, _ := GenerateKey()

	entry, err := ks.StoreKey("dev1", key, "decrypt")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Version != 1 {
		t.Errorf("expected version 1, got %d", entry.Version)
	}

	// Encrypt with the stored key, decrypt manually.
	ct, _ := ks.EncryptMessage("dev1", []byte("test"))
	pt, _ := Decrypt(key, ct)
	if string(pt) != "test" {
		t.Errorf("expected 'test', got %q", pt)
	}
}

func TestKeyStore_InvalidKeySize(t *testing.T) {
	ks := NewKeyStore()
	_, err := ks.StoreKey("dev1", []byte("short"), "decrypt")
	if err == nil {
		t.Error("expected error for invalid key size")
	}
}
