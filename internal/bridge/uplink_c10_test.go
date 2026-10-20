package bridge

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/sos"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// linkedStore is the uplink store with modems linked to bridges.
type linkedStore struct {
	fakeUplinkStore
	modems map[string]string // imei -> bridge
}

func (l *linkedStore) DeviceBridgeID(_ context.Context, _ string, imei string) (string, error) {
	return l.modems[imei], nil
}

// recRuns records what the sink applies; err, when set, is returned.
type recRuns struct {
	events   []sos.Event
	err      error
	unstored []sos.Event // PageUnstored calls
	pageOK   bool
}

func (r *recRuns) PageUnstored(_ context.Context, ev sos.Event) bool {
	r.unstored = append(r.unstored, ev)
	return r.pageOK
}

func (r *recRuns) Apply(_ context.Context, ev sos.Event) (store.SOSRunApplied, error) {
	r.events = append(r.events, ev)
	return store.SOSRunApplied{}, r.err
}

type c10Sink struct {
	sink *UplinkSink
	runs *recRuns
	pubs *[]pubRec
}

func newC10Sink(modems map[string]string, withRuns bool) c10Sink {
	var pubs []pubRec
	runs := &recRuns{}
	s := NewUplinkSink(&linkedStore{modems: modems}, func(topic string, _ byte, retained bool, v any) {
		b, _ := json.Marshal(v)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		pubs = append(pubs, pubRec{topic: topic, retained: retained, body: m})
	}, nil, "test")
	if withRuns {
		s.SetSOSRuns(runs)
	}
	return c10Sink{sink: s, runs: runs, pubs: &pubs}
}

func (c c10Sink) oldWayPages() int {
	n := 0
	for _, p := range *c.pubs {
		if strings.HasSuffix(p.topic, "/mo/decoded") && p.body["sos"] == true {
			n++
		}
	}
	return n
}

