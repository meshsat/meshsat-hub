package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/meshsat/meshsat-hub/internal/directory"
	"github.com/meshsat/meshsat-hub/internal/protocol"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var directoryPushes = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "meshsat_hub_directory_pushes_total",
	Help: "Directory snapshots pushed to bridges, by format (v1, v2) and result (applied, current, refused, failed).",
}, []string{"format", "result"})

// DirectoryPushAnswer is what a bridge answers a directory_push with
// (Result of the command response). AppliedVersion is the version the bridge
// holds after the push, applied or not: a push that is not newer than what
// it holds is answered applied=false, reason not_newer, with its version.
type DirectoryPushAnswer struct {
	TenantID       string `json:"tenant_id"`
	Version        int64  `json:"version"`
	Applied        bool   `json:"applied"`
	Reason         string `json:"reason,omitempty"`
	AppliedVersion *int64 `json:"applied_version,omitempty"`
	// KeyID is the directory key the bridge trusts after the command.
	KeyID string `json:"key_id,omitempty"`
}

// DirectoryPushSource is what the pusher reads of the directory.
type DirectoryPushSource interface {
	CurrentVersion(ctx context.Context, tenantID string) (int64, error)
	Snapshot(ctx context.Context, tenantID string) (*directory.Snapshot, error)
	RotationChain(ctx context.Context, tenantID, fromKeyID string) ([]directory.Rotation, error)
}

// DirectoryPusherStore is what the pusher reads of the Hub store.
type DirectoryPusherStore interface {
	ListTenants(ctx context.Context) ([]store.Tenant, error)
	ListBridges(ctx context.Context, tenantID string) ([]*store.Bridge, error)
}

// DirectoryPusher keeps every online bridge at its tenant's directory
// version (contract C7.5). Each tick it compares the tenant's version with
// the one the bridge last reported, in its birth or in its answer to a push,
// and pushes the snapshot to a bridge that is behind. The tick is the
// debounce: writes within one tick go out as one push of the last version.
// A bridge that missed a push gets it within a tick of its next birth. It
// runs on the leader only; its memory is a cache of what bridges said, so a
// new leader at worst pushes once more, and a bridge answers a push that is
// not newer with the version it holds. [MESHSAT-1584 C7]
type DirectoryPusher struct {
	store  DirectoryPusherStore
	dir    DirectoryPushSource
	anchor *directory.TrustAnchor
	send   func(ctx context.Context, bridgeID string, cmd protocol.Command) (*protocol.CommandResponse, error)
	now    func() time.Time

	mu    sync.Mutex
	state map[string]*pushState // tenant + "\x00" + bridge id
	wg    sync.WaitGroup
}

type pushState struct {
	birth    [32]byte // the stored birth this state last read
	applied  int64    // the bridge's version as last heard
	keyID    string   // the key the bridge trusts, as last heard (v2 bridges)
	inflight bool
	failures int
	retryAt  time.Time
}

// NewDirectoryPusher builds the pusher. send is the commander's SendCommand.
func NewDirectoryPusher(st DirectoryPusherStore, dir DirectoryPushSource, anchor *directory.TrustAnchor,
	send func(ctx context.Context, bridgeID string, cmd protocol.Command) (*protocol.CommandResponse, error)) *DirectoryPusher {
	return &DirectoryPusher{store: st, dir: dir, anchor: anchor, send: send, now: time.Now, state: map[string]*pushState{}}
}

