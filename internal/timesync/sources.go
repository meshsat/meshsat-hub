// Package timesync source adapters for the Hub.
//
// The Hub does not have GPS or Iridium MSSTM hardware -- those sources exist
// only on the bridge. The Hub has:
//   - LocalNTPSource: reads system time (assumes the server has NTP sync, stratum 1)
//   - HubNTPSource: receives time readings from bridges via MQTT (stratum 2+)
//
// Ported from meshsat bridge internal/timesync/sources.go with bridge-only
// sources (GPS, MSSTM) removed and LocalNTPSource added.
package timesync

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"
)

// ---------- Local NTP Source (Stratum 1) ----------

// LocalNTPSource reads the system clock as an authoritative time source.
// On a properly NTP-synchronized server (which the Hub should be), this is
// stratum 1 -- the Hub IS the NTP authority for bridges.
type LocalNTPSource struct{}

// NewLocalNTPSource creates a local system clock time source.
func NewLocalNTPSource() *LocalNTPSource {
	return &LocalNTPSource{}
}

func (s *LocalNTPSource) Name() string { return "local_ntp" }
func (s *LocalNTPSource) Stratum() int { return 1 }

func (s *LocalNTPSource) Start(ctx context.Context, cb CorrectionCallback) {
	// On startup, immediately report zero offset (system clock is our reference).
	cb("local_ntp", 1, 0, 1_000_000) // 1ms uncertainty (NTP jitter)

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// The Hub system clock IS the reference -- offset is always 0.
			// This ensures the TimeService stays at stratum 1 even if no
			// other sources are available.
			cb("local_ntp", 1, 0, 1_000_000) // 1ms uncertainty

			log.Debug().Msg("timesync: local_ntp heartbeat")
		}
	}
}

// ---------- Hub NTP Source (Stratum 2) ----------

// HubNTPSource receives time corrections from bridges via MQTT.
// Bridges publish their GPS/MSSTM-derived time; the Hub can use these
// as a cross-check against its own NTP.
type HubNTPSource struct {
	// HubNTP is passive -- it receives readings via InjectReading().
	// Start() just keeps the goroutine alive for context cancellation.
	readings chan hubNTPReading
}

type hubNTPReading struct {
	unixNanos int64
	stratum   int
}

// NewHubNTPSource creates a Hub NTP time source adapter.
func NewHubNTPSource() *HubNTPSource {
	return &HubNTPSource{
		readings: make(chan hubNTPReading, 8),
	}
}

func (s *HubNTPSource) Name() string { return "hub_ntp" }
func (s *HubNTPSource) Stratum() int { return 2 }

func (s *HubNTPSource) Start(ctx context.Context, cb CorrectionCallback) {
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-s.readings:
			localNow := time.Now()
			offsetNs := r.unixNanos - localNow.UnixNano()
			// Hub<->bridge MQTT latency adds ~500ms uncertainty.
			cb("hub_ntp", r.stratum+1, offsetNs, 500_000_000)

			log.Debug().
				Float64("offset_ms", float64(offsetNs)/1e6).
				Msg("timesync: hub_ntp reading")
		}
	}
}

// InjectReading allows the MQTT handler to feed time readings from bridges.
func (s *HubNTPSource) InjectReading(unixNanos int64, stratum int) {
	select {
	case s.readings <- hubNTPReading{unixNanos: unixNanos, stratum: stratum}:
	default:
		// Channel full -- drop reading.
	}
}
