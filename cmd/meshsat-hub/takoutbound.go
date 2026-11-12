package main

import (
	"context"
	"errors"
	"log/slog"
	"net"

	"github.com/meshsat/meshsat-hub/internal/bus"
	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/leader"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/takfront"
	"github.com/meshsat/meshsat-hub/internal/takhosted"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
)

// The outbound CoT leg: a tenant's device positions, SOS and telemetry pushed to
// every TAK server that tenant has -- the hosted instance the operator runs for
// them, the server they run themselves (MESHSAT-1065), or both.
//
// # Why this is not part of startTAKHosted
//
// It was once part of the public TAK front's start-up (retired with port 8089,
// MESHSAT-1466), and that was wrong in a way only bring-your-own-server exposed:
// a customer pointing the Hub at their OWN TAK server needs no hosted instance,
// and nothing inbound touches the Hub for them.
//
// So the outbound leg starts whenever the Hub starts, and resolves whatever
// upstreams exist at the time. With hosted TAK off, that is tenants' own
// servers; with it on (HUB_TAK_HOSTED_ENABLED), hosted instances as well.

// startTAKOutbound registers the forwarder as a leader singleton.
//
// A singleton because it WRITES: two replicas forwarding the same position would
// draw every device twice on every map. The hosted-instance resolution it reads,
// by contrast, runs on every replica, so a new leader finds it already done.
//
// It returns the forwarder as something a tenant eviction can reach, or nil when
// there is nothing to register: a purged or closed tenant must not keep a
// connection open to its TAK server (MESHSAT-1460).
func startTAKOutbound(
	dataStore store.Store,
	msgBus bus.MessageBus,
	singletons *leader.Singletons,
	upstreams *takhosted.Upstreams,
	chat bool,
) tenancy.TenantForgetter {
	if singletons == nil || upstreams == nil {
		return nil
	}

	// takfront.DialUpstream rather than a dial of our own. That function is the
	// upstream TLS policy: when no certificate name is configured it still verifies
	// the chain against the tenant's CA and skips only the name check, through
	// VerifyConnection so it also fires on a resumed session. A second
	// implementation here is exactly where that property would quietly rot into
	// InsecureSkipVerify. Zero takes its default timeout.
	dial := func(ctx context.Context, t *takfront.Tenant) (net.Conn, error) {
		return takfront.DialUpstream(ctx, t, 0)
	}

	fwd := takhosted.NewForwarder(msgBus, dataStore, dial, upstreams.For, slog.Default())
	// Dispatcher-to-crew chat (MESHSAT-1706, HUB_TAKCHAT_ENABLED).
	fwd.SetChat(chat)
	singletons.Add("takhosted-outbound", fwd.Run)
	slog.Info("takhosted: outbound CoT forwarding registered (hosted instances and tenants' own TAK servers)")
	return fwd
}

// newTAKUpstreams builds the resolver the forwarder asks for upstreams.
//
// Constructed here rather than in main.go so that main needs no import of
// internal/takhosted: the type is inferred at the call site and only passed along.
func newTAKUpstreams(accts *integrations.Service) *takhosted.Upstreams {
	return takhosted.NewUpstreams(
		takhosted.NewExternalUpstreams(takConfigSource{accts: accts}, slog.Default()),
		slog.Default(),
	)
}

// takConfigSource answers takhosted's one question about a tenant's own TAK server
// from the per-tenant credential service.
//
// The seam exists because internal/takhosted should not import the credential
// service to read six strings: that service carries the encryption and store
// surface needed to produce them, and the forwarder needs none of it.
type takConfigSource struct{ accts *integrations.Service }

var _ takhosted.TAKConfigSource = takConfigSource{}

func (s takConfigSource) TAKUpstream(ctx context.Context, tenantID string) (map[string]string, error) {
	if s.accts == nil {
		return nil, nil
	}
	acct, err := s.accts.ForTenant(ctx, tenantID, integrations.ProviderTAK)
	if err != nil {
		if errors.Is(err, integrations.ErrUnknownProvider) {
			// The provider is not registered in this build. Nothing configured,
			// rather than an error to log per position.
			return nil, nil
		}
		return nil, err
	}
	if acct == nil {
		// The ordinary case: this tenant runs no TAK server of their own.
		return nil, nil
	}
	// A copy, not the service's own map. ForTenant hands back a cached Account, and
	// a caller that mutated its Fields would be editing a decrypted credential
	// every other reader shares.
	out := make(map[string]string, len(acct.Fields))
	for k, v := range acct.Fields {
		out[k] = v
	}
	return out, nil
}
