package oob

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// Command codes. See spec section 6.
const (
	CmdPing      byte = 0x01
	CmdReboot    byte = 0x02
	CmdRestart   byte = 0x03
	CmdReset     byte = 0x04
	CmdBearer    byte = 0x05
	CmdLog       byte = 0x06
	CmdStatusNet byte = 0x07
)

// Command describes one allowlisted command.
type Command struct {
	Code byte   `json:"code"`
	Name string `json:"name"`
}

// Commands is the fixed allowlist.
var Commands = []Command{
	{CmdPing, "PING"}, {CmdReboot, "REBOOT"}, {CmdRestart, "RESTART"}, {CmdReset, "RESET"},
	{CmdBearer, "BEARER"}, {CmdLog, "LOG"}, {CmdStatusNet, "STATUS-NET"},
}

// hubNames maps the Hub's MQTT command names onto OOB commands, the same
// table the bridge's MQTT command handler uses.
var hubNames = map[string]byte{
	"ping": CmdPing, "mgmt_ping": CmdPing,
	"mgmt_status": CmdStatusNet, "status": CmdStatusNet, "status_net": CmdStatusNet, "status-net": CmdStatusNet,
	"mgmt_log": CmdLog, "log": CmdLog,
	"mgmt_reset": CmdReset, "reset": CmdReset,
	"mgmt_bearer": CmdBearer, "bearer": CmdBearer,
	"mgmt_restart": CmdRestart, "restart": CmdRestart,
	"reboot": CmdReboot, "mgmt_reboot": CmdReboot,
}

// CommandByName resolves an OOB name ("STATUS-NET") or a Hub MQTT command
// name ("mgmt_status", "reboot").
func CommandByName(name string) (Command, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	if code, ok := hubNames[n]; ok {
		return CommandByCode(code)
	}
	u := strings.ToUpper(strings.ReplaceAll(n, "_", "-"))
	for _, c := range Commands {
		if c.Name == u {
			return c, true
		}
	}
	return Command{}, false
}

// CommandByCode looks a command up by wire code.
func CommandByCode(code byte) (Command, bool) {
	for _, c := range Commands {
		if c.Code == code {
			return c, true
		}
	}
	return Command{}, false
}

// ResultCode is the first byte of every reply's args.
type ResultCode byte

// Result codes. See spec section 6.
const (
	RCOK           ResultCode = 0
	RCDenied       ResultCode = 1
	RCBadArgs      ResultCode = 2
	RCUnavailable  ResultCode = 3
	RCRefused      ResultCode = 4
	RCAgentError   ResultCode = 5
	RCUnknownCmd   ResultCode = 6
	RCBusy         ResultCode = 7
	RCKeyExhausted ResultCode = 8
)

func (rc ResultCode) String() string {
	switch rc {
	case RCOK:
		return "ok"
	case RCDenied:
		return "denied"
	case RCBadArgs:
		return "bad_args"
	case RCUnavailable:
		return "unavailable"
	case RCRefused:
		return "refused"
	case RCAgentError:
		return "agent_error"
	case RCUnknownCmd:
		return "unknown_cmd"
	case RCBusy:
		return "busy"
	case RCKeyExhausted:
		return "key_exhausted"
	}
	return fmt.Sprintf("rc%d", byte(rc))
}

// Argument limits.
const (
	DefaultRebootDelay = 10
	MaxRebootDelay     = 3600
	DefaultLogLines    = 10
	MaxLogLines        = 20
	MinLevel           = 1
	MaxLevel           = 3
)

var errArgs = errors.New("oob: bad args")

// EncodeRebootArgs encodes a REBOOT delay in seconds.
func EncodeRebootArgs(delay uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, delay)
	return b
}

// EncodeResetArgs encodes RESET target and level.
func EncodeResetArgs(target, level byte) []byte { return []byte{target, level} }

// EncodeBearerArgs encodes BEARER target and state.
func EncodeBearerArgs(target, state byte) []byte { return []byte{target, state} }

// EncodeLogArgs encodes LOG unit and line count.
func EncodeLogArgs(unit, lines byte) []byte { return []byte{unit, lines} }

// ReplyHeaderLen is the fixed prefix of every reply's args.
const ReplyHeaderLen = 5

// ReplyArgs is the decoded args of a reply frame.
type ReplyArgs struct {
	RC           ResultCode
	ReqCounterLo uint16
	Seq          byte
	Total        byte
	Body         []byte
}

// EncodeReplyArgs lays out [rc:1][req_counter_lo16:2][seq:1][total:1][body].
func EncodeReplyArgs(r ReplyArgs) []byte {
	out := make([]byte, ReplyHeaderLen, ReplyHeaderLen+len(r.Body))
	out[0] = byte(r.RC)
	binary.BigEndian.PutUint16(out[1:3], r.ReqCounterLo)
	out[3] = r.Seq
	out[4] = r.Total
	return append(out, r.Body...)
}

