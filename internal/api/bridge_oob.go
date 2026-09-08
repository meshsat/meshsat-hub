package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/bridge"
	"github.com/meshsat/meshsat-hub/internal/oob"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// BridgeOOBHandler manages the Hub's out-of-band pairing with a bridge
// (MESHSAT-964 C): the management key, the kit's bearer addresses and the
// counters.
type BridgeOOBHandler struct {
	store     store.Store
	oob       *oob.Service
	commander *bridge.Commander
}

// NewBridgeOOBHandler creates the handler; commander may be nil (no MQTT
// provisioning).
func NewBridgeOOBHandler(s store.Store, svc *oob.Service, cmdr *bridge.Commander) *BridgeOOBHandler {
	return &BridgeOOBHandler{store: s, oob: svc, commander: cmdr}
}

type oobPairRequest struct {
	KeyHex    string `json:"key_hex"`              // 64 hex chars: the mgmt key from the kit's bundle
	LocalRole string `json:"local_role,omitempty"` // "importer" (default: the kit issued the key) or "issuer"
	Phone     string `json:"phone,omitempty"`      // the kit's SIM, E.164
	SatIMEI   string `json:"sat_imei,omitempty"`   // the kit's Iridium modem IMEI
}

type oobProvisionRequest struct {
	Phone   string `json:"phone,omitempty"`
	SatIMEI string `json:"sat_imei,omitempty"`
}

type oobStatus struct {
	Paired    bool     `json:"paired"`
	PeerID    int      `json:"peer_id,omitempty"`
	LocalRole string   `json:"local_role,omitempty"`
	Phone     string   `json:"phone,omitempty"`
	SatIMEI   string   `json:"sat_imei,omitempty"`
	TxCounter int64    `json:"tx_counter,omitempty"`
	RxHigh    int64    `json:"rx_high,omitempty"`
	Bearers   []string `json:"bearers"`
	KeyHex    string   `json:"key_hex,omitempty"` // only on provision: shown once
}

func roleName(r int) string {
	if oob.RoleOf(r) == oob.RoleIssuer {
		return "issuer"
	}
	return "importer"
}

func (h *BridgeOOBHandler) status(p *store.OOBPeer) oobStatus {
	st := oobStatus{Bearers: h.oob.Bearers()}
	if p == nil {
		return st
	}
	st.Paired, st.PeerID, st.LocalRole, st.Phone, st.SatIMEI, st.TxCounter, st.RxHigh = true, p.PeerID, roleName(p.LocalRole), p.Phone, p.SatIMEI, p.TxCounter, p.RxHigh
	return st
}

func (h *BridgeOOBHandler) bridge(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	bridgeID := chi.URLParam(r, "id")
	tid := auth.TenantIDFromContext(r.Context())
	if b, err := h.store.GetBridge(r.Context(), tid, bridgeID); err != nil || b == nil {
		writeError(w, http.StatusNotFound, "bridge not found")
		return "", "", false
	}
	return tid, bridgeID, true
}

// Get returns the pairing state.
//
//	@Summary      Out-of-band pairing state of a bridge
//	@Tags         bridges
//	@Produce      json
//	@Param        id  path  string  true  "Bridge ID"
//	@Success      200  {object}  oobStatus
//	@Failure      404  {object}  map[string]string
//	@Router       /api/bridges/{id}/oob [get]
func (h *BridgeOOBHandler) Get(w http.ResponseWriter, r *http.Request) {
	tid, bridgeID, ok := h.bridge(w, r)
	if !ok {
		return
	}
	p, _ := h.oob.Peer(r.Context(), tid, bridgeID)
	writeJSON(w, http.StatusOK, h.status(p))
}

