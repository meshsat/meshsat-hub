package sms

import (
	"log/slog"
	"net/http"
)

// StatusHandler receives Twilio's delivery receipts for messages we sent.
//
// It is registered under /api/webhook/, which is auth-exempt by prefix, so the
// Twilio signature is the only thing standing in front of it -- the same
// treatment the inbound handler gets, and for the same reason: an unauthenticated
// endpoint here lets anyone assert that a message was delivered or failed.
type StatusHandler struct {
	channel   string
	authToken string
	secret    string
}

// NewStatusHandler builds the delivery-receipt handler for one bearer.
func NewStatusHandler(channel, authToken, secret string) *StatusHandler {
	return &StatusHandler{channel: channel, authToken: authToken, secret: secret}
}

// @Summary      Receive a Twilio delivery receipt
// @Description  Twilio posts outbound message status transitions here
// @Tags         webhook
// @Accept       application/x-www-form-urlencoded
// @Produce      json
// @Success      204
// @Failure      401  {object}  map[string]string
// @Failure      403  {object}  map[string]string
// @Router       /api/webhook/whatsapp/status [post]
func (h *StatusHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, `{"error":"invalid form data"}`, http.StatusBadRequest)
		return
	}

	// Same two accepted proofs as the inbound handler, and the same refusal
	// when neither is configured (MESHSAT-1168).
	switch {
	case h.authToken != "":
		sig := r.Header.Get("X-Twilio-Signature")
		if sig == "" || !validateTwilioSignature(h.authToken, r, sig) {
			slog.Warn("status: twilio signature rejected", "channel", h.channel)
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
	case h.secret != "":
		// A custom relay has no Twilio signature to offer. Accepting the
		// receipt unauthenticated would be worse than not having receipts, so
		// this arm exists only so a deployment with no auth token is refused
		// loudly rather than silently trusting the caller.
		slog.Warn("status: no account auth token; delivery receipts refused", "channel", h.channel)
		http.Error(w, `{"error":"status callback requires an account auth token"}`, http.StatusForbidden)
		return
	default:
		http.Error(w, `{"error":"webhook authentication not configured"}`, http.StatusForbidden)
		return
	}

	sid := r.FormValue("MessageSid")
	status := r.FormValue("MessageStatus")
	to := stripAddr(r.FormValue("To"))

	// Twilio retries a receipt it considers undelivered, and the terminal
	// states are the only ones worth acting on. "failed" and "undelivered" on
	// WhatsApp most often mean the 24h session window closed, which is the
	// failure a booth operator needs to see rather than have swallowed.
	switch status {
	case "failed", "undelivered":
		slog.Warn("delivery failed", "channel", h.channel, "sid", sid, "to", to,
			"status", status, "error_code", r.FormValue("ErrorCode"))
	default:
		slog.Info("delivery status", "channel", h.channel, "sid", sid, "to", to, "status", status)
	}

	w.WriteHeader(http.StatusNoContent)
}
