package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/meshsat/meshsat-hub/internal/audit"
	"github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/bridge"
	"github.com/meshsat/meshsat-hub/internal/directory"
	"github.com/meshsat/meshsat-hub/internal/store"
	qrcode "github.com/skip2/go-qrcode"
	"golang.org/x/crypto/bcrypt"
)

// ProvisionBundle contains everything a bridge/Android app needs to connect.
// Returned by the nonce-authenticated claim endpoint, NOT embedded in the QR.
type ProvisionBundle struct {
	Version             string `json:"v"`                 // "1"
	BridgeID            string `json:"bid"`               // bridge identifier
	MQTTURL             string `json:"mqtt"`              // wss://mqtt-hub.meshsat.net/mqtt
	MQTTTopicPrefix     string `json:"mqtt_topic_prefix"` // "meshsat" (default tenant) or "meshsat/{tenant}"; bridge publishes {prefix}/bridge/{id}/... and {prefix}/{device}/...
	Username            string `json:"user"`              // MQTT username (always "meshsat" for NATS auth)
	Password            string `json:"pass"`              // MQTT password (shared NATS auth password)
	CertPEM             string `json:"cert"`              // client TLS certificate
	KeyPEM              string `json:"key"`               // client TLS private key (one-time)
	CaPEM               string `json:"ca"`                // CA certificate
	CertExpires         string `json:"cert_exp"`          // certificate expiry (RFC3339)
	ReticulumTCP        string `json:"ret_tcp"`           // Reticulum TCP peer
	DirectorySigningPub []byte `json:"dir_sign_pub"`      // Hub's ECDSA-P256 directory-signing pubkey (PKIX DER) — bridge pins on first provision [MESHSAT-539]
}

// provisionStash holds pre-generated credentials waiting to be claimed.
// Stored as JSON in system_config under provisionStashKey(tenant, bridge).
//
// Until MESHSAT-1336 the credentials were written to the bridge row, and so
// to the broker, the moment the QR was generated. A kit that was connected
// when its operator opened "Show setup QR" was dropped by the broker's reload
// about half a minute later and refused on every reconnect with its old
// password, whether or not anybody ever scanned the code (24 Sep 2026: the
// first iPhone kit, dismissed QR, kit offline until re-provisioned). So the
// stash now carries everything the bridge row will need, and the row is
// written only when the bundle is CLAIMED. A QR nobody scans changes nothing.
type provisionStash struct {
	Nonce     string          `json:"nonce"`
	Bundle    ProvisionBundle `json:"bundle"`
	CreatedAt time.Time       `json:"created_at"`
	// PasswordHash is the bcrypt of Bundle.Password, written to the bridge
	// row on install. Empty on a stash from before MESHSAT-1336, which was
	// installed at generation and needs nothing.
	PasswordHash string `json:"password_hash,omitempty"`
	// Installed is set once the bridge row and the broker carry these
	// credentials, so a claim that is held on the broker (503, retried with
	// the same nonce) does not rewrite them on every attempt.
	Installed bool `json:"installed,omitempty"`
}

// installStash writes the stash's credentials to the bridge row and asks the
// broker to load them: the moment the kit's OLD login stops working. Called
// on the first claim, and by the direct endpoint that hands the bundle out
// inline. Idempotent: a stash already installed is left alone. A stash from
// before MESHSAT-1336 (no PasswordHash) was installed when it was generated.
func (h *BridgeProvisionHandler) installStash(ctx context.Context, key, tid, id string, stash *provisionStash) error {
	if stash.Installed || stash.PasswordHash == "" {
		return nil
	}
	if err := h.store.SetBridgeCredentials(ctx, tid, id, stash.Bundle.Username, stash.PasswordHash); err != nil {
		return fmt.Errorf("store credentials: %w", err)
	}
	expiry, err := time.Parse(time.RFC3339, stash.Bundle.CertExpires)
	if err != nil {
		return fmt.Errorf("stash certificate expiry: %w", err)
	}
	if err := h.store.SetBridgeCertificate(ctx, tid, id, stash.Bundle.CertPEM, expiry); err != nil {
		return fmt.Errorf("store certificate: %w", err)
	}
	// NATS MQTT auth: every bridge gets its own NATS user (its bridge ID) whose
	// bcrypt hash and permissions the Hub renders into the NATS users file
	// (MESHSAT-864 MR 21). Identity is confirmed by the mTLS certificate CN.
	if h.natsAuth != nil {
		h.natsAuth.Trigger()
	}
	stash.Installed = true
	raw, err := json.Marshal(stash)
	if err != nil {
		return fmt.Errorf("marshal stash: %w", err)
	}
	// The row is written and the broker kicked; failing to record that only
	// means the next retry writes the same hash and cert again. Not fatal.
	if err := h.store.SetSystemConfig(ctx, key, string(raw)); err != nil {
		slog.Warn("provision: credentials installed but the stash could not record it; a retry reinstalls the same ones",
			"bridge_id", id, "error", err)
	}
	return nil
}

