package rock7

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	username   string
	password   string
	apiURL     string
	httpClient *http.Client
}

type SendResult struct {
	OK    bool
	MTID  string
	Error string
}

func NewClient(username, password string) *Client {
	return &Client{
		username:   username,
		password:   password,
		apiURL:     "https://rockblock.rock7.com/rockblock/MT",
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) SetAPIURL(u string) { c.apiURL = u }

func (c *Client) SendMT(ctx context.Context, imei, dataHex string) (*SendResult, error) {
	form := url.Values{
		"imei":     {imei},
		"username": {c.username},
		"password": {c.password},
		"data":     {dataHex},
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.apiURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("rock7: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rock7: send MT: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("rock7: read response: %w", err)
	}

	text := strings.TrimSpace(string(body))
	slog.Info("rock7: MT response", "imei", imei, "status", resp.StatusCode, "body", text)

	if strings.HasPrefix(text, "OK,") {
		return &SendResult{OK: true, MTID: strings.TrimPrefix(text, "OK,")}, nil
	}
	return &SendResult{OK: false, Error: text}, fmt.Errorf("rock7: MT failed: %s", text)
}