// Pair stores the management key the kit issued (bundle path).
//
//	@Summary      Pair the Hub with a bridge for out-of-band commands
//	@Description  Stores the 32-byte management key from the kit's key bundle (mgmt entry) and the kit's bearer addresses. The Hub takes the importer role unless local_role is "issuer".
//	@Tags         bridges
//	@Accept       json
//	@Produce      json
//	@Param        id    path  string          true  "Bridge ID"
//	@Param        body  body  oobPairRequest  true  "key and addresses"
//	@Success      200  {object}  oobStatus
//	@Failure      400  {object}  map[string]string
//	@Router       /api/bridges/{id}/oob [post]
func (h *BridgeOOBHandler) Pair(w http.ResponseWriter, r *http.Request) {
	tid, bridgeID, ok := h.bridge(w, r)
	if !ok {
		return
	}
	var req oobPairRequest
	if err := readJSON(w, r, &req, 4096); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	key, err := oob.ParseKeyHex(req.KeyHex)
	if err != nil {
		writeError(w, http.StatusBadRequest, "key_hex must be 64 hex characters")
		return
	}
	role := oob.RoleImporter
	switch strings.ToLower(strings.TrimSpace(req.LocalRole)) {
	case "", "importer":
	case "issuer":
		role = oob.RoleIssuer
	default:
		writeError(w, http.StatusBadRequest, "local_role must be importer or issuer")
		return
	}
	if req.Phone != "" && !validPhone(req.Phone) {
		writeError(w, http.StatusBadRequest, "phone must be E.164")
		return
	}
	if _, err := h.oob.Pair(r.Context(), tid, bridgeID, key, role, req.Phone, req.SatIMEI); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p, _ := h.oob.Peer(r.Context(), tid, bridgeID)
	writeJSON(w, http.StatusOK, h.status(p))
}

// Provision generates a key with the Hub as issuer and pushes it to the
// bridge over MQTT (key_rotate, channel mgmt, address hub).
//
//	@Summary      Generate a management key and push it to the bridge over MQTT
//	@Description  The Hub becomes the issuer of the key. The bridge must register the peer "hub" on this key_rotate (bridge-side support tracked on MESHSAT-964); the key is returned once for a manual import.
//	@Tags         bridges
//	@Accept       json
//	@Produce      json
//	@Param        id    path  string               true  "Bridge ID"
//	@Param        body  body  oobProvisionRequest  true  "bearer addresses"
//	@Success      200  {object}  oobStatus
//	@Failure      409  {object}  map[string]string  "bridge offline"
//	@Router       /api/bridges/{id}/oob/provision [post]
func (h *BridgeOOBHandler) Provision(w http.ResponseWriter, r *http.Request) {
	tid, bridgeID, ok := h.bridge(w, r)
	if !ok {
		return
	}
	var req oobProvisionRequest
	if err := readJSON(w, r, &req, 4096); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Phone != "" && !validPhone(req.Phone) {
		writeError(w, http.StatusBadRequest, "phone must be E.164")
		return
	}
	key, err := oob.RandomKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "key generation failed")
		return
	}
	if h.commander != nil {
		b, _ := h.store.GetBridge(r.Context(), tid, bridgeID)
		if b == nil || !b.Online {
			writeError(w, http.StatusConflict, "bridge is offline; pair with the kit's bundle key instead")
			return
		}
		if _, err := h.commander.SendCommand(r.Context(), bridgeID, bridge.KeyRotateCommand("mgmt", "hub", oob.KeyHex(key), 1)); err != nil {
			slog.Warn("oob: provision over MQTT failed", "bridge", bridgeID, "error", err)
			writeError(w, http.StatusBadGateway, fmt.Sprintf("bridge did not accept the key: %s", err))
			return
		}
	}
	if _, err := h.oob.Pair(r.Context(), tid, bridgeID, key, oob.RoleIssuer, req.Phone, req.SatIMEI); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p, _ := h.oob.Peer(r.Context(), tid, bridgeID)
	st := h.status(p)
	st.KeyHex = oob.KeyHex(key)
	writeJSON(w, http.StatusOK, st)
}

// Unpair removes the pairing.
//
//	@Summary      Remove the out-of-band pairing of a bridge
//	@Tags         bridges
//	@Param        id  path  string  true  "Bridge ID"
//	@Success      204
//	@Router       /api/bridges/{id}/oob [delete]
func (h *BridgeOOBHandler) Unpair(w http.ResponseWriter, r *http.Request) {
	tid, bridgeID, ok := h.bridge(w, r)
	if !ok {
		return
	}
	if err := h.oob.Unpair(r.Context(), tid, bridgeID); err != nil && !errors.Is(err, oob.ErrNotPaired) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func validPhone(p string) bool {
	p = strings.TrimSpace(p)
	if len(p) < 8 || len(p) > 16 || p[0] != '+' {
		return false
	}
	for _, c := range p[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
