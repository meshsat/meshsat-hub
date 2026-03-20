package reticulum

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// DB config keys for hub identity persistence.
const (
	configKeyEncryptionKey = "reticulum_encryption_key" // hex-encoded X25519 private key
	configKeySigningKey    = "reticulum_signing_key"    // hex-encoded Ed25519 private key
)

// IdentityStore is the subset of store.Store needed for identity persistence.
// Using an interface avoids importing the store package (which imports us).
type IdentityStore interface {
	GetSystemConfig(ctx context.Context, key string) (string, error)
	SetSystemConfig(ctx context.Context, key, value string) error
}

// identityFile is the JSON format for persisting the Hub's Reticulum identity.
type identityFile struct {
	EncryptionKeyHex string `json:"encryption_key_hex"` // 32-byte X25519 private key, hex-encoded
	SigningKeyHex    string `json:"signing_key_hex"`    // 64-byte Ed25519 private key, hex-encoded
}

// HubIdentity manages the Hub's Reticulum network identity. It generates or
// loads an Ed25519+X25519 keypair, persists it to the database (primary) and
// optionally to a file (fallback), and exposes the identity for use by the
// routing engine and API.
type HubIdentity struct {
	mu       sync.RWMutex
	identity *Identity
	appName  string
	filePath string // optional file path for fallback persistence
	store    IdentityStore
	loaded   bool
}

// NewHubIdentity creates, loads, or generates the Hub's Reticulum identity.
//
// Load order:
//  1. Database (system_config table) — primary, works in cluster mode
//  2. File on disk — fallback for standalone/migration from file-only mode
//  3. Generate new — if neither source has keys
//
// After loading or generating, keys are persisted to the database and
// optionally to the file path.
func NewHubIdentity(store IdentityStore, filePath, appName string) (*HubIdentity, error) {
	hi := &HubIdentity{
		appName:  appName,
		filePath: filePath,
		store:    store,
	}

	ctx := context.Background()

	// 1. Try loading from database.
	if store != nil {
		if err := hi.loadFromDB(ctx); err == nil {
			slog.Info("reticulum: identity loaded from database",
				"dest_hash", hi.DestHashHex(),
				"app_name", appName,
			)
			hi.loaded = true
			return hi, nil
		}
	}

	// 2. Try loading from file (migration path from file-only mode).
	if filePath != "" {
		if data, err := os.ReadFile(filePath); err == nil {
			if err := hi.loadFromJSON(data); err != nil {
				return nil, fmt.Errorf("load reticulum identity from %s: %w", filePath, err)
			}
			slog.Info("reticulum: identity loaded from file (migrating to database)",
				"file", filePath,
				"dest_hash", hi.DestHashHex(),
				"app_name", appName,
			)
			// Migrate to database.
			if store != nil {
				if err := hi.persistToDB(ctx); err != nil {
					slog.Warn("reticulum: failed to migrate identity to database", "error", err)
				}
			}
			hi.loaded = true
			return hi, nil
		}
	}

	// 3. Generate new identity.
	if err := hi.generate(); err != nil {
		return nil, fmt.Errorf("generate reticulum identity: %w", err)
	}

	// Persist to database.
	if store != nil {
		if err := hi.persistToDB(ctx); err != nil {
			return nil, fmt.Errorf("persist reticulum identity to database: %w", err)
		}
	}

	// Persist to file as backup.
	if filePath != "" {
		if err := hi.persistToFile(); err != nil {
			slog.Warn("reticulum: failed to persist identity to file (database is primary)", "error", err)
		}
	}

	slog.Info("reticulum: new identity generated and persisted",
		"dest_hash", hi.DestHashHex(),
		"app_name", appName,
	)

	hi.loaded = true
	return hi, nil
}

// Identity returns the underlying Reticulum identity.
func (hi *HubIdentity) Identity() *Identity {
	hi.mu.RLock()
	defer hi.mu.RUnlock()
	return hi.identity
}

// DestHash returns the Hub's destination hash for its configured app name.
func (hi *HubIdentity) DestHash() [TruncatedHashLen]byte {
	hi.mu.RLock()
	defer hi.mu.RUnlock()
	return hi.identity.DestHash(hi.appName)
}

// DestHashHex returns the hex-encoded destination hash.
func (hi *HubIdentity) DestHashHex() string {
	dest := hi.DestHash()
	return DestHashHex(dest)
}

// AppName returns the configured Reticulum app name.
func (hi *HubIdentity) AppName() string {
	return hi.appName
}

// IsLoaded returns true if the identity was successfully loaded or generated.
func (hi *HubIdentity) IsLoaded() bool {
	hi.mu.RLock()
	defer hi.mu.RUnlock()
	return hi.loaded
}

