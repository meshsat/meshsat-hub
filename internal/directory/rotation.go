package directory

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Key rotation without leaving a bridge behind (contract C7.6). A bridge
// trusts the one key it pinned at provisioning. A rotation record, signed
// by the OLD key, names the new key and takes a slot in the tenant's
// directory version sequence; a bridge applies the records it is missing in
// order, then the snapshot the new key signed. A rotate that is not signed
// by the key the bridge trusts is refused by the bridge. The Hub keeps every
// record. [MESHSAT-1584 C7]

// nextKeyConfig holds the new private key between "generated" and
// "current", so a rotation that stopped half-way resumes with the key its
// records already name. Sealed at rest like the current key.
const nextKeyConfig = "directory_signing_key_next"

// RotationPayload is what a rotation record signs: the fixture's shape
// (docs/fixtures/v1/directory_v2.json).
type RotationPayload struct {
	Type       string `json:"type"`
	TenantID   string `json:"tenant_id"`
	Version    int64  `json:"version"`
	NewKeyID   string `json:"new_key_id"`
	NewKeyPKIX []byte `json:"new_key_pkix"`
}

// Rotation is one stored record.
type Rotation struct {
	TenantID string
	Version  int64
	OldKeyID string
	NewKeyID string
	Envelope Envelope
}

// ErrRotationInProgress: another rotation from the current key has begun
// and its pending key is not visible here yet.
var ErrRotationInProgress = errors.New("directory: a key rotation from this key is already under way")

// appendRotation writes the tenant's record for oldKeyID -> newKeyID, if it
// has none yet, in one transaction with the version bump the record takes.
// seal signs the record at that version.
func (s *SQLStore) appendRotation(ctx context.Context, tenantID, oldKeyID, newKeyID string, seal func(version int64) (*Envelope, error)) (bool, error) {
	inserted := false
	err := s.write(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM directory_rotations WHERE tenant_id = $1 AND old_key_id = $2`,
			tenantID, oldKeyID).Scan(&n); err != nil {
			return fmt.Errorf("read rotations: %w", err)
		}
		if n > 0 {
			return nil
		}
		now := s.ts(storeNow())
		version, err := bumpVersion(ctx, tx, tenantID, now)
		if err != nil {
			return err
		}
		env, err := seal(version)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(env)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO directory_rotations (tenant_id, version, old_key_id, new_key_id, envelope, created_at)
			VALUES ($1, $2, $3, $4, $5, $6)`, tenantID, version, oldKeyID, newKeyID, string(raw), now); err != nil {
			return fmt.Errorf("store rotation: %w", err)
		}
		inserted = true
		return nil
	})
	return inserted, err
}

// RotationChain returns the tenant's records that lead from fromKeyID to the
// newest key, oldest first; empty when fromKeyID is the newest or the Hub
// has no record from it.
func (s *SQLStore) RotationChain(ctx context.Context, tenantID, fromKeyID string) ([]Rotation, error) {
	if tenantID == "" || fromKeyID == "" {
		return nil, ErrEmptyID
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT version, old_key_id, new_key_id, envelope FROM directory_rotations
		WHERE tenant_id = $1 ORDER BY version ASC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("read rotations: %w", err)
	}
	byOld := map[string]Rotation{}
	for rows.Next() {
		r := Rotation{TenantID: tenantID}
		var raw string
		if err := rows.Scan(&r.Version, &r.OldKeyID, &r.NewKeyID, &raw); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &r.Envelope); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("rotation %d: %w", r.Version, err)
		}
		byOld[r.OldKeyID] = r
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}
	var chain []Rotation
	seen := map[string]bool{}
	for cur := fromKeyID; !seen[cur]; {
		seen[cur] = true
		r, ok := byOld[cur]
		if !ok {
			break
		}
		chain = append(chain, r)
		cur = r.NewKeyID
	}
	return chain, nil
}

// RotationResult says what a rotation did.
type RotationResult struct {
	OldKeyID string `json:"old_key_id"`
	NewKeyID string `json:"new_key_id"`
	Tenants  int    `json:"tenants"`
	Records  int    `json:"records_added"`
	Resumed  bool   `json:"resumed"`
}

