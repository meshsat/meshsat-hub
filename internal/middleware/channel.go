package middleware

import (
	"context"
	"net/http"
)

// Channel is the route a request took to reach the Hub.
//
// It exists because the Hub is reachable by more than one path and those paths
// do not carry the same guarantees. A request through the internet edge has
// passed the VPS HAProxy and ingress-nginx, which are ours and which set the
// forwarding headers the Hub reads. A request through the Tor hidden service has
// passed neither: tor forwards raw TCP to the Hub's port, so the peer address is
// the tor pod -- inside HUB_TRUSTED_PROXIES, because that list names the pod
// network -- and every header is the anonymous client's own invention.
//
// That combination is what made the onion a way to forge a rate-limit key
// (MESHSAT-1169): trusted peer plus client-authored X-Forwarded-For.
//
// The channel is therefore established by WHICH LISTENER accepted the
// connection, never by a header. A header would be attacker-supplied on exactly
// the path it is meant to describe, and a shared-secret header would only move
// the question to who else can reach the port.
type Channel string

const (
	// ChannelInternet is the edge path: VPS HAProxy -> ingress-nginx -> Hub.
	ChannelInternet Channel = "internet"
	// ChannelOnion is the Tor hidden service, reaching its own Hub listener.
	ChannelOnion Channel = "onion"
)

type channelKey struct{}

// WithChannel wraps h so that every request it serves is marked as having
// arrived on c. It is applied per listener in cmd/meshsat-hub.
func WithChannel(h http.Handler, c Channel) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), channelKey{}, c)))
	})
}

// ChannelOf reports the channel a request arrived on.
//
// Anything not explicitly marked is the internet edge. That default is the safe
// one: it is the path whose forwarding headers are set by our own proxies, so
// treating an unmarked request as edge traffic cannot silently grant the onion's
// exemptions to something else.
func ChannelOf(r *http.Request) Channel {
	if c, ok := r.Context().Value(channelKey{}).(Channel); ok {
		return c
	}
	return ChannelInternet
}
