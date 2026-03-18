// Package hawkbit provides a Go client for the Eclipse hawkBit Device
// Management API (Management API v1). It manages firmware artifacts,
// distribution sets, rollout campaigns, and per-device update assignments.
//
// hawkBit is used for OTA firmware updates to MeshSat field devices over
// satellite or terrestrial links.
//
// Reference: https://www.eclipse.org/hawkbit/apis/management_api/
package hawkbit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Client communicates with the hawkBit Management API.
type Client struct {
	baseURL    string
	username   string
	password   string
	httpClient *http.Client
}

// NewClient creates a new hawkBit Management API client.
func NewClient(baseURL, username, password string) *Client {
	return &Client{
		baseURL:  baseURL,
		username: username,
		password: password,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// Target represents a managed device (field device) in hawkBit.
type Target struct {
	ControllerId string `json:"controllerId"`
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	IPAddress    string `json:"ipAddress,omitempty"`
	UpdateStatus string `json:"updateStatus,omitempty"` // "registered", "in_sync", "pending", "error"
	LastPollAt   string `json:"lastControllerRequestAt,omitempty"`
}

// SoftwareModule represents a firmware artifact grouping.
type SoftwareModule struct {
	ID          int64  `json:"id,omitempty"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Type        string `json:"type"` // "os", "application", "firmware"
	Vendor      string `json:"vendor,omitempty"`
	Description string `json:"description,omitempty"`
}

// DistributionSet is an installable bundle of software modules.
type DistributionSet struct {
	ID          int64  `json:"id,omitempty"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type,omitempty"` // e.g. "os_with_app"
	Complete    bool   `json:"requiredMigrationStep"`
}

// Rollout represents a deployment campaign.
type Rollout struct {
	ID              int64  `json:"id,omitempty"`
	Name            string `json:"name"`
	DistributionSet int64  `json:"distributionSetId,omitempty"`
	TargetFilter    string `json:"targetFilterQuery,omitempty"`
	Status          string `json:"status,omitempty"` // "creating", "ready", "running", "paused", "finished"
	TotalTargets    int    `json:"totalTargets,omitempty"`
	GroupCount      int    `json:"amountGroups,omitempty"`
}

// ActionStatus represents the deployment action status for a target.
type ActionStatus struct {
	ID     int64  `json:"id"`
	Type   string `json:"type"`   // "update", "cancel"
	Status string `json:"status"` // "finished", "running", "error", "canceled"
}

// listResponse is the generic paginated response wrapper from hawkBit.
type listResponse struct {
	Content json.RawMessage `json:"content"`
	Total   int             `json:"total"`
	Size    int             `json:"size"`
}

// CreateTarget registers a new device in hawkBit.
func (c *Client) CreateTarget(ctx context.Context, target Target) (*Target, error) {
	body, err := json.Marshal([]Target{target})
	if err != nil {
		return nil, fmt.Errorf("hawkbit: marshal target: %w", err)
	}

	respBody, err := c.doRequest(ctx, "POST", "/rest/v1/targets", body)
	if err != nil {
		return nil, err
	}

	var targets []Target
	if err := json.Unmarshal(respBody, &targets); err != nil {
		return nil, fmt.Errorf("hawkbit: parse targets: %w", err)
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("hawkbit: no target returned")
	}

	slog.Info("hawkbit: target created", "controllerId", targets[0].ControllerId)
	return &targets[0], nil
}

// GetTarget retrieves a target by its controller ID.
func (c *Client) GetTarget(ctx context.Context, controllerID string) (*Target, error) {
	respBody, err := c.doRequest(ctx, "GET", fmt.Sprintf("/rest/v1/targets/%s", controllerID), nil)
	if err != nil {
		return nil, err
	}

	var target Target
	if err := json.Unmarshal(respBody, &target); err != nil {
		return nil, fmt.Errorf("hawkbit: parse target: %w", err)
	}

	return &target, nil
}

// ListTargets returns all registered targets.
func (c *Client) ListTargets(ctx context.Context) ([]Target, error) {
	respBody, err := c.doRequest(ctx, "GET", "/rest/v1/targets?limit=500", nil)
	if err != nil {
		return nil, err
	}

	var resp listResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("hawkbit: parse targets list: %w", err)
	}

	var targets []Target
	if err := json.Unmarshal(resp.Content, &targets); err != nil {
		return nil, fmt.Errorf("hawkbit: parse targets content: %w", err)
	}

	return targets, nil
}

// DeleteTarget removes a target from hawkBit.
func (c *Client) DeleteTarget(ctx context.Context, controllerID string) error {
	_, err := c.doRequest(ctx, "DELETE", fmt.Sprintf("/rest/v1/targets/%s", controllerID), nil)
	return err
}

// CreateSoftwareModule creates a new firmware module.
func (c *Client) CreateSoftwareModule(ctx context.Context, module SoftwareModule) (*SoftwareModule, error) {
	body, err := json.Marshal([]SoftwareModule{module})
	if err != nil {
		return nil, fmt.Errorf("hawkbit: marshal module: %w", err)
	}

	respBody, err := c.doRequest(ctx, "POST", "/rest/v1/softwaremodules", body)
	if err != nil {
		return nil, err
	}

	var modules []SoftwareModule
	if err := json.Unmarshal(respBody, &modules); err != nil {
		return nil, fmt.Errorf("hawkbit: parse modules: %w", err)
	}
	if len(modules) == 0 {
		return nil, fmt.Errorf("hawkbit: no module returned")
	}

	slog.Info("hawkbit: software module created", "id", modules[0].ID, "name", modules[0].Name)
	return &modules[0], nil
}

// CreateDistributionSet creates a new distribution set (installable bundle).
func (c *Client) CreateDistributionSet(ctx context.Context, ds DistributionSet) (*DistributionSet, error) {
	body, err := json.Marshal([]DistributionSet{ds})
	if err != nil {
		return nil, fmt.Errorf("hawkbit: marshal distribution set: %w", err)
	}

	respBody, err := c.doRequest(ctx, "POST", "/rest/v1/distributionsets", body)
	if err != nil {
		return nil, err
	}

	var sets []DistributionSet
	if err := json.Unmarshal(respBody, &sets); err != nil {
		return nil, fmt.Errorf("hawkbit: parse distribution sets: %w", err)
	}
	if len(sets) == 0 {
		return nil, fmt.Errorf("hawkbit: no distribution set returned")
	}

	slog.Info("hawkbit: distribution set created", "id", sets[0].ID, "name", sets[0].Name)
	return &sets[0], nil
}

// CreateRollout creates a new rollout campaign for deploying a distribution set.
func (c *Client) CreateRollout(ctx context.Context, rollout Rollout) (*Rollout, error) {
	type rolloutReq struct {
		Name              string `json:"name"`
		DistributionSetID int64  `json:"distributionSetId"`
		TargetFilterQuery string `json:"targetFilterQuery"`
		AmountGroups      int    `json:"amountGroups"`
	}
	groups := rollout.GroupCount
	if groups <= 0 {
		groups = 1
	}
	req := rolloutReq{
		Name:              rollout.Name,
		DistributionSetID: rollout.DistributionSet,
		TargetFilterQuery: rollout.TargetFilter,
		AmountGroups:      groups,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("hawkbit: marshal rollout: %w", err)
	}

	respBody, err := c.doRequest(ctx, "POST", "/rest/v1/rollouts", body)
	if err != nil {
		return nil, err
	}

	var created Rollout
	if err := json.Unmarshal(respBody, &created); err != nil {
		return nil, fmt.Errorf("hawkbit: parse rollout: %w", err)
	}

	slog.Info("hawkbit: rollout created", "id", created.ID, "name", created.Name)
	return &created, nil
}

// StartRollout transitions a rollout from ready to running.
func (c *Client) StartRollout(ctx context.Context, rolloutID int64) error {
	_, err := c.doRequest(ctx, "POST", fmt.Sprintf("/rest/v1/rollouts/%d/start", rolloutID), nil)
	return err
}

// PauseRollout pauses a running rollout.
func (c *Client) PauseRollout(ctx context.Context, rolloutID int64) error {
	_, err := c.doRequest(ctx, "POST", fmt.Sprintf("/rest/v1/rollouts/%d/pause", rolloutID), nil)
	return err
}

// GetRollout retrieves rollout details.
func (c *Client) GetRollout(ctx context.Context, rolloutID int64) (*Rollout, error) {
	respBody, err := c.doRequest(ctx, "GET", fmt.Sprintf("/rest/v1/rollouts/%d", rolloutID), nil)
	if err != nil {
		return nil, err
	}

	var rollout Rollout
	if err := json.Unmarshal(respBody, &rollout); err != nil {
		return nil, fmt.Errorf("hawkbit: parse rollout: %w", err)
	}

	return &rollout, nil
}

// GetTargetActions returns deployment actions for a target.
func (c *Client) GetTargetActions(ctx context.Context, controllerID string) ([]ActionStatus, error) {
	path := fmt.Sprintf("/rest/v1/targets/%s/actions?limit=50", controllerID)
	respBody, err := c.doRequest(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}

	var resp listResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("hawkbit: parse actions list: %w", err)
	}

	var actions []ActionStatus
	if err := json.Unmarshal(resp.Content, &actions); err != nil {
		return nil, fmt.Errorf("hawkbit: parse actions content: %w", err)
	}

	return actions, nil
}

// CancelAction cancels a pending deployment action (rollback).
func (c *Client) CancelAction(ctx context.Context, controllerID string, actionID int64) error {
	path := fmt.Sprintf("/rest/v1/targets/%s/actions/%d", controllerID, actionID)
	_, err := c.doRequest(ctx, "DELETE", path, nil)
	return err
}

// IsReachable checks if the hawkBit server is reachable.
func (c *Client) IsReachable(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/rest/v1/system/configs", nil)
	if err != nil {
		return false
	}
	req.SetBasicAuth(c.username, c.password)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode < 500
}

// doRequest executes an authenticated request against the hawkBit Management API.
func (c *Client) doRequest(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	url := c.baseURL + path

	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("hawkbit: create request: %w", err)
	}
	req.SetBasicAuth(c.username, c.password)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hawkbit: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("hawkbit: read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("hawkbit: HTTP %d on %s %s: %s", resp.StatusCode, method, path, string(respBody))
	}

	return respBody, nil
}
