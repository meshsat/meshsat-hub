package takhosted

import (
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/meshsat/meshsat-hub/internal/tak"
	"github.com/meshsat/meshsat-hub/internal/takfront"
)

// Whether a tenant's CoT actually reached their TAK server (MESHSAT-1066).
//
// Until this file existed, it could fail silently: see upstreamRejected for the
// measurement and the mechanism.

// Metrics carry no tenant label, matching every other metric in the Hub: a label
// whose values are customer identifiers makes the series count grow with the
// customer count, and the audit log is where per-tenant detail belongs.
//
// This is also why kind is DERIVED rather than taken from takfront.Tenant.Label.
// That field is "external" for a tenant's own server but an opaque ten-character
// random string for a hosted instance -- deliberately non-identifying, but one
// value per customer, which is exactly the cardinality a metric label must not
// have.
var forwardsFailed = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "meshsat_hub_takhosted_forward_failed_total",
	Help: "CoT events that did not reach a tenant's TAK server, by upstream kind and cause.",
}, []string{"kind", "reason"})

// Upstream kinds. Two values, forever: a tenant's server is either one the
// operator runs for them or one they run themselves.
const (
	kindHosted   = "hosted"
	kindExternal = "external"
)

// Causes. Materialised in init below so an alert on a rate can fire: an absent
// series is not a zero one, and the case this file was written for produced no
// series at all.
const (
	// reasonRefused is the one this file exists for: the dial and the first write
	// both succeed, and the server has already refused our certificate.
	reasonRefused = "refused"
	// reasonDial is an upstream that could not be reached at all.
	reasonDial = "dial"
	// reasonWrite is a connection that was accepted and then failed under a write.
	reasonWrite = "write"
	// reasonQueue is an upstream that is not keeping up: its link already had a
	// full queue waiting, so the event was dropped rather than made to wait.
	reasonQueue = "queue"
	// reasonStream is a connection closed because what the server sent on it
	// could not be read as CoT XML.
	reasonStream = "stream"
)

// intakeDropped counts bus messages the forwarder could not take because its
// dispatcher had fallen behind. No kind label: at that point the message has not
// been matched to an upstream yet.
var intakeDropped = promauto.NewCounter(prometheus.CounterOpts{
	Name: "meshsat_hub_takhosted_intake_dropped_total",
	Help: "Bus messages the TAK forwarder dropped because its dispatcher was behind.",
})

// cotTotal counts the CoT that reaches the Hub from outside, by where it came from
// and what became of it. No tenant and no sender label, for the reason given
// above.
//
// source "client" is what kits and apps export (MESHSAT-1458): result is
// "forwarded", "nowhere" (accepted, and the tenant has no TAK server), or why it
// was dropped. source "upstream" is what a tenant's TAK server sends back
// (MESHSAT-1461): result is "delivered" (handed to the tenant's clients) or why
// it was dropped. The reasons are the fixed results in cot_ingest.go and
// fanout.go, or a tak.Refused reason.
var cotTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "meshsat_hub_takhosted_cot_total",
	Help: "CoT events reaching the Hub from kits and apps (client) or from a tenant's TAK server (upstream), by what became of them.",
}, []string{"source", "result"})

func init() {
	// Materialised for the same reason as the failure counter: a rate over a
	// series that does not exist yet is not zero, it is absent.
	for _, result := range []string{
		cotForwarded, cotNowhere, cotUnregistered, cotRateLimited, cotDuplicate,
		cotControl, cotHubUID, cotError,
		tak.ReasonSize, tak.ReasonEncoding, tak.ReasonMarkup, tak.ReasonStructure,
		tak.ReasonNamespace, tak.ReasonLimits, tak.ReasonFields, tak.ReasonStale,
	} {
		cotTotal.WithLabelValues(sourceClient, result)
	}
	for _, result := range []string{
		cotDelivered, cotEcho, cotRateLimited, cotDuplicate, cotControl, cotHubUID, cotError,
		tak.ReasonSize, tak.ReasonEncoding, tak.ReasonMarkup, tak.ReasonStructure,
		tak.ReasonNamespace, tak.ReasonLimits, tak.ReasonFields, tak.ReasonStale,
	} {
		cotTotal.WithLabelValues(sourceUpstream, result)
	}
	for _, kind := range []string{kindHosted, kindExternal} {
		for _, reason := range []string{reasonRefused, reasonDial, reasonWrite, reasonQueue, reasonStream} {
			forwardsFailed.WithLabelValues(kind, reason)
		}
	}
}

