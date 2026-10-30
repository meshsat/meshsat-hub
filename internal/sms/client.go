// Package sms provides SMS send/receive via Twilio or Vonage REST APIs.
// Outbound: subscribe to meshsat/+/mt/sms on MQTT, send via provider.
// Inbound: POST /api/webhook/sms receives provider callbacks, publishes to MQTT.
package sms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/meshsat/meshsat-hub/internal/integrations"
)

// Client sends SMS messages via Twilio REST API.
type Client struct {
	accountSID string
	authToken  string
	fromNumber string
	channel    string // "" or "sms" for SMS; "whatsapp" for the WhatsApp bearer
	apiURL     string // overridable for tests
	httpClient *http.Client
	// alphaSender is the account's alphanumeric sender ("MeshSat"), used by
	// SendNotification only, and only to numbers starting with one of
	// alphaPrefixes (MESHSAT-1217). Empty = every message from fromNumber.
	alphaSender   string
	alphaPrefixes []string
}

// HTTPError is a request Twilio answered with an error status. A 4xx means
// Twilio refused it, so nothing was queued and nothing will be delivered.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("sms: HTTP %d: %s", e.Status, e.Body) }

// SendResult is the response after queuing an outbound SMS.
type SendResult struct {
	SID    string `json:"sid"`
	Status string `json:"status"` // "queued", "sent", "delivered", "failed", "undelivered"
	Error  string `json:"error,omitempty"`
}

