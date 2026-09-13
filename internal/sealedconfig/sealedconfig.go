// Package sealedconfig keeps the Hub's own long-lived keys wrapped at rest.
//
// # Why (MESHSAT-1098)
//
// system_config held five private keys in clear text:
//
//	bridge_ca_key             signs EVERY bridge's client certificate
//	directory_signing_key     signs the tenant directory every bridge pins
//	credential_master_key     decrypts EVERY tenant's carrier credentials
//	reticulum_signing_key     the Hub's Reticulum identity
//	reticulum_encryption_key
//
// barman ships that database to an object store, and the CNPG cluster declares
// no encryption on either the data or the WAL. So a backup on its own was enough
// to mint a client certificate for any customer's bridge, and enough to open
// every tenant's Cloudloop and Twilio credentials -- which are billed to the
// customer, not to us.
//
// This codebase had already solved the same problem one directory over.
// internal/takoperator/pki.go wraps each tenant's TAK CA key and says why:
// "This cluster stores Secrets unencrypted in etcd, and Velero copies them to
// the object store." The Hub's own database never got that reasoning applied.
//
// # The shape
//
// A value stored under KEY moves to KEY+"_enc", AES-256-GCM sealed with a key
// that lives only in OpenBao and reaches the process as an environment
// variable. Reads prefer the sealed row and fall back to the plaintext one, so
// a deploy migrates forward on its own and can be rolled back at any point
// before the plaintext rows are blanked. Writes always seal.
//
// Wire format is the one this codebase already uses everywhere:
// [12-byte random nonce][ciphertext + 16-byte tag], via internal/crypto. There
// is deliberately no new cipher and no new format.
//
// # The rule that matters more than any of the above
//
// A SEALED VALUE THAT DOES NOT AUTHENTICATE MUST NEVER FALL THROUGH TO
// "generate a new one". Every one of the five call sites had that exact hole:
//
//   - credential_master_key was accepted only if it hex-decoded to 32 bytes,
//     and anything else generated a fresh key and logged "bootstrapped" --
//     every tenant's carrier credentials undecryptable, and the log reading
//     like a normal first boot.
//   - directory_signing_key was worse: a parse error was swallowed entirely and
//     a new anchor was generated and persisted, silently invalidating the key
//     every field bridge pinned at provisioning.
//   - bridge_ca_key only tested != "", leaving a nil CA, 500s on every issuance
//     and a CA Secret exporter that never starts.
//
// So Open returns ErrSealed on a failed unwrap and the caller must treat it as
// fatal. Losing the wrap key has to be loud, because the alternative is lossy.
package sealedconfig

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	hubcrypto "github.com/meshsat/meshsat-hub/internal/crypto"
)

// Suffix is appended to a key to name its sealed twin. The plaintext row keeps
// its own name so a rollback needs no migration and no rename.
const Suffix = "_enc"

// noValue is what External Secrets renders for a property missing from the
// backing store. It is four characters of ordinary text, not an empty string,
// so every `!= ""` guard in the process would treat it as a configured key.
// internal/config refuses it for the Stripe variables; this refuses it here,
// where accepting it would mean sealing five keys under the literal string
// "<no value>" and never being able to open them again.
const noValue = "<no value>"

var (
	// ErrNoWrapKey means the process was not given one. Callers decide whether
	// that is fatal; during the migration it is not, because the plaintext rows
	// are still readable.
	ErrNoWrapKey = errors.New("sealedconfig: no wrap key")

	// ErrSealed means a sealed value exists and did not authenticate. This is
	// never recoverable by generating a new key, and a caller that treats it as
	// "absent" destroys data.
	ErrSealed = errors.New("sealedconfig: the sealed value did not authenticate (wrong wrap key?)")

	// ErrNotFound means neither the sealed nor the plaintext row exists. This
	// one IS the signal to generate.
	ErrNotFound = errors.New("sealedconfig: no value")
)

// Store is the slice of store.Store this package needs. Declared here rather
// than imported so the dependency runs one way.
type Store interface {
	GetSystemConfig(ctx context.Context, key string) (string, error)
	SetSystemConfig(ctx context.Context, key, value string) error
}

// Keeper seals and opens system_config values.
type Keeper struct {
	store   Store
	wrapKey []byte
}