// provisionStashPrefix starts every stash key; the stash reaper sweeps it.
const provisionStashPrefix = "provision_stash:"

// provisionStashKey names a bridge's stash by its tenant AND its id
// (MESHSAT-1303). It used to be the id alone, but a bridge id is unique only
// within a tenant, so two tenants with a bridge of the same name shared one
// row: one tenant's new QR silently killed the other's. Neither id can contain
// ':' (bridge ids are [A-Za-z0-9._-]), so the key is unambiguous.
func provisionStashKey(tenantID, bridgeID string) string {
	return provisionStashPrefix + tenantID + ":" + bridgeID
}

// legacyProvisionStashKey is the pre-MESHSAT-1303 key. A claim still reads
// it, so a QR issued before the change keeps working until it expires;
// nothing writes it any more and the reaper removes what is left.
func legacyProvisionStashKey(bridgeID string) string { return provisionStashPrefix + bridgeID }

// findStash returns the stash a claim for (bridgeID, nonce) refers to, and its
// key. The claim is unauthenticated, so it cannot know the tenant: every
// tenant's stash for that bridge id is a candidate, and the nonce picks one,
// compared in constant time. Not found is ("", nil, nil).
func (h *BridgeProvisionHandler) findStash(ctx context.Context, bridgeID, nonce string) (string, *provisionStash, error) {
	keys, err := h.store.ListSystemConfigOlderThan(ctx, provisionStashPrefix, time.Now().Add(time.Hour))
	if err != nil {
		return "", nil, err
	}
	candidates := []string{}
	for _, k := range keys {
		if strings.HasSuffix(k, ":"+bridgeID) || k == legacyProvisionStashKey(bridgeID) {
			candidates = append(candidates, k)
		}
	}
	for _, k := range candidates {
		raw, err := h.store.GetSystemConfig(ctx, k)
		if err != nil || raw == "" {
			continue
		}
		var stash provisionStash
		if err := json.Unmarshal([]byte(raw), &stash); err != nil {
			continue
		}
		if stash.Bundle.BridgeID != bridgeID && stash.Bundle.BridgeID != "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(stash.Nonce), []byte(nonce)) == 1 {
			return k, &stash, nil
		}
	}
	return "", nil, nil
}

// BridgeProvisionHandler provides QR-based provisioning.
type BridgeProvisionHandler struct {
	store       store.Store
	ca          *bridge.CertAuthority
	trustAnchor *directory.TrustAnchor
	natsAuth    bridge.Resyncer // nil outside Kubernetes
	prober      CredentialChecker
	audit       *audit.Service // nil = no audit (tests)
}

// SetAudit makes handing out a provisioning bundle (directly, as a QR, or on a
// claim) an audit event (MESHSAT-1308): each one mints the bridge's password
// and private key.
func (h *BridgeProvisionHandler) SetAudit(a *audit.Service) { h.audit = a }

// stashTenant is the tenant a stash key belongs to, for the audit record of an
// unauthenticated claim. A pre-MESHSAT-1303 key carries none; the bridge's
// owner stands in.
func (h *BridgeProvisionHandler) stashTenant(ctx context.Context, key, bridgeID string) string {
	rest := strings.TrimPrefix(key, provisionStashPrefix)
	if i := strings.IndexByte(rest, ':'); i > 0 {
		return rest[:i]
	}
	if t, err := h.store.LookupBridgeTenant(ctx, bridgeID); err == nil {
		return t
	}
	return store.DefaultTenantID
}

// CredentialChecker reports whether every broker member accepts a user and
// password yet (bridge.CredentialProber).
type CredentialChecker interface {
	Live(ctx context.Context, user, pass string) (bool, []bridge.ProbeResult, error)
}

