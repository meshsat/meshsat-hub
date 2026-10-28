package api

import (
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/meshsat/meshsat-hub/internal/audit"
	"github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/warnings"
)

// WarningsHandler is the console's view of the last-mile warning relay
// (MESHSAT-1734): the warnings that reached the tenant's kits, the key the
// kits verify them with, and an owner's test injection.
type WarningsHandler struct {
	store store.Store
	svc   *warnings.Service
	audit *audit.Service
}

// NewWarningsHandler wires the endpoints. svc may be nil (the relay off).
func NewWarningsHandler(s store.Store, svc *warnings.Service, a *audit.Service) *WarningsHandler {
	return &WarningsHandler{store: s, svc: svc, audit: a}
}

// maxTestCAPBytes bounds a test injection's body.
const maxTestCAPBytes = 256 << 10

// warningsKey is the body of GET /api/warnings/key.
type warningsKey struct {
	// Enabled is whether the relay runs on this Hub.
	Enabled bool `json:"enabled"`
	// Alg is the signature scheme of a digest: ES256, r and s raw.
	Alg string `json:"alg,omitempty"`
	// PKIXHex is the public key, PKIX DER in hex; the key the kits pin as
	// the Hub's directory key.
	PKIXHex string `json:"pkix_hex,omitempty"`
}

// List lists the warnings relayed to the tenant.
// @Summary List relayed warnings
// @Description The official public warnings that matched this workspace's areas or kits and were handed to its kits, newest first, with which kits got them and on which bearers. Empty while the relay is off or nothing has matched.
// @Tags warnings
// @Produce json
// @Param limit query int false "Most rows to answer (default 100, max 500)"
// @Success 200 {array} store.TenantWarning
// @Failure 500 {object} map[string]string
// @Router /api/warnings [get]
func (h *WarningsHandler) List(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	rows, err := h.store.ListTenantWarnings(r.Context(), tid, parseLimit(r, 100, 500))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list warnings")
		return
	}
	if rows == nil {
		rows = []store.TenantWarning{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// Key answers the key the kits verify warning digests with.
// @Summary The warning relay's signing key
// @Description The public key every warning digest is signed with: the Hub's directory key, which a kit pins at provisioning (docs/warning-relay.md). PKIX DER in hex; signatures are ECDSA P-256 over SHA-256 with r and s raw.
// @Tags warnings
// @Produce json
// @Success 200 {object} warningsKey
// @Router /api/warnings/key [get]
func (h *WarningsHandler) Key(w http.ResponseWriter, r *http.Request) {
	out := warningsKey{}
	if h.svc != nil {
		if pub := h.svc.PublicKey(); len(pub) > 0 {
			out.Enabled, out.Alg, out.PKIXHex = true, "ES256", hex.EncodeToString(pub)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// InjectTest runs a CAP document through the relay for this workspace.
// @Summary Inject a test warning
// @Description Runs one CAP 1.2 document, posted as the body, through the same parse, match, sign and deliver path a feed's message takes, for this workspace alone. The document must carry status Test or Exercise; the kits mark it as a test on every bearer. The answer says which kits matched and what each was handed. Owner only: it transmits on the workspace's radios, and over satellite on its own account when that is on.
// @Tags warnings
// @Accept xml
// @Produce json
// @Param body body string true "A CAP 1.2 alert with status Test or Exercise"
// @Success 200 {object} warnings.InjectResult
// @Failure 400 {object} map[string]string "Not a CAP 1.2 alert, or not a test"
// @Failure 404 {object} map[string]string "The relay is off on this Hub"
// @Failure 413 {object} map[string]string "Body over 256 KB"
// @Router /api/warnings/test [post]
func (h *WarningsHandler) InjectTest(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		writeError(w, http.StatusNotFound, "the warning relay is off on this Hub")
		return
	}
	tid := auth.TenantIDFromContext(r.Context())
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxTestCAPBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body over 256 KB")
		return
	}
	res, err := h.svc.Inject(r.Context(), tid, body)
	switch {
	case errors.Is(err, warnings.ErrNotTest), errors.Is(err, warnings.ErrNotCAP):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not run the test")
		return
	}
	if h.audit != nil {
		kits := 0
		for _, d := range res.Deliveries {
			kits += len(d.Bridges)
		}
		_ = h.audit.Log(r.Context(), tid, "warning_test_injected", operatorEmail(r),
			"cap "+res.Identifier+" "+res.MsgType+" to "+strconv.Itoa(kits)+" kit(s)", clientIPFromRequest(r))
	}
	writeJSON(w, http.StatusOK, res)
}
