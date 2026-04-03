package tak

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// MartiProxy proxies DataPackage and Mission API calls to a TAK Server
// on behalf of the Hub's managed bridges.
type MartiProxy struct {
	baseURL string
	client  *http.Client
}

// MartiMission represents a TAK Server mission (same structure as bridge).
type MartiMission struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Tool        string `json:"tool,omitempty"`
	CreateTime  string `json:"createTime,omitempty"`
}

// MartiMissionsResponse is the TAK Server response envelope.
type MartiMissionsResponse struct {
	Version string         `json:"version"`
	Type    string         `json:"type"`
	Data    []MartiMission `json:"data"`
}

// NewMartiProxy creates a proxy to the TAK Server Marti API.
func NewMartiProxy(takHost string, takPort int, takSSL bool) *MartiProxy {
	scheme := "http"
	if takSSL {
		scheme = "https"
	}
	if takPort == 0 {
		takPort = 8443
	}

	return &MartiProxy{
		baseURL: fmt.Sprintf("%s://%s:%d", scheme, takHost, takPort),
		client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true, //nolint:gosec // TAK Server self-signed
					MinVersion:         tls.VersionTLS12,
				},
			},
		},
	}
}

// ListMissions returns all missions from the TAK Server.
func (p *MartiProxy) ListMissions() ([]MartiMission, error) {
	resp, err := p.client.Get(p.baseURL + "/Marti/api/missions")
	if err != nil {
		return nil, fmt.Errorf("marti proxy: list missions: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("marti proxy: %d: %s", resp.StatusCode, string(body))
	}

	var result MartiMissionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("marti proxy: decode: %w", err)
	}

	slog.Debug("marti proxy: listed missions", "count", len(result.Data))
	return result.Data, nil
}

// DownloadContent downloads a file from the TAK Server by hash.
func (p *MartiProxy) DownloadContent(hash string) ([]byte, string, error) {
	url := fmt.Sprintf("%s/Marti/sync/content?hash=%s", p.baseURL, hash)
	resp, err := p.client.Get(url)
	if err != nil {
		return nil, "", fmt.Errorf("marti proxy: download %s: %w", hash, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("marti proxy: download %s: %d", hash, resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("marti proxy: read: %w", err)
	}

	return data, resp.Header.Get("Content-Disposition"), nil
}

// GetSASnapshot returns the current SA snapshot.
func (p *MartiProxy) GetSASnapshot() ([]byte, error) {
	resp, err := p.client.Get(p.baseURL + "/Marti/api/cot/sa")
	if err != nil {
		return nil, fmt.Errorf("marti proxy: SA: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("marti proxy: SA: %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}