// SetProber makes a claim wait until the broker accepts the bundle's
// credentials (MESHSAT-1298). Without one, a bundle is handed out at once, as
// before.
func (h *BridgeProvisionHandler) SetProber(p CredentialChecker) { h.prober = p }

// claimRetryAfter is what a client is told to wait when its bundle is not live
// on the broker yet. The members took 4 to 53 s after a rotation on 21 Sep
// 2026, so a few polls cover it.
const claimRetryAfter = 5

// SetNATSAuth registers the NATS auth syncer to kick after provisioning.
func (h *BridgeProvisionHandler) SetNATSAuth(r *bridge.NATSAuthSyncer) {
	if r != nil {
		h.natsAuth = r
	}
}

// NewBridgeProvisionHandler returns a handler that stashes credentials for
// nonce-authenticated claim. trustAnchor supplies the Hub's directory-signing
// pubkey published in the bundle (MESHSAT-539); it may be nil in test setups.
func NewBridgeProvisionHandler(s store.Store, ca *bridge.CertAuthority, trustAnchor *directory.TrustAnchor) *BridgeProvisionHandler {
	return &BridgeProvisionHandler{store: s, ca: ca, trustAnchor: trustAnchor}
}

// generateAndStash creates fresh credentials, stores them in a stash keyed
// by nonce, and returns the nonce. The full bundle is claimed via ClaimProvision.
//
// Nothing here touches the bridge row or the broker (MESHSAT-1336): the kit
// keeps the login it has until installStash runs, on the claim.
func (h *BridgeProvisionHandler) generateAndStash(r *http.Request, id, tid string) (string, error) {
	// Generate single-use nonce (16 random bytes = 32 hex chars for security).
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	nonce := hex.EncodeToString(nonceBytes)

	// Generate MQTT password.
	passwordBytes := make([]byte, 32)
	if _, err := rand.Read(passwordBytes); err != nil {
		return "", fmt.Errorf("generate password: %w", err)
	}
	password := hex.EncodeToString(passwordBytes)

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}

	username := id

	mqttURL, _ := h.store.GetSystemConfig(r.Context(), mqttPublicURLKey)
	if mqttURL == "" {
		mqttURL = os.Getenv("MESHSAT_MQTT_PUBLIC_URL")
	}
	if mqttURL == "" {
		return "", fmt.Errorf("MQTT public URL not configured")
	}

	if h.ca == nil {
		return "", fmt.Errorf("certificate authority not configured")
	}

	certPEM, keyPEM, err := h.ca.IssueBridgeCert(id, 90)
	if err != nil {
		return "", fmt.Errorf("issue certificate: %w", err)
	}

	expiry := time.Now().Add(90 * 24 * time.Hour)

	retTCP := os.Getenv("MESHSAT_RETICULUM_PUBLIC_TCP")
	if retTCP == "" {
		retTCP = "reticulum.meshsat.net:443"
	}

	var dirSignPub []byte
	if h.trustAnchor != nil {
		dirSignPub = h.trustAnchor.PublicKey()
	}

	stash := provisionStash{
		Nonce: nonce,
		Bundle: ProvisionBundle{
			Version:             "1",
			BridgeID:            id,
			MQTTURL:             mqttURL,
			MQTTTopicPrefix:     hubmqtt.Namespace(tid),
			Username:            username,
			Password:            password,
			CertPEM:             string(certPEM),
			KeyPEM:              string(keyPEM),
			CaPEM:               string(h.ca.CACertPEM()),
			CertExpires:         expiry.Format(time.RFC3339),
			ReticulumTCP:        retTCP,
			DirectorySigningPub: dirSignPub,
		},
		CreatedAt:    time.Now(),
		PasswordHash: string(hash),
	}

	stashJSON, err := json.Marshal(stash)
	if err != nil {
		return "", fmt.Errorf("marshal stash: %w", err)
	}

	// Store stash — overwrites any previous (invalidates old QRs).
	stashKey := provisionStashKey(tid, id)
	if err := h.store.SetSystemConfig(r.Context(), stashKey, string(stashJSON)); err != nil {
		return "", fmt.Errorf("store stash: %w", err)
	}

	return nonce, nil
}

