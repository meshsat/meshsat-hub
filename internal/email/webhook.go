package email

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/webhookroute"
)

// InboundEmail is published to MQTT when an email is received.
type InboundEmail struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
	PGPSigned bool   `json:"pgp_signed"`
	PGPSigner string `json:"pgp_signer,omitempty"`
	Decrypted bool   `json:"decrypted"`
	Timestamp string `json:"timestamp"`
}

// ReplyLane is the "@" lane (internal/personchat): it takes a person's reply
// to a device that wrote to them, and answers whether it did. Declared here
// so this package does not import the lane.
type ReplyLane interface {
	TakeMailReply(ctx context.Context, tenantID string, in InboundReply) bool
}

// InboundReply is what the webhook hands the lane.
type InboundReply struct {
	From      string
	Subject   string
	MessageID string
	// Text is the mail's text, decrypted when the tenant's key opened a
	// message encrypted to that key ALONE.
	Text string
	// PGP is the armoured message the mail carried, inline or as a PGP/MIME
	// part, when it is end to end: encrypted to some key other than the
	// tenant's. The lane carries it to the device unopened.
	PGP string
	// AutoReply: an out-of-office or list mail, which must never become a
	// paid satellite message. AuthFailed: the mail service checked the
	// sender's domain (DKIM, SPF) and nothing passed.
	AutoReply  bool
	AuthFailed bool
}

// WebhookHandler handles inbound email webhooks from services like
// Mailgun, SendGrid, or custom SMTP-to-webhook bridges.
type WebhookHandler struct {
	mqtt   bus.MessageBus
	pool   *Pool
	secret string // shared secret (X-Webhook-Secret or ?secret=); empty = every request refused
	lane   ReplyLane
}

// SetReplyLane attaches the "@" lane. Until MESHSAT-1685 inbound mail was
// published on {ns}/hub/email/inbound and nothing read it, so a reply to a
// device could never reach one.
func (h *WebhookHandler) SetReplyLane(l ReplyLane) { h.lane = l }

// tenantOf is the tenant whose webhook path this mail arrived at, or the
// platform tenant on the legacy shared path. Inbound mail used to be published
// on one global topic for every tenant at once, so anything subscribed to the
// hub namespace saw all of it (MESHSAT-975).
func (h *WebhookHandler) tenantOf(r *http.Request) string {
	if t := webhookroute.TenantID(r.Context()); t != "" {
		return t
	}
	return hubmqtt.DefaultTenant
}

// SetSecret configures the shared secret the email service must present (MESHSAT-976).
func (h *WebhookHandler) SetSecret(s string) { h.secret = s }

func (h *WebhookHandler) secretOK(r *http.Request) bool {
	// A request that came through a tenant's webhook path already proved it
	// holds that tenant's secret; the middleware resolved it.
	if webhookroute.TenantID(r.Context()) != "" {
		return true
	}
	got := r.Header.Get("X-Webhook-Secret")
	if got == "" {
		got = r.URL.Query().Get("secret")
	}
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(h.secret)) == 1
}

// NewWebhookHandler creates a new inbound email webhook handler.
//
// It takes the pool rather than one keyring because inbound mail is decrypted
// with the PGP key of the tenant whose webhook path it arrived at -- mail
// encrypted to one tenant's key is not another tenant's to read (MESHSAT-1121).
func NewWebhookHandler(mqtt bus.MessageBus, pool *Pool) *WebhookHandler {
	return &WebhookHandler{mqtt: mqtt, pool: pool}
}

