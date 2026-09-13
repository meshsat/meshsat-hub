package main

import (
	"context"

	"github.com/meshsat/meshsat-hub/internal/sealedconfig"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// sealedStore intercepts the two system_config methods so the Hub's own
// long-lived keys are sealed at rest, and passes everything else through.
//
// A decorator rather than a change at each call site, because those call sites
// are in four packages that reach system_config through their own narrow
// interfaces (internal/directory, internal/reticulum, and two places here).
// Threading a keeper through all of them would be plumbing for a property that
// belongs to the storage layer, and it would still leave the next secret
// somebody stores in system_config unsealed.
//
// The list of what gets sealed is sealedconfig.Sensitive. What stops a bad wrap
// key from being read as "nothing stored" -- which is how three of those call
// sites would generate a replacement and orphan live data -- is
// sealedconfig.Preflight, which runs before any of them. This decorator is the
// mechanism; the preflight is the guard.
type sealedStore struct {
	store.Store
	keeper *sealedconfig.Keeper
}

func newSealedStore(inner store.Store, k *sealedconfig.Keeper) store.Store {
	if k == nil || !k.Sealing() {
		// No wrap key: behave exactly as before rather than half-sealing.
		return inner
	}
	return &sealedStore{Store: inner, keeper: k}
}

func (s *sealedStore) GetSystemConfig(ctx context.Context, key string) (string, error) {
	if !sealedconfig.IsSensitive(key) {
		return s.Store.GetSystemConfig(ctx, key)
	}
	v, err := s.keeper.Get(ctx, key)
	if err == nil {
		return v, nil
	}
	if err == sealedconfig.ErrNotFound {
		// Report "nothing stored" the way the underlying store does, so a
		// caller checking for sql.ErrNoRows still recognises it. Only this
		// case: an unreadable sealed value must NOT look absent.
		return s.Store.GetSystemConfig(ctx, key)
	}
	return "", err
}

func (s *sealedStore) SetSystemConfig(ctx context.Context, key, value string) error {
	if !sealedconfig.IsSensitive(key) {
		return s.Store.SetSystemConfig(ctx, key, value)
	}
	return s.keeper.Set(ctx, key, value)
}