func fixtureFrame(t *testing.T, i int) []byte {
	t.Helper()
	b, err := hex.DecodeString(loadSOSFixture(t).Frames[i].Hex)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Frames of sos.json: 1 a v7 start, 2 its cancel, 3 a legacy start, 4 its cancel; all kit-a.
const (
	frameStartV7 = 1
	frameCancel7 = 2
	frameStartV5 = 3
	frameCancel5 = 4
)

// Over a satellite modem the run's bridge is the modem's: a start from kit-a's modem is
// applied as kit-a's, with the frame's id and run start (HeMB-NG C10).
func TestC10StartOverSatelliteIsTheModemsBridge(t *testing.T) {
	c := newC10Sink(map[string]string{"300434060000001": "kit-a"}, true)
	if !c.sink.Handle(context.Background(), "default", "imt", "300434060000001", fixtureFrame(t, frameStartV7)) {
		t.Fatal("not taken")
	}
	if len(c.runs.events) != 1 {
		t.Fatalf("applied %d events", len(c.runs.events))
	}
	ev := c.runs.events[0]
	if ev.BridgeID != "kit-a" || ev.Bearer != "imt" || ev.Kind != store.SOSEventStart || ev.RunStartMs != 1792330000123 ||
		ev.Text != "Injured, need help" || !ev.HasPosition {
		t.Errorf("event %+v", ev)
	}
	if n := c.oldWayPages(); n != 0 {
		t.Errorf("also paged the old way %d times", n)
	}
}

// A frame that names another bridge than its modem's, or comes from a modem of no bridge,
// changes no run, and its start still pages the old way: an SOS is never dropped.
func TestC10AFrameTheModemDoesNotVouchForPagesTheOldWay(t *testing.T) {
	for name, modems := range map[string]map[string]string{
		"another bridge's modem": {"300434060000001": "kit-b"},
		"a modem of no bridge":   {},
	} {
		c := newC10Sink(modems, true)
		c.sink.Handle(context.Background(), "default", "sbd", "300434060000001", fixtureFrame(t, frameStartV7))
		c.sink.Handle(context.Background(), "default", "sbd", "300434060000001", fixtureFrame(t, frameCancel7))
		if len(c.runs.events) != 0 {
			t.Errorf("%s: applied %+v", name, c.runs.events)
		}
		if n := c.oldWayPages(); n != 1 {
			t.Errorf("%s: %d old-way pages, want 1 (the start)", name, n)
		}
	}
}

// A start the run service refuses (an id that does not name its run) pages the old way.
func TestC10ARefusedStartPagesTheOldWay(t *testing.T) {
	c := newC10Sink(map[string]string{"300434060000001": "kit-a"}, true)
	c.runs.err = sos.ErrBadSOSID
	c.sink.Handle(context.Background(), "default", "imt", "300434060000001", fixtureFrame(t, frameStartV7))
	if n := c.oldWayPages(); n != 1 {
		t.Errorf("%d old-way pages, want 1", n)
	}
	// Without the run service the start pages the old way too.
	c = newC10Sink(map[string]string{"300434060000001": "kit-a"}, false)
	c.sink.Handle(context.Background(), "default", "imt", "300434060000001", fixtureFrame(t, frameStartV7))
	if n := c.oldWayPages(); n != 1 {
		t.Errorf("without runs: %d old-way pages, want 1", n)
	}
}

// Over SMS the run's bridge is the tenant's bridge the frame names, and only a v7 id may
// cancel: a legacy id is computable from the bridge id and the start.
func TestC10OverSMSOnlyARandomIDMayCancel(t *testing.T) {
	c := newC10Sink(nil, true)
	ctx := context.Background()
	c.sink.Handle(ctx, "default", "sms", "+31600000001", fixtureFrame(t, frameStartV5))
	c.sink.Handle(ctx, "default", "sms", "+31600000001", fixtureFrame(t, frameCancel5))
	c.sink.Handle(ctx, "default", "sms", "+31600000001", fixtureFrame(t, frameCancel7))
	if len(c.runs.events) != 2 {
		t.Fatalf("applied %d events, want the legacy start and the v7 cancel: %+v", len(c.runs.events), c.runs.events)
	}
	if e := c.runs.events[0]; e.Kind != store.SOSEventStart || e.BridgeID != "kit-a" || e.Bearer != "sms" {
		t.Errorf("start %+v", e)
	}
	if e := c.runs.events[1]; e.Kind != store.SOSEventCancel || e.BridgeID != "kit-a" || e.Reason != 0 {
		t.Errorf("cancel %+v", e)
	}
}

// A valid start the database did not take pages under its run's own id (PageUnstored), not
// the old way under a second identity; the old way only when even that fails (r16 C10-R2).
func TestC10AStartNotStoredPagesUnderItsRunsID(t *testing.T) {
	for _, pageOK := range []bool{true, false} {
		c := newC10Sink(map[string]string{"300434060000001": "kit-a"}, true)
		c.runs.err = errors.New("injected: database unavailable")
		c.runs.pageOK = pageOK
		c.sink.Handle(context.Background(), "default", "imt", "300434060000001", fixtureFrame(t, frameStartV7))
		if len(c.runs.unstored) != 1 || c.runs.unstored[0].SOSID != c.runs.events[0].SOSID {
			t.Fatalf("pageOK %v: PageUnstored calls %+v", pageOK, c.runs.unstored)
		}
		want := 0
		if !pageOK {
			want = 1 // the last resort: an SOS is never dropped
		}
		if n := c.oldWayPages(); n != want {
			t.Errorf("pageOK %v: %d old-way pages, want %d", pageOK, n, want)
		}
	}
	// A refused id is not a database failure: the old way, and no PageUnstored.
	c := newC10Sink(map[string]string{"300434060000001": "kit-a"}, true)
	c.runs.err = sos.ErrBadSOSID
	c.sink.Handle(context.Background(), "default", "imt", "300434060000001", fixtureFrame(t, frameStartV7))
	if len(c.runs.unstored) != 0 || c.oldWayPages() != 1 {
		t.Errorf("a refused id: %d PageUnstored, %d old-way pages", len(c.runs.unstored), c.oldWayPages())
	}
}
