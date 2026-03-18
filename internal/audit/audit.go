// Package audit provides a tamper-evident hash-chain audit log.
// Each entry's hash is computed from its content + the previous entry's hash,
// forming an append-only chain that can be verified for integrity.
package audit

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/cubeos-app/meshsat-hub/internal/store"
)

// Service manages the append-only audit log with hash-chain tamper evidence.
type Service struct {
	store store.Store
	mu    sync.Mutex // serializes writes to maintain chain integrity
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
// The entry's Hash and PrevHash fields are computed automatically.
func (s *Service) Log(ctx context.Context, tenantID, action, actor, detail, ip string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Get the previous entry's hash to chain.
	prevHash := ""
	prev, err := s.store.GetLatestAuditEntry(ctx, tenantID)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("get latest audit entry: %w", err)
	}
	if prev != nil {
		prevHash = prev.Hash
	}

	entry := &store.AuditEntry{
		Action:   action,
		Actor:    actor,
		Detail:   detail,
		IP:       ip,
		PrevHash: prevHash,
	}
	entry.Hash = ComputeHash(entry)

	return s.store.InsertAuditEntry(ctx, tenantID, entry)
}

// ComputeHash computes the SHA-256 hash of an audit entry's content + prev_hash.
func ComputeHash(e *store.AuditEntry) string {
	data := fmt.Sprintf("%s|%s|%s|%s|%s", e.Action, e.Actor, e.Detail, e.IP, e.PrevHash)
	h := sha256.Sum256([]byte(data))
	return hex.EncodeToString(h[:])
}

// VerifyChain verifies the integrity of the audit chain for a tenant.
// Returns the number of entries verified and the first broken entry (if any).
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

	prevHash := ""
	for i, e := range entries {
		// Verify prev_hash links to the previous entry.
		if e.PrevHash != prevHash {
			return i, &entries[i], nil
		}
		// Verify the hash matches the content.
		expected := ComputeHash(&entries[i])
		if e.Hash != expected {
			return i, &entries[i], nil
		}
		prevHash = e.Hash
	}

	return len(entries), nil, nil
}