// RotateSigningKey replaces the Hub's directory signing key. Every step can
// be repeated, so a rotation that stopped resumes where it stopped:
//
//  1. the new private key is stored as pending (sealed), unless one is
//     pending already; claim makes sure only one caller generates it;
//  2. each tenant gets its rotation record, signed by the old key, in one
//     transaction with the version bump it takes (a tenant that has one
//     is skipped);
//  3. the new key becomes current and the pending one is cleared.
//
// The new key is stored before any record names it, so no record can name a
// key the Hub does not hold. tenants should be every tenant: a bridge of a
// tenant without a record could not follow the rotation.
func RotateSigningKey(ctx context.Context, kv signingKeyStore, claim func(ctx context.Context, key string) (bool, error),
	dir *SQLStore, anchor *TrustAnchor, tenants []string) (*RotationResult, error) {
	if err := anchor.Refresh(ctx, kv); err != nil {
		return nil, err
	}
	oldPEM, oldID := anchor.current()
	res := &RotationResult{OldKeyID: oldID, Tenants: len(tenants)}
	pending, err := kv.GetSystemConfig(ctx, nextKeyConfig)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("directory: read pending key: %w", err)
	}
	if pending == oldPEM {
		// Stopped after step 3 wrote the key: only the clean-up is left.
		res.NewKeyID, res.Resumed = oldID, true
		return res, kv.SetSystemConfig(ctx, nextKeyConfig, "")
	}
	if pending == "" {
		won, err := claim(ctx, "directory-rotate:"+oldID)
		if err != nil {
			return nil, fmt.Errorf("directory: claim rotation: %w", err)
		}
		if !won {
			return nil, ErrRotationInProgress
		}
		_, pemBytes, err := generateECPrivateKeyPEM()
		if err != nil {
			return nil, err
		}
		if err := kv.SetSystemConfig(ctx, nextKeyConfig, string(pemBytes)); err != nil {
			return nil, fmt.Errorf("directory: store pending key: %w", err)
		}
		pending = string(pemBytes)
	} else {
		res.Resumed = true
	}
	next, err := parseECPrivateKeyPEM([]byte(pending))
	if err != nil {
		return nil, fmt.Errorf("directory: pending key: %w", err)
	}
	nextPKIX, err := x509.MarshalPKIXPublicKey(&next.PublicKey)
	if err != nil {
		return nil, err
	}
	res.NewKeyID = KeyID(nextPKIX)
	for _, tenant := range tenants {
		added, err := dir.appendRotation(ctx, tenant, oldID, res.NewKeyID, func(version int64) (*Envelope, error) {
			payload, err := json.Marshal(RotationPayload{Type: PayloadRotation, TenantID: tenant, Version: version,
				NewKeyID: res.NewKeyID, NewKeyPKIX: nextPKIX})
			if err != nil {
				return nil, err
			}
			return anchor.sealWith(oldID, payload)
		})
		if err != nil {
			return res, fmt.Errorf("directory: rotation record for %s: %w", tenant, err)
		}
		if added {
			res.Records++
		}
	}
	if err := kv.SetSystemConfig(ctx, systemConfigKey, pending); err != nil {
		return res, fmt.Errorf("directory: make the new key current: %w", err)
	}
	anchor.set(next, nextPKIX, pending)
	if err := kv.SetSystemConfig(ctx, nextKeyConfig, ""); err != nil {
		return res, fmt.Errorf("directory: clear pending key: %w", err)
	}
	slog.Info("directory: signing key rotated", "old_key_id", oldID, "new_key_id", res.NewKeyID,
		"tenants", len(tenants), "records_added", res.Records, "resumed", res.Resumed)
	return res, nil
}

// Refresh reloads the current key from the store when another replica
// changed it. A stored key that does not parse is an error, never a reason
// to make a new one.
func (t *TrustAnchor) Refresh(ctx context.Context, s signingKeyStore) error {
	stored, err := s.GetSystemConfig(ctx, systemConfigKey)
	if err != nil {
		return fmt.Errorf("directory: read signing key: %w", err)
	}
	if stored == "" {
		return errors.New("directory: no signing key stored")
	}
	t.mu.RLock()
	same := sha256.Sum256([]byte(stored)) == t.pemSum
	t.mu.RUnlock()
	if same {
		return nil
	}
	key, err := parseECPrivateKeyPEM([]byte(stored))
	if err != nil {
		return fmt.Errorf("directory: stored signing key: %w", err)
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return err
	}
	t.set(key, pub, stored)
	slog.Info("directory: signing key reloaded", "key_id", KeyID(pub))
	return nil
}

// KeepFresh refreshes every interval until ctx ends, so a replica that did
// not rotate signs with the new key within one interval.
func (t *TrustAnchor) KeepFresh(ctx context.Context, s signingKeyStore, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := t.Refresh(ctx, s); err != nil {
				slog.Warn("directory: signing key refresh", "error", err)
			}
		}
	}
}

// KeyID is the id of the anchor's current key.
func (t *TrustAnchor) KeyID() string {
	_, id := t.current()
	return id
}