// Provision generates credentials and returns the full bundle (authenticated endpoint).
// @Summary One-step bridge provisioning
// @Tags bridges
// @Produce json
// @Param id path string true "Bridge ID"
// @Success 200 {object} ProvisionBundle
// @Router /api/bridges/{id}/provision [post]
func (h *BridgeProvisionHandler) Provision(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tid := auth.TenantIDFromContext(r.Context())

	if _, err := h.store.GetBridge(r.Context(), tid, id); err != nil {
		writeError(w, http.StatusNotFound, "bridge not found")
		return
	}

	nonce, err := h.generateAndStash(r, id, tid)
	if err != nil {
		slog.Error("bridge provision failed", "bridge_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "provisioning failed")
		return
	}

	// For the direct API, return the full bundle immediately.
	stashKey := provisionStashKey(tid, id)
	stashJSON, err := h.store.GetSystemConfig(r.Context(), stashKey)
	if err != nil || stashJSON == "" {
		writeError(w, http.StatusInternalServerError, "stash not found")
		return
	}

	var stash provisionStash
	if err := json.Unmarshal([]byte(stashJSON), &stash); err != nil {
		writeError(w, http.StatusInternalServerError, "corrupt stash")
		return
	}

	// The bundle leaves in this response, so the row and the broker take the
	// new login now; the caller asked for it and is holding it.
	if err := h.installStash(r.Context(), stashKey, tid, id, &stash); err != nil {
		slog.Error("bridge provision failed", "bridge_id", id, "error", err)
		_ = h.store.SetSystemConfig(r.Context(), stashKey, "")
		writeError(w, http.StatusInternalServerError, "provisioning failed")
		return
	}

	// Blank it, exactly as a claim does. The bundle has just been handed to the
	// caller in this response, so the stash has served its purpose -- and it
	// carries the plaintext MQTT password and the client PRIVATE KEY, which
	// exist nowhere else. Leaving it behind is how the two production field
	// kits kept theirs in system_config, and therefore in every unencrypted
	// barman backup, from March to September (MESHSAT-1098).
	if err := h.store.SetSystemConfig(r.Context(), stashKey, ""); err != nil {
		// Loud, not silent: the credential is out and the row is still there.
		slog.Error("bridge provision: could not clear the stash after handing out the bundle; "+
			"it holds a plaintext key and this needs a person", "bridge_id", id, "error", err)
	}

	slog.Info("bridge provisioned (direct)", "bridge_id", id, "nonce", nonce[:8])
	auditRequest(h.audit, r, tid, "bridge_provision_bundle_issued", "bridge="+id)
	writeJSON(w, http.StatusOK, stash.Bundle)
}

// ProvisionQR renders a QR code containing a short provisioning URL.
// The QR encodes: meshsat://provision/{bridge_id}/{nonce}?hub={hub_host}
// The app scans this, then fetches the full bundle from the Hub via the
// ClaimProvision endpoint (no auth needed — the nonce IS the auth).
// @Summary Generate provisioning QR code
// @Tags bridges
// @Produce image/png
// @Param id path string true "Bridge ID"
// @Param size query int false "QR code size in pixels (default 512)"
// @Success 200 {file} image/png
// @Router /api/bridges/{id}/provision/qr [post]
func (h *BridgeProvisionHandler) ProvisionQR(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tid := auth.TenantIDFromContext(r.Context())

	if _, err := h.store.GetBridge(r.Context(), tid, id); err != nil {
		writeError(w, http.StatusNotFound, "bridge not found")
		return
	}

	nonce, err := h.generateAndStash(r, id, tid)
	if err != nil {
		slog.Error("bridge provision QR failed", "bridge_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "provisioning failed")
		return
	}

	// Hub public hostname for the claim URL.
	hubHost := os.Getenv("MESHSAT_HUB_PUBLIC_HOST")
	if hubHost == "" {
		hubHost = r.Host // fallback to request host
	}
	if hubHost == "" {
		hubHost = "hub.meshsat.net"
	}

	// QR content: ~100 bytes, fits easily in any QR code.
	// Format: meshsat://provision/{bridge_id}/{nonce}?hub={host}
	qrContent := fmt.Sprintf("meshsat://provision/%s/%s?hub=%s", id, nonce, hubHost)

	// QR code size.
	size := 512
	if s := r.URL.Query().Get("size"); s != "" {
		n := 0
		for _, c := range s {
			if c >= '0' && c <= '9' {
				n = n*10 + int(c-'0')
			}
		}
		if n >= 128 && n <= 2048 {
			size = n
		}
	}

	png, err := qrcode.Encode(qrContent, qrcode.Medium, size)
	if err != nil {
		slog.Error("failed to generate QR code", "bridge_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "QR code generation failed")
		return
	}

	auditRequest(h.audit, r, tid, "bridge_provision_qr_generated", "bridge="+id)
	slog.Info("provision QR generated",
		"bridge_id", id,
		"nonce", nonce[:8]+"...",
		"qr_content_len", len(qrContent),
		"qr_size", size)

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=\"meshsat-provision-%s.png\"", id))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(png)
}

// ClaimProvision is the unauthenticated endpoint that the Android app calls
// after scanning the QR code. The nonce acts as a single-use bearer token.
// Returns the full ProvisionBundle and deletes the stash (one-time use).
// @Summary Claim provisioning bundle (no auth, nonce-authenticated)
// @Tags bridges
// @Produce json
// @Param id path string true "Bridge ID"
// @Param nonce path string true "Single-use provisioning nonce"
// @Success 200 {object} ProvisionBundle
// @Failure 404 {object} map[string]string "Invalid or expired nonce"
// @Failure 503 {object} map[string]interface{} "The broker does not accept the bundle's credentials yet; retry after Retry-After seconds with the same nonce"
// @Router /api/bridges/{id}/provision/{nonce} [get]
// provisionClaimRefused is the single answer to every failed claim: unknown
// bridge, wrong nonce, expired stash. One string, so the response cannot be
// used to tell those cases apart.
const provisionClaimRefused = "invalid or expired provisioning token"

// ProvisionTTL is how long a provisioning bundle may be claimed for.
//
// Named rather than written inline at the one place it is enforced, which is
// why nothing tested expiry: a bare `30*time.Minute` in the handler cannot be
// reached from a test, so the stash-expiry path had no coverage at all. The TAK
// enrolment path's enrolTTL is the precedent.
//
// ⚠ This bounds when a bundle can be CLAIMED. It does not bound how long the
// row survives -- an unclaimed stash outlived its TTL by six months before the
// reaper in internal/stashreaper existed (MESHSAT-1098).
const ProvisionTTL = 30 * time.Minute

func (h *BridgeProvisionHandler) ClaimProvision(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	nonce := chi.URLParam(r, "nonce")

	// Uniform 404 for every miss, deliberately: an unknown bridge, no stash
	// and a wrong nonce look the same, or an unauthenticated caller could
	// enumerate bridge ids and learn which have a provisioning session in
	// flight. internal/webhookroute answers the same way for the same reason.
	stashKey, found, err := h.findStash(r.Context(), id, nonce)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "provision lookup failed")
		return
	}
	if found == nil {
		// The expected nonce is never logged, not even a prefix: while the
		// stash is claimable it IS the credential.
		slog.Warn("provision claim: no stash matches", "bridge_id", id, "got", nonce[:min(8, len(nonce))]+"...")
		writeError(w, http.StatusNotFound, provisionClaimRefused)
		return
	}
	stash := *found

	// Check age — reject if older than 30 minutes.
	if time.Since(stash.CreatedAt) > ProvisionTTL {
		// Clean up expired stash.
		_ = h.store.SetSystemConfig(r.Context(), stashKey, "")
		writeError(w, http.StatusGone, "provisioning token expired")
		return
	}

	// The scan is the moment the kit's old login gives way to the new one
	// (MESHSAT-1336): write the row and kick the broker now, once. A stash
	// that is never claimed never gets here, and the kit keeps working.
	if err := h.installStash(r.Context(), stashKey, h.stashTenant(r.Context(), stashKey, id), id, &stash); err != nil {
		slog.Error("provision claim: could not install the credentials; the stash is kept for a retry",
			"bridge_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "provisioning failed")
		return
	}

	// Not before the broker accepts the credentials (MESHSAT-1298). A new
	// password reaches each NATS member through the kubelet's Secret sync and a
	// reload, up to a minute after it was installed; a client that connected in
	// between was refused with the right password, and one that did not retry
	// stayed offline. So the claim says "not yet" and KEEPS the stash: the same
	// nonce works a few seconds later. It holds only on an actual refusal from a
	// member. If the probe itself cannot run, the bundle is handed out as it was
	// before, because blocking every provisioning on the prober's health would be worse.
	if h.prober != nil {
		live, res, err := h.prober.Live(r.Context(), stash.Bundle.Username, stash.Bundle.Password)
		switch {
		case err != nil || len(res) == 0:
			slog.Warn("provision claim: could not check the credentials at the broker; handing the bundle out unchecked",
				"bridge_id", id, "error", err)
		case !live:
			accepted := 0
			for _, m := range res {
				if m.Accepted {
					accepted++
				}
			}
			slog.Info("provision claim held: the broker does not accept these credentials yet",
				"bridge_id", id, "accepted", accepted, "members", len(res))
			w.Header().Set("Retry-After", strconv.Itoa(claimRetryAfter))
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error":       "the broker has not accepted these credentials yet; retry",
				"retry_after": claimRetryAfter,
				"accepted":    accepted,
				"members":     len(res),
			})
			return
		}
	}

	// Delete the stash — single use.
	_ = h.store.SetSystemConfig(r.Context(), stashKey, "")

	// The claimer has no Hub account: the record says so, and carries the
	// client address, which is all there is to know about who scanned.
	if h.audit != nil {
		if err := h.audit.Log(r.Context(), h.stashTenant(r.Context(), stashKey, id), "bridge_provision_claimed", "provisioning-claim",
			fmt.Sprintf("bridge=%s age_sec=%d", id, int(time.Since(stash.CreatedAt).Seconds())), clientIPFromRequest(r)); err != nil {
			slog.Warn("audit: failed to log bridge_provision_claimed", "error", err)
		}
	}
	slog.Info("provision claimed",
		"bridge_id", id,
		"nonce", nonce[:8]+"...",
		"age_sec", int(time.Since(stash.CreatedAt).Seconds()))

	writeJSON(w, http.StatusOK, stash.Bundle)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// provisionStatus is what the Fleet page polls after it shows a QR.
