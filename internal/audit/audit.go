// Package audit provides a tamper-evident hash-chain audit log.
// Each entry's hash is computed from its content + the previous entry's hash,
// forming an append-only chain that can be verified for integrity.
package audit

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// HashVersionCurrent is what every new entry is written with.
const HashVersionCurrent = 2

// hashTimeLayout is the canonical form of created_at inside a v2 digest:
// UTC, microseconds. Both stores round-trip microseconds exactly (TIMESTAMPTZ
// in Postgres, a fixed-width text column in SQLite), which is what makes the
// timestamp hashable at all -- a value that came back different from how it
// went in would break every chain on the next verify.
const hashTimeLayout = "2006-01-02T15:04:05.000000Z07:00"

// Service manages the append-only audit log with hash-chain tamper evidence.
type Service struct {
	store store.Store
	// mu serialises appends within this process. Cross-replica ordering is the
	// store's job (AppendAuditEntry holds a database-side lock per tenant);
	// this only spares the SQLite store, which has no such lock, from
	// contending writers in one process. The callback under it does no I/O
	// outside the store's own transaction.
	mu sync.Mutex
}

// New creates an audit service.
func New(s store.Store) *Service {
	return &Service{store: s}
}

// Store returns the underlying store for direct queries (e.g. listing entries).
func (s *Service) Store() store.Store {
	return s.store
}

// Log appends a new entry to the audit chain for the given tenant.
// ID, CreatedAt, PrevHash, HashVersion and Hash are all fixed here, before
// the row is written, so the digest binds every one of them.
func (s *Service) Log(ctx context.Context, tenantID, action, actor, detail, ip string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.store.AppendAuditEntry(ctx, tenantID, func(prev *store.AuditEntry) (*store.AuditEntry, error) {
		prevHash := ""
		now := time.Now().UTC().Truncate(time.Microsecond)
		if prev != nil {
			prevHash = prev.Hash
			// created_at is the chain's ORDER (both stores read the latest entry
			// by it), and it is now written from the replica's clock rather than
			// the database's. Two replicas' clocks disagree by milliseconds, so
			// an entry chained correctly onto its predecessor could still sort
			// before it and the next append would chain onto the wrong row.
			// Monotonic per tenant, always.
			if !now.After(prev.CreatedAt) {
				now = prev.CreatedAt.Add(time.Microsecond)
			}
		}
		e := &store.AuditEntry{
			ID:          newEntryID(now),
			Action:      action,
			Actor:       actor,
			Detail:      detail,
			IP:          ip,
			PrevHash:    prevHash,
			HashVersion: HashVersionCurrent,
			CreatedAt:   now,
		}
		e.Hash = ComputeHashV2(tenantID, e)
		return e, nil
	})
}

// newEntryID keeps the historical aud-<unixnano> shape and adds 32 random
// bits, because two replicas can and do write in the same nanosecond.
func newEntryID(now time.Time) string {
	var r [4]byte
	_, _ = rand.Read(r[:])
	return "aud-" + strconv.FormatInt(now.UnixNano(), 10) + "-" + hex.EncodeToString(r[:])
}

// ComputeHash is the version-1 digest: SHA-256 of action|actor|detail|ip|prev_hash.
// Kept only to verify rows written before hash_version existed. It does not
// bind the tenant, the id or the timestamp, which is why it was replaced.
func ComputeHash(e *store.AuditEntry) string {
	data := fmt.Sprintf("%s|%s|%s|%s|%s", e.Action, e.Actor, e.Detail, e.IP, e.PrevHash)
	h := sha256.Sum256([]byte(data))
	return hex.EncodeToString(h[:])
}

// ComputeHashV2 is the version-2 digest. It binds the tenant the row belongs
// to, its id, its timestamp and everything v1 bound, each field
// length-prefixed so that a "|" inside detail cannot be used to shift the
// boundaries between fields.
func ComputeHashV2(tenantID string, e *store.AuditEntry) string {
	h := sha256.New()
	for _, f := range []string{
		"v2", tenantID, e.ID, e.CreatedAt.UTC().Format(hashTimeLayout),
		e.Action, e.Actor, e.Detail, e.IP, e.PrevHash,
	} {
		_, _ = fmt.Fprintf(h, "%d:%s;", len(f), f) // hash.Hash never errors
	}
	return hex.EncodeToString(h.Sum(nil))
}

// VerifyChain verifies the integrity of the audit chain for a tenant.
// Returns the number of entries verified and the first broken entry (if any).
//
// Each row is checked with the digest its hash_version names. A version-1 row
// is accepted only while no version-2 row has been seen yet: legacy rows are
// history at the head of the chain, and a "legacy" row appearing after the
// cut-over is a downgrade -- exactly what an attacker recomputing an edited
// row "the old way" would produce -- so it breaks the chain.
func (s *Service) VerifyChain(ctx context.Context, tenantID string) (verified int, brokenAt *store.AuditEntry, err error) {
	// Fetch all entries oldest-first for chain verification.
	entries, err := s.store.ListAuditEntries(ctx, tenantID, 0)
	if err != nil {
		return 0, nil, fmt.Errorf("list audit entries: %w", err)
	}

	// ListAuditEntries returns newest-first; reverse for chain walk.
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}

	// Anchor at the oldest retained entry: retention purges the head of the
	// chain, so the first surviving entry legitimately links to a hash that
	// is no longer stored. Its own hash still covers that link.
	prevHash := ""
	if len(entries) > 0 {
		prevHash = entries[0].PrevHash
	}
	seenV2 := false
	for i, e := range entries {
		// Verify prev_hash links to the previous entry.
		if e.PrevHash != prevHash {
			return i, &entries[i], nil
		}
		// Verify the hash matches the content, with the formula the row claims.
		var expected string
		switch e.HashVersion {
		case 1:
			if seenV2 {
				return i, &entries[i], nil
			}
			expected = ComputeHash(&entries[i])
		case 2:
			seenV2 = true
			expected = ComputeHashV2(tenantID, &entries[i])
		default:
			return i, &entries[i], nil
		}
		if e.Hash != expected {
			return i, &entries[i], nil
		}
		prevHash = e.Hash
	}

	return len(entries), nil, nil
}
