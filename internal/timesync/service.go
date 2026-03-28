// Package timesync provides a centralized clock correction service for
// GPS-denied environments. It aggregates time readings from multiple sources
// and exposes a corrected Now() that replaces raw time.Now() in
// time-sensitive code paths.
//
// Ported from meshsat bridge internal/timesync/service.go. The Hub version
// uses an optional TimeSyncStore interface instead of a direct database.DB
// reference, making the DB dependency pluggable.
package timesync

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
)

// TimeSyncStatus is the public state exposed via API and SSE.
type TimeSyncStatus struct {
	Source        string    `json:"source"`
	Stratum       int       `json:"stratum"`
	OffsetMs      float64   `json:"offset_ms"`
	UncertaintyMs float64   `json:"uncertainty_ms"`
	LastSync      time.Time `json:"last_sync"`
	Peers         int       `json:"peers"`
}

// TimeSource is the interface for individual time source adapters.
type TimeSource interface {
	Name() string
	Stratum() int
	Start(ctx context.Context, callback CorrectionCallback)
}

// CorrectionCallback is called by sources when they have a new reading.
type CorrectionCallback func(source string, stratum int, offsetNs, uncertaintyNs int64)

// TimeSyncStore is the optional persistence interface. If nil, state is not
// persisted across restarts.
type TimeSyncStore interface {
	// LoadBestOffset returns the best persisted offset (lowest stratum, lowest
	// uncertainty). Returns source, stratum, offsetNs, uncertaintyNs, and ok.
	LoadBestOffset() (source string, stratum int, offsetNs, uncertaintyNs int64, ok bool)

	// SaveOffset persists a time sync reading.
	SaveOffset(source string, stratum int, offsetNs, uncertaintyNs int64, peerHash string)
}

