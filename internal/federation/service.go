package federation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/netguard"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/webhookroute"
)

// Accounts is the slice of the integrations service federation uses: the
// tenant's identity lives in its encrypted provider row.
type Accounts interface {
	ForTenant(ctx context.Context, tenantID, provider string) (*integrations.Account, error)
	Set(ctx context.Context, tenantID, provider string, fields map[string]string) (*integrations.Account, error)
}

// Store is the slice of the store federation uses.
type Store interface {
	GetFederationPeer(ctx context.Context, tenantID, id string) (*store.FederationPeer, error)
	GetFederationPeerByFingerprint(ctx context.Context, tenantID, fingerprint string) (*store.FederationPeer, error)
	HasFederationGrant(ctx context.Context, tenantID, peerID, fromIMEI, toIMEI string) (bool, error)
	GetDevice(ctx context.Context, tenantID string, imei string) (*store.Device, error)
	ClaimOnce(ctx context.Context, key string) (bool, error)
}

// MTSender sends a verbatim payload to one of the tenant's modems on that
// tenant's own account (cloudloop.Sender.SendDirect in production, after the
// same IMT check satellite_relay makes).
type MTSender func(tenantID, imei, wireB64, imtTopic string) error

// Auditor records a federation event on the tenant's audit chain.
type Auditor func(ctx context.Context, tenantID, action, detail string)

// Service holds a Hub's federation: identities, sending and receiving.
type Service struct {
	store     Store
	accounts  Accounts
	publicURL string
	send      MTSender
	audit     Auditor
	client    *http.Client
	checkURL  func(string) error
	now       func() time.Time
	// self is HUB_PUBLIC_URL taken apart, local the receiver a relay to a
	// tenant on this same Hub is handed to without leaving the process, and
	// owner the token lookup that says a token is one of this Hub's
	// (MESHSAT-1788). All nil until SetLocalReceiver is called.
	self       *selfURL
	local      http.Handler
	owner      TokenOwner
	localLimit *tenantLimiter
	// Counters for /metrics, read with Snapshot.
	sent, sendFailed, received, duplicate, refused, failed atomic.Uint64
}

// Stats are monotonic counters of relay outcomes on this replica.
type Stats struct {
	Sent, SendFailed                     uint64
	Received, Duplicate, Refused, Failed uint64
}

// New builds the service. publicURL is HUB_PUBLIC_URL, the base a peer
// reaches this Hub on.
func New(st Store, accounts Accounts, publicURL string, send MTSender, audit Auditor) *Service {
	client := netguard.SafeHTTPClient(10 * time.Second)
	// A peer may not redirect a relay, and its payload, to a third party.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Service{store: st, accounts: accounts, publicURL: strings.TrimRight(publicURL, "/"), send: send, audit: audit,
		client: client, checkURL: netguard.ValidatePublicURL, now: time.Now}
}

// WebhookPathPrefix is the path a tenant's relay token is appended to.
const WebhookPathPrefix = "/api/webhook/federation/"

// SetLocalReceiver lets two tenants on this Hub peer (MESHSAT-1788). A relay
// whose peer URL is this Hub's own (HUB_PUBLIC_URL) is not sent over the
// network: the hosted Hub's own name resolves to a private address inside
// the cluster, which the request-forgery guard rightly refuses, and lifting
// the guard for private addresses would open every URL a tenant can type to
// the cluster. Instead the SAME signed request is served in-process by a
// router that holds exactly one route, the receiving endpoint, behind
// resolve, which must be the middleware the public route puts in front of
// the service (webhookroute.Middleware for provider federation, field
// relay_token). Every check a peer on another Hub meets runs unchanged:
// token, pinned peer, addressee, signature, date, grant, device, claim.
// owner must answer from the same accounts: a URL is this Hub's only when its
// token names one of this Hub's tenants, so a Hub that left HUB_PUBLIC_URL at
// the hosted default still reaches a tenant on the hosted Hub over the
// network. Call once at boot, before any relay.
func (s *Service) SetLocalReceiver(resolve func(http.Handler) http.Handler, owner TokenOwner) {
	self := parseSelfURL(s.publicURL)
	if self == nil || resolve == nil || owner == nil {
		slog.Warn("federation: no same-Hub peering, HUB_PUBLIC_URL is not a bare https origin", "public_url", s.publicURL)
		return
	}
	r := chi.NewRouter()
	r.Method(http.MethodPost, WebhookPathPrefix+"{"+webhookroute.URLParam+"}", resolve(s))
	s.self, s.local, s.owner = self, r, owner
	s.localLimit = newTenantLimiter(LocalRelaysPerMinute, time.Minute)
	slog.Info("federation: same-Hub peering on", "host", self.host, "port", self.port)
}

