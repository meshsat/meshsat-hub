package api

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/skip2/go-qrcode"

	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/lxmf"
	"github.com/meshsat/meshsat-hub/internal/lxmfhub"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// LXMFHandler is a tenant's LXMF address on the Hub's Reticulum node
// (MESHSAT-1555): the identity, its inbox and outbox, and the peers it has
// seen. Reading is for viewers, sending for operators, the identity's
// creation and settings for the owner: an address is a standing decision to
// receive messages from strangers on the Reticulum network.
type LXMFHandler struct {
	svc   *lxmfhub.Service // nil when the Reticulum node is configured off
	store store.Store
	// endpoint is the public host:port a client connects to, "" when none.
	endpoint string
}

// NewLXMFHandler builds the handler; svc may be nil. endpoint is the public
// host:port a stock client connects to (HUB_RNS_PUBLIC_ENDPOINT).
func NewLXMFHandler(svc *lxmfhub.Service, st store.Store, endpoint string) *LXMFHandler {
	return &LXMFHandler{svc: svc, store: st, endpoint: endpoint}
}

type lxmfIdentityResponse struct {
	// Exists is false until the owner creates the address.
	Exists bool `json:"exists"`
	// Address is the lxmf.delivery destination hash, 32 hex characters: what
	// a Sideband, CrossTalk, MeshChat or NomadNet user enters to reach this
	// tenant.
	Address     string `json:"address,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	StampCost   int    `json:"stamp_cost"`
	Enabled     bool   `json:"enabled"`
	// ActiveOnThisReplica is true on the replica whose node carries the
	// address (the leader); LastAnnounce is when it last announced there.
	ActiveOnThisReplica bool       `json:"active_on_this_replica"`
	LastAnnounce        *time.Time `json:"last_announce,omitempty"`
	// QRURL renders the address as a QR code image.
	QRURL string `json:"qr_url,omitempty"`
	// PropagationAddress is the tenant's propagation node (lxmf.propagation
	// on the same identity): what a client sets as its propagation node so
	// messages for it are held while it is offline. PropagationStampCost is
	// the proof of work a client attaches to a message it hands the node.
	PropagationAddress   string `json:"propagation_address,omitempty"`
	PropagationEnabled   bool   `json:"propagation_enabled"`
	PropagationStampCost int    `json:"propagation_stamp_cost"`
	// PublicEndpoint is the host:port a client connects to with a TCP client
	// interface, "" when this Hub has none.
	PublicEndpoint string `json:"public_endpoint,omitempty"`
	// AccessMode says who can reach the address (MESHSAT-1722): private
	// (default: only a client that enters the network name and passphrase
	// below on its TCP interface), public (anyone on Reticulum) or off.
	AccessMode  string `json:"access_mode,omitempty"`
	IFACNetname string `json:"ifac_netname,omitempty"`
	// IFACPassphrase is shown to owners and operators only;
	// IFACPassphraseHidden is true when the caller may not see it.
	IFACPassphrase       string `json:"ifac_passphrase,omitempty"`
	IFACPassphraseHidden bool   `json:"ifac_passphrase_hidden,omitempty"`
	// RotationAdvised names the kits deleted since this network name was
	// made, newest first (MESHSAT-1731): each may still hold the code and join
	// with it, so the page offers a new one. Empty after a rotation.
	RotationAdvised []string `json:"rotation_advised,omitempty"`
}

type lxmfIdentityRequest struct {
	DisplayName          *string `json:"display_name"`
	StampCost            *int    `json:"stamp_cost"`
	Enabled              *bool   `json:"enabled"`
	PropagationEnabled   *bool   `json:"propagation_enabled"`
	PropagationStampCost *int    `json:"propagation_stamp_cost"`
	// AccessMode is private, public or off.
	AccessMode *string `json:"access_mode"`
}

type lxmfSendRequest struct {
	// Destination is the peer's LXMF address, 32 hex characters.
	Destination string `json:"destination"`
	Content     string `json:"content"`
	Title       string `json:"title"`
	// Method is auto (default), direct or opportunistic.
	Method string `json:"method"`
	// Audio is a voice note and Image a photo, carried in the message's
	// LXMF fields as Sideband sends them; together at most 64 KiB. Content
	// may be empty when the message carries one. [MESHSAT-1735]
	Audio *lxmfAudioRequest `json:"audio,omitempty"`
	Image *lxmfImageRequest `json:"image,omitempty"`
}

// lxmfAudioRequest is a voice note: the LXMF audio mode (Codec2 0x01-0x09,
// 700C = 3; Opus 0x10-0x19) and the frames, base64.
type lxmfAudioRequest struct {
	Mode    int    `json:"mode"`
	DataB64 string `json:"data_b64"`
}

// lxmfImageRequest is a photo: the format name ("webp") and the bytes, base64.
type lxmfImageRequest struct {
	Format  string `json:"format"`
	DataB64 string `json:"data_b64"`
}

// lxmfAttachmentsResponse is the reply of GET /api/lxmf/messages/{id}/attachments.
type lxmfAttachmentsResponse struct {
	MessageID   string               `json:"message_id"`
	Attachments []lxmfAttachmentItem `json:"attachments"`
}

// lxmfAttachmentItem is one attachment's metadata and where its bytes are.
type lxmfAttachmentItem struct {
	store.LXMFAttachment
	ContentType string `json:"content_type"`
	URL         string `json:"url"`
}

// lxmfSendMaxBody bounds POST /api/lxmf/messages: the text, the two
// attachments in base64 and the JSON around them.
const lxmfSendMaxBody = int64(lxmfhub.MaxContentBytes + lxmf.MaxAttachmentBytes*2 + 4096)

func (h *LXMFHandler) off(w http.ResponseWriter) bool {
	if h.svc == nil {
		writeError(w, http.StatusServiceUnavailable, "the Reticulum node is not enabled on this Hub")
		return true
	}
	return false
}

func (h *LXMFHandler) identityResponse(r *http.Request, tenantID string, id *lxmfhub.Identity) lxmfIdentityResponse {
	resp := lxmfIdentityResponse{Exists: true, Address: id.Address, DisplayName: id.DisplayName, StampCost: id.StampCost, Enabled: id.Enabled,
		QRURL: "/api/lxmf/identity/qr.png", PropagationAddress: id.PropagationAddress, PropagationEnabled: id.PropagationEnabled, PropagationStampCost: id.PropagationStampCost,
		PublicEndpoint: h.endpoint, AccessMode: id.AccessMode, IFACNetname: id.IFACNetname}
	// The passphrase admits to the tenant's Private network. Owners hand it
	// out and operators set up the devices; a viewer reads the page only.
	if hubauth.RoleAtLeast(hubauth.FromContext(r.Context()), hubauth.RoleOperator) {
		resp.IFACPassphrase = id.IFACPassphrase
	} else if id.IFACPassphrase != "" {
		resp.IFACPassphraseHidden = true
	}
	if id.IFACNetname != "" {
		resp.RotationAdvised = h.rotationAdvised(r.Context(), tenantID, id.IFACNetname)
	}
	if active, last := h.svc.RegisteredOnThisReplica(tenantID); active {
		resp.ActiveOnThisReplica = true
		if !last.IsZero() {
			resp.LastAnnounce = &last
		}
	}
	return resp
}

// auditRotationAdvised is the audit entry a kit's deletion writes while the
// workspace has a Reticulum access code (MESHSAT-1731).
const auditRotationAdvised = "lxmf_access_rotation_advised"

func rotationAdvisedDetail(bridgeID, netname string) string {
	return "bridge=" + bridgeID + " network=" + netname
}

// rotationAdvised reads back the kits deleted under the current network name.
// It is advice: a failed read is logged and leaves the list out.
func (h *LXMFHandler) rotationAdvised(ctx context.Context, tenantID, netname string) []string {
	if h.store == nil {
		return nil
	}
	entries, err := h.store.ListAuditEntriesByAction(ctx, tenantID, []string{auditRotationAdvised}, 200)
	if err != nil {
		slog.Warn("lxmf: deleted kits not read; no new code advised", "tenant", tenantID, "error", err)
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, e := range entries {
		var bridgeID, network string
		for _, f := range strings.Fields(e.Detail) {
			if v, ok := strings.CutPrefix(f, "bridge="); ok {
				bridgeID = v
			} else if v, ok := strings.CutPrefix(f, "network="); ok {
				network = v
			}
		}
		if network == netname && bridgeID != "" && !seen[bridgeID] {
			seen[bridgeID] = true
			out = append(out, bridgeID)
		}
	}
	return out
}

// Identity returns the tenant's LXMF address, or exists=false.
//
//	@Summary      LXMF identity
//	@Description  This tenant's LXMF address on the Hub's Reticulum node, its display name, stamp cost and whether it is enabled. exists is false until the owner creates it.
//	@Tags         lxmf
//	@Produce      json
//	@Success      200  {object}  lxmfIdentityResponse
//	@Failure      503  {object}  map[string]string
//	@Router       /api/lxmf/identity [get]
func (h *LXMFHandler) Identity(w http.ResponseWriter, r *http.Request) {
	if h.off(w) {
		return
	}
	tid := hubauth.TenantIDFromContext(r.Context())
	id, err := h.svc.LoadIdentity(r.Context(), tid)
	if errors.Is(err, lxmfhub.ErrNoIdentity) {
		writeJSON(w, http.StatusOK, lxmfIdentityResponse{PublicEndpoint: h.endpoint})
		return
	}
	if err != nil {
		writeInternalError(w, err, "lxmf identity")
		return
	}
	writeJSON(w, http.StatusOK, h.identityResponse(r, tid, id))
}

// CreateIdentity creates the tenant's LXMF address on first call.
//
//	@Summary      Create the LXMF identity
//	@Description  Generates this tenant's Reticulum identity and LXMF address, named after the tenant with no stamp cost. Idempotent: an existing address is returned. Owner only.
//	@Tags         lxmf
//	@Produce      json
//	@Success      201  {object}  lxmfIdentityResponse
//	@Failure      503  {object}  map[string]string
//	@Router       /api/lxmf/identity [post]
func (h *LXMFHandler) CreateIdentity(w http.ResponseWriter, r *http.Request) {
	if h.off(w) {
		return
	}
	tid := hubauth.TenantIDFromContext(r.Context())
	id, err := h.svc.EnsureIdentity(r.Context(), tid, operatorEmail(r))
	if err != nil {
		writeInternalError(w, err, "lxmf identity create")
		return
	}
	writeJSON(w, http.StatusCreated, h.identityResponse(r, tid, id))
}

// UpdateIdentity changes the display name, stamp cost or enabled flag.
//
//	@Summary      Update the LXMF identity
//	@Description  Changes the display name other clients see, the stamp cost a sender must pay, whether the address is enabled, whether the propagation node holds messages for offline clients and at what stamp cost, and who can reach the address (access_mode: private, the default, admits only clients that enter this tenant's network name and passphrase; public admits anyone on Reticulum; off registers it nowhere). Absent fields are left alone. Owner only.
//	@Tags         lxmf
//	@Accept       json
//	@Produce      json
//	@Param        body  body      lxmfIdentityRequest  true  "fields to change"
//	@Success      200   {object}  lxmfIdentityResponse
//	@Failure      400   {object}  map[string]string
//	@Failure      404   {object}  map[string]string
//	@Router       /api/lxmf/identity [put]
func (h *LXMFHandler) UpdateIdentity(w http.ResponseWriter, r *http.Request) {
	if h.off(w) {
		return
	}
	var req lxmfIdentityRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	tid := hubauth.TenantIDFromContext(r.Context())
	id, err := h.svc.UpdateIdentity(r.Context(), tid, operatorEmail(r), lxmfhub.IdentityUpdate{DisplayName: req.DisplayName, StampCost: req.StampCost, Enabled: req.Enabled,
		PropagationEnabled: req.PropagationEnabled, PropagationStampCost: req.PropagationStampCost, AccessMode: req.AccessMode})
	switch {
	case errors.Is(err, lxmfhub.ErrNoIdentity):
		writeError(w, http.StatusNotFound, "this tenant has no LXMF address yet")
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, h.identityResponse(r, tid, id))
}

// RotateAccess replaces the network name and passphrase of a Private address.
//
//	@Summary      Rotate the Reticulum access code
//	@Description  Generates a new network name and passphrase for this tenant's Private Reticulum network. Every client that joined with the old ones is disconnected at once and must enter the new ones. Owner only.
//	@Tags         lxmf
//	@Produce      json
//	@Success      200  {object}  lxmfIdentityResponse
//	@Failure      404  {object}  map[string]string
//	@Failure      503  {object}  map[string]string
//	@Router       /api/lxmf/access/rotate [post]
func (h *LXMFHandler) RotateAccess(w http.ResponseWriter, r *http.Request) {
	if h.off(w) {
		return
	}
	tid := hubauth.TenantIDFromContext(r.Context())
	id, err := h.svc.RotateAccess(r.Context(), tid, operatorEmail(r))
	switch {
	case errors.Is(err, lxmfhub.ErrNoIdentity):
		writeError(w, http.StatusNotFound, "this tenant has no LXMF address yet")
		return
	case err != nil:
		writeInternalError(w, err, "lxmf access rotate")
		return
	}
	writeJSON(w, http.StatusOK, h.identityResponse(r, tid, id))
}

// IdentityQR renders the address as a PNG QR code.
//
//	@Summary      LXMF address as QR
//	@Description  The tenant's 32-character LXMF address as a QR code, for a client's "scan" button. size is the image side in pixels (128-1024, default 256).
//	@Tags         lxmf
//	@Produce      png
//	@Param        size  query     int  false  "image side in pixels"
//	@Success      200   {file}    binary
//	@Failure      404   {object}  map[string]string
//	@Router       /api/lxmf/identity/qr.png [get]
func (h *LXMFHandler) IdentityQR(w http.ResponseWriter, r *http.Request) {
	if h.off(w) {
		return
	}
	tid := hubauth.TenantIDFromContext(r.Context())
	id, err := h.svc.LoadIdentity(r.Context(), tid)
	if errors.Is(err, lxmfhub.ErrNoIdentity) {
		writeError(w, http.StatusNotFound, "this tenant has no LXMF address yet")
		return
	}
	if err != nil {
		writeInternalError(w, err, "lxmf identity qr")
		return
	}
	size := 256
	if v, err := strconv.Atoi(r.URL.Query().Get("size")); err == nil && v >= 128 && v <= 1024 {
		size = v
	}
	png, err := qrcode.Encode(id.Address, qrcode.Medium, size)
	if err != nil {
		writeInternalError(w, err, "lxmf identity qr")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(png)
}

// ListMessages lists the inbox and outbox, newest first.
//
//	@Summary      LXMF messages
//	@Description  The tenant's received and sent LXMF messages, newest first. direction in|out, state received|queued|sending|delivered|failed, limit 1-500 (default 50), before an RFC 3339 time for paging.
//	@Tags         lxmf
//	@Produce      json
//	@Param        direction  query  string  false  "in or out"
//	@Param        state      query  string  false  "message state"
//	@Param        limit      query  int     false  "page size"
//	@Param        before     query  string  false  "RFC 3339: only messages created before this"
//	@Success      200  {array}   store.LXMFMessage
//	@Failure      400  {object}  map[string]string
//	@Router       /api/lxmf/messages [get]
func (h *LXMFHandler) ListMessages(w http.ResponseWriter, r *http.Request) {
	if h.off(w) {
		return
	}
	q := store.LXMFQuery{Direction: r.URL.Query().Get("direction"), State: r.URL.Query().Get("state")}
	switch q.Direction {
	case "", store.LXMFDirectionIn, store.LXMFDirectionOut:
	default:
		writeError(w, http.StatusBadRequest, "direction must be in or out")
		return
	}
	switch q.State {
	case "", store.LXMFStateReceived, store.LXMFStateQueued, store.LXMFStateSending, store.LXMFStateDelivered, store.LXMFStateFailed:
	default:
		writeError(w, http.StatusBadRequest, "unknown state")
		return
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			writeError(w, http.StatusBadRequest, "limit must be 1 to 500")
			return
		}
		q.Limit = n
	}
	if v := r.URL.Query().Get("before"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "before must be an RFC 3339 time")
			return
		}
		q.Before = t
	}
	tid := hubauth.TenantIDFromContext(r.Context())
	list, err := h.store.ListLXMFMessages(r.Context(), tid, q)
	if err != nil {
		writeInternalError(w, err, "lxmf messages")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// GetMessage returns one message.
//
//	@Summary      One LXMF message
//	@Tags         lxmf
//	@Produce      json
//	@Param        id   path      string  true  "message id"
//	@Success      200  {object}  store.LXMFMessage
//	@Failure      404  {object}  map[string]string
//	@Router       /api/lxmf/messages/{id} [get]
func (h *LXMFHandler) GetMessage(w http.ResponseWriter, r *http.Request) {
	if h.off(w) {
		return
	}
	tid := hubauth.TenantIDFromContext(r.Context())
	m, err := h.store.GetLXMFMessage(r.Context(), tid, chi.URLParam(r, "id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such message")
		return
	}
	if err != nil {
		writeInternalError(w, err, "lxmf message")
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// Send queues one message for the peer; the state moves with the proof.
//
//	@Summary      Send an LXMF message
//	@Description  Queues a message from this tenant's address to a peer's LXMF address. The leader delivers it and the row moves to delivered on the peer's proof or to failed. Operator or owner.
//	@Tags         lxmf
//	@Accept       json
//	@Produce      json
//	@Param        body  body      lxmfSendRequest  true  "the message"
//	@Success      202   {object}  store.LXMFMessage
//	@Failure      400   {object}  map[string]string
//	@Failure      404   {object}  map[string]string
//	@Router       /api/lxmf/messages [post]
func (h *LXMFHandler) Send(w http.ResponseWriter, r *http.Request) {
	if h.off(w) {
		return
	}
	var req lxmfSendRequest
	if err := readJSON(w, r, &req, lxmfSendMaxBody); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	send := lxmfhub.SendRequest{Destination: req.Destination, Content: req.Content, Title: req.Title, Method: req.Method, Actor: operatorEmail(r)}
	if req.Audio != nil {
		data, err := base64.StdEncoding.DecodeString(req.Audio.DataB64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "audio.data_b64 is not base64")
			return
		}
		send.Audio = &lxmf.Attachment{Mode: req.Audio.Mode, Data: data}
	}
	if req.Image != nil {
		data, err := base64.StdEncoding.DecodeString(req.Image.DataB64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "image.data_b64 is not base64")
			return
		}
		send.Image = &lxmf.Attachment{Format: req.Image.Format, Data: data}
	}
	tid := hubauth.TenantIDFromContext(r.Context())
	row, err := h.svc.Send(r.Context(), tid, send)
	switch {
	case errors.Is(err, lxmfhub.ErrNoIdentity):
		writeError(w, http.StatusNotFound, "this tenant has no LXMF address yet")
		return
	case errors.Is(err, lxmf.ErrAttachmentsTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, row)
}

// Attachments lists a message's voice note and photo.
//
//	@Summary      An LXMF message's attachments
//	@Description  The voice note (audio) and photo (image) a received or sent LXMF message carried in its fields, in the message's order (the image first), with each one's media type and the URL of its bytes.
//	@Tags         lxmf
//	@Produce      json
//	@Param        id   path      string  true  "message id"
//	@Success      200  {object}  lxmfAttachmentsResponse
//	@Failure      404  {object}  map[string]string
//	@Router       /api/lxmf/messages/{id}/attachments [get]
func (h *LXMFHandler) Attachments(w http.ResponseWriter, r *http.Request) {
	if h.off(w) {
		return
	}
	tid := hubauth.TenantIDFromContext(r.Context())
	id := chi.URLParam(r, "id")
	rows, err := h.svc.Attachments(r.Context(), tid, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such message")
		return
	}
	if err != nil {
		writeInternalError(w, err, "lxmf attachments")
		return
	}
	resp := lxmfAttachmentsResponse{MessageID: id, Attachments: []lxmfAttachmentItem{}}
	for _, a := range rows {
		ct := lxmf.Attachment{Kind: a.Kind, Mode: a.Mode, Format: a.Format, Data: []byte{0}}.ContentType()
		resp.Attachments = append(resp.Attachments, lxmfAttachmentItem{LXMFAttachment: a, ContentType: ct, URL: "/api/lxmf/messages/" + id + "/attachments/" + strconv.Itoa(a.Index)})
	}
	writeJSON(w, http.StatusOK, resp)
}

// Attachment serves one attachment's bytes.
//
//	@Summary      Download an LXMF attachment
//	@Description  The bytes of one attachment: Codec2 frames as audio/codec2 with the mode in X-LXMF-Audio-Mode (700C = 3), an Opus note as audio/ogg, a photo as image/webp. n is the attachment's index in the message, from the list.
//	@Tags         lxmf
//	@Produce      octet-stream
//	@Param        id   path      string  true  "message id"
//	@Param        n    path      int     true  "attachment index"
//	@Success      200  {file}    binary
//	@Failure      404  {object}  map[string]string
//	@Router       /api/lxmf/messages/{id}/attachments/{n} [get]
func (h *LXMFHandler) Attachment(w http.ResponseWriter, r *http.Request) {
	if h.off(w) {
		return
	}
	n, err := strconv.Atoi(chi.URLParam(r, "n"))
	if err != nil || n < 0 {
		writeError(w, http.StatusNotFound, "no such attachment")
		return
	}
	tid := hubauth.TenantIDFromContext(r.Context())
	row, data, err := h.svc.Attachment(r.Context(), tid, chi.URLParam(r, "id"), n)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// The row or its bytes: ErrNoBlob wraps ErrNotFound.
		writeError(w, http.StatusNotFound, "no such attachment")
		return
	case err != nil:
		writeInternalError(w, err, "lxmf attachment")
		return
	}
	att := lxmf.Attachment{Kind: row.Kind, Mode: row.Mode, Format: row.Format, Data: data}
	w.Header().Set("Content-Type", att.ContentType())
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if row.Kind == lxmf.AttachmentAudio {
		w.Header().Set("X-LXMF-Audio-Mode", strconv.Itoa(row.Mode))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// Peers lists the LXMF peers this address has heard of.
//
//	@Summary      LXMF peers
//	@Description  LXMF delivery destinations the tenant's address has seen announce, with display names and hop counts. Filled on the leader replica, empty elsewhere.
//	@Tags         lxmf
//	@Produce      json
//	@Success      200  {array}  lxmf.Peer
//	@Router       /api/lxmf/peers [get]
func (h *LXMFHandler) Peers(w http.ResponseWriter, r *http.Request) {
	if h.off(w) {
		return
	}
	peers := h.svc.Peers(hubauth.TenantIDFromContext(r.Context()))
	if peers == nil {
		peers = []lxmf.Peer{}
	}
	writeJSON(w, http.StatusOK, peers)
}
