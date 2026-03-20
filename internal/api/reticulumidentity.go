package api

import (
	"encoding/hex"
	"net/http"

	"github.com/cubeos-app/meshsat-hub/internal/reticulum"
)

// ReticulumIdentityHandler serves the Hub's Reticulum identity info.
type ReticulumIdentityHandler struct {
	hubID *reticulum.HubIdentity
}

// NewReticulumIdentityHandler creates a handler for /api/reticulum/identity.
func NewReticulumIdentityHandler(hubID *reticulum.HubIdentity) *ReticulumIdentityHandler {
	return &ReticulumIdentityHandler{hubID: hubID}
}

// reticulumIdentityResponse is the JSON response for GET /api/reticulum/identity.
type reticulumIdentityResponse struct {
	DestHash     string `json:"dest_hash"`
	PublicKeyHex string `json:"public_key_hex"`
	AppName      string `json:"app_name"`
}

// GetIdentity returns the Hub's Reticulum identity (public info only).
// @Summary      Get Hub Reticulum identity
// @Description  Returns the Hub's Reticulum destination hash and public key. Private key is never exposed.
// @Tags         reticulum
// @Produce      json
// @Success      200  {object}  reticulumIdentityResponse
// @Failure      503  {object}  map[string]string
// @Router       /api/reticulum/identity [get]
func (h *ReticulumIdentityHandler) GetIdentity(w http.ResponseWriter, _ *http.Request) {
	if h.hubID == nil || !h.hubID.IsLoaded() {
		writeError(w, http.StatusServiceUnavailable, "reticulum identity not loaded")
		return
	}

	id := h.hubID.Identity()
	writeJSON(w, http.StatusOK, reticulumIdentityResponse{
		DestHash:     h.hubID.DestHashHex(),
		PublicKeyHex: hex.EncodeToString(id.PublicBytes()),
		AppName:      h.hubID.AppName(),
	})
}
