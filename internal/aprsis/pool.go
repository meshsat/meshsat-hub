package aprsis

import (
	"context"
	"log/slog"
	"strconv"
	"sync"

	"github.com/meshsat/meshsat-hub/internal/integrations"
)

// ConnPool holds one live APRS-IS connection per tenant that has configured its
// own callsign.
//
// It deliberately does NOT copy internal/sms/pool.go, and the difference matters.
// Those pools build a stateless HTTP client on demand inside the send path,
// which is free. An APRS-IS client is a long-lived TCP connection with a login
// handshake, a read loop and reconnect backoff, so building one on the message
// path would block an MQTT handler for as long as a dial takes -- and a tenant
// with a wrong server address would slow down every other tenant's positions.
//
// So connections are made by Reconcile, on its own schedule, and ForTenant is a
// map lookup that never dials. A tenant whose connection is not up yet, or is
// down, simply does not transmit until it is; APRS-IS is a best-effort feed and
// a missed position there is invisible.
//
// One connection per tenant per PROCESS, and the caller must ensure only the
// lease holder runs this: APRS-IS allows one login per callsign-SSID and drops
// the older session, so two replicas connected under one callsign would evict
// each other in a loop -- the same failure as MESHSAT-980 on the MQTT broker.
type ConnPool struct {
	platform      *Client // the operator's own, from the environment
	accounts      *integrations.Service
	defaultTenant string
	onPacket      func(tenantID, callsign, line string)

	mu    sync.Mutex
	conns map[string]*conn
}

type conn struct {
	fingerprint string
	client      *Client
}

// NewConnPool builds the pool. platform may be nil when the operator has not
// configured a callsign of its own; tenants with their own accounts still work.
func NewConnPool(platform *Client, accounts *integrations.Service, defaultTenant string) *ConnPool {
	return &ConnPool{
		platform:      platform,
		accounts:      accounts,
		defaultTenant: defaultTenant,
		conns:         map[string]*conn{},
	}
}

// SetPacketHandler registers the inbound handler. It is given the tenant the
// connection belongs to, because an APRS message arriving on one tenant's
// connection is addressed to THAT tenant's callsign and must not be published
// into another's topic space.
func (p *ConnPool) SetPacketHandler(fn func(tenantID, callsign, line string)) {
	p.mu.Lock()
	p.onPacket = fn
	p.mu.Unlock()
}

// ForTenant returns the tenant's connected client, or nil.
//
// nil is the normal answer and means "this tenant does not transmit": no
// account, transmission paused, or the connection is not up. There is NO
// fallback to the platform client for a non-default tenant, and that is the
// whole point of MESHSAT-1121 -- a callsign is a licence issued to a person,
// and putting one tenant's positions on air under the operator's licence is
// what MESHSAT-1032 contained in the first place.
func (p *ConnPool) ForTenant(tenantID string) *Client {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.conns[tenantID]; ok && c.client.IsConnected() {
		return c.client
	}
	return nil
}

