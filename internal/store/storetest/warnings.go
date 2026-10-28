package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// MESHSAT-1734, the warning relay. In the conformance suite because the SQL
// is written twice, and because a match listed in the wrong tenant would
// show one tenant's warnings in another's console. The default tenant is the
// VICTIM.
func testWarnings(t *testing.T, db store.Store) {
	ctx := context.Background()
	const (
		mine  = "t_warn_mine"
		other = store.DefaultTenantID
	)
	sent := time.Date(2026, 10, 28, 10, 11, 41, 0, time.UTC)
	w := &store.Warning{Source: "meteoalarm-nl", Identifier: "2.49.0.0.528.0.NL.1", Sender: "noreply@knmi.nl",
		SenderName: "KNMI", MsgType: "Alert", Status: "Actual", Sent: sent, Expires: sent.Add(24 * time.Hour),
		Severity: "Moderate", Urgency: "Expected", Certainty: "Observed", Category: "Met", Event: "Moderate Wind",
		Headline: "Yellow wind warning", Language: "en-GB", Areas: `[{"desc":"Texel","geocodes":["EMMA_ID:NL804"]}]`,
		Text: "Chance of wind.", RawHash: "aa"}

	if got, err := db.GetWarning(ctx, w.Source, w.Identifier); err != nil || got != nil {
		t.Fatalf("an unknown warning: %+v %v", got, err)
	}
	changed, err := db.UpsertWarning(ctx, w)
	if err != nil || !changed {
		t.Fatalf("first upsert: changed=%v err=%v", changed, err)
	}
	// The same message seen again on the next poll is not a change.
	changed, err = db.UpsertWarning(ctx, w)
	if err != nil || changed {
		t.Fatalf("a re-poll of the same message counted as a change: %v %v", changed, err)
	}
	// A re-sent identifier (new sent or document) is.
	w2 := *w
	w2.Sent = sent.Add(time.Hour)
	w2.RawHash = "bb"
	w2.Headline = "Orange wind warning"
	changed, err = db.UpsertWarning(ctx, &w2)
	if err != nil || !changed {
		t.Fatalf("a re-sent message did not count as a change: %v %v", changed, err)
	}
	got, err := db.GetWarning(ctx, w.Source, w.Identifier)
	if err != nil || got == nil {
		t.Fatalf("get: %+v %v", got, err)
	}
	if got.Headline != "Orange wind warning" || got.RawHash != "bb" || !got.Sent.Equal(w2.Sent) || got.Expires.IsZero() || !got.Effective.IsZero() {
		t.Fatalf("stored row: %+v", got)
	}
	if got.FirstSeen.IsZero() || got.LastSeen.Before(got.FirstSeen) {
		t.Fatalf("seen times: first %v last %v", got.FirstSeen, got.LastSeen)
	}

	// The tenant's match, and the victim's.
	if err := db.UpsertWarningMatch(ctx, &store.WarningMatch{TenantID: mine, Source: w.Source, Identifier: w.Identifier,
		MsgType: "Alert", Bridges: `["kit-a"]`, Detail: `{"kit-a":["mqtt"]}`}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertWarningMatch(ctx, &store.WarningMatch{TenantID: other, Source: w.Source, Identifier: w.Identifier,
		MsgType: "Alert", Test: true, Bridges: `["kit-v"]`}); err != nil {
		t.Fatal(err)
	}
	// A second match of the same message takes the latest bridges and keeps
	// the row one row.
	if err := db.UpsertWarningMatch(ctx, &store.WarningMatch{TenantID: mine, Source: w.Source, Identifier: w.Identifier,
		MsgType: "Alert", Bridges: `["kit-a","kit-b"]`}); err != nil {
		t.Fatal(err)
	}
	// One tenant's match by the message it is of: the latest bridges, never
	// another tenant's row, nil for a message that went to none of its kits.
	if got, err := db.GetWarningMatch(ctx, mine, w.Source, w.Identifier); err != nil || got == nil || got.Bridges != `["kit-a","kit-b"]` || got.Test || got.MatchedAt.IsZero() || got.TenantID != mine {
		t.Fatalf("get match: %+v %v", got, err)
	}
	if got, err := db.GetWarningMatch(ctx, other, w.Source, w.Identifier); err != nil || got == nil || got.Bridges != `["kit-v"]` || !got.Test {
		t.Fatalf("the victim's match by its own tenant: %+v %v", got, err)
	}
	if got, err := db.GetWarningMatch(ctx, "t_warn_nobody", w.Source, w.Identifier); err != nil || got != nil {
		t.Fatalf("a tenant with no match: %+v %v", got, err)
	}
	if got, err := db.GetWarningMatch(ctx, mine, w.Source, "2.49.0.0.528.0.NL.9"); err != nil || got != nil {
		t.Fatalf("a message the tenant never matched: %+v %v", got, err)
	}
	list, err := db.ListTenantWarnings(ctx, mine, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %d %v", len(list), err)
	}
	if list[0].Bridges != `["kit-a","kit-b"]` || list[0].Test || list[0].Headline != "Orange wind warning" || list[0].MatchedAt.IsZero() {
		t.Fatalf("read another tenant's match or lost the update: %+v", list[0])
	}
	victim, err := db.ListTenantWarnings(ctx, other, 10)
	if err != nil || len(victim) != 1 || victim[0].Bridges != `["kit-v"]` || !victim[0].Test {
		t.Fatalf("the victim's match was touched: %+v %v", victim, err)
	}
	if none, err := db.ListTenantWarnings(ctx, "t_warn_nobody", 10); err != nil || len(none) != 0 {
		t.Fatalf("a tenant with no match: %v %v", none, err)
	}

	// The repeats: the document is kept only on request; in force lists the
	// tenant's own matches that are unexpired, kept and not superseded, with
	// the document; the plain listing never carries it.
	now := w2.Sent.Add(2 * time.Hour)
	if rows, err := db.ListWarningsInForce(ctx, mine, now); err != nil || len(rows) != 0 {
		t.Fatalf("in force before the document is kept: %+v %v", rows, err)
	}
	if err := db.SetWarningRaw(ctx, w.Source, w.Identifier, "<alert/>"); err != nil {
		t.Fatal(err)
	}
	rows, err := db.ListWarningsInForce(ctx, mine, now)
	if err != nil || len(rows) != 1 || rows[0].Raw != "<alert/>" || rows[0].Bridges != `["kit-a","kit-b"]` || rows[0].SupersededBy != "" {
		t.Fatalf("in force: %+v %v", rows, err)
	}
	if list, _ := db.ListTenantWarnings(ctx, mine, 10); len(list) != 1 || list[0].Raw != "" {
		t.Fatalf("the listing carries the document: %+v", list)
	}
	if rows, _ := db.ListWarningsInForce(ctx, other, now); len(rows) != 1 || rows[0].Bridges != `["kit-v"]` {
		t.Fatalf("the victim's in-force rows are not its own match: %+v", rows)
	}
	if rows, _ := db.ListWarningsInForce(ctx, "t_warn_nobody", now); len(rows) != 0 {
		t.Fatalf("a tenant with no match has a warning in force: %+v", rows)
	}
	if rows, _ := db.ListWarningsInForce(ctx, mine, w2.Expires.Add(time.Second)); len(rows) != 0 {
		t.Fatalf("an expired warning is in force: %+v", rows)
	}
	// Superseded by an Update: out of force, and the row says by whom.
	if err := db.MarkWarningSuperseded(ctx, w.Source, w.Identifier, "2.49.0.0.528.0.NL.2"); err != nil {
		t.Fatal(err)
	}
	if rows, _ := db.ListWarningsInForce(ctx, mine, now); len(rows) != 0 {
		t.Fatalf("a superseded warning is in force: %+v", rows)
	}
	if got, _ := db.GetWarning(ctx, w.Source, w.Identifier); got == nil || got.SupersededBy != "2.49.0.0.528.0.NL.2" {
		t.Fatalf("superseded_by: %+v", got)
	}
	if list, _ := db.ListTenantWarnings(ctx, mine, 10); len(list) != 1 || list[0].SupersededBy != "2.49.0.0.528.0.NL.2" {
		t.Fatalf("the listing does not say the warning was superseded: %+v", list)
	}
	// A re-sent identifier drops the kept document until it matches again.
	if err := db.MarkWarningSuperseded(ctx, w.Source, w.Identifier, ""); err != nil {
		t.Fatal(err)
	}
	if rows, _ := db.ListWarningsInForce(ctx, mine, now); len(rows) != 1 {
		t.Fatalf("back in force: %+v", rows)
	}
	w3 := w2
	w3.RawHash = "cc"
	if changed, err := db.UpsertWarning(ctx, &w3); err != nil || !changed {
		t.Fatalf("re-sent: %v %v", changed, err)
	}
	if rows, _ := db.ListWarningsInForce(ctx, mine, now); len(rows) != 0 {
		t.Fatalf("a re-sent message kept the old document: %+v", rows)
	}
}