// TokenOwner returns the tenant whose relay token this is, "" for none.
type TokenOwner func(ctx context.Context, token string) (string, error)

// LocalRelaysPerMinute bounds one sending tenant's in-process relays on one
// replica, as the public route bounds one address (60 a minute): a tenant's
// bridge can publish as many messages as it likes, and each fans out to
// every recipient of a route.
const LocalRelaysPerMinute = 60

// selfURL is the origin of HUB_PUBLIC_URL: scheme https, a lower-case ASCII
// host and an explicit port.
type selfURL struct{ host, port string }

// parseSelfURL accepts only a bare https origin. A public URL with a path
// (a Hub behind a path prefix) never matches, so its relays go out on the
// network as before.
func parseSelfURL(raw string) *selfURL {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.Path != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil
	}
	host, ok := asciiLower(u.Hostname())
	if !ok || host == "" {
		return nil
	}
	return &selfURL{host: host, port: httpsPort(u.Port())}
}

// relayTokenRE is the character set of a generated relay token (base64url,
// or hex), so a local relay URL carries nothing a router and a signer could
// read two ways: no escape, no dot, no slash.
var relayTokenRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

// IsLocalRelayURL reports whether a peer relay URL is this Hub's own receiving
// endpoint for one of its tenants, which the relay then serves in-process. It
// is strict on purpose: anything it does not recognise exactly, or a token
// no tenant here holds, is a URL on another Hub, which goes through the
// request-forgery guard like every tenant-typed URL.
func (s *Service) IsLocalRelayURL(ctx context.Context, raw string) bool {
	token, ok := s.localToken(raw)
	if !ok {
		return false
	}
	tenant, err := s.owner(ctx, token)
	if err != nil {
		slog.Warn("federation: relay token lookup failed, treating the URL as another Hub's", "error", err)
		return false
	}
	return tenant != ""
}

// localToken is the syntactic half of IsLocalRelayURL: the token, when the
// URL is exactly this Hub's receiving endpoint.
func (s *Service) localToken(raw string) (string, bool) {
	if s.local == nil || s.self == nil || s.owner == nil {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.RawPath != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return "", false
	}
	host, ok := asciiLower(u.Hostname())
	if !ok || host != s.self.host || httpsPort(u.Port()) != s.self.port {
		return "", false
	}
	token, ok := strings.CutPrefix(u.Path, WebhookPathPrefix)
	if !ok || !relayTokenRE.MatchString(token) {
		return "", false
	}
	return token, true
}

// asciiLower lower-cases an ASCII host and refuses anything else, so a
// Unicode case fold (the Kelvin sign is a "k" to strings.ToLower) never
// makes another name look like this Hub's.
func asciiLower(h string) (string, bool) {
	b := []byte(h)
	for i, c := range b {
		if c >= 0x80 {
			return "", false
		}
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b), true
}

func httpsPort(p string) string {
	if p == "" {
		return "443"
	}
	return p
}

// DisableNetguardForTest replaces the request-forgery-guarded client and URL
// check, which refuse the loopback address a test server listens on.
func (s *Service) DisableNetguardForTest(c *http.Client) {
	s.client = c
	s.checkURL = func(string) error { return nil }
}

// Snapshot returns the counters.
func (s *Service) Snapshot() Stats {
	return Stats{Sent: s.sent.Load(), SendFailed: s.sendFailed.Load(), Received: s.received.Load(),
		Duplicate: s.duplicate.Load(), Refused: s.refused.Load(), Failed: s.failed.Load()}
}

// ErrNoIdentity is returned by LoadIdentityFor before the tenant's owner has
// opened federation once.
var ErrNoIdentity = errors.New("federation: this tenant has no federation identity yet")

// LoadIdentityFor reads the tenant's identity and relay URL without creating
// anything. Sending and receiving use this: a tenant that never created an
// identity cannot have been pinned by anyone.
func (s *Service) LoadIdentityFor(ctx context.Context, tenantID string) (*Identity, string, error) {
	acct, err := s.accounts.ForTenant(ctx, tenantID, integrations.ProviderFederation)
	if err != nil {
		return nil, "", err
	}
	if acct == nil || acct.Get("private_key_pem") == "" || acct.Get("certificate_pem") == "" {
		return nil, "", ErrNoIdentity
	}
	id, err := LoadIdentity([]byte(acct.Get("private_key_pem")), []byte(acct.Get("certificate_pem")))
	if err != nil {
		return nil, "", err
	}
	return id, s.RelayURL(acct.Get("relay_token")), nil
}