// kindOf reports which sort of upstream this is, for a metric label.
func kindOf(t *takfront.Tenant) string {
	if t != nil && t.Label == externalLabel {
		return kindExternal
	}
	return kindHosted
}

// dialProbeWindow is how long a freshly dialled upstream is watched for a
// rejection before it is trusted.
//
// Half a second: comfortably longer than the round trip the dial has already
// paid for an in-cluster or nearby server, and invisible against the fifteen
// minutes the connection is then held for. It is not a correctness boundary --
// see upstreamRejected for what a window that is too short costs.
const dialProbeWindow = 500 * time.Millisecond

// upstreamRejected reports why a freshly dialled upstream is unusable, or nil if
// it looks healthy.
//
// # Why this is necessary at all
//
// Under TLS 1.3 the client certificate is sent AFTER the server's Finished, so a
// server that refuses it does so on a connection the client already considers
// established. Measured against a listener requiring a client certificate, with
// the same TLS configuration dialUpstreamWith builds:
//
//	step          valid certificate   certificate from an untrusted CA
//	dial          nil                 nil          <- the dial SUCCEEDS
//	bounded read  i/o timeout         remote error: tls: certificate required
//	first write   nil                 nil          <- so send() returns nil
//	second write  nil                 broken pipe
//
// That is the whole defect: the dial succeeds, the first write lands in the
// kernel buffer and returns nil, so send returns nil and onPosition never reaches
// its Warn. A tenant whose TAK server refuses this Hub's certificate lost every
// position in silence, while the log showed the same line a working tenant's
// does.
//
// # Why a bounded read is the right probe
//
// The Hub has written nothing yet when this runs, and a TAK server says nothing
// to a client that has not spoken. That makes silence the healthy answer, and an
// error the server telling us why it is finished with us.
//
// Rule 15 is satisfied and not bent: this is a raw conn.Read with a deadline,
// used exactly once, not SetReadDeadline around a bufio.Scanner in a loop.
// internal/reticulum/iface_tcp.go is the existing precedent for the permitted
// form.
//
// Whatever it does read is returned, not dropped: since MESHSAT-1461 the return
// stream is read, and those bytes are its beginning.
//
// # When the window is too short
//
// For a distant server whose alert has not arrived yet, the connection is cached,
// the first write succeeds, the second fails, send drops it and redials, and this
// runs again on a connection whose alert has certainly arrived. The failure is
// then reported one position late rather than never. Late is recoverable; silent
// is what this fixes.
func upstreamRejected(c net.Conn) (first []byte, why error) {
	if err := c.SetReadDeadline(time.Now().Add(dialProbeWindow)); err != nil {
		// A connection that cannot take a deadline cannot be probed. Forwarding is
		// the more useful failure mode than refusing to try.
		return nil, nil
	}
	defer func() { _ = c.SetReadDeadline(time.Time{}) }()

	buf := make([]byte, 4096)
	switch n, err := c.Read(buf); {
	case err == nil:
		// The server sent something. It is alive and talking, which is the
		// opposite of the problem being looked for -- and what it said is the
		// start of its stream, so it is handed back rather than dropped.
		return buf[:n], nil
	case isTimeout(err):
		return nil, nil
	case errors.Is(err, io.EOF), errors.Is(err, net.ErrClosed):
		return nil, fmt.Errorf("it closed the connection without sending anything: %w", err)
	default:
		// The interesting case, and the one with a message worth showing somebody:
		// "remote error: tls: certificate required".
		return nil, err
	}
}

// isTimeout reports whether err is a deadline expiry rather than a real fault.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
