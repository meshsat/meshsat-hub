package satrelay

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

type recStore struct {
	msgs  []*store.Message
	costs []*store.CostEntry
}

func (s *recStore) InsertMessage(_ context.Context, _ string, m *store.Message) error {
	s.msgs = append(s.msgs, m)
	return nil
}

func (s *recStore) InsertCostEntry(_ context.Context, _ string, c *store.CostEntry) error {
	s.costs = append(s.costs, c)
	return nil
}

type recRig struct {
	r      *Recorder
	st     *recStore
	audits []string
	pubs   []MTStatus
}

func newRecRig(cost float64) *recRig {
	g := &recRig{st: &recStore{}}
	g.r = &Recorder{
		Messages: g.st, Costs: g.st, CostPerMsg: cost,
		Audit: func(_ context.Context, _, action, detail string) error {
			g.audits = append(g.audits, action+" "+detail)
			return nil
		},
		Publish: func(_, _ string, v any) error {
			g.pubs = append(g.pubs, v.(MTStatus))
			return nil
		},
		Now: func() time.Time { return time.Date(2026, 10, 24, 2, 30, 0, 0, time.UTC) },
	}
	return g
}

// sbd_mt_is_recorded_as_cloudloop_does: a taken MT is stored, audited with its
// id and byte count, costed, and published queued under Rock7's id.
func TestRecorder_SBDMTIsRecordedAsCloudloopDoes(t *testing.T) {
	g := newRecRig(0.05)
	wire := []byte{0x01, 0xDE, 0xAD}
	g.r.Record(context.Background(), "default", sbdModem, wire, "4711", nil)
	if len(g.st.msgs) != 1 {
		t.Fatalf("messages %d", len(g.st.msgs))
	}
	m := g.st.msgs[0]
	if m.ID != "mt-4711" || m.Direction != "mt" || m.Channel != "iridium" || m.RawHex != "01dead" || m.Status != "queued" || m.DeviceIMEI != sbdModem {
		t.Errorf("stored %+v", m)
	}
	if len(g.audits) != 1 || !strings.HasPrefix(g.audits[0], "message_sent ") || !strings.Contains(g.audits[0]+" ", "mt_id=4711 ") ||
		!strings.Contains(g.audits[0], "bytes=3") || !strings.Contains(g.audits[0], "provider=rock7") {
		t.Errorf("audit %v", g.audits)
	}
	if len(g.st.costs) != 1 || g.st.costs[0].InterfaceType != "iridium_sbd" || g.st.costs[0].Direction != "mt" ||
		g.st.costs[0].MessageID != "4711" || g.st.costs[0].CostUSD != 0.05 {
		t.Errorf("costs %+v", g.st.costs)
	}
	if len(g.pubs) != 1 || g.pubs[0].ID != "4711" || g.pubs[0].Status != "queued" || g.pubs[0].Channel != "iridium" {
		t.Errorf("published %+v", g.pubs)
	}
}

// sbd_mt_failure_is_recorded_but_not_billed: a refused MT is stored and
// published failed with the reason, and never audited as sent or costed.
func TestRecorder_SBDMTFailureIsRecordedButNotBilled(t *testing.T) {
	g := newRecRig(0.05)
	g.r.Record(context.Background(), "default", sbdModem, []byte{0x01}, "", errors.New("over the 270 byte MT frame"))
	if len(g.st.msgs) != 1 || g.st.msgs[0].Status != "failed" || !strings.HasPrefix(g.st.msgs[0].ID, "mt-relay-") ||
		!strings.Contains(g.st.msgs[0].Error, "270") {
		t.Errorf("stored %+v", g.st.msgs)
	}
	if len(g.audits) != 0 || len(g.st.costs) != 0 {
		t.Errorf("billed a failure: audits %v costs %v", g.audits, g.st.costs)
	}
	if len(g.pubs) != 1 || g.pubs[0].Status != "failed" || g.pubs[0].Error == "" {
		t.Errorf("published %+v", g.pubs)
	}
}

// no_cost_row_without_a_price: CostPerMsg 0 records no cost, as Cloudloop.
func TestRecorder_NoCostRowWithoutAPrice(t *testing.T) {
	g := newRecRig(0)
	g.r.Record(context.Background(), "default", sbdModem, []byte{0x01}, "9", nil)
	if len(g.st.costs) != 0 || len(g.audits) != 1 {
		t.Errorf("costs %v audits %v", g.st.costs, g.audits)
	}
}
