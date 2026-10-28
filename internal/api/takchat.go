package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/meshsat/meshsat-hub/internal/audit"
	"github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/takchat"
)

// TAKChatHandler lets an operator write to one crew from the map and read the
// conversation (MESHSAT-1706).
type TAKChatHandler struct {
	store store.Store
	chat  *takchat.Service
	audit *audit.Service
}

// NewTAKChatHandler wires the endpoints.
func NewTAKChatHandler(s store.Store, chat *takchat.Service, a *audit.Service) *TAKChatHandler {
	return &TAKChatHandler{store: s, chat: chat, audit: a}
}

// takCrewFresh is how far back the crew list reaches.
const takCrewFresh = 24 * time.Hour

// takCrew is one crew as the console lists it.
type takCrew struct {
	UID      string    `json:"uid"`
	Callsign string    `json:"callsign"`
	BridgeID string    `json:"bridge_id"`
	LastSeen time.Time `json:"last_seen"`
	// NodeKey matches the crew to a map marker: "!aabbccdd" for a mesh node
	// (uid meshsat-aabbccdd), the bridge id for a phone (uid MESHSAT-<bridge
	// id>), empty otherwise.
	NodeKey string `json:"node_key,omitempty"`
}

// takChatSendRequest is the body of POST /api/tak/contacts/{uid}/messages.
type takChatSendRequest struct {
	// Text is the message, 1 to 200 bytes; line breaks become spaces.
	Text string `json:"text"`
	// ClientRef makes the request idempotent: the same ref again sends nothing.
	ClientRef string `json:"client_ref,omitempty"`
}

func crewNodeKey(uid string) string {
	if hexID, ok := strings.CutPrefix(uid, "meshsat-"); ok && len(hexID) == 8 {
		return "!" + strings.ToLower(hexID)
	}
	if id, ok := strings.CutPrefix(uid, "MESHSAT-"); ok {
		return id
	}
	return ""
}

// ListCrews lists the tenant's crews on the TAK map.
// @Summary List TAK crews
// @Description The crews the tenant's kits and phones put on the TAK map in the last day, newest first, each with the client that owns it. An operator can write to any of them. Empty while dispatcher-to-crew chat is off.
// @Tags tak
// @Produce json
// @Success 200 {array} takCrew
// @Failure 500 {object} map[string]string
// @Router /api/tak/contacts [get]
func (h *TAKChatHandler) ListCrews(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	out := []takCrew{}
	if h.chat.Enabled() {
		contacts, err := h.store.ListTAKContacts(r.Context(), tid, time.Now().Add(-takCrewFresh))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not list crews")
			return
		}
		for _, c := range contacts {
			out = append(out, takCrew{UID: c.UID, Callsign: c.Callsign, BridgeID: c.BridgeID,
				LastSeen: c.LastSeen, NodeKey: crewNodeKey(c.UID)})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ListMessages lists the conversation with one crew.
// @Summary List messages with a TAK crew
// @Description The last 100 messages between the tenant's operators and one crew, oldest first: "out" written from the Hub, "in" the crew's answers. Kept 30 days. An "out" message's status is the Hub's word first ("sent" to the kit, "failed" at the bus), then the kit's ("queued" in its delivery ledger, "delivered", "failed" when the ledger gave up, "undelivered" when it refused the chat), with status_detail one line saying why.
// @Tags tak
// @Produce json
// @Param uid path string true "Crew uid"
// @Success 200 {array} store.TAKChatMessage
// @Failure 500 {object} map[string]string
// @Router /api/tak/contacts/{uid}/messages [get]
func (h *TAKChatHandler) ListMessages(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	msgs, err := h.store.ListTAKChatMessages(r.Context(), tid, chi.URLParam(r, "uid"), 100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list messages")
		return
	}
	if msgs == nil {
		msgs = []store.TAKChatMessage{}
	}
	writeJSON(w, http.StatusOK, msgs)
}

// SendMessage writes to one crew.
// @Summary Write to a TAK crew
// @Description Sends a message to one crew as a GeoChat from the operator. It goes to the kit or phone that owns the crew alone: a kit sends it to that node as a direct message, a phone shows it. The crew's answer is listed with the conversation. A kit that is off MQTT gets it as a compact frame over satellite (when the workspace allows the spend) or as an SMS to its own SIM (unless turned off); the message's bearer says which (mqtt, sbd, imt, sms).
// @Tags tak
// @Accept json
// @Produce json
// @Param uid path string true "Crew uid"
// @Param body body takChatSendRequest true "The message"
// @Success 202 {object} store.TAKChatMessage
// @Success 200 {object} store.TAKChatMessage "A repeat of a client_ref already sent"
// @Failure 400 {object} map[string]string "No text"
// @Failure 404 {object} map[string]string "No such crew, or chat is off"
// @Failure 409 {object} map[string]string "The crew has not been seen for a day, its kit or phone is no longer registered, or the kit is off MQTT and no other bearer reaches it"
// @Failure 413 {object} map[string]string "More than 200 bytes, or too long for the bearer the kit is reachable on"
// @Failure 429 {object} map[string]string "Too many messages"
// @Failure 502 {object} map[string]string "The message could not be handed to the bus"
// @Router /api/tak/contacts/{uid}/messages [post]
func (h *TAKChatHandler) SendMessage(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	crewUID := chi.URLParam(r, "uid")
	var req takChatSendRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	userID, name := "", ""
	if u := auth.FromContext(r.Context()); u != nil {
		userID, name = u.ID, u.Name
	}
	msg, created, err := h.chat.Send(r.Context(), takchat.SendRequest{
		TenantID: tid, CrewUID: crewUID, UserID: userID, Name: name, Text: req.Text, ClientRef: req.ClientRef,
	})
	switch {
	case errors.Is(err, takchat.ErrDisabled), errors.Is(err, takchat.ErrNoCrew):
		writeError(w, http.StatusNotFound, "no such crew")
		return
	case errors.Is(err, takchat.ErrEmpty):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, takchat.ErrTooLong):
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	case errors.Is(err, takchat.ErrStale), errors.Is(err, takchat.ErrUnregistered), errors.Is(err, takchat.ErrNoBearer):
		writeError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, takchat.ErrFrameTooLong):
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	case errors.Is(err, takchat.ErrLimited):
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	case errors.Is(err, takchat.ErrPublish):
		writeError(w, http.StatusBadGateway, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not send")
		return
	}
	if !created {
		writeJSON(w, http.StatusOK, msg)
		return
	}
	if h.audit != nil {
		// Who wrote to which crew through which client; never the words.
		_ = h.audit.Log(r.Context(), tid, "tak_chat_sent", operatorEmail(r),
			"crew "+crewUID+" via "+msg.BridgeID+" message "+msg.ID+" bearer "+msg.Bearer, clientIPFromRequest(r))
	}
	writeJSON(w, http.StatusAccepted, msg)
}
