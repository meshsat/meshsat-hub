package api

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/audit"
	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/federation"
	"github.com/meshsat/meshsat-hub/internal/netguard"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// FederationHandler is a tenant owner's side of Hub federation
// (MESHSAT-1549): this tenant's own identity, the peer Hubs it trusts by
// certificate, and the grants that let a peer's modem reach one of its
// devices. Every route is owner-only: a peer and a grant are standing
// decisions to receive traffic and to pay for the MT that delivers it.
type FederationHandler struct {
	store store.Store
	svc   *federation.Service
	audit *audit.Service
}

func NewFederationHandler(s store.Store, svc *federation.Service, a *audit.Service) *FederationHandler {
	return &FederationHandler{store: s, svc: svc, audit: a}
}

type federationIdentityResponse struct {
	// Fingerprint is the SPKI SHA-256 of the certificate, the value both
	// owners compare out of band.
	Fingerprint    string `json:"fingerprint"`
	CertificatePEM string `json:"certificate_pem"`
	// RelayURL is what a peer enters for this tenant. It carries a
	// capability token: share it with the peer's owner only.
	RelayURL string `json:"relay_url"`
}

func (h *FederationHandler) log(r *http.Request, tenantID, action, detail string) {
	if h.audit == nil {
		return
	}
	_ = h.audit.Log(r.Context(), tenantID, action, operatorEmail(r), detail, clientIPFromRequest(r))
}

// Identity returns this tenant's federation identity, creating it on first use.
//
//	@Summary      Federation identity
//	@Description  The certificate and fingerprint a peer Hub's owner pins, and the URL that peer relays to. Created on first request.
//	@Tags         federation
//	@Produce      json
//	@Success      200  {object}  federationIdentityResponse
//	@Failure      500  {object}  map[string]string
//	@Router       /api/federation/identity [get]
func (h *FederationHandler) Identity(w http.ResponseWriter, r *http.Request) {
	tid := hubauth.TenantIDFromContext(r.Context())
	id, url, err := h.svc.Identity(r.Context(), tid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load the federation identity")
		return
	}
	writeJSON(w, http.StatusOK, federationIdentityResponse{Fingerprint: id.Fingerprint, CertificatePEM: string(id.CertPEM), RelayURL: url})
}

type federationPeerResponse struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	RelayURL       string `json:"relay_url"`
	Fingerprint    string `json:"fingerprint"`
	CreatedAt      string `json:"created_at"`
	CreatedByEmail string `json:"created_by_email,omitempty"`
}

func peerResponse(p store.FederationPeer) federationPeerResponse {
	return federationPeerResponse{ID: p.ID, Name: p.Name, RelayURL: p.RelayURL, Fingerprint: p.Fingerprint,
		CreatedAt: p.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"), CreatedByEmail: p.CreatedByEmail}
}

// ListPeers lists the peer Hubs this tenant trusts.
//
//	@Summary      List federation peers
//	@Tags         federation
//	@Produce      json
//	@Success      200  {array}   federationPeerResponse
//	@Router       /api/federation/peers [get]
func (h *FederationHandler) ListPeers(w http.ResponseWriter, r *http.Request) {
	peers, err := h.store.ListFederationPeers(r.Context(), hubauth.TenantIDFromContext(r.Context()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list peers")
		return
	}
	out := make([]federationPeerResponse, 0, len(peers))
	for _, p := range peers {
		out = append(out, peerResponse(p))
	}
	writeJSON(w, http.StatusOK, out)
}

type federationPeerRequest struct {
	Name           string `json:"name"`
	RelayURL       string `json:"relay_url"`
	CertificatePEM string `json:"certificate_pem"`
}

// CreatePeer adds a peer Hub by its relay URL and certificate. The response
// carries the certificate's fingerprint: compare it with the one the peer's
// owner reads on their own Federation page before relying on it.
//
//	@Summary      Add a federation peer
//	@Tags         federation
//	@Accept       json
//	@Produce      json
//	@Param        body  body      federationPeerRequest  true  "Name, the peer's relay URL and its certificate (PEM)"
//	@Success      201   {object}  federationPeerResponse
//	@Failure      400   {object}  map[string]string
//	@Failure      409   {object}  map[string]string
//	@Router       /api/federation/peers [post]
func (h *FederationHandler) CreatePeer(w http.ResponseWriter, r *http.Request) {
	tid := hubauth.TenantIDFromContext(r.Context())
	var req federationPeerRequest
	if err := readJSON(w, r, &req, 16*1024); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || utf8.RuneCountInString(req.Name) > 128 {
		writeError(w, http.StatusBadRequest, "name must be 1 to 128 characters")
		return
	}
	req.RelayURL = strings.TrimSpace(req.RelayURL)
	if !strings.HasPrefix(req.RelayURL, "https://") || !strings.Contains(req.RelayURL, "/api/webhook/federation/") {
		writeError(w, http.StatusBadRequest, "relay_url must be the peer's https://.../api/webhook/federation/<token> URL")
		return
	}
	if err := netguard.ValidatePublicURL(req.RelayURL); err != nil {
		writeError(w, http.StatusBadRequest, "relay_url: "+err.Error())
		return
	}
	fp, _, err := federation.ParsePeerCert([]byte(req.CertificatePEM))
	if err != nil {
		writeError(w, http.StatusBadRequest, "certificate_pem: "+err.Error())
		return
	}
	if own, _, err := h.svc.LoadIdentityFor(r.Context(), tid); err == nil && own.Fingerprint == fp {
		writeError(w, http.StatusBadRequest, "that is this tenant's own certificate")
		return
	}
	p := &store.FederationPeer{Name: req.Name, RelayURL: req.RelayURL, CertPEM: strings.TrimSpace(req.CertificatePEM), Fingerprint: fp}
	if u := hubauth.FromContext(r.Context()); u != nil {
		p.CreatedByEmail = u.Email
	}
	if err := h.store.CreateFederationPeer(r.Context(), tid, p); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			writeError(w, http.StatusConflict, "a peer with this certificate is already added")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not store the peer")
		return
	}
	h.log(r, tid, "federation_peer_added", fmt.Sprintf("peer %s %q fingerprint %s", p.ID, p.Name, fp))
	writeJSON(w, http.StatusCreated, peerResponse(*p))
}

// DeletePeer removes a peer and every grant naming it.
//
//	@Summary      Remove a federation peer
//	@Tags         federation
//	@Param        id  path  string  true  "peer id"
//	@Success      204
//	@Failure      404  {object}  map[string]string
//	@Router       /api/federation/peers/{id} [delete]
func (h *FederationHandler) DeletePeer(w http.ResponseWriter, r *http.Request) {
	tid := hubauth.TenantIDFromContext(r.Context())
	id := chi.URLParam(r, "id")
	if err := h.store.DeleteFederationPeer(r.Context(), tid, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no such peer")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not remove the peer")
		return
	}
	h.log(r, tid, "federation_peer_removed", "peer "+id)
	w.WriteHeader(http.StatusNoContent)
}

