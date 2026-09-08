// Package authentik talks to the identity provider so approving a beta request
// is something an operator does in the Hub rather than a script somebody has
// to remember to run (MESHSAT-978).
//
// Enrollment already works: a stranger signs up at auth.meshsat.net, the
// account is created inactive in the meshsat-pending group with the address
// they signed up from recorded, and the Hub refuses any login whose groups map
// to no role. What was manual is the step after that. This is the same work
// approve-meshsat-user.py does, over the REST API.
package authentik

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a small authentik REST client. It knows only the operations
// approval needs; it is not a general SDK.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// New returns a client for an authentik base URL (e.g. https://auth.meshsat.net)
// and an API token. A blank base or token yields nil, which every caller reads
// as "the feature is not configured" rather than an error at startup.
func New(base, token string) *Client {
	base, token = strings.TrimRight(strings.TrimSpace(base), "/"), strings.TrimSpace(token)
	if base == "" || token == "" {
		return nil
	}
	return &Client{base: base, token: token, http: &http.Client{Timeout: 20 * time.Second}}
}

// PendingUser is someone waiting for a decision.
type PendingUser struct {
	PK            int       `json:"pk"`
	Username      string    `json:"username"`
	Name          string    `json:"name"`
	Email         string    `json:"email"`
	Active        bool      `json:"is_active"`
	SignupIP      string    `json:"signup_ip,omitempty"`
	Organisation  string    `json:"organisation,omitempty"`
	Country       string    `json:"country,omitempty"`
	Callsign      string    `json:"callsign,omitempty"`
	IntendedUse   string    `json:"intended_use,omitempty"`
	MatrixID      string    `json:"matrix_id,omitempty"`
	EmailVerified bool      `json:"email_verified"`
	Created       time.Time `json:"created,omitempty"`
}

type akUser struct {
	PK       int      `json:"pk"`
	Username string   `json:"username"`
	Name     string   `json:"name"`
	Email    string   `json:"email"`
	IsActive bool     `json:"is_active"`
	Groups   []string `json:"groups"`
	// groups_obj is deliberately not decoded: authentik returns full group
	// objects there, not names, and decoding it into the wrong shape made the
	// whole list fail the moment a real user appeared. Nothing here needs it.
	Attributes map[string]any `json:"attributes"`
	DateJoined time.Time      `json:"date_joined"`
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("authentik %s %s: %s", method, path, resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func attrStr(a map[string]any, k string) string {
	if v, ok := a[k].(string); ok {
		return v
	}
	return ""
}

func attrBool(a map[string]any, k string) bool {
	switch v := a[k].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "True" || v == "1"
	}
	return false
}

// ListPending returns everyone in the pending group awaiting a decision, newest
// first, with the details the enrollment form collected so the decision can be
// made without leaving the page.
func (c *Client) ListPending(ctx context.Context) ([]PendingUser, error) {
	var page struct {
		Results []akUser `json:"results"`
	}
	q := url.Values{"groups_by_name": {PendingGroup}, "page_size": {"200"}}
	if err := c.do(ctx, http.MethodGet, "/api/v3/core/users/?"+q.Encode(), nil, &page); err != nil {
		return nil, err
	}
	out := make([]PendingUser, 0, len(page.Results))
	for _, u := range page.Results {
		out = append(out, PendingUser{
			PK: u.PK, Username: u.Username, Name: u.Name, Email: u.Email, Active: u.IsActive,
			SignupIP:      attrStr(u.Attributes, "signup_ip"),
			Organisation:  attrStr(u.Attributes, "organisation"),
			Country:       attrStr(u.Attributes, "country"),
			Callsign:      attrStr(u.Attributes, "callsign"),
			IntendedUse:   attrStr(u.Attributes, "intended_use"),
			MatrixID:      attrStr(u.Attributes, "matrix_id"),
			EmailVerified: attrBool(u.Attributes, "email_verified"),
			Created:       u.DateJoined,
		})
	}
	return out, nil
}

// Group names the bootstrap creates.
const (
	PendingGroup = "meshsat-pending"
	GroupPrefix  = "meshsat-"
)

// ValidRole reports whether r is a role the Hub maps to.
func ValidRole(r string) bool {
	return r == "owner" || r == "operator" || r == "viewer"
}

func (c *Client) groupPK(ctx context.Context, name string) (string, error) {
	var page struct {
		Results []struct {
			PK string `json:"pk"`
		} `json:"results"`
	}
	q := url.Values{"name": {name}}
	if err := c.do(ctx, http.MethodGet, "/api/v3/core/groups/?"+q.Encode(), nil, &page); err != nil {
		return "", err
	}
	if len(page.Results) == 0 {
		return "", fmt.Errorf("authentik: no group %q", name)
	}
	return page.Results[0].PK, nil
}

func (c *Client) user(ctx context.Context, pk int) (*akUser, error) {
	var u akUser
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/v3/core/users/%d/", pk), nil, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// Approve activates the account and moves it from pending into the role group.
// It returns the address the person signed up from, which is what the edge
// allowlist needs. The Hub creates their tenant on first login.
func (c *Client) Approve(ctx context.Context, pk int, role string) (signupIP string, email string, err error) {
	if !ValidRole(role) {
		return "", "", fmt.Errorf("authentik: not a role: %q", role)
	}
	u, err := c.user(ctx, pk)
	if err != nil {
		return "", "", err
	}
	pendingPK, err := c.groupPK(ctx, PendingGroup)
	if err != nil {
		return "", "", err
	}
	rolePK, err := c.groupPK(ctx, GroupPrefix+role)
	if err != nil {
		return "", "", err
	}
	groups := []string{rolePK}
	for _, g := range u.Groups {
		if g != pendingPK && g != rolePK {
			groups = append(groups, g)
		}
	}
	if u.Attributes == nil {
		u.Attributes = map[string]any{}
	}
	u.Attributes["meshsat_approved"] = true
	patch := map[string]any{"is_active": true, "groups": groups, "attributes": u.Attributes}
	if err := c.do(ctx, http.MethodPatch, fmt.Sprintf("/api/v3/core/users/%d/", pk), patch, nil); err != nil {
		return "", "", err
	}
	return attrStr(u.Attributes, "signup_ip"), u.Email, nil
}

// Reject deletes the account. It refuses an account that is already active or
// no longer pending, so this cannot be turned into a way to delete a real user.
func (c *Client) Reject(ctx context.Context, pk int) (email string, err error) {
	u, err := c.user(ctx, pk)
	if err != nil {
		return "", err
	}
	pendingPK, err := c.groupPK(ctx, PendingGroup)
	if err != nil {
		return "", err
	}
	stillPending := false
	for _, g := range u.Groups {
		if g == pendingPK {
			stillPending = true
		}
	}
	if u.IsActive || !stillPending {
		return "", fmt.Errorf("authentik: refusing to delete %s: active or no longer pending", u.Email)
	}
	if err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/api/v3/core/users/%d/", pk), nil, nil); err != nil {
		return "", err
	}
	return u.Email, nil
}