// ParseWrapKey decodes the hex wrap key the Hub is given, refusing the shapes
// that would otherwise be sealed under and never opened again.
func ParseWrapKey(hexKey string) ([]byte, error) {
	v := strings.TrimSpace(hexKey)
	if v == "" {
		return nil, ErrNoWrapKey
	}
	if v == noValue {
		return nil, fmt.Errorf("%w: the secret store rendered the literal %q, "+
			"which means the property is missing; write it before referencing it", ErrNoWrapKey, noValue)
	}
	raw, err := hex.DecodeString(v)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoWrapKey, err)
	}
	if len(raw) != hubcrypto.KeySize {
		return nil, fmt.Errorf("%w: got %d bytes, want %d", ErrNoWrapKey, len(raw), hubcrypto.KeySize)
	}
	return raw, nil
}

// New returns a Keeper. A nil wrapKey is allowed and means "read plaintext, do
// not seal" -- the state before the wrap key is deployed.
func New(s Store, wrapKey []byte) *Keeper { return &Keeper{store: s, wrapKey: wrapKey} }

// Sealing reports whether this Keeper can seal. False means the process has no
// wrap key and is still reading the plaintext rows.
func (k *Keeper) Sealing() bool { return k != nil && len(k.wrapKey) == hubcrypto.KeySize }

// Get returns the value for key, preferring the sealed row.
//
// The three outcomes are deliberately distinct, and a caller that collapses
// them will destroy data: a value, ErrNotFound (nothing stored -- generate),
// or ErrSealed (something IS stored and could not be opened -- STOP).
func (k *Keeper) Get(ctx context.Context, key string) (string, error) {
	sealed, err := k.store.GetSystemConfig(ctx, key+Suffix)
	switch {
	case err == nil && sealed != "":
		if !k.Sealing() {
			// A sealed row exists and we have no key for it. Falling back to
			// the plaintext row here would look like it worked right up until
			// the plaintext rows are blanked, and then fail on a restart with
			// no obvious cause.
			return "", fmt.Errorf("%w: a sealed value exists for %q but this process has no wrap key", ErrSealed, key)
		}
		plain, oerr := k.open(sealed)
		if oerr != nil {
			return "", fmt.Errorf("%w: %s", ErrSealed, key)
		}
		return plain, nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("sealedconfig: reading %q: %w", key+Suffix, err)
	}

	plain, err := k.store.GetSystemConfig(ctx, key)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("sealedconfig: reading %q: %w", key, err)
	}
	if plain == "" {
		return "", ErrNotFound
	}
	return plain, nil
}

// Set seals and stores the value. Without a wrap key it writes the plaintext
// row, which is what the pre-migration process does and what a rollback needs.
func (k *Keeper) Set(ctx context.Context, key, value string) error {
	if !k.Sealing() {
		return k.store.SetSystemConfig(ctx, key, value)
	}
	sealed, err := k.seal(value)
	if err != nil {
		return err
	}
	return k.store.SetSystemConfig(ctx, key+Suffix, sealed)
}

// Migrate seals a value that is currently stored in plaintext, leaving the
// plaintext row alone. Blanking it is a separate, deliberate step taken only
// after the sealed rows are known to open on every replica.
//
// It is a no-op when there is no wrap key, when nothing is stored, or when the
// sealed row already exists -- so it is safe to call on every start.
func (k *Keeper) Migrate(ctx context.Context, key string) (bool, error) {
	if !k.Sealing() {
		return false, nil
	}
	if existing, err := k.store.GetSystemConfig(ctx, key+Suffix); err == nil && existing != "" {
		return false, nil
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("sealedconfig: reading %q: %w", key+Suffix, err)
	}
	plain, err := k.store.GetSystemConfig(ctx, key)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("sealedconfig: reading %q: %w", key, err)
	}
	if plain == "" {
		return false, nil
	}
	sealed, err := k.seal(plain)
	if err != nil {
		return false, err
	}
	if err := k.store.SetSystemConfig(ctx, key+Suffix, sealed); err != nil {
		return false, fmt.Errorf("sealedconfig: writing %q: %w", key+Suffix, err)
	}
	return true, nil
}

func (k *Keeper) seal(value string) (string, error) {
	ct, err := hubcrypto.Encrypt(k.wrapKey, []byte(value))
	if err != nil {
		return "", fmt.Errorf("sealedconfig: sealing: %w", err)
	}
	return base64.StdEncoding.EncodeToString(ct), nil
}

func (k *Keeper) open(sealed string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return "", err
	}
	pt, err := hubcrypto.Decrypt(k.wrapKey, raw)
	if err != nil {
		// Deliberately vague, the way takoperator's unwrap is: a tag oracle is
		// not worth the debugging convenience, and the cause is almost always
		// the wrong wrap key.
		return "", ErrSealed
	}
	return string(pt), nil
}