// PublicKeyHex returns the hex-encoded 64-byte public key ([X25519][Ed25519]).
func (hi *HubIdentity) PublicKeyHex() string {
	hi.mu.RLock()
	defer hi.mu.RUnlock()
	return hexEncode(hi.identity.PublicBytes())
}

// --- Database persistence ---

func (hi *HubIdentity) loadFromDB(ctx context.Context) error {
	encHex, err := hi.store.GetSystemConfig(ctx, configKeyEncryptionKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return fmt.Errorf("get encryption key from db: %w", err)
	}
	sigHex, err := hi.store.GetSystemConfig(ctx, configKeySigningKey)
	if err != nil {
		return fmt.Errorf("get signing key from db: %w", err)
	}

	encBytes, err := hexDecode(encHex, EncryptionPubLen)
	if err != nil {
		return fmt.Errorf("decode encryption key: %w", err)
	}
	sigBytes, err := hexDecode(sigHex, 64)
	if err != nil {
		return fmt.Errorf("decode signing key: %w", err)
	}

	id, err := LoadIdentity(encBytes, sigBytes)
	if err != nil {
		return err
	}

	hi.mu.Lock()
	hi.identity = id
	hi.mu.Unlock()
	return nil
}

func (hi *HubIdentity) persistToDB(ctx context.Context) error {
	hi.mu.RLock()
	id := hi.identity
	hi.mu.RUnlock()

	if err := hi.store.SetSystemConfig(ctx, configKeyEncryptionKey, hexEncode(id.EncryptionPrivateBytes())); err != nil {
		return fmt.Errorf("set encryption key: %w", err)
	}
	if err := hi.store.SetSystemConfig(ctx, configKeySigningKey, hexEncode(id.SigningPrivateBytes())); err != nil {
		return fmt.Errorf("set signing key: %w", err)
	}
	return nil
}

// --- File persistence (fallback) ---

func (hi *HubIdentity) loadFromJSON(data []byte) error {
	var f identityFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("parse identity JSON: %w", err)
	}

	encBytes, err := hexDecode(f.EncryptionKeyHex, EncryptionPubLen)
	if err != nil {
		return fmt.Errorf("encryption key: %w", err)
	}
	sigBytes, err := hexDecode(f.SigningKeyHex, 64) // ed25519.PrivateKeySize
	if err != nil {
		return fmt.Errorf("signing key: %w", err)
	}

	id, err := LoadIdentity(encBytes, sigBytes)
	if err != nil {
		return err
	}

	hi.mu.Lock()
	hi.identity = id
	hi.mu.Unlock()
	return nil
}

func (hi *HubIdentity) generate() error {
	id, err := GenerateIdentity()
	if err != nil {
		return err
	}
	hi.mu.Lock()
	hi.identity = id
	hi.mu.Unlock()
	return nil
}

func (hi *HubIdentity) persistToFile() error {
	hi.mu.RLock()
	id := hi.identity
	hi.mu.RUnlock()

	f := identityFile{
		EncryptionKeyHex: hexEncode(id.EncryptionPrivateBytes()),
		SigningKeyHex:    hexEncode(id.SigningPrivateBytes()),
	}

	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal identity: %w", err)
	}

	// Ensure directory exists.
	dir := filepath.Dir(hi.filePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create identity directory %s: %w", dir, err)
	}

	// Write with restrictive permissions (owner read/write only).
	if err := os.WriteFile(hi.filePath, data, 0600); err != nil {
		return fmt.Errorf("write identity file: %w", err)
	}

	return nil
}

// --- Hex helpers ---

func hexEncode(b []byte) string {
	const hextable = "0123456789abcdef"
	dst := make([]byte, len(b)*2)
	for i, v := range b {
		dst[i*2] = hextable[v>>4]
		dst[i*2+1] = hextable[v&0x0f]
	}
	return string(dst)
}

func hexDecode(s string, expectedLen int) ([]byte, error) {
	if len(s) != expectedLen*2 {
		return nil, fmt.Errorf("expected %d hex chars, got %d", expectedLen*2, len(s))
	}
	b := make([]byte, expectedLen)
	for i := range expectedLen {
		hi, err := hexVal(s[i*2])
		if err != nil {
			return nil, err
		}
		lo, err := hexVal(s[i*2+1])
		if err != nil {
			return nil, err
		}
		b[i] = (hi << 4) | lo
	}
	return b, nil
}

func hexVal(c byte) (byte, error) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', nil
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, nil
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, nil
	default:
		return 0, fmt.Errorf("invalid hex char: %c", c)
	}
}
