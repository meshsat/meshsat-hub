package crypto

import (
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// KeyEntry represents a versioned encryption key for a device.
type KeyEntry struct {
	DeviceIMEI string    `json:"device_imei"`
	Version    int       `json:"version"`
	KeyHex     string    `json:"key_hex,omitempty"` // hex-encoded key (omitted in pass-through mode)
	KeyHashHex string    `json:"key_hash"`          // SHA-256 hash for identification
	Mode       string    `json:"mode"`              // "decrypt" (server can read) or "passthrough" (opaque)
	CreatedAt  time.Time `json:"created_at"`
}

// KeyStore manages per-device encryption keys with versioning.
// Thread-safe. In-memory for now; persistence via store interface later.
type KeyStore struct {
	mu   sync.RWMutex
	keys map[string][]KeyEntry // device IMEI → ordered key versions (latest last)
}

// NewKeyStore creates a new key store.
func NewKeyStore() *KeyStore {
	return &KeyStore{
		keys: make(map[string][]KeyEntry),
	}
}

// GenerateAndStore creates a new key for a device and stores it.
// Returns the key entry (with key material if mode is "decrypt").
func (ks *KeyStore) GenerateAndStore(deviceIMEI, mode string) (*KeyEntry, []byte, error) {
	key, err := GenerateKey()
	if err != nil {
		return nil, nil, err
	}

	ks.mu.Lock()
	defer ks.mu.Unlock()

	versions := ks.keys[deviceIMEI]
	version := len(versions) + 1

	entry := KeyEntry{
		DeviceIMEI: deviceIMEI,
		Version:    version,
		KeyHashHex: KeyHash(key),
		Mode:       mode,
		CreatedAt:  time.Now().UTC(),
	}

	if mode == "decrypt" {
		entry.KeyHex = hex.EncodeToString(key)
	}

	ks.keys[deviceIMEI] = append(versions, entry)
	return &entry, key, nil
}

// StoreKey stores an externally-provided key for a device.
func (ks *KeyStore) StoreKey(deviceIMEI string, key []byte, mode string) (*KeyEntry, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("crypto: key must be %d bytes, got %d", KeySize, len(key))
	}

	ks.mu.Lock()
	defer ks.mu.Unlock()

	versions := ks.keys[deviceIMEI]
	version := len(versions) + 1

	entry := KeyEntry{
		DeviceIMEI: deviceIMEI,
		Version:    version,
		KeyHashHex: KeyHash(key),
		Mode:       mode,
		CreatedAt:  time.Now().UTC(),
	}

	if mode == "decrypt" {
		entry.KeyHex = hex.EncodeToString(key)
	}

	ks.keys[deviceIMEI] = append(versions, entry)
	return &entry, nil
}

// GetLatest returns the latest key entry for a device.
func (ks *KeyStore) GetLatest(deviceIMEI string) (*KeyEntry, error) {
	ks.mu.RLock()
	defer ks.mu.RUnlock()

	versions := ks.keys[deviceIMEI]
	if len(versions) == 0 {
		return nil, fmt.Errorf("crypto: no key for device %s", deviceIMEI)
	}
	entry := versions[len(versions)-1]
	return &entry, nil
}

// GetVersion returns a specific key version for a device.
func (ks *KeyStore) GetVersion(deviceIMEI string, version int) (*KeyEntry, error) {
	ks.mu.RLock()
	defer ks.mu.RUnlock()

	versions := ks.keys[deviceIMEI]
	if version < 1 || version > len(versions) {
		return nil, fmt.Errorf("crypto: version %d not found for device %s", version, deviceIMEI)
	}
	entry := versions[version-1]
	return &entry, nil
}

// ListVersions returns all key versions for a device (without key material).
func (ks *KeyStore) ListVersions(deviceIMEI string) []KeyEntry {
	ks.mu.RLock()
	defer ks.mu.RUnlock()

	versions := ks.keys[deviceIMEI]
	result := make([]KeyEntry, len(versions))
	for i, v := range versions {
		result[i] = v
		result[i].KeyHex = "" // never expose key material in listings
	}
	return result
}

// DecryptMessage decrypts a message using the device's latest key.
// Returns error if no key exists or if mode is "passthrough".
func (ks *KeyStore) DecryptMessage(deviceIMEI string, ciphertext []byte) ([]byte, error) {
	entry, err := ks.GetLatest(deviceIMEI)
	if err != nil {
		return nil, err
	}
	if entry.Mode != "decrypt" {
		return nil, fmt.Errorf("crypto: device %s is in passthrough mode", deviceIMEI)
	}
	if entry.KeyHex == "" {
		return nil, fmt.Errorf("crypto: no key material stored for device %s", deviceIMEI)
	}

	key, err := hex.DecodeString(entry.KeyHex)
	if err != nil {
		return nil, fmt.Errorf("crypto: decode key: %w", err)
	}
	return Decrypt(key, ciphertext)
}

// EncryptMessage encrypts a message using the device's latest key.
func (ks *KeyStore) EncryptMessage(deviceIMEI string, plaintext []byte) ([]byte, error) {
	entry, err := ks.GetLatest(deviceIMEI)
	if err != nil {
		return nil, err
	}
	if entry.KeyHex == "" {
		return nil, fmt.Errorf("crypto: no key material for device %s (passthrough mode?)", deviceIMEI)
	}

	key, err := hex.DecodeString(entry.KeyHex)
	if err != nil {
		return nil, fmt.Errorf("crypto: decode key: %w", err)
	}
	return Encrypt(key, plaintext)
}

// DeviceCount returns the number of devices with stored keys.
func (ks *KeyStore) DeviceCount() int {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	return len(ks.keys)
}
