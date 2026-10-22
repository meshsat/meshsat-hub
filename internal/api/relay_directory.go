package api

import (
	"context"
	"net/http"
	"strconv"

	"github.com/meshsat/meshsat-hub/internal/directory"
)

// relayDirectory is what the phones' directory read needs of the directory.
type relayDirectory interface {
	Snapshot(ctx context.Context, tenantID string) (*directory.Snapshot, error)
	RotationChain(ctx context.Context, tenantID, fromKeyID string) ([]directory.Rotation, error)
}

// RelayDirectoryAnswer is the signed directory as a phone reads it: the
// rotation records that lead from the key it trusts to the current one,
// oldest first, then the snapshot the current key signed, when it is newer
// than what the phone holds.
type RelayDirectoryAnswer struct {
	Rotations []directory.Envelope `json:"rotations"`
	Snapshot  *directory.Envelope  `json:"snapshot,omitempty"`
}

// SetDirectory gives the relay the tenant directory and its signing key, for
// GET /api/relay/directory.
func (h *RelayHandler) SetDirectory(d relayDirectory, anchor *directory.TrustAnchor) {
	h.directory = d
	h.dirAnchor = anchor
}

// Directory serves a phone its tenant's signed directory.
// @Summary      The tenant's signed directory, for a phone (HeMB-NG C9.5)
// @Description  MeshSat Android and iOS take no directory_push (the pusher leaves phones out) and read the directory here: HTTP Basic with the phone's own bridge credentials, the tenant being the bridge's owner. The answer is the v2 envelope of the tenant's snapshot (contract C7.2), which the phone verifies against the key it pinned from its provisioning bundle (dir_sign_pub) or once over HTTPS, and the signed rotation records from the key it names to the current one, oldest first. 304 when there is neither a newer snapshot nor a rotation to give. [MESHSAT-1642]
// @Tags         relay
// @Security     BasicAuth
// @Produce      json
// @Param        since   query  integer  false  "The snapshot version the phone holds; a snapshot at or below it is left out"
// @Param        key_id  query  string   false  "The id of the directory key the phone trusts (SHA-256 of its PKIX DER, hex)"
// @Success      200  {object}  RelayDirectoryAnswer
// @Success      304  "nothing newer"
// @Failure      400  {object}  map[string]string  "since is not a number"
// @Failure      401  {object}  map[string]string
// @Failure      503  {object}  map[string]string  "no directory or no signing key on this Hub"
// @Router       /api/relay/directory [get]
func (h *RelayHandler) Directory(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := h.authenticate(r)
	if !ok {
		h.unauthorized(w)
		return
	}
	if h.directory == nil || h.dirAnchor == nil {
		writeError(w, http.StatusServiceUnavailable, "directory not configured")
		return
	}
	var since int64
	if v := r.URL.Query().Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "since must be a non-negative integer")
			return
		}
		since = n
	}
	ctx := r.Context()
	ans := RelayDirectoryAnswer{Rotations: []directory.Envelope{}}
	if keyID := r.URL.Query().Get("key_id"); keyID != "" && keyID != h.dirAnchor.KeyID() {
		chain, err := h.directory.RotationChain(ctx, tenantID, keyID)
		if err != nil {
			writeInternalError(w, err, "")
			return
		}
		for _, rot := range chain {
			ans.Rotations = append(ans.Rotations, rot.Envelope)
		}
	}
	snap, err := h.directory.Snapshot(ctx, tenantID)
	if err != nil {
		writeInternalError(w, err, "")
		return
	}
	if snap.Version > since {
		env, err := h.dirAnchor.SealSnapshot(snap)
		if err != nil {
			writeInternalError(w, err, "")
			return
		}
		ans.Snapshot = env
	}
	if ans.Snapshot == nil && len(ans.Rotations) == 0 {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, ans)
}
