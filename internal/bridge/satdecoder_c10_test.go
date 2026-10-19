package bridge

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/hubmsg"
)

// sosFixture is docs/fixtures/v1/sos.json (release fixtures-v1.1), C10's shared cases.
type sosFixture struct {
	Frames []struct {
		Case    string         `json:"case"`
		Type    string         `json:"type"`
		Hex     string         `json:"hex"`
		Valid   *bool          `json:"valid"`
		Decoded map[string]any `json:"decoded"`
	} `json:"frames"`
}

func loadSOSFixture(t *testing.T) sosFixture {
	t.Helper()
	b, err := os.ReadFile("../../docs/fixtures/v1/sos.json")
	if err != nil {
		t.Fatal(err)
	}
	var f sosFixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// Every C10 frame of the shared fixture decodes to its fields, and the sos_id rule gives the
// fixture's verdict against the bridge the frame names (HeMB-NG C10, MESHSAT-1584).
func TestC10FramesDecodeAsTheFixtureSays(t *testing.T) {
	f := loadSOSFixture(t)
	seen := map[string]int{}
	for _, c := range f.Frames {
		raw, err := hex.DecodeString(c.Hex)
		if err != nil {
			t.Fatalf("%s: %v", c.Case, err)
		}
		if !IsBridgeSatUplink(raw) {
			t.Fatalf("%s: not taken as an uplink frame", c.Case)
		}
		typ, payload, err := DecodeSatUplink(raw)
		if err != nil {
			t.Fatalf("%s: %v", c.Case, err)
		}
		seen[c.Type]++
		var bridgeID, sosID string
		var runMs int64
		switch typ {
		case SatMsgSOS:
			if _, _, _, _, _, _, err := DecodeSatSOS(payload); err != nil {
				t.Errorf("%s: %v", c.Case, err)
			}
			continue
		case SatMsgSOSStart:
			s, err := DecodeSatSOSStart(payload)
			if err != nil {
				t.Fatalf("%s: %v", c.Case, err)
			}
			bridgeID, sosID, runMs = s.BridgeID, hubmsg.UUIDText(s.SOSID), s.RunStartMs
			if s.DeviceID != c.Decoded["device_id"] || s.Message != c.Decoded["text"] {
				t.Errorf("%s: device %q text %q, want %v %v", c.Case, s.DeviceID, s.Message, c.Decoded["device_id"], c.Decoded["text"])
			}
		case SatMsgSOSCancel:
			s, err := DecodeSatSOSCancel(payload)
			if err != nil {
				t.Fatalf("%s: %v", c.Case, err)
			}
			bridgeID, sosID, runMs = s.BridgeID, hubmsg.UUIDText(s.SOSID), s.RunStartMs
			if float64(s.Reason) != c.Decoded["reason"] {
				t.Errorf("%s: reason %d, want %v", c.Case, s.Reason, c.Decoded["reason"])
			}
		default:
			t.Fatalf("%s: type 0x%02x", c.Case, typ)
		}
		if bridgeID != c.Decoded["bridge_id"] || sosID != c.Decoded["sos_id"] || float64(runMs) != c.Decoded["run_start_ms"] {
			t.Errorf("%s: decoded %s %s %d, want %v", c.Case, bridgeID, sosID, runMs, c.Decoded)
		}
		if c.Valid == nil {
			t.Fatalf("%s: the fixture gives no verdict", c.Case)
		}
		if got := hubmsg.SOSIDValid(sosID, bridgeID, runMs); got != *c.Valid {
			t.Errorf("%s: valid = %v, want %v", c.Case, got, *c.Valid)
		}
	}
	if seen["0x02"] != 1 || seen["0x07"] != 4 || seen["0x08"] != 3 {
		t.Errorf("frames seen %v, want one 0x02, four 0x07, three 0x08", seen)
	}
}

// A C10 frame is exactly its layout: one byte short or one byte over is refused, and a
// run_start_ms past int64 is not taken for a negative time.
func TestC10FramesAreExactlyTheirLayout(t *testing.T) {
	f := loadSOSFixture(t)
	for _, c := range f.Frames {
		if c.Type == "0x02" {
			continue
		}
		raw, _ := hex.DecodeString(c.Hex)
		decode := DecodeSatSOSStart
		cancel := c.Type == "0x08"
		try := func(p []byte) error {
			if cancel {
				_, err := DecodeSatSOSCancel(p)
				return err
			}
			_, err := decode(p)
			return err
		}
		body := raw[satHeaderLen:]
		if err := try(body[:len(body)-1]); !errors.Is(err, ErrSatTruncated) {
			t.Errorf("%s: one byte short: %v", c.Case, err)
		}
		if err := try(append(append([]byte{}, body...), 0)); !errors.Is(err, ErrSatTrailing) {
			t.Errorf("%s: one byte over: %v", c.Case, err)
		}
	}
	// run_start_ms with its top bit set.
	raw, _ := hex.DecodeString(f.Frames[2].Hex) // a cancel under sos_v2
	body := append([]byte{}, raw[satHeaderLen:]...)
	n := int(body[0]) + 1
	body[n+16] |= 0x80
	if _, err := DecodeSatSOSCancel(body); err == nil {
		t.Error("a run_start_ms past int64 was taken")
	}
}
