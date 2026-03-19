// Package ipougrs implements an experimental IP-over-UGS (Unreliable Ground
// Station) tunnel adapter. It encapsulates IP packets into Iridium SBD frames,
// fragmenting across multiple MO/MT messages and reassembling on the other end.
//
// STATUS: EXPERIMENTAL / ALPHA — not for production use.
//
// The tunnel creates a Linux TUN device and bridges IP traffic bidirectionally
// through satellite SBD messages. Each IP packet is optionally compressed with
// DEFLATE, then fragmented into SBD-sized frames using the IPoUGRS frame
// protocol. On the receiving end, frames are reassembled and decompressed
// back into IP packets injected into the TUN device.
//
// IPoUGRS Frame Header (4 bytes):
//
//	Byte 0: Magic 0x49 ('I' — IPoUGRS identifier)
//	Byte 1: [4-bit frag_index | 4-bit frag_total (encoded as total-1)]
//	Byte 2: Packet ID (uint8, wrapping counter per tunnel endpoint)
//	Byte 3: Flags (bit 0: compressed with DEFLATE, bits 1-7: reserved)
//	Bytes 4+: payload (IP packet fragment, optionally compressed)
package ipougrs

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds IPoUGRS tunnel configuration.
type Config struct {
	// Enabled activates the tunnel. Default: false (experimental).
	Enabled bool `yaml:"enabled" json:"enabled"`

	// TunnelSubnet is the CIDR for the tunnel network.
	TunnelSubnet string `yaml:"tunnel_subnet" json:"tunnel_subnet"`

	// HubAddress is the Hub's IP within the tunnel subnet.
	HubAddress string `yaml:"hub_address" json:"hub_address"`

	// MTU is the IP-level MTU for the TUN device (not SBD MTU).
	MTU int `yaml:"mtu" json:"mtu"`

	// DeviceName is the TUN device name (e.g., "tun-ugrs0").
	DeviceName string `yaml:"device_name" json:"device_name"`

	// Compress enables DEFLATE compression of IP packets before framing.
	Compress bool `yaml:"compress" json:"compress"`

	// SBDMTU is the maximum SBD payload size per frame. Iridium MT = 270,
	// MO = 340. Uses the smaller value (MT) by default for bidirectional safety.
	SBDMTU int `yaml:"sbd_mtu" json:"sbd_mtu"`

	// FragTimeout is how long to wait for all fragments of a packet before
	// discarding incomplete reassemblies.
	FragTimeout time.Duration `yaml:"frag_timeout" json:"frag_timeout"`
}

// DefaultConfig returns a Config with sensible defaults.
// The tunnel is disabled by default — enable via HUB_IPOUGRS_ENABLED=true.
func DefaultConfig() Config {
	cfg := Config{
		Enabled:      false,
		TunnelSubnet: "10.99.0.0/24",
		HubAddress:   "10.99.0.1",
		MTU:          1400,
		DeviceName:   "tun-ugrs0",
		Compress:     true,
		SBDMTU:       270,
		FragTimeout:  2 * time.Minute,
	}

	// Environment variable overrides (HUB_IPOUGRS_ prefix).
	if v := os.Getenv("HUB_IPOUGRS_ENABLED"); v != "" {
		cfg.Enabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("HUB_IPOUGRS_TUNNEL_SUBNET"); v != "" {
		cfg.TunnelSubnet = v
	}
	if v := os.Getenv("HUB_IPOUGRS_HUB_ADDRESS"); v != "" {
		cfg.HubAddress = v
	}
	if v := os.Getenv("HUB_IPOUGRS_MTU"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MTU = n
		}
	}
	if v := os.Getenv("HUB_IPOUGRS_DEVICE_NAME"); v != "" {
		cfg.DeviceName = v
	}
	if v := os.Getenv("HUB_IPOUGRS_COMPRESSION"); v != "" {
		cfg.Compress = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("HUB_IPOUGRS_SBD_MTU"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.SBDMTU = n
		}
	}
	if v := os.Getenv("HUB_IPOUGRS_FRAG_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.FragTimeout = d
		}
	}

	return cfg
}
