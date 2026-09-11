package stripe

import (
	"context"
	"log/slog"
	"sync"

	"github.com/meshsat/meshsat-hub/internal/metrics"
)

// This file exists because of the shape of every Stripe defect found so far.
//
// Seven went to production in two days and six were the same bug: Stripe moved
// a field between API versions, the Go struct kept the old `json` tag,
// encoding/json produced a zero value, and a fallback made it look like normal
// operation. No error, no log, no failing test. One of them granted every
// subscriber an expiry invented from the clock and then logged "plan granted"
// with the invented date as if Stripe had said it. Another left the first
// payment of every new subscriber with no VAT document at all.
//
// The fallbacks themselves are right: reading the new location and then the old
// one is what lets a replayed event from before a version bump still work. What
// was wrong is that falling back was SILENT. So every fallback that stands in
// for an absent field now says so, loudly and exactly once per field per event,
// and the webhook's own API version is checked against what this code was
// written to read.

// knownAPIVersions are the Stripe API versions these structs have actually been
// read against, with a real captured payload in testdata for each. A delivery
// rendered by anything else is not necessarily broken -- most version bumps
// touch nothing the Hub decodes -- but it is unreviewed, and unreviewed is how
// the last six defects arrived.
//
// Raising this list is a deliberate act: capture the payloads, run the shape
// tests against them, then add the version here.
// Literal keys, deliberately -- not `apiVersion:`. Keying on the constant meant
// that raising the pin to a version already listed here collided and the package
// stopped compiling with "duplicate key in map literal", which says nothing about
// Stripe, payments or what to do. A test asserts the pinned version is present,
// so the same mistake now fails with a sentence instead.
var knownAPIVersions = map[string]string{
	// What every outbound call pins, via the Stripe-Version header.
	"2025-08-27.basil": "pinned for outbound calls; the webhook endpoint is set to this in the dashboard",
	// The account default. The /v1/events endpoint renders archived events at
	// this version regardless of any header, so it is what a replay tool sees.
	"2026-08-26.dahlia": "the account default; what a replayed archived event is rendered at",
}

// seenVersion keeps an unknown version to one log line per process rather than
// one per delivery. Stripe retries, and a shouting log that repeats every few
// seconds is a log nobody reads.
var seenVersion sync.Map

// missingField records that a payload field the Hub depends on was not there.
//
// It never fails the delivery. The fallback still runs and the Hub still does
// the right thing with what it has -- refusing an event because one field moved
// would turn a cosmetic problem into a payment outage, and Stripe would retry
// it for three days. This is the signal, not the enforcement.
func missingField(field, eventType, consequence string) {
	metrics.StripeFieldMissingTotal.WithLabelValues(field).Inc()
	slog.Error("stripe: A FIELD THIS HUB DEPENDS ON WAS NOT IN THE PAYLOAD. "+
		"This is how a moved field used to pass unnoticed: a fallback covered it and "+
		"nothing said so. Check the Stripe changelog for the webhook endpoint's API version.",
		"field", field, "event_type", eventType, "consequence", consequence)
}

// noteAPIVersion records which API version rendered a delivery, and shouts once
// if it is one nobody has read the structs against.
//
// The webhook endpoint's version lives in the Stripe dashboard, outside this
// repo and outside any config the Hub can see, so before this the Hub had no
// idea what it was being sent. The envelope carries it on every delivery and it
// was being decoded into nothing.
func noteAPIVersion(version, eventType string) {
	if version == "" {
		// Every genuine delivery carries one. An empty value means either a
		// hand-built payload or an envelope shape change.
		metrics.StripeAPIVersionTotal.WithLabelValues("absent").Inc()
		return
	}
	metrics.StripeAPIVersionTotal.WithLabelValues(version).Inc()
	if _, known := knownAPIVersions[version]; known {
		return
	}
	if _, already := seenVersion.LoadOrStore(version, true); already {
		return
	}
	slog.Error("stripe: A DELIVERY ARRIVED RENDERED BY AN API VERSION THIS HUB HAS NOT BEEN READ AGAINST. "+
		"Stripe moves fields between versions and this codebase decodes them by hand, so a payload "+
		"shape may have changed underneath it. Read the changelog between the two versions, capture a "+
		"payload into internal/stripe/testdata, run the shape tests, then add the version to "+
		"knownAPIVersions.",
		"version", version, "event_type", eventType, "known", knownVersionList())
}

func knownVersionList() []string {
	out := make([]string, 0, len(knownAPIVersions))
	for v := range knownAPIVersions {
		out = append(out, v)
	}
	return out
}

// WebhookAPIVersion asks Stripe what API version the webhook endpoint is
// actually pinned to.
//
// This is the one assumption in the Stripe path that cannot be read from the
// code or the config: the endpoint's version is set in the dashboard, and
// `POST /v1/accounts` refuses to write your own account, so it can never be
// managed from here. It is one click away from changing, and the only sign
// would have been a payload that decoded to zero values.
func (c *Client) WebhookAPIVersion(ctx context.Context) ([]WebhookEndpoint, error) {
	if c == nil || c.key == "" {
		return nil, ErrNotConfigured
	}
	var out struct {
		Data []WebhookEndpoint `json:"data"`
	}
	if err := c.get(ctx, "/webhook_endpoints", &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// WebhookEndpoint is the part of a Stripe webhook endpoint worth checking.
type WebhookEndpoint struct {
	ID         string   `json:"id"`
	URL        string   `json:"url"`
	Status     string   `json:"status"`
	APIVersion string   `json:"api_version"`
	Events     []string `json:"enabled_events"`
}

// Enabled reports whether Stripe will actually deliver to this endpoint.
func (w WebhookEndpoint) Enabled() bool { return w.Status == "enabled" }

// PinnedAPIVersion is the version every outbound call is made at, exported so
// startup can compare it with what the webhook endpoint is set to.
func PinnedAPIVersion() string { return apiVersion }

// KnownAPIVersion reports whether the structs in this package have been read
// against a version, so a caller can decide how loudly to complain.
func KnownAPIVersion(v string) bool { _, ok := knownAPIVersions[v]; return ok }
