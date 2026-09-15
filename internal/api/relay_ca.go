package api

import (
	"net/http"
	"sync"
)

// caMu guards caPEM; SetCA runs once at startup, CA is read per request.
var caMu sync.RWMutex

// SetCA publishes the Hub's bridge CA certificate at GET /api/relay/ca.
func (h *RelayHandler) SetCA(pem []byte) {
	caMu.Lock()
	h.caPEM = append([]byte(nil), pem...)
	caMu.Unlock()
}

// CA serves the bridge CA certificate, the only root either end of a relay
// tunnel verifies the other against (docs/relay.md, "Inside the tunnel").
// A kit must not carry this CA in its Hub connection settings, because that
// field is the root store for the MQTT broker, which has a public
// certificate; so the Bridge's relay client fetches it here over HTTPS, and
// the system roots vouch for which CA it will trust its clients against.
// The certificate is public; no credentials are needed.
//
// @Summary      Bridge CA certificate for the relay
// @Description  The PEM-encoded CA that issues bridge certificates. Both ends of a relay tunnel verify each other against it.
// @Tags         relay
// @Produce      application/x-pem-file
// @Success      200 {string} string "PEM"
// @Failure      404 {object} map[string]string "no bridge CA on this Hub"
// @Router       /api/relay/ca [get]
func (h *RelayHandler) CA(w http.ResponseWriter, r *http.Request) {
	caMu.RLock()
	pem := h.caPEM
	caMu.RUnlock()
	if len(pem) == 0 {
		writeError(w, http.StatusNotFound, "this Hub has no bridge CA")
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pem)
}
