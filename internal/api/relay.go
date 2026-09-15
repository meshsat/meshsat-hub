package api

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/meshsat/meshsat-hub/internal/metrics"
	"github.com/meshsat/meshsat-hub/internal/relay"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
)

// RelayHandler is the HTTP face of the WebSocket relay (MESHSAT-612). Both
// ends are bridges of one tenant and authenticate with the MQTT credentials
// they already hold: HTTP Basic, username = bridge id, password checked with
// bcrypt against the stored hash. The tenant is the bridge's owner in the
// store, never anything the request says. These two routes are exempt from
// the user auth middleware (internal/auth isExempt) because a bridge has no
// user, JWT or API key to give it.
type RelayHandler struct {
	store   store.Store
	tenants *tenancy.Resolver
	relay   *relay.Relay
}

// NewRelayHandler wires the relay to the store that holds bridge credentials.
func NewRelayHandler(s store.Store, tenants *tenancy.Resolver, r *relay.Relay) *RelayHandler {
	return &RelayHandler{store: s, tenants: tenants, relay: r}
}

// dummyHash is compared against when the bridge does not exist or has no
// credentials, so an unknown id costs the caller the same bcrypt time as a
// wrong password and cannot be told apart by the clock.
var dummyHash = func() []byte {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	h, err := bcrypt.GenerateFromPassword([]byte(hex.EncodeToString(b)), bcrypt.DefaultCost)
	if err != nil {
		panic(err)
	}
	return h
}()

// authenticate returns the caller's tenant and bridge id, or false.
func (h *RelayHandler) authenticate(r *http.Request) (tenantID, bridgeID string, ok bool) {
	user, pass, ok := r.BasicAuth()
	if !ok || user == "" || pass == "" {
		return "", "", false
	}
	ctx := r.Context()
	tenant := h.tenants.ForBridge(ctx, user)
	// GetBridgeCredentials, not GetBridge: the latter deliberately leaves the
	// hash column out of its SELECT (it feeds JSON), so an auth check written
	// against it compares every password to the dummy and refuses everyone.
	c, err := h.store.GetBridgeCredentials(ctx, tenant, user)
	hash := dummyHash
	if err == nil && c != nil && c.Password != "" {
		hash = []byte(c.Password)
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(pass)) != nil || err != nil || c == nil || c.Password == "" {
		return "", "", false
	}
	return tenant, user, true
}

func (h *RelayHandler) unauthorized(w http.ResponseWriter) {
	metrics.RelayRejected.WithLabelValues("auth").Inc()
	w.Header().Set("WWW-Authenticate", `Basic realm="meshsat-relay"`)
	writeError(w, http.StatusUnauthorized, "bridge credentials required")
}

// Serve is the bridge end of the relay.
// @Summary      Serve this bridge's clients through the Hub (WebSocket)
// @Description  The bridge opens one outbound WebSocket and every client of it is multiplexed through the envelope [0x01][len][client_id][payload]. HTTP Basic with the bridge's own MQTT credentials. Binary frames only; the Hub never reads the payload.
// @Tags         relay
// @Security     BasicAuth
// @Success      101 "switching protocols"
// @Failure      401 {object} map[string]string
// @Router       /api/relay/serve [get]
func (h *RelayHandler) Serve(w http.ResponseWriter, r *http.Request) {
	tenantID, bridgeID, ok := h.authenticate(r)
	if !ok {
		h.unauthorized(w)
		return
	}
	h.relay.ServeBridge(w, r, tenantID, bridgeID)
}

// Connect is the client end of the relay.
// @Summary      Open a tunnel to one of your tenant's bridges (WebSocket)
// @Description  The caller is itself a bridge of the tenant (an Android device is a bridge of type android) and authenticates with its own MQTT credentials; its bridge id is the client id on the other end, never a parameter. The target must belong to the same tenant. Bare binary frames, 100 per minute per client.
// @Tags         relay
// @Security     BasicAuth
// @Param        bridge_id path string true "the bridge to reach"
// @Success      101 "switching protocols"
// @Failure      401 {object} map[string]string
// @Failure      403 {object} map[string]string "not a bridge of your tenant"
// @Failure      429 {object} map[string]string "budget exhausted for this minute"
// @Router       /api/relay/connect/{bridge_id} [get]
func (h *RelayHandler) Connect(w http.ResponseWriter, r *http.Request) {
	tenantID, callerID, ok := h.authenticate(r)
	if !ok {
		h.unauthorized(w)
		return
	}
	target := chi.URLParam(r, "bridge_id")
	if target == "" || target == callerID {
		writeError(w, http.StatusBadRequest, "bridge_id must name another bridge")
		return
	}
	// GetBridge is tenant-scoped: another tenant's bridge, or no bridge at
	// all, is the same answer, and the answer does not say which.
	if b, err := h.store.GetBridge(r.Context(), tenantID, target); err != nil || b == nil {
		metrics.RelayRejected.WithLabelValues("tenant").Inc()
		slog.Info("relay: connect refused, bridge is not the caller's tenant's", "tenant", tenantID, "caller", callerID)
		writeError(w, http.StatusForbidden, "not a bridge of your tenant")
		return
	}
	if !h.relay.Admit(tenantID, callerID) {
		writeError(w, http.StatusTooManyRequests, "relay budget exhausted for this minute")
		return
	}
	h.relay.ServeClient(w, r, tenantID, target, callerID)
}
