package satrelay

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/rock7"
)

const (
	imtModem = "300258060000001" // synthetic
	sbdModem = "300234060000002" // synthetic
	gsModem  = "0-1234567"       // a Globalstar id, synthetic
)

type rock7Call struct{ imei, dataHex string }

type fakeRock7 struct {
	calls []rock7Call
	err   error
}

func (f *fakeRock7) SendMT(_ context.Context, imei, dataHex string) (*rock7.SendResult, error) {
	f.calls = append(f.calls, rock7Call{imei, dataHex})
	if f.err != nil {
		return &rock7.SendResult{Error: f.err.Error()}, f.err
	}
	return &rock7.SendResult{OK: true, MTID: "4711"}, nil
}

type record struct {
	imei, mtID string
	wire       []byte
	err        error
}

type rig struct {
	s       *Sender
	imt     []string // "imei|wire|topic"
	r7      *fakeRock7
	records []record
	allow   bool
	noR7    bool
	lookErr error
}

func newRig() *rig {
	g := &rig{r7: &fakeRock7{}, allow: true}
	g.s = &Sender{
		IsIMT: func(_, imei string) bool { return imei == imtModem },
		SendIMT: func(imei, w, topic string) error {
			g.imt = append(g.imt, imei+"|"+w+"|"+topic)
			return nil
		},
		DeviceType: func(_ context.Context, _, imei string) (string, error) {
			if g.lookErr != nil {
				return "", g.lookErr
			}
			switch imei {
			case sbdModem:
				return "rockblock", nil
			case gsModem:
				return "globalstar", nil
			}
			return "", sql.ErrNoRows
		},
		Rock7: func(context.Context, string) MTSender {
			if g.noR7 {
				return nil
			}
			return g.r7
		},
		Allow: func(string, string) bool { return g.allow },
		Record: func(_ context.Context, _, imei string, wire []byte, mtID string, err error) {
			g.records = append(g.records, record{imei, mtID, wire, err})
		},
	}
	return g
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// imt_recipient_goes_through_cloudloop: an IMT modem gets the wire on the
// topic, and Rock7 is never touched.
func TestSend_IMTRecipientGoesThroughCloudloop(t *testing.T) {
	g := newRig()
	w := b64([]byte{0x01, 0xAA, 0xBB})
	if err := g.s.Send(context.Background(), "default", imtModem, w, ""); err != nil {
		t.Fatal(err)
	}
	if len(g.imt) != 1 || g.imt[0] != imtModem+"|"+w+"|" {
		t.Fatalf("imt sends %v", g.imt)
	}
	if len(g.r7.calls) != 0 || len(g.records) != 0 {
		t.Fatalf("rock7 touched: %v %v", g.r7.calls, g.records)
	}
}

// sbd_recipient_gets_the_bytes_through_rock7: a registered Rock7 modem gets
// exactly the bytes, hex encoded, once; the MT is recorded with Rock7's id.
func TestSend_SBDRecipientGetsTheBytesThroughRock7(t *testing.T) {
	g := newRig()
	wire := append([]byte{0x01}, bytes.Repeat([]byte{0x5A}, 40)...)
	if err := g.s.Send(context.Background(), "default", sbdModem, b64(wire), "IMT_TOPIC_RAW"); err != nil {
		t.Fatal(err)
	}
	if len(g.r7.calls) != 1 || g.r7.calls[0].imei != sbdModem || g.r7.calls[0].dataHex != hex.EncodeToString(wire) {
		t.Fatalf("rock7 calls %v", g.r7.calls)
	}
	if len(g.imt) != 0 {
		t.Fatalf("cloudloop touched: %v", g.imt)
	}
	if len(g.records) != 1 || g.records[0].mtID != "4711" || g.records[0].err != nil || !bytes.Equal(g.records[0].wire, wire) {
		t.Fatalf("records %+v", g.records)
	}
}

// sbd_mt_is_never_cut: 270 bytes go, 271 are refused and recorded as failed,
// and Rock7 is never called for them.
func TestSend_SBDMTIsNeverCut(t *testing.T) {
	g := newRig()
	if err := g.s.Send(context.Background(), "default", sbdModem, b64(make([]byte, SBDMTMax)), ""); err != nil {
		t.Fatalf("270 bytes refused: %v", err)
	}
	err := g.s.Send(context.Background(), "default", sbdModem, b64(make([]byte, SBDMTMax+1)), "")
	if err == nil || !strings.Contains(err.Error(), "271 bytes") {
		t.Fatalf("271 bytes: err = %v", err)
	}
	if len(g.r7.calls) != 1 {
		t.Fatalf("rock7 calls %d, want 1 (the 270-byte one)", len(g.r7.calls))
	}
	if len(g.records) != 2 || g.records[1].err == nil || g.records[1].mtID != "" {
		t.Fatalf("records %+v", g.records)
	}
}

// relay_refuses_what_it_cannot_reach: an unregistered modem, another kind of
// device, a failed lookup and a bad payload send nothing; a missing Rock7
// account, the rate limit and Rock7's own refusal are recorded as failed.
func TestSend_RefusesWhatItCannotReach(t *testing.T) {
	ok := b64([]byte{0x01, 0x02})
	for _, c := range []struct {
		name     string
		imei     string
		wire     string
		setup    func(*rig)
		recorded bool
	}{
		{"unregistered", "300234069999999", ok, nil, false},
		{"globalstar", gsModem, ok, nil, false},
		{"lookup failed", sbdModem, ok, func(g *rig) { g.lookErr = errors.New("db down") }, false},
		{"not base64", sbdModem, "%%%", nil, false},
		{"empty", sbdModem, "", nil, false},
		{"no rock7 account", sbdModem, ok, func(g *rig) { g.noR7 = true }, true},
		{"rate limited", sbdModem, ok, func(g *rig) { g.allow = false }, true},
		{"rock7 refused", sbdModem, ok, func(g *rig) { g.r7.err = errors.New("FAILED,10,Invalid login credentials") }, true},
	} {
		g := newRig()
		if c.setup != nil {
			c.setup(g)
		}
		if err := g.s.Send(context.Background(), "default", c.imei, c.wire, ""); err == nil {
			t.Errorf("%s: sent", c.name)
		}
		if len(g.imt) != 0 {
			t.Errorf("%s: cloudloop touched", c.name)
		}
		if c.name != "rock7 refused" && len(g.r7.calls) != 0 {
			t.Errorf("%s: rock7 called %v", c.name, g.r7.calls)
		}
		if got := len(g.records) == 1 && g.records[0].err != nil; got != c.recorded {
			t.Errorf("%s: recorded failure = %v (%+v)", c.name, got, g.records)
		}
	}
}