// NewClient creates a Twilio SMS client using Account SID + Auth Token.
func NewClient(accountSID, authToken, fromNumber string) *Client {
	return &Client{
		accountSID: accountSID,
		authToken:  authToken,
		fromNumber: fromNumber,
		apiURL:     fmt.Sprintf("https://api.twilio.com/2010-04-01/Accounts/%s", accountSID),
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// NewClientWithAPIKey creates a Twilio SMS client using API Key auth.
// accountSID is used in the URL path; apiKeySID + apiKeySecret for Basic Auth.
func NewClientWithAPIKey(accountSID, apiKeySID, apiKeySecret, fromNumber string) *Client {
	return &Client{
		accountSID: apiKeySID,    // used for Basic Auth username
		authToken:  apiKeySecret, // used for Basic Auth password
		fromNumber: fromNumber,
		apiURL:     fmt.Sprintf("https://api.twilio.com/2010-04-01/Accounts/%s", accountSID),
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// SetAPIURL overrides the API base URL (for testing with mock server).
func (c *Client) SetAPIURL(url string) {
	c.apiURL = url
}

// channelName is the bearer this client sends on, for logging.
func (c *Client) channelName() string {
	if c.channel == "" {
		return "sms"
	}
	return c.channel
}

// SetChannel selects the bearer this client sends on: "" / "sms" for SMS,
// "whatsapp" for WhatsApp.
//
// Twilio sends both through the same /Messages.json endpoint on the same
// account; a WhatsApp send is the same request with "whatsapp:" in front of
// both To and From. So this is a decoration on the existing client rather than
// a second one, which is also what keeps the credential handling, timeouts,
// error mapping and status parsing identical across the two bearers.
func (c *Client) SetChannel(ch string) { c.channel = ch }

// addr applies the channel prefix Twilio expects on an address.
//
// Callers pass bare E.164 throughout, because that is the Hub's device id
// (MESHSAT-1173); the prefix exists only on the wire to Twilio and is added
// here, at the last possible moment, and stripped on the way back in.
func (c *Client) addr(a string) string {
	if c.channel == "whatsapp" && !strings.HasPrefix(a, "whatsapp:") {
		return "whatsapp:" + a
	}
	return a
}

// FromNumber is the number the client sends from.
func (c *Client) FromNumber() string { return c.fromNumber }

// SetAlphaSender gives the client an alphanumeric sender for SendNotification,
// used only for recipients whose number starts with one of prefixes ("+31").
// An empty name turns it off. An invalid name or prefix list leaves it off and
// returns the reason: a send From a name Twilio refuses would fail the page.
func (c *Client) SetAlphaSender(name, prefixes string) error {
	c.alphaSender, c.alphaPrefixes = "", nil
	if name == "" {
		return nil
	}
	if err := integrations.CheckAlphaSender(name); err != nil {
		return err
	}
	list, err := integrations.ParseCallingPrefixes(prefixes)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		return errors.New("sms: a sender name needs the calling codes it is used for, e.g. +31")
	}
	c.alphaSender, c.alphaPrefixes = name, list
	return nil
}

// AlphaSender is the alphanumeric sender in force, empty when off.
func (c *Client) AlphaSender() string {
	if c == nil {
		return ""
	}
	return c.alphaSender
}

// notificationFrom is the sender for a notification to the given number: the
// alphanumeric sender when the number is in one of its countries, else the
// From number. Never the alphanumeric one on WhatsApp, which has no such thing.
func (c *Client) notificationFrom(to string) string {
	if c.alphaSender == "" || (c.channel != "" && c.channel != "sms") {
		return c.fromNumber
	}
	for _, p := range c.alphaPrefixes {
		if strings.HasPrefix(to, p) {
			return c.alphaSender
		}
	}
	return c.fromNumber
}

// SendNotification sends a one-way notification to a person: an alert page.
//
// It is the ONLY send that may leave from the alphanumeric sender, because
// nobody can reply to one (MESHSAT-1217). Everything that expects an answer --
// the kits' SMS bearer, OOB mgmt_* commands, the booth, the "@" lane, routed
// messages, the API send -- uses Send and the From number;
// TestOnlyTheAlertNotifierSendsFromTheAlphaSender holds that.
//
// If Twilio refuses the alphanumeric sender outright (a 4xx: nothing was
// queued), the page goes again from the From number. An alert that reaches
// its person from a number beats one that does not reach them at all.
func (c *Client) SendNotification(ctx context.Context, to, body string) (*SendResult, error) {
	from := c.notificationFrom(to)
	if from == c.fromNumber {
		return c.Send(ctx, to, body)
	}
	if to == "" {
		return nil, fmt.Errorf("sms: empty recipient number")
	}
	if body == "" {
		return nil, fmt.Errorf("sms: empty message body")
	}
	res, err := c.post(ctx, url.Values{"To": {to}, "From": {from}, "Body": {body}})
	var he *HTTPError
	if err != nil && errors.As(err, &he) && he.Status >= 400 && he.Status < 500 && c.fromNumber != "" {
		slog.Warn("sms: alphanumeric sender refused, sending from the number instead",
			"to", to, "sender", from, "status", he.Status, "error", he.Body)
		return c.Send(ctx, to, body)
	}
	return res, err
}

// Send sends a message to the given phone number on this client's channel.
func (c *Client) Send(ctx context.Context, to, body string) (*SendResult, error) {
	if to == "" {
		return nil, fmt.Errorf("sms: empty recipient number")
	}
	if body == "" {
		return nil, fmt.Errorf("sms: empty message body")
	}

	form := url.Values{
		"To":   {c.addr(to)},
		"From": {c.addr(c.fromNumber)},
		"Body": {body},
	}
	return c.post(ctx, form)
}

// SendContent sends a Twilio Content template, which is how WhatsApp carries an
// interactive list or buttons. contentVars is the JSON Twilio expects in
// ContentVariables, e.g. {"1":"Welcome..."}.
//
// A Content resource is NOT submitted to Meta. That matters: an unapproved
// template can only be sent inside the 24h window a visitor opened by messaging
// us first, which is exactly the booth's shape. Outside that window Twilio will
// refuse it, and the refusal arrives on the status callback as failed rather
// than as an error here.
func (c *Client) SendContent(ctx context.Context, to, contentSid, contentVars string) (*SendResult, error) {
	if to == "" {
		return nil, fmt.Errorf("sms: empty recipient number")
	}
	if contentSid == "" {
		return nil, fmt.Errorf("sms: empty content sid")
	}
	form := url.Values{
		"To":         {c.addr(to)},
		"From":       {c.addr(c.fromNumber)},
		"ContentSid": {contentSid},
	}
	if contentVars != "" {
		form.Set("ContentVariables", contentVars)
	}
	return c.post(ctx, form)
}

// statusCallbackURL is where Twilio should report what became of a message.
//
// It is set per message rather than on the phone number, because a number has
// no SMS status field -- its status_callback is the VOICE one -- and a
// Messaging Service would be a second place to keep in step with this code.
//
// Without it, a carrier drop is invisible: Twilio accepts the send, answers
// "queued", and the Hub logs a success for a message nobody ever received.
// That is how a 200-character prompt went missing at the stand while every log
// line said it had been sent (MESHSAT-1175).
func (c *Client) statusCallbackURL() string {
	base := strings.TrimSuffix(publicBaseURL, "/")
	if base == "" {
		return ""
	}
	return base + "/api/webhook/" + c.channelName() + "/status"
}

func (c *Client) post(ctx context.Context, form url.Values) (*SendResult, error) {
	if cb := c.statusCallbackURL(); cb != "" && form.Get("StatusCallback") == "" {
		form.Set("StatusCallback", cb)
	}

	apiURL := fmt.Sprintf("%s/Messages.json", c.apiURL)
	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("sms: create request: %w", err)
	}
	req.SetBasicAuth(c.accountSID, c.authToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sms: send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("sms: read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, &HTTPError{Status: resp.StatusCode, Body: string(respBody)}
	}

	var result struct {
		SID    string `json:"sid"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("sms: parse response: %w", err)
	}

	slog.Info("message sent", "channel", c.channelName(),
		"to", form.Get("To"), "sid", result.SID, "status", result.Status)
	return &SendResult{SID: result.SID, Status: result.Status}, nil
}

// CheckStatus queries the delivery status of a previously sent message.
func (c *Client) CheckStatus(ctx context.Context, messageSID string) (*SendResult, error) {
	apiURL := fmt.Sprintf("%s/Messages/%s.json", c.apiURL, messageSID)
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("sms: create request: %w", err)
	}
	req.SetBasicAuth(c.accountSID, c.authToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sms: check status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("sms: read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, &HTTPError{Status: resp.StatusCode, Body: string(respBody)}
	}

	var result struct {
		SID    string `json:"sid"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("sms: parse response: %w", err)
	}

	return &SendResult{SID: result.SID, Status: result.Status}, nil
}