// ListGrants lists which peer modems may reach which of this tenant's devices.
//
//	@Summary      List federation grants
//	@Tags         federation
//	@Produce      json
//	@Success      200  {array}   store.FederationGrant
//	@Router       /api/federation/grants [get]
func (h *FederationHandler) ListGrants(w http.ResponseWriter, r *http.Request) {
	grants, err := h.store.ListFederationGrants(r.Context(), hubauth.TenantIDFromContext(r.Context()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list grants")
		return
	}
	writeJSON(w, http.StatusOK, grants)
}

type federationGrantRequest struct {
	PeerID   string `json:"peer_id"`
	FromIMEI string `json:"from_imei"`
	ToIMEI   string `json:"to_imei"`
}

var federationIMEI = regexp.MustCompile(`^[0-9]{15}$`)

// CreateGrant lets one modem of a peer reach one of this tenant's devices.
// Every message it carries is an MT on this tenant's own satellite account.
//
//	@Summary      Add a federation grant
//	@Tags         federation
//	@Accept       json
//	@Produce      json
//	@Param        body  body      federationGrantRequest  true  "peer, the peer's sending modem, this tenant's device"
//	@Success      201   {object}  store.FederationGrant
//	@Failure      400   {object}  map[string]string
//	@Failure      409   {object}  map[string]string
//	@Router       /api/federation/grants [post]
func (h *FederationHandler) CreateGrant(w http.ResponseWriter, r *http.Request) {
	tid := hubauth.TenantIDFromContext(r.Context())
	var req federationGrantRequest
	if err := readJSON(w, r, &req, 4096); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !federationIMEI.MatchString(req.FromIMEI) || !federationIMEI.MatchString(req.ToIMEI) {
		writeError(w, http.StatusBadRequest, "from_imei and to_imei must be 15 digits")
		return
	}
	if _, err := h.store.GetFederationPeer(r.Context(), tid, req.PeerID); err != nil {
		writeError(w, http.StatusBadRequest, "no such peer")
		return
	}
	if _, err := h.store.GetDevice(r.Context(), tid, req.ToIMEI); err != nil {
		writeError(w, http.StatusBadRequest, "to_imei is not one of this tenant's devices")
		return
	}
	g := &store.FederationGrant{PeerID: req.PeerID, FromIMEI: req.FromIMEI, ToIMEI: req.ToIMEI}
	if u := hubauth.FromContext(r.Context()); u != nil {
		g.CreatedByEmail = u.Email
	}
	if err := h.store.CreateFederationGrant(r.Context(), tid, g); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			writeError(w, http.StatusConflict, "this grant exists already")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not store the grant")
		return
	}
	h.log(r, tid, "federation_grant_added", fmt.Sprintf("grant %s peer %s from %s to %s", g.ID, g.PeerID, g.FromIMEI, g.ToIMEI))
	writeJSON(w, http.StatusCreated, g)
}

// DeleteGrant withdraws a grant.
//
//	@Summary      Remove a federation grant
//	@Tags         federation
//	@Param        id  path  string  true  "grant id"
//	@Success      204
//	@Failure      404  {object}  map[string]string
//	@Router       /api/federation/grants/{id} [delete]
func (h *FederationHandler) DeleteGrant(w http.ResponseWriter, r *http.Request) {
	tid := hubauth.TenantIDFromContext(r.Context())
	id := chi.URLParam(r, "id")
	if err := h.store.DeleteFederationGrant(r.Context(), tid, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no such grant")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not remove the grant")
		return
	}
	h.log(r, tid, "federation_grant_removed", "grant "+id)
	w.WriteHeader(http.StatusNoContent)
}