// ParseReplyArgs decodes a reply's args.
func ParseReplyArgs(args []byte) (ReplyArgs, error) {
	if len(args) < ReplyHeaderLen {
		return ReplyArgs{}, errArgs
	}
	return ReplyArgs{RC: ResultCode(args[0]), ReqCounterLo: binary.BigEndian.Uint16(args[1:3]), Seq: args[3], Total: args[4], Body: append([]byte{}, args[ReplyHeaderLen:]...)}, nil
}

// Target is a RESET/BEARER target (spec section 7); IfaceID is set for the
// bearer targets only.
type Target struct {
	Code    byte
	Name    string
	IfaceID string
}

// Targets mirrors the bridge's table.
var Targets = []Target{
	{0x01, "wifi", ""}, {0x02, "usb_wifi", ""}, {0x03, "cellular", "cellular_0"}, {0x04, "mesh", "mesh_0"},
	{0x05, "iridium", "iridium_0"}, {0x06, "imt", "iridium_imt_0"}, {0x07, "zigbee", "zigbee_0"}, {0x08, "ble", "ble_0"},
	{0x09, "aprs", "aprs_0"}, {0x0A, "gps", ""}, {0x0B, "rtl_sdr", ""}, {0x7E, "bridge", ""}, {0x7F, "host", ""},
}

// TargetByName resolves a target name.
func TargetByName(name string) (Target, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, t := range Targets {
		if t.Name == n {
			return t, true
		}
	}
	return Target{}, false
}

// LogUnits mirrors the bridge's journal allowlist, indexed by the unit byte.
var LogUnits = []string{
	"meshsat-oob-agent", "docker", "netplan-wpa-wlan0", "systemd-networkd",
	"x1202-monitor", "meshsat-mgmt-keepalive", "meshsat-p2p-link", "bluetooth",
}

// LogUnitIndex resolves a unit name to its byte.
func LogUnitIndex(name string) (byte, bool) {
	for i, u := range LogUnits {
		if u == name {
			return byte(i), true
		}
	}
	return 0, false
}

// ArgSpec is the operator-facing form of command arguments (the JSON payload
// of POST /api/bridges/{id}/command for mgmt_* commands).
type ArgSpec struct {
	Delay  int    `json:"delay,omitempty"`
	Target string `json:"target,omitempty"`
	Level  int    `json:"level,omitempty"`
	State  string `json:"state,omitempty"`
	Unit   string `json:"unit,omitempty"`
	Lines  int    `json:"lines,omitempty"`
}

// BuildArgs encodes an ArgSpec for a command.
func BuildArgs(cmd byte, a ArgSpec) ([]byte, error) {
	switch cmd {
	case CmdPing, CmdRestart, CmdStatusNet:
		return nil, nil
	case CmdReboot:
		d := a.Delay
		if d <= 0 {
			d = DefaultRebootDelay
		}
		d = min(d, MaxRebootDelay)
		return EncodeRebootArgs(uint16(d)), nil
	case CmdReset:
		t, ok := TargetByName(a.Target)
		if !ok {
			return nil, fmt.Errorf("oob: unknown target %q", a.Target)
		}
		level := a.Level
		if level == 0 {
			level = MinLevel
		}
		if level < MinLevel || level > MaxLevel {
			return nil, fmt.Errorf("oob: level must be %d..%d", MinLevel, MaxLevel)
		}
		return EncodeResetArgs(t.Code, byte(level)), nil
	case CmdBearer:
		t, ok := TargetByName(a.Target)
		if !ok || t.IfaceID == "" {
			return nil, fmt.Errorf("oob: %q is not a bearer target", a.Target)
		}
		switch strings.ToLower(strings.TrimSpace(a.State)) {
		case "on", "1", "true", "up":
			return EncodeBearerArgs(t.Code, 1), nil
		case "off", "0", "false", "down":
			return EncodeBearerArgs(t.Code, 0), nil
		}
		return nil, errors.New("oob: state must be on or off")
	case CmdLog:
		idx, ok := LogUnitIndex(strings.TrimSpace(a.Unit))
		if !ok {
			return nil, fmt.Errorf("oob: unknown log unit %q", a.Unit)
		}
		lines := a.Lines
		if lines <= 0 {
			lines = DefaultLogLines
		}
		lines = min(lines, MaxLogLines)
		return EncodeLogArgs(idx, byte(lines)), nil
	}
	return nil, errors.New("oob: unknown command")
}
