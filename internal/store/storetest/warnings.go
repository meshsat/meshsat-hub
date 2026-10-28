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
}
