package booth

import (
	"context"
	"strings"
	"testing"
)

// MESHSAT-1181. The kit-online gate is a cellular fact; these cover the radio
// one, and above all the rule that silence is never treated as an empty mesh.

// The picker marks a mesh we have heard from, and leaves the other plain --
// NOT labelled dead, because we do not know that.
func TestLiveMeshIsMarkedAndQuietOneIsNotCondemned(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	e := newEngine(f, nil)
	e.SetMeshLive(func(_ context.Context, _, bridgeID string) bool {
		return bridgeID == "nllei01parallax01"
	})

	_, _ = e.Handle(ctx, tenant, who, ch, OptSendMessage, "")
	r, err := e.Handle(ctx, tenant, who, ch, OptOptInYes, "")
	if err != nil {
		t.Fatalf("opt in: %v", err)
	}
	if len(r.Options) != 2 {
		t.Fatalf("picker showed %d options, want 2", len(r.Options))
	}
	var live, quiet Option
	for _, o := range r.Options {
		if strings.Contains(o.ID, "parallax") {
			live = o
		} else {
			quiet = o
		}
	}
	if !strings.Contains(live.Label, "live") {
		t.Errorf("the mesh we heard from is not marked: %q", live.Label)
	}
	if quiet.Label != "Tesseract" {
		t.Errorf("a mesh with no recent traffic was relabelled %q; silence is not proof it is empty", quiet.Label)
	}
}

// The order of the options is the resolution order for a typed "1" or "2", and
// it must not move when presence changes. A visitor reads the menu on one
// replica and answers on the other; if liveness reordered the list, a node
// going quiet in between would silently redirect the message to the other mesh.
func TestPresenceNeverReordersTheOptions(t *testing.T) {
	ctx := context.Background()
	order := func(live bool) []string {
		f := newFake()
		e := newEngine(f, nil)
		e.SetMeshLive(func(_ context.Context, _, bridgeID string) bool {
			return live && bridgeID == "nllei01parallax01"
		})
		_, _ = e.Handle(ctx, tenant, who, ch, OptSendMessage, "")
		r, _ := e.Handle(ctx, tenant, who, ch, OptOptInYes, "")
		var ids []string
		for _, o := range r.Options {
			ids = append(ids, o.ID)
		}
		return ids
	}
	a, b := order(false), order(true)
	if len(a) != len(b) {
		t.Fatalf("option count changed with presence: %v vs %v", a, b)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("presence reordered the menu: %v became %v -- a typed digit now selects a different mesh", a, b)
		}
	}
}

// A quiet mesh is still offered and still relays. Presence is only observable
// when a node transmits, so at the start of a show day every kit is "quiet"
// while both meshes are fine; refusing on that would close the booth.
func TestAQuietMeshIsStillOfferedAndStillRelays(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	e := newEngine(f, nil)
	e.SetMeshLive(func(context.Context, string, string) bool { return false })

	_, _ = e.Handle(ctx, tenant, who, ch, OptSendMessage, "")
	_, _ = e.Handle(ctx, tenant, who, ch, OptOptInYes, "")
	r, err := e.Handle(ctx, tenant, who, ch, kitOptPrefix+"nllei01parallax01", "")
	if err != nil {
		t.Fatalf("pick a quiet kit: %v", err)
	}
	// It warns...
	if !strings.Contains(strings.ToLower(r.Text), "may not answer") {
		t.Errorf("no warning that the mesh has been quiet: %q", r.Text)
	}
	// ...and then it still sends.
	r, err = e.Handle(ctx, tenant, who, ch, "", "hello from the stand")
	if err != nil {
		t.Fatalf("relay to a quiet kit: %v", err)
	}
	if r.Relay == nil {
		t.Fatal("a quiet mesh refused the relay; silence is not evidence of an empty mesh, and on the " +
			"first message of the day every mesh is quiet")
	}
}

// A live mesh gets the plain prompt, with no warning to read.
func TestALiveMeshGetsNoWarning(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	e := newEngine(f, nil)
	e.SetMeshLive(func(context.Context, string, string) bool { return true })

	_, _ = e.Handle(ctx, tenant, who, ch, OptSendMessage, "")
	_, _ = e.Handle(ctx, tenant, who, ch, OptOptInYes, "")
	r, _ := e.Handle(ctx, tenant, who, ch, kitOptPrefix+"nllei01parallax01", "")
	if strings.Contains(strings.ToLower(r.Text), "may not answer") {
		t.Errorf("a mesh we just heard from was announced as quiet: %q", r.Text)
	}
}

// With no presence source wired at all, the flow is exactly what it was before
// MESHSAT-1181: every kit offered plainly, no markers, no warnings.
func TestWithoutAPresenceSourceNothingChanges(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	e := newEngine(f, nil) // meshLive deliberately unset

	_, _ = e.Handle(ctx, tenant, who, ch, OptSendMessage, "")
	r, _ := e.Handle(ctx, tenant, who, ch, OptOptInYes, "")
	for _, o := range r.Options {
		if strings.Contains(o.Label, "(") {
			t.Errorf("a marker appeared with no presence source wired: %q", o.Label)
		}
	}
	r, _ = e.Handle(ctx, tenant, who, ch, kitOptPrefix+"nllei01parallax01", "")
	if strings.Contains(strings.ToLower(r.Text), "may not answer") {
		t.Errorf("a quiet-mesh warning appeared with no presence source wired: %q", r.Text)
	}
}
