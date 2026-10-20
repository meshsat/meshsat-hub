package api

import (
	"net/http"

	"github.com/meshsat/meshsat-hub/internal/directory"
)

// DirectoryKeyHandler serves the Hub's current directory signing key, the
// public half, to bridges (MESHSAT-1584 C7). A bridge that has pinned no key
// fetches it once over HTTPS (web PKI) and pins it; from then on only a
// rotation record signed by the pinned key changes it. The provisioning
// bundle carries the same key, but the Bridge's Hub connection is set by
// hand, not from the bundle. Public, under /api/relay/ like the bridge CA.
type DirectoryKeyHandler struct {
	anchor *directory.TrustAnchor
}

// NewDirectoryKeyHandler builds the handler; nil answers 404.
func NewDirectoryKeyHandler(a *directory.TrustAnchor) *DirectoryKeyHandler {
	return &DirectoryKeyHandler{anchor: a}
}

type directoryKeyResponse struct {
	KeyID string `json:"key_id"`
	PKIX  []byte `json:"pkix"` // base64 PKIX DER
	Alg   string `json:"alg"`
}

// Key answers the current directory signing key.
// @Summary Directory signing key (public)
// @Description The Hub's current directory signing key, public half, for a bridge to pin on its first connect. key_id is the SHA-256 of the PKIX DER, hex. No authentication: it is a public key.
// @Tags directory
// @Produce json
// @Success 200 {object} directoryKeyResponse
// @Failure 404 {object} map[string]string "This Hub signs no directory"
// @Router /api/relay/directory-key [get]
func (h *DirectoryKeyHandler) Key(w http.ResponseWriter, r *http.Request) {
	if h.anchor == nil {
		writeError(w, http.StatusNotFound, "this Hub signs no directory")
		return
	}
	pub := h.anchor.PublicKey()
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, directoryKeyResponse{KeyID: directory.KeyID(pub), PKIX: pub, Alg: directory.EnvelopeAlg})
}