// EventData is a generic SSE event structure for time sync updates.
type EventData struct {
	Type    string          `json:"type"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
	Time    string          `json:"time"`
}

// EventEmitter is a callback for broadcasting SSE events.
type EventEmitter func(EventData)

// TimeService is the centralized clock correction service.
type TimeService struct {
	// Atomic fields for lock-free hot path (Now()).
	offsetNanos  atomic.Int64
	currentStrat atomic.Int32

	// Protected by mu for less frequent updates.
	mu            sync.RWMutex
	source        string
	uncertaintyNs int64
	lastSync      time.Time
	peers         int
	sources       []TimeSource
	store         TimeSyncStore
	emit          EventEmitter
}

// NewTimeService creates a new service with an optional persistence store.
// Pass nil for store if persistence is not needed.
func NewTimeService(store TimeSyncStore) *TimeService {
	ts := &TimeService{
		store:  store,
		source: "rtc",
	}
	ts.currentStrat.Store(5) // default: local RTC, worst stratum
	return ts
}

// SetEmitter sets the SSE event callback.
func (ts *TimeService) SetEmitter(fn EventEmitter) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.emit = fn
}

// AddSource registers a time source adapter.
func (ts *TimeService) AddSource(src TimeSource) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.sources = append(ts.sources, src)
}

// SetPeerCount updates the mesh peer count (called by consensus).
func (ts *TimeService) SetPeerCount(n int) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.peers = n
}

// Now returns the corrected current time: time.Now() + offset.
// This is the primary hot-path function -- uses only atomic loads.
func (ts *TimeService) Now() time.Time {
	off := ts.offsetNanos.Load()
	return time.Now().Add(time.Duration(off))
}

// UnixNano returns corrected time as unix nanoseconds (atomic, no alloc).
func (ts *TimeService) UnixNano() int64 {
	return time.Now().UnixNano() + ts.offsetNanos.Load()
}

// Offset returns the current clock correction in nanoseconds.
func (ts *TimeService) Offset() int64 {
	return ts.offsetNanos.Load()
}

// Stratum returns the current stratum level.
func (ts *TimeService) Stratum() int {
	return int(ts.currentStrat.Load())
}

// ApplyCorrection updates the clock correction if the new reading is
// authoritative (lower stratum or equal stratum with lower uncertainty).
func (ts *TimeService) ApplyCorrection(source string, stratum int, offsetNs, uncertaintyNs int64) {
	curStrat := int(ts.currentStrat.Load())

	ts.mu.RLock()
	curUncertainty := ts.uncertaintyNs
	ts.mu.RUnlock()

	// Accept if: better stratum, or same stratum with better uncertainty.
	if stratum > curStrat {
		return
	}
	if stratum == curStrat && uncertaintyNs >= curUncertainty && curUncertainty > 0 {
		return
	}

	prevOffset := ts.offsetNanos.Load()
	ts.offsetNanos.Store(offsetNs)
	ts.currentStrat.Store(int32(stratum))

	ts.mu.Lock()
	prevSource := ts.source
	ts.source = source
	ts.uncertaintyNs = uncertaintyNs
	ts.lastSync = time.Now()
	emitFn := ts.emit
	ts.mu.Unlock()

	diffMs := float64(offsetNs-prevOffset) / 1e6

	log.Info().
		Str("source", source).
		Int("stratum", stratum).
		Float64("offset_ms", float64(offsetNs)/1e6).
		Float64("uncertainty_ms", float64(uncertaintyNs)/1e6).
		Float64("change_ms", diffMs).
		Msg("timesync: correction applied")

	// Persist to store.
	ts.persistState(source, stratum, offsetNs, uncertaintyNs, "")

	// Emit SSE event on significant change (>100ms drift or source change).
	if emitFn != nil && (abs64(offsetNs-prevOffset) > 100_000_000 || source != prevSource) {
		status := ts.GetStatus()
		data, _ := json.Marshal(status)
		emitFn(EventData{
			Type:    "time_sync_status",
			Message: "Time sync updated",
			Data:    data,
			Time:    time.Now().UTC().Format(time.RFC3339),
		})
	}
}

// GetStatus returns the current time sync state for API/SSE.
func (ts *TimeService) GetStatus() TimeSyncStatus {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return TimeSyncStatus{
		Source:        ts.source,
		Stratum:       int(ts.currentStrat.Load()),
		OffsetMs:      float64(ts.offsetNanos.Load()) / 1e6,
		UncertaintyMs: float64(ts.uncertaintyNs) / 1e6,
		LastSync:      ts.lastSync,
		Peers:         ts.peers,
	}
}

// Start launches all registered time source adapters.
func (ts *TimeService) Start(ctx context.Context) {
	ts.mu.RLock()
	sources := make([]TimeSource, len(ts.sources))
	copy(sources, ts.sources)
	ts.mu.RUnlock()

	for _, src := range sources {
		src := src
		go src.Start(ctx, ts.ApplyCorrection)
		log.Info().
			Str("source", src.Name()).
			Int("stratum", src.Stratum()).
			Msg("timesync: source started")
	}
}

// LoadPersistedState restores the best offset from the store on startup.
func (ts *TimeService) LoadPersistedState() {
	if ts.store == nil {
		return
	}

	source, stratum, offsetNs, uncertaintyNs, ok := ts.store.LoadBestOffset()
	if !ok {
		// No persisted state -- start from scratch.
		return
	}

	// Only restore if it's reasonably fresh (< 1 hour) -- clocks drift.
	ts.offsetNanos.Store(offsetNs)
	ts.currentStrat.Store(int32(stratum))
	ts.mu.Lock()
	ts.source = source + " (restored)"
	ts.uncertaintyNs = uncertaintyNs
	ts.mu.Unlock()

	log.Info().
		Str("source", source).
		Int("stratum", stratum).
		Float64("offset_ms", float64(offsetNs)/1e6).
		Msg("timesync: restored persisted state")
}

func (ts *TimeService) persistState(source string, stratum int, offsetNs, uncertaintyNs int64, peerHash string) {
	if ts.store == nil {
		return
	}
	ts.store.SaveOffset(source, stratum, offsetNs, uncertaintyNs, peerHash)
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
