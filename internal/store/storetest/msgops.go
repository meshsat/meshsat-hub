package storetest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// testMsgOps pins the Hub app route's store (HeMB-NG stage 2 C4, MESHSAT-1584) in both
// dialects: admission by (tenant, sender, op_id) with the sender's mid index, one stored row
// under racing admitters, events with consecutive revisions and a projection that sees every
// event, the outbox's due/lease/answer/expiry rules, the status outbox and its replay, and
// pruning. The default tenant is the victim tenant, as everywhere in this suite.
func testMsgOps(t *testing.T, s store.Store) {
	ctx := context.Background()
	t0 := time.Now().UTC().Truncate(time.Second)
	op := func(tenant, sender, opID, mid, digest string) *store.MsgOp {
		return &store.MsgOp{
			TenantID: tenant, Sender: sender, OpID: opID, Mid: mid, Recipient: "bridge:kit-b",
			ContentType: "text/plain;charset=utf-8", MidExpiresAt: t0.Add(2 * time.Hour).UnixMilli(),
			ExpiresAt: t0.Add(time.Hour).UnixMilli(), ContentSHA256: digest, Body: []byte("Water at the north gate"),
		}
	}
	admit := func(o *store.MsgOp) string {
		t.Helper()
		got, err := s.AdmitMsgOp(ctx, o)
		if err != nil {
			t.Fatalf("admit %s/%s/%s: %v", o.TenantID, o.Sender, o.OpID, err)
		}
		return got.Outcome
	}
	victim := store.DefaultTenantID

	// Admission.
	if got := admit(op(victim, "bridge:kit-a", "op-1", "mid-1", "d1")); got != store.MsgOpAdmitted {
		t.Fatalf("a new op: %s", got)
	}
	stored, err := s.GetMsgOp(ctx, victim, "bridge:kit-a", "op-1")
	if err != nil || stored.Mid != "mid-1" || string(stored.Body) != "Water at the north gate" || stored.State != "pending" ||
		stored.Recipient != "bridge:kit-b" || stored.ExpiresAt != t0.Add(time.Hour).UnixMilli() {
		t.Fatalf("stored op: %+v, %v", stored, err)
	}
	again, err := s.AdmitMsgOp(ctx, op(victim, "bridge:kit-a", "op-1", "mid-9", "d9"))
	if err != nil || again.Outcome != store.MsgOpExisting || again.Existing == nil || again.Existing.Mid != "mid-1" {
		t.Fatalf("the same key with other fields answers the stored row: %+v, %v", again, err)
	}
	if got := admit(op(victim, "bridge:kit-a", "op-2", "mid-1", "d1")); got != store.MsgOpAdmitted {
		t.Fatalf("a new op of a known mid with the same digest: %s", got)
	}
	if got := admit(op(victim, "bridge:kit-a", "op-3", "mid-1", "d2")); got != store.MsgOpMidConflict {
		t.Fatalf("a new op of a known mid with another digest: %s", got)
	}
	if _, err := s.GetMsgOp(ctx, victim, "bridge:kit-a", "op-3"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a mid conflict stored the op: %v", err)
	}
	if got := admit(op("msgops-other", "bridge:kit-a", "op-1", "mid-1", "d2")); got != store.MsgOpAdmitted {
		t.Fatalf("the same key in another tenant is another op: %s", got)
	}
	if got := admit(op(victim, "bridge:kit-c", "op-1", "mid-1", "d2")); got != store.MsgOpAdmitted {
		t.Fatalf("the same key from another sender is another op: %s", got)
	}
	// A refused repeat (op_conflict at the caller) left the mid index alone: mid-9 is free.
	if got := admit(op(victim, "bridge:kit-a", "op-4", "mid-9", "d4")); got != store.MsgOpAdmitted {
		t.Fatalf("the mid of a repeat that was answered from the stored row was kept: %s", got)
	}

	// Racing admitters store one row.
	var wg sync.WaitGroup
	outcomes := make([]string, 10)
	errs := make([]error, 10)
	for i := range outcomes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := s.AdmitMsgOp(ctx, op(victim, "bridge:kit-a", "op-race", "mid-race", "dr"))
			outcomes[i], errs[i] = got.Outcome, err
		}(i)
	}
	wg.Wait()
	admitted := 0
	for i := range outcomes {
		if errs[i] != nil {
			t.Fatalf("racing admit %d: %v", i, errs[i])
		}
		if outcomes[i] == store.MsgOpAdmitted {
			admitted++
		} else if outcomes[i] != store.MsgOpExisting {
			t.Fatalf("racing admit %d: %s", i, outcomes[i])
		}
	}
	if admitted != 1 {
		t.Fatalf("%d racing admitters stored the op, want 1", admitted)
	}

	// Events.
	var seen []int
	project := func(evs []store.MsgOpEvent) (string, bool) {
		seen = append(seen, len(evs))
		return evs[len(evs)-1].Class, len(evs) == 2
	}
	ev := func(id, class string) *store.MsgOpEvent {
		return &store.MsgOpEvent{TenantID: victim, Sender: "bridge:kit-a", OpID: "op-1", EventID: id, Class: class,
			Kind: "hub_forwarded", Authority: "hub", Scope: "the Hub", At: t0}
	}
	for i, id := range []string{"ev-1", "ev-2"} {
		e := ev(id, []string{"forwarded", "completed"}[i])
		added, err := s.AddMsgOpEvent(ctx, e, project)
		if err != nil || !added || e.Rev != i+1 {
			t.Fatalf("event %s: added=%v rev=%d err=%v", id, added, e.Rev, err)
		}
	}
	if added, err := s.AddMsgOpEvent(ctx, ev("ev-2", "failed"), project); err != nil || added {
		t.Fatalf("a repeated event id changed something: %v %v", added, err)
	}
	if fmt.Sprint(seen) != "[1 2]" {
		t.Fatalf("the projection saw %v events, want [1 2]", seen)
	}
	got, _ := s.GetMsgOp(ctx, victim, "bridge:kit-a", "op-1")
	if got.State != "completed" || !got.Conflict || got.Rev != 2 {
		t.Fatalf("the stored projection: %+v", got)
	}
	evs, err := s.ListMsgOpEvents(ctx, victim, "bridge:kit-a", "op-1")
	if err != nil || len(evs) != 2 || evs[0].EventID != "ev-1" || evs[1].Rev != 2 || !evs[0].At.Equal(t0) {
		t.Fatalf("events: %+v, %v", evs, err)
	}
	if _, err := s.AddMsgOpEvent(ctx, &store.MsgOpEvent{TenantID: victim, Sender: "bridge:kit-a", OpID: "op-none", EventID: "ev-x"},
		project); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an event of an unknown op: %v", err)
	}
	// Racing events of one op get consecutive revisions.
	errs = make([]error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := &store.MsgOpEvent{TenantID: victim, Sender: "bridge:kit-a", OpID: "op-race", EventID: fmt.Sprintf("ev-race-%d", i),
				Class: "forwarded", Kind: "hub_forwarded", Authority: "hub", At: t0}
			_, errs[i] = s.AddMsgOpEvent(ctx, e, func(evs []store.MsgOpEvent) (string, bool) { return "forwarded", false })
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("racing event %d: %v", i, err)
		}
	}
	evs, _ = s.ListMsgOpEvents(ctx, victim, "bridge:kit-a", "op-race")
	revs := []int{}
	for _, e := range evs {
		revs = append(revs, e.Rev)
	}
	sort.Ints(revs)
	if fmt.Sprint(revs) != "[1 2 3 4 5 6 7 8 9 10]" {
		t.Fatalf("racing events' revisions: %v", revs)
	}

	// The outbox. op-2 is pending and due.
	due := func(now time.Time) map[string]bool {
		t.Helper()
		ops, err := s.ListDueMsgOps(ctx, now, 100)
		if err != nil {
			t.Fatalf("due: %v", err)
		}
		m := map[string]bool{}
		for _, o := range ops {
			m[o.TenantID+"/"+o.Sender+"/"+o.OpID] = true
		}
		return m
	}
	key2 := victim + "/bridge:kit-a/op-2"
	now := time.Now().UTC().Add(time.Second)
	if !due(now)[key2] {
		t.Fatalf("a pending op is not due: %v", due(now))
	}
	if due(now)[victim+"/bridge:kit-a/op-1"] {
		t.Fatal("a completed op is due")
	}
	won := 0
	for i := 0; i < 5; i++ {
		if ok, err := s.ClaimMsgOp(ctx, victim, "bridge:kit-a", "op-2", now, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		} else if ok {
			won++
		}
	}
	if won != 1 || due(now)[key2] {
		t.Fatalf("claims won %d, due while leased %v", won, due(now)[key2])
	}
	// The lease is judged by the caller's clock, not the store's: still held a second before
	// it ends, free a second after (a drainer that died holding it).
	if ok, err := s.ClaimMsgOp(ctx, victim, "bridge:kit-a", "op-2", now.Add(59*time.Second), now.Add(2*time.Minute)); err != nil || ok {
		t.Fatalf("a held lease was taken: %v %v", ok, err)
	}
	if ok, err := s.ClaimMsgOp(ctx, victim, "bridge:kit-a", "op-2", now.Add(61*time.Second), now.Add(2*time.Minute)); err != nil || !ok {
		t.Fatalf("an ended lease was not taken: %v %v", ok, err)
	}
	if err := s.MarkMsgOpAttempt(ctx, victim, "bridge:kit-a", "op-2", now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if due(now)[key2] || !due(now.Add(31 * time.Second))[key2] {
		t.Fatal("an attempt's next time is not kept")
	}
	if g, _ := s.GetMsgOp(ctx, victim, "bridge:kit-a", "op-2"); g.Attempts != 1 {
		t.Fatalf("attempts: %d", g.Attempts)
	}
	if err := s.SetMsgOpRecipientAnswer(ctx, victim, "bridge:kit-a", "op-2", "stored"); err != nil {
		t.Fatal(err)
	}
	if due(now.Add(time.Hour))[key2] {
		t.Fatal("an op the recipient answered is still due")
	}
	// Past its deadline: not due, listed as expired until a final state.
	late := op(victim, "bridge:kit-a", "op-late", "mid-late", "dl")
	late.ExpiresAt = t0.Add(-time.Minute).UnixMilli()
	admit(late)
	if due(now)[victim+"/bridge:kit-a/op-late"] {
		t.Fatal("an op past its deadline is due")
	}
	exp, err := s.ListExpiredMsgOps(ctx, now, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, o := range exp {
		if o.OpID == "op-late" {
			found = true
		}
		if o.OpID == "op-1" {
			t.Fatal("a completed op is listed as expired")
		}
	}
	if !found {
		t.Fatal("an op past its deadline is not listed as expired")
	}

	// The senders' status outbox and its replay.
	unpub := func() map[string]bool {
		evs, err := s.ListUnpublishedMsgOpEvents(ctx, 1000)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]bool{}
		for _, e := range evs {
			m[e.EventID] = true
		}
		return m
	}
	if !unpub()["ev-1"] || !unpub()["ev-2"] {
		t.Fatal("new events are not unpublished")
	}
	if err := s.MarkMsgOpEventsPublished(ctx, []string{"ev-1", "ev-2"}, now); err != nil {
		t.Fatal(err)
	}
	if unpub()["ev-1"] || unpub()["ev-2"] {
		t.Fatal("published events are listed")
	}
	if n, err := s.ReplayMsgOpEvents(ctx, victim, "bridge:kit-a", "op-1"); err != nil || n != 2 || !unpub()["ev-1"] || !unpub()["ev-2"] {
		t.Fatalf("a replay: %d, %v", n, err)
	}

	// Pruning: what passed its deadline before the cutoff goes, with its events and mids.
	pruned, err := s.PruneMsgOps(ctx, t0)
	if err != nil || pruned != 1 {
		t.Fatalf("pruned %d, %v; want op-late", pruned, err)
	}
	if _, err := s.GetMsgOp(ctx, victim, "bridge:kit-a", "op-late"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a pruned op is still there: %v", err)
	}
	if g, err := s.GetMsgOp(ctx, victim, "bridge:kit-a", "op-1"); err != nil || g.Rev != 2 {
		t.Fatalf("a live op went with the prune: %v", err)
	}
}
