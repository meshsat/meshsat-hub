package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/directory"
)

// DirectoryHandler serves the Hub directory REST API: per-tenant
// contacts, groups, dispatch policies, and a signed snapshot endpoint
// bridges can pull on reconnect. Every response is tenant-scoped via
// auth.TenantIDFromContext — handlers never trust a client-supplied
// tenant identifier. [MESHSAT-538]
type DirectoryHandler struct {
	store       directory.Store
	trustAnchor *directory.TrustAnchor // optional; snapshots go unsigned when nil
}

// NewDirectoryHandler constructs a handler bound to s. trustAnchor
// supplies the Hub's ECDSA-P256 signing key (MESHSAT-539); a nil
// anchor disables snapshot signing but lets the rest of the CRUD
// surface function normally (useful in tests).
func NewDirectoryHandler(s directory.Store, anchor *directory.TrustAnchor) *DirectoryHandler {
	return &DirectoryHandler{store: s, trustAnchor: anchor}
}

// ─── Contacts ────────────────────────────────────────────────────────

// ListContacts returns every contact belonging to the caller's tenant.
// @Summary List directory contacts
// @Description Returns every contact in the caller's tenant. Newest
// @Description clients also honour ?kind=<bearer> to narrow to
// @Description contacts that have at least one address of the given
// @Description kind.
// @Tags directory
// @Produce json
// @Param kind query string false "Optional bearer kind filter (SMS, MESHTASTIC, APRS, IRIDIUM_SBD, IRIDIUM_IMT, CELLULAR, TAK, RETICULUM, ZIGBEE, BLE, WEBHOOK, EMAIL, MQTT)"
// @Success 200 {array} directory.Contact
// @Failure 500 {object} map[string]string
// @Router /api/v1/directory/contacts [get]
func (h *DirectoryHandler) ListContacts(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	contacts, err := h.store.ListContacts(r.Context(), tid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if kind := r.URL.Query().Get("kind"); kind != "" {
		contacts = filterContactsByKind(contacts, directory.AddressKind(kind))
	}
	if contacts == nil {
		contacts = []directory.Contact{}
	}
	writeJSON(w, http.StatusOK, contacts)
}

// GetContact returns a single contact by ID.
// @Summary Get directory contact
// @Tags directory
// @Produce json
// @Param id path string true "Contact UUID"
// @Success 200 {object} directory.Contact
// @Failure 404 {object} map[string]string
// @Router /api/v1/directory/contacts/{id} [get]
func (h *DirectoryHandler) GetContact(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	id := chi.URLParam(r, "id")
	c, err := h.store.GetContact(r.Context(), tid, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if c == nil {
		writeError(w, http.StatusNotFound, "contact not found")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// CreateContact persists a new contact for the caller's tenant. The
// body may omit an ID (Hub generates a UUIDv4). Any supplied ID is
// honoured so clients can round-trip Hub-originated identifiers.
// @Summary Create directory contact
// @Tags directory
// @Accept json
// @Produce json
// @Param body body directory.Contact true "Contact (tenant_id will be overridden by the caller's tenant)"
// @Success 201 {object} directory.Contact
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/v1/directory/contacts [post]
func (h *DirectoryHandler) CreateContact(w http.ResponseWriter, r *http.Request) {
	var body directory.Contact
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.DisplayName == "" {
		writeError(w, http.StatusBadRequest, "display_name is required")
		return
	}
	body.TenantID = auth.TenantIDFromContext(r.Context())
	body.CreatedAt = time.Time{} // let the store stamp these
	body.UpdatedAt = time.Time{}
	if err := h.store.PutContact(r.Context(), &body); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_, _ = h.store.BumpVersion(r.Context(), body.TenantID)
	writeJSON(w, http.StatusCreated, body)
}

// UpdateContact replaces a contact record by ID.
// @Summary Update directory contact
// @Tags directory
// @Accept json
// @Produce json
// @Param id path string true "Contact UUID"
// @Param body body directory.Contact true "Contact"
// @Success 200 {object} directory.Contact
// @Failure 400 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Router /api/v1/directory/contacts/{id} [put]
func (h *DirectoryHandler) UpdateContact(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id required")
		return
	}
	tid := auth.TenantIDFromContext(r.Context())
	existing, _ := h.store.GetContact(r.Context(), tid, id)
	if existing == nil {
		writeError(w, http.StatusNotFound, "contact not found")
		return
	}
	var body directory.Contact
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	body.ID = id
	body.TenantID = tid
	body.CreatedAt = existing.CreatedAt // preserve
	body.UpdatedAt = time.Time{}        // store re-stamps
	if err := h.store.PutContact(r.Context(), &body); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_, _ = h.store.BumpVersion(r.Context(), tid)
	writeJSON(w, http.StatusOK, body)
}

// DeleteContact removes a contact and cascades addresses, keys, and
// group memberships.
// @Summary Delete directory contact
// @Tags directory
// @Produce json
// @Param id path string true "Contact UUID"
// @Success 204
// @Failure 404 {object} map[string]string
// @Router /api/v1/directory/contacts/{id} [delete]
func (h *DirectoryHandler) DeleteContact(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	id := chi.URLParam(r, "id")
	if err := h.store.DeleteContact(r.Context(), tid, id); err != nil {
		if errors.Is(err, directory.ErrNotFound) {
			writeError(w, http.StatusNotFound, "contact not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_, _ = h.store.BumpVersion(r.Context(), tid)
	w.WriteHeader(http.StatusNoContent)
}

// ─── Snapshot ────────────────────────────────────────────────────────

// GetSnapshot assembles the caller's tenant directory (contacts +
// groups + policies + version), canonicalises the payload, and signs
// it with the Hub's ECDSA-P256 directory-signing key
// (MESHSAT-539). Bridges verify the signature against the trust
// anchor published in their provisioning bundle.
// @Summary Get signed directory snapshot
// @Description Full tenant directory payload with an ECDSA-P256-SHA256
// @Description signature over its canonical JSON. Bridges consume via
// @Description the directory_push MQTT command (MESHSAT-540) or by
// @Description polling this endpoint on reconnect. since is advisory —
// @Description v1 always returns the full snapshot; a future delta
// @Description endpoint (MESHSAT-540) ships incremental changes.
// @Tags directory
// @Produce json
// @Param since query integer false "Hint: only return data if stored version > since"
// @Success 200 {object} directory.Snapshot
// @Success 304 "Not modified — stored version <= since"
// @Failure 500 {object} map[string]string
// @Router /api/v1/directory/snapshot [get]
func (h *DirectoryHandler) GetSnapshot(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	since := r.URL.Query().Get("since")
	snap, err := h.store.Snapshot(r.Context(), tid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if since != "" {
		var sinceV int64
		_ = json.Unmarshal([]byte(since), &sinceV)
		if snap.Version > 0 && snap.Version <= sinceV {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	if h.trustAnchor != nil {
		if sig, err := SignSnapshot(snap, h.trustAnchor); err == nil {
			snap.Signature = sig
		}
	}
	writeJSON(w, http.StatusOK, snap)
}

// SignSnapshot canonicalises a snapshot (signature field zeroed) and
// returns an ECDSA-P256-SHA256 ASN.1 DER signature over it. Exposed
// for the commander package (MESHSAT-540) so directory_push commands
// reuse the same canonical form used by the REST snapshot endpoint.
func SignSnapshot(snap *directory.Snapshot, anchor *directory.TrustAnchor) ([]byte, error) {
	if snap == nil || anchor == nil {
		return nil, errors.New("nil snapshot or anchor")
	}
	canonical, err := CanonicalSnapshotBytes(snap)
	if err != nil {
		return nil, err
	}
	return anchor.Sign(canonical)
}

// CanonicalSnapshotBytes returns the deterministic byte representation
// of a snapshot used as the signature input on both sides. Uses
// encoding/json with a sorted map-key stable output for the Signature
// field zeroed.
func CanonicalSnapshotBytes(snap *directory.Snapshot) ([]byte, error) {
	if snap == nil {
		return nil, errors.New("nil snapshot")
	}
	// Copy and zero the Signature so signers and verifiers agree.
	cp := *snap
	cp.Signature = nil
	return json.Marshal(&cp)
}

// filterContactsByKind returns only contacts with at least one
// address of the given kind. O(N×M) with tiny N and M in practice.
func filterContactsByKind(contacts []directory.Contact, kind directory.AddressKind) []directory.Contact {
	out := make([]directory.Contact, 0, len(contacts))
	for _, c := range contacts {
		for _, a := range c.Addresses {
			if a.Kind == kind {
				out = append(out, c)
				break
			}
		}
	}
	return out
}
