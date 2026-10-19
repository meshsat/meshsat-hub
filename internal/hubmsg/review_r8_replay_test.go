package hubmsg

// The r8 review's reproduction of HM-R3, kept as a regression: a sender's repeat five seconds
// after admission is answered with every event, as one eleven seconds after is. On 32449415
// the first got none (the 10 s age rule). [MESHSAT-1584]

import (
	"context"
	"testing"
	"time"
)

func TestReviewR8ExplicitReplayWithinTenSeconds(t *testing.T) {
	st, b, c := newStore(t), &fakeBus{}, &clock{time.Now().UTC()}
	s := newService(st, b, c)
	sub := submission(t, c.t, "bridge:kit-b", "replay me")
	s.HandleOut(outA, mustJSON(t, sub))
	s.Drain(context.Background())
	b.take(inB)
	b.take(statusA)
	s.HandleAck(ackB, mustJSON(t, Ack{V: 1, OpID: sub.OpID, Mid: sub.Mid, From: "bridge:kit-a", Result: "stored"}))
	s.Drain(context.Background())
	b.take(statusA)
	// Sender missed its events and deliberately repeats five seconds after admission.
	op, err := st.GetMsgOp(context.Background(), "t-1", "bridge:kit-a", sub.OpID)
	if err != nil {
		t.Fatal(err)
	}
	c.t = op.CreatedAt.Add(5 * time.Second)
	s.HandleOut(outA, mustJSON(t, sub))
	s.Drain(context.Background())
	if n := len(b.take(statusA)); n != 2 {
		t.Errorf("explicit same-operation replay at +5s: got %d events want 2", n)
	}
	c.t = op.CreatedAt.Add(11 * time.Second)
	s.HandleOut(outA, mustJSON(t, sub))
	s.Drain(context.Background())
	if n := len(b.take(statusA)); n != 2 {
		t.Fatalf("+11s control: got %d events want 2", n)
	}
	t.Log("+11s replay control passes")
}
