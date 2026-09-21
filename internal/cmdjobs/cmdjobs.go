// Package cmdjobs remembers bridge commands that are in flight or recently
// finished, so a slow command does not have to live inside one HTTP request
// (MESHSAT-1279).
//
// A command over an out-of-band bearer waits for a field kit: about ten seconds
// over SMS when all is well, a minute when it is not, and up to ten minutes for
// a satellite pass. Holding an HTTP request open that long did not survive the
// path in front of the Hub. Something on it hangs up on a request that has been
// idle for 65 s, and the edge proxy then RE-SENDS the POST, up to three times.
// On 2026-09-20 one SMS ping that got no answer became four real SMS commands
// and a 502, and nothing on the Hub had misbehaved.
//
// Two properties fix that whatever the path does:
//
//   - A command can run as a job. The POST answers at once with a request id and
//     the caller polls for the result, so no request is ever idle for long.
//   - A command is claimed before it is sent. A second POST for the same
//     command (the same request id, or the same command to the same bridge over
//     the same bearer within the window) does not send anything: it is handed
//     the job that is already running. A proxy's retry becomes harmless.
//
// The store is shared between replicas because the poll, or the retry, may land
// on the other one.
package cmdjobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// States of a job.
const (
	StatePending = "pending"
	StateDone    = "done"
	StateFailed  = "failed"
)

// Retention is how long a finished job can still be read, and DedupWindow how
// long an identical command without a request id counts as a retry of the first
// rather than a new command. The window is longer than the edge's 65 s hang-up
// and shorter than anybody's patience for pressing the same button again.
const (
	Retention   = 2 * time.Hour
	DedupWindow = 90 * time.Second
)

// Job is one command and, once it has finished, its outcome.
type Job struct {
	RequestID  string          `json:"request_id"`
	BridgeID   string          `json:"bridge_id"`
	Cmd        string          `json:"cmd"`
	Via        string          `json:"via,omitempty"`
	State      string          `json:"state"`
	HTTPStatus int             `json:"http_status,omitempty"` // what a synchronous caller would have been given
	Response   json.RawMessage `json:"response,omitempty"`    // the command response, when done
	Error      string          `json:"error,omitempty"`
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
}

// KV is the little the package needs from a shared store.
type KV interface {
	// SetNX stores value under key only if the key is free. It reports whether
	// it did.
	SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Get(ctx context.Context, key string) ([]byte, bool, error)
}

// Store keeps jobs in a KV.
type Store struct {
	kv  KV
	now func() time.Time
}

// New returns a job store over kv.
func New(kv KV) *Store { return &Store{kv: kv, now: time.Now} }

func jobKey(tenantID, bridgeID, requestID string) string {
	return "cmdjob:" + tenantID + ":" + bridgeID + ":" + requestID
}

// Fingerprint identifies "the same command" when the caller gave no request id:
// same bridge, command, arguments and bearer.
func Fingerprint(bridgeID, cmd, via string, payload []byte) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{bridgeID, cmd, via, string(payload)}, "\x00")))
	return hex.EncodeToString(sum[:12])
}

