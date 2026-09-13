package sealedconfig

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
)

// Sensitive is the set of system_config keys that are sealed at rest.
//
// It is a list rather than a heuristic on purpose. A rule like "anything whose
// name contains key" would have swept up mqtt_public_url, and a rule that
// guessed from the value would have swept up whatever the next feature stores.
// Adding a secret to system_config means adding it here, and the preflight
// below is what makes forgetting visible.
var Sensitive = []string{
	"bridge_ca_key",
	"directory_signing_key",
	"credential_master_key",
	"reticulum_signing_key",
	"reticulum_encryption_key",
}

// IsSensitive reports whether a key is sealed at rest.
func IsSensitive(key string) bool {
	for _, k := range Sensitive {
		if k == key {
			return true
		}
	}
	return false
}

// Preflight opens every sealed Sensitive key and reports the first that fails.
//
// THIS IS THE GUARD. Three of the five call sites respond to an unreadable
// value by generating a new one and carrying on -- the master key with a log
// line reading "bootstrapped", the directory anchor silently replacing the key
// every field bridge pinned. By the time any of them runs, this has already
// decided whether the wrap key is the right one.
//
// It returns (migrated, error). A nil error means every sealed value opened, so
// the process may proceed to the paths that can generate. A non-nil error must
// stop the process: there is a value here, it belongs to somebody, and starting
// anyway replaces it.
func Preflight(ctx context.Context, k *Keeper, log *slog.Logger) (int, error) {
	if log == nil {
		log = slog.Default()
	}
	if !k.Sealing() {
		log.Warn("sealedconfig: no wrap key, so the Hub's own keys stay in clear text in the database, " +
			"which barman ships to an object store that declares no encryption (MESHSAT-1098)")
		// Still fatal if a sealed row exists -- that means the key was there
		// and is now gone, and proceeding regenerates.
	}

	keys := append([]string(nil), Sensitive...)
	sort.Strings(keys)

	opened, migrated := 0, 0
	for _, key := range keys {
		sealed, err := k.store.GetSystemConfig(ctx, key+Suffix)
		if err == nil && sealed != "" {
			if _, err := k.Get(ctx, key); err != nil {
				return migrated, fmt.Errorf("%s: %w", key, err)
			}
			opened++
			continue
		}
		moved, err := k.Migrate(ctx, key)
		if err != nil {
			return migrated, fmt.Errorf("%s: %w", key, err)
		}
		if moved {
			migrated++
		}
	}
	if k.Sealing() {
		log.Info("sealedconfig: the Hub's own keys are sealed at rest",
			"opened", opened, "migrated", migrated, "total", len(keys))
	}
	return migrated, nil
}
