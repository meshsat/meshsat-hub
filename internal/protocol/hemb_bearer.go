package protocol

// HeMB bearer profiles — ported from meshsat/internal/hemb/bearer.go and profiles.go.
// Hub-side subset: no SendFn (Hub only receives, never sends coded symbols).
// Used for redundancy estimation, cost tracking, and health display.

import "math"

// HeMBBearerProfile describes a physical transport channel reported by a bridge.
// Hub uses these to understand the bearer set behind a bonded stream — cost
// tracking, redundancy estimation, and dashboard display. Hub never calls SendFn.
type HeMBBearerProfile struct {
	Index       uint8   // position in the bearer set (0-based)
	InterfaceID string  // e.g. "mesh_0", "iridium_0"
	ChannelType string  // e.g. "mesh", "iridium_sbd", "sms"
	MTU         int     // effective payload bytes after HeMB header
	CostPerMsg  float64 // $0.00 = free, $0.05 = SBD
	LossRate    float64 // estimated 0.0-1.0
	LatencyMs   int     // median one-way latency in ms
	HealthScore int     // 0-100
	HeaderMode  string  // "compact", "extended", "implicit"
}

// IsFree returns true if this bearer has zero per-message cost.
func (b *HeMBBearerProfile) IsFree() bool { return b.CostPerMsg == 0 }

// Per-bearer RLNC redundancy factors. Determines how many extra coded
// symbols are generated for each bearer to tolerate its expected loss rate.
var hembDefaultRedundancy = map[string]float64{
	"mesh":        1.30, // LoRa: bursty fading, 10-40% loss
	"iridium_sbd": 1.00, // SBD: reliable but expensive
	"iridium_imt": 1.00, // IMT: reliable, large MTU
	"astrocast":   1.40, // LEO uplink: high loss, constrained MTU
	"cellular":    1.10, // SMS: moderate reliability
	"sms":         1.10, // alias
	"zigbee":      1.30, // short-range ISM: interference
	"aprs":        1.30, // AX.25: very lossy shared channel
	"ipougrs":     2.00, // GSM ring: extreme loss (~30%), micro-MTU
	"tcp":         1.00, // reliable transport
	"mqtt":        1.00, // reliable transport
	"webhook":     1.00, // reliable transport
}

// HeMBSelectRedundancy computes the RLNC redundancy factor for a bearer set.
// Returns R >= 1.0 where N = ceil(K * R).
func HeMBSelectRedundancy(bearers []HeMBBearerProfile, priority int) float64 {
	if len(bearers) == 0 {
		return 1.0
	}

	var totalWeight, weightedLoss float64
	for _, b := range bearers {
		w := float64(b.MTU)
		totalWeight += w
		weightedLoss += w * b.LossRate
	}
	avgLoss := weightedLoss / totalWeight

	var baseR float64
	switch {
	case avgLoss < 0.05:
		baseR = 1.10
	case avgLoss < 0.15:
		baseR = 1.25
	case avgLoss < 0.30:
		baseR = 1.40
	default:
		baseR = 1.60
	}

	switch priority {
	case 0:
		baseR *= 1.30
	case 1:
		baseR *= 1.10
	}

	paidCount := 0
	for _, b := range bearers {
		if !b.IsFree() {
			paidCount++
		}
	}
	if paidFrac := float64(paidCount) / float64(len(bearers)); paidFrac > 0.5 {
		baseR = math.Max(baseR*0.85, 1.05)
	}

	return math.Min(baseR, 2.0)
}

// HeMBBearerRedundancy returns the per-bearer RLNC redundancy factor.
func HeMBBearerRedundancy(b *HeMBBearerProfile) float64 {
	if r, ok := hembDefaultRedundancy[b.ChannelType]; ok {
		if !b.IsFree() {
			return math.Min(r, 1.10)
		}
		return r
	}
	if b.IsFree() {
		return 1.30
	}
	return 1.05
}

// HeMBRepairSymbols calculates how many repair symbols to add for a bearer.
func HeMBRepairSymbols(b *HeMBBearerProfile, sourceCount int) int {
	if sourceCount == 0 {
		return 0
	}
	repair := int(math.Ceil(float64(sourceCount) * b.LossRate * 1.5))
	if !b.IsFree() {
		if repair > 1 {
			repair = 1
		}
	}
	return repair
}