// Begin claims a command. When the claim is won, the returned job is new and
// pending and the caller must run the command and call Finish. When it is lost,
// the returned job is the one already running (or finished) and the caller must
// NOT send anything.
//
// explicitID is the caller's request id; with one, the claim is on that id
// alone, for as long as the job is retained. Without one the claim is on the
// fingerprint for DedupWindow.
func (s *Store) Begin(ctx context.Context, tenantID string, job Job, explicitID bool, fingerprint string) (won bool, current *Job, err error) {
	job.State, job.StartedAt = StatePending, s.now().UTC()
	body, err := json.Marshal(job)
	if err != nil {
		return false, nil, err
	}
	key := jobKey(tenantID, job.BridgeID, job.RequestID)
	if explicitID {
		ok, err := s.kv.SetNX(ctx, key, body, Retention)
		if err != nil {
			return false, nil, err
		}
		if ok {
			return true, &job, nil
		}
		existing, err := s.Get(ctx, tenantID, job.BridgeID, job.RequestID)
		return false, existing, err
	}
	// No request id: the fingerprint points at whichever request id got there
	// first.
	ptr := "cmdjobfp:" + tenantID + ":" + fingerprint
	ok, err := s.kv.SetNX(ctx, ptr, []byte(job.RequestID), DedupWindow)
	if err != nil {
		return false, nil, err
	}
	if ok {
		if err := s.kv.Set(ctx, key, body, Retention); err != nil {
			return false, nil, err
		}
		return true, &job, nil
	}
	firstID, found, err := s.kv.Get(ctx, ptr)
	if err != nil || !found {
		return false, nil, errors.Join(errors.New("cmdjobs: a matching command is in flight but its id could not be read"), err)
	}
	existing, err := s.Get(ctx, tenantID, job.BridgeID, string(firstID))
	return false, existing, err
}

// Finish records the outcome of a job the caller won.
func (s *Store) Finish(ctx context.Context, tenantID string, job *Job, httpStatus int, response any, errText string) error {
	now := s.now().UTC()
	job.FinishedAt, job.HTTPStatus, job.Error = &now, httpStatus, errText
	job.State = StateDone
	if httpStatus >= 400 {
		job.State = StateFailed
	}
	if response != nil {
		raw, err := json.Marshal(response)
		if err != nil {
			return err
		}
		job.Response = raw
	}
	body, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return s.kv.Set(ctx, jobKey(tenantID, job.BridgeID, job.RequestID), body, Retention)
}

// Get returns a job, or nil when there is none (unknown id, another tenant's,
// or older than Retention).
func (s *Store) Get(ctx context.Context, tenantID, bridgeID, requestID string) (*Job, error) {
	raw, found, err := s.kv.Get(ctx, jobKey(tenantID, bridgeID, requestID))
	if err != nil || !found {
		return nil, err
	}
	var j Job
	if err := json.Unmarshal(raw, &j); err != nil {
		return nil, err
	}
	return &j, nil
}

// --- KV implementations ---

// MemoryKV is the single-process store, for a standalone Hub.
type MemoryKV struct {
	mu   sync.Mutex
	data map[string]memEntry
}

type memEntry struct {
	value   []byte
	expires time.Time
}

// NewMemoryKV returns an empty in-memory KV.
func NewMemoryKV() *MemoryKV { return &MemoryKV{data: map[string]memEntry{}} }

func (m *MemoryKV) live(key string) (memEntry, bool) {
	e, ok := m.data[key]
	if ok && time.Now().After(e.expires) {
		delete(m.data, key)
		return memEntry{}, false
	}
	return e, ok
}

// SetNX implements KV.
func (m *MemoryKV) SetNX(_ context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.live(key); ok {
		return false, nil
	}
	m.data[key] = memEntry{value: append([]byte(nil), value...), expires: time.Now().Add(ttl)}
	return true, nil
}

// Set implements KV.
func (m *MemoryKV) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = memEntry{value: append([]byte(nil), value...), expires: time.Now().Add(ttl)}
	return nil
}

// Get implements KV.
func (m *MemoryKV) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.live(key)
	return e.value, ok, nil
}

// RedisKV is the shared store, for a Hub with more than one replica.
type RedisKV struct{ Client *redis.Client }

// SetNX implements KV.
func (r RedisKV) SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	err := r.Client.SetArgs(ctx, key, value, redis.SetArgs{Mode: "NX", TTL: ttl}).Err()
	if errors.Is(err, redis.Nil) {
		return false, nil // the key was taken
	}
	return err == nil, err
}

// Set implements KV.
func (r RedisKV) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return r.Client.Set(ctx, key, value, ttl).Err()
}

// Get implements KV.
func (r RedisKV) Get(ctx context.Context, key string) ([]byte, bool, error) {
	v, err := r.Client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	return v, err == nil, err
}