// Identity returns the tenant's identity, creating it on first use. Only the
// owner's identity endpoint calls it. One replica creates (a claim per
// tenant and minute, so a creator that dies only blocks that minute); the
// other waits for what was stored, and both answer with the stored identity,
// never with a key they generated but did not keep.
func (s *Service) Identity(ctx context.Context, tenantID string) (*Identity, string, error) {
	id, url, err := s.LoadIdentityFor(ctx, tenantID)
	if !errors.Is(err, ErrNoIdentity) {
		return id, url, err
	}
	won, err := s.store.ClaimOnce(ctx, fmt.Sprintf("federation-identity:%s:%d", tenantID, s.now().Unix()/60))
	if err != nil {
		return nil, "", err
	}
	if won {
		fresh, err := NewIdentity(tenantID, s.now())
		if err != nil {
			return nil, "", err
		}
		keyPEM, err := fresh.KeyPEM()
		if err != nil {
			return nil, "", err
		}
		if _, err := s.accounts.Set(ctx, tenantID, integrations.ProviderFederation, map[string]string{
			"private_key_pem": string(keyPEM), "certificate_pem": string(fresh.CertPEM),
		}); err != nil {
			return nil, "", err
		}
		if s.audit != nil {
			s.audit(ctx, tenantID, "federation_identity_created", "fingerprint "+fresh.Fingerprint)
		}
	}
	for i := 0; i < 20; i++ {
		id, url, err = s.LoadIdentityFor(ctx, tenantID)
		if !errors.Is(err, ErrNoIdentity) {
			return id, url, err
		}
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return nil, "", ErrNoIdentity
}

// RelayURL is the capability URL a peer posts to for this token.
func (s *Service) RelayURL(token string) string {
	if token == "" || s.publicURL == "" {
		return ""
	}
	return s.publicURL + WebhookPathPrefix + token
}

// ---------------------------------------------------------------------------
// Sending
// ---------------------------------------------------------------------------

// Relay sends one verbatim satellite payload to a device on a peer. It is
// called by the route engine after the engine's own per-route dispatch
// claim, so it runs on one replica; msgID is stable across retries so the
// peer's claim drops a repeat.
func (s *Service) Relay(ctx context.Context, tenantID, peerID, fromIMEI, toIMEI, wireB64, imtTopic, msgID string) error {
	peer, err := s.store.GetFederationPeer(ctx, tenantID, peerID)
	if err != nil {
		return fmt.Errorf("federation: peer %s: %w", peerID, err)
	}
	// A tenant on this same Hub is served in-process (SetLocalReceiver);
	// every other peer URL is checked again here, because a name that was
	// public when the peer was added can resolve somewhere else now.
	local := s.IsLocalRelayURL(ctx, peer.RelayURL)
	if !local {
		if err := s.checkURL(peer.RelayURL); err != nil {
			return fmt.Errorf("federation: peer URL refused: %w", err)
		}
	}
	id, _, err := s.LoadIdentityFor(ctx, tenantID)
	if err != nil {
		return err
	}
	var last error
	for attempt, wait := range []time.Duration{0, 2 * time.Second, 5 * time.Second} {
		if wait > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		frame := RelayFrame{MsgID: msgID, Attempt: attempt + 1, FromIMEI: fromIMEI, ToIMEI: toIMEI, IMTTopic: imtTopic, WireB64: wireB64}
		if err := frame.Validate(); err != nil {
			return fmt.Errorf("federation: %w", err)
		}
		body, err := json.Marshal(frame)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, peer.RelayURL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		Sign(req, body, id, peerFingerprint(peer), s.now())
		var status int
		if local {
			status = s.serveLocal(ctx, tenantID, req)
		} else {
			resp, err := s.client.Do(req)
			if err != nil {
				last = err
				continue
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			status = resp.StatusCode
		}
		switch {
		case status == http.StatusAccepted || status == http.StatusOK:
			s.sent.Add(1)
			slog.Info("federation: relayed to peer", "tenant", tenantID, "peer", peer.ID, "to", toIMEI, "attempt", attempt+1, "same_hub", local)
			return nil
		case status >= 500 || status == http.StatusTooManyRequests:
			last = fmt.Errorf("peer answered %d", status)
			continue
		default:
			// 4xx: the peer refused (no peering, no grant, bad signature).
			// Retrying sends the same refusal.
			s.sendFailed.Add(1)
			return fmt.Errorf("federation: peer refused with %d", status)
		}
	}
	s.sendFailed.Add(1)
	return fmt.Errorf("federation: peer unreachable: %w", last)
}

// serveLocal hands a signed relay request to this Hub's own receiver and
// returns the status it answered. The receiver runs on a FRESH context that
// keeps the caller's deadline and cancellation but none of its values, so
// nothing of the sending tenant (its tenancy, a chi route context) reaches
// the receiving one: the receiver learns its tenant from the token, as it
// does for a request from the network. A panic answers 500, as the server's
// recoverer would, unless the receiver had already answered: a 202 written
// before a panic is a sent MT, and a retry would pay for a second one.
func (s *Service) serveLocal(ctx context.Context, senderTenant string, req *http.Request) (status int) {
	if !s.localLimit.allow(senderTenant, s.now()) {
		slog.Warn("federation: same-Hub relays over the limit", "tenant", senderTenant, "per_minute", LocalRelaysPerMinute)
		return http.StatusTooManyRequests
	}
	lctx, cancel := detachValues(ctx)
	defer cancel()
	in := req.Clone(lctx)
	in.RemoteAddr = "in-process"
	in.RequestURI = in.URL.RequestURI()
	w := &localResponse{header: http.Header{}}
	defer func() {
		if p := recover(); p != nil {
			slog.Error("federation: same-Hub receiver panicked", "panic", p, "answered", w.code, "stack", string(debug.Stack()))
			status = http.StatusInternalServerError
			if w.code != 0 {
				status = w.code
			}
		}
	}()
	s.local.ServeHTTP(w, in)
	return w.status()
}

// detachValues returns a context with parent's deadline and cancellation and
// none of its values.
func detachValues(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	release := cancel
	if dl, ok := parent.Deadline(); ok {
		var cancelDL context.CancelFunc
		ctx, cancelDL = context.WithDeadline(ctx, dl)
		release = func() { cancelDL(); cancel() }
	}
	stop := context.AfterFunc(parent, cancel)
	return ctx, func() { stop(); release() }
}

// localResponse is the ResponseWriter of an in-process relay: only the status
// matters to the sender, as on the network, where it reads 4 KB and drops it.
type localResponse struct {
	header http.Header
	code   int
}

func (l *localResponse) Header() http.Header { return l.header }

func (l *localResponse) WriteHeader(code int) {
	if l.code == 0 {
		l.code = code
	}
}

func (l *localResponse) Write(b []byte) (int, error) {
	l.WriteHeader(http.StatusOK)
	return len(b), nil
}

func (l *localResponse) status() int {
	if l.code == 0 {
		return http.StatusOK
	}
	return l.code
}

// tenantLimiter is a fixed window per key. In memory and per replica, like
// the public route's limiter: the route engine runs a dispatch on one replica.
type tenantLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	seen   map[string]*limitWindow
}

type limitWindow struct {
	n   int
	end time.Time
}

func newTenantLimiter(limit int, window time.Duration) *tenantLimiter {
	return &tenantLimiter{limit: limit, window: window, seen: map[string]*limitWindow{}}
}

func (l *tenantLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.seen) > 4096 {
		for k, w := range l.seen {
			if now.After(w.end) {
				delete(l.seen, k)
			}
		}
	}
	w := l.seen[key]
	if w == nil || now.After(w.end) {
		w = &limitWindow{end: now.Add(l.window)}
		l.seen[key] = w
	}
	if w.n >= l.limit {
		return false
	}
	w.n++
	return true
}