// Run ticks until ctx ends, then waits for the pushes in flight.
func (p *DirectoryPusher) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	defer p.wg.Wait()
	for {
		p.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick starts the pushes one pass finds owed; they run in the background.
func (p *DirectoryPusher) Tick(ctx context.Context) {
	tenants, err := p.store.ListTenants(ctx)
	if err != nil {
		slog.Warn("directory push: list tenants", "error", err)
		return
	}
	for _, t := range tenants {
		p.tickTenant(ctx, t.ID)
	}
}

// Wait waits for the pushes in flight (tests).
func (p *DirectoryPusher) Wait() { p.wg.Wait() }

func (p *DirectoryPusher) tickTenant(ctx context.Context, tenantID string) {
	bridges, err := p.store.ListBridges(ctx, tenantID)
	if err != nil || len(bridges) == 0 {
		if err != nil {
			slog.Warn("directory push: list bridges", "tenant", tenantID, "error", err)
		}
		return
	}
	version, err := p.dir.CurrentVersion(ctx, tenantID)
	if err != nil || version == 0 {
		if err != nil {
			slog.Warn("directory push: version", "tenant", tenantID, "error", err)
		}
		return
	}
	// The tenant goes v2 once every bridge of it says it verifies v2.
	v2 := true
	type owed struct {
		bridgeID string
		st       *pushState
	}
	var due []owed
	now := p.now()
	p.mu.Lock()
	listed := make(map[string]bool, len(bridges))
	for _, b := range bridges {
		listed[tenantID+"\x00"+b.BridgeID] = true
	}
	for key := range p.state {
		if strings.HasPrefix(key, tenantID+"\x00") && !listed[key] && !p.state[key].inflight {
			delete(p.state, key) // a bridge deleted or moved away
		}
	}
	for _, b := range bridges {
		birth := parseBirth(b.LastBirth)
		v2 = v2 && slices.Contains(birth.Capabilities, protocol.CapDirectoryV2)
		key := tenantID + "\x00" + b.BridgeID
		st := p.state[key]
		if st == nil {
			st = &pushState{}
			p.state[key] = st
		}
		// A new birth is what the bridge says now, also when it holds
		// less than it answered before (a reset bridge starts over).
		if sum := sha256.Sum256([]byte(b.LastBirth)); sum != st.birth {
			st.birth, st.applied, st.failures, st.retryAt = sum, birth.DirectoryVersion, 0, time.Time{}
			st.keyID = birth.DirectoryKeyID
		}
		if !b.Online || st.inflight || st.applied >= version || now.Before(st.retryAt) {
			continue
		}
		st.inflight = true
		due = append(due, owed{b.BridgeID, st})
	}
	p.mu.Unlock()
	if len(due) == 0 {
		return
	}
	cmd, format, sent, err := p.command(ctx, tenantID, v2)
	if err != nil || format == "" {
		p.mu.Lock()
		for _, d := range due {
			d.st.inflight = false
			if err == nil {
				// Nothing a v1 bridge should get: the bridge is current
				// with what the Hub has for it at this version.
				d.st.applied = sent
				directoryPushes.WithLabelValues("v1", "empty").Inc()
			}
		}
		p.mu.Unlock()
		if err != nil {
			slog.Warn("directory push: build", "tenant", tenantID, "error", err)
		}
		return
	}
	for _, d := range due {
		p.wg.Add(1)
		go func(bridgeID string, st *pushState) {
			defer p.wg.Done()
			p.push(ctx, tenantID, bridgeID, st, cmd, format, sent)
		}(d.bridgeID, d.st)
	}
}

// command builds the tenant's push: the v2 envelope, or the signed v1
// snapshot. sent is the version it carries. A v1 snapshot leaves out the
// precedence defaults the Hub seeded: v1 cannot say a row is seeded, and a
// v1 bridge applies a pushed policy over its own for the scope and clears
// its seeded mark, so a Hub default would replace what the kit's operator
// set. v2 carries the mark and the bridge decides (C7.7). A v1 snapshot with
// nothing left in it is not sent: format is "" then.
func (p *DirectoryPusher) command(ctx context.Context, tenantID string, v2 bool) (protocol.Command, string, int64, error) {
	snap, err := p.dir.Snapshot(ctx, tenantID)
	if err != nil {
		return protocol.Command{}, "", 0, err
	}
	if v2 {
		env, err := p.anchor.SealSnapshot(snap)
		if err != nil {
			return protocol.Command{}, "", 0, err
		}
		return DirectoryPushV2Command(env), "v2", snap.Version, nil
	}
	kept := snap.Policies[:0:0]
	for _, pol := range snap.Policies {
		if !pol.Seeded {
			kept = append(kept, pol)
		}
	}
	snap.Policies = kept
	if len(snap.Contacts) == 0 && len(snap.Policies) == 0 {
		return protocol.Command{}, "", snap.Version, nil
	}
	sig, err := p.anchor.SignV1(snap)
	if err != nil {
		return protocol.Command{}, "", 0, err
	}
	snap.Signature = sig
	return DirectoryPushCommand(snap), "v1", snap.Version, nil
}

func (p *DirectoryPusher) push(ctx context.Context, tenantID, bridgeID string, st *pushState, cmd protocol.Command, format string, sent int64) {
	var (
		resp *protocol.CommandResponse
		err  error
		ans  DirectoryPushAnswer
	)
	if format == "v2" {
		err = p.rotate(ctx, tenantID, bridgeID, st)
	}
	if err == nil {
		cmd.RequestID = "" // one id per bridge
		resp, err = p.send(ctx, bridgeID, cmd)
	}
	ok := err == nil && resp != nil && resp.Status == "ok" && json.Unmarshal(resp.Result, &ans) == nil
	result := "failed"
	p.mu.Lock()
	defer p.mu.Unlock()
	st.inflight = false
	if ok && ans.KeyID != "" {
		st.keyID = ans.KeyID
	}
	switch {
	case ok && ans.AppliedVersion != nil:
		st.applied = *ans.AppliedVersion
		result = "current"
		if ans.Applied {
			result = "applied"
		}
	case ok && ans.Applied:
		// A bridge from before applied_version answers what it applied.
		st.applied = sent
		result = "applied"
	case ok:
		result = "refused"
	}
	if result == "applied" || (result == "current" && st.applied >= sent) {
		st.failures, st.retryAt = 0, time.Time{}
	} else {
		st.failures++
		st.retryAt = p.now().Add(pushBackoff(st.failures))
		reason := ans.Reason
		if err != nil {
			reason = err.Error()
		} else if resp != nil && resp.Error != "" {
			reason = resp.Error
		}
		slog.Warn("directory push: not applied", "tenant", tenantID, "bridge", bridgeID, "format", format,
			"version", sent, "result", result, "reason", reason, "retry_at", st.retryAt)
	}
	directoryPushes.WithLabelValues(format, result).Inc()
	if result == "applied" {
		slog.Info("directory push: applied", "tenant", tenantID, "bridge", bridgeID, "format", format, "version", sent)
	}
}

// rotate brings a v2 bridge to the current signing key before a snapshot:
// it sends the rotation records from the key the bridge trusts on, oldest
// first, each signed by the key before it, and stops at the first one the
// bridge does not take. A bridge that trusts a key the Hub holds no chain
// from cannot verify the snapshot, so nothing is sent to it.
func (p *DirectoryPusher) rotate(ctx context.Context, tenantID, bridgeID string, st *pushState) error {
	p.mu.Lock()
	trusted := st.keyID
	p.mu.Unlock()
	current := p.anchor.KeyID()
	if trusted == current {
		return nil
	}
	if trusted == "" {
		return fmt.Errorf("bridge reports no directory key (not paired)")
	}
	chain, err := p.dir.RotationChain(ctx, tenantID, trusted)
	if err != nil {
		return err
	}
	if len(chain) == 0 || chain[len(chain)-1].NewKeyID != current {
		return fmt.Errorf("bridge trusts key %s and no rotation chain leads from it to %s", trusted, current)
	}
	for _, r := range chain {
		env := r.Envelope
		resp, err := p.send(ctx, bridgeID, DirectoryRotateCommand(&env))
		if err != nil {
			return fmt.Errorf("rotation %d: %w", r.Version, err)
		}
		var ans DirectoryPushAnswer
		if resp.Status != "ok" || json.Unmarshal(resp.Result, &ans) != nil || ans.KeyID != r.NewKeyID {
			return fmt.Errorf("rotation %d not taken: %s %s", r.Version, resp.Error, ans.Reason)
		}
		p.mu.Lock()
		st.keyID = ans.KeyID
		if ans.AppliedVersion != nil {
			st.applied = *ans.AppliedVersion
		}
		p.mu.Unlock()
		directoryPushes.WithLabelValues("rotation", "applied").Inc()
		slog.Info("directory push: rotation applied", "tenant", tenantID, "bridge", bridgeID, "version", r.Version, "key_id", r.NewKeyID)
	}
	return nil
}

// pushBackoff is 30 s doubling per failure, at most 30 min: a bridge that
// cannot take the push (no pinned anchor, an old bridge) is asked again
// rarely, not every tick.
func pushBackoff(failures int) time.Duration {
	d := 30 * time.Second
	for i := 1; i < failures && d < 30*time.Minute; i++ {
		d *= 2
	}
	return min(d, 30*time.Minute)
}

// parseBirth reads what the pusher needs from a stored birth; an empty or
// unreadable one reads as version 0 without capabilities.
func parseBirth(raw string) protocol.BridgeBirth {
	var b protocol.BridgeBirth
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &b)
	}
	return b
}