//
// Counts only: a member's address is the cluster's business, and every
// tenant's owner can call this.
type provisionStatus struct {
	State    string `json:"state"`    // ready (not scanned yet), pending (scanned, broker loading it), live, none (claimed or never generated), expired
	Accepted int    `json:"accepted"` // broker members that accept the credentials
	Members  int    `json:"members"`  // broker members asked
	Checked  bool   `json:"checked"`  // false when no prober is configured or it could not run
	AgeSec   int    `json:"age_sec,omitempty"`
}

// ProvisionStatus reports where the bridge's unclaimed provisioning bundle
// stands. Until it is scanned it is `ready`: the kit's current login is
// untouched and there is nothing to ask the broker (MESHSAT-1336). Once a
// claim has installed it, the broker is asked member by member (MESHSAT-1298)
// so the Fleet page can show the scan being taken up, and the app's own
// claim retry is what carries the kit through the reload.
// @Summary Whether a provisioning bundle's credentials are live on the broker
// @Tags bridges
// @Produce json
// @Param id path string true "Bridge ID"
// @Success 200 {object} provisionStatus
// @Failure 404 {object} map[string]string
// @Router /api/bridges/{id}/provision/status [get]
func (h *BridgeProvisionHandler) ProvisionStatus(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tid := auth.TenantIDFromContext(r.Context())
	if _, err := h.store.GetBridge(r.Context(), tid, id); err != nil {
		writeError(w, http.StatusNotFound, "bridge not found")
		return
	}
	raw, err := h.store.GetSystemConfig(r.Context(), provisionStashKey(tid, id))
	if err != nil || raw == "" {
		writeJSON(w, http.StatusOK, provisionStatus{State: "none"})
		return
	}
	var stash provisionStash
	if err := json.Unmarshal([]byte(raw), &stash); err != nil {
		writeError(w, http.StatusInternalServerError, "corrupt provision data")
		return
	}
	st := provisionStatus{AgeSec: int(time.Since(stash.CreatedAt).Seconds())}
	if time.Since(stash.CreatedAt) > ProvisionTTL {
		st.State = "expired"
		writeJSON(w, http.StatusOK, st)
		return
	}
	if !stash.Installed && stash.PasswordHash != "" {
		st.State = "ready"
		writeJSON(w, http.StatusOK, st)
		return
	}
	if h.prober == nil {
		st.State = "live"
		writeJSON(w, http.StatusOK, st)
		return
	}
	live, res, err := h.prober.Live(r.Context(), stash.Bundle.Username, stash.Bundle.Password)
	st.Checked, st.Members = err == nil && len(res) > 0, len(res)
	for _, m := range res {
		if m.Accepted {
			st.Accepted++
		}
	}
	switch {
	case !st.Checked:
		st.State = "live" // as the claim does: an unchecked bundle is not held
	case live:
		st.State = "live"
	default:
		st.State = "pending"
	}
	writeJSON(w, http.StatusOK, st)
}