// peerFingerprint is the To value for a peer: its stored fingerprint.
func peerFingerprint(p *store.FederationPeer) string { return p.Fingerprint }

// MessageID derives the relay msg_id from the routed payload, the same bytes
// on every replica and on every retry.
func MessageID(payload []byte) string {
	sum := sha256.Sum256(payload)
	return "mo-" + hex.EncodeToString(sum[:16])
}

// ---------------------------------------------------------------------------
// Receiving
// ---------------------------------------------------------------------------

// maxRelayBody bounds a relay request: a 100 KB payload in base64 plus the
// frame fields.
const maxRelayBody = 160 * 1024

// ServeHTTP receives a relay at /api/webhook/federation/{token}. The token
// (resolved by internal/webhookroute into the request's tenant) only says
// WHICH tenant is addressed; what authorises the request is the signature
// of a peer that tenant's owner added. Every refusal before the signature
// check is a 404, so a scan learns nothing about peers or devices.
//
// @Summary Receive a federated satellite relay
// @Description Signed by a peer Hub (X-MeshSat-Federation-* headers) with the federation key its owner pinned. Sends the payload byte for byte to the named device on this tenant's own satellite account when a grant allows it.
// @Tags federation
// @Accept json
// @Produce json
// @Param token path string true "the tenant's relay token"
// @Param body body RelayFrame true "relay frame"
// @Success 202 {object} map[string]string
// @Success 200 {object} map[string]string "a duplicate, already sent"
// @Failure 400 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Failure 502 {object} map[string]string
// @Router /api/webhook/federation/{token} [post]
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := ""
	if res, ok := webhookroute.FromContext(ctx); ok && res != nil {
		tenantID = res.TenantID
	}
	if tenantID == "" {
		s.refuse(w, http.StatusNotFound, "not found", "no tenant for the path")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRelayBody))
	if err != nil {
		s.refuse(w, http.StatusBadRequest, "request body too large or unreadable", err.Error())
		return
	}
	peer, err := s.store.GetFederationPeerByFingerprint(ctx, tenantID, r.Header.Get(HeaderFrom))
	if err != nil {
		s.refuse(w, http.StatusNotFound, "not found", "sender is not a peer of the tenant")
		return
	}
	id, _, err := s.LoadIdentityFor(ctx, tenantID)
	if err != nil || id.Fingerprint != r.Header.Get(HeaderTo) {
		s.refuse(w, http.StatusNotFound, "not found", "request addressed to another identity")
		return
	}
	// A bad signature answers like an unknown sender, so holding the URL
	// does not tell anybody which fingerprints are peers.
	_, pub, err := ParsePeerCert([]byte(peer.CertPEM))
	if err != nil {
		s.refuse(w, http.StatusNotFound, "not found", "peer certificate unusable: "+err.Error())
		return
	}
	if err := Verify(r, raw, pub, s.now()); err != nil {
		s.refuse(w, http.StatusNotFound, "not found", "signature refused: "+err.Error())
		return
	}
	var frame RelayFrame
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&frame); err != nil || dec.More() {
		s.refuse(w, http.StatusBadRequest, "malformed relay frame", "decode")
		return
	}
	if err := frame.Validate(); err != nil {
		s.refuse(w, http.StatusBadRequest, err.Error(), "validate")
		return
	}
	ok, err := s.store.HasFederationGrant(ctx, tenantID, peer.ID, frame.FromIMEI, frame.ToIMEI)
	if err != nil {
		s.refuse(w, http.StatusServiceUnavailable, "try again", err.Error())
		return
	}
	if !ok {
		s.refuse(w, http.StatusForbidden, "no grant for this sender and device", "grant")
		return
	}
	if _, err := s.store.GetDevice(ctx, tenantID, frame.ToIMEI); err != nil {
		s.refuse(w, http.StatusForbidden, "no grant for this sender and device", "device not in tenant")
		return
	}
	// Once per attempt across replicas: a replay of an attempt sends nothing,
	// a retry after a failed send does (see RelayFrame.Attempt). Without the
	// claim table nothing is sent.
	won, err := s.store.ClaimOnce(ctx, fmt.Sprintf("federation:%s:%s:%s:%d", tenantID, peer.ID, frame.MsgID, frame.Attempt))
	if err != nil {
		s.refuse(w, http.StatusServiceUnavailable, "try again", err.Error())
		return
	}
	if !won {
		s.duplicate.Add(1)
		writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate"})
		return
	}
	if err := s.send(tenantID, frame.ToIMEI, frame.WireB64, frame.IMTTopic); err != nil {
		s.failed.Add(1)
		slog.Error("federation: MT send failed", "tenant", tenantID, "peer", peer.ID, "to", frame.ToIMEI, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "satellite send failed"})
		return
	}
	s.received.Add(1)
	if s.audit != nil {
		s.audit(ctx, tenantID, "federation_relay_received", fmt.Sprintf("peer %s from %s to %s", peer.ID, frame.FromIMEI, frame.ToIMEI))
	}
	slog.Info("federation: relay received and sent", "tenant", tenantID, "peer", peer.ID, "from", frame.FromIMEI, "to", frame.ToIMEI)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "sent"})
}

func (s *Service) refuse(w http.ResponseWriter, status int, public, reason string) {
	s.refused.Add(1)
	slog.Warn("federation: relay refused", "status", status, "reason", reason)
	writeJSON(w, status, map[string]string{"error": public})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