// ServeHTTP handles the inbound email webhook POST.
//
//	@Summary      Receive inbound email
//	@Description  Email service (Mailgun/SendGrid) posts inbound emails here
//	@Tags         webhook
//	@Accept       application/x-www-form-urlencoded
//	@Produce      json
//	@Success      200
//	@Failure      400  {object}  map[string]string
//	@Failure      401  {object}  map[string]string
//	@Failure      403  {object}  map[string]string
//	@Router       /api/webhook/email [post]
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	if h.secret == "" {
		slog.Warn("email: webhook secret not configured, rejecting request")
		http.Error(w, `{"error":"webhook secret not configured"}`, http.StatusForbidden)
		return
	}
	if !h.secretOK(r) {
		slog.Warn("email: webhook secret missing or wrong", "remote", r.RemoteAddr)
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 10<<20) // 10MB limit for email content

	// Mailgun forwards inbound mail as multipart/form-data. ParseForm does
	// not read that, and FormValue only parses a multipart body when nothing
	// was parsed before it, so the fields of such a post used to read as
	// empty. Kept in memory up to the body limit: the Hub's scratch image has
	// no temp directory for the parser to spill to.
	var perr error
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") {
		perr = r.ParseMultipartForm(10 << 20)
		if r.MultipartForm != nil {
			defer func() { _ = r.MultipartForm.RemoveAll() }()
		}
	} else {
		perr = r.ParseForm()
	}
	if perr != nil {
		slog.Warn("email: parse form failed", "error", perr)
		http.Error(w, `{"error":"invalid form data"}`, http.StatusBadRequest)
		return
	}

	from := r.FormValue("from")
	if from == "" {
		from = r.FormValue("sender")
	}
	to := r.FormValue("to")
	if to == "" {
		to = r.FormValue("recipient")
	}
	subject := r.FormValue("subject")
	body := r.FormValue("body-plain")
	if body == "" {
		body = r.FormValue("body")
	}
	if body == "" {
		body = r.FormValue("text")
	}

	if from == "" || body == "" {
		http.Error(w, `{"error":"missing from or body"}`, http.StatusBadRequest)
		return
	}

	// Not the subject: a reply to a device carries its conversation's
	// reference there, and a log line is not where a capability belongs.
	slog.Info("email: inbound received", "from", from, "to", to, "len", len(body))

	msg := InboundEmail{
		From:      from,
		To:        to,
		Subject:   subject,
		Body:      body,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	// Try PGP decryption if body looks PGP-encrypted.
	gw := h.pool.ForTenant(r.Context(), h.tenantOf(r))
	if gw != nil && strings.Contains(body, "-----BEGIN PGP MESSAGE-----") {
		plaintext, signer, err := gw.KeyRing.Decrypt(body)
		if err != nil {
			slog.Warn("email: PGP decryption failed", "from", from, "error", err)
		} else {
			msg.Body = plaintext
			msg.Decrypted = true
			if signer != "" {
				msg.PGPSigned = true
				msg.PGPSigner = signer
			}
			slog.Info("email: PGP decrypted", "from", from, "signer", signer)
		}
	}

	// A reply to a device: the lane checks the conversation's reference and
	// the sender, and sends it to the device off this request.
	if h.lane != nil {
		in := InboundReply{From: from, Subject: subject, MessageID: inboundMessageID(r), Text: msg.Body,
			AutoReply: autoReply(r, subject), AuthFailed: authFailed(r)}
		block, inline := armouredBlock(body)
		if !inline {
			block = pgpPart(r)
		}
		if block != "" {
			onlyTenant := gw != nil && gw.KeyRing.EncryptedOnlyToMe(block)
			switch {
			case onlyTenant && inline && msg.Decrypted:
				// Encrypted to the tenant's key alone: the hop to the Hub, so
				// the text goes on as plain.
			case onlyTenant:
				if plaintext, _, err := gw.KeyRing.Decrypt(block); err == nil {
					in.Text = mimeText(plaintext)
				}
			default:
				in.PGP, in.Text = block, ""
			}
		}
		if h.lane.TakeMailReply(r.Context(), h.tenantOf(r), in) {
			slog.Info("email: inbound taken by the person lane", "tenant", h.tenantOf(r))
		}
	}

	if h.mqtt != nil {
		topic := hubmqtt.Namespace(h.tenantOf(r)) + "/hub/email/inbound"
		if err := h.mqtt.PublishJSON(topic, 1, false, msg); err != nil {
			slog.Error("email: mqtt publish failed", "error", err)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
