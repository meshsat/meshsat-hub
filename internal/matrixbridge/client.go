package matrixbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/meshsat/meshsat-hub/internal/netguard"
)

// The few calls of the Matrix client-server API the bridge makes, on net/http.
// Not mautrix-go: Mode A is one ordinary bot in unencrypted rooms, five
// endpoints, and every dependency is attack surface. Mode B (an appservice,
// each crew a virtual user) is where a full SDK would earn its place.

const (
	// syncTimeout is how long the homeserver may hold a sync open.
	syncTimeout = 30 * time.Second
	// callTimeout bounds every call, a held sync included.
	callTimeout = syncTimeout + 15*time.Second
	// maxResponse bounds what one answer from the homeserver may be.
	maxResponse = 8 << 20
)

// httpClient builds the client every call goes through: the dial-time SSRF
// guard (the URL was checked when it was saved; this catches a name that
// resolves elsewhere later) and no redirects, so the bot's token never follows
// one. A variable so tests can reach a loopback server.
var httpClient = func() *http.Client {
	c := netguard.SafeHTTPClient(callTimeout)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

// apiError is a homeserver's refusal.
type apiError struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
}

func (e *apiError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("matrix: HTTP %d", e.Status)
	}
	return fmt.Sprintf("matrix: HTTP %d %s: %s", e.Status, e.Code, e.Message)
}

type client struct {
	base  string
	token string
	http  *http.Client
}

func newClient(homeserver, token string) *client {
	return &client{base: strings.TrimRight(homeserver, "/"), token: token, http: httpClient()}
}

func (c *client) call(ctx context.Context, method, path string, q url.Values, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	u := c.base + "/_matrix/client/v3" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &apiError{Status: resp.StatusCode}
		var m struct {
			Errcode      string `json:"errcode"`
			Error        string `json:"error"`
			RetryAfterMs int64  `json:"retry_after_ms"`
		}
		if json.Unmarshal(data, &m) == nil {
			e.Code, e.Message = m.Errcode, clip(m.Error, 200)
			e.RetryAfter = time.Duration(m.RetryAfterMs) * time.Millisecond
		}
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && e.RetryAfter == 0 {
			e.RetryAfter = time.Duration(s) * time.Second
		}
		return e
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// whoami answers the bot's own user id.
func (c *client) whoami(ctx context.Context) (string, error) {
	var out struct {
		UserID string `json:"user_id"`
	}
	if err := c.call(ctx, http.MethodGet, "/account/whoami", nil, nil, &out); err != nil {
		return "", err
	}
	if out.UserID == "" {
		return "", errors.New("matrix: whoami named no user")
	}
	return out.UserID, nil
}

// join joins a room the bot was invited to (or is already in).
func (c *client) join(ctx context.Context, room string) error {
	return c.call(ctx, http.MethodPost, "/join/"+url.PathEscape(room), nil, struct{}{}, nil)
}

// encrypted reports whether a room has encryption switched on. Matrix cannot
// switch it off again, so a yes is final.
func (c *client) encrypted(ctx context.Context, room string) (bool, error) {
	err := c.call(ctx, http.MethodGet, "/rooms/"+url.PathEscape(room)+"/state/m.room.encryption/", nil, nil, nil)
	var ae *apiError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &ae) && ae.Status == http.StatusNotFound:
		return false, nil
	default:
		return false, err
	}
}

// displayName is a member's name in a room, or "" when it cannot be read.
func (c *client) displayName(ctx context.Context, room, user string) string {
	var out struct {
		DisplayName string `json:"displayname"`
	}
	if err := c.call(ctx, http.MethodGet, "/rooms/"+url.PathEscape(room)+"/state/m.room.member/"+url.PathEscape(user), nil, nil, &out); err != nil {
		return ""
	}
	return out.DisplayName
}

// send sends one event into a room. txn makes a retry the same event.
func (c *client) send(ctx context.Context, room, eventType, txn string, content any) error {
	return c.call(ctx, http.MethodPut,
		"/rooms/"+url.PathEscape(room)+"/send/"+url.PathEscape(eventType)+"/"+url.PathEscape(txn), nil, content, nil)
}

// notice sends a line from the bot itself; m.notice, so other bots leave it be.
func (c *client) notice(ctx context.Context, room, txn, text string) error {
	return c.send(ctx, room, "m.room.message", txn, map[string]any{"msgtype": "m.notice", "body": text})
}

type event struct {
	Type    string          `json:"type"`
	EventID string          `json:"event_id"`
	Sender  string          `json:"sender"`
	Content json.RawMessage `json:"content"`
}

type syncResponse struct {
	NextBatch string `json:"next_batch"`
	Rooms     struct {
		Join map[string]struct {
			Timeline struct {
				Events  []event `json:"events"`
				Limited bool    `json:"limited"`
			} `json:"timeline"`
		} `json:"join"`
	} `json:"rooms"`
}

// sync reads what happened since the token; with timeout 0 it answers at once.
func (c *client) sync(ctx context.Context, since, filter string, timeout time.Duration) (*syncResponse, error) {
	q := url.Values{"timeout": {strconv.FormatInt(timeout.Milliseconds(), 10)}, "filter": {filter}}
	if since != "" {
		q.Set("since", since)
	}
	var out syncResponse
	if err := c.call(ctx, http.MethodGet, "/sync", q, nil, &out); err != nil {
		return nil, err
	}
	if out.NextBatch == "" {
		return nil, errors.New("matrix: sync answered no next_batch")
	}
	return &out, nil
}

// clip shortens s to at most n runes.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
