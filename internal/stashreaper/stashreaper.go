// Package stashreaper removes one-time provisioning material that was never
// claimed.
//
// # Why (MESHSAT-1098)
//
// A bridge provisioning bundle is stashed in system_config under
// provision_stash:{bridge_id} and handed over once, on a claim. Claiming blanks
// it. Nothing ever removed the ones that were NOT claimed.
//
// Production held twelve of them, the oldest from 2026-03-28 -- six months. Each
// carried the plaintext MQTT password and the client PRIVATE KEY, which exist
// nowhere else: the bridges table keeps only the bcrypt hash and the
// certificate. All twelve were in every barman backup, and that database is
// shipped to an object store with no encryption declared.
//
// Two of the twelve belonged to bridges that are online today, because the
// direct Provision endpoint returned the bundle and never blanked its stash.
// That is fixed at the source; this is the sweeper for everything that slipped
// through before, and for the ordinary case of a QR nobody ever scans.
//
// # Why a sweeper rather than a shorter TTL
//
// The TTL already exists and is already enforced -- but only when somebody
// arrives to claim. An abandoned stash is precisely the one nobody arrives for,
// so the TTL never runs for it. Expiry has to be driven by something other than
// the request that will never come.
package stashreaper

import (
	"context"
	"log/slog"
	"time"
)

// Store is the slice of store.Store this needs.
type Store interface {
	ListSystemConfigOlderThan(ctx context.Context, prefix string, cutoff time.Time) ([]string, error)
	DeleteSystemConfig(ctx context.Context, key string) error
}

// Prefix names a family of one-time rows and how long one may live past its
// last write before the sweeper takes it.
type Prefix struct {
	Prefix string
	TTL    time.Duration
	Why    string
}

// Job sweeps expired one-time rows out of system_config.
type Job struct {
	store    Store
	prefixes []Prefix
	every    time.Duration
	now      func() time.Time
	log      *slog.Logger
}

// New returns a Job. Interval defaults to an hour: these rows are a disclosure
// risk measured in months, not a queue with a latency budget.
func New(s Store, prefixes []Prefix, every time.Duration, log *slog.Logger) *Job {
	if every <= 0 {
		every = time.Hour
	}
	if log == nil {
		log = slog.Default()
	}
	return &Job{store: s, prefixes: prefixes, every: every, now: time.Now, log: log}
}

// Run sweeps once and then on the interval, until ctx is cancelled.
//
// Registered as a leader singleton: it deletes shared rows, so having three
// replicas race to delete the same key would be noise at best. The sweep at
// the top handles the case where nothing has been leader for a while.
func (j *Job) Run(ctx context.Context) {
	t := time.NewTicker(j.every)
	defer t.Stop()
	j.once(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			j.once(ctx)
		}
	}
}

func (j *Job) once(ctx context.Context) {
	for _, p := range j.prefixes {
		// The grace is deliberately longer than the TTL the claim path
		// enforces. A row that expired thirty seconds ago is still being
		// claimed by somebody whose request is in flight, and a sweeper that
		// wins that race turns a working provision into a mystery 404.
		cutoff := j.now().Add(-p.TTL).Add(-graceAfterTTL)
		keys, err := j.store.ListSystemConfigOlderThan(ctx, p.Prefix, cutoff)
		if err != nil {
			j.log.Error("stashreaper: listing expired rows failed, will retry next run",
				"prefix", p.Prefix, "error", err)
			continue
		}
		removed := 0
		for _, k := range keys {
			if err := j.store.DeleteSystemConfig(ctx, k); err != nil {
				// Per row, so one failure does not stall the rest.
				j.log.Error("stashreaper: could not remove an expired row",
					"key", k, "error", err)
				continue
			}
			removed++
		}
		if removed > 0 {
			// Warn, not Info: every one of these is credential material that
			// outlived its purpose, and the count is the thing worth noticing
			// if it stops being small.
			j.log.Warn("stashreaper: removed expired one-time material",
				"prefix", p.Prefix, "count", removed, "why", p.Why)
		}
	}
}

// graceAfterTTL is how long past expiry a row is left alone, so the sweeper
// cannot race a claim that is already in flight.
const graceAfterTTL = 10 * time.Minute