// Reconcile brings the live connections in line with the stored accounts:
// connects tenants that have one, drops tenants that no longer do, and
// reconnects a tenant whose callsign, passcode or server changed.
//
// Called on a timer by the lease holder. Errors are logged per tenant and never
// returned, because one tenant's bad passcode must not stop another tenant's
// connection being established.
func (p *ConnPool) Reconcile(ctx context.Context) {
	want := map[string]*integrations.Account{}

	// The platform's own callsign serves the default tenant, exactly like every
	// other provider: the environment values ARE the default tenant's account.
	if p.platform != nil {
		want[p.defaultTenant] = nil // nil account = use the pre-built platform client
	}

	if p.accounts != nil {
		tenants, err := p.accounts.TenantsWith(ctx, integrations.ProviderAPRSIS)
		if err != nil {
			slog.Warn("aprsis: listing tenants with an APRS account failed", "error", err)
		}
		for _, tid := range tenants {
			acct, err := p.accounts.ForTenant(ctx, tid, integrations.ProviderAPRSIS)
			if err != nil || acct == nil {
				continue
			}
			// A tenant's own row wins over the platform client even for the
			// default tenant: the operator may have moved its callsign into the
			// UI, and the stored value is the one somebody last edited.
			if !enabled(acct.Get("enabled")) {
				delete(want, tid)
				continue
			}
			if acct.Get("callsign") == "" || acct.Get("passcode") == "" {
				continue
			}
			want[tid] = acct
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Drop connections nobody wants any more.
	for tid, c := range p.conns {
		if _, keep := want[tid]; !keep {
			slog.Info("aprsis: disconnecting, the tenant no longer has an APRS account", "tenant", tid)
			c.client.Disconnect()
			delete(p.conns, tid)
		}
	}

	for tid, acct := range want {
		if acct == nil {
			// The platform client: its lifecycle belongs to the caller that built
			// it, so the pool only records it, never dials or closes it.
			if _, ok := p.conns[tid]; !ok && p.platform != nil {
				p.conns[tid] = &conn{fingerprint: "platform", client: p.platform}
				p.attach(tid, p.platform)
			}
			continue
		}

		fp := integrations.Fingerprint(acct.Get("callsign"), acct.Get("ssid"),
			acct.Get("passcode"), acct.Get("server"))
		if c, ok := p.conns[tid]; ok {
			if c.fingerprint == fp && c.client.IsConnected() {
				continue
			}
			// Credentials changed, or the link is down: start again rather than
			// try to mutate a client around a live read loop.
			c.client.Disconnect()
			delete(p.conns, tid)
		}

		client := NewClient(serverOrDefault(acct.Get("server")), acct.Get("callsign"),
			ssidOrDefault(acct.Get("ssid")), acct.Get("passcode"), "")
		if err := client.Connect(); err != nil {
			// Logged, not returned. A tenant that typed its passcode wrongly gets
			// a line naming the tenant; everybody else keeps transmitting.
			slog.Warn("aprsis: connect failed for a tenant's callsign",
				"tenant", tid, "callsign", acct.Get("callsign"), "error", err)
			continue
		}
		p.conns[tid] = &conn{fingerprint: fp, client: client}
		p.attach(tid, client)
		slog.Info("aprsis: connected under a tenant's own callsign",
			"tenant", tid, "callsign", client.FormatCallsign())
	}
}

// attach wires the inbound handler for one connection, closing over the tenant
// so an inbound message cannot be attributed to the wrong one. Caller holds mu.
func (p *ConnPool) attach(tenantID string, c *Client) {
	fn := p.onPacket
	if fn == nil {
		return
	}
	callsign := c.callsign
	c.SetPacketHandler(func(line string) { fn(tenantID, callsign, line) })
}

// Close disconnects every tenant connection the pool dialled. The platform
// client is left alone: the pool did not open it.
func (p *ConnPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for tid, c := range p.conns {
		if c.client != p.platform {
			c.client.Disconnect()
		}
		delete(p.conns, tid)
	}
}

// Tenants lists the tenants with a live connection, for logging and tests.
func (p *ConnPool) Tenants() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.conns))
	for tid := range p.conns {
		out = append(out, tid)
	}
	return out
}

// enabled defaults to TRUE for an empty value: the field carries Default "true",
// but a row written before the field existed, or by hand, has nothing there. A
// tenant who entered a callsign and a passcode has asked to transmit.
func enabled(v string) bool {
	switch v {
	case "", "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

func serverOrDefault(v string) string {
	if v == "" {
		return "rotate.aprs2.net:14580"
	}
	return v
}

// ssidOrDefault keeps an out-of-range SSID out of the packet builder. APRS
// allows 0-15; anything else would render a callsign that is not the tenant's.
func ssidOrDefault(v string) int {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > 15 {
		return 10
	}
	return n
}
